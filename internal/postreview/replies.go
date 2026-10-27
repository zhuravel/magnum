package postreview

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/textx"
)

// Reply kinds (Reply.Kind).
const (
	ReplyAck      = "ack"      // a reason the judge accepts: one short acknowledgement
	ReplyRebuttal = "rebuttal" // the judge keeps the finding: one sentence with evidence
	ReplyAnswer   = "answer"   // an answer to a question
)

// StatusReplied: the replies are posted, or were all posted already.
const StatusReplied = "replied"

// maxRebuttals is how many times magnum argues in one thread: after that
// RunReplies refuses any reply there and the operator takes over.
const maxRebuttals = 2

// anyMarker starts every marker magnum writes; a judge's reply never
// carries one of its own (RunReplies appends the reply marker).
const anyMarker = "<!-- magnum:"

// replyMarkerRe reads a reply marker: its run id (no spaces) and kind.
var replyMarkerRe = regexp.MustCompile(`<!-- magnum:reply run=(\S+) kind=(\S+) -->`)

// ReplyMarker is the line every reply of run carries:
// `<!-- magnum:reply run=<run id> kind=<kind> -->`. The daemon finds a
// round's replies on GitHub by it, and counts magnum's rebuttals per thread.
func ReplyMarker(runID, kind string) string {
	return fmt.Sprintf("<!-- magnum:reply run=%s kind=%s -->", runID, kind)
}

// FixReplyMarker ends every reply the fixer posts (skills/magnum-fix/SKILL.md,
// section 6). The fixer posts as the PR's author, the operator, whose login
// a gh identity shares, so the retro (learn.OwnThreads) tells its answers
// from the author's by this marker alone.
const FixReplyMarker = "<!-- magnum:fix -->"

// IsFixReply reports whether body carries FixReplyMarker.
func IsFixReply(body string) bool { return strings.Contains(body, FixReplyMarker) }

// ParseReplyMarker reads the reply marker in body: its run id and kind; ok
// is false when body has none.
func ParseReplyMarker(body string) (runID, kind string, ok bool) {
	m := replyMarkerRe.FindStringSubmatch(body)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// RepliesFile is the file the judge writes for --replies.
type RepliesFile struct {
	Replies []Reply `json:"replies"`
}

// Reply is one answer in a review thread.
type Reply struct {
	CommentID int64  `json:"comment_id"` // the thread's first comment (threads_file's comment_id)
	Kind      string `json:"kind"`
	Body      string `json:"body"`
}

// PostedReply is a reply RunReplies posted, found already posted by this
// run, or (dry run) planned.
type PostedReply struct {
	CommentID int64  `json:"comment_id"`
	ID        int64  `json:"id,omitempty"`
	URL       string `json:"url,omitempty"`
	Kind      string `json:"kind"`
	Already   bool   `json:"already,omitempty"` // posted by an earlier run of the tool for this run id: not posted again
}

// SkippedReply is a reply RunReplies did not post because its thread is not
// the reviewer's: another reviewer or the author started it, so it is theirs
// and never answered inline (DECISIONS "An author's own inline threads are
// not replies").
type SkippedReply struct {
	CommentID int64  `json:"comment_id"`
	Kind      string `json:"kind"`
	Reason    string `json:"reason"` // who started the thread (no text)
}

// replyBody is what is posted for r: its text, a blank line and the run's
// reply marker.
func replyBody(runID string, r Reply) string {
	return strings.TrimRight(r.Body, " \t\r\n") + "\n\n" + ReplyMarker(runID, r.Kind)
}

// parseReplies decodes and checks the judge's replies file for the run o
// names, or returns the problems found, each naming its reply.
func parseReplies(data []byte, o Options) (RepliesFile, []string) {
	var f RepliesFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return RepliesFile{}, []string{"the file is not a replies object (replies): " + err.Error()}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return RepliesFile{}, []string{"the file holds more than one JSON value"}
	}
	if f.Replies == nil {
		return RepliesFile{}, []string{"replies is missing: it is an array, empty when there is nothing to answer"}
	}
	var problems []string
	first := map[int64]int{} // comment_id → the reply that named it first
	for i, r := range f.Replies {
		bad := func(format string, args ...any) {
			problems = append(problems, fmt.Sprintf("replies[%d]: ", i)+fmt.Sprintf(format, args...))
		}
		switch j, seen := first[r.CommentID]; {
		case r.CommentID < 1:
			bad("comment_id %d is not a comment id (1 or more)", r.CommentID)
		case seen:
			bad("comment_id %d repeats replies[%d]: one reply per thread", r.CommentID, j)
		default:
			first[r.CommentID] = i
		}
		switch r.Kind {
		case ReplyAck, ReplyRebuttal, ReplyAnswer:
		default:
			bad("kind %q is not %s, %s or %s", r.Kind, ReplyAck, ReplyRebuttal, ReplyAnswer)
		}
		switch {
		case strings.TrimSpace(r.Body) == "":
			bad("body is empty")
		case strings.Contains(r.Body, anyMarker):
			bad("body carries a %s… marker: magnum appends the reply marker, leave it out", anyMarker)
		case localPath(r.Body) != "":
			bad("%s", localPathProblem(r.Body))
		default:
			if n := utf8.RuneCountInString(replyBody(o.RunID, r)); n > MaxBody {
				bad("body is %d characters (the reply marker included); GitHub takes at most %d", n, MaxBody)
			}
		}
	}
	if len(problems) > 0 {
		return RepliesFile{}, problems
	}
	return f, nil
}

