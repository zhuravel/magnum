package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// pinnedClosedWindow is how long the toast "its slot stays pinned" for one
// PR is deduplicated in the registry (the close happens once, in one
// transition; the key keeps a restart that replays the poll from saying it
// twice).
const pinnedClosedWindow = 24 * time.Hour

// Event kinds of a pin on a slot without a PR (pinEvent), beside the PR's
// own (evPRPinned and the others, review_unpin.go).
const (
	evSlotPinned   = "slot.pinned"
	evSlotUnpinned = "slot.unpinned"
)

// closedPinnedSlot is the slot of pr, a PR just confirmed closed, and whether
// a pin keeps it: the PR or its slot is pinned. A pin never lapses, so
// cleanup's plan skips the slot (cleanup.SkipPinned, "unpin first") whatever
// the close grace says. A PR without a slot keeps nothing.
func (e *Engine) closedPinnedSlot(ctx context.Context, pr store.PR) (store.Slot, bool) {
	slot, has, err := e.slotOf(ctx, pr.ID)
	if err != nil || !has {
		return store.Slot{}, false
	}
	return slot, pr.Pinned || slot.Pinned
}

// noteClosedPinned toasts, once per PR, that GitHub closed a PR whose slot a
// pin keeps: the close event says the slot is kept, and nothing else would
// tell the operator that a slot and a worktree are held for a PR that is
// gone.
func (e *Engine) noteClosedPinned(repo store.Repo, pr store.PR, slot store.Slot) {
	e.urgent(fmt.Sprintf("closed-pinned:%d", pr.ID), fmt.Sprintf("magnum: %s#%d closed", repo.Name, pr.Number),
		fmt.Sprintf("its slot %s stays pinned (magnum unpin)", slot.Name), pinnedClosedWindow)
}

// PinnedSince is when each subject that is pinned now was pinned, by the
// registry's pin events: "pr:owner/name#N" for a PR pinned by `magnum open`,
// `magnum pin` or `magnum slots pin` (pr.opened, pr.pinned), "slot:<name>"
// for a slot pinned without a PR (slot.pinned); an unpin, or the end of an
// open's hold (pr.open_hold_ended), forgets the subject. A pin older than
// the events kept (store.Prune), or set by hand, has no entry.
func PinnedSince(ctx context.Context, st *store.Store) (map[string]time.Time, error) {
	evs, err := st.EventsOfKindsSince(ctx, time.Time{}, evPROpened, evPRPinned, evPRUnpinned, evSlotPinned, evSlotUnpinned, evPROpenHoldEnded)
	if err != nil {
		return nil, err
	}
	out := map[string]time.Time{}
	for _, ev := range evs {
		subject := deref(ev.Subject)
		switch ev.Kind {
		case evPROpened, evPRPinned, evSlotPinned:
			out[subject] = ev.At
		case evPRUnpinned, evSlotUnpinned, evPROpenHoldEnded:
			delete(out, subject)
		}
	}
	return out, nil
}
