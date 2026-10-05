package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/launchd"
	"github.com/zhuravel/magnum/internal/paths"
)

// installRules fakes launchd as it behaves: the job runs (pid 4242) until
// bootout unloads it, and bootstrap loads it again; launchd.Install may poll
// `launchctl print` until the bootout took effect.
func installRules(pluginList string) []execx.Rule {
	loaded := true
	return []execx.Rule{
		daemonRuleOK([]string{"/bin/sh", "-c", "command -v mise"}, "/opt/homebrew/bin/mise\n"),
		{Prefix: []string{"launchctl", "bootout"}, Fn: func(execx.Cmd) (execx.Result, error) {
			loaded = false
			return execx.Result{}, nil
		}},
		daemonRuleOK([]string{"launchctl", "enable"}, ""),
		{Prefix: []string{"launchctl", "bootstrap"}, Fn: func(execx.Cmd) (execx.Result, error) {
			loaded = true
			return execx.Result{}, nil
		}},
		{Prefix: []string{"launchctl", "print"}, Fn: func(c execx.Cmd) (execx.Result, error) {
			if !loaded {
				res := execx.Result{Code: 113, Stderr: []byte("Could not find service")}
				return res, &execx.ExitError{Cmd: c, Code: 113, Stderr: string(res.Stderr)}
			}
			return execx.Result{Stdout: []byte(launchctlPrint("running", 4242))}, nil
		}},
		daemonRuleOK([]string{"herdr", "plugin", "list"}, pluginList),
		daemonRuleOK([]string{"herdr", "plugin", "link"}, ""),
	}
}

