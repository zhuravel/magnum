package engine

// The fix job's end (fix.go runs it): the settle reads the fixer's report,
// puts the slot back in order (Slots.Resync at the pushed head, or at the
// reviewed one when nothing was pushed, so the next round's guard sees no
// drift) and ends the cycle: a push goes on to a forced re-review (phase
// reviewing; findings the fixer left to the author are toasted at once and
// wait in the loop's row for that round's end), anything else ends the
// loop, with an info toast when the fixer answered everything without a
// push and as needs attention (attention.KindFix) otherwise. A stop
// (`magnum abort`) ends the loop quietly.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/steps"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// kindFixEnded counts the fix loops that ended without the operator in a
// batched toast.
var kindFixEnded = notify.Kind{One: "fix loop ended", Many: "fix loops ended"}

// fixThreadsRe reads the thread count off a report's "## Replied" line
// ("3 threads (2 fixed, ...)").
var fixThreadsRe = regexp.MustCompile(`^(\d+)\s+threads?\b`)

// settleFix ends the cycle of a fixer whose run ended. Its step (fix.settle)
// puts the slot back in order: at the head the fixer pushed (pushedHead)
// when its report lists commits, else (or when that head cannot be
// confirmed, which fails the job) at the reviewed head; a slot that cannot
// be put back fails the job. Then the report decides: none, one of
// another run or a broken one, or a failed push, fails the job; a push goes
// on to the re-review (fixReReview), with the findings left to the author
// kept for that round's end (spec: the push deserves its re-review);
// findings left to the author without a push end the loop as needs
// attention (a fix the fixer left as unanswered in another reviewer's
// thread that GitHub shows its reply in is none: dropFixerAnswered);
// replies alone end the loop with an info toast. The events name counts,
// SHAs, the role and the report, never what the fixer wrote.
func (e *Engine) settleFix(ctx context.Context, job *fixJob) {
	ctx = github.WithOp(ctx, opFixSettle) // github_cost.go
	subject, report := job.subject(), deref(job.run.ReportPath)
	rep, rerr := e.readFixReport(job)
	pushed := rerr == nil && len(rep.Pushed) > 0 && !rep.PushFailed
	target := job.head
	var headErr error
	err := steps.Step(ctx, e.st, subject, "fix.settle", func(ctx context.Context) error {
		if pushed {
			if target, headErr = e.pushedHead(ctx, job, rep); headErr != nil {
				target = job.head
			}
		}
		return e.resyncFixSlot(ctx, job, target)
	})
	switch {
	case err != nil:
		e.failFix(ctx, job, "fix settle failed: "+err.Error(), "")
		return
	case headErr != nil:
		e.failFix(ctx, job, "fix settle failed: "+headErr.Error(), "")
		return
	case rerr != nil:
		e.failFix(ctx, job, "fix run failed: "+rerr.Error(), "")
		return
	case rep.PushFailed:
		e.failFix(ctx, job, "fix run failed: the fixer's push failed: "+report, "")
		return
	}
	data := job.eventData()
	described := len(rep.Description) > 0 // rerr is nil here
	if described {
		data["description"] = len(rep.Description)
		e.event(ctx, "info", subject, "fix.description", fmt.Sprintf("%s updated the PR description (%s)", job.role.Name,
			textx.Count(len(rep.Description), "change", "changes")), data)
	}
	if pushed {
		data["head"], data["commits"] = target, len(rep.Pushed)
		e.event(ctx, "info", subject, "fix.pushed", fmt.Sprintf("%s pushed %s: the head is %s", job.role.Name,
			textx.Count(len(rep.Pushed), "commit", "commits"), textx.ShortSHA(target)), data)
	}
	threads, counted := fixThreads(rep.Replied)
	replied := "replied"
	if counted {
		replied, data["threads"] = "replied in "+textx.Count(threads, "thread", "threads"), threads
	}
	unread := job.othersUnread() // fix_external.go: what the fixer did not get of the other reviewers
	if unread != "" {
		replied += "; " + unread
	}
	e.event(ctx, "info", subject, "fix.replied", job.role.Name+" "+replied, data)
	if len(rep.NeedsYou) > 0 {
		rep.NeedsYou = e.dropFixerAnswered(ctx, job, rep.NeedsYou) // fix_external.go
	}
	n := len(rep.NeedsYou)
	needs := "fix: " + needsYouText(rep.NeedsYou, report, job.pr.URL, job.label(), false)
	attn := withUnread(needs, unread)
	if n > 0 {
		data["needs_you"] = n
	}
	switch {
	case pushed && n > 0:
		e.event(ctx, "warn", subject, "fix.needs_you", needs+"; the re-review of "+textx.ShortSHA(target)+" runs first", data)
		e.finishFixRun(ctx, job.run, store.RunVerified, "pushed", "")
		if e.fixReReview(ctx, job, target, rep.NeedsYou) {
			e.fixCallToast(job, attn) // the operator can act on it while the re-review runs
		}
	case pushed:
		e.finishFixRun(ctx, job.run, store.RunVerified, "pushed", "")
		e.fixReReview(ctx, job, target, nil)
	case n > 0:
		e.finishFixRun(ctx, job.run, store.RunVerified, "needs_you", "")
		e.endFixState(ctx, job)
		e.event(ctx, "warn", subject, "fix.needs_you", needs, data)
		e.fixAttention(ctx, job, attn)
	default:
		e.finishFixRun(ctx, job.run, store.RunVerified, "replied", "")
		e.endFixState(ctx, job)
		answered := "the findings"
		switch {
		case job.alone && counted: // a review that found nothing: the fixer answered the other reviewers alone
			answered = textx.Count(threads, "thread or comment", "threads and comments") + " of other reviewers"
		case job.alone:
			answered = "the other reviewers"
		case counted:
			answered = textx.Count(threads, "finding", "findings")
		}
		did := "pushed nothing"
		if described {
			did = "updated the description"
		}
		what := fmt.Sprintf("PR #%d: %s %s, replied to %s", job.pr.Number, job.role.Name, did, answered)
		body, line := job.pr.URL+"\nthe fix loop ends; the report: "+report, what
		if unread != "" {
			body, line = body+"\n"+unread, line+"; "+unread
		}
		e.info(notify.Item{Key: "fix-replied:" + job.run.ID, Title: "magnum: " + what, Body: body, Line: line + " " + job.pr.URL, Kind: kindFixEnded})
	}
}

