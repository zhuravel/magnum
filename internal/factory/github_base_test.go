package factory

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// rewriteOrigin points the main clone's origin at a bare repository of the
// writer's own (a url.<own>.insteadOf of the origin's URL), whose master is
// mine, and fetches it into the copy: the copy's origin/master is mine
// then, while master on GitHub (the bare origin) stays where it was.
func (r *codingRig) rewriteOrigin(mine string) {
	r.t.Helper()
	own := filepath.Join(r.root, "own.git")
	r.git(r.root, "clone", "--quiet", "--bare", r.origin, own)
	r.git(r.copyDir, "push", "--quiet", "--force", own, mine+":refs/heads/master")
	r.git(r.main, "config", "url."+own+".insteadOf", r.origin)
	r.git(r.copyDir, "fetch", "--quiet", "origin")
	if got := r.git(r.copyDir, "rev-parse", "origin/master"); got != mine {
		r.t.Fatalf("origin/master is at %s, want the writer's %s: the rig's rewritten remote is not what the copy reads", short(got), short(mine))
	}
}

// The local round's diff starts where the change starts on GitHub: at the
// merge base of its head with the commit the base is at on GitHub, read as
// you, never with the copy's origin/<base>. The writer's pane points the
// main clone's origin at a bare repository of its own (a
// url.<x>.insteadOf), whose master is the writer's own commit on master,
// fetches it, and builds the fix on it: a diff from origin/master would
// start at that commit and leave its change out of the round. The round's
// diff starts at master on GitHub, so the writer's commit is in it.
func TestARoundsDiffBaseIsGitHubsBaseNotTheRewrittenRemote(t *testing.T) {
	r := newReviewRig(t, result())
	onGitHub := r.git(r.origin, "rev-parse", "refs/heads/master")
	task := r.codingTask("PR-12420", "bug")
	mine := r.commit("the writer's own change", map[string]string{"lib/own.rb": "class Own\nend\n"})
	r.rewriteOrigin(mine)
	head := r.fix(task)
	if err := r.reg.TransitionTask(r.ctx, task.ID, nil, registry.TaskActive, func(u *registry.TaskUpdate) {
		u.Set("stage", "local_review")
		u.Set("head_sha", head)
	}); err != nil {
		t.Fatal(err)
	}
	r.run(4, func() bool { return len(r.rv.calls()) > 0 })

	calls := r.rv.calls()
	if len(calls) != 1 || calls[0].HeadSHA != head || calls[0].BaseSHA != onGitHub {
		got := ""
		if len(calls) > 0 {
			got = short(calls[0].BaseSHA)
		}
		t.Fatalf("%d rounds, the first from %q; want one at %s from master on GitHub (%s), not from the writer's %s", len(calls), got,
			short(head), short(onGitHub), short(mine))
	}
	if got := r.git(r.copyDir, "rev-parse", "origin/master"); got != mine {
		t.Fatalf("origin/master is at %s after the round's fetch, want the writer's %s: the round read a remote the rig did not rewrite",
			short(got), short(mine))
	}
}

