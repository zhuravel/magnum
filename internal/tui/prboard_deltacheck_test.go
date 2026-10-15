package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// A delta check says so in the state cell, waiting ("delta check · quiet →
// 14:09") or in flight; an ordinary re-review does not. The card's LAST
// ROUND names it and its judge.
func TestPRBoardShowsTheDeltaCheckKind(t *testing.T) {
	m, _, _ := newBoard(t, 220, 40, PRBoardOptions{})
	p := m.painter()
	cell := func(r PRBoardRow) string { return ansi.Strip(p.stateWaitCell(sanitizeRow(r)).render(nil)) }

	row := PRBoardRow{Owner: "talkable", Repo: "talkable", Number: 730, State: "rereview_pending",
		Wait: "delta check · quiet → 14:09", DeltaCheck: true}
	if got := cell(row); !strings.Contains(got, "re-review") || !strings.HasSuffix(got, " delta check · quiet → 14:09") {
		t.Errorf("waiting: state cell = %q", got)
	}
	plain := row
	plain.Wait, plain.DeltaCheck = "re-review · quiet → 14:09", false
	if got := cell(plain); strings.Contains(got, "delta check") || !strings.HasSuffix(got, " quiet → 14:09") {
		t.Errorf("an ordinary re-review: state cell = %q", got)
	}

	row.State, row.Wait, row.DeltaCheck = "reviewing", "", false
	row.Progress = &RoundProgress{StartedAt: boardNow.Add(-4 * time.Minute)}
	row.RoundWhy = &RoundWhy{Kind: "rereview", DeltaCheck: true, DeltaLines: 4, Roles: []string{"codex-judge"}, At: row.Progress.StartedAt}
	if got := cell(row); !strings.Contains(got, "reviewing") || !strings.HasSuffix(got, " delta check · 4m") {
		t.Errorf("in flight: state cell = %q", got)
	}
	row.State = "reviewed" // the round ended: the pill says nothing more
	if got := cell(row); strings.Contains(got, "delta check") {
		t.Errorf("reviewed: state cell = %q", got)
	}

	section := lastRoundSection(t, roundsCard(t, row, 100))
	if !slices.ContainsFunc(section, func(l string) bool { return strings.Contains(l, "delta check (4 lines): judge only") }) {
		t.Errorf("LAST ROUND = %q", section)
	}
}

// A scheduled full pass runs as a recovery of every role: the card's LAST
// ROUND calls it a full pass, not a recovery.
func TestTheCardNamesAFullPass(t *testing.T) {
	row := PRBoardRow{Owner: "talkable", Repo: "talkable", Number: 730, State: "reviewed",
		RoundWhy: &RoundWhy{Kind: "recovery", FullPass: true, Roles: []string{"codex-judge", "claude-review"}}}
	section := lastRoundSection(t, roundsCard(t, row, 100))
	if !slices.ContainsFunc(section, func(l string) bool { return strings.Contains(l, "full pass: codex-judge, claude-review") }) ||
		slices.ContainsFunc(section, func(l string) bool { return strings.Contains(l, "recovery") }) {
		t.Errorf("LAST ROUND = %q", section)
	}
}

// sinceSection is the SINCE REVIEW section of the card of row.
func sinceSection(t *testing.T, row PRBoardRow) string {
	t.Helper()
	lines := roundsCard(t, row, 120)
	for i, l := range lines {
		if strings.Contains(l, "SINCE REVIEW") && i+1 < len(lines) {
			return lines[i+1]
		}
	}
	t.Fatalf("no SINCE REVIEW section:\n%s", strings.Join(lines, "\n"))
	return ""
}

