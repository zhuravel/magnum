package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/fixwork"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// factoryOwnsAt is when the tests' factory took PR #2's fixes.
var factoryOwnsAt = time.Date(2026, 10, 5, 9, 30, 0, 0, time.Local)

// setOwner writes PR #2's owner row as owner, as the handover writes it.
func setOwner(h *harness, owner string) {
	h.t.Helper()
	raw, err := fixwork.FixOwner{Owner: owner, At: factoryOwnsAt, By: "magnum fix --handover"}.Value()
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.SetKV(h.ctx, fixwork.KVPRFixOwner(h.pr(2).ID), raw); err != nil {
		h.t.Fatal(err)
	}
}

// factoryOwns makes the factory fix PR #2.
func factoryOwns(h *harness) { setOwner(h, fixwork.OwnerFactory) }

// wantNoFixerSince fails when PR #2 has a fix loop's row or a held fix, or
// more fix runs than runs.
func wantNoFixerSince(t *testing.T, h *harness, runs int) {
	t.Helper()
	pr := h.pr(2)
	if st, ok := h.fixStateOf(2); ok {
		t.Errorf("a fix loop runs: %+v", st)
	}
	if v, ok := kvValue(h, KVPRFixHeld(pr.ID)); ok {
		t.Errorf("a fix is held: %s", v)
	}
	if n := len(h.fixRuns(pr.ID)); n != runs {
		t.Errorf("fix runs = %d, want %d", n, runs)
	}
}

