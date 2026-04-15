package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"

	"artificial.pt/pkg-go-shared/protocol"
	"artificial.pt/svc-artificial/internal/db"

	"nhooyr.io/websocket"
)

// client represents a connected WebSocket client.
type client struct {
	nick string
	conn *websocket.Conn
}

// Hub manages WebSocket connections and message routing.
type Hub struct {
	db      *db.DB
	port    int
	mu      sync.RWMutex
	clients map[string]*client // nick → client
}

// NewHub creates a new WebSocket hub.
func NewHub(database *db.DB, port int) *Hub {
	return &Hub{
		db:      database,
		port:    port,
		clients: make(map[string]*client),
	}
}

// HandleWebSocket upgrades an HTTP request to a WebSocket connection.
func (h *Hub) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	nick := r.URL.Query().Get("nick")
	if nick == "" {
		http.Error(w, "nick query param required", http.StatusBadRequest)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // Allow any origin for dashboard
	})
	if err != nil {
		slog.Error("ws accept", "err", err)
		return
	}
	conn.SetReadLimit(-1) // default 32 KiB truncates large task lists / histories
	defer conn.Close(websocket.StatusNormalClosure, "bye")

	c := &client{nick: nick, conn: conn}

	h.mu.Lock()
	h.clients[nick] = c
	h.mu.Unlock()

	// Update employee last_connected
	isReconnect := false
	if emp, err := h.db.GetEmployeeByNick(nick); err == nil {
		h.db.UpdateEmployeeLastConnected(emp.ID)
		// Check if already a channel member — if so, this is a reconnect, not a fresh join
		if channels, err := h.db.GetEmployeeChannels(emp.ID); err == nil && len(channels) > 0 {
			isReconnect = true
		}
	}

	// Only broadcast join for genuinely new connections, not reconnects after hub restart
	if nick != "commander" && !isReconnect {
		h.broadcast(protocol.WSMessage{
			Type: protocol.MsgMemberJoined,
			Nick: nick,
		}, nick)
	}

	slog.Info("ws connected", "nick", nick)

	ctx := r.Context()
	h.readLoop(ctx, c)

	// Cleanup on disconnect
	h.mu.Lock()
	delete(h.clients, nick)
	h.mu.Unlock()

	// Don't broadcast leave messages — workers disconnect/reconnect on hub
	// restarts and this just creates noise. The dashboard tracks online status
	// via the clients map directly.

	slog.Info("ws disconnected", "nick", nick)
}

func (h *Hub) readLoop(ctx context.Context, c *client) {
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		var msg protocol.WSMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		h.handleMessage(ctx, c, msg)
	}
}

