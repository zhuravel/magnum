package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum"
	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// hooksFixture is the inspect fixture with a user home of its own, under
// the test's temp dir: Claude's directory and the default Codex home are
// there, never the user's real ones (hooksSys points at it).
type hooksFixture struct {
	*inspFixture
	user, claude, codex string
}

func newHooksFixture(t *testing.T) *hooksFixture {
	t.Helper()
	storetest.Serial(t) // swaps hooksSys
	f := newInspFixture(t)
	if err := f.Ctx.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(f.Home, "user")
	if err := os.MkdirAll(user, 0o700); err != nil {
		t.Fatal(err)
	}
	prev := hooksSys
	hooksSys = hooksSystem{UserHome: func() (string, error) { return user, nil }, Getenv: func(string) string { return "" }}
	agents.SetStaleCodexTrustDir(hooksStaleTrustDir(f.Ctx.Layout)) // as Main sets it for the process
	t.Cleanup(func() { hooksSys = prev; agents.SetStaleCodexTrustDir("") })
	return &hooksFixture{inspFixture: f, user: user, claude: filepath.Join(user, ".claude"), codex: filepath.Join(user, ".codex")}
}

func (f *hooksFixture) claudeScript() string   { return filepath.Join(f.claude, "hooks", hookScriptName) }
func (f *hooksFixture) claudeSettings() string { return filepath.Join(f.claude, "settings.json") }
func (f *hooksFixture) codexScript() string    { return filepath.Join(f.codex, hookScriptName) }
func (f *hooksFixture) codexHooks() string     { return filepath.Join(f.codex, "hooks.json") }

