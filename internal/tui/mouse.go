package tui

// Mouse support shared by the PR board and the status dashboard. Bubble
// Tea's cell-motion mode reports clicks, releases, the wheel and motion
// while a button is held. The wheel scrolls; a click selects a row and a
// double click opens it as enter does; a click on a column heading sorts
// by it (the board); dragging the gap between two headings resizes the
// column on its left; a right click opens a menu of the row's actions.
// Every action still runs through its key's path, y/N question included.
// m turns it all off and on: while it is on the terminal's own text
// selection needs a modifier (Option-drag in iTerm2, Shift-drag in most
// others).

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

const (
	wheelStep   = 3                      // rows (or lines) one wheel notch scrolls
	doubleClick = 400 * time.Millisecond // the most between the two clicks of a double click
	minColWidth = 4                      // the narrowest a dragged column gets
	sepGrab     = 1                      // cells either side of a heading gap that still grab it
	// widthsTimeout bounds loading or saving the column widths.
	widthsTimeout = 5 * time.Second
)

// The screens' names for ColumnWidths.
const (
	widthsBoard     = "board"
	widthsDashboard = "dashboard"
)

// ColumnWidths keeps the column widths dragged with the mouse across runs,
// per screen ("board", "dashboard") and column name. LoadWidths returns
// nil when nothing is kept; SaveWidths with an empty map forgets them.
// The screens call both from commands, so a slow store never blocks a key.
type ColumnWidths interface {
	LoadWidths(ctx context.Context, screen string) (map[string]int, error)
	SaveWidths(ctx context.Context, screen string, widths map[string]int) error
}

type (
	// widthsLoadedMsg carries the widths kept for a screen.
	widthsLoadedMsg struct {
		widths map[string]int
		err    error
	}
	// widthsSavedMsg reports a failed save.
	widthsSavedMsg struct{ err error }
)

// widthSaver loads and saves one screen's widths. Saves run as commands,
// concurrently: each takes a number when it is made, and a save older than
// the last one written is skipped, so a quick reset after a drag is never
// undone. Only the commands take mu, which they hold across the store
// write: numbering a save never waits for a slow write, so the event loop
// does not either.
type widthSaver struct {
	store  ColumnWidths
	screen string

	seq   atomic.Int64
	mu    sync.Mutex // serializes the writes and guards saved
	saved int64
}

func newWidthSaver(store ColumnWidths, screen string) *widthSaver {
	if store == nil {
		return nil
	}
	return &widthSaver{store: store, screen: screen}
}

// load reads the kept widths; nil without a store.
func (s *widthSaver) load(ctx context.Context) tea.Cmd {
	if s == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, widthsTimeout)
		defer cancel()
		w, err := s.store.LoadWidths(ctx, s.screen)
		return widthsLoadedMsg{widths: w, err: err}
	}
}

// save keeps widths (a copy); nil without a store.
func (s *widthSaver) save(ctx context.Context, widths map[string]int) tea.Cmd {
	if s == nil {
		return nil
	}
	seq := s.seq.Add(1)
	widths = maps.Clone(widths)
	if widths == nil {
		widths = map[string]int{}
	}
	return func() tea.Msg {
		s.mu.Lock()
		defer s.mu.Unlock()
		if seq < s.saved {
			return nil
		}
		s.saved = seq
		ctx, cancel := context.WithTimeout(ctx, widthsTimeout)
		defer cancel()
		if err := s.store.SaveWidths(ctx, s.screen, widths); err != nil {
			return widthsSavedMsg{err: err}
		}
		return nil
	}
}

// mouseState is the mouse support a screen embeds. Every field is a value:
// models are copied on every Update.
type mouseState struct {
	mouseOn  bool
	clickAt  time.Time // the last left click on a row, for double clicks
	clickRow string    // that row's key
	drag     colDrag
	menu     ctxMenu
	// widthsSet is true once the user dragged or reset a width this run,
	// so widths loaded late do not override it.
	widthsSet bool
}

