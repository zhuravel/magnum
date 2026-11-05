package tui

// The factory's cards (spec 9.C): the question card and its recap, the ready
// card (factory_card_ready.go), the task card and the new task form. Like the
// home painter (factory_view.go) they hold no state and read no clock: data
// in, frame out.
//
// A card is a head (the title line, the facts line and a blank), a body, the
// rule that ends the body and the footer: the key hints (the card's own unless
// its data gives them), the y/N question being asked (on up to three lines
// when it does not fit one, so its words are not cut), or the reasons a key
// asks for. With a height the footer sits at the bottom of it and
// a body that does not fit is cut, the rule counting the lines below; without
// one the card is as tall as its content. Everything a card shows is cleaned
// first, and no line is wider than the screen.
//
// The tables line their columns up from the data, five cells apart, and the
// tags on the right ("recommended") end five cells from the edge.

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const (
	cardGap        = 5  // between the columns of a card's tables
	cardTagMargin  = 5  // the room right of a tag such as "recommended"
	cardWrapMargin = 7  // the room kept right of wrapped text (the ready card's PROOF wraps where its mockup does)
	cardInputLines = 6  // the lines of the new task text that are drawn: the last ones
	cardReadyX     = 14 // the text of the ready card starts here, behind its labels
	cardFormX      = 11 // ... and the text of the new task form
	cardNameMax    = 20 // the widest name column of the task card
	askMaxLines    = 3  // the most lines a y/N question takes
	cardMore       = "…"
)

// cardPainter draws one card. It borrows the home painter's glyphs and
// helpers (the tick, the marks, the spinner, who-styles) and its rule.
type cardPainter struct{ *factoryPainter }

func newCardPainter(width int, st styles, g glyphs, now time.Time) *cardPainter {
	if width <= 0 {
		width = factoryDefaultW
	}
	return &cardPainter{&factoryPainter{st: st, g: g, now: now, width: width, cursor: -1,
		base: prbPainter{st: st, pal: newPRBPalette(st), g: g}}}
}

// renderQuestionCard draws one question of the batch the agents asked before
// coding, in width cells and at most height lines (height <= 0: as many as it
// needs; width <= 0: 140). The recommended option is marked and its reason is
// under it; with none recommended nothing is marked.
func renderQuestionCard(c QuestionCard, width, height int, st styles, g glyphs, now time.Time) string {
	return newCardPainter(width, st, g, now).questionCard(cleanQuestionCard(c), height)
}

// renderQuestionRecap draws the answers of the batch for the one y that sends
// them: what each answer is and where it came from, what the agents get and
// what the operator is left to post.
func renderQuestionRecap(c QuestionCard, width, height int, st styles, g glyphs, now time.Time) string {
	return newCardPainter(width, st, g, now).recap(cleanQuestionCard(c), height)
}

// renderReadyCard draws a finished task for the operator's decision. The
// recommendation shows only once c.DiffOpened is set.
func renderReadyCard(c ReadyCard, width, height int, st styles, g glyphs, now time.Time) string {
	return newCardPainter(width, st, g, now).ready(cleanReadyCard(c), height)
}

// renderTaskCard draws a task's card. The title, the facts and what the task
// waits for stay pinned; the rest scrolls: scroll is the first of its lines
// drawn, moved just far enough to keep the line under the mark in view. below
// is how many of its lines are cut off under the screen.
func renderTaskCard(c TaskCard, width, height, scroll int, st styles, g glyphs, now time.Time) (frame string, below int) {
	return newCardPainter(width, st, g, now).task(cleanTaskCard(c), height, scroll)
}

// renderNewTask draws the new task screen: the text being typed, what the
// factory understood of it, and the question that starts the task.
func renderNewTask(f NewTaskForm, width, height int, st styles, g glyphs, now time.Time) string {
	return newCardPainter(width, st, g, now).newTask(cleanNewTaskForm(f), height)
}

// cleaning: the caller's data is copied, never changed

func cleanLines(s string) []string {
	var out []string
	for l := range strings.SplitSeq(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		out = append(out, cleanText(l))
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// cleanInput is typed text with its escape sequences and control characters
// taken out and everything else kept: the spaces, the indents and the line
// breaks (also the one at the end) are what the operator typed.
func cleanInput(s string) string {
	s = strings.ReplaceAll(ansi.Strip(s), "\t", "    ")
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	return strings.Map(func(r rune) rune {
		if r != '\n' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0)) {
			return ' '
		}
		return r
	}, s)
}

