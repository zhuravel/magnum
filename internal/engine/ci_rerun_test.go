package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// liveRunID is the live case's run of ci.yml that started no job
// (2026-11-09).
const liveRunID = 42673098082

// rerunHarness is the live case of 2026-11-09: GitHub requires build on
// main, the repository sets no ci_dispatch, and the operator's PR #2,
// whose files are paths (one .rst file by default), was reviewed clean.
// The head has no check at all, and GitHub lists no run at it until the
// test says one (setHeadRuns).
func rerunHarness(t *testing.T, paths ...string) *harness {
	t.Helper()
	if len(paths) == 0 {
		paths = []string{"source/partials/ip_list.rst"}
	}
	h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"), func(h *harness) {
		h.gh.required = map[string][]string{"talkable/talkable": {"build"}}
		h.gh.defaultFiles = paths
	})
	h.openOwn(prSpec{head: fixHead})
	h.tick() // reviewed clean
	return h
}

// noJobRun is run liveRunID's first attempt as GitHub listed it: of
// ci.yml for the pull_request, ended "failure" with no job.
func noJobRun() github.WorkflowRun {
	return github.WorkflowRun{ID: liveRunID, Status: "completed", Conclusion: "failure", Attempt: 1, Workflow: "ci.yml",
		Event: "pull_request", HeadSHA: fixHead}
}

// setHeadRuns makes the fake GitHub list runs, newest first, as every
// workflow's runs at sha.
func (h *harness) setHeadRuns(sha string, runs ...github.WorkflowRun) {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	if h.gh.headRuns == nil {
		h.gh.headRuns = map[string][]github.WorkflowRun{}
	}
	h.gh.headRuns[sha] = runs
}

// setJobs makes the fake GitHub list n jobs of run id's latest attempt.
func (h *harness) setJobs(id int64, n int) {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	if h.gh.jobs == nil {
		h.gh.jobs = map[int64][]github.WorkflowJob{}
	}
	h.gh.jobs[id] = nil
	for i := range n {
		h.gh.jobs[id] = append(h.gh.jobs[id], github.WorkflowJob{ID: int64(i + 1), Name: fmt.Sprintf("job%d", i+1), Status: "completed", Conclusion: "failure"})
	}
}

// rule11Words is rule 11's needs-you at the live case's head when no run
// there started without a job: the words of master before the re-run.
const rule11Words = "required check build did not run at a1a1a1a: run its workflow on the PR's branch, or push"

// The live case: run 42673098082 of ci.yml ended "failure" with no job, a
// startup failure of GitHub's, and the PR changes one .rst file. Past
// settle_max, at rule 11's "did not run", the look reads the head's runs
// once, re-runs that run once as startCI's identity (the watch's poll
// identity here), records it before the call (fix.open_ci_rerun) and
// waits for it; the next looks neither read nor re-run again.
func TestARunThatStartedNoJobIsReRunOnceForTheHead(t *testing.T) {
	h := rerunHarness(t)
	h.setHeadRuns(fixHead, noJobRun())
	h.pastSettle()
	h.tick()
	h.tick()
	want := []string{"rerun:talkable/talkable#42673098082"}
	if calls := h.ghCalls("rerun:"); !slices.Equal(calls, want) {
		t.Fatalf("reruns = %q, want %q", calls, want)
	}
	if calls := h.ghCalls("run_jobs:"); len(calls) != 1 {
		t.Errorf("jobs reads = %q, want one: a failure's jobs tell a start that failed", calls)
	}
	h.wantOpen(OpenWaits, "CI at a1a1a1a: magnum re-ran run 42673098082 of ci.yml at ")
	h.wantOpen(OpenWaits, ", as required check build did not run")
	data := h.dispatchData("fix.open_ci_rerun")
	if data["head"] != fixHead || data["run"] != float64(liveRunID) || data["workflow"] != "ci.yml" || data["attempt"] != float64(1) ||
		data["conclusion"] != "failure" || data["identity"] != "zhuravel" {
		t.Errorf("fix.open_ci_rerun data = %v", data)
	}
	ev, _ := h.fixEvent("fix.open_ci_rerun")
	if ev.Message != "required check build did not run at a1a1a1a, as run 42673098082 of ci.yml started no job (failure); magnum re-ran it as zhuravel" {
		t.Errorf("fix.open_ci_rerun = %q", ev.Message)
	}
	reads := len(h.ghCalls("head_runs:"))
	for range 5 {
		h.advance(time.Minute)
		h.tick()
	}
	if calls := h.ghCalls("rerun:"); !slices.Equal(calls, want) {
		t.Fatalf("reruns = %q, want one for the head", calls)
	}
	if n := len(h.ghCalls("head_runs:")) - reads; n > 5 {
		t.Errorf("head runs read %d times in 5 polls, want one a poll at most while the re-run waits", n)
	}
}

