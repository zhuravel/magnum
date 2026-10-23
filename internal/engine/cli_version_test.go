package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// fakeCLIs are the agent CLIs on the daemon's PATH: files in a temporary
// directory that the engine's LookPath finds, with what their --version
// prints, what `mise which <kind>` names and the tools `mise ls --current
// --json` lists, through a fake runner.
type fakeCLIs struct {
	t   *testing.T
	dir string
	run *execx.Fake

	mu     sync.Mutex
	out    map[string]string    // a command's path → what its --version prints
	which  map[string]string    // kind → the binary `mise which <kind>` names
	fails  map[string][]error   // a command's path → the errors its next --version reads fail with, in order
	listed map[string]miseEntry // tool → what `mise ls --current --json` says of it
}

// miseEntry is a tool as `mise ls --current --json` lists it.
type miseEntry struct {
	Version     string     `json:"version"`
	Requested   string     `json:"requested_version"`
	InstallPath string     `json:"install_path"`
	Source      miseSource `json:"source"`
	Installed   bool       `json:"installed"`
	Active      bool       `json:"active"`
}

type miseSource struct {
	Type string `json:"type"`
	Path string `json:"path"`
}

func newFakeCLIs(t *testing.T) *fakeCLIs {
	t.Helper()
	c := &fakeCLIs{t: t, dir: t.TempDir(), out: map[string]string{}, which: map[string]string{}, fails: map[string][]error{},
		listed: map[string]miseEntry{}}
	c.run = &execx.Fake{Rules: []execx.Rule{{Fn: c.answer}}}
	return c
}

// install is a harness mod: LookPath finds the directory's entries, and the
// runner answers for them.
func (c *fakeCLIs) install(h *harness) {
	h.d.LookPath = func(file string) (string, error) {
		p := filepath.Join(c.dir, file)
		if _, err := os.Lstat(p); err != nil {
			return "", exec.ErrNotFound
		}
		return p, nil
	}
	h.d.Runner = c.run
}

func (c *fakeCLIs) answer(cmd execx.Cmd) (execx.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case slices.Equal(cmd.Args, []string{"--version"}):
		if errs := c.fails[cmd.Name]; len(errs) > 0 {
			c.fails[cmd.Name] = errs[1:]
			return execx.Result{Code: -1}, &execx.RunError{Cmd: cmd, Err: errs[0]}
		}
		if out, ok := c.out[cmd.Name]; ok {
			return execx.Result{Stdout: []byte(out)}, nil
		}
	case len(cmd.Args) == 2 && cmd.Args[0] == "which":
		if p, ok := c.which[cmd.Args[1]]; ok {
			return execx.Result{Stdout: []byte(p + "\n")}, nil
		}
	case slices.Equal(cmd.Args, []string{"ls", "--current", "--json"}) && len(c.listed) > 0:
		list := map[string][]miseEntry{}
		for tool, e := range c.listed {
			list[tool] = []miseEntry{e}
		}
		out, err := json.Marshal(list)
		if err != nil {
			c.t.Error(err)
		}
		return execx.Result{Stdout: out}, nil
	}
	return execx.Result{Code: 1}, fmt.Errorf("fake CLIs: no answer for %s", cmd.String())
}

