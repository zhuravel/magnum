package cli

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
// it, unless stuck, or until healAt, from which launchd's own attempts
// succeed. A bootstrap spawns the job after bootstrapDelay. Each new daemon
// takes the next pid from 200; the pidfile names the running one.
type fakeLaunchd struct {
	clock          time.Duration
	loaded         bool
	pid            int
	nextPID        int
	runs           int
	spawnAt        time.Duration // the next spawn; negative: none scheduled
	delay          time.Duration // from the kickstart to its spawn
	bootstrapDelay time.Duration // from the bootstrap to its spawn
	spawnFails     bool
	healAt         time.Duration // a spawn from then on succeeds; 0: never by itself
	crashes        bool          // the program runs and exits 1 at once (not launchd's failure)
	stuck          bool          // a reload does not heal the spawn failure
	bootstrap      execx.Result

	kickstarts, bootouts, bootstraps int
	bootoutAt                        time.Duration
}

func newFakeLaunchd() *fakeLaunchd {
	return &fakeLaunchd{loaded: true, pid: 100, nextPID: 200, spawnAt: -1}
}

// settle runs the spawns whose time came: a running daemon, or failed
// attempts, each rescheduled a throttle later.
func (f *fakeLaunchd) settle() {
	for f.loaded && f.pid == 0 && f.spawnAt >= 0 && f.clock >= f.spawnAt {
		f.runs++
		healed := f.healAt > 0 && f.spawnAt >= f.healAt
		if f.spawnFails && !healed || f.crashes {
			f.spawnAt += 10 * time.Second
			continue
		}
		f.pid, f.spawnAt = f.nextPID, -1
		f.nextPID++
	}
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
			f.loaded, f.runs, f.spawnAt = true, 0, f.clock+f.bootstrapDelay // RunAtLoad
			f.spawnFails = f.spawnFails && f.stuck
			return execx.Result{}, nil
		}},
	}
}

// restartSetup is the scene of a restart against f: its rules, a built binary
// (the build check passes quietly), the plist where install writes it, and
// f's clock and pidfile. It returns the plist's path and bytes.
func restartSetup(t *testing.T, f *fakeLaunchd, extra ...execx.Rule) (*daemonGroupTest, string, []byte) {
	t.Helper()
	dt := newDaemonGroupTest(t, append(extra, f.rules()...)...)
	dt.writeBinary(t)
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
	return dt, path, plist
}

// restartWithLaunchd runs `magnum daemon-restart` against f with a seeded
// registry for the events, and checks that the plist did not change.
func restartWithLaunchd(t *testing.T, f *fakeLaunchd) (*daemonGroupTest, int) {
	t.Helper()
	dt, path, plist := restartSetup(t, f)
	storetest.Seed(t, dt.ctx.Layout.DB())
	code := dt.run("daemon-restart")
	if got, _ := os.ReadFile(path); string(got) != string(plist) {
		t.Errorf("the plist changed:\n%s", got)
	}
	return dt, code
}

