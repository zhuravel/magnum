package github

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// RepoRadar is one non-archived repository of an organization or user with its
// open pull requests.
type RepoRadar struct {
	NodeID        string
	NameWithOwner string    // "talkable/talkable"
	PushedAt      time.Time // zero for an empty repository
	PRs           []PRRadar
}

// PRRadar is the scalar-only view of an open pull request the poller diffs.
type PRRadar struct {
	NodeID      string
	Number      int
	IsDraft     bool
	UpdatedAt   time.Time
	HeadRefOid  string
	BaseRefName string
}

const prRadarFields = "nodes { id number isDraft updatedAt headRefOid baseRefName }"

// radarQuery is the per-owner poll: scalar fields only, so 100 repositories x
// 100 pull requests cost one point. repositoryOwner resolves both users and
// organizations (organization(login:) answers NOT_FOUND for a user). The
// repositories field defaults to ownerAffiliations [OWNER, COLLABORATOR],
// which for a user would list other people's repositories they collaborate
// on; OWNER keeps the poll to repositories the owner itself owns.
const radarQuery = `query($org: String!, $first: Int!, $prFirst: Int!, $after: String) {
  ` + rateLimitFields + `
  repositoryOwner(login: $org) {
    repositories(first: $first, after: $after, isArchived: false, ownerAffiliations: OWNER, orderBy: {field: NAME, direction: ASC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        id nameWithOwner pushedAt
        pullRequests(states: OPEN, first: $prFirst) {
          pageInfo { hasNextPage endCursor }
          ` + prRadarFields + `
        }
      }
    }
  }
}`

// radarPRsQuery continues one repository's open pull requests.
const radarPRsQuery = `query($id: ID!, $prFirst: Int!, $after: String) {
  ` + rateLimitFields + `
  node(id: $id) {
    ... on Repository {
      pullRequests(states: OPEN, first: $prFirst, after: $after) {
        pageInfo { hasNextPage endCursor }
        ` + prRadarFields + `
      }
    }
  }
}`

// maxPages bounds every pagination loop (100 per page -> 50k items).
const maxPages = 500

type pageInfo struct {
	HasNextPage bool    `json:"hasNextPage"`
	EndCursor   *string `json:"endCursor"`
}

// next returns the cursor for the following page, or "" when done.
func (p pageInfo) next(prev string) (string, error) {
	if !p.HasNextPage {
		return "", nil
	}
	if p.EndCursor == nil || *p.EndCursor == "" || *p.EndCursor == prev {
		return "", errors.New("hasNextPage without a new endCursor")
	}
	return *p.EndCursor, nil
}

type prConn struct {
	PageInfo pageInfo `json:"pageInfo"`
	Nodes    []struct {
		ID          string    `json:"id"`
		Number      int       `json:"number"`
		IsDraft     bool      `json:"isDraft"`
		UpdatedAt   time.Time `json:"updatedAt"`
		HeadRefOid  string    `json:"headRefOid"`
		BaseRefName string    `json:"baseRefName"`
	} `json:"nodes"`
}

func (p prConn) radar() []PRRadar {
	out := make([]PRRadar, 0, len(p.Nodes))
	for _, n := range p.Nodes {
		out = append(out, PRRadar{
			NodeID:      n.ID,
			Number:      n.Number,
			IsDraft:     n.IsDraft,
			UpdatedAt:   n.UpdatedAt,
			HeadRefOid:  n.HeadRefOid,
			BaseRefName: n.BaseRefName,
		})
	}
	return out
}

// rateSum folds the RateLimit of several calls into one: Cost is summed, the
// other fields are the most conservative snapshot seen.
type rateSum struct{ total RateLimit }

func (s *rateSum) add(r RateLimit) {
	cost := s.total.Cost + r.Cost
	if r.tighter(s.total) {
		s.total = r
	}
	s.total.Cost = cost
}

