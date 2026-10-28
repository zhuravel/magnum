package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
)

// provisionRetry spaces out attempts to provision a pool slot whose last
// attempt failed: meanwhile waiting PRs get a slot by eviction.
const provisionRetry = 10 * time.Minute

// poolCensus is a pool's slots as provisioning sees them.
type poolCensus struct {
	count     int          // slots that take a place below pool.max: all but removed and lost
	resumable []store.Slot // provisioning, ready for an attempt
	backoff   []store.Slot // provisioning whose last attempt failed within provisionRetry
}

func (e *Engine) census(sls []store.Slot) poolCensus {
	var c poolCensus
	now := e.now()
	for _, s := range sls {
		switch s.State {
		case store.SlotRemoved, store.SlotLost:
			continue
		case store.SlotProvisioning:
			if s.LastError != nil && now.Sub(s.UpdatedAt) < provisionRetry {
				c.backoff = append(c.backoff, s)
			} else {
				c.resumable = append(c.resumable, s)
			}
		}
		c.count++
	}
	return c
}

// needSlot gets a pool slot freed or made for a waiting PR: provision one
// below pool.max (or resume one being provisioned), else evict the least
// recently used idle held slot (park its sessions, release it), else resume a
// release that stalled. All of them run on the heavy worker. A slot whose
// provisioning failed recently is left alone for provisionRetry.
func (e *Engine) needSlot(ctx context.Context, pool config.Pool, subject string) {
	sls, err := e.st.ListSlots(ctx, store.SlotFilter{RepoFullName: pool.Repo, Kind: store.SlotKindPool})
	if err != nil {
		e.warnUnlessStopped(ctx, err, "slots")
		return
	}
	c := e.census(sls)
	if len(c.resumable) > 0 || (len(c.backoff) == 0 && c.count < pool.Max) {
		if e.d.DryRun {
			e.rec.Record(ctx, "pool:"+pool.Repo, "provision", fmt.Sprintf("a slot for %s (%d of max %d)", subject, c.count, pool.Max))
			return
		}
		if e.enqueueHeavy("provision:"+pool.Repo, e.provisionJob(pool)) {
			e.event(ctx, "info", "pool:"+pool.Repo, "pool.provision", fmt.Sprintf("provisioning a slot for %s (%d of max %d)", subject, c.count, pool.Max), nil)
		}
		return
	}
	ev, err := e.st.EvictableSlots(ctx, pool.Repo, e.now(), e.cfg.Daemon.MinWarm.Duration)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "evictable slots")
		return
	}
	if len(ev) == 0 {
		if sl, ok := e.stalledRelease(ctx, sls); ok {
			e.resumeRelease(ctx, pool, sl, subject)
			return
		}
		e.log.Debug("no slot free or evictable", "subject", subject)
		return
	}
	sl := ev[0]
	if e.d.DryRun {
		e.rec.Record(ctx, "slot:"+sl.Name, "evict", "park sessions and release for "+subject)
		return
	}
	if e.enqueueHeavy("evict:"+pool.Repo, e.evictJob(pool, sl.ID)) {
		e.event(ctx, "info", "slot:"+sl.Name, "slot.evict", "evicting "+sl.Name+" for "+subject, nil)
	}
}

// stalledRelease is a pool slot whose release (an eviction, a `magnum
// release` of an open PR) failed half way and that nothing else resumes:
// releasing or dirty_schema, not held by a closing PR (cleanup resumes
// those).
func (e *Engine) stalledRelease(ctx context.Context, sls []store.Slot) (store.Slot, bool) {
	if all := e.stalledReleases(ctx, sls); len(all) > 0 {
		return all[0], true
	}
	return store.Slot{}, false
}

