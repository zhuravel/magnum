package factory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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
		` "baseRefName": "master", "headRefOid": "` + head + `", "isDraft": false, "headRepository": {"name": "shop"},` +
		` "headRepositoryOwner": {"login": "example"}}`
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

	// The login behind your gh config dir, as you only.
	run.answer = func(execx.Cmd) string { return "zhuravel\n" }
	if login, err := gh.ActiveLogin(ctx, "example/shop"); err != nil || login != "zhuravel" {
		t.Fatalf("ActiveLogin = %q, %v", login, err)
	}
	if c := run.cmds[len(run.cmds)-1]; !slices.Equal(c.Args, []string{"api", "user", "--jq", ".login", "--hostname", "github.com"}) ||
		c.Env["GH_HOST"] != "github.com" || !slices.Contains(c.Unset, "GH_TOKEN") {
		t.Fatalf("gh %q, env %v, unset %v", c.Args, c.Env, c.Unset)
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

// The GitHub client reads a repository's merge methods from its settings as
// your gh login; an answer without them (GitHub shows them only to a login
// that may push) or allowing none is an error, never "none allowed".
func TestTheGitHubClientReadsTheMergeMethods(t *testing.T) {
	answer := `{"allow_merge_commit":false,"allow_squash_merge":true,"allow_rebase_merge":false}`
	run := &ghRunner{answer: func(execx.Cmd) string { return answer }}
	cfg := config.Defaults()
	cfg.Identities = []config.Identity{{Name: "zhuravel", Kind: "gh", Login: "zhuravel"}}
	gh := NewGitHub(cfg, run)
	ctx := t.Context()

	m, err := gh.MergeMethods(ctx, "example/shop")
	if err != nil || m != (MergeMethods{Squash: true}) || m.String() != "squash" {
		t.Fatalf("MergeMethods = %+v (%q), %v; want squash only", m, m, err)
	}
	c := run.cmds[0]
	if c.Name != "gh" || len(c.Args) < 2 || c.Args[0] != "api" || c.Args[1] != "repos/example/shop" || c.Mutates ||
		c.Env["GH_HOST"] != "github.com" || !slices.Contains(c.Unset, "GH_TOKEN") {
		t.Fatalf("gh %q (mutates %v), env %v, unset %v; want a read of repos/example/shop as your login", c.Args, c.Mutates, c.Env, c.Unset)
	}

	answer = `{"allow_merge_commit":true,"allow_squash_merge":true,"allow_rebase_merge":true}`
	if m, err := gh.MergeMethods(ctx, "example/shop"); err != nil || m.String() != "merge commit, squash or rebase" {
		t.Fatalf("MergeMethods = %q, %v; want all three", m, err)
	}
	for _, bad := range []string{
		`{"allow_merge_commit":null,"allow_squash_merge":null,"allow_rebase_merge":null}`,
		`{"allow_merge_commit":false,"allow_squash_merge":false,"allow_rebase_merge":false}`,
		`not json`,
	} {
		answer = bad
		if m, err := gh.MergeMethods(ctx, "example/shop"); err == nil {
			t.Errorf("MergeMethods of %s = %+v, want an error", bad, m)
		}
	}
}

// The publish checks the card's review again before it pushes: a task at
// its publish stage whose review is no longer clean at the approved head
// needs you, and nothing leaves the Mac.
func TestThePublishChecksTheReviewAgainBeforeItPushes(t *testing.T) {
	r := newReadyRig(t)
	task, head := r.toCard("PR-12383")
	lift := r.pushHeld(task.ID)
	if got := r.reload(task.ID); got.Stage != "publish" {
		t.Fatalf("task at %s after your push, want publish", got.Stage)
	}
	r.summary(task.ID, registry.ExitUnavailable, head)
	lift()
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
	// A task of a Jira key pushes its own branch, under its own name.
	if got.State != registry.TaskPublished || got.PRNumber != 4501 || r.remoteHead(task.Branch) != head || got.RemoteBranch != task.Branch {
		t.Fatalf("task %s waits %q, PR %d, pushes to %q; remote %q, want published at %s on %s", got.State, got.WaitsWhat, got.PRNumber,
			got.RemoteBranch, r.remoteHead(task.Branch), head, task.Branch)
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

// A task of a prompt whose Jira key was linked after its branch was made
// pushes to a branch named for the key, <login>-<KEY>-<slug of its title>,
// recorded on the task: the card and its y/N sentence name it, the PR is
// opened and linked on it, a publish cut after the push resumes on it, and
// the local branch keeps its name.
func TestATaskLinkedToAKeyLaterPushesToABranchNamedForTheKey(t *testing.T) {
	r := newReadyRig(t)
	task, err := r.reg.CreateTask(r.ctx, registry.Task{JiraKey: "PR-12376", Prompt: "count the archived activities", Kind: "bug",
		Repo: "example/shop", Title: "count the archived activities", State: registry.TaskActive, Stage: "coding",
		Branch: "zhuravel-count-archived-activities"})
	if err != nil {
		t.Fatal(err)
	}
	r.taskFile(task.ID, ticketFile, "# PR-12376\n\n## Summary\n\nArchived activities lose their points\n")
	r.git(r.copyDir, "switch", "--quiet", "--no-track", "-c", task.Branch, "origin/master")
	got, head := r.cardOf(task)
	const remote = "zhuravel-PR-12376-archived-activities-lose-their-points"
	c := r.card(task.ID)
	if got.RemoteBranch != remote || c.Remote != remote || c.Branch != task.Branch {
		t.Fatalf("task pushes to %q, card %+v; want %s, the local branch %s kept", got.RemoteBranch, c, remote, task.Branch)
	}
	if s := ConfirmSentence(got, c.Copy, ""); !strings.HasPrefix(s, "Push "+remote+" and open the PR?") {
		t.Fatalf("the y/N sentence %q, want it to name %s", s, remote)
	}

	r.gh.createErr = fmt.Errorf("gh: HTTP 502")
	if q := r.decideOn(task.ID, c, ChoicePush, "", ""); q.State != registry.RequestDone || !strings.Contains(q.Result, "pushes "+remote+" at") {
		t.Fatalf("push request %s: %q, want it to name %s", q.State, q.Result, remote)
	}
	r.tick() // the push goes through, the PR does not
	if r.remoteHead(remote) != head || r.remoteHead(task.Branch) != "" {
		t.Fatalf("origin has %s at %q and %s at %q, want the key's branch at %s and no other", remote, r.remoteHead(remote), task.Branch,
			r.remoteHead(task.Branch), short(head))
	}
	r.restart()
	pushes := 0
	r.failGit = func(args []string) error {
		if slices.Contains(args, "push") {
			pushes++
		}
		return nil
	}
	if got := r.published(task.ID); got.State != registry.TaskPublished || pushes != 0 {
		t.Fatalf("task %s waits %q after %d pushes, want published with none", got.State, got.WaitsWhat, pushes)
	}
	cs := r.gh.createCalls()
	link, err := r.reg.LinkedPR(r.ctx, task.ID)
	if len(cs) != 2 || cs[0].branch != remote || cs[1].branch != remote || err != nil || link.Branch != remote {
		t.Fatalf("CreatePR %+v; link %+v (%v); want both on %s", cs, link, err, remote)
	}
	if b := r.git(r.copyDir, "rev-parse", "refs/heads/"+task.Branch); b != head || r.gitOut(r.copyDir, "rev-parse", "--verify", "--quiet",
		"refs/heads/"+remote) != "" {
		t.Fatalf("the local branch %s at %s, want it kept at %s and no local %s", task.Branch, b, short(head), remote)
	}
}

// A key linked to the task after its card was drawn changes nothing your
// push publishes: the branch and the title are the card's, as its y/N
// sentence named them.
func TestAKeyLinkedAfterTheCardChangesNothingThePushPublishes(t *testing.T) {
	r := newReadyRig(t)
	task, err := r.reg.CreateTask(r.ctx, registry.Task{Prompt: "count the archived activities", Kind: "bug", Repo: "example/shop",
		Title: "count the archived activities", State: registry.TaskActive, Stage: "coding", Branch: "zhuravel-count-archived-activities"})
	if err != nil {
		t.Fatal(err)
	}
	r.git(r.copyDir, "switch", "--quiet", "--no-track", "-c", task.Branch, "origin/master")
	got, head := r.cardOf(task)
	c := r.card(task.ID)
	if c.Remote != task.Branch || c.Title != "count the archived activities" || got.RemoteBranch != task.Branch {
		t.Fatalf("card %+v, task pushes to %q; want its own branch and title", c, got.RemoteBranch)
	}
	if err := r.reg.LinkKey(r.ctx, task.ID, "PR-12371"); err != nil {
		t.Fatal(err)
	}
	r.taskFile(task.ID, ticketFile, "# PR-12371\n\n## Summary\n\nArchived activities lose their points\n")
	if q := r.decideOn(task.ID, c, ChoicePush, "", ""); q.State != registry.RequestDone {
		t.Fatalf("push request %s: %q", q.State, q.Result)
	}
	if got := r.published(task.ID); got.State != registry.TaskPublished {
		t.Fatalf("task %s waits %q", got.State, got.WaitsWhat)
	}
	cs := r.gh.createCalls()
	if len(cs) != 1 || cs[0].branch != c.Remote || cs[0].title != c.Title || r.remoteHead(c.Remote) != head ||
		r.remoteHead("zhuravel-PR-12371-archived-activities-lose-their-points") != "" {
		t.Fatalf("CreatePR %+v; origin %s at %q; want the card's branch and title", cs, c.Remote, r.remoteHead(c.Remote))
	}
}

// A drop is never bound to the card: after the PR body went and the card
// could not be drawn again, your drop still frees the copy and keeps the
// branch. (A branch that moved no longer strands the card: it goes back
// through the coding stage's checks, TestAMovedHeadGoesThroughTheChecksToANewCard.)
func TestDropWorksAfterTheCardChanged(t *testing.T) {
	r := newReadyRig(t)
	task, moved := r.toCard("PR-12370")
	c := r.card(task.ID)
	if err := os.Remove(filepath.Join(r.layout.FactoryTaskDir(task.ID), prBodyFile)); err != nil {
		t.Fatal(err)
	}
	if q := r.decideOn(task.ID, c, ChoicePush, "", ""); q.State != registry.RequestFailed {
		t.Fatalf("push request %s: %q, want it refused", q.State, q.Result)
	}
	if q := r.retry(task.ID); q.State != registry.RequestDone { // the card is drawn again, and cannot be
		t.Fatalf("retry %s: %q", q.State, q.Result)
	}
	r.run(3, nil)
	if got := r.reload(task.ID); got.State != registry.TaskNeedsYou || strings.HasPrefix(got.WaitsWhat, readyWait) {
		t.Fatalf("task %s waits %q, want you, with no card", got.State, got.WaitsWhat)
	}
	if q := r.decideOn(task.ID, c, ChoiceDrop, "", ""); q.State != registry.RequestDone {
		t.Fatalf("drop request %s: %q, want it done", q.State, q.Result)
	}
	if got := r.reload(task.ID); got.State != registry.TaskStopped || !slices.Contains(r.copies.releases(), task.ID) ||
		r.git(r.copyDir, "rev-parse", "refs/heads/"+task.Branch) != moved {
		t.Fatalf("task %s; releases %v; want stopped, its copy freed and its branch kept", got.State, r.copies.releases())
	}
}

// The publish checks the login behind your gh config dir first: another
// account active there stops it, naming both logins, before anything is
// pushed or opened.
func TestADifferentActiveGhAccountStopsThePublishBeforeAnyPush(t *testing.T) {
	r := newReadyRig(t)
	task, _ := r.toCard("PR-12369")
	r.gh.active = "alice"
	if q := r.decideOn(task.ID, r.card(task.ID), ChoicePush, "", ""); q.State != registry.RequestDone {
		t.Fatalf("push request %s: %q", q.State, q.Result)
	}
	got := r.published(task.ID)
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "alice") || !strings.Contains(got.WaitsWhat, "zhuravel") ||
		r.remoteHead(task.Branch) != "" || len(r.gh.createCalls()) != 0 {
		t.Fatalf("task %s waits %q; remote %q; %d PRs", got.State, got.WaitsWhat, r.remoteHead(task.Branch), len(r.gh.createCalls()))
	}
}

// A publish that keeps failing for a cause of its own (gh logged out: `gh
// api user` exits 4 asking for `gh auth login`) is tried again a few times,
// then the task needs you, naming the cause, with an alert: it is never
// retried every tick in silence, each try costing GitHub calls. Nothing is
// pushed or opened meanwhile.
func TestAPublishWithGhLoggedOutNeedsYouAfterAFewTries(t *testing.T) {
	r := newReadyRig(t)
	task, _ := r.toCard("PR-12368")
	r.gh.mu.Lock()
	r.gh.activeErr = errors.New("gh api user: exit status 4: To get started with GitHub CLI, please run:  gh auth login")
	r.gh.mu.Unlock()
	if q := r.decideOn(task.ID, r.card(task.ID), ChoicePush, "", ""); q.State != registry.RequestDone {
		t.Fatalf("push request %s: %q", q.State, q.Result)
	}
	for range 12 {
		r.clock.Add(45 * time.Second)
		r.tick()
		if r.reload(task.ID).State == registry.TaskNeedsYou {
			break
		}
	}
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "gh auth login") || !strings.Contains(got.WaitsWhat, "5 times") {
		t.Fatalf("task %s at %s waits %q, want you, the publish's cause named", got.State, got.Stage, got.WaitsWhat)
	}
	r.gh.mu.Lock()
	calls := r.gh.activeCalls
	r.gh.mu.Unlock()
	if calls > stageFailLimit {
		t.Fatalf("gh api user ran %d times, want at most %d", calls, stageFailLimit)
	}
	if !slices.ContainsFunc(r.eventTexts(task.ID, "alert"), func(s string) bool { return strings.Contains(s, "gh auth login") }) {
		t.Fatalf("alerts %q, want one naming the cause", r.eventTexts(task.ID, "alert"))
	}
	if r.remoteHead(task.Branch) != "" || len(r.gh.createCalls()) != 0 {
		t.Fatal("the publish pushed or opened a PR while gh was logged out")
	}
	r.clock.Add(10 * time.Minute)
	r.tick()
	r.gh.mu.Lock()
	after := r.gh.activeCalls
	r.gh.mu.Unlock()
	if after != calls {
		t.Fatalf("gh api user ran %d more times while the task waited on you", after-calls)
	}
}

// The publish's own bound: a publish whose tries keep failing on the
// network (the infrastructure hold's cause, which the stage's count leaves
// to the hold) is tried at most stageFailLimit times, at the hold's
// backoff, then the task needs you naming the effect's last result.
func TestAPublishThatKeepsFailingOnTheNetworkStopsAfterAFewTries(t *testing.T) {
	r := newReadyRig(t)
	task, _ := r.toCard("PR-12367")
	r.gh.mu.Lock()
	r.gh.activeErr = errors.New("gh api user: error connecting to api.github.com: dial tcp: lookup api.github.com: no such host")
	r.gh.mu.Unlock()
	if q := r.decideOn(task.ID, r.card(task.ID), ChoicePush, "", ""); q.State != registry.RequestDone {
		t.Fatalf("push request %s: %q", q.State, q.Result)
	}
	for range 40 {
		if r.reload(task.ID).State == registry.TaskNeedsYou {
			break
		}
		r.clock.Add(2 * time.Minute)
		r.tick()
	}
	got := r.reload(task.ID)
	r.gh.mu.Lock()
	calls := r.gh.activeCalls
	r.gh.mu.Unlock()
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "no such host") || !strings.Contains(got.WaitsWhat, "the publish of") ||
		calls != stageFailLimit {
		t.Fatalf("task %s waits %q after %d tries of gh api user, want you after %d, the publish's result named", got.State, got.WaitsWhat,
			calls, stageFailLimit)
	}
}

