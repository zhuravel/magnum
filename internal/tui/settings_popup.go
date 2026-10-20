package tui

// The settings screen's popups: a huh form over the list for one edit. huh
// edits the field and the screen keeps the frame: the popup ends when the
// form completes, its values are staged in the draft (nothing is written),
// esc cancels it, and huh's own quit key, filter and external editor are
// off. No huh Confirm is ever used: saving and discarding ask y/N on the
// screen's own footer.

import (
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

// popupKind is what a popup edits.
type popupKind int

const (
	popField popupKind = iota // one setting
	popRole                   // a role's model and efforts at its spend level, or its base model
	popScope                  // the scope picker
	popPause                  // the pause form
	popList                   // a list of logins, repositories or names
)

// The values of a Select that are not a setting's value.
const (
	optDefault = "\x00default" // back to the default (the key goes)
	optOther   = "\x00other"   // another value, typed in the next field
)

// popupTextLimit caps a text setting (stakes and the like).
const popupTextLimit = 300

// setPopup is the open popup. huh binds its fields to the values here, so
// it lives behind a pointer every copy of the model shares.
type setPopup struct {
	id     int64 // tags its form's messages: a closed popup's never reach the next
	kind   popupKind
	target string // popField, popList: the setting
	role   string // popRole
	level  string // popRole: the spend level whose table it edits
	form   *huh.Form
	list   *listEditor
	theme  *popupTheme
	// selects are its Selects with the values they bind and the description
	// of their hovered option: synced after every message, since huh's
	// g/G (and its half pages) move the cursor without writing the value.
	selects      []boundSelect
	width        int // the box's inner width
	widest, most int // what the width is made of, again on a resize
	live         bool
	cand         string // the candidate the live preview last asked for
	hints        []hint

	// The values huh's fields bind to.
	choice, text string
	multi        []string
	where        string // popRole: "table" or "base"
	model, first, again, other,
	baseModel, baseOther string
	pauseHow, pauseFor, pauseUntil, pauseReason string

	// done stages the popup's values once its form completes; candidate is
	// the draft its current values make, for the live roles preview (nil:
	// no preview).
	done      func(m settingsModel, p *setPopup) (settingsModel, tea.Cmd)
	candidate func(p *setPopup) (label string, d SettingsDraft)
}

// boundSelect is one Select of a popup, the value it binds, and how the
// hovered option is described (nil: a fixed description).
type boundSelect struct {
	sel      *huh.Select[string]
	val      *string
	describe func(choice string) string
}

// popupTheme is a popup's huh theme over the screen's current styles: huh
// keeps the first theme a field gets, so a background answer changes the
// styles it reads, not the theme.
type popupTheme struct{ st styles }

// Theme builds huh's styles from the screen's.
func (t *popupTheme) Theme(bool) *huh.Styles { return settingsStyles(t.st) }

// newPopup is a popup of kind with its own id and theme.
func (m settingsModel) newPopup(kind popupKind) *setPopup {
	return &setPopup{id: nextGen(), kind: kind, theme: &popupTheme{st: m.st}}
}

// size sets the popup's width from its widest line, at most most cells
// (0: popupWidth's cap), kept for a resize.
func (p *setPopup) size(m settingsModel, widest, most int) {
	if most == 0 {
		most = popupMaxWidth
	}
	p.widest, p.most = widest, most
	p.width = m.popupWidthMax(widest, most)
}

// bind keeps sel and the value it binds in step, describing the hovered
// option with describe (nil: sel's own description).
func (p *setPopup) bind(sel *huh.Select[string], val *string, describe func(string) string) *huh.Select[string] {
	p.selects = append(p.selects, boundSelect{sel: sel, val: val, describe: describe})
	if describe != nil {
		sel.Description(describe(*val))
	}
	return sel
}

// sync writes each Select's hovered option to its value and its
// description, on the screen's goroutine (no huh evaluation runs elsewhere).
func (p *setPopup) sync() {
	for _, b := range p.selects {
		if v, ok := b.sel.Hovered(); ok {
			*b.val = v
		}
		if b.describe != nil {
			b.sel.Description(b.describe(*b.val))
		}
	}
}

// popupMsg is a message one of a popup's commands answered, with the popup.
type popupMsg struct {
	id  int64
	msg tea.Msg
}

// blinkCode is a cursor blink's command: it is left untagged (a blink names
// its cursor, which no other cursor takes) and it waits half a second, so
// it must stay recognisable as itself.
var blinkCode = func() uintptr {
	c := cursor.New()
	return reflect.ValueOf(c.Blink()).Pointer()
}()

// wrap tags the messages of cmd with the popup, expanding batches and
// sequences so each of their commands is tagged too.
func (p *setPopup) wrap(cmd tea.Cmd) tea.Cmd { return tagCmd(p.id, cmd) }

func tagCmd(id int64, cmd tea.Cmd) tea.Cmd {
	if cmd == nil || reflect.ValueOf(cmd).Pointer() == blinkCode {
		return cmd
	}
	return func() tea.Msg { return tagMsg(id, cmd()) }
}

func tagMsg(id int64, msg tea.Msg) tea.Msg {
	switch m := msg.(type) {
	case nil:
		return nil
	case tea.BatchMsg:
		out := make(tea.BatchMsg, 0, len(m))
		for _, c := range m {
			out = append(out, tagCmd(id, c))
		}
		return out
	}
	if v := reflect.ValueOf(msg); v.Kind() == reflect.Slice && v.Type().Elem() == reflect.TypeFor[tea.Cmd]() {
		cmds := make([]tea.Cmd, v.Len())
		for i := range cmds {
			c, _ := reflect.TypeAssert[tea.Cmd](v.Index(i)) // the slice's element type is tea.Cmd
			cmds[i] = tagCmd(id, c)
		}
		return tea.Sequence(cmds...)()
	}
	return popupMsg{id: id, msg: msg}
}

// settingsStyles are huh's styles in magnum's: the screens' 16 colors,
// the "› " cursor, dim descriptions and red errors.
func settingsStyles(st styles) *huh.Styles {
	{
		t := huh.ThemeBase(st.dark)
		f := &t.Focused
		f.Base = lipgloss.NewStyle()
		f.Card = f.Base
		f.Title = st.Label
		f.NoteTitle = st.Label
		f.Description = st.Dim
		f.ErrorIndicator = st.Err.SetString(" ✗")
		f.ErrorMessage = st.Err
		f.SelectSelector = st.Accent.SetString(cursorMark)
		f.Option = lipgloss.NewStyle()
		f.SelectedOption = st.Accent
		f.UnselectedOption = lipgloss.NewStyle()
		f.MultiSelectSelector = st.Accent.SetString(cursorMark)
		f.SelectedPrefix = st.OK.SetString("[x] ")
		f.UnselectedPrefix = st.Dim.SetString("[ ] ")
		f.NextIndicator = st.Dim.MarginLeft(1).SetString("→")
		f.PrevIndicator = st.Dim.MarginRight(1).SetString("←")
		f.TextInput.Cursor = st.Accent
		f.TextInput.CursorText = lipgloss.NewStyle()
		f.TextInput.Prompt = st.Accent
		f.TextInput.Placeholder = st.Dim
		f.TextInput.Text = lipgloss.NewStyle()
		t.Blurred = *f
		t.Blurred.Title = st.Dim
		t.Blurred.SelectSelector = lipgloss.NewStyle().SetString(noMark)
		t.Blurred.MultiSelectSelector = lipgloss.NewStyle().SetString(noMark)
		t.Blurred.SelectedOption = lipgloss.NewStyle()
		t.Blurred.NextIndicator = lipgloss.NewStyle()
		t.Blurred.PrevIndicator = lipgloss.NewStyle()
		t.Group.Title = st.Label
		t.Group.Description = st.Dim
		t.FieldSeparator = lipgloss.NewStyle().SetString("\n")
		return t
	}
}

// settingsKeyMap is huh's key map without what the screen keeps for
// itself: ctrl+c (huh's quit, which would abort silently) reaches the
// screen, which asks before discarding; a Select's filter and a Text's
// external editor are off.
func settingsKeyMap() *huh.KeyMap {
	km := huh.NewDefaultKeyMap()
	km.Quit = key.NewBinding(key.WithKeys("ctrl+c"), key.WithDisabled())
	km.Select.Filter.SetEnabled(false)
	km.MultiSelect.Filter.SetEnabled(false)
	km.Text.Editor.SetEnabled(false)
	return km
}

// newForm builds a popup's form with its theme and the screen's keys, help
// off.
func (p *setPopup) newForm(groups ...*huh.Group) *huh.Form {
	return huh.NewForm(groups...).WithShowHelp(false).WithShowErrors(true).
		WithTheme(p.theme).WithKeyMap(settingsKeyMap())
}

// open shows p: its form starts (Init) and gets its size at once, since an
// embedded form sizes itself only from a WindowSizeMsg (huh's popup sizing
// bug otherwise leaves it at zero width until the terminal resizes).
func (m settingsModel) open(p *setPopup) (settingsModel, tea.Cmd, bool) {
	m.popup = p
	var cmds []tea.Cmd
	if p.form != nil {
		cmds = append(cmds, p.wrap(p.form.Init()))
	}
	cmds = append(cmds, p.resize(m))
	if p.live {
		label, d := p.candidate(p)
		p.cand = draftKey(d)
		cmds = append(cmds, m.previewCmd(label, d))
	}
	return m, tea.Batch(cmds...), true
}

// resize gives the popup its share of the screen: its width again from
// what it shows (the screen may have changed), then its form's size.
func (p *setPopup) resize(m settingsModel) tea.Cmd {
	if p.most > 0 {
		p.width = m.popupWidthMax(p.widest, p.most)
	}
	f := p.form
	if p.list != nil {
		f = p.list.form
	}
	if f == nil {
		return nil
	}
	_, cmd := f.Update(tea.WindowSizeMsg{Width: p.width, Height: max(m.bodyHeight()-2, 4)})
	return p.wrap(cmd)
}

// restyle repaints the popup in new styles (the terminal's background
// answered).
func (p *setPopup) restyle(st styles) { p.theme.st = st }

// update feeds msg to the popup's form and syncs its Selects.
func (p *setPopup) update(msg tea.Msg) tea.Cmd {
	f := p.form
	if p.list != nil {
		f = p.list.form
	}
	if f == nil {
		return nil
	}
	_, cmd := f.Update(msg)
	p.sync()
	return p.wrap(cmd)
}

// popupKey handles a key while a popup is open: esc cancels it, ctrl+c
// cancels it and leaves (asking when the draft holds edits), anything else
// goes to the form.
func (m settingsModel) popupKey(msg tea.KeyPressMsg) (settingsModel, tea.Cmd, bool) {
	p := m.popup
	switch msg.String() {
	case "esc":
		if p.list != nil && p.list.form != nil { // an item's edit only
			p.list.form, p.list.err = nil, ""
			p.id = nextGen() // its late answers are dropped
			return m, nil, true
		}
		return m.closePopup()
	case "ctrl+c":
		m, cmd, _ := m.closePopup()
		next, cmd2, _ := m.leave()
		return next, tea.Batch(cmd, cmd2), true
	}
	if p.list != nil && p.list.form == nil {
		return m.listKey(msg.String())
	}
	cmd := p.update(msg)
	next, cmd2 := m.afterPopup()
	return next, tea.Batch(cmd, cmd2, next.resizePopup()), true
}

// resizePopup sizes the open popup's form again: a group's height follows
// its content (a description evaluated late, a field shown or hidden), and
// huh measures it only on a WindowSizeMsg.
func (m settingsModel) resizePopup() tea.Cmd {
	if m.popup == nil {
		return nil
	}
	return m.popup.resize(m)
}

// closePopup cancels the popup: nothing is staged, and the roles grid
// shows the draft's roles again at once (a candidate's answer still on its
// way is dropped).
func (m settingsModel) closePopup() (settingsModel, tea.Cmd, bool) {
	live := m.popup != nil && m.popup.live && m.previewCand != ""
	m.popup = nil
	if !live {
		return m, nil, true
	}
	m.preview, m.previewCand, m.previewErr = m.draftRoles, "", nil
	if m.rolesOf != m.draftGen { // the draft's own answer had been overtaken by the candidate's
		cmd := m.previewCmd("")
		return m, cmd, true
	}
	m.previewGen = nextGen()
	return m, nil, true
}

// afterPopup acts on the form's state after a message: a completed form
// stages its values, a moved cursor previews its candidate.
func (m settingsModel) afterPopup() (settingsModel, tea.Cmd) {
	p := m.popup
	if p == nil {
		return m, nil
	}
	if p.list != nil {
		if p.list.form != nil && p.list.form.State == huh.StateCompleted {
			return m.listItemDone()
		}
		return m, nil
	}
	if p.form == nil {
		return m, nil
	}
	switch p.form.State {
	case huh.StateCompleted:
		m.popup = nil
		return p.done(m, p)
	case huh.StateAborted:
		m.popup = nil
		return m, nil
	}
	if p.live {
		label, d := p.candidate(p)
		if k := draftKey(d); k != p.cand {
			p.cand = k
			cmd := m.previewCmd(label, d)
			return m, cmd
		}
	}
	return m, nil
}

// draftKey is a draft's fingerprint, to tell candidates apart.
func draftKey(d SettingsDraft) string {
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range d {
			if !yield(k) {
				return
			}
		}
	})
	var b strings.Builder
	for _, k := range keys {
		e := d[k]
		b.WriteString(k + "=" + e.Text + "|" + strings.Join(e.List, ",") + "|" + strconv.FormatBool(e.Default) + ";")
	}
	return b.String()
}

