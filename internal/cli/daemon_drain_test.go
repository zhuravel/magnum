package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// draining reads engine.KVDaemonDraining from the test registry.
func draining(t *testing.T, dt *daemonGroupTest) (string, bool) {
	t.Helper()
	st, err := store.Open(dt.ctx.Layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	v, ok, err := st.GetKV(context.Background(), engine.KVDaemonDraining)
	if err != nil {
		t.Fatal(err)
	}
	return v, ok
}

// finishRound moves talkable/talkable#42 from reviewing to reviewed.
func finishRound(t *testing.T, dt *daemonGroupTest) {
	t.Helper()
	st, err := store.Open(dt.ctx.Layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, pr := inspSeedPR(t, st, "talkable/talkable", 42, store.PRReviewing, nil)
	if err := st.TransitionPR(context.Background(), pr.ID, []string{store.PRReviewing}, store.PRReviewed, nil); err != nil {
		t.Fatal(err)
	}
}

// TestDaemonRestartRefusesWhenTheBuildRejectsTheConfig: bin/magnum (the
// build launchd starts) checks the configuration and prompts first; its
// error is shown and nothing is restarted.
func TestDaemonRestartRefusesWhenTheBuildRejectsTheConfig(t *testing.T) {
	dt := newDaemonGroupTest(t,
		launchctlPrintSequence(launchctlPrint("running", 100)),
		daemonRuleOK([]string{"launchctl", "kickstart"}, ""),
	)
	// Ahead of the harness's passing check.
	dt.fake.Rules = append([]execx.Rule{daemonRuleExit([]string{dt.ctx.Layout.Binary(), "config"}, 1,
		"config /x/config.toml: unknown keys: [daemon.no_such_key]\n")}, dt.fake.Rules...)
	dt.writeBinary(t)
	daemonSys.DaemonPID = func(paths.Layout) (int, error) { return 100, nil }
	if code := dt.run("daemon-restart", "--now"); code != 1 {
		t.Fatalf("exit %d, want 1; stderr %s", code, dt.stderr)
	}
	actContains(t, dt.stderr.String(), "rejects the configuration", "unknown keys: [daemon.no_such_key]", "config`, then retry")
	if n := len(dt.fake.CallsWithPrefix("launchctl", "kickstart")); n != 0 {
		t.Fatalf("kickstart calls = %d", n)
	}
	calls := dt.fake.CallsWithPrefix(dt.ctx.Layout.Binary(), "config")
	if len(calls) != 1 || calls[0].Env["MAGNUM_HOME"] != dt.ctx.Layout.Home || calls[0].Mutates {
		t.Fatalf("check calls = %+v", calls)
	}
}

// TestDaemonRestartDrainWaitsForRoundsThenRestarts: --drain stops new
// rounds, reports progress every 15s while one is in flight, restarts once
// none is, and lifts the drain afterwards.
func TestDaemonRestartDrainWaitsForRoundsThenRestarts(t *testing.T) {
	dt := newDaemonGroupTest(t,
		launchctlPrintSequence(launchctlPrint("running", 100), launchctlPrint("running", 100), launchctlPrint("running", 200)),
		daemonRuleOK([]string{"launchctl", "kickstart", "-k", "gui/501/zhuravel.magnum"}, ""),
	)
	daemonSys.DaemonPID = func(paths.Layout) (int, error) { return 100, nil }
	seedRoundInFlight(t, dt)
	sleeps := 0
	daemonSys.Sleep = func(context.Context, time.Duration) error {
		sleeps++
		v, ok := draining(t, dt)
		if dr, parsed := engine.ParseDrain(v); !ok || !parsed || dr.PID != os.Getpid() {
			t.Errorf("sleep %d: drain %q is not owned by this process", sleeps, v)
		}
		if sleeps == 4 {
			finishRound(t, dt)
		}
		return nil
	}
	if code := dt.run("daemon-restart", "--drain"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	out := dt.stdout.String()
	actContains(t, out, fmt.Sprintf("draining (pid %d): no new rounds start; waiting for 1 round(s) in flight (at most 2h0m0s): talkable/talkable#42 (reviewing)", os.Getpid()),
		"draining: 1 round(s) in flight after 15s", "drained after 20s", "restarted zhuravel.magnum")
	if n := len(dt.fake.CallsWithPrefix("launchctl", "kickstart")); n != 1 {
		t.Fatalf("kickstart calls = %d", n)
	}
	if _, ok := draining(t, dt); ok {
		t.Fatal("drain kept after the restart")
	}
	if got := drainEvents(t, dt); strings.Join(got, ",") != "daemon.drain_started,daemon.drain_ended" {
		t.Fatalf("drain events = %v", got)
	}
}

// drainEvents are the kinds of the drain events in the test registry.
func drainEvents(t *testing.T, dt *daemonGroupTest) []string {
	t.Helper()
	st, err := store.Open(dt.ctx.Layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows, err := st.DB().Query("SELECT kind FROM events WHERE kind LIKE 'daemon.drain%' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		out = append(out, k)
	}
	return out
}

// TestDaemonRestartDrainGivesUp: rounds still in flight after --timeout
// lift the drain and restart nothing.
func TestDaemonRestartDrainGivesUp(t *testing.T) {
	dt := newDaemonGroupTest(t,
		launchctlPrintSequence(launchctlPrint("running", 100)),
		daemonRuleOK([]string{"launchctl", "kickstart"}, ""),
	)
	daemonSys.DaemonPID = func(paths.Layout) (int, error) { return 100, nil }
	seedRoundInFlight(t, dt)
	if code := dt.run("daemon-restart", "--drain", "--timeout", "10s"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	actContains(t, dt.stderr.String(), "1 round(s) still in flight after 10s", "the drain is lifted", "--now")
	if n := len(dt.fake.CallsWithPrefix("launchctl", "kickstart")); n != 0 {
		t.Fatalf("kickstart calls = %d", n)
	}
	if _, ok := draining(t, dt); ok {
		t.Fatal("drain kept after giving up")
	}
}

func TestDaemonRestartFlagConflicts(t *testing.T) {
	dt := newDaemonGroupTest(t)
	for _, args := range [][]string{{"--now", "--drain"}, {"--drain", "--timeout", "0s"}} {
		if code := dt.run("daemon-restart", args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if code := dt.run("install", "--now", "--drain"); code != 2 {
		t.Errorf("install --now --drain: exit %d, want 2", code)
	}
}

// TestDaemonRefusesToStartOnAPromptThisBuildCannotRender: the daemon checks
// its configuration and prompts before it starts; the error goes to stderr
// (launchd.log) and daemon.log, and the exit is non-zero.
func TestDaemonRefusesToStartOnAPromptThisBuildCannotRender(t *testing.T) {
	dt := newDaemonGroupTest(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "judge-initial.md"), []byte("{{.FieldOfANewerBuild}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	dt.ctx.Config.Pipeline.PromptsDir = dir
	if code := dt.run("daemon"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	actContains(t, dt.stderr.String(), "magnum daemon: refusing to start", "FieldOfANewerBuild", filepath.Join(dir, "judge-initial.md"))
	b, err := os.ReadFile(dt.ctx.Layout.DaemonLog())
	if err != nil {
		t.Fatal(err)
	}
	actContains(t, string(b), `"msg":"magnum daemon refusing to start"`, "FieldOfANewerBuild")
}

// TestConfigCommandRendersEveryPrompt: `magnum config` reports the prompt
// renders and fails on a prompt this build cannot render.
func TestConfigCommandRendersEveryPrompt(t *testing.T) {
	f := newInspFixture(t)
	c := &Context{Version: "test", Layout: f.Ctx.Layout, Stdout: f.Out, Stderr: f.Err}
	if code := execute(c, []string{"config"}); code != 0 {
		t.Fatalf("exit %d: %s", code, f.Err.String())
	}
	if !strings.Contains(f.Out.String(), " renders ok\n") {
		t.Fatalf("stdout:\n%s", f.Out.String())
	}

	cfg := config.Defaults()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude-review.md"), []byte("{{if .NotesPath}}x{{else}}{{.Missing}}{{end}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Pipeline.PromptsDir = dir
	f.Out.Reset()
	f.Err.Reset()
	c = &Context{Version: "test", Layout: f.Ctx.Layout, Config: cfg, Stdout: f.Out, Stderr: f.Err}
	if code := execute(c, []string{"config"}); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	actContains(t, f.Err.String(), "prompts do not render with this build", "claude-review.md")
}
