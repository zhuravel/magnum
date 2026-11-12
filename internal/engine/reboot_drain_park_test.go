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

// A session that becomes restartable during the drain (`magnum unpin`, an
// open's hold that ends) is parked too: every quiet check marks what may
// park now, so the acknowledgement reaches parked. The first quiet check
// had marked the other PR's sessions already.
func TestARebootDrainParksASessionUnpinnedDuringIt(t *testing.T) {
	h, _ := newLoginHarness(t)
	pr := h.reviewedPR(2, "b1")
	h.withSessionIDs(pr.ID)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"}, prSpec{n: 4, head: "d1"})
	h.tick()
	pinnedPR := h.pr(4)
	if err := h.st.UpdatePR(h.ctx, pinnedPR.ID, func(u *store.PRUpdate) { u.Set("pinned", true) }); err != nil {
		t.Fatal(err)
	}
	pinned := h.liveSession(pinnedPR.ID, store.RoleJudge, agents.KindCodex, h.clock.Now(), herdr.StatusIdle)
	d := h.rebootDrain(40, DrainUntilBoot)
	h.tick()
	h.advance(30 * time.Second)
	h.tick()
	if got := h.liveRoles(pr.ID); len(got) != 0 {
		t.Fatalf("live sessions %q of the unpinned PR, want them parked", got)
	}

	if err := h.st.UpdatePR(h.ctx, pinnedPR.ID, func(u *store.PRUpdate) { u.Set("pinned", false) }); err != nil { // magnum unpin
		t.Fatal(err)
	}
	for range 3 { // marked, its park queued and run, then parked
		h.advance(30 * time.Second)
		h.tick()
	}
	if st := h.sessionState(pinned.ID); st != store.SessionParked {
		t.Fatalf("the session unpinned during the drain is %s, want parked", st)
	}
	if ack, _ := h.drainAck(); !ack.Done(d.Generation, os.Getpid()) {
		t.Fatalf("acknowledgement %+v, want quiet and parked", ack)
	}
}

// The acknowledgement is the row the CLI reads, not a copy in memory: a row
// that went (a failed write, a hand's delete) is written again at the next
// tick.
func TestTheRebootDrainsAcknowledgementIsWrittenAgainWhenItsRowIsGone(t *testing.T) {
	h := newHarness(t)
	h.startup()
	d := h.rebootDrain(41, DrainUntilBoot)
	h.tick()
	if _, ok := h.drainAck(); !ok {
		t.Fatal("no acknowledgement")
	}
	if err := h.st.DeleteKV(h.ctx, KVDaemonDrainAck); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if ack, ok := h.drainAck(); !ok || ack.Generation != d.Generation {
		t.Fatalf("acknowledgement %+v (%v) after its row went, want it written again", ack, ok)
	}
}

// A session that shows a dialog (blocked: a "Hooks need review", a
// permission prompt) cannot park: the acknowledgement names it with what
// to do, and is not parked until it is answered.
func TestABlockedSessionIsNamedInTheRebootDrainsWork(t *testing.T) {
	h, _ := newLoginHarness(t)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 3, head: "c1"})
	h.startup()
	h.tick()
	pr := h.pr(3)
	blocked := h.liveSession(pr.ID, store.RoleJudge, agents.KindCodex, h.clock.Now(), herdr.StatusBlocked)
	h.rebootDrain(42, DrainUntilBoot)
	h.tick()

	ack, _ := h.drainAck()
	want := "codex-judge of talkable/talkable#3 shows a dialog: answer it in its pane (`magnum open talkable/talkable#3`)"
	if ack.Parked || !slices.ContainsFunc(ack.Work, func(w string) bool { return strings.Contains(w, "shows a dialog") }) {
		t.Fatalf("acknowledgement %+v, want not parked, naming the blocked session (%q)", ack, want)
	}
	if err := h.st.UpdateSession(h.ctx, blocked.ID, func(u *store.SessionUpdate) { u.Set("agent_status", string(herdr.StatusIdle)) }); err != nil {
		t.Fatal(err)
	}
	h.advance(30 * time.Second)
	h.tick()
	h.advance(30 * time.Second)
	h.tick()
	if ack, _ := h.drainAck(); !ack.Parked || len(ack.Work) != 0 {
		t.Fatalf("acknowledgement %+v once the dialog was answered, want parked", ack)
	}
}
