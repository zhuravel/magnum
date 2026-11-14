package factory

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
