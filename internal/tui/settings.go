package tui

// The settings screen: `magnum settings`, and `,` on the dashboard and the
// PR board (settings_host.go). A rail of sections, one line per setting, a
// three-line detail strip and, in a section that sets roles, the grid of
// the roles after a restart. Edits are staged in a draft and touch nothing
// until s shows their diff and y writes them; the Runtime section's pauses
// and resumes apply at once, each after its own y. Field editing happens in
// huh popups (settings_popup.go); the frame, the keys and the confirmations
// are the screen's own.

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/zhuravel/magnum/internal/textx"
)

// RunSettings shows the settings screen until the user leaves it or ctx
// ends. src may not be nil; rt may be (the Runtime section then says so).
func RunSettings(ctx context.Context, src SettingsSource, rt SettingsRuntime, opts SettingsOptions) error {
	if src == nil {
		return errors.New("tui: RunSettings needs a source")
	}
	_, err := runProgram(ctx, newSettingsModel(ctx, src, rt, opts, false))
	return err
}

// NewSettings is the settings screen as a model, for a program of the
// caller's: RunSettings runs it in one of its own. Leaving it quits that
// program (tea.Quit).
func NewSettings(ctx context.Context, src SettingsSource, rt SettingsRuntime, opts SettingsOptions) tea.Model {
	return newSettingsModel(ctx, src, rt, opts, false)
}

// sectionRuntime is the screen's own section: pauses and resumes, which
// apply at once and are never saved to the file.
const sectionRuntime = "Runtime"

// wideSettings is the narrowest screen that shows the roles grid beside the
// list; a narrower one lists the roles in the list and the roles that change
// under it.
const wideSettings = 125

type (
	settingsLoadedMsg struct {
		data *SettingsData
		err  error
	}
	settingsRuntimeMsg struct {
		state *RuntimeState
		err   error
		gen   int64 // the read it answers: an older one is dropped
	}
	settingsPreviewMsg struct {
		gen   int64
		roles []SettingsRole
		err   error
		draft int64 // the draft's generation it resolves
		cand  bool  // it resolves a popup's candidate, not the draft alone
	}
	settingsPlanMsg struct {
		plan *SettingsPlan
		err  error
		gen  int64 // the draft's generation it was made of
	}
	settingsSavedMsg struct {
		saved SettingsSaved
		err   error
	}
	// settingsAnswerMsg is the answer of a runtime action or a restart
	// request, for the footer; reload asks for the runtime state again.
	settingsAnswerMsg struct {
		text   string
		err    error
		reload bool
	}
	// settingsClosedMsg tells the screen that opened settings (the host)
	// that the user left it.
	settingsClosedMsg struct{}
	// settingsAsync carries the answer of one of the screen's commands with
	// the screen that sent it: a screen opened again with `,` drops the
	// answers of the one before.
	settingsAsync struct {
		from int64
		msg  tea.Msg
	}
)

// async tags the message fn returns with this screen.
func (m settingsModel) async(fn func() tea.Msg) tea.Cmd {
	from := m.id
	return func() tea.Msg { return settingsAsync{from: from, msg: fn()} }
}

// settingsMode is what the keys act on.
type settingsMode int

const (
	modeBrowse settingsMode = iota // the rail, the list and the grid
	modeSearch                     // typing a search
	modeSave                       // the save's diff
)

// settingsConfirm is a question waiting for y/N in the footer; only y runs
// yes, any other key runs no (nil: nothing).
type settingsConfirm struct {
	question string
	yes      func(m settingsModel) (settingsModel, tea.Cmd)
	no       func(m settingsModel) (settingsModel, tea.Cmd)
}

// settingsUndo is one staged edit u takes back: the target's edit before it
// (had false: none).
type settingsUndo struct {
	target string
	prev   SettingsEdit
	had    bool
}

