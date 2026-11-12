package engine

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// The boot times of the tests' Mac: the one a drain began on, and another
// after a reboot.
var (
	bootOne = time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	bootTwo = time.Date(2026, 11, 12, 18, 30, 0, 0, time.UTC)
)

// rebootDrain writes a drain for a reboot of generation gen, begun now on
// bootOne and lasting until until, as `magnum drain` does.
func (h *harness) rebootDrain(gen int64, until string) RebootDrain {
	h.t.Helper()
	d := RebootDrain{Generation: gen, Since: h.clock.Now(), Until: until, Boot: bootOne, Reason: "reboot"}
	if err := h.st.SetKV(h.ctx, KVDaemonRebootDrain, d.Value()); err != nil {
		h.t.Fatal(err)
	}
	return d
}

// drainAck is the daemon's acknowledgement as `magnum drain` reads it;
// false when there is none.
func (h *harness) drainAck() (DrainAck, bool) {
	h.t.Helper()
	v, ok, err := h.st.GetKV(h.ctx, KVDaemonDrainAck)
	if err != nil {
		h.t.Fatal(err)
	}
	if !ok {
		return DrainAck{}, false
	}
	a, ok := ParseDrainAck(v)
	if !ok {
		h.t.Fatalf("an acknowledgement %q that does not parse", v)
	}
	return a, true
}

// onBoot has the harness's daemon read boot as the Mac's boot time.
func onBoot(boot *time.Time) func(*harness) {
	return func(h *harness) {
		h.d.BootTime = func(context.Context) (time.Time, error) { return *boot, nil }
	}
}

// A round about to start as the drain begins does not start once the
// daemon acknowledged it: the tick's dispatch reads the record the health
// checks acknowledged, the acknowledgement says quiet with nothing to park,
// and no later tick starts it either.
func TestARoundAboutToStartAsTheRebootDrainBeginsNeverStartsAfterItsAcknowledgement(t *testing.T) {
	h := newHarness(t)
	h.queuedPR(2, "b1")
	h.advance(5 * time.Minute) // the round is due at this tick's dispatch
	d := h.rebootDrain(7, DrainUntilBoot)

	h.tick()

	h.wantState(2, store.PRQueued)
	ack, ok := h.drainAck()
	if !ok || !ack.Done(d.Generation, os.Getpid()) || len(ack.Work) != 0 {
		t.Fatalf("acknowledgement %+v (%v), want generation 7 of this daemon, quiet and parked", ack, ok)
	}
	if n := len(h.rd.all()); n != 0 {
		t.Fatalf("%d rounds ran under the drain", n)
	}
	if evs := h.events("daemon.reboot_drain_held"); len(evs) != 1 || !strings.Contains(evs[0].Message, "nothing new starts") {
		t.Fatalf("held events %+v", evs)
	}
	if bar := h.e.tabBar(h.ctx); !strings.Contains(bar, "draining") {
		t.Fatalf("tab bar %q, want it draining", bar)
	}
	h.advance(10 * time.Minute)
	h.tick()
	h.wantState(2, store.PRQueued)
}

// The drain waits for a staging resolver's job that took its slot and for a
// merge of the parent (a fix job in phase fixing, which reserves its PR): the
// acknowledgement names both and is not quiet until they end.
func TestTheRebootDrainWaitsForAStagingResolveAndAMergeOfTheParent(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.d.stagingResolver = &stagingResolver{} })
	merging := h.queuedPR(3, "c1")
	h.rebootDrain(8, DrainUntilBoot)
	job := resolveJob{ID: "dep-1.r1.0a0b0c0d", Effect: "dep-1", Repo: "talkable/talkable", Number: 12, Target: "staging-void",
		State: resolveRunning, Started: h.clock.Now()}
	if err := h.e.saveResolveJob(h.ctx, job); err != nil {
		t.Fatal(err)
	}
	if !h.e.reserve(merging.ID, &roundHandle{cancel: func() {}, fix: true}) {
		t.Fatal("the merge could not reserve its PR")
	}

	h.tick()

	ack, _ := h.drainAck()
	want := []string{"fix of talkable/talkable#3", "staging resolve of talkable/talkable#12 into staging-void"}
	if ack.Quiet || !slices.Equal(ack.Work, want) {
		t.Fatalf("acknowledgement %+v, want not quiet, naming %q", ack, want)
	}

	h.e.unreserve(merging.ID)
	job.State = resolveDone
	if err := h.e.saveResolveJob(h.ctx, job); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if ack, _ := h.drainAck(); !ack.Quiet || len(ack.Work) != 0 {
		t.Fatalf("acknowledgement %+v after both ended, want quiet", ack)
	}
}

