package factory

// The factory tracks where its tasks' Jira issues stand (Jira.StatusSince:
// the status's name, id and category, since when, the assignee) in the
// registry's tracking rows, which the PR's status file (its Jira status)
// and the screen (each task's row and card) read. The reads never run on
// the loop: after the advance, a tick picks the due reads and hands them to
// one batch off the loop (trackJira), and the next pass takes the batch's
// answers (takeTracking, before the status files are written) and writes
// the rows; while a batch runs, no other starts. A task is tracked while it
// holds a Jira issue's key (not a key of another tracker, which is never
// read from Jira), has not ended and does not wait for Jira itself (its
// intake's or its linked key's tries read Jira, on the loop as before):
// each one at most every trackEvery, those never read first (the published
// ones first of them), then the longest unread, at the pace that keeps
// every one within trackEvery (at least one a tick), so the reads come due
// apart across the ticks. A task that ended (done, stopped, failed) owes
// one last read, lastReadAfter after its end (Jira's own automation moves
// a merged PR's issue a while after it), tried again while it fails, until
// lastReadBound after the end: its PR's last status write waits for it up
// to then (lastReadPending). A batch reads what the tick's Jira budget
// (jiraLeft) leaves after the intake's tries; a read Jira did not answer
// ends it, and a batch whose every read Jira did not answer holds the next
// one back (trackBackoff), which a batch Jira answers ends. A batch has a
// hard deadline (trackBatchBound) and each read its own (trackBound); the
// drain cancels a batch and waits for it within its budget, and a read it
// cut records nothing. A task whose Jira move has a plan begun
// (jira_plans.go) gets its read at once, first after the owed last reads,
// with the issue's transitions when the move could go, for two of the
// budget; the next pass settles the plan from it. A failure is recorded on
// the row in the jira
// package's words (a class and a code, never ticket text), the facts of the
// read before kept (or, after the issue answered, what it said), and logged
// once while its cause stays: a failure of the site (Jira down, the
// credentials, the token file) once for every task, one of an issue (its
// refusal, a part of its read) once for its task; it fails neither the
// tick nor the task.

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/jira"
	"github.com/zhuravel/magnum/internal/textx"
)

const (
	// trackEvery is the least time between two reads of one task's issue.
	trackEvery = 10 * time.Minute
	// trackBound bounds one read: curl's bound on one request, and some
	// time for the others a read may make (myself, the pages of a long
	// changelog).
	trackBound = jira.MaxTime + 10*time.Second
	// batchBound is a batch's hard deadline, well within trackEvery.
	batchBound = 2 * time.Minute
	// The site's backoff after batches whose reads Jira did not answer: 1,
	// 2, 4, 8, then 15 minutes.
	trackBackoffFirst = time.Minute
	trackBackoffMax   = 15 * time.Minute
	// lastReadAfter is how long after a task's end its last read waits, and
	// lastReadBound how long after the end it is owed at most.
	lastReadAfter = 2 * time.Minute
	lastReadBound = 5 * time.Minute
	// sendWait is how long a batch's answers wait for their place on the
	// loop's next pass before the batch drops them.
	sendWait = 10 * time.Second
)

// trackBackoff is the hold after n batches in a row whose reads Jira did
// not answer.
func trackBackoff(n int) time.Duration {
	return min(trackBackoffFirst<<min(max(n-1, 0), 4), trackBackoffMax)
}

// trackState is the tracking of the tasks' Jira issues: the loop's own,
// but for done, which a batch sends its answers on (a buffer of one, which
// the loop's next pass empties), running, which the loop sets as it starts
// a batch and the batch clears as it ends, and dropped, whether a batch's
// drop of its answers was logged (once, until answers find their place
// again). The loop starts no batch while one runs or answers wait, which
// saves reads; a batch whose answers find no place within trackSendWait
// drops them, so a batch never waits for good either way.
type trackState struct {
	running atomic.Bool
	dropped atomic.Bool
	done    chan trackBatch
	// down counts the batches in a row whose reads Jira did not answer, and
	// next is when the next batch may start (the backoff).
	down int
	next time.Time
	log  trackLog
}

// trackLog is what the tracking logged of its failures, each said once
// while it stays: site is the cause of the last failure of the whole
// site's ("" after a read Jira answered), tasks each task's own cause
// (until its read works) and store the registry's failure ("" after a
// write that worked).
type trackLog struct {
	site  string
	tasks map[int64]string
	store string
}

// trackJob is one read a batch makes: the task, its issue's key, its row
// (had: one exists, of the key), whether it is the task's last, and the
// begun plan of a Jira move it reads for (nil for none: jira_plans.go).
type trackJob struct {
	t    registry.Task
	key  string
	row  registry.Tracking
	had  bool
	last bool
	plan *planJob
}

