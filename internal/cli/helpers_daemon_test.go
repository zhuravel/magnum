package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/paths"
)

// daemonGroupTest is one command invocation of the daemon group against a
// temporary repo checkout and a temporary user home.
type daemonGroupTest struct {
	ctx      *Context
	stdout   *bytes.Buffer
	stderr   *bytes.Buffer
	userHome string
	fake     *execx.Fake
	killed   []string
}

// newDaemonGroupTest swaps daemonSys for hermetic seams: a Fake runner,
// uid 501, a temp user home, no real signals and no sleeping.
func newDaemonGroupTest(t *testing.T, rules ...execx.Rule) *daemonGroupTest {
	t.Helper()
	repo := t.TempDir() // a checkout: the herdr plugin manifest marks it
	if err := os.WriteFile(filepath.Join(repo, "herdr-plugin.toml"), []byte("id = \"zhuravel.magnum\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Layout = paths.Layout{Home: repo}
	cfg.Herdr.Socket = "/tmp/herdr-test.sock"
	dt := &daemonGroupTest{
		stdout:   &bytes.Buffer{},
		stderr:   &bytes.Buffer{},
		userHome: t.TempDir(),
		// The skew guard's `bin/magnum config` passes unless a test's own
		// rule (matched first) says otherwise.
		fake: &execx.Fake{Rules: append(rules, daemonRuleOK([]string{paths.Layout{Home: repo}.Binary(), "config"}, ""))},
	}
	dt.ctx = &Context{Version: "test", Layout: paths.Layout{Home: repo}, Config: cfg, Stdout: dt.stdout, Stderr: dt.stderr}
	old := daemonSys
	t.Cleanup(func() { daemonSys = old })
	daemonSys = daemonGroupSys{
		Runner:   dt.fake,
		UID:      func() int { return 501 },
		UserHome: func() (string, error) { return dt.userHome, nil },
		Getenv:   func(string) string { return "" },
		Kill: func(pid int, sig syscall.Signal) error {
			dt.killed = append(dt.killed, sig.String())
			return nil
		},
		DaemonPID: func(paths.Layout) (int, error) { return 0, nil },
		Sleep:     func(time.Duration) {},
		RunEngine: func(context.Context, *Context, daemonOptions) (daemonDryRunReport, error) {
			t.Fatal("RunEngine not expected")
			return daemonDryRunReport{}, nil
		},
	}
	return dt
}

// useMiseKey makes the checkout's mise environment hold an App's key
// (.mise.toml, an App identity with private_key_env and no
// private_key_file): launchd and the gh shim then go through mise.
func (dt *daemonGroupTest) useMiseKey(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dt.ctx.Layout.Home, ".mise.toml"), []byte("[env]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := *dt.ctx.Config
	cfg.Identities = append(slices.Clone(cfg.Identities), config.Identity{Name: "app", Kind: "app", Login: "example[bot]",
		AppID: 1, ClientID: "c", InstallationID: 2, PrivateKeyEnv: "MAGNUM_TEST_APP_KEY"})
	dt.ctx.Config = &cfg
}

func (dt *daemonGroupTest) run(name string, args ...string) int {
	return execute(dt.ctx, append([]string{name}, args...))
}

func (dt *daemonGroupTest) writeBinary(t *testing.T) {
	t.Helper()
	bin := dt.ctx.Layout.Binary()
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func daemonRuleOK(prefix []string, out string) execx.Rule {
	return execx.Rule{Prefix: prefix, Result: execx.Result{Stdout: []byte(out)}}
}

// daemonRulePS answers stopMagnumDaemon's two ps calls for pid 250
// (engine.ProcessCommand): the executable (comm), then the command line.
func daemonRulePS(comm, args string) (execx.Rule, execx.Rule) {
	return daemonRuleOK([]string{"ps", "-p", "250", "-o", "comm="}, comm+"\n"),
		daemonRuleOK([]string{"ps", "-p", "250", "-o", "args="}, args+"\n")
}

func daemonRuleExit(prefix []string, code int, stderr string) execx.Rule {
	return execx.Rule{Prefix: prefix, Result: execx.Result{Code: code, Stderr: []byte(stderr)}}
}

const herdrPluginListEmpty = `{"id":"cli:plugin","result":{"plugins":[],"type":"plugin_list"}}`

func herdrPluginListWith(root string, enabled bool) string {
	en := "false"
	if enabled {
		en = "true"
	}
	return `{"id":"cli:plugin","result":{"plugins":[{"plugin_id":"zhuravel.magnum","plugin_root":"` + root +
		`","manifest_path":"` + root + `/herdr-plugin.toml","enabled":` + en + `,"source":{"kind":"local"},"version":"0.1.0"}],"type":"plugin_list"}}`
}

func launchctlPrint(state string, pid int) string {
	s := "gui/501/zhuravel.magnum = {\n\tactive count = 1\n\tstate = " + state + "\n"
	if pid > 0 {
		s += "\tpid = " + strconv.Itoa(pid) + "\n"
	}
	return s + "}\n"
}

func TestTabBarSnippet(t *testing.T) {
	got := tabBarSnippet("/Users/x/Projects/magnum/state/tabbar.txt")
	want := `{ type = 'command', command = 'f="/Users/x/Projects/magnum/state/tabbar.txt"; { read -r at max`
	if !strings.Contains(got, want) || !strings.Contains(got, `interval_seconds = 6, timeout_seconds = 2 },`) {
		t.Fatalf("snippet missing entry:\n%s", got)
	}
	if !strings.Contains(got, "tab_bar_right") || !strings.Contains(got, "never edits") || !strings.Contains(got, `"magnum down"`) {
		t.Fatalf("snippet missing instructions:\n%s", got)
	}
	// A path with shell/TOML metacharacters stays valid in both languages.
	odd := tabBarSnippet(`/tmp/it's "$x"/tabbar.txt`)
	if !strings.Contains(odd, `command = "f=\"/tmp/it's \\\"\\$x\\\"/tabbar.txt\"; `) {
		t.Fatalf("odd path not escaped:\n%s", odd)
	}
}

// TestTabBarCommandShowsDownWhenStale runs the tab-bar command with /bin/sh
// on files the engine writes: a fresh file shows its line; a stale, missing
// or timestamp-less file shows "magnum down".
func TestTabBarCommandShowsDownWhenStale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tabbar.txt")
	run := func() string {
		t.Helper()
		out, err := exec.Command("/bin/sh", "-c", tabBarCommand(path)).Output()
		if err != nil {
			t.Fatalf("sh: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	if got := run(); got != "magnum down" {
		t.Fatalf("missing file: %q", got)
	}
	if err := engine.WriteTabBarFile(path, time.Now(), 90*time.Second, "magnum · 1 reviewing · codex 42%"); err != nil {
		t.Fatal(err)
	}
	if got := run(); got != "magnum · 1 reviewing · codex 42%" {
		t.Fatalf("fresh file: %q", got)
	}
	if err := engine.WriteTabBarFile(path, time.Now().Add(-91*time.Second), 90*time.Second, "magnum · idle"); err != nil {
		t.Fatal(err)
	}
	if got := run(); got != "magnum down" {
		t.Fatalf("stale file: %q", got)
	}
	if err := os.WriteFile(path, []byte("magnum · idle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run(); got != "magnum down" {
		t.Fatalf("file without a timestamp: %q", got)
	}
}