// The re-run's new attempt (the same run id, run_attempt 2) runs: its
// build check shows in the rollup, the look waits, and once it passed the
// PR is idle; nothing is re-run again.
func TestTheReRunsNewAttemptEndsTheWait(t *testing.T) {
	h := rerunHarness(t)
	h.setHeadRuns(fixHead, noJobRun())
	h.pastSettle()
	h.tick()
	h.tick()
	run := noJobRun()
	run.Attempt, run.Status, run.Conclusion = 2, "in_progress", ""
	h.setHeadRuns(fixHead, run)
	h.setCI(2, "PENDING", ciCheck("CI", "build", github.CheckPending))
	h.advance(time.Minute)
	h.tick()
	h.wantOpen(OpenWaits, "CI at a1a1a1a")
	run.Status, run.Conclusion = "completed", "success"
	h.setHeadRuns(fixHead, run)
	h.setCI(2, "SUCCESS", ciCheck("CI", "build", github.CheckPassed))
	h.advance(time.Minute)
	h.tick()
	h.tick()
	h.wantOpen(OpenIdle, "nothing open at a1a1a1a")
	if n := len(h.ghCalls("rerun:")); n != 1 {
		t.Fatalf("reruns = %d, want 1", n)
	}
}

// A second start that fails at the head (the re-run's attempt 2 ends
// "failure" with no job) needs you at once, with words that say the run
// failed to start twice and name it; the poll says it once
// (fix.open_ci_rerun_twice) and nothing re-runs it again.
func TestARunThatFailsToStartTwiceNeedsYou(t *testing.T) {
	h := rerunHarness(t)
	h.setHeadRuns(fixHead, noJobRun())
	h.pastSettle()
	h.tick()
	h.tick()
	run := noJobRun()
	run.Attempt = 2
	h.setHeadRuns(fixHead, run)
	h.advance(time.Minute)
	h.tick()
	h.tick()
	h.wantOpen(OpenNeedsYou, "required check build did not run at a1a1a1a: run 42673098082 of ci.yml failed to start twice (failure); re-run it on GitHub, or push")
	evs := h.fixEvents("fix.open_ci_rerun_twice")
	if len(evs) != 1 || evs[0].Level != "warn" || evs[0].Message != "run 42673098082 of ci.yml failed to start twice at a1a1a1a (failure): the PR needs you" {
		t.Errorf("fix.open_ci_rerun_twice events = %+v, want one", evs)
	}
	for range 3 {
		h.advance(10 * time.Minute)
		h.tick()
	}
	if n := len(h.ghCalls("rerun:")); n != 1 {
		t.Fatalf("reruns = %d, want 1", n)
	}
	if n := len(h.fixEvents("fix.open_ci_rerun_twice")); n != 1 {
		t.Errorf("fix.open_ci_rerun_twice said %d times, want once", n)
	}
	h.wantOpen(OpenNeedsYou, "run 42673098082 of ci.yml failed to start twice")
}

