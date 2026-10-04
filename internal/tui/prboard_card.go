package tui

// The PR board's details card and help overlay.

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

// helpContent is the keys and the legend, at most width-6 cells wide (the
// box adds a border and padding): two columns of keys when they fit.
func (p prbPainter) helpContent(width int) []string {
	inner := max(width-6, 10)
	section := func(title string, hs []hint) string {
		keyW := 0
		for _, h := range hs {
			keyW = max(keyW, ansi.StringWidth(h.key))
		}
		lines := []string{p.st.Section.Render(title)}
		for _, h := range hs {
			lines = append(lines, p.st.Key.Render(h.key)+spaces(keyW-ansi.StringWidth(h.key)+2)+h.desc)
		}
		return strings.Join(lines, "\n")
	}
	nav := section("Move and view", []hint{
		{"j/k ↑/↓", "move"}, {"g / G", "first / last PR"}, {"pgup/pgdn", "a page up / down"},
		{"enter", "details card"}, {"s / S", "next sort / reverse it"}, {"/", "filter (fuzzy)"},
		{"v", "next view: all, magnum, mine, ready"},
		{"esc", "back, clear the filter, quit"}, {"ctrl+r / F5", "refresh now"}, {"tab", "status dashboard"},
		{"W", "reset the column widths"}, {"?", "this help"}, {"q", "quit"},
	})
	acts := section("Act on the PR", []hint{
		{"r", "review now (asks y/N)"}, {"R", "fresh review in new agent sessions (asks y/N)"},
		{"i", "review with /simplify (asks y/N)"}, {"o", "open the " + judgeName(p.judge) + " pane"},
		{"b", "open in the browser"}, {"p / u", "pin / unpin"}, {"M / U", "mute / unmute (asks y/N)"},
		{"x", "release (asks y/N)"}, {"K", "kill the running review (asks y/N)"},
		{"I", "ignore: kill, mute, free slot; U undoes"}, {"a", "jump to what needs attention"},
		{"y", "answer yes; any other key, enter too, cancels"},
	})
	keys := lipgloss.JoinHorizontal(lipgloss.Top, nav, "     ", acts)
	if lipgloss.Width(keys) > inner {
		keys = nav + "\n\n" + acts
	}
	lines := strings.Split(keys, "\n")
	lines = append(lines, "", p.st.Section.Render("Views and filter"))
	for _, l := range []string{
		"magnum: what magnum reviewed or is reviewing · mine: assigned to you or your review requested · " +
			"ready: approved, no changes requested, not a draft",
		"/ words match fuzzily; state:<s> assignee:<login> author:<login> (@me: you) review:requested narrow it; " +
			"a,b lists alternatives",
	} {
		lines = append(lines, strings.Split(p.st.Dim.Width(inner).Render(l), "\n")...)
	}
	lines = append(lines, "")
	lines = append(lines, strings.Split(section("Mouse", mouseHelp(true, "opens the card")), "\n")...)
	lines = append(lines, strings.Split(p.st.Dim.Width(inner).Render(mouseSelectNote), "\n")...)

	g := p.g
	lines = append(lines, "", p.st.Section.Render("Legend"))
	lines = append(lines, flow([]string{
		p.pal.green.Render(g.approved) + " approved", p.pal.red.Render(g.changes) + " changes requested",
		p.pal.yellow.Render(g.commented) + " commented", p.st.Dim.Render(g.pending) + " requested",
		p.st.Dim.Render(g.dismissed) + " dismissed", p.pal.yellow.Render(g.stale) + " stale: the head moved since",
		p.pal.mine.Render(g.mine) + " yours", g.pin + " pinned", p.st.Err.Render(g.errMark) + " last round failed",
		p.pal.tag.Render("muted") + " dimmed row",
	}, "   ", inner)...)
	ex := &ReviewDelta{Base: "reviewed", Commits: 3, Additions: 41, Deletions: 7}
	since := p.sinceCell(ex, p.sinceWidths([]PRBoardRow{{SinceReview: ex}})).render(nil) +
		"  commits, lines added and removed since the last review " + p.st.Dim.Render("(dim: the whole PR, never reviewed)")
	lines = append(lines, strings.Split(lipgloss.NewStyle().Width(inner).Render(since), "\n")...)
	var pills []string
	for _, st := range prStateOrder {
		pills = append(pills, p.stateCell(st).render(nil))
	}
	lines = append(lines, flow(pills, " ", inner)...)
	for i, l := range lines {
		lines[i] = truncate(strings.TrimRight(l, " "), inner)
	}
	return lines
}

