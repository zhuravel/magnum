package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
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

// othersChecks are the checks of the other apps, passed: the rollup of a
// head with no run of CI is SUCCESS.
func othersChecks() []github.Check {
	return []github.Check{ciCheck("Pronto", "pronto", github.CheckPassed), ciCheck("", "Devin Review", github.CheckPassed)}
}

// openOwn has GitHub list the operator's PR #2 as spec, into main (the
// default branch, whose required checks GitHub's list is) unless spec
// names its base, beside the baseline.
func (h *harness) openOwn(spec prSpec) {
	h.t.Helper()
	spec.n, spec.author, spec.base = 2, "zhuravel", cmp.Or(spec.base, "main")
	h.open(prSpec{n: 1, head: "base1"}, spec)
}

// othersPassed opens PR #2 at head with the checks of the other apps
// passed and no run of CI: the rollup is SUCCESS.
func (h *harness) othersPassed(head string) {
	h.t.Helper()
	h.openOwn(prSpec{head: head, ci: "SUCCESS", checks: othersChecks()})
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
// files are paths (Details' list), at which the other apps' checks passed.
func ciDispatched(t *testing.T, paths ...string) *harness {
	t.Helper()
	return ciDispatchedAt(t, prSpec{head: fixHead, ci: "SUCCESS", checks: othersChecks()}, paths...)
}

// ciDispatchedAt is ciDispatched with the operator's PR #2 opened as spec
// (openOwn), reviewed clean.
func ciDispatchedAt(t *testing.T, spec prSpec, paths ...string) *harness {
	t.Helper()
	h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"), requiredCompletion, func(h *harness) {
		h.cfg.Repos = append(h.cfg.Repos, config.Repo{Repo: "talkable/talkable", CIDispatch: "ci.yml"})
		h.gh.defaultFiles = paths
	})
	h.openOwn(spec)
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

// detailsReads counts the fake GitHub's Details reads that asked for PR #n.
func (h *harness) detailsReads(n int) int {
	h.t.Helper()
	reads := 0
	for _, c := range h.ghCalls("details:talkable/talkable:") {
		list := strings.TrimSuffix(c[strings.LastIndexByte(c, '[')+1:], "]")
		if slices.Contains(strings.Fields(list), strconv.Itoa(n)) {
			reads++
		}
	}
	return reads
}

// polls advances the clock a minute and ticks, n times, and returns how
// many Details reads asked for PR #2 meanwhile, and how many reads of the
// CI workflow's dispatched runs there were.
func (h *harness) polls(n int) (details, runs int) {
	h.t.Helper()
	before, runsBefore := h.detailsReads(2), len(h.ghCalls("runs:"))
	for range n {
		h.advance(time.Minute)
		h.tick()
	}
	return h.detailsReads(2) - before, len(h.ghCalls("runs:")) - runsBefore
}

// setRuns makes the fake GitHub list runs, newest first, as the runs a
// workflow_dispatch started at sha.
func (h *harness) setRuns(sha string, runs ...github.WorkflowRun) {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	if h.gh.runs == nil {
		h.gh.runs = map[string][]github.WorkflowRun{}
	}
	h.gh.runs[sha] = runs
}

// The live case of 2026-11-08: GitHub's rollup of the head left out the
// run of the workflow magnum started (its workflow_dispatch check suite),
// and the run's Completion check came between two polls, so neither the
// rollup's state nor its counts moved, and nothing moved the PR's
// updatedAt: the poll never read the head's checks again, and settle_max
// after the start the look said that no run came while Completion had
// passed. While a start waits for its run, each poll reads the head's
// checks: the look sees Completion pass, the PR is idle, and the reads end
// once the head's checks show the run.
func TestTheRunOfAStartedCIWorkflowIsSeenThoughTheRollupLeavesItOut(t *testing.T) {
	h := ciDispatched(t, "app/models/order.rb")
	h.pastSettle()
	h.tick()
	h.tick()
	h.wantOpen(OpenWaits, "CI at a1a1a1a: magnum started ci.yml at ")
	others := othersChecks()
	h.setCounts(2, fakeCounts(others)) // the rollup as before the start: the run is not in it
	h.setCI(2, "SUCCESS", append(others, ciCheck("CI", "Completion", github.CheckPassed))...)
	h.advance(5 * time.Minute)
	h.tick()
	h.tick()
	h.wantOpen(OpenIdle, "nothing open at a1a1a1a")
	if n, runs := h.polls(3); n != 0 || runs != 0 {
		t.Errorf("details of #2 read %d times and runs %d times in 3 polls after the run was seen, want none", n, runs)
	}
	h.advance(20 * time.Minute)
	h.tick()
	h.wantOpen(OpenIdle, "nothing open at a1a1a1a")
	if n := len(h.ghCalls("dispatch:")); n != 1 {
		t.Fatalf("dispatches = %d, want 1", n)
	}
}

