package protocol

import "encoding/json"

// WSMessage is the envelope for WebSocket messages between workers and svc-artificial.
type WSMessage struct {
	Type      string          `json:"type"`
	RequestID string          `json:"requestId,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`

	// Flattened convenience fields used by various message types.
	Nick    string `json:"nick,omitempty"`
	Channel string `json:"channel,omitempty"`
	Text    string `json:"text,omitempty"`
	To      string `json:"to,omitempty"`
	From    string `json:"from,omitempty"`
	Topic   string `json:"topic,omitempty"`
	ID      int64  `json:"id,omitempty"`
	Status  string `json:"status,omitempty"`
}

// WebSocket message types (client → server).
const (
	MsgChatSend     = "chat_send"
	MsgChatDM       = "chat_dm"
	MsgJoinChannel  = "join_channel"
	MsgLeaveChannel = "leave_channel"
	MsgSetTopic     = "set_topic"
	MsgGetMembers   = "get_members"
	MsgListChannels = "list_channels"
	MsgMarkRead       = "mark_read"
	MsgChannelGrep    = "channel_grep"
	MsgChannelMessage = "channel_message"
	MsgChannelHistory = "channel_history"
	MsgWorkerNotify  = "worker_notify"  // send a channel notification directly to a worker
	MsgWorkerCommand  = "worker_command"  // send a slash command (e.g. /compact) to a worker's claude
	MsgWorkerTTYInput  = "worker_tty_input"  // send raw keystrokes to a worker's PTY
	MsgWorkerTTYResize = "worker_tty_resize" // resize a worker's PTY (cols/rows in Text as "COLSxROWS")
)

// WebSocket message types (client → server, tasks).
const (
	MsgTaskCreate    = "task_create"
	MsgTaskUpdate    = "task_update"
	MsgTaskList      = "task_list"
	MsgTaskGet       = "task_get"
	MsgTaskSubscribe = "task_subscribe"
	MsgTaskGrep      = "task_grep"
)

// WebSocket message types (client → server, projects).
const (
	MsgProjectList   = "project_list"
	MsgProjectCreate = "project_create"
)

// WebSocket message types (client → server, recruitment).
const (
	MsgRecruit       = "recruit"
	MsgRecruitAccept = "recruit_accept"
)

// WebSocket message types (server → client broadcasts).
const (
	MsgMessage      = "message"
	MsgDM           = "dm"
	MsgMemberJoined = "member_joined"
	MsgMemberLeft   = "member_left"
	MsgTopicChanged = "topic_changed"
	MsgUnread       = "unread_messages"
	MsgTaskCreated   = "task_created"
	MsgTaskUpdated   = "task_updated"
	MsgWorkerOnline  = "worker_online"
	MsgWorkerOffline = "worker_offline"
	MsgChannelCreated  = "channel_created"
	MsgProjectCreated  = "project_created"
	MsgReviewCreated   = "review_created"
	MsgReviewResponded = "review_responded"
)

// WebSocket message types (client → server, reviews).
const (
	MsgReviewCreate  = "review_create"
	MsgReviewRespond = "review_respond"
	MsgReviewList    = "review_list"
)

// WebSocket message types (client → server, worker lifecycle).
const (
	MsgFireWorker  = "fire_worker"
	MsgSpawnWorker = "spawn_worker"
)
