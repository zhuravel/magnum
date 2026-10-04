package pipeline

// The reply contract (skills/magnum-review/SKILL.md section 6): a re-review
// reads the inline threads its reviewer login started on the PR, classifies
// every reply by its first words and hands them to the judge as a file next
// to the candidate reports. Replies are PR content, so the prompt names the
// file and the counts, never the text.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/github"
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
	// replyAgentPrefix is the tag the team's resolve-review skill puts before
	// every reply it writes.
	replyAgentPrefix = regexp.MustCompile(`(?i)^\(claude\)`)
	// replyClasses match a reply's first words (after any quote, markup and
	// the agent prefix), in order.
	replyClasses = []struct {
		class string
		re    *regexp.Regexp
	}{
		{ReplyNotABug, regexp.MustCompile(`(?i)^(?:not a bug|by design|(?:as )?intended|intentional(?:ly)?)\b`)},
		{ReplyWontFix, regexp.MustCompile(`(?i)^(?:won'?t fix|will not fix|wontfix|out of scope|follow[- ]?up)\b`)},
		{ReplyFixed, regexp.MustCompile(`(?i)^(?:fixed|done|addressed)\b`)},
	}
)

// classifyReply says what a reply claims from its first words: "fixed",
// "done" or "addressed in <sha>" (ReplyFixed); "not a bug", "by design",
// "intended" (ReplyNotABug); "won't fix", "out of scope", "follow-up"
// (ReplyWontFix); anything else is ReplyOther. Case does not matter; leading
// quoted lines (">"), markup, emoji and a "(Claude)" prefix are skipped.
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
	for _, c := range replyClasses {
		if c.re.MatchString(s) {
			return c.class
		}
	}
	return ReplyOther
}

// trimReplyLead drops what precedes a reply's first word: spaces, markup,
// punctuation and emoji (an opening parenthesis stays for the agent prefix).
func trimReplyLead(s string) string {
	return strings.TrimLeftFunc(s, func(r rune) bool {
		return r != '(' && !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// excerpt is s trimmed and cut to at most n characters, the last one an
// ellipsis when it was cut.
func excerpt(s string, n int) (string, bool) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	r := []rune(s)
	return strings.TrimRightFunc(string(r[:n-1]), unicode.IsSpace) + "…", true
}

// firstLine is the first non-empty line of s, trimmed.
func firstLine(s string) string {
	for line := range strings.Lines(s) {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
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
		finding, _ := excerpt(firstLine(root.Body), findingLineMax)
		loc := t.Path
		if line := cmp.Or(t.Line, t.OriginalLine); line > 0 {
			loc = fmt.Sprintf("%s:%d", t.Path, line)
		}
		rt := agents.ReviewThread{ID: t.ID, CommentID: root.ID, URL: root.URL, Finding: finding, Location: loc,
			Resolved: t.Resolved, Outdated: t.Outdated, Replies: []agents.ThreadReply{}}
		for _, c := range t.Comments[1:] {
			author := c.AuthorLogin
			if author == "" {
				author = "ghost"
			}
			rep := agents.ThreadReply{ID: c.ID, Author: author, Own: rd.isOwnHistory(c.AuthorLogin, c.AuthorType)}
			if !rep.Own {
				rep.Class = classifyReply(c.Body)
			}
			rep.Body, rep.Truncated = excerpt(c.Body, replyExcerptMax)
			rt.Replies = append(rt.Replies, rep)
		}
		out = append(out, rt)
	}
	return out
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
	s := fmt.Sprintf("%d thread%s", len(ts), plural(len(ts)))
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
		return s + "; replies: " + strings.Join(replies, ", ") + fmt.Sprintf("; %d thread%s without a reply", silent, plural(silent))
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
		return github.SameLogin(login, f) && github.IsBot(typ, login) == strings.HasSuffix(strings.ToLower(f), "[bot]")
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
		rd.logf("pipeline: encode review threads: %v", err)
		return
	}
	path := filepath.Join(rd.dir, ThreadsFile)
	if err := writeFileAtomic(path, append(b, '\n')); err != nil {
		rd.event(ctx, "warn", "round.threads", fmt.Sprintf("could not write the review threads; the judge reads the replies itself: %v", err), nil)
		return
	}
	jd.Threads, jd.ThreadsFile, jd.ThreadSummary = threads, path, threadSummary(threads)
	rd.event(ctx, "info", "round.threads", fmt.Sprintf("%s by %s: %s", ThreadsFile, rd.login, jd.ThreadSummary),
		map[string]any{"threads": len(threads), "file": path})
}