type settingsModel struct {
	ctx    context.Context
	src    SettingsSource
	rt     SettingsRuntime
	opts   SettingsOptions
	hosted bool // opened from the dashboard or the board: leaving goes back there

	st     styles
	width  int
	height int

	data    *SettingsData
	read    bool // a load has read the file (data is set from the first answer, even a failure)
	loadErr error
	runtime *RuntimeState
	rtErr   error
	rtGen   int64 // the runtime read in flight; older answers are dropped
	// restoreAsked: the restore of opts.Restore was asked for (once).
	restoreAsked bool
	// keepDraft: the next load re-reads a file that changed under the
	// draft, keeping the edits that still apply (and saying so).
	keepDraft bool
	// reloading: a read of the file is on its way (after a save, a re-read,
	// ctrl+r); the keys that change the draft wait for it, so no edit is
	// staged against rows it replaces.
	reloading bool

	draft   SettingsDraft
	undo    []settingsUndo
	invalid map[string]string // a staged target the loader refused, with its words

	sections []string
	section  int
	watchAt  int // the watch the watch sections edit (index into the watch scopes)
	repoAt   int // the repository the repository sections edit
	lastGlob int // the global section c's Global goes back to
	cursor   int // the selected row of rows()
	scroll   int

	// preview is the roles after a restart on the draft (plus the open
	// popup's candidate); previewGen drops answers to older drafts.
	preview     []SettingsRole
	previewErr  error
	previewGen  int64
	previewCand string // the popup's candidate the preview shows ("" = the draft)
	// draftRoles are the roles of the draft alone (never a candidate's),
	// which the role popup takes a role's level from; rolesOf is the
	// draft generation they resolve.
	draftRoles []SettingsRole
	rolesOf    int64
	lastStaged string // the target staged last, which a refused preview marks

	popup *setPopup

	mode    settingsMode
	search  textinput.Model
	query   string // the search in effect ("" = none)
	help    bool
	helpOff int

	plan     *SettingsPlan
	notes    bool // dated notes on the changed lines
	planning bool
	saveOff  int // the diff's scroll

	confirm *settingsConfirm
	flash   string
	flashOK bool // the flash is good news (green), else a warning
	busy    string

	cache *frameCache[settingsFrameKey]
	gen   int64
	id    int64 // this screen, in its commands' answers
	// draftGen counts the draft's changes; a plan answers the draft it was
	// made of, and one older than the draft is dropped.
	draftGen int64
}

type settingsFrameKey struct {
	gen           int64
	width, height int
	dark          bool
}

func newSettingsModel(ctx context.Context, src SettingsSource, rt SettingsRuntime, opts SettingsOptions, hosted bool) settingsModel {
	in := textinput.New()
	in.Prompt = "/ "
	in.Placeholder = "label or key; * for the settings your file sets or the draft changes"
	in.CharLimit = 80
	return settingsModel{
		ctx: ctx, src: src, rt: rt, opts: opts, hosted: hosted,
		st: defaultStyles, draft: SettingsDraft{}, invalid: map[string]string{},
		search: in, notes: true, lastGlob: -1,
		cache: &frameCache[settingsFrameKey]{}, gen: nextGen(), id: nextGen(),
	}
}

func (m settingsModel) Init() tea.Cmd {
	return tea.Batch(tea.RequestBackgroundColor, m.loadCmd(), m.runtimeRead(m.rtGen))
}

func (m settingsModel) loadCmd() tea.Cmd {
	ctx, src := m.ctx, m.src
	return m.async(func() tea.Msg {
		d, err := src.Load(ctx)
		return settingsLoadedMsg{data: d, err: err}
	})
}

// runtimeCmd reads the runtime state again; an answer to an older read is
// dropped, so a slow one never overwrites a newer state.
func (m *settingsModel) runtimeCmd() tea.Cmd {
	m.rtGen++
	return m.runtimeRead(m.rtGen)
}

func (m settingsModel) runtimeRead(gen int64) tea.Cmd {
	if m.rt == nil {
		return nil
	}
	ctx, rt := m.ctx, m.rt
	return m.async(func() tea.Msg {
		s, err := rt.Runtime(ctx)
		return settingsRuntimeMsg{state: &s, err: err, gen: gen}
	})
}

func (m settingsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) { return m.step(msg) }

// step is Update in the model's own type, for the host.
func (m settingsModel) step(msg tea.Msg) (settingsModel, tea.Cmd) {
	next, cmd, changed := m.update(msg)
	if changed {
		next.gen = nextGen()
		next.fixCursor()
		next.fixScroll()
	}
	return next, cmd
}

