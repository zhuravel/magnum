package tui

// The factory's home screen painter (spec 9.C): data in, frame out. It holds
// no state and reads no clock; the screen model (a later task) feeds it the
// rows of the factory registry and the key hints of the moment.
//
// A row is one line: cursor, mark, key, title, WAITS ON, YOUR MOVE, one
// column per staging server, WAITED. The widths come from the data. When the
// screen is too narrow the painter gives up, in this order, (1) the title's
// width down to 14 cells, (2) YOUR MOVE for its short form, (3) WAITS ON for
// its short form (both blockers stay), (4) the staging cells for their short
// forms, (5) the gap after the key; beyond the spec it then drops the staging
// columns, YOUR MOVE, and squeezes the title and WAITS ON. When the screen is
// wide enough for everything at its fullest (key split into JIRA and GITHUB,
// long blockers, the age of each staging cell) it draws that instead and
// shares the spare width out.

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// factoryFrame is everything the home screen draws.
type factoryFrame struct {
	Repo string
	Rows []FactoryRow // the painter groups them by Group, keeping their order in a group; Cursor indexes this slice
	// Blocking are the machine facts that stop work, drawn plain on the right
	// of the title ("memory full: new tasks wait"); BlockingLong are the same
	// facts for a wide screen, with their detail.
	Blocking, BlockingLong []string
	RuleChip               string // "1 rule to approve (L)", yellow
	Flash                  string // the status rule's message, green
	Hints                  []hint // the key hints, most useful first; the last one is kept longest
	Cursor                 int    // the selected row, an index of Rows; out of range = none
	Spinner                int    // the frame of the working marks

	// Decisions are the single answers owed across all tasks ("5 decisions"),
	// counted apart from the tasks that need the operator.
	Decisions int
	// Next is what the operator does next when nothing waits on them ("PR-12371's
	// questions or its diff, once its plan is written."), under "Nothing waits on you."
	Next string
	// Digest is what happened while the operator was away; nil: no digest.
	Digest *Digest

	// rule is the screen's own note on the status rule (styled): a question,
	// a refusal or an action's outcome; it takes the place of Flash.
	rule string
	// status takes the place of the empty home's one line (styled): the
	// screen is loading, or could not read the factory.
	status string
}

const (
	factoryMarkW     = 4  // the cursor's cell, the row mark and the gap after each
	factoryWaitedW   = 6  // the width of "WAITED"
	factoryMinTitle  = 14 // the title's floor while the spec's shrink steps run
	factoryGitHubMin = 6  // room for "#12345", and the width of "GITHUB"
	factoryCountMax  = 9  // a staging count wider than this reads as a percent
	factoryTop       = 3  // the title, the chips and a blank above the body
	factoryBottom    = 3  // the rule, the help line and the hints below it
	factoryMoveFloor = 24 // the longest step the factory recommends: "read the diff, then push"
	factoryMoveShort = 15 // ... and its short form, "diff, then push"
	factoryMoveWide  = 28 // YOUR MOVE on a wide screen: the longest step and a little air
	factoryChipGap   = 16 // the room the rule chip keeps from the counts
	factoryLate      = 30 * time.Minute
	factoryDefaultW  = 140
)

// factoryForm picks which of a text's forms is drawn.
type factoryForm int

const (
	formShort factoryForm = iota // 100 columns
	formMid                      // 140 columns: the text itself
	formLong                     // 200 columns
)

// factoryLayout is how the table is laid out at one width.
type factoryLayout struct {
	wide                  bool // JIRA and GITHUB apart, ages on the staging cells, long chips and help
	waits, move, stage    factoryForm
	keyGap                int
	stageGap              int
	hideStaging, hideMove bool

	// measured
	staging, hasMove                 bool
	keyW, ghW, titleW, waitsW, moveW int
	stageW                           []int
}

func (l factoryLayout) total() int {
	n := factoryMarkW + l.keyW + l.keyGap + l.titleW + 2 + l.waitsW + factoryWaitedW
	if l.wide {
		n += l.ghW + 2
	}
	if l.hasMove {
		n += 2 + l.moveW
	}
	if l.staging {
		for _, w := range l.stageW {
			n += w + l.stageGap // the gap after the last one is the gap before WAITED
		}
	}
	return n + 2 // the gap before the first staging column, or before WAITED
}

// factoryPainter draws one frame.
type factoryPainter struct {
	f     factoryFrame
	st    styles
	g     glyphs
	base  prbPainter // the board's rule and footer rule
	now   time.Time
	width int

	rows   []FactoryRow // in display order
	cursor int          // the selected row's index in rows, -1 for none
	names  []string     // the staging columns, by first appearance
	lay    factoryLayout
}

