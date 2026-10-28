package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// workflowsWords is the card's words for a PR that changes its own
// workflows or actions.
const workflowsWords = "the PR changes its own workflows or actions: approve by hand"

// On talkable/talkable required_checks = ["workflow:CI"] decides the
// automatic approval's CI gate and GitHub enforces no checks, so a PR that
// adds a workflow, edits the CI workflow or a local action its jobs use
// would decide that gate itself. A PR whose head's file list names anything
// under .github/workflows/ or .github/actions/ (in any case) is never
// approved as the operator: the card and one review.auto_approve_refused
// event say so, and A as the operator still approves it (they decide by
// hand). A list magnum cannot read whole holds the PR too and says it
// cannot tell. A path that only looks alike (a nested .github/, a
// workflows-old directory, CODEOWNERS) is approved as before.
func TestAPRThatChangesItsWorkflowsIsNotAutoApproved(t *testing.T) {
	passed := github.CheckPassed
	checks := []github.Check{
		{Name: "rspec", State: passed, Workflow: "CI", WorkflowFile: "ci.yml", At: ciBefore},
		{Name: "Completion", State: passed, Workflow: "CI", WorkflowFile: "ci.yml", At: ciBefore},
	}
	for _, tc := range []struct {
		name      string
		files     []string
		truncated bool
		want      string // "" = approved
	}{
		{name: "a workflow it adds", files: []string{"app/models/order.rb", ".github/workflows/ci-fast.yml"}, want: workflowsWords},
		{name: "the CI workflow it edits", files: []string{".github/workflows/ci.yml"}, want: workflowsWords},
		{name: "a local action", files: []string{".github/actions/setup/action.yml"}, want: workflowsWords},
		{name: "in another case", files: []string{".GitHub/Workflows/CI.YML"}, want: workflowsWords},
		{name: "a cut file list", files: []string{"app/models/order.rb"}, truncated: true,
			want: "Magnum cannot tell whether it changes the review agents' instructions or hooks, or its own workflows or actions"},
		{name: "a nested .github", files: []string{"vendor/gem/.github/workflows/ci.yml"}},
		{name: "a lookalike directory", files: []string{".github/workflows-old/ci.yml", ".github/CODEOWNERS"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newAutoHarness(t, withRequiredChecks("workflow:CI"))
			h.open(prSpec{n: 1, head: "base1"})
			h.startup()
			h.tick()
			h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", ci: "SUCCESS", checks: checks, files: tc.files, filesTruncated: tc.truncated})
			h.tick()
			h.reviewTo(2, "b1")
			h.flush()
			if tc.want == "" {
				if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:") {
					t.Fatalf("posted = %q, want the approval of b1 (held back for %q)", got, h.autoRefused(2))
				}
				return
			}
			h.wantRefused(2, tc.want)

			if r := h.verdictAs(ReqApprove, 2, "zhuravel"); r.State != store.RequestDone {
				t.Fatalf("A as the operator: %s %q", r.State, deref(r.Result))
			}
			if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:") {
				t.Fatalf("posted = %q", got)
			}
		})
	}
}

// A standing approval of b1 is withdrawn when magnum's clean review of b2
// finds that b2 changes a workflow, as for any gate a new approval runs.
func TestAStandingApprovalIsWithdrawnWhenTheNewHeadChangesAWorkflow(t *testing.T) {
	h := approvedThenReviewedOn(t, prSpec{n: 2, head: "b2", files: []string{"app/models/order.rb", ".github/workflows/ci.yml"}}, cleanRound)
	h.wantGateWithdrawal(withdrawnWorkflows)
}

