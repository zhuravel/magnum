package learn

// The gate of a skill proposal (DECISIONS "The gate replays the learned
// cases"): the eval cases a proposal is scored on (SelectCases: the learned
// cases of the inputs it noted, its targets, then up to [learn] gate_guards
// guards, cases whose newest replay found a defect, then the newest other
// cases that can be replayed, the targets and the others up to [learn]
// gate_cases), whether the judge alone replays them (Mode: only the judge
// skill and the judge prompts changed) and what the replays say against the
// baseline's (Judge: passed, held or failed). The engine runs the replays
// (internal/engine/learn_gate.go).

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/store"
)

// The gate's verdicts (Verdict.State), the proposal's next state.
const (
	GatePassed = store.LearnPassed // it lands
	GateHeld   = store.LearnHeld   // the operator decides
	GateFailed = store.LearnFailed // it goes
)

// The gate's modes (Mode).
const (
	GateJudgeOnly = "judge_only" // the judge alone, on a full replay's reviewer reports
	GateFull      = "full"       // every role
)

// Why SelectCases leaves a target out, and why the gate does once it has
// the target's baseline (AtBaseline).
const (
	SkipNotInCorpus = "not in the corpus"
	SkipFlagged     = "flagged: never replayed"
	SkipNotWatched  = "not watched"
	// SkipFoundAtBaseline and SkipNotReportedAtBaseline are a target the
	// baseline covers already: its input retires (store.LearnTargetBaseline).
	SkipFoundAtBaseline       = store.LearnDismissedFound
	SkipNotReportedAtBaseline = store.LearnDismissedNotReported
	// SkipLostAtBase is a guard the candidate lost a defect of on both
	// replays that the base lost too (GateRow.AtBase): the loss is not the
	// candidate's.
	SkipLostAtBase = "lost at the base too"
)

// overCapPrefix opens skipOverCap's reason.
const overCapPrefix = "past the gate's "

func skipOverCap(limit int) string { return fmt.Sprintf(overCapPrefix+"%d cases", limit) }

// AtBaseline is why target name is no target once before, its baseline's
// score, is known ("" = it is one): a miss- case whose every defect the
// baseline found (SkipFoundAtBaseline) or a decline- case whose declined
// finding the baseline no longer reports (SkipNotReportedAtBaseline):
// nothing is left to improve.
func AtBaseline(name string, before *eval.Score) string {
	switch {
	case before == nil:
		return ""
	case strings.HasPrefix(name, store.LearnInputMiss+"-") && before.Total > 0 && before.Found == before.Total:
		return SkipFoundAtBaseline
	case strings.HasPrefix(name, store.LearnInputDecline+"-") && len(before.AbsentHits) == 0:
		return SkipNotReportedAtBaseline
	}
	return ""
}

// atBaseline reports whether skip says the baseline covers the target.
func atBaseline(skip string) bool {
	return skip == SkipFoundAtBaseline || skip == SkipNotReportedAtBaseline
}

// PromoteTarget gives the place of the target at cases[after], which the
// baseline covers, to the first target after it that SelectCases left out
// over its cap, which becomes runnable; false when there is none.
func PromoteTarget(cases []GateCase, after int) bool {
	for i := after + 1; i < len(cases); i++ {
		if cases[i].Target && strings.HasPrefix(cases[i].Skip, overCapPrefix) {
			cases[i].Skip = ""
			return true
		}
	}
	return false
}

// TargetName is the learned case of a proposal's input of kind
// (store.LearnInputMiss, store.LearnInputDecline) and id: miss-<id>
// (CaseFromMiss) or decline-<id> (CaseFromDecline).
func TargetName(kind string, id int64) string { return fmt.Sprintf("%s-%d", kind, id) }

// GateCase is a case the gate replays, or a target it cannot (Skip says
// why; Case has the target's name alone when the corpus lacks it). Guard
// marks a guard place (Guards).
type GateCase struct {
	Case   eval.Case
	Target bool
	Guard  bool
	Skip   string
}

// Guards are a gate's guard places (SelectCases): up to N cases besides the
// targets whose newest stored replay found a defect (Found), the least
// recently replayed first (Last; zero = never, a nil Last orders none), so
// successive gates take turns through them: a gate always has cases whose
// baseline found something the candidate can lose, which its targets
// (their baseline finds nothing yet) and the newest others (mostly declined
// findings, with nothing to find) seldom are. N = 0 or a nil Found gives
// none.
type Guards struct {
	N     int
	Found func(c eval.Case) bool
	Last  func(c eval.Case) time.Time
}