// renderFactory draws the home screen in width cells and at most height
// lines (height <= 0: as many as it needs; width <= 0: 140). rowLines maps
// each task id on screen to its line of the frame, for mouse hits.
func renderFactory(f factoryFrame, width, height int, st styles, g glyphs, now time.Time) (frame string, rowLines map[int64]int) {
	if width <= 0 {
		width = factoryDefaultW
	}
	p := newFactoryPainter(f, width, st, g, now)
	return p.render(height)
}

func newFactoryPainter(f factoryFrame, width int, st styles, g glyphs, now time.Time) *factoryPainter {
	p := &factoryPainter{f: f, st: st, g: g, now: now, width: width, cursor: -1,
		base: prbPainter{st: st, pal: newPRBPalette(st), g: g}}
	p.f.Repo = cleanText(f.Repo)
	p.f.Blocking, p.f.BlockingLong = cleanAll(f.Blocking), cleanAll(f.BlockingLong)
	p.f.RuleChip, p.f.Flash, p.f.Next = cleanText(f.RuleChip), cleanText(f.Flash), cleanText(f.Next)
	p.f.Hints = make([]hint, len(f.Hints))
	for i, h := range f.Hints {
		p.f.Hints[i] = hint{cleanText(h.key), cleanText(h.desc)}
	}
	if d := f.Digest; d != nil {
		p.f.Digest = &Digest{Since: d.Since, Done: cleanAll(d.Done), Chip: cleanText(d.Chip)}
	}
	order := make([]int, len(f.Rows))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return int(f.Rows[a].Group) - int(f.Rows[b].Group) })
	for _, i := range order {
		if f.Rows[i].Group == GroupDone && p.digestLists(cleanText(f.Rows[i].Key)) {
			continue // the digest's line stands for the task
		}
		if i == f.Cursor {
			p.cursor = len(p.rows)
		}
		p.rows = append(p.rows, cleanFactoryRow(f.Rows[i]))
	}
	for _, r := range p.rows {
		for _, c := range r.Stagings {
			if !slices.Contains(p.names, c.Name) {
				p.names = append(p.names, c.Name)
			}
		}
	}
	p.lay = p.plan()
	return p
}

// digestDone is true when the digest has lines of what finished: they take
// the place of the done group.
func (p *factoryPainter) digestDone() bool { return p.f.Digest != nil && len(p.f.Digest.Done) > 0 }

// digestLists is true when a line of the digest names the task key: its
// row gives way to that line, so the task has one line (spec 9.A.1). A done
// task the digest does not name keeps its row under DONE TODAY.
func (p *factoryPainter) digestLists(key string) bool {
	if p.f.Digest == nil || key == "" {
		return false
	}
	return slices.ContainsFunc(p.f.Digest.Done, func(d string) bool {
		first, _, _ := strings.Cut(d, " ")
		return first == key
	})
}

func cleanFactoryRow(r FactoryRow) FactoryRow {
	r.Key, r.GitHub, r.Jira, r.Title = cleanText(r.Key), cleanText(r.GitHub), cleanText(r.Jira), cleanText(r.Title)
	r.Move, r.MoveShort, r.Frees = cleanText(r.Move), cleanText(r.MoveShort), cleanText(r.Frees)
	r.Help, r.HelpShort, r.HelpLong = cleanText(r.Help), cleanText(r.HelpShort), cleanText(r.HelpLong)
	r.WaitsOn = slices.Clone(r.WaitsOn)
	for i, w := range r.WaitsOn {
		r.WaitsOn[i] = WaitsOn{Who: cleanText(w.Who), What: cleanText(w.What), OnYou: w.OnYou,
			Short: cleanText(w.Short), Long: cleanText(w.Long)}
	}
	r.Stagings = slices.Clone(r.Stagings)
	for i, c := range r.Stagings {
		r.Stagings[i].Name, r.Stagings[i].State, r.Stagings[i].Count = cleanText(c.Name), cleanText(c.State), cleanText(c.Count)
	}
	if r.PR != nil {
		pr := *r.PR
		pr.Ref, pr.Off = cleanText(pr.Ref), cleanText(pr.Off)
		r.PR = &pr
	}
	return r
}

// text forms

func (w WaitsOn) text(form factoryForm) string {
	switch {
	case form == formShort && w.Short != "":
		return w.Short
	case form == formLong && w.Long != "":
		return w.Long
	}
	return w.What
}

// blockerText is the text of the row's blocker i in a form: what the last
// blocker of a queued row says is what frees it (see Frees).
func (r FactoryRow) blockerText(i int, form factoryForm) string {
	w := r.WaitsOn[i]
	if r.Frees == "" || i != len(r.WaitsOn)-1 {
		return w.text(form)
	}
	switch {
	case form == formShort && w.Short != "":
		return w.Short
	case form == formLong && w.text(form) != "":
		return w.text(form) + ", " + r.Frees
	}
	return r.Frees
}

func (r FactoryRow) moveText(form factoryForm) string {
	if r.Group != GroupNeedsYou { // only the rows that wait on the operator have a move
		return ""
	}
	if form == formShort && r.MoveShort != "" {
		return r.MoveShort
	}
	return r.Move
}

