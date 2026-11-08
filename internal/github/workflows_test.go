package github

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// DispatchWorkflow starts a workflow_dispatch run of a workflow file on a
// branch (POST .../actions/workflows/<file>/dispatches) as the client's
// identity. The branch, a PR's own text, goes as JSON on gh's stdin, never
// in argv (a failing call logs argv); the call is marked Mutates, so a dry
// run only plans it.
func TestDispatchWorkflowPostsTheBranchOnStdin(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "-X", "POST"}}}}
	c := &Client{Run: f, Env: map[string]string{"GH_CONFIG_DIR": "/state/gh/zhuravel"}}
	if err := c.DispatchWorkflow(context.Background(), "talkable", "talkable", "ci.yml", "feature/fix-it"); err != nil {
		t.Fatal(err)
	}
	cmd := f.Calls[0]
	want := []string{"api", "-X", "POST", "repos/talkable/talkable/actions/workflows/ci.yml/dispatches", "--hostname", "github.com", "--input", "-"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("args = %q", cmd.Args)
	}
	var sent map[string]any
	if err := json.Unmarshal(cmd.Stdin, &sent); err != nil || len(sent) != 1 || sent["ref"] != "feature/fix-it" {
		t.Errorf("stdin = %s, %v", cmd.Stdin, err)
	}
	if !cmd.Mutates || cmd.Env["GH_CONFIG_DIR"] != "/state/gh/zhuravel" {
		t.Errorf("mutates = %v, env = %v", cmd.Mutates, cmd.Env)
	}

	dry := &execx.DryRun{Inner: &execx.Fake{}}
	if err := (&Client{Run: dry}).DispatchWorkflow(context.Background(), "talkable", "talkable", "ci.yml", "main"); err != nil {
		t.Fatal(err)
	}
	if len(dry.Planned) != 1 {
		t.Errorf("planned = %d, want the dispatch planned and not run", len(dry.Planned))
	}
}

// A workflow GitHub will not start on the branch (its file there has no
// workflow_dispatch trigger) answers HTTP 422: an APIError with that status.
// A workflow that is not a plain file name of .github/workflows, or an empty
// branch, is refused before any call.
func TestDispatchWorkflowRefusals(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Result: execx.Result{
		Stdout: compact(t, `{"message":"Workflow does not have 'workflow_dispatch' trigger","documentation_url":"https://docs.github.com/rest/actions/workflows#create-a-workflow-dispatch-event","status":"422"}`),
		Stderr: []byte("gh: Workflow does not have 'workflow_dispatch' trigger (HTTP 422)\n"), Code: 1}}}}
	err := (&Client{Run: f}).DispatchWorkflow(context.Background(), "talkable", "talkable", "ci.yml", "main")
	var api *APIError
	if !errors.As(err, &api) || api.Status != 422 {
		t.Fatalf("err = %v, want an APIError of HTTP 422", err)
	}
	for _, tc := range []struct{ workflow, ref string }{
		{"../ci.yml", "main"}, {"ci", "main"}, {"sub/ci.yml", "main"}, {"", "main"}, {"ci.yml", ""}, {"ci.yml", " "},
	} {
		f := &execx.Fake{}
		if err := (&Client{Run: f}).DispatchWorkflow(context.Background(), "talkable", "talkable", tc.workflow, tc.ref); err == nil ||
			len(f.Calls) != 0 || !strings.HasPrefix(err.Error(), "github: ") {
			t.Errorf("DispatchWorkflow(%q, %q) = %v after %d calls, want a refusal before any", tc.workflow, tc.ref, err, len(f.Calls))
		}
	}
}

// DispatchedRuns lists the runs a workflow_dispatch started of a workflow
// file at a commit (GET .../actions/workflows/<file>/runs, filtered by the
// event and the commit), newest first, with their status, conclusion, page
// and creation time: a queued or running one is there before any of its
// checks. A run of another event or commit in the answer is left out, and a
// workflow that is not a plain file name of .github/workflows or a sha that
// is no full commit id is refused before any call.
func TestDispatchedRunsListsTheWorkflowsDispatchedRunsAtACommit(t *testing.T) {
	sha, other := strings.Repeat("a", 40), strings.Repeat("b", 40)
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Result: execx.Result{Stdout: compact(t, `{"total_count": 4, "workflow_runs": [
		{"id": 4, "event": "workflow_dispatch", "head_sha": "`+sha+`", "status": "queued", "conclusion": null,
		 "html_url": "https://github.com/talkable/talkable/actions/runs/4", "created_at": "2026-11-08T15:00:50Z"},
		{"id": 3, "event": "push", "head_sha": "`+sha+`", "status": "in_progress", "conclusion": null,
		 "html_url": "https://github.com/talkable/talkable/actions/runs/3", "created_at": "2026-11-08T14:59:00Z"},
		{"id": 2, "event": "workflow_dispatch", "head_sha": "`+other+`", "status": "in_progress", "conclusion": null,
		 "html_url": "https://github.com/talkable/talkable/actions/runs/2", "created_at": "2026-11-08T14:30:00Z"},
		{"id": 1, "event": "workflow_dispatch", "head_sha": "`+sha+`", "status": "completed", "conclusion": "cancelled",
		 "html_url": "https://github.com/talkable/talkable/actions/runs/1", "created_at": "2026-11-08T14:00:00Z"}]}`)}}}}
	runs, err := (&Client{Run: f}).DispatchedRuns(context.Background(), "talkable", "talkable", "ci.yml", sha)
	if err != nil {
		t.Fatal(err)
	}
	cmd := f.Calls[0]
	want := []string{"api", "repos/talkable/talkable/actions/workflows/ci.yml/runs?event=workflow_dispatch&head_sha=" + sha +
		"&exclude_pull_requests=true&per_page=20", "--hostname", "github.com"}
	if !reflect.DeepEqual(cmd.Args, want) || cmd.Mutates {
		t.Errorf("args = %q, mutates = %v", cmd.Args, cmd.Mutates)
	}
	wantRuns := []WorkflowRun{
		{Status: "queued", URL: "https://github.com/talkable/talkable/actions/runs/4", CreatedAt: time.Date(2026, 11, 8, 15, 0, 50, 0, time.UTC)},
		{Status: "completed", Conclusion: "cancelled", URL: "https://github.com/talkable/talkable/actions/runs/1",
			CreatedAt: time.Date(2026, 11, 8, 14, 0, 0, 0, time.UTC)},
	}
	if !reflect.DeepEqual(runs, wantRuns) {
		t.Errorf("runs = %+v, want %+v", runs, wantRuns)
	}
	if runs[0].Done() || !runs[1].Done() {
		t.Errorf("Done = %v, %v; want a queued run not done and a completed one done", runs[0].Done(), runs[1].Done())
	}
	for _, tc := range []struct{ workflow, sha string }{
		{"../ci.yml", sha}, {"ci", sha}, {"ci.yml", "abc1234"}, {"ci.yml", strings.Repeat("A", 40)}, {"ci.yml", ""},
	} {
		f := &execx.Fake{}
		if _, err := (&Client{Run: f}).DispatchedRuns(context.Background(), "talkable", "talkable", tc.workflow, tc.sha); err == nil ||
			len(f.Calls) != 0 || !strings.HasPrefix(err.Error(), "github: ") {
			t.Errorf("DispatchedRuns(%q, %q) = %v after %d calls, want a refusal before any", tc.workflow, tc.sha, err, len(f.Calls))
		}
	}
}