// Radar lists every non-archived repository owned by owner (an organization
// or a user) with all its open pull requests, following both the repository
// and the per-repository pull request cursors. It is all-or-nothing: any
// failed page fails the call and no repository list is returned, so the
// caller never mistakes a partial list for closed pull requests.
// The returned RateLimit sums Cost over all calls; the other fields are the
// most conservative snapshot seen. When a later page fails it still carries
// what the earlier pages reported, so the caller can pause on a low budget.
func (c *Client) Radar(ctx context.Context, org string) ([]RepoRadar, RateLimit, error) {
	if !ownerRe.MatchString(org) {
		return nil, RateLimit{}, fmt.Errorf("github: invalid owner %q", org)
	}
	repoPage, prPage := pageSize(c.repoPage), pageSize(c.prPage)
	var (
		repos []RepoRadar
		sum   rateSum
		after string
	)
	for page := 1; ; page++ {
		if page > maxPages {
			return nil, sum.total, fmt.Errorf("github radar %s: more than %d repository pages", org, maxPages)
		}
		vars := map[string]any{"org": org, "first": repoPage, "prFirst": prPage}
		if after != "" {
			vars["after"] = after
		}
		var data struct {
			RepositoryOwner *struct {
				Repositories struct {
					PageInfo pageInfo `json:"pageInfo"`
					Nodes    []struct {
						ID            string    `json:"id"`
						NameWithOwner string    `json:"nameWithOwner"`
						PushedAt      time.Time `json:"pushedAt"`
						PullRequests  prConn    `json:"pullRequests"`
					} `json:"nodes"`
				} `json:"repositories"`
			} `json:"repositoryOwner"`
		}
		op := fmt.Sprintf("radar %s page %d", org, page)
		rate, notFound, err := c.graphql(ctx, op, radarQuery, vars, &data)
		if err != nil {
			return nil, sum.total, err
		}
		sum.add(rate)
		if len(notFound) > 0 || data.RepositoryOwner == nil {
			return nil, sum.total, &APIError{Op: op, Errors: notFoundOr(notFound, "owner "+org)}
		}
		conn := data.RepositoryOwner.Repositories
		for _, n := range conn.Nodes {
			r := RepoRadar{NodeID: n.ID, NameWithOwner: n.NameWithOwner, PushedAt: n.PushedAt, PRs: n.PullRequests.radar()}
			cursor, err := n.PullRequests.PageInfo.next("")
			if err != nil {
				return nil, sum.total, fmt.Errorf("github %s: %s pull requests: %w", op, n.NameWithOwner, err)
			}
			if cursor != "" {
				more, err := c.morePRs(ctx, &sum, n.ID, n.NameWithOwner, cursor, prPage)
				if err != nil {
					return nil, sum.total, err
				}
				r.PRs = append(r.PRs, more...)
			}
			repos = append(repos, r)
		}
		next, err := conn.PageInfo.next(after)
		if err != nil {
			return nil, sum.total, fmt.Errorf("github %s: %w", op, err)
		}
		if next == "" {
			break
		}
		after = next
	}
	return repos, sum.total, nil
}

// morePRs follows one repository's pull request cursor to the end, adding
// each call's rate limit to sum.
func (c *Client) morePRs(ctx context.Context, sum *rateSum, id, name, after string, prPage int) ([]PRRadar, error) {
	var out []PRRadar
	for page := 2; ; page++ {
		if page > maxPages {
			return nil, fmt.Errorf("github radar %s: more than %d pull request pages", name, maxPages)
		}
		var data struct {
			Node *struct {
				PullRequests *prConn `json:"pullRequests"`
			} `json:"node"`
		}
		op := fmt.Sprintf("radar %s pulls page %d", name, page)
		rate, notFound, err := c.graphql(ctx, op, radarPRsQuery, map[string]any{"id": id, "prFirst": prPage, "after": after}, &data)
		if err != nil {
			return nil, err
		}
		sum.add(rate)
		if len(notFound) > 0 || data.Node == nil || data.Node.PullRequests == nil {
			return nil, &APIError{Op: op, Errors: notFoundOr(notFound, "repository "+name)}
		}
		out = append(out, data.Node.PullRequests.radar()...)
		next, err := data.Node.PullRequests.PageInfo.next(after)
		if err != nil {
			return nil, fmt.Errorf("github %s: %w", op, err)
		}
		if next == "" {
			return out, nil
		}
		after = next
	}
}

func pageSize(n int) int {
	if n <= 0 || n > 100 {
		return 100
	}
	return n
}

// notFoundOr returns errs, or a synthetic NOT_FOUND for what when GitHub
// returned a null without saying why.
func notFoundOr(errs []GraphQLError, what string) []GraphQLError {
	if len(errs) > 0 {
		return errs
	}
	return []GraphQLError{{Type: "NOT_FOUND", Message: "could not resolve " + what}}
}
