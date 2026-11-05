package tui

// The factory screen's mouse (spec 9.D, mouse.go): the wheel moves home's
// cursor, the task card's mark and the help's and the log's lines; a click
// selects a row on home and a double click is enter; a right click opens a
// menu built from the view's own keys, dimmed by their refusals, and an
// item runs its key's path, y/N question included. A click while a question
// waits answers it with no, as any key but y does, and starts nothing.

import (
	"slices"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
)

func (m *factoryModel) updateMouse(msg tea.MouseMsg) tea.Cmd {
	if !m.mouseOn {
		return nil
	}
	ev := msg.Mouse()
	switch msg.(type) {
	case tea.MouseWheelMsg:
		if m.bar.asking == nil && !m.menu.open {
			m.wheel(ev.Button)
		}
	case tea.MouseClickMsg:
		return m.click(ev)
	}
	return nil
}

// wheel moves by wheelStep rows (or lines); it never acts.
func (m *factoryModel) wheel(b tea.MouseButton) {
	n := 0
	switch b {
	case tea.MouseWheelDown:
		n = wheelStep
	case tea.MouseWheelUp:
		n = -wheelStep
	default:
		return
	}
	switch m.view {
	case fvHome:
		m.moveTo(m.selIdx + n)
	case fvTask:
		c := m.taskCard()
		m.cursors[m.task] = min(max(c.Cursor+n, 0), max(taskStops(c)-1, 0))
	case fvHelp, fvLog:
		m.scroll = max(m.scroll+n, 0)
		m.clampScroll()
	}
}

// click is a button going down: it answers a question with no, works the
// menu, selects or opens a row, or opens the menu.
func (m *factoryModel) click(ev tea.Mouse) tea.Cmd {
	switch {
	case m.bar.asking != nil:
		cmd, _ := m.bar.cancel()
		m.asked = nil
		return cmd
	case m.menu.open:
		return m.menuClick(ev)
	case m.view == fvNew || (m.view == fvQuestion && m.q.typing) || (m.view == fvReady && m.readyState().noting):
		return nil // the keyboard types here
	}
	row, onRow := m.rowAt(ev.Y)
	switch ev.Button {
	case tea.MouseLeft:
		if !onRow {
			return nil
		}
		m.moveTo(slices.Index(m.order(), row))
		if m.clickTwice(m.taskKey(row), m.now()) {
			cmd, _ := m.homeKey("enter")
			return cmd
		}
	case tea.MouseRight:
		if onRow {
			m.moveTo(slices.Index(m.order(), row))
		}
		if len(m.menuItems()) > 0 {
			m.menu = ctxMenu{open: true, x: ev.X, y: ev.Y, rowKey: m.menuOwner()}
		}
	}
	return nil
}

// rowAt is the task drawn on home at screen line y.
func (m factoryModel) rowAt(y int) (int64, bool) {
	if m.view != fvHome {
		return 0, false
	}
	_, lines := renderFactory(m.homeFrame(), m.viewWidth(), m.viewHeight(), m.st, m.g, m.now())
	for id, l := range lines {
		if l == y {
			return id, true
		}
	}
	return 0, false
}

// menuOwner names what the menu is for: home's row, or the card's task. An
// item runs only while it still is.
func (m factoryModel) menuOwner() string {
	if m.view == fvHome {
		if r, ok := m.selectedRow(); ok {
			return m.taskKey(r.ID)
		}
		return ""
	}
	return m.taskKey(m.task)
}

