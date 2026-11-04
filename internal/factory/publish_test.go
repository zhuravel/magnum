package factory

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// ghRunner is gh as the GitHub client meets it: it records each command
// and answers with answer.
type ghRunner struct {
	mu     sync.Mutex
	cmds   []execx.Cmd
	answer func(c execx.Cmd) string
}

func (g *ghRunner) Run(_ context.Context, c execx.Cmd) (execx.Result, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cmds = append(g.cmds, c)
	return execx.Result{Stdout: []byte(g.answer(c))}, nil
}

// The GitHub client opens the PR ready for review as your gh login: no
// --draft, the title as one argument, the drafted body by its file, gh as
// your keyring login (GH_HOST pinned, no token of the environment); it
// reads the PR back with its author, base, head and ready state, and the
// open PRs of a branch.
func TestTheGitHubClientOpensTheReadyPRAsYourGhLogin(t *testing.T) {
	const head = "3c3d79e4fc0c04f4d49bd3467fc079f6bc2662ad"
	view := `{"number": 4501, "url": "https://github.com/example/shop/pull/4501", "author": {"login": "zhuravel"},` +
		` "baseRefName": "master", "headRefOid": "` + head + `", "isDraft": false}`
	run := &ghRunner{answer: func(c execx.Cmd) string {
		switch {
		case slices.Contains(c.Args, "create"):
			return "https://github.com/example/shop/pull/4501\n"
		case slices.Contains(c.Args, "view"):
			return view
		case slices.Contains(c.Args, "list") && slices.Contains(c.Args, "zhuravel-PR-12360-copy"):
			return "[" + view + "]"
		}
		return "[]"
	}}
	cfg := config.Defaults()
	cfg.Identities = []config.Identity{{Name: "zhuravel", Kind: "gh", Login: "zhuravel"}}
	cfg.Watches = []config.Watch{{Owner: "example", Include: []string{"*"}, Identity: "zhuravel"}}
	gh := NewGitHub(cfg, run)
	ctx := t.Context()

	pr, err := gh.CreatePR(ctx, "example/shop", "master", "zhuravel-PR-12360-copy", "PR-12360 Custom activities lose points on copy",
		"/tasks/1/pr-body.md")
	if err != nil || pr.Number != 4501 || pr.URL != "https://github.com/example/shop/pull/4501" {
		t.Fatalf("CreatePR = %+v, %v", pr, err)
	}
	c := run.cmds[0]
	want := []string{"pr", "create", "--repo", "example/shop", "--base", "master", "--head", "zhuravel-PR-12360-copy",
		"--title", "PR-12360 Custom activities lose points on copy", "--body-file", "/tasks/1/pr-body.md"}
	if c.Name != "gh" || !slices.Equal(c.Args, want) || slices.Contains(c.Args, "--draft") || !c.Mutates {
		t.Fatalf("gh %q (mutates %v), want %q and never --draft", c.Args, c.Mutates, want)
	}
	if c.Env["GH_HOST"] != "github.com" || !slices.Contains(c.Unset, "GH_TOKEN") || !slices.Contains(c.Unset, "GITHUB_TOKEN") {
		t.Fatalf("gh env %v, unset %v: want your keyring login on github.com, no token of the environment", c.Env, c.Unset)
	}

	v, err := gh.ViewPR(ctx, "example/shop", 4501)
	if err != nil || v != (PRView{Number: 4501, URL: "https://github.com/example/shop/pull/4501", Author: "zhuravel", Base: "master", HeadSHA: head}) {
		t.Fatalf("ViewPR = %+v, %v", v, err)
	}
	if v, ok, err := gh.PRByBranch(ctx, "example/shop", "zhuravel-PR-12360-copy"); err != nil || !ok || v.Number != 4501 || v.Draft {
		t.Fatalf("PRByBranch = %+v, %v, %v", v, ok, err)
	}
	if _, ok, err := gh.PRByBranch(ctx, "example/shop", "zhuravel-PR-12361-copy"); err != nil || ok {
		t.Fatalf("PRByBranch of a branch without a PR = %v, %v", ok, err)
	}

	// Without a gh identity no gh runs, and the error names the key.
	cfg.Identities = nil
	n := len(run.cmds)
	if _, err := gh.CreatePR(ctx, "example/shop", "master", "b", "t", "/f"); err == nil || !strings.Contains(err.Error(), `[[identity]] kind = "gh"`) {
		t.Fatalf("CreatePR without a gh identity = %v, want the key named", err)
	}
	if len(run.cmds) != n {
		t.Fatal("gh ran without a gh identity")
	}
}

