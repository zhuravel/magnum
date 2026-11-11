package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// wantPlan fails unless the dry run id answered a merge plan, and returns
// it.
func (h *harness) wantPlan(id int64) MergePlan {
	h.t.Helper()
	res := h.wantDone(id)
	plan, ok := ParseMergePlan(res)
	if !ok {
		h.t.Fatalf("the dry run's answer is no plan: %q", res)
	}
	if lastLine(res) != plan.Text {
		h.t.Errorf("the answer's last line = %q, want the plan's words %q", lastLine(res), plan.Text)
	}
	return plan
}

// planMerge asks the daemon's plan of a merge of PR #2's parent (a dry
// run) and returns it.
func (h *harness) planMerge() MergePlan {
	h.t.Helper()
	id := h.fix(FixPayload{Mode: FixModeMerge, DryRun: true})
	h.handle()
	return h.wantPlan(id)
}

// The y/N of a merge of the parent names what the daemon would merge,
// which only the daemon knows, so the y binds the merge to it: the plan (a
// dry run) answers, beside its words, the PR's head, the parent (its base
// branch), the tip of it the plan read and how many files of the merge
// conflict, and when it planned. The plan fetches the base first, so the
// question names the tip the base has now, not one fetched hours ago.
func TestAMergePlanAnswersTheHeadTheParentAndTheTipItAsksAbout(t *testing.T) {
	h, mg := mergeHarness(t, withFixerApp)
	fixablePR(h, "zhuravel", nil)
	mg.behind = true
	plan := h.planMerge()
	const words = "dry run: would merge the parent of talkable/talkable#2: magnum merges master at c0c0c0c into feature as " +
		fixerAppLogin + ", with no conflict"
	if plan.Ref != "talkable/talkable#2" || plan.Text != words {
		t.Errorf("the plan = %q %q, want talkable/talkable#2 %q", plan.Ref, plan.Text, words)
	}
	a := plan.Asked
	if a.Head != fixHead || a.Base != "master" || a.Tip != fixBaseTip || a.Conflicts != 0 || !a.At.Equal(h.clock.Now()) {
		t.Errorf("the plan asks about %+v, want the head %s, master at %s, no conflict, at %s", a, fixHead, fixBaseTip, h.clock.Now())
	}
	if fetches, _ := mg.calls(); len(fetches) != 1 || fetches[0] != h.slot("review1").MainClone+":master" {
		t.Errorf("fetches = %v, want the plan's of master", fetches)
	}

	mg.conflict("app/models/offer.rb", "db/schema.rb")
	plan = h.planMerge()
	if a := plan.Asked; a.Head != fixHead || a.Base != "master" || a.Tip != fixBaseTip || a.Conflicts != 2 {
		t.Errorf("the plan of a conflict asks about %+v, want master at %s with 2 conflicting files", a, fixBaseTip)
	}
	if want := "claude-fix merges master at c0c0c0c (2 conflicting files) into feature as " + fixerAppLogin; !strings.Contains(plan.Text, want) {
		t.Errorf("the plan's words = %q, want them to say %q", plan.Text, want)
	}
	if commits, pushes := mg.made(); len(commits)+len(pushes) != 0 {
		t.Errorf("a plan merged: %v %v", commits, pushes)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("a plan started a loop")
	}
}

// fixMovedTip is the tip the base moved to after the plan read fixBaseTip.
const fixMovedTip = "d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1"

func (g *mergeGit) RevParse(ctx context.Context, dir, ref string) (string, error) {
	g.mu.Lock()
	n, tips, baseErr := len(g.fetches), g.tips, g.baseErr
	g.mu.Unlock()
	if strings.HasPrefix(ref, "refs/remotes/origin/") && baseErr == nil && n > 0 && len(tips) > 0 {
		return tips[min(n, len(tips))-1], nil
	}
	return g.fixGit.RevParse(ctx, dir, ref)
}

// execSQL runs q on the registry.
func (h *harness) execSQL(q string, args ...any) {
	h.t.Helper()
	if _, err := h.st.DB().ExecContext(h.ctx, q, args...); err != nil {
		h.t.Fatal(err)
	}
}

// mergeBound sends the merge of PR #2's parent bound to asked, as the y of
// its question does, and handles it.
func (h *harness) mergeBound(asked MergeAsked) store.Request {
	h.t.Helper()
	id := h.fix(FixPayload{Mode: FixModeMerge, Asked: &asked})
	h.handle()
	return h.request(id)
}

