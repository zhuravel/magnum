package attention

// The fixer's report a fix job's message names, and whether the message
// asks for what the fixer left the author. The factory screen reads both
// from a PR's last_error to draw the report's "## Needs you" in place of
// magnum's count of it: only a message that asks for those calls, since a
// cap's or a failed run's report says other things.

import (
	"regexp"

	"github.com/zhuravel/magnum/internal/textx"
)

// fixYourCallRe finds the engine's words for what the fixer left the author
// (its needYourCall and needsYouCalls): "1 finding needs your call", "2
// findings need your call", "1 conflict needs your call", "1 finding and 1
// conflict need your call", "2 more findings need your call".
var fixYourCallRe = regexp.MustCompile(`\b\d+ (?:more )?(?:findings?|conflicts?)(?: and \d+ conflicts?)? needs? your call\b`)

// FixReport is the fixer's report that a fix job's message msg (a PR's
// last_error under KindFix) names: the last absolute path to a .md file of
// its first line, as the remedy names the report (fixRemedy); "" when that
// line names none.
func FixReport(msg string) string {
	return lastReport(textx.FirstLine(msg))
}

// lastReport is the last absolute path to a .md file in s, "" for none.
func lastReport(s string) string {
	m := fixReportRe.FindAllString(s, -1)
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1]
}

// FixAsks reports whether msg, a fix job's message, asks the author for the
// calls of what its fixer left them, whatever the loop's own words before
// them (a re-review that did not post): the calls (FixCallsText) or the
// findings and conflicts named or counted ("2 findings need your call
// (…)", the settle's "1 finding needs your call: <report>"). A loop that
// ended otherwise (its cap, a failed run, a guard) asks for something else.
func FixAsks(msg string) bool {
	line := textx.FirstLine(msg)
	if _, _, _, ok := fixCalls(line); ok {
		return true
	}
	return fixYourCallRe.MatchString(line)
}
