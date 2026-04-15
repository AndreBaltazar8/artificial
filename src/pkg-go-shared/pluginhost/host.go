// Package pluginhost spawns go-plugin binaries as subprocesses and
// exposes their tool surface to whoever owns the Host — historically
// cmd-worker, but after the host/worker scope split svc-artificial
// owns the default host pluginhost and cmd-worker keeps only the
// worker-scope subset.
//
// The host is deliberately thin: it does not know the names of any
// specific tools, it does not validate schema semantics, and it does
// not interpret Execute return values. Everything flows through as
// opaque bytes from the plugin to the caller and back.
//
// Lifecycle:
//
//  1. caller constructs a Host with the scope it owns ("host" for
//     svc-artificial, "worker" for cmd-worker)
//  2. caller fetches a plugin list from its source of truth (DB for
//     svc-artificial, HTTP /api/plugins for cmd-worker) and calls
//     Reconcile(ctx, plugins) — the Host filters by scope, spawns
//     newly-enabled plugins, kills disappeared ones, and keeps its
//     in-memory registry in sync
//  3. caller uses Tools()/ExecuteByTool() to answer MCP-side calls
//  4. on shutdown, caller calls Shutdown to kill every subprocess
//     cleanly
//
// Reloads (MsgPluginChanged from the server, Reload/Enable/Disable
// from the dashboard) are handled by calling Reconcile again; the
// host diffs the new list against the running set and only touches
// what actually changed.
package pluginhost

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"

	"artificial.pt/pkg-go-shared/plugin"
	"artificial.pt/pkg-go-shared/protocol"

	goplugin "github.com/hashicorp/go-plugin"
)

// Host owns the set of currently-loaded plugins for one scope.
//
// Concurrency: every public method is safe to call from multiple
// goroutines. The rwmutex (mu) guards the plugins + errors maps —
// readers (Tools, ExecuteByTool, State) take RLock, the Reconcile
// write path takes Lock around the edits only. A separate mutex
// (loadMu) serialises the full Reconcile call so two concurrent
// reloads cannot compute the same toLaunch set and spawn duplicate
// subprocesses across the unlocked spawn window.
type Host struct {
	scope string // PluginScopeHost or PluginScopeWorker

	loadMu sync.Mutex // serialises Reconcile calls end-to-end

	mu      sync.RWMutex
	plugins map[string]*LoadedPlugin // name → plugin
	errors  map[string]string        // name → load error message for disabled rows
}

// LoadedPlugin is one plugin that is currently running in a subprocess.
//
// Tools is captured once at load time from plugin.Tools() and treated
// as immutable by the host — plugins cannot hot-swap their tool list
// without being reloaded.
type LoadedPlugin struct {
	Name  string
	Tools []plugin.ToolDescriptor
	Impl  plugin.ArtificialPlugin

	client *goplugin.Client // go-plugin subprocess handle
}

// New constructs a Host for a specific scope. The scope filter is
// applied inside Reconcile: plugin rows with a different scope are
// ignored, so the same plugin list can be fed to both a host-scope
// and a worker-scope Host in different processes and each one loads
// only its own subset.
func New(scope string) *Host {
	return &Host{
		scope:   protocol.NormalizePluginScope(scope),
		plugins: make(map[string]*LoadedPlugin),
		errors:  make(map[string]string),
	}
}

// Scope returns the scope this host was constructed for.
func (h *Host) Scope() string { return h.scope }

