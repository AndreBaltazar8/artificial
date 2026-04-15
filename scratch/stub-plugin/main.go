// Command stub-plugin is a minimal ArtificialPlugin used to exercise
// the pluginhost loader end-to-end without needing the real harness
// plugin. It registers a single MCP tool named "stub_help" whose
// Execute handler echoes the argsJSON back wrapped in a predictable
// envelope so tests can assert the full round-trip.
//
// Run directly it hangs — go-plugin wants to own stdio for the
// handshake. Only meaningful when launched as a subprocess by a
// hashicorp/go-plugin client. The pluginhost integration test builds
// this binary in a tempdir, wires up a fake /api/plugins pointing at
// it, and asserts the tool reaches the MCP server.
package main

import (
	"encoding/json"
	"fmt"

	"artificial.pt/pkg-go-shared/plugin"
)

type stub struct{}

func (*stub) Name() string { return "stub" }

func (*stub) Tools() []plugin.ToolDescriptor {
	return []plugin.ToolDescriptor{{
		Name:        "stub_help",
		Description: "Canned help message from the stub plugin. Echoes input args.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"question":{"type":"string"}}}`),
	}}
}

func (*stub) Execute(toolName string, argsJSON []byte) ([]byte, error) {
	if toolName != "stub_help" {
		return nil, fmt.Errorf("unknown tool %q (stub only serves stub_help)", toolName)
	}
	// Mirror the raw input into a result envelope so integration tests
	// can verify the worker → plugin → worker bytes round-trip without
	// touching plugin-internal state.
	if len(argsJSON) == 0 {
		argsJSON = []byte(`{}`)
	}
	return []byte(`{"message":"hello from stub","echoed_args":` + string(argsJSON) + `}`), nil
}

func main() { plugin.Serve(&stub{}) }
