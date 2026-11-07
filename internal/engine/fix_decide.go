package engine

// Your decisions on what the fixer left you. When the fixer leaves another
// reviewer's thread to you under Needs you (a decline, a product choice:
// attention.DeclinedCall), and you answer in that thread, your reply is the
// thread's last comment, which alone would read as "answered", and the next
// loop's fixer would leave it. While the fixer's call is still open in the
// loop's record (the PR's attention of kind fix that names it, and its
// report: fixerCalls), your own word there after the call is a decision
// (decisionIn): the external file marks the thread `decision` with your
// reply's link, it counts as unanswered, and the fixer builds it (the fix,
// or the won't-fix reply you chose; skills/magnum-fix/SKILL.md). Only your
// reply counts, never another reviewer's, a bot's or the fixer's own (its
// replies carry the fix marker), and only after the fixer's call for that
// same thread: a fix-marked reply of the fixer's after the call closes it.
// A person's decision never closes on the fixer's word alone: after the run,
// a decision GitHub shows no reply of the fixer's in goes back under Needs
// you (decisionsLeft); a loop that built every decision its attention asked
// for, and left nothing, lifts that attention (liftDecided); and a failed
// run or the cap keeps the decision calls of the attention it replaces
// (keepDecisions). `magnum fix --decide <thread>="<text>"` posts your reply
// as you and starts the loop in one step (decideCheck, decidePost). Under
// autofix = "open" a decision is new work for the look: the thread's key
// takes your reply, and an attention of decision calls you all decided stops
// no start (callsDecided).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/postreview"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// FixDecision is one reply of `magnum fix --decide`: the thread, by the id
// of its first comment (attention.FixCall's Thread), and your words, which
// magnum posts there as you; no event or log carries them.
type FixDecision struct {
	Thread int64  `json:"thread"`
	Text   string `json:"text"`
}

// fixDecideBy names `magnum fix --decide` in its event.
const fixDecideBy = "magnum fix --decide"

// decisionIn reads a thread in which the fixer made a call at at, from cs,
// the reviewers' comments of it oldest first (externalOf's): the call is
// open while no fix-marked reply of the fixer's (by the author, whom a gh
// fixer posts as, or by one of magnum's logins) came after it, and the
// thread holds the author's decision when the call is open and its last
// comment is the author's own word (ownWord: no magnum marker), made after
// the call.
func decisionIn(cs []github.ThreadComment, at time.Time, byAuthor, isOwn func(login, typ string) bool) (open, decision bool) {
	open = !slices.ContainsFunc(cs, func(c github.ThreadComment) bool {
		return c.CreatedAt.After(at) && postreview.IsFixReply(c.Body) && (byAuthor(c.AuthorLogin, c.AuthorType) || isOwn(c.AuthorLogin, c.AuthorType))
	})
	last := cs[len(cs)-1]
	return open, open && len(cs) > 1 && byAuthor(last.AuthorLogin, last.AuthorType) && ownWord(last.Body) && last.CreatedAt.After(at)
}

// decisionCall reports whether c, a call of a fix job's message, asks the
// author to decide in another reviewer's thread (attention.DeclinedCall;
// "a human's" in the older words).
func decisionCall(c attention.FixCall) bool {
	return c.Thread != "" && (strings.HasPrefix(c.Action, "decide another reviewer's ") || strings.HasPrefix(c.Action, "decide a human's "))
}

