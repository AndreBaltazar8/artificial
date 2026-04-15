package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"artificial.pt/pkg-go-shared/protocol"
	"artificial.pt/cmd-worker/internal/harness"
	"artificial.pt/cmd-worker/internal/hub"
	"artificial.pt/cmd-worker/internal/pluginhost"
)

func main() {
	serverURL := flag.String("server", "localhost:3456", "svc-artificial host:port")
	employeeID := flag.Int64("employee-id", 0, "Employee ID to connect as")
	preWorkerID := flag.Int64("worker-id", 0, "Pre-assigned worker ID (from spawn endpoint)")
	flag.Parse()

	if *employeeID == 0 {
		fmt.Fprintln(os.Stderr, "error: --employee-id required")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// 1. Fetch employee config from svc-artificial
	empConfig, err := fetchEmployeeConfig(*serverURL, *employeeID)
	if err != nil {
		slog.Error("fetch employee config", "err", err)
		os.Exit(1)
	}
	nick := empConfig.Employee.Nickname
	slog.Info("employee config loaded", "nick", nick, "channels", empConfig.Channels)

	// 2. Register worker (or use pre-assigned ID from spawn endpoint)
	var workerID int64
	if *preWorkerID > 0 {
		workerID = *preWorkerID
		slog.Info("using pre-assigned worker", "worker_id", workerID)
	} else {
		var err error
		workerID, err = registerWorker(*serverURL, *employeeID)
		if err != nil {
			slog.Error("register worker", "err", err)
			os.Exit(1)
		}
		slog.Info("worker registered", "worker_id", workerID)
	}
	defer updateWorkerStatus(*serverURL, workerID, "offline")

	// 3. Determine project path and channels
	projectPath := "."
	if empConfig.Project != nil && empConfig.Project.Path != "" {
		projectPath = empConfig.Project.Path
	}
	// ACP spec requires absolute cwd path
	if !filepath.IsAbs(projectPath) {
		if abs, err := filepath.Abs(projectPath); err == nil {
			projectPath = abs
		}
	}
	var projectID int64
	if empConfig.Project != nil {
		projectID = empConfig.Project.ID
	}
	channels := empConfig.Channels
	if len(channels) == 0 {
		channels = []string{"general"}
	}

	// 4. Create the harness based on employee config
	var h harness.Harness

	// 4a. Launch plugins BEFORE the harness so their tools are available
	// when mcpserver registers the toolbelt. Individual plugin load
	// failures are non-fatal — the worker boots, the broken plugin shows
	// up in State() with an Error string, and the dashboard surfaces it.
	host := pluginhost.New(*serverURL)
	if err := host.LoadAll(ctx); err != nil {
		slog.Warn("pluginhost: LoadAll failed, continuing without plugins", "err", err)
	}
	defer host.Shutdown()

	// Connect to hub WebSocket (handler set after harness is created).
	// Forwarding `host` into the handler lets MsgPluginChanged trigger a
	// reconcile — LoadAll is idempotent, so a reload just diffs the DB
	// state against what's running.
	var hubClient *hub.Client
	hubClient = hub.New(*serverURL, nick, func(msg protocol.WSMessage) {
		handleHubMessage(msg, nick, h, host, hubClient)
	})
	go hubClient.ConnectWithRetry(ctx)

	// Fire-and-forget: once the hub is connected, report the initial
	// pluginhost state so the dashboard sees what this worker loaded.
	// Retries every second until the first Send succeeds or ctx cancels.
	go reportPluginStateOnConnect(ctx, hubClient, host)

	cfg := harness.Config{
		Nickname:             nick,
		Role:                 empConfig.Employee.Role,
		Persona:              empConfig.Employee.Persona,
		Channels:             channels,
		ProjectPath:          projectPath,
		ProjectID:            projectID,
		Returning:            empConfig.Returning,
		ResumeSessionID:      empConfig.PreviousSessionID,
		CompanyKnowledgePath: empConfig.CompanyKnowledgePath,
		ACPURL:               empConfig.Employee.ACPURL,
		ACPProvider:          empConfig.Employee.ACPProvider,
		Model:                empConfig.Employee.Model,
		PluginHost:           host,
	}

	switch empConfig.Employee.Harness {
	case "acp":
		slog.Info("using ACP harness", "provider", cfg.ACPProvider, "url", cfg.ACPURL)
		h = harness.NewACP(cfg, hubClient)
	case "codex":
		slog.Info("using Codex harness", "model", cfg.Model)
		h = harness.NewCodex(ctx, cfg, hubClient)
	default:
		slog.Info("using Claude harness")
		h = harness.NewClaude(ctx, cfg, hubClient)
	}

	// 5. Start the harness
	if err := h.Start(); err != nil {
		slog.Error("start harness", "err", err)
		os.Exit(1)
	}
	defer h.Stop()

	// 6. Report transcript path based on harness type
	if ch, ok := h.(*harness.Claude); ok {
		go func() {
			if tp := ch.WaitForTranscript(40 * time.Second); tp != "" {
				slog.Info("transcript discovered", "path", tp)
				reportTranscriptPath(*serverURL, workerID, tp)
			} else {
				slog.Warn("transcript path not discovered after timeout")
			}
		}()
	}
	if ah, ok := h.(*harness.ACP); ok {
		if tp := ah.TranscriptPath(); tp != "" {
			slog.Info("acp transcript path", "path", tp)
			reportTranscriptPath(*serverURL, workerID, tp)
		}
	}
	if cx, ok := h.(*harness.Codex); ok {
		if lp := cx.LogPath(); lp != "" {
			slog.Info("codex log path", "path", lp)
			reportTranscriptPath(*serverURL, workerID, lp)
		}
	}

	// 7. Wait for harness to exit
	result := h.Wait()

	slog.Info("harness exited", "exit_code", result.ExitCode, "session_id", result.SessionID)

	if result.SessionID != "" {
		updateWorkerSessionID(*serverURL, workerID, result.SessionID)
	}
}

// reportPluginStateOnConnect sends the current host.State() over the
// hub once the WebSocket is connected. Retries every second until the
// first send succeeds because ConnectWithRetry runs asynchronously and
// there's a brief window after startup where hubClient.Send returns
// "not connected".
func reportPluginStateOnConnect(ctx context.Context, hc *hub.Client, host *pluginhost.Host) {
	for {
		if ctx.Err() != nil {
			return
		}
		if hc.IsConnected() {
			sendPluginState(hc, host)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(1 * time.Second):
		}
	}
}

// sendPluginState marshals host.State() and fires MsgWorkerPluginState
// to the server. Best-effort: the Hub aggregator treats absent reports
// as "worker has no plugins loaded", which is the correct default.
func sendPluginState(hc *hub.Client, host *pluginhost.Host) {
	data, err := json.Marshal(host.State())
	if err != nil {
		slog.Error("pluginhost: marshal state", "err", err)
		return
	}
	if err := hc.Send(protocol.WSMessage{
		Type: protocol.MsgWorkerPluginState,
		Data: data,
	}); err != nil {
		slog.Warn("pluginhost: send state", "err", err)
	}
}

// handleHubMessage processes messages from svc-artificial and pushes them to the harness.
func handleHubMessage(msg protocol.WSMessage, myNick string, h harness.Harness, host *pluginhost.Host, hc *hub.Client) {
	// MsgPluginChanged is routed to the pluginhost even before the
	// harness exists — a reload during startup must not be dropped just
	// because the harness hasn't been constructed yet.
	if msg.Type == protocol.MsgPluginChanged {
		if host == nil {
			return
		}
		slog.Info("pluginhost: reload triggered", "plugin", msg.Text)
		if err := host.LoadAll(context.Background()); err != nil {
			slog.Error("pluginhost: reload failed", "err", err)
		}
		sendPluginState(hc, host)
		return
	}

	if h == nil {
		return
	}

	switch msg.Type {
	case protocol.MsgMessage:
		var m protocol.Message
		json.Unmarshal(msg.Data, &m)
		if m.Sender == myNick {
			return // skip own messages
		}
		content := fmt.Sprintf("%s: %s", m.Sender, m.Text)
		if m.Channel != "general" {
			content = fmt.Sprintf("[#%s] %s: %s", m.Channel, m.Sender, m.Text)
		}
		h.PushNotification(content, map[string]string{
			"chat_id": m.Channel,
			"user":    m.Sender,
		})

	case protocol.MsgDM:
		var m protocol.Message
		json.Unmarshal(msg.Data, &m)
		if m.Sender == myNick {
			return
		}
		h.PushNotification(
			fmt.Sprintf("[DM from %s]: %s", m.Sender, m.Text),
			map[string]string{"chat_id": "dm-" + m.Sender, "user": m.Sender},
		)

	case protocol.MsgMemberJoined:
		if msg.Nick != myNick {
			h.PushNotification(
				fmt.Sprintf("%s joined the team", msg.Nick),
				map[string]string{"chat_id": "general", "user": msg.Nick},
			)
		}

	case protocol.MsgMemberLeft:
		h.PushNotification(
			fmt.Sprintf("%s left the team", msg.Nick),
			map[string]string{"chat_id": "general", "user": msg.Nick},
		)

	case protocol.MsgTopicChanged:
		if msg.Nick != myNick {
			h.PushNotification(
				fmt.Sprintf("%s set topic for #%s: %s", msg.Nick, msg.Channel, msg.Topic),
				map[string]string{"chat_id": msg.Channel, "user": msg.Nick},
			)
		}

	case protocol.MsgTaskCreated, protocol.MsgTaskUpdated:
		var t protocol.Task
		json.Unmarshal(msg.Data, &t)
		action := msg.Text
		if action == "" {
			if msg.Type == protocol.MsgTaskUpdated {
				action = "updated"
			} else {
				action = "created"
			}
		}
		h.PushNotification(
			fmt.Sprintf("Task #%d %q — %s — task_get(%d) for details", t.ID, t.Title, action, t.ID),
			map[string]string{"chat_id": "tasks", "user": t.CreatedBy},
		)

	case protocol.MsgReviewResponded:
		var review protocol.Review
		json.Unmarshal(msg.Data, &review)
		content := fmt.Sprintf("[Commander responded to review #%d \"%s\"]: %s", review.ID, review.Title, review.Response)
		h.PushNotification(content, map[string]string{
			"chat_id": "review",
			"user":    "commander",
		})

	case protocol.MsgWorkerNotify:
		from := msg.From
		if from == "" {
			from = "system"
		}
		h.PushNotification(
			fmt.Sprintf("[directive from %s]: %s", from, msg.Text),
			map[string]string{"chat_id": "directive", "user": from},
		)

	case protocol.MsgWorkerCommand:
		// PTY-specific: forward slash commands if harness supports it
		if ph, ok := h.(harness.PTYHarness); ok && msg.Text != "" {
			slog.Info("sending command to agent", "command", msg.Text)
			ph.SendCommand(msg.Text)
		}

	case protocol.MsgWorkerTTYInput:
		// PTY-specific: raw keystrokes
		if ph, ok := h.(harness.PTYHarness); ok && msg.Text != "" {
			ph.WriteRaw([]byte(msg.Text))
		}

	case protocol.MsgWorkerTTYResize:
		// PTY-specific: resize terminal
		if ph, ok := h.(harness.PTYHarness); ok && msg.Text != "" {
			ph.Resize(msg.Text)
		}
	}
}

// ── HTTP helpers for svc-artificial API ─────────────────────────────────

func fetchEmployeeConfig(serverURL string, employeeID int64) (protocol.EmployeeConfig, error) {
	url := fmt.Sprintf("http://%s/api/employees/%d", serverURL, employeeID)
	resp, err := http.Get(url)
	if err != nil {
		return protocol.EmployeeConfig{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return protocol.EmployeeConfig{}, fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	var config protocol.EmployeeConfig
	if err := json.NewDecoder(resp.Body).Decode(&config); err != nil {
		return protocol.EmployeeConfig{}, err
	}
	return config, nil
}

func registerWorker(serverURL string, employeeID int64) (int64, error) {
	body := fmt.Sprintf(`{"employee_id":%d,"pid":%d}`, employeeID, os.Getpid())
	resp, err := http.Post(
		fmt.Sprintf("http://%s/api/workers", serverURL),
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var w protocol.Worker
	json.NewDecoder(resp.Body).Decode(&w)
	return w.ID, nil
}

func updateWorkerStatus(serverURL string, workerID int64, status string) {
	body := fmt.Sprintf(`{"status":"%s"}`, status)
	req, _ := http.NewRequest("PUT",
		fmt.Sprintf("http://%s/api/workers/%d/status", serverURL, workerID),
		strings.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	http.DefaultClient.Do(req)
}

func updateWorkerSessionID(serverURL string, workerID int64, sessionID string) {
	body := fmt.Sprintf(`{"session_id":%q}`, sessionID)
	req, _ := http.NewRequest("PUT",
		fmt.Sprintf("http://%s/api/workers/%d/session", serverURL, workerID),
		strings.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	http.DefaultClient.Do(req)
	slog.Info("session id stored", "worker_id", workerID, "session_id", sessionID)
}

func reportTranscriptPath(serverURL string, workerID int64, path string) {
	body := fmt.Sprintf(`{"transcript_path":%q}`, path)
	req, _ := http.NewRequest("PUT",
		fmt.Sprintf("http://%s/api/workers/%d/transcript", serverURL, workerID),
		strings.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	http.DefaultClient.Do(req)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

