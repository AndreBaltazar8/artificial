package main

import (
	"strings"
	"testing"

	"artificial.pt/pkg-go-shared/protocol"
)

func testRunnerConfig() protocol.RunnerConfig {
	return protocol.RunnerConfig{
		Runner: protocol.TaskRunner{
			ID:           7,
			TaskID:       346,
			Nickname:     "runner-t346-test",
			ParentNick:   "manager",
			WorktreePath: "/tmp/artificial/worktrees/t346-test",
			BranchName:   "runner/t346-test",
			BaseBranch:   "main",
		},
		Task: protocol.Task{
			ID:        346,
			Title:     "Include full task details in runner initial prompt",
			Status:    "in_progress",
			Assignee:  "runner-t346-test",
			ProjectID: 12,
			CreatedBy: "commander",
			Description: `## Goal
Start runners with enough context to begin immediately.

## Context
The first user prompt should replace the mandatory describe round-trip.

## Files in scope
- src/cmd-worker/cmd/worker/runner.go
- src/cmd-worker/internal/mcpserver/runner.go

## Acceptance criteria
- [ ] Initial prompt includes the complete task details
- [ ] task_describe remains available as an optional recovery tool

## Constraints
- Keep prompt construction centralized`,
		},
		Project: &protocol.Project{
			ID:        12,
			Name:      "Artificial",
			Path:      "/repo/artificial",
			GitRemote: "git@example.com:artificial.git",
		},
		Harness: "claude",
		Model:   "sonnet",
	}
}

func TestBuildRunnerInitialPromptIncludesAssignmentDetails(t *testing.T) {
	cfg := testRunnerConfig()
	assignment := buildRunnerAssignmentBrief(cfg)
	prompt := buildRunnerInitialPrompt(cfg, assignment)

	mustContain := []string{
		"runner-t346-test",
		"Task #346: Include full task details in runner initial prompt",
		"## Project context",
		"ID: 12",
		"Name: Artificial",
		"Path: /repo/artificial",
		"Git remote: git@example.com:artificial.git",
		"Worktree: /tmp/artificial/worktrees/t346-test",
		"Branch: runner/t346-test",
		"Base branch: main",
		"## Task record",
		"Status: in_progress",
		"Created by: commander",
		"--- Description ---",
		"## Goal",
		"## Context",
		"## Files in scope",
		"src/cmd-worker/cmd/worker/runner.go",
		"## Acceptance criteria",
		"- [ ] Initial prompt includes the complete task details",
		"## Constraints",
		"Keep prompt construction centralized",
	}
	for _, frag := range mustContain {
		if !strings.Contains(prompt, frag) {
			t.Errorf("initial prompt missing %q in:\n%s", frag, prompt)
		}
	}

	forbidden := forbiddenTaskDescribeDirectives()
	lower := strings.ToLower(prompt)
	for _, frag := range forbidden {
		if strings.Contains(lower, frag) {
			t.Errorf("initial prompt still requires first-step task_describe via %q:\n%s", frag, prompt)
		}
	}
}

func TestBuildRunnerPersonaDoesNotRequireDescribeFirst(t *testing.T) {
	persona := buildRunnerPersona(testRunnerConfig())
	lower := strings.ToLower(persona)

	forbidden := forbiddenTaskDescribeDirectives()
	for _, frag := range forbidden {
		if strings.Contains(lower, frag) {
			t.Errorf("persona still requires first-step task_describe via %q:\n%s", frag, persona)
		}
	}
	if !strings.Contains(persona, "optionally re-read the assignment") {
		t.Fatalf("persona should keep task_describe as an optional reread tool:\n%s", persona)
	}
}

func forbiddenTaskDescribeDirectives() []string {
	return []string{
		"call " + "task_describe " + "first",
		"call " + "task_describe " + "now",
		"task_describe " + "first",
	}
}
