package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// pushedCIHarness is a repository whose CI workflow ci.yml runs only when
// a PR opens and on workflow_dispatch ([[repo]] ci_dispatch), whose PR #2,
// the operator's, the daemon merged its parent into (fixParentMerge):
// magnum's own push, after which nothing runs CI at the new head.
func pushedCIHarness(t *testing.T, files []string, mods ...func(*harness)) (*harness, *mergeGit) {
	t.Helper()
	h, mg := mergeHarness(t, append([]func(*harness){withFixerApp, func(h *harness) {
		h.cfg.Repos = append(h.cfg.Repos, config.Repo{Repo: "talkable/talkable", CIDispatch: "ci.yml"})
		h.gh.defaultFiles = files
	}}, mods...)...)
	fixablePR(h, "zhuravel", nil)
	mg.behind = true
	id := h.fix(FixPayload{Mode: FixModeMerge})
	h.handle()
	h.wantDone(id)
	if h.pr(2).HeadSHA != fixParentMerge {
		t.Fatalf("the merge was not pushed: head %s", h.pr(2).HeadSHA)
	}
	return h, mg
}

// The repository's CI runs only when a PR opens, so a push magnum makes to
// a PR (a merge of its parent, a fix) gets no run of the checks GitHub
// requires. magnum starts ci_dispatch's workflow on the PR's branch itself,
// 2 minutes after its push (a run the push started, on a repository whose
// CI does run on pushes, shows by then), as R47 starts it: as [[repo]]
// ci_dispatch_as, else the poll identity, and once for the head, recorded
// as the look's start, so the look waits for its run.
func TestAPushMagnumMadeStartsTheCIWorkflowOnceAfterAGrace(t *testing.T) {
	h, _ := pushedCIHarness(t, []string{"app/models/order.rb"})
	h.tick()
	if calls := h.ghCalls("dispatch:"); len(calls) != 0 {
		t.Fatalf("dispatched %q within the grace", calls)
	}
	h.advance(ciPushGrace)
	h.tick()
	if calls := h.ghCalls("dispatch:"); len(calls) != 1 || calls[0] != "dispatch:talkable/talkable:ci.yml@feature" {
		t.Fatalf("dispatches = %q, want ci.yml on the PR's branch once", calls)
	}
	if s := h.e.ciStartAt(h.ctx, h.pr(2)); s.Head != fixParentMerge || s.At.IsZero() || s.Workflow != "ci.yml" {
		t.Errorf("the start's record = %+v, want the merge's", s)
	}
	ev := h.fixEvents("fix.push_ci_dispatch")
	if len(ev) != 1 || !strings.Contains(ev[0].Message, "magnum pushed e7e7e7e; started ci.yml on the PR's branch as zhuravel") {
		t.Errorf("fix.push_ci_dispatch = %+v", ev)
	}
	h.advance(10 * time.Minute)
	h.tick()
	h.tick()
	if calls := h.ghCalls("dispatch:"); len(calls) != 1 {
		t.Errorf("dispatches = %q, want one per head", calls)
	}
}

