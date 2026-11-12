package attention

// The fixer's report a fix job's message names, and whether the message
// asks for what the fixer left the author. The factory screen reads both
// from a PR's last_error to draw the report's "## Needs you" in place of
// magnum's count of it: only a message that asks for those calls, since a
// cap's or a failed run's report says other things.

import (
	"regexp"
	"strings"

	"github.com/zhuravel/magnum/internal/textx"
)

// fixYourCallRe finds the engine's words for what the fixer left the author
// (its needYourCall and needsYouCalls): "1 finding needs your call", "2
// findings need your call", "1 conflict needs your call", "1 finding and 1
// conflict need your call", "2 more findings need your call".
var fixYourCallRe = regexp.MustCompile(`\b\d+ (?:more )?(?:findings?|conflicts?)(?: and \d+ conflicts?)? needs? your call\b`)

// lookRaiseRe matches the look's raise (LookRaiseText) after its kind.
var lookRaiseRe = regexp.MustCompile(`^\d+ items? the fixer left needs? you: `)

// LookLift is what lifts the look's raise (LookRaise): the look reads the
// PR again and puts it back to reviewed once each raised item is answered
// on GitHub, or its reviewer approved the PR since (engine openLift). It is
// the raise's remedy and `magnum fix --done`'s refusal of it.
const LookLift = "answer each linked item on GitHub (or its reviewer approves the PR), and magnum lifts this at its next read of the PR"

// LookRaiseText is the message of the look's raise (engine openRaise, with
// autofix = "open"): n items that fix loops left and a later read of the
// look found still open, at links: "fix: 2 items the fixer left need you:
// <url>, <url>". No fixer's report or pane goes with it, and `magnum fix
// --done` does not take it: the look lifts it itself (LookLift).
func LookRaiseText(n int, links []string) string {
	return KindFix + ": " + textx.Count(n, "item the fixer left needs you", "items the fixer left need you") + ": " + strings.Join(links, ", ")
}

// LookRaise reports whether msg, a PR's last_error (with its "fix: " or
// without, as Explain reads it), is the look's raise (LookRaiseText).
func LookRaise(msg string) bool {
	return lookRaiseRe.MatchString(strings.TrimPrefix(textx.FirstLine(msg), KindFix+": "))
}

// mergeReadyWords start a held merge's attention after its kind
// (MergeReadyText).
const mergeReadyWords = "merge ready: "

// MergeReadyText is the attention of a merge of the parent that magnum
// holds for your y (the engine's HeldMerge), what its words, the merge and
// who pushes it: "fix: merge ready: claude-fix merged master at c0c0c0c
// and resolved 5 conflicts. …". Its next step is the held push's question
// (MergeReadyNext), never `magnum fix --done`.
func MergeReadyText(what string) string { return KindFix + ": " + mergeReadyWords + what }

// MergeReady reports whether msg, a PR's last_error (with its "fix: " or
// without, as Explain reads it), is a held merge's attention
// (MergeReadyText).
func MergeReady(msg string) bool {
	return strings.HasPrefix(strings.TrimPrefix(textx.FirstLine(msg), KindFix+": "), mergeReadyWords)
}

// MergeReadyNext is the next step of a held merge's attention on ref: the
// held push's y/N.
func MergeReadyNext(ref string) string {
	return "`magnum merge-parent " + ref + "` on a terminal (P on the screens) asks y/N and pushes it after your y; " +
		"e on the screens opens held-merge.diff, what it does to the workflow files"
}

// mergeDroppedWords start, after its kind, the attention of a held merge
// magnum dropped because it went stale (MergeDroppedText).
const mergeDroppedWords = "merge dropped: "

// MergeDroppedText is the attention of a held merge magnum dropped, what
// its words (whose merge, and why): "fix: merge dropped: magnum dropped
// claude-fix's held merge d3d3d3d of master at c0c0c0c: master moved on
// GitHub from c0c0c0c to d1d1d1d; P merges the parent again". Its next step
// is a new merge of the parent (MergeDroppedNext).
func MergeDroppedText(what string) string {
	return KindFix + ": " + mergeDroppedWords + what + "; P merges the parent again"
}

// MergeDropped reports whether msg, a PR's last_error (with its "fix: " or
// without), is a dropped held merge's attention (MergeDroppedText).
func MergeDropped(msg string) bool {
	return strings.HasPrefix(strings.TrimPrefix(textx.FirstLine(msg), KindFix+": "), mergeDroppedWords)
}

// MergeDroppedNext is the next step of a dropped held merge's attention on
// ref: a new merge of the parent.
func MergeDroppedNext(ref string) string {
	return "`magnum merge-parent " + ref + "` on a terminal (P on the screens) merges the parent again"
}

// heldNext is the next step of a held merge's attention, or of a dropped
// one's, whose chain (without its "fix: ") msg is; "" for any other.
func heldNext(chain, ref string) string {
	switch {
	case MergeReady(chain):
		return MergeReadyNext(ref)
	case MergeDropped(chain):
		return MergeDroppedNext(ref)
	}
	return ""
}

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

// fixDoneFits reports whether `magnum fix --done` (engine fixDone) can lift
// a fix job's message whose first line is line, with rest under it: line
// asks for the fixer's calls alone (FixAsks, with none of the loop's own
// words before them: a re-review that did not post leaves the fixer's
// pushed head unreviewed, which --done refuses), and nothing is under it
// (the line that the fixer did not get the other reviewers' items, which
// --done refuses too, since the fixer may have left more). line may start
// with the kind ("fix: ") or not, as Explain reads it.
func fixDoneFits(line, rest string) bool {
	if strings.TrimSpace(rest) != "" || !FixAsks(line) {
		return false
	}
	line = strings.TrimPrefix(line, KindFix+": ")
	if prefix, _, _, ok := fixCalls(line); ok {
		return prefix == ""
	}
	loc := fixYourCallRe.FindStringIndex(line)
	return loc != nil && loc[0] == 0
}
