package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// ciCompletion is talkable's required check Completion of ci.yml, in state.
func ciCompletion(state string) github.Check {
	return github.Check{Name: "Completion", State: state, Workflow: "CI", WorkflowFile: "ci.yml", At: ciBefore}
}

// ciFixHarness is the operator's PR #2 of talkable/talkable, whose GitHub
// rulesets require Completion, reviewed clean at fixHead, where Completion
// failed in run ciRunID of ci.yml (setFailedJobs' jobs and logs); the
// base's tip ran Completion as base says. The PR's files are app code.
func ciFixHarness(t *testing.T, base string) (*harness, *fixGit) {
	t.Helper()
	return ciFixHarnessWith(t, base, findings(map[string]int{}, "COMMENTED"))
}

// ciFixHarnessWith is ciFixHarness whose reviews give what next gives,
// with mods applied before the engine is built.
func ciFixHarnessWith(t *testing.T, base string, next func(in pipeline.RoundInput) (map[string]int, string), mods ...func(*harness)) (*harness, *fixGit) {
	t.Helper()
	h, git := openHarness(t, next, append([]func(*harness){requiredCompletion, func(h *harness) {
		h.gh.defaultFiles = []string{"app/models/offer.rb"}
	}}, mods...)...)
	h.openOwn(prSpec{head: fixHead, ci: "FAILURE", checks: []github.Check{ciCompletion(github.CheckFailed)}})
	h.tick() // reviewed clean
	h.setHeadRuns(fixHead, failedCIRun(fixHead, 1))
	h.setFailedJobs()
	h.setBaseCI(base)
	return h, git
}

// setBaseCI makes the base's tip run Completion as state.
func (h *harness) setBaseCI(state string) {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	h.gh.firstParentsAll = []github.CommitWithChecks{{Checks: []github.Check{ciCompletion(state)}}}
}

// ciRecordOf is PR #2's record of CI fixes.
func (h *harness) ciRecordOf() ciFixRecord {
	h.t.Helper()
	r, err := h.e.ciFixRecordOf(h.ctx, h.pr(2).ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

// cannotFixer is a CI fixer that fixes nothing and lists job 101 as class.
func cannotFixer(h *harness, git *fixGit, class string) {
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "nothing", "0 threads", "- 101 ci: not fixed. "+class+": rspec passes when run again")
	})
}

// A failure that is red on the base too starts no CI fix and reads no log:
// the base's CI is read once per head and failed set (one GraphQL call over
// three looks), and a new head is read again.
func TestTheLookReadsTheBaseOncePerHeadAndFailedSet(t *testing.T) {
	h, _ := ciFixHarness(t, github.CheckFailed)
	h.pastSettle()
	h.tick()
	h.tick()
	h.tick()
	if calls := h.ghCalls("first_parents:"); len(calls) != 1 {
		t.Fatalf("base reads = %q, want one", calls)
	}
	v := h.wantOpen(OpenNeedsYou, "required check Completion failed at a1a1a1a, and on main at ")
	if !strings.HasSuffix(v.Why, " too: magnum fixes only a check that passes on the base") || len(v.Needs) != 1 || v.Needs[0].Kind != "ci" {
		t.Errorf("verdict = %+v", v)
	}
	if n := len(h.ghCalls("job_log:")) + len(h.ghCalls("run_jobs:")); n != 0 || len(h.fixRuns(h.pr(2).ID)) != 0 {
		t.Errorf("a failure red on the base read %d jobs or logs, or started a fixer", n)
	}
	next := strings.Repeat("e", 40)
	h.openOwn(prSpec{head: next, ci: "FAILURE", checks: []github.Check{ciCompletion(github.CheckFailed)}})
	h.tick() // magnum reviews the new head
	h.pastSettle()
	h.tick()
	h.tick()
	if calls := h.ghCalls("first_parents:"); len(calls) != 2 {
		t.Fatalf("base reads = %q, want one more for the new head", calls)
	}
}

// The base's CI is read at the tip the look's conflict check fetched, not
// at the PR's stored base_sha (Details' last read, which can trail by
// days), and a tip that moved is read again.
func TestTheBasesCIIsReadAtTheFetchedTipAndAgainWhenItMoves(t *testing.T) {
	h, git := ciFixHarness(t, github.CheckFailed)
	if base := deref(h.pr(2).BaseSHA); base == "" || base == fixBaseTip {
		t.Fatalf("base_sha = %q, want one the fetch does not give", base)
	}
	h.pastSettle()
	h.tick()
	h.tick()
	want := []string{"first_parents:talkable/talkable@" + fixBaseTip}
	if calls := h.ghCalls("first_parents:"); !slices.Equal(calls, want) {
		t.Fatalf("base reads = %q, want %q", calls, want)
	}
	moved := strings.Repeat("d0", 20)
	git.mu.Lock()
	git.tip = moved
	git.mu.Unlock()
	h.advance(31 * time.Minute) // the conflict check fetches the base again
	h.tick()
	if calls := h.ghCalls("first_parents:"); !slices.Equal(calls, append(want, "first_parents:talkable/talkable@"+moved)) {
		t.Fatalf("base reads = %q, want the moved tip read again", calls)
	}
	h.wantOpen(OpenNeedsYou, "required check Completion failed at a1a1a1a, and on main at d0d0d0d too")
}

