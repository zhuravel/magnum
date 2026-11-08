package github

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// RecentRuns lists a workflow's newest runs of any event and branch, each
// with the commit and the branch it ran on: whether a run of a PR's branch
// is in progress, and whether one ran at a head, is read from it without
// the branch's name reaching the command line (a failed command is logged
// with its arguments, and a branch is the PR's text).
func TestRecentRunsListsAWorkflowsNewestRunsWithTheirBranch(t *testing.T) {
	sha := strings.Repeat("a", 40)
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Result: execx.Result{Stdout: compact(t, `{"total_count": 2, "workflow_runs": [
		{"id": 9, "event": "workflow_dispatch", "head_sha": "`+sha+`", "head_branch": "feature/x", "status": "in_progress", "conclusion": null,
		 "html_url": "https://github.com/talkable/talkable/actions/runs/9", "created_at": "2026-11-08T15:00:00Z"},
		{"id": 8, "event": "push", "head_sha": "`+sha+`", "head_branch": "master", "status": "completed", "conclusion": "success",
		 "html_url": "https://github.com/talkable/talkable/actions/runs/8", "created_at": "2026-11-08T14:00:00Z"}]}`)}}}}
	runs, err := (&Client{Run: f}).RecentRuns(context.Background(), "talkable", "talkable", "ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	want := []WorkflowRun{
		{ID: 9, Status: "in_progress", URL: "https://github.com/talkable/talkable/actions/runs/9", CreatedAt: time.Date(2026, 11, 8, 15, 0, 0, 0, time.UTC),
			HeadSHA: sha, HeadBranch: "feature/x", Event: "workflow_dispatch"},
		{ID: 8, Status: "completed", Conclusion: "success", URL: "https://github.com/talkable/talkable/actions/runs/8", CreatedAt: time.Date(2026, 11, 8, 14, 0, 0, 0, time.UTC),
			HeadSHA: sha, HeadBranch: "master", Event: "push"},
	}
	if !reflect.DeepEqual(runs, want) {
		t.Errorf("runs = %+v, want %+v", runs, want)
	}
	if wantArgs := []string{"api", "repos/talkable/talkable/actions/workflows/ci.yml/runs?exclude_pull_requests=true&per_page=50",
		"--hostname", "github.com"}; !reflect.DeepEqual(f.Calls[0].Args, wantArgs) {
		t.Errorf("args = %q, want %q", f.Calls[0].Args, wantArgs)
	}
	if _, err := (&Client{Run: &execx.Fake{}}).RecentRuns(context.Background(), "talkable", "talkable", "../ci.yml"); err == nil {
		t.Error("a workflow outside .github/workflows was asked for")
	}
}
