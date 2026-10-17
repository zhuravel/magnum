package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/slots"
)

// heldPR is actPR in slot review3, which the slot guard holds for hold.
func heldPR(hold string, edit func(r *PRBoardRow)) PRBoardRow {
	return actPR(func(r *PRBoardRow) {
		r.Slot, r.SlotHold = "review3", hold
		if edit != nil {
			edit(r)
		}
	})
}

// u on a PR that is not pinned but whose slot the guard holds for a moved HEAD
// or for commits on no remote asks to acknowledge the hold, and y sends the
// same unpin request the key always sends: the daemon takes the slot's HEAD as
// the checkout it expects and answers that it acknowledged the hold. Before,
// the board refused the row with "is not pinned" before any request existed,
// and the operator of talkable-integration#867 could not hand back a slot
// held head_drift from it. A row that is neither pinned nor held in a way a HEAD
// acknowledges keeps the refusal.
func TestUnpinKeyAcknowledgesAHeldSlot(t *testing.T) {
	for _, c := range []struct {
		name, hold string
		edit       func(r *PRBoardRow)
		want       string
	}{
		{"moved HEAD", "head_drift", nil,
			"Acknowledge the head_drift hold of slot review3 (takes its HEAD as the checkout)?"},
		{"commits on no remote", "unpushed_commits", nil,
			"Acknowledge the unpushed_commits hold of slot review3 (takes its HEAD as the checkout)?"},
		{"pinned as well", "head_drift", func(r *PRBoardRow) { r.Pinned = true },
			"Unpin talkable#5 and acknowledge the head_drift hold of slot review3 (takes its HEAD as the checkout)?"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, _, act := newBoard(t, 200, 24, PRBoardOptions{})
			m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{heldPR(c.hold, c.edit)}}, keyMsg("u"))
			if m.confirm == nil || m.confirm.question != c.want {
				t.Fatalf("u asked %+v, want %q", m.confirm, c.want)
			}
			mustContain(t, viewOf(m), c.want+" y/N")
			if len(act.calls) != 0 {
				t.Fatalf("u ran %v before y", act.calls)
			}
			if _, _ = boardAct(t, m, "y"); act.last() != "unpin talkable/talkable#5" {
				t.Fatalf("y called %q, want the unpin request", act.last())
			}
		})
	}

	t.Run("n sends nothing", func(t *testing.T) {
		m, _, act := newBoard(t, 200, 24, PRBoardOptions{})
		m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{heldPR("head_drift", nil)}}, keyMsg("u"))
		if _, _ = boardAct(t, m, "n"); len(act.calls) != 0 {
			t.Fatalf("n called %v", act.calls)
		}
	})

	for _, c := range []struct {
		name, hold string
	}{
		{"unpinned and not held", ""},
		{"a hold a HEAD does not acknowledge", "dirty_worktree"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, _, act := newBoard(t, 200, 24, PRBoardOptions{})
			m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{heldPR(c.hold, nil)}}, keyMsg("u"))
			if m.confirm != nil || len(act.calls) != 0 || !m.flashErr || !strings.Contains(m.flash, "talkable#5 is not pinned") {
				t.Fatalf("u asked %+v / flashed %q (error %v) / called %v, want the refusal \"is not pinned\"", m.confirm, m.flash, m.flashErr, act.calls)
			}
		})
	}
}

