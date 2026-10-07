package postreview

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/textx"
)

// GitHub is what Run reads and writes on GitHub (*github.Client, as the
// judge's identity).
type GitHub interface {
	PullSHAs(ctx context.Context, owner, repo string, number int) (base, head string, err error)
	PullFiles(ctx context.Context, owner, repo string, number int) ([]github.FileDelta, bool, error)
	CompareFiles(ctx context.Context, owner, repo, base, head string) ([]github.FileDelta, error)
	// AllReviews (and whether the list is complete) is the post-once
	// guard's: a review carrying the run's marker is the review.
	AllReviews(ctx context.Context, owner, repo string, number int) ([]github.Review, bool, error)
	SubmitReview(ctx context.Context, owner, repo string, number int, r github.ReviewRequest) (github.RESTReview, error)
	ReviewREST(ctx context.Context, owner, repo string, number int, id int64) (github.RESTReview, error)
	ReviewComments(ctx context.Context, owner, repo string, number int, reviewID int64) ([]github.ReviewComment, error)
	// ReviewThreads and ReplyToReviewComment are RunReplies's.
	ReviewThreads(ctx context.Context, owner, repo string, number int) ([]github.Thread, error)
	ReplyToReviewComment(ctx context.Context, owner, repo string, number int, commentID int64, body string) (github.ReviewCommentReply, error)
}

// Git is what Run reads from the checkout (*gitx.Client).
type Git interface {
	RevParse(ctx context.Context, dir, ref string) (string, error)
	MergeBase(ctx context.Context, dir, a, b string) (string, error)
	FileDiff(ctx context.Context, dir, base, head, path string) (string, error)
}

// Deps are Run's GitHub, as the judge's identity, and git in Dir, the
// checkout ("." = the current directory).
type Deps struct {
	GitHub GitHub
	Git    Git
	Dir    string
}

// Options are the run's facts, from the judge's <magnum> block.
type Options struct {
	Owner, Repo string
	Number      int
	HeadSHA     string // the reviewed commit: the review's commit_id
	RunID       string // the round's marker id
	Login       string // reviewer_login, REST form ("talkable[bot]")
	// FormerLogins posted this PR's reviews before its identity migrated: a
	// review of theirs carrying the marker counts as posted too.
	FormerLogins []string
	DryRun       bool // check everything, post nothing
	// LocalBase (a blind replay, with DryRun): check the anchors against
	// `git diff LocalBase HeadSHA` in the checkout and read nothing from
	// GitHub, whose pull request may have moved on since HeadSHA.
	LocalBase string
}

var (
	ownerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
	repoRe  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	shaRe   = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Check reports what is wrong with o.
func (o Options) Check() error {
	switch {
	case !ownerRe.MatchString(o.Owner) || !repoRe.MatchString(o.Repo) || o.Repo == "." || o.Repo == "..":
		return fmt.Errorf("the repository %q is not owner/name", o.Owner+"/"+o.Repo)
	case o.Number < 1:
		return fmt.Errorf("the pull request number %d is not 1 or more", o.Number)
	case !shaRe.MatchString(o.HeadSHA):
		return fmt.Errorf("the head %q is not a full commit id", o.HeadSHA)
	case strings.TrimSpace(o.RunID) == "" || strings.ContainsAny(o.RunID, " \t\r\n"):
		return fmt.Errorf("the run id %q is empty or has spaces", o.RunID)
	case strings.TrimSpace(o.Login) == "":
		return errors.New("the reviewer login is empty")
	case o.LocalBase != "" && !shaRe.MatchString(o.LocalBase):
		return fmt.Errorf("the local base %q is not a full commit id", o.LocalBase)
	case o.LocalBase != "" && !o.DryRun:
		return errors.New("a local base is for a dry run only")
	}
	return nil
}

// Outcome is what Run printed: a status and its details. ExitCode maps it
// to the command's exit status.
type Outcome struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"` // what went wrong (error, rejected), or what was found
	// Problems are the review or replies file's faults (invalid).
	Problems []string `json:"problems,omitempty"`
	// InvalidAnchors are the comments off the diff (invalid_anchors).
	InvalidAnchors []BadAnchor `json:"invalid_anchors,omitempty"`
	// Unchecked are the indexes of comments no diff could check (a binary
	// or too large file without its commits in the checkout): kept, GitHub
	// decides.
	Unchecked []int `json:"unchecked_anchors,omitempty"`
	// MarkerAppended: the body lacked the run marker; it was added as its
	// last line.
	MarkerAppended bool `json:"marker_appended,omitempty"`

	ReviewID  int64  `json:"review_id,omitempty"`
	ReviewURL string `json:"review_url,omitempty"`
	Event     string `json:"event,omitempty"` // the event posted (or, already posted, the review's)
	// EventDowngraded: GitHub refused a self-verdict, so the review went
	// out as COMMENT.
	EventDowngraded bool   `json:"event_downgraded,omitempty"`
	State           string `json:"state,omitempty"` // as read back
	CommitID        string `json:"commit_id,omitempty"`
	Comments        *int   `json:"comments,omitempty"` // inline comments read back (dry run: planned)
	// Mismatches say how the review read back differs (readback_mismatch).
	Mismatches []string `json:"mismatches,omitempty"`
	// PlannedReview is the request a dry run would have sent.
	PlannedReview *github.ReviewRequest `json:"planned_review,omitempty"`
	// Replies are the replies RunReplies posted (so far, after a failure),
	// found already posted, or planned in a dry run, in the file's order.
	Replies []PostedReply `json:"replies,omitempty"`
}