func (h *Hub) handleMessage(ctx context.Context, c *client, msg protocol.WSMessage) {
	switch msg.Type {
	case protocol.MsgChatSend:
		h.handleChatSend(c, msg)
	case protocol.MsgChatDM:
		h.handleChatDM(c, msg)
	case protocol.MsgJoinChannel:
		h.handleJoinChannel(c, msg)
	case protocol.MsgLeaveChannel:
		h.handleLeaveChannel(c, msg)
	case protocol.MsgSetTopic:
		h.handleSetTopic(c, msg)
	case protocol.MsgGetMembers:
		h.handleGetMembers(c, msg)
	case protocol.MsgListChannels:
		h.handleListChannels(c, msg)
	case protocol.MsgMarkRead:
		h.handleMarkRead(c, msg)
	case protocol.MsgTaskCreate:
		h.handleTaskCreate(c, msg)
	case protocol.MsgTaskUpdate:
		h.handleTaskUpdate(c, msg)
	case protocol.MsgTaskList:
		h.handleTaskList(c, msg)
	case protocol.MsgTaskGet:
		h.handleTaskGet(c, msg)
	case protocol.MsgTaskSubscribe:
		h.handleTaskSubscribe(c, msg)
	case protocol.MsgTaskGrep:
		h.handleTaskGrep(c, msg)
	case protocol.MsgReviewCreate:
		h.handleReviewCreate(c, msg)
	case protocol.MsgReviewRespond:
		h.handleReviewRespond(c, msg)
	case protocol.MsgReviewList:
		h.handleReviewList(c, msg)
	case protocol.MsgChannelGrep:
		h.handleChannelGrep(c, msg)
	case protocol.MsgChannelMessage:
		h.handleChannelMessage(c, msg)
	case protocol.MsgChannelHistory:
		h.handleChannelHistory(c, msg)
	case protocol.MsgWorkerNotify:
		h.handleWorkerNotify(c, msg)
	case protocol.MsgWorkerCommand:
		h.handleWorkerNotify(c, msg) // same routing — the worker handles the distinction
	case protocol.MsgProjectList:
		h.handleProjectList(c, msg)
	case protocol.MsgProjectCreate:
		h.handleProjectCreate(c, msg)
	case protocol.MsgRecruit:
		h.handleRecruit(c, msg)
	case protocol.MsgRecruitAccept:
		h.handleRecruitAccept(c, msg)
	case protocol.MsgFireWorker:
		h.handleFireWorker(c, msg)
	case protocol.MsgSpawnWorker:
		h.handleSpawnWorker(c, msg)
	case protocol.MsgWorkerTTYInput, protocol.MsgWorkerTTYResize:
		// Forward directly to target worker — no broadcast
		if msg.To != "" {
			h.sendTo(msg.To, msg)
		}
	}
}

// ── Chat ────────────────────────────────────────────────────────────────

func (h *Hub) handleChatSend(c *client, msg protocol.WSMessage) {
	channel := msg.Channel
	if channel == "" {
		channel = "general"
	}
	text := msg.Text
	if text == "" {
		return
	}

	saved, err := h.db.SaveMessage(channel, c.nick, text)
	if err != nil {
		slog.Error("save message", "err", err)
		return
	}

	data, _ := json.Marshal(saved)
	h.broadcastToChannel(channel, protocol.WSMessage{
		Type: protocol.MsgMessage,
		Data: data,
	}, "")
}

func (h *Hub) handleChatDM(c *client, msg protocol.WSMessage) {
	to := msg.To
	text := msg.Text
	if to == "" || text == "" {
		return
	}

	dmChannel := protocol.DMChannelName(c.nick, to)
	saved, err := h.db.SaveMessage(dmChannel, c.nick, text)
	if err != nil {
		slog.Error("save dm", "err", err)
		return
	}

	data, _ := json.Marshal(saved)

	// Send to recipient
	h.sendTo(to, protocol.WSMessage{
		Type: protocol.MsgDM,
		From: c.nick,
		Data: data,
	})

	// Echo back to sender
	h.sendTo(c.nick, protocol.WSMessage{
		Type: protocol.MsgDM,
		From: c.nick,
		Data: data,
	})
}

// ── Channels ────────────────────────────────────────────────────────────

func (h *Hub) handleJoinChannel(c *client, msg protocol.WSMessage) {
	channel := msg.Channel
	if channel == "" {
		return
	}
	emp, err := h.db.GetEmployeeByNick(c.nick)
	if err != nil {
		return
	}
	if err := h.db.JoinChannel(channel, emp.ID); err != nil {
		slog.Error("join channel", "err", err)
		return
	}
	// Broadcast that a new channel may have been created
	h.broadcast(protocol.WSMessage{
		Type:    protocol.MsgChannelCreated,
		Channel: channel,
	}, "")
	h.broadcastToChannel(channel, protocol.WSMessage{
		Type:    protocol.MsgMemberJoined,
		Nick:    c.nick,
		Channel: channel,
	}, "")
}

func (h *Hub) handleLeaveChannel(c *client, msg protocol.WSMessage) {
	channel := msg.Channel
	if channel == "" || channel == "general" {
		return
	}
	emp, err := h.db.GetEmployeeByNick(c.nick)
	if err != nil {
		return
	}
	// Broadcast leave to members before removing membership
	h.broadcastToChannel(channel, protocol.WSMessage{
		Type:    protocol.MsgMemberLeft,
		Nick:    c.nick,
		Channel: channel,
	}, "")
	if err := h.db.LeaveChannel(channel, emp.ID); err != nil {
		slog.Error("leave channel", "err", err)
		return
	}
}

