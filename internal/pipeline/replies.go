package pipeline

// The reply contract (skills/magnum-review/SKILL.md section 6): a re-review
// reads the inline threads its reviewer login started on the PR, classifies
// every reply by its first clause (classifyReply) and hands them to the judge
// as a file next to the candidate reports. Replies are PR content, so the
// prompt names the file and the counts, never the text.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/postreview"
	"github.com/zhuravel/magnum/internal/textx"
)

// Reply classes (agents.ThreadReply.Class).
const (
	ReplyFixed   = "fixed"
	ReplyNotABug = "not a bug"
	ReplyWontFix = "won't fix"
	ReplyOther   = "other"
)

const (
	// replyExcerptMax bounds a reply's body in the threads file, in characters.
	replyExcerptMax = 600
	// findingLineMax bounds a thread's finding line, in characters.
	findingLineMax = 200
	// ThreadsFile is the threads file in the round's report directory.
	ThreadsFile = "review-threads.json"
)

// ThreadLister is implemented by a GitHub client that lists a pull request's
// review threads with their comments (github.Client.ReviewThreads).
// Optional: without it a re-review names no threads file and the judge reads
// the replies itself.
type ThreadLister interface {
	ReviewThreads(ctx context.Context, owner, repo string, number int) ([]github.Thread, error)
}

var (
	// replyAgentPrefix is the tag an agent puts before every reply it writes:
	// "(Claude)" from the team's resolve-review skill, "[Codex]" and the like.
	replyAgentPrefix = regexp.MustCompile(`(?i)^[(\[](?:claude|codex|copilot|cursor|gemini|devin|agent|bot|ai)(?:[ -][a-z]+)?[)\]]:?`)
	// replyClauseEnd splits a reply's first paragraph into clauses: at a
	// sentence end, a comma, colon or semicolon (with any closing markup), a
	// dash and a line break. Dots inside "v1.2" or a URL's colon do not split.
	replyClauseEnd = regexp.MustCompile("[.!?;:,]+[*_`)\\]]*(?:\\s+|$)|\\s+-{1,2}\\s+|\\s*[–—]+\\s*|\\n+")
	// replyClauseLead is what may precede a verdict in its clause: a
	// conjunction ("but out of scope") and a subject ("this is intentional",
	// "it was already addressed", "that's incorrect").
	replyClauseLead = regexp.MustCompile(`(?i)^(?:(?:but|and|so)\s+)?(?:(?:this|that|it)(?:'s|\s+(?:is|was|has\s+been))\s+)?(?:now\s+)?`)
	// replyRe is the comment an agent answers, before the reply's first
	// word: "Re 4100000001:".
	replyRe = regexp.MustCompile(`(?i)^re\s+#?\d+\s*:`)
	// replyAck is a clause that acknowledges without a verdict ("Good catch",
	// "Valid", "Analyzed", "Noted", "Low priority", "Low priority (Net 0)",
	// "Correction", "You're right"): the verdict follows.
	replyAck = regexp.MustCompile(`(?i)^(?:(?:good|nice|great)\s+(?:catch|find|point|call)|valid(?:\s+(?:point|concern|finding|catch))?|` +
		`analy[sz]ed|noted|low\s+priority(?:\s*\([^)]*\)?)?|thanks?(?:\s+you)?|agreed|true|right|correct|confirmed|acknowledged|fair(?:\s+(?:point|enough))?|` +
		`yes|yep|ok(?:ay)?|sure|investigated|checked|verified|reviewed|correction|you(?:'re|\s+are)\s+(?:right|correct))$`)
	// replyLowPriority is the acknowledgement that declines the fix unless a
	// clause says it was made.
	replyLowPriority = regexp.MustCompile(`(?i)^low\s+priority\b`)
	// replyClasses match a verdict clause (replyClauseLead dropped), in order:
	// a clause that starts with "fixed" is ReplyFixed though it ends in "for
	// now".
	replyClasses = []struct {
		class string
		re    *regexp.Regexp
	}{
		{ReplyFixed, regexp.MustCompile("(?i)^(?:(?:already\\s+)?(?:fixed|done|addressed|applied)\\b|" +
			"(?:|.*\\b(?:is|are|was|were|been)\\s+)(?:already\\s+|now\\s+)?(?:covered\\s+in|resolved\\s+(?:by|in))\\s+`?[0-9a-f]{7,40}\\b|" +
			"(?:|.*\\b(?:is|are|was|were|been|now|also|landed)\\s+)on\\s+this\\s+branch\\s+as\\s+`?[0-9a-f]{7,40}\\b)")},
		{ReplyNotABug, regexp.MustCompile(`(?i)^(?:(?:not\s+a\s+bug|by\s+design|(?:as\s+)?intended|intentional(?:ly)?)\b|` +
			`(?:incorrect|moot(?:\s+point)?|not\s+applicable)$|(?:(?:this|the)\s+(?:concern|finding|comment|issue)\s+)?does(?:\s+not|n't)\s+apply\b)`)},
		{ReplyWontFix, regexp.MustCompile(`(?i)^(?:(?:won't\s+fix|wont\s+fix|will\s+not\s+fix|wontfix|out\s+of\s+scope|follow[- ]?up|declined|deprioriti[sz]ed|` +
			`left\s+open|deferred|not\s+(?:fixed|changed)\s+in\s+this\s+(?:pr|push|round))\b|` +
			`(?:kept|keeping|left|leaving)(?:\s+(?:it|this|that))?\s+as[- ]is\b|kept$|` +
			`.*\bstay(?:s|ing)?\s+as(?:\s+it\s+|[- ])is\b|(?:no|not|nothing|none|kept|keeping|left|leaving)\b.*\bfor\s+now$)`)},
	}
	// replyCommitVerb is a clause that starts with a commit and a verb: "84c0b1e
	// adds …", "9be04f2 also fixes …". replyCommitFixes tells the commit from a
	// number and the verb from "is" and the like.
	replyCommitVerb = regexp.MustCompile(`^([0-9a-f]{7,40})\s+(?:also\s+|now\s+)?([a-z]+s)\b`)
	// replyByDesign anywhere in the first paragraph argues the finding when
	// no clause names a verdict ("…, so such a behaviour is by design").
	replyByDesign = regexp.MustCompile(`(?i)\bby\s+(?:design|decision)\b`)
	// replyNegativeScore is a negative score for the proposed fix, with a
	// hyphen-minus or a U+2212 minus: "I score that fix at −8", "scored the
	// fix at -3", "Net: −3", "net -2.5".
	replyNegativeScore = regexp.MustCompile(`(?i)(?:\bscore[sd]?\s+(?:that|the|this)\s+fix\s+at|\bnet:?)\s*[-−]\s*\d`)
	// replyDeclines is a clause (replyClauseLead dropped) that declines the
	// fix: "we accept the risk", "not worth it", "but not worth the cost".
	replyDeclines = regexp.MustCompile(`(?i)^(?:we\s+accept\s+(?:the|this)\s+risk|not\s+worth\s+(?:it|the))\b`)
)

