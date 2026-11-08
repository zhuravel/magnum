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
	// folder: those of the defects earlier runs handed the writer, fixed or
	// declined, since the newest run that passed each of them (qaRechecks),
	// which the walker's prompt names.
	qaRecheckFile = "recheck.json"
	// qaSectionMost is how many items each list of the PR body's Visual QA
	// section names; a line counts the rest, which the run's report has.
	qaSectionMost = 20
	// qaNotWalkedWhy is why a page to walk again is a state the run did not
	// reach.
	qaNotWalkedWhy = "not walked again after the writer's fix"
	// qaDeclinedNotWalkedWhy is why the page of a defect the writer declined,
	// which no accept of yours at the run's head covers, is a state the run
	// did not reach.
	qaDeclinedNotWalkedWhy = "not walked again after the writer declined it"
	// qaDeclinedNoRiskNotWalkedWhy is the same for a decline that named no
	// risk: no fix was made either.
	qaDeclinedNoRiskNotWalkedWhy = "not walked again after the writer declined it with no risk"
	// qaDeclinedNotFoundWhy is why a run that walked the page of a defect the
	// writer declined, at the head it declined it at, and did not find it, is
	// no pass for it: the factory's own check found it on that page at that
	// code, so another state or other data stood behind the walker's label.
	qaDeclinedNotFoundWhy = "not found again at the head the writer declined it at"
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
// page and state (the newest run's page), its fingerprint, whether a run
// handed it since the newest run that passed it (sincePass: the run checks
// it), whether the writer's newest reply declined it, with its risk or with
// none (declined: no fix of it was made; noRisk: with none), and the head of
// the newest run that handed it (handedAt, as its run folder names it), the
// head the writer replied at.
type qaHandedDefect struct {
	QAFixed
	fingerprint string
	sincePass   bool
	declined    bool
	noRisk      bool
	handedAt    string
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
// flagged; "1 state not reached, accepted by you" when you accepted them as
// ones QA could not check); "not needed: no user-facing file changed"; ""
// for a repository without [factory.repo.qa], or a run that did not pass.
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
	flagged, accepted := qaBlockedSplit(res.Blocked)
	if n := len(flagged); n > 0 {
		but = append(but, plural(n, "state")+" not reached")
	}
	if n := len(accepted); n > 0 {
		but = append(but, plural(n, "state")+" not reached, accepted by you")
	}
	if len(but) == 0 {
		return "clean: " + line
	}
	return "passed with " + strings.Join(but, " and ") + ": " + line
}