// A round whose base GitHub does not report does not start on a guessed
// one: the task waits saying why, the read is tried again a poll later
// (one read a try, never one a tick), and after stageFailLimit tries the
// task needs you with the reason and an alert. A base GitHub reports at a
// commit the copy has no object of, even after the fetch, is no base
// either: the read names it.
func TestARoundWaitsWhenGitHubsBaseCannotBeRead(t *testing.T) {
	r := newReviewRig(t, result())
	r.gh.setBase("", errors.New("gh: Not Found (HTTP 404)"), nil)
	task, head := r.reviewTask("PR-12421")
	r.run(3, nil)
	got := r.reload(task.ID)
	if len(r.rv.calls()) != 0 || len(r.cycles(task.ID)) != 0 || got.State != registry.TaskActive ||
		!strings.Contains(got.WaitsWhat, "master on GitHub") || !strings.Contains(got.WaitsWhat, "HTTP 404") {
		t.Fatalf("%d rounds, %d cycles; task %s waits %q: %q; want no round, the wait naming GitHub's base", len(r.rv.calls()),
			len(r.cycles(task.ID)), got.State, got.WaitsWho, got.WaitsWhat)
	}
	for range 12 {
		if r.reload(task.ID).State == registry.TaskNeedsYou {
			break
		}
		r.clock.Add(45 * time.Second)
		r.tick()
		r.e.waitBackground()
	}
	got = r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "5 times in a row") || !strings.Contains(got.WaitsWhat, "master on GitHub") ||
		!strings.Contains(got.WaitsWhat, "HTTP 404") || len(r.rv.calls()) != 0 {
		t.Fatalf("task %s waits %q after %d rounds, want you, the reason named, and no round", got.State, got.WaitsWhat, len(r.rv.calls()))
	}
	if n := r.gh.baseReads(); n != stageFailLimit {
		t.Fatalf("GitHub's base was read %d times, want once a try, %d", n, stageFailLimit)
	}
	if !slices.ContainsFunc(r.eventTexts(task.ID, "alert"), func(s string) bool { return strings.Contains(s, "HTTP 404") }) {
		t.Fatalf("alerts %q, want one naming the reason", r.eventTexts(task.ID, "alert"))
	}

	repo, err := r.e.factoryRepo(task)
	if err != nil {
		t.Fatal(err)
	}
	missing := strings.Repeat("ab", 20)
	r.gh.setBase(missing, nil, nil)
	if _, err := r.e.baseOnGitHub(r.ctx, repo, r.cp, head, nil); err == nil || !strings.Contains(err.Error(), short(missing)) {
		t.Fatalf("a base at a commit the copy has no object of: %v, want an error naming %s", err, short(missing))
	}
	r.gh.setBase("", nil, nil)
	if b, err := r.e.baseOnGitHub(r.ctx, repo, r.cp, head, nil); err != nil || b.base != r.git(r.origin, "rev-parse", "refs/heads/master") {
		t.Fatalf("GitHub's base read: %+v, %v; want the merge base with master on GitHub", b, err)
	}
}

