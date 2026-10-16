package github

import (
	"context"
	"fmt"
	"slices"
)

// answersPage is how many reviews one ReviewAnswers call asks about, the
// most nodes(ids:) takes.
const answersPage = 100

// reviewAnswersQuery reads, by node id, each review's inline comments with
// the author of the thread each replies to (reviewCommentsFields, as
// Details' replies connection reads them).
const reviewAnswersQuery = `query($ids: [ID!]!) {
  ` + rateLimitFields + `
  nodes(ids: $ids) { ... on PullRequestReview { id ` + reviewCommentsFields + ` } }
}`

// ReviewAnswers reads what the inline comments of each review answer
// (ReviewAnswer), by the review's node id (Remark.ID): Details reads it
// for a pull request's last two reviews only (Remark.AnswersKnown), and the
// poll asks for the reviews it needs beyond them (DECISIONS "An author's own
// inline threads are not replies"). A review GitHub does not know is
// missing from the result. Each call asks about answersPage reviews and
// costs 1 point: each review's comments connection is one request of
// GitHub's count, so a hundred reviews are a hundred requests (GitHub's dry
// run, 2026-10-16: 1 point for 1, 10, 40 and 100 reviews). A call that fails
// ends the read: the answers read before it come back with the error.
func (c *Client) ReviewAnswers(ctx context.Context, ids []string) (map[string]ReviewAnswer, error) {
	out := map[string]ReviewAnswer{}
	for chunk := range slices.Chunk(ids, answersPage) {
		var data struct {
			Nodes []*struct {
				ID       string             `json:"id"`
				Comments reviewCommentsJSON `json:"comments"`
			} `json:"nodes"`
		}
		op := fmt.Sprintf("review answers (%d)", len(chunk))
		if _, _, err := c.graphql(ctx, op, reviewAnswersQuery, map[string]any{"ids": chunk}, &data); err != nil {
			return out, err
		}
		for _, n := range data.Nodes {
			if n != nil && n.ID != "" { // nil: gone (NOT_FOUND)
				out[n.ID] = n.Comments.answer()
			}
		}
	}
	return out, nil
}
