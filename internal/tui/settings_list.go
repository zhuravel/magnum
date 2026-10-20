package tui

// The settings screen's list editor, for logins, repositories and other
// lists: add, edit and remove items, their order kept. Each change is
// staged in the draft at once (u takes it back); an item is typed in a huh
// Input.

import (
	"errors"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

var (
	errEmpty    = errors.New("type a value, or esc")
	errDuration = errors.New("a duration, like 15m or 2h")
	errInt      = errors.New("a whole number")
	errPercent  = errors.New("a whole number from 0 to 100")
	errTooLong  = errors.New("300 characters at most")
	errSpace    = errors.New("no spaces")
	errTwice    = errors.New("already in the list")
)

// listEditor is a list popup's state: the items, the selected one
// (len(items) is the "add" line) and the Input of the item being typed.
type listEditor struct {
	field  SettingField
	items  []string
	cur    int
	form   *huh.Form
	val    string
	adding bool
	err    string
}

// openListPopup opens the list editor on f's current items.
func (m settingsModel) openListPopup(f SettingField) (settingsModel, tea.Cmd, bool) {
	cur := m.currentEdit(f)
	items := slices.Clone(cur.List)
	if cur.Default {
		items = nil
	}
	p := m.newPopup(popList)
	p.target, p.list = f.Target, &listEditor{field: f, items: items}
	widest := len([]rune(f.Label))
	for _, it := range items {
		widest = max(widest, len([]rune(cleanText(it)))+2)
	}
	p.size(m, max(widest, 32), 0)
	p.hints = []hint{{"enter", "edit"}, {"a", "add"}, {"x", "remove"}, {"esc", "close (each change is staged now; u on the list takes it back)"}}
	return m.open(p)
}

// listKey handles a key on the list (no item being typed).
func (m settingsModel) listKey(k string) (settingsModel, tea.Cmd, bool) {
	l := m.popup.list
	l.err = ""
	switch k {
	case "j", "down":
		l.cur = min(l.cur+1, len(l.items))
	case "k", "up":
		l.cur = max(l.cur-1, 0)
	case "enter":
		if l.cur >= len(l.items) {
			return m.listEdit(true)
		}
		return m.listEdit(false)
	case "a":
		return m.listEdit(true)
	case "x", "d", "delete", "backspace":
		if l.cur >= len(l.items) {
			return m, nil, true
		}
		items := slices.Delete(slices.Clone(l.items), l.cur, l.cur+1)
		return m.listStage(items, min(l.cur, len(items)))
	}
	return m, nil, true
}

// listEdit opens the Input for a new item (add) or the selected one.
func (m settingsModel) listEdit(add bool) (settingsModel, tea.Cmd, bool) {
	l := m.popup.list
	l.adding, l.val = add, ""
	title := "add to " + l.field.Label
	if !add {
		l.val = l.items[l.cur]
		title = "edit in " + l.field.Label
	}
	in := huh.NewInput().Title(title).Placeholder(listPlaceholder(l.field.Type)).Value(&l.val).
		Validate(func(s string) error { return l.check(s) })
	m.popup.id = nextGen() // this item's form: an earlier item's late answers never reach it
	l.form = m.popup.newForm(huh.NewGroup(in))
	cmds := []tea.Cmd{m.popup.wrap(l.form.Init()), m.popup.resize(m)}
	return m, tea.Batch(cmds...), true
}

// listPlaceholder is the Input's hint for a list type.
func listPlaceholder(t SettingType) string {
	switch t {
	case SettingLogins:
		return "a GitHub login (alice)"
	case SettingRepos:
		return "a repository (web)"
	}
	return ""
}

// check checks an item typed in the Input: its shape by the list's type,
// no duplicate, then the setting's own check of the whole list.
func (l *listEditor) check(s string) error {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return errEmpty
	case strings.ContainsAny(s, " \t") && l.field.Type != SettingList:
		return errSpace
	}
	items, _ := l.withItem(s)
	same := func(a, b string) bool { return a == b } // a check's name is case-sensitive
	if l.field.Type == SettingLogins || l.field.Type == SettingRepos || l.field.Type == SettingRoles {
		same = strings.EqualFold // GitHub logins and repository names are not
	}
	for i, it := range items {
		if slices.ContainsFunc(items[:i], func(o string) bool { return same(o, it) }) {
			return errTwice
		}
	}
	if l.field.ValidateList != nil {
		return l.field.ValidateList(items)
	}
	return nil
}

// withItem is the list with s added after the selected item (at the end
// from the "add" line), or replacing it, and where s is.
func (l *listEditor) withItem(s string) (items []string, at int) {
	items = slices.Clone(l.items)
	if l.adding {
		at = min(l.cur+1, len(items))
		if l.cur >= len(l.items) {
			at = len(items)
		}
		return slices.Insert(items, at, s), at
	}
	items[l.cur] = s
	return items, l.cur
}

// listItemDone stages the list with the item typed.
func (m settingsModel) listItemDone() (settingsModel, tea.Cmd) {
	l := m.popup.list
	items, at := l.withItem(strings.TrimSpace(l.val))
	l.form = nil
	m.popup.id = nextGen() // the item's form is done: its late answers are dropped
	next, cmd, _ := m.listStage(items, at)
	return next, cmd
}

// listStage stages items as the setting's list and selects at.
func (m settingsModel) listStage(items []string, at int) (settingsModel, tea.Cmd, bool) {
	l := m.popup.list
	if l.field.ValidateList != nil && len(items) > 0 {
		if err := l.field.ValidateList(items); err != nil {
			l.err = errLine(err)
			return m, nil, true
		}
	}
	l.items, l.cur = items, max(at, 0)
	e := SettingsEdit{List: slices.Clone(items)}
	switch {
	case len(items) == 0 && !l.field.Set:
		e = SettingsEdit{Default: true} // emptied a list the file never set: nothing to write
	case len(items) == 0:
		e = SettingsEdit{List: []string{}}
	}
	m, _ = m.stageIfChanged(l.field, e)
	cmd := m.previewCmd("")
	return m, cmd, true
}