// The y of a merge of the parent merges what its question named: a merge
// bound to the plan, with nothing moved since, merges that tip, as the
// daemon's merge does.
func TestAMergeBoundToItsQuestionMergesWhatItAskedAbout(t *testing.T) {
	h, mg := mergeHarness(t, withFixerApp)
	fixablePR(h, "zhuravel", nil)
	mg.behind = true
	plan := h.planMerge()
	h.advance(9 * time.Minute)
	r := h.mergeBound(plan.Asked)
	const want = "merging the parent of talkable/talkable#2: magnum merges master at c0c0c0c into feature as " + fixerAppLogin +
		", with no conflict; `magnum abort talkable/talkable#2` stops it"
	if r.State != store.RequestDone || deref(r.Result) != want {
		t.Fatalf("%s %q, want done %q", r.State, deref(r.Result), want)
	}
	wantParentMerged(t, h, mg, fixHead)
}

// The bug: the y of a merge of the parent bound nothing, and the daemon
// merged whatever the base's tip was when the y came, even one that
// conflicts and goes to the fix role. Now a merge bound to its question is
// refused, after its fetch, when the PR's head moved, its base was
// retargeted, the base's tip moved or the merge's conflicts changed since
// the plan, and when the question is as old as a staging deploy's wait (10
// minutes): the answer says what moved and that nothing was merged, and no
// loop starts.
func TestAMergeBoundToItsQuestionIsRefusedWhenWhatItAskedAboutMoved(t *testing.T) {
	storetest.Parallel(t)
	for _, tc := range []struct {
		name string
		move func(h *harness, mg *mergeGit, asked *MergeAsked)
		want string
	}{
		{"the head", func(h *harness, _ *mergeGit, _ *MergeAsked) {
			h.execSQL(`UPDATE prs SET head_sha = ? WHERE id = ?`, fixPushed, h.pr(2).ID)
		}, "talkable/talkable#2: its head moved from a1a1a1a to b2b2b2b since you asked: nothing was merged; ask again"},
		{"the base", func(h *harness, _ *mergeGit, _ *MergeAsked) {
			h.execSQL(`UPDATE prs SET base_ref = 'release' WHERE id = ?`, h.pr(2).ID)
		}, "talkable/talkable#2: its base moved from master to release since you asked: nothing was merged; ask again"},
		{"the base's tip", func(_ *harness, mg *mergeGit, _ *MergeAsked) {
			mg.tips = []string{fixBaseTip, fixMovedTip}
		}, "talkable/talkable#2: master moved from c0c0c0c to d1d1d1d since you asked: nothing was merged; ask again"},
		{"the conflicts", func(_ *harness, mg *mergeGit, _ *MergeAsked) {
			mg.mu.Lock()
			mg.mergeAnswers = [][]string{nil, {"app/models/offer.rb", "db/schema.rb"}}
			mg.mu.Unlock()
		}, "talkable/talkable#2: the merge of master at c0c0c0c has 2 conflicting files now, and you were asked about no conflict: " +
			"nothing was merged; ask again"},
		{"the wait", func(h *harness, _ *mergeGit, _ *MergeAsked) {
			h.advance(10 * time.Minute)
		}, "talkable/talkable#2: asked 10 minutes ago, longer than a merge of the parent waits for its y (10 minutes): " +
			"nothing was merged; ask again"},
		{"no plan", func(_ *harness, _ *mergeGit, asked *MergeAsked) {
			*asked = MergeAsked{}
		}, "talkable/talkable#2: the merge names no plan it was asked about: nothing was merged; ask again " +
			"(`magnum merge-parent talkable/talkable#2`)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, mg := mergeHarness(t, withFixerApp)
			fixablePR(h, "zhuravel", nil)
			mg.behind = true
			if tc.name == "the conflicts" { // the plan's look is the first: clean
				mg.mu.Lock()
				mg.mergeAnswers = [][]string{nil}
				mg.mu.Unlock()
			}
			asked := h.planMerge().Asked
			tc.move(h, mg, &asked)
			r := h.mergeBound(asked)
			if r.State != store.RequestFailed || deref(r.Result) != tc.want {
				t.Errorf("%s %q, want failed %q", r.State, deref(r.Result), tc.want)
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

// The job's guard fetches the base again, moments or (after a slot's
// claim) minutes after the request's check: a base whose tip moved in
// between ends the job with the same words, and nothing is merged.
func TestAMergeWhoseBaseMovesBeforeItsJobMergesNothing(t *testing.T) {
	h, mg := mergeHarness(t, withFixerApp)
	fixablePR(h, "zhuravel", nil)
	mg.behind = true
	asked := h.planMerge().Asked
	mg.tips = []string{fixBaseTip, fixBaseTip, fixMovedTip} // the plan's, the request's, the guard's
	if r := h.mergeBound(asked); r.State != store.RequestDone {
		t.Fatalf("the request: %s %q", r.State, deref(r.Result))
	}
	const why = "talkable/talkable#2: master moved from c0c0c0c to d1d1d1d since you asked: nothing was merged; ask again"
	if ev, ok := h.fixEvent("fix.ended"); !ok || !strings.Contains(ev.Message, why) {
		t.Errorf("fix.ended = %q, %v; want it to say %q", ev.Message, ok, why)
	}
	if commits, pushes := mg.made(); len(commits)+len(pushes) != 0 {
		t.Errorf("merged: %v %v", commits, pushes)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the merge's loop row stayed")
	}
}

// A binding goes with a merge of the parent alone.
func TestAFixWithAMergesBindingIsRefused(t *testing.T) {
	h, _ := mergeHarness(t, withFixerApp)
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	id := h.fix(FixPayload{Asked: &MergeAsked{Head: fixHead, Base: "master", Tip: fixBaseTip}})
	h.handle()
	const want = "magnum fix: a merge's plan (asked) goes with magnum merge-parent alone"
	if r := h.request(id); r.State != store.RequestFailed || deref(r.Result) != want {
		t.Errorf("%s %q, want failed %q", r.State, deref(r.Result), want)
	}
}

// A daemon run with --dry-run changes nothing outside its registry
// (Deps.DryRun): the plan of a merge of the parent fetches no base then,
// and a merge asked of it, bound to its question or not, is answered with
// the plan, merges nothing and fetches nothing either.
func TestADryRunDaemonsMergeOfTheParentFetchesNothing(t *testing.T) {
	h, mg := mergeHarness(t, withFixerApp)
	fixablePR(h, "zhuravel", nil)
	mg.behind = true
	h.e.d.DryRun = true
	asked := h.planMerge().Asked
	if r := h.mergeBound(asked); r.State != store.RequestDone {
		t.Fatalf("the bound merge: %s %q", r.State, deref(r.Result))
	} else if _, ok := ParseMergePlan(deref(r.Result)); !ok {
		t.Errorf("the bound merge's answer = %q, want the plan", deref(r.Result))
	}
	id := h.fix(FixPayload{Mode: FixModeMerge})
	h.handle()
	h.wantPlan(id)
	if fetches, _ := mg.calls(); len(fetches) != 0 {
		t.Errorf("fetches = %v, want none", fetches)
	}
	if commits, pushes := mg.made(); len(commits)+len(pushes) != 0 {
		t.Errorf("merged: %v %v", commits, pushes)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("a loop started")
	}
}

// The binding holds for the daemon's own push too. The question named
// master at c0c0c0c with a conflict, and the fixer merged master at
// d1d1d1d instead (it fetched the base again); its merge brings the
// base's workflow files, so magnum push handed it to the daemon. The
// daemon does not push it with its Workflows token, though d1d1d1d is on
// master as GitHub has it: nobody was asked about that tip.
func TestABoundMergesHandedMergeOfAnotherTipIsNotPushed(t *testing.T) {
	h, mg := handedMerge(t)
	other := gitx.CommitIdentity{SHA: fixMergeHead, Parents: []string{fixHead, fixMovedTip}, AuthorEmail: fixerAppEmail, CommitterEmail: fixerAppEmail}
	mg.mu.Lock()
	mg.handed = &other
	mg.mu.Unlock()
	fixer(h, mg.fixGit, fixMergeHead, func(run store.Run) string {
		h.gh.mu.Lock()
		h.gh.baseOid = fixMovedTip // master moved on GitHub while the fixer ran
		h.gh.mu.Unlock()
		return fixReport(run.ID, "d3d3d3d Merge d1d1d1d: not run: environment", "0 threads (0 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none")
	})
	asked := h.planMerge().Asked
	if r := h.mergeBound(asked); r.State != store.RequestDone {
		t.Fatalf("the bound merge: %s %q", r.State, deref(r.Result))
	}
	if _, pushes := mg.made(); len(pushes) != 0 {
		t.Errorf("pushes = %v, want none", pushes)
	}
	if mints, _ := h.ids[fixerAppName].daemonPushes(); len(mints) != 0 {
		t.Errorf("daemon push tokens minted: %v", mints)
	}
	pr := h.wantState(2, store.PRNeedsAttention)
	const want = "fix: magnum did not push claude-fix's merge of the parent of talkable/talkable#2: it merges master at d1d1d1d, not the tip " +
		"c0c0c0c you were asked about; merge master into feature by hand and resolve app/models/offer.rb (GitHub offers no Update branch on a conflict)"
	if got := deref(pr.LastError); got != want {
		t.Errorf("attention = %q, want %q", got, want)
	}
}
