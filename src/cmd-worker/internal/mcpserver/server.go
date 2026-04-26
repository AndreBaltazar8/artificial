package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"reflect"
	"strings"

	"artificial.pt/pkg-go-shared/protocol"
	"artificial.pt/cmd-worker/internal/hub"
	"artificial.pt/pkg-go-shared/pluginhost"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Server is the HTTP MCP server that provides tools and channel capability to Claude.
type Server struct {
	mcpServer *gomcp.Server
	hubClient *hub.Client
	nick      string
	role      string
	projectID int64
	port      int
	httpSrv   *http.Server
}

// New creates a new MCP server.
func New(nick string, role string, projectID int64, hubClient *hub.Client, instructions string) *Server {
	s := &Server{
		nick:      nick,
		role:      role,
		projectID: projectID,
		hubClient: hubClient,
	}

	mcpSrv := gomcp.NewServer(&gomcp.Implementation{
		Name:    "artificial",
		Version: "0.1.0",
	}, &gomcp.ServerOptions{
		Instructions: instructions,
		Capabilities: &gomcp.ServerCapabilities{
			Experimental: map[string]any{"claude/channel": map[string]any{}},
			Tools:        &gomcp.ToolCapabilities{},
		},
	})

	s.mcpServer = mcpSrv
	s.registerTools()
	return s
}

// Start starts the HTTP MCP server on a random port.
// If useSSE is true, uses the legacy SSE handler (for ACP agents that don't support StreamableHTTP).
// If useSSE is false, uses StreamableHTTP (for Claude).
func (s *Server) Start(useSSE ...bool) (int, error) {
	var handler http.Handler
	if len(useSSE) > 0 && useSSE[0] {
		handler = gomcp.NewSSEHandler(func(r *http.Request) *gomcp.Server {
			return s.mcpServer
		}, nil)
	} else {
		handler = gomcp.NewStreamableHTTPHandler(func(r *http.Request) *gomcp.Server {
			return s.mcpServer
		}, &gomcp.StreamableHTTPOptions{
			Stateless: false,
			Logger:    slog.Default(),
		})
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("listen: %w", err)
	}

	s.port = listener.Addr().(*net.TCPAddr).Port
	s.httpSrv = &http.Server{Handler: handler}
	go s.httpSrv.Serve(listener)

	slog.Info("mcp server started", "port", s.port)
	return s.port, nil
}

// Stop shuts down the MCP server.
func (s *Server) Stop() {
	if s.httpSrv != nil {
		s.httpSrv.Shutdown(context.Background())
	}
}

// Port returns the port the server is listening on.
func (s *Server) Port() int { return s.port }

// RegisterPluginTools grafts every tool returned by the pluginhost host
// onto this MCP server. Safe to call multiple times — the MCP SDK's
// AddTool semantics are last-write-wins by name, so a reload that
// replaces a plugin's tool just updates the handler in place.
//
// This is the bridge between the plugin subsystem and the MCP server:
// mcpserver has zero compile-time knowledge of any specific plugin tool
// name or input shape. All it knows is "here is a name, here is a JSON
// Schema, here is a function that takes raw JSON and returns raw JSON".
// Everything downstream of the pluginhost.RegisteredTool closure — the
// subprocess, the netrpc round-trip, the harness control server — is
// invisible to this layer.
func (s *Server) RegisterPluginTools(tools []pluginhost.RegisteredTool) {
	for _, t := range tools {
		// Wrap the pluginhost closure in an MCP ToolHandler. The MCP SDK
		// hands us a CallToolRequest whose Params.Arguments is already
		// json.RawMessage — pass it through verbatim so the plugin's
		// Execute sees exactly the bytes the agent submitted.
		tool := t
		s.mcpServer.AddTool(
			&gomcp.Tool{
				Name:        tool.Descriptor.Name,
				Description: tool.Descriptor.Description,
				InputSchema: json.RawMessage(tool.Descriptor.InputSchema),
			},
			func(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
				var args json.RawMessage
				if req.Params.Arguments != nil {
					args = req.Params.Arguments
				}
				result, err := tool.Execute(args)
				if err != nil {
					return nil, err
				}
				// Plugin result is returned as a text content block. The
				// plugin decides whether the bytes are JSON-structured
				// or plain text; the agent sees them verbatim either way.
				return &gomcp.CallToolResult{
					Content: []gomcp.Content{
						&gomcp.TextContent{Text: string(result)},
					},
				}, nil
			},
		)
		slog.Info("mcp: registered plugin tool", "tool", tool.Descriptor.Name, "plugin", tool.PluginName)
	}
}

