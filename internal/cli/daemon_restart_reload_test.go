package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/launchd"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// fakeLaunchd is launchd as a restart meets it, on the clock daemonSys.Sleep
// moves. The job runs pid 100. A kickstart kills it and spawns the next one
// after delay (launchd's throttle); while spawnFails, every spawn fails as
// on 2026-10-28 (spawn scheduled, job state spawn failed, last exit code 78,
// a new attempt each 10 s), until a reload (bootout, then bootstrap) heals
// it, unless stuck. The new daemon is pid 200; the pidfile names the running
// one.
type fakeLaunchd struct {
	clock      time.Duration
	loaded     bool
	pid        int
	runs       int
	spawnAt    time.Duration // the next spawn; negative: none scheduled
	delay      time.Duration // from the kickstart to its spawn
	spawnFails bool
	crashes    bool // the program runs and exits 1 at once (not launchd's failure)
	stuck      bool // a reload does not heal the spawn failure
	bootstrap  execx.Result

	kickstarts, bootouts, bootstraps int
	bootoutAt                        time.Duration
}

func newFakeLaunchd() *fakeLaunchd { return &fakeLaunchd{loaded: true, pid: 100, spawnAt: -1} }

// settle spawns the job if its time came: a running daemon, or one more
// failed attempt rescheduled a throttle later.
func (f *fakeLaunchd) settle() {
	if !f.loaded || f.pid != 0 || f.spawnAt < 0 || f.clock < f.spawnAt {
		return
	}
	f.runs++
	if f.spawnFails || f.crashes {
		f.spawnAt = f.clock + 10*time.Second
		return
	}
	f.pid, f.spawnAt = 200, -1
}

func (f *fakeLaunchd) print(c execx.Cmd) (execx.Result, error) {
	f.settle()
	if !f.loaded {
		res := execx.Result{Code: 113, Stderr: []byte("Could not find service")}
		return res, &execx.ExitError{Cmd: c, Code: 113, Stderr: string(res.Stderr)}
	}
	if f.pid > 0 {
		return execx.Result{Stdout: []byte(launchctlPrint("running", f.pid))}, nil
	}
	out := "gui/501/zhuravel.magnum = {\n\tactive count = 0\n\tstate = spawn scheduled\n\truns = " + strconv.Itoa(f.runs) + "\n"
	switch {
	case f.runs > 0 && f.spawnFails:
		out += "\tjob state = spawn failed\n\tproperties = needs LWCR update | keepalive | runatload\n\tlast exit code = 78: EX_CONFIG\n"
	case f.runs > 0 && f.crashes:
		out += "\tlast exit code = 1\n"
	default:
		out += "\tlast exit code = 0\n" // the old daemon's clean exit on SIGTERM
	}
	return execx.Result{Stdout: []byte(out + "}\n")}, nil
}

func (f *fakeLaunchd) rules() []execx.Rule {
	return []execx.Rule{
		{Prefix: []string{"launchctl", "print"}, Fn: f.print},
		{Prefix: []string{"launchctl", "kickstart", "-k", "gui/501/zhuravel.magnum"}, Fn: func(execx.Cmd) (execx.Result, error) {
			f.kickstarts++
			f.pid, f.spawnAt = 0, f.clock+f.delay
			return execx.Result{}, nil
		}},
		{Prefix: []string{"launchctl", "bootout", "gui/501/zhuravel.magnum"}, Fn: func(execx.Cmd) (execx.Result, error) {
			f.bootouts++
			f.bootoutAt = f.clock
			f.loaded, f.pid, f.spawnAt = false, 0, -1
			return execx.Result{}, nil
		}},
		daemonRuleOK([]string{"launchctl", "enable", "gui/501/zhuravel.magnum"}, ""),
		{Prefix: []string{"launchctl", "bootstrap", "gui/501"}, Fn: func(c execx.Cmd) (execx.Result, error) {
			f.bootstraps++
			if f.bootstrap.Code != 0 {
				return f.bootstrap, &execx.ExitError{Cmd: c, Code: f.bootstrap.Code, Stderr: string(f.bootstrap.Stderr)}
			}
			f.loaded, f.runs, f.spawnAt = true, 0, f.clock // RunAtLoad
			f.spawnFails = f.spawnFails && f.stuck
			return execx.Result{}, nil
		}},
	}
}

