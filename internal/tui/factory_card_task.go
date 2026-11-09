package tui

// The factory's task card (spec 9.C, "Task card at 140"), cut to the sections
// of milestone 1a: the header, WAITING FOR, the PR's own lines (on a
// published task's card and on your PR's, which has nothing else), the
// STAGINGS and JIRA lines, STAGES, AGENTS, REPORT and LEARNED. See factory_card.go for the painter it shares with the other cards.

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func (p *cardPainter) task(c TaskCard, height, scroll int) (string, int) {
	p.f.Spinner = c.Spinner
	facts := []string{c.Branch, c.PR, c.Copy}
	if c.Repo != "" { // the repository and its PR first: "example/talkable #4530"
		facts = []string{strings.TrimSpace(c.Repo + " " + c.PR), c.Branch, c.Copy}
	}
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
		head = append(head, p.fixNeedRows(c.FixNeeds)...)
		head = append(head, p.waitLinkRows(c.Links)...)
		head = append(head, "")
	}
	body, cursor := p.taskBody(c)
	foot := []string{p.hintsLine(orHints(c.Hints, p.taskHints(c)))}
	switch {
	case c.Ask != "" && c.askWrap: // all it names shows, over up to half the screen (all of it without a height), as the board's
		most := 0
		if height > 0 {
			most = max(height/2, askMaxLines)
		}
		foot = p.askLinesIn(most, c.Ask)
	case c.Ask != "":
		foot = p.askLines(c.Ask)
	}
	start := scroll
	if height > 0 && cursor >= 0 {
		room := max(height-len(head)-1-len(foot), 1)
		start = min(max(start, cursor-room+1), cursor)
	}
	return p.assemble(head, body, start, c.rule, foot, height)
}

