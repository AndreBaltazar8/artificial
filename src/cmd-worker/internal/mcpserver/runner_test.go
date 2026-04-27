package mcpserver

import (
	"strings"
	"testing"
)

func TestRunnerAssignmentTextUsesProvidedBrief(t *testing.T) {
	opts := RunnerOptions{
		TaskID:          123,
		TaskTitle:       "fallback title",
		TaskDesc:        "fallback description",
		WorktreePath:    "/tmp/worktree",
		BranchName:      "runner/test",
		AssignmentBrief: "rendered assignment with project context and acceptance criteria",
	}

	got := runnerAssignmentText(opts)
	if got != opts.AssignmentBrief {
		t.Fatalf("runnerAssignmentText should reuse provided brief, got %q", got)
	}
}

func TestRunnerAssignmentTextFallback(t *testing.T) {
	opts := RunnerOptions{
		TaskID:       123,
		TaskTitle:    "fallback title",
		TaskDesc:     "fallback description",
		WorktreePath: "/tmp/worktree",
		BranchName:   "runner/test",
	}

	got := runnerAssignmentText(opts)
	mustContain := []string{
		"Task #123: fallback title",
		"Worktree: /tmp/worktree",
		"Branch: runner/test",
		"--- Description ---",
		"fallback description",
	}
	for _, frag := range mustContain {
		if !strings.Contains(got, frag) {
			t.Errorf("fallback assignment missing %q in:\n%s", frag, got)
		}
	}
}
