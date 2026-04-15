package server

import (
	"context"
	"encoding/json"
	"log/slog"

	"artificial.pt/pkg-go-shared/protocol"
)

// Plugin host scoping on svc-artificial.
//
// svc-artificial runs its own pluginhost.Host (Hub.pluginHost), scoped
// to PluginScopeHost. Plugin rows flagged `scope = 'host'` (the default
// after migration 3) live there — the subprocess is spawned in this
// process, and workers reach the plugin over the hub WebSocket via
// MsgHostToolList + MsgCallTool. Worker-scoped plugins (`scope =
// 'worker'`) are ignored here and handled by each worker's own
// pluginhost.Host as before.
//
// The lifecycle hooks below are called from:
//
//   - main() on startup, via ReconcileHostPlugins(), before the HTTP
//     server begins accepting WS connections, so the host tool list is
//     populated the first time a worker asks for it.
//   - broadcastPluginChanged() on plugin mutation / dashboard reload,
//     so host-side plugins reload in lock-step with the worker-side
//     MsgPluginChanged broadcast.
//   - Server.Run() defer path via ShutdownHostPlugins(), so every
//     subprocess dies cleanly on SIGINT / process exit.

// ReconcileHostPlugins reads the current plugin list from the DB and
// hands the host-scope subset to the underlying pluginhost.Host for
// reconciliation. Called on startup and after every plugin mutation.
// Non-fatal: a broken plugin is logged and recorded in the Host's
// errors map; the others still launch.
func (h *Hub) ReconcileHostPlugins(ctx context.Context) {
	if h.pluginHost == nil {
		return
	}
	plugins, err := h.db.ListPlugins()
	if err != nil {
		slog.Error("pluginhost: ListPlugins failed", "err", err)
		return
	}
	if err := h.pluginHost.Reconcile(ctx, plugins); err != nil {
		slog.Error("pluginhost: Reconcile failed", "err", err)
		return
	}
	state := h.pluginHost.State()
	// Fold the host-side state into the per-worker aggregator under a
	// synthetic "(host)" nick so the dashboard's "loaded in N workers"
	// count reflects both worker-side subprocesses and the host-side
	// one. Without this the dashboard would say a host-scope plugin
	// is "loaded in 0 workers" even when svc-artificial is running it
	// correctly and every worker can reach it.
	h.pluginState.setWorker("(host)", state)
}

// ShutdownHostPlugins kills every host-scope subprocess. Idempotent.
func (h *Hub) ShutdownHostPlugins() {
	if h.pluginHost == nil {
		return
	}
	h.pluginHost.Shutdown()
}

// handleHostToolList answers a worker's MsgHostToolList request with
// the current host-scope tool surface. Response is keyed by the
// request's RequestID so the worker's hub.Client.Request() can resolve
// its pending entry. The response MUST carry the same RequestID or the
// worker sits on a 60 s timeout.
func (h *Hub) handleHostToolList(c *client, msg protocol.WSMessage) {
	if h.pluginHost == nil {
		h.sendTo(c.nick, protocol.WSMessage{
			Type:      protocol.MsgHostToolList,
			RequestID: msg.RequestID,
			Data:      jsonMustMarshal(protocol.HostToolList{}),
		})
		return
	}
	list := protocol.HostToolList{
		Tools: h.pluginHost.HostToolDescriptors(),
	}
	h.sendTo(c.nick, protocol.WSMessage{
		Type:      protocol.MsgHostToolList,
		RequestID: msg.RequestID,
		Data:      jsonMustMarshal(list),
	})
}

// handleCallTool dispatches a worker-originated tool call into the
// locally-hosted plugin subprocess owning the named tool. Runs the
// Execute in a goroutine so a slow plugin cannot stall the hub's
// readLoop — readLoop is single-threaded per client, and blocking it
// would queue up every other message from that worker until the
// Execute returns. The response is still sent back via sendTo with
// the original RequestID, so the worker's hub.Client.Request() on
// the other side resolves its pending entry by correlation ID, not
// by read order.
func (h *Hub) handleCallTool(c *client, msg protocol.WSMessage) {
	reqID := msg.RequestID
	senderNick := c.nick

	var args protocol.CallToolArgs
	if err := json.Unmarshal(msg.Data, &args); err != nil {
		h.sendTo(senderNick, protocol.WSMessage{
			Type:      protocol.MsgCallToolResult,
			RequestID: reqID,
			Data:      jsonMustMarshal(protocol.CallToolResult{Error: "pluginhost: invalid call args: " + err.Error()}),
		})
		return
	}
	if args.ToolName == "" {
		h.sendTo(senderNick, protocol.WSMessage{
			Type:      protocol.MsgCallToolResult,
			RequestID: reqID,
			Data:      jsonMustMarshal(protocol.CallToolResult{Error: "pluginhost: tool_name is required"}),
		})
		return
	}

	host := h.pluginHost
	if host == nil {
		h.sendTo(senderNick, protocol.WSMessage{
			Type:      protocol.MsgCallToolResult,
			RequestID: reqID,
			Data:      jsonMustMarshal(protocol.CallToolResult{Error: "pluginhost: host is not initialised"}),
		})
		return
	}

	go func() {
		result, err := host.ExecuteByTool(args.ToolName, args.Args)
		payload := protocol.CallToolResult{}
		if err != nil {
			payload.Error = err.Error()
		} else {
			// ExecuteByTool returns an opaque byte slice the plugin's
			// Execute produced. Pass it through as RawMessage so the
			// worker's MCP handler doesn't double-decode.
			payload.Result = json.RawMessage(result)
		}
		h.sendTo(senderNick, protocol.WSMessage{
			Type:      protocol.MsgCallToolResult,
			RequestID: reqID,
			Data:      jsonMustMarshal(payload),
		})
	}()
}

// jsonMustMarshal is a tight helper for response payloads. The shapes
// we marshal here are all protocol-defined and guaranteed JSON-safe;
// a marshal error would be a programming bug, so we log it loudly and
// fall through to an empty object rather than propagate.
func jsonMustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		slog.Error("pluginhost: json marshal failed", "err", err, "type", trimTypeName(v))
		return json.RawMessage(`{}`)
	}
	return b
}

func trimTypeName(v any) string {
	return (func() string {
		type namer interface{ Name() string }
		if n, ok := v.(namer); ok {
			return n.Name()
		}
		return "?"
	})()
}
