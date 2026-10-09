package learn

import (
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/store"
)

const gateHead = "0123456789abcdef0123456789abcdef01234567"

// gateCorpus is the operator's two cases, then the learned ones, oldest
// first, as eval.LoadCorpora returns them.
func gateCorpus() *eval.Corpus {
	c := func(name, pr string) eval.Case {
		return eval.Case{Name: name, PR: pr, Head: gateHead, Defects: []eval.Defect{{ID: name, Title: name, Paths: []string{"a.go"}}}}
	}
	return &eval.Corpus{Cases: []eval.Case{
		c("coupon-scope", "talkable/talkable#1"), c("old-race", "example/other#2"),
		c("miss-3", "talkable/talkable#3"), c("decline-4", "talkable/talkable#4"), c("miss-5", "example/flagged#5"),
		c("miss-6", "talkable/talkable#6"),
	}}
}

func names(cs []GateCase) []string {
	var out []string
	for _, c := range cs {
		s := c.Case.Name
		if c.Target {
			s += "*"
		}
		if c.Skip != "" {
			s += " (" + c.Skip + ")"
		}
		out = append(out, s)
	}
	return out
}

func watchedOnly(prs ...string) func(string) bool {
	return func(pr string) bool {
		return slices.ContainsFunc(prs, func(p string) bool { return strings.HasPrefix(pr, p) })
	}
}

func TestSelectCasesPutsTargetsFirstAndCapsTheRest(t *testing.T) {
	all := func(string) bool { return true }
	none := func(string) bool { return false }
	got := names(SelectCases(gateCorpus(), []string{"decline-4", TargetName(store.LearnInputMiss, 3)}, all, none, 4))
	// The targets first in their order, then the newest others (the end
	// of the corpus first) up to the cap.
	if want := []string{"decline-4*", "miss-3*", "miss-6", "miss-5"}; !slices.Equal(got, want) {
		t.Fatalf("cases = %q, want %q", got, want)
	}
	// Others that cannot run take no place: they are left out.
	got = names(SelectCases(gateCorpus(), []string{"miss-3"}, watchedOnly("talkable/"), none, 3))
	if want := []string{"miss-3*", "miss-6", "decline-4"}; !slices.Equal(got, want) {
		t.Fatalf("cases = %q, want %q", got, want)
	}
	// A target past the cap is listed, skipped, so the gate says it did not
	// run.
	got = names(SelectCases(gateCorpus(), []string{"miss-3", "decline-4"}, all, none, 1))
	if want := []string{"miss-3*", "decline-4* (" + skipOverCap(1) + ")"}; !slices.Equal(got, want) {
		t.Fatalf("cases = %q, want %q", got, want)
	}
	// No corpus: the targets alone, each skipped.
	got = names(SelectCases(nil, []string{"miss-3"}, all, none, 6))
	if want := []string{"miss-3* (" + SkipNotInCorpus + ")"}; !slices.Equal(got, want) {
		t.Fatalf("cases = %q, want %q", got, want)
	}
}

func TestSelectCasesSkipsAFlaggedOrUnwatchedTarget(t *testing.T) {
	flagged := func(pr string) bool { return strings.HasPrefix(pr, "example/flagged") }
	got := SelectCases(gateCorpus(), []string{"miss-5", "old-race", "miss-99", "miss-3"}, watchedOnly("talkable/", "example/flagged"), flagged, 6)
	want := []string{"miss-5* (" + SkipFlagged + ")", "old-race* (" + SkipNotWatched + ")", "miss-99* (" + SkipNotInCorpus + ")", "miss-3*",
		"miss-6", "decline-4", "coupon-scope"}
	if !slices.Equal(names(got), want) {
		t.Fatalf("cases = %q, want %q", names(got), want)
	}
	if got[2].Case.Name != "miss-99" || got[2].Case.PR != "" {
		t.Fatalf("a target not in the corpus = %+v, want its name alone", got[2].Case)
	}
	// A flagged PR in no watch is flagged first: it is never replayed.
	got = SelectCases(gateCorpus(), []string{"miss-5"}, func(string) bool { return false }, flagged, 6)
	if got[0].Skip != SkipFlagged {
		t.Fatalf("a flagged unwatched target is skipped %q, want %q", got[0].Skip, SkipFlagged)
	}
}

