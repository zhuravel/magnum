package engine

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/store"
)

// localSeams is what the fixture's daemon reads of the local cases: their corpus (or why it cannot be
// read), the inputs a local round reads with each tree (evalrun.LocalReviewInputs) and why a case is
// refused (evalrun.Refused).
type localSeams struct {
	mu      sync.Mutex
	corpus  *eval.Corpus
	err     error
	inputs  map[string]string
	refused map[string]string // case -> why
}

func (l *localSeams) read() (*eval.Corpus, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.corpus, l.err
}

func (l *localSeams) inputsOf(dir string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	in, ok := l.inputs[dir]
	if !ok {
		return "", errors.New("no local inputs for " + dir)
	}
	return in, nil
}

func (l *localSeams) refusedOf(_ context.Context, c eval.Case) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.refused[c.Name]
}

// localRef is the operator's run whose replay of the local case, at the base's local inputs, found its
// question defect: the replay the case guards by.
const localRef = "20261002-120000"

// localGateFixture is a gate of files (gate_guards 1) whose daemon reads the local cases' corpus, one
// local case, task-1-round-1, whose newest replay (localRef, at the base's local inputs) found its
// question defect q; the base and the candidate read different local inputs. The target has a stored
// baseline that misses it, and the candidate finds it every time; the posted other case guards
// nothing. local scripts the local case's candidate replays.
func localGateFixture(t *testing.T, files []string, local ...*eval.Score) (*gateFixture, *localSeams, eval.Case) {
	t.Helper()
	return localGateFixtureOf(t, files, []eval.Defect{{ID: "q", Title: "the plan's choice", Question: true, Match: []string{"stored"}}}, local...)
}

// localGateFixtureOf is localGateFixture with the local case's defects, every one of which its
// guarding replay found.
func localGateFixtureOf(t *testing.T, files []string, defects []eval.Defect, local ...*eval.Score) (*gateFixture, *localSeams, eval.Case) {
	t.Helper()
	lc := eval.Case{Name: "task-1-round-1", Kind: eval.KindLocal, PR: "talkable/talkable#900011", Head: retroShaA, Defects: defects}
	seams := &localSeams{corpus: &eval.Corpus{Cases: []eval.Case{lc}}}
	f := newGateFixture(t, files, func(h *harness) {
		h.cfg.Learn.GateGuards = 1
		h.d.EvalLocalCorpus, h.d.EvalLocalInputs, h.d.EvalRefused = seams.read, seams.inputsOf, seams.refusedOf
	})
	seams.inputs = map[string]string{f.base: "local-master", f.wt: "local-cand"}
	f.ev.local = seams.inputs
	f.ev.corpus = &eval.Corpus{Cases: []eval.Case{f.target}}
	f.baseline("20261001-120000", "rev-master", map[string]*eval.Score{f.target.Name: found(nil, f.target.Name)})
	var ids []string
	for _, d := range defects {
		ids = append(ids, d.ID)
	}
	s := found(ids)
	s.Case = lc.Name
	r := eval.Run{ID: localRef, Inputs: "in-master", ReviewerInputs: "rev-master", Commit: "abc1234",
		Cases: []eval.CaseRun{{Case: lc.Name, Kind: eval.KindLocal, PR: lc.PR, Head: lc.Head, Outcome: "dry_run", Score: s, Inputs: "local-master",
			ReportDir: filepath.Join(f.h.layout.Eval(), localRef, "cases", lc.Name)}}}
	if err := eval.SaveRun(filepath.Join(f.h.layout.Eval(), r.ID), r); err != nil {
		t.Fatal(err)
	}
	f.ev.script(f.wt, f.target.Name, found([]string{f.target.Name}))
	f.ev.script(f.wt, lc.Name, local...)
	return f, seams, lc
}

// localRow is the gate's row of the local case.
func localRow(t *testing.T, f *gateFixture, lc eval.Case) GateCase {
	t.Helper()
	g := f.gate()
	i := slices.IndexFunc(g.Cases, func(c GateCase) bool { return c.Case == lc.Name })
	if i < 0 {
		t.Fatalf("no row of %s: %+v", lc.Name, g.Cases)
	}
	return g.Cases[i]
}

