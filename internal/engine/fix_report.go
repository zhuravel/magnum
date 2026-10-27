package engine

// The fixer's report. The fix job's agent writes it last, after the push and
// the replies (skills/magnum-fix/SKILL.md, section 7): the run's marker on
// the first line, then three sections, all always present, in this order:
//
//	<!-- magnum:run=<run id> -->
//	## Pushed
//	<sha7> <message>              one line per commit on GitHub, the merge of
//	                              the base first when the PR conflicted with
//	                              it ("<sha7> Merge <base sha7>: <specs>");
//	                              or "nothing";
//	                              or "push failed: <git's first error line>";
//	description: <what changed>   a change to the PR description, beside the
//	                              commits or alone
//	## Replied
//	N threads (a fixed, b already fixed, c not a bug, d won't fix)
//	## Needs you
//	- <comment id> <priority> <path>:<line>: fixed in <sha7>. <why>   or "none";
//	- <comment id> <priority> <path>:<line>: not fixed. <reason>      the words
//	                              after the place say what the fixer did
//	                              (fix_calls.go reads them); another
//	                              reviewer's item has no priority, and a
//	                              review body's or comment's place is
//	                              "review" or "comment"
//	- conflict: <path>, <path>: <the decision it needs>
//	                              a conflict of the base merge the fixer
//	                              aborted: no id (needsYouEntryRe)
//
// A fourth section, "## Other reviewers", follows: one line per item of the
// external file (fix_external.go) and what the fixer did with it, for the
// operator. Magnum does not read it, and a report without it (one from
// before the external file) is still whole.
//
// ParseFixReport reads it the way reportProblem reads a reviewer's report:
// strict about the marker and the headings, lenient about whitespace.

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/textx"
)

var (
	// ErrFixReportMarker is the error for a report whose first line is not the
	// marker of the run asked for: another run wrote it, or none did.
	ErrFixReportMarker = errors.New("fix report: the first line is not the run's marker")
	// ErrFixReportPartial is the error for a report with a section missing,
	// empty or out of order, as when the fixer ran out of time before it
	// finished writing.
	ErrFixReportPartial = errors.New("fix report: a section is missing")
)

// FixCommit is a commit the fixer says it pushed.
type FixCommit struct {
	SHA     string // 7 to 40 hex digits, as the fixer wrote it
	Message string
}

// FixReport is what the fixer reports.
type FixReport struct {
	// Pushed are the commits on GitHub, none for "nothing".
	Pushed []FixCommit
	// Replied is the line that counts the threads the fixer replied in, as
	// written ("N threads (a fixed, b already fixed, c not a bug, d won't
	// fix)").
	Replied string
	// NeedsYou are the findings that need the author, one entry per line
	// without its "- " (`<comment id> <priority> <path>:<line>: <reason>`),
	// none for "none".
	NeedsYou []string
	// PushFailed: the fixer says its push failed ("push failed: …").
	PushFailed bool
	// Description are the changes to the PR description ("description:
	// <what changed>" lines under "## Pushed"), what changed per line.
	Description []string
}

// The report's sections, in the order they come.
const (
	fixSectionPushed = iota
	fixSectionReplied
	fixSectionNeedsYou
	fixSections
)

var fixHeadings = [fixSections]string{"## Pushed", "## Replied", "## Needs you"}

// fixCommitLine is a "## Pushed" line for a commit: 7 to 40 hex digits and,
// as the format says, the message (a commit without one is still read).
var fixCommitLine = regexp.MustCompile(`^([0-9a-f]{7,40})(?:\s+(.*))?$`)

// maxQuotedFixLine bounds a malformed line quoted in an error: the line is
// the fixer's text, which may carry anything the PR did.
const maxQuotedFixLine = 120