// x on a row whose slot the guard holds for a moved HEAD or for commits on no
// remote says so and names the key that hands it back, where the release would
// skip the slot without a word (the plan's "hold_reason" skip).
func TestReleaseKeyOnAHeldSlotSaysHowToAcknowledgeIt(t *testing.T) {
	for _, c := range []struct {
		hold, want string
	}{
		{"head_drift", "talkable#5: slot review3 is held: head_drift, acknowledge it with u"},
		{"unpushed_commits", "talkable#5: slot review3 is held: unpushed_commits, acknowledge it with u"},
	} {
		r := boardActRow(heldPR(c.hold, nil), "talkable#5", boardNow)
		if got := actionRefusal(actRelease, r); got != c.want {
			t.Errorf("release of a slot held %s: refusal %q, want %q", c.hold, got, c.want)
		}
	}
	// What is not a hold a HEAD acknowledges is released as before, and a
	// pinned row still says to unpin first.
	for _, hold := range []string{"", "dirty_worktree"} {
		if got := actionRefusal(actRelease, boardActRow(heldPR(hold, nil), "talkable#5", boardNow)); got != "" {
			t.Errorf("release of a slot held %q: refused %q", hold, got)
		}
	}
	pinned := boardActRow(heldPR("head_drift", func(r *PRBoardRow) { r.Pinned = true }), "talkable#5", boardNow)
	if got := actionRefusal(actRelease, pinned); got != "talkable#5 is pinned: unpin first (u)" {
		t.Errorf("release of a pinned, held row: %q", got)
	}

	m, _, act := newBoard(t, 200, 24, PRBoardOptions{})
	m, cmd := send(t, m, prbDataMsg{rows: []PRBoardRow{heldPR("head_drift", nil)}}, keyMsg("x"))
	if m.confirm != nil || m.busy != "" || hasMsg[actionDoneMsg](execCmd(t, cmd)) || len(act.calls) != 0 {
		t.Fatalf("x asked (%+v) or ran (%q, %v)", m.confirm, m.busy, act.calls)
	}
	if !m.flashErr || !strings.Contains(m.flash, "held: head_drift, acknowledge it with u") {
		t.Fatalf("x flashed %q (error %v)", m.flash, m.flashErr)
	}
}

// The card's ACTIONS and the `?` help name the key for what it does on a
// held slot: unpin, or acknowledge a hold. A row whose slot holds nothing
// keeps "unpin" on its card, and a row that is not pinned offers nothing.
func TestTheUnpinKeyIsNamedForAcknowledgingAHold(t *testing.T) {
	labels := func(r PRBoardRow) []string {
		var out []string
		for _, h := range actionCard(boardActs, boardActRow(r, "", boardNow)) {
			if h.key == "u" {
				out = append(out, h.desc)
			}
		}
		return out
	}
	if got := labels(heldPR("head_drift", nil)); !slices.Equal(got, []string{"unpin / acknowledge a hold"}) {
		t.Errorf("card of an unpinned, held row: %q, want the acknowledge label", got)
	}
	if got := labels(heldPR("head_drift", func(r *PRBoardRow) { r.Pinned = true })); !slices.Equal(got, []string{"unpin / acknowledge a hold"}) {
		t.Errorf("card of a pinned, held row: %q, want the acknowledge label", got)
	}
	if got := labels(heldPR("", func(r *PRBoardRow) { r.Pinned = true })); !slices.Equal(got, []string{"unpin"}) {
		t.Errorf("card of a pinned row: %q, want plain unpin", got)
	}
	if got := labels(heldPR("", nil)); len(got) != 0 {
		t.Errorf("card of a row that is not pinned offers %q", got)
	}

	m, _, _ := newBoard(t, 120, 44, PRBoardOptions{})
	m, _ = send(t, m, keyMsg("?"))
	mustContain(t, viewOf(m), "pin / unpin / acknowledge a hold")
}

// The screens cannot import the slots package, so the holds they offer to
// acknowledge are spelled again: they must stay the guard's names.
func TestTheBoardsHoldNamesAreTheSlotsHoldNames(t *testing.T) {
	if holdHeadDrift != slots.HoldHeadDrift || holdUnpushed != slots.HoldUnpushed {
		t.Fatalf("the board offers to acknowledge %q and %q, the guard persists %q and %q",
			holdHeadDrift, holdUnpushed, slots.HoldHeadDrift, slots.HoldUnpushed)
	}
}