// GuardsFrom are n guard places over the stored runs (oldest first:
// eval.ListRuns), which the gate and `magnum eval guards` both take: a case
// guards when its newest scored replay at its head (eval.NewestReplay)
// found a defect, the runs with inputs skip left out (the gate's
// candidate's own, so a gate that stops short after its candidate lost a
// guard still guards it; "" = none), and is ordered by its newest such
// replay among the runs started before since (zero = every run; the gate's
// proposal's creation, so every gate of one proposal takes the same guards
// whatever it replayed of them).
func GuardsFrom(runs []eval.Run, n int, skip string, since time.Time) Guards {
	older := runs
	if !since.IsZero() {
		older = slices.DeleteFunc(slices.Clone(runs), func(r eval.Run) bool { return !r.Started.Before(since) })
	}
	return Guards{N: n,
		Found: func(c eval.Case) bool {
			r, ok := eval.NewestReplay(runs, c, skip)
			return ok && r.Case.Score.Found > 0
		},
		Last: func(c eval.Case) time.Time {
			r, _ := eval.NewestReplay(older, c, skip)
			return r.At
		},
	}
}

// pick is g's guard places among cases, none of them seen (a target)
// or skipped (skip: a flagged or unwatched PR), which it marks seen.
func (g Guards) pick(cases []eval.Case, seen map[string]bool, skip func(eval.Case) string) []GateCase {
	if g.N <= 0 || g.Found == nil {
		return nil
	}
	type candidate struct {
		c    eval.Case
		last time.Time
	}
	var cands []candidate
	for _, c := range cases {
		if seen[c.Name] || skip(c) != "" || !g.Found(c) {
			continue
		}
		cand := candidate{c: c}
		if g.Last != nil {
			cand.last = g.Last(c)
		}
		cands = append(cands, cand)
	}
	slices.SortStableFunc(cands, func(a, b candidate) int { return a.last.Compare(b.last) })
	var out []GateCase
	for _, cand := range cands[:min(g.N, len(cands))] {
		seen[cand.c.Name] = true
		out = append(out, GateCase{Case: cand.c, Guard: true})
	}
	return out
}

// SelectCases picks a proposal's cases from corpus (nil = none): every
// target, in its order, first (one the corpus lacks, whose PR is flagged
// or in no watch is skipped with why, and one past limit too), then the
// guard places (guards, on top of limit, so targets never crowd them out;
// a guard is never a target), then the newest other cases (the corpus's
// last first: the learned ones come after the operator's, oldest first)
// whose PR is watched and not flagged, until limit targets and others can
// run. A flagged PR is never replayed, watched or not.
func SelectCases(corpus *eval.Corpus, targets []string, watched func(pr string) bool, flagged func(pr string) bool, limit int, guards Guards) []GateCase {
	var cases []eval.Case
	if corpus != nil {
		cases = corpus.Cases
	}
	skip := func(c eval.Case) string {
		switch {
		case flagged != nil && flagged(c.PR):
			return SkipFlagged
		case watched != nil && !watched(c.PR):
			return SkipNotWatched
		}
		return ""
	}
	var out []GateCase
	runnable := 0
	seen := map[string]bool{}
	for _, name := range targets {
		if seen[name] {
			continue
		}
		seen[name] = true
		g := GateCase{Case: eval.Case{Name: name}, Target: true, Skip: SkipNotInCorpus}
		if i := slices.IndexFunc(cases, func(c eval.Case) bool { return c.Name == name }); i >= 0 {
			g.Case, g.Skip = cases[i], skip(cases[i])
		}
		if g.Skip == "" && runnable >= limit {
			g.Skip = skipOverCap(limit)
		}
		if g.Skip == "" {
			runnable++
		}
		out = append(out, g)
	}
	out = append(out, guards.pick(cases, seen, skip)...)
	for _, c := range slices.Backward(cases) {
		if runnable >= limit {
			break
		}
		if seen[c.Name] || skip(c) != "" {
			continue
		}
		seen[c.Name] = true
		out = append(out, GateCase{Case: c})
		runnable++
	}
	return out
}

// Mode is how the gate replays a proposal that changes files: the judge
// alone (GateJudgeOnly) when the only files a review reads that it changes
// are the judge skill and the judge prompts (prompts/judge-*.md), every
// role (GateFull) when it changes anything else; the design log and the
// golden files the format gate regenerated change no review.
func Mode(files []string) string {
	for _, f := range files {
		judgePrompt, _ := path.Match("prompts/judge-*.md", f)
		golden := strings.HasPrefix(f, "internal/agents/testdata/") && strings.HasSuffix(f, ".golden")
		if f != SkillFile && !judgePrompt && f != DecisionsFile && !golden {
			return GateFull
		}
	}
	return GateJudgeOnly
}