// A queued staging resolve takes no slot while the drain holds: it waits,
// saying so (a job that took its slot before runs to its end).
func TestTheRebootDrainHoldsAQueuedStagingResolve(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.d.stagingResolver = &stagingResolver{} })
	h.open(prSpec{n: 5, head: "e1", author: "zhuravel"})
	h.startup()
	h.tick()
	h.rebootDrain(9, DrainUntilBoot)
	job := resolveJob{ID: "dep-2.r1.0a0b0c0e", Effect: "dep-2", Repo: "talkable/talkable", Number: 5, Target: "staging-void",
		State: resolveQueued, Started: h.clock.Now()}
	if err := h.e.saveResolveJob(h.ctx, job); err != nil {
		t.Fatal(err)
	}
	w := &resolveWork{e: h.e, r: h.d.stagingResolver, job: &job}
	if err := w.take(h.ctx); !errors.Is(err, errResolveWaits) || !strings.Contains(job.Why, "drains for a reboot") {
		t.Fatalf("take = %v (%q), want it to wait for the drain", err, job.Why)
	}
	if job.State != resolveQueued || job.SlotID != 0 {
		t.Fatalf("the job is %s in slot %d, want queued with no slot", job.State, job.SlotID)
	}
}

// Once quiet, the drain parks the idle sessions (a pinned PR's stay with
// the operator), and the acknowledgement says parked only when none is
// live; `magnum resume` (the record gone) unmarks what is left and the
// acknowledgement goes.
func TestAQuietRebootDrainParksTheIdleSessionsThenSaysParked(t *testing.T) {
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
	if len(h.liveRoles(pr.ID)) == 0 {
		t.Fatal("the reviewed PR has no live session to park")
	}
	d := h.rebootDrain(10, DrainUntilBoot)

	h.tick() // quiet: the sessions are marked and their park queued
	h.advance(30 * time.Second)
	h.tick()

	if got := h.liveRoles(pr.ID); len(got) != 0 {
		t.Fatalf("live sessions %q of the reviewed PR after a quiet drain, want parked", got)
	}
	if st := h.sessionState(pinned.ID); st != store.SessionLive {
		t.Fatalf("the pinned PR's session is %s, want live", st)
	}
	if ack, _ := h.drainAck(); !ack.Done(d.Generation, os.Getpid()) {
		t.Fatalf("acknowledgement %+v, want quiet and parked", ack)
	}
	if evs := h.events("daemon.reboot_drain_quiet"); len(evs) != 1 {
		t.Fatalf("quiet events %+v, want one", evs)
	}

	if err := h.st.DeleteKV(h.ctx, KVDaemonRebootDrain); err != nil { // magnum resume
		t.Fatal(err)
	}
	h.tick()
	if _, ok := h.drainAck(); ok {
		t.Fatal("the acknowledgement stays after the drain was lifted")
	}
	for _, m := range h.e.restartMarks(h.ctx) {
		if m.Why == drainParkWhy {
			t.Fatalf("a mark of the drain's park stays after the lift: %+v", m)
		}
	}
	if evs := h.events("daemon.reboot_drain_lifted"); len(evs) != 1 {
		t.Fatalf("lifted events %+v, want one", evs)
	}
}

// The drain outlives its command and the daemon's restarts on the same
// boot (a crash, a new build, a restart by command): each start drops the
// former daemon's acknowledgement and keeps the record. The first start on
// another boot ends it; a drain until "resume" (`magnum drain --keep`)
// stays even then.
func TestARebootDrainEndsAtTheFirstStartOnANewBootAndOnlyThen(t *testing.T) {
	boot := bootOne
	h := newHarness(t, onBoot(&boot))
	h.startup()
	h.rebootDrain(11, DrainUntilBoot)
	h.tick()
	if _, ok := h.drainAck(); !ok {
		t.Fatal("no acknowledgement")
	}

	h.e = New(h.d) // a crash restart on the same boot
	h.startup()
	if !h.e.rebootDraining(h.ctx) {
		t.Fatal("a restart on the same boot ended the drain")
	}
	if _, ok := h.drainAck(); ok {
		t.Fatal("the former daemon's acknowledgement stays after the restart")
	}
	h.tick()
	if ack, ok := h.drainAck(); !ok || ack.Generation != 11 {
		t.Fatalf("acknowledgement %+v (%v) after the restart, want the new daemon's of generation 11", ack, ok)
	}

	boot = bootTwo // the Mac rebooted
	h.e = New(h.d)
	h.startup()
	if h.e.rebootDraining(h.ctx) {
		t.Fatal("the first start after the reboot kept the drain")
	}
	if evs := h.events("daemon.reboot_drain_ended"); len(evs) != 1 || !strings.Contains(evs[0].Message, "rebooted") {
		t.Fatalf("ended events %+v", evs)
	}

	h.rebootDrain(12, DrainUntilResume) // magnum drain --keep, on bootOne
	h.e = New(h.d)
	h.startup()
	if !h.e.rebootDraining(h.ctx) {
		t.Fatal("a drain until resume ended at a start on another boot")
	}
}

