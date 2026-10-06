package cli

// The value section of `magnum stats`: who found the posted findings, per
// kind of round, in the rounds whose provenance can tell. Since the judge's
// own pass (DECISIONS "The judge does its own pass while the reviewers
// work"), the judge writes judge-own.md before it reads any report, and the
// skill names `judge` among a finding's sources only when that pass found it;
// before, it read the reports first, and `judge` meant "found or confirmed".
// A delta check is the judge alone, so everything it posts is its own.

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// The kinds of round the value section counts (statsValue.Kind), in order.
const (
	statsValueFirst    = "first_review" // a first review with the judge's own pass
	statsValueRereview = "rereview"     // a re-review (or its recovery) with the judge's own pass
	statsValueDelta    = "delta_check"  // a re-review the judge ran alone
)

var statsValueKinds = []string{statsValueFirst, statsValueRereview, statsValueDelta}

// statsValueLabels name the kinds in the text report.
var statsValueLabels = map[string]string{statsValueFirst: "first review", statsValueRereview: "re-review", statsValueDelta: "delta check"}

// statsValue is what the posted rounds of one kind posted and who found it
// (statsReport.Value).
type statsValue struct {
	Kind   string `json:"kind"`   // first_review | rereview | delta_check
	Rounds int    `json:"rounds"` // posted rounds of the kind
	// Posted counts the posted findings by priority. Judge counts those only
	// the judge's own pass found (all of a delta check's), Both those the own
	// pass and a reviewer found, Reviewers those only reviewers found, by the
	// reviewers that did ("claude-review", "claude-review+codex-review"), and
	// Unattributed those whose provenance names no source.
	Posted       map[string]int            `json:"posted,omitempty"`
	Judge        map[string]int            `json:"judge,omitempty"`
	Both         map[string]int            `json:"both,omitempty"`
	Reviewers    map[string]map[string]int `json:"reviewers,omitempty"`
	Unattributed map[string]int            `json:"unattributed,omitempty"`
	// ReviewerOnlyP0P2 counts the posted P0 to P2 findings only reviewers
	// found, Per10Rounds the same per 10 rounds (one decimal).
	ReviewerOnlyP0P2 int     `json:"reviewer_only_p0_p2"`
	Per10Rounds      float64 `json:"reviewer_only_p0_p2_per_10_rounds"`
	// MedianSeconds is each role's median turn in these rounds (statsRound
	// durations), the judge's own pass as "<judge> own pass".
	MedianSeconds map[string]int64 `json:"median_seconds,omitempty"`
}

// statsValueKind is the kind rd counts under in the value section: a first
// review or a re-review (a recovery included) with a run of the judge's own
// pass, or a delta check, a re-review the judge ran alone (an unchanged head
// too); "" for any other round, whose sources cannot tell what the judge
// found itself.
func (rd *statsRound) valueKind(isJudge func(string) bool) string {
	ownPass, reviewers, kind := false, false, ""
	for _, r := range rd.runs {
		switch {
		case r.Kind == store.RunOwnPass:
			ownPass = true
		case !isJudge(r.Role):
			reviewers = true
		case kind == "" && slices.Contains([]string{store.RunInitial, store.RunRereview, store.RunRecovery}, r.Kind):
			kind = r.Kind
		}
	}
	switch {
	case ownPass && kind == store.RunInitial:
		return statsValueFirst
	case ownPass && kind != "":
		return statsValueRereview
	case !ownPass && !reviewers && (kind == store.RunRereview || kind == store.RunRecovery):
		return statsValueDelta
	}
	return ""
}

// statsValueRound is a round the value section counts.
type statsValueRound struct {
	kind  string
	spans map[string]time.Duration
}

// statsValueAcc accumulates one kind.
type statsValueAcc struct {
	v     statsValue
	spans map[string][]time.Duration
}

