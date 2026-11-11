package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/staging"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// otherHead is the head of a coworker's PR #3 in the deploy tests.
const otherHead = "d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3"

// deployHarness is a daemon whose talkable/talkable has the three staging
// envs (f its staging client; nil: none), the fixer App, its mirrors
// under /mirrors, the operator's own PR #2 at fixHead and alice's #3.
func deployHarness(t *testing.T, f *fakeStaging, mods ...func(*harness)) *harness {
	t.Helper()
	h := newHarness(t, append([]func(*harness){withStagings(f), withFixerApp, withMirrors}, mods...)...)
	h.open(prSpec{n: 2, head: fixHead, author: "zhuravel"}, prSpec{n: 3, head: otherHead, author: "alice"})
	h.startup()
	h.tick()
	return h
}

// stagingRequest queues a request of kind with p and returns it once the
// daemon answered it (off the tick).
func (h *harness) stagingRequest(kind string, p any) store.Request {
	h.t.Helper()
	id := h.enqueue(kind, p)
	h.tick()
	h.e.stagingWG.Wait()
	return h.request(id)
}

// deployAnswer is a done staging_deploy request's answer.
func deployAnswer(t *testing.T, r store.Request) StagingDeployAnswer {
	t.Helper()
	if r.State != store.RequestDone {
		t.Fatalf("request %s: %q", r.State, deref(r.Result))
	}
	ans, ok := ParseStagingDeployAnswer(deref(r.Result))
	if !ok {
		t.Fatalf("the answer does not parse: %q", deref(r.Result))
	}
	return ans
}

var (
	envVoid    = staging.Env{Name: "void", Branch: "staging-void"}
	envBart    = staging.Env{Name: "bart", Branch: "staging-bart"}
	envBastion = staging.Env{Name: "bastion", Branch: "staging-bastion"}
)

// mixedPlan is a plan where void merges cleanly and adds a migration, bart
// has the head already and bastion conflicts.
func mixedPlan(pr staging.PR, envs []staging.Env) (staging.DeployPlan, error) {
	p := staging.DeployPlan{PR: pr}
	for _, e := range envs {
		ep := staging.EnvPlan{Env: e, Tip: fakeTip, Clean: true}
		switch e.Name {
		case "void":
			ep.Migrations = []string{"db/migrate/20261105_add_coupons.rb"}
		case "bart":
			ep.Clean, ep.Why = false, "already there: staging-bart holds PR #2's head"
		case "bastion":
			ep.Clean, ep.Conflicts = false, []string{"db/schema.rb", "app/models/\x1b[31mcoupon.rb"}
		}
		p.Envs = append(p.Envs, ep)
	}
	return p, nil
}