// GateRow is a case of the gate: its score at the baseline (the newest
// replay with master's prompts and skill) and with the candidate's, or why
// it was not compared (Skip). Retried is set once the caller replayed a
// lost case again, After then being the second replay's: only then does a
// loss count. A guard (Guard) keeps the newest stored replay as its
// baseline (Stored), of whatever inputs, while the candidate finds what that
// replay found; one it lost twice takes the base's score (AtBase). A miss
// target won on one replay counts once a second finds it too (Confirmed,
// Confirm). Note says, for the operator, what a guard's baseline or a
// second replay was.
type GateRow struct {
	Case      string      `json:"case"`
	Target    bool        `json:"target,omitempty"`
	Guard     bool        `json:"guard,omitempty"`
	Before    *eval.Score `json:"before,omitempty"`
	After     *eval.Score `json:"after,omitempty"`
	Stored    bool        `json:"stored,omitempty"`
	Retried   bool        `json:"retried,omitempty"`
	Confirmed bool        `json:"confirmed,omitempty"`
	Skip      string      `json:"skip,omitempty"`
	Note      string      `json:"note,omitempty"`
}

// compared reports whether r has both scores and was not skipped.
func (r GateRow) compared() bool { return r.Skip == "" && r.Before != nil && r.After != nil }

// AtBase gives guard row r, whose candidate lost a defect of its stored
// baseline on both replays, the base's score as its baseline, and reports
// whether the base lost every such defect too: the row is then skipped
// (SkipLostAtBase), the loss not the candidate's; otherwise a defect the
// base finds and the candidate lost is a regression (Judge).
func (r *GateRow) AtBase(base *eval.Score) bool {
	r.Before, r.Stored = base, false
	if len(r.Lost()) > 0 {
		return false
	}
	r.Skip = SkipLostAtBase
	return true
}

// WonOnce reports whether target row r is a miss case its candidate's
// first replay found more of than the baseline: the gate replays it again
// (Confirm) before the win counts. A declined finding no longer reported is
// not replayed again.
func (r GateRow) WonOnce() bool {
	return r.Target && r.compared() && strings.HasPrefix(r.Case, store.LearnInputMiss+"-") && r.After.Found > r.Before.Found
}

// Confirm records the second candidate replay of row r (WonOnce): the win
// counts (Confirmed) only when second finds more than the baseline too.
func (r *GateRow) Confirm(second *eval.Score) {
	r.Confirmed = second != nil && r.Before != nil && second.Found > r.Before.Found
}

// Lost names the defects the baseline found that the candidate did not.
func (r GateRow) Lost() []string {
	if !r.compared() {
		return nil
	}
	var out []string
	for _, d := range r.Before.Defects {
		if d.Found && !slices.ContainsFunc(r.After.Defects, func(a eval.DefectResult) bool { return a.ID == d.ID && a.Found }) {
			out = append(out, d.ID)
		}
	}
	return out
}

// NewHits names the absent defects (findings authors declined) the
// candidate reported that the baseline did not.
func (r GateRow) NewHits() []string {
	if !r.compared() {
		return nil
	}
	var out []string
	for _, d := range r.After.AbsentHits {
		if !slices.ContainsFunc(r.Before.AbsentHits, func(b eval.DefectResult) bool { return b.ID == d.ID }) {
			out = append(out, d.ID)
		}
	}
	return out
}

// improved says how target row r improved ("" = it did not): a missed
// finding found more often on both replays (a miss- case, Confirmed), a
// declined finding reported less (a decline- case).
func (r GateRow) improved() string {
	if !r.Target || !r.compared() {
		return ""
	}
	switch {
	case r.WonOnce() && r.Confirmed:
		return fmt.Sprintf("%s found (%d → %d of %d) on two replays", r.Case, r.Before.Found, r.After.Found, r.After.Total)
	case strings.HasPrefix(r.Case, store.LearnInputDecline+"-") && len(r.After.AbsentHits) < len(r.Before.AbsentHits):
		return fmt.Sprintf("%s no longer reported (%d → %d)", r.Case, len(r.Before.AbsentHits), len(r.After.AbsentHits))
	}
	return ""
}

// TargetGate is what target row r says of its input's learned case
// (store.LearnTarget*): covered at the baseline, improved or not once
// compared; "" for a row that is no target or was not compared.
func (r GateRow) TargetGate() string {
	switch {
	case !r.Target:
		return ""
	case atBaseline(r.Skip):
		return store.LearnTargetBaseline
	case !r.compared():
		return ""
	case r.improved() != "":
		return store.LearnTargetImproved
	}
	return store.LearnTargetNotImproved
}

