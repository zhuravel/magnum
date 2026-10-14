package github

import (
	"context"
	"fmt"
	"strings"
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