// A dry run of a staging deploy plans every env asked ("all": the
// repository's, in the config's order) on the PR's head and answers what
// each would get: a clean merge and its migrations, the head there
// already, or the conflicted paths (made safe for a terminal); who pushes
// (the fixer App's bot) and who asked (the operator). It deploys nothing.
func TestAStagingDeployDryRunAnswersWhatEachEnvWouldGet(t *testing.T) {
	f := &fakeStaging{plan: mixedPlan}
	h := deployHarness(t, f)
	r := h.stagingRequest(ReqStagingDeploy, StagingDeployPayload{PRTarget: PRTarget{Ref: "2"}, Envs: []string{"all"}, DryRun: true})
	ans := deployAnswer(t, r)
	if ans.Ref != "talkable/talkable#2" || ans.Head != fixHead || ans.Pusher != fixerAppLogin || ans.AskedBy != "zhuravel" || !ans.DryRun {
		t.Fatalf("answer %+v", ans)
	}
	if got := len(f.plans); got != 1 {
		t.Fatalf("%d plans, want 1", got)
	}
	if p := f.plans[0]; p.pr.Repo != "talkable/talkable" || p.pr.Number != 2 || p.pr.Head != fixHead || p.pr.Base != "master" ||
		!slices.Equal(p.envs, []staging.Env{envVoid, envBart, envBastion}) {
		t.Fatalf("Plan(%+v, %v)", p.pr, p.envs)
	}
	if len(ans.Envs) != 3 || ans.Envs[0].Env != "void" || ans.Envs[1].Env != "bart" || ans.Envs[2].Env != "bastion" {
		t.Fatalf("envs %+v", ans.Envs)
	}
	if v := ans.Envs[0]; !v.Clean || v.Branch != "staging-void" || v.Tip != fakeTip || !slices.Equal(v.Migrations, []string{"db/migrate/20261105_add_coupons.rb"}) {
		t.Errorf("void %+v", v)
	}
	if b := ans.Envs[1]; b.Clean || !strings.HasPrefix(b.Why, "already there") {
		t.Errorf("bart %+v", b)
	}
	if b := ans.Envs[2]; b.Clean || len(b.Conflicts) != 2 || strings.ContainsRune(strings.Join(b.Conflicts, ""), '\x1b') {
		t.Errorf("bastion %+v", b)
	}
	if !strings.HasPrefix(lastLine(deref(r.Result)), "dry run: would merge talkable/talkable#2 at a1a1a1a") {
		t.Errorf("the answer's words: %q", lastLine(deref(r.Result)))
	}
	if len(f.deploys) != 0 {
		t.Fatalf("a dry run deployed: %+v", f.deploys)
	}
}

// The deploy itself plans the envs asked again and starts their effects as
// the CLI (staging.ByCLI: the operator's terminal) on the head the question
// named, after checking that each still merges cleanly and adds nothing the
// question did not name; the answer names the effects, and an event
// records who asked.
func TestAStagingDeployStartsTheEffectsOfTheEnvsAsked(t *testing.T) {
	f := &fakeStaging{plan: mixedPlan}
	h := deployHarness(t, f)
	r := h.stagingRequest(ReqStagingDeploy, StagingDeployPayload{PRTarget: PRTarget{Ref: "2"}, Envs: []string{"void"}, Head: fixHead,
		Seen: []StagingSeen{{Env: "void", Migrations: []string{"db/migrate/20261105_add_coupons.rb"}}}})
	ans := deployAnswer(t, r)
	if len(f.deploys) != 1 {
		t.Fatalf("%d deploys, want 1", len(f.deploys))
	}
	d := f.deploys[0]
	if d.by != staging.ByCLI || d.plan.PR.Head != fixHead || len(d.plan.Envs) != 1 || d.plan.Envs[0].Env != envVoid {
		t.Fatalf("Deploy(%+v, %q)", d.plan, d.by)
	}
	if ans.DryRun || !slices.Equal(ans.Effects, []string{"void-2-20261005t100000-abcd"}) {
		t.Fatalf("answer %+v", ans)
	}
	if got := lastLine(deref(r.Result)); !strings.HasPrefix(got, "deploying talkable/talkable#2 at a1a1a1a to void as fixer-app[bot]") {
		t.Errorf("the answer's words: %q", got)
	}
	asked := h.events("staging.deploy_asked")
	if len(asked) != 1 || !strings.Contains(asked[0].Message, "asked by zhuravel") || !strings.Contains(asked[0].Message, "void-2-20261005t100000-abcd") {
		t.Fatalf("asked events %+v", asked)
	}
}

