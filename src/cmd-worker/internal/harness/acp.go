package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"artificial.pt/cmd-worker/internal/hub"
	"artificial.pt/cmd-worker/internal/mcpserver"

	"github.com/creack/pty/v2"
)

// ACP implements Harness for ACP-compatible agents.
// Supports two communication modes:
//   - STDIO: spawns the agent process (e.g. `opencode acp`, `agent acp`) and
//     communicates via JSON-RPC 2.0 over stdin/stdout.
//   - HTTP: connects to an external ACP server via REST (POST /runs).
//
// The client implements file system and terminal operations that the agent
// delegates to us (fs/read_text_file, fs/write_text_file, terminal/*).
type ACP struct {
	cfg       Config
	mode      string // "stdio" or "http"
	hubClient *hub.Client
	mcpSrv    *mcpserver.Server
	mcpPort   int

	// STDIO mode
	proc      *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	nextID    atomic.Int64
	sessionID string

	// HTTP mode
	baseURL       string
	client        *http.Client
	httpSessionID string

	// Transcript file (agent conversation output, visible in dashboard Transcript tab)
	transcriptFile *os.File
	transcriptPath string

	// Terminal management (agent delegates shell execution to us)
	terminals   map[string]*terminal
	terminalsMu sync.Mutex
	termNextID  atomic.Int64

	// Message queue — prompts queue while one is in-flight
	promptQueue chan string
	prompting   atomic.Bool

	mu      sync.Mutex
	running bool
	done    chan struct{}
	result  Result

	// Pending JSON-RPC responses keyed by request ID (stdio mode only)
	pending   map[int64]chan *jsonrpcResponse
	pendingMu sync.Mutex
}

// terminal represents a managed terminal process for the agent.
type terminal struct {
	cmd    *exec.Cmd
	output bytes.Buffer
	outMu  sync.Mutex
	done   chan struct{}
	exit   *exitStatus
}

type exitStatus struct {
	ExitCode int     `json:"exitCode"`
	Signal   *string `json:"signal"`
}

// NewACP creates an ACP harness.
func NewACP(cfg Config, hubClient *hub.Client) *ACP {
	a := &ACP{
		cfg:         cfg,
		hubClient:   hubClient,
		done:        make(chan struct{}),
		pending:     make(map[int64]chan *jsonrpcResponse),
		terminals:   make(map[string]*terminal),
		promptQueue: make(chan string, 100),
	}
	if cfg.ACPProvider != "" {
		a.mode = "stdio"
	} else if cfg.ACPURL != "" {
		a.mode = "http"
		a.baseURL = cfg.ACPURL
		a.client = &http.Client{Timeout: 5 * time.Minute}
	}
	return a
}

// ── JSON-RPC 2.0 types ─────────────────────────────────────────────────

type jsonrpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ── HTTP mode types ─────────────────────────────────────────────────────

type acpMessagePart struct {
	Content     string `json:"content"`
	ContentType string `json:"content_type"`
}

type acpMessage struct {
	Role  string           `json:"role"`
	Parts []acpMessagePart `json:"parts"`
}

type acpRunResult struct {
	RunID   string       `json:"run_id"`
	Status  string       `json:"status"`
	Output  []acpMessage `json:"output,omitempty"`
	Session *struct {
		ID string `json:"id"`
	} `json:"session,omitempty"`
}

// ── Managed providers ───────────────────────────────────────────────────

type providerConfig struct {
	Binary string
	Args   func(projectPath string) []string
}

var managedProviders = map[string]providerConfig{
	"opencode": {
		Binary: "opencode",
		Args:   func(projectPath string) []string { return []string{"acp"} },
	},
	"cursor": {
		Binary: "agent",
		Args:   func(projectPath string) []string { return []string{"acp"} },
	},
}

// ── Harness interface ───────────────────────────────────────────────────

// TranscriptPath returns the path to the ACP transcript file.
func (a *ACP) TranscriptPath() string { return a.transcriptPath }