// taskHints are the task card's own key hints. A task a retry can go on
// for offers r: retry, which goes on at its stage (spec 9.D); your PR's
// card (no stages, its PR's lines) offers its PR's keys.
func (p *cardPainter) taskHints(c TaskCard) []hint {
	if len(c.Stages) == 0 && c.PRInfo != nil { // your PR's card: its PR's keys
		return []hint{{"f", "fix"}, {"F", "fix off"}, {"o", "magnum's pane"}, {"b", "PR"}, {"esc", "back"}, {"?", "more"}}
	}
	hs := []hint{{"j k", "move"}, {"enter", "open the line"}}
	if c.Retry {
		hs = append(hs, hint{"r", "retry"})
	}
	if c.AcceptQA != "" {
		hs = append(hs, hint{acceptQAKey, "accept QA"})
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

// cardLinksShown bounds the links WAITING FOR lists; a last line counts the
// rest.
const cardLinksShown = 5

// waitLinkRows are the links of what the task waits on you for, under
// WAITING FOR's text: each without its scheme, cut from the left when it
// must be (its end names the thread), at most cardLinksShown and a line
// that counts the rest.
func (p *cardPainter) waitLinkRows(links []string) []string {
	const x = 1 + len("WAITING FOR") + 2
	w := max(p.width-1-x, 1)
	shown := links[:min(len(links), cardLinksShown)]
	out := make([]string, 0, len(shown)+1)
	for _, l := range shown {
		u := strings.TrimPrefix(strings.TrimPrefix(l, "https://"), "http://")
		out = append(out, p.line(placed{x, w, cell{{u, p.st.Accent}}.fitLeft(w)}))
	}
	if n := len(links) - len(shown); n > 0 {
		out = append(out, p.line(at(x, w, seg{fmt.Sprintf("+%d more", n), p.st.Dim})))
	}
	return out
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
	lines = append(lines, p.prSection(c)...)
	if len(c.Stages) > 0 || c.PRInfo == nil { // your PR's card is no task's: it has none
		p.stageRows(c, stop, add)
	}
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

// prSection is the PR's own lines (CardPR) at the top of the card's body:
// PR, CI, REVIEW, APPROVALS, THREADS and FIX LOOP, each under its label in
// WAITING FOR's column and only when it has its data, cut at the edge; the
// CI, findings and approvals in the board's cells, as home's columns draw
// them, the CI with why GitHub blocks the merge and the failed jobs'
// pages under it (ciLinkRows); then STAGINGS, the PR on each staging
// server (stagingsText) and under it the PR's staging deploys
// (stagingDeployRows), and JIRA, where its issue stands (jiraText), why
// its last read failed and Jira done while the PR is open (jiraDoneOpen),
// with or without the PR's lines; a blank line after them.
func (p *cardPainter) prSection(card TaskCard) []string {
	const x = 1 + len("WAITING FOR") + 2
	c := card.PRInfo
	var out []string
	row := func(label string, cl cell) {
		if cl.width() > 0 {
			out = append(out, p.line(at(1, x-1, seg{label, p.st.Section}), placed{x, max(p.width-1-x, 1), cl}))
		}
	}
	words := func(cl cell, text string) cell {
		switch {
		case text == "":
			return cl
		case cl.width() > 0:
			cl = append(cl, p.sepSeg())
		}
		return append(cl, seg{text, noStyle})
	}
	if c != nil {
		row("PR", words(nil, c.State))
		if ci := c.CI; ci != nil {
			// GitHub's block beside the cell, or under it when it does not fit.
			cl, under := p.besides(x, p.ciText(FactoryRow{CI: ci}, formLong), seg{ci.Blocked, p.st.Warn})
			row("CI", cl)
			out = append(out, under...)
			out = append(out, p.ciLinkRows(ci.Links)...)
		}
		row("REVIEW", words(p.findingsText(FactoryRow{Findings: c.Findings}, formLong), c.Review))
		row("APPROVALS", words(p.apprText(FactoryRow{Approvals: c.Approvals}, formLong), c.Reviewers))
		row("THREADS", words(nil, c.Threads))
		row("FIX LOOP", words(nil, c.FixLoop))
	}
	cells := p.stagingsText(card.Stagings)
	row("STAGINGS", cells)
	if len(card.StagingDeploys) > 0 {
		if cells.width() == 0 { // no server's cell: the label stands over the deploys alone
			out = append(out, p.line(at(1, x-1, seg{"STAGINGS", p.st.Section})))
		}
		out = append(out, p.stagingDeployRows(x, card.StagingDeploys)...)
	}
	if js := card.JiraStatus; js != nil {
		more := append(p.jiraParts(js), p.jiraFailure(js), p.jiraDoneOpen(card))
		cl, under := p.besides(x, p.jiraText(cmp.Or(card.Issue, card.Key), js, p.width-1-x), more...)
		row("JIRA", cl)
		out = append(out, under...)
	}
	if jp := card.JiraPlan; jp != nil {
		row("JIRA MOVE", p.jiraPlanText(jp))
	}
	if len(out) > 0 {
		out = append(out, "")
	}
	return out
}

// besides is cl with each of more after it, one by one, while the line
// from column x holds them, and the rest on the lines under it, as many on
// a line as its wrapped width holds, joined by the separator; one that no
// line holds is wrapped to at most two lines of its own, in its own style.
// A seg without text is left out.
func (p *cardPainter) besides(x int, cl cell, more ...seg) (cell, []string) {
	var under []string
	var cur cell // the line under cl being filled
	flush := func() {
		if len(cur) > 0 {
			under = append(under, p.line(placed{x, p.width - x, cur}))
			cur = nil
		}
	}
	room := p.wrapW(x)
	for _, m := range more {
		switch {
		case m.text == "":
			continue
		case len(under) == 0 && len(cur) == 0:
			if with := append(slices.Clone(cl), p.sepSeg(), m); with.width() <= p.width-1-x {
				cl = with
				continue
			}
		case len(cur) > 0:
			if with := append(slices.Clone(cur), p.sepSeg(), m); with.width() <= room {
				cur = with
				continue
			}
		}
		flush()
		switch lines := wrapText(m.text, room, 2); len(lines) {
		case 1:
			cur = cell{{lines[0], m.st}}
		default:
			under = append(under, p.textRows(x, m.st, lines)...)
		}
	}
	flush()
	return cl, under
}

// jiraText is the start of the card's JIRA line for the issue key, in
// room cells: its status in its category's style and since when ("In Code
// Review for 13h", "since just now" under a minute), the name cut first;
// for a key no read of which worked, that it never did.
func (p *cardPainter) jiraText(key string, js *FactoryJiraStatus, room int) cell {
	out := cell{{key, p.st.Accent}, p.sepSeg()}
	if js.Name == "" {
		return append(out, seg{"no read of it has worked yet", noStyle})
	}
	var since cell
	switch d := max(p.now.Sub(js.Since), 0); {
	case js.Since.IsZero():
	case d < time.Minute: // "for now" would say it holds the status for a while
		since = cell{{" since just now", noStyle}}
	default:
		since = cell{{" for " + shortAge(d), noStyle}}
	}
	name := cell{{js.Name, jiraStyle(p.st, js)}}
	if w := out.width() + name.width() + since.width(); w > room {
		name = name.fit(max(room-out.width()-since.width(), 1))
	}
	return append(append(out, name...), since...)
}

// jiraParts are the JIRA line's parts after the status: who has the issue
// (you, someone else, nobody, or someone not known to be you) and the last
// read's age.
func (p *cardPainter) jiraParts(js *FactoryJiraStatus) []seg {
	if js.Name == "" {
		return nil
	}
	who := "unassigned"
	switch {
	case js.AssignedToYou:
		who = "assigned to you"
	case js.Assigned && js.YouUnknown:
		who = "assigned (not known to be you)"
	case js.Assigned:
		who = "assigned to someone else"
	}
	parts := []seg{{who, noStyle}}
	if !js.ReadAt.IsZero() {
		parts = append(parts, seg{"last read " + agoText(p.now.Sub(js.ReadAt)), p.st.Dim})
	}
	return parts
}

// jiraDoneOpen is the card's yellow fact when Jira says the issue is done
// while the task's PR is still open (PRInfo: magnum's registry has it open),
// as its row says it; none otherwise, nor on an old read (old).
func (p *cardPainter) jiraDoneOpen(c TaskCard) seg {
	if c.JiraStatus.Category != "done" || c.PRInfo == nil || c.JiraStatus.old() {
		return seg{}
	}
	return seg{"Jira says done while " + cmp.Or(c.PR, "the PR") + " is still open", p.st.Warn}
}

// jiraFailure is why the issue is not read now ("now: Jira did not answer
// within 30s"), in the failure's style: no factory daemon runs, or the
// last try failed; none when it worked.
func (p *cardPainter) jiraFailure(js *FactoryJiraStatus) seg {
	switch {
	case js.DaemonDown:
		return seg{"now: the factory daemon is down", p.st.Err}
	case js.LastError != "":
		return seg{"now: " + js.LastError, p.st.Err}
	}
	return seg{}
}

// jiraPlanText is the card's JIRA MOVE line: the factory's latest plan of
// a move of the task's Jira issue, the move and when it was planned, in
// its mode ("To Do → In Development (planned 14:02, shadow)"), then what
// you did after it ("you moved it 14:20"); nothing to move; why the
// factory could not plan it, for you to move it; a plan that waits for its
// read. The time and the mode are dim.
func (p *cardPainter) jiraPlanText(jp *FactoryJiraPlan) cell {
	mode := cmp.Or(jp.Mode, "shadow")
	when := seg{"(" + p.clock(jp.At) + ", " + mode + ")", p.st.Dim}
	to := "→ " + jp.To + ": "
	text := func(main string, more ...seg) cell {
		return append(cell{{main + " ", noStyle}, when}, more...)
	}
	switch jp.Outcome {
	case "":
		return cell{{to + "planning since " + p.clock(jp.Begun) + " ", noStyle}, {"(" + mode + ")", p.st.Dim}}
	case "planned":
		out := cell{{jp.From + " → " + jp.To + " ", noStyle}, {"(planned " + p.clock(jp.At) + ", " + mode + ")", p.st.Dim}}
		switch jp.Moved {
		case "target":
			out = append(out, p.sepSeg(), seg{"you moved it " + p.clock(jp.MovedAt), noStyle})
		case "elsewhere":
			out = append(out, p.sepSeg(), seg{"you moved it to " + jp.MovedTo + " " + p.clock(jp.MovedAt), noStyle})
		case "none":
			out = append(out, p.sepSeg(), seg{"no move by the task's end", p.st.Dim})
		}
		return out
	case "already":
		if strings.EqualFold(jp.From, jp.To) {
			return text("already " + jp.To + ": nothing to move")
		}
		return text(jp.From + ", past " + jp.To + ": nothing to move")
	case "assigned_other":
		return text(to + "assigned to someone else, move it yourself")
	case "no_transition":
		if len(jp.Transitions) == 0 {
			return text(to + "Jira offers no transition, move it yourself")
		}
		return text(to+"no transition to it, move it yourself", p.sepSeg(), seg{"Jira offers " + strings.Join(jp.Transitions, ", "), p.st.Dim})
	case "needs_fields":
		return text(to + "Jira wants fields for " + jp.Transition + ", move it yourself")
	case "target_done":
		return text(to + "a done status, never the factory's move")
	case "unread":
		return cell{{to + "not planned, its read failed: " + jp.Cause + " ", noStyle}, {"(" + mode + ")", p.st.Dim}}
	}
	return text(to + jp.Outcome)
}

// stagingsText is the card's STAGINGS line: each staging server's name and
// its cell as a wide row draws it, the state and how long it has stood
// ("void: latest 3m · bart: CI 12m · bastion: none"); a server the PR is not
// on says none, in words. None for no cell.
func (p *cardPainter) stagingsText(cells []StagingCell) cell {
	var out cell
	for i, c := range cells {
		if i > 0 {
			out = append(out, p.sepSeg())
		}
		out = append(out, seg{c.Name + ": ", noStyle})
		if c.State == "none" {
			out = append(out, seg{"none", p.st.Dim})
			continue
		}
		out = append(out, p.stageCell(c, formLong)...)
	}
	return out
}

// stagingDeployRows are the PR's staging deploys under the card's STAGINGS
// line, from column x, in the board card's words and colors
// (prbPainter.stagingDeploysLines): each deploy's effect, state, last step
// and how long ago, its last event's words or why it ended, its CI run; a
// deploy that a later one supersedes is one dim line (StagingDeploysFolded).
func (p *cardPainter) stagingDeployRows(x int, ds []StagingDeploy) []string {
	b := p.base
	b.now = p.now
	return b.stagingDeploysLines(ds, spaces(x), p.width-1)
}

// ciCardLinksShown bounds the failed jobs' pages under the card's CI line; a
// last line counts the rest.
const ciCardLinksShown = 3

// ciLinkRows are the failed jobs' pages under the card's CI line, one a
// line in its column: the fail mark, the job's name and its page without
// the scheme; a line too narrow for both keeps the page, cut from the left
// when it must be (its end names the job); at most ciCardLinksShown and a
// line that counts the rest.
func (p *cardPainter) ciLinkRows(links []CheckLink) []string {
	const x = 1 + len("WAITING FOR") + 2
	w := max(p.width-1-x, 1)
	var out []string
	for i, l := range links {
		if i == ciCardLinksShown {
			out = append(out, p.line(at(x, w, seg{fmt.Sprintf("+%d more failed", len(links)-i), p.st.Dim})))
			break
		}
		u := strings.TrimPrefix(strings.TrimPrefix(l.URL, "https://"), "http://")
		cl := cell{{p.g.fail + " ", p.st.Err}, {l.Name + " ", p.st.Dim}, {u, p.st.Accent}}
		if cl.width() > w {
			cl = cell{{u, p.st.Accent}}.fitLeft(w)
		}
		out = append(out, p.line(placed{x, w, cl}))
	}
	return out
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
		case AgentWaitsOnYou:
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