// While a start of the CI workflow waits for its run, each poll reads the
// head's checks and the workflow's dispatched runs at the head once; with
// no run the look waits, settle_max after the start it needs you as before
// (the head is not started again), and the reads end with the wait. A run
// GitHub created before the start (an earlier run of the head, cancelled)
// is not the start's.
func TestAStartWithNoRunReadsTheHeadsChecksOncePerPollUntilItsWaitEnds(t *testing.T) {
	h := ciDispatched(t, "app/models/order.rb")
	h.pastSettle()
	h.tick()
	h.tick()
	h.setRuns(fixHead, github.WorkflowRun{Status: "completed", Conclusion: "cancelled", CreatedAt: h.clock.Now().Add(-2 * time.Hour)})
	if n, runs := h.polls(3); n != 3 || runs != 3 {
		t.Fatalf("details of #2 read %d times and runs %d times in 3 polls of the wait, want 3 each", n, runs)
	}
	h.wantOpen(OpenWaits, "CI at a1a1a1a: magnum started ci.yml at ")
	h.advance(20 * time.Minute)
	h.tick()
	v := h.wantOpen(OpenNeedsYou, "required check Completion did not run at a1a1a1a: magnum started ci.yml at ")
	if !strings.HasSuffix(v.Why, " and no run came; run it on the PR's branch") {
		t.Errorf("why = %q, want no run came", v.Why)
	}
	if n, runs := h.polls(3); n != 0 || runs != 0 {
		t.Errorf("details of #2 read %d times and runs %d times in 3 polls after the wait, want none", n, runs)
	}
	if n := len(h.ghCalls("dispatch:")); n != 1 {
		t.Fatalf("dispatches = %d, want 1", n)
	}
}

// A run magnum started shows nothing in GitHub's rollup of the head while
// it waits for a runner or runs (its check suite is left out, and its
// Completion is created passed when it ends), so one that ran past
// settle_max read as no run, and the PR needed you to start a second. The
// poll reads the workflow's runs that a dispatch started at the head: a
// queued or running one keeps the look waiting past settle_max, the poll
// that sees it end reads the head's checks, so its Completion makes the PR
// idle at once, and the reads end there.
func TestARunOfTheStartedCIWorkflowThatOutlastsTheWaitKeepsTheLookWaiting(t *testing.T) {
	h := ciDispatched(t, "app/models/order.rb")
	h.pastSettle()
	h.tick()
	h.tick()
	h.wantOpen(OpenWaits, "CI at a1a1a1a: magnum started ci.yml at ")
	created := h.clock.Now().Add(2 * time.Second)
	h.setRuns(fixHead, github.WorkflowRun{Status: "queued", CreatedAt: created})
	h.advance(15 * time.Minute)
	h.tick()
	h.setRuns(fixHead, github.WorkflowRun{Status: "in_progress", CreatedAt: created})
	h.advance(10 * time.Minute) // past settle_max after the start
	h.tick()
	h.tick()
	h.wantOpen(OpenWaits, "CI at a1a1a1a: magnum started ci.yml at ")
	others := othersChecks()
	h.setCounts(2, fakeCounts(others)) // the run's Completion moves none of the rollup's counts
	h.setCI(2, "SUCCESS", append(others, ciCheck("CI", "Completion", github.CheckPassed))...)
	h.setRuns(fixHead, github.WorkflowRun{Status: "completed", Conclusion: "success", CreatedAt: created})
	h.advance(time.Minute)
	h.tick()
	h.wantOpen(OpenIdle, "nothing open at a1a1a1a")
	if n, runs := h.polls(3); n != 0 || runs != 0 {
		t.Errorf("details of #2 read %d times and runs %d times in 3 polls after the run ended, want none", n, runs)
	}
	if n := len(h.ghCalls("dispatch:")); n != 1 {
		t.Fatalf("dispatches = %d, want 1", n)
	}
}

