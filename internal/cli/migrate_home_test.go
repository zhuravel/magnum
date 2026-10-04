package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/launchd"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// migrateFixture is a checkout whose state/ holds a registry, reports,
// notes, logs, a gh config dir, runtime files and a backup magnum does not
// own; XDG points into temp dirs.
func migrateFixture(t *testing.T, rules ...execx.Rule) (*daemonGroupTest, paths.Layout) {
	t.Helper()
	dt := newDaemonGroupTest(t, rules...)
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	st := dt.ctx.Layout.State()
	reg, err := store.Open(dt.ctx.Layout.DB()) // a real registry: the rounds guard reads it
	if err != nil {
		t.Fatal(err)
	}
	reg.Close()
	for rel, body := range map[string]string{
		"reviews/o/r/1/abc/judge.json": "{}", "notes/o/r.md": "n",
		"logs/daemon.log": "l", "gh/app/hosts.yml": "h", "tabbar.txt": "t", "daemon.pid": "1", "magnum.lock": "",
		"config.toml.bak": "b",
	} {
		p := filepath.Join(st, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dst, err := dt.ctx.Layout.Installed()
	if err != nil {
		t.Fatal(err)
	}
	return dt, dst
}

func notLoaded() execx.Rule {
	return daemonRuleExit([]string{"launchctl", "print"}, 113, "Could not find service")
}

// Everything moves by rename to the XDG places, nothing links back, the
// runtime files go, and what magnum does not own stays and is named.
func TestMigrateHomeMovesWithoutLinks(t *testing.T) {
	dt, dst := migrateFixture(t, notLoaded())
	src := dt.ctx.Layout
	if code := dt.run("migrate-home"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	for _, p := range []string{dst.DB(), filepath.Join(dst.Reviews(), "o/r/1/abc/judge.json"),
		filepath.Join(dst.Notes(), "o/r.md"), dst.DaemonLog(), filepath.Join(dst.GhConfigDir("app"), "hosts.yml"), dst.TabBar()} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("not moved: %s", p)
		}
	}
	ents, _ := os.ReadDir(src.State())
	if len(ents) != 1 || ents[0].Name() != "config.toml.bak" {
		t.Fatalf("left in state/: %v", ents)
	}
	_ = filepath.WalkDir(filepath.Dir(src.State()), func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type()&fs.ModeSymlink != 0 {
			t.Errorf("a link was left: %s", p)
		}
		return nil
	})
	out := dt.stdout.String()
	for _, want := range []string{"moved ", "left in ", "config.toml.bak", "magnum install"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// A state/ that the move empties is removed.
func TestMigrateHomeRemovesTheEmptiedStateDir(t *testing.T) {
	dt, _ := migrateFixture(t, notLoaded())
	old := dt.ctx.Layout.State()
	if err := os.Remove(filepath.Join(old, "config.toml.bak")); err != nil {
		t.Fatal(err)
	}
	if code := dt.run("migrate-home"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("state/ kept: %v", err)
	}
	if dt.ctx.Layout.CheckoutLayout() {
		t.Fatal("the command kept using the checkout layout")
	}
}

// A rename that fails puts back every item already moved: the registry
// never ends up split between two places.
func TestMigrateHomeRollsBackAFailedMove(t *testing.T) {
	dt, dst := migrateFixture(t, notLoaded())
	src := dt.ctx.Layout
	if err := os.MkdirAll(filepath.Dir(dst.Data()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dst.Data(), 0o500); err != nil { // the data dir refuses new entries
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dst.Data(), 0o700) })
	if code := dt.run("migrate-home"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	for _, rel := range []string{"magnum.db", "reviews/o/r/1/abc/judge.json", "logs/daemon.log", "gh/app/hosts.yml", "tabbar.txt"} {
		if _, err := os.Stat(filepath.Join(src.State(), rel)); err != nil {
			t.Errorf("not put back: %s", rel)
		}
	}
	if _, err := os.Stat(dst.DaemonLog()); !os.IsNotExist(err) {
		t.Errorf("a moved item stayed at the new place: %v", err)
	}
	if !strings.Contains(dt.stderr.String(), "nothing moved") {
		t.Errorf("stderr: %s", dt.stderr)
	}
}

// MAGNUM_HOME keeps the checkout layout on purpose; a registry already at
// the new place is never overwritten; a dry run moves nothing.
func TestMigrateHomeRefusals(t *testing.T) {
	dt, dst := migrateFixture(t, notLoaded())
	src := dt.ctx.Layout
	if code := dt.run("migrate-home", "--dry-run"); code != 0 || !strings.Contains(dt.stdout.String(), "would move") {
		t.Fatalf("dry run: exit %d\n%s", code, dt.stdout)
	}
	if _, err := os.Stat(src.DB()); err != nil {
		t.Fatal("the dry run moved the registry")
	}
	old := daemonSys.Getenv
	daemonSys.Getenv = func(k string) string {
		if k == "MAGNUM_HOME" {
			return src.Home
		}
		return ""
	}
	if code := dt.run("migrate-home"); code != 1 || !strings.Contains(dt.stderr.String(), "unset MAGNUM_HOME") {
		t.Fatalf("MAGNUM_HOME: exit %d, %s", code, dt.stderr)
	}
	daemonSys.Getenv = old
	if err := os.MkdirAll(dst.Data(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst.DB(), []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := dt.run("migrate-home"); code != 1 || !strings.Contains(dt.stderr.String(), "holds a registry already") {
		t.Fatalf("an existing registry: exit %d, %s", code, dt.stderr)
	}
	if b, _ := os.ReadFile(dst.DB()); string(b) != "other" {
		t.Fatal("the existing registry was touched")
	}
}

// A loaded launchd job is stopped, rewritten for the new places (no
// MAGNUM_HOME, logs in the state dir) and started again.
func TestMigrateHomeRewritesTheLaunchAgent(t *testing.T) {
	dt, dst := migrateFixture(t, installRules(herdrPluginListEmpty)...)
	dt.writeBinary(t)
	if code := dt.run("migrate-home"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if n := len(dt.fake.CallsWithPrefix("launchctl", "bootout")); n < 1 {
		t.Fatalf("bootout calls = %d", n)
	}
	b, err := os.ReadFile(launchd.AgentPath(dt.userHome, launchd.DefaultLabel))
	if err != nil {
		t.Fatal(err)
	}
	plist := string(b)
	if strings.Contains(plist, "MAGNUM_HOME") || !strings.Contains(plist, launchd.LogPath(dst.Logs())) {
		t.Fatalf("plist:\n%s", plist)
	}
}

// A real invocation starts without a loaded config (the fixtures hand one
// in); migrate-home loads it before it rewrites the launchd agent, which
// needs the herdr socket and the identities.
func TestMigrateHomeLoadsTheConfig(t *testing.T) {
	dt, dst := migrateFixture(t, installRules(herdrPluginListEmpty)...)
	dt.writeBinary(t)
	dt.ctx.Config = nil
	if code := dt.run("migrate-home"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if _, err := os.Stat(dst.DB()); err != nil {
		t.Fatalf("registry not moved: %v", err)
	}
	if _, err := os.Stat(launchd.AgentPath(dt.userHome, launchd.DefaultLabel)); err != nil {
		t.Fatalf("plist not written: %v", err)
	}
}