// lists makes `mise ls --current --json` list tool as mise's config
// requests it (an exact version pins it, "latest" floats), resolved to
// version and installed in installs/<tool>/<version> of the directory; the
// directory has a mise binary from then on, which LookPath finds.
func (c *fakeCLIs) lists(tool, requested, version string) {
	c.t.Helper()
	if _, err := os.Lstat(filepath.Join(c.dir, "mise")); err != nil {
		c.binary("mise", time.Date(2026, 10, 1, 9, 0, 0, 0, time.Local))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listed[tool] = miseEntry{Version: version, Requested: requested, Installed: true, Active: true,
		InstallPath: filepath.Join(c.dir, installDir(tool, version)), Source: miseSource{Type: "mise.toml", Path: filepath.Join(c.dir, ".config", "mise", "config.toml")}}
}

// installDir is where, in the directory, the fake mise installs version of
// tool.
func installDir(tool, version string) string {
	return filepath.Join("installs", strings.NewReplacer("/", "-", ":", "-", "@", "").Replace(tool), version)
}

// miseLists counts the `mise ls --current --json` commands the engine ran.
func (c *fakeCLIs) miseLists() int {
	n := 0
	for _, cmd := range c.run.CallsWithPrefix() {
		if slices.Equal(cmd.Args, []string{"ls", "--current", "--json"}) {
			n++
		}
	}
	return n
}

// binary writes a binary at rel, modified at mtime, and returns its path.
func (c *fakeCLIs) binary(rel string, mtime time.Time) string {
	c.t.Helper()
	p := filepath.Join(c.dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		c.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\n# "+rel+" "+mtime.String()+"\n"), 0o755); err != nil {
		c.t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		c.t.Fatal(err)
	}
	return p
}

// prints makes the command at path print out for --version.
func (c *fakeCLIs) prints(path, out string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.out[path] = out
}

// failsWith makes the next len(errs) --version reads of the command at path
// fail with errs, in order; the reads after them answer what prints says.
func (c *fakeCLIs) failsWith(path string, errs ...error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fails[path] = append(c.fails[path], errs...)
}

// names makes `mise which kind` name path.
func (c *fakeCLIs) names(kind, path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.which[kind] = path
}

// versionReads counts the --version commands the engine ran.
func (c *fakeCLIs) versionReads() int {
	n := 0
	for _, cmd := range c.run.CallsWithPrefix() {
		if slices.Equal(cmd.Args, []string{"--version"}) {
			n++
		}
	}
	return n
}

// versionReadsOf counts the --version commands the engine ran for the
// command at path, and reports whether every one had the health check's
// timeout (a read at the start included).
func (c *fakeCLIs) versionReadsOf(path string) (n int, timeouts bool) {
	timeouts = true
	for _, cmd := range c.run.CallsWithPrefix(path, "--version") {
		n++
		timeouts = timeouts && cmd.Timeout == cliVersionTimeout
	}
	return n, timeouts
}

// repeated is err n times.
func repeated(err error, n int) []error {
	return slices.Repeat([]error{err}, n)
}

// logLines counts the lines of logs at level that contain msg.
func logLines(logs *syncBuffer, level, msg string) int {
	n := 0
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if strings.Contains(line, "level="+level+" ") && strings.Contains(line, msg) {
			n++
		}
	}
	return n
}

// kindVersion is what the registry holds of kind's CLI version.
func kindVersion(h *harness, kind string) KindVersion {
	h.t.Helper()
	v, ok, err := readKVJSON[KindVersion](h.ctx, h.e, KVKindVersion(kind), "kind version")
	if err != nil || !ok {
		h.t.Fatalf("no version of %s recorded: %v", kind, err)
	}
	return v
}

