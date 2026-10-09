package engine

// A review the operator asks for (`magnum review`, the board's r) of a PR
// that is pinned unpins it: `magnum open` pins the PR it restores, and a
// later review would otherwise wait behind that pin without a word
// (slotGate). Only the pin goes. A hold a slot guard persisted (a person's
// changes, unpushed commits, a moved HEAD) stays, and so does every live
// check of the next checkout (a person's agent or process in the slot): the
// reply and the PR's wait name the guard that keeps the slot.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
)

// Event kinds of a pin: magnum open pins the PR it restores (pr.opened),
// magnum pin and magnum slots pin pin it (pr.pinned), magnum unpin and a
// review request unpin it (pr.unpinned).
const (
	evPROpened   = "pr.opened"
	evPRPinned   = "pr.pinned"
	evPRUnpinned = "pr.unpinned"
)

// unpinForReview unpins pr (and its slot) for a review the operator asked
// for. note is what the reply says about it ("unpinned review1 (pinned by
// magnum open at 12:35) to review it", "" when nothing was pinned); held is
// why the slot still cannot take the round ("slot review1 is held:
// dirty_worktree (magnum unpin)", "" when nothing holds it), which is also
// recorded as the PR's gate, so its wait names the guard until the next
// dispatch looks again.
func (e *Engine) unpinForReview(ctx context.Context, repo store.Repo, pr store.PR) (note, held string) {
	slot, has, err := e.slotOf(ctx, pr.ID)
	if err != nil {
		e.log.Warn("review request: the PR's slot", "pr", pr.ID, "err", err)
		return "", ""
	}
	if !pr.Pinned && !(has && slot.Pinned) {
		if has && deref(slot.HoldReason) != "" {
			return "", slotHeldReason(slot)
		}
		return "", ""
	}
	subject := prSubject(repo, pr.Number)
	by, at := e.pinOrigin(ctx, subject)
	what := "the PR"
	if has {
		what = slot.Name
	}
	if by != "" {
		note = fmt.Sprintf("unpinned %s (pinned by %s at %s) to review it", what, by, pastClock(at, e.now()))
	} else {
		note = fmt.Sprintf("unpinned %s to review it", what)
	}
	if pr.Pinned {
		if err := e.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) { u.Set("pinned", false) }); err != nil {
			return fmt.Sprintf("could not unpin %s: %v (`magnum unpin %s#%d`)", what, err, repo.FullName(), pr.Number), ""
		}
	}
	if has && slot.Pinned && e.d.Slots != nil {
		if err := e.d.Slots.ClearPin(ctx, slot); err != nil {
			return fmt.Sprintf("could not unpin %s: %v (`magnum unpin %s#%d`)", what, err, repo.FullName(), pr.Number), ""
		}
	}
	e.event(ctx, "info", subject, evPRUnpinned, "magnum review: "+note, map[string]any{"by": "magnum review", "slot": slot.Name})
	if has {
		held = e.reviewSlotHold(ctx, slot)
	}
	if held != "" {
		e.noteGate(ctx, pr.ID, gate{reason: WaitSlot, text: held})
	}
	return note, held
}

// reviewSlotHold runs the slot's guard after the pin went: "" when it lets a
// round in, else why it does not, in the words dispatch uses for a held slot
// (slotGate) or, for a live hold (a person's agent or process in the slot),
// the guard's own. A guard that could not look (herdr unreachable) is left
// to the round's checkout.
func (e *Engine) reviewSlotHold(ctx context.Context, slot store.Slot) string {
	var live error
	if e.d.Slots != nil && !e.d.DryRun {
		live = e.d.Slots.Guard(ctx, slot)
	}
	if cur, err := e.st.SlotByID(ctx, slot.ID); err == nil {
		slot = cur
	}
	if deref(slot.HoldReason) != "" {
		return slotHeldReason(slot)
	}
	if h, ok := slots.AsHold(live); ok && h.Reason != slots.HoldPinned {
		why := "slot " + slot.Name + " is held: " + h.Reason
		if h.Detail != "" {
			why += ": " + h.Detail
		}
		return why
	}
	return ""
}

// slotHeldReason is slotGate's sentence for a slot with a persisted hold.
func slotHeldReason(slot store.Slot) string {
	return "slot " + slot.Name + " is held: " + deref(slot.HoldReason) + " (magnum unpin)"
}

// pinEvent records a pin or unpin request (magnum pin|unpin, magnum slots
// pin|unpin) on the PR it concerns (subject, else the PR the slot holds), so
// a later review request can say who pinned it (pinOrigin). A slot without
// a PR gets a slot.pinned or slot.unpinned event.
func (e *Engine) pinEvent(ctx context.Context, subject string, slot store.Slot, bySlot, pin bool, msg string) {
	by := map[bool]string{true: "magnum pin", false: "magnum unpin"}[pin]
	if bySlot {
		by = map[bool]string{true: "magnum slots pin", false: "magnum slots unpin"}[pin]
	}
	if subject == "" && slot.PRID != nil {
		if pr, err := e.st.PRByID(ctx, *slot.PRID); err == nil {
			if repo, err := e.st.RepoByID(ctx, pr.RepoID); err == nil {
				subject = prSubject(repo, pr.Number)
			}
		}
	}
	kind := map[bool]string{true: evPRPinned, false: evPRUnpinned}[pin]
	if subject == "" {
		subject, kind = "slot:"+slot.Name, map[bool]string{true: "slot.pinned", false: "slot.unpinned"}[pin]
	}
	e.event(ctx, "info", subject, kind, by+": "+msg, map[string]any{"by": by, "slot": slot.Name})
}

// pinOrigin is who pinned the PR of subject and when: the newest pin event
// of the subject ("magnum open", "magnum pin", "magnum slots pin"); "" when
// there is none (a pin older than the events kept, or set by hand) or an
// unpin is newer.
func (e *Engine) pinOrigin(ctx context.Context, subject string) (string, time.Time) {
	evs, err := e.st.EventsOfKindsSince(ctx, time.Time{}, evPROpened, evPRPinned, evPRUnpinned)
	if err != nil {
		e.log.Warn("pin events", "subject", subject, "err", err)
		return "", time.Time{}
	}
	for _, ev := range slices.Backward(evs) {

		if deref(ev.Subject) != subject {
			continue
		}
		switch ev.Kind {
		case evPROpened:
			return "magnum open", ev.At
		case evPRPinned:
			var d struct {
				By string `json:"by"`
			}
			if json.Unmarshal(ev.Data, &d) == nil && d.By != "" {
				return d.By, ev.At
			}
			return "magnum pin", ev.At
		}
		return "", time.Time{}
	}
	return "", time.Time{}
}

// pastClock is a past t as a reply shows it: "12:35" on now's day, else
// "Oct 4 21:04".
func pastClock(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}
