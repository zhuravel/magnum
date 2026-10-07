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

// reviewNodeFields selects one review; reviewNodeJSON decodes it.
const reviewNodeFields = "databaseId state body url submittedAt commit { oid } author { login __typename }"

const reviewsQuery = `query($owner: String!, $name: String!, $number: Int!) {
  ` + rateLimitFields + `
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviews(last: 30) { nodes { ` + reviewNodeFields + ` } }
    }
  }
}`

const (
	reviewsPageSize = 100
	reviewsMaxPages = 5
)

var reviewsPagedQuery = fmt.Sprintf(`query($owner: String!, $name: String!, $number: Int!, $cursor: String) {
  `+rateLimitFields+`
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviews(first: %d, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes { `+reviewNodeFields+` }
      }
    }
  }
}`, reviewsPageSize)

// reviewNodeJSON is one node of a pull request's reviews connection.
type reviewNodeJSON struct {
	DatabaseID  int64      `json:"databaseId"`
	State       string     `json:"state"`
	Body        string     `json:"body"`
	URL         string     `json:"url"`
	SubmittedAt time.Time  `json:"submittedAt"`
	Author      *actorJSON `json:"author"`
	Commit      *struct {
		Oid string `json:"oid"`
	} `json:"commit"`
}

func (n reviewNodeJSON) review() Review {
	r := Review{DatabaseID: n.DatabaseID, State: n.State, Body: n.Body, URL: n.URL, SubmittedAt: n.SubmittedAt}
	if n.Author != nil {
		r.AuthorLogin, r.AuthorType = n.Author.Login, n.Author.Typename
	}
	if n.Commit != nil {
		r.CommitOid = n.Commit.Oid
	}
	return r
}

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
					Nodes []reviewNodeJSON `json:"nodes"`
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
		out = append(out, n.review())
	}
	return out, nil
}

// Reviews is AllReviews without saying whether the list is complete: for a
// reader that takes the first reviewsMaxPages pages as they are. A caller
// that acts on what the list lacks (a review to dismiss, a review already
// posted) reads AllReviews.
func (c *Client) Reviews(ctx context.Context, owner, repo string, number int) ([]Review, error) {
	out, _, err := c.AllReviews(ctx, owner, repo, number)
	return out, err
}