// cost is how much of the tick's Jira budget the job takes: one read, and
// a plan's read of the transitions.
func (j trackJob) cost() int {
	if j.plan != nil {
		return 2
	}
	return 1
}

// trackRead is a batch's read of job's issue: Jira's answer and when, and
// for a plan the issue's transitions (trans, transErr; none read when the
// move could not go).
type trackRead struct {
	job      trackJob
	st       jira.IssueStatus
	err      error
	at       time.Time
	trans    []jira.Transition
	transErr error
}

// answered reports whether Jira answered the read: it worked, or the issue
// itself answered (a PartError), or Jira refused it; not when Jira did not
// answer (jiraDown, or the read's bound ran out before the issue answered).
func (r trackRead) answered() bool {
	if r.err == nil || r.st.Name != "" {
		return true
	}
	return !jiraDown(r.err) && !errors.Is(r.err, context.DeadlineExceeded)
}

// trackBatch is a batch's reads, in its order: a read the drain cut is not
// among them.
type trackBatch struct {
	reads []trackRead
}

// trackJira hands one batch off the loop the reads that are due (see the
// file's comment), within the tick's Jira budget, unless a batch runs or
// its answers wait, the site's backoff holds, or the daemon drains. Its
// pass over the Jira plans (jiraPlans) runs first, whatever holds the
// batch, when a repository plans its moves.
func (e *Engine) trackJira(ctx context.Context, tasks []registry.Task) {
	now := e.now()
	if e.d.Jira == nil || e.draining.Load() {
		return
	}
	held := e.jiraLeft <= 0 || e.trk.running.Load() || len(e.trk.done) > 0 || now.Before(e.trk.next)
	if held && !slices.ContainsFunc(e.cfg.Factory.Repos, config.FactoryRepo.JiraMovesShadow) {
		return
	}
	rows, err := e.reg.Trackings(ctx)
	if err != nil {
		e.trackStoreFailed(err)
		return
	}
	plans := e.jiraPlans(ctx, tasks, rows) // a milestone's plan begins when it comes, whatever holds the batch
	if held {
		return
	}
	var last, planned, due []trackJob
	tracked := 0
	for _, t := range tasks {
		key := e.jiraKeyOf(t)
		if key == "" || t.WaitsWho == waitJira {
			continue
		}
		row, ok := rows[t.ID]
		j := trackJob{t: t, key: key, row: row, had: ok && row.JiraKey == key}
		if trackEnded(t) {
			if e.lastReadOwed(t, j.row, j.had, now) && !now.Before(t.Updated.Add(lastReadAfter)) {
				j.last = true
				last = append(last, j)
			}
			continue
		}
		tracked++
		isDue := !j.had || !now.Before(row.TriedAt.Add(trackEvery))
		if p, ok := plans[t.ID]; ok && (isDue || row.TriedAt.Before(p.begun)) { // a plan reads at once, then as the refreshes do
			j.plan = &p
			planned = append(planned, j)
			continue
		}
		if isDue {
			due = append(due, j)
		}
	}
	ticks := max(int(trackEvery/e.poll()), 1) // the pace that reads every tracked task within trackEvery
	pace := max((tracked+ticks-1)/ticks, 1)
	slices.SortStableFunc(due, func(a, b trackJob) int {
		if a.had != b.had { // never read first
			if !a.had {
				return -1
			}
			return 1
		}
		if pa, pb := a.t.State == registry.TaskPublished, b.t.State == registry.TaskPublished; !a.had && pa != pb {
			if pa { // a PR's status file waits for its first read
				return -1
			}
			return 1
		}
		return cmp.Or(a.row.TriedAt.Compare(b.row.TriedAt), cmp.Compare(a.t.ID, b.t.ID))
	})
	slices.SortStableFunc(planned, func(a, b trackJob) int { return a.plan.begun.Compare(b.plan.begun) })
	// The owed last reads first (an ended PR's last write waits for them),
	// then the plans (a milestone's plan is read when it comes), then the
	// refreshes at their pace, within the budget.
	var jobs []trackJob
	for _, j := range slices.Concat(last, planned, due[:min(pace, len(due))]) {
		if j.cost() > e.jiraLeft { // a plan that does not fit waits for the next tick, first again; a cheaper read behind it goes
			continue
		}
		e.jiraLeft -= j.cost()
		jobs = append(jobs, j)
	}
	if len(jobs) == 0 {
		return
	}
	e.startTrackBatch(jobs)
}