// update handles msg; changed is false for a message the screen does not
// show (the frame cache keeps its frame).
func (m settingsModel) update(msg tea.Msg) (settingsModel, tea.Cmd, bool) {
	if a, ok := msg.(settingsAsync); ok {
		if a.from != m.id {
			return m, nil, false
		}
		msg = a.msg
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.popup != nil {
			return m, m.popup.resize(m), true
		}
		return m, nil, true
	case tea.BackgroundColorMsg:
		m.st = newStyles(msg.IsDark())
		if m.popup != nil {
			m.popup.restyle(m.st)
			return m, m.popup.resize(m), true // huh redraws a group only on an update
		}
		return m, nil, true
	case popupMsg: // one of a popup's commands answered
		if m.popup == nil || msg.id != m.popup.id {
			return m, nil, false // a closed popup's: it must not reach the next one
		}
		cmd := m.popup.update(msg.msg)
		m, cmd2 := m.afterPopup()
		return m, tea.Batch(cmd, cmd2, m.resizePopup()), true
	case settingsLoadedMsg:
		m = m.loaded(msg)
		cmd := m.previewCmd("")
		if !m.restoreAsked && m.opts.Restore > 0 { // even over a file that does not load: a restore may mend it
			m.restoreAsked = true
			cmd = tea.Batch(cmd, m.restoreCmd(m.opts.Restore))
		}
		return m, cmd, true
	case settingsRuntimeMsg:
		if msg.gen != m.rtGen {
			return m, nil, false
		}
		if msg.err != nil {
			m.rtErr = msg.err
		} else {
			m.runtime, m.rtErr = sanitizeRuntime(msg.state), nil
		}
		return m, nil, true
	case settingsPreviewMsg:
		if msg.gen != m.previewGen {
			return m, nil, false
		}
		m.previewErr = msg.err
		if msg.err == nil {
			m.preview = msg.roles
			if !msg.cand && msg.draft == m.draftGen {
				m.draftRoles, m.rolesOf = msg.roles, msg.draft
			}
			if !msg.cand && m.lastStaged != "" {
				delete(m.invalid, m.lastStaged)
			}
		} else if !msg.cand && m.lastStaged != "" {
			m.invalid[m.lastStaged] = errLine(msg.err)
		}
		return m, nil, true
	case settingsPlanMsg:
		m.planning, m.busy = false, ""
		if msg.gen != m.draftGen {
			return m.warn("the draft changed while it was checked: s checks it again"), nil, true
		}
		if errors.Is(msg.err, ErrSettingsChanged) {
			return m.askReread(msg.err), nil, true
		}
		if msg.err != nil {
			if len(m.draft) > 0 {
				m.markInvalid(msg.err)
			}
			m = m.warn("not saved: " + errLine(msg.err))
			return m, nil, true
		}
		m.plan, m.mode, m.saveOff = sanitizePlan(msg.plan), modeSave, 0
		return m, nil, true
	case settingsSavedMsg:
		return m.saved(msg)
	case settingsAnswerMsg:
		m.busy = ""
		if msg.err != nil {
			m = m.warn(errLine(msg.err))
		} else {
			m = m.good(cleanText(msg.text))
		}
		if msg.reload {
			cmd := m.runtimeCmd()
			return m, cmd, true
		}
		return m, nil, true
	case tea.KeyPressMsg:
		return m.key(msg)
	case tea.PasteMsg: // into the open Input or the search being typed, never a question
		switch {
		case m.confirm != nil:
			return m, nil, false
		case m.popup != nil:
			cmd := m.popup.update(msg)
			return m, cmd, true
		case m.mode == modeSearch:
			var cmd tea.Cmd
			m.search, cmd = m.search.Update(msg)
			m.query = strings.TrimSpace(m.search.Value())
			m.cursor, m.scroll = 0, 0
			return m, cmd, true
		}
		return m, nil, false
	case tea.MouseMsg:
		// No mouse mode on this screen: nothing a click could start or
		// confirm.
		return m, nil, false
	}
	if !fieldMsg(msg) { // another screen's timers, when opened over it, or a closed popup's
		return m, nil, false
	}
	if m.popup != nil { // huh's own messages (field updates, cursor blink)
		cmd := m.popup.update(msg)
		m, cmd2 := m.afterPopup()
		return m, tea.Batch(cmd, cmd2, m.resizePopup()), true
	}
	if m.mode == modeSearch {
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(msg)
		return m, cmd, true
	}
	return m, nil, false
}