// A boot time the daemon cannot read keeps the drain: only `magnum resume`
// ends it then.
func TestARebootDrainStaysWhenTheBootTimeIsUnknown(t *testing.T) {
	fake := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{sysctlPath, "-n", bootOID}, Result: execx.Result{Code: 1}}}}
	h := newHarness(t, func(h *harness) { h.d.MemoryRunner = fake })
	h.rebootDrain(13, DrainUntilBoot)
	h.startup()
	if !h.e.rebootDraining(h.ctx) {
		t.Fatal("a start that could not read the boot time ended the drain")
	}
}

// A restart's drain and a drain for a reboot are two records: the start
// ends the restart's alone, a restart drain whose command is gone is lifted
// alone, and the lift of the reboot's leaves the restart's.
func TestARestartDrainAndARebootDrainDoNotLiftEachOther(t *testing.T) {
	boot := bootOne
	h := newHarness(t, onBoot(&boot))
	h.rebootDrain(14, DrainUntilBoot)
	if err := h.st.SetKV(h.ctx, KVDaemonDraining, Drain{Since: h.clock.Now(), PID: os.Getpid()}.Value()); err != nil {
		t.Fatal(err)
	}
	h.startup()
	if _, ok, _ := h.st.GetKV(h.ctx, KVDaemonDraining); ok {
		t.Fatal("the start kept the restart's drain")
	}
	if !h.e.rebootDraining(h.ctx) {
		t.Fatal("the start ended the drain for a reboot with the restart's")
	}

	restart := Drain{Since: h.clock.Now()}.Value() // an older CLI's: no pid, so only a restart ends it
	if err := h.st.SetKV(h.ctx, KVDaemonDraining, restart); err != nil {
		t.Fatal(err)
	}
	if err := h.st.DeleteKV(h.ctx, KVDaemonRebootDrain); err != nil { // magnum resume
		t.Fatal(err)
	}
	h.tick()
	if v, _, _ := h.st.GetKV(h.ctx, KVDaemonDraining); v != restart {
		t.Fatalf("the restart's drain is %q after the reboot's was lifted, want it kept", v)
	}
	if !h.e.draining(h.ctx) {
		t.Fatal("the daemon stopped draining with the restart's drain in force")
	}
}

// An hour quiet under the drain with no reboot, the daemon says once that
// the drain still holds.
func TestAQuietRebootDrainToastsOnceAfterAnHour(t *testing.T) {
	h := newHarness(t)
	h.startup()
	h.rebootDrain(15, DrainUntilBoot)
	h.tick()
	h.advance(59 * time.Minute)
	h.tick()
	if got := h.nh.titles("drain for a reboot"); len(got) != 0 {
		t.Fatalf("toasts %q before an hour", got)
	}
	h.advance(2 * time.Minute)
	h.tick()
	h.advance(time.Minute)
	h.tick()
	if got := h.nh.titles("drain for a reboot"); len(got) != 1 {
		t.Fatalf("toasts %q after an hour quiet, want one", got)
	}
}

// kern.boottime reads as its seconds and microseconds.
func TestParseBootTimeReadsSysctlsOutput(t *testing.T) {
	got, err := ParseBootTime("{ sec = 1762940000, usec = 123456 } Wed Nov 12 10:13:20 2026\n")
	if err != nil || !got.Equal(time.Unix(1762940000, 123456000)) {
		t.Fatalf("ParseBootTime = %v, %v", got, err)
	}
	if _, err := ParseBootTime("kern.boottime: unknown oid"); err == nil {
		t.Fatal("an answer with no boot time parsed")
	}
}
