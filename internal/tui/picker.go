package tui

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/paginator"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// PickEntry is one PR the picker lists.
type PickEntry struct {
	Ref    string // what actions receive: owner/name#N (repo#N labels work too)
	Title  string
	Author string
	State  string // magnum state, with flags ("reviewed,pinned")
	Age    string // since the last activity ("3h")
	URL    string
	Pinned bool         // ctrl+p means unpin when true
	Review *ReviewFacts // for the y/N question before a review; nil when unknown
	// GHState is GitHub's state of the PR: OPEN, CLOSED or MERGED; "" when
	// unknown. A MERGED one is asked about as a post-merge review.
	GHState string
}

// PickAction is what the user chose to do with the picked PR.
type PickAction int

// The picker's actions; PickActionCancel is the zero value.
const (
	PickActionCancel    PickAction = iota
	PickActionReview               // enter
	PickActionAgain                // ctrl+r: review again
	PickActionFresh                // ctrl+f: review with new sessions
	PickActionOpen                 // ctrl+g: open the judge pane
	PickActionBrowser              // ctrl+o: open the PR URL
	PickActionTogglePin            // ctrl+p: pin, or unpin when Entry.Pinned
	PickActionRelease              // ctrl+x
)

var pickActionNames = [...]string{"cancel", "review", "again", "fresh", "open", "browser", "toggle-pin", "release"}

func (a PickAction) String() string {
	if a >= 0 && int(a) < len(pickActionNames) {
		return pickActionNames[a]
	}
	return "PickAction(" + strconv.Itoa(int(a)) + ")"
}

// PickOutcome is the picker's result. Entry is nil when the action is
// Cancel, or when the query matched no entry but reads as a PR reference
// (URL, owner/repo#N, repo#N, #N or N): the caller resolves Query then.
type PickOutcome struct {
	Action PickAction
	Entry  *PickEntry
	Query  string // the filter text as the user left it
}

// PickerOptions tune the picker.
type PickerOptions struct {
	Query string           // initial filter; a PR URL or reference narrows to that PR
	Title string           // default "magnum pick"
	Now   func() time.Time // clock for "reviewed 2h ago" in the y/N question; default time.Now
	// Lookup resolves a typed reference the list lacks to a PR magnum knows,
	// e.g. a merged one, so the y/N question can say what it is; nil means none.
	Lookup func(query string) (PickEntry, bool)
}

// RunPicker lets the user filter entries and pick one with an action. It
// returns a Cancel outcome (and nil error) when the user cancels or ctx
// ends.
func RunPicker(ctx context.Context, entries []PickEntry, opts PickerOptions) (PickOutcome, error) {
	m := newPickerModel(entries, opts)
	final, err := runProgram(ctx, m)
	if pm, ok := final.(pickerModel); ok && pm.done && err == nil && ctx.Err() == nil {
		return pm.outcome, nil
	}
	q := strings.TrimSpace(opts.Query)
	if pm, ok := final.(pickerModel); ok {
		q = pm.query()
	}
	return PickOutcome{Action: PickActionCancel, Query: q}, err
}

// pickItem adapts a PickEntry to list.Item.
type pickItem struct{ e PickEntry }

func (i pickItem) FilterValue() string {
	return i.e.Ref + " " + i.e.Title + " " + i.e.Author + " " + i.e.State
}

// pickDelegate renders an entry on one line: ref, state, age, author, title.
type pickDelegate struct {
	st                          styles
	refW, stateW, ageW, authorW int
}

func newPickDelegate(entries []PickEntry, st styles) pickDelegate {
	d := pickDelegate{st: st}
	for _, e := range entries {
		d.refW = max(d.refW, lipgloss.Width(e.Ref))
		d.stateW = max(d.stateW, lipgloss.Width(e.State))
		d.ageW = max(d.ageW, lipgloss.Width(e.Age))
		d.authorW = max(d.authorW, lipgloss.Width(e.Author))
	}
	d.refW, d.stateW, d.ageW, d.authorW = min(d.refW, 28), min(d.stateW, 22), min(d.ageW, 5), min(d.authorW, 16)
	return d
}

func (pickDelegate) Height() int                         { return 1 }
func (pickDelegate) Spacing() int                        { return 0 }
func (pickDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }

func (d pickDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	it, ok := item.(pickItem)
	if !ok {
		return
	}
	e := it.e
	cells := []string{fit(e.Ref, d.refW), fit(e.State, d.stateW), fit(e.Age, d.ageW), fit(e.Author, d.authorW), oneLine(e.Title)}
	line := strings.TrimRight(strings.Join(cells, colGap), " ")
	room := max(m.Width()-len(cursorMark), 1)
	line = truncate(line, room)
	if index == m.Index() {
		fmt.Fprint(w, d.st.Accent.Render(cursorMark)+d.st.Selected.Render(line))
		return
	}
	fmt.Fprint(w, noMark+line)
}

