package evalrun

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/pipeline"
)

// A local round's reviewer that names the posted reviewer prompt (claude-review.md, `/code-review <PR
// URL>`, a URL no GitHub serves for a synthetic number) reads the factory's local reviewer prompt
// (claude-review-local.md) in its place, whoever runs the round: the factory's rounds and the replays
// of a local case both go through RunLocal, so a replay reviews as the round that ran did. The
// caller's config keeps its roles.
func TestALocalRoundReadsTheFactorysLocalReviewerPrompt(t *testing.T) {
	d, _ := localFixture(t, cleanResult)
	var got *config.Config
	inner := d.localRound
	d.localRound = func(ctx context.Context, cfg *config.Config, l paths.Layout, tag string, lr engine.LocalRound) (pipeline.RoundResult, error) {
		got = cfg
		return inner(ctx, cfg, l, tag, lr)
	}
	if _, err := RunLocal(t.Context(), d, localCase(t, 1)); err != nil {
		t.Fatal(err)
	}
	r, ok := got.RoleByNameOrAlias(nil, config.RoleClaudeReview)
	if !ok || r.PromptFile(config.PromptInitial) != LocalReviewPrompt {
		t.Fatalf("the round's claude-review reads %q, want %s", r.PromptFile(config.PromptInitial), LocalReviewPrompt)
	}
	p, err := got.ResolvePrompt(r.PromptFile(config.PromptInitial))
	if err != nil || !strings.HasPrefix(p.Text, "Review the change `git diff") || strings.Contains(p.Text, "/code-review") {
		t.Fatalf("the round's reviewer prompt %q, %v", p.Text, err)
	}
	if live, _ := d.Config.RoleByNameOrAlias(nil, config.RoleClaudeReview); live.PromptFile(config.PromptInitial) != LiveReviewPrompt {
		t.Fatalf("the caller's claude-review reads %q now", live.PromptFile(config.PromptInitial))
	}
}

// A local replay records, as its case's own inputs (eval.CaseRun.Inputs), the inputs of the round it
// ran (LocalReviewInputs: the local reviewer prompt in the posted one's place), never the run's, which
// hash the posted prompt no local round reads. An edit of the local reviewer prompt changes them, and
// one of the posted reviewer prompt does not.
func TestALocalReplayRecordsTheInputsOfTheRoundItRan(t *testing.T) {
	f := newReplayFixture(t, questionResult)
	cr := RunCase(context.Background(), f.d, localEvalCase(t), Options{RunDir: filepath.Join(RunsRoot(f.d.Layout), "20261109-120000"),
		AgentTag: "eval-test"})
	if cr.Score == nil {
		t.Fatalf("case run %+v", cr)
	}
	local := LocalReviewInputs(f.d.Config)
	if cr.Inputs == "" || cr.Inputs != local || local == Inputs(f.d.Config) {
		t.Fatalf("the replay's inputs %q, want the local round's %q, not the posted %q", cr.Inputs, local, Inputs(f.d.Config))
	}
	base, posted := testConfig(t), testConfig(t)
	writeFile(t, filepath.Join(posted.Pipeline.PromptsDir, LiveReviewPrompt), "/code-review {{.URL}} and more\n")
	if LocalReviewInputs(posted) != LocalReviewInputs(base) || Inputs(posted) == Inputs(base) {
		t.Fatal("an edit of the posted reviewer prompt moved the local inputs, or not the posted ones")
	}
	edited := testConfig(t)
	writeFile(t, filepath.Join(edited.Pipeline.PromptsDir, LocalReviewPrompt), "Review the change, and more.\n")
	if LocalReviewInputs(edited) == LocalReviewInputs(base) || Inputs(edited) != Inputs(base) {
		t.Fatal("an edit of the local reviewer prompt left the local inputs, or moved the posted ones")
	}
}
