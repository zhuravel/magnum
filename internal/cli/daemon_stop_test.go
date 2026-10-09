package cli

import (
	"strings"
	"syscall"
	"testing"

	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// seedRoundInFlight records talkable/talkable#42 as reviewing.
func seedRoundInFlight(t *testing.T, dt *daemonGroupTest) {
	t.Helper()
	if err := dt.ctx.Layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	storetest.Seed(t, dt.ctx.Layout.DB())
	st, err := store.Open(dt.ctx.Layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	inspSeedPR(t, st, "talkable/talkable", 42, store.PRReviewing, nil)
}

func TestDaemonStopUnloadsLaunchdJob(t *testing.T) {
	dt := newDaemonGroupTest(t,
		daemonRuleOK([]string{"launchctl", "print"}, launchctlPrint("running", 100)),
		daemonRuleOK([]string{"launchctl", "bootout"}, ""),
	)
	daemonSys.DaemonPID = func(paths.Layout) (int, error) { return 100, nil }
	if code := dt.run("daemon-stop"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if n := len(dt.fake.CallsWithPrefix("launchctl", "bootout")); n != 1 {
		t.Fatalf("bootout calls = %d", n)
	}
	if len(dt.killed) != 0 {
		t.Fatalf("signalled the launchd daemon directly: %v", dt.killed)
	}
	actContains(t, dt.stdout.String(), "unloaded zhuravel.magnum")
}

func TestDaemonStopSignalsADaemonOutsideLaunchd(t *testing.T) {
	comm, args := daemonRulePS("/Users/b/Projects/magnum/bin/magnum", "/Users/b/Projects/magnum/bin/magnum daemon")
	dt := newDaemonGroupTest(t,
		daemonRuleExit([]string{"launchctl", "print"}, 113, "Could not find service"),
		comm, args,
	)
	alive := true
	daemonSys.DaemonPID = func(paths.Layout) (int, error) {
		if alive {
			return 250, nil
		}
		return 0, nil
	}
	daemonSys.Kill = func(pid int, sig syscall.Signal) error {
		dt.killed = append(dt.killed, sig.String())
		alive = false
		return nil
	}
	if code := dt.run("daemon-stop"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if len(dt.killed) != 1 || dt.killed[0] != "terminated" {
		t.Fatalf("signals = %v", dt.killed)
	}
	actContains(t, dt.stdout.String(), "outside launchd (pid 250)")
}

func TestDaemonStopNothingRunning(t *testing.T) {
	dt := newDaemonGroupTest(t, daemonRuleExit([]string{"launchctl", "print"}, 113, ""))
	if code := dt.run("daemon-stop"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	actContains(t, dt.stdout.String(), "not running")
}

func TestDaemonStopRefusesWhileRoundsRunUnlessNow(t *testing.T) {
	dt := newDaemonGroupTest(t,
		daemonRuleOK([]string{"launchctl", "print"}, launchctlPrint("running", 100)),
		daemonRuleOK([]string{"launchctl", "bootout"}, ""),
	)
	daemonSys.DaemonPID = func(paths.Layout) (int, error) { return 100, nil }
	seedRoundInFlight(t, dt)
	if code := dt.run("daemon-stop"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	actContains(t, dt.stderr.String(), "talkable/talkable#42 (reviewing)", "--now")
	if n := len(dt.fake.CallsWithPrefix("launchctl", "bootout")); n != 0 {
		t.Fatalf("bootout calls = %d, want 0", n)
	}
	if code := dt.run("daemon-stop", "--now"); code != 0 {
		t.Fatalf("--now: exit %d, stderr: %s", code, dt.stderr)
	}
	if n := len(dt.fake.CallsWithPrefix("launchctl", "bootout")); n != 1 {
		t.Fatalf("bootout calls = %d, want 1", n)
	}
}

// The daemon can exit between the pidfile read and ps: that is a stop, not
// a stale pidfile.
func TestDaemonStopToleratesADaemonThatExitedMeanwhile(t *testing.T) {
	dt := newDaemonGroupTest(t,
		daemonRuleExit([]string{"launchctl", "print"}, 113, ""),
		daemonRuleExit([]string{"ps", "-p", "250"}, 1, ""),
	)
	calls := 0
	daemonSys.DaemonPID = func(paths.Layout) (int, error) {
		calls++
		if calls == 1 {
			return 250, nil
		}
		return 0, nil
	}
	if code := dt.run("daemon-stop"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if len(dt.killed) != 0 {
		t.Fatalf("signals = %v", dt.killed)
	}
}

func TestDaemonRestartFailsWhenNoNewDaemonStarts(t *testing.T) {
	dt := newDaemonGroupTest(t,
		launchctlPrintSequence(launchctlPrint("running", 100), launchctlPrint("not running", 0)),
		daemonRuleOK([]string{"launchctl", "kickstart", "-k", "gui/501/zhuravel.magnum"}, ""),
	)
	daemonSys.DaemonPID = func(paths.Layout) (int, error) { return 100, nil }
	if code := dt.run("daemon-restart"); code != 1 {
		t.Fatalf("exit %d, want 1; stdout %s", code, dt.stdout)
	}
	actContains(t, dt.stderr.String(), "no new daemon was running", "not running", dt.ctx.Layout.DaemonLog(), "launchd.log")
	if strings.Contains(dt.stdout.String(), "restarted") {
		t.Fatalf("claimed a restart: %s", dt.stdout)
	}
}

func TestInstallRefusesWhileRoundsRunUnlessNow(t *testing.T) {
	dt := newDaemonGroupTest(t, installRules(herdrPluginListEmpty)...)
	dt.writeBinary(t)
	seedRoundInFlight(t, dt)
	if code := dt.run("install"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	actContains(t, dt.stderr.String(), "magnum install", "talkable/talkable#42 (reviewing)", "--now")
	if n := len(dt.fake.CallsWithPrefix("launchctl", "bootstrap")); n != 0 {
		t.Fatalf("bootstrap calls = %d, want 0", n)
	}
	if code := dt.run("install", "--now"); code != 0 {
		t.Fatalf("--now: exit %d, stderr: %s", code, dt.stderr)
	}
	if n := len(dt.fake.CallsWithPrefix("launchctl", "bootstrap")); n != 1 {
		t.Fatalf("bootstrap calls = %d, want 1", n)
	}
}

// After a crash the registry can still show a round in flight; with no
// daemon running nothing is abandoned, so install goes ahead.
func TestInstallIgnoresStaleRoundsWithoutADaemon(t *testing.T) {
	rules := installRules(herdrPluginListEmpty)
	rules[4] = daemonRuleExit([]string{"launchctl", "print"}, 113, "Could not find service")
	dt := newDaemonGroupTest(t, rules...)
	dt.writeBinary(t)
	seedRoundInFlight(t, dt)
	if code := dt.run("install"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
}

func TestUninstallRefusesWhileRoundsRunUnlessNow(t *testing.T) {
	dt := newDaemonGroupTest(t,
		daemonRuleOK([]string{"launchctl", "print"}, launchctlPrint("running", 100)),
		daemonRuleOK([]string{"launchctl", "bootout"}, ""),
		daemonRuleOK([]string{"herdr", "plugin", "list"}, herdrPluginListEmpty),
	)
	seedRoundInFlight(t, dt)
	if code := dt.run("uninstall"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	actContains(t, dt.stderr.String(), "magnum uninstall", "--now")
	if n := len(dt.fake.CallsWithPrefix("launchctl", "bootout")); n != 0 {
		t.Fatalf("bootout calls = %d, want 0", n)
	}
	if code := dt.run("uninstall", "--now"); code != 0 {
		t.Fatalf("--now: exit %d, stderr: %s", code, dt.stderr)
	}
	if code := dt.run("uninstall", "--dry-run"); code != 0 {
		t.Fatalf("--dry-run must not need --now: exit %d, stderr: %s", code, dt.stderr)
	}
}

// launchctl print failing leaves the job's state unknown: daemon-stop still
// runs bootout, so its result (or launchctl's real error) decides.
func TestDaemonStopBootsOutWhenStatusFails(t *testing.T) {
	dt := newDaemonGroupTest(t,
		daemonRuleExit([]string{"launchctl", "print"}, 1, "Could not print domain: Operation not permitted"),
		daemonRuleOK([]string{"launchctl", "bootout"}, ""),
	)
	if code := dt.run("daemon-stop"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if n := len(dt.fake.CallsWithPrefix("launchctl", "bootout")); n != 1 {
		t.Fatalf("bootout calls = %d", n)
	}
	actContains(t, dt.stdout.String(), "unloaded zhuravel.magnum")
	actContains(t, dt.stderr.String(), "warning")

	dt = newDaemonGroupTest(t,
		daemonRuleExit([]string{"launchctl", "print"}, 1, "Could not print domain: Operation not permitted"),
		daemonRuleExit([]string{"launchctl", "bootout"}, 1, "Boot-out failed: 1: Operation not permitted"),
	)
	if code := dt.run("daemon-stop"); code != 1 {
		t.Fatalf("exit %d, want 1; stderr: %s", code, dt.stderr)
	}
	actContains(t, dt.stderr.String(), "launchctl bootout")
}
