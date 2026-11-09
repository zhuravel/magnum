package tui

// The factory's home screen painter (spec 9.C): data in, frame out. It holds
// no state and reads no clock; the screen model (a later task) feeds it the
// rows of the factory registry and the key hints of the moment.
//
// A row is one line: cursor, mark, repository, key, title, STATUS (its
// Jira issue's, on a wide screen), WAITS ON, its PR's CI, FINDINGS and
// APPR, YOUR MOVE, one column per staging server, WAITED. The PR's columns
// are drawn when a row has their data, in the PR board's own cells
// (ciCell, findingsCell). The repository has a column
// only when home shows more than one, or one the title does not name, by
// its name (its owner too only beside another owner's of the same name); it
// is cut to 11 cells on a narrow screen and to 6 once the staging cells go
// short, unless the title keeps 24 cells with its whole name, and never so
// short that two repositories' cells read alike. The widths
// come from the data. When the screen is too narrow the painter gives up,
// in this order, (1) the title's width down to 14 cells, (2) YOUR MOVE for
// its short form, then the PR's cells for theirs (the CI's glyph, the
// findings' verdict and worst priority), (3) WAITS ON for its short form
// (both blockers stay), (4) the staging cells for their short forms, (5)
// the gap after the key; beyond the spec it then drops the staging
// columns, YOUR MOVE (whose step the help line then names), FINDINGS and
// APPR (the CI's glyph stays), and squeezes the title and WAITS ON. When
// the screen is wide enough for everything at its fullest (long blockers,
// the age of each staging cell, the PR's whole cells) with the title
// squeezed to no less than 24 cells, it draws that instead and shares the
// spare width out; from 180 columns the longest blockers give up their
// ends there, down to 48 cells, before the title goes under 24. From 120
// columns, at every step, a row with a PR shows its number in a column of
// its own after the key, which is then the Jira key. From 180 columns,
// when a task row has a Jira status, a STATUS column after the title says
// it, in the wide layout or the 140-column one with the title at no less
// than 24 cells: its room comes from the wide forms, never from another
// column, and it is the first column the screen gives up.

import (
	"cmp"
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
	Repos []string     // the factory's repositories ("owner/name"), which the title names
	Rows  []FactoryRow // the painter groups them by Group, keeping their order in a group; Cursor indexes this slice
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
	// E's chooser, a refusal or an action's outcome; it takes the place of
	// Flash. ask are the lines above it of a question that wraps (styled,
	// indented: the lines before its last, which is rule): they take the
	// body's last lines.
	rule string
	ask  []string
	// status takes the place of the empty home's one line (styled): the
	// screen is loading, or could not read the factory.
	status string
	// The screen's narrowing of home (its / filter, its repository scope
	// and O's owner scope): all are every row, which the chips count, when
	// Rows are the filter's and the scopes' alone (nil: Rows are all); repo
	// is the repository shown alone, which the title says as the board's
	// does, or "all repos"; owner is the owner shown alone, which the title
	// says as the board's does, or "all owners" while owners (the rows span
	// several owners); filter is the filter's text, hidden the rows they
	// hide, which the title line says; ownerNext is the owner O moves to
	// ("" = every owner), which a scope that shows nothing names; input is
	// the filter as it is typed, drawn in place of the selected row's help
	// line.
	all                                   []FactoryRow
	repo, owner, ownerNext, filter, input string
	owners                                bool
	hidden                                int
}

const (
	factoryMarkW     = 4   // the cursor's cell, the row mark and the gap after each
	factoryWaitedW   = 6   // the width of "WAITED"
	factoryMinTitle  = 14  // the title's floor while the spec's shrink steps run
	factoryKeepTitle = 24  // the title the wide layout, or a cut REPO cell that takes its whole name back, leaves
	factoryPRFrom    = 120 // the narrowest screen with a PR column at every step
	factoryGitHubMin = 6   // room for "#12345", and the width of "GITHUB"
	factoryCountMax  = 9   // a staging count wider than this reads as a percent
	factoryRepoWide  = 24  // the repository's widest cell
	factoryRepoMid   = 11  // ... on a narrow screen: "integration"
	factoryRepoShort = 6   // ... once the staging cells go short
	factoryTop       = 3   // the title, the chips and a blank above the body
	factoryBottom    = 3   // the rule, the help line and the hints below it
	factoryMoveFloor = 24  // the longest step the factory recommends: "read the diff, then push"
	factoryMoveShort = 15  // ... and its short form, "diff, then push"
	factoryMoveWide  = 28  // YOUR MOVE on a wide screen: the longest step and a little air
	factoryChipGap   = 16  // the room the rule chip keeps from the counts
	factoryWaitsWide = 48  // WAITS ON's floor on a wide screen, whose longest blockers give their ends up for the title ...
	factoryCutFrom   = 180 // ... from this width
	factoryCIMid     = 14  // the CI cell's widest on a 140-column screen ...
	factoryCILong    = 22  // ... and on a wide one
	factoryFindMid   = 14  // the FINDINGS cell's widest on a 140-column screen ...
	factoryFindLong  = 18  // ... and on a wide one
	factoryLate      = 30 * time.Minute
	factoryDefaultW  = 140

	factoryStatusFrom = 180 // the narrowest screen with a STATUS column ...
	factoryStatusMax  = 26  // ... and its widest cell: "In Code Review 13h not you"
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
	wide                  bool // long blockers, ages on the staging cells, long chips and help
	split                 bool // the key is the Jira key, the PR number in a column of its own
	waits, move, stage    factoryForm
	pr                    factoryForm // the PR's CI, FINDINGS and APPR cells
	prMin                 bool        // the CI's glyph alone: no FINDINGS or APPR
	keyGap                int
	stageGap              int
	repoCap               int // the repository's widest cell
	hideStaging, hideMove bool
	status                bool // a STATUS column, when a row has a Jira status
	// longLines keeps the title line's and the help line's long forms in a
	// layout that is not wide: the wide one fits without its STATUS column.
	longLines bool

	// measured
	staging, hasMove                        bool
	repoW, keyW, ghW, titleW, waitsW, moveW int // repoW 0: no REPO column
	repoFull                                int // the widest REPO cell uncut (to factoryRepoWide)
	stageW                                  []int
	ciW, findW, apprW, statusW              int // 0: no such column
}

