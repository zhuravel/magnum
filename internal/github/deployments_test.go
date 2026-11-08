package github

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// deploymentsAnswer is what `gh api --include` prints for a 200: the status
// line and headers (CRLF, as gh writes them), a blank line, the body.
const deploymentsAnswer = "HTTP/2.0 200 OK\r\nContent-Type: application/json; charset=utf-8\r\nEtag: W/\"d1\"\r\n" +
	"X-Ratelimit-Remaining: 4990\r\n\r\n" +
	`[{"id": 12, "sha": "` + "1111111111111111111111111111111111111111" + `", "ref": "staging-void", "environment": "void",
	   "created_at": "2026-11-08T10:00:00Z", "payload": {}, "description": "a description magnum never reads"},
	  {"id": 11, "sha": "` + "2222222222222222222222222222222222222222" + `", "ref": "2222222222222222222222222222222222222222",
	   "environment": "void", "created_at": "2026-11-07T10:00:00Z"}]`

// Deployments reads an environment's deployments newest first with gh's
// --include, so the answer's ETag is read, and sends the last ETag as
// If-None-Match: a 304 says not modified, and nothing is decoded.
func TestDeploymentsAreReadWithTheirETag(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Fn: func(c execx.Cmd) (execx.Result, error) {
		if slices.Contains(c.Args, `If-None-Match: W/"d1"`) {
			return failResult(c, []byte("HTTP/2.0 304 Not Modified\r\nEtag: W/\"d1\"\r\n\r\n"), []byte("gh: HTTP 304\n"))
		}
		return okResult([]byte(deploymentsAnswer))
	}}}}
	c := &Client{Run: f}
	list, etag, notModified, err := c.Deployments(context.Background(), "talkable", "talkable", "void", "")
	if err != nil || notModified {
		t.Fatalf("Deployments: %v, not modified %v", err, notModified)
	}
	want := []Deployment{
		{ID: 12, SHA: strings.Repeat("1", 40), Ref: "staging-void", Environment: "void", CreatedAt: time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)},
		{ID: 11, SHA: strings.Repeat("2", 40), Ref: strings.Repeat("2", 40), Environment: "void", CreatedAt: time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)},
	}
	if !reflect.DeepEqual(list, want) || etag != `W/"d1"` {
		t.Errorf("Deployments = %+v, %q; want %+v, W/\"d1\"", list, etag, want)
	}
	args := f.Calls[0].Args
	if wantArgs := []string{"api", "repos/talkable/talkable/deployments?environment=void&per_page=20", "--hostname", "github.com", "--include"}; !reflect.DeepEqual(args, wantArgs) || f.Calls[0].Mutates {
		t.Errorf("args = %q, want %q (no If-None-Match without an ETag)", args, wantArgs)
	}

	list, etag, notModified, err = c.Deployments(context.Background(), "talkable", "talkable", "void", `W/"d1"`)
	if err != nil || !notModified || list != nil || etag != `W/"d1"` {
		t.Errorf("with the ETag: %+v, %q, not modified %v, %v; want a 304 with the same ETag", list, etag, notModified, err)
	}
	if got := f.Calls[1].Args; !slices.Contains(got, "-H") || !slices.Contains(got, `If-None-Match: W/"d1"`) {
		t.Errorf("args = %q, want If-None-Match", got)
	}
	if f.Calls[1].Expected == nil || !f.Calls[1].Expected(execx.Result{Code: 1, Stderr: []byte("gh: HTTP 304\n")}) {
		t.Error("a 304 is not an expected answer: it would be logged as a failure")
	}
}

