package engine

// A review the operator asks for (`magnum review`, the board's r) of a PR
// that is pinned unpins it: `magnum open` pins the PR it restores, and a
// later review would otherwise wait behind that pin without a word
// (slotGate). A fix the operator asks for (`magnum fix`, the board's f)
// unpins it the same way (unpinFor; fix.go prepareFix). Only the pin goes. A
// hold a slot guard persisted (a person's changes, unpushed commits, a moved
// HEAD) stays, and so does every live check of the next checkout (a
// person's agent or process in the slot): the reply and the PR's wait name
// the guard that keeps the slot.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// Event kinds of a pin: magnum open pins the PR it restores (pr.opened),
// magnum pin and magnum slots pin pin it (pr.pinned), magnum unpin and a
// review or fix request unpin it (pr.unpinned).
const (
	evPROpened   = "pr.opened"
	evPRPinned   = "pr.pinned"
	evPRUnpinned = "pr.unpinned"
)

// unpinForReview unpins pr (and its slot) for a review the operator asked
// for (unpinFor). note is what the reply says about it ("unpinned review1
// (pinned by magnum open at 12:35) to review it", "" when nothing was
// pinned); held is why the slot still cannot take the round ("slot review1
// is held: dirty_worktree (magnum unpin)", "" when nothing holds it), which
// is also recorded as the PR's gate, so its wait names the guard until the
// next dispatch looks again.
func (e *Engine) unpinForReview(ctx context.Context, repo store.Repo, pr store.PR) (note, held string) {
	slot, has, err := e.slotOf(ctx, pr.ID)
	if err != nil {
		e.log.Warn("review request: the PR's slot", "pr", pr.ID, "err", err)
		return "", ""
	}
	if !isPinned(pr, slot, has) {
		if has && deref(slot.HoldReason) != "" {
			return "", slotHeldReason(slot)
		}
		return "", ""
	}
	note, held, err = e.unpinFor(ctx, repo, pr, slot, has, "magnum review", "review")
	if err != nil {
		return err.Error(), ""
	}
	if held != "" {
		e.noteGate(ctx, pr.ID, gate{reason: WaitSlot, text: held})
	}
	return note, held
}

// isPinned reports whether pr or its slot (has: it has one) is pinned.
func isPinned(pr store.PR, slot store.Slot, has bool) bool { return pr.Pinned || (has && slot.Pinned) }

// unpinFor takes the pin off pr and off its slot (has: it has one) for what
// the operator asked for: by is the command ("magnum review", "magnum fix"),
// which the pr.unpinned event names, and verb what it does ("review",
// "fix"). Then it runs the slot's guard (unpinnedSlotHold). note is what the
// reply says ("unpinned review1 (pinned by magnum open at 12:35) to fix
// it"), held why the slot still cannot take the work ("" when nothing holds
// it), and err an unpin that failed, in a sentence that names `magnum
// unpin`.
func (e *Engine) unpinFor(ctx context.Context, repo store.Repo, pr store.PR, slot store.Slot, has bool, by, verb string) (note, held string, err error) {
	what, origin := e.pinnedWhat(ctx, repo, pr, slot, has)
	failed := func(err error) error {
		return fmt.Errorf("could not unpin %s: %w (`magnum unpin %s#%d`)", what, err, repo.FullName(), pr.Number)
	}
	if pr.Pinned {
		if err := e.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) { u.Set("pinned", false) }); err != nil {
			return "", "", failed(err)
		}
	}
	if has && slot.Pinned && e.d.Slots != nil {
		if err := e.d.Slots.ClearPin(ctx, slot); err != nil {
			return "", "", failed(err)
		}
	}
	e.dropOpenHold(ctx, pr.ID) // open_hold.go: the unpin ends an open's hold too
	note = "unpinned " + what + origin + " to " + verb + " it"
	e.event(ctx, "info", prSubject(repo, pr.Number), evPRUnpinned, by+": "+note, map[string]any{"by": by, "slot": slot.Name})
	if has {
		held = e.unpinnedSlotHold(ctx, slot)
	}
	return note, held, nil
}

// pinnedWhat is what an unpin of pr takes the pin off, its slot (has: it
// has one) or else "the PR", and origin who pinned it when, as a reply says
// it (" (pinned by magnum open at 12:35)"; "" when the pin events do not
// say: pinOrigin).
func (e *Engine) pinnedWhat(ctx context.Context, repo store.Repo, pr store.PR, slot store.Slot, has bool) (what, origin string) {
	what = "the PR"
	if has {
		what = slot.Name
	}
	if by, at := e.pinOrigin(ctx, prSubject(repo, pr.Number)); by != "" {
		origin = fmt.Sprintf(" (pinned by %s at %s)", by, pastClock(at, e.now()))
	}
	return what, origin
}

// unpinnedSlotHold runs the slot's guard after the pin went: "" when it lets
// a round or a fixer in, else why it does not, in the words dispatch uses for
// a held slot (slotGate) or, for a live hold (a person's agent or process in
// the slot), the guard's own. A guard that could not look (herdr
// unreachable) is left to the round's checkout or the fix job's guard.
func (e *Engine) unpinnedSlotHold(ctx context.Context, slot store.Slot) string {
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

// slotHeldReason is slotGate's sentence for a slot with a persisted hold. It
// names the command that hands the slot back, with the slot in quotes ("slot
// review1 is held: head_drift (magnum unpin "review1")"): a per-PR
// worktree's name, owner/name#N, is also a PR reference, and the command takes
// either.
func slotHeldReason(slot store.Slot) string {
	return "slot " + slot.Name + " is held: " + deref(slot.HoldReason) + " (magnum unpin " + strconv.Quote(slot.Name) + ")"
}

// acknowledgedHold is the answer of an unpin that took no pin off but handed
// back a slot its guard held for a moved HEAD (head_drift) or for commits on
// no remote (unpushed_commits): slots.Manager.Unpin took the folder's HEAD as
// the checkout magnum expects and cleared the hold, so the next release
// resets the slot. before is the slot as it was before the unpin; "" when it
// held nothing of the kind, or when the unpin left it held (the caller keeps
// its own answer).
func (e *Engine) acknowledgedHold(ctx context.Context, before store.Slot) string {
	hold := deref(before.HoldReason)
	if hold != slots.HoldHeadDrift && hold != slots.HoldUnpushed {
		return ""
	}
	cur, err := e.st.SlotByID(ctx, before.ID)
	if err != nil || cur.HoldReason != nil || deref(cur.CheckedOutSHA) == "" {
		return ""
	}
	return fmt.Sprintf("acknowledged the %s hold of slot %s: HEAD %s is now the checkout magnum expects; the next release resets it",
		hold, before.Name, textx.ShortSHA(deref(cur.CheckedOutSHA)))
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
// unpin, or the end of an open's hold, is newer.
func (e *Engine) pinOrigin(ctx context.Context, subject string) (string, time.Time) {
	evs, err := e.st.EventsOfKindsSince(ctx, time.Time{}, evPROpened, evPRPinned, evPRUnpinned, evPROpenHoldEnded)
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
