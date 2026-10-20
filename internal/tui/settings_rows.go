package tui

// The settings screen's rows: which settings a section lists for the scope
// the screen is on, the roles grid of a section that sets roles, the search
// across every section, and the Runtime section's pauses.

import (
	"slices"
	"strconv"
	"strings"
)

// setRowKind is what a row of the list is.
type setRowKind int

const (
	rowField   setRowKind = iota // a setting
	rowRole                      // a role of the roles grid
	rowHeading                   // a subheading, not selectable
	rowRuntime                   // a pause of the Runtime section
)

// rtWhat is which pause a Runtime row is.
type rtWhat int

const (
	rtAutomation rtWhat = iota
	rtKind
	rtAllKinds
	rtWatch
)

type setRow struct {
	kind   setRowKind
	field  int    // rowField: index into data.Fields
	role   string // rowRole
	label  string // rowHeading's text
	prefix string // a search row's scope ("watch 1 ·")
	what   rtWhat // rowRuntime
	entry  RuntimeEntry
	paused int // rtAllKinds: how many kinds are paused
	kinds  int // rtAllKinds: how many kinds there are
}

func (r setRow) selectable() bool { return r.kind != rowHeading }

// currentSection is the section the rail selects ("" before a load).
func (m settingsModel) currentSection() string {
	if m.section < 0 || m.section >= len(m.sections) {
		return ""
	}
	return m.sections[m.section]
}

// sectionKind is "watch" when a section's settings belong to a watch,
// "repo" when to a repository, else "" (global).
func (m settingsModel) sectionKind(sec string) string {
	if m.data == nil {
		return ""
	}
	for _, f := range m.data.Fields {
		if f.Section != sec || f.Scope == "" {
			continue
		}
		if strings.HasPrefix(f.Scope, "watch:") {
			return "watch"
		}
		return "repo"
	}
	return ""
}

// scopeKind is sectionKind's word for a scope.
func scopeKind(watch bool) string {
	if watch {
		return "watch"
	}
	return "repo"
}

// scopesOf lists the watches (or the repositories) in order.
func (m settingsModel) scopesOf(watch bool) []SettingsScope {
	if m.data == nil {
		return nil
	}
	var out []SettingsScope
	for _, s := range m.data.Scopes {
		if s.Watch == watch {
			out = append(out, s)
		}
	}
	return out
}

// currentScope is the watch or repository the section edits; ok is false
// for a global section or when there is none.
func (m settingsModel) currentScope() (SettingsScope, bool) {
	kind := m.sectionKind(m.currentSection())
	if kind == "" {
		return SettingsScope{}, false
	}
	list, at := m.scopesOf(kind == "watch"), m.watchAt
	if kind == "repo" {
		at = m.repoAt
	}
	if len(list) == 0 {
		return SettingsScope{}, false
	}
	return list[min(at, len(list)-1)], true
}

// scopeLabel names the scope in the title: "Global", "watch 1: talkable",
// "repo talkable/web".
func (m settingsModel) scopeLabel() string {
	s, ok := m.currentScope()
	switch {
	case !ok:
		return "Global"
	case s.Watch:
		return "watch " + strconv.Itoa(s.Index) + ": " + s.Name
	}
	return "repo " + s.Name
}

// scopePrefix is a search row's scope, for a setting of a watch or a
// repository.
func (m settingsModel) scopePrefix(scope string) string {
	if scope == "" || m.data == nil {
		return ""
	}
	for _, s := range m.data.Scopes {
		if s.ID != scope {
			continue
		}
		if s.Watch {
			return "watch " + strconv.Itoa(s.Index) + " · "
		}
		return s.Name + " · "
	}
	return scope + " · "
}

// gridSection reports whether sec sets roles (its settings fill the roles
// grid).
func (m settingsModel) gridSection(sec string) bool {
	if m.data == nil {
		return false
	}
	for _, f := range m.data.Fields {
		if f.Section == sec && f.Role != "" {
			return true
		}
	}
	return false
}

// rows lists the rows the list shows: the search's matches, the Runtime
// section's pauses, or the current section's settings for its scope, then
// its roles.
func (m settingsModel) rows() []setRow {
	if m.data == nil {
		return nil
	}
	if m.query != "" {
		return m.searchRows()
	}
	sec := m.currentSection()
	if sec == sectionRuntime {
		return m.runtimeRows()
	}
	scope := ""
	if s, ok := m.currentScope(); ok {
		scope = s.ID
	}
	var out []setRow
	for i, f := range m.data.Fields {
		if f.Section == sec && f.Scope == scope && f.Role == "" {
			out = append(out, setRow{kind: rowField, field: i})
		}
	}
	if m.gridSection(sec) {
		for _, r := range m.gridRoles() {
			out = append(out, setRow{kind: rowRole, role: r})
		}
	}
	return out
}