// write writes content to path, making its directory.
func (f *hooksFixture) write(path, content string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// read is path's content; "" when it does not exist.
func (f *hooksFixture) read(path string) string {
	f.t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

// entries are magnum's entries of agent in the fixture's home.
func (f *hooksFixture) entries(agent string) []hookEntry {
	if agent == agenthooks.AgentClaude {
		return testEntries(agent, f.claudeScript(), claudeHookEvents)
	}
	return testEntries(agent, f.codexScript(), codexHookEvents)
}

// allCurrent reports whether every entry of agent is in the settings file
// as the install writes it.
func (f *hooksFixture) allCurrent(agent, file string) bool {
	f.t.Helper()
	got, err := readHookEntries([]byte(f.read(file)), agent, f.entries(agent))
	if err != nil {
		f.t.Fatal(err)
	}
	for _, e := range got {
		if e.state != entryCurrent {
			return false
		}
	}
	return len(got) > 0
}

// The install writes each agent's script (mode 0755, the build's own) and
// appends magnum's entries to Claude's settings.json, keeping herdr's
// entries byte for byte, and to the Codex home's hooks.json, which it
// makes; it ends with Codex's one-time trust step.
func TestHooksInstallWritesTheScriptsAndAppendsTheEntries(t *testing.T) {
	f := newHooksFixture(t)
	f.write(f.claudeSettings(), herdrSettings)
	if code := f.run("hooks", "install"); code != 0 {
		t.Fatalf("install = %d\nstdout:\n%s\nstderr:\n%s", code, f.Out, f.Err)
	}
	out := f.Out.String()
	for _, agent := range []string{agenthooks.AgentClaude, agenthooks.AgentCodex} {
		path := f.claudeScript()
		if agent == agenthooks.AgentCodex {
			path = f.codexScript()
		}
		want, err := agenthooks.Script(magnum.AgentHookScript, agent)
		if err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != 0o755 || f.read(path) != string(want) {
			t.Fatalf("%s script = %v %v, want the build's at 0755", agent, err, fi)
		}
	}
	if !f.allCurrent(agenthooks.AgentClaude, f.claudeSettings()) || !f.allCurrent(agenthooks.AgentCodex, f.codexHooks()) {
		t.Fatalf("entries not installed:\n%s\n%s", f.read(f.claudeSettings()), f.read(f.codexHooks()))
	}
	back, _, err := unspliceHooks([]byte(f.read(f.claudeSettings())), agenthooks.AgentClaude)
	if err != nil || string(back) != herdrSettings {
		t.Fatalf("herdr's entries did not stay byte for byte: %v\n%s", err, back)
	}
	for _, want := range []string{"claude: current (v1) (" + f.claudeScript() + ")", "added Stop, StopFailure, PermissionRequest, SessionStart, SessionEnd",
		"choose Trust all (or `/hooks`), or `magnum resume --tool codex`"} {
		if !strings.Contains(out, want) {
			t.Errorf("install output lacks %q:\n%s", want, out)
		}
	}
	// Again: nothing to change.
	claude, codex := f.read(f.claudeSettings()), f.read(f.codexHooks())
	if code := f.run("hooks", "install"); code != 0 || f.read(f.claudeSettings()) != claude || f.read(f.codexHooks()) != codex ||
		!strings.Contains(f.Out.String(), "nothing to change") {
		t.Fatalf("second install = %d\n%s%s", code, f.Out, f.Err)
	}
}

// A file at the script's path without magnum's marker is someone else's:
// the install refuses it and changes nothing anywhere, settings included.
func TestHooksInstallRefusesAForeignScriptAndChangesNothing(t *testing.T) {
	f := newHooksFixture(t)
	f.write(f.claudeSettings(), herdrSettings)
	f.write(f.claudeScript(), "#!/bin/sh\necho mine\n")
	for _, args := range [][]string{{"install", "claude"}, {"install"}} {
		code := f.run("hooks", args...)
		if code != 1 || !strings.Contains(f.Err.String(), f.claudeScript()) || !strings.Contains(f.Err.String(), "not magnum's") {
			t.Fatalf("%v = %d\n%s%s", args, code, f.Out, f.Err)
		}
		if f.read(f.claudeScript()) != "#!/bin/sh\necho mine\n" || f.read(f.claudeSettings()) != herdrSettings ||
			f.read(f.codexHooks()) != "" || f.read(f.codexScript()) != "" {
			t.Fatalf("%v changed a file", args)
		}
	}
}

// Settings that are no JSON are refused before anything is written: no
// script, no settings change.
func TestHooksInstallRefusesSettingsThatAreNotJSONAndWritesNothing(t *testing.T) {
	f := newHooksFixture(t)
	broken := "{\n  \"hooks\": {\n"
	f.write(f.claudeSettings(), broken)
	if code := f.run("hooks", "install", "claude"); code != 1 || !strings.Contains(f.Err.String(), "is not valid JSON") {
		t.Fatalf("install = %d\n%s%s", code, f.Out, f.Err)
	}
	if f.read(f.claudeSettings()) != broken || f.read(f.claudeScript()) != "" {
		t.Fatal("the install wrote something")
	}
}

// A dry run prints each file and what it would add or change, and writes
// nothing; an uninstall's dry run removes nothing.
func TestHooksDryRunWritesNothing(t *testing.T) {
	f := newHooksFixture(t)
	f.write(f.claudeSettings(), herdrSettings)
	if code := f.run("hooks", "install", "--dry-run"); code != 0 {
		t.Fatalf("dry run = %d\n%s%s", code, f.Out, f.Err)
	}
	out := f.Out.String()
	for _, want := range []string{"dry-run: claude: would write " + f.claudeScript(), "dry-run: claude: would add to " + f.claudeSettings() + ": Stop,",
		"dry-run: codex: would write " + f.codexHooks(), "sh '" + f.codexScript() + "' codex interrupt"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run lacks %q:\n%s", want, out)
		}
	}
	if f.read(f.claudeSettings()) != herdrSettings || f.read(f.claudeScript()) != "" || f.read(f.codexHooks()) != "" || f.read(f.codexScript()) != "" {
		t.Fatal("the dry run wrote something")
	}
	if code := f.run("hooks", "install"); code != 0 {
		t.Fatal(f.Err)
	}
	installed := f.read(f.claudeSettings())
	if code := f.run("hooks", "uninstall", "--dry-run"); code != 0 || f.read(f.claudeSettings()) != installed || f.read(f.claudeScript()) == "" ||
		!strings.Contains(f.Out.String(), "dry-run: claude: would remove") {
		t.Fatalf("uninstall dry run = %d\n%s%s", code, f.Out, f.Err)
	}
}

// A drifted entry is rewritten in place, and for Codex the install says
// that Codex asks for its trust again.
func TestHooksInstallRewritesADriftedEntryAndSaysCodexAsksAgain(t *testing.T) {
	f := newHooksFixture(t)
	if code := f.run("hooks", "install", "codex"); code != 0 {
		t.Fatal(f.Err)
	}
	f.write(f.codexHooks(), strings.Replace(f.read(f.codexHooks()), `"timeout": 5`, `"timeout": 9`, 1))
	if code := f.run("hooks", "install", "codex"); code != 0 || !strings.Contains(f.Out.String(), "rewrote Stop") ||
		!strings.Contains(f.Out.String(), "Codex will ask for trust again") {
		t.Fatalf("install = %d\n%s%s", code, f.Out, f.Err)
	}
	if !f.allCurrent(agenthooks.AgentCodex, f.codexHooks()) {
		t.Fatalf("not rewritten:\n%s", f.read(f.codexHooks()))
	}
}

// Status prints one line per target in herdr's form and exits 1 unless
// every target is current: missing, current, outdated, modified (a foreign
// script, a drifted entry), incomplete (an event without the entry) and,
// for Codex, untrusted until each entry has a trust entry.
func TestHooksStatusSaysEveryStateAndExitsOneUnlessCurrent(t *testing.T) {
	f := newHooksFixture(t)
	status := func(target string) (int, string) {
		t.Helper()
		code := f.run("hooks", "status", target)
		return code, strings.TrimSpace(f.Out.String())
	}
	if code, out := status("claude"); code != 1 || out != "claude: missing ("+f.claudeScript()+")" {
		t.Fatalf("missing = %d %q", code, out)
	}
	if code := f.run("hooks", "install"); code != 0 {
		t.Fatal(f.Err)
	}
	if code, out := status("claude"); code != 0 || out != "claude: current (v1) ("+f.claudeScript()+")" {
		t.Fatalf("current = %d %q", code, out)
	}
	script := f.read(f.claudeScript())
	f.write(f.claudeScript(), strings.Replace(script, "MAGNUM_HOOK_VERSION=1", "MAGNUM_HOOK_VERSION=0", 1))
	if code, out := status("claude"); code != 1 || out != "claude: outdated (v0 < v1) ("+f.claudeScript()+")" {
		t.Fatalf("outdated = %d %q", code, out)
	}
	f.write(f.claudeScript(), "#!/bin/sh\necho mine\n")
	if code, out := status("claude"); code != 1 || !strings.HasPrefix(out, "claude: modified ("+f.claudeScript()+"): ") || !strings.Contains(out, "not magnum's") {
		t.Fatalf("foreign = %d %q", code, out)
	}
	f.write(f.claudeScript(), script)
	settings := f.read(f.claudeSettings())
	f.write(f.claudeSettings(), strings.Replace(settings, "claude stop_failure", "claude stop", 1))
	if code, out := status("claude"); code != 1 || !strings.HasPrefix(out, "claude: modified (v1) (") || !strings.Contains(out, "StopFailure") {
		t.Fatalf("drifted = %d %q", code, out)
	}
	partial, _, err := spliceHooks(nil, agenthooks.AgentClaude, f.entries(agenthooks.AgentClaude)[:3])
	if err != nil {
		t.Fatal(err)
	}
	f.write(f.claudeSettings(), string(partial))
	if code, out := status("claude"); code != 1 || !strings.HasPrefix(out, "claude: incomplete (v1) (") || !strings.Contains(out, "no entry for SessionStart, SessionEnd in "+f.claudeSettings()) {
		t.Fatalf("incomplete = %d %q", code, out)
	}
	f.write(f.claudeSettings(), settings)
	if err := os.Remove(f.claudeScript()); err != nil {
		t.Fatal(err)
	}
	if code, out := status("claude"); code != 1 || out != "claude: modified ("+f.claudeScript()+"): the entries in "+f.claudeSettings()+
		" run a script that is missing, so each event's hook fails" {
		t.Fatalf("entries without the script = %d %q", code, out)
	}
	f.write(f.claudeScript(), script)

	// Codex: untrusted until config.toml has a trust entry for each of
	// magnum's places.
	if code, out := status("codex"); code != 1 || !strings.HasPrefix(out, "codex: untrusted (v1) ("+f.codexScript()+"): ") ||
		!strings.Contains(out, "stop:0:0") {
		t.Fatalf("untrusted = %d %q", code, out)
	}
	var state strings.Builder
	for _, e := range codexHookEvents {
		state.WriteString("[hooks.state.\"" + f.codexHooks() + ":" + e.arg + ":0:0\"]\ntrusted_hash = \"sha256:1\"\n")
	}
	f.write(filepath.Join(f.codex, "config.toml"), state.String())
	if code, out := status("codex"); code != 0 || out != "codex: current (v1) ("+f.codexScript()+")" {
		t.Fatalf("trusted = %d %q", code, out)
	}
	if code := f.run("hooks", "status"); code != 0 || strings.Count(f.Out.String(), "\n") != 2 {
		t.Fatalf("all = %d %q", code, f.Out)
	}
}

// Uninstall removes magnum's handlers and its scripts. When a Codex hook
// of another tool would move to another place (and lose its trust), it
// names the hook and asks y/N: without a terminal it refuses unless --yes,
// and n leaves every file as it was.
func TestHooksUninstallAsksBeforeMovingACodexHooksPlace(t *testing.T) {
	f := newHooksFixture(t)
	if code := f.run("hooks", "install"); code != 0 {
		t.Fatal(f.Err)
	}
	other := `{"hooks": [{"type": "command", "command": "/opt/homebrew/bin/moshi-hook codex-hook"}]}`
	hooks := strings.Replace(f.read(f.codexHooks()), "\n    ]\n  }\n}", ",\n      "+other+"\n    ]\n  }\n}", 1) // after Interrupt's group
	if hooks == f.read(f.codexHooks()) {
		t.Fatalf("could not add a group after magnum's:\n%s", hooks)
	}
	f.write(f.codexHooks(), hooks)
	unchanged := func() bool {
		return f.read(f.codexHooks()) == hooks && f.read(f.codexScript()) != "" && f.read(f.claudeScript()) != "" && f.allCurrent(agenthooks.AgentClaude, f.claudeSettings())
	}

	if code := f.run("hooks", "uninstall"); code != 1 || !strings.Contains(f.Err.String(), "moshi-hook (interrupt:1:0 to interrupt:0:0)") ||
		!strings.Contains(f.Err.String(), "--yes") || !unchanged() {
		t.Fatalf("without a terminal = %d\n%s%s", code, f.Out, f.Err)
	}
	f.tty("n\n")
	if code := f.run("hooks", "uninstall"); code != 1 || !strings.Contains(f.Err.String(), "Remove them? [y/N]") || !unchanged() {
		t.Fatalf("n = %d\n%s%s", code, f.Out, f.Err)
	}
	f.tty("y\n")
	if code := f.run("hooks", "uninstall"); code != 0 {
		t.Fatalf("y = %d\n%s%s", code, f.Out, f.Err)
	}
	if strings.Contains(f.read(f.codexHooks()), hookScriptName) || !strings.Contains(f.read(f.codexHooks()), "moshi-hook") ||
		f.read(f.codexScript()) != "" || f.read(f.claudeScript()) != "" || strings.Contains(f.read(f.claudeSettings()), hookScriptName) {
		t.Fatalf("after y:\n%s\n%s", f.read(f.codexHooks()), f.read(f.claudeSettings()))
	}

	// --yes answers for a script, with no terminal.
	f.tty("")
	inspIsTTY = func() bool { return false }
	if code := f.run("hooks", "install"); code != 0 {
		t.Fatal(f.Err)
	}
	f.write(f.codexHooks(), strings.Replace(f.read(f.codexHooks()), `"Interrupt": [`, `"Interrupt": [`+other+",", 1)) // before magnum's: no move
	if code := f.run("hooks", "uninstall", "codex"); code != 0 || strings.Contains(f.read(f.codexHooks()), hookScriptName) {
		t.Fatalf("no move = %d\n%s%s", code, f.Out, f.Err)
	}
	if code := f.run("hooks", "install", "codex"); code != 0 {
		t.Fatal(f.Err)
	}
	f.write(f.codexHooks(), strings.Replace(f.read(f.codexHooks()), "\n    ]\n  }\n}", ",\n      "+other+"\n    ]\n  }\n}", 1))
	if code := f.run("hooks", "uninstall", "codex", "--yes"); code != 0 || strings.Contains(f.read(f.codexHooks()), hookScriptName) {
		t.Fatalf("--yes = %d\n%s%s", code, f.Out, f.Err)
	}
}

// No test reaches the user's own ~/.claude or ~/.codex: the defaults of
// hooksSys refuse the home and read no env in a test binary, so a command
// run without the fixture's home fails before it reads a file.
func TestHooksNeverReachTheRealHomeInATest(t *testing.T) {
	storetest.Serial(t)
	t.Setenv("CODEX_HOME", "/nonexistent/codex")
	t.Setenv("CLAUDE_CONFIG_DIR", "/nonexistent/claude")
	if _, err := hooksUserHome(); err == nil {
		t.Fatal("the default home answered in a test binary")
	}
	if v := hooksGetenv("CODEX_HOME"); v != "" {
		t.Fatalf("the default env read CODEX_HOME = %q", v)
	}
	f := newInspFixture(t)
	if code := f.run("hooks", "status"); code != 1 || !strings.Contains(f.Err.String(), "a test never uses the user's home") {
		t.Fatalf("status with the defaults = %d\n%s%s", code, f.Out, f.Err)
	}
	// The fixture's home is under the test's temp dir.
	h := newHooksFixture(t)
	if home, _ := hooksSys.UserHome(); !strings.HasPrefix(home, h.Home+string(filepath.Separator)) {
		t.Fatalf("the fixture's home %s is not under %s", home, h.Home)
	}
	if len(testRealHomes) == 0 {
		t.Fatal("TestMain recorded no real home")
	}
	if home, _ := os.UserHomeDir(); slices.Contains(testRealHomes, home) {
		t.Fatalf("the test binary's HOME %s is the user's real home", home)
	}
	if code := h.run("hooks", "install"); code != 0 {
		t.Fatal(h.Err)
	}
	for _, realHome := range testRealHomes {
		for _, path := range []string{h.claudeScript(), h.claudeSettings(), h.codexScript(), h.codexHooks()} {
			for _, own := range []string{".claude", ".codex"} {
				if under(path, filepath.Join(realHome, own)) {
					t.Fatalf("the hooks commands wrote %s, under the real %s", path, filepath.Join(realHome, own))
				}
			}
		}
		if out := h.Out.String(); strings.Contains(out, filepath.Join(realHome, ".claude")) || strings.Contains(out, filepath.Join(realHome, ".codex")) {
			t.Fatalf("the install names a real agent home:\n%s", out)
		}
	}
}

// under reports whether path is dir or inside it.
func under(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// After an in-place rewrite of magnum's Codex entry, the entry keeps its
// place and Codex's old trusted_hash there, which Codex no longer accepts:
// install records it, and status says untrusted (exit 1) until the
// operator's Trust all writes a new hash; then it is current again.
func TestHooksARewrittenCodexEntryIsUntrustedUntilCodexTrustsItAgain(t *testing.T) {
	f := newHooksFixture(t)
	if code := f.run("hooks", "install", "codex"); code != 0 {
		t.Fatal(f.Err)
	}
	trust := func(hash string) {
		var state strings.Builder
		for _, e := range codexHookEvents {
			h := "sha256:1"
			if e.arg == agenthooks.NameStop {
				h = hash
			}
			state.WriteString("[hooks.state.\"" + f.codexHooks() + ":" + e.arg + ":0:0\"]\ntrusted_hash = \"" + h + "\"\n")
		}
		f.write(filepath.Join(f.codex, "config.toml"), state.String())
	}
	trust("sha256:old")
	if code := f.run("hooks", "status", "codex"); code != 0 {
		t.Fatalf("trusted = %d %q", code, f.Out)
	}
	f.write(f.codexHooks(), strings.Replace(f.read(f.codexHooks()), `"async": true`, `"async": false`, 1)) // another tool drifted Stop
	if code := f.run("hooks", "install", "codex"); code != 0 {
		t.Fatal(f.Err)
	}
	out := f.Out.String()
	if !strings.Contains(out, "rewrote Stop") || !strings.Contains(out, "codex: untrusted (v1)") || strings.Contains(out, "codex: current") ||
		!strings.Contains(out, codexTrustStep) {
		t.Fatalf("install after the drift:\n%s", out)
	}
	code := f.run("hooks", "status", "codex")
	if line := strings.TrimSpace(f.Out.String()); code != 1 || !strings.HasPrefix(line, "codex: untrusted (v1) (") ||
		!strings.Contains(line, "stop:0:0 (its trust is of the hook before magnum rewrote it)") || !strings.Contains(line, codexForgetStep) {
		t.Fatalf("status after the rewrite = %d %q", code, line)
	}
	trust("sha256:new") // the operator chose Trust all
	if code := f.run("hooks", "status", "codex"); code != 0 || strings.TrimSpace(f.Out.String()) != "codex: current (v1) ("+f.codexScript()+")" {
		t.Fatalf("after a new trust = %d %q", code, f.Out)
	}
}

// A dangling symlink at the script's path or the settings' is never
// replaced by a file, and a script of a newer magnum is never downgraded:
// both are refused, with nothing changed.
func TestHooksInstallRefusesADanglingLinkAndANewerScript(t *testing.T) {
	f := newHooksFixture(t)
	if err := os.MkdirAll(filepath.Dir(f.claudeScript()), 0o700); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(f.Home, "dotfiles", "magnum-agent-hook.sh")
	if err := os.Symlink(gone, f.claudeScript()); err != nil {
		t.Fatal(err)
	}
	if code := f.run("hooks", "install", "claude"); code != 1 || !strings.Contains(f.Err.String(), "a link to "+gone+", which does not exist") {
		t.Fatalf("dangling script link = %d\n%s", code, f.Err)
	}
	if target, err := os.Readlink(f.claudeScript()); err != nil || target != gone || f.read(f.claudeSettings()) != "" {
		t.Fatalf("the link was replaced (%q, %v) or the settings written", target, err)
	}
	if err := os.Remove(f.claudeScript()); err != nil {
		t.Fatal(err)
	}
	goneSettings := filepath.Join(f.Home, "dotfiles", "settings.json")
	if err := os.Symlink(goneSettings, f.claudeSettings()); err != nil {
		t.Fatal(err)
	}
	if code := f.run("hooks", "install", "claude"); code != 1 || !strings.Contains(f.Err.String(), "a link to "+goneSettings+", which does not exist") {
		t.Fatalf("dangling settings link = %d\n%s", code, f.Err)
	}
	if target, err := os.Readlink(f.claudeSettings()); err != nil || target != goneSettings || f.read(f.claudeScript()) != "" {
		t.Fatalf("the settings link was replaced (%q, %v) or the script written", target, err)
	}
	if err := os.Remove(f.claudeSettings()); err != nil {
		t.Fatal(err)
	}

	newer := strings.Replace(string(hookTarget{agent: agenthooks.AgentClaude, dir: f.claude}.scriptBody()), "MAGNUM_HOOK_VERSION=1", "MAGNUM_HOOK_VERSION=2", 1)
	f.write(f.claudeScript(), newer)
	if code := f.run("hooks", "install", "claude"); code != 1 || !strings.Contains(f.Err.String(), "a newer magnum's (v2 > v1)") {
		t.Fatalf("newer script = %d\n%s", code, f.Err)
	}
	if f.read(f.claudeScript()) != newer || f.read(f.claudeSettings()) != "" {
		t.Fatal("the newer script was downgraded or the settings written")
	}
	if code := f.run("hooks", "status", "claude"); code != 1 || !strings.Contains(f.Out.String(), "the script is a newer magnum's (v2 > v1)") {
		t.Fatalf("status of a newer script = %d %q", code, f.Out)
	}
}

// changingReader changes a file the first time the question is answered,
// as Claude Code may write its settings while the operator thinks, then
// answers.
type changingReader struct {
	t       *testing.T
	path    string
	content string
	answer  *strings.Reader
	done    bool
}

func (r *changingReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		if err := os.WriteFile(r.path, []byte(r.content), 0o600); err != nil {
			r.t.Error(err)
		}
	}
	return r.answer.Read(p)
}

// Uninstall reads each file again after the y/N question and refuses when
// it changed meanwhile: it never writes back bytes planned before another
// tool's write.
func TestHooksUninstallRefusesAFileThatChangedWhileItAsked(t *testing.T) {
	f := newHooksFixture(t)
	if code := f.run("hooks", "install"); code != 0 {
		t.Fatal(f.Err)
	}
	other := `{"hooks": [{"type": "command", "command": "/opt/homebrew/bin/moshi-hook codex-hook"}]}`
	f.write(f.codexHooks(), strings.Replace(f.read(f.codexHooks()), "\n    ]\n  }\n}", ",\n      "+other+"\n    ]\n  }\n}", 1))
	changed := strings.Replace(f.read(f.claudeSettings()), "{", "{\n  \"model\": \"opus\",", 1) // Claude wrote a setting
	f.tty("")
	inspStdin = &changingReader{t: t, path: f.claudeSettings(), content: changed, answer: strings.NewReader("y\n")}
	codex := f.read(f.codexHooks())
	if code := f.run("hooks", "uninstall"); code != 1 || !strings.Contains(f.Err.String(), f.claudeSettings()+" changed while magnum asked") {
		t.Fatalf("uninstall = %d\n%s%s", code, f.Out, f.Err)
	}
	if f.read(f.claudeSettings()) != changed || f.read(f.codexHooks()) != codex || f.read(f.claudeScript()) == "" || f.read(f.codexScript()) == "" {
		t.Fatal("the uninstall changed a file after the refusal")
	}
}

// A home that is unknown or not an absolute path is refused by install,
// uninstall and status alike: the command would name a script relative to
// whatever directory the agent runs in.
func TestHooksRefuseAHomeThatIsNotAnAbsolutePath(t *testing.T) {
	t.Parallel()
	for _, dir := range []string{"", "codex", "./codex"} {
		target := hookTarget{agent: agenthooks.AgentCodex, dir: dir, users: []string{"codex-judge"}}
		if _, err := planHookInstall(target); err == nil || !strings.Contains(err.Error(), "codex-judge") ||
			!strings.Contains(err.Error(), "give the role an absolute CODEX_HOME in its env") {
			t.Errorf("install into %q = %v, want a refusal", dir, err)
		}
		if _, err := planHookRemoval(target); err == nil {
			t.Errorf("uninstall from %q took it", dir)
		}
		if s := readHookStatus(target); s.state != hookRefused || !strings.Contains(s.line(), "codex-judge") {
			t.Errorf("status of %q = %q, want refused", dir, s.line())
		}
	}
	// Claude's directory, and a Codex home no role sets, come from this
	// shell's env: the fix is there, not in a role's.
	for _, target := range []hookTarget{{agent: agenthooks.AgentClaude, dir: "claude-alt", fromShell: true},
		{agent: agenthooks.AgentCodex, dir: "codex-alt", fromShell: true}} {
		want := "set " + hookHomeEnv(target.agent) + " in this shell to an absolute path, or unset it"
		if _, err := planHookInstall(target); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s install = %v, want %q", target.agent, err, want)
		}
		if fix := hookHomeFix(target); fix != want {
			t.Errorf("%s fix = %q, want %q", target.agent, fix, want)
		}
	}
}

