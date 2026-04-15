//go:build integration

// Integration test for the pluginhost loader. Spawns the scratch stub
// plugin as a real subprocess via hashicorp/go-plugin, points the
// loader at a fake /api/plugins served from httptest, and asserts the
// whole LoadAll → Tools → Execute → Shutdown chain.
//
// Run with:
//
//	go test -tags=integration ./internal/pluginhost/...
//
// The stub binary must be built first:
//
//	cd scratch/stub-plugin && GOWORK=off go build -o stub-plugin ./
//
// The build tag keeps this out of the default unit-test run — the
// stub binary is not part of the CI build graph and we don't want a
// missing path to trip plain `go test ./...`.
package pluginhost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"artificial.pt/pkg-go-shared/protocol"
)

// stubBinaryPath resolves the path to scratch/stub-plugin/stub-plugin
// relative to this test file, so the test works regardless of where
// it is invoked from.
func stubBinaryPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	// <repo>/src/cmd-worker/internal/pluginhost/integration_test.go →
	// <repo>/scratch/stub-plugin/stub-plugin
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..")
	bin := filepath.Join(repoRoot, "scratch", "stub-plugin", "stub-plugin")
	abs, err := filepath.Abs(bin)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return abs
}

// fakeArtificial stands up an httptest server that serves a single
// plugin row pointing at the stub binary. Replaces the real
// svc-artificial for the duration of the test.
func fakeArtificial(t *testing.T, plugins []protocol.Plugin) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/plugins", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(plugins)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// serverURL strips the scheme off httptest.URL so it matches the
// host:port shape Host.New expects.
func serverURL(t *testing.T, s *httptest.Server) string {
	t.Helper()
	return strings.TrimPrefix(s.URL, "http://")
}

func TestIntegration_LoadAllAndExecute(t *testing.T) {
	bin := stubBinaryPath(t)

	srv := fakeArtificial(t, []protocol.Plugin{{
		ID:      1,
		Name:    "stub",
		Path:    bin,
		Enabled: true,
	}})

	host := New(serverURL(t, srv))
	t.Cleanup(host.Shutdown)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := host.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	tools := host.Tools()
	if len(tools) != 1 {
		t.Fatalf("Tools() len = %d, want 1 (got %+v)", len(tools), tools)
	}
	tool := tools[0]
	if tool.PluginName != "stub" {
		t.Errorf("PluginName = %q, want %q", tool.PluginName, "stub")
	}
	if tool.Descriptor.Name != "stub_help" {
		t.Errorf("tool name = %q, want %q", tool.Descriptor.Name, "stub_help")
	}
	// Schema survived the round-trip intact.
	var schema map[string]any
	if err := json.Unmarshal(tool.Descriptor.InputSchema, &schema); err != nil {
		t.Fatalf("schema not valid JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("schema.type = %v, want object", schema["type"])
	}

	// Full round-trip: invoke the closure, verify echo envelope.
	result, err := tool.Execute(json.RawMessage(`{"question":"what?"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("result not valid JSON: %v (raw=%s)", err, result)
	}
	if parsed["message"] != "hello from stub" {
		t.Errorf("message = %v, want hello from stub", parsed["message"])
	}
	echoed, ok := parsed["echoed_args"].(map[string]any)
	if !ok {
		t.Fatalf("echoed_args missing or wrong type: %v", parsed["echoed_args"])
	}
	if echoed["question"] != "what?" {
		t.Errorf("echoed_args.question = %v, want what?", echoed["question"])
	}
}

func TestIntegration_State(t *testing.T) {
	bin := stubBinaryPath(t)
	srv := fakeArtificial(t, []protocol.Plugin{{
		ID: 1, Name: "stub", Path: bin, Enabled: true,
	}})

	host := New(serverURL(t, srv))
	t.Cleanup(host.Shutdown)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := host.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	state := host.State()
	if len(state.Plugins) != 1 {
		t.Fatalf("State.Plugins len = %d, want 1", len(state.Plugins))
	}
	entry := state.Plugins[0]
	if entry.Name != "stub" {
		t.Errorf("State entry name = %q, want stub", entry.Name)
	}
	if entry.Error != "" {
		t.Errorf("State entry unexpected error: %q", entry.Error)
	}
	if len(entry.Tools) != 1 || entry.Tools[0] != "stub_help" {
		t.Errorf("State entry tools = %v, want [stub_help]", entry.Tools)
	}
}

func TestIntegration_BrokenPluginIsNonFatal(t *testing.T) {
	bin := stubBinaryPath(t)
	srv := fakeArtificial(t, []protocol.Plugin{
		{ID: 1, Name: "stub", Path: bin, Enabled: true},
		{ID: 2, Name: "nope", Path: "/definitely/not/a/binary", Enabled: true},
	})

	host := New(serverURL(t, srv))
	t.Cleanup(host.Shutdown)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := host.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	// Stub still loads, broken plugin shows up in State with an error.
	tools := host.Tools()
	if len(tools) != 1 {
		t.Fatalf("Tools() len = %d, want 1 (broken plugin must not block stub)", len(tools))
	}
	if tools[0].PluginName != "stub" {
		t.Errorf("surviving plugin = %q, want stub", tools[0].PluginName)
	}

	state := host.State()
	var sawError bool
	for _, p := range state.Plugins {
		if p.Name == "nope" && p.Error != "" {
			sawError = true
			t.Logf("broken plugin error surfaced as expected: %s", p.Error)
		}
	}
	if !sawError {
		t.Errorf("broken plugin error not surfaced in State: %+v", state)
	}
}

func TestIntegration_ReloadDisablesPlugin(t *testing.T) {
	bin := stubBinaryPath(t)

	// Start with the stub enabled.
	plugins := []protocol.Plugin{{
		ID: 1, Name: "stub", Path: bin, Enabled: true,
	}}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/plugins", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(plugins)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	host := New(serverURL(t, srv))
	t.Cleanup(host.Shutdown)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := host.LoadAll(ctx); err != nil {
		t.Fatalf("first LoadAll: %v", err)
	}
	if got := len(host.Tools()); got != 1 {
		t.Fatalf("after first load: %d tools, want 1", got)
	}

	// Flip the enabled flag on the served response, reload.
	plugins[0].Enabled = false
	if err := host.LoadAll(ctx); err != nil {
		t.Fatalf("second LoadAll: %v", err)
	}
	if got := len(host.Tools()); got != 0 {
		t.Fatalf("after disable reload: %d tools, want 0", got)
	}

	// Re-enable and confirm reconciliation re-launches.
	plugins[0].Enabled = true
	if err := host.LoadAll(ctx); err != nil {
		t.Fatalf("third LoadAll: %v", err)
	}
	if got := len(host.Tools()); got != 1 {
		t.Fatalf("after re-enable reload: %d tools, want 1", got)
	}

	// Sanity: the re-enabled plugin still answers.
	_, err := host.Tools()[0].Execute(json.RawMessage(`{}`))
	if err != nil {
		t.Errorf("post-reload Execute: %v", err)
	}
}
