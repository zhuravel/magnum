package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// fakeFactoryDaemon is the factory daemon and the Mac as the control
// commands see them (controlSys): whether its launchd job is installed, its
// pid (0: none runs), its kicks and the Mac's boot time.
type fakeFactoryDaemon struct {
	installed bool
	pid       int
	kicks     int
	boot      time.Time
	bootErr   error
}

// controlBoot is the tests' Mac's boot time.
var controlBoot = time.Date(2026, 10, 3, 7, 30, 0, 0, time.UTC)

// newControlHarness is an act harness whose registry is a file (the factory's
// status and screen read it read-only), with a fake factory daemon in
// controlSys: installed, not running.
func newControlHarness(t *testing.T) (*actHarness, *fakeFactoryDaemon) {
	t.Helper()
	storetest.Serial(t) // swaps controlSys
	h := newActHarnessOn(t, storetest.OpenFile)
	f := &fakeFactoryDaemon{installed: true, boot: controlBoot}
	old := controlSys
	t.Cleanup(func() { controlSys = old })
	controlSys = controlSeams{
		FactoryInstalled: func(paths.Layout) bool { return f.installed },
		FactoryPID:       func(context.Context, paths.Layout) (int, error) { return f.pid, nil },
		FactoryAlive:     func(paths.Layout) int { return f.pid },
		FactoryKick:      func(paths.Layout) (int, error) { f.kicks++; return f.pid, nil },
		BootTime:         func(context.Context) (time.Time, error) { return f.boot, f.bootErr },
	}
	return h, f
}

// runDaemons has both daemons run on this build: the review daemon as pid 3
// and the factory daemon as pid 4300, each with its record of the controls.
func (h *actHarness) runDaemons(f *fakeFactoryDaemon) {
	h.t.Helper()
	h.pid, f.pid = 3, 4300
	if err := h.st.SetKV(h.ctx, engine.KVDaemonControls, engine.Controls{Version: engine.ControlsVersion, PID: 3}.Value()); err != nil {
		h.t.Fatal(err)
	}
	h.factoryRecord(f, nil)
}

