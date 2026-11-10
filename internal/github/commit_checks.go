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
// tip, for auto-approval): the latest run of each check (a check run's
// workflow file and name, a commit status apart), normalized as Details'
// CIRollup.Checks, in GitHub's order; empty, never nil, for a commit
// without checks. It reads one page of 100: a check past
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

// FirstParentDepth is how many commits FirstParentChecks reads: a commit
// and its first parents, four deep.
const FirstParentDepth = 5

// firstParentQuery reads a commit and its first parents, FirstParentDepth
// commits in all, each with its check rollup as commitChecksQuery reads
// one (contextNodes, one page of 100): one call of about 1 to 2 points.
var firstParentQuery = func() string {
	level := "oid statusCheckRollup { contexts(first: 100) { " + contextNodes + " } }"
	q := level
	for range FirstParentDepth - 1 {
		q = level + " parents(first: 1) { nodes { " + q + " } }"
	}
	return `query($owner: String!, $name: String!, $oid: GitObjectID!) {
  ` + rateLimitFields + `
  repository(owner: $owner, name: $name) {
    object(oid: $oid) { ... on Commit { ` + q + ` } }
  }
}`
}()

// CommitWithChecks is a commit and its checks, normalized as
// CommitChecks reads them (empty, never nil, for none).
type CommitWithChecks struct {
	SHA    string
	Checks []Check
}

// firstParentJSON is one commit of firstParentQuery's answer.
type firstParentJSON struct {
	Oid               string `json:"oid"`
	StatusCheckRollup *struct {
		Contexts rollupContextsJSON `json:"contexts"`
	} `json:"statusCheckRollup"`
	Parents *struct {
		Nodes []firstParentJSON `json:"nodes"`
	} `json:"parents"`
}

// FirstParentChecks reads commit sha of owner/repo and its first parents,
// FirstParentDepth commits at most, newest first, each with its checks as
// CommitChecks reads them, in one GraphQL query: the CI fix's read of
// whether a check the PR failed passes on its base (internal/engine's
// ci_fix.go), at the base tip the look fetched. The list ends early at a
// commit with no parent. No branch name reaches the command line: the
// commit goes in the query's variables. A missing repository or commit is
// an error matching ErrNotFound; a sha that is no full commit id is refused
// before any call.
func (c *Client) FirstParentChecks(ctx context.Context, owner, repo, sha string) ([]CommitWithChecks, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if !commitOidRe.MatchString(sha) {
		return nil, fmt.Errorf("github: invalid commit %q", sha)
	}
	var data struct {
		Repository *struct {
			Object *firstParentJSON `json:"object"`
		} `json:"repository"`
	}
	op := fmt.Sprintf("first parent checks %s/%s@%s", owner, repo, sha[:7])
	vars := map[string]any{"owner": owner, "name": repo, "oid": sha}
	_, notFound, err := c.graphql(ctx, op, firstParentQuery, vars, &data)
	if err != nil {
		return nil, err
	}
	if len(notFound) > 0 || data.Repository == nil || data.Repository.Object == nil || data.Repository.Object.Oid == "" {
		return nil, &APIError{Op: op, Errors: notFoundOr(notFound, op)}
	}
	var out []CommitWithChecks
	for n := data.Repository.Object; n != nil && n.Oid != "" && len(out) < FirstParentDepth; {
		cw := CommitWithChecks{SHA: n.Oid, Checks: []Check{}}
		if n.StatusCheckRollup != nil {
			cw.Checks = n.StatusCheckRollup.Contexts.checks()
		}
		out = append(out, cw)
		if n.Parents == nil || len(n.Parents.Nodes) == 0 {
			break
		}
		n = &n.Parents.Nodes[0]
	}
	return out, nil
}
