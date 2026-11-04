package factory

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/zhuravel/magnum"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/prompts"
)

// The review candidate the factory's local rounds run with is the build's
// own prompts and judge skill with one swap: claude-review.md is the local
// reviewer prompt (claude-review-local.md), which names no PR URL. Every
// other prompt and the skill are the embedded ones, a config pointed at it
// (config.WithCandidate) resolves claude-review.md to the local prompt and
// gives the judges its skill, and the live config resolves what it did. A
// restart writes it again over whatever was there.
func TestReviewCandidateSwapsOnlyTheClaudeReviewPrompt(t *testing.T) {
	l := paths.Layout{Home: t.TempDir()}
	stale := filepath.Join(l.Data(), "factory", "review-candidate", "prompts", "claude-review.md")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("an older build's\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, err := WriteReviewCandidate(l)
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(l.Data(), "factory", "review-candidate") {
		t.Fatalf("candidate at %s", dir)
	}
	read := func(rel string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	local, _ := prompts.Read("claude-review-local.md")
	if got := read("prompts/claude-review.md"); got != local {
		t.Fatalf("the candidate's claude-review.md is not the local reviewer prompt:\n%s", got)
	}
	for _, name := range append(prompts.Names(), prompts.Shared, prompts.FactoryShared) {
		if name == "claude-review.md" {
			continue
		}
		want, _ := prompts.Read(name)
		if got := read("prompts/" + name); got != want {
			t.Errorf("the candidate's %s differs from the embedded one", name)
		}
	}
	if got := read(config.CandidateSkill); got != string(magnum.Skill) {
		t.Error("the candidate's judge skill is not the embedded one")
	}

	live := config.Defaults()
	livePrompt, err := live.ResolvePrompt("claude-review.md")
	if err != nil {
		t.Fatal(err)
	}
	cand, err := live.WithCandidate(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := cand.ResolvePrompt("claude-review.md")
	if err != nil || p.Text != local {
		t.Fatalf("the candidate config's claude-review.md: %v\n%s", err, p.Text)
	}
	if again, err := live.ResolvePrompt("claude-review.md"); err != nil || again.Text != livePrompt.Text || again.Text == local {
		t.Fatalf("the live config's claude-review.md changed: %v", err)
	}
	judges := cand.Judges()
	if len(judges) == 0 || !slices.ContainsFunc(judges, func(r config.Role) bool { return r.Skill == filepath.Join(dir, config.CandidateSkill) }) {
		t.Fatalf("the candidate's judges %+v do not read its skill", judges)
	}
}
