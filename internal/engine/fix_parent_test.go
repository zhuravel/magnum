package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// fixOneFixed is a fixer's "## Replied" line for one fixed finding.
const fixOneFixed = "1 threads (1 fixed, 0 already fixed, 0 not a bug, 0 won't fix)"

// The parent of a PR is its base branch, never the repository's default
// branch: a stacked PR's parent is the branch below it, and merging master
// into it would put master's commits into its own diff. A PR whose base
// neither GitHub nor the registry names gets no merge step at all, though
// the default branch's tip conflicts with it: the guard fetches nothing and
// checks no merge, the prompt names no merge and no fix-sync line, and the
// answer says that no parent is merged and why.
func TestAFixOnAPRWithNoKnownBaseMergesNoParent(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead, author: "zhuravel", noBase: true})
	if _, err := h.st.DB().ExecContext(h.ctx, `UPDATE prs SET base_ref = NULL WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	git.conflict("app/models/offer.rb")
	fixer(h, git, fixPushed, func(run store.Run) string {
		return fixReport(run.ID, "b2b2b2b Fix the total (review comment 4100000001)", fixOneFixed, "none")
	})
	id := h.fix(FixPayload{})
	h.handle()
	const note = "no parent merge: the PR's base branch is not known yet"
	if res := h.wantDone(id); !strings.Contains(res, note) {
		t.Errorf("answer = %q, want it to say %q", res, note)
	}
	if fetches, merges := git.calls(); len(fetches)+len(merges) != 0 {
		t.Errorf("fetches %v, merge checks %v: the job looked at a base the PR does not name", fetches, merges)
	}
	in := h.fixInput()
	if in.ConflictSHA != "" || in.ConflictBase != "" || in.FixSyncCommand != "" {
		t.Errorf("the prompt merges %q at %q, fix-sync line %q; want no merge and no fix-sync line", in.ConflictBase, in.ConflictSHA, in.FixSyncCommand)
	}
}

// A PR retargeted on GitHub after the poll's last read (its base merged, or
// its author moved it onto another branch) is merged with the base GitHub
// names when the job starts, not the one the registry still holds: the
// guard reads the PR again, fetches that base and checks the merge with its
// tip, and the prompt's merge and fix-sync line name it.
func TestAFixMergesTheBaseGitHubNamesWhenItsJobStarts(t *testing.T) {
	h, git := fixHarness(t)
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead, author: "zhuravel", base: "release"})
	git.conflict("app/models/offer.rb")
	fixer(h, git, fixPushed, func(run store.Run) string {
		return fixReport(run.ID, "d3d3d3d Merge c0c0c0c: not run: environment\nb2b2b2b Fix the total (review comment 4100000001)", fixOneFixed, "none")
	})
	if base := deref(h.pr(2).BaseRef); base != "master" {
		t.Fatalf("the registry's base = %q before the job, want master", base)
	}
	id := h.fix(FixPayload{})
	h.handle()
	h.wantDone(id)
	slot := h.slot("review1")
	if fetches, _ := git.calls(); len(fetches) != 1 || fetches[0] != slot.MainClone+":release" {
		t.Errorf("fetches = %v, want the guard's of release alone", fetches)
	}
	in := h.fixInput()
	if in.ConflictBase != "release" || in.ConflictSHA != fixBaseTip {
		t.Errorf("the prompt merges %q at %q, want release at %s", in.ConflictBase, in.ConflictSHA, fixBaseTip)
	}
	if want := agents.FixSyncLine(h.d.Layout.Binary(), slot.Path, fixHead, "feature", "release"); in.FixSyncCommand != want {
		t.Errorf("fix-sync line = %q, want %q", in.FixSyncCommand, want)
	}
}

const (
	// fixParentTree is the tree of a clean merge of the base's tip into
	// the head, and fixParentMerge the merge commit the daemon makes of it.
	fixParentTree  = "f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1"
	fixParentMerge = "e7e7e7e7e7e7e7e7e7e7e7e7e7e7e7e7e7e7e7e7"
	// fixerAppEmail is the fixer App's bot's noreply email.
	fixerAppEmail = "4242+" + fixerAppLogin + "@users.noreply.github.com"
)

// mergeGit is fixGit that can merge a PR's parent (parentGit): every
// commit is its own ancestor, the head lacks the base's tip when behind is
// set (IsAncestor of fixBaseTip in any other commit), MergeTree answers
// fixGit's conflicts (and no tree), else fixParentTree, CommitTree makes
// fixParentMerge and records its call ("<dir>:<tree>:<parents>:<name>
// <email>:<message>"), ChangedPaths answers workflows for
// .github/workflows and schema for any other pathspec, DifferingPaths of
// .github/workflows answers differ by the commit compared with, else own,
// FirstParentIdentities answers handed when set, and PushLeaseGitHubAs
// records its call ("<owner>/<repo>:<sha>:<branch>:<expect>:<pushedAs>")
// and moves the PR's head on the fake GitHub, unless
// pushErr fails it. With tips set, origin/<base> is tips[i] after the
// (i+1)th fetch (the last one after more), fixBaseTip before any (the
// base moves). Its harness's GitHub has fixBaseTip as every base's tip
// (mergeHarness).
type mergeGit struct {
	*fixGit
	gh        *fakeGH
	behind    bool
	tips      []string
	pushErr   error
	workflows []string
	own       []string            // DifferingPaths of .github/workflows: the workflow files that differ from the base's
	differ    map[string][]string // own by the commit the merge is compared with
	handed    *gitx.CommitIdentity
	schema    []string
	commits   []string
	pushes    []string
	pushDirs  []string    // the directory each push ran in
	held      heldGitFake // fix_held_merge_test.go: the refs, parents and diffs of a held merge
	objects   objectsFake // fix_parent_objects_test.go: the isolated reads and the objects' check
}

func (g *mergeGit) DifferingPaths(_ context.Context, dir, _, b string, paths ...string) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.objects.read("DifferingPaths", dir)
	switch {
	case !slices.Contains(paths, workflowsPath):
		return nil, nil
	case g.differ != nil:
		return slices.Clone(g.differ[b]), nil
	}
	return slices.Clone(g.own), nil
}

// FirstParentIdentities answers that to is one merge, by the fixer App's
// bot, of the base's tip into from (the fixer's base merge), or handed.
func (g *mergeGit) FirstParentIdentities(_ context.Context, _, from, to string) ([]gitx.CommitIdentity, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.handed != nil {
		return []gitx.CommitIdentity{*g.handed}, nil
	}
	return []gitx.CommitIdentity{{SHA: to, Parents: []string{from, fixBaseTip}, AuthorEmail: fixerAppEmail, CommitterEmail: fixerAppEmail}}, nil
}

// mergeHarness is fixHarness whose git merges parents (mergeGit), and
// whose GitHub names fixBaseTip as the tip of every PR's base, as the
// slot's origin/<base> has it.
func mergeHarness(t *testing.T, mods ...func(*harness)) (*harness, *mergeGit) {
	t.Helper()
	mg := &mergeGit{}
	h, _ := fixHarness(t, append(mods, func(h *harness) {
		mg.fixGit, mg.gh = h.d.Git.(*fixGit), h.gh
		h.gh.baseOid = fixBaseTip
		h.d.Git = mg
	})...)
	return h, mg
}

func (g *mergeGit) IsAncestor(ctx context.Context, dir, ancestor, rev string) (bool, error) {
	switch ancestor {
	case rev:
		return true, nil
	case fixBaseTip:
		g.mu.Lock()
		defer g.mu.Unlock()
		return !g.behind, nil
	}
	return g.fixGit.IsAncestor(ctx, dir, ancestor, rev)
}

func (g *mergeGit) MergeTree(ctx context.Context, dir, head, base string) (string, []string, error) {
	paths, err := g.MergeConflicts(ctx, dir, head, base)
	if err != nil || len(paths) > 0 {
		return "", paths, err
	}
	return fixParentTree, nil, nil
}

func (g *mergeGit) CommitTree(_ context.Context, dir, tree string, parents []string, message string, who gitx.Signature) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.commits = append(g.commits, fmt.Sprintf("%s:%s:%s:%s <%s>:%s", dir, tree, strings.Join(parents, ","), who.Name, who.Email, message))
	return fixParentMerge, nil
}

func (g *mergeGit) ChangedPaths(_ context.Context, dir, base, head string, pathspecs ...string) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if slices.Contains(pathspecs, workflowsPath) {
		g.objects.read("ChangedPaths", dir)
		return slices.Clone(g.workflows), nil
	}
	return slices.Clone(g.schema), nil
}

func (g *mergeGit) PushLeaseGitHubAs(ctx context.Context, dir, owner, repo, sha, branch, expect string, env map[string]string) error {
	g.mu.Lock()
	gate, started, panics := g.held.pushGate, g.held.pushStarted, g.held.pushPanics
	g.mu.Unlock()
	if gate != nil { // fix_held_merge_test.go: the push parks until the test lets it go, or its context ends
		started <- struct{}{}
		select {
		case <-gate:
		case <-ctx.Done():
			return fmt.Errorf("git push: %w", ctx.Err())
		}
	}
	if panics {
		panic("git push: a fake's panic")
	}
	g.mu.Lock()
	g.pushes = append(g.pushes, fmt.Sprintf("%s/%s:%s:%s:%s:%s", owner, repo, sha, branch, expect, pushedAs(env)))
	g.pushDirs = append(g.pushDirs, dir)
	err := g.pushErr
	g.mu.Unlock()
	if err == nil {
		g.gh.setHead(owner+"/"+repo, 2, sha)
	}
	return err
}

// pushedAs is who a push with env goes as in mergeGit's record: its gh
// config dir, and "+GH_TOKEN" when a token of the daemon's own rides in
// the env (identity.App.DaemonPushEnv).
func pushedAs(env map[string]string) string {
	if env["GH_TOKEN"] != "" {
		return env["GH_CONFIG_DIR"] + "+GH_TOKEN"
	}
	return env["GH_CONFIG_DIR"]
}

// pushedFrom are the directories the pushes so far ran in.
func (g *mergeGit) pushedFrom() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.pushDirs)
}

// made are the merge commits and pushes the daemon made so far.
func (g *mergeGit) made() (commits, pushes []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.commits), slices.Clone(g.pushes)
}

// The daemon's merge of the PR's parent: the merge-tree of the head and
// the tip of the base, a commit of it with both as parents by the fixer
// App's bot, and a push with a lease on the head as the App.
func wantParentMerged(t *testing.T, h *harness, mg *mergeGit, head string) {
	t.Helper()
	slot := h.slot("review1")
	commits, pushes := mg.made()
	wantCommit := fmt.Sprintf("%s:%s:%s,%s:%s <%s>:Merge branch 'master' into feature", slot.Path, fixParentTree, head, fixBaseTip, fixerAppLogin, fixerAppEmail)
	if len(commits) != 1 || commits[0] != wantCommit {
		t.Errorf("merge commits = %q, want %q", commits, wantCommit)
	}
	wantPush := fmt.Sprintf("talkable/talkable:%s:feature:%s:/state/gh/%s", fixParentMerge, head, fixerAppName)
	if len(pushes) != 1 || pushes[0] != wantPush {
		t.Errorf("pushes = %q, want %q", pushes, wantPush)
	}
	if cur := h.pr(2); cur.HeadSHA != fixParentMerge {
		t.Errorf("the PR's head = %s, want the merge %s", cur.HeadSHA, fixParentMerge)
	}
	if cur := h.slot("review1"); deref(cur.CheckedOutSHA) != fixParentMerge {
		t.Errorf("slot review1 holds %s, want the merge", deref(cur.CheckedOutSHA))
	}
}

// The operator asked for a merge of the parent on any PR of theirs, with
// or without a magnum review. A merge without a conflict is the daemon's:
// no agent starts. It merges the tip of the PR's base into its head with
// `git merge-tree` and `git commit-tree` (the head first, the tip second,
// by the fixer App's bot), pushes the merge with a lease on the head as the
// App, moves the slot and the PR's head to it, and ends: no fixer, no
// forced review (a push that only merges the base keeps the verdict).
func TestAMergeOfACleanParentIsTheDaemonsAndStartsNoFixer(t *testing.T) {
	h, mg := mergeHarness(t, withFixerApp)
	pr := fixablePR(h, "zhuravel", nil) // reviewed, but no review stands to fix: the merge needs none
	mg.behind = true
	dry := h.fix(FixPayload{Mode: FixModeMerge, DryRun: true})
	h.handle()
	const what = "magnum merges master at c0c0c0c into feature as " + fixerAppLogin + ", with no conflict"
	if plan := h.wantPlan(dry); plan.Text != "dry run: would merge the parent of talkable/talkable#2: "+what {
		t.Errorf("dry run = %q", plan.Text)
	}
	if commits, pushes := mg.made(); len(commits)+len(pushes) != 0 {
		t.Fatalf("the dry run merged: %v %v", commits, pushes)
	}

	id := h.fix(FixPayload{Mode: FixModeMerge})
	h.handle()
	if res := h.wantDone(id); res != "merging the parent of talkable/talkable#2: "+what+"; `magnum abort talkable/talkable#2` stops it" {
		t.Errorf("answer = %q", res)
	}
	wantParentMerged(t, h, mg, fixHead)
	if n := len(h.fixRuns(pr.ID)); n != 0 || h.ag.count("submit:") != 0 {
		t.Errorf("fix runs %d, submits %v: an agent started for a clean merge", n, h.ag.all())
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the merge's loop row stayed")
	}
	if ev, ok := h.fixEvent("fix.ended"); !ok || !strings.Contains(ev.Message, "merged master at c0c0c0c with no conflict: the head is e7e7e7e") {
		t.Errorf("fix.ended = %q, %v", ev.Message, ok)
	}
	if _, ok := h.fixEvent("pr.forced"); ok {
		t.Error("the merge forced a review")
	}
}

