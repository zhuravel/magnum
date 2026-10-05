package tui

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// recentRows are boardRows (the merged #11800 among them stays a row of
// --all: it is outside the window) with three PRs GitHub merged or closed
// within [board] recent_closed: #11990 merged before magnum reviewed its
// last push, #11991 closed unmerged, #11992 merged after its review (the
// most recently updated PR of all).
func recentRows() []PRBoardRow {
	return append(boardRows(),
		PRBoardRow{Ref: "talkable/talkable#11990", Owner: "talkable", Repo: "talkable", Number: 11990,
			Title: "Retire the coupon v1 API", Author: "alice", State: "closed", GHState: "MERGED",
			UpdatedAt: ago(2 * time.Hour), HeadSHA: "b2b2b2b2b2", ClosedAt: ago(2 * time.Hour), Recent: true, MergedUnreviewed: true,
			LastReview: &ReviewInfo{Login: "talkable[bot]", Event: "COMMENTED", SubmittedAt: ago(6 * time.Hour), CommitSHA: "a1a1a1a1a1", Stale: true}},
		PRBoardRow{Ref: "talkable/talkable#11991", Owner: "talkable", Repo: "talkable", Number: 11991,
			Title: "Spike: referral sharing via SMS", Author: "rev-ann", State: "closed", GHState: "CLOSED",
			UpdatedAt: ago(30 * time.Minute), HeadSHA: "c3c3c3c3c3", ClosedAt: ago(30 * time.Minute), Recent: true},
		PRBoardRow{Ref: "talkable/talkable#11992", Owner: "talkable", Repo: "talkable", Number: 11992,
			Title: "Speed up the campaign list query", Author: "alice", State: "released", GHState: "MERGED",
			UpdatedAt: ago(time.Minute), HeadSHA: "d4d4d4d4d4", ClosedAt: ago(5 * time.Hour), Recent: true,
			LastReview: &ReviewInfo{Login: "talkable[bot]", Event: "APPROVED", SubmittedAt: ago(6 * time.Hour), CommitSHA: "d4d4d4d4d4"}},
	)
}

// recentBoard is a board w x h that received recentRows, with a 24h window.
func recentBoard(t *testing.T, w, h int, opts PRBoardOptions) prBoardModel {
	t.Helper()
	opts.Now = func() time.Time { return boardNow }
	if opts.SelfLogins == nil {
		opts.SelfLogins = boardSelf
	}
	if opts.RecentClosed == 0 {
		opts.RecentClosed = 24 * time.Hour
	}
	src := &fakeBoardSource{rows: recentRows()}
	m := newPRBoardModel(context.Background(), src, &fakeActions{}, opts)
	m, _ = send(t, m, tea.WindowSizeMsg{Width: w, Height: h}, prbDataMsg{rows: src.rows})
	return m
}

var recentOrder = []string{"talkable/talkable#11991", "talkable/talkable#11990", "talkable/talkable#11992"}

// The recently closed PRs come after the open ones, newest closed first,
// whatever the sort and its direction; the open rows keep the sort.
func TestPRBoardRecentlyClosedSectionFollowsTheOpenRows(t *testing.T) {
	m := recentBoard(t, 220, 40, PRBoardOptions{})
	for _, keys := range [][]string{nil, {"s"}, {"s", "s"}, {"S"}, {"s", "s", "s", "S"}, {"s", "s", "s", "s", "s"}} {
		next, _ := send(t, m, func() []tea.Msg {
			var out []tea.Msg
			for _, k := range keys {
				out = append(out, keyMsg(k))
			}
			return out
		}()...)
		refs := boardRefs(next)
		if len(refs) != 10 || !slices.Equal(refs[7:], recentOrder) {
			t.Errorf("keys %v (sort %s, desc %v): rows %v, want the open seven then %v", keys, next.sort, next.desc, refs, recentOrder)
		}
		open := SortPRBoard(boardRows(), next.sort, next.desc)
		if !slices.Equal(refs[:7], refsOfFull(open)) {
			t.Errorf("keys %v: open rows %v, want %v", keys, refs[:7], refsOfFull(open))
		}
	}

	// The heading sits between the last open row and the first closed one.
	lines := strings.Split(viewOf(m), "\n")
	at := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "merged or closed in the last 24h") })
	if at < 1 || !strings.Contains(lines[at+1], "#11991") || !strings.Contains(lines[at-1], "#"+strconv.Itoa(m.view[6].Number)) {
		t.Fatalf("heading at line %d:\n%s", at, strings.Join(lines, "\n"))
	}
	mustContain(t, lines[at], "(3)")
}

