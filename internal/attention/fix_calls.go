package attention

// A fix job's call per finding. When the fixer leaves findings to the
// author and says of each whether it fixed it, the engine's message names
// what to do about each one, where: a FixCall's sentence with the link of the
// finding's thread, all joined by "; " with the fixer's report last
// (FixCallsText). Explain reads the calls back from the stored message, so
// the card, the board and `magnum status --json` show them as the engine
// wrote them. Nothing of the fixer's own words is in them: the report keeps
// its reasons.

import (
	"cmp"
	"regexp"
	"strconv"
	"strings"

	"github.com/zhuravel/magnum/internal/textx"
)

// FixCall is one finding a fix job's fixer left to the author that its
// message names with what to do (Reason.NeedsYou): the id of the comment
// that starts the finding's thread (none for a conflict with the base,
// which has no thread), the thread's link (the PR's page for a conflict),
// and the sentence that says what to do there (FixedCall, DeclinedCall,
// ResolveCall, ConflictCall).
type FixCall struct {
	Thread string `json:"thread,omitempty"`
	URL    string `json:"url"`
	Action string `json:"action"`
}

// FixedCall is the call of a finding the fixer fixed in commit sha but did
// not answer: when magnums, in one of magnum's threads whose reply failed,
// where the author replies and resolves it ("reply fixed in <sha7> in
// magnum's thread and resolve it: <url>"); else in a thread another
// reviewer started (its reply failed, or the author had answered there
// before, where the fixer does not write), where the author replies and
// that reviewer resolves it ("reply fixed in <sha7> in another reviewer's
// thread: <url>"). The words of the time when the fixer never wrote in a
// thread a human started, "in the thread a human started and resolve it",
// are still read.
func FixedCall(thread, url, sha string, magnums bool) FixCall {
	where := "another reviewer's thread"
	if magnums {
		where = "magnum's thread and resolve it"
	}
	return FixCall{Thread: thread, URL: url, Action: "reply fixed in " + textx.ShortSHA(sha) + " in " + where + ": " + url}
}

// DeclinedCall is the call of a finding the fixer did not fix and left to
// the author to decide: "decide magnum's P2 on <place>: the fixer declined
// it; reply won't fix at <url>, or `magnum fix <ref>` sends it back"
// ("another reviewer's" in a thread another reviewer started, "a human's"
// in the older words; "finding" for one without a priority).
func DeclinedCall(thread, url, priority, place, ref string, magnums bool) FixCall {
	whose := "another reviewer's"
	if magnums {
		whose = "magnum's"
	}
	return FixCall{Thread: thread, URL: url, Action: "decide " + whose + " " + cmp.Or(priority, "finding") + " on " + place +
		": the fixer declined it; reply won't fix at " + url + ", or `magnum fix " + ref + "` sends it back"}
}

// ResolveCall is the call of a finding in one of magnum's threads that shows
// the answer its call asked for (a "Fixed in" reply for a fix, "Won't fix"
// or "Not a bug" for a decline) and is still open: the author resolves it
// ("resolve magnum's thread, already answered: <url>"). Magnum never asks
// for the resolution of a thread another reviewer started.
func ResolveCall(thread, url string) FixCall {
	return FixCall{Thread: thread, URL: url, Action: "resolve magnum's thread, already answered: " + url}
}

// ConflictCall is the call of a conflict with the PR's base that the fixer
// left to the author: a product decision it would not make, so it aborted
// its merge of the base. The author merges the base on the PR's page url
// and decides the conflict in paths (plain path characters, which fixCallRe
// reads back): "merge the base by hand and decide the conflict in
// app/a.rb, db/schema.rb: <url>", the first 20 named and the rest counted
// ("… and 5 more: <url>"), as the fix prompt names them. It has no thread.
func ConflictCall(paths []string, url string) FixCall {
	named := strings.Join(paths[:min(len(paths), maxConflictPaths)], ", ")
	if more := len(paths) - maxConflictPaths; more > 0 {
		named += " and " + strconv.Itoa(more) + " more"
	}
	return FixCall{URL: url, Action: "merge the base by hand and decide the conflict in " + named + ": " + url}
}

// maxConflictPaths bounds the paths a conflict call names, as the fix
// prompt bounds its own: the call is one sentence of the message, the card
// and `magnum status`.
const maxConflictPaths = 20

// fixDetails starts the last part of a fix job's message with calls: where
// the fixer's report is.
const fixDetails = "details: "

// FixCallsText is the part of a fix job's message that names its calls,
// joined by "; ": each call's sentence, then more (the findings named in
// the older words, "1 more finding needs your call (…)"; "" for none), then
// the report ("details: <report>"). Explain reads it back.
func FixCallsText(calls []FixCall, more, report string) string {
	parts := make([]string, 0, len(calls)+2)
	for _, c := range calls {
		parts = append(parts, c.Action)
	}
	if more != "" {
		parts = append(parts, more)
	}
	return strings.Join(append(parts, fixDetails+report), "; ")
}

