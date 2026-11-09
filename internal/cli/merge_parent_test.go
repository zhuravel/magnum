package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

const (
	// mergePlan is the daemon's answer to a dry run of a merge of the
	// parent, and mergeAnswer to the merge.
	mergePlan   = "dry run: would merge the parent of talkable/talkable#5: magnum merges master at c0c0c0c into feature as fixer-app[bot], with no conflict"
	mergeAnswer = "merging the parent of talkable/talkable#5: magnum merges master at c0c0c0c into feature as fixer-app[bot], with no conflict; " +
		"`magnum abort talkable/talkable#5` stops it"
)

// mergeAsked is what the daemon's plan (mergePlan) asks about: the PR's
// head, master at c0c0c0c, no conflict, planned at mergePlannedAt.
var mergeAsked = engine.MergeAsked{Head: "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1", Base: "master",
	Tip: "c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0", At: time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)}

// mergePlanResult is the daemon's answer to the dry run, as the engine
// writes it: the plan as JSON on its first line, then its words
// (mergePlan).
func mergePlanResult(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(engine.MergePlan{Ref: "talkable/talkable#5", Asked: mergeAsked, Text: mergePlan})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n" + mergePlan
}

// mergeDaemon answers each pending request as the daemon does: a dry run
// with mergePlanResult, a merge with mergeAnswer.
func (h *actHarness) mergeDaemon() {
	h.onSleep = func(h *actHarness) {
		for _, r := range h.requests() {
			if r.State != store.RequestPending {
				continue
			}
			res := mergeAnswer
			if actDecode[engine.FixPayload](h.t, r.Payload).DryRun {
				res = mergePlanResult(h.t)
			}
			if err := h.st.CompleteRequest(h.ctx, r.ID, store.RequestDone, res); err != nil {
				h.t.Fatal(err)
			}
		}
	}
}

// `magnum merge-parent` merges the PR's base into it as the fixer App's
// bot. On a terminal its y/N names what the daemon would do, the branch,
// the base's tip and the bot's login, from a dry run asked first: only y
// queues the merge, a request of kind fix in the merge mode, bound to what
// the plan asked about (the head, master at c0c0c0c, no conflict, when),
// which the daemon checks again before it merges.
func TestMergeParentAsksWithTheDaemonsPlanThenQueuesTheMerge(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.mergeDaemon()
	const question = "Merge the parent of talkable/talkable#5: magnum merges master at c0c0c0c into feature as fixer-app[bot], with no conflict? [y/N]"

	h.stdin("n\n")
	if code := h.cmd("merge-parent", "5"); code != 1 {
		t.Fatalf("exit %d: %s%s", code, h.out.String(), h.errb.String())
	}
	actContains(t, h.out.String(), question)
	actContains(t, h.errb.String(), "nothing queued")
	reqs := h.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %+v, want the dry run alone", reqs)
	}
	if p := actDecode[engine.FixPayload](t, reqs[0].Payload); !p.DryRun || p.Mode != engine.FixModeMerge || p.Number != 5 {
		t.Fatalf("the plan's payload = %+v", p)
	}

	h.stdin("y\n")
	h.out.Reset()
	if code := h.cmd("merge-parent", "5"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	reqs = h.requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want a dry run and the merge more", len(reqs))
	}
	if p := actDecode[engine.FixPayload](t, reqs[2].Payload); p.DryRun || p.Mode != engine.FixModeMerge || reqs[2].Kind != engine.ReqFix {
		t.Fatalf("the merge's payload = %+v (%s)", p, reqs[2].Kind)
	} else if p.Asked == nil || !sameAsked(*p.Asked, mergeAsked) {
		t.Errorf("the merge is bound to %+v, want the plan's %+v", p.Asked, mergeAsked)
	}
	actContains(t, h.out.String(), question, mergeAnswer)
}

// sameAsked reports whether two bindings name the same plan.
func sameAsked(a, b engine.MergeAsked) bool {
	return a.Head == b.Head && a.Base == b.Base && a.Tip == b.Tip && a.Conflicts == b.Conflicts && a.At.Equal(b.At)
}

// --yes asks nothing and sends the merge alone, bound to nothing (the
// daemon merges the tip its fetch brings); --dry-run asks the daemon what
// would happen and asks nothing.
func TestMergeParentWithYesSendsTheMergeAloneAndADryRunAsksNothing(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.mergeDaemon()
	if code := h.cmd("merge-parent", "5", "--yes"); code != 0 || strings.Contains(h.out.String(), "[y/N]") {
		t.Fatalf("--yes: exit %d: %s%s", code, h.out.String(), h.errb.String())
	}
	if reqs := h.requests(); len(reqs) != 1 || actDecode[engine.FixPayload](t, reqs[0].Payload).DryRun {
		t.Fatalf("requests = %+v, want the merge alone", reqs)
	} else if p := actDecode[engine.FixPayload](t, reqs[0].Payload); p.Asked != nil {
		t.Errorf("--yes bound the merge to %+v, which nobody was asked", p.Asked)
	}
	h.out.Reset()
	if code := h.cmd("merge-parent", "5", "--dry-run"); code != 0 || strings.Contains(h.out.String(), "[y/N]") {
		t.Fatalf("--dry-run: exit %d: %s", code, h.out.String())
	}
	actContains(t, h.out.String(), mergePlan)
	if strings.Contains(h.out.String(), `{"ref"`) {
		t.Errorf("--dry-run printed the plan's JSON line:\n%s", h.out.String())
	}
	if code := h.cmd("merge-parent"); code != 2 {
		t.Errorf("no PR: exit %d", code)
	}
}

