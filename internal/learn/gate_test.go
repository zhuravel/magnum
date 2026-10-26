package learn

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/prompts"
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
		if c.Guard {
			s += "+"
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
	got := names(SelectCases(gateCorpus(), []string{"decline-4", TargetName(store.LearnInputMiss, 3)}, all, none, 4, Guards{}))
	// The targets first in their order, then the newest others (the end
	// of the corpus first) up to the cap.
	if want := []string{"decline-4*", "miss-3*", "miss-6", "miss-5"}; !slices.Equal(got, want) {
		t.Fatalf("cases = %q, want %q", got, want)
	}
	// Others that cannot run take no place: they are left out.
	got = names(SelectCases(gateCorpus(), []string{"miss-3"}, watchedOnly("talkable/"), none, 3, Guards{}))
	if want := []string{"miss-3*", "miss-6", "decline-4"}; !slices.Equal(got, want) {
		t.Fatalf("cases = %q, want %q", got, want)
	}
	// A target past the cap is listed, skipped, so the gate says it did not
	// run.
	got = names(SelectCases(gateCorpus(), []string{"miss-3", "decline-4"}, all, none, 1, Guards{}))
	if want := []string{"miss-3*", "decline-4* (" + skipOverCap(1) + ")"}; !slices.Equal(got, want) {
		t.Fatalf("cases = %q, want %q", got, want)
	}
	// No corpus: the targets alone, each skipped.
	got = names(SelectCases(nil, []string{"miss-3"}, all, none, 6, Guards{}))
	if want := []string{"miss-3* (" + SkipNotInCorpus + ")"}; !slices.Equal(got, want) {
		t.Fatalf("cases = %q, want %q", got, want)
	}
	// Guards come after the targets and before the newest others, on top of
	// the cap: the targets and the others still take up to 4 places. A target
	// is never a guard.
	guards := Guards{N: 2, Found: func(c eval.Case) bool { return c.Name == "coupon-scope" || c.Name == "miss-3" }}
	got = names(SelectCases(gateCorpus(), []string{"decline-4", "miss-3"}, all, none, 4, guards))
	if want := []string{"decline-4*", "miss-3*", "coupon-scope+", "miss-6", "miss-5"}; !slices.Equal(got, want) {
		t.Fatalf("cases with guards = %q, want %q", got, want)
	}
}

// guardCorpus is the operator's two cases of gateCorpus, then n learned
// miss cases, miss-1 to miss-n.
func guardCorpus(n int) *eval.Corpus {
	c := gateCorpus()
	c.Cases = c.Cases[:2] // coupon-scope, old-race
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("miss-%d", i)
		c.Cases = append(c.Cases, eval.Case{Name: name, PR: fmt.Sprintf("talkable/talkable#%d", 100+i), Head: gateHead,
			Defects: []eval.Defect{{ID: name, Title: name, Paths: []string{"a.go"}}}})
	}
	return c
}

