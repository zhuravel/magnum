package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// The settings screen's rows show the review daemon's pauses alone, so its
// r lifts them alone: never a drain for a reboot (yours, before a reboot)
// nor the factory's pause; and its p pauses the review daemon alone.
func TestTheSettingsScreenResumesAndPausesTheReviewDaemonAlone(t *testing.T) {
	h, _ := newControlHarness(t)
	drain := engine.RebootDrain{Generation: 9, Since: h.now, Until: engine.DrainUntilBoot, Boot: controlBoot}.Value()
	if err := h.st.SetKV(h.ctx, engine.KVDaemonRebootDrain, drain); err != nil {
		t.Fatal(err)
	}
	if err := factory.WriteRebootDrain(h.c.Layout, drain); err != nil {
		t.Fatal(err)
	}
	freeze := factory.Pause{Generation: 1, Since: h.now, Reason: "freeze"}.Value()
	if err := factory.WritePause(h.c.Layout, freeze); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetKV(h.ctx, store.KVDaemonPaused, "1"); err != nil {
		t.Fatal(err)
	}
	acts := newScreenActions(h.c)
	t.Cleanup(func() { _ = acts.Close() })
	rt := screenRuntime{acts}

	text, err := rt.Resume(h.ctx, tui.ResumeSpec{})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, ok := h.kv(store.KVDaemonPaused); ok {
		t.Error("the screen's resume left the review pause")
	}
	if v, _ := h.kv(engine.KVDaemonRebootDrain); v != drain {
		t.Errorf("the review daemon's drain for a reboot is %q after the screen's resume, want it kept", v)
	}
	if v, _ := factory.ReadRebootDrain(h.c.Layout); v != drain {
		t.Errorf("the factory's drain for a reboot is %q after the screen's resume, want it kept", v)
	}
	if v, _ := factory.ReadPause(h.c.Layout); v != freeze {
		t.Errorf("the factory's pause is %q after the screen's resume, want it kept", v)
	}
	actContains(t, text, "still held: "+keptDrainText)

	if err := factory.LiftPause(h.c.Layout, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Pause(h.ctx, tui.PauseSpec{For: time.Hour, Reason: "lunch"}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if v, _ := factory.ReadPause(h.c.Layout); v != "" {
		t.Errorf("the screen's pause wrote the factory's pause.json: %q", v)
	}
}

// `magnum resume --keep-drain` lifts the pauses alone, on both daemons: a
// script's own pause around a restart (magnum-improve's restart.sh) never
// lifts your drain for a reboot.
func TestResumeKeepDrainLiftsThePausesAlone(t *testing.T) {
	h, _ := newControlHarness(t)
	if code := h.cmd("drain", "--no-wait"); code != 0 {
		t.Fatalf("drain: exit %d: %s", code, h.errb.String())
	}
	if code := h.cmd("pause", "--for", "30m", "restart"); code != 0 {
		t.Fatalf("pause: exit %d: %s", code, h.errb.String())
	}
	h.out.Reset()
	if code := h.cmd("resume", "--keep-drain"); code != 0 {
		t.Fatalf("resume --keep-drain: exit %d: %s", code, h.errb.String())
	}
	if _, ok := h.reviewDrain(); !ok {
		t.Error("resume --keep-drain lifted the review daemon's drain for a reboot")
	}
	if _, ok := h.factoryDrain(); !ok {
		t.Error("resume --keep-drain lifted the factory's drain for a reboot")
	}
	if _, ok := h.kv(store.KVDaemonPaused); ok {
		t.Error("resume --keep-drain left the review pause")
	}
	if v, _ := factory.ReadPause(h.c.Layout); v != "" {
		t.Errorf("resume --keep-drain left the factory's pause %q", v)
	}
	actContains(t, h.out.String(), "factory: still held: "+keptDrainText, "review: still held: "+keptDrainText)
}

// magnum-improve's restart onto a build with a migration pauses and resumes
// the review daemon alone, and its resume keeps a drain for a reboot: it
// never changes or lifts the factory's pause, nor lifts your drain.
func TestTheImproveRestartScriptPausesAndResumesTheReviewDaemonAlone(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".claude", "skills", "magnum-improve", "scripts", "restart.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		cmd := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(cmd, "bin/magnum pause ") && !strings.HasPrefix(cmd, "bin/magnum pause --help"):
			if !strings.Contains(cmd, `"${scope[@]}"`) {
				t.Errorf("restart.sh pauses with no scope: %q", cmd)
			}
		case strings.HasPrefix(cmd, "bin/magnum resume"):
			if !strings.Contains(cmd, "--review --keep-drain") {
				t.Errorf("restart.sh resumes both daemons, or lifts the drain: %q", cmd)
			}
		}
	}
	actContains(t, string(b), "scope=(--review)")
}

