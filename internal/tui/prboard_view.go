package tui

import (
	"cmp"
	"fmt"
	"image/color"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// RenderPRBoard renders the board once, for output that is not
// interactive and for tests: the title bar, the state summary, the column
// headings and every row (in opts.DefaultSort order, largest first),
// fitted to width (0 = no limit). It uses the dark palette and no cursor;
// ages count from opts.Now.
func RenderPRBoard(rows []PRBoardRow, width int, opts PRBoardOptions) string {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Title == "" {
		opts.Title = defaultBoardTitle
	}
	by := opts.DefaultSort
	if !by.valid() {
		by = SortUpdated
	}
	view := opts.DefaultView
	if !view.valid() {
		view = ViewAll
	}
	scoped := scopeRows(rows, opts.Repo)
	st := defaultStyles
	p := newPRBPainter(st, newPRBPalette(st), newPRBGlyphs(opts.ASCII), opts.Now(), selfSet(opts.SelfLogins), scoped, by, true)
	p.judge = opts.Judge
	sorted := SortPRBoard(FilterPRBoard(scoped, view, opts.SelfLogins), by, true)
	lay := p.layout(scoped, width)
	w := width
	if w <= 0 {
		w = lay.total()
	}
	lines := []string{p.titleLine(w, opts.Title, opts.Repo, view, len(sorted), "", len(sorted), ""), p.summaryLine(w), p.headerLine(lay, w), p.rule(w, 0, false)}
	for _, r := range sorted {
		lines = append(lines, p.rowLine(r, lay, w, false))
	}
	if len(sorted) == 0 {
		lines = append(lines, "  "+st.Dim.Render("no open pull requests"))
	}
	return strings.Join(lines, "\n")
}

// prbGlyphs are the symbols the board draws, in Unicode or plain ASCII.
type prbGlyphs struct {
	approved, changes, commented, pending, dismissed, other string
	nonBlocking, simplify, times                            string
	stale, mine, pin, errMark, fail, ok                     string
	cursor, dash, minus, atLeast, rule, up, down, dot       string
	sortDesc, sortAsc, refresh, sep, chipSep                string
	spinner                                                 spinner.Spinner
}

func newPRBGlyphs(ascii bool) prbGlyphs {
	if ascii {
		return prbGlyphs{
			approved: "+", changes: "x", commented: "c", pending: "?", dismissed: "-", other: "*",
			stale: "~", mine: "*", pin: "pin", errMark: "!", fail: "x", ok: "+",
			nonBlocking: "~", simplify: "s", times: "x",
			cursor: ">", dash: "-", minus: "-", atLeast: ">=", rule: "-", up: "^", down: "v", dot: "",
			sortDesc: "v", sortAsc: "^", refresh: "at", sep: " | ", chipSep: ":",
			spinner: spinner.Line,
		}
	}
	return prbGlyphs{
		approved: "✔", changes: "✗", commented: "💬", pending: "◌", dismissed: "⊘", other: "•",
		stale: "⟳", mine: "★", pin: "📌", errMark: "!", fail: "✗", ok: "✔",
		nonBlocking: "●", simplify: "✂", times: "×",
		cursor: "▌", dash: "—", minus: "−", atLeast: "≥", rule: "─", up: "▲", down: "▼", dot: "●",
		sortDesc: "↓", sortAsc: "↑", refresh: "↻", sep: " · ",
		spinner: spinner.MiniDot,
	}
}

// prbPalette is the board's own looks on top of styles: state pills,
// verdict colors and the cursor row's background. 16 ANSI colors only.
type prbPalette struct {
	rule, mine, num, add, del, green, red, yellow, tag, sorted, bold lipgloss.Style

	ruleColor, dim, selBg color.Color
	pills, dots           map[string]lipgloss.Style
}

func newPRBPalette(st styles) prbPalette {
	pick := lipgloss.LightDark(st.dark)
	faint := pick(lipgloss.White, lipgloss.BrightBlack)
	dim := pick(lipgloss.BrightBlack, lipgloss.White)
	green := pick(lipgloss.Green, lipgloss.BrightGreen)
	red := pick(lipgloss.Red, lipgloss.BrightRed)
	yellow := pick(lipgloss.Yellow, lipgloss.BrightYellow)
	blue := pick(lipgloss.Blue, lipgloss.BrightBlue)
	magenta := pick(lipgloss.Magenta, lipgloss.BrightMagenta)
	fg := func(c color.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }
	// A pill is the state's color in reverse video: the color fills the
	// badge and the text takes the terminal's own background, which reads
	// on every theme (a fixed black or white text does not).
	pill := func(c color.Color) lipgloss.Style { return lipgloss.NewStyle().Bold(true).Foreground(c).Reverse(true) }
	yellowPill, redPill := pill(yellow), pill(red)
	gone := fg(dim).Italic(true)
	return prbPalette{
		rule: fg(faint), mine: fg(magenta), num: lipgloss.NewStyle().Bold(true),
		add: fg(green), del: fg(red), green: fg(green), red: fg(red), yellow: fg(yellow),
		tag:       fg(dim).Italic(true),
		sorted:    lipgloss.NewStyle().Bold(true).Foreground(st.Accent.GetForeground()),
		bold:      lipgloss.NewStyle().Bold(true),
		ruleColor: faint, dim: dim, selBg: faint,
		pills: map[string]lipgloss.Style{
			"queued": yellowPill, "rereview_pending": yellowPill,
			"reviewing":       pill(blue),
			"reviewed":        pill(green),
			"needs_attention": redPill, "paused": redPill,
			"baseline": fg(dim), "ineligible": gone, "ignored": gone, "closed": gone, "released": gone,
		},
		dots: map[string]lipgloss.Style{
			"queued": fg(yellow), "rereview_pending": fg(yellow), "reviewing": fg(blue), "reviewed": fg(green),
			"needs_attention": fg(red), "paused": fg(red),
			"baseline": fg(dim), "ineligible": fg(dim), "ignored": fg(dim), "closed": fg(dim), "released": fg(dim),
		},
	}
}

// prbStateLabels are the state names the board shows.
var prbStateLabels = map[string]string{
	"rereview_pending": "re-review", "needs_attention": "attention",
	// baseline: open before magnum began watching the repository (the card says what starts a review)
	"baseline": "not reviewed",
}

func stateLabel(state string) string {
	s := normState(state)
	if l, ok := prbStateLabels[s]; ok {
		return l
	}
	return strings.ReplaceAll(s, "_", " ")
}

// normVerdict folds GitHub's review events and magnum's verdicts into
// approved, changes_requested, commented, dismissed or pending.
func normVerdict(s string) string {
	v := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(s)))
	switch v {
	case "approved", "approve":
		return "approved"
	case "changes_requested", "request_changes", "changes":
		return "changes_requested"
	case "commented", "comment":
		return "commented"
	case "dismissed":
		return "dismissed"
	case "", "pending", "requested", "review_requested":
		return "pending"
	}
	return v
}

