package github

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Review is a pull request review as GraphQL reports it. AuthorLogin is the
// GraphQL login (no "[bot]" suffix); ReviewREST has the REST login.
type Review struct {
	DatabaseID  int64  // REST review id
	State       string // APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED | PENDING
	Body        string
	URL         string
	SubmittedAt time.Time
	CommitOid   string // "" when the commit is gone
	AuthorLogin string // "" for a ghost
	AuthorType  string // GraphQL __typename
}

// RESTReview is a review as REST reports it; UserLogin keeps the "[bot]"
// suffix ("talkable[bot]"), which identifies the posting App.
type RESTReview struct {
	ID          int64
	NodeID      string
	UserLogin   string
	UserType    string // User | Bot
	State       string
	Body        string
	HTMLURL     string
	CommitID    string
	SubmittedAt time.Time
}

const reviewsQuery = `query($owner: String!, $name: String!, $number: Int!) {
  ` + rateLimitFields + `
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviews(last: 30) { nodes { databaseId state body url submittedAt commit { oid } author { login __typename } } }
    }
  }
}`

// ReviewsWithMarker returns the last 30 reviews of a pull request whose body
// contains marker, oldest first; an empty marker returns all of them. A
// missing repository or pull request is an error matching ErrNotFound.
func (c *Client) ReviewsWithMarker(ctx context.Context, owner, repo string, number int, marker string) ([]Review, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if number <= 0 {
		return nil, fmt.Errorf("github: invalid pull request number %d", number)
	}
	var data struct {
		Repository *struct {
			PullRequest *struct {
				Reviews struct {
					Nodes []struct {
						DatabaseID  int64      `json:"databaseId"`
						State       string     `json:"state"`
						Body        string     `json:"body"`
						URL         string     `json:"url"`
						SubmittedAt time.Time  `json:"submittedAt"`
						Author      *actorJSON `json:"author"`
						Commit      *struct {
							Oid string `json:"oid"`
						} `json:"commit"`
					} `json:"nodes"`
				} `json:"reviews"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	op := fmt.Sprintf("reviews %s/%s#%d", owner, repo, number)
	vars := map[string]any{"owner": owner, "name": repo, "number": number}
	_, notFound, err := c.graphql(ctx, op, reviewsQuery, vars, &data)
	if err != nil {
		return nil, err
	}
	if len(notFound) > 0 || data.Repository == nil || data.Repository.PullRequest == nil {
		return nil, &APIError{Op: op, Errors: notFoundOr(notFound, op)}
	}
	out := []Review{}
	for _, n := range data.Repository.PullRequest.Reviews.Nodes {
		if marker != "" && !strings.Contains(n.Body, marker) {
			continue
		}
		r := Review{DatabaseID: n.DatabaseID, State: n.State, Body: n.Body, URL: n.URL, SubmittedAt: n.SubmittedAt}
		if n.Author != nil {
			r.AuthorLogin, r.AuthorType = n.Author.Login, n.Author.Typename
		}
		if n.Commit != nil {
			r.CommitOid = n.Commit.Oid
		}
		out = append(out, r)
	}
	return out, nil
}

// ReviewREST reads one review over REST (GET /repos/{o}/{r}/pulls/{n}/reviews/{id})
// for the "[bot]"-suffixed user.login. REST has no review lookup without the
// pull request number.
func (c *Client) ReviewREST(ctx context.Context, owner, repo string, number int, id int64) (RESTReview, error) {
	if err := checkRepo(owner, repo); err != nil {
		return RESTReview{}, err
	}
	if number <= 0 || id <= 0 {
		return RESTReview{}, fmt.Errorf("github: invalid review %d on pull request %d", id, number)
	}
	var r struct {
		ID     int64  `json:"id"`
		NodeID string `json:"node_id"`
		User   *struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
		State       string    `json:"state"`
		Body        string    `json:"body"`
		HTMLURL     string    `json:"html_url"`
		CommitID    string    `json:"commit_id"`
		SubmittedAt time.Time `json:"submitted_at"`
	}
	path := fmt.Sprintf("repos/%s/%s/pulls/%d/reviews/%d", owner, repo, number, id)
	op := fmt.Sprintf("review %s/%s#%d/%d", owner, repo, number, id)
	if err := c.rest(ctx, op, "GET", path, nil, false, &r); err != nil {
		return RESTReview{}, err
	}
	out := RESTReview{ID: r.ID, NodeID: r.NodeID, State: r.State, Body: r.Body, HTMLURL: r.HTMLURL, CommitID: r.CommitID, SubmittedAt: r.SubmittedAt}
	if r.User != nil {
		out.UserLogin, out.UserType = r.User.Login, r.User.Type
	}
	return out, nil
}

// DismissReview dismisses a review (PUT /repos/{o}/{r}/pulls/{n}/reviews/{id}/dismissals)
// as the client's identity. It is marked Mutates, so execx.DryRun only plans
// it. A missing permission is an error matching ErrForbidden; callers report
// it and do not retry.
func (c *Client) DismissReview(ctx context.Context, owner, repo string, number int, reviewID int64, message string) error {
	if err := checkRepo(owner, repo); err != nil {
		return err
	}
	if number <= 0 || reviewID <= 0 {
		return fmt.Errorf("github: invalid review %d on pull request %d", reviewID, number)
	}
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("github: dismissing review %d needs a message", reviewID)
	}
	path := fmt.Sprintf("repos/%s/%s/pulls/%d/reviews/%d/dismissals", owner, repo, number, reviewID)
	op := fmt.Sprintf("dismiss review %s/%s#%d/%d", owner, repo, number, reviewID)
	return c.rest(ctx, op, "PUT", path, [][2]string{{"message", message}, {"event", "DISMISS"}}, true, nil)
}

// UpdateReviewBody replaces the summary body of a review (PUT
// /repos/{o}/{r}/pulls/{n}/reviews/{id}) as the client's identity; GitHub
// lets only the review's author edit it, so another login's review fails
// (an error matching ErrForbidden or ErrNotFound). It is marked Mutates, so
// execx.DryRun only plans it.
func (c *Client) UpdateReviewBody(ctx context.Context, owner, repo string, number int, reviewID int64, body string) error {
	if err := checkRepo(owner, repo); err != nil {
		return err
	}
	if number <= 0 || reviewID <= 0 {
		return fmt.Errorf("github: invalid review %d on pull request %d", reviewID, number)
	}
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("github: review %d needs a body", reviewID)
	}
	path := fmt.Sprintf("repos/%s/%s/pulls/%d/reviews/%d", owner, repo, number, reviewID)
	op := fmt.Sprintf("update review %s/%s#%d/%d", owner, repo, number, reviewID)
	return c.rest(ctx, op, "PUT", path, [][2]string{{"body", body}}, true, nil)
}
