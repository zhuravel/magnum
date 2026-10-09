package cli

import (
	"context"
	"strings"
	"syscall"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// printSequence answers successive `launchctl print` calls with outs (the
// last one repeats).
func launchctlPrintSequence(outs ...string) execx.Rule {
	i := 0
	return execx.Rule{Prefix: []string{"launchctl", "print"}, Fn: func(execx.Cmd) (execx.Result, error) {
		out := outs[min(i, len(outs)-1)]
		i++
		return execx.Result{Stdout: []byte(out)}, nil
	}}
}

func TestDaemonRestartKickstartsLaunchdJob(t *testing.T) {
	dt := newDaemonGroupTest(t,
		launchctlPrintSequence(launchctlPrint("running", 100), launchctlPrint("running", 100), launchctlPrint("running", 200)),
		daemonRuleOK([]string{"launchctl", "kickstart", "-k", "gui/501/zhuravel.magnum"}, ""),
	)
	daemonSys.DaemonPID = func(paths.Layout) (int, error) { return 100, nil }
	if code := dt.run("daemon-restart"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if n := len(dt.fake.CallsWithPrefix("launchctl", "kickstart")); n != 1 {
		t.Fatalf("kickstart calls = %d", n)
	}
	if len(dt.killed) != 0 {
		t.Fatalf("signalled the launchd daemon directly: %v", dt.killed)
	}
	if !strings.Contains(dt.stdout.String(), "pid 200") {
		t.Fatalf("output: %s", dt.stdout)
	}
}

func TestDaemonRestartStopsForegroundDaemonThenKickstarts(t *testing.T) {
	comm, args := daemonRulePS("/Users/b/Projects/magnum/bin/magnum", "/Users/b/Projects/magnum/bin/magnum daemon")
	dt := newDaemonGroupTest(t,
		launchctlPrintSequence(launchctlPrint("not running", 0), launchctlPrint("running", 300)),
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
	daemonSys.Kill = func(pid int, sig syscall.Signal) error {
		dt.killed = append(dt.killed, sig.String())
		alive = false
		return nil
	}
	if code := dt.run("daemon-restart"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if len(dt.killed) != 1 || dt.killed[0] != "terminated" {
		t.Fatalf("signals = %v", dt.killed)
	}
	if n := len(dt.fake.CallsWithPrefix("launchctl", "kickstart")); n != 1 {
		t.Fatalf("kickstart calls = %d", n)
	}
	if !strings.Contains(dt.stdout.String(), "outside launchd (pid 250)") {
		t.Fatalf("output: %s", dt.stdout)
	}
}

func TestDaemonRestartWithoutLaunchdStopsAndSaysHowToStart(t *testing.T) {
	comm, args := daemonRulePS("magnum", "magnum daemon --once")
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
	if code := dt.run("daemon-restart"); code != 1 {
		t.Fatalf("exit %d, want 1 (stopped, not restarted)", code)
	}
	if len(dt.killed) != 1 {
		t.Fatalf("signals = %v", dt.killed)
	}
	if calls := dt.fake.CallsWithPrefix("launchctl", "kickstart"); len(calls) != 0 {
		t.Fatalf("kickstarted a job that is not loaded: %+v", calls)
	}
	if !strings.Contains(dt.stderr.String(), "magnum install") {
		t.Fatalf("stderr lacks the fix: %s", dt.stderr)
	}
}

func TestDaemonRestartNothingRunning(t *testing.T) {
	dt := newDaemonGroupTest(t, daemonRuleExit([]string{"launchctl", "print"}, 113, ""))
	if code := dt.run("daemon-restart"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(dt.stderr.String(), "magnum install") || !strings.Contains(dt.stderr.String(), "magnum daemon") {
		t.Fatalf("stderr lacks the fix: %s", dt.stderr)
	}
}

func TestDaemonRestartRefusesToSignalARecycledPid(t *testing.T) {
	// The pid now belongs to an editor that opened a file named magnum.
	comm, args := daemonRulePS("/usr/bin/vim", "/usr/bin/vim /tmp/magnum daemon")
	dt := newDaemonGroupTest(t,
		daemonRuleExit([]string{"launchctl", "print"}, 113, ""),
		comm, args,
	)
	daemonSys.DaemonPID = func(paths.Layout) (int, error) { return 250, nil }
	if code := dt.run("daemon-restart"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if len(dt.killed) != 0 {
		t.Fatalf("signalled a non-magnum process: %v", dt.killed)
	}
	if !strings.Contains(dt.stderr.String(), dt.ctx.Layout.Pid()) {
		t.Fatalf("stderr should name the stale pidfile: %s", dt.stderr)
	}
}

func TestDaemonRestartRejectsArguments(t *testing.T) {
	dt := newDaemonGroupTest(t)
	if code := dt.run("daemon-restart", "now"); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

// A restart abandons review rounds in flight, so it is refused while any PR
// is claiming, reviewing or verifying, unless --now is passed.
func TestDaemonRestartRefusesWhileRoundsRun(t *testing.T) {
	dt := newDaemonGroupTest(t,
		launchctlPrintSequence(launchctlPrint("running", 100), launchctlPrint("running", 100), launchctlPrint("running", 200)),
		daemonRuleOK([]string{"launchctl", "kickstart", "-k", "gui/501/zhuravel.magnum"}, ""),
	)
	daemonSys.DaemonPID = func(paths.Layout) (int, error) { return 100, nil }
	storetest.Seed(t, dt.ctx.Layout.DB())
	st, err := store.Open(dt.ctx.Layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repo, err := st.UpsertRepo(ctx, store.Repo{NodeID: "R", Owner: "talkable", Name: "talkable", Mode: store.RepoModePool})
	if err != nil {
		t.Fatal(err)
	}
	res, err := st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: "P", Number: 42, URL: "u", HeadSHA: "abc",
		InitialState: store.PRQueued, Identity: "talkable-app"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.TransitionPR(ctx, res.PR.ID, []string{store.PRQueued}, store.PRReviewing, nil); err != nil {
		t.Fatal(err)
	}
	st.Close()

	if code := dt.run("daemon-restart"); code != 1 {
		t.Fatalf("exit %d, want 1 (rounds in flight); stderr: %s", code, dt.stderr)
	}
	if !strings.Contains(dt.stderr.String(), "talkable/talkable#42 (reviewing)") || !strings.Contains(dt.stderr.String(), "--now") {
		t.Fatalf("stderr: %s", dt.stderr)
	}
	if n := len(dt.fake.CallsWithPrefix("launchctl", "kickstart")); n != 0 {
		t.Fatalf("kickstart calls = %d, want 0", n)
	}
	if code := dt.run("daemon-restart", "--now"); code != 0 {
		t.Fatalf("--now: exit %d, stderr: %s", code, dt.stderr)
	}
	if n := len(dt.fake.CallsWithPrefix("launchctl", "kickstart")); n != 1 {
		t.Fatalf("kickstart calls = %d, want 1", n)
	}
}
