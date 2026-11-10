package github

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// The CI reader (internal/engine's ci_failures.go) reads why a required
// check failed from the same jobs answer RunJobs reads: each job's steps,
// with their numbers, conclusions and times, the job's times, its page and
// its run's attempt. A step not started has no time.
func TestRunJobsReadsTheStepsAndTimes(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "repos/talkable/talkable/actions/runs/9/jobs?filter=latest&per_page=100"},
		Result: execx.Result{Stdout: compact(t, `{"total_count": 1, "jobs": [
			{"id": 101, "name": "rspec (3)", "status": "completed", "conclusion": "failure", "run_attempt": 2,
			 "html_url": "https://github.com/talkable/talkable/actions/runs/9/job/101",
			 "started_at": "2026-11-08T22:10:00Z", "completed_at": "2026-11-08T22:14:00Z",
			 "steps": [
				{"number": 1, "name": "Set up job", "status": "completed", "conclusion": "success",
				 "started_at": "2026-11-08T22:10:00Z", "completed_at": "2026-11-08T22:10:05Z"},
				{"number": 4, "name": "Run tests", "status": "completed", "conclusion": "failure",
				 "started_at": "2026-11-08T22:10:05Z", "completed_at": "2026-11-08T22:13:30Z"},
				{"number": 5, "name": "Upload", "status": "completed", "conclusion": "skipped", "started_at": null, "completed_at": null}]}]}`)}}}}
	jobs, err := (&Client{Run: f}).RunJobs(context.Background(), "talkable", "talkable", 9)
	if err != nil {
		t.Fatal(err)
	}
	at := func(hms string) time.Time { return time.Date(2026, 11, 8, 22, 0, 0, 0, time.UTC).Add(mustDur(t, hms)) }
	want := []WorkflowJob{{ID: 101, Name: "rspec (3)", Status: "completed", Conclusion: "failure", RunAttempt: 2,
		URL: "https://github.com/talkable/talkable/actions/runs/9/job/101", StartedAt: at("10m"), CompletedAt: at("14m"),
		Steps: []JobStep{
			{Number: 1, Name: "Set up job", Status: "completed", Conclusion: "success", StartedAt: at("10m"), CompletedAt: at("10m5s")},
			{Number: 4, Name: "Run tests", Status: "completed", Conclusion: "failure", StartedAt: at("10m5s"), CompletedAt: at("13m30s")},
			{Number: 5, Name: "Upload", Status: "completed", Conclusion: "skipped"},
		}}}
	if !reflect.DeepEqual(jobs, want) {
		t.Errorf("jobs =\n%+v\nwant\n%+v", jobs, want)
	}
}

func mustDur(t *testing.T, s string) time.Duration {
	t.Helper()
	d, err := time.ParseDuration(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// JobLogTail reads a job's log (GET .../actions/jobs/<id>/logs, which gh
// follows to the signed blob) and keeps its end, where the failure is: gh
// is asked for the escape sequences a coloured log holds (without the
// flag, gh 2.103 prints a refusal in place of the log) and keeps the last
// keep bytes (execx.Cmd.KeepTail). A log longer than that says it was cut;
// a log GitHub no longer has (404, 410 after 90 days) is GitHub's error,
// and a job id that is no id is refused before any call.
func TestJobLogTailAsksForEscapesAndKeepsTheEnd(t *testing.T) {
	log := "2026-11-08T22:10:00.0000000Z start\n" + strings.Repeat("2026-11-08T22:11:00.0000000Z noise\n", 100) + "2026-11-08T22:13:24.8041519Z \x1b[31mFAIL\x1b[0m the end\n"
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "repos/talkable/talkable/actions/jobs/101/logs"}, Fn: func(c execx.Cmd) (execx.Result, error) {
		return execx.Result{Stdout: []byte(log)}, nil
	}}}}
	got, cut, err := (&Client{Run: f}).JobLogTail(context.Background(), "talkable", "talkable", 101, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 200 || !strings.HasSuffix(got, "FAIL\x1b[0m the end\n") || !cut {
		t.Errorf("tail = %q (%d bytes), cut %v; want the last 200 bytes", got, len(got), cut)
	}
	cmd := f.Calls[0]
	wantArgs := []string{"api", "repos/talkable/talkable/actions/jobs/101/logs", "--hostname", "github.com", "--allow-escape-sequences"}
	if !reflect.DeepEqual(cmd.Args, wantArgs) || cmd.KeepTail != 200 || cmd.Mutates {
		t.Errorf("args = %q, keep tail %d, mutates %v", cmd.Args, cmd.KeepTail, cmd.Mutates)
	}
	whole, cut, err := (&Client{Run: f}).JobLogTail(context.Background(), "talkable", "talkable", 101, 1<<20)
	if err != nil || whole != log || cut {
		t.Errorf("a log within keep = %d bytes, cut %v, %v; want it whole", len(whole), cut, err)
	}
	gone := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Fn: func(c execx.Cmd) (execx.Result, error) {
		return failResult(c, []byte(`{"message":"Not Found","status":"404"}`), []byte("gh: Not Found (HTTP 404)"))
	}}}}
	if _, _, err := (&Client{Run: gone}).JobLogTail(context.Background(), "talkable", "talkable", 101, 200); !errors.Is(err, ErrNotFound) {
		t.Errorf("an expired log: %v, want ErrNotFound", err)
	}
	none := &execx.Fake{}
	if _, _, err := (&Client{Run: none}).JobLogTail(context.Background(), "talkable", "talkable", 0, 200); err == nil || len(none.Calls) != 0 {
		t.Errorf("job 0: %v after %d calls, want a refusal before any", err, len(none.Calls))
	}
}