// RunReplies posts the judge's replies file data for the run o names (o's
// head is only checked for form: a reply answers a thread, whatever its
// commit). It checks the file, reads the review threads once, and checks
// that each reply answers the first comment of a thread on the pull request
// and that magnum has not already rebutted twice there; a reply to a thread
// someone other than the reviewer (or a former login) started is skipped
// (Outcome.Skipped), not refused; a thread that already holds a reply
// carrying the run's marker gets nothing more, so the tool is safe to run
// again after a failure. Then it posts the other replies in file order (or,
// in a dry run, plans them) and stops at the first failure: replied even
// when it posted nothing.
func RunReplies(ctx context.Context, d Deps, o Options, data []byte) Outcome {
	if err := o.Check(); err != nil {
		return Outcome{Status: StatusError, Message: err.Error()}
	}
	if o.LocalBase != "" {
		return Outcome{Status: StatusError, Message: "a local base is for a review's dry run only"}
	}
	f, problems := parseReplies(data, o)
	if len(problems) > 0 {
		return Outcome{Status: StatusInvalid, Problems: problems}
	}
	if len(f.Replies) == 0 {
		return Outcome{Status: StatusReplied}
	}
	threads, err := d.GitHub.ReviewThreads(ctx, o.Owner, o.Repo, o.Number)
	if err != nil {
		return Outcome{Status: StatusError, Message: "read the review threads: " + err.Error()}
	}
	r := &run{d: d, o: o}
	plan, skip, problems := r.planReplies(f.Replies, threads)
	if len(problems) > 0 {
		return Outcome{Status: StatusInvalid, Problems: problems}
	}
	out := Outcome{Status: StatusReplied}
	if o.DryRun {
		out.Status = StatusDryRun
	}
	for i, rp := range f.Replies {
		if skip[i] != "" {
			out.Skipped = append(out.Skipped, SkippedReply{CommentID: rp.CommentID, Kind: rp.Kind, Reason: skip[i]})
		}
	}
	for i, rp := range f.Replies {
		if skip[i] != "" {
			continue
		}
		if plan[i] != nil {
			out.Replies = append(out.Replies, *plan[i])
			continue
		}
		if o.DryRun {
			out.Replies = append(out.Replies, PostedReply{CommentID: rp.CommentID, Kind: rp.Kind})
			continue
		}
		posted, err := d.GitHub.ReplyToReviewComment(ctx, o.Owner, o.Repo, o.Number, rp.CommentID, replyBody(o.RunID, rp))
		if err != nil {
			out.Status, out.Message = StatusError, fmt.Sprintf("post the reply to comment %d: %v", rp.CommentID, err)
			return out
		}
		out.Replies = append(out.Replies, PostedReply{CommentID: rp.CommentID, ID: posted.ID, URL: posted.URL, Kind: rp.Kind})
	}
	return out
}