// fieldMsg reports whether msg is a bubble's own message (a cursor
// blinking, which names its cursor), which the popup or the search input
// takes. huh's own reach a popup only tagged with it (popupMsg); anything
// else is not the screen's.
func fieldMsg(msg tea.Msg) bool {
	t := reflect.TypeOf(msg)
	if t == nil {
		return false
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return strings.HasPrefix(t.PkgPath(), "charm.land/bubbles/")
}

// loaded takes a load: the sections, the scope the screen opened on, and
// the first row selected. A file the source cannot read leaves the Runtime
// section, whose pauses the runtime reads on its own.
func (m settingsModel) loaded(msg settingsLoadedMsg) settingsModel {
	m.reloading = false
	if msg.err != nil {
		m.loadErr = msg.err
		if m.data == nil {
			m.data, m.sections, m.section = &SettingsData{}, []string{sectionRuntime}, 0
		}
		return m
	}
	first := !m.read
	m.read = true
	var before []SettingsScope // the scopes the staged edits were made on
	if m.data != nil {
		before = m.data.Scopes
	}
	keep := m.keepDraft
	m.keepDraft = false
	m.data, m.loadErr = sanitizeSettings(msg.data), nil
	m.sections = append([]string{sectionRuntime}, slices.DeleteFunc(slices.Clone(m.data.Sections), func(s string) bool {
		return s == sectionRuntime
	})...)
	for _, f := range m.data.Fields { // a section the source did not list
		if !slices.Contains(m.sections, f.Section) {
			m.sections = append(m.sections, f.Section)
		}
	}
	// A reload drops the edits of settings the file no longer has (a watch
	// or a repository removed by hand meanwhile), and of a watch whose
	// position another watch now holds.
	dropped := 0
	for t := range m.draft {
		f, ok := m.fieldByTarget(t)
		if !ok || movedWatch(before, m.data.Scopes, f.Scope) {
			delete(m.draft, t)
			dropped++
		}
	}
	if dropped > 0 {
		m.draftGen++
		m.undo = nil
	}
	if keep {
		m = m.good("read the file again: kept " + textx.Count(len(m.draft), "staged edit", "staged edits") +
			prefixed(", dropped ", map[bool]string{true: strconv.Itoa(dropped) + " of settings it no longer has"}[dropped > 0]) +
			"; s checks the draft again")
	}
	m.preview = m.data.Roles
	m.draftRoles, m.rolesOf = m.data.Roles, -1
	if len(m.draft) == 0 {
		m.rolesOf = m.draftGen // the saved roles are the empty draft's
	}
	if first {
		m.section = m.firstEditable()
		m.openScope(m.opts.Scope)
	}
	m.section = min(m.section, len(m.sections)-1)
	return m
}

// movedWatch reports whether the watch at scope's position is another
// watch in after than in before (the file's watches were reordered or
// replaced): another owner, or the same owner with other repositories, as
// when two watches of one owner swap places.
func movedWatch(before, after []SettingsScope, scope string) bool {
	if !strings.HasPrefix(scope, "watch:") || before == nil {
		return false
	}
	identity := func(list []SettingsScope) string {
		for _, s := range list {
			if s.ID == scope {
				return strings.ToLower(s.Name + "\x00" + strings.Join(s.Repos, "\x00"))
			}
		}
		return ""
	}
	return identity(before) != identity(after)
}

// askReread asks, after a save refused because the file changed on disk,
// whether to read it again and keep the staged edits.
func (m settingsModel) askReread(err error) settingsModel {
	n := len(m.draft)
	m.mode, m.plan = modeBrowse, nil
	m = m.warn("not saved: " + errLine(err))
	m.confirm = &settingsConfirm{
		question: "The file changed on disk. Read it again and keep the " + textx.Count(n, "staged edit", "staged edits") + "?",
		yes: func(m settingsModel) (settingsModel, tea.Cmd) {
			m.keepDraft, m.reloading = true, true
			return m, m.loadCmd()
		},
		no: func(m settingsModel) (settingsModel, tea.Cmd) {
			return m.warn("not saved: the file changed on disk (ctrl+r reads it again once nothing is staged)"), nil
		},
	}
	return m
}

// firstEditable is the first section after Runtime, where the screen opens.
func (m settingsModel) firstEditable() int { return min(1, len(m.sections)-1) }

// openScope moves to the section and instance of a scope id ("watch:2",
// "repo:owner/name"); "" or an unknown id leaves the screen where it is.
func (m *settingsModel) openScope(id string) {
	if id == "" || m.data == nil {
		return
	}
	for i, s := range m.data.Scopes {
		if !strings.EqualFold(s.ID, id) {
			continue
		}
		if s.Watch {
			m.watchAt = m.scopeIndex(true, i)
		} else {
			m.repoAt = m.scopeIndex(false, i)
		}
		for j, sec := range m.sections {
			if m.sectionKind(sec) == scopeKind(s.Watch) {
				m.section = j
				break
			}
		}
		return
	}
	m.flash, m.flashOK = "no "+id+" in your config", false
}

// scopeIndex is the position of data.Scopes[i] among the scopes of its kind.
func (m settingsModel) scopeIndex(watch bool, i int) int {
	n := 0
	for _, s := range m.data.Scopes[:i] {
		if s.Watch == watch {
			n++
		}
	}
	return n
}

// key handles a key press in the screen's current state.
func (m settingsModel) key(msg tea.KeyPressMsg) (settingsModel, tea.Cmd, bool) {
	k := msg.String()
	if m.confirm != nil {
		c := m.confirm
		m.confirm = nil
		switch {
		case confirms(k) && c.yes != nil:
			next, cmd := c.yes(m)
			return next, cmd, true
		case c.no != nil:
			next, cmd := c.no(m)
			return next, cmd, true
		}
		return m.cancelled(), nil, true
	}
	if m.popup != nil {
		return m.popupKey(msg)
	}
	if m.help {
		switch k {
		case "?", "esc", "q", "enter":
			m.help, m.helpOff = false, 0
		default:
			m.helpOff = scrollKey(k, m.helpOff, max(m.bodyHeight()-3, 1))
		}
		return m, nil, true
	}
	switch m.mode {
	case modeSearch:
		return m.searchKey(msg)
	case modeSave:
		return m.saveKey(k)
	}
	return m.browseKey(k)
}

// cancelled is the footer after a question was answered with anything but y.
func (m settingsModel) cancelled() settingsModel {
	m.flash, m.flashOK = "cancelled", false
	return m
}

// browseKey handles a key on the rail, the list and the grid.
func (m settingsModel) browseKey(k string) (settingsModel, tea.Cmd, bool) {
	m.flash = ""
	if m.data == nil {
		switch k {
		case "esc", "q", "ctrl+c":
			return m.leave()
		}
		return m, nil, true
	}
	rows := m.rows()
	if (m.busy != "" || m.reloading) && draftKeys[k] {
		return m.warn("wait: " + orEmpty(m.busy, "reading the file")), nil, true
	}
	switch k {
	case "esc":
		if m.query != "" {
			m.query, m.cursor, m.scroll = "", 0, 0
			return m, nil, true
		}
		return m.leave()
	case "q", "ctrl+c":
		return m.leave()
	case "j", "down":
		m.cursor = nextSelectable(rows, m.cursor, 1)
	case "k", "up":
		m.cursor = nextSelectable(rows, m.cursor, -1)
	case "pgdown", "ctrl+d":
		m.cursor = moveSelectable(rows, m.cursor, max(m.bodyHeight()/2, 1))
	case "pgup", "ctrl+u":
		m.cursor = moveSelectable(rows, m.cursor, -max(m.bodyHeight()/2, 1))
	case "g", "home":
		m.cursor = nextSelectable(rows, -1, 1)
	case "G", "end":
		m.cursor = nextSelectable(rows, len(rows), -1)
	case "l", "right":
		m.cursor = m.intoGrid(rows, true)
	case "h", "left":
		m.cursor = m.intoGrid(rows, false)
	case "tab":
		m = m.moveSection(1)
	case "shift+tab":
		m = m.moveSection(-1)
	case "[":
		return m.moveScope(-1), nil, true
	case "]":
		return m.moveScope(1), nil, true
	case "c":
		return m.openScopePicker()
	case "/":
		m.mode = modeSearch
		m.search.SetValue(m.query)
		m.search.CursorEnd()
		return m, m.search.Focus(), true
	case "?":
		m.help, m.helpOff = true, 0
	case "enter":
		return m.edit(rows)
	case "d", "backspace", "delete":
		return m.toDefault(rows)
	case "u":
		return m.undoLast()
	case "s":
		return m.startSave()
	case "p", "r", "R", "x":
		return m.runtimeKey(k, rows)
	case "ctrl+r", "f5":
		return m.refresh()
	}
	return m, nil, true
}

// refresh reads the pauses again, and the file when the draft is empty (a
// draft is planned against the file it was made on; the save refuses one
// that changed since).
func (m settingsModel) refresh() (settingsModel, tea.Cmd, bool) {
	cmds := []tea.Cmd{m.runtimeCmd()}
	if len(m.draft) == 0 {
		cmds = append(cmds, m.loadCmd())
		m.reloading = true
		return m, tea.Batch(cmds...), true
	}
	return m.good("reading the pauses again (the file waits: changes are staged)"), tea.Batch(cmds...), true
}

// draftKeys are the keys that change the draft, open a popup or act: they
// wait while the draft is checked or saved, or a runtime request is out.
var draftKeys = map[string]bool{"enter": true, "d": true, "backspace": true, "delete": true, "u": true, "s": true,
	"c": true, "p": true, "r": true, "R": true, "x": true}

// leave closes the screen, asking first when the draft holds edits.
func (m settingsModel) leave() (settingsModel, tea.Cmd, bool) {
	if n := len(m.draft); n > 0 {
		m.confirm = &settingsConfirm{
			question: "Discard " + textx.Count(n, "unsaved change", "unsaved changes") + "?",
			yes: func(m settingsModel) (settingsModel, tea.Cmd) {
				m.draft, m.undo = SettingsDraft{}, nil
				return m, m.closeCmd()
			},
		}
		return m, nil, true
	}
	return m, m.closeCmd(), true
}

// closeCmd ends the screen: back to the screen that opened it, or quit.
func (m settingsModel) closeCmd() tea.Cmd {
	if m.hosted {
		return func() tea.Msg { return settingsClosedMsg{} }
	}
	return tea.Quit
}

// moveSection selects the next (or previous) section, its first row, and
// leaves a search.
func (m settingsModel) moveSection(d int) settingsModel {
	if len(m.sections) == 0 {
		return m
	}
	m.section = (m.section + d + len(m.sections)) % len(m.sections)
	m.query, m.cursor, m.scroll = "", 0, 0
	if m.sectionKind(m.sections[m.section]) == "" && m.sections[m.section] != sectionRuntime {
		m.lastGlob = m.section
	}
	return m
}

// moveScope moves a watch or repository section to the previous or next
// instance.
func (m settingsModel) moveScope(d int) settingsModel {
	kind := m.sectionKind(m.currentSection())
	n := len(m.scopesOf(kind == "watch"))
	switch {
	case kind == "":
		return m.warn("[ and ] move between watches (in Watches) and repositories (in Repositories)")
	case n == 0:
		return m
	case kind == "watch":
		m.watchAt = (m.watchAt + d + n) % n
	default:
		m.repoAt = (m.repoAt + d + n) % n
	}
	m.cursor, m.scroll = 0, 0
	return m
}

// searchKey handles a key while the search is typed: enter keeps it, esc
// clears it.
func (m settingsModel) searchKey(msg tea.KeyPressMsg) (settingsModel, tea.Cmd, bool) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.mode, m.query = modeBrowse, ""
		m.search.Blur()
		m.cursor, m.scroll = 0, 0
		return m, nil, true
	case "enter":
		m.mode = modeBrowse
		m.search.Blur()
		m.cursor = nextSelectable(m.rows(), -1, 1)
		return m, nil, true
	}
	var cmd tea.Cmd
	m.search, cmd = m.search.Update(msg)
	m.query = strings.TrimSpace(m.search.Value())
	m.cursor, m.scroll = 0, 0
	return m, cmd, true
}

