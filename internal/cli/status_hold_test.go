package cli

import (
	"context"
	"testing"
)

// `magnum status` names a held slot's hand-back as the command to type, with
// the slot in quotes (a per-PR worktree's name, owner/name#N, is not a shell
// word to leave bare): the same `magnum unpin "<slot>"` every other hold
// sentence gives.
func TestAHoldHintNamesTheUnpinCommandWithTheSlot(t *testing.T) {
	_, _, d, _ := statusFixture(t)
	d.Inventory.(*fakeScanner).inv.Slots[0].Slot.HoldReason = new("head_drift")
	r, err := statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	const want = "check the folder, then `magnum unpin \"review1\"`"
	for _, a := range r.Attention {
		if a.Subject == "slot review1" {
			if a.Message != "held: head_drift" || a.Fix != want {
				t.Fatalf("attention = %q, fix %q, want held: head_drift with %q", a.Message, a.Fix, want)
			}
			return
		}
	}
	t.Fatalf("no attention row for the held slot: %+v", r.Attention)
}
