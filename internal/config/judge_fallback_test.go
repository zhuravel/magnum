package config

import (
	"path/filepath"
	"testing"

	"github.com/zhuravel/magnum/internal/paths"
)

// The second built-in judge stands in reserve: a watch without a roles list
// runs its first judge, and claude-judge waits for judge_fallback, which
// names it by default, to switch a refused PR to it (or for a roles list to
// name it), so the default configuration still has exactly one judge per
// watch.
func TestASecondJudgeStandsInReserve(t *testing.T) {
	cfg := Defaults()
	judges := cfg.Judges()
	if len(judges) != 2 || judges[0].Name != RoleCodexJudge || judges[1].Name != RoleClaudeJudge {
		t.Fatalf("Judges() = %+v", judges)
	}
	cj := judges[1]
	if cj.Kind != KindClaude || !cj.Judge || cj.Model != "opus" || cj.Effort != "xhigh" || cj.RereviewEffort != "high" ||
		cj.MaxSessionRounds != DefaultJudgeSessionRounds || cj.Skill != DefaultSkill || cj.Prompt != "judge-initial.md" ||
		cj.Recovery != "judge-recovery.md" || len(cj.Aliases) != 0 {
		t.Errorf("claude-judge = %+v", cj)
	}
	for _, r := range cfg.RolesFor(nil) {
		if r.Name == RoleClaudeJudge {
			t.Errorf("RolesFor(nil) lists the reserve judge: %v", roleNames(cfg.RolesFor(nil)))
		}
	}
	if j := cfg.JudgeFor(nil); j.Name != RoleCodexJudge {
		t.Errorf("JudgeFor(nil) = %s", j.Name)
	}
	if r, ok := cfg.JudgeFallbackFor(nil); !ok || r.Name != RoleClaudeJudge {
		t.Errorf("the default judge_fallback = %+v, %v, want the reserve judge", r, ok)
	}
	root := repoRoot(t)
	explicit, err := LoadWithOptions(paths.Layout{Home: root}, filepath.Join(root, "config.defaults.toml"), LoadOptions{NoOverlay: true})
	if err != nil {
		t.Fatal(err)
	}
	if explicit.Pipeline.JudgeFallback != RoleClaudeJudge {
		t.Errorf("config.defaults.toml judge_fallback = %q", explicit.Pipeline.JudgeFallback)
	}
	// A watch whose roles name the reserve judge runs it as its judge.
	w := &Watch{Owner: "example", Roles: []string{"claude-review", "claude-judge"}}
	if j := cfg.JudgeFor(w); j.Name != RoleClaudeJudge {
		t.Errorf("JudgeFor(roles with claude-judge) = %s", j.Name)
	}
	// A stored role name (no watch) resolves the reserve judge too: the agents
	// manager and the pipeline name the roles a flagged PR's rounds run.
	if r, ok := cfg.RoleByNameOrAlias(nil, "claude-judge"); !ok || r.Name != RoleClaudeJudge {
		t.Errorf("RoleByNameOrAlias(nil, claude-judge) = %+v, %v", r, ok)
	}
	if _, ok := cfg.RoleByNameOrAlias(&Watch{Owner: "example"}, "claude-judge"); ok {
		t.Error("a watch without a roles list resolved the reserve judge as one of its roles")
	}
}

// judge_fallback resolves per watch: the watch's value, else the
// pipeline's, among every declared role.
func TestJudgeFallbackForResolvesPerWatch(t *testing.T) {
	cfg := validPipelineConfig()
	cfg.Pipeline.JudgeFallback = "claude-judge"
	if r, ok := cfg.JudgeFallbackFor(nil); !ok || r.Name != RoleClaudeJudge {
		t.Fatalf("JudgeFallbackFor(nil) = %+v, %v", r, ok)
	}
	w := &Watch{Owner: "example", JudgeFallback: "nope"}
	if _, ok := cfg.JudgeFallbackFor(w); ok {
		t.Error("an unknown watch value resolved")
	}
	w.JudgeFallback = " Claude-Judge "
	if r, ok := cfg.JudgeFallbackFor(w); !ok || r.Name != RoleClaudeJudge {
		t.Errorf("the watch's value (case and space ignored) = %+v, %v", r, ok)
	}
	cfg.Pipeline.JudgeFallback = ""
	if _, ok := cfg.JudgeFallbackFor(nil); ok {
		t.Error("an empty value resolved")
	}
	if errs := cfg.Validate(); errs != nil {
		t.Fatalf("a valid judge_fallback fails validation: %v", errs)
	}
}

// Validation refuses a judge_fallback that names no role, a role that is
// not a judge, a shell role, the watch's own judge's kind, and a role the
// watch's roles already list.
func TestValidateJudgeFallback(t *testing.T) {
	cfg := validPipelineConfig()
	cfg.Pipeline.JudgeFallback = "nope"
	wantError(t, cfg.Validate(), `pipeline: judge_fallback names unknown role "nope"`)

	cfg = validPipelineConfig()
	cfg.Pipeline.JudgeFallback = "claude-review"
	wantError(t, cfg.Validate(), "pipeline: judge_fallback role claude-review is not a judge")

	cfg = validPipelineConfig()
	cfg.Pipeline.JudgeFallback = "codex-judge"
	wantError(t, cfg.Validate(), "judge_fallback role codex-judge is the watch's own judge")

	cfg = validPipelineConfig()
	cfg.Roles = append(cfg.Roles, Role{Name: "codex-judge-2", Kind: KindCodex, Judge: true, Skill: DefaultSkill})
	cfg.Normalize()
	cfg.Pipeline.JudgeFallback = "codex-judge-2"
	wantError(t, cfg.Validate(), "judge_fallback role codex-judge-2 is of the same kind (codex) as judge codex-judge")

	cfg = validPipelineConfig()
	cfg.Pipeline.JudgeFallback = "claude-judge"
	cfg.judgeFallbackSet = true // a file writes it: the built-in default is not checked (TestAWatchJudgedByClaudeJudgeLoadsWithTheFallbackOff)
	cfg.Watches[0].Roles = []string{"claude-review", "codex-judge", "claude-judge"}
	wantError(t, cfg.Validate(), "judge_fallback role claude-judge is also in roles")

	cfg = validPipelineConfig()
	cfg.Watches[0].JudgeFallback = "claude-judge"
	if errs := cfg.Validate(); errs != nil {
		t.Fatalf("a watch's valid judge_fallback fails validation: %v", errs)
	}
}
