package mcpserver

import (
	"strings"
	"testing"
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
