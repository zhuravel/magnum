package tui

// The settings screen's painter: the title, the rail of sections, the list,
// the roles grid (beside the list on a wide screen, the roles that change
// under it on a narrow one), the detail strip, the footer and the popup over
// the list. Frames are cached by the model's generation.

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/zhuravel/magnum/internal/textx"
)

const (
	settingsRailW   = 20 // the rail's width, its gap included
	settingsTop     = 4  // the title, a blank, the section heading and the column headings
	settingsBottom  = 7  // a blank, the rule, three strip lines, the flash line and the footer
	settingsPanelW  = 52 // the roles grid beside the list, its rule included
	settingsMoreMax = 4  // a list's items shown under its first one
)

func (m settingsModel) View() tea.View {
	k := settingsFrameKey{gen: m.gen, width: m.width, height: m.height, dark: m.st.dark}
	return newView(m.cache.get(k, m.render))
}

// size is the screen's size, with a default before the terminal says.
func (m settingsModel) size() (w, h int) {
	w, h = m.width, m.height
	if w <= 0 {
		w = 120
	}
	if h <= 0 {
		h = 32
	}
	return w, h
}

// bodyHeight is the lines the list gets.
func (m settingsModel) bodyHeight() int {
	_, h := m.size()
	return max(h-settingsTop-settingsBottom, 3)
}

// wideGrid reports whether the roles grid shows beside the list.
func (m settingsModel) wideGrid() bool {
	w, _ := m.size()
	return w >= wideSettings && m.query == "" && m.gridSection(m.currentSection())
}

// listWidth is the list's width (from the cursor mark to its right edge).
func (m settingsModel) listWidth() int {
	w, _ := m.size()
	lw := w - settingsRailW
	if m.wideGrid() {
		lw -= settingsPanelW + 1
	}
	return max(lw, 20)
}

func (m settingsModel) render() string {
	w, h := m.size()
	out := []string{m.titleLine(w), ""}
	switch {
	case m.data == nil:
		out = append(out, m.st.Dim.Render("reading the config…"))
		return strings.Join(out, "\n")
	case m.mode == modeSave && m.plan != nil:
		return strings.Join(append(out, m.saveLines(w, h)...), "\n")
	case m.help:
		return strings.Join(append(out, m.helpLines(w, h)...), "\n")
	}
	body := m.mainLines(w)
	out = append(out, body...)
	out = append(out, "", m.st.Dim.Render(strings.Repeat("─", w)))
	out = append(out, m.stripLines(w)...)
	out = append(out, m.flashLine(w), m.footerLine(w))
	if m.popup != nil {
		m.drawPopup(out, w)
	}
	return strings.Join(out, "\n")
}

// titleLine is "magnum · settings · <scope> · <file> · N unsaved", with
// "restart pending" at the right while the daemon runs an older file.
func (m settingsModel) titleLine(w int) string {
	sep := m.st.Dim.Render(" · ")
	right := ""
	if m.data != nil && m.data.RestartPending {
		right = m.st.Warn.Render("restart pending")
	}
	room := w
	if right != "" {
		room = max(w-ansi.StringWidth(right)-2, 10)
	}
	// The file goes first when the line is short: the unsaved count stays.
	for _, file := range []bool{true, false} {
		left := m.st.Title.Render("magnum · settings")
		if m.data != nil {
			left += sep + m.scopeLabel()
			if file && m.data.File != "" {
				left += sep + m.st.Dim.Render(m.data.File)
			}
			if n := len(m.draft); n > 0 {
				left += sep + m.st.Warn.Render(strconv.Itoa(n)+" unsaved")
			}
		}
		if ansi.StringWidth(left) <= room || !file {
			return spread(truncate(left, room), right, w)
		}
	}
	return ""
}

// mainLines are the section heading, the column headings and the body
// (the rail beside the list, and the roles grid on a wide screen), padded
// to the body's height.
func (m settingsModel) mainLines(w int) []string {
	bodyH := m.bodyHeight()
	rail := m.railLines()
	list, head, cols := m.listLines()
	lw := m.listWidth()
	var panel []string
	if m.wideGrid() {
		panel = m.panelLines(bodyH + 2)
	}
	lines := make([]string, 0, bodyH+2)
	for i := range bodyH + 2 {
		r := ""
		switch {
		case i == 0:
			r = m.st.Header.Render("SECTIONS")
		case i >= 2 && i-2 < len(rail):
			r = rail[i-2]
		}
		var mid string
		switch {
		case i == 0:
			mid = head
		case i == 1:
			mid = cols
		case i-2 < len(list):
			mid = list[i-2]
		}
		line := fit(r, settingsRailW)
		if panel != nil {
			line += fit(mid, lw) + " " + panel[i]
		} else {
			line += truncate(mid, w-settingsRailW)
		}
		lines = append(lines, strings.TrimRight(line, " "))
	}
	return lines
}

