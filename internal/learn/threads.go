package learn

// Magnum's own findings in the retro (DECISIONS "The retro classifies the
// replies to magnum's own findings"): a review thread whose root comment is
// in a review magnum posted and that someone else answered goes to the
// classifier with the finding as posted and the replies, and comes back
// with what the author did with it (ThreadItem), stored as a decline
// (store.Decline). Reply text stays in the retro's files.

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/postreview"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// OwnAuthor is a reply's Author when magnum wrote it: no GitHub login has
// parentheses, so it never names a person.
const OwnAuthor = "(magnum)"

// FixerAuthor is a reply's Author when magnum's fixer wrote it
// (postreview.FixReplyMarker): the fixer answers for the PR's author, as
// that author's login, but what it chose is no person's answer.
const FixerAuthor = "(magnum fix)"

// IsMagnum reports whether a reply's Author is one of magnum's (OwnAuthor,
// FixerAuthor) and so names no person.
func IsMagnum(author string) bool { return author == OwnAuthor || author == FixerAuthor }

// OwnTitleMax bounds an own thread's Title, in runes.
const OwnTitleMax = 200

// OwnThread is a review thread magnum started that someone else answered,
// for the classifier ("o<comment id>"): the finding as magnum posted it
// (where, its title and priority) and every reply after it.
type OwnThread struct {
	ID        string `json:"id"`
	URL       string `json:"url"` // GitHub's link to the root comment
	CommentID int64  `json:"comment_id"`
	ReviewID  int64  `json:"review_id,omitempty"` // the review magnum posted the root comment in (Input.ReviewRuns)
	// Path and Line are where the root comment sits (Line 0 for a file
	// comment or a comment on deleted lines, as for a Candidate).
	Path string `json:"path,omitempty"`
	Line int    `json:"line,omitempty"`
	// Title is the root comment's first line without its emphasis and
	// priority tag, Priority the first P0 to P3 on that line ("" when none).
	Title    string `json:"title"`
	Priority string `json:"priority,omitempty"`
	// ReviewedSHA is the commit the finding was about: the root comment's
	// when magnum reviewed it, else the newest reviewed before the comment.
	ReviewedSHA string `json:"reviewed_sha"`
	// FindingRef is the posted finding ("<run id>/<finding id>") of the
	// root's review on the same path within NearLines; "" when none.
	FindingRef string `json:"finding_ref,omitempty"`
	// File is the finding's file at ReviewedSHA, relative to the
	// candidates file's directory, which the classifier judges the finding
	// against; FileSkipped says why there is no copy. The engine fills
	// both, as for a Candidate.
	File        string        `json:"file,omitempty"`
	FileSkipped string        `json:"file_skipped,omitempty"`
	Replies     []ThreadReply `json:"replies"`
	// Fixer is set when magnum's fixer gave the thread's only answers
	// (FixerAuthor): its outcome is stored, but no lesson is drawn from it
	// (parseThreads), since no person answered the finding.
	Fixer bool `json:"fixer,omitempty"`
}

// ThreadReply is one reply in an own thread: who wrote it (Account form,
// OwnAuthor for magnum, FixerAuthor for its fixer) and what it says, data
// for the classifier only.
type ThreadReply struct {
	Author string `json:"author"`
	Body   string `json:"body"`
}

// priorityRe finds a priority on a finding's first line.
var priorityRe = regexp.MustCompile(`\bP[0-3]\b`)