// A GitHub server error (or a rate limit) at the read of a round's base is
// GitHub's, not the task's: the round waits for GitHub, saying so, and the
// read is tried again each poll for as long as the infrastructure hold
// waits before it needs you (holdNeedsYou), never stageFailLimit's few
// tries. GitHub answers 502 for 10 polls, past both of stageFailLimit's
// bounds: the task never needs you, and once the read works the round
// starts from master on GitHub. A 502 that lasts holdNeedsYou sends the
// task to you, naming GitHub, with an alert.
func TestAGitHubServerErrorAtTheRoundsBaseDoesNotSendTheTaskToYouAtOnce(t *testing.T) {
	const badGateway = "gh: HTTP 502: Bad Gateway"
	r := newReviewRig(t, result())
	onGitHub := r.git(r.origin, "rev-parse", "refs/heads/master")
	r.gh.setBase("", errors.New(badGateway), nil)
	task, head := r.reviewTask("PR-12426")
	r.run(3, nil)
	for i := range 10 {
		if got := r.reload(task.ID); got.State != registry.TaskActive {
			t.Fatalf("after %d polls of HTTP 502 the task is %s, waiting %q; want it active, waiting for GitHub", i, got.State, got.WaitsWhat)
		}
		r.clock.Add(45 * time.Second)
		r.tick()
		r.e.waitBackground()
	}
	got := r.reload(task.ID)
	if got.State != registry.TaskActive || len(r.rv.calls()) != 0 || !strings.Contains(got.WaitsWhat, "master on GitHub") ||
		!strings.Contains(got.WaitsWhat, "HTTP 502") {
		t.Fatalf("task %s waits %q: %q after %d rounds; want it active, no round, the wait naming GitHub's base", got.State, got.WaitsWho,
			got.WaitsWhat, len(r.rv.calls()))
	}
	if n := r.gh.baseReads(); n < 10 {
		t.Fatalf("GitHub's base was read %d times in 10 polls, want once a poll", n)
	}
	r.gh.setBase("", nil, nil)
	r.clock.Add(45 * time.Second)
	r.run(4, func() bool { return len(r.rv.calls()) > 0 })
	if calls := r.rv.calls(); len(calls) != 1 || calls[0].HeadSHA != head || calls[0].BaseSHA != onGitHub {
		t.Fatalf("%d rounds after GitHub answers again, want one at %s from master on GitHub (%s)", len(calls), short(head), short(onGitHub))
	}

	r = newReviewRig(t, result())
	r.gh.setBase("", errors.New(badGateway), nil)
	task, _ = r.reviewTask("PR-12427")
	r.run(3, nil)
	for range 60 {
		if r.reload(task.ID).State == registry.TaskNeedsYou {
			break
		}
		r.clock.Add(45 * time.Second)
		r.tick()
		r.e.waitBackground()
	}
	got = r.reload(task.ID)
	fails := slices.DeleteFunc(r.events(task.ID, "advance"), func(e registry.Event) bool { return e.Phase != registry.PhaseFail })
	if got.State != registry.TaskNeedsYou || len(fails) == 0 || r.clock.Now().Sub(fails[0].At) < holdNeedsYou ||
		!strings.HasPrefix(got.WaitsWhat, "the local review stage has waited 30 minutes for GitHub (GitHub server error): ") ||
		!strings.Contains(got.WaitsWhat, "master on GitHub") || !strings.Contains(got.WaitsWhat, "HTTP 502") {
		t.Fatalf("task %s waits %q, %d fail events; want you once GitHub failed for %s, the cause named", got.State, got.WaitsWhat, len(fails),
			holdNeedsYou)
	}
	if !slices.ContainsFunc(r.eventTexts(task.ID, "alert"), func(s string) bool { return strings.Contains(s, "HTTP 502") }) {
		t.Fatalf("alerts %q, want one naming GitHub's answer", r.eventTexts(task.ID, "alert"))
	}
}

// A round that waits for the heavy slot reads no base on GitHub: the read
// comes once the round may start, after its waits and the heavy slot, so a
// pass that waits costs GitHub no call. Another task's round holds the
// slot for 3 polls, and the task waits on it without a read; once the slot
// is free, one read, and the round starts.
func TestARoundThatWaitsForTheHeavySlotReadsNoBaseOnGitHub(t *testing.T) {
	r := newReviewRig(t, result())
	r.cfg.Factory.HeavyStages = 1
	other := r.kindTask("PR-12428", "bug", "local_review")
	review := r.e.runners[config.FactoryStageReview]
	r.stage(config.FactoryStageReview, func(ctx context.Context, t registry.Task, st config.FactoryStage) (bool, error) {
		if t.ID == other.ID {
			return false, nil // its round runs: it holds the heavy slot
		}
		return review(ctx, t, st)
	})
	release, ok := r.e.heavy(r.ctx, other, heavyReview)
	if !ok {
		t.Fatal("the other task could not take the heavy slot")
	}
	task, head := r.reviewTask("PR-12429")
	for range 3 {
		r.clock.Add(45 * time.Second)
		r.tick()
		r.e.waitBackground()
	}
	if got := r.reload(task.ID); got.WaitsWho != "heavy" || got.WaitsWhat != "PR-12428's local review runs" || len(r.rv.calls()) != 0 ||
		r.gh.baseReads() != 0 {
		t.Fatalf("task waits %q: %q after %d rounds and %d reads of GitHub's base; want it waiting for the heavy slot with none",
			got.WaitsWho, got.WaitsWhat, len(r.rv.calls()), r.gh.baseReads())
	}
	release()
	r.clock.Add(45 * time.Second)
	r.run(4, func() bool { return len(r.rv.calls()) > 0 })
	if calls := r.rv.calls(); len(calls) != 1 || calls[0].HeadSHA != head || r.gh.baseReads() != 1 {
		t.Fatalf("%d rounds and %d reads of GitHub's base once the slot is free, want one round at %s after one read", len(calls),
			r.gh.baseReads(), short(head))
	}
}

