package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory"
)

// The factory's status line agrees with its daemon, which holds a pause for
// a pause.json it cannot read or parse, at its start and during its run:
// "paused (unreadable record)" with the remedy that works, whether the
// daemon runs or not, for both kinds of file; a pause it can read says who
// paused it.
func TestTheFactorysStatusSaysPausedForAPauseFileItCannotUse(t *testing.T) {
	const want = "paused (unreadable record): magnum resume --factory removes it"
	for _, tc := range []struct {
		name    string
		setup   func(h *actHarness)
		running bool
	}{
		{name: "unreadable, not running", setup: func(h *actHarness) {
			if err := os.MkdirAll(h.c.Layout.FactoryPause(), 0o700); err != nil {
				h.t.Fatal(err)
			}
		}},
		{name: "unparseable, not running", setup: func(h *actHarness) {
			if err := factory.WritePause(h.c.Layout, "{not a pause"); err != nil {
				h.t.Fatal(err)
			}
		}},
		{name: "unreadable, running", running: true, setup: func(h *actHarness) {
			if err := os.MkdirAll(h.c.Layout.FactoryPause(), 0o700); err != nil {
				h.t.Fatal(err)
			}
		}},
		{name: "unparseable, running", running: true, setup: func(h *actHarness) {
			if err := factory.WritePause(h.c.Layout, "{not a pause"); err != nil {
				h.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, f := newControlHarness(t)
			pid := 0
			if tc.running {
				h.runDaemons(f)
				pid = f.pid
			}
			tc.setup(h)
			if got := factoryControlOf(h.c.Layout, pid).holdText(h.now, true); !strings.Contains(got, want) {
				t.Fatalf("the factory's status says %q, want %q", got, want)
			}
		})
	}
	h, _ := newControlHarness(t)
	if err := factory.WritePause(h.c.Layout, factory.Pause{Generation: 1, Since: h.now, Reason: "freeze"}.Value()); err != nil {
		t.Fatal(err)
	}
	if got := factoryControlOf(h.c.Layout, 0).holdText(h.now, true); !strings.Contains(got, "paused by you (freeze): magnum resume") {
		t.Fatalf("the factory's status says %q for a pause it can read", got)
	}
}

// A drain whose write fails on a daemon after it landed (a write that
// reports an error once it is done) undoes it there too: every daemon this
// command changed gets its drain back as it was, the review daemon's row
// included, which no earlier failure could reach (the factory comes first).
func TestADrainUndoesEveryDaemonItChangedTheFailedOneIncluded(t *testing.T) {
	h, _ := newControlHarness(t)
	before := engine.RebootDrain{Generation: 5, Since: h.now.Add(-time.Hour), Until: engine.DrainUntilResume}.Value()
	if err := factory.WriteRebootDrain(h.c.Layout, before); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetKV(h.ctx, engine.KVDaemonRebootDrain, before); err != nil {
		t.Fatal(err)
	}
	targets := drainTargets(h.ctx, h.c, h.d, controlScope{review: true, factory: true})
	review := targets[len(targets)-1]
	write := review.write
	review.write = func(ctx context.Context, v string) error {
		if err := write(ctx, v); err != nil {
			return err
		}
		return errors.New("disk I/O error once the row was written")
	}
	out := controlOut{c: h.c, cmd: "drain", both: true}
	value := engine.RebootDrain{Generation: 6, Since: h.now, Until: engine.DrainUntilBoot, Boot: controlBoot}.Value()
	if drainWrite(h.ctx, out, targets, value) {
		t.Fatal("drainWrite reports success after a failed write")
	}
	if v, _ := h.kv(engine.KVDaemonRebootDrain); v != before {
		t.Errorf("the review daemon's drain is %q after the failure, want the one it overwrote put back", v)
	}
	if v, _ := factory.ReadRebootDrain(h.c.Layout); v != before {
		t.Errorf("the factory's drain is %q after the failure, want the one it overwrote put back", v)
	}
	actContains(t, h.errb.String(), "review: cannot start the drain", "review: its drain is put back as it was",
		"factory: its drain is put back as it was")
}

// A drain's undo races a newer `magnum drain` on both daemons: whichever
// comes first, the newer drain stays on each, never the one the undo put
// back over it.
func TestADrainsUndoNeverOverwritesADrainWrittenMeanwhile(t *testing.T) {
	h, _ := newControlHarness(t)
	targets := drainTargets(h.ctx, h.c, h.d, controlScope{review: true, factory: true})
	out := controlOut{c: h.c, cmd: "drain", both: true}
	const before, value = "before", "mine"
	for i := range 200 {
		newer := fmt.Sprintf("newer-%d", i)
		for _, tg := range targets {
			if err := tg.write(h.ctx, value); err != nil {
				t.Fatal(err)
			}
			tg.prev = before
		}
		done := make(chan error, len(targets))
		for _, tg := range targets {
			go func() { done <- tg.write(h.ctx, newer) }()
		}
		drainUndo(h.ctx, out, targets, value)
		for range targets {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
		for _, tg := range targets {
			if v, err := tg.read(h.ctx); err != nil || v != newer {
				t.Fatalf("round %d: the %s daemon's drain is %q (%v) after an undo raced a newer drain, want the newer one", i, tg.name, v, err)
			}
		}
	}
}

// restartPauseLines are restart.sh's lines from its scope probe to its
// pause, as the script runs them.
func restartPauseLines(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", ".claude", "skills", "magnum-improve", "scripts", "restart.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	from := strings.Index(s, "scope=()")
	to := strings.Index(s, "start=$SECONDS")
	if from < 0 || to < from {
		t.Fatal("restart.sh has no scope probe before its pause")
	}
	return s[from:to]
}

// magnum-improve's migration pause runs under the Mac's /bin/bash (3.2,
// with the script's set -euo pipefail): a binary that does not know
// --review still pauses (the review daemon, the only one it knows), and one
// that does pauses with --review, its help printed in several writes as
// cobra prints it. An empty scope array must not abort the pause, and the
// probe must not depend on the help's pipe.
func TestTheImproveRestartPausesUnderTheMacsBash(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no /bin/bash")
	}
	lines := restartPauseLines(t)
	for _, tc := range []struct {
		name, help, want string
	}{
		{name: "a binary without --review", help: `echo "pause automation"; echo "usage: pause [--for d]"`,
			want: "pause --for 50m --reason restart onto a build with a migration"},
		{name: "a binary with --review", help: `echo "Hold new work; --review narrows it."; sleep 0.2; echo "usage: pause [--review]"`,
			want: "pause --review --for 50m --reason restart onto a build with a migration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o700); err != nil {
				t.Fatal(err)
			}
			fake := "#!/bin/bash\nif [ \"$2\" = --help ]; then " + tc.help + "; exit 0; fi\necho \"$*\" >> \"$(dirname \"$0\")/calls\"\n"
			if err := os.WriteFile(filepath.Join(dir, "bin", "magnum"), []byte(fake), 0o700); err != nil {
				t.Fatal(err)
			}
			script := "set -euo pipefail\nmax_secs=1200\n" + lines
			cmd := exec.Command("/bin/bash", "-c", script)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("the pause lines fail under /bin/bash: %v\n%s", err, out)
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "bin", "calls"))
			if got := strings.TrimSpace(string(calls)); got != tc.want {
				t.Fatalf("bin/magnum ran %q (output %q), want %q", got, out, tc.want)
			}
		})
	}
}

