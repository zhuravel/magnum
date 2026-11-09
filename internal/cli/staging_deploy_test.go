package cli

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

// stagingMigration is the migration the fake daemon's plan of void adds.
const stagingMigration = "db/migrate/20261105_add_coupons.rb"

// stagingPlanOf is the fake daemon's answer to a staging deploy of p: void
// merges cleanly and adds stagingMigration, bart has the head already; a
// deploy starts an effect per env it names.
func stagingPlanOf(t *testing.T, p engine.StagingDeployPayload, head string) string {
	t.Helper()
	ans := engine.StagingDeployAnswer{Ref: "talkable/talkable#5", Head: head, DryRun: p.DryRun, Pusher: "fixer-app[bot]", AskedBy: "zhuravel"}
	for _, env := range p.Envs {
		switch env {
		case "void", engine.StagingAll:
			ans.Envs = append(ans.Envs, engine.StagingPlanEnv{Env: "void", Branch: "staging-void", Tip: "7c7c7c7", Clean: true,
				Migrations: []string{stagingMigration}})
		}
		switch env {
		case "bart", engine.StagingAll:
			ans.Envs = append(ans.Envs, engine.StagingPlanEnv{Env: "bart", Branch: "staging-bart",
				Why: "already there: staging-bart holds PR #5's head"})
		}
	}
	ans.Text = "dry run: would merge talkable/talkable#5"
	if !p.DryRun {
		for _, e := range ans.Envs {
			ans.Effects = append(ans.Effects, e.Env+"-5-20261005t100000-abcd")
		}
		ans.Text = "deploying talkable/talkable#5 at abc1234 to " + strings.Join(p.Envs, " and ") + " as fixer-app[bot]"
	}
	b, err := json.Marshal(ans)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n" + ans.Text
}

// stagingDaemon answers each pending staging request as the daemon does:
// a deploy with stagingPlanOf, a cancel with its words.
func (h *actHarness) stagingDaemon(head string) {
	h.onSleep = func(h *actHarness) {
		for _, r := range h.requests() {
			if r.State != store.RequestPending {
				continue
			}
			res := "cancelled staging deploy void-5-x"
			if r.Kind == engine.ReqStagingDeploy {
				res = stagingPlanOf(h.t, actDecode[engine.StagingDeployPayload](h.t, r.Payload), head)
			}
			if err := h.st.CompleteRequest(h.ctx, r.ID, store.RequestDone, res); err != nil {
				h.t.Fatal(err)
			}
		}
	}
}

// `magnum staging <pr> --deploy <envs>` asks the daemon's plan first (a
// dry run, which deploys nothing) and puts it as one y/N: the envs that take
// the merge, the head, the bot that pushes, the migrations Jenkins then
// runs on each env's shared database, and the envs left out and why. Only y
// queues the deploy, bound to that head and to what the question named.
func TestStagingDeployAsksWithTheDaemonsPlanThenQueuesTheDeploy(t *testing.T) {
	h, prs := stagingHarness(t)
	h.pid = 4242
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.stagingDaemon(prs[5].HeadSHA)

	h.stdin("n\n")
	if code := h.cmd("staging", "5", "--deploy", "void,bart"); code != 1 {
		t.Fatalf("exit %d: %s%s", code, h.out.String(), h.errb.String())
	}
	actContains(t, h.out.String(), "Deploy talkable#5's head abc1234 to void", "fixer-app[bot] merges it into staging-void and pushes",
		"Jenkins then runs "+stagingMigration+" on void's shared database", "bart is left out (already there", "[y/N]")
	actContains(t, h.errb.String(), "nothing queued")
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqStagingDeploy {
		t.Fatalf("requests %+v, want the plan alone", reqs)
	}
	if p := actDecode[engine.StagingDeployPayload](t, reqs[0].Payload); !p.DryRun || p.Number != 5 || !slices.Equal(p.Envs, []string{"void", "bart"}) {
		t.Fatalf("the plan's payload %+v", p)
	}

	h.stdin("y\n")
	h.out.Reset()
	if code := h.cmd("staging", "5", "--deploy", "void,bart"); code != 0 {
		t.Fatalf("exit %d: %s%s", code, h.out.String(), h.errb.String())
	}
	reqs = h.requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests, want a plan and the deploy more", len(reqs))
	}
	p := actDecode[engine.StagingDeployPayload](t, reqs[2].Payload)
	if p.DryRun || !slices.Equal(p.Envs, []string{"void"}) || p.Head != prs[5].HeadSHA || len(p.Seen) != 1 || p.Seen[0].Env != "void" ||
		!slices.Equal(p.Seen[0].Migrations, []string{stagingMigration}) {
		t.Fatalf("the deploy's payload %+v", p)
	}
	actContains(t, h.out.String(), "deploying talkable/talkable#5 at abc1234 to void as fixer-app[bot]")
	if strings.Contains(h.out.String(), `"ref"`) {
		t.Errorf("the answer's JSON is printed:\n%s", h.out.String())
	}
}