// A pause of both daemons whose review side fails puts the factory's pause
// back as it was before (a freeze with no end), never leaves the one this
// command wrote.
func TestAPauseThatFailsOnTheReviewDaemonPutsTheFactorysPauseBack(t *testing.T) {
	h, _ := newControlHarness(t)
	freeze := factory.Pause{Generation: 1, Since: h.now.Add(-time.Hour), Reason: "freeze"}.Value()
	if err := factory.WritePause(h.c.Layout, freeze); err != nil {
		t.Fatal(err)
	}
	if err := h.st.Close(); err != nil { // the review side fails
		t.Fatal(err)
	}
	if code := pauseMain(h.ctx, h.c, h.d, pauseOpts{dur: 30 * time.Minute, reason: "lunch"}); code == 0 {
		t.Fatal("pause: exit 0 with the review side failing")
	}
	if v, _ := factory.ReadPause(h.c.Layout); v != freeze {
		t.Fatalf("the factory's pause is %q after the failure, want the freeze put back (%q)", v, freeze)
	}
	actContains(t, h.errb.String(), "factory: its pause is put back as it was")
}

// A factory control file that cannot be read is no dead end: `magnum
// resume --factory` says so and removes it, then goes on to the next one;
// the status line shows it with that remedy; doctor names it.
func TestAnUnreadableFactoryControlFileIsShownAndResumeRemovesIt(t *testing.T) {
	h, _ := newControlHarness(t)
	if err := os.MkdirAll(h.c.Layout.FactoryPause(), 0o700); err != nil { // EISDIR on its read
		t.Fatal(err)
	}
	if err := os.MkdirAll(h.c.Layout.FactoryRebootDrain(), 0o700); err != nil {
		t.Fatal(err)
	}
	dc := factoryControlOf(h.c.Layout, 0)
	got := dc.holdText(h.now, false)
	for _, want := range []string{"draining (unreadable record): magnum resume --factory ends it",
		"paused (unreadable pause.json): magnum resume --factory removes it"} {
		if !strings.Contains(got, want) {
			t.Errorf("the factory's status says %q, want %q", got, want)
		}
	}
	_, d, _, _ := doctorFixture(t)
	d.Layout = h.c.Layout
	for _, c := range doctorControls(context.Background(), d) {
		if strings.Contains(c.Fix, "magnum pause") {
			t.Errorf("doctor %+v names `magnum pause`, which cannot write a file it cannot read", c)
		}
	}

	if code := h.cmd("resume", "--factory"); code != 0 {
		t.Fatalf("resume --factory: exit %d: %s", code, h.errb.String())
	}
	for _, p := range []string{h.c.Layout.FactoryPause(), h.c.Layout.FactoryRebootDrain()} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s stays after resume --factory: %v", p, err)
		}
	}
	actContains(t, h.out.String(), "cannot read", "it is removed", "lifted the pause and the drain for a reboot")
}

// A review daemon in its start-up gap (its pidfile written, its record of
// the controls not yet) is starting, not an older build: `magnum drain`
// writes the drain for it and waits, and says it is starting. A daemon
// whose start recorded its pid without the controls is an older build, and
// is refused as before.
func TestADrainWaitsForAReviewDaemonThatIsStarting(t *testing.T) {
	h, f := newControlHarness(t)
	f.installed = false
	h.pid = 3 // running, its start has recorded neither its pid nor its controls
	if code := h.cmd("drain", "--no-wait"); code != 0 {
		t.Fatalf("drain with the review daemon starting: exit %d: %s", code, h.errb.String())
	}
	if _, ok := h.reviewDrain(); !ok {
		t.Fatal("no drain written for a review daemon that is starting")
	}

	if err := h.st.DeleteKV(h.ctx, engine.KVDaemonRebootDrain); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetKV(h.ctx, store.KVDaemonPid, "3"); err != nil { // an older build: its pid, no controls
		t.Fatal(err)
	}
	h.errb.Reset()
	if code := h.cmd("drain", "--no-wait"); code != 1 {
		t.Fatalf("drain with an older review daemon: exit %d, want 1", code)
	}
	actContains(t, h.errb.String(), "runs a build that does not know `magnum drain`")
}

// A drain whose command cannot read the Mac's boot never takes the boot of
// a drain that holds: that record may be an earlier boot's, which the next
// start would take for a reboot and end the new drain. The boot stays
// unknown, so the drain lasts until resume, and the warning says so.
func TestADrainThatCannotReadTheBootLeavesItUnknown(t *testing.T) {
	h, f := newControlHarness(t)
	earlier := engine.BootID{Session: "0B1C2D3E-4F50-4162-8374-95A6B7C8D9E0", Time: controlBoot.Time.Add(-24 * time.Hour)}
	held := engine.RebootDrain{Generation: 3, Since: h.now.Add(-24 * time.Hour), Until: engine.DrainUntilBoot, Boot: earlier}.Value()
	if err := factory.WriteRebootDrain(h.c.Layout, held); err != nil {
		t.Fatal(err)
	}
	f.bootErr = errors.New("sysctl: timed out")
	if code := h.cmd("drain", "--no-wait"); code != 0 {
		t.Fatalf("drain: exit %d: %s", code, h.errb.String())
	}
	d, ok := h.factoryDrain()
	if !ok || d.Boot.Known() {
		t.Fatalf("the drain written %+v, want its boot unknown, never the held record's", d)
	}
	actContains(t, h.errb.String(), "cannot read the Mac's boot", "lasts until `magnum resume`")
}
