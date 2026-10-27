package engine

// The other reviewers' open items a fix job hands its fixer (spec 93). At
// the job's start the engine writes externalFile beside the fixer's report,
// in the review's directory: every unresolved review thread of the PR that
// another reviewer started, neither magnum's logins nor the PR author
// (people and bots alike: a colleague, another review tool, a code-review
// bot), each "answered" when its latest comment is the PR author's or the
// fixer's, and the review bodies and conversation comments of others since
// the PR author's last comment of their own, each "answered" when a later
// comment of the fixer names it on its target line (replyTarget).
// Only the repository's owners, members and collaborators and GitHub Apps
// count as reviewers here, as for a reply round (trustedReplier): in a
// public repository anyone may comment, and the fixer acts on these items
// as the author, so what an outsider writes is left out. The fixer handles
// the unanswered items after magnum's findings and replies once to each
// that asks for an answer, as the author, signed with the fix marker
// (skills/magnum-fix/SKILL.md); it never resolves or minimizes them. The
// bodies are PR text: they reach the fixer in the file, capped as the
// threads file caps a reply, and never the prompt, an event or a log. After
// the run, a fix the fixer left under Needs you as unanswered in one of
// these threads is looked at on GitHub (dropFixerAnswered): one it did
// reply in during the run asks nothing of the operator. prepareFix reads
// these items before every cycle starts (othersBeforeStart, spec 102), and
// the job writes that read: GitHub refusing it for the rate limit holds the
// job until the budget recovers, as the floor does, rather than start a
// fixer that never sees them (talkable#12054). A read that fails otherwise
// is tried once more at the job's start, and a second failure leaves the
// file out: the fixer then works on magnum's findings alone, and a
// fix.external event says so. A review of magnum's that found nothing and
// left nothing open starts a fix only when one of these items waits for an
// answer (spec 96): the fix is refused when none waits or the read fails
// (othersWaiting).

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/postreview"
	"github.com/zhuravel/magnum/internal/textx"
)

const (
	// externalFile is the file of the other reviewers' open items, beside
	// the fixer's report (FixPromptInput.ExternalFile).
	externalFile = "external-threads.json"
	// externalBodyMax bounds a body in the file, in characters, as the
	// threads file bounds a reply (pipeline's replyExcerptMax).
	externalBodyMax = 600
	// magnumMarker starts every hidden marker magnum writes on GitHub (a
	// review's run marker, the status comment's, the fixer's; postreview's
	// anyMarker).
	magnumMarker = "<!-- magnum:"
)

// externalItems is the file's content: the PR author (Account form), whom
// the fixer posts as, and magnum's logins (FixerLogins, Account form), the
// only two whose comments answer an item (the skill's look right before a
// reply checks them by name), the threads and the conversation.
type externalItems struct {
	Author       string           `json:"author"`
	FixerLogins  []string         `json:"fixer_logins"`
	Threads      []externalThread `json:"threads"`
	Conversation []externalRemark `json:"conversation"`
}

// externalThread is an unresolved review thread another reviewer started:
// the REST id and link of its first comment (a reply goes to that id),
// where it is (its line, else its original line), whether it is outdated
// and answered, and its comments, oldest first: in a long thread the first
// and the newest GitHub lists (github.Thread.AllComments), Omitted
// counting the ones between them that the file leaves out.
type externalThread struct {
	CommentID int64             `json:"comment_id"`
	URL       string            `json:"url"`
	Path      string            `json:"path"`
	Line      int               `json:"line,omitempty"`
	Outdated  bool              `json:"outdated"`
	Answered  bool              `json:"answered"`
	Comments  []externalComment `json:"comments"`
	Omitted   int               `json:"omitted,omitempty"`
}

// externalComment is one comment of an externalThread: its REST id and
// link, its author (Account form, "ghost" for none), its body (an
// excerpt), when it was made, and whether it carries the fixer's marker.
type externalComment struct {
	ID        int64  `json:"id"`
	URL       string `json:"url,omitempty"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	Truncated bool   `json:"truncated,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	Fix       bool   `json:"fix,omitempty"`
}