// startTrackBatch starts the batch of jobs off the loop, on the engine's
// context: it reads them in order and sends its answers to the loop's next
// pass, on done, within trackSendWait (or drops them, logged once), then
// marks itself ended (running). Even after a panic it sends what it read.
func (e *Engine) startTrackBatch(jobs []trackJob) {
	e.trk.running.Store(true)
	done, readBound, batchBound, wait := e.trk.done, e.trackReadBound, e.trackBatchBound, e.trackSendWait
	e.goBackground("the tasks' Jira statuses", func(bg context.Context) {
		var b trackBatch
		defer e.trk.running.Store(false)
		defer func() {
			select {
			case done <- b:
				e.trk.dropped.Store(false)
			case <-bg.Done(): // the daemon stops: nobody takes them
			case <-time.After(wait):
				if e.trk.dropped.CompareAndSwap(false, true) {
					e.log.Warn("a batch of the tasks' Jira reads found no place on the loop: its answers were dropped, and the next batch reads them again",
						"reads", len(b.reads), "wait", wait)
				}
			}
		}()
		ctx, cancel := context.WithTimeout(bg, batchBound)
		defer cancel()
		for _, j := range jobs {
			if ctx.Err() != nil || e.draining.Load() {
				break
			}
			rctx, rcancel := context.WithTimeout(ctx, readBound)
			st, err := e.d.Jira.StatusSince(rctx, j.key, priorStatus(j))
			rcancel()
			if bg.Err() != nil { // the drain cut it: nothing to record
				break
			}
			r := trackRead{job: j, st: st, err: err, at: e.now()}
			if j.plan != nil && planNeedsTransitions(j.plan.move, st, err) {
				rctx, rcancel := context.WithTimeout(ctx, readBound)
				r.trans, r.transErr = e.d.Jira.Transitions(rctx, j.key)
				rcancel()
				if bg.Err() != nil {
					break
				}
			}
			b.reads = append(b.reads, r)
			if !r.answered() || r.transErr != nil && (jiraDown(r.transErr) || errors.Is(r.transErr, context.DeadlineExceeded)) {
				break // the next reads would wait for a Jira that does not answer too
			}
		}
		if slices.ContainsFunc(b.reads, func(r trackRead) bool { return r.job.last }) {
			e.Kick() // an ended PR's last write waits for this read: the next pass need not wait a poll
		}
	})
}

// priorStatus is the read before j's, as StatusSince takes it: the row's
// facts when it holds a read of j's key that worked, else none (the
// changelog is searched).
func priorStatus(j trackJob) jira.IssueStatus {
	if !j.had || j.row.ReadAt.IsZero() {
		return jira.IssueStatus{}
	}
	r := j.row
	return jira.IssueStatus{Key: j.key, Name: r.Status, StatusID: r.StatusID, Category: r.Category, Since: r.Since, Searched: r.Searched,
		Assigned: r.Assigned, AssigneeYou: r.AssigneeYou, MovedBy: r.MovedBy}
}

// takeTracking takes a batch's answers once it ended, without waiting for
// one that runs: it writes each read's row (the facts, or the failure on
// the facts before), logs the failures once per cause, and moves the
// site's backoff: a batch whose every read Jira did not answer holds the
// next one back, a batch Jira answered ends the hold.
func (e *Engine) takeTracking(ctx context.Context) {
	var b trackBatch
	select {
	case b = <-e.trk.done:
	default:
		return
	}
	if len(b.reads) == 0 {
		return
	}
	answered := false
	for _, r := range b.reads {
		answered = answered || r.answered()
		e.applyRead(ctx, r)
		if r.job.plan != nil {
			e.settlePlan(ctx, r)
		}
	}
	if answered {
		e.trk.down, e.trk.next = 0, time.Time{}
		return
	}
	e.trk.down++
	e.trk.next = b.reads[len(b.reads)-1].at.Add(trackBackoff(e.trk.down))
}

// applyRead writes read r's row: what Jira said (all of it, or what the
// issue said before a part of the read failed), or the failure on the facts
// of the read before (none of a row of another key).
func (e *Engine) applyRead(ctx context.Context, r trackRead) {
	j := r.job
	row := j.row
	if !j.had {
		row = registry.Tracking{TaskID: j.t.ID, JiraKey: j.key}
	}
	if r.err == nil || r.st.Name != "" {
		st := r.st
		row = registry.Tracking{TaskID: j.t.ID, JiraKey: j.key, Status: st.Name, StatusID: st.StatusID, Category: st.Category, Since: st.Since,
			Searched: st.Searched, Assigned: st.Assigned, AssigneeYou: st.AssigneeYou, MovedBy: st.MovedBy, ReadAt: r.at}
	}
	row.TriedAt, row.LastError = r.at, ""
	if r.err == nil || r.st.Name != "" { // the issue answered: Jira is up
		e.trk.log.site = ""
	}
	if r.err == nil {
		delete(e.trk.log.tasks, j.t.ID)
	} else {
		cause, site := e.trackCause(r)
		row.LastError = cause
		e.trackFailed(j.t, cause, site)
	}
	if err := e.reg.SetTracking(ctx, row); err != nil {
		e.trackStoreFailed(err)
		return
	}
	e.trk.log.store = ""
}