// currentEdit is what a setting holds now: its staged edit, else the file's.
func (m settingsModel) currentEdit(f SettingField) SettingsEdit {
	if e, ok := m.draft[f.Target]; ok {
		return e
	}
	if !f.Set {
		return SettingsEdit{Default: true}
	}
	return SettingsEdit{Text: f.Text, List: f.List}
}

// sameEdit reports whether a and b stage the same thing.
func sameEdit(a, b SettingsEdit) bool {
	if a.Default || b.Default {
		return a.Default == b.Default
	}
	return a.Text == b.Text && slices.Equal(a.List, b.List)
}

// stageIfChanged stages e for f unless f already holds it.
func (m settingsModel) stageIfChanged(f SettingField, e SettingsEdit) (settingsModel, bool) {
	if sameEdit(m.currentEdit(f), e) {
		return m, false
	}
	return m.stage(f.Target, e), true
}

// popupMaxWidth caps a popup's inner width.
const popupMaxWidth = 56

// popupWidthMax is popupWidth up to most cells.
func (m settingsModel) popupWidthMax(widest, most int) int {
	room := max(m.listWidth()-4, 24)
	return min(max(widest+2, 30), most, room)
}

// selectChoice is the Select value that shows an edit: the default option,
// one of the values offered, or "other" (the text goes to the next field).
func selectChoice(e SettingsEdit, values []string) (choice, other string) {
	switch {
	case e.Default:
		return optDefault, ""
	case slices.Contains(values, e.Text):
		return e.Text, ""
	}
	return optOther, e.Text
}