// resolved is path with its links resolved, as the engine records it (a
// temporary directory on macOS sits behind /var → /private/var).
func resolved(t *testing.T, path string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// On 2026-10-16 mise's `codex = "latest"` installed codex-cli 0.163.0 at
// 11:24, which died at its TUI bootstrap in every judge pane, and nothing in
// magnum said the CLI had changed. The daemon reads each agent kind's
// version at its start and keeps it (kind.<name>.version) with the binary's
// path and mtime; a health check runs the version command again only when
// the binary changed, and a new version of a CLI mise pins is one warning
// and one toast, which later checks and a restart do not repeat.
func TestACLIVersionChangeIsRecordedOnceAndToasted(t *testing.T) {
	cli := newFakeCLIs(t)
	h := newHarness(t, cli.install)
	installed := time.Date(2026, 10, 9, 21, 20, 0, 0, time.Local)
	codex := cli.binary("codex", installed)
	cli.prints(codex, "codex-cli 0.162.1\n")
	cli.lists("codex", "0.162.1", "0.162.1")
	claude := cli.binary("claude", installed)
	cli.prints(claude, "2.1.290 (Claude Code)\n")

	h.startup()
	v := kindVersion(h, "codex")
	if v.Version != "codex-cli 0.162.1" || v.Path != resolved(t, codex) || !v.MTime.Equal(installed) || !v.SeenAt.Equal(h.clock.Now()) ||
		v.Previous != "" || !v.ChangedAt.IsZero() {
		t.Fatalf("codex at the start: %+v", v)
	}
	if v := kindVersion(h, "claude"); v.Version != "2.1.290 (Claude Code)" {
		t.Fatalf("claude at the start: %+v", v)
	}
	if n := cli.versionReads(); n != 2 {
		t.Fatalf("version reads at the start = %d, want one per kind", n)
	}
	h.tick()
	h.advance(time.Minute)
	h.tick()
	if n, evs := cli.versionReads(), h.events(EventKindVersionChanged); n != 2 || len(evs) != 0 {
		t.Fatalf("health checks on unchanged binaries: %d version reads, events %+v", n, evs)
	}

	h.advance(time.Hour + 23*time.Minute) // 11:24
	at := h.clock.Now()
	cli.binary("codex", at)
	cli.prints(codex, "codex-cli 0.163.0\n")
	cli.lists("codex", "0.163.0", "0.163.0")
	h.tick()
	evs := h.events(EventKindVersionChanged)
	if len(evs) != 1 || evs[0].Level != "warn" || !strings.Contains(evs[0].Message, "the codex CLI changed: codex-cli 0.162.1 → 0.163.0 at 11:24") {
		t.Fatalf("events %+v, want one warning naming the change", evs)
	}
	v = kindVersion(h, "codex")
	if v.Version != "codex-cli 0.163.0" || v.Previous != "codex-cli 0.162.1" || !v.ChangedAt.Equal(at) || !v.MTime.Equal(at) {
		t.Fatalf("codex after the change: %+v", v)
	}
	if _, err := h.e.batch.Flush(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := toastsWith(h, "codex-cli 0.162.1 → 0.163.0 at 11:24"); len(got) != 1 {
		t.Fatalf("toasts %q, want one naming the change", h.nh.shown())
	}

	h.advance(time.Minute)
	h.tick()
	h.e = New(h.d) // the daemon restarts: the recorded binary is the one on disk
	h.startup()
	h.tick()
	if _, err := h.e.batch.Flush(h.ctx); err != nil {
		t.Fatal(err)
	}
	if n, evs, toasts := cli.versionReads(), h.events(EventKindVersionChanged), toastsWith(h, "→ 0.163.0"); n != 3 || len(evs) != 1 || len(toasts) != 1 {
		t.Fatalf("after the change: %d version reads, %d events, toasts %q; want 3, 1 and 1", n, len(evs), toasts)
	}
}

// The daemon's PATH finds codex as mise's shim, a link to mise itself,
// whose file never changes when mise installs another version: the binary
// is the one `mise which codex` names, and the version is read through the
// shim, as `magnum doctor` reads it.
func TestACLIBehindAMiseShimIsReadThroughMiseWhich(t *testing.T) {
	cli := newFakeCLIs(t)
	h := newHarness(t, cli.install)
	mise := cli.binary("bin/mise", time.Date(2026, 7, 11, 1, 21, 0, 0, time.Local))
	if err := os.Symlink(mise, filepath.Join(cli.dir, "codex")); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(cli.dir, "codex")
	old := cli.binary("installs/codex/0.162.1/bin/codex", time.Date(2026, 10, 9, 21, 20, 0, 0, time.Local))
	cli.names("codex", old)
	cli.prints(shim, "codex-cli 0.162.1\n")

	h.startup()
	if v := kindVersion(h, "codex"); v.Version != "codex-cli 0.162.1" || v.Path != resolved(t, old) {
		t.Fatalf("codex at the start: %+v", v)
	}
	h.tick()
	if n := cli.versionReads(); n != 1 {
		t.Fatalf("version reads = %d, want 1: mise names the same binary", n)
	}

	h.advance(time.Hour)
	cli.names("codex", cli.binary("installs/codex/0.163.0/bin/codex", h.clock.Now()))
	cli.prints(shim, "codex-cli 0.163.0\n")
	h.tick()
	if v, evs := kindVersion(h, "codex"), h.events(EventKindVersionChanged); v.Version != "codex-cli 0.163.0" || len(evs) != 1 {
		t.Fatalf("after mise installed 0.163.0: %+v, events %+v", v, evs)
	}
	if got := cli.run.CallsWithPrefix(shim, "--version"); len(got) != 2 {
		t.Fatalf("the version is read through the shim: %d reads", len(got))
	}
}

// From 2026-10-18 to 10-21 Claude Code's daily auto-update (2.1.302 to
// 2.1.306) was four kind.version_changed warnings, each with a toast, and no
// launch failure followed any of them. A change of a CLI mise does not pin
// (not in mise's config, or requested as latest) is routine: recorded at
// info, with no toast. The launch-failure path still names it, in a parked
// launch's note and in a shared failure's line.
func TestARoutineCLIUpdateRecordsInfoAndNoToast(t *testing.T) {
	for name, inMise := range map[string]bool{
		"not in mise's config": false,
		"mise requests latest": true,
	} {
		t.Run(name, func(t *testing.T) {
			cli := newFakeCLIs(t)
			h := newHarness(t, cli.install)
			installed := time.Date(2026, 10, 20, 7, 31, 0, 0, time.Local)
			codex := cli.binary("codex", installed)
			cli.prints(codex, "codex-cli 0.162.1\n")
			cli.lists("codex", "0.162.1", "0.162.1")
			claude := cli.binary("claude", installed)
			cli.prints(claude, "2.1.305 (Claude Code)\n")
			if inMise {
				cli.lists("claude", "latest", "2.1.305")
			}
			h.startup()
			h.tick()

			h.advance(time.Hour)
			at := h.clock.Now()
			cli.binary("claude", at)
			cli.prints(claude, "2.1.306 (Claude Code)\n")
			if inMise {
				cli.lists("claude", "latest", "2.1.306")
			}
			h.tick()
			evs := h.events(EventKindVersionChanged)
			if len(evs) != 1 || evs[0].Level != "info" ||
				!strings.Contains(evs[0].Message, "the claude CLI changed: 2.1.305 (Claude Code) → 2.1.306 at "+at.Local().Format("15:04")) {
				t.Fatalf("events %+v, want one at info naming the change", evs)
			}
			if v := kindVersion(h, "claude"); v.Version != "2.1.306 (Claude Code)" || v.Previous != "2.1.305 (Claude Code)" || !v.ChangedAt.Equal(at) {
				t.Fatalf("claude after the update: %+v", v)
			}
			h.flush()
			if got := toastsWith(h, "2.1.306"); len(got) != 0 {
				t.Fatalf("toasts %q, want none for a CLI mise does not pin", got)
			}

			note := attention.CLIChange("claude", at, h.clock.Now())
			if got := h.e.cliChangeNote(h.ctx, "start claude-review: agents: the agent did not come up"); got != note {
				t.Fatalf("a parked launch's note = %q, want %q", got, note)
			}
			const line = "Error: unexpected argument '--frobnicate' found"
			clearedLaunch(h, 1, "claude-review", line)
			clearedLaunch(h, 2, "claude-review", line)
			h.tick()
			if evs := h.events(EventSharedFailure); len(evs) != 1 || !strings.Contains(evs[0].Message, note+", to 2.1.306 (Claude Code)") {
				t.Fatalf("shared failures %+v, want one naming the update", evs)
			}
		})
	}
}

// Codex is pinned by mise (0.162.1 since 0.163.0 broke the judge panes on
// 2026-10-16), so a change of its version is unexpected: a warning and one
// toast, both naming the pin. mise's tool is found by the kind's name, or
// by the install directory of the binary the kind's command runs; the pin
// is read only when the version changed.
func TestAChangeOfAPinnedCLIStillWarnsAndToasts(t *testing.T) {
	for name, tc := range map[string]struct {
		tool string // as mise's config names it
		shim bool   // the daemon's PATH finds codex as mise's shim, which runs the pinned install
		pin  string // what mise pins once the version changed
	}{
		"by its name, a binary on PATH the pin does not explain": {tool: "codex", pin: "0.162.1"},
		"by the binary's install directory, the pin moved":       {tool: "npm:@openai/codex", shim: true, pin: "0.163.0"},
	} {
		t.Run(name, func(t *testing.T) {
			cli := newFakeCLIs(t)
			h := newHarness(t, cli.install)
			installed := time.Date(2026, 10, 16, 11, 0, 0, 0, time.Local)
			cli.lists(tc.tool, "0.162.1", "0.162.1")
			codex := filepath.Join(cli.dir, "codex")
			if tc.shim {
				if err := os.Symlink(filepath.Join(cli.dir, "mise"), codex); err != nil {
					t.Fatal(err)
				}
				cli.names("codex", cli.binary(filepath.Join(installDir(tc.tool, "0.162.1"), "bin", "codex"), installed))
			} else {
				cli.binary("codex", installed)
			}
			cli.prints(codex, "codex-cli 0.162.1\n")
			claude := cli.binary("claude", installed)
			cli.prints(claude, "2.1.305 (Claude Code)\n")
			h.startup()
			h.tick()
			if n := cli.miseLists(); n != 0 {
				t.Fatalf("mise listed its tools %d times before any change", n)
			}

			h.advance(time.Hour + 24*time.Minute)
			at := h.clock.Now()
			if tc.shim {
				cli.names("codex", cli.binary(filepath.Join(installDir(tc.tool, tc.pin), "bin", "codex"), at))
			} else {
				cli.binary("codex", at)
			}
			cli.prints(codex, "codex-cli 0.163.0\n")
			cli.lists(tc.tool, tc.pin, tc.pin)
			h.tick()
			pins := "mise pins it to " + tc.pin
			evs := h.events(EventKindVersionChanged)
			if len(evs) != 1 || evs[0].Level != "warn" || !strings.Contains(evs[0].Message, "the codex CLI changed: codex-cli 0.162.1 → 0.163.0 at 11:24") ||
				!strings.Contains(evs[0].Message, pins) {
				t.Fatalf("events %+v, want one warning naming the change and %q", evs, pins)
			}
			h.flush()
			if got := toastsWith(h, "codex-cli 0.162.1 → 0.163.0 at 11:24"); len(got) != 1 || !strings.Contains(got[0], pins) {
				t.Fatalf("toasts %q, want one naming the change and %q", h.nh.shown(), pins)
			}
			if n := cli.miseLists(); n != 1 {
				t.Fatalf("mise listed its tools %d times, want once, at the change", n)
			}
		})
	}
}

// A setup that parked a PR because the judge did not start (a dead launch
// that held its agent name, three attempts) names a change of the codex CLI
// within the last day in the stored error, the needs-attention line and its
// toast; an older change says nothing.
func TestAParkedLaunchFailureNamesTheRecentCLIChange(t *testing.T) {
	for name, tc := range map[string]struct {
		ago  time.Duration
		note bool
	}{
		"changed an hour before": {time.Hour, true},
		"changed a day before":   {25 * time.Hour, false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			changed := h.clock.Now().Add(-tc.ago)
			if err := writeKVJSON(h.ctx, h.e, KVKindVersion("codex"), "kind version", KindVersion{Version: "codex-cli 0.163.0",
				Path: "/opt/codex/0.163.0/codex", SeenAt: changed, Previous: "codex-cli 0.162.1", ChangedAt: changed}); err != nil {
				t.Fatal(err)
			}
			h.ag.startErr = func(pr store.PR, role config.Role, pane string) error {
				if !role.Judge {
					return nil
				}
				name := agents.AgentName("talkable/talkable", pr.Number, agents.Role(role.Name))
				return fmt.Errorf("agents: start %s in %s: %w", name, pane,
					&herdr.Error{Method: "agent.start", Code: "agent_name_taken", Message: "agent name " + name + " is already in use"})
			}
			h.open(prSpec{n: 1, head: "base1"})
			h.startup()
			h.tick()
			h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "abc1234def5678"})
			h.tick()
			h.advance(5 * time.Minute)
			h.tick()
			h.advance(time.Minute)
			h.tick()
			h.advance(2 * time.Minute)
			h.tick()
			pr := h.wantState(2, store.PRNeedsAttention)
			note := "the codex CLI changed at " + changed.Format("15:04")
			if got := deref(pr.LastError); strings.HasSuffix(got, "; "+note) != tc.note || strings.Contains(got, "CLI changed") != tc.note {
				t.Fatalf("stored error %q, want the note %q: %v", got, note, tc.note)
			}
			kind, _ := kvValue(h, KVPRAttention(pr.ID))
			r := attention.Explain(kind, deref(pr.LastError), "talkable#2")
			if r.Kind != attention.KindFailed || strings.HasSuffix(r.Summary, "; "+note) != tc.note || !strings.Contains(r.Summary, "a dead launch holds the codex-judge's pane") {
				t.Fatalf("reason %+v", r)
			}
			if got := toastsWith(h, "CLI changed"); len(got) != map[bool]int{true: 1}[tc.note] {
				t.Fatalf("toasts naming the change %q", got)
			}
		})
	}
}

