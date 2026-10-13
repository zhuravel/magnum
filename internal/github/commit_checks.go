package github

import (
	"context"
	"fmt"
	"regexp"
)

// commitChecksQuery reads one commit's check rollup by its oid: the
// contexts Details reads of a pull request's head (contextNodes), one page
// of 100, without their counts or the rollup's state. 1 point a call.
const commitChecksQuery = `query($owner: String!, $name: String!, $oid: GitObjectID!) {
  ` + rateLimitFields + `
  repository(owner: $owner, name: $name) {
    object(oid: $oid) { ... on Commit { oid statusCheckRollup { contexts(first: 100) { ` + contextNodes + ` } } } }
  }
}`

// commitOidRe is a full commit id as GitHub writes one.
var commitOidRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// CommitChecks reads the checks of commit sha of owner/repo (a base branch's
// tip, for auto-approval): the latest run of each (Workflow, Name),
// normalized as Details' CIRollup.Checks, in GitHub's order; empty, never
// nil, for a commit without checks. It reads one page of 100: a check past
// it is not listed. A missing repository or commit is an error matching
// ErrNotFound; a sha that is no full commit id is refused before any call.
func (c *Client) CommitChecks(ctx context.Context, owner, repo, sha string) ([]Check, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if !commitOidRe.MatchString(sha) {
		return nil, fmt.Errorf("github: invalid commit %q", sha)
	}
	var data struct {
		Repository *struct {
			Object *struct {
				Oid               string `json:"oid"`
				StatusCheckRollup *struct {
					Contexts rollupContextsJSON `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"object"`
		} `json:"repository"`
	}
	op := fmt.Sprintf("commit checks %s/%s@%s", owner, repo, sha[:7])
	vars := map[string]any{"owner": owner, "name": repo, "oid": sha}
	_, notFound, err := c.graphql(ctx, op, commitChecksQuery, vars, &data)
	if err != nil {
		return nil, err
	}
	// A tree or a blob of that oid answers the Commit fragment empty.
	if len(notFound) > 0 || data.Repository == nil || data.Repository.Object == nil || data.Repository.Object.Oid == "" {
		return nil, &APIError{Op: op, Errors: notFoundOr(notFound, op)}
	}
	r := data.Repository.Object.StatusCheckRollup
	if r == nil {
		return []Check{}, nil
	}
	return r.Contexts.checks(), nil
}
