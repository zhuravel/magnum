package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// withIgnoreChecks sets talkable/talkable's [[repo]] ignore_checks.
func withIgnoreChecks(checks ...string) func(*harness) {
	return func(h *harness) {
		h.cfg.Repos = append(h.cfg.Repos, config.Repo{Repo: "talkable/talkable", IgnoreChecks: checks})
	}
}

// vendorCheck is a third-party app's check that fails on every PR ("Entire
// Gates" on talkable/talkable since 2026-10-26): another app's check run,
// no workflow.
func vendorCheck(state string) github.Check {
	return github.Check{Name: "Vendor Gates", State: state, At: ciBefore}
}

// On talkable#12107 the CI-red follow-up queued a judge-only round for a
// check that fails on every PR, and its ci_followup record named only that
// check. A check ignore_checks names that fails after the review queues no
// judge check and stays out of the record; a check it does not name that
// fails beside it still queues one, and the record names that one alone.
func TestAnIgnoredCheckThatFailsAfterTheReviewQueuesNoJudgeCheck(t *testing.T) {
	h := newHarness(t, withIgnoreChecks("Vendor Gates"))
	ciReviewedPR(h)
	h.advance(10 * time.Minute)
	redCI(h, github.Check{Name: "Vendor Gates"})
	for range 3 {
		h.tick()
		h.advance(time.Minute)
	}
	h.wantState(2, store.PRReviewed)
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d: an ignored check that failed after the review started a judge check", n)
	}
	if evs := approvalEvents(t, h, 2, "pr.ci_red"); len(evs) != 0 {
		t.Fatalf("pr.ci_red events for an ignored check: %+v", evs)
	}
	if rec, ok := ciFollowupOfPR(t, h); ok {
		t.Fatalf("a follow-up record for an ignored check: %+v", rec)
	}

	// RSpec fails too (the rollup's counts move, so the poll reads the
	// checks again): its judge check follows, and the record names it alone.
	h.advance(10 * time.Minute)
	checks := redCI(h, github.Check{Name: "Vendor Gates"}, rspec)
	h.tick()
	ins := h.rd.all()
	if len(ins) != 2 || !ins[1].CIRed || !ins[1].SameHead {
		t.Fatalf("rounds = %d, want the judge check of RSpec", len(ins))
	}
	rec, ok := ciFollowupOfPR(t, h)
	if !ok || len(rec.Checks) != 1 || rec.Checks[0] != (ciFailure{Workflow: "RSpec", Name: "rspec", At: checks[2].At}) {
		t.Fatalf("follow-up record = %+v (%v), want RSpec alone", rec, ok)
	}
	if evs := approvalEvents(t, h, 2, "pr.ci_red"); len(evs) != 1 || !strings.Contains(evs[0].Message, "(1 failed check, at ") {
		t.Fatalf("pr.ci_red events = %+v, want one counting RSpec alone", evs)
	}
}