// Start launches the ACP agent.
func (a *ACP) Start() error {
	home, _ := os.UserHomeDir()
	logsDir := filepath.Join(home, ".config", "artificial", "logs")
	os.MkdirAll(logsDir, 0755)
	a.transcriptPath = filepath.Join(logsDir, fmt.Sprintf("worker-%s-%d.transcript", a.cfg.Nickname, time.Now().Unix()))
	if f, err := os.Create(a.transcriptPath); err == nil {
		a.transcriptFile = f
		slog.Info("acp transcript file", "path", a.transcriptPath)
	}

	// Start MCP server (provides our Artificial tools to the agent)
	if a.hubClient != nil {
		instructions := buildMCPInstructions(a.cfg.Nickname)
		a.mcpSrv = mcpserver.New(a.cfg.Nickname, a.cfg.Role, a.cfg.ProjectID, a.hubClient, instructions)
		port, err := a.mcpSrv.Start() // StreamableHTTP
		if err != nil {
			return fmt.Errorf("start mcp server: %w", err)
		}
		a.mcpPort = port
		slog.Info("acp mcp server started", "port", port)
	}

	// Write .cursor/mcp.json so the agent discovers our MCP server
	if a.mcpPort > 0 {
		a.writeCursorMCPConfig()

		// Cursor's `agent` CLI requires an interactive "trust/permission" acceptance
		// for newly discovered MCP servers. Their docs suggest launching `agent`
		// first and pressing Enter to accept, before running `agent acp`.
		//
		// We do that automatically here in a short-lived PTY so `agent acp` can run
		// non-interactively afterwards.
		if a.cfg.ACPProvider == "cursor" {
			if err := a.cursorAgentPreflightAcceptMCP(); err != nil {
				slog.Warn("cursor agent preflight accept mcp failed", "err", err)
			}
		}
	}

	switch a.mode {
	case "stdio":
		if err := a.startStdio(); err != nil {
			if a.mcpSrv != nil {
				a.mcpSrv.Stop()
			}
			return err
		}
	case "http":
		slog.Info("connecting to external ACP server", "url", a.baseURL)
	default:
		return fmt.Errorf("ACP harness requires either acp_provider or acp_url")
	}

	// Send initial instructions
	channels := a.cfg.Channels
	if len(channels) == 0 {
		channels = []string{"general"}
	}

	var initialPrompt string
	if a.cfg.Returning {
		initialPrompt = fmt.Sprintf(
			"You are back online as %s. You have been here before — do NOT re-introduce yourself. "+
				"Just check for any new messages or tasks. Your channels: %s. "+
				"After checking in, wait for incoming messages.",
			a.cfg.Nickname, joinStrings(channels),
		)
	} else {
		initialPrompt = fmt.Sprintf(
			"You are now online as %s for the first time. "+
				"Introduce yourself briefly on #general, then wait for incoming messages. "+
				"Your channels: %s",
			a.cfg.Nickname, joinStrings(channels),
		)
	}

	if a.cfg.Persona != "" {
		initialPrompt = a.cfg.Persona + "\n\n" + initialPrompt
	}

	if err := a.sendPrompt(initialPrompt); err != nil {
		slog.Error("acp initial prompt failed", "err", err)
	}

	a.mu.Lock()
	a.running = true
	a.mu.Unlock()

	// Start the message queue processor
	go a.processQueue()

	slog.Info("acp harness started", "mode", a.mode, "provider", a.cfg.ACPProvider)
	return nil
}