func TestInstallWritesPlistLoadsItAndLinksPlugin(t *testing.T) {
	dt := newDaemonGroupTest(t, installRules(herdrPluginListEmpty)...)
	dt.useMiseKey(t)
	dt.writeBinary(t)
	if code := dt.run("install", "--plugin"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	l := dt.ctx.Layout

	// State tree exists (launchd needs the log directory to exist).
	if fi, err := os.Stat(l.Logs()); err != nil || !fi.IsDir() {
		t.Fatalf("logs dir not created: %v", err)
	}

	plistPath := filepath.Join(dt.userHome, "Library", "LaunchAgents", "zhuravel.magnum.plist")
	b, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("plist not written: %v", err)
	}
	plist := string(b)
	for _, want := range []string{
		"<string>/opt/homebrew/bin/mise</string>\n\t\t<string>-C</string>\n\t\t<string>" + l.Home + "</string>\n\t\t<string>exec</string>\n\t\t<string>--</string>\n\t\t<string>" + l.Binary() + "</string>\n\t\t<string>daemon</string>",
		"<key>WorkingDirectory</key>\n\t<string>" + l.Home + "</string>",
		"<key>PATH</key>\n\t\t<string>" + dt.userHome + "/.local/bin:" + dt.userHome + "/.local/share/mise/shims:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>",
		"<key>HOME</key>\n\t\t<string>" + dt.userHome + "</string>",
		"<key>HERDR_SOCKET_PATH</key>\n\t\t<string>/tmp/herdr-test.sock</string>",
		"<key>MAGNUM_HOME</key>\n\t\t<string>" + l.Home + "</string>",
		"<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>",
		"<key>ThrottleInterval</key>\n\t<integer>10</integer>",
		"<key>StandardOutPath</key>\n\t<string>" + filepath.Join(l.Logs(), "launchd.log") + "</string>",
		"<key>StandardErrorPath</key>\n\t<string>" + filepath.Join(l.Logs(), "launchd.log") + "</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q:\n%s", want, plist)
		}
	}
	if strings.Contains(plist, l.DaemonLog()) {
		t.Errorf("launchd must not write the daemon's own log %s:\n%s", l.DaemonLog(), plist)
	}
	if strings.Contains(plist, "MAGNUM_CONFIG") {
		t.Errorf("default config must not be pinned in the plist")
	}

	if n := len(dt.fake.CallsWithPrefix("launchctl", "bootstrap", "gui/501", plistPath)); n != 1 {
		t.Fatalf("bootstrap calls = %d", n)
	}
	links := dt.fake.CallsWithPrefix("herdr", "plugin", "link", l.Home)
	if len(links) != 1 || !links[0].Mutates {
		t.Fatalf("herdr plugin link calls = %+v", links)
	}
	if got := links[0].Env["HERDR_SOCKET_PATH"]; got != "/tmp/herdr-test.sock" {
		t.Fatalf("herdr env socket = %q", got)
	}

	out := dt.stdout.String()
	for _, want := range []string{"pid 4242", l.DaemonLog(), filepath.Join(l.Logs(), "launchd.log"), "linked", `f="` + l.TabBar() + `"`} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestInstallDryRunChangesNothing(t *testing.T) {
	dt := newDaemonGroupTest(t, installRules(herdrPluginListEmpty)...)
	if code := dt.run("install", "--plugin", "--dry-run"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if _, err := os.Stat(dt.ctx.Layout.State()); !os.IsNotExist(err) {
		t.Fatalf("dry run created the state dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dt.userHome, "Library")); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote under ~/Library: %v", err)
	}
	if calls := dt.fake.CallsWithPrefix("launchctl"); len(calls) != 0 {
		t.Fatalf("dry run ran launchctl: %+v", calls)
	}
	if calls := dt.fake.CallsWithPrefix("herdr", "plugin", "link"); len(calls) != 0 {
		t.Fatalf("dry run linked the plugin: %+v", calls)
	}
	out := dt.stdout.String()
	for _, want := range []string{
		"would create " + dt.ctx.Layout.State(),
		"missing", // the binary is not built yet: a warning under --dry-run
		"would write " + filepath.Join(dt.userHome, "Library", "LaunchAgents", "zhuravel.magnum.plist"),
		"<key>Label</key>",
		"launchctl bootstrap gui/501",
		"would run herdr plugin link " + dt.ctx.Layout.Home,
		"tab_bar_right",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestInstallPluginAlreadyLinkedIsANoop(t *testing.T) {
	dt := newDaemonGroupTest(t)
	dt.fake.Rules = installRules(herdrPluginListWith(dt.ctx.Layout.Home, true))
	if code := dt.run("install", "--plugin", "--no-launchd"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if calls := dt.fake.CallsWithPrefix("herdr", "plugin", "link"); len(calls) != 0 {
		t.Fatalf("relinked an already linked plugin: %+v", calls)
	}
	if calls := dt.fake.CallsWithPrefix("launchctl"); len(calls) != 0 {
		t.Fatalf("--no-launchd ran launchctl: %+v", calls)
	}
	if _, err := os.Stat(filepath.Join(dt.userHome, "Library")); !os.IsNotExist(err) {
		t.Fatalf("--no-launchd wrote a plist: %v", err)
	}
	if !strings.Contains(dt.stdout.String(), "already linked") {
		t.Fatalf("output: %s", dt.stdout)
	}
}

func TestInstallPluginDisabledSaysHowToEnable(t *testing.T) {
	dt := newDaemonGroupTest(t)
	dt.fake.Rules = installRules(herdrPluginListWith(dt.ctx.Layout.Home, false))
	if code := dt.run("install", "--plugin", "--no-launchd"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if !strings.Contains(dt.stdout.String(), "herdr plugin enable zhuravel.magnum") {
		t.Fatalf("output: %s", dt.stdout)
	}
}

func TestInstallPluginLinkedElsewhereRefuses(t *testing.T) {
	dt := newDaemonGroupTest(t)
	dt.fake.Rules = installRules(herdrPluginListWith("/somewhere/else", true))
	if code := dt.run("install", "--plugin", "--no-launchd"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if calls := dt.fake.CallsWithPrefix("herdr", "plugin", "link"); len(calls) != 0 {
		t.Fatalf("linked over another checkout: %+v", calls)
	}
	if !strings.Contains(dt.stderr.String(), "herdr plugin unlink zhuravel.magnum") {
		t.Fatalf("stderr lacks the fix: %s", dt.stderr)
	}
}

func TestInstallWithoutBinaryNamesTheBuild(t *testing.T) {
	dt := newDaemonGroupTest(t, installRules(herdrPluginListEmpty)...)
	if code := dt.run("install"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(dt.stderr.String(), "make build") {
		t.Fatalf("stderr lacks the fix: %s", dt.stderr)
	}
	if calls := dt.fake.CallsWithPrefix("launchctl"); len(calls) != 0 {
		t.Fatalf("loaded a job without a binary: %+v", calls)
	}
}

func TestInstallFallsBackToHomebrewMiseAndPinsCustomConfig(t *testing.T) {
	dt := newDaemonGroupTest(t)
	rules := installRules(herdrPluginListEmpty)
	rules[0] = daemonRuleExit([]string{"/bin/sh", "-c", "command -v mise"}, 1, "")
	dt.fake.Rules = append(rules, daemonRuleOK([]string{dt.ctx.Layout.Binary(), "config", "--config", "/etc/magnum/custom.toml"}, ""))
	dt.useMiseKey(t)
	dt.writeBinary(t)
	dt.ctx.cfgPath = "/etc/magnum/custom.toml"
	if code := dt.run("install"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	b, err := os.ReadFile(filepath.Join(dt.userHome, "Library", "LaunchAgents", "zhuravel.magnum.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "<string>/opt/homebrew/bin/mise</string>") {
		t.Fatalf("mise fallback missing:\n%s", b)
	}
	if !strings.Contains(string(b), "<key>MAGNUM_CONFIG</key>\n\t\t<string>/etc/magnum/custom.toml</string>") {
		t.Fatalf("custom config not pinned:\n%s", b)
	}
	if calls := dt.fake.CallsWithPrefix("herdr"); len(calls) != 0 {
		t.Fatalf("plugin touched without --plugin: %+v", calls)
	}
}

func TestInstallLaunchdFailureNamesTheFix(t *testing.T) {
	dt := newDaemonGroupTest(t)
	rules := installRules(herdrPluginListEmpty)
	rules[3] = daemonRuleExit([]string{"launchctl", "bootstrap"}, 37, "Bootstrap failed: 37: Operation already in progress")
	dt.fake.Rules = rules
	dt.writeBinary(t)
	if code := dt.run("install", "--plugin"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(dt.stderr.String(), "fix:") {
		t.Fatalf("stderr lacks a fix: %s", dt.stderr)
	}
	if calls := dt.fake.CallsWithPrefix("herdr", "plugin", "link"); len(calls) != 0 {
		t.Fatalf("went on to link the plugin after launchd failed: %+v", calls)
	}
}

// The daemon's plist carries the installing shell's SSH agent socket, so
// fetches work for users whose keys are not in Apple's agent; Apple's own
// listener is left to launchd.
func TestPlistCarriesTheShellsSSHAgentSocket(t *testing.T) {
	layout := paths.Layout{Home: "/Users/x/Projects/magnum"}
	t.Setenv("SSH_AUTH_SOCK", "/Users/x/Library/Group Containers/agent.sock")
	env := magnumPlistOptions(layout, "/Users/x", nil, "/tmp/herdr.sock", "").Env
	if env["SSH_AUTH_SOCK"] != "/Users/x/Library/Group Containers/agent.sock" {
		t.Fatalf("SSH_AUTH_SOCK = %q", env["SSH_AUTH_SOCK"])
	}
	t.Setenv("SSH_AUTH_SOCK", "/private/tmp/com.apple.launchd.abc/Listeners")
	if _, ok := magnumPlistOptions(layout, "/Users/x", nil, "/tmp/herdr.sock", "").Env["SSH_AUTH_SOCK"]; ok {
		t.Fatal("Apple's launchd listener must not be pinned into the plist")
	}
	t.Setenv("SSH_AUTH_SOCK", "")
	if _, ok := magnumPlistOptions(layout, "/Users/x", nil, "/tmp/herdr.sock", "").Env["SSH_AUTH_SOCK"]; ok {
		t.Fatal("no socket in the shell, none in the plist")
	}
}

// launchd (and the gh shim) go through mise only while an App's key comes
// from the checkout's mise environment; with key files, or installed
// without a checkout, the binary runs itself (Homebrew's stable bin/ link,
// not the versioned Cellar path). MAGNUM_HOME is set only for the checkout
// layout, which it selects.
func TestLaunchRunsTheBinaryDirectlyUnlessAKeyComesFromMise(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".mise.toml"), []byte("[env]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Identities: []config.Identity{{Name: "app", Kind: "app", PrivateKeyEnv: "K"}}}
	checkout := paths.Layout{Home: repo}
	if got := launchPrefix(checkout, cfg, "/m/mise"); !slices.Equal(got, []string{"/m/mise", "-C", repo, "exec", "--"}) {
		t.Fatalf("an env key: %q", got)
	}
	cfg.Identities[0].PrivateKeyFile = "/k/app.pem"
	if got := launchPrefix(checkout, cfg, "/m/mise"); got != nil {
		t.Fatalf("a key file: %q", got)
	}

	installed := paths.Layout{DataDir: "/d/magnum", StateDir: "/s/magnum", Exe: "/opt/homebrew/Cellar/magnum/1.0.0/bin/magnum"}
	if got := launchPrefix(installed, cfg, "/m/mise"); got != nil {
		t.Fatalf("no checkout: %q", got)
	}
	o := magnumPlistOptions(installed, "/Users/x", nil, "/tmp/h.sock", "")
	if !slices.Equal(o.ProgramArguments, []string{"/opt/homebrew/bin/magnum", "daemon"}) || o.WorkingDir != "/Users/x" ||
		o.StdoutPath != launchd.LogPath("/s/magnum/logs") {
		t.Fatalf("installed: %+v", o)
	}
	if _, ok := o.Env["MAGNUM_HOME"]; ok {
		t.Fatal("an installed layout pinned MAGNUM_HOME")
	}
	if got := magnumPlistOptions(checkout, "/Users/x", nil, "/tmp/h.sock", "").Env["MAGNUM_HOME"]; got != repo {
		t.Fatalf("checkout layout: MAGNUM_HOME = %q", got)
	}
}

// An installed binary has no checkout to link: `install --plugin` writes the
// plugin it embeds under the data directory and links that copy.
func TestInstallPluginWritesTheEmbeddedPluginWithoutACheckout(t *testing.T) {
	dt := newDaemonGroupTest(t, installRules(herdrPluginListEmpty)...)
	data := t.TempDir()
	dt.ctx.Layout = paths.Layout{DataDir: data, StateDir: t.TempDir(), Exe: filepath.Join(t.TempDir(), "magnum")}
	if code := dt.run("install", "--plugin", "--no-launchd"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	dir := filepath.Join(data, "herdr-plugin")
	for rel, want := range map[string][]byte{"herdr-plugin.toml": magnum.PluginManifest, "scripts/magnum-ctl.sh": magnum.PluginScript} {
		if b, err := os.ReadFile(filepath.Join(dir, rel)); err != nil || !bytes.Equal(b, want) {
			t.Fatalf("%s: %v", rel, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "scripts", "magnum-ctl.sh")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("script mode: %v %v", fi.Mode(), err)
	}
	if links := dt.fake.CallsWithPrefix("herdr", "plugin", "link", dir); len(links) != 1 {
		t.Fatalf("herdr plugin link calls = %+v", dt.fake.CallsWithPrefix("herdr"))
	}
	if wrote, err := writePluginFiles(dir); wrote || err != nil {
		t.Fatalf("an unchanged plugin was rewritten: %v %v", wrote, err)
	}
}
