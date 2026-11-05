package tui

// The factory's new task screen (spec 9.C, "New task", and 6.1): the text
// being typed, what the factory understood of it (the issue a key or a link
// names, the keys found in the text, its links, what else touches the work),
// and the question that starts the task. See factory_card.go for the painter
// it shares with the other cards.

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

func (p *cardPainter) newTask(f NewTaskForm, height int) string {
	p.f.Spinner = f.Spinner
	head := []string{p.titleLine(p.chip(hint{"esc", "cancels"}), seg{"new task", noStyle}), ""}
	body := []string{p.inputLabel()}
	body = append(body, p.inputRows(f.Text)...)
	if rows := p.previewRows(f); len(rows) > 0 {
		body = append(body, "")
		body = append(body, rows...)
	}
	var foot []string
	switch {
	case f.Ask != "":
		foot = p.askLines(f.Ask)
	case len(f.Hints) > 0:
		foot = []string{p.hintsLine(f.Hints)}
	case strings.TrimSpace(f.Text) == "":
		foot = []string{" " + p.st.Dim.Render("type a Jira key or describe the task"+p.g.sep) + p.st.hints(0, hint{"esc", "cancels"})}
	default:
		foot = []string{p.hintsLine(p.newTaskHints(f))}
	}
	frame, _ := p.assemble(head, body, 0, "", foot, height)
	return frame
}

// newTaskHints are the form's own key hints: tab once there is a kind to
// change.
func (p *cardPainter) newTaskHints(f NewTaskForm) []hint {
	var hs []hint
	if f.Kind != "" {
		hs = append(hs, hint{"tab", "kind"})
	}
	if f.Jira != nil && f.Jira.More && !severalKeys(f) {
		hs = append(hs, hint{"pgdn", "reads more"})
	}
	return append(hs, hint{"esc", "cancels"})
}

// severalKeys is true when the text names several keys Jira knows: the task
// is the prompt task it is, and asks which issue it belongs to.
func severalKeys(f NewTaskForm) bool { return len(f.Keys) > 1 }

// inputLabel is the label of the text with the keys of the input on its
// right, as many as leave the label whole.
func (p *cardPainter) inputLabel() string {
	left := " " + p.st.Header.Render("Jira key or describe the task")
	keys := []hint{{"enter", "starts"}, {"ctrl+j", "new line"}, {"ctrl+g", "editor"}}
	for n := len(keys); n > 0; n-- {
		right := p.st.hints(0, keys[:n]...) + " "
		if ansi.StringWidth(left)+2+ansi.StringWidth(right) <= p.width {
			return spread(left, right, p.width)
		}
	}
	return truncate(left, p.width)
}

// inputRows echo the text being typed, behind the quote bar, with the caret
// at its end (on a new line after a line break). A line wider than the screen
// breaks at the width, not at a word, so every space stays where it was typed.
// The last lines are kept when there are many.
func (p *cardPainter) inputRows(text string) []string {
	w := max(p.width-4, 8) // the caret takes the cell after the text
	var lines []string
	for l := range strings.SplitSeq(text, "\n") {
		if ansi.StringWidth(l) <= w {
			lines = append(lines, l)
			continue
		}
		lines = append(lines, strings.Split(ansi.Hardwrap(l, w, true), "\n")...)
	}
	if len(lines) > cardInputLines {
		lines = lines[len(lines)-cardInputLines:]
	}
	last := len(lines) - 1
	out := make([]string, len(lines))
	for i, l := range lines {
		c := cell{{l, noStyle}}
		if i == last {
			c = append(c, seg{p.caret(), p.st.Accent})
		}
		out[i] = p.line(at(1, 1, seg{p.bar(), p.st.Accent}), placed{3, p.width - 3, c})
	}
	return out
}