// orHints are hs, or def when hs has none: a card's hints are its data's,
// else its own.
func orHints(hs, def []hint) []hint {
	if len(hs) == 0 {
		return def
	}
	return hs
}

// cleanHints are key hints with their text cleaned; nil stays nil.
func cleanHints(hs []hint) []hint {
	if hs == nil {
		return nil
	}
	out := make([]hint, len(hs))
	for i, h := range hs {
		out[i] = hint{cleanText(h.key), cleanText(h.desc)}
	}
	return out
}

func cleanQuestionCard(c QuestionCard) QuestionCard {
	c.TaskKey, c.Title, c.Kind = cleanText(c.TaskKey), cleanText(c.Title), cleanText(c.Kind)
	c.NeedlessReasons, c.Hints = cleanAll(c.NeedlessReasons), cleanHints(c.Hints)
	qs := make([]CardQuestion, len(c.Questions))
	for i, q := range c.Questions {
		q.Text, q.Context, q.Example = cleanText(q.Text), cleanText(q.Context), cleanText(q.Example)
		q.Why, q.Answer, q.Asks = cleanText(q.Why), cleanText(q.Answer), cleanText(q.Asks)
		q.Comment = strings.Join(cleanLines(q.Comment), "\n")
		q.Options = slices.Clone(q.Options)
		for j, o := range q.Options {
			q.Options[j] = CardOption{Label: cleanText(o.Label), Means: cleanText(o.Means), Send: cleanText(o.Send), Own: o.Own}
		}
		qs[i] = q
	}
	c.Questions = qs
	return c
}

func cleanReadyCard(c ReadyCard) ReadyCard {
	c.TaskKey, c.Title, c.Branch, c.Kind = cleanText(c.TaskKey), cleanText(c.Title), cleanText(c.Branch), cleanText(c.Kind)
	c.Cause, c.Fix, c.Proof, c.Why = cleanText(c.Cause), cleanText(c.Fix), cleanText(c.Proof), cleanText(c.Why)
	c.ScreenshotsNote, c.Ask, c.Frees = cleanText(c.ScreenshotsNote), cleanText(c.Ask), cleanText(c.Frees)
	c.Doubts, c.Flags, c.Leftovers = cleanAll(c.Doubts), cleanAll(c.Flags), cleanAll(c.Leftovers)
	c.Unread, c.Reasons, c.Hints = cleanAll(c.Unread), cleanAll(c.Reasons), cleanHints(c.Hints)
	c.Copy, c.NoPush, c.Failed = cleanText(c.Copy), cleanText(c.NoPush), cleanAll(c.Failed)
	c.Files = slices.Clone(c.Files)
	for i := range c.Files {
		c.Files[i].Path = cleanText(c.Files[i].Path)
	}
	return c
}

func cleanTaskCard(c TaskCard) TaskCard {
	c.Key, c.Kind, c.Title, c.Branch = cleanText(c.Key), cleanText(c.Kind), cleanText(c.Title), cleanText(c.Branch)
	c.Copy, c.WaitingFor, c.PR, c.AgentsNote = cleanText(c.Copy), cleanText(c.WaitingFor), cleanText(c.PR), cleanText(c.AgentsNote)
	c.Ask = cleanText(c.Ask)
	c.Report, c.Learned, c.Hints = cleanAll(c.Report), cleanAll(c.Learned), cleanHints(c.Hints)
	c.Stages = slices.Clone(c.Stages)
	for i, s := range c.Stages {
		c.Stages[i].Name, c.Stages[i].State, c.Stages[i].Proof = cleanText(s.Name), cleanText(s.State), cleanText(s.Proof)
	}
	c.Agents = slices.Clone(c.Agents)
	for i, a := range c.Agents {
		c.Agents[i].Name, c.Agents[i].Why = cleanText(a.Name), cleanText(a.Why)
		c.Agents[i].Now, c.Agents[i].State = cleanText(a.Now), cleanText(a.State)
	}
	return c
}

