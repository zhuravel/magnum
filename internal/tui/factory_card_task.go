package tui

// The factory's task card (spec 9.C, "Task card at 140"), cut to the sections
// of milestone 1a: the header, WAITING FOR, STAGES, AGENTS, REPORT and
// LEARNED. See factory_card.go for the painter it shares with the other cards.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

func (p *cardPainter) task(c TaskCard, height, scroll int) (string, int) {
	p.f.Spinner = c.Spinner
	facts := []string{c.Branch, c.PR, c.Copy}
	if !c.Started.IsZero() {
		layout := "15:04"
		if y, m, d := c.Started.In(p.now.Location()).Date(); y != p.now.Year() || m != p.now.Month() || d != p.now.Day() {
			layout = "2 Jan 15:04"
		}
		facts = append(facts, "started "+c.Started.In(p.now.Location()).Format(layout))
	}
	if c.YourTime > 0 {
		facts = append(facts, "your time on it "+tookText(c.YourTime))
	}
	crumbs := []seg{{c.Key, p.st.Accent}, {c.Kind, noStyle}, {c.Title, noStyle}}
	clock := p.st.Dim.Render(p.g.refresh+" "+p.now.Format("15:04")) + " "
	head := []string{
		p.titleLine(clock, crumbs...),
		p.factsLine(facts[0], facts[1:]...),
		"",
	}
	if c.WaitingFor != "" {
		head = append(head, p.waitingRows(c.WaitingFor)...)
		head = append(head, "")
	}
	body, cursor := p.taskBody(c)
	start := scroll
	if height > 0 && cursor >= 0 {
		room := max(height-len(head)-1-1, 1)
		start = min(max(start, cursor-room+1), cursor)
	}
	return p.assemble(head, body, start, "", []string{p.hintsLine(orHints(c.Hints, p.taskHints(c)))}, height)
}

// taskHints are the task card's own key hints. A task that waits on the
// operator offers r: retry, which goes on at its stage (spec 9.D).
func (p *cardPainter) taskHints(c TaskCard) []hint {
	hs := []hint{{"j k", "move"}, {"enter", "open the line"}}
	if strings.HasPrefix(c.WaitingFor, waitsOnYou+":") {
		hs = append(hs, hint{"r", "retry"})
	}
	return append(hs, hint{"d", "diff"}, hint{"l", "log"}, hint{"+ " + p.minus(), "rate"}, hint{"esc", "back"}, hint{"?", "more"})
}

// waitingRows is the WAITING FOR block: who the task waits on, in the color
// of the home list, and why.
func (p *cardPainter) waitingRows(text string) []string {
	const label = "WAITING FOR"
	x := 1 + len(label) + 2
	who, _, ok := strings.Cut(text, ": ")
	ok = ok && who != "" && !strings.Contains(who, " ")
	lines := wrapText(text, p.wrapW(x), 3)
	cells := make([]cell, len(lines))
	for i, l := range lines {
		cells[i] = cell{{l, noStyle}}
		if i == 0 && ok && strings.HasPrefix(l, who+":") {
			cells[i] = cell{{who + ":", p.whoStyle(WaitsOn{Who: who})}, {strings.TrimPrefix(l, who+":"), noStyle}}
		}
	}
	return p.labelRowsAs(label, p.st.Section, x, cells)
}

// heading is a section's name with its text, from column 1.
func (p *cardPainter) heading(label string, c cell) string {
	return p.line(at(1, len(label), seg{label, p.st.Section}), placed{1 + len(label) + 2, p.width - len(label) - 3, c})
}

// stageMark is the mark of a stage, as the home list's.
func (p *cardPainter) stageMark(state string) seg {
	switch state {
	case "done":
		return seg{p.tick(), p.st.OK}
	case "working":
		return seg{p.spinnerFrame(), p.st.Accent}
	case "waiting":
		return seg{p.markGlyph(MarkWaits), p.st.Dim}
	case "failed":
		return seg{p.g.fail, p.st.Err}
	case "skipped":
		return seg{p.dash(), p.st.Dim}
	}
	if p.ascii() {
		return seg{".", p.st.Dim}
	}
	return seg{"·", p.st.Dim}
}

