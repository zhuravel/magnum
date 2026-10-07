package tui

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// fakeWidths keeps column widths in memory, per screen.
type fakeWidths struct {
	mu    sync.Mutex
	kept  map[string]map[string]int
	saves int
	err   error
}

func (f *fakeWidths) LoadWidths(_ context.Context, screen string) (map[string]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.kept[screen]), f.err
}

func (f *fakeWidths) SaveWidths(_ context.Context, screen string, w map[string]int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves++
	if f.err != nil {
		return f.err
	}
	if f.kept == nil {
		f.kept = map[string]map[string]int{}
	}
	f.kept[screen] = maps.Clone(w)
	return nil
}

func (f *fakeWidths) get(screen string) map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.kept[screen])
}

func leftClick(x, y int) tea.Msg  { return tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft} }
func rightClick(x, y int) tea.Msg { return tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseRight} }
func motion(x, y int) tea.Msg     { return tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseLeft} }
func release(x, y int) tea.Msg    { return tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft} }
func wheelDown() tea.Msg          { return tea.MouseWheelMsg{X: 10, Y: 10, Button: tea.MouseWheelDown} }
func wheelUp() tea.Msg            { return tea.MouseWheelMsg{X: 10, Y: 10, Button: tea.MouseWheelUp} }

// boardHeadingY is the screen row of the board's column headings (the
// title bar and the summary line come first).
const boardHeadingY = 2

// boardRowY is the screen row of the board's row i.
func boardRowY(m prBoardModel, i int) int { return boardHeadingY + prbTableChrome + i - m.scroll }

// boardCol is where column c starts on the board's heading line and how
// wide it is.
func boardCol(t *testing.T, m prBoardModel, c prbCol) (start, width int) {
	t.Helper()
	lay := m.tableLayout(m.painter(), m.viewWidth())
	i := slices.Index(lay.cols, c)
	if i < 0 {
		t.Fatalf("column %s is not shown", prbColTitles[c])
	}
	return colStart(lay.widths, prbMarkW, i), lay.widths[i]
}

// boardRow is the index of ref on the board.
func boardRow(t *testing.T, m prBoardModel, ref string) int {
	t.Helper()
	i := slices.Index(boardRefs(m), ref)
	if i < 0 {
		t.Fatalf("%s is not on the board", ref)
	}
	return i
}

// run executes cmd and feeds its messages back into m.
func run[M tea.Model](t *testing.T, m M, cmd tea.Cmd) M {
	t.Helper()
	m, _ = send(t, m, execCmd(cmd)...)
	return m
}

func (f *fakeActions) list() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// A click on a heading sorts by its column and a second click reverses
// it; the heading shows the column and the direction. Columns without a
// sort ignore the click, and nothing acts.
func TestBoardHeadingClickSorts(t *testing.T) {
	m, _, act := newBoard(t, 180, 30, PRBoardOptions{})
	state, _ := boardCol(t, m, colState)
	m, _ = send(t, m, leftClick(state+1, boardHeadingY))
	if m.sort != SortState || !m.desc {
		t.Fatalf("after a click on STATE: sort %s desc %v", m.sort, m.desc)
	}
	mustContain(t, viewOf(m), "STATE ↓")
	if r, _ := m.selected(); boardRefs(m)[0] != "talkable/talkable#11902" || prRef(r) == "" {
		t.Errorf("most urgent first: %v", boardRefs(m))
	}
	m, _ = send(t, m, leftClick(state+1, boardHeadingY))
	if m.sort != SortState || m.desc {
		t.Fatalf("a second click must reverse: sort %s desc %v", m.sort, m.desc)
	}
	mustContain(t, viewOf(m), "STATE ↑")

	for c, want := range map[prbCol]PRSort{colLastReview: SortLastReview, colSince: SortChanges, colReviewers: SortReviewerActivity, colRequested: SortRequested, colUpdated: SortUpdated} {
		x, w := boardCol(t, m, c)
		m, _ = send(t, m, leftClick(x+w/2, boardHeadingY))
		if m.sort != want || !m.desc {
			t.Errorf("click on %s: sort %s desc %v, want %s newest first", prbColTitles[c], m.sort, m.desc, want)
		}
	}
	m, _ = send(t, m, leftClick(state+1, boardHeadingY), leftClick(state+1, boardHeadingY)) // state, then reversed
	m, _ = send(t, m, keyMsg("s"))                                                          // s still cycles from there
	if m.sort != SortUpdated || !m.desc {
		t.Errorf("s after a heading click: %s desc %v", m.sort, m.desc)
	}
	before, order := m.sort, boardRefs(m)
	for _, c := range []prbCol{colRef, colTitle, colAuthor, colAssignee} {
		x, w := boardCol(t, m, c)
		m, _ = send(t, m, leftClick(x+w/2, boardHeadingY))
		if m.sort != before || !m.desc || !slices.Equal(boardRefs(m), order) {
			t.Errorf("click on %s changed the sort to %s desc %v", prbColTitles[c], m.sort, m.desc)
		}
	}
	if calls := act.list(); len(calls) != 0 {
		t.Errorf("heading clicks acted: %v", calls)
	}
}

