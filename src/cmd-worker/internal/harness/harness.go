// Package harness defines the interface for agent harnesses (claude, acp, etc.)
// and provides a factory to create the correct harness based on employee config.
package harness

import (
	"artificial.pt/cmd-worker/internal/hub"
	"artificial.pt/pkg-go-shared/pluginhost"
)

// Harness abstracts agent process lifecycle. Each agent type (claude, acp)
// implements this interface to provide a uniform way to start agents, push
// notifications, and wait for them to finish.
type Harness interface {
	// Start launches the agent process. Blocks until the agent is ready to
	// receive notifications. The caller should call Wait() in a goroutine or
	// after Start returns.
	Start() error

	// PushNotification delivers a message to the agent (chat message, task
	// update, etc.). For Claude this uses MCP channel notifications; for ACP
	// this creates a new run in the same session.
	PushNotification(content string, meta map[string]string)

	// ReloadPluginTools re-registers the harness's MCP server tools from
	// the current pluginhost snapshot. Called after pluginhost.LoadAll has
	// reconciled subprocesses (e.g. in response to a dashboard Reload or
	// Enable/Disable click) so the MCP tool handlers are re-bound to the
	// fresh RPC clients instead of the subprocesses that LoadAll just
	// killed. No-op if the harness hasn't started its mcpserver yet or
	// was constructed without a PluginHost.
	ReloadPluginTools()

	// Wait blocks until the agent process exits and returns the result.
	Wait() Result

	// Stop gracefully shuts down the agent process.
	Stop()
}

// PTYHarness extends Harness with PTY-specific operations for terminal-based
// agents like Claude CLI. Not all harnesses implement this.
type PTYHarness interface {
	Harness
	SendCommand(command string)
	WriteRaw(data []byte)
	Resize(size string)
}

// Result holds the outcome of an agent run.
type Result struct {
	ExitCode   int
	SessionID  string
	LogPath    string
	InputTokens  int
	OutputTokens int
}

// Config holds common configuration for any harness.
type Config struct {
	Nickname             string
	Role                 string
	Persona              string
	Channels             []string
	ProjectPath          string
	ProjectID            int64
	Returning            bool
	ResumeSessionID      string
	CompanyKnowledgePath string

	// Claude-specific
	MCPPort int

	// ACP-specific
	ACPURL      string // External ACP server URL (if empty, auto-spawn based on provider)
	ACPProvider string // ACP provider to auto-spawn: "opencode", etc.
	Model       string

	// PluginHost, if non-nil, is the worker-scope pluginhost the
	// harness should query for plugin-contributed tools immediately
	// after constructing its mcpserver. Populated by cmd/worker/main.go
	// after the first Reconcile succeeds.
	PluginHost *pluginhost.Host

	// HubClient, if non-nil, is the live hub connection the harness
	// uses to fetch host-scope tool descriptors from svc-artificial
	// via MsgHostToolList, and to forward tool-call RPCs via
	// MsgCallTool. Leaving it nil disables host-scope plugin forwarding
	// — the harness will only see worker-scope plugins, which is the
	// right fallback for tests and local smoke runs.
	HubClient *hub.Client
}