// ExitCode is 0 for posted, already posted, replied and a dry run, 2 when
// the judge must fix its file, 1 otherwise.
func (o Outcome) ExitCode() int {
	switch o.Status {
	case StatusPosted, StatusAlreadyPosted, StatusReplied, StatusDryRun:
		return 0
	case StatusInvalid, StatusInvalidAnchors:
		return 2
	}
	return 1
}

// Summary is the outcome in one human line (ids and counts, no PR text).
func (o Outcome) Summary() string {
	if o.Status == StatusReplied || o.Status == StatusDryRun && o.PlannedReview == nil {
		return o.repliesSummary()
	}
	n := 0
	if o.Comments != nil {
		n = *o.Comments
	}
	comments := textx.Count(n, "inline comment", "inline comments")
	switch o.Status {
	case StatusPosted:
		s := fmt.Sprintf("posted review %d (%s, %s): %s", o.ReviewID, o.Event, comments, o.ReviewURL)
		if o.EventDowngraded {
			s += " (GitHub refused a verdict on this PR, so it went out as COMMENT)"
		}
		return s
	case StatusAlreadyPosted:
		return fmt.Sprintf("review %d already carries this run's marker; nothing posted: %s", o.ReviewID, o.ReviewURL)
	case StatusDryRun:
		return fmt.Sprintf("dry run: nothing posted; the planned %s review has %s", o.Event, comments)
	case StatusInvalid:
		return fmt.Sprintf("the file has %s; nothing posted", textx.Count(len(o.Problems), "problem", "problems"))
	case StatusInvalidAnchors:
		return fmt.Sprintf("%s not on lines of the pull request's diff; nothing posted",
			textx.Count(len(o.InvalidAnchors), "inline comment is", "inline comments are"))
	case StatusReadbackMismatch:
		return fmt.Sprintf("review %d was posted but reads back wrong: %s", o.ReviewID, strings.Join(o.Mismatches, "; "))
	case StatusRejected:
		return "GitHub refused the review; nothing posted: " + o.Message
	}
	return o.Message
}

// Run posts the judge's review file data for the run o names: check the
// file, check its inline anchors against the pull request's diff, look for
// a review already carrying the run's marker, then post (or, in a dry run,
// plan) the review once and read it back.
func Run(ctx context.Context, d Deps, o Options, data []byte) Outcome {
	if err := o.Check(); err != nil {
		return Outcome{Status: StatusError, Message: err.Error()}
	}
	rv, appended, problems := Parse(data, o)
	if len(problems) > 0 {
		return Outcome{Status: StatusInvalid, Problems: problems}
	}
	r := &run{d: d, o: o}
	if r.d.Dir == "" {
		r.d.Dir = "."
	}
	bad, unchecked, err := r.checkAnchors(ctx, rv.Comments)
	if err != nil {
		return Outcome{Status: StatusError, Message: "read the pull request's diff: " + err.Error()}
	}
	if len(bad) > 0 {
		return Outcome{Status: StatusInvalidAnchors, InvalidAnchors: bad, Unchecked: unchecked, MarkerAppended: appended}
	}
	req := github.ReviewRequest{CommitID: o.HeadSHA, Event: rv.Event, Body: rv.Body, Comments: rv.Comments}
	out := r.post(ctx, req)
	out.Unchecked, out.MarkerAppended = unchecked, appended
	return out
}