// When magnum rewrote its Codex entry back to the very hook Codex already
// trusts, Codex shows no review and never writes a new hash, so the record
// would hold the place untrusted for good: status names the way out, and
// `magnum hooks install codex --forget-stale` removes the records, which
// ends the hold's reason (agents.ReadCodexHooksTrust has nothing untrusted).
func TestHooksForgetStaleClearsARecordCodexWillNeverReplace(t *testing.T) {
	f := newHooksFixture(t)
	if code := f.run("hooks", "install", "codex"); code != 0 {
		t.Fatal(f.Err)
	}
	var state strings.Builder
	for _, e := range codexHookEvents {
		state.WriteString("[hooks.state.\"" + f.codexHooks() + ":" + e.arg + ":0:0\"]\ntrusted_hash = \"sha256:canonical-" + e.arg + "\"\n")
	}
	f.write(filepath.Join(f.codex, "config.toml"), state.String())
	// A tool drops async on magnum's Stop, and no Codex session trusts it.
	f.write(f.codexHooks(), strings.Replace(f.read(f.codexHooks()), `"async": true`, `"async": false`, 1))
	if code := f.run("hooks", "install", "codex"); code != 0 {
		t.Fatal(f.Err)
	}
	for range 2 {
		if code := f.run("hooks", "status", "codex"); code != 1 || !strings.Contains(f.Out.String(), codexForgetStep) {
			t.Fatalf("status of the stuck record = %d %q", code, f.Out)
		}
	}
	if trust, _, err := agents.ReadCodexHooksTrust(f.codex); err != nil || len(trust.Untrusted) != 1 {
		t.Fatalf("the hold's reason = %+v %v, want the one stale place", trust.Untrusted, err)
	}
	if code := f.run("hooks", "install", "codex", "--forget-stale", "--dry-run"); code != 0 || !strings.Contains(f.Out.String(), "dry-run: codex: would forget 1 stale trust record") {
		t.Fatalf("dry run = %d %q", code, f.Out)
	}
	if trust, _, _ := agents.ReadCodexHooksTrust(f.codex); len(trust.Untrusted) != 1 {
		t.Fatal("the dry run forgot the record")
	}
	if code := f.run("hooks", "install", "codex", "--forget-stale"); code != 0 || !strings.Contains(f.Out.String(), "codex: forgot 1 stale trust record") ||
		!strings.Contains(f.Out.String(), "codex: current (v1)") {
		t.Fatalf("--forget-stale = %d %q%s", code, f.Out, f.Err)
	}
	if trust, _, err := agents.ReadCodexHooksTrust(f.codex); err != nil || len(trust.Untrusted) != 0 {
		t.Fatalf("the hold's reason after --forget-stale = %+v %v", trust.Untrusted, err)
	}
}