// A running factory daemon with no work.json of its own is a build from
// before the record (it never writes one), not a daemon that is starting:
// `magnum drain` refuses it as `magnum pause` and the status line do, and
// drains nothing. A work.json that names another pid is a daemon that is
// starting, and the drain waits for it.
func TestADrainRefusesAFactoryWithNoWorkRecordAsAnOlderBuild(t *testing.T) {
	h, f := newControlHarness(t)
	f.pid = 4300 // running, no work.json
	if code := h.cmd("drain", "--factory", "--no-wait"); code != 1 {
		t.Fatalf("drain with a factory that writes no work.json: exit %d, want 1", code)
	}
	actContains(t, h.errb.String(), "the factory daemon (pid 4300) runs a build that does not know `magnum drain`")
	if _, ok := h.factoryDrain(); ok {
		t.Fatal("a factory drain was written for an older build")
	}

	h.errb.Reset()
	h.factoryRecord(&fakeFactoryDaemon{pid: 4299}, nil) // the former daemon's record: this one is starting
	if code := h.cmd("drain", "--factory", "--no-wait"); code != 0 {
		t.Fatalf("drain with a factory that is starting: exit %d: %s", code, h.errb.String())
	}
}

// `magnum resume --factory` removes a pause.json the daemon cannot read
// even when the file's lock cannot be opened (another owner, a first write
// under sudo): the remove deletes whatever is there, and the daemon's read
// takes no lock, so the remedy never depends on the lock.
func TestResumeRemovesAnUnreadablePauseWhoseLockItCannotOpen(t *testing.T) {
	h, _ := newControlHarness(t)
	if err := os.MkdirAll(h.c.Layout.FactoryPause(), 0o700); err != nil { // EISDIR on its read
		t.Fatal(err)
	}
	lock := h.c.Layout.FactoryPause() + ".lock"
	if err := os.WriteFile(lock, nil, 0o000); err != nil { // EACCES on its open
		t.Fatal(err)
	}
	if code := h.cmd("resume", "--factory"); code != 0 {
		t.Fatalf("resume --factory with a lock it cannot open: exit %d: %s", code, h.errb.String())
	}
	if _, err := os.Stat(h.c.Layout.FactoryPause()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pause.json stays after resume --factory: %v", err)
	}
}

// A pause.json that `magnum resume --factory` cannot remove does not keep
// it from lifting the drain for a reboot: it reports both, and exits 1.
func TestResumeLiftsTheDrainWhenThePauseCannotBeRemoved(t *testing.T) {
	h, _ := newControlHarness(t)
	stuck := filepath.Join(h.c.Layout.FactoryPause(), "sub")
	if err := os.MkdirAll(stuck, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stuck, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stuck, 0o000); err != nil { // its contents cannot be listed, so the remove fails
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stuck, 0o700) })
	drain := engine.RebootDrain{Generation: 7, Since: h.now, Until: engine.DrainUntilBoot, Boot: controlBoot}.Value()
	if err := factory.WriteRebootDrain(h.c.Layout, drain); err != nil {
		t.Fatal(err)
	}
	if code := h.cmd("resume", "--factory"); code != 1 {
		t.Fatalf("resume --factory with a pause it cannot remove: exit %d, want 1", code)
	}
	if v, err := factory.ReadRebootDrain(h.c.Layout); err != nil || v != "" {
		t.Fatalf("the drain for a reboot is %q (%v) after the resume, want it lifted", v, err)
	}
	actContains(t, h.errb.String(), "cannot lift the pause")
	actContains(t, h.out.String(), "lifted the drain for a reboot")
}