func cleanNewTaskForm(f NewTaskForm) NewTaskForm {
	f.Text = cleanInput(f.Text)
	f.Kind, f.KindWhy, f.Branch, f.Starts = cleanText(f.Kind), cleanText(f.KindWhy), cleanText(f.Branch), cleanText(f.Starts)
	f.Err, f.Repo, f.Title, f.Ask = cleanText(f.Err), cleanText(f.Repo), cleanText(f.Title), cleanText(f.Ask)
	f.Agents, f.Rules, f.Keys = cleanAll(f.Agents), cleanAll(f.Rules), cleanAll(f.Keys)
	f.Overlap, f.Guidance, f.Hints = cleanText(f.Overlap), cleanText(f.Guidance), cleanHints(f.Hints)
	f.Links = slices.Clone(f.Links)
	for i, l := range f.Links {
		f.Links[i] = FormLink{cleanText(l.Kind), cleanText(l.URL), cleanText(l.Offer)}
	}
	if f.Jira != nil {
		j := *f.Jira
		j.Key, j.Type, j.Status, j.Priority = cleanText(j.Key), cleanText(j.Type), cleanText(j.Status), cleanText(j.Priority)
		j.Reporter, j.Summary, j.Excerpt, j.Found = cleanText(j.Reporter), cleanText(j.Summary), cleanText(j.Excerpt), cleanText(j.Found)
		f.Jira = &j
	}
	return f
}

// words and times

// agoText is how long ago something was: "2s ago", "41m ago".
func agoText(d time.Duration) string {
	switch d = max(d, 0); {
	case d < time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d/time.Second))
	}
	return shortAge(d) + " ago"
}

// tookText is how long a stage took: "0s", "24m", "1h11m", "3h".
func tookText(d time.Duration) string {
	switch d = max(d, 0); {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%02dm", h, m)
}

// joinAnd is "1", "1 and 2" or "1, 2 and 3".
func joinAnd(ss []string) string {
	switch len(ss) {
	case 0:
		return ""
	case 1:
		return ss[0]
	}
	return strings.Join(ss[:len(ss)-1], ", ") + " and " + ss[len(ss)-1]
}

// upperFirst puts the first letter of s in capitals.
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// wrapText word-wraps plain text to width cells. With maxLines > 0 the rest is
// left out, the last line ending in an ellipsis.
func wrapText(s string, width, maxLines int) []string {
	if s = strings.TrimSpace(s); s == "" {
		return nil
	}
	lines := cleanupWrap(s, max(width, 8))
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[:maxLines]
		lines[maxLines-1] = strings.TrimRight(lines[maxLines-1], " ") + cardMore
	}
	return lines
}

func (p *cardPainter) clock(t time.Time) string { return t.In(p.now.Location()).Format("15:04") }

// wrapW is the width wrapped text gets from column x on.
func (p *cardPainter) wrapW(x int) int { return max(p.width-x-cardWrapMargin, 12) }

// pieces and lines

func at(x, w int, segs ...seg) placed { return placed{x, w, cell(segs)} }

// rightAt places c so that it ends at the cell edge.
func rightAt(edge int, c cell) placed {
	w := c.width()
	return placed{edge - w, w, c}
}

// line lays pieces out on one line, in the order of their columns.
func (p *cardPainter) line(ps ...placed) string { return strings.TrimRight(compose(ps), " ") }

// textRows are lines of plain text from column x on, in one style.
func (p *cardPainter) textRows(x int, st lipgloss.Style, lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = p.line(at(x, p.width-x, seg{l, st}))
	}
	return out
}

// labelRows are the rows of one labelled field: its label on the first one,
// each cell at column x.
func (p *cardPainter) labelRows(label string, x int, cells []cell) []string {
	return p.labelRowsAs(label, p.st.Header, x, cells)
}

func (p *cardPainter) labelRowsAs(label string, st lipgloss.Style, x int, cells []cell) []string {
	if len(cells) == 0 {
		cells = []cell{nil}
	}
	out := make([]string, len(cells))
	for i, c := range cells {
		var ps []placed
		if i == 0 {
			ps = append(ps, at(1, x-1, seg{label, st}))
		}
		out[i] = p.line(append(ps, placed{x, p.width - x, c})...)
	}
	return out
}

// plainCells are wrapped lines of text in one style.
func plainCells(lines []string, st lipgloss.Style) []cell {
	out := make([]cell, len(lines))
	for i, l := range lines {
		out[i] = cell{{l, st}}
	}
	return out
}

func (p *cardPainter) sepSeg() seg { return seg{p.g.sep, p.st.Dim} }

// titleLine is "magnum factory · crumb · crumb" with a chip on the right.
func (p *cardPainter) titleLine(right string, crumbs ...seg) string {
	left := " " + p.st.Title.Render("magnum factory")
	for _, c := range crumbs {
		if c.text != "" {
			left += p.st.Dim.Render(p.g.sep) + c.st.Render(c.text)
		}
	}
	return spread(left, right, p.width)
}

// chip is a key hint kept for the right of the title line.
func (p *cardPainter) chip(h hint) string { return p.st.hints(0, h) + " " }

