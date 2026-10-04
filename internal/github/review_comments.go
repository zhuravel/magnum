package github

import (
	"context"
	"fmt"
)

// ReviewComment is one inline comment of a review as REST reports it.
type ReviewComment struct {
	ID      int64
	Path    string
	Line    int // 0 when GitHub reports none (an outdated comment)
	Body    string
	HTMLURL string
}

// ReviewComments lists the inline comments of a review (GET
// /repos/{o}/{r}/pulls/{n}/reviews/{id}/comments), at most 100: the first
// page, which holds every review magnum posts.
func (c *Client) ReviewComments(ctx context.Context, owner, repo string, number int, reviewID int64) ([]ReviewComment, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	if number <= 0 || reviewID <= 0 {
		return nil, fmt.Errorf("github: invalid review %d on pull request %d", reviewID, number)
	}
	var raw []struct {
		ID      int64  `json:"id"`
		Path    string `json:"path"`
		Line    *int   `json:"line"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
	}
	path := fmt.Sprintf("repos/%s/%s/pulls/%d/reviews/%d/comments?per_page=100", owner, repo, number, reviewID)
	op := fmt.Sprintf("review comments %s/%s#%d/%d", owner, repo, number, reviewID)
	if err := c.rest(ctx, op, "GET", path, nil, false, &raw); err != nil {
		return nil, err
	}
	out := make([]ReviewComment, 0, len(raw))
	for _, r := range raw {
		rc := ReviewComment{ID: r.ID, Path: r.Path, Body: r.Body, HTMLURL: r.HTMLURL}
		if r.Line != nil {
			rc.Line = *r.Line
		}
		out = append(out, rc)
	}
	return out, nil
}

// DeletePendingReview deletes a review that was never submitted (DELETE
// /repos/{o}/{r}/pulls/{n}/reviews/{id}) as the client's identity; GitHub
// refuses a submitted review (an *APIError with status 422). It is marked
// Mutates, so execx.DryRun only plans it.
func (c *Client) DeletePendingReview(ctx context.Context, owner, repo string, number int, reviewID int64) error {
	if err := checkRepo(owner, repo); err != nil {
		return err
	}
	if number <= 0 || reviewID <= 0 {
		return fmt.Errorf("github: invalid review %d on pull request %d", reviewID, number)
	}
	path := fmt.Sprintf("repos/%s/%s/pulls/%d/reviews/%d", owner, repo, number, reviewID)
	op := fmt.Sprintf("delete pending review %s/%s#%d/%d", owner, repo, number, reviewID)
	return c.rest(ctx, op, "DELETE", path, nil, true, nil)
}
