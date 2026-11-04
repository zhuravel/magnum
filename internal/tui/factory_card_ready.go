package tui

// The factory's ready card (spec 9.C, "Ready card", with the refinements of
// revision 1): a finished task for the operator's decision. The FLAGS line
// comes first, then the cause, the fix and the proof, the files (the flagged
// ones first), the doubts, what the local review left open (KNOWN) and the
// screenshots; then the three options, numbered, and the files not read in
// the diff. The recommendation shows only once the diff was opened. See
// factory_card.go for the painter it shares with the other cards.

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
	wrapW := p.wrapW(cardReadyX)
	var body []string
	field := func(label, text string) {
		if text != "" {
			body = append(body, p.labelRows(label, cardReadyX, plainCells(wrapText(text, wrapW, 0), noStyle))...)
		}
	}
	list := func(label string, items []string, none string, st lipgloss.Style) {
		cells := p.hangingCells(items, wrapW, st)
		if len(cells) == 0 {
			cells = []cell{{{none, p.st.Dim}}}
		}
		body = append(body, p.labelRows(label, cardReadyX, cells)...)
	}
	// The flags are on the operator, a second look before the push: yellow.
	list("FLAGS", c.Flags, "none", p.st.Warn)
	field("CAUSE", c.Cause)
	field("FIX", c.Fix)
	field("PROOF", c.Proof)
	body = append(body, p.fileRows(c.Files)...)
	list("DOUBTS", c.Doubts, "none", noStyle)
	list("KNOWN", c.Leftovers, "none: the local review left nothing open", noStyle)
	shots := cell{{"none", p.st.Dim}}
	if c.Screenshots > 0 {
		text := strconv.Itoa(c.Screenshots)
		if c.ScreenshotsNote != "" {
			text += ", " + c.ScreenshotsNote
		}
		shots = cell{{text, noStyle}}
	}
	body = append(body, p.labelRows("SCREENSHOTS", cardReadyX, []cell{shots})...)
	body = append(body, "")

	read := !c.DiffOpened.IsZero()
	rec := -1
	if read && c.Recommended >= 0 && c.Recommended < len(readyOptions) {
		rec = c.Recommended
	}
	body = append(body, p.optionList(readyOptions, c.Cursor, rec, c.Why)...)
	body = append(body, p.unreadRows(c.Unread, read)...)
	msg := ""
	if read {
		msg = p.st.Dim.Render("you read the diff " + p.clock(c.DiffOpened))
	}
	foot := []string{p.hintsLine(orHints(c.Hints, p.readyHints(c)))}
	switch {
	case c.Ask != "":
		var frees string
		if c.Frees != "" && (c.Cursor == readyPush || c.Cursor == readyDrop) {
			frees = "It frees " + c.Frees + "."
		}
		foot = p.askLines(c.Ask, frees)
	case len(c.Reasons) > 0:
		foot = p.reasonLines("Why send back?", c.Reasons)
	}
	frame, _ := p.assemble(head, body, 0, msg, foot, height)
	return frame
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
// them, or the plan did not) first.
func (p *cardPainter) fileRows(files []CardFile) []string {
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
	out := make([]string, len(files))
	for i, f := range files {
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
	return out
}