// colDrag is a column being resized: the mouse went down on the heading
// gap right of it and has not come up yet.
type colDrag struct {
	active bool
	table  string // the dashboard's table; "" on the board
	col    int    // the board's prbCol, or the column's index in the dashboard's table
	startX int    // where the drag started
	startW int    // the column's width then
	maxW   int    // the widest it may get
	moved  bool   // the width changed: save on release
}

// width is the column's width for the mouse at x, within bounds.
func (d colDrag) width(x int) int {
	return min(max(d.startW+x-d.startX, minColWidth), max(d.maxW, minColWidth))
}

// ctxMenu is the right-click menu: open over the row named rowKey,
// anchored at the click (x, y), sel the highlighted item. Its items are
// built from the row each time, so they follow refreshes.
type ctxMenu struct {
	open   bool
	x, y   int
	sel    int
	rowKey string
}

// menuItem is one entry of the menu: what it does, the key hint shown,
// and the key it presses. A dimmed item (ok false) does nothing.
type menuItem struct {
	label, hint, key string
	ok               bool
}

// clickTwice records a left click on row and reports whether it completes
// a double click: the previous click hit the same row less than
// doubleClick ago. A completed double click starts afresh, so a third
// click does not count twice.
func (ms *mouseState) clickTwice(row string, now time.Time) bool {
	double := row != "" && row == ms.clickRow && !ms.clickAt.IsZero() &&
		now.Sub(ms.clickAt) >= 0 && now.Sub(ms.clickAt) < doubleClick
	if double {
		ms.clickAt, ms.clickRow = time.Time{}, ""
		return true
	}
	ms.clickAt, ms.clickRow = now, row
	return false
}

// toggleMouse flips mouse support and drops a drag or menu in progress.
func (ms *mouseState) toggleMouse() {
	ms.mouseOn = !ms.mouseOn
	ms.drag, ms.menu = colDrag{}, ctxMenu{}
	ms.clickAt, ms.clickRow = time.Time{}, ""
}

// mouseNote is the footer news after m.
func mouseNote(on bool) string {
	if on {
		return "mouse on: Option-drag (iTerm2) or Shift-drag selects text · m turns it off"
	}
	return "mouse off: the terminal selects text again · m turns it back on"
}

// mouseHelp is the help's lines about the mouse; sorts says whether a
// heading click sorts (the board).
func mouseHelp(sorts bool, open string) []hint {
	hs := []hint{
		{"wheel", "scroll"},
		{"click", "select a row; double-click " + open},
	}
	if sorts {
		hs = append(hs, hint{"click heading", "sort by it; again reverses"})
	}
	return append(hs,
		hint{"drag heading gap", "resize the column on its left (W resets)"},
		hint{"right-click", "the row's actions"},
		hint{"m", "mouse on / off"},
	)
}

// mouseSelectNote explains text selection while the mouse is on.
const mouseSelectNote = "While the mouse is on, the terminal selects text with Option-drag (iTerm2) or Shift-drag (most others); m turns the mouse off."

// withMouse sets the view's mouse mode when mouse support is on.
func withMouse(v tea.View, on bool) tea.View {
	if on {
		v.MouseMode = tea.MouseModeCellMotion
	}
	return v
}

// menuBoxLayout is where the menu sits on a screen of w x h cells.
type menuBoxLayout struct {
	x, y, w, h int // the box, borders included
	off, room  int // the first item shown and how many fit
	labelW     int
	hintW      int
}

// placeMenu puts a box for items at the click (x, y), moved left and up
// to stay on a w x h screen; a screen too short scrolls the items so sel
// shows.
func placeMenu(items []menuItem, sel, x, y, w, h int) menuBoxLayout {
	var l menuBoxLayout
	for _, it := range items {
		l.labelW = max(l.labelW, ansi.StringWidth(it.label))
		l.hintW = max(l.hintW, ansi.StringWidth(it.hint))
	}
	l.w = l.labelW + 2 + l.hintW + 4 // "│ " label "  " hint " │"
	l.room = min(len(items), max(h-2, 1))
	l.h = l.room + 2
	if sel >= l.room {
		l.off = sel - l.room + 1
	}
	l.x = max(min(x, w-l.w), 0)
	l.y = max(min(y, h-l.h), 0)
	return l
}

