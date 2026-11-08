package staging

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
)

// ghAnswers scripts gh: each path prefix of `gh api <path>` answers with a
// body, or, for a body "HTTP <status or message>", with gh's failure.
func ghAnswers(answers map[string]string) *execx.Fake {
	return &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Fn: func(c execx.Cmd) (execx.Result, error) {
		path := c.Args[1]
		best := ""
		for prefix := range answers {
			if strings.HasPrefix(path, prefix) && len(prefix) > len(best) {
				best = prefix
			}
		}
		if best == "" {
			return execx.Result{}, errors.New("no answer for " + path)
		}
		body := answers[best]
		if msg, ok := strings.CutPrefix(body, "HTTP "); ok { // gh's line: "gh: HTTP 304", "gh: Not Found (HTTP 404)"
			if _, err := strconv.Atoi(msg); err == nil {
				msg = "HTTP " + msg
			}
			stderr := "gh: " + msg + "\n"
			return execx.Result{Code: 1, Stderr: []byte(stderr)}, &execx.ExitError{Cmd: c, Code: 1, Stderr: stderr}
		}
		return execx.Result{Stdout: []byte(body)}, nil
	}}}}
}

func adapterOn(f *execx.Fake) GitHub {
	return NewGitHub(func(string) *github.Client { return &github.Client{Run: f} })
}

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// The adapter reads an environment's deployments with their ETag through
// magnum's GitHub client, and each one's statuses from the newest down to
// the first whose newest status is a success, which it marks (a success
// that a failure followed, a health check or a rollback, is none); none
// older is read.
func TestTheGitHubAdapterReadsDeploymentsAndTheNewestSuccess(t *testing.T) {
	t.Parallel()
	f := ghAnswers(map[string]string{
		"repos/acme/app/deployments?environment=void": "HTTP/2.0 200 OK\r\nEtag: W/\"e1\"\r\n\r\n" + `[
			{"id": 12, "sha": "` + shaA + `", "ref": "` + shaA + `", "created_at": "2026-11-08T10:00:00Z"},
			{"id": 11, "sha": "` + shaB + `", "ref": "staging-void", "created_at": "2026-11-08T09:00:00Z"},
			{"id": 10, "sha": "` + shaB + `", "ref": "staging-void", "created_at": "2026-11-08T08:00:00Z"}]`,
		"repos/acme/app/deployments/12/statuses": `[{"state": "failure"}, {"state": "success"}]`,
		"repos/acme/app/deployments/11/statuses": `[{"state": "success"}, {"state": "in_progress"}]`,
	})
	got, err := adapterOn(f).DeploymentsOf(context.Background(), "acme/app", "void", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.ETag != `W/"e1"` || got.NotModified || len(got.List) != 3 {
		t.Fatalf("DeploymentsOf = %+v", got)
	}
	if d := got.List[0]; d.ID != 12 || d.SHA != shaA || d.Success || !d.CreatedAt.Equal(time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("the newest = %+v, want 12 at A, not a success: its newest status is a failure", d)
	}
	if d := got.List[1]; d.ID != 11 || !d.Success || d.Ref != "staging-void" {
		t.Errorf("the next = %+v, want 11, a success: its newest status", d)
	}
	if n := len(f.CallsWithPrefix("gh", "api", "repos/acme/app/deployments/10/statuses?per_page=10")); n != 0 {
		t.Errorf("read the statuses of a deployment older than the newest success %d times, want none", n)
	}
}

// A 304 is not modified with no statuses read; a 403 or a 404 (a token
// without Deployments: read), of the list or of a status, is
// ErrNotReadable; a rate limit is not.
func TestTheGitHubAdapterTellsNotReadableFromNotModified(t *testing.T) {
	t.Parallel()
	f := ghAnswers(map[string]string{"repos/acme/app/deployments?environment=void": "HTTP 304"})
	got, err := adapterOn(f).DeploymentsOf(context.Background(), "acme/app", "void", `W/"e1"`)
	if err != nil || !got.NotModified || len(f.Calls) != 1 {
		t.Errorf("a 304: %+v, %v after %d calls; want not modified after one", got, err, len(f.Calls))
	}
	for name, answers := range map[string]map[string]string{
		"the list 403": {"repos/acme/app/deployments?environment=void": "HTTP Resource not accessible by integration (HTTP 403)"},
		"the list 404": {"repos/acme/app/deployments?environment=void": "HTTP Not Found (HTTP 404)"},
		"a status 403": {
			"repos/acme/app/deployments?environment=void": "HTTP/2.0 200 OK\r\n\r\n" + `[{"id": 12, "sha": "` + shaA + `", "created_at": "2026-11-08T10:00:00Z"}]`,
			"repos/acme/app/deployments/12/statuses":      "HTTP Resource not accessible by integration (HTTP 403)",
		},
	} {
		if _, err := adapterOn(ghAnswers(answers)).DeploymentsOf(context.Background(), "acme/app", "void", ""); !errors.Is(err, ErrNotReadable) {
			t.Errorf("%s: %v, want ErrNotReadable", name, err)
		}
	}
	limited := ghAnswers(map[string]string{"repos/acme/app/deployments?environment=void": "HTTP API rate limit exceeded for installation (HTTP 403)"})
	if _, err := adapterOn(limited).DeploymentsOf(context.Background(), "acme/app", "void", ""); err == nil || errors.Is(err, ErrNotReadable) {
		t.Errorf("a rate limit: %v, want an error that is not ErrNotReadable", err)
	}
}

// The adapter reads ci.yml's runs at a sha; for the newest, when it failed,
// the failed jobs with the paths their check runs' annotations name. A job
// whose annotations cannot be read gets no path: "cannot tell".
func TestTheGitHubAdapterReadsAFailedRunsJobsAndTheirAnnotationPaths(t *testing.T) {
	t.Parallel()
	f := ghAnswers(map[string]string{
		"repos/acme/app/actions/workflows/ci.yml/runs?head_sha=" + shaA: `{"workflow_runs": [
			{"id": 9, "head_sha": "` + shaA + `", "status": "completed", "conclusion": "failure",
			 "html_url": "https://github.com/acme/app/actions/runs/9", "created_at": "2026-11-08T15:00:00Z"},
			{"id": 8, "head_sha": "` + shaA + `", "status": "completed", "conclusion": "failure",
			 "html_url": "https://github.com/acme/app/actions/runs/8", "created_at": "2026-11-08T14:00:00Z"}]}`,
		"repos/acme/app/actions/runs/9/jobs": `{"jobs": [
			{"id": 101, "name": "rspec", "status": "completed", "conclusion": "failure"},
			{"id": 102, "name": "jest", "status": "completed", "conclusion": "failure"},
			{"id": 103, "name": "rubocop", "status": "completed", "conclusion": "success"}]}`,
		"repos/acme/app/check-runs/101/annotations?per_page=50": `[{"path": "spec/a_spec.rb", "message": "never read"}, {"path": ".github"}]`,
		"repos/acme/app/check-runs/102/annotations?per_page=50": "HTTP Server Error (HTTP 500)",
	})
	runs, err := adapterOn(f).RunsOf(context.Background(), "acme/app", Workflow, shaA)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != 9 || runs[0].URL != "https://github.com/acme/app/actions/runs/9" || runs[0].Conclusion != "failure" {
		t.Fatalf("runs = %+v", runs)
	}
	failed := runs[0].Failed
	if len(failed) != 2 || failed[0].Name != "rspec" || len(failed[0].Paths) != 1 || failed[0].Paths[0] != "spec/a_spec.rb" ||
		failed[1].Name != "jest" || len(failed[1].Paths) != 0 {
		t.Errorf("the newest run's failed jobs = %+v, want rspec naming spec/a_spec.rb and jest naming none", failed)
	}
	if runs[1].Failed != nil {
		t.Errorf("an older run's jobs were read: %+v", runs[1].Failed)
	}
	if n := len(f.CallsWithPrefix("gh", "api", "repos/acme/app/actions/runs/8/jobs?filter=latest&per_page=100")); n != 0 {
		t.Errorf("read an older run's jobs %d times, want none", n)
	}
	if ours, why := failureIsOurs(&runs[0], &pathsEntry{Paths: []string{"app/a.rb"}}, nil, ""); ours || !strings.Contains(why, "cannot tell") {
		t.Errorf("jest named no path: ours %v, why %q; want cannot tell", ours, why)
	}
	if _, err := NewGitHub(func(string) *github.Client { return nil }).RunsOf(context.Background(), "acme/app", Workflow, shaA); err == nil {
		t.Error("no client for the repository: no error")
	}
}