// selected draws a line under the mark: the cursor and the whole line in
// reverse video, to the edge, as the home list does. A line that starts in the
// second cell (a heading) is pushed one cell right by the mark.
func (p *cardPainter) selected(line string) string {
	text := strings.TrimRight(ansi.Strip(line), " ")
	rest, ok := strings.CutPrefix(text, noMark)
	if !ok {
		rest = strings.TrimPrefix(text, " ")
	}
	rest = ansi.Truncate(rest, p.width-len(noMark), "")
	rest += spaces(p.width - len(noMark) - ansi.StringWidth(rest))
	return p.cursorMark() + p.st.Selected.Render(rest)
}

func (p *cardPainter) taskBody(c TaskCard) (lines []string, cursor int) {
	cursor = -1
	stops := 0
	stop := func(line string) {
		if stops == c.Cursor {
			line, cursor = p.selected(line), len(lines)
		}
		stops++
		lines = append(lines, line)
	}
	add := func(l string) { lines = append(lines, l) }
	p.stageRows(c, stop, add)
	if len(c.Agents) > 0 || c.AgentsNote != "" {
		add("")
		p.agentRows(c, stop, add)
	}
	if len(c.Report) > 0 {
		lines = append(lines, "")
		stop(p.line(at(1, len("REPORT"), seg{"REPORT", p.st.Section})))
		labelW := 0
		labels := make([]string, len(c.Report))
		texts := make([]string, len(c.Report))
		for i, r := range c.Report {
			labels[i], texts[i], _ = strings.Cut(r, " ")
			labelW = max(labelW, ansi.StringWidth(labels[i]))
		}
		for i := range c.Report {
			lines = append(lines, p.line(at(2, labelW, seg{labels[i], p.st.Header}),
				at(2+labelW+3, p.width-labelW-5, seg{texts[i], noStyle})))
		}
	}
	if n := len(c.Learned); n > 0 {
		add("")
		text := fmt.Sprintf("%d from this task", n)
		if n == 1 {
			text += ": " + c.Learned[0]
		}
		const x = 1 + len("LEARNED") + 2
		cells := p.keyedCells(wrapText(text+" (L)", p.wrapW(x), 3))
		lines = append(lines, p.labelRowsAs("LEARNED", p.st.Section, x, cells)...)
		if n > 1 {
			for _, l := range c.Learned {
				for _, w := range wrapText(l, p.wrapW(3), 2) {
					add(p.line(at(3, p.width-3, seg{w, noStyle})))
				}
			}
		}
	}
	return lines, cursor
}

// keyedCells are wrapped lines of text whose end names a key: the " (L)" of
// the last line is dim.
func (p *cardPainter) keyedCells(lines []string) []cell {
	cells := plainCells(lines, noStyle)
	if n := len(lines); n > 0 {
		if body, ok := strings.CutSuffix(lines[n-1], "(L)"); ok {
			cells[n-1] = cell{{body, noStyle}, {"(L)", p.st.Dim}}
		}
	}
	return cells
}

// stageRows are the STAGES lines: a mark, the name, the proof in words, and
// on the right how long the stage took and when it finished.
func (p *cardPainter) stageRows(c TaskCard, stop, add func(string)) {
	nameW, tookW, finW := 0, len("took"), len("finished")
	took := make([]string, len(c.Stages))
	fin := make([]string, len(c.Stages))
	for i, s := range c.Stages {
		nameW = max(nameW, ansi.StringWidth(s.Name))
		switch s.State {
		case "done", "working", "waiting", "failed":
			took[i] = tookText(s.Took)
		}
		switch {
		case s.State == "working":
			fin[i] = "now"
		case s.State == "waiting":
			fin[i] = "waiting"
		case (s.State == "done" || s.State == "failed") && !s.Finished.IsZero():
			fin[i] = p.clock(s.Finished)
		}
		tookW, finW = max(tookW, len(took[i])), max(finW, len(fin[i]))
	}
	nameW = min(nameW, cardNameMax)
	proofX := 4 + nameW + 2
	edge := p.width - 2
	finX := edge - finW
	tookEdge := finX - 3
	showTook, showFin := true, true
	if tookEdge-tookW-2-proofX < 16 {
		showTook = false
	}
	if !showTook && finX-2-proofX < 16 {
		showFin = false
	}
	proofEnd := edge
	switch {
	case showTook:
		proofEnd = tookEdge - tookW - 2
	case showFin:
		proofEnd = finX - 2
	}
	var heads []placed
	if showTook {
		heads = append(heads, rightAt(tookEdge, cell{{"took", p.st.Header}}))
	}
	if showFin {
		heads = append(heads, rightAt(edge, cell{{"finished", p.st.Header}}))
	}
	add(p.line(append([]placed{at(1, len("STAGES"), seg{"STAGES", p.st.Section})}, heads...)...))
	for i, s := range c.Stages {
		text := noStyle
		if s.State == "skipped" || s.State == "todo" {
			text = p.st.Dim
		}
		ps := []placed{at(2, 1, p.stageMark(s.State)), at(4, nameW, seg{s.Name, text}), at(proofX, max(proofEnd-proofX, 1), seg{s.Proof, text})}
		if showTook && took[i] != "" {
			ps = append(ps, rightAt(tookEdge, cell{{took[i], p.st.Dim}}))
		}
		if showFin && fin[i] != "" {
			ps = append(ps, rightAt(edge, cell{{fin[i], p.st.Dim}}))
		}
		stop(p.line(ps...))
	}
}

