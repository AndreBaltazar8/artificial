package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"artificial.pt/pkg-go-shared/protocol"
)

// registerRunnerAPI mounts every HTTP route used by the ephemeral
// task-runner subsystem. Wired in from registerAPI in api.go.
func (s *Server) registerRunnerAPI() {
	s.Mux.HandleFunc("GET /api/runners", s.apiListRunners)
	s.Mux.HandleFunc("POST /api/tasks/{id}/spawn-runner", s.apiSpawnRunner)
	s.Mux.HandleFunc("GET /api/tasks/{id}/runners", s.apiListRunnersForTask)
	s.Mux.HandleFunc("GET /api/runners/{id}", s.apiGetRunner)
	s.Mux.HandleFunc("GET /api/runners/{id}/config", s.apiGetRunnerConfig)
	s.Mux.HandleFunc("POST /api/runners/{id}/heartbeat", s.apiRunnerHeartbeat)
	s.Mux.HandleFunc("POST /api/runners/{id}/status", s.apiRunnerStatus)
	s.Mux.HandleFunc("POST /api/runners/{id}/cancel", s.apiCancelRunner)
	s.Mux.HandleFunc("POST /api/runners/{id}/harness-touch", s.apiRunnerHarnessTouch)
	s.Mux.HandleFunc("GET /api/runners/{id}/logs", s.apiStreamRunnerLogs)
	s.Mux.HandleFunc("GET /api/runners/{id}/tty", s.apiStreamRunnerTTY)
}

// apiRunnerHarnessTouch flips harness_in_use=1 on the runner row and
// schedules an auto-clear after harnessTouchTTL of idle. Idempotent —
// repeated calls just refresh the timer. The harness-plugin pings this
// on every Execute so the dashboard's 🎮 badge reflects active use
// without the plugin needing a structured "session" lifecycle.
//
// Auto-clear runs in a goroutine keyed by runner ID; a fresh touch
// cancels the previous timer via the per-runner mutex below. We don't
// persist the timer — a server restart just leaves the flag set until
// the next touch (or until the runner row finalizes), which is fine for
// a UI hint.
func (s *Server) apiRunnerHarnessTouch(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	if err := s.DB.SetTaskRunnerHarnessInUse(id, true); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// Broadcast so the dashboard repaints the badge instantly.
	if tr, err := s.DB.GetTaskRunner(id); err == nil {
		data, _ := json.Marshal(tr)
		s.Hub.broadcast(protocol.WSMessage{Type: protocol.MsgRunnerStatus, Data: data}, "")
	}
	scheduleHarnessClear(s, id)
	w.WriteHeader(http.StatusNoContent)
}

const harnessTouchTTL = 60 * time.Second

var (
	harnessTimersMu sync.Mutex
	harnessTimers   = map[int64]*time.Timer{}
)

func scheduleHarnessClear(s *Server, runnerID int64) {
	harnessTimersMu.Lock()
	defer harnessTimersMu.Unlock()
	if t, ok := harnessTimers[runnerID]; ok {
		t.Stop()
	}
	harnessTimers[runnerID] = time.AfterFunc(harnessTouchTTL, func() {
		if err := s.DB.SetTaskRunnerHarnessInUse(runnerID, false); err != nil {
			return
		}
		if tr, err := s.DB.GetTaskRunner(runnerID); err == nil {
			data, _ := json.Marshal(tr)
			s.Hub.broadcast(protocol.WSMessage{Type: protocol.MsgRunnerStatus, Data: data}, "")
		}
		harnessTimersMu.Lock()
		delete(harnessTimers, runnerID)
		harnessTimersMu.Unlock()
	})
}