// factsLine is the line under the title: first as it is, the rest dim.
func (p *cardPainter) factsLine(first string, rest ...string) string {
	parts := slices.DeleteFunc(append([]string{first}, rest...), func(s string) bool { return s == "" })
	if len(parts) == 0 {
		return ""
	}
	line := " " + parts[0]
	if len(parts) > 1 {
		line += p.st.Dim.Render(p.g.sep + strings.Join(parts[1:], p.g.sep))
	}
	return truncate(line, p.width)
}

// hintsLine is the footer's key hints; narrow screens drop the hints before
// the last one, which stays.
func (p *cardPainter) hintsLine(hs []hint) string {
	if len(hs) == 0 {
		return ""
	}
	sets := [][]hint{hs}
	last := hs[len(hs)-1]
	for k := len(hs) - 2; k >= 0; k-- {
		sets = append(sets, append(slices.Clone(hs[:k]), last))
	}
	return " " + p.st.fitHints(p.width-1, sets...)
}

// askLines are the footer when a y/N question is asked: its sentences on one
// line, or, when they do not fit, each on a line of its own, or the words
// wrapped over up to three lines, "y/N" ending the last, so that the question
// does not lose the words that say what it frees (only a question longer
// than three lines of the screen is cut, at the end of its third).
func (p *cardPainter) askLines(sentences ...string) []string {
	sentences = slices.DeleteFunc(sentences, func(s string) bool { return s == "" })
	w := p.width - 1
	one := strings.Join(sentences, " ")
	if ansi.StringWidth(one)+len(confirmYN) <= w || len(sentences) == 0 {
		return []string{" " + p.st.confirmLine(one, w)}
	}
	lines := sentences
	if len(lines) != 2 || ansi.StringWidth(lines[0]) > w || ansi.StringWidth(lines[1])+len(confirmYN) > w {
		lines = wrapText(one, w-len(confirmYN), askMaxLines)
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = " " + p.st.Warn.Render(truncate(l, w))
	}
	out[len(out)-1] = " " + p.st.confirmLine(lines[len(lines)-1], w)
	return out
}

// reasonLines are the footer that asks for a reason by its digit: the
// question, then each reason after its digit, over as many lines as they
// need, so that no reason is cut.
func (p *cardPainter) reasonLines(question string, reasons []string) []string {
	hs := make([]hint, 0, len(reasons)+1)
	for i, r := range reasons {
		hs = append(hs, hint{strconv.Itoa(i + 1), r})
	}
	hs = append(hs, hint{"esc", "cancels"})
	q := p.st.Warn.Render(question) + "  "
	qw := ansi.StringWidth(q)
	if p.width-1-qw < 24 { // too narrow beside the question: the reasons go under it
		out := []string{" " + p.st.Warn.Render(question)}
		for _, l := range p.st.wrapHints(p.width-3, hs...) {
			out = append(out, "   "+l)
		}
		return out
	}
	lines := p.st.wrapHints(p.width-1-qw, hs...)
	out := []string{" " + q + lines[0]}
	for _, l := range lines[1:] {
		out = append(out, " "+spaces(qw)+l)
	}
	return out
}

// assemble puts a card together: the head, the body from its line start, the
// rule (carrying msg, already styled, and the count of body lines cut off
// below) and the footer. With a height the body is cut or padded to fit and
// the footer sits at the bottom.
//
// On a pane too short for the head, the rule and the footer, the head gives
// way from its end (a blank, then what the card waits for, then its facts;
// the title last): the footer holds the y/N question, which must be on
// screen while a y answers it, and the rule stays above it.
func (p *cardPainter) assemble(head, body []string, start int, msg string, foot []string, height int) (string, int) {
	if height > 0 && len(head)+1+len(foot) > height {
		head = head[:max(height-1-len(foot), 0)]
	}
	room := len(body)
	if height > 0 {
		room = max(height-len(head)-1-len(foot), 0)
		start = min(max(start, 0), max(len(body)-room, 0))
	} else {
		start = 0
	}
	shown := body[start:min(start+room, len(body))]
	below := len(body) - start - len(shown)
	lines := append(slices.Clone(head), shown...)
	for height > 0 && len(lines) < len(head)+room {
		lines = append(lines, "")
	}
	lines = append(lines, p.base.footerRule(p.width, msg, below))
	lines = append(lines, foot...)
	if height > 0 && len(lines) > height {
		lines = lines[len(lines)-height:] // the footer stays: it holds the question
	}
	for i, l := range lines {
		lines[i] = truncate(l, p.width)
	}
	return strings.Join(lines, "\n"), below
}

// marks

func (p *cardPainter) dash() string {
	if p.ascii() {
		return "-"
	}
	return "–"
}