// railLines are the sections, the selected one marked, a yellow ● on each
// that holds unsaved edits.
func (m settingsModel) railLines() []string {
	out := make([]string, 0, len(m.sections))
	for i, s := range m.sections {
		dot := ""
		if m.sectionDirty(s) {
			dot = " " + m.st.Warn.Render("●")
		}
		if i == m.section && m.query == "" {
			out = append(out, m.st.Accent.Render("▌")+" "+m.st.Title.Render(s)+dot)
			continue
		}
		out = append(out, "  "+s+dot)
	}
	return out
}

// listLines are the list's lines as shown (scrolled), its heading and its
// column headings.
func (m settingsModel) listLines() (lines []string, head, cols string) {
	all, cur, head, cols := m.listAll()
	return scrolled(all, m.scroll, cur, m.bodyHeight()), head, cols
}

// fixScroll keeps the selected row within the list's lines.
func (m *settingsModel) fixScroll() {
	all, cur, _, _ := m.listAll()
	m.scroll = scrollFor(m.scroll, cur, m.bodyHeight(), len(all))
}

// listAll is every line of the list, the selected row's line, the list's
// heading and its column headings.
func (m settingsModel) listAll() (all []string, cur int, head, cols string) {
	rows := m.rows()
	lw := m.listWidth()
	head = m.sectionHeading(lw, len(rows))
	if m.currentSection() == sectionRuntime && m.query == "" {
		all, cur, cols = m.runtimeLines(rows, lw)
		return all, cur, head, cols
	}
	widths := m.fieldWidths(rows, lw)
	cols = noMark + m.st.headerRow([]column{{title: "SETTING"}, {title: "VALUE"}, {title: "STATE"}}, widths)
	wide := m.wideGrid()
	for i, r := range rows {
		if wide && r.kind == rowRole {
			continue
		}
		if i == m.cursor {
			cur = len(all)
		}
		all = append(all, m.rowLines(r, i == m.cursor, widths)...)
	}
	if len(rows) == 0 {
		all = append(all, noMark+m.st.Dim.Render(m.emptyText()))
	}
	if !wide && m.query == "" && m.gridSection(m.currentSection()) {
		all = append(all, m.changedRolesLines(lw)...)
	}
	return all, cur, head, cols
}

// emptyText says why a list is empty.
func (m settingsModel) emptyText() string {
	switch {
	case m.query != "":
		return "no setting matches"
	case m.sectionKind(m.currentSection()) == "" && m.currentSection() != sectionRuntime:
		return "nothing to edit here"
	}
	kind := "watches"
	if strings.HasSuffix(m.currentSection(), "ies") || m.sectionKind(m.currentSection()) == "repo" {
		kind = "repositories"
	}
	return "no " + kind + " in your config"
}

// scrolled is lines from off, moved so line cur shows, h lines at most.
func scrolled(lines []string, off, cur, h int) []string {
	off = scrollFor(off, cur, h, len(lines))
	return lines[off:min(off+h, len(lines))]
}

// scrollFor is the first line shown so cur shows within h lines of n.
func scrollFor(off, cur, h, n int) int {
	if cur < off {
		off = cur
	}
	if cur >= off+h {
		off = cur - h + 1
	}
	return max(min(off, n-h), 0)
}

// sectionHeading is the list's heading: the section (or the watch, the
// repository, the search) and when its changes apply.
func (m settingsModel) sectionHeading(w, n int) string {
	sep := m.st.Dim.Render(" · ")
	after := m.st.Warn.Render("applies after restart")
	if m.query != "" {
		return truncate(m.st.Section.Render("SEARCH")+sep+m.st.Dim.Render(textx.Count(n, "match", "matches")+" for "+strconv.Quote(m.query)), w)
	}
	sec := m.currentSection()
	if sec == sectionRuntime {
		return truncate(m.st.Section.Render("RUNTIME")+sep+m.st.OK.Render("applies now")+sep+m.st.Dim.Render("not saved to the config file"), w)
	}
	s, ok := m.currentScope()
	switch {
	case ok && s.Watch:
		h := m.st.Section.Render("WATCH "+strconv.Itoa(s.Index)) + sep + s.Name
		if len(s.Repos) > 0 {
			h += sep + m.st.Dim.Render(firstRepos(s.Repos, 3))
		}
		return truncate(h+sep+after, w)
	case ok:
		h := m.st.Section.Render("REPO") + " " + s.Name
		if s.NoBlock {
			h += sep + m.st.Dim.Render("no [[repo]] block yet: a save creates one")
		}
		return truncate(h+sep+after, w)
	}
	return truncate(m.st.Section.Render(strings.ToUpper(sec))+sep+after, w)
}