// help is the help box centered in width x height, its content scrolled
// by scroll lines; below counts the lines out of view.
func (p prbPainter) help(width, height, scroll int) ([]string, int) {
	lines := p.helpContent(width)
	room := max(height-2, 1)
	scroll = min(max(scroll, 0), max(len(lines)-room, 0))
	end := min(scroll+room, len(lines))
	box := p.st.Box.BorderForeground(p.pal.ruleColor).Padding(0, 2)
	out := strings.Split(lipgloss.PlaceHorizontal(width, lipgloss.Center, box.Render(strings.Join(lines[scroll:end], "\n"))), "\n")
	for i := range out {
		out[i] = truncate(strings.TrimRight(out[i], " "), width)
	}
	return out, len(lines) - end
}

// cardInner is the card's text width inside its border and padding.
func cardInner(width int) int { return max(width-4, 4) }

// card is the details of r in a bordered box of width x height, its
// content scrolled by scroll lines; below counts the lines out of view.
func (p prbPainter) card(r PRBoardRow, width, height, scroll int) ([]string, int) {
	lines := p.cardContent(r, cardInner(width))
	room := max(height-2, 1)
	scroll = min(max(scroll, 0), max(len(lines)-room, 0))
	end := min(scroll+room, len(lines))
	box := p.st.Box.BorderForeground(p.pal.ruleColor).Width(max(width, 8))
	return strings.Split(box.Render(strings.Join(lines[scroll:end], "\n")), "\n"), len(lines) - end
}