// qaBlockedSplit is the states bs the walker could not reach in two: those
// flagged alone, and those you accepted as ones QA could not check
// (QABlocked.Accepted).
func qaBlockedSplit(bs []QABlocked) (flagged, accepted []QABlocked) {
	for _, b := range bs {
		if b.Accepted {
			accepted = append(accepted, b)
		} else {
			flagged = append(flagged, b)
		}
	}
	return flagged, accepted
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
// autofill list (headless Chrome draws no autofill)", ", accepted by you"
// after one you accepted as a state QA could not check.
func qaBlockedFlags(bs []QABlocked) []string {
	var out []string
	for _, b := range bs {
		s := qaBlockedFlag + cmp.Or(qaPlain(b.What), "a state")
		if why := qaPlain(b.Why); why != "" {
			s += " (" + why + ")"
		}
		if b.Accepted {
			s += ", accepted by you"
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
// the states the walker could not reach, flagged, those you accepted as
// ones QA could not check apart (accepted by you). "" without QA (a
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
	flaggedStates, acceptedStates := qaBlockedSplit(res.Blocked)
	states := "the states it could not reach are flagged below"
	if len(flaggedStates) == 0 {
		states = "the states it could not reach, which you accepted, are flagged below"
	}
	verdict := "Nothing it found blocks."
	switch accepted, blocked := len(res.Accepted) > 0 || len(res.Unchecked) > 0, len(res.Blocked) > 0; {
	case accepted && blocked:
		verdict = "Nothing it found on the pages it reached blocks but what you accepted; " + states + "."
	case accepted:
		verdict = "Nothing it found blocks but what you accepted, below."
	case blocked:
		verdict = "Nothing it found on the pages it reached blocks; " + states + "."
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
	list("The writer's fixes QA did not walk again, accepted by you", fixes(res.Unchecked))
	defects := func(ds []QADefect) []string {
		var out []string
		for _, d := range ds {
			on := "a page"
			if pg, ok := pages[d.Page]; ok {
				on = qaCode(pg.Path) + qaInState(pg.State)
			} else if d.FoundPath != "" { // accepted by you, not seen by this run
				on = qaCode(d.FoundPath) + qaInState(d.FoundState)
			}
			out = append(out, fmt.Sprintf("%s %s on %s: %s", qaKnownSeverity(d.Severity), qaKnownClass(d.Class), on,
				cmp.Or(qaClassWords[d.Class], "a defect")))
		}
		return out
	}
	list("Accepted by you, not fixed", defects(res.Accepted))
	list("Notes, none of them blocks", defects(res.Known))
	blocked := func(bs []QABlocked) []string {
		var out []string
		for _, bl := range bs {
			s := qaCode(cmp.Or(bl.What, "a state"))
			if bl.Why != "" {
				s += ": " + qaCode(bl.Why)
			}
			out = append(out, s)
		}
		return out
	}
	list("Not reached, flagged", blocked(flaggedStates))
	list("Not reached, accepted by you", blocked(acceptedStates))
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
// not walk again, the states it could not reach, and apart from them the
// defects the writer declined that it did not find again at the head they
// were declined at. The walker's words are on one line, their control
// characters spaces.
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
			} else if p := qaPlain(d.FoundPath); p != "" {
				on = p + " (not seen in this run)"
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
	fixes("The writer's fixes QA did not walk again, accepted by you", res.Unchecked)
	notFound := func(bl QABlocked) bool { return bl.Why == qaDeclinedNotFoundWhy }
	blocked := func(title string, bs []QABlocked) {
		if len(bs) == 0 {
			return
		}
		section(title, len(bs))
		for _, bl := range bs {
			fmt.Fprintf(&b, "- %s", cmp.Or(qaPlain(bl.What), "a state"))
			if why := qaPlain(bl.Why); why != "" && !notFound(bl) {
				fmt.Fprintf(&b, ": %s", why)
			}
			if c := qaPlain(bl.Check); c != "" {
				fmt.Fprintf(&b, " (check %s)", c)
			}
			if bl.Accepted {
				b.WriteString(", accepted by you")
			}
			b.WriteString("\n")
		}
	}
	blocked("Not reached", slices.DeleteFunc(slices.Clone(res.Blocked), notFound))
	blocked("Declined by the writer, not found again at this head", slices.DeleteFunc(slices.Clone(res.Blocked),
		func(bl QABlocked) bool { return !notFound(bl) }))
	return fsx.WriteFileAtomic(path, []byte(b.String()), 0o600)
}

// qaHandedBefore are the defects the runs before run k handed the writer
// (their defects files, in the task directory dir), each once, by its
// fingerprint: the first run that found it, its class and severity from
// that run's result, the path its probe read and the state of its page as
// the newest run that handed it saw them (that run's walk), whether a run
// handed it after the newest run before k that passed it, and whether the
// writer's newest reply declined it (qaHistory): run k checks those handed
// since. A run passes a defect when its result is clean or not_needed and it
// did not let the defect through on your accept alone (its Accepted, its
// Unchecked): an accept holds at its head, so a run at a new head checks the
// defect again.
func qaHandedBefore(dir string, k int) []qaHandedDefect {
	type qaPass struct {
		k       int
		through map[string]bool
	}
	var passes []qaPass
	for _, ref := range qaRunFolders(dir) {
		res, ok := readQAResult(dir, qaRun{Dir: qaRunDir(ref.k, ref.head)})
		if !ok || ref.k >= k || (res.Outcome != QAOutcomeClean && res.Outcome != QAOutcomeNotNeeded) {
			continue
		}
		p := qaPass{k: ref.k, through: map[string]bool{}}
		for _, d := range res.Accepted {
			p.through[d.Fingerprint] = true
		}
		for _, f := range res.Unchecked {
			p.through[f.Fingerprint] = true
		}
		passes = append(passes, p)
	}
	passOf := func(fp string) int {
		n := 0
		for _, p := range passes {
			if !p.through[fp] {
				n = max(n, p.k)
			}
		}
		return n
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
			out[i].handedAt = ref.head
			out[i].sincePass = out[i].sincePass || ref.k > passOf(d.Fingerprint)
		}
	}
	hist := qaHistory(dir, k)
	for i := range out {
		h := hist[out[i].fingerprint]
		out[i].declined, out[i].noRisk = h.declined || h.declinedNoRisk, h.declinedNoRisk
	}
	return out
}

// qaRechecks are the pages run k walks again (recheck.json): those of the
// defects handed the writer since the newest run that passed each of them
// (qaHandedBefore), fixed or declined, each page in its state once.
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
// labels. One the run finds again is the run's own. A decline of the
// writer's that you accepted at the run's head, which the run does not find,
// stays accepted by you (res.Accepted, its page as an earlier run found it).
// A fix of the writer's whose page the run walked in its state, finding it
// no more, is fixed (res.Fixed); a declined defect never is, as no fix was
// made. One handed since the newest run that passed it, whose page the run
// did not walk again, is a state it did not reach (res.Blocked, with its
// fingerprint): the run is no pass for it, and you look; unless you let its
// fix go unchecked at the run's head (qa-accepted.json, --accept-qa): then
// it is res.Unchecked. One a pass checked before, which the run did not
// walk, is neither.
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
	accepted := func(fps []string, fp string) bool { return acc.Head == run.Head && slices.Contains(fps, fp) }
	for _, h := range qaHandedBefore(dir, run.N) {
		switch {
		case found[h.fingerprint]:
		case h.declined && accepted(acc.Fingerprints, h.fingerprint):
			res.Accepted = append(res.Accepted, QADefect{Class: h.Class, Severity: h.Severity, Source: qaSourceFactory,
				Fingerprint: h.fingerprint, FoundPath: h.Path, FoundState: h.State})
		case walked[qaPageKey(h.Path, h.State)] && !h.declined:
			res.Fixed = append(res.Fixed, h.QAFixed)
		case !h.sincePass:
		case walked[qaPageKey(h.Path, h.State)] && h.handedAt == textx.ShortSHA(run.Head):
			res.Blocked = append(res.Blocked, QABlocked{What: qaRecheckWhat(h), Why: qaDeclinedNotFoundWhy, Fingerprint: h.fingerprint})
		case walked[qaPageKey(h.Path, h.State)]: // a decline the head moved past, walked and not found: no fix to name
		case h.declined:
			why := qaDeclinedNotWalkedWhy
			if h.noRisk {
				why = qaDeclinedNoRiskNotWalkedWhy
			}
			res.Blocked = append(res.Blocked, QABlocked{What: qaRecheckWhat(h), Why: why, Fingerprint: h.fingerprint})
		case accepted(acc.Unchecked, h.fingerprint):
			f := h.QAFixed
			f.Fingerprint = h.fingerprint
			res.Unchecked = append(res.Unchecked, f)
		default:
			res.Blocked = append(res.Blocked, QABlocked{What: qaRecheckWhat(h), Why: qaNotWalkedWhy, Fingerprint: h.fingerprint})
		}
	}
}