// ciShape3Harness is talkable's live shape: Completion required, [[repo]]
// ci_dispatch = "ci.yml", and the operator's PR #2 reviewed clean at
// fixHead, whose rollup lists the other apps' checks alone (Completion is
// posted on success alone), with magnum's start of ci.yml at the head an
// hour ago whose run ciRunID ended failure (setFailedJobs' jobs and logs);
// the base ran Completion green, and the PR's files are app code.
func ciShape3Harness(t *testing.T) (*harness, *fixGit) {
	t.Helper()
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"), requiredCompletion, func(h *harness) {
		h.cfg.Repos = append(h.cfg.Repos, config.Repo{Repo: "talkable/talkable", CIDispatch: "ci.yml"})
		h.gh.defaultFiles = []string{"app/models/offer.rb"}
	})
	h.othersPassed(fixHead)
	h.tick() // reviewed clean
	run := failedCIRun(fixHead, 1)
	run.Event = "workflow_dispatch"
	h.setHeadRuns(fixHead, run)
	h.setFailedJobs()
	h.setBaseCI(github.CheckPassed)
	if !h.e.writeCIStart(h.ctx, h.pr(2), ciStart{Head: fixHead, Workflow: "ci.yml", At: h.clock.Now().Add(-time.Hour),
		Run: &ciRun{ID: ciRunID, Status: "completed", Conclusion: "failure"}}) {
		t.Fatal("the start of ci.yml was not recorded")
	}
	return h, git
}

// talkable's main path: magnum's run of ci.yml at the head ended failure,
// and Completion, posted on success alone, is missing. The look reads that
// run's jobs, starts a CI fix, and the head then needs you for a failed
// check, kind ci, never for one that did not run.
func TestTheLookFixesTheCIOfARunMagnumStartedThatEndedFailure(t *testing.T) {
	h, git := ciShape3Harness(t)
	cannotFixer(h, git, "flaky")
	h.pastSettle()
	h.tick()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 1 {
		t.Fatalf("fix runs = %d, want the CI fix's", n)
	}
	if calls := h.ghCalls("run_jobs:"); !slices.Equal(calls, []string{"run_jobs:talkable/talkable#500"}) {
		t.Errorf("jobs reads = %q, want magnum's run's", calls)
	}
	h.tick()
	v := h.wantOpen(OpenNeedsYou, "required check Completion failed at a1a1a1a (magnum's run of ci.yml ended failure): the CI fix found a flaky test, not the PR's code")
	if len(v.Needs) != 1 || v.Needs[0].Kind != "ci" || strings.Contains(v.Why, " did not run at ") {
		t.Errorf("verdict = %+v, want kind ci", v)
	}
}

// `magnum fix --ci` reads magnum's run that ended failure as the look does:
// a failed check it fixes, not one that did not run.
func TestMagnumFixCIFixesARunMagnumStartedThatEndedFailure(t *testing.T) {
	h, git := ciShape3Harness(t)
	cannotFixer(h, git, "flaky")
	id := h.fix(FixPayload{Mode: FixModeCI})
	h.handle()
	if res := h.wantDone(id); !strings.HasPrefix(res, "fixing the failed CI of talkable/talkable#2: ") {
		t.Errorf("answer = %q", res)
	}
	if n := len(h.fixRuns(h.pr(2).ID)); n != 1 {
		t.Errorf("fix runs = %d, want the CI fix's", n)
	}
}

