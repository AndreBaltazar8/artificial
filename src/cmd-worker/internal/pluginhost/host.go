// Package pluginhost is the worker-side counterpart to
// pkg-go-shared/plugin. It fetches the list of plugins from
// svc-artificial, launches each enabled plugin as a subprocess via
// hashicorp/go-plugin, and exposes the tools those plugins register for
// grafting onto the worker's MCP server.
//
// The host is deliberately thin: it does not know the names of any
// specific tools, it does not validate schema semantics, and it does
// not interpret Execute return values. Everything flows through as
// opaque bytes from the plugin to the MCP server and back.
//
// Lifecycle:
//
//  1. main() constructs a Host with the svc-artificial URL
//  2. main() calls LoadAll(ctx) before starting the harness — each
//     enabled plugin subprocess is spawned and its Tools() output is
//     cached in the host's in-memory registry
//  3. mcpserver reads Tools() off the host and registers one
//     AddTool-per-descriptor on the MCP server, with a handler that
//     routes the CallToolRequest args through Host.Execute into the
//     owning plugin
//  4. on shutdown, main() calls Shutdown to kill every plugin
//     subprocess cleanly
//
// Reloads (MsgPluginChanged from the server) are handled by calling
// LoadAll again; the host reconciles by stopping plugins that are
// gone/disabled and starting newly-enabled ones.
package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"artificial.pt/pkg-go-shared/plugin"
	"artificial.pt/pkg-go-shared/protocol"

	goplugin "github.com/hashicorp/go-plugin"
)

// Host owns the set of currently-loaded plugins for one worker.
//
// Concurrency: every public method is safe to call from multiple
// goroutines. The rwmutex (mu) guards the plugins + errors maps —
// readers (Tools, Execute, State) take RLock, the LoadAll write path
// takes Lock around the edits only. A separate mutex (loadMu) serialises
// the full LoadAll call so two concurrent reloads cannot compute the
// same toLaunch set and spawn duplicate subprocesses across the
// unlocked spawn window.
type Host struct {
	serverURL string

	loadMu sync.Mutex // serialises LoadAll calls end-to-end

	mu      sync.RWMutex
	plugins map[string]*LoadedPlugin // name → plugin
	errors  map[string]string         // name → load error message for disabled rows
}

// LoadedPlugin is one plugin that is currently running in a subprocess.
//
// tools is captured once at load time from plugin.Tools() and treated
// as immutable by the host — plugins cannot hot-swap their tool list
// without being reloaded.
type LoadedPlugin struct {
	Name   string
	Tools  []plugin.ToolDescriptor
	Impl   plugin.ArtificialPlugin

	client *goplugin.Client // go-plugin subprocess handle; nil before launch
}

// New constructs a Host. serverURL is the host:port of svc-artificial
// — used to GET /api/plugins during LoadAll.
func New(serverURL string) *Host {
	return &Host{
		serverURL: serverURL,
		plugins:   make(map[string]*LoadedPlugin),
		errors:    make(map[string]string),
	}
}

