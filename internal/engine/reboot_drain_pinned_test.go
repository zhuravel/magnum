package engine

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// A session you opened (its PR pinned) and type into works with no round
// holding its PR: the park leaves it to you, and no round counts it, so the
// drain names it as work while herdr shows it working, and is not certified
// until it is idle again.
func TestASessionAtWorkWithNoRoundIsTheRebootDrainsWorkPinnedOrNot(t *testing.T) {
	h, _ := newLoginHarness(t)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 4, head: "d1"})
	h.startup()
	h.tick()
	pr := h.pr(4)
	if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("pinned", true) }); err != nil {
		t.Fatal(err)
	}
	s := h.liveSession(pr.ID, store.RoleJudge, agents.KindCodex, h.clock.Now(), herdr.StatusWorking)
	d := h.rebootDrain(95, DrainUntilBoot)
	h.tick()

	ack, _ := h.drainAck()
	if ack.Done(d.Generation, os.Getpid()) || !slices.ContainsFunc(ack.Work, func(w string) bool {
		return strings.Contains(w, "talkable/talkable#4 at work in herdr")
	}) {
		t.Fatalf("acknowledgement %+v, want the pinned PR's working session named, not certified", ack)
	}

	if err := h.st.UpdateSession(h.ctx, s.ID, func(u *store.SessionUpdate) { u.Set("agent_status", string(herdr.StatusIdle)) }); err != nil {
		t.Fatal(err)
	}
	h.advance(30 * time.Second)
	h.tick()
	if ack, _ := h.drainAck(); !ack.Done(d.Generation, os.Getpid()) {
		t.Fatalf("acknowledgement %+v once the pinned session is idle, want certified (its pane stays yours)", ack)
	}
}