// No rollup shows magnum's run of the head, so a re-run of it by hand shows
// only in the head's runs: a read of its failed jobs that stands from an
// earlier look is checked against them before the start it feeds (once a
// settle_quiet at most, not every look), and a newer attempt is read
// again: one that still runs is waited for as magnum's start (the poll reads
// it), and one that failed again gives the fixer its own logs.
func TestAReRunByHandOfMagnumsRunIsReadAgainBeforeTheStart(t *testing.T) {
	h, git := ciShape3Harness(t)
	cannotFixer(h, git, "flaky")
	h.pastSettle()
	pr := h.pr(2)
	if err := h.st.TransitionPR(h.ctx, pr.ID, []string{pr.State}, pr.State, func(u *store.PRUpdate) { u.Set("human_active_at", h.clock.Now()) }); err != nil {
		t.Fatal(err)
	}
	h.tick() // the read, and a start refused: someone typed into the panes
	if rec := h.ciRecordOf(); rec.Read.Jobs != 2 || len(rec.Starts) != 1 || rec.Starts[0].Outcome != ciOutcomeUnstarted {
		t.Fatalf("record = %+v", rec)
	}
	runReads := len(h.ghCalls("head_runs:"))
	h.advance(time.Minute)
	h.tick()
	if n := len(h.ghCalls("head_runs:")); n != runReads {
		t.Errorf("the head's runs read %d times more within settle_quiet", n-runReads)
	}
	attempt := failedCIRun(fixHead, 2)
	attempt.Event, attempt.Status, attempt.Conclusion = "workflow_dispatch", "in_progress", ""
	h.setHeadRuns(fixHead, attempt)
	h.setRuns(fixHead, attempt)
	h.advance(2 * time.Hour) // past the cooldown
	h.tick()
	h.wantOpen(OpenWaits, "CI at a1a1a1a: ci.yml was run again after magnum's run of it failed")
	if n := len(h.fixRuns(pr.ID)); n != 0 {
		t.Fatalf("a CI fix started on attempt 1 while attempt 2 runs (%d fix runs)", n)
	}
	attempt.Status, attempt.Conclusion = "completed", "failure"
	h.setHeadRuns(fixHead, attempt)
	h.setRuns(fixHead, attempt)
	h.advance(time.Minute)
	h.tick() // the poll reads the attempt's end, and the look reads its failed jobs
	if n := len(h.fixRuns(pr.ID)); n != 1 {
		t.Fatalf("fix runs = %d, want the CI fix on attempt 2", n)
	}
	raw, err := os.ReadFile(filepath.Join(h.d.Layout.ReviewDir("talkable", "talkable", 2, fixHead), ciFailuresFile))
	if err != nil {
		t.Fatal(err)
	}
	var f ciFailuresJSON
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Checks) != 1 || f.Checks[0].Attempt != 2 {
		t.Errorf("the fixer's file = %+v, want attempt 2's", f.Checks)
	}
}

// ciHeldHarness is ciShape3Harness past the settle, with magnum's run's
// failed jobs read once (attempt 1, two jobs) and the CI fix held: someone
// typed into the PR's panes, so each look decides on the CI fix and the fix
// job refuses it (unstarted) until the human cooldown ends.
func ciHeldHarness(t *testing.T) *harness {
	t.Helper()
	h, git := ciShape3Harness(t)
	cannotFixer(h, git, "flaky")
	h.pastSettle()
	pr := h.pr(2)
	if err := h.st.TransitionPR(h.ctx, pr.ID, []string{pr.State}, pr.State, func(u *store.PRUpdate) { u.Set("human_active_at", h.clock.Now()) }); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if rec := h.ciRecordOf(); rec.Read.Jobs != 2 || len(rec.Starts) != 1 || rec.Starts[0].Outcome != ciOutcomeUnstarted {
		t.Fatalf("record = %+v", rec)
	}
	return h
}

// A read that stands while the CI fix waits is checked against the head's
// runs once a settle_quiet, not at every look, and an unchanged run is not
// read again: the check's time and the read's runs are kept.
func TestAStandingReadChecksTheHeadsRunsOnceASettleQuiet(t *testing.T) {
	h := ciHeldHarness(t)
	runs, jobs := len(h.ghCalls("head_runs:")), len(h.ghCalls("run_jobs:"))
	for i, step := range []struct {
		advance time.Duration
		reads   int
	}{{4 * time.Minute, 1}, {time.Minute, 1}, {time.Minute, 1}, {time.Minute, 2}} {
		h.advance(step.advance)
		h.tick()
		if n := len(h.ghCalls("head_runs:")) - runs; n != step.reads {
			t.Fatalf("look %d: the head's runs read %d times, want %d", i+1, n, step.reads)
		}
	}
	if n := len(h.ghCalls("run_jobs:")) - jobs; n != 0 {
		t.Errorf("the run's jobs read %d times more, want none", n)
	}
}

// A run still running at the attempt the standing read read (GitHub lists
// a queued re-run at its old attempt) feeds no CI fix once the hold ends:
// the look waits for it as for its own start.
func TestARunStillRunningAtTheAttemptReadFeedsNoCIFix(t *testing.T) {
	h := ciHeldHarness(t)
	running := failedCIRun(fixHead, 1)
	running.Event, running.Status, running.Conclusion = "workflow_dispatch", "in_progress", ""
	h.setHeadRuns(fixHead, running)
	h.setRuns(fixHead, running)
	h.advance(2 * time.Hour) // past the cooldown
	h.tick()
	h.wantOpen(OpenWaits, "CI at a1a1a1a: ci.yml was run again after magnum's run of it failed")
	if n := len(h.fixRuns(h.pr(2).ID)); n != 0 {
		t.Fatalf("a CI fix started on the attempt that still runs (%d fix runs)", n)
	}
}