// Proposal 7 noted 32 inputs: 26 targets were left past gate_cases = 6 and
// all 6 compared rows were targets, none of which the baseline found, so no
// row could lose anything. The guard places come on top of gate_cases:
// targets never crowd them out, and gate_guards = 0 turns them off.
func TestSelectCasesReservesGuardPlacesPastTheTargets(t *testing.T) {
	all, none := func(string) bool { return true }, func(string) bool { return false }
	corpus := guardCorpus(34)
	var targets []string
	for i := 1; i <= 32; i++ {
		targets = append(targets, fmt.Sprintf("miss-%d", i))
	}
	// The operator's two cases and miss-33 found a defect at their newest
	// replay, miss-34 did not.
	found := func(c eval.Case) bool { return !strings.HasPrefix(c.Name, "miss-") || c.Name == "miss-33" }
	got := SelectCases(corpus, targets, all, none, 6, Guards{N: 2, Found: found})
	runnable, guards := 0, []string{}
	for i, c := range got {
		if c.Skip == "" {
			runnable++
		}
		if c.Guard {
			guards = append(guards, c.Case.Name)
			if c.Target || c.Skip != "" || i < len(targets) {
				t.Errorf("guard %s: %+v at %d, want a runnable case that is no target, after the %d targets", c.Case.Name, c, i, len(targets))
			}
		}
	}
	if runnable != 8 || !slices.Equal(guards, []string{"coupon-scope", "old-race"}) {
		t.Fatalf("cases = %q: %d runnable, guards %q; want the 6 targets under the cap and 2 guards", names(got), runnable, guards)
	}
	if over := slices.DeleteFunc(names(got), func(s string) bool { return !strings.Contains(s, skipOverCap(6)) }); len(over) != 26 {
		t.Fatalf("targets past the cap: %d, want 26", len(over))
	}
	if got := SelectCases(corpus, targets, all, none, 6, Guards{Found: found}); slices.ContainsFunc(got, func(c GateCase) bool { return c.Guard }) {
		t.Fatalf("gate_guards = 0: %q", names(got))
	}
}

// replayOf is a stored run, started at at with inputs, of case c that found
// found of its one defect.
func replayOf(id, inputs string, at time.Time, c eval.Case, found int) eval.Run {
	s := &eval.Score{Case: c.Name, Found: found, Total: 1, Defects: []eval.DefectResult{{ID: c.Name, Found: found > 0}}}
	return eval.Run{ID: id, Inputs: inputs, Started: at, Cases: []eval.CaseRun{{Case: c.Name, PR: c.PR, Head: c.Head, Outcome: "dry_run", Score: s}}}
}

// A guard is a case whose newest stored replay found a defect; the gate
// takes the least recently replayed first, so successive gates take turns
// through them. A case whose newest replay found nothing, a flagged one and
// a target are no guard. The candidate's own replays are left out (a gate
// that stopped short after its candidate lost a guard keeps it), and so are
// the replays since the proposal was made from the order (every gate of one
// proposal takes the same guards).
func TestSelectCasesRotatesGuardsLeastRecentlyReplayedFirst(t *testing.T) {
	corpus := gateCorpus() // coupon-scope, old-race, miss-3, decline-4, miss-5, miss-6
	byName := func(name string) eval.Case {
		return corpus.Cases[slices.IndexFunc(corpus.Cases, func(c eval.Case) bool { return c.Name == name })]
	}
	day := func(d int) time.Time { return time.Date(2026, 10, d, 9, 0, 0, 0, time.UTC) }
	runs := []eval.Run{
		replayOf("20261001-090000", "in-a", day(1), byName("old-race"), 1),
		replayOf("20261002-090000", "in-a", day(2), byName("miss-6"), 1),
		replayOf("20261003-090000", "in-a", day(3), byName("coupon-scope"), 1),
		replayOf("20261004-090000", "in-a", day(4), byName("decline-4"), 0),
		replayOf("20261004-100000", "in-a", day(4), byName("miss-3"), 1), // a target
		replayOf("20261005-090000", "in-a", day(5), byName("miss-5"), 1), // flagged
	}
	pick := func(runs []eval.Run, skip string, since time.Time) []string {
		return guardNames(corpus, GuardsFrom(runs, 2, skip, since, ""))
	}
	if got := pick(runs, "", time.Time{}); !slices.Equal(got, []string{"old-race", "miss-6"}) {
		t.Fatalf("guards = %q, want the two least recently replayed: old-race, miss-6", got)
	}
	// The operator replays them; the next gate takes coupon-scope, then the
	// least recently replayed of the two (a gate's own replays of its guards:
	// TestSuccessiveGatesTakeTurnsThroughTheGuardsTheyReplayed).
	runs = append(runs, replayOf("20261006-090000", "in-b", day(6), byName("old-race"), 1),
		replayOf("20261006-100000", "in-b", day(6).Add(time.Hour), byName("miss-6"), 1))
	if got := pick(runs, "", time.Time{}); !slices.Equal(got, []string{"coupon-scope", "old-race"}) {
		t.Fatalf("the next gate's guards = %q, want coupon-scope, old-race", got)
	}
	// A newest replay that found nothing makes no guard, unless it is the
	// candidate's own.
	lost := append(slices.Clone(runs), replayOf("20261007-090000", "in-cand", day(7), byName("coupon-scope"), 0))
	if got := pick(lost, "", time.Time{}); !slices.Equal(got, []string{"old-race", "miss-6"}) {
		t.Fatalf("guards once coupon-scope's newest replay found nothing = %q, want old-race, miss-6", got)
	}
	if got := pick(lost, "in-cand", time.Time{}); !slices.Equal(got, []string{"coupon-scope", "old-race"}) {
		t.Fatalf("the candidate's own replay of coupon-scope took its guard place: %q", got)
	}
	// The replays since the proposal was made do not move the order.
	if got := pick(runs, "", day(6)); !slices.Equal(got, []string{"old-race", "miss-6"}) {
		t.Fatalf("guards of a proposal made before the 6th = %q, want those its first gate took", got)
	}
}

