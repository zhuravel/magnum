package engine

// Work in flight that no registry row shows (DECISIONS "A restart waits for
// the heavy worker's jobs and the operator's approval writes"): the restart's
// guards, the daemon's own idle restart (restartBusy) and the CLI's
// daemon-restart, --when-idle, --drain, install, uninstall and daemon-stop
// (backgroundWork), counted rounds, fix and whiteboard jobs and the learn
// jobs, but not a job on the heavy worker (a slot's provision, release or
// eviction, a slot request, the reconcile), whose 20 to 45 minutes of slot
// work a restart cut short, nor a GitHub write as the operator (an automatic
// approval, its withdrawal), whose answer a restart lost. beginWork records
// each while it is queued or runs, in this process and in KVDaemonWork.

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"sync"
	"time"
)

// KVDaemonWork holds the work in flight that no registry row shows (WorkMark
// as JSON), which the CLI reads before it stops the daemon: the jobs queued
// or running on the heavy worker and the GitHub writes as the operator under
// way. The engine keeps it to its own state (beginWork): written whenever
// that changes, deleted when nothing is in flight, at the shutdown and at
// every start of the daemon.
const KVDaemonWork = "daemon.work"

// WorkMark is KVDaemonWork's value: the work in flight, oldest first.
type WorkMark struct {
	Work []WorkItem `json:"work"`
}

// WorkItem is one piece of work in flight: what it is ("heavy job
// provision:talkable/talkable", "approval of talkable/talkable#2 as
// zhuravel") and since when (a heavy job's since it was queued).
type WorkItem struct {
	What    string    `json:"what"`
	Started time.Time `json:"started"`
}

// beginWork records what as work in flight until the returned end is called
// (once; later calls do nothing).
func (e *Engine) beginWork(what string) (end func()) {
	e.workMu.Lock()
	e.workSeq++
	id := e.workSeq
	e.work[id] = WorkItem{What: what, Started: e.now()}
	e.writeWorkLocked()
	e.workMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			e.workMu.Lock()
			delete(e.work, id)
			e.writeWorkLocked()
			e.workMu.Unlock()
		})
	}
}

// workInFlight is the work in flight, oldest first.
func (e *Engine) workInFlight() []WorkItem {
	e.workMu.Lock()
	defer e.workMu.Unlock()
	return e.workItemsLocked()
}

func (e *Engine) workItemsLocked() []WorkItem {
	out := make([]WorkItem, 0, len(e.work))
	for _, id := range slices.Sorted(maps.Keys(e.work)) {
		out = append(out, e.work[id])
	}
	return out
}

// writeWorkMark keeps KVDaemonWork to the work in flight: the startup's call
// drops a crashed daemon's mark.
func (e *Engine) writeWorkMark() {
	e.workMu.Lock()
	defer e.workMu.Unlock()
	e.writeWorkLocked()
}

// writeWorkLocked writes KVDaemonWork under workMu, so the last write is the
// latest state, on a context no stop cancels: work that ends after the stop
// still clears its entry. A dry run writes nothing a real daemon's CLI reads.
func (e *Engine) writeWorkLocked() {
	if e.d.DryRun || e.st == nil {
		return
	}
	ctx := context.Background()
	var err error
	if items := e.workItemsLocked(); len(items) == 0 {
		err = e.st.DeleteKV(ctx, KVDaemonWork)
	} else {
		b, _ := json.Marshal(WorkMark{Work: items})
		err = e.st.SetKV(ctx, KVDaemonWork, string(b))
	}
	if err != nil {
		e.log.Warn("work in flight: mark not written", "err", err)
	}
}