// A run of the started workflow that ended with no run of the required
// check at the head as GitHub's rollup lists it (a check that is a job of
// that workflow, whose check suite the rollup leaves out, or a run
// cancelled before its last job) is no "no run came": settle_max after the
// start the PR needs you with how the run ended, and the reads end.
func TestARunOfTheStartedCIWorkflowThatEndedWithoutTheCheckNeedsYou(t *testing.T) {
	h := ciDispatched(t, "app/models/order.rb")
	h.pastSettle()
	h.tick()
	h.tick()
	h.setRuns(fixHead, github.WorkflowRun{Status: "completed", Conclusion: "timed_out", CreatedAt: h.clock.Now().Add(time.Second)})
	h.advance(5 * time.Minute)
	h.tick()
	h.wantOpen(OpenWaits, "CI at a1a1a1a: magnum started ci.yml at ")
	h.advance(16 * time.Minute)
	h.tick()
	v := h.wantOpen(OpenNeedsYou, "required check Completion did not run at a1a1a1a: magnum started ci.yml at ")
	if !strings.HasSuffix(v.Why, " and its run ended (timed out) without it; run it on the PR's branch") {
		t.Errorf("why = %q, want the run's end", v.Why)
	}
	if n, runs := h.polls(3); n != 0 || runs != 0 {
		t.Errorf("details of #2 read %d times and runs %d times in 3 polls after the wait, want none", n, runs)
	}
	if n := len(h.ghCalls("dispatch:")); n != 1 {
		t.Fatalf("dispatches = %d, want 1", n)
	}
}

// Only a start that waits for its run reads the head's checks and the
// workflow's dispatched runs on every poll: an own PR of a repository with
// ci_dispatch whose required check has not started yet (no start), and one
// whose start GitHub refused, get neither read while nothing of theirs
// moves.
func TestOnlyAStartThatWaitsReadsTheHeadsChecksOnEveryPoll(t *testing.T) {
	h := ciDispatched(t, "app/models/order.rb")
	if n, runs := h.polls(3); n != 0 || runs != 0 {
		t.Errorf("no start: details of #2 read %d times and runs %d times in 3 polls, want none", n, runs)
	}
	h.wantOpen(OpenWaits, "CI at a1a1a1a: required check Completion has not started")
	h.gh.mu.Lock()
	h.gh.dispatchErr = &github.APIError{Op: "dispatch", Status: 422}
	h.gh.mu.Unlock()
	h.pastSettle()
	h.tick()
	h.tick()
	h.wantOpen(OpenNeedsYou, "magnum's start of ci.yml failed (GitHub answered HTTP 422)")
	if n, runs := h.polls(3); n != 0 || runs != 0 {
		t.Errorf("a failed start: details of #2 read %d times and runs %d times in 3 polls, want none", n, runs)
	}
}

