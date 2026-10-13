package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// ciBefore is when the checks below finished: before every review, so no
// CI follow-up (ci_red.go) mixes in.
var ciBefore = time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)

// auditCheck and specsCheck are talkable#11483's checks: Dependency Audit, red on
// the PR's heads and on master alike, and the PR's own specs.
func auditCheck(state string) github.Check {
	return github.Check{Name: "audit", State: state, Workflow: "Dependency Audit", At: ciBefore}
}

func specsCheck(state string) github.Check {
	return github.Check{Name: "rspec", State: state, Workflow: "CI", At: ciBefore}
}

// withBaseChecks makes the fake GitHub's base tip (fakeBaseOid, every PR's)
// report checks.
func withBaseChecks(checks ...github.Check) func(*harness) {
	return func(h *harness) { h.gh.commitChecks = map[string][]github.Check{fakeBaseOid: checks} }
}

const (
	ciFailsWords = "its head's checks fail (magnum approves it once they pass)"
	ciRunsWords  = "its head's checks have not finished (magnum approves it once they pass)"
	ciBaseEvent  = "review.auto_approve_ci_base"
)

// On talkable#11483 Dependency Audit was red for days on the PR's heads and
// on master alike, and every clean review was held back with "its head's
// checks fail". A check that fails the same way on the base branch's tip
// (the same workflow and name failed there) no longer holds the approval:
// the round's end approves, a review.auto_approve_ci_base event says so
// once, and the base tip's checks were read once.
func TestAFailingCheckThatFailsOnTheBaseTooDoesNotHoldTheApproval(t *testing.T) {
	h, _ := newAutoHarness(t, withBaseChecks(auditCheck(github.CheckFailed), specsCheck(github.CheckPassed)))
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", ci: "FAILURE",
		checks: []github.Check{auditCheck(github.CheckFailed), specsCheck(github.CheckPassed)}})
	h.tick()
	h.reviewTo(2, "b1")
	h.flush()
	h.flush()
	if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:") {
		t.Fatalf("posted = %q, want the approval of b1", got)
	}
	if got := h.autoRefused(2); got != "" {
		t.Fatalf("held back for %q", got)
	}
	evs := h.events(ciBaseEvent)
	want := "talkable/talkable#2: its head's 1 failing check fails on master's tip base-ti too; the approval goes ahead"
	if len(evs) != 1 || evs[0].Message != want || !h.hasEvent("pr:talkable/talkable#2", ciBaseEvent) {
		t.Fatalf("%s events = %+v, want one %q", ciBaseEvent, evs, want)
	}
	if n := len(h.ghCalls("commit_checks:talkable/talkable@" + fakeBaseOid)); n != 1 {
		t.Fatalf("read the base tip's checks %d times, want once", n)
	}
}

// A failing check the base tip does not fail the same way still holds the
// approval with the reason it had: one that passes there, one the base has
// under another workflow only, a second failing check beside one the base
// fails too, a head without a known base tip and a base read that fails
// (whose error stays out of the reason). One that fails like the base while
// another check still runs waits for it.
func TestAFailingCheckTheBaseDoesNotFailTheSameWayHoldsTheApproval(t *testing.T) {
	failed, passed, pending := github.CheckFailed, github.CheckPassed, github.CheckPending
	for _, tc := range []struct {
		name    string
		head    []github.Check
		base    []github.Check
		noBase  bool
		baseErr error
		want    string
	}{
		{name: "the base passes it", head: []github.Check{auditCheck(failed), specsCheck(passed)}, base: []github.Check{auditCheck(passed)}, want: ciFailsWords},
		{name: "the base fails it in another workflow", head: []github.Check{auditCheck(failed)},
			base: []github.Check{{Name: "audit", State: failed, Workflow: "Nightly", At: ciBefore}}, want: ciFailsWords},
		{name: "a second failing check passes on the base", head: []github.Check{auditCheck(failed), specsCheck(failed)},
			base: []github.Check{auditCheck(failed), specsCheck(passed)}, want: ciFailsWords},
		{name: "another check still runs", head: []github.Check{auditCheck(failed), specsCheck(pending)}, base: []github.Check{auditCheck(failed)},
			want: ciRunsWords},
		{name: "no base tip", head: []github.Check{auditCheck(failed)}, base: []github.Check{auditCheck(failed)}, noBase: true, want: ciFailsWords},
		{name: "the base read fails", head: []github.Check{auditCheck(failed)},
			baseErr: &github.APIError{Op: "commit checks", Status: 502, Message: "Bad Gateway"}, want: ciFailsWords},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newAutoHarness(t, withBaseChecks(tc.base...), func(h *harness) { h.gh.commitChecksErr = tc.baseErr })
			h.open(prSpec{n: 1, head: "base1"})
			h.startup()
			h.tick()
			h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", ci: "FAILURE", checks: tc.head, noBaseOid: tc.noBase})
			h.tick()
			h.reviewTo(2, "b1")
			h.flush()
			h.wantRefused(2, tc.want)
			if strings.Contains(h.autoRefused(2), "502") || strings.Contains(h.autoRefused(2), "Gateway") {
				t.Fatalf("the reason carries the read's error: %q", h.autoRefused(2))
			}
			if evs := h.events(ciBaseEvent); len(evs) != 0 {
				t.Fatalf("%s events = %+v", ciBaseEvent, evs)
			}
		})
	}
}

