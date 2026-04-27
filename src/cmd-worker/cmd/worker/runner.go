package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"artificial.pt/cmd-worker/internal/agent"
	"artificial.pt/cmd-worker/internal/hub"
	"artificial.pt/cmd-worker/internal/mcpserver"
	"artificial.pt/pkg-go-shared/pluginhost"
	"artificial.pt/pkg-go-shared/protocol"
)

// runTaskRunner is the entry point for cmd-worker invoked with
// --task-runner. It bypasses the long-lived employee/channel/plugin
// dance entirely: a runner is an ephemeral process scoped to one
// task, one worktree, one branch. It calls home for its config, drives
// Claude through the runner-mode MCP server, and exits as soon as
// Claude calls task_complete (or crashes).
//
// Lifetime invariants:
//   - exit code 0 only after MsgRunnerComplete has been sent
//   - exit code != 0 means the watchdog should treat us as crashed
//   - heartbeat goroutine keeps last_heartbeat fresh while we're alive
func runTaskRunner(serverURL string, runnerID int64) {
	if runnerID == 0 {
		fmt.Fprintln(os.Stderr, "error: --runner-id required when --task-runner is set")
		os.Exit(2)
	}

	// Two contexts: parentCtx unblocks on signals (Ctrl-C); runnerCtx
	// is the one task_complete cancels via the MCP tool. main waits on
	// runnerCtx — when it fires, we tear down Claude and exit cleanly.
	parentCtx, parentCancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer parentCancel()
	runnerCtx, runnerCancel := context.WithCancel(parentCtx)
	defer runnerCancel()

	// Fetch runner config — task title, description, worktree path,
	// parent nick — from svc-artificial. The spawner has already
	// created the task_runners row, the worktree, and the branch; we
	// just read what's there.
	cfg, err := fetchRunnerConfig(parentCtx, serverURL, runnerID)
	if err != nil {
		slog.Error("fetch runner config", "err", err, "runner_id", runnerID)
		os.Exit(1)
	}
	slog.Info("runner config loaded",
		"runner_id", cfg.Runner.ID,
		"task_id", cfg.Task.ID,
		"nick", cfg.Runner.Nickname,
		"worktree", cfg.Runner.WorktreePath,
		"branch", cfg.Runner.BranchName,
	)

	nick := cfg.Runner.Nickname

	// Hub WS connection — the runner's only outbound channel for
	// MsgRunnerCheckpoint / MsgRunnerBlocked / MsgRunnerComplete and
	// inbound channel for MsgWorkerNotify (manager DMs).
	//
	// hubClient and mcpSrv are declared before either is constructed so
	// the WS callback closure can reference both — the hub forwards
	// manager directives to Claude via mcpSrv.PushChannelNotification, so
	// the closure needs to see whichever value mcpSrv holds at the time a
	// message lands. ConnectWithRetry is started AFTER mcpSrv is
	// assigned, so the closure can never observe a nil mcpSrv in
	// practice.
	var hubClient *hub.Client
	var mcpSrv *mcpserver.Server
	hubClient = hub.New(serverURL, nick, func(msg protocol.WSMessage) {
		handleRunnerHubMessage(msg, nick, hubClient, mcpSrv)
	})

	// Heartbeat: every 20s, while we're not yet exiting, POST a
	// heartbeat so the watchdog knows we're alive even when Claude is
	// thinking (no checkpoints yet). Stops on runnerCtx cancel.
	go runHeartbeatLoop(runnerCtx, serverURL, runnerID)

	// MCP server registers the four runner tools with a Cancel hook
	// pointing at runnerCancel — task_complete will fire it.
	mcpInstructions := buildRunnerMCPInstructions(cfg)
	mcpSrv = mcpserver.NewRunner(nick, hubClient, mcpInstructions, mcpserver.RunnerOptions{
		RunnerID:     cfg.Runner.ID,
		TaskID:       cfg.Task.ID,
		TaskTitle:    cfg.Task.Title,
		TaskDesc:     cfg.Task.Description,
		WorktreePath: cfg.Runner.WorktreePath,
		BranchName:   cfg.Runner.BranchName,
		Cancel:       runnerCancel,
	})
	mcpPort, err := mcpSrv.Start()
	if err != nil {
		slog.Error("start runner mcp server", "err", err)
		os.Exit(1)
	}
	defer mcpSrv.Stop()

	// Worker-scope plugins (e.g. game-krunker-harness) are loaded so the
	// runner has the same tool surface a long-lived worker would. Plugin
	// subprocesses inherit ARTIFICIAL_RUNNER_ID + ARTIFICIAL_SERVER from
	// our env, which lets a plugin like harness-plugin ping the
	// /api/runners/{id}/harness-touch endpoint to surface 🎮 on the
	// dashboard. Failure is non-fatal — we'd rather run the task without
	// the plugin than refuse to start.
	os.Setenv("ARTIFICIAL_RUNNER_ID", strconv.FormatInt(cfg.Runner.ID, 10))
	os.Setenv("ARTIFICIAL_SERVER", serverURL)
	pluginHost := pluginhost.New(protocol.PluginScopeWorker)
	if pluginList, perr := fetchPluginList(parentCtx, serverURL); perr != nil {
		slog.Warn("runner pluginhost: fetch list failed, continuing without plugins", "err", perr)
	} else if rerr := pluginHost.Reconcile(parentCtx, pluginList); rerr != nil {
		slog.Warn("runner pluginhost: reconcile failed, continuing without plugins", "err", rerr)
	} else {
		tools := pluginHost.Tools()
		mcpSrv.RegisterPluginTools(tools)
		slog.Info("runner plugins loaded", "tool_count", len(tools))
	}
	defer pluginHost.Shutdown()

	// mcpSrv is now ready — safe to start receiving WS messages.
	go hubClient.ConnectWithRetry(parentCtx)

	persona := buildRunnerPersona(cfg)
	initialPrompt := buildRunnerInitialPrompt(cfg)

	// Pipe agent stream events to debug logs — same pattern as the
	// regular worker harness; we don't repurpose them for anything
	// runner-specific in v1.
	events := make(chan agent.StreamEvent, 100)
	go func() {
		for ev := range events {
			switch ev.Type {
			case "tool_use":
				slog.Info("runner tool", "name", ev.Text)
			case "error":
				slog.Error("runner error", "msg", ev.Text)
			}
		}
	}()

	// Launch Claude inside the worktree. No channels, no company-
	// knowledge prelude — the runner persona contains everything it
	// needs for this single task. ProjectPath is the worktree root so
	// every Read/Edit/Bash Claude does is scoped to it by default.
	//
	// Tie the Claude process to runnerCtx (not parentCtx) so a
	// task_complete tool call — which cancels runnerCtx — automatically
	// terminates Claude via exec.CommandContext's SIGKILL-on-cancel
	// behavior. No manual signal handling needed.
	proc, err := agent.Start(runnerCtx, agent.Config{
		MCPPort:         mcpPort,
		ProjectPath:     cfg.Runner.WorktreePath,
		Persona:         persona,
		Nickname:        nick,
		Channels:        nil,
		Returning:       false,
		SkipChatPrelude: true,
		// Drive Claude straight into the work. The persona contains the
		// runner protocol; this initial prompt tells it to start with
		// task_describe and skip any greeting behavior.
		InitialPrompt: initialPrompt,
	}, events)
	if err != nil {
		slog.Error("start runner claude", "err", err)
		// No PID known yet — ask the server to mark us crashed.
		postRunnerStatus(serverURL, runnerID, protocol.RunnerStatusCrashed, "claude failed to start: "+err.Error())
		os.Exit(1)
	}

	// The initial prompt is the FIRST positional arg passed to claude
	// (via agent.Config.InitialPrompt) — it lands as the user turn
	// immediately after Claude reads its system prompt, so we don't
	// need a separate SendMessage goroutine to inject it later.

	// Wait for either Claude to exit on its own, or the MCP
	// task_complete tool to cancel runnerCtx. Whichever fires first
	// drives shutdown.
	procDone := make(chan agent.Result, 1)
	go func() {
		r, err := proc.Wait()
		if err != nil {
			slog.Warn("runner claude wait err", "err", err)
		}
		procDone <- r
	}()

	exitCode := 0
	exitStatus := protocol.RunnerStatusComplete
	exitReason := ""

	select {
	case <-runnerCtx.Done():
		// task_complete fired (or signal received). exec.CommandContext
		// is wired to runnerCtx so Claude has already had SIGKILL fired
		// at it; just block until proc.Wait returns so logs/transcript
		// flushes complete. 5s ceiling keeps us from hanging if the
		// PTY reader goroutine is wedged.
		slog.Info("runner: shutdown signal received, waiting for claude to exit")
		select {
		case <-procDone:
		case <-time.After(5 * time.Second):
			slog.Warn("runner: claude did not exit within 5s, abandoning")
		}
	case r := <-procDone:
		// Claude exited without task_complete. Treat as crashed.
		exitCode = r.ExitCode
		exitStatus = protocol.RunnerStatusCrashed
		exitReason = fmt.Sprintf("claude exited (code=%d) without calling task_complete", r.ExitCode)
		slog.Warn("runner: claude exited without task_complete", "exit_code", r.ExitCode)
	}

	// Tell the server which terminal state we ended in. For complete,
	// task_complete already sent MsgRunnerComplete via the WS — this
	// HTTP POST is a belt-and-braces redundancy in case the WS was
	// already torn down. For crashed, this is the only signal.
	if err := postRunnerStatus(serverURL, runnerID, exitStatus, exitReason); err != nil {
		slog.Warn("runner: final status post failed", "err", err, "status", exitStatus)
	}

	// Give the WS a moment to flush before exit so the manager
	// dashboard sees the final status.
	time.Sleep(300 * time.Millisecond)

	if exitStatus == protocol.RunnerStatusCrashed {
		os.Exit(1)
	}
	_ = exitCode
}

