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
// or the won't-fix reply you chose; skills/magnum-fix/SKILL.md). A call in
// one of magnum's own threads (an author's call of its review, a decline of
// a blocking finding) is read the same way, and the file lists that thread
// under its decisions, since it is no other reviewer's. Only your
// reply counts, never another reviewer's, a bot's or the fixer's own (its
// replies carry the fix marker), and only after the fixer's call for that
// same thread: a fix-marked reply of the fixer's after the call closes it.
// The call is dated when its attention is raised (kvPRFixCalls), and a word
// the fixer may have written (an unmarked one of magnum's logins, or yours
// while a fix run was at work under a gh fixer: fixBusy) is no decision.
// A person's decision never closes on the fixer's word alone: after the run,
// a decision GitHub shows no reply of the fixer's in goes back under Needs
// you (decisionsLeft); a loop that built every decision its attention asked
// for (or left a call's P3 as an optional call, which is no decision of
// yours), and left nothing, lifts that attention (liftDecided); and a failed
// run or the cap keeps the decision calls of the attention it replaces
// (keepDecisions). `magnum fix --decide <thread>="<text>"` posts your reply
// as you and starts the loop in one step (decideCheck, decidePost). Under
// autofix = "open" a decision is new work for the look: the thread's key
// takes your reply, and an attention of decision calls you all decided stops
// no start (callsDecided).

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
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
// the reviewers' comments of it oldest first (externalOf's): whether the
// call is still open (callOpen), and the author's decision there, nil for
// none: while the call is open, the last word in the thread that the fixer
// cannot have written, when it is the author's own (ownWord: no magnum
// marker) and came after the call. The fixer may have written a word of
// one of magnum's logins that is not the author's, and, as busy says, one
// of the author's made while a fix run of the PR was at work (a gh fixer
// posts as the author, and a reply it posts without the fix marker cannot
// be told from theirs; fixBusy): those are skipped, so a decision made
// before them stands, and none of them is one.
func decisionIn(cs []github.ThreadComment, at time.Time, byAuthor, isOwn func(login, typ string) bool, busy func(time.Time) bool) (bool, *github.ThreadComment) {
	if !callOpen(cs, at, byAuthor, isOwn) {
		return false, nil
	}
	for i := len(cs) - 1; i > 0; i-- { // the first comment is the reviewer's
		c := cs[i]
		author := byAuthor(c.AuthorLogin, c.AuthorType)
		switch {
		case !author && isOwn(c.AuthorLogin, c.AuthorType), author && busy(c.CreatedAt):
			continue // the fixer's word, or maybe its
		case author && ownWord(c.Body) && c.CreatedAt.After(at):
			return true, &c
		}
		break
	}
	return true, nil
}

// callOpen reports whether the fixer's call made at at in a thread, cs its
// comments oldest first, is still open: no fix-marked reply of the fixer's
// (by the author, whom a gh fixer posts as, or by one of magnum's logins)
// came after it.
func callOpen(cs []github.ThreadComment, at time.Time, byAuthor, isOwn func(login, typ string) bool) bool {
	return !slices.ContainsFunc(cs, func(c github.ThreadComment) bool {
		return c.CreatedAt.After(at) && postreview.IsFixReply(c.Body) && (byAuthor(c.AuthorLogin, c.AuthorType) || isOwn(c.AuthorLogin, c.AuthorType))
	})
}

// fixBusy says whether a fix run of PR prID was at work at a time: after
// the run was made and until it ended (the later of its end and its
// verification), or at any time since it was made for one still at work.
// Runs that cannot be read make every time busy: a word that the fixer may
// have written is never a decision.
func (e *Engine) fixBusy(ctx context.Context, prID int64) func(time.Time) bool {
	runs, err := e.st.RunsByPR(ctx, prID)
	if err != nil {
		e.log.Warn("fix: the PR's fix runs, for the replies the fixer may have posted as the author", "pr", prID, "err", err)
		return func(time.Time) bool { return true }
	}
	type span struct{ from, to time.Time } // to zero: still at work
	var spans []span
	for _, r := range runs {
		if r.Kind != store.RunFix {
			continue
		}
		s := span{from: r.CreatedAt}
		for _, t := range []*time.Time{r.EndedAt, r.VerifiedAt} {
			if t != nil && t.After(s.to) {
				s.to = *t
			}
		}
		spans = append(spans, s)
	}
	return func(at time.Time) bool {
		return slices.ContainsFunc(spans, func(s span) bool { return at.After(s.from) && (s.to.IsZero() || !at.After(s.to)) })
	}
}

