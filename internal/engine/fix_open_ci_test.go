package engine

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
)

// requiredCompletion makes GitHub's rulesets of talkable/talkable require
// Completion, a check of its CI workflow.
func requiredCompletion(h *harness) {
	h.gh.required = map[string][]string{"talkable/talkable": {"Completion"}}
}

// othersPassed opens PR #2 at head with the checks of the other apps
// passed and no run of CI: the rollup is SUCCESS.
func (h *harness) othersPassed(head string) {
	h.t.Helper()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: head, author: "zhuravel", ci: "SUCCESS",
		checks: []github.Check{ciCheck("Pronto", "pronto", github.CheckPassed), ciCheck("", "Devin Review", github.CheckPassed)}})
}

// The live case: an own PR whose repository requires Completion, and whose
// CI workflow runs only when a PR opens. GitHub lists the other apps'
// checks at the head, all passed, and the rollup says SUCCESS; the look
// called the PR idle ("nothing open") while GitHub blocked its merge. Once
// the head is older than settle_max it now says that the required check
// did not run there.
func TestTheLookNeverCallsAHeadIdleWhoseRequiredCheckDidNotRun(t *testing.T) {
	h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"), requiredCompletion)
	h.othersPassed(fixHead)
	h.tick() // reviewed clean
	h.pastSettle()
	h.tick()
	h.tick()
	h.wantOpen(OpenNeedsYou, "required check Completion did not run at a1a1a1a: run its workflow on the PR's branch, or push")
	if calls := h.ghCalls("dispatch:"); len(calls) != 0 {
		t.Errorf("dispatched %q without ci_dispatch", calls)
	}
}

// ciDispatched is a harness for the look's start of the CI workflow:
// requiredCompletion, [[repo]] ci_dispatch = "ci.yml", and a head whose
// files are paths (Details' list).
func ciDispatched(t *testing.T, paths ...string) *harness {
	t.Helper()
	h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"), requiredCompletion, func(h *harness) {
		h.cfg.Repos = append(h.cfg.Repos, config.Repo{Repo: "talkable/talkable", CIDispatch: "ci.yml"})
		h.gh.defaultFiles = paths
	})
	h.othersPassed(fixHead)
	h.tick() // reviewed clean
	return h
}

// dispatchData is the data of PR #2's newest event of kind.
func (h *harness) dispatchData(kind string) map[string]any {
	h.t.Helper()
	ev, ok := h.fixEvent(kind)
	if !ok {
		h.t.Fatalf("no %s event", kind)
	}
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		h.t.Fatal(err)
	}
	return data
}

// With ci_dispatch, the look starts the workflow that produces the
// required check on the PR's branch (as the watch's poll identity) once the
// check did not run at the head past settle_max, records it
// (fix.open_ci_dispatch) and waits for it: never twice for the head, and
// once settle_max passed again with no run, the PR needs you.
func TestTheLookStartsTheCIWorkflowOnceForAHeadWhoseRequiredCheckDidNotRun(t *testing.T) {
	h := ciDispatched(t, "app/models/order.rb")
	h.pastSettle()
	h.tick()
	h.tick()
	want := []string{"dispatch:talkable/talkable:ci.yml@feature"}
	if calls := h.ghCalls("dispatch:"); !slices.Equal(calls, want) {
		t.Fatalf("dispatches = %q, want %q", calls, want)
	}
	h.wantOpen(OpenWaits, "CI at a1a1a1a: magnum started ci.yml at ")
	data := h.dispatchData("fix.open_ci_dispatch")
	if data["head"] != fixHead || data["workflow"] != "ci.yml" {
		t.Errorf("fix.open_ci_dispatch data = %v", data)
	}
	h.advance(10 * time.Minute)
	h.tick()
	h.wantOpen(OpenWaits, "CI at a1a1a1a: magnum started ci.yml at ")
	h.advance(11 * time.Minute)
	h.tick()
	h.wantOpen(OpenNeedsYou, "required check Completion did not run at a1a1a1a: magnum started ci.yml at ")
	if calls := h.ghCalls("dispatch:"); !slices.Equal(calls, want) {
		t.Fatalf("dispatches = %q, want one for the head", calls)
	}
}