// fieldWidths sizes the SETTING, VALUE and STATE columns to the rows.
func (m settingsModel) fieldWidths(rows []setRow, lw int) []int {
	cells := make([][]string, 0, len(rows))
	for _, r := range rows {
		switch r.kind {
		case rowField:
			f := m.data.Fields[r.field]
			text, list := m.shownValue(f)
			v := text
			for _, it := range list {
				v = longer(v, it)
			}
			cells = append(cells, []string{r.prefix + f.Label, v, "● unsaved"})
		case rowRole:
			if !m.wideGrid() {
				cells = append(cells, []string{r.role, m.roleValue(r.role, false), "● unsaved"})
			}
		}
	}
	cols := []column{{title: "SETTING", min: 8}, {title: "VALUE", min: 8, flex: true}, {title: "STATE", min: 9}}
	return layoutColumns(cols, cells, lw, len(noMark))
}

// longer is the longer of a and b.
func longer(a, b string) string {
	if ansi.StringWidth(b) > ansi.StringWidth(a) {
		return b
	}
	return a
}

// rowLines draws a setting or a role of the list: its line, then a list's
// further items under its value.
func (m settingsModel) rowLines(r setRow, sel bool, widths []int) []string {
	switch r.kind {
	case rowRole:
		st := m.roleState(r.role)
		return []string{m.cells(sel, []string{r.role, m.roleValue(r.role, true), m.stateCell(st, true)},
			[]string{r.role, m.roleValue(r.role, false), m.stateCell(st, false)}, widths)}
	case rowHeading:
		return []string{noMark + m.st.Section.Render(r.label)}
	}
	f := m.data.Fields[r.field]
	st := m.fieldState(f)
	text, list := m.shownValue(f)
	first := text
	if len(list) > 0 {
		first = list[0]
	}
	label := r.prefix + f.Label
	styled := []string{label, m.valueCell(first, st), m.stateCell(st, true)}
	plain := []string{label, first, m.stateCell(st, false)}
	out := []string{m.cells(sel, styled, plain, widths)}
	if len(list) > 1 {
		more := list[1:]
		if len(more) > settingsMoreMax {
			more = append(more[:settingsMoreMax-1:settingsMoreMax-1], "+"+strconv.Itoa(len(list)-settingsMoreMax)+" more")
		}
		pad := noMark + spaces(widths[0]+len(colGap))
		for _, it := range more {
			out = append(out, pad+truncate(m.valueCell(it, st), widths[1]))
		}
	}
	return out
}

// cells lays a row out: plain text in reverse video when selected (with
// the cursor mark), else styled.
func (m settingsModel) cells(sel bool, styled, plain []string, widths []int) string {
	if sel {
		return cursorMark + m.st.Selected.Render(renderRow(plain, widths))
	}
	return noMark + renderRow(styled, widths)
}

// valueCell is a setting's value as its state shows it: dim when the
// default applies.
func (m settingsModel) valueCell(v string, st setState) string {
	if st == stateDefault {
		return m.st.Dim.Render(v)
	}
	return v
}

// stateCell is a state's word: default dim, set plain, ● unsaved yellow, ✗
// invalid red (plain text when not styled).
func (m settingsModel) stateCell(st setState, styled bool) string {
	word := [...]string{"default", "set", "● unsaved", "✗ invalid"}[st]
	if !styled {
		return word
	}
	switch st {
	case stateDefault:
		return m.st.Dim.Render(word)
	case stateUnsaved:
		return m.st.Warn.Render(word)
	case stateInvalid:
		return m.st.Err.Render(word)
	}
	return word
}

// roleValue is a role's list value on a narrow screen: "model first/again"
// after a restart, a changed part marked * (yellow when styled).
func (m settingsModel) roleValue(role string, styled bool) string {
	p, _ := m.previewRole(role)
	s, _ := m.savedRole(role)
	part := func(now, was string) string {
		v := orDash(now)
		if now != was {
			v += "*"
			if styled {
				v = m.st.Warn.Render(v)
			}
		}
		return v
	}
	return part(p.Model, s.Model) + " " + part(p.First, s.First) + "/" + part(p.Again, s.Again)
}

