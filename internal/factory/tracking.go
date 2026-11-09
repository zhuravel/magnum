package factory

// Each tick reads where the tasks' Jira issues stand (Jira.Status: the
// status's name and category, since when, the assignee) into the
// registry's tracking rows, which the PR's status file (its Jira status)
// and the screen (each task's row and card) read. A task is tracked while
// it holds a Jira issue's key (not a key of another tracker, which is never
// read from Jira) and has not ended: each one at most every trackEvery,
// those never read first, then the longest unread, at the pace that keeps
// every one within trackEvery (at least one a tick), so the reads come due
// apart across the ticks; a task that ended (done, stopped, failed) gets
// one last read, before its PR's last status write, and none after. The
// reads spend the tick's Jira budget (jiraLeft) after the intake's tries,
// which come first: the ended tasks' last reads before the tasks advance,
// the refreshes after; a read Jira did not answer ends the tick's reads, as
// an intake try does. A task waiting for Jira (its intake or its linked
// key's read) is not read: its own tries read Jira. A failure is recorded
// on the row in the jira package's words (a class and a code, never
// ticket text), the facts of the read before kept, and logged once while
// its cause stays: a failure of the site (Jira down, the credentials, the
// token file) once for every task, a refusal of one issue once for its
// task; it fails neither the tick nor the task.

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/jira"
	"github.com/zhuravel/magnum/internal/textx"
)

const (
	// trackEvery is the least time between two reads of one task's issue.
	trackEvery = 10 * time.Minute
	// trackBound bounds one read on the loop: curl's bound on one request,
	// and some time for the others a read may make (myself, the pages of a
	// long changelog).
	trackBound = jira.MaxTime + 10*time.Second
)

// trackLog is what the tracking pass logged of its failures, each said
// once while it stays: site is the cause of the last failure of the whole
// site's ("" after a read that worked), tasks each task's own cause (until
// its read works) and store the registry's failure ("" after a write that
// worked). The loop's own.
type trackLog struct {
	site  string
	tasks map[int64]string
	store string
}

// trackDue is a task whose issue the pass reads, and its row (had: one
// exists, of its key).
type trackDue struct {
	t   registry.Task
	row registry.Tracking
	had bool
}

// trackJira reads the issues of tasks that are due (see the file's
// comment): with last, the ended tasks' last reads alone; else the other
// tasks' refreshes, paced.
func (e *Engine) trackJira(ctx context.Context, tasks []registry.Task, last bool) {
	if e.d.Jira == nil || e.jiraLeft <= 0 || e.draining.Load() {
		return
	}
	rows, err := e.reg.Trackings(ctx)
	if err != nil {
		e.trackStoreFailed(err)
		return
	}
	now := e.now()
	var due []trackDue
	tracked := 0
	for _, t := range tasks {
		key := e.jiraKeyOf(t)
		if key == "" || t.WaitsWho == waitJira {
			continue
		}
		row, ok := rows[t.ID]
		had := ok && row.JiraKey == key
		if trackEnded(t) {
			if last && had && !row.TriedAt.After(t.Updated) { // tracked, and not read since its end
				due = append(due, trackDue{t, row, had})
			}
			continue
		}
		tracked++
		if !last && (!had || !now.Before(row.TriedAt.Add(trackEvery))) {
			due = append(due, trackDue{t, row, had})
		}
	}
	n := e.jiraLeft
	if !last { // the pace that reads every tracked task within trackEvery: tracked over the ticks in it, rounded up
		ticks := max(int(trackEvery/e.poll()), 1)
		n = min(n, max((tracked+ticks-1)/ticks, 1))
	}
	slices.SortStableFunc(due, func(a, b trackDue) int {
		if a.had != b.had { // never read first
			if !a.had {
				return -1
			}
			return 1
		}
		return cmp.Or(a.row.TriedAt.Compare(b.row.TriedAt), cmp.Compare(a.t.ID, b.t.ID))
	})
	for _, d := range due[:min(n, len(due))] {
		if e.jiraLeft <= 0 || e.draining.Load() || ctx.Err() != nil {
			return
		}
		e.jiraLeft--
		e.trackOne(ctx, d)
	}
}

// trackOne reads task d.t's issue and writes its row: the read's facts, or
// the failure on the facts of the read before (none of a row of another
// key). A read Jira did not answer spends the rest of the tick's budget.
func (e *Engine) trackOne(ctx context.Context, d trackDue) {
	key := e.jiraKeyOf(d.t)
	rctx, cancel := context.WithTimeout(ctx, trackBound)
	st, err := e.d.Jira.Status(rctx, key)
	cancel()
	if ctx.Err() != nil { // the daemon stops: no try to record
		return
	}
	at := e.now()
	row := d.row
	if !d.had {
		row = registry.Tracking{TaskID: d.t.ID, JiraKey: key}
	}
	row.TriedAt = at
	if err != nil {
		cause, site := trackCause(err)
		row.LastError = cause
		if jiraDown(err) || errors.Is(err, context.DeadlineExceeded) {
			e.jiraLeft = 0 // the next reads would wait for a Jira that does not answer too
		}
		e.trackFailed(d.t, cause, site)
	} else {
		row = registry.Tracking{TaskID: d.t.ID, JiraKey: key, Status: st.Name, Category: st.Category, Since: st.Since,
			Assigned: st.Assigned, AssigneeYou: st.AssigneeYou, ReadAt: at, TriedAt: at}
		e.trk.site = ""
		delete(e.trk.tasks, d.t.ID)
	}
	if err := e.reg.SetTracking(ctx, row); err != nil {
		e.trackStoreFailed(err)
		return
	}
	e.trk.store = ""
}

// trackCause is a failed read's cause in the words the row and the log
// keep, and whether it is the site's (any task's read would fail so) rather
// than its issue's: a jira.Error's own message (its class and code, never
// the answer's body), the bound's run-out, or the first line of another
// error, redacted.
func trackCause(err error) (cause string, site bool) {
	if errors.Is(err, context.DeadlineExceeded) {
		return "Jira did not answer within " + trackBound.String(), true
	}
	if je, ok := errors.AsType[*jira.Error](err); ok {
		return je.Error(), je.Class != jira.ClassNotFound && je.Class != jira.ClassMalformed
	}
	return textx.Clip(textx.FirstLine(execx.Redact(err.Error())), 200), false
}

// trackFailed logs task t's failed read, once while its cause stays: the
// site's for every task, an issue's for its task.
func (e *Engine) trackFailed(t registry.Task, cause string, site bool) {
	if site {
		if e.trk.site == cause {
			return
		}
		e.trk.site = cause
		e.log.Warn("the factory cannot read its tasks' Jira statuses", "err", cause)
		return
	}
	if e.trk.tasks[t.ID] == cause {
		return
	}
	if e.trk.tasks == nil {
		e.trk.tasks = map[int64]string{}
	}
	e.trk.tasks[t.ID] = cause
	e.log.Warn("the factory cannot read a task's Jira status", "task", t.ID, "key", t.JiraKey, "err", cause)
}

// trackStoreFailed logs a failed read or write of the tracking rows, once
// while it stays the same.
func (e *Engine) trackStoreFailed(err error) {
	msg := textx.FirstLine(execx.Redact(err.Error()))
	if msg == e.trk.store {
		return
	}
	e.trk.store = msg
	e.log.Warn("the factory's tracking of Jira statuses cannot use its registry", "err", msg)
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
