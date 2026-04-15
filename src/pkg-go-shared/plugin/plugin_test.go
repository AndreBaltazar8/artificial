package plugin

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/rpc"
	"reflect"
	"testing"
)

// stubPlugin is a minimal ArtificialPlugin used to verify the netrpc
// round-trip. It records the last Execute call so tests can assert the
// argsJSON payload travelled across the wire unchanged.
type stubPlugin struct {
	lastTool string
	lastArgs []byte
	failWith error
}

func (s *stubPlugin) Name() string { return "stub" }

func (s *stubPlugin) Tools() []ToolDescriptor {
	return []ToolDescriptor{{
		Name:        "stub_tool",
		Description: "Returns a canned greeting. Echoes args back in the result.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"who":{"type":"string"}}}`),
	}}
}

func (s *stubPlugin) Execute(toolName string, argsJSON []byte) ([]byte, error) {
	s.lastTool = toolName
	s.lastArgs = append([]byte(nil), argsJSON...)
	if s.failWith != nil {
		return nil, s.failWith
	}
	return []byte(`{"ok":true,"echoed":` + string(argsJSON) + `}`), nil
}

// startInProcRPC stands up the plugin-side net/rpc server on one end of
// an in-memory socketpair and returns an rpcClient wired to the other
// end. Lets us exercise rpc.go without paying for a go-plugin subprocess
// spawn, which would need a built plugin binary on disk.
func startInProcRPC(t *testing.T, impl ArtificialPlugin) (*rpcClient, func()) {
	t.Helper()
	serverConn, clientConn := net.Pipe()

	srv := rpc.NewServer()
	if err := srv.RegisterName("Plugin", &rpcServer{Impl: impl}); err != nil {
		t.Fatalf("register: %v", err)
	}
	go srv.ServeConn(serverConn)

	c := rpc.NewClient(clientConn)
	return &rpcClient{client: c}, func() {
		_ = c.Close()
		_ = serverConn.Close()
	}
}

func TestRPCRoundTrip_NameAndTools(t *testing.T) {
	client, done := startInProcRPC(t, &stubPlugin{})
	defer done()

	if got := client.Name(); got != "stub" {
		t.Errorf("Name() = %q, want %q", got, "stub")
	}

	tools := client.Tools()
	if len(tools) != 1 {
		t.Fatalf("Tools() returned %d descriptors, want 1", len(tools))
	}
	if tools[0].Name != "stub_tool" {
		t.Errorf("tool name = %q, want %q", tools[0].Name, "stub_tool")
	}
	// Schema must survive the round-trip byte-for-byte so the worker can
	// feed it to the MCP SDK's InputSchema field unmodified.
	var schema map[string]any
	if err := json.Unmarshal(tools[0].InputSchema, &schema); err != nil {
		t.Fatalf("InputSchema not valid JSON after round-trip: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("schema.type = %v, want object", schema["type"])
	}
}

func TestRPCRoundTrip_ExecuteHappyPath(t *testing.T) {
	impl := &stubPlugin{}
	client, done := startInProcRPC(t, impl)
	defer done()

	args := []byte(`{"who":"winston"}`)
	out, err := client.Execute("stub_tool", args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Worker sees exactly what the plugin returned.
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("result not valid JSON: %v (raw=%s)", err, out)
	}
	if parsed["ok"] != true {
		t.Errorf("ok field = %v, want true", parsed["ok"])
	}

	// Plugin sees exactly what the worker sent.
	if impl.lastTool != "stub_tool" {
		t.Errorf("plugin saw tool = %q, want %q", impl.lastTool, "stub_tool")
	}
	if !reflect.DeepEqual(impl.lastArgs, args) {
		t.Errorf("plugin saw args = %q, want %q", impl.lastArgs, args)
	}
}

func TestRPCRoundTrip_ExecuteError(t *testing.T) {
	impl := &stubPlugin{failWith: errors.New("deliberate: boom")}
	client, done := startInProcRPC(t, impl)
	defer done()

	_, err := client.Execute("stub_tool", []byte(`{}`))
	if err == nil {
		t.Fatal("Execute: expected error, got nil")
	}
	if err.Error() != "deliberate: boom" {
		t.Errorf("err = %q, want %q", err.Error(), "deliberate: boom")
	}
}

// Sanity check: connecting to a closed client yields a clean error and
// does not hang. Real workers need this behaviour to notice plugin death
// within one tool call rather than blocking the MCP session forever.
func TestRPCRoundTrip_ClosedClient(t *testing.T) {
	client, done := startInProcRPC(t, &stubPlugin{})
	done() // tears the socketpair down before we call
	_, err := client.Execute("stub_tool", []byte(`{}`))
	if err == nil {
		t.Fatal("Execute on closed client: expected error, got nil")
	}
	// Accept either the rpc.ErrShutdown sentinel or io.EOF depending on
	// which side wins the teardown race.
	if !errors.Is(err, rpc.ErrShutdown) && !errors.Is(err, io.EOF) && err.Error() != "connection is shut down" {
		t.Logf("note: got %T %v (acceptable — just verifying it errors)", err, err)
	}
}