// The live PR already said that no run came when the run was there: the
// rollup had moved by the checks the run adds to an older check suite of
// the head (Completion and its test reports, created passed: no pending
// count, the state SUCCESS throughout). A rollup whose passed count moved
// reads the head's checks again, so the look, with no operator action,
// calls the PR idle.
func TestAPassedCheckThatCameAfterTheWaitClearsNoRunCame(t *testing.T) {
	h := ciDispatched(t, "app/models/order.rb")
	h.pastSettle()
	h.tick()
	h.tick()
	h.advance(21 * time.Minute)
	h.tick()
	h.wantOpen(OpenNeedsYou, "required check Completion did not run at a1a1a1a: magnum started ci.yml at ")
	h.setCI(2, "SUCCESS", append(othersChecks(), ciCheck("CI", "Completion", github.CheckPassed))...)
	h.advance(time.Minute)
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
	if data := h.dispatchData("fix.open_ci_dispatch_refused"); data["identity"] != "zhuravel" {
		t.Errorf("fix.open_ci_dispatch_refused data = %v, want it to name the identity it would start as", data)
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

// A required check its workflow creates only when the jobs it needs end
// (Completion `needs:` the other CI jobs) has no run at the head while that
// workflow runs, however long it takes: the look waits for it as CI at the
// head, past settle_max too, never says that it did not run and starts
// nothing; once it ran and passed the PR is idle.
func TestTheLookWaitsForARequiredCheckWhileTheHeadsChecksRun(t *testing.T) {
	passed, pending := github.CheckPassed, github.CheckPending
	h := ciDispatched(t, "app/models/order.rb")
	checks := append(othersChecks(), ciCheck("CI", "RSpec (3)", pending), ciCheck("CI", "Checks", passed))
	h.openOwn(prSpec{head: fixHead, ci: "PENDING", checks: checks})
	h.pastSettle()
	h.tick()
	h.tick()
	if calls := h.ghCalls("dispatch:"); len(calls) != 0 {
		t.Fatalf("dispatched %q while the head's CI ran", calls)
	}
	h.wantOpen(OpenWaits, "CI at a1a1a1a: required check Completion has not started")
	h.advance(30 * time.Minute) // a slow run
	h.tick()
	h.wantOpen(OpenWaits, "CI at a1a1a1a: required check Completion has not started")

	checks[2].State = passed
	checks = append(checks, ciCheck("CI", "Completion", pending))
	h.openOwn(prSpec{head: fixHead, ci: "PENDING", checks: checks})
	h.tick()
	h.tick()
	h.wantOpen(OpenWaits, "CI at a1a1a1a")
	checks[4].State = passed
	h.openOwn(prSpec{head: fixHead, ci: "SUCCESS", checks: checks})
	h.tick()
	h.tick()
	h.wantOpen(OpenIdle, "nothing open at a1a1a1a")
	if calls := h.ghCalls("dispatch:"); len(calls) != 0 {
		t.Fatalf("dispatched %q", calls)
	}
}

// GitHub's list is the default branch's: a PR stacked on another branch
// merges without its checks, so one with no run at such a PR's head leaves
// rule 11 to the rollup, as before, and magnum starts nothing for it.
func TestTheLookLeavesTheRequiredChecksOfAStackedPRToTheRollup(t *testing.T) {
	h := ciDispatchedAt(t, prSpec{head: fixHead, base: "feature-a", ci: "SUCCESS", checks: othersChecks()}, "app/models/order.rb")
	h.pastSettle()
	h.tick()
	h.tick()
	h.wantOpen(OpenIdle, "nothing open at a1a1a1a")
	if calls := h.ghCalls("dispatch:"); len(calls) != 0 {
		t.Fatalf("dispatched %q for a stacked PR", calls)
	}
}

// A [[repo]] required_checks list GitHub does not enforce may name a
// workflow whose path filters skip a head (an infra-only PR gets only its
// plan checks, and GitHub merges it): without ci_dispatch an entry with no
// run at the head leaves rule 11 to the rollup, as before.
func TestTheLookLeavesAConfiguredCheckWithNoRunToTheRollupWithoutCIDispatch(t *testing.T) {
	h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"), withRequiredChecks("workflow:CI"))
	h.openOwn(prSpec{head: fixHead, ci: "SUCCESS", checks: []github.Check{ciCheck("Infra", "infra-plan", github.CheckPassed)}})
	h.tick() // reviewed clean
	h.pastSettle()
	h.tick()
	h.tick()
	h.wantOpen(OpenIdle, "nothing open at a1a1a1a")
}

// A required check ignore_checks names ran at the head though magnum does
// not count it: it is never missing, whether GitHub's rulesets require it
// or a [[repo]] required_checks list of a repository that sets ci_dispatch
// names it, and the PR is idle once the rest passed.
func TestARequiredCheckIgnoreChecksNamesIsNotMissingWhereItRan(t *testing.T) {
	for name, mod := range map[string]func(*harness){
		"GitHub's rulesets": func(h *harness) { h.gh.required = map[string][]string{"talkable/talkable": {"Vendor Gates"}} },
		"required_checks":   withRequiredDispatch("Vendor Gates"),
	} {
		t.Run(name, func(t *testing.T) {
			h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"), mod, func(h *harness) {
				h.cfg.Watches[0].IgnoreChecks = []string{"Vendor Gates"}
			})
			h.openOwn(prSpec{head: fixHead, ci: "SUCCESS", checks: append(othersChecks(), vendorCheck(github.CheckPassed))})
			h.tick() // reviewed clean
			h.pastSettle()
			h.tick()
			h.tick()
			h.wantOpen(OpenIdle, "nothing open at a1a1a1a")
			if calls := h.ghCalls("dispatch:"); len(calls) != 0 {
				t.Fatalf("dispatched %q for a check that ran", calls)
			}
		})
	}
}