// SINCE REVIEW across a merge of the base branch: the PR's own counts get a
// merge mark in the table and say what they leave out on the card; counts
// that include the base branch's changes are marked raw.
func TestPRBoardSinceReviewMarksAMergeOfTheBase(t *testing.T) {
	own := &ReviewDelta{Base: "reviewed", BaseSHA: "2cb4d7c000", Commits: 2, Files: 1, Additions: 1, Deletions: 1, MergedBase: "master"}
	raw := &ReviewDelta{Base: "reviewed", BaseSHA: "2cb4d7c000", Commits: 25, Files: 269, Additions: 13307, Deletions: 1062, RawBase: "master"}
	plain := &ReviewDelta{Base: "reviewed", BaseSHA: "2cb4d7c000", Commits: 1, Files: 1, Additions: 3}
	for _, tc := range []struct {
		mode IconMode
		mark string
	}{{IconsUnicode, "⑂"}, {IconsNerd, "\uf419"}, {IconsASCII, "m"}} {
		m, _, _ := newBoard(t, 220, 40, PRBoardOptions{Icons: tc.mode})
		p := m.painter()
		w := p.sinceWidths([]PRBoardRow{{SinceReview: own}, {SinceReview: plain}})
		if got := ansi.Strip(p.sinceCell(own, w).render(nil)); !strings.HasSuffix(got, "+1 "+p.g.minus+"1 "+tc.mark) {
			t.Errorf("%s: own counts cell = %q, want the mark %q last", tc.mode, got, tc.mark)
		}
		if got := ansi.Strip(p.sinceCell(plain, w).render(nil)); !strings.HasSuffix(got, "+3 "+p.g.minus+"0") {
			t.Errorf("%s: a plain push's cell = %q, want no mark", tc.mode, got)
		}
	}

	row := roundsRow()
	row.SinceReview = own
	if got := sinceSection(t, row); !strings.Contains(got, "2 commits · 1 file · +1 −1 since the reviewed head 2cb4d7c (excluding a merge of master)") {
		t.Errorf("own counts: %q", got)
	}
	row.SinceReview = raw
	if got := sinceSection(t, row); !strings.Contains(got, "25 commits · 269 files") || !strings.Contains(got, "(raw: including a merge of master)") {
		t.Errorf("raw counts: %q", got)
	}
	row.SinceReview = plain
	if got := sinceSection(t, row); strings.Contains(got, "merge") || strings.Contains(got, "raw") {
		t.Errorf("a plain push: %q", got)
	}
}

// The engine stamps a round's start (Progress.StartedAt) before it writes
// the round's engine.round_start, so for a moment the RoundWhy a row carries
// is the previous round's. A running round's state cell and card trust it
// only once it is the running round's: a start of its own at or after the
// round's; until then the cell shows the stage and time with no kind. A
// delta check that waits keeps its words, which the daemon's wait decides.
func TestARunningRoundIgnoresThePreviousRoundsDeltaCheck(t *testing.T) {
	m, _, _ := newBoard(t, 220, 40, PRBoardOptions{})
	p := m.painter()
	start := boardNow.Add(-4 * time.Minute)
	judge := RoleProgress{Role: "codex-judge", Label: "cj", Judge: true, Started: start, Working: true}
	running := func(at time.Time) PRBoardRow {
		r := runningRow(start, judge)
		r.RoundWhy = &RoundWhy{Kind: "rereview", DeltaCheck: true, DeltaLines: 4, Roles: []string{"codex-judge"}, At: at}
		return r
	}
	noProgress := running(start)
	noProgress.Progress = nil
	unstarted := running(start)
	unstarted.Progress = &RoundProgress{Roles: []RoleProgress{judge}}

	for _, tc := range []struct {
		name  string
		row   PRBoardRow
		delta bool
	}{
		{"the previous round's delta check", running(start.Add(-time.Hour)), false},
		{"a delta check a second before the round", running(start.Add(-time.Second)), false},
		{"a RoundWhy with no start", running(time.Time{}), false},
		{"a round whose progress is unknown", noProgress, false},
		{"a round with no start", unstarted, false},
		{"the round's own delta check, at its start", running(start), true},
		{"the round's own delta check, after its start", running(start.Add(3 * time.Second)), true},
	} {
		r := sanitizeRow(tc.row)
		for _, d := range []stateDetail{stateFull, stateTime, stateBare} {
			got := ansi.Strip(p.stateWaitDetail(r, d).render(nil))
			if strings.Contains(got, "delta check") != tc.delta {
				t.Errorf("%s (detail %d): state cell = %q, delta check shown: %v, want %v", tc.name, d, got, !tc.delta, tc.delta)
			}
		}
		section := lastRoundSection(t, roundsCard(t, tc.row, 100))
		said := slices.ContainsFunc(section, func(l string) bool { return strings.Contains(l, "delta check (4 lines)") })
		if said != tc.delta && roundProgress(r) != nil { // the card of a round it cannot see runs names the last round
			t.Errorf("%s: LAST ROUND = %q, delta check named: %v, want %v", tc.name, section, said, tc.delta)
		}
	}
	if got := ansi.Strip(p.stateWaitCell(sanitizeRow(running(start.Add(-time.Hour)))).render(nil)); !strings.HasSuffix(got, " judge · 4m") {
		t.Errorf("a round whose kind is not known yet: state cell = %q, want its stage and time", got)
	}

	queued := running(start.Add(-time.Hour))
	queued.State, queued.Progress, queued.Wait, queued.DeltaCheck = "rereview_pending", nil, "delta check · quiet → 14:09", true
	if got := ansi.Strip(p.stateWaitCell(sanitizeRow(queued)).render(nil)); !strings.HasSuffix(got, " delta check · quiet → 14:09") {
		t.Errorf("a delta check that waits: state cell = %q", got)
	}
}