// GitHub refuses the re-run (HTTP 403) to an identity that may not run
// workflows: the PR needs you with cannotStartWorkflows' words, never
// GitHub's message, and the head is not re-run again.
func TestAReRunGitHubForbidsNeedsYou(t *testing.T) {
	h := rerunHarness(t)
	h.setHeadRuns(fixHead, noJobRun())
	h.gh.mu.Lock()
	h.gh.rerunErr = &github.APIError{Op: "rerun", Status: 403, Message: "Resource not accessible by integration"}
	h.gh.mu.Unlock()
	h.pastSettle()
	h.tick()
	h.tick()
	h.wantOpen(OpenNeedsYou, "required check build did not run at a1a1a1a: magnum's re-run of run 42673098082 of ci.yml failed "+
		"(GitHub answered HTTP 403: zhuravel cannot start workflows, a gh login needs push access and the repo scope; "+
		"set [[repo]] ci_dispatch_as to an identity that can); re-run it on GitHub")
	ev, ok := h.fixEvent("fix.open_ci_rerun_failed")
	if !ok || ev.Level != "warn" || strings.Contains(ev.Message, "Resource not accessible") || !strings.Contains(ev.Message, "HTTP 403") {
		t.Fatalf("fix.open_ci_rerun_failed = %+v, %v", ev, ok)
	}
	h.advance(30 * time.Minute)
	h.tick()
	if n := len(h.ghCalls("rerun:")); n != 1 {
		t.Fatalf("reruns = %d, want 1", n)
	}
}

// A PR that changes its own workflows or actions (a workflow, a local
// action, any action.yml or action.yaml) is never re-run: its run would
// run its own code with the repository's secrets.
func TestTheLookNeverReRunsAPRThatChangesItsWorkflowsOrActions(t *testing.T) {
	for _, p := range []string{".github/workflows/ci.yml", ".github/actions/setup/action.yml", "tools/lint/action.yaml", "action.yml"} {
		t.Run(p, func(t *testing.T) {
			h := rerunHarness(t, "README.md", p)
			h.setHeadRuns(fixHead, noJobRun())
			h.pastSettle()
			h.tick()
			h.tick()
			if calls := h.ghCalls("rerun:"); len(calls) != 0 {
				t.Fatalf("re-ran %q", calls)
			}
			h.wantOpen(OpenNeedsYou, "required check build did not run at a1a1a1a: magnum does not re-run run 42673098082 of ci.yml "+
				"(the PR changes its own workflows or actions); re-run it on GitHub")
			if evs := h.fixEvents("fix.open_ci_rerun_refused"); len(evs) != 1 {
				t.Fatalf("fix.open_ci_rerun_refused events = %+v, want one", evs)
			}
		})
	}
}

// A run of the head that has not ended (queued, no job yet) is no start
// that failed: the look re-runs nothing beside it (the workflow's
// concurrency group would cancel it), waits, and reads the head's runs
// again only settle_max later; a startup_failure then is re-run without a
// read of its jobs.
func TestAQueuedRunIsNoStartThatFailed(t *testing.T) {
	h := rerunHarness(t)
	run := noJobRun()
	run.Status, run.Conclusion = "queued", ""
	h.setHeadRuns(fixHead, run)
	h.pastSettle()
	h.tick()
	for range 5 {
		h.advance(time.Minute)
		h.tick()
	}
	h.wantOpen(OpenWaits, "CI at a1a1a1a: run 42673098082 has started no job yet")
	if calls := h.ghCalls("head_runs:"); len(calls) != 1 {
		t.Fatalf("head runs read %d times in 6 looks, want once", len(calls))
	}
	if calls := h.ghCalls("rerun:"); len(calls) != 0 {
		t.Fatalf("re-ran %q beside a queued run", calls)
	}
	run.Status, run.Conclusion = "completed", "startup_failure"
	h.setHeadRuns(fixHead, run)
	h.advance(20 * time.Minute)
	h.tick()
	if calls := h.ghCalls("rerun:"); !slices.Equal(calls, []string{"rerun:talkable/talkable#42673098082"}) {
		t.Fatalf("reruns = %q", calls)
	}
	if calls := h.ghCalls("run_jobs:"); len(calls) != 0 {
		t.Errorf("jobs read %q: a startup_failure started none", calls)
	}
}

