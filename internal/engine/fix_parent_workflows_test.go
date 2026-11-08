package engine

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/identity"
	"github.com/zhuravel/magnum/internal/store"
)

// daemonPushDir is the gh config dir of the fixer App's daemon-only push
// token (fakeAppIdentity.DaemonPushEnv), never the pane's /state/gh/<name>.
const daemonPushDir = "/state/gh/." + fixerAppName + ".push"

// withoutWorkflows makes the fixer App's daemon push token fail as GitHub
// does while the App lacks the Workflows permission.
func withoutWorkflows(h *harness) {
	id := h.ids[fixerAppName]
	id.mu.Lock()
	id.pushErr = fmt.Errorf("identity %s: mint a push token: %w", fixerAppName, identity.ErrWorkflowsNotGranted)
	id.mu.Unlock()
}

// A merge of the parent that brings the base's own workflow changes, and
// whose workflow files are all as the base's tip has them, is pushed by
// the daemon with a token of its own that may write workflows (Workflows
// write added, never the pane's gh config dir), released after the push:
// no merge of the base waits for the operator for it.
func TestAMergeThatBringsTheBasesWorkflowsIsPushedWithTheDaemonsOwnToken(t *testing.T) {
	h, mg := mergeHarness(t, withFixerApp)
	fixablePR(h, "zhuravel", nil)
	mg.behind, mg.workflows = true, []string{".github/workflows/ci.yml"}
	id := h.fix(FixPayload{Mode: FixModeMerge})
	h.handle()
	h.wantDone(id)
	_, pushes := mg.made()
	if want := fmt.Sprintf("talkable/talkable:%s:feature:%s:%s", fixParentMerge, fixHead, daemonPushDir); !slices.Equal(pushes, []string{want}) {
		t.Errorf("pushes = %q, want %q", pushes, want)
	}
	if mints, released := h.ids[fixerAppName].daemonPushes(); !slices.Equal(mints, []bool{true}) || released != 1 {
		t.Errorf("daemon push tokens %v, released %d; want one with workflows, released", mints, released)
	}
	if cur := h.pr(2); cur.HeadSHA != fixParentMerge || cur.State == store.PRNeedsAttention {
		t.Errorf("the PR = %s at %s, want the merge and no call", cur.State, cur.HeadSHA)
	}
	if ev, ok := h.fixEvent("fix.parent_merged"); !ok || !strings.Contains(ev.Message, "with Workflows") {
		t.Errorf("fix.parent_merged = %q, %v; want it to say the push took Workflows", ev.Message, ok)
	}
}

// While the App lacks the Workflows permission, GitHub mints no such
// token: the merge is refused with "needs Workflows on the App", nothing
// is pushed, and a merge of the parent the operator asked for waits for
// them under Needs you; a fix run's first cycle goes on without it.
func TestAMergeThatBringsTheBasesWorkflowsNeedsWorkflowsOnTheApp(t *testing.T) {
	t.Run("a merge of the parent", func(t *testing.T) {
		h, mg := mergeHarness(t, withFixerApp, withoutWorkflows)
		fixablePR(h, "zhuravel", nil)
		mg.behind, mg.workflows = true, []string{".github/workflows/ci.yml"}
		id := h.fix(FixPayload{Mode: FixModeMerge})
		h.handle()
		h.wantDone(id)
		if _, pushes := mg.made(); len(pushes) != 0 {
			t.Errorf("pushes = %v, want none", pushes)
		}
		pr := h.wantState(2, store.PRNeedsAttention)
		const want = "fix: magnum did not merge the parent of talkable/talkable#2: the merge of master at c0c0c0c brings its workflow " +
			"changes (1 file): needs Workflows on the App (" + fixerAppLogin + " lacks it); merge it on GitHub (Update branch)"
		if got := deref(pr.LastError); !strings.HasPrefix(got, want) {
			t.Errorf("attention = %q, want %q", got, want)
		}
		if _, ok := h.fixStateOf(2); ok {
			t.Error("the merge's loop row stayed")
		}
	})
	t.Run("a fix run", func(t *testing.T) {
		h, mg := mergeHarness(t, withFixerApp, withoutWorkflows)
		pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
		mg.behind, mg.workflows = true, []string{".github/workflows/ci.yml"}
		fixer(h, mg.fixGit, fixPushed, func(run store.Run) string {
			return fixReport(run.ID, "b2b2b2b Fix the total (review comment 4100000001)", fixOneFixed, "none")
		})
		id := h.fix(FixPayload{})
		h.handle()
		h.wantDone(id)
		if _, pushes := mg.made(); len(pushes) != 0 {
			t.Errorf("pushes = %v, want none of the daemon's", pushes)
		}
		if ev, ok := h.fixEvent("fix.parent_refused"); !ok || !strings.Contains(ev.Message, "needs Workflows on the App") {
			t.Errorf("fix.parent_refused = %q, %v", ev.Message, ok)
		}
		if runs := h.fixRuns(pr.ID); len(runs) != 1 || runs[0].TargetSHA != fixHead {
			t.Errorf("fix runs = %+v, want one on the PR's own head", runs)
		}
	})
}

