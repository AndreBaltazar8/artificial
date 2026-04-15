package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"artificial.pt/pkg-go-shared/protocol"
)

// Plugin CRUD handlers. The persisted rows come from the DB; runtime
// state (loaded_in_workers, tools, status, last_error) is layered on top
// from the Hub in-memory aggregator populated by MsgWorkerPluginState
// reports. The aggregator lives on Hub.PluginRuntime — when it's not
// yet wired the enrich step is a no-op and the dashboard shows
// static-only rows, which is the fail-soft behaviour the UI already
// handles.

func (s *Server) apiListPlugins(w http.ResponseWriter, r *http.Request) {
	plugins, err := s.DB.ListPlugins()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.enrichPluginsRuntime(plugins)
	// ListPlugins returns nil when there are no rows; the dashboard's
	// list renderer wants a concrete [] so JSON does not serialise null.
	if plugins == nil {
		plugins = []protocol.Plugin{}
	}
	writeJSON(w, plugins)
}

type pluginCreateInput struct {
	Name    string          `json:"name"`
	Path    string          `json:"path"`
	Enabled *bool           `json:"enabled,omitempty"`
	Scope   string          `json:"scope,omitempty"`
	Config  json.RawMessage `json:"config,omitempty"`
}

func (s *Server) apiCreatePlugin(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, 400, "read body")
		return
	}
	var in pluginCreateInput
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if in.Name == "" || in.Path == "" {
		writeErr(w, 400, "name and path required")
		return
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	configJSON := "{}"
	if len(in.Config) > 0 {
		// Validate it's parseable JSON before persisting; we never store
		// a raw blob that can't round-trip.
		var probe any
		if err := json.Unmarshal(in.Config, &probe); err != nil {
			writeErr(w, 400, "config must be valid json: "+err.Error())
			return
		}
		configJSON = string(in.Config)
	}
	p, err := s.DB.UpsertPlugin(in.Name, in.Path, enabled, in.Scope, configJSON)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.enrichPluginRuntime(&p)
	s.broadcastPluginChanged(p.Name)
	writeJSON(w, p)
}

type pluginUpdateInput struct {
	Enabled *bool           `json:"enabled,omitempty"`
	Scope   *string         `json:"scope,omitempty"`
	Config  json.RawMessage `json:"config,omitempty"`
}

func (s *Server) apiUpdatePlugin(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if id == 0 {
		writeErr(w, 400, "invalid id")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, 400, "read body")
		return
	}
	var in pluginUpdateInput
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}

	// Fetch current row first so we can return 404 cleanly and know the
	// plugin name for the broadcast without an extra round-trip.
	current, err := s.DB.GetPlugin(id)
	if err != nil {
		writeErr(w, 404, "plugin not found")
		return
	}

	updated := current
	if in.Enabled != nil {
		updated, err = s.DB.SetPluginEnabled(id, *in.Enabled)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if in.Scope != nil {
		updated, err = s.DB.SetPluginScope(id, *in.Scope)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if len(in.Config) > 0 {
		var probe any
		if err := json.Unmarshal(in.Config, &probe); err != nil {
			writeErr(w, 400, "config must be valid json: "+err.Error())
			return
		}
		updated, err = s.DB.SetPluginConfig(id, string(in.Config))
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}

	s.enrichPluginRuntime(&updated)
	s.broadcastPluginChanged(updated.Name)
	writeJSON(w, updated)
}

func (s *Server) apiReloadPlugin(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if id == 0 {
		writeErr(w, 400, "invalid id")
		return
	}
	p, err := s.DB.GetPlugin(id)
	if err != nil {
		writeErr(w, 404, "plugin not found")
		return
	}
	s.broadcastPluginChanged(p.Name)
	writeJSON(w, map[string]string{"status": "reload broadcast sent", "plugin": p.Name})
}

func (s *Server) apiDeletePlugin(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if id == 0 {
		writeErr(w, 400, "invalid id")
		return
	}
	p, err := s.DB.GetPlugin(id)
	if err != nil {
		writeErr(w, 404, "plugin not found")
		return
	}
	if err := s.DB.DeletePlugin(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.broadcastPluginChanged(p.Name)
	writeJSON(w, map[string]string{"status": "deleted", "plugin": p.Name})
}

// enrichPluginsRuntime is called on list responses. It is a bulk variant
// of enrichPluginRuntime that avoids per-row Hub lookups — useful once
// the aggregator grows a single read-all method.
func (s *Server) enrichPluginsRuntime(plugins []protocol.Plugin) {
	for i := range plugins {
		s.enrichPluginRuntime(&plugins[i])
	}
}

// enrichPluginRuntime layers Hub runtime state onto a DB-sourced Plugin.
// Kept as a thin indirection so the Hub aggregator can be swapped in
// without touching every handler — today it's a no-op placeholder
// until the plugins_state.go aggregator lands on top of this commit.
func (s *Server) enrichPluginRuntime(p *protocol.Plugin) {
	if s.Hub == nil {
		return
	}
	// Best-effort runtime lookup; the aggregator hook is an interface so
	// this file compiles whether or not the aggregator is wired up yet.
	if lookup, ok := any(s.Hub).(pluginRuntimeLookup); ok {
		rt := lookup.LookupPluginRuntime(p.Name)
		p.LoadedInWorkers = rt.LoadedInWorkers
		p.Tools = rt.Tools
		p.Status = rt.Status
		p.LastError = rt.LastError
	}
	if p.Status == "" {
		if p.Enabled {
			p.Status = "enabled"
		} else {
			p.Status = "disabled"
		}
	}
}

// PluginRuntime is the shape of the per-plugin runtime state the Hub
// aggregator owns. Defined here so the api layer has something to talk
// to before the concrete aggregator struct lands.
type PluginRuntime struct {
	LoadedInWorkers int
	Tools           []string
	Status          string
	LastError       string
}

// pluginRuntimeLookup is the contract the Hub aggregator implements.
// Kept as an anonymous interface assertion in enrichPluginRuntime so
// the symbol can appear on Hub post-hoc without pulling the aggregator
// file into this commit.
type pluginRuntimeLookup interface {
	LookupPluginRuntime(name string) PluginRuntime
}

// broadcastPluginChanged notifies every connected worker that the
// plugin list on the server has changed and simultaneously reconciles
// svc-artificial's own host-scope pluginhost. The two halves run
// together because the plugin that just changed might live on either
// side — host or worker — and the dashboard has no way to narrow the
// broadcast. Workers react by re-reading /api/plugins and reconciling
// their local worker-scope pluginhost; svc-artificial reconciles its
// host-scope one in-process.
func (s *Server) broadcastPluginChanged(name string) {
	if s.Hub == nil {
		return
	}
	// Reconcile host-scope first so the post-broadcast tool list the
	// workers will fetch is already up to date — otherwise a worker
	// could win the race with MsgHostToolList and see the pre-change
	// tools.
	s.Hub.ReconcileHostPlugins(context.Background())
	s.Hub.broadcast(protocol.WSMessage{
		Type: protocol.MsgPluginChanged,
		Text: name,
	}, "")
}
