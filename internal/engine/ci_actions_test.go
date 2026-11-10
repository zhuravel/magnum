package engine

import (
	"context"
	"slices"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
)

// tokenGH is an identity's client over a token of the daemon's own
// (Deps.GitHubEnv): the poll identity's fake, whose Actions calls it
// records as "with <token>" before each.
type tokenGH struct {
	*fakeGH
	token string
}

func (g tokenGH) HeadRuns(ctx context.Context, owner, repo, sha string) ([]github.WorkflowRun, error) {
	g.record("with " + g.token)
	return g.fakeGH.HeadRuns(ctx, owner, repo, sha)
}

func (g tokenGH) RunJobs(ctx context.Context, owner, repo string, id int64) ([]github.WorkflowJob, error) {
	g.record("with " + g.token)
	return g.fakeGH.RunJobs(ctx, owner, repo, id)
}

func (g tokenGH) RerunRun(ctx context.Context, owner, repo string, id int64) error {
	g.record("with " + g.token)
	return g.fakeGH.RerunRun(ctx, owner, repo, id)
}

func (g tokenGH) DispatchWorkflow(ctx context.Context, owner, repo, workflow, ref string) error {
	g.record("with " + g.token)
	return g.fakeGH.DispatchWorkflow(ctx, owner, repo, workflow, ref)
}

func (g tokenGH) DispatchedRuns(ctx context.Context, owner, repo, workflow, sha string) ([]github.WorkflowRun, error) {
	g.record("with " + g.token)
	return g.fakeGH.DispatchedRuns(ctx, owner, repo, workflow, sha)
}

func (g tokenGH) RecentRuns(ctx context.Context, owner, repo, workflow string) ([]github.WorkflowRun, error) {
	g.record("with " + g.token)
	return g.fakeGH.RecentRuns(ctx, owner, repo, workflow)
}

// plainGH is the fixer App's own client, the one its panes' token backs,
// which may not use Actions: it records every Actions call as "plain
// <call>", which must never come, and answers it as GitHub does, 403.
type plainGH struct{ *fakeGH }

// errNoActions is GitHub's answer to an Actions call of a token without
// Actions.
var errNoActions = &github.APIError{Op: "actions", Status: 403, Message: "Resource not accessible by integration"}

func (g plainGH) HeadRuns(context.Context, string, string, string) ([]github.WorkflowRun, error) {
	g.record("plain head_runs")
	return nil, errNoActions
}

func (g plainGH) RunJobs(context.Context, string, string, int64) ([]github.WorkflowJob, error) {
	g.record("plain run_jobs")
	return nil, errNoActions
}

func (g plainGH) RerunRun(context.Context, string, string, int64) error {
	g.record("plain rerun")
	return errNoActions
}

func (g plainGH) DispatchWorkflow(context.Context, string, string, string, string) error {
	g.record("plain dispatch")
	return errNoActions
}

func (g plainGH) DispatchedRuns(context.Context, string, string, string, string) ([]github.WorkflowRun, error) {
	g.record("plain dispatched_runs")
	return nil, errNoActions
}

func (g plainGH) RecentRuns(context.Context, string, string, string) ([]github.WorkflowRun, error) {
	g.record("plain recent_runs")
	return nil, errNoActions
}

// withFixerAppClients gives the fixer App (withFixerApp) its own client,
// plainGH, and its client over a token of the daemon's own, tokenGH, whose
// env must be the App's config dir.
func withFixerAppClients(h *harness) {
	plain := h.d.GitHub
	h.d.GitHub = func(id string) GitHub {
		if id == fixerAppName {
			return plainGH{h.gh}
		}
		return plain(id)
	}
	h.d.GitHubEnv = func(id string, env map[string]string) GitHub {
		if id != fixerAppName || env["GH_CONFIG_DIR"] != "/state/gh/"+fixerAppName {
			h.t.Errorf("GitHubEnv(%s, %v)", id, env)
		}
		return tokenGH{h.gh, env["GH_TOKEN"]}
	}
}

// actionsAsFixerApp is rerunHarness with the fixer App (withFixerApp:
// token_permissions contents and pull_requests write alone) as [[repo]]
// ci_dispatch_as and dispatch as ci_dispatch ("" leaves it unset).
func actionsAsFixerApp(t *testing.T, dispatch string) *harness {
	t.Helper()
	return rerunHarnessWith(t, nil, withFixerApp, withFixerAppClients, func(h *harness) {
		h.cfg.Repos = append(h.cfg.Repos, config.Repo{Repo: "talkable/talkable", CIDispatch: dispatch, CIDispatchAs: fixerAppName})
	})
}

// wantNoPlainActions fails if the fixer App's own client made an Actions
// call.
func (h *harness) wantNoPlainActions() {
	h.t.Helper()
	if calls := h.ghCalls("plain"); len(calls) != 0 {
		h.t.Fatalf("the App's own client made %q", calls)
	}
}

