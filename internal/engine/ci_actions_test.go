package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/identity"
	"github.com/zhuravel/magnum/internal/store"
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
// env must be the minted one: its token over the batch's gh config dir,
// never the pane's.
func withFixerAppClients(h *harness) {
	plain := h.d.GitHub
	h.d.GitHub = func(id string) GitHub {
		if id == fixerAppName {
			return plainGH{h.gh}
		}
		return plain(id)
	}
	h.d.GitHubEnv = func(id string, env map[string]string) GitHub {
		if id != fixerAppName || !strings.HasPrefix(env["GH_CONFIG_DIR"], "/state/gh/.actions-"+fixerAppName+"-") || !strings.HasPrefix(env["GH_TOKEN"], fixerAppName+"-actions-token") {
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

// While a run of the PR's branch is in progress, magnum's push waits, and
// every tick reads the workflow's newest runs again: as a fixer App, over
// a token of that tick's own, released by the tick's end, so no token
// outlives the tick that minted it; nothing starts meanwhile.
func TestAPushMagnumMadeThatWaitsOnTheBranchsRunMintsOneTokenPerTick(t *testing.T) {
	h, _ := pushedCIHarness(t, []string{"app/models/order.rb"}, withFixerAppClients, func(h *harness) {
		h.cfg.Repos[len(h.cfg.Repos)-1].CIDispatchAs = fixerAppName
	})
	h.gh.mu.Lock()
	h.gh.recent = []github.WorkflowRun{{ID: 7, Status: "in_progress", HeadSHA: fixHead, HeadBranch: "feature", Event: "workflow_dispatch"}}
	h.gh.mu.Unlock()
	h.advance(ciPushGrace)
	for tick := 1; tick <= 3; tick++ {
		h.tick()
		if mints, released := h.ids[fixerAppName].actionsTokens(); mints != tick || released != mints {
			t.Fatalf("after tick %d: mints = %d, released = %d; want one per tick, each released", tick, mints, released)
		}
		h.advance(30 * time.Second)
	}
	if calls := h.ghCalls("dispatch:"); len(calls) != 0 {
		t.Errorf("dispatched %q while a run of the branch ran", calls)
	}
	h.wantNoPlainActions()
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

// withActionsErr makes the fixer App's ActionsEnv fail with err, or pass
// again (nil).
func (h *harness) withActionsErr(err error) {
	h.ids[fixerAppName].mu.Lock()
	h.ids[fixerAppName].actionsErr = err
	h.ids[fixerAppName].mu.Unlock()
}

// errActionsRefused is ActionsEnv's error when GitHub answers the mint
// with 422: the App, or its installation, lacks Actions.
var errActionsRefused = fmt.Errorf("identity %s: mint an Actions token: %w (The permissions requested are not granted to this installation.)",
	fixerAppName, identity.ErrActionsNotGranted)

// grantWords are the words of a fixer App GitHub will not mint a token
// with Actions write.
const grantWords = fixerAppName + " may not use Actions: grant Actions: write on the App and accept it on the installation"

// While GitHub will not mint the App a token with Actions write (422:
// identity.ErrActionsNotGranted), the look reads nothing, and the PR needs
// you with what to grant, not with the 403 words, which only a call to
// GitHub earns; the read is asked again settle_max later.
func TestAFixerAppThatMayNotUseActionsNeedsYouWithWhatToGrant(t *testing.T) {
	h := actionsAsFixerApp(t, "")
	h.withActionsErr(errActionsRefused)
	h.setHeadRuns(fixHead, noJobRun())
	h.pastSettle()
	h.tick()
	h.tick()
	v := h.wantOpen(OpenNeedsYou, "required check build did not run at a1a1a1a: magnum cannot read the head's runs ("+grantWords+
		"); run its workflow on the PR's branch, or push")
	if strings.Contains(v.Why, "403") {
		t.Errorf("why = %q", v.Why)
	}
	h.wantNoPlainActions()
	if calls := h.ghCalls("with "); len(calls) != 0 {
		t.Fatalf("calls over a token = %q", calls)
	}
	if evs := h.fixEvents("fix.open_ci_runs_error"); len(evs) != 1 {
		t.Errorf("fix.open_ci_runs_error = %d events, want one", len(evs))
	}
	mints, _ := h.ids[fixerAppName].actionsTokens()
	h.withActionsErr(nil) // granted: the next read, settle_max on, re-runs the run
	h.advance(h.e.openSettle().max)
	h.tick()
	if again, _ := h.ids[fixerAppName].actionsTokens(); again == mints {
		t.Fatal("the read was not asked again settle_max on")
	}
	if n := len(h.ghCalls("rerun:")); n != 1 {
		t.Errorf("reruns = %d once granted, want 1", n)
	}
}

// With ci_dispatch the refused mint needs you the same way, before any
// start: a start would fail on the same mint.
func TestAFixerAppThatMayNotUseActionsStartsNothing(t *testing.T) {
	h := actionsAsFixerApp(t, "ci.yml")
	h.withActionsErr(errActionsRefused)
	h.pastSettle()
	h.tick()
	h.tick()
	h.wantOpen(OpenNeedsYou, "magnum cannot read the head's runs ("+grantWords+")")
	if s := h.e.ciStartAt(h.ctx, h.pr(2)); !s.At.IsZero() || s.Failed != "" {
		t.Errorf("the start's record = %+v, want none", s)
	}
	h.wantNoPlainActions()
}

// GitHub refuses the token of the re-run (the App lost Actions after the
// read): the record says so before the mint, the PR needs you with what to
// grant in place of the 403 words, and the head is not re-run again.
func TestAReRunAsAnAppThatMayNotUseActionsFailsWithWhatToGrant(t *testing.T) {
	h := actionsAsFixerApp(t, "")
	h.setHeadRuns(fixHead, noJobRun())
	h.e.setKV(h.ctx, store.KVDaemonPaused, "1")
	h.pastSettle()
	h.tick()
	h.tick() // read under the pause, the re-run held
	h.withActionsErr(errActionsRefused)
	h.e.delKV(h.ctx, store.KVDaemonPaused)
	h.tick()
	h.tick()
	h.wantOpen(OpenNeedsYou, "required check build did not run at a1a1a1a: magnum's re-run of run 42673098082 of ci.yml failed ("+
		grantWords+"); re-run it on GitHub")
	mints, _ := h.ids[fixerAppName].actionsTokens()
	h.withActionsErr(nil)
	h.advance(30 * time.Minute)
	h.tick()
	if calls := h.ghCalls("rerun:"); len(calls) != 0 {
		t.Fatalf("reruns = %q", calls)
	}
	if again, _ := h.ids[fixerAppName].actionsTokens(); again != mints {
		t.Errorf("minted %d more after the refusal", again-mints)
	}
	if ev, ok := h.fixEvent("fix.open_ci_rerun_failed"); !ok || strings.Contains(ev.Message, "403") || !strings.Contains(ev.Message, grantWords) {
		t.Errorf("fix.open_ci_rerun_failed = %+v, %v", ev, ok)
	}
}

// After a push magnum made, a ci_dispatch_as App that may not use Actions
// cannot read the branch's runs nor start the workflow: the start records
// why, with what to grant, after one mint, and the next ticks mint no
// more (a read that fails would be asked again every tick).
func TestAPushMagnumMadeAsAnAppThatMayNotUseActionsMintsOnce(t *testing.T) {
	h, _ := pushedCIHarness(t, []string{"app/models/order.rb"}, withFixerAppClients, func(h *harness) {
		h.cfg.Repos[len(h.cfg.Repos)-1].CIDispatchAs = fixerAppName
	})
	h.withActionsErr(errActionsRefused)
	h.advance(ciPushGrace)
	h.tick()
	h.advance(time.Minute)
	h.tick()
	h.tick()
	if mints, _ := h.ids[fixerAppName].actionsTokens(); mints != 1 {
		t.Errorf("mints = %d, want one", mints)
	}
	s := h.e.ciStartAt(h.ctx, h.pr(2))
	if s.Head != fixParentMerge || s.At.IsZero() || s.Failed != grantWords {
		t.Errorf("the start's record = %+v, want the refusal's words", s)
	}
	if evs := h.fixEvents("fix.push_ci_dispatch_failed"); len(evs) != 1 || strings.Contains(evs[0].Message, "403") {
		t.Errorf("fix.push_ci_dispatch_failed = %+v", evs)
	}
	h.wantNoPlainActions()
}