// A merge of the parent that conflicts goes to the fix role alone, with a
// prompt of the merge (no review, no findings, no other reviewer's item):
// the conflicting paths and the tip it merges. Its push is judged as any
// push of a merge: the loop ends with no forced re-review. A pushed merge
// that changes the pool's schema paths marks the slot's schema dirty.
func TestAMergeOfAConflictingParentGoesToTheFixerAlone(t *testing.T) {
	h, mg := mergeHarness(t, withFixerApp, func(h *harness) { h.cfg.Pools[0].SchemaPaths = []string{"db/schema.rb"} })
	pr := fixablePR(h, "zhuravel", nil)
	mg.conflict("app/models/offer.rb", "db/schema.rb")
	mg.schema = []string{"db/schema.rb"}
	fixer(h, mg.fixGit, fixMergeHead, func(run store.Run) string {
		return fixReport(run.ID, "d3d3d3d Merge c0c0c0c: not run: environment", "0 threads (0 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none")
	})
	id := h.fix(FixPayload{Mode: FixModeMerge})
	h.handle()
	const what = "claude-fix merges master at c0c0c0c (2 conflicting files) into feature as " + fixerAppLogin
	if res := h.wantDone(id); !strings.Contains(res, what) {
		t.Errorf("answer = %q, want it to say %q", res, what)
	}
	if commits, pushes := mg.made(); len(commits)+len(pushes) != 0 {
		t.Errorf("the daemon merged a conflict: %v %v", commits, pushes)
	}
	in := h.fixInput()
	if !in.MergeOnly || in.ConflictBase != "master" || in.ConflictSHA != fixBaseTip || len(in.ConflictPaths) != 2 || in.ExternalFile != "" || in.ReviewFile != "" {
		t.Errorf("the prompt = %+v, want the merge alone", in)
	}
	if n := len(h.fixRuns(pr.ID)); n != 1 {
		t.Errorf("fix runs = %d, want 1", n)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the merge's loop waits for a re-review")
	}
	if _, ok := h.fixEvent("pr.forced"); ok {
		t.Error("the merge forced a review")
	}
	if cur := h.pr(2); cur.HeadSHA != fixMergeHead {
		t.Errorf("the PR's head = %s, want the fixer's merge", cur.HeadSHA)
	}
	if !h.slot("review1").DirtySchema {
		t.Error("the slot's schema is not marked dirty after a merge that changed it")
	}
}