// fixerCalls are the calls the fixer left the author in other reviewers'
// threads that the loop's record still asks for, by the id of each
// thread's first comment, with when each was made. The record is the PR's
// attention of kind fix while it asks for the fixer's calls
// (attention.FixAsks): each call it names that asks for a decision
// (decisionCall; one magnum put back for a decision the fixer did not
// build, decisionsLeft, or kept after a failure, keepDecisions, among
// them), and each entry of its report's "## Needs you" that it still names
// (FixNeedsAsked) and the fixer did not fix ("not fixed.", or neither
// word) in a thread the external file beside the report lists. A call is
// made when the settle reads the report: at the time of the fix run that
// wrote it (verified, else ended). None while the PR needs nothing of the
// kind, or when the report or its run cannot be read.
func (e *Engine) fixerCalls(ctx context.Context, pr store.PR) map[int64]time.Time {
	msg := deref(pr.LastError)
	if pr.State != store.PRNeedsAttention || !attention.FixAsks(msg) {
		return nil
	}
	if kind, _ := e.getKV(ctx, KVPRAttention(pr.ID)); kind != attention.KindFix {
		return nil
	}
	report := attention.FixReport(msg)
	if report == "" {
		return nil
	}
	b, err := os.ReadFile(report)
	if err != nil {
		return nil
	}
	runID, _ := agents.ReportRun(b)
	if runID == "" {
		return nil
	}
	run, err := e.st.RunByID(ctx, runID)
	if err != nil || run.PRID != pr.ID || run.Kind != store.RunFix {
		return nil
	}
	at := run.VerifiedAt
	if at == nil {
		at = run.EndedAt
	}
	if at == nil {
		return nil
	}
	calls := map[int64]time.Time{}
	add := func(id string) {
		if n, err := strconv.ParseInt(id, 10, 64); err == nil {
			calls[n] = *at
		}
	}
	for _, c := range attention.Explain(attention.KindFix, msg, "").NeedsYou {
		if decisionCall(c) {
			add(c.Thread)
		}
	}
	if rep, err := ParseFixReport(string(b), runID); err == nil {
		owners := readThreadOwners(filepath.Dir(report))
		for _, entry := range FixNeedsAsked(msg, rep.NeedsYou) {
			n, ok := readNeedsYouEntry(entry)
			if !ok || n.fixedIn != "" || !strings.Contains(n.place, ":") {
				continue // a fix asks for a reply, not a decision; the description, a remark and a conflict have no thread
			}
			if magnums, known := owners.owner(n); known && !magnums {
				add(n.id)
			}
		}
	}
	if len(calls) == 0 {
		return nil
	}
	return calls
}

// callsDecided reports whether msg, a PR's attention, asks for the fixer's
// decision calls alone (attention.FixDecisionCalls) and decided says that
// the author decided in each one's thread: nothing else is asked of them,
// so a loop may start over it, and once the fixer built them all it is
// lifted.
func callsDecided(msg string, decided func(thread int64) bool) bool {
	calls, ok := attention.FixDecisionCalls(msg)
	if !ok {
		return false
	}
	for _, c := range calls {
		id, err := strconv.ParseInt(c.Thread, 10, 64)
		if err != nil || !decided(id) {
			return false
		}
	}
	return true
}

// decisionsLeft is entries, the fixer's "## Needs you", with an entry of
// magnum's for each decision of the job's external file that the fixer
// neither built nor left there, and the decisions it built: those whose
// thread GitHub shows a fix-marked reply of the fixer's in since its run
// started (read, the settle's one read of the threads; a read that fails
// shows none). A person's decision never closes on the fixer's word alone,
// its report's included: one it says it answered, whose reply GitHub does
// not show, goes back to the author too. Magnum's entry reads as the
// fixer's own ("<id> <path>:<line>: not fixed. …"), so its call asks the
// author to decide again (attention.DeclinedCall), and a fix.decisions
// event counts both.
func (e *Engine) decisionsLeft(ctx context.Context, job *fixJob, entries []string, read func() ([]github.Thread, error)) ([]string, map[int64]bool) {
	x, ok := readExternalFile(filepath.Dir(deref(job.run.ReportPath)))
	if !ok {
		return entries, nil
	}
	decided := slices.DeleteFunc(slices.Clone(x.Threads), func(t externalThread) bool { return !t.Decision })
	if len(decided) == 0 {
		return entries, nil
	}
	replied := map[int64]bool{}
	if threads, err := read(); err == nil {
		asFixer := e.sinceFixStart(ctx, job)
		for _, t := range threads {
			if all := t.AllComments(); len(all) > 0 && slices.ContainsFunc(all[1:], func(c github.ThreadComment) bool {
				return postreview.IsFixReply(c.Body) && asFixer(c.AuthorLogin, c.AuthorType, c.CreatedAt)
			}) {
				replied[all[0].ID] = true
			}
		}
	}
	listed := map[string]bool{}
	for _, entry := range entries {
		if n, ok := readNeedsYouEntry(entry); ok {
			listed[n.id] = true
		}
	}
	out, built, back := slices.Clone(entries), map[int64]bool{}, 0
	for _, t := range decided {
		id := strconv.FormatInt(t.CommentID, 10)
		switch {
		case replied[t.CommentID]:
			built[t.CommentID] = true
		case listed[id]:
			back++
		default:
			out = append(out, fmt.Sprintf("%s %s:%d: not fixed. magnum: GitHub shows no reply of the fixer's that builds your decision", id, t.Path, t.Line))
			back++
		}
	}
	var said []string
	if n := len(built); n > 0 {
		said = append(said, "built your "+textx.Plural(n, "decision", "decisions")+" in "+textx.Count(n, "thread", "threads"))
	} else {
		said = append(said, "built none of your decisions")
	}
	level := "info"
	if back > 0 {
		level = "warn"
		said = append(said, textx.Count(back, "decision", "decisions")+" "+textx.Plural(back, "goes", "go")+" back to you")
	}
	data := job.eventData()
	data["built"], data["back"] = len(built), back
	e.event(ctx, level, job.subject(), "fix.decisions", job.role.Name+" "+strings.Join(said, "; "), data)
	return out, built
}

