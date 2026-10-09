package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/zhuravel/magnum/internal/textx"
)

// CleanupPlan is what the cleanup screen reviews: the actions the planner
// proposes, the things it left alone, and whole-plan estimates.
type CleanupPlan struct {
	Actions []CleanupAction
	Skipped []CleanupSkip
	Totals  CleanupTotals // whole-plan estimates, shown for reference
}

// CleanupAction is one thing the plan would do.
type CleanupAction struct {
	ID      string // stable id the caller maps back to its own action
	Kind    string // release_slot, remove_slot, drop_db, reset_external, ...
	Subject string // owner/name#N, slot:<name>, slug:<slug>, path:<dir>
	Why     string
	Bytes   int64 // disk freed (0 = none or unknown)
	DBBytes int64 // MySQL space freed
	DBNames []string
	// NeedsTypedConfirm, when non-empty, is the exact text (for example a
	// slug) the user must type before the plan is applied.
	NeedsTypedConfirm string
}

// CleanupSkip is something the planner left alone, with the reason.
type CleanupSkip struct{ Subject, Reason string }

// CleanupTotals are whole-plan estimates, in bytes.
type CleanupTotals struct{ Disk, MySQL int64 }

// CleanupOutcome is what the user decided.
type CleanupOutcome struct {
	Apply       bool     // true only after the user confirmed (and typed every required text)
	SelectedIDs []string // in plan order; empty when cancelled
}

// RunCleanupPlan shows the plan full-screen so the user can pick actions,
// confirm them and type any required texts. It returns Apply false when the
// user cancelled or ctx ended.
func RunCleanupPlan(ctx context.Context, plan CleanupPlan) (CleanupOutcome, error) {
	final, err := runProgram(ctx, newCleanupModel(plan))
	if err != nil {
		return CleanupOutcome{}, err
	}
	m, ok := final.(cleanupModel)
	if !ok || ctx.Err() != nil {
		return CleanupOutcome{}, nil
	}
	return m.outcome, nil
}

type cleanupStep int

const (
	cleanupStepSelect cleanupStep = iota
	cleanupStepConfirm
)

const (
	cleanupSelectFixed = 7 // title, header, blank, totals, plan totals, note, hints
	cleanupMaxSkipped  = 5 // skipped entries listed before "… N more"
	cleanupRowPrefix   = 6 // cursor mark + "[x] "
)

// cleanupModel is the Bubble Tea model of the cleanup screen.
type cleanupModel struct {
	plan     CleanupPlan
	selected []bool
	cursor   int
	offset   int // first visible action row
	step     cleanupStep
	note     string // footer note on the select step

	// Confirm step: the distinct texts to type, in plan order.
	texts    []string
	typedIdx int
	input    textinput.Model
	mismatch string

	width, height int
	st            styles
	outcome       CleanupOutcome
	done          bool // decided: the program is quitting, the view is empty
}

func newCleanupModel(plan CleanupPlan) cleanupModel {
	sel := make([]bool, len(plan.Actions))
	for i := range sel {
		sel[i] = true
	}
	in := textinput.New()
	in.Prompt = "> "
	in.Focus()
	m := cleanupModel{plan: plan, selected: sel, input: in, width: 80, height: 24}
	m.applyStyles(defaultStyles)
	return m
}

// applyStyles switches the screen and its text input to st.
func (m *cleanupModel) applyStyles(st styles) {
	m.st = st
	in := textinput.DefaultStyles(st.dark)
	in.Focused.Prompt = st.Warn
	in.Cursor.Color = st.Accent.GetForeground()
	m.input.SetStyles(in)
}

// Init implements tea.Model.
func (m cleanupModel) Init() tea.Cmd { return tea.RequestBackgroundColor }