// The publish checks the card's review again before it pushes: a task at
// its publish stage whose review is no longer clean at the approved head
// needs you, and nothing leaves the Mac.
func TestThePublishChecksTheReviewAgainBeforeItPushes(t *testing.T) {
	r := newReadyRig(t)
	task, head := r.toCard("PR-12383")
	c := r.card(task.ID)
	if q := r.decideOn(task.ID, c, ChoicePush, "", ""); q.State != registry.RequestDone {
		t.Fatalf("push request %s: %q", q.State, q.Result)
	}
	if got := r.reload(task.ID); got.Stage != "publish" {
		t.Fatalf("task at %s after your push, want publish", got.Stage)
	}
	r.summary(task.ID, registry.ExitUnavailable, head)
	r.run(3, nil)
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "review unavailable") || r.remoteHead(task.Branch) != "" {
		t.Fatalf("task %s waits %q; remote %q", got.State, got.WaitsWhat, r.remoteHead(task.Branch))
	}
}

// One head is published once: your push records the decision with what the
// card showed, the publish pushes the approved head with a lease and opens
// one PR; a second push of the same head is refused and pushes nothing,
// and the publish run again does nothing either.
func TestPublishRunsOnceForOneHead(t *testing.T) {
	r := newReadyRig(t)
	task, head := r.toCard("PR-12387")
	c := r.card(task.ID)
	if q := r.decideOn(task.ID, c, ChoicePush, "", ""); q.State != registry.RequestDone {
		t.Fatalf("push request %s: %q", q.State, q.Result)
	}
	got := r.published(task.ID)
	if got.State != registry.TaskPublished || got.PRNumber != 4501 || r.remoteHead(task.Branch) != head {
		t.Fatalf("task %s waits %q, PR %d; remote %q, want published at %s", got.State, got.WaitsWhat, got.PRNumber, r.remoteHead(task.Branch), head)
	}
	ds, err := r.reg.Decisions(r.ctx, task.ID)
	if err != nil || len(ds) != 1 || ds[0].Choice != ChoicePush || ds[0].HeadSHA != head || ds[0].ReportHash != c.ReportHash ||
		ds[0].DiffHash != c.DiffHash || ds[0].Kind != config.FactoryDecisionReadyPush || ds[0].Proof == "" {
		t.Fatalf("decisions %+v (%v)", ds, err)
	}
	link, err := r.reg.LinkedPR(r.ctx, task.ID)
	if err != nil || link.Number != 4501 || link.ApprovedHead != head || link.OpenedHead != head || link.Branch != task.Branch {
		t.Fatalf("PR link %+v (%v)", link, err)
	}
	eff, err := r.reg.EffectByKey(r.ctx, fmt.Sprintf("publish:%d:%s", task.ID, head))
	if err != nil || eff.State != registry.EffectOK || eff.Resumed {
		t.Fatalf("effect %+v (%v)", eff, err)
	}

	pushes := 0
	r.failGit = func(args []string) error {
		if slices.Contains(args, "push") {
			pushes++
		}
		return nil
	}
	if q := r.decideOn(task.ID, c, ChoicePush, "", ""); q.State != registry.RequestFailed {
		t.Fatalf("a second push of %s: %s %q, want it refused", short(head), q.State, q.Result)
	}
	st, _ := r.e.stageOf(got, "publish")
	if done, err := r.e.runEffect(r.ctx, got, st); err != nil || !done {
		t.Fatalf("the publish run again: %v, %v", done, err)
	}
	if n := len(r.gh.createCalls()); n != 1 || pushes != 0 {
		t.Fatalf("%d PRs opened and %d pushes after the second push, want 1 and none", n, pushes)
	}
}