// A token without Deployments: read gets a 403 or a 404, which the client
// reports as ErrForbidden or ErrNotFound; a bad environment or ETag is
// refused before any call.
func TestDeploymentsRefusalsAndNotReadable(t *testing.T) {
	for status, want := range map[string]error{"403": ErrForbidden, "404": ErrNotFound} {
		f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Fn: func(c execx.Cmd) (execx.Result, error) {
			return failResult(c, []byte("HTTP/2.0 "+status+" x\r\n\r\n"+`{"message": "Resource not accessible by integration"}`),
				[]byte("gh: Resource not accessible by integration (HTTP "+status+")\n"))
		}}}}
		_, _, _, err := (&Client{Run: f}).Deployments(context.Background(), "talkable", "talkable", "void", "")
		if !errors.Is(err, want) {
			t.Errorf("HTTP %s: %v, want %v", status, err, want)
		}
	}
	for _, tc := range []struct{ env, etag string }{{"", ""}, {"void\nx", ""}, {"void", "W/\"a\"\r\nX: y"}} {
		f := &execx.Fake{}
		if _, _, _, err := (&Client{Run: f}).Deployments(context.Background(), "talkable", "talkable", tc.env, tc.etag); err == nil || len(f.Calls) != 0 {
			t.Errorf("Deployments(%q, %q) = %v after %d calls, want a refusal before any", tc.env, tc.etag, err, len(f.Calls))
		}
	}
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Result: execx.Result{Stdout: []byte("[]")}}}}
	if _, _, _, err := (&Client{Run: f}).Deployments(context.Background(), "talkable", "talkable", "eu west", ""); err != nil ||
		!strings.Contains(f.Calls[0].Args[1], "environment=eu+west&") {
		t.Errorf("an environment with a space: %v, path %q, want it escaped", err, f.Calls[0].Args[1])
	}
}

// DeploymentStatuses lists a deployment's status states, newest first.
func TestDeploymentStatusesListsTheStates(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Result: execx.Result{Stdout: compact(t, `[
		{"id": 3, "state": "inactive", "description": "never read"}, {"id": 2, "state": "success"}, {"id": 1, "state": "in_progress"}]`)}}}}
	states, err := (&Client{Run: f}).DeploymentStatuses(context.Background(), "talkable", "talkable", 12)
	if err != nil || !slices.Equal(states, []string{"inactive", "success", "in_progress"}) {
		t.Errorf("DeploymentStatuses = %q, %v", states, err)
	}
	if want := []string{"api", "repos/talkable/talkable/deployments/12/statuses?per_page=10", "--hostname", "github.com"}; !reflect.DeepEqual(f.Calls[0].Args, want) {
		t.Errorf("args = %q, want %q", f.Calls[0].Args, want)
	}
}

// CommitRuns lists a workflow's runs at a commit of any event (a push to a
// staging branch is one), newest first with their ids; a run of another
// commit is left out.
func TestCommitRunsListsAWorkflowsRunsAtACommit(t *testing.T) {
	sha, other := strings.Repeat("a", 40), strings.Repeat("b", 40)
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Result: execx.Result{Stdout: compact(t, `{"total_count": 3, "workflow_runs": [
		{"id": 9, "event": "push", "head_sha": "`+sha+`", "status": "completed", "conclusion": "failure",
		 "html_url": "https://github.com/talkable/talkable/actions/runs/9", "created_at": "2026-11-08T15:00:00Z"},
		{"id": 8, "event": "push", "head_sha": "`+other+`", "status": "completed", "conclusion": "success",
		 "html_url": "https://github.com/talkable/talkable/actions/runs/8", "created_at": "2026-11-08T14:00:00Z"},
		{"id": 7, "event": "workflow_dispatch", "head_sha": "`+sha+`", "status": "in_progress", "conclusion": null,
		 "html_url": "https://github.com/talkable/talkable/actions/runs/7", "created_at": "2026-11-08T13:00:00Z"}]}`)}}}}
	runs, err := (&Client{Run: f}).CommitRuns(context.Background(), "talkable", "talkable", "ci.yml", sha)
	if err != nil {
		t.Fatal(err)
	}
	want := []WorkflowRun{
		{ID: 9, Status: "completed", Conclusion: "failure", URL: "https://github.com/talkable/talkable/actions/runs/9", CreatedAt: time.Date(2026, 11, 8, 15, 0, 0, 0, time.UTC)},
		{ID: 7, Status: "in_progress", URL: "https://github.com/talkable/talkable/actions/runs/7", CreatedAt: time.Date(2026, 11, 8, 13, 0, 0, 0, time.UTC)},
	}
	if !reflect.DeepEqual(runs, want) {
		t.Errorf("runs = %+v, want %+v", runs, want)
	}
	if wantArgs := []string{"api", "repos/talkable/talkable/actions/workflows/ci.yml/runs?head_sha=" + sha + "&exclude_pull_requests=true&per_page=20",
		"--hostname", "github.com"}; !reflect.DeepEqual(f.Calls[0].Args, wantArgs) {
		t.Errorf("args = %q, want %q", f.Calls[0].Args, wantArgs)
	}
	for _, tc := range []struct{ workflow, sha string }{{"../ci.yml", sha}, {"ci.yml", "abc"}} {
		f := &execx.Fake{}
		if _, err := (&Client{Run: f}).CommitRuns(context.Background(), "talkable", "talkable", tc.workflow, tc.sha); err == nil || len(f.Calls) != 0 {
			t.Errorf("CommitRuns(%q, %q) = %v after %d calls, want a refusal before any", tc.workflow, tc.sha, err, len(f.Calls))
		}
	}
}