// run is one Run's state.
type run struct {
	d Deps
	o Options

	baseSHA string // the PR's base.sha, once read
	// mergeBase is the PR's merge base in the checkout for the git
	// fallback; mbDone once looked for ("" = not available).
	mergeBase string
	mbDone    bool
}

// post looks for a review carrying the marker, then posts req (or plans it).
func (r *run) post(ctx context.Context, req github.ReviewRequest) Outcome {
	if r.o.LocalBase == "" {
		found, err := r.findPosted(ctx)
		if err != nil {
			return Outcome{Status: StatusError, Message: "list the reviews to look for the run marker: " + err.Error()}
		}
		if found != nil {
			return r.readBack(ctx, found.DatabaseID, found.URL, nil, false)
		}
	}
	if r.o.DryRun {
		n := len(req.Comments)
		return Outcome{Status: StatusDryRun, Event: req.Event, CommitID: req.CommitID, Comments: &n, PlannedReview: &req}
	}
	downgraded := false
	for {
		posted, err := r.d.GitHub.SubmitReview(ctx, r.o.Owner, r.o.Repo, r.o.Number, req)
		if err == nil {
			return r.readBack(ctx, posted.ID, posted.HTMLURL, &req, downgraded)
		}
		if !downgraded && req.Event != EventComment && selfVerdict(err) {
			req.Event, downgraded = EventComment, true
			continue
		}
		// Refused or unclear: a review carrying the marker counts as posted.
		found, lerr := r.findPosted(ctx)
		if lerr == nil && found != nil {
			return r.readBack(ctx, found.DatabaseID, found.URL, &req, downgraded)
		}
		if lerr == nil && refused(err) {
			return Outcome{Status: StatusRejected, Message: err.Error(), Event: req.Event, EventDowngraded: downgraded}
		}
		msg := "post the review: " + err.Error()
		if lerr != nil {
			msg += "; then listing the reviews to look for the run marker failed: " + lerr.Error()
		} else {
			msg += "; no review carries the run marker"
		}
		return Outcome{Status: StatusError, Message: msg, Event: req.Event, EventDowngraded: downgraded}
	}
}

// selfVerdict reports whether err is GitHub refusing an APPROVE or
// REQUEST_CHANGES on the identity's own pull request (422 "Can not approve
// your own pull request", "Can not request changes on your own pull
// request").
func selfVerdict(err error) bool {
	var apiErr *github.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 422 {
		return false
	}
	text := strings.ToLower(strings.Join(append([]string{apiErr.Message}, apiErr.Details...), " "))
	return strings.Contains(text, "your own pull request")
}

// refused reports whether GitHub answered err with a 4xx: the request was
// refused, so it created nothing.
func refused(err error) bool {
	var apiErr *github.APIError
	return errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500
}

// findPosted returns the first submitted review by the reviewer login (or a
// former login) carrying the run's marker, or nil. A list of the PR's
// reviews cut at the client's page limit without one is an error: the
// review may be among the ones left out, and posting would post it twice.
func (r *run) findPosted(ctx context.Context) (*github.Review, error) {
	reviews, complete, err := r.d.GitHub.AllReviews(ctx, r.o.Owner, r.o.Repo, r.o.Number)
	if err != nil {
		return nil, err
	}
	re := markerRe(r.o.RunID)
	for _, rv := range reviews {
		if strings.EqualFold(rv.State, "PENDING") || !re.MatchString(rv.Body) || !r.ownLogin(github.Account(rv.AuthorLogin, rv.AuthorType)) {
			continue
		}
		return &rv, nil
	}
	if !complete {
		return nil, fmt.Errorf("the PR has more reviews than the list holds (%d read) and none read carries the run marker: "+
			"the review may be among the ones left out, so nothing is posted", len(reviews))
	}
	return nil, nil
}

