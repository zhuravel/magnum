package cli

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/launchd"
	"github.com/zhuravel/magnum/internal/paths"
)

// The daemon's own shutdown runs up to launchd's ExitTimeOut (its rounds,
// then the toasts' drain): a stop that waits less reports a daemon that is
// still shutting down as one that "did not exit".
func TestDaemonStopTimeoutOutwaitsTheJobsExitTimeOut(t *testing.T) {
	if daemonStopTimeout <= launchd.ExitTimeOut*time.Second {
		t.Fatalf("daemonStopTimeout = %v, want more than the plist's ExitTimeOut %ds", daemonStopTimeout, launchd.ExitTimeOut)
	}
}

// interruptingSleep is a Sleep fake that ends the command's context on the
// sleep numbered at (1-based), then answers as the real one does: ctx's
// error at once once it ended.
func interruptingSleep(cancel context.CancelFunc, at int, sleeps *int) func(context.Context, time.Duration) error {
	return func(ctx context.Context, _ time.Duration) error {
		*sleeps++
		if *sleeps == at {
			cancel()
		}
		return ctx.Err()
	}
}

// ctrl+c while a stopped daemon is waited for ends the wait at once and
// says so, instead of sleeping out the stop timeout.
func TestStopMagnumDaemonReturnsAtOnceOnInterrupt(t *testing.T) {
	comm, args := daemonRulePS("magnum", "magnum daemon")
	dt := newDaemonGroupTest(t, comm, args)
	daemonSys.DaemonPID = func(paths.Layout) (int, error) { return 250, nil } // never exits
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sleeps := 0
	daemonSys.Sleep = interruptingSleep(cancel, 1, &sleeps)
	err := stopMagnumDaemon(ctx, dt.fake, dt.ctx.Layout, 250)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("stop = %v, want an interrupt", err)
	}
	if sleeps != 1 {
		t.Fatalf("slept %d times after the interrupt, want 1", sleeps)
	}
}

// ctrl+c while launchd's new daemon is waited for is an interrupt, not "no
// new daemon was running".
func TestWaitLaunchdStartedReportsAnInterrupt(t *testing.T) {
	dt := newDaemonGroupTest(t, launchctlPrintSequence(launchctlPrint("running", 100)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sleeps := 0
	daemonSys.Sleep = interruptingSleep(cancel, 1, &sleeps)
	st, err := waitLaunchdStarted(ctx, dt.fake, 501, launchd.DefaultLabel, 100)
	if !errors.Is(err, context.Canceled) || errors.Is(err, errLaunchdNotStarted) {
		t.Fatalf("wait = %v, want the interrupt", err)
	}
	if sleeps != 1 || st.PID != 100 {
		t.Fatalf("slept %d times, last state %+v", sleeps, st)
	}
	// Without an interrupt the wait gives up after launchdStartWait.
	daemonSys.Sleep = func(context.Context, time.Duration) error { return nil }
	if _, err := waitLaunchdStarted(context.Background(), dt.fake, 501, launchd.DefaultLabel, 100); !errors.Is(err, errLaunchdNotStarted) {
		t.Fatalf("wait without a new pid = %v, want errLaunchdNotStarted", err)
	}
}

// ctrl+c during --drain's wait lifts the drain and restarts nothing, at the
// first sleep.
func TestDaemonRestartDrainStopsAtOnceOnInterrupt(t *testing.T) {
	dt := newDaemonGroupTest(t,
		launchctlPrintSequence(launchctlPrint("running", 100)),
		daemonRuleOK([]string{"launchctl", "kickstart"}, ""),
	)
	daemonSys.DaemonPID = func(paths.Layout) (int, error) { return 100, nil }
	seedRoundInFlight(t, dt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	daemonSys.Signals = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	sleeps := 0
	daemonSys.Sleep = interruptingSleep(cancel, 1, &sleeps)
	if code := dt.run("daemon-restart", "--drain"); code != 1 {
		t.Fatalf("exit %d, want 1; stdout: %s", code, dt.stdout)
	}
	if sleeps != 1 || !strings.Contains(dt.stderr.String(), "interrupted") {
		t.Fatalf("slept %d times; stderr: %s", sleeps, dt.stderr)
	}
	if _, ok := draining(t, dt); ok {
		t.Fatal("the drain was not lifted")
	}
	if n := len(dt.fake.CallsWithPrefix("launchctl", "kickstart")); n != 0 {
		t.Fatalf("kickstart calls = %d after the interrupt", n)
	}
}

// ctrl+c during --when-idle's wait stops the wait at the first sleep.
func TestDaemonRestartWhenIdleStopsAtOnceOnInterrupt(t *testing.T) {
	dt := newDaemonGroupTest(t,
		launchctlPrintSequence(launchctlPrint("running", 100)),
		daemonRuleOK([]string{"launchctl", "kickstart"}, ""),
	)
	daemonSys.DaemonPID = func(paths.Layout) (int, error) { return 100, nil }
	seedRoundInFlight(t, dt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	daemonSys.Signals = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	sleeps := 0
	daemonSys.Sleep = interruptingSleep(cancel, 1, &sleeps)
	if code := dt.run("daemon-restart", "--when-idle"); code != 1 {
		t.Fatalf("exit %d, want 1; stdout: %s", code, dt.stdout)
	}
	if sleeps != 1 || !strings.Contains(dt.stderr.String(), "interrupted") {
		t.Fatalf("slept %d times; stderr: %s", sleeps, dt.stderr)
	}
	if n := len(dt.fake.CallsWithPrefix("launchctl", "kickstart")); n != 0 {
		t.Fatalf("kickstart calls = %d after the interrupt", n)
	}
}

// Once the build and the rounds' guard passed, ctrl+c no longer stops the
// restart: a daemon stopped and never started again would stay down. An
// interrupt right after the SIGTERM to a daemon started by hand still sees
// launchd start the new one, and the command said it would.
func TestDaemonRestartFinishesAfterAnInterruptDuringTheStop(t *testing.T) {
	comm, args := daemonRulePS("/Users/b/Projects/magnum/bin/magnum", "/Users/b/Projects/magnum/bin/magnum daemon")
	dt := newDaemonGroupTest(t,
		launchctlPrintSequence(launchctlPrint("not running", 0), launchctlPrint("not running", 0), launchctlPrint("running", 300)),
		comm, args,
		daemonRuleOK([]string{"launchctl", "kickstart"}, ""),
	)
	alive := true
	daemonSys.DaemonPID = func(paths.Layout) (int, error) {
		if alive {
			return 250, nil
		}
		return 0, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	daemonSys.Signals = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	daemonSys.Kill = func(pid int, sig syscall.Signal) error {
		dt.killed = append(dt.killed, sig.String())
		cancel() // ctrl+c right after the SIGTERM
		return nil
	}
	sleeps := 0
	daemonSys.Sleep = func(sctx context.Context, _ time.Duration) error {
		sleeps++
		if err := sctx.Err(); err != nil {
			return err
		}
		alive = false // the stopped daemon exits during the first wait
		return nil
	}
	if code := dt.run("daemon-restart"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if ctx.Err() == nil {
		t.Fatal("the interrupt never came")
	}
	if n := len(dt.fake.CallsWithPrefix("launchctl", "kickstart")); n != 1 {
		t.Fatalf("kickstart calls = %d, want 1", n)
	}
	out := dt.stdout.String()
	if !strings.Contains(out, "restarted zhuravel.magnum") || !strings.Contains(out, "finishes even after ctrl+c") {
		t.Fatalf("output: %s", out)
	}
}