// Update implements tea.Model. Once the user decided (done), Bubble Tea may
// still hand it the keys read before it handles tea.Quit: they do nothing,
// so a y queued behind a cancel never applies the cancelled plan.
func (m cleanupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.done {
		return m, nil
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.SetWidth(max(10, m.width-len(m.input.Prompt)-2))
		m.keepCursorVisible()
		return m, nil
	case tea.BackgroundColorMsg:
		m.applyStyles(newStyles(msg.IsDark()))
		return m, nil
	case tea.KeyPressMsg:
		if m.step == cleanupStepConfirm {
			return m.updateConfirm(msg)
		}
		return m.updateSelect(msg)
	}
	if m.step == cleanupStepConfirm && m.typing() {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg) // cursor blink
		return m, cmd
	}
	return m, nil
}

func (m cleanupModel) cancel() (tea.Model, tea.Cmd) {
	m.outcome, m.done = CleanupOutcome{}, true
	return m, tea.Quit
}

func (m cleanupModel) updateSelect(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	m.note = ""
	n := len(m.plan.Actions)
	switch msg.String() {
	case "esc", "q", "ctrl+c":
		return m.cancel()
	case "j", "down":
		m.cursor = min(m.cursor+1, max(n-1, 0))
	case "k", "up":
		m.cursor = max(m.cursor-1, 0)
	case "pgdown":
		m.cursor = min(m.cursor+m.listHeight(), max(n-1, 0))
	case "pgup":
		m.cursor = max(m.cursor-m.listHeight(), 0)
	case "g", "home":
		m.cursor = 0
	case "G", "end":
		m.cursor = max(n-1, 0)
	case "space":
		if n > 0 {
			m.selected = slices.Clone(m.selected)
			m.selected[m.cursor] = !m.selected[m.cursor]
		}
	case "a":
		m.selected = slices.Repeat([]bool{true}, n)
	case "n":
		m.selected = make([]bool, n)
	case "enter":
		if m.selectedCount() == 0 {
			m.note = "select at least one action"
			return m, nil
		}
		return m.enterConfirm()
	}
	m.keepCursorVisible()
	return m, nil
}

// enterConfirm moves to the confirm step and, when typed texts are needed,
// focuses the text input.
func (m cleanupModel) enterConfirm() (tea.Model, tea.Cmd) {
	m.step = cleanupStepConfirm
	m.texts = m.texts[:0:0]
	for i, a := range m.plan.Actions {
		if m.selected[i] && a.NeedsTypedConfirm != "" && !slices.Contains(m.texts, a.NeedsTypedConfirm) {
			m.texts = append(m.texts, a.NeedsTypedConfirm)
		}
	}
	m.typedIdx, m.mismatch = 0, ""
	m.input.Reset()
	if len(m.texts) == 0 {
		return m, nil
	}
	return m, m.input.Focus()
}

// backToSelect returns to the select step, keeping the selection and
// forgetting any typed progress.
func (m cleanupModel) backToSelect() cleanupModel {
	m.step = cleanupStepSelect
	m.texts, m.typedIdx, m.mismatch, m.note = nil, 0, "", ""
	m.input.Reset()
	m.keepCursorVisible()
	return m
}

// typing reports whether the confirm step is asking for typed text.
func (m cleanupModel) typing() bool { return len(m.texts) > 0 }

func (m cleanupModel) updateConfirm(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m.cancel()
	}
	if !m.typing() {
		// Only y applies: every action starts selected, so a double enter
		// (or key autorepeat) must not apply the whole plan.
		switch strings.ToLower(msg.String()) {
		case "y":
			return m.finish()
		case "n", "esc":
			return m.backToSelect(), nil
		case "enter":
			m.note = "press y to apply"
		}
		return m, nil
	}
	switch msg.String() {
	case "esc":
		return m.backToSelect(), nil
	case "enter":
		if m.typedIdx >= len(m.texts) {
			return m, nil // every text is typed: finish has quit
		}
		want := m.texts[m.typedIdx]
		got := m.input.Value()
		if got != want && strings.TrimSpace(got) != want {
			m.mismatch = "does not match " + want
			m.input.Reset()
			return m, nil
		}
		m.typedIdx++
		m.mismatch = ""
		m.input.Reset()
		if m.typedIdx >= len(m.texts) {
			return m.finish()
		}
		return m, nil
	}
	m.mismatch = ""
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// finish records the confirmed outcome and quits. Bubble Tea renders the
// model once more before it quits, so done empties the view (the typed
// step would otherwise index past its last text).
func (m cleanupModel) finish() (tea.Model, tea.Cmd) {
	m.outcome, m.done = CleanupOutcome{Apply: true, SelectedIDs: m.selectedIDs()}, true
	return m, tea.Quit
}