// guardNames are the guard places SelectCases gives a gate of corpus whose
// target is miss-3, of the watched talkable/ and example/ PRs, example/flagged
// flagged, with guards.
func guardNames(corpus *eval.Corpus, guards Guards) []string {
	flagged := func(pr string) bool { return strings.HasPrefix(pr, "example/flagged") }
	var out []string
	for _, c := range SelectCases(corpus, []string{"miss-3"}, watchedOnly("talkable/", "example/"), flagged, 1, guards) {
		if c.Guard {
			out = append(out, c.Case.Name)
		}
	}
	return out
}

// caseNamed is corpus's case named name.
func caseNamed(corpus *eval.Corpus, name string) eval.Case {
	return corpus.Cases[slices.IndexFunc(corpus.Cases, func(c eval.Case) bool { return c.Name == name })]
}

// Every gate of one proposal takes the same guards with the same baselines:
// GuardsFrom reads only the replays made before the proposal, for which
// cases guard as for their order. A replay since that found nothing (an
// improve run's eval-at.sh, the gate's own replay of the base) drops no
// guard, a case first replayed since takes no place, and a guard's baseline
// (Replay) stays the replay made before. `magnum eval guards`, with no
// proposal, reads every replay.
func TestAProposalsGatesTakeTheGuardsOfTheReplaysMadeBeforeIt(t *testing.T) {
	corpus := gateCorpus()
	day := func(d int) time.Time { return time.Date(2026, 10, d, 9, 0, 0, 0, time.UTC) }
	runs := []eval.Run{
		replayOf("20261001-090000", "in-a", day(1), caseNamed(corpus, "old-race"), 1),
		replayOf("20261003-090000", "in-a", day(3), caseNamed(corpus, "coupon-scope"), 1),
		// Since the proposal, made on the 6th.
		replayOf("20261007-090000", "in-x", day(7), caseNamed(corpus, "coupon-scope"), 0),
		replayOf("20261007-100000", "in-x", day(7), caseNamed(corpus, "miss-6"), 1),
	}
	g := GuardsFrom(runs, 2, "in-cand", day(6), "")
	if got := guardNames(corpus, g); !slices.Equal(got, []string{"old-race", "coupon-scope"}) {
		t.Fatalf("guards of the proposal made on the 6th = %q, want old-race, coupon-scope: the replays before it", got)
	}
	if r, ok := g.Replay(caseNamed(corpus, "coupon-scope")); !ok || r.Run != "20261003-090000" || r.Case.Score.Found != 1 {
		t.Fatalf("coupon-scope's baseline = %+v, %v; want the replay of the 3rd", r, ok)
	}
	if got := guardNames(corpus, GuardsFrom(runs, 2, "", time.Time{}, "")); !slices.Equal(got, []string{"old-race", "miss-6"}) {
		t.Fatalf("guards with no proposal = %q, want old-race, miss-6: every replay", got)
	}
}