// choiceEdit is the edit a Select's choice (and the "other" text) makes.
func choiceEdit(choice, other string) SettingsEdit {
	switch choice {
	case optDefault:
		return SettingsEdit{Default: true}
	case optOther:
		if strings.TrimSpace(other) == "" {
			return SettingsEdit{Default: true}
		}
		return SettingsEdit{Text: strings.TrimSpace(other)}
	}
	return SettingsEdit{Text: choice}
}

// selectOptions are a Select's options for f: the default first, then the
// members, "(saved)" on the one in the file, and "other…" when withOther.
// A saved value off the list is offered too (as "other: …" when withOther),
// so a Select never starts on another value than the file's. values are the
// values offered, for selectChoice.
func selectOptions(f SettingField, opts []string, withOther bool) (out []huh.Option[string], values []string) {
	saved := f.Text
	if !f.Set {
		saved = optDefault
	}
	tag := func(v, label string) string {
		if v == saved {
			return label + " (saved)"
		}
		return label
	}
	// The default is always a choice, the first: an unset key starts on
	// it, so enter keeps what applies, and a set one can go back to it.
	label := "default"
	switch d := strings.TrimSpace(f.Default); {
	case d != "" && slices.Contains(opts, d):
		label = "default (" + d + ")"
	case d != "":
		label = d
	}
	out = append(out, huh.NewOption(tag(optDefault, cleanText(label)), optDefault))
	values = slices.Clone(opts)
	for _, o := range opts {
		out = append(out, huh.NewOption(tag(o, cleanText(o)), o))
	}
	offList := f.Set && f.Text != "" && !slices.Contains(opts, f.Text)
	switch {
	case withOther && offList:
		out = append(out, huh.NewOption("other: "+cleanText(f.Text)+" (saved)", optOther))
	case withOther:
		out = append(out, huh.NewOption("other…", optOther))
	case offList:
		out = append(out, huh.NewOption(cleanText(f.Text)+" (saved)", f.Text))
		values = append(values, f.Text)
	}
	return out, values
}