func (m cleanupModel) selectedIDs() []string {
	var ids []string
	for i, a := range m.plan.Actions {
		if m.selected[i] {
			ids = append(ids, a.ID)
		}
	}
	return ids
}

func (m cleanupModel) selectedCount() int {
	n := 0
	for _, s := range m.selected {
		if s {
			n++
		}
	}
	return n
}

// layout sizes the select step: how many action rows fit and how many
// skipped entries are listed.
func (m cleanupModel) layout() (listH, skipShown int) {
	nSkip := len(m.plan.Skipped)
	skipShown = min(nSkip, cleanupMaxSkipped)
	want := min(len(m.plan.Actions), 3)
	for {
		block := 0
		if nSkip > 0 {
			block = 2 + skipShown // blank + heading + entries
			if nSkip > skipShown {
				block++ // "… N more"
			}
		}
		listH = m.height - cleanupSelectFixed - block
		if listH >= want || skipShown == 0 {
			break
		}
		skipShown--
	}
	return max(listH, 1), skipShown
}

func (m cleanupModel) listHeight() int {
	h, _ := m.layout()
	return min(h, max(len(m.plan.Actions), 1))
}

// keepCursorVisible scrolls the row window so the cursor is inside it.
func (m *cleanupModel) keepCursorVisible() {
	h := m.listHeight()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+h {
		m.offset = m.cursor - h + 1
	}
	m.offset = max(0, min(m.offset, max(len(m.plan.Actions)-h, 0)))
}

// View implements tea.Model.
func (m cleanupModel) View() tea.View { return newView(m.render()) }

func (m cleanupModel) render() string {
	if m.done {
		return ""
	}
	var lines []string
	if m.step == cleanupStepConfirm {
		lines = m.confirmLines()
		if m.height > 0 && len(lines) > m.height {
			// A short screen keeps the bottom: the prompt, the input
			// and the keys.
			lines = lines[len(lines)-m.height:]
		}
	} else {
		lines = m.selectLines()
	}
	for i, l := range lines {
		lines[i] = truncate(l, m.width)
	}
	return strings.Join(lines, "\n")
}

// selection sums what the selected actions free.
func (m cleanupModel) selection() (disk, mysql int64, dbs int) {
	seen := map[string]bool{}
	for i, a := range m.plan.Actions {
		if !m.selected[i] {
			continue
		}
		disk += a.Bytes
		mysql += a.DBBytes
		for _, n := range a.DBNames {
			seen[n] = true
		}
	}
	return disk, mysql, len(seen)
}