// The operator's choice of 2026-11-10: a fixer App as ci_dispatch_as. Its
// tokens carry contents and pull_requests write alone (its panes hold
// them), so the look reads the head's runs and re-runs the one that
// started no job over a token of the daemon's own with Actions write,
// minted once for the look and revoked at its end; the App's own client
// makes no Actions call. The poll's read of the re-run's attempt mints a
// token of its own, revoked too.
func TestTheLooksActionsCallsAsAFixerAppGoOverATokenOfTheDaemonsOwn(t *testing.T) {
	h := actionsAsFixerApp(t, "")
	h.setHeadRuns(fixHead, noJobRun())
	h.pastSettle()
	h.tick()
	h.wantNoPlainActions()
	tok := fixerAppName + "-actions-token1"
	if calls := h.ghCalls("with "); len(calls) != 3 || slices.ContainsFunc(calls, func(c string) bool { return c != "with "+tok }) {
		t.Fatalf("calls over a minted token = %q, want the read, the jobs and the re-run with %s", calls, tok)
	}
	if n := len(h.ghCalls("rerun:")); n != 1 {
		t.Fatalf("reruns = %d, want 1", n)
	}
	if mints, released := h.ids[fixerAppName].actionsTokens(); mints != 1 || released != 1 {
		t.Errorf("mints = %d, released = %d after the look; want one, released", mints, released)
	}
	if data := h.dispatchData("fix.open_ci_rerun"); data["identity"] != fixerAppName {
		t.Errorf("fix.open_ci_rerun data = %v, want it to name %s", data, fixerAppName)
	}
	h.tick() // the poll reads the re-run's attempt over a token of its own
	if mints, released := h.ids[fixerAppName].actionsTokens(); mints < 2 || mints != released {
		t.Errorf("mints = %d, released = %d after the poll; want each released", mints, released)
	}
	h.wantNoPlainActions()
}

// With ci_dispatch the look's start goes over the look's token too, and
// the poll's reads of the started run over one of each poll; every token
// is released.
func TestTheLookStartsTheCIWorkflowAsAFixerAppOverATokenOfTheDaemonsOwn(t *testing.T) {
	h := actionsAsFixerApp(t, "ci.yml")
	h.pastSettle()
	h.tick()
	h.tick()
	if calls := h.ghCalls("dispatch:"); len(calls) != 1 {
		t.Fatalf("dispatches = %q, want one", calls)
	}
	h.wantNoPlainActions()
	if mints, released := h.ids[fixerAppName].actionsTokens(); mints == 0 || mints != released {
		t.Errorf("mints = %d, released = %d; want each released", mints, released)
	}
	h.wantOpen(OpenWaits, "CI at a1a1a1a: magnum started ci.yml at ")
}

// After a push magnum made, the read of the workflow's newest runs and the
// start go over one token of the daemon's own, released after.
func TestAPushMagnumMadeStartsTheCIWorkflowAsAFixerAppOverOneToken(t *testing.T) {
	h, _ := pushedCIHarness(t, []string{"app/models/order.rb"}, withFixerAppClients, func(h *harness) {
		h.cfg.Repos[len(h.cfg.Repos)-1].CIDispatchAs = fixerAppName
	})
	h.advance(ciPushGrace)
	h.tick()
	if calls := h.ghCalls("dispatch:"); len(calls) != 1 {
		t.Fatalf("dispatches = %q, want one", calls)
	}
	h.wantNoPlainActions()
	tok := fixerAppName + "-actions-token1"
	if calls := h.ghCalls("with "); !slices.Equal(calls, []string{"with " + tok, "with " + tok}) {
		t.Errorf("calls over a minted token = %q, want the read and the start with %s", calls, tok)
	}
	if mints, released := h.ids[fixerAppName].actionsTokens(); mints != 1 || released != 1 {
		t.Errorf("mints = %d, released = %d; want one, released", mints, released)
	}
}

// A gh login as ci_dispatch_as (the operator's main repository) keeps
// its own client: no token is minted for any App.
func TestTheLooksActionsCallsAsAGhLoginMintNothing(t *testing.T) {
	h := rerunHarnessWith(t, nil, withFixerApp, func(h *harness) {
		h.cfg.Repos = append(h.cfg.Repos, config.Repo{Repo: "talkable/talkable", CIDispatchAs: "zhuravel"})
		h.d.GitHubEnv = func(string, map[string]string) GitHub { t.Error("GitHubEnv called"); return nil }
	})
	h.setHeadRuns(fixHead, noJobRun())
	h.pastSettle()
	h.tick()
	h.tick()
	if n := len(h.ghCalls("rerun:")); n != 1 {
		t.Fatalf("reruns = %d, want 1", n)
	}
	h.wantMintedNothing(fixerAppName)
	h.wantMintedNothing("talkable-app")
}

// wantMintedNothing fails if the harness minted an Actions token for id.
func (h *harness) wantMintedNothing(id string) {
	h.t.Helper()
	if mints, _ := h.ids[id].actionsTokens(); mints != 0 {
		h.t.Errorf("minted %d Actions tokens for %s", mints, id)
	}
}