func refsOfFull(rows []PRBoardRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = prRef(r)
	}
	return out
}

// SortPRBoard (the printed rows too) puts the Recent rows last, newest
// closed first, in every sort and direction.
func TestSortPRBoardPutsRecentlyClosedLast(t *testing.T) {
	for _, by := range PRSorts() {
		for _, desc := range []bool{true, false} {
			refs := refsOfFull(SortPRBoard(recentRows(), by, desc))
			if !slices.Equal(refs[7:], recentOrder) {
				t.Errorf("%s desc=%v: %v", by, desc, refs)
			}
		}
	}
}

// A merged or closed row says merged or closed in its state cell and is
// dimmed; a PR merged before magnum reviewed its last push says "merged ·
// unreviewed" in the red attention pill, which the dimming leaves alone.
func TestPRBoardRecentlyClosedStateCells(t *testing.T) {
	m := recentBoard(t, 220, 40, PRBoardOptions{})
	raw := m.View().Content
	plain := ansi.Strip(raw)

	flagged := lineWith(t, raw, "#11990")
	mustContain(t, ansi.Strip(flagged), "merged · unreviewed")
	if !strings.Contains(flagged, m.pal.pills["merged_unreviewed"].Render(" merged · unreviewed ")) {
		t.Errorf("the flag is not a red pill: %q", flagged)
	}
	if m.pal.pills["merged_unreviewed"].GetForeground() != m.pal.pills["needs_attention"].GetForeground() {
		t.Error("merged unreviewed must use the needs_attention color")
	}
	dim := lipgloss.NewStyle().Foreground(m.pal.dim)
	if !strings.Contains(flagged, dim.Render("Retire the coupon v1 API")) {
		t.Errorf("a recently closed row's title is not dimmed: %q", flagged)
	}

	closed := ansi.Strip(lineWith(t, raw, "#11991"))
	mustContain(t, closed, " closed ")
	mustNotContain(t, closed, "merged")
	merged := lineWith(t, raw, "#11992")
	mustContain(t, ansi.Strip(merged), " merged ")
	mustNotContain(t, ansi.Strip(merged), "unreviewed")
	if !strings.Contains(merged, dim.Render("Speed up the campaign list query")) {
		t.Errorf("merged row not dimmed: %q", merged)
	}
	// Open rows are not dimmed.
	if strings.Contains(lineWith(t, raw, "#11950"), dim.Render("Add OAuth login for the merchant portal")) {
		t.Error("an open row is dimmed")
	}
	mustContain(t, plain, "merged or closed in the last 24h")

	// Nerd icons: the flagged pill leads with the alert icon, merged with the merge icon.
	n := recentBoard(t, 220, 40, PRBoardOptions{Icons: IconsNerd})
	nv := viewOf(n)
	mustContain(t, nv, "\uf421 merged · unreviewed", "\uf419 merged")
	// ASCII has no "·".
	a := recentBoard(t, 220, 40, PRBoardOptions{Icons: IconsASCII})
	mustContain(t, viewOf(a), "merged | unreviewed")
}