// planReplies checks each reply against threads. plan[i] is non-nil for a
// reply already posted by this run (Already); skip[i] says why a reply to a
// thread the reviewer did not start is skipped; problems lists the replies
// that cannot be posted at all.
func (r *run) planReplies(replies []Reply, threads []github.Thread) (plan []*PostedReply, skip, problems []string) {
	byFirst := make(map[int64]github.Thread, len(threads))
	for _, t := range threads {
		if len(t.Comments) > 0 {
			byFirst[t.Comments[0].ID] = t
		}
	}
	plan, skip = make([]*PostedReply, len(replies)), make([]string, len(replies))
	for i, rp := range replies {
		bad := func(format string, args ...any) {
			problems = append(problems, fmt.Sprintf("replies[%d]: ", i)+fmt.Sprintf(format, args...))
		}
		t, ok := byFirst[rp.CommentID]
		if !ok {
			bad("comment %d is no thread's first comment on this pull request", rp.CommentID)
			continue
		}
		if first := t.Comments[0]; !r.ownComment(first) {
			skip[i] = fmt.Sprintf("%s started this thread, not %s: theirs, never answered inline",
				cmp.Or(github.Account(first.AuthorLogin, first.AuthorType), "a deleted account"), r.o.Login)
			continue
		}
		rebuttals := 0
		for _, c := range t.Comments[1:] {
			if !r.ownComment(c) {
				continue
			}
			runID, kind, marked := ParseReplyMarker(c.Body)
			if marked && runID == r.o.RunID && plan[i] == nil {
				plan[i] = &PostedReply{CommentID: rp.CommentID, ID: c.ID, URL: c.URL, Kind: kind, Already: true}
			}
			// Magnum's rebuttals before the reply marker existed carry none.
			if !marked || kind == ReplyRebuttal {
				rebuttals++
			}
		}
		if plan[i] == nil && rebuttals >= maxRebuttals {
			bad("you already rebutted twice in this thread; magnum stops arguing there and asks the operator")
		}
	}
	return plan, skip, problems
}

// ownComment reports whether c was written by the reviewer login or a
// former one.
func (r *run) ownComment(c github.ThreadComment) bool {
	return r.ownLogin(github.Account(c.AuthorLogin, c.AuthorType))
}

// repliesSummary is the one line of a replies outcome (counts, no text).
func (o Outcome) repliesSummary() string {
	var posted, already int
	byKind := map[string]int{}
	for _, rp := range o.Replies {
		if rp.Already {
			already++
			continue
		}
		posted++
		byKind[rp.Kind]++
	}
	var kinds []string
	for _, k := range []struct{ kind, one, many string }{
		{ReplyAck, "ack", "acks"}, {ReplyRebuttal, "rebuttal", "rebuttals"}, {ReplyAnswer, "answer", "answers"},
	} {
		if n := byKind[k.kind]; n > 0 {
			kinds = append(kinds, textx.Count(n, k.one, k.many))
		}
	}
	detail := ""
	if len(kinds) > 0 {
		detail = " (" + strings.Join(kinds, ", ") + ")"
	}
	tail := ""
	if already > 0 {
		tail = fmt.Sprintf("; %d already posted", already)
	}
	skipped := ""
	if n := len(o.Skipped); n > 0 {
		skipped = "; skipped " + textx.Count(n, "reply to a thread", "replies to threads") + " someone else started"
	}
	replies := textx.Count(posted, "reply", "replies")
	switch {
	case o.Status == StatusDryRun:
		return fmt.Sprintf("dry run: nothing posted; %s planned%s%s%s", replies, detail, tail, skipped)
	case len(o.Replies) == 0 && skipped != "":
		return "nothing posted" + skipped
	case len(o.Replies) == 0:
		return "no replies to post"
	case posted == 0:
		return fmt.Sprintf("%s already posted; nothing posted%s", textx.Count(already, "reply is", "replies are"), skipped)
	}
	return fmt.Sprintf("posted %s%s%s%s", replies, detail, tail, skipped)
}

// FixReplyOptions name what `magnum fix-reply` answers: the pull request
// (the fixer's pane's MAGNUM_PR_URL) and either a review comment, whose
// thread the reply goes into (CommentID: its first comment or any reply in
// it), or the pull request's conversation (Issue).
type FixReplyOptions struct {
	Owner, Repo string
	Number      int
	CommentID   int64
	Issue       bool
}

// Check reports what is wrong with o.
func (o FixReplyOptions) Check() error {
	switch {
	case !ownerRe.MatchString(o.Owner) || !repoRe.MatchString(o.Repo) || o.Repo == "." || o.Repo == "..":
		return fmt.Errorf("the repository %q is not owner/name", o.Owner+"/"+o.Repo)
	case o.Number < 1:
		return fmt.Errorf("the pull request number %d is not 1 or more", o.Number)
	case o.CommentID < 0:
		return fmt.Errorf("the comment id %d is not 1 or more", o.CommentID)
	case o.Issue == (o.CommentID != 0):
		return errors.New("give exactly one of --comment <id> and --issue")
	}
	return nil
}