// liftDecided puts the job's PR back to reviewed once its loop ends with
// nothing pushed and nothing left to the author, when the PR's attention is
// the fixer's decision calls alone and this run built every one (built,
// decisionsLeft): you decided each call, and GitHub shows the fixer's reply
// that builds each. The compare-and-set holds the head, the reviewed head
// and the attention's words, as `magnum fix --done` does; the attention's
// rows go, and a fix.decisions_built event says so.
func (e *Engine) liftDecided(ctx context.Context, job *fixJob, built map[int64]bool) {
	if len(built) == 0 {
		return
	}
	pr, err := e.st.PRByID(ctx, job.pr.ID)
	if err != nil || pr.State != store.PRNeedsAttention || deref(pr.ReviewedSHA) != pr.HeadSHA {
		return
	}
	if kind, _ := e.getKV(ctx, KVPRAttention(pr.ID)); kind != attention.KindFix {
		return
	}
	msg := deref(pr.LastError)
	if !callsDecided(msg, func(id int64) bool { return built[id] }) {
		return
	}
	err = e.st.TransitionPR(ctx, pr.ID, []string{store.PRNeedsAttention}, store.PRReviewed, func(u *store.PRUpdate) {
		u.Set("last_error", nil)
		u.Where("head_sha", pr.HeadSHA)
		u.Where("reviewed_sha", pr.HeadSHA)
		u.Where("last_error", msg)
	})
	if err != nil {
		e.log.Info("fix: the PR moved on before the lift of the decisions the fixer built", "subject", job.subject(), "err", err)
		return
	}
	e.delKV(ctx, KVPRAttention(pr.ID), kvPRAttentionRun(pr.ID))
	e.event(ctx, "info", job.subject(), "fix.decisions_built", fmt.Sprintf("%s built every decision you made: the review of %s stands, with no new round",
		job.role.Name, textx.ShortSHA(pr.HeadSHA)), job.eventData())
}

// keepDecisions is msg, the attention a fix job raises over old, the
// PR's attention it replaces, with old's decision calls (decisionCall)
// after msg's own words, when msg asks for no call of its own (a failed
// run, the cap): a person's decision never closes on a failure, and the
// next `magnum fix` finds those calls still open (fixerCalls) and builds
// the decisions made on them. A message with calls of its own is the
// fixer's last word on them (decisionsLeft put back the ones it did not
// build), and stays as it is.
func keepDecisions(old, msg string) string {
	if !attention.FixAsks(old) || attention.FixAsks(msg) {
		return msg
	}
	var keep []attention.FixCall
	for _, c := range attention.Explain(attention.KindFix, old, "").NeedsYou {
		if decisionCall(c) {
			keep = append(keep, c)
		}
	}
	report := attention.FixReport(old)
	if len(keep) == 0 || report == "" {
		return msg
	}
	first, rest, under := strings.Cut(msg, "\n")
	first += "; " + attention.FixCallsText(keep, "", report)
	if under {
		first += "\n" + rest
	}
	return first
}