type pickerModel struct {
	entries []PickEntry
	list    list.Model
	opts    PickerOptions
	st      styles

	width, height int
	note          string       // footer note, e.g. "no PR matches"
	confirm       *pickConfirm // a review awaiting y/N
	// typed is the PR magnum knows that a typed reference the list lacks
	// names (Lookup, asked when the filter changes); nil when none.
	typed *PickEntry

	done    bool
	outcome PickOutcome
}

const (
	pickPreviewMinWidth = 100 // narrower terminals hide the preview
	pickPrompt          = "PR> "
)

func newPickerModel(entries []PickEntry, opts PickerOptions) pickerModel {
	if opts.Title == "" {
		opts.Title = "magnum pick"
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	entries = append([]PickEntry(nil), entries...)
	items := make([]list.Item, len(entries))
	for i, e := range entries {
		items[i] = pickItem{e}
	}
	l := list.New(items, newPickDelegate(entries, defaultStyles), 80, 20)
	l.Title = opts.Title
	l.SetShowHelp(false)
	l.DisableQuitKeybindings()
	l.SetStatusBarItemName("PR", "PRs")
	l.Paginator.Type = paginator.Arabic
	l.Filter = pickFilter(entries)
	l.FilterInput.Prompt = pickPrompt

	m := pickerModel{entries: entries, list: l, opts: opts}
	m.applyStyles(defaultStyles)
	m.setFilter(strings.TrimSpace(opts.Query))
	m.resize(80, 24)
	return m
}

// applyStyles re-skins the list, its filter input and the rows with st.
func (m *pickerModel) applyStyles(st styles) {
	m.st = st
	in := textinput.DefaultStyles(st.dark)
	in.Focused.Prompt, in.Blurred.Prompt = st.Accent, st.Dim
	in.Cursor.Color = st.Accent.GetForeground()

	s := list.DefaultStyles(st.dark)
	s.TitleBar = lipgloss.NewStyle().Padding(0, 0, 1, 0)
	s.Title = st.Title
	s.Spinner = st.Accent
	s.Filter = in
	s.StatusBar = st.Dim.Padding(0, 0, 1, 2)
	s.StatusEmpty = st.Dim
	s.StatusBarActiveFilter = st.Accent
	s.StatusBarFilterCount = st.Dim
	s.NoItems = st.Dim
	s.ArabicPagination = st.Dim
	s.PaginationStyle = lipgloss.NewStyle().PaddingLeft(2)
	s.ActivePaginationDot = st.Accent.SetString("•")
	s.InactivePaginationDot = st.Dim.SetString("•")
	s.DividerDot = st.Dim.SetString(" • ")
	m.list.Styles = s
	m.list.FilterInput.SetStyles(in)
	m.list.SetDelegate(newPickDelegate(m.entries, st))
}

func (m pickerModel) Init() tea.Cmd { return tea.Batch(tea.RequestBackgroundColor, textinput.Blink) }

// query is the filter text as typed, trimmed.
func (m pickerModel) query() string { return strings.TrimSpace(m.list.FilterInput.Value()) }

// setFilter filters synchronously (the list's own filtering is async) and
// leaves the filter input focused, so typing always edits the filter.
func (m *pickerModel) setFilter(v string) {
	pos := m.list.FilterInput.Position()
	if m.list.FilterInput.Value() != v {
		pos = len([]rune(v))
	}
	m.list.SetFilterText(v)
	m.list.SetFilterState(list.Filtering)
	m.list.FilterInput.SetCursor(pos)
	m.lookupTyped()
}

// lookupTyped resolves the filter through Lookup when it is a reference the
// list does not match (a merged PR, say), so the preview, the footer and the
// question describe the PR magnum knows; m.typed is nil otherwise.
func (m *pickerModel) lookupTyped() {
	m.typed = nil
	q := m.query()
	if m.opts.Lookup == nil || len(m.list.VisibleItems()) != 0 {
		return
	}
	if _, ok := parsePickRef(q); !ok {
		return
	}
	if e, ok := m.opts.Lookup(q); ok {
		m.typed = &e
	}
}

func (m *pickerModel) resize(w, h int) {
	m.width, m.height = w, h
	listW := w
	if w >= pickPreviewMinWidth {
		listW = w * 55 / 100
	}
	listH := max(h-1-len(m.footerLines()), 3) // title line above, footer below
	m.list.SetSize(listW, listH)
	m.list.FilterInput.SetWidth(max(listW-len(pickPrompt)-1, 8))
}

func (m pickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		return m, nil
	case tea.BackgroundColorMsg:
		m.applyStyles(newStyles(msg.IsDark()))
		return m, nil
	case list.FilterMatchesMsg:
		return m, nil // filtering is synchronous; a late async result would be stale
	case tea.KeyPressMsg:
		return m.updateKey(msg)
	case tea.PasteMsg:
		if m.confirm != nil {
			m.confirm = nil // a paste cancels the question, like any key
			return m, nil
		}
	}
	return m.editFilter(msg) // pastes and cursor blinks
}

