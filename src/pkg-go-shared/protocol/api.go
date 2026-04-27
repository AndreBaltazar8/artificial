package protocol

// EmployeeConfig is returned by GET /api/employees/:id to give a worker
// everything it needs to start: persona, channels, project path, etc.
type EmployeeConfig struct {
	Employee             Employee `json:"employee"`
	Channels             []string `json:"channels"`                         // channel names this employee is a member of
	Project              *Project `json:"project,omitempty"`                // assigned project (if any)
	Returning            bool     `json:"returning"`                        // true if this employee has had a previous worker
	PreviousSessionID    string   `json:"previous_session_id,omitempty"`    // last Claude session ID for resume
	CompanyKnowledgePath string   `json:"company_knowledge_path,omitempty"` // path to company knowledge directory
}

// RunnerConfig is returned by GET /api/runners/{id}/config so the
// cmd-worker booting in --task-runner mode has everything it needs
// without going through employee-config: the task to work on, the
// worktree it should `cd` into, the branch it should commit to, and
// the parent worker (manager) that should hear runner messages.
type RunnerConfig struct {
	Runner     TaskRunner `json:"runner"`
	Task       Task       `json:"task"`
	Project    *Project   `json:"project,omitempty"`
	ServerAddr string     `json:"server_addr"`
	Harness    string     `json:"harness"`
	Model      string     `json:"model,omitempty"`
}

// RunnerCheckpointPayload is the body of MsgRunnerCheckpoint and the
// /api/runners/{id}/checkpoint POST endpoint. Lets the runner record
// progress in a structured way without needing chat — useful both as a
// crash-safe "what was I doing" log and as a place for the manager UI
// to surface incremental work.
type RunnerCheckpointPayload struct {
	Summary      string   `json:"summary"`
	Commits      []string `json:"commits,omitempty"`
	FilesTouched []string `json:"files_touched,omitempty"`
}

// RunnerCompletePayload is the body of MsgRunnerComplete and the
// /api/runners/{id}/complete POST endpoint. The runner emits this when
// its work is finished and committed; the server marks the runner
// terminal, optionally moves the task to in_qa, and notifies the
// manager so it can review the branch.
type RunnerCompletePayload struct {
	Summary    string   `json:"summary"`
	BranchName string   `json:"branch_name,omitempty"`
	Commits    []string `json:"commits,omitempty"`
}

// RunnerBlockedPayload is the body of MsgRunnerBlocked and the
// /api/runners/{id}/blocked POST endpoint. Used when the runner can't
// make progress on its own — e.g. ambiguous spec, missing credentials,
// failing test it can't diagnose. Surfaces in the manager dashboard.
type RunnerBlockedPayload struct {
	Reason   string `json:"reason"`
	Question string `json:"question,omitempty"`
}

// TaskRunnerSpawnRequest is sent by long-lived workers that want the server to
// create an ephemeral runner for a task. ParentNick is a privileged override;
// ordinary workers are always set as their own runner parent.
type TaskRunnerSpawnRequest struct {
	TaskID     int64  `json:"task_id"`
	ParentNick string `json:"parent_nick,omitempty"`
}

// TaskRunnerListRequest lists active runners globally when TaskID is zero, or
// a single task's runner history when TaskID is set.
type TaskRunnerListRequest struct {
	TaskID int64 `json:"task_id,omitempty"`
}

// TaskRunnerGetRequest inspects a runner by runner ID, or the active/latest
// runner for a task when TaskID is supplied instead.
type TaskRunnerGetRequest struct {
	RunnerID int64 `json:"runner_id,omitempty"`
	TaskID   int64 `json:"task_id,omitempty"`
}

// TaskRunnerCancelRequest cancels a runner by runner ID, or the active runner
// for a task when TaskID is supplied instead.
type TaskRunnerCancelRequest struct {
	RunnerID int64 `json:"runner_id,omitempty"`
	TaskID   int64 `json:"task_id,omitempty"`
}

// TaskRunnerManageResponse is the common response body for manager-facing
// task-runner WS operations.
type TaskRunnerManageResponse struct {
	Runner  *TaskRunner  `json:"runner,omitempty"`
	Runners []TaskRunner `json:"runners,omitempty"`
	Message string       `json:"message,omitempty"`
	Error   string       `json:"error,omitempty"`
}

// DMChannelName returns the canonical DM channel name for two nicknames.
// Nicks are sorted alphabetically so dm:alice:bob == dm:bob:alice.
func DMChannelName(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return "dm:" + a + ":" + b
}
