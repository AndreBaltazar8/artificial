package protocol

// Employee represents a team member (agent or commander).
type Employee struct {
	ID            int64  `json:"id"`
	Nickname      string `json:"nickname"`
	Role          string `json:"role"` // "commander", "ceo", "worker"
	Persona       string `json:"persona"`
	Email         string `json:"email,omitempty"`
	Employed      int    `json:"employed"`
	Harness       string `json:"harness"` // "claude", "acp"
	Model         string `json:"model"`
	ACPURL        string `json:"acp_url,omitempty"`
	ACPProvider   string `json:"acp_provider,omitempty"`
	CreatedAt     string `json:"created_at"`
	LastConnected string `json:"last_connected,omitempty"`
}

// Project represents a codebase being worked on.
type Project struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	GitRemote string `json:"git_remote,omitempty"`
	CreatedAt string `json:"created_at"`
}

// Task represents a unit of work within a project.
type Task struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"` // backlog, todo, in_progress, in_qa, done
	Assignee    string `json:"assignee,omitempty"`
	ProjectID   int64  `json:"project_id"`
	CreatedBy   string `json:"created_by"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// Channel represents a communication channel.
type Channel struct {
	Name  string `json:"name"`
	Topic string `json:"topic"`
	SetBy string `json:"set_by,omitempty"`
	SetAt string `json:"set_at,omitempty"`
}

// Message represents a chat message in a channel or DM.
// DMs use channel format "dm:<nick1>:<nick2>" with nicks sorted alphabetically.
type Message struct {
	ID      int64  `json:"id"`
	Channel string `json:"channel"`
	Sender  string `json:"sender"`
	Text    string `json:"text"`
	TS      string `json:"ts"`
}

// Worker represents an active Claude process connected to an employee.
type Worker struct {
	ID             int64  `json:"id"`
	EmployeeID     int64  `json:"employee_id"`
	PID            int    `json:"pid"`
	Status         string `json:"status"` // idle, online, busy, offline
	SessionID      string `json:"session_id,omitempty"`
	LogPath        string `json:"log_path,omitempty"`
	TranscriptPath string `json:"transcript_path,omitempty"`
	CreatedAt      string `json:"created_at"`
	LastConnected  string `json:"last_connected,omitempty"`
}

// ReadCursor tracks the last message read by an employee in a channel/DM.
type ReadCursor struct {
	EmployeeID  int64  `json:"employee_id"`
	ChannelName string `json:"channel_name"`
	LastReadID  int64  `json:"last_read_id"`
}

// Review represents a commander review request from an agent.
type Review struct {
	ID          int64  `json:"id"`
	WorkerNick  string `json:"worker_nick"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Type        string `json:"type"`                    // choice, approval, form, info
	Body        string `json:"body"`                    // JSON string, schema depends on type
	Status      string `json:"status"`                  // pending, responded, expired
	Response    string `json:"response,omitempty"`       // JSON response from commander
	CreatedAt   string `json:"created_at"`
	RespondedAt string `json:"responded_at,omitempty"`
}

// ChannelMember represents an employee's membership in a channel.
type ChannelMember struct {
	ChannelName string `json:"channel_name"`
	EmployeeID  int64  `json:"employee_id"`
	JoinedAt    string `json:"joined_at"`
}

// Plugin represents an external go-plugin binary that workers load on
// start. Workers register their MCP tools from each enabled plugin's
// Tools() output at load time.
//
// The persisted columns (id, name, path, enabled, config, created_at)
// come straight from the `plugins` table. The runtime fields
// (LoadedInWorkers, Tools, Status, LastError) are populated from Hub
// in-memory state aggregated from MsgWorkerPluginState reports, so they
// reflect what's actually running right now — not what the DB says
// should be running.
type Plugin struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	Enabled   bool   `json:"enabled"`
	Config    any    `json:"config,omitempty"` // parsed JSON
	CreatedAt string `json:"created_at"`

	// runtime (not persisted)
	LoadedInWorkers int      `json:"loaded_in_workers"`
	Tools           []string `json:"tools,omitempty"`
	Status          string   `json:"status,omitempty"`     // enabled | disabled | error
	LastError       string   `json:"last_error,omitempty"`
}

// WorkerPluginState is the payload a worker sends to the server via
// MsgWorkerPluginState to report which plugins it has loaded and what
// tools each is exposing. Sent on worker spawn (after pluginhost has
// dispensed all enabled plugins) and on any subsequent reload event.
//
// The Hub aggregates these reports per-plugin-name in memory so the
// dashboard's /api/plugins response can show "loaded in N workers" and
// the current set of tool names without any persistence round-trip.
type WorkerPluginState struct {
	Plugins []LoadedPlugin `json:"plugins"`
}

// LoadedPlugin is a single entry in WorkerPluginState.Plugins.
//
// Error is non-empty only when the plugin failed to load on this worker
// — in that case Tools will be empty. The Hub uses that signal to flip
// the plugin's aggregated Status to "error" with LastError set.
type LoadedPlugin struct {
	Name  string   `json:"name"`
	Tools []string `json:"tools,omitempty"`
	Error string   `json:"error,omitempty"`
}