// kvPRFixCalls holds when the fixer made each call that the PR's attention
// of kind fix asks the author to decide (JSON: the id of each thread's
// first comment, then the time), written with that attention
// (fixAttention, recordFixCalls) and at the start of a loop over it
// (startFix): the report the calls came from is one per head, so a later
// run at the same head that fails after it wrote its own would otherwise
// date them at its end, and make a decision made before it an answer.
func kvPRFixCalls(prID int64) string { return fmt.Sprintf("pr.%d.fix_calls", prID) }

// fixerCalls are the calls the fixer left the author to decide, in other
// reviewers' threads and in magnum's own, that the loop's record still asks
// for (decisionThreads), by the
// id of each thread's first comment, with when each was made: as recorded
// (kvPRFixCalls), else at the time of the fix run whose report the
// attention names (an attention raised before the record); and the PR's
// optional calls, which raise no attention, at the time the settle took
// each (fix_optional.go). None while the PR needs nothing of the kind.
func (e *Engine) fixerCalls(ctx context.Context, pr store.PR) map[int64]time.Time {
	threads, made := e.decisionThreads(ctx, pr)
	calls := map[int64]time.Time{}
	if len(threads) > 0 {
		recorded := e.fixCallTimes(ctx, pr.ID)
		for _, id := range threads {
			switch at, ok := recorded[id]; {
			case ok:
				calls[id] = at
			case !made.IsZero():
				calls[id] = made
			}
		}
	}
	for _, c := range e.optionalCalls(ctx, pr.ID) {
		if _, ok := calls[c.Thread]; !ok && !c.At.IsZero() {
			calls[c.Thread] = c.At
		}
	}
	if len(calls) == 0 {
		return nil
	}
	return calls
}

// decisionThreads are the threads, by the id of each one's first comment,
// in which the PR's attention of kind fix, while it asks for the fixer's
// calls (attention.FixAsks), asks the author to decide a call the fixer left
// them: each call it names that asks for a decision (attention.DecisionCall,
// in magnum's thread or another reviewer's; one
// magnum put back for a decision the fixer did not build, decisionsLeft, or
// kept after a failure, keepDecisions, among them), and each entry of its
// report's "## Needs you" that it still names (FixNeedsAsked) and the fixer
// did not fix ("not fixed.", or neither word) in a thread the external file
// beside the report lists. made is the time of the fix run that wrote that
// report (verified, else ended; zero when the report or its run cannot be
// read): a call is made when the settle reads the report.
func (e *Engine) decisionThreads(ctx context.Context, pr store.PR) (threads []int64, made time.Time) {
	msg := deref(pr.LastError)
	if pr.State != store.PRNeedsAttention || !attention.FixAsks(msg) {
		return nil, time.Time{}
	}
	if kind, _ := e.getKV(ctx, KVPRAttention(pr.ID)); kind != attention.KindFix {
		return nil, time.Time{}
	}
	add := func(id string) {
		if n, err := strconv.ParseInt(id, 10, 64); err == nil && !slices.Contains(threads, n) {
			threads = append(threads, n)
		}
	}
	for _, c := range attention.Explain(attention.KindFix, msg, "").NeedsYou {
		if attention.DecisionCall(c) {
			add(c.Thread)
		}
	}
	report := attention.FixReport(msg)
	if report == "" {
		return threads, time.Time{}
	}
	b, err := os.ReadFile(report)
	if err != nil {
		return threads, time.Time{}
	}
	runID, _ := agents.ReportRun(b)
	if runID == "" {
		return threads, time.Time{}
	}
	made = e.fixRunTime(ctx, pr.ID, runID)
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
	return threads, made
}

// fixRunTime is when fix run runID of PR prID ended (verified, else
// ended): a call it left is made when its settle reads its report. Zero
// when the run cannot be read, is no fix run of that PR, or has not ended.
func (e *Engine) fixRunTime(ctx context.Context, prID int64, runID string) time.Time {
	if run, err := e.st.RunByID(ctx, runID); err == nil && run.PRID == prID && run.Kind == store.RunFix {
		if at := cmp.Or(run.VerifiedAt, run.EndedAt); at != nil {
			return *at
		}
	}
	return time.Time{}
}

