package tui

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden frames under testdata/")

// checkGolden compares got with testdata/name byte for byte; -update
// rewrites the file.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s: the frame differs from the golden (ANSI stripped):\n--- got\n%s\n--- want\n%s", name, ansi.Strip(got), ansi.Strip(string(want)))
	}
}

// At a width where every column fits with a 40-cell title the board puts
// each PR on one line, byte for byte as it did before the two-line layout:
// the goldens were drawn by that code (the cursor row, a muted row, the
// recently closed section and its flagged row included). Only the key
// hints, the frame's last line, changed since: they name L. 212 cells is
// the narrowest such width for these rows.
func TestBoardWideFrameIsTheOneLineTable(t *testing.T) {
	for _, w := range []int{260, 212} {
		m := recentBoard(t, w, 40, PRBoardOptions{})
		m, _ = send(t, m, keys("j", "j")...)
		if m.rowHeight() != 1 {
			t.Fatalf("width %d: auto put each PR on %d lines", w, m.rowHeight())
		}
		frame := m.View().Content
		cut := strings.LastIndexByte(frame, '\n')
		checkGolden(t, "board_"+strconv.Itoa(w)+".golden", frame[:cut])
		mustContain(t, ansi.Strip(frame[cut+1:]), "s/S sort · L layout · tab overview")
		checkGolden(t, "board_render_"+strconv.Itoa(w)+".golden", RenderPRBoard(recentRows(), w,
			PRBoardOptions{Now: func() time.Time { return boardNow }, SelfLogins: boardSelf, RecentClosed: 24 * time.Hour}))
	}
}

// boardHeadings are the frame's column heading lines, ANSI stripped.
func boardHeadings(m prBoardModel) []string {
	lines := strings.Split(viewOf(m), "\n")
	return lines[boardHeadingY : boardHeadingY+m.rowHeight()]
}

// boardColAt is where column c's heading is: its first cell, its screen
// row and its width.
func boardColAt(t *testing.T, m prBoardModel, c prbCol) (x, y, w int) {
	t.Helper()
	lay := m.tableLayout(m.painter(), m.viewWidth())
	for li := range lay.height() {
		l := lay.line(li)
		if i := slices.Index(l.cols, c); i >= 0 {
			return colStart(l.widths, l.indent, i), boardHeadingY + li, l.widths[i]
		}
	}
	t.Fatalf("column %s is not shown", prbColTitles[c])
	return 0, 0, 0
}

// Auto keeps a PR on one line while every column fits at its content width
// with a 40-cell title (205 cells for these rows), and puts it on two lines
// below that, every column still there; on a very narrow screen each line
// drops its own columns, the second SINCE REVIEW then LAST REVIEW, the
// first ASSIGNEE, then AUTHOR, then REQUESTED.
func TestBoardAutoLayoutByWidth(t *testing.T) {
	line1 := []string{"REPO", "#", "TITLE", "AUTHOR", "ASSIGNEE", "UPDATED", "REQUESTED"}
	line2 := []string{"STATE", "LAST REVIEW", "FINDINGS", "CI", "SINCE REVIEW", "REVIEWERS"}
	for _, c := range []struct {
		width, lines int
		hidden       []string
	}{
		{260, 1, nil},
		{205, 1, nil},
		{204, 2, nil},
		{140, 2, nil},
		{88, 2, nil},
		{86, 2, []string{"ASSIGNEE"}},
		{80, 2, []string{"ASSIGNEE", "SINCE REVIEW"}},
		{70, 2, []string{"ASSIGNEE", "AUTHOR", "SINCE REVIEW"}},
		{60, 2, []string{"ASSIGNEE", "AUTHOR", "SINCE REVIEW", "LAST REVIEW"}},
		{55, 2, []string{"ASSIGNEE", "AUTHOR", "REQUESTED", "SINCE REVIEW", "LAST REVIEW"}},
	} {
		m, _, _ := newBoard(t, c.width, 30, PRBoardOptions{})
		if got := m.rowHeight(); got != c.lines {
			t.Errorf("width %d: %d lines per PR, want %d", c.width, got, c.lines)
			continue
		}
		heads := boardHeadings(m)
		for i, set := range [][]string{line1, line2} {
			head := heads[0]
			if c.lines == 2 {
				head = heads[i]
			}
			for _, title := range set {
				if shown := !slices.Contains(c.hidden, title); strings.Contains(head, title) != shown {
					t.Errorf("width %d: heading line %q shows %s = %v, want %v", c.width, head, title, !shown, shown)
				}
			}
		}
		if c.lines == 2 && (strings.Contains(heads[0], "STATE") || strings.Contains(heads[1], "TITLE") || !strings.HasPrefix(heads[1], "    STATE")) {
			t.Errorf("width %d: headings %q", c.width, heads)
		}
		v := viewOf(m)
		if w := maxLineWidth(v); w > c.width {
			t.Errorf("width %d: a line is %d cells wide:\n%s", c.width, w, v)
		}
		if c.lines == 2 && c.width >= 140 {
			lines := strings.Split(v, "\n")
			at := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "#11920") })
			mustContain(t, lines[at], "Fix referral widget alignment on mobile Safari", "ann", "bob", "3h")
			mustContain(t, lines[at+1], "reviewed", "approved 2h", "3c", "★zhuravel✔")
			if !strings.HasPrefix(lines[at+1], "    ") {
				t.Errorf("width %d: the second line is not indented: %q", c.width, lines[at+1])
			}
		}
	}
}