// Since spec 84 the stored checks kept one run per workflow name and check
// name, the newest, so on talkable/talkable (required_checks =
// ["workflow:CI"]) a workflow file the PR adds that declares `name: CI`, and
// whose rspec passes after the base's CI failed it, replaced the failure.
// Runs are kept by their workflow's file: the base's ci.yml failing rspec
// holds the approval whatever the PR's own ci-pr.yml says, in either order
// GitHub lists them. Nor does a failure of the PR's own CI pass for the
// base tip's failure of the base's (the comparison is by file too). The
// PR's file list names no workflow here, so the CI gate decides alone: the
// changed-files gate (TestAPRThatChangesItsWorkflowsIsNotAutoApproved) is
// the first guard, this the second.
func TestASameNamedWorkflowAddedByThePRDoesNotPassTheGate(t *testing.T) {
	failed, passed := github.CheckFailed, github.CheckPassed
	base := github.Check{Name: "rspec", State: failed, Workflow: "CI", WorkflowFile: "ci.yml", At: ciBefore}
	own := github.Check{Name: "rspec", State: passed, Workflow: "CI", WorkflowFile: "ci-pr.yml", At: ciBefore.Add(10 * time.Minute)}
	completion := github.Check{Name: "Completion", State: passed, Workflow: "CI", WorkflowFile: "ci.yml", At: ciBefore}
	basePasses := github.Check{Name: "rspec", State: passed, Workflow: "CI", WorkflowFile: "ci.yml", At: ciBefore}
	ownFails := github.Check{Name: "rspec", State: failed, Workflow: "CI", WorkflowFile: "ci-pr.yml", At: ciBefore}
	for _, tc := range []struct {
		name   string
		checks []github.Check
		tip    []github.Check // the base tip's
	}{
		{"the base's run listed first", []github.Check{base, completion, own}, []github.Check{basePasses, completion}},
		{"the PR's run listed first", []github.Check{own, completion, base}, []github.Check{basePasses, completion}},
		{"the PR's own CI fails as the base tip's does", []github.Check{basePasses, completion, ownFails}, []github.Check{base, completion}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newAutoHarness(t, withRequiredChecks("workflow:CI"), withBaseChecks(tc.tip...))
			reviewedWithChecks(h, "FAILURE", tc.checks...)
			h.wantRefused(2, ciFailsWords)
			if got := h.autoRefused(2); got != ciFailsWords {
				t.Fatalf("held back for %q, want exactly %q", got, ciFailsWords)
			}
		})
	}
}

// A name-glob entry of required_checks counted the newest run of each
// matching name, a commit status included, so a status anyone who can
// write to the repository posts as `rspec` after the CI's rspec failed
// passed the gate. A status and a check run are kept apart: a failing
// check run holds the approval whichever is newer (one of the CI's, or of
// another app, which has no workflow), and so does a failing status beside
// a passing run.
func TestAStatusNeverReplacesACheckRun(t *testing.T) {
	failed, passed := github.CheckFailed, github.CheckPassed
	later := ciBefore.Add(10 * time.Minute)
	for _, tc := range []struct {
		name     string
		required string
		checks   []github.Check
	}{
		{name: "a passing status after a failing run", required: "rspec", checks: []github.Check{
			{Name: "rspec", State: failed, Workflow: "CI", WorkflowFile: "ci.yml", At: ciBefore},
			{Name: "rspec", State: passed, Status: true, At: later}}},
		{name: "a passing status after another app's failing run", required: "danger", checks: []github.Check{
			{Name: "danger", State: failed, At: ciBefore},
			{Name: "danger", State: passed, Status: true, At: later}}},
		{name: "a failing status before a passing run", required: "rspec", checks: []github.Check{
			{Name: "rspec", State: failed, Status: true, At: ciBefore},
			{Name: "rspec", State: passed, Workflow: "CI", WorkflowFile: "ci.yml", At: later}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newAutoHarness(t, withRequiredChecks(tc.required))
			reviewedWithChecks(h, "FAILURE", tc.checks...)
			h.wantRefused(2, ciFailsWords)
		})
	}
}

// Checks an earlier build stored kept one run per workflow name and name,
// a status merged with another app's run: the poll reads them again (one
// Details read, whatever the radar says), and until then the gate does not
// decide by required_checks but by GitHub's rollup.
func TestChecksAnEarlierBuildStoredAreReadAgain(t *testing.T) {
	h, _ := newAutoHarness(t, withRequiredChecks("workflow:CI"))
	pass := []github.Check{{Name: "rspec", State: github.CheckPassed, Workflow: "CI", WorkflowFile: "ci.yml", At: ciBefore}}
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", ci: "SUCCESS", checks: pass})
	h.startup()
	h.tick()
	if ci := h.pr(2).CI; ci == nil || !ci.ByWorkflowFile || len(ci.Checks) != 1 || ci.Checks[0].WorkflowFile != "ci.yml" {
		t.Fatalf("stored checks = %+v", ci)
	}
	if _, err := h.st.DB().ExecContext(h.ctx, `UPDATE prs SET ci_json = json_remove(ci_json, '$.by_workflow_file') WHERE id = ?`, h.pr(2).ID); err != nil {
		t.Fatal(err)
	}
	if ci := h.pr(2).CI; ci == nil || ci.ByWorkflowFile {
		t.Fatalf("an earlier build's checks = %+v", ci)
	}
	if _, ok := requiredCIOf(h.pr(2), []string{"workflow:CI"}); ok {
		t.Fatal("the gate decides by an earlier build's checks")
	}
	before := len(h.ghCalls("details:"))
	h.advance(time.Minute)
	h.tick()
	if n := len(h.ghCalls("details:")) - before; n != 1 {
		t.Fatalf("details reads = %d, want 1", n)
	}
	if ci := h.pr(2).CI; ci == nil || !ci.ByWorkflowFile {
		t.Fatalf("checks read again = %+v", ci)
	}
}