// agentRows are the AGENTS lines: the count, then a mark, the name, what the
// agent is for (or does now) and its state.
func (p *cardPainter) agentRows(c TaskCard, stop, add func(string)) {
	working, done := 0, 0
	for _, a := range c.Agents {
		switch a.State {
		case "working":
			working++
		case "done":
			done++
		}
	}
	summary := "none working"
	if working > 0 {
		summary = strconv.Itoa(working) + " working"
	}
	if c.AgentsNote != "" {
		summary += ": " + c.AgentsNote
	}
	if done > 0 {
		summary += p.g.sep + strconv.Itoa(done) + " done, logs kept"
	}
	add(p.heading("AGENTS", cell{{summary, p.st.Dim}}))
	nameW, whyW := 0, 0
	whys := make([]string, len(c.Agents))
	states := make([]cell, len(c.Agents))
	for i, a := range c.Agents {
		whys[i] = a.Why
		if a.State == "working" && a.Now != "" {
			whys[i] = a.Now
		}
		states[i] = p.agentState(a)
		nameW, whyW = max(nameW, ansi.StringWidth(a.Name)), max(whyW, ansi.StringWidth(whys[i]))
	}
	nameW = min(nameW, cardNameMax)
	whyX := 4 + nameW + cardGap
	edge := p.width - 2
	stateW := 0
	for _, s := range states {
		stateW = max(stateW, s.width())
	}
	stateX := whyX + whyW + cardGap
	if stateX+stateW > edge {
		if whyW = max(edge-stateW-cardGap-whyX, 16); whyX+whyW+cardGap+stateW > edge {
			stateW = max(edge-whyX-whyW-cardGap, 0)
		}
		stateX = whyX + whyW + cardGap
	}
	for i, a := range c.Agents {
		// Yellow means on you: only an agent that waits on the operator has it.
		mark := p.stageMark("")
		switch a.State {
		case "working":
			mark = seg{p.spinnerFrame(), p.st.Accent}
		case "done":
			mark = seg{p.tick(), p.st.OK}
		case "failed":
			mark = seg{p.g.fail, p.st.Err}
		case agentWaitsOnYou:
			mark = seg{p.markGlyph(MarkNeedsYou), p.st.Warn}
		}
		ps := []placed{at(2, 1, mark), at(4, nameW, seg{a.Name, noStyle}), at(whyX, whyW, seg{whys[i], noStyle})}
		if stateW > 0 {
			ps = append(ps, placed{stateX, stateW, states[i]})
		}
		stop(p.line(ps...))
	}
}

// agentState is how an agent is doing: "working 10m · output 3s ago",
// "done 12:24".
func (p *cardPainter) agentState(a CardAgent) cell {
	switch a.State {
	case "working":
		text := "working"
		if a.For > 0 {
			text += " " + shortAge(a.For)
		}
		if !a.Output.IsZero() {
			text += p.g.sep + "output " + agoText(p.now.Sub(a.Output))
		}
		return cell{{text, noStyle}}
	case "":
		return cell{{"unknown", p.st.Dim}}
	}
	text := a.State
	if !a.Output.IsZero() {
		text += " " + p.clock(a.Output)
	}
	return cell{{text, p.st.Dim}}
}