// fixCallTimes is the PR's record of when the fixer made its calls
// (kvPRFixCalls); none when there is none or it cannot be read.
func (e *Engine) fixCallTimes(ctx context.Context, prID int64) map[int64]time.Time {
	raw, ok := e.getKV(ctx, kvPRFixCalls(prID))
	if !ok {
		return nil
	}
	var byID map[string]time.Time
	if json.Unmarshal([]byte(raw), &byID) != nil {
		return nil
	}
	calls := make(map[int64]time.Time, len(byID))
	for id, at := range byID {
		if n, err := strconv.ParseInt(id, 10, 64); err == nil {
			calls[n] = at
		}
	}
	return calls
}

// setFixCallTimes records calls as the PR's record of when the fixer made
// its calls (kvPRFixCalls), unless it holds them already; none removes it.
func (e *Engine) setFixCallTimes(ctx context.Context, prID int64, calls map[int64]time.Time) {
	if maps.EqualFunc(calls, e.fixCallTimes(ctx, prID), time.Time.Equal) {
		return
	}
	if len(calls) == 0 {
		e.delKV(ctx, kvPRFixCalls(prID))
		return
	}
	byID := make(map[string]time.Time, len(calls))
	for id, at := range calls {
		byID[strconv.FormatInt(id, 10)] = at.UTC()
	}
	b, err := json.Marshal(byID)
	if err != nil {
		return
	}
	e.setKV(ctx, kvPRFixCalls(prID), string(b))
}