// FixReplyGitHub is what RunFixReply reads and writes on GitHub
// (*github.Client, as the fixer's gh login: the PR's author).
type FixReplyGitHub interface {
	AllReviewThreads(ctx context.Context, owner, repo string, number int) ([]github.Thread, bool, error)
	ReplyToReviewComment(ctx context.Context, owner, repo string, number int, commentID int64, body string) (github.ReviewCommentReply, error)
	IssueComments(ctx context.Context, owner, repo string, number int, stop func(github.PRComment) bool) ([]github.PRComment, bool, error)
	CreateIssueComment(ctx context.Context, owner, repo string, number int, body string) (github.IssueComment, error)
}

// FixReplyOutcome is what RunFixReply printed: a status (StatusReplied,
// StatusAlreadyPosted, StatusInvalid or StatusError) and its details.
type FixReplyOutcome struct {
	Status   string   `json:"status"`
	Message  string   `json:"message,omitempty"`  // what went wrong (error)
	Problems []string `json:"problems,omitempty"` // why the reply was refused (invalid)
	// Thread is the first comment of the thread the reply went into, 0 for
	// a comment on the conversation; ID and URL are the reply's, posted or
	// found already there.
	Thread int64  `json:"thread,omitempty"`
	ID     int64  `json:"id,omitempty"`
	URL    string `json:"url,omitempty"`
}

// ExitCode is 0 for a reply posted or already there, 2 when the fixer must
// fix its reply (or the comment it names), 1 otherwise.
func (o FixReplyOutcome) ExitCode() int {
	switch o.Status {
	case StatusReplied, StatusAlreadyPosted:
		return 0
	case StatusInvalid:
		return 2
	}
	return 1
}

// Summary is the outcome in one line (ids and links, no reply text).
func (o FixReplyOutcome) Summary() string {
	where := "as a comment on the pull request"
	if o.Thread != 0 {
		where = fmt.Sprintf("in the thread of comment %d", o.Thread)
	}
	switch o.Status {
	case StatusReplied:
		return fmt.Sprintf("posted the reply %s: %s", where, o.URL)
	case StatusAlreadyPosted:
		if o.Thread == 0 {
			where = "a comment on the pull request"
		}
		return fmt.Sprintf("the reply is already %s; nothing posted: %s", where, o.URL)
	case StatusInvalid:
		return fmt.Sprintf("the reply has %s; nothing posted: %s", textx.Count(len(o.Problems), "problem", "problems"), strings.Join(o.Problems, "; "))
	}
	return o.Message
}

// fixReplyBody is what the fixer's reply text becomes on GitHub: the text, a
// blank line and FixReplyMarker.
func fixReplyBody(text string) string {
	return strings.TrimRight(text, " \t\r\n") + "\n\n" + FixReplyMarker
}