// A daemon stopped after the publish's effect ended ok, before the task's
// move, finishes the task from the effect row: it is published with its
// linked PR, and nothing is pushed, opened or checked again.
func TestAPublishCutAfterItsEffectEndedFinishesIt(t *testing.T) {
	r := newReadyRig(t)
	task, head := r.toCard("PR-12378")
	if q := r.decideOn(task.ID, r.card(task.ID), ChoicePush, "", ""); q.State != registry.RequestDone {
		t.Fatalf("push request %s: %q", q.State, q.Result)
	}
	key := publishKey(task.ID, head)
	if _, _, err := r.reg.BeginEffect(r.ctx, key, "publish", task.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.reg.LinkPR(r.ctx, registry.PRLink{TaskID: task.ID, Repo: "example/shop", Number: 4520, Branch: task.Branch, OpenedHead: head,
		ApprovedHead: head}); err != nil {
		t.Fatal(err)
	}
	if err := r.reg.EndEffect(r.ctx, key, registry.EffectOK, "https://github.com/example/shop/pull/4520"); err != nil {
		t.Fatal(err)
	}
	r.restart()
	r.cfg.Identities = nil // what the publish would check again: not read
	got := r.published(task.ID)
	if got.State != registry.TaskPublished || got.PRNumber != 4520 || r.remoteHead(task.Branch) != "" || len(r.gh.createCalls()) != 0 {
		t.Fatalf("task %s waits %q, PR %d; remote %q; %d PRs opened", got.State, got.WaitsWhat, got.PRNumber, r.remoteHead(task.Branch),
			len(r.gh.createCalls()))
	}
}

// A daemon stopped between the push and the PR resumes through the effect
// row: it reads the remote branch and the PRs of the branch first, so the
// branch at the approved head is not pushed again, and the PR is opened
// once.
func TestAPublishCutAfterThePushResumesWithoutPushingAgain(t *testing.T) {
	r := newReadyRig(t)
	task, head := r.toCard("PR-12388")
	r.gh.createErr = fmt.Errorf("gh: HTTP 502")
	if q := r.decideOn(task.ID, r.card(task.ID), ChoicePush, "", ""); q.State != registry.RequestDone {
		t.Fatalf("push request %s: %q", q.State, q.Result)
	}
	r.tick() // the publish: the push goes through, the PR does not
	if r.remoteHead(task.Branch) != head || len(r.gh.createCalls()) != 1 || r.reload(task.ID).State == registry.TaskPublished {
		t.Fatalf("remote %q, %d PRs: want the push done and the PR not", r.remoteHead(task.Branch), len(r.gh.createCalls()))
	}
	r.restart()
	pushes := 0
	r.failGit = func(args []string) error {
		if slices.Contains(args, "push") {
			pushes++
		}
		return nil
	}
	got := r.published(task.ID)
	eff, err := r.reg.EffectByKey(r.ctx, fmt.Sprintf("publish:%d:%s", task.ID, head))
	if got.State != registry.TaskPublished || err != nil || !eff.Resumed || eff.State != registry.EffectOK || pushes != 0 ||
		len(r.gh.createCalls()) != 2 {
		t.Fatalf("task %s waits %q; effect %+v (%v); %d pushes, %d creates", got.State, got.WaitsWhat, eff, err, pushes, len(r.gh.createCalls()))
	}
}

// The PR opens ready for review under your login: the title is the Jira
// key and the task's title, the body the drafted one, and the read-back
// says author zhuravel and not a draft.
func TestThePRIsReadyForReviewUnderTheOperatorsLogin(t *testing.T) {
	r := newReadyRig(t)
	task, head := r.toCard("PR-12389")
	if q := r.decideOn(task.ID, r.card(task.ID), ChoicePush, "", ""); q.State != registry.RequestDone {
		t.Fatalf("push request %s: %q", q.State, q.Result)
	}
	if got := r.published(task.ID); got.State != registry.TaskPublished {
		t.Fatalf("task %s waits %q", got.State, got.WaitsWhat)
	}
	cs := r.gh.createCalls()
	if len(cs) != 1 || cs[0].repo != "example/shop" || cs[0].base != "master" || cs[0].branch != task.Branch ||
		cs[0].title != "PR-12389 title" || cs[0].body != r.readTask(task.ID, prBodyFile) {
		t.Fatalf("CreatePR %+v", cs)
	}
	v, err := r.gh.ViewPR(r.ctx, "example/shop", 4501)
	if err != nil || v.Draft || v.Author != "zhuravel" || v.HeadSHA != head {
		t.Fatalf("read back %+v (%v)", v, err)
	}
}

// Review Focus 3: the PR read back is not what you approved (another head,
// base or author, or a draft): the task needs you naming the field, and is
// not published.
func TestPublishStopsWhenThePRIsNotWhatWasApproved(t *testing.T) {
	for _, tc := range []struct {
		field string
		edit  func(*PRView)
	}{
		{"head", func(v *PRView) { v.HeadSHA = strings.Repeat("b", 40) }},
		{"base", func(v *PRView) { v.Base = "staging" }},
		{"author", func(v *PRView) { v.Author = "alice" }},
		{"draft", func(v *PRView) { v.Draft = true }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			r := newReadyRig(t)
			task, _ := r.toCard("PR-12390")
			r.gh.edit = tc.edit
			if q := r.decideOn(task.ID, r.card(task.ID), ChoicePush, "", ""); q.State != registry.RequestDone {
				t.Fatalf("push request %s: %q", q.State, q.Result)
			}
			got := r.published(task.ID)
			if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "the PR's "+tc.field+" is not what you approved") {
				t.Fatalf("task %s waits %q, want you naming the PR's %s", got.State, got.WaitsWhat, tc.field)
			}
			if _, err := r.reg.LinkedPR(r.ctx, task.ID); err == nil {
				t.Fatal("a PR that is not what you approved was linked")
			}
		})
	}
}

