package learn

// The gate of a skill proposal (DECISIONS "The gate replays the learned
// cases"): the eval cases a proposal is scored on (SelectCases: the learned
// cases of the inputs it noted, its targets, then the newest other cases
// that can be replayed, up to [learn] gate_cases), whether the judge alone
// replays them (Mode: only the judge skill and the judge prompts changed)
// and what the replays say against the baseline's (Judge: passed, held or
// failed). The engine runs the replays (internal/engine/learn_gate.go).

import (
	"fmt"
	"path"
	"slices"
	"strings"

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

// Why SelectCases leaves a target out.
const (
	SkipNotInCorpus = "not in the corpus"
	SkipFlagged     = "flagged: never replayed"
	SkipNotWatched  = "not watched"
)

func skipOverCap(limit int) string { return fmt.Sprintf("past the gate's %d cases", limit) }

// TargetName is the learned case of a proposal's input of kind
// (store.LearnInputMiss, store.LearnInputDecline) and id: miss-<id>
// (CaseFromMiss) or decline-<id> (CaseFromDecline).
func TargetName(kind string, id int64) string { return fmt.Sprintf("%s-%d", kind, id) }

// GateCase is a case the gate replays, or a target it cannot (Skip says
// why; Case has the target's name alone when the corpus lacks it).
type GateCase struct {
	Case   eval.Case
	Target bool
	Skip   string
}

// SelectCases picks a proposal's cases from corpus (nil = none): every
// target, in its order, first (one the corpus lacks, whose PR is flagged
// or in no watch is skipped with why, and one past limit too), then the
// newest other cases (the corpus's last first: the learned ones come
// after the operator's, oldest first) whose PR is watched and not flagged,
// until limit cases can run. A flagged PR is never replayed, watched or
// not.
func SelectCases(corpus *eval.Corpus, targets []string, watched func(pr string) bool, flagged func(pr string) bool, limit int) []GateCase {
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
// loss count.
type GateRow struct {
	Case    string      `json:"case"`
	Target  bool        `json:"target,omitempty"`
	Before  *eval.Score `json:"before,omitempty"`
	After   *eval.Score `json:"after,omitempty"`
	Retried bool        `json:"retried,omitempty"`
	Skip    string      `json:"skip,omitempty"`
}

// compared reports whether r has both scores and was not skipped.
func (r GateRow) compared() bool { return r.Skip == "" && r.Before != nil && r.After != nil }

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
// finding found more often (a miss- case), a declined finding reported
// less (a decline- case).
func (r GateRow) improved() string {
	if !r.Target || !r.compared() {
		return ""
	}
	switch {
	case strings.HasPrefix(r.Case, store.LearnInputMiss+"-") && r.After.Found > r.Before.Found:
		return fmt.Sprintf("%s found (%d → %d of %d)", r.Case, r.Before.Found, r.After.Found, r.After.Total)
	case strings.HasPrefix(r.Case, store.LearnInputDecline+"-") && len(r.After.AbsentHits) < len(r.Before.AbsentHits):
		return fmt.Sprintf("%s no longer reported (%d → %d)", r.Case, len(r.Before.AbsentHits), len(r.After.AbsentHits))
	}
	return ""
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

// Judge decides a proposal on its rows. It fails one that regressed: a
// defect the baseline found and the candidate lost, once replayed again
// (Retried), or a declined finding the candidate reports and the baseline
// did not; else it passes one whose targets include a miss that improved
// (improved), and holds one whose improved targets are all declines
// (RuleOnlyDeclines: one author's reply and one classifier's answer would
// otherwise remove a class of findings from every review unseen); else it
// holds one none of whose targets could be compared, and fails the rest
// (no target improved).
func Judge(rows []GateRow) Verdict {
	v := Verdict{Rows: rows}
	var regressed, better, unchanged []string
	compared, targets, missBetter := 0, 0, false
	for _, r := range rows {
		if r.Target {
			targets++
		}
		if lost := r.Lost(); r.Retried && len(lost) > 0 {
			regressed = append(regressed, fmt.Sprintf("%s lost %s after a second replay", r.Case, strings.Join(lost, ", ")))
		}
		if hits := r.NewHits(); len(hits) > 0 {
			regressed = append(regressed, fmt.Sprintf("%s reports the declined %s the baseline did not (a new hit)", r.Case, strings.Join(hits, ", ")))
		}
		if !r.Target || !r.compared() {
			continue
		}
		compared++
		if s := r.improved(); s != "" {
			better = append(better, s)
			missBetter = missBetter || strings.HasPrefix(r.Case, store.LearnInputMiss+"-")
		} else {
			unchanged = append(unchanged, r.Case)
		}
	}
	switch {
	case len(regressed) > 0:
		v.State, v.Rule = GateFailed, "regressed: "+strings.Join(regressed, "; ")
	case len(better) > 0 && !missBetter:
		v.State, v.Rule = GateHeld, RuleOnlyDeclines+" ("+strings.Join(better, "; ")+"; nothing lost, no new hit)"
	case len(better) > 0:
		v.State, v.Rule = GatePassed, strings.Join(better, "; ")+"; nothing lost, no new hit"
	case targets == 0:
		v.State, v.Rule = GateHeld, "no input the proposal noted has a learned case to replay, and nothing regressed"
	case compared == 0:
		v.State, v.Rule = GateHeld, "no target could be replayed"+skipped(rows)+", and nothing regressed"
	default:
		v.State, v.Rule = GateFailed, "no target improved ("+strings.Join(unchanged, ", ")+" as at the baseline)"
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