// roleCols are the roles grid's columns.
var roleCols = []column{{title: "ROLE", min: 6}, {title: "MODEL", min: 6, flex: true}, {title: "FIRST", min: 5}, {title: "AGAIN", min: 5}}

// roleCells are a role's grid cells after a restart; changed cells get a *
// and yellow, the rest of a changed role plain, an unchanged role dim.
func (m settingsModel) roleCells(role string, styled bool) []string {
	p, _ := m.previewRole(role)
	s, _ := m.savedRole(role)
	changed := m.roleChanged(role)
	cell := func(now, was string) string {
		v := orDash(now)
		switch {
		case now != was && styled:
			return m.st.Warn.Render(v + "*")
		case now != was:
			return v + "*"
		case styled && !changed:
			return m.st.Dim.Render(v)
		}
		return v
	}
	name := role
	if styled && !changed {
		name = m.st.Dim.Render(role)
	}
	return []string{name, cell(p.Model, s.Model), cell(p.First, s.First), cell(p.Again, s.Again)}
}

// gridTitle is the roles grid's heading: the level, or the popup's
// candidate it previews.
func (m settingsModel) gridTitle() string {
	what := "spend " + m.spendShown()
	if m.previewCand != "" {
		what = m.previewCand
	}
	return m.st.Section.Render("ROLES AFTER RESTART") + m.st.Dim.Render(" · "+what)
}

// spendShown is the spend level the preview resolves most roles at.
func (m settingsModel) spendShown() string {
	count := map[string]int{}
	best := ""
	for _, r := range m.preview {
		count[r.Level]++
		if best == "" || count[r.Level] > count[best] {
			best = r.Level
		}
	}
	return orDash(best)
}

// gridWidths sizes the roles grid's columns for width w.
func (m settingsModel) gridWidths(roles []string, w int) []int {
	cells := make([][]string, 0, len(roles))
	for _, r := range roles {
		cells = append(cells, m.roleCells(r, false))
	}
	return layoutColumns(roleCols, cells, w, len(noMark))
}

// panelLines are the roles grid beside the list, n lines with its rule.
func (m settingsModel) panelLines(n int) []string {
	rule := m.st.Dim.Render("│") + " "
	roles := m.gridRoles()
	inner := settingsPanelW - 2
	widths := m.gridWidths(roles, inner)
	rows := m.rows()
	var body []string
	body = append(body, truncate(m.gridTitle(), inner), noMark+m.st.headerRow(roleCols, widths))
	for _, role := range roles {
		sel := false
		if r, ok := rowAt(rows, m.cursor); ok && r.kind == rowRole && r.role == role {
			sel = true
		}
		if sel {
			body = append(body, cursorMark+m.st.Selected.Render(renderRow(m.roleCells(role, false), widths)))
			continue
		}
		body = append(body, noMark+renderRow(m.roleCells(role, true), widths))
	}
	body = append(body, "", m.gridNote(inner))
	out := make([]string, n)
	for i := range out {
		line := ""
		if i < len(body) {
			line = body[i]
		}
		out[i] = rule + truncate(line, inner)
	}
	return out
}

// gridNote is the line under the roles: what * means, or why the preview
// failed.
func (m settingsModel) gridNote(w int) string {
	if m.previewErr != nil {
		return m.st.Err.Render(truncate("✗ "+errLine(m.previewErr), w))
	}
	return m.st.Dim.Render("* differs from the saved file")
}

// changedRolesLines are, under a narrow list, the roles whose model or
// efforts change against the saved file.
func (m settingsModel) changedRolesLines(w int) []string {
	var changed []string
	for _, r := range m.gridRoles() {
		if m.roleChanged(r) {
			changed = append(changed, r)
		}
	}
	out := []string{"", noMark + m.gridTitle()}
	if len(changed) == 0 {
		return append(out, noMark+m.st.Dim.Render("no role changes against the saved file"))
	}
	widths := m.gridWidths(changed, w)
	out = append(out, noMark+noMark+m.st.headerRow(roleCols, widths))
	for _, r := range changed {
		out = append(out, noMark+noMark+renderRow(m.roleCells(r, true), widths))
	}
	return append(out, "", noMark+m.gridNote(w-2))
}