// The summary line shows a red pill counting the flagged PRs in the window,
// and none when there are none.
func TestPRBoardSummaryCountsMergedUnreviewed(t *testing.T) {
	m := recentBoard(t, 220, 40, PRBoardOptions{})
	raw := m.View().Content
	summary := strings.Split(raw, "\n")[1]
	mustContain(t, ansi.Strip(summary), "1 merged unreviewed")
	if !strings.Contains(summary, m.pal.pills["merged_unreviewed"].Render(" 1 merged unreviewed ")) {
		t.Errorf("not a red pill: %q", summary)
	}

	rows := recentRows()
	rows[7].MergedUnreviewed = false
	m, _ = send(t, m, prbDataMsg{rows: rows}) // a refresh redraws: the flag is part of the frame
	mustNotContain(t, strings.Split(viewOf(m), "\n")[1], "merged unreviewed")
	mustNotContain(t, viewOf(m), "unreviewed")
	rows[7].MergedUnreviewed = true
	m, _ = send(t, m, prbDataMsg{rows: rows})
	mustContain(t, viewOf(m), "1 merged unreviewed", "merged · unreviewed")
}

// The closed rows follow the filter, the view and the owner scope; with
// none left the heading goes too.
func TestPRBoardRecentlyClosedFollowFilterViewAndOwner(t *testing.T) {
	m := recentBoard(t, 220, 40, PRBoardOptions{})
	m, _ = send(t, m, keyMsg("/"))
	m, _ = send(t, m, keys("c", "o", "u", "p", "o", "n")...)
	if got := boardRefs(m); !slices.Equal(got, []string{"talkable/talkable#11800", "talkable/talkable#11990"}) {
		t.Errorf("filter coupon: %v", got)
	}
	mustContain(t, viewOf(m), "merged or closed in the last 24h (1)")

	m, _ = send(t, m, keyMsg("esc"))
	m, _ = send(t, m, keyMsg("/"))
	m, _ = send(t, m, keys("O", "A", "u", "t", "h")...)
	if got := boardRefs(m); !slices.Equal(got, []string{"talkable/talkable#11950"}) {
		t.Errorf("filter OAuth: %v", got)
	}
	mustNotContain(t, viewOf(m), "merged or closed in the last")

	mine := recentBoard(t, 220, 40, PRBoardOptions{DefaultView: ViewMine})
	for _, r := range mine.view {
		if r.Recent {
			t.Errorf("view mine lists %s (not assigned to me, no review asked of me)", prRef(r))
		}
	}

	rows := append(recentRows(), PRBoardRow{Ref: "example/widgets#7", Owner: "example", Repo: "widgets", Number: 7, Title: "Widget export",
		State: "closed", GHState: "MERGED", UpdatedAt: ago(time.Hour), ClosedAt: ago(time.Hour), Recent: true})
	o := newPRBoardModel(context.Background(), &fakeBoardSource{rows: rows}, &fakeActions{},
		PRBoardOptions{Now: func() time.Time { return boardNow }, RecentClosed: 24 * time.Hour, DefaultOwner: "example"})
	o, _ = send(t, o, tea.WindowSizeMsg{Width: 220, Height: 40}, prbDataMsg{rows: rows})
	if got := boardRefs(o); !slices.Equal(got, []string{"example/widgets#7"}) {
		t.Errorf("owner example: %v", got)
	}
}

// The card of a PR merged before magnum reviewed its last push says so in
// one sentence, with the reviewed and the merged commits.
func TestPRBoardCardSaysMergedUnreviewed(t *testing.T) {
	m := recentBoard(t, 160, 60, PRBoardOptions{})
	m.moveTo(slices.Index(boardRefs(m), "talkable/talkable#11990"))
	m, _ = send(t, m, keyMsg("enter"))
	when := ago(2 * time.Hour).Format("Jan 2 15:04")
	mustContain(t, oneLine(strings.ReplaceAll(viewOf(m), "│", " ")),
		"Merged "+when+" (2h ago) before magnum reviewed its last push: last review on a1a1a1a, merged head b2b2b2b")

	rows := recentRows()
	rows[7].LastReview = nil
	m, _ = send(t, m, prbDataMsg{rows: rows})
	mustContain(t, oneLine(strings.ReplaceAll(viewOf(m), "│", " ")),
		"before magnum reviewed its last push: never reviewed, merged head b2b2b2b")

	m.moveTo(slices.Index(boardRefs(m), "talkable/talkable#11992"))
	mustNotContain(t, viewOf(m), "before magnum reviewed")
}

