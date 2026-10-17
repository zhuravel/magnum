package engine

import (
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
// prints and what `mise which <kind>` names, through a fake runner.
type fakeCLIs struct {
	t   *testing.T
	dir string
	run *execx.Fake

	mu    sync.Mutex
	out   map[string]string // a command's path → what its --version prints
	which map[string]string // kind → the binary `mise which <kind>` names
}

func newFakeCLIs(t *testing.T) *fakeCLIs {
	t.Helper()
	c := &fakeCLIs{t: t, dir: t.TempDir(), out: map[string]string{}, which: map[string]string{}}
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
		if out, ok := c.out[cmd.Name]; ok {
			return execx.Result{Stdout: []byte(out)}, nil
		}
	case len(cmd.Args) == 2 && cmd.Args[0] == "which":
		if p, ok := c.which[cmd.Args[1]]; ok {
			return execx.Result{Stdout: []byte(p + "\n")}, nil
		}
	}
	return execx.Result{Code: 1}, fmt.Errorf("fake CLIs: no answer for %s", cmd.String())
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
// the binary changed, and a new version is one warning and one toast, which
// later checks and a restart do not repeat.
func TestACLIVersionChangeIsRecordedOnceAndToasted(t *testing.T) {
	cli := newFakeCLIs(t)
	h := newHarness(t, cli.install)
	installed := time.Date(2026, 10, 9, 21, 20, 0, 0, time.Local)
	codex := cli.binary("codex", installed)
	cli.prints(codex, "codex-cli 0.162.1\n")
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
