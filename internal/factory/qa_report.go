package factory

// What visual QA found, in words (R55 slice 1, step 7): the ready card's QA
// line (qaCardLine) and its notes under KNOWN (qaKnownLines), the PR body's
// Visual QA section (qaSection), the run's report in its folder
// (summary.md, writeQAReport), which the card's e opens, and what an
// earlier run found that the writer fixed (qaFixedBefore). The PR body's
// words are the factory's own: classes, severities, counts and the head,
// with the walker's paths, states and blocked steps as code, never a title
// or a quote of the walker's, which may carry the page's text.

import (
	"cmp"
	"fmt"
	"html"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/textx"
)

const (
	// qaReportFile is a run's report in its folder, written again at each
	// of its results: what the ready card's e opens.
	qaReportFile = "summary.md"
	// qaMarker starts the Visual QA section the factory appends to
	// pr-body.md after the Known list (knownMarker): the factory's, written
	// again on each append.
	qaMarker = "<!-- magnum factory: visual QA -->"
	// qaBlockedFlag starts the ready card's FLAGS line of a state the walker
	// could not reach.
	qaBlockedFlag = "QA did not reach: "
	// qaNoBlockWhy is why a task of a repository without [factory.repo.qa]
	// needs no QA: no card line and no PR body section say it.
	qaNoBlockWhy = "the repository has no [factory.repo.qa]"
	// qaShotsNote is what the ready card's shots of a walked change are.
	qaShotsNote = "the after side, from the copy's own server; e opens the QA report"
)

// qaClassWords are the factory's own words for each class of a QA defect.
var qaClassWords = map[string]string{
	QAEscapedMarkup: "HTML markup shown as text",
	QARawI18nKey:    "a missing translation shown as text",
	QAErrorPage:     "an error page",
	QABrokenLayout:  "a broken layout",
	QAConsoleError:  "errors in the browser's console",
	QAFlow:          "a step that does not do what its acceptance check says",
}

// qaCardLine is the ready card's QA line of res, the QA run that passed the
// stage: "clean: 3 pages walked at abc1234, 1 note"; "passed with 1 defect
// accepted by you and 1 state not reached: ..." when you accepted defects
// or the walker could not reach states (they are flagged); "not needed: no
// user-facing file changed"; "" for a repository without
// [factory.repo.qa], or a run that did not pass.
func qaCardLine(res QAResult) string {
	switch {
	case res.Outcome == QAOutcomeNotNeeded && res.Why == qaNoBlockWhy:
		return ""
	case res.Outcome == QAOutcomeNotNeeded:
		return "not needed: no user-facing file changed"
	case res.Outcome != QAOutcomeClean:
		return ""
	}
	line := fmt.Sprintf("%s walked at %s, %s", plural(res.Pages, "page"), textx.ShortSHA(res.Head), plural(len(res.Known), "note"))
	var but []string
	if n := len(res.Accepted); n > 0 {
		but = append(but, plural(n, "defect")+" accepted by you")
	}
	if n := len(res.Blocked); n > 0 {
		but = append(but, plural(n, "state")+" not reached")
	}
	if len(but) == 0 {
		return "clean: " + line
	}
	return "passed with " + strings.Join(but, " and ") + ": " + line
}

// qaWhy is what the ready card's recommendation adds for res, the QA run
// that passed at the card's head: "" for none, or a run that walked no
// page.
func qaWhy(res *QAResult) string {
	if res == nil || res.Outcome != QAOutcomeClean || res.Pages == 0 {
		return ""
	}
	s := " and QA found nothing that blocks"
	if len(res.Accepted) > 0 {
		s += " but what you accepted"
	}
	if len(res.Blocked) > 0 {
		s += " on the pages it reached: read FLAGS"
	}
	return s
}

// qaKnownLines are the KNOWN lines of res on the ready card: QA's notes,
// then the defects you accepted, each "qa: P3 broken_layout: <title>".
func qaKnownLines(res QAResult) []string {
	var out []string
	for _, d := range res.Known {
		out = append(out, "qa: "+qaDefectLine(d))
	}
	for _, d := range res.Accepted {
		out = append(out, "qa: accepted by you: "+qaDefectLine(d))
	}
	return out
}

// qaDefectLine is defect d in a line: its severity, its class and its title
// (the class's words for none).
func qaDefectLine(d QADefect) string {
	return d.Severity + " " + d.Class + ": " + cmp.Or(oneLine(d.Title), qaClassWords[d.Class])
}

// qaBlockedFlags are the ready card's FLAGS lines of the states bs the
// walker could not reach, one each: "QA did not reach: the passkey
// autofill list (headless Chrome draws no autofill)".
func qaBlockedFlags(bs []QABlocked) []string {
	var out []string
	for _, b := range bs {
		s := qaBlockedFlag + cmp.Or(qaPlain(b.What), "a state")
		if why := qaPlain(b.Why); why != "" {
			s += " (" + why + ")"
		}
		out = append(out, s)
	}
	return out
}

