package tui

import (
	"slices"
	"strings"
	"testing"

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
	row.RoundWhy = &RoundWhy{Kind: "rereview", DeltaCheck: true, DeltaLines: 4, Roles: []string{"codex-judge"}}
	if got := cell(row); !strings.Contains(got, "reviewing") || !strings.HasSuffix(got, " delta check") {
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
