package factory

// What visual QA found, in words (R55 slice 1, step 7): the ready card's QA
// line (qaCardLine) and its notes under KNOWN (qaKnownLines), the PR body's
// Visual QA section (qaSection), the run's report in its folder
// (summary.md, writeQAReport), which the card's e opens, and the defects
// earlier runs handed the writer (qaHandedBefore): a run walks their pages
// again (recheck.json), and only a walk of a page in its state that finds
// the defect no more makes it fixed. The PR body's words are the factory's
// own: classes, severities, counts and the head, with the walker's paths,
// states and blocked steps as code, never a title or a quote of the
// walker's, which may carry the page's text.

import (
	"cmp"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	// qaShotsSide is what the ready card's shots of a walked change are, and
	// qaShotsNote the same when the run's report is there for e to open.
	qaShotsSide = "the after side, from the copy's own server"
	qaShotsNote = qaShotsSide + "; e opens the QA report"
	// qaRecheckFile is a run's list of the pages it walks again, in its
	// folder: those of the defects earlier runs handed the writer since the
	// newest run that passed (qaRecheck), which the walker's prompt names.
	qaRecheckFile = "recheck.json"
	// qaSectionMost is how many items each list of the PR body's Visual QA
	// section names; a line counts the rest, which the run's report has.
	qaSectionMost = 20
	// qaNotWalkedWhy is why a page to walk again is a state the run did not
	// reach.
	qaNotWalkedWhy = "not walked again after the writer's fix"
)

// qaRecheck is a page a run walks again, in recheck.json: the path and the
// state of the page an earlier run found a defect of class on.
type qaRecheck struct {
	Path  string `json:"path"`
	State string `json:"state,omitempty"`
	Class string `json:"class"`
}

// qaHandedDefect is a defect an earlier run handed the writer, as
// qaHandedBefore has it: the first run that found it, its class, severity,
// page and state (the newest run's page), its fingerprint, and whether a run
// handed it since the newest run that passed (sincePass: the run checks its
// fix).
type qaHandedDefect struct {
	QAFixed
	fingerprint string
	sincePass   bool
}

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
// accepted by you and 1 state not reached: ..." when you accepted defects,
// let a fix go that QA did not walk again ("1 fix not walked again,
// accepted by you"), or the walker could not reach states (they are
// flagged); "not needed: no
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
	if n := len(res.Unchecked); n > 0 {
		but = append(but, qaFixes(n)+" not walked again, accepted by you")
	}
	if n := len(res.Blocked); n > 0 {
		but = append(but, plural(n, "state")+" not reached")
	}
	if len(but) == 0 {
		return "clean: " + line
	}
	return "passed with " + strings.Join(but, " and ") + ": " + line
}

// CardLine is the ready card's QA line of r, the QA run that passed the
// stage (qaCardLine), as `magnum factory decide` prints it: "" for none.
func (r QAResult) CardLine() string { return qaCardLine(r) }

// qaWhy is what the ready card's recommendation adds for res, the QA run
// that passed at the card's head: "" for none, or a run that walked no
// page.
func qaWhy(res *QAResult) string {
	if res == nil || res.Outcome != QAOutcomeClean || res.Pages == 0 {
		return ""
	}
	s := " and QA found nothing that blocks"
	if len(res.Accepted) > 0 || len(res.Unchecked) > 0 {
		s += " but what you accepted"
	}
	if len(res.Blocked) > 0 {
		s += " on the pages it reached: read FLAGS"
	}
	return s
}

