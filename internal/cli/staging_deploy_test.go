package cli

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

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

// --dry-run prints the daemon's plan per env and queues nothing more;
// --json prints the answer. --yes (or a script) asks nothing and sends the
// deploy alone, bound to the registry's head, with nothing seen.
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