// verdictRank orders reviewers: blockers first, then approvals, comments,
// dismissed reviews and finally reviews still to come.
func verdictRank(v string) int {
	switch v {
	case "changes_requested":
		return 0
	case "approved":
		return 1
	case "commented":
		return 2
	case "dismissed":
		return 3
	case "pending":
		return 5
	}
	return 4
}

// seg is a run of text in one style. Cells are built from segs so a row
// overlay (the cursor's background, a muted row's dim) reaches every run:
// a style rendered around already styled text would end at its first reset.
type seg struct {
	text string
	st   lipgloss.Style
}

type cell []seg

func (c cell) width() int {
	w := 0
	for _, s := range c {
		w += ansi.StringWidth(s.text)
	}
	return w
}

// fit cuts c to w cells, ending in an ellipsis when it had to cut.
func (c cell) fit(w int) cell {
	if w <= 0 {
		return nil
	}
	if c.width() <= w {
		return c
	}
	room := w - 1
	var out cell
	var last lipgloss.Style
	for _, s := range c {
		last = s.st
		if sw := ansi.StringWidth(s.text); sw <= room {
			out = append(out, s)
			room -= sw
			continue
		}
		if cut := strings.TrimRight(ansi.Truncate(s.text, room, ""), " "); cut != "" {
			out = append(out, seg{cut, s.st})
		}
		break
	}
	return append(out, seg{"…", last})
}

// fitLeft cuts c to w cells from the LEFT, keeping its tail and starting
// with an ellipsis when it had to cut: a PR reference keeps "#N" and the end
// of a long repository name rather than losing the number.
func (c cell) fitLeft(w int) cell {
	if w <= 0 {
		return nil
	}
	if c.width() <= w {
		return c
	}
	room := w - 1
	var out cell
	var first lipgloss.Style
	for i := len(c) - 1; i >= 0; i-- {
		s := c[i]
		first = s.st
		if sw := ansi.StringWidth(s.text); sw <= room {
			out = append(cell{s}, out...)
			room -= sw
			continue
		}
		if room > 0 {
			if cut := strings.TrimLeft(ansi.TruncateLeft(s.text, ansi.StringWidth(s.text)-room, ""), " "); cut != "" {
				out = append(cell{{cut, s.st}}, out...)
			}
		}
		break
	}
	return append(cell{{"…", first}}, out...)
}

// render styles each run, passing its style through ov first (nil = as is).
func (c cell) render(ov func(lipgloss.Style) lipgloss.Style) string {
	var b strings.Builder
	for _, s := range c {
		if s.text == "" {
			continue
		}
		st := s.st
		if ov != nil {
			st = ov(st)
		}
		b.WriteString(st.Render(s.text))
	}
	return b.String()
}

func spaces(n int) string { return strings.Repeat(" ", max(n, 0)) }

// spread puts left and right on one line of width cells, right-aligned;
// left gives way first.
func spread(left, right string, width int) string {
	lw, rw := ansi.StringWidth(left), ansi.StringWidth(right)
	switch {
	case width <= 0:
		return left + "  " + right
	case rw == 0:
		return truncate(left, width)
	case lw+1+rw > width:
		if width-rw-1 < 12 {
			return truncate(left, width)
		}
		left = truncate(left, width-rw-1)
		lw = ansi.StringWidth(left)
	}
	return left + spaces(width-lw-rw) + right
}

