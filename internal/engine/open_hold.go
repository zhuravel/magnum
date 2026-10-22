package engine

// The hold of `magnum open` (spec 121). A restore by `magnum open` (or the
// board's o, which sends the same request) pins the PR and its slot, so
// every pin's protection applies unchanged: no round, park, release,
// eviction or cleanup touches them. Unless [daemon] open_hold is 0, the pin
// is also a hold that ends by itself: the PR's KVPROpenHold record says
// until when (the open plus open_hold), and the hold ends at the later of
// that and human_cooldown after the last input in the PR's panes
// (OpenHold.End). Then the tick unpins the PR and its slot with one
// pr.open_hold_ended event (endOpenHolds), before dispatch, so a paused or
// queued round goes on in the same tick. The record lives in the registry,
// so a restart keeps the hold; `magnum pin` drops it (the pin lasts), every
// unpin drops it too, and a record whose PR is no longer pinned holds
// nothing and goes. A focus of a live pane (nothing restored) holds and pins
// nothing. At the start a lasting pin `magnum open` made before holds
// existed (its newest pin event is the open's) becomes a hold ending
// open_hold after the start (convertOpenPins).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// Event kinds of an open's hold: it ended by itself (endOpenHolds), and a
// lasting pin of an earlier open became a hold at the start
// (convertOpenPins).
const (
	evPROpenHoldEnded = "pr.open_hold_ended"
	evPROpenHoldSet   = "pr.open_hold_set"
)

// KVPROpenHold holds the hold `magnum open` set on a PR it restored
// (OpenHold as JSON).
func KVPROpenHold(prID int64) string { return fmt.Sprintf("pr.%d.open_hold", prID) }

// openHoldGlob matches every KVPROpenHold key.
const openHoldGlob = "pr.*.open_hold"

// OpenHold is a PR's hold by `magnum open`: At is when it was set (the open,
// or the start that converted the open's lasting pin) and Until At plus
// open_hold. Input in the PR's panes extends it (End).
type OpenHold struct {
	Until time.Time `json:"until"`
	At    time.Time `json:"at"`
}

// ParseOpenHold reads a KVPROpenHold value; ok is false for "" or a value it
// cannot read.
func ParseOpenHold(s string) (OpenHold, bool) {
	var h OpenHold
	if s == "" || json.Unmarshal([]byte(s), &h) != nil || h.Until.IsZero() {
		return OpenHold{}, false
	}
	return h, true
}

// End is when the hold ends: Until, or human_cooldown after humanAt (the
// PR's human_active_at, the last input in its panes) when that is later.
func (h OpenHold) End(d config.Daemon, humanAt *time.Time) time.Time {
	return d.OpenHoldEnd(h.Until, humanAt)
}

func (h OpenHold) marshal() (string, error) {
	b, err := json.Marshal(h)
	return string(b), err
}

// OpenHolds reads every PR's hold record in one query, by PR id; a record
// it cannot read is left out. A record of a PR no longer pinned holds
// nothing (the next tick drops it): callers read it beside the PR's pin.
func OpenHolds(ctx context.Context, st *store.Store) (map[int64]OpenHold, error) {
	vals, err := st.KVValues(ctx, openHoldGlob)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]OpenHold, len(vals))
	for k, v := range vals {
		id, ok := openHoldPR(k)
		if !ok {
			continue
		}
		if h, ok := ParseOpenHold(v); ok {
			out[id] = h
		}
	}
	return out, nil
}

// OpenHoldOf is PR prID's hold record, whether its PR is still pinned or
// not (OpenHolds).
func OpenHoldOf(ctx context.Context, st *store.Store, prID int64) (OpenHold, bool) {
	v, ok, err := st.GetKV(ctx, KVPROpenHold(prID))
	if err != nil || !ok {
		return OpenHold{}, false
	}
	return ParseOpenHold(v)
}

// openHoldPR is the PR id of a KVPROpenHold key.
func openHoldPR(key string) (int64, bool) {
	s, ok := strings.CutPrefix(key, "pr.")
	if !ok {
		return 0, false
	}
	if s, ok = strings.CutSuffix(s, ".open_hold"); !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil
}

// openHold is the PR's hold record.
func (e *Engine) openHold(ctx context.Context, prID int64) (OpenHold, bool) {
	v, _ := e.getKV(ctx, KVPROpenHold(prID))
	return ParseOpenHold(v)
}

// holdOpened records the hold of pr, which `magnum open` restores and is
// about to pin (pinned: pr or its slot is pinned already): until now plus
// open_hold, a hold it had renewed. It records nothing, and reports false,
// when open_hold is 0 or a lasting pin (`magnum pin`, a pin without a hold)
// keeps the PR already: the restore leaves that pin as it is. The record is
// written before the pin, so a crash in between leaves a record the next
// tick drops (its PR is not pinned), never a lasting pin.
func (e *Engine) holdOpened(ctx context.Context, pr store.PR, pinned bool) (OpenHold, bool, error) {
	d := e.cfg.Daemon.OpenHold.Duration
	if d <= 0 {
		return OpenHold{}, false, nil
	}
	if _, held := e.openHold(ctx, pr.ID); pinned && !held {
		return OpenHold{}, false, nil
	}
	now := e.now().UTC()
	h := OpenHold{Until: now.Add(d), At: now}
	v, err := h.marshal()
	if err != nil {
		return OpenHold{}, false, err
	}
	if err := e.st.SetKV(ctx, KVPROpenHold(pr.ID), v); err != nil {
		return OpenHold{}, false, fmt.Errorf("record the hold: %w", err)
	}
	return h, true, nil
}