// edit opens the popup for the selected row.
func (m settingsModel) edit(rows []setRow) (settingsModel, tea.Cmd, bool) {
	r, ok := rowAt(rows, m.cursor)
	if !ok {
		return m, nil, true
	}
	switch r.kind {
	case rowField:
		return m.openFieldPopup(m.data.Fields[r.field])
	case rowRole:
		return m.openRolePopup(r.role)
	case rowRuntime:
		return m.warn("p pauses automation, r resumes the selected row, R resumes every agent kind, x restarts a kind's live sessions"), nil, true
	}
	return m, nil, true
}

// toDefault stages "back to the default" for the selected setting (or the
// selected role's cells at its spend level).
func (m settingsModel) toDefault(rows []setRow) (settingsModel, tea.Cmd, bool) {
	r, ok := rowAt(rows, m.cursor)
	if !ok {
		return m, nil, true
	}
	var targets []string
	switch r.kind {
	case rowField:
		targets = []string{m.data.Fields[r.field].Target}
	case rowRole:
		if m.rolesOf != m.draftGen { // the level whose cells go back could still change
			return m.warn("wait: resolving the roles of the draft"), nil, true
		}
		for _, f := range m.roleFields(r.role, m.roleLevel(r.role)) {
			targets = append(targets, f.Target)
		}
	default:
		return m, nil, true
	}
	staged := 0
	for _, t := range targets {
		f, _ := m.fieldByTarget(t)
		if !f.Set {
			if _, had := m.draft[t]; had { // an unsaved value of a key the file lacks: drop it
				m = m.stage(t, SettingsEdit{Default: true})
				staged++
			}
			continue
		}
		m = m.stage(t, SettingsEdit{Default: true})
		staged++
	}
	if staged == 0 {
		return m.warn("already the default"), nil, true
	}
	cmd := m.previewCmd("")
	return m, cmd, true
}