func (p *cardPainter) arrow() string {
	if p.ascii() {
		return "->"
	}
	return "→"
}

// bar is the quote bar before a drafted text and the typed text.
func (p *cardPainter) bar() string {
	if p.ascii() {
		return "|"
	}
	return "┃"
}

func (p *cardPainter) caret() string {
	if p.ascii() {
		return "_"
	}
	return "▏"
}

// minus is the sign of a removed line.
func (p *cardPainter) minus() string {
	if p.ascii() {
		return "-"
	}
	return "−"
}

func (p *cardPainter) mark(selected bool) seg {
	if !selected {
		return seg{" ", noStyle}
	}
	return seg{strings.TrimSpace(p.cursorMark()), p.st.Key}
}

// the question card

func (p *cardPainter) questionCard(c QuestionCard, height int) string {
	n := len(c.Questions)
	cur := min(max(c.Current, 0), max(n-1, 0))
	where := "no questions"
	if n > 0 {
		where = fmt.Sprintf("question %d of %d", cur+1, n)
	}
	asked, starts := "", ""
	if !c.Asked.IsZero() {
		asked = "asked " + agoText(p.now.Sub(c.Asked))
	}
	switch {
	case n == 1:
		starts = "coding starts after this answer"
	case n > 1:
		starts = fmt.Sprintf("coding starts after all %d", n)
	}
	head := []string{
		p.titleLine(p.chip(hint{"esc", "back"}), seg{c.TaskKey, p.st.Accent}, seg{where, noStyle}),
		p.factsLine(c.Title, c.Kind, asked, starts),
		"",
	}
	if n == 0 {
		frame, _ := p.assemble(head, nil, 0, "", []string{p.hintsLine([]hint{{"esc", "back"}})}, height)
		return frame
	}
	q := c.Questions[cur]
	parts := questionParts{
		list: p.questionList(c, cur), cur: cur,
		text:    p.textRows(1, p.st.Label, wrapText(q.Text, p.wrapW(1), 3)),
		context: p.textRows(1, p.st.Dim, wrapText(q.Context, p.wrapW(1), 2)),
	}
	if q.Example != "" {
		parts.example = p.textRows(1, p.st.Dim, wrapText("Example: "+q.Example, p.wrapW(1), 2))
	}
	rec := -1
	if q.Recommended >= 0 && q.Recommended < len(q.Options) {
		rec = q.Recommended
	}
	parts.choice = p.optionList(q.Options, c.Cursor, rec, q.Why)
	if rec < 0 && len(q.Options) > 0 {
		text := "no recommendation"
		if q.Why != "" {
			text += ": " + q.Why
		}
		if c.Cursor < 0 || c.Cursor >= len(q.Options) {
			text += p.g.sep + "nothing is marked: pick one (j k)"
		}
		parts.choice = append(parts.choice, p.textRows(1, p.st.Dim, wrapText(text, p.wrapW(1), 2))...)
	}
	foot := []string{p.hintsLine(orHints(c.Hints, p.questionHints(c, q, cur)))}
	switch {
	case c.input != nil:
		foot = p.inputFoot(c.input)
	case len(c.NeedlessReasons) > 0:
		foot = p.reasonLines("Why needless?", c.NeedlessReasons)
	}
	room := -1 // no height: everything is drawn
	if height > 0 {
		room = max(height-len(head)-1-len(foot), 0)
	}
	body := parts.fit(room)
	frame, _ := p.assemble(head, body, p.markInView(body, room), c.rule, foot, height)
	return frame
}

// questionParts are the pieces of the question card's body: the batch's
// questions, the question on the card (cur) with its context and example,
// and its options (choice), which stay in view.
type questionParts struct {
	list                   []string
	cur                    int
	text, context, example []string
	choice                 []string
}

// fit lays the parts out in room lines (room < 0: all of them). On a short
// pane they give way in this order, the least needed first: the blanks, the
// example, the context, the other questions of the batch, the line of the
// question on the card; the question and its options stay.
func (q questionParts) fit(room int) []string {
	cat := func(blanks bool, list []string, ex, ctx bool) []string {
		var out []string
		out = append(out, list...)
		if blanks {
			out = append(out, "")
		}
		out = append(out, q.text...)
		if ctx {
			out = append(out, q.context...)
		}
		if ex {
			out = append(out, q.example...)
		}
		if blanks {
			out = append(out, "")
		}
		return append(out, q.choice...)
	}
	var mine []string
	if q.cur >= 0 && q.cur < len(q.list) {
		mine = q.list[q.cur : q.cur+1]
	}
	steps := [][]string{
		cat(true, q.list, true, true),
		cat(false, q.list, true, true),
		cat(false, q.list, false, true),
		cat(false, q.list, false, false),
		cat(false, mine, false, false),
	}
	for _, s := range steps {
		if room < 0 || len(s) <= room {
			return s
		}
	}
	return cat(false, nil, false, false)
}