// OwnThreads returns the threads of in whose root comment is by one of
// in.Own in a review magnum posted (in.ReviewRuns: a gh identity is the
// operator's own login, and what he writes by hand is no finding of
// magnum's) with at least one reply by someone else (the author or another
// reviewer; not a ghost, not a bot unless in.IncludeBots), in the order
// GitHub listed them. A reply is the fixer's (FixerAuthor) when it carries
// the fixer's marker and comes from the PR's author or one of in.Own
// (fixerReply): it answers the finding, and the thread is Fixer when no
// one else did. A reply is magnum's (OwnAuthor) when it carries magnum's
// reply marker or comes from one of in.Own's Apps; the operator replying by
// hand is someone else. The finding binds to a posted one of the root's
// review's run by path and line, as Build's nearness does.
func OwnThreads(in Input) []OwnThread {
	reviewed := map[string]bool{}
	for _, r := range in.Reviewed {
		reviewed[r.SHA] = true
	}
	var out []OwnThread
	for _, t := range in.Threads {
		if len(t.Comments) == 0 {
			continue
		}
		root := t.Comments[0]
		run := in.ReviewRuns[root.ReviewID]
		if !in.own(github.Account(root.AuthorLogin, root.AuthorType)) || root.ReviewID <= 0 || run == "" || root.ID <= 0 || root.URL == "" {
			continue
		}
		var replies []ThreadReply
		byPerson, byFixer := false, false
		for _, c := range t.Comments[1:] {
			who := github.Account(c.AuthorLogin, c.AuthorType)
			switch {
			case who == "":
				continue
			case in.fixerReply(who, c):
				who, byFixer = FixerAuthor, true
			case in.ownReply(who, c):
				who = OwnAuthor
			case in.IncludeBots || !github.IsBot(c.AuthorType, who):
				byPerson = true
			}
			replies = append(replies, ThreadReply{Author: who, Body: c.Body})
		}
		if !byPerson && !byFixer {
			continue
		}
		title, priority := findingTitle(root.Body)
		o := OwnThread{ID: fmt.Sprintf("o%d", root.ID), URL: root.URL, CommentID: root.ID, ReviewID: root.ReviewID,
			Path: t.Path, Line: t.OriginalLine, Title: title, Priority: priority, Replies: replies, Fixer: !byPerson}
		if t.DiffSide == "LEFT" { // deleted lines: numbered in the old file
			o.Line = 0
		}
		if root.OriginalCommitOid != "" && reviewed[root.OriginalCommitOid] {
			o.ReviewedSHA = root.OriginalCommitOid
		} else {
			o.ReviewedSHA = cmp.Or(in.reviewedBefore(root.CreatedAt), root.OriginalCommitOid, in.newest())
		}
		c := Candidate{Path: o.Path, Line: o.Line}
		if t.OriginalStartLine > 0 && t.OriginalStartLine < t.OriginalLine && o.Line > 0 {
			c.StartLine = t.OriginalStartLine
		}
		fs := slices.DeleteFunc(slices.Clone(in.Findings), func(f store.Finding) bool { return f.RunID != run })
		if f := near(fs, store.FindingPosted, c); f != nil {
			o.FindingRef = findingRef(*f)
		}
		out = append(out, o)
	}
	return out
}

// own reports whether login (Account form) is one of magnum's.
func (in Input) own(login string) bool {
	return login != "" && slices.ContainsFunc(in.Own, func(o string) bool { return github.SameAccount(o, login) })
}

// ownReply reports whether reply c by who (Account form) is magnum's: by
// one of its logins, and either one of its Apps, which no person can write
// as, or carrying the reply marker post-review appends
// (postreview.ReplyMarker). A gh identity's login is the operator's own,
// so what he replies by hand is someone else's answer.
func (in Input) ownReply(who string, c github.ThreadComment) bool {
	if !in.own(who) {
		return false
	}
	_, _, marked := postreview.ParseReplyMarker(c.Body)
	return marked || github.IsBot(c.AuthorType, who)
}

// fixerReply reports whether reply c by who (Account form) is magnum's
// fixer's: it carries the marker the fix skill ends every reply with
// (postreview.FixReplyMarker), and it comes from the PR's author, whom the
// fixer acts as, or from one of magnum's logins (a gh identity's is the
// operator's own). The marker in anyone else's reply makes no fixer of them.
func (in Input) fixerReply(who string, c github.ThreadComment) bool {
	return postreview.IsFixReply(c.Body) && (github.SameAccount(in.Author, who) || in.own(who))
}

// simplificationRe is a simplification comment's heading once its bold is
// gone: the word and the optional tag, "(optional, no reply needed)" in the
// judge prompts, "(optional)" in the earlier ones.
var simplificationRe = regexp.MustCompile(`(?i)^simplification(?:\s*\(.*\))?$`)

// findingTitle is a posted finding's title and priority from its comment:
// the first line that is not blank, without its bold and italic markers
// and the "[P2]" tag in front, clipped to OwnTitleMax runes, and the first
// P0 to P3 on that line. A simplification comment's first line is the
// heading alone (prompts/judge-*.md), so its title is "Simplification: "
// and the first sentence of the paragraph below it, the one on what the
// proposal removes ("Simplification" when there is none), and it has no
// priority.
func findingTitle(body string) (string, string) {
	head, rest := splitHead(body)
	priority := priorityRe.FindString(head)
	title := plainText(head)
	if after, ok := strings.CutPrefix(title, "["+priority+"]"); ok && priority != "" {
		title = strings.TrimSpace(after)
	}
	if simplificationRe.MatchString(title) {
		title = "Simplification"
		if s := plainText(firstSentence(rest)); s != "" {
			title += ": " + s
		}
	}
	return textx.Clip(title, OwnTitleMax), priority
}

// splitHead is the first line of s that is not blank, trimmed, and what
// follows it.
func splitHead(s string) (head, rest string) {
	off := 0
	for line := range strings.Lines(s) {
		off += len(line)
		if t := strings.TrimSpace(line); t != "" {
			return t, s[off:]
		}
	}
	return "", ""
}