// The wheel scrolls the table (the cursor keeps to the rows on screen),
// the card and the help, three lines a notch, and never acts.
func TestBoardWheelScrollsWithoutActing(t *testing.T) {
	m := stormBoard(t, 60)
	act := m.act.(*fakeActions)
	m, cmd := send(t, m, wheelDown())
	if m.scroll != wheelStep || m.cursor != wheelStep || cmd != nil {
		t.Fatalf("one notch down: scroll %d cursor %d cmd %v", m.scroll, m.cursor, cmd != nil)
	}
	m, _ = send(t, m, keyMsg("j"), keyMsg("j"), wheelUp())
	if m.scroll != 0 || m.cursor != wheelStep+2 {
		t.Fatalf("a notch up keeps the cursor on screen: scroll %d cursor %d", m.scroll, m.cursor)
	}
	for range 40 {
		m, _ = send(t, m, wheelDown())
	}
	if want := len(m.view) - m.tableHeight(); m.scroll != want || m.cursor < m.scroll {
		t.Fatalf("at the bottom: scroll %d (want %d) cursor %d", m.scroll, want, m.cursor)
	}
	if m.mode != prbTable || m.confirm != nil || m.busy != "" || len(act.list()) != 0 {
		t.Fatalf("the wheel acted: mode %d confirm %v busy %q calls %v", m.mode, m.confirm, m.busy, act.list())
	}

	small, _, _ := newBoard(t, 80, 12, PRBoardOptions{})
	small, _ = send(t, small, keyMsg("enter"), wheelDown())
	if small.mode != prbDetail || small.detailScroll != wheelStep {
		t.Errorf("card: mode %d scroll %d", small.mode, small.detailScroll)
	}
	small, _ = send(t, small, wheelUp(), wheelUp())
	if small.detailScroll != 0 {
		t.Errorf("card scrolled above its top: %d", small.detailScroll)
	}
	small, _ = send(t, small, keyMsg("esc"), keyMsg("?"), wheelDown())
	if small.mode != prbHelp || small.helpScroll != wheelStep {
		t.Errorf("help: mode %d scroll %d", small.mode, small.helpScroll)
	}
}

// A click selects the row under it; two clicks on one row within 400ms
// open its card, as enter does. Slower clicks, clicks on two rows and
// clicks off the rows do not.
func TestBoardClickSelectsAndDoubleClickOpens(t *testing.T) {
	m, _, act := newBoard(t, 160, 30, PRBoardOptions{})
	now := boardNow
	m.opts.Now = func() time.Time { return now }
	m, _ = send(t, m, leftClick(30, boardRowY(m, 2)))
	if m.cursor != 2 || m.mode != prbTable {
		t.Fatalf("click: cursor %d mode %d", m.cursor, m.mode)
	}
	now = now.Add(500 * time.Millisecond)
	m, _ = send(t, m, leftClick(30, boardRowY(m, 2)))
	if m.mode != prbTable {
		t.Fatal("two clicks 500ms apart opened the card")
	}
	now = now.Add(100 * time.Millisecond)
	m, _ = send(t, m, leftClick(30, boardRowY(m, 2)))
	if m.mode != prbDetail || m.cursor != 2 {
		t.Fatalf("double click: mode %d cursor %d", m.mode, m.cursor)
	}
	mustContain(t, viewOf(m), prRef(m.view[2]))

	m, _ = send(t, m, keyMsg("esc"))
	m, _ = send(t, m, leftClick(30, boardRowY(m, 3)), leftClick(30, boardRowY(m, 4)))
	if m.mode != prbTable || m.cursor != 4 {
		t.Fatalf("clicks on two rows: mode %d cursor %d", m.mode, m.cursor)
	}
	for _, y := range []int{0, 1, boardHeadingY + 1, boardRowY(m, len(m.view)), 28, 29} {
		m, _ = send(t, m, leftClick(30, y))
		if m.cursor != 4 || m.mode != prbTable {
			t.Errorf("a click on screen row %d: cursor %d mode %d", y, m.cursor, m.mode)
		}
	}
	if len(act.list()) != 0 {
		t.Errorf("clicks acted: %v", act.list())
	}

	// While filtering, a double click keeps the filter and opens the card.
	f, _, _ := newBoard(t, 160, 30, PRBoardOptions{})
	f, _ = send(t, f, keyMsg("/"))
	f, _ = send(t, f, typed("talkable")...)
	f, _ = send(t, f, leftClick(30, boardRowY(f, 1)), leftClick(30, boardRowY(f, 1)))
	if f.mode != prbDetail || f.filtering || f.filter.Value() != "talkable" {
		t.Errorf("double click while filtering: mode %d filtering %v filter %q", f.mode, f.filtering, f.filter.Value())
	}
}