// handleRunnerHubMessage is the runner's WS handler. It is much
// narrower than the long-lived worker's handleHubMessage in main.go —
// chat broadcasts, task notifications, member joins are all ignored.
// The runner only listens for one thing: a manager DM (MsgWorkerNotify
// addressed at the runner's nick) telling it how to unblock.
//
// On receipt the directive is pushed straight to Claude's channel-
// notification stream so a runner blocked on task_blocked actually
// sees the manager's reply mid-thought. Without this the runner would
// remain idle forever after the MCP tool returned its "wait for
// instructions" message.
func handleRunnerHubMessage(msg protocol.WSMessage, myNick string, hc *hub.Client, mcpSrv *mcpserver.Server) {
	if msg.Type != protocol.MsgWorkerNotify {
		return
	}
	from := msg.From
	if from == "" {
		from = "manager"
	}
	slog.Info("runner: directive received", "from", from, "text", msg.Text)
	if mcpSrv == nil {
		return
	}
	mcpSrv.PushChannelNotification(
		fmt.Sprintf("[directive from %s]: %s", from, msg.Text),
		map[string]string{"chat_id": "directive", "user": from},
	)
	_ = hc
}

// runHeartbeatLoop posts to /api/runners/{id}/heartbeat every 20s while
// runnerCtx is alive. The watchdog uses last_heartbeat to detect
// runners that have wedged (alive PID, no progress).
func runHeartbeatLoop(ctx context.Context, serverURL string, runnerID int64) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			url := fmt.Sprintf("http://%s/api/runners/%d/heartbeat", serverURL, runnerID)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
			if err != nil {
				continue
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				slog.Debug("runner heartbeat failed", "err", err)
				continue
			}
			resp.Body.Close()
		}
	}
}