// findWorkerBin locates the cmd-worker binary the runner spawn (and
// the persistent-worker spawn in api.go) execs. Two name conventions
// to handle: `make` produces `cmd-worker`, `go install
// ./src/cmd-worker/cmd/worker/` produces `worker`. Sibling lookup
// (next to this binary) is preferred so a `go install` of both
// `artificial` and `worker` into ~/go/bin Just Works without flags.
// Returns "" if neither name is found anywhere.
func findWorkerBin() string {
	candidates := []string{"cmd-worker", "worker"}
	if self, err := os.Executable(); err == nil && self != "" {
		dir := filepath.Dir(self)
		for _, name := range candidates {
			p := filepath.Join(dir, name)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	for _, name := range candidates {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// apiListRunners returns runners across all tasks. With ?active=1 it
// filters to non-terminal status (running/blocked) — that's what the
// board uses to badge tasks. Without the flag, the route is currently
// equivalent (no terminal-listing use case yet).
func (s *Server) apiListRunners(w http.ResponseWriter, r *http.Request) {
	active := r.URL.Query().Get("active") == "1"
	if active {
		runners, err := s.DB.ListActiveRunners()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, runners)
		return
	}
	// No "all runners" listing path needed yet; mirror active for now.
	runners, err := s.DB.ListActiveRunners()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, runners)
}

// runnerWorktreesDir is where every runner's git worktree gets created.
// External to the project tree on purpose: a runner that does
// `rm -rf .` on its own worktree shouldn't damage the source repo.
func runnerWorktreesDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "artificial", "worktrees")
}

// runnerLogsDir mirrors the persistent-worker logs folder so dashboard
// log streaming works the same for both worker types.
func runnerLogsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "artificial", "logs")
}

// shortHex returns 6 hex chars — enough entropy to avoid collisions
// inside a single task's runner history without bloating the nick.
func shortHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ── Spawn ───────────────────────────────────────────────────────────────

// apiSpawnRunner is the manual entry point: POST /api/tasks/{id}/spawn-runner.
// Body: {"parent_nick": "manager-bob"} — optional, defaults to the
// task's assignee or "commander". Returns the runner row.
func (s *Server) apiSpawnRunner(w http.ResponseWriter, r *http.Request) {
	taskID := pathID(r, "id")
	var input struct {
		ParentNick string `json:"parent_nick"`
	}
	json.NewDecoder(r.Body).Decode(&input)

	tr, err := s.spawnRunnerForTask(taskID, input.ParentNick)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(201)
	writeJSON(w, tr)
}

// spawnRunnerForTask is the shared spawn path used both by the manual
// HTTP endpoint and by the auto-trigger that fires when a task's
// status moves to in_progress (see apiUpdateTask). Returns the
// freshly-created TaskRunner row, or an error before any side effects
// have been committed (worktree, fork) so the caller can decide
// whether to propagate or quietly retry.
func (s *Server) spawnRunnerForTask(taskID int64, parentNick string) (protocol.TaskRunner, error) {
	task, err := s.DB.GetTask(taskID)
	if err != nil {
		return protocol.TaskRunner{}, fmt.Errorf("task not found: %w", err)
	}

	// Refuse if a non-terminal runner already exists. Two runners on
	// one task means two worktrees stomping on each other.
	if existing, err := s.DB.GetActiveRunnerForTask(taskID); err == nil {
		return protocol.TaskRunner{}, fmt.Errorf("task already has active runner %q (id=%d, status=%s)", existing.Nickname, existing.ID, existing.Status)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return protocol.TaskRunner{}, fmt.Errorf("check active runner: %w", err)
	}

	if task.ProjectID == 0 {
		return protocol.TaskRunner{}, errors.New("task has no project; cannot infer worktree source")
	}
	project, err := s.DB.GetProject(task.ProjectID)
	if err != nil {
		return protocol.TaskRunner{}, fmt.Errorf("project not found: %w", err)
	}
	if project.Path == "" {
		return protocol.TaskRunner{}, errors.New("project has no path; cannot create worktree")
	}
	absProjectPath, err := filepath.Abs(project.Path)
	if err != nil {
		return protocol.TaskRunner{}, fmt.Errorf("resolve project path: %w", err)
	}
	if !isGitRepo(absProjectPath) {
		return protocol.TaskRunner{}, fmt.Errorf("project path %q is not a git repo", absProjectPath)
	}

	if parentNick == "" {
		parentNick = task.Assignee
	}
	if parentNick == "" {
		parentNick = "commander"
	}

	suffix := shortHex(3)
	nick := fmt.Sprintf("runner-t%d-%s", taskID, suffix)
	branch := fmt.Sprintf("runner/t%d-%s", taskID, suffix)
	worktree := filepath.Join(runnerWorktreesDir(), fmt.Sprintf("t%d-%s", taskID, suffix))

	baseBranch, err := currentBranch(absProjectPath)
	if err != nil {
		// Non-fatal — git worktree can still create a branch from HEAD.
		slog.Warn("could not resolve base branch, proceeding", "err", err)
		baseBranch = ""
	}

	if err := os.MkdirAll(runnerWorktreesDir(), 0755); err != nil {
		return protocol.TaskRunner{}, fmt.Errorf("mkdir worktrees: %w", err)
	}
	if err := createWorktree(absProjectPath, worktree, branch); err != nil {
		return protocol.TaskRunner{}, fmt.Errorf("create worktree: %w", err)
	}

	if err := os.MkdirAll(runnerLogsDir(), 0755); err != nil {
		// Non-fatal — runner will still spawn, just without a log file.
		slog.Warn("mkdir runner logs", "err", err)
	}
	logPath := filepath.Join(runnerLogsDir(), fmt.Sprintf("runner-%s-%d.log", nick, time.Now().Unix()))

	tr, err := s.DB.CreateTaskRunner(taskID, nick, parentNick, worktree, branch, baseBranch, logPath)
	if err != nil {
		// Roll back the worktree we just created — the task_runners
		// row is the source of truth, no row means no runner.
		removeWorktree(absProjectPath, worktree)
		return protocol.TaskRunner{}, fmt.Errorf("persist runner row: %w", err)
	}

	pid, err := s.forkRunnerProcess(tr, logPath)
	if err != nil {
		s.DB.MarkRunnerCrashed(tr.ID, "fork failed: "+err.Error())
		removeWorktree(absProjectPath, worktree)
		return protocol.TaskRunner{}, fmt.Errorf("fork runner: %w", err)
	}
	s.DB.UpdateTaskRunnerPID(tr.ID, pid)
	tr.PID = pid

	// Notify the parent (manager-worker) that the runner is up. The
	// parent's handleHubMessage in cmd-worker doesn't yet have a case
	// for MsgRunnerSpawned, so this is also picked up by the
	// commander dashboard which listens for everything.
	data, _ := json.Marshal(tr)
	s.Hub.broadcast(protocol.WSMessage{
		Type: protocol.MsgRunnerSpawned,
		Data: data,
	}, "")

	slog.Info("runner spawned",
		"runner_id", tr.ID,
		"task_id", tr.TaskID,
		"nick", tr.Nickname,
		"pid", pid,
		"worktree", tr.WorktreePath,
		"branch", tr.BranchName,
	)
	return tr, nil
}