func (r FactoryRow) helpText(form factoryForm) string {
	switch {
	case form == formShort && r.HelpShort != "":
		return r.HelpShort
	case form == formLong && r.HelpLong != "":
		return r.HelpLong
	}
	return r.Help
}

// jiraKey is the row's JIRA cell on a wide screen.
func (r FactoryRow) jiraKey() string {
	switch {
	case r.Jira != "":
		return r.Jira
	case r.Key == r.GitHub:
		return ""
	}
	return r.Key
}

// glyphs and cells

func (p *factoryPainter) ascii() bool { return p.g.mode == IconsASCII }

// tick is the mark of something that went well.
func (p *factoryPainter) tick() string {
	if p.ascii() {
		return "+"
	}
	return "✓"
}

// markGlyph is the mark of a row that does not move.
func (p *factoryPainter) markGlyph(m FactoryMark) string {
	switch p.g.mode {
	case IconsASCII:
		return map[FactoryMark]string{MarkNeedsYou: "*", MarkWaits: "o", MarkDone: "+"}[m]
	case IconsNerd:
		return map[FactoryMark]string{MarkNeedsYou: p.g.stateIcon["needs_attention"], MarkWaits: p.g.stateIcon["queued"],
			MarkDone: p.g.stateIcon["reviewed"]}[m]
	}
	return map[FactoryMark]string{MarkNeedsYou: p.g.dot, MarkWaits: p.g.queued, MarkDone: "✓"}[m]
}

// spinnerFrame is the working mark: the only thing on the screen that moves.
func (p *factoryPainter) spinnerFrame() string {
	frames := p.g.working
	if len(frames) == 0 {
		frames = p.g.spinner.Frames
	}
	if len(frames) == 0 {
		return "*"
	}
	n := len(frames)
	return frames[(p.f.Spinner%n+n)%n]
}

func (p *factoryPainter) markSeg(r FactoryRow) seg {
	switch r.Mark {
	case MarkNeedsYou:
		return seg{p.markGlyph(r.Mark), p.st.Warn}
	case MarkWorking:
		return seg{p.spinnerFrame(), p.st.Accent}
	}
	return seg{p.markGlyph(r.Mark), p.st.Dim}
}

// tone is st, or Dim on a done row and on the row of your PR that magnum
// leaves alone (it asks nothing).
func (p *factoryPainter) tone(r FactoryRow, st lipgloss.Style) lipgloss.Style {
	if r.Group == GroupDone || r.PR != nil && r.PR.Off != "" {
		return p.st.Dim
	}
	return st
}

// noStyle draws text as it is.
var noStyle = lipgloss.Style{}

func (p *factoryPainter) whoStyle(w WaitsOn) lipgloss.Style {
	switch who := strings.ToLower(w.Who); {
	case w.OnYou || who == WaitsOnYou:
		return p.st.Warn
	case who == "memory" || who == "usage" || who == "copy": // the Mac's own resources
		return noStyle
	case who == "nothing":
		return p.st.Dim
	}
	return p.st.Accent
}

func (p *factoryPainter) waitsCell(r FactoryRow, form factoryForm) cell {
	var c cell
	for i, w := range r.WaitsOn {
		if i > 0 {
			c = append(c, seg{p.g.sep, p.st.Dim})
		}
		if w.Who != "" {
			c = append(c, seg{w.Who + ":", p.tone(r, p.whoStyle(w))})
		}
		if t := r.blockerText(i, form); t != "" {
			lead := " "
			if w.Who == "" {
				lead = ""
			}
			c = append(c, seg{lead + t, p.tone(r, noStyle)})
		}
	}
	return c
}

// percentOf turns "12/41" into "29%"; anything else stays as it is.
func percentOf(count string) string {
	a, b, ok := strings.Cut(count, "/")
	x, errA := strconv.Atoi(strings.TrimSpace(a))
	y, errB := strconv.Atoi(strings.TrimSpace(b))
	if !ok || errA != nil || errB != nil || y <= 0 {
		return count
	}
	return strconv.Itoa((x*100+y/2)/y) + "%"
}