// A start of ci.yml cancels a run of the same branch that is in progress
// (its concurrency group is the workflow and the branch): magnum never
// starts it while one runs, and starts it once that run ended. A head that
// a run of the workflow ran at already (a repository whose CI runs on
// pushes) needs no start at all.
func TestAPushMagnumMadeWaitsForTheBranchsRunAndSkipsAHeadThatRan(t *testing.T) {
	storetest.Parallel(t)
	t.Run("a run of the branch in progress", func(t *testing.T) {
		h, _ := pushedCIHarness(t, []string{"app/models/order.rb"})
		running := github.WorkflowRun{ID: 7, Status: "in_progress", HeadSHA: fixHead, HeadBranch: "feature", Event: "workflow_dispatch"}
		other := github.WorkflowRun{ID: 6, Status: "in_progress", HeadSHA: fixBaseTip, HeadBranch: "master", Event: "push"}
		h.gh.mu.Lock()
		h.gh.recent = []github.WorkflowRun{running, other}
		h.gh.mu.Unlock()
		h.advance(ciPushGrace)
		h.tick()
		if calls := h.ghCalls("dispatch:"); len(calls) != 0 {
			t.Fatalf("dispatched %q while a run of the branch ran", calls)
		}
		running.Status, running.Conclusion = "completed", "success"
		h.gh.mu.Lock()
		h.gh.recent = []github.WorkflowRun{running, other}
		h.gh.mu.Unlock()
		h.advance(time.Minute)
		h.tick()
		if calls := h.ghCalls("dispatch:"); len(calls) != 1 {
			t.Errorf("dispatches = %q, want one once the branch's run ended", calls)
		}
	})
	t.Run("a run at the head", func(t *testing.T) {
		h, _ := pushedCIHarness(t, []string{"app/models/order.rb"})
		h.gh.mu.Lock()
		h.gh.recent = []github.WorkflowRun{{ID: 8, Status: "queued", HeadSHA: fixParentMerge, HeadBranch: "feature", Event: "push"}}
		h.gh.mu.Unlock()
		h.advance(ciPushGrace)
		h.tick()
		h.advance(10 * time.Minute)
		h.tick()
		if calls := h.ghCalls("dispatch:"); len(calls) != 0 {
			t.Errorf("dispatched %q for a head its run ran at", calls)
		}
		if n := h.ghCalls("recent_runs:"); len(n) != 1 {
			t.Errorf("runs read %d times, want once: the push needs nothing more", len(n))
		}
	})
}

// R47's refusals hold for magnum's own pushes: a PR that changes its own
// workflows or actions never gets its CI started (it would run its own
// code with the repository's secrets), and a head that moved on since
// magnum's push (the author pushed) is not magnum's to start.
func TestAPushMagnumMadeKeepsR47sRefusals(t *testing.T) {
	storetest.Parallel(t)
	t.Run("the PR changes its own workflows", func(t *testing.T) {
		h, _ := pushedCIHarness(t, []string{"app/models/order.rb", ".github/workflows/ci.yml"})
		h.advance(ciPushGrace)
		h.tick()
		if calls := h.ghCalls("dispatch:"); len(calls) != 0 {
			t.Fatalf("dispatched %q", calls)
		}
		if s := h.e.ciStartAt(h.ctx, h.pr(2)); s.Refused != "the PR changes its own workflows or actions" {
			t.Errorf("the start's record = %+v, want refused", s)
		}
		if ev := h.fixEvents("fix.push_ci_dispatch_refused"); len(ev) != 1 {
			t.Errorf("fix.push_ci_dispatch_refused = %+v, want one", ev)
		}
	})
	t.Run("the head moved on", func(t *testing.T) {
		h, _ := pushedCIHarness(t, []string{"app/models/order.rb"})
		const authors = "f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9f9"
		h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: authors, author: "zhuravel"})
		h.advance(ciPushGrace)
		h.tick()
		h.tick()
		if calls := h.ghCalls("dispatch:"); len(calls) != 0 {
			t.Errorf("dispatched %q for a head magnum did not push", calls)
		}
	})
}

// A fixer's push is magnum's too: the settle that confirms it on GitHub
// records it, and its CI starts after the grace.
func TestAFixersPushStartsTheCIWorkflow(t *testing.T) {
	h, git := fixHarness(t, withFixerApp, func(h *harness) {
		h.cfg.Repos = append(h.cfg.Repos, config.Repo{Repo: "talkable/talkable", CIDispatch: "ci.yml"})
		h.gh.defaultFiles = []string{"app/models/order.rb"}
	})
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	fixer(h, git, fixPushed, func(run store.Run) string {
		return fixReport(run.ID, "b2b2b2b Fix the total (review comment 4100000001)", fixOneFixed, "none")
	})
	id := h.fix(FixPayload{})
	h.handle()
	h.wantDone(id)
	if h.pr(2).HeadSHA != fixPushed {
		t.Fatalf("the fixer's push is not on the PR: %s", h.pr(2).HeadSHA)
	}
	h.advance(ciPushGrace)
	h.tick()
	if calls := h.ghCalls("dispatch:"); len(calls) != 1 || calls[0] != "dispatch:talkable/talkable:ci.yml@feature" {
		t.Errorf("dispatches = %q, want ci.yml on the PR's branch once", calls)
	}
}
