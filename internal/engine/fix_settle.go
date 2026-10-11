package engine

// The fix job's end (fix.go runs it): the settle reads the fixer's report,
// puts the slot back in order (Slots.Resync at the pushed head, or at the
// reviewed one when nothing was pushed, so the next round's guard sees no
// drift) and ends the cycle: a push goes on to a forced re-review (phase
// reviewing; findings the fixer left to the author wait in the loop's row
// for that round's end), anything else ends the loop, with an info toast
// when the fixer answered everything without a push and as needs attention
// (attention.KindFix) otherwise. A stop (`magnum abort`) ends the loop
// quietly.

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

	"github.com/zhuravel/magnum/internal/attention"
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
// attention; replies alone end the loop with an info toast. The events name
// counts, SHAs, the role and the report, never what the fixer wrote.
func (e *Engine) settleFix(ctx context.Context, job *fixJob) {
	subject, report := job.subject(), deref(job.run.ReportPath)
	rep, rerr := e.readFixReport(job)
	pushed := rerr == nil && len(rep.Pushed) > 0 && !rep.PushFailed
	target := job.review.SHA
	var headErr error
	err := steps.Step(ctx, e.st, subject, "fix.settle", func(ctx context.Context) error {
		if pushed {
			if target, headErr = e.pushedHead(ctx, job, rep); headErr != nil {
				target = job.review.SHA
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
	e.event(ctx, "info", subject, "fix.replied", job.role.Name+" "+replied, data)
	n := len(rep.NeedsYou)
	needs := fmt.Sprintf("fix: %s %s your call: %s", textx.Count(n, "finding", "findings"), textx.Plural(n, "needs", "need"), report)
	if n > 0 {
		data["needs_you"] = n
	}
	switch {
	case pushed && n > 0:
		e.event(ctx, "warn", subject, "fix.needs_you", needs+"; the re-review of "+textx.ShortSHA(target)+" runs first", data)
		e.finishFixRun(ctx, job.run, store.RunVerified, "pushed", "")
		e.fixReReview(ctx, job, target, rep.NeedsYou)
	case pushed:
		e.finishFixRun(ctx, job.run, store.RunVerified, "pushed", "")
		e.fixReReview(ctx, job, target, nil)
	case n > 0:
		e.finishFixRun(ctx, job.run, store.RunVerified, "needs_you", "")
		e.endFixState(ctx, job)
		e.event(ctx, "warn", subject, "fix.needs_you", needs, data)
		e.fixAttention(ctx, job, needs)
	default:
		e.finishFixRun(ctx, job.run, store.RunVerified, "replied", "")
		e.endFixState(ctx, job)
		answered := "the findings"
		if counted {
			answered = textx.Count(threads, "finding", "findings")
		}
		what := fmt.Sprintf("PR #%d: %s pushed nothing, replied to %s", job.pr.Number, job.role.Name, answered)
		e.info(notify.Item{Key: "fix-replied:" + job.run.ID, Title: "magnum: " + what,
			Body: job.pr.URL + "\nthe fix loop ends; the report: " + report, Line: what + " " + job.pr.URL, Kind: kindFixEnded})
	}
}

// fixReReview takes the loop to its re-review after a push: the row in
// phase reviewing (the cycle counted, the pushed head, needs the findings
// the fixer left to the author, none after a clean push), the push on the
// PR row as the poller records one (recordFixPush), and a forced review of
// the PR on it (forceReview, as `magnum review` queues one: no throttle, no
// daily cap, and the fixing hold does not apply to it). The PR is forced
// before onHeadChange sees the push, so the loop's re-review is never taken
// for a push the review covers already (settlePush). A force that fails
// after the head moved still lets onHeadChange see the push before the
// loop ends, so the push is recorded and its re-review queued the normal
// way (the poller, which finds the head already moved, would never queue
// it). A PR GitHub closed meanwhile ends the loop instead.
func (e *Engine) fixReReview(ctx context.Context, job *fixJob, head string, needs []string) {
	pr, err := e.st.PRByID(ctx, job.pr.ID)
	if err == nil && pr.GHState != store.GHOpen {
		e.endFixState(ctx, job)
		e.event(ctx, "info", job.subject(), "fix.ended", fmt.Sprintf("the fix loop ends: the PR is %s on GitHub", strings.ToLower(pr.GHState)), job.eventData())
		return
	}
	st := job.state
	st.Phase, st.Head, st.NeedsYou = FixPhaseReviewing, head, needs
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
		return
	}
	job.state = st
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
	if pr.HeadSHA != job.review.SHA || head == pr.HeadSHA {
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
	return map[string]any{"run": j.run.ID, "role": j.role.Name, "cycle": j.state.Cycle, "cap": j.state.Cap, "report": deref(j.run.ReportPath)}
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
// (the report gives short shas; Resync needs the whole one).
func (e *Engine) pushedHead(ctx context.Context, job *fixJob, rep FixReport) (string, error) {
	if e.d.Git == nil {
		return "", errors.New("no git to read the checkout's HEAD")
	}
	head, err := e.d.Git.RevParse(ctx, job.slot.Path, "HEAD")
	if err != nil {
		return "", fmt.Errorf("the checkout's HEAD: %w", err)
	}
	if slices.ContainsFunc(rep.Pushed, func(c FixCommit) bool { return strings.HasPrefix(head, c.SHA) }) {
		return head, nil
	}
	return "", fmt.Errorf("the checkout's HEAD %s is none of the %s the report lists", textx.ShortSHA(head),
		textx.Count(len(rep.Pushed), "commit", "commits"))
}

// resyncFixSlot puts the job's slot back in order at sha (Slots.Resync:
// detached, recorded as checked out), once the slot is still claimed or
// held for the PR.
func (e *Engine) resyncFixSlot(ctx context.Context, job *fixJob, sha string) error {
	slot, err := e.st.SlotByID(ctx, job.slot.ID)
	if err != nil {
		return err
	}
	if deref(slot.PRID) != job.pr.ID || (slot.State != store.SlotClaimed && slot.State != store.SlotHeld) {
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
// loop's cycle's own, so each loop that needs the author toasts.
func (e *Engine) fixAttention(ctx context.Context, job *fixJob, msg string) {
	pr, err := e.st.PRByID(ctx, job.pr.ID)
	if err != nil {
		e.log.Warn("fix: the PR is gone", "subject", job.subject(), "err", err)
		return
	}
	rj := job.asRound()
	rj.pr = pr
	key := fmt.Sprintf("attention:%d:%s:%s:%d:%s", pr.ID, attention.KindFix, store.FormatTime(job.state.StartedAt), job.state.Cycle, job.run.ID)
	switch {
	case pr.State == store.PRReviewed || pr.State == store.PRNeedsAttention:
		e.needsAttentionKeyed(ctx, rj, pr, []string{store.PRReviewed, store.PRNeedsAttention}, attention.KindFix, msg, nil, key)
	case slices.Contains(waitingStates, pr.State):
		label := fmt.Sprintf("%s#%d", job.repo.Name, pr.Number)
		e.urgent(key, "magnum: "+label+" needs attention", attention.Explain(attention.KindFix, msg, label).Summary, attentionWindow)
	}
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
		if err := e.resyncFixSlot(ctx, job, job.review.SHA); err != nil {
			problems = append(problems, fmt.Errorf("slot %s not put back: %w", job.slot.Name, err))
		} else {
			done = append(done, "slot "+job.slot.Name+" back at "+textx.ShortSHA(job.review.SHA))
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
// re-review, or the row of a stopped daemon) for a stop of the PR: its row
// goes, with an event and an info toast. A stop without the PR's
// reservation (`magnum abort` on a reviewed PR) can meet the hook after a
// round starting the loop's next cycle, so the row goes only while it is
// the one read here (Store.DeleteKVIf; claimFixRow is the other side). It
// returns what it did for the stop's answer ("" = the PR had no loop, or a
// cycle took it on meanwhile).
func (e *Engine) endFixLoop(ctx context.Context, repo store.Repo, pr store.PR, by string) string {
	st, raw, _, err := e.fixRow(ctx, pr.ID)
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
	if err != nil {
		note = "the fix loop's unreadable row cleared"
	}
	e.event(ctx, "info", prSubject(repo, pr.Number), "fix.stopped", note+" by "+by, map[string]any{"by": by, "report": st.Report})
	e.toastFixStopped(repo, pr, st, note)
	return note
}

// toastFixStopped is the info toast of a fix loop a stop ended (st: its
// row), with what the stop did.
func (e *Engine) toastFixStopped(repo store.Repo, pr store.PR, st FixState, what string) {
	title := fmt.Sprintf("magnum: fix stopped: %s#%d", repo.Name, pr.Number)
	e.info(notify.Item{Key: fmt.Sprintf("fix-stopped:%d:%s:%d", pr.ID, store.FormatTime(st.StartedAt), st.Cycle), Title: title,
		Body: pr.URL + "\n" + what, Line: strings.TrimPrefix(title, "magnum: ") + " " + pr.URL, Kind: kindFixEnded})
}