// markInView is the first body line to draw so that the line under the mark
// is in view in room lines: 0 unless the mark would fall below.
func (p *cardPainter) markInView(body []string, room int) int {
	if room <= 0 || len(body) <= room {
		return 0
	}
	mark := strings.TrimSpace(p.cursorMark())
	for i, l := range body {
		if strings.HasPrefix(ansi.Strip(l), " "+mark+" ") && i >= room {
			return i - room + 1
		}
	}
	return 0
}

// inputFoot is a footer that takes a typed line: what it is for, then the
// text behind the bar, with the caret.
func (p *cardPainter) inputFoot(in *cardInput) []string {
	rows := p.inputRows(cleanInput(strings.ReplaceAll(in.text, "\n", " ")))
	return []string{" " + p.st.Label.Render(cleanText(in.label)), rows[len(rows)-1]}
}

// answerTag is where an answer came from, in the recap's words.
func answerTag(q CardQuestion) string {
	switch {
	case q.Recommended < 0:
		return "there was no recommendation"
	case q.TookRecommendation:
		return "recommended"
	}
	return "your answer"
}

// questionList is the batch's questions, one line each: a mark, the number,
// the question, its answer and where the answer came from.
func (p *cardPainter) questionList(c QuestionCard, cur int) []string {
	qs := c.Questions
	numW := len(strconv.Itoa(len(qs)))
	textX := 3 + numW + 2
	var tw, aw, sw int
	stats := make([]cell, len(qs))
	for i, q := range qs {
		tw, aw = max(tw, ansi.StringWidth(q.Text)), max(aw, ansi.StringWidth(q.Answer))
		switch {
		case q.Answer != "":
			word, st := "your answer", p.st.Dim
			if q.TookRecommendation && q.Recommended >= 0 {
				word, st = "recommended", p.st.OK
			}
			stats[i] = cell{{word, st}}
			if !q.AnsweredAt.IsZero() {
				stats[i] = append(stats[i], seg{" " + p.clock(q.AnsweredAt), p.st.Dim})
			}
		case i == cur:
			stats[i] = cell{{"now", p.st.Dim}}
		}
		sw = max(sw, stats[i].width())
	}
	// The status gives way first, then the answer is cut, then the question.
	statX := p.width - 4 - sw
	ansX := textX + tw + cardGap
	showStat := sw > 0 && ansX+aw+2 <= statX
	end := p.width - 4
	if showStat {
		end = statX - 2
	}
	if aw > 0 && ansX+aw > end {
		if aw = end - ansX; aw < 8 {
			tw = max(min(tw, end-textX-cardGap-8), 12)
			ansX = textX + tw + cardGap
			aw = max(end-ansX, 0)
		}
	}
	tw = max(min(tw, end-textX), 1)
	out := make([]string, len(qs))
	for i, q := range qs {
		mark := seg{" ", noStyle}
		switch {
		case i == cur:
			mark = seg{p.markGlyph(MarkNeedsYou), p.st.Warn}
		case q.Answer != "":
			mark = seg{p.tick(), p.st.OK}
		}
		ps := []placed{at(1, 1, mark), at(3, numW, seg{strconv.Itoa(i + 1), noStyle}), at(textX, tw, seg{q.Text, noStyle})}
		if aw > 0 {
			ps = append(ps, at(ansX, aw, seg{q.Answer, noStyle}))
		}
		if showStat {
			ps = append(ps, placed{statX, sw, stats[i]})
		}
		out[i] = p.line(ps...)
	}
	return out
}

// optionCols fits the label, the means and the tag of the options in the
// screen: the means give way first (all of them, when they would be short),
// then the tag.
func (p *cardPainter) optionCols(labelX, lw, mw, tagW int) (lwOut, mwOut int, tag bool) {
	fit := func(room int) (int, int) {
		if mw > 0 && lw+cardGap+mw > room {
			if m := room - lw - cardGap; m >= 10 {
				return lw, m
			}
			return min(lw, room), 0
		}
		return min(lw, room), mw
	}
	if tagW > 0 {
		if room := p.width - cardTagMargin - tagW - 2 - labelX; room >= min(lw, 14) {
			l, m := fit(room)
			return l, m, true
		}
	}
	l, m := fit(p.width - 1 - labelX)
	return l, m, false
}