// Only a run of a .github/workflows file, for a pull_request or a push,
// that ended with no job started is re-run: GitHub's startup_failure (no
// jobs read), or a failure or a cancel whose latest attempt has no job.
// Any other run leaves rule 11's words as they were, and the head's runs
// are read once for the head, never per look.
func TestOnlyARunThatEndedWithNoJobStartedIsReRun(t *testing.T) {
	for _, c := range []struct {
		name            string
		event, workflow string
		conclusion      string
		jobs            int
		rerun           bool
		jobReads        int
	}{
		{"startup failure", "pull_request", "ci.yml", "startup_failure", 0, true, 0},
		{"failure with no job", "pull_request", "ci.yml", "failure", 0, true, 1},
		{"cancelled with no job", "pull_request", "ci.yml", "cancelled", 0, true, 1},
		{"push startup failure", "push", "ci.yml", "startup_failure", 0, true, 0},
		{"failure with a job", "pull_request", "ci.yml", "failure", 1, false, 1},
		{"action required", "pull_request", "ci.yml", "action_required", 0, false, 0},
		{"skipped", "pull_request", "ci.yml", "skipped", 0, false, 0},
		{"success", "pull_request", "ci.yml", "success", 0, false, 0},
		{"a dispatch", "workflow_dispatch", "ci.yml", "failure", 0, false, 0},
		{"a dynamic run", "dynamic", "", "startup_failure", 0, false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := rerunHarness(t)
			run := noJobRun()
			run.Event, run.Workflow, run.Conclusion = c.event, c.workflow, c.conclusion
			h.setHeadRuns(fixHead, run)
			h.setJobs(liveRunID, c.jobs)
			h.pastSettle()
			for range 6 {
				h.tick()
				h.advance(time.Minute)
			}
			if calls := h.ghCalls("rerun:"); (len(calls) == 1) != c.rerun || len(calls) > 1 {
				t.Fatalf("reruns = %q, want a re-run %v", calls, c.rerun)
			}
			if n := len(h.ghCalls("run_jobs:")); n != c.jobReads {
				t.Errorf("jobs read %d times, want %d", n, c.jobReads)
			}
			if !c.rerun {
				h.wantOpen(OpenNeedsYou, rule11Words)
				if calls := h.ghCalls("head_runs:"); len(calls) != 1 {
					t.Errorf("head runs read %d times, want once for the head", len(calls))
				}
			}
		})
	}
}

// Each workflow's newest run for an event is the one that counts: an older
// run of ci.yml that started no job, which a newer run of ci.yml for the
// same event at the head followed with its jobs, is not re-run, and the
// older one's jobs are not read.
func TestAnOlderRunOfAWorkflowThatRanAgainIsNotReRun(t *testing.T) {
	h := rerunHarness(t)
	newer := noJobRun()
	newer.ID = liveRunID + 5
	h.setHeadRuns(fixHead, newer, noJobRun())
	h.setJobs(newer.ID, 2)
	h.pastSettle()
	h.tick()
	h.tick()
	if calls := h.ghCalls("rerun:"); len(calls) != 0 {
		t.Fatalf("re-ran %q, an older run of a workflow that ran again", calls)
	}
	if calls := h.ghCalls("run_jobs:"); !slices.Equal(calls, []string{"run_jobs:talkable/talkable#42673098087"}) {
		t.Errorf("jobs reads = %q, want the newer run's only", calls)
	}
	h.wantOpen(OpenNeedsYou, rule11Words)
}

// A read of the head's runs that fails is said once
// (fix.open_ci_runs_error), leaves rule 11's words, and is read again only
// settle_max later; that read finds the run and re-runs it.
func TestAFailedReadOfTheHeadsRunsIsSaidOnceAndReadAgainAfterSettleMax(t *testing.T) {
	h := rerunHarness(t)
	h.setHeadRuns(fixHead, noJobRun())
	h.gh.mu.Lock()
	h.gh.headRunsErr = errors.New("HTTP 502")
	h.gh.mu.Unlock()
	h.pastSettle()
	for range 6 {
		h.tick()
		h.advance(time.Minute)
	}
	h.wantOpen(OpenNeedsYou, rule11Words)
	if calls := h.ghCalls("head_runs:"); len(calls) != 1 {
		t.Fatalf("head runs read %d times in 6 looks, want once", len(calls))
	}
	evs := h.fixEvents("fix.open_ci_runs_error")
	if len(evs) != 1 || evs[0].Level != "warn" || !strings.Contains(evs[0].Message, "HTTP 502") {
		t.Fatalf("fix.open_ci_runs_error events = %+v, want one", evs)
	}
	h.gh.mu.Lock()
	h.gh.headRunsErr = nil
	h.gh.mu.Unlock()
	h.advance(15 * time.Minute)
	h.tick()
	if calls := h.ghCalls("rerun:"); !slices.Equal(calls, []string{"rerun:talkable/talkable#42673098082"}) {
		t.Fatalf("reruns = %q after the read again", calls)
	}
}

