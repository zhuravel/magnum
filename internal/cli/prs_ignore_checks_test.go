package cli

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// A third-party app's check that fails on every PR ("Entire Gates" on
// talkable/talkable, 2026-10-26) put a red CI on every row. Where the
// watch's ignore_checks names it, the board's CI and `magnum prs`'s (the
// row, its printed cell and its JSON) leave it out: a head whose only
// failed check is ignored reads passed, and the card names no failing job.
// A repository of another watch still counts it.
func TestTheBoardsCIIsNotRedWhenOnlyAnIgnoredCheckFailed(t *testing.T) {
	h := newActHarness(t)
	h.c.preset.Watches[0].IgnoreChecks = []string{"Vendor Gates"}
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

// The board's CI links each failed job to its page and says why GitHub
// blocks the merge, by the PR's review gate as magnum last read it: the
// card's CI lines draw both.
func TestTheBoardsCILinksTheFailedJobAndSaysWhyGitHubBlocksTheMerge(t *testing.T) {
	h := newActHarness(t)
	job := "https://github.com/example/talkable/actions/runs/7001/job/9001"
	pr := h.seedPR("talkable/talkable", 12511, store.PRReviewed)
	ci := store.CIStatus{SHA: pr.HeadSHA, State: "FAILURE", Total: 1, Complete: true, ByWorkflowFile: true, Merge: "BLOCKED",
		Checks: []store.CheckResult{{Name: "rspec", State: store.CheckFailed, Workflow: "CI", WorkflowFile: "ci.yml", At: h.now, URL: job}}}
	ci.Tally()
	if _, err := h.st.UpsertPRFromGitHub(h.ctx, store.GitHubPR{RepoID: pr.RepoID, NodeID: "PR_talkable/talkable_12511", Number: 12511, URL: pr.URL,
		HeadSHA: pr.HeadSHA, GHState: store.GHOpen, Identity: "talkable-app", CIState: new("FAILURE"), CI: &ci,
		ReviewGate: &store.ReviewGate{Decision: "REVIEW_REQUIRED", Complete: true}}); err != nil {
		t.Fatal(err)
	}
	got := boardRowsOf(t, h)[12511].CI
	if got == nil || len(got.Links) != 1 || got.Links[0] != (tui.CheckLink{Name: "CI / rspec", URL: job}) {
		t.Fatalf("the board's CI links %+v, want CI / rspec's job", got)
	}
	if got.Blocked != "GitHub blocks the merge: a required approval is missing" {
		t.Errorf("the board's CI says %q of GitHub's block", got.Blocked)
	}
	if j := prsJSONCIOf(*got); len(j.Links) != 1 || j.Links[0] != (prsJSONLink{Name: "CI / rspec", URL: job}) || j.Blocked != got.Blocked {
		t.Errorf("magnum prs --json's ci = %+v, want the link and the block", j)
	}
}
