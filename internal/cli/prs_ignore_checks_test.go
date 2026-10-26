package cli

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// A third-party app's check that fails on every PR ("Entire Gates" on
// talkable/talkable, 2026-10-26) put a red CI on every row. Where the
// watch's ignore_checks names it, the board's CI and `magnum prs`'s (the
// row, its printed cell and its JSON) leave it out: a head whose only
// failed check is ignored reads passed, and the card names no failing job.
// A repository of another watch still counts it.
func TestTheBoardsCIIsNotRedWhenOnlyAnIgnoredCheckFailed(t *testing.T) {
	h := newActHarness(t)
	h.c.Config.Watches[0].IgnoreChecks = []string{"Vendor Gates"}
	at := h.now.Add(-time.Hour)
	checks := []store.CheckResult{
		{Name: "Vendor Gates", State: store.CheckFailed, At: at},
		{Name: "rspec", State: store.CheckPassed, Workflow: "CI", WorkflowFile: "ci.yml", At: at},
	}
	setCI := func(full string, n int) {
		t.Helper()
		pr := h.seedPR(full, n, store.PRReviewed)
		ci := store.CIStatus{SHA: pr.HeadSHA, State: "FAILURE", Total: 2, Complete: true, ByWorkflowFile: true, Checks: checks}
		ci.Tally()
		if _, err := h.st.UpsertPRFromGitHub(h.ctx, store.GitHubPR{RepoID: pr.RepoID, NodeID: fmt.Sprintf("PR_%s_%d", full, n), Number: n,
			URL: pr.URL, HeadSHA: pr.HeadSHA, GHState: store.GHOpen, Identity: "talkable-app", CIState: new("FAILURE"), CI: &ci}); err != nil {
			t.Fatal(err)
		}
	}
	setCI("talkable/talkable", 12510)
	setCI("zhuravel/dotfiles", 7)

	rows := boardRowsOf(t, h)
	ci := rows[12510].CI
	if ci == nil || ci.State != "passed" || ci.Failed != 0 || ci.Total != 1 || len(ci.Failing) != 0 {
		t.Fatalf("the board's CI = %+v, want passed without the ignored check", ci)
	}
	for _, w := range ci.Workflows {
		if w.State == store.CheckFailed {
			t.Fatalf("a workflow reads failed: %+v", ci.Workflows)
		}
	}
	if cell := prsCICell(ci); cell != "passed 1/1" {
		t.Fatalf("magnum prs's CI cell = %q, want passed 1/1", cell)
	}
	if j := prsJSONCIOf(*ci); j.State != "passed" || j.Failed != 0 || len(j.Failing) != 0 {
		t.Fatalf("magnum prs --json's ci = %+v", j)
	}
	if line := boardLine(t, rows, 12510); strings.Contains(line, "failed") || strings.Contains(line, "✗") {
		t.Fatalf("the board's line reads red: %q", line)
	}

	other := rows[7].CI
	if other == nil || other.State != "failed" || other.Failed != 1 || len(other.Failing) != 1 || other.Failing[0] != "Vendor Gates" {
		t.Fatalf("another watch's repository: CI = %+v, want the check counted", other)
	}
}