// AllReviews lists every review of a pull request, oldest first:
// reviewsPageSize (100) per page, at most reviewsMaxPages (5) pages. It
// reports whether the list is complete: false when the pull request has
// more reviews than that, the later ones left out. A missing repository or
// pull request is an error matching ErrNotFound.
func (c *Client) AllReviews(ctx context.Context, owner, repo string, number int) ([]Review, bool, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, false, err
	}
	if number <= 0 {
		return nil, false, fmt.Errorf("github: invalid pull request number %d", number)
	}
	op := fmt.Sprintf("all reviews %s/%s#%d", owner, repo, number)
	out := []Review{}
	var cursor any // nil: the first page
	for range reviewsMaxPages {
		var data struct {
			Repository *struct {
				PullRequest *struct {
					Reviews struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []reviewNodeJSON `json:"nodes"`
					} `json:"reviews"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": owner, "name": repo, "number": number, "cursor": cursor}
		_, notFound, err := c.graphql(ctx, op, reviewsPagedQuery, vars, &data)
		if err != nil {
			return nil, false, err
		}
		if len(notFound) > 0 || data.Repository == nil || data.Repository.PullRequest == nil {
			return nil, false, &APIError{Op: op, Errors: notFoundOr(notFound, op)}
		}
		rv := data.Repository.PullRequest.Reviews
		for _, n := range rv.Nodes {
			out = append(out, n.review())
		}
		if !rv.PageInfo.HasNextPage {
			return out, true, nil
		}
		if rv.PageInfo.EndCursor == "" {
			return out, false, nil // more pages, and no way to ask for them
		}
		cursor = rv.PageInfo.EndCursor
	}
	return out, false, nil
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
	var r restReviewJSON
	path := fmt.Sprintf("repos/%s/%s/pulls/%d/reviews/%d", owner, repo, number, id)
	op := fmt.Sprintf("review %s/%s#%d/%d", owner, repo, number, id)
	if err := c.rest(ctx, op, "GET", path, nil, false, &r); err != nil {
		return RESTReview{}, err
	}
	return r.review(), nil
}

// restReviewJSON is a review as REST returns it.
type restReviewJSON struct {
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

func (r restReviewJSON) review() RESTReview {
	out := RESTReview{ID: r.ID, NodeID: r.NodeID, State: r.State, Body: r.Body, HTMLURL: r.HTMLURL, CommitID: r.CommitID, SubmittedAt: r.SubmittedAt}
	if r.User != nil {
		out.UserLogin, out.UserType = r.User.Login, r.User.Type
	}
	return out
}

// CreateReview submits a review without inline comments (POST
// /repos/{o}/{r}/pulls/{n}/reviews) on commitID as the client's identity.
// event is APPROVE, REQUEST_CHANGES or COMMENT; GitHub wants a body for the
// last two. GitHub counts each reviewer's latest review, so a new APPROVE
// or REQUEST_CHANGES supersedes the identity's earlier verdict. It is
// marked Mutates, so execx.DryRun only plans it.
func (c *Client) CreateReview(ctx context.Context, owner, repo string, number int, commitID, event, body string) (RESTReview, error) {
	if err := checkRepo(owner, repo); err != nil {
		return RESTReview{}, err
	}
	switch event {
	case "APPROVE", "REQUEST_CHANGES", "COMMENT":
	default:
		return RESTReview{}, fmt.Errorf("github: review event %q is not APPROVE, REQUEST_CHANGES or COMMENT", event)
	}
	if number <= 0 || len(commitID) != 40 {
		return RESTReview{}, fmt.Errorf("github: a review needs a pull request number and a full commit id")
	}
	if event != "APPROVE" && strings.TrimSpace(body) == "" {
		return RESTReview{}, fmt.Errorf("github: a %s review needs a body", event)
	}
	var r restReviewJSON
	path := fmt.Sprintf("repos/%s/%s/pulls/%d/reviews", owner, repo, number)
	op := fmt.Sprintf("review %s/%s#%d (%s)", owner, repo, number, event)
	fields := [][2]string{{"commit_id", commitID}, {"event", event}, {"body", body}}
	if err := c.rest(ctx, op, "POST", path, fields, true, &r); err != nil {
		return RESTReview{}, err
	}
	return r.review(), nil
}

// ReviewRequest is the body of POST /repos/{o}/{r}/pulls/{n}/reviews: one
// submitted review with all its inline comments (SubmitReview).
type ReviewRequest struct {
	CommitID string         `json:"commit_id"`
	Event    string         `json:"event"` // APPROVE, REQUEST_CHANGES or COMMENT
	Body     string         `json:"body"`
	Comments []DraftComment `json:"comments"`
}

// DraftComment is one inline comment of a ReviewRequest, anchored on line
// (and, for a multi-line comment, from start_line) of path on side: RIGHT
// for the head's lines (added or context), LEFT for the base's (deleted or
// context).
type DraftComment struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Side      string `json:"side,omitempty"`
	StartLine int    `json:"start_line,omitempty"`
	StartSide string `json:"start_side,omitempty"`
	Body      string `json:"body"`
}

// SubmitReview posts one review with its inline comments (POST
// /repos/{o}/{r}/pulls/{n}/reviews) as the client's identity, the request
// sent as JSON on gh's stdin so no review text reaches argv. It checks only
// what the request needs to be well formed (event, a full commit id);
// GitHub checks the rest. It is marked Mutates, so execx.DryRun only plans
// it.
func (c *Client) SubmitReview(ctx context.Context, owner, repo string, number int, r ReviewRequest) (RESTReview, error) {
	if err := checkRepo(owner, repo); err != nil {
		return RESTReview{}, err
	}
	switch r.Event {
	case "APPROVE", "REQUEST_CHANGES", "COMMENT":
	default:
		return RESTReview{}, fmt.Errorf("github: review event %q is not APPROVE, REQUEST_CHANGES or COMMENT", r.Event)
	}
	if number <= 0 || len(r.CommitID) != 40 {
		return RESTReview{}, fmt.Errorf("github: a review needs a pull request number and a full commit id")
	}
	if r.Comments == nil {
		r.Comments = []DraftComment{}
	}
	var out restReviewJSON
	path := fmt.Sprintf("repos/%s/%s/pulls/%d/reviews", owner, repo, number)
	op := fmt.Sprintf("review %s/%s#%d (%s)", owner, repo, number, r.Event)
	if err := c.restInput(ctx, op, "POST", path, r, true, &out); err != nil {
		return RESTReview{}, err
	}
	return out.review(), nil
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
// (an error matching ErrForbidden or ErrNotFound). The body, which quotes
// the PR, goes as JSON on gh's stdin, so a failing call logs none of it. It
// is marked Mutates, so execx.DryRun only plans it.
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
	return c.restInput(ctx, op, "PUT", path, map[string]string{"body": body}, true, nil)
}
