// Package learn is the deterministic half of the retro (DECISIONS "Learning
// loop: daily retro"): after a pull request magnum reviewed closes, Build
// turns what other reviewers said about it into candidates for a
// classifier, after dropping what cannot be a miss (the author's and
// magnum's own comments, short approvals, comments on code magnum never
// saw, findings magnum already posted or dropped as a duplicate of the
// comment), and ParseOutput reads the classifier's answer back, refusing
// anything off schema and any lesson that retells the pull request instead
// of teaching (ScrubLesson). The threads magnum started that someone
// answered go to the classifier too (OwnThreads), which says what the
// author did with each finding.
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
	// and the ones the pull request's reviews were posted as. A gh
	// identity's is the operator's own, so a comment by one of them is
	// never a candidate, and is magnum's finding only in one of magnum's
	// reviews (ReviewRuns, OwnThreads).
	Own []string
	// Threads and Reviews are the pull request's review threads and
	// reviews as GitHub reports them. The reviews magnum posted are among
	// them: a review in ReviewRuns gives the time its run's findings were
	// posted at (SubmittedAt).
	Threads []github.Thread
	Reviews []github.Review
	// Findings are the judge's findings of the pull request
	// (store.Store.FindingsByPR): a posted one near a comment means magnum
	// caught it; a rejected one, that magnum raised it and let it go, or,
	// for a duplicate of a thread the comment predates the review of, that
	// it caught. The rejected ones also go to the classifier
	// (Result.Rejected).
	Findings []store.Finding
	// ReviewRuns names, by review id (runs.review_id), the run of each
	// review magnum posted whose findings are the review's: a thread is
	// magnum's only when its root is in one of these reviews, and its
	// finding is one of that run's (OwnThreads). A finding's review, and so
	// the time it was judged at, is the one its RunID is bound to here.
	ReviewRuns map[int64]string
	// Targets names, by run id, the commit each of magnum's runs on the
	// pull request reviewed (runs.target_sha): a finding is of its run's
	// head. A posted finding catches a comment measured against that head
	// or a later one (postedNear); a rejected finding links only a comment
	// of its head, or of a later reviewed head the commented file did not
	// change since (rejectedNear). A finding whose run is not in it is near
	// none.
	Targets map[string]string
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
	// commented file did not change (for an outside comment, the review
	// before it, else its own commit). CommentSHA is the commit the comment
	// was made on.
	ReviewedSHA string    `json:"reviewed_sha"`
	CommentSHA  string    `json:"comment_sha,omitempty"`
	DiffHunk    string    `json:"diff_hunk,omitempty"`
	Body        string    `json:"body"`
	CreatedAt   time.Time `json:"created_at"`
	// Raised is store.MissRaisedRejected when the judge raised a finding
	// near the comment and rejected it (FindingRef "<run id>/<finding id>",
	// ReasonCode its reason; not for a duplicate of a comment made before
	// the review, which is caught), else store.MissRaisedNone. A rejected
	// finding the classifier names replaces it (LinkRejected).
	Raised     string `json:"raised"`
	FindingRef string `json:"finding_ref,omitempty"`
	ReasonCode string `json:"reason_code,omitempty"`
	// File is the commented file at ReviewedSHA, relative to the candidates
	// file's directory; FileSkipped says why there is no copy. The engine
	// fills both.
	File        string `json:"file,omitempty"`
	FileSkipped string `json:"file_skipped,omitempty"`

	// OutsideReason says why Build put the comment in Result.Outside
	// (store.MissOutside*); "" for a candidate. The classifier never sees
	// an outside comment, so it is not in the candidates file.
	OutsideReason string `json:"-"`
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
	// Caught counts comments near a finding magnum posted, or near one the
	// judge rejected as a duplicate of a comment made before that review
	// (reasonDuplicate), Dropped the comments too short or only an
	// approval. Neither is stored.
	Caught, Dropped int
	// Rejected are the findings the judge rejected, the newest RejectedMax
	// with a title, oldest first: the classifier names the one that
	// reports a candidate's problem (Item.Rejected, LinkRejected).
	Rejected []Rejected
	// Threads are magnum's own findings that someone answered
	// (OwnThreads), for the classifier beside the candidates.
	Threads []OwnThread
}