// fixReReview takes the loop to its re-review after a push: the row in
// phase reviewing (the cycle counted, the pushed head, needs the findings
// the fixer left to the author, none after a clean push, and when the loop
// forces its re-review, by which the hook tells that round from the
// others: FixState.ForcedAt), the push on the
// PR row as the poller records one (recordFixPush), and a forced review of
// the PR on it (forceReview, as `magnum review` queues one: no throttle, no
// daily cap, and the fixing hold does not apply to it). The PR is forced
// before onHeadChange sees the push, so the loop's re-review is never taken
// for a push the review covers already (settlePush). A force that fails
// after the head moved still lets onHeadChange see the push before the
// loop ends, so the push is recorded and its re-review queued the normal
// way (the poller, which finds the head already moved, would never queue
// it). A PR GitHub closed meanwhile ends the loop instead. It reports whether
// the loop goes on to its re-review (false: the loop ended, closed or failed).
func (e *Engine) fixReReview(ctx context.Context, job *fixJob, head string, needs []string) bool {
	pr, err := e.st.PRByID(ctx, job.pr.ID)
	if err == nil && pr.GHState != store.GHOpen {
		e.endFixState(ctx, job)
		e.event(ctx, "info", job.subject(), "fix.ended", fmt.Sprintf("the fix loop ends: the PR is %s on GitHub", strings.ToLower(pr.GHState)), job.eventData())
		return false
	}
	st := job.state
	st.Phase, st.Head, st.NeedsYou, st.ForcedAt, st.Held = FixPhaseReviewing, head, needs, e.now().UTC(), false
	if err == nil {
		err = e.setFixState(ctx, pr.ID, st)
	}
	moved := false
	if err == nil {
		pr, moved, err = e.recordFixPush(ctx, job, pr, head)
	}
	if err == nil {
		_, err = e.forceReview(ctx, job.repo, pr, ReviewPayload{}, nil, fixBy)
	}
	if moved {
		e.seeFixPush(ctx, job, pr.ID)
	}
	if err != nil {
		e.failFix(ctx, job, "fix settle failed: the re-review could not be queued: "+err.Error(), "")
		return false
	}
	job.state = st
	return true
}