// A rewrite Codex hashes the same as the hook it replaced (an Interrupt
// timeout of 0 is Codex's 1 s) records nothing, so status stays current.
func TestHooksARewriteCodexHashesTheSameRecordsNothing(t *testing.T) {
	f := newHooksFixture(t)
	if code := f.run("hooks", "install", "codex"); code != 0 {
		t.Fatal(f.Err)
	}
	var state strings.Builder
	for _, e := range codexHookEvents {
		state.WriteString("[hooks.state.\"" + f.codexHooks() + ":" + e.arg + ":0:0\"]\ntrusted_hash = \"sha256:1\"\n")
	}
	f.write(filepath.Join(f.codex, "config.toml"), state.String())
	hooks := f.read(f.codexHooks())
	i := strings.Index(hooks, `"Interrupt"`)
	f.write(f.codexHooks(), hooks[:i]+strings.Replace(hooks[i:], `"timeout": 1`, `"timeout": 0`, 1))
	if code := f.run("hooks", "install", "codex"); code != 0 || !strings.Contains(f.Out.String(), "rewrote Interrupt") ||
		!strings.Contains(f.Out.String(), "codex: current (v1)") || strings.Contains(f.Out.String(), "Codex will ask for trust again") {
		t.Fatalf("install = %d\n%s%s", code, f.Out, f.Err)
	}
	if entries, err := os.ReadDir(hooksStaleTrustDir(f.Ctx.Layout)); err == nil && len(entries) != 0 {
		t.Fatalf("records = %d, want none", len(entries))
	}
}