// reloadEvents are the kinds and levels of the reload's events in the test
// registry.
func reloadEvents(t *testing.T, dt *daemonGroupTest) []string {
	t.Helper()
	st, err := store.Open(dt.ctx.Layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	return reloadEventsIn(t, st.DB())
}

func reloadEventsIn(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("SELECT kind, level, message FROM events WHERE kind LIKE 'daemon.reload%' ORDER BY rowid")
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

func wantReloadEvents(t *testing.T, dt *daemonGroupTest, want ...string) {
	t.Helper()
	if got := reloadEvents(t, dt); !slices.Equal(got, want) {
		t.Fatalf("reload events = %q, want %q", got, want)
	}
}

// reloadMessages are the messages of the reload's events, in order.
func reloadMessages(t *testing.T, dt *daemonGroupTest) []string {
	t.Helper()
	st, err := store.Open(dt.ctx.Layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows, err := st.DB().Query("SELECT message FROM events WHERE kind LIKE 'daemon.reload%' ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var msg string
		if err := rows.Scan(&msg); err != nil {
			t.Fatal(err)
		}
		out = append(out, msg)
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
	wantReloadEvents(t, dt, "daemon.reload_begin warn", "daemon.reloaded info")
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
	wantReloadEvents(t, dt)
}

// The job a reload loads again may wait out launchd's throttle too: a daemon
// that starts 10 s after the bootstrap is a healed restart, with one reload
// and its two events.
func TestDaemonRestartWaitsForADaemonLaunchdStartsAfterTheReload(t *testing.T) {
	f := newFakeLaunchd()
	f.spawnFails = true
	f.bootstrapDelay = 10 * time.Second
	dt, code := restartWithLaunchd(t, f)
	if code != 0 {
		t.Fatalf("exit %d, stdout: %s\nstderr: %s", code, dt.stdout, dt.stderr)
	}
	if f.bootouts != 1 || f.bootstraps != 1 {
		t.Fatalf("bootouts %d, bootstraps %d; want one each", f.bootouts, f.bootstraps)
	}
	actContains(t, dt.stdout.String(), "restarted zhuravel.magnum via launchd after reloading the job (running, pid 200)")
	if s := dt.stderr.String(); s != "" {
		t.Fatalf("stderr: %s", s)
	}
	wantReloadEvents(t, dt, "daemon.reload_begin warn", "daemon.reloaded info")
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
	wantReloadEvents(t, dt, "daemon.reload_begin warn", "daemon.reload_failed error")
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
	wantReloadEvents(t, dt, "daemon.reload_begin warn", "daemon.reload_failed error")
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
	wantReloadEvents(t, dt)
}

// The begin event is written first and the guard is read after it, right
// before the bootout: a daemon that runs by then, whether launchd's own
// retry started it while the event waited on a busy registry or someone
// started one by hand, is not booted out, and the restart ends with success
// and an event that says the reload was skipped. A plist the reload cannot
// use does not hide a daemon launchd started meanwhile. The line that
// announces the reload says that a last check may cancel it.
func TestDaemonRestartReloadsNothingOnceADaemonRunsAfterTheWait(t *testing.T) {
	for _, plistGone := range []bool{false, true} {
		name := "launchd's retry starts it while the begin event waits"
		if plistGone {
			name += ", and the plist is gone"
		}
		t.Run(name, func(t *testing.T) {
			f := newFakeLaunchd()
			f.spawnFails = true
			f.healAt = 16 * time.Second // the attempts at 0 s and 10 s fail; the one at 20 s succeeds
			dt, path, plist := restartSetup(t, f)
			storetest.Seed(t, dt.ctx.Layout.DB())
			if plistGone {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			daemonSys.RestartEvent = func(ctx context.Context, c *Context, level, kind, msg string) {
				c.writeRestartEvent(ctx, level, kind, msg)
				if kind == "daemon.reload_begin" {
					f.clock += 10 * time.Second // the registry was busy
				}
			}
			if code := dt.run("daemon-restart"); code != 0 {
				t.Fatalf("exit %d, stdout: %s\nstderr: %s", code, dt.stdout, dt.stderr)
			}
			if f.bootouts != 0 || f.bootstraps != 0 {
				t.Fatalf("bootouts %d, bootstraps %d with launchd's daemon running", f.bootouts, f.bootstraps)
			}
			actContains(t, dt.stdout.String(), "unless a last check finds a daemon running or a reason not to",
				"launchd started zhuravel.magnum meanwhile; the job was not reloaded",
				"restarted zhuravel.magnum via launchd (running, pid 200)")
			if s := dt.stderr.String(); s != "" {
				t.Fatalf("stderr: %s", s)
			}
			if got, _ := os.ReadFile(path); !plistGone && !bytes.Equal(got, plist) {
				t.Fatalf("the plist changed:\n%s", got)
			}
			wantReloadEvents(t, dt, "daemon.reload_begin warn", "daemon.reload_skipped info")
		})
	}
	t.Run("a daemon started by hand holds the pidfile", func(t *testing.T) {
		f := newFakeLaunchd()
		f.spawnFails = true
		comm, args := daemonRulePS("/Users/b/Projects/magnum/bin/magnum", "/Users/b/Projects/magnum/bin/magnum daemon")
		dt, _, _ := restartSetup(t, f, comm, args)
		storetest.Seed(t, dt.ctx.Layout.DB())
		daemonSys.DaemonPID = func(paths.Layout) (int, error) {
			if f.clock >= launchdStartWait {
				return 250, nil
			}
			return f.pid, nil
		}
		if code := dt.run("daemon-restart"); code != 0 {
			t.Fatalf("exit %d, stdout: %s\nstderr: %s", code, dt.stdout, dt.stderr)
		}
		if f.bootouts != 0 || f.bootstraps != 0 {
			t.Fatalf("bootouts %d, bootstraps %d with a daemon running", f.bootouts, f.bootstraps)
		}
		actContains(t, dt.stdout.String(), "a magnum daemon runs outside launchd (pid 250); the job was not reloaded")
		if s := dt.stderr.String(); s != "" {
			t.Fatalf("stderr: %s", s)
		}
		// launchd's job still cannot spawn: the event, the trace that stays,
		// warns and says so.
		wantReloadEvents(t, dt, "daemon.reload_begin warn", "daemon.reload_skipped warn")
		if msgs := reloadMessages(t, dt); !strings.Contains(msgs[1], "launchd still reports spawn scheduled, last exit code 78") {
			t.Fatalf("skip event %q does not say that the job still cannot spawn", msgs[1])
		}
	})
}

// The command line of a process that holds a recycled pid is not magnum's to
// keep (it can carry a password): a refusal names the pid and the
// executable's base name, never its arguments, in the event and the output.
func TestARefusedReloadNamesAStrangerByItsExecutableAlone(t *testing.T) {
	f := newFakeLaunchd()
	f.spawnFails = true
	comm, args := daemonRulePS("/usr/local/bin/mysql", "/usr/local/bin/mysql -uroot -pHUNTER2 talkable")
	dt, _, _ := restartSetup(t, f, comm, args)
	storetest.Seed(t, dt.ctx.Layout.DB())
	daemonSys.DaemonPID = func(paths.Layout) (int, error) {
		if f.clock >= launchdStartWait {
			return 250, nil
		}
		return f.pid, nil
	}
	if code := dt.run("daemon-restart"); code != 1 {
		t.Fatalf("exit %d, want 1; stdout: %s", code, dt.stdout)
	}
	actContains(t, dt.stderr.String(), "pid 250 holds the daemon's pidfile", "is not a magnum daemon (mysql)")
	wantReloadEvents(t, dt, "daemon.reload_begin warn", "daemon.reload_refused warn")
	msgs := reloadMessages(t, dt)
	if !strings.Contains(msgs[1], "is not a magnum daemon (mysql)") {
		t.Fatalf("refusal event %q does not name the executable", msgs[1])
	}
	for _, out := range append(msgs, dt.stdout.String(), dt.stderr.String()) {
		if strings.Contains(out, "HUNTER2") || strings.Contains(out, "-uroot") || strings.Contains(out, "/usr/local/bin") {
			t.Fatalf("the stranger's command line got out: %q", out)
		}
	}
}

// A reload that does not run says why in an event, after its begin event,
// and leaves the job as it is: a pidfile naming a process that is not a
// magnum daemon (a recycled pid), or no plist to load the job from.
func TestDaemonRestartRecordsWhyItDidNotReload(t *testing.T) {
	t.Run("a stranger in the pidfile", func(t *testing.T) {
		f := newFakeLaunchd()
		f.spawnFails = true
		comm, args := daemonRulePS("/usr/bin/vim", "/usr/bin/vim notes.txt")
		dt, _, _ := restartSetup(t, f, comm, args)
		storetest.Seed(t, dt.ctx.Layout.DB())
		daemonSys.DaemonPID = func(paths.Layout) (int, error) {
			if f.clock >= launchdStartWait {
				return 250, nil
			}
			return f.pid, nil
		}
		if code := dt.run("daemon-restart"); code != 1 {
			t.Fatalf("exit %d, want 1; stdout: %s", code, dt.stdout)
		}
		if f.bootouts != 0 {
			t.Fatalf("booted out with pid 250 in the pidfile")
		}
		actContains(t, dt.stderr.String(), "the job was not reloaded: pid 250 holds the daemon's pidfile", "is not a magnum daemon")
		wantReloadEvents(t, dt, "daemon.reload_begin warn", "daemon.reload_refused warn")
	})
	t.Run("no plist", func(t *testing.T) {
		f := newFakeLaunchd()
		f.spawnFails = true
		dt, path, _ := restartSetup(t, f)
		storetest.Seed(t, dt.ctx.Layout.DB())
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if code := dt.run("daemon-restart"); code != 1 {
			t.Fatalf("exit %d, want 1; stdout: %s", code, dt.stdout)
		}
		if f.bootouts != 0 || !f.loaded {
			t.Fatalf("bootouts %d, loaded %v without a plist to load it from", f.bootouts, f.loaded)
		}
		actContains(t, dt.stderr.String(), "the job was not reloaded: launchd reload: read the job's plist: open "+path, "magnum install")
		if strings.Contains(dt.stderr.String(), "left as it was") {
			t.Fatalf("the refusal says it twice: %s", dt.stderr)
		}
		wantReloadEvents(t, dt, "daemon.reload_begin warn", "daemon.reload_refused warn")
	})
}

// The reload's events are a record, not a step: with no registry, a
// read-only or busy one, or one of an older schema, the restart heals and
// says so as it does with a registry it can write, and the registry is
// neither created nor migrated.
func TestDaemonRestartReloadKeepsTheRegistryAsItIs(t *testing.T) {
	heal := func(t *testing.T, dt *daemonGroupTest, f *fakeLaunchd) {
		t.Helper()
		if code := dt.run("daemon-restart", "--now"); code != 0 {
			t.Fatalf("exit %d, stdout: %s\nstderr: %s", code, dt.stdout, dt.stderr)
		}
		if f.bootouts != 1 || f.bootstraps != 1 {
			t.Fatalf("bootouts %d, bootstraps %d; want one each", f.bootouts, f.bootstraps)
		}
		actContains(t, dt.stdout.String(), "reloading the job once",
			"restarted zhuravel.magnum via launchd after reloading the job (running, pid 200)")
		if s := dt.stderr.String(); s != "" {
			t.Fatalf("stderr: %s", s)
		}
	}
	t.Run("none", func(t *testing.T) {
		f := newFakeLaunchd()
		f.spawnFails = true
		dt, _, _ := restartSetup(t, f)
		heal(t, dt, f)
		if _, err := os.Stat(dt.ctx.Layout.DB()); !os.IsNotExist(err) {
			t.Fatalf("the restart created the registry: %v", err)
		}
	})
	t.Run("read-only", func(t *testing.T) {
		f := newFakeLaunchd()
		f.spawnFails = true
		dt, _, _ := restartSetup(t, f)
		db := dt.ctx.Layout.DB()
		storetest.Seed(t, db)
		before, err := os.ReadFile(db)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(db, 0o444); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(db, 0o644) })
		heal(t, dt, f)
		if after, _ := os.ReadFile(db); !bytes.Equal(before, after) {
			t.Fatal("the read-only registry changed")
		}
	})
	t.Run("busy", func(t *testing.T) {
		storetest.Serial(t) // swaps registryWriteBusy
		f := newFakeLaunchd()
		f.spawnFails = true
		dt, _, _ := restartSetup(t, f)
		storetest.Seed(t, dt.ctx.Layout.DB())
		old := registryWriteBusy
		registryWriteBusy = 50 * time.Millisecond
		t.Cleanup(func() { registryWriteBusy = old })
		holder, err := sql.Open("sqlite", registryWriteDSN(dt.ctx.Layout.DB()))
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Close()
		tx, err := holder.Begin()
		if err != nil {
			t.Fatal(err)
		}
		// Another writer holds the registry for the whole restart.
		if _, err := tx.Exec("INSERT INTO kv (key, value, updated_at) VALUES ('test.busy', 'x', 'now')"); err != nil {
			t.Fatal(err)
		}
		heal(t, dt, f)
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		wantReloadEvents(t, dt)
	})
	t.Run("an older schema", func(t *testing.T) {
		f := newFakeLaunchd()
		f.spawnFails = true
		dt, _, _ := restartSetup(t, f)
		older := store.LatestSchemaVersion() - 1
		db := olderSchemaRegistry(t, dt, older)
		tables := func() []string {
			t.Helper()
			rows, err := db.Query("SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var out []string
			for rows.Next() {
				var n string
				if err := rows.Scan(&n); err != nil {
					t.Fatal(err)
				}
				out = append(out, n)
			}
			return out
		}
		before := tables()
		heal(t, dt, f)
		if v := registryVersion(t, dt.ctx.Layout.DB()); v != older {
			t.Fatalf("registry at schema %d, want %d (not migrated)", v, older)
		}
		if after := tables(); !slices.Equal(before, after) {
			t.Fatalf("tables %q, want %q", after, before)
		}
		if got := reloadEventsIn(t, db); !slices.Equal(got, []string{"daemon.reload_begin warn", "daemon.reloaded info"}) {
			t.Fatalf("reload events = %q", got)
		}
	})
}

// olderSchemaRegistry builds dt's registry from this build's first n
// migrations, as a magnum of that schema left it, and returns it open.
func olderSchemaRegistry(t *testing.T, dt *daemonGroupTest, n int) *sql.DB {
	t.Helper()
	files, err := filepath.Glob("../store/migrations/*.sql")
	if err != nil || len(files) != store.LatestSchemaVersion() {
		t.Fatalf("migrations: %d files (%v), want %d", len(files), err, store.LatestSchemaVersion())
	}
	slices.Sort(files)
	if err := dt.ctx.Layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dt.ctx.Layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for i, file := range files[:n] {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// The reload loads the plist install wrote, as it is: mise's prefix, the
// pinned config, the working directory and the environment stay byte for
// byte, and launchd bootstraps that very path.
func TestDaemonRestartReloadsThePlistInstallWrote(t *testing.T) {
	f := newFakeLaunchd()
	dt, _, _ := restartSetup(t, f, daemonRuleOK([]string{"/bin/sh", "-c", "command -v mise"}, "/opt/homebrew/bin/mise\n"))
	dt.useMiseKey(t)
	dt.ctx.cfgPath = "/etc/magnum/custom.toml"
	if code := dt.run("install"); code != 0 {
		t.Fatalf("install: exit %d, stderr: %s", code, dt.stderr)
	}
	path := launchd.AgentPath(dt.userHome, launchd.DefaultLabel)
	plist, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	actContains(t, string(plist), "<string>/opt/homebrew/bin/mise</string>", "<key>MAGNUM_CONFIG</key>\n\t\t<string>/etc/magnum/custom.toml</string>",
		"<key>WorkingDirectory</key>", "<key>HERDR_SOCKET_PATH</key>", "<key>MAGNUM_HOME</key>")
	bootouts, bootstraps := f.bootouts, f.bootstraps
	f.spawnFails = true // the rebuilt binary: launchd cannot spawn it
	dt.stdout.Reset()
	dt.stderr.Reset()
	if code := dt.run("daemon-restart"); code != 0 {
		t.Fatalf("exit %d, stdout: %s\nstderr: %s", code, dt.stdout, dt.stderr)
	}
	if f.bootouts-bootouts != 1 || f.bootstraps-bootstraps != 1 {
		t.Fatalf("the restart booted out %d times and bootstrapped %d times; want one each", f.bootouts-bootouts, f.bootstraps-bootstraps)
	}
	calls := dt.fake.CallsWithPrefix("launchctl", "bootstrap")
	if last := calls[len(calls)-1]; !slices.Equal(last.Args, []string{"bootstrap", "gui/501", path}) {
		t.Fatalf("bootstrapped %q, want %s", last.Args, path)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, plist) {
		t.Fatalf("the reload changed the plist install wrote:\n%s", got)
	}
	actContains(t, dt.stdout.String(), "restarted zhuravel.magnum via launchd after reloading the job (running, pid 201)")
}

// The help and the README give the waits as the constants set them, so a
// change of the throttle cannot leave them stating another wait.
func TestTheRestartWaitsInTheHelpAndTheREADMEFollowTheConstants(t *testing.T) {
	help := commandHelp(t, "daemon-restart")
	for _, want := range []string{
		"waits " + launchdStartWait.String() + " for the new daemon",
		"launchd's " + launchdThrottle.String() + " throttle",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("the help lacks %q:\n%s", want, help)
		}
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	secs := func(d time.Duration) int { return int(d / time.Second) }
	for _, want := range []string{
		fmt.Sprintf("waits %d s for the new daemon", secs(launchdStartWait)),
		fmt.Sprintf("its %d s ThrottleInterval", secs(launchdThrottle)),
		fmt.Sprintf("waits %d s more", secs(launchdStartWait)),
	} {
		if !strings.Contains(string(readme), want) {
			t.Errorf("README.md lacks %q", want)
		}
	}
}

// The reload's guard is read again right before the bootout: a new daemon
// launchd started meanwhile ends the restart with success, a live pid in the
// pidfile is left to the caller to name, and an unreadable state or a spawn
// failure gone keeps the job as it is.
func TestReloadGuardRereadsTheStateRightBeforeTheReload(t *testing.T) {
	failed := "gui/501/zhuravel.magnum = {\n\tstate = spawn scheduled\n\truns = 3\n\tjob state = spawn failed\n\tlast exit code = 78: EX_CONFIG\n}\n"
	cases := []struct {
		name    string
		print   execx.Rule
		pidfile int
		started bool
		held    int
		refused string
	}{
		{"still failing", daemonRuleOK([]string{"launchctl", "print"}, failed), 0, false, 0, ""},
		{"a new daemon runs", daemonRuleOK([]string{"launchctl", "print"}, launchctlPrint("running", 200)), 200, true, 0, ""},
		{"the old daemon runs", daemonRuleOK([]string{"launchctl", "print"}, launchctlPrint("running", 100)), 100, false, 0, "launchd runs the job (running, pid 100)"},
		{"a daemon holds the pidfile", daemonRuleOK([]string{"launchctl", "print"}, failed), 300, false, 300, ""},
		{"unreadable state", daemonRuleExit([]string{"launchctl", "print"}, 1, "Operation not permitted"), 0, false, 0, "cannot read"},
		{"not loaded", daemonRuleExit([]string{"launchctl", "print"}, 113, "Could not find service"), 0, false, 0, "not loaded"},
		{"the failure is gone", daemonRuleOK([]string{"launchctl", "print"}, launchctlPrint("spawn scheduled", 0)), 0, false, 0, "no longer reports"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dt := newDaemonGroupTest(t, tc.print)
			daemonSys.DaemonPID = func(paths.Layout) (int, error) { return tc.pidfile, nil }
			chk := reloadGuard(context.Background(), dt.fake, dt.ctx.Layout, 501, launchd.DefaultLabel, 100)
			if chk.started != tc.started || chk.held != tc.held || (tc.refused == "") != (chk.refused == "") || !strings.Contains(chk.refused, tc.refused) {
				t.Fatalf("guard = %+v, want started %v, held %d, refused %q", chk, tc.started, tc.held, tc.refused)
			}
			if chk.reload() != (tc.name == "still failing") {
				t.Fatalf("reload() = %v for %+v", chk.reload(), chk)
			}
		})
	}
}