func (a *ACP) cursorAgentPreflightAcceptMCP() error {
	binPath, err := exec.LookPath("agent")
	if err != nil {
		return fmt.Errorf("agent binary not found: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binPath)
	cmd.Dir = a.cfg.ProjectPath
	cmd.Env = os.Environ()

	// Run `agent` as a real interactive session so it can prompt for trust.
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 100})
	if err != nil {
		return fmt.Errorf("start agent pty: %w", err)
	}
	defer ptmx.Close()

	// Drain output so the PTY buffer can't fill and block the process.
	doneCopy := make(chan struct{})
	go func() {
		io.Copy(io.Discard, ptmx)
		close(doneCopy)
	}()

	// Give `agent` time to boot, then "press Enter" to accept the MCP server.
	time.Sleep(2 * time.Second)
	_, _ = ptmx.Write([]byte{'\r'})

	// Let it persist the trust decision.
	time.Sleep(2 * time.Second)

	// Stop the preflight process. Prefer graceful shutdown.
	if cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		// If the context times out, force kill.
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-waitCh
	case <-time.After(3 * time.Second):
		// If it doesn't exit promptly, force kill.
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-waitCh
	case <-waitCh:
	}

	// Ensure the copy goroutine can finish.
	select {
	case <-doneCopy:
	case <-time.After(1 * time.Second):
	}

	return nil
}

// PushNotification delivers a message to the ACP agent. Messages are queued
// and sent sequentially — only one session/prompt can be in-flight at a time.
func (a *ACP) PushNotification(content string, meta map[string]string) {
	a.mu.Lock()
	running := a.running
	a.mu.Unlock()
	if !running {
		return
	}

	select {
	case a.promptQueue <- content:
		slog.Info("acp notification queued", "content", truncate(content, 60))
	default:
		slog.Warn("acp prompt queue full, dropping message", "content", truncate(content, 60))
	}
}

// processQueue drains the prompt queue, sending one message at a time.
// ACP's session/prompt is request-response: we must wait for the agent to
// finish processing before sending the next message.
func (a *ACP) processQueue() {
	for content := range a.promptQueue {
		a.mu.Lock()
		running := a.running
		a.mu.Unlock()
		if !running {
			return
		}

		a.prompting.Store(true)
		if err := a.sendPrompt(content); err != nil {
			slog.Error("acp prompt failed", "err", err, "content", truncate(content, 60))
		} else {
			slog.Info("acp prompt delivered", "content", truncate(content, 60))
		}
		a.prompting.Store(false)
	}
}

// Wait blocks until the ACP process exits.
func (a *ACP) Wait() Result {
	if a.proc != nil {
		err := a.proc.Wait()
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			}
		}
		a.mu.Lock()
		a.running = false
		a.result.ExitCode = exitCode
		a.result.LogPath = a.transcriptPath
		r := a.result
		a.mu.Unlock()
		return r
	}
	<-a.done
	return a.result
}

// Stop gracefully shuts down the ACP process.
func (a *ACP) Stop() {
	a.mu.Lock()
	wasRunning := a.running
	a.running = false
	a.mu.Unlock()

	// Kill all managed terminals
	a.terminalsMu.Lock()
	for _, t := range a.terminals {
		if t.cmd != nil && t.cmd.Process != nil {
			t.cmd.Process.Signal(syscall.SIGTERM)
		}
	}
	a.terminalsMu.Unlock()

	close(a.promptQueue)

	a.cleanupCursorMCPConfig()

	if a.mcpSrv != nil {
		a.mcpSrv.Stop()
	}
	if a.transcriptFile != nil {
		a.transcriptFile.Close()
	}
	if a.stdin != nil {
		a.stdin.Close()
	}
	if a.proc != nil && a.proc.Process != nil {
		slog.Info("stopping ACP process")
		a.proc.Process.Signal(syscall.SIGTERM)
	}
	if wasRunning && a.proc == nil {
		a.result = Result{ExitCode: 0}
		close(a.done)
	}
}

// ── .cursor/mcp.json management ─────────────────────────────────────────

func (a *ACP) writeCursorMCPConfig() {
	cursorDir := filepath.Join(a.cfg.ProjectPath, ".cursor")
	os.MkdirAll(cursorDir, 0755)

	mcpConfig := map[string]any{
		"mcpServers": map[string]any{
			"artificial": map[string]any{
				"url": fmt.Sprintf("http://localhost:%d/mcp", a.mcpPort),
			},
		},
	}

	data, err := json.MarshalIndent(mcpConfig, "", "  ")
	if err != nil {
		slog.Error("marshal cursor mcp config", "err", err)
		return
	}

	configPath := filepath.Join(cursorDir, "mcp.json")
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		slog.Error("write cursor mcp config", "err", err, "path", configPath)
		return
	}
	slog.Info("wrote .cursor/mcp.json", "path", configPath, "port", a.mcpPort)
}