// threadReplier is what `magnum fix --decide` posts your reply with: a
// client of your gh identity (*github.Client has it).
type threadReplier interface {
	ReplyToReviewComment(ctx context.Context, owner, repo string, number int, commentID int64, body string) (github.ReviewCommentReply, error)
}

var _ threadReplier = (*github.Client)(nil)

// decidePlan is what `magnum fix --decide` posts and starts: the replies,
// the identity's login and client, the PR's read of the other reviewers
// before them (the refusals' and the dry run's), and the replies already
// on GitHub (by thread, their links: a --decide run again after a start
// that was refused posts no second reply), then what decidePost said.
type decidePlan struct {
	decide []FixDecision
	login  string
	client threadReplier
	read   externalRead
	posted map[int64]string
	note   string
}

// checkDecisions refuses replies `magnum fix --decide` cannot post: an
// empty one, one with a magnum marker (magnum would read it as its own, not
// your word), one longer than GitHub takes, and a thread named twice.
func checkDecisions(decide []FixDecision) error {
	seen := map[int64]bool{}
	for _, d := range decide {
		switch {
		case d.Thread <= 0:
			return fmt.Errorf("magnum fix --decide: %d is no thread's comment id", d.Thread)
		case seen[d.Thread]:
			return fmt.Errorf("magnum fix --decide: thread %d twice: one reply per thread", d.Thread)
		case strings.TrimSpace(d.Text) == "":
			return fmt.Errorf("magnum fix --decide: the reply in thread %d is empty", d.Thread)
		case strings.Contains(d.Text, magnumMarker):
			return fmt.Errorf("magnum fix --decide: the reply in thread %d carries a magnum marker, which would make it magnum's word, not yours", d.Thread)
		case len([]rune(d.Text)) > postreview.MaxBody:
			return fmt.Errorf("magnum fix --decide: the reply in thread %d is longer than GitHub takes (%d characters)", d.Thread, postreview.MaxBody)
		}
		seen[d.Thread] = true
	}
	return nil
}

// decideCheck answers `magnum fix --decide` on pr before anything is
// posted: the replies are refused as checkDecisions refuses them, and so is
// a PR that is not yours (the reply goes out as you), a loop that runs, a
// fix held for later, a drain for a restart (the reply would wait with the
// fix), a GitHub budget below the floor, and a thread in which the fixer
// left you no call that is still open (the other reviewers' items, read
// now: fixerCalls through readExternal). The reply goes out as your gh
// identity (decideIdentity).
func (e *Engine) decideCheck(ctx context.Context, repo store.Repo, pr store.PR, decide []FixDecision) (decidePlan, error) {
	if err := checkDecisions(decide); err != nil {
		return decidePlan{}, err
	}
	ref := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	author := deref(pr.AuthorLogin)
	switch {
	case author == "":
		return decidePlan{}, fmt.Errorf("PR #%d's author is not known yet (no details from GitHub); your reply goes out as you", pr.Number)
	case !e.ownPR(pr):
		return decidePlan{}, fmt.Errorf("PR #%d is %s's, not yours; your reply goes out as you", pr.Number, author)
	}
	switch st, running, err := e.fixState(ctx, pr.ID); {
	case err != nil:
		return decidePlan{}, err
	case running:
		return decidePlan{}, errors.New(fixRunningSentence(st))
	}
	if _, held := e.getKV(ctx, KVPRFixHeld(pr.ID)); held {
		return decidePlan{}, fmt.Errorf("a fix of PR #%d is held to start later: `magnum abort %s` takes it back first; nothing was posted", pr.Number, ref)
	}
	if e.draining(ctx) {
		return decidePlan{}, fmt.Errorf("the daemon drains for a restart: run `magnum fix --decide` on %s again after it; nothing was posted", ref)
	}
	w := e.cfg.WatchFor(repo.FullName())
	if w == nil {
		return decidePlan{}, fmt.Errorf("%s is not watched (no [[watch]] covers it)", repo.FullName())
	}
	login, client, err := e.decideIdentity(w, github.Account(author, deref(pr.AuthorType)))
	if err != nil {
		return decidePlan{}, err
	}
	if low, ok := e.githubLow(w.PollIdentity); ok {
		return decidePlan{}, fmt.Errorf("%s; nothing was posted: run it again once the budget recovers", low.text())
	}
	probe := &fixJob{repo: repo, watch: *w, pr: pr}
	if src := e.d.Identities[pr.Identity]; src != nil {
		probe.login = src.Login()
	}
	read, err := e.readExternal(ctx, probe)
	if err != nil {
		return decidePlan{}, fmt.Errorf("PR #%d: the other reviewers' threads could not be read (%s); nothing was posted", pr.Number, unreadCause(err))
	}
	plan := decidePlan{decide: decide, login: login, client: client, read: read, posted: map[int64]string{}}
	for _, d := range decide {
		i := slices.IndexFunc(read.items.Threads, func(t externalThread) bool { return t.CommentID == d.Thread })
		if i < 0 || !read.items.Threads[i].called {
			return decidePlan{}, fmt.Errorf("PR #%d: the fixer left you no call in thread %d (`magnum status %s` lists what it left); nothing was posted",
				pr.Number, d.Thread, ref)
		}
		if t := read.items.Threads[i]; t.Decision && len(t.Comments) > 0 {
			if body, _ := excerptOf(d.Text); t.Comments[len(t.Comments)-1].Body == body {
				plan.posted[d.Thread] = t.DecisionURL // this very reply is your decision there already
			}
		}
	}
	return plan, nil
}

