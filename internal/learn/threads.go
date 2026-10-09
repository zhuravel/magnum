package learn

// Magnum's own findings in the retro (DECISIONS "The retro classifies the
// replies to magnum's own findings"): a review thread whose root comment is
// one of magnum's logins and that someone else answered goes to the
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

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// OwnAuthor is a reply's Author when magnum wrote it: no GitHub login has
// parentheses, so it never names a person.
const OwnAuthor = "(magnum)"

// OwnTitleMax bounds an own thread's Title, in runes.
const OwnTitleMax = 200

// OwnThread is a review thread magnum started that someone else answered,
// for the classifier ("o<comment id>"): the finding as magnum posted it
// (where, its title and priority) and every reply after it.
type OwnThread struct {
	ID        string `json:"id"`
	URL       string `json:"url"` // GitHub's link to the root comment
	CommentID int64  `json:"comment_id"`
	ReviewID  int64  `json:"review_id,omitempty"` // the review the root comment was posted in; 0 when unknown
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
	FindingRef string        `json:"finding_ref,omitempty"`
	Replies    []ThreadReply `json:"replies"`
}

// ThreadReply is one reply in an own thread: who wrote it (Account form,
// OwnAuthor for magnum) and what it says, data for the classifier only.
type ThreadReply struct {
	Author string `json:"author"`
	Body   string `json:"body"`
}

// priorityRe finds a priority on a finding's first line.
var priorityRe = regexp.MustCompile(`\bP[0-3]\b`)

// OwnThreads returns the threads of in whose root comment is by one of
// in.Own with at least one reply by someone else (the author or another
// reviewer; not a ghost, not a bot unless in.IncludeBots), in the order
// GitHub listed them. The finding binds to a posted one of the root's
// review (in.ReviewRuns) by path and line, as Build's nearness does.
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
		if !in.own(github.Account(root.AuthorLogin, root.AuthorType)) || root.ID <= 0 || root.URL == "" {
			continue
		}
		var replies []ThreadReply
		answered := false
		for _, c := range t.Comments[1:] {
			who := github.Account(c.AuthorLogin, c.AuthorType)
			switch {
			case who == "":
				continue
			case in.own(who):
				who = OwnAuthor
			case in.IncludeBots || !github.IsBot(c.AuthorType, who):
				answered = true
			}
			replies = append(replies, ThreadReply{Author: who, Body: c.Body})
		}
		if !answered {
			continue
		}
		title, priority := findingTitle(root.Body)
		o := OwnThread{ID: fmt.Sprintf("o%d", root.ID), URL: root.URL, CommentID: root.ID, ReviewID: root.ReviewID,
			Path: t.Path, Line: t.OriginalLine, Title: title, Priority: priority, Replies: replies}
		if t.DiffSide == "LEFT" { // deleted lines: numbered in the old file
			o.Line = 0
		}
		if root.OriginalCommitOid != "" && reviewed[root.OriginalCommitOid] {
			o.ReviewedSHA = root.OriginalCommitOid
		} else {
			o.ReviewedSHA = cmp.Or(in.reviewedBefore(root.CreatedAt), root.OriginalCommitOid, in.newest())
		}
		if run := in.ReviewRuns[root.ReviewID]; run != "" && root.ReviewID > 0 {
			c := Candidate{Path: o.Path, Line: o.Line}
			if t.OriginalStartLine > 0 && t.OriginalStartLine < t.OriginalLine && o.Line > 0 {
				c.StartLine = t.OriginalStartLine
			}
			fs := slices.DeleteFunc(slices.Clone(in.Findings), func(f store.Finding) bool { return f.RunID != run })
			if f := near(fs, store.FindingPosted, c); f != nil {
				o.FindingRef = findingRef(*f)
			}
		}
		out = append(out, o)
	}
	return out
}

// own reports whether login (Account form) is one of magnum's.
func (in Input) own(login string) bool {
	return login != "" && slices.ContainsFunc(in.Own, func(o string) bool { return github.SameAccount(o, login) })
}

// findingTitle is a posted finding's title and priority from its comment:
// the first line that is not blank, without the bold around it and the
// "[P2]" tag in front, clipped to OwnTitleMax runes, and the first P0 to
// P3 on that line.
func findingTitle(body string) (string, string) {
	line := textx.FirstLine(body)
	priority := priorityRe.FindString(line)
	title := strings.Trim(line, "*_ ")
	if rest, ok := strings.CutPrefix(title, "["+priority+"]"); ok && priority != "" {
		title = strings.Trim(rest, "*_ ")
	}
	return textx.Clip(title, OwnTitleMax), priority
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
// with the lesson, scope and patterns when the verdict teaches.
func checkThread(it ThreadItem) (ThreadItem, error) {
	switch it.Outcome {
	case store.DeclineFixed, store.DeclineDeferred, store.DeclineQuestion, store.DeclineUnrelated:
		return ThreadItem{ID: it.ID, Outcome: it.Outcome}, nil
	case store.DeclineDeclined, store.DeclineArgued:
	default:
		return ThreadItem{}, fmt.Errorf("outcome must be fixed, declined, deferred, argued, question or unrelated")
	}
	switch it.Verdict {
	case store.VerdictRight:
		return ThreadItem{ID: it.ID, Outcome: it.Outcome, Verdict: it.Verdict}, nil
	case store.VerdictWrong, store.VerdictOverRanked, store.VerdictMissingContext:
	default:
		return ThreadItem{}, fmt.Errorf("a declined or argued finding needs a verdict: wrong, over_ranked, missing_context or right")
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