// A read of the head's runs that fails while the CI fix waits is tried
// again a settle_quiet later, not at every look.
func TestAFailedReadOfTheHeadsRunsWaitsASettleQuiet(t *testing.T) {
	h := ciHeldHarness(t)
	h.gh.mu.Lock()
	h.gh.headRunsErr = &github.APIError{Op: "head runs", Status: 502}
	h.gh.mu.Unlock()
	runs := len(h.ghCalls("head_runs:"))
	for i, step := range []struct {
		advance time.Duration
		reads   int
	}{{4 * time.Minute, 1}, {time.Minute, 1}, {time.Minute, 1}, {time.Minute, 2}} {
		h.advance(step.advance)
		h.tick()
		if n := len(h.ghCalls("head_runs:")) - runs; n != step.reads {
			t.Fatalf("look %d: the head's runs read %d times, want %d", i+1, n, step.reads)
		}
	}
}

// `magnum fix --ci` finds a re-run by hand of magnum's failed run as the
// look does: nothing failed to fix while it runs, and the start's record
// takes the run, which the look then waits for.
func TestMagnumFixCIRefusesARunOfMagnumsThatRanAgain(t *testing.T) {
	h, git := ciShape3Harness(t)
	cannotFixer(h, git, "flaky")
	attempt := failedCIRun(fixHead, 2)
	attempt.Event, attempt.Status, attempt.Conclusion = "workflow_dispatch", "in_progress", ""
	h.setHeadRuns(fixHead, attempt)
	h.setRuns(fixHead, attempt)
	h.wantCIFixRefused("ci.yml was run again at a1a1a1a after magnum's run of it failed: nothing failed to fix")
	if s := h.e.ciStartAt(h.ctx, h.pr(2)); s.Moved.IsZero() || !s.Run.Running() {
		t.Errorf("start = %+v, want the re-run as its run", s)
	}
	if n := len(h.ghCalls("run_jobs:")); n != 0 {
		t.Errorf("%d jobs reads, want none", n)
	}
}

// No rollup shows magnum's run of ci.yml (ciShape3), nor a re-run of it by
// hand or someone's later start of ci.yml at the head, and the Completion
// such a run posts when it passes may move none of the rollup's counts. The
// look, about to start a CI fix, finds that run in the head's runs it reads
// and takes the failure back: it waits for the run as for its own start,
// the poll reads the run and the head's checks (while it runs, and
// settle_max after the look found it), and once it passed the PR is idle.
// Nothing needs you on the way, no failed job is read and no CI fix starts.
// It said "its run lists no failed job magnum can read; it is yours", and
// nothing read the head's checks again.
func TestAPassedReRunByHandOfMagnumsFailedRunLeavesThePRIdle(t *testing.T) {
	storetest.Parallel(t)
	first := failedCIRun(fixHead, 1)
	first.Event = "workflow_dispatch"
	rerun := func(status, conclusion string) []github.WorkflowRun {
		r := failedCIRun(fixHead, 2)
		r.Event, r.Status, r.Conclusion = "workflow_dispatch", status, conclusion
		return []github.WorkflowRun{r}
	}
	later := func(status, conclusion string) []github.WorkflowRun {
		r := first
		r.ID, r.Status, r.Conclusion, r.CreatedAt = ciRunID+1, status, conclusion, first.CreatedAt.Add(10*time.Minute)
		return []github.WorkflowRun{r, first}
	}
	for _, tc := range []struct {
		name               string
		runs               func(status, conclusion string) []github.WorkflowRun
		status, conclusion string // as the look first reads the head's runs
	}{
		{"a re-run that runs", rerun, "in_progress", ""},
		{"a re-run that passed", rerun, "completed", "success"},
		{"a later start by hand", later, "queued", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, git := ciShape3Harness(t)
			cannotFixer(h, git, "flaky") // a CI fix that starts on the old failure ends
			others := othersChecks()
			h.setCounts(2, fakeCounts(others)) // the run's Completion moves none of the rollup's counts
			ran := func(status, conclusion string) {
				runs := tc.runs(status, conclusion)
				h.setHeadRuns(fixHead, runs...)
				h.setRuns(fixHead, runs...)
				if runs[0].ID == ciRunID {
					h.setJobs(ciRunID, 0) // a run's jobs are its newest attempt's
				}
				if conclusion == "success" {
					h.setCI(2, "SUCCESS", append(others, ciCompletion(github.CheckPassed))...)
				}
			}
			ran(tc.status, tc.conclusion)
			h.pastSettle()
			h.tick()
			h.wantOpen(OpenWaits, "CI at a1a1a1a: ci.yml was run again after magnum's run of it failed")
			ran("completed", "success")
			h.advance(time.Minute)
			h.tick()
			h.wantOpen(OpenIdle, "nothing open at a1a1a1a")
			if n := len(h.fixRuns(h.pr(2).ID)); n != 0 || len(h.ghCalls("run_jobs:")) != 0 {
				t.Errorf("%d fix runs and %d jobs reads, want none", n, len(h.ghCalls("run_jobs:")))
			}
		})
	}
}