// ownLogin reports whether login (Account form) is the reviewer login or a
// former one.
func (r *run) ownLogin(login string) bool {
	return github.SameAccount(login, r.o.Login) ||
		slices.ContainsFunc(r.o.FormerLogins, func(f string) bool { return github.SameAccount(login, f) })
}

// reviewState is the state a submitted review of event reads back with.
func reviewState(event string) string {
	switch event {
	case EventApprove:
		return "APPROVED"
	case EventRequestChanges:
		return "CHANGES_REQUESTED"
	}
	return "COMMENTED"
}

// eventOf is the event a review state was posted with ("" when unknown).
func eventOf(state string) string {
	switch strings.ToUpper(state) {
	case "APPROVED":
		return EventApprove
	case "CHANGES_REQUESTED":
		return EventRequestChanges
	case "COMMENTED":
		return EventComment
	}
	return ""
}

// readBack reads review id and its comments back and compares them with
// what was sent (sent nil: the review was already posted, by an earlier
// run of the tool or by hand, so its state and comments are reported, not
// compared).
func (r *run) readBack(ctx context.Context, id int64, url string, sent *github.ReviewRequest, downgraded bool) Outcome {
	out := Outcome{Status: StatusPosted, ReviewID: id, ReviewURL: url, EventDowngraded: downgraded}
	if sent == nil {
		out.Status = StatusAlreadyPosted
	} else {
		out.Event = sent.Event
	}
	rest, err := r.d.GitHub.ReviewREST(ctx, r.o.Owner, r.o.Repo, r.o.Number, id)
	if err != nil {
		out.Status, out.Message = StatusError, fmt.Sprintf("read review %d back: %v", id, err)
		return out
	}
	comments, err := r.d.GitHub.ReviewComments(ctx, r.o.Owner, r.o.Repo, r.o.Number, id)
	if err != nil {
		out.Status, out.Message = StatusError, fmt.Sprintf("read the comments of review %d back: %v", id, err)
		return out
	}
	n := len(comments)
	out.ReviewURL, out.State, out.CommitID, out.Comments = cmp.Or(rest.HTMLURL, url), rest.State, rest.CommitID, &n
	if sent == nil {
		out.Event = eventOf(rest.State)
	}
	var bad []string
	if !r.ownLogin(rest.UserLogin) {
		bad = append(bad, fmt.Sprintf("its author is %q, not %q", rest.UserLogin, r.o.Login))
	}
	if !strings.EqualFold(rest.CommitID, r.o.HeadSHA) {
		bad = append(bad, fmt.Sprintf("its commit is %q, not the head %s", rest.CommitID, r.o.HeadSHA))
	}
	if !markerRe(r.o.RunID).MatchString(rest.Body) {
		bad = append(bad, "its body lacks the run marker")
	}
	if sent != nil {
		if want := reviewState(sent.Event); !strings.EqualFold(rest.State, want) {
			bad = append(bad, fmt.Sprintf("its state is %s, not %s for %s", rest.State, want, sent.Event))
		}
		if n != len(sent.Comments) {
			bad = append(bad, fmt.Sprintf("it has %d inline comments, %d were sent", n, len(sent.Comments)))
		}
	}
	if len(bad) > 0 {
		out.Status, out.Mismatches = StatusReadbackMismatch, bad
	}
	return out
}

// checkAnchors checks every comment's lines against the pull request's
// diff (or, with LocalBase, the checkout's). It returns the comments off
// the diff and the indexes of those nothing could check; err when the
// diff could not be read at all.
func (r *run) checkAnchors(ctx context.Context, comments []github.DraftComment) (bad []BadAnchor, unchecked []int, err error) {
	if len(comments) == 0 {
		return nil, nil, nil
	}
	diffs, err := r.diffs(ctx, comments)
	if err != nil {
		return nil, nil, err
	}
	for i, c := range comments {
		fd := diffs[c.Path]
		if fd == nil {
			unchecked = append(unchecked, i)
			continue
		}
		if why := fd.check(c); why != "" {
			bad = append(bad, BadAnchor{Index: i, Path: c.Path, Line: c.Line, StartLine: c.StartLine, Side: c.Side, Why: why, Valid: fd.valid(c.Side)})
		}
	}
	return bad, unchecked, nil
}