// Without a terminal to ask on (an agent's shell, a pipe, a script) a
// merge needs --yes, as a staging deploy does: it never starts on nobody's
// word. --dry-run still asks the daemon's plan, and --yes sends the merge.
func TestMergeParentWithoutATerminalNeedsYes(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.mergeDaemon()
	for _, args := range [][]string{{"5"}, {"5", "--json"}} {
		h.errb.Reset()
		if code := h.cmd("merge-parent", args...); code != 2 {
			t.Errorf("%v: exit %d, want 2: %s%s", args, code, h.out.String(), h.errb.String())
		}
		actContains(t, h.errb.String(), "pass --yes to merge without asking, or --dry-run to see the plan; nothing queued")
	}
	if reqs := h.requests(); len(reqs) != 0 {
		t.Fatalf("requests = %+v, want none", reqs)
	}
	h.out.Reset()
	if code := h.cmd("merge-parent", "5", "--dry-run"); code != 0 {
		t.Fatalf("--dry-run: exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), mergePlan)
	if code := h.cmd("merge-parent", "5", "--yes"); code != 0 {
		t.Fatalf("--yes: exit %d: %s", code, h.errb.String())
	}
	if reqs := h.requests(); len(reqs) != 2 || actDecode[engine.FixPayload](t, reqs[1].Payload).DryRun {
		t.Errorf("requests = %+v, want the plan and the merge", reqs)
	}
}

// --wait follows the merge to its end: a merge the daemon made, or the
// fixer pushed, ends the loop with fix.ended, which is a success (exit 0,
// "merged"), not a loop that ended otherwise.
func TestMergeParentWaitSaysTheParentWasMerged(t *testing.T) {
	h := newActHarness(t)
	h.pid = 1
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	step := 0
	h.onSleep = func(h *actHarness) {
		switch step++; step {
		case 1:
			h.completePending(store.RequestDone, mergeAnswer)
			h.setFixRow(pr, engine.FixPhaseFixing, 1)
		case 3:
			h.fixLoopGone(pr)
			h.fixEvent("fix.ended", "the merge of the parent ends: merged master at c0c0c0c with no conflict: the head is e7e7e7e",
				map[string]any{"merged_base": "master", "merged_sha": "c0c0c0c0", "head": "e7e7e7e7"})
		}
	}
	if code := h.cmd("merge-parent", "5", "--wait", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s%s", code, h.out.String(), h.errb.String())
	}
	actContains(t, h.out.String(), "talkable#5: merged master at c0c0c0c with no conflict: the head is e7e7e7e")
}

// The screens' P runs the command's own path: its dry run hands back the
// daemon's plan as the result's question (Ask), and the merge the screen
// then asked y/N for queues the merge alone, never asking again; a plan
// the daemon refuses is the screen's error, with no question.
func TestScreenMergeParentPlansThenMerges(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.mergeDaemon()
	acts := newScreenActions(h.c)
	defer acts.Close()
	ctx := context.Background()

	res, err := acts.MergeParent(ctx, "talkable#5", true)
	if want := "Merge the parent of talkable/talkable#5: magnum merges master at c0c0c0c into feature as fixer-app[bot], with no conflict?"; err != nil || res.Ask != want {
		t.Fatalf("the plan = %q, %v; want the question %q", res.Ask, err, want)
	}
	if res, err = acts.MergeParent(ctx, "talkable#5", false); err != nil || res.Ask != "" || !strings.Contains(res.Text, mergeAnswer) {
		t.Fatalf("the merge = %+v, %v", res, err)
	}
	reqs := h.requests()
	if len(reqs) != 2 || !actDecode[engine.FixPayload](t, reqs[0].Payload).DryRun || actDecode[engine.FixPayload](t, reqs[1].Payload).DryRun {
		t.Fatalf("requests = %+v, want the plan and the merge", reqs)
	}
	h.onSleep = func(h *actHarness) {
		h.completePending(store.RequestFailed, "PR #5 is alice's, not yours: magnum merges the parent of your own PRs alone")
	}
	if res, err := acts.MergeParent(ctx, "talkable#5", true); err == nil || res.Ask != "" {
		t.Errorf("a refused plan = %+v, %v; want the error and no question", res, err)
	}
}
