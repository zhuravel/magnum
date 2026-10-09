package cli

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

func TestDaemonParsesFlagsAndPrintsDryRunPlan(t *testing.T) {
	dt := newDaemonGroupTest(t)
	var got daemonOptions
	daemonSys.RunEngine = func(_ context.Context, c *Context, o daemonOptions) (daemonDryRunReport, error) {
		if c != dt.ctx {
			t.Fatal("RunEngine got another context")
		}
		got = o
		return daemonDryRunReport{
			Ops: []engine.PlannedOp{{
				At:      time.Date(2026, 10, 3, 9, 30, 0, 0, time.UTC),
				Subject: "pr:talkable/talkable#11932",
				Action:  "claim_slot",
				Detail:  "review3",
			}},
			Commands: []string{"git -C /x fetch origin refs/pull/11932/head"},
		}, nil
	}
	if code := dt.run("daemon", "--once", "--dry-run", "--log-level", "debug"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if !got.Once || !got.DryRun || got.JSON || got.LogLevel != slog.LevelDebug {
		t.Fatalf("options = %+v", got)
	}
	out := dt.stdout.String()
	for _, want := range []string{"1 planned action", "1 command", "SUBJECT", "ACTION", "pr:talkable/talkable#11932", "claim_slot", "review3", "git -C /x fetch origin refs/pull/11932/head", "nothing was changed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestDaemonDryRunJSON(t *testing.T) {
	dt := newDaemonGroupTest(t)
	daemonSys.RunEngine = func(context.Context, *Context, daemonOptions) (daemonDryRunReport, error) {
		return daemonDryRunReport{}, nil
	}
	if code := dt.run("daemon", "--dry-run", "--json"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	var rep struct {
		Ops      []engine.PlannedOp `json:"ops"`
		Commands []string           `json:"commands"`
	}
	if err := json.Unmarshal(dt.stdout.Bytes(), &rep); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, dt.stdout)
	}
	if rep.Ops == nil || rep.Commands == nil {
		t.Fatalf("empty plan must render as [] not null: %s", dt.stdout)
	}
}

func TestDaemonForegroundPrintsNothingOnCleanExit(t *testing.T) {
	dt := newDaemonGroupTest(t)
	var got daemonOptions
	daemonSys.RunEngine = func(_ context.Context, _ *Context, o daemonOptions) (daemonDryRunReport, error) {
		got = o
		return daemonDryRunReport{}, nil // SIGTERM and "already running" both return nil
	}
	if code := dt.run("daemon"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got.Once || got.DryRun || got.LogLevel != slog.LevelInfo {
		t.Fatalf("defaults = %+v", got)
	}
	if dt.stdout.Len() != 0 {
		t.Fatalf("unexpected stdout: %s", dt.stdout)
	}
}

func TestDaemonEngineErrorExitsNonZero(t *testing.T) {
	dt := newDaemonGroupTest(t)
	daemonSys.RunEngine = func(context.Context, *Context, daemonOptions) (daemonDryRunReport, error) {
		return daemonDryRunReport{}, errors.New("engine: lock: permission denied")
	}
	if code := dt.run("daemon"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(dt.stderr.String(), "engine: lock: permission denied") {
		t.Fatalf("stderr: %s", dt.stderr)
	}
}

func TestDaemonUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"--log-level", "loud"},
		{"--json"},
		{"extra"},
		{"--bogus"},
	} {
		dt := newDaemonGroupTest(t)
		if code := dt.run("daemon", args...); code != 2 {
			t.Errorf("daemon %v: exit %d, want 2 (stderr %s)", args, code, dt.stderr)
		}
	}
	dt := newDaemonGroupTest(t)
	dt.run("daemon", "--log-level", "loud")
	if !strings.Contains(dt.stderr.String(), "debug, info, warn or error") {
		t.Fatalf("log-level error lacks the fix: %s", dt.stderr)
	}
}

func TestDaemonConfigErrorNamesTheFix(t *testing.T) {
	dt := newDaemonGroupTest(t)
	dt.ctx.Config = nil
	dt.ctx.cfgPath = dt.ctx.Layout.Home + "/missing.toml"
	if code := dt.run("daemon"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(dt.stderr.String(), "magnum config") {
		t.Fatalf("stderr lacks the fix: %s", dt.stderr)
	}
}

// Only the daemon rotates daemon.log: its App says so, and the other
// commands' Apps (act, inspect) do not.
func TestOnlyTheDaemonOwnsLogRotation(t *testing.T) {
	c, _, _ := bareContext(t)
	if o := daemonAppOptions(c, daemonOptions{LogLevel: slog.LevelDebug}); !o.Daemon || o.LogLevel != slog.LevelDebug || o.Stderr != c.Stderr {
		t.Fatalf("daemon app options %+v", o)
	}
	f := newInspFixture(t)
	storetest.Seed(t, f.Ctx.Layout.DB())
	var seen []app.Options
	prev := inspAppHook
	inspAppHook = func(o *app.Options) { prev(o); seen = append(seen, *o) }
	t.Cleanup(func() { inspAppHook = prev })
	a, err := inspOpenApp(f.Ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	if len(seen) != 1 || seen[0].Daemon {
		t.Fatalf("inspect app options %+v", seen)
	}
}
