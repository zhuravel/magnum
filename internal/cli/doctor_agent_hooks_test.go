package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/paths"
)

// installHookFiles writes what `magnum hooks install` writes for t: the
// script, and the settings with every entry; with only, the entries of
// those events alone.
func installHookFiles(t *testing.T, target hookTarget, only ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(target.script()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target.script(), target.scriptBody(), 0o755); err != nil {
		t.Fatal(err)
	}
	entries := target.entries()
	if len(only) > 0 {
		var some []hookEntry
		for _, e := range entries {
			for _, o := range only {
				if e.Event == o {
					some = append(some, e)
				}
			}
		}
		entries = some
	}
	doc, _, err := spliceHooks(nil, target.agent, entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target.settings(), doc, 0o600); err != nil {
		t.Fatal(err)
	}
}

// doctorHookCheck runs the agent hook checks and returns the one named
// name.
func doctorHookCheck(t *testing.T, d doctorDeps, name string) doctorCheck {
	t.Helper()
	var out []doctorCheck
	for _, c := range doctorAgentHooks(context.Background(), d) {
		if c.Name == name {
			out = append(out, c)
		}
	}
	if len(out) != 1 {
		t.Fatalf("%s checks = %+v, want one", name, out)
	}
	return out[0]
}

// Doctor warns while magnum's Claude hooks are missing, outdated, modified
// or incomplete, with the install as the fix, and passes when they are
// current.
func TestDoctorWarnsUntilMagnumsClaudeHooksAreCurrent(t *testing.T) {
	_, d, _, _ := doctorFixture(t)
	target := hookTarget{agent: agenthooks.AgentClaude, dir: filepath.Join(d.UserHome, ".claude")}
	const fix = "magnum hooks install claude"
	if c := doctorHookCheck(t, d, "claude hooks"); c.Status != doctorWarn || c.Fix != fix || !strings.Contains(c.Detail, "not installed") ||
		!strings.Contains(c.Detail, "observe passes") {
		t.Fatalf("missing = %s %q fix %q", c.Status, c.Detail, c.Fix)
	}
	installHookFiles(t, target)
	if c := doctorHookCheck(t, d, "claude hooks"); c.Status != doctorPass || !strings.Contains(c.Detail, "current (v1)") ||
		!strings.Contains(c.Detail, target.settings()) {
		t.Fatalf("current = %s %q", c.Status, c.Detail)
	}
	script := string(target.scriptBody())
	if err := os.WriteFile(target.script(), []byte(strings.Replace(script, "MAGNUM_HOOK_VERSION=1", "MAGNUM_HOOK_VERSION=0", 1)), 0o755); err != nil {
		t.Fatal(err)
	}
	if c := doctorHookCheck(t, d, "claude hooks"); c.Status != doctorWarn || c.Fix != fix || !strings.Contains(c.Detail, "outdated (v0 < v1)") {
		t.Fatalf("outdated = %s %q fix %q", c.Status, c.Detail, c.Fix)
	}
	if err := os.WriteFile(target.script(), []byte("#!/bin/sh\necho mine\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if c := doctorHookCheck(t, d, "claude hooks"); c.Status != doctorWarn || !strings.Contains(c.Detail, "modified") ||
		!strings.Contains(c.Fix, "move "+target.script()+" away") || !strings.Contains(c.Fix, fix) {
		t.Fatalf("modified = %s %q fix %q", c.Status, c.Detail, c.Fix)
	}
	newer := strings.Replace(script, "MAGNUM_HOOK_VERSION=1", "MAGNUM_HOOK_VERSION=2", 1)
	if err := os.WriteFile(target.script(), []byte(newer), 0o755); err != nil {
		t.Fatal(err)
	}
	if c := doctorHookCheck(t, d, "claude hooks"); c.Status != doctorWarn || !strings.Contains(c.Detail, "a newer magnum's (v2 > v1)") ||
		c.Fix != "run the newer magnum's `magnum hooks install claude`" {
		t.Fatalf("newer = %s %q fix %q", c.Status, c.Detail, c.Fix)
	}
	installHookFiles(t, target, "Stop", "SessionStart")
	if c := doctorHookCheck(t, d, "claude hooks"); c.Status != doctorWarn || c.Fix != fix || !strings.Contains(c.Detail, "incomplete") ||
		!strings.Contains(c.Detail, "StopFailure, PermissionRequest, SessionEnd") {
		t.Fatalf("incomplete = %s %q fix %q", c.Status, c.Detail, c.Fix)
	}
}

// Doctor checks magnum's Codex hooks in each Codex home the Codex roles
// read: missing gives the install as the fix, an entry without a trust
// entry gives Codex's one-time Trust all, naming the home's files, trusted
// passes, and an unknown home is skipped.
func TestDoctorChecksMagnumsCodexHooksPerHome(t *testing.T) {
	_, d, _, ag := doctorFixture(t)
	home := t.TempDir()
	ag.codexHome = home
	target := hookTarget{agent: agenthooks.AgentCodex, dir: home}
	if c := doctorHookCheck(t, d, "codex hooks"); c.Status != doctorWarn || c.Fix != "magnum hooks install codex" || !strings.Contains(c.Detail, "not installed") ||
		!strings.Contains(c.Detail, "codex-judge") {
		t.Fatalf("missing = %s %q fix %q", c.Status, c.Detail, c.Fix)
	}
	installHookFiles(t, target)
	c := doctorHookCheck(t, d, "codex hooks")
	if c.Status != doctorWarn || !strings.Contains(c.Detail, target.settings()) || !strings.Contains(c.Detail, "no trust entry for stop:0:0") ||
		!strings.Contains(c.Detail, "new Codex sessions wait") || c.Fix != codexTrustStep+": Codex asks it once per Codex home" {
		t.Fatalf("untrusted = %s %q fix %q", c.Status, c.Detail, c.Fix)
	}
	var state strings.Builder
	for _, e := range codexHookEvents {
		state.WriteString("[hooks.state.\"" + target.settings() + ":" + e.arg + ":0:0\"]\ntrusted_hash = \"sha256:1\"\n")
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(state.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := doctorHookCheck(t, d, "codex hooks"); c.Status != doctorPass || !strings.Contains(c.Detail, "current (v1)") {
		t.Fatalf("trusted = %s %q", c.Status, c.Detail)
	}
	ag.codexHome = ""
	if c := doctorHookCheck(t, d, "codex hooks"); c.Status != doctorSkip || !strings.Contains(c.Detail, "unknown") {
		t.Fatalf("unknown home = %s %q", c.Status, c.Detail)
	}
}

// socketChecks runs the hook socket check with the review and the factory
// daemon running or not.
func socketChecks(t *testing.T, l paths.Layout, review, factory bool) map[string]doctorCheck {
	t.Helper()
	pid := func(running bool) func() (int, error) {
		return func() (int, error) {
			if running {
				return 4242, nil
			}
			return 0, nil
		}
	}
	d := doctorDeps{Layout: l, DaemonPID: pid(review), FactoryPID: pid(factory)}
	out := map[string]doctorCheck{}
	for _, c := range doctorHookSocket(context.Background(), d) {
		if c.Name != "hook socket" {
			t.Fatalf("check %q", c.Name)
		}
		switch {
		case strings.Contains(c.Detail, "factory daemon"):
			out["factory"] = c
		case strings.Contains(c.Detail, "review daemon"):
			out["review"] = c
		}
	}
	if len(out) != 2 {
		t.Fatalf("socket checks = %+v, want one per daemon", out)
	}
	return out
}

// The hook socket check sends each daemon's socket a ping datagram (which
// the listeners drop): one that takes it passes; a missing or deaf socket
// warns while its daemon runs (the daemon then listens for no hooks and
// polls), and is skipped while it does not.
func TestDoctorChecksEachDaemonsHookSocket(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("", "dh") //nolint:usetesting // t.TempDir's path is too long for a Unix socket's
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l := paths.Layout{Home: dir, StateDir: dir}

	cs := socketChecks(t, l, false, false)
	if cs["review"].Status != doctorSkip || cs["factory"].Status != doctorSkip {
		t.Fatalf("stopped daemons = %+v", cs)
	}
	cs = socketChecks(t, l, true, true)
	if c := cs["review"]; c.Status != doctorWarn || !strings.Contains(c.Detail, "polling only") || !strings.Contains(c.Fix, "magnum daemon-restart --drain") {
		t.Fatalf("no review socket = %+v", c)
	}
	if c := cs["factory"]; c.Status != doctorWarn || !strings.Contains(c.Detail, "polling only") || !strings.Contains(c.Fix, "magnum factory install --drain") {
		t.Fatalf("no factory socket = %+v", c)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := agenthooks.Listen(l.HookSocket(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan agenthooks.Event, 4)
	done := make(chan struct{})
	go func() { defer close(done); ln.Serve(ctx, func(ev agenthooks.Event) { got <- ev }) }()
	cs = socketChecks(t, l, true, false)
	if c := cs["review"]; c.Status != doctorPass || !strings.Contains(c.Detail, "accepts events") || !strings.Contains(c.Detail, l.HookSocket()) {
		t.Fatalf("listening = %+v", c)
	}
	cancel()
	<-done
	select {
	case ev := <-got:
		t.Fatalf("the listener handed the ping on: %+v", ev)
	default:
	}
	// The socket file stays after its listener closed: deaf.
	if _, err := os.Stat(l.HookSocket()); errors.Is(err, os.ErrNotExist) {
		t.Fatal("the closed listener removed its socket")
	}
	if c := socketChecks(t, l, true, false)["review"]; c.Status != doctorWarn || !strings.Contains(c.Detail, "does not accept events") {
		t.Fatalf("deaf socket, daemon running = %+v", c)
	}
	if c := socketChecks(t, l, false, false)["review"]; c.Status != doctorSkip {
		t.Fatalf("deaf socket, daemon stopped = %+v", c)
	}
}

// While magnum's rewritten Codex entry keeps the trusted_hash of the hook
// before the rewrite, doctor warns that Codex will ask to trust it, and
// passes once the operator's Trust all gave the place a new hash.
func TestDoctorWarnsWhileARewrittenCodexEntryKeepsItsOldTrust(t *testing.T) {
	_, d, _, ag := doctorFixture(t)
	agents.SetStaleCodexTrustDir(filepath.Join(t.TempDir(), "codex-stale-trust"))
	t.Cleanup(func() { agents.SetStaleCodexTrustDir("") })
	home := t.TempDir()
	ag.codexHome = home
	target := hookTarget{agent: agenthooks.AgentCodex, dir: home}
	installHookFiles(t, target)
	trust := func(stop string) {
		t.Helper()
		var state strings.Builder
		for _, e := range codexHookEvents {
			h := "sha256:1"
			if e.arg == agenthooks.NameStop {
				h = stop
			}
			state.WriteString("[hooks.state.\"" + target.settings() + ":" + e.arg + ":0:0\"]\ntrusted_hash = \"" + h + "\"\n")
		}
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(state.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	trust("sha256:old")
	if err := agents.RecordStaleCodexTrust(home, []string{"stop:0:0"}); err != nil {
		t.Fatal(err)
	}
	if c := doctorHookCheck(t, d, "codex hooks"); c.Status != doctorWarn || !strings.Contains(c.Detail, "stop:0:0 (its trust is of the hook before magnum rewrote it)") ||
		!strings.Contains(c.Fix, codexForgetStep) {
		t.Fatalf("stale trust = %s %q fix %q", c.Status, c.Detail, c.Fix)
	}
	trust("sha256:new")
	if c := doctorHookCheck(t, d, "codex hooks"); c.Status != doctorPass {
		t.Fatalf("new trust = %s %q", c.Status, c.Detail)
	}
}

// Claude's directory comes from the shell's CLAUDE_CONFIG_DIR: when that is
// not an absolute path, doctor warns with the fix in the shell, not in a
// role's env.
func TestDoctorNamesTheShellForAClaudeDirectoryThatIsNotAbsolute(t *testing.T) {
	_, d, _, _ := doctorFixture(t)
	d.Getenv = func(k string) string {
		if k == "CLAUDE_CONFIG_DIR" {
			return "claude-alt"
		}
		return ""
	}
	if c := doctorHookCheck(t, d, "claude hooks"); c.Status != doctorWarn || !strings.Contains(c.Detail, `"claude-alt"`) ||
		c.Fix != "set CLAUDE_CONFIG_DIR in this shell to an absolute path, or unset it" {
		t.Fatalf("relative Claude directory = %s %q fix %q", c.Status, c.Detail, c.Fix)
	}
}

// A Codex hooks.json with a root key Codex does not know is one Codex
// skips: doctor warns with the key and the fix.
func TestDoctorWarnsForAHooksJSONCodexSkips(t *testing.T) {
	_, d, _, ag := doctorFixture(t)
	home := t.TempDir()
	ag.codexHome = home
	target := hookTarget{agent: agenthooks.AgentCodex, dir: home}
	installHookFiles(t, target)
	raw, err := os.ReadFile(target.settings())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target.settings(), []byte(strings.Replace(string(raw), "{", `{"x-note": 1,`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := doctorHookCheck(t, d, "codex hooks"); c.Status != doctorWarn || !strings.Contains(c.Detail, `Codex skips this file: unknown key "x-note"`) ||
		!strings.Contains(c.Fix, `remove the key "x-note" from `+target.settings()) {
		t.Fatalf("Codex skips = %s %q fix %q", c.Status, c.Detail, c.Fix)
	}
}

// The doctor pings the very sockets the daemons listen on: the review
// daemon's claim listens on Layout.HookSocket, and the factory's on
// Layout.FactoryHookSocket, so both lines pass while both daemons run.
func TestTheHookSocketCheckPassesAgainstTheDaemonsOwnListeners(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("", "dl") //nolint:usetesting // t.TempDir's path is too long for a Unix socket's
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l := paths.Layout{Home: dir, StateDir: dir}
	claim, err := engine.ClaimDaemon(l)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(claim.Release)
	if claim.HookSocket() != l.HookSocket() || claim.Off() != "" {
		t.Fatalf("the review daemon listens on %q (off %q), want %q", claim.HookSocket(), claim.Off(), l.HookSocket())
	}
	factory, err := agenthooks.Listen(l.FactoryHookSocket(), nil) // as the factory's listenHooks does
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = factory.Close() })
	cs := socketChecks(t, l, true, true)
	if cs["review"].Status != doctorPass || cs["factory"].Status != doctorPass {
		t.Fatalf("socket checks = %+v, want both PASS", cs)
	}
}