// A merge of the parent is refused where the daemon cannot make it: a fix
// role that is no fixer App (the daemon pushes as one), another author's
// PR (own PRs alone for now), a base nobody names; a head that has the
// base's tip already is answered with nothing to merge, and no job.
func TestAMergeOfTheParentIsRefusedWhereItCannotRun(t *testing.T) {
	storetest.Parallel(t)
	for _, tc := range []struct {
		name, author string
		app, behind  bool
		noBase       bool
		want         string
		failed       bool
	}{
		{name: "a gh fixer", author: "zhuravel", behind: true, failed: true,
			want: "magnum merges a PR's parent as a fixer App, and the fix role's identity zhuravel is not one: set the watch's fix_identity to a dedicated App"},
		{name: "another author's PR", author: "alice", app: true, behind: true, failed: true,
			want: "PR #2 is alice's, not yours: magnum merges the parent of your own PRs alone"},
		{name: "no known base", author: "zhuravel", app: true, behind: true, noBase: true, failed: true,
			want: "talkable/talkable#2: no parent merge: the PR's base branch is not known yet"},
		{name: "up to date", author: "zhuravel", app: true,
			want: "nothing to merge: talkable/talkable#2 has the tip of master already"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mods []func(*harness)
			if tc.app {
				mods = append(mods, withFixerApp)
			}
			h, mg := mergeHarness(t, mods...)
			pr := fixablePR(h, tc.author, nil)
			mg.behind = tc.behind
			if tc.noBase {
				h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead, author: tc.author, noBase: true})
				if _, err := h.st.DB().ExecContext(h.ctx, `UPDATE prs SET base_ref = NULL WHERE id = ?`, pr.ID); err != nil {
					t.Fatal(err)
				}
			}
			id := h.fix(FixPayload{Mode: FixModeMerge})
			h.handle()
			r := h.request(id)
			if want := store.RequestDone; tc.failed {
				want = store.RequestFailed
				if r.State != want || deref(r.Result) != tc.want {
					t.Errorf("%s %q, want %s %q", r.State, deref(r.Result), want, tc.want)
				}
			} else if r.State != want || deref(r.Result) != tc.want {
				t.Errorf("%s %q, want %s %q", r.State, deref(r.Result), want, tc.want)
			}
			if commits, pushes := mg.made(); len(commits)+len(pushes) != 0 {
				t.Errorf("merged: %v %v", commits, pushes)
			}
			if _, ok := h.fixStateOf(2); ok {
				t.Error("a loop started")
			}
		})
	}
}