// classifyReply says what a reply claims from its first clause: "fixed",
// "done", "addressed", "applied", "already addressed", "<it> is covered in
// <sha>", "(already) resolved by <sha>" (or "in"), "<it> is now on this branch
// as <sha>" or a commit and a verb ("84c0b1e adds …") (ReplyFixed); "not a
// bug", "incorrect", "moot", "by design", "intended", "intentional"
// (ReplyNotABug); "won't fix", "declined", "out of scope", "follow-up", "kept
// as is", "<it> stays as is", "left open", "deferred", "not fixed in this
// PR" (or push, round; or "not changed"), "No guard for now" (ReplyWontFix).
// An acknowledgement ("Good catch", "Valid", "Analyzed", "Noted", "Low
// priority", "Low priority (Net 0)", "Correction", "You're right") passes the
// verdict on to a later clause of the first paragraph ("Good catch, fixed in
// <sha>", "Noted — left as is", "Noted. <why>. Deferred.", "Correction:
// you're right. <where it landed>. The same fix is now on this branch as
// <sha>"); a low priority acknowledgement no verdict follows is ReplyWontFix
// ("Low priority (Net +1): not worth a change on its own"; "Low priority —
// fixed in <sha>" stays ReplyFixed). "Resolved" without a commit is no verdict.
// Anything else is ReplyOther, as is another acknowledgement no verdict
// follows, unless the paragraph declines the fix before any clause says fixed
// or not a bug (replyDeclined: "Confirmed. Still open … so I score that fix at
// −8" is ReplyWontFix), or says "by design" or "by decision" anywhere and no
// clause names a verdict (ReplyNotABug: "…, so such a behaviour is by
// design"). The class is a hint for the judge, not a verdict. Case does not
// matter; leading quoted lines (">"), markup, emoji, an agent's "(Claude)" tag
// and the comment it answers ("Re 4100000001:") are skipped, and a verdict may
// follow "but" or "this is" ("Valid, but out of scope", "Analyzed — this is
// intentional").
func classifyReply(body string) string {
	s := strings.ReplaceAll(body, "’", "'")
	for {
		s = strings.TrimLeftFunc(s, unicode.IsSpace)
		if !strings.HasPrefix(s, ">") {
			break
		}
		_, rest, ok := strings.Cut(s, "\n")
		if !ok {
			return ReplyOther // nothing but a quote
		}
		s = rest
	}
	s = trimReplyLead(s)
	if loc := replyAgentPrefix.FindStringIndex(s); loc != nil {
		s = trimReplyLead(s[loc[1]:])
	}
	if loc := replyRe.FindStringIndex(s); loc != nil {
		s = trimReplyLead(s[loc[1]:])
	}
	if para, _, ok := strings.Cut(s, "\n\n"); ok {
		s = para
	}
	clauses := replyClauses(s)
	acked, low := false, false
	for _, c := range clauses {
		if class := replyVerdict(c.text); class != "" {
			return class
		}
		if replyAck.MatchString(c.text) {
			acked = true
			low = low || replyLowPriority.MatchString(c.text)
		} else if !acked {
			break // the first clause claims nothing known
		}
	}
	if low {
		return ReplyWontFix // every clause read, none says fixed
	}
	if class := replyDeclined(s, clauses); class != ReplyOther {
		return class
	}
	if replyByDesign.MatchString(s) && !slices.ContainsFunc(clauses, func(c replyClause) bool { return replyVerdict(c.text) != "" }) {
		return ReplyNotABug
	}
	return ReplyOther
}