// diffs reads the diff of every path the comments name; a nil entry is a
// file no source could check.
func (r *run) diffs(ctx context.Context, comments []github.DraftComment) (map[string]*fileDiff, error) {
	out := map[string]*fileDiff{}
	if r.o.LocalBase != "" {
		for _, c := range comments {
			if _, done := out[c.Path]; !done {
				out[c.Path] = r.gitDiff(ctx, r.o.LocalBase, c.Path, true)
			}
		}
		return out, nil
	}
	files, complete, err := r.prFiles(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range comments {
		if _, done := out[c.Path]; done {
			continue
		}
		f, ok := files[c.Path]
		switch {
		case !ok && complete:
			out[c.Path] = &fileDiff{}
		case !ok:
			out[c.Path] = nil // past the listing's cap: GitHub decides
		case !f.Truncated:
			fd, _ := parsePatch(f.Patch) // no hunk: no line to comment on
			out[c.Path] = &fd
		default:
			out[c.Path] = r.gitDiff(ctx, r.prMergeBase(ctx), c.Path, false)
		}
	}
	return out, nil
}

// prFiles reads the pull request's files as GitHub diffs them for
// HeadSHA: pulls/{n}/files while the PR's head is HeadSHA, else (the PR
// moved on while the judge worked) the comparison of its base with
// HeadSHA. complete is false when the list may lack files.
func (r *run) prFiles(ctx context.Context) (map[string]github.FileDelta, bool, error) {
	base, head, err := r.d.GitHub.PullSHAs(ctx, r.o.Owner, r.o.Repo, r.o.Number)
	if err != nil {
		return nil, false, err
	}
	r.baseSHA = base
	var files []github.FileDelta
	complete := true
	if strings.EqualFold(head, r.o.HeadSHA) || base == "" {
		files, complete, err = r.d.GitHub.PullFiles(ctx, r.o.Owner, r.o.Repo, r.o.Number)
	} else {
		files, err = r.d.GitHub.CompareFiles(ctx, r.o.Owner, r.o.Repo, base, r.o.HeadSHA)
		complete = len(files) < github.CompareFileLimit
	}
	if err != nil {
		return nil, false, err
	}
	m := make(map[string]github.FileDelta, len(files))
	for _, f := range files {
		m[f.Path] = f
	}
	return m, complete, nil
}

// prMergeBase is the merge base of the PR's base.sha and HeadSHA in the
// checkout, "" when either commit is missing there.
func (r *run) prMergeBase(ctx context.Context) string {
	if r.mbDone {
		return r.mergeBase
	}
	r.mbDone = true
	if r.baseSHA == "" {
		return ""
	}
	for _, sha := range []string{r.baseSHA, r.o.HeadSHA} {
		if _, err := r.d.Git.RevParse(ctx, r.d.Dir, sha); err != nil {
			return ""
		}
	}
	mb, err := r.d.Git.MergeBase(ctx, r.d.Dir, r.baseSHA, r.o.HeadSHA)
	if err != nil {
		return ""
	}
	r.mergeBase = mb
	return mb
}

// gitDiff reads path's diff from base to HeadSHA in the checkout; nil when
// it cannot tell (no base, git failed, a binary file). An empty diff is a
// file the range does not change when unchanged counts (a local replay's
// whole diff), else nil: GitHub listed the file, so git's answer is the
// odd one and GitHub decides.
func (r *run) gitDiff(ctx context.Context, base, path string, unchanged bool) *fileDiff {
	if base == "" {
		return nil
	}
	patch, err := r.d.Git.FileDiff(ctx, r.d.Dir, base, r.o.HeadSHA, path)
	if err != nil {
		return nil
	}
	if strings.TrimSpace(patch) == "" {
		if unchanged {
			return &fileDiff{}
		}
		return nil
	}
	fd, ok := parsePatch(patch)
	if !ok {
		return nil
	}
	return &fd
}