// The second opinion weighs the change the round reviewed: from where it
// starts on GitHub, never from the copy's origin/<base>. The writer's pane
// points the main clone's origin at a bare repository of its own whose
// master is the writer's commit, which adds a migration, and the fix on
// top of it changes app/credit.rb alone: from origin/master the change is
// one file, narrow; from master on GitHub it holds the migration, wide, so
// the clean round of the Codex judge gets its second opinion, which names
// the migration. A read of GitHub's base that fails at the round's end
// decides nothing (never "narrow"): it is tried again a poll later.
func TestTheSecondOpinionWeighsTheChangeFromGitHubsBase(t *testing.T) {
	r := newReviewRig(t, rounds(2, result())...)
	task := r.codingTask("PR-12425", "bug")
	mine := r.commit("the writer's own migration", map[string]string{"db/migrate/20261114_own.rb": "class Own < ActiveRecord::Migration[7.1]\nend\n"})
	r.rewriteOrigin(mine)
	head := r.commit(task.JiraKey+" Credit#points counts the activities", map[string]string{
		"app/credit.rb": strings.Replace(creditRB, "    0\n", "    10\n", 1),
	})
	r.taskFile(task.ID, proofFile, `{"specs": ["spec/models/credit_spec.rb"]}`)
	if err := r.reg.TransitionTask(r.ctx, task.ID, nil, registry.TaskActive, func(u *registry.TaskUpdate) {
		u.Set("stage", "local_review")
		u.Set("head_sha", head)
	}); err != nil {
		t.Fatal(err)
	}
	r.run(6, func() bool { return len(r.rv.calls()) > 0 })
	if calls := r.rv.calls(); len(calls) != 1 || !slices.Equal(calls[0].Roles, firstRoles) {
		t.Fatalf("rounds %v, want round 1 with the Codex judge", roundsOf(calls))
	}

	dir := r.layout.FactoryTaskDir(task.ID)
	r.gh.setBase("", errors.New("gh: Not Found (HTTP 404)"), nil)
	for range 2 {
		r.clock.Add(45 * time.Second)
		r.tick()
		r.e.waitBackground()
	}
	if got := r.reload(task.ID); got.Stage != "local_review" || r.firstDecided(task.ID) || fileIn(dir, secondOpinionFile) ||
		!strings.Contains(got.WaitsWhat, "the second opinion after round 1 cannot weigh the change") || !strings.Contains(got.WaitsWhat, "master on GitHub") ||
		!slices.ContainsFunc(r.eventTexts(task.ID, "advance"), func(s string) bool { return strings.Contains(s, "master on GitHub") }) {
		t.Fatalf("task at %s waits %q, cycles %+v, advance events %q; want round 1 undecided while GitHub's base cannot be read, the failure named",
			got.Stage, got.WaitsWhat, r.cycles(task.ID), r.eventTexts(task.ID, "advance"))
	}

	r.gh.setBase("", nil, nil)
	r.clock.Add(45 * time.Second)
	r.run(6, func() bool { return fileIn(dir, secondOpinionFile) })
	so, ok := readSecondOpinion(dir)
	if !ok || !strings.Contains(so.Because, "db/migrate/20261114_own.rb") || so.Judge != "claude-judge" {
		t.Fatalf("second opinion %+v (%v), want one by claude-judge because of db/migrate/20261114_own.rb; events %q", so, ok,
			r.eventTexts(task.ID, "review"))
	}
}