// The factory fixes PR #2: every path that can start a fixer of magnum's
// on it, or write a row one would start from, refuses. A request answers
// why; a start nobody asked for logs it, or says it in its event.
func TestEveryStartPathRefusesAFactoryOwnedPR(t *testing.T) {
	storetest.Parallel(t)
	const refusal = "the factory fixes this PR"
	for _, tc := range []struct {
		name string
		// run builds the case, makes the factory own PR #2 at the right
		// moment and takes the path: what it said, and the fix runs PR #2
		// had when the factory took it.
		run func(t *testing.T) (h *harness, said string, runs int)
	}{
		{"magnum fix", func(t *testing.T) (*harness, string, int) {
			h, _ := fixHarness(t)
			fixablePR(h, "zhuravel", map[string]int{"P2": 2})
			factoryOwns(h)
			return h, h.refusedFix(FixPayload{}), 0
		}},
		{"magnum fix --dry-run", func(t *testing.T) (*harness, string, int) {
			h, _ := fixHarness(t)
			fixablePR(h, "zhuravel", map[string]int{"P2": 2})
			factoryOwns(h)
			return h, h.refusedFix(FixPayload{DryRun: true}), 0
		}},
		{"magnum fix during a drain, which holds a fix", func(t *testing.T) (*harness, string, int) {
			h, _ := fixHarness(t)
			fixablePR(h, "zhuravel", map[string]int{"P2": 2})
			h.e.setKV(h.ctx, KVDaemonDraining, store.FormatTime(h.clock.Now()))
			factoryOwns(h)
			return h, h.refusedFix(FixPayload{}), 0
		}},
		{"magnum fix --decide", func(t *testing.T) (*harness, string, int) {
			h, _ := fixHarness(t)
			fixablePR(h, "zhuravel", map[string]int{"P2": 2})
			factoryOwns(h)
			return h, h.refusedFix(FixPayload{Decide: []FixDecision{{Thread: 4100, Text: "keep it"}}}), 0
		}},
		{"magnum fix --ci", func(t *testing.T) (*harness, string, int) {
			h, _ := fixHarness(t)
			fixablePR(h, "zhuravel", map[string]int{"P2": 2})
			factoryOwns(h)
			return h, h.refusedFix(FixPayload{Mode: FixModeCI}), 0
		}},
		{"magnum merge-parent", func(t *testing.T) (*harness, string, int) {
			h, mg := mergeHarness(t, withFixerApp)
			fixablePR(h, "zhuravel", nil)
			mg.behind = true
			factoryOwns(h)
			return h, h.refusedFix(FixPayload{Mode: FixModeMerge}), 0
		}},
		{"the push of a held merge", func(t *testing.T) (*harness, string, int) {
			h, mg := holdMerged(t)
			plan := h.planMerge()
			runs := len(h.fixRuns(h.pr(2).ID))
			factoryOwns(h)
			r := h.mergeBound(plan.Asked)
			wantNoPush(t, mg)
			if r.State != store.RequestFailed {
				t.Errorf("the held merge's push: %s %q, want refused", r.State, deref(r.Result))
			}
			return h, deref(r.Result), runs
		}},
		{"autofix after a round", func(t *testing.T) (*harness, string, int) {
			logs := &syncBuffer{}
			h, _ := autofixHarness(t, "zhuravel", findings(map[string]int{"P2": 2}, "COMMENTED"), func(h *harness) { h.d.Logger = newTeeLogger(t, logs) })
			factoryOwns(h)
			h.tick() // the review posts findings, and its end would start autofix's loop
			h.wantState(2, store.PRReviewed)
			return h, logs.String(), 0
		}},
		{"autofix = open's look", func(t *testing.T) (*harness, string, int) {
			logs := &syncBuffer{}
			h, _ := openHarness(t, findings(map[string]int{"P2": 2}, "COMMENTED"), func(h *harness) { h.d.Logger = newTeeLogger(t, logs) })
			factoryOwns(h)
			h.tick()
			h.pastSettle()
			h.tick()
			if _, ok := h.fixEvent("autofix.open"); ok {
				t.Error("the look started a loop")
			}
			return h, logs.String(), 0
		}},
		{"a fix held for the restart, at the start", func(t *testing.T) (*harness, string, int) {
			h, _ := fixHarness(t)
			heldFixDuringADrain(t, h)
			factoryOwns(h)
			h.e.delKV(h.ctx, KVDaemonDraining)
			h.e.startHeldFixes(h.ctx)
			h.settle()
			ev, _ := h.fixEvent("fix.dropped")
			return h, ev.Message, 0
		}},
		{"a fix held for a drain, after it", func(t *testing.T) (*harness, string, int) {
			h, _ := fixHarness(t)
			heldFixDuringADrain(t, h)
			factoryOwns(h)
			h.e.delKV(h.ctx, KVDaemonDraining)
			h.e.continueDrainHeldLoops(h.ctx)
			h.settle()
			ev, _ := h.fixEvent("fix.dropped")
			return h, ev.Message, 0
		}},
		{"a fix held for the fixer's pause, after it", func(t *testing.T) (*harness, string, int) {
			h, _ := fixHarness(t)
			pr := heldFixDuringADrain(t, h)
			factoryOwns(h)
			h.e.delKV(h.ctx, KVDaemonDraining)
			h.e.holdForPause(pr.ID)
			h.e.continuePauseHeld(h.ctx)
			h.settle()
			ev, _ := h.fixEvent("fix.dropped")
			return h, ev.Message, 0
		}},
		{"a fix held for the GitHub budget, after it", func(t *testing.T) (*harness, string, int) {
			h, _ := fixHarness(t)
			pr := heldFixDuringADrain(t, h)
			factoryOwns(h)
			h.e.delKV(h.ctx, KVDaemonDraining)
			h.e.holdForBudget(pr.ID)
			h.e.continueBudgetHeld(h.ctx)
			h.settle()
			ev, _ := h.fixEvent("fix.dropped")
			return h, ev.Message, 0
		}},
		{"a loop's next cycle", func(t *testing.T) (*harness, string, int) {
			h, git := fixHarness(t)
			pr := loopToReReview(t, h, git, findings(map[string]int{"P2": 1}, "COMMENTED"), fixPushed2)
			factoryOwns(h)
			reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"}) // the re-review finds a P2: the next cycle would start
			ev, _ := h.fixEvent("fix.failed")
			return h, ev.Message, len(h.fixRuns(pr.ID))
		}},
		{"the late settle of an unconfirmed push", func(t *testing.T) (*harness, string, int) {
			logs := &syncBuffer{}
			h, _, pr := unconfirmedFixWithLogs(t, logs)
			runs := len(h.fixRuns(pr.ID))
			factoryOwns(h)
			h.gh.setHead("talkable/talkable", 2, fixPushed)
			if err := h.e.poll(h.ctx); err != nil {
				t.Fatal(err)
			}
			h.settle()
			if n := len(h.fixEvents("fix.push_confirmed_late")); n != 0 {
				t.Errorf("the push was settled late %d times", n)
			}
			if cur := h.pr(2); cur.Forced {
				t.Error("the late settle forced a re-review")
			}
			return h, logs.String(), runs
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, said, runs := tc.run(t)
			wantNoFixerSince(t, h, runs)
			if !strings.Contains(said, refusal) {
				t.Errorf("it said %q, want %q", said, refusal)
			}
		})
	}
}

