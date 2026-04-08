package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/creack/pty/v2"
	"time"
)

// Config holds configuration for spawning a Claude agent.
type Config struct {
	MCPPort              int      // Port of the HTTP MCP server
	ProjectPath          string   // Working directory for Claude
	Persona              string   // System prompt / persona
	Nickname             string   // Agent's nickname
	LogDir               string   // Directory for log files
	Channels             []string // Channels the agent is a member of
	Returning            bool     // True if this agent has been spawned before
	ResumeSessionID      string   // If set, resume this Claude session instead of starting fresh
	CompanyKnowledgePath string   // Path to company knowledge directory (optional)
}

// Result holds the result of a Claude agent run.
type Result struct {
	ExitCode          int
	SessionID         string
	TranscriptPath    string // path to Claude's session transcript JSONL
	RateLimit         bool
	RateLimitResetsAt int64
	InputTokens       int
	OutputTokens      int
	LogID             string
	LogPath           string
}

// Process is a running Claude agent that you can send messages to.
type Process struct {
	stdin     io.WriteCloser
	cmd       *exec.Cmd
	parseDone chan parseOutput
	stderr    *bytes.Buffer
	logFile   *os.File
	logID     string
	logPath   string

	mu        sync.Mutex
	sessionID string
	cwd       string
}

type parseOutput struct {
	result ParseResult
	err    error
}

// Start spawns the Claude CLI and returns a Process handle.
// The caller must call Wait() to collect the result.
func Start(ctx context.Context, cfg Config, events chan<- StreamEvent) (*Process, error) {
	systemPrompt := cfg.Persona
	if systemPrompt == "" {
		systemPrompt = fmt.Sprintf("You are %s, a team member.", cfg.Nickname)
	}

	channelList := "general"
	if len(cfg.Channels) > 0 {
		channelList = strings.Join(cfg.Channels, ", ")
	}

	systemPrompt += fmt.Sprintf(`

## Team Communication

You are part of a team with other agents and a human commander. Messages arrive as <channel source="artificial"> notifications.
Your nickname is "%s". Use the chat tools to communicate with the team.

You are a member of these channels: %s
You do NOT need to call join_channel for these — you are already a member.
`, cfg.Nickname, channelList)

	if cfg.CompanyKnowledgePath != "" {
		systemPrompt += fmt.Sprintf(`
## Company Knowledge — MANDATORY

BEFORE starting any work, you MUST read: %s/README.md
This contains critical information about CLI tools, deployment workflows, and company conventions that you are expected to know and use. Failing to read this first will result in mistakes and wasted effort. Do it immediately when you come online.
`, cfg.CompanyKnowledgePath)
	}

	mcpConfig := fmt.Sprintf(
		`{"mcpServers":{"artificial":{"type":"http","url":"http://localhost:%d/mcp"}}}`,
		cfg.MCPPort,
	)

	var initialPrompt string
	if cfg.Returning {
		initialPrompt = fmt.Sprintf(
			"You are back online as %s. You have been here before — do NOT re-introduce yourself. "+
				"Just check for any new messages or tasks. Your channels: %s. "+
				"After checking in, wait for incoming messages.",
			cfg.Nickname, strings.Join(cfg.Channels, ", "),
		)
	} else {
		initialPrompt = fmt.Sprintf(
			"You are now online as %s for the first time. "+
				"Introduce yourself briefly on #general, then wait for incoming messages. "+
				"Your channels: %s",
			cfg.Nickname, strings.Join(cfg.Channels, ", "),
		)
	}

	var args []string
	if cfg.ResumeSessionID != "" {
		// Resume an existing session — Claude picks up where it left off
		slog.Info("resuming session", "session_id", cfg.ResumeSessionID)
		args = []string{
			"--resume", cfg.ResumeSessionID,
			initialPrompt, // positional prompt for the resumed session
			"--dangerously-skip-permissions",
			"--dangerously-load-development-channels", "server:artificial",
			"--verbose",
			"--mcp-config", mcpConfig,
		}
	} else {
		args = []string{
			initialPrompt, // positional prompt, interactive mode
			"--dangerously-skip-permissions",
			"--dangerously-load-development-channels", "server:artificial",
			"--verbose",
			"--mcp-config", mcpConfig,
			"--append-system-prompt", systemPrompt,
		}
	}

	// Log file
	logDir := cfg.LogDir
	if logDir == "" {
		home, _ := os.UserHomeDir()
		logDir = filepath.Join(home, ".config", "artificial", "logs")
	}
	os.MkdirAll(logDir, 0755)

	logID := fmt.Sprintf("%s-%s", cfg.Nickname, time.Now().Format("20060102-150405"))
	logPath := filepath.Join(logDir, logID+".tty")
	logFile, err := os.Create(logPath)
	if err != nil {
		slog.Warn("could not create log file", "path", logPath, "err", err)
		logFile = nil
	}
	slog.Info("log file", "id", logID, "path", logPath)

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = cfg.ProjectPath

	// Full PTY — Claude runs as a real interactive session.
	// This is required so it stays alive and receives channel notifications.
	slog.Info("starting claude with full pty", "args_count", len(args), "dir", cfg.ProjectPath)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		if logFile != nil {
			logFile.Close()
		}
		return nil, fmt.Errorf("start claude pty: %w", err)
	}

	p := &Process{
		stdin:     ptmx,
		cmd:       cmd,
		stderr:    &bytes.Buffer{},
		logFile:   logFile,
		logID:     logID,
		logPath:   logPath,
		parseDone: make(chan parseOutput, 1),
	}

	// Read PTY output. Contains ANSI + text. Parse writes raw lines to log
	// and extracts any JSON (unlikely in interactive mode, but handles it).
	go func() {
		pr, err := Parse(ptmx, logFile, events)
		p.mu.Lock()
		p.sessionID = pr.SessionID
		p.cwd = pr.CWD
		p.mu.Unlock()
		p.parseDone <- parseOutput{result: pr, err: err}
	}()

	// Accept the dev channel confirmation dialog by sending Enter via PTY.
	// Claude stays alive in interactive mode, receiving channel notifications via MCP SSE.
	go func() {
		time.Sleep(3 * time.Second)
		slog.Info("sending Enter to accept dev channel dialog")
		ptmx.Write([]byte{'\r'})

		// Try to discover session ID from ~/.claude/sessions/<pid>.json
		time.Sleep(5 * time.Second)
		p.discoverSession(cmd.Process.Pid, cfg.ProjectPath)
	}()

	return p, nil
}

