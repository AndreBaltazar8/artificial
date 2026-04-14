package server

import (
	"encoding/json"
	"log/slog"
	"sort"
	"sync"

	"artificial.pt/pkg-go-shared/protocol"
)

// pluginStateStore aggregates WorkerPluginState reports from every
// connected worker into per-plugin-name runtime info the dashboard can
// render. Ownership lives on *Hub as Hub.pluginState.
//
// The store is indexed by worker nick so a reconnect / disconnect /
// state-change from a single worker replaces that worker's slice of
// the aggregate in place, without hand-rolled delta tracking.
type pluginStateStore struct {
	mu sync.RWMutex
	// byWorker[nick] → map[pluginName] → LoadedPlugin report from that worker
	byWorker map[string]map[string]protocol.LoadedPlugin
}

func newPluginStateStore() *pluginStateStore {
	return &pluginStateStore{
		byWorker: map[string]map[string]protocol.LoadedPlugin{},
	}
}

// setWorker replaces the full per-worker snapshot with the given state.
// Workers always send their complete set of loaded plugins on every
// report, so a replace is correct — no merge semantics needed.
func (s *pluginStateStore) setWorker(nick string, state protocol.WorkerPluginState) {
	entry := make(map[string]protocol.LoadedPlugin, len(state.Plugins))
	for _, p := range state.Plugins {
		if p.Name == "" {
			continue
		}
		entry[p.Name] = p
	}
	s.mu.Lock()
	s.byWorker[nick] = entry
	s.mu.Unlock()
}

// removeWorker drops all state reported by nick. Called when a worker
// disconnects from the hub so stale counts don't persist.
func (s *pluginStateStore) removeWorker(nick string) {
	s.mu.Lock()
	delete(s.byWorker, nick)
	s.mu.Unlock()
}

// lookup aggregates every worker's view of a plugin into one PluginRuntime.
//
// Rules:
//   - LoadedInWorkers counts workers that report the plugin with no Error
//   - Tools is the deduped union of all reported tool names, sorted for
//     stable output so the dashboard row doesn't flicker between reloads
//   - Status is "error" if ANY worker reports a non-empty Error for the
//     plugin; otherwise empty so enrichPluginRuntime falls back to the
//     DB enabled/disabled label
//   - LastError is the most recent non-empty Error seen (map iteration
//     order is unspecified — acceptable, agents only see it as a hint)
func (s *pluginStateStore) lookup(name string) PluginRuntime {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var loaded int
	var lastError string
	hasError := false
	toolSet := map[string]struct{}{}

	for _, workerView := range s.byWorker {
		entry, ok := workerView[name]
		if !ok {
			continue
		}
		if entry.Error != "" {
			hasError = true
			lastError = entry.Error
			continue
		}
		loaded++
		for _, t := range entry.Tools {
			if t == "" {
				continue
			}
			toolSet[t] = struct{}{}
		}
	}

	rt := PluginRuntime{LoadedInWorkers: loaded}
	if len(toolSet) > 0 {
		rt.Tools = make([]string, 0, len(toolSet))
		for t := range toolSet {
			rt.Tools = append(rt.Tools, t)
		}
		sort.Strings(rt.Tools)
	}
	if hasError {
		rt.Status = "error"
		rt.LastError = lastError
	}
	return rt
}

// LookupPluginRuntime satisfies the pluginRuntimeLookup contract in
// plugins_api.go so enrichPluginRuntime can join runtime state onto
// DB-sourced Plugin rows. Kept on *Hub (not on the store directly) so
// the existing api.go indirection keeps working without reaching past
// the hub boundary.
func (h *Hub) LookupPluginRuntime(name string) PluginRuntime {
	if h.pluginState == nil {
		return PluginRuntime{}
	}
	return h.pluginState.lookup(name)
}

// handleWorkerPluginState is the ws.go readLoop dispatch target for
// MsgWorkerPluginState. Decodes the payload, replaces the worker's slice
// of the aggregate, and logs on malformed input so a misbehaving worker
// shows up in the server log rather than silently poisoning the count.
func (h *Hub) handleWorkerPluginState(c *client, msg protocol.WSMessage) {
	if h.pluginState == nil {
		return
	}
	var state protocol.WorkerPluginState
	if err := json.Unmarshal(msg.Data, &state); err != nil {
		slog.Warn("worker plugin state decode", "nick", c.nick, "err", err)
		return
	}
	h.pluginState.setWorker(c.nick, state)
}

// dropWorkerPluginState is called from HandleWebSocket's disconnect
// cleanup so a worker that goes offline stops contributing to the
// aggregated counts.
func (h *Hub) dropWorkerPluginState(nick string) {
	if h.pluginState == nil {
		return
	}
	h.pluginState.removeWorker(nick)
}