// shortAge renders an age for a narrow column: "now", "7m", "3h", "2d",
// "3w", "5mo", "2y".
func shortAge(d time.Duration) string {
	const day = 24 * time.Hour
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < day:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d < 14*day:
		return fmt.Sprintf("%dd", int(d/day))
	case d < 60*day:
		return fmt.Sprintf("%dw", int(d/(7*day)))
	case d < 365*day:
		return fmt.Sprintf("%dmo", int(d/(30*day)))
	}
	return fmt.Sprintf("%dy", int(d/(365*day)))
}

// compactNum renders a count in at most four cells: "41", "1.2k", "12k",
// "1.3M".
func compactNum(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 10_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1000), ".0") + "k"
	case n < 1_000_000:
		return fmt.Sprintf("%dk", n/1000)
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1e6), ".0") + "M"
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// prbCol is a table column.
type prbCol int

const (
	colRef prbCol = iota // the repository (owner/ shown when the board spans several owners)
	colNum               // the PR number
	colTitle
	colAuthor
	colAssignee
	colUpdated
	colState
	colLastReview
	colFindings
	colSince
	colReviewers
	prbNumCols
)

var prbColTitles = [prbNumCols]string{"REPO", "#", "TITLE", "AUTHOR", "ASSIGNEE", "UPDATED", "STATE", "LAST REVIEW", "FINDINGS", "SINCE REVIEW", "REVIEWERS"}

var (
	// prbDropOrder is which columns give way, in turn, on a narrow screen.
	prbDropOrder = []prbCol{colAssignee, colSince, colAuthor, colUpdated, colLastReview, colReviewers, colFindings}
	// prbFlexMin is the narrowest the flexible columns get; the others
	// keep their content's width (up to prbCap).
	prbFlexMin = map[prbCol]int{colTitle: 18, colReviewers: 12}
	prbCap     = map[prbCol]int{colRef: 28, colNum: 7, colAuthor: 14, colAssignee: 14, colLastReview: 22, colFindings: 22, colReviewers: 44}
)

const prbMarkW = 2 // the cursor mark before each row

func prbRightAligned(c prbCol) bool { return c == colUpdated || c == colNum }

func sortColumn(s PRSort) prbCol {
	switch s {
	case SortLastReview:
		return colLastReview
	case SortReviewerActivity:
		return colReviewers
	case SortChanges:
		return colSince
	case SortState:
		return colState
	}
	return colUpdated
}

// prbPainter draws the board's parts for one frame.
type prbPainter struct {
	st      styles
	pal     prbPalette
	g       prbGlyphs
	now     time.Time
	self    map[string]bool
	all     []PRBoardRow // every row in scope: counts and column widths
	owners  bool         // rows span several owners, so refs name them
	anyMine bool         // some last review is mine: that column keeps room for ★
	sort    PRSort
	desc    bool
	judge   string // PRBoardOptions.Judge
}

func newPRBPainter(st styles, pal prbPalette, g prbGlyphs, now time.Time, self map[string]bool, all []PRBoardRow, by PRSort, desc bool) prbPainter {
	p := prbPainter{st: st, pal: pal, g: g, now: now, self: self, all: all, sort: by, desc: desc}
	owners := map[string]bool{}
	for _, r := range all {
		if o, _, _ := prRefParts(r); o != "" {
			owners[strings.ToLower(o)] = true
		}
		if r.LastReview != nil && p.isMine(r.LastReview.Login, r.LastReview.Mine) {
			p.anyMine = true
		}
	}
	p.owners = len(owners) > 1
	return p
}

// glyphW is the widest verdict glyph.
func (p prbPainter) glyphW() int {
	w := 0
	for _, g := range []string{p.g.approved, p.g.changes, p.g.commented, p.g.pending, p.g.dismissed, p.g.other} {
		w = max(w, ansi.StringWidth(g))
	}
	return w
}

func (p prbPainter) isMine(login string, mine bool) bool { return mine || p.self[normLogin(login)] }

func (p prbPainter) dash() cell { return cell{{p.g.dash, p.st.Dim}} }

func (p prbPainter) arrow() string {
	if p.desc {
		return p.g.sortDesc
	}
	return p.g.sortAsc
}

func (p prbPainter) colTitle(c prbCol) string {
	if c == sortColumn(p.sort) {
		return prbColTitles[c] + " " + p.arrow()
	}
	return prbColTitles[c]
}

// prbCells are one row's cells; the reviewers are chips joined only once
// the column's width is known.
type prbCells struct {
	c    [prbNumCols]cell
	revs []cell
}

func (p prbPainter) cells(r PRBoardRow, since [3]int) prbCells {
	var cs prbCells
	cs.c[colRef] = p.refCell(r)
	cs.c[colNum] = p.numCell(r)
	cs.c[colTitle] = p.titleCell(r)
	cs.c[colAuthor] = p.loginCell(r.Author)
	cs.c[colAssignee] = p.assigneeCell(r.Assignees)
	cs.c[colUpdated] = p.ageCell(r.UpdatedAt)
	cs.c[colState] = p.stateWaitCell(r)
	cs.c[colLastReview] = p.lastReviewCell(r.LastReview)
	cs.c[colFindings] = p.findingsCell(r.Findings)
	cs.c[colSince] = p.sinceCell(r.SinceReview, since)
	cs.revs = p.reviewerChips(r.Reviewers)
	return cs
}