// Build makes the candidates of a pull request: the root comment of every
// review thread and the body of every review by another reviewer (not the
// author, not one of magnum's logins, not a bot unless IncludeBots).
// Replies are never candidates. A thread magnum started that someone
// answered is not a candidate either: it goes to Result.Threads
// (OwnThreads).
//
// A comment shorter than MinChars without its quotes and code blocks, or
// that only approves, is dropped. A comment made on a reviewed commit
// applies to it; an inline comment on another commit applies to the newest
// commit magnum reviewed before the comment when that commit descends from
// the reviewed one and the commented file did not change between the two
// (Compare), and is outside otherwise, as is a review body on a commit
// magnum did not review; an outside comment says why (OutsideReason). Only
// a failing Compare is an error; one GitHub answers with a 404
// (github.ErrNotFound: a commit it no longer has) proves nothing, so the
// comment is outside and the build goes on.
//
// A comment within NearLines of a finding magnum posted on the same path,
// at the reviewed commit the comment applies to (for an outside one, the
// review before it) or at one reviewed before that, is caught (postedNear):
// the finding stays open as a thread on the later heads, so the comment
// repeats magnum's own finding, whatever changed in the file. One near a
// rejected finding is raised (a comment on deleted lines has no line, so
// neither), until the classifier names another (Result.Rejected lists
// them); near here counts only the findings of the commit the comment
// applies to and of the earlier reviewed commits the file did not change
// since (rejectedNear), since a line number of another version of the file
// names other code, and an outside comment's lines hold at no reviewed
// commit, so it raises nothing. The exception is a finding rejected as a
// duplicate (reasonDuplicate: an existing thread already covers it) when
// the comment was made before the review that rejected it
// (rejectedAsDuplicate): the judge read the comment and agreed, so it is
// caught. A rejection for any other reason says the judge disagreed, and a
// comment made after the review was never read by it.
func Build(in Input) (Result, error) {
	res := Result{Rejected: rejectedRows(in.Findings), Threads: OwnThreads(in)}
	b := builder{in: in, reviewed: map[string]time.Time{}, compared: map[[2]string]compared{}}
	for _, r := range in.Reviewed {
		if at, ok := b.reviewed[r.SHA]; !ok || r.At.Before(at) {
			b.reviewed[r.SHA] = r.At
		}
	}
	postedAt := in.postedAt()

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
		base := in.reviewedBefore(cm.CreatedAt)
		why, err := b.place(&c, base)
		if err != nil {
			return Result{}, err
		}
		// A comment outside is measured against the review before it.
		if b.postedNear(c, cmp.Or(c.ReviewedSHA, base)) {
			res.Caught++
			continue
		}
		if why != "" {
			c.ReviewedSHA, c.OutsideReason = cmp.Or(base, c.CommentSHA, in.newest()), why
			res.Outside = append(res.Outside, c)
			continue
		}
		f, err := b.rejectedNear(c)
		if err != nil {
			return Result{}, err
		}
		if f != nil {
			if rejectedAsDuplicate(*f, cm.CreatedAt, postedAt) {
				res.Caught++
				continue
			}
			c.Raised, c.FindingRef, c.ReasonCode = store.MissRaisedRejected, findingRef(*f), f.ReasonCode
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
		if _, ok := b.reviewed[r.CommitOid]; ok && r.CommitOid != "" {
			c.ReviewedSHA = r.CommitOid
			res.Candidates = append(res.Candidates, c)
			continue
		}
		// A body has no file to compare: one after a review is on a commit
		// magnum did not review.
		base := in.reviewedBefore(r.SubmittedAt)
		c.ReviewedSHA, c.OutsideReason = cmp.Or(base, r.CommitOid, in.newest()), store.MissOutsideBodyUnreviewed
		if base == "" {
			c.OutsideReason = store.MissOutsideBeforeReview
		}
		res.Outside = append(res.Outside, c)
	}
	return res, nil
}