// A start whose read of the owner row fails starts no fixer: the fixer side
// fails closed. A row that is not an owner is that failed read, never no
// row: a request names it, and autofix starts nothing.
func TestAFailedReadOfTheOwnerRowStartsNoFixer(t *testing.T) {
	storetest.Parallel(t)
	t.Run("magnum fix", func(t *testing.T) {
		h, _ := fixHarness(t)
		pr := fixablePR(h, "zhuravel", map[string]int{"P2": 2})
		if err := h.st.SetKV(h.ctx, fixwork.KVPRFixOwner(pr.ID), `{"owner":"someone newer"}`); err != nil {
			t.Fatal(err)
		}
		if said := h.refusedFix(FixPayload{}); !strings.Contains(said, "who fixes this PR cannot be read") {
			t.Errorf("magnum fix = %q, want the failed read named", said)
		}
		wantNoFixerSince(t, h, 0)
	})
	t.Run("autofix", func(t *testing.T) {
		logs := &syncBuffer{}
		h, _ := autofixHarness(t, "zhuravel", findings(map[string]int{"P2": 2}, "COMMENTED"), func(h *harness) { h.d.Logger = newTeeLogger(t, logs) })
		if err := h.st.SetKV(h.ctx, fixwork.KVPRFixOwner(h.pr(2).ID), "not json"); err != nil {
			t.Fatal(err)
		}
		h.tick()
		h.wantState(2, store.PRReviewed)
		wantNoFixerSince(t, h, 0)
		if !strings.Contains(logs.String(), "who fixes this PR cannot be read") {
			t.Error("autofix did not log the failed read")
		}
	})
}

// refusedFix asks for a fix of PR #2 with p and returns the refusal; a
// request that was not refused fails the test.
func (h *harness) refusedFix(p FixPayload) string {
	h.t.Helper()
	id := h.fix(p)
	h.handle()
	r := h.request(id)
	if r.State != store.RequestFailed {
		h.t.Errorf("magnum fix %+v: %s %q, want refused", p, r.State, deref(r.Result))
	}
	return deref(r.Result)
}

// unconfirmedFixWithLogs is unconfirmedFix whose engine logs to logs too.
func unconfirmedFixWithLogs(t *testing.T, logs *syncBuffer) (*harness, *fixGit, store.PR) {
	t.Helper()
	h, git := fixHarness(t, func(h *harness) { h.d.Logger = newTeeLogger(t, logs) })
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	git.lostPush = true
	fixerBy(h, git, func(run store.Run) (string, string) {
		return fixPushed, fixReport(run.ID, "b2b2b2b Guard the nil owner", fixReplied, "none")
	})
	h.fix(FixPayload{})
	h.handle()
	h.wantState(2, store.PRNeedsAttention)
	if rec, ok := h.fixPushOf(2); !ok || rec.Kind != fixPushUnconfirmed {
		t.Fatalf("the record = %+v, %v; want the push unconfirmed", rec, ok)
	}
	return h, git, pr
}
