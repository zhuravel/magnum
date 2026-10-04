package github

import (
	"context"
	"fmt"
	"time"
)

// Thread is an inline review thread of a pull request as GraphQL reports it.
type Thread struct {
	ID           string // GraphQL node id (PRRT_…)
	Path         string
	Line         int // 0 when GitHub reports none (an outdated thread)
	OriginalLine int // the line the thread was started on
	Resolved     bool
	Outdated     bool
	// Comments are oldest first; Comments[0] started the thread and the rest
	// are its replies. At most threadCommentsMax per thread.
	Comments []ThreadComment
}

// ThreadComment is one comment of a review thread.
type ThreadComment struct {
	ID          int64  // REST comment id (in_reply_to for a reply)
	AuthorLogin string // GraphQL login (no "[bot]" suffix); "" for a ghost
	AuthorType  string // GraphQL __typename: User | Bot
	Body        string
	URL         string
	CreatedAt   time.Time
	ReviewID    int64 // the review the comment belongs to; 0 when unknown
}

const (
	threadsPageSize   = 100
	threadsMaxPages   = 10
	threadCommentsMax = 50
)

var threadsQuery = fmt.Sprintf(`query($owner: String!, $name: String!, $number: Int!, $cursor: String) {
  `+rateLimitFields+`
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: %d, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id isResolved isOutdated path line originalLine
          comments(first: %d) {
            nodes { databaseId body url createdAt author { login __typename } pullRequestReview { databaseId } }
          }
        }
      }
    }
  }
}`, threadsPageSize, threadCommentsMax)

// ReviewThreads lists the inline review threads of a pull request with their
// comments, oldest thread first (at most threadsMaxPages pages of
// threadsPageSize threads, threadCommentsMax comments each). A missing
// repository or pull request is an error matching ErrNotFound.
func (c *Client) ReviewThreads(ctx context.Context, owner, repo string, number int) ([]Thread, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if number <= 0 {
		return nil, fmt.Errorf("github: invalid pull request number %d", number)
	}
	op := fmt.Sprintf("review threads %s/%s#%d", owner, repo, number)
	out := []Thread{}
	var cursor any // nil: the first page
	for range threadsMaxPages {
		var data struct {
			Repository *struct {
				PullRequest *struct {
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							ID           string `json:"id"`
							IsResolved   bool   `json:"isResolved"`
							IsOutdated   bool   `json:"isOutdated"`
							Path         string `json:"path"`
							Line         *int   `json:"line"`
							OriginalLine *int   `json:"originalLine"`
							Comments     struct {
								Nodes []struct {
									DatabaseID        int64      `json:"databaseId"`
									Body              string     `json:"body"`
									URL               string     `json:"url"`
									CreatedAt         time.Time  `json:"createdAt"`
									Author            *actorJSON `json:"author"`
									PullRequestReview *struct {
										DatabaseID int64 `json:"databaseId"`
									} `json:"pullRequestReview"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": owner, "name": repo, "number": number, "cursor": cursor}
		_, notFound, err := c.graphql(ctx, op, threadsQuery, vars, &data)
		if err != nil {
			return nil, err
		}
		if len(notFound) > 0 || data.Repository == nil || data.Repository.PullRequest == nil {
			return nil, &APIError{Op: op, Errors: notFoundOr(notFound, op)}
		}
		rt := data.Repository.PullRequest.ReviewThreads
		for _, n := range rt.Nodes {
			t := Thread{ID: n.ID, Path: n.Path, Resolved: n.IsResolved, Outdated: n.IsOutdated}
			if n.Line != nil {
				t.Line = *n.Line
			}
			if n.OriginalLine != nil {
				t.OriginalLine = *n.OriginalLine
			}
			for _, cm := range n.Comments.Nodes {
				tc := ThreadComment{ID: cm.DatabaseID, Body: cm.Body, URL: cm.URL, CreatedAt: cm.CreatedAt}
				if cm.Author != nil {
					tc.AuthorLogin, tc.AuthorType = cm.Author.Login, cm.Author.Typename
				}
				if cm.PullRequestReview != nil {
					tc.ReviewID = cm.PullRequestReview.DatabaseID
				}
				t.Comments = append(t.Comments, tc)
			}
			out = append(out, t)
		}
		if !rt.PageInfo.HasNextPage || rt.PageInfo.EndCursor == "" {
			return out, nil
		}
		cursor = rt.PageInfo.EndCursor
	}
	return out, nil
}