// builder is Build's state: the reviewed commits with when magnum reviewed
// each (the earliest when one was reviewed twice), and GitHub's
// comparisons, each asked once.
type builder struct {
	in       Input
	reviewed map[string]time.Time
	compared map[[2]string]compared
}

// compared is one comparison's answer; gone when GitHub could not make it
// (a 404).
type compared struct {
	Comparison
	gone bool
}

// changed says what GitHub's comparison of from with a later commit to
// shows of path: "" when to descends from from and path did not change
// between them, else why the code at to is not the code magnum saw at from
// (store.MissOutsideFileChanged, MissOutsideNotDescendant, or
// MissOutsideNoCompare when no comparison could tell: no Compare, a 404, a
// file list GitHub cut).
func (b *builder) changed(from, to, path string) (string, error) {
	if b.in.Compare == nil {
		return store.MissOutsideNoCompare, nil
	}
	key := [2]string{from, to}
	cmpd, ok := b.compared[key]
	if !ok {
		c, err := b.in.Compare(from, to)
		// GitHub cannot compare a commit it no longer has (a force push, a
		// deleted branch): a comparison that proves nothing.
		if err != nil && !errors.Is(err, github.ErrNotFound) {
			return "", fmt.Errorf("compare %s...%s: %w", short(from), short(to), err)
		}
		cmpd = compared{Comparison: c, gone: err != nil}
		b.compared[key] = cmpd
	}
	switch {
	case cmpd.gone:
		return store.MissOutsideNoCompare, nil
	case !cmpd.Descendant:
		return store.MissOutsideNotDescendant, nil
	case !cmpd.Complete:
		return store.MissOutsideNoCompare, nil
	case slices.Contains(cmpd.Paths, path):
		return store.MissOutsideFileChanged, nil
	}
	return "", nil
}

// place sets the reviewed commit inline comment c applies to: its own
// commit when magnum reviewed it, else base, the newest commit magnum
// reviewed before the comment, when the file there is as at the comment's
// commit (changed). It returns why c is outside instead ("" once placed):
// made before magnum's first review, on no commit, or what the comparison
// with base showed.
func (b *builder) place(c *Candidate, base string) (string, error) {
	if _, ok := b.reviewed[c.CommentSHA]; ok && c.CommentSHA != "" {
		c.ReviewedSHA = c.CommentSHA
		return "", nil
	}
	switch {
	case base == "":
		return store.MissOutsideBeforeReview, nil
	case c.CommentSHA == "":
		return store.MissOutsideNoCompare, nil
	}
	why, err := b.changed(base, c.CommentSHA, c.Path)
	if why == "" && err == nil {
		c.ReviewedSHA = base // the file is as magnum saw it, so the comment's lines hold there too
	}
	return why, err
}

// postedNear reports whether magnum posted a finding within NearLines of
// c's line range on c's path at ref, the reviewed commit the comment is
// measured against, or at a commit it reviewed before ref (its run's head
// in Targets), whatever changed in the file since: a posted finding stays
// open as a thread on the later heads, so a comment that repeats it there
// repeats magnum's own finding. A finding of a later review, or of a run
// Targets does not name, catches nothing, and nothing does when ref is no
// reviewed commit (a comment made before magnum's first review).
func (b *builder) postedNear(c Candidate, ref string) bool {
	at, ok := b.reviewed[ref]
	if !ok || ref == "" {
		return false
	}
	for _, f := range nearby(b.in.Findings, store.FindingPosted, c) {
		head := b.in.Targets[f.RunID]
		if headAt, ok := b.reviewed[head]; ok && head != "" && !headAt.After(at) {
			return true
		}
	}
	return false
}