// seeFixPush lets onHeadChange see the push recordFixPush put on the PR row,
// as the poller would have: the push recorded for the burst rules,
// pr.head_changed, the approval followed, the PR requeued (a forced one due
// now, else as the throttle says).
func (e *Engine) seeFixPush(ctx context.Context, job *fixJob, prID int64) {
	if cur, err := e.st.PRByID(ctx, prID); err != nil {
		e.log.Warn("fix: the PR after its push", "subject", job.subject(), "err", err)
	} else if err := e.onHeadChange(ctx, job.repo, job.watch, cur, e.now()); err != nil {
		e.log.Warn("fix: the push's head change", "subject", job.subject(), "err", err)
	}
}

// recordFixPush moves the PR row's head to the head the fixer pushed the way
// the poller records a push (store.UpsertPRFromGitHub: head_sha,
// head_changed_at and the activity time), so the forced re-review targets
// it and the poller's own sight of it later changes nothing. A row whose
// head is no longer the reviewed one (the poller saw this push already, or
// a newer one) is left as it is. It returns the PR and whether its head
// moved; the caller lets onHeadChange see the push.
func (e *Engine) recordFixPush(ctx context.Context, job *fixJob, pr store.PR, head string) (store.PR, bool, error) {
	if pr.HeadSHA != job.head || head == pr.HeadSHA {
		return pr, false, nil
	}
	res, err := e.st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: pr.RepoID, NodeID: pr.NodeID, Number: pr.Number, URL: pr.URL,
		HeadSHA: head, IsDraft: pr.IsDraft})
	if err != nil {
		return pr, false, err
	}
	return res.PR, res.HeadChanged, nil
}

// eventData is the data every fix event carries: the run, the role, the
// cycle and the report.
func (j *fixJob) eventData() map[string]any {
	data := map[string]any{"run": j.run.ID, "role": j.role.Name, "cycle": j.state.Cycle, "cap": j.state.Cap, "report": deref(j.run.ReportPath)}
	if j.state.OthersUnread != "" {
		data["others_unread"] = j.state.OthersUnread
	}
	return data
}

// withUnread is the attention message msg of a loop whose fixer did not get
// all the other reviewers' items, with unread (fixJob.othersUnread) on a
// line of its own: the reason's first line, which attention.Explain reads
// for the calls and the remedy, stays msg, and unread shows under it.
func withUnread(msg, unread string) string {
	if unread == "" {
		return msg
	}
	return msg + "\n" + unread
}

// readFixReport reads the fixer's report of the job's run. Its errors name
// the path and what is wrong, never a line of it: the fixer's text may
// carry the PR's.
func (e *Engine) readFixReport(job *fixJob) (FixReport, error) {
	path := deref(job.run.ReportPath)
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return FixReport{}, fmt.Errorf("%s wrote no report at %s", job.role.Name, path)
	case err != nil:
		return FixReport{}, err
	}
	rep, err := ParseFixReport(string(b), job.run.ID)
	switch {
	case err == nil:
		return rep, nil
	case errors.Is(err, ErrFixReportMarker), errors.Is(err, ErrFixReportPartial):
		return FixReport{}, fmt.Errorf("%w: %s", err, path)
	}
	return FixReport{}, fmt.Errorf("the report at %s has a line that does not fit the fix skill's format", path)
}

// pushedHead is the head the fixer pushed: the slot's HEAD, which `git push
// origin HEAD:<branch>` sent, once it is one of the commits the report lists
// (the report gives short shas; Resync needs the whole one) and GitHub shows
// it as the PR's head (confirmPush): the fixer's word alone would record a
// head GitHub lacks on the PR row and force a review of it.
func (e *Engine) pushedHead(ctx context.Context, job *fixJob, rep FixReport) (string, error) {
	if e.d.Git == nil {
		return "", errors.New("no git to read the checkout's HEAD")
	}
	head, err := e.d.Git.RevParse(ctx, job.slot.Path, "HEAD")
	if err != nil {
		return "", fmt.Errorf("the checkout's HEAD: %w", err)
	}
	if !slices.ContainsFunc(rep.Pushed, func(c FixCommit) bool { return strings.HasPrefix(head, c.SHA) }) {
		return "", fmt.Errorf("the checkout's HEAD %s is none of the %s the report lists", textx.ShortSHA(head),
			textx.Count(len(rep.Pushed), "commit", "commits"))
	}
	if err := e.confirmPush(ctx, job, head); err != nil {
		return "", err
	}
	return head, nil
}