// LoadAll fetches the current plugin list from svc-artificial and
// reconciles the host's loaded set against it. Enabled plugins that
// aren't loaded yet are launched; loaded plugins that are no longer
// present in the list are stopped; loaded plugins whose enabled flag
// flipped off are stopped. Called at worker startup and again on every
// MsgPluginChanged broadcast.
//
// Errors during the list fetch are fatal to the reconciliation (the
// caller needs to know). Errors during individual plugin launches are
// logged and recorded in the errors map — one broken plugin does not
// block the others from loading.
//
// Locking: the write lock is released around the subprocess spawn so
// concurrent Tools/State/Execute callers don't stall behind a slow
// plugin launch. A spawn can take a second or two as go-plugin does
// the handshake + RPC dial, and LoadAll is called from the hub
// readLoop path which must not block the hub.
func (h *Host) LoadAll(ctx context.Context) error {
	// Serialise the whole LoadAll call. Two concurrent MsgPluginChanged
	// broadcasts arriving while a LoadAll is mid-spawn would otherwise
	// compute the same toLaunch set and race on the commit
	// — one subprocess would leak on the overwrite.
	h.loadMu.Lock()
	defer h.loadMu.Unlock()

	plugins, err := h.fetchPluginList(ctx)
	if err != nil {
		return fmt.Errorf("fetch plugin list: %w", err)
	}

	want := make(map[string]protocol.Plugin, len(plugins))
	for _, p := range plugins {
		if p.Enabled {
			want[p.Name] = p
		}
	}

	// Under lock: stop plugins we no longer want and compute
	// the set of plugins still missing from the registry.
	h.mu.Lock()
	for name, loaded := range h.plugins {
		if _, keep := want[name]; !keep {
			slog.Info("pluginhost: stopping plugin", "name", name)
			if loaded.client != nil {
				loaded.client.Kill()
			}
			delete(h.plugins, name)
			// Clear any stale error record from a previous failed load
			// — a plugin that has been disabled should not keep
			// showing status=error on the dashboard forever.
			delete(h.errors, name)
		}
	}
	var toLaunch []protocol.Plugin
	for name, p := range want {
		if _, already := h.plugins[name]; already {
			continue
		}
		toLaunch = append(toLaunch, p)
	}
	h.mu.Unlock()

	// Unlocked: launch newly-wanted plugins. Readers
	// (Tools/State/Execute) can proceed concurrently. Launch failures
	// are non-fatal — a broken plugin is recorded and the others still
	// load.
	launched := make(map[string]*LoadedPlugin, len(toLaunch))
	launchErrors := make(map[string]string, len(toLaunch))
	for _, p := range toLaunch {
		loaded, err := h.launch(p)
		if err != nil {
			slog.Error("pluginhost: launch failed", "name", p.Name, "err", err)
			launchErrors[p.Name] = err.Error()
			continue
		}
		launched[p.Name] = loaded
		slog.Info("pluginhost: plugin loaded", "name", p.Name, "tools", toolNames(loaded.Tools))
	}

	// Under lock: commit the launched plugins into the
	// registry and update error bookkeeping atomically.
	h.mu.Lock()
	defer h.mu.Unlock()
	for name, loaded := range launched {
		h.plugins[name] = loaded
		delete(h.errors, name)
	}
	for name, msg := range launchErrors {
		h.errors[name] = msg
	}
	return nil
}

// launch spawns a single plugin subprocess via hashicorp/go-plugin and
// captures the ArtificialPlugin handle + the descriptor list.
//
// Caller must NOT hold h.mu — this call blocks on subprocess spawn.
func (h *Host) launch(p protocol.Plugin) (*LoadedPlugin, error) {
	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig: plugin.Handshake,
		Plugins:         plugin.PluginMap,
		Cmd:             exec.Command(p.Path),
		Logger:          nil, // fall back to go-plugin's default hclog
	})

	rpcClient, err := client.Client()
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("connect: %w", err)
	}

	raw, err := rpcClient.Dispense("artificial")
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("dispense: %w", err)
	}

	impl, ok := raw.(plugin.ArtificialPlugin)
	if !ok {
		client.Kill()
		return nil, fmt.Errorf("plugin does not implement ArtificialPlugin (got %T)", raw)
	}

	// Cross-check the plugin's self-reported Name with the DB row —
	// a mismatch is almost always a misconfigured path pointing at the
	// wrong binary, which would silently register tools under the
	// wrong owner.
	got := impl.Name()
	if got != p.Name {
		client.Kill()
		return nil, fmt.Errorf("plugin name mismatch: DB says %q, binary reports %q", p.Name, got)
	}

	tools := impl.Tools()
	return &LoadedPlugin{
		Name:   p.Name,
		Tools:  tools,
		Impl:   impl,
		client: client,
	}, nil
}