// A new head starts fresh: the re-run record is the last head's, so a run
// that started no job at the next head is read and re-run once again.
func TestANewHeadIsReRunAfresh(t *testing.T) {
	h := rerunHarness(t)
	h.setHeadRuns(fixHead, noJobRun())
	h.pastSettle()
	h.tick()
	h.tick()
	next := headOf(3)
	run := noJobRun()
	run.ID, run.HeadSHA = liveRunID+1, next
	h.setHeadRuns(next, run)
	h.openOwn(prSpec{head: next})
	h.tick()
	h.pastSettle()
	h.tick()
	h.tick()
	want := []string{"rerun:talkable/talkable#42673098082", "rerun:talkable/talkable#42673098083"}
	if calls := h.ghCalls("rerun:"); !slices.Equal(calls, want) {
		t.Fatalf("reruns = %q, want %q", calls, want)
	}
}

// With ci_dispatch, a head whose run started no job is re-run, not
// dispatched (the run of its own event, whose suite GitHub's rollup
// lists); a head with no run at all is dispatched as before.
func TestCIDispatchReRunsARunThatStartedNoJobAndDispatchesAHeadWithNone(t *testing.T) {
	h := ciDispatched(t, "app/models/order.rb")
	h.setHeadRuns(fixHead, noJobRun())
	h.pastSettle()
	h.tick()
	h.tick()
	if calls := h.ghCalls("dispatch:"); len(calls) != 0 {
		t.Fatalf("dispatched %q beside a run that started no job", calls)
	}
	if calls := h.ghCalls("rerun:"); len(calls) != 1 {
		t.Fatalf("reruns = %q, want one", calls)
	}
	h.wantOpen(OpenWaits, "CI at a1a1a1a: magnum re-ran run 42673098082 of ci.yml at ")
	h2 := ciDispatched(t, "app/models/order.rb")
	h2.pastSettle()
	h2.tick()
	h2.tick()
	if calls := h2.ghCalls("dispatch:"); len(calls) != 1 {
		t.Fatalf("dispatches = %q, want one where no run started", calls)
	}
	h2.wantOpen(OpenWaits, "CI at a1a1a1a: magnum started ci.yml at ")
}

// The re-run waits for the end of `magnum pause` and for the GitHub budget
// of the identity it re-runs as (the poll identity's, f.low, and
// ci_dispatch_as's, f.ciLow), as a start does, and so does the read of
// the head's runs for the budget; without either, the verdict asks the
// look to read, then to re-run.
func TestTheReRunWaitsForThePauseAndTheBudget(t *testing.T) {
	f := idleFacts()
	f.ci = openCI{missing: []string{"build"}}
	v, _, _ := decideOpen(f)
	if !v.readRuns || v.rerun {
		t.Fatalf("verdict = %+v, want the read of the head's runs asked for", v)
	}
	f.low = true
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenWaits, "CI at a1a1a1a: required check build did not run; magnum reads the head's runs once the GitHub budget allows")
	if v.readRuns {
		t.Errorf("read asked for below the floor")
	}
	f.low = false
	f.ciRerun = ciRerun{Head: fixHead, ReadAt: openNow.Add(-time.Minute),
		Run: &ciNoJob{ID: liveRunID, Workflow: "ci.yml", Attempt: 1, Conclusion: "failure"}}
	v, _, _ = decideOpen(f)
	if !v.rerun || v.readRuns || v.Verdict != OpenNeedsYou {
		t.Fatalf("verdict = %+v, want the re-run asked for", v)
	}
	wantVerdict(t, v, OpenNeedsYou, "required check build did not run at a1a1a1a: magnum re-runs run 42673098082 of ci.yml")
	f.pause, f.pauseUntil = "daemon paused", openNow.Add(time.Hour)
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenWaits, "CI at a1a1a1a: required check build did not run; magnum re-runs run 42673098082 of ci.yml once the pause ends")
	if v.rerun || !v.Until.Equal(f.pauseUntil) {
		t.Errorf("rerun %v, until %s under the pause", v.rerun, v.Until)
	}
	f.pause, f.pauseUntil, f.ciLow = "", time.Time{}, true
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenWaits, "CI at a1a1a1a: required check build did not run; magnum re-runs run 42673098082 of ci.yml once the GitHub budget allows")
	if v.rerun {
		t.Errorf("re-run asked for below the floor")
	}
}