// In two lines the columns line up within each line across rows, the
// second line indented by four cells and dimmed: a run without a color of
// its own takes the dim, a state pill keeps its colors.
func TestBoardTwoLineFrame(t *testing.T) {
	m := recentBoard(t, 140, 40, PRBoardOptions{})
	if m.rowHeight() != 2 {
		t.Fatalf("auto at 140 cells: %d lines per PR", m.rowHeight())
	}
	v := viewOf(m)
	lines := strings.Split(v, "\n")
	heads := boardHeadings(m)
	title, last := strings.Index(heads[0], "TITLE"), strings.Index(heads[1], "LAST REVIEW")
	rows := 0
	for i, l := range lines[boardHeadingY+3:] {
		if !strings.Contains(l, "#") || strings.Contains(l, "merged or closed") {
			continue
		}
		rows++
		second := lines[boardHeadingY+3+i+1]
		if ansi.Cut(l, title, title+1) == " " || ansi.Cut(l, title-1, title) != " " {
			t.Errorf("a title does not start under TITLE (%d): %q", title, l)
		}
		if ansi.Cut(second, last, last+1) == " " || ansi.Cut(second, last-1, last) != " " {
			t.Errorf("a last review does not start under LAST REVIEW (%d): %q", last, second)
		}
		if !strings.HasPrefix(second, "    ") || strings.Contains(second, "#") {
			t.Errorf("second line %q", second)
		}
	}
	if rows != len(m.view) {
		t.Fatalf("%d rows drawn, want %d:\n%s", rows, len(m.view), v)
	}
	p := m.painter()
	lay := m.tableLayout(p, 140)
	r := m.view[slices.IndexFunc(m.view, func(r PRBoardRow) bool { return r.Number == 11920 })]
	two := p.rowLines(r, lay, 140, false)
	dim := lipgloss.NewStyle().Foreground(p.pal.dim)
	if !hasStyled(two[1], dim, "2h") || !strings.Contains(two[1], p.stateCell("reviewed").render(nil)) {
		t.Errorf("the second line is not dimmed, or its pill lost its colors: %q", two[1])
	}
	if hasStyled(two[0], dim, "Fix referral widget alignment on mobile Safari") {
		t.Error("the first line is dimmed")
	}
	if testing.Verbose() {
		t.Log("\n" + v)
		wide := recentBoard(t, 260, 40, PRBoardOptions{})
		t.Log("\n" + viewOf(wide))
	}
}