// stageCell is one staging cell in a form: the words of the spec's table (9.C).
func (p *factoryPainter) stageCell(c StagingCell, form factoryForm) cell {
	short, ascii := form == formShort, p.ascii()
	pick := func(full, brief string) string {
		if short {
			return brief
		}
		return full
	}
	text, style := "", noStyle
	switch c.State {
	case "none":
		text, style = "–", p.st.Dim
		if ascii {
			text = "-"
		}
	case "outdated":
		text = pick("outdated", "old")
	case "merging":
		text = pick("merging", "merge")
	case "conflict":
		text = pick("conflict", "confl")
		if c.OnYou {
			style = p.st.Warn
		}
	case "ci":
		text = "CI " + c.Count
		if short || ansi.StringWidth(text) > factoryCountMax {
			text = "CI " + percentOf(c.Count)
		}
	case "deploying":
		text = pick("deploying", "deploy")
	case "qa":
		text = "QA " + c.Count
	case "qa-pass":
		text, style = "QA ✓", p.st.OK
		if ascii {
			text = pick("QA pass", "QA +")
		}
	case "qa-fail":
		text, style = "QA ✗", p.st.Err
		if ascii {
			text = pick("QA fail", "QA x")
		}
	case "latest":
		text, style = "latest", p.st.OK
	default:
		text = "?"
	}
	out := cell{{strings.TrimSpace(text), style}}
	if form == formLong && !c.Seen.IsZero() && c.State != "none" && c.State != "unknown" {
		out = append(out, seg{" " + shortAge(max(p.now.Sub(c.Seen), 0)), p.st.Dim})
	}
	return out
}

// stagingNamed is the row's cell for a staging server.
func stagingNamed(r FactoryRow, name string) (StagingCell, bool) {
	i := slices.IndexFunc(r.Stagings, func(c StagingCell) bool { return c.Name == name })
	if i < 0 {
		return StagingCell{}, false
	}
	return r.Stagings[i], true
}

// waitedCell is how long the row has waited, right-aligned in its column.
func (p *factoryPainter) waitedCell(r FactoryRow) cell {
	if r.Since.IsZero() {
		return nil
	}
	d := max(p.now.Sub(r.Since), 0)
	text, style := shortAge(d), noStyle
	if r.Group == GroupNeedsYou && d > factoryLate {
		style = p.st.Warn
	}
	return cell{{spaces(factoryWaitedW - ansi.StringWidth(text)), noStyle}, {text, style}}
}

// layout

// measure sizes every column of l from the data, the title at its natural width.
func (p *factoryPainter) measure(l factoryLayout) factoryLayout {
	l.staging = len(p.names) > 0 && !l.hideStaging
	l.hasMove = !l.hideMove
	l.keyW, l.ghW, l.titleW, l.waitsW, l.moveW, l.stageW = 1, 0, 1, len("WAITS ON"), 0, nil
	if l.wide {
		l.ghW = factoryGitHubMin
	}
	for _, r := range p.rows {
		key := r.Key
		if l.wide {
			key = r.jiraKey()
			l.ghW = max(l.ghW, ansi.StringWidth(r.GitHub))
		}
		l.keyW = max(l.keyW, ansi.StringWidth(key))
		l.titleW = max(l.titleW, ansi.StringWidth(r.Title))
		l.waitsW = max(l.waitsW, p.waitsCell(r, l.waits).width())
		l.moveW = max(l.moveW, ansi.StringWidth(r.moveText(l.move)))
	}
	if l.hasMove {
		floor := factoryMoveFloor
		switch {
		case l.move == formShort:
			floor = factoryMoveShort
		case l.wide:
			floor = factoryMoveWide
		}
		l.moveW = max(l.moveW, floor)
	}
	if l.staging {
		cw := 0
		for _, r := range p.rows {
			for _, c := range r.Stagings {
				cw = max(cw, p.stageCell(c, l.stage).width())
			}
		}
		for _, n := range p.names {
			l.stageW = append(l.stageW, max(cw, ansi.StringWidth(strings.ToUpper(n))))
		}
	}
	return l
}

// grow shares the width the table does not need out to the title and WAITS
// ON, the rest going to WAITS ON.
func (p *factoryPainter) grow(l factoryLayout) factoryLayout {
	slack := p.width - l.total()
	if slack <= 0 {
		return l
	}
	l.titleW += slack / 2
	l.waitsW += slack - slack/2
	return l
}

// plan picks the fullest layout that fits the width.
func (p *factoryPainter) plan() factoryLayout {
	wide := p.measure(factoryLayout{wide: true, waits: formLong, move: formMid, stage: formLong, keyGap: 2, stageGap: 2})
	if wide.total() <= p.width {
		return p.grow(wide)
	}
	steps := []factoryLayout{
		{waits: formMid, move: formMid, stage: formMid, keyGap: 2, stageGap: 2},
		{waits: formMid, move: formShort, stage: formMid, keyGap: 2, stageGap: 2},
		{waits: formShort, move: formShort, stage: formMid, keyGap: 2, stageGap: 2},
		{waits: formShort, move: formShort, stage: formShort, keyGap: 2, stageGap: 1},
		{waits: formShort, move: formShort, stage: formShort, keyGap: 1, stageGap: 1},
		{waits: formShort, move: formShort, stage: formShort, keyGap: 1, stageGap: 1, hideStaging: true},
		{waits: formShort, move: formShort, stage: formShort, keyGap: 1, stageGap: 1, hideStaging: true, hideMove: true},
	}
	var l factoryLayout
	for _, s := range steps {
		l = p.measure(s)
		room := p.width - (l.total() - l.titleW)
		if room >= min(l.titleW, factoryMinTitle) {
			l.titleW = min(l.titleW, room)
			return p.grow(l)
		}
	}
	// Nothing left to give up: the title and WAITS ON share what is left.
	avail := max(p.width-(l.total()-l.titleW-l.waitsW), 0)
	l.titleW = max(min(l.titleW, avail*2/5), min(avail, 4))
	l.waitsW = max(min(l.waitsW, avail-l.titleW), 0)
	return l
}