// searchRows are the settings whose label or key holds the query, across
// every section and scope; a query starting with * keeps only the ones the
// file sets or the draft changes.
func (m settingsModel) searchRows() []setRow {
	q := strings.ToLower(strings.TrimSpace(m.query))
	changed := strings.HasPrefix(q, "*")
	q = strings.TrimSpace(strings.TrimPrefix(q, "*"))
	var out []setRow
	for i, f := range m.data.Fields {
		if f.Section == sectionRuntime {
			continue
		}
		_, staged := m.draft[f.Target]
		if changed && !f.Set && !staged {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(f.Label), q) && !strings.Contains(strings.ToLower(f.Key), q) {
			continue
		}
		out = append(out, setRow{kind: rowField, field: i, prefix: m.scopePrefix(f.Scope)})
	}
	return out
}

// gridRoles lists the roles of the grid: the preview's order, then any role
// only the settings name.
func (m settingsModel) gridRoles() []string {
	var out []string
	for _, r := range m.preview {
		if !slices.Contains(out, r.Role) {
			out = append(out, r.Role)
		}
	}
	for _, r := range m.data.Roles {
		if !slices.Contains(out, r.Role) {
			out = append(out, r.Role)
		}
	}
	return out
}

// previewRole and savedRole are a role after a restart on the draft, and as
// the file saved resolves it.
func (m settingsModel) previewRole(role string) (SettingsRole, bool) {
	for _, r := range m.preview {
		if r.Role == role {
			return r, true
		}
	}
	return m.savedRole(role)
}

func (m settingsModel) savedRole(role string) (SettingsRole, bool) {
	if m.data == nil {
		return SettingsRole{}, false
	}
	for _, r := range m.data.Roles {
		if r.Role == role {
			return r, true
		}
	}
	return SettingsRole{}, false
}

// roleLevel is the spend level a role follows after a restart on the
// draft, whose table the grid edits.
func (m settingsModel) roleLevel(role string) string {
	for _, r := range m.draftRoles { // the draft's own roles: never a popup's candidate
		if r.Role == role {
			return r.Level
		}
	}
	if r, ok := m.savedRole(role); ok {
		return r.Level
	}
	return ""
}

// roleFields are the settings of a role at a spend level ("" = its base
// [[role]] block), in the grid's cell order.
func (m settingsModel) roleFields(role, level string) []SettingField {
	var out []SettingField
	for _, cell := range []string{"model", "effort", "rereview_effort"} {
		if f, ok := m.roleField(role, level, cell); ok {
			out = append(out, f)
		}
	}
	return out
}

func (m settingsModel) roleField(role, level, cell string) (SettingField, bool) {
	if m.data == nil {
		return SettingField{}, false
	}
	for _, f := range m.data.Fields {
		if f.Role == role && f.Level == level && f.Cell == cell {
			return f, true
		}
	}
	return SettingField{}, false
}

// roleChanged reports whether a role runs a different model or effort
// after a restart on the draft than on the saved file (a new level that
// resolves the same changes nothing it runs).
func (m settingsModel) roleChanged(role string) bool {
	p, _ := m.previewRole(role)
	s, _ := m.savedRole(role)
	return p.Model != s.Model || p.First != s.First || p.Again != s.Again
}

// setState is a row's state word: invalid, unsaved, set or default.
type setState int

const (
	stateDefault setState = iota
	stateSet
	stateUnsaved
	stateInvalid
)

// fieldState is a setting's state.
func (m settingsModel) fieldState(f SettingField) setState {
	_, staged := m.draft[f.Target]
	switch {
	case m.invalid[f.Target] != "" && staged:
		return stateInvalid
	case staged:
		return stateUnsaved
	case f.Set:
		return stateSet
	}
	return stateDefault
}

// roleState is a grid role's state: its worst cell at its level, its base
// model included.
func (m settingsModel) roleState(role string) setState {
	fs := m.roleFields(role, m.roleLevel(role))
	if f, ok := m.roleField(role, "", "model"); ok {
		fs = append(fs, f)
	}
	st := stateDefault
	for _, f := range fs {
		st = max(st, m.fieldState(f))
	}
	return st
}