// restartWithLaunchd runs `magnum daemon-restart` against f: a built binary
// (the build check passes quietly), a seeded registry for the events, the
// plist on disk where install writes it, and f's clock and pidfile.
func restartWithLaunchd(t *testing.T, f *fakeLaunchd) (*daemonGroupTest, int) {
	t.Helper()
	dt := newDaemonGroupTest(t, f.rules()...)
	dt.writeBinary(t)
	storetest.Seed(t, dt.ctx.Layout.DB())
	plist := launchd.Plist(launchd.Options{Label: launchd.DefaultLabel, ProgramArguments: []string{dt.ctx.Layout.Binary(), "daemon"}})
	path := launchd.AgentPath(dt.userHome, launchd.DefaultLabel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, plist, 0o644); err != nil {
		t.Fatal(err)
	}
	daemonSys.DaemonPID = func(paths.Layout) (int, error) { return f.pid, nil }
	daemonSys.Sleep = func(ctx context.Context, d time.Duration) error {
		f.clock += d
		return ctx.Err()
	}
	code := dt.run("daemon-restart")
	if got, _ := os.ReadFile(path); string(got) != string(plist) {
		t.Errorf("the plist changed:\n%s", got)
	}
	return dt, code
}

// reloadEvents are the kinds of the reload's events in the test registry.
func reloadEvents(t *testing.T, dt *daemonGroupTest) []string {
	t.Helper()
	st, err := store.Open(dt.ctx.Layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows, err := st.DB().Query("SELECT kind, level, message FROM events WHERE kind LIKE 'daemon.reload%' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind, level, msg string
		if err := rows.Scan(&kind, &level, &msg); err != nil {
			t.Fatal(err)
		}
		if msg == "" {
			t.Errorf("%s has no message", kind)
		}
		out = append(out, kind+" "+level)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// On 2026-10-28 launchd failed every spawn of the rebuilt binary with exit
// 78 after a restart, and only `magnum install --now` (unload, load) brought
// the daemon back. daemon-restart now reloads the job itself, once, after
// waiting past launchd's throttle, and ends with success when a daemon runs.
func TestDaemonRestartReloadsAJobLaunchdCannotSpawnOnce(t *testing.T) {
	f := newFakeLaunchd()
	f.spawnFails = true
	dt, code := restartWithLaunchd(t, f)
	if code != 0 {
		t.Fatalf("exit %d, stdout: %s\nstderr: %s", code, dt.stdout, dt.stderr)
	}
	if f.kickstarts != 1 || f.bootouts != 1 || f.bootstraps != 1 {
		t.Fatalf("kickstarts %d, bootouts %d, bootstraps %d; want one each", f.kickstarts, f.bootouts, f.bootstraps)
	}
	if f.bootoutAt <= 10*time.Second {
		t.Fatalf("reloaded %v after the kickstart, within launchd's 10s throttle", f.bootoutAt)
	}
	if f.pid != 200 {
		t.Fatalf("pid %d after the reload", f.pid)
	}
	actContains(t, dt.stdout.String(),
		"launchd could not spawn zhuravel.magnum after the restart (spawn scheduled, last exit code 78)",
		"reloading the job once",
		"restarted zhuravel.magnum via launchd after reloading the job (running, pid 200)")
	if s := dt.stderr.String(); s != "" {
		t.Fatalf("stderr: %s", s)
	}
	if got := strings.Join(reloadEvents(t, dt), ","); got != "daemon.reload_begin warn,daemon.reloaded info" {
		t.Fatalf("reload events = %s", got)
	}
}

// launchd starts the job at most once per ThrottleInterval (10 s): a daemon
// that appears 10 s after the kickstart is a normal restart, with no reload
// and no "no new daemon was running" warning.
func TestDaemonRestartWaitsOutLaunchdsThrottleWithoutAWarning(t *testing.T) {
	f := newFakeLaunchd()
	f.delay = 10 * time.Second
	dt, code := restartWithLaunchd(t, f)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if f.bootouts != 0 || f.bootstraps != 0 {
		t.Fatalf("bootouts %d, bootstraps %d after a throttled start", f.bootouts, f.bootstraps)
	}
	if s := dt.stderr.String(); s != "" {
		t.Fatalf("stderr: %s", s)
	}
	actContains(t, dt.stdout.String(), "restarted zhuravel.magnum via launchd (running, pid 200)")
	if strings.Contains(dt.stdout.String(), "reload") {
		t.Fatalf("stdout speaks of a reload: %s", dt.stdout)
	}
	if got := reloadEvents(t, dt); len(got) != 0 {
		t.Fatalf("reload events = %v", got)
	}
}

// A reload that does not help is reported with launchd's state and exit
// code, the daemon down as before; it is never tried a second time.
func TestDaemonRestartReportsAReloadThatDidNotHelpAndStops(t *testing.T) {
	f := newFakeLaunchd()
	f.spawnFails, f.stuck = true, true
	dt, code := restartWithLaunchd(t, f)
	if code != 1 {
		t.Fatalf("exit %d, want 1; stdout: %s", code, dt.stdout)
	}
	if f.kickstarts != 1 || f.bootouts != 1 || f.bootstraps != 1 {
		t.Fatalf("kickstarts %d, bootouts %d, bootstraps %d; want one each", f.kickstarts, f.bootouts, f.bootstraps)
	}
	actContains(t, dt.stderr.String(),
		"launchd restarted zhuravel.magnum, but no new daemon was running after 15s (spawn scheduled, last exit code 78)",
		"and still none 15s after reloading the job (spawn scheduled, last exit code 78)",
		"launchctl print gui/501/zhuravel.magnum", "magnum install --now")
	if got := strings.Join(reloadEvents(t, dt), ","); got != "daemon.reload_begin warn,daemon.reload_failed error" {
		t.Fatalf("reload events = %s", got)
	}
}

// A reload whose bootstrap fails says so: the job may now be unloaded, and
// `magnum install --now` loads it again.
func TestDaemonRestartReportsAFailedReload(t *testing.T) {
	f := newFakeLaunchd()
	f.spawnFails = true
	f.bootstrap = execx.Result{Code: 37, Stderr: []byte("Bootstrap failed: 37: Operation already in progress")}
	dt, code := restartWithLaunchd(t, f)
	if code != 1 {
		t.Fatalf("exit %d, want 1; stdout: %s", code, dt.stdout)
	}
	if f.bootouts != 1 || f.bootstraps != 1 {
		t.Fatalf("bootouts %d, bootstraps %d; want one each", f.bootouts, f.bootstraps)
	}
	actContains(t, dt.stderr.String(), "reloading the job failed", "launchctl bootstrap gui/501", "(not loaded)", "magnum install --now")
	if got := strings.Join(reloadEvents(t, dt), ","); got != "daemon.reload_begin warn,daemon.reload_failed error" {
		t.Fatalf("reload events = %s", got)
	}
}

// A program that runs and exits 1 (a daemon that refuses to start) is not
// launchd failing to spawn it: reloading the job would not change that, so
// the restart reports it as before and reloads nothing.
func TestDaemonRestartReloadsNothingForADaemonThatExitsAtStart(t *testing.T) {
	f := newFakeLaunchd()
	f.crashes = true
	dt, code := restartWithLaunchd(t, f)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if f.bootouts != 0 || f.bootstraps != 0 {
		t.Fatalf("bootouts %d, bootstraps %d for a daemon that ran", f.bootouts, f.bootstraps)
	}
	actContains(t, dt.stderr.String(), "no new daemon was running after 15s (spawn scheduled, last exit code 1)",
		"then `magnum daemon-restart` again")
	if got := reloadEvents(t, dt); len(got) != 0 {
		t.Fatalf("reload events = %v", got)
	}
}

// The reload's guard is read again right before it: a new daemon launchd
// started meanwhile ends the restart with success, and a daemon that holds
// the pidfile, an unreadable state or a spawn failure gone keeps the job as
// it is.
func TestReloadGuardRereadsTheStateRightBeforeTheReload(t *testing.T) {
	failed := "gui/501/zhuravel.magnum = {\n\tstate = spawn scheduled\n\truns = 3\n\tjob state = spawn failed\n\tlast exit code = 78: EX_CONFIG\n}\n"
	cases := []struct {
		name    string
		print   execx.Rule
		pidfile int
		started bool
		refused string
	}{
		{"still failing", daemonRuleOK([]string{"launchctl", "print"}, failed), 0, false, ""},
		{"a new daemon runs", daemonRuleOK([]string{"launchctl", "print"}, launchctlPrint("running", 200)), 200, true, ""},
		{"the old daemon runs", daemonRuleOK([]string{"launchctl", "print"}, launchctlPrint("running", 100)), 100, false, "launchd runs the job (running, pid 100)"},
		{"a daemon holds the pidfile", daemonRuleOK([]string{"launchctl", "print"}, failed), 300, false, "pid 300 holds"},
		{"unreadable state", daemonRuleExit([]string{"launchctl", "print"}, 1, "Operation not permitted"), 0, false, "cannot read"},
		{"not loaded", daemonRuleExit([]string{"launchctl", "print"}, 113, "Could not find service"), 0, false, "not loaded"},
		{"the failure is gone", daemonRuleOK([]string{"launchctl", "print"}, launchctlPrint("spawn scheduled", 0)), 0, false, "no longer reports"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dt := newDaemonGroupTest(t, tc.print)
			daemonSys.DaemonPID = func(paths.Layout) (int, error) { return tc.pidfile, nil }
			chk := reloadGuard(context.Background(), dt.fake, dt.ctx.Layout, 501, launchd.DefaultLabel, 100)
			if chk.started != tc.started || (tc.refused == "") != (chk.refused == "") || !strings.Contains(chk.refused, tc.refused) {
				t.Fatalf("guard = %+v, want started %v, refused %q", chk, tc.started, tc.refused)
			}
		})
	}
}
