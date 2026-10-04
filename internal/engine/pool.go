package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

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
		e.log.Warn("slots", "err", err)
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
		e.log.Warn("evictable slots", "err", err)
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
	for _, s := range sls {
		if s.State != store.SlotReleasing && s.State != store.SlotDirtySchema {
			continue
		}
		if s.PRID != nil {
			if pr, err := e.st.PRByID(ctx, *s.PRID); err == nil && slices.Contains(closingStates, pr.State) {
				continue
			}
		}
		return s, true
	}
	return store.Slot{}, false
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

// evictJob parks the sessions of a held slot's PR and releases the slot, or
// resumes a release that stalled (releasing, dirty_schema). The PR is
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
			}
		}
		if err := e.d.Slots.Release(ctx, sl, pool, "evicted"); err != nil {
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
