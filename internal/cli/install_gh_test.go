package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallGhWritesTheShim(t *testing.T) {
	dt := newDaemonGroupTest(t, installRules(herdrPluginListEmpty)...)
	dt.useMiseKey(t)
	dt.writeBinary(t)
	if code := dt.run("install", "--gh", "--no-launchd"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	l := dt.ctx.Layout
	path := filepath.Join(dt.userHome, ".local", "share", "gh", "extensions", "gh-magnum", "gh-magnum")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("shim not written: %v", err)
	}
	script := string(b)
	if !strings.HasPrefix(script, "#!/bin/sh\n") ||
		!strings.HasSuffix(script, "exec /opt/homebrew/bin/mise -C "+l.Home+" exec -- "+l.Binary()+" \"$@\"\n") ||
		strings.Contains(script, "MAGNUM_CONFIG") {
		t.Errorf("shim:\n%s", script)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("shim mode: %v %v", fi.Mode(), err)
	}
	if calls := dt.fake.CallsWithPrefix("launchctl"); len(calls) != 0 {
		t.Errorf("--no-launchd ran launchctl: %+v", calls)
	}
	out := dt.stdout.String()
	for _, want := range []string{"wrote " + path, "gh magnum prs", filepath.Join(l.Home, "docs", "gh-dash.yml")} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	// Idempotent: a second run leaves it alone; a wrong mode is fixed.
	dt.stdout.Reset()
	if code := dt.run("install", "--gh", "--no-launchd"); code != 0 || !strings.Contains(dt.stdout.String(), "up to date") {
		t.Fatalf("second run: exit %d\n%s", code, dt.stdout)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	dt.stdout.Reset()
	if code := dt.run("install", "--gh", "--no-launchd"); code != 0 || !strings.Contains(dt.stdout.String(), "wrote") {
		t.Fatalf("mode repair: exit %d\n%s", code, dt.stdout)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode after repair %v", fi.Mode())
	}
}

func TestInstallGhHonorsGhDataDirAndConfig(t *testing.T) {
	dt := newDaemonGroupTest(t, installRules(herdrPluginListEmpty)...)
	dataDir := t.TempDir()
	cfgFile := filepath.Join(t.TempDir(), "my config.toml")
	daemonSys.Getenv = func(k string) string {
		switch k {
		case "GH_DATA_DIR":
			return dataDir
		case "MAGNUM_CONFIG":
			return cfgFile
		}
		return ""
	}
	if code := dt.run("install", "--gh", "--no-launchd"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	b, err := os.ReadFile(filepath.Join(dataDir, "extensions", "gh-magnum", "gh-magnum"))
	if err != nil {
		t.Fatalf("shim not under GH_DATA_DIR: %v", err)
	}
	if !strings.Contains(string(b), "MAGNUM_CONFIG='"+cfgFile+"'\nexport MAGNUM_CONFIG\n") {
		t.Errorf("custom config not pinned:\n%s", b)
	}
	if !strings.Contains(dt.stdout.String(), "missing") {
		t.Errorf("a missing binary is a warning:\n%s", dt.stdout)
	}

	daemonSys.Getenv = func(k string) string {
		if k == "XDG_DATA_HOME" {
			return dataDir
		}
		return ""
	}
	if got := ghExtensionsDir("/home/u"); got != filepath.Join(dataDir, "gh", "extensions") {
		t.Errorf("XDG_DATA_HOME: %s", got)
	}
	daemonSys.Getenv = func(string) string { return "" }
	if got := ghExtensionsDir("/home/u"); got != "/home/u/.local/share/gh/extensions" {
		t.Errorf("default: %s", got)
	}
}

func TestInstallGhLeavesForeignExtensionsAlone(t *testing.T) {
	dt := newDaemonGroupTest(t, installRules(herdrPluginListEmpty)...)
	dir := filepath.Join(dt.userHome, ".local", "share", "gh", "extensions", "gh-magnum")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := "#!/bin/sh\necho someone else's\n"
	if err := os.WriteFile(filepath.Join(dir, "gh-magnum"), []byte(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := dt.run("install", "--gh", "--no-launchd"); code != 1 || !strings.Contains(dt.stderr.String(), "gh extension remove magnum") {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "gh-magnum")); string(b) != foreign {
		t.Fatalf("overwrote a foreign extension:\n%s", b)
	}

	// A symlinked (gh extension install .) extension is refused too.
	dt2 := newDaemonGroupTest(t, installRules(herdrPluginListEmpty)...)
	ext := filepath.Join(dt2.userHome, ".local", "share", "gh", "extensions")
	if err := os.MkdirAll(ext, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(ext, "gh-magnum")); err != nil {
		t.Fatal(err)
	}
	if code := dt2.run("install", "--gh", "--no-launchd"); code != 1 {
		t.Fatalf("symlinked extension: exit %d, stderr: %s", code, dt2.stderr)
	}
}

func TestInstallGhDryRunWritesNothing(t *testing.T) {
	dt := newDaemonGroupTest(t, installRules(herdrPluginListEmpty)...)
	dt.useMiseKey(t)
	if code := dt.run("install", "--gh", "--no-launchd", "--dry-run"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if _, err := os.Stat(filepath.Join(dt.userHome, ".local")); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote under ~/.local: %v", err)
	}
	if out := dt.stdout.String(); !strings.Contains(out, "would write "+filepath.Join(dt.userHome, ".local", "share", "gh", "extensions", "gh-magnum", "gh-magnum")) ||
		!strings.Contains(out, "exec /opt/homebrew/bin/mise") {
		t.Fatalf("dry run output:\n%s", out)
	}
}