// The same holds for the look's re-run of a run that started no job
// (ciRerun): its new attempt that ended failure is the failed check, and a
// re-run of it by hand that passed takes the failure back where the reader
// finds it. The look waits for it, the poll reads the head's checks, and
// the PR is idle; no failed job is read and no CI fix starts. It said "its
// run lists no failed job magnum can read; it is yours".
func TestAPassedReRunByHandOfMagnumsFailedReRunLeavesThePRIdle(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"), requiredCompletion, func(h *harness) {
		h.gh.defaultFiles = []string{"app/models/offer.rb"}
	})
	cannotFixer(h, git, "flaky") // a CI fix that starts on the old failure ends
	h.othersPassed(fixHead)
	h.tick() // reviewed clean
	h.setBaseCI(github.CheckPassed)
	at := h.clock.Now().Add(-time.Hour)
	if err := writeKVJSON(h.ctx, h.e, kvPRCIRerun(h.pr(2).ID), "the look's re-run", ciRerun{Head: fixHead, ReadAt: at,
		Run: &ciNoJob{ID: ciRunID, Workflow: "ci.yml", Attempt: 1, Conclusion: "failure"}, At: at,
		Seen: &ciAttempt{Attempt: 2, Status: "completed", Conclusion: "failure"}}); err != nil {
		t.Fatal(err)
	}
	others := othersChecks()
	h.setCounts(2, fakeCounts(others)) // the rollup's counts have not moved
	passed := failedCIRun(fixHead, 3)
	passed.Conclusion = "success"
	h.setHeadRuns(fixHead, passed)
	h.setJobs(ciRunID, 0)
	h.setCI(2, "SUCCESS", append(others, ciCompletion(github.CheckPassed))...)
	h.pastSettle()
	h.tick()
	h.wantOpen(OpenWaits, "CI at a1a1a1a: run 500 of ci.yml was run again after magnum's re-run of it failed")
	h.advance(time.Minute)
	h.tick()
	h.wantOpen(OpenIdle, "nothing open at a1a1a1a")
	if n := len(h.fixRuns(h.pr(2).ID)); n != 0 || len(h.ghCalls("run_jobs:")) != 0 {
		t.Errorf("%d fix runs and %d jobs reads, want none", n, len(h.ghCalls("run_jobs:")))
	}
}

// A read of magnum's failed run that listed no failed job needs you, and
// stands no longer than a settle_quiet: the look checks it against the
// head's runs as it does before a start (ciRunMoved, at most once a
// settle_quiet), so a re-run of that run by hand that failed again is read,
// and its CI fix starts on the new attempt's logs.
func TestAReadThatListedNoFailedJobIsCheckedAgainstTheHeadsRuns(t *testing.T) {
	h, git := ciShape3Harness(t)
	cannotFixer(h, git, "flaky")
	h.setJobs(ciRunID, 0)
	h.pastSettle()
	h.tick()
	h.wantOpen(OpenNeedsYou, "(magnum's run of ci.yml ended failure): its run lists no failed job magnum can read; it is yours")
	reads := len(h.ghCalls("head_runs:"))
	h.advance(time.Minute)
	h.tick()
	if n := len(h.ghCalls("head_runs:")); n != reads {
		t.Errorf("the head's runs read %d times more within settle_quiet", n-reads)
	}
	attempt := failedCIRun(fixHead, 2)
	attempt.Event = "workflow_dispatch"
	h.setHeadRuns(fixHead, attempt)
	h.setFailedJobs()
	h.advance(3 * time.Minute)
	h.tick()
	if n := len(h.fixRuns(h.pr(2).ID)); n != 1 {
		t.Fatalf("fix runs = %d, want the CI fix on attempt 2", n)
	}
	if rec := h.ciRecordOf(); rec.Read.Jobs != 2 || len(rec.Read.Runs) != 1 || rec.Read.Runs[0].Attempt != 2 {
		t.Errorf("read = %+v, want attempt 2's two failed jobs", rec.Read)
	}
}