// runtimeLines are the Runtime section's rows: what, its state, the key
// that acts on it.
func (m settingsModel) runtimeLines(rows []setRow, lw int) (all []string, cursorLine int, cols string) {
	c := []column{{title: "WHAT", min: 8}, {title: "STATE", min: 10, flex: true}, {title: "ACTION", min: 6}}
	cells := make([][]string, 0, len(rows))
	for _, r := range rows {
		if r.kind != rowRuntime {
			continue
		}
		key, label := rtAction(r)
		cells = append(cells, []string{rtLabel(r), rtState(r), strings.TrimSpace(key + " " + label)})
	}
	widths := layoutColumns(c, cells, lw, len(noMark))
	cols = noMark + m.st.headerRow(c, widths)
	switch {
	case m.rt == nil:
		all = append(all, noMark+m.st.Dim.Render("runtime controls need magnum's registry"))
	case m.rtErr != nil && m.runtime == nil:
		all = append(all, noMark+m.st.Err.Render("cannot read the pauses: "+errLine(m.rtErr)))
	case m.runtime == nil:
		all = append(all, noMark+m.st.Dim.Render("reading the pauses…"))
	}
	for i, r := range rows {
		if i == m.cursor {
			cursorLine = len(all)
		}
		if r.kind == rowHeading {
			all = append(all, noMark+m.st.Section.Render(r.label))
			continue
		}
		key, label := rtAction(r)
		action := orDash(strings.TrimSpace(key + " " + label))
		plain := []string{rtLabel(r), rtState(r), action}
		if i == m.cursor {
			all = append(all, cursorMark+m.st.Selected.Render(renderRow(plain, widths)))
			continue
		}
		all = append(all, noMark+renderRow([]string{rtLabel(r), m.rtStateCell(r), m.actionCell(key, label)}, widths))
	}
	if m.runtime != nil && !m.runtime.Daemon {
		all = append(all, "", noMark+m.st.Dim.Render("no daemon is running: a change applies when it starts"))
	}
	return all, cursorLine, cols
}

// rtStateCell is a Runtime row's state, styled: running green, a pause's
// "paused" yellow, no pause dim.
func (m settingsModel) rtStateCell(r setRow) string {
	s := rtState(r)
	switch {
	case r.what == rtAllKinds:
		if r.paused > 0 {
			return m.st.Warn.Render(s)
		}
		return m.st.Dim.Render(s)
	case r.entry.Paused:
		if word, rest, ok := strings.Cut(s, ":"); ok {
			return m.st.Warn.Render(word) + ":" + rest
		}
		return m.st.Warn.Render(s)
	case r.what == rtAutomation:
		return m.st.OK.Render(s)
	}
	return m.st.Dim.Render(s)
}

// actionCell is the ACTION cell: the key, then its short label.
func (m settingsModel) actionCell(key, label string) string {
	if key == "" {
		return m.st.Dim.Render("-")
	}
	return m.st.Key.Render(key) + " " + label
}

// stripLines are the detail strip's three lines for the selected row.
func (m settingsModel) stripLines(w int) []string {
	r, ok := rowAt(m.rows(), m.cursor)
	after := m.st.Warn.Render("applies after restart")
	now := m.st.OK.Render("applies now")
	sep := m.st.Dim.Render(" · ")
	if !ok {
		return []string{"", "", ""}
	}
	switch r.kind {
	case rowRuntime:
		return []string{
			spread(m.st.Accent.Render(rtLabel(r)), now, w),
			truncate(orEmpty(r.entry.Detail, rtState(r)), w),
			m.st.Dim.Render(truncate(rtCommand(r), w)),
		}
	case rowRole:
		return m.roleStrip(r.role, w)
	case rowField:
		f := m.data.Fields[r.field]
		applies := after
		if f.AppliesNow {
			applies = now
		}
		desc := f.Description
		if msg := m.invalid[f.Target]; msg != "" && m.fieldState(f) == stateInvalid {
			desc = m.st.Err.Render("✗ " + msg)
		}
		var facts []string
		if _, staged := m.draft[f.Target]; staged {
			facts = append(facts, m.st.Warn.Render("was "+orEmpty(savedText(f), "unset")))
		}
		if f.Default != "" {
			facts = append(facts, m.st.Dim.Render("default: "+f.Default))
		}
		if f.From != "" {
			facts = append(facts, m.st.Dim.Render("from: "+f.From))
		}
		return []string{spread(m.st.Accent.Render(f.Key), applies, w), truncate(desc, w), truncate(strings.Join(facts, sep), w)}
	}
	return []string{"", "", ""}
}

// savedText is a setting's value as saved, for "was".
func savedText(f SettingField) string {
	if f.Type.IsList() && f.Set {
		return cleanText("[" + strings.Join(f.List, ", ") + "]")
	}
	return f.Value
}