// columns

type placed struct {
	x, w int
	c    cell
}

// xs are the left edges of the columns.
type factoryXs struct {
	key, gh, title, waits, move, waited int
	stageAt                             []int
}

func (p *factoryPainter) xs() factoryXs {
	l := p.lay
	var x factoryXs
	x.key = factoryMarkW
	at := x.key + l.keyW + l.keyGap
	if l.wide {
		x.gh = at
		at += l.ghW + 2
	}
	x.title = at
	x.waits = x.title + l.titleW + 2
	at = x.waits + l.waitsW + 2
	if l.hasMove {
		x.move = at
		at += l.moveW + 2
	}
	if l.staging {
		for _, w := range l.stageW {
			x.stageAt = append(x.stageAt, at)
			at += w + l.stageGap
		}
	}
	x.waited = at
	return x
}

// compose lays the pieces out at their columns, each cut to its width.
func compose(pieces []placed) string {
	var b strings.Builder
	pos := 0
	for _, pc := range pieces {
		if pc.x > pos {
			b.WriteString(spaces(pc.x - pos))
		}
		c := pc.c.fit(pc.w)
		b.WriteString(c.render(nil))
		b.WriteString(spaces(pc.w - c.width()))
		pos = pc.x + pc.w
	}
	return b.String()
}

func (p *factoryPainter) rowPieces(r FactoryRow) []placed {
	l, x := p.lay, p.xs()
	key := r.Key
	if l.wide {
		key = r.jiraKey()
	}
	ps := []placed{
		{2, 1, cell{p.markSeg(r)}},
		{x.key, l.keyW, cell{{key, p.tone(r, p.st.Accent)}}},
	}
	if l.wide {
		ps = append(ps, placed{x.gh, l.ghW, cell{{r.GitHub, p.tone(r, p.st.Accent)}}})
	}
	ps = append(ps,
		placed{x.title, l.titleW, cell{{r.Title, p.tone(r, noStyle)}}},
		placed{x.waits, l.waitsW, p.waitsCell(r, l.waits)})
	if l.hasMove {
		ps = append(ps, placed{x.move, l.moveW, cell{{r.moveText(l.move), noStyle}}})
	}
	if l.staging {
		for i, n := range p.names {
			if c, ok := stagingNamed(r, n); ok {
				ps = append(ps, placed{x.stageAt[i], l.stageW[i], p.stageCell(c, l.stage)})
			}
		}
	}
	return append(ps, placed{x.waited, factoryWaitedW, p.waitedCell(r)})
}

// rowLine draws one row; the selected row is the cursor mark and the whole
// line in reverse video, to the edge.
func (p *factoryPainter) rowLine(r FactoryRow, selected bool) string {
	line := compose(p.rowPieces(r))
	if !selected {
		return strings.TrimRight(line, " ")
	}
	text := ansi.Strip(line)
	text += spaces(p.width - ansi.StringWidth(text))
	return p.cursorMark() + p.st.Selected.Render(text[len(noMark):])
}

func (p *factoryPainter) cursorMark() string {
	if p.ascii() {
		return "> "
	}
	return cursorMark
}

// headerLine is a group's heading with, when columns is set, the column
// names; above counts the tasks above the screen ("▲ 3 more").
func (p *factoryPainter) headerLine(label string, columns bool, above int) string {
	l, x := p.lay, p.xs()
	head := cell{{label, p.st.Section}}
	if above > 0 {
		lead := " "
		if label == "" {
			lead = ""
		}
		head = append(head, seg{fmt.Sprintf("%s%s %d more", lead, p.g.up, above), p.st.Dim})
	}
	ps := []placed{{1, max(x.waits-3, 1), head}}
	if columns {
		head := func(at, w int, s string) placed { return placed{at, w, cell{{s, p.st.Header}}} }
		ps = append(ps, head(x.waits, l.waitsW, "WAITS ON"))
		if l.hasMove {
			ps = append(ps, head(x.move, l.moveW, "YOUR MOVE"))
		}
		if l.staging {
			for i, n := range p.names {
				ps = append(ps, head(x.stageAt[i], l.stageW[i], strings.ToUpper(n)))
			}
		}
		ps = append(ps, head(x.waited, factoryWaitedW, "WAITED"))
	}
	return strings.TrimRight(compose(ps), " ")
}

// title, chips, footer