// A failure green on the base starts a CI fix once: the look reads the
// failed jobs once (the head's runs, the run's jobs, two logs), writes
// ci-failures.json and its summary beside the fixer's report, records only
// the file's digest and counts, and names no check's or job's name in its
// events; the fixer gets the file and no external file, and fix.start
// names no review it worked on. A fixer that says the test is flaky ends
// the loop quietly (no needs attention), and the head needs you with its
// words, kind ci; nothing starts or reads again at the head.
func TestTheLookReadsTheFailedJobsOnceAndStartsACIFixThatEndsQuietly(t *testing.T) {
	h, git := ciFixHarness(t, github.CheckPassed)
	cannotFixer(h, git, "flaky")
	h.pastSettle()
	h.tick()
	pr := h.pr(2)
	if runs := h.fixRuns(pr.ID); len(runs) != 1 {
		t.Fatalf("fix runs = %d, want the CI fix's", len(runs))
	}
	for prefix, want := range map[string]int{"first_parents:": 1, "head_runs:": 1, "run_jobs:": 1, "job_log:": 2} {
		if n := len(h.ghCalls(prefix)); n != want {
			t.Errorf("%s calls = %d, want %d", prefix, n, want)
		}
	}
	dir := h.d.Layout.ReviewDir("talkable", "talkable", 2, fixHead)
	for _, name := range []string{ciFailuresFile, ciFailuresMD} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	h.ag.mu.Lock()
	inputs := h.ag.fixInputs
	h.ag.mu.Unlock()
	if len(inputs) != 1 || !inputs[0].CIOnly || inputs[0].CIFile != filepath.Join(dir, ciFailuresFile) || inputs[0].ExternalFile != "" {
		t.Fatalf("fix inputs = %+v, want the CI file and no external file", inputs)
	}
	rec := h.ciRecordOf()
	if rec.Read.Digest == "" || rec.Read.Jobs != 2 || len(rec.Starts) != 1 {
		t.Fatalf("record = %+v", rec)
	}
	raw, _ := kvValue(h, kvPRCIFix(pr.ID))
	start := h.dispatchData("fix.start")
	if start["mode"] != FixModeCI || start["review_run"] != nil || start["ci_jobs"] != float64(2) {
		t.Errorf("fix.start data = %v", start)
	}
	for _, kind := range []string{"fix.ci_read", "fix.start", "fix.ended", "autofix.open"} {
		ev, ok := h.fixEvent(kind)
		if !ok {
			t.Fatalf("no %s event", kind)
		}
		for _, name := range []string{"rspec", "capybara", "Run tests", "Failure line"} {
			if strings.Contains(ev.Message, name) || strings.Contains(string(ev.Data), name) || strings.Contains(raw, name) {
				t.Errorf("%s or the record names %q: %s %s", kind, name, ev.Message, ev.Data)
			}
		}
	}
	if s := rec.Starts[0]; s.Head != fixHead || s.Outcome != ciOutcomeCannot || s.Why != "flaky" || s.By != autofixBy ||
		strings.Join(s.Entries, ",") != "Completion" {
		t.Errorf("start = %+v, want a cannot of class flaky", s)
	}
	if ev, _ := h.fixEvent("fix.ended"); ev.Message != "the CI fix ends: the CI fix found a flaky test, not the PR's code" {
		t.Errorf("fix.ended = %q", ev.Message)
	}
	h.wantState(2, store.PRReviewed)
	h.tick()
	h.tick()
	v := h.wantOpen(OpenNeedsYou, "required check Completion failed at a1a1a1a: the CI fix found a flaky test, not the PR's code")
	if len(v.Needs) != 1 || v.Needs[0].Kind != "ci" {
		t.Errorf("needs = %+v", v.Needs)
	}
	if n := len(h.fixRuns(pr.ID)); n != 1 || len(h.ghCalls("job_log:")) != 2 || len(h.ghCalls("first_parents:")) != 1 {
		t.Errorf("after the CI fix: %d fix runs, %d log reads, %d base reads; want nothing again at the head", n,
			len(h.ghCalls("job_log:")), len(h.ghCalls("first_parents:")))
	}
}

// The CI fix is recorded before its start: a start the fix job refuses at
// once (here someone typed into the PR's panes) is marked unstarted and
// does not count; CI fixes recorded with no loop (a crash between the
// record and the start) count, so a restart never starts a fourth.
func TestTheCIFixRecordIsWrittenBeforeTheStart(t *testing.T) {
	h, _ := ciFixHarness(t, github.CheckPassed)
	h.pastSettle()
	pr := h.pr(2)
	typed := h.clock.Now()
	if err := h.st.TransitionPR(h.ctx, pr.ID, []string{pr.State}, pr.State, func(u *store.PRUpdate) { u.Set("human_active_at", typed) }); err != nil {
		t.Fatal(err)
	}
	h.tick()
	rec := h.ciRecordOf()
	if len(rec.Starts) != 1 || rec.Starts[0].Outcome != ciOutcomeUnstarted || rec.Count() != 0 {
		t.Fatalf("record = %+v, want one unstarted start", rec.Starts)
	}
	h.wantOpen(OpenWaits, "someone typed into its panes")
	if len(h.fixRuns(pr.ID)) != 0 {
		t.Fatal("a fixer ran")
	}
	h.advance(6 * time.Minute) // the look asks again, still refused
	h.tick()
	if rec := h.ciRecordOf(); len(rec.Starts) != 1 || rec.Starts[0].Outcome != ciOutcomeUnstarted || !rec.Starts[0].At.After(typed) {
		t.Fatalf("record = %+v, want the one unstarted start: a refused start takes its place", rec.Starts)
	}
	if runs, logs := len(h.ghCalls("run_jobs:")), len(h.ghCalls("job_log:")); runs != 1 || logs != 2 {
		t.Errorf("a start that waits read the jobs %d times and %d logs; want the first look's one read", runs, logs)
	}
	lost := ciFixRecord{Read: rec.Read, Base: rec.Base}
	for _, head := range []string{"1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222", "3333333333333333333333333333333333333333"} {
		lost.Starts = append(lost.Starts, ciFixStart{Head: head, Entries: []string{"Completion"}, At: typed})
	}
	b, err := json.Marshal(lost)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetKV(h.ctx, kvPRCIFix(pr.ID), string(b)); err != nil {
		t.Fatal(err)
	}
	h.advance(2 * time.Hour) // past the cooldown
	h.tick()
	h.wantOpen(OpenNeedsYou, "required check Completion failed at a1a1a1a after 3 CI fixes: the CI is yours")
	if len(h.fixRuns(pr.ID)) != 0 || len(h.ciRecordOf().Starts) != 3 {
		t.Fatal("a fourth CI fix started")
	}
}