// Dragging a heading gap resizes the column on its left, live, within a
// minimum of 4 cells and the screen; the widths are kept on release and
// come back on the next start; W resets them.
func TestBoardDragResizesAndKeepsWidths(t *testing.T) {
	kept := &fakeWidths{}
	m, _, _ := newBoard(t, 190, 30, PRBoardOptions{Widths: kept})
	x, w := boardCol(t, m, colAuthor)
	gap := x + w
	cols := len(m.tableLayout(m.painter(), 190).cols)
	m, _ = send(t, m, leftClick(gap, boardHeadingY))
	if m.sort != SortUpdated {
		t.Fatal("a click on a gap sorted")
	}
	m, _ = send(t, m, motion(gap+5, boardHeadingY))
	if _, got := boardCol(t, m, colAuthor); got != w+5 {
		t.Fatalf("drag 5 right: author %d, want %d", got, w+5)
	}
	m, _ = send(t, m, motion(gap-100, boardHeadingY))
	if _, got := boardCol(t, m, colAuthor); got != minColWidth {
		t.Fatalf("drag far left: author %d, want the minimum %d", got, minColWidth)
	}
	m, _ = send(t, m, motion(gap+1000, boardHeadingY))
	lay := m.tableLayout(m.painter(), 190)
	if len(lay.cols) != cols || lay.total() > 190 {
		t.Fatalf("drag far right pushed columns off: %d of %d shown, %d cells", len(lay.cols), cols, lay.total())
	}
	if maxLineWidth(viewOf(m)) > 190 {
		t.Fatal("a line overflows the screen")
	}
	if kept.saves != 0 {
		t.Fatal("saved before the release")
	}
	m, cmd := send(t, m, motion(gap+7, boardHeadingY), release(gap+7, boardHeadingY))
	m = run(t, m, cmd)
	if got := kept.get(widthsBoard); got["author"] != w+7 || len(got) != 1 {
		t.Fatalf("kept %v, want author %d", got, w+7)
	}
	if !strings.Contains(viewOf(m), "AUTHOR") {
		t.Error("the heading lost its title")
	}

	// The title gives up room as before, never the other columns.
	tx, tw := boardCol(t, m, colTitle)
	m, _ = send(t, m, leftClick(tx+tw, boardHeadingY), motion(tx+tw-10, boardHeadingY), release(tx+tw-10, boardHeadingY))
	if _, got := boardCol(t, m, colTitle); got != tw-10 {
		t.Errorf("title dragged 10 left: %d, want %d", got, tw-10)
	}

	// The next board starts with the kept widths.
	next, _, _ := newBoard(t, 190, 30, PRBoardOptions{Widths: kept})
	next = run(t, next, next.saver.load(next.ctx))
	if _, got := boardCol(t, next, colAuthor); got != w+7 {
		t.Fatalf("reloaded author %d, want %d", got, w+7)
	}

	next, cmd = send(t, next, keyMsg("W"))
	next = run(t, next, cmd)
	if got := kept.get(widthsBoard); len(got) != 0 {
		t.Fatalf("W kept %v", got)
	}
	if _, got := boardCol(t, next, colAuthor); got != w {
		t.Fatalf("after W the author is %d, want %d", got, w)
	}
	mustContain(t, viewOf(next), "column widths reset")

	// A failing store says so in the footer.
	kept.err = errors.New("registry locked")
	next, _ = send(t, next, leftClick(gap, boardHeadingY), motion(gap+3, boardHeadingY))
	next, cmd = send(t, next, release(gap+3, boardHeadingY))
	next = run(t, next, cmd)
	mustContain(t, viewOf(next), "could not keep the column widths: registry locked")
}

// Widths loaded after the user already dragged do not undo the drag.
func TestBoardLateWidthsKeepTheDrag(t *testing.T) {
	kept := &fakeWidths{kept: map[string]map[string]int{widthsBoard: {"author": 30, "bogus": 9}}}
	m, _, _ := newBoard(t, 160, 30, PRBoardOptions{Widths: kept})
	load := m.saver.load(m.ctx)
	x, w := boardCol(t, m, colAuthor)
	m, _ = send(t, m, leftClick(x+w, boardHeadingY), motion(x+w+2, boardHeadingY), release(x+w+2, boardHeadingY))
	m = run(t, m, load)
	if _, got := boardCol(t, m, colAuthor); got != w+2 {
		t.Errorf("late widths replaced the drag: author %d, want %d", got, w+2)
	}
	fresh, _, _ := newBoard(t, 160, 30, PRBoardOptions{Widths: kept})
	fresh = run(t, fresh, fresh.saver.load(fresh.ctx))
	if fresh.widths[colAuthor] != 30 || fresh.widths.named()["bogus"] != 0 {
		t.Errorf("loaded widths %v", fresh.widths.named())
	}
}

