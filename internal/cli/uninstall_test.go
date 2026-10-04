package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

func writeDaemonGroupFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUninstallRemovesJobAndPluginButKeepsState(t *testing.T) {
	dt := newDaemonGroupTest(t)
	dt.fake.Rules = []execx.Rule{
		daemonRuleOK([]string{"launchctl", "bootout"}, ""),
		daemonRuleOK([]string{"herdr", "plugin", "list"}, herdrPluginListWith(dt.ctx.Layout.Home, true)),
		daemonRuleOK([]string{"herdr", "plugin", "unlink", "zhuravel.magnum"}, ""),
	}
	plist := filepath.Join(dt.userHome, "Library", "LaunchAgents", "zhuravel.magnum.plist")
	writeDaemonGroupFile(t, plist, "<plist/>")
	db := dt.ctx.Layout.DB()
	writeDaemonGroupFile(t, db, "sqlite")

	if code := dt.run("uninstall"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if _, err := os.Stat(plist); !os.IsNotExist(err) {
		t.Fatalf("plist still present: %v", err)
	}
	if _, err := os.Stat(db); err != nil {
		t.Fatalf("state was removed: %v", err)
	}
	if n := len(dt.fake.CallsWithPrefix("launchctl", "bootout", "gui/501/zhuravel.magnum")); n != 1 {
		t.Fatalf("bootout calls = %d", n)
	}
	unlinks := dt.fake.CallsWithPrefix("herdr", "plugin", "unlink", "zhuravel.magnum")
	if len(unlinks) != 1 || !unlinks[0].Mutates {
		t.Fatalf("unlink calls = %+v", unlinks)
	}
	out := dt.stdout.String()
	for _, want := range []string{"removed " + plist, "unlinked", "kept " + dt.ctx.Layout.State()} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestUninstallIsIdempotent(t *testing.T) {
	dt := newDaemonGroupTest(t,
		daemonRuleExit([]string{"launchctl", "bootout"}, 3, "Boot-out failed: 3: No such process"),
		daemonRuleExit([]string{"launchctl", "print"}, 113, "Could not find service"),
		daemonRuleOK([]string{"herdr", "plugin", "list"}, herdrPluginListEmpty),
	)
	if code := dt.run("uninstall"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if calls := dt.fake.CallsWithPrefix("herdr", "plugin", "unlink"); len(calls) != 0 {
		t.Fatalf("unlinked a plugin that is not linked: %+v", calls)
	}
	if !strings.Contains(dt.stdout.String(), "not linked") {
		t.Fatalf("output: %s", dt.stdout)
	}
}

func TestUninstallDryRunChangesNothing(t *testing.T) {
	dt := newDaemonGroupTest(t)
	dt.fake.Rules = []execx.Rule{
		daemonRuleOK([]string{"herdr", "plugin", "list"}, herdrPluginListWith(dt.ctx.Layout.Home, true)),
	}
	plist := filepath.Join(dt.userHome, "Library", "LaunchAgents", "zhuravel.magnum.plist")
	writeDaemonGroupFile(t, plist, "<plist/>")
	if code := dt.run("uninstall", "--dry-run"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if _, err := os.Stat(plist); err != nil {
		t.Fatalf("dry run removed the plist: %v", err)
	}
	if calls := dt.fake.CallsWithPrefix("launchctl"); len(calls) != 0 {
		t.Fatalf("dry run ran launchctl: %+v", calls)
	}
	out := dt.stdout.String()
	for _, want := range []string{"would unload gui/501/zhuravel.magnum", "would run herdr plugin unlink zhuravel.magnum"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestUninstallHerdrUnavailableStillRemovesJob(t *testing.T) {
	dt := newDaemonGroupTest(t,
		daemonRuleOK([]string{"launchctl", "bootout"}, ""),
		daemonRuleExit([]string{"herdr", "plugin", "list"}, 1, "error: herdr server is not running"),
	)
	plist := filepath.Join(dt.userHome, "Library", "LaunchAgents", "zhuravel.magnum.plist")
	writeDaemonGroupFile(t, plist, "<plist/>")
	if code := dt.run("uninstall"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if _, err := os.Stat(plist); !os.IsNotExist(err) {
		t.Fatalf("plist still present: %v", err)
	}
	if !strings.Contains(dt.stderr.String(), "herdr plugin unlink zhuravel.magnum") {
		t.Fatalf("stderr lacks the fix: %s", dt.stderr)
	}
}