// A guard that caught a regression stays a guard: proposal 1's base run was
// made at its first baseline replay (9:00), before its candidate's runs
// (10:00, 11:00) lost coupon-scope twice, and the base's replay of it at
// 12:00, which found it, went into that run. That replay is coupon-scope's
// newest, so proposal 2, made at 13:00, guards it again.
func TestAGuardWhoseBaseFoundWhatACandidateLostStaysAGuard(t *testing.T) {
	corpus := gateCorpus()
	at := func(h int) time.Time { return time.Date(2026, 10, 20, h, 0, 0, 0, time.UTC) }
	coupon, target := caseNamed(corpus, "coupon-scope"), caseNamed(corpus, "miss-3")
	base := eval.Run{ID: "20261020-090000", Inputs: "in-master", Started: at(9), Cases: []eval.CaseRun{
		replayOf("", "", time.Time{}, target, 0).Cases[0], replayOf("", "", time.Time{}, coupon, 1).Cases[0]}}
	base.Cases[0].Started, base.Cases[1].Started = at(9), at(12)
	runs := []eval.Run{
		replayOf("20261019-090000", "in-old", at(9).AddDate(0, 0, -1), coupon, 1),
		base,
		replayOf("20261020-100000", "in-p1", at(10), coupon, 0),
		replayOf("20261020-110000", "in-p1", at(11), coupon, 0),
	}
	g := GuardsFrom(runs, 2, "in-p2", at(13), "")
	if got := guardNames(corpus, g); !slices.Contains(got, "coupon-scope") {
		t.Fatalf("proposal 2's guards = %q, want coupon-scope: the base found it last", got)
	}
	if r, ok := g.Replay(coupon); !ok || r.Run != base.ID || !r.At.Equal(at(12)) {
		t.Fatalf("coupon-scope's baseline = %+v, %v; want the base's replay of 12:00", r, ok)
	}
}

// A guard comes only from replays not made by a gate's candidate: the
// operator's runs and the gates' replays of their base. A gate's replays
// of its candidate, of any proposal, read a skill that may never land, so
// they make no guard and no baseline: a case whose newest replay is another
// proposal's candidate replay that found a defect master's never did is no
// guard (the next proposal's candidate would "lose" it, and its base too),
// and one that found nothing, or a second replay that found nothing, leaves
// a guard on its older replay by the operator or by a base, ordered by its
// last replay, the candidate's included (miss-6's on the 3rd, coupon-scope's
// on the 4th).
func TestAGuardComesOnlyFromReplaysNotMadeByAGatesCandidate(t *testing.T) {
	corpus := gateCorpus()
	day := func(d int) time.Time { return time.Date(2026, 10, d, 9, 0, 0, 0, time.UTC) }
	labeled := func(r eval.Run, label string) eval.Run { r.Label = label; return r }
	race, coupon, six := caseNamed(corpus, "old-race"), caseNamed(corpus, "coupon-scope"), caseNamed(corpus, "miss-6")
	runs := []eval.Run{
		labeled(replayOf("20261001-090000", "in-master", day(1), race, 0), "before spec 01"),
		labeled(replayOf("20261001-100000", "in-master", day(1).Add(time.Hour), coupon, 1), ""),
		labeled(replayOf("20261002-090000", "in-master", day(2), six, 1), GateRunLabel(1, GateRunBase)),
		labeled(replayOf("20261003-090000", "in-p1", day(3), six, 0), GateRunLabel(1, GateRunSecond)),
		labeled(replayOf("20261004-090000", "in-p1", day(4), race, 1), GateRunLabel(1, GateRunCandidate)),
		labeled(replayOf("20261004-100000", "in-p1", day(4).Add(time.Hour), coupon, 0), GateRunLabel(1, GateRunCandidate)),
	}
	g := GuardsFrom(runs, 3, "in-p2", day(5), "")
	if got := guardNames(corpus, g); !slices.Equal(got, []string{"miss-6", "coupon-scope"}) {
		t.Fatalf("proposal 2's guards = %q, want miss-6 and coupon-scope on the base's and the operator's replays, "+
			"not old-race on proposal 1's candidate's", got)
	}
	for _, want := range []struct {
		c   eval.Case
		run string
	}{{coupon, "20261001-100000"}, {six, "20261002-090000"}, {race, "20261001-090000"}} {
		if r, ok := g.Replay(want.c); !ok || r.Run != want.run {
			t.Fatalf("%s's baseline = %+v, %v; want run %s", want.c.Name, r, ok, want.run)
		}
	}
	if got := guardNames(corpus, GuardsFrom(runs, 3, "", time.Time{}, "")); !slices.Equal(got, []string{"miss-6", "coupon-scope"}) {
		t.Fatalf("`magnum eval guards` = %q, want miss-6 and coupon-scope too", got)
	}
}

