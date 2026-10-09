package cli

import (
	"bytes"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// statsValueSpec is one synthetic round for the value section: its runs
// (role, kind, from and to, in minutes after t0; the judge's main run ends
// the round) and the provenance its judge recorded.
type statsValueSpec struct {
	pr, number, round int
	runs              []statsValueRun
	outcome           string
	findings          []store.Finding
}

type statsValueRun struct {
	role, kind string
	from, to   int
}

// statsValueRuns turns the rounds into runs (oldest first) and findings.
func statsValueRuns(t0 time.Time, rounds []statsValueSpec) ([]store.RoundRun, []store.Finding) {
	var runs []store.RoundRun
	var fs []store.Finding
	for _, rd := range rounds {
		for i, r := range rd.runs {
			rr := statsRR(rd.outcome+string(rune('a'+i)), int64(rd.pr), rd.number, rd.round, r.role, store.RunVerified, t0.Add(time.Duration(r.from)*time.Minute))
			rr.Kind = r.kind
			rr.SubmittedAt, rr.EndedAt = statsOpt(t0.Add(time.Duration(r.from)*time.Minute)), statsOpt(t0.Add(time.Duration(r.to)*time.Minute))
			if r.role == store.RoleJudge && r.kind != store.RunOwnPass {
				rr.Outcome = new(rd.outcome)
			}
			runs = append(runs, rr)
		}
		for _, f := range rd.findings {
			f.PRID, f.Round, f.Repo, f.RunID, f.CreatedAt = int64(rd.pr), rd.round, "talkable/talkable", "judge", t0.Add(time.Hour)
			fs = append(fs, f)
		}
	}
	return runs, fs
}

// statsPosted and statsRejected are a posted or rejected finding of a
// priority raised by sources.
func statsPosted(id, sev string, sources ...string) store.Finding {
	return store.Finding{FindingID: id, Severity: sev, Sources: sources, Verdict: store.FindingPosted}
}

func statsRejected(id, sev string, sources ...string) store.Finding {
	return store.Finding{FindingID: id, Severity: sev, Sources: sources, Verdict: store.FindingRejected, ReasonCode: "not_reproducible"}
}

// statsValueFixture is a day of rounds: two first reviews and a re-review
// with the judge's own pass, a delta check (the judge alone), and four
// rounds the section leaves out: a first review without an own pass (from
// before it, when `judge` meant "found or confirmed"), an own-pass round
// that was stopped, a first review the judge ran alone and a round that
// began before the window.
func statsValueFixture(t0 time.Time) ([]store.RoundRun, []store.Finding) {
	J, C, X := store.RoleJudge, store.RoleClaude, store.RoleCodexReview
	return statsValueRuns(t0, []statsValueSpec{
		{pr: 1, number: 201, round: 1, outcome: "posted", runs: []statsValueRun{
			{C, store.RunInitial, 1, 19}, {X, store.RunInitial, 1, 9}, {J, store.RunOwnPass, 1, 11}, {J, store.RunInitial, 20, 34}},
			findings: []store.Finding{
				statsPosted("V1", "P2", "judge"),
				statsPosted("V2", "P1", "claude-review"),
				statsPosted("V3", "P3", "claude-review", "judge"),
				statsPosted("V4", "P2", "codex-review", "claude-review"),
				statsRejected("V5", "P2", "codex-review"),
				statsPosted("V6", "P3"),
			}},
		{pr: 2, number: 202, round: 1, outcome: "posted", runs: []statsValueRun{
			{C, store.RunInitial, 1, 13}, {J, store.RunOwnPass, 1, 7}, {J, store.RunInitial, 14, 20}},
			findings: []store.Finding{statsPosted("W1", "P2", "judge")}},
		{pr: 2, number: 202, round: 2, outcome: "posted", runs: []statsValueRun{
			{C, store.RunRereview, 100, 110}, {J, store.RunOwnPass, 100, 104}, {J, store.RunRereview, 111, 115}},
			findings: []store.Finding{statsPosted("X1", "P3", "claude-review"), statsPosted("X2", "P2", "judge", "codex-review")}},
		{pr: 3, number: 203, round: 2, outcome: "posted", runs: []statsValueRun{{J, store.RunRereview, 200, 204}},
			findings: []store.Finding{statsPosted("Y1", "P2", "judge"), statsPosted("Y2", "P3")}},
		{pr: 4, number: 204, round: 1, outcome: "posted", runs: []statsValueRun{{C, store.RunInitial, 300, 310}, {J, store.RunInitial, 311, 320}},
			findings: []store.Finding{statsPosted("Z1", "P1", "claude-review")}},
		{pr: 5, number: 205, round: 1, outcome: "stopped", runs: []statsValueRun{
			{C, store.RunInitial, 400, 410}, {J, store.RunOwnPass, 400, 405}, {J, store.RunInitial, 411, 412}}},
		{pr: 6, number: 206, round: 1, outcome: "posted", runs: []statsValueRun{{J, store.RunInitial, 500, 510}},
			findings: []store.Finding{statsPosted("Q1", "P1", "judge")}},
		{pr: 7, number: 207, round: 1, outcome: "posted", runs: []statsValueRun{
			{C, store.RunInitial, -120, -100}, {J, store.RunOwnPass, -120, -110}, {J, store.RunInitial, -99, -90}},
			findings: []store.Finding{statsPosted("R1", "P1", "claude-review")}},
	})
}

// `magnum stats` tells what the reviewers add: in the rounds where the
// judge made its own pass before reading any report, and in the delta
// checks it runs alone, each posted finding counts under who found it (the
// own pass, the own pass and a reviewer, or reviewers only, by the
// reviewers), with the reviewer-only P0-P2 per 10 rounds and each role's
// median turn per kind of round.
func TestStatsValueSplitsWhoFoundThePostedFindings(t *testing.T) {
	t0 := statsOct(3, 10, 0)
	runs, fs := statsValueFixture(t0)
	r := statsCompute(runs, fs, nil, func(role string) bool { return role == store.RoleJudge }, t0.Add(-time.Hour), t0.Add(24*time.Hour), "")
	want := []statsValue{
		{Kind: statsValueFirst, Rounds: 2,
			Posted:           map[string]int{"P1": 1, "P2": 3, "P3": 2},
			Judge:            map[string]int{"P2": 2},
			Both:             map[string]int{"P3": 1},
			Reviewers:        map[string]map[string]int{"claude-review": {"P1": 1}, "claude-review+codex-review": {"P2": 1}},
			Unattributed:     map[string]int{"P3": 1},
			ReviewerOnlyP0P2: 2, Per10Rounds: 10,
			MedianSeconds: map[string]int64{"claude-review": 12 * 60, "codex-review": 8 * 60, "codex-judge": 6 * 60, "codex-judge own pass": 6 * 60}},
		{Kind: statsValueRereview, Rounds: 1,
			Posted:        map[string]int{"P2": 1, "P3": 1},
			Both:          map[string]int{"P2": 1},
			Reviewers:     map[string]map[string]int{"claude-review": {"P3": 1}},
			MedianSeconds: map[string]int64{"claude-review": 10 * 60, "codex-judge": 4 * 60, "codex-judge own pass": 4 * 60}},
		{Kind: statsValueDelta, Rounds: 1,
			Posted:        map[string]int{"P2": 1, "P3": 1},
			Judge:         map[string]int{"P2": 1, "P3": 1},
			MedianSeconds: map[string]int64{"codex-judge": 4 * 60}},
	}
	if got, w := statsJSON(t, r.Value), statsJSON(t, want); got != w {
		t.Fatalf("value =\n%s\nwant\n%s", got, w)
	}

	// One reviewer-only P2 in three rounds is 3.3 per 10 rounds; another
	// repository leaves nothing.
	if got := statsPer10(1, 3); got != 3.3 {
		t.Errorf("1 in 3 rounds = %v per 10, want 3.3", got)
	}
	if r := statsCompute(runs, fs, nil, func(role string) bool { return role == store.RoleJudge }, t0.Add(-time.Hour), t0.Add(24*time.Hour), "example/widgets"); len(r.Value) != 0 {
		t.Errorf("another repository: %s", statsJSON(t, r.Value))
	}
}

// The text report shows who found the posted findings, row by row, and what
// the reviewers add per kind of round; a delta check has no reviewers.
func TestStatsValueRendersBothTables(t *testing.T) {
	t0 := statsOct(3, 10, 0)
	runs, fs := statsValueFixture(t0)
	r := statsCompute(runs, fs, nil, func(role string) bool { return role == store.RoleJudge }, t0.Add(-time.Hour), t0.Add(24*time.Hour), "")
	var b bytes.Buffer
	statsRender(&b, r)
	lines := statsLines(b.String())
	want := []string{
		"WHO FOUND THE POSTED FINDINGS",
		"KIND FOUND BY P0 P1 P2 P3",
		"first review own pass 0 0 2 0",
		"first review own pass + reviewers 0 0 0 1",
		"first review claude-review 0 1 0 0",
		"first review claude-review + codex-review 0 0 1 0",
		"first review no source 0 0 0 1",
		"first review all 0 1 3 2",
		"re-review own pass + reviewers 0 0 1 0",
		"re-review claude-review 0 0 0 1",
		"re-review all 0 0 1 1",
		"delta check judge alone 0 0 1 1",
		"delta check all 0 0 1 1",
		"WHAT THE REVIEWERS ADD",
		"KIND ROUNDS REVIEWER-ONLY P0-P2 PER 10 ROUNDS MEDIAN TURN",
		"first review 2 2 10.0 claude-review 12m, codex-judge 6m, codex-judge own pass 6m, codex-review 8m",
		"re-review 1 0 0.0 claude-review 10m, codex-judge 4m, codex-judge own pass 4m",
		"delta check 1 - - codex-judge 4m",
	}
	at := 0
	for _, w := range want {
		i := slices.Index(lines[at:], w)
		if i < 0 {
			t.Fatalf("missing line %q (after line %d) in\n%s", w, at, b.String())
		}
		at += i + 1
	}
	if slices.Contains(lines, "re-review own pass 0 0 0 0") {
		t.Errorf("an empty row in\n%s", b.String())
	}
}