func (p *factoryPainter) helpForm() factoryForm {
	switch {
	case p.lay.wide:
		return formLong
	case p.lay.waits == formShort:
		return formShort
	}
	return formMid
}

func (p *factoryPainter) titleLine() string {
	st := p.st
	blocking := p.f.Blocking
	if p.lay.wide && len(p.f.BlockingLong) > 0 {
		blocking = p.f.BlockingLong
	}
	fact := strings.Join(blocking, st.Dim.Render(p.g.sep))
	clock := st.Dim.Render(p.g.refresh + " " + p.now.Format("15:04"))
	var rights []string
	if fact != "" {
		rights = append(rights, fact+st.Dim.Render(p.g.sep)+clock+" ", fact+" ")
	}
	rights = append(rights, clock+" ")
	lefts := []string{" " + st.Title.Render("magnum factory")}
	if p.f.Repo != "" {
		lefts = slices.Insert(lefts, 0, lefts[0]+st.Dim.Render(p.g.sep)+p.f.Repo)
	}
	for _, left := range lefts {
		for _, right := range rights {
			if ansi.StringWidth(left)+1+ansi.StringWidth(right) <= p.width {
				return spread(left, right, p.width)
			}
		}
	}
	return spread(lefts[len(lefts)-1], rights[len(rights)-1], p.width)
}

func factoryPlural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// chipLine counts the rows by group: the tasks and your PRs that need you
// apart ("1 task and 2 PRs need you"), the tasks that wait on others, your
// PRs at work (YOUR PRS) and the tasks done today.
func (p *factoryPainter) chipLine() string {
	st := p.st
	var tasks, prs, waits, yours, done int
	for _, r := range p.rows {
		switch r.Group {
		case GroupNeedsYou:
			if r.PR != nil {
				prs++
			} else {
				tasks++
			}
		case GroupWaits:
			waits++
		case GroupYours:
			yours++
		default:
			done++
		}
	}
	var parts []string
	if needs := tasks + prs; needs > 0 {
		verb := "need"
		if needs == 1 {
			verb = "needs"
		}
		var who []string
		if tasks > 0 {
			who = append(who, factoryPlural(tasks, "task", "tasks"))
		}
		if prs > 0 {
			who = append(who, factoryPlural(prs, "PR", "PRs"))
		}
		head := st.Warn.Render(p.markGlyph(MarkNeedsYou) + " " + strings.Join(who, " and ") + " " + verb + " you")
		if p.f.Decisions > 0 {
			head += st.Dim.Render(", " + factoryPlural(p.f.Decisions, "decision", "decisions"))
		}
		parts = append(parts, head)
	} else {
		parts = append(parts, st.OK.Render(p.tick()+" nothing waits on you"))
	}
	if waits > 0 {
		verb := "wait"
		if waits == 1 {
			verb = "waits"
		}
		parts = append(parts, st.Dim.Render(fmt.Sprintf("%d %s on others", waits, verb)))
	}
	if yours > 0 {
		parts = append(parts, st.Dim.Render(fmt.Sprintf("%d of your PRs at work", yours)))
	}
	chip := ""
	if p.f.RuleChip != "" {
		chip = st.Warn.Render(p.f.RuleChip) + " "
	}
	// "1 done today" gives way to "1 done" when the rule chip would sit close
	// to it. A digest's segment takes the place of the done count, until the
	// next key.
	dones := []string{fmt.Sprintf("%d done today", done), fmt.Sprintf("%d done", done)}
	switch d := p.f.Digest; {
	case d != nil && d.Chip != "":
		dones, done = []string{st.Label.Render(d.Chip)}, 1
	case done > 0:
		dones = []string{st.Dim.Render(dones[0]), st.Dim.Render(dones[1])}
	}
	left := ""
	for _, segment := range dones {
		all := parts
		if done > 0 {
			all = append(slices.Clone(parts), segment)
		}
		left = " " + strings.Join(all, st.Dim.Render(p.g.sep))
		if chip == "" || ansi.StringWidth(left)+ansi.StringWidth(chip)+factoryChipGap <= p.width {
			break
		}
	}
	if chip == "" {
		return truncate(left, p.width)
	}
	return spread(left, chip, p.width)
}

func (p *factoryPainter) hintsLine() string {
	hs := p.f.Hints
	if len(hs) == 0 {
		return ""
	}
	// Narrow screens drop the hints before the last one, which stays.
	sets := [][]hint{hs}
	last := hs[len(hs)-1]
	for k := len(hs) - 2; k >= 0; k-- {
		sets = append(sets, append(slices.Clone(hs[:k]), last))
	}
	return " " + p.st.fitHints(p.width-1, sets...)
}

// body

// lineKind is what a line of the home body is.
type lineKind int