// Successive gates take turns through the guards: a gate replays its guards
// only as its candidate's first and second replays (the base only after the
// candidate lost one twice), and those replays, which make no guard and no
// baseline, still count as the case's last replay, so the next proposal's
// gate takes the guards replayed longest ago. Each guard's baseline stays
// the operator's replay.
func TestSuccessiveGatesTakeTurnsThroughTheGuardsTheyReplayed(t *testing.T) {
	corpus := gateCorpus()
	at := func(d, h int) time.Time { return time.Date(2026, 10, d, h, 0, 0, 0, time.UTC) }
	labeled := func(r eval.Run, label string) eval.Run { r.Label = label; return r }
	race, coupon, six := caseNamed(corpus, "old-race"), caseNamed(corpus, "coupon-scope"), caseNamed(corpus, "miss-6")
	runs := []eval.Run{
		replayOf("20261001-090000", "in-master", at(1, 9), race, 1),
		replayOf("20261002-090000", "in-master", at(2, 9), six, 1),
		replayOf("20261003-090000", "in-master", at(3, 9), coupon, 1),
	}
	gate := func(id int64, kind, runID string, when time.Time, c eval.Case, found int) eval.Run {
		return labeled(replayOf(runID, fmt.Sprintf("in-p%d", id), when, c, found), GateRunLabel(id, kind))
	}
	guards := func(since time.Time) []string {
		return guardNames(corpus, GuardsFrom(runs, 2, "in-next", since, ""))
	}
	if got := guards(at(4, 9)); !slices.Equal(got, []string{"old-race", "miss-6"}) {
		t.Fatalf("proposal 1's guards = %q, want old-race, miss-6", got)
	}
	// Proposal 1's gate on the 5th: the candidate finds old-race and loses
	// miss-6, which its second replay finds.
	candidate := gate(1, GateRunCandidate, "20261005-090000", at(5, 9), race, 1)
	candidate.Cases = append(candidate.Cases, gate(1, GateRunCandidate, "", time.Time{}, six, 0).Cases[0])
	candidate.Cases[1].Started = at(5, 10)
	runs = append(runs, candidate, gate(1, GateRunSecond, "20261005-110000", at(5, 11), six, 1))
	if got := guards(at(6, 9)); !slices.Equal(got, []string{"coupon-scope", "old-race"}) {
		t.Fatalf("proposal 2's guards = %q, want coupon-scope, old-race: proposal 1's gate replayed old-race and miss-6", got)
	}
	// Proposal 2's gate on the 7th: the candidate loses old-race and finds
	// coupon-scope, and its second replay finds old-race last.
	candidate = gate(2, GateRunCandidate, "20261007-090000", at(7, 9), race, 0)
	candidate.Cases = append(candidate.Cases, gate(2, GateRunCandidate, "", time.Time{}, coupon, 1).Cases[0])
	candidate.Cases[1].Started = at(7, 10)
	runs = append(runs, candidate, gate(2, GateRunSecond, "20261007-110000", at(7, 11), race, 1))
	g := GuardsFrom(runs, 2, "in-next", at(8, 9), "")
	if got := guardNames(corpus, g); !slices.Equal(got, []string{"miss-6", "coupon-scope"}) {
		t.Fatalf("proposal 3's guards = %q, want miss-6, coupon-scope: old-race's second replay came last", got)
	}
	for _, want := range []struct {
		c   eval.Case
		run string
	}{{race, "20261001-090000"}, {six, "20261002-090000"}, {coupon, "20261003-090000"}} {
		if r, ok := g.Replay(want.c); !ok || r.Run != want.run {
			t.Fatalf("%s's baseline = %+v, %v; want the operator's run %s", want.c.Name, r, ok, want.run)
		}
	}
}