// The gates run on every tick while something else holds the post (here
// the operator's review in progress on GitHub): the event is recorded once
// per head and round, and the base tip's checks are read once per ten
// minutes, not on every tick.
func TestTheBaseTipsChecksAreReadOnceAndTheEventRecordedOncePerRound(t *testing.T) {
	h, _ := newAutoHarness(t, withBaseChecks(auditCheck(github.CheckFailed)))
	h.gh.addReview(2, github.Review{DatabaseID: 7, State: "PENDING", AuthorLogin: "zhuravel", AuthorType: "User"})
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", ci: "FAILURE", checks: []github.Check{auditCheck(github.CheckFailed)}})
	h.tick()
	h.reviewTo(2, "b1")
	reads := len(h.ghCalls("commit_checks:"))
	h.flush()
	h.flush()
	h.flush()
	if got := h.created(); len(got) != 0 {
		t.Fatalf("posted = %q while the operator's review is in progress", got)
	}
	if n := len(h.ghCalls("commit_checks:")); reads != 1 || n != 1 {
		t.Fatalf("read the base tip's checks %d, then %d times, want once", reads, n)
	}
	h.advance(10 * time.Minute)
	h.tick()
	if n := len(h.ghCalls("commit_checks:")); n != 2 {
		t.Fatalf("read the base tip's checks %d times, want again after ten minutes", n)
	}
	if evs := h.events(ciBaseEvent); len(evs) != 1 {
		t.Fatalf("%s events = %+v, want one for the head and round", ciBaseEvent, evs)
	}
}

// Two PRs on the same base tip decided in one tick read its checks once.
func TestPRsOnTheSameBaseTipReadItsChecksOnce(t *testing.T) {
	h, _ := newAutoHarness(t, withBaseChecks(auditCheck(github.CheckFailed)))
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	running := []github.Check{auditCheck(github.CheckFailed), specsCheck(github.CheckPending)}
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", ci: "PENDING", checks: running},
		prSpec{n: 3, head: "c1", ci: "PENDING", checks: running})
	h.tick()
	h.reviewTo(2, "b1")
	h.reviewTo(3, "c1")
	if n := len(h.ghCalls("commit_checks:")); n != 0 || len(h.created()) != 0 {
		t.Fatalf("read the base tip %d times, posted %q while the rollups ran", n, h.created())
	}
	h.setCI(2, "FAILURE", auditCheck(github.CheckFailed), specsCheck(github.CheckPassed))
	h.setCI(3, "FAILURE", auditCheck(github.CheckFailed), specsCheck(github.CheckPassed))
	h.advance(time.Minute)
	h.tick()
	if got := h.created(); len(got) != 2 || !strings.HasPrefix(got[0], "APPROVE@b1:") || !strings.HasPrefix(got[1], "APPROVE@c1:") {
		t.Fatalf("posted = %q, want b1's and c1's approvals", got)
	}
	if n := len(h.ghCalls("commit_checks:")); n != 1 {
		t.Fatalf("read the base tip's checks %d times, want once for both", n)
	}
}

// A standing approval of b1 stays after magnum's clean review of b2 while
// b2's one failing check fails on the base tip too: it waits while another
// check runs, then the gates let the round through as they would a new
// approval.
func TestAStandingApprovalStaysWhenTheNewHeadFailsOnlyLikeTheBase(t *testing.T) {
	h := approvedThenReviewedOn(t, prSpec{n: 2, head: "b2", ci: "FAILURE",
		checks: []github.Check{auditCheck(github.CheckFailed), specsCheck(github.CheckPending)}}, cleanRound, withBaseChecks(auditCheck(github.CheckFailed)))
	h.flush()
	if d := h.ghCalls("dismiss:"); len(d) != 0 {
		t.Fatalf("withdrawn while a check runs: %q", d)
	}
	h.setCI(2, "FAILURE", auditCheck(github.CheckFailed), specsCheck(github.CheckPassed))
	h.flush()
	h.flush()
	if d := h.ghCalls("dismiss:"); len(d) != 0 {
		t.Fatalf("withdrawn for a check that fails on the base too: %q", d)
	}
	if a := h.autoApproval(2); a.State != store.AutoStanding || a.HeadSHA != "b1" {
		t.Fatalf("approval = %+v", a)
	}
	if evs := h.events(ciBaseEvent); len(evs) != 1 || !strings.Contains(evs[0].Message, "#2: its head's 1 failing check fails on master's tip") {
		t.Fatalf("%s events = %+v", ciBaseEvent, evs)
	}
}