// previewRows are what the factory understood of the text; a row is drawn
// only when its field is filled.
func (p *cardPainter) previewRows(f NewTaskForm) []string {
	const x = cardFormX
	wrapW := p.wrapW(x)
	var out []string
	switch {
	case f.Reading:
		out = append(out, p.labelRows("JIRA", x, []cell{{{p.spinnerFrame(), p.st.Accent}, {" the daemon reads Jira", p.st.Dim}}})...)
	case severalKeys(f):
		// No ticket is the task's: the JIRA row names the keys, and a found
		// key Jira refused beside them is its error.
		cells := []cell{p.keysCell(f.Keys)}
		if f.Err != "" {
			cells = append(cells, plainCells(wrapText(f.Err, wrapW, 2), p.st.Err)...)
		}
		out = append(out, p.labelRows("JIRA", x, cells)...)
	case f.Jira != nil:
		out = append(out, p.jiraRows(f.Jira)...)
	case f.Err != "":
		out = append(out, p.labelRows("JIRA", x, plainCells(wrapText(f.Err, wrapW, 2), p.st.Err))...)
	case strings.TrimSpace(f.Text) != "":
		out = append(out, p.labelRows("JIRA", x, []cell{{{"none: the agents work from your words", noStyle}}})...)
	}
	// The guidance is the daemon's, as the start sends it: the text is never
	// parsed here.
	if f.Guidance != "" && !severalKeys(f) {
		out = append(out, p.labelRows("GUIDANCE", x, plainCells(wrapText(`"`+f.Guidance+`" goes to every agent`, wrapW, 3), noStyle))...)
	}
	if len(f.Links) > 0 {
		out = append(out, p.linkRows(f.Links)...)
	}
	if f.Kind != "" {
		c := cell{{f.Kind + spaces(max(12-ansi.StringWidth(f.Kind), 1)), noStyle}}
		if f.KindWhy != "" {
			c = append(c, seg{f.KindWhy + p.g.sep, p.st.Dim})
		}
		out = append(out, p.labelRows("KIND", x, []cell{append(c, seg{"tab: bug, feature, chore", p.st.Dim})})...)
	}
	if (f.Jira == nil || severalKeys(f)) && f.Title != "" {
		out = append(out, p.labelRows("TITLE", x, plainCells(wrapText(f.Title, wrapW, 2), noStyle))...)
	}
	if f.Branch != "" {
		out = append(out, p.labelRows("BRANCH", x, []cell{{{f.Branch, noStyle}}})...)
	}
	if f.Starts != "" {
		out = append(out, p.labelRows("STARTS", x, plainCells(wrapText(f.Starts, wrapW, 2), noStyle))...)
	}
	if f.Overlap != "" {
		out = append(out, p.labelRows("OVERLAP", x, plainCells(wrapText(f.Overlap, wrapW, 2), noStyle))...)
	}
	if len(f.Agents) > 0 {
		text := fmt.Sprintf("%d: %s", len(f.Agents), strings.Join(f.Agents, p.g.sep))
		out = append(out, p.labelRows("AGENTS", x, plainCells(wrapText(text, wrapW, 3), noStyle))...)
	}
	if len(f.Rules) > 0 {
		out = append(out, p.ruleRows(f, wrapW)...)
	}
	return out
}

// keysCell names the several keys of a text: the prompt task they are asks
// which issue it is.
func (p *cardPainter) keysCell(keys []string) cell {
	var c cell
	for i, k := range keys {
		switch {
		case i == 0:
		case i == len(keys)-1:
			c = append(c, seg{" and ", noStyle})
		default:
			c = append(c, seg{", ", noStyle})
		}
		c = append(c, seg{k, p.st.Accent})
	}
	return append(c, seg{" found in your text", noStyle}, p.sepSeg(), seg{"the task asks which issue it is", noStyle})
}

// linkWords name the kinds of a link.
var linkWords = map[string]string{"jira": "Jira", "pr": "PR", "run": "CI run"}