// factoryRecord writes the factory daemon's work.json as a daemon of this
// build does, with its acknowledgement ack.
func (h *actHarness) factoryRecord(f *fakeFactoryDaemon, ack *engine.DrainAck) {
	h.t.Helper()
	rec := factory.WorkRecord{PID: f.pid, Work: []factory.WorkItem{}, Controls: engine.ControlsVersion, Build: "v1.2.3", RebootDrain: ack}
	b, err := json.Marshal(rec)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(h.c.Layout.FactoryWork()), 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := fsx.WriteFileAtomic(h.c.Layout.FactoryWork(), b, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// reviewDrain is the review daemon's drain for a reboot (false: none).
func (h *actHarness) reviewDrain() (engine.RebootDrain, bool) {
	h.t.Helper()
	v, _ := h.kv(engine.KVDaemonRebootDrain)
	return engine.ParseRebootDrain(v)
}

// factoryDrain is the factory's drain for a reboot (false: none).
func (h *actHarness) factoryDrain() (engine.RebootDrain, bool) {
	h.t.Helper()
	v, err := factory.ReadRebootDrain(h.c.Layout)
	if err != nil {
		h.t.Fatal(err)
	}
	return engine.ParseRebootDrain(v)
}

// ackBoth plays both daemons: each acknowledges the drain of generation gen,
// done or still running work.
func (h *actHarness) ackBoth(f *fakeFactoryDaemon, gen int64, done bool, work string) {
	h.t.Helper()
	a := engine.DrainAck{Generation: gen, PID: 3, Quiet: done, Parked: done}
	if !done {
		a.Work = []string{work}
	}
	if err := h.st.SetKV(h.ctx, engine.KVDaemonDrainAck, a.Value()); err != nil {
		h.t.Fatal(err)
	}
	a.PID = f.pid
	h.factoryRecord(f, &a)
}

// `magnum pause` with no flag holds both daemons; with no daemon running
// both records are written, and `magnum resume` lifts both. --review leaves
// the factory alone.
func TestPauseWithNoFlagHoldsBothDaemonsAndReviewLeavesTheFactoryAlone(t *testing.T) {
	h, _ := newControlHarness(t)
	if code := h.cmd("pause", "--for", "2h", "lunch"); code != 0 {
		t.Fatalf("pause: exit %d: %s", code, h.errb.String())
	}
	if v, _ := h.kv(store.KVDaemonPaused); v != "1" {
		t.Fatalf("review paused = %q", v)
	}
	v, _ := factory.ReadPause(h.c.Layout)
	p, ok := factory.ParsePause(v)
	if !ok || p.Reason != "lunch" || !p.Until.Equal(h.now.Add(2*time.Hour)) {
		t.Fatalf("pause.json %q, want lunch until two hours from now", v)
	}
	actContains(t, h.out.String(), "review: paused automation until", "factory: paused by you until", "(lunch)",
		"factory: no factory daemon is running; the pause applies when it starts")

	h.out.Reset()
	if code := h.cmd("resume"); code != 0 {
		t.Fatalf("resume: exit %d: %s", code, h.errb.String())
	}
	if _, ok := h.kv(store.KVDaemonPaused); ok {
		t.Fatal("the review pause stays after resume")
	}
	if v, _ := factory.ReadPause(h.c.Layout); v != "" {
		t.Fatalf("pause.json stays after resume: %q", v)
	}
	actContains(t, h.out.String(), "review: resumed automation", "factory: lifted the pause")

	h.out.Reset()
	if code := h.cmd("pause", "--review"); code != 0 {
		t.Fatalf("pause --review: exit %d: %s", code, h.errb.String())
	}
	if v, _ := h.kv(store.KVDaemonPaused); v != "1" {
		t.Fatal("pause --review did not pause the review daemon")
	}
	if v, _ := factory.ReadPause(h.c.Layout); v != "" {
		t.Fatalf("pause --review paused the factory: %q", v)
	}
	if strings.Contains(h.out.String(), "review:") {
		t.Fatalf("one daemon's line names it: %q", h.out.String())
	}
}

// A factory job that is not installed is left out of a command with no flag.
func TestPauseLeavesOutAFactoryThatIsNotInstalled(t *testing.T) {
	h, f := newControlHarness(t)
	f.installed = false
	if code := h.cmd("pause"); code != 0 {
		t.Fatalf("pause: exit %d: %s", code, h.errb.String())
	}
	if v, _ := factory.ReadPause(h.c.Layout); v != "" {
		t.Fatalf("pause.json %q for a factory that is not installed", v)
	}
	if code := h.cmd("pause", "--factory"); code != 0 {
		t.Fatalf("pause --factory: exit %d: %s", code, h.errb.String())
	}
	if v, _ := factory.ReadPause(h.c.Layout); v == "" {
		t.Fatal("pause --factory wrote no pause.json")
	}
}

// A factory daemon of an older build does not know pause.json: the command
// says to restart it on this build first, and pauses neither daemon.
func TestPauseRefusesAFactoryDaemonOfAnOlderBuild(t *testing.T) {
	h, f := newControlHarness(t)
	f.pid = 4300 // its work.json names no controls
	if code := h.cmd("pause"); code != 1 {
		t.Fatalf("pause: exit %d, want 1", code)
	}
	actContains(t, h.errb.String(), "factory: the factory daemon (pid 4300) runs a build that does not know `magnum pause`: "+
		"restart it on this build first (`magnum factory install --drain`)", "review: left as it was")
	if _, ok := h.kv(store.KVDaemonPaused); ok {
		t.Fatal("the review daemon was paused")
	}
	if v, _ := factory.ReadPause(h.c.Layout); v != "" {
		t.Fatalf("pause.json %q", v)
	}
}

// `magnum drain` writes one record of one generation on both daemons, wakes
// them, and says "safe to reboot" only once both acknowledged it, quiet and
// parked; the record stays after the command (the first start after the
// reboot ends it), with the Mac's boot time.
func TestDrainWaitsForBothAcknowledgementsThenSaysSafeToReboot(t *testing.T) {
	h, f := newControlHarness(t)
	h.runDaemons(f)
	sleeps := 0
	h.onSleep = func(h *actHarness) {
		sleeps++
		d, ok := h.reviewDrain()
		if !ok {
			t.Error("no review drain while the command waits")
			return
		}
		switch sleeps {
		case 1:
			h.ackBoth(f, d.Generation, false, "round of talkable/talkable#12")
		case 4:
			h.ackBoth(f, d.Generation, true, "")
		}
	}
	if code := h.cmd("drain", "--reason", "reboot"); code != 0 {
		t.Fatalf("drain: exit %d: %s", code, h.errb.String())
	}
	out := h.out.String()
	actContains(t, out, "review: draining for a reboot", "factory: draining for a reboot",
		"review: draining: still runs: round of talkable/talkable#12", drainSafeText)
	if strings.Index(out, "still runs") > strings.Index(out, "safe to reboot") {
		t.Fatalf("it said safe before the work ended:\n%s", out)
	}
	rd, ok := h.reviewDrain()
	fd, fok := h.factoryDrain()
	if !ok || !fok || rd.Generation != fd.Generation || rd.Until != engine.DrainUntilBoot || !rd.Boot.Equal(controlBoot) || rd.Reason != "reboot" {
		t.Fatalf("records %+v / %+v, want one generation until boot on the Mac's boot", rd, fd)
	}
	if h.kicks == 0 || f.kicks == 0 {
		t.Fatalf("kicks: review %d, factory %d: both daemons must be woken", h.kicks, f.kicks)
	}
}

// --keep makes the drain last until resume, and says so.
func TestDrainKeepLastsUntilResume(t *testing.T) {
	h, _ := newControlHarness(t)
	if code := h.cmd("drain", "--keep"); code != 0 {
		t.Fatalf("drain --keep: exit %d: %s", code, h.errb.String())
	}
	if d, _ := h.reviewDrain(); d.Until != engine.DrainUntilResume {
		t.Fatalf("drain until %q, want resume", d.Until)
	}
	actContains(t, h.out.String(), drainSafeKeepText)
}

// The timeout and ctrl+c keep the drain, say what still runs and how to go
// on, and exit 1; status then shows the drain on both daemons' lines, and
// the screens say it too.
func TestDrainTimeoutAndCtrlCKeepTheDrainAndExit1(t *testing.T) {
	h, f := newControlHarness(t)
	h.runDaemons(f)
	h.onSleep = func(h *actHarness) {
		if d, ok := h.reviewDrain(); ok {
			h.ackBoth(f, d.Generation, false, "fx-writer turn of PR-12360")
		}
	}
	if code := h.cmd("drain", "--timeout", "30s"); code != 1 {
		t.Fatalf("drain --timeout 30s: exit %d, want 1", code)
	}
	actContains(t, h.errb.String(), "still not quiet after 30s; the drain holds", "fx-writer turn of PR-12360",
		"`magnum drain` waits for it again; `magnum resume` allows new work")
	if _, ok := h.reviewDrain(); !ok {
		t.Fatal("the timeout lifted the review drain")
	}

	h.errb.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	h.onSleep = func(*actHarness) { cancel() }
	if code := drainMain(ctx, h.c, h.d, drainOpts{timeout: time.Hour}); code != 1 {
		t.Fatalf("an interrupted drain: exit %d, want 1", code)
	}
	actContains(t, h.errb.String(), "interrupted after", "the drain holds", "`magnum resume` allows new work")
	if _, ok := h.reviewDrain(); !ok {
		t.Fatal("ctrl+c lifted the review drain")
	}
	if _, ok := h.factoryDrain(); !ok {
		t.Fatal("ctrl+c lifted the factory drain")
	}

	d := statusDeps{Store: h.st, Layout: h.c.Layout, Now: func() time.Time { return h.now }, Factory: statusFactoryControl(h.c.Layout),
		DaemonPID: func() (int, error) { return 3, nil }}
	r, err := statusGather(h.ctx, d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	at := inspClock(h.now, h.now)
	actContains(t, b.String(), "review   running · pid 3 · draining since "+at+": magnum resume ends it",
		"factory  running · pid 4300 · draining since "+at+": magnum resume ends it")
	facts := screenFacts(h.ctx, d)
	if len(facts.Controls) != 2 || !strings.Contains(facts.Controls[0].Full, "draining since") {
		t.Fatalf("the titles' facts %+v, want both daemons draining", facts.Controls)
	}
	short, _ := factoryScreenControlLines(h.ctx, h.c, f.pid, h.now)
	if len(short) != 2 || short[0] != "review draining since "+at+": magnum resume ends it" ||
		short[1] != "factory draining since "+at+": magnum resume ends it" {
		t.Fatalf("the factory screen's blocker lines %q", short)
	}
}

// A drain that waits sees `magnum resume`: it says the drain was lifted and
// exits 0. A newer `magnum drain` takes the drain over: the old one stops,
// certifying nothing.
func TestAWaitingDrainExitsZeroAfterAResumeAndStopsForANewerDrain(t *testing.T) {
	h, f := newControlHarness(t)
	h.runDaemons(f)
	sleeps := 0
	h.onSleep = func(h *actHarness) {
		if sleeps++; sleeps == 2 {
			_ = h.st.DeleteKV(h.ctx, engine.KVDaemonRebootDrain)
			_ = factory.LiftRebootDrain(h.c.Layout, "")
		}
	}
	if code := h.cmd("drain"); code != 0 {
		t.Fatalf("drain then resume: exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "the drain was lifted (`magnum resume`)")

	sleeps = 0
	h.onSleep = func(h *actHarness) {
		if sleeps++; sleeps == 2 {
			newer := engine.RebootDrain{Generation: 99, Since: h.now, Until: engine.DrainUntilBoot}
			_ = h.st.SetKV(h.ctx, engine.KVDaemonRebootDrain, newer.Value())
		}
	}
	if code := h.cmd("drain"); code != 1 {
		t.Fatalf("drain replaced by a newer one: exit %d, want 1", code)
	}
	actContains(t, h.errb.String(), "a newer `magnum drain` took the drain over")
	if d, _ := h.reviewDrain(); d.Generation != 99 {
		t.Fatalf("the old command changed the newer drain: %+v", d)
	}
}

// A review daemon of an older build does not know the drain: the command
// says to restart it on this build first and drains neither daemon.
func TestDrainRefusesADaemonOfAnOlderBuild(t *testing.T) {
	h, f := newControlHarness(t)
	h.runDaemons(f)
	if err := h.st.DeleteKV(h.ctx, engine.KVDaemonControls); err != nil {
		t.Fatal(err)
	}
	if code := h.cmd("drain"); code != 1 {
		t.Fatalf("drain: exit %d, want 1", code)
	}
	actContains(t, h.errb.String(), "review: the review daemon (pid 3) runs a build that does not know `magnum drain`: "+
		"restart it on this build first (`magnum daemon-restart --drain`)", "factory: left as it was: nothing is drained")
	if _, ok := h.reviewDrain(); ok {
		t.Fatal("a review drain was written")
	}
	if _, ok := h.factoryDrain(); ok {
		t.Fatal("a factory drain was written")
	}
}

// A drain that fails on one daemon reports each and lifts only what it set:
// the factory's drain it wrote where none was; one that held before stays.
func TestADrainThatFailsOnOneDaemonLiftsOnlyWhatItSet(t *testing.T) {
	h, _ := newControlHarness(t)
	if err := h.st.Close(); err != nil { // the review registry fails
		t.Fatal(err)
	}
	if code := drainMain(h.ctx, h.c, h.d, drainOpts{timeout: time.Hour}); code != 1 {
		t.Fatalf("drain: exit %d, want 1", code)
	}
	actContains(t, h.errb.String(), "review: cannot start the drain", "factory: its drain is lifted again")
	if _, ok := h.factoryDrain(); ok {
		t.Fatal("the factory keeps the drain this command set")
	}

	before := engine.RebootDrain{Generation: 5, Since: h.now.Add(-time.Hour), Until: engine.DrainUntilResume}.Value()
	if err := factory.WriteRebootDrain(h.c.Layout, before); err != nil {
		t.Fatal(err)
	}
	h.errb.Reset()
	if code := drainMain(h.ctx, h.c, h.d, drainOpts{timeout: time.Hour}); code != 1 {
		t.Fatalf("drain: exit %d, want 1", code)
	}
	actContains(t, h.errb.String(), "factory: keeps the drain it held before this command")
}

// `magnum resume` lifts the pause and the drain on both daemons and keeps an
// agent kind's pause, a watch's pause, a mute and a pin, naming the
// pauses it left with their remedies.
func TestResumeLiftsThePauseAndTheDrainAndKeepsTheOtherHolds(t *testing.T) {
	h, _ := newControlHarness(t)
	if code := h.cmd("pause"); code != 0 {
		t.Fatalf("pause: exit %d", code)
	}
	if code := h.cmd("drain", "--no-wait"); code != 0 {
		t.Fatalf("drain --no-wait: exit %d: %s", code, h.errb.String())
	}
	pr := h.seedPR("talkable/talkable", 12, store.PRReviewed)
	h.setPR(pr.ID, store.PRReviewed, func(u *store.PRUpdate) { u.Set("muted", true); u.Set("pinned", true) })
	for k, v := range map[string]string{
		store.KVToolPausedUntil("codex"):  store.FormatTime(h.now.Add(time.Hour)),
		store.KVToolPausedReason("codex"): "usage_limit",
		store.KVWatchPaused("talkable"):   "review 77 posted by zhuravel",
	} {
		if err := h.st.SetKV(h.ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	h.out.Reset()
	if code := h.cmd("resume"); code != 0 {
		t.Fatalf("resume: exit %d: %s", code, h.errb.String())
	}
	if _, ok := h.reviewDrain(); ok {
		t.Fatal("the review drain stays")
	}
	if _, ok := h.factoryDrain(); ok {
		t.Fatal("the factory drain stays")
	}
	for _, k := range []string{store.KVToolPausedUntil("codex"), store.KVWatchPaused("talkable")} {
		if _, ok := h.kv(k); !ok {
			t.Fatalf("resume lifted %s", k)
		}
	}
	if got, _ := h.st.PRByID(h.ctx, pr.ID); !got.Muted || !got.Pinned {
		t.Fatalf("resume touched the mute or the pin: muted %v pinned %v", got.Muted, got.Pinned)
	}
	actContains(t, h.out.String(), "review: lifted the drain for a reboot", "factory: lifted the pause and the drain for a reboot",
		"review: still held: watch talkable is paused (identity leak): `magnum resume --watch talkable`",
		"review: still held: codex is paused (usage_limit) until "+inspClock(h.now, h.now.Add(time.Hour))+": `magnum resume --tool codex`")
}

// `magnum status` shows one line per daemon, and its JSON lists them.
func TestStatusShowsOneLinePerDaemonInTextAndJSON(t *testing.T) {
	h, f := newControlHarness(t)
	h.runDaemons(f)
	if code := h.cmd("pause", "--factory", "--for", "3h30m", "lunch"); code != 0 {
		t.Fatalf("pause --factory: exit %d: %s", code, h.errb.String())
	}
	d := statusDeps{Store: h.st, Layout: h.c.Layout, Now: func() time.Time { return h.now }, Factory: statusFactoryControl(h.c.Layout),
		DaemonPID: func() (int, error) { return 3, nil }}
	r, err := statusGather(h.ctx, d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	actContains(t, b.String(), "review   running · pid 3\n",
		"factory  running · pid 4300 · paused by you until "+inspClock(h.now, h.now.Add(210*time.Minute))+" (lunch): magnum resume")
	b.Reset()
	if err := writeJSON(&b, r); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Daemons []daemonControl `json:"daemons"`
	}
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Daemons) != 2 || got.Daemons[0].Name != "review" || got.Daemons[1].Name != "factory" || got.Daemons[1].Build != "v1.2.3" ||
		got.Daemons[1].Pause == nil || got.Daemons[1].Pause.Reason != "lunch" || !got.Daemons[1].Running {
		t.Fatalf("daemons %+v", got.Daemons)
	}

	h.out.Reset()
	if code := h.cmd("factory", "status", "--json"); code != 0 {
		t.Fatalf("factory status --json: exit %d: %s", code, h.errb.String())
	}
	if !strings.Contains(h.out.String(), `"daemons": [`) || !strings.Contains(h.out.String(), `"reason": "lunch"`) {
		t.Fatalf("factory status --json lacks the daemons:\n%s", h.out.String())
	}
}