// item is the index of the item at screen cell (x, y): -1 on the border,
// -2 outside the box.
func (l menuBoxLayout) item(x, y int) int {
	if x < l.x || x >= l.x+l.w || y < l.y || y >= l.y+l.h {
		return -2
	}
	if y == l.y || y == l.y+l.h-1 || x == l.x || x == l.x+l.w-1 {
		return -1
	}
	return l.off + y - l.y - 1
}

// menuBox draws the menu's lines for l.
func (st styles) menuBox(items []menuItem, sel int, l menuBoxLayout, ascii bool) []string {
	h, v, tl, tr, bl, br := "─", "│", "╭", "╮", "╰", "╯"
	if ascii {
		h, v, tl, tr, bl, br = "-", "|", "+", "+", "+", "+"
	}
	edge := st.Dim
	inner := l.w - 2
	out := []string{edge.Render(tl + strings.Repeat(h, inner) + tr)}
	for i := l.off; i < l.off+l.room && i < len(items); i++ {
		it := items[i]
		label := it.label + spaces(l.labelW-ansi.StringWidth(it.label))
		key := spaces(l.hintW-ansi.StringWidth(it.hint)) + it.hint
		var body string
		switch {
		case i == sel:
			body = st.Selected.Render(" " + label + "  " + key + " ")
		case !it.ok:
			body = st.Dim.Render(" " + label + "  " + key + " ")
		default:
			body = " " + label + "  " + st.Key.Render(key) + " "
		}
		out = append(out, edge.Render(v)+body+edge.Render(v))
	}
	return append(out, edge.Render(bl+strings.Repeat(h, inner)+br))
}

// overlay draws box over lines with its top left corner at (x, y), cut to
// width cells; lines are changed in place (pass a copy of shared lines).
func overlay(lines, box []string, x, y, width int) {
	for i, b := range box {
		row := y + i
		if row < 0 || row >= len(lines) || x >= width {
			continue
		}
		if room := width - x; ansi.StringWidth(b) > room {
			b = ansi.Truncate(b, room, "")
		}
		bw := ansi.StringWidth(b)
		line := lines[row]
		lw := ansi.StringWidth(line)
		left := ansi.Truncate(line, x, "")
		left += spaces(x - ansi.StringWidth(left)) // a wide rune cut at x, or a short line
		right := ""
		if rest := lw - x - bw; rest > 0 {
			right = ansi.TruncateLeft(line, x+bw, "")
			for cut := x + bw + 1; ansi.StringWidth(right) > rest; cut++ { // a wide rune under the box's right edge
				right = ansi.TruncateLeft(line, cut, "")
			}
			right = spaces(rest-ansi.StringWidth(right)) + right
		}
		lines[row] = left + "\x1b[m" + b + "\x1b[m" + right
	}
}

// menuNav moves the menu's highlight for a navigation key; ok is false
// for other keys.
func menuNav(k string, sel, n int) (int, bool) {
	switch k {
	case "j", "down", "ctrl+n", "tab":
		return (sel + 1) % max(n, 1), true
	case "k", "up", "ctrl+p", "shift+tab":
		return (sel - 1 + n) % max(n, 1), true
	case "g", "home":
		return 0, true
	case "G", "end":
		return max(n-1, 0), true
	}
	return sel, false
}

// menuKeyItem is the index of the enabled item a key presses directly
// (its hint key), or -1.
func menuKeyItem(items []menuItem, k string) int {
	return slices.IndexFunc(items, func(it menuItem) bool { return it.ok && it.key == k })
}

// cleanWidths drops unknown names and keeps the known ones at least
// minColWidth wide.
func cleanWidths(w map[string]int, known func(string) bool) map[string]int {
	out := map[string]int{}
	for k, v := range w {
		if v > 0 && known(k) {
			out[k] = max(v, minColWidth)
		}
	}
	return out
}
