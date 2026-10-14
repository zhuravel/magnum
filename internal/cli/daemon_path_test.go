package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// launchdTestPATH stands for the PATH the LaunchAgent gives the daemon.
const launchdTestPATH = "/Users/alice/.local/bin:/Users/alice/.local/share/mise/shims:/opt/homebrew/bin:/usr/bin:/bin"

// shellPATHTest is the daemon's start against a fake login shell: a script
// named zsh in a temp dir that $SHELL names, run by the real runner, and an
// environment that daemonSys's Getenv reads and Setenv writes, so the test
// process's own PATH never changes.
type shellPATHTest struct {
	env map[string]string
	cfg *config.Config
}

func newShellPATHTest(t *testing.T, script string) *shellPATHTest {
	t.Helper()
	storetest.Serial(t) // swaps daemonSys and shellPATHTimeout
	shell := filepath.Join(t.TempDir(), "zsh")
	// The daemon asks `$SHELL -ilc <script>`: anything else exits 9.
	body := "#!/bin/sh\n[ \"$1\" = -ilc ] || exit 9\n" + script + "\n"
	if err := os.WriteFile(shell, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	st := &shellPATHTest{env: map[string]string{"SHELL": shell, "PATH": launchdTestPATH}}
	old, oldTimeout := daemonSys, shellPATHTimeout
	t.Cleanup(func() { daemonSys, shellPATHTimeout = old, oldTimeout })
	daemonSys = daemonGroupSys{
		Runner: &execx.Real{},
		Getenv: func(k string) string { return st.env[k] },
		Setenv: func(k, v string) error { st.env[k] = v; return nil },
	}
	st.cfg = config.Defaults()
	st.cfg.Daemon.ShellPath = true
	return st
}

// The daemon's PATH gains the login shell's directories it lacked, in the
// shell's order (a repeated one once), after its own, which keeps its order;
// the event names the count.
func TestTheDaemonTakesItsPATHFromTheLoginShell(t *testing.T) {
	st := newShellPATHTest(t, "echo '/Users/alice/.local/bin:/opt/homebrew/bin:/Applications/DBngin.app/mysql-8.0.33/bin:/usr/bin:"+
		"/Applications/DBngin.app/redis-7.2.4/bin:/bin:/Applications/DBngin.app/mysql-8.0.33/bin/:/Users/alice/go/bin'")
	got := daemonShellPATH(context.Background(), st.cfg)
	if got == nil {
		t.Fatal("no result with the key on")
	}
	want := launchdTestPATH + ":/Applications/DBngin.app/mysql-8.0.33/bin:/Applications/DBngin.app/redis-7.2.4/bin:/Users/alice/go/bin"
	if st.env["PATH"] != want {
		t.Fatalf("PATH = %s\nwant   %s", st.env["PATH"], want)
	}
	if got.Failed != "" || got.Count != 3 || len(got.Added) != 3 || got.Shell != "zsh" {
		t.Fatalf("result = %+v", got)
	}
	if msg := got.Message(); msg != "PATH from zsh: 3 directories added" {
		t.Fatalf("event message = %q", msg)
	}
}

// A login shell that never answers leaves the launchd PATH as it was once the
// timeout ends, and the start records a warning.
func TestAShellThatHangsLeavesTheLaunchdPATH(t *testing.T) {
	st := newShellPATHTest(t, "exec sleep 30")
	shellPATHTimeout = 200 * time.Millisecond
	start := time.Now()
	got := daemonShellPATH(context.Background(), st.cfg)
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the capture took %s", took)
	}
	if st.env["PATH"] != launchdTestPATH {
		t.Fatalf("PATH changed: %s", st.env["PATH"])
	}
	if got == nil || got.Failed != "answered nothing in 200ms" || got.Count != 0 {
		t.Fatalf("result = %+v", got)
	}
	if msg := got.Message(); msg != "the login shell (zsh) answered nothing in 200ms. The launchd PATH stays" {
		t.Fatalf("event message = %q", msg)
	}
}

// Banners and hook output around the PATH are skipped: the last non-empty
// line that reads as a PATH counts, a line that only starts like a path
// does not.
func TestTheShellsBannerIsSkipped(t *testing.T) {
	st := newShellPATHTest(t, "echo 'Welcome back, alice'\necho '/etc/motd: 3 updates waiting'\n"+
		"echo '/usr/bin:/Applications/DBngin.app/postgresql-16/bin:/bin'\necho\necho 'see you'")
	got := daemonShellPATH(context.Background(), st.cfg)
	if want := launchdTestPATH + ":/Applications/DBngin.app/postgresql-16/bin"; st.env["PATH"] != want {
		t.Fatalf("PATH = %s\nwant   %s", st.env["PATH"], want)
	}
	if got == nil || got.Count != 1 || got.Failed != "" {
		t.Fatalf("result = %+v", got)
	}
}

// A shell that fails or prints no PATH leaves the launchd PATH too, and says
// why without quoting anything it printed.
func TestAShellThatFailsLeavesTheLaunchdPATH(t *testing.T) {
	for _, tc := range []struct{ script, failed string }{
		{"echo '/opt/x/bin'\nexit 3", "exited with status 3"},
		{"echo 'no path here'", "printed no PATH"},
	} {
		st := newShellPATHTest(t, tc.script)
		got := daemonShellPATH(context.Background(), st.cfg)
		if st.env["PATH"] != launchdTestPATH {
			t.Errorf("%q: PATH changed: %s", tc.script, st.env["PATH"])
		}
		if got == nil || got.Failed != tc.failed || strings.Contains(got.Message(), "/opt/x") {
			t.Errorf("%q: result = %+v", tc.script, got)
		}
	}
	st := newShellPATHTest(t, "")
	st.env["SHELL"] = filepath.Join(t.TempDir(), "missing-shell")
	if got := daemonShellPATH(context.Background(), st.cfg); got == nil || got.Failed != "could not start" || got.Shell != "missing-shell" {
		t.Errorf("a missing shell: result = %+v", got)
	}
}

// With [daemon] shell_path off (the default) no shell runs and the PATH
// stays.
func TestTheShellPATHKeyOffRunsNoShell(t *testing.T) {
	st := newShellPATHTest(t, "echo /opt/x/bin")
	fake := &execx.Fake{}
	daemonSys.Runner = fake
	st.cfg.Daemon.ShellPath = false
	if got := daemonShellPATH(context.Background(), st.cfg); got != nil {
		t.Fatalf("result with the key off = %+v", got)
	}
	if len(fake.Calls) != 0 || st.env["PATH"] != launchdTestPATH {
		t.Fatalf("calls %v, PATH %s", fake.Calls, st.env["PATH"])
	}
	if config.Defaults().Daemon.ShellPath {
		t.Fatal("shell_path is on by default")
	}
}
