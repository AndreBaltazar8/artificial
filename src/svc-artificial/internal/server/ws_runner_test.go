package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"artificial.pt/pkg-go-shared/protocol"
	"artificial.pt/svc-artificial/internal/db"
)

type autoSpawnCall struct {
	taskID     int64
	parentNick string
}

func TestHandleTaskUpdateAutoSpawnsOnInProgressTransition(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "artificial.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	project, err := database.CreateProject("test", t.TempDir(), "")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := database.CreateEmployee("manager", "manager", "", ""); err != nil {
		t.Fatalf("create employee: %v", err)
	}
	task, err := database.CreateTask("runner task", "do work", "", project.ID, "commander")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	hub := NewHub(database, 0)
	calls := make(chan autoSpawnCall, 2)
	hub.autoSpawnRunnerForTask = func(taskID int64, parentNick string) {
		calls <- autoSpawnCall{taskID: taskID, parentNick: parentNick}
	}

	payload, _ := json.Marshal(map[string]any{
		"id":       task.ID,
		"status":   "in_progress",
		"assignee": "manager",
	})
	hub.handleTaskUpdate(&client{nick: "manager"}, protocol.WSMessage{
		Type: protocol.MsgTaskUpdate,
		Data: payload,
	})

	select {
	case got := <-calls:
		if got.taskID != task.ID {
			t.Fatalf("auto-spawn task id = %d, want %d", got.taskID, task.ID)
		}
		if got.parentNick != "manager" {
			t.Fatalf("auto-spawn parent = %q, want manager", got.parentNick)
		}
	case <-time.After(time.Second):
		t.Fatal("auto-spawn was not called")
	}

	updated, err := database.GetTask(task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if updated.Status != "in_progress" {
		t.Fatalf("task status = %q, want in_progress", updated.Status)
	}
	if updated.Assignee != "manager" {
		t.Fatalf("task assignee = %q, want manager", updated.Assignee)
	}

	repeatPayload, _ := json.Marshal(map[string]any{
		"id":     task.ID,
		"status": "in_progress",
	})
	hub.handleTaskUpdate(&client{nick: "manager"}, protocol.WSMessage{
		Type: protocol.MsgTaskUpdate,
		Data: repeatPayload,
	})
	assertNoAutoSpawnCall(t, calls)

	task2, err := database.CreateTask("non runner task", "do work", "", project.ID, "commander")
	if err != nil {
		t.Fatalf("create second task: %v", err)
	}
	todoPayload, _ := json.Marshal(map[string]any{
		"id":     task2.ID,
		"status": "todo",
	})
	hub.handleTaskUpdate(&client{nick: "manager"}, protocol.WSMessage{
		Type: protocol.MsgTaskUpdate,
		Data: todoPayload,
	})
	assertNoAutoSpawnCall(t, calls)
}

func TestHandleTaskUpdateAutoSpawnsRunnerRowThroughSharedPath(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "artificial.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	workerBin := setupRunnerSpawnTestEnvironment(t)
	repoPath := initGitRepo(t)
	project, err := database.CreateProject("test", repoPath, "")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	task, err := database.CreateTask("runner task", "do work", "manager", project.ID, "commander")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	s := New(database, 0, workerBin)
	payload, _ := json.Marshal(map[string]any{
		"id":     task.ID,
		"status": "in_progress",
	})
	s.Hub.handleTaskUpdate(&client{nick: "manager"}, protocol.WSMessage{
		Type: protocol.MsgTaskUpdate,
		Data: payload,
	})

	runner := waitForActiveRunner(t, database, task.ID)
	if runner.TaskID != task.ID {
		t.Fatalf("runner task id = %d, want %d", runner.TaskID, task.ID)
	}
	if runner.ParentNick != "manager" {
		t.Fatalf("runner parent = %q, want manager", runner.ParentNick)
	}
	runners, err := database.ListRunnersForTask(task.ID)
	if err != nil {
		t.Fatalf("list runners: %v", err)
	}
	if len(runners) != 1 {
		t.Fatalf("runner count = %d, want 1", len(runners))
	}
}