// With the review daemon's resolver, an env whose merge conflicts at the
// plan takes the deploy too: the dry run says that the resolver merges it
// first (at most 2 tries), beside the migrations the merge adds, and the
// deploy starts its effect, whose merge step hands the conflict to the
// resolver. A daemon without one refuses it, naming the paths.
func TestAConflictAtThePlanGoesToTheResolver(t *testing.T) {
	f := &fakeStaging{plan: func(pr staging.PR, envs []staging.Env) (staging.DeployPlan, error) {
		p, err := mixedPlan(pr, envs)
		for i := range p.Envs {
			if p.Envs[i].Env == envBastion {
				p.Envs[i].Migrations = []string{"db/migrate/20261106_add_codes.rb"}
			}
		}
		return p, err
	}}
	h := deployHarness(t, f, func(h *harness) { h.d.stagingResolver = &stagingResolver{} })
	ans := deployAnswer(t, h.stagingRequest(ReqStagingDeploy, StagingDeployPayload{PRTarget: PRTarget{Ref: "2"}, Envs: []string{"bastion"}, DryRun: true}))
	if len(ans.Envs) != 1 {
		t.Fatalf("envs %+v", ans.Envs)
	}
	b := ans.Envs[0]
	if b.Clean || !b.Resolves || !b.Deployable() || len(b.Conflicts) != 2 {
		t.Fatalf("bastion %+v: a conflict the resolver merges takes the deploy", b)
	}
	if says := StagingPlanSays(b); !strings.HasPrefix(says, "conflicts in db/schema.rb, app/models/") ||
		!strings.Contains(says, "the resolver merges it first (at most 2 tries)") || !strings.Contains(says, "adds 1 migration: db/migrate/20261106_add_codes.rb") {
		t.Errorf("bastion's plan says %q", says)
	}
	ans = deployAnswer(t, h.stagingRequest(ReqStagingDeploy, StagingDeployPayload{PRTarget: PRTarget{Ref: "2"}, Envs: []string{"bastion"}, Head: fixHead,
		Seen: []StagingSeen{{Env: "bastion", Migrations: []string{"db/migrate/20261106_add_codes.rb"}}}}))
	if len(f.deploys) != 1 || len(f.deploys[0].plan.Envs) != 1 || f.deploys[0].plan.Envs[0].Env != envBastion || !slices.Equal(ans.Effects, []string{"bastion-2-20261005t100000-abcd"}) {
		t.Fatalf("deploys %+v, answer %+v: the conflicted env's effect starts", f.deploys, ans)
	}

	f = &fakeStaging{plan: mixedPlan}
	h = deployHarness(t, f)
	ans = deployAnswer(t, h.stagingRequest(ReqStagingDeploy, StagingDeployPayload{PRTarget: PRTarget{Ref: "2"}, Envs: []string{"bastion"}, DryRun: true}))
	if b := ans.Envs[0]; b.Resolves || b.Deployable() {
		t.Fatalf("bastion %+v without a resolver", b)
	}
	r := h.stagingRequest(ReqStagingDeploy, StagingDeployPayload{PRTarget: PRTarget{Ref: "2"}, Envs: []string{"bastion"}, Head: fixHead})
	if r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "bastion conflicts in db/schema.rb") || !strings.Contains(deref(r.Result), "no resolver") {
		t.Fatalf("%s %q: without a resolver a conflict is refused, naming the paths", r.State, deref(r.Result))
	}
	if len(f.deploys) != 0 {
		t.Fatalf("deployed %+v", f.deploys)
	}
}