// pickKeyActions maps keys to actions.
var pickKeyActions = map[string]PickAction{
	"enter": PickActionReview, "ctrl+r": PickActionAgain, "ctrl+f": PickActionFresh, "ctrl+g": PickActionOpen,
	"ctrl+o": PickActionBrowser, "ctrl+p": PickActionTogglePin, "ctrl+x": PickActionRelease,
}

func (m pickerModel) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if c := m.confirm; c != nil && k != "ctrl+c" {
		m.confirm = nil // any key but yes cancels and restores the footer
		if confirms(k) {
			return m.finish(c.action, c.entry)
		}
		return m, nil
	}
	if a, ok := pickKeyActions[k]; ok {
		return m.choose(a)
	}
	switch k {
	case "ctrl+c", "esc": // not q: a query may start with it ("quinn")
		return m.finish(PickActionCancel, nil)
	case "up", "ctrl+k":
		m.list.CursorUp()
		return m, nil
	case "down", "ctrl+j", "ctrl+n":
		m.list.CursorDown()
		return m, nil
	case "pgup":
		m.list.PrevPage()
		return m, nil
	case "pgdown":
		m.list.NextPage()
		return m, nil
	}
	return m.editFilter(msg)
}

// editFilter hands msg to the filter input and refilters when it changed.
func (m pickerModel) editFilter(msg tea.Msg) (tea.Model, tea.Cmd) {
	before := m.list.FilterInput.Value()
	var cmd tea.Cmd
	m.list.FilterInput, cmd = m.list.FilterInput.Update(msg)
	if v := m.list.FilterInput.Value(); v != before {
		m.note = ""
		m.setFilter(v)
	}
	return m, cmd
}

// pickConfirm is a review the picker asks about before it finishes.
type pickConfirm struct {
	action   PickAction
	entry    *PickEntry // nil for a typed reference the list lacks, even when Lookup knows it
	question string
}

// choose acts on the highlighted entry, or on the typed reference when
// nothing matches. The review actions ask y/N first.
func (m pickerModel) choose(a PickAction) (tea.Model, tea.Cmd) {
	var e *PickEntry // what the outcome carries: the highlighted entry, nil for a typed reference
	q := m.query()
	if it, ok := m.list.SelectedItem().(pickItem); ok {
		ent := it.e
		e = &ent
	} else if _, ok := parsePickRef(q); !ok {
		switch {
		case q == "" && len(m.entries) == 0:
			m.note = "magnum lists no open PRs yet: type a PR URL or reference"
		default:
			m.note = fmt.Sprintf("no PR matches %q: type a URL, owner/repo#N, repo#N or N", q)
		}
		return m, nil
	}
	asked := e // the PR the question is about: a typed reference Lookup knows is asked about as that PR
	if e == nil && m.typed != nil {
		asked = m.typed
	}
	if question := pickQuestion(a, asked, q, m.opts.Now()); question != "" {
		m.confirm = &pickConfirm{action: a, entry: e, question: question}
		return m, nil
	}
	return m.finish(a, e)
}

// pickReviewOpts is the review variant action a asks for; false for the
// actions that do not start a review.
func pickReviewOpts(a PickAction) (ReviewOpts, bool) {
	switch a {
	case PickActionReview:
		return ReviewOpts{}, true
	case PickActionAgain:
		return ReviewOpts{Again: true}, true
	case PickActionFresh:
		return ReviewOpts{Fresh: true}, true
	}
	return ReviewOpts{}, false
}

// pickQuestion asks before a review of e, or of the typed reference query
// when e is nil; a PR GitHub merged gets the post-merge question. "" for the
// actions that do not start a review.
func pickQuestion(a PickAction, e *PickEntry, query string, now time.Time) string {
	o, review := pickReviewOpts(a)
	if !review {
		return ""
	}
	if e == nil {
		return reviewQuestion(query, o, "not in magnum's list")
	}
	if ghStateMerged(e.GHState) {
		return postMergeQuestion(e.Ref, o)
	}
	facts := reviewFacts(e.State, "", e.Review, now)
	if e.Age != "" && e.Age != "-" {
		facts = joinFacts(facts, "last activity "+e.Age+" ago")
	}
	return reviewQuestion(e.Ref, o, facts)
}

func (m pickerModel) finish(a PickAction, e *PickEntry) (tea.Model, tea.Cmd) {
	m.done = true
	m.outcome = PickOutcome{Action: a, Entry: e, Query: m.query()}
	if a == PickActionCancel {
		m.outcome.Entry = nil
	}
	return m, tea.Quit
}