// A merge whose workflow files are not all the base's (the PR changed
// its own) is never pushed by the bot, whatever the App may write: the
// daemon mints no token, and the operator merges it.
func TestAMergeThatKeepsThePRsOwnWorkflowChangesIsNeverPushedByTheBot(t *testing.T) {
	h, mg := mergeHarness(t, withFixerApp)
	fixablePR(h, "zhuravel", nil)
	mg.behind, mg.workflows, mg.own = true, []string{".github/workflows/ci.yml"}, []string{".github/workflows/own.yml"}
	id := h.fix(FixPayload{Mode: FixModeMerge})
	h.handle()
	h.wantDone(id)
	if _, pushes := mg.made(); len(pushes) != 0 {
		t.Errorf("pushes = %v, want none", pushes)
	}
	if mints, _ := h.ids[fixerAppName].daemonPushes(); len(mints) != 0 {
		t.Errorf("daemon push tokens minted: %v", mints)
	}
	pr := h.wantState(2, store.PRNeedsAttention)
	if got := deref(pr.LastError); !strings.Contains(got, "keeps the PR's own workflow changes (.github/workflows/own.yml)") {
		t.Errorf("attention = %q", got)
	}
}

// handedMerge sets up a merge of the parent that conflicts and whose
// fixer's merge carries the base's workflow files: magnum push hands it
// to magnum (exit 4), so GitHub never sees it from the pane.
func handedMerge(t *testing.T, mods ...func(*harness)) (*harness, *mergeGit) {
	t.Helper()
	h, mg := mergeHarness(t, append([]func(*harness){withFixerApp}, mods...)...)
	fixablePR(h, "zhuravel", nil)
	mg.conflict("app/models/offer.rb")
	mg.workflows = []string{".github/workflows/ci.yml"}
	mg.lostPush = true
	fixer(h, mg.fixGit, fixMergeHead, func(run store.Run) string {
		return fixReport(run.ID, "d3d3d3d Merge c0c0c0c: not run: environment", "0 threads (0 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none")
	})
	return h, mg
}

// The fixer's resolved base merge that magnum push handed to magnum (its
// workflow files are the base's, which the pane's token may not push) is
// pushed by the daemon when the run ends, with its own Workflows token and
// a lease on the head the fixer started from: the merge is on GitHub and
// the loop ends as for any pushed merge.
func TestTheFixersMergeThatMagnumPushHandedOverIsPushedByTheDaemon(t *testing.T) {
	h, mg := handedMerge(t)
	id := h.fix(FixPayload{Mode: FixModeMerge})
	h.handle()
	h.wantDone(id)
	_, pushes := mg.made()
	if want := fmt.Sprintf("talkable/talkable:%s:feature:%s:%s", fixMergeHead, fixHead, daemonPushDir); !slices.Equal(pushes, []string{want}) {
		t.Errorf("pushes = %q, want %q", pushes, want)
	}
	if mints, released := h.ids[fixerAppName].daemonPushes(); !slices.Equal(mints, []bool{true}) || released != 1 {
		t.Errorf("daemon push tokens %v, released %d", mints, released)
	}
	if cur := h.pr(2); cur.HeadSHA != fixMergeHead {
		t.Errorf("the PR's head = %s, want the fixer's merge", cur.HeadSHA)
	}
	if ev, ok := h.fixEvent("fix.parent_pushed"); !ok || !strings.Contains(ev.Message, "magnum pushed claude-fix's merge of master") {
		t.Errorf("fix.parent_pushed = %q, %v", ev.Message, ok)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the merge's loop row stayed")
	}
}

// Without Workflows on the App, the handed merge is not pushed: the slot
// goes back to the PR's head and the operator merges it.
func TestTheFixersHandedMergeNeedsWorkflowsOnTheApp(t *testing.T) {
	h, mg := handedMerge(t, withoutWorkflows)
	id := h.fix(FixPayload{Mode: FixModeMerge})
	h.handle()
	h.wantDone(id)
	if _, pushes := mg.made(); len(pushes) != 0 {
		t.Errorf("pushes = %v, want none", pushes)
	}
	pr := h.wantState(2, store.PRNeedsAttention)
	if got := deref(pr.LastError); !strings.Contains(got, "needs Workflows on the App") || !strings.Contains(got, "merge it on GitHub (Update branch)") {
		t.Errorf("attention = %q", got)
	}
	if cur := h.slot("review1"); deref(cur.CheckedOutSHA) != fixHead {
		t.Errorf("slot review1 holds %s, want the PR's head back", deref(cur.CheckedOutSHA))
	}
}
