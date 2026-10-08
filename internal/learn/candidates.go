// Package learn is the deterministic half of the retro (DECISIONS "Learning
// loop: daily retro"): after a pull request magnum reviewed closes, Build
// turns what other reviewers said about it into candidates for a
// classifier, after dropping what cannot be a miss (the author's and
// magnum's own comments, short approvals, comments on code magnum never
// saw, findings magnum already posted), and ParseOutput reads the
// classifier's answer back, refusing anything off schema and any lesson
// that retells the pull request instead of teaching (ScrubLesson).
//
// The package is pure: it starts no process and touches no network or
// registry. The engine reads GitHub and the registry and hands the data in;
// the one comparison Build may need is a function of Input.
package learn

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// NearLines is how far, in lines, a comment may be from one of magnum's
// findings on the same path and still be about it.
const NearLines = 3

// Reviewed is a commit magnum posted a review of.
type Reviewed struct {
	SHA string
	At  time.Time // when the review was posted
}

// Input is everything Build reads about one pull request.
type Input struct {
	// Author is the pull request's author in Account form
	// (github.Account): its own comments are never candidates.
	Author string
	// Reviewed are the commits magnum reviewed; their order does not matter.
	Reviewed []Reviewed
	// Own are magnum's logins in Account form: every configured identity's,
	// and the ones the pull request's reviews were posted as.
	Own []string
	// Threads and Reviews are the pull request's review threads and
	// reviews as GitHub reports them.
	Threads []github.Thread
	Reviews []github.Review
	// Findings are the judge's findings of the pull request
	// (store.Store.FindingsByPR): a posted one near a comment means magnum
	// caught it; a rejected one, that magnum raised it and let it go. The
	// rejected ones also go to the classifier (Result.Rejected).
	Findings []store.Finding
	// MinChars drops a comment whose text, without quotes and code blocks,
	// is shorter.
	MinChars int
	// IncludeBots counts comments of bot accounts other than magnum's.
	IncludeBots bool
	// Compare compares a reviewed commit with a later one (GitHub's
	// comparison). Build asks it only for an inline comment on a commit
	// magnum did not review; nil means no comparison, so such a comment is
	// outside.
	Compare func(from, to string) (Comparison, error)
}

// Comparison is what Build needs of GitHub's comparison of two commits.
type Comparison struct {
	// Descendant: the later commit descends from the reviewed one (GitHub's
	// status "ahead" or "identical"). The file list is three-dot, so only
	// then is it the difference between the two: for an older commit or a
	// history a force push replaced it lists nothing that matters.
	Descendant bool
	// Paths are the files that differ (renames under both names); Complete
	// is false when GitHub cut the list.
	Paths    []string
	Complete bool
}

// Candidate kinds (store.MissSourceThread, store.MissSourceReview).
const (
	KindThread = store.MissSourceThread
	KindReview = store.MissSourceReview
)

// Candidate is one comment of another reviewer for the classifier: the
// root comment of a review thread ("t<comment id>") or the body of a
// review ("r<review id>").
type Candidate struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"` // KindThread | KindReview
	URL      string `json:"url"`
	Reviewer string `json:"reviewer"` // Account form
	// Path and the line range [StartLine, Line] are where an inline comment
	// sits in the file at ReviewedSHA (both 0 for a file comment, and for a
	// comment on deleted lines, Side "LEFT", whose lines are the old
	// file's); a review body has neither.
	Path      string `json:"path,omitempty"`
	StartLine int    `json:"start_line,omitempty"`
	Line      int    `json:"line,omitempty"`
	Side      string `json:"side,omitempty"` // the diff side: RIGHT, or LEFT for deleted lines
	// ReviewedSHA is the commit magnum reviewed that the comment applies
	// to: the comment's own commit, or a reviewed commit since which the
	// commented file did not change. CommentSHA is the commit the comment
	// was made on.
	ReviewedSHA string    `json:"reviewed_sha"`
	CommentSHA  string    `json:"comment_sha,omitempty"`
	DiffHunk    string    `json:"diff_hunk,omitempty"`
	Body        string    `json:"body"`
	CreatedAt   time.Time `json:"created_at"`
	// Raised is store.MissRaisedRejected when the judge raised a finding
	// near the comment and rejected it (FindingRef "<run id>/<finding id>",
	// ReasonCode its reason), else store.MissRaisedNone. A rejected finding
	// the classifier names replaces it (LinkRejected).
	Raised     string `json:"raised"`
	FindingRef string `json:"finding_ref,omitempty"`
	ReasonCode string `json:"reason_code,omitempty"`
	// File is the commented file at ReviewedSHA, relative to the candidates
	// file's directory; FileSkipped says why there is no copy. The engine
	// fills both.
	File        string `json:"file,omitempty"`
	FileSkipped string `json:"file_skipped,omitempty"`
}

// Lines is the candidate's line range as [from, to] (nil without a line).
func (c Candidate) Lines() []int {
	if c.Line <= 0 {
		return nil
	}
	return []int{max(c.StartLine, 1), c.Line}
}