// What a deploy refuses before it deploys anything: staging off (no
// client), a coworker's PR (own PRs only for now), a PR merged or closed,
// one from a fork, one whose head branch is master, main or a staging
// branch (internal/staging checks none of these: the engine is the only
// guard, and it plans nothing for them), an env the repository does not
// configure, no env, a head that moved since the question, an env that
// cannot take the merge now (the head there already, a conflict), a merge
// that adds a migration or a workflow change the question did not name,
// and Plan's own refusal.
func TestAStagingDeployRefusesWhatItCannotDeployAsAsked(t *testing.T) {
	storetest.Parallel(t)
	seenVoid := []StagingSeen{{Env: "void", Migrations: []string{"db/migrate/20261105_add_coupons.rb"}}}
	headRef := func(ref string) func(*store.PRUpdate) { return func(u *store.PRUpdate) { u.Set("head_ref", ref) } }
	for _, c := range []struct {
		name, want string
		edit       func(*store.PRUpdate) // PR #2 in the registry
	}{
		{"a merged PR", "talkable/talkable#2 is not open", func(u *store.PRUpdate) { u.Set("gh_state", store.GHMerged) }},
		{"a closed PR", "talkable/talkable#2 is not open", func(u *store.PRUpdate) { u.Set("gh_state", store.GHClosed) }},
		{"a fork's PR", "comes from a fork", func(u *store.PRUpdate) { u.Set("is_cross_repo", true) }},
		{"a head branch master", "head branch is master", headRef("master")},
		{"a head branch main", "head branch is main", headRef("main")},
		{"a staging head branch", "head branch is staging-void", headRef("staging-void")},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeStaging{}
			h := deployHarness(t, f)
			if err := h.st.UpdatePR(h.ctx, h.pr(2).ID, c.edit); err != nil {
				t.Fatal(err)
			}
			// Handled at once: a tick's poll would tell the PR again.
			for _, dry := range []bool{true, false} {
				p := StagingDeployPayload{PRTarget: PRTarget{Ref: "2"}, Envs: []string{"void"}, Head: fixHead, Seen: seenVoid, DryRun: dry}
				if _, err := h.e.requestStagingDeploy(h.ctx, p, h.clock.Now()); err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("dry run %v: %v, want a refusal saying %q", dry, err, c.want)
				}
			}
			if len(f.plans) != 0 || len(f.deploys) != 0 {
				t.Fatalf("planned %+v, deployed %+v", f.plans, f.deploys)
			}
		})
	}
	for _, c := range []struct {
		name string
		f    *fakeStaging
		p    StagingDeployPayload
		want string
	}{
		{"off", nil, StagingDeployPayload{Envs: []string{"void"}, DryRun: true}, "staging deploys are off"},
		{"a coworker's PR", &fakeStaging{}, StagingDeployPayload{PRTarget: PRTarget{Ref: "3"}, Envs: []string{"void"}, DryRun: true},
			"only your own PRs"},
		{"an unknown env", &fakeStaging{}, StagingDeployPayload{Envs: []string{"production"}, DryRun: true},
			`"production" is not a staging env of talkable/talkable (void, bart, bastion)`},
		{"no env", &fakeStaging{}, StagingDeployPayload{DryRun: true}, "which staging envs"},
		{"no head", &fakeStaging{}, StagingDeployPayload{Envs: []string{"void"}}, "names no head"},
		{"a moved head", &fakeStaging{}, StagingDeployPayload{Envs: []string{"void"}, Head: fixPushed},
			"the head moved since you were asked: b2b2b2b is now a1a1a1a"},
		{"already there", &fakeStaging{plan: mixedPlan}, StagingDeployPayload{Envs: []string{"bart"}, Head: fixHead},
			"bart: already there"},
		{"a conflict", &fakeStaging{plan: mixedPlan}, StagingDeployPayload{Envs: []string{"bastion"}, Head: fixHead},
			"bastion conflicts in db/schema.rb, app/models/"},
		{"an unseen migration", &fakeStaging{plan: mixedPlan}, StagingDeployPayload{Envs: []string{"void"}, Head: fixHead, Seen: []StagingSeen{{Env: "void"}}},
			"void now adds db/migrate/20261105_add_coupons.rb, which the question did not name"},
		{"an unseen workflow change", &fakeStaging{plan: func(pr staging.PR, envs []staging.Env) (staging.DeployPlan, error) {
			p, _ := mixedPlan(pr, envs)
			p.Envs[0].Workflows = true
			return p, nil
		}}, StagingDeployPayload{Envs: []string{"void"}, Head: fixHead, Seen: seenVoid}, "void now changes .github/workflows"},
		{"an env the question did not see", &fakeStaging{}, StagingDeployPayload{Envs: []string{"void", "bart"}, Head: fixHead, Seen: seenVoid},
			"bart was not in the question"},
		{"plan's refusal", &fakeStaging{plan: func(staging.PR, []staging.Env) (staging.DeployPlan, error) {
			return staging.DeployPlan{}, errors.New("staging talkable/talkable: plan: PR #2's head on the remote is b2b2b2b2b2b2, not a1a1a1a1a1a1")
		}}, StagingDeployPayload{Envs: []string{"void"}, Head: fixHead}, "PR #2's head on the remote is b2b2b2b2b2b2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := deployHarness(t, c.f)
			if c.p.Ref == "" {
				c.p.Ref = "2"
			}
			r := h.stagingRequest(ReqStagingDeploy, c.p)
			if r.State != store.RequestFailed || !strings.Contains(deref(r.Result), c.want) {
				t.Fatalf("%s %q, want a refusal saying %q", r.State, deref(r.Result), c.want)
			}
			if c.f != nil && len(c.f.deploys) != 0 {
				t.Fatalf("deployed %+v", c.f.deploys)
			}
		})
	}
}