const (
	lineRow     lineKind = iota // a task
	lineHeading                 // a group's name, maybe with the column names
	lineBlank                   // between two groups
	lineNote                    // a group's words: "Nothing waits on you.", a digest line
	lineMore                    // a collapsed group's "+<n> more: <keys>"
)

type factoryLine struct {
	text  string
	group FactoryGroup
	row   int // the row's index in rows; -1 for any other line
	kind  lineKind
	key   string // the task a row or a digest line names, for a more line
	label string // its group's heading
	tasks int    // the tasks the line stands for: 1 for a row, n for a more line
}

var factoryGroupLabels = [...]string{"NEEDS YOU", "WAITS ON OTHERS", "YOUR PRS", "DONE TODAY"}

// factoryBand is one group of the home list: its heading and its items, of
// which the first keep are drawn and the rest folded into one more line.
type factoryBand struct {
	group   FactoryGroup
	label   string
	columns bool // the heading carries the column names
	items   []factoryLine
	keep    int
	fold    bool // its items may collapse: never NEEDS YOU, never its words
	pin     int  // the items that stay drawn: up to the cursor's row
}

// bands are the groups the body draws. One task draws its own group only;
// otherwise an empty NEEDS YOU says so. A digest's lines are a group of
// their own after the done rows, under "DONE SINCE <day time>". The column
// names go on the first group that has rows.
func (p *factoryPainter) bands() []factoryBand {
	var out []factoryBand
	columns := true
	for g := GroupNeedsYou; g <= GroupDone; g++ {
		b := factoryBand{group: g, label: factoryGroupLabels[g], fold: g != GroupNeedsYou}
		for i, r := range p.rows {
			if r.Group != g {
				continue
			}
			if i == p.cursor {
				b.pin = len(b.items) + 1
			}
			b.items = append(b.items, factoryLine{text: p.rowLine(r, i == p.cursor), group: g, row: i, kind: lineRow, key: r.Key, tasks: 1})
		}
		hasRows := len(b.items) > 0
		if len(b.items) == 0 && g == GroupNeedsYou && len(p.rows) > 1 {
			b.fold = false
			b.items = append(b.items, factoryLine{text: "   " + p.st.OK.Bold(true).Render(p.tick()+" Nothing waits on you."), group: g, row: -1, kind: lineNote})
			if p.f.Next != "" {
				b.items = append(b.items, factoryLine{text: "     " + p.st.Dim.Render("Next for you: "+p.f.Next), group: g, row: -1, kind: lineNote})
			}
		}
		if len(b.items) > 0 {
			b.columns = columns && hasRows
			if hasRows {
				columns = false
			}
			b.keep = len(b.items)
			out = append(out, b)
		}
		if g == GroupDone && p.digestDone() {
			d := factoryBand{group: g, label: "DONE SINCE " + p.f.Digest.Since.In(p.now.Location()).Format("Mon 15:04"), fold: true}
			for _, text := range p.f.Digest.Done {
				key, _, _ := strings.Cut(text, " ")
				d.items = append(d.items, factoryLine{text: p.digestLine(text), group: g, row: -1, kind: lineNote, key: key, tasks: 1})
			}
			d.keep = len(d.items)
			out = append(out, d)
		}
	}
	return out
}

// digestLine is a line of the digest ("PR-12302 Remove old sync job ·
// merged 21:15"), drawn as a done task: its key in the key column, the rest
// from the title's.
func (p *factoryPainter) digestLine(text string) string {
	x := p.xs()
	key, rest, _ := strings.Cut(text, " ")
	return strings.TrimRight(compose([]placed{
		{2, 1, cell{p.markSeg(FactoryRow{Mark: MarkDone})}},
		{x.key, max(x.title-x.key-1, 1), cell{{key, p.st.Dim}}},
		{x.title, max(p.width-x.title, 1), cell{{rest, p.st.Dim}}},
	}), " ")
}

// moreLine stands for a collapsed group's hidden items: how many, and which.
func (p *factoryPainter) moreLine(g FactoryGroup, hidden []factoryLine) factoryLine {
	keys := make([]string, 0, len(hidden))
	n := 0
	for _, l := range hidden {
		keys = append(keys, l.key)
		n += l.tasks
	}
	text := fmt.Sprintf("+%d more: %s", len(hidden), strings.Join(keys, ", "))
	return factoryLine{text: strings.TrimRight(compose([]placed{{factoryMarkW, p.width - factoryMarkW, cell{{text, p.st.Dim}}}}), " "),
		group: g, row: -1, kind: lineMore, tasks: n}
}

// lines lays the bands out: a blank between two, each heading, its items
// as far as it keeps them and a more line for the rest.
func (p *factoryPainter) lines(bands []factoryBand) []factoryLine {
	var out []factoryLine
	for i, b := range bands {
		if i > 0 {
			out = append(out, factoryLine{group: bands[i-1].group, row: -1, kind: lineBlank})
		}
		out = append(out, factoryLine{text: p.headerLine(b.label, b.columns, 0), group: b.group, row: -1, kind: lineHeading, label: b.label})
		items := b.items[:b.keep]
		if b.keep < len(b.items) {
			items = append(slices.Clone(items), p.moreLine(b.group, b.items[b.keep:]))
		}
		for _, l := range items {
			l.label = b.label
			out = append(out, l)
		}
	}
	return out
}