// Result is what Build made of a pull request.
type Result struct {
	// Candidates are the comments to classify, Outside the ones about code
	// magnum did not review, stored as such without a classifier.
	Candidates []Candidate
	Outside    []Candidate
	// Caught counts comments near a finding magnum posted, Dropped the
	// comments too short or only an approval. Neither is stored.
	Caught, Dropped int
	// Rejected are the findings the judge rejected, the newest RejectedMax
	// with a title, oldest first: the classifier names the one that
	// reports a candidate's problem (Item.Rejected, LinkRejected).
	Rejected []Rejected
}

// Build makes the candidates of a pull request: the root comment of every
// review thread and the body of every review by another reviewer (not the
// author, not one of magnum's logins, not a bot unless IncludeBots).
// Replies are never candidates.
//
// A comment shorter than MinChars without its quotes and code blocks, or
// that only approves, is dropped. One near a finding magnum posted on the
// same path is caught; one near a rejected finding is raised (a comment on
// deleted lines has no line, so neither), until the classifier names
// another (Result.Rejected lists them). A comment made on a reviewed
// commit applies to it; an inline comment on another commit applies to the
// newest commit magnum reviewed before the comment when that commit
// descends from the reviewed one and the commented file did not change
// between the two (Compare), and is outside otherwise, as is a review body
// on a commit magnum did not review. Only a failing Compare is an error; one
// GitHub answers with a 404 (github.ErrNotFound: a commit it no longer has)
// proves nothing, so the comment is outside and the build goes on.
func Build(in Input) (Result, error) {
	res := Result{Rejected: rejectedRows(in.Findings)}
	reviewed := map[string]bool{}
	for _, r := range in.Reviewed {
		reviewed[r.SHA] = true
	}
	compared := map[[2]string]Comparison{}
	unchanged := func(from, to, path string) (bool, error) {
		if in.Compare == nil {
			return false, nil
		}
		key := [2]string{from, to}
		cmpd, ok := compared[key]
		if !ok {
			var err error
			// GitHub cannot compare a commit it no longer has (a force
			// push, a deleted branch): a comparison that proves nothing.
			if cmpd, err = in.Compare(from, to); err != nil && !errors.Is(err, github.ErrNotFound) {
				return false, fmt.Errorf("compare %s...%s: %w", short(from), short(to), err)
			}
			compared[key] = cmpd
		}
		return cmpd.Descendant && cmpd.Complete && !slices.Contains(cmpd.Paths, path), nil
	}

	for _, t := range in.Threads {
		if len(t.Comments) == 0 {
			continue
		}
		cm := t.Comments[0]
		who := github.Account(cm.AuthorLogin, cm.AuthorType)
		if !in.other(who, cm.AuthorType) || cm.ID <= 0 || cm.URL == "" || t.Path == "" {
			continue
		}
		if tooShort(cm.Body, in.MinChars) {
			res.Dropped++
			continue
		}
		c := Candidate{ID: fmt.Sprintf("t%d", cm.ID), Kind: KindThread, URL: cm.URL, Reviewer: who, Path: t.Path,
			Line: t.OriginalLine, Side: t.DiffSide, CommentSHA: cm.OriginalCommitOid, DiffHunk: cm.DiffHunk, Body: cm.Body,
			CreatedAt: cm.CreatedAt, Raised: store.MissRaisedNone}
		if t.OriginalStartLine > 0 && t.OriginalStartLine < t.OriginalLine {
			c.StartLine = t.OriginalStartLine
		}
		if t.DiffSide == "LEFT" { // deleted lines: numbered in the old file, not the reviewed one
			c.Line, c.StartLine = 0, 0
		}
		if near(in.Findings, store.FindingPosted, c) != nil {
			res.Caught++
			continue
		}
		if f := near(in.Findings, store.FindingRejected, c); f != nil {
			c.Raised, c.FindingRef, c.ReasonCode = store.MissRaisedRejected, findingRef(*f), f.ReasonCode
		}
		base := in.reviewedBefore(cm.CreatedAt)
		switch {
		case c.CommentSHA != "" && reviewed[c.CommentSHA]:
			c.ReviewedSHA = c.CommentSHA
		case c.CommentSHA != "" && base != "":
			same, err := unchanged(base, c.CommentSHA, c.Path)
			if err != nil {
				return Result{}, err
			}
			if same {
				c.ReviewedSHA = base // the file is as magnum saw it, so the comment's lines hold there too
			}
		}
		if c.ReviewedSHA == "" {
			c.ReviewedSHA = cmp.Or(base, c.CommentSHA, in.newest())
			res.Outside = append(res.Outside, c)
			continue
		}
		res.Candidates = append(res.Candidates, c)
	}

	for _, r := range in.Reviews {
		who := github.Account(r.AuthorLogin, r.AuthorType)
		switch {
		case r.State != "COMMENTED" && r.State != "CHANGES_REQUESTED" && r.State != "APPROVED":
			continue
		case strings.TrimSpace(r.Body) == "" || !in.other(who, r.AuthorType) || r.DatabaseID <= 0 || r.URL == "":
			continue
		case tooShort(r.Body, in.MinChars):
			res.Dropped++
			continue
		}
		c := Candidate{ID: fmt.Sprintf("r%d", r.DatabaseID), Kind: KindReview, URL: r.URL, Reviewer: who,
			CommentSHA: r.CommitOid, Body: r.Body, CreatedAt: r.SubmittedAt, Raised: store.MissRaisedNone}
		if r.CommitOid != "" && reviewed[r.CommitOid] {
			c.ReviewedSHA = r.CommitOid
			res.Candidates = append(res.Candidates, c)
			continue
		}
		c.ReviewedSHA = cmp.Or(in.reviewedBefore(r.SubmittedAt), r.CommitOid, in.newest())
		res.Outside = append(res.Outside, c)
	}
	return res, nil
}

