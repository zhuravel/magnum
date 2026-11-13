package fixwork

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// fakeCIRead is GitHub as the CI reader reads it: the head's runs (or
// runsErr), each run's jobs (or its jobsErr) and each job's log tail, with
// the calls it got in their order.
type fakeCIRead struct {
	runs    []github.WorkflowRun
	runsErr error
	jobs    map[int64][]github.WorkflowJob
	jobsErr map[int64]error
	logs    map[int64]string
	calls   []string
}

func (f *fakeCIRead) HeadRuns(_ context.Context, owner, repo, sha string) ([]github.WorkflowRun, error) {
	f.calls = append(f.calls, fmt.Sprintf("head_runs:%s/%s@%s", owner, repo, sha))
	return f.runs, f.runsErr
}

func (f *fakeCIRead) RunJobs(_ context.Context, owner, repo string, runID int64) ([]github.WorkflowJob, error) {
	f.calls = append(f.calls, fmt.Sprintf("run_jobs:%s/%s#%d", owner, repo, runID))
	if err := f.jobsErr[runID]; err != nil {
		return nil, err
	}
	return f.jobs[runID], nil
}

func (f *fakeCIRead) JobLogTail(_ context.Context, owner, repo string, jobID int64, keep int) (string, bool, error) {
	f.calls = append(f.calls, fmt.Sprintf("job_log:%s/%s#%d keep %d", owner, repo, jobID, keep))
	log, ok := f.logs[jobID]
	if !ok {
		return "", false, github.ErrNotFound
	}
	return log, false, nil
}

// ciPR is the operator's PR #2 at fixHead, whose CI the reader reads.
var ciPR = store.PR{ID: 2, Number: 2, URL: prPage, HeadSHA: fixHead}

// ciRunPage is run id's page on GitHub.
func ciRunPage(id int64) string {
	return fmt.Sprintf("https://github.com/talkable/talkable/actions/runs/%d", id)
}

// ciRun is run id of workflow at fixHead, of event pull_request, created
// ago before openNow, at attempt in status (and conclusion, once
// completed).
func ciRun(id int64, workflow string, attempt int, status, conclusion string, ago time.Duration) github.WorkflowRun {
	return github.WorkflowRun{ID: id, Status: status, Conclusion: conclusion, URL: ciRunPage(id), CreatedAt: openNow.Add(-ago), HeadSHA: fixHead,
		Event: "pull_request", Attempt: attempt, Workflow: workflow}
}

// ciLogLine is a job log's line at 22:mm:ss on 2026-11-08, as GitHub
// writes one.
func ciLogLine(mmss, text string) string {
	return "2026-11-08T22:" + mmss + ".1234567Z " + text + "\n"
}

// ciStepTime is 2026-11-08 22:mm:ss.
func ciStepTime(t *testing.T, mmss string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, "2026-11-08T22:"+mmss+"Z")
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// completionCheck is talkable's required check Completion of ci.yml.
var completionCheck = CIFailedCheck{Entry: "Completion", Name: "Completion", WorkflowFile: "ci.yml"}

// wantNoFile fails unless the read gave no file, and GitHub got calls.
func wantNoFile(t *testing.T, f *fakeCIRead, file CIFailures, calls ...string) {
	t.Helper()
	if !reflect.DeepEqual(file, CIFailures{}) {
		t.Errorf("file = %+v, want none", file)
	}
	if !slices.Equal(f.calls, calls) {
		t.Errorf("calls = %q, want %q", f.calls, calls)
	}
}