// trackCause is a failed read's cause in the words the row and the log
// keep, and whether it is the site's (any task's read would fail so) rather
// than its issue's: a jira.Error's own message (its class and code, never
// the answer's body), a part of the read that failed after the issue
// answered (its task's), the read's bound run out before the issue
// answered, or the first line of another error, redacted.
func (e *Engine) trackCause(r trackRead) (cause string, site bool) {
	words := func(err error) string {
		if errors.Is(err, context.DeadlineExceeded) {
			return "Jira did not answer within " + e.trackReadBound.String()
		}
		if je, ok := errors.AsType[*jira.Error](err); ok {
			return je.Error()
		}
		return textx.Clip(textx.FirstLine(execx.Redact(err.Error())), 200)
	}
	if pe, ok := errors.AsType[*jira.PartError](r.err); ok {
		return trackPartCause(pe.Part) + words(pe.Err), false
	}
	if r.st.Name != "" {
		return words(r.err), false
	}
	if errors.Is(r.err, context.DeadlineExceeded) {
		return words(r.err), true
	}
	if je, ok := errors.AsType[*jira.Error](r.err); ok {
		return je.Error(), je.Class != jira.ClassNotFound && je.Class != jira.ClassMalformed
	}
	return words(r.err), false
}

// trackPartCause is how a row's failure starts when part of a read failed
// after the issue answered (trackCause); the screen tells by it an assignee
// not known to be you.
func trackPartCause(part string) string {
	return "Jira answered for the issue but not for its " + part + ": "
}

// trackFailed logs task t's failed read, once while its cause stays: the
// site's for every task, an issue's for its task.
func (e *Engine) trackFailed(t registry.Task, cause string, site bool) {
	l := &e.trk.log
	if site {
		if l.site == cause {
			return
		}
		l.site = cause
		e.log.Warn("the factory cannot read its tasks' Jira statuses", "err", cause)
		return
	}
	if l.tasks[t.ID] == cause {
		return
	}
	if l.tasks == nil {
		l.tasks = map[int64]string{}
	}
	l.tasks[t.ID] = cause
	e.log.Warn("the factory cannot read a task's Jira status", "task", t.ID, "key", t.JiraKey, "err", cause)
}

// trackStoreFailed logs a failed read or write of the tracking rows, once
// while it stays the same.
func (e *Engine) trackStoreFailed(err error) {
	msg := textx.FirstLine(execx.Redact(err.Error()))
	if msg == e.trk.log.store {
		return
	}
	e.trk.log.store = msg
	e.log.Warn("the factory's tracking of Jira statuses cannot use its registry", "err", msg)
}

// lastReadOwed reports whether task t, ended, owes its issue's last read at
// now, by its row (had: one of its key): it is tracked (it has a row, or a
// PR whose status file waits for it), it ended less than lastReadBound ago,
// and no read of it worked since lastReadAfter after its end.
func (e *Engine) lastReadOwed(t registry.Task, row registry.Tracking, had bool, now time.Time) bool {
	if e.d.Jira == nil || !trackEnded(t) || e.jiraKeyOf(t) == "" || !had && t.PRNumber <= 0 {
		return false
	}
	end := t.Updated
	if !now.Before(end.Add(lastReadBound)) {
		return false
	}
	return !had || row.ReadAt.Before(end.Add(lastReadAfter))
}

// lastReadPending reports whether ended task t's PR's last status write
// waits for its last read (lastReadOwed); a row that cannot be read waits
// none.
func (e *Engine) lastReadPending(ctx context.Context, t registry.Task) bool {
	if e.d.Jira == nil || e.jiraKeyOf(t) == "" {
		return false
	}
	row, err := e.reg.Tracking(ctx, t.ID)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		return e.lastReadOwed(t, registry.Tracking{}, false, e.now())
	case err != nil:
		e.trackStoreFailed(err)
		return false
	}
	return e.lastReadOwed(t, row, row.JiraKey == e.jiraKeyOf(t), e.now())
}

// jiraKeyOf is task t's Jira issue's key: its key, unless it has none or
// its key is another tracker's (otherTracker), never read from Jira.
func (e *Engine) jiraKeyOf(t registry.Task) string {
	if t.JiraKey == "" {
		return ""
	}
	if _, other := e.otherTracker(t.Repo, t.JiraKey); other {
		return ""
	}
	return t.JiraKey
}

// trackEnded reports whether t ended for the tracking: done, stopped or
// failed (taskEnded, which a copy's release goes by, counts a failed task
// as holding its work).
func trackEnded(t registry.Task) bool { return taskEnded(t) || t.State == registry.TaskFailed }
