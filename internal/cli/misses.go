package cli

// `magnum misses`: what the retro found other reviewers caught on PRs magnum
// reviewed and its own review did not (the registry's misses table, written
// by the daemon's retro). Read-only: it never asks GitHub or the daemon.

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/store"
)

const missesUsage = "[<ref>] [--all] [--class miss|not_issue|style|outside|unclassified] [--json]"

const (
	// missesLessonRunes cuts the lesson when stdout is not a terminal.
	missesLessonRunes = 120
	// missesDefaultWidth is the terminal width when it cannot be read.
	missesDefaultWidth = 160
	// missesMinLesson keeps a narrow terminal from cutting the lesson to nothing.
	missesMinLesson = 30
	// missesTitleRunes and missesWhereRunes cut the other long columns.
	missesTitleRunes = 40
	missesWhereRunes = 44
)

// missesClass is a --class value with what it means.
type missesClass struct{ name, desc string }

// missesClasses are the --class values.
var missesClasses = []missesClass{
	{store.MissMiss, "a real defect a careful reviewer should have reported"},
	{store.MissNotIssue, "the comment is wrong, already handled or a question"},
	{store.MissStyle, "taste or naming, no effect on behaviour"},
	{store.MissOutside, "about code the PR did not change, or a product decision"},
	{store.MissUnclassified, "stored without a classifier"},
}

func newMissesCmd(c *Context) *cobra.Command {
	var f missesFlags
	cmd := newCommand(groupInspect, "misses "+missesUsage, "what other reviewers caught on closed PRs that magnum's review did not",
		"List what other reviewers commented on pull requests magnum reviewed, as the retro classified it (`magnum "+
			"retro` runs one now; [learn] enabled = true runs one daily). Comments magnum had already posted never "+
			"appear. By default it shows the real misses nobody drew a lesson from yet (class miss, state new); "+
			"--class picks another class and --all lists every class and every state (with --class: that class in "+
			"every state). A <ref> shows one PR's.\n\n"+
			"RAISED says what magnum did with the point: rejected:<reason> when its judge raised a similar finding and "+
			"rejected it, blank when it never raised it. WHERE is path:line, or - for a review summary. The lesson is "+
			"cut to fit the terminal.",
		func(pos []string) int { return runMisses(c, f, pos) })
	fs := cmd.Flags()
	fs.BoolVar(&f.all, "all", false, "every class and every state (not only new misses)")
	fs.StringVar(&f.class, "class", "", "only this `class`: miss, not_issue, style, outside or unclassified (default: miss)")
	fs.BoolVar(&f.json, "json", false, "print JSON")
	_ = cmd.RegisterFlagCompletionFunc("class", completeFlag(completeMissClasses))
	cmd.ValidArgsFunction = completeFirst(c.completeClosedPRs)
	return cmd
}

// missesFlags are the parsed `magnum misses` flags.
type missesFlags struct {
	all, json bool
	class     string
}

// completeMissClasses offers the --class values.
func completeMissClasses(string) []cobra.Completion {
	out := make([]cobra.Completion, 0, len(missesClasses))
	for _, k := range missesClasses {
		out = append(out, cobra.CompletionWithDesc(k.name, k.desc))
	}
	return out
}

func runMisses(c *Context, f missesFlags, pos []string) int {
	if len(pos) > 1 {
		return inspUsage(c, "misses", "at most one <ref>", missesUsage)
	}
	class := strings.ToLower(strings.TrimSpace(f.class))
	if class != "" && !slices.ContainsFunc(missesClasses, func(k missesClass) bool { return k.name == class }) {
		return inspUsage(c, "misses", fmt.Sprintf("--class %q: want miss, not_issue, style, outside or unclassified", f.class), missesUsage)
	}
	a, err := inspOpenApp(c, false)
	if err != nil {
		return cmdFail(c, "misses", err)
	}
	defer a.Close()
	ctx, cancel := signalContext()
	defer cancel()

	filter := missesFilter(class, f.all)
	if len(pos) == 1 {
		pr, err := retroLookupPR(ctx, a.Store, a.Refs(), pos[0])
		if err != nil {
			return cmdFail(c, "misses", err)
		}
		filter.PRID = pr.ID
	}
	ms, err := a.Store.Misses(ctx, filter)
	if err != nil {
		return cmdFail(c, "misses", err)
	}
	if f.json {
		if ms == nil {
			ms = []store.Miss{}
		}
		if err := writeJSON(c.Stdout, ms); err != nil {
			return cmdFail(c, "misses", err)
		}
		return 0
	}
	if len(ms) == 0 {
		fmt.Fprintln(c.Stdout, missesNone(class, f.all))
		return 0
	}
	missesRender(c.Stdout, ms, defaultRepo(a.Config), missesTermWidth(c.Stdout))
	return 0
}