// externalRemark is a review body (Kind "review", with its State) or a
// conversation comment (Kind "comment") of another reviewer: its REST id,
// link, author, time and body (an excerpt), and whether the fixer answered
// it already (a later comment of its links it).
type externalRemark struct {
	Kind      string `json:"kind"`
	ID        int64  `json:"id"`
	URL       string `json:"url"`
	Author    string `json:"author"`
	State     string `json:"state,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	Body      string `json:"body"`
	Truncated bool   `json:"truncated,omitempty"`
	Answered  bool   `json:"answered"`
}

// Remark kinds (externalRemark.Kind), also the place a "## Needs you" entry
// names for one (needsYouEntryRe).
const (
	remarkReview  = "review"
	remarkComment = "comment"
)

// externalReader is what writeExternal reads beyond the engine's GitHub
// (*github.Client): the PR's review threads, saying whether GitHub listed
// them all, and its conversation comments.
type externalReader interface {
	AllReviewThreads(ctx context.Context, owner, repo string, number int) ([]github.Thread, bool, error)
	IssueComments(ctx context.Context, owner, repo string, number int, stop func(github.PRComment) bool) ([]github.PRComment, bool, error)
}

var _ externalReader = (*github.Client)(nil)

// unanswered counts the items of x that nobody answered: threads and
// conversation.
func (x externalItems) unanswered() (threads, conversation int) {
	for _, t := range x.Threads {
		if !t.Answered {
			threads++
		}
	}
	for _, r := range x.Conversation {
		if !r.Answered {
			conversation++
		}
	}
	return threads, conversation
}

// excerptOf is s trimmed and cut to externalBodyMax characters, and
// whether it was cut.
func excerptOf(s string) (string, bool) {
	s = strings.TrimSpace(s)
	c := textx.Clip(s, externalBodyMax)
	return c, c != s
}

// rfc3339 is t in UTC ("" for the zero time).
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// replyTarget is the item a fixer's issue comment answers: the line under
// its quote (skills/magnum-fix/SKILL.md, section 6), the first line that is
// neither blank nor quoted ("> …"), trimmed; "" when there is none. A link
// to another item in its quote or its reply is not its target.
func replyTarget(body string) string {
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ">") {
			continue
		}
		return line
	}
	return ""
}

// externalInput is what externalOf makes the file of: the PR's review
// threads, its reviews and its conversation comments as GitHub listed them
// (CommentsComplete false when the comments stop short of the oldest one
// that matters, github.Client.IssueComments), the PR author and magnum's
// logins (Account form; fixLogins).
type externalInput struct {
	Threads          []github.Thread
	Reviews          []github.Review
	Comments         []github.PRComment
	CommentsComplete bool
	Author           string
	Own              []string
}

// ownWord reports whether a review body or comment by the author is the
// author's own word: no magnum marker in it (the fixer's replies, a status
// comment or a review posted as the author's own login are magnum's).
func ownWord(body string) bool { return !strings.Contains(body, magnumMarker) }

// externalOf makes the file's content from in, and counts the items it
// left out because an outsider wrote them. Only a reviewer counts: a GitHub
// App, or an owner, member or collaborator of the repository
// (trustedReplier); the author and magnum's logins always do. A thread is
// another reviewer's when it is unresolved and a reviewer other than the
// author and magnum's logins started it; its comments are the reviewers'
// among all GitHub listed (its first and its newest:
// github.Thread.AllComments), and it is answered when the latest of them is
// the author's, the fixer's included, or carries the fixer's marker
// (postreview.IsFixReply) and comes from one of magnum's logins (the marker
// in anyone else's comment answers nothing). A review body or conversation
// comment is another reviewer's when a reviewer other than the author and
// magnum's logins wrote it, it says something (a review's body, not a
// pending or dismissed review) and it came after the author's last own
// word (ownWord) among the reviews and the comments, or, when the comments
// stop short of that word (CommentsComplete false), after the oldest comment
// GitHub listed; it is answered when a later comment of the author or of
// magnum's logins carrying the fixer's marker names it as its target
// (replyTarget). The conversation is oldest first.
func externalOf(in externalInput) (externalItems, int) {
	isOwn := func(login, typ string) bool {
		a := github.Account(login, typ)
		return slices.ContainsFunc(in.Own, func(o string) bool { return github.SameAccount(o, a) })
	}
	byAuthor := func(login, typ string) bool { return github.SameAccount(github.Account(login, typ), in.Author) }
	reviewer := func(login, typ, association string) bool {
		return github.IsBot(typ, login) || trustedReplier(association) || byAuthor(login, typ) || isOwn(login, typ)
	}
	name := func(login, typ string) string { return cmp.Or(github.Account(login, typ), "ghost") }

	x := externalItems{Author: in.Author, FixerLogins: append([]string{}, in.Own...), Threads: []externalThread{}, Conversation: []externalRemark{}}
	outside := 0
	for _, t := range in.Threads {
		all := t.AllComments()
		if t.Resolved || len(all) == 0 || isOwn(all[0].AuthorLogin, all[0].AuthorType) || byAuthor(all[0].AuthorLogin, all[0].AuthorType) {
			continue // resolved, or magnum's or the author's own thread
		}
		cs := slices.DeleteFunc(slices.Clone(all), func(c github.ThreadComment) bool {
			return !reviewer(c.AuthorLogin, c.AuthorType, c.AuthorAssociation)
		})
		outside += len(all) - len(cs)
		if len(cs) == 0 || cs[0].ID != all[0].ID {
			continue // an outsider started it
		}
		root, last := cs[0], cs[len(cs)-1]
		answered := byAuthor(last.AuthorLogin, last.AuthorType) || (postreview.IsFixReply(last.Body) && isOwn(last.AuthorLogin, last.AuthorType))
		et := externalThread{CommentID: root.ID, URL: root.URL, Path: t.Path, Line: cmp.Or(t.Line, t.OriginalLine), Outdated: t.Outdated,
			Answered: answered, Comments: []externalComment{}, Omitted: max(t.CommentCount-len(all), 0)}
		for _, c := range cs {
			ec := externalComment{ID: c.ID, URL: c.URL, Author: name(c.AuthorLogin, c.AuthorType), CreatedAt: rfc3339(c.CreatedAt), Fix: postreview.IsFixReply(c.Body)}
			ec.Body, ec.Truncated = excerptOf(c.Body)
			et.Comments = append(et.Comments, ec)
		}
		x.Threads = append(x.Threads, et)
	}

	var since time.Time // the author's last own word
	for _, r := range in.Reviews {
		if byAuthor(r.AuthorLogin, r.AuthorType) && strings.TrimSpace(r.Body) != "" && ownWord(r.Body) && r.SubmittedAt.After(since) {
			since = r.SubmittedAt
		}
	}
	for _, c := range in.Comments {
		if byAuthor(c.AuthorLogin, c.AuthorType) && ownWord(c.Body) && c.CreatedAt.After(since) {
			since = c.CreatedAt
		}
	}
	if !in.CommentsComplete && len(in.Comments) > 0 {
		// Older items are beyond what GitHub listed of the conversation.
		if start := in.Comments[0].CreatedAt.Add(-time.Nanosecond); start.After(since) {
			since = start
		}
	}
	type dated struct {
		at time.Time
		r  externalRemark
	}
	var items []dated
	// other reports whether a remark is another reviewer's, after the
	// author's last own word; an outsider's is counted and left out.
	other := func(login, typ, association string, at time.Time) bool {
		if byAuthor(login, typ) || isOwn(login, typ) || !at.After(since) {
			return false
		}
		if !reviewer(login, typ, association) {
			outside++
			return false
		}
		return true
	}
	for _, r := range in.Reviews {
		if strings.TrimSpace(r.Body) == "" || r.State == "PENDING" || r.State == "DISMISSED" || !other(r.AuthorLogin, r.AuthorType, r.AuthorAssociation, r.SubmittedAt) {
			continue
		}
		er := externalRemark{Kind: remarkReview, ID: r.DatabaseID, URL: r.URL, Author: name(r.AuthorLogin, r.AuthorType), State: r.State,
			CreatedAt: rfc3339(r.SubmittedAt)}
		er.Body, er.Truncated = excerptOf(r.Body)
		items = append(items, dated{r.SubmittedAt, er})
	}
	for _, c := range in.Comments {
		if strings.TrimSpace(c.Body) == "" || !other(c.AuthorLogin, c.AuthorType, c.AuthorAssociation, c.CreatedAt) {
			continue
		}
		er := externalRemark{Kind: remarkComment, ID: c.ID, URL: c.URL, Author: name(c.AuthorLogin, c.AuthorType), CreatedAt: rfc3339(c.CreatedAt)}
		er.Body, er.Truncated = excerptOf(c.Body)
		items = append(items, dated{c.CreatedAt, er})
	}
	slices.SortStableFunc(items, func(a, b dated) int { return a.at.Compare(b.at) })
	for _, it := range items {
		it.r.Answered = it.r.URL != "" && slices.ContainsFunc(in.Comments, func(c github.PRComment) bool {
			return c.CreatedAt.After(it.at) && postreview.IsFixReply(c.Body) && (byAuthor(c.AuthorLogin, c.AuthorType) || isOwn(c.AuthorLogin, c.AuthorType)) &&
				replyTarget(c.Body) == it.r.URL
		})
		x.Conversation = append(x.Conversation, it.r)
	}
	return x, outside
}

// fixLogins are the logins whose threads and replies on the job's PR are
// magnum's (Account form): ownLogins and the login that posted the review.
func (e *Engine) fixLogins(ctx context.Context, job *fixJob) []string {
	own := e.ownLogins(ctx, job.watch, job.pr)
	if job.login != "" && !slices.ContainsFunc(own, func(o string) bool { return github.SameAccount(o, job.login) }) {
		own = append(own, job.login)
	}
	return own
}

// fixReader is the job's GitHub client as its watch's poll identity, with
// the reads writeExternal needs (ok false when there is none).
func (e *Engine) fixReader(job *fixJob) (GitHub, externalReader, bool) {
	gh := e.gh(job.watch.PollIdentity)
	rd, ok := gh.(externalReader)
	return gh, rd, gh != nil && ok
}

// externalRead is readExternal's result: the file's content, the count of
// items an outsider wrote, what GitHub cut short (cut: "over 1000 review
// threads", "over 500 reviews", "over 1000 comments since the author's
// last"; none when every list is whole) and the PR head it was read at.
type externalRead struct {
	items   externalItems
	outside int
	cut     []string
	head    string
}

// readExternal reads the job's PR from GitHub and makes the file's content
// (externalOf), with the count of items an outsider wrote and which of the
// three lists GitHub cut short.
func (e *Engine) readExternal(ctx context.Context, job *fixJob) (externalRead, error) {
	ctx = github.WithOp(ctx, opFixExternal) // github_cost.go
	gh, rd, ok := e.fixReader(job)
	if !ok {
		return externalRead{}, fmt.Errorf("no GitHub client that reads review threads for poll identity %q", job.watch.PollIdentity)
	}
	owner, name, number := job.repo.Owner, job.repo.Name, job.pr.Number
	threads, threadsComplete, err := rd.AllReviewThreads(ctx, owner, name, number)
	if err != nil {
		return externalRead{}, err
	}
	reviews, reviewsComplete, err := gh.AllReviews(ctx, owner, name, number)
	if err != nil {
		return externalRead{}, err
	}
	author := github.Account(deref(job.pr.AuthorLogin), deref(job.pr.AuthorType))
	comments, complete, err := rd.IssueComments(ctx, owner, name, number, func(c github.PRComment) bool {
		return github.SameAccount(github.Account(c.AuthorLogin, c.AuthorType), author) && ownWord(c.Body) // back to the author's last own word
	})
	if err != nil {
		return externalRead{}, err
	}
	x, outside := externalOf(externalInput{Threads: threads, Reviews: reviews, Comments: comments, CommentsComplete: complete,
		Author: author, Own: e.fixLogins(ctx, job)})
	read := externalRead{items: x, outside: outside, head: job.pr.HeadSHA}
	if !threadsComplete {
		read.cut = append(read.cut, fmt.Sprintf("over %d review threads", github.MaxReviewThreads))
	}
	if !reviewsComplete {
		read.cut = append(read.cut, fmt.Sprintf("over %d reviews", github.MaxReviews))
	}
	if !complete {
		read.cut = append(read.cut, fmt.Sprintf("over %d comments since the author's last", github.MaxIssueComments))
	}
	return read, nil
}

// othersWaiting reads the other reviewers' items of probe's PR (its repo,
// watch, pr and login) for a fix on a review that found nothing and left
// nothing open, on, its short sha (spec 96), or takes carried, the read the
// same request made at the same head before a whiteboard job yielded to it
// (requestFix). It returns the read when at least one item waits for an
// answer, which the job then writes to its external file (writeExternal),
// else the refusal: nothing to fix when GitHub listed every item, and a
// word to answer them by hand when it cut a list short, which can hide one
// (1,000 resolved threads, then an open one). A read that fails refuses
// with its cause, since the job would have nothing to answer.
func (e *Engine) othersWaiting(ctx context.Context, probe *fixJob, on string, carried *externalRead) (*externalRead, error) {
	read := carried
	if read == nil || read.head != probe.pr.HeadSHA {
		r, err := e.readExternal(ctx, probe)
		if err != nil {
			return nil, fmt.Errorf("%s: the review on %s found nothing, and the other reviewers' threads and comments could not be read: %w", probe.label(), on, err)
		}
		read = &r
	}
	switch threads, conversation := read.items.unanswered(); {
	case threads+conversation > 0:
		return read, nil
	case len(read.cut) > 0:
		return nil, fmt.Errorf("%s: the review on %s found nothing, and the other reviewers' threads and comments could not all be read (%s); answer them by hand",
			probe.label(), on, strings.Join(read.cut, ", "))
	}
	return nil, fmt.Errorf("nothing to fix: the review on %s found nothing and no other reviewer's thread or comment waits for an answer", on)
}

// othersBeforeStart reads the other reviewers' items of probe's PR (its
// repo, watch, pr and login) before a fix cycle starts, or takes carried,
// the read the same request made at the same head before a whiteboard job
// yielded to it (spec 102). alone, a new loop on a review that found
// nothing and left nothing open (on: its short sha), has othersWaiting
// decide, refusing when nothing waits or the read fails. A read GitHub
// refuses for the rate limit, once the meter says the poll identity's
// budget is below the floor, returns that low budget instead, for
// prepareFix to hold the job. Any other failure of a job with findings
// returns no read and no error: the job starts and writeExternal tries the
// read once more (with the floor off, a rate limit too).
func (e *Engine) othersBeforeStart(ctx context.Context, probe *fixJob, on string, alone bool, carried *externalRead) (*externalRead, *githubLowBudget, error) {
	var read *externalRead
	var err error
	switch {
	case alone:
		read, err = e.othersWaiting(ctx, probe, on, carried)
	case carried != nil && carried.head == probe.pr.HeadSHA:
		read = carried
	default:
		var r externalRead
		if r, err = e.readExternal(ctx, probe); err == nil {
			read = &r
		}
	}
	if err == nil {
		return read, nil, nil
	}
	if errors.Is(err, github.ErrRateLimited) {
		if low, ok := e.githubLow(probe.watch.PollIdentity); ok { // github_cost.go: the refusal told the meter
			return nil, &low, nil
		}
	}
	if alone {
		return nil, nil, err
	}
	e.log.Info("fix: the other reviewers' items could not be read before the start; the job reads them again", "subject", probe.subject(), "err", err)
	return nil, nil, nil
}

// writeExternal writes the other reviewers' open items of the job's PR to
// externalFile in dir, the fixer's report's directory, and returns its
// path, recording a fix.external event with the counts; "" when they cannot
// be read or written, which the event says (the fixer then answers
// magnum's findings alone, and a job of the others alone fails), and a
// file an earlier run of the review left there is removed, so that nothing
// reads it for this run. A job prepareFix read them for (others) writes
// that read; one whose read failed reads them here, its one retry. The
// event names the lists GitHub cut short. unread is what the fixer does not
// get of them, for the loop's row (FixState.OthersUnread): "not read (HTTP
// 502)", "not all read (over 1000 review threads)", "" when it gets all.
func (e *Engine) writeExternal(ctx context.Context, job *fixJob, dir string) (path, unread string) {
	var read externalRead
	var err error
	if job.others != nil {
		read = *job.others
	} else {
		read, err = e.readExternal(ctx, job)
	}
	x, outside := read.items, read.outside
	path = filepath.Join(dir, externalFile)
	cause := ""
	if err != nil {
		cause = unreadCause(err)
	} else if err = writeExternalFile(path, x); err != nil {
		cause = "their file could not be written"
	}
	if err != nil {
		if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			e.log.Warn("fix: remove an earlier external file", "subject", job.subject(), "err", rerr)
		}
		if ctx.Err() == nil {
			then := job.role.Name + " answers magnum's findings alone"
			if job.alone {
				then = "the review found nothing, so " + job.role.Name + " has nothing to answer"
			}
			e.event(ctx, "warn", job.subject(), "fix.external", fmt.Sprintf("could not list the other reviewers' open threads and comments; "+
				"%s: %v", then, err), map[string]any{"run": job.run.ID})
		}
		return "", "not read (" + cause + ")"
	}
	if len(read.cut) > 0 {
		unread = "not all read (" + strings.Join(read.cut, ", ") + ")"
	}
	threads, conversation := x.unanswered()
	msg := fmt.Sprintf("%s: %s of other reviewers (%d unanswered), %s since the author's last (%d unanswered)",
		externalFile, textx.Count(len(x.Threads), "open thread", "open threads"), threads,
		textx.Count(len(x.Conversation), "review body or comment", "review bodies and comments"), conversation)
	if outside > 0 {
		msg += fmt.Sprintf("; %s by people outside the repository left out", textx.Count(outside, "comment", "comments"))
	}
	if len(read.cut) > 0 {
		msg += "; not all read: " + strings.Join(read.cut, ", ")
	}
	e.event(ctx, "info", job.subject(), "fix.external", msg,
		map[string]any{"run": job.run.ID, "file": path, "threads": len(x.Threads), "unanswered_threads": threads,
			"conversation": len(x.Conversation), "unanswered_conversation": conversation, "outside": outside, "cut": len(read.cut)})
	return path, unread
}

// graphQLTypeRe is a GraphQL error type as GitHub names it (RATE_LIMIT,
// NOT_FOUND), the only part of a GraphQL error unreadCause repeats.
var graphQLTypeRe = regexp.MustCompile(`^[A-Z_]{1,40}$`)

// unreadCause is why the other reviewers' items could not be read, in a few
// words for the loop's row and its end, never an error's text: GitHub's
// rate limit, a time-out, an HTTP status, a GraphQL error type, else "a
// failed read".
func unreadCause(err error) string {
	var api *github.APIError
	isAPI := errors.As(err, &api)
	switch {
	case errors.Is(err, github.ErrRateLimited):
		return "GitHub's rate limit"
	case errors.Is(err, context.DeadlineExceeded):
		return "a time-out"
	case isAPI && api.Status != 0:
		return fmt.Sprintf("HTTP %d", api.Status)
	case isAPI && len(api.Errors) > 0 && graphQLTypeRe.MatchString(api.Errors[0].Type):
		return api.Errors[0].Type
	}
	return "a failed read"
}

// othersUnread is what a loop's end says when the fixer of its last cycle
// did not get all the other reviewers' items (FixState.OthersUnread): "the
// other reviewers' threads and comments were not read (HTTP 502): `magnum
// fix talkable/talkable#2` reads them again"; "" when it got them.
func (j *fixJob) othersUnread() string {
	if j.state.OthersUnread == "" {
		return ""
	}
	return "the other reviewers' threads and comments were " + j.state.OthersUnread + ": `magnum fix " + j.label() + "` reads them again"
}

// writeExternalFile writes x to path as indented JSON, the directory made
// when missing.
func writeExternalFile(path string, x externalItems) error {
	b, err := json.MarshalIndent(x, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsx.WriteFileAtomic(path, append(b, '\n'), 0o600)
}

// readExternalFile reads the external file in dir (ok false when there is
// none, or it cannot be read).
func readExternalFile(dir string) (externalItems, bool) {
	b, err := os.ReadFile(filepath.Join(dir, externalFile))
	if err != nil {
		return externalItems{}, false
	}
	var x externalItems
	if json.Unmarshal(b, &x) != nil {
		return externalItems{}, false
	}
	return x, true
}

// dropFixerAnswered is entries, the fixer's "## Needs you", without the
// fixes it says it did not answer in a thread of the external file beside
// the job's report that GitHub shows its reply in, made since the job
// started (a comment with the fixer's marker by the PR author or one of
// magnum's logins: the reply took though the fixer saw it fail). The skill
// lists a fix there only for its missing reply, so such an entry asks
// nothing of the operator, and a fix.external event counts them. The
// threads come from read, the settle's one GitHub read (threadsReader).
// Nothing to look at, or a read that fails, keeps every entry.
func (e *Engine) dropFixerAnswered(ctx context.Context, job *fixJob, entries []string, read func() ([]github.Thread, error)) []string {
	x, ok := readExternalFile(filepath.Dir(deref(job.run.ReportPath)))
	if !ok {
		return entries
	}
	look := map[int64]bool{}
	for _, entry := range entries {
		if id, ok := fixedInExternal(entry, x); ok {
			look[id] = true
		}
	}
	if len(look) == 0 {
		return entries
	}
	threads, err := read() // a thread past the list keeps its entry
	if err != nil {
		return entries
	}
	own := e.fixLogins(ctx, job)
	author := github.Account(deref(job.pr.AuthorLogin), deref(job.pr.AuthorType))
	byFixer := func(c github.ThreadComment) bool {
		a := github.Account(c.AuthorLogin, c.AuthorType)
		return postreview.IsFixReply(c.Body) && !c.CreatedAt.Before(job.started) &&
			(github.SameAccount(a, author) || slices.ContainsFunc(own, func(o string) bool { return github.SameAccount(o, a) }))
	}
	replied := map[int64]bool{}
	for _, t := range threads {
		if all := t.AllComments(); len(all) > 0 && look[all[0].ID] && slices.ContainsFunc(all[1:], byFixer) {
			replied[all[0].ID] = true
		}
	}
	if len(replied) == 0 {
		return entries
	}
	kept := slices.DeleteFunc(slices.Clone(entries), func(entry string) bool {
		id, ok := fixedInExternal(entry, x)
		return ok && replied[id]
	})
	n := len(entries) - len(kept)
	e.event(ctx, "info", job.subject(), "fix.external", fmt.Sprintf("%s left %s to you as fixed but unanswered in another reviewer's thread; "+
		"GitHub shows its reply there, so %s nothing", job.role.Name, textx.Count(n, "finding", "findings"), textx.Plural(n, "it asks", "they ask")),
		map[string]any{"run": job.run.ID, "answered": n})
	return kept
}

// fixedInExternal is the thread of entry when it is a fix the fixer says it
// did not answer ("fixed in <sha>.") in a thread x lists.
func fixedInExternal(entry string, x externalItems) (int64, bool) {
	n, ok := readNeedsYouEntry(entry)
	if !ok || n.fixedIn == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(n.id, 10, 64)
	if err != nil || !slices.ContainsFunc(x.Threads, func(t externalThread) bool { return t.CommentID == id }) {
		return 0, false
	}
	return id, true
}