// statsValues computes the value section from the rounds it counts (keyed
// by PR id and round) and the window's findings: the posted ones of each
// round's latest judge run that recorded any.
func statsValues(rounds map[statsValueKey]statsValueRound, findings []store.Finding) []statsValue {
	latest := map[statsValueKey]string{} // the run whose provenance counts
	for _, f := range findings {         // oldest first
		latest[statsValueKey{f.PRID, f.Round}] = f.RunID
	}
	accs := map[string]*statsValueAcc{}
	acc := func(kind string) *statsValueAcc {
		a := accs[kind]
		if a == nil {
			a = &statsValueAcc{v: statsValue{Kind: kind, Posted: map[string]int{}, Judge: map[string]int{}, Both: map[string]int{},
				Reviewers: map[string]map[string]int{}, Unattributed: map[string]int{}, MedianSeconds: map[string]int64{}},
				spans: map[string][]time.Duration{}}
			accs[kind] = a
		}
		return a
	}
	for _, rd := range rounds {
		a := acc(rd.kind)
		a.v.Rounds++
		for role, d := range rd.spans {
			if role != statsRoundRole {
				a.spans[role] = append(a.spans[role], d)
			}
		}
	}
	for _, f := range findings {
		k := statsValueKey{f.PRID, f.Round}
		rd, ok := rounds[k]
		if !ok || f.Verdict != store.FindingPosted || f.RunID != latest[k] {
			continue
		}
		sev := strings.ToUpper(strings.TrimSpace(f.Severity))
		if !slices.Contains(statsSeverities, sev) {
			continue
		}
		a := acc(rd.kind)
		a.v.Posted[sev]++
		judge := slices.Contains(f.Sources, statsJudgeSource)
		var reviewers []string
		for _, s := range f.Sources {
			if s != statsJudgeSource {
				reviewers = append(reviewers, s)
			}
		}
		reviewers = slices.Compact(slices.Sorted(slices.Values(reviewers)))
		switch {
		case rd.kind == statsValueDelta || (judge && len(reviewers) == 0):
			a.v.Judge[sev]++
		case judge:
			a.v.Both[sev]++
		case len(reviewers) > 0:
			by := strings.Join(reviewers, "+")
			if a.v.Reviewers[by] == nil {
				a.v.Reviewers[by] = map[string]int{}
			}
			a.v.Reviewers[by][sev]++
			if sev != "P3" {
				a.v.ReviewerOnlyP0P2++
			}
		default:
			a.v.Unattributed[sev]++
		}
	}
	out := []statsValue{}
	for _, kind := range statsValueKinds {
		a := accs[kind]
		if a == nil {
			continue
		}
		for role, ds := range a.spans {
			a.v.MedianSeconds[role] = statsSeconds(statsRank(slices.Sorted(slices.Values(ds)), 50))
		}
		a.v.Per10Rounds = statsPer10(a.v.ReviewerOnlyP0P2, a.v.Rounds)
		out = append(out, a.v)
	}
	return out
}

// statsValueKey is a round: its PR's id and its number.
type statsValueKey struct {
	pr    int64
	round int
}

// statsPer10 is n per 10 rounds, to one decimal (0 without rounds).
func statsPer10(n, rounds int) float64 {
	if rounds == 0 {
		return 0
	}
	return math.Round(100*float64(n)/float64(rounds)) / 10
}

// statsRenderValue prints the value section: who found the posted findings
// (a row per finder of each kind, then all of them) and what the reviewers
// add per kind. Nothing without rounds.
func statsRenderValue(w io.Writer, values []statsValue) {
	var found, worth [][]string
	for _, v := range values {
		label := statsValueLabels[v.Kind]
		row := func(by string, counts map[string]int) {
			cells := []string{label, by}
			sum := 0
			for _, sev := range statsSeverities {
				cells = append(cells, strconv.Itoa(counts[sev]))
				sum += counts[sev]
			}
			if sum > 0 {
				found = append(found, cells)
			}
		}
		if v.Kind == statsValueDelta {
			row("judge alone", v.Judge)
		} else {
			row("own pass", v.Judge)
		}
		row("own pass + reviewers", v.Both)
		for _, by := range slices.Sorted(maps.Keys(v.Reviewers)) {
			row(actClean(strings.ReplaceAll(by, "+", " + ")), v.Reviewers[by])
		}
		row("no source", v.Unattributed)
		row("all", v.Posted)

		reviewerOnly, per10 := strconv.Itoa(v.ReviewerOnlyP0P2), fmt.Sprintf("%.1f", v.Per10Rounds)
		if v.Kind == statsValueDelta {
			reviewerOnly, per10 = "-", "-"
		}
		var turns []string
		for _, role := range statsOrdered(v.MedianSeconds, "") {
			turns = append(turns, actClean(role)+" "+tui.HumanDuration(time.Duration(v.MedianSeconds[role])*time.Second))
		}
		worth = append(worth, []string{label, strconv.Itoa(v.Rounds), reviewerOnly, per10, cmp.Or(strings.Join(turns, ", "), "-")})
	}
	statsTable(w, "WHO FOUND THE POSTED FINDINGS", append([]string{"KIND", "FOUND BY"}, statsSeverities...), found)
	statsTable(w, "WHAT THE REVIEWERS ADD", []string{"KIND", "ROUNDS", "REVIEWER-ONLY P0-P2", "PER 10 ROUNDS", "MEDIAN TURN"}, worth)
}