// other reports whether login (Account form) is another reviewer: not the
// author, not magnum's, not a ghost, and not a bot unless IncludeBots.
func (in Input) other(login, typename string) bool {
	if login == "" || github.SameAccount(login, in.Author) {
		return false
	}
	if slices.ContainsFunc(in.Own, func(o string) bool { return github.SameAccount(o, login) }) {
		return false
	}
	return in.IncludeBots || !github.IsBot(typename, login)
}

// reviewedBefore is the newest commit magnum reviewed before t ("" when
// none).
func (in Input) reviewedBefore(t time.Time) string {
	var best Reviewed
	for _, r := range in.Reviewed {
		if r.At.Before(t) && (best.SHA == "" || r.At.After(best.At)) {
			best = r
		}
	}
	return best.SHA
}

// newest is the newest commit magnum reviewed ("" when none): an outside
// comment with no commit of its own is recorded against it.
func (in Input) newest() string {
	var best Reviewed
	for _, r := range in.Reviewed {
		if best.SHA == "" || r.At.After(best.At) {
			best = r
		}
	}
	return best.SHA
}

// near is the finding of verdict on c's path whose line is nearest to c's
// range, within NearLines (nil when none). Review bodies and file comments
// have no line, so nothing is near them.
func near(fs []store.Finding, verdict string, c Candidate) *store.Finding {
	if c.Path == "" || c.Line <= 0 {
		return nil
	}
	from := max(c.StartLine, 1)
	if c.StartLine == 0 {
		from = c.Line
	}
	var best *store.Finding
	bestD := NearLines + 1
	for i := range fs {
		f := &fs[i]
		if f.Verdict != verdict || f.Path != c.Path || f.Line <= 0 {
			continue
		}
		d := 0
		switch {
		case f.Line < from:
			d = from - f.Line
		case f.Line > c.Line:
			d = f.Line - c.Line
		}
		if d < bestD {
			best, bestD = f, d
		}
	}
	return best
}

// tooShort reports whether a comment says too little to be a miss: its
// text without quoted lines and code blocks is shorter than minChars runes,
// or it only approves.
func tooShort(body string, minChars int) bool {
	text := strings.TrimSpace(prose(body))
	return utf8.RuneCountInString(text) < minChars || onlyApproval(text)
}

// prose is body without quoted lines ("> ...") and fenced code blocks
// (``` or ~~~, suggestion blocks included).
func prose(body string) string {
	var b strings.Builder
	fence := ""
	for line := range strings.Lines(body) {
		t := strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(t, fence) {
				fence = ""
			}
			continue
		}
		switch {
		case strings.HasPrefix(t, "```"):
			fence = "```"
			continue
		case strings.HasPrefix(t, "~~~"):
			fence = "~~~"
			continue
		case strings.HasPrefix(t, ">"):
			continue
		}
		b.WriteString(line)
	}
	return b.String()
}

// approvalWords are the words of an approval ("LGTM", "looks good to me,
// thanks!", "nice work 👍"): a comment made of them alone teaches nothing.
var approvalWords = map[string]bool{
	"lgtm": true, "lg": true, "looks": true, "look": true, "good": true, "great": true, "fine": true, "nice": true,
	"ok": true, "okay": true, "to": true, "me": true, "thanks": true, "thank": true, "you": true, "thx": true,
	"ty": true, "a": true, "lot": true, "so": true, "much": true, "very": true, "really": true, "all": true,
	"overall": true, "now": true, "this": true, "it": true, "ship": true, "approved": true, "approve": true,
	"approving": true, "awesome": true, "work": true, "job": true, "well": true, "done": true, "cool": true,
	"perfect": true, "+1": true, "and": true, "again": true, "for": true, "the": true, "fix": true, "fixes": true,
}

// onlyApproval reports whether text holds nothing but approval words,
// punctuation and emoji.
func onlyApproval(text string) bool {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '+'
	})
	for _, w := range words {
		if !approvalWords[w] {
			return false
		}
	}
	return true
}

func short(sha string) string { return sha[:min(len(sha), 12)] }