// menuState is each menu item's label and whether it applies.
func menuState(items []menuItem) map[string]bool {
	out := map[string]bool{}
	for _, it := range items {
		out[it.label] = it.ok
	}
	return out
}

// A right click on a row selects it and opens its menu: every action with
// its key, the ones that cannot apply dimmed. Enter or a click runs an
// item through its key (y/N included); esc, a click outside or a second
// right click close it.
func TestBoardRightClickMenu(t *testing.T) {
	m, _, act := newBoard(t, 160, 30, PRBoardOptions{})
	i := boardRow(t, m, "talkable/talkable#11920") // reviewed, pinned, in a slot
	y := boardRowY(m, i)
	m, _ = send(t, m, rightClick(40, y))
	if !m.menu.open || m.cursor != i {
		t.Fatalf("right click: menu %v cursor %d, want %d", m.menu.open, m.cursor, i)
	}
	v := viewOf(m)
	for _, it := range m.menuItems() {
		mustContain(t, v, it.label)
	}
	mustContain(t, v, "kill review", "reset column widths", "enter")
	if maxLineWidth(v) > 160 || lineCount(v) != 30 {
		t.Errorf("the menu broke the frame: %d cells, %d lines", maxLineWidth(v), lineCount(v))
	}
	want := map[string]bool{
		"review": true, "fresh review": true, "simplify review": true, "kill review": false, "ignore": true,
		"open pane": true, "browser": true, "tracker": false, "pin": false, "unpin": true, "release": false, // pinned: unpin first
		"mute": true, "unmute": false, "details": true, "reset column widths": false,
		"approve": false, "request changes": false, // the fixture has no findings for #11920
		"withdraw approval": false, // nor an approval magnum posted as the operator
		"snooze 2h":         true,
	}
	if got := menuState(m.menuItems()); !maps.Equal(got, want) {
		t.Errorf("menu of #11920:\n got %v\nwant %v", got, want)
	}
	if len(m.menuItems()) != 19 {
		t.Errorf("%d items", len(m.menuItems()))
	}

	m, _ = send(t, m, keyMsg("esc"))
	if m.menu.open {
		t.Fatal("esc left the menu open")
	}
	m, _ = send(t, m, rightClick(40, y), rightClick(40, y))
	if m.menu.open {
		t.Fatal("a second right click left the menu open")
	}
	m, _ = send(t, m, rightClick(40, y), leftClick(2, 0))
	if m.menu.open || m.sort != SortUpdated || m.cursor != i {
		t.Fatalf("a click outside: menu %v sort %s cursor %d", m.menu.open, m.sort, m.cursor)
	}

	// Keyboard: j/k move, enter runs review through its y/N question.
	m, _ = send(t, m, rightClick(40, y), keyMsg("j"), keyMsg("j"), keyMsg("k"))
	if m.menu.sel != 1 {
		t.Fatalf("j j k: item %d", m.menu.sel)
	}
	m, _ = send(t, m, keyMsg("k"), keyMsg("enter"))
	if m.menu.open || m.confirm == nil || !strings.Contains(m.confirm.question, "Review talkable#11920 now") {
		t.Fatalf("enter on review: menu %v confirm %+v", m.menu.open, m.confirm)
	}
	mustContain(t, viewOf(m), "y/N")
	m, cmd := send(t, m, keyMsg("y"))
	m = run(t, m, cmd)
	if calls := act.list(); len(calls) != 1 || !strings.HasPrefix(calls[0], "review talkable/talkable#11920 fresh=false") {
		t.Fatalf("calls %v", calls)
	}

	// Mouse: a dimmed item does nothing; unpin runs (it does not ask).
	m, _ = send(t, m, rightClick(40, y))
	items := m.menuItems()
	l := placeMenu(items, m.menu.sel, m.menu.x, m.menu.y, 160, 30)
	itemY := func(label string) int {
		return l.y + 1 + slices.IndexFunc(items, func(it menuItem) bool { return it.label == label }) - l.off
	}
	m, _ = send(t, m, leftClick(l.x+3, itemY("pin")))
	if !m.menu.open || len(act.list()) != 1 {
		t.Fatalf("a dimmed item acted: menu %v calls %v", m.menu.open, act.list())
	}
	m, _ = send(t, m, leftClick(l.x, itemY("pin"))) // the border
	if !m.menu.open {
		t.Fatal("a click on the border closed the menu")
	}
	m, cmd = send(t, m, leftClick(l.x+3, itemY("unpin")))
	m = run(t, m, cmd)
	if calls := act.list(); m.menu.open || len(calls) != 2 || calls[1] != "unpin talkable/talkable#11920" {
		t.Fatalf("unpin by click: menu %v calls %v", m.menu.open, calls)
	}

	// An item's own key runs it from the menu: M asks before muting.
	m, _ = send(t, m, rightClick(40, y), keyMsg("M"))
	if m.confirm == nil || !strings.Contains(m.confirm.question, "Mute talkable#11920") {
		t.Fatalf("M in the menu: %+v", m.confirm)
	}
	m, _ = send(t, m, leftClick(40, y)) // a click answers no
	if m.confirm != nil || len(act.list()) != 2 {
		t.Fatalf("a click must cancel the question: %+v %v", m.confirm, act.list())
	}
	mustContain(t, viewOf(m), "cancelled")

	// A running review can be killed; a PR without a slot cannot be released.
	j := boardRow(t, m, "talkable/magnum#42")
	m, _ = send(t, m, rightClick(40, boardRowY(m, j)))
	got := menuState(m.menuItems())
	if !got["kill review"] || got["release"] || !got["pin"] || got["unpin"] || got["review"] {
		t.Errorf("menu of #42: %v", got)
	}
	m, _ = send(t, m, keyMsg("enter")) // review is dimmed while its round runs: nothing happens
	if !m.menu.open || m.confirm != nil {
		t.Fatalf("enter on a dimmed review: menu %v, question %+v", m.menu.open, m.confirm)
	}
	m, _ = send(t, m, keyMsg("esc"))

	// details opens the card; reset column widths applies once a width was dragged.
	m, _ = send(t, m, rightClick(40, boardRowY(m, j)), keyMsg("G"), keyMsg("k"), keyMsg("enter"))
	if m.mode != prbDetail {
		t.Fatalf("details from the menu: mode %d", m.mode)
	}

	// A right click near the corner keeps the menu on screen.
	c, _, _ := newBoard(t, 100, 20, PRBoardOptions{Icons: IconsASCII})
	c, _ = send(t, c, rightClick(99, boardRowY(c, len(c.view)-1)))
	v = viewOf(c)
	if !c.menu.open || maxLineWidth(v) > 100 || lineCount(v) != 20 {
		t.Fatalf("corner menu: open %v, %d cells, %d lines:\n%s", c.menu.open, maxLineWidth(v), lineCount(v), v)
	}
	mustContain(t, v, "+----", "| review")

	// Without actions every action is dimmed.
	none := newPRBoardModel(context.Background(), &fakeBoardSource{}, nil, PRBoardOptions{Now: func() time.Time { return boardNow }})
	none, _ = send(t, none, tea.WindowSizeMsg{Width: 160, Height: 30}, prbDataMsg{rows: boardRows()})
	none, _ = send(t, none, rightClick(40, boardRowY(none, 0)))
	for label, ok := range menuState(none.menuItems()) {
		if ok != (label == "details") {
			t.Errorf("without actions %q applies = %v", label, ok)
		}
	}
}

