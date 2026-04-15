// Package plugin defines the contract between svc-artificial workers and
// external process plugins loaded via hashicorp/go-plugin.
//
// A plugin is an independent binary that workers launch as a subprocess on
// start. At load time the worker calls Tools() on each plugin and grafts
// every returned descriptor directly onto the worker's MCP server — the
// worker itself has no compile-time knowledge of any specific tool name.
// When an agent invokes one of those tools, the worker routes the call
// back into Execute(toolName, argsJSON) on the owning plugin.
//
// Consequences worth internalising:
//
//   - Plugin binaries own the tool namespace: names, schemas, descriptions,
//     dispatch semantics. Adding a new tool is "return one more descriptor
//     from Tools()" and does not require a worker rebuild.
//   - Artificial core stays fully plugin-agnostic. No hardcoded tool names,
//     no "if plugin == harness" branches, no per-plugin env knobs.
//   - The wire format is deliberately bytes-in / bytes-out so evolving a
//     tool's input schema is a plugin-local change; the RPC surface here
//     never needs to learn about tool-specific types.
//
// This interface is the shared contract between the `pluginhost` loader in
// cmd-worker and any plugin binary in the wider company (the harness
// plugin being the first). Additive changes only — plugins and workers may
// be built against different revisions of this module and must stay
// forward-compatible.
package plugin

import "encoding/json"

// ArtificialPlugin is the contract every plugin binary must implement.
//
// Name is the stable identifier the worker uses to address the plugin.
// It must match the `name` column in the plugins DB row that caused this
// binary to be loaded. Primarily used in logs and routing; agents never
// see it — they see the tool names exposed via Tools().
//
// Tools returns the MCP tools this plugin wants registered on every
// worker's MCP server. Called exactly once by the worker at plugin load.
// The returned slice is treated as immutable by the worker; dynamic tool
// registration after load is not supported in v1 (would require the
// plugin to signal back to the worker, which is out of scope).
//
// Execute is the single dispatch entry point the worker routes tool
// invocations through. toolName matches one of the Name fields returned
// by Tools(); argsJSON is the raw JSON body the agent submitted as the
// MCP tool input; the returned resultJSON is surfaced to the agent as a
// text content block (JSON or plain text — the plugin decides).
//
// Execute MUST be safe to call concurrently: workers may issue tool
// invocations in parallel from different agent sessions.
type ArtificialPlugin interface {
	Name() string
	Tools() []ToolDescriptor
	Execute(toolName string, argsJSON []byte) (resultJSON []byte, err error)
}

// ToolDescriptor describes a single MCP tool that a plugin wants
// registered on the worker's MCP server.
//
// Name is the tool name agents will call (e.g. "harness_command"). It
// must be unique across ALL loaded plugins on the same worker — the
// pluginhost loader rejects collisions at registration time.
//
// Description is the human-readable tool description shown to agents
// when they list available tools. Keep it short and action-oriented.
//
// InputSchema is a JSON Schema document (as raw JSON bytes) describing
// the expected shape of argsJSON. Any valid JSON Schema the MCP SDK
// accepts is fine; `{"type":"object","properties":{...}}` is the norm.
// Bytes rather than a Go struct because the worker never needs to know
// the concrete input type — it passes argsJSON through verbatim.
type ToolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}