// fullReplayOf is replayOf a full replay whose reviewers read reviewer, with
// its reports.
func fullReplayOf(id, reviewer string, at time.Time, c eval.Case, found int) eval.Run {
	r := replayOf(id, "in-"+id, at, c, found)
	r.ReviewerInputs, r.Cases[0].ReportDir = reviewer, "/eval/"+id+"/cases/"+c.Name
	return r
}

// A judge-only gate's candidate judge reads the reports of a full replay
// whose reviewers read the candidate's prompts, and the gate makes no full
// replay for a guard: a case without one made before the proposal is no
// guard of it, so its place goes to the next case that has one, and the
// guards a judge-only gate replays still take turns.
func TestAJudgeOnlyGateGuardsOnlyCasesWithAFullReplayOfItsReviewers(t *testing.T) {
	corpus := gateCorpus()
	day := func(d int) time.Time { return time.Date(2026, 10, d, 9, 0, 0, 0, time.UTC) }
	runs := []eval.Run{
		fullReplayOf("20261001-090000", "rev-old", day(1), caseNamed(corpus, "old-race"), 1),
		fullReplayOf("20261002-090000", "rev-cand", day(2), caseNamed(corpus, "miss-6"), 1),
		fullReplayOf("20261003-090000", "rev-cand", day(3), caseNamed(corpus, "coupon-scope"), 1),
		// Since the proposal, made on the 6th.
		fullReplayOf("20261007-090000", "rev-cand", day(7), caseNamed(corpus, "old-race"), 1),
	}
	if got := guardNames(corpus, GuardsFrom(runs, 2, "in-cand", day(6), "rev-cand")); !slices.Equal(got, []string{"miss-6", "coupon-scope"}) {
		t.Fatalf("judge-only guards = %q, want miss-6, coupon-scope: old-race had no full replay of the candidate's reviewers", got)
	}
	if got := guardNames(corpus, GuardsFrom(runs, 2, "in-cand", day(6), "")); !slices.Equal(got, []string{"old-race", "miss-6"}) {
		t.Fatalf("guards of every role = %q, want old-race, miss-6", got)
	}
}