// cardContent is the card's lines, each at most inner cells wide.
func (p prbPainter) cardContent(r PRBoardRow, inner int) []string {
	var lines []string
	add := func(s ...string) {
		for _, l := range s {
			lines = append(lines, truncate(l, inner))
		}
	}
	dash := p.st.Dim.Render(p.g.dash)
	orDim := func(s string) string {
		if strings.TrimSpace(s) == "" {
			return dash
		}
		return s
	}

	head := p.st.Title.Render(orDim(prRef(r))) + "  " + p.stateCell(r.State).render(nil)
	var flags []string
	if r.Pinned {
		flags = append(flags, p.g.pin+" pinned")
	}
	if r.Draft {
		flags = append(flags, p.pal.tag.Render("draft"))
	}
	if r.Muted {
		flags = append(flags, p.pal.tag.Render("muted"))
	}
	if gh := strings.ToUpper(strings.TrimSpace(r.GHState)); gh != "" && gh != "OPEN" {
		flags = append(flags, p.st.Warn.Render(strings.ToLower(gh)))
	}
	if len(flags) > 0 {
		head += "  " + strings.Join(flags, p.st.Dim.Render(p.g.sep))
	}
	add(head)
	title := lipgloss.NewStyle().Bold(true).Width(inner).Render(orDim(oneLine(r.Title)))
	tl := strings.Split(title, "\n")
	if len(tl) > 3 {
		tl = append(tl[:2], truncate(tl[2]+" …", inner))
	}
	add(tl...)
	if url := prURL(r); url != "" {
		add(p.st.Accent.Render(url))
	}
	add("")

	// Facts, in two columns when there is room.
	when := func(t time.Time) string {
		if t.IsZero() {
			return dash
		}
		return t.Local().Format("Jan 2 15:04") + p.st.Dim.Render(" · "+HumanAgo(max(p.now.Sub(t), time.Second)))
	}
	logins := func(ls []string) string {
		if len(ls) == 0 {
			return dash
		}
		out := make([]string, len(ls))
		for i, l := range ls {
			out[i] = p.loginStyle(l, false).Render(l)
		}
		return strings.Join(out, ", ")
	}
	next := dash
	if t := r.NextEligibleAt; !t.IsZero() {
		if t.After(p.now) {
			next = "in " + HumanDuration(t.Sub(p.now)) + p.st.Dim.Render(" · "+t.Local().Format("15:04"))
		} else {
			next = p.st.OK.Render("eligible now")
		}
	}
	if _, rest, ok := strings.Cut(r.Wait, " · "); ok && rest != "" {
		next = rest
	}
	gh := orDim(strings.ToLower(r.GHState))
	if r.Draft {
		gh += p.st.Dim.Render(" · draft")
	}
	labels := dash
	if len(r.Labels) > 0 {
		labels = p.st.Accent.Render(strings.Join(r.Labels, ", "))
	}
	facts := [][2]string{
		{"Author", logins([]string{r.Author})},
		{"Assignees", logins(r.Assignees)},
		{"Labels", labels},
		{"Updated", when(r.UpdatedAt)},
		{"GitHub", gh},
		{"Head", orDim(shortSHA(r.HeadSHA))},
		{"Slot", orDim(r.Slot)},
		{"Notes", map[bool]string{true: "yes", false: "no"}[r.Notes]},
		{"Next review", next},
		{"Rounds today", strconv.Itoa(r.RoundsToday)},
	}
	if r.Author == "" {
		facts[0][1] = dash
	}
	const labelW = 14
	fact := func(f [2]string, w int) string {
		return fit(p.st.Header.Render(f[0])+spaces(labelW-ansi.StringWidth(f[0]))+f[1], w)
	}
	if inner >= 96 {
		colW := (inner - 4) / 2
		half := (len(facts) + 1) / 2
		for i := range half {
			l := fact(facts[i], colW)
			if j := i + half; j < len(facts) {
				l += "    " + fact(facts[j], colW)
			}
			add(strings.TrimRight(l, " "))
		}
	} else {
		for _, f := range facts {
			add(strings.TrimRight(fact(f, inner), " "))
		}
	}

	if r.WaitDetail != "" {
		add("", p.st.Section.Render("WAITING"))
		for _, l := range strings.Split(lipgloss.NewStyle().Width(max(inner-2, 10)).Render(r.WaitDetail), "\n") {
			add("  " + l)
		}
	}

	add("", p.st.Section.Render("SINCE REVIEW"))
	for _, l := range p.sinceSentence(r) {
		add("  " + l)
	}
	add("", p.st.Section.Render("LAST REVIEW"))
	for _, l := range p.lastReviewSentence(r) {
		add("  " + l)
	}
	if r.Note != "" {
		add("  " + p.st.Dim.Render(r.Note))
	}
	if t := r.LastRound; t != nil {
		head := fmt.Sprintf("LAST ROUND (%d", t.Round)
		if t.Kind != "" {
			head += ", " + t.Kind
		}
		add("", p.st.Section.Render(head+")"))
		for _, l := range flow(p.timingParts(*t), p.st.Dim.Render(" · "), max(inner-2, 10)) {
			add("  " + l)
		}
	}

	add("", p.st.Section.Render(fmt.Sprintf("REVIEWERS (%d)", len(r.Reviewers))))
	if len(r.Reviewers) == 0 {
		add("  " + p.st.Dim.Render("nobody yet"))
	} else {
		add(p.reviewerTable(r, inner)...)
	}

	if r.LastError != "" {
		title := "LAST ERROR"
		if normState(r.State) == "needs_attention" {
			title = "NEEDS YOU"
		}
		wrap := func(s string) []string {
			return strings.Split(lipgloss.NewStyle().Width(max(inner-2, 10)).Render(oneLine(s)), "\n")
		}
		add("", p.st.Err.Render(title))
		for _, l := range wrap(r.LastError) {
			add("  " + p.pal.red.Render(l))
		}
		if r.ErrorFix != "" {
			for i, l := range wrap("fix: " + r.ErrorFix) {
				if i > 0 {
					l = "     " + l
				}
				add("  " + l)
			}
		}
		if len(r.ErrorDetail) > 0 {
			add("  " + p.st.Dim.Render("output:"))
			for _, l := range r.ErrorDetail {
				add("    " + p.st.Dim.Render(truncate(oneLine(l), max(inner-4, 10))))
			}
		}
	}

	acts := []hint{hint{"r", "review"}, hint{"R", "fresh review"}, hint{"i", "simplify"}, hint{"o", "open pane"}, hint{"b", "browser"},
		hint{"p", "pin"}, hint{"u", "unpin"}, hint{"M", "mute"}, hint{"U", "unmute"}, hint{"x", "release"},
		hint{"K", "kill review"}, hint{"I", "ignore"}, hint{"esc", "back"}}
	if normState(r.State) == "ignored" { // U undoes the ignore; ignoring again means nothing
		acts = slices.DeleteFunc(acts, func(h hint) bool { return h.key == "I" })
		for i := range acts {
			if acts[i].key == "U" {
				acts[i].desc = "unmute: stop ignoring"
			}
		}
	}
	add("", p.st.Section.Render("ACTIONS"))
	for _, l := range p.st.wrapHints(inner-2, acts...) {
		add("  " + l)
	}
	return lines
}