// A CI fix's push goes to its forced re-review like any fixer's push (the
// verdict says it is the CI fix's), its outcome is recorded at once, and
// the loop's next cycle, on the re-review's findings, fixes them in
// findings mode: the CI fix is the first cycle alone.
func TestACIFixPushGoesToItsForcedReReviewAndTheNextCycleFixesFindings(t *testing.T) {
	var mu sync.Mutex
	rounds := 0
	h, git := ciFixHarnessWith(t, github.CheckPassed, func(pipeline.RoundInput) (map[string]int, string) {
		mu.Lock()
		defer mu.Unlock()
		if rounds++; rounds == 2 {
			return map[string]int{"P2": 1}, "COMMENTED" // the CI fix's push brings a finding
		}
		return map[string]int{}, "COMMENTED"
	})
	cycles := 0
	fixerBy(h, git, func(run store.Run) (string, string) {
		if cycles++; cycles == 1 {
			return fixPushed, fixReport(run.ID, "b2b2b2b Keep the total in cents", "0 threads", "none")
		}
		return "", fixReport(run.ID, "nothing", fixReplied, "none")
	})
	h.pastSettle()
	h.tick() // the CI fix runs and pushes
	rec := h.ciRecordOf()
	if len(rec.Starts) != 1 || rec.Starts[0].Outcome != ciOutcomePushed || rec.Starts[0].Pushed != fixPushed {
		t.Fatalf("record = %+v, want the push recorded", rec.Starts)
	}
	st, ok := h.fixStateOf(2)
	if !ok || st.Phase != FixPhaseReviewing || st.Mode != FixModeCI {
		t.Fatalf("loop = %+v, %v; want the CI fix's push in its re-review", st, ok)
	}
	h.e.autofixOpen(h.ctx)
	h.wantOpen(OpenFixing, "fixing, re-review of b2b2b2b (the CI fix's push)")
	for range 4 {
		h.advance(time.Minute)
		h.tick()
	}
	h.ag.mu.Lock()
	inputs := slices.Clone(h.ag.fixInputs)
	h.ag.mu.Unlock()
	if len(inputs) != 2 || !inputs[0].CIOnly || inputs[1].CIOnly || inputs[1].CIFile != "" {
		t.Fatalf("fix inputs = %+v, want the CI fix, then a cycle on the re-review's findings", inputs)
	}
	if ev, ok := h.fixEventOf(2, "fix.start"); !ok || strings.Contains(string(ev.Data), `"mode"`) {
		t.Errorf("the second cycle's fix.start = %s, want no mode", ev.Data)
	}
	if rec := h.ciRecordOf(); rec.Count() != 1 || rec.Starts[0].Outcome != ciOutcomePushed {
		t.Errorf("record = %+v: the loop's end keeps the CI fix's push", rec.Starts)
	}
}

// The daemon's push of a merge of the parent (a loop's first cycle, a CI
// fix's included, merges a base tip the head lacks first) is magnum's, not
// the operator's: its head is the fixer's, so hands_off_after_push does
// not hold the next start on it with "you pushed".
func TestADaemonsMergeOfTheParentIsNoPushOfYours(t *testing.T) {
	h, _ := ciFixHarness(t, github.CheckPassed)
	repo := store.Repo{Owner: "talkable", Name: "talkable"}
	for kind, merge := range map[string]string{"fix.parent_merged": fixPushed, "fix.parent_pushed": strings.Repeat("d", 40)} {
		h.e.event(h.ctx, "info", prSubject(repo, 2), kind, "magnum merged main into the PR", map[string]any{"merge": merge, "head": fixHead})
	}
	l := openLedger{Since: h.clock.Now().Add(-time.Hour)}
	h.e.openLoopsEnded(h.ctx, repo, h.pr(2), &l, false)
	if !slices.Contains(l.FixerHeads, fixPushed) || !slices.Contains(l.FixerHeads, strings.Repeat("d", 40)) {
		t.Errorf("fixer heads = %q, want the daemon's merges", l.FixerHeads)
	}
}