// forkRunnerProcess execs cmd-worker in --task-runner mode. Mirrors
// apiSpawnWorker's fork pattern (Setsid for survival across server
// restarts, log file redirection) but doesn't pre-create a workers
// row — runners live in their own table.
func (s *Server) forkRunnerProcess(tr protocol.TaskRunner, logPath string) (int, error) {
	workerBin := s.WorkerBin
	if workerBin == "" {
		// Same fallback chain as apiSpawnWorker: try sibling binary,
		// then PATH. Keeps `make run-artificial` working without an
		// explicit --worker-bin flag. The `make` target produces
		// `cmd-worker`; `go install ./src/cmd-worker/cmd/worker/`
		// produces `worker` — try both names in either location.
		workerBin = findWorkerBin()
	}
	if workerBin == "" {
		return 0, errors.New("cmd-worker binary not found")
	}

	serverAddr := fmt.Sprintf("localhost:%d", s.Port)
	cmd := exec.Command(workerBin,
		"--server", serverAddr,
		"--task-runner",
		"--runner-id", strconv.FormatInt(tr.ID, 10),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	// Per-runner GOCACHE so Claude's `go build` calls inside the worktree
	// don't collide with whatever the human's interactive shell is doing
	// in the shared ~/Library/Caches/go-build. Concurrent writes to that
	// cache from a sandboxed-ish subprocess intermittently produce
	// "operation not permitted" errors that wedge the runner. Each
	// runner gets its own cache dir under the worktree so a `git clean`
	// between runs is enough to reclaim space.
	runnerCacheDir := filepath.Join(tr.WorktreePath, ".artificial-gocache")
	_ = os.MkdirAll(runnerCacheDir, 0o755)
	cmd.Env = append(os.Environ(), "GOCACHE="+runnerCacheDir)

	logFile, logErr := os.Create(logPath)
	if logErr == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	if err := cmd.Start(); err != nil {
		if logFile != nil {
			logFile.Close()
		}
		return 0, err
	}
	if logFile != nil {
		logFile.Close()
	}
	pid := cmd.Process.Pid
	cmd.Process.Release()
	return pid, nil
}

// ── Git plumbing ────────────────────────────────────────────────────────

func isGitRepo(path string) bool {
	cmd := exec.Command("git", "-C", path, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

func currentBranch(path string) (string, error) {
	cmd := exec.Command("git", "-C", path, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// createWorktree creates a worktree at worktreePath on a new branch.
// Procedure mirrors the project's create-worktree skill so it works for
// repos protected by git-crypt:
//   1. `git worktree add --no-checkout` so smudge filters never run
//      against files we can't decrypt yet.
//   2. Symlink .git/worktrees/<name>/git-crypt -> ../../git-crypt so the
//      git-crypt smudge filter, when it does run, can find the keys
//      stored in the main repo's .git/git-crypt directory.
//   3. `git checkout HEAD .` inside the worktree to materialize files —
//      now smudge has access to the keys via the symlink.
//
// Safe for non-git-crypt repos: the symlink is only created if the
// source directory exists, and `git checkout HEAD .` is a no-op when
// no smudge filter is registered.
func createWorktree(repoPath, worktreePath, branchName string) error {
	cmd := exec.Command("git", "-C", repoPath, "worktree", "add", "--no-checkout", "-b", branchName, worktreePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree add: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Symlink git-crypt state into the worktree's git metadata so smudge
	// filters can find the keys. Skip silently if the source repo doesn't
	// use git-crypt.
	cryptSrc := filepath.Join(repoPath, ".git", "git-crypt")
	if _, err := os.Stat(cryptSrc); err == nil {
		wtName := filepath.Base(worktreePath)
		cryptLink := filepath.Join(repoPath, ".git", "worktrees", wtName, "git-crypt")
		if err := os.Symlink("../../git-crypt", cryptLink); err != nil && !os.IsExist(err) {
			slog.Warn("create git-crypt symlink", "link", cryptLink, "err", err)
		}
	}

	cmd = exec.Command("git", "-C", worktreePath, "checkout", "HEAD", ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git checkout in worktree: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// removeWorktree is best-effort cleanup. Failures are logged but never
// propagated — leaving an orphan directory is preferable to wedging
// the spawn path on a missing executable or permission error.
func removeWorktree(repoPath, worktreePath string) {
	cmd := exec.Command("git", "-C", repoPath, "worktree", "remove", "--force", worktreePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		slog.Warn("git worktree remove failed", "path", worktreePath, "err", err, "output", string(out))
	}
}

// ── HTTP handlers (runner self-service) ─────────────────────────────────

// apiGetRunner is a generic accessor used by the dashboard.
func (s *Server) apiGetRunner(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	tr, err := s.DB.GetTaskRunner(id)
	if err != nil {
		writeErr(w, 404, "runner not found")
		return
	}
	writeJSON(w, tr)
}

// apiGetRunnerConfig is what the cmd-worker booting in --task-runner
// mode calls. Returns enough to skip employee config entirely:
// runner row + task body + project + the server's own loopback addr
// (in case the worker needs to construct further REST URLs).
func (s *Server) apiGetRunnerConfig(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	tr, err := s.DB.GetTaskRunner(id)
	if err != nil {
		writeErr(w, 404, "runner not found")
		return
	}
	task, err := s.DB.GetTask(tr.TaskID)
	if err != nil {
		writeErr(w, 500, "task not found: "+err.Error())
		return
	}
	cfg := protocol.RunnerConfig{
		Runner:     tr,
		Task:       task,
		ServerAddr: fmt.Sprintf("localhost:%d", s.Port),
	}
	if task.ProjectID > 0 {
		if proj, err := s.DB.GetProject(task.ProjectID); err == nil {
			cfg.Project = &proj
		}
	}
	writeJSON(w, cfg)
}

// apiRunnerHeartbeat is the simple liveness ping. The runner posts
// every 20s; the watchdog tolerates ~3 missed pings.
func (s *Server) apiRunnerHeartbeat(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if err := s.DB.RecordRunnerHeartbeat(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

// apiRunnerStatus is the runner's terminal-status backstop. The
// happy path is a WS MsgRunnerComplete message — but if the WS is
// down at exit time (or the runner crashed mid-flight and main()
// caught the exit), the cmd-worker also POSTs the final status here.
// Idempotent: re-posting a terminal status is a no-op.
func (s *Server) apiRunnerStatus(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	var input struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	tr, err := s.DB.GetTaskRunner(id)
	if err != nil {
		writeErr(w, 404, "not found")
		return
	}
	// If already terminal, return the existing row without rewriting —
	// keeps the first reason intact (the WS message usually wins the
	// race against the HTTP backstop).
	switch tr.Status {
	case protocol.RunnerStatusComplete, protocol.RunnerStatusCrashed, protocol.RunnerStatusCancelled:
		writeJSON(w, tr)
		return
	}
	switch input.Status {
	case protocol.RunnerStatusComplete:
		s.DB.MarkRunnerComplete(id, input.Reason)
	case protocol.RunnerStatusCrashed:
		s.DB.MarkRunnerCrashed(id, input.Reason)
	case protocol.RunnerStatusCancelled:
		s.DB.UpdateTaskRunnerStatus(id, protocol.RunnerStatusCancelled)
	default:
		writeErr(w, 400, "unsupported status")
		return
	}
	updated, _ := s.DB.GetTaskRunner(id)
	s.broadcastRunnerStatus(updated)
	writeJSON(w, updated)
}

// apiCancelRunner is the manager's "stop this runner" button. SIGTERMs
// the process and marks the row cancelled. Worktree is left in place
// for inspection — the manager decides whether to discard or salvage.
func (s *Server) apiCancelRunner(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	tr, err := s.DB.GetTaskRunner(id)
	if err != nil {
		writeErr(w, 404, "not found")
		return
	}
	if tr.PID > 0 {
		if proc, err := os.FindProcess(tr.PID); err == nil {
			proc.Signal(syscall.SIGTERM)
		}
	}
	s.DB.UpdateTaskRunnerStatus(id, protocol.RunnerStatusCancelled)
	updated, _ := s.DB.GetTaskRunner(id)
	s.broadcastRunnerStatus(updated)
	w.WriteHeader(204)
}

// apiListRunnersForTask returns every runner ever spawned for a task,
// newest first. Powers the runner-history strip in the task detail
// modal.
func (s *Server) apiListRunnersForTask(w http.ResponseWriter, r *http.Request) {
	taskID := pathID(r, "id")
	runners, err := s.DB.ListRunnersForTask(taskID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, runners)
}

// broadcastRunnerStatus pushes a MsgRunnerStatus update over the WS
// hub. The dashboard listens for this on the global socket; a
// manager-worker that owns the runner picks it up via its own client.
func (s *Server) broadcastRunnerStatus(tr protocol.TaskRunner) {
	data, _ := json.Marshal(tr)
	s.Hub.broadcast(protocol.WSMessage{
		Type: protocol.MsgRunnerStatus,
		Data: data,
	}, "")
}

// ── Watchdog ────────────────────────────────────────────────────────────

// runRunnerWatchdog scans every active runner periodically and marks
// crashed any whose PID is gone or whose last heartbeat is older than
// the threshold. v1 is intentionally simple: we don't try to revive
// or auto-respawn — a crashed runner surfaces in the dashboard, the
// manager decides what to do.
//
// Runs until ctx cancels (server shutdown). Intended to be go'd from
// Run() right after the host plugins reconcile.
func (s *Server) runRunnerWatchdog(ctx context.Context) {
	const (
		interval        = 30 * time.Second
		heartbeatStale  = 90 * time.Second // ~3 missed pings
		spawnGrace      = 60 * time.Second // give a fresh runner this long before we expect heartbeats
	)
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.scanRunnersOnce(heartbeatStale, spawnGrace)
		}
	}
}

func (s *Server) scanRunnersOnce(heartbeatStale, spawnGrace time.Duration) {
	runners, err := s.DB.ListActiveRunners()
	if err != nil {
		slog.Warn("watchdog: list active runners failed", "err", err)
		return
	}
	now := time.Now().UTC()
	for _, r := range runners {
		// PID liveness: if the process is gone, mark crashed.
		if r.PID > 0 && !pidAlive(r.PID) {
			slog.Info("watchdog: runner pid is dead, marking crashed", "runner_id", r.ID, "pid", r.PID)
			s.DB.MarkRunnerCrashed(r.ID, "process exited without reporting status")
			updated, _ := s.DB.GetTaskRunner(r.ID)
			s.broadcastRunnerStatus(updated)
			continue
		}
		// Heartbeat staleness: only meaningful once the spawn grace
		// has elapsed (otherwise a brand-new runner that hasn't sent
		// its first ping yet looks dead).
		started, _ := time.Parse(time.DateTime, r.StartedAt)
		if now.Sub(started) < spawnGrace {
			continue
		}
		lastHB := r.LastHeartbeat
		if lastHB == "" {
			lastHB = r.StartedAt
		}
		hb, err := time.Parse(time.DateTime, lastHB)
		if err != nil {
			continue
		}
		if now.Sub(hb) > heartbeatStale {
			slog.Warn("watchdog: heartbeat stale, marking crashed",
				"runner_id", r.ID, "pid", r.PID, "last_hb", lastHB,
			)
			s.DB.MarkRunnerCrashed(r.ID, fmt.Sprintf("no heartbeat for %s", now.Sub(hb)))
			updated, _ := s.DB.GetTaskRunner(r.ID)
			s.broadcastRunnerStatus(updated)
		}
	}
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 is the standard "process exists?" probe.
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return true
}

// ── WS plumbing helpers ─────────────────────────────────────────────────

// notifyParentOfRunnerEvent forwards a runner-originated event to the
// parent (manager-worker) as a structured worker_notify so the
// manager Claude sees it as a channel notification. Best-effort —
// drops silently if the parent isn't online.
func (s *Server) notifyParentOfRunnerEvent(parentNick, msg string) {
	if parentNick == "" || parentNick == "commander" {
		return
	}
	s.Hub.sendTo(parentNick, protocol.WSMessage{
		Type: protocol.MsgWorkerNotify,
		From: "runner-system",
		Text: msg,
	})
}

// drainBody consumes and closes an HTTP response body — small DRY
// helper used in a couple of places to avoid leaking idle connections.
func drainBody(b io.ReadCloser) {
	_, _ = io.Copy(io.Discard, b)
	b.Close()
}

var _ = drainBody // reserved for future use; keeping the helper in tree

// apiStreamRunnerLogs streams the runner's slog/stdout file (the file
// at runner.LogPath created by forkRunnerProcess). Mirrors the worker
// /logs endpoint so the dashboard can reuse its log overlay UI.
func (s *Server) apiStreamRunnerLogs(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	tr, err := s.DB.GetTaskRunner(id)
	if err != nil {
		writeErr(w, 404, "runner not found")
		return
	}
	if tr.LogPath == "" {
		writeErr(w, 404, "no log file for this runner")
		return
	}
	s.streamFile(w, r, tr.LogPath)
}

// apiStreamRunnerTTY streams the runner's PTY .tty file via SSE. The
// file is named <runner-nick>-<timestamp>.tty inside the artificial
// logs dir — same layout as the worker variant, just keyed on the
// runner nickname instead of an employee.
func (s *Server) apiStreamRunnerTTY(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	tr, err := s.DB.GetTaskRunner(id)
	if err != nil {
		writeErr(w, 404, "runner not found")
		return
	}
	home, _ := os.UserHomeDir()
	logsDir := filepath.Join(home, ".config", "artificial", "logs")

	entries, _ := os.ReadDir(logsDir)
	var ttyPath string
	// Pick the most recent matching tty file. ReadDir returns entries
	// in directory order; iterate in reverse so newer timestamps win.
	for i := len(entries) - 1; i >= 0; i-- {
		name := entries[i].Name()
		if strings.HasPrefix(name, tr.Nickname+"-") && strings.HasSuffix(name, ".tty") {
			ttyPath = filepath.Join(logsDir, name)
			break
		}
	}
	if ttyPath == "" {
		writeErr(w, 404, "no TTY log found for this runner")
		return
	}

	f, err := os.Open(ttyPath)
	if err != nil {
		writeErr(w, 404, "tty file not found")
		return
	}
	defer f.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	ctx := r.Context()
	tmp := make([]byte, 16*1024)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for {
				n, _ := f.Read(tmp)
				if n == 0 {
					break
				}
				// Same SSE encoding as the worker handler: base64 each
				// chunk so xterm.js can replay raw escape sequences.
				fmt.Fprintf(w, "data: %s\n\n", base64.StdEncoding.EncodeToString(tmp[:n]))
			}
			flusher.Flush()
		}
	}
}