// bodyLen is the lines the bands take.
func bodyLen(bands []factoryBand) int {
	n := 0
	for i, b := range bands {
		n += 1 + b.keep
		if i > 0 {
			n++
		}
		if b.keep < len(b.items) {
			n++
		}
	}
	return n
}

// collapse folds the long groups of bands until they fit room lines: the
// last group first (the done ones matter least), each from its end, one
// item at a time, a group's first fold hiding two for the more line it
// adds. NEEDS YOU and the cursor's row stay. When even that does not fit,
// it folds nothing: the list scrolls instead.
func collapse(bands []factoryBand, room int) []factoryBand {
	n := bodyLen(bands)
	if n <= room {
		return bands
	}
	out := slices.Clone(bands)
	for i := len(out) - 1; i >= 0 && n > room; i-- {
		b := &out[i]
		for b.fold && n > room {
			next := b.keep - 1
			if b.keep == len(b.items) {
				next = b.keep - 2
			}
			if next < b.pin || next < 0 {
				break
			}
			b.keep = next
			n--
		}
	}
	if n > room {
		return bands
	}
	return out
}

// emptyLine is the one line of a home with no task.
func (p *factoryPainter) emptyLine() string {
	text := p.st.Dim.Render("No tasks yet. ") + p.st.Key.Render("n") + p.st.Dim.Render(" starts one from a Jira key or your words.")
	if p.f.status != "" { // the screen is loading, or could not read the factory
		text = truncate(p.f.status, p.width)
	}
	return spaces((p.width-ansi.StringWidth(text))/2) + text
}

func (p *factoryPainter) render(height int) (string, map[int64]int) {
	empty := len(p.rows) == 0 && !p.digestDone()
	bodyH := 0
	if height > 0 {
		bodyH = max(height-factoryTop-factoryBottom, 1)
	}
	bands := p.bands()
	if height > 0 {
		bands = collapse(bands, bodyH)
	}
	body := p.lines(bands)
	if empty {
		body = []factoryLine{{text: p.emptyLine(), row: -1, kind: lineNote}}
	}
	if height <= 0 {
		bodyH = len(body)
	}
	// A list longer than the screen scrolls to the cursor. Its first line is
	// then the column names, on a line of their own with the tasks above
	// counted: s is the line it stands on, a heading or a blank it replaces,
	// or a task it pushes above the screen.
	start, sticky := 0, false
	if cur := slices.IndexFunc(body, func(l factoryLine) bool { return l.row >= 0 && l.row == p.cursor }); cur >= bodyH {
		start = cur - bodyH + 1
		sticky = bodyH >= 2
	}
	tasks := func(ls []factoryLine) int {
		n := 0
		for _, l := range ls {
			n += l.tasks
		}
		return n
	}
	var window []factoryLine
	if sticky {
		label, under := "", body[start+1]
		if under.kind != lineHeading && under.kind != lineBlank {
			label = under.label
		}
		window = append([]factoryLine{{text: p.headerLine(label, true, tasks(body[:start+1])), row: -1, kind: lineHeading}},
			body[start+1:min(start+bodyH, len(body))]...)
	} else {
		window = body[start:min(start+bodyH, len(body))]
	}
	// The rule counts the tasks below the screen, not the headings and blanks between them.
	below := tasks(body[min(start+bodyH, len(body)):])
	top := []string{p.titleLine(), p.chipLine(), ""}
	if empty {
		top[1] = ""
	}
	lines := top
	rowLines := map[int64]int{}
	if empty && height > 0 {
		for range (bodyH - 1) / 2 {
			lines = append(lines, "")
		}
	}
	for _, l := range window {
		if l.row >= 0 {
			rowLines[p.rows[l.row].ID] = len(lines)
		}
		lines = append(lines, l.text)
	}
	for height > 0 && len(lines) < factoryTop+bodyH {
		lines = append(lines, "")
	}
	msg := p.f.rule
	if msg == "" && p.f.Flash != "" {
		msg = p.st.OK.Render(p.tick() + " " + p.f.Flash)
	}
	help := ""
	if p.cursor >= 0 {
		if t := p.rows[p.cursor].helpText(p.helpForm()); t != "" {
			help = truncate(" "+t, p.width)
		}
	}
	lines = append(lines, p.base.footerRule(p.width, msg, below), help, p.hintsLine())
	if height > 0 && len(lines) > height {
		lines = lines[:height]
	}
	for i, l := range lines {
		lines[i] = truncate(l, p.width)
	}
	return strings.Join(lines, "\n"), rowLines
}
