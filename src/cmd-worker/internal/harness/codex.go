package harness

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"artificial.pt/cmd-worker/internal/hub"
	"artificial.pt/cmd-worker/internal/mcpserver"

	"github.com/creack/pty/v2"
)

// Codex implements Harness and PTYHarness for the OpenAI Codex CLI agent.
// Runs codex interactively via PTY with MCP tools. Notifications are pushed
// by writing directly to the PTY stdin since Codex has no channel mechanism.
type Codex struct {
	ctx       context.Context
	cfg       Config
	hubClient *hub.Client
	mcpSrv    *mcpserver.Server

	ptmx    *os.File
	cmd     *exec.Cmd
	logFile *os.File
	logID   string
	logPath string

	done chan codexResult
}

type codexResult struct {
	exitCode int
	err      error
}

// NewCodex creates a Codex harness.
func NewCodex(ctx context.Context, cfg Config, hubClient *hub.Client) *Codex {
	return &Codex{
		ctx:       ctx,
		cfg:       cfg,
		hubClient: hubClient,
		done:      make(chan codexResult, 1),
	}
}

// Start launches the MCP server and Codex CLI process.
func (c *Codex) Start() error {
	// Start MCP server for tools
	instructions := buildCodexMCPInstructions(c.cfg.Nickname)
	c.mcpSrv = mcpserver.New(c.cfg.Nickname, c.cfg.Role, c.cfg.ProjectID, c.hubClient, instructions)
	c.mcpSrv.RegisterPluginTools(combinedPluginTools(c.cfg))
	mcpPort, err := c.mcpSrv.Start()
	if err != nil {
		return err
	}

	mcpURL := fmt.Sprintf("http://localhost:%d/mcp", mcpPort)

	// Build system prompt
	systemPrompt := c.cfg.Persona
	if systemPrompt == "" {
		systemPrompt = fmt.Sprintf("You are %s, a team member.", c.cfg.Nickname)
	}

	channelList := "general"
	if len(c.cfg.Channels) > 0 {
		channelList = strings.Join(c.cfg.Channels, ", ")
	}

	systemPrompt += fmt.Sprintf(`

## Team Communication

You are part of a team with other agents and a human commander. Messages from teammates will appear as user messages prefixed with the sender's name.
Your nickname is "%s". Use the chat tools (from the "artificial" MCP server) to communicate with the team.

You are a member of these channels: %s
You do NOT need to call join_channel for these — you are already a member.
`, c.cfg.Nickname, channelList)

	if c.cfg.CompanyKnowledgePath != "" {
		systemPrompt += fmt.Sprintf(`
## Company Knowledge — MANDATORY

BEFORE starting any work, you MUST read: %s/README.md
This contains critical information about CLI tools, deployment workflows, and company conventions that you are expected to know and use. Failing to read this first will result in mistakes and wasted effort. Do it immediately when you come online.
`, c.cfg.CompanyKnowledgePath)
	}

	// Build initial prompt
	var initialPrompt string
	if c.cfg.Returning {
		initialPrompt = fmt.Sprintf(
			"You are back online as %s. You have been here before — do NOT re-introduce yourself. "+
				"Just check for any new messages or tasks. Your channels: %s. "+
				"After checking in, wait for incoming messages.",
			c.cfg.Nickname, strings.Join(c.cfg.Channels, ", "),
		)
	} else {
		initialPrompt = fmt.Sprintf(
			"You are now online as %s for the first time. "+
				"Introduce yourself briefly on #general, then wait for incoming messages. "+
				"Your channels: %s",
			c.cfg.Nickname, strings.Join(c.cfg.Channels, ", "),
		)
	}

	// Ensure project is trusted in ~/.codex/config.toml
	trustCodexProject(c.cfg.ProjectPath)

	// Build codex args — MCP server passed inline via -c so it's per-session
	args := []string{
		initialPrompt,
		"--dangerously-bypass-approvals-and-sandbox",
		"-C", c.cfg.ProjectPath,
		"-c", fmt.Sprintf("instructions=%q", systemPrompt),
		"-c", fmt.Sprintf("mcp_servers.artificial.url=%q", mcpURL),
	}

	model := c.cfg.Model
	if model == "" {
		model = "gpt-5.4"
	}
	args = append(args, "-m", model)

	// Log file
	logDir := filepath.Join(os.Getenv("HOME"), ".config", "artificial", "logs")
	os.MkdirAll(logDir, 0755)

	c.logID = fmt.Sprintf("%s-%s", c.cfg.Nickname, time.Now().Format("20060102-150405"))
	c.logPath = filepath.Join(logDir, c.logID+".tty")
	c.logFile, err = os.Create(c.logPath)
	if err != nil {
		slog.Warn("could not create log file", "path", c.logPath, "err", err)
		c.logFile = nil
	}
	slog.Info("log file", "id", c.logID, "path", c.logPath)

	// Launch codex with PTY
	c.cmd = exec.CommandContext(c.ctx, "codex", args...)
	c.cmd.Dir = c.cfg.ProjectPath

	slog.Info("starting codex with full pty", "args_count", len(args), "dir", c.cfg.ProjectPath)
	ptmx, err := pty.StartWithSize(c.cmd, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		if c.logFile != nil {
			c.logFile.Close()
		}
		return fmt.Errorf("start codex pty: %w", err)
	}
	c.ptmx = ptmx

	// Read PTY output and log it
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 && c.logFile != nil {
				c.logFile.Write(buf[:n])
				c.logFile.Sync()
			}
			if err != nil {
				break
			}
		}
	}()

	// Wait for process to exit
	go func() {
		err := c.cmd.Wait()
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			}
		}
		c.done <- codexResult{exitCode: exitCode, err: err}
	}()

	return nil
}