// fetchHTTPClient is used by fetchPluginList. Has an explicit timeout
// so a hung svc-artificial cannot wedge a worker indefinitely, even if
// a caller passes ctx.Background() — the timeout is belt+braces on top
// of the ctx deadline LoadAll already respects.
var fetchHTTPClient = &http.Client{Timeout: 10 * time.Second}

// fetchPluginList does one GET /api/plugins round-trip.
func (h *Host) fetchPluginList(ctx context.Context) ([]protocol.Plugin, error) {
	url := fmt.Sprintf("http://%s/api/plugins", h.serverURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := fetchHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	var plugins []protocol.Plugin
	if err := json.NewDecoder(resp.Body).Decode(&plugins); err != nil {
		return nil, err
	}
	return plugins, nil
}

// Shutdown kills every plugin subprocess. Called from main() on worker
// exit — tied into the same defer chain as the harness teardown.
// Idempotent; calling Shutdown twice is a no-op on the second pass.
func (h *Host) Shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for name, loaded := range h.plugins {
		if loaded.client != nil {
			loaded.client.Kill()
		}
		delete(h.plugins, name)
	}
}

// RegisteredTool is the shape mcpserver consumes when it enumerates
// the plugin-contributed tools it needs to register on its MCP server.
// Execute is a closure that owns the routing to the correct plugin so
// mcpserver never has to know which plugin a tool came from.
type RegisteredTool struct {
	PluginName string
	Descriptor plugin.ToolDescriptor
	Execute    func(argsJSON json.RawMessage) (json.RawMessage, error)
}

// Tools returns a flat list of every tool across every currently-loaded
// plugin, each wired up with a closure that routes Execute calls back
// to the owning plugin. Safe to call repeatedly — the MCP server is
// expected to re-register on reload.
//
// If two plugins register tools with the same name, the second one
// wins (last-write-wins on the map). This is consistent with the
// MCP SDK's own AddTool semantics and keeps the reload path simple.
func (h *Host) Tools() []RegisteredTool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var out []RegisteredTool
	seen := make(map[string]string) // tool name → plugin name, for collision logging
	for _, loaded := range h.plugins {
		for _, desc := range loaded.Tools {
			if prev, dup := seen[desc.Name]; dup {
				slog.Warn("pluginhost: tool name collision",
					"tool", desc.Name, "previous_plugin", prev, "new_plugin", loaded.Name)
			}
			seen[desc.Name] = loaded.Name

			// Capture loaded by value so the closure binds to the
			// correct plugin across loop iterations — a classic Go
			// loop-variable trap if you forget.
			owner := loaded
			d := desc
			out = append(out, RegisteredTool{
				PluginName: loaded.Name,
				Descriptor: desc,
				Execute: func(argsJSON json.RawMessage) (json.RawMessage, error) {
					result, err := owner.Impl.Execute(d.Name, []byte(argsJSON))
					if err != nil {
						return nil, err
					}
					return json.RawMessage(result), nil
				},
			})
		}
	}
	return out
}

// State returns a WorkerPluginState snapshot suitable for sending over
// MsgWorkerPluginState. Includes both successfully loaded plugins and
// plugins that failed to load (with their error strings) — the Hub
// aggregator on the server surfaces both on the dashboard.
func (h *Host) State() protocol.WorkerPluginState {
	h.mu.RLock()
	defer h.mu.RUnlock()

	out := protocol.WorkerPluginState{
		Plugins: make([]protocol.LoadedPlugin, 0, len(h.plugins)+len(h.errors)),
	}
	for name, loaded := range h.plugins {
		out.Plugins = append(out.Plugins, protocol.LoadedPlugin{
			Name:  name,
			Tools: toolNames(loaded.Tools),
		})
	}
	for name, errMsg := range h.errors {
		out.Plugins = append(out.Plugins, protocol.LoadedPlugin{
			Name:  name,
			Error: errMsg,
		})
	}
	return out
}

func toolNames(tools []plugin.ToolDescriptor) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name
	}
	return out
}