// rejectedNear is the finding the judge rejected nearest to c's line range
// on c's path, within NearLines, at the commit c applies to (c.ReviewedSHA,
// its run's head in Targets) or at an earlier reviewed commit the file did
// not change since (changed), where its line numbers name the same code;
// nil when none. A finding of a later review, or of a run Targets does not
// name, is near nothing. It compares only for the findings near by line,
// the nearest first, until one holds.
func (b *builder) rejectedNear(c Candidate) (*store.Finding, error) {
	at, ok := b.reviewed[c.ReviewedSHA]
	if !ok {
		return nil, nil
	}
	for _, f := range nearby(b.in.Findings, store.FindingRejected, c) {
		head := b.in.Targets[f.RunID]
		if head != "" && head == c.ReviewedSHA {
			return f, nil
		}
		if headAt, ok := b.reviewed[head]; !ok || head == "" || !headAt.Before(at) {
			continue
		}
		why, err := b.changed(head, c.ReviewedSHA, c.Path)
		if err != nil {
			return nil, err
		}
		if why == "" {
			return f, nil
		}
	}
	return nil, nil
}

// other reports whether login (Account form) is another reviewer: not the
// author, not magnum's, not a ghost, and not a bot unless IncludeBots.
func (in Input) other(login, typename string) bool {
	if login == "" || github.SameAccount(login, in.Author) {
		return false
	}
	if in.own(login) {
		return false
	}
	return in.IncludeBots || !github.IsBot(typename, login)
}

// reasonDuplicate is the reason code of a finding the judge dropped because
// an earlier review, an existing thread or another reviewer's comment
// already covers it (the review skill's reason codes, as pipeline records
// them). It is the one rejection that agrees with a comment.
const reasonDuplicate = "duplicate"

// postedAt is when each review of magnum's was posted, by the id of the run
// whose findings are the review's (ReviewRuns, Reviews): the time a
// finding's judge had read the pull request's threads by. A run whose
// review GitHub does not list, or lists without a time (pending), is not
// in it.
func (in Input) postedAt() map[string]time.Time {
	at := map[string]time.Time{}
	for _, r := range in.Reviews {
		if run := in.ReviewRuns[r.DatabaseID]; run != "" && !r.SubmittedAt.IsZero() {
			at[run] = r.SubmittedAt
		}
	}
	return at
}

// rejectedAsDuplicate reports whether f, a rejected finding near a comment
// made at commentAt, says the comment already covered the problem: the
// judge dropped f as a duplicate (reasonDuplicate) in a review posted
// after the comment (postedAt, by f's run), so it read the comment and
// agreed. Any other reason means the judge disagreed or dismissed the
// problem, a comment made at or after the review was never read by that
// judge, and a review of unknown time proves nothing, so none of them is
// caught.
func rejectedAsDuplicate(f store.Finding, commentAt time.Time, postedAt map[string]time.Time) bool {
	at, ok := postedAt[f.RunID]
	return f.ReasonCode == reasonDuplicate && ok && commentAt.Before(at)
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
// range, within NearLines, by line alone (nil when none): for findings of
// one run (OwnThreads). Review bodies and file comments have no line, so
// nothing is near them.
func near(fs []store.Finding, verdict string, c Candidate) *store.Finding {
	if all := nearby(fs, verdict, c); len(all) > 0 {
		return all[0]
	}
	return nil
}

// nearby are the findings of verdict on c's path within NearLines of c's
// range, by line alone, the nearest first (in fs's order for the same
// distance).
func nearby(fs []store.Finding, verdict string, c Candidate) []*store.Finding {
	if c.Path == "" || c.Line <= 0 {
		return nil
	}
	from := max(c.StartLine, 1)
	if c.StartLine == 0 {
		from = c.Line
	}
	type hit struct {
		f *store.Finding
		d int
	}
	var hits []hit
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
		if d <= NearLines {
			hits = append(hits, hit{f, d})
		}
	}
	slices.SortStableFunc(hits, func(a, b hit) int { return a.d - b.d })
	out := make([]*store.Finding, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.f)
	}
	return out
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