// Install reads each settings file again just before it writes, and
// refuses one another tool wrote since the plan read it: nothing is
// written, scripts included.
func TestHooksInstallRefusesASettingsFileThatChangedSinceItWasRead(t *testing.T) {
	f := newHooksFixture(t)
	f.write(f.claudeSettings(), herdrSettings)
	changed := strings.Replace(herdrSettings, `"model": "opus"`, `"model": "sonnet"`, 1)
	prev := hooksBeforeWrite
	hooksBeforeWrite = func() { f.write(f.claudeSettings(), changed) } // Claude wrote a setting meanwhile
	t.Cleanup(func() { hooksBeforeWrite = prev })
	if code := f.run("hooks", "install"); code != 1 || !strings.Contains(f.Err.String(), f.claudeSettings()+" changed since magnum read it") {
		t.Fatalf("install = %d\n%s%s", code, f.Out, f.Err)
	}
	if f.read(f.claudeSettings()) != changed || f.read(f.claudeScript()) != "" || f.read(f.codexScript()) != "" || f.read(f.codexHooks()) != "" {
		t.Fatal("the install wrote a file after the refusal")
	}
}

// A usage error names every flag of the subcommands, --forget-stale too.
func TestHooksUsageNamesForgetStale(t *testing.T) {
	f := newHooksFixture(t)
	if code := f.run("hooks", "install", "codex", "claude", "--forget-stale"); code != 2 || !strings.Contains(f.Err.String(), "install [--dry-run] [--forget-stale]") {
		t.Fatalf("usage = %d\n%s", code, f.Err)
	}
}