// sameFixReply reports whether a comment's body on GitHub is body, the
// marked reply about to be posted, but for its line ends and the blanks
// around it.
func sameFixReply(onGitHub, body string) bool {
	norm := func(s string) string { return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n")) }
	return norm(onGitHub) == norm(body)
}

// postedLast is the index among n comments, oldest first (at(i) gives the
// body and author, Account form, of comment i), of body posted already by a
// retry's earlier run: the newest comment that is body, when nobody else
// wrote after it (only its own author's comments follow, the fixer's other
// replies); -1 when there is none. The same answer after someone else's
// comment is a new reply, to new pushback, and goes out.
func postedLast(n int, at func(int) (body, author string), body string) int {
	var after []string // the authors of the comments after i
	for i := n - 1; i >= 0; i-- {
		b, a := at(i)
		if sameFixReply(b, body) {
			if slices.ContainsFunc(after, func(x string) bool { return !github.SameAccount(x, a) }) {
				return -1
			}
			return i
		}
		after = append(after, a)
	}
	return -1
}

// checkFixReply is what is wrong with text, the fixer's reply (nil: it can
// go out once marked).
func checkFixReply(text string) []string {
	switch {
	case strings.TrimSpace(text) == "":
		return []string{"the reply is empty: pass it on standard input"}
	case strings.Contains(text, anyMarker):
		return []string{fmt.Sprintf("the reply carries a %s… marker: fix-reply appends the fix marker, leave it out", anyMarker)}
	case localPath(text) != "":
		return []string{strings.Replace(localPathProblem(text), "body", "the reply", 1)}
	}
	if n := utf8.RuneCountInString(fixReplyBody(text)); n > MaxBody {
		return []string{fmt.Sprintf("the reply is %d characters (the fix marker included); GitHub takes at most %d", n, MaxBody)}
	}
	return nil
}

// RunFixReply posts text, one reply of the fixer, on the pull request o
// names, as gh's login, which is the PR's author: the text, a blank line
// and FixReplyMarker, by which magnum tells the fixer's replies from the
// author's own (learn.OwnThreads, the fix job's external file). It refuses
// a reply that is empty, carries a magnum marker of its own, names a local
// path or is too long before it reads GitHub. With a comment id it reads
// the review threads once, refuses a comment that is in none of them (one
// of another pull request, or of none) and answers its thread's first
// comment, the only one GitHub takes a reply to; with Issue it writes a
// comment on the conversation. A thread, or the conversation, that already
// holds the same marked reply with nobody else's comment after it gets
// nothing more (StatusAlreadyPosted, postedLast), so a retry after an
// unclear failure posts the reply once, while the same answer to new
// pushback goes out.
func RunFixReply(ctx context.Context, gh FixReplyGitHub, o FixReplyOptions, text []byte) FixReplyOutcome {
	if err := o.Check(); err != nil {
		return FixReplyOutcome{Status: StatusError, Message: err.Error()}
	}
	if problems := checkFixReply(string(text)); len(problems) > 0 {
		return FixReplyOutcome{Status: StatusInvalid, Problems: problems}
	}
	body := fixReplyBody(string(text))
	same := func(c github.PRComment) bool { return sameFixReply(c.Body, body) }
	if o.Issue {
		// Read from the newest back until a page holds the reply.
		comments, _, err := gh.IssueComments(ctx, o.Owner, o.Repo, o.Number, same)
		if err != nil {
			return FixReplyOutcome{Status: StatusError, Message: "read the pull request's comments: " + err.Error()}
		}
		if i := postedLast(len(comments), func(i int) (string, string) {
			return comments[i].Body, github.Account(comments[i].AuthorLogin, comments[i].AuthorType)
		}, body); i >= 0 {
			return FixReplyOutcome{Status: StatusAlreadyPosted, ID: comments[i].ID, URL: comments[i].URL}
		}
		posted, err := gh.CreateIssueComment(ctx, o.Owner, o.Repo, o.Number, body)
		if err != nil {
			return FixReplyOutcome{Status: StatusError, Message: "post the comment: " + err.Error()}
		}
		return FixReplyOutcome{Status: StatusReplied, ID: posted.ID, URL: posted.URL}
	}
	threads, complete, err := gh.AllReviewThreads(ctx, o.Owner, o.Repo, o.Number)
	if err != nil {
		return FixReplyOutcome{Status: StatusError, Message: "read the review threads: " + err.Error()}
	}
	i := slices.IndexFunc(threads, func(t github.Thread) bool {
		return slices.ContainsFunc(t.AllComments(), func(c github.ThreadComment) bool { return c.ID == o.CommentID })
	})
	if i < 0 {
		problem := fmt.Sprintf("comment %d is no review comment of %s/%s#%d: answer a thread of this pull request", o.CommentID, o.Owner, o.Repo, o.Number)
		if !complete {
			problem = fmt.Sprintf("comment %d is in none of the first %d review threads of %s/%s#%d", o.CommentID, github.MaxReviewThreads, o.Owner, o.Repo, o.Number)
		}
		return FixReplyOutcome{Status: StatusInvalid, Problems: []string{problem}}
	}
	all := threads[i].AllComments()
	first, replies := all[0].ID, all[1:]
	if j := postedLast(len(replies), func(j int) (string, string) {
		return replies[j].Body, github.Account(replies[j].AuthorLogin, replies[j].AuthorType)
	}, body); j >= 0 {
		return FixReplyOutcome{Status: StatusAlreadyPosted, Thread: first, ID: replies[j].ID, URL: replies[j].URL}
	}
	posted, err := gh.ReplyToReviewComment(ctx, o.Owner, o.Repo, o.Number, first, body)
	if err != nil {
		return FixReplyOutcome{Status: StatusError, Thread: first, Message: fmt.Sprintf("post the reply to comment %d: %v", first, err)}
	}
	return FixReplyOutcome{Status: StatusReplied, Thread: first, ID: posted.ID, URL: posted.URL}
}