// score is a case's score: found of the defects ids, the absent hits.
func score(found []string, missed []string, hits ...string) *eval.Score {
	s := &eval.Score{}
	for _, id := range found {
		s.Defects = append(s.Defects, eval.DefectResult{ID: id, Found: true})
		s.Found++
		s.Total++
	}
	for _, id := range missed {
		s.Defects = append(s.Defects, eval.DefectResult{ID: id})
		s.Total++
	}
	for _, id := range hits {
		s.AbsentHits = append(s.AbsentHits, eval.DefectResult{ID: id, Found: true})
		s.Absent++
	}
	return s
}

func TestJudgePassesWhenATargetImprovesAndNothingIsLost(t *testing.T) {
	rows := []GateRow{
		{Case: "miss-3", Target: true, Before: score(nil, []string{"miss-3"}), After: score([]string{"miss-3"}, nil)},
		{Case: "coupon-scope", Before: score([]string{"a"}, []string{"b"}), After: score([]string{"a", "b"}, nil)},
	}
	v := Judge(rows)
	if v.State != GatePassed || !strings.Contains(v.Rule, "miss-3") || len(v.Rows) != 2 {
		t.Fatalf("verdict = %+v, want passed naming miss-3", v)
	}
	// A target that did not move improves nothing: the gate fails it.
	v = Judge([]GateRow{{Case: "miss-3", Target: true, Before: score(nil, []string{"miss-3"}), After: score(nil, []string{"miss-3"})}})
	if v.State != GateFailed || !strings.Contains(v.Rule, "no target improved") {
		t.Fatalf("verdict = %+v, want failed: no target improved", v)
	}
}

// A declined finding the candidate no longer reports is an improvement, but
// one the operator reviews when it is the only kind: one author's reply and
// one classifier's answer would otherwise remove a class of findings from
// every review unseen. With a missed finding found too, the proposal passes.
func TestJudgeHoldsAProposalThatImprovesOnlyDeclinedFindings(t *testing.T) {
	decline := GateRow{Case: "decline-4", Target: true, Before: score(nil, nil, "decline-4"), After: score(nil, nil)}
	unmoved := GateRow{Case: "miss-3", Target: true, Before: score(nil, []string{"miss-3"}), After: score(nil, []string{"miss-3"})}
	for name, rows := range map[string][]GateRow{"a decline alone": {decline}, "with a miss that did not move": {decline, unmoved}} {
		v := Judge(rows)
		if v.State != GateHeld || !strings.HasPrefix(v.Rule, "only declined findings improved: review the rule it adds") ||
			!strings.Contains(v.Rule, "decline-4 no longer reported") {
			t.Errorf("%s: verdict = %+v, want held naming decline-4", name, v)
		}
	}
	found := GateRow{Case: "miss-3", Target: true, Before: score(nil, []string{"miss-3"}), After: score([]string{"miss-3"}, nil)}
	if v := Judge([]GateRow{decline, found}); v.State != GatePassed || !strings.Contains(v.Rule, "decline-4") || !strings.Contains(v.Rule, "miss-3") {
		t.Fatalf("a miss found too: %+v, want passed naming both", v)
	}
	// A regression still fails it.
	hit := GateRow{Case: "coupon-scope", Before: score(nil, nil), After: score(nil, nil, "decline-9")}
	if v := Judge([]GateRow{decline, hit}); v.State != GateFailed {
		t.Fatalf("a decline improved and a new hit: %+v, want failed", v)
	}
}