// A Codex hooks.json Codex skips (a root key it does not know) reads
// modified with the reason, and install refuses it and writes nothing.
func TestHooksStatusAndInstallNameAHooksJSONCodexSkips(t *testing.T) {
	f := newHooksFixture(t)
	f.write(f.codexHooks(), `{"x-note": 1, "hooks": {}}`)
	code := f.run("hooks", "status", "codex")
	if line := strings.TrimSpace(f.Out.String()); code != 1 || line != "codex: modified ("+f.codexScript()+`): Codex skips this file: unknown key "x-note" (`+f.codexHooks()+")" {
		t.Fatalf("status = %d %q", code, line)
	}
	if code := f.run("hooks", "install", "codex"); code != 1 || !strings.Contains(f.Err.String(), `Codex skips this file: unknown key "x-note"`) {
		t.Fatalf("install = %d\n%s%s", code, f.Out, f.Err)
	}
	if f.read(f.codexHooks()) != `{"x-note": 1, "hooks": {}}` || f.read(f.codexScript()) != "" {
		t.Fatal("the install wrote something")
	}
}

// trustPlaces writes the Codex home's config.toml with a trust entry of
// hash at each of places of its hooks.json.
func (f *hooksFixture) trustPlaces(hash string, places ...string) {
	f.t.Helper()
	var state strings.Builder
	for _, p := range places {
		state.WriteString("[hooks.state.\"" + f.codexHooks() + ":" + p + "\"]\ntrusted_hash = \"" + hash + "\"\n")
	}
	f.write(filepath.Join(f.codex, "config.toml"), state.String())
}

