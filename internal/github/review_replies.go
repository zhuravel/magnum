package github

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ReviewCommentReply is a reply posted to a review thread.
type ReviewCommentReply struct {
	ID        int64     // REST comment id
	URL       string    // html_url
	CreatedAt time.Time // created_at
}

// ReplyToReviewComment posts body as a reply in the thread whose first
// comment is commentID (POST /repos/{o}/{r}/pulls/{n}/comments/{id}/replies)
// as the client's identity, the request {"body": …} sent as JSON on gh's
// stdin so no text reaches argv. GitHub refuses a reply to a reply, so
// commentID is the thread's first comment. It is marked Mutates, so
// execx.DryRun only plans it.
func (c *Client) ReplyToReviewComment(ctx context.Context, owner, repo string, number int, commentID int64, body string) (ReviewCommentReply, error) {
	if err := checkRepo(owner, repo); err != nil {
		return ReviewCommentReply{}, err
	}
	if number <= 0 || commentID <= 0 {
		return ReviewCommentReply{}, fmt.Errorf("github: invalid review comment %d on pull request %d", commentID, number)
	}
	if strings.TrimSpace(body) == "" {
		return ReviewCommentReply{}, fmt.Errorf("github: a reply to review comment %d needs a body", commentID)
	}
	var out struct {
		ID        int64     `json:"id"`
		HTMLURL   string    `json:"html_url"`
		CreatedAt time.Time `json:"created_at"`
	}
	path := fmt.Sprintf("repos/%s/%s/pulls/%d/comments/%d/replies", owner, repo, number, commentID)
	op := fmt.Sprintf("reply to review comment %s/%s#%d/%d", owner, repo, number, commentID)
	if err := c.restInput(ctx, op, "POST", path, map[string]string{"body": body}, true, &out); err != nil {
		return ReviewCommentReply{}, err
	}
	return ReviewCommentReply{ID: out.ID, URL: out.HTMLURL, CreatedAt: out.CreatedAt}, nil
}
