package mcpserver

import (
	"context"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRenderTaskDescription_FreeformOnly(t *testing.T) {
	in := taskCreateInput{Description: "do the thing"}
	got := renderTaskDescription(in)
	if got != "do the thing" {
		t.Fatalf("freeform-only: want passthrough, got %q", got)
	}
}

func TestRenderTaskDescription_Empty(t *testing.T) {
	if got := renderTaskDescription(taskCreateInput{}); got != "" {
		t.Fatalf("empty input: want empty string, got %q", got)
	}
}

func TestRenderTaskDescription_StructuredAllFields(t *testing.T) {
	in := taskCreateInput{
		Title:              "Add jitter",
		Goal:               "Hide login-shed via timing",
		Context:            "Shed branch returns instantly while real path takes 50-200ms.",
		Files:              []string{"src/svc-gapi/service/r_auth.go", "src/svc-gapi/service/s_loginshed.go"},
		AcceptanceCriteria: []string{"go build passes", "shed branch sleeps 80-220ms"},
		Constraints:        []string{"no new deps", "do not change response body"},
		Description:        "extra notes",
	}
	got := renderTaskDescription(in)

	mustContain := []string{
		"## Goal\nHide login-shed via timing",
		"## Context\nShed branch returns instantly",
		"## Files in scope",
		"`src/svc-gapi/service/r_auth.go`",
		"## Acceptance criteria",
		"- [ ] go build passes",
		"- [ ] shed branch sleeps 80-220ms",
		"## Constraints",
		"- no new deps",
		"## Notes\nextra notes",
	}
	for _, frag := range mustContain {
		if !strings.Contains(got, frag) {
			t.Errorf("missing fragment %q in:\n%s", frag, got)
		}
	}

	// Section ordering: Goal must come before Context, Context before
	// Files, Files before Acceptance, Acceptance before Constraints,
	// Constraints before Notes. The runner reads this top-to-bottom and
	// the persona instructs it to verify Acceptance before declaring
	// done — out-of-order rendering would silently break that contract.
	want := []string{"## Goal", "## Context", "## Files in scope", "## Acceptance criteria", "## Constraints", "## Notes"}
	last := -1
	for _, s := range want {
		i := strings.Index(got, s)
		if i < 0 {
			t.Fatalf("section %q missing from output", s)
		}
		if i <= last {
			t.Fatalf("section %q at offset %d appears before previous section (offset %d)", s, i, last)
		}
		last = i
	}

	if strings.HasSuffix(got, "\n") {
		t.Errorf("output should not end with trailing newline; got %q", got[len(got)-3:])
	}
}

func TestRenderTaskDescription_AcceptanceOnly(t *testing.T) {
	// Common shape: manager fills only the contract bullets, no prose.
	in := taskCreateInput{AcceptanceCriteria: []string{"only criterion"}}
	got := renderTaskDescription(in)

	if !strings.Contains(got, "## Acceptance criteria") {
		t.Errorf("missing Acceptance heading in:\n%s", got)
	}
	if !strings.Contains(got, "- [ ] only criterion") {
		t.Errorf("missing acceptance bullet in:\n%s", got)
	}
	if strings.Contains(got, "## Goal") || strings.Contains(got, "## Context") || strings.Contains(got, "## Constraints") || strings.Contains(got, "## Notes") {
		t.Errorf("unexpected empty-section heading rendered:\n%s", got)
	}
}

func TestProjectAssignmentToolExposureIsCEOOnly(t *testing.T) {
	ceoTools := listToolNames(t, New("test-ceo", "ceo", 0, nil, ""))
	if !hasTool(ceoTools, "project_assign_employees") {
		t.Fatal("CEO tools did not include project_assign_employees")
	}

	workerTools := listToolNames(t, New("test-worker", "worker", 0, nil, ""))
	if hasTool(workerTools, "project_assign_employees") {
		t.Fatal("worker tools unexpectedly included project_assign_employees")
	}
}

func TestLongLivedWorkerRegistersRunnerManagementTools(t *testing.T) {
	srv := New("manager", "worker", 1, nil, "")
	names := listToolNames(t, srv)

	for _, name := range []string{"runner_spawn", "runner_list", "runner_get", "runner_cancel"} {
		if !hasTool(names, name) {
			t.Fatalf("long-lived worker tools missing %q; got %v", name, names)
		}
	}
}

func TestRunnerModeDoesNotRegisterRunnerManagementTools(t *testing.T) {
	srv := NewRunner("runner-t1-test", nil, "", RunnerOptions{RunnerID: 1, TaskID: 1})
	names := listToolNames(t, srv)

	if !hasTool(names, "task_describe") {
		t.Fatalf("runner tool surface missing task_describe; got %v", names)
	}
	for _, name := range []string{"runner_spawn", "runner_list", "runner_get", "runner_cancel"} {
		if hasTool(names, name) {
			t.Fatalf("runner-mode MCP server exposed manager tool %q; got %v", name, names)
		}
	}
}

func listToolNames(t *testing.T, srv *Server) []string {
	t.Helper()
	ctx := context.Background()
	serverTransport, clientTransport := gomcp.NewInMemoryTransports()
	serverSession, err := srv.mcpServer.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect test server: %v", err)
	}
	defer serverSession.Close()
	client := gomcp.NewClient(&gomcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect test client: %v", err)
	}
	defer clientSession.Close()
	result, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func hasTool(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}