func (m pickerModel) View() tea.View {
	if m.done {
		return newView("")
	}
	body := m.list.View()
	if m.width >= pickPreviewMinWidth {
		body = lipgloss.JoinHorizontal(lipgloss.Top, body, " ", m.preview(m.width-m.list.Width()-1, m.list.Height()))
	}
	title := truncate(m.st.Title.Render(m.opts.Title), m.width)
	return newView(title + "\n" + body + "\n" + strings.Join(m.footerLines(), "\n"))
}

// preview describes the highlighted entry in a box of w x h cells.
func (m pickerModel) preview(w, h int) string {
	box := m.st.Box.Width(w).Height(h).MaxHeight(h) // sizes include border and padding
	inner := max(w-4, 1)
	it, ok := m.list.SelectedItem().(pickItem)
	e := it.e
	switch {
	case !ok && m.typed != nil:
		e = *m.typed // a typed reference magnum knows but does not list
	case !ok:
		msg := "no PR selected"
		if q := m.query(); q != "" {
			if _, ref := parsePickRef(q); ref {
				msg = "not in magnum's list:\n" + q + "\n\nenter reviews it"
			}
		}
		return box.Render(m.st.Dim.Width(inner).Render(msg))
	}
	state := e.State
	if ghStateMerged(e.GHState) {
		state = joinFacts(state, "merged on GitHub")
	}
	field := func(name, v string) string {
		return m.st.Label.Render(fmt.Sprintf("%-7s", name)) + lipgloss.NewStyle().Width(max(inner-7, 1)).Render(orDash(v))
	}
	lines := []string{
		m.st.Title.Width(inner).Render(orDash(oneLine(e.Title))),
		"",
		field("ref", e.Ref),
		field("state", state),
		field("author", e.Author),
		field("age", e.Age),
		"",
		m.st.Accent.Width(inner).Render(orDash(e.URL)), // alone on its line: URLs are long
	}
	return box.Render(strings.Join(lines, "\n"))
}

func (m pickerModel) footerLines() []string {
	w := m.width
	if w <= 0 {
		w = 80
	}
	status := ""
	switch q := m.query(); {
	case m.confirm != nil:
		status = m.st.confirmLine(m.confirm.question, w)
	case m.note != "":
		status = m.st.Warn.Render(m.note)
	case len(m.list.VisibleItems()) == 0 && m.typed != nil && ghStateMerged(m.typed.GHState):
		status = m.st.Accent.Render("merged: enter asks for a post-merge review of " + m.typed.Ref)
	case len(m.list.VisibleItems()) == 0 && m.typed != nil:
		status = m.st.Accent.Render("enter reviews " + m.typed.Ref)
	case len(m.list.VisibleItems()) == 0 && q != "":
		if _, ok := parsePickRef(q); ok {
			status = m.st.Accent.Render("not in the list: enter reviews " + q)
		}
	}
	lines := m.st.wrapHints(w,
		hint{"enter", "review"}, hint{"^r", "re-review"}, hint{"^f", "fresh"}, hint{"^g", "open"},
		hint{"^o", "browser"}, hint{"^p", "pin/unpin"}, hint{"^x", "release"}, hint{"↑/↓", "move"}, hint{"esc", "cancel"})
	return append([]string{truncate(status, w)}, lines...)
}

// entryRef reads an entry's own reference from its Ref, preferring its URL
// when the Ref names no repository.
func entryRef(e PickEntry) (pickRef, bool) {
	r, ok := parsePickRef(e.Ref)
	if ok && r.repo != "" {
		return r, true
	}
	if u, uok := parsePickRef(e.URL); uok {
		return u, true
	}
	return r, ok
}

// matches reports whether the entry is the PR q names: same number, and
// same repository (and owner) wherever both sides say.
func (q pickRef) matches(e PickEntry) bool {
	r, ok := entryRef(e)
	if !ok || r.number != q.number {
		return false
	}
	if q.repo != "" && r.repo != "" && !strings.EqualFold(q.repo, r.repo) {
		return false
	}
	if q.owner != "" && r.owner != "" && !strings.EqualFold(q.owner, r.owner) {
		return false
	}
	return true
}

// pickFilter matches a typed PR reference exactly (so a pasted URL never
// fuzzily lands on a different PR) and anything else fuzzily.
func pickFilter(entries []PickEntry) list.FilterFunc {
	return func(term string, targets []string) []list.Rank {
		q, ok := parsePickRef(term)
		if !ok {
			return list.DefaultFilter(term, targets)
		}
		var ranks []list.Rank
		for i := range targets {
			if i < len(entries) && q.matches(entries[i]) {
				ranks = append(ranks, list.Rank{Index: i})
			}
		}
		return ranks
	}
}