// awaitPlans waits until f was asked n Plans.
func awaitPlans(t *testing.T, f *fakeStaging, n int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); f.planCount() < n; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d staging Plans, want %d", f.planCount(), n)
		}
	}
}

// voidDeploy is a deploy of PR #2 to void, as its y/N bound it.
var voidDeploy = StagingDeployPayload{PRTarget: PRTarget{Ref: "2"}, Envs: []string{"void"}, Head: fixHead, Seen: []StagingSeen{{Env: "void"}}}

// A staging request is answered off the tick: a plan that waits (a fetch,
// a mirror a step holds) never holds the tick. The daemon's stop cuts it
// and leaves it pending, nothing deployed, for the next start to answer.
func TestAStagingRequestRunsOffTheTickAndAStopLeavesItPending(t *testing.T) {
	f := &fakeStaging{planGate: make(chan struct{})}
	h := deployHarness(t, f)
	id := h.enqueue(ReqStagingDeploy, voidDeploy)
	h.stagingTick() // fails when the tick waits for the plan
	awaitPlans(t, f, 1)
	if r := h.request(id); r.State != store.RequestPending {
		t.Fatalf("while its plan waits the request is %s %q", r.State, deref(r.Result))
	}
	h.e.stagingStop()
	h.e.stagingWG.Wait()
	if r := h.request(id); r.State != store.RequestPending {
		t.Fatalf("the stop left the request %s %q, want it pending", r.State, deref(r.Result))
	}
	if n := f.deployCount(); n != 0 {
		t.Fatalf("%d deploys after the stop", n)
	}
}

// A deploy runs only while the operator still waits for its answer: one
// asked longer ago than StagingDeployWait (the CLI's wait) is refused and
// deploys nothing, whether it waited in the queue (a stop cut it and the
// next start found it pending) or behind a slow plan. A dry run deploys
// nothing, so an old one is still answered.
func TestAStagingDeployAskedTooLongAgoDeploysNothing(t *testing.T) {
	const want = "asked 11 minutes ago, longer than a deploy waits for its answer (10 minutes): nothing was deployed; ask again"
	f := &fakeStaging{}
	h := deployHarness(t, f)
	id := h.enqueue(ReqStagingDeploy, voidDeploy)
	h.advance(StagingDeployWait + time.Minute)
	if r := h.stagingRequest(ReqStagingDeploy, StagingDeployPayload{PRTarget: PRTarget{Ref: "2"}, Envs: []string{"void"}, DryRun: true}); r.State != store.RequestDone {
		t.Fatalf("an old dry run is %s %q", r.State, deref(r.Result))
	}
	if r := h.request(id); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), want) {
		t.Fatalf("an old deploy is %s %q, want %q", r.State, deref(r.Result), want)
	}
	if n := f.planCount(); n != 1 {
		t.Errorf("%d plans, want the dry run's alone", n)
	}

	f = &fakeStaging{planGate: make(chan struct{})}
	h = deployHarness(t, f)
	id = h.enqueue(ReqStagingDeploy, voidDeploy)
	h.stagingTick()
	awaitPlans(t, f, 1)
	h.advance(StagingDeployWait + time.Minute)
	close(f.planGate)
	h.e.stagingWG.Wait()
	if r := h.request(id); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), want) {
		t.Fatalf("a deploy behind a slow plan is %s %q, want %q", r.State, deref(r.Result), want)
	}
	if n := f.deployCount(); n != 0 {
		t.Fatalf("%d deploys", n)
	}
}