// versionUnreadable is the first words of the warning a failed version read
// logs.
const versionUnreadable = "cannot read the agent CLI's version"

// At the daemon start of 2026-10-18 01:27 the read of codex's version failed
// ("shims/codex --version: context canceled"), and the stamp of the binary
// was kept as if it had been read, so the version was not read again until
// the shim's mtime changed, which a mise shim's never does: `magnum status`
// stayed without codex's version. A read that failed stores no stamp: the
// next health check runs the command again, with the same timeout as the
// start's, and the version is recorded once it works, with one warning for
// the failure.
func TestAFailedVersionReadIsTriedAgainAtTheNextHealthCheck(t *testing.T) {
	cli := newFakeCLIs(t)
	logs := &syncBuffer{}
	h := newHarness(t, cli.install, func(h *harness) { h.d.Logger = newTeeLogger(h.t, logs) })
	installed := time.Date(2026, 10, 9, 21, 20, 0, 0, time.Local)
	codex := cli.binary("codex", installed)
	cli.prints(codex, "codex-cli 0.162.1\n")
	cli.failsWith(codex, context.Canceled)
	claude := cli.binary("claude", installed)
	cli.prints(claude, "2.1.290 (Claude Code)\n")

	h.startup()
	if _, ok, _ := readKVJSON[KindVersion](h.ctx, h.e, KVKindVersion("codex"), "kind version"); ok {
		t.Fatal("a version was recorded for codex after a read that failed")
	}
	if v := kindVersion(h, "claude"); v.Version != "2.1.290 (Claude Code)" {
		t.Fatalf("claude at the start: %+v", v)
	}
	if n := logLines(logs, "WARN", versionUnreadable); n != 1 {
		t.Fatalf("warnings after the failed read = %d, want 1\n%s", n, logs.String())
	}

	h.advance(time.Minute)
	h.tick()
	v := kindVersion(h, "codex")
	if v.Version != "codex-cli 0.162.1" || v.Path != resolved(t, codex) || !v.MTime.Equal(installed) || !v.SeenAt.Equal(h.clock.Now()) {
		t.Fatalf("codex after the next health check: %+v", v)
	}
	if n, timeouts := cli.versionReadsOf(codex); n != 2 || !timeouts {
		t.Fatalf("codex reads = %d (every one with the health check's timeout: %v), want 2 and true", n, timeouts)
	}
	if n := logLines(logs, "INFO", "agent CLI version"); n != 2 { // claude's at the start, codex's now
		t.Fatalf("version lines at info = %d, want one per kind\n%s", n, logs.String())
	}
	h.advance(time.Minute)
	h.tick()
	if n, _ := cli.versionReadsOf(codex); n != 2 {
		t.Fatalf("codex reads after it was read = %d: the stamp keeps the command from running at every check", n)
	}
	if n, evs := logLines(logs, "WARN", versionUnreadable), h.events(EventKindVersionUnknown); n != 1 || len(evs) != 0 {
		t.Fatalf("warnings = %d, %d kind.version_unknown events; want 1 and none", n, len(evs))
	}
}