const (
	// fixPushLooks bounds how often confirmPush looks at the PR's head on
	// GitHub, fixPushWait apart: GitHub moves a PR's head a moment after
	// the push to its branch.
	fixPushLooks = 3
	fixPushWait  = 5 * time.Second
	// fixLsRemoteTimeout bounds the `git ls-remote` confirmPush falls back on.
	fixLsRemoteTimeout = 30 * time.Second
)

// remoteLister is the git capability confirmPush falls back on
// (*gitx.Client has it): `git ls-remote origin` in a checkout.
type remoteLister interface {
	LsRemote(ctx context.Context, dir string, timeout time.Duration, patterns ...string) (string, error)
}

// confirmPush checks that GitHub has head, the commit the fixer says it
// pushed, as the head of the PR's branch: the PR's head as the poll
// identity's client reads it (Details), else, when that client cannot say,
// the branch on origin (`git ls-remote` in the slot). A head GitHub shows
// otherwise is looked at again fixPushWait later, up to fixPushLooks times.
// The error ("push not confirmed on GitHub: ...") names the shas, never the
// fixer's words.
func (e *Engine) confirmPush(ctx context.Context, job *fixJob, head string) error {
	var last string
	var lastErr error
	for i := range fixPushLooks {
		if i > 0 {
			if err := e.d.Sleep(ctx, fixPushWait); err != nil {
				return err
			}
		}
		got, err := e.remoteHead(ctx, job)
		switch {
		case err != nil:
			lastErr = err
			continue
		case got == head:
			return nil
		}
		last, lastErr = got, nil
	}
	if lastErr != nil {
		return fmt.Errorf("push not confirmed on GitHub: %w", lastErr)
	}
	return fmt.Errorf("push not confirmed on GitHub: the PR's head is %s, not the pushed %s", textx.ShortSHA(last), textx.ShortSHA(head))
}

// remoteHead is the head of the job's PR on GitHub: the poll identity's
// client's answer, else the branch's tip on origin.
func (e *Engine) remoteHead(ctx context.Context, job *fixJob) (string, error) {
	var why error
	if gh := e.gh(job.watch.PollIdentity); gh == nil {
		why = fmt.Errorf("no GitHub client for poll identity %q", job.watch.PollIdentity)
	} else if det, _, err := gh.Details(ctx, job.repo.Owner, job.repo.Name, []int{job.pr.Number}); err != nil {
		why = err
	} else if d, ok := det[job.pr.Number]; !ok || d.HeadRefOid == "" {
		why = errors.New("GitHub does not say where the PR's head is")
	} else {
		return d.HeadRefOid, nil
	}
	branch := deref(job.pr.HeadRef)
	git, ok := e.d.Git.(remoteLister)
	if !ok || branch == "" {
		return "", why
	}
	out, err := git.LsRemote(ctx, job.slot.Path, fixLsRemoteTimeout, "refs/heads/"+branch)
	if err != nil {
		return "", errors.Join(why, fmt.Errorf("git ls-remote: %w", err))
	}
	for line := range strings.Lines(out) {
		if sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok && ref == "refs/heads/"+branch {
			return sha, nil
		}
	}
	return "", errors.Join(why, fmt.Errorf("origin has no branch %s", branch))
}

// resyncFixSlot puts the job's slot back in order at sha (Slots.Resync:
// detached, recorded as checked out), once the slot is still claimed or
// held for the PR.
func (e *Engine) resyncFixSlot(ctx context.Context, job *fixJob, sha string) error {
	return e.resyncSlot(ctx, job.pr.ID, job.slot, sha)
}