// plainText is s without its bold and italic markers: every run of '*' and
// '_' except one between two letters or digits (snake_case) and any inside
// a code span, trimmed.
func plainText(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i := 0; i < len(rs); {
		r := rs[i]
		if r == '`' {
			if end := slices.Index(rs[i+1:], '`'); end >= 0 { // a code span, kept as it is
				b.WriteString(string(rs[i : i+end+2]))
				i += end + 2
				continue
			}
		}
		if r == '*' || r == '_' {
			j := i
			for j < len(rs) && (rs[j] == '*' || rs[j] == '_') {
				j++
			}
			if i > 0 && j < len(rs) && isWordRune(rs[i-1]) && isWordRune(rs[j]) {
				b.WriteString(string(rs[i:j]))
			}
			i = j
			continue
		}
		b.WriteRune(r)
		i++
	}
	return strings.TrimSpace(b.String())
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// firstSentence is the first sentence of the first paragraph of s (its
// lines up to the first blank one, joined by spaces): up to the first '.',
// '!' or '?' outside a code span that is followed by a space or the end,
// with the emphasis or quote it ends inside. It is "" when s has no
// paragraph or the paragraph is a code fence, a suggestion with nothing
// said above it.
func firstSentence(s string) string {
	var para []string
	for line := range strings.Lines(s) {
		t := strings.TrimSpace(line)
		if t == "" {
			if len(para) > 0 {
				break
			}
			continue
		}
		if len(para) == 0 && strings.HasPrefix(t, "```") {
			return ""
		}
		para = append(para, t)
	}
	rs := []rune(strings.Join(para, " "))
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '`':
			if end := slices.Index(rs[i+1:], '`'); end >= 0 {
				i += end + 1
			}
		case '.', '!', '?':
			j := i + 1
			for j < len(rs) && strings.ContainsRune("*_)\"'", rs[j]) {
				j++
			}
			if j == len(rs) || unicode.IsSpace(rs[j]) {
				return string(rs[:j])
			}
		}
	}
	return string(rs)
}

// ThreadItem is the classifier's answer for one own thread: what the
// author did with the finding (Outcome, store.Decline*), whether magnum
// was right for a declined or argued one (Verdict, store.Verdict*), and
// for a wrong, over-ranked or missing-context one a lesson with its scope
// and the patterns that recognize the same finding again.
type ThreadItem struct {
	ID      string   `json:"id"`
	Outcome string   `json:"outcome"`
	Verdict string   `json:"verdict,omitempty"`
	Lesson  string   `json:"lesson,omitempty"`
	Scope   string   `json:"scope,omitempty"`
	Match   []string `json:"match,omitempty"`
}

// checkThread validates one thread item and returns what is kept of it:
// the outcome alone, with the verdict for a declined or argued finding,
// with the lesson, scope and patterns when the verdict teaches. A thread
// only the fixer answered (fixer, OwnThread.Fixer) teaches nothing: its
// verdict needs no lesson, and any lesson, scope or patterns are dropped,
// so its decline makes no learned case and no repository miss.
func checkThread(it ThreadItem, fixer bool) (ThreadItem, error) {
	switch it.Outcome {
	case store.DeclineFixed, store.DeclineDeferred, store.DeclineQuestion, store.DeclineUnrelated:
		return ThreadItem{ID: it.ID, Outcome: it.Outcome}, nil
	case store.DeclineDeclined, store.DeclineArgued:
	default:
		return ThreadItem{}, fmt.Errorf("outcome must be fixed, declined, deferred, argued, question or unrelated")
	}
	switch it.Verdict {
	case store.VerdictRight, store.VerdictWrong, store.VerdictOverRanked, store.VerdictMissingContext:
	default:
		return ThreadItem{}, fmt.Errorf("a declined or argued finding needs a verdict: wrong, over_ranked, missing_context or right")
	}
	if it.Verdict == store.VerdictRight || fixer {
		return ThreadItem{ID: it.ID, Outcome: it.Outcome, Verdict: it.Verdict}, nil
	}
	var errs []error
	it.Lesson = strings.TrimSpace(it.Lesson)
	if it.Lesson == "" {
		errs = append(errs, fmt.Errorf("a %s verdict needs a lesson", it.Verdict))
	}
	if it.Scope != store.MissScopeRepo && it.Scope != store.MissScopeGeneral {
		errs = append(errs, fmt.Errorf("a %s verdict needs a scope, repo or general", it.Verdict))
	}
	errs = append(errs, checkMatch(it.Match, "a "+it.Verdict+" verdict")...)
	if len(errs) > 0 {
		return ThreadItem{}, errors.Join(errs...)
	}
	return it, nil
}