// dropOpenHold forgets the PR's hold: a pin made lasting, or an unpin.
// It reports whether there was one.
func (e *Engine) dropOpenHold(ctx context.Context, prID int64) bool {
	_, held := e.openHold(ctx, prID)
	e.delKV(ctx, KVPROpenHold(prID))
	return held
}

// heldWords is what a reply or a wait says of a hold that ends at end:
// "held by magnum open until 03:21".
func heldWords(end, now time.Time) string {
	return "held by magnum open until " + waitClock(end, now)
}

// endOpenHolds ends every hold that is over, before dispatch: the PR and its
// slot are unpinned as `magnum unpin` unpins them, except that a hold a
// slot guard persisted (a person's changes, unpushed commits, a moved HEAD)
// stays, as a review's unpin leaves it (unpinFor): magnum never takes a
// slot's HEAD as its checkout unasked. One pr.open_hold_ended event says so.
// A PR some work holds (an open that is pinning it, a round, a park) waits
// for the next tick; a record whose PR is gone or no longer pinned goes
// without a word.
func (e *Engine) endOpenHolds(ctx context.Context) {
	if e.d.DryRun {
		return
	}
	holds, err := OpenHolds(ctx, e.st)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "open holds")
		return
	}
	now := e.now()
	for id, h := range holds {
		if e.roundActive(id) { // an open writes the record before the pin (holdOpened)
			continue
		}
		pr, err := e.st.PRByID(ctx, id)
		switch {
		case errors.Is(err, store.ErrNotFound) || err == nil && !pr.Pinned:
			e.delKV(ctx, KVPROpenHold(id))
			continue
		case err != nil:
			e.warnUnlessStopped(ctx, err, "open hold: the PR", "pr", id)
			continue
		}
		if end := h.End(e.cfg.Daemon, pr.HumanActiveAt); now.Before(end) {
			continue
		}
		if !e.reserveSlotWork(id, "magnum open's hold is ending") {
			continue
		}
		e.endOpenHold(ctx, pr, h, now)
		e.endEviction(id)
	}
}

// endOpenHold unpins pr and its slot (under the PR's reservation), drops
// the hold record and writes the event.
func (e *Engine) endOpenHold(ctx context.Context, pr store.PR, h OpenHold, now time.Time) {
	repo, err := e.st.RepoByID(ctx, pr.RepoID)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "open hold: repository", "pr", pr.ID)
		return
	}
	subject := prSubject(repo, pr.Number)
	if err := e.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) { u.Set("pinned", false) }); err != nil {
		e.warnUnlessStopped(ctx, err, "open hold: unpin", "subject", subject)
		return
	}
	what := "the PR"
	slot, has, err := e.slotOf(ctx, pr.ID)
	if err == nil && has {
		what = slot.Name
		if slot.Pinned && e.d.Slots != nil {
			if err := e.d.Slots.ClearPin(ctx, slot); err != nil {
				// The PR's pin went: dispatch waits on the slot's, which
				// `magnum unpin` lifts; the record goes so the tick does not
				// try every time.
				e.event(ctx, "warn", subject, evPROpenHoldEnded, fmt.Sprintf("magnum open's hold ended, but %s stays pinned: %v (`magnum unpin %s#%d`)",
					slot.Name, err, repo.FullName(), pr.Number), map[string]any{"slot": slot.Name})
				e.delKV(ctx, KVPROpenHold(pr.ID))
				return
			}
		}
	}
	e.delKV(ctx, KVPROpenHold(pr.ID))
	msg := fmt.Sprintf("magnum open's hold (set %s) ended: unpinned %s, its rounds go on", pastClock(h.At, now), what)
	if c := e.cfg.Daemon.CooldownUntil(pr.HumanActiveAt); c.After(h.Until) {
		msg = fmt.Sprintf("magnum open's hold (set %s, last input in its panes %s) ended: unpinned %s, its rounds go on",
			pastClock(h.At, now), pastClock(*pr.HumanActiveAt, now), what)
	}
	e.event(ctx, "info", subject, evPROpenHoldEnded, msg, map[string]any{"slot": slot.Name, "at": h.At, "until": h.Until})
}

// convertOpenPins runs at the start: a pinned PR without a hold whose newest
// pin event is `magnum open`'s (pinOrigin: it was restored before holds
// existed, or while open_hold was 0) gets a hold ending open_hold after the
// start, with one pr.open_hold_set event; a pin `magnum pin` or `magnum
// slots pin` made, or one the events no longer remember, stays as it is.
// open_hold = 0 converts nothing.
func (e *Engine) convertOpenPins(ctx context.Context) {
	d := e.cfg.Daemon.OpenHold.Duration
	if d <= 0 || e.d.DryRun {
		return
	}
	prs, err := e.st.ListPRs(ctx, store.PRFilter{})
	if err != nil {
		e.warnUnlessStopped(ctx, err, "open holds: pinned PRs")
		return
	}
	now := e.now()
	for _, pr := range prs {
		if !pr.Pinned {
			continue
		}
		if _, held := e.openHold(ctx, pr.ID); held {
			continue
		}
		repo, err := e.st.RepoByID(ctx, pr.RepoID)
		if err != nil {
			continue
		}
		subject := prSubject(repo, pr.Number)
		by, at := e.pinOrigin(ctx, subject) // review_unpin.go
		if by != "magnum open" {
			continue
		}
		h := OpenHold{Until: now.Add(d).UTC(), At: now.UTC()}
		v, err := h.marshal()
		if err != nil {
			continue
		}
		e.setKV(ctx, KVPROpenHold(pr.ID), v)
		e.event(ctx, "info", subject, evPROpenHoldSet, fmt.Sprintf("the pin magnum open set at %s is now a hold until %s (open_hold %s)",
			pastClock(at, now), waitClock(h.Until, now), humanDuration(d)), map[string]any{"until": h.Until, "opened": at})
	}
}