// m turns the mouse off and on: off, the view asks for no mouse events,
// the footer says so and mouse messages change nothing.
func TestMouseToggle(t *testing.T) {
	var heard []bool
	m, _, _ := newBoard(t, 160, 30, PRBoardOptions{MouseToggled: func(on bool) { heard = append(heard, on) }})
	if m.View().MouseMode != tea.MouseModeCellMotion {
		t.Fatal("the board must ask for mouse events")
	}
	m, _ = send(t, m, keyMsg("m"))
	if m.mouseOn || m.View().MouseMode != tea.MouseModeNone {
		t.Fatal("m left the mouse on")
	}
	mustContain(t, viewOf(m), "mouse off")
	state, _ := boardCol(t, m, colState)
	x, w := boardCol(t, m, colAuthor)
	m, _ = send(t, m, leftClick(30, boardRowY(m, 3)), wheelDown(), rightClick(30, boardRowY(m, 3)),
		leftClick(state+1, boardHeadingY), leftClick(x+w, boardHeadingY), motion(x+w+5, boardHeadingY), release(x+w+5, boardHeadingY))
	if m.cursor != 0 || m.scroll != 0 || m.menu.open || m.sort != SortUpdated || m.widths != (prbWidths{}) {
		t.Fatalf("mouse off still acted: cursor %d scroll %d menu %v sort %s widths %v", m.cursor, m.scroll, m.menu.open, m.sort, m.widths)
	}
	m, _ = send(t, m, keyMsg("m"))
	if !m.mouseOn || !slices.Equal(heard, []bool{false, true}) {
		t.Fatalf("mouse %v, heard %v", m.mouseOn, heard)
	}
	mustNotContain(t, viewOf(m), "mouse off")

	off, _, _ := newBoard(t, 160, 30, PRBoardOptions{NoMouse: true})
	off, _ = send(t, off, leftClick(30, boardRowY(off, 2)))
	if off.mouseOn || off.cursor != 0 || off.View().MouseMode != tea.MouseModeNone {
		t.Fatal("NoMouse must start with the mouse off")
	}

	d, _, _ := newDash(t, 140, 50)
	if d.View().MouseMode != tea.MouseModeCellMotion {
		t.Fatal("the dashboard must ask for mouse events")
	}
	d, _ = send(t, d, keyMsg("m"))
	d, _ = send(t, d, leftClick(10, dashRowY(d, 2)), wheelDown())
	if d.mouseOn || d.cursor != 0 || d.View().MouseMode != tea.MouseModeNone {
		t.Fatal("the dashboard's m left the mouse on")
	}
	mustContain(t, viewOf(d), "mouse off")
	d, _ = send(t, d, keyMsg("w"))
	mustContain(t, viewOf(d), "MANUAL WORKTREES (1)")
}

