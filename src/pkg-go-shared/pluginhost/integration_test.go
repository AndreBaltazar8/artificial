//go:build integration

// Integration test for the shared pluginhost Reconcile path. Spawns
// the scratch stub plugin as a real subprocess via hashicorp/go-plugin
// and asserts Reconcile → Tools → ExecuteByTool → Shutdown works
// end-to-end, plus the scope filter (worker-scope host skips a
// host-scope row and vice versa) and the reload-diff semantics.
//
// Run with:
//
//	go test -tags=integration ./pluginhost/...
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
	"path/filepath"
	"runtime"
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
	// <repo>/src/pkg-go-shared/pluginhost/integration_test.go →
	// <repo>/scratch/stub-plugin/stub-plugin
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	bin := filepath.Join(repoRoot, "scratch", "stub-plugin", "stub-plugin")
	abs, err := filepath.Abs(bin)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return abs
}

func TestIntegration_ReconcileAndExecute(t *testing.T) {
	bin := stubBinaryPath(t)

	host := New(protocol.PluginScopeHost)
	t.Cleanup(host.Shutdown)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	plugins := []protocol.Plugin{{
		ID:      1,
		Name:    "stub",
		Path:    bin,
		Enabled: true,
		Scope:   protocol.PluginScopeHost,
	}}

	if err := host.Reconcile(ctx, plugins); err != nil {
		t.Fatalf("Reconcile: %v", err)
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

	// Round-trip via the per-tool closure.
	result, err := tool.Execute([]byte(`{"question":"what?"}`))
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

	// Round-trip via ExecuteByTool (the svc-artificial dispatch path).
	result2, err := host.ExecuteByTool("stub_help", []byte(`{"question":"again?"}`))
	if err != nil {
		t.Fatalf("ExecuteByTool: %v", err)
	}
	var parsed2 map[string]any
	if err := json.Unmarshal(result2, &parsed2); err != nil {
		t.Fatalf("ExecuteByTool result not valid JSON: %v (raw=%s)", err, result2)
	}
	if parsed2["message"] != "hello from stub" {
		t.Errorf("ExecuteByTool message = %v, want hello from stub", parsed2["message"])
	}
}

func TestIntegration_ScopeFilter(t *testing.T) {
	bin := stubBinaryPath(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Feed the same plugin list ("stub" as a worker-scope row) to two
	// Host instances in different scopes. The worker-scope host must
	// load it, the host-scope host must ignore it entirely.
	plugins := []protocol.Plugin{
		{ID: 1, Name: "stub", Path: bin, Enabled: true, Scope: protocol.PluginScopeWorker},
	}

	workerHost := New(protocol.PluginScopeWorker)
	t.Cleanup(workerHost.Shutdown)
	if err := workerHost.Reconcile(ctx, plugins); err != nil {
		t.Fatalf("Reconcile worker: %v", err)
	}
	if got := len(workerHost.Tools()); got != 1 {
		t.Fatalf("worker host Tools() = %d, want 1 (scope filter dropped the worker row)", got)
	}

	hostScopeHost := New(protocol.PluginScopeHost)
	t.Cleanup(hostScopeHost.Shutdown)
	if err := hostScopeHost.Reconcile(ctx, plugins); err != nil {
		t.Fatalf("Reconcile host: %v", err)
	}
	if got := len(hostScopeHost.Tools()); got != 0 {
		t.Fatalf("host-scope host Tools() = %d, want 0 (worker-scope row leaked through filter)", got)
	}
	if got := len(hostScopeHost.State().Plugins); got != 0 {
		t.Fatalf("host-scope host State.Plugins len = %d, want 0", got)
	}

	// Flip the row to host-scope and re-run; now the host-scope host
	// must load it and the worker host must drop it on the next
	// Reconcile.
	plugins[0].Scope = protocol.PluginScopeHost
	if err := workerHost.Reconcile(ctx, plugins); err != nil {
		t.Fatalf("Reconcile worker after flip: %v", err)
	}
	if got := len(workerHost.Tools()); got != 0 {
		t.Fatalf("worker host after scope flip: %d tools, want 0", got)
	}
	if err := hostScopeHost.Reconcile(ctx, plugins); err != nil {
		t.Fatalf("Reconcile host after flip: %v", err)
	}
	if got := len(hostScopeHost.Tools()); got != 1 {
		t.Fatalf("host-scope host after scope flip: %d tools, want 1", got)
	}
}

func TestIntegration_ReconcileDiff(t *testing.T) {
	bin := stubBinaryPath(t)

	host := New(protocol.PluginScopeHost)
	t.Cleanup(host.Shutdown)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	plugins := []protocol.Plugin{{
		ID: 1, Name: "stub", Path: bin, Enabled: true, Scope: protocol.PluginScopeHost,
	}}

	if err := host.Reconcile(ctx, plugins); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	if got := len(host.Tools()); got != 1 {
		t.Fatalf("after first load: %d tools, want 1", got)
	}

	// Flip the enabled flag and reconcile again — the plugin must be
	// stopped.
	plugins[0].Enabled = false
	if err := host.Reconcile(ctx, plugins); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if got := len(host.Tools()); got != 0 {
		t.Fatalf("after disable: %d tools, want 0", got)
	}

	// Re-enable and confirm the Host relaunches.
	plugins[0].Enabled = true
	if err := host.Reconcile(ctx, plugins); err != nil {
		t.Fatalf("third Reconcile: %v", err)
	}
	if got := len(host.Tools()); got != 1 {
		t.Fatalf("after re-enable: %d tools, want 1", got)
	}

	// Sanity: the re-enabled plugin still answers via ExecuteByTool.
	_, err := host.ExecuteByTool("stub_help", []byte(`{}`))
	if err != nil {
		t.Errorf("post-reload ExecuteByTool: %v", err)
	}
}

func TestIntegration_BrokenPluginIsNonFatal(t *testing.T) {
	bin := stubBinaryPath(t)

	host := New(protocol.PluginScopeHost)
	t.Cleanup(host.Shutdown)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	plugins := []protocol.Plugin{
		{ID: 1, Name: "stub", Path: bin, Enabled: true, Scope: protocol.PluginScopeHost},
		{ID: 2, Name: "nope", Path: "/definitely/not/a/binary", Enabled: true, Scope: protocol.PluginScopeHost},
	}
	if err := host.Reconcile(ctx, plugins); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	tools := host.Tools()
	if len(tools) != 1 {
		t.Fatalf("Tools() len = %d, want 1 (broken plugin must not block stub)", len(tools))
	}

	state := host.State()
	var sawError bool
	for _, p := range state.Plugins {
		if p.Name == "nope" && p.Error != "" {
			sawError = true
		}
	}
	if !sawError {
		t.Errorf("broken plugin error not surfaced in State: %+v", state)
	}
}