func TestSelectCasesSkipsAFlaggedOrUnwatchedTarget(t *testing.T) {
	flagged := func(pr string) bool { return strings.HasPrefix(pr, "example/flagged") }
	got := SelectCases(gateCorpus(), []string{"miss-5", "old-race", "miss-99", "miss-3"}, watchedOnly("talkable/", "example/flagged"), flagged, 6, Guards{})
	want := []string{"miss-5* (" + SkipFlagged + ")", "old-race* (" + SkipNotWatched + ")", "miss-99* (" + SkipNotInCorpus + ")", "miss-3*",
		"miss-6", "decline-4", "coupon-scope"}
	if !slices.Equal(names(got), want) {
		t.Fatalf("cases = %q, want %q", names(got), want)
	}
	if got[2].Case.Name != "miss-99" || got[2].Case.PR != "" {
		t.Fatalf("a target not in the corpus = %+v, want its name alone", got[2].Case)
	}
	// A flagged PR in no watch is flagged first: it is never replayed.
	got = SelectCases(gateCorpus(), []string{"miss-5"}, func(string) bool { return false }, flagged, 6, Guards{})
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
		{Case: "miss-3", Target: true, Before: score(nil, []string{"miss-3"}), After: score([]string{"miss-3"}, nil), Confirmed: true},
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
	found := GateRow{Case: "miss-3", Target: true, Before: score(nil, []string{"miss-3"}), After: score([]string{"miss-3"}, nil), Confirmed: true}
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
	found := GateRow{Case: "miss-6", Target: true, Before: score(nil, []string{"miss-6"}), After: score([]string{"miss-6"}, nil), Confirmed: true}
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
	cases := SelectCases(gateCorpus(), []string{"miss-3", "decline-4", "miss-6"}, all, none, 1, Guards{})
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
	target := GateRow{Case: "miss-3", Target: true, Before: score(nil, []string{"miss-3"}), After: score([]string{"miss-3"}, nil), Confirmed: true}
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

// A guard's baseline is the newest stored replay (Stored), of whatever
// inputs, until the candidate lost one of its defects on both replays: that
// loss never counts against the stored replay (nor a declined finding it did
// not report), only against the base's score (AtBase). A defect the base
// finds and the candidate lost twice is a regression, as for any lost case.
func TestJudgeFailsAGuardTheCandidateLostTwiceAndTheBaseFinds(t *testing.T) {
	target := GateRow{Case: "miss-3", Target: true, Before: score(nil, []string{"miss-3"}), After: score([]string{"miss-3"}, nil), Confirmed: true}
	guard := GateRow{Case: "coupon-scope", Guard: true, Stored: true, Before: score([]string{"a"}, []string{"b"}),
		After: score(nil, []string{"a", "b"}, "decline-9"), Retried: true}
	if got := guard.Lost(); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("Lost() = %q, want [a]", got)
	}
	if v := Judge([]GateRow{target, guard}); v.State != GatePassed {
		t.Fatalf("a guard lost against the stored replay, before the base's score: %+v, want passed", v)
	}
	if lostAtBase := guard.AtBase(score([]string{"a", "b"}, nil)); lostAtBase || guard.Stored || guard.Skip != "" {
		t.Fatalf("AtBase = %v, row %+v: want the base's score as the baseline, compared", lostAtBase, guard)
	}
	v := Judge([]GateRow{target, guard})
	if v.State != GateFailed || !strings.Contains(v.Rule, "coupon-scope lost a, b after a second replay") ||
		!strings.Contains(v.Rule, "decline-9") {
		t.Fatalf("verdict = %+v, want failed: coupon-scope lost a and b, and a new hit", v)
	}
}

// A guard the candidate lost twice that the base loses too is not the
// candidate's loss: the row is skipped (SkipLostAtBase), so it is no
// regression, and the gate says the review at the base no longer finds it.
func TestAGuardTheBaseAlsoLosesIsSkippedNotARegression(t *testing.T) {
	target := GateRow{Case: "miss-3", Target: true, Before: score(nil, []string{"miss-3"}), After: score([]string{"miss-3"}, nil), Confirmed: true}
	guard := GateRow{Case: "coupon-scope", Guard: true, Stored: true, Before: score([]string{"a"}, []string{"b"}),
		After: score([]string{"b"}, []string{"a"}), Retried: true}
	if lostAtBase := guard.AtBase(score([]string{"b"}, []string{"a"})); !lostAtBase || guard.Skip != SkipLostAtBase || guard.Stored {
		t.Fatalf("AtBase = %v, row %+v: want it skipped as %q", lostAtBase, guard, SkipLostAtBase)
	}
	if v := Judge([]GateRow{target, guard}); v.State != GatePassed || len(guard.Lost()) != 0 {
		t.Fatalf("verdict = %+v, want passed on miss-3: the loss is not the candidate's", v)
	}
}