func (a *ACP) cleanupCursorMCPConfig() {
	configPath := filepath.Join(a.cfg.ProjectPath, ".cursor", "mcp.json")
	if err := os.Remove(configPath); err == nil {
		slog.Info("removed .cursor/mcp.json", "path", configPath)
	}
}

// ── STDIO startup ───────────────────────────────────────────────────────

func (a *ACP) startStdio() error {
	provider, ok := managedProviders[a.cfg.ACPProvider]
	if !ok {
		return fmt.Errorf("unknown ACP provider: %q (supported: opencode, cursor)", a.cfg.ACPProvider)
	}

	binPath, err := exec.LookPath(provider.Binary)
	if err != nil {
		return fmt.Errorf("ACP provider %q binary not found: %w", a.cfg.ACPProvider, err)
	}

	args := provider.Args(a.cfg.ProjectPath)
	slog.Info("spawning ACP process (stdio)", "provider", a.cfg.ACPProvider, "binary", binPath)

	cmd := exec.Command(binPath, args...)
	cmd.Dir = a.cfg.ProjectPath
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn %s: %w", a.cfg.ACPProvider, err)
	}

	a.proc = cmd
	a.stdin = stdin
	a.stdout = stdout
	go a.readLoop()

	// 1. Initialize — report our capabilities (fs + terminal)
	initResult, err := a.sendRPC("initialize", map[string]any{
		"protocolVersion": 1,
		"clientInfo": map[string]string{
			"name":    "artificial",
			"version": "0.1.0",
		},
		"clientCapabilities": map[string]any{
			"fs": map[string]any{
				"readTextFile":  true,
				"writeTextFile": true,
			},
			"terminal": true,
		},
	})
	if err != nil {
		return fmt.Errorf("initialize failed: %w", err)
	}
	slog.Info("acp initialized")

	// 2. Authenticate if needed
	var initData struct {
		AuthMethods []struct {
			ID string `json:"id"`
		} `json:"authMethods"`
	}
	if initResult.Result != nil {
		json.Unmarshal(initResult.Result, &initData)
	}
	if len(initData.AuthMethods) > 0 {
		authMethod := initData.AuthMethods[0].ID
		slog.Info("acp authenticating", "method", authMethod)
		if _, err := a.sendRPC("authenticate", map[string]any{"methodId": authMethod}); err != nil {
			slog.Warn("acp authenticate failed", "err", err)
		} else {
			slog.Info("acp authenticated")
		}
	}

	// 3. Create session with MCP server config
	mcpServers := []map[string]any{}
	if a.mcpPort > 0 {
		// Our MCP server uses StreamableHTTP (SSE transport)
		mcpServers = append(mcpServers, map[string]any{
			"type":    "sse",
			"name":    "artificial",
			"url":     fmt.Sprintf("http://localhost:%d/mcp", a.mcpPort),
			"headers": []map[string]string{},
		})
	}

	sessionResult, err := a.sendRPC("session/new", map[string]any{
		"cwd":        a.cfg.ProjectPath,
		"mcpServers": mcpServers,
	})
	if err != nil {
		return fmt.Errorf("session/new failed: %w", err)
	}

	var sessionData struct {
		SessionID string `json:"sessionId"`
	}
	if sessionResult.Result != nil {
		json.Unmarshal(sessionResult.Result, &sessionData)
	}
	if sessionData.SessionID != "" {
		a.sessionID = sessionData.SessionID
		slog.Info("acp session created", "session_id", a.sessionID)
	}

	// 4. Set model if configured
	if a.cfg.Model != "" && a.sessionID != "" {
		slog.Info("acp setting model", "model", a.cfg.Model)
		if _, err := a.sendRPC("session/set_config_option", map[string]any{
			"sessionId": a.sessionID,
			"configId":  "model",
			"value":     a.cfg.Model,
		}); err != nil {
			slog.Warn("acp set model failed, trying session/set_model", "err", err, "model", a.cfg.Model)
			// Fallback to OpenCode-specific method
			if _, err := a.sendRPC("session/set_model", map[string]any{
				"sessionId": a.sessionID,
				"modelId":   a.cfg.Model,
			}); err != nil {
				slog.Warn("acp set model fallback also failed", "err", err, "model", a.cfg.Model)
			}
		}
	}

	return nil
}