// stage records e for target in the draft (an edit back to the saved
// state drops the target from it), with an undo entry.
func (m settingsModel) stage(target string, e SettingsEdit) settingsModel {
	prev, had := m.draft[target]
	m.undo = append(m.undo, settingsUndo{target: target, prev: prev, had: had})
	m.draftGen++
	m.gen = nextGen()
	m.draft = maps.Clone(m.draft)
	if f, ok := m.fieldByTarget(target); ok && sameAsSaved(f, e) {
		delete(m.draft, target)
	} else {
		m.draft[target] = e
	}
	delete(m.invalid, target)
	m.lastStaged = target
	return m
}

// sameAsSaved reports whether e leaves f as the file has it.
func sameAsSaved(f SettingField, e SettingsEdit) bool {
	switch {
	case e.Default:
		return !f.Set
	case !f.Set:
		return false
	case f.Type.IsList():
		return slices.Equal(f.List, e.List)
	}
	return f.Text == e.Text
}

// undoLast takes the last staged edit back.
func (m settingsModel) undoLast() (settingsModel, tea.Cmd, bool) {
	if len(m.undo) == 0 {
		return m.warn("nothing to undo"), nil, true
	}
	u := m.undo[len(m.undo)-1]
	m.undo = m.undo[:len(m.undo)-1]
	m.draftGen++
	m.draft = maps.Clone(m.draft)
	if u.had {
		m.draft[u.target] = u.prev
	} else {
		delete(m.draft, u.target)
	}
	delete(m.invalid, u.target)
	f, _ := m.fieldByTarget(u.target)
	m.lastStaged = ""
	m = m.good("undone: " + f.Label)
	cmd := m.previewCmd("")
	return m, cmd, true
}

