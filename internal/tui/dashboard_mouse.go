package tui

// The status dashboard's mouse support (see mouse.go): the wheel scrolls
// the body or the help; a click selects a slot or a queued PR and a
// double click opens its pane as enter does; dragging a heading gap
// resizes the column on its left; a right click opens the row's menu.
// The dashboard has no sorts, so a click on a heading does nothing.

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// The dashboard's tables, as their columns' width names start.
const (
	dashSlots  = "slots"
	dashQueue  = "queue"
	dashManual = "manual"
)

// dashColKey names a column for ColumnWidths: "queue.title".
func dashColKey(table string, c column) string {
	return table + "." + strings.ToLower(strings.ReplaceAll(c.title, " ", "_"))
}

// dashKnownCol reports whether name is one of the dashboard's columns.
func dashKnownCol(name string) bool {
	for table, cols := range map[string][]column{dashSlots: slotCols, dashQueue: queueCols, dashManual: manualCols} {
		for _, c := range cols {
			if dashColKey(table, c) == name {
				return true
			}
		}
	}
	return false
}

// fixedWidths are the dragged widths of table's columns (0: automatic).
func (m dashboardModel) fixedWidths(table string, cols []column) []int {
	if len(m.widths) == 0 {
		return nil
	}
	out := make([]int, len(cols))
	for i, c := range cols {
		out[i] = m.widths[dashColKey(table, c)]
	}
	return out
}

// setWidths replaces the dragged widths.
func (m *dashboardModel) setWidths(w map[string]int) {
	m.widths = w
	keys := slices.Sorted(maps.Keys(w))
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + strconv.Itoa(w[k]) + ";")
	}
	m.widthsKey = b.String()
}

// bodyLineAt is the body line drawn at screen row y, -1 when y is not on
// the body (or the help shows). It follows render: the header, the body
// from scroll, the status and hint lines, the top cut on a short screen.
func (m dashboardModel) bodyLineAt(y int) int {
	w, h := m.viewWidth(), m.viewHeight()
	if !m.haveData || m.showHelp || m.showLog || h <= 1 {
		return -1
	}
	hdr := len(m.header(w))
	n, _ := m.bodySize(w)
	start := min(m.scroll, max(n-1, 0))
	end := min(start+m.bodyHeight(), n)
	cut := max(hdr+end-start+dashFooter-h, 0)
	l := y + cut - hdr
	if l < 0 || l >= end-start {
		return -1
	}
	return start + l
}

func (m dashboardModel) updateMouse(msg tea.MouseMsg) (dashboardModel, tea.Cmd) {
	if !m.mouseOn || m.leaving {
		return m, nil
	}
	ev := msg.Mouse()
	switch msg.(type) {
	case tea.MouseMotionMsg:
		m.dragTo(ev.X)
	case tea.MouseReleaseMsg:
		return m.endDrag()
	case tea.MouseWheelMsg:
		if m.confirm == nil && !m.menu.open {
			m.wheel(ev.Button)
		}
	case tea.MouseClickMsg:
		return m.click(ev)
	}
	return m, nil
}

// wheel scrolls the body (the cursor keeps to the rows on screen, or
// stays on the last row while the sections under it are read) or the
// help by wheelStep lines. It never acts.
func (m *dashboardModel) wheel(b tea.MouseButton) {
	n := 0
	switch b {
	case tea.MouseWheelDown:
		n = wheelStep
	case tea.MouseWheelUp:
		n = -wheelStep
	default:
		return
	}
	if m.showHelp {
		m.helpScroll = max(m.helpScroll+n, 0) // fixScroll keeps it within the help
		return
	}
	if m.showLog {
		m.logScroll = max(m.logScroll+n, 0)
		return
	}
	if !m.haveData {
		return
	}
	body := m.body(m.viewWidth())
	avail := m.bodyHeight()
	m.scroll = min(max(m.scroll+n, 0), max(len(body.lines)-avail, 0))
	if len(body.rowAt) == 0 || m.cursor >= len(body.rowAt) {
		return
	}
	switch cur := body.rowAt[m.cursor]; {
	case cur < m.scroll:
		i := slices.IndexFunc(body.rowAt, func(l int) bool { return l >= m.scroll })
		if i < 0 {
			i = len(body.rowAt) - 1 // past the last row: fixScroll lets it stay above
		}
		m.moveTo(i)
	case cur >= m.scroll+avail:
		i := 0
		for j, l := range body.rowAt {
			if l < m.scroll+avail {
				i = j
			}
		}
		m.moveTo(i)
	}
}

// click handles a button going down: it answers a pending question with
// no (as any key but y does), works the menu, or acts on the body.
func (m dashboardModel) click(ev tea.Mouse) (dashboardModel, tea.Cmd) {
	switch {
	case m.confirm != nil:
		cmd, _ := m.answer("click")
		return m, cmd
	case m.menu.open:
		return m.menuClick(ev)
	}
	l := m.bodyLineAt(ev.Y)
	if l < 0 {
		return m, nil
	}
	w := m.viewWidth()
	body := m.body(w)
	if t := slices.IndexFunc(body.tables, func(t dashTable) bool { return t.line == l }); t >= 0 {
		if ev.Button == tea.MouseLeft {
			m.startDrag(body.tables[t], ev.X, w)
		}
		return m, nil
	}
	i := slices.Index(body.rowAt, l)
	if i < 0 {
		return m, nil
	}
	switch ev.Button {
	case tea.MouseLeft:
		m.moveTo(i)
		if m.clickTwice(m.selKey, m.opts.Now()) {
			return m.listKey("enter")
		}
	case tea.MouseRight:
		m.moveTo(i)
		m.menu = ctxMenu{open: true, x: ev.X, y: ev.Y, rowKey: m.selKey}
	}
	return m, nil
}