// A target its baseline already covers (a miss the baseline finds every
// time, a declined finding it no longer reports) has nothing to improve:
// it is no target. A proposal whose every target is so fails (its inputs
// retire), and one with another target is judged on that one.
func TestATargetTheBaselineCoversIsNoTarget(t *testing.T) {
	for _, c := range []struct {
		name   string
		before *eval.Score
		want   string
	}{
		{"miss-3", score([]string{"miss-3"}, nil), SkipFoundAtBaseline},
		{"miss-3", score(nil, []string{"miss-3"}), ""},
		{"miss-3", &eval.Score{}, ""}, // no defect: nothing it found
		{"decline-4", score(nil, nil), SkipNotReportedAtBaseline},
		{"decline-4", score(nil, nil, "decline-4"), ""},
		{"coupon-scope", score([]string{"a"}, nil), ""}, // the operator's case is never a learned target
		{"miss-3", nil, ""},
	} {
		if got := AtBaseline(c.name, c.before); got != c.want {
			t.Errorf("AtBaseline(%s, %+v) = %q, want %q", c.name, c.before, got, c.want)
		}
	}
	covered := GateRow{Case: "miss-3", Target: true, Before: score([]string{"miss-3"}, nil), Skip: SkipFoundAtBaseline}
	declined := GateRow{Case: "decline-4", Target: true, Before: score(nil, nil), Skip: SkipNotReportedAtBaseline}
	found := GateRow{Case: "miss-6", Target: true, Before: score(nil, []string{"miss-6"}), After: score([]string{"miss-6"}, nil)}
	unmoved := GateRow{Case: "miss-6", Target: true, Before: score(nil, []string{"miss-6"}), After: score(nil, []string{"miss-6"})}
	other := GateRow{Case: "coupon-scope", Before: score([]string{"a"}, nil), After: score([]string{"a"}, nil)}
	if v := Judge([]GateRow{covered, declined, other}); v.State != GateFailed || !strings.Contains(v.Rule, "at the baseline") ||
		!strings.Contains(v.Rule, "miss-3") || !strings.Contains(v.Rule, "decline-4") {
		t.Fatalf("every target covered at the baseline: %+v, want failed naming them", v)
	}
	if v := Judge([]GateRow{covered, found, other}); v.State != GatePassed || !strings.Contains(v.Rule, "miss-6 found") {
		t.Fatalf("a covered target and one found: %+v, want passed on miss-6", v)
	}
	if v := Judge([]GateRow{covered, unmoved}); v.State != GateFailed || !strings.Contains(v.Rule, "no target improved (miss-6") {
		t.Fatalf("a covered target and one unmoved: %+v, want failed on miss-6", v)
	}
	if v := Judge([]GateRow{covered, {Case: "miss-7", Target: true, Skip: SkipNotInCorpus}}); v.State != GateHeld {
		t.Fatalf("a covered target and one not replayed: %+v, want held", v)
	}
	for _, c := range []struct {
		row  GateRow
		want string
	}{{covered, store.LearnTargetBaseline}, {declined, store.LearnTargetBaseline}, {found, store.LearnTargetImproved},
		{unmoved, store.LearnTargetNotImproved}, {other, ""}, {GateRow{Case: "miss-7", Target: true, Skip: SkipNotInCorpus}, ""}} {
		if got := c.row.TargetGate(); got != c.want {
			t.Errorf("TargetGate(%s %q) = %q, want %q", c.row.Case, c.row.Skip, got, c.want)
		}
	}
}

// A target the baseline covers gives its place among the gate's cases to
// the first target SelectCases left out over its cap.
func TestPromoteTargetGivesACoveredTargetsPlaceToTheNext(t *testing.T) {
	all, none := func(string) bool { return true }, func(string) bool { return false }
	cases := SelectCases(gateCorpus(), []string{"miss-3", "decline-4", "miss-6"}, all, none, 1)
	if got := names(cases); !slices.Equal(got, []string{"miss-3*", "decline-4* (" + skipOverCap(1) + ")", "miss-6* (" + skipOverCap(1) + ")"}) {
		t.Fatalf("cases = %q", got)
	}
	if !PromoteTarget(cases, 0) || cases[1].Skip != "" || cases[2].Skip == "" {
		t.Fatalf("after promoting: %q, want decline-4 runnable, miss-6 still over the cap", names(cases))
	}
	if !PromoteTarget(cases, 1) || cases[2].Skip != "" {
		t.Fatalf("after a second promotion: %q", names(cases))
	}
	if PromoteTarget(cases, 2) {
		t.Fatal("a promotion with no target over the cap left")
	}
}