// startSave checks the draft with magnum's loader and asks for the diff.
func (m settingsModel) startSave() (settingsModel, tea.Cmd, bool) {
	switch {
	case len(m.draft) == 0:
		return m.warn("nothing to save"), nil, true
	case m.planning:
		return m, nil, true
	}
	m.planning, m.busy = true, "checking the draft with magnum's loader"
	return m, m.planCmd(m.notes), true
}

// restoreCmd asks for the plan that puts the n-th kept version back; it
// opens on its diff like a save.
func (m *settingsModel) restoreCmd(n int) tea.Cmd {
	m.planning, m.busy = true, "reading kept version "+strconv.Itoa(n)
	ctx, src, gen := m.ctx, m.src, m.draftGen
	return m.async(func() tea.Msg {
		p, err := src.Restore(ctx, n)
		return settingsPlanMsg{plan: p, err: err, gen: gen}
	})
}

func (m settingsModel) planCmd(notes bool) tea.Cmd {
	ctx, src, d, gen := m.ctx, m.src, maps.Clone(m.draft), m.draftGen
	return m.async(func() tea.Msg {
		p, err := src.Plan(ctx, d, notes)
		return settingsPlanMsg{plan: p, err: err, gen: gen}
	})
}

// saveKey handles a key on the diff: n turns the dated notes on or off, s
// asks to write, esc goes back to editing.
func (m settingsModel) saveKey(k string) (settingsModel, tea.Cmd, bool) {
	if m.busy != "" { // a commit or a new diff is on its way
		return m, nil, true
	}
	m.flash = ""
	switch k {
	case "esc", "q":
		m.mode, m.plan = modeBrowse, nil
		return m, nil, true
	case "ctrl+c":
		m.mode, m.plan = modeBrowse, nil
		return m.leave()
	case "n":
		if m.plan.Restore > 0 {
			return m.warn("a restore puts the kept file back as it was: no notes"), nil, true
		}
		m.notes = !m.notes
		m.planning, m.busy = true, "rebuilding the diff"
		return m, m.planCmd(m.notes), true
	case "s":
		if m.planning {
			return m, nil, true
		}
		p := m.plan
		m.confirm = &settingsConfirm{
			question: "Save " + textx.Count(p.Count, "change", "changes") + " to " + orEmpty(m.data.File, "the config file") + "?",
			yes: func(m settingsModel) (settingsModel, tea.Cmd) {
				m.busy = "saving"
				ctx, src := m.ctx, m.src
				return m, m.async(func() tea.Msg {
					s, err := src.Commit(ctx, p)
					return settingsSavedMsg{saved: s, err: err}
				})
			},
		}
		return m, nil, true
	default:
		m.saveOff = scrollKey(k, m.saveOff, max(m.bodyHeight()-2, 1))
	}
	return m, nil, true
}

// saved takes a commit's result: the draft is gone, the file reloaded, and
// a save that applies after a restart asks whether to restart.
func (m settingsModel) saved(msg settingsSavedMsg) (settingsModel, tea.Cmd, bool) {
	m.busy = ""
	if errors.Is(msg.err, ErrSettingsChanged) {
		return m.askReread(msg.err), nil, true
	}
	if msg.err != nil {
		m.mode, m.plan = modeBrowse, nil
		return m.warn("not saved: " + errLine(msg.err)), nil, true
	}
	n := 0
	if m.plan != nil {
		n = m.plan.Count
	}
	m.mode, m.plan = modeBrowse, nil
	m.draft, m.undo, m.invalid, m.lastStaged = SettingsDraft{}, nil, map[string]string{}, ""
	text := "saved " + textx.Count(n, "change", "changes")
	if msg.saved.History != "" {
		text += "; the previous file is kept as " + msg.saved.History
	}
	m = m.good(text)
	m.reloading = true // the saved file's rows: nothing is staged against the old ones meanwhile
	if msg.saved.AfterRestart {
		s := msg.saved
		m.confirm = &settingsConfirm{
			question: "Restart the daemon when it is idle to apply?",
			yes: func(m settingsModel) (settingsModel, tea.Cmd) {
				m.busy = "asking the daemon to restart"
				ctx, src := m.ctx, m.src
				return m, m.async(func() tea.Msg {
					text, err := src.RequestRestart(ctx, s)
					return settingsAnswerMsg{text: text, err: err}
				})
			},
			no: func(m settingsModel) (settingsModel, tea.Cmd) {
				return m.warn(text + "; restart pending: the daemon applies it at its next restart"), nil
			},
		}
	}
	return m, m.loadCmd(), true
}

