package factory

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/staging"
)

// stubGitHub is a staging.GitHub whose every read fails: the deploy's deps
// are complete, so only the deploy's git decides whether it may go on, and
// a read that comes first fails in words.
type stubGitHub struct{}

func (stubGitHub) DeploymentsOf(context.Context, string, string, string) (staging.Deployments, error) {
	return staging.Deployments{}, errors.New("stub GitHub: no deployments")
}

func (stubGitHub) RunsOf(context.Context, string, string, string) ([]staging.Run, error) {
	return nil, errors.New("stub GitHub: no runs")
}

// stagingPlanner is the deploy side of internal/staging's Client, which
// NewStaging's client carries (it embeds the Client): a refactor that drops
// it breaks this build, so the pin below never passes by testing nothing.
type stagingPlanner interface {
	Plan(ctx context.Context, pr staging.PR, envs []staging.Env) (staging.DeployPlan, error)
	Deploy(ctx context.Context, plan staging.DeployPlan, by string) ([]staging.EffectID, error)
}

var _ stagingPlanner = (*stagingClient)(nil)

// The factory daemon's staging client reads, and can never deploy, so the
// factory daemon cannot push to a staging branch by accident: given
// internal/staging's own git, which deploys, a GitHub and a mirror, its
// Plan and Deploy still refuse (its fetch clock hides the deploy's git),
// and no git runs. The factory screen's E deploys through magnum's daemon.
// internal/staging's client on the same deps gets past that refusal (and
// stops at the next one: the deps name no git env), so the refusal is the
// factory client's own.
func TestTheFactoryStagingClientCannotDeploy(t *testing.T) {
	root := t.TempDir() + "/staging"
	run := &execx.Fake{}
	deps := func() staging.Deps {
		return staging.Deps{Git: staging.NewGit(run), GitHub: stubGitHub{}, Mirror: staging.MirrorDir(root), MirrorRoot: staging.MirrorRoot(root)}
	}
	cfg := config.Defaults()
	cfg.Factory.Repos = []config.FactoryRepo{{Repo: "example/shop", Staging: []config.FactoryStaging{{Name: "void", Branch: "staging-void"}}}}
	pr := staging.PR{Repo: "example/shop", Number: 5, Head: strings.Repeat("ab", 20)}
	envs := []staging.Env{{Name: "void", Branch: "staging-void"}}

	c := NewStaging(cfg, deps(), nil)
	defer c.Close()
	p, ok := c.(stagingPlanner)
	if !ok {
		t.Fatalf("NewStaging's client %T has no Plan and Deploy: the pin needs a new form", c)
	}
	if _, err := p.Plan(t.Context(), pr, envs); err == nil || !strings.Contains(err.Error(), "cannot deploy") {
		t.Errorf("the factory client's Plan: %v, want its refusal to deploy", err)
	}
	plan := staging.DeployPlan{PR: pr, Envs: []staging.EnvPlan{{Env: envs[0]}}}
	if _, err := p.Deploy(t.Context(), plan, staging.ByFactory); err == nil || !strings.Contains(err.Error(), "cannot deploy") {
		t.Errorf("the factory client's Deploy: %v, want its refusal to deploy", err)
	}
	if len(run.Calls) != 0 {
		t.Errorf("the refused deploy ran git: %v", run.Calls)
	}

	raw := staging.New(deps())
	defer raw.Close()
	if _, err := raw.Plan(t.Context(), pr, envs); err == nil || strings.Contains(err.Error(), "cannot deploy") {
		t.Errorf("internal/staging's client on the same deps: %v, want it past the deploy's git", err)
	}
}
