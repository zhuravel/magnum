package github

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// IssueComment is a comment on the conversation of an issue or a pull
// request, as GitHub answered its creation or edit.
type IssueComment struct {
	ID  int64  // REST comment id
	URL string // html_url
}

// issueCommentJSON is the part of GitHub's issue comment the client reads.
type issueCommentJSON struct {
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
}

// CreateIssueComment posts body on the conversation of pull request (or
// issue) number (POST /repos/{o}/{r}/issues/{n}/comments) as the client's
// identity, the request {"body": …} sent as JSON on gh's stdin so no text
// reaches argv. It is marked Mutates, so execx.DryRun only plans it.
func (c *Client) CreateIssueComment(ctx context.Context, owner, repo string, number int, body string) (IssueComment, error) {
	if err := checkRepo(owner, repo); err != nil {
		return IssueComment{}, err
	}
	if number <= 0 {
		return IssueComment{}, fmt.Errorf("github: invalid pull request %d", number)
	}
	if strings.TrimSpace(body) == "" {
		return IssueComment{}, fmt.Errorf("github: a comment on %s/%s#%d needs a body", owner, repo, number)
	}
	var out issueCommentJSON
	path := fmt.Sprintf("repos/%s/%s/issues/%d/comments", owner, repo, number)
	op := fmt.Sprintf("comment on %s/%s#%d", owner, repo, number)
	if err := c.restInput(ctx, op, "POST", path, map[string]string{"body": body}, true, &out); err != nil {
		return IssueComment{}, err
	}
	return IssueComment{ID: out.ID, URL: out.HTMLURL}, nil
}

// EditIssueComment replaces the body of issue comment commentID (PATCH
// /repos/{o}/{r}/issues/comments/{id}) as the client's identity, the body
// as JSON on gh's stdin. GitHub lets only the comment's author edit it; a
// comment someone deleted is an error matching ErrNotFound. It is marked
// Mutates, so execx.DryRun only plans it.
func (c *Client) EditIssueComment(ctx context.Context, owner, repo string, commentID int64, body string) (IssueComment, error) {
	if err := checkRepo(owner, repo); err != nil {
		return IssueComment{}, err
	}
	if commentID <= 0 {
		return IssueComment{}, fmt.Errorf("github: invalid issue comment %d", commentID)
	}
	if strings.TrimSpace(body) == "" {
		return IssueComment{}, fmt.Errorf("github: an edit of issue comment %d needs a body", commentID)
	}
	var out issueCommentJSON
	path := fmt.Sprintf("repos/%s/%s/issues/comments/%d", owner, repo, commentID)
	op := fmt.Sprintf("edit issue comment %s/%s/%d", owner, repo, commentID)
	if err := c.restInput(ctx, op, "PATCH", path, map[string]string{"body": body}, true, &out); err != nil {
		return IssueComment{}, err
	}
	return IssueComment{ID: out.ID, URL: out.HTMLURL}, nil
}

// PRComment is a comment on a pull request's conversation (an issue
// comment) as GraphQL reports it. AuthorLogin is the GraphQL login (no
// "[bot]" suffix); compare it with Account and SameAccount.
type PRComment struct {
	ID          int64  // REST comment id
	URL         string // its link: <pull request>#issuecomment-<id>
	Body        string
	CreatedAt   time.Time
	AuthorLogin string // "" for a ghost
	AuthorType  string // GraphQL __typename: User | Bot | ...
	// AuthorAssociation is the author's authorAssociation with the
	// repository (OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR, NONE, ...), as
	// the reading identity sees it; "" when GitHub did not say.
	AuthorAssociation string
}

const (
	// issueCommentsPage is how many conversation comments one query of
	// IssueComments reads; issueCommentsMaxPages how many pages it reads at
	// most (1000 comments).
	issueCommentsPage     = 100
	issueCommentsMaxPages = 10
)

var issueCommentsQuery = fmt.Sprintf(`query($owner: String!, $name: String!, $number: Int!, $cursor: String) {
  `+rateLimitFields+`
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      comments(last: %d, before: $cursor) {
        pageInfo { hasPreviousPage startCursor }
        nodes { databaseId url body createdAt author { login __typename } authorAssociation }
      }
    }
  }
}`, issueCommentsPage)

// IssueComments lists the comments on the conversation of pull request
// number (review bodies and inline comments are not among them: AllReviews
// and ReviewThreads read those), oldest first. It reads issueCommentsPage
// at a time from the newest back, until a page holds a comment that stop
// (nil: none) reports true for, or it reached the first comment, and then
// returns every comment it read; complete is false when it stopped at
// issueCommentsMaxPages pages with older comments, none of which stop was
// asked about, left unread. A missing repository or pull request is an
// error matching ErrNotFound.
func (c *Client) IssueComments(ctx context.Context, owner, repo string, number int, stop func(PRComment) bool) (comments []PRComment, complete bool, err error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, false, err
	}
	if number <= 0 {
		return nil, false, fmt.Errorf("github: invalid pull request number %d", number)
	}
	op := fmt.Sprintf("issue comments %s/%s#%d", owner, repo, number)
	var pages [][]PRComment // newest page first
	var cursor any          // nil: the newest page
	for range issueCommentsMaxPages {
		var data struct {
			Repository *struct {
				PullRequest *struct {
					Comments struct {
						PageInfo struct {
							HasPreviousPage bool   `json:"hasPreviousPage"`
							StartCursor     string `json:"startCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							DatabaseID int64      `json:"databaseId"`
							URL        string     `json:"url"`
							Body       string     `json:"body"`
							CreatedAt  time.Time  `json:"createdAt"`
							Author     *actorJSON `json:"author"`
							Assoc      string     `json:"authorAssociation"`
						} `json:"nodes"`
					} `json:"comments"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": owner, "name": repo, "number": number, "cursor": cursor}
		_, notFound, err := c.graphql(ctx, op, issueCommentsQuery, vars, &data)
		if err != nil {
			return nil, false, err
		}
		if len(notFound) > 0 || data.Repository == nil || data.Repository.PullRequest == nil {
			return nil, false, &APIError{Op: op, Errors: notFoundOr(notFound, op)}
		}
		cs := data.Repository.PullRequest.Comments
		page := make([]PRComment, 0, len(cs.Nodes))
		found := false
		for _, n := range cs.Nodes {
			pc := PRComment{ID: n.DatabaseID, URL: n.URL, Body: n.Body, CreatedAt: n.CreatedAt, AuthorAssociation: n.Assoc}
			if n.Author != nil {
				pc.AuthorLogin, pc.AuthorType = n.Author.Login, n.Author.Typename
			}
			found = found || (stop != nil && stop(pc))
			page = append(page, pc)
		}
		pages = append(pages, page)
		if found || !cs.PageInfo.HasPreviousPage || cs.PageInfo.StartCursor == "" {
			return joinPages(pages), true, nil
		}
		cursor = cs.PageInfo.StartCursor
	}
	return joinPages(pages), false, nil
}

// joinPages is pages, newest first, as one list oldest first.
func joinPages(pages [][]PRComment) []PRComment {
	out := []PRComment{}
	for _, p := range slices.Backward(pages) {
		out = append(out, p...)
	}
	return out
}