var (
	// fixCallRe finds a call FixedCall, DeclinedCall, ResolveCall or
	// ConflictCall wrote, in today's words or the older ones ("the thread a
	// human started", "a human's"). Groups: the link and its comment id, of a
	// fixed call (1, 2), a declined one (3, 4) or a resolution (5, 6), and
	// the PR's page of a conflict (7).
	fixCallRe = regexp.MustCompile("reply fixed in [0-9a-f]{7} in (?:(?:the thread a human started|magnum's thread) and resolve it|another reviewer's thread): " +
		"(https://[^\\s#;,]+#discussion_r(\\d{1,20}))" +
		"|decide (?:magnum's|a human's|another reviewer's) (?:P[0-3]|finding) on [A-Za-z0-9._/@+-]+:\\d{1,9}: the fixer declined it; reply won't fix at " +
		"(https://[^\\s#;,]+#discussion_r(\\d{1,20})), or `magnum fix [^`\\s]+` sends it back" +
		"|resolve magnum's thread, already answered: (https://[^\\s#;,]+#discussion_r(\\d{1,20}))" +
		"|merge the base by hand and decide the conflict in [A-Za-z0-9._/@+-]+(?:, [A-Za-z0-9._/@+-]+)*(?: and \\d{1,9} more)?: (https://[^\\s#;,]+)")
	// fixMoreRe reads how many findings the older words after the calls name.
	fixMoreRe = regexp.MustCompile(`^(\d+) more findings? needs? your call\b`)
)

// fixCalls reads the calls a fix job's message line names (FixCallsText):
// the words before them (prefix: the loop's own when it ended for another
// reason, "" for calls alone), the calls, and the remedy's steps, which are
// the calls' sentences, then the older words and the report that follow
// them. ok is false for a line that names no call.
func fixCalls(line string) (prefix string, calls []FixCall, steps []string, ok bool) {
	locs := fixCallRe.FindAllStringSubmatchIndex(line, -1)
	if len(locs) == 0 {
		return "", nil, nil, false
	}
	prefix = strings.TrimRight(line[:locs[0][0]], "; ")
	end := locs[0][0]
	for _, l := range locs {
		if strings.Trim(line[end:l[0]], "; ") != "" {
			break // the calls come together; a later match is not one of them
		}
		action := line[l[0]:l[1]]
		urlAt := 2 // the fixed call's groups, else the declined one's, the resolution's or the conflict's
		switch {
		case l[6] >= 0:
			urlAt = 6
		case l[10] >= 0:
			urlAt = 10
		case l[14] >= 0:
			urlAt = 14
		}
		call := FixCall{URL: line[l[urlAt]:l[urlAt+1]], Action: action}
		if urlAt != 14 { // a conflict has no thread
			call.Thread = line[l[urlAt+2]:l[urlAt+3]]
		}
		calls = append(calls, call)
		steps = append(steps, action)
		end = l[1]
	}
	rest := strings.TrimLeft(line[end:], "; ")
	if i := strings.Index(rest, fixDetails); i >= 0 {
		if more := strings.TrimRight(rest[:i], "; "); more != "" {
			steps = append(steps, more)
		}
		steps = append(steps, rest[i:])
	} else if rest != "" {
		steps = append(steps, rest)
	}
	return prefix, calls, steps, true
}

// fixCallsCause is the cause of a fix job's message with calls: what they
// ask, counted ("merge the base by hand, reply in 1 thread, resolve 1
// thread, decide 2 findings", then "1 more needs your call" for the
// findings in the older words), after the loop's own words (prefix) when it
// ended for another reason.
func fixCallsCause(prefix string, calls []FixCall, steps []string) string {
	replies, resolves, merges := 0, 0, 0
	for _, c := range calls {
		switch {
		case strings.HasPrefix(c.Action, "reply "):
			replies++
		case strings.HasPrefix(c.Action, "resolve "):
			resolves++
		case strings.HasPrefix(c.Action, "merge the base "):
			merges++
		}
	}
	var parts []string
	if merges > 0 {
		parts = append(parts, "merge the base by hand")
	}
	if replies > 0 {
		parts = append(parts, "reply in "+textx.Count(replies, "thread", "threads"))
	}
	if resolves > 0 {
		parts = append(parts, "resolve "+textx.Count(resolves, "thread", "threads"))
	}
	if decides := len(calls) - replies - resolves - merges; decides > 0 {
		parts = append(parts, "decide "+textx.Count(decides, "finding", "findings"))
	}
	if len(steps) > len(calls) {
		if m := fixMoreRe.FindStringSubmatch(steps[len(calls)]); m != nil {
			n, _ := strconv.Atoi(m[1])
			parts = append(parts, m[1]+" more "+textx.Plural(n, "needs", "need")+" your call")
		}
	}
	s := strings.Join(parts, ", ")
	if prefix != "" {
		s = prefix + "; " + s
	}
	return s
}