// typeOptions are the members a Select offers for f.
func typeOptions(f SettingField) []string {
	if f.Type == SettingBool {
		return []string{"true", "false"}
	}
	return f.Options
}

// widestOption is the widest label among options.
func widestOption(opts []huh.Option[string]) int {
	w := 0
	for _, o := range opts {
		w = max(w, len([]rune(o.Key)))
	}
	return w
}

// openFieldPopup opens the editor for one setting, by its type.
func (m settingsModel) openFieldPopup(f SettingField) (settingsModel, tea.Cmd, bool) {
	switch f.Type {
	case SettingLogins, SettingRepos, SettingList:
		return m.openListPopup(f)
	case SettingRoles:
		return m.openRolesPopup(f)
	case SettingEnum, SettingBool, SettingEffort, SettingModel:
		if len(typeOptions(f)) > 0 || f.Type == SettingBool {
			return m.openSelectPopup(f)
		}
	}
	return m.openInputPopup(f)
}

// openSelectPopup edits an enum, a bool, an effort or a model: a Select
// whose hovered option previews the roles when the setting changes them; a
// model may also be typed ("other…").
func (m settingsModel) openSelectPopup(f SettingField) (settingsModel, tea.Cmd, bool) {
	p := m.newPopup(popField)
	p.target = f.Target
	withOther := f.Type == SettingModel
	opts, values := selectOptions(f, typeOptions(f), withOther)
	p.choice, p.other = selectChoice(m.currentEdit(f), values)
	sel := huh.NewSelect[string]().Title(f.Label).Options(opts...).Value(&p.choice)
	var describe func(string) string
	if len(f.Notes) > 0 {
		describe = func(choice string) string { return optionNote(f, choice) }
	}
	groups := []*huh.Group{huh.NewGroup(p.bind(sel, &p.choice, describe))}
	if withOther {
		in := huh.NewInput().Title(f.Label).Description("the model's name").Value(&p.other).Validate(fieldCheck(f, true))
		groups = append(groups, huh.NewGroup(in).WithHideFunc(func() bool { return p.choice != optOther }))
	}
	p.form = p.newForm(groups...)
	p.size(m, max(widestOption(opts), len([]rune(f.Label))), 0)
	p.live = m.gridSection(f.Section) || f.Role != ""
	p.candidate = func(p *setPopup) (string, SettingsDraft) {
		e := choiceEdit(p.choice, p.other)
		label := e.Text
		if e.Default {
			label = f.Default
		}
		return "preview: " + label, SettingsDraft{f.Target: e}
	}
	p.done = func(m settingsModel, p *setPopup) (settingsModel, tea.Cmd) {
		return m.finishField(f, choiceEdit(p.choice, p.other))
	}
	p.hints = []hint{{"j/k", "choose"}, {"enter", "stage"}, {"esc", "cancel"}}
	if p.live {
		p.hints[0].desc = "choose (the roles preview follows)"
	}
	return m.open(p)
}