func (m cleanupModel) selectLines() []string {
	acts := m.plan.Actions
	listH, skipShown := m.layout()
	listH = min(listH, max(len(acts), 1))

	title := m.st.Title.Render("Cleanup plan")
	if len(acts) > listH {
		title += m.st.Dim.Render(fmt.Sprintf("  rows %d-%d of %d", m.offset+1, min(m.offset+listH, len(acts)), len(acts)))
	}
	lines := []string{title}

	cols, rows, typedCol := m.tableCells(false)
	widths := layoutColumns(cols, rows, m.width, cleanupRowPrefix)
	if typedCol >= 0 && widths[typedCol] < cleanupColumnWidth(rows, typedCol) {
		// Too narrow for "[confirm: slug]": keep the marker whole but short.
		cols, rows, typedCol = m.tableCells(true)
		widths = layoutColumns(cols, rows, m.width, cleanupRowPrefix)
	}
	lines = append(lines, strings.Repeat(" ", cleanupRowPrefix)+m.st.headerRow(cols, widths))
	if len(acts) == 0 {
		lines = append(lines, m.st.Dim.Render(noMark+"no actions in this plan"))
	}
	for i := m.offset; i < min(m.offset+listH, len(acts)); i++ {
		box := "[ ]"
		if m.selected[i] {
			box = "[x]"
		}
		mark := noMark
		if i == m.cursor {
			mark = cursorMark
		}
		line := mark + box + " " + renderRow(rows[i], widths)
		if i == m.cursor {
			line = m.st.Selected.Render(line)
		}
		lines = append(lines, line)
	}

	if n := len(m.plan.Skipped); n > 0 {
		lines = append(lines, "", m.st.Dim.Render(fmt.Sprintf("SKIPPED (%d)", n)))
		for _, s := range m.plan.Skipped[:skipShown] {
			lines = append(lines, m.st.Dim.Render("  "+s.Subject+": "+s.Reason))
		}
		if n > skipShown {
			lines = append(lines, m.st.Dim.Render(fmt.Sprintf("  … %d more", n-skipShown)))
		}
	}

	disk, mysql, dbs := m.selection()
	lines = append(lines, "",
		fmt.Sprintf("selected %d/%d · disk %s · mysql %s · %d %s",
			m.selectedCount(), len(acts), HumanBytes(disk), HumanBytes(mysql), dbs, textx.Plural(dbs, "database", "databases")),
		m.st.Dim.Render(fmt.Sprintf("whole plan: disk %s · mysql %s", HumanBytes(m.plan.Totals.Disk), HumanBytes(m.plan.Totals.MySQL))),
		cleanupStyled(m.st.Warn, m.note),
		m.st.hints(m.width,
			hint{"j/k", "move"}, hint{"space", "toggle"}, hint{"a/n", "all/none"},
			hint{"enter", "review"}, hint{"q", "cancel"}),
	)
	return lines
}

// tableCells builds the column set and every action's cells, and the index
// of the CONFIRM column (-1 when no action needs typed confirmation, so the
// column is left out). short swaps "[confirm: slug]" for "[confirm]".
func (m cleanupModel) tableCells(short bool) (cols []column, rows [][]string, typedCol int) {
	typedCol = -1
	if slices.ContainsFunc(m.plan.Actions, func(a CleanupAction) bool { return a.NeedsTypedConfirm != "" }) {
		typedCol = 4
	}
	cols = []column{
		{title: "KIND", min: 6, flex: true},
		{title: "SUBJECT", min: 10, flex: true},
		{title: "DISK", min: 4},
		{title: "MYSQL", min: 5},
	}
	if typedCol >= 0 {
		cols = append(cols, column{title: "CONFIRM", min: 7})
		if short {
			cols[typedCol].min = len("[confirm]")
		}
	}
	cols = append(cols, column{title: "WHY", min: 6, flex: true})

	rows = make([][]string, len(m.plan.Actions))
	for i, a := range m.plan.Actions {
		disk := "-"
		if a.Bytes > 0 {
			disk = HumanBytes(a.Bytes)
		}
		row := []string{a.Kind, a.Subject, disk, cleanupMySQLCell(a)}
		if typedCol >= 0 {
			marker := ""
			switch {
			case a.NeedsTypedConfirm == "":
			case short:
				marker = "[confirm]"
			default:
				marker = "[confirm: " + a.NeedsTypedConfirm + "]"
			}
			row = append(row, marker)
		}
		rows[i] = append(row, a.Why)
	}
	return cols, rows, typedCol
}

// cleanupColumnWidth is the widest cell of column c.
func cleanupColumnWidth(rows [][]string, c int) int {
	w := 0
	for _, r := range rows {
		w = max(w, ansi.StringWidth(r[c]))
	}
	return w
}

func cleanupMySQLCell(a CleanupAction) string {
	var parts []string
	if a.DBBytes > 0 {
		parts = append(parts, HumanBytes(a.DBBytes))
	}
	if n := len(a.DBNames); n > 0 {
		parts = append(parts, fmt.Sprintf("(%d %s)", n, textx.Plural(n, "db", "dbs")))
	}
	return orDash(strings.Join(parts, " "))
}