// qaForCard is res as the ready card keeps it: the pages' quotes left out
// (they stay in the run's folder).
func qaForCard(res QAResult) *QAResult {
	unquote := func(ds []QADefect) []QADefect {
		ds = slices.Clone(ds)
		for i := range ds {
			ds[i].Quote = ""
		}
		return ds
	}
	res.Blocking, res.Known, res.Accepted = unquote(res.Blocking), unquote(res.Known), unquote(res.Accepted)
	return &res
}

// qaSection is the PR body's Visual QA section of res, the QA run that
// passed at the task's head, folded as the Known list is: the card's line
// as its summary, then in the factory's own words what QA did and found,
// the pages walked (path and state), the defects an earlier run found and
// the writer fixed, those you accepted (accepted by you), the notes, and
// the states the walker could not reach, flagged. "" without QA (a
// repository without [factory.repo.qa]).
func qaSection(res QAResult) string {
	line := qaCardLine(res)
	if line == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Visual QA\n\n<details>\n<summary>%s</summary>\n\n", html.EscapeString(line))
	if res.Outcome == QAOutcomeNotNeeded {
		b.WriteString("No user-facing file changed: no visual QA ran.\n\n</details>\n")
		return b.String()
	}
	verdict := "Nothing it found blocks."
	switch accepted, blocked := len(res.Accepted) > 0, len(res.Blocked) > 0; {
	case accepted && blocked:
		verdict = "Nothing it found on the pages it reached blocks but what you accepted; the states it could not reach are flagged below."
	case accepted:
		verdict = "Nothing it found blocks but what you accepted, below."
	case blocked:
		verdict = "Nothing it found on the pages it reached blocks; the states it could not reach are flagged below."
	}
	fmt.Fprintf(&b, "Before this PR the factory ran the change's app in its own copy at `%s`, an agent walked the changed pages in a headless "+
		"browser, and the factory checked each page it saved itself for HTML shown as text, missing translations, error pages, console "+
		"errors and sideways scrolling. %s\n\n", textx.ShortSHA(res.Head), verdict)
	pages := map[string]QAPage{}
	for _, pg := range res.Walk {
		pages[pg.ID] = pg
	}
	list := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		b.WriteString(title + ":\n\n")
		for _, it := range items {
			b.WriteString("- " + it + "\n")
		}
		b.WriteString("\n")
	}
	var walked []string
	for _, pg := range res.Walk {
		walked = append(walked, qaCode(pg.Path)+qaInState(pg.State))
	}
	list(fmt.Sprintf("%s walked", plural(res.Pages, "page")), walked)
	var fixed []string
	for _, f := range res.Fixed {
		fixed = append(fixed, fmt.Sprintf("%s %s on %s%s: %s, found by QA run %d", f.Severity, f.Class, qaCode(f.Path), qaInState(f.State),
			qaClassWords[f.Class], f.Run))
	}
	list("Found by QA and fixed before this PR", fixed)
	defects := func(ds []QADefect) []string {
		var out []string
		for _, d := range ds {
			pg, ok := pages[d.Page]
			on := "a page"
			if ok {
				on = qaCode(pg.Path) + qaInState(pg.State)
			}
			out = append(out, fmt.Sprintf("%s %s on %s: %s", d.Severity, d.Class, on, qaClassWords[d.Class]))
		}
		return out
	}
	list("Accepted by you, not fixed", defects(res.Accepted))
	list("Notes, none of them blocks", defects(res.Known))
	var blocked []string
	for _, bl := range res.Blocked {
		s := qaCode(cmp.Or(bl.What, "a state"))
		if bl.Why != "" {
			s += ": " + qaCode(bl.Why)
		}
		blocked = append(blocked, s)
	}
	list("Not reached, flagged", blocked)
	b.WriteString("</details>\n")
	return b.String()
}

// qaInState is ", in the state `<state>`" for a page's state, "" for none.
func qaInState(state string) string {
	if qaPlain(state) == "" {
		return ""
	}
	return " in the state " + qaCode(state)
}

// qaCode is the walker's s in the PR body: on one line, its control
// characters spaces, in a code span (no HTML, link or Markdown of its own
// renders; a backtick of its own is a quote).
func qaCode(s string) string {
	return "`" + strings.ReplaceAll(cmp.Or(qaPlain(s), "?"), "`", "'") + "`"
}