// optionNote is the description of a Select's hovered option.
func optionNote(f SettingField, choice string) string {
	switch choice {
	case optDefault:
		return "the default: " + f.Default
	case optOther:
		return "type another value next"
	}
	if n := f.Notes[choice]; n != "" {
		return cleanText(choice + ": " + n)
	}
	return f.Description
}

// fieldCheck is an Input's check for f: empty means the default (unless
// required), then the setting's own check, else a check by its type.
func fieldCheck(f SettingField, required bool) func(string) error {
	return func(s string) error {
		s = strings.TrimSpace(s)
		if s == "" {
			if required {
				return errEmpty
			}
			return nil
		}
		if f.Validate != nil {
			return f.Validate(s)
		}
		return typeCheck(f.Type, s)
	}
}

// typeCheck is the check of a value by its type alone, for a setting the
// source gave no check.
func typeCheck(t SettingType, s string) error {
	switch t {
	case SettingDuration:
		if _, err := time.ParseDuration(s); err != nil {
			return errDuration
		}
	case SettingInt:
		if _, err := strconv.Atoi(s); err != nil {
			return errInt
		}
	case SettingPercent:
		if n, err := strconv.Atoi(s); err != nil || n < 0 || n > 100 {
			return errPercent
		}
	case SettingText:
		if len([]rune(s)) > popupTextLimit {
			return errTooLong
		}
	}
	return nil
}