func (p prbPainter) refCell(r PRBoardRow) cell {
	owner, repo, n := prRefParts(r)
	if repo == "" || n <= 0 {
		if ref := prRef(r); ref != "" {
			return cell{{ref, p.pal.num}}
		}
		return p.dash()
	}
	var c cell
	if p.owners && owner != "" {
		c = append(c, seg{owner + "/", p.st.Dim})
	}
	return append(c, seg{repo, p.st.Dim})
}

// numCell is the PR number, its own right-aligned column so a long
// repository name can never hide it.
func (p prbPainter) numCell(r PRBoardRow) cell {
	_, repo, n := prRefParts(r)
	if repo == "" || n <= 0 {
		return p.dash()
	}
	return cell{{"#" + strconv.Itoa(n), p.pal.num}}
}

// titleCell is the title after the row's tags: 📌 pinned, ! failed, draft,
// muted. The title is the last run (the cursor row bolds it).
func (p prbPainter) titleCell(r PRBoardRow) cell {
	var c cell
	if r.Pinned {
		pin := lipgloss.Style{}
		if p.g.pin == "pin" {
			pin = p.st.Accent
		}
		c = append(c, seg{p.g.pin + " ", pin})
	}
	if r.LastError != "" {
		c = append(c, seg{p.g.errMark + " ", p.st.Err})
	}
	if r.Draft {
		c = append(c, seg{"draft", p.pal.tag}, seg{" ", lipgloss.Style{}})
	}
	if r.Muted && normState(r.State) != "ignored" { // the state column says ignored
		c = append(c, seg{"muted", p.pal.tag}, seg{" ", lipgloss.Style{}})
	}
	title := oneLine(r.Title)
	if title == "" {
		return append(c, seg{"(no title)", p.st.Dim})
	}
	if normState(r.State) == "ignored" {
		return append(c, seg{title, lipgloss.NewStyle().Strikethrough(true)}) // greyed by the muted overlay
	}
	return append(c, seg{title, lipgloss.Style{}})
}

func (p prbPainter) loginStyle(login string, mine bool) lipgloss.Style {
	if p.isMine(login, mine) {
		return p.pal.mine
	}
	return lipgloss.Style{}
}

func (p prbPainter) loginCell(login string) cell {
	if strings.TrimSpace(login) == "" {
		return p.dash()
	}
	return cell{{shortLogin(login), p.loginStyle(login, false)}}
}

func (p prbPainter) assigneeCell(as []string) cell {
	if len(as) == 0 {
		return p.dash()
	}
	first := as[0]
	for _, a := range as { // me first
		if p.isMine(a, false) {
			first = a
			break
		}
	}
	c := cell{{shortLogin(first), p.loginStyle(first, false)}}
	if len(as) > 1 {
		c = append(c, seg{fmt.Sprintf(" +%d", len(as)-1), p.st.Dim})
	}
	return c
}

func (p prbPainter) ageCell(t time.Time) cell {
	if t.IsZero() {
		return p.dash()
	}
	d := max(p.now.Sub(t), 0)
	st := lipgloss.Style{}
	if d >= 24*time.Hour {
		st = p.st.Dim
	}
	return cell{{shortAge(d), st}}
}

// stateWaitCell is the state pill followed, for a PR waiting for a round,
// by what holds it and until when, dimmed ("quiet → 14:09").
func (p prbPainter) stateWaitCell(r PRBoardRow) cell {
	c := p.stateCell(r.State)
	if _, rest, ok := strings.Cut(r.Wait, " · "); ok && rest != "" {
		c = append(c, seg{" " + rest, p.st.Dim})
	}
	return c
}

func (p prbPainter) stateCell(state string) cell {
	s := normState(state)
	if s == "" {
		return p.dash()
	}
	return cell{{" " + stateLabel(s) + " ", p.pal.pills[s]}}
}

// findingsCell is the latest review's verdict glyph, its findings by
// priority ("P1 P2×3") and its simplifications ("✂4"): what the review
// concluded, also where it could only comment.
func (p prbPainter) findingsCell(f *FindingsInfo) cell {
	if f == nil {
		return p.dash()
	}
	var c cell
	switch f.Verdict {
	case "blocking":
		c = append(c, seg{p.g.changes + " ", p.pal.red})
	case "non_blocking":
		c = append(c, seg{p.g.nonBlocking + " ", p.pal.yellow})
	default:
		c = append(c, seg{p.g.ok + " ", p.pal.green})
	}
	styles := [4]lipgloss.Style{p.pal.red, p.pal.red, p.pal.yellow, p.st.Dim}
	listed := false
	for i, n := range f.Counts {
		if n <= 0 {
			continue
		}
		if listed {
			c = append(c, seg{" ", lipgloss.Style{}})
		}
		listed = true
		t := fmt.Sprintf("P%d", i)
		if n > 1 {
			t += p.g.times + strconv.Itoa(n)
		}
		c = append(c, seg{t, styles[i]})
	}
	if !listed {
		c = append(c, seg{"clean", p.pal.green})
	}
	if f.Simplifications > 0 {
		c = append(c, seg{" " + p.g.simplify + strconv.Itoa(f.Simplifications), p.st.Dim})
	}
	return c
}

