package tui

// The PR board's mouse support (see mouse.go): the wheel scrolls the
// table, the card or the help; a click selects a row and a double click
// opens its card; a click on a heading sorts by that column and a second
// one reverses it; dragging a heading gap resizes the column on its left;
// a right click opens the row's menu.

import (
	tea "charm.land/bubbletea/v2"
)

// colSort is the sort a click on c's heading picks: the one whose arrow
// that heading shows. False for columns without a sort.
func colSort(c prbCol) (PRSort, bool) {
	for _, s := range prSortOrder {
		if sortColumn(s) == c {
			return s, true
		}
	}
	return "", false
}

// tableLayout is the table's column layout at width w, dragged widths
// applied, from the cache.
func (m prBoardModel) tableLayout(p *prbPainter, w int) prbLayout {
	rk := m.rowsKey(w)
	return m.cache.layoutFor(rk, func() prbLayout {
		nk := rk
		nk.widths = prbWidths{}
		n := m.cache.naturalFor(nk, func() prbNatural { return p.natural(m.all) })
		return p.fit(n, w, m.widths)
	})
}

// bodyLine is the body line at screen row y (0 is the table's heading),
// -1 off the body. A screen too short for the frame loses its top lines,
// as render cuts them.
func (m prBoardModel) bodyLine(y int) int {
	cut := max(prbChrome+m.bodyHeight()-m.viewHeight(), 0)
	l := y + cut - 2 // the title bar and the summary line
	if l < 0 || l >= m.bodyHeight() {
		return -1
	}
	return l
}

// rowAt is the index in view of the row drawn at screen row y, -1 when
// no row is drawn there.
func (m prBoardModel) rowAt(y int) int {
	l := m.bodyLine(y)
	if m.mode != prbTable || !m.haveData || l < prbTableChrome {
		return -1
	}
	start := min(m.scroll, max(len(m.view)-1, 0))
	end, heading := m.visible(start)
	off := l - prbTableChrome
	if at := m.section - start; heading && off >= at {
		if off == at {
			return -1 // the recently closed section's heading
		}
		off--
	}
	if i := start + off; i < end {
		return i
	}
	return -1
}