func (h *Hub) handleSetTopic(c *client, msg protocol.WSMessage) {
	channel := msg.Channel
	topic := msg.Topic
	if channel == "" {
		return
	}
	if err := h.db.SetTopic(channel, topic, c.nick); err != nil {
		slog.Error("set topic", "err", err)
		return
	}
	h.broadcastToChannel(channel, protocol.WSMessage{
		Type:    protocol.MsgTopicChanged,
		Channel: channel,
		Topic:   topic,
		Nick:    c.nick,
	}, "")
}

// ── Queries (request/response) ──────────────────────────────────────────

func (h *Hub) handleGetMembers(c *client, msg protocol.WSMessage) {
	h.mu.RLock()
	var members []map[string]any
	for nick := range h.clients {
		emp, err := h.db.GetEmployeeByNick(nick)
		if err != nil {
			continue
		}
		channels, _ := h.db.GetEmployeeChannels(emp.ID)
		members = append(members, map[string]any{
			"nick":     nick,
			"role":     emp.Role,
			"channels": channels,
		})
	}
	h.mu.RUnlock()

	data, _ := json.Marshal(members)
	h.sendTo(c.nick, protocol.WSMessage{
		Type:      msg.Type,
		RequestID: msg.RequestID,
		Data:      data,
	})
}

func (h *Hub) handleListChannels(c *client, msg protocol.WSMessage) {
	channels, err := h.db.ListChannels()
	if err != nil {
		return
	}
	data, _ := json.Marshal(channels)
	h.sendTo(c.nick, protocol.WSMessage{
		Type:      msg.Type,
		RequestID: msg.RequestID,
		Data:      data,
	})
}

func (h *Hub) handleMarkRead(c *client, msg protocol.WSMessage) {
	channel := msg.Channel
	msgID := msg.ID
	if channel == "" || msgID == 0 {
		return
	}
	emp, err := h.db.GetEmployeeByNick(c.nick)
	if err != nil {
		return
	}
	h.db.SetReadCursor(emp.ID, channel, msgID)
}

// ── Tasks ───────────────────────────────────────────────────────────────

func (h *Hub) handleTaskCreate(c *client, msg protocol.WSMessage) {
	var input struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Assignee    string `json:"assignee"`
		ProjectID   int64  `json:"project_id"`
	}
	if err := json.Unmarshal(msg.Data, &input); err != nil {
		// Try flattened fields
		input.Title = msg.Text
	}
	if input.Title == "" {
		return
	}

	task, err := h.db.CreateTask(input.Title, input.Description, input.Assignee, input.ProjectID, c.nick)
	if err != nil {
		slog.Error("create task", "err", err)
		return
	}

	// Auto-subscribe creator and assignee
	if emp, err := h.db.GetEmployeeByNick(c.nick); err == nil {
		h.db.SubscribeToTask(task.ID, emp.ID)
	}
	if input.Assignee != "" {
		if emp, err := h.db.GetEmployeeByNick(input.Assignee); err == nil {
			h.db.SubscribeToTask(task.ID, emp.ID)
		}
	}

	// Strip description from the broadcast payload — subscribers only need
	// id/title/status/assignee to render the notification; full body lives
	// behind task_get. Keeps the wire small and the notification focused.
	broadcast := task
	broadcast.Description = ""
	data, _ := json.Marshal(broadcast)
	h.broadcastToTaskSubscribers(task.ID, protocol.WSMessage{
		Type: protocol.MsgTaskCreated,
		Data: data,
		Text: "created",
	}, "")
}