// Reconcile reconciles the host's loaded set against `wantPlugins`.
// Plugins whose scope doesn't match the host's own scope are ignored.
// Enabled plugins that aren't loaded yet are launched; loaded plugins
// that are no longer present in the list are stopped; loaded plugins
// whose enabled flag flipped off are stopped. Called at caller startup
// and again on every plugin-change event.
//
// Errors during individual plugin launches are logged and recorded in
// the errors map — one broken plugin does not block the others from
// loading.
//
// Locking: the write lock is released around the subprocess spawn so
// concurrent Tools/State/Execute callers don't stall behind a slow
// plugin launch. A spawn can take a second or two as go-plugin does
// the handshake + RPC dial, and Reconcile is typically called from a
// hub readLoop goroutine which must not block the hub.
func (h *Host) Reconcile(ctx context.Context, wantPlugins []protocol.Plugin) error {
	// Serialise the whole Reconcile call. Two concurrent reload
	// broadcasts arriving while a Reconcile is mid-spawn would otherwise
	// compute the same toLaunch set and race on the commit
	// — one subprocess would leak on the overwrite.
	h.loadMu.Lock()
	defer h.loadMu.Unlock()

	want := make(map[string]protocol.Plugin)
	for _, p := range wantPlugins {
		if protocol.NormalizePluginScope(p.Scope) != h.scope {
			continue
		}
		if p.Enabled {
			want[p.Name] = p
		}
	}

	// Under lock: stop plugins we no longer want and compute the set
	// of plugins still missing from the registry.
	h.mu.Lock()
	for name, loaded := range h.plugins {
		if _, keep := want[name]; !keep {
			slog.Info("pluginhost: stopping plugin", "scope", h.scope, "name", name)
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
			slog.Error("pluginhost: launch failed", "scope", h.scope, "name", p.Name, "err", err)
			launchErrors[p.Name] = err.Error()
			continue
		}
		launched[p.Name] = loaded
		slog.Info("pluginhost: plugin loaded", "scope", h.scope, "name", p.Name, "tools", toolNames(loaded.Tools))
	}

	// Under lock: commit the launched plugins into the registry and
	// update error bookkeeping atomically.
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
		Name:  p.Name,
		Tools: tools,
		Impl:  impl,
		client: client,
	}, nil
}

// Shutdown kills every plugin subprocess. Called from main() on
// worker/server exit. Idempotent; calling Shutdown twice is a no-op on
// the second pass.
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
	Execute    func(argsJSON []byte) ([]byte, error)
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
					"scope", h.scope, "tool", desc.Name,
					"previous_plugin", prev, "new_plugin", loaded.Name)
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
				Execute: func(argsJSON []byte) ([]byte, error) {
					return owner.Impl.Execute(d.Name, argsJSON)
				},
			})
		}
	}
	return out
}

// ExecuteByTool looks up a tool by name across every loaded plugin and
// dispatches the call into the owning plugin's Execute. Used by the
// svc-artificial side of the host-scope RPC path: the hub hands us a
// tool name + raw args, we find the plugin that owns it, and we
// return whatever bytes the plugin produces.
//
// Returns a typed error when the tool isn't registered so the caller
// can translate it into an MsgCallToolResult error payload without
// guessing at the wire format.
func (h *Host) ExecuteByTool(toolName string, argsJSON []byte) ([]byte, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, loaded := range h.plugins {
		for _, desc := range loaded.Tools {
			if desc.Name == toolName {
				return loaded.Impl.Execute(toolName, argsJSON)
			}
		}
	}
	return nil, fmt.Errorf("pluginhost: no loaded plugin exposes tool %q", toolName)
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

// HostToolDescriptors returns the plugin-contributed tools as protocol
// wire descriptors, ready to send across MsgHostToolList. Only called
// on the svc-artificial side — workers don't speak this directly.
// Each descriptor carries its owning plugin name so the caller can
// correlate MsgCallTool results with the plugin that served them, and
// the dashboard can render "tool X from plugin Y" if it wants to.
func (h *Host) HostToolDescriptors() []protocol.HostToolDescriptor {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var out []protocol.HostToolDescriptor
	for _, loaded := range h.plugins {
		for _, desc := range loaded.Tools {
			out = append(out, protocol.HostToolDescriptor{
				Name:        desc.Name,
				Description: desc.Description,
				InputSchema: desc.InputSchema,
				PluginName:  loaded.Name,
			})
		}
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