// A local case takes a guard place of a gate in either mode and replays with every role, in a run of
// its own ("local candidate"), even in a judge-only gate, whose target still replays the judge alone:
// a local round runs every role. Its baseline is the stored replay that chose it, and a candidate that
// finds what it found passes with no replay of the base.
func TestAGateReplaysALocalGuardWithEveryRoleInARunOfItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
	}{{"judge-only", []string{learn.DecisionsFile, learn.SkillFile}}, {"full", gateFullFiles}} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, lc := localGateFixture(t, tc.files, found([]string{"q"}))
			f.h.tick()
			if p := f.proposal(); p.State != store.LearnPassed {
				t.Fatalf("proposal %s %q", p.State, p.Reason)
			}
			var localRun, targetRun string
			for _, c := range f.ev.callsFor(f.wt) {
				switch c.Case {
				case lc.Name:
					if c.Opts.JudgeOnly {
						t.Fatalf("the local case replayed judge-only: %+v", c)
					}
					localRun = c.Opts.RunDir
				case f.target.Name:
					if c.Opts.JudgeOnly != (tc.name == "judge-only") {
						t.Fatalf("the target replayed judge-only %v in a %s gate", c.Opts.JudgeOnly, tc.name)
					}
					targetRun = c.Opts.RunDir
				}
			}
			if localRun == "" || localRun == targetRun {
				t.Fatalf("local run %q, target run %q: want a run of its own", localRun, targetRun)
			}
			if row := localRow(t, f, lc); !row.Local || !row.Guard || !row.Stored || row.ReportsRun != "" || row.BaselineRun != localRef || row.Skip != "" {
				t.Fatalf("the local row %+v", row)
			}
			if got := f.replaysByKind(); got[learn.GateRunLocal] != 1 || got["baseline"] != 0 || got["candidate"] != 1 {
				t.Fatalf("replays %v", got)
			}
			if data := f.h.eventData("learn.gate_start"); !strings.Contains(data, `"local_guards":1`) {
				t.Fatalf("gate start %s, want the local guard counted", data)
			}
		})
	}
}

// A local guard the candidate lost on both replays is weighed against two replays of the base made
// after the one that chose it, even when that one was the base's own (a local replay is a coin flip,
// and the one that chose the guard found its defect by being chosen): a loss counts only when both
// find what the candidate lost; one base replay that does not find it is enough to call it lost at
// the base, and no second is made.
func TestALocalGuardLostTwiceIsALossOnlyWhenTwoFreshBaseReplaysFindIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		base   []*eval.Score
		state  string
		reason string
		second int
	}{
		{"both find it", []*eval.Score{found([]string{"q"})}, store.LearnFailed, "task-1-round-1 lost q after a second replay", 1},
		{"the second loses it", []*eval.Score{found([]string{"q"}), found(nil, "q")}, store.LearnPassed, "", 1},
		{"the first loses it", []*eval.Score{found(nil, "q")}, store.LearnPassed, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, lc := localGateFixture(t, gateFullFiles, found(nil, "q"))
			f.ev.script(f.base, lc.Name, tc.base...)
			f.h.tick()
			if p := f.proposal(); p.State != tc.state || !strings.Contains(p.Reason, tc.reason) {
				t.Fatalf("proposal %s %q, want %s naming %q", p.State, p.Reason, tc.state, tc.reason)
			}
			got := f.replaysByKind()
			if got["baseline"] != 1 || got[learn.GateRunBaseSecond] != tc.second || got[learn.GateRunLocal] != 1 || got[learn.GateRunLocalSecond] != 1 {
				t.Fatalf("replays %v, want one of the base, %d second of it, and two of the candidate", got, tc.second)
			}
			row := localRow(t, f, lc)
			if row.Stored || !row.Retried || row.First == nil || !strings.Contains(row.Note, localRef) {
				t.Fatalf("row %+v", row)
			}
			if tc.state == store.LearnPassed && row.Skip != learn.SkipLostAtBase {
				t.Fatalf("row %+v, want it lost at the base", row)
			}
			if tc.state == store.LearnPassed { // the warning names the inputs the base's replays ran with: its round's
				evs := f.h.events("learn.guard_lost_at_base")
				if data := f.h.eventData("learn.guard_lost_at_base"); len(evs) != 1 || !strings.Contains(evs[0].Message, "(inputs local-master)") ||
					!strings.Contains(data, `"inputs":"local-master"`) {
					t.Fatalf("lost at the base %+v, data %s; want the base's local inputs", evs, data)
				}
			}
		})
	}
}