// The start reads the head's own file list: a stored list of an older head
// (the poll has not listed the new head's yet, and that head may add a
// workflow) starts nothing, and the look starts it once the head's list is
// stored. A new head after a recorded start is started again, once: the
// start of an older head is not this head's.
func TestTheLookStartsTheCIWorkflowOncePerHeadByThatHeadsFiles(t *testing.T) {
	h := ciDispatched(t, "app/models/order.rb")
	h.pastSettle()
	h.tick()
	h.tick()
	if n := len(h.ghCalls("dispatch:")); n != 1 {
		t.Fatalf("dispatches = %d, want one for a1a1a1a", n)
	}
	pr := h.pr(2)
	pr.HeadSHA = fixPushed // pushed, its files not listed yet
	if s, ok := h.e.openStartCI(h.ctx, h.repo(), h.cfg.Watches[0], pr, "ci.yml", "required check Completion"); ok || s != (ciStart{}) {
		t.Fatalf("start = %+v, %v with the files of the older head", s, ok)
	}
	if n := len(h.ghCalls("dispatch:")); n != 1 {
		t.Fatalf("dispatches = %d with the files of the older head", n)
	}

	h.othersPassed(fixPushed)
	h.tick() // reviewed clean
	h.pastSettle()
	h.tick()
	h.tick()
	want := []string{"dispatch:talkable/talkable:ci.yml@feature", "dispatch:talkable/talkable:ci.yml@feature"}
	if calls := h.ghCalls("dispatch:"); !slices.Equal(calls, want) {
		t.Fatalf("dispatches = %q, want one for each head", calls)
	}
	h.wantOpen(OpenWaits, "CI at b2b2b2b: magnum started ci.yml at ")
	h.tick()
	if n := len(h.ghCalls("dispatch:")); n != 2 {
		t.Fatalf("dispatches = %d, want one for each head", n)
	}
}

// magnum starts the workflow only on a PR whose files GitHub listed whole
// (a file past the list may change a workflow) and whose branch it knows:
// it refuses any other once for the head (fix.open_ci_dispatch_refused),
// and the PR needs you.
func TestTheLookRefusesToStartTheCIWorkflowOfAHeadItCannotCheck(t *testing.T) {
	t.Run("files GitHub did not list whole", func(t *testing.T) {
		// a head of its own: the registry keeps the first list of a head
		h := ciDispatchedAt(t, prSpec{head: fixPushed, ci: "SUCCESS", checks: othersChecks(), filesTruncated: true}, "app/models/order.rb")
		h.pastSettle()
		h.tick()
		h.tick()
		if calls := h.ghCalls("dispatch:"); len(calls) != 0 {
			t.Fatalf("dispatched %q", calls)
		}
		h.wantOpen(OpenNeedsYou, "did not run at b2b2b2b: magnum does not start ci.yml (GitHub did not list every file of the PR)")
		if evs := h.fixEvents("fix.open_ci_dispatch_refused"); len(evs) != 1 {
			t.Fatalf("fix.open_ci_dispatch_refused events = %+v, want one", evs)
		}
	})
	t.Run("a branch magnum does not know", func(t *testing.T) {
		h := ciDispatched(t, "app/models/order.rb")
		pr := h.pr(2)
		pr.HeadRef = nil
		s, ok := h.e.openStartCI(h.ctx, h.repo(), h.cfg.Watches[0], pr, "ci.yml", "required check Completion")
		if !ok || s.Refused != "the PR's branch is not known" || !s.At.IsZero() {
			t.Fatalf("start = %+v, %v; want refused", s, ok)
		}
		if calls := h.ghCalls("dispatch:"); len(calls) != 0 {
			t.Fatalf("dispatched %q", calls)
		}
		if evs := h.fixEvents("fix.open_ci_dispatch_refused"); len(evs) != 1 {
			t.Fatalf("fix.open_ci_dispatch_refused events = %+v, want one", evs)
		}
	})
}