// A binary whose version command keeps failing is tried at every health
// check, and says so once: one warning for the cause, and one
// kind.version_unknown event at the third failed check in a row (the
// start's read counts as the first); the read that works at last records the
// version.
func TestAVersionReadThatKeepsFailingWarnsOnce(t *testing.T) {
	cli := newFakeCLIs(t)
	logs := &syncBuffer{}
	h := newHarness(t, cli.install, func(h *harness) { h.d.Logger = newTeeLogger(h.t, logs) })
	codex := cli.binary("codex", time.Date(2026, 10, 9, 21, 20, 0, 0, time.Local))
	cli.prints(codex, "codex-cli 0.162.1\n")
	cli.failsWith(codex, repeated(context.DeadlineExceeded, 6)...)

	h.startup()
	h.advance(time.Minute)
	h.tick()
	if n, evs := logLines(logs, "WARN", versionUnreadable), h.events(EventKindVersionUnknown); n != 1 || len(evs) != 0 {
		t.Fatalf("after 2 failed checks: %d warnings, %d kind.version_unknown events; want 1 and none", n, len(evs))
	}
	for range 4 {
		h.advance(time.Minute)
		h.tick()
	}
	if n, _ := cli.versionReadsOf(codex); n != 6 {
		t.Fatalf("reads = %d, want one at the start and one at each of the 5 checks", n)
	}
	if n := logLines(logs, "WARN", versionUnreadable); n != 1 {
		t.Fatalf("warnings = %d, want 1 for one cause\n%s", n, logs.String())
	}
	evs := h.events(EventKindVersionUnknown)
	if len(evs) != 1 || evs[0].Level != "warn" ||
		!strings.Contains(evs[0].Message, "the codex CLI's version cannot be read") || !strings.Contains(evs[0].Message, "3 health checks in a row") {
		t.Fatalf("kind.version_unknown events %+v, want one warning for codex naming 3 checks", evs)
	}
	var subject string
	if err := h.st.DB().QueryRowContext(h.ctx, "SELECT subject FROM events WHERE kind = ?", EventKindVersionUnknown).Scan(&subject); err != nil || subject != "tool:codex" {
		t.Fatalf("kind.version_unknown subject = %q, %v; want tool:codex", subject, err)
	}
	if _, ok, _ := readKVJSON[KindVersion](h.ctx, h.e, KVKindVersion("codex"), "kind version"); ok {
		t.Fatal("a version was recorded for codex while its reads failed")
	}

	h.advance(time.Minute)
	h.tick() // the seventh read works
	if v := kindVersion(h, "codex"); v.Version != "codex-cli 0.162.1" {
		t.Fatalf("codex after its read worked: %+v", v)
	}
	h.advance(time.Minute)
	h.tick()
	if n, evs := cli.versionReads(), h.events(EventKindVersionUnknown); n != 7 || len(evs) != 1 {
		t.Fatalf("reads = %d, kind.version_unknown events = %d; want 7 and 1", n, len(evs))
	}
}