// RunJobs lists the jobs of a run's latest attempt; AnnotationPaths the
// paths a check run's annotations name, at most the limit, without repeats
// and without GitHub's ".github" of an annotation tied to no file. No
// message is read.
func TestRunJobsAndAnnotationPaths(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"gh", "api", "repos/talkable/talkable/actions/runs/9/jobs?filter=latest&per_page=100"}, Result: execx.Result{Stdout: compact(t, `{"total_count": 2, "jobs": [
			{"id": 101, "name": "rspec (3)", "status": "completed", "conclusion": "failure"},
			{"id": 102, "name": "rubocop", "status": "completed", "conclusion": "success"}]}`)}},
		{Prefix: []string{"gh", "api", "repos/talkable/talkable/check-runs/101/annotations?per_page=50"}, Result: execx.Result{Stdout: compact(t, `[
			{"path": "spec/models/a_spec.rb", "start_line": 3, "annotation_level": "failure", "message": "PR text magnum never reads"},
			{"path": "spec/models/a_spec.rb", "start_line": 9, "annotation_level": "failure", "message": "x"},
			{"path": "app/models/a.rb", "start_line": 1, "annotation_level": "failure", "message": "x"},
			{"path": ".github", "start_line": 1, "annotation_level": "failure", "message": "Process completed with exit code 1."}]`)}},
		{Prefix: []string{"gh", "api", "repos/talkable/talkable/check-runs/101/annotations?per_page=1"}, Result: execx.Result{Stdout: compact(t, `[
			{"path": "spec/models/a_spec.rb", "start_line": 3, "annotation_level": "failure", "message": "x"}]`)}},
	}}
	c := &Client{Run: f}
	jobs, err := c.RunJobs(context.Background(), "talkable", "talkable", 9)
	if err != nil {
		t.Fatal(err)
	}
	if want := []WorkflowJob{{ID: 101, Name: "rspec (3)", Status: "completed", Conclusion: "failure"},
		{ID: 102, Name: "rubocop", Status: "completed", Conclusion: "success"}}; !reflect.DeepEqual(jobs, want) {
		t.Errorf("jobs = %+v, want %+v", jobs, want)
	}
	paths, err := c.AnnotationPaths(context.Background(), "talkable", "talkable", 101, 50)
	if err != nil || !slices.Equal(paths, []string{"spec/models/a_spec.rb", "app/models/a.rb"}) {
		t.Errorf("AnnotationPaths = %q, %v", paths, err)
	}
	if paths, err := c.AnnotationPaths(context.Background(), "talkable", "talkable", 101, 1); err != nil || !slices.Equal(paths, []string{"spec/models/a_spec.rb"}) {
		t.Errorf("AnnotationPaths with a limit of 1 = %q, %v", paths, err)
	}
	if got := f.CallsWithPrefix("gh", "api", "repos/talkable/talkable/check-runs/101/annotations?per_page=1"); len(got) != 1 {
		t.Errorf("a limit of 1 reads %d pages of 1, want one", len(got))
	}
}