// talkable/talkable has no required_checks, so the automatic approval
// counted every check and waited for one that never passes. A head whose
// only failed check ignore_checks names, its rollup FAILURE and its checks
// complete, is approved as one whose checks pass: no refusal, and the base
// tip is never read for it. While another check still runs it waits, as
// before; another check that fails on the base tip too (Dependency Audit's
// case, beside an ignored check that never runs on the base branch) goes
// through on the base-match rule, the ignored check never compared; a
// standing approval stays after a clean review of a new head whose only
// failure is ignored, where without the key it is withdrawn.
func TestTheAutomaticApprovalPassesAHeadWhoseOnlyFailedCheckIsIgnored(t *testing.T) {
	failed, passed, pending := github.CheckFailed, github.CheckPassed, github.CheckPending
	t.Run("the rest passed", func(t *testing.T) {
		h, _ := newAutoHarness(t, withIgnoreChecks("Vendor Gates"))
		reviewedWithChecks(h, "FAILURE", vendorCheck(failed), specsCheck(passed))
		if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:") {
			t.Fatalf("posted = %q, want the approval of b1", got)
		}
		if r := h.autoRefused(2); r != "" {
			t.Fatalf("held back for %q", r)
		}
		if reads := h.ghCalls("commit_checks:"); len(reads) != 0 {
			t.Fatalf("the base tip was read for an ignored check: %q", reads)
		}
		if evs := h.events(ciBaseEvent); len(evs) != 0 {
			t.Fatalf("%s events = %+v", ciBaseEvent, evs)
		}
		for _, ev := range h.events("review.auto_approved") {
			if strings.Contains(ev.Message, "Vendor") || strings.Contains(ev.Message, "ignored") {
				t.Fatalf("review.auto_approved names the ignored check: %q", ev.Message)
			}
		}
	})
	t.Run("another check still runs", func(t *testing.T) {
		h, _ := newAutoHarness(t, withIgnoreChecks("Vendor Gates"))
		reviewedWithChecks(h, "FAILURE", vendorCheck(failed), specsCheck(pending))
		h.wantRefused(2, ciRunsWords)
	})
	t.Run("another check failed", func(t *testing.T) {
		h, _ := newAutoHarness(t, withIgnoreChecks("Vendor Gates"))
		reviewedWithChecks(h, "FAILURE", vendorCheck(failed), specsCheck(failed))
		h.wantRefused(2, ciFailsWords)
	})
	t.Run("another check fails on the base tip too", func(t *testing.T) {
		h, _ := newAutoHarness(t, withIgnoreChecks("Vendor Gates"), withBaseChecks(specsCheck(failed)))
		reviewedWithChecks(h, "FAILURE", vendorCheck(failed), specsCheck(failed))
		if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:") {
			t.Fatalf("posted = %q, want the approval of b1 (held back for %q)", got, h.autoRefused(2))
		}
		if evs := approvalEvents(t, h, 2, ciBaseEvent); len(evs) != 1 || eventData(t, evs[0])["failing"] != float64(1) {
			t.Fatalf("%s events = %+v, want one counting 1 failing check", ciBaseEvent, evs)
		}
	})
	next := prSpec{n: 2, head: "b2", ci: "FAILURE", checks: []github.Check{vendorCheck(failed), specsCheck(passed)}}
	t.Run("a standing approval stays", func(t *testing.T) {
		h := approvedThenReviewedOn(t, next, cleanRound, withIgnoreChecks("Vendor Gates"))
		h.flush()
		if d := h.ghCalls("dismiss:"); len(d) != 0 {
			t.Fatalf("withdrawn for an ignored check: %q", d)
		}
		if a := h.autoApproval(2); a.State != store.AutoStanding {
			t.Fatalf("approval = %+v", a)
		}
	})
	t.Run("without the key it is withdrawn", func(t *testing.T) {
		h := approvedThenReviewedOn(t, next, cleanRound)
		h.wantGateWithdrawal(withdrawnCIFails, ciFailsWords)
	})
}