// replyDeclined is ReplyWontFix when the paragraph s weighs the proposed fix
// and declines it before any clause says fixed or not a bug: a negative score
// for the fix anywhere ("I score that fix at −8", "Net: −3"), or a clause
// "we accept the risk" or "not worth it". Otherwise it is ReplyOther.
func replyDeclined(s string, clauses []replyClause) string {
	scored := len(s)
	if loc := replyNegativeScore.FindStringIndex(s); loc != nil {
		scored = loc[0]
	}
	for _, c := range clauses {
		if class := replyVerdict(c.text); class == ReplyFixed || class == ReplyNotABug {
			return ReplyOther
		}
		if scored < c.end || replyDeclines.MatchString(c.text[len(replyClauseLead.FindString(c.text)):]) {
			return ReplyWontFix
		}
	}
	return ReplyOther
}

// replyClause is a clause of a reply's first paragraph, trimmed of what is no
// letter or digit at either end, and the offset in the paragraph where it ends.
type replyClause struct {
	text string
	end  int
}

// replyClauses splits a reply's first paragraph at replyClauseEnd, leaving
// out the clauses that trim to nothing.
func replyClauses(s string) []replyClause {
	var out []replyClause
	start := 0
	for _, loc := range append(replyClauseEnd.FindAllStringIndex(s, -1), []int{len(s), len(s)}) {
		c := strings.TrimFunc(s[start:loc[0]], func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		start = loc[1]
		if c != "" {
			out = append(out, replyClause{text: c, end: loc[0]})
		}
	}
	return out
}

// replyVerdict is the class a clause claims (replyClauseLead dropped), or ""
// when it claims none.
func replyVerdict(c string) string {
	v := c[len(replyClauseLead.FindString(c)):]
	for _, rc := range replyClasses {
		if rc.re.MatchString(v) {
			return rc.class
		}
	}
	if replyCommitFixes(v) {
		return ReplyFixed
	}
	return ""
}

// replyCommitFixes reports whether a clause starts with a commit and the
// verb that says what it does ("84c0b1e adds …"): the commit has a letter and
// a digit (a count such as "2026100 requests" is no commit), and the verb is
// not "is", "was", "has", "does" or "keeps".
func replyCommitFixes(c string) bool {
	m := replyCommitVerb.FindStringSubmatch(c)
	if m == nil || !strings.ContainsAny(m[1], "abcdef") || !strings.ContainsAny(m[1], "0123456789") {
		return false
	}
	return !slices.Contains([]string{"is", "was", "has", "does", "keeps"}, m[2])
}

// trimReplyLead drops what precedes a reply's first word: spaces, markup,
// punctuation and emoji (an opening parenthesis or bracket stays for the
// agent's tag).
func trimReplyLead(s string) string {
	return strings.TrimLeftFunc(s, func(r rune) bool {
		return r != '(' && r != '[' && !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// excerpt is s trimmed and cut to at most n characters, the last one an
// ellipsis when it was cut.
func excerpt(s string, n int) (string, bool) {
	s = strings.TrimSpace(s)
	c := textx.Clip(s, n)
	return c, c != s
}

// ownThreads turns the PR's threads into the judge's: only those whose
// first comment the reviewer login (or a former login of the PR, see
// RoundInput.FormerLogins) wrote, every other comment a reply, classified
// unless one of those logins wrote it.
func (rd *round) ownThreads(ts []github.Thread) []agents.ReviewThread {
	out := []agents.ReviewThread{}
	for _, t := range ts {
		if len(t.Comments) == 0 || !rd.isOwnHistory(t.Comments[0].AuthorLogin, t.Comments[0].AuthorType) {
			continue
		}
		root := t.Comments[0]
		finding, _ := excerpt(textx.FirstLine(root.Body), findingLineMax)
		loc := t.Path
		if line := cmp.Or(t.Line, t.OriginalLine); line > 0 {
			loc = fmt.Sprintf("%s:%d", t.Path, line)
		}
		rt := agents.ReviewThread{ID: t.ID, CommentID: root.ID, URL: root.URL, Finding: finding, Location: loc,
			ReviewID: root.ReviewID, Commit: root.OriginalCommitOid,
			Resolved: t.Resolved, Outdated: t.Outdated, Replies: []agents.ThreadReply{}}
		if !root.CreatedAt.IsZero() {
			rt.CreatedAt = root.CreatedAt.UTC().Format(time.RFC3339)
		}
		for _, c := range t.Comments[1:] {
			author := c.AuthorLogin
			if author == "" {
				author = "ghost"
			}
			rep := agents.ThreadReply{ID: c.ID, Author: author, Own: rd.isOwnHistory(c.AuthorLogin, c.AuthorType)}
			if !rep.Own {
				rep.Class = classifyReply(c.Body)
			} else if _, kind, ok := postreview.ParseReplyMarker(c.Body); ok {
				rep.Kind = kind
			}
			rep.Body, rep.Truncated = excerpt(c.Body, replyExcerptMax)
			rt.Replies = append(rt.Replies, rep)
		}
		rt.Rebuttals, rt.Stop = rebuttals(rt.Replies)
		out = append(out, rt)
	}
	return out
}

// rebuttals counts the reviewer's rebuttals among a thread's replies (its
// own replies of kind rebuttal, and those without a kind: magnum's
// rebuttals carried no reply marker before reply rounds existed) and
// reports whether it stops arguing there: after two of them, the last
// reply is someone else's. post-review refuses another reply in such a
// thread, and the engine asks the operator.
func rebuttals(replies []agents.ThreadReply) (n int, stop bool) {
	for _, r := range replies {
		if r.Own && (r.Kind == "" || r.Kind == postreview.ReplyRebuttal) {
			n++
		}
	}
	return n, n >= 2 && len(replies) > 0 && !replies[len(replies)-1].Own
}

// threadSummary counts threads and their replies by class for the prompt,
// e.g. "3 threads (1 resolved, 1 outdated); replies: 1 fixed, 1 not a bug;
// 1 thread without a reply". The reviewer's own replies are not counted.
func threadSummary(ts []agents.ReviewThread) string {
	if len(ts) == 0 {
		return "no threads"
	}
	resolved, outdated, silent := 0, 0, 0
	counts := map[string]int{}
	for _, t := range ts {
		if t.Resolved {
			resolved++
		}
		if t.Outdated {
			outdated++
		}
		answered := false
		for _, r := range t.Replies {
			if !r.Own {
				counts[r.Class]++
				answered = true
			}
		}
		if !answered {
			silent++
		}
	}
	s := textx.Count(len(ts), "thread", "threads")
	var state []string
	if resolved > 0 {
		state = append(state, fmt.Sprintf("%d resolved", resolved))
	}
	if outdated > 0 {
		state = append(state, fmt.Sprintf("%d outdated", outdated))
	}
	if len(state) > 0 {
		s += " (" + strings.Join(state, ", ") + ")"
	}
	var replies []string
	for _, c := range []string{ReplyFixed, ReplyNotABug, ReplyWontFix, ReplyOther} {
		if counts[c] > 0 {
			replies = append(replies, fmt.Sprintf("%d %s", counts[c], c))
		}
	}
	switch {
	case len(replies) == 0:
		return s + "; no replies"
	case silent > 0:
		return s + "; replies: " + strings.Join(replies, ", ") + "; " + textx.Count(silent, "thread", "threads") + " without a reply"
	}
	return s + "; replies: " + strings.Join(replies, ", ")
}

// isOwnHistory reports whether a comment or review author is the round's
// reviewer login or one of the PR's former logins (same login ignoring
// "[bot]", and the same kind of account: a login ending in "[bot]" is a
// bot's). Only history reads use it; verification accepts the reviewer
// login alone (isReviewer).
func (rd *round) isOwnHistory(login, typ string) bool {
	if rd.isReviewer(login, typ) {
		return true
	}
	return login != "" && slices.ContainsFunc(rd.in.FormerLogins, func(f string) bool {
		return github.IsAccount(login, typ, f, strings.HasSuffix(strings.ToLower(f), "[bot]"))
	})
}

// addThreads reads the reviewer's threads on the PR (its former logins'
// included) for a re-review, or a recovery of a PR with an earlier review,
// and fills jd's Threads, ThreadsFile and ThreadSummary. A dry run, a client
// that cannot list threads, a failed read or a file that cannot be written
// leaves them empty: the judge then reads the replies itself.
func (rd *round) addThreads(ctx context.Context, jd *agents.JudgeData) {
	recovery := rd.in.Kind == KindRecovery && rd.in.Previous != nil && rd.in.Previous.ID != 0
	if (rd.in.Kind != KindRereview && !recovery) || rd.in.DryRun {
		return
	}
	lister, ok := rd.r.GitHub.(ThreadLister)
	if !ok {
		return
	}
	ts, err := lister.ReviewThreads(ctx, rd.owner, rd.name, rd.in.PR.Number)
	if err != nil {
		if ctx.Err() == nil {
			rd.event(ctx, "warn", "round.threads", fmt.Sprintf("could not read the review threads; the judge reads the replies itself: %v", err), nil)
		}
		return
	}
	threads := rd.ownThreads(ts)
	b, err := json.MarshalIndent(threads, "", "  ")
	if err != nil {
		rd.logAt(slog.LevelWarn, "pipeline: encode review threads: %v", err)
		return
	}
	path := filepath.Join(rd.dir, ThreadsFile)
	if err := fsx.WriteFileAtomic(path, append(b, '\n'), 0o600); err != nil {
		rd.event(ctx, "warn", "round.threads", fmt.Sprintf("could not write the review threads; the judge reads the replies itself: %v", err), nil)
		return
	}
	jd.Threads, jd.ThreadsFile, jd.ThreadSummary = threads, path, threadSummary(threads)
	var stops []StopThread
	for _, t := range threads {
		if t.Stop {
			stops = append(stops, StopThread{ID: t.ID, URL: t.URL, LastReply: t.Replies[len(t.Replies)-1].ID})
		}
	}
	jd.StopThreads = len(stops)
	rd.mu.Lock()
	rd.res.ThreadsRead, rd.res.Stops = true, stops
	rd.mu.Unlock()
	rd.event(ctx, "info", "round.threads", fmt.Sprintf("%s by %s: %s", ThreadsFile, rd.login, jd.ThreadSummary),
		map[string]any{"threads": len(threads), "file": path})
}
