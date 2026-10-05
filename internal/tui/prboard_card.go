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
		{"v / O", "next view / next owner (all, then each)"},
		{"L", "layout: auto, one line, two lines per PR"},
		{"esc", "back, clear the filter, quit"}, {"ctrl+r / F5", "refresh now"}, {"tab", "status dashboard"},
		{"h / W", "hide ignored and skipped / reset widths"}, {"? / q", "this help / quit"},
	})
	acts := section("Act on the PR", []hint{
		{"r", "review now (asks y/N); post-merge if merged"}, {"R", "fresh review in new agent sessions (asks y/N)"},
		{"i", "review with /simplify (asks y/N)"}, {"o", "open the " + judgeName(p.judge) + " pane"},
		{"b / t", "open the PR / its issue in the browser"}, {"p / u", "pin / unpin"}, {"M / U", "mute / unmute (asks y/N); merged: dismiss flag"},
		{"x", "release (asks y/N)"}, {"K", "kill the running review (asks y/N)"},
		{"I", "ignore: kill, mute, free slot; U undoes"}, {"A / C", "approve / request changes (asks y/N)"}, {"a", "jump to what needs attention"},
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
			"ready: approved on the head, no changes requested, magnum not blocking, required checks passed, not a draft",
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
		p.pal.mine.Render(g.mine) + " yours", p.pinStyle().Render(g.pin) + " pinned", p.st.Err.Render(g.errMark) + " last round failed",
		p.pal.tag.Render("muted") + " dimmed row",
	}, "   ", inner)...)
	lines = append(lines, flow([]string{
		p.stateCell("merged_unreviewed").render(nil) + " GitHub merged it before magnum reviewed its last push",
		p.st.Dim.Render("merged or closed in the last …") + " the recently closed PRs, dimmed, newest first ([board] recent_closed)",
	}, "   ", inner)...)
	prios := make([]string, len(g.priority))
	for i := range prios {
		prios[i] = p.priorityStyle(i).Render(p.priorityMark(i) + fmt.Sprintf("P%d", i))
	}
	lines = append(lines, flow([]string{
		p.st.Header.Render("FINDINGS"), p.pal.red.Render(g.changes) + " blocking", p.pal.yellow.Render(g.nonBlocking) + " non-blocking",
		p.pal.green.Render(g.ok) + " clean", strings.Join(prios, " ") + " by priority, " + g.times + "n how many",
		p.st.Dim.Render(g.simplify) + " simplifications",
	}, "   ", inner)...)
	lines = append(lines, flow([]string{
		p.st.Header.Render("CI"), p.pal.green.Render(g.ciPass) + " passed", p.pal.red.Render(g.ciFail) + " failed",
		p.pal.yellow.Render(g.ciPending) + " pending", p.pal.yellow.Render(g.ciMissing) + " not run", p.pal.yellow.Render(g.ciSkip) + " skipped",
		"65/65 checks done", "Completion +1: the worst required check and how many more", p.pal.yellow.Render(g.stale) + " of an older commit",
	}, "   ", inner)...)
	lines = append(lines, flow([]string{
		p.st.Header.Render("REQUESTED"), p.pal.mine.Render(g.mine) + " 2h: a review asked of you 2h ago",
		p.st.Dim.Render("3d") + " the latest ask of someone else",
	}, "   ", inner)...)
	ex := &ReviewDelta{Base: "reviewed", Commits: 3, Additions: 41, Deletions: 7}
	since := p.sinceCell(ex, p.sinceWidths([]PRBoardRow{{SinceReview: ex}})).render(nil) +
		"  commits, lines added and removed since the last review " + p.st.Dim.Render("(dim: the whole PR, never reviewed)")
	lines = append(lines, strings.Split(lipgloss.NewStyle().Width(inner).Render(since), "\n")...)
	var pills []string
	for _, st := range prStateOrder {
		pills = append(pills, p.stateCell(st).render(nil))
	}
	sep := " "
	if g.rich { // the marks need room to tell the pills apart
		sep = "   "
	}
	lines = append(lines, flow(pills, sep, inner)...)
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

	head := p.st.Title.Render(orDim(prRef(r))) + "  " + p.stateCell(rowState(r)).render(nil)
	var flags []string
	if r.Pinned {
		flags = append(flags, p.pinStyle().Render(p.g.pin)+" pinned")
	}
	for _, b := range r.Badges {
		flags = append(flags, strings.TrimSpace(p.pal.named[b.Color].Render(b.Text)+" "+b.Label))
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
	if r.MergedUnreviewed {
		for _, l := range strings.Split(lipgloss.NewStyle().Width(inner).Render(p.mergedUnreviewedSentence(r)), "\n") {
			add(p.pal.red.Render(l))
		}
	}
	if r.FlagDismissed {
		add(p.st.Dim.Render(truncate(flagDismissedNote, inner)))
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
	issue := dash
	if r.IssueURL != "" {
		issue = p.st.Accent.Render(r.Issue) + p.st.Dim.Render(" · "+r.IssueURL)
	}
	facts := [][2]string{
		{"Author", logins([]string{r.Author})},
		{"Assignees", logins(r.Assignees)},
		{"Labels", labels},
		{"Issue", issue},
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
		add("", p.st.Section.Render(p.g.headed("WAITING")))
		for _, l := range strings.Split(lipgloss.NewStyle().Width(max(inner-2, 10)).Render(r.WaitDetail), "\n") {
			add("  " + l)
		}
	}

	add("", p.st.Section.Render(p.g.headed("SINCE REVIEW")))
	for _, l := range p.sinceSentence(r) {
		add("  " + l)
	}
	add("", p.st.Section.Render(p.g.headed("LAST REVIEW")))
	for _, l := range p.lastReviewSentence(r) {
		add("  " + l)
	}
	if r.Note != "" {
		add("  " + p.st.Dim.Render(r.Note))
	}
	if f := r.Findings; f != nil {
		add("", p.st.Section.Render(p.g.headed("FINDINGS")))
		for _, l := range p.findingsLines(*f) {
			for _, w := range strings.Split(lipgloss.NewStyle().Width(max(inner-2, 10)).Render(l), "\n") {
				add("  " + w)
			}
		}
	}
	if ci := r.CI; ci != nil {
		head := p.st.Section.Render(p.g.headed("CI"))
		if ci.Stale {
			head += " " + p.pal.yellow.Render("(older commit)")
		}
		add("", head)
		for _, l := range p.ciLines(*ci) {
			for _, w := range strings.Split(lipgloss.NewStyle().Width(max(inner-2, 10)).Render(l), "\n") {
				add("  " + w)
			}
		}
	}
	if skipped(r) {
		add("", p.st.Section.Render(p.g.headed("SKIPPED")))
		why := "magnum's configuration skips this PR"
		if r.SkipReason != "" {
			why += ": " + r.SkipReason
		}
		for _, w := range strings.Split(lipgloss.NewStyle().Width(max(inner-2, 10)).Render(why), "\n") {
			add("  " + w)
		}
		add("  " + p.st.Key.Render("R") + " reviews it anyway (asks y/N)")
	}
	if normState(r.State) == "baseline" {
		add("", p.st.Section.Render(p.g.headed("NOT REVIEWED")))
		for _, w := range strings.Split(lipgloss.NewStyle().Width(max(inner-2, 10)).Render(notReviewedSentence), "\n") {
			add("  " + w)
		}
	}
	if t := r.LastRound; t != nil {
		head := p.g.headed("LAST ROUND") + fmt.Sprintf(" (%d", t.Round)
		if t.Kind != "" {
			head += ", " + t.Kind
		}
		add("", p.st.Section.Render(head+")"))
		for _, l := range flow(p.timingParts(*t), p.st.Dim.Render(" · "), max(inner-2, 10)) {
			add("  " + l)
		}
	}

	add("", p.st.Section.Render(fmt.Sprintf("%s (%d)", p.g.headed("REVIEWERS"), len(r.Reviewers))))
	if len(r.Reviewers) == 0 {
		add("  " + p.st.Dim.Render("nobody yet"))
	} else {
		add(p.reviewerTable(r, inner)...)
	}
	if lines := p.requestLines(r, inner); len(lines) > 0 {
		add("")
		add(lines...)
	}

	if r.LastError != "" {
		title := "LAST ERROR"
		if normState(r.State) == "needs_attention" {
			title = "NEEDS YOU"
		}
		wrap := func(s string) []string {
			return strings.Split(lipgloss.NewStyle().Width(max(inner-2, 10)).Render(oneLine(s)), "\n")
		}
		add("", p.st.Err.Render(p.g.headed(title)))
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

	acts := []hint{hint{"r", "review"}, hint{"R", "fresh review"}, hint{"i", "simplify"}, hint{"o", "open pane"}, hint{"b", "browser"}, hint{"t", "tracker"},
		hint{"p", "pin"}, hint{"u", "unpin"}, hint{"M", "mute"}, hint{"U", "unmute"}, hint{"x", "release"},
		hint{"K", "kill review"}, hint{"I", "ignore"}, hint{"A", "approve"}, hint{"C", "request changes"}, hint{"esc", "back"}}
	switch {
	case closedUnmerged(r) || mergedOnGitHub(r) && !postMergeable(r): // nothing left to review
		acts = slices.DeleteFunc(acts, func(h hint) bool { return h.key == "r" || h.key == "R" || h.key == "i" })
	case mergedOnGitHub(r): // the review comments only
		for i := range acts {
			switch acts[i].key {
			case "r":
				acts[i].desc = "post-merge review"
			case "R":
				acts[i].desc = "fresh post-merge review"
			}
		}
	}
	if r.Findings == nil { // nothing magnum reviewed to approve or reject
		acts = slices.DeleteFunc(acts, func(h hint) bool { return h.key == "A" || h.key == "C" })
	}
	if r.IssueURL == "" {
		acts = slices.DeleteFunc(acts, func(h hint) bool { return h.key == "t" })
	}
	switch act, _ := muteActFor(r.GHState, r.MergedUnreviewed, r.FlagDismissed); act {
	case muteDismiss, muteRestore:
		for i := range acts {
			if acts[i].key == "M" {
				acts[i].desc = map[muteAct]string{muteDismiss: "dismiss merged flag", muteRestore: "restore merged flag"}[act]
			}
		}
	case muteNothing: // M only says there is nothing to mute
		acts = slices.DeleteFunc(acts, func(h hint) bool { return h.key == "M" })
	}
	if normState(r.State) == "ignored" { // U undoes the ignore; ignoring again means nothing
		acts = slices.DeleteFunc(acts, func(h hint) bool { return h.key == "I" })
		for i := range acts {
			if acts[i].key == "U" {
				acts[i].desc = "unmute: stop ignoring"
			}
		}
	}
	add("", p.st.Section.Render(p.g.headed("ACTIONS")))
	for _, l := range p.st.wrapHints(inner-2, acts...) {
		add("  " + l)
	}
	return lines
}

// flagDismissedNote is what the card of a merged PR muted after it merged
// unreviewed says in place of the merged-unreviewed sentence.
const flagDismissedNote = "merged-unreviewed flag dismissed (M restores it)"

// mergedUnreviewedSentence says that GitHub merged r before magnum reviewed
// its last push: when, the commit magnum reviewed last and the merged head.
func (p prbPainter) mergedUnreviewedSentence(r PRBoardRow) string {
	when := "on GitHub"
	if !r.ClosedAt.IsZero() {
		when = r.ClosedAt.Local().Format("Jan 2 15:04") + " (" + HumanAgo(max(p.now.Sub(r.ClosedAt), time.Second)) + ")"
	}
	last := "never reviewed"
	if li := r.LastReview; li != nil && li.CommitSHA != "" {
		last = "last review on " + shortSHA(li.CommitSHA)
	}
	return fmt.Sprintf("Merged %s before magnum reviewed its last push: %s, merged head %s", when, last, cmp.Or(shortSHA(r.HeadSHA), "unknown"))
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

// requestLines say when each reviewer was last asked for a review and by
// whom, newest first, mine starred ("Requested: ★ zhuravel by alice 2h ago ·
// bob by alice 3d ago"), wrapped under the label; none when the PR shows no
// request.
func (p prbPainter) requestLines(r PRBoardRow, inner int) []string {
	if len(r.Requests) == 0 {
		return nil
	}
	label := p.st.Header.Render("Requested:") + " "
	indent := spaces(ansi.StringWidth(label))
	items := make([]string, len(r.Requests))
	for i, q := range r.Requests {
		item := p.loginStyle(q.To, q.Mine).Render(q.To)
		if p.isMine(q.To, q.Mine) {
			item = p.pal.mine.Render(p.g.mine+" ") + item
		}
		if q.By != "" {
			item += p.st.Dim.Render(" by ") + q.By
		}
		if !q.At.IsZero() {
			item += p.st.Dim.Render(" " + requestAgo(max(p.now.Sub(q.At), 0)))
		}
		items[i] = item
	}
	lines := flow(items, p.st.Dim.Render(p.g.sep), max(inner-2-ansi.StringWidth(label), 10))
	for i, l := range lines {
		if i == 0 {
			lines[i] = "  " + label + l
		} else {
			lines[i] = "  " + indent + l
		}
	}
	return lines
}

// requestAgo is how long ago a request was made, in words: "2h ago".
func requestAgo(d time.Duration) string {
	if s := shortAge(d); s != "now" {
		return s + " ago"
	}
	return "just now"
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

// notReviewedSentence explains a baseline PR on its card.
const notReviewedSentence = "Open before magnum began watching this repository, so magnum has not reviewed it. " +
	"A push, a review request for you or a posting identity, or R (fresh review) starts one."

// findingsLines say what the latest review concluded: the decision (and
// what was posted instead, when the repository only lets it comment), the
// findings by priority with the simplifications, and the earlier findings.
// The nerd mode marks the decision with its verdict and each priority.
func (p prbPainter) findingsLines(f FindingsInfo) []string {
	decision := map[string]string{
		"blocking":     "request changes",
		"non_blocking": "comment (nothing blocks the merge)",
		"clean":        "approve",
	}[f.Verdict]
	if decision == "" {
		decision = f.Verdict
	}
	want := map[string]string{"blocking": "REQUEST_CHANGES", "non_blocking": "COMMENT", "clean": "APPROVE"}[f.Verdict]
	line := "Decision: " + decision
	if p.g.rich {
		glyph, st := p.findingsVerdict(f.Verdict)
		line = st.Render(glyph) + " " + line
	}
	if f.Posted != "" && want != "" && !strings.EqualFold(f.Posted, want) {
		line += "; posted as " + strings.ToLower(strings.ReplaceAll(f.Posted, "_", " ")) +
			" (A approves, C requests changes)"
	}
	out := []string{line}
	var parts []string
	for i, n := range f.Counts {
		parts = append(parts, p.priorityStyle(i).Render(p.priorityMark(i))+fmt.Sprintf("P%d %d", i, n))
	}
	counts := strings.Join(parts, " · ")
	scissors := ""
	if p.g.rich {
		scissors = p.g.simplify + " "
	}
	switch f.Simplifications {
	case 0:
	case 1:
		counts += " · " + scissors + "1 simplification suggested"
	default:
		counts += fmt.Sprintf(" · %s%d simplifications suggested", scissors, f.Simplifications)
	}
	out = append(out, counts)
	if f.Fixed+f.Open+f.Answered > 0 {
		out = append(out, fmt.Sprintf("Earlier findings: %d fixed, %d still open, %d answered", f.Fixed, f.Open, f.Answered))
	}
	return out
}

// ciLines say what the head's checks did: each workflow's counts (the
// whole run's when no workflow is known), the failed checks by name and
// the required checks' states, who requires them and, when one was
// skipped, why that matters. The nerd mode marks each state with its
// emoji.
func (p prbPainter) ciLines(ci CIInfo) []string {
	mark := func(state string) string {
		if m := p.g.ciMark[normCI(state)]; m != "" {
			return m
		}
		l := p.ciLook(state)
		if l.glyph == "" {
			return ""
		}
		return l.glyphSt.Render(l.glyph)
	}
	counts := func(state, name string, passed, failed, pending, skipped, total int) string {
		if normCI(state) == "skipped" { // every check skipped: nothing ran
			m := p.g.ciMark["missing"]
			if m == "" {
				m = p.st.Dim.Render(p.g.ciMissing)
			}
			return marked(m, marked(name, p.st.Dim.Render("not run")))
		}
		s := fmt.Sprintf("%d/%d passed", passed, total)
		if failed > 0 {
			s += p.st.Dim.Render(p.g.sep) + p.pal.red.Render(fmt.Sprintf("%d failed", failed))
		}
		if pending > 0 {
			s += p.st.Dim.Render(p.g.sep) + p.pal.yellow.Render(fmt.Sprintf("%d pending", pending))
		}
		if skipped > 0 {
			s += p.st.Dim.Render(fmt.Sprintf("%s%d skipped", p.g.sep, skipped))
		}
		return marked(mark(state), marked(name, s))
	}
	var out []string
	for _, w := range ci.Workflows {
		name := w.Name
		if name == "" {
			name = "other checks"
		}
		out = append(out, counts(w.State, p.pal.bold.Render(name), w.Passed, w.Failed, w.Pending, 0, w.Total))
	}
	switch {
	case len(ci.Workflows) > 0:
	case ci.Total > 0:
		out = append(out, counts(ci.State, "", ci.Passed, ci.Failed, ci.Pending, ci.Skipped, ci.Total))
	default:
		out = append(out, p.st.Dim.Render("no checks"))
	}
	if len(ci.Failing) > 0 {
		names := ci.Failing
		if len(ci.Workflows) > 0 {
			names = failingNames(names)
		}
		out = append(out, p.pal.red.Render("Failed:")+" "+strings.Join(names, ", "))
	}
	if len(ci.Required) == 0 {
		return out
	}
	items := make([]string, len(ci.Required))
	skipped := false
	for i, c := range ci.Required {
		l := p.ciLook(c.State)
		text := orDash(c.Name) + " " + orDash(l.word)
		if n := c.Count(); n != "" {
			text += " " + n
		}
		items[i] = marked(mark(c.State), l.textSt.Render(text))
		skipped = skipped || normCI(c.State) == "skipped"
	}
	line := "Required: " + strings.Join(items, p.st.Dim.Render(p.g.sep))
	if src := map[string]string{"github": "GitHub", "config": "config"}[normCI(ci.RequiredSource)]; src != "" {
		line += p.st.Dim.Render(" (required by " + src + ")")
	}
	out = append(out, line)
	if skipped {
		out = append(out, p.st.Dim.Render("(GitHub accepts a skipped required check; a cancelled dependency skips it)"))
	}
	return out
}

// failingNames are the failed checks' names, without their workflow
// ("CI / rspec (3)" is "rspec (3)") when one workflow holds them all: its
// line above says which.
func failingNames(names []string) []string {
	wf, _, ok := strings.Cut(names[0], " / ")
	for _, n := range names {
		if !ok || !strings.HasPrefix(n, wf+" / ") {
			return names
		}
	}
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = strings.TrimPrefix(n, wf+" / ")
	}
	return out
}