// An env whose merge conflicts takes the deploy when the daemon's resolver
// merges it: the y/N names it with the others and says, in a few words,
// that the resolver merges it first (at most 2 tries); its migrations are
// named as a clean env's are, and the deploy is bound to it. One without
// the resolver is left out, naming the paths.
func TestTheStagingQuestionSaysTheResolverMergesAConflict(t *testing.T) {
	ans := engine.StagingDeployAnswer{Ref: "talkable/talkable#5", Head: "abc1234abc1234abc1234abc1234abc1234abc12", Pusher: "fixer-app[bot]",
		Envs: []engine.StagingPlanEnv{
			{Env: "void", Branch: "staging-void", Tip: "7c7c7c7", Clean: true},
			{Env: "bastion", Branch: "staging-bastion", Tip: "8d8d8d8", Conflicts: []string{"db/schema.rb", "app/models/coupon.rb"}, Resolves: true,
				Migrations: []string{stagingMigration}},
		}}
	q, envs, seen, ok := StagingQuestion("talkable#5", ans)
	if !ok || !slices.Equal(envs, []string{"void", "bastion"}) || len(seen) != 2 || seen[1].Env != "bastion" ||
		!slices.Equal(seen[1].Migrations, []string{stagingMigration}) {
		t.Fatalf("StagingQuestion = %q, %v, %+v, %v", q, envs, seen, ok)
	}
	actContains(t, q, "Deploy talkable#5's head abc1234 to void and bastion",
		"bastion conflicts in db/schema.rb, app/models/coupon.rb: the resolver merges it first (at most 2 tries)",
		"Jenkins then runs "+stagingMigration+" on bastion's shared database")
	if strings.Contains(q, "left out") {
		t.Errorf("a conflict the resolver merges is left out: %q", q)
	}

	ans.Envs[1].Resolves = false
	q, envs, _, ok = StagingQuestion("talkable#5", ans)
	if !ok || !slices.Equal(envs, []string{"void"}) {
		t.Fatalf("StagingQuestion without a resolver = %q, %v, %v", q, envs, ok)
	}
	actContains(t, q, "bastion is left out (conflicts in db/schema.rb, app/models/coupon.rb)")
}

// A plan where no env takes the merge asks nothing and queues nothing: it
// says why for each.
func TestStagingDeployWithNothingToDeployQueuesNothing(t *testing.T) {
	h, prs := stagingHarness(t)
	h.pid = 4242
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.stagingDaemon(prs[5].HeadSHA)
	if code := h.cmd("staging", "5", "--deploy", "bart"); code != 1 {
		t.Fatalf("exit %d: %s%s", code, h.out.String(), h.errb.String())
	}
	actContains(t, h.errb.String(), "nothing to deploy", "bart: already there")
	if reqs := h.requests(); len(reqs) != 1 {
		t.Fatalf("%d requests, want the plan alone", len(reqs))
	}
}