// Every change a mouse makes shows in the next frame, and changes that
// show nothing (a press on a gap, a release without a drag, mouse events
// while it is off) keep the frame.
func TestBoardFrameCacheFollowsTheMouse(t *testing.T) {
	m := stormBoard(t, 60)
	x, w := boardCol(t, m, colFindings)
	state, _ := boardCol(t, m, colState)
	// Sorting by state takes the arrow off UPDATED, which then fits again
	// and moves STATE to the right.
	sorted, _ := send(t, m, leftClick(state+1, boardHeadingY))
	stateSorted, _ := boardCol(t, sorted, colState)
	findingsSorted, _ := boardCol(t, sorted, colFindings)
	gap := x + w
	steps := []step{
		{name: "press on a gap", msgs: []tea.Msg{leftClick(gap, boardHeadingY)}, same: true},
		{name: "drag", msgs: []tea.Msg{motion(gap-2, boardHeadingY)}},
		{name: "drag on", msgs: []tea.Msg{motion(gap-4, boardHeadingY)}},
		{name: "release", msgs: []tea.Msg{release(gap-4, boardHeadingY)}, same: true},
		{name: "wheel", msgs: []tea.Msg{wheelDown()}},
		{name: "click a row", msgs: []tea.Msg{leftClick(30, 10)}},
		{name: "menu", msgs: []tea.Msg{rightClick(30, 12)}, want: "reset column widths"},
		{name: "menu move", msgs: keys("j")},
		{name: "menu close", msgs: keys("esc")},
		{name: "reset widths", msgs: keys("W"), want: "column widths reset"},
		{name: "heading sort", msgs: []tea.Msg{leftClick(state+1, boardHeadingY)}, want: "STATE ↓"},
		{name: "heading reverse", msgs: []tea.Msg{leftClick(stateSorted+1, boardHeadingY)}, want: "STATE ↑"},
		{name: "heading without a sort", msgs: []tea.Msg{leftClick(findingsSorted+1, boardHeadingY)}, same: true},
		{name: "mouse off", msgs: keys("m"), want: "mouse off"},
		{name: "wheel while off", msgs: []tea.Msg{wheelDown()}, same: true},
	}
	runSteps(t, m, uncachedBoard, steps)
	if k := m.rowsKey(120); k == (prbRowsKey{}) {
		t.Fatal("empty key")
	}
	a, b := m, m
	b.widths[colFindings] = 20
	if a.rowsKey(120) == b.rowsKey(120) || a.frameKey() == b.frameKey() {
		t.Error("the layout and frame keys must include the widths")
	}
}

// dashRowY is the screen row of the dashboard's selectable row i (the
// screen is tall enough that nothing is cut).
func dashRowY(m dashboardModel, i int) int {
	w := m.viewWidth()
	return len(m.header(w)) + m.body(w).rowAt[i] - m.scroll
}

// dashTableAt is the dashboard's table named name.
func dashTableAt(t *testing.T, m dashboardModel, name string) dashTable {
	t.Helper()
	b := m.body(m.viewWidth())
	i := slices.IndexFunc(b.tables, func(t dashTable) bool { return t.name == name })
	if i < 0 {
		t.Fatalf("no %s table", name)
	}
	return b.tables[i]
}