// Verdict is the gate's decision on a proposal: State (GatePassed,
// GateHeld, GateFailed), the Rule that decided it, as a sentence, and the
// rows it read.
type Verdict struct {
	State string    `json:"state"`
	Rule  string    `json:"rule"`
	Rows  []GateRow `json:"rows"`
}

// RuleOnlyDeclines opens the rule of a proposal the gate holds because the
// only targets it improved are declined findings (Judge).
const RuleOnlyDeclines = "only declined findings improved: review the rule it adds"

// Regressions names how rows regressed (none = they did not): a defect the
// baseline found and the candidate lost, once replayed again (Retried), or
// a declined finding the candidate reports and the baseline did not. A
// guard compared with a stored replay of other inputs (Stored) counts for
// neither: only the base's score may (AtBase).
func Regressions(rows []GateRow) []string {
	var out []string
	for _, r := range rows {
		if r.Stored {
			continue
		}
		if lost := r.Lost(); r.Retried && len(lost) > 0 {
			out = append(out, fmt.Sprintf("%s lost %s after a second replay", r.Case, strings.Join(lost, ", ")))
		}
		if hits := r.NewHits(); len(hits) > 0 {
			out = append(out, fmt.Sprintf("%s reports the declined %s the baseline did not (a new hit)", r.Case, strings.Join(hits, ", ")))
		}
	}
	return out
}

// Judge decides a proposal on its rows. It fails one that regressed
// (Regressions); else it passes one whose targets include a miss that
// improved on both replays (improved), and holds one whose improved
// targets are all declines (RuleOnlyDeclines: one author's reply and one
// classifier's answer would otherwise remove a class of findings from every
// review unseen); a target the baseline covers (AtBaseline) is no target,
// so it fails one whose every target is so (nothing to improve); else it
// holds one none of whose targets could be compared, and fails the rest (no
// target improved, a miss its second replay did not find again included).
func Judge(rows []GateRow) Verdict {
	v := Verdict{Rows: rows}
	regressed := Regressions(rows)
	var better, unchanged, once, covered []string
	compared, targets, missBetter := 0, 0, false
	for _, r := range rows {
		switch {
		case r.Target && atBaseline(r.Skip):
			covered = append(covered, r.Case+": "+r.Skip)
		case r.Target:
			targets++
		}
		if !r.Target || !r.compared() {
			continue
		}
		compared++
		switch s := r.improved(); {
		case s != "":
			better = append(better, s)
			missBetter = missBetter || strings.HasPrefix(r.Case, store.LearnInputMiss+"-")
		case r.WonOnce():
			once = append(once, r.Case)
		default:
			unchanged = append(unchanged, r.Case)
		}
	}
	var notImproved []string
	if len(unchanged) > 0 {
		notImproved = append(notImproved, strings.Join(unchanged, ", ")+" as at the baseline")
	}
	if len(once) > 0 {
		notImproved = append(notImproved, strings.Join(once, ", ")+" found by the first replay only")
	}
	switch {
	case len(regressed) > 0:
		v.State, v.Rule = GateFailed, "regressed: "+strings.Join(regressed, "; ")
	case len(better) > 0 && !missBetter:
		v.State, v.Rule = GateHeld, RuleOnlyDeclines+" ("+strings.Join(better, "; ")+"; nothing lost, no new hit)"
	case len(better) > 0:
		v.State, v.Rule = GatePassed, strings.Join(better, "; ")+"; nothing lost, no new hit"
	case targets == 0 && len(covered) > 0:
		v.State, v.Rule = GateFailed, "nothing to improve: every target is covered at the baseline already ("+strings.Join(covered, "; ")+")"
	case targets == 0:
		v.State, v.Rule = GateHeld, "no input the proposal noted has a learned case to replay, and nothing regressed"
	case compared == 0:
		v.State, v.Rule = GateHeld, "no target could be replayed"+skipped(rows)+", and nothing regressed"
	default:
		v.State, v.Rule = GateFailed, "no target improved ("+strings.Join(notImproved, "; ")+")"
	}
	return v
}

// skipped is " (miss-3: not in the corpus; ...)" for the targets rows
// skips, "" for none.
func skipped(rows []GateRow) string {
	var out []string
	for _, r := range rows {
		if r.Target && r.Skip != "" {
			out = append(out, r.Case+": "+r.Skip)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return " (" + strings.Join(out, "; ") + ")"
}
