package staging

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zhuravel/magnum/internal/github"
)

// The bounds of the adapter's reads.
const (
	// maxStatusReads is how many deployments' statuses DeploymentsOf reads
	// at most, from the newest down to the first success.
	maxStatusReads = 10
	// maxFailedJobs is how many failed jobs' annotations RunsOf reads at
	// most; the others name no path ("cannot tell").
	maxFailedJobs = 10
)

// ghAdapter is GitHub over magnum's GitHub client (internal/github): its
// gh api calls, identity, ETag read and cost meter.
type ghAdapter struct {
	client func(repo string) *github.Client
}

// NewGitHub is the package's GitHub over magnum's GitHub client: client
// gives a repository's client, the one of its poll identity (nil = none:
// each read fails). Deployments need Deployments: read, runs and jobs
// Actions: read, and the failed jobs' annotations Checks: read (without it
// every failure reads "cannot tell"); a 403 or 404 of a deployment read is
// ErrNotReadable.
func NewGitHub(client func(repo string) *github.Client) GitHub { return ghAdapter{client: client} }

// of is repo's client and its owner and name.
func (a ghAdapter) of(repo string) (*github.Client, string, string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return nil, "", "", fmt.Errorf("staging: %q is not owner/name", repo)
	}
	c := a.client(repo)
	if c == nil {
		return nil, "", "", fmt.Errorf("staging: no GitHub client for %s", repo)
	}
	return c, owner, name, nil
}

// notReadable is err, wrapping ErrNotReadable too for a 403 or a 404 (a
// rate limit is neither: GitHub's ErrForbidden leaves it out).
func notReadable(err error) error {
	if errors.Is(err, github.ErrForbidden) || errors.Is(err, github.ErrNotFound) {
		return fmt.Errorf("%w: %w", ErrNotReadable, err)
	}
	return err
}

// DeploymentsOf reads the environment's deployments with etag (GET
// deployments?environment=<name>, If-None-Match), then the statuses of each
// from the newest down to the first whose newest status is a success, at
// most maxStatusReads: a success that a failure or an error followed (a
// health check, a rollback) is none. The deployments older than it count
// for nothing, so none of theirs is read.
func (a ghAdapter) DeploymentsOf(ctx context.Context, repo, environment, etag string) (Deployments, error) {
	c, owner, name, err := a.of(repo)
	if err != nil {
		return Deployments{}, err
	}
	list, newETag, notModified, err := c.Deployments(ctx, owner, name, environment, etag)
	if err != nil {
		return Deployments{}, notReadable(err)
	}
	if notModified {
		return Deployments{ETag: newETag, NotModified: true}, nil
	}
	out := Deployments{ETag: newETag, List: make([]Deployment, 0, len(list))}
	found := false
	for i, d := range list {
		dep := Deployment{ID: d.ID, SHA: d.SHA, Ref: d.Ref, CreatedAt: d.CreatedAt}
		if !found && i < maxStatusReads {
			states, err := c.DeploymentStatuses(ctx, owner, name, d.ID)
			if err != nil {
				return Deployments{}, notReadable(err)
			}
			dep.Success = len(states) > 0 && states[0] == "success"
			found = dep.Success
		}
		out.List = append(out.List, dep)
	}
	return out, nil
}

// RunsOf reads the workflow's runs at sha (CommitRuns), and for the newest,
// when it failed, its failed jobs with the paths of their check runs'
// annotations (at most github.MaxAnnotations each, of at most maxFailedJobs
// jobs). Jobs that cannot be read leave Failed nil, annotations that cannot
// be read a job's Paths empty: both read as "cannot tell".
func (a ghAdapter) RunsOf(ctx context.Context, repo, workflow, sha string) ([]Run, error) {
	c, owner, name, err := a.of(repo)
	if err != nil {
		return nil, err
	}
	runs, err := c.CommitRuns(ctx, owner, name, workflow, sha)
	if err != nil {
		return nil, err
	}
	out := make([]Run, len(runs))
	newest := -1
	for i, r := range runs {
		out[i] = Run{ID: r.ID, URL: r.URL, Status: r.Status, Conclusion: r.Conclusion, CreatedAt: r.CreatedAt}
		if newest < 0 || r.CreatedAt.After(out[newest].CreatedAt) {
			newest = i
		}
	}
	if newest >= 0 && failedRun(&out[newest]) {
		out[newest].Failed = failedJobs(ctx, c, owner, name, out[newest].ID)
	}
	return out, nil
}

// failedRun reports whether a run ended in a failure (not a pass, not a
// cancel).
func failedRun(r *Run) bool {
	return r.Status == "completed" && !passed(r) && r.Conclusion != "cancelled"
}

// failedJobs is the failed jobs of run id with their annotations' paths
// (Checks: read); nil when the jobs cannot be read, empty when none failed.
func failedJobs(ctx context.Context, c *github.Client, owner, name string, runID int64) []Job {
	jobs, err := c.RunJobs(ctx, owner, name, runID)
	if err != nil {
		return nil
	}
	out := []Job{}
	for _, j := range jobs {
		if j.Status != "completed" || j.Conclusion == "success" || j.Conclusion == "skipped" || j.Conclusion == "neutral" || j.Conclusion == "cancelled" {
			continue
		}
		job := Job{Name: j.Name, Paths: []string{}}
		if len(out) < maxFailedJobs {
			if paths, err := c.AnnotationPaths(ctx, owner, name, j.ID, github.MaxAnnotations); err == nil {
				job.Paths = paths
			}
		}
		out = append(out, job)
	}
	return out
}
