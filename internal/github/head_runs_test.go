package github

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// HeadRuns lists every workflow's runs at a commit (GET .../actions/runs
// filtered by head_sha), newest first, with their attempt, event and
// workflow file: a run that started no job is there, though its check
// suite holds no check run and a PR's rollup cannot show it (the live case
// of 2026-11-09, run 42673098082 of ci.yml). A run of another commit is
// left out, a path outside .github/workflows (GitHub's dynamic runs) names
// no workflow, and a sha that is no full commit id is refused before any
// call.
func TestHeadRunsListsEveryWorkflowsRunsAtACommitWithTheirAttempt(t *testing.T) {
	sha, other := strings.Repeat("a", 40), strings.Repeat("b", 40)
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Result: execx.Result{Stdout: compact(t, `{"total_count": 3, "workflow_runs": [
		{"id": 42673098082, "event": "pull_request", "head_sha": "`+sha+`", "path": ".github/workflows/ci.yml", "status": "completed",
		 "conclusion": "failure", "run_attempt": 1, "html_url": "https://github.com/talkable/talkable/actions/runs/42673098082",
		 "created_at": "2026-11-09T18:19:58Z"},
		{"id": 7, "event": "dynamic", "head_sha": "`+sha+`", "path": "dynamic/copilot-pull-request-reviewer/copilot", "status": "completed",
		 "conclusion": "success", "run_attempt": 1, "html_url": "https://github.com/talkable/talkable/actions/runs/7", "created_at": "2026-11-09T18:19:00Z"},
		{"id": 6, "event": "push", "head_sha": "`+other+`", "path": ".github/workflows/ci.yml", "status": "queued", "conclusion": null,
		 "run_attempt": 1, "html_url": "https://github.com/talkable/talkable/actions/runs/6", "created_at": "2026-11-09T18:18:00Z"}]}`)}}}}
	runs, err := (&Client{Run: f}).HeadRuns(context.Background(), "talkable", "talkable", sha)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"api", "repos/talkable/talkable/actions/runs?head_sha=" + sha + "&exclude_pull_requests=true&per_page=20", "--hostname", "github.com"}
	if cmd := f.Calls[0]; !reflect.DeepEqual(cmd.Args, want) || cmd.Mutates {
		t.Errorf("args = %q, mutates = %v", cmd.Args, cmd.Mutates)
	}
	wantRuns := []WorkflowRun{
		{ID: 42673098082, Status: "completed", Conclusion: "failure", URL: "https://github.com/talkable/talkable/actions/runs/42673098082",
			CreatedAt: time.Date(2026, 11, 9, 18, 19, 58, 0, time.UTC), HeadSHA: sha, Event: "pull_request", Attempt: 1, Workflow: "ci.yml"},
		{ID: 7, Status: "completed", Conclusion: "success", URL: "https://github.com/talkable/talkable/actions/runs/7",
			CreatedAt: time.Date(2026, 11, 9, 18, 19, 0, 0, time.UTC), HeadSHA: sha, Event: "dynamic", Attempt: 1},
	}
	if !reflect.DeepEqual(runs, wantRuns) {
		t.Errorf("runs = %+v, want %+v", runs, wantRuns)
	}
	for _, bad := range []string{"abc1234", strings.Repeat("A", 40), ""} {
		f := &execx.Fake{}
		if _, err := (&Client{Run: f}).HeadRuns(context.Background(), "talkable", "talkable", bad); err == nil || len(f.Calls) != 0 {
			t.Errorf("HeadRuns(%q) = %v after %d calls, want a refusal before any", bad, err, len(f.Calls))
		}
	}
}

// RerunRun re-runs a run (POST .../actions/runs/<id>/rerun) as the
// client's identity, a new attempt of the same run, marked Mutates, so a
// dry run only plans it; a run id that is no id is refused before any call.
func TestRerunRunPostsTheRerun(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "-X", "POST"}}}}
	if err := (&Client{Run: f}).RerunRun(context.Background(), "talkable", "talkable", 42673098082); err != nil {
		t.Fatal(err)
	}
	want := []string{"api", "-X", "POST", "repos/talkable/talkable/actions/runs/42673098082/rerun", "--hostname", "github.com"}
	if cmd := f.Calls[0]; !reflect.DeepEqual(cmd.Args, want) || !cmd.Mutates {
		t.Errorf("args = %q, mutates = %v", cmd.Args, cmd.Mutates)
	}
	dry := &execx.DryRun{Inner: &execx.Fake{}}
	if err := (&Client{Run: dry}).RerunRun(context.Background(), "talkable", "talkable", 1); err != nil || len(dry.Planned) != 1 {
		t.Errorf("dry run: %v, planned %d", err, len(dry.Planned))
	}
	for _, bad := range []int64{0, -1} {
		f := &execx.Fake{}
		if err := (&Client{Run: f}).RerunRun(context.Background(), "talkable", "talkable", bad); err == nil || len(f.Calls) != 0 {
			t.Errorf("RerunRun(%d) = %v after %d calls, want a refusal before any", bad, err, len(f.Calls))
		}
	}
}