// appDispatcher is talkable-app's client in the look's CI tests: the poll
// identity's fake for every call but the start of a workflow, which it
// records as "dispatch as talkable-app:<owner>/<repo>:<workflow>@<ref>" and
// fails with the fake's dispatchErr.
type appDispatcher struct{ *fakeGH }

func (g appDispatcher) DispatchWorkflow(_ context.Context, owner, repo, workflow, ref string) error {
	g.record(fmt.Sprintf("dispatch as talkable-app:%s/%s:%s@%s", owner, repo, workflow, ref))
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.dispatchErr
}

// ciDispatchedAs is ciDispatched with talkable-app's client in place
// (appDispatcher) and [[repo]] ci_dispatch_as = as ("" leaves it unset).
func ciDispatchedAs(t *testing.T, as string) *harness {
	t.Helper()
	h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"), requiredCompletion, func(h *harness) {
		h.cfg.Repos = append(h.cfg.Repos, config.Repo{Repo: "talkable/talkable", CIDispatch: "ci.yml", CIDispatchAs: as})
		h.gh.defaultFiles = []string{"app/models/order.rb"}
		plain := h.d.GitHub
		h.d.GitHub = func(id string) GitHub {
			if id == "talkable-app" {
				return appDispatcher{h.gh}
			}
			return plain(id)
		}
	})
	h.othersPassed(fixHead)
	h.tick() // reviewed clean
	return h
}

// The live case: the watch's poll identity is an App installation that
// may only read Actions, so GitHub refused its start of the workflow.
// [[repo]] ci_dispatch_as names the identity that starts it: the look
// starts it through that identity's client, and fix.open_ci_dispatch names
// it. Unset, the look starts it as the poll identity, as before.
func TestTheLookStartsTheCIWorkflowAsCIDispatchAs(t *testing.T) {
	for _, c := range []struct{ as, call, identity string }{
		{"talkable-app", "dispatch as talkable-app:talkable/talkable:ci.yml@feature", "talkable-app"},
		{"", "dispatch:talkable/talkable:ci.yml@feature", "zhuravel"},
	} {
		t.Run(cmp.Or(c.as, "unset"), func(t *testing.T) {
			h := ciDispatchedAs(t, c.as)
			h.pastSettle()
			h.tick()
			h.tick()
			if calls := h.ghCalls("dispatch"); !slices.Equal(calls, []string{c.call}) {
				t.Fatalf("dispatches = %q, want %q", calls, c.call)
			}
			h.wantOpen(OpenWaits, "CI at a1a1a1a: magnum started ci.yml at ")
			ev, _ := h.fixEvent("fix.open_ci_dispatch")
			if data := h.dispatchData("fix.open_ci_dispatch"); data["identity"] != c.identity || !strings.HasSuffix(ev.Message, " as "+c.identity) {
				t.Errorf("fix.open_ci_dispatch = %q, %v; want it to name %s", ev.Message, data, c.identity)
			}
		})
	}
}

// The start waits for the GitHub budget of the identity it starts as:
// with ci_dispatch_as below the floor the look starts nothing and says it
// waits for the budget, and it starts the workflow once the budget is back.
func TestTheStartOfTheCIWorkflowWaitsForTheBudgetOfCIDispatchAs(t *testing.T) {
	h := ciDispatchedAs(t, "talkable-app")
	h.pastSettle()
	until := h.lowBudget("talkable-app", 100, h.clock.Now().Add(20*time.Minute))
	h.tick()
	h.tick()
	if calls := h.ghCalls("dispatch"); len(calls) != 0 {
		t.Fatalf("dispatched %q below the floor of ci_dispatch_as", calls)
	}
	h.wantOpen(OpenWaits, "CI at a1a1a1a: required check Completion did not run; magnum starts ci.yml once the GitHub budget allows")
	h.advance(until.Sub(h.clock.Now()) + time.Minute)
	h.tick()
	h.tick()
	if calls := h.ghCalls("dispatch"); !slices.Equal(calls, []string{"dispatch as talkable-app:talkable/talkable:ci.yml@feature"}) {
		t.Fatalf("dispatches = %q once the budget is back", calls)
	}
}