// The publish's push, its read of the remote branch and every gh call run
// as you only: your gh config dir, and none of the GitHub tokens of the
// daemon's environment, which could push or open the PR as someone else.
func TestThePublishRunsAsYouOnly(t *testing.T) {
	storetest.Serial(t)
	ghDir := t.TempDir()
	t.Setenv("GH_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", ghDir)
	for _, k := range ghTokens {
		t.Setenv(k, "a-token-of-the-daemon")
	}
	want := filepath.Join(ghDir, "gh")
	asYou := func(what string, c execx.Cmd) {
		t.Helper()
		if c.Env["GH_CONFIG_DIR"] != want {
			t.Errorf("%s runs with GH_CONFIG_DIR %q, want your %s", what, c.Env["GH_CONFIG_DIR"], want)
		}
		for _, k := range ghTokens {
			if !slices.Contains(c.Unset, k) || c.Env[k] != "" {
				t.Errorf("%s keeps %s of the daemon's environment (unset %v)", what, k, c.Unset)
			}
		}
	}
	r := newReadyRig(t)
	var mu sync.Mutex
	var seen []execx.Cmd
	r.seenGit = func(c execx.Cmd) {
		if slices.Contains(c.Args, "push") || slices.Contains(c.Args, "ls-remote") {
			mu.Lock()
			seen = append(seen, c)
			mu.Unlock()
		}
	}
	task, _ := r.toCard("PR-12375")
	if q := r.decideOn(task.ID, r.card(task.ID), ChoicePush, "", ""); q.State != registry.RequestDone {
		t.Fatalf("push request %s: %q", q.State, q.Result)
	}
	if got := r.published(task.ID); got.State != registry.TaskPublished {
		t.Fatalf("task %s waits %q", got.State, got.WaitsWhat)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("git ran %d push or ls-remote commands, want the publish's", len(seen))
	}
	for _, c := range seen {
		asYou("git "+strings.Join(c.Args, " "), c)
	}

	run := &ghRunner{answer: func(execx.Cmd) string { return "https://github.com/example/shop/pull/4501\n" }}
	if _, err := NewGitHub(r.cfg, run).CreatePR(r.ctx, "example/shop", "master", task.Branch, "t", "/f"); err != nil {
		t.Fatal(err)
	}
	asYou("gh pr create", run.cmds[0])
}