// With the heading on screen every row still scrolls into view, the
// scroll marks count rows, and a click picks the row under it (the
// heading is no row).
func TestPRBoardRecentlyClosedHeadingScrollsAndClicks(t *testing.T) {
	m := recentBoard(t, 200, 10, PRBoardOptions{Layout: LayoutOneLine}) // 4 table lines
	for i := range len(m.view) {
		m.moveTo(i)
		m.fixScroll()
		if !containsSplitRef(viewOf(m), strings.TrimPrefix(prRef(m.view[i]), "talkable/")) {
			t.Fatalf("row %d (%s) is off screen:\n%s", i, prRef(m.view[i]), viewOf(m))
		}
	}
	m, _ = send(t, m, keyMsg("G"))
	v := viewOf(m)
	mustContain(t, v, "talkable#11992", "10/10")
	mustNotContain(t, v, "▼")

	// Scroll so the heading is the third table line: rows 6 and 7 around it.
	m.scroll, m.cursor = 5, 5
	v = viewOf(m)
	lines := strings.Split(v, "\n")
	at := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "merged or closed in the last 24h") })
	if at < 0 {
		t.Fatalf("no heading:\n%s", v)
	}
	if got := m.rowAt(at); got != -1 {
		t.Errorf("the heading line maps to row %d", got)
	}
	if got := m.rowAt(at + 1); got != 7 {
		t.Errorf("the line under the heading maps to row %d, want 7", got)
	}
	if got := m.rowAt(at - 1); got != 6 {
		t.Errorf("the line above the heading maps to row %d, want 6", got)
	}
	m, _ = send(t, m, leftClick(30, at+1))
	if m.cursor != 7 {
		t.Errorf("click under the heading selected %d, want 7", m.cursor)
	}
	m, _ = send(t, m, leftClick(30, at))
	if m.cursor != 7 {
		t.Errorf("click on the heading moved the cursor to %d", m.cursor)
	}

	// The wheel keeps the cursor on a drawn row.
	for range 4 {
		m, _ = send(t, m, tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 10, Y: 5})
		if !containsSplitRef(viewOf(m), strings.TrimPrefix(prRef(m.view[m.cursor]), "talkable/")) {
			t.Fatalf("wheel left the cursor row %d off screen:\n%s", m.cursor, viewOf(m))
		}
	}
}

// RenderPRBoard (output that is not interactive, and tests) draws the
// section too.
func TestRenderPRBoardRecentlyClosed(t *testing.T) {
	out := ansi.Strip(RenderPRBoard(recentRows(), 200, PRBoardOptions{Now: func() time.Time { return boardNow }, SelfLogins: boardSelf, RecentClosed: 24 * time.Hour, Layout: LayoutOneLine}))
	lines := strings.Split(out, "\n")
	at := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "merged or closed in the last 24h (3)") })
	if at < 0 || !strings.Contains(lines[at+1], "#11991") || !strings.Contains(lines[at+2], "merged · unreviewed") {
		t.Fatalf("section:\n%s", out)
	}
	mustContain(t, lines[1], "1 merged unreviewed")
	if testing.Verbose() {
		t.Log("\n" + out)
	}
}

// The window reads as the configuration wrote it.
func TestRecentWindowText(t *testing.T) {
	for d, want := range map[time.Duration]string{
		24 * time.Hour: "24h", 6 * time.Hour: "6h", 72 * time.Hour: "3d", 90 * time.Minute: "90m", 36 * time.Hour: "36h",
	} {
		if got := recentWindow(d); got != want {
			t.Errorf("recentWindow(%v) = %q, want %q", d, got, want)
		}
	}
}