func fetchRunnerConfig(ctx context.Context, serverURL string, runnerID int64) (protocol.RunnerConfig, error) {
	url := fmt.Sprintf("http://%s/api/runners/%d/config", serverURL, runnerID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return protocol.RunnerConfig{}, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return protocol.RunnerConfig{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return protocol.RunnerConfig{}, fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	var cfg protocol.RunnerConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return protocol.RunnerConfig{}, err
	}
	return cfg, nil
}

func postRunnerStatus(serverURL string, runnerID int64, status, reason string) error {
	body, _ := json.Marshal(map[string]string{"status": status, "reason": reason})
	url := fmt.Sprintf("http://%s/api/runners/%d/status", serverURL, runnerID)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, b)
	}
	return nil
}

// buildRunnerPersona is the system prompt for the ephemeral runner.
// Deliberately stripped down — there is no team, no chat, no recovery
// from past sessions. Just the assignment and the four runner tools.
func buildRunnerPersona(cfg protocol.RunnerConfig) string {
	projInfo := ""
	if cfg.Project != nil && cfg.Project.Name != "" {
		projInfo = fmt.Sprintf("Project: %s\n", cfg.Project.Name)
	}
	return fmt.Sprintf(`You are a task runner — a short-lived autonomous agent assigned to ONE task.

%sWorktree: %s
Branch: %s

YOU HAVE NO CHAT. There are no teammates to message, no channels to join, no commander to ping. The only way you communicate with the rest of the system is through these four tools:

  task_describe   — re-read the assignment any time
  task_checkpoint — record progress after each milestone
  task_blocked    — escalate when stuck (blocks until manager replies)
  task_complete   — declare done; this terminates you

Workflow:
  1. Call task_describe FIRST to see your assignment. Read it carefully — the
     "Acceptance criteria" section is the contract you have to satisfy.
  2. Do the work using your standard tools (Read, Edit, Write, Bash, Grep, etc.).
  3. After each meaningful unit of work, commit and call task_checkpoint.
  4. Before declaring done, walk the Acceptance criteria one by one and
     confirm each is met. Run any builds/tests the criteria mention.
  5. Commit your work to the runner branch with descriptive messages.
  6. Call task_complete with a summary that names the commit hash(es) and
     restates which acceptance criteria were verified.

Hard rules:
  - Stay inside the worktree. Do not edit files outside it.
  - Honor the Constraints section in the task. They are not suggestions.
  - Do NOT call task_complete unless every acceptance criterion is verified.
    If a criterion can't be satisfied, call task_blocked with the specific
    bullet that's stuck, never silently skip it.
  - Never push or merge — the manager handles integration.
  - If anything is unclear, call task_blocked rather than guessing.
  - Do not introduce yourself, do not greet — just start working.
`, projInfo, cfg.Runner.WorktreePath, cfg.Runner.BranchName)
}

// buildRunnerInitialPrompt is the first user-turn message sent to
// Claude after boot. Tells it to call task_describe immediately.
func buildRunnerInitialPrompt(cfg protocol.RunnerConfig) string {
	return fmt.Sprintf(
		"You are runner %s, assigned to task #%d. Call task_describe now to read your assignment, then do the work.",
		cfg.Runner.Nickname, cfg.Task.ID,
	)
}

// buildRunnerMCPInstructions is what the gomcp.Server reports on
// initialize. Short — the persona above already covers the workflow.
func buildRunnerMCPInstructions(cfg protocol.RunnerConfig) string {
	return fmt.Sprintf(
		"Runner MCP. Tools: task_describe, task_checkpoint, task_blocked, task_complete. Runner #%d, task #%d.",
		cfg.Runner.ID, cfg.Task.ID,
	)
}