// A CI fix whose claim lost its free slot to another PR ran no fixer: its
// start is unstarted, it does not count, and the next look starts the CI
// fix again in its place.
func TestACIFixWhoseClaimLostItsSlotDoesNotCount(t *testing.T) {
	h, git := ciFixHarness(t, github.CheckPassed)
	cannotFixer(h, git, "flaky")
	pr := h.pr(2)
	repo, err := h.st.RepoByID(h.ctx, pr.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if !h.e.noteCIFixStart(h.ctx, pr, []string{"Completion"}, autofixBy) {
		t.Fatal("the start was not recorded")
	}
	job := &fixJob{repo: repo, watch: h.cfg.Watches[0], pr: pr, mode: FixModeCI,
		state: FixState{Cycle: 1, Cap: 3, By: autofixBy, Since: fixHead, Mode: FixModeCI, CIChecks: []string{"Completion"}, StartedAt: h.clock.Now()}}
	h.e.endFixUnclaimed(h.ctx, job, noFreeFixSlot{repo: "talkable/talkable", label: "talkable/talkable#2"}.Error(), true)
	if rec := h.ciRecordOf(); len(rec.Starts) != 1 || rec.Starts[0].Outcome != ciOutcomeUnstarted || rec.Count() != 0 {
		t.Fatalf("record = %+v, want the start unstarted", rec.Starts)
	}
	h.pastSettle()
	h.tick()
	if rec := h.ciRecordOf(); len(h.fixRuns(pr.ID)) != 1 || len(rec.Starts) != 1 || rec.Count() != 1 {
		t.Errorf("record = %+v, %d fix runs; want the CI fix started in the unstarted one's place", rec.Starts, len(h.fixRuns(pr.ID)))
	}
}

// `magnum abort` of a CI fix's fixer records it stopped, and the head's
// verdict says so; nothing starts again at the head.
func TestAnAbortedCIFixIsStopped(t *testing.T) {
	var kh *keyHerdr
	h, _ := ciFixHarnessWith(t, github.CheckPassed, findings(map[string]int{}, "COMMENTED"), realSleep, withKeys(&kh))
	h.fix(FixPayload{Mode: FixModeCI})
	h.e.handleRequests(h.ctx)
	h.awaitSubmit()
	id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.e.handleRequests(h.ctx)
	h.settle()
	h.wantDone(id)
	if rec := h.ciRecordOf(); len(rec.Starts) != 1 || rec.Starts[0].Outcome != ciOutcomeStopped || rec.Count() != 1 {
		t.Fatalf("record = %+v, want the CI fix stopped", rec.Starts)
	}
	h.pastSettle()
	h.tick()
	h.tick()
	h.wantOpen(OpenNeedsYou, "required check Completion failed at a1a1a1a: the CI fix at a1a1a1a was stopped; it is yours")
	if n := len(h.fixRuns(h.pr(2).ID)); n != 1 {
		t.Errorf("fix runs = %d, want the stopped one alone", n)
	}
}

// A CI fix whose run failed (here its fixer wrote no report) is recorded
// failed, and counts.
func TestACIFixWhoseRunFailedIsFailed(t *testing.T) {
	h, git := ciFixHarness(t, github.CheckPassed)
	fixer(h, git, "", func(store.Run) string { return "" })
	id := h.fix(FixPayload{Mode: FixModeCI})
	h.handle()
	h.wantDone(id)
	if rec := h.ciRecordOf(); len(rec.Starts) != 1 || rec.Starts[0].Outcome != ciOutcomeFailed || rec.Count() != 1 {
		t.Fatalf("record = %+v, want the CI fix failed", rec.Starts)
	}
	h.wantState(2, store.PRNeedsAttention)
}

// A CI fix whose fixer was at work when the daemon stopped is recorded lost
// by the restart, and counts: its end is not known.
func TestACIFixLostInARestartIsLost(t *testing.T) {
	h, _ := ciFixHarness(t, github.CheckPassed)
	pr := h.pr(2)
	if !h.e.noteCIFixStart(h.ctx, pr, []string{"Completion"}, autofixBy) {
		t.Fatal("the start was not recorded")
	}
	if err := h.e.setFixState(h.ctx, pr.ID, FixState{Cycle: 1, Cap: 3, Phase: FixPhaseFixing, Head: fixHead, Since: fixHead, By: autofixBy,
		Mode: FixModeCI, CIChecks: []string{"Completion"}}); err != nil {
		t.Fatal(err)
	}
	h.startup()
	if rec := h.ciRecordOf(); len(rec.Starts) != 1 || rec.Starts[0].Outcome != ciOutcomeLost || rec.Count() != 1 {
		t.Fatalf("record = %+v, want the CI fix lost", rec.Starts)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the lost loop goes on")
	}
}