// orEmpty is s, or alt when s is blank.
func orEmpty(s, alt string) string {
	if strings.TrimSpace(s) == "" {
		return alt
	}
	return s
}

// roleStrip is the detail strip of a grid role: its table, what it runs,
// and what its staged cells were.
func (m settingsModel) roleStrip(role string, w int) []string {
	level := m.roleLevel(role)
	key := "[spend." + level + "]." + role
	if level == "" {
		key = "[[role]] " + role
	}
	p, _ := m.previewRole(role)
	desc := role + " runs " + orDash(p.Model) + ", first " + orDash(p.First) + ", again " + orDash(p.Again)
	if level != "" {
		desc += " while spend is " + level
	}
	desc += " · enter edits its table or its base model"
	var facts []string
	sep := m.st.Dim.Render(" · ")
	fs := m.roleFields(role, level)
	if f, ok := m.roleField(role, "", "model"); ok {
		fs = append(fs, f)
	}
	from := ""
	for _, f := range fs {
		if _, staged := m.draft[f.Target]; staged {
			facts = append(facts, m.st.Warn.Render("was "+f.Cell+" "+orEmpty(savedText(f), "unset")))
		}
		if from == "" && f.Set && f.From != "" {
			from = f.From
		}
	}
	if f, ok := m.roleField(role, level, "model"); ok && from == "" {
		from = f.From // the default's source, or why the role is not edited here
	}
	if from != "" { // before the default: it may say why the role is not edited here
		facts = append(facts, m.st.Dim.Render("from: "+from))
	}
	if f, ok := m.roleField(role, level, "model"); ok && f.Default != "" && level != "" {
		facts = append(facts, m.st.Dim.Render("default model: "+f.Default))
	}
	return []string{spread(m.st.Accent.Render(key), m.st.Warn.Render("applies after restart"), w), truncate(desc, w), truncate(strings.Join(facts, sep), w)}
}

// flashLine is the line over the footer: the search being typed, a
// request in flight, or the last answer.
func (m settingsModel) flashLine(w int) string {
	switch {
	case m.mode == modeSearch:
		return truncate(m.search.View(), w)
	case m.busy == "" && m.flash == "" && m.loadErr != nil:
		return m.st.Err.Render(truncate("cannot read the config: "+errLine(m.loadErr)+" (ctrl+r reads it again)", w))
	case m.busy == "" && m.flash == "" && m.data != nil && len(m.data.Warnings) > 0:
		return m.st.Warn.Render(truncate("warning: "+strings.Join(m.data.Warnings, "; "), w))
	case m.busy != "":
		return m.st.Dim.Render(truncate(m.busy+"…", w))
	case m.reloading && m.flash == "":
		return m.st.Dim.Render(truncate("reading the file and the pauses again…", w))
	case m.flash != "" && m.flashOK:
		return m.st.OK.Render(truncate(m.flash, w))
	case m.flash != "":
		return m.st.Warn.Render(truncate(m.flash, w))
	}
	return ""
}

// footerLine is the pending question, or the keys for what the screen
// shows.
func (m settingsModel) footerLine(w int) string {
	if m.confirm != nil {
		return m.st.confirmLine(m.confirm.question, w)
	}
	if m.popup != nil {
		return m.st.fitHints(w, m.popup.hints)
	}
	if m.mode == modeSearch {
		return m.st.fitHints(w,
			[]hint{{"type", "label or key"}, {"*", "only what your file sets or the draft changes"}, {"enter", "keep"}, {"esc", "clear"}},
			[]hint{{"enter", "keep"}, {"esc", "clear"}})
	}
	tail := []hint{{"?", "help"}, {"esc", "back"}}
	switch kind := m.sectionKind(m.currentSection()); {
	case m.currentSection() == sectionRuntime && m.query == "":
		return m.st.fitHints(w,
			append([]hint{{"j/k", "move"}, {"tab", "section"}, {"p", "pause"}, {"r", "resume"}, {"R", "resume all"}, {"x", "restart sessions"}, {"c", "scope"}}, tail...),
			append([]hint{{"j/k", "move"}, {"tab", "section"}, {"p", "pause"}, {"r", "resume"}, {"R", "resume all"}, {"x", "restart"}}, tail...),
			append([]hint{{"p", "pause"}, {"r", "resume"}, {"R", "resume all"}, {"x", "restart"}}, tail...), tail)
	case kind != "" && m.query == "":
		edit := "edit"
		if r, ok := rowAt(m.rows(), m.cursor); ok && r.kind == rowField && m.data.Fields[r.field].Type.IsList() {
			edit = "edit list"
		}
		return m.st.fitHints(w,
			append([]hint{{"j/k", "move"}, {"enter", edit}, {"d", "default"}, {"c", "scope"}, {"[/]", kind}, {"s", "save"}, {"u", "undo"}}, tail...),
			append([]hint{{"enter", edit}, {"[/]", kind}, {"s", "save"}}, tail...), tail)
	}
	return m.st.fitHints(w,
		append([]hint{{"j/k", "move"}, {"tab", "section"}, {"enter", "edit"}, {"d", "default"}, {"/", "search"}, {"c", "scope"}, {"s", "save"}, {"u", "undo"}}, tail...),
		append([]hint{{"j/k", "move"}, {"tab", "section"}, {"enter", "edit"}, {"s", "save"}}, tail...), tail)
}