func TestJudgeFailsALostCaseOnlyAfterARetry(t *testing.T) {
	target := GateRow{Case: "miss-3", Target: true, Before: score(nil, []string{"miss-3"}), After: score([]string{"miss-3"}, nil)}
	lost := GateRow{Case: "coupon-scope", Before: score([]string{"a", "b"}, nil), After: score([]string{"a"}, []string{"b"})}
	if got := lost.Lost(); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("Lost() = %q, want [b]", got)
	}
	if v := Judge([]GateRow{target, lost}); v.State != GatePassed {
		t.Fatalf("a loss not replayed again counts: %+v", v)
	}
	lost.Retried = true
	v := Judge([]GateRow{target, lost})
	if v.State != GateFailed || !strings.Contains(v.Rule, "coupon-scope") || !strings.Contains(v.Rule, "lost") {
		t.Fatalf("verdict = %+v, want failed: coupon-scope lost", v)
	}
	// A retry that found it again is no loss.
	lost.After = score([]string{"a", "b"}, nil)
	if v := Judge([]GateRow{target, lost}); v.State != GatePassed || len(lost.Lost()) != 0 {
		t.Fatalf("a retry that found it again: %+v", v)
	}
}

func TestJudgeFailsANewAbsentHit(t *testing.T) {
	target := GateRow{Case: "miss-3", Target: true, Before: score(nil, []string{"miss-3"}), After: score([]string{"miss-3"}, nil)}
	hit := GateRow{Case: "decline-4", Before: score(nil, nil), After: score(nil, nil, "decline-4")}
	if got := hit.NewHits(); !slices.Equal(got, []string{"decline-4"}) {
		t.Fatalf("NewHits() = %q", got)
	}
	v := Judge([]GateRow{target, hit})
	if v.State != GateFailed || !strings.Contains(v.Rule, "decline-4") || !strings.Contains(v.Rule, "hit") {
		t.Fatalf("verdict = %+v, want failed: decline-4 hit", v)
	}
	// A hit the baseline had already is no regression.
	hit.Before = score(nil, nil, "decline-4")
	if v := Judge([]GateRow{target, hit}); v.State != GatePassed {
		t.Fatalf("a hit the baseline had: %+v", v)
	}
}

func TestJudgeHoldsWhenNoTargetCouldRun(t *testing.T) {
	rows := []GateRow{
		{Case: "miss-3", Target: true, Skip: "no full replay"},
		{Case: "miss-5", Target: true, Skip: SkipFlagged},
		{Case: "coupon-scope", Before: score([]string{"a"}, nil), After: score([]string{"a"}, nil)},
	}
	v := Judge(rows)
	if v.State != GateHeld || !strings.Contains(v.Rule, "no target") {
		t.Fatalf("verdict = %+v, want held: no target could be replayed", v)
	}
	// No target at all (the inputs it noted make no case) holds too.
	if v := Judge(rows[2:]); v.State != GateHeld || !strings.Contains(v.Rule, "no input") {
		t.Fatalf("no target: %+v", v)
	}
	// A regression fails it whatever the targets did.
	rows[2].After, rows[2].Retried = score(nil, []string{"a"}), true
	if v := Judge(rows); v.State != GateFailed {
		t.Fatalf("a regression with no target replayed: %+v", v)
	}
}

func TestModeIsJudgeOnlyForSkillAndJudgePromptEdits(t *testing.T) {
	for _, c := range []struct {
		files []string
		want  string
	}{
		{[]string{DecisionsFile, SkillFile}, GateJudgeOnly},
		{[]string{DecisionsFile, "internal/agents/testdata/judge_initial.golden", "prompts/judge-initial.md", SkillFile}, GateJudgeOnly},
		{[]string{DecisionsFile, "prompts/claude-review.md", SkillFile}, GateFull},
		{[]string{"prompts/judge-initial.md", "prompts/retro.md"}, GateFull},
		{[]string{"internal/engine/engine.go"}, GateFull},
		{[]string{"prompts/judge-initial.md.bak"}, GateFull},
	} {
		if got := Mode(c.files); got != c.want {
			t.Errorf("Mode(%q) = %s, want %s", c.files, got, c.want)
		}
	}
}