func TestHandleRunnerCompleteDoesNotCreateReview(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "artificial.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	project, err := database.CreateProject("test", t.TempDir(), "")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	task, err := database.CreateTask("runner task", "do work", "", project.ID, "commander")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	runner, err := database.CreateTaskRunner(task.ID, "runner-test", "manager", "/tmp/worktree", "runner/test", "master", "")
	if err != nil {
		t.Fatalf("create runner: %v", err)
	}

	hub := NewHub(database, 0)
	payload, _ := json.Marshal(protocol.RunnerCompletePayload{
		Summary:    "implemented",
		BranchName: "runner/test",
		Commits:    []string{"abc123"},
	})
	hub.handleRunnerComplete(nil, protocol.WSMessage{
		Type: protocol.MsgRunnerComplete,
		ID:   runner.ID,
		Data: payload,
	})

	updatedRunner, err := database.GetTaskRunner(runner.ID)
	if err != nil {
		t.Fatalf("get runner: %v", err)
	}
	if updatedRunner.Status != protocol.RunnerStatusComplete {
		t.Fatalf("runner status = %q, want %q", updatedRunner.Status, protocol.RunnerStatusComplete)
	}
	if updatedRunner.LastSummary != "implemented" {
		t.Fatalf("runner summary = %q, want implemented", updatedRunner.LastSummary)
	}
	if updatedRunner.FinishedAt == "" {
		t.Fatal("runner finished_at was not set")
	}

	updatedTask, err := database.GetTask(task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if updatedTask.Status != "in_qa" {
		t.Fatalf("task status = %q, want in_qa", updatedTask.Status)
	}

	reviews, err := database.ListPendingReviews()
	if err != nil {
		t.Fatalf("list reviews: %v", err)
	}
	if len(reviews) != 0 {
		t.Fatalf("pending reviews = %d, want 0", len(reviews))
	}
}

func assertNoAutoSpawnCall(t *testing.T, calls <-chan autoSpawnCall) {
	t.Helper()
	select {
	case got := <-calls:
		t.Fatalf("unexpected auto-spawn call for task %d parent %q", got.taskID, got.parentNick)
	default:
	}
}

func setupRunnerSpawnTestEnvironment(t *testing.T) string {
	t.Helper()

	t.Setenv("HOME", t.TempDir())
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "claude"), "#!/bin/sh\nexit 0\n")
	workerBin := filepath.Join(binDir, "cmd-worker")
	writeExecutable(t, workerBin, "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return workerBin
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0755); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not found")
	}
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s: %v", args, string(out), err)
		}
	}
	runGit("init")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("test\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit("add", "README.md")
	runGit("commit", "-m", "initial")
	return dir
}

func waitForActiveRunner(t *testing.T, database *db.DB, taskID int64) protocol.TaskRunner {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		runner, err := database.GetActiveRunnerForTask(taskID)
		if err == nil {
			return runner
		}
		lastErr = err
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("active runner was not created for task %d: %v", taskID, lastErr)
	return protocol.TaskRunner{}
}

func TestTaskRunnerManagementSpawnListCancel(t *testing.T) {
	database, hub, task := newRunnerManagementFixture(t)

	spawn := sendRunnerManagement(t, hub, "manager", protocol.MsgTaskRunnerSpawn, protocol.TaskRunnerSpawnRequest{TaskID: task.ID})
	if spawn.Runner == nil {
		t.Fatalf("spawn returned no runner: %+v", spawn)
	}
	if spawn.Runner.ParentNick != "manager" {
		t.Fatalf("spawn parent = %q, want manager", spawn.Runner.ParentNick)
	}

	list := sendRunnerManagement(t, hub, "manager", protocol.MsgTaskRunnerList, protocol.TaskRunnerListRequest{})
	if len(list.Runners) != 1 || list.Runners[0].ID != spawn.Runner.ID {
		t.Fatalf("active list = %+v, want spawned runner", list.Runners)
	}

	get := sendRunnerManagement(t, hub, "manager", protocol.MsgTaskRunnerGet, protocol.TaskRunnerGetRequest{TaskID: task.ID})
	if get.Runner == nil || get.Runner.ID != spawn.Runner.ID {
		t.Fatalf("get by task = %+v, want spawned runner", get.Runner)
	}

	cancel := sendRunnerManagement(t, hub, "manager", protocol.MsgTaskRunnerCancel, protocol.TaskRunnerCancelRequest{RunnerID: spawn.Runner.ID})
	if cancel.Runner == nil || cancel.Runner.Status != protocol.RunnerStatusCancelled {
		t.Fatalf("cancel = %+v, want cancelled runner", cancel.Runner)
	}
	updated, err := database.GetTaskRunner(spawn.Runner.ID)
	if err != nil {
		t.Fatalf("get cancelled runner: %v", err)
	}
	if updated.Status != protocol.RunnerStatusCancelled {
		t.Fatalf("database status = %q, want cancelled", updated.Status)
	}
}