// previewCmd resolves the roles for the draft plus cand (a popup's live
// candidate; nil = the draft alone), dropping any older answer.
func (m *settingsModel) previewCmd(label string, cand ...SettingsDraft) tea.Cmd {
	if m.data == nil {
		return nil
	}
	m.previewGen = nextGen()
	m.previewCand = label
	d := maps.Clone(m.draft)
	for _, c := range cand {
		maps.Copy(d, c)
	}
	gen, draft, isCand, ctx, src := m.previewGen, m.draftGen, len(cand) > 0, m.ctx, m.src
	return m.async(func() tea.Msg {
		roles, err := src.Preview(ctx, d)
		return settingsPreviewMsg{gen: gen, roles: roles, err: err, draft: draft, cand: isCand}
	})
}

// markInvalid marks the staged settings a loader message names (by a whole
// key path), so their rows show ✗ and the strip the words.
func (m *settingsModel) markInvalid(err error) {
	text := strings.ToLower(err.Error())
	for t := range m.draft {
		f, ok := m.fieldByTarget(t)
		if !ok {
			continue
		}
		// Whole key paths: a last word could name another setting. A
		// refusal that names no staged setting marks none; the footer says it.
		for _, name := range []string{f.Path, f.Key} {
			if name != "" && strings.Contains(text, strings.ToLower(name)) {
				m.invalid[t] = errLine(err)
				break
			}
		}
	}
}

// warn and good set the footer's flash.
func (m settingsModel) warn(s string) settingsModel {
	m.flash, m.flashOK = s, false
	return m
}

func (m settingsModel) good(s string) settingsModel {
	m.flash, m.flashOK = s, true
	return m
}

// fieldByTarget is the setting a draft key names.
func (m settingsModel) fieldByTarget(t string) (SettingField, bool) {
	if m.data == nil {
		return SettingField{}, false
	}
	for _, f := range m.data.Fields {
		if f.Target == t {
			return f, true
		}
	}
	return SettingField{}, false
}

// sanitizeSettings cleans every text a load hands the screen of terminal
// controls; the data is the user's own file, but a stray escape must not
// paint the screen.
func sanitizeSettings(d *SettingsData) *SettingsData {
	if d == nil {
		return &SettingsData{}
	}
	c := *d
	c.File, c.HistoryDir = cleanText(c.File), cleanText(c.HistoryDir)
	c.Fields = slices.Clone(d.Fields)
	for i := range c.Fields {
		f := &c.Fields[i]
		f.Label, f.Key, f.Description, f.Default = cleanText(f.Label), cleanText(f.Key), cleanText(f.Description), cleanText(f.Default)
		// Text, List and Options stay as the file has them: they are what an
		// untouched value stages back. They are cleaned where they are shown.
		f.Value, f.From = cleanText(f.Value), cleanText(f.From)
	}
	c.Scopes = slices.Clone(d.Scopes)
	for i := range c.Scopes {
		c.Scopes[i].Name = cleanText(c.Scopes[i].Name)
		c.Scopes[i].Repos = cleanList(c.Scopes[i].Repos)
	}
	c.Roles = cleanRoles(d.Roles)
	c.Warnings = cleanList(d.Warnings)
	return &c
}

// sanitizePlan and sanitizeRuntime clean what a plan and the runtime state
// show, as sanitizeSettings does a load.
func sanitizePlan(p *SettingsPlan) *SettingsPlan {
	if p == nil {
		return &SettingsPlan{}
	}
	c := *p
	c.Hunks = slices.Clone(p.Hunks)
	for i := range c.Hunks {
		h := &c.Hunks[i]
		h.Table, h.Removed, h.Added = cleanText(h.Table), keepSpaces(h.Removed), keepSpaces(h.Added)
	}
	c.Warnings = cleanList(p.Warnings)
	return &c
}

// keepSpaces cleans the file's lines of terminal controls and keeps their
// spacing (aligned columns, the gap before a comment), which cleanText
// would fold.
func keepSpaces(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
				return ' '
			}
			return r
		}, ansi.Strip(l))
	}
	return out
}

func sanitizeRuntime(s *RuntimeState) *RuntimeState {
	c := *s
	clean := func(in []RuntimeEntry) []RuntimeEntry {
		out := slices.Clone(in)
		for i := range out {
			e := &out[i]
			e.Name, e.Label, e.State, e.Detail = cleanText(e.Name), cleanText(e.Label), cleanText(e.State), cleanText(e.Detail)
		}
		return out
	}
	c.Automation = clean([]RuntimeEntry{s.Automation})[0]
	c.Kinds, c.Watches = clean(s.Kinds), clean(s.Watches)
	return &c
}

func cleanList(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = cleanText(s)
	}
	return out
}

func cleanRoles(in []SettingsRole) []SettingsRole {
	out := slices.Clone(in)
	for i := range out {
		r := &out[i]
		r.Role, r.Kind, r.Model, r.First, r.Again, r.Level = cleanText(r.Role), cleanText(r.Kind), cleanText(r.Model),
			cleanText(r.First), cleanText(r.Again), cleanText(r.Level)
	}
	return out
}
