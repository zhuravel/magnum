package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zhuravel/magnum/internal/paths"
)

// The four defaults the operator chose by hand (DECISIONS "Four built-in
// defaults follow what the operator chose by hand"): a fresh install gets
// the fallback judge, skips drafts, keeps the team's Codex MCP servers out
// and takes its PATH from the login shell.

// fallbackOf is the name of the fallback judge that resolves for w, "" for
// none.
func fallbackOf(cfg *Config, w *Watch) string {
	r, ok := cfg.JudgeFallbackFor(w)
	if !ok {
		return ""
	}
	return r.Name
}

// The built-in defaults name claude-judge as the fallback, and it resolves
// for a watch whose judge is codex-judge, the built-in first judge, and for
// a PR no watch covers.
func TestTheBuiltInFallbackJudgeIsClaudeJudgeForACodexJudgedWatch(t *testing.T) {
	if got := Defaults().Pipeline.JudgeFallback; got != RoleClaudeJudge {
		t.Fatalf("Defaults().Pipeline.JudgeFallback = %q", got)
	}
	cfg, _, err := loadUser(t, snapTOML)
	if err != nil {
		t.Fatal(err)
	}
	w := &cfg.Watches[0]
	if j := cfg.JudgeFor(w); j.Name != RoleCodexJudge {
		t.Fatalf("judge = %s", j.Name)
	}
	if got := fallbackOf(cfg, w); got != RoleClaudeJudge {
		t.Errorf("the watch's fallback = %q, want claude-judge", got)
	}
	if got := fallbackOf(cfg, nil); got != RoleClaudeJudge {
		t.Errorf("the fallback of a PR no watch covers = %q, want claude-judge", got)
	}
	if w := cfg.Warnings(); len(w) != 0 {
		t.Errorf("warnings = %q", w)
	}
}

// A full --config base that declares its own [[role]] list without
// claude-judge loads as it did before the default changed: the built-in
// fallback names no declared judge, so it is off, with no error and no
// warning. The same name written in that base is refused as before.
func TestABaseWithoutClaudeJudgeLoadsWithTheFallbackOff(t *testing.T) {
	const roles = "\n[[role]]\nname = \"codex-judge\"\n[[role]]\nname = \"claude-review\"\n"
	cfg := mustLoad(t, map[string]string{"config.toml": snapTOML + roles})
	if _, ok := cfg.RoleByNameOrAlias(nil, RoleClaudeJudge); ok {
		t.Fatal("the base's role list kept claude-judge")
	}
	if got := fallbackOf(cfg, &cfg.Watches[0]); got != "" {
		t.Errorf("fallback = %q, want none", got)
	}
	if got := fallbackOf(cfg, nil); got != "" {
		t.Errorf("fallback without a watch = %q, want none", got)
	}
	if w := cfg.Warnings(); len(w) != 0 {
		t.Errorf("warnings = %q", w)
	}
	_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": "[pipeline]\njudge_fallback = \"claude-judge\"\n" + snapTOML + roles})
	wantError(t, err, `pipeline: judge_fallback names unknown role "claude-judge"`)
}

// A watch whose judge is claude-judge itself (its roles name it) loads: the
// built-in fallback would be of the judge's own kind, so it is off for that
// watch alone, while a codex-judged watch beside it keeps it. The same
// value written by the user, globally or for the watch, is refused as
// before.
func TestAWatchJudgedByClaudeJudgeLoadsWithTheFallbackOff(t *testing.T) {
	const claudeJudged = snapTOML + "roles = [\"claude-review\", \"claude-judge\"]\n" +
		"[[watch]]\nowner = \"talkable\"\ninclude = [\"*\"]\nidentity = \"z\"\n"
	cfg, _, err := loadUser(t, claudeJudged)
	if err != nil {
		t.Fatalf("a claude-judge watch fails to load: %v", err)
	}
	own, codex := &cfg.Watches[0], &cfg.Watches[1]
	if j := cfg.JudgeFor(own); j.Name != RoleClaudeJudge {
		t.Fatalf("judge = %s", j.Name)
	}
	if got := fallbackOf(cfg, own); got != "" {
		t.Errorf("the claude-judge watch's fallback = %q, want none", got)
	}
	if got := fallbackOf(cfg, codex); got != RoleClaudeJudge {
		t.Errorf("the codex-judge watch's fallback = %q, want claude-judge", got)
	}
	if w := cfg.Warnings(); len(w) != 0 {
		t.Errorf("warnings = %q", w)
	}

	_, _, err = loadUser(t, "[pipeline]\njudge_fallback = \"claude-judge\"\n"+claudeJudged)
	wantError(t, err, "watch example: judge_fallback role claude-judge is also in roles")
	_, _, err = loadUser(t, snapTOML+"roles = [\"claude-review\", \"claude-judge\"]\njudge_fallback = \"claude-judge\"\n")
	wantError(t, err, "watch example: judge_fallback role claude-judge is also in roles")
}