func TestTaskRunnerManagementPermissions(t *testing.T) {
	database, hub, task := newRunnerManagementFixture(t)
	runner, err := database.CreateTaskRunner(task.ID, "runner-owned", "manager", "/tmp/worktree", "runner/test", "master", "")
	if err != nil {
		t.Fatalf("create runner: %v", err)
	}

	otherCancel := sendRunnerManagementAllowError(t, hub, "other", protocol.MsgTaskRunnerCancel, protocol.TaskRunnerCancelRequest{RunnerID: runner.ID})
	if !strings.Contains(otherCancel.Error, "owned by manager") {
		t.Fatalf("other cancel error = %q, want ownership error", otherCancel.Error)
	}
	unchanged, err := database.GetTaskRunner(runner.ID)
	if err != nil {
		t.Fatalf("get runner: %v", err)
	}
	if unchanged.Status != protocol.RunnerStatusRunning {
		t.Fatalf("unauthorized cancel changed status to %q", unchanged.Status)
	}

	override := sendRunnerManagementAllowError(t, hub, "manager", protocol.MsgTaskRunnerSpawn, protocol.TaskRunnerSpawnRequest{
		TaskID:     task.ID,
		ParentNick: "other",
	})
	if !strings.Contains(override.Error, "parent_nick override") {
		t.Fatalf("parent override error = %q", override.Error)
	}

	runnerList := sendRunnerManagementAllowError(t, hub, "runner-t1-test", protocol.MsgTaskRunnerList, protocol.TaskRunnerListRequest{})
	if !strings.Contains(runnerList.Error, "not available to task runners") {
		t.Fatalf("runner identity error = %q", runnerList.Error)
	}

	ceoCancel := sendRunnerManagement(t, hub, "boss", protocol.MsgTaskRunnerCancel, protocol.TaskRunnerCancelRequest{RunnerID: runner.ID})
	if ceoCancel.Runner == nil || ceoCancel.Runner.Status != protocol.RunnerStatusCancelled {
		t.Fatalf("ceo cancel = %+v, want cancelled", ceoCancel.Runner)
	}

	ceoSpawn := sendRunnerManagement(t, hub, "boss", protocol.MsgTaskRunnerSpawn, protocol.TaskRunnerSpawnRequest{
		TaskID:     task.ID,
		ParentNick: "other",
	})
	if ceoSpawn.Runner == nil || ceoSpawn.Runner.ParentNick != "other" {
		t.Fatalf("ceo parent override = %+v, want parent other", ceoSpawn.Runner)
	}
}

func newRunnerManagementFixture(t *testing.T) (*db.DB, *Hub, protocol.Task) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "artificial.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := database.CreateEmployee("manager", "worker", "manager", ""); err != nil {
		t.Fatalf("create manager: %v", err)
	}
	if _, err := database.CreateEmployee("other", "worker", "other", ""); err != nil {
		t.Fatalf("create other: %v", err)
	}
	if _, err := database.CreateEmployee("boss", "ceo", "boss", ""); err != nil {
		t.Fatalf("create boss: %v", err)
	}
	project, err := database.CreateProject("test", t.TempDir(), "")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	task, err := database.CreateTask("runner task", "do work", "manager", project.ID, "commander")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	hub := NewHub(database, 0)
	spawnCount := 0
	hub.spawnRunnerForTask = func(taskID int64, parentNick string) (protocol.TaskRunner, error) {
		if _, err := database.GetTask(taskID); err != nil {
			return protocol.TaskRunner{}, fmt.Errorf("task not found: %w", err)
		}
		if existing, err := database.GetActiveRunnerForTask(taskID); err == nil {
			return protocol.TaskRunner{}, fmt.Errorf("task already has active runner %q (id=%d, status=%s)", existing.Nickname, existing.ID, existing.Status)
		}
		spawnCount++
		return database.CreateTaskRunner(
			taskID,
			fmt.Sprintf("runner-test-%d", spawnCount),
			parentNick,
			"/tmp/worktree",
			fmt.Sprintf("runner/test-%d", spawnCount),
			"master",
			"",
		)
	}
	return database, hub, task
}

func sendRunnerManagement(t *testing.T, hub *Hub, nick, typ string, payload any) protocol.TaskRunnerManageResponse {
	t.Helper()
	resp := sendRunnerManagementAllowError(t, hub, nick, typ, payload)
	if resp.Error != "" {
		t.Fatalf("%s returned error: %s", typ, resp.Error)
	}
	return resp
}

func sendRunnerManagementAllowError(t *testing.T, hub *Hub, nick, typ string, payload any) protocol.TaskRunnerManageResponse {
	t.Helper()
	var replies []protocol.WSMessage
	hub.sendHook = func(to string, msg protocol.WSMessage) bool {
		if to == nick {
			replies = append(replies, msg)
		}
		return true
	}
	defer func() { hub.sendHook = nil }()

	data, _ := json.Marshal(payload)
	hub.handleMessage(context.Background(), &client{nick: nick}, protocol.WSMessage{
		Type:      typ,
		RequestID: "test-request",
		Data:      data,
	})
	if len(replies) != 1 {
		t.Fatalf("got %d replies, want 1", len(replies))
	}
	if replies[0].RequestID != "test-request" {
		t.Fatalf("reply request id = %q, want test-request", replies[0].RequestID)
	}
	var resp protocol.TaskRunnerManageResponse
	if err := json.Unmarshal(replies[0].Data, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}