// stalledReleases are all of them, in the order of sls.
func (e *Engine) stalledReleases(ctx context.Context, sls []store.Slot) []store.Slot {
	var out []store.Slot
	for _, s := range sls {
		if s.State != store.SlotReleasing && s.State != store.SlotDirtySchema {
			continue
		}
		if s.PRID != nil {
			if pr, err := e.st.PRByID(ctx, *s.PRID); err == nil && slices.Contains(closingStates, pr.State) {
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

// The reconcile resumes a stalled release that has waited this long
// (resumeStalledReleases), and tries the same slot again no sooner than
// that. A step of a release that began under releaseStepMaxAge ago, in this
// daemon's lifetime and has no end event counts as running: the longest
// (the schema reload, 30 minutes a try, two tries) ends well within it.
// After releaseResumeCap resumed releases of one slot in a row that failed
// it stops resuming that slot (noteResumedRelease).
const (
	stalledReleaseAfter = 30 * time.Minute
	releaseStepMaxAge   = 2 * time.Hour
	releaseResumeCap    = 3
)

// kvSlotReleaseResumes holds slot id's releaseResumes (JSON): what the
// reconcile has done to resume its stalled release. It is in the registry,
// not in memory, because a restart would otherwise give the slot three more
// tries, and the daemon restarts every few hours (each start reconciles at
// once). Deleted when the slot is no longer stalled, and when another path
// has changed the slot since the last try (releaseRecord).
func kvSlotReleaseResumes(slotID int64) string { return fmt.Sprintf("slot.%d.release_resumes", slotID) }

// releaseResumes is what the reconcile remembers of one slot's resumed
// releases.
type releaseResumes struct {
	// Tried is when the reconcile last queued a resumption.
	Tried time.Time `json:"tried,omitzero"`
	// Failed counts the resumed releases in a row that ran and ended without
	// the slot free; State and UpdatedAt are the slot as the last of them
	// left it (the row changes under a failed release too: its last error),
	// so a later difference means another path touched the slot.
	Failed    int       `json:"failed,omitempty"`
	State     string    `json:"state,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
	// GivenUp: Failed reached releaseResumeCap, and the slot is left alone
	// until it differs from State and UpdatedAt.
	GivenUp bool `json:"given_up,omitempty"`
}

// releaseRecord is the registry's releaseResumes of slot s, as long as the
// slot is as the last failed try left it; a slot that differs (its state or
// updated_at moved: the operator's repair or release, a PR waiting for a
// slot that resumed it) starts over, and its record is deleted.
func (e *Engine) releaseRecord(ctx context.Context, s store.Slot) releaseResumes {
	rec, ok := e.readReleaseRecord(ctx, s.ID)
	if ok && rec.Failed > 0 && (rec.State != s.State || !rec.UpdatedAt.Equal(s.UpdatedAt)) {
		e.delKV(ctx, kvSlotReleaseResumes(s.ID))
		return releaseResumes{}
	}
	return rec
}

// readReleaseRecord is slot id's record as stored (zero and false when
// there is none or it cannot be read).
func (e *Engine) readReleaseRecord(ctx context.Context, id int64) (releaseResumes, bool) {
	raw, ok := e.getKV(ctx, kvSlotReleaseResumes(id))
	if !ok {
		return releaseResumes{}, false
	}
	var rec releaseResumes
	if json.Unmarshal([]byte(raw), &rec) != nil {
		return releaseResumes{}, false
	}
	return rec, true
}

func (e *Engine) saveReleaseRecord(ctx context.Context, id int64, rec releaseResumes) {
	if b, err := json.Marshal(rec); err == nil {
		e.setKV(ctx, kvSlotReleaseResumes(id), string(b))
	}
}

// resumeStalledReleases queues, for the reconcile that calls it (a job of
// the heavy worker), the resumption of one pool slot's stalled release, the
// one that has been releasing the longest: a slot in releasing or
// dirty_schema for over stalledReleaseAfter, whose PR is not closing (cleanup
// resumes those), whose release this process does not run (releasingNow)
// and none of whose release steps runs (releaseStepRunning), tried no
// sooner than stalledReleaseAfter ago and not given up
// (noteResumedRelease: releaseResumeCap failed resumptions in a row). Before
// it only a PR waiting for a slot with every held one in use resumed a
// release (needSlot), so a stalled release kept its slot out of the pool
// while held slots existed. It is the same job as an eviction (evictJob:
// the PR is reserved, so no round or open uses the slot meanwhile, its
// sessions are parked, and Release resumes after its last completed step
// once its guard allows it), queued only when the heavy worker has nothing
// else to do (heavyIdle), and the queue's key makes a resumption already
// queued or running for the repository the one that counts.
func (e *Engine) resumeStalledReleases(ctx context.Context) {
	if e.d.Slots == nil || !e.heavyIdle() {
		return
	}
	now := e.now()
	var slot store.Slot
	var rec releaseResumes
	var pool *config.Pool
	for i := range e.cfg.Pools {
		p := &e.cfg.Pools[i]
		sls, err := e.st.ListSlots(ctx, store.SlotFilter{RepoFullName: p.Repo, Kind: store.SlotKindPool})
		if err != nil {
			e.warnUnlessStopped(ctx, err, "slots")
			return
		}
		for _, s := range e.stalledReleases(ctx, sls) {
			if now.Sub(s.UpdatedAt) < stalledReleaseAfter || e.releasingNow(s.ID) {
				continue
			}
			r := e.releaseRecord(ctx, s)
			if r.GivenUp || now.Sub(r.Tried) < stalledReleaseAfter || e.releaseStepRunning(ctx, s) {
				continue
			}
			if pool == nil || s.UpdatedAt.Before(slot.UpdatedAt) {
				slot, rec, pool = s, r, p
			}
		}
	}
	if pool == nil {
		return
	}
	stalled := humanDuration(now.Sub(slot.UpdatedAt).Round(time.Minute))
	if e.d.DryRun {
		e.rec.Record(ctx, "slot:"+slot.Name, "release", fmt.Sprintf("resume the release of %s (%s for %s)", slot.Name, slot.State, stalled))
		return
	}
	job := e.evictJob(*pool, slot.ID)
	if !e.enqueueHeavy("evict:"+pool.Repo, func(ctx context.Context) error {
		err := job(ctx)
		e.noteResumedRelease(ctx, slot.ID, err)
		return err
	}) {
		return
	}
	rec.Tried = now
	e.saveReleaseRecord(ctx, slot.ID, rec)
	e.event(ctx, "info", "slot:"+slot.Name, "slot.release_resumed",
		fmt.Sprintf("resuming the release of %s, %s for %s (it kept the slot out of the pool)", slot.Name, slot.State, stalled),
		map[string]any{"slot": slot.Name, "state": slot.State, "stalled_for": now.Sub(slot.UpdatedAt).Round(time.Second).String()})
}

// noteResumedRelease counts, after the reconcile's resumption of slot id's
// release ended with err, the resumed releases of the slot in a row that
// ran and failed (releaseResumes): err != nil, the daemon not stopping, and
// the slot still stalled. A resumption that ran no release (the PR was
// reserved, the slot no longer evictable: nil) counts nothing; one that
// leaves the slot in another state than releasing or dirty_schema (free, or
// broken, which `magnum slots repair` takes) ends the record. The
// releaseResumeCap-th failure gives the slot up: one slot.release_given_up
// warning and one toast, and the reconcile leaves the slot alone until
// another path changes it (releaseRecord).
func (e *Engine) noteResumedRelease(ctx context.Context, id int64, err error) {
	if ctx.Err() != nil {
		return // a stop cut it, not a failure
	}
	sl, rerr := e.st.SlotByID(ctx, id)
	if rerr != nil {
		e.warnUnlessStopped(ctx, rerr, "slot after its resumed release", "slot", id)
		return
	}
	if sl.State != store.SlotReleasing && sl.State != store.SlotDirtySchema {
		e.delKV(ctx, kvSlotReleaseResumes(id))
		return
	}
	if err == nil {
		return
	}
	rec, _ := e.readReleaseRecord(ctx, id) // as the try began: the slot differs from its snapshot by the try itself
	rec.Failed++
	rec.State, rec.UpdatedAt = sl.State, sl.UpdatedAt
	if rec.Failed >= releaseResumeCap {
		rec.GivenUp = true
	}
	e.saveReleaseRecord(ctx, id, rec)
	if !rec.GivenUp {
		return
	}
	e.event(ctx, "warn", "slot:"+sl.Name, "slot.release_given_up",
		fmt.Sprintf("the release of %s (%s) failed in %d resumptions in a row: %v; magnum stops resuming it until the slot changes",
			sl.Name, sl.State, rec.Failed, err),
		map[string]any{"slot": sl.Name, "state": sl.State, "failed": rec.Failed})
	e.urgent(fmt.Sprintf("release_given_up:%s:%d", sl.Name, sl.UpdatedAt.Unix()), "magnum: slot "+sl.Name+" will not release",
		fmt.Sprintf("slot %s (%s): its release failed %d times in a row, so magnum stops resuming it. Fix the cause, then `magnum release %s` "+
			"(`magnum slots repair %s` once it is broken).", sl.Name, sl.State, rec.Failed, sl.Name, sl.Name), releaseGiveUpToastWindow)
}

// releaseGiveUpToastWindow is the dedupe window of the give-up toast.
const releaseGiveUpToastWindow = 24 * time.Hour

// releaseSlot is the slot manager's Release, with a mark that this process
// runs the slot's release while it does (releasingNow): every release the
// engine starts goes through it, the ones a request's goroutine runs
// (magnum stop, a failed magnum open) as the ones on the heavy worker, so
// the reconcile never resumes a release whose steps are running in this
// process, in the moment between two of them.
func (e *Engine) releaseSlot(ctx context.Context, sl store.Slot, pool config.Pool, reason string) error {
	return e.release(ctx, sl, pool, reason, false)
}

// handBackSlot is releaseSlot for a slot that a claim of magnum fix or
// magnum open reserved and could not use: a herdr that cannot say what the
// PR's shell panes there run does not keep it from the pool, since nothing
// ran in it after the release that freed it looked.
func (e *Engine) handBackSlot(ctx context.Context, sl store.Slot, pool config.Pool, reason string) error {
	return e.release(ctx, sl, pool, reason, true)
}

func (e *Engine) release(ctx context.Context, sl store.Slot, pool config.Pool, reason string, handBack bool) error {
	// The release resets the checkout: never under a command that still
	// runs in a shell pane of the slot's PR (the shell hold, as a round's
	// setup sees it; the release is tried again later).
	if sl.PRID != nil {
		if pr, err := e.st.PRByID(ctx, *sl.PRID); err == nil {
			if err := e.clearShells(ctx, pr, sl, holdRelease); err != nil && !(handBack && errors.Is(err, agents.ErrHerdrUnread)) {
				return fmt.Errorf("release %s: %w", sl.Name, err)
			}
		}
	}
	e.heavyMu.Lock()
	if e.releasing == nil {
		e.releasing = map[int64]int{}
	}
	e.releasing[sl.ID]++
	e.heavyMu.Unlock()
	defer func() {
		e.heavyMu.Lock()
		defer e.heavyMu.Unlock()
		if e.releasing[sl.ID]--; e.releasing[sl.ID] <= 0 {
			delete(e.releasing, sl.ID)
		}
	}()
	return e.d.Slots.Release(ctx, sl, pool, reason)
}

// releasingNow reports whether this process runs slot id's release.
func (e *Engine) releasingNow(id int64) bool {
	e.heavyMu.Lock()
	defer e.heavyMu.Unlock()
	return e.releasing[id] > 0
}

// heavyIdle reports whether the heavy worker has nothing to do but the
// reconcile that asks: no job is queued, and none runs except a reconcile.
func (e *Engine) heavyIdle() bool {
	e.heavyMu.Lock()
	defer e.heavyMu.Unlock()
	if len(e.heavy) > 0 {
		return false
	}
	for key := range e.heavyKeys {
		if key != "reconcile" {
			return false
		}
	}
	return true
}

// releaseStepRunning reports whether a step of slot sl's release may be
// running: the newest step event of its subject is a begin that no ok or
// fail row followed, written by this daemon (after it started: an earlier
// one is a dead daemon's) less than releaseStepMaxAge ago.
func (e *Engine) releaseStepRunning(ctx context.Context, sl store.Slot) bool {
	evs, err := e.st.EventsBySubject(ctx, "slot:"+sl.Name+":release", 20)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "release steps", "slot", sl.Name)
		return true // unknown: leave it alone
	}
	for _, ev := range slices.Backward(evs) {
		if ev.Kind != store.KindStep && ev.Kind != store.KindStepReset {
			continue
		}
		if ev.Kind != store.KindStep || ev.Phase == nil || *ev.Phase != store.PhaseBegin {
			return false
		}
		started, _ := e.kvTime(ctx, store.KVDaemonStartedAt)
		return ev.At.After(started) && e.now().Sub(ev.At) < releaseStepMaxAge
	}
	return false
}

// resumeRelease queues the resumption of a pool slot's stalled release.
func (e *Engine) resumeRelease(ctx context.Context, pool config.Pool, sl store.Slot, subject string) {
	if e.d.DryRun {
		e.rec.Record(ctx, "slot:"+sl.Name, "release", "resume the release of "+sl.Name+" for "+subject)
		return
	}
	if e.enqueueHeavy("evict:"+pool.Repo, e.evictJob(pool, sl.ID)) {
		e.event(ctx, "info", "slot:"+sl.Name, "slot.evict", "resuming the release of "+sl.Name+" for "+subject, nil)
	}
}

// provisionJob provisions (or resumes provisioning) one pool slot: the first
// one being provisioned, else a new one below pool.max.
func (e *Engine) provisionJob(pool config.Pool) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		sls, err := e.st.ListSlots(ctx, store.SlotFilter{RepoFullName: pool.Repo, Kind: store.SlotKindPool})
		if err != nil {
			return err
		}
		c := e.census(sls)
		n := -1
		for _, s := range append(c.resumable, c.backoff...) {
			if k, ok := slotNumber(pool, s.Name); ok {
				n = k
				break
			}
		}
		if n < 0 {
			if c.count >= pool.Max {
				return nil
			}
			if n, err = e.d.Slots.NextSlotNumber(ctx, pool); err != nil {
				return err
			}
		}
		err = e.d.Slots.ProvisionPool(ctx, pool, n)
		subject := "slot:" + pool.Slot(n)
		switch {
		case errors.Is(err, slots.ErrLowDisk):
			e.event(ctx, "warn", subject, "pool.low_disk", err.Error(), nil)
			e.urgent("lowdisk:"+pool.Repo, "magnum: low disk", "Cannot provision "+pool.Slot(n)+": "+err.Error(), identityToastSpan)
		case err != nil:
			e.event(ctx, "warn", subject, "pool.provision_failed", err.Error(), nil)
			if cause := infraCause(err); cause != "" {
				e.pauseInfra(ctx, cause, err, pool.MainClone)
			}
		default:
			e.event(ctx, "info", subject, "pool.provisioned", pool.Slot(n)+" provisioned", nil)
		}
		return err
	}
}

// evictJob parks the sessions of a held slot's PR (closing the panes the
// park retired) and releases the slot, or resumes a release that stalled
// (releasing, dirty_schema). The PR is
// reserved first (reserveEviction), so no round or open starts in the slot
// meanwhile, and one that holds the PR makes the eviction a no-op.
func (e *Engine) evictJob(pool config.Pool, slotID int64) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		sl, err := e.st.SlotByID(ctx, slotID)
		if err != nil {
			return err
		}
		if !evictable(sl) {
			return nil
		}
		if sl.PRID != nil {
			if !e.reserveEviction(*sl.PRID) {
				return nil
			}
			defer e.endEviction(*sl.PRID)
			// Re-read under the reservation: a round may have used the slot
			// since.
			if sl, err = e.st.SlotByID(ctx, slotID); err != nil {
				return err
			}
			if !evictable(sl) || sl.PRID == nil {
				return nil
			}
			if e.d.Agents != nil {
				pr, err := e.st.PRByID(ctx, *sl.PRID)
				if err != nil {
					return err
				}
				if err := e.d.Agents.Park(ctx, pr); err != nil {
					e.event(ctx, "info", "slot:"+sl.Name, "slot.evict_failed", "park sessions: "+err.Error(), nil)
					return err
				}
				e.closeRetiredPanes(ctx, pr) // retired_panes.go
			}
		}
		if err := e.releaseSlot(ctx, sl, pool, "evicted"); err != nil {
			e.event(ctx, "warn", "slot:"+sl.Name, "slot.evict_failed", err.Error(), nil)
			return err
		}
		e.event(ctx, "info", "slot:"+sl.Name, "slot.evicted", sl.Name+" released for another PR", nil)
		return nil
	}
}

// evictable reports a slot evictJob may release: held, unpinned and without
// a hold reason, or with a release in progress.
func evictable(sl store.Slot) bool {
	switch sl.State {
	case store.SlotHeld:
		return !sl.Pinned && sl.HoldReason == nil
	case store.SlotReleasing, store.SlotDirtySchema:
		return true
	}
	return false
}

// slotNumber parses n out of a pool slot name ("review3" for "review{n}").
func slotNumber(pool config.Pool, name string) (int, bool) {
	pre, suf, ok := strings.Cut(pool.SlotName, "{n}")
	if !ok || !strings.HasPrefix(name, pre) || !strings.HasSuffix(name, suf) || len(name) <= len(pre)+len(suf) {
		return 0, false
	}
	n, err := strconv.Atoi(name[len(pre) : len(name)-len(suf)])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// activeSlotCount counts a pool's slots that exist or are being made.
func activeSlotCount(sls []store.Slot) int {
	n := 0
	for _, s := range sls {
		if !slices.Contains([]string{store.SlotRemoved, store.SlotLost, store.SlotBroken, store.SlotRemoving}, s.State) {
			n++
		}
	}
	return n
}