func (h *Hub) handleTaskUpdate(c *client, msg protocol.WSMessage) {
	var input struct {
		ID       int64   `json:"id"`
		Status   *string `json:"status"`
		Assignee *string `json:"assignee"`
	}
	if msg.Data != nil {
		json.Unmarshal(msg.Data, &input)
	}
	if input.ID == 0 {
		input.ID = msg.ID
	}
	if input.ID == 0 {
		return
	}

	// If assignee is changing, auto-subscribe the new assignee
	if input.Assignee != nil && *input.Assignee != "" {
		if emp, err := h.db.GetEmployeeByNick(*input.Assignee); err == nil {
			h.db.SubscribeToTask(input.ID, emp.ID)
		}
	}

	task, err := h.db.UpdateTask(input.ID, input.Status, input.Assignee)
	if err != nil {
		slog.Error("update task", "err", err)
		return
	}

	// Describe what actually changed, so subscribers don't need the full task.
	var actions []string
	if input.Status != nil {
		actions = append(actions, "status="+*input.Status)
	}
	if input.Assignee != nil {
		a := *input.Assignee
		if a == "" {
			a = "unassigned"
		}
		actions = append(actions, "assignee="+a)
	}
	action := "updated"
	if len(actions) > 0 {
		action = strings.Join(actions, ", ")
	}

	// Strip description from the broadcast payload — see handleTaskCreate.
	broadcast := task
	broadcast.Description = ""
	data, _ := json.Marshal(broadcast)
	h.broadcastToTaskSubscribers(input.ID, protocol.WSMessage{
		Type: protocol.MsgTaskUpdated,
		Data: data,
		Text: action,
	}, "")
}

func (h *Hub) handleTaskList(c *client, msg protocol.WSMessage) {
	var input struct {
		Status    string `json:"status"`
		Assignee  string `json:"assignee"`
		ProjectID int64  `json:"project_id"`
		Limit     int    `json:"limit"`
	}
	if msg.Data != nil {
		json.Unmarshal(msg.Data, &input)
	}

	result, err := h.db.ListTasks(input.Status, input.Assignee, input.ProjectID, input.Limit)
	if err != nil {
		return
	}
	// Truncate descriptions on the wire — list view only needs a preview, and
	// full descriptions can push the response past the websocket read limit.
	for i := range result.Tasks {
		d := result.Tasks[i].Description
		if len(d) > 200 {
			result.Tasks[i].Description = d[:200] + fmt.Sprintf("... (truncated, use task_get(%d) for full task)", result.Tasks[i].ID)
		}
	}
	data, _ := json.Marshal(result)
	h.sendTo(c.nick, protocol.WSMessage{
		Type:      msg.Type,
		RequestID: msg.RequestID,
		Data:      data,
	})
}

func (h *Hub) handleTaskGet(c *client, msg protocol.WSMessage) {
	id := msg.ID
	if id == 0 {
		return
	}
	task, err := h.db.GetTask(id)
	if err != nil {
		return
	}
	// Include project info if available
	result := map[string]any{"task": task}
	if task.ProjectID > 0 {
		if proj, err := h.db.GetProject(task.ProjectID); err == nil {
			result["project"] = proj
		}
	}
	data, _ := json.Marshal(result)
	h.sendTo(c.nick, protocol.WSMessage{
		Type:      msg.Type,
		RequestID: msg.RequestID,
		Data:      data,
	})
}

func (h *Hub) handleTaskSubscribe(c *client, msg protocol.WSMessage) {
	id := msg.ID
	if id == 0 {
		return
	}
	emp, err := h.db.GetEmployeeByNick(c.nick)
	if err != nil {
		return
	}
	h.db.SubscribeToTask(id, emp.ID)
}

func (h *Hub) handleTaskGrep(c *client, msg protocol.WSMessage) {
	var input struct {
		Query     string `json:"query"`
		ProjectID int64  `json:"project_id"`
	}
	if msg.Data != nil {
		json.Unmarshal(msg.Data, &input)
	}
	if input.Query == "" {
		return
	}
	tasks, err := h.db.GrepTasks(input.Query, input.ProjectID)
	if err != nil {
		return
	}
	data, _ := json.Marshal(tasks)
	h.sendTo(c.nick, protocol.WSMessage{
		Type:      msg.Type,
		RequestID: msg.RequestID,
		Data:      data,
	})
}

