package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// factoryInstallTest is the daemon group's harness with this build on disk
// and a factory daemon running as pid 250 (its pidfile, and ps naming it);
// kicks counts the wakes the command sends it.
func factoryInstallTest(t *testing.T) (dt *daemonGroupTest, kicks *int) {
	t.Helper()
	storetest.Serial(t) // newDaemonGroupTest swaps daemonSys
	comm, args := daemonRulePS("/opt/bin/magnum", "/opt/bin/magnum factory daemon")
	start := daemonRuleOK([]string{"ps", "-p", strconv.Itoa(os.Getpid()), "-o", "lstart="}, "Sat Nov  7 15:04:05 2026\n")
	dt = newDaemonGroupTest(t, append([]execx.Rule{comm, args, start}, installRules(herdrPluginListEmpty)...)...)
	dt.writeBinary(t)
	daemonSys.FactoryPID = func(paths.Layout) (int, error) { return 250, nil }
	kicks = new(int)
	daemonSys.FactoryKick = func(paths.Layout) (int, error) {
		*kicks++
		return 250, nil
	}
	return dt, kicks
}

// commandStart is when ps says the command started (its lstart, which
// pads the day; the drain keeps its words joined by one space).
const commandStart = "Sat Nov 7 15:04:05 2026"

// writeFactoryWork writes the record the factory daemon keeps.
func writeFactoryWork(t *testing.T, l paths.Layout, rec factory.WorkRecord) {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(l.FactoryWork()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsx.WriteFileAtomic(l.FactoryWork(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// factoryRound and factoryCheck are a local review round of PR-12340 and a
// precheck of task 7 that run.
var (
	factoryRound = factory.WorkItem{Kind: factory.WorkRound, Task: 3, What: "local review round 2 of PR-12340",
		Since: time.Now().Add(-18 * time.Minute)}
	factoryCheck = factory.WorkItem{Kind: factory.WorkCheck, Task: 7, What: "precheck of task 7", Since: time.Now().Add(-4 * time.Minute)}
)

// reloads counts the factory job's bootstraps.
func reloads(dt *daemonGroupTest) int {
	return len(dt.fake.CallsWithPrefix("launchctl", "bootstrap", "gui/501"))
}

// While the factory daemon runs a local review round or a check, install
// refuses and names them, what a restart would do to them and how to wait
// for them: the restart would cut them, as the 3 rounds and the precheck
// cut on 2026-11-06 and 11-07.
func TestFactoryInstallRefusesWhileTheFactoryRunsWork(t *testing.T) {
	dt, _ := factoryInstallTest(t)
	writeFactoryWork(t, dt.ctx.Layout, factory.WorkRecord{PID: 250, Work: []factory.WorkItem{factoryRound, factoryCheck}})

	if code := dt.run("factory", "install"); code != 1 {
		t.Fatalf("exit %d, want 1; stdout %s stderr %s", code, dt.stdout, dt.stderr)
	}
	actContains(t, dt.stderr.String(),
		"magnum factory install: 1 local review round(s) and 1 background job(s) in flight: local review round 2 of PR-12340 (since "+
			inspClock(time.Now(), factoryRound.Since)+"), precheck of task 7 (since "+inspClock(time.Now(), factoryCheck.Since)+")",
		"stopping the factory daemon now cuts them short: a local review round runs again from its start (its agents' turns are lost), "+
			"a proof or a precheck runs again",
		"fix: `magnum factory install --drain` starts nothing new and waits for these, --now interrupts them")
	if n := len(dt.fake.CallsWithPrefix("launchctl", "bootout")); n != 0 || reloads(dt) != 0 {
		t.Fatalf("bootout %d, bootstrap %d: the refusal touched launchd", n, reloads(dt))
	}
}

// factoryQA is a QA run of PR-12345 whose server runs.
var factoryQA = factory.WorkItem{Kind: factory.WorkQA, Task: 5, What: "QA run 1 of PR-12345", Since: time.Now().Add(-6 * time.Minute)}

// While the factory daemon runs a QA run, install refuses and names it: the
// restart would stop its server and its browser, and the run would start
// again.
func TestFactoryInstallRefusesWhileTheFactoryRunsQA(t *testing.T) {
	dt, _ := factoryInstallTest(t)
	writeFactoryWork(t, dt.ctx.Layout, factory.WorkRecord{PID: 250, Work: []factory.WorkItem{factoryQA}})

	if code := dt.run("factory", "install"); code != 1 {
		t.Fatalf("exit %d, want 1; stdout %s stderr %s", code, dt.stdout, dt.stderr)
	}
	actContains(t, dt.stderr.String(),
		"magnum factory install: 1 background job(s) in flight: QA run 1 of PR-12345 (since "+inspClock(time.Now(), factoryQA.Since)+")",
		"stopping the factory daemon now cuts them short: a QA run stops its server and its browser and starts again in a fresh folder",
		"fix: `magnum factory install --drain` starts nothing new and waits for these, --now interrupts them")
	if reloads(dt) != 0 {
		t.Fatal("the refusal reloaded the job")
	}
}

// --drain waits for a QA run as for any work, and reloads once the daemon,
// holding the drain, ended it.
func TestFactoryInstallDrainWaitsForAQARun(t *testing.T) {
	dt, _ := factoryInstallTest(t)
	l := dt.ctx.Layout
	writeFactoryWork(t, l, factory.WorkRecord{PID: 250, Work: []factory.WorkItem{factoryQA}})
	sleeps := 0
	daemonSys.Sleep = func(context.Context, time.Duration) error {
		sleeps++
		v, _ := factory.ReadDrain(l)
		switch sleeps {
		case 1: // the daemon's tick took the drain; the run's walker is at work
			writeFactoryWork(t, l, factory.WorkRecord{PID: 250, Drain: v, Work: []factory.WorkItem{factoryQA}})
		case 3: // the run ended
			writeFactoryWork(t, l, factory.WorkRecord{PID: 250, Drain: v, Work: []factory.WorkItem{}})
		}
		if sleeps < 3 && reloads(dt) != 0 {
			t.Errorf("sleep %d: the job was reloaded while the QA run ran", sleeps)
		}
		return nil
	}

	if code := dt.run("factory", "install", "--drain"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	actContains(t, dt.stdout.String(), "waiting for 1 background job(s) in flight (at most 3h30m0s): QA run 1 of PR-12345",
		"drained after 15s: nothing in flight")
	if reloads(dt) != 1 {
		t.Fatalf("bootstrap calls %d, want the job reloaded once", reloads(dt))
	}
}

// --now restarts the factory daemon at once, cutting what runs, as install
// always did.
func TestFactoryInstallNowCutsTheWork(t *testing.T) {
	dt, _ := factoryInstallTest(t)
	writeFactoryWork(t, dt.ctx.Layout, factory.WorkRecord{PID: 250, Work: []factory.WorkItem{factoryRound}})

	if code := dt.run("factory", "install", "--now"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	if reloads(dt) != 1 {
		t.Fatalf("bootstrap calls %d, want the job reloaded once", reloads(dt))
	}
}

// With nothing in flight install restarts the factory daemon without a
// word about it.
func TestFactoryInstallGoesOnWhenTheFactoryRunsNothing(t *testing.T) {
	dt, _ := factoryInstallTest(t)
	writeFactoryWork(t, dt.ctx.Layout, factory.WorkRecord{PID: 250, Work: []factory.WorkItem{}})

	if code := dt.run("factory", "install"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	if reloads(dt) != 1 {
		t.Fatalf("bootstrap calls %d, want the job reloaded once", reloads(dt))
	}
}

// A factory daemon that keeps no record of its work (a build before this
// one), or a record of another daemon's, cannot tell install what it runs:
// install refuses, fails closed, and --now goes on.
func TestFactoryInstallRefusesAFactoryDaemonThatKeepsNoRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  *factory.WorkRecord
	}{
		{"none", nil},
		{"another daemon's", &factory.WorkRecord{PID: 249, Work: []factory.WorkItem{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dt, _ := factoryInstallTest(t)
			if tc.rec != nil {
				writeFactoryWork(t, dt.ctx.Layout, *tc.rec)
			}
			for _, args := range [][]string{nil, {"--drain"}} {
				if code := dt.run("factory", append([]string{"install"}, args...)...); code != 1 {
					t.Fatalf("%v: exit %d, want 1; stdout %s stderr %s", args, code, dt.stdout, dt.stderr)
				}
			}
			actContains(t, dt.stderr.String(), "cannot tell what the factory daemon (pid 250) runs: it keeps no record of its work",
				"a build before this one", "--now")
			if v, err := factory.ReadDrain(dt.ctx.Layout); err != nil || v != "" {
				t.Fatalf("drain.json holds %q (%v): no daemon takes it", v, err)
			}
			if reloads(dt) != 0 {
				t.Fatal("the refusal reloaded the job")
			}
			if code := dt.run("factory", "install", "--now"); code != 0 || reloads(dt) != 1 {
				t.Fatalf("--now: exit %d, bootstrap %d; stderr %s", code, reloads(dt), dt.stderr)
			}
		})
	}
}

// --drain asks the factory daemon for a drain (its pid in it) and wakes it,
// says what it waits for, reports every 15 s, and reloads the job once the
// daemon's record says it holds the drain and nothing runs. The drain stays
// for the restarted daemon to end.
func TestFactoryInstallDrainWaitsForTheWorkThenReloads(t *testing.T) {
	dt, kicks := factoryInstallTest(t)
	l := dt.ctx.Layout
	writeFactoryWork(t, l, factory.WorkRecord{PID: 250, Work: []factory.WorkItem{factoryRound}})
	var value string
	sleeps := 0
	daemonSys.Sleep = func(context.Context, time.Duration) error {
		sleeps++
		v, err := factory.ReadDrain(l)
		if dr, ok := factory.ParseDrain(v); err != nil || !ok || dr.PID != os.Getpid() || dr.Start != commandStart {
			t.Errorf("sleep %d: drain %q (%v) does not name this process by its pid and start", sleeps, v, err)
		}
		value = v
		switch sleeps {
		case 2: // the daemon's tick took the drain; the round still runs
			writeFactoryWork(t, l, factory.WorkRecord{PID: 250, Drain: v, Work: []factory.WorkItem{factoryRound}})
		case 4:
			writeFactoryWork(t, l, factory.WorkRecord{PID: 250, Drain: v, Work: []factory.WorkItem{}})
		}
		if reloads(dt) != 0 {
			t.Errorf("sleep %d: the job was reloaded while the drain waited", sleeps)
		}
		return nil
	}

	if code := dt.run("factory", "install", "--drain"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	actContains(t, dt.stdout.String(),
		fmt.Sprintf("draining (pid %d): nothing new starts in the factory; waiting for 1 local review round(s) in flight (at most 3h30m0s): "+
			"local review round 2 of PR-12340", os.Getpid()),
		"draining: 1 local review round(s) in flight after 15s", "drained after 20s: nothing in flight",
		"launchd: loaded zhuravel.magnum.factory")
	if reloads(dt) != 1 {
		t.Fatalf("bootstrap calls %d, want the job reloaded once", reloads(dt))
	}
	if *kicks == 0 {
		t.Fatal("the drain did not wake the factory daemon")
	}
	if v, err := factory.ReadDrain(l); err != nil || v != value {
		t.Fatalf("drain.json holds %q (%v) after the reload, want %q left for the restarted daemon to end", v, err, value)
	}
}

// With nothing in flight --drain still waits for the daemon to hold the
// drain: until its record says so, a tick that began before the drain may
// start a round the restart would cut.
func TestFactoryInstallDrainWaitsUntilTheDaemonHoldsTheDrain(t *testing.T) {
	dt, _ := factoryInstallTest(t)
	l := dt.ctx.Layout
	writeFactoryWork(t, l, factory.WorkRecord{PID: 250, Work: []factory.WorkItem{}})
	sleeps := 0
	daemonSys.Sleep = func(context.Context, time.Duration) error {
		sleeps++
		if sleeps == 3 {
			v, _ := factory.ReadDrain(l)
			writeFactoryWork(t, l, factory.WorkRecord{PID: 250, Drain: v, Work: []factory.WorkItem{}})
		} else if reloads(dt) != 0 {
			t.Errorf("sleep %d: the job was reloaded before the daemon held the drain", sleeps)
		}
		return nil
	}

	if code := dt.run("factory", "install", "--drain"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	actContains(t, dt.stdout.String(), "waiting for the factory daemon to hold the drain", "drained after 15s")
	if sleeps != 3 || reloads(dt) != 1 {
		t.Fatalf("%d sleeps, %d bootstraps: want the reload right after the daemon held the drain", sleeps, reloads(dt))
	}
}

// Work still in flight after --timeout lifts the drain and reloads nothing.
func TestFactoryInstallDrainGivesUpAndLiftsTheDrain(t *testing.T) {
	dt, _ := factoryInstallTest(t)
	l := dt.ctx.Layout
	writeFactoryWork(t, l, factory.WorkRecord{PID: 250, Work: []factory.WorkItem{factoryRound}})

	if code := dt.run("factory", "install", "--drain", "--timeout", "10s"); code != 1 {
		t.Fatalf("exit %d, want 1; stderr %s", code, dt.stderr)
	}
	actContains(t, dt.stderr.String(), "1 local review round(s) still in flight after 10s", "the drain is lifted",
		"the factory daemon was not restarted", "--now")
	if v, err := factory.ReadDrain(l); err != nil || v != "" {
		t.Fatalf("drain.json holds %q (%v) after giving up, want it lifted", v, err)
	}
	if reloads(dt) != 0 {
		t.Fatal("the job was reloaded after the drain gave up")
	}
}

// ctrl+c during the wait lifts the drain and reloads nothing.
func TestFactoryInstallDrainInterruptedLiftsTheDrain(t *testing.T) {
	dt, _ := factoryInstallTest(t)
	l := dt.ctx.Layout
	writeFactoryWork(t, l, factory.WorkRecord{PID: 250, Work: []factory.WorkItem{factoryRound}})
	daemonSys.Sleep = func(context.Context, time.Duration) error { return context.Canceled }

	if code := dt.run("factory", "install", "--drain"); code != 1 {
		t.Fatalf("exit %d, want 1; stderr %s", code, dt.stderr)
	}
	actContains(t, dt.stderr.String(), "interrupted after 0s; the drain is lifted and the factory daemon was not restarted")
	if v, err := factory.ReadDrain(l); err != nil || v != "" {
		t.Fatalf("drain.json holds %q (%v) after the interrupt, want it lifted", v, err)
	}
	if reloads(dt) != 0 {
		t.Fatal("the job was reloaded after the interrupt")
	}
}

// A factory daemon that started again during the wait (launchd after a
// crash) ended the drain as the one it was waited for: --drain asks for it
// again, and a daemon that stopped meanwhile has nothing left to cut.
func TestFactoryInstallDrainAsksAgainForADrainARestartEnded(t *testing.T) {
	dt, _ := factoryInstallTest(t)
	l := dt.ctx.Layout
	writeFactoryWork(t, l, factory.WorkRecord{PID: 250, Work: []factory.WorkItem{factoryRound}})
	sleeps := 0
	var asked []string
	daemonSys.Sleep = func(context.Context, time.Duration) error {
		sleeps++
		v, _ := factory.ReadDrain(l)
		asked = append(asked, v)
		switch sleeps {
		case 1: // the daemon restarted: it ended the drain and runs the round again
			daemonSys.FactoryPID = func(paths.Layout) (int, error) { return 251, nil }
			writeFactoryWork(t, l, factory.WorkRecord{PID: 251, Work: []factory.WorkItem{factoryRound}})
			if err := os.Remove(l.FactoryDrain()); err != nil {
				t.Error(err)
			}
		case 3: // and it stopped
			daemonSys.FactoryPID = func(paths.Layout) (int, error) { return 0, nil }
		}
		return nil
	}
	dt.fake.Rules = append([]execx.Rule{
		daemonRuleOK([]string{"ps", "-p", "251", "-o", "comm="}, "/opt/bin/magnum\n"),
		daemonRuleOK([]string{"ps", "-p", "251", "-o", "args="}, "/opt/bin/magnum factory daemon\n"),
	}, dt.fake.Rules...)

	if code := dt.run("factory", "install", "--drain"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	if len(asked) < 3 || asked[1] == "" || asked[1] != asked[0] {
		t.Fatalf("the drains seen at each sleep %q: want the one the restart ended asked for again", asked)
	}
	actContains(t, dt.stdout.String(), "the factory daemon stopped during the drain (after 15s): nothing in flight")
	if reloads(dt) != 1 {
		t.Fatalf("bootstrap calls %d, want the job loaded once", reloads(dt))
	}
}

func TestFactoryInstallFlagConflicts(t *testing.T) {
	dt, _ := factoryInstallTest(t)
	for _, args := range [][]string{{"--now", "--drain"}, {"--drain", "--timeout", "0s"}} {
		if code := dt.run("factory", append([]string{"install"}, args...)...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	actContains(t, dt.stderr.String(), "magnum factory install: use either --now or --drain",
		"usage: magnum factory install [--now | --drain [--timeout D]] [--dry-run]")
}

// --drain reloads only once the daemon holds its own drain: a record that
// names another command's drain (one this command replaced, or a stale
// record) proves nothing about this one.
func TestFactoryInstallDrainWaitsForItsOwnDrain(t *testing.T) {
	dt, _ := factoryInstallTest(t)
	l := dt.ctx.Layout
	other := factory.Drain{Since: time.Now().Add(-time.Hour), PID: 4100}.Value()
	writeFactoryWork(t, l, factory.WorkRecord{PID: 250, Drain: other, Work: []factory.WorkItem{}})
	sleeps := 0
	daemonSys.Sleep = func(context.Context, time.Duration) error {
		sleeps++
		if sleeps == 3 {
			v, _ := factory.ReadDrain(l)
			writeFactoryWork(t, l, factory.WorkRecord{PID: 250, Drain: v, Work: []factory.WorkItem{}})
		} else if reloads(dt) != 0 {
			t.Errorf("sleep %d: the job was reloaded on another command's drain", sleeps)
		}
		return nil
	}

	if code := dt.run("factory", "install", "--drain"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	if sleeps != 3 || reloads(dt) != 1 {
		t.Fatalf("%d sleeps, %d bootstraps: want the reload once the daemon held this command's drain", sleeps, reloads(dt))
	}
}

// The build is checked again after the drain's wait, which can last hours
// while the checkout is rebuilt: a build that now rejects the configuration
// is refused, and the drain is lifted.
func TestFactoryInstallDrainChecksTheBuildAgainAfterTheWait(t *testing.T) {
	dt, _ := factoryInstallTest(t)
	l := dt.ctx.Layout
	checks := 0
	dt.fake.Rules = append([]execx.Rule{{Prefix: []string{l.Binary(), "config"}, Fn: func(c execx.Cmd) (execx.Result, error) {
		checks++
		if checks == 1 {
			return execx.Result{}, nil
		}
		res := execx.Result{Code: 1, Stderr: []byte("config: unknown keys: [factory.no_such_key]\n")}
		return res, &execx.ExitError{Cmd: c, Code: 1, Stderr: string(res.Stderr)}
	}}}, dt.fake.Rules...)
	writeFactoryWork(t, l, factory.WorkRecord{PID: 250, Work: []factory.WorkItem{}})
	daemonSys.Sleep = func(context.Context, time.Duration) error {
		v, _ := factory.ReadDrain(l)
		writeFactoryWork(t, l, factory.WorkRecord{PID: 250, Drain: v, Work: []factory.WorkItem{}})
		return nil
	}

	if code := dt.run("factory", "install", "--drain"); code != 1 {
		t.Fatalf("exit %d, want 1; stdout %s stderr %s", code, dt.stdout, dt.stderr)
	}
	actContains(t, dt.stderr.String(), "rejects the configuration", "factory.no_such_key")
	if checks != 2 || reloads(dt) != 0 {
		t.Fatalf("%d build checks, %d bootstraps: want the build checked again after the wait, and nothing reloaded", checks, reloads(dt))
	}
	if v, err := factory.ReadDrain(l); err != nil || v != "" {
		t.Fatalf("drain.json holds %q (%v), want it lifted", v, err)
	}
}

// `magnum factory status` shows the holds of the running factory daemon,
// the install drain among them, and the work a restart would cut.
func TestFactoryStatusShowsTheDrainAndTheWorkInFlight(t *testing.T) {
	dt, _ := factoryInstallTest(t)
	l := dt.ctx.Layout
	writeFactoryWork(t, l, factory.WorkRecord{PID: 250, Drain: "x", Work: []factory.WorkItem{factoryRound}})
	since := time.Now().Add(-10 * time.Minute)
	b, err := json.Marshal([]factory.Hold{{Kind: factory.HoldDrain, Reason: "`magnum factory install --drain` (pid 4242): nothing new starts until the restart",
		Since: since}})
	if err != nil {
		t.Fatal(err)
	}
	if err := fsx.WriteFileAtomic(l.FactoryHolds(), b, 0o600); err != nil {
		t.Fatal(err)
	}

	if code := dt.run("factory", "status"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	actContains(t, dt.stdout.String(), "factory daemon: running (pid 250)",
		"held: drain: `magnum factory install --drain` (pid 4242): nothing new starts until the restart (since "+inspClock(time.Now(), since)+")",
		"in flight: local review round 2 of PR-12340 (since "+inspClock(time.Now(), factoryRound.Since)+")")

	dt.stdout.Reset()
	if code := dt.run("factory", "status", "--json"); code != 0 {
		t.Fatalf("--json: exit %d, stderr %s", code, dt.stderr)
	}
	var got struct {
		Daemon struct {
			Holds []struct{ Kind, Reason string } `json:"holds"`
			Work  []struct{ Kind, What string }   `json:"work"`
		} `json:"daemon"`
	}
	if err := json.Unmarshal(dt.stdout.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, dt.stdout)
	}
	if len(got.Daemon.Holds) != 1 || got.Daemon.Holds[0].Kind != "drain" || len(got.Daemon.Work) != 1 ||
		!strings.HasPrefix(got.Daemon.Work[0].What, "local review round 2") {
		t.Fatalf("daemon %+v, want the drain's hold and the round", got.Daemon)
	}
}
