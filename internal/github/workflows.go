package github

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// workflowFileRe is a workflow's file name in .github/workflows ("ci.yml"):
// no directory, no "..", a .yml or .yaml file.
var workflowFileRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*\.ya?ml$`)

// ValidWorkflowFile reports whether name is a workflow file DispatchWorkflow
// takes: a plain file name of .github/workflows ending in .yml or .yaml.
func ValidWorkflowFile(name string) bool {
	return workflowFileRe.MatchString(name) && !strings.Contains(name, "..")
}

// DispatchWorkflow starts a run of the workflow file workflow ("ci.yml") on
// ref, a branch of owner/repo, as the client's identity: POST
// /repos/{o}/{r}/actions/workflows/{file}/dispatches. GitHub runs the
// workflow as the branch has it, at its tip, and refuses (HTTP 422) one whose
// file there has no workflow_dispatch trigger; the identity needs the right
// to run workflows (a gh login that can push, an App with Actions: write).
// The branch, which a PR names, goes as JSON on gh's stdin, so a failing call
// logs none of it. A workflow that is not a plain file name of
// .github/workflows (ValidWorkflowFile) or an empty ref is refused before
// any call. It is marked Mutates, so execx.DryRun only plans it.
func (c *Client) DispatchWorkflow(ctx context.Context, owner, repo, workflow, ref string) error {
	if err := checkRepo(owner, repo); err != nil {
		return err
	}
	if !ValidWorkflowFile(workflow) {
		return fmt.Errorf("github: invalid workflow file %q", workflow)
	}
	if strings.TrimSpace(ref) == "" || strings.ContainsRune(ref, 0) {
		return fmt.Errorf("github: dispatching %s needs a branch", workflow)
	}
	path := fmt.Sprintf("repos/%s/%s/actions/workflows/%s/dispatches", owner, repo, workflow)
	op := fmt.Sprintf("dispatch %s/%s:%s", owner, repo, workflow)
	return c.restInput(ctx, op, "POST", path, map[string]string{"ref": ref}, true, nil)
}

// WorkflowRun is a run of a workflow as DispatchedRuns lists it: its status
// (queued, waiting, requested, pending, in_progress or completed), its
// conclusion once completed (success, failure, cancelled, timed_out, ...),
// its page and when GitHub created it.
type WorkflowRun struct {
	Status     string
	Conclusion string
	URL        string
	CreatedAt  time.Time
}

// Done reports whether r ended (completed, whatever its conclusion).
func (r WorkflowRun) Done() bool { return r.Status == "completed" }

// dispatchedRunsPage is how many runs DispatchedRuns reads: the newest of a
// commit's dispatched runs of one workflow, which magnum starts at most
// once per head.
const dispatchedRunsPage = 20

// DispatchedRuns lists the runs of the workflow file workflow ("ci.yml") of
// owner/repo that a workflow_dispatch started at commit sha, newest first:
// GET /repos/{o}/{r}/actions/workflows/{file}/runs filtered by event and
// head_sha, one page of dispatchedRunsPage. A run is listed from the moment
// GitHub creates it, queued, before any of its jobs or checks: what a PR's
// check rollup, which leaves a dispatched run's check suite out, cannot
// show. A run of another event or commit in the answer is left out. The
// identity needs read access to the repository's Actions (an App: Actions:
// read). A workflow that is not a plain file name of .github/workflows
// (ValidWorkflowFile) or a sha that is no full commit id is refused before
// any call.
func (c *Client) DispatchedRuns(ctx context.Context, owner, repo, workflow, sha string) ([]WorkflowRun, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if !ValidWorkflowFile(workflow) {
		return nil, fmt.Errorf("github: invalid workflow file %q", workflow)
	}
	if !commitOidRe.MatchString(sha) {
		return nil, fmt.Errorf("github: invalid commit %q", sha)
	}
	var data struct {
		Runs []struct {
			Event      string    `json:"event"`
			HeadSHA    string    `json:"head_sha"`
			Status     string    `json:"status"`
			Conclusion *string   `json:"conclusion"`
			URL        string    `json:"html_url"`
			CreatedAt  time.Time `json:"created_at"`
		} `json:"workflow_runs"`
	}
	path := fmt.Sprintf("repos/%s/%s/actions/workflows/%s/runs?event=workflow_dispatch&head_sha=%s&exclude_pull_requests=true&per_page=%d",
		owner, repo, workflow, sha, dispatchedRunsPage)
	op := fmt.Sprintf("dispatched runs %s/%s:%s@%s", owner, repo, workflow, sha[:7])
	if err := c.rest(ctx, op, "GET", path, nil, false, &data); err != nil {
		return nil, err
	}
	runs := []WorkflowRun{}
	for _, r := range data.Runs {
		if r.Event != "workflow_dispatch" || r.HeadSHA != sha {
			continue
		}
		run := WorkflowRun{Status: r.Status, URL: r.URL, CreatedAt: r.CreatedAt}
		if r.Conclusion != nil {
			run.Conclusion = *r.Conclusion
		}
		runs = append(runs, run)
	}
	return runs, nil
}