// decideIdentity is the identity `magnum fix --decide` posts your reply
// as, by its login, with its client: the gh identity the watch's fix role
// uses (FixRoleFor: the role's own, fix_identity or the poll identity), or,
// when the fixer acts as an App, your operator identity of the watch
// (config.OperatorIdentity), when it is a gh identity of author's login (the
// PR's, Account form). A reply as anyone else would be no word of yours.
func (e *Engine) decideIdentity(w *config.Watch, author string) (string, threadReplier, error) {
	var names []string
	if role, ok := e.cfg.FixRoleFor(w); ok {
		names = append(names, role.Identity)
	}
	if id := e.cfg.OperatorIdentity(*w); id != nil {
		names = append(names, id.Name)
	}
	for _, name := range names {
		src := e.d.Identities[name]
		if src == nil || src.Kind() != "gh" || !github.SameAccount(src.Login(), author) {
			continue
		}
		if r, ok := e.gh(name).(threadReplier); ok {
			return src.Login(), r, nil
		}
	}
	return "", nil, fmt.Errorf("no gh identity of %s, the PR's author, to post your reply as (the fix role's and the watch's operator identity are someone else's); nothing was posted",
		author)
}

// decidePost posts plan's replies in their threads as plan's identity,
// each once (one already on GitHub is not posted again), with a
// fix.decided event that names the threads and the replies' links, never
// your words, and returns what the request's answer says of them ("replied
// in 1 thread as zhuravel: <link>"). A reply that fails ends the request
// with which ones went out before it, and no loop starts.
func (e *Engine) decidePost(ctx context.Context, repo store.Repo, pr store.PR, plan decidePlan) (string, error) {
	var urls []string
	var threads []int64
	for _, d := range plan.decide {
		url, ok := plan.posted[d.Thread]
		if !ok {
			r, err := plan.client.ReplyToReviewComment(ctx, repo.Owner, repo.Name, pr.Number, d.Thread, d.Text)
			if err != nil {
				before := ""
				if len(urls) > 0 {
					before = "; posted before it: " + strings.Join(urls, ", ")
				}
				return "", fmt.Errorf("your reply in thread %d could not be posted: %w%s; no loop started", d.Thread, err, before)
			}
			url = r.URL
		}
		urls, threads = append(urls, url), append(threads, d.Thread)
	}
	what := fmt.Sprintf("replied in %s as %s: %s", textx.Count(len(urls), "thread", "threads"), plan.login, strings.Join(urls, ", "))
	e.event(ctx, "info", prSubject(repo, pr.Number), "fix.decided", "you decided: "+what, map[string]any{"threads": threads, "by": fixDecideBy})
	return what, nil
}