// ── Projects ────────────────────────────────────────────────────────────

func (h *Hub) handleProjectList(c *client, msg protocol.WSMessage) {
	projects, err := h.db.ListProjects()
	if err != nil {
		return
	}
	data, _ := json.Marshal(projects)
	h.sendTo(c.nick, protocol.WSMessage{
		Type:      msg.Type,
		RequestID: msg.RequestID,
		Data:      data,
	})
}

func (h *Hub) handleProjectCreate(c *client, msg protocol.WSMessage) {
	var input struct {
		Name      string `json:"name"`
		Path      string `json:"path"`
		GitRemote string `json:"git_remote"`
	}
	if msg.Data != nil {
		json.Unmarshal(msg.Data, &input)
	}
	if input.Name == "" {
		return
	}
	proj, err := h.db.CreateProject(input.Name, input.Path, input.GitRemote)
	if err != nil {
		return
	}
	data, _ := json.Marshal(proj)
	h.sendTo(c.nick, protocol.WSMessage{
		Type:      msg.Type,
		RequestID: msg.RequestID,
		Data:      data,
	})
	// Broadcast to all so dashboards update
	h.broadcast(protocol.WSMessage{
		Type: protocol.MsgProjectCreated,
		Data: data,
	}, "")
}

// ── Recruitment ─────────────────────────────────────────────────────────

