package engine

import (
	"strings"
	"testing"
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