// A resumed publish never forces: a remote branch at a head the factory did
// not push, or a PR of the branch that is not yours, needs you naming it,
// and neither is changed.
func TestAResumedPublishNeverForces(t *testing.T) {
	t.Run("remote branch", func(t *testing.T) {
		r := newReadyRig(t)
		task, _ := r.toCard("PR-12391")
		other := r.git(r.work, "rev-parse", "HEAD")
		r.git(r.work, "push", "--quiet", "origin", other+":refs/heads/"+task.Branch)
		if q := r.decideOn(task.ID, r.card(task.ID), ChoicePush, "", ""); q.State != registry.RequestDone {
			t.Fatalf("push request %s: %q", q.State, q.Result)
		}
		got := r.published(task.ID)
		if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, task.Branch) || !strings.Contains(got.WaitsWhat, short(other)) ||
			r.remoteHead(task.Branch) != other || len(r.gh.createCalls()) != 0 {
			t.Fatalf("task %s waits %q; remote %q", got.State, got.WaitsWhat, r.remoteHead(task.Branch))
		}
	})
	t.Run("PR of the branch", func(t *testing.T) {
		r := newReadyRig(t)
		task, head := r.toCard("PR-12392")
		r.gh.addPR(task.Branch, PRView{Number: 4590, URL: "https://github.com/example/shop/pull/4590", Author: "alice", Base: "master", HeadSHA: head})
		if q := r.decideOn(task.ID, r.card(task.ID), ChoicePush, "", ""); q.State != registry.RequestDone {
			t.Fatalf("push request %s: %q", q.State, q.Result)
		}
		got := r.published(task.ID)
		if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "#4590") || !strings.Contains(got.WaitsWhat, "alice") ||
			len(r.gh.createCalls()) != 0 || r.remoteHead(task.Branch) != "" {
			t.Fatalf("task %s waits %q; %d PRs opened", got.State, got.WaitsWhat, len(r.gh.createCalls()))
		}
	})
}