// optionList are the options of a card, numbered: the label, what it means
// (behind an arrow), the mark on the option under the cursor, and the
// recommended one in bold with its tag and its reason under it.
func (p *cardPainter) optionList(opts []CardOption, cursor, recommended int, why string) []string {
	numW := len(strconv.Itoa(len(opts)))
	labelX := 3 + numW + 2
	var lw, mw int
	for _, o := range opts {
		lw = max(lw, ansi.StringWidth(o.Label))
		if o.Means != "" {
			mw = max(mw, ansi.StringWidth(p.arrow()+" "+o.Means))
		}
	}
	const tagText = "recommended"
	tagW := 0
	if recommended >= 0 && recommended < len(opts) {
		tagW = len(tagText)
	}
	lw, mw, showTag := p.optionCols(labelX, lw, mw, tagW)
	var out []string
	for i, o := range opts {
		label := noStyle
		if i == recommended {
			label = p.st.Label
		}
		ps := []placed{at(1, 1, p.mark(i == cursor)), at(3, numW, seg{strconv.Itoa(i + 1), noStyle}), at(labelX, lw, seg{o.Label, label})}
		if mw > 0 && o.Means != "" {
			ps = append(ps, at(labelX+lw+cardGap, mw, seg{p.arrow() + " ", p.st.Dim}, seg{o.Means, noStyle}))
		}
		if showTag && i == recommended {
			ps = append(ps, rightAt(p.width-cardTagMargin, cell{{tagText, p.st.OK}}))
		}
		out = append(out, p.line(ps...))
		if i == recommended && why != "" {
			out = append(out, p.textRows(labelX, p.st.Dim, wrapText("why: "+why, p.wrapW(labelX), 2))...)
		}
	}
	return out
}

// questionHints are the question card's own key hints: enter only when the
// mark is on an option, − when the card takes it.
func (p *cardPainter) questionHints(c QuestionCard, q CardQuestion, cur int) []hint {
	var hs []hint
	if c.Cursor >= 0 && c.Cursor < len(q.Options) {
		hs = append(hs, hint{"enter", "take the marked one"})
	}
	hs = append(hs, hint{"j k", "move"})
	switch n := len(q.Options); {
	case n == 1:
		hs = append(hs, hint{"1", "pick"})
	case n > 1:
		hs = append(hs, hint{fmt.Sprintf("1-%d", n), "pick"})
	}
	if c.Needless {
		hs = append(hs, hint{p.minus(), "needless"})
	}
	if cur > 0 {
		hs = append(hs, hint{"backspace", "previous"})
	}
	return append(hs, hint{"z", "snooze 2h"}, hint{"esc", "back"})
}

// the recap

// splitAnswers numbers the answers and the asks (an answer that asks someone
// through a drafted Jira comment) of a batch, from 1. An answer the factory
// has already (sent) is no part of the batch.
func splitAnswers(qs []CardQuestion) (answers, asks []int) {
	for i, q := range qs {
		switch {
		case q.Answer == "" || q.sent:
		case q.Comment != "":
			asks = append(asks, i+1)
		default:
			answers = append(answers, i+1)
		}
	}
	return answers, asks
}

func numberWords(ns []int) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = strconv.Itoa(n)
	}
	return out
}

// recapQuestion is what the y/N question of the recap asks; empty when there
// is nothing to send.
func recapQuestion(answers, asks []int) string {
	var parts []string
	if len(answers) > 0 {
		parts = append(parts, "send "+factoryPlural(len(answers), "answer", "answers"))
	}
	if len(asks) > 0 {
		parts = append(parts, "copy "+factoryPlural(len(asks), "Jira comment", "Jira comments")+" for you to post")
	}
	if len(parts) == 0 {
		return ""
	}
	return upperFirst(strings.Join(parts, " and ")) + "?"
}