func TestDashboardMouse(t *testing.T) {
	kept := &fakeWidths{}
	src := &fakeSource{data: dashData()}
	act := &fakeActions{}
	now := dashNow
	m := newDashboardModel(context.Background(), src, act, DashboardOptions{Now: func() time.Time { return now }, Widths: kept})
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 140, Height: 50}, dashDataMsg{data: src.data})

	// rows: review1, review2 (empty), talkable#7, talkable#1 (reviewing)
	m, _ = send(t, m, leftClick(10, dashRowY(m, 2)))
	if r, _ := m.selected(); m.cursor != 2 || r.prRef() != "talkable#7" {
		t.Fatalf("click: cursor %d", m.cursor)
	}
	now = now.Add(time.Second)
	m, cmd := send(t, m, leftClick(10, dashRowY(m, 2)))
	if cmd != nil {
		t.Fatal("two slow clicks acted")
	}
	now = now.Add(100 * time.Millisecond)
	m, cmd = send(t, m, leftClick(10, dashRowY(m, 2)))
	m = run(t, m, cmd)
	if calls := act.list(); len(calls) != 1 || calls[0] != "open talkable#7" {
		t.Fatalf("double click: calls %v", calls)
	}
	for _, y := range []int{0, 3, len(m.header(140))} { // the header, the SLOTS heading
		m, _ = send(t, m, leftClick(10, y))
		if m.cursor != 2 {
			t.Errorf("a click on screen row %d moved the cursor to %d", y, m.cursor)
		}
	}

	// Menus: an empty slot has no PR to act on; a reviewing PR can be killed.
	m, _ = send(t, m, rightClick(10, dashRowY(m, 1)))
	got := menuState(m.menuItems())
	want := map[string]bool{
		"review": false, "fresh review": false, "simplify review": false, "kill review": false, "ignore": false,
		"open pane": false, "browser": false, "pin": true, "unpin": false, "release": true,
		"mute": false, "unmute": false, "reset column widths": false,
	}
	if !m.menu.open || m.cursor != 1 || !maps.Equal(got, want) {
		t.Fatalf("empty slot menu (open %v cursor %d):\n got %v\nwant %v", m.menu.open, m.cursor, got, want)
	}
	mustContain(t, viewOf(m), "open pane", "o / enter", "reset column widths")
	m, _ = send(t, m, keyMsg("esc"), rightClick(10, dashRowY(m, 3)))
	got = menuState(m.menuItems())
	if !got["kill review"] || got["review"] || got["release"] || !got["open pane"] || !got["browser"] {
		t.Fatalf("reviewing PR menu (review and release are dimmed while its round runs) %v", got)
	}
	m, _ = send(t, m, keyMsg("K"))
	if m.confirm == nil || !strings.Contains(m.confirm.question, "Kill the running review of talkable#1") {
		t.Fatalf("K from the menu: %+v", m.confirm)
	}
	m, cmd = send(t, m, keyMsg("y"))
	m = run(t, m, cmd)
	if calls := act.list(); calls[len(calls)-1] != "abort talkable#1" {
		t.Fatalf("calls %v", calls)
	}
	m, cmd = send(t, m, rightClick(10, dashRowY(m, 2)), keyMsg("o"))
	m = run(t, m, cmd)
	if calls := act.list(); calls[len(calls)-1] != "open talkable#7" {
		t.Fatalf("o from the menu: calls %v", calls)
	}
	m, _ = send(t, m, rightClick(10, dashRowY(m, 2)), leftClick(139, 0))
	if m.menu.open {
		t.Fatal("a click outside left the menu open")
	}

	// Drag NEXT's right gap in the queue: live, kept on release, reset by W.
	q := dashTableAt(t, m, dashQueue)
	y := len(m.header(140)) + q.line - m.scroll
	gap := colStart(q.widths, len(noMark), 2) + q.widths[2]
	m, _ = send(t, m, leftClick(gap, y), motion(gap+6, y))
	if got := dashTableAt(t, m, dashQueue).widths[2]; got != q.widths[2]+6 {
		t.Fatalf("NEXT after a drag: %d, want %d", got, q.widths[2]+6)
	}
	m, cmd = send(t, m, release(gap+6, y))
	m = run(t, m, cmd)
	if got := kept.get(widthsDashboard); got["queue.next"] != q.widths[2]+6 || len(got) != 1 {
		t.Fatalf("kept %v", got)
	}
	m, cmd = send(t, m, leftClick(gap+6, y), motion(gap+1000, y), release(gap+1000, y))
	m = run(t, m, cmd)
	if mw := maxLineWidth(viewOf(m)); mw > 140 {
		t.Fatalf("a dragged column overflows: %d cells", mw)
	}
	reloaded := newDashboardModel(context.Background(), src, act, DashboardOptions{Now: func() time.Time { return now }, Widths: kept})
	reloaded, _ = send(t, reloaded, tea.WindowSizeMsg{Width: 140, Height: 50}, dashDataMsg{data: src.data})
	reloaded = run(t, reloaded, reloaded.saver.load(reloaded.ctx))
	if reloaded.widths["queue.next"] != m.widths["queue.next"] {
		t.Fatalf("reloaded widths %v, want %v", reloaded.widths, m.widths)
	}
	m, cmd = send(t, m, keyMsg("W"))
	m = run(t, m, cmd)
	if len(m.widths) != 0 || len(kept.get(widthsDashboard)) != 0 || dashTableAt(t, m, dashQueue).widths[2] != q.widths[2] {
		t.Fatalf("W: widths %v kept %v", m.widths, kept.get(widthsDashboard))
	}
}