// linkRows are the LINKS row: what each link is, the link without its
// scheme (cut from the left when it must be: its end names the thing), and
// what the factory makes of it.
func (p *cardPainter) linkRows(links []FormLink) []string {
	const x, gap = cardFormX, 2
	kinds := make([]string, len(links))
	urls := make([]string, len(links))
	var kw, uw, ow int
	for i, l := range links {
		kinds[i] = cmp.Or(linkWords[l.Kind], l.Kind)
		urls[i] = strings.TrimPrefix(strings.TrimPrefix(l.URL, "https://"), "http://")
		kw, uw, ow = max(kw, ansi.StringWidth(kinds[i])), max(uw, ansi.StringWidth(urls[i])), max(ow, ansi.StringWidth(l.Offer))
	}
	avail := p.width - 1 - x
	if kw+gap+uw+gap+ow > avail {
		uw = max(avail-kw-gap-gap-ow, 16)
	}
	urlX := x + kw + gap
	offerX := urlX + uw + gap
	out := make([]string, len(links))
	for i, l := range links {
		var ps []placed
		if i == 0 {
			ps = append(ps, at(1, x-1, seg{"LINKS", p.st.Header}))
		}
		ps = append(ps, at(x, kw, seg{kinds[i], noStyle}), placed{urlX, uw, cell{{urls[i], p.st.Accent}}.fitLeft(uw)})
		if l.Offer != "" && offerX < p.width-1 {
			ps = append(ps, at(offerX, p.width-1-offerX, seg{l.Offer, p.st.Dim}))
		}
		out[i] = p.line(ps...)
	}
	return out
}

func (p *cardPainter) jiraRows(j *JiraPreview) []string {
	const x = cardFormX
	facts := cell{{j.Key, p.st.Accent}}
	for _, part := range []string{j.Type, j.Status, j.Priority} {
		if part != "" {
			facts = append(facts, p.sepSeg(), seg{part, noStyle})
		}
	}
	if j.Reporter != "" {
		facts = append(facts, p.sepSeg(), seg{"reporter " + j.Reporter, noStyle})
	}
	if !j.Read.IsZero() {
		facts = append(facts, p.sepSeg(), seg{"read by the daemon " + agoText(p.now.Sub(j.Read)), p.st.Dim})
	}
	cells := []cell{facts}
	if j.Summary != "" {
		cells = append(cells, cell{{j.Summary, p.st.Label}})
	}
	if j.Found != "" { // after the summary: the facts row has no room for it at 100 columns
		note := seg{j.Found, p.st.Dim}
		if len(cells) > 1 {
			cells[1] = append(cells[1], p.sepSeg(), note)
		} else {
			cells = append(cells, cell{note})
		}
	}
	out := p.labelRows("JIRA", x, cells)
	if j.Excerpt == "" {
		return out
	}
	quote := placed{x, p.width - x, cell{{`"` + j.Excerpt + `"`, noStyle}}}
	if j.More {
		more := cell{{"pgdn", p.st.Key}, {" reads more", p.st.Dim}}
		room := p.width - cardTagMargin - more.width() - 2 - x
		quote = placed{x, max(room, 12), cell{{`"` + j.Excerpt + `"`, noStyle}}}
		return append(out, p.line(quote, rightAt(p.width-cardTagMargin, more)))
	}
	return append(out, p.line(quote))
}

func (p *cardPainter) ruleRows(f NewTaskForm, wrapW int) []string {
	const x = cardFormX
	words := []string{strconv.Itoa(len(f.Rules)), f.Repo, "rules go", "into its prompts"}
	if len(f.Rules) == 1 {
		words[2] = "rule goes"
	}
	words = slices.DeleteFunc(words, func(s string) bool { return s == "" })
	text := strings.Join(words, " ")
	more := cell{{"L", p.st.Key}, {" shows them", p.st.Dim}}
	out := []string{p.line(at(1, x-1, seg{"RULES", p.st.Header}),
		placed{x, max(p.width-cardTagMargin-more.width()-2-x, 12), cell{{text, noStyle}}},
		rightAt(p.width-cardTagMargin, more))}
	if !f.ShowRules {
		return out
	}
	return append(out, p.labelRowsAfter(x, plainCells(wrapText(strings.Join(f.Rules, p.g.sep), wrapW, 4), noStyle))...)
}

// labelRowsAfter are continuation rows of a field: no label, cells at column x.
func (p *cardPainter) labelRowsAfter(x int, cells []cell) []string {
	out := make([]string, len(cells))
	for i, c := range cells {
		out[i] = p.line(placed{x, p.width - x, c})
	}
	return out
}
