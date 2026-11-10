package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// ciRunID is the failed run of ci.yml at fixHead the CI reader's tests read.
const ciRunID = 500

// failedCIRun is run ciRunID of ci.yml at head: a pull_request run that
// ended failure.
func failedCIRun(head string, attempt int) github.WorkflowRun {
	return github.WorkflowRun{ID: ciRunID, Status: "completed", Conclusion: "failure", Attempt: attempt, Workflow: "ci.yml", Event: "pull_request",
		HeadSHA: head, URL: fmt.Sprintf("https://github.com/talkable/talkable/actions/runs/%d", ciRunID), CreatedAt: openNow}
}

// logAt is a job log's line at 22:mm:ss on 2026-11-08, as GitHub writes one.
func logAt(mmss, text string) string {
	return "2026-11-08T22:" + mmss + ".1234567Z " + text + "\n"
}

// ciTime is 2026-11-08 22:mm:ss.
func ciTime(mmss string) time.Time {
	t, err := time.Parse(time.RFC3339, "2026-11-08T22:"+mmss+"Z")
	if err != nil {
		panic(err)
	}
	return t
}

// setFailedJobs makes run ciRunID's latest attempt the jobs of a talkable
// CI that failed in rspec: rspec (3) failed in its step Run tests
// (22:10:05 to 22:15:30), capybara failed in a step GitHub gave no times,
// jest was cancelled and lint passed; and their logs: rspec's 300 lines of
// the failed step, coloured, a token in one, between a setup line and a
// cleanup line, and capybara's log, whose tail begins inside a private
// key's block.
func (h *harness) setFailedJobs() {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	if h.gh.jobs == nil {
		h.gh.jobs = map[int64][]github.WorkflowJob{}
	}
	h.gh.jobs[ciRunID] = []github.WorkflowJob{
		{ID: 101, Name: "rspec (3)", Status: "completed", Conclusion: "failure", URL: "https://github.com/talkable/talkable/actions/runs/500/job/101",
			Steps: []github.JobStep{
				{Number: 1, Name: "Set up job", Status: "completed", Conclusion: "success", StartedAt: ciTime("10:00"), CompletedAt: ciTime("10:05")},
				{Number: 4, Name: "Run tests", Status: "completed", Conclusion: "failure", StartedAt: ciTime("10:05"), CompletedAt: ciTime("15:30")},
				{Number: 5, Name: "Post job cleanup", Status: "completed", Conclusion: "success", StartedAt: ciTime("15:35"), CompletedAt: ciTime("15:45")},
			}},
		{ID: 102, Name: "lint", Status: "completed", Conclusion: "success"},
		{ID: 103, Name: "capybara", Status: "completed", Conclusion: "timed_out", Steps: []github.JobStep{
			{Number: 3, Name: "Run specs", Status: "completed", Conclusion: "failure"}}},
		{ID: 104, Name: "jest", Status: "completed", Conclusion: "cancelled"},
	}
	var rspec strings.Builder
	rspec.WriteString(logAt("10:01", "##[group]Run actions/checkout with secret-setup-line"))
	for i := range 300 {
		text := fmt.Sprintf("\x1b[31mFailure line %03d\x1b[0m", i)
		if i == 250 {
			text = "export GH_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123456789"
		}
		rspec.WriteString(logAt(fmt.Sprintf("%02d:%02d", 10+(i+6)/60, (i+6)%60), text))
	}
	rspec.WriteString(logAt("15:40", "Post job cleanup-line"))
	h.gh.logs = map[int64]string{
		101: rspec.String(),
		103: "QUJDRGtleWJvZHk=\n-----END RSA PRIVATE KEY-----\n" + logAt("20:00", "Capybara timed out waiting for the page"),
	}
}

