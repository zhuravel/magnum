package github

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// Thread is an inline review thread of a pull request as GraphQL reports it.
type Thread struct {
	ID           string // GraphQL node id (PRRT_…)
	Path         string
	Line         int // 0 when GitHub reports none (an outdated thread); the last line of a multi-line thread
	OriginalLine int // the line the thread was started on (the last line of a multi-line range)
	// StartLine is the first line of a multi-line thread's range, so the
	// thread covers StartLine..Line; 0 for a single-line thread (and when
	// GitHub reports none, as for an outdated thread). OriginalStartLine is
	// the same for the range the thread was started on (OriginalLine is its
	// last line).
	StartLine         int
	OriginalStartLine int
	// DiffSide is the side of the diff the thread is on: "RIGHT" (the
	// new file's lines) or "LEFT" (deleted lines, numbered in the old file).
	DiffSide string
	Resolved bool
	Outdated bool
	// Comments are oldest first; Comments[0] started the thread and the rest
	// are its replies. At most threadCommentsMax per thread: the first ones.
	Comments []ThreadComment
	// CommentCount is how many comments the thread has, Comments' and
	// beyond. Tail is set only when it has more than Comments holds: its
	// newest comments (at most threadTailMax) that Comments does not hold,
	// oldest first, so that the thread's latest comment is Tail's last;
	// CommentCount minus both is the middle neither holds. Callers that
	// read only Comments see the first threadCommentsMax as before.
	CommentCount int
	Tail         []ThreadComment
}

// AllComments are t's comments as ReviewThreads read them, oldest first:
// Comments, then Tail; its last is the thread's latest comment.
func (t Thread) AllComments() []ThreadComment {
	if len(t.Tail) == 0 {
		return t.Comments
	}
	return append(slices.Clone(t.Comments), t.Tail...)
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
	// AuthorAssociation is the author's authorAssociation with the
	// repository (OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR, NONE, ...), as
	// the reading identity sees it; "" when GitHub did not say.
	AuthorAssociation string
	// OriginalCommitOid is the commit the comment was made on, and DiffHunk
	// the diff hunk it was made on (the lines around the commented one, as
	// they were then). Both are set on a thread's first comment (Comments[0])
	// only, the comment that anchors the thread; replies carry neither, and
	// OriginalCommitOid is "" when GitHub reports no commit (it is
	// gone).
	OriginalCommitOid string
	DiffHunk          string
}

const (
	threadsPageSize   = 100
	threadsMaxPages   = 10
	threadCommentsMax = 50
	// threadTailMax is how many of a long thread's newest comments
	// ReviewThreads reads for Tail; threadTailBatch how many threads one
	// query of tails asks for.
	threadTailMax   = 50
	threadTailBatch = 50
)

// threadCommentFields selects one comment of a review thread
// (threadCommentJSON).
const threadCommentFields = "databaseId body url createdAt authorAssociation author { login __typename } pullRequestReview { databaseId }"

var threadsQuery = fmt.Sprintf(`query($owner: String!, $name: String!, $number: Int!, $cursor: String) {
  `+rateLimitFields+`
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: %d, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id isResolved isOutdated path line originalLine startLine originalStartLine diffSide
          comments(first: %d) {
            totalCount
            nodes { `+threadCommentFields+` }
          }
          root: comments(first: 1) { nodes { originalCommit { oid } diffHunk } }
        }
      }
    }
  }
}`, threadsPageSize, threadCommentsMax)

// threadCommentJSON is one comment of a review thread as
// threadCommentFields reads it.
type threadCommentJSON struct {
	DatabaseID        int64      `json:"databaseId"`
	Body              string     `json:"body"`
	URL               string     `json:"url"`
	CreatedAt         time.Time  `json:"createdAt"`
	Author            *actorJSON `json:"author"`
	AuthorAssociation string     `json:"authorAssociation"`
	PullRequestReview *struct {
		DatabaseID int64 `json:"databaseId"`
	} `json:"pullRequestReview"`
}

func (cm threadCommentJSON) comment() ThreadComment {
	tc := ThreadComment{ID: cm.DatabaseID, Body: cm.Body, URL: cm.URL, CreatedAt: cm.CreatedAt, AuthorAssociation: cm.AuthorAssociation}
	if cm.Author != nil {
		tc.AuthorLogin, tc.AuthorType = cm.Author.Login, cm.Author.Typename
	}
	if cm.PullRequestReview != nil {
		tc.ReviewID = cm.PullRequestReview.DatabaseID
	}
	return tc
}

