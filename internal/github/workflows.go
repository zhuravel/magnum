package github

import (
	"context"
	"fmt"
	"regexp"
	"slices"
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

// WorkflowRun is a run of a workflow as DispatchedRuns, CommitRuns and
// RecentRuns list it: its id, its status (queued, waiting, requested,
// pending, in_progress or completed), its conclusion once completed
// (success, failure, cancelled, timed_out, ...), its page and when GitHub
// created it; RecentRuns also says the commit, the branch and the event it
// ran for (the branch is a PR's text: never logged).
type WorkflowRun struct {
	ID         int64
	Status     string
	Conclusion string
	URL        string
	CreatedAt  time.Time
	HeadSHA    string
	HeadBranch string
	Event      string
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
			ID         int64     `json:"id"`
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
		run := WorkflowRun{ID: r.ID, Status: r.Status, URL: r.URL, CreatedAt: r.CreatedAt}
		if r.Conclusion != nil {
			run.Conclusion = *r.Conclusion
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// commitRunsPage is how many runs CommitRuns reads: the newest of one
// workflow at one commit (a push's run and its re-triggers).
const commitRunsPage = 20

// CommitRuns lists the runs of the workflow file workflow ("ci.yml") of
// owner/repo at commit sha, of any event (a push to a branch, a dispatch),
// newest first: GET /repos/{o}/{r}/actions/workflows/{file}/runs filtered
// by head_sha, one page of commitRunsPage. A run of another commit in the
// answer is left out. The identity needs read access to the repository's
// Actions (an App: Actions: read). A workflow that is not a plain file name
// of .github/workflows (ValidWorkflowFile) or a sha that is no full commit
// id is refused before any call.
func (c *Client) CommitRuns(ctx context.Context, owner, repo, workflow, sha string) ([]WorkflowRun, error) {
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
			ID         int64     `json:"id"`
			HeadSHA    string    `json:"head_sha"`
			Status     string    `json:"status"`
			Conclusion *string   `json:"conclusion"`
			URL        string    `json:"html_url"`
			CreatedAt  time.Time `json:"created_at"`
		} `json:"workflow_runs"`
	}
	path := fmt.Sprintf("repos/%s/%s/actions/workflows/%s/runs?head_sha=%s&exclude_pull_requests=true&per_page=%d",
		owner, repo, workflow, sha, commitRunsPage)
	op := fmt.Sprintf("commit runs %s/%s:%s@%s", owner, repo, workflow, sha[:7])
	if err := c.rest(ctx, op, "GET", path, nil, false, &data); err != nil {
		return nil, err
	}
	runs := []WorkflowRun{}
	for _, r := range data.Runs {
		if r.HeadSHA != sha {
			continue
		}
		run := WorkflowRun{ID: r.ID, Status: r.Status, URL: r.URL, CreatedAt: r.CreatedAt}
		if r.Conclusion != nil {
			run.Conclusion = *r.Conclusion
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// WorkflowJob is a job of a workflow run as RunJobs lists it. An Actions
// job's id is also its check run's (AnnotationPaths).
type WorkflowJob struct {
	ID         int64
	Name       string
	Status     string
	Conclusion string
}

// runJobsPage is how many jobs RunJobs reads of one run.
const runJobsPage = 100

// RunJobs lists the jobs of the latest attempt of run id of owner/repo: GET
// /repos/{o}/{r}/actions/runs/{id}/jobs?filter=latest, one page of
// runJobsPage. No step or log is read.
func (c *Client) RunJobs(ctx context.Context, owner, repo string, runID int64) ([]WorkflowJob, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if runID <= 0 {
		return nil, fmt.Errorf("github: invalid run %d", runID)
	}
	var data struct {
		Jobs []struct {
			ID         int64   `json:"id"`
			Name       string  `json:"name"`
			Status     string  `json:"status"`
			Conclusion *string `json:"conclusion"`
		} `json:"jobs"`
	}
	path := fmt.Sprintf("repos/%s/%s/actions/runs/%d/jobs?filter=latest&per_page=%d", owner, repo, runID, runJobsPage)
	op := fmt.Sprintf("run jobs %s/%s#%d", owner, repo, runID)
	if err := c.rest(ctx, op, "GET", path, nil, false, &data); err != nil {
		return nil, err
	}
	jobs := make([]WorkflowJob, 0, len(data.Jobs))
	for _, j := range data.Jobs {
		job := WorkflowJob{ID: j.ID, Name: j.Name, Status: j.Status}
		if j.Conclusion != nil {
			job.Conclusion = *j.Conclusion
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

// MaxAnnotations is the most annotations AnnotationPaths reads of one check
// run: one page, GitHub's own limit per page being higher.
const MaxAnnotations = 50

// annotationNoFile is the path GitHub gives an annotation tied to no file
// ("Process completed with exit code 1.").
const annotationNoFile = ".github"

// AnnotationPaths lists the repository paths the annotations of check run
// id of owner/repo name, in their order without repeats: GET
// /repos/{o}/{r}/check-runs/{id}/annotations, one page of limit (1 to
// MaxAnnotations). The identity needs Checks: read (an App; Actions: read
// covers the runs and jobs only). An annotation tied to no file (path
// ".github") names none, and no annotation's message is read: it may quote
// a PR.
func (c *Client) AnnotationPaths(ctx context.Context, owner, repo string, checkRunID int64, limit int) ([]string, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if checkRunID <= 0 {
		return nil, fmt.Errorf("github: invalid check run %d", checkRunID)
	}
	limit = min(max(limit, 1), MaxAnnotations)
	var data []struct {
		Path string `json:"path"`
	}
	path := fmt.Sprintf("repos/%s/%s/check-runs/%d/annotations?per_page=%d", owner, repo, checkRunID, limit)
	op := fmt.Sprintf("annotations %s/%s#%d", owner, repo, checkRunID)
	if err := c.rest(ctx, op, "GET", path, nil, false, &data); err != nil {
		return nil, err
	}
	paths := []string{}
	for _, a := range data {
		if a.Path == "" || a.Path == annotationNoFile || slices.Contains(paths, a.Path) {
			continue
		}
		paths = append(paths, a.Path)
	}
	return paths, nil
}

// recentRunsPage is how many runs RecentRuns reads: the newest of one
// workflow, of every branch, in which a run still in progress shows.
const recentRunsPage = 50

// RecentRuns lists the newest runs of the workflow file workflow ("ci.yml")
// of owner/repo, of any event and branch, newest first, each with its
// commit, branch and event: GET /repos/{o}/{r}/actions/workflows/{file}/runs,
// one page of recentRunsPage. The caller picks a branch's runs out of it,
// so no branch name reaches the command line, which a failed call logs. The
// identity needs read access to the repository's Actions (an App: Actions:
// read). A workflow that is not a plain file name of .github/workflows
// (ValidWorkflowFile) is refused before any call.
func (c *Client) RecentRuns(ctx context.Context, owner, repo, workflow string) ([]WorkflowRun, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if !ValidWorkflowFile(workflow) {
		return nil, fmt.Errorf("github: invalid workflow file %q", workflow)
	}
	var data struct {
		Runs []struct {
			ID         int64     `json:"id"`
			Event      string    `json:"event"`
			HeadSHA    string    `json:"head_sha"`
			HeadBranch string    `json:"head_branch"`
			Status     string    `json:"status"`
			Conclusion *string   `json:"conclusion"`
			URL        string    `json:"html_url"`
			CreatedAt  time.Time `json:"created_at"`
		} `json:"workflow_runs"`
	}
	path := fmt.Sprintf("repos/%s/%s/actions/workflows/%s/runs?exclude_pull_requests=true&per_page=%d", owner, repo, workflow, recentRunsPage)
	if err := c.rest(ctx, fmt.Sprintf("recent runs %s/%s:%s", owner, repo, workflow), "GET", path, nil, false, &data); err != nil {
		return nil, err
	}
	runs := []WorkflowRun{}
	for _, r := range data.Runs {
		run := WorkflowRun{ID: r.ID, Status: r.Status, URL: r.URL, CreatedAt: r.CreatedAt, HeadSHA: r.HeadSHA, HeadBranch: r.HeadBranch, Event: r.Event}
		if r.Conclusion != nil {
			run.Conclusion = *r.Conclusion
		}
		runs = append(runs, run)
	}
	return runs, nil
}