// A miss target the candidate found more of than the baseline is replayed
// once more: the win counts (Confirmed) only when the second replay finds
// more than the baseline too; otherwise the target did not improve, and the
// dismissal after two failed gates applies to its input. A declined finding
// no longer reported needs no second replay (the gate holds a proposal that
// improves only those).
func TestJudgePassesAMissWinOnlyWhenTheSecondReplayFindsItToo(t *testing.T) {
	won := GateRow{Case: "miss-3", Target: true, Before: score(nil, []string{"miss-3"}), After: score([]string{"miss-3"}, nil)}
	if !won.WonOnce() {
		t.Fatalf("a miss found by the first replay: WonOnce false: %+v", won)
	}
	if v := Judge([]GateRow{won}); v.State != GateFailed || !strings.Contains(v.Rule, "miss-3 found by the first replay only") {
		t.Fatalf("a win no second replay confirmed: %+v, want failed naming it", v)
	}
	// A gate that regressed elsewhere makes no second replay: a win nothing
	// checked again is recorded neither way, so it never counts toward the
	// dismissal after two failed gates.
	if got := won.TargetGate(); got != "" {
		t.Fatalf("a win no second replay checked: TargetGate %q, want none", got)
	}
	again := won
	again.Confirm(score([]string{"miss-3"}, nil))
	if v := Judge([]GateRow{again}); !again.Confirmed || v.State != GatePassed || !strings.Contains(v.Rule, "miss-3 found (0 → 1 of 1) on two replays") ||
		again.TargetGate() != store.LearnTargetImproved {
		t.Fatalf("a win the second replay found too: %+v (row %+v), want passed", v, again)
	}
	once := won
	once.Confirm(score(nil, []string{"miss-3"}))
	if v := Judge([]GateRow{once}); once.Confirmed || v.State != GateFailed || once.TargetGate() != store.LearnTargetNotImproved {
		t.Fatalf("a win the second replay did not find: %+v (row %+v), want failed, not improved", v, once)
	}
	decline := GateRow{Case: "decline-4", Target: true, Before: score(nil, nil, "decline-4"), After: score(nil, nil)}
	if decline.WonOnce() || (GateRow{Case: "coupon-scope", Before: score(nil, []string{"a"}), After: score([]string{"a"}, nil)}).WonOnce() {
		t.Fatal("a decline target or a case that is no target is replayed again for its win")
	}
	if v := Judge([]GateRow{decline}); v.State != GateHeld {
		t.Fatalf("a decline no longer reported: %+v, want held as before", v)
	}
}

func TestJudgeFailsANewAbsentHit(t *testing.T) {
	target := GateRow{Case: "miss-3", Target: true, Before: score(nil, []string{"miss-3"}), After: score([]string{"miss-3"}, nil), Confirmed: true}
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

// The judge prompts' partials file matches the editor's glob prompts/judge-*.md: the editor may
// change it, a change to it is listed like any judge prompt's, and the gate replays the judge alone
// for it, as its blocks reach only the judge prompts.
func TestTheEditorAndTheGateTreatThePartialsFileAsAJudgePrompt(t *testing.T) {
	file := path.Join("prompts", prompts.Shared)
	if !AllowedEditFile(file) {
		t.Fatalf("%s is not a file the editor may change", file)
	}
	if got := Mode([]string{DecisionsFile, file}); got != GateJudgeOnly {
		t.Errorf("Mode of an edit of %s = %s, want %s", file, got, GateJudgeOnly)
	}
	if got := Mode([]string{file, "prompts/claude-review.md"}); got != GateFull {
		t.Errorf("Mode of an edit of %s and a reviewer's prompt = %s, want %s", file, got, GateFull)
	}
	c, ch := goodEdit()
	c.Changed = append(c.Changed, file)
	ch.Files = append(ch.Files, FileChange{Name: file, Action: FileEdited, Reason: "the shared prepass paragraph"})
	if problems := ValidateEdit(c, ch); len(problems) != 0 {
		t.Errorf("an edit of %s listed as edited: %q", file, problems)
	}
}