// recordFixCalls records when the fixer made each decision call of msg, the
// attention a fix job just raised on PR prID (fixAttention): a call kept
// from the attention it replaced (keepDecisions) at the time recorded for
// it there (kept), any other at the time of the run whose report msg names.
// A raise that did not take (the PR moved on) records nothing.
func (e *Engine) recordFixCalls(ctx context.Context, prID int64, msg string, kept map[int64]time.Time) {
	pr, err := e.st.PRByID(ctx, prID)
	if err != nil || pr.State != store.PRNeedsAttention || deref(pr.LastError) != msg {
		return
	}
	threads, made := e.decisionThreads(ctx, pr)
	calls := map[int64]time.Time{}
	for _, id := range threads {
		switch at, ok := kept[id]; {
		case ok:
			calls[id] = at
		case !made.IsZero():
			calls[id] = made
		}
	}
	e.setFixCallTimes(ctx, prID, calls)
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
	decided := x.decisions()
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
// the fixer's decision calls alone and this run covered every one: it built
// the call's decision (built, decisionsLeft: you decided the call, and
// GitHub shows the fixer's reply that builds it), or it left the call's
// thread as an optional call (optional, settleOptional: magnum's own
// comment shows a P3 whose Fix ends with the author's call, which holds
// nothing; a fixer from before optional calls left such a P3 as a call to
// decide). An optional call closes no decision of yours: one you made in
// its thread is in the run's external file, and decisionsLeft put it back
// under Needs you unless the fixer built it; so a run without that file (the
// other reviewers' items unread: no decision was read) counts no optional
// call here. The compare-and-set holds the head, the reviewed head and the
// attention's words, as `magnum fix --done` does; the attention's rows go,
// and a fix.decisions_built event says so, or fix.optional_lifted when an
// optional call covered a call.
func (e *Engine) liftDecided(ctx context.Context, job *fixJob, built, optional map[int64]bool) {
	if len(optional) > 0 {
		if _, ok := readExternalFile(filepath.Dir(deref(job.run.ReportPath))); !ok {
			optional = nil
		}
	}
	if len(built) == 0 && len(optional) == 0 {
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
	left := 0 // the attention's calls the run left as optional calls, not built
	if !callsDecided(msg, func(id int64) bool {
		if !built[id] && optional[id] {
			left++
		}
		return built[id] || optional[id]
	}) {
		return
	}
	err = e.st.TransitionPR(ctx, pr.ID, []string{store.PRNeedsAttention}, store.PRReviewed, func(u *store.PRUpdate) {
		u.Set("last_error", nil)
		u.Where("head_sha", pr.HeadSHA)
		u.Where("reviewed_sha", pr.HeadSHA)
		u.Where("last_error", msg)
	})
	if err != nil {
		e.log.Info("fix: the PR moved on before the lift of the calls the fixer built or left optional", "subject", job.subject(), "err", err)
		return
	}
	e.delKV(ctx, KVPRAttention(pr.ID), kvPRAttentionRun(pr.ID), kvPRFixCalls(pr.ID))
	kind, did := "fix.decisions_built", job.role.Name+" built every decision you made"
	if left > 0 {
		kind, did = "fix.optional_lifted", fmt.Sprintf("%s left %s of the attention as %s, which %s nothing before the merge", job.role.Name,
			textx.Count(left, "call", "calls"), textx.Plural(left, "an optional call", "optional calls"), textx.Plural(left, "holds", "hold"))
		if len(built) > 0 {
			did = job.role.Name + " built every decision you made and" + strings.TrimPrefix(did, job.role.Name)
		}
	}
	e.event(ctx, "info", job.subject(), kind, fmt.Sprintf("%s: the review of %s stands, with no new round", did, textx.ShortSHA(pr.HeadSHA)), job.eventData())
}

// keepDecisions is msg, the attention a fix job raises over old, the
// PR's attention it replaces, with old's decision calls (attention.DecisionCall)
// that open says are still open after msg's own words, when msg asks for no
// call of its own (a failed run, the cap): a person's decision never closes
// on a failure, and the next `magnum fix` finds those calls still open
// (fixerCalls, at the time recorded for them: recordFixCalls) and builds
// the decisions made on them. A call GitHub shows the fixer built (callsOpen)
// is done and goes. A message with calls of its own is the fixer's last
// word on them (decisionsLeft put back the ones it did not build), and
// stays as it is.
func keepDecisions(old, msg string, open func(thread string) bool) string {
	if !attention.FixAsks(old) || attention.FixAsks(msg) {
		return msg
	}
	var keep []attention.FixCall
	for _, c := range attention.Explain(attention.KindFix, old, "").NeedsYou {
		if attention.DecisionCall(c) && open(c.Thread) {
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

// callsOpen says whether the fixer's call in a thread (by its first
// comment's id) is still open on GitHub (callOpen): no fix-marked reply of
// the fixer's came there since it was made (calls; fixerCalls). The job's
// PR's threads are read once, when first asked. A call magnum has no time
// for, a thread the read does not list and a read that fails count as open:
// a person's decision never closes on a guess.
func (e *Engine) callsOpen(ctx context.Context, job *fixJob, calls map[int64]time.Time) func(thread string) bool {
	read := e.threadsReader(ctx, job)
	author := github.Account(deref(job.pr.AuthorLogin), deref(job.pr.AuthorType))
	byAuthor := func(login, typ string) bool { return github.SameAccount(github.Account(login, typ), author) }
	own := sync.OnceValue(func() []string { return e.fixLogins(ctx, job) })
	isOwn := func(login, typ string) bool {
		a := github.Account(login, typ)
		return slices.ContainsFunc(own(), func(o string) bool { return github.SameAccount(o, a) })
	}
	return func(thread string) bool {
		id, err := strconv.ParseInt(thread, 10, 64)
		at, ok := calls[id]
		if err != nil || !ok {
			return true
		}
		threads, err := read()
		if err != nil {
			return true
		}
		for _, t := range threads {
			if all := t.AllComments(); len(all) > 0 && all[0].ID == id {
				return callOpen(all, at, byAuthor, isOwn)
			}
		}
		return true
	}
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
// left you no call that is still open (another reviewer's or magnum's own,
// read now: fixerCalls through readExternal). The reply goes out as your
// gh identity (decideIdentity).
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
		if (i < 0 || !read.items.Threads[i].called) && !read.items.called[d.Thread] {
			return decidePlan{}, fmt.Errorf("PR #%d: the fixer left you no call in thread %d (`magnum status %s` lists what it left); nothing was posted",
				pr.Number, d.Thread, ref)
		}
		if j := slices.IndexFunc(read.items.decisions(), func(t externalThread) bool { return t.CommentID == d.Thread }); j >= 0 {
			t := read.items.decisions()[j]
			body, _ := excerptOf(d.Text)
			if slices.ContainsFunc(t.Comments, func(c externalComment) bool { return c.URL == t.DecisionURL && c.Body == body }) {
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