// drawPopup draws the popup's box over the list, under the selected row.
func (m settingsModel) drawPopup(out []string, w int) {
	p := m.popup
	content := m.popupContent(p)
	box := m.boxed(content, p.width)
	bw := p.width + 4
	x := settingsRailW + len(noMark)
	rows := m.rows()
	y := settingsTop
	if r, ok := rowAt(rows, m.cursor); ok {
		if m.wideGrid() && r.kind == rowRole {
			x = max(w-settingsPanelW-bw, settingsRailW)
			y = settingsTop + m.gridIndex(r.role) + 1
		} else {
			widths := m.fieldWidths(rows, m.listWidth())
			x += widths[0] + len(colGap)
			y = settingsTop + m.cursorLine() + 1
		}
	}
	bottom := settingsTop + m.bodyHeight()
	y = max(min(y, bottom-len(box)), settingsTop-2)
	x = max(min(x, w-bw), 0)
	overlay(out, box, x, y, w)
}

// gridIndex is a role's line in the grid beside the list.
func (m settingsModel) gridIndex(role string) int {
	for i, r := range m.gridRoles() {
		if r == role {
			return i
		}
	}
	return 0
}

// cursorLine is the selected row's line in the list as shown (scrolled).
func (m settingsModel) cursorLine() int {
	all, cur, _, _ := m.listAll()
	return cur - scrollFor(m.scroll, cur, m.bodyHeight(), len(all))
}

// popupContent is the popup's lines: the form, or the list editor.
func (m settingsModel) popupContent(p *setPopup) []string {
	if p.list == nil {
		return strings.Split(strings.TrimRight(p.form.View(), "\n"), "\n")
	}
	l := p.list
	out := []string{m.st.Label.Render(l.field.Label)}
	if l.form != nil {
		out = append(out, strings.Split(strings.TrimRight(l.form.View(), "\n"), "\n")...)
		return out
	}
	for i, it := range l.items {
		it = cleanText(it)
		mark := noMark
		if i == l.cur {
			mark = m.st.Accent.Render(cursorMark)
			it = m.st.Accent.Render(it)
		}
		out = append(out, mark+it)
	}
	add := "+ add"
	if l.cur == len(l.items) {
		out = append(out, m.st.Accent.Render(cursorMark+add))
	} else {
		out = append(out, noMark+m.st.Dim.Render(add))
	}
	if len(l.items) == 0 {
		out = append(out, m.st.Dim.Render("empty: "+emptyList(l.field)))
	}
	if l.err != "" {
		out = append(out, m.st.Err.Render("✗ "+l.err))
	}
	return out
}

// boxed draws lines in a rounded box of inner width w.
func (m settingsModel) boxed(lines []string, w int) []string {
	edge := m.st.Dim
	out := []string{edge.Render("╭" + strings.Repeat("─", w+2) + "╮")}
	for _, l := range lines {
		out = append(out, edge.Render("│")+" "+fit(l, w)+" "+edge.Render("│"))
	}
	return append(out, edge.Render("╰"+strings.Repeat("─", w+2)+"╯"))
}