// PushNotification delivers a message to Codex by writing to PTY stdin.
// Since Codex has no channel/notification mechanism, we type the message
// directly into the interactive session. Characters are written individually
// with a small delay to work with Codex's raw-mode TUI.
func (c *Codex) PushNotification(content string, meta map[string]string) {
	if c.ptmx == nil {
		return
	}
	go func() {
		c.ptmx.Write([]byte(content))
		time.Sleep(100 * time.Millisecond)
		c.ptmx.Write([]byte{'\r'})
	}()
}

// Wait blocks until Codex exits and returns the result.
func (c *Codex) Wait() Result {
	res := <-c.done
	if c.logFile != nil {
		c.logFile.Close()
	}
	slog.Info("codex exited", "exit_code", res.exitCode)
	return Result{
		ExitCode: res.exitCode,
		LogPath:  c.logPath,
	}
}

// Stop shuts down the MCP server.
func (c *Codex) Stop() {
	if c.mcpSrv != nil {
		c.mcpSrv.Stop()
	}
}

// ReloadPluginTools re-grafts plugin-contributed tools onto the live
// MCP server after a pluginhost reload. See the Claude implementation
// for the rationale.
func (c *Codex) ReloadPluginTools() {
	if c.mcpSrv == nil {
		return
	}
	c.mcpSrv.RegisterPluginTools(combinedPluginTools(c.cfg))
}

// LogID returns the log file identifier.
func (c *Codex) LogID() string { return c.logID }

// LogPath returns the full path to the log file.
func (c *Codex) LogPath() string { return c.logPath }

// ── PTYHarness interface ────────────────────────────────────────────────

// SendCommand sends a command to Codex's PTY.
func (c *Codex) SendCommand(command string) {
	if c.ptmx != nil {
		c.ptmx.Write([]byte(command))
		c.ptmx.Write([]byte{'\t'})
	}
}

// WriteRaw writes raw bytes to Codex's PTY stdin.
func (c *Codex) WriteRaw(data []byte) {
	if c.ptmx != nil {
		c.ptmx.Write(data)
	}
}

// Resize updates Codex's PTY window size.
func (c *Codex) Resize(size string) {
	parts := strings.SplitN(size, "x", 2)
	if len(parts) != 2 {
		return
	}
	cols, rows := 0, 0
	fmt.Sscanf(parts[0], "%d", &cols)
	fmt.Sscanf(parts[1], "%d", &rows)
	if cols > 0 && rows > 0 {
		pty.Setsize(c.ptmx, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
		slog.Info("pty resized", "cols", cols, "rows", rows)
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────

// trustCodexProject ensures the project path is marked as trusted in ~/.codex/config.toml.
func trustCodexProject(projectPath string) {
	home, _ := os.UserHomeDir()
	configPath := filepath.Join(home, ".codex", "config.toml")

	data, _ := os.ReadFile(configPath)
	content := string(data)

	key := fmt.Sprintf(`[projects."%s"]`, projectPath)
	if strings.Contains(content, key) {
		return
	}

	f, err := os.OpenFile(configPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		slog.Warn("could not write codex config", "err", err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "\n%s\ntrust_level = \"trusted\"\n", key)
	slog.Info("trusted codex project", "path", projectPath)
}

func buildCodexMCPInstructions(nick string) string {
	return `You are ` + nick + `, part of a team managed by Artificial. Messages from teammates arrive as user messages.

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

Commander Review (non-blocking, response arrives as a message):
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