// openInputPopup edits a duration, a number, a percent or a text: an Input
// with the setting's check; empty puts the default back.
func (m settingsModel) openInputPopup(f SettingField) (settingsModel, tea.Cmd, bool) {
	p := m.newPopup(popField)
	p.target = f.Target
	if e := m.currentEdit(f); !e.Default {
		p.text = e.Text
	}
	desc := f.Description
	if f.Default != "" {
		desc = joinNonEmpty(desc, "empty = the default ("+f.Default+")")
	}
	in := huh.NewInput().Title(f.Label).Description(desc).Placeholder(f.Default).Value(&p.text).Validate(fieldCheck(f, false))
	if f.Type == SettingText {
		in = in.CharLimit(popupTextLimit)
	}
	p.form = p.newForm(huh.NewGroup(in))
	p.size(m, max(len([]rune(f.Label)), 40), 0)
	p.done = func(m settingsModel, p *setPopup) (settingsModel, tea.Cmd) {
		e := SettingsEdit{Text: strings.TrimSpace(p.text)}
		if e.Text == "" {
			e = SettingsEdit{Default: true}
		}
		return m.finishField(f, e)
	}
	p.hints = []hint{{"enter", "stage (empty = default)"}, {"esc", "cancel"}}
	return m.open(p)
}

// openRolesPopup edits a list of roles: a MultiSelect with the setting's
// own check (one judge, for a watch's roles).
func (m settingsModel) openRolesPopup(f SettingField) (settingsModel, tea.Cmd, bool) {
	p := m.newPopup(popField)
	p.target = f.Target
	cur := m.currentEdit(f)
	selected := cur.List
	if cur.Default {
		selected = nil
	}
	var opts []huh.Option[string]
	for _, o := range f.Options {
		label := cleanText(o)
		if n := f.Notes[o]; n != "" {
			label += " (" + cleanText(n) + ")"
		}
		opts = append(opts, huh.NewOption(label, o).Selected(slices.Contains(selected, o)))
	}
	p.multi = slices.Clone(selected)
	ms := huh.NewMultiSelect[string]().Title(f.Label).Description(f.Description).Options(opts...).Value(&p.multi).
		Validate(func(list []string) error {
			if f.ValidateList != nil && len(list) > 0 {
				return f.ValidateList(list)
			}
			return nil
		})
	p.form = p.newForm(huh.NewGroup(ms))
	p.size(m, max(widestOption(opts)+4, len([]rune(f.Label))), 0)
	p.live = m.gridSection(f.Section)
	p.candidate = func(p *setPopup) (string, SettingsDraft) {
		return "preview: " + f.Label, SettingsDraft{f.Target: rolesEdit(p.multi)}
	}
	p.done = func(m settingsModel, p *setPopup) (settingsModel, tea.Cmd) {
		return m.finishField(f, rolesEdit(p.multi))
	}
	p.hints = []hint{{"space", "toggle"}, {"enter", "stage (none = default)"}, {"esc", "cancel"}}
	return m.open(p)
}

// rolesEdit is the edit of a roles list: none selected is the default.
func rolesEdit(list []string) SettingsEdit {
	if len(list) == 0 {
		return SettingsEdit{Default: true}
	}
	return SettingsEdit{List: slices.Clone(list)}
}

// finishField stages a popup's edit of f and previews the roles.
func (m settingsModel) finishField(f SettingField, e SettingsEdit) (settingsModel, tea.Cmd) {
	m, changed := m.stageIfChanged(f, e)
	if !changed {
		cmd := m.previewCmd("")
		return m.warn(f.Label + ": no change"), cmd
	}
	cmd := m.previewCmd("")
	return m, cmd
}