// verdict is a verdict's glyph, its short and long names and its color.
func (p prbPainter) verdict(v string) (glyph, short, long string, st lipgloss.Style) {
	switch v {
	case "approved":
		return p.g.approved, "approved", "approved", p.pal.green
	case "changes_requested":
		return p.g.changes, "changes", "changes requested", p.pal.red
	case "commented":
		return p.g.commented, "commented", "commented", p.pal.yellow
	case "dismissed":
		return p.g.dismissed, "dismissed", "dismissed", p.st.Dim
	case "pending":
		return p.g.pending, "pending", "pending", p.st.Dim
	}
	return p.g.other, v, v, lipgloss.Style{}
}

// lastReviewCell is "★ ✔ approved 2h": whose (★ mine), the verdict and its
// age; a stale review (the head moved since) is dimmed and marked ⟳.
func (p prbPainter) lastReviewCell(li *ReviewInfo) cell {
	if li == nil {
		return p.dash()
	}
	glyph, short, _, vst := p.verdict(normVerdict(li.Event))
	if li.Stale {
		vst = p.st.Dim
	}
	var c cell
	switch {
	case p.isMine(li.Login, li.Mine):
		mst := p.pal.mine
		if li.Stale {
			mst = p.st.Dim
		}
		c = append(c, seg{p.g.mine + " ", mst})
	case p.anyMine:
		c = append(c, seg{spaces(ansi.StringWidth(p.g.mine) + 1), lipgloss.Style{}})
	}
	// Glyphs differ in width (💬 takes two cells): pad so the names align.
	c = append(c, seg{glyph + spaces(p.glyphW()-ansi.StringWidth(glyph)), vst}, seg{" " + short, vst})
	if !li.SubmittedAt.IsZero() {
		c = append(c, seg{" " + shortAge(max(p.now.Sub(li.SubmittedAt), 0)), p.st.Dim})
	}
	if li.Stale {
		c = append(c, seg{" " + p.g.stale, p.pal.yellow})
	}
	return c
}

// sinceParts are the commits, additions and deletions of a delta.
func (p prbPainter) sinceParts(d *ReviewDelta) (commits, adds, dels string) {
	commits = compactNum(d.Commits) + "c"
	if d.Truncated {
		commits = p.g.atLeast + commits
	}
	return commits, "+" + compactNum(d.Additions), p.g.minus + compactNum(d.Deletions)
}

// sinceWidths are the widest commits, additions and deletions parts, so
// the numbers line up across rows.
func (p prbPainter) sinceWidths(rows []PRBoardRow) [3]int {
	var w [3]int
	for _, r := range rows {
		if r.SinceReview == nil {
			continue
		}
		c, a, d := p.sinceParts(r.SinceReview)
		w[0], w[1], w[2] = max(w[0], ansi.StringWidth(c)), max(w[1], ansi.StringWidth(a)), max(w[2], ansi.StringWidth(d))
	}
	return w
}

// sinceCell is "3c +41 −7" since the last review, right-aligned part by
// part; against the base branch (no review yet) it is dimmed.
func (p prbPainter) sinceCell(d *ReviewDelta, w [3]int) cell {
	if d == nil {
		return p.dash()
	}
	c, a, del := p.sinceParts(d)
	cst, ast, dst := lipgloss.Style{}, p.pal.add, p.pal.del
	if d.Commits == 0 {
		cst = p.st.Dim
	}
	if d.Additions == 0 {
		ast = p.st.Dim
	}
	if d.Deletions == 0 {
		dst = p.st.Dim
	}
	if isBaseDelta(d) {
		cst, ast, dst = p.st.Dim, p.st.Dim, p.st.Dim
	}
	padL := func(s string, n int) string { return spaces(n-ansi.StringWidth(s)) + s }
	return cell{{padL(c, w[0]), cst}, {" " + padL(a, w[1]), ast}, {" " + padL(del, w[2]), dst}}
}

func isBaseDelta(d *ReviewDelta) bool {
	return strings.Contains(strings.ToLower(d.Base), "base")
}

// reviewerChips are "login✔" chips: mine first (★), then blockers,
// approvals, comments, dismissals and requested reviews (◌).
func (p prbPainter) reviewerChips(list []ReviewerInfo) []cell {
	revs := slices.Clone(list)
	slices.SortStableFunc(revs, func(a, b ReviewerInfo) int {
		ma, mb := p.isMine(a.Login, a.Mine), p.isMine(b.Login, b.Mine)
		if ma != mb {
			if ma {
				return -1
			}
			return 1
		}
		if c := cmp.Compare(verdictRank(normVerdict(a.Verdict)), verdictRank(normVerdict(b.Verdict))); c != 0 {
			return c
		}
		return cmp.Compare(strings.ToLower(a.Login), strings.ToLower(b.Login))
	})
	chips := make([]cell, 0, len(revs))
	for _, v := range revs {
		verdict := normVerdict(v.Verdict)
		glyph, _, _, vst := p.verdict(verdict)
		mine := p.isMine(v.Login, v.Mine)
		lst := p.loginStyle(v.Login, v.Mine)
		if verdict == "pending" || v.Stale {
			lst, vst = p.st.Dim, p.st.Dim
		}
		var c cell
		if mine {
			c = append(c, seg{p.g.mine, lst})
		}
		c = append(c, seg{shortLogin(v.Login), lst}, seg{p.g.chipSep + glyph, vst})
		if v.Stale {
			c = append(c, seg{p.g.stale, p.pal.yellow})
		}
		chips = append(chips, c)
	}
	return chips
}