// sectionDirty reports whether a section holds staged edits (any scope).
func (m settingsModel) sectionDirty(sec string) bool {
	if m.data == nil {
		return false
	}
	for t := range m.draft {
		if f, ok := m.fieldByTarget(t); ok && f.Section == sec {
			return true
		}
	}
	return false
}

// shownValue is what a setting's row shows, cleaned for the screen: the
// staged value, else the value in effect. A list shows its items, each with
// its note ("(judge)").
func (m settingsModel) shownValue(f SettingField) (text string, list []string) {
	text, raw := m.shownRaw(f)
	if len(raw) > 0 {
		list = make([]string, len(raw))
		for i, it := range raw {
			list[i] = cleanText(it)
			if n := f.Notes[it]; n != "" {
				list[i] += " (" + cleanText(n) + ")"
			}
		}
	}
	return cleanText(text), list
}

// inherited is what an unset setting shows: the value in effect, or, while
// the setting it inherits from has a staged edit, that edit's value.
func (m settingsModel) inherited(f SettingField) string {
	if g, ok := m.fieldByTarget(f.Inherits); ok && f.Inherits != "" {
		if _, staged := m.draft[g.Target]; staged {
			text, list := m.shownRaw(g)
			if len(list) > 0 {
				text = strings.Join(list, ", ")
			}
			return "inherit (" + text + ")"
		}
	}
	return orEmpty(f.Value, f.Default)
}

// emptyList is what an empty list stands for: the setting's default
// ("every author", "inherit (the watch's list)"), never a bare "none".
func emptyList(f SettingField) string {
	if !f.Set {
		return orEmpty(f.Value, orEmpty(f.Default, "none"))
	}
	return orEmpty(f.Default, "none")
}

// shownRaw is shownValue without the notes.
func (m settingsModel) shownRaw(f SettingField) (text string, list []string) {
	if e, ok := m.draft[f.Target]; ok {
		switch {
		case e.Default && f.Set:
			return orEmpty(f.Default, "default"), nil
		case e.Default:
			return m.inherited(f), nil
		case f.Type.IsList():
			if len(e.List) == 0 {
				return emptyList(f), nil
			}
			return "", e.List
		}
		return e.Text, nil
	}
	switch {
	case f.Type.IsList() && f.Set && len(f.List) == 0:
		return orEmpty(f.Value, emptyList(f)), nil
	case f.Type.IsList() && f.Set:
		return "", f.List
	case !f.Set:
		return m.inherited(f), nil
	}
	return orEmpty(f.Value, f.Default), nil
}

// rowAt is the row at i.
func rowAt(rows []setRow, i int) (setRow, bool) {
	if i < 0 || i >= len(rows) {
		return setRow{}, false
	}
	return rows[i], true
}

// nextSelectable is the next selectable row from i in direction d (1 or
// -1); i itself when there is none.
func nextSelectable(rows []setRow, i, d int) int {
	for j := i + d; j >= 0 && j < len(rows); j += d {
		if rows[j].selectable() {
			return j
		}
	}
	return max(min(i, len(rows)-1), 0)
}

// moveSelectable moves n selectable rows (negative: up), stopping at the
// ends.
func moveSelectable(rows []setRow, i, n int) int {
	d := 1
	if n < 0 {
		d, n = -1, -n
	}
	for range n {
		j := nextSelectable(rows, i, d)
		if j == i {
			break
		}
		i = j
	}
	return i
}

// intoGrid moves the cursor from the list into the roles grid (in) or
// back to the last setting above it.
func (m settingsModel) intoGrid(rows []setRow, in bool) int {
	r, ok := rowAt(rows, m.cursor)
	if !ok {
		return m.cursor
	}
	switch {
	case in && r.kind == rowField:
		for i, x := range rows {
			if x.kind == rowRole {
				return i
			}
		}
	case !in && r.kind == rowRole:
		for i, x := range slices.Backward(rows) {
			if x.kind == rowField {
				return i
			}
		}
	}
	return m.cursor
}

// fixCursor keeps the cursor on a selectable row of the list.
func (m *settingsModel) fixCursor() {
	rows := m.rows()
	if len(rows) == 0 {
		m.cursor = 0
		return
	}
	m.cursor = min(max(m.cursor, 0), len(rows)-1)
	if !rows[m.cursor].selectable() {
		if j := nextSelectable(rows, m.cursor, 1); j != m.cursor {
			m.cursor = j
		} else {
			m.cursor = nextSelectable(rows, m.cursor, -1)
		}
	}
}