// openRolePopup edits a role of the grid: its model and efforts in its
// spend level's table, or its base [[role]] model.
func (m settingsModel) openRolePopup(role string) (settingsModel, tea.Cmd, bool) {
	if m.rolesOf != m.draftGen { // the draft's roles are on their way: its level could still change
		return m.warn("wait: resolving the roles of the draft"), nil, true
	}
	level := m.roleLevel(role)
	tm, hasTM := m.roleField(role, level, "model")
	te, hasTE := m.roleField(role, level, "effort")
	ta, hasTA := m.roleField(role, level, "rereview_effort")
	bm, hasBM := m.roleField(role, "", "model")
	table := level != "" && (hasTM || hasTE || hasTA)
	if !table && !hasBM {
		return m.warn(role + ": nothing to edit here"), nil, true
	}
	p := m.newPopup(popRole)
	p.role, p.level, p.where = role, level, "table"
	if !table {
		p.where = "base"
	}
	var groups []*huh.Group
	widest := len([]rune(role)) + 10
	if table && hasBM {
		groups = append(groups, huh.NewGroup(p.bind(huh.NewSelect[string]().Title(role+" · edit").Options(
			huh.NewOption("[spend."+level+"] table: model and efforts", "table"),
			huh.NewOption("[[role]] "+role+": base model", "base"),
		).Value(&p.where), &p.where, nil)))
		widest = max(widest, len([]rune("[[role]] "+role+": base model"))+2)
	}
	if table {
		// One field a page: the model (another one typed when "other…"),
		// then the first effort, then the re-review effort.
		table := func() bool { return p.where != "table" }
		if hasTM {
			opts, values := selectOptions(tm, tm.Options, true)
			p.model, p.other = selectChoice(m.currentEdit(tm), values)
			groups = append(groups, huh.NewGroup(p.bind(huh.NewSelect[string]().Title(role+" · model while spend is "+level).
				Options(opts...).Value(&p.model), &p.model, nil)).WithHideFunc(table))
			in := huh.NewInput().Title(role + " · model while spend is " + level).Description("the model's name").Value(&p.other).Validate(fieldCheck(tm, true))
			groups = append(groups, huh.NewGroup(in).WithHideFunc(func() bool { return p.where != "table" || p.model != optOther }))
			widest = max(widest, widestOption(opts))
		}
		if hasTE {
			opts, values := selectOptions(te, te.Options, false)
			p.first, _ = selectChoice(m.currentEdit(te), values)
			groups = append(groups, huh.NewGroup(p.bind(huh.NewSelect[string]().Title(role+" · first effort while spend is "+level).
				Options(opts...).Value(&p.first), &p.first, nil)).WithHideFunc(table))
		}
		if hasTA {
			opts, values := selectOptions(ta, ta.Options, false)
			p.again, _ = selectChoice(m.currentEdit(ta), values)
			groups = append(groups, huh.NewGroup(p.bind(huh.NewSelect[string]().Title(role+" · re-review effort while spend is "+level).
				Options(opts...).Value(&p.again), &p.again, nil)).WithHideFunc(table))
		}
	}
	if hasBM {
		opts, values := selectOptions(bm, bm.Options, true)
		p.baseModel, p.baseOther = selectChoice(m.currentEdit(bm), values)
		groups = append(groups, huh.NewGroup(p.bind(huh.NewSelect[string]().Title(role+" · base model").Options(opts...).Value(&p.baseModel),
			&p.baseModel, nil)).WithHideFunc(func() bool { return p.where != "base" }))
		in := huh.NewInput().Title(role + " · base model").Description("the model's name").Value(&p.baseOther).Validate(fieldCheck(bm, true))
		groups = append(groups, huh.NewGroup(in).WithHideFunc(func() bool { return p.where != "base" || p.baseModel != optOther }))
		widest = max(widest, widestOption(opts))
	}
	p.form = p.newForm(groups...)
	p.size(m, widest, 0)
	p.live = true
	edits := func(p *setPopup) []roleEdit {
		var out []roleEdit
		if p.where == "base" {
			if hasBM {
				out = append(out, roleEdit{bm, choiceEdit(p.baseModel, p.baseOther)})
			}
			return out
		}
		if hasTM {
			out = append(out, roleEdit{tm, choiceEdit(p.model, p.other)})
		}
		if hasTE {
			out = append(out, roleEdit{te, choiceEdit(p.first, "")})
		}
		if hasTA {
			out = append(out, roleEdit{ta, choiceEdit(p.again, "")})
		}
		return out
	}
	p.candidate = func(p *setPopup) (string, SettingsDraft) {
		d := SettingsDraft{}
		for _, e := range edits(p) {
			d[e.f.Target] = e.e
		}
		return "preview: " + role, d
	}
	p.done = func(m settingsModel, p *setPopup) (settingsModel, tea.Cmd) {
		staged := 0
		for _, e := range edits(p) {
			var changed bool
			if m, changed = m.stageIfChanged(e.f, e.e); changed {
				staged++
			}
		}
		if staged == 0 {
			m = m.warn(role + ": no change")
		}
		cmd := m.previewCmd("")
		return m, cmd
	}
	p.hints = []hint{{"j/k", "choose (the roles preview follows)"}, {"enter", "next / stage"}, {"shift+tab", "back"}, {"esc", "cancel"}}
	return m.open(p)
}