// saveLines are the save view under the title: the changes' diff, the
// notes and the history, when they apply, then the footer.
func (m settingsModel) saveLines(w, h int) []string {
	p := m.plan
	sep := m.st.Dim.Render(" · ")
	head := m.st.Section.Render("SAVE") + sep + m.st.Dim.Render(textx.Count(p.Count, "change", "changes"))
	if p.Restore > 0 {
		head += sep + m.st.Dim.Render("puts back kept version "+strconv.Itoa(p.Restore))
	}
	head += sep + m.st.Dim.Render("checked with magnum's loader and the prompt check: ") + m.st.OK.Render("ok")
	var body []string
	for i, hk := range p.Hunks {
		if i > 0 {
			body = append(body, "")
		}
		where := m.st.Header.Render(hk.Table)
		if hk.Line > 0 {
			where += m.st.Dim.Render(" · line " + strconv.Itoa(hk.Line))
		}
		body = append(body, truncate("  "+where, w))
		for _, l := range hk.Removed {
			body = append(body, m.st.Err.Render(truncate("- "+l, w)))
		}
		for _, l := range hk.Added {
			body = append(body, m.addedLine(l, w))
		}
	}
	for _, warn := range p.Warnings {
		body = append(body, m.st.Warn.Render(truncate("warning: "+warn, w)))
	}
	notes := "dated notes: on (n turns them off)"
	switch {
	case p.Restore > 0:
		notes = "a restore writes the kept file as it was"
	case !p.Notes:
		notes = "dated notes: off (n turns them on)"
	}
	tail := []string{"",
		m.st.Dim.Render(truncate(notes+" · the previous file goes to "+m.data.HistoryDir, w)),
		m.st.Warn.Render("applies after restart") + m.st.Dim.Render(truncate(": the daemon reads the config at start; agents take new models at their next launch or resume", w-21)),
	}
	room := max(h-2-2-len(tail)-2, 3) // title and blank, heading and blank, tail, flash and footer
	off := min(m.saveOff, max(len(body)-room, 0))
	shown := body[off:min(off+room, len(body))]
	out := []string{truncate(head, w), ""}
	out = append(out, shown...)
	for len(out) < room+2 {
		out = append(out, "")
	}
	out = append(out, tail...)
	out = append(out, m.flashLine(w))
	if m.confirm != nil {
		return append(out, m.st.confirmLine(m.confirm.question, w))
	}
	notesKey := "notes off"
	if !p.Notes {
		notesKey = "notes on"
	}
	hs := []hint{{"n", notesKey}, {"s", "save"}, {"esc", "back to editing"}}
	if len(body) > room {
		hs = append(hs, hint{"j/k", "scroll"})
	}
	return append(out, m.st.fitHints(w, hs))
}

// addedLine is a "+" line of the diff, its trailing comment dim.
func (m settingsModel) addedLine(l string, w int) string {
	if i := strings.LastIndex(l, "  # "); i > 0 {
		return truncate(m.st.OK.Render("+ "+l[:i])+m.st.Dim.Render(l[i:]), w)
	}
	return m.st.OK.Render(truncate("+ "+l, w))
}

// helpLines are the keys, in a box over the list.
func (m settingsModel) helpLines(w, h int) []string {
	lines := []string{m.st.Title.Render("Keys")}
	for _, k := range settingsKeys() {
		lines = append(lines, m.st.Key.Render(fit(k.key, 13))+" "+k.desc)
	}
	room := max(h-4, 3)
	off := min(m.helpOff, max(len(lines)-room+2, 0))
	inner := min(w-4, 90)
	box := m.boxed(lines[off:min(off+room-2, len(lines))], inner)
	out := append([]string{}, box...)
	return append(out, "", m.st.hints(w, hint{"j/k", "scroll"}, hint{"?", "close help"}))
}

// settingsKeys is the help's content.
func settingsKeys() []hint {
	return []hint{
		{"j/k ↑/↓", "move; pgup/pgdn half a page, g/G the ends"},
		{"tab/shift+tab", "next / previous section"},
		{"l/h →/←", "into the roles grid and back (Spend & models, wide screens)"},
		{"enter", "edit the setting in a popup (enter stages, esc cancels)"},
		{"a / x", "in a list: add after the selected item / remove it (each is staged)"},
		{"d", "back to the default: your key goes, its comment stays"},
		{"u", "undo the last staged edit"},
		{"ctrl+r / F5", "read the pauses again, and the file when nothing is staged"},
		{"/", "search every label and key; a leading * keeps what is set or staged"},
		{"c", "scope: Global, a watch, a repository"},
		{"[ / ]", "previous / next watch (Watches) or repository (Repositories)"},
		{"s", "save: the diff first (n notes on/off), then s and y write it"},
		{"p", "Runtime: pause automation (asks y/N; applies now)"},
		{"r", "Runtime: resume the selected row (asks y/N; applies now)"},
		{"R", "Runtime: resume every agent kind (asks y/N; applies now)"},
		{"x", "Runtime: restart a kind's live sessions in new processes (asks y/N)"},
		{"y", "answer yes; any other key, enter too, cancels"},
		{"?", "close this help"},
		{"esc, q", "back (asks before discarding unsaved changes; ctrl+c too)"},
	}
}