func (h *Hub) handleRecruit(c *client, msg protocol.WSMessage) {
	var input struct {
		Description string `json:"description"`
	}
	if msg.Data != nil {
		json.Unmarshal(msg.Data, &input)
	}
	if input.Description == "" && msg.Text != "" {
		input.Description = msg.Text
	}
	if input.Description == "" {
		return
	}

	// Call the REST API on localhost (it handles persona generation + candidate storage)
	body, _ := json.Marshal(map[string]any{"description": input.Description, "count": 3})
	resp, err := http.Post(
		fmt.Sprintf("http://localhost:%d/api/recruit", h.port),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	h.sendTo(c.nick, protocol.WSMessage{
		Type:      msg.Type,
		RequestID: msg.RequestID,
		Data:      json.RawMessage(data),
	})
}

func (h *Hub) handleRecruitAccept(c *client, msg protocol.WSMessage) {
	var input struct {
		CandidateID string `json:"candidate_id"`
	}
	if msg.Data != nil {
		json.Unmarshal(msg.Data, &input)
	}
	if input.CandidateID == "" {
		return
	}

	body, _ := json.Marshal(map[string]string{"candidate_id": input.CandidateID})
	resp, err := http.Post(
		fmt.Sprintf("http://localhost:%d/api/recruit/accept", h.port),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	h.sendTo(c.nick, protocol.WSMessage{
		Type:      msg.Type,
		RequestID: msg.RequestID,
		Data:      json.RawMessage(data),
	})
}

// ── Worker Lifecycle (fire/spawn) ────────────────────────────────────────

func (h *Hub) handleFireWorker(c *client, msg protocol.WSMessage) {
	// Authorization: only CEO can fire workers
	if caller, err := h.db.GetEmployeeByNick(c.nick); err != nil || caller.Role != "ceo" {
		h.sendTo(c.nick, protocol.WSMessage{
			Type:      msg.Type,
			RequestID: msg.RequestID,
			Data:      json.RawMessage(`{"error":"fire_worker is restricted to CEO role"}`),
		})
		return
	}

	var input struct {
		Nickname string `json:"nickname"`
	}
	if msg.Data != nil {
		json.Unmarshal(msg.Data, &input)
	}
	if input.Nickname == "" {
		h.sendTo(c.nick, protocol.WSMessage{
			Type:      msg.Type,
			RequestID: msg.RequestID,
			Data:      json.RawMessage(`{"error":"nickname required"}`),
		})
		return
	}

	emp, err := h.db.GetEmployeeByNick(input.Nickname)
	if err != nil {
		h.sendTo(c.nick, protocol.WSMessage{
			Type:      msg.Type,
			RequestID: msg.RequestID,
			Data:      json.RawMessage(fmt.Sprintf(`{"error":"employee %q not found"}`, input.Nickname)),
		})
		return
	}

	// Kill active worker if any (best-effort, worker may already be offline)
	workerKilled := false
	if w, ok := h.db.GetLatestWorkerForEmployee(emp.ID); ok && w.Status != "offline" {
		if w.PID > 0 {
			if proc, err := os.FindProcess(w.PID); err == nil {
				proc.Signal(syscall.SIGTERM)
			}
		}
		h.db.UpdateWorkerStatus(w.ID, "offline")
		h.BroadcastWorkerStatus(emp.Nickname, "offline")
		workerKilled = true
	}

	// Mark as not employed
	h.db.UpdateEmployeeEmployed(emp.ID, 0)

	result := map[string]any{
		"nickname":      emp.Nickname,
		"employed":      0,
		"worker_killed": workerKilled,
		"message":       fmt.Sprintf("%s fired — marked as not employed", emp.Nickname),
	}
	data, _ := json.Marshal(result)
	h.sendTo(c.nick, protocol.WSMessage{
		Type:      msg.Type,
		RequestID: msg.RequestID,
		Data:      data,
	})
}

func (h *Hub) handleSpawnWorker(c *client, msg protocol.WSMessage) {
	// Authorization: only CEO can spawn workers
	if caller, err := h.db.GetEmployeeByNick(c.nick); err != nil || caller.Role != "ceo" {
		h.sendTo(c.nick, protocol.WSMessage{
			Type:      msg.Type,
			RequestID: msg.RequestID,
			Data:      json.RawMessage(`{"error":"spawn_worker is restricted to CEO role"}`),
		})
		return
	}

	var input struct {
		Nickname string `json:"nickname"`
	}
	if msg.Data != nil {
		json.Unmarshal(msg.Data, &input)
	}
	if input.Nickname == "" {
		h.sendTo(c.nick, protocol.WSMessage{
			Type:      msg.Type,
			RequestID: msg.RequestID,
			Data:      json.RawMessage(`{"error":"nickname required"}`),
		})
		return
	}

	emp, err := h.db.GetEmployeeByNick(input.Nickname)
	if err != nil {
		h.sendTo(c.nick, protocol.WSMessage{
			Type:      msg.Type,
			RequestID: msg.RequestID,
			Data:      json.RawMessage(fmt.Sprintf(`{"error":"employee %q not found"}`, input.Nickname)),
		})
		return
	}

	// Mark as employed
	h.db.UpdateEmployeeEmployed(emp.ID, 1)

	// Spawn via internal HTTP API
	body, _ := json.Marshal(map[string]any{"employee_id": emp.ID})
	resp, err := http.Post(
		fmt.Sprintf("http://localhost:%d/api/workers/spawn", h.port),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		h.sendTo(c.nick, protocol.WSMessage{
			Type:      msg.Type,
			RequestID: msg.RequestID,
			Data:      json.RawMessage(fmt.Sprintf(`{"error":"spawn failed: %s"}`, err)),
		})
		return
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		// Spawn failed — revert employed flag
		h.db.UpdateEmployeeEmployed(emp.ID, 0)
	}

	h.sendTo(c.nick, protocol.WSMessage{
		Type:      msg.Type,
		RequestID: msg.RequestID,
		Data:      json.RawMessage(data),
	})
}

// ── Reviews ─────────────────────────────────────────────────────────────

func (h *Hub) handleReviewCreate(c *client, msg protocol.WSMessage) {
	var input struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Type        string `json:"type"`
		Body        string `json:"body"`
	}
	if msg.Data != nil {
		json.Unmarshal(msg.Data, &input)
	}
	if input.Title == "" || input.Type == "" {
		return
	}
	review, err := h.db.CreateReview(c.nick, input.Title, input.Description, input.Type, input.Body)
	if err != nil {
		return
	}
	data, _ := json.Marshal(review)
	// Respond to the requester with the review ID
	h.sendTo(c.nick, protocol.WSMessage{
		Type:      msg.Type,
		RequestID: msg.RequestID,
		Data:      data,
	})
	// Broadcast to all (dashboard sees it)
	h.broadcast(protocol.WSMessage{
		Type: protocol.MsgReviewCreated,
		Data: data,
	}, "")
}

func (h *Hub) handleReviewRespond(c *client, msg protocol.WSMessage) {
	id := msg.ID
	response := msg.Text

	// Try parsing Data as object or as double-encoded string
	if msg.Data != nil {
		var input struct {
			ID       int64  `json:"id"`
			Response string `json:"response"`
		}
		// First try direct unmarshal
		if err := json.Unmarshal(msg.Data, &input); err != nil {
			// Data might be a JSON string that needs unwrapping
			var s string
			if json.Unmarshal(msg.Data, &s) == nil {
				json.Unmarshal([]byte(s), &input)
			}
		}
		if input.ID != 0 {
			id = input.ID
		}
		if input.Response != "" {
			response = input.Response
		}
	}
	if id == 0 {
		return
	}
	review, err := h.db.RespondToReview(id, response)
	if err != nil {
		return
	}
	data, _ := json.Marshal(review)

	if msg.Channel == "broadcast" {
		// Send to ALL workers + dashboards
		h.broadcast(protocol.WSMessage{
			Type: protocol.MsgReviewResponded,
			Data: data,
		}, "")
	} else {
		// Send ONLY to the requesting worker (private)
		h.sendTo(review.WorkerNick, protocol.WSMessage{
			Type: protocol.MsgReviewResponded,
			Data: data,
		})
		// Notify dashboards (commander UI) — but not other workers
		h.sendTo("commander", protocol.WSMessage{
			Type: protocol.MsgReviewResponded,
			Data: data,
		})
	}
}

func (h *Hub) handleReviewList(c *client, msg protocol.WSMessage) {
	reviews, err := h.db.ListPendingReviews()
	if err != nil {
		return
	}
	data, _ := json.Marshal(reviews)
	h.sendTo(c.nick, protocol.WSMessage{
		Type:      msg.Type,
		RequestID: msg.RequestID,
		Data:      data,
	})
}

// ── Channel History/Search ──────────────────────────────────────────────

func (h *Hub) handleChannelGrep(c *client, msg protocol.WSMessage) {
	var input struct {
		Query   string `json:"query"`
		Channel string `json:"channel"`
		Sender  string `json:"sender"`
	}
	if msg.Data != nil {
		json.Unmarshal(msg.Data, &input)
	}
	if input.Query == "" && msg.Text != "" {
		input.Query = msg.Text
	}
	if input.Channel == "" {
		input.Channel = msg.Channel
	}
	if input.Query == "" {
		return
	}
	msgs, err := h.db.GrepMessages(input.Query, input.Channel, input.Sender, 20)
	if err != nil {
		return
	}
	data, _ := json.Marshal(msgs)
	h.sendTo(c.nick, protocol.WSMessage{
		Type:      msg.Type,
		RequestID: msg.RequestID,
		Data:      data,
	})
}

func (h *Hub) handleChannelMessage(c *client, msg protocol.WSMessage) {
	id := msg.ID
	if id == 0 {
		return
	}
	m, err := h.db.GetMessage(id)
	if err != nil {
		return
	}
	data, _ := json.Marshal(m)
	h.sendTo(c.nick, protocol.WSMessage{
		Type:      msg.Type,
		RequestID: msg.RequestID,
		Data:      data,
	})
}

func (h *Hub) handleChannelHistory(c *client, msg protocol.WSMessage) {
	channel := msg.Channel
	if channel == "" {
		channel = "general"
	}
	msgs, err := h.db.GetMessages(channel, 3)
	if err != nil {
		return
	}
	data, _ := json.Marshal(msgs)
	h.sendTo(c.nick, protocol.WSMessage{
		Type:      msg.Type,
		RequestID: msg.RequestID,
		Data:      data,
	})
}

// ── Worker Channel Notify ───────────────────────────────────────────────

func (h *Hub) handleWorkerNotify(c *client, msg protocol.WSMessage) {
	to := msg.To
	text := msg.Text
	if to == "" || text == "" {
		return
	}
	// Send as a special notification type that the worker forwards to Claude's channel
	h.sendTo(to, protocol.WSMessage{
		Type: protocol.MsgWorkerNotify,
		From: c.nick,
		Text: text,
	})
}

// BroadcastWorkerStatus broadcasts a worker status change to all connected clients.
func (h *Hub) BroadcastWorkerStatus(nick, status string) {
	h.broadcast(protocol.WSMessage{
		Type:   protocol.MsgWorkerOnline,
		Nick:   nick,
		Status: status,
	}, "")
}

// ── Helpers ─────────────────────────────────────────────────────────────

// broadcast sends a message to all connected clients, optionally skipping one.
func (h *Hub) broadcast(msg protocol.WSMessage, skipNick string) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	// Snapshot clients under the lock, then write outside of it — holding
	// h.mu during a blocking network write lets one slow client stall the
	// whole hub and can deadlock against connect/disconnect (which take Lock).
	h.mu.RLock()
	targets := make([]*client, 0, len(h.clients))
	for nick, c := range h.clients {
		if nick == skipNick {
			continue
		}
		targets = append(targets, c)
	}
	h.mu.RUnlock()
	for _, c := range targets {
		c.conn.Write(context.Background(), websocket.MessageText, data)
	}
}