// L cycles auto, one line and two lines, says which in the footer, names a
// layout other than auto in the title bar (dimmed), tells LayoutChanged
// and acts on nothing; an unknown layout opens as auto.
func TestBoardLayoutKey(t *testing.T) {
	var heard []PRLayout
	m, _, act := newBoard(t, 140, 30, PRBoardOptions{LayoutChanged: func(l PRLayout) { heard = append(heard, l) }})
	for _, step := range []struct {
		want  PRLayout
		lines int
		note  string
	}{
		{LayoutOneLine, 1, "layout: one line per PR"},
		{LayoutTwoLines, 2, "layout: two lines per PR"},
		{LayoutAuto, 2, "layout: auto"},
	} {
		m, _ = send(t, m, keyMsg("L"))
		title := lineWith(t, m.View().Content, "pull requests")
		if m.layout != step.want || m.rowHeight() != step.lines {
			t.Fatalf("L: layout %s, %d lines per PR; want %s, %d", m.layout, m.rowHeight(), step.want, step.lines)
		}
		mustContain(t, viewOf(m), step.note)
		label := step.want != LayoutAuto
		if strings.Contains(title, m.st.Dim.Render(string(step.want))) != label || strings.Contains(ansi.Strip(title), "auto") {
			t.Errorf("%s: title bar %q", step.want, ansi.Strip(title))
		}
	}
	if !slices.Equal(heard, []PRLayout{LayoutOneLine, LayoutTwoLines, LayoutAuto}) {
		t.Errorf("LayoutChanged heard %v", heard)
	}
	if m.confirm != nil || len(act.list()) != 0 {
		t.Errorf("L acted: confirm %v calls %v", m.confirm, act.list())
	}

	wide, _, _ := newBoard(t, 260, 30, PRBoardOptions{Layout: LayoutTwoLines})
	if wide.rowHeight() != 2 {
		t.Error("two lines asked for: a wide screen still takes two")
	}
	narrow, _, _ := newBoard(t, 120, 30, PRBoardOptions{Layout: LayoutOneLine})
	if narrow.rowHeight() != 1 || maxLineWidth(viewOf(narrow)) > 120 {
		t.Error("one line asked for: a narrow screen keeps one, dropping columns")
	}
	bogus, _, _ := newBoard(t, 140, 30, PRBoardOptions{Layout: "three-line"})
	if bogus.layout != LayoutAuto {
		t.Errorf("an unknown layout opens as %q", bogus.layout)
	}

	mustContain(t, viewOf(wide), "L layout")
	help, _ := send(t, m, keyMsg("?"))
	mustContain(t, viewOf(help), "layout: auto, one line, two lines per PR")
}

// checkWholeRows fails unless the table draws whole rows from m.scroll:
// both lines of each, in order, the cursor's highlighted on both (its
// background to the edge), and the last one above the footer.
func checkWholeRows(t *testing.T, m prBoardModel) {
	t.Helper()
	w := m.viewWidth()
	raw := strings.Split(m.View().Content, "\n")
	rh := m.rowHeight()
	end, heading := m.visible(m.scroll)
	if m.cursor < m.scroll || m.cursor >= end {
		t.Fatalf("cursor %d off the rows drawn [%d, %d)", m.cursor, m.scroll, end)
	}
	p := m.painter()
	lay := m.tableLayout(p, w)
	at := boardHeadingY + tableChrome(rh)
	for i := m.scroll; i < end; i++ {
		if heading && i == m.section {
			at++
		}
		want := p.rowLines(m.view[i], lay, w, i == m.cursor)
		if len(want) != rh || at+rh > len(raw)-2 {
			t.Fatalf("row %d: %d lines from line %d of %d", i, len(want), at, len(raw))
		}
		for j := range want {
			if raw[at+j] != want[j] {
				t.Fatalf("row %d line %d (scroll %d cursor %d):\n%q\nwant\n%q", i, j, m.scroll, m.cursor, ansi.Strip(raw[at+j]), ansi.Strip(want[j]))
			}
		}
		if i == m.cursor {
			plain := p.rowLines(m.view[i], lay, w, false)
			for j := range want {
				if want[j] == plain[j] || ansi.StringWidth(want[j]) != w {
					t.Fatalf("the cursor row's line %d is not highlighted to the edge: %q", j, want[j])
				}
			}
		}
		at += rh
	}
}

// Keys and the wheel move by PR: j/k a row, page up and down a screenful
// of rows less one; the table never shows half a row at the top or the
// bottom, and the cursor row is highlighted on both its lines.
func TestBoardTwoLineCursorAndScroll(t *testing.T) {
	m := stormBoard(t, 60) // 120 x 40: 33 table lines, 16 rows of two
	if m.rowHeight() != 2 || m.tableRows() != 16 {
		t.Fatalf("%d lines per PR, %d rows a screen", m.rowHeight(), m.tableRows())
	}
	for range len(m.view) + 2 {
		m, _ = send(t, m, keyMsg("j"))
		checkWholeRows(t, m)
	}
	if m.cursor != len(m.view)-1 || m.scroll != len(m.view)-16 {
		t.Fatalf("at the bottom: cursor %d scroll %d", m.cursor, m.scroll)
	}
	for range 20 {
		m, _ = send(t, m, keyMsg("k"))
		checkWholeRows(t, m)
	}
	m, _ = send(t, m, keyMsg("g"), keyMsg("pgdown"))
	if m.cursor != 15 {
		t.Errorf("pgdown from the top: cursor %d, want 15 (a screenful of rows less one)", m.cursor)
	}
	checkWholeRows(t, m)
	m, _ = send(t, m, keyMsg("pgdown"), keyMsg("pgup"))
	if m.cursor != 15 {
		t.Errorf("pgdown then pgup: cursor %d, want 15", m.cursor)
	}
	checkWholeRows(t, m)
	for range 25 {
		m, _ = send(t, m, wheelDown())
		checkWholeRows(t, m)
	}
	m, _ = send(t, m, keyMsg("G"))
	checkWholeRows(t, m)
	mustContain(t, viewOf(m), "60/60")
	mustNotContain(t, viewOf(m), "▼")

	// A screen with room for the heading line but not a third row.
	odd, _, _ := newBoard(t, 140, 12, PRBoardOptions{}) // 8 body lines: 5 for rows
	for range len(odd.view) {
		checkWholeRows(t, odd)
		odd, _ = send(t, odd, keyMsg("j"))
	}
}