type roleEdit struct {
	f SettingField
	e SettingsEdit
}

// openScopePicker opens the scope picker: Global, each watch by position
// with its owner and first repositories, each repository.
func (m settingsModel) openScopePicker() (settingsModel, tea.Cmd, bool) {
	if m.data == nil {
		return m, nil, true
	}
	p := m.newPopup(popScope)
	opts := []huh.Option[string]{huh.NewOption("Global: spend, caps, schedule, reviews", "")}
	for _, s := range m.data.Scopes {
		label := "repo " + cleanText(s.Name)
		if s.Watch {
			label = "watch " + strconv.Itoa(s.Index) + ": " + cleanText(s.Name)
			if len(s.Repos) > 0 {
				label += " · " + firstRepos(s.Repos, 2)
			}
		} else if s.NoBlock {
			label += " (no [[repo]] block yet)"
		}
		opts = append(opts, huh.NewOption(label, s.ID))
	}
	if cur, ok := m.currentScope(); ok {
		p.choice = cur.ID
	}
	p.form = p.newForm(huh.NewGroup(p.bind(huh.NewSelect[string]().Title("scope").Options(opts...).Value(&p.choice).
		Height(min(len(opts)+2, max(m.bodyHeight()-2, 4))), &p.choice, nil)))
	p.size(m, widestOption(opts)+len(cursorMark), 76)
	p.done = func(m settingsModel, p *setPopup) (settingsModel, tea.Cmd) {
		if p.choice == "" {
			if m.sectionKind(m.currentSection()) != "" || m.currentSection() == sectionRuntime {
				m.section = m.firstEditable()
				if m.lastGlob >= 0 {
					m.section = m.lastGlob
				}
			}
		} else {
			m.openScope(p.choice)
		}
		m.query, m.cursor, m.scroll = "", 0, 0
		return m, nil
	}
	p.hints = []hint{{"j/k", "choose"}, {"enter", "go"}, {"esc", "cancel"}}
	return m.open(p)
}

// firstRepos names the first n repositories, "+N" for the rest.
func firstRepos(repos []string, n int) string {
	if len(repos) <= n {
		return strings.Join(repos, ", ")
	}
	return strings.Join(repos[:n], ", ") + " +" + strconv.Itoa(len(repos)-n)
}

// openPausePopup opens the pause form: how long, then the reason.
func (m settingsModel) openPausePopup() (settingsModel, tea.Cmd, bool) {
	p := m.newPopup(popPause)
	how := huh.NewSelect[string]().Title("pause automation").Description("running rounds finish; nothing new starts").Options(
		huh.NewOption("until magnum resume", ""),
		huh.NewOption("for a while (--for)", "for"),
		huh.NewOption("until a time (--until)", "until"),
	).Value(&p.pauseHow)
	p.bind(how, &p.pauseHow, nil)
	forIn := huh.NewInput().Title("for").Placeholder("2h").Value(&p.pauseFor).Validate(func(s string) error {
		_, err := parsePauseFor(s)
		return err
	})
	untilIn := huh.NewInput().Title("until").Placeholder("15:30").Value(&p.pauseUntil).Validate(checkPauseUntil)
	reason := huh.NewInput().Title("reason").Description("shown in magnum status (optional)").Value(&p.pauseReason).CharLimit(popupTextLimit)
	p.form = p.newForm(
		huh.NewGroup(how),
		huh.NewGroup(forIn).WithHideFunc(func() bool { return p.pauseHow != "for" }),
		huh.NewGroup(untilIn).WithHideFunc(func() bool { return p.pauseHow != "until" }),
		huh.NewGroup(reason),
	)
	p.size(m, 40, 0)
	p.done = func(m settingsModel, p *setPopup) (settingsModel, tea.Cmd) {
		spec := PauseSpec{Reason: strings.TrimSpace(p.pauseReason)}
		switch p.pauseHow {
		case "for":
			spec.For, _ = parsePauseFor(p.pauseFor)
		case "until":
			spec.Until = strings.TrimSpace(p.pauseUntil)
		}
		return m.askPause(spec), nil
	}
	p.hints = []hint{{"enter", "next"}, {"shift+tab", "back"}, {"esc", "cancel"}}
	return m.open(p)
}