func chipsWidth(chips []cell) int {
	w := max(len(chips)-1, 0)
	for _, c := range chips {
		w += c.width()
	}
	return w
}

// fitChips joins as many chips as fit in w cells and counts the rest as
// "+N".
func (p prbPainter) fitChips(chips []cell, w int) cell {
	if len(chips) == 0 {
		return p.dash()
	}
	var out cell
	used := 0
	for i, ch := range chips {
		sep := min(i, 1)
		more := 0
		if rest := len(chips) - i - 1; rest > 0 {
			more = 1 + len("+"+strconv.Itoa(rest))
		}
		if used+sep+ch.width()+more <= w {
			if sep > 0 {
				out = append(out, seg{" ", lipgloss.Style{}})
			}
			out = append(out, ch...)
			used += sep + ch.width()
			continue
		}
		left := len(chips) - i
		if i == 0 { // not even one chip fits: cut it
			if left == 1 {
				return ch.fit(w)
			}
			tag := " +" + strconv.Itoa(left-1)
			return append(ch.fit(max(w-len(tag), 1)), seg{tag, p.st.Dim})
		}
		return append(out, seg{" +" + strconv.Itoa(left), p.st.Dim})
	}
	return out
}

// prbLayout is which columns show and how wide they are, and how wide a
// drag may make each (the most it can take before a column would give way).
type prbLayout struct {
	cols   []prbCol
	widths []int
	maxW   []int
	since  [3]int
}

func (l prbLayout) total() int {
	t := prbMarkW + len(colGap)*max(len(l.cols)-1, 0)
	for _, w := range l.widths {
		t += w
	}
	return t
}

// prbWidths are the column widths the user dragged; 0 is automatic.
type prbWidths [prbNumCols]int

// prbColNames name the columns for ColumnWidths.
var prbColNames = [prbNumCols]string{"repo", "num", "title", "author", "assignee", "updated", "state", "last_review", "findings", "since_review", "reviewers"}

// prbWidthsFrom reads kept widths by column name.
func prbWidthsFrom(m map[string]int) prbWidths {
	var w prbWidths
	for c, name := range prbColNames {
		if v := m[name]; v > 0 {
			w[c] = max(v, minColWidth)
		}
	}
	return w
}

// named is w by column name, for ColumnWidths.
func (w prbWidths) named() map[string]int {
	out := map[string]int{}
	for c, v := range w {
		if v > 0 {
			out[prbColNames[c]] = v
		}
	}
	return out
}

// prbNatural is what the layout needs from the rows: each column's
// content width (capped) and the since-review parts' widths.
type prbNatural struct {
	nat   [prbNumCols]int
	since [3]int
}

// natural measures rows; it is the costly part of a layout.
func (p prbPainter) natural(rows []PRBoardRow) prbNatural {
	n := prbNatural{since: p.sinceWidths(rows)}
	for c := range prbNumCols {
		n.nat[c] = ansi.StringWidth(p.colTitle(c))
	}
	for _, r := range rows {
		cs := p.cells(r, n.since)
		for c := range prbNumCols {
			w := cs.c[c].width()
			if c == colReviewers {
				w = chipsWidth(cs.revs)
			}
			n.nat[c] = max(n.nat[c], w)
		}
	}
	for c, limit := range prbCap {
		n.nat[c] = min(n.nat[c], max(limit, ansi.StringWidth(p.colTitle(c))))
	}
	return n
}

// layout sizes the columns to rows' content (see fit).
func (p prbPainter) layout(rows []PRBoardRow, width int) prbLayout {
	return p.fit(p.natural(rows), width, prbWidths{})
}

// fit sizes the columns to their content and hides the least important
// ones (assignee, since review, author, ...) until the table fits width
// (0 = no limit). Extra room goes to the title and the reviewers. A
// dragged width (over) replaces a column's content width; the title and
// the reviewers still give way down to their minimum on a narrow screen.
func (p prbPainter) fit(n prbNatural, width int, over prbWidths) prbLayout {
	lay := prbLayout{since: n.since}
	nat := n.nat
	for c, v := range over {
		if v > 0 {
			nat[c] = max(v, minColWidth)
			if width > 0 {
				nat[c] = min(nat[c], max(width-prbMarkW, minColWidth))
			}
		}
	}
	minW := func(c prbCol) int {
		if f, ok := prbFlexMin[c]; ok {
			return min(nat[c], f)
		}
		return nat[c]
	}
	need := func(cols []prbCol) int {
		t := prbMarkW + len(colGap)*(len(cols)-1)
		for _, c := range cols {
			t += minW(c)
		}
		return t
	}
	cols := []prbCol{colRef, colNum, colTitle, colAuthor, colAssignee, colUpdated, colState, colLastReview, colFindings, colSince, colReviewers}
	for _, d := range prbDropOrder {
		if width <= 0 || need(cols) <= width {
			break
		}
		cols = slices.DeleteFunc(cols, func(c prbCol) bool { return c == d })
	}
	lay.cols = cols
	lay.widths = make([]int, len(cols))
	lay.maxW = make([]int, len(cols))
	total := need(cols)
	for i, c := range cols {
		lay.widths[i] = nat[c]
		lay.maxW[i] = nat[c]
		if width <= 0 {
			continue
		}
		lay.widths[i] = minW(c)
		if _, flex := prbFlexMin[c]; flex {
			lay.maxW[i] = max(width-prbMarkW, minColWidth)
		} else {
			lay.maxW[i] = max(width-(total-minW(c)), minColWidth)
		}
	}
	if width <= 0 {
		return lay
	}
	extra := width - total
	want := func(c prbCol) int {
		if i := slices.Index(cols, c); i >= 0 {
			return nat[c] - lay.widths[i]
		}
		return 0
	}
	give := func(c prbCol, n int) {
		if i := slices.Index(cols, c); i >= 0 && n > 0 {
			lay.widths[i] += n
			extra -= n
		}
	}
	give(colReviewers, min(want(colReviewers), extra/3))
	give(colTitle, min(want(colTitle), extra))
	give(colReviewers, min(want(colReviewers), extra))
	return lay
}