// sinceSentence says what changed since the review, with a second line
// when the counts are lower bounds.
func (p prbPainter) sinceSentence(r PRBoardRow) []string {
	d := r.SinceReview
	if d == nil {
		return []string{p.st.Dim.Render("unknown: no compare yet")}
	}
	a, del := "+"+strconv.Itoa(d.Additions), p.g.minus+strconv.Itoa(d.Deletions) // exact in the card
	nums := plural(d.Commits, "commit", "commits") + p.st.Dim.Render(" · ") + plural(d.Files, "file", "files") +
		p.st.Dim.Render(" · ") + p.pal.add.Render(a) + " " + p.pal.del.Render(del)
	var s string
	switch {
	case isBaseDelta(d):
		s = nums + p.st.Dim.Render(" against the base branch: never reviewed")
	case d.Commits == 0 && d.Additions == 0 && d.Deletions == 0:
		s = p.st.OK.Render("nothing new") + p.st.Dim.Render(" since the reviewed head "+shortSHA(d.BaseSHA))
	default:
		s = nums + p.st.Dim.Render(" since the reviewed head "+shortSHA(d.BaseSHA))
	}
	if d.Truncated {
		return []string{s, p.st.Dim.Render("at least: GitHub truncated the compare")}
	}
	return []string{s}
}

// lastReviewSentence is who reviewed, the verdict, when and on which
// commit, with a second line when the head moved since.
func (p prbPainter) lastReviewSentence(r PRBoardRow) []string {
	li := r.LastReview
	if li == nil {
		return []string{p.st.Dim.Render("none yet")}
	}
	glyph, _, long, vst := p.verdict(normVerdict(li.Event))
	who := p.loginStyle(li.Login, li.Mine).Render(orDash(li.Login))
	if p.isMine(li.Login, li.Mine) {
		who = p.pal.mine.Render(p.g.mine+" ") + who
	}
	s := who + "  " + vst.Render(glyph+" "+long)
	if !li.SubmittedAt.IsZero() {
		s += "  " + li.SubmittedAt.Local().Format("Jan 2 15:04") + p.st.Dim.Render(" · "+HumanAgo(max(p.now.Sub(li.SubmittedAt), time.Second)))
	}
	if li.CommitSHA != "" {
		s += p.st.Dim.Render("  on ") + shortSHA(li.CommitSHA)
	}
	if !li.Stale {
		return []string{s}
	}
	moved := "the head moved since"
	if r.HeadSHA != "" {
		moved = "the head moved to " + shortSHA(r.HeadSHA) + " since"
	}
	return []string{s, p.pal.yellow.Render(p.g.stale + " stale: " + moved)}
}

