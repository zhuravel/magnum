package github

import (
	"context"
	"fmt"
	"time"
)

// ReviewDismissal is a review's dismissal as a pull request's timeline
// records it (a ReviewDismissedEvent).
type ReviewDismissal struct {
	ReviewID int64  // the dismissed review's REST id; 0 when the review is gone
	Actor    string // who dismissed it, in Account form ("" for a deleted account)
	// ByPush: GitHub dismissed it as stale when Commit was pushed (branch
	// protection's "dismiss stale approvals"), not a person.
	ByPush bool
	Commit string
	At     time.Time
}

const dismissalsQuery = `query($owner: String!, $name: String!, $number: Int!) {
  ` + rateLimitFields + `
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      timelineItems(last: 100, itemTypes: [REVIEW_DISMISSED_EVENT]) {
        nodes { ... on ReviewDismissedEvent { createdAt actor { login __typename } review { databaseId } pullRequestCommit { commit { oid } } } }
      }
    }
  }
}`

// ReviewDismissals lists the last 100 review dismissals of a pull request,
// oldest first: who dismissed each review, or that GitHub did on a push. A
// missing repository or pull request is an error matching ErrNotFound.
func (c *Client) ReviewDismissals(ctx context.Context, owner, repo string, number int) ([]ReviewDismissal, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if number <= 0 {
		return nil, fmt.Errorf("github: invalid pull request number %d", number)
	}
	var data struct {
		Repository *struct {
			PullRequest *struct {
				TimelineItems struct {
					Nodes []struct {
						CreatedAt time.Time  `json:"createdAt"`
						Actor     *actorJSON `json:"actor"`
						Review    *struct {
							DatabaseID int64 `json:"databaseId"`
						} `json:"review"`
						PullRequestCommit *struct {
							Commit *struct {
								Oid string `json:"oid"`
							} `json:"commit"`
						} `json:"pullRequestCommit"`
					} `json:"nodes"`
				} `json:"timelineItems"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	op := fmt.Sprintf("review dismissals %s/%s#%d", owner, repo, number)
	vars := map[string]any{"owner": owner, "name": repo, "number": number}
	_, notFound, err := c.graphql(ctx, op, dismissalsQuery, vars, &data)
	if err != nil {
		return nil, err
	}
	if len(notFound) > 0 || data.Repository == nil || data.Repository.PullRequest == nil {
		return nil, &APIError{Op: op, Errors: notFoundOr(notFound, op)}
	}
	out := []ReviewDismissal{}
	for _, n := range data.Repository.PullRequest.TimelineItems.Nodes {
		d := ReviewDismissal{At: n.CreatedAt}
		if n.Actor != nil {
			d.Actor = Account(n.Actor.Login, n.Actor.Typename)
		}
		if n.Review != nil {
			d.ReviewID = n.Review.DatabaseID
		}
		if p := n.PullRequestCommit; p != nil {
			d.ByPush = true
			if p.Commit != nil {
				d.Commit = p.Commit.Oid
			}
		}
		out = append(out, d)
	}
	return out, nil
}