// The recently closed section keeps its one-line heading between whole
// two-line rows, and a merged unreviewed row keeps its red pill on its
// second line while the rest of it is dimmed.
func TestBoardTwoLineRecentlyClosed(t *testing.T) {
	m := recentBoard(t, 140, 40, PRBoardOptions{})
	lines := strings.Split(viewOf(m), "\n")
	at := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "merged or closed in the last 24h (3)") })
	if at < 0 || !strings.Contains(lines[at-2], "#11000") || !strings.Contains(lines[at-1], "not reviewed") ||
		!strings.Contains(lines[at+1], "#11991") || !strings.Contains(lines[at+2], "closed") || !strings.Contains(lines[at+4], "merged · unreviewed") {
		t.Fatalf("heading at line %d:\n%s", at, strings.Join(lines, "\n"))
	}
	raw := strings.Split(m.View().Content, "\n")
	if !strings.Contains(raw[at+4], m.pal.pills["merged_unreviewed"].Render(" merged · unreviewed ")) {
		t.Errorf("the flag lost its pill: %q", raw[at+4])
	}
	dim := lipgloss.NewStyle().Foreground(m.pal.dim)
	if !hasStyled(raw[at+3], dim, "Retire the coupon v1 API") {
		t.Errorf("a recently closed row is not dimmed: %q", raw[at+3])
	}

	// A click on the heading selects nothing; one on a closed row's second
	// line selects it.
	before := m.cursor
	m, _ = send(t, m, leftClick(20, at))
	if m.cursor != before {
		t.Errorf("a click on the heading moved the cursor to %d", m.cursor)
	}
	m, _ = send(t, m, leftClick(20, at+4))
	if r, _ := m.selected(); r.Number != 11990 {
		t.Errorf("a click on #11990's second line selected #%d", r.Number)
	}

	// On a short screen every row scrolls into view whole, the heading too.
	short := recentBoard(t, 140, 12, PRBoardOptions{})
	for i := range len(short.view) {
		short.moveTo(i)
		short.fixScroll()
		checkWholeRows(t, short)
	}
	short, _ = send(t, short, keyMsg("G"))
	mustContain(t, viewOf(short), "#11992", "10/10")
	if testing.Verbose() {
		t.Log("\n" + viewOf(short))
	}
}