// The PR opens as the gh identity of the local review: a config without
// one needs you naming the key, and nothing is pushed.
func TestAPublishWithoutAGhIdentityNeedsYouNamingTheKey(t *testing.T) {
	r := newReadyRig(t)
	task, _ := r.toCard("PR-12393")
	if q := r.decideOn(task.ID, r.card(task.ID), ChoicePush, "", ""); q.State != registry.RequestDone {
		t.Fatalf("push request %s: %q", q.State, q.Result)
	}
	r.cfg.Identities = nil
	got := r.published(task.ID)
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, `[[identity]] kind = "gh"`) || r.remoteHead(task.Branch) != "" {
		t.Fatalf("task %s waits %q; remote %q", got.State, got.WaitsWhat, r.remoteHead(task.Branch))
	}
}

// A published task frees its copy (its agents parked first, its copy
// released and no longer on the task), and the task queued behind it
// starts.
func TestAPublishedTaskFreesItsCopyAndStartsTheQueuedOne(t *testing.T) {
	r := newReadyRig(t)
	r.cfg.Factory.MaxActive = 1
	task, _ := r.toCard("PR-12394")
	queued, err := r.reg.CreateTask(r.ctx, registry.Task{JiraKey: "PR-12395", Kind: "bug", Repo: "example/shop", Title: "next",
		State: registry.TaskQueued, WaitsWho: waitCopy, WaitsWhat: "all 1 busy"})
	if err != nil {
		t.Fatal(err)
	}
	r.tick()
	if got := r.reload(queued.ID); got.State != registry.TaskQueued {
		t.Fatalf("the queued task is %s while the card's task holds its copy", got.State)
	}
	if q := r.decideOn(task.ID, r.card(task.ID), ChoicePush, "", ""); q.State != registry.RequestDone {
		t.Fatalf("push request %s: %q", q.State, q.Result)
	}
	got := r.published(task.ID)
	if got.State != registry.TaskPublished || got.Copy != "" || !slices.Contains(r.copies.releases(), task.ID) ||
		!slices.Contains(r.rt.parked(), task.ID) {
		t.Fatalf("task %s holds %q; releases %v, parked %v", got.State, got.Copy, r.copies.releases(), r.rt.parked())
	}
	if q := r.reload(queued.ID); q.State != registry.TaskActive {
		t.Fatalf("the queued task is %s after the publish, want it started", q.State)
	}
}

// The factory sends magnum no review request: magnum's request table stays
// untouched, the PR is no draft and magnum reviews it by itself.
func TestTheFactorySendsMagnumNoReviewRequest(t *testing.T) {
	r := newReadyRig(t)
	storetest.Seed(t, r.layout.DB())
	task, _ := r.toCard("PR-12397")
	if q := r.decideOn(task.ID, r.card(task.ID), ChoicePush, "", ""); q.State != registry.RequestDone {
		t.Fatalf("push request %s: %q", q.State, q.Result)
	}
	if got := r.published(task.ID); got.State != registry.TaskPublished {
		t.Fatalf("task %s waits %q", got.State, got.WaitsWhat)
	}
	st := storetest.Open(t, r.layout.DB())
	if qs, err := st.PendingRequests(r.ctx, 100); err != nil || len(qs) != 0 {
		t.Fatalf("magnum's requests %+v (%v), want none", qs, err)
	}
}