// ── JSON-RPC transport ──────────────────────────────────────────────────

func (a *ACP) sendRPC(method string, params any) (*jsonrpcResponse, error) {
	id := a.nextID.Add(1)
	req := jsonrpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	// Log outbound request to transcript
	a.writeTranscript(map[string]any{"direction": "out", "type": "request", "method": method, "id": id, "params": params, "ts": time.Now().UTC().Format(time.RFC3339Nano)})

	data = append(data, '\n')

	ch := make(chan *jsonrpcResponse, 1)
	a.pendingMu.Lock()
	a.pending[id] = ch
	a.pendingMu.Unlock()
	defer func() {
		a.pendingMu.Lock()
		delete(a.pending, id)
		a.pendingMu.Unlock()
	}()

	if _, err := a.stdin.Write(data); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	resp := <-ch
	if resp == nil {
		return nil, fmt.Errorf("connection closed")
	}

	// Log response to transcript
	a.writeTranscript(map[string]any{"direction": "in", "type": "response", "id": id, "result": resp.Result, "error": resp.Error, "ts": time.Now().UTC().Format(time.RFC3339Nano)})

	if resp.Error != nil {
		return nil, fmt.Errorf("rpc error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return resp, nil
}

func (a *ACP) sendResponse(id int64, result any) {
	a.writeTranscript(map[string]any{"direction": "out", "type": "response", "id": id, "result": result, "ts": time.Now().UTC().Format(time.RFC3339Nano)})
	data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	data = append(data, '\n')
	if a.stdin != nil {
		a.stdin.Write(data)
	}
}

func (a *ACP) sendErrorResponse(id int64, code int, message string) {
	errObj := map[string]any{"code": code, "message": message}
	a.writeTranscript(map[string]any{"direction": "out", "type": "error_response", "id": id, "error": errObj, "ts": time.Now().UTC().Format(time.RFC3339Nano)})
	data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": errObj})
	data = append(data, '\n')
	if a.stdin != nil {
		a.stdin.Write(data)
	}
}

// ── Read loop — routes all incoming messages ────────────────────────────

func (a *ACP) readLoop() {
	scanner := bufio.NewScanner(a.stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var msg struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *int64          `json:"id,omitempty"`
			Method  string          `json:"method,omitempty"`
			Params  json.RawMessage `json:"params,omitempty"`
			Result  json.RawMessage `json:"result,omitempty"`
			Error   *jsonrpcError   `json:"error,omitempty"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			slog.Debug("acp: non-json", "line", truncate(string(line), 100))
			continue
		}

		if msg.ID != nil && msg.Method != "" {
			// Agent REQUEST — needs our response
			a.writeTranscript(map[string]any{"direction": "in", "type": "request", "method": msg.Method, "id": *msg.ID, "params": msg.Params, "ts": time.Now().UTC().Format(time.RFC3339Nano)})
			go a.handleAgentRequest(*msg.ID, msg.Method, msg.Params)
		} else if msg.Method != "" {
			// NOTIFICATION — no response needed
			a.writeTranscript(map[string]any{"direction": "in", "type": "notification", "method": msg.Method, "params": msg.Params, "ts": time.Now().UTC().Format(time.RFC3339Nano)})
			a.handleNotification(msg.Method, msg.Params)
		} else if msg.ID != nil {
			// RESPONSE to our request
			resp := &jsonrpcResponse{JSONRPC: msg.JSONRPC, ID: *msg.ID, Result: msg.Result, Error: msg.Error}
			a.pendingMu.Lock()
			ch, ok := a.pending[*msg.ID]
			a.pendingMu.Unlock()
			if ok {
				ch <- resp
			}
		}
	}

	if err := scanner.Err(); err != nil {
		slog.Error("acp stdout error", "err", err)
	}
	a.pendingMu.Lock()
	for id, ch := range a.pending {
		close(ch)
		delete(a.pending, id)
	}
	a.pendingMu.Unlock()
}

// ── Agent requests — methods the agent calls on us ──────────────────────

func (a *ACP) handleAgentRequest(id int64, method string, params json.RawMessage) {
	slog.Info("acp agent request", "method", method, "id", id)

	switch method {

	// ── Permission ──
	case "session/request_permission":
		a.handlePermission(id, params)

	// ── File System ──
	case "fs/read_text_file":
		a.handleFSRead(id, params)
	case "fs/write_text_file":
		a.handleFSWrite(id, params)

	// ── Terminal ──
	case "terminal/create":
		a.handleTerminalCreate(id, params)
	case "terminal/output":
		a.handleTerminalOutput(id, params)
	case "terminal/wait_for_exit":
		a.handleTerminalWaitForExit(id, params)
	case "terminal/kill":
		a.handleTerminalKill(id, params)
	case "terminal/release":
		a.handleTerminalRelease(id, params)

	default:
		slog.Warn("acp: unsupported agent method", "method", method)
		a.sendErrorResponse(id, -32601, fmt.Sprintf("method %q not supported", method))
	}
}

// ── Permission handling ─────────────────────────────────────────────────

func (a *ACP) handlePermission(id int64, params json.RawMessage) {
	var req struct {
		Options []struct {
			ID   string `json:"optionId"`
			Kind string `json:"kind"`
		} `json:"options"`
	}
	json.Unmarshal(params, &req)

	// Auto-approve: prefer allow-always, fallback to first option
	optionID := ""
	for _, o := range req.Options {
		if o.Kind == "allow_always" {
			optionID = o.ID
			break
		}
	}
	if optionID == "" && len(req.Options) > 0 {
		optionID = req.Options[0].ID
	}
	if optionID == "" {
		optionID = "allow-once"
	}

	slog.Info("acp: auto-approving permission", "option", optionID)
	a.sendResponse(id, map[string]any{
		"outcome": map[string]any{
			"outcome":  "selected",
			"optionId": optionID,
		},
	})
}

// ── File System operations ──────────────────────────────────────────────

func (a *ACP) handleFSRead(id int64, params json.RawMessage) {
	var req struct {
		Path  string `json:"path"`
		Line  *int   `json:"line,omitempty"`
		Limit *int   `json:"limit,omitempty"`
	}
	json.Unmarshal(params, &req)

	if req.Path == "" {
		a.sendErrorResponse(id, -32602, "path is required")
		return
	}

	// Resolve relative paths against project directory
	path := req.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(a.cfg.ProjectPath, path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		a.sendErrorResponse(id, -32603, fmt.Sprintf("read failed: %s", err))
		return
	}

	content := string(data)

	// Handle line/limit slicing
	if req.Line != nil || req.Limit != nil {
		lines := strings.Split(content, "\n")
		start := 0
		if req.Line != nil {
			start = *req.Line - 1 // 1-indexed
			if start < 0 {
				start = 0
			}
			if start > len(lines) {
				start = len(lines)
			}
		}
		end := len(lines)
		if req.Limit != nil {
			end = start + *req.Limit
			if end > len(lines) {
				end = len(lines)
			}
		}
		content = strings.Join(lines[start:end], "\n")
	}

	slog.Info("acp: fs/read_text_file", "path", req.Path, "bytes", len(content))
	a.sendResponse(id, map[string]any{"content": content})
}

func (a *ACP) handleFSWrite(id int64, params json.RawMessage) {
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	json.Unmarshal(params, &req)

	if req.Path == "" {
		a.sendErrorResponse(id, -32602, "path is required")
		return
	}

	path := req.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(a.cfg.ProjectPath, path)
	}

	// Ensure parent directory exists
	os.MkdirAll(filepath.Dir(path), 0755)

	if err := os.WriteFile(path, []byte(req.Content), 0644); err != nil {
		a.sendErrorResponse(id, -32603, fmt.Sprintf("write failed: %s", err))
		return
	}

	slog.Info("acp: fs/write_text_file", "path", req.Path, "bytes", len(req.Content))
	a.sendResponse(id, map[string]any{})
}

// ── Terminal operations ─────────────────────────────────────────────────

func (a *ACP) handleTerminalCreate(id int64, params json.RawMessage) {
	var req struct {
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		Cwd     string            `json:"cwd"`
		Env     map[string]string `json:"env"`
	}
	json.Unmarshal(params, &req)

	if req.Command == "" {
		a.sendErrorResponse(id, -32602, "command is required")
		return
	}

	cwd := req.Cwd
	if cwd == "" {
		cwd = a.cfg.ProjectPath
	}

	termID := fmt.Sprintf("term_%d", a.termNextID.Add(1))

	args := req.Args
	cmd := exec.Command(req.Command, args...)
	cmd.Dir = cwd

	// Merge environment
	cmd.Env = os.Environ()
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	t := &terminal{
		cmd:  cmd,
		done: make(chan struct{}),
	}

	// Capture stdout + stderr
	cmd.Stdout = &captureWriter{buf: &t.output, mu: &t.outMu}
	cmd.Stderr = &captureWriter{buf: &t.output, mu: &t.outMu}

	if err := cmd.Start(); err != nil {
		a.sendErrorResponse(id, -32603, fmt.Sprintf("exec failed: %s", err))
		return
	}

	// Wait for exit in background
	go func() {
		err := cmd.Wait()
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			}
		}
		t.outMu.Lock()
		t.exit = &exitStatus{ExitCode: exitCode}
		t.outMu.Unlock()
		close(t.done)
	}()

	a.terminalsMu.Lock()
	a.terminals[termID] = t
	a.terminalsMu.Unlock()

	cmdStr := req.Command
	if len(args) > 0 {
		cmdStr += " " + strings.Join(args, " ")
	}
	slog.Info("acp: terminal/create", "id", termID, "cmd", cmdStr)
	a.sendResponse(id, map[string]any{"terminalId": termID})
}

func (a *ACP) handleTerminalOutput(id int64, params json.RawMessage) {
	var req struct {
		TerminalID string `json:"terminalId"`
	}
	json.Unmarshal(params, &req)

	a.terminalsMu.Lock()
	t, ok := a.terminals[req.TerminalID]
	a.terminalsMu.Unlock()
	if !ok {
		a.sendErrorResponse(id, -32603, "terminal not found")
		return
	}

	t.outMu.Lock()
	output := t.output.String()
	exitSt := t.exit
	t.outMu.Unlock()

	result := map[string]any{
		"output":    output,
		"truncated": false,
	}
	if exitSt != nil {
		result["exitStatus"] = exitSt
	}
	a.sendResponse(id, result)
}

func (a *ACP) handleTerminalWaitForExit(id int64, params json.RawMessage) {
	var req struct {
		TerminalID string `json:"terminalId"`
	}
	json.Unmarshal(params, &req)

	a.terminalsMu.Lock()
	t, ok := a.terminals[req.TerminalID]
	a.terminalsMu.Unlock()
	if !ok {
		a.sendErrorResponse(id, -32603, "terminal not found")
		return
	}

	// Block until the process exits
	<-t.done

	t.outMu.Lock()
	exitSt := t.exit
	t.outMu.Unlock()

	if exitSt != nil {
		a.sendResponse(id, exitSt)
	} else {
		a.sendResponse(id, map[string]any{"exitCode": -1})
	}
}

func (a *ACP) handleTerminalKill(id int64, params json.RawMessage) {
	var req struct {
		TerminalID string `json:"terminalId"`
	}
	json.Unmarshal(params, &req)

	a.terminalsMu.Lock()
	t, ok := a.terminals[req.TerminalID]
	a.terminalsMu.Unlock()
	if !ok {
		a.sendErrorResponse(id, -32603, "terminal not found")
		return
	}

	if t.cmd != nil && t.cmd.Process != nil {
		t.cmd.Process.Signal(syscall.SIGTERM)
	}
	slog.Info("acp: terminal/kill", "id", req.TerminalID)
	a.sendResponse(id, map[string]any{})
}

func (a *ACP) handleTerminalRelease(id int64, params json.RawMessage) {
	var req struct {
		TerminalID string `json:"terminalId"`
	}
	json.Unmarshal(params, &req)

	a.terminalsMu.Lock()
	t, ok := a.terminals[req.TerminalID]
	if ok {
		delete(a.terminals, req.TerminalID)
	}
	a.terminalsMu.Unlock()

	if ok && t.cmd != nil && t.cmd.Process != nil {
		// Kill if still running
		select {
		case <-t.done:
			// Already exited
		default:
			t.cmd.Process.Signal(syscall.SIGTERM)
		}
	}

	slog.Info("acp: terminal/release", "id", req.TerminalID)
	a.sendResponse(id, map[string]any{})
}

// captureWriter is a thread-safe writer that appends to a buffer.
type captureWriter struct {
	buf *bytes.Buffer
	mu  *sync.Mutex
}

func (w *captureWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// ── Notification handling ───────────────────────────────────────────────

func (a *ACP) handleNotification(method string, params json.RawMessage) {
	// All notifications are already logged as raw JSONL in readLoop.
	// This handler is for any side-effects we need to take.
	if method == "session/update" {
		var update struct {
			Update struct {
				Type string `json:"sessionUpdate"`
			} `json:"update"`
		}
		json.Unmarshal(params, &update)
		slog.Debug("acp session/update", "type", update.Update.Type)
	}
}

// writeTranscript writes a raw JSONL entry to the transcript file.
// Every input/output to the ACP agent is logged as a JSON object per line.
func (a *ACP) writeTranscript(entry map[string]any) {
	if a.transcriptFile != nil {
		data, err := json.Marshal(entry)
		if err != nil {
			return
		}
		data = append(data, '\n')
		a.transcriptFile.Write(data)
	}
}

// ── Prompt sending ──────────────────────────────────────────────────────

func (a *ACP) sendPrompt(content string) error {
	switch a.mode {
	case "stdio":
		_, err := a.sendRPC("session/prompt", map[string]any{
			"sessionId": a.sessionID,
			"prompt":    []map[string]any{{"type": "text", "text": content}},
		})
		return err
	case "http":
		return a.sendPromptHTTP(content)
	default:
		return fmt.Errorf("no mode configured")
	}
}

func (a *ACP) sendPromptHTTP(content string) error {
	body, _ := json.Marshal(map[string]any{
		"input":      []map[string]any{{"role": "user", "parts": []map[string]any{{"content": content, "content_type": "text/plain"}}}},
		"session_id": a.httpSessionID,
		"mode":       "sync",
	})
	httpResp, err := a.client.Post(a.baseURL+"/runs", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()
	respBody, _ := io.ReadAll(httpResp.Body)
	if httpResp.StatusCode >= 400 {
		return fmt.Errorf("POST /runs: status %d: %s", httpResp.StatusCode, respBody)
	}
	var result acpRunResult
	if err := json.Unmarshal(respBody, &result); err == nil && result.Session != nil {
		a.httpSessionID = result.Session.ID
	}
	return nil
}

// ── Helpers ─────────────────────────────────────────────────────────────

func joinStrings(ss []string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += ", "
		}
		result += s
	}
	return result
}