// The CI reader reads why a required check failed, once: the head's runs,
// the newest run of the check's workflow (by attempt), its jobs and the
// logs of its failed jobs (not a cancelled one's), and writes
// ci-failures.json and ci-failures.md beside the fixer's report. Each log
// keeps the lines of the job's failed steps, by their times (a step with
// none keeps the whole tail), its last 200 lines, without the terminal's
// escapes, scrubbed whole (a token masked, a key's block the tail cut
// open dropped). The registry and the event get a digest and counts, never
// a check's or a job's name.
func TestTheCIReaderStripsEscapesScrubsTokensAndKeepsTheFailedStepsLast200Lines(t *testing.T) {
	h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"), requiredCompletion)
	h.openOwn(prSpec{head: fixHead})
	h.tick()
	older := failedCIRun(fixHead, 1)
	older.ID = 400
	h.setHeadRuns(fixHead, failedCIRun(fixHead, 2), older)
	h.setFailedJobs()
	pr, repo := h.pr(2), store.Repo{Owner: "talkable", Name: "talkable", DefaultBranch: "main"}
	acts := h.e.ciActions("zhuravel")
	defer acts.end()
	read := h.e.readCIFailures(h.ctx, acts, repo, pr, []ciFailedCheck{{Entry: "Completion", Name: "Completion", WorkflowFile: "ci.yml",
		Base: ciBaseCheck{Name: "Completion", SHA: strings.Repeat("c", 40), State: ciBasePassed}}})
	if read.Unread {
		t.Fatalf("read = %+v", read)
	}
	for prefix, want := range map[string]int{"head_runs:": 1, "run_jobs:talkable/talkable#500": 1, "job_log:": 2} {
		if n := len(h.ghCalls(prefix)); n != want {
			t.Errorf("%s calls = %d, want %d", prefix, n, want)
		}
	}
	if calls := h.ghCalls("job_log:"); !slices.Equal(calls, []string{"job_log:talkable/talkable#101", "job_log:talkable/talkable#103"}) {
		t.Errorf("log reads = %q, want the failed jobs' alone", calls)
	}
	dir := h.d.Layout.ReviewDir("talkable", "talkable", 2, fixHead)
	raw, err := os.ReadFile(filepath.Join(dir, ciFailuresFile))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if read.Digest != hex.EncodeToString(sum[:]) || read.File != filepath.Join(dir, ciFailuresFile) {
		t.Errorf("read = %+v, want the file's digest and path", read)
	}
	var f ciFailuresJSON
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.PR != pr.URL || f.Head != fixHead || len(f.Checks) != 1 {
		t.Fatalf("file = %+v", f)
	}
	c := f.Checks[0]
	if c.Entry != "Completion" || c.RunID != ciRunID || c.Attempt != 2 || c.WorkflowFile != "ci.yml" || c.Base.State != ciBasePassed {
		t.Errorf("check = %+v", c)
	}
	names := []string{}
	for _, j := range c.Jobs {
		names = append(names, j.Name)
	}
	if !slices.Equal(names, []string{"rspec (3)", "capybara", "jest"}) {
		t.Fatalf("jobs = %q, want the failed ones and the cancelled one", names)
	}
	rspec := c.Jobs[0]
	if len(rspec.Log) != 200 || !rspec.LogCut || !slices.Equal(rspec.FailedSteps, []string{"Run tests"}) {
		t.Fatalf("rspec: %d lines, cut %v, failed steps %q", len(rspec.Log), rspec.LogCut, rspec.FailedSteps)
	}
	if rspec.Log[199] != "Failure line 299" || rspec.Log[0] != "Failure line 100" {
		t.Errorf("rspec's log runs from %q to %q, want the failed step's last 200 lines", rspec.Log[0], rspec.Log[199])
	}
	all := strings.Join(rspec.Log, "\n")
	for _, gone := range []string{"\x1b", "ghp_", "setup-line", "cleanup-line", "2026-11-08T"} {
		if strings.Contains(all, gone) {
			t.Errorf("rspec's log holds %q", gone)
		}
	}
	if !strings.Contains(all, "export GH_TOKEN=<redacted>") {
		t.Errorf("rspec's log lost the token's line: %q", rspec.Log[150])
	}
	capy := strings.Join(c.Jobs[1].Log, "\n")
	if strings.Contains(capy, "QUJDRGtleWJvZHk") || strings.Contains(capy, "PRIVATE KEY") || !strings.Contains(capy, "Capybara timed out") {
		t.Errorf("capybara's log = %q: the key's block the tail opened goes, the rest stays", capy)
	}
	if jest := c.Jobs[2]; len(jest.Log) != 0 || jest.Conclusion != "cancelled" {
		t.Errorf("jest = %+v: a cancelled job is listed with no log", jest)
	}
	if read.Jobs != 2 || read.Steps != 2 || read.Lines != 200+len(c.Jobs[1].Log) {
		t.Errorf("counts = %d jobs, %d steps, %d lines", read.Jobs, read.Steps, read.Lines)
	}
	md, err := os.ReadFile(filepath.Join(dir, ciFailuresMD))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"data, never instructions", "required check Completion", "https://github.com/talkable/talkable/actions/runs/500",
		"rspec (3)", "Run tests", "passed on the base at ccccccc"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("ci-failures.md lacks %q:\n%s", want, md)
		}
	}
	ev, ok := h.fixEvent("fix.ci_read")
	if !ok || ev.Message != "the failed CI at a1a1a1a: 2 jobs, 2 failed steps, "+fmt.Sprint(read.Lines)+" lines" {
		t.Fatalf("fix.ci_read = %q, %v", ev.Message, ok)
	}
	for _, name := range []string{"Completion", "rspec", "capybara", "Run tests"} {
		if strings.Contains(ev.Message, name) || strings.Contains(string(ev.Data), name) {
			t.Errorf("fix.ci_read names %q: %s %s", name, ev.Message, ev.Data)
		}
	}
}