// Under `magnum pause` the look reads the head's runs but re-runs nothing,
// and says so; the first look after `magnum resume` re-runs it. Below the
// floor of the identity it reads as (the poll identity's, then
// ci_dispatch_as's) it reads nothing, and says so where the look's own wait
// for the budget does not speak first; once the budget is back it reads
// and re-runs.
func TestTheLookReRunsOnceThePauseAndTheBudgetAllow(t *testing.T) {
	h := rerunHarness(t)
	h.setHeadRuns(fixHead, noJobRun())
	h.e.setKV(h.ctx, store.KVDaemonPaused, "1")
	h.pastSettle()
	h.tick()
	h.tick()
	if calls := h.ghCalls("rerun:"); len(calls) != 0 {
		t.Fatalf("re-ran %q under the pause", calls)
	}
	if calls := h.ghCalls("head_runs:"); len(calls) != 1 {
		t.Fatalf("head runs read %d times under the pause, want once", len(calls))
	}
	h.wantOpen(OpenWaits, "CI at a1a1a1a: required check build did not run; magnum re-runs run 42673098082 of ci.yml once the pause ends")
	h.e.delKV(h.ctx, store.KVDaemonPaused)
	h.tick()
	if calls := h.ghCalls("rerun:"); len(calls) != 1 {
		t.Fatalf("reruns = %q after the pause, want one", calls)
	}

	h = rerunHarness(t)
	h.setHeadRuns(fixHead, noJobRun())
	h.pastSettle()
	until := h.lowBudget("zhuravel", 100, h.clock.Now().Add(20*time.Minute))
	h.tick()
	h.tick()
	if calls := h.ghCalls("head_runs:"); len(calls) != 0 {
		t.Fatalf("read the head's runs below the poll identity's floor: %q", calls)
	}
	h.advance(until.Sub(h.clock.Now()) + time.Minute)
	h.tick()
	h.tick()
	if calls := h.ghCalls("rerun:"); len(calls) != 1 {
		t.Fatalf("reruns = %q once the budget is back", calls)
	}

	h = rerunHarnessAs(t, "talkable-app")
	h.setHeadRuns(fixHead, noJobRun())
	h.pastSettle()
	until = h.lowBudget("talkable-app", 100, h.clock.Now().Add(20*time.Minute))
	h.tick()
	h.tick()
	if calls := h.ghCalls("head_runs:"); len(calls) != 0 {
		t.Fatalf("read the head's runs below ci_dispatch_as's floor: %q", calls)
	}
	h.wantOpen(OpenWaits, "CI at a1a1a1a: required check build did not run; magnum reads the head's runs once the GitHub budget allows")
	h.advance(until.Sub(h.clock.Now()) + time.Minute)
	h.tick()
	h.tick()
	if calls := h.ghCalls("rerun"); !slices.Equal(calls, []string{"rerun as talkable-app:talkable/talkable#42673098082"}) {
		t.Fatalf("reruns = %q once the budget is back", calls)
	}
}

// The re-run's attempt that waits for a runner past settle_max keeps the
// look waiting (the poll reads it), as a started run does.
func TestAReRunStillQueuedPastSettleMaxKeepsTheLookWaiting(t *testing.T) {
	h := rerunHarness(t)
	h.setHeadRuns(fixHead, noJobRun())
	h.pastSettle()
	h.tick()
	h.tick()
	h.advance(25 * time.Minute) // past settle_max after the re-run; the fake's attempt 2 is queued
	h.tick()
	h.tick()
	h.wantOpen(OpenWaits, "CI at a1a1a1a: magnum re-ran run 42673098082 of ci.yml at ")
}

