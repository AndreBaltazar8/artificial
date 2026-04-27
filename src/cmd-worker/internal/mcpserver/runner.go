package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"artificial.pt/cmd-worker/internal/hub"
	"artificial.pt/pkg-go-shared/protocol"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// RunnerOptions configures NewRunner. The runner-mode MCP server
// exposes a small, task-focused tool set instead of the full chat /
// task / review surface that long-lived workers get. Tools available:
//
//	task_describe   — optionally re-read the assignment.
//	task_checkpoint — record incremental progress; bumps liveness.
//	task_blocked    — escalate to the manager when stuck.
//	task_complete   — declare done, triggers the runner to exit.
//
// Notably absent: chat_*, join/leave, set_topic, task_create,
// commander_review. A runner that wants to talk to a human has exactly
// one structured channel — task_blocked — and a runner that wants to
// finish has exactly one — task_complete. That's the whole point of
// the manager/runner split.
type RunnerOptions struct {
	RunnerID        int64              // task_runners.id, surfaced in WS payloads
	TaskID          int64              // tasks.id this runner owns
	TaskTitle       string             // for task_describe fallback
	TaskDesc        string             // for task_describe fallback
	AssignmentBrief string             // rendered initial assignment, reused by task_describe
	WorktreePath    string             // for task_describe
	BranchName      string             // for task_describe
	Cancel          context.CancelFunc // called by task_complete; cmd-worker main waits on this
}

// NewRunner is the runner-mode counterpart to New. Same MCP plumbing
// (HTTP listener, gomcp.Server, channel-notification capability) so the
// agent process is identical from Claude's point of view; only the
// registered tool set differs.
func NewRunner(nick string, hubClient *hub.Client, instructions string, opts RunnerOptions) *Server {
	s := &Server{
		nick:      nick,
		role:      "runner",
		hubClient: hubClient,
	}

	mcpSrv := gomcp.NewServer(&gomcp.Implementation{
		Name:    "artificial-runner",
		Version: "0.1.0",
	}, &gomcp.ServerOptions{
		Instructions: instructions,
		Capabilities: &gomcp.ServerCapabilities{
			Experimental: map[string]any{"claude/channel": map[string]any{}},
			Tools:        &gomcp.ToolCapabilities{},
		},
	})

	s.mcpServer = mcpSrv
	registerRunnerTools(s, opts)
	return s
}

// runnerInputs are kept tiny on purpose — every field a runner needs
// to fill is also a field the manager needs to read. Wider schemas
// invite the runner to dump more state than the dashboard can render.

type taskCheckpointInput struct {
	Summary      string   `json:"summary" jsonschema:"One sentence on what was just accomplished. Shown in the dashboard."`
	Commits      []string `json:"commits,omitempty" jsonschema:"Optional commit hashes for this checkpoint."`
	FilesTouched []string `json:"files_touched,omitempty" jsonschema:"Optional list of files modified in this checkpoint."`
}

type taskBlockedInput struct {
	Reason   string `json:"reason"             jsonschema:"Why you're blocked. Be specific — this surfaces in the manager dashboard."`
	Question string `json:"question,omitempty" jsonschema:"Optional concrete question for the manager to answer."`
}

type taskCompleteInput struct {
	Summary    string   `json:"summary"               jsonschema:"What was done, in one to three sentences. Shown to the manager during review."`
	BranchName string   `json:"branch_name,omitempty" jsonschema:"Branch the work was committed to. Defaults to the runner's assigned branch."`
	Commits    []string `json:"commits,omitempty"     jsonschema:"Optional commit hashes for the manager to review."`
}