// Every fix run merges the parent first: on a loop's first cycle a head
// that lacks the base's tip, and merges it cleanly, is merged by the
// daemon before the fixer starts, and the fixer starts from that merge
// (its run's target, its fix-sync line), told that magnum merged it.
func TestAFixRunMergesABehindParentFirstAndTheFixerStartsFromTheMerge(t *testing.T) {
	h, mg := mergeHarness(t, withFixerApp)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	mg.behind = true
	fixer(h, mg.fixGit, fixPushed, func(run store.Run) string {
		return fixReport(run.ID, "b2b2b2b Fix the total (review comment 4100000001)", fixOneFixed, "none")
	})
	id := h.fix(FixPayload{})
	h.handle()
	h.wantDone(id)
	wantParentMergedBeforeTheFixer(t, h, mg)
	runs := h.fixRuns(pr.ID)
	if len(runs) != 1 || runs[0].TargetSHA != fixParentMerge {
		t.Fatalf("fix runs = %+v, want one on the merge", runs)
	}
	in := h.fixInput()
	if in.MergedBase != "master" || in.MergedSHA != fixBaseTip || in.ConflictSHA != "" || in.MergeOnly {
		t.Errorf("the prompt's merge = %q %q, conflict %q, merge only %v; want magnum's merge of master", in.MergedBase, in.MergedSHA, in.ConflictSHA, in.MergeOnly)
	}
	if want := agents.FixSyncLine(h.d.Layout.Binary(), h.slot("review1").Path, fixParentMerge, "feature", "master"); in.FixSyncCommand != want {
		t.Errorf("fix-sync line = %q, want %q", in.FixSyncCommand, want)
	}
	if st, ok := h.fixStateOf(2); !ok || st.Phase != FixPhaseReviewing || st.Head != fixPushed {
		t.Errorf("loop = %+v, %v; want the re-review of the fixer's push", st, ok)
	}
}