// settle_max after the re-run with no new attempt seen, or with one that
// ended without the required check, the PR needs you, with how it ended.
func TestAReRunWhoseAttemptNeverCameOrEndedWithoutTheCheckNeedsYou(t *testing.T) {
	h := rerunHarness(t)
	h.setHeadRuns(fixHead, noJobRun())
	h.pastSettle()
	h.tick()
	if n := len(h.ghCalls("rerun:")); n != 1 {
		t.Fatalf("reruns = %d after the look past settle_max, want 1", n)
	}
	h.setHeadRuns(fixHead, noJobRun()) // GitHub lists attempt 1 still
	h.tick()
	h.advance(10 * time.Minute)
	h.tick()
	h.wantOpen(OpenWaits, "CI at a1a1a1a: magnum re-ran run 42673098082 of ci.yml at ")
	h.advance(11 * time.Minute)
	h.tick()
	v := h.wantOpen(OpenNeedsYou, "required check build did not run at a1a1a1a: magnum re-ran run 42673098082 of ci.yml at ")
	if !strings.HasSuffix(v.Why, " and no new attempt came; re-run it on GitHub") {
		t.Errorf("why = %q, want no new attempt", v.Why)
	}

	h = rerunHarness(t)
	h.setHeadRuns(fixHead, noJobRun())
	h.pastSettle()
	h.tick()
	h.tick()
	run := noJobRun()
	run.Attempt, run.Conclusion = 2, "success"
	h.setHeadRuns(fixHead, run)
	h.advance(time.Minute)
	h.tick()
	h.wantOpen(OpenWaits, "CI at a1a1a1a: magnum re-ran run 42673098082 of ci.yml at ")
	h.advance(20 * time.Minute)
	h.tick()
	v = h.wantOpen(OpenNeedsYou, "required check build did not run at a1a1a1a: magnum re-ran run 42673098082 of ci.yml at ")
	if !strings.HasSuffix(v.Why, " and it ended (success) without it; re-run it on GitHub") {
		t.Errorf("why = %q, want how the attempt ended", v.Why)
	}
	if n := len(h.ghCalls("rerun:")); n != 1 {
		t.Fatalf("reruns = %d, want 1", n)
	}
}

// appRerunner is talkable-app's client: the poll identity's fake for every
// call but the re-run, which it records as "rerun as talkable-app:...".
type appRerunner struct{ *fakeGH }

func (g appRerunner) RerunRun(_ context.Context, owner, repo string, runID int64) error {
	g.record(fmt.Sprintf("rerun as talkable-app:%s/%s#%d", owner, repo, runID))
	return nil
}

// rerunHarnessAs is rerunHarness with [[repo]] ci_dispatch_as = as and no
// ci_dispatch, and talkable-app's client in place (appRerunner).
func rerunHarnessAs(t *testing.T, as string) *harness {
	t.Helper()
	h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"), func(h *harness) {
		h.gh.required = map[string][]string{"talkable/talkable": {"build"}}
		h.gh.defaultFiles = []string{"source/partials/ip_list.rst"}
		h.cfg.Repos = append(h.cfg.Repos, config.Repo{Repo: "talkable/talkable", CIDispatchAs: as})
		plain := h.d.GitHub
		h.d.GitHub = func(id string) GitHub {
			if id == "talkable-app" {
				return appRerunner{h.gh}
			}
			return plain(id)
		}
	})
	h.openOwn(prSpec{head: fixHead})
	h.tick() // reviewed clean
	return h
}

// [[repo]] ci_dispatch_as alone (no ci_dispatch) names who reads the
// head's runs and re-runs: the live repository's poll identity is an App
// with Actions: read only, whose re-run GitHub refuses.
func TestTheReRunGoesAsCIDispatchAsWithoutCIDispatch(t *testing.T) {
	h := rerunHarnessAs(t, "talkable-app")
	h.setHeadRuns(fixHead, noJobRun())
	h.pastSettle()
	h.tick()
	h.tick()
	if calls := h.ghCalls("rerun"); !slices.Equal(calls, []string{"rerun as talkable-app:talkable/talkable#42673098082"}) {
		t.Fatalf("reruns = %q", calls)
	}
	data := h.dispatchData("fix.open_ci_rerun")
	if ev, _ := h.fixEvent("fix.open_ci_rerun"); data["identity"] != "talkable-app" || !strings.HasSuffix(ev.Message, " as talkable-app") {
		t.Errorf("fix.open_ci_rerun = %q, %v; want it to name talkable-app", ev.Message, data)
	}
}