func (m prBoardModel) updateMouse(msg tea.MouseMsg) (prBoardModel, tea.Cmd) {
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

// wheel scrolls the table by wheelStep rows (the cursor keeps to the rows
// on screen), or the card or the help by as many lines. It never acts.
func (m *prBoardModel) wheel(b tea.MouseButton) {
	n := 0
	switch b {
	case tea.MouseWheelDown:
		n = wheelStep
	case tea.MouseWheelUp:
		n = -wheelStep
	default:
		return
	}
	switch m.mode {
	case prbHelp:
		m.helpScroll = max(m.helpScroll+n, 0) // fixScroll keeps it within the help
	case prbLog:
		m.logScroll = max(m.logScroll+n, 0)
	case prbDetail:
		m.detailScroll = max(m.detailScroll+n, 0)
	default:
		m.scroll = min(max(m.scroll+n, 0), m.maxScroll())
		end, _ := m.visible(m.scroll)
		switch {
		case m.cursor < m.scroll:
			m.moveTo(m.scroll)
		case m.cursor >= end:
			m.moveTo(end - 1)
		}
	}
}

// click handles a button going down: it answers a pending question with
// no (as any key but y does), works the menu, or acts on the table.
func (m prBoardModel) click(ev tea.Mouse) (prBoardModel, tea.Cmd) {
	switch {
	case m.confirm != nil:
		cmd, _ := m.answer("click")
		return m, cmd
	case m.menu.open:
		return m.menuClick(ev)
	case m.mode != prbTable:
		return m, nil
	}
	switch ev.Button {
	case tea.MouseLeft:
		if m.haveData && m.bodyLine(ev.Y) == 0 {
			return m.headingClick(ev.X)
		}
		i := m.rowAt(ev.Y)
		if i < 0 {
			return m, nil
		}
		m.moveTo(i)
		if m.clickTwice(m.selKey, m.opts.Now()) {
			m.stopFiltering()
			return m.tableKey("enter")
		}
	case tea.MouseRight:
		i := m.rowAt(ev.Y)
		if i < 0 {
			return m, nil
		}
		m.moveTo(i)
		m.stopFiltering()
		m.menu = ctxMenu{open: true, x: ev.X, y: ev.Y, rowKey: m.selKey}
	}
	return m, nil
}

// headingClick sorts by the column under x (again: reverses it), or
// starts resizing the column left of the gap under x.
func (m prBoardModel) headingClick(x int) (prBoardModel, tea.Cmd) {
	p := m.painter()
	lay := m.tableLayout(&p, m.viewWidth())
	i, gap := lay.colAt(x)
	switch {
	case i < 0:
	case gap:
		m.drag = colDrag{active: true, col: int(lay.cols[i]), startX: x, startW: lay.widths[i], maxW: lay.maxW[i]}
	default:
		if s, ok := colSort(lay.cols[i]); ok {
			if s == m.sort {
				m.desc = !m.desc
			} else {
				m.sort, m.desc = s, true
			}
			m.rebuild()
		}
	}
	return m, nil
}

// dragTo resizes the dragged column for the mouse at x, live.
func (m *prBoardModel) dragTo(x int) {
	d := m.drag
	if !d.active || (x == d.startX && !d.moved) {
		return
	}
	m.widths[d.col] = d.width(x)
	m.drag.moved, m.widthsSet = true, true
}

// endDrag ends a resize and keeps the new widths.
func (m prBoardModel) endDrag() (prBoardModel, tea.Cmd) {
	d := m.drag
	m.drag = colDrag{}
	if !d.active || !d.moved {
		return m, nil
	}
	return m, m.saver.save(m.ctx, m.widths.named())
}

// resetWidths goes back to the automatic widths and forgets the kept ones.
func (m prBoardModel) resetWidths() (prBoardModel, tea.Cmd) {
	m.widths, m.widthsSet, m.drag = prbWidths{}, true, colDrag{}
	save := m.saver.save(m.ctx, nil)
	note := m.note("column widths reset")
	return m, tea.Batch(save, note)
}

// widthsLoaded applies the kept widths, unless the user already changed
// them this run.
func (m prBoardModel) widthsLoaded(msg widthsLoadedMsg) (prBoardModel, tea.Cmd) {
	if msg.err != nil {
		return m.fail("could not read the kept column widths: " + errLine(msg.err))
	}
	if !m.widthsSet {
		m.widths = prbWidthsFrom(msg.widths)
	}
	return m, nil
}

// stopFiltering hands the keyboard back from the filter, keeping its text.
func (m *prBoardModel) stopFiltering() {
	if m.filtering {
		m.filtering = false
		m.filter.Blur()
	}
}

// menuItems are the cursor row's actions as the menu lists them (the row
// actions' table, then the board's own), the ones that cannot run on the row
// dimmed by the same predicate the keys check (actionRefusal).
func (m prBoardModel) menuItems() []menuItem {
	_, ok := m.selected()
	return append(actionMenu(boardActs, m.actRow(), m.act != nil, nil),
		menuItem{"details", "enter", "enter", ok},
		menuItem{"reset column widths", "W", "W", m.widths != prbWidths{}})
}

// menuKey handles a key while the menu is open: move, run the highlighted
// item (enter) or the item whose key it is, or close (esc, q).
func (m prBoardModel) menuKey(k string) (prBoardModel, tea.Cmd) {
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
func (m prBoardModel) menuClick(ev tea.Mouse) (prBoardModel, tea.Cmd) {
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

// runMenuItem closes the menu and presses item i's key on the table, the
// way the keyboard would (y/N question included). A dimmed item does
// nothing and leaves the menu open.
func (m prBoardModel) runMenuItem(items []menuItem, i int) (prBoardModel, tea.Cmd) {
	if i < 0 || i >= len(items) || !items[i].ok {
		return m, nil
	}
	row := m.menu.rowKey
	m.menu = ctxMenu{}
	if m.selKey != row {
		return m.fail("that PR is no longer listed")
	}
	return m.tableKey(items[i].key)
}

// drawMenu draws the open menu over the frame's lines.
func (m prBoardModel) drawMenu(out []string, w, h int) {
	items := m.menuItems()
	l := placeMenu(items, m.menu.sel, m.menu.x, m.menu.y, w, h)
	overlay(out, m.st.menuBox(items, m.menu.sel, l, m.g.mode == IconsASCII), l.x, l.y, w)
}
