package engine

import (
	"strings"
	"sync"
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

// unreadableOwner writes PR #2 an owner row this build cannot parse: who
// fixes it cannot be read.
func unreadableOwner(h *harness) {
	h.t.Helper()
	if err := h.st.SetKV(h.ctx, fixwork.KVPRFixOwner(h.pr(2).ID), `{"owner":"someone newer"}`); err != nil {
		h.t.Fatal(err)
	}
}

// startPath is a path that can start a fixer of magnum's on PR #2, or write
// a row one would start from: run builds the case, calls own at the right
// moment, which makes magnum not PR #2's fixer, and takes the path: what it
// said, and the fix runs PR #2 had at that moment.
type startPath struct {
	name string
	run  func(t *testing.T, own func(*harness)) (h *harness, said string, runs int)
}

// refusesEveryStart takes every start path with own, and fails unless each
// refuses, saying refusal (a request answers why; a start nobody asked for
// logs it, or says it in its event), and starts no fixer.
func refusesEveryStart(t *testing.T, own func(*harness), refusal string) {
	t.Helper()
	for _, tc := range startPaths() {
		t.Run(tc.name, func(t *testing.T) {
			h, said, runs := tc.run(t, own)
			wantNoFixerSince(t, h, runs) // no held fix either: the held-fix cases checked theirs, kept, and set it aside
			if !strings.Contains(said, refusal) {
				t.Errorf("it said %q, want %q", said, refusal)
			}
		})
	}
}

// The factory fixes PR #2: every path that can start a fixer of magnum's
// on it, or write a row one would start from, refuses.
func TestEveryStartPathRefusesAFactoryOwnedPR(t *testing.T) {
	storetest.Parallel(t)
	refusesEveryStart(t, factoryOwns, "the factory fixes this PR")
}

// A start whose read of the owner row fails starts no fixer: the fixer side
// fails closed, on every start path. A row this build cannot parse is that
// failed read, never no row.
func TestAFailedReadOfTheOwnerRowStartsNoFixer(t *testing.T) {
	storetest.Parallel(t)
	refusesEveryStart(t, unreadableOwner, "who fixes this PR cannot be read")
}

// startPaths are the paths that can start a fixer of magnum's on PR #2.
func startPaths() []startPath {
	return []startPath{
		{"magnum fix", func(t *testing.T, own func(*harness)) (*harness, string, int) {
			t.Helper()
			h, _ := fixHarness(t)
			fixablePR(h, "zhuravel", map[string]int{"P2": 2})
			own(h)
			said := h.refusedFix(FixPayload{})
			return h, said, 0
		}},
		{"magnum fix --dry-run", func(t *testing.T, own func(*harness)) (*harness, string, int) {
			t.Helper()
			h, _ := fixHarness(t)
			fixablePR(h, "zhuravel", map[string]int{"P2": 2})
			own(h)
			said := h.refusedFix(FixPayload{DryRun: true})
			return h, said, 0
		}},
		{"magnum fix during a drain, which holds a fix", func(t *testing.T, own func(*harness)) (*harness, string, int) {
			t.Helper()
			h, _ := fixHarness(t)
			fixablePR(h, "zhuravel", map[string]int{"P2": 2})
			h.e.setKV(h.ctx, KVDaemonDraining, store.FormatTime(h.clock.Now()))
			own(h)
			said := h.refusedFix(FixPayload{})
			return h, said, 0
		}},
		{"magnum fix --decide", func(t *testing.T, own func(*harness)) (*harness, string, int) {
			t.Helper()
			h, _ := fixHarness(t)
			fixablePR(h, "zhuravel", map[string]int{"P2": 2})
			own(h)
			said := h.refusedFix(FixPayload{Decide: []FixDecision{{Thread: 4100, Text: "keep it"}}})
			return h, said, 0
		}},
		{"magnum fix --ci", func(t *testing.T, own func(*harness)) (*harness, string, int) {
			t.Helper()
			h, _ := fixHarness(t)
			fixablePR(h, "zhuravel", map[string]int{"P2": 2})
			own(h)
			said := h.refusedFix(FixPayload{Mode: FixModeCI})
			return h, said, 0
		}},
		{"magnum merge-parent", func(t *testing.T, own func(*harness)) (*harness, string, int) {
			t.Helper()
			h, mg := mergeHarness(t, withFixerApp)
			fixablePR(h, "zhuravel", nil)
			mg.behind = true
			own(h)
			said := h.refusedFix(FixPayload{Mode: FixModeMerge})
			return h, said, 0
		}},
		{"the push of a held merge", func(t *testing.T, own func(*harness)) (*harness, string, int) {
			t.Helper()
			h, mg := holdMerged(t)
			plan := h.planMerge()
			runs := len(h.fixRuns(h.pr(2).ID))
			own(h)
			r := h.mergeBound(plan.Asked)
			wantNoPush(t, mg)
			if r.State != store.RequestFailed {
				t.Errorf("the held merge's push: %s %q, want refused", r.State, deref(r.Result))
			}
			return h, deref(r.Result), runs
		}},
		{"autofix after a round", func(t *testing.T, own func(*harness)) (*harness, string, int) {
			t.Helper()
			logs := &syncBuffer{}
			h, _ := autofixHarness(t, "zhuravel", findings(map[string]int{"P2": 2}, "COMMENTED"), func(h *harness) { h.d.Logger = newTeeLogger(t, logs) })
			own(h)
			h.tick() // the review posts findings, and its end would start autofix's loop
			h.wantState(2, store.PRReviewed)
			return h, logs.String(), 0
		}},
		{"autofix = open's look", func(t *testing.T, own func(*harness)) (*harness, string, int) {
			t.Helper()
			logs := &syncBuffer{}
			h, _ := openHarness(t, findings(map[string]int{"P2": 2}, "COMMENTED"), func(h *harness) { h.d.Logger = newTeeLogger(t, logs) })
			own(h)
			h.tick()
			h.pastSettle()
			h.tick()
			if _, ok := h.fixEvent("autofix.open"); ok {
				t.Error("the look started a loop")
			}
			return h, logs.String(), 0
		}},
		{"a fix held for the restart, at the start", func(t *testing.T, own func(*harness)) (*harness, string, int) {
			t.Helper()
			return heldFixOfTheFactory(t, own, func(h *harness, _ store.PR) { h.e.startHeldFixes(h.ctx) })
		}},
		{"a fix held for a drain, after it", func(t *testing.T, own func(*harness)) (*harness, string, int) {
			t.Helper()
			return heldFixOfTheFactory(t, own, func(h *harness, _ store.PR) { h.e.continueDrainHeldLoops(h.ctx) })
		}},
		{"a fix held for the fixer's pause, after it", func(t *testing.T, own func(*harness)) (*harness, string, int) {
			t.Helper()
			return heldFixOfTheFactory(t, own, func(h *harness, pr store.PR) {
				h.e.holdForPause(pr.ID)
				h.e.continuePauseHeld(h.ctx)
			})
		}},
		{"a fix held for the GitHub budget, after it", func(t *testing.T, own func(*harness)) (*harness, string, int) {
			t.Helper()
			return heldFixOfTheFactory(t, own, func(h *harness, pr store.PR) {
				h.e.holdForBudget(pr.ID)
				h.e.continueBudgetHeld(h.ctx)
			})
		}},
		{"a loop's next cycle", func(t *testing.T, own func(*harness)) (*harness, string, int) {
			t.Helper()
			h, git := fixHarness(t)
			pr := loopToReReview(t, h, git, findings(map[string]int{"P2": 1}, "COMMENTED"), fixPushed2)
			own(h)
			reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"}) // the re-review finds a P2: the next cycle would start
			ev, _ := h.fixEvent("fix.failed")
			return h, ev.Message, len(h.fixRuns(pr.ID))
		}},
		{"the late settle of an unconfirmed push", func(t *testing.T, own func(*harness)) (*harness, string, int) {
			t.Helper()
			logs := &syncBuffer{}
			h, _, pr := unconfirmedFixWithLogs(t, logs)
			runs := len(h.fixRuns(pr.ID))
			own(h)
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
		{"a flip between the start's checks and its claim", func(t *testing.T, own func(*harness)) (*harness, string, int) {
			t.Helper()
			h, git := fixHarness(t)
			fixablePR(h, "zhuravel", map[string]int{"P2": 2})
			fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") }) // a fixer that started would end
			var once sync.Once
			h.gh.mu.Lock()
			h.gh.onThreads = func() { // prepareFix read no owner row and reads the other reviewers, unreserved
				once.Do(func() { own(h) }) // the handover's flip lands meanwhile
			}
			h.gh.mu.Unlock()
			said := h.refusedFix(FixPayload{}) // claimFixRow, under the reservation, reads the row again
			if threads := h.gh.count("threads:talkable/talkable#2"); threads == 0 {
				t.Error("the start read no threads: the flip did not land between its checks and its claim")
			}
			return h, said, 0
		}},
	}
}

// heldFixOfTheFactory holds a fix of PR #2 through a drain, makes magnum
// not its fixer (own), ends the drain and takes act, a path that would start
// the held fix: it fails unless the held fix stays as it was, for the
// factory's import, and none was dropped. Then it sets the held fix aside,
// so that its caller's check finds no other. It returns what the engine
// logged.
func heldFixOfTheFactory(t *testing.T, own func(*harness), act func(h *harness, pr store.PR)) (*harness, string, int) {
	t.Helper()
	logs := &syncBuffer{}
	h, _ := fixHarness(t, func(h *harness) { h.d.Logger = newTeeLogger(t, logs) })
	pr := heldFixDuringADrain(t, h)
	held, _ := kvValue(h, KVPRFixHeld(pr.ID))
	own(h)
	h.e.delKV(h.ctx, KVDaemonDraining)
	act(h, pr)
	h.settle()
	if now, ok := kvValue(h, KVPRFixHeld(pr.ID)); !ok || now != held {
		t.Errorf("the held fix = %q, %v; want it kept as %s for the factory's import", now, ok, held)
	}
	if _, ok := h.fixEvent("fix.dropped"); ok {
		t.Error("a held fix was dropped")
	}
	h.e.delKV(h.ctx, KVPRFixHeld(pr.ID))
	return h, logs.String(), 0
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