// cleanupDescribe is the confirm-step line for one action.
func cleanupDescribe(a CleanupAction) string {
	var parts []string
	if a.Bytes > 0 {
		parts = append(parts, "frees "+HumanBytes(a.Bytes))
	}
	if n := len(a.DBNames); n > 0 {
		s := fmt.Sprintf("drops %d %s", n, textx.Plural(n, "database", "databases"))
		if a.DBBytes > 0 {
			s += " (" + HumanBytes(a.DBBytes) + ")"
		}
		parts = append(parts, s+": "+strings.Join(a.DBNames, ", "))
	} else if a.DBBytes > 0 {
		parts = append(parts, "frees "+HumanBytes(a.DBBytes)+" of MySQL")
	}
	if len(parts) == 0 {
		parts = append(parts, orDash(a.Why))
	}
	line := a.Kind + " " + a.Subject + " — " + strings.Join(parts, ", ")
	if a.NeedsTypedConfirm != "" {
		line += " (type " + a.NeedsTypedConfirm + ")"
	}
	return line
}

func (m cleanupModel) confirmLines() []string {
	n := m.selectedCount()
	disk, mysql, dbs := m.selection()
	lines := []string{
		m.st.Title.Render(fmt.Sprintf("Apply %d action(s)?", n)) +
			m.st.Dim.Render(fmt.Sprintf("  disk %s · mysql %s · %d %s", HumanBytes(disk), HumanBytes(mysql), dbs, textx.Plural(dbs, "database", "databases"))),
		"",
	}

	var prompt []string
	fixed := 4 // title, blank, blank, hints
	if m.typing() {
		text := fmt.Sprintf("Type %s to confirm dropping its databases (cannot be undone):", m.texts[m.typedIdx])
		if len(m.texts) > 1 {
			text += fmt.Sprintf(" (%d of %d)", m.typedIdx+1, len(m.texts))
		}
		prompt = cleanupWrap(text, m.width)
		fixed = 4 + len(prompt) + 2 // + prompt, input, error line
	}

	var blocks [][]string
	for i, a := range m.plan.Actions {
		if !m.selected[i] {
			continue
		}
		var blk []string
		for j, l := range cleanupWrap(cleanupDescribe(a), m.width-4) {
			blk = append(blk, "  "+strings.Repeat("  ", min(j, 1))+l)
		}
		blocks = append(blocks, blk)
	}
	lines = append(lines, cleanupFitBlocks(m.st, blocks, max(m.height-fixed, 1))...)

	if !m.typing() {
		return append(lines, cleanupStyled(m.st.Warn, m.note),
			m.st.hints(m.width, hint{"y", "apply"}, hint{"n/esc", "back"}, hint{"ctrl+c", "cancel"}))
	}
	lines = append(lines, "")
	for _, l := range prompt {
		lines = append(lines, m.st.Warn.Render(l))
	}
	return append(lines,
		m.input.View(),
		cleanupStyled(m.st.Err, m.mismatch),
		m.st.hints(m.width, hint{"enter", "confirm"}, hint{"esc", "back"}, hint{"ctrl+c", "cancel"}),
	)
}

// cleanupFitBlocks flattens blocks of lines into at most room lines; when they do
// not all fit, whole trailing blocks are replaced by an "and N more" line.
func cleanupFitBlocks(st styles, blocks [][]string, room int) []string {
	total := 0
	for _, b := range blocks {
		total += len(b)
	}
	var out []string
	if total <= room {
		for _, b := range blocks {
			out = append(out, b...)
		}
		return out
	}
	budget := max(room-1, 1)
	shown := 0
	for _, b := range blocks {
		if len(out)+len(b) > budget {
			break
		}
		out = append(out, b...)
		shown++
	}
	if shown == 0 {
		out = append(out, blocks[0][:min(len(blocks[0]), budget)]...)
		shown = 1
	}
	return append(out, st.Dim.Render(fmt.Sprintf("  … and %d more", len(blocks)-shown)))
}

// cleanupWrap word-wraps plain text to width cells (0 = no limit).
func cleanupWrap(s string, width int) []string {
	if width <= 0 {
		return []string{s}
	}
	return strings.Split(ansi.Wrap(s, width, ""), "\n")
}

// cleanupStyled renders s with st, leaving an empty string empty.
func cleanupStyled(st lipgloss.Style, s string) string {
	if s == "" {
		return ""
	}
	return st.Render(s)
}