// colAt is the column of lay at x on the heading line (see colAtWidths).
func (l prbLayout) colAt(x int) (i int, gap bool) { return colAtWidths(l.widths, prbMarkW, x) }

func (p prbPainter) headerLine(lay prbLayout, width int) string {
	var b strings.Builder
	b.WriteString(spaces(prbMarkW))
	for i, c := range lay.cols {
		w := lay.widths[i]
		t := truncate(p.colTitle(c), w)
		st := p.st.Header
		if c == sortColumn(p.sort) {
			st = p.pal.sorted
		}
		pad := spaces(w - ansi.StringWidth(t))
		if i > 0 {
			b.WriteString(colGap)
		}
		switch {
		case prbRightAligned(c):
			b.WriteString(pad + st.Render(t))
		case i < len(lay.cols)-1:
			b.WriteString(st.Render(t) + pad)
		default:
			b.WriteString(st.Render(t))
		}
	}
	return truncate(b.String(), width)
}

// overlay is the cursor row's background and a muted row's dim, laid over
// every run of the row.
func (p prbPainter) overlay(selected, muted bool) func(lipgloss.Style) lipgloss.Style {
	if !selected && !muted {
		return nil
	}
	return func(s lipgloss.Style) lipgloss.Style {
		if muted {
			s = s.UnsetBackground().UnsetReverse().Foreground(p.pal.dim)
		}
		if _, none := s.GetBackground().(lipgloss.NoColor); selected && none && !s.GetReverse() {
			s = s.Background(p.pal.selBg)
		}
		return s
	}
}

// rowLine renders one PR; the cursor row gets the mark, a background to
// the edge and a bold title.
func (p prbPainter) rowLine(r PRBoardRow, lay prbLayout, width int, selected bool) string {
	cs := p.cells(r, lay.since)
	line := cell{{spaces(prbMarkW), lipgloss.Style{}}}
	if selected {
		line = cell{{p.g.cursor + spaces(prbMarkW-ansi.StringWidth(p.g.cursor)), p.st.Accent}}
	}
	for i, c := range lay.cols {
		w := lay.widths[i]
		var cl cell
		switch c {
		case colReviewers:
			cl = p.fitChips(cs.revs, w)
		case colRef:
			cl = cs.c[c].fitLeft(w) // keep the distinctive end of a long repository name
		default:
			cl = cs.c[c].fit(w)
		}
		if selected && c == colTitle {
			cl = slices.Clone(cl)
			for j := range cl {
				if _, plain := cl[j].st.GetForeground().(lipgloss.NoColor); plain {
					cl[j].st = cl[j].st.Bold(true)
				}
			}
		}
		if i > 0 {
			line = append(line, seg{colGap, lipgloss.Style{}})
		}
		pad := seg{spaces(w - cl.width()), lipgloss.Style{}}
		switch {
		case prbRightAligned(c):
			line = append(append(line, pad), cl...)
		case i < len(lay.cols)-1 || selected:
			line = append(append(line, cl...), pad)
		default:
			line = append(line, cl...)
		}
	}
	if width > 0 {
		if selected {
			line = append(line, seg{spaces(width - line.width()), lipgloss.Style{}})
		}
		line = line.fit(width)
	}
	return line.render(p.overlay(selected, r.Muted))
}

// rule is a full-width separator; n > 0 marks "▲ n more" (up) or "▼ n
// more" near its right end.
func (p prbPainter) rule(width, n int, up bool) string {
	if n <= 0 {
		return p.pal.rule.Render(strings.Repeat(p.g.rule, max(width, 1)))
	}
	arrow := p.g.down
	if up {
		arrow = p.g.up
	}
	tag := fmt.Sprintf(" %s %d more ", arrow, n)
	tail := min(2, max(width-ansi.StringWidth(tag), 0))
	head := max(width-ansi.StringWidth(tag)-tail, 0)
	return p.pal.rule.Render(strings.Repeat(p.g.rule, head)) + p.st.Dim.Render(tag) + p.pal.rule.Render(strings.Repeat(p.g.rule, tail))
}

