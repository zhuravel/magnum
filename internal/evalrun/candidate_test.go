package evalrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/paths"
)

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// candidateDir is a checkout-shaped candidate: prompts/ with the judge's initial prompt and the
// judge skill.
func candidateDir(t *testing.T, judgePrompt, prompt, skill string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "prompts", judgePrompt), prompt)
	writeFile(t, filepath.Join(dir, CandidateSkill), skill)
	return dir
}

// testConfig is the built-in config of a checkout at home, its roles normalized, with a second judge.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	home := t.TempDir()
	cfg := config.Defaults()
	cfg.Layout = paths.Layout{Home: home}
	cfg.Pipeline.PromptsDir = filepath.Join(home, "prompts")
	judge := cfg.JudgeFor(nil)
	judge.Name, judge.Skill = "second-judge", filepath.Join(home, "other", "SKILL.md")
	cfg.Roles = append(cfg.RolesFor(nil), judge)
	return cfg
}

// A candidate is a directory shaped like magnum's checkout: the copy of the config WithCandidate
// returns reads its prompts from <dir>/prompts and gives every judge <dir>'s skill, while the config
// it was made from keeps its own. A daemon's config, whose prompts were loaded at startup, gives a
// copy that reads the candidate's files, not what the daemon loaded.
func TestWithCandidatePointsPromptsAndEveryJudgeSkillAtTheDirectory(t *testing.T) {
	cfg := testConfig(t)
	name := cfg.JudgeFor(nil).PromptFile(config.PromptInitial)
	writeFile(t, filepath.Join(cfg.Pipeline.PromptsDir, name), "the live judge prompt\n")
	livePrompts, liveSkills := cfg.Pipeline.PromptsDir, map[string]string{}
	for _, r := range cfg.RolesFor(nil) {
		liveSkills[r.Name] = r.Skill
	}
	if _, err := cfg.SnapshotPrompts("", time.Now()); err != nil {
		t.Fatal(err)
	}
	dir := candidateDir(t, name, "the candidate's judge prompt\n", "# Candidate skill\n")

	cand, err := WithCandidate(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if cand.Pipeline.PromptsDir != filepath.Join(dir, "prompts") {
		t.Errorf("prompts_dir = %s, want %s/prompts", cand.Pipeline.PromptsDir, dir)
	}
	judges := 0
	for _, r := range cand.RolesFor(nil) {
		switch {
		case r.Judge:
			judges++
			if r.Skill != filepath.Join(dir, CandidateSkill) {
				t.Errorf("judge %s: skill %s, want the candidate's", r.Name, r.Skill)
			}
			if got := cand.SkillFile(config.SkillPath(r.Skill, cand.Layout)); got != filepath.Join(dir, CandidateSkill) {
				t.Errorf("judge %s reads the skill at %s", r.Name, got)
			}
		case r.Skill != liveSkills[r.Name]:
			t.Errorf("reviewer %s: skill %q changed", r.Name, r.Skill)
		}
	}
	if judges != 2 {
		t.Fatalf("%d judges in the copy, want 2", judges)
	}
	if p, err := cand.ResolvePrompt(name); err != nil || p.Text != "the candidate's judge prompt\n" {
		t.Errorf("the candidate's %s: %q, %v", name, p.Text, err)
	}
	if p, err := cfg.ResolvePrompt(name); err != nil || p.Text != "the live judge prompt\n" || cfg.Pipeline.PromptsDir != livePrompts {
		t.Errorf("the daemon's config changed: %s %q, %v", cfg.Pipeline.PromptsDir, p.Text, err)
	}
	for _, r := range cfg.RolesFor(nil) {
		if r.Skill != liveSkills[r.Name] {
			t.Errorf("the daemon's %s: skill %s changed", r.Name, r.Skill)
		}
	}

	t.Chdir(filepath.Dir(dir))
	rel, err := WithCandidate(cfg, filepath.Base(dir))
	if err != nil || rel.Pipeline.PromptsDir != filepath.Join(dir, "prompts") {
		t.Errorf("a relative directory: %v, prompts_dir %s", err, rel.Pipeline.PromptsDir)
	}
}

// A directory without the judge skill or without prompts/ is no candidate: the error names what is
// missing, and no config comes back.
func TestWithCandidateRefusesADirectoryWithoutASkill(t *testing.T) {
	cfg := testConfig(t)
	noSkill := t.TempDir()
	writeFile(t, filepath.Join(noSkill, "prompts", "judge-initial.md"), "x")
	if c, err := WithCandidate(cfg, noSkill); err == nil || c != nil || !strings.Contains(err.Error(), CandidateSkill) {
		t.Errorf("no skill: %v, %v", c, err)
	}
	noPrompts := t.TempDir()
	writeFile(t, filepath.Join(noPrompts, CandidateSkill), "# skill")
	if c, err := WithCandidate(cfg, noPrompts); err == nil || c != nil || !strings.Contains(err.Error(), "prompts") {
		t.Errorf("no prompts: %v, %v", c, err)
	}
	if c, err := WithCandidate(cfg, filepath.Join(noPrompts, "missing")); err == nil || c != nil {
		t.Errorf("no directory: %v, %v", c, err)
	}
	skillDir := t.TempDir()
	writeFile(t, filepath.Join(skillDir, "prompts", "x.md"), "x")
	if err := os.MkdirAll(filepath.Join(skillDir, CandidateSkill), 0o700); err != nil {
		t.Fatal(err)
	}
	if c, err := WithCandidate(cfg, skillDir); err == nil || c != nil {
		t.Errorf("a directory where the skill file belongs: %v, %v", c, err)
	}
}