// A read that fails for another cause than the one warned about is another
// warning, and the causes do not restart the count of failed checks.
func TestAVersionReadFailingForAnotherCauseWarnsAgain(t *testing.T) {
	cli := newFakeCLIs(t)
	logs := &syncBuffer{}
	h := newHarness(t, cli.install, func(h *harness) { h.d.Logger = newTeeLogger(h.t, logs) })
	codex := cli.binary("codex", time.Date(2026, 10, 9, 21, 20, 0, 0, time.Local))
	cli.prints(codex, "codex-cli 0.162.1\n")
	cli.failsWith(codex, context.DeadlineExceeded, context.DeadlineExceeded, errors.New("exit status 2"), errors.New("exit status 2"))

	h.startup()
	for range 3 {
		h.advance(time.Minute)
		h.tick()
	}
	if n := logLines(logs, "WARN", versionUnreadable); n != 2 {
		t.Fatalf("warnings = %d, want one per cause (a timeout, an exit)\n%s", n, logs.String())
	}
	if evs := h.events(EventKindVersionUnknown); len(evs) != 1 {
		t.Fatalf("kind.version_unknown events = %d, want 1: the causes do not restart the count", len(evs))
	}
}

// Four daemon stops from 2026-10-20 to 10-22 each cut the health check's
// read of the recorded CLI version, and each logged "read the recorded CLI
// version" at warn for "context canceled". A read a stop cuts logs nothing
// at warn, records nothing and is tried again at the next start.
func TestTheCLIVersionReadAStopCutsLogsNoWarning(t *testing.T) {
	cli := newFakeCLIs(t)
	logs := &syncBuffer{}
	h := newHarness(t, cli.install, func(h *harness) { h.d.Logger = newTeeLogger(h.t, logs) })
	codex := cli.binary("codex", time.Date(2026, 10, 9, 21, 20, 0, 0, time.Local))
	cli.prints(codex, "codex-cli 0.162.1\n")
	ctx, cancel := context.WithCancel(h.ctx)
	cancel()
	h.e.checkCLIVersion(ctx, "codex")
	if n := logLines(logs, "WARN", ""); n != 0 {
		t.Fatalf("warnings at a stop = %d, want none\n%s", n, logs.String())
	}
	if _, ok, _ := readKVJSON[KindVersion](h.ctx, h.e, KVKindVersion("codex"), "kind version"); ok {
		t.Fatal("a version was recorded by a read the stop cut")
	}
	h.e.checkCLIVersion(h.ctx, "codex")
	if v := kindVersion(h, "codex"); v.Version != "codex-cli 0.162.1" {
		t.Fatalf("codex at the next check: %+v", v)
	}
}