// wantParentMergedBeforeTheFixer fails unless the daemon merged the
// parent (wantParentMerged) and its fix.start says the fixer starts from it.
func wantParentMergedBeforeTheFixer(t *testing.T, h *harness, mg *mergeGit) {
	t.Helper()
	commits, pushes := mg.made()
	if len(commits) != 1 || len(pushes) != 1 {
		t.Fatalf("merge commits %v, pushes %v; want the daemon's one merge", commits, pushes)
	}
	if ev, ok := h.fixEvent("fix.start"); !ok || !strings.Contains(ev.Message, "; on magnum's merge of master at c0c0c0c") {
		t.Errorf("fix.start = %q, %v", ev.Message, ok)
	}
}

// Later cycles of a loop merge the parent again only on a conflict: the
// guard of a second cycle leaves a head that is only behind as it is,
// while a first cycle's has the daemon merge it.
func TestALaterCycleMergesTheParentOnlyOnAConflict(t *testing.T) {
	h, mg := mergeHarness(t, withFixerApp)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	mg.behind = true
	repo, err := h.st.RepoByID(h.ctx, pr.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		cycle  int
		behind bool
	}{{1, true}, {2, false}} {
		job := &fixJob{repo: repo, watch: h.cfg.Watches[0], pr: pr, slot: h.slot("review1"), head: fixHead, state: FixState{Cycle: tc.cycle}}
		if err := h.e.checkFixBase(h.ctx, job); err != nil {
			t.Fatal(err)
		}
		if got := job.behind.sha != ""; got != tc.behind {
			t.Errorf("cycle %d: behind = %+v, want merged first: %v", tc.cycle, job.behind, tc.behind)
		}
	}
}