// ParseFixReport reads the fixer's report of the run runID.
//
// The first line must be the run's marker (agents.ReportMarker), else the
// error is ErrFixReportMarker. The three sections must all follow, in
// order, each with something under its heading, else the error is
// ErrFixReportPartial; text before the first heading is ignored. The first
// two sections run to the next heading. "## Needs you" runs to the next
// heading or, after a blank line, to the first line that is not a "- "
// entry, so a loose list (a blank line between entries) keeps all of its
// entries and a closing remark after the sections is ignored. Any other
// line that does not fit its section is an error that quotes the line (not
// a sentinel).
func ParseFixReport(text string, runID string) (FixReport, error) {
	got, rest := agents.ReportRun([]byte(text))
	if runID == "" || got != runID {
		return FixReport{}, ErrFixReportMarker
	}
	bodies, err := fixSectionBodies(string(rest))
	if err != nil {
		return FixReport{}, err
	}

	var rep FixReport
	if err := rep.readPushed(bodies[fixSectionPushed]); err != nil {
		return FixReport{}, err
	}
	if replied := bodies[fixSectionReplied]; len(replied) != 1 {
		return FixReport{}, malformedFixLine(fixSectionReplied, replied[1])
	}
	rep.Replied = bodies[fixSectionReplied][0]
	if err := rep.readNeedsYou(bodies[fixSectionNeedsYou]); err != nil {
		return FixReport{}, err
	}
	return rep, nil
}

// fixSectionBodies splits the report after its marker into the non-blank
// lines (trimmed) under each heading, and checks that every heading came
// once, in order, with something under it.
func fixSectionBodies(rest string) ([fixSections][]string, error) {
	var bodies [fixSections][]string
	next := fixSectionPushed // the section whose heading is due
	in := -1                 // the section being read, -1 before the first heading
	gap := false             // a blank line follows the last section's list so far
	for line := range strings.SplitSeq(rest, "\n") {
		line = strings.TrimSpace(line)
		if in == fixSectionNeedsYou {
			if strings.HasPrefix(line, "## ") || (gap && line != "" && !strings.HasPrefix(line, "- ")) {
				break // the last section ends; what follows is ignored
			}
			if line == "" {
				gap = len(bodies[in]) > 0
				continue
			}
			gap = false
		}
		if i := slices.Index(fixHeadings[:], line); i >= 0 {
			if i != next {
				return bodies, fmt.Errorf("%w: found %s where %s was due", ErrFixReportPartial, fixHeadings[i], fixHeadings[next])
			}
			in, next = i, next+1
			continue
		}
		if in >= 0 && line != "" {
			bodies[in] = append(bodies[in], line)
		}
	}
	if next < fixSections {
		return bodies, fmt.Errorf("%w: no %s", ErrFixReportPartial, fixHeadings[next])
	}
	for i, b := range bodies {
		if len(b) == 0 {
			return bodies, fmt.Errorf("%w: %s is empty", ErrFixReportPartial, fixHeadings[i])
		}
	}
	return bodies, nil
}

// readPushed reads the "## Pushed" lines: commits, a failed push,
// description changes, or the one line "nothing".
func (r *FixReport) readPushed(lines []string) error {
	nothing := false
	for i, line := range lines {
		m := fixCommitLine.FindStringSubmatch(line)
		switch {
		case nothing:
			return malformedFixLine(fixSectionPushed, line)
		case i == 0 && strings.EqualFold(line, "nothing"):
			nothing = true
		case strings.HasPrefix(strings.ToLower(line), "push failed"):
			r.PushFailed = true
		case strings.HasPrefix(strings.ToLower(line), "description:") && strings.TrimSpace(line[len("description:"):]) != "":
			r.Description = append(r.Description, strings.TrimSpace(line[len("description:"):]))
		case m != nil:
			r.Pushed = append(r.Pushed, FixCommit{SHA: m[1], Message: strings.TrimSpace(m[2])})
		default:
			return malformedFixLine(fixSectionPushed, line)
		}
	}
	return nil
}

// readNeedsYou reads the "## Needs you" lines: bullets, or the one line
// "none".
func (r *FixReport) readNeedsYou(lines []string) error {
	none := false
	for i, line := range lines {
		entry, bullet := strings.CutPrefix(line, "- ")
		entry = strings.TrimSpace(entry)
		switch {
		case none:
			return malformedFixLine(fixSectionNeedsYou, line)
		case i == 0 && strings.EqualFold(line, "none"):
			none = true
		case bullet && entry != "":
			r.NeedsYou = append(r.NeedsYou, entry)
		default:
			return malformedFixLine(fixSectionNeedsYou, line)
		}
	}
	return nil
}

// malformedFixLine is the error for a line that does not fit its section.
func malformedFixLine(section int, line string) error {
	return fmt.Errorf("fix report: %s: unexpected line %q", fixHeadings[section], textx.Clip(line, maxQuotedFixLine))
}