// footerRule is the rule above the key hints, carrying msg (already
// styled) on its left and the rows below the screen on its right.
func (p prbPainter) footerRule(width int, msg string, below int) string {
	if msg == "" {
		return p.rule(width, below, false)
	}
	tag := ""
	if below > 0 {
		tag = p.st.Dim.Render(fmt.Sprintf(" %s %d more ", p.g.down, below)) + p.pal.rule.Render(strings.Repeat(p.g.rule, 2))
	}
	lead := p.pal.rule.Render(strings.Repeat(p.g.rule, 2)) + " "
	room := width - ansi.StringWidth(lead) - ansi.StringWidth(tag) - 1
	msg = truncate(msg, max(room, 1))
	fill := room - ansi.StringWidth(msg)
	return truncate(lead+msg+" "+p.pal.rule.Render(strings.Repeat(p.g.rule, max(fill, 0)))+tag, width)
}

// titleLine is the title bar: the title, the repository scope, the open
// count, the view (with inView, its row count), the sort and the filter,
// with right (the refresh time) on the right.
func (p prbPainter) titleLine(width int, title, repo string, view PRView, inView int, filter string, shown int, right string) string {
	scope := "all repos"
	if repo != "" {
		scope = repo
	}
	open := 0
	for _, r := range p.all {
		if isOpen(r) {
			open++
		}
	}
	sorted := p.st.Accent.Render(p.sort.label() + " " + p.arrow())
	match := ""
	if filter != "" {
		match = p.st.Accent.Render(fmt.Sprintf("%d match %q", shown, filter))
	}
	if right != "" {
		right += " "
	}
	// From the fullest to the barest: narrow screens drop the "all repos"
	// scope, then the "sorted by" words, before anything is cut.
	var left string
	for _, full := range []bool{true, false} {
		parts := []string{p.st.Title.Render(title)}
		if full || repo != "" {
			parts = append(parts, scope)
		}
		parts = append(parts, p.pal.bold.Render(strconv.Itoa(open))+" open")
		viewName := p.st.Accent.Render(string(view))
		if view != ViewAll {
			viewName += " " + p.pal.bold.Render(strconv.Itoa(inView))
		}
		if full {
			parts = append(parts, p.st.Dim.Render("view ")+viewName)
		} else {
			parts = append(parts, viewName)
		}
		if full {
			parts = append(parts, p.st.Dim.Render("sorted by ")+sorted)
		} else {
			parts = append(parts, sorted)
		}
		if match != "" {
			parts = append(parts, match)
		}
		left = " " + strings.Join(parts, p.st.Dim.Render(p.g.sep))
		if width <= 0 || ansi.StringWidth(left)+1+ansi.StringWidth(right) <= width {
			break
		}
	}
	return spread(left, right, width)
}

// summaryLine counts the rows per state, most urgent first, with the
// stale reviews, failures and pins on the right.
func (p prbPainter) summaryLine(width int) string {
	if len(p.all) == 0 {
		return " " + p.st.Dim.Render("no pull requests yet")
	}
	counts := map[string]int{}
	var stale, failing, pinned int
	for _, r := range p.all {
		counts[normState(r.State)]++
		if r.LastReview != nil && r.LastReview.Stale {
			stale++
		}
		if r.LastError != "" {
			failing++
		}
		if r.Pinned {
			pinned++
		}
	}
	var chips []string
	chip := func(dot lipgloss.Style, n int, label string) {
		s := p.pal.bold.Render(strconv.Itoa(n)) + " " + p.st.Dim.Render(label)
		if p.g.dot != "" {
			s = dot.Render(p.g.dot) + " " + s
		}
		chips = append(chips, s)
	}
	known := 0
	for _, s := range prStateOrder {
		if n := counts[s]; n > 0 {
			chip(p.pal.dots[s], n, stateLabel(s))
			known += n
		}
	}
	if other := len(p.all) - known; other > 0 {
		chip(p.st.Dim, other, "other")
	}
	var right []string
	if stale > 0 {
		right = append(right, p.pal.yellow.Render(p.g.stale)+" "+p.pal.bold.Render(strconv.Itoa(stale))+" "+p.st.Dim.Render("stale"))
	}
	if failing > 0 {
		right = append(right, p.st.Err.Render(p.g.errMark)+" "+p.pal.bold.Render(strconv.Itoa(failing))+" "+p.st.Dim.Render("failing"))
	}
	if pinned > 0 {
		right = append(right, p.g.pin+" "+p.pal.bold.Render(strconv.Itoa(pinned))+" "+p.st.Dim.Render("pinned"))
	}
	r := strings.Join(right, "   ")
	if r != "" {
		r += " "
	}
	// Whole chips only: the ones that do not fit become "…".
	room := width - ansi.StringWidth(r) - 2
	left := ""
	for i, c := range chips {
		next := left + "   " + c
		if i == 0 {
			next = " " + c
		}
		reserve := 0 // room for the "…" when more chips follow
		if i < len(chips)-1 {
			reserve = 3
		}
		if width > 0 && ansi.StringWidth(next)+reserve > room {
			left += p.st.Dim.Render("  …")
			break
		}
		left = next
	}
	return spread(left, r, width)
}

// flow packs styled items into lines of at most width cells, sep
// between them on a line.
func flow(items []string, sep string, width int) []string {
	var lines []string
	cur := ""
	for _, it := range items {
		switch next := cur + sep + it; {
		case cur == "":
			cur = it
		case ansi.StringWidth(next) <= width:
			cur = next
		default:
			lines = append(lines, cur)
			cur = it
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}