// The run magnum started fills the check in: it runs (CI at the head
// waits), then passes, and the PR is idle.
func TestTheRunOfTheStartedCIWorkflowEndsTheWait(t *testing.T) {
	h := ciDispatched(t, "app/models/order.rb")
	h.pastSettle()
	h.tick()
	h.tick()
	checks := []github.Check{ciCheck("Pronto", "pronto", github.CheckPassed), ciCheck("CI", "Completion", github.CheckPending)}
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead, author: "zhuravel", ci: "PENDING", checks: checks})
	h.tick()
	h.tick()
	h.wantOpen(OpenWaits, "CI at a1a1a1a")
	checks[1].State = github.CheckPassed
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead, author: "zhuravel", ci: "SUCCESS", checks: checks})
	h.tick()
	h.tick()
	h.wantOpen(OpenIdle, "nothing open at a1a1a1a")
	if n := len(h.ghCalls("dispatch:")); n != 1 {
		t.Fatalf("dispatches = %d, want 1", n)
	}
}

// A PR that changes its own workflows or actions would run its own CI with
// the repository's secrets: magnum starts nothing, says why once
// (fix.open_ci_dispatch_refused), and the PR needs you.
func TestTheLookNeverStartsTheCIWorkflowOfAPRThatChangesItsWorkflows(t *testing.T) {
	h := ciDispatched(t, "app/models/order.rb", ".github/workflows/ci.yml")
	h.pastSettle()
	h.tick()
	h.tick()
	if calls := h.ghCalls("dispatch:"); len(calls) != 0 {
		t.Fatalf("dispatched %q for a PR that changes its workflows", calls)
	}
	h.wantOpen(OpenNeedsYou, "required check Completion did not run at a1a1a1a: magnum does not start ci.yml (the PR changes its own workflows or actions)")
	if evs := h.fixEvents("fix.open_ci_dispatch_refused"); len(evs) != 1 {
		t.Fatalf("fix.open_ci_dispatch_refused events = %+v, want one", evs)
	}
}

// A start GitHub refuses (a workflow without a workflow_dispatch trigger
// at the branch: HTTP 422) is recorded (fix.open_ci_dispatch_failed, with
// the status and never GitHub's message, which can name the branch), never
// tried again for the head, and the PR needs you.
func TestAFailedStartOfTheCIWorkflowNeedsYouAndIsNotRepeated(t *testing.T) {
	h := ciDispatched(t, "app/models/order.rb")
	h.gh.mu.Lock()
	h.gh.dispatchErr = &github.APIError{Op: "dispatch", Status: 422, Message: "Workflow does not have 'workflow_dispatch' trigger"}
	h.gh.mu.Unlock()
	h.pastSettle()
	h.tick()
	h.tick()
	h.advance(10 * time.Minute)
	h.tick()
	if n := len(h.ghCalls("dispatch:")); n != 1 {
		t.Fatalf("dispatches = %d, want one for the head", n)
	}
	h.wantOpen(OpenNeedsYou, "required check Completion did not run at a1a1a1a: magnum's start of ci.yml failed (GitHub answered HTTP 422)")
	ev, ok := h.fixEvent("fix.open_ci_dispatch_failed")
	if !ok || ev.Level != "warn" || !strings.Contains(ev.Message, "HTTP 422") || strings.Contains(ev.Message, "workflow_dispatch") {
		t.Fatalf("fix.open_ci_dispatch_failed = %+v, %v", ev, ok)
	}
}

// The start waits for the end of `magnum pause` and for the GitHub budget
// of the identity it starts as; without either, the verdict asks the look
// to start it.
func TestTheStartOfTheCIWorkflowWaitsForThePauseAndTheBudget(t *testing.T) {
	f := idleFacts()
	f.ci = openCI{missing: []string{"Completion"}}
	f.ciWorkflow = "ci.yml"
	v, _, _ := decideOpen(f)
	if !v.dispatch || v.Verdict != OpenNeedsYou {
		t.Fatalf("verdict = %+v, want the start asked for", v)
	}
	f.pause, f.pauseUntil = "daemon paused", openNow.Add(time.Hour)
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenWaits, "CI at a1a1a1a: required check Completion did not run; magnum starts ci.yml once the pause ends")
	if v.dispatch || !v.Until.Equal(f.pauseUntil) {
		t.Errorf("dispatch %v, until %s under the pause", v.dispatch, v.Until)
	}
	f.pause, f.pauseUntil, f.low = "", time.Time{}, true
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenWaits, "CI at a1a1a1a: required check Completion did not run; magnum starts ci.yml once the GitHub budget allows")
	if v.dispatch {
		t.Error("asked to start it below the GitHub floor")
	}
}