// resyncSlot puts slot, PR prID's, back in order at sha (Slots.Resync), once
// it is still claimed or held for the PR: the fix and whiteboard jobs'.
func (e *Engine) resyncSlot(ctx context.Context, prID int64, of store.Slot, sha string) error {
	slot, err := e.st.SlotByID(ctx, of.ID)
	if err != nil {
		return err
	}
	if deref(slot.PRID) != prID || (slot.State != store.SlotClaimed && slot.State != store.SlotHeld) {
		return fmt.Errorf("slot %s is no longer the PR's (%s)", slot.Name, slot.State)
	}
	return e.d.Slots.Resync(ctx, slot, sha)
}

// fixThreads reads the thread count off a "## Replied" line (ok false when
// it does not start with one).
func fixThreads(line string) (int, bool) {
	m := fixThreadsRe.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// failFix ends the job's loop as failed: its run fails (one still active),
// the loop's row goes, a fix.failed event names msg and the report (report,
// "" = the run's), and the operator is told (fixAttention).
func (e *Engine) failFix(ctx context.Context, job *fixJob, msg, report string) {
	if job.run.ID != "" {
		e.finishFixRun(ctx, job.run, store.RunFailed, "failed", msg)
	}
	e.endFixState(ctx, job)
	data := job.eventData()
	if report != "" {
		data["report"] = report
	}
	e.event(ctx, "warn", job.subject(), "fix.failed", msg, data)
	e.fixAttention(ctx, job, msg)
}

// endFixState ends the job's loop: its row goes.
func (e *Engine) endFixState(ctx context.Context, job *fixJob) {
	if err := e.clearFixState(ctx, job.pr.ID); err != nil {
		e.log.Warn("fix: clear the loop's row", "subject", job.subject(), "err", err)
	}
}

// fixAttention tells the operator that the job's loop needs them, with msg,
// as the fix job's attention (attention.KindFix: the fixer, its report and
// `magnum fix` or `magnum abort` as the next step). A reviewed PR goes to
// needs_attention (needsAttention: its toast and attention key), and so
// does one already there, a second loop's (`magnum fix` once the author
// answered the first): its message replaces the first loop's. A PR waiting
// for a round keeps it, because a push came during the fix and that
// re-review still runs: it gets the urgent toast alone. A PR elsewhere
// (closed, ineligible) has the events only. The toast's dedupe key is the
// loop's cycle's own (fixAttentionKey), so each loop that needs the author
// toasts and a cycle's call is told once, however late the loop ends.
func (e *Engine) fixAttention(ctx context.Context, job *fixJob, msg string) {
	pr, err := e.st.PRByID(ctx, job.pr.ID)
	if err != nil {
		e.log.Warn("fix: the PR is gone", "subject", job.subject(), "err", err)
		return
	}
	rj := job.asRound()
	rj.pr = pr
	switch {
	case pr.State == store.PRReviewed || pr.State == store.PRNeedsAttention:
		e.needsAttentionKeyed(ctx, rj, pr, []string{store.PRReviewed, store.PRNeedsAttention}, attention.KindFix, msg, nil,
			fixAttentionKey(job), fixAttentionWindow)
	case slices.Contains(waitingStates, pr.State):
		e.fixCallToast(job, msg)
	}
}

// fixAttentionWindow is how long a loop cycle's attention toast (its key,
// fixAttentionKey) holds back a second one: a day, since the key is the
// cycle's own and a re-review can wait for hours before the loop's end asks
// for the same call again.
const fixAttentionWindow = 24 * time.Hour

// fixAttentionKey is the dedupe key of the toast of the job's loop cycle:
// the PR, the loop (when it started) and the cycle. The loop's end after a
// re-review (loopJob, which has no run) builds the key of the settle that
// started that re-review, so a call toasted at once is not toasted again.
func fixAttentionKey(job *fixJob) string {
	return fmt.Sprintf("attention:%d:%s:%s:%d", job.pr.ID, attention.KindFix, store.FormatTime(job.state.StartedAt), job.state.Cycle)
}

// fixCallToast is the fix job's urgent toast alone (no state change): msg,
// as the fix job's attention, for the PR the job's loop is on. The settle
// raises it when the fixer pushed and left findings to the author, so they
// can act on them while the push is re-reviewed; fixAttention raises it for
// a PR waiting for a round.
func (e *Engine) fixCallToast(job *fixJob, msg string) {
	label := fmt.Sprintf("%s#%d", job.repo.Name, job.pr.Number)
	e.urgent(fixAttentionKey(job), "magnum: "+label+" needs attention", attentionToast(attention.Explain(attention.KindFix, msg, label)), fixAttentionWindow)
}

// finishFixRun moves the fixer's run to to (verified or failed) with
// outcome and errMsg; a run already final keeps its state and outcome.
func (e *Engine) finishFixRun(ctx context.Context, run store.Run, to, outcome, errMsg string) {
	from := []string{store.RunPending, store.RunSubmitted, store.RunWorking, store.RunEnded}
	if to == store.RunVerified {
		from = from[1:]
	}
	now := e.now()
	err := e.st.TransitionRun(context.WithoutCancel(ctx), run.ID, from, to, func(u *store.RunUpdate) {
		u.Set("outcome", outcome)
		if errMsg != "" {
			u.Set("error", oneLine(errMsg, 300))
		}
		if to == store.RunVerified {
			u.Set("verified_at", now)
		} else if run.EndedAt == nil {
			u.Set("ended_at", now)
		}
	})
	if err != nil && !errors.Is(err, store.ErrConflict) {
		e.log.Warn("fix: finish run", "run", run.ID, "to", to, "err", err)
	}
}

// stopFix serves the abort and ignore orders that stopped the job: the
// fixer is interrupted and its run abandoned, the slot is put back at the
// reviewed head when the fixer may have touched it, and the loop ends
// quietly (its row goes, an info toast, no attention); an ignore also mutes
// the PR as `magnum ignore` does. The orders are completed with what
// happened.
func (e *Engine) stopFix(ctx context.Context, job *fixJob, orders []stopOrder) {
	var done []string
	var problems []error
	if job.run.ID != "" {
		if e.interruptSessions(ctx, job.pr, &job.watch, job.role.Name) > 0 {
			done = append(done, "fixer interrupted")
			if !e.waitAgentsQuiet(ctx, job.pr.ID) {
				e.log.Info("the fixer still works after the interrupt", "subject", job.subject())
			}
		}
		if e.abandonFixRun(ctx, job.run, abortedBy) {
			done = append(done, "run "+job.run.ID+" abandoned")
		}
	}
	if job.touched {
		if err := e.resyncFixSlot(ctx, job, job.head); err != nil {
			problems = append(problems, fmt.Errorf("slot %s not put back: %w", job.slot.Name, err))
		} else {
			done = append(done, "slot "+job.slot.Name+" back at "+textx.ShortSHA(job.head))
		}
	}
	e.endFixState(ctx, job)
	ignore := slices.ContainsFunc(orders, func(o stopOrder) bool { return o.ignore })
	what, kind := "aborted", "pr.aborted"
	if ignore {
		what, kind = "ignored", "pr.ignored"
		if to, err := e.settleStopped(ctx, job.pr, true); err != nil {
			problems = append(problems, err)
		} else if to != "" {
			done = append(done, "PR "+to)
		}
	}
	res := what + " the fix of " + job.label()
	if len(done) > 0 {
		res += ": " + strings.Join(done, ", ")
	}
	if ignore {
		res += "; muted until `magnum unmute " + job.label() + "`"
	}
	perr := errors.Join(problems...)
	level := "info"
	if perr != nil {
		level, res = "warn", res+"; "+perr.Error()
	}
	data := job.eventData()
	data["fix"] = true
	e.event(ctx, level, job.subject(), kind, res, data)
	e.toastFixStopped(job.repo, job.pr, job.state, res)
	for _, o := range orders {
		if perr != nil {
			e.complete(ctx, o.id, errors.New(res), "")
		} else {
			e.complete(ctx, o.id, nil, res)
		}
		e.mu.Lock()
		delete(e.inflight, o.id)
		e.mu.Unlock()
	}
}

// endFixLoop ends a fix loop that no fix job holds (one waiting for its
// re-review, or the row of a stopped daemon) for a stop of the PR that holds
// its reservation: its row goes, with an event and an info toast. The row
// goes only while it is the one read here (Store.DeleteKVIf; claimFixRow is
// the other side), and an abort without the reservation goes through
// abortFixLoop, which hands a fix job that claimed the PR meanwhile the
// stop instead. A PR without a loop has its held fix (a drain's, the
// GitHub budget's or the fixer's pause's) taken back instead (dropHeldFix).
// It returns what it did for the
// stop's answer ("" = the PR had neither, or a cycle took the loop on
// meanwhile).
func (e *Engine) endFixLoop(ctx context.Context, repo store.Repo, pr store.PR, by string) string {
	st, raw, _, err := e.fixRow(ctx, pr.ID)
	if note := e.endFixLoopRow(ctx, repo, pr, st, raw, err, by); note != "" {
		return note
	}
	return e.dropHeldFix(ctx, repo, pr, by) // fix_held.go: a held fix, which no loop runs yet
}

// endFixLoopRow is endFixLoop for the row the caller read: st, raw as stored
// ("" = none) and readErr, the error of a row that cannot be read.
func (e *Engine) endFixLoopRow(ctx context.Context, repo store.Repo, pr store.PR, st FixState, raw string, readErr error, by string) string {
	if raw == "" {
		return ""
	}
	if gone, cerr := e.st.DeleteKVIf(ctx, KVPRFix(pr.ID), raw); cerr != nil || !gone {
		if cerr != nil {
			e.log.Warn("stop: clear the fix loop's row", "pr", pr.ID, "err", cerr)
		}
		return ""
	}
	note := fmt.Sprintf("fix loop ended (cycle %d of %d, %s)", st.Cycle, st.Cap, st.Phase)
	if readErr != nil {
		note = "the fix loop's unreadable row cleared"
	}
	e.event(ctx, "info", prSubject(repo, pr.Number), "fix.stopped", note+" by "+by, map[string]any{"by": by, "report": st.Report})
	e.toastFixStopped(repo, pr, st, note)
	return note
}

// abortFixLoopTries bounds how often abortFixLoop looks at a loop that keeps
// changing under it.
const abortFixLoopTries = 3

// abortFixLoop serves `magnum abort` (request id) for the fix loop of a PR
// no reservation held when the abort looked. The hook after a round can
// reserve the PR and claim the loop's next cycle meanwhile, so the abort
// reads the row, then looks at the reservation again: a fix job holding the
// PR gets the abort as its stop order (stopHeld: its fixer interrupted, the
// loop ended quietly by stopFix), never a bare delete of the row the job
// works under; with no reservation, the row read goes (endFixLoopRow, a
// compare-and-set), and a row that changed since it was read (a cycle took
// the loop on) is looked at again. A PR without a loop has its held fix
// taken back (dropHeldFix). It reports whether it
// answered the request (false: the PR has neither).
func (e *Engine) abortFixLoop(ctx context.Context, id int64, repo store.Repo, pr store.PR) bool {
	label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	for range abortFixLoopTries {
		st, raw, _, err := e.fixRow(ctx, pr.ID)
		if raw == "" {
			if note := e.dropHeldFix(ctx, repo, pr, "magnum abort"); note != "" {
				e.complete(ctx, id, nil, "aborted "+label+": "+note)
				return true
			}
			return false
		}
		if e.stopHeld(ctx, id, repo, pr, false) {
			return true
		}
		if note := e.endFixLoopRow(ctx, repo, pr, st, raw, err, "magnum abort"); note != "" {
			e.complete(ctx, id, nil, "aborted "+label+": "+note)
			return true
		}
	}
	e.complete(ctx, id, fmt.Errorf("%s: its fix loop changed while the abort looked at it; run `magnum abort %s` again", label, label), "")
	return true
}

// toastFixStopped is the info toast of a fix loop a stop ended (st: its
// row), with what the stop did.
func (e *Engine) toastFixStopped(repo store.Repo, pr store.PR, st FixState, what string) {
	title := fmt.Sprintf("magnum: fix stopped: %s#%d", repo.Name, pr.Number)
	e.info(notify.Item{Key: fmt.Sprintf("fix-stopped:%d:%s:%d", pr.ID, store.FormatTime(st.StartedAt), st.Cycle), Title: title,
		Body: pr.URL + "\n" + what, Line: strings.TrimPrefix(title, "magnum: ") + " " + pr.URL, Kind: kindFixEnded})
}