// missesFilter is the registry query: class miss and state new, unless --class
// names another class or --all lifts the state (and, without --class, the
// class).
func missesFilter(class string, all bool) store.MissFilter {
	var f store.MissFilter
	switch {
	case class != "":
		f.Classes = []string{class}
	case !all:
		f.Classes = []string{store.MissMiss}
	}
	if !all {
		f.States = []string{store.MissNew}
	}
	return f
}

// missesNone explains an empty list.
func missesNone(class string, all bool) string {
	what, more := "no new misses", "; --all lists every class and state"
	switch {
	case all && class == "":
		what, more = "no misses recorded", ""
	case all:
		what, more = "no "+class+" comments recorded", ""
	case class != "" && class != store.MissMiss:
		what = "no new " + class + " comments"
	}
	return what + " yet (`magnum retro` runs a retro now; [learn] enabled = true schedules one daily" + more + ")"
}

// missesTermWidth is the width in columns of w when it is a terminal, else 0
// (tests replace it): $COLUMNS when set, then the terminal's own answer, else
// missesDefaultWidth.
var missesTermWidth = func(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok || !actTerminal(f) {
		return 0
	}
	if n, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && n > 0 {
		return n
	}
	if cols, _, err := term.GetSize(f.Fd()); err == nil && cols > 0 {
		return cols
	}
	return missesDefaultWidth
}

// missesRender prints the table. Everything from GitHub or the classifier
// goes through statusSafe. The lesson is the last column and is cut to the
// terminal's width (width > 0) or to missesLessonRunes.
func missesRender(w io.Writer, ms []store.Miss, defRepo string, width int) {
	header := []string{"PR", "REVIEWER", "WHERE", "CLASS", "SEV", "RAISED", "TITLE"}
	rows := make([][]string, 0, len(ms))
	lessons := make([]string, 0, len(ms))
	for _, m := range ms {
		rows = append(rows, []string{
			actRefLabel(defRepo, m.Repo, m.Number),
			inspOrDash(statusSafe(m.Reviewer, 0)),
			missesWhere(m),
			statusSafe(m.Class, 0),
			inspOrDash(statusSafe(m.Severity, 0)),
			missesRaised(m),
			inspOrDash(statusSafe(m.Title, missesTitleRunes)),
		})
		lessons = append(lessons, statusSafe(m.Lesson, 0))
	}
	budget := missesLessonRunes
	if width > 0 {
		// tabwriter pads every column to its widest cell plus two.
		used := 0
		for i := range header {
			cell := utf8.RuneCountInString(header[i])
			for _, r := range rows {
				cell = max(cell, utf8.RuneCountInString(r[i]))
			}
			used += cell + 2
		}
		budget = max(width-used, missesMinLesson)
	}
	tw := inspTable(w)
	fmt.Fprintln(tw, strings.Join(header, "\t")+"\tLESSON")
	for i, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t")+"\t"+inspOrDash(trunc(lessons[i], budget)))
	}
	_ = tw.Flush()
}

// missesWhere is the comment's place, path:line, "-" for a review summary;
// a long path keeps its tail.
func missesWhere(m store.Miss) string {
	if m.Path == "" {
		return "-"
	}
	s := statusSafe(m.Path, 0)
	if m.Line > 0 {
		s += ":" + strconv.Itoa(m.Line)
	}
	if r := []rune(s); len(r) > missesWhereRunes {
		s = "…" + string(r[len(r)-missesWhereRunes+1:])
	}
	return s
}

// missesRaised is what magnum did with the point: rejected:<reason>, or
// blank when it never raised it.
func missesRaised(m store.Miss) string {
	if m.Raised != store.MissRaisedRejected {
		return ""
	}
	if m.ReasonCode == "" {
		return store.MissRaisedRejected
	}
	return store.MissRaisedRejected + ":" + statusSafe(m.ReasonCode, 0)
}