// A PR of the branch is the task's repository's own: a fork's PR whose
// head branch has the same name is no PR of the task, and the task's own
// is found.
func TestPRByBranchIgnoresAForksPR(t *testing.T) {
	const head = "3c3d79e4fc0c04f4d49bd3467fc079f6bc2662ad"
	pr := func(n int, owner string) string {
		return fmt.Sprintf(`{"number": %d, "url": "https://github.com/%s/shop/pull/%d", "author": {"login": "zhuravel"}, "baseRefName": "master",`+
			` "headRefOid": "%s", "isDraft": false, "headRepository": {"name": "shop"}, "headRepositoryOwner": {"login": "%s"}}`, n, owner, n, head, owner)
	}
	run := &ghRunner{answer: func(c execx.Cmd) string {
		if slices.Contains(c.Args, "zhuravel-PR-12360-copy") {
			return "[" + pr(4590, "alice") + ", " + pr(4501, "example") + "]"
		}
		return "[" + pr(4591, "alice") + "]"
	}}
	cfg := config.Defaults()
	cfg.Identities = []config.Identity{{Name: "zhuravel", Kind: "gh", Login: "zhuravel"}}
	gh := NewGitHub(cfg, run)
	v, ok, err := gh.PRByBranch(t.Context(), "example/shop", "zhuravel-PR-12360-copy")
	if err != nil || !ok || v.Number != 4501 {
		t.Fatalf("PRByBranch = %+v, %v, %v; want #4501, the repository's own", v, ok, err)
	}
	if f := run.cmds[0].Args; !slices.Contains(f, "--json") || !strings.Contains(f[slices.Index(f, "--json")+1], "headRepository,headRepositoryOwner") {
		t.Fatalf("gh %q asks for no head repository", f)
	}
	if v, ok, err := gh.PRByBranch(t.Context(), "example/shop", "zhuravel-PR-12361-copy"); err != nil || ok {
		t.Fatalf("PRByBranch of a branch only a fork has = %+v, %v, %v; want none", v, ok, err)
	}
}

// A daemon stopped after the publish's effect ended ok, before the task's
// move, finishes the task from the effect row: it is published with its
// linked PR, and nothing is pushed, opened or checked again.
func TestAPublishCutAfterItsEffectEndedFinishesIt(t *testing.T) {
	r := newReadyRig(t)
	task, head := r.toCard("PR-12378")
	r.pushHeld(task.ID)()
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
	// The push's tick chains the publish: the push goes through, the PR does not.
	if q := r.decideOn(task.ID, r.card(task.ID), ChoicePush, "", ""); q.State != registry.RequestDone {
		t.Fatalf("push request %s: %q", q.State, q.Result)
	}
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
	lift := r.pushHeld(task.ID)
	r.cfg.Identities = nil
	lift()
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
	st := storetest.OpenFile(t, r.layout.DB())
	if qs, err := st.PendingRequests(r.ctx, 100); err != nil || len(qs) != 0 {
		t.Fatalf("magnum's requests %+v (%v), want none", qs, err)
	}
}
