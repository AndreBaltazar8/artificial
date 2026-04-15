package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"artificial.pt/cmd-worker/internal/hub"
	"artificial.pt/pkg-go-shared/plugin"
	"artificial.pt/pkg-go-shared/pluginhost"
	"artificial.pt/pkg-go-shared/protocol"
)

// combinedPluginTools builds the full list of plugin-contributed MCP
// tools to register on this worker's mcpserver. Union of two sources:
//
//   - worker-scope plugins: subprocesses spawned locally by this
//     worker's own pluginhost.Host. Execute closures dispatch
//     directly into the local go-plugin RPC client.
//
//   - host-scope plugins: subprocesses spawned by svc-artificial in
//     its own process. The worker fetches the current tool list from
//     svc-artificial over the hub (MsgHostToolList) and builds
//     synthetic RegisteredTool entries whose Execute closure
//     round-trips every call back through the hub via MsgCallTool /
//     MsgCallToolResult. Reload-safety comes for free: the closure
//     re-resolves the hub.Client connection on every call, so if
//     svc-artificial relaunches a plugin subprocess the next tool
//     call routes into the fresh one without any closure
//     reattachment on this side.
//
// Both sources are best-effort — a failure in either returns whatever
// tools were successfully collected from the other, and the caller
// (harness.ReloadPluginTools) logs the error. A plugin reload that
// only affects one layer doesn't blow away the other.
func combinedPluginTools(cfg Config) []pluginhost.RegisteredTool {
	var out []pluginhost.RegisteredTool

	// Worker-scope subprocesses spawned in this process.
	if cfg.PluginHost != nil {
		out = append(out, cfg.PluginHost.Tools()...)
	}

	// Host-scope tools fetched from svc-artificial over the hub. If
	// the hub isn't connected yet (early boot, before the first
	// successful dial), we return what we have and let the
	// "on-connect" pass re-register later.
	if cfg.HubClient != nil && cfg.HubClient.IsConnected() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hostTools, err := fetchHostTools(ctx, cfg.HubClient)
		if err != nil {
			slog.Warn("host plugin tools fetch failed", "err", err)
		} else {
			out = append(out, hostTools...)
		}
	}

	return out
}

// fetchHostTools asks svc-artificial for its current host-scope tool
// surface and wraps every descriptor in a RegisteredTool whose Execute
// closure forwards calls over the hub via MsgCallTool. The closure
// captures hc by pointer so a hub reconnect is transparent — the
// next tool call just uses whatever the hub client currently owns.
func fetchHostTools(ctx context.Context, hc *hub.Client) ([]pluginhost.RegisteredTool, error) {
	resp, err := hc.Request(ctx, protocol.WSMessage{
		Type: protocol.MsgHostToolList,
	})
	if err != nil {
		return nil, fmt.Errorf("host tool list: %w", err)
	}
	var list protocol.HostToolList
	if len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, &list); err != nil {
			return nil, fmt.Errorf("decode host tool list: %w", err)
		}
	}

	out := make([]pluginhost.RegisteredTool, 0, len(list.Tools))
	for _, desc := range list.Tools {
		// Copy the loop variable so the closure below binds to its
		// own snapshot instead of re-reading the iterator slot.
		toolName := desc.Name
		pluginName := desc.PluginName
		descriptor := plugin.ToolDescriptor{
			Name:        desc.Name,
			Description: desc.Description,
			InputSchema: desc.InputSchema,
		}
		client := hc

		out = append(out, pluginhost.RegisteredTool{
			PluginName: pluginName,
			Descriptor: descriptor,
			Execute: func(argsJSON []byte) ([]byte, error) {
				callArgs := protocol.CallToolArgs{
					ToolName: toolName,
					Args:     json.RawMessage(argsJSON),
				}
				data, err := json.Marshal(callArgs)
				if err != nil {
					return nil, fmt.Errorf("marshal call args: %w", err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				resp, err := client.Request(ctx, protocol.WSMessage{
					Type: protocol.MsgCallTool,
					Data: data,
				})
				if err != nil {
					return nil, err
				}
				var result protocol.CallToolResult
				if len(resp.Data) > 0 {
					if err := json.Unmarshal(resp.Data, &result); err != nil {
						return nil, fmt.Errorf("decode call_tool_result: %w", err)
					}
				}
				if result.Error != "" {
					return nil, fmt.Errorf("%s", result.Error)
				}
				return []byte(result.Result), nil
			},
		})
	}
	return out, nil
}
