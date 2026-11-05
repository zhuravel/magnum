package tui

// The factory's ready card (spec 9.C, "Ready card", with the refinements of
// revision 1): a finished task for the operator's decision. The FLAGS line
// comes first, then the cause, the fix and the proof, the files (the flagged
// ones first), the doubts, what the local review left open (KNOWN) and the
// screenshots; then the three options, numbered, and the files not read in
// the diff. The recommendation shows only once the diff was opened.
//
// On a short screen the options, the mark, the recommendation, the unread
// files and the question stay in view: the lists above them give way (see
// fitReady), each saying how much it does not show. See factory_card.go for
// the painter it shares with the other cards.

import (
	"slices"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// readyOptions are the ready card's options, picked by 1 to 3: push + PR
// opens the PR ready for review, never a draft.
var readyOptions = []CardOption{
	{"push + PR", "push the branch, open a PR ready for review"},
	{"send back", "the writer tries again with your reason and note"},
	{"drop", "free the copy, keep the branch"},
}

// The ready card's options, by index.
const (
	readyPush = iota
	readySendBack
	readyDrop
)

func (p *cardPainter) ready(c ReadyCard, height int) string {
	head := []string{
		p.titleLine(p.chip(hint{"esc", "back"}), seg{c.TaskKey, p.st.Accent}, seg{"ready to push", noStyle}),
		p.factsLine(c.Title, c.Kind, "waited "+shortAge(max(c.Waited, 0))),
	}
	line3 := " " + c.Branch
	if c.Commits > 0 {
		line3 += p.st.Dim.Render(p.g.sep + factoryPlural(c.Commits, "commit", "commits") + p.g.sep + "not on GitHub yet")
	}
	head = append(head, truncate(line3, p.width), "")

	read := !c.DiffOpened.IsZero()
	rec := -1
	if read && c.Recommended >= 0 && c.Recommended < len(readyOptions) {
		rec = c.Recommended
	}
	choice := append(p.optionList(readyOptions, c.Cursor, rec, c.Why), p.unreadRows(c.Unread, read)...)
	msg := ""
	if read {
		msg = p.st.Dim.Render("you read the diff " + p.clock(c.DiffOpened))
	}
	foot := []string{p.hintsLine(orHints(c.Hints, p.readyHints(c)))}
	switch {
	case c.Ask != "":
		var frees string
		// The push's y/N says what it frees; a drop's is the spec's own
		// sentence, which names the copy ("Drop PR-12360: free
		// talkable.factory3, keep the branch?").
		if c.Frees != "" && c.Cursor == readyPush {
			frees = "It frees " + c.Frees + "."
		}
		foot = p.askLines(c.Ask, frees)
	case len(c.Reasons) > 0:
		foot = p.reasonLines("Why send back?", c.Reasons)
	}
	room := -1 // no height: everything is drawn
	if height > 0 {
		room = max(height-len(head)-1-len(foot), 0) - len(choice)
	}
	// A blank line parts the lists from the options, when there is room for
	// it and a list line.
	blank := room < 0 || room > 1
	if blank && room > 0 {
		room--
	}
	body := p.fitReady(c, room)
	if blank && len(body) > 0 {
		body = append(body, "")
	}
	frame, _ := p.assemble(head, append(body, choice...), 0, msg, foot, height)
	return frame
}

// readyFit says how much of the ready card's lists is drawn: the items of
// FLAGS, FILES and KNOWN (the rest counted on a line of their own), and
// which lists draw each item on one line, cut, instead of wrapped.
type readyFit struct {
	flags, files, known                     int
	flagsOne, proofOne, doubtsOne, knownOne bool
}

// fitReady are the ready card's lists, the rows above its options, in at
// most room lines (room < 0: all of them, whole). They give way in this
// order, the least needed first: KNOWN goes to one line an item, then to a
// count ("listed under Known in the PR body"); FLAGS to one line a flag;
// PROOF to one line; FILES to a count ("d shows the diff"), the flagged files
// staying longest; DOUBTS to one line a doubt; FLAGS to its first flag and a
// count. When that is still too tall, the lists are cut, their last line
// counting the lines not shown.
func (p *cardPainter) fitReady(c ReadyCard, room int) []string {
	f := readyFit{flags: len(c.Flags), files: len(c.Files), known: len(c.Leftovers)}
	lines := p.readyLists(c, f)
	if room < 0 || len(lines) <= room {
		return lines
	}
	fits := func() bool { lines = p.readyLists(c, f); return len(lines) <= room }
	once := func(b *bool) func() bool {
		return func() bool { done := *b; *b = true; return !done }
	}
	fewer := func(n *int, least int) func() bool {
		return func() bool {
			if *n <= least {
				return false
			}
			*n--
			return true
		}
	}
	for _, step := range []func() bool{
		once(&f.knownOne), fewer(&f.known, 0), once(&f.flagsOne), once(&f.proofOne),
		fewer(&f.files, 0), once(&f.doubtsOne), fewer(&f.flags, 1),
	} {
		for !fits() {
			if !step() {
				break
			}
		}
		if len(lines) <= room {
			return lines
		}
	}
	if room <= 0 {
		return nil
	}
	rest := len(lines) - room + 1
	return append(lines[:room-1], p.line(at(1, p.width-1, seg{factoryPlural(rest, "more line", "more lines") + ", a taller window shows them", p.st.Dim})))
}

// readyLists are the rows above the ready card's options, as f draws them.
func (p *cardPainter) readyLists(c ReadyCard, f readyFit) []string {
	wrapW := p.wrapW(cardReadyX)
	var body []string
	field := func(label, text string, one bool) {
		switch {
		case text == "":
		case one:
			body = append(body, p.labelRows(label, cardReadyX, []cell{{{text, noStyle}}})...)
		default:
			body = append(body, p.labelRows(label, cardReadyX, plainCells(wrapText(text, wrapW, 0), noStyle))...)
		}
	}
	// list draws the first keep items, each on one line when one is set,
	// and counts the rest on a line of its own: more(n, shown) says it.
	list := func(label string, items []string, keep int, one bool, none string, st lipgloss.Style, more func(n, shown int) string) {
		var cells []cell
		for _, it := range items[:min(keep, len(items))] {
			if one {
				cells = append(cells, cell{{it, st}})
				continue
			}
			cells = append(cells, p.hangingCells([]string{it}, wrapW, st)...)
		}
		if n := len(items) - keep; n > 0 {
			cells = append(cells, cell{{more(n, keep), p.st.Dim}})
		}
		if len(cells) == 0 {
			cells = []cell{{{none, p.st.Dim}}}
		}
		body = append(body, p.labelRows(label, cardReadyX, cells)...)
	}
	// The flags are on the operator, a second look before the push: yellow.
	list("FLAGS", c.Flags, f.flags, f.flagsOne, "none", p.st.Warn, func(n, _ int) string {
		return "+" + strconv.Itoa(n) + " more, a taller window shows them"
	})
	field("CAUSE", c.Cause, false)
	field("FIX", c.Fix, false)
	field("PROOF", c.Proof, f.proofOne)
	body = append(body, p.fileRows(c.Files, f.files)...)
	list("DOUBTS", c.Doubts, len(c.Doubts), f.doubtsOne, "none", noStyle, nil)
	list("KNOWN", c.Leftovers, f.known, f.knownOne, "none: the local review left nothing open", noStyle, func(n, shown int) string {
		if shown == 0 {
			return factoryPlural(n, "note", "notes") + ", listed under Known in the PR body"
		}
		return "+" + strconv.Itoa(n) + " more, listed under Known in the PR body"
	})
	shots := cell{{"none", p.st.Dim}}
	if c.Screenshots > 0 {
		text := strconv.Itoa(c.Screenshots)
		if c.ScreenshotsNote != "" {
			text += ", " + c.ScreenshotsNote
		}
		shots = cell{{text, noStyle}}
	}
	return append(body, p.labelRows("SCREENSHOTS", cardReadyX, []cell{shots})...)
}

// readyHints are the ready card's own key hints.
func (p *cardPainter) readyHints(c ReadyCard) []hint {
	hs := []hint{{"d", "read the diff"}}
	if c.Screenshots > 0 {
		hs = append(hs, hint{"e", "screenshots"})
	}
	return append(hs, hint{"1-3", "pick"}, hint{"j k", "move"}, hint{"enter", "do it"}, hint{"esc", "back"})
}

// hangingCells are the items of a list, wrapped, each line after an item's
// first indented by two cells, so that where an item ends shows.
func (p *cardPainter) hangingCells(items []string, width int, st lipgloss.Style) []cell {
	var out []cell
	for _, it := range items {
		for i, l := range wrapText(it, width-2, 0) {
			if i > 0 {
				l = "  " + l
			}
			out = append(out, cell{{l, st}})
		}
	}
	return out
}

// unreadRows name the files the operator has not opened in the diff, after
// "unread:"; before the diff was opened at all they also say when the
// recommendation shows. A diff read whole draws nothing.
func (p *cardPainter) unreadRows(unread []string, read bool) []string {
	if len(unread) == 0 && read {
		return nil
	}
	text := strings.Join(unread, ", ")
	if text == "" {
		text = "the diff"
	}
	if !read {
		text += p.g.sep + "the recommendation shows after d"
	}
	const label = "unread:"
	x := 1 + len(label) + 1
	var out []string
	for i, l := range wrapText(text, p.wrapW(x), 0) {
		var ps []placed
		if i == 0 {
			ps = append(ps, at(1, len(label), seg{label, p.st.Warn}))
		}
		out = append(out, p.line(append(ps, at(x, p.width-x, seg{l, noStyle}))...))
	}
	return out
}

// fileRows is the FILES table: the path, the lines added and removed, and
// whether the plan named the file, the flagged files (a FLAGS line names
// them, or the plan did not) first. The first keep files are drawn; a line
// counts the rest, which the diff shows.
func (p *cardPainter) fileRows(files []CardFile, keep int) []string {
	if len(files) == 0 {
		return p.labelRows("FILES", cardReadyX, []cell{{{"none", p.st.Dim}}})
	}
	files = slices.Clone(files)
	flagged := func(f CardFile) bool { return f.Flagged || !f.InPlan }
	slices.SortStableFunc(files, func(a, b CardFile) int {
		switch {
		case flagged(a) == flagged(b):
			return 0
		case flagged(a):
			return -1
		}
		return 1
	})
	counts := make([]cell, len(files))
	notes := make([]cell, len(files))
	var pw, nw, noteW int
	for i, f := range files {
		pw = max(pw, ansi.StringWidth(f.Path))
		if f.Added > 0 {
			counts[i] = append(counts[i], seg{"+" + strconv.Itoa(f.Added), p.base.pal.add})
		}
		if f.Removed > 0 {
			if len(counts[i]) > 0 {
				counts[i] = append(counts[i], seg{" ", noStyle})
			}
			counts[i] = append(counts[i], seg{p.minus() + strconv.Itoa(f.Removed), p.base.pal.del})
		}
		nw = max(nw, counts[i].width())
		notes[i] = cell{{"in the plan", p.st.Dim}}
		if !f.InPlan {
			notes[i] = cell{{"not in the plan", p.st.Warn}}
		}
		if f.Flagged {
			notes[i] = append(notes[i], p.sepSeg(), seg{"flagged", p.st.Warn})
		}
		noteW = max(noteW, notes[i].width())
	}
	// The path is cut from the left (its end names the file), then the counts
	// go: what the table keeps is the note that says the file needs a look.
	avail := p.width - 1 - cardReadyX
	if pw+4+nw+4+noteW > avail {
		if pw = max(avail-4-nw-4-noteW, 12); pw+4+nw+4+noteW > avail {
			nw = 0
			pw = max(avail-4-noteW, 8)
		}
	}
	numX := cardReadyX + pw + 4
	noteX := numX + nw + 4
	if nw == 0 {
		noteX = numX
	}
	keep = min(max(keep, 0), len(files))
	out := make([]string, keep)
	for i, f := range files[:keep] {
		var ps []placed
		if i == 0 {
			ps = append(ps, at(1, cardReadyX-1, seg{"FILES", p.st.Header}))
		}
		path := cell{{f.Path, noStyle}}.fitLeft(pw)
		ps = append(ps, placed{cardReadyX, pw, path})
		if nw > 0 {
			ps = append(ps, placed{numX, nw, counts[i]})
		}
		out[i] = p.line(append(ps, placed{noteX, p.width - noteX, notes[i]})...)
	}
	switch n := len(files) - keep; {
	case n > 0 && keep == 0:
		out = p.labelRows("FILES", cardReadyX, []cell{{{factoryPlural(n, "file", "files") + ", d shows the diff", p.st.Dim}}})
	case n > 0:
		out = append(out, p.line(at(cardReadyX, p.width-cardReadyX, seg{"+" + strconv.Itoa(n) + " more, d shows the diff", p.st.Dim})))
	}
	return out
}