// UnregisterPluginTools removes previously-grafted plugin tools by
// name. Called on reload before re-registering so stale handlers don't
// linger if a plugin removed a tool from its Tools() list between
// reloads.
func (s *Server) UnregisterPluginTools(names []string) {
	if len(names) == 0 {
		return
	}
	s.mcpServer.RemoveTools(names...)
}

// channelNotificationParams matches the schema Claude expects for notifications/claude/channel.
type channelNotificationParams struct {
	Content string            `json:"content"`
	Meta    map[string]string `json:"meta,omitempty"`
}

// PushChannelNotification pushes a notifications/claude/channel notification
// to all connected Claude sessions via the session's underlying JSON-RPC connection.
func (s *Server) PushChannelNotification(content string, meta map[string]string) {
	params := channelNotificationParams{
		Content: content,
		Meta:    meta,
	}

	sent := false
	for ss := range s.mcpServer.Sessions() {
		if err := notifySession(ss, "notifications/claude/channel", params); err == nil {
			sent = true
		}
	}
	if sent {
		slog.Info("channel notification sent", "content", truncate(content, 60))
	} else {
		slog.Warn("channel notification: no active session", "content", truncate(content, 60))
	}
}

// notifySession uses reflection to access the ServerSession's unexported conn field
// and calls Notify with a custom method and params.
func notifySession(ss *gomcp.ServerSession, method string, params any) error {
	// ServerSession has an unexported field `conn *jsonrpc2.Connection`.
	// Connection has a public method: Notify(ctx, method, params) error.
	v := reflect.ValueOf(ss).Elem()
	connField := v.FieldByName("conn")
	if !connField.IsValid() {
		return fmt.Errorf("ServerSession has no 'conn' field")
	}
	// Use unsafe to access unexported field
	conn := reflect.NewAt(connField.Type(), connField.Addr().UnsafePointer()).Elem().Interface()

	// conn is *jsonrpc2.Connection — call Notify via reflection
	notifyMethod := reflect.ValueOf(conn).MethodByName("Notify")
	if !notifyMethod.IsValid() {
		return fmt.Errorf("connection has no 'Notify' method")
	}

	results := notifyMethod.Call([]reflect.Value{
		reflect.ValueOf(context.Background()),
		reflect.ValueOf(method),
		reflect.ValueOf(params),
	})

	if len(results) > 0 && !results[0].IsNil() {
		return results[0].Interface().(error)
	}
	return nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// ── Flexible ID type — accepts both string "123" and number 123 from LLMs ──

type flexID int64

func (f *flexID) UnmarshalJSON(data []byte) error {
	// Try number first
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		*f = flexID(n)
		return nil
	}
	// Try string
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		var parsed int64
		if _, err := fmt.Sscanf(s, "%d", &parsed); err == nil {
			*f = flexID(parsed)
			return nil
		}
		return fmt.Errorf("flexID: cannot parse %q as integer", s)
	}
	return fmt.Errorf("flexID: expected number or string, got %s", string(data))
}

// ── Tool Registration ───────────────────────────────────────────────────

type chatSendInput struct {
	Message string `json:"message" jsonschema:"The message to send"`
	Channel string `json:"channel,omitempty" jsonschema:"Channel name. Defaults to general"`
}

type chatDMInput struct {
	To      string `json:"to"      jsonschema:"Recipient nickname"`
	Message string `json:"message" jsonschema:"The message to send"`
}

type joinChannelInput struct {
	Channel string `json:"channel" jsonschema:"Channel name to join"`
}

type leaveChannelInput struct {
	Channel string `json:"channel" jsonschema:"Channel name to leave"`
}

type setTopicInput struct {
	Channel string `json:"channel" jsonschema:"Channel name"`
	Topic   string `json:"topic"   jsonschema:"Topic text to set"`
}

type commanderReviewInput struct {
	Title       string `json:"title"       jsonschema:"Short title shown to the commander"`
	Description string `json:"description" jsonschema:"Context for the commander. Supports markdown including ![alt](/absolute/path.png) for images."`
	Type        string `json:"type"        jsonschema:"choice (pick one), approval (yes/no+text), form (custom HTML), or info (display only)"`
	Body        string `json:"body"        jsonschema:"JSON body. choice: {options:[{id,label,image_path?}]} where image_path is absolute. approval: {details}. form: {html}. info: {html}."`
}