// The mouse in two lines: a click on either line of a row selects it, a
// double click on the second opens the card and a right click there the
// row's menu; a click on a heading of either line sorts by its column
// (again reverses); dragging a gap on the second heading line resizes the
// column left of it within that line, and W resets it.
func TestBoardTwoLineMouse(t *testing.T) {
	kept := &fakeWidths{}
	m, _, act := newBoard(t, 140, 30, PRBoardOptions{Widths: kept})
	now := boardNow
	m.opts.Now = func() time.Time { return now }
	if m.rowHeight() != 2 {
		t.Fatal("auto at 140 cells: one line per PR")
	}

	m, _ = send(t, m, leftClick(30, boardRowY(m, 3)+1))
	if m.cursor != 3 || m.mode != prbTable {
		t.Fatalf("a click on row 3's second line: cursor %d mode %d", m.cursor, m.mode)
	}
	m, _ = send(t, m, leftClick(60, boardRowY(m, 2)))
	if m.cursor != 2 {
		t.Fatalf("a click on row 2's first line: cursor %d", m.cursor)
	}
	now = now.Add(time.Second)
	m, _ = send(t, m, leftClick(10, boardRowY(m, 4)+1))
	now = now.Add(100 * time.Millisecond)
	m, _ = send(t, m, leftClick(50, boardRowY(m, 4)+1))
	if m.mode != prbDetail || m.cursor != 4 {
		t.Fatalf("a double click on row 4's second line: mode %d cursor %d", m.mode, m.cursor)
	}
	m, _ = send(t, m, keyMsg("esc"), rightClick(40, boardRowY(m, 5)+1))
	if !m.menu.open || m.cursor != 5 || m.menu.rowKey != prRef(m.view[5]) {
		t.Fatalf("a right click on row 5's second line: menu %v cursor %d", m.menu.open, m.cursor)
	}
	m, _ = send(t, m, keyMsg("esc"))

	x, y, _ := boardColAt(t, m, colState)
	if y != boardHeadingY+1 {
		t.Fatalf("STATE heads the second line, not screen row %d", y)
	}
	m, _ = send(t, m, leftClick(x+1, y))
	if m.sort != SortState || !m.desc {
		t.Fatalf("a click on STATE: sort %s desc %v", m.sort, m.desc)
	}
	mustContain(t, boardHeadings(m)[1], "STATE ↓")
	m, _ = send(t, m, leftClick(x+1, y))
	if m.sort != SortState || m.desc {
		t.Fatalf("a second click on STATE: sort %s desc %v", m.sort, m.desc)
	}
	ux, uy, uw := boardColAt(t, m, colUpdated)
	m, _ = send(t, m, leftClick(ux+uw/2, uy))
	if m.sort != SortUpdated || !m.desc || uy != boardHeadingY {
		t.Fatalf("a click on UPDATED: sort %s desc %v", m.sort, m.desc)
	}

	lx, ly, lw := boardColAt(t, m, colLastReview)
	tx, _, tw := boardColAt(t, m, colTitle)
	gap := lx + lw
	m, _ = send(t, m, leftClick(gap, ly), motion(gap+5, ly))
	if _, _, got := boardColAt(t, m, colLastReview); got != lw+5 {
		t.Fatalf("a drag 5 right on the gap after LAST REVIEW: %d, want %d", got, lw+5)
	}
	m, cmd := send(t, m, release(gap+5, ly))
	m = run(t, m, cmd)
	if got := kept.get(widthsBoard); got["last_review"] != lw+5 || len(got) != 1 {
		t.Fatalf("kept %v", got)
	}
	if x2, _, w2 := boardColAt(t, m, colTitle); x2 != tx || w2 != tw || m.rowHeight() != 2 {
		t.Errorf("the drag moved the first line: title at %d, %d wide (was %d, %d)", x2, w2, tx, tw)
	}
	if maxLineWidth(viewOf(m)) > 140 {
		t.Error("a line overflows the screen")
	}
	m, cmd = send(t, m, keyMsg("W"))
	m = run(t, m, cmd)
	if _, _, got := boardColAt(t, m, colLastReview); got != lw || len(kept.get(widthsBoard)) != 0 {
		t.Errorf("after W: LAST REVIEW %d wide, kept %v", got, kept.get(widthsBoard))
	}
	if calls := act.list(); len(calls) != 0 {
		t.Errorf("the mouse acted: %v", calls)
	}
}

// The layout is part of the fingerprint: L changes the frame, and so does a
// width at which auto chooses differently; every cached frame is the one an
// uncached render draws.
func TestBoardFrameCacheFollowsTheLayout(t *testing.T) {
	m := stormBoard(t, 60)
	a, b := m, m
	b.layout = LayoutOneLine
	if a.rowsKey(120) == b.rowsKey(120) || a.frameKey() == b.frameKey() {
		t.Error("the layout and frame keys must include the layout")
	}
	wide, _ := send(t, m, tea.WindowSizeMsg{Width: 260, Height: 40})
	if wide.frameKey().rowLines != 1 || m.frameKey().rowLines != 2 {
		t.Errorf("the frame key's lines per PR: %d at 260 cells, %d at 120", wide.frameKey().rowLines, m.frameKey().rowLines)
	}
	runSteps(t, m, uncachedBoard, []step{
		{name: "one line", msgs: keys("L"), want: "1-line"},
		{name: "down", msgs: keys("j")},
		{name: "two lines", msgs: keys("L"), want: "2-line"},
		{name: "auto", msgs: keys("L"), want: "layout: auto"},
		{name: "wide: auto takes one line", msgs: []tea.Msg{tea.WindowSizeMsg{Width: 260, Height: 40}}},
		{name: "narrow again", msgs: []tea.Msg{tea.WindowSizeMsg{Width: 120, Height: 40}}},
		{name: "up in two lines", msgs: keys("k")},
	})
}