// A parent merge that changes the pool's schema paths marks the slot's
// schema dirty and loads the merged schema before the fixer starts, so the
// fixer's specs run on it instead of falling to `not run: environment`.
func TestAParentMergeThatChangesTheSchemaLoadsItBeforeTheFixer(t *testing.T) {
	h, mg := mergeHarness(t, withFixerApp, func(h *harness) { h.cfg.Pools[0].SchemaPaths = []string{"db/schema.rb", "db/migrate"} })
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	mg.behind, mg.schema = true, []string{"db/schema.rb"}
	h.sl.mu.Lock()
	h.sl.ensureNote = "loaded the schema of e7e7e7e (the schema its databases carry is unknown)"
	h.sl.mu.Unlock()
	fixer(h, mg.fixGit, fixPushed, func(run store.Run) string {
		return fixReport(run.ID, "b2b2b2b Fix the total (review comment 4100000001)", fixOneFixed, "none")
	})
	id := h.fix(FixPayload{})
	h.handle()
	h.wantDone(id)
	if !h.slot("review1").DirtySchema {
		t.Error("the slot's schema is not marked dirty")
	}
	calls := h.sl.all()
	ensure, submit := slices.Index(calls, "ensure_schema:review1"), h.callIndex("submit:claude-fix", 0)
	if ensure < 0 || submit < 0 {
		t.Fatalf("slots calls %v, agents %v: want the schema loaded and the fixer prompted", calls, h.ag.all())
	}
	if ev, ok := h.fixEvent("fix.parent_merged"); !ok || !strings.Contains(ev.Message, "; loaded the schema of e7e7e7e") {
		t.Errorf("fix.parent_merged = %q, %v", ev.Message, ok)
	}
}

// A branch that is only behind its base never starts an autofix loop by
// itself, as a conflict alone does not: autofix starts on findings, and
// the parent is merged first within the loop it starts.
func TestAutofixDoesNotStartForABranchThatIsOnlyBehind(t *testing.T) {
	var mg *mergeGit
	h, _ := autofixHarness(t, "zhuravel", findings(map[string]int{}, "COMMENTED"), withFixerApp, func(h *harness) {
		mg = &mergeGit{fixGit: h.d.Git.(*fixGit), gh: h.gh, behind: true}
		h.d.Git = mg
	})
	h.tick()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 0 {
		t.Errorf("fix runs = %d, want none", n)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("a loop started for a branch that is only behind")
	}
	if commits, pushes := mg.made(); len(commits)+len(pushes) != 0 {
		t.Errorf("autofix merged the parent: %v %v", commits, pushes)
	}
}