// An explicit "" in the user config stays the opt-out: a refused PR is never
// reviewed again. A value the user writes that names no judge is refused as
// before, globally and for one watch.
func TestAnExplicitEmptyFallbackTurnsItOff(t *testing.T) {
	cfg, _, err := loadUser(t, "[pipeline]\njudge_fallback = \"\"\n"+snapTOML)
	if err != nil {
		t.Fatal(err)
	}
	if got := fallbackOf(cfg, &cfg.Watches[0]); got != "" {
		t.Errorf("fallback = %q after judge_fallback = \"\"", got)
	}
	if got := fallbackOf(cfg, nil); got != "" {
		t.Errorf("fallback without a watch = %q after judge_fallback = \"\"", got)
	}
	_, _, err = loadUser(t, "[pipeline]\njudge_fallback = \"nope\"\n"+snapTOML)
	wantError(t, err, `pipeline: judge_fallback names unknown role "nope"`)
	_, _, err = loadUser(t, snapTOML+"judge_fallback = \"claude-review\"\n")
	wantError(t, err, "watch example: judge_fallback role claude-review is not a judge")
	_, _, err = loadUser(t, "[pipeline]\njudge_fallback = \"codex-judge\"\n"+snapTOML)
	wantError(t, err, "judge_fallback role codex-judge is the watch's own judge")
}

// A watch skips drafts unless it sets include_drafts = true: a draft waits
// until it is ready for review, or until a review request.
func TestDraftsWaitUntilReadyByDefault(t *testing.T) {
	if (Watch{}).DraftsIncluded() {
		t.Error("a watch without include_drafts includes drafts")
	}
	cfg, _, err := loadUser(t, snapTOML+"[[watch]]\nowner = \"talkable\"\ninclude = [\"*\"]\nidentity = \"z\"\ninclude_drafts = true\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Watches[0].DraftsIncluded() {
		t.Error("the loaded watch without include_drafts includes drafts")
	}
	if !cfg.Watches[1].DraftsIncluded() {
		t.Error("include_drafts = true does not include drafts")
	}
}

// The codex kind turns the team's project MCP servers off by default
// (project_mcp "off"); mcp_allow keeps one, and project_mcp = "allow" keeps
// them all. Claude has no such servers to turn off by name (mcp_off with
// --strict-mcp-config keeps every server out), so its value is unchanged.
func TestTheCodexKindTurnsTheTeamsProjectMCPServersOffByDefault(t *testing.T) {
	for name, cfg := range map[string]*Config{"Defaults": Defaults(), "built-in": mustLoadBuiltin(t)} {
		codex, _ := cfg.KindSpec(KindCodex)
		if codex.ProjectMCP != ProjectMCPOff {
			t.Errorf("%s: codex project_mcp = %q, want off", name, codex.ProjectMCP)
		}
		if claude, _ := cfg.KindSpec(KindClaude); claude.ProjectMCP != ProjectMCPAllow {
			t.Errorf("%s: claude project_mcp = %q, want allow (unchanged)", name, claude.ProjectMCP)
		}
	}
	allow, _, err := loadUser(t, "[kinds.codex]\nproject_mcp = \"allow\"\n"+snapTOML)
	if err != nil {
		t.Fatal(err)
	}
	if k, _ := allow.KindSpec(KindCodex); k.ProjectMCP != ProjectMCPAllow {
		t.Errorf("project_mcp = \"allow\" resolves %q", k.ProjectMCP)
	}
	other, _, err := loadUser(t, "[kinds.codex]\nmcp_allow = [\"tracker\"]\n"+snapTOML)
	if err != nil {
		t.Fatal(err)
	}
	if k, _ := other.KindSpec(KindCodex); k.ProjectMCP != ProjectMCPOff {
		t.Errorf("a [kinds.codex] table without project_mcp resolves %q, want off", k.ProjectMCP)
	}
}

// The daemon takes the login shell's PATH by default; shell_path = false
// keeps launchd's.
func TestTheDaemonTakesTheLoginShellsPATHByDefault(t *testing.T) {
	if !Defaults().Daemon.ShellPath {
		t.Error("Defaults(): shell_path is off")
	}
	if !mustLoadBuiltin(t).Daemon.ShellPath {
		t.Error("the built-in defaults: shell_path is off")
	}
	off, _, err := loadUser(t, "[daemon]\nshell_path = false\n"+snapTOML)
	if err != nil {
		t.Fatal(err)
	}
	if off.Daemon.ShellPath {
		t.Error("shell_path = false leaves it on")
	}
}

// mustLoadBuiltin loads the built-in defaults alone, in a fresh home.
func mustLoadBuiltin(t *testing.T) *Config {
	t.Helper()
	home := t.TempDir()
	cfg, err := LoadWithOptions(paths.Layout{Home: home, UserConfig: filepath.Join(home, "absent.toml")}, "", LoadOptions{NoOverlay: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "absent.toml")); !os.IsNotExist(err) {
		t.Fatalf("the user config exists: %v", err)
	}
	return cfg
}