// discoverSession reads ~/.claude/sessions/<pid>.json to find the session ID.
func (p *Process) discoverSession(pid int, projectPath string) {
	home, _ := os.UserHomeDir()
	sessionFile := filepath.Join(home, ".claude", "sessions", fmt.Sprintf("%d.json", pid))
	data, err := os.ReadFile(sessionFile)
	if err != nil {
		slog.Debug("session file not found", "path", sessionFile, "err", err)
		return
	}
	var info struct {
		SessionID string `json:"sessionId"`
		CWD       string `json:"cwd"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return
	}
	p.mu.Lock()
	if p.sessionID == "" && info.SessionID != "" {
		p.sessionID = info.SessionID
	}
	if p.cwd == "" {
		if info.CWD != "" {
			p.cwd = info.CWD
		} else {
			abs, _ := filepath.Abs(projectPath)
			p.cwd = abs
		}
	}
	p.mu.Unlock()
	slog.Info("discovered session", "session_id", info.SessionID, "transcript", p.TranscriptPath())
}

// SendMessage sends a user message to Claude via stdin NDJSON.
func (p *Process) SendMessage(content string) error {
	p.mu.Lock()
	sid := p.sessionID
	p.mu.Unlock()

	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": content,
		},
	}
	if sid != "" {
		msg["session_id"] = sid
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = p.stdin.Write(data)
	return err
}

// SendCommand sends a slash command (e.g. "/compact") as a user message.
func (p *Process) SendCommand(command string) error {
	return p.SendMessage(command)
}

// WriteRaw writes raw bytes directly to the PTY stdin.
func (p *Process) WriteRaw(data []byte) error {
	_, err := p.stdin.Write(data)
	return err
}

// Resize updates the PTY window size. size is "COLSxROWS".
func (p *Process) Resize(size string) {
	parts := strings.SplitN(size, "x", 2)
	if len(parts) != 2 {
		return
	}
	cols, err1 := strconv.Atoi(parts[0])
	rows, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || cols <= 0 || rows <= 0 {
		return
	}
	// stdin is the PTY master fd — use it as *os.File for ioctl
	if f, ok := p.stdin.(*os.File); ok {
		pty.Setsize(f, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
		slog.Info("pty resized", "cols", cols, "rows", rows)
	}
}

// SessionID returns the current Claude session ID (may be empty early on).
func (p *Process) SessionID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessionID
}

// TranscriptPath returns the path to Claude's session transcript JSONL file.
// Requires session_id and cwd to be known (populated after system/init).
func (p *Process) TranscriptPath() string {
	p.mu.Lock()
	sid := p.sessionID
	cwd := p.cwd
	p.mu.Unlock()
	if sid == "" || cwd == "" {
		return ""
	}
	home, _ := os.UserHomeDir()
	// Claude encodes cwd by replacing "/" with "-"
	encoded := strings.ReplaceAll(cwd, "/", "-")
	return filepath.Join(home, ".claude", "projects", encoded, sid+".jsonl")
}

// LogID returns the log file identifier.
func (p *Process) LogID() string { return p.logID }

// LogPath returns the full path to the log file.
func (p *Process) LogPath() string { return p.logPath }

// Wait waits for the Claude process to exit and returns the result.
func (p *Process) Wait() (Result, error) {
	po := <-p.parseDone

	err := p.cmd.Wait()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return Result{ExitCode: -1}, fmt.Errorf("wait claude: %w", err)
		}
	}

	if p.logFile != nil {
		p.logFile.Close()
	}

	if p.stderr.Len() > 0 {
		slog.Warn("claude stderr", "output", p.stderr.String())
	}

	return Result{
		ExitCode:          exitCode,
		SessionID:         po.result.SessionID,
		TranscriptPath:    p.TranscriptPath(),
		RateLimit:         po.result.RateLimit,
		RateLimitResetsAt: po.result.RateLimitResetsAt,
		InputTokens:       po.result.InputTokens,
		OutputTokens:      po.result.OutputTokens,
		LogID:             p.logID,
		LogPath:           p.logPath,
	}, po.err
}
