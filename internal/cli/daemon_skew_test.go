package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/launchd"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// recordBuild stores the build a daemon records at its start.
func recordBuild(t *testing.T, st *store.Store, b engine.Build) {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(context.Background(), engine.KVDaemonBuild, string(raw)); err != nil {
		t.Fatal(err)
	}
}

// The screens and the CLI used to say nothing while the daemon ran an older
// build than the one just built: a request's reply warns.
func TestRequestReplyWarnsWhenTheDaemonRunsAnOlderBuild(t *testing.T) {
	h := newActHarness(t)
	h.c.Version, h.d.Version = "v0.9.0", "v0.9.0"
	h.pid = 4242
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	recordBuild(t, h.st, engine.Build{Version: "v0.8.0", StartedAt: h.now.Add(-time.Hour)})
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestDone, "muted talkable/talkable#5") }
	if code := h.cmd("mute", "5"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.errb.String(), "warning: daemon runs v0.8.0 since ", "; v0.9.0 is built: `magnum daemon-restart`")
	actContains(t, h.out.String(), "muted talkable/talkable#5")

	h.out.Reset()
	h.errb.Reset()
	recordBuild(t, h.st, engine.Build{Version: "v0.9.0", StartedAt: h.now.Add(-time.Hour)})
	if code := h.cmd("unmute", "5"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if strings.Contains(h.errb.String(), "warning:") {
		t.Fatalf("same build warned: %s", h.errb.String())
	}
}