// The CI fix's base read: one GraphQL query reads a commit and its first
// parents, four deep (five commits), each with its check rollup as
// CommitChecks reads one, newest first, for about the cost of one call and
// with no branch name on the command line; a commit with no rollup has no
// checks, and the list ends at the first commit with no parent.
func TestFirstParentChecksReadsFiveCommitsInOneQuery(t *testing.T) {
	oid := func(c byte) string { return strings.Repeat(string(c), 40) }
	node := func(sha, checks, parents string) string {
		return `{"oid":"` + sha + `","statusCheckRollup":` + checks + `,"parents":{"nodes":[` + parents + `]}}`
	}
	completion := func(conclusion string) string {
		return `{"contexts":{"nodes":[{"__typename":"CheckRun","name":"Completion","status":"COMPLETED","conclusion":"` + conclusion +
			`","startedAt":"2026-11-08T08:00:00Z","completedAt":"2026-11-08T08:30:00Z","checkSuite":{"workflowRun":{"workflow":{"name":"CI","resourcePath":"/talkable/talkable/actions/workflows/ci.yml"}}}}]}}`
	}
	tree := node(oid('a'), "null", node(oid('b'), completion("SUCCESS"), node(oid('c'), completion("FAILURE"), node(oid('d'), "null", ""))))
	var req gqlReq
	f := &execx.Fake{Rules: []execx.Rule{gqlRule(t, func(c execx.Cmd, r gqlReq) (execx.Result, error) {
		req = r
		return okResult(commitAnswer(t, tree))
	})}}
	got, err := (&Client{Run: f}).FirstParentChecks(context.Background(), "talkable", "talkable", oid('a'))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 8, 8, 30, 0, 0, time.UTC)
	check := func(state string) []Check {
		return []Check{{Name: "Completion", State: state, Workflow: "CI", WorkflowFile: "ci.yml", At: at}}
	}
	want := []CommitWithChecks{{SHA: oid('a'), Checks: []Check{}}, {SHA: oid('b'), Checks: check(CheckPassed)},
		{SHA: oid('c'), Checks: check(CheckFailed)}, {SHA: oid('d'), Checks: []Check{}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("commits =\n%+v\nwant\n%+v", got, want)
	}
	if n := len(f.Calls); n != 1 {
		t.Errorf("calls = %d, want one query", n)
	}
	q := oneLine(req.Query)
	if strings.Count(q, "parents(first: 1)") != FirstParentDepth-1 || strings.Count(q, "statusCheckRollup") != FirstParentDepth {
		t.Errorf("query reads %d parents and %d rollups, want %d commits:\n%s", strings.Count(q, "parents(first: 1)"),
			strings.Count(q, "statusCheckRollup"), FirstParentDepth, q)
	}
	for _, a := range f.Calls[0].Args {
		if strings.Contains(a, "master") || strings.Contains(a, oid('a')) {
			t.Errorf("argv carries %q: the commit goes in the query's variables alone", a)
		}
	}
	if req.Variables["oid"] != oid('a') {
		t.Errorf("variables = %v", req.Variables)
	}
	none := &execx.Fake{}
	if _, err := (&Client{Run: none}).FirstParentChecks(context.Background(), "talkable", "talkable", "master"); err == nil || len(none.Calls) != 0 {
		t.Errorf("a branch name: %v after %d calls, want a refusal before any", err, len(none.Calls))
	}
}