// magnumPlaces are the places of magnum's Codex entries the install wrote.
func (f *hooksFixture) magnumPlaces() []string {
	f.t.Helper()
	got, err := readHookEntries([]byte(f.read(f.codexHooks())), agenthooks.AgentCodex, f.entries(agenthooks.AgentCodex))
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, e := range got {
		out = append(out, e.places...)
	}
	return out
}

// A place magnum appends to can hold a trust entry a hook that once stood
// there left behind (Codex never removes one): it trusts that old hook,
// not magnum's, so Codex will ask. The install records it as it records
// the old trust of a rewrite, so status, doctor and the hold read the place
// untrusted until Codex writes a new hash; a place with no trust entry
// needs no record.
func TestHooksAnAppendOntoAPlaceWithAnOldTrustEntryReadsUntrusted(t *testing.T) {
	f := newHooksFixture(t)
	f.write(f.codexHooks(), `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "/opt/homebrew/bin/moshi-hook codex-hook"}]}]}}`)
	// Left behind: magnum's Stop lands at stop:1:0, its Interrupt at
	// interrupt:0:0; moshi's own entry is valid.
	f.trustPlaces("sha256:old", "stop:0:0", "stop:1:0", "interrupt:0:0")
	if code := f.run("hooks", "install", "codex"); code != 0 {
		t.Fatalf("install = %d\n%s%s", code, f.Out, f.Err)
	}
	if !strings.Contains(f.Out.String(), "codex: untrusted (v1)") {
		t.Fatalf("install output:\n%s", f.Out)
	}
	code := f.run("hooks", "status", "codex")
	line := strings.TrimSpace(f.Out.String())
	for _, want := range []string{"stop:1:0 (its trust is of the hook before magnum", "interrupt:0:0 (its trust is of the hook before magnum",
		"no trust entry for session_start:0:0, permission_request:0:0"} {
		if code != 1 || !strings.Contains(line, want) {
			t.Fatalf("status = %d %q, want %q", code, line, want)
		}
	}
	trust, _, err := agents.ReadCodexHooksTrust(f.codex)
	if err != nil || slices.ContainsFunc(trust.Untrusted, func(h agents.CodexHookAt) bool { return h.Place == "stop:0:0" }) {
		t.Fatalf("moshi's own trust = %+v %v, want it trusted", trust.Untrusted, err)
	}
	// The operator chose Trust all: Codex wrote the new hooks' hashes.
	f.trustPlaces("sha256:new", append([]string{"stop:0:0"}, f.magnumPlaces()...)...)
	if code := f.run("hooks", "status", "codex"); code != 0 || strings.TrimSpace(f.Out.String()) != "codex: current (v1) ("+f.codexScript()+")" {
		t.Fatalf("after a new trust = %d %q", code, f.Out)
	}
}

