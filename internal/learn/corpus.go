package learn

// The learned corpus (DECISIONS "The daemon keeps a learned eval corpus"):
// the retro's general misses and the findings authors declined as wrong or
// ranked too high become eval cases, so a skill or prompt change can be
// scored on what magnum missed and on what it should not have posted. The
// engine writes them to paths.Layout.LearnCorpus; `magnum eval` reads them
// after the operator's corpus.

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/store"
)

// learnedSeverities are the miss severities a learned case is made of: a P3
// miss is a nit, not worth a replay.
var learnedSeverities = []string{"P0", "P1", "P2"}

// CaseFromMiss is the learned case of miss m: ok when m is a miss (class
// miss) of scope general, P0 to P2, still new or used (or dismissed as
// covered at a gate's baseline, learnedState), on a reviewed commit
// the case can pin. The case is miss-<id> on m's PR at that commit, why its
// title, with one defect to find, miss-<id>: m's path (literally, not as a
// glob), its lines (or its line), the match expressions that compile and
// its severity. A row the corpus would refuse (no path nor match left, a
// PR it cannot name) makes no case.
func CaseFromMiss(m store.Miss) (eval.Case, bool) {
	if m.Class != store.MissMiss || m.Scope != store.MissScopeGeneral || !slices.Contains(learnedSeverities, m.Severity) ||
		!learnedState(m.State, m.Dismissed) {
		return eval.Case{}, false
	}
	name := fmt.Sprintf("miss-%d", m.ID)
	d := eval.Defect{ID: name, Title: cmp.Or(strings.TrimSpace(m.Title), fmt.Sprintf("miss %d", m.ID)), Severity: m.Severity,
		Paths: literalPaths(m.Path), Lines: defectLines(m.Lines, m.Line), Match: compiling(m.Match)}
	return learnedCase(name, m.Repo, m.Number, m.ReviewedSHA, m.Title, d)
}

// CaseFromDecline is the learned case of decline d: ok when the author's
// answer showed magnum's finding wrong or ranked too high (verdict wrong or
// over_ranked) for every repository (scope general), and d is still new or
// used (or dismissed as covered at a gate's baseline, learnedState). The
// case is decline-<id> on d's PR at the reviewed commit, why the
// finding's title, with one absent defect, decline-<id>, on the finding's
// path (literally) and line with d's match expressions: a finding that
// reports it again is a false positive. An over-ranked one is a hit only at
// the priority magnum posted or stronger (eval.Defect.Severity), so the
// point ranked lower is no hit; one whose posted priority is not P0 to P3
// makes no case, since every matching finding would be a hit and the point
// could stop being one only by not being reported.
func CaseFromDecline(d store.Decline) (eval.Case, bool) {
	if (d.Verdict != store.VerdictWrong && d.Verdict != store.VerdictOverRanked) || d.Scope != store.MissScopeGeneral ||
		!learnedState(d.State, d.Dismissed) {
		return eval.Case{}, false
	}
	name := fmt.Sprintf("decline-%d", d.ID)
	def := eval.Defect{ID: name, Title: cmp.Or(strings.TrimSpace(d.Title), fmt.Sprintf("decline %d", d.ID)), Expect: eval.ExpectAbsent,
		Paths: literalPaths(d.Path), Lines: defectLines(nil, d.Line), Match: compiling(d.Match)}
	if d.Verdict == store.VerdictOverRanked {
		if !slices.Contains([]string{"P0", "P1", "P2", "P3"}, d.Priority) {
			return eval.Case{}, false
		}
		def.Severity = d.Priority
	}
	return learnedCase(name, d.Repo, d.Number, d.ReviewedSHA, d.Title, def)
}

// learnedState: a dismissed row teaches nothing, unless a gate dismissed it
// because its baseline covers it already (dismissed, store.LearnDismissedFound
// or LearnDismissedNotReported): its case checks that a later candidate
// does not lose what the skill does; a used one still measures.
func learnedState(state, dismissed string) bool {
	switch state {
	case store.MissNew, store.MissUsed:
		return true
	case store.MissDismissed:
		return dismissed == store.LearnDismissedFound || dismissed == store.LearnDismissedNotReported
	}
	return false
}

// learnedCase is the case name of repo#number at head with the one defect
// d, why title; ok is false when the corpus would refuse it.
func learnedCase(name, repo string, number int, head, title string, d eval.Defect) (eval.Case, bool) {
	c := eval.Case{Name: name, PR: fmt.Sprintf("%s#%d", repo, number), Head: head, Why: strings.TrimSpace(title), Defects: []eval.Defect{d}}
	if c.Validate() != nil {
		return eval.Case{}, false
	}
	return c, true
}

// literalPaths is the defect paths of a comment's path: the path with the
// glob characters escaped, so it matches itself alone; none for "".
func literalPaths(path string) []string {
	if path == "" {
		return nil
	}
	var b strings.Builder
	for _, r := range path {
		if strings.ContainsRune(`\*?[]`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return []string{b.String()}
}

// defectLines is a row's line range as a defect's: lines when it is one
// ([from, to], 1 <= from <= to), else [line, line] for a line, else none.
func defectLines(lines []int, line int) []int {
	if len(lines) == 2 && lines[0] >= 1 && lines[0] <= lines[1] {
		return slices.Clone(lines)
	}
	if line > 0 {
		return []int{line, line}
	}
	return nil
}

// compiling keeps the match expressions that compile and match something
// less than everything (not empty).
func compiling(match []string) []string {
	var out []string
	for _, m := range match {
		if m == "" {
			continue
		}
		if _, err := regexp.Compile("(?i)" + m); err == nil {
			out = append(out, m)
		}
	}
	return out
}