// The wheel scrolls the dashboard's body, keeping the cursor on screen,
// and the help; it never acts.
func TestDashboardWheelScrolls(t *testing.T) {
	m := stormDash(t, 60)
	act := m.act.(*fakeActions)
	m, cmd := send(t, m, wheelDown())
	if m.scroll != wheelStep || cmd != nil {
		t.Fatalf("a notch: scroll %d", m.scroll)
	}
	if cur := m.body(120).rowAt[m.cursor]; cur < m.scroll {
		t.Fatalf("the cursor row %d is above the screen (scroll %d)", cur, m.scroll)
	}
	for range 100 {
		m, _ = send(t, m, wheelDown())
	}
	n, _ := m.bodySize(120)
	if cur := m.body(120).rowAt[m.cursor]; m.scroll != n-m.bodyHeight() || cur < m.scroll || cur >= m.scroll+m.bodyHeight() {
		t.Fatalf("at the bottom: scroll %d of %d, cursor on line %d", m.scroll, n-m.bodyHeight(), cur)
	}
	mustContain(t, viewOf(m), "MANUAL WORKTREES")
	for range 100 {
		m, _ = send(t, m, wheelUp())
	}
	if m.scroll != 0 {
		t.Fatalf("back at the top: scroll %d", m.scroll)
	}
	m, _ = send(t, m, keyMsg("?"), wheelDown())
	if m.helpScroll != wheelStep {
		t.Errorf("help scroll %d", m.helpScroll)
	}
	if len(act.list()) != 0 || m.confirm != nil {
		t.Errorf("the wheel acted: %v", act.list())
	}
}

func TestDashboardFrameCacheFollowsTheMouse(t *testing.T) {
	m := stormDash(t, 60)
	q := dashTableAt(t, m, dashQueue)
	y := len(m.header(120)) + q.line - m.scroll
	gap := colStart(q.widths, len(noMark), 1) + q.widths[1]
	steps := []step{
		{name: "press on a gap", msgs: []tea.Msg{leftClick(gap, y)}, same: true},
		{name: "drag", msgs: []tea.Msg{motion(gap+4, y)}},
		{name: "release", msgs: []tea.Msg{release(gap+4, y)}, same: true},
		{name: "menu", msgs: []tea.Msg{rightClick(10, dashRowY(m, 0))}, want: "reset column widths"},
		{name: "menu move", msgs: keys("j")},
		{name: "menu close", msgs: keys("esc")},
		{name: "wheel", msgs: []tea.Msg{wheelDown()}},
		{name: "reset widths", msgs: keys("W"), want: "column widths reset"},
		{name: "mouse off", msgs: keys("m"), want: "mouse off"},
		{name: "wheel while off", msgs: []tea.Msg{wheelDown()}, same: true},
	}
	runSteps(t, m, uncachedDash, steps)
	a, b := m, m
	b.setWidths(map[string]int{"queue.title": 20})
	if a.bodyCacheKey(120) == b.bodyCacheKey(120) || a.frameKey() == b.frameKey() {
		t.Error("the body and frame keys must include the widths")
	}
}

// The menu box replaces the cells under it: a wide rune cut by either
// edge becomes a space, styles around it survive, and no line grows.
func TestOverlayKeepsLineWidths(t *testing.T) {
	for _, c := range []struct {
		line     string
		x, width int
		want     string
	}{
		{"ab📌cd", 3, 6, "ab XYd"},
		{"ab📌cd", 1, 6, "aXY cd"},
		{"ab📌cd", 2, 6, "abXYcd"},
		{"abc", 5, 7, "abc  XY"},
		{"abcdef", 5, 6, "abcdeX"}, // cut at the screen's edge
	} {
		lines := []string{c.line}
		overlay(lines, []string{"XY"}, c.x, 0, c.width)
		if got := ansi.Strip(lines[0]); got != c.want {
			t.Errorf("%q at %d: %q, want %q", c.line, c.x, got, c.want)
		}
	}
	styled := []string{defaultStyles.Err.Render("abcdef")}
	overlay(styled, []string{"X"}, 2, 0, 6)
	if ansi.Strip(styled[0]) != "abXdef" || !strings.Contains(styled[0], "\x1b[") {
		t.Errorf("styled line %q", styled[0])
	}
}