// On talkable#12105 the judge posted its clean verdict as a comment because
// the head's only failed check was one that fails on every PR. The judge's
// approval gate decides by the same checks: a head whose only failed check
// is ignored is green and approves, one where another check runs lets the
// approval through as running checks do, one where another check fails
// still refuses, while a third check runs too (a remaining failure decides
// before a remaining check that runs), and one where another check fails
// on the base tip too is red like the base, the ignored check, which the
// base tip never ran, not compared. The base tip is read only for a check
// that counts and failed.
func TestTheJudgesApprovalGateApprovesAHeadWhoseOnlyFailedCheckIsIgnored(t *testing.T) {
	failed, passed, pending := github.CheckFailed, github.CheckPassed, github.CheckPending
	for _, tc := range []struct {
		name     string
		checks   []github.Check // beside the ignored check, which failed
		base     []github.Check // the base tip's
		refusal  string
		state    string
		baseRead bool
	}{
		{name: "the rest passed", checks: []github.Check{specsCheck(passed)}, state: pipeline.CIGreen},
		{name: "another check runs", checks: []github.Check{specsCheck(pending)}, state: pipeline.CIRunning},
		{name: "another check failed", checks: []github.Check{specsCheck(failed)}, refusal: ciFailsJudge, state: pipeline.CIRed, baseRead: true},
		{name: "another check failed while a third runs", checks: []github.Check{specsCheck(failed), ciCheck("CI", "lint", pending)},
			refusal: ciFailsJudge, state: pipeline.CIRed, baseRead: true},
		{name: "another check fails on the base tip too", checks: []github.Check{specsCheck(failed)}, base: []github.Check{specsCheck(failed)},
			state: pipeline.CIRedLikeBase, baseRead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := judgeApprovesHarness(t, withIgnoreChecks("Vendor Gates"), withBaseChecks(tc.base...))
			in := roundOf(t, h, prSpec{n: 2, head: "b1", ci: "FAILURE", checks: append([]github.Check{vendorCheck(failed)}, tc.checks...)}, false)
			if g := in.CIGate(h.ctx, h.pr(2), "b1"); g.Refusal != tc.refusal || g.State != tc.state {
				t.Fatalf("CI gate = %+v, want refusal %q, state %s", g, tc.refusal, tc.state)
			}
			refused := approvalEvents(t, h, 2, "round.approval_refused")
			if tc.refusal == "" && len(refused) != 0 {
				t.Fatalf("round.approval_refused events %+v", refused)
			}
			if tc.refusal != "" && (len(refused) != 1 || eventData(t, refused[0])["ci"] != tc.state) {
				t.Fatalf("round.approval_refused events %+v: want one, CI %s", refused, tc.state)
			}
			if read := h.gh.count("commit_checks:") != 0; read != tc.baseRead {
				t.Fatalf("base tip read: %v, want %v", read, tc.baseRead)
			}
		})
	}
}

// A repository that sets required_checks beside ignore_checks reads its
// required checks among the checks magnum sees: a failing check outside
// the list is still named in review.auto_approve_begin, review.auto_approved
// and the daemon's log while the ignored one never is; "*" requires every
// check but the ignored one, so a head whose only failure is ignored is
// approved; and a list that names only the ignored check names no check on
// the head, so the rollup derived from the rest decides, as for a list
// whose entry never ran there.
func TestRequiredChecksCountOnlyTheChecksMagnumSees(t *testing.T) {
	failed, passed := github.CheckFailed, github.CheckPassed
	for _, tc := range []struct {
		name     string
		required []string
		checks   []github.Check
		note     string
	}{
		{name: "a failing check outside the list", required: []string{"workflow:CI"},
			checks: []github.Check{vendorCheck(failed), specsCheck(passed), ciCheck("Terraform scripts", "lint", failed)},
			note:   "ignored 1 failing check outside required_checks: Terraform scripts / lint"},
		{name: "every check", required: []string{"*"}, checks: []github.Check{vendorCheck(failed), specsCheck(passed)}},
		{name: "only the ignored check", required: []string{"Vendor Gates"}, checks: []github.Check{vendorCheck(failed), specsCheck(passed)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &syncBuffer{}
			h, _ := newAutoHarness(t, func(h *harness) {
				h.cfg.Repos = append(h.cfg.Repos, config.Repo{Repo: "talkable/talkable", RequiredChecks: tc.required, IgnoreChecks: []string{"Vendor Gates"}})
				h.d.Logger = newTeeLogger(h.t, logs)
			})
			reviewedWithChecks(h, "FAILURE", tc.checks...)
			if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:") {
				t.Fatalf("posted = %q, want the approval of b1 (held back for %q)", got, h.autoRefused(2))
			}
			for _, kind := range []string{"review.auto_approve_begin", "review.auto_approved"} {
				evs := h.events(kind)
				if len(evs) != 1 {
					t.Fatalf("%s events = %+v, want one", kind, evs)
				}
				if msg := evs[0].Message; strings.Contains(msg, "Vendor") || (tc.note == "") == strings.Contains(msg, "ignored") ||
					tc.note != "" && !strings.HasSuffix(msg, "; "+tc.note) {
					t.Fatalf("%s = %q, want the note %q and never the ignored check", kind, msg, tc.note)
				}
			}
			if strings.Contains(logs.String(), "Vendor") || tc.note != "" && !strings.Contains(logs.String(), tc.note) {
				t.Fatalf("the daemon's log, want the note %q and never the ignored check:\n%s", tc.note, logs.String())
			}
		})
	}
}
