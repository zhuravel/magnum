package engine

// The other reviewers' open items a fix job hands its fixer (spec 93). At
// the job's start the engine writes externalFile beside the fixer's report,
// in the review's directory: every unresolved review thread of the PR that
// another reviewer started, neither magnum's logins nor the PR author
// (people and bots alike: a colleague, another review tool, a code-review
// bot), each "answered" when its latest comment is the PR author's or the
// fixer's, and the review bodies (an approval's never: it asks for
// nothing) and conversation comments of others since
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

// externalItems is the file's content: the PR author (Account form), the
// login the fixer posts as (Fixer: a fixer App's login, else the author),
// and magnum's logins (FixerLogins, Account form, a fixer App's
// included), the only ones whose comments answer an item (the skill's look
// right before a reply checks them by name), the threads and the
// conversation.
type externalItems struct {
	Author       string           `json:"author"`
	Fixer        string           `json:"fixer,omitempty"`
	FixerLogins  []string         `json:"fixer_logins"`
	Threads      []externalThread `json:"threads"`
	Conversation []externalRemark `json:"conversation"`
	// Decisions are magnum's own threads that hold the author's decision on
	// a call the fixer left them there (fix_decide.go), as a thread of
	// Threads holds one: Decision set, at DecisionURL.
	Decisions []externalThread `json:"decisions,omitempty"`
	// called are magnum's own threads, by their first comment's id, in
	// which the fixer's call is still open (not in the file), which `magnum
	// fix --decide` answers.
	called map[int64]bool
}