// A hooks.json the install makes can land on trust entries an earlier
// hooks.json left in config.toml: they are recorded too.
func TestHooksAnInstallIntoANewHooksJSONRecordsTheOldTrustAtItsPlaces(t *testing.T) {
	f := newHooksFixture(t)
	f.trustPlaces("sha256:old", "stop:0:0", "session_start:0:0", "permission_request:0:0", "interrupt:0:0")
	if code := f.run("hooks", "install", "codex"); code != 0 {
		t.Fatalf("install = %d\n%s%s", code, f.Out, f.Err)
	}
	if code := f.run("hooks", "status", "codex"); code != 1 || !strings.Contains(f.Out.String(), "a trust entry Codex no longer accepts for stop:0:0") {
		t.Fatalf("status = %d %q", code, f.Out)
	}
}

// An install from before magnum recorded the trust an append landed on
// left magnum's entries on old trust entries with no record. A re-run of
// the install records them when config.toml has not changed since magnum
// wrote its script (no trust was written after the install, so every
// entry at magnum's places is older than magnum's hooks); when config.toml
// changed since, it cannot tell and records nothing, and
// --assume-untrusted records them on the operator's word.
func TestHooksAReRunRecordsOldTrustOnlyWhenItCanTell(t *testing.T) {
	f := newHooksFixture(t)
	installed := func() {
		t.Helper()
		if code := f.run("hooks", "install", "codex"); code != 0 {
			t.Fatalf("install = %d\n%s%s", code, f.Out, f.Err)
		}
		if n, err := agents.ForgetStaleCodexTrust(f.codex); err != nil {
			t.Fatal(err)
		} else if n != 0 {
			t.Fatalf("the first install recorded %d, want none (no trust entries yet)", n)
		}
	}
	installed()
	f.trustPlaces("sha256:old", f.magnumPlaces()...) // as an earlier hooks.json left them, unrecorded
	scriptAt := time.Now().Add(-time.Hour)
	if err := os.Chtimes(f.codexScript(), scriptAt, scriptAt); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(f.codex, "config.toml")

	// config.toml written after the script: the re-run cannot tell.
	if err := os.Chtimes(config, scriptAt.Add(time.Minute), scriptAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if code := f.run("hooks", "install", "codex"); code != 0 || !strings.Contains(f.Out.String(), "codex: current (v1)") {
		t.Fatalf("re-run with a newer config.toml = %d\n%s%s", code, f.Out, f.Err)
	}
	if code := f.run("hooks", "install", "codex", "--assume-untrusted"); code != 0 || !strings.Contains(f.Out.String(), "codex: recorded 4 trust entries at magnum's places as stale") ||
		!strings.Contains(f.Out.String(), "codex: untrusted (v1)") {
		t.Fatalf("--assume-untrusted = %d\n%s%s", code, f.Out, f.Err)
	}
	if n, err := agents.ForgetStaleCodexTrust(f.codex); err != nil || n != 4 {
		t.Fatalf("forget = %d %v", n, err)
	}

	// config.toml unchanged since the script: every entry predates it.
	if err := os.Chtimes(config, scriptAt.Add(-time.Minute), scriptAt.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if code := f.run("hooks", "install", "codex"); code != 0 || !strings.Contains(f.Out.String(), "codex: recorded 4 trust entries at magnum's places as stale") ||
		!strings.Contains(f.Out.String(), "codex: untrusted (v1)") {
		t.Fatalf("re-run with an older config.toml = %d\n%s%s", code, f.Out, f.Err)
	}
	if code := f.run("hooks", "install", "codex"); code != 0 || strings.Contains(f.Out.String(), "recorded") {
		t.Fatalf("a second re-run = %d\n%s", code, f.Out)
	}
}

// An upgrade rewrites the script, but the re-run compares config.toml
// with the script as the earlier install wrote it, so a trust written
// since that install is never taken for an old one.
func TestHooksAnUpgradeNeverTakesTheTrustSinceTheInstallForOld(t *testing.T) {
	f := newHooksFixture(t)
	if code := f.run("hooks", "install", "codex"); code != 0 {
		t.Fatal(f.Err)
	}
	f.trustPlaces("sha256:valid", f.magnumPlaces()...) // the operator's Trust all
	old := time.Now().Add(-2 * time.Hour)
	f.write(f.codexScript(), strings.Replace(f.read(f.codexScript()), "MAGNUM_HOOK_VERSION=1", "MAGNUM_HOOK_VERSION=0", 1))
	if err := os.Chtimes(f.codexScript(), old, old); err != nil {
		t.Fatal(err)
	}
	trusted := old.Add(time.Hour)
	if err := os.Chtimes(filepath.Join(f.codex, "config.toml"), trusted, trusted); err != nil {
		t.Fatal(err)
	}
	if code := f.run("hooks", "install", "codex"); code != 0 || !strings.Contains(f.Out.String(), "codex: current (v1)") || strings.Contains(f.Out.String(), "recorded") {
		t.Fatalf("upgrade = %d\n%s%s", code, f.Out, f.Err)
	}
}
