package tui

import (
	"strings"
	"testing"
)

// The dashboard reads a slot's pin from SlotRow.Pinned, never from the
// display string: a slot held for a reason that says "pinned" is not
// pinned, so x releases it, p pins it and the review question does not say
// the review unpins it; a pinned slot still is.
func TestASlotHoldReasonSayingPinnedDoesNotPinIt(t *testing.T) {
	data := dashData()
	data.Slots = append(data.Slots,
		SlotRow{Name: "review3", PRRef: "talkable#11", PRState: "reviewed", State: "held", SlotState: "held [hold:pinned branch moved]", PRGHState: "OPEN"},
		SlotRow{Name: "review4", PRRef: "talkable#12", PRState: "reviewed", State: "held", Pinned: true, SlotState: "held [pinned]", PRGHState: "OPEN"})
	m, _, _ := newDash(t, 220, 60)
	m, _ = send(t, m, dashDataMsg{data: data})
	at := func(key string) {
		t.Helper()
		for i, r := range m.rows {
			if r.key() == key {
				m.moveTo(i)
				return
			}
		}
		t.Fatalf("no row %s", key)
	}
	at("slot:review3")
	if r := m.actRow(); r.pinned {
		t.Fatalf("a hold reason saying pinned pins the slot: %+v", r)
	}
	for _, k := range []string{"x", "p"} {
		if why := actionRefusal(map[string]rowAct{"x": actRelease, "p": actPin}[k], m.actRow()); why != "" {
			t.Errorf("%s on the held slot is refused: %q", k, why)
		}
	}
	if q := actionQuestion(actReview, m.actRow()); strings.Contains(q, "unpins") {
		t.Errorf("the review question says the review unpins the held slot: %q", q)
	}
	at("slot:review4")
	if why := actionRefusal(actRelease, m.actRow()); !strings.Contains(why, "is pinned") {
		t.Errorf("x on the pinned slot: %q, want a refusal", why)
	}
	if q := actionQuestion(actReview, m.actRow()); !strings.Contains(q, "pinned: the review unpins it") {
		t.Errorf("the review question on the pinned slot: %q", q)
	}
}