// The ready card's flags read the change from where it starts on GitHub,
// and its conflicts with the base GitHub reports, never with the copy's
// origin/<base>: the writer's pane points the main clone's origin at a
// bare repository of its own whose master is the writer's commit, and its
// fix builds on that commit, while master on GitHub moved in the line the
// fix changes. The changed files then hold the writer's commit's file,
// which the plan did not name, and the conflict is the one with master on
// GitHub.
func TestTheCardsFlagsUseGitHubsBase(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12422", "bug")
	moved := r.moveMaster("app/credit.rb", strings.Replace(creditRB, "    0\n", "    5\n", 1))
	r.git(r.main, "fetch", "--quiet", "origin") // the copy has master on GitHub's commit, fetched before the rewrite
	mine := r.commit("the writer's own change", map[string]string{"lib/own.rb": "class Own\nend\n"})
	r.rewriteOrigin(mine)
	r.fix(task)
	r.taskFile(task.ID, reportFile, planReport("app/credit.rb", "spec/models/"))

	f, err := r.e.flags(r.ctx, task, r.cp)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.Conflicts, []string{"app/credit.rb"}) || !slices.Contains(f.NotInPlan, "lib/own.rb") {
		t.Fatalf("conflicts %q, not in the plan %q; want the conflict with master on GitHub (%s) and the writer's lib/own.rb", f.Conflicts,
			f.NotInPlan, short(moved))
	}
	if f.baseSHA != moved {
		t.Fatalf("the flags' base is %s, want master on GitHub at %s, the base a merge turn merges", short(f.baseSHA), short(moved))
	}
}

// The ready card shows the change from where it starts on GitHub: its
// base (where the diff you page starts), its files and its diff's hash are
// those of the head's merge base with master on GitHub, never with the
// copy's origin/<base>, which the writer's pane pointed at a commit of its
// own that the fix builds on. The writer's file is on the card, not in the
// plan.
func TestTheReadyCardShowsTheChangeFromGitHubsBase(t *testing.T) {
	r := newReadyRig(t)
	onGitHub := r.git(r.origin, "rev-parse", "refs/heads/master")
	task := r.codingTask("PR-12424", "bug")
	mine := r.commit("the writer's own change", map[string]string{"lib/own.rb": "class Own\nend\n"})
	r.rewriteOrigin(mine)
	_, head := r.cardOf(task)

	c := r.card(task.ID)
	if c.Head != head || c.Base != onGitHub || !slices.ContainsFunc(c.Files, func(f CardFile) bool { return f.Path == "lib/own.rb" && !f.InPlan }) {
		t.Fatalf("card at %s from %s with files %+v; want it at %s from master on GitHub (%s), not the writer's %s, lib/own.rb on it",
			short(c.Head), short(c.Base), c.Files, short(head), short(onGitHub), short(mine))
	}
	if dh, err := r.e.d.Git.OwnDiffID(r.ctx, r.copyDir, onGitHub, head); err != nil || c.DiffHash != dh {
		t.Fatalf("the card's diff hash %s (%v), want the one from master on GitHub, %s", c.DiffHash, err, dh)
	}
}

// The flags need the base GitHub reports in the copy: a commit the copy
// has no object of even after the fetch (the main clone's remote reaches
// another repository than GitHub's), or a read of GitHub that fails, is an
// error that names it, never a card that says "no conflicts".
func TestTheCardsFlagsNeedGitHubsBaseInTheCopy(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12423", "bug")
	r.fix(task)
	r.taskFile(task.ID, reportFile, planReport("app/credit.rb", "spec/models/"))
	missing := strings.Repeat("ab", 20)
	r.gh.setBase(missing, nil, nil)
	if f, err := r.e.flags(r.ctx, task, r.cp); err == nil || !strings.Contains(err.Error(), short(missing)) {
		t.Fatalf("flags %+v, %v; want an error naming the missing %s", f, err, short(missing))
	}
	r.gh.setBase("", errors.New("gh: Not Found (HTTP 404)"), nil)
	if f, err := r.e.flags(r.ctx, task, r.cp); err == nil || !strings.Contains(err.Error(), "master on GitHub") || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("flags %+v, %v; want an error naming the failed read", f, err)
	}
	r.gh.setBase("", nil, nil)
	if f, err := r.e.flags(r.ctx, task, r.cp); err != nil || len(f.Conflicts) != 0 {
		t.Fatalf("flags %+v, %v; want them read against master on GitHub", f, err)
	}
}