// GitHub refuses a start (HTTP 403) to an identity that may not start
// workflows: an App with Actions: read only, a gh login without push
// access. The verdict and fix.open_ci_dispatch_failed say that the identity
// cannot start workflows, what it needs (by its kind), and that
// ci_dispatch_as names one that can, never GitHub's message; the PR needs
// you exactly as after any refused start (the verdict's needs and its end,
// the PR's state and its wait), and the head is not started again.
func TestAStartGitHubForbidsSaysTheIdentityCannotStartWorkflows(t *testing.T) {
	failed := func(t *testing.T, as string, err error) *harness {
		t.Helper()
		h := ciDispatchedAs(t, as)
		h.gh.mu.Lock()
		h.gh.dispatchErr = err
		h.gh.mu.Unlock()
		h.pastSettle()
		h.tick()
		h.tick()
		h.advance(10 * time.Minute)
		h.tick()
		if n := len(h.ghCalls("dispatch")); n != 1 {
			t.Fatalf("dispatches = %d, want one for the head", n)
		}
		return h
	}
	unprocessable := failed(t, "talkable-app", &github.APIError{Op: "dispatch", Status: 422, Message: "Workflow does not have 'workflow_dispatch' trigger"})
	before := unprocessable.wantOpen(OpenNeedsYou, "magnum's start of ci.yml failed (GitHub answered HTTP 422)")
	beforeWait, beforeWaits := unprocessable.waitOf(2)
	for _, c := range []struct{ as, identity, words string }{
		{"talkable-app", "talkable-app", "talkable-app cannot start workflows, an App needs Actions: write"},
		{"", "zhuravel", "zhuravel cannot start workflows, a gh login needs push access and the repo scope"},
	} {
		t.Run(c.identity, func(t *testing.T) {
			h := failed(t, c.as, &github.APIError{Op: "dispatch", Status: 403, Message: "Resource not accessible by integration"})
			fix := "; set [[repo]] ci_dispatch_as to an identity that can"
			v := h.wantOpen(OpenNeedsYou, "required check Completion did not run at a1a1a1a: magnum's start of ci.yml failed (GitHub answered HTTP 403: "+c.words+fix+"); run it on the PR's branch")
			if !slices.Equal(v.Needs, before.Needs) || !v.Until.Equal(before.Until) || v.Head != before.Head {
				t.Errorf("verdict = %+v, want it as after the 422's %+v", v, before)
			}
			if w, ok := h.waitOf(2); ok != beforeWaits || w != beforeWait {
				t.Errorf("wait = %+v, %v; want %+v, %v as after the 422", w, ok, beforeWait, beforeWaits)
			}
			if got, want := h.pr(2).State, unprocessable.pr(2).State; got != want {
				t.Errorf("PR state = %s, want %s as after the 422", got, want)
			}
			ev, ok := h.fixEvent("fix.open_ci_dispatch_failed")
			if !ok || ev.Level != "warn" || !strings.Contains(ev.Message, c.words+fix) || strings.Contains(ev.Message, "Resource not accessible") {
				t.Fatalf("fix.open_ci_dispatch_failed = %+v, %v", ev, ok)
			}
			if data := h.dispatchData("fix.open_ci_dispatch_failed"); data["identity"] != c.identity {
				t.Errorf("fix.open_ci_dispatch_failed data = %v, want it to name %s", data, c.identity)
			}
		})
	}
	// A 403 for GitHub's rate limit says nothing of the identity's right
	// to start workflows: the budget ran out, and ci_dispatch_as is not
	// the fix.
	t.Run("a rate limit", func(t *testing.T) {
		h := failed(t, "talkable-app", &github.APIError{Op: "dispatch", Status: 403, Message: "API rate limit exceeded for installation"})
		v := h.wantOpen(OpenNeedsYou, "required check Completion did not run at a1a1a1a: magnum's start of ci.yml failed (GitHub answered HTTP 403); run it on the PR's branch")
		ev, ok := h.fixEvent("fix.open_ci_dispatch_failed")
		if !ok || strings.Contains(ev.Message, "cannot start workflows") || strings.Contains(v.Why, "cannot start workflows") {
			t.Errorf("fix.open_ci_dispatch_failed = %+v, %v; verdict %q: a rate limit says the identity cannot start workflows", ev, ok, v.Why)
		}
	})
}