// Without a terminal to ask y/N on (an agent's Bash tool, a pipe, a
// script), a deploy is refused unless --yes says so: nothing is planned
// and nothing queued, whatever the output; --dry-run still prints the plan.
func TestStagingDeployWithoutATerminalNeedsYes(t *testing.T) {
	h, prs := stagingHarness(t)
	h.pid = 4242
	h.stagingDaemon(prs[5].HeadSHA)
	for _, tty := range []struct{ in, out bool }{{false, false}, {true, false}, {false, true}} {
		h.d.StdinTTY, h.d.StdoutTTY = tty.in, tty.out
		for _, args := range [][]string{{"5", "--deploy", "all"}, {"5", "--deploy", "void", "--json"}} {
			h.errb.Reset()
			if code := h.cmd("staging", args...); code != 2 {
				t.Errorf("stdin tty %v, stdout tty %v, %v: exit %d, want 2: %s", tty.in, tty.out, args, code, h.errb.String())
			}
			actContains(t, h.errb.String(), "pass --yes", "nothing queued")
		}
	}
	if reqs := h.requests(); len(reqs) != 0 {
		t.Fatalf("queued %+v without a terminal or --yes", reqs)
	}
}

// --dry-run prints the daemon's plan per env and queues nothing more;
// --json prints the answer. --yes asks nothing and sends the deploy alone,
// bound to the registry's head, with nothing seen.
func TestStagingDeployDryRunAndYes(t *testing.T) {
	h, prs := stagingHarness(t)
	h.pid = 4242
	h.stagingDaemon(prs[5].HeadSHA)
	if code := h.cmd("staging", "5", "--deploy", "all", "--dry-run"); code != 0 {
		t.Fatalf("exit %d: %s%s", code, h.out.String(), h.errb.String())
	}
	actContains(t, h.out.String(), "void  clean, adds 1 migration: "+stagingMigration, "bart  already there")
	h.out.Reset()
	if code := h.cmd("staging", "5", "--deploy", "void", "--dry-run", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	var ans engine.StagingDeployAnswer
	if err := json.Unmarshal(h.out.Bytes(), &ans); err != nil || ans.Ref != "talkable/talkable#5" || !ans.DryRun || len(ans.Envs) != 1 {
		t.Fatalf("--json %+v (%v)\n%s", ans, err, h.out.String())
	}
	n := len(h.requests())
	h.out.Reset()
	if code := h.cmd("staging", "5", "--deploy", "void", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != n+1 {
		t.Fatalf("--yes sent %d requests, want the deploy alone", len(reqs)-n)
	}
	if p := actDecode[engine.StagingDeployPayload](t, reqs[n].Payload); p.DryRun || p.Head != prs[5].HeadSHA || p.Seen != nil ||
		!slices.Equal(p.Envs, []string{"void"}) {
		t.Fatalf("--yes payload %+v", p)
	}
}

// --cancel queues the cancel of the effect it names; the flags that go
// together are checked first.
func TestStagingCancelAndTheFlagsThatGoTogether(t *testing.T) {
	h, _ := stagingHarness(t)
	h.pid = 4242
	h.stagingDaemon("")
	if code := h.cmd("staging", "--cancel", "void-5-20261005t100000-abcd"); code != 0 {
		t.Fatalf("exit %d: %s%s", code, h.out.String(), h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqStagingCancel ||
		actDecode[engine.StagingCancelPayload](t, reqs[0].Payload).Effect != "void-5-20261005t100000-abcd" {
		t.Fatalf("requests %+v", reqs)
	}
	actContains(t, h.out.String(), "cancelled staging deploy")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"5", "--cancel", "void-5-x"}, "--cancel names an effect, not a PR"},
		{[]string{"--deploy", "void"}, "--deploy needs a PR"},
		{[]string{"5", "--dry-run"}, "--dry-run and --yes go with --deploy"},
		{[]string{"--cancel", "void-5-x", "--deploy", "void"}, "--cancel goes without --deploy"},
		{[]string{"5", "--deploy", ""}, "--deploy names the envs"},
	} {
		h.errb.Reset()
		if code := h.cmd("staging", c.args...); code != 2 {
			t.Errorf("%v: exit %d, want 2: %s", c.args, code, h.errb.String())
		}
		actContains(t, h.errb.String(), c.want)
	}
}

// The screens' E runs the command's own path: its plan hands back the
// question (Ask) and the binding (Staging: the envs that take the merge,
// the head, what the question named), and the deploy the screen then
// asked y/N for queues the deploy alone, so bound, never asking again; a
// plan with nothing to deploy is the screen's error, with no question.
func TestScreenStagingDeployPlansThenDeploysBoundToThePlan(t *testing.T) {
	h, prs := stagingHarness(t)
	h.pid = 4242
	h.stagingDaemon(prs[5].HeadSHA)
	acts := newScreenActions(h.c)
	defer acts.Close()
	ctx := context.Background()

	res, err := acts.StagingPlan(ctx, "talkable#5", []string{"void", "bart"})
	if err != nil || !strings.HasPrefix(res.Ask, "Deploy talkable#5's head abc1234 to void: fixer-app[bot] merges it into staging-void") ||
		res.Staging == nil || !slices.Equal(res.Staging.Envs, []string{"void"}) || res.Staging.Head != prs[5].HeadSHA ||
		len(res.Staging.Seen) != 1 || !slices.Equal(res.Staging.Seen[0].Migrations, []string{stagingMigration}) {
		t.Fatalf("the plan = %+v, %v", res, err)
	}
	if res, err = acts.StagingDeploy(ctx, "talkable#5", *res.Staging); err != nil || res.Ask != "" ||
		!strings.Contains(res.Text, "deploying talkable/talkable#5 at abc1234 to void") {
		t.Fatalf("the deploy = %+v, %v", res, err)
	}
	reqs := h.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want the plan and the deploy", len(reqs))
	}
	if p := actDecode[engine.StagingDeployPayload](t, reqs[1].Payload); p.DryRun || p.Head != prs[5].HeadSHA || !slices.Equal(p.Envs, []string{"void"}) ||
		len(p.Seen) != 1 || p.Seen[0].Env != "void" {
		t.Fatalf("the deploy's payload %+v", p)
	}
	if res, err := acts.StagingPlan(ctx, "talkable#5", []string{"bart"}); err == nil || res.Ask != "" || !strings.Contains(err.Error(), "nothing to deploy") {
		t.Errorf("a plan with nothing to deploy = %+v, %v", res, err)
	}
}