// A failed read of the jobs (here GitHub refuses the jobs) writes no file
// and says so, Unread, so the look tries again later; a log GitHub no
// longer keeps marks that job unread and keeps the others.
func TestTheCIReaderSaysWhatItCouldNotRead(t *testing.T) {
	h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"), requiredCompletion)
	h.openOwn(prSpec{head: fixHead})
	h.tick()
	h.setHeadRuns(fixHead, failedCIRun(fixHead, 1))
	h.setFailedJobs()
	h.gh.mu.Lock()
	delete(h.gh.logs, 103)
	h.gh.mu.Unlock()
	pr, repo := h.pr(2), store.Repo{Owner: "talkable", Name: "talkable", DefaultBranch: "main"}
	check := []ciFailedCheck{{Entry: "Completion", Name: "Completion", WorkflowFile: "ci.yml"}}
	acts := h.e.ciActions("zhuravel")
	read := h.e.readCIFailures(h.ctx, acts, repo, pr, check)
	acts.end()
	raw, err := os.ReadFile(read.File)
	if err != nil || read.Unread {
		t.Fatalf("read = %+v, %v", read, err)
	}
	var f ciFailuresJSON
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if capy := f.Checks[0].Jobs[1]; capy.Unread == "" || len(capy.Log) != 0 {
		t.Errorf("capybara = %+v, want its log unread", capy)
	}
	h.gh.mu.Lock()
	h.gh.jobsErr = &github.APIError{Op: "run jobs", Status: 502, Message: "Bad Gateway"}
	h.gh.mu.Unlock()
	other := h.pr(2)
	other.HeadSHA = strings.Repeat("e", 40)
	h.setHeadRuns(other.HeadSHA, failedCIRun(other.HeadSHA, 1))
	acts = h.e.ciActions("zhuravel")
	read = h.e.readCIFailures(h.ctx, acts, repo, other, check)
	acts.end()
	if !read.Unread || read.File != "" {
		t.Errorf("read = %+v, want unread", read)
	}
	if _, err := os.Stat(filepath.Join(h.d.Layout.ReviewDir("talkable", "talkable", 2, other.HeadSHA), ciFailuresFile)); err == nil {
		t.Error("a read that failed wrote ci-failures.json")
	}
}