func registerRunnerTools(s *Server, opts RunnerOptions) {
	// completeOnce guards Cancel so a confused runner that calls
	// task_complete twice doesn't double-cancel — second call is a no-op
	// that returns success.
	var completeOnce sync.Once

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name: "task_describe",
		Description: "Return the assignment for this runner: task title, description, " +
			"project context, worktree path, and branch name. Optional re-read/recovery tool.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, _ struct{}) (*gomcp.CallToolResult, any, error) {
		return textResult(runnerAssignmentText(opts)), nil, nil
	})

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name: "task_checkpoint",
		Description: "Record incremental progress on the task. Use this after each meaningful unit of work " +
			"(a commit, a passing test, a milestone). Helps the manager see liveness without reading your transcript.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input taskCheckpointInput) (*gomcp.CallToolResult, any, error) {
		if input.Summary == "" {
			return nil, nil, fmt.Errorf("summary required")
		}
		payload, _ := json.Marshal(protocol.RunnerCheckpointPayload{
			Summary:      input.Summary,
			Commits:      input.Commits,
			FilesTouched: input.FilesTouched,
		})
		s.hubClient.Send(protocol.WSMessage{
			Type: protocol.MsgRunnerCheckpoint,
			ID:   opts.RunnerID,
			Data: payload,
		})
		return textResult("checkpoint recorded"), nil, nil
	})

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name: "task_blocked",
		Description: "Signal that you cannot make further progress without manager input. " +
			"Provide a clear reason and, ideally, a concrete question. The runner stays alive " +
			"after this call so the manager can DM with a resolution.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input taskBlockedInput) (*gomcp.CallToolResult, any, error) {
		if input.Reason == "" {
			return nil, nil, fmt.Errorf("reason required")
		}
		payload, _ := json.Marshal(protocol.RunnerBlockedPayload{
			Reason:   input.Reason,
			Question: input.Question,
		})
		s.hubClient.Send(protocol.WSMessage{
			Type: protocol.MsgRunnerBlocked,
			ID:   opts.RunnerID,
			Data: payload,
		})
		return textResult("manager has been notified — wait for instructions via channel notifications"), nil, nil
	})

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name: "task_complete",
		Description: "Declare the task done. Only call this AFTER you have committed your work to the runner branch. " +
			"This terminates the runner process, so no further tools can be called after this returns.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input taskCompleteInput) (*gomcp.CallToolResult, any, error) {
		if input.Summary == "" {
			return nil, nil, fmt.Errorf("summary required")
		}
		branch := input.BranchName
		if branch == "" {
			branch = opts.BranchName
		}
		payload, _ := json.Marshal(protocol.RunnerCompletePayload{
			Summary:    input.Summary,
			BranchName: branch,
			Commits:    input.Commits,
		})
		s.hubClient.Send(protocol.WSMessage{
			Type: protocol.MsgRunnerComplete,
			ID:   opts.RunnerID,
			Data: payload,
		})
		// Trigger graceful shutdown of the cmd-worker. main waits on
		// the same context so this unblocks the wait and triggers PTY
		// teardown. completeOnce protects against duplicate calls — the
		// second call returns success but is a no-op.
		completeOnce.Do(func() {
			slog.Info("task_complete invoked, signaling runner shutdown", "runner_id", opts.RunnerID)
			if opts.Cancel != nil {
				opts.Cancel()
			}
		})
		return textResult("task complete — runner exiting"), nil, nil
	})
}

func runnerAssignmentText(opts RunnerOptions) string {
	if opts.AssignmentBrief != "" {
		return opts.AssignmentBrief
	}
	return fmt.Sprintf(
		"Task #%d: %s\n\nWorktree: %s\nBranch: %s\n\n--- Description ---\n%s",
		opts.TaskID, opts.TaskTitle, opts.WorktreePath, opts.BranchName, opts.TaskDesc,
	)
}

// textResult is duplicated from server.go's helper rather than
// exported, because the helper there is unexported and this file is in
// the same package — direct call works.
//
// (The compiler will error if textResult disappears from server.go.
// Keeping a small reminder here is cheaper than refactoring it into a
// shared helpers.go just for two callers.)