// The daemon handles requests before its GitHub poll, yet an answer may take
// a whole poll (12 s and more): the CLI and the screens wait 30 s for it.
func TestQuickWaitOutlastsTheDaemonsPoll(t *testing.T) {
	c := &Context{Version: "v1", Layout: paths.Layout{Home: t.TempDir()}, Config: config.Defaults(), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	d, err := actNewDeps(c, actLight)
	if err != nil {
		t.Fatal(err)
	}
	if d.Quick != 30*time.Second || d.Running == nil || d.Version != "v1" {
		t.Fatalf("Quick %s Running set %v Version %q", d.Quick, d.Running != nil, d.Version)
	}

	h := newActHarness(t)
	h.pid = 4242
	h.d.Quick = reqQuickWait
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	start := h.now
	h.onSleep = func(h *actHarness) {
		if h.now.Sub(start) >= 15*time.Second {
			h.completePending(store.RequestDone, "pinned talkable/talkable#5")
		}
	}
	if code := h.cmd("pin", "5"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "pinned talkable/talkable#5")
}

// status names the skew, and the drain's owner with how to lift it.
func TestStatusShowsBuildSkewAndTheDrainer(t *testing.T) {
	_, st, d, now := statusFixture(t)
	ctx := context.Background()
	bin := filepath.Join(t.TempDir(), "magnum")
	if err := os.WriteFile(bin, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	b := engine.CurrentBuild("v1.0.0", bin)
	b.StartedAt = now.Add(-3 * time.Hour)
	recordBuild(t, st, b)
	rebuilt := b.ModTime.Add(time.Hour)
	if err := os.Chtimes(bin, rebuilt, rebuilt); err != nil {
		t.Fatal(err)
	}
	d.Version = "v1.0.0"
	if err := st.SetKV(ctx, engine.KVDaemonDraining, engine.Drain{Since: now.Add(-time.Minute), PID: 31337}.Value()); err != nil {
		t.Fatal(err)
	}
	d.DrainerAlive = func(pid int) bool { return pid == 31337 }
	r, err := statusGather(ctx, d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Daemon.DrainerPID != 31337 || r.Daemon.Build == nil || !strings.Contains(r.Daemon.Skew, "a new build is on disk ("+bin) {
		t.Fatalf("daemon = %+v", r.Daemon)
	}
	var out bytes.Buffer
	statusRender(&out, r)
	actContains(t, out.String(), "build:    daemon runs v1.0.0 since", "`magnum daemon-restart`",
		"daemon: draining for a restart since", "(drained by pid 31337)", "ctrl+c in its terminal, or `kill 31337`")
	if dash := statusDashData(r, "talkable/talkable"); dash.Daemon.Skew == "" {
		t.Fatal("the dashboard data lacks the skew")
	}

	d.DrainerAlive = func(int) bool { return false }
	if r, err = statusGather(ctx, d, statusOptions{}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	statusRender(&out, r)
	actContains(t, out.String(), "(pid 31337) is gone: the daemon lifts the drain on its next tick", "`magnum kick` lifts it now")
}

// Closing the terminal of `daemon-restart --drain` (SIGHUP) killed it before
// it lifted the drain; the signal now ends the context like ctrl+c.
func TestSignalContextEndsOnSIGHUP(t *testing.T) {
	ctx, stop := signalContext()
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGHUP did not end the context")
	}
}

// A refused restart names the ways to wait for the rounds.
func TestRefusedRestartNamesDrainAndWhenIdle(t *testing.T) {
	dt := newDaemonGroupTest(t, launchctlPrintSequence(launchctlPrint("running", 100)))
	daemonSys.DaemonPID = func(paths.Layout) (int, error) { return 100, nil }
	seedRoundInFlight(t, dt)
	if code := dt.run("daemon-restart"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, dt.stderr.String(), "1 review round(s) in flight", "`magnum daemon-restart --when-idle`", "--drain", "--now")
}

// --when-idle waits, holding no drain, until no round runs, then restarts;
// a round that starts in between makes it wait again.
func TestDaemonRestartWhenIdleWaitsWithoutADrain(t *testing.T) {
	dt := newDaemonGroupTest(t,
		launchctlPrintSequence(launchctlPrint("running", 100), launchctlPrint("running", 100), launchctlPrint("running", 200)),
		daemonRuleOK([]string{"launchctl", "kickstart", "-k", "gui/501/zhuravel.magnum"}, ""),
	)
	pidReads := 0
	daemonSys.DaemonPID = func(paths.Layout) (int, error) {
		pidReads++
		if pidReads == 1 { // read after the first wait: a round starts in between
			storetest.Seed(t, dt.ctx.Layout.DB())
			st, err := store.Open(dt.ctx.Layout.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			inspSeedPR(t, st, "talkable/talkable", 43, store.PRReviewing, nil)
		}
		return 100, nil
	}
	seedRoundInFlight(t, dt)
	sleeps := 0
	daemonSys.Sleep = func(context.Context, time.Duration) error {
		sleeps++
		if _, ok := draining(t, dt); ok {
			t.Errorf("sleep %d: --when-idle drained", sleeps)
		}
		switch sleeps {
		case 2:
			finishRound(t, dt)
		case 3:
			storetest.Seed(t, dt.ctx.Layout.DB())
			st, err := store.Open(dt.ctx.Layout.DB())
			if err != nil {
				t.Fatal(err)
			}
			_, pr := inspSeedPR(t, st, "talkable/talkable", 43, store.PRReviewing, nil)
			if err := st.TransitionPR(context.Background(), pr.ID, []string{store.PRReviewing}, store.PRReviewed, nil); err != nil {
				t.Fatal(err)
			}
			st.Close()
		}
		return nil
	}
	if code := dt.run("daemon-restart", "--when-idle"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	actContains(t, dt.stdout.String(), "waiting for 1 round(s) in flight to end", "talkable/talkable#42 (reviewing)",
		"a round started in between (talkable/talkable#43 (reviewing)); waiting again", "restarted zhuravel.magnum")
	if n := len(dt.fake.CallsWithPrefix("launchctl", "kickstart")); n != 1 {
		t.Fatalf("kickstart calls = %d", n)
	}
	if got := drainEvents(t, dt); len(got) != 0 {
		t.Fatalf("drain events = %v", got)
	}
}

func TestDaemonRestartWhenIdleGivesUpAfterItsTimeout(t *testing.T) {
	dt := newDaemonGroupTest(t,
		launchctlPrintSequence(launchctlPrint("running", 100)),
		daemonRuleOK([]string{"launchctl", "kickstart"}, ""),
	)
	daemonSys.DaemonPID = func(paths.Layout) (int, error) { return 100, nil }
	seedRoundInFlight(t, dt)
	if code := dt.run("daemon-restart", "--when-idle", "--timeout", "10s"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	actContains(t, dt.stderr.String(), "1 round(s) still in flight after 10s", "the daemon was not restarted", "--drain", "--now")
	if n := len(dt.fake.CallsWithPrefix("launchctl", "kickstart")); n != 0 {
		t.Fatalf("kickstart calls = %d", n)
	}
	for _, args := range [][]string{{"--when-idle", "--now"}, {"--when-idle", "--drain"}} {
		if code := dt.run("daemon-restart", args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

// The daemon's engine options: its build, launchd (XPC_SERVICE_NAME) and the
// check a new build passes before the daemon restarts on it.
func TestDaemonEngineOptionsCheckANewBuild(t *testing.T) {
	dt := newDaemonGroupTest(t,
		daemonRuleOK([]string{"/new/magnum", "version"}, "magnum v2.0.0\n"),
		daemonRuleExit([]string{"/new/magnum", "config"}, 1, "config: unknown keys: [daemon.nope]\n"),
	)
	env := map[string]string{"XPC_SERVICE_NAME": launchd.DefaultLabel}
	daemonSys.Getenv = func(k string) string { return env[k] }
	dt.ctx.Version = "v1.0.0"
	o := daemonEngineOptions(dt.ctx, daemonOptions{})
	if !o.Supervised || o.Build.Version != "v1.0.0" || o.CheckBuild == nil {
		t.Fatalf("options = %+v", o)
	}
	v, err := o.CheckBuild(context.Background(), "/new/magnum")
	if v != "v2.0.0" || err == nil || !strings.Contains(err.Error(), "unknown keys: [daemon.nope]") {
		t.Fatalf("check = %q %v", v, err)
	}
	env = map[string]string{"XPC_SERVICE_NAME": "0"}
	if o := daemonEngineOptions(dt.ctx, daemonOptions{DryRun: true}); o.Supervised || o.CheckBuild != nil {
		t.Fatalf("a terminal's dry run: %+v", o)
	}
}