// The dashboard's rows carry their open PR's staging status, as the board's
// do, for E's chooser; none without staging envs.
func TestTheDashboardRowsCarryTheirPRsStagings(t *testing.T) {
	h, prs := stagingHarness(t)
	if err := h.st.SetKV(h.ctx, store.KVDaemonLastTick, store.FormatTime(time.Now())); err != nil { // the dashboard reads the wall clock
		t.Fatal(err)
	}
	pr := prs[5]
	r := statusReport{Queue: []statusPRLine{{Repo: "talkable/talkable", Number: 5, rec: &pr}}}
	data := statusDashData(r, "talkable/talkable")
	statusDashStagings(h.ctx, statusDeps{Store: h.st, Config: h.d.Cfg}, r, &data)
	if s := data.Queue[0].Stagings; len(s) != 2 || s[0].Env != "void" || s[0].State != "latest" || s[1].Env != "bart" {
		t.Fatalf("the queue row's stagings %+v", s)
	}
	h.d.Cfg.Factory.Repos = nil
	data = statusDashData(r, "talkable/talkable")
	statusDashStagings(h.ctx, statusDeps{Store: h.st, Config: h.d.Cfg}, r, &data)
	if s := data.Queue[0].Stagings; s != nil {
		t.Fatalf("without staging envs: %+v", s)
	}
}