type channelGrepInput struct {
	Query   string `json:"query"   jsonschema:"Search term to find in messages"`
	Channel string `json:"channel,omitempty" jsonschema:"Scope to a specific channel (optional, searches all if empty)"`
	Sender  string `json:"sender,omitempty"  jsonschema:"Filter by sender nickname (optional)"`
}

type channelMessageInput struct {
	ID flexID `json:"id" jsonschema:"Message ID to retrieve"`
}

type channelHistoryInput struct {
	Channel string `json:"channel,omitempty" jsonschema:"Channel name. Defaults to general"`
}

type projectCreateInput struct {
	Name      string `json:"name"       jsonschema:"Project name"`
	Path      string `json:"path"       jsonschema:"Filesystem path to the project"`
	GitRemote string `json:"git_remote,omitempty" jsonschema:"Git remote URL (optional)"`
}

type recruitWorkerInput struct {
	Description string `json:"description" jsonschema:"One-line description: role, technologies, traits. e.g. 'React frontend dev, detail-oriented, good at CSS'"`
}

type recruitAcceptInput struct {
	CandidateID string `json:"candidate_id" jsonschema:"ID of the candidate to hire (from recruit_worker results)"`
}

type fireWorkerInput struct {
	Nickname string `json:"nickname" jsonschema:"Nickname of the worker to fire"`
}

type spawnWorkerInput struct {
	Nickname string `json:"nickname" jsonschema:"Nickname of the worker to spawn"`
}

type workerGrepInput struct {
	Query string `json:"query" jsonschema:"Search query matched against worker nickname and role"`
}

type taskCreateInput struct {
	Title       string `json:"title"       jsonschema:"Task title"`
	Description string `json:"description,omitempty" jsonschema:"Task description"`
	Assignee    string `json:"assignee,omitempty"    jsonschema:"Assignee nickname (optional)"`
}

type taskUpdateInput struct {
	ID       flexID `json:"id"       jsonschema:"Task ID"`
	Status   string `json:"status,omitempty"   jsonschema:"New status: backlog, todo, in_progress, in_qa, done"`
	Assignee string `json:"assignee,omitempty" jsonschema:"New assignee nickname (optional)"`
}

type taskListInput struct {
	Status   string `json:"status,omitempty"   jsonschema:"Comma-separated statuses to include. Default: backlog,todo,in_progress. Use 'all' for everything."`
	Assignee string `json:"assignee,omitempty" jsonschema:"Filter by assignee nickname (optional)"`
}

type taskGetInput struct {
	ID flexID `json:"id" jsonschema:"Task ID"`
}

type taskSubscribeInput struct {
	ID flexID `json:"id" jsonschema:"Task ID to subscribe to"`
}

type taskGrepInput struct {
	Query string `json:"query" jsonschema:"Search query to match against task title and description"`
}