// A failed required check is read to the end of its jobs' logs: the
// newest run of its workflow file at the head (by attempt; another
// workflow's run is not read), that run's jobs that failed, the check's
// own job last, each with its steps and failed steps, and each log's lines
// of the failed steps, by their times, without the terminal's escapes or
// the times (a job with no failed step keeps its whole tail). The read
// names the run at the attempt it read, and the file the base's word.
func TestTheCIReaderReadsAFailedCheckToItsJobsLogTail(t *testing.T) {
	jobPage := ciRunPage(ciRunID) + "/job/101"
	f := &fakeCIRead{
		runs: []github.WorkflowRun{ciRun(400, "ci.yml", 1, "completed", "failure", 3*time.Hour), ciRun(ciRunID, "ci.yml", 2, "completed", "failure", 2*time.Hour),
			ciRun(600, "lint.yml", 1, "completed", "failure", time.Hour)},
		jobs: map[int64][]github.WorkflowJob{ciRunID: {
			{ID: 100, Name: "Completion", Status: "completed", Conclusion: "failure"},
			{ID: 101, Name: "rspec (3)", Status: "completed", Conclusion: "failure", URL: jobPage, Steps: []github.JobStep{
				{Number: 1, Name: "Set up job", Status: "completed", Conclusion: "success", StartedAt: ciStepTime(t, "10:00"), CompletedAt: ciStepTime(t, "10:05")},
				{Number: 4, Name: "Run tests", Status: "completed", Conclusion: "failure", StartedAt: ciStepTime(t, "10:05"), CompletedAt: ciStepTime(t, "15:30")},
				{Number: 5, Name: "Post job cleanup", Status: "completed", Conclusion: "success", StartedAt: ciStepTime(t, "15:35"),
					CompletedAt: ciStepTime(t, "15:45")},
			}},
			{ID: 102, Name: "lint", Status: "completed", Conclusion: "success"},
		}},
		logs: map[int64]string{
			100: ciLogLine("16:00", "One of the jobs failed"),
			101: ciLogLine("10:01", "##[group]Run actions/checkout") + ciLogLine("10:06", "\x1b[31mexpected 2, got 3\x1b[0m") +
				ciLogLine("10:07", "rspec ./spec/models/user_spec.rb:12") + ciLogLine("15:40", "Post job cleanup"),
		},
	}
	check := completionCheck
	check.Base = CIBaseCheck{Name: "Completion", SHA: strings.Repeat("c", 40), State: CIBasePassed}
	read, file, err := ReadCIFailures(t.Context(), f, "talkable", "talkable", ciPR, []CIFailedCheck{check}, openNow)
	if err != nil {
		t.Fatal(err)
	}
	if want := (CIFailRead{Head: fixHead, At: openNow, Runs: []CIReadRun{{ID: ciRunID, Attempt: 2}}}); !reflect.DeepEqual(read, want) {
		t.Errorf("read = %+v, want %+v", read, want)
	}
	want := CIFailures{PR: prPage, Head: fixHead, ReadAt: openNow, Note: ciFailuresNote, Checks: []ciFailureJSON{{
		Entry: "Completion", Name: "Completion", WorkflowFile: "ci.yml", RunID: ciRunID, RunURL: ciRunPage(ciRunID), Attempt: 2, Base: check.Base,
		Jobs: []ciJobJSON{
			{ID: 101, Name: "rspec (3)", Conclusion: "failure", URL: jobPage,
				Steps:       []ciStepJSON{{N: 1, Name: "Set up job", Conclusion: "success"}, {N: 4, Name: "Run tests", Conclusion: "failure"}, {N: 5, Name: "Post job cleanup", Conclusion: "success"}},
				FailedSteps: []string{"Run tests"}, Log: []string{"expected 2, got 3", "rspec ./spec/models/user_spec.rb:12"}, LogCut: true},
			{ID: 100, Name: "Completion", Conclusion: "failure", Log: []string{"One of the jobs failed"}, PassedOnBase: true},
		},
	}}}
	if !reflect.DeepEqual(file, want) {
		t.Errorf("file = %+v\nwant %+v", file, want)
	}
	if calls := []string{"head_runs:talkable/talkable@" + fixHead, "run_jobs:talkable/talkable#500", "job_log:talkable/talkable#101 keep 1048576",
		"job_log:talkable/talkable#100 keep 1048576"}; !slices.Equal(f.calls, calls) {
		t.Errorf("calls = %q, want %q", f.calls, calls)
	}
}

// A failed check whose run's newest attempt runs (a re-run someone started
// by hand) makes the read Running: what failed is not known yet, so no
// job or log is read and no file comes.
func TestTheCIReaderWaitsForARunStillInProgress(t *testing.T) {
	f := &fakeCIRead{runs: []github.WorkflowRun{ciRun(ciRunID, "ci.yml", 2, "in_progress", "", time.Hour)}}
	read, file, err := ReadCIFailures(t.Context(), f, "talkable", "talkable", ciPR, []CIFailedCheck{completionCheck}, openNow)
	if err != nil {
		t.Fatal(err)
	}
	if want := (CIFailRead{Head: fixHead, At: openNow, Running: true}); !reflect.DeepEqual(read, want) {
		t.Errorf("read = %+v, want %+v", read, want)
	}
	wantNoFile(t, f, file, "head_runs:talkable/talkable@"+fixHead)
}