// reviewerTable lists every reviewer: login, verdict, when, on which
// commit and whether it is stale or requested.
func (p prbPainter) reviewerTable(r PRBoardRow, inner int) []string {
	chipsOrder := slices.Clone(r.Reviewers)
	slices.SortStableFunc(chipsOrder, func(a, b ReviewerInfo) int {
		ma, mb := p.isMine(a.Login, a.Mine), p.isMine(b.Login, b.Mine)
		if ma != mb {
			if ma {
				return -1
			}
			return 1
		}
		return cmp.Compare(verdictRank(normVerdict(a.Verdict)), verdictRank(normVerdict(b.Verdict)))
	})
	cols := []column{{title: "LOGIN", min: 8}, {title: "VERDICT", min: 9}, {title: "SUBMITTED", min: 9, flex: true}, {title: "COMMIT", min: 7}, {title: "NOTE", min: 6, flex: true}}
	rows := make([][]string, len(chipsOrder))
	for i, v := range chipsOrder {
		verdict := normVerdict(v.Verdict)
		glyph, _, long, vst := p.verdict(verdict)
		if verdict == "pending" && v.Requested {
			long = "requested"
		}
		login := p.loginStyle(v.Login, v.Mine).Render(v.Login)
		if p.isMine(v.Login, v.Mine) {
			login = p.pal.mine.Render(p.g.mine+" ") + login
		}
		submitted := p.st.Dim.Render(p.g.dash)
		if !v.SubmittedAt.IsZero() {
			submitted = v.SubmittedAt.Local().Format("Jan 2 15:04") + p.st.Dim.Render(" · "+shortAge(max(p.now.Sub(v.SubmittedAt), 0)))
		}
		commit := p.st.Dim.Render(p.g.dash)
		if v.CommitSHA != "" {
			commit = shortSHA(v.CommitSHA)
			if r.HeadSHA != "" && !strings.HasPrefix(r.HeadSHA, v.CommitSHA) && !strings.HasPrefix(v.CommitSHA, r.HeadSHA) {
				commit = p.st.Dim.Render(commit)
			}
		}
		var notes []string
		if v.Stale {
			notes = append(notes, p.pal.yellow.Render(p.g.stale+" stale"))
		}
		if v.Requested {
			notes = append(notes, p.st.Dim.Render("review requested"))
		}
		rows[i] = []string{login, vst.Render(glyph + " " + long), submitted, commit, strings.Join(notes, p.st.Dim.Render(", "))}
	}
	widths := layoutColumns(cols, rows, inner, 2)
	out := []string{"  " + p.st.headerRow(cols, widths)}
	for _, row := range rows {
		out = append(out, "  "+renderRow(row, widths))
	}
	return out
}

// timingParts are the round's stages and its total, styled: a running
// stage in the accent color, a failed one in red.
func (p prbPainter) timingParts(t RoundTimings) []string {
	part := func(name string, d time.Duration, running, failed bool) string {
		v := StageDuration(d)
		switch {
		case running:
			v = p.st.Accent.Render(v + " running")
		case failed:
			v = p.pal.red.Render(v + " failed")
		}
		return p.st.Dim.Render(name+" ") + v
	}
	out := make([]string, 0, len(t.Stages)+1)
	for _, s := range t.Stages {
		out = append(out, part(s.Name, s.Duration, s.Running, s.Failed))
	}
	return append(out, part("total", t.Total, t.Running, false))
}