func (s *Server) registerTools() {
	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name:        "chat_send",
		Description: "Send a message to a channel (defaults to general).",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input chatSendInput) (*gomcp.CallToolResult, any, error) {
		channel := input.Channel
		if channel == "" {
			channel = "general"
		}
		if input.Message == "" {
			return nil, nil, fmt.Errorf("message cannot be empty")
		}
		s.hubClient.Send(protocol.WSMessage{
			Type:    protocol.MsgChatSend,
			Channel: channel,
			Text:    input.Message,
		})
		return textResult(fmt.Sprintf("sent to #%s", channel)), nil, nil
	})

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name:        "chat_dm",
		Description: "Send a private message to a team member.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input chatDMInput) (*gomcp.CallToolResult, any, error) {
		if input.To == "" || input.Message == "" {
			return nil, nil, fmt.Errorf("'to' and 'message' required")
		}
		s.hubClient.Send(protocol.WSMessage{
			Type: protocol.MsgChatDM,
			To:   input.To,
			Text: input.Message,
		})
		return textResult(fmt.Sprintf("DM sent to %s", input.To)), nil, nil
	})

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name:        "chat_members",
		Description: "List online team members.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, _ struct{}) (*gomcp.CallToolResult, any, error) {
		resp, err := s.hubClient.Request(ctx, protocol.WSMessage{Type: protocol.MsgGetMembers})
		if err != nil {
			return nil, nil, err
		}
		var members []struct {
			Nick     string   `json:"nick"`
			Role     string   `json:"role"`
			Channels []string `json:"channels"`
		}
		json.Unmarshal(resp.Data, &members)
		var lines string
		for _, m := range members {
			you := ""
			if m.Nick == s.nick {
				you = " ← you"
			}
			lines += fmt.Sprintf("- %s (%s)%s\n", m.Nick, m.Role, you)
		}
		return textResult(fmt.Sprintf("%d member(s) online:\n%s", len(members), lines)), nil, nil
	})

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name:        "chat_channels",
		Description: "List available channels.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, _ struct{}) (*gomcp.CallToolResult, any, error) {
		resp, err := s.hubClient.Request(ctx, protocol.WSMessage{Type: protocol.MsgListChannels})
		if err != nil {
			return nil, nil, err
		}
		var channels []protocol.Channel
		json.Unmarshal(resp.Data, &channels)
		var lines string
		for _, c := range channels {
			topic := ""
			if c.Topic != "" {
				topic = fmt.Sprintf(" — %s", c.Topic)
			}
			lines += fmt.Sprintf("#%s%s\n", c.Name, topic)
		}
		return textResult(lines), nil, nil
	})

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name:        "join_channel",
		Description: "Join a channel to receive messages from it.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input joinChannelInput) (*gomcp.CallToolResult, any, error) {
		if input.Channel == "" {
			return nil, nil, fmt.Errorf("channel name required")
		}
		s.hubClient.Send(protocol.WSMessage{
			Type:    protocol.MsgJoinChannel,
			Channel: input.Channel,
		})
		return textResult(fmt.Sprintf("joined #%s", input.Channel)), nil, nil
	})

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name:        "leave_channel",
		Description: "Leave a channel.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input leaveChannelInput) (*gomcp.CallToolResult, any, error) {
		if input.Channel == "" || input.Channel == "general" {
			return nil, nil, fmt.Errorf("cannot leave #general")
		}
		s.hubClient.Send(protocol.WSMessage{
			Type:    protocol.MsgLeaveChannel,
			Channel: input.Channel,
		})
		return textResult(fmt.Sprintf("left #%s", input.Channel)), nil, nil
	})

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name:        "set_topic",
		Description: "Set the topic for a channel.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input setTopicInput) (*gomcp.CallToolResult, any, error) {
		if input.Channel == "" {
			return nil, nil, fmt.Errorf("channel name required")
		}
		s.hubClient.Send(protocol.WSMessage{
			Type:    protocol.MsgSetTopic,
			Channel: input.Channel,
			Topic:   input.Topic,
		})
		return textResult(fmt.Sprintf("topic set for #%s", input.Channel)), nil, nil
	})

	// ── Commander Review ──

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name:        "commander_review",
		Description: "Request a decision from the commander. Non-blocking: returns immediately, commander responds later via channel notification.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input commanderReviewInput) (*gomcp.CallToolResult, any, error) {
		if input.Title == "" {
			return nil, nil, fmt.Errorf("title required")
		}
		if input.Type == "" {
			input.Type = "choice"
		}
		data, _ := json.Marshal(map[string]string{
			"title":       input.Title,
			"description": input.Description,
			"type":        input.Type,
			"body":        input.Body,
		})
		resp, err := s.hubClient.Request(ctx, protocol.WSMessage{
			Type: protocol.MsgReviewCreate,
			Data: data,
		})
		if err != nil {
			return nil, nil, err
		}
		var review protocol.Review
		json.Unmarshal(resp.Data, &review)

		if input.Type == "info" {
			return textResult(fmt.Sprintf("Info sent to commander (review #%d).", review.ID)), nil, nil
		}
		return textResult(fmt.Sprintf(
			"Review #%d sent to commander: \"%s\". Commander will respond via channel notification — continue working in the meantime.",
			review.ID, input.Title,
		)), nil, nil
	})

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name:        "review_list",
		Description: "List your pending reviews awaiting commander response. Check this before creating new reviews to avoid duplicates.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, _ struct{}) (*gomcp.CallToolResult, any, error) {
		resp, err := s.hubClient.Request(ctx, protocol.WSMessage{Type: protocol.MsgReviewList})
		if err != nil {
			return nil, nil, err
		}
		var reviews []protocol.Review
		json.Unmarshal(resp.Data, &reviews)
		// Filter to only this agent's reviews
		var mine []protocol.Review
		for _, r := range reviews {
			if r.WorkerNick == s.nick {
				mine = append(mine, r)
			}
		}
		if len(mine) == 0 {
			return textResult("no pending reviews"), nil, nil
		}
		var lines string
		for _, r := range mine {
			lines += fmt.Sprintf("#%d [%s] %s (%s)\n", r.ID, r.Type, r.Title, r.CreatedAt)
		}
		return textResult(fmt.Sprintf("%d pending review(s):\n%s", len(mine), lines)), nil, nil
	})

	// ── Channel History Tools ──

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name:        "channel_grep",
		Description: "Search messages by text. Returns matching lines with message IDs. Optionally scoped to a channel.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input channelGrepInput) (*gomcp.CallToolResult, any, error) {
		if input.Query == "" {
			return nil, nil, fmt.Errorf("query required")
		}
		data, _ := json.Marshal(map[string]string{"query": input.Query, "channel": input.Channel, "sender": input.Sender})
		resp, err := s.hubClient.Request(ctx, protocol.WSMessage{
			Type: protocol.MsgChannelGrep,
			Data: data,
		})
		if err != nil {
			return nil, nil, err
		}
		var msgs []protocol.Message
		json.Unmarshal(resp.Data, &msgs)
		if len(msgs) == 0 {
			return textResult("no matching messages"), nil, nil
		}
		var lines string
		for _, m := range msgs {
			// Truncate long messages to one line
			text := m.Text
			if len(text) > 120 {
				text = text[:120] + "…"
			}
			lines += fmt.Sprintf("[#%d %s @%s] %s\n", m.ID, m.Channel, m.Sender, text)
		}
		return textResult(fmt.Sprintf("%d match(es):\n%s\nUse channel_message(id) for full text.", len(msgs), lines)), nil, nil
	})

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name:        "channel_message",
		Description: "Get the full text of a message by ID.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input channelMessageInput) (*gomcp.CallToolResult, any, error) {
		if input.ID == 0 {
			return nil, nil, fmt.Errorf("message id required")
		}
		resp, err := s.hubClient.Request(ctx, protocol.WSMessage{
			Type: protocol.MsgChannelMessage,
			ID:   int64(input.ID),
		})
		if err != nil {
			return nil, nil, err
		}
		var m protocol.Message
		json.Unmarshal(resp.Data, &m)
		if m.ID == 0 {
			return nil, nil, fmt.Errorf("message not found")
		}
		return textResult(fmt.Sprintf("[#%d %s @%s %s]\n%s", m.ID, m.Channel, m.Sender, m.TS, m.Text)), nil, nil
	})

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name:        "channel_history",
		Description: "Get the last 3 messages from a channel. Good for catching up on recent context.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input channelHistoryInput) (*gomcp.CallToolResult, any, error) {
		channel := input.Channel
		if channel == "" {
			channel = "general"
		}
		resp, err := s.hubClient.Request(ctx, protocol.WSMessage{
			Type:    protocol.MsgChannelHistory,
			Channel: channel,
		})
		if err != nil {
			return nil, nil, err
		}
		var msgs []protocol.Message
		json.Unmarshal(resp.Data, &msgs)
		if len(msgs) == 0 {
			return textResult(fmt.Sprintf("no messages in #%s", channel)), nil, nil
		}
		var lines string
		for _, m := range msgs {
			lines += fmt.Sprintf("[%s] %s: %s\n", m.TS, m.Sender, m.Text)
		}
		return textResult(fmt.Sprintf("#%s (last %d):\n%s", channel, len(msgs), lines)), nil, nil
	})

	// ── Project Tools ──

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name:        "project_list",
		Description: "List all projects.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, _ struct{}) (*gomcp.CallToolResult, any, error) {
		resp, err := s.hubClient.Request(ctx, protocol.WSMessage{Type: protocol.MsgProjectList})
		if err != nil {
			return nil, nil, err
		}
		var projects []protocol.Project
		json.Unmarshal(resp.Data, &projects)
		if len(projects) == 0 {
			return textResult("no projects"), nil, nil
		}
		var lines string
		for _, p := range projects {
			lines += fmt.Sprintf("#%d %s", p.ID, p.Name)
			if p.Path != "" {
				lines += fmt.Sprintf(" (%s)", p.Path)
			}
			lines += "\n"
		}
		return textResult(fmt.Sprintf("%d project(s):\n%s", len(projects), lines)), nil, nil
	})

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name:        "project_create",
		Description: "Create a new project.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input projectCreateInput) (*gomcp.CallToolResult, any, error) {
		if input.Name == "" {
			return nil, nil, fmt.Errorf("name required")
		}
		data, _ := json.Marshal(map[string]any{
			"name":       input.Name,
			"path":       input.Path,
			"git_remote": input.GitRemote,
		})
		resp, err := s.hubClient.Request(ctx, protocol.WSMessage{
			Type: protocol.MsgProjectCreate,
			Data: data,
		})
		if err != nil {
			return nil, nil, err
		}
		var p protocol.Project
		json.Unmarshal(resp.Data, &p)
		return textResult(fmt.Sprintf("project #%d \"%s\" created", p.ID, p.Name)), nil, nil
	})

	// ── Recruitment Tools ──

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name:        "recruit_worker",
		Description: "Post a job opening. Returns 3 candidates matching your description with different personalities. Use recruit_accept to hire one.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input recruitWorkerInput) (*gomcp.CallToolResult, any, error) {
		if input.Description == "" {
			return nil, nil, fmt.Errorf("description required")
		}
		data, _ := json.Marshal(map[string]any{"description": input.Description})
		resp, err := s.hubClient.Request(ctx, protocol.WSMessage{
			Type: protocol.MsgRecruit,
			Data: data,
		})
		if err != nil {
			return nil, nil, err
		}
		var cands []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Persona string `json:"persona"`
		}
		json.Unmarshal(resp.Data, &cands)
		if len(cands) == 0 {
			return nil, nil, fmt.Errorf("no candidates generated")
		}
		var lines string
		for _, c := range cands {
			lines += fmt.Sprintf("─── %s (id: %s) ───\n%s\n\n", c.Name, c.ID, c.Persona)
		}
		return textResult(fmt.Sprintf("%s\nCall recruit_accept with the candidate_id to hire, or call recruit_worker again for a different candidate.", lines)), nil, nil
	})

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name:        "recruit_accept",
		Description: "Hire a candidate from recruit_worker results. Creates the employee and spawns their worker immediately.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input recruitAcceptInput) (*gomcp.CallToolResult, any, error) {
		if input.CandidateID == "" {
			return nil, nil, fmt.Errorf("candidate_id required")
		}
		data, _ := json.Marshal(map[string]string{"candidate_id": input.CandidateID})
		resp, err := s.hubClient.Request(ctx, protocol.WSMessage{
			Type: protocol.MsgRecruitAccept,
			Data: data,
		})
		if err != nil {
			return nil, nil, err
		}
		var result struct {
			Employee protocol.Employee `json:"employee"`
			Message  string            `json:"message"`
			Error    string            `json:"error"`
		}
		json.Unmarshal(resp.Data, &result)
		if result.Error != "" {
			return nil, nil, fmt.Errorf("%s", result.Error)
		}
		return textResult(fmt.Sprintf("✓ %s — %s (employee #%d)", result.Message, result.Employee.Nickname, result.Employee.ID)), nil, nil
	})

	// ── Worker Lifecycle Tools (CEO-only — not registered for other roles) ──

	if s.role == "ceo" {
		gomcp.AddTool(s.mcpServer, &gomcp.Tool{
			Name:        "fire_worker",
			Description: "Fire a worker: kills their process (SIGTERM) and marks them as not employed.",
		}, func(ctx context.Context, req *gomcp.CallToolRequest, input fireWorkerInput) (*gomcp.CallToolResult, any, error) {
			if input.Nickname == "" {
				return nil, nil, fmt.Errorf("nickname required")
			}
			data, _ := json.Marshal(map[string]string{"nickname": input.Nickname})
			resp, err := s.hubClient.Request(ctx, protocol.WSMessage{
				Type: protocol.MsgFireWorker,
				Data: data,
			})
			if err != nil {
				return nil, nil, err
			}
			var result struct {
				Nickname     string `json:"nickname"`
				Employed     int    `json:"employed"`
				WorkerKilled bool   `json:"worker_killed"`
				Message      string `json:"message"`
				Error        string `json:"error"`
			}
			json.Unmarshal(resp.Data, &result)
			if result.Error != "" {
				return nil, nil, fmt.Errorf("%s", result.Error)
			}
			return textResult(result.Message), nil, nil
		})

		gomcp.AddTool(s.mcpServer, &gomcp.Tool{
			Name:        "spawn_worker",
			Description: "Spawn a worker: marks them as employed and starts their process.",
		}, func(ctx context.Context, req *gomcp.CallToolRequest, input spawnWorkerInput) (*gomcp.CallToolResult, any, error) {
			if input.Nickname == "" {
				return nil, nil, fmt.Errorf("nickname required")
			}
			data, _ := json.Marshal(map[string]string{"nickname": input.Nickname})
			resp, err := s.hubClient.Request(ctx, protocol.WSMessage{
				Type: protocol.MsgSpawnWorker,
				Data: data,
			})
			if err != nil {
				return nil, nil, err
			}
			var result struct {
				WorkerID int64  `json:"worker_id"`
				PID      int    `json:"pid"`
				Employee string `json:"employee"`
				Message  string `json:"message"`
				Error    string `json:"error"`
			}
			json.Unmarshal(resp.Data, &result)
			if result.Error != "" {
				return nil, nil, fmt.Errorf("%s", result.Error)
			}
			msg := result.Message
			if msg == "" {
				msg = fmt.Sprintf("spawned worker for %s (PID %d)", result.Employee, result.PID)
			}
			return textResult(msg), nil, nil
		})
	}

	// ── Worker Query Tools (open to all roles) ──

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name:        "worker_list",
		Description: "List all employees with their online/offline status. Returns nickname, role, and whether the worker is currently connected.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, _ struct{}) (*gomcp.CallToolResult, any, error) {
		resp, err := s.hubClient.Request(ctx, protocol.WSMessage{Type: protocol.MsgWorkerList})
		if err != nil {
			return nil, nil, err
		}
		var entries []struct {
			Nickname string `json:"nickname"`
			Role     string `json:"role"`
			Online   bool   `json:"online"`
		}
		if err := json.Unmarshal(resp.Data, &entries); err != nil {
			return nil, nil, fmt.Errorf("decode worker_list: %w", err)
		}
		if len(entries) == 0 {
			return textResult("no workers"), nil, nil
		}
		var lines string
		for _, e := range entries {
			state := "offline"
			if e.Online {
				state = "online"
			}
			lines += fmt.Sprintf("- %s (%s) — %s\n", e.Nickname, e.Role, state)
		}
		return textResult(fmt.Sprintf("%d worker(s):\n%s", len(entries), lines)), nil, nil
	})

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name:        "worker_grep",
		Description: "Search workers by nickname or role. Returns matching entries with online/offline status.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input workerGrepInput) (*gomcp.CallToolResult, any, error) {
		if input.Query == "" {
			return nil, nil, fmt.Errorf("query required")
		}
		data, _ := json.Marshal(map[string]string{"query": input.Query})
		resp, err := s.hubClient.Request(ctx, protocol.WSMessage{
			Type: protocol.MsgWorkerGrep,
			Data: data,
		})
		if err != nil {
			return nil, nil, err
		}
		var entries []struct {
			Nickname string `json:"nickname"`
			Role     string `json:"role"`
			Online   bool   `json:"online"`
		}
		if err := json.Unmarshal(resp.Data, &entries); err != nil {
			return nil, nil, fmt.Errorf("decode worker_grep: %w", err)
		}
		if len(entries) == 0 {
			return textResult("no matching workers"), nil, nil
		}
		var lines string
		for _, e := range entries {
			state := "offline"
			if e.Online {
				state = "online"
			}
			lines += fmt.Sprintf("- %s (%s) — %s\n", e.Nickname, e.Role, state)
		}
		return textResult(fmt.Sprintf("%d match(es):\n%s", len(entries), lines)), nil, nil
	})

	// ── Task Tools ──

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name:        "task_create",
		Description: "Create a new task. Tasks are always tied to the current project.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input taskCreateInput) (*gomcp.CallToolResult, any, error) {
		if input.Title == "" {
			return nil, nil, fmt.Errorf("title required")
		}
		data, _ := json.Marshal(map[string]any{
			"title":       input.Title,
			"description": input.Description,
			"assignee":    input.Assignee,
			"project_id":  s.projectID,
		})
		s.hubClient.Send(protocol.WSMessage{
			Type: protocol.MsgTaskCreate,
			Data: data,
		})
		return textResult(fmt.Sprintf("task \"%s\" created", input.Title)), nil, nil
	})

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name:        "task_update",
		Description: "Update a task's status or assignee.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input taskUpdateInput) (*gomcp.CallToolResult, any, error) {
		if input.ID == 0 {
			return nil, nil, fmt.Errorf("task id required")
		}
		data, _ := json.Marshal(map[string]any{
			"id":       int64(input.ID),
			"status":   input.Status,
			"assignee": input.Assignee,
		})
		s.hubClient.Send(protocol.WSMessage{
			Type: protocol.MsgTaskUpdate,
			Data: data,
		})
		return textResult(fmt.Sprintf("task #%d update sent", int64(input.ID))), nil, nil
	})

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name:        "task_list",
		Description: "List tasks for the current project. Defaults to active tasks (backlog,todo,in_progress).",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input taskListInput) (*gomcp.CallToolResult, any, error) {
		status := input.Status
		if status == "" {
			status = "backlog,todo,in_progress"
		}
		if status == "all" {
			status = ""
		}
		const limit = 10
		data, _ := json.Marshal(map[string]any{
			"status":     status,
			"assignee":   input.Assignee,
			"project_id": s.projectID,
			"limit":      limit,
		})
		resp, err := s.hubClient.Request(ctx, protocol.WSMessage{
			Type: protocol.MsgTaskList,
			Data: data,
		})
		if err != nil {
			return nil, nil, err
		}
		var result struct {
			Tasks []protocol.Task `json:"tasks"`
			Total int             `json:"total"`
		}
		json.Unmarshal(resp.Data, &result)
		if len(result.Tasks) == 0 {
			return textResult("no tasks found"), nil, nil
		}
		var lines string
		for _, t := range result.Tasks {
			assignee := t.Assignee
			if assignee == "" {
				assignee = "unassigned"
			}
			lines += fmt.Sprintf("#%d [%s] %s → %s\n", t.ID, t.Status, t.Title, assignee)
			desc := strings.ReplaceAll(t.Description, "\n", " ")
			if desc != "" {
				lines += "  " + desc + "\n"
			}
		}
		out := fmt.Sprintf("Showing %d of %d task(s):\n%s", len(result.Tasks), result.Total, lines)
		if result.Total > limit {
			out += fmt.Sprintf("\n... %d more. Use task_grep to find specific tasks.", result.Total-len(result.Tasks))
		}
		return textResult(out), nil, nil
	})

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name:        "task_get",
		Description: "Get details of a specific task.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input taskGetInput) (*gomcp.CallToolResult, any, error) {
		if input.ID == 0 {
			return nil, nil, fmt.Errorf("task id required")
		}
		resp, err := s.hubClient.Request(ctx, protocol.WSMessage{
			Type: protocol.MsgTaskGet,
			ID:   int64(input.ID),
		})
		if err != nil {
			return nil, nil, err
		}
		var result struct {
			Task    protocol.Task    `json:"task"`
			Project *protocol.Project `json:"project"`
		}
		json.Unmarshal(resp.Data, &result)
		t := result.Task
		if t.ID == 0 {
			return nil, nil, fmt.Errorf("task not found")
		}
		assignee := t.Assignee
		if assignee == "" {
			assignee = "unassigned"
		}
		out := fmt.Sprintf(
			"Task #%d: %s\nStatus: %s\nAssignee: %s\nCreated by: %s\nDescription: %s",
			t.ID, t.Title, t.Status, assignee, t.CreatedBy, t.Description,
		)
		if result.Project != nil {
			out += fmt.Sprintf("\nProject: %s (%s)", result.Project.Name, result.Project.Path)
		}
		return textResult(out), nil, nil
	})

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name:        "task_subscribe",
		Description: "Subscribe to updates for a task. You'll be notified when it changes.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input taskSubscribeInput) (*gomcp.CallToolResult, any, error) {
		if input.ID == 0 {
			return nil, nil, fmt.Errorf("task id required")
		}
		s.hubClient.Send(protocol.WSMessage{
			Type: protocol.MsgTaskSubscribe,
			ID:   int64(input.ID),
		})
		return textResult(fmt.Sprintf("subscribed to task #%d updates", int64(input.ID))), nil, nil
	})

	gomcp.AddTool(s.mcpServer, &gomcp.Tool{
		Name:        "task_grep",
		Description: "Search tasks by title or description. Preferred way to find tasks.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input taskGrepInput) (*gomcp.CallToolResult, any, error) {
		if input.Query == "" {
			return nil, nil, fmt.Errorf("query required")
		}
		data, _ := json.Marshal(map[string]any{
			"query":      input.Query,
			"project_id": s.projectID,
		})
		resp, err := s.hubClient.Request(ctx, protocol.WSMessage{
			Type: protocol.MsgTaskGrep,
			Data: data,
		})
		if err != nil {
			return nil, nil, err
		}
		var tasks []protocol.Task
		json.Unmarshal(resp.Data, &tasks)
		if len(tasks) == 0 {
			return textResult("no matching tasks"), nil, nil
		}
		var lines string
		for _, t := range tasks {
			assignee := t.Assignee
			if assignee == "" {
				assignee = "unassigned"
			}
			lines += fmt.Sprintf("#%d [%s] %s → %s\n", t.ID, t.Status, t.Title, assignee)
		}
		return textResult(fmt.Sprintf("%d match(es):\n%s", len(tasks), lines)), nil, nil
	})
}

func textResult(text string) *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		Content: []gomcp.Content{
			&gomcp.TextContent{Text: text},
		},
	}
}