// recapSummary is the paragraph under the answers: what happens when the
// operator sends. The factory does not write to Jira in milestone 1, so the
// comments are copied for the operator to post, and the task waits until the
// operator marks the question answered.
func (p *cardPainter) recapSummary(c QuestionCard, answers, asks []int) string {
	var parts []string
	switch len(answers) {
	case 0:
	case 1:
		parts = append(parts, "the agents get answer "+strconv.Itoa(answers[0]))
	default:
		parts = append(parts, "the agents get answers "+joinAnd(numberWords(answers)))
	}
	if len(asks) > 0 {
		on := ""
		if c.TaskKey != "" {
			on = " on " + c.TaskKey
		}
		if len(asks) == 1 {
			parts = append(parts, "1 Jira comment is copied for you to post"+on)
		} else {
			parts = append(parts, fmt.Sprintf("%d Jira comments are copied for you to post%s", len(asks), on))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	text := "When you send: " + strings.Join(parts, ", and ") + "."
	if len(asks) > 0 {
		var who []string
		for _, i := range asks {
			if a := c.Questions[i-1].Asks; a != "" && !slices.Contains(who, a) {
				who = append(who, a)
			}
		}
		waits := "The task waits"
		if len(who) > 0 {
			waits += " for " + joinAnd(who)
		}
		noun := "question"
		if len(asks) > 1 {
			noun = "questions"
		}
		text += " " + waits + " until you mark " + noun + " " + joinAnd(numberWords(asks)) + " answered."
	}
	if !c.AsOf.IsZero() {
		text += " These answers are for the questions as of " + p.clock(c.AsOf) + "."
	}
	return text
}

func (p *cardPainter) recap(c QuestionCard, height int) string {
	qs := c.Questions
	head := []string{
		p.titleLine(p.chip(hint{"esc", "back"}), seg{c.TaskKey, p.st.Accent}, seg{"your answers", noStyle}),
		p.factsLine(c.Title, c.Kind, "nothing is sent yet"),
		"",
	}
	numW := len(strconv.Itoa(len(qs)))
	textX := 1 + numW + 2
	var body []string
	for i, q := range qs {
		body = append(body, p.line(at(1, numW, seg{strconv.Itoa(i + 1), noStyle}), at(textX, p.width-textX-1, seg{q.Text, noStyle})))
		if q.Answer == "" {
			body = append(body, p.line(at(textX, p.width-textX-1, seg{"not answered yet", p.st.Dim})))
			continue
		}
		tag := answerTag(q)
		tagSt := p.st.Dim
		if tag == "recommended" {
			tagSt = p.st.OK
		}
		tagW := ansi.StringWidth(tag)
		ansW := p.width - cardTagMargin - tagW - 2 - textX
		if ansW < 12 {
			tag, ansW = "", p.width-1-textX
		}
		ps := []placed{at(textX, ansW, seg{q.Answer, noStyle})}
		if tag != "" {
			ps = append(ps, rightAt(p.width-cardTagMargin, cell{{tag, tagSt}}))
		}
		body = append(body, p.line(ps...))
		if q.Comment == "" {
			continue
		}
		for l := range strings.SplitSeq(q.Comment, "\n") {
			for _, w := range wrapText(l, p.wrapW(textX+2), 0) {
				body = append(body, p.line(at(textX, 1, seg{p.bar(), p.st.Dim}), at(textX+2, p.width-textX-2, seg{w, noStyle})))
			}
		}
	}
	answers, asks := splitAnswers(qs)
	if text := p.recapSummary(c, answers, asks); text != "" {
		body = append(body, "")
		lines := wrapText(text, p.wrapW(1), 0)
		for i, l := range lines {
			cl := cell{{l, noStyle}}
			if i == 0 {
				cl = cell{{"When you send:", p.st.Label}, {strings.TrimPrefix(l, "When you send:"), noStyle}}
			}
			body = append(body, p.line(placed{1, p.width - 1, cl}))
		}
	}
	foot := []string{p.hintsLine(orHints(c.Hints, []hint{{"esc", "back"}}))}
	msg := c.rule
	switch {
	case c.changed != nil:
		msg, foot = p.changedFoot(*c.changed)
	case c.sending:
	default:
		if q := recapQuestion(answers, asks); q != "" {
			foot = p.askLines(q)
		}
	}
	frame, _ := p.assemble(head, body, 0, msg, foot, height)
	return frame
}

// changedFoot is the recap after the factory refused the batch because the
// questions changed (spec 9.C): the rule says so and that nothing was sent,
// the footer why, then the keys.
func (p *cardPainter) changedFoot(ch recapChange) (msg string, foot []string) {
	text := "the questions changed"
	if !ch.at.IsZero() {
		text += " at " + p.clock(ch.at)
	}
	msg = p.st.Warn.Render(text + p.g.sep + "nothing was sent")
	for _, l := range wrapText(cleanText(ch.why), p.width-2, 2) {
		foot = append(foot, " "+l)
	}
	again := hint{"enter", "see the questions again"}
	if ch.again > 0 {
		again = hint{"enter", fmt.Sprintf("see question %d again", ch.again)}
	}
	return msg, append(foot, p.hintsLine([]hint{again, {"esc", "back"}}))
}