// A gate resumed after a stop between the base's two replays of a local guard reuses the first,
// which it stored at the base's local inputs, and replays only the second: the loss is decided on
// both.
func TestAResumedGateReusesTheBasesStoredReplayOfALocalGuard(t *testing.T) {
	f, _, lc := localGateFixture(t, gateFullFiles, found(nil, "q"))
	f.ev.script(f.base, lc.Name, found([]string{"q"}))
	f.ev.after = func(c evalCall) {
		if c.Case == lc.Name && c.Opts.Candidate == f.base {
			f.ev.fail(f.base, lc.Name, "error") // the base's second replay
		}
	}
	f.h.tick()
	if p := f.proposal(); p.State != store.LearnGating {
		t.Fatalf("proposal %s %q, want gating", p.State, p.Reason)
	}
	if n := countCalls(f.ev.callsFor(f.base), lc.Name); n != 2 {
		t.Fatalf("base replays %d, want two", n)
	}
	f.ev.mu.Lock()
	f.ev.after = nil
	f.ev.mu.Unlock()
	f.ev.fail(f.base, lc.Name, "")
	f.h.advance(gateRetryAfter + time.Minute)
	f.h.tick()
	if p := f.proposal(); p.State != store.LearnFailed || !strings.Contains(p.Reason, "task-1-round-1 lost q after a second replay") {
		t.Fatalf("proposal %s %q", p.State, p.Reason)
	}
	if n := countCalls(f.ev.callsFor(f.base), lc.Name); n != 3 {
		t.Fatalf("base replays %d, want the next gate to replay the second alone", n)
	}
	if n := countCalls(f.ev.callsFor(f.wt), lc.Name); n != 2 {
		t.Fatalf("candidate replays %d, want both reused", n)
	}
	if got := f.replaysByKind(); got["baseline"] != 1 || got[learn.GateRunBaseSecond] != 2 {
		t.Fatalf("replays %v", got)
	}
}

// A local guard lost twice takes the base's two stored replays at its local inputs replayed last
// (by when they were replayed, not by their run's id), no more, and makes no replay of the base.
func TestALocalGuardTakesTheTwoBaseReplaysMadeLast(t *testing.T) {
	f, _, lc := localGateFixture(t, gateFullFiles, found(nil, "q"))
	day := f.p.CreatedAt.Add(time.Hour).Truncate(24 * time.Hour)
	for _, r := range []struct {
		id string
		at time.Duration
		s  *eval.Score
	}{ // run ids in another order than their replays
		{"20261005-180000", 23 * time.Hour, found([]string{"q"})},
		{"20261005-190000", 21 * time.Hour, found(nil, "q")},
		{"20261005-200000", 22 * time.Hour, found([]string{"q"})},
	} {
		r.s.Case = lc.Name
		run := eval.Run{ID: r.id, Inputs: "in-master", ReviewerInputs: "rev-master", Commit: "abc1234", Started: day.Add(r.at),
			Cases: []eval.CaseRun{{Case: lc.Name, Kind: eval.KindLocal, PR: lc.PR, Head: lc.Head, Outcome: "dry_run", Score: r.s,
				Inputs: "local-master", Started: day.Add(r.at), ReportDir: filepath.Join(f.h.layout.Eval(), r.id, "cases", lc.Name)}}}
		if err := eval.SaveRun(filepath.Join(f.h.layout.Eval(), r.id), run); err != nil {
			t.Fatal(err)
		}
	}
	f.h.tick()
	if p := f.proposal(); p.State != store.LearnFailed || !strings.Contains(p.Reason, "task-1-round-1 lost q after a second replay") {
		t.Fatalf("proposal %s %q", p.State, p.Reason)
	}
	if got := f.replaysByKind(); got["baseline"] != 0 || got[learn.GateRunBaseSecond] != 0 {
		t.Fatalf("replays %v, want none of the base", got)
	}
	if row := localRow(t, f, lc); row.BaselineRun != "20261005-200000" || !strings.HasSuffix(row.Note, "runs 20261005-180000 and 20261005-200000") {
		t.Fatalf("row %+v, want the base's replays of 23:00 and 22:00", row)
	}
}