// externalThread is an unresolved review thread another reviewer started:
// the REST id and link of its first comment (a reply goes to that id),
// where it is (its line, else its original line), whether it is outdated
// and answered, and its comments, oldest first: in a long thread the first
// and the newest GitHub lists (github.Thread.AllComments), Omitted
// counting the ones between them that the file leaves out. Decision says
// that the thread holds the author's decision on a call the fixer left them
// there (fix_decide.go), at DecisionURL (its last comment but for any the
// fixer may have written since): not answered, the fixer builds it. called
// (not in the file) says that the fixer's call there is still open, which
// `magnum fix --decide` answers.
type externalThread struct {
	CommentID   int64             `json:"comment_id"`
	URL         string            `json:"url"`
	Path        string            `json:"path"`
	Line        int               `json:"line,omitempty"`
	Outdated    bool              `json:"outdated"`
	Answered    bool              `json:"answered"`
	Decision    bool              `json:"decision,omitempty"`
	DecisionURL string            `json:"decision_url,omitempty"`
	Comments    []externalComment `json:"comments"`
	Omitted     int               `json:"omitted,omitempty"`
	called      bool
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
	EditedAt  string `json:"edited_at,omitempty"`
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
	// Markers are [pipeline] quiet_markers, and Resolvers the fixer's
	// logins (Account form), whose resolution of another reviewer's thread
	// does not close it: the look's part of the read (externalReadOf),
	// which externalOf does not use.
	Markers   []string
	Resolvers []string
	// AppFixer says the watch's fixer acts as a dedicated App, whose
	// resolution GitHub names no one for (a review thread's resolvedBy is
	// a User): a person's thread resolved by no one named is the fixer's.
	AppFixer bool
	// Calls are the threads, by their first comment's id, in which the
	// fixer left the author a call that the loop's record still asks for,
	// with when it made each (fixerCalls).
	Calls map[int64]time.Time
	// Busy says whether a fix run of the PR was at work at a time
	// (fixBusy; nil: none ever was).
	Busy func(time.Time) bool
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
// in anyone else's comment answers nothing), unless it holds the author's
// decision (decisionIn): their own word after the fixer's call there
// (in.Calls) while that call is open, which no word the fixer may have
// written since hides (in.Busy, under a gh fixer). One of magnum's own
// threads is no item, but one holding such a call: it is called while the
// call is open, and with the author's decision there it is one of the
// file's decisions (Decisions), read the same way. A review body or conversation
// comment is another reviewer's when a reviewer other than the author and
// magnum's logins wrote it, it says something (the body of a review that
// comments or requests changes: remarkState) and it came after the author's last own
// word (ownWord) among the reviews and the comments, or, when the comments
// stop short of that word (CommentsComplete false), after the oldest comment
// GitHub listed; it is answered when a later comment of the author or of
// magnum's logins carrying the fixer's marker names it as its target
// (replyTarget). A remark edited in place counts from its last edit, for
// both (its text is new: an answer to the old text answers nothing), and
// the file gives that time (edited_at). The conversation is oldest first.
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
	busy := func(time.Time) bool { return false } // a fixer App never posts as the author
	if !in.AppFixer && in.Busy != nil {
		busy = in.Busy
	}

	x := externalItems{Author: in.Author, FixerLogins: append([]string{}, in.Own...), Threads: []externalThread{}, Conversation: []externalRemark{}}
	outside := 0
	for _, t := range in.Threads {
		all := t.AllComments()
		if t.Resolved || len(all) == 0 || byAuthor(all[0].AuthorLogin, all[0].AuthorType) {
			continue // resolved, or the author's own thread
		}
		cs := slices.DeleteFunc(slices.Clone(all), func(c github.ThreadComment) bool {
			return !reviewer(c.AuthorLogin, c.AuthorType, c.AuthorAssociation)
		})
		if isOwn(all[0].AuthorLogin, all[0].AuthorType) { // magnum's own: only the fixer's call there makes it the file's
			if at, ok := in.Calls[all[0].ID]; ok {
				if called, d := decisionIn(cs, at, byAuthor, isOwn, busy); called {
					if x.called == nil {
						x.called = map[int64]bool{}
					}
					x.called[all[0].ID] = true
					if d != nil {
						et := threadOf(t, all, cs, name)
						et.Decision, et.DecisionURL = true, d.URL
						x.Decisions = append(x.Decisions, et)
					}
				}
			}
			continue
		}
		outside += len(all) - len(cs)
		if len(cs) == 0 || cs[0].ID != all[0].ID {
			continue // an outsider started it
		}
		last := cs[len(cs)-1]
		et := threadOf(t, all, cs, name)
		et.Answered = byAuthor(last.AuthorLogin, last.AuthorType) || (postreview.IsFixReply(last.Body) && isOwn(last.AuthorLogin, last.AuthorType))
		if at, ok := in.Calls[et.CommentID]; ok {
			var d *github.ThreadComment
			if et.called, d = decisionIn(cs, at, byAuthor, isOwn, busy); d != nil {
				et.Decision, et.Answered, et.DecisionURL = true, false, d.URL
			}
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
		at, text time.Time // when it was made, and its text (its last edit)
		r        externalRemark
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
		if strings.TrimSpace(r.Body) == "" || !remarkState(r.State) || !other(r.AuthorLogin, r.AuthorType, r.AuthorAssociation, later(r.SubmittedAt, r.EditedAt)) {
			continue
		}
		er := externalRemark{Kind: remarkReview, ID: r.DatabaseID, URL: r.URL, Author: name(r.AuthorLogin, r.AuthorType), State: r.State,
			CreatedAt: rfc3339(r.SubmittedAt), EditedAt: editedAt(r.SubmittedAt, r.EditedAt)}
		er.Body, er.Truncated = excerptOf(r.Body)
		items = append(items, dated{r.SubmittedAt, later(r.SubmittedAt, r.EditedAt), er})
	}
	for _, c := range in.Comments {
		if strings.TrimSpace(c.Body) == "" || !other(c.AuthorLogin, c.AuthorType, c.AuthorAssociation, later(c.CreatedAt, c.EditedAt)) {
			continue
		}
		er := externalRemark{Kind: remarkComment, ID: c.ID, URL: c.URL, Author: name(c.AuthorLogin, c.AuthorType), CreatedAt: rfc3339(c.CreatedAt),
			EditedAt: editedAt(c.CreatedAt, c.EditedAt)}
		er.Body, er.Truncated = excerptOf(c.Body)
		items = append(items, dated{c.CreatedAt, later(c.CreatedAt, c.EditedAt), er})
	}
	slices.SortStableFunc(items, func(a, b dated) int { return a.at.Compare(b.at) })
	for _, it := range items {
		it.r.Answered = it.r.URL != "" && slices.ContainsFunc(in.Comments, func(c github.PRComment) bool {
			return c.CreatedAt.After(it.text) && postreview.IsFixReply(c.Body) && (byAuthor(c.AuthorLogin, c.AuthorType) || isOwn(c.AuthorLogin, c.AuthorType)) &&
				replyTarget(c.Body) == it.r.URL
		})
		x.Conversation = append(x.Conversation, it.r)
	}
	return x, outside
}

// threadOf is the file's thread t, all its comments GitHub listed and cs
// the reviewers' among them (the first is its root), each comment's author
// named by name; not answered, and with no decision.
func threadOf(t github.Thread, all, cs []github.ThreadComment, name func(login, typ string) string) externalThread {
	root := cs[0]
	et := externalThread{CommentID: root.ID, URL: root.URL, Path: t.Path, Line: cmp.Or(t.Line, t.OriginalLine), Outdated: t.Outdated,
		Comments: []externalComment{}, Omitted: max(t.CommentCount-len(all), 0)}
	for _, c := range cs {
		ec := externalComment{ID: c.ID, URL: c.URL, Author: name(c.AuthorLogin, c.AuthorType), CreatedAt: rfc3339(c.CreatedAt), Fix: postreview.IsFixReply(c.Body)}
		ec.Body, ec.Truncated = excerptOf(c.Body)
		et.Comments = append(et.Comments, ec)
	}
	return et
}

// editedAt is the file's edited_at of a remark made at made and last
// edited at edited: "" for one never edited after it was made.
func editedAt(made, edited time.Time) string {
	if !edited.After(made) {
		return ""
	}
	return rfc3339(edited)
}

// remarkKey is the look's key of a remark of the file (fix_open_items.go):
// its id, and the time of its last edit when it was edited, so an edit
// makes a new key, as a new reply makes a thread's.
func remarkKey(r externalRemark) string {
	key := fmt.Sprintf("remark:%d", r.ID)
	if r.EditedAt == "" {
		return key
	}
	if at, err := time.Parse(time.RFC3339, r.EditedAt); err == nil {
		return fmt.Sprintf("%s@%d", key, at.Unix())
	}
	return key + "@" + r.EditedAt
}

// remarkState reports whether a review in state asks something of the
// author: one that comments or requests changes. A pending review is not
// posted, a dismissed one was taken back, and an approval asks for
// nothing: the fix skill gives it no verdict and no reply, so listed it
// was an item no loop could answer (a loop on a clean review that pushed
// nothing, then a leftover that needed the operator).
func remarkState(state string) bool { return state == "COMMENTED" || state == "CHANGES_REQUESTED" }

// fixLogins are the logins whose threads and replies on the job's PR are
// magnum's (Account form): ownLogins, the login that posted the review, and
// the watch's fixer App's (config.Config.FixerLogins), whose fix-marked
// replies answer an item as a gh fixer's do and whose resolution of
// another reviewer's thread does not close it.
func (e *Engine) fixLogins(ctx context.Context, job *fixJob) []string {
	own := e.ownLogins(ctx, job.watch, job.pr)
	for _, l := range append([]string{job.login}, e.cfg.FixerLogins(&job.watch)...) {
		if l != "" && !slices.ContainsFunc(own, func(o string) bool { return github.SameAccount(o, l) }) {
			own = append(own, l)
		}
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
// autofix = "open" reads the rest from the same lists, with no call of its
// own (fix_open_items.go): magnum's unresolved threads (mine), the review
// bodies and comments whose whole body holds a quiet marker (quiet, by id)
// and the other reviewers' threads a fixer login resolved (resolved).
type externalRead struct {
	items    externalItems
	outside  int
	cut      []string
	head     string
	mine     []magnumThread
	quiet    map[int64]bool
	resolved []resolvedThread
}

// decided reports whether the read shows the author's decision in the
// thread whose first comment is thread (externalThread.Decision), another
// reviewer's or magnum's own.
func (r *externalRead) decided(thread int64) bool {
	return slices.ContainsFunc(r.items.decisions(), func(t externalThread) bool { return t.CommentID == thread })
}

// decisions are the threads of x that hold the author's decision on a call
// the fixer left them: other reviewers' (Threads, Decision set), then
// magnum's own (Decisions).
func (x externalItems) decisions() []externalThread {
	out := slices.DeleteFunc(slices.Clone(x.Threads), func(t externalThread) bool { return !t.Decision })
	return append(out, x.Decisions...)
}

// externalReadOf is the read of in at head: the file's content
// (externalOf) and the look's part of it (fix_open_items.go), with no cut
// list (readExternal adds it).
func externalReadOf(in externalInput, head string) externalRead {
	x, outside := externalOf(in)
	return externalRead{items: x, outside: outside, head: head, mine: magnumThreadsOf(in.Threads, in.Own, in.Author), quiet: quietOf(in),
		resolved: resolvedByFixerOf(in)}
}

// readExternal reads the job's PR from GitHub and makes the file's content
// (externalOf), with the count of items an outsider wrote and which of the
// three lists GitHub cut short, and the look's part of it
// (externalReadOf): magnum's logins resolving another reviewer's thread
// never close it. The fixer's calls the PR's attention still asks for
// (fixerCalls) make the author's later word in those threads a decision,
// unless a fix run was at work when it came (fixBusy).
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
	own := e.fixLogins(ctx, job)
	var busy func(time.Time) bool
	calls := e.fixerCalls(ctx, job.pr)
	if len(calls) > 0 {
		busy = e.fixBusy(ctx, job.pr.ID)
	}
	read := externalReadOf(externalInput{Threads: threads, Reviews: reviews, Comments: comments, CommentsComplete: complete,
		Author: author, Own: own, Markers: e.cfg.Pipeline.QuietMarkers, Resolvers: own, AppFixer: len(e.cfg.FixerLogins(&job.watch)) > 0,
		Calls: calls, Busy: busy}, job.pr.HeadSHA)
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
// with its cause, since the job would have nothing to answer. Before it
// refuses, it asks merges whether the head conflicts with the PR's base
// (prepareFix's look, spec 5.6): a conflict is work enough, and the read,
// whole, cut short or none when it failed, goes to the job, whose start
// reads a failed one again; a look that failed ends the refusal with why
// ("; its merge with master could not be checked: …", a
// mergeUncheckedError when it says nothing to fix). A read GitHub
// refused for the rate limit asks nothing: the floor may hold the job, and
// the held fix looks at the base when it starts.
func (e *Engine) othersWaiting(ctx context.Context, probe *fixJob, on string, carried *externalRead, merges func() (bool, error)) (*externalRead, error) {
	read := carried
	var readErr error
	if read == nil || read.head != probe.pr.HeadSHA {
		r, err := e.readExternal(ctx, probe)
		if err != nil {
			read, readErr = nil, err
		} else {
			read = &r
		}
	}
	switch {
	case read != nil:
		if threads, conversation := read.items.unanswered(); threads+conversation > 0 {
			return read, nil
		}
	case errors.Is(readErr, github.ErrRateLimited): // held as the floor holds it (othersBeforeStart), the merge looked at when it starts
		return nil, fmt.Errorf("%s: the review on %s found nothing, and the other reviewers' threads and comments could not be read: %w", probe.label(), on, readErr)
	}
	merge, unchecked := merges()
	if merge {
		return read, nil
	}
	why := ""
	if unchecked != nil {
		why = "; " + oneLine(unchecked.Error(), 300)
	}
	switch {
	case readErr != nil:
		return nil, fmt.Errorf("%s: the review on %s found nothing, and the other reviewers' threads and comments could not be read: %w%s", probe.label(), on, readErr, why)
	case len(read.cut) > 0:
		return nil, fmt.Errorf("%s: the review on %s found nothing, and the other reviewers' threads and comments could not all be read (%s); answer them by hand%s",
			probe.label(), on, strings.Join(read.cut, ", "), why)
	}
	refusal := fmt.Errorf("nothing to fix: the review on %s found nothing and no other reviewer's thread or comment waits for an answer%s", on, why)
	if unchecked != nil {
		return nil, &mergeUncheckedError{refusal: refusal, cause: unchecked}
	}
	return nil, refusal
}

// mergeUncheckedError is othersWaiting's "nothing to fix" whose look at
// the PR's base failed (cause: "its merge with master could not be
// checked: …"): nothing else waits, but a conflict may, so the refusal is
// not final. It says what the refusal says; autofix = "open" waits on it
// and asks again (openStart), where a plain "nothing to fix" needs you.
type mergeUncheckedError struct{ refusal, cause error }

func (e *mergeUncheckedError) Error() string { return e.refusal.Error() }

// othersBeforeStart reads the other reviewers' items of probe's PR (its
// repo, watch, pr and login) before a fix cycle starts, or takes carried,
// the read the same request made at the same head before a whiteboard job
// yielded to it (spec 102). alone, a new loop on a review that found
// nothing and left nothing open (on: its short sha), has othersWaiting
// decide, refusing when nothing waits or the read fails, unless merges
// finds a conflict with the base. A read GitHub refuses for the rate limit,
// once the meter says the poll identity's budget is below the floor,
// returns that low budget instead, for prepareFix to hold the job. Any
// other failure of a job with findings returns no read and no error: the
// job starts and writeExternal tries the read once more (with the floor
// off, a rate limit too).
func (e *Engine) othersBeforeStart(ctx context.Context, probe *fixJob, on string, alone bool, carried *externalRead,
	merges func() (bool, error)) (*externalRead, *githubLowBudget, error) {
	var read *externalRead
	var err error
	switch {
	case alone:
		read, err = e.othersWaiting(ctx, probe, on, carried, merges)
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
	x.Fixer = x.Author // a gh fixer posts as the author
	if logins := e.cfg.FixerLogins(&job.watch); len(logins) > 0 {
		x.Fixer = logins[0]
	}
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
			switch {
			case job.alone && job.conflict.sha != "":
				then = "the review found nothing, so " + job.role.Name + " only merges the base"
			case job.alone:
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
	asFixer := e.sinceFixStart(ctx, job)
	byFixer := func(c github.ThreadComment) bool {
		return postreview.IsFixReply(c.Body) && asFixer(c.AuthorLogin, c.AuthorType, c.CreatedAt)
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

// sinceFixStart reports whether a comment (its author's login and
// __typename, and when it was made) is one the fixer could have posted in
// the job's run: made since the run started, by one of magnum's logins
// (fixLogins), or by the PR author, whom a gh fixer posts as. A fixer App
// never posts as the author, so under one the author's own replies during
// the run are theirs.
func (e *Engine) sinceFixStart(ctx context.Context, job *fixJob) func(login, typ string, at time.Time) bool {
	own := e.fixLogins(ctx, job)
	author := github.Account(deref(job.pr.AuthorLogin), deref(job.pr.AuthorType))
	app := len(e.cfg.FixerLogins(&job.watch)) > 0
	return func(login, typ string, at time.Time) bool {
		a := github.Account(login, typ)
		return !at.Before(job.started) &&
			((!app && github.SameAccount(a, author)) || slices.ContainsFunc(own, func(o string) bool { return github.SameAccount(o, a) }))
	}
}

// nameUnmarkedReplies names, in a fix.unmarked warning, each reply that the
// PR's author (or one of magnum's logins) made since the job's run started
// and that carries no magnum marker (ownWord): in a review thread, and on
// the conversation. The fixer posts every reply through `magnum
// fix-reply`, which signs it with the fix marker, but nothing forces a
// reply typed another way through it (on talkable#12112 two went out with
// `gh api`), and magnum reads such a reply as the author's own word: the
// retro learns from it (learn.OwnThreads), it answers a thread of the
// external file, and on the conversation it moves the author's last own
// word, so the next cycle's external file drops the other reviewers'
// earlier items (externalOf). The event gives each reply's link and id,
// never its text, and magnum edits no posted comment. The threads come from
// read, the settle's one GitHub read of them (threadsReader); the
// conversation from one read, newest first, back to the run's start
// (usually one page). A read that fails names nothing of its part. The
// author's decisions the job's external file hands the fixer are their own
// word, not the fixer's: `magnum fix --decide` posts one right before the
// run starts, which a clock a moment behind GitHub's would put after it.
func (e *Engine) nameUnmarkedReplies(ctx context.Context, job *fixJob, read func() ([]github.Thread, error)) {
	if job.started.IsZero() {
		return
	}
	decisions := map[string]bool{}
	if x, ok := readExternalFile(filepath.Dir(deref(job.run.ReportPath))); ok {
		for _, t := range x.decisions() {
			decisions[t.DecisionURL] = true
		}
	}
	asFixer := e.sinceFixStart(ctx, job)
	var urls []string
	var inThreads, onConversation []int64
	if threads, err := read(); err == nil {
		for _, t := range threads {
			all := t.AllComments()
			for _, c := range all[min(1, len(all)):] { // replies only: the fixer starts no thread
				if asFixer(c.AuthorLogin, c.AuthorType, c.CreatedAt) && ownWord(c.Body) && (c.URL == "" || !decisions[c.URL]) {
					urls, inThreads = append(urls, cmp.Or(c.URL, fmt.Sprintf("review comment %d", c.ID))), append(inThreads, c.ID)
				}
			}
		}
	}
	if _, rd, ok := e.fixReader(job); ok {
		before := func(c github.PRComment) bool { return c.CreatedAt.Before(job.started) }
		comments, _, err := rd.IssueComments(ctx, job.repo.Owner, job.repo.Name, job.pr.Number, before)
		if err != nil {
			e.log.Warn("fix: the PR's conversation, for the author's replies without the fix marker", "subject", job.subject(), "err", err)
		}
		for _, c := range comments {
			if asFixer(c.AuthorLogin, c.AuthorType, c.CreatedAt) && ownWord(c.Body) {
				urls, onConversation = append(urls, cmp.Or(c.URL, fmt.Sprintf("comment %d", c.ID))), append(onConversation, c.ID)
			}
		}
	}
	n := len(urls)
	if n == 0 {
		return
	}
	data := map[string]any{"run": job.run.ID, "unmarked": n}
	if len(inThreads) > 0 {
		data["comments"] = inThreads
	}
	if len(onConversation) > 0 {
		data["issue_comments"] = onConversation
	}
	e.event(ctx, "warn", job.subject(), "fix.unmarked", fmt.Sprintf("%s: %s by the PR's author since the fixer started %s no fix marker, so magnum reads %s as the author's own: %s",
		job.role.Name, textx.Count(n, "reply", "replies"), textx.Plural(n, "carries", "carry"), textx.Plural(n, "it", "them"), strings.Join(urls, ", ")), data)
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