// broadcastToTaskSubscribers sends a message only to subscribers of the given task.
// The commander always receives all messages (dashboard needs full visibility).
func (h *Hub) broadcastToTaskSubscribers(taskID int64, msg protocol.WSMessage, skipNick string) {
	nicks, err := h.db.GetTaskSubscriberNicks(taskID)
	if err != nil {
		slog.Error("get task subscriber nicks", "task_id", taskID, "err", err)
		return
	}
	memberSet := make(map[string]bool, len(nicks))
	for _, n := range nicks {
		memberSet[n] = true
	}
	// Commander always gets task updates for dashboard visibility
	memberSet["commander"] = true

	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.RLock()
	targets := make([]*client, 0, len(h.clients))
	for nick, c := range h.clients {
		if nick == skipNick {
			continue
		}
		if !memberSet[nick] {
			continue
		}
		targets = append(targets, c)
	}
	h.mu.RUnlock()
	for _, c := range targets {
		c.conn.Write(context.Background(), websocket.MessageText, data)
	}
}

// broadcastToChannel sends a message only to members of the given channel.
// The commander always receives all messages (dashboard needs full visibility).
func (h *Hub) broadcastToChannel(channel string, msg protocol.WSMessage, skipNick string) {
	nicks, err := h.db.GetChannelMemberNicks(channel)
	if err != nil {
		slog.Error("get channel member nicks", "channel", channel, "err", err)
		return
	}
	memberSet := make(map[string]bool, len(nicks))
	for _, n := range nicks {
		memberSet[n] = true
	}
	// Commander always gets messages for dashboard visibility
	memberSet["commander"] = true

	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.RLock()
	targets := make([]*client, 0, len(h.clients))
	for nick, c := range h.clients {
		if nick == skipNick {
			continue
		}
		if !memberSet[nick] {
			continue
		}
		targets = append(targets, c)
	}
	h.mu.RUnlock()
	for _, c := range targets {
		c.conn.Write(context.Background(), websocket.MessageText, data)
	}
}

// sendTo sends a message to a specific client by nick.
func (h *Hub) sendTo(nick string, msg protocol.WSMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.RLock()
	c, ok := h.clients[nick]
	h.mu.RUnlock()
	if ok {
		c.conn.Write(context.Background(), websocket.MessageText, data)
	}
}

// IsOnline returns whether a nick is currently connected.
func (h *Hub) IsOnline(nick string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.clients[nick]
	return ok
}