// menuItems are the view's actions as the menu lists them: the same keys,
// dimmed when the key would be refused. A question's y is never an item: a
// click never confirms.
func (m factoryModel) menuItems() []menuItem {
	acts := m.act != nil
	switch m.view {
	case fvHome:
		sel, row := m.selectedRow()
		_, decision := m.nextDecision()
		return []menuItem{
			{"open its card", "enter", "enter", row}, {"oldest decision", "a", "a", decision},
			{"step into its pane", "o", "o", row && acts && m.paneHint(sel) != ""}, {"new task", "n", "n", acts},
			{"refresh", "ctrl+r", "ctrl+r", true}, {"action log", logKey, logKey, true}, {"help", "?", "?", true},
		}
	case fvQuestion:
		c, ok := m.questionCard()
		if !ok || len(c.Questions) == 0 {
			return []menuItem{{"back", "esc", "esc", true}}
		}
		q := c.Questions[c.Current]
		return []menuItem{
			{"take the marked one", "enter", "enter", c.Cursor >= 0 && c.Cursor < len(q.Options)},
			{"flag it needless", "−", "-", c.Needless}, {"previous question", "backspace", "backspace", c.Current > 0},
			{"step into its pane", "o", "o", acts}, {"back", "esc", "esc", true},
		}
	case fvRecap: // a click there answers its standing question with no: no menu
	case fvReady:
		c, _ := m.readyCard()
		return []menuItem{
			{"read the diff", "d", "d", acts}, {"push + PR", "1", "1", acts && c.NoPush == ""},
			{"send back", "2", "2", acts}, {"drop", "3", "3", acts}, {"step into its pane", "o", "o", acts}, {"back", "esc", "esc", true},
		}
	case fvTask:
		c := m.taskCard()
		return []menuItem{
			{"retry", "r", "r", acts && c.Retry}, {"diff", "d", "d", acts}, {"drop", "x", "x", acts},
			{"step into its pane", "o", "o", acts}, {"back", "esc", "esc", true},
		}
	}
	return nil
}

// menuKey is a key while the menu is open: move, run the highlighted item
// (enter) or the item whose key it is, or close (esc, q).
func (m *factoryModel) menuKey(k string) tea.Cmd {
	items := m.menuItems()
	switch k {
	case "esc", "q":
		m.menu = ctxMenu{}
		return nil
	case "enter", "space":
		return m.runMenuItem(items, m.menu.sel)
	}
	if sel, ok := menuNav(k, m.menu.sel, len(items)); ok {
		m.menu.sel = sel
		return nil
	}
	if i := menuKeyItem(items, k); i >= 0 {
		return m.runMenuItem(items, i)
	}
	return nil
}

// menuClick runs the item clicked; a click outside the menu, or a right
// click, closes it.
func (m *factoryModel) menuClick(ev tea.Mouse) tea.Cmd {
	items := m.menuItems()
	l := placeMenu(items, m.menu.sel, m.menu.x, m.menu.y, m.viewWidth(), m.viewHeight())
	switch i := l.item(ev.X, ev.Y); {
	case ev.Button != tea.MouseLeft || i == -2:
		m.menu = ctxMenu{}
	case i >= 0:
		m.menu.sel = i
		return m.runMenuItem(items, i)
	}
	return nil
}

// runMenuItem closes the menu and presses item i's key the way the
// keyboard would. A dimmed item does nothing and leaves the menu open.
func (m *factoryModel) runMenuItem(items []menuItem, i int) tea.Cmd {
	if i < 0 || i >= len(items) || !items[i].ok {
		return nil
	}
	owner := m.menu.rowKey
	m.menu = ctxMenu{}
	if m.menuOwner() != owner {
		return m.bar.show("that task is no longer there", factoryFlashFailed)
	}
	return m.viewKey(tea.KeyPressMsg(menuPress(items[i].key)))
}

// menuPress is the key press of a menu item's key.
func menuPress(k string) tea.Key {
	switch k {
	case "enter":
		return tea.Key{Code: tea.KeyEnter}
	case "esc":
		return tea.Key{Code: tea.KeyEscape}
	case "backspace":
		return tea.Key{Code: tea.KeyBackspace}
	case "ctrl+r":
		return tea.Key{Code: 'r', Mod: tea.ModCtrl}
	}
	r, _ := utf8.DecodeRuneInString(k)
	return tea.Key{Code: r, Text: k}
}
