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
// that starts the finding's thread, the thread's link, and the sentence
// that says what to do there (FixedCall, DeclinedCall).
type FixCall struct {
	Thread string `json:"thread"`
	URL    string `json:"url"`
	Action string `json:"action"`
}

// FixedCall is the call of a finding the fixer fixed in commit sha but did
// not answer, because its thread is one a human started, where the fixer
// never writes, or, when magnums, one of magnum's whose reply failed: the
// author replies there and resolves it. "reply fixed in <sha7> in the
// thread a human started and resolve it: <url>" ("in magnum's thread").
func FixedCall(thread, url, sha string, magnums bool) FixCall {
	where := "the thread a human started"
	if magnums {
		where = "magnum's thread"
	}
	return FixCall{Thread: thread, URL: url, Action: "reply fixed in " + textx.ShortSHA(sha) + " in " + where + " and resolve it: " + url}
}

// DeclinedCall is the call of a finding the fixer did not fix and left to
// the author to decide: "decide magnum's P2 on <place>: the fixer declined
// it; reply won't fix at <url>, or `magnum fix <ref>` sends it back" ("a
// human's" in a thread a human started; "finding" for one without a
// priority).
func DeclinedCall(thread, url, priority, place, ref string, magnums bool) FixCall {
	whose := "a human's"
	if magnums {
		whose = "magnum's"
	}
	return FixCall{Thread: thread, URL: url, Action: "decide " + whose + " " + cmp.Or(priority, "finding") + " on " + place +
		": the fixer declined it; reply won't fix at " + url + ", or `magnum fix " + ref + "` sends it back"}
}

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
	// fixCallRe finds a call FixedCall or DeclinedCall wrote. Groups: the
	// link and its comment id, of a fixed call (1, 2) or a declined one (3,
	// 4).
	fixCallRe = regexp.MustCompile("reply fixed in [0-9a-f]{7} in (?:the thread a human started|magnum's thread) and resolve it: " +
		"(https://[^\\s#;,]+#discussion_r(\\d{1,20}))" +
		"|decide (?:magnum's|a human's) (?:P[0-3]|finding) on [A-Za-z0-9._/@+-]+:\\d{1,9}: the fixer declined it; reply won't fix at " +
		"(https://[^\\s#;,]+#discussion_r(\\d{1,20})), or `magnum fix [^`\\s]+` sends it back")
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
		urlAt := 2 // the fixed call's groups, else the declined one's
		if l[urlAt] < 0 {
			urlAt = 6
		}
		action := line[l[0]:l[1]]
		calls = append(calls, FixCall{Thread: line[l[urlAt+2]:l[urlAt+3]], URL: line[l[urlAt]:l[urlAt+1]], Action: action})
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
// ask, counted ("reply in 1 thread, decide 2 findings", then "1 more needs
// your call" for the findings in the older words), after the loop's own
// words (prefix) when it ended for another reason.
func fixCallsCause(prefix string, calls []FixCall, steps []string) string {
	replies := 0
	for _, c := range calls {
		if strings.HasPrefix(c.Action, "reply ") {
			replies++
		}
	}
	var parts []string
	if replies > 0 {
		parts = append(parts, "reply in "+textx.Count(replies, "thread", "threads"))
	}
	if decides := len(calls) - replies; decides > 0 {
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
