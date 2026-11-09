package learn

import (
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/eval"
)

// localGateCase is a local case of the corpus: a round of the factory's local review with a question
// defect, q.
func localGateCase() eval.Case {
	return eval.Case{Name: "task-1-round-1", Kind: eval.KindLocal, PR: "talkable/talkable#900011", Head: gateHead,
		Defects: []eval.Defect{{ID: "q", Title: "the plan's choice", Question: true, Match: []string{"x"}}}}
}

// localReplayOf is a stored run, started at at, whose replay of local case c recorded the inputs of its
// round (in; "" for a replay from before they were) and found found of its one defect.
func localReplayOf(id, label, in string, at time.Time, c eval.Case, found int) eval.Run {
	s := &eval.Score{Case: c.Name, Found: found, Total: 1, Defects: []eval.DefectResult{{ID: "q", Found: found > 0}}}
	return eval.Run{ID: id, Label: label, Inputs: "in-posted", Started: at,
		Cases: []eval.CaseRun{{Case: c.Name, Kind: eval.KindLocal, PR: c.PR, Head: c.Head, Outcome: "dry_run", Score: s, Inputs: in}}}
}

// A local case takes a guard place only (Guards.Local), the least recently replayed first as any
// guard, and never a target's or an other's: a target that names one is SkipLocal, and without
// Guards.Local (`magnum eval guards`) it takes no place at all.
func TestSelectCasesGivesALocalCaseAGuardPlaceOnly(t *testing.T) {
	corpus := gateCorpus()
	local := localGateCase()
	corpus.Cases = append(corpus.Cases, local)
	all := func(string) bool { return true }
	none := func(string) bool { return false }
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	guards := Guards{N: 1, Local: true, Found: func(eval.Case) bool { return true },
		Last: func(c eval.Case) time.Time {
			if c.Local() {
				return old
			}
			return old.Add(time.Hour)
		}}
	if got := SelectCases(corpus, []string{local.Name}, all, none, 3, guards); got[0].Skip != SkipLocal {
		t.Fatalf("a local target: %q, want it %q", names(got), SkipLocal)
	}
	got := SelectCases(corpus, nil, all, none, 3, guards)
	if !got[0].Guard || !got[0].Case.Local() {
		t.Fatalf("cases %q: want the local case, the least recently replayed, in the guard place", names(got))
	}
	for _, gc := range got[1:] {
		if gc.Case.Local() {
			t.Fatalf("cases %q: the local case took an other place", names(got))
		}
	}
	guards.Local = false
	if got := SelectCases(corpus, nil, all, none, 3, guards); slices.ContainsFunc(got, func(gc GateCase) bool { return gc.Case.Local() }) {
		t.Fatalf("cases %q: a local case without Guards.Local", names(got))
	}
}

// A local case guards on its newest scored replay that recorded its round's inputs and found a defect,
// not made by a gate's candidate (its "local candidate" and "local second" runs are a candidate's): in a
// judge-only gate too, which replays it with every role, so it needs no full replay of the candidate's
// reviewers, as a posted guard does. A replay from before a local replay recorded its round's inputs
// reviewed with the posted reviewer prompt, no local round's, and makes no guard.
func TestGuardsFromTakesALocalCaseOnItsRoundsReplaysWithoutAFullReplayOfTheReviewers(t *testing.T) {
	corpus := gateCorpus()
	local := localGateCase()
	corpus.Cases = append(corpus.Cases, local)
	day := func(d int) time.Time { return time.Date(2026, 10, d, 9, 0, 0, 0, time.UTC) }
	pick := func(runs []eval.Run, reviewers string) []string {
		g := GuardsFrom(runs, 3, "in-cand", day(9), reviewers)
		g.Local = true
		return guardNames(corpus, g)
	}
	posted := fullReplayOf("20261001-090000", "rev-cand", day(1), caseNamed(corpus, "coupon-scope"), 1)
	if got := pick([]eval.Run{posted, localReplayOf("20261002-090000", "", "local-a", day(2), local, 1)}, "rev-cand"); !slices.Equal(got, []string{"coupon-scope", local.Name}) {
		t.Fatalf("judge-only guards = %q, want coupon-scope and the local case", got)
	}
	if got := pick([]eval.Run{posted, localReplayOf("20261002-090000", "", "", day(2), local, 1)}, ""); !slices.Equal(got, []string{"coupon-scope"}) {
		t.Fatalf("guards = %q: a local replay with the posted prompt made a guard", got)
	}
	for _, kind := range []string{GateRunLocal, GateRunLocalSecond} {
		runs := []eval.Run{posted, localReplayOf("20261002-090000", "", "local-a", day(2), local, 0),
			localReplayOf("20261003-090000", GateRunLabel(3, kind), "local-b", day(3), local, 1)}
		if got := pick(runs, ""); !slices.Equal(got, []string{"coupon-scope"}) {
			t.Fatalf("guards = %q: a gate's %s replay made a guard", got, kind)
		}
	}
}

// lscore is a score of defects ids, found the ones in found.
func lscore(ids []string, found ...string) *eval.Score {
	s := &eval.Score{}
	for _, id := range ids {
		d := eval.DefectResult{ID: id, Found: slices.Contains(found, id)}
		if d.Found {
			s.Found++
		}
		s.Defects, s.Total = append(s.Defects, d), s.Total+1
	}
	return s
}

// A loss counts for a defect both candidate replays lost (First, the first replay's score the second
// replaced): two replays that each lost another defect lost none twice.
func TestALossCountsOnlyForADefectBothCandidateReplaysLost(t *testing.T) {
	ids := []string{"a", "b"}
	r := GateRow{Case: "c", Before: lscore(ids, "a", "b"), First: lscore(ids, "a"), After: lscore(ids, "b"), Retried: true}
	if got := Regressions([]GateRow{r}); len(got) != 0 || len(r.Lost()) != 0 {
		t.Fatalf("each defect lost once: %q, lost %q", got, r.Lost())
	}
	r.After = lscore(ids, "a")
	if got := Regressions([]GateRow{r}); len(got) != 1 || !slices.Equal(r.Lost(), []string{"b"}) {
		t.Fatalf("b lost twice: %q, lost %q", got, r.Lost())
	}
	if r := (GateRow{Case: "c", Before: lscore(ids, "a"), After: lscore(ids)}); !slices.Equal(r.Lost(), []string{"a"}) {
		t.Fatalf("a first replay's loss: %q", r.Lost())
	}
}

// The base's score of a local guard is two of its replays' (BothFound): a defect counts as found only
// when both found it, so one lucky replay of the base never makes the candidate's loss a regression.
func TestTheBaseScoreOfALocalGuardFindsOnlyWhatBothReplaysFound(t *testing.T) {
	ids := []string{"a", "b"}
	both := BothFound(lscore(ids, "a", "b"), lscore(ids, "a"))
	if both.Found != 1 || both.Total != 2 || !both.Defects[0].Found || both.Defects[1].Found {
		t.Fatalf("both = %+v", both)
	}
	r := GateRow{Case: "task-1-round-1", Guard: true, Local: true, Stored: true, Before: lscore(ids, "a", "b"), First: lscore(ids), After: lscore(ids), Retried: true}
	if r.AtBase(both) || !slices.Equal(r.Lost(), []string{"a"}) {
		t.Fatalf("at base: lost %q, skip %q", r.Lost(), r.Skip)
	}
	if BothFound(nil, lscore(ids)) != nil {
		t.Fatal("a missing score gave a base score")
	}
}
