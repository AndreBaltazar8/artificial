package harness

import (
	"context"
	"log/slog"
	"time"

	"artificial.pt/cmd-worker/internal/agent"
	"artificial.pt/cmd-worker/internal/hub"
	"artificial.pt/cmd-worker/internal/mcpserver"
)

// Claude implements Harness and PTYHarness for the Claude CLI agent.
// This wraps the existing PTY-based Claude process with zero behavior change.
type Claude struct {
	ctx       context.Context
	cfg       Config
	hubClient *hub.Client
	mcpSrv    *mcpserver.Server
	proc      *agent.Process
	result    Result
}

// NewClaude creates a Claude harness.
func NewClaude(ctx context.Context, cfg Config, hubClient *hub.Client) *Claude {
	return &Claude{
		ctx:       ctx,
		cfg:       cfg,
		hubClient: hubClient,
	}
}

// Start launches the MCP server and Claude CLI process.
func (c *Claude) Start() error {
	instructions := buildMCPInstructions(c.cfg.Nickname)
	c.mcpSrv = mcpserver.New(c.cfg.Nickname, c.cfg.Role, c.cfg.ProjectID, c.hubClient, instructions)
	if c.cfg.PluginHost != nil {
		c.mcpSrv.RegisterPluginTools(c.cfg.PluginHost.Tools())
	}
	mcpPort, err := c.mcpSrv.Start()
	if err != nil {
		return err
	}

	events := make(chan agent.StreamEvent, 100)
	go func() {
		for ev := range events {
			switch ev.Type {
			case "text":
				slog.Debug("claude", "text", truncate(ev.Text, 100))
			case "tool_use":
				slog.Info("claude tool", "name", ev.Text)
			case "error":
				slog.Error("claude error", "msg", ev.Text)
			}
		}
	}()

	channels := c.cfg.Channels
	if len(channels) == 0 {
		channels = []string{"general"}
	}

	proc, err := agent.Start(c.ctx, agent.Config{
		MCPPort:              mcpPort,
		ProjectPath:          c.cfg.ProjectPath,
		Persona:              c.cfg.Persona,
		Nickname:             c.cfg.Nickname,
		Channels:             channels,
		Returning:            c.cfg.Returning,
		ResumeSessionID:      c.cfg.ResumeSessionID,
		CompanyKnowledgePath: c.cfg.CompanyKnowledgePath,
	}, events)
	if err != nil {
		c.mcpSrv.Stop()
		return err
	}

	c.proc = proc
	return nil
}

// PushNotification delivers a message to Claude via MCP channel notification.
func (c *Claude) PushNotification(content string, meta map[string]string) {
	if c.mcpSrv != nil {
		c.mcpSrv.PushChannelNotification(content, meta)
	}
}

// Wait blocks until Claude exits and returns the result.
func (c *Claude) Wait() Result {
	if c.proc == nil {
		return Result{ExitCode: -1}
	}
	agentResult, err := c.proc.Wait()
	if err != nil {
		slog.Error("claude exited with error", "err", err)
	}
	slog.Info("claude exited",
		"exit_code", agentResult.ExitCode,
		"session_id", agentResult.SessionID,
		"input_tokens", agentResult.InputTokens,
		"output_tokens", agentResult.OutputTokens,
	)
	return Result{
		ExitCode:     agentResult.ExitCode,
		SessionID:    agentResult.SessionID,
		LogPath:      agentResult.LogPath,
		InputTokens:  agentResult.InputTokens,
		OutputTokens: agentResult.OutputTokens,
	}
}

// Stop shuts down the MCP server.
func (c *Claude) Stop() {
	if c.mcpSrv != nil {
		c.mcpSrv.Stop()
	}
}

// TranscriptPath returns the Claude transcript path (for reporting).
func (c *Claude) TranscriptPath() string {
	if c.proc != nil {
		return c.proc.TranscriptPath()
	}
	return ""
}

// Process returns the underlying agent.Process for session discovery.
func (c *Claude) Process() *agent.Process {
	return c.proc
}

// ── PTYHarness interface ────────────────────────────────────────────────

// SendCommand sends a slash command to Claude's PTY.
func (c *Claude) SendCommand(command string) {
	if c.proc != nil {
		c.proc.SendCommand(command)
	}
}

// WriteRaw writes raw bytes to Claude's PTY stdin.
func (c *Claude) WriteRaw(data []byte) {
	if c.proc != nil {
		c.proc.WriteRaw(data)
	}
}

// Resize updates Claude's PTY window size.
func (c *Claude) Resize(size string) {
	if c.proc != nil {
		c.proc.Resize(size)
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────

func buildMCPInstructions(nick string) string {
	return `You are ` + nick + `, part of a team managed by Artificial. Messages and task updates arrive as <channel source="artificial"> notifications.

Your nickname is "` + nick + `".

Chat:
- chat_send: Send to "general" or a named channel. Do NOT use this for DMs.
- chat_dm: Private message to a member by nickname. Use this to DM someone (e.g. to: "commander").
- chat_members: See who's online.
- chat_channels: List channels.
- join_channel / leave_channel: Manage channel membership.
- set_topic: Set a channel's topic.
- channel_history: Last 3 messages from a channel. Good for catching up.
- channel_grep: Search messages by text. Returns truncated matches with IDs.
- channel_message: Get full text of a message by ID (from channel_grep results).

Tasks:
- task_create: Create a task (tied to current project).
- task_update: Update status/assignee.
- task_list: List tasks (defaults to active). Comma-separated statuses or "all".
- task_get: Get task details including project path.
- task_grep: Search tasks by title/description.
- task_subscribe: Subscribe to task update notifications.

Projects:
- project_list / project_create: Manage projects.

Recruitment:
- recruit_worker: Find a candidate matching a job description.
- recruit_accept: Hire a candidate and spawn their worker.

Commander Review (non-blocking, response arrives as channel notification):
- review_list: Check pending reviews first — avoid duplicates.
- commander_review(title, description, type, body): Request a decision.
  description supports markdown + images: ![alt](/absolute/path.png)
  Types & body format:
    choice: body={"options":[{"id":"a","label":"Option A","image_path":"/abs/path.png"}]}
    approval: body={"details":"markdown text"} — commander gets approve/reject + text input
    form: body={"html":"<form>...</form>"} — custom HTML in sandboxed iframe
    info: body={"html":"..."} — display only, no response

Task statuses: backlog, todo, in_progress, in_qa, done.`
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// WaitForTranscript polls for the Claude transcript path and returns it.
func (c *Claude) WaitForTranscript(timeout time.Duration) string {
	if c.proc == nil {
		return ""
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if tp := c.proc.TranscriptPath(); tp != "" {
			return tp
		}
		time.Sleep(2 * time.Second)
	}
	return ""
}