// startDrag starts resizing t's column left of the heading gap under x.
func (m *dashboardModel) startDrag(t dashTable, x, w int) {
	start := len(noMark)
	i, gap := colAtWidths(t.widths, start, x)
	if i < 0 || !gap {
		return
	}
	m.drag = colDrag{
		active: true, table: t.name, col: i, startX: x, startW: t.widths[i],
		maxW: w - colStart(t.widths, start, i),
	}
}

// dragTo resizes the dragged column for the mouse at x, live.
func (m *dashboardModel) dragTo(x int) {
	d := m.drag
	if !d.active || (x == d.startX && !d.moved) {
		return
	}
	cols := map[string][]column{dashSlots: slotCols, dashQueue: queueCols, dashManual: manualCols}[d.table]
	if d.col >= len(cols) {
		return
	}
	w := maps.Clone(m.widths)
	if w == nil {
		w = map[string]int{}
	}
	w[dashColKey(d.table, cols[d.col])] = d.width(x)
	m.setWidths(w)
	m.drag.moved, m.widthsSet = true, true
}

// endDrag ends a resize and keeps the new widths.
func (m dashboardModel) endDrag() (dashboardModel, tea.Cmd) {
	d := m.drag
	m.drag = colDrag{}
	if !d.active || !d.moved {
		return m, nil
	}
	return m, m.saver.save(m.ctx, m.widths)
}

// resetWidths goes back to the automatic widths and forgets the kept ones.
func (m dashboardModel) resetWidths() (dashboardModel, tea.Cmd) {
	m.setWidths(nil)
	m.widthsSet, m.drag = true, colDrag{}
	save := m.saver.save(m.ctx, nil)
	note := m.note("column widths reset")
	return m, tea.Batch(save, note)
}

// widthsLoaded applies the kept widths, unless the user already changed
// them this run.
func (m dashboardModel) widthsLoaded(msg widthsLoadedMsg) (dashboardModel, tea.Cmd) {
	if msg.err != nil {
		return m.fail("could not read the kept column widths: " + errLine(msg.err))
	}
	if !m.widthsSet {
		m.setWidths(cleanWidths(msg.widths, dashKnownCol))
	}
	return m, nil
}

// rowState is magnum's state of the row's PR ("" when unknown or no PR):
// a slot row reads its queue row when it has one.
func (m dashboardModel) rowState(r dashRow) string {
	var s string
	switch {
	case r.pr != nil:
		s = r.pr.State
	case r.prRef() == "":
	default:
		s = r.slot.PRState
		if q, ok := m.queueRow(r.prRef()); ok {
			s = q.State
		}
	}
	s, _, _ = strings.Cut(s, ",")
	return normState(s)
}

// menuItems are the selected row's actions as the menu lists them (the row
// actions' table, then reset widths), the ones that cannot run on the row
// dimmed by the same predicate the keys check (actionRefusal). The dashboard
// does not know whether a PR is muted, nor pinned unless its slot is: the
// daemon decides those.
func (m dashboardModel) menuItems() []menuItem {
	return append(actionMenu(dashActs, m.actRow(), m.act != nil, map[rowAct]string{actOpen: "o / enter"}),
		menuItem{"reset column widths", "W", "W", len(m.widths) > 0})
}

// menuKey handles a key while the menu is open: move, run the highlighted
// item (enter) or the item whose key it is, or close (esc, q).
func (m dashboardModel) menuKey(k string) (dashboardModel, tea.Cmd) {
	items := m.menuItems()
	switch k {
	case "esc", "q":
		m.menu = ctxMenu{}
		return m, nil
	case "enter", "space":
		return m.runMenuItem(items, m.menu.sel)
	}
	if sel, ok := menuNav(k, m.menu.sel, len(items)); ok {
		m.menu.sel = sel
		return m, nil
	}
	if i := menuKeyItem(items, k); i >= 0 {
		return m.runMenuItem(items, i)
	}
	return m, nil
}

// menuClick runs the item clicked; a click outside the menu, or any
// right click, closes it.
func (m dashboardModel) menuClick(ev tea.Mouse) (dashboardModel, tea.Cmd) {
	items := m.menuItems()
	l := placeMenu(items, m.menu.sel, m.menu.x, m.menu.y, m.viewWidth(), m.viewHeight())
	switch i := l.item(ev.X, ev.Y); {
	case ev.Button != tea.MouseLeft || i == -2:
		m.menu = ctxMenu{}
	case i >= 0:
		m.menu.sel = i
		return m.runMenuItem(items, i)
	}
	return m, nil
}

// runMenuItem closes the menu and presses item i's key on the list, the
// way the keyboard would (y/N question included). A dimmed item does
// nothing and leaves the menu open.
func (m dashboardModel) runMenuItem(items []menuItem, i int) (dashboardModel, tea.Cmd) {
	if i < 0 || i >= len(items) || !items[i].ok {
		return m, nil
	}
	row := m.menu.rowKey
	m.menu = ctxMenu{}
	if m.selKey != row {
		return m.fail("that row is no longer listed")
	}
	return m.listKey(items[i].key)
}

// drawMenu draws the open menu over the frame's lines, padded to the
// screen's height first: a short body leaves the frame shorter.
func (m dashboardModel) drawMenu(out []string, w, h int) []string {
	for len(out) < h {
		out = append(out, "")
	}
	items := m.menuItems()
	l := placeMenu(items, m.menu.sel, m.menu.x, m.menu.y, w, h)
	overlay(out, m.st.menuBox(items, m.menu.sel, l, m.g.mode == IconsASCII), l.x, l.y, w)
	return out
}