// qaPlain is the walker's s on one line, each control character a space, so
// no escape sequence reaches a pager or the PR body.
func qaPlain(s string) string {
	return oneLine(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
}

// writeQAReport writes path, summary.md of run's result res: the run, its
// head and the copy's server, how it ended, each page it walked with its
// state and its shot's absolute path, the defects that block, those you
// accepted, the notes (each with its title and the page's quote), what an
// earlier run found that the writer fixed, and the states it could not
// reach. The walker's words are on one line, their control characters
// spaces.
func writeQAReport(path string, run qaRun, res QAResult) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# QA run %d at %s\n\n", res.Run, textx.ShortSHA(res.Head))
	fmt.Fprintf(&b, "- head: %s\n- the copy's server: %s\n", res.Head, cmp.Or(run.URL, "none"))
	outcome := qaCardLine(res)
	if outcome == "" {
		outcome = res.Outcome
	}
	fmt.Fprintf(&b, "- outcome: %s\n", outcome)
	if res.Why != "" {
		fmt.Fprintf(&b, "- why: %s\n", qaPlain(res.Why))
	}
	runPath := filepath.Dir(path)
	pages := map[string]QAPage{}
	section := func(title string, n int) {
		fmt.Fprintf(&b, "\n## %s (%d)\n\n", title, n)
	}
	if len(res.Walk) > 0 {
		section("Pages", len(res.Walk))
	}
	for _, pg := range res.Walk {
		pages[pg.ID] = pg
		fmt.Fprintf(&b, "- %s", cmp.Or(qaPlain(pg.Path), "a page"))
		if st := qaPlain(pg.State); st != "" {
			fmt.Fprintf(&b, ", state %s", st)
		}
		b.WriteString("\n")
		if rel := filepath.FromSlash(pg.Shot); rel != "" && filepath.IsLocal(rel) {
			fmt.Fprintf(&b, "  shot: %s\n", qaPlain(filepath.Join(runPath, rel)))
		}
	}
	defects := func(title string, ds []QADefect) {
		if len(ds) == 0 {
			return
		}
		section(title, len(ds))
		for _, d := range ds {
			on := "a page"
			if pg, ok := pages[d.Page]; ok {
				on = cmp.Or(qaPlain(pg.Path), on)
			}
			fmt.Fprintf(&b, "- %s %s on %s: %s\n", d.Severity, d.Class, on, cmp.Or(qaPlain(d.Title), qaClassWords[d.Class]))
			if q := qaPlain(d.Quote); q != "" {
				fmt.Fprintf(&b, "  the page shows: %s\n", q)
			}
		}
	}
	defects("Defects that block", res.Blocking)
	defects("Accepted by you", res.Accepted)
	defects("Notes, none of them blocks", res.Known)
	if len(res.Fixed) > 0 {
		section("Found by an earlier run and fixed", len(res.Fixed))
		for _, f := range res.Fixed {
			fmt.Fprintf(&b, "- %s %s on %s: %s, found by run %d\n", f.Severity, f.Class, cmp.Or(qaPlain(f.Path), "a page"), qaClassWords[f.Class], f.Run)
		}
	}
	if len(res.Blocked) > 0 {
		section("Not reached", len(res.Blocked))
		for _, bl := range res.Blocked {
			fmt.Fprintf(&b, "- %s", cmp.Or(qaPlain(bl.What), "a state"))
			if why := qaPlain(bl.Why); why != "" {
				fmt.Fprintf(&b, ": %s", why)
			}
			if c := qaPlain(bl.Check); c != "" {
				fmt.Fprintf(&b, " (check %s)", c)
			}
			b.WriteString("\n")
		}
	}
	return fsx.WriteFileAtomic(path, []byte(b.String()), 0o600)
}

// qaFixedBefore are the defects the runs before res's handed the writer
// (their defects files, in the task directory dir) that res finds no more,
// neither blocking, known nor accepted: each once, by the first run that
// handed it, with its class, severity, page and state from that run's
// result and verdict.
func qaFixedBefore(dir string, res QAResult) []QAFixed {
	found := map[string]bool{}
	for _, ds := range [][]QADefect{res.Blocking, res.Known, res.Accepted} {
		for _, d := range ds {
			found[d.Fingerprint] = true
		}
	}
	var out []QAFixed
	for _, ref := range qaDefectsFiles(dir) {
		if ref.k >= res.Run {
			continue
		}
		run := qaRun{N: ref.k, Head: ref.head, Dir: qaRunDir(ref.k, ref.head)}
		rf, ok := readQADefects(dir, run)
		if !ok {
			continue
		}
		prev, _ := readQAResult(dir, run)
		pages := qaVerdictPages(dir, run)
		for _, it := range rf.Findings {
			i := slices.IndexFunc(prev.Blocking, func(d QADefect) bool { return d.Fingerprint == it.Fingerprint })
			if found[it.Fingerprint] || i < 0 {
				continue
			}
			found[it.Fingerprint] = true
			d := prev.Blocking[i]
			pg := pages[d.Page]
			out = append(out, QAFixed{Run: ref.k, Class: d.Class, Severity: d.Severity, Path: qaClip(pg.Path), State: qaClip(pg.State)})
		}
	}
	return out
}