// The daemon pushes a merge of the parent to the PR's own branch alone: a
// PR whose head is master, main or the repository's default branch (a PR
// from main into production) is never pushed to by the bot, as `magnum
// push` refuses it. A merge of the parent is refused before any job, and
// a fix run's first cycle leaves the parent unmerged and says why.
func TestTheDaemonNeverPushesAParentMergeToMasterMainOrTheDefaultBranch(t *testing.T) {
	storetest.Parallel(t)
	for _, tc := range []struct{ head, def string }{{"master", "main"}, {"main", "main"}, {"develop", "develop"}} {
		t.Run("a merge of the parent from "+tc.head, func(t *testing.T) {
			h, mg := mergeHarness(t, withFixerApp)
			pr := fixablePR(h, "zhuravel", nil)
			mg.behind = true
			onBranch(h, pr, tc.head, tc.def)
			id := h.fix(FixPayload{Mode: FixModeMerge})
			h.handle()
			r := h.request(id)
			want := "talkable/talkable#2: magnum never pushes to " + tc.head + " (master, main, the default branch or the PR's base)"
			if r.State != store.RequestFailed || deref(r.Result) != want {
				t.Errorf("%s %q, want failed %q", r.State, deref(r.Result), want)
			}
			if commits, pushes := mg.made(); len(commits)+len(pushes) != 0 {
				t.Errorf("merged: %v %v", commits, pushes)
			}
		})
	}
	t.Run("a fix run", func(t *testing.T) {
		h, mg := mergeHarness(t, withFixerApp)
		pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
		mg.behind = true
		onBranch(h, pr, "main", "main")
		fixer(h, mg.fixGit, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixOneFixed, "none") })
		id := h.fix(FixPayload{})
		h.handle()
		h.wantDone(id)
		if _, pushes := mg.made(); len(pushes) != 0 {
			t.Errorf("pushes = %v, want none", pushes)
		}
		if ev, ok := h.fixEvent("fix.parent_refused"); !ok || !strings.Contains(ev.Message, "magnum never pushes to main") {
			t.Errorf("fix.parent_refused = %q, %v", ev.Message, ok)
		}
	})
}

// onBranch makes pr's head branch head and its repository's default branch
// def in the registry.
func onBranch(h *harness, pr store.PR, head, def string) {
	h.t.Helper()
	if _, err := h.st.DB().ExecContext(h.ctx, `UPDATE prs SET head_ref = ? WHERE id = ?`, head, pr.ID); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.st.DB().ExecContext(h.ctx, `UPDATE repos SET default_branch = ? WHERE id = ?`, def, pr.RepoID); err != nil {
		h.t.Fatal(err)
	}
}

// A PR retargeted after the poll's last read keeps the base GitHub names
// past the daemon's merge of it: the merge is of that base, and the fixer
// that starts from the merge is told that base (its fix-sync and push
// lines, its pane's MAGNUM_BASE_BRANCH), not the registry's older one.
func TestTheDaemonsMergeOfARetargetedPRKeepsTheBaseGitHubNames(t *testing.T) {
	h, mg := mergeHarness(t, withFixerApp)
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead, author: "zhuravel", base: "release"})
	mg.behind = true
	fixer(h, mg.fixGit, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixOneFixed, "none") })
	id := h.fix(FixPayload{})
	h.handle()
	h.wantDone(id)
	if commits, _ := mg.made(); len(commits) != 1 || !strings.HasSuffix(commits[0], ":Merge branch 'release' into feature") {
		t.Errorf("merge commits = %q, want the merge of release", commits)
	}
	slot := h.slot("review1")
	in := h.fixInput()
	if want := agents.FixSyncLine(h.d.Layout.Binary(), slot.Path, fixParentMerge, "feature", "release"); in.FixSyncCommand != want {
		t.Errorf("fix-sync line = %q, want %q", in.FixSyncCommand, want)
	}
	if want := agents.PushLine(h.layout.Binary(), slot.Path, "release"); in.PushCommand != want {
		t.Errorf("push line = %q, want %q", in.PushCommand, want)
	}
	if env := h.ag.paneEnv("claude-fix"); env["MAGNUM_BASE_BRANCH"] != "release" {
		t.Errorf("the pane's MAGNUM_BASE_BRANCH = %q, want release", env["MAGNUM_BASE_BRANCH"])
	}
}