// threadTailsQuery reads the newest comments of review threads by node id.
var threadTailsQuery = fmt.Sprintf(`query($ids: [ID!]!) {
  `+rateLimitFields+`
  nodes(ids: $ids) {
    ... on PullRequestReviewThread { id comments(last: %d) { nodes { `+threadCommentFields+` } } }
  }
}`, threadTailMax)

// ReviewThreads lists the inline review threads of a pull request with their
// comments, oldest thread first (at most threadsMaxPages pages of
// threadsPageSize threads, threadCommentsMax comments each, and the newest
// threadTailMax of a longer thread as its Tail, read in one more query per
// threadTailBatch such threads). The diff hunk and the original commit are
// read once per thread, for its first comment only (see ThreadComment), so
// replies cost no hunk. A missing repository or pull request is an error
// matching ErrNotFound.
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
							ID                string `json:"id"`
							IsResolved        bool   `json:"isResolved"`
							IsOutdated        bool   `json:"isOutdated"`
							Path              string `json:"path"`
							Line              *int   `json:"line"`
							OriginalLine      *int   `json:"originalLine"`
							StartLine         *int   `json:"startLine"`
							OriginalStartLine *int   `json:"originalStartLine"`
							DiffSide          string `json:"diffSide"`
							Comments          struct {
								TotalCount int                 `json:"totalCount"`
								Nodes      []threadCommentJSON `json:"nodes"`
							} `json:"comments"`
							// Root is the first comment again (a "root" alias of
							// comments(first: 1)) with the fields only it needs.
							Root struct {
								Nodes []struct {
									OriginalCommit *struct {
										Oid string `json:"oid"`
									} `json:"originalCommit"`
									DiffHunk string `json:"diffHunk"`
								} `json:"nodes"`
							} `json:"root"`
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
			t := Thread{ID: n.ID, Path: n.Path, DiffSide: n.DiffSide, Resolved: n.IsResolved, Outdated: n.IsOutdated, CommentCount: n.Comments.TotalCount}
			if n.Line != nil {
				t.Line = *n.Line
			}
			if n.OriginalLine != nil {
				t.OriginalLine = *n.OriginalLine
			}
			if n.StartLine != nil {
				t.StartLine = *n.StartLine
			}
			if n.OriginalStartLine != nil {
				t.OriginalStartLine = *n.OriginalStartLine
			}
			for _, cm := range n.Comments.Nodes {
				t.Comments = append(t.Comments, cm.comment())
			}
			t.CommentCount = max(t.CommentCount, len(t.Comments))
			if len(t.Comments) > 0 && len(n.Root.Nodes) > 0 {
				root := n.Root.Nodes[0]
				t.Comments[0].DiffHunk = root.DiffHunk
				if root.OriginalCommit != nil {
					t.Comments[0].OriginalCommitOid = root.OriginalCommit.Oid
				}
			}
			out = append(out, t)
		}
		if !rt.PageInfo.HasNextPage || rt.PageInfo.EndCursor == "" {
			break
		}
		cursor = rt.PageInfo.EndCursor
	}
	if err := c.readTails(ctx, op, out); err != nil {
		return nil, err
	}
	return out, nil
}

// readTails sets the Tail of each thread of ts that has more comments than
// it holds: the newest threadTailMax comments, those already in Comments
// left out, threadTailBatch threads a query.
func (c *Client) readTails(ctx context.Context, op string, ts []Thread) error {
	var long []int
	for i, t := range ts {
		if t.CommentCount > len(t.Comments) && t.ID != "" {
			long = append(long, i)
		}
	}
	for batch := range slices.Chunk(long, threadTailBatch) {
		ids := make([]string, len(batch))
		at := make(map[string]int, len(batch))
		for k, i := range batch {
			ids[k], at[ts[i].ID] = ts[i].ID, i
		}
		var data struct {
			Nodes []*struct {
				ID       string `json:"id"`
				Comments struct {
					Nodes []threadCommentJSON `json:"nodes"`
				} `json:"comments"`
			} `json:"nodes"`
		}
		if _, _, err := c.graphql(ctx, op+" (newest comments)", threadTailsQuery, map[string]any{"ids": ids}, &data); err != nil {
			return err
		}
		for _, n := range data.Nodes {
			if n == nil {
				continue
			}
			i, ok := at[n.ID]
			if !ok {
				continue
			}
			for _, cm := range n.Comments.Nodes {
				if !slices.ContainsFunc(ts[i].Comments, func(x ThreadComment) bool { return x.ID == cm.DatabaseID }) {
					ts[i].Tail = append(ts[i].Tail, cm.comment())
				}
			}
		}
	}
	return nil
}