// tipGit is the engine's git with the staging mirror's branch tips
// (RevParse of "<dir>:<ref>").
type tipGit struct {
	fakeGit
	tips map[string]string
}

func (g tipGit) RevParse(ctx context.Context, dir, ref string) (string, error) {
	if sha, ok := g.tips[dir+":"+ref]; ok {
		return sha, nil
	}
	return g.fakeGit.RevParse(ctx, dir, ref)
}

// While the default branch's ci.yml runs at its tip in the mirror (a
// production deploy may be building), the plan says so, for the question's
// warning: three staging builds slow it. A finished run says nothing.
func TestAStagingPlanWarnsWhileTheDefaultBranchsCIRuns(t *testing.T) {
	const tip = "e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4"
	f := &fakeStaging{}
	h := deployHarness(t, f, func(h *harness) {
		h.d.Git = tipGit{tips: map[string]string{stagingMirror + ":refs/heads/main": tip, stagingMirror + ":refs/heads/master": tip}}
	})
	h.gh.mu.Lock()
	h.gh.commitChecks = map[string][]github.Check{tip: {{Name: "rspec", State: github.CheckPending, WorkflowFile: "ci.yml"}}}
	h.gh.mu.Unlock()
	ans := deployAnswer(t, h.stagingRequest(ReqStagingDeploy, StagingDeployPayload{PRTarget: PRTarget{Ref: "2"}, Envs: []string{"void"}, DryRun: true}))
	if !strings.Contains(ans.MasterCI, "CI is running at e4e4e4e") || !strings.Contains(ans.MasterCI, "production deploy") {
		t.Fatalf("master CI %q", ans.MasterCI)
	}
	h.gh.mu.Lock()
	h.gh.commitChecks = map[string][]github.Check{tip: {{Name: "rspec", State: github.CheckPassed, WorkflowFile: "ci.yml"}}}
	h.gh.mu.Unlock()
	if ans := deployAnswer(t, h.stagingRequest(ReqStagingDeploy, StagingDeployPayload{PRTarget: PRTarget{Ref: "2"}, Envs: []string{"void"}, DryRun: true})); ans.MasterCI != "" {
		t.Fatalf("a finished CI warns: %q", ans.MasterCI)
	}
}

// A cancel stops the effect it names, through the client (whoever started
// it); a refusal of the client is the request's; without staging it is
// refused.
func TestAStagingCancelStopsTheEffectItNames(t *testing.T) {
	f := &fakeStaging{}
	h := deployHarness(t, f)
	r := h.stagingRequest(ReqStagingCancel, StagingCancelPayload{Effect: "void-2-20261005t100000-abcd"})
	if r.State != store.RequestDone || !slices.Equal(f.cancels, []staging.EffectID{"void-2-20261005t100000-abcd"}) {
		t.Fatalf("%s %q, cancels %v", r.State, deref(r.Result), f.cancels)
	}
	f.cancelErr = errors.New("staging: no deploy effect void-9-x")
	if r := h.stagingRequest(ReqStagingCancel, StagingCancelPayload{Effect: "void-9-x"}); r.State != store.RequestFailed ||
		!strings.Contains(deref(r.Result), "no deploy effect") {
		t.Fatalf("%s %q", r.State, deref(r.Result))
	}
	h = deployHarness(t, nil)
	if r := h.stagingRequest(ReqStagingCancel, StagingCancelPayload{Effect: "void-2-x"}); r.State != store.RequestFailed ||
		!strings.Contains(deref(r.Result), "staging deploys are off") {
		t.Fatalf("%s %q", r.State, deref(r.Result))
	}
}

// lastLine is s's last line.
func lastLine(s string) string {
	s = strings.TrimRight(s, "\n")
	return s[strings.LastIndexByte(s, '\n')+1:]
}