// A failed check of a run the look named that ran again since it failed
// makes the read Moved to the run that speaks for it now: someone's later
// start of the workflow magnum dispatched, or a re-run by hand of magnum's
// re-run (its id kept, a newer attempt). Nothing failed to read, so no job
// or log is read and no file comes.
func TestTheCIReaderMovesToTheRunThatSpeaksForTheCheckNow(t *testing.T) {
	dispatched := ciRun(ciRunID, "ci.yml", 1, "completed", "failure", time.Hour)
	dispatched.Event = "workflow_dispatch"
	later := ciRun(501, "ci.yml", 1, "in_progress", "", 10*time.Minute)
	later.Event = "workflow_dispatch"
	for _, tc := range []struct {
		name  string
		runs  []github.WorkflowRun
		check CIFailedCheck
		want  github.WorkflowRun
	}{
		{"a later start of the dispatched workflow", []github.WorkflowRun{dispatched, later},
			CIFailedCheck{Entry: "workflow:CI", WorkflowFile: "ci.yml", Run: ciRunID, Dispatched: true}, later},
		{"a re-run by hand of magnum's re-run", []github.WorkflowRun{ciRun(ciRunID, "ci.yml", 2, "completed", "success", time.Hour)},
			CIFailedCheck{Entry: "Completion", Name: "Completion", WorkflowFile: "ci.yml", Run: ciRunID}, ciRun(ciRunID, "ci.yml", 2, "completed", "success", time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCIRead{runs: tc.runs}
			read, file, err := ReadCIFailures(t.Context(), f, "talkable", "talkable", ciPR, []CIFailedCheck{tc.check}, openNow)
			if err != nil {
				t.Fatal(err)
			}
			if read.Moved == nil || !reflect.DeepEqual(*read.Moved, tc.want) {
				t.Fatalf("read moved to %+v, want %+v", read.Moved, tc.want)
			}
			if read.Head != fixHead || !read.At.Equal(openNow) || read.Running || len(read.Runs) != 0 {
				t.Errorf("read = %+v, want the head and the time alone beside the run", read)
			}
			wantNoFile(t, f, file, "head_runs:talkable/talkable@"+fixHead)
		})
	}
}

// A failed read of the head's runs returns the read as far as it went,
// the head and the time, with that error, and no file.
func TestAFailedReadOfTheRunsReturnsTheReadSoFarWithTheError(t *testing.T) {
	down := &github.APIError{Op: "head runs talkable/talkable", Status: 502, Message: "Bad Gateway"}
	f := &fakeCIRead{runsErr: down}
	read, file, err := ReadCIFailures(t.Context(), f, "talkable", "talkable", ciPR, []CIFailedCheck{completionCheck}, openNow)
	if !errors.Is(err, down) {
		t.Fatalf("ReadCIFailures = %v, want the runs' error", err)
	}
	if want := (CIFailRead{Head: fixHead, At: openNow}); !reflect.DeepEqual(read, want) {
		t.Errorf("read = %+v, want %+v", read, want)
	}
	wantNoFile(t, f, file, "head_runs:talkable/talkable@"+fixHead)
}

// A failed read of a run's jobs returns the read as far as it went, the
// runs it read up to that one, with that error, and no file: no log is
// read.
func TestAFailedReadOfARunsJobsReturnsTheReadSoFarWithTheError(t *testing.T) {
	refused := &github.APIError{Op: "run jobs talkable/talkable", Status: 403, Message: "Resource not accessible by integration"}
	f := &fakeCIRead{
		runs:    []github.WorkflowRun{ciRun(ciRunID, "ci.yml", 1, "completed", "failure", time.Hour), ciRun(600, "lint.yml", 1, "completed", "failure", time.Hour)},
		jobs:    map[int64][]github.WorkflowJob{ciRunID: {{ID: 100, Name: "Completion", Status: "completed", Conclusion: "failure"}}},
		jobsErr: map[int64]error{600: refused},
		logs:    map[int64]string{100: ciLogLine("16:00", "One of the jobs failed")},
	}
	checks := []CIFailedCheck{completionCheck, {Entry: "lint", Name: "lint", WorkflowFile: "lint.yml"}}
	read, file, err := ReadCIFailures(t.Context(), f, "talkable", "talkable", ciPR, checks, openNow)
	if !errors.Is(err, refused) {
		t.Fatalf("ReadCIFailures = %v, want the jobs' error", err)
	}
	if want := (CIFailRead{Head: fixHead, At: openNow, Runs: []CIReadRun{{ID: ciRunID, Attempt: 1}, {ID: 600, Attempt: 1}}}); !reflect.DeepEqual(read, want) {
		t.Errorf("read = %+v, want %+v", read, want)
	}
	wantNoFile(t, f, file, "head_runs:talkable/talkable@"+fixHead, "run_jobs:talkable/talkable#500", "run_jobs:talkable/talkable#600")
}
