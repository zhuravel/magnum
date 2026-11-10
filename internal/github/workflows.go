package github

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
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

// WorkflowRun is a run of a workflow as DispatchedRuns, CommitRuns,
// RecentRuns and HeadRuns list it: its id, its status (queued, waiting,
// requested, pending, in_progress or completed), its conclusion once
// completed (success, failure, cancelled, timed_out, startup_failure, ...),
// its page and when GitHub created it; RecentRuns also says the commit, the
// branch and the event it ran for (the branch is a PR's text: never
// logged), and HeadRuns the commit, the event, the run's attempt (Attempt:
// its run_attempt, 1, then 2 after a re-run) and its workflow's file in
// .github/workflows (Workflow: "ci.yml"; "" for a run of no such file,
// GitHub's own dynamic runs).
type WorkflowRun struct {
	ID         int64
	Status     string
	Conclusion string
	URL        string
	CreatedAt  time.Time
	HeadSHA    string
	HeadBranch string
	Event      string
	Attempt    int
	Workflow   string
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

// WorkflowJob is a job of a workflow run as RunJobs lists it: its id, name,
// status and conclusion once completed (success, failure, cancelled,
// timed_out, skipped, ...), its steps, when it started and completed (zero
// for neither yet), its page (URL) and the attempt of its run it ran in
// (RunAttempt). An Actions job's id is also its check run's
// (AnnotationPaths), and its name the check run's.
type WorkflowJob struct {
	ID          int64
	Name        string
	Status      string
	Conclusion  string
	Steps       []JobStep
	StartedAt   time.Time
	CompletedAt time.Time
	URL         string
	RunAttempt  int
}

// conclusionOf is a nullable conclusion GitHub answered ("" for null).
func conclusionOf(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// JobStep is one step of a job: its number in the job (the log's group
// order), name, status, conclusion once completed, and when it started and
// completed (zero for a step that did not run).
type JobStep struct {
	Number      int
	Name        string
	Status      string
	Conclusion  string
	StartedAt   time.Time
	CompletedAt time.Time
}

// runJobsPage is how many jobs RunJobs reads of one run.
const runJobsPage = 100

// RunJobs lists the jobs of the latest attempt of run id of owner/repo,
// each with its steps and times: GET
// /repos/{o}/{r}/actions/runs/{id}/jobs?filter=latest, one page of
// runJobsPage. No log is read (JobLogTail).
func (c *Client) RunJobs(ctx context.Context, owner, repo string, runID int64) ([]WorkflowJob, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if runID <= 0 {
		return nil, fmt.Errorf("github: invalid run %d", runID)
	}
	type stepJSON struct {
		Number      int        `json:"number"`
		Name        string     `json:"name"`
		Status      string     `json:"status"`
		Conclusion  *string    `json:"conclusion"`
		StartedAt   *time.Time `json:"started_at"`
		CompletedAt *time.Time `json:"completed_at"`
	}
	var data struct {
		Jobs []struct {
			ID          int64      `json:"id"`
			Name        string     `json:"name"`
			Status      string     `json:"status"`
			Conclusion  *string    `json:"conclusion"`
			RunAttempt  int        `json:"run_attempt"`
			URL         string     `json:"html_url"`
			StartedAt   *time.Time `json:"started_at"`
			CompletedAt *time.Time `json:"completed_at"`
			Steps       []stepJSON `json:"steps"`
		} `json:"jobs"`
	}
	path := fmt.Sprintf("repos/%s/%s/actions/runs/%d/jobs?filter=latest&per_page=%d", owner, repo, runID, runJobsPage)
	op := fmt.Sprintf("run jobs %s/%s#%d", owner, repo, runID)
	if err := c.rest(ctx, op, "GET", path, nil, false, &data); err != nil {
		return nil, err
	}
	at := func(t *time.Time) time.Time {
		if t == nil {
			return time.Time{}
		}
		return *t
	}
	jobs := make([]WorkflowJob, 0, len(data.Jobs))
	for _, j := range data.Jobs {
		job := WorkflowJob{ID: j.ID, Name: j.Name, Status: j.Status, Conclusion: conclusionOf(j.Conclusion), RunAttempt: j.RunAttempt, URL: j.URL,
			StartedAt: at(j.StartedAt), CompletedAt: at(j.CompletedAt)}
		for _, s := range j.Steps {
			job.Steps = append(job.Steps, JobStep{Number: s.Number, Name: s.Name, Status: s.Status, Conclusion: conclusionOf(s.Conclusion),
				StartedAt: at(s.StartedAt), CompletedAt: at(s.CompletedAt)})
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

// JobLogTail reads the log of job id of owner/repo and keeps its last keep
// bytes (at most execx.MaxOutput), where a failed job's error is: GET
// /repos/{o}/{r}/actions/jobs/{id}/logs, whose redirect to the log's
// signed URL gh follows, with --allow-escape-sequences (a job whose tools
// colour their output, FORCE_COLOR, has a log gh 2.103 otherwise refuses
// to print, printing its refusal in place of the log). The whole log
// downloads; execx keeps its tail (Cmd.KeepTail). cut reports that the log
// was longer than keep. The log is the output of the PR's code and its
// tests: data for an agent's file, never for an event or a log line. A log
// GitHub no longer keeps (90 days) answers HTTP 404 or 410, and a job id
// that is no id is refused before any call. The identity needs Actions:
// read.
func (c *Client) JobLogTail(ctx context.Context, owner, repo string, jobID int64, keep int) (log string, cut bool, err error) {
	if err := checkRepo(owner, repo); err != nil {
		return "", false, err
	}
	if jobID <= 0 {
		return "", false, fmt.Errorf("github: invalid job %d", jobID)
	}
	keep = min(max(keep, 1), execx.MaxOutput)
	op := fmt.Sprintf("job log %s/%s#%d", owner, repo, jobID)
	args := append([]string{"api", fmt.Sprintf("repos/%s/%s/actions/jobs/%d/logs", owner, repo, jobID)}, ghHost...)
	args = append(args, "--allow-escape-sequences")
	once := func() (execx.Result, error) {
		res, err := c.ghCmd(ctx, execx.Cmd{Args: args, Label: "gh " + op, KeepTail: keep})
		err = restFailure(op, res, err)
		c.meter(ctx, op, false, RateLimit{}, err)
		return res, err
	}
	res, err := once()
	if retry, rerr := c.reauthorize(ctx, err); retry {
		res, err = once()
	} else {
		err = rerr
	}
	if err != nil {
		return "", false, err
	}
	out := res.Stdout
	if len(out) > keep { // a runner that kept more than asked
		out, res.Truncated = out[len(out)-keep:], true
	}
	return string(out), res.Truncated, nil
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

// headRunsPage is how many runs HeadRuns reads: the newest runs of every
// workflow at one commit.
const headRunsPage = 20

// HeadRuns lists the runs of every workflow of owner/repo at commit sha, of
// any event, newest first, each with its event, its attempt and its
// workflow's file: GET /repos/{o}/{r}/actions/runs filtered by head_sha,
// one page of headRunsPage. A run that started no job is there (its check
// suite holds no check run, so a PR's check rollup cannot show it), and so
// is one still queued. A run of another commit in the answer is left out.
// The identity needs read access to the repository's Actions (an App:
// Actions: read). A sha that is no full commit id is refused before any
// call.
func (c *Client) HeadRuns(ctx context.Context, owner, repo, sha string) ([]WorkflowRun, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if !commitOidRe.MatchString(sha) {
		return nil, fmt.Errorf("github: invalid commit %q", sha)
	}
	var data struct {
		Runs []struct {
			ID         int64     `json:"id"`
			Event      string    `json:"event"`
			HeadSHA    string    `json:"head_sha"`
			Path       string    `json:"path"`
			Status     string    `json:"status"`
			Conclusion *string   `json:"conclusion"`
			Attempt    int       `json:"run_attempt"`
			URL        string    `json:"html_url"`
			CreatedAt  time.Time `json:"created_at"`
		} `json:"workflow_runs"`
	}
	path := fmt.Sprintf("repos/%s/%s/actions/runs?head_sha=%s&exclude_pull_requests=true&per_page=%d", owner, repo, sha, headRunsPage)
	if err := c.rest(ctx, fmt.Sprintf("head runs %s/%s@%s", owner, repo, sha[:7]), "GET", path, nil, false, &data); err != nil {
		return nil, err
	}
	runs := []WorkflowRun{}
	for _, r := range data.Runs {
		if r.HeadSHA != sha {
			continue
		}
		run := WorkflowRun{ID: r.ID, Status: r.Status, URL: r.URL, CreatedAt: r.CreatedAt, HeadSHA: r.HeadSHA, Event: r.Event, Attempt: r.Attempt}
		if file, ok := strings.CutPrefix(r.Path, ".github/workflows/"); ok && ValidWorkflowFile(file) {
			run.Workflow = file
		}
		if r.Conclusion != nil {
			run.Conclusion = *r.Conclusion
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// RerunRun re-runs every job of run id of owner/repo as the client's
// identity, a new attempt of the same run (its run_attempt goes up by one):
// POST /repos/{o}/{r}/actions/runs/{id}/rerun. GitHub runs it with the
// run's own event, commit and workflow file, and refuses (HTTP 403) an
// identity that may not run workflows (a gh login that cannot push, an App
// without Actions: write). A run id that is no id is refused before any
// call. It is marked Mutates, so execx.DryRun only plans it.
func (c *Client) RerunRun(ctx context.Context, owner, repo string, runID int64) error {
	if err := checkRepo(owner, repo); err != nil {
		return err
	}
	if runID <= 0 {
		return fmt.Errorf("github: invalid run %d", runID)
	}
	path := fmt.Sprintf("repos/%s/%s/actions/runs/%d/rerun", owner, repo, runID)
	return c.rest(ctx, fmt.Sprintf("rerun %s/%s#%d", owner, repo, runID), "POST", path, nil, true, nil)
}