// qaKnownLines are the KNOWN lines of res on the ready card: QA's notes,
// then the defects you accepted, each "qa: P3 broken_layout: <title>", then
// the fixes you let go that QA did not walk again.
func qaKnownLines(res QAResult) []string {
	var out []string
	for _, d := range res.Known {
		out = append(out, "qa: "+qaDefectLine(d))
	}
	for _, d := range res.Accepted {
		out = append(out, "qa: accepted by you: "+qaDefectLine(d))
	}
	for _, f := range res.Unchecked {
		out = append(out, fmt.Sprintf("qa: accepted by you: %s %s on %s: the writer's fix, which QA did not walk again", f.Severity, f.Class,
			cmp.Or(qaPlain(f.Path), "a page")))
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
// the writer fixed, those whose fix QA did not walk again and you let go,
// those you accepted (accepted by you), the notes, and
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
	switch accepted, blocked := len(res.Accepted) > 0 || len(res.Unchecked) > 0, len(res.Blocked) > 0; {
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
		for _, it := range items[:min(len(items), qaSectionMost)] {
			b.WriteString("- " + it + "\n")
		}
		if n := len(items) - qaSectionMost; n > 0 {
			fmt.Fprintf(&b, "- and %d more in the run's report\n", n)
		}
		b.WriteString("\n")
	}
	var walked []string
	for _, pg := range res.Walk {
		walked = append(walked, qaCode(pg.Path)+qaInState(pg.State))
	}
	list(fmt.Sprintf("%s walked", plural(res.Pages, "page")), walked)
	fixes := func(fs []QAFixed) []string {
		var out []string
		for _, f := range fs {
			out = append(out, fmt.Sprintf("%s %s on %s%s: %s, found by QA run %d", qaKnownSeverity(f.Severity), qaKnownClass(f.Class),
				qaCode(f.Path), qaInState(f.State), cmp.Or(qaClassWords[f.Class], "a defect"), f.Run))
		}
		return out
	}
	list("Found by QA and fixed before this PR", fixes(res.Fixed))
	list("Fixed by the writer, not walked again by QA, accepted by you", fixes(res.Unchecked))
	defects := func(ds []QADefect) []string {
		var out []string
		for _, d := range ds {
			pg, ok := pages[d.Page]
			on := "a page"
			if ok {
				on = qaCode(pg.Path) + qaInState(pg.State)
			}
			out = append(out, fmt.Sprintf("%s %s on %s: %s", qaKnownSeverity(d.Severity), qaKnownClass(d.Class), on,
				cmp.Or(qaClassWords[d.Class], "a defect")))
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

// qaKnownClass is class c as the PR body prints it: one of the factory's
// classes, else "?" (the run's files are the factory's, checked on the way
// in, but an agent could edit its folder); qaKnownSeverity is the same for a
// severity, P1 to P3.
func qaKnownClass(c string) string {
	if _, ok := qaClassWords[c]; ok {
		return c
	}
	return "?"
}

func qaKnownSeverity(s string) string {
	if s == "P1" || s == "P2" || s == "P3" {
		return s
	}
	return "?"
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

// qaPlain is the walker's s on one line, each control, format (a bidi
// override or isolate, a zero-width character) and line or paragraph
// separator character a space, so no escape sequence reaches a pager and no
// line of the PR body is reordered.
func qaPlain(s string) string {
	return oneLine(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return ' '
		}
		return r
	}, s))
}

// writeQAReport writes path, summary.md of run's result res: the run, its
// head and the copy's server, how it ended, each page it walked with its
// state and its shot's absolute path, the defects that block, those you
// accepted, the notes (each with its title and the page's quote), what an
// earlier run found that the writer fixed, the fixes you let go that it did
// not walk again, and the states it could not reach. The walker's words are on one line, their control characters
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
	fixes := func(title string, fs []QAFixed) {
		if len(fs) == 0 {
			return
		}
		section(title, len(fs))
		for _, f := range fs {
			fmt.Fprintf(&b, "- %s %s on %s: %s, found by run %d\n", f.Severity, f.Class, cmp.Or(qaPlain(f.Path), "a page"), qaClassWords[f.Class], f.Run)
		}
	}
	fixes("Found by an earlier run and fixed", res.Fixed)
	fixes("Fixed by the writer, not walked again, accepted by you", res.Unchecked)
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

// qaHandedBefore are the defects the runs before run k handed the writer
// (their defects files, in the task directory dir), each once, by its
// fingerprint: the first run that found it, its class and severity from
// that run's result, the path its probe read and the state of its page as
// the newest run that handed it saw them (that run's walk), and whether a run handed it after the newest run
// before k that passed (a clean or not_needed result): run k checks the fix
// of those.
func qaHandedBefore(dir string, k int) []qaHandedDefect {
	pass := 0
	for _, ref := range qaRunFolders(dir) {
		if res, ok := readQAResult(dir, qaRun{Dir: qaRunDir(ref.k, ref.head)}); ok && ref.k < k &&
			(res.Outcome == QAOutcomeClean || res.Outcome == QAOutcomeNotNeeded) {
			pass = max(pass, ref.k)
		}
	}
	var out []qaHandedDefect
	at := map[string]int{}
	for _, ref := range qaDefectsFiles(dir) {
		if ref.k >= k {
			continue
		}
		run := qaRun{N: ref.k, Head: ref.head, Dir: qaRunDir(ref.k, ref.head)}
		rf, ok := readQADefects(dir, run)
		if !ok {
			continue
		}
		prev, _ := readQAResult(dir, run)
		pages := map[string]QAPage{}
		for _, pg := range prev.Walk {
			pages[pg.ID] = pg
		}
		for _, it := range rf.Findings {
			j := slices.IndexFunc(prev.Blocking, func(d QADefect) bool { return d.Fingerprint == it.Fingerprint })
			if j < 0 {
				continue
			}
			d := prev.Blocking[j]
			pg := pages[d.Page]
			i, seen := at[d.Fingerprint]
			if !seen {
				i, at[d.Fingerprint] = len(out), len(out)
				out = append(out, qaHandedDefect{QAFixed: QAFixed{Run: ref.k, Class: d.Class, Severity: d.Severity}, fingerprint: d.Fingerprint})
			}
			out[i].Path, out[i].State = cmp.Or(pg.ProbePath, pg.Path), pg.State // the path its probe read (an older build kept the walker's)
			out[i].sincePass = out[i].sincePass || ref.k > pass
		}
	}
	return out
}

// qaRechecks are the pages run k walks again (recheck.json): those of the
// defects handed the writer since the newest run that passed
// (qaHandedBefore), each page in its state once.
func qaRechecks(dir string, k int) []qaRecheck {
	var out []qaRecheck
	seen := map[string]bool{}
	for _, h := range qaHandedBefore(dir, k) {
		if key := qaPageKey(h.Path, h.State); h.sincePass && !seen[key] {
			seen[key] = true
			out = append(out, qaRecheck{Path: h.Path, State: h.State, Class: h.Class})
		}
	}
	return out
}

// qaPageKey is a page in a state as the factory matches it across runs:
// its path as a probe's is read (qaProbePath: no trailing slash, no query,
// a leading one), and its state's words in lower case, an underscore or a
// hyphen a space ("signed_in" is "Signed in").
func qaPageKey(path, state string) string {
	words := strings.Fields(strings.NewReplacer("_", " ", "-", " ").Replace(strings.ToLower(qaPlain(state))))
	return cmp.Or(qaProbePath(qaPlain(path)), "/") + "\x00" + strings.Join(words, " ")
}

// qaRecheckWhat names handed defect h as a state its run did not reach:
// "escaped_markup on /settings/security in the state signed_in, which QA
// run 1 found".
func qaRecheckWhat(h qaHandedDefect) string {
	return fmt.Sprintf("%s, which QA run %d found", qaRecheckOn(h), h.Run)
}

// qaRecheckOn is handed defect h on its page: "escaped_markup on
// /settings/security in the state signed_in".
func qaRecheckOn(h qaHandedDefect) string {
	s := h.Class + " on " + cmp.Or(qaPlain(h.Path), "a page")
	if st := qaPlain(h.State); st != "" {
		s += " in the state " + st
	}
	return s
}

// qaRunFolders are the run folders under dir/qa: their numbers and their
// heads (sha7), ascending.
func qaRunFolders(dir string) []qaDefectsRef {
	entries, _ := os.ReadDir(filepath.Join(dir, "qa"))
	var out []qaDefectsRef
	for _, en := range entries {
		if m := qaRunDirRe.FindStringSubmatch(en.Name()); m != nil && en.IsDir() {
			if k, err := strconv.Atoi(m[1]); err == nil {
				out = append(out, qaDefectsRef{k: k, head: en.Name()[len(m[1])+1:]})
			}
		}
	}
	slices.SortFunc(out, func(a, b qaDefectsRef) int { return a.k - b.k })
	return out
}

// qaCheckFixes checks run's result res (in the task directory dir) against
// the defects earlier runs handed the writer (qaHandedBefore), by the pages
// of its walk (res.Walk) as their probes read them, never by the walker's
// labels. One the run finds again is the run's own. One whose page the run
// walked in its state, and finds no more, is fixed (res.Fixed). One handed
// since the newest pass whose page the run did not walk again is a state
// it did not reach (res.Blocked, with its fingerprint): the run is no pass
// for it, and you look; unless you let its fix go unchecked at the run's
// head (qa-accepted.json, --accept-qa): then it is res.Unchecked. One a pass
// checked before, which the run did not walk, is neither.
func qaCheckFixes(dir string, run qaRun, res *QAResult) {
	found := map[string]bool{}
	for _, ds := range [][]QADefect{res.Blocking, res.Known, res.Accepted} {
		for _, d := range ds {
			found[d.Fingerprint] = true
		}
	}
	walked := map[string]bool{}
	for _, pg := range res.Walk {
		if pg.ProbePath != "" {
			walked[qaPageKey(pg.ProbePath, pg.State)] = true
		}
	}
	acc, _ := readQAAccepted(dir)
	for _, h := range qaHandedBefore(dir, run.N) {
		switch {
		case found[h.fingerprint]:
		case walked[qaPageKey(h.Path, h.State)]:
			res.Fixed = append(res.Fixed, h.QAFixed)
		case !h.sincePass:
		case acc.Head == run.Head && slices.Contains(acc.Unchecked, h.fingerprint):
			res.Unchecked = append(res.Unchecked, h.QAFixed)
		default:
			res.Blocked = append(res.Blocked, QABlocked{What: qaRecheckWhat(h), Why: qaNotWalkedWhy, Fingerprint: h.fingerprint})
		}
	}
}