func (l factoryLayout) total() int {
	n := factoryMarkW + l.keyW + l.keyGap + l.titleW + 2 + l.waitsW + factoryWaitedW
	if l.repoW > 0 {
		n += l.repoW + 2
	}
	if l.split {
		n += l.ghW + 2
	}
	if l.statusW > 0 {
		n += l.statusW + 2
	}
	if l.hasMove {
		n += 2 + l.moveW
	}
	for _, w := range []int{l.ciW, l.findW, l.apprW} {
		if w > 0 {
			n += w + 2
		}
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

	// repos is true when the rows need a REPO column: home shows more than
	// one repository, or one the title does not name. labels name each
	// repository of the title and the rows (repoLabels, by its lower-cased
	// owner/name), and repoFloor is the narrowest REPO cell that still tells
	// the rows' repositories apart.
	repos     bool
	labels    map[string]string
	repoFloor int
	// prs is true when a row has a PR: a split layout has a PR column.
	prs bool
	// counted are the rows the chips count: every row of home, those the
	// filter and the scope hide too.
	counted []FactoryRow
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
	p.f.Repos = cleanAll(f.Repos)
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
	p.counted = p.rows
	if f.all != nil {
		p.counted = slices.DeleteFunc(slices.Clone(f.all), func(r FactoryRow) bool {
			return r.Group == GroupDone && p.digestLists(cleanText(r.Key))
		})
	}
	p.f.repo, p.f.owner, p.f.ownerNext = cleanText(f.repo), cleanText(f.owner), cleanText(f.ownerNext)
	p.f.filter, p.f.input = cleanText(f.filter), cleanText(f.input)
	for _, r := range p.rows {
		for _, c := range r.Stagings {
			if !slices.Contains(p.names, c.Name) {
				p.names = append(p.names, c.Name)
			}
		}
	}
	p.repos, p.labels, p.repoFloor = p.repoColumn()
	p.prs = slices.ContainsFunc(p.rows, func(r FactoryRow) bool { return r.GitHub != "" })
	p.lay = p.plan()
	return p
}

// repoColumn says whether the rows need a REPO column, how the title and
// the rows name each repository (repoLabels, over both), and the narrowest
// REPO cell at which the rows' cells still tell their repositories apart
// (repoFloor). A title that names one repository every row is of leaves the
// column out.
func (p *factoryPainter) repoColumn() (column bool, labels map[string]string, floor int) {
	one := ""
	if len(p.f.Repos) == 1 {
		one = p.f.Repos[0]
	}
	repos := slices.Clone(p.f.Repos)
	var shown []string
	for _, r := range p.rows {
		if r.Repo != "" {
			column = column || !strings.EqualFold(r.Repo, one)
			repos = append(repos, r.Repo)
			shown = append(shown, r.Repo)
		}
	}
	labels = repoLabels(repos)
	var cells []string
	for _, full := range shown {
		if l := labels[strings.ToLower(full)]; !slices.Contains(cells, l) {
			cells = append(cells, l)
		}
	}
	return column, labels, repoFloor(cells)
}

// repoText is the row's REPO cell: its repository as the screen names it.
func (p *factoryPainter) repoText(r FactoryRow) string {
	return cmp.Or(p.labels[strings.ToLower(r.Repo)], r.Repo)
}

// repoLabels names each of repos ("owner/name") as the screen does, by its
// lower-cased owner/name: its name alone, or owner/name when a repository
// of another owner has the same name, so the owner is said only where it
// tells two repositories apart. One without an owner keeps its words.
func repoLabels(repos []string) map[string]string {
	owners := map[string]map[string]bool{} // by lower-cased name
	for _, full := range repos {
		if owner, name, ok := strings.Cut(full, "/"); ok {
			n := strings.ToLower(name)
			if owners[n] == nil {
				owners[n] = map[string]bool{}
			}
			owners[n][strings.ToLower(owner)] = true
		}
	}
	out := make(map[string]string, len(repos))
	for _, full := range repos {
		label := full
		if _, name, ok := strings.Cut(full, "/"); ok && len(owners[strings.ToLower(name)]) < 2 {
			label = name
		}
		out[strings.ToLower(full)] = label
	}
	return out
}

// repoFloor is the narrowest cell in which labels, each cut as a cell is
// (fit), still tell their repositories apart: no cut label's text before
// its ellipsis starts another label, so talkable-widget is never "talka…"
// beside talkable. It is the widest label's width when no narrower cell
// does.
func repoFloor(labels []string) int {
	widest := 0
	for _, l := range labels {
		widest = max(widest, ansi.StringWidth(l))
	}
	for w := 1; w < widest; w++ {
		if repoCutsApart(labels, w) {
			return w
		}
	}
	return widest
}

// repoCutsApart says whether labels cut to w cells tell their repositories
// apart (repoFloor).
func repoCutsApart(labels []string, w int) bool {
	for _, l := range labels {
		if ansi.StringWidth(l) <= w {
			continue
		}
		shown := strings.ToLower(strings.TrimRight(ansi.Truncate(l, w-1, ""), " "))
		for _, o := range labels {
			if o != l && strings.HasPrefix(strings.ToLower(o), shown) {
				return false
			}
		}
	}
	return true
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
	r.Key, r.GitHub, r.Jira, r.Title, r.Repo = cleanText(r.Key), cleanText(r.GitHub), cleanText(r.Jira), cleanText(r.Title), cleanText(r.Repo)
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
	r.JiraStatus = cleanJiraStatus(r.JiraStatus)
	r.CI = cleanCI(r.CI)
	if r.Findings != nil {
		f := *r.Findings
		f.Verdict, f.Posted, f.SHA = cleanText(f.Verdict), cleanText(f.Posted), cleanText(f.SHA)
		r.Findings = &f
	}
	if r.Approvals != nil {
		a := *r.Approvals
		a.Decision = cleanText(a.Decision)
		r.Approvals = &a
	}
	return r
}

// cleanJiraStatus is a copy of js with its words cleaned (nil for nil):
// the row and its task card share js, which a painter never changes.
func cleanJiraStatus(js *FactoryJiraStatus) *FactoryJiraStatus {
	if js == nil {
		return nil
	}
	c := *js
	c.Name, c.Category, c.LastError = cleanText(c.Name), cleanText(c.Category), cleanText(c.LastError)
	return &c
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

// jiraKey is the row's key cell beside a PR column: your PR's row has only
// the issue key of its title.
func (r FactoryRow) jiraKey() string {
	switch {
	case r.Jira != "":
		return r.Jira
	case r.yourPR() || r.Key == r.GitHub:
		return ""
	}
	return r.Key
}

// keyText is the row's key cell in layout l: the Jira key beside a PR
// column; your PR's number when a REPO column names its repository; else
// its key.
func (r FactoryRow) keyText(l factoryLayout) string {
	switch {
	case l.split:
		return r.jiraKey()
	case l.repoW > 0 && r.yourPR() && r.GitHub != "":
		return r.GitHub
	}
	return r.Key
}

// titleText is the row's title in layout l, without the issue key it
// starts with ("[PR-12400] ", "PR-12400: ") when the row's key cell shows
// that key: the row would say it twice. A title that is the key alone
// keeps it.
func (r FactoryRow) titleText(l factoryLayout) string {
	k := r.keyText(l)
	if k == "" || strings.Contains(k, "#") {
		return r.Title
	}
	rest, ok := strings.CutPrefix(r.Title, "["+k+"]")
	if !ok {
		if rest, ok = strings.CutPrefix(r.Title, k); ok && rest != "" && !strings.ContainsAny(rest[:1], " :") {
			return r.Title // a longer key: PR-123456 is not PR-12345
		}
	}
	if rest = strings.TrimLeft(rest, " :"); !ok || rest == "" {
		return r.Title
	}
	return rest
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

// tone is st, or Dim on a done row and on a row whose PR magnum leaves
// alone, your PR's or a task's (it asks nothing).
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

// waitsCell is the row's WAITS ON cell in layout l: the yellow fact of
// Jira done while its PR is open when l has no STATUS column to say it
// (jiraDoneOpen), the PR's marks (prMarks), then what it waits on, in l's
// form.
func (p *factoryPainter) waitsCell(r FactoryRow, l factoryLayout) cell {
	form := l.waits
	var c cell
	if r.jiraDoneOpen() && l.statusW == 0 {
		c = append(c, seg{"Jira done", p.tone(r, p.st.Warn)}, seg{p.g.sep, p.st.Dim})
	}
	for _, m := range p.prMarks(r) {
		c = append(c, seg{m, p.st.Accent}, seg{p.g.sep, p.st.Dim})
	}
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

// prMarks are the states of the row's PR that M and z set, before what it
// waits on: "muted", and "snoozed → 16:09" while its snooze holds, each
// unless the row's own words say it already (a muted PR magnum's look
// leaves alone, in LEFT ALONE).
func (p *factoryPainter) prMarks(r FactoryRow) []string {
	if r.PR == nil || r.PR.Facts == nil {
		return nil
	}
	var said strings.Builder
	for _, w := range r.WaitsOn {
		said.WriteString(strings.ToLower(w.What + " " + w.Short + " " + w.Long + " "))
	}
	var marks []string
	f := r.PR.Facts
	if f.Muted && !strings.Contains(said.String(), "muted") {
		marks = append(marks, "muted")
	}
	if f.SnoozedUntil.After(p.now) && !strings.Contains(said.String(), "snoozed") {
		marks = append(marks, "snoozed → "+untilClock(f.SnoozedUntil, p.now))
	}
	return marks
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
	case "ci-fail":
		text, style = "CI ✗", p.st.Err
		if ascii {
			text = pick("CI fail", "CI x")
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

// jiraStyle is the style of a Jira status's name by its category, as the
// screen draws other states: done green, new (to do) dim, any other plain.
func jiraStyle(st styles, js *FactoryJiraStatus) lipgloss.Style {
	switch js.Category {
	case "done":
		return st.OK
	case "new":
		return st.Dim
	}
	return noStyle
}

// jiraAssignee is the STATUS cell's mark of who has the issue: "" for you,
// "not you" for someone else, "nobody" for nobody, "assigned" for an
// assignee not known to be you (YouUnknown).
func jiraAssignee(js *FactoryJiraStatus) string {
	switch {
	case js.AssignedToYou:
		return ""
	case js.Assigned && js.YouUnknown:
		return "assigned"
	case js.Assigned:
		return "not you"
	}
	return "nobody"
}

// old reports whether js's facts may no longer hold: its last try failed
// before the issue answered (so they are an older read's), or no factory
// daemon runs to read it again.
func (js *FactoryJiraStatus) old() bool {
	return js.DaemonDown || js.LastError != "" && js.TriedAt.After(js.ReadAt)
}

// jiraDoneOpen reports whether Jira says the task's issue is done while
// its PR is still open (magnum's registry has it open: r.PR), a fact on the
// operator, drawn yellow: close or merge the PR, or move the issue back.
// An old read (old) is no such fact: the issue may have moved since.
func (r FactoryRow) jiraDoneOpen() bool {
	return r.JiraStatus != nil && r.JiraStatus.Category == "done" && r.PR != nil && !r.JiraStatus.old()
}

// statusText is the row's STATUS cell in w cells (0: whole): its Jira
// issue's status in its category's style (yellow when it is done while its
// PR is open: jiraDoneOpen), how long the issue has held it
// (none when that is not known) and who has it when it is not you, the
// name cut first; an old read (old) all dim, with a dim ? after it; a dim
// ? when no read of it worked; nothing for a row without a Jira status.
func (p *factoryPainter) statusText(r FactoryRow, w int) cell {
	js := r.JiraStatus
	switch {
	case js == nil:
		return nil
	case js.Name == "":
		return cell{{"?", p.st.Dim}}
	}
	old := js.old()
	var tail cell
	if !js.Since.IsZero() {
		tail = append(tail, seg{" " + shortAge(max(p.now.Sub(js.Since), 0)), p.st.Dim})
	}
	if who := jiraAssignee(js); who != "" {
		st := p.tone(r, p.st.Accent)
		if old {
			st = p.st.Dim
		}
		tail = append(tail, seg{" ", noStyle}, seg{who, st})
	}
	if old {
		tail = append(tail, seg{" ?", p.st.Dim})
	}
	style := jiraStyle(p.st, js)
	switch {
	case old:
		style = p.st.Dim
	case r.jiraDoneOpen():
		style = p.st.Warn
	}
	name := cell{{js.Name, p.tone(r, style)}}
	if w > 0 && name.width()+tail.width() > w {
		name = name.fit(max(w-tail.width(), 1))
	}
	return append(name, tail...)
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

// ciText is the row's CI cell in a form: the board's (ciCell), its glyph
// alone in the short form (the worst required check's, else the counts'),
// with the board's ⟳ for a CI of an older commit than the head in both;
// nothing for a row without a PR.
func (p *factoryPainter) ciText(r FactoryRow, form factoryForm) cell {
	ci := r.CI
	switch {
	case ci == nil:
		return nil
	case form != formShort:
		return p.base.ciCell(ci)
	}
	state := ci.State
	if len(ci.Required) > 0 {
		state = slices.MinFunc(ci.Required, func(a, b CheckState) int { return ciRank(a.State) - ciRank(b.State) }).State
	}
	c := p.base.dash()
	if l := p.base.ciLook(state); l.glyph != "" {
		c = cell{{l.glyph, l.glyphSt}}
	}
	if ci.Stale {
		c = append(c, seg{" " + p.g.stale, p.base.pal.yellow})
	}
	return c
}

// findingsText is the row's FINDINGS cell in a form: the board's
// (findingsCell), in the short form the review's verdict and its worst
// priority, new or still open ("✗ P1"), or the board's cell when it lists
// no priority ("✔ clean", "● 3 open"); nothing for a row without a PR.
func (p *factoryPainter) findingsText(r FactoryRow, form factoryForm) cell {
	f := r.Findings
	if f == nil {
		return nil
	}
	if form == formShort {
		for i := range f.Counts {
			if f.Counts[i] > 0 || f.OpenCounts != nil && f.OpenCounts[i] > 0 {
				glyph, vst := p.base.findingsVerdict(f.Verdict)
				return cell{{glyph + " ", vst}, {"P" + strconv.Itoa(i), p.base.priorityStyle(i)}}
			}
		}
	}
	return p.base.findingsCell(f)
}

// apprText is the row's APPR cell: the changes requested ("✗ 1"), else the
// approvals ("✔ 2", "⟳" when one is of an older commit than the head),
// else a dash; nothing for a row without a PR.
func (p *factoryPainter) apprText(r FactoryRow, _ factoryForm) cell {
	a := r.Approvals
	switch {
	case a == nil:
		return nil
	case a.Changes > 0:
		return cell{{p.g.changes + " " + strconv.Itoa(a.Changes), p.base.pal.red}}
	case a.Approved > 0:
		c := cell{{p.g.approved + " " + strconv.Itoa(a.Approved), p.base.pal.green}}
		if a.Stale > 0 {
			c = append(c, seg{" " + p.g.stale, p.base.pal.yellow})
		}
		return c
	}
	return p.base.dash()
}

// layout

// measure sizes every column of l from the data, the title at its natural width.
func (p *factoryPainter) measure(l factoryLayout) factoryLayout {
	l.staging = len(p.names) > 0 && !l.hideStaging
	l.hasMove = !l.hideMove
	l.repoW, l.repoFull, l.keyW, l.ghW, l.titleW, l.waitsW, l.moveW, l.stageW = 0, 0, 1, 0, 1, len("WAITS ON"), 0, nil
	l.ciW, l.findW, l.apprW, l.statusW = 0, 0, 0, 0
	ciCap, findCap := factoryCIMid, factoryFindMid
	if l.pr == formLong {
		ciCap, findCap = factoryCILong, factoryFindLong
	}
	if l.split {
		l.ghW = factoryGitHubMin
	}
	if p.repos { // never so narrow that two repositories' cells read alike
		l.repoFull = p.repoFloor
	}
	for _, r := range p.rows {
		if c := p.statusText(r, 0); l.status && c != nil {
			l.statusW = max(l.statusW, len("STATUS"), min(c.width(), factoryStatusMax))
		}
	}
	for _, r := range p.rows {
		if p.repos {
			l.repoFull = max(l.repoFull, min(ansi.StringWidth(p.repoText(r)), factoryRepoWide), 1)
			l.repoW = max(min(l.repoFull, l.repoCap), p.repoFloor)
		}
		if l.split {
			l.ghW = max(l.ghW, ansi.StringWidth(r.GitHub))
		}
		l.keyW = max(l.keyW, ansi.StringWidth(r.keyText(l)))
		l.titleW = max(l.titleW, ansi.StringWidth(r.titleText(l)))
		l.waitsW = max(l.waitsW, p.waitsCell(r, l).width())
		l.moveW = max(l.moveW, ansi.StringWidth(r.moveText(l.move)))
		if r.CI != nil {
			l.ciW = max(l.ciW, len("CI"), min(p.ciText(r, l.pr).width(), ciCap))
		}
		if r.Findings != nil && !l.prMin {
			l.findW = max(l.findW, len("FINDINGS"), min(p.findingsText(r, l.pr).width(), findCap))
		}
		if r.Approvals != nil && !l.prMin {
			l.apprW = max(l.apprW, len("APPR"), p.apprText(r, l.pr).width())
		}
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

// grow shares the width the table does not need out: to a cut REPO cell
// first, up to its whole name, then to the title and WAITS ON, the rest
// going to WAITS ON.
func (p *factoryPainter) grow(l factoryLayout) factoryLayout {
	slack := p.width - l.total()
	if slack <= 0 {
		return l
	}
	more := min(slack, max(l.repoFull-l.repoW, 0))
	l.repoW += more
	slack -= more
	l.titleW += slack / 2
	l.waitsW += slack - slack/2
	return l
}

// plan picks the fullest layout that fits the width. From
// factoryStatusFrom, when a row has a Jira status, it first tries the wide
// layout and the 140-column one with a STATUS column, the title keeping
// factoryKeepTitle cells: the column takes its room from the wide forms
// (which the help line and the card say), never from another column, and
// it is the first column the screen gives up. The title line and the help
// line span the screen, so they keep the forms the layout without the
// column gives them (longLines).
func (p *factoryPainter) plan() factoryLayout {
	full := factoryLayout{wide: true, split: p.prs, waits: formLong, move: formMid, stage: formLong, pr: formLong, keyGap: 2,
		stageGap: 2, repoCap: factoryRepoWide}
	const mid, short = factoryRepoMid, factoryRepoShort
	split := p.prs && p.width >= factoryPRFrom
	steps := []factoryLayout{
		{split: split, waits: formMid, move: formMid, stage: formMid, pr: formMid, keyGap: 2, stageGap: 2, repoCap: mid},
		{split: split, waits: formMid, move: formShort, stage: formMid, pr: formMid, keyGap: 2, stageGap: 2, repoCap: mid},
		{split: split, waits: formMid, move: formShort, stage: formMid, pr: formShort, keyGap: 2, stageGap: 2, repoCap: mid},
		{split: split, waits: formShort, move: formShort, stage: formMid, pr: formShort, keyGap: 2, stageGap: 2, repoCap: mid},
		{split: split, waits: formShort, move: formShort, stage: formShort, pr: formShort, keyGap: 2, stageGap: 1, repoCap: short},
		{split: split, waits: formShort, move: formShort, stage: formShort, pr: formShort, keyGap: 1, stageGap: 1, repoCap: short},
		{split: split, waits: formShort, move: formShort, stage: formShort, pr: formShort, keyGap: 1, stageGap: 1, repoCap: short,
			hideStaging: true},
		{split: split, waits: formShort, move: formShort, stage: formShort, pr: formShort, keyGap: 1, stageGap: 1, repoCap: short,
			hideStaging: true, hideMove: true},
		{split: split, waits: formShort, move: formShort, stage: formShort, pr: formShort, prMin: true, keyGap: 1, stageGap: 1,
			repoCap: short, hideStaging: true, hideMove: true},
	}
	if p.width >= factoryStatusFrom && slices.ContainsFunc(p.rows, func(r FactoryRow) bool { return r.JiraStatus != nil }) {
		wide, mid := full, steps[0]
		wide.status, mid.status = true, true
		if l, ok := p.fitWide(wide); ok {
			return l
		}
		if l, ok := p.fitStep(mid, factoryKeepTitle); ok {
			_, l.longLines = p.fitWide(full)
			return l
		}
	}
	if l, ok := p.fitWide(full); ok {
		return l
	}
	var l factoryLayout
	for _, s := range steps {
		var ok bool
		if l, ok = p.fitStep(s, factoryMinTitle); ok {
			return l
		}
	}
	// Nothing left to give up: the title and WAITS ON share what is left.
	avail := max(p.width-(l.total()-l.titleW-l.waitsW), 0)
	l.titleW = max(min(l.titleW, avail*2/5), min(avail, 4))
	l.waitsW = max(min(l.waitsW, avail-l.titleW), 0)
	return l
}

// fitWide is the wide layout s at the screen's width, when it fits with
// the title squeezed to no less than factoryKeepTitle cells; from
// factoryCutFrom the longest blockers give up their ends (the help line
// says them whole) before the wide layout gives up the PR's columns.
func (p *factoryPainter) fitWide(s factoryLayout) (factoryLayout, bool) {
	wide := p.measure(s)
	need, room := min(wide.titleW, factoryKeepTitle), p.width-(wide.total()-wide.titleW)
	if cut := min(need-room, wide.waitsW-factoryWaitsWide); p.width >= factoryCutFrom && cut > 0 && room+cut >= need {
		wide.waitsW -= cut
		room += cut
	}
	if room < need {
		return wide, false
	}
	wide.titleW = min(wide.titleW, room)
	return p.grow(wide), true
}

// fitStep is step s at the screen's width, when it fits with the title
// squeezed to no less than floor cells (its own width when narrower); the
// layout measured when it does not.
func (p *factoryPainter) fitStep(s factoryLayout, floor int) (factoryLayout, bool) {
	l := p.measure(s)
	room := p.width - (l.total() - l.titleW)
	if room < min(l.titleW, floor) {
		return l, false
	}
	l.titleW = min(l.titleW, room)
	// A cut REPO cell takes its whole name back from a title that keeps
	// factoryKeepTitle cells.
	more := min(max(l.repoFull-l.repoW, 0), max(l.titleW-factoryKeepTitle, 0))
	l.repoW, l.titleW = l.repoW+more, l.titleW-more
	return p.grow(l), true
}

// columns

type placed struct {
	x, w int
	c    cell
}

// xs are the left edges of the columns.
type factoryXs struct {
	repo, key, gh, title, status, waits, ci, find, appr, move, waited int
	stageAt                                                           []int
}

func (p *factoryPainter) xs() factoryXs {
	l := p.lay
	var x factoryXs
	x.repo, x.key = factoryMarkW, factoryMarkW
	if l.repoW > 0 {
		x.key += l.repoW + 2
	}
	at := x.key + l.keyW + l.keyGap
	if l.split {
		x.gh = at
		at += l.ghW + 2
	}
	x.title = at
	x.waits = x.title + l.titleW + 2
	if l.statusW > 0 {
		x.status = x.waits
		x.waits += l.statusW + 2
	}
	at = x.waits + l.waitsW + 2
	for _, c := range []struct {
		x *int
		w int
	}{{&x.ci, l.ciW}, {&x.find, l.findW}, {&x.appr, l.apprW}} {
		if c.w > 0 {
			*c.x = at
			at += c.w + 2
		}
	}
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
	ps := []placed{{2, 1, cell{p.markSeg(r)}}}
	if l.repoW > 0 {
		ps = append(ps, placed{x.repo, l.repoW, cell{{p.repoText(r), p.st.Dim}}})
	}
	ps = append(ps, placed{x.key, l.keyW, cell{{r.keyText(l), p.tone(r, p.st.Accent)}}})
	if l.split {
		ps = append(ps, placed{x.gh, l.ghW, cell{{r.GitHub, p.tone(r, p.st.Accent)}}})
	}
	ps = append(ps, placed{x.title, l.titleW, cell{{r.titleText(l), p.tone(r, noStyle)}}})
	if l.statusW > 0 {
		ps = append(ps, placed{x.status, l.statusW, p.statusText(r, l.statusW)})
	}
	ps = append(ps, placed{x.waits, l.waitsW, p.waitsCell(r, l)})
	if l.ciW > 0 {
		ps = append(ps, placed{x.ci, l.ciW, p.ciText(r, l.pr)})
	}
	if l.findW > 0 {
		ps = append(ps, placed{x.find, l.findW, p.findingsText(r, l.pr)})
	}
	if l.apprW > 0 {
		ps = append(ps, placed{x.appr, l.apprW, p.apprText(r, l.pr)})
	}
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
	first := x.waits // the label's room ends before the first named column
	if l.statusW > 0 {
		first = x.status
	}
	ps := []placed{{1, max(first-3, 1), head}}
	if columns {
		head := func(at, w int, s string) placed { return placed{at, w, cell{{s, p.st.Header}}} }
		if l.statusW > 0 {
			ps = append(ps, head(x.status, l.statusW, "STATUS"))
		}
		ps = append(ps, head(x.waits, l.waitsW, "WAITS ON"))
		for _, c := range []struct {
			x, w int
			name string
		}{{x.ci, l.ciW, "CI"}, {x.find, l.findW, "FINDINGS"}, {x.appr, l.apprW, "APPR"}} {
			if c.w > 0 {
				ps = append(ps, head(c.x, c.w, c.name))
			}
		}
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
	case p.lay.wide || p.lay.longLines:
		return formLong
	case p.lay.waits == formShort:
		return formShort
	}
	return formMid
}

func (p *factoryPainter) titleLine() string {
	st := p.st
	blocking := p.f.Blocking
	if (p.lay.wide || p.lay.longLines) && len(p.f.BlockingLong) > 0 {
		blocking = p.f.BlockingLong
	}
	fact := strings.Join(blocking, st.Dim.Render(p.g.sep))
	clock := st.Dim.Render(p.g.refresh + " " + p.now.Format("15:04"))
	var rights []string
	if fact != "" {
		rights = append(rights, fact+st.Dim.Render(p.g.sep)+clock+" ", fact+" ")
	}
	rights = append(rights, clock+" ")
	head := func(full bool) string {
		left := " " + st.Title.Render("magnum factory")
		for _, part := range append(p.scopes(full), p.narrowing()...) { // the scopes, the filter and the rows they hide
			left += st.Dim.Render(p.g.sep) + part
		}
		return left
	}
	// The repositories' names give way to their count before the right side
	// gives up anything, and go only when the count does not fit either.
	var named []string
	if n := len(p.f.Repos); n > 0 {
		names := make([]string, n)
		for i, full := range p.f.Repos {
			names[i] = cmp.Or(p.labels[strings.ToLower(full)], full)
		}
		named = append(named, st.Dim.Render(p.g.sep)+strings.Join(names, ", "))
		if n > 1 {
			named = append(named, st.Dim.Render(p.g.sep)+fmt.Sprintf("%d repos", n))
		}
	}
	fits := func(left, right string) bool { return ansi.StringWidth(left)+1+ansi.StringWidth(right) <= p.width }
	// From the fullest to the barest, as the board's title: the "all repos"
	// and "all owners" scopes and the word "owner" give way first, with the
	// right side whole; then the names give way to their count, then the
	// right side gives up its parts, then the names go. The repository and
	// the owner shown alone, the filter and the rows they hide stay,
	// whatever else gives way.
	if full := head(true) + strings.Join(named[:min(len(named), 1)], ""); fits(full, rights[0]) {
		return spread(full, rights[0], p.width)
	}
	bare := head(false)
	for _, right := range rights {
		for _, tail := range named {
			if fits(bare+tail, right) {
				return spread(bare+tail, right, p.width)
			}
		}
	}
	for _, right := range rights {
		if fits(bare, right) {
			return spread(bare, right, p.width)
		}
	}
	return spread(bare, rights[len(rights)-1], p.width)
}

// scopes are the title's words of the repository and the owner scopes,
// the board's words in the board's places: "all repos" or the repository,
// then "owner alice", or "all owners" while the rows span several owners;
// a short title (full false) says the repository and the owner shown
// alone, without the word "owner", and no "all repos" or "all owners".
func (p *factoryPainter) scopes(full bool) []string {
	var parts []string
	switch {
	case p.f.repo != "":
		parts = append(parts, p.st.Accent.Render(p.f.repo))
	case full:
		parts = append(parts, "all repos")
	}
	switch {
	case p.f.owner != "" && full:
		parts = append(parts, p.st.Dim.Render("owner ")+p.st.Accent.Render(p.f.owner))
	case p.f.owner != "":
		parts = append(parts, p.st.Accent.Render(p.f.owner))
	case full && p.f.owners:
		parts = append(parts, "all owners")
	}
	return parts
}

// narrowing is what the title line says of the filter and of the rows the
// filter and the scopes hide (`filter "coupon"`, "6 hidden"); none when
// home shows every row.
func (p *factoryPainter) narrowing() []string {
	var parts []string
	if p.f.filter != "" {
		parts = append(parts, p.st.Accent.Render(fmt.Sprintf("filter %q", p.f.filter)))
	}
	if p.f.hidden > 0 {
		parts = append(parts, p.st.Accent.Render(fmt.Sprintf("%d hidden", p.f.hidden)))
	}
	return parts
}

func factoryPlural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// chipLine counts the rows by group: the tasks and your PRs that need you
// apart ("1 task and 2 PRs need you"), the rows that wait on others with
// those an agent or magnum's fixer works on now (the moving mark: "3 wait
// on others, 2 at work"), your PRs magnum leaves alone ("1 left alone":
// they ask nothing, and nothing works on them) and the tasks done today.
func (p *factoryPainter) chipLine() string {
	st := p.st
	var tasks, prs, waits, working, alone, done int
	for _, r := range p.counted {
		switch r.Group {
		case GroupNeedsYou:
			if r.yourPR() {
				prs++
			} else {
				tasks++
			}
		case GroupWaits:
			waits++
			if r.Mark == MarkWorking {
				working++
			}
		case GroupYours:
			alone++
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
	// The waits' segment gives up its "at work" before the line is cut.
	waitParts := []string{""}
	if waits > 0 {
		verb := "wait"
		if waits == 1 {
			verb = "waits"
		}
		text := fmt.Sprintf("%d %s on others", waits, verb)
		waitParts = []string{st.Dim.Render(text)}
		if working > 0 {
			waitParts = append([]string{st.Dim.Render(text + fmt.Sprintf(", %d at work", working))}, waitParts...)
		}
	}
	var tail []string
	if alone > 0 {
		tail = append(tail, st.Dim.Render(fmt.Sprintf("%d left alone", alone)))
	}
	chip := ""
	if p.f.RuleChip != "" {
		chip = st.Warn.Render(p.f.RuleChip) + " "
	}
	// "1 done today" gives way to "1 done" when the rule chip would sit close
	// to it, or the line would be cut. A digest's segment takes the place of
	// the done count, until the next key.
	dones := []string{fmt.Sprintf("%d done today", done), fmt.Sprintf("%d done", done)}
	switch d := p.f.Digest; {
	case d != nil && d.Chip != "":
		dones, done = []string{st.Label.Render(d.Chip)}, 1
	case done > 0:
		dones = []string{st.Dim.Render(dones[0]), st.Dim.Render(dones[1])}
	}
	room := p.width
	if chip != "" {
		room -= ansi.StringWidth(chip) + factoryChipGap
	}
	left := ""
fit:
	for _, waitPart := range waitParts {
		for _, segment := range dones {
			all := slices.Clone(parts)
			if waitPart != "" {
				all = append(all, waitPart)
			}
			all = append(all, tail...)
			if done > 0 {
				all = append(all, segment)
			}
			left = " " + strings.Join(all, st.Dim.Render(p.g.sep))
			if ansi.StringWidth(left) <= room {
				break fit
			}
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

var factoryGroupLabels = [...]string{"NEEDS YOU", "WAITS ON OTHERS", "LEFT ALONE", "DONE TODAY"}

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
		if len(b.items) == 0 && g == GroupNeedsYou && len(p.rows) > 1 && p.f.all == nil { // a narrowed home may hide what needs you
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
	switch {
	case p.f.status != "": // the screen is loading, or could not read the factory
		text = truncate(p.f.status, p.width)
	case p.f.all != nil && p.f.filter != "": // the filter (and the scopes) hide every row
		in := ""
		if scope := cmp.Or(p.f.repo, p.f.owner); scope != "" {
			in = " in " + scope
		}
		text = p.st.Dim.Render(fmt.Sprintf("Nothing matches %q%s. ", p.f.filter, in)) + p.st.Key.Render("esc") +
			p.st.Dim.Render(" clears the filter.")
	case p.f.all != nil && p.f.owner != "": // the owner scope shows nothing: what O shows next
		next := cmp.Or(p.f.ownerNext, "every owner")
		text = p.st.Dim.Render("Nothing of "+p.f.owner+". ") + p.st.Key.Render("O") + p.st.Dim.Render(" shows "+next+".")
	case len(p.f.all) > 0 && p.f.repo != "": // the repository scope shows nothing
		text = p.st.Dim.Render("Nothing of " + p.f.repo + ".")
	}
	return spaces((p.width-ansi.StringWidth(text))/2) + text
}

func (p *factoryPainter) render(height int) (string, map[int64]int) {
	empty := len(p.rows) == 0 && !p.digestDone()
	narrowedEmpty := empty && p.f.all != nil && len(p.counted) > 0 // the chips still count what it hides
	bodyH := 0
	if height > 0 {
		bodyH = max(height-factoryTop-factoryBottom-len(p.f.ask), 1)
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
	if empty && !narrowedEmpty {
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
		r := p.rows[p.cursor]
		t := r.helpText(p.helpForm())
		if m := r.moveText(formShort); !p.lay.hasMove && m != "" { // the layout gave YOUR MOVE up: the help line says it
			t = strings.TrimPrefix(t+p.g.sep+"your move: "+m, p.g.sep)
		}
		if t != "" {
			help = truncate(" "+t, p.width)
		}
	}
	if p.f.input != "" { // the filter as it is typed
		help = truncate(" "+p.st.Accent.Render(p.f.input), p.width)
	}
	lines = append(lines, p.f.ask...)
	lines = append(lines, p.base.footerRule(p.width, msg, below), help, p.hintsLine())
	if height > 0 && len(lines) > height {
		lines = lines[:height]
	}
	for i, l := range lines {
		lines[i] = truncate(l, p.width)
	}
	return strings.Join(lines, "\n"), rowLines
}
