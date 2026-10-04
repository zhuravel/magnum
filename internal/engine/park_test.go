package engine

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

func (h *harness) parks(prID int64) int {
	return len(slices.DeleteFunc(h.ag.all(), func(c string) bool { return c != fmt.Sprintf("park:%d", prID) }))
}

// TestIdleAgentsOfReviewedPRsAreParked: once every live agent of a reviewed
// PR has been idle for park_idle_after (2h by default) its sessions are
// parked, once; before that, nothing happens.
func TestIdleAgentsOfReviewedPRsAreParked(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	before := h.parks(pr.ID)

	h.advance(time.Hour)
	h.tick()
	if h.parks(pr.ID) != before {
		t.Fatal("parked before park_idle_after")
	}
	h.advance(61 * time.Minute)
	h.tick()
	if h.parks(pr.ID) != before+1 {
		t.Fatalf("parks = %d, want %d: %v", h.parks(pr.ID), before+1, h.ag.all())
	}
	live, err := h.st.LiveSessions(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(live, func(s store.Session) bool { return s.PRID == pr.ID }) {
		t.Fatal("sessions still live after the park")
	}
	h.advance(3 * time.Hour)
	h.tick()
	if h.parks(pr.ID) != before+1 {
		t.Fatal("parked again without live sessions")
	}
}

// TestParkIdleLeavesPinnedBusyAndHumanPRsAlone: a pinned slot, a working
// agent, a recent human keystroke or park_idle_after = 0 keep the agents.
func TestParkIdleLeavesPinnedBusyAndHumanPRsAlone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *harness, pr store.PR)
	}{
		{"pinned", func(h *harness, pr store.PR) {
			sl, has, err := h.e.slotOf(h.ctx, pr.ID)
			if err != nil || !has {
				t.Fatalf("slot: %v %v", has, err)
			}
			if err := h.st.UpdateSlotFields(h.ctx, sl.ID, func(u *store.SlotUpdate) { u.Set("pinned", true) }); err != nil {
				t.Fatal(err)
			}
		}},
		{"working", func(h *harness, pr store.PR) {
			ss, _ := h.st.SessionsByPR(h.ctx, pr.ID)
			now := h.clock.Now().Add(-3 * time.Hour)
			if err := h.st.UpdateSession(h.ctx, ss[0].ID, func(u *store.SessionUpdate) {
				u.Set("agent_status", string(herdr.StatusWorking))
				u.Set("agent_status_at", now)
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{"human", func(h *harness, pr store.PR) {
			if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("human_active_at", h.clock.Now().Add(-time.Minute)) }); err != nil {
				t.Fatal(err)
			}
		}},
		{"off", func(h *harness, _ store.PR) { h.cfg.Daemon.ParkIdleAfter.Duration = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			pr := h.reviewedPR(2, "b1")
			before := h.parks(pr.ID)
			h.advance(3 * time.Hour)
			tc.setup(h, pr)
			h.tick()
			if h.parks(pr.ID) != before {
				t.Fatalf("parked: %v", h.ag.all())
			}
		})
	}
}
