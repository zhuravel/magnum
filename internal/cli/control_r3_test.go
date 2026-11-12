package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/fsx"
)

// A daemon that started writes its pidfile before its record of the
// controls: a poll in that gap must not mark its pid as an older build for
// good. The drain checks again while it says so, and goes on once the
// record is there.
func TestADrainChecksAgainADaemonThatHasNotRecordedItsControlsYet(t *testing.T) {
	h, f := newControlHarness(t)
	h.runDaemons(f)
	sleeps := 0
	h.onSleep = func(h *actHarness) {
		sleeps++
		switch sleeps {
		case 1:
			h.pid = 9 // the daemon restarted: its pidfile is written, its controls not yet
		case 3:
			if err := h.st.SetKV(h.ctx, engine.KVDaemonControls, engine.Controls{Version: engine.ControlsVersion, PID: 9}.Value()); err != nil {
				t.Error(err)
			}
			if d, ok := h.reviewDrain(); ok {
				h.ackBoth(f, d.Generation, true, "")
				a := engine.DrainAck{Generation: d.Generation, PID: 9, Quiet: true, Parked: true}
				_ = h.st.SetKV(h.ctx, engine.KVDaemonDrainAck, a.Value())
			}
		}
	}
	if code := h.cmd("drain", "--timeout", "60s"); code != 0 {
		t.Fatalf("drain: exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), drainSafeText)
}

// A factory daemon that stopped with work in its record (a crash: a stop
// removes the record) may have left that work's agents in herdr, a local
// review round's in a scratch registry the drain cannot read: the drain is
// not certified while that record says so. A factory directory that cannot
// be read is never none either.
func TestADrainNeverCertifiesAFactoryThatStoppedWithWorkOrCannotBeRead(t *testing.T) {
	h, _ := newControlHarness(t)
	rec := factory.WorkRecord{PID: 4300, Controls: engine.ControlsVersion, Work: []factory.WorkItem{{Kind: factory.WorkRound, Task: 7,
		What: "local review round 1 of PR-12345", Since: h.now}}}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(h.c.Layout.FactoryWork()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsx.WriteFileAtomic(h.c.Layout.FactoryWork(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := h.cmd("drain", "--timeout", "10s"); code != 1 {
		t.Fatalf("drain: exit %d, want 1", code)
	}
	actContains(t, h.errb.String(), "local review round 1 of PR-12345")

	if err := os.Remove(h.c.Layout.FactoryWork()); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(h.c.Layout.FactoryAgentsDB())
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := factoryTurnsDown(h.ctx, h.c.Layout); err == nil {
		t.Fatal("a factory directory that cannot be read reads as no agent at work")
	}
}

// A daemon of an older build ignores the pause and the drain it does not
// know: status says so, with how to go on, rather than only "paused by you".
func TestStatusNamesAnOlderFactoryBuildThatIgnoresItsPause(t *testing.T) {
	h, f := newControlHarness(t)
	f.pid = 4300
	rec := factory.WorkRecord{PID: 4300, Work: []factory.WorkItem{}} // an older build: no controls
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(h.c.Layout.FactoryWork()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsx.WriteFileAtomic(h.c.Layout.FactoryWork(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := factory.WritePause(h.c.Layout, factory.Pause{Generation: 1, Since: h.now, Reason: "lunch"}.Value()); err != nil {
		t.Fatal(err)
	}
	line := factoryControlOf(h.c.Layout, f.pid).line(h.now)
	if !strings.Contains(line, "paused by you") || !strings.Contains(line, "restart it on this build first") {
		t.Fatalf("status line %q, want the older build named with its remedy", line)
	}
}