// A defect counts as found at the base only when both of the base's replays found it: a local guard
// the candidate lost two defects of, each found by one base replay alone, is lost at the base.
func TestALocalGuardsDefectsFoundByOneBaseReplayEachAreLostAtTheBase(t *testing.T) {
	f, _, lc := localGateFixtureOf(t, gateFullFiles, []eval.Defect{{ID: "a", Title: "a", Match: []string{"a"}}, {ID: "b", Title: "b", Match: []string{"b"}}},
		found(nil, "a", "b"))
	f.ev.script(f.base, lc.Name, found([]string{"a"}, "b"), found([]string{"b"}, "a"))
	f.h.tick()
	if p := f.proposal(); p.State != store.LearnPassed {
		t.Fatalf("proposal %s %q", p.State, p.Reason)
	}
	if got := f.replaysByKind(); got["baseline"] != 1 || got[learn.GateRunBaseSecond] != 1 {
		t.Fatalf("replays %v", got)
	}
	if row := localRow(t, f, lc); row.Skip != learn.SkipLostAtBase {
		t.Fatalf("row %+v, want it lost at the base", row)
	}
}

// A local guard can only add a loss, so a gate its other cases fail already replays none: its
// replays with every role, up to four of 30 to 47 minutes, could not change the verdict. A revert,
// which waits for the operator whatever the gate finds, still replays it.
func TestAGateThatFailsOnItsOtherCasesReplaysNoLocalGuard(t *testing.T) {
	t.Run("failed", func(t *testing.T) {
		f, _, lc := localGateFixture(t, gateFullFiles, found(nil, "q"))
		f.ev.script(f.wt, f.target.Name, found(nil, f.target.Name))
		f.h.tick()
		if p := f.proposal(); p.State != store.LearnFailed || !strings.Contains(p.Reason, "no target improved") {
			t.Fatalf("proposal %s %q", p.State, p.Reason)
		}
		if n, b := countCalls(f.ev.callsFor(f.wt), lc.Name), countCalls(f.ev.callsFor(f.base), lc.Name); n != 0 || b != 0 {
			t.Fatalf("local replays %d of the candidate, %d of the base; want none", n, b)
		}
		if row := localRow(t, f, lc); !row.Guard || !row.Local || !strings.Contains(row.Skip, "fails") {
			t.Fatalf("row %+v, want it skipped, saying why", row)
		}
	})
	t.Run("a revert", func(t *testing.T) {
		f, _, lc := localGateFixture(t, gateFullFiles, found([]string{"q"}))
		f.ev.script(f.wt, f.target.Name, found(nil, f.target.Name))
		v, err := f.h.st.CreateLearnVersion(f.h.ctx, store.LearnVersion{LandedSHA: learnMaster, SkillSHA: "skill", PromptsSHA: "prompts"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.h.st.DB().ExecContext(f.h.ctx, "UPDATE learn_proposals SET reverts = ? WHERE id = ?", v.ID, f.p.ID); err != nil {
			t.Fatal(err)
		}
		f.h.tick()
		if p := f.proposal(); p.State != store.LearnHeld || !strings.Contains(p.Reason, "revert") {
			t.Fatalf("proposal %s %q", p.State, p.Reason)
		}
		if n := countCalls(f.ev.callsFor(f.wt), lc.Name); n != 1 {
			t.Fatalf("local replays %d, want the revert's guard replayed", n)
		}
	})
}

// A gate that stops before its local guards, on a stop that holds the proposal (the third in a row
// on a replay without a score, here the target's second replay that would confirm its win), stores
// the local guard's row saying why it has no score, never a bare row: the guard was not replayed.
func TestAHeldGateThatStoppedBeforeItsLocalGuardsSaysWhyTheirRowsHaveNoScore(t *testing.T) {
	f, _, lc := localGateFixture(t, gateFullFiles, found([]string{"q"}))
	f.ev.after = func(c evalCall) {
		if c.Case == f.target.Name && c.Opts.Candidate == f.wt {
			f.ev.fail(f.wt, f.target.Name, "needs_attention") // every replay after its first
		}
	}
	for range gateStopLimit {
		f.h.tick()
		f.h.advance(gateRetryAfter + time.Minute)
	}
	if p := f.proposal(); p.State != store.LearnHeld || !strings.Contains(p.Reason, "needs_attention") {
		t.Fatalf("proposal %s %q, want held on the stops", p.State, p.Reason)
	}
	if n := countCalls(f.ev.callsFor(f.wt), lc.Name); n != 0 {
		t.Fatalf("local replays %d, want none", n)
	}
	if row := localRow(t, f, lc); !row.Guard || !row.Local || row.Skip != skipStoppedBefore || row.After != nil || row.Before != nil {
		t.Fatalf("row %+v, want it skipped, saying the gate stopped before it", row)
	}
}

// A local candidate's first replay that ends without a score (a frozen role the config no longer
// declares or no gh identity, config_error; a lost reviewer's report; a setup that failed; a
// refusal, a flag, an error such as frozen inputs that are gone) is never a loss and never a stop:
// its row is skipped with why and the proposal is decided on its other cases, as a posted guard's
// is. config_error, which a replay cannot change until the config does, skips the row on the
// candidate's second replay and the base's too.
func TestALocalReplayThatCannotScoreIsNeverALossNorAStop(t *testing.T) {
	for _, outcome := range []string{eval.OutcomeMissingRole, eval.OutcomeConfigError, eval.OutcomeEnvironment, "refused", eval.OutcomeFlagged, "error"} {
		t.Run("first "+outcome, func(t *testing.T) {
			f, _, lc := localGateFixture(t, gateFullFiles, found(nil, "q"))
			f.ev.fail(f.wt, lc.Name, outcome)
			f.h.tick()
			if p := f.proposal(); p.State != store.LearnPassed || len(f.h.events("learn.gate_waiting")) != 0 {
				t.Fatalf("proposal %s %q", p.State, p.Reason)
			}
			if row := localRow(t, f, lc); row.Skip == "" || row.Retried {
				t.Fatalf("row %+v", row)
			}
			if outcome == "refused" { // the factory never reads the flag the replay set: the operator is told
				if evs := f.h.events("learn.local_refused"); len(evs) != 1 || !strings.Contains(f.toasts(), "local case "+lc.Name) {
					t.Fatalf("refused events %+v, toasts %q", evs, f.toasts())
				}
			}
		})
	}
	for _, outcome := range []string{eval.OutcomeConfigError} {
		t.Run("second "+outcome, func(t *testing.T) {
			f, _, lc := localGateFixture(t, gateFullFiles, found(nil, "q"))
			f.ev.after = func(c evalCall) {
				if c.Case == lc.Name && c.Opts.Candidate == f.wt {
					f.ev.fail(f.wt, lc.Name, outcome) // the second replay
				}
			}
			f.h.tick()
			if p := f.proposal(); p.State != store.LearnPassed || len(f.h.events("learn.gate_waiting")) != 0 {
				t.Fatalf("proposal %s %q", p.State, p.Reason)
			}
			if row := localRow(t, f, lc); !strings.Contains(row.Skip, outcome) {
				t.Fatalf("row %+v", row)
			}
		})
		t.Run("base "+outcome, func(t *testing.T) {
			f, _, lc := localGateFixture(t, gateFullFiles, found(nil, "q"))
			f.ev.fail(f.base, lc.Name, outcome)
			f.h.tick()
			if p := f.proposal(); p.State != store.LearnPassed || len(f.h.events("learn.gate_waiting")) != 0 {
				t.Fatalf("proposal %s %q", p.State, p.Reason)
			}
			if row := localRow(t, f, lc); !strings.Contains(row.Skip, outcome) {
				t.Fatalf("row %+v", row)
			}
		})
	}
}

// A local candidate's first replay that a usage limit or a logout stopped (evalrun ends it so, also
// when a reviewer's report is missing) stops the gate as a posted case's does: the proposal stays
// gating, one learn.gate_waiting event names the outcome, and the stop counts as an attempt
// (gateAttemptLimit), never as a replay without a score (gateStopLimit); the row is not left out, as
// a missing_role one would be.
func TestALocalReplayAUsageLimitOrALogoutStoppedStopsTheGate(t *testing.T) {
	for _, outcome := range []string{"usage_limit", "login_required"} {
		t.Run(outcome, func(t *testing.T) {
			f, _, lc := localGateFixture(t, gateFullFiles, found(nil, "q"))
			f.ev.fail(f.wt, lc.Name, outcome)
			f.h.tick()
			evs := f.h.events("learn.gate_waiting")
			if p := f.proposal(); p.State != store.LearnGating || len(evs) != 1 || !strings.Contains(evs[0].Message, outcome) {
				t.Fatalf("proposal %s %q, waiting events %+v; want gating on the %s", p.State, p.Reason, evs, outcome)
			}
			if s, ok := f.h.e.gateStopsOf(f.h.ctx, f.p.ID); !ok || s.Stops != 0 || s.Attempts != 1 {
				t.Fatalf("stops %+v, %v; want one attempt and no stop without a score", s, ok)
			}
			if n := countCalls(f.ev.callsFor(f.wt), lc.Name); n != 1 {
				t.Fatalf("local replays %d, want the first alone", n)
			}
		})
	}
}

// A local replay that lost a reviewer's report (a lost pane, a timeout) or whose worktree could not
// be prepared (another case's preparation held the lock, a setup that failed) after the candidate's
// first replay scored and lost a defect is no evidence the loss is not the candidate's: on the
// candidate's second replay or the base's it stops the gate as a posted case's does, the stop
// counted, and the proposal stays gating for the next gate.
func TestALocalReplayThatLostAReportLaterStopsTheGate(t *testing.T) {
	for _, outcome := range []string{eval.OutcomeMissingRole, eval.OutcomeEnvironment} {
		for _, which := range []string{"second", "base"} {
			t.Run(which+" "+outcome, func(t *testing.T) {
				f, _, lc := localGateFixture(t, gateFullFiles, found(nil, "q"))
				if which == "base" {
					f.ev.fail(f.base, lc.Name, outcome)
				} else {
					f.ev.after = func(c evalCall) {
						if c.Case == lc.Name && c.Opts.Candidate == f.wt {
							f.ev.fail(f.wt, lc.Name, outcome)
						}
					}
				}
				f.h.tick()
				if p := f.proposal(); p.State != store.LearnGating || len(f.h.events("learn.gate_waiting")) != 1 {
					t.Fatalf("proposal %s %q, want gating", p.State, p.Reason)
				}
				if s, ok := f.h.e.gateStopsOf(f.h.ctx, f.p.ID); !ok || s.Stops != 1 {
					t.Fatalf("stops %+v, %v; want one counted", s, ok)
				}
			})
		}
	}
}

// A local case's second replay that ends without a score for another reason stops the gate as a
// posted case's does (a loss is decided on two replays); the proposal stays gating, the stop counted.
// The next gate reuses the first replay the stopped one made (its round's inputs are the candidate's)
// and replays only the second.
func TestALocalSecondReplayWithoutAScoreStopsTheGateAndTheNextReusesTheFirst(t *testing.T) {
	f, _, lc := localGateFixture(t, gateFullFiles, found(nil, "q"))
	f.ev.script(f.base, lc.Name, found([]string{"q"}))
	f.ev.after = func(c evalCall) {
		if c.Case == lc.Name && c.Opts.Candidate == f.wt {
			f.ev.fail(f.wt, lc.Name, "error")
		}
	}
	f.h.tick()
	if p := f.proposal(); p.State != store.LearnGating {
		t.Fatalf("proposal %s %q, want gating", p.State, p.Reason)
	}
	if s, ok := f.h.e.gateStopsOf(f.h.ctx, f.p.ID); !ok || s.Stops != 1 {
		t.Fatalf("stops %+v, %v; want one", s, ok)
	}
	if n := countCalls(f.ev.callsFor(f.wt), lc.Name); n != 2 {
		t.Fatalf("local replays %d, want the first and the second", n)
	}
	f.ev.mu.Lock()
	f.ev.after = nil
	f.ev.mu.Unlock()
	f.ev.fail(f.wt, lc.Name, "")
	f.h.advance(gateRetryAfter + time.Minute)
	f.h.tick()
	if p := f.proposal(); p.State != store.LearnFailed || !strings.Contains(p.Reason, "task-1-round-1 lost q after a second replay") {
		t.Fatalf("proposal %s %q", p.State, p.Reason)
	}
	if n := countCalls(f.ev.callsFor(f.wt), lc.Name); n != 3 {
		t.Fatalf("local replays %d, want the next gate to replay the second alone", n)
	}
}

// countCalls counts the replays of case name among calls.
func countCalls(calls []evalCall, name string) int {
	n := 0
	for _, c := range calls {
		if c.Case == name {
			n++
		}
	}
	return n
}

// A local replay that ends without a score on a launch failure shared across PRs is a CLI's outage, a
// delay as for a posted case, whatever the outcome it ended with.
func TestALocalReplayOnASharedLaunchFailureIsADelay(t *testing.T) {
	f, _, lc := localGateFixture(t, gateFullFiles, found(nil, "q"))
	f.ev.failOn(f.wt, lc.Name, eval.OutcomeMissingRole, bootstrapLine)
	clearedLaunch(f.h, 20, "codex-judge", bootstrapLine)
	f.h.tick()
	if p := f.proposal(); p.State != store.LearnGating || len(f.h.events("learn.gate_waiting")) != 1 {
		t.Fatalf("proposal %s %q, want gating on a delay", p.State, p.Reason)
	}
	if s, ok := f.h.e.gateStopsOf(f.h.ctx, f.p.ID); ok && (s.Stops != 0 || s.Attempts != 0) {
		t.Fatalf("stops %+v: the outage counted", s)
	}
}

// What the gate makes of the local cases' corpus never stops it: a corpus that cannot be read leaves
// the local cases out, saying why (GateResult.Local), and the gate decides on the posted cases; a case
// whose name the corpus has, a posted case in the local file and a case evalrun.Refused names (a task
// Codex must not see again) are left out, each named.
func TestTheLocalCasesCorpusLeavesOutWhatTheGateCannotTake(t *testing.T) {
	f, seams, _ := localGateFixture(t, gateFullFiles, found([]string{"q"}))
	seams.err = errors.New("learn/local/cases.toml: line 3: unknown key")
	f.h.tick()
	g := f.gate()
	if p := f.proposal(); p.State != store.LearnPassed || !strings.Contains(g.Local, "cannot be read") || !strings.Contains(g.Local, "unknown key") {
		t.Fatalf("proposal %s %q, local %q", p.State, p.Reason, g.Local)
	}
	if slices.ContainsFunc(g.Cases, func(c GateCase) bool { return c.Local }) {
		t.Fatalf("rows %+v", g.Cases)
	}

	f2, seams2, lc := localGateFixture(t, gateFullFiles, found([]string{"q"}))
	seams2.corpus.Cases = append(seams2.corpus.Cases,
		eval.Case{Name: f2.target.Name, Kind: eval.KindLocal, PR: "talkable/talkable#900021", Head: retroShaA, Defects: lc.Defects},
		eval.Case{Name: "posted-here", PR: "talkable/talkable#30", Head: retroShaA, Defects: lc.Defects})
	seams2.refused = map[string]string{lc.Name: "Codex never works on task 1 again"}
	f2.h.tick()
	g = f2.gate()
	for _, want := range []string{f2.target.Name, "posted-here", lc.Name, "Codex never works on task 1 again"} {
		if !strings.Contains(g.Local, want) {
			t.Errorf("local %q lacks %q", g.Local, want)
		}
	}
	if p := f2.proposal(); p.State != store.LearnPassed || slices.ContainsFunc(g.Cases, func(c GateCase) bool { return c.Local }) {
		t.Fatalf("proposal %s %q, rows %+v", p.State, p.Reason, g.Cases)
	}
}

// No local case takes a place in a gate whose daemon does not read them (no seam), nor in one whose
// candidate changes nothing a local round reads (the same local inputs as the base's: the round would
// read what it read at the base), which says so.
func TestAGateTakesNoLocalCaseItCannotTellApart(t *testing.T) {
	f, seams, lc := localGateFixture(t, gateFullFiles, found([]string{"q"}))
	seams.inputs[f.wt] = seams.inputs[f.base]
	f.h.tick()
	g := f.gate()
	if p := f.proposal(); p.State != store.LearnPassed || !strings.Contains(g.Local, "nothing a local round reads") ||
		slices.ContainsFunc(g.Cases, func(c GateCase) bool { return c.Local }) || countCalls(f.ev.callsFor(f.wt), lc.Name) != 0 {
		t.Fatalf("proposal %s %q, local %q, rows %+v", p.State, p.Reason, g.Local, g.Cases)
	}

	f2, _, lc2 := localGateFixture(t, gateFullFiles, found([]string{"q"}))
	f2.h.e.d.EvalLocalCorpus = nil
	f2.h.tick()
	if g := f2.gate(); g.Local != "" || slices.ContainsFunc(g.Cases, func(c GateCase) bool { return c.Local }) || countCalls(f2.ev.callsFor(f2.wt), lc2.Name) != 0 {
		t.Fatalf("local %q, rows %+v", g.Local, g.Cases)
	}
}
