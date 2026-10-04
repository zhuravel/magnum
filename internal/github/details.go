package github

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// batchSize caps the pN aliases per batched query.
const batchSize = 40

// PRDetails is what the poller stores for a pull request whose radar row
// changed. Logins are GraphQL logins, which never carry the "[bot]" suffix;
// compare them with SameLogin.
type PRDetails struct {
	NodeID      string
	Number      int
	Title       string
	URL         string
	AuthorLogin string // "" for a deleted (ghost) author
	AuthorType  string // GraphQL __typename: User, Bot, Mannequin, ...
	// AuthorAssociation is the author's relationship with the repository
	// (OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR, FIRST_TIME_CONTRIBUTOR,
	// FIRST_TIMER, MANNEQUIN, NONE), as of now.
	AuthorAssociation string
	Labels            []string
	// LabelsComplete is true when Labels is every label of the pull request.
	// GitHub returned a single page of at most 100 labels; when the
	// connection reports more, the list is truncated and must not be used to
	// decide that a skip label is absent.
	LabelsComplete    bool
	HeadRefName       string
	BaseRefName       string
	IsCrossRepository bool
	State             string // OPEN | CLOSED | MERGED
	Merged            bool
	MergedAt          time.Time // zero when not merged
	ClosedAt          time.Time // zero when open
	UpdatedAt         time.Time
	IsDraft           bool
	HeadRefOid        string
	BaseRefOid        string   // the base branch's current tip
	Assignees         []string // logins; never nil
	ReviewRequests    []Reviewer
	LatestReviews     []LatestReview // latest review per reviewer; never nil
	// LatestReviewsComplete is true when LatestReviews has every reviewer's
	// latest review (one page of at most 100); when false, the
	// identity's own review may be missing from the list.
	LatestReviewsComplete bool
	// ReviewRequestEvents are the newest review requests of the timeline
	// (at most 10, oldest first); never nil.
	ReviewRequestEvents []ReviewRequestEvent
	// Size of the whole pull request (merge base of BaseRefOid ... HeadRefOid).
	Additions    int
	Deletions    int
	ChangedFiles int
	Commits      int
}

// Reviewer is a requested reviewer. For Type "Team", Login is the team slug.
type Reviewer struct {
	Type  string // User | Bot | Mannequin | Team
	Login string
}

// LatestReview is one entry of a pull request's latestReviews.
type LatestReview struct {
	State       string // APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED | PENDING
	SubmittedAt time.Time
	AuthorLogin string // "" for a ghost
	AuthorType  string
	CommitOid   string // "" when the commit is gone
}

// PRState is the closed/merged confirmation of one pull request.
type PRState struct {
	State      string // OPEN | CLOSED | MERGED
	Merged     bool
	MergedAt   time.Time // zero when not merged
	ClosedAt   time.Time // zero when open
	HeadRefOid string
}

// detailsFragment reads labels and latestReviews in one page of 100 (GitHub's
// maximum); pageInfo and totalCount tell whether that page was everything
// (PRDetails.LabelsComplete, PRDetails.LatestReviewsComplete).
const detailsFragment = `fragment PRDetails on PullRequest {
  id number title url
  author { login __typename } authorAssociation
  labels(first: 100) { totalCount pageInfo { hasNextPage } nodes { name } }
  headRefName baseRefName isCrossRepository
  state merged mergedAt closedAt updatedAt isDraft headRefOid baseRefOid
  additions deletions changedFiles commits { totalCount }
  assignees(first: 10) { nodes { login } }
  reviewRequests(first: 30) { nodes { requestedReviewer { __typename ... on User { login } ... on Bot { login } ... on Mannequin { login } ... on Team { slug } } } }
  latestReviews(first: 100) { totalCount pageInfo { hasNextPage } nodes { state submittedAt author { login __typename } commit { oid } } }
  timelineItems(itemTypes: [REVIEW_REQUESTED_EVENT], last: 10) { nodes { ... on ReviewRequestedEvent { createdAt actor { login } requestedReviewer { __typename ... on User { login } ... on Bot { login } ... on Mannequin { login } ... on Team { slug } } } } }
}`

const stateFragment = `fragment PRState on PullRequest { number state merged mergedAt closedAt headRefOid }`

type actorJSON struct {
	Login    string `json:"login"`
	Typename string `json:"__typename"`
}

// connInfo is what a GraphQL connection reports about its size.
type connInfo struct {
	TotalCount int `json:"totalCount"`
	PageInfo   struct {
		HasNextPage bool `json:"hasNextPage"`
	} `json:"pageInfo"`
}

// complete reports whether fetched nodes are the whole connection: no further
// page, and no more items than were fetched.
func (c connInfo) complete(fetched int) bool {
	return !c.PageInfo.HasNextPage && c.TotalCount <= fetched
}

type detailsJSON struct {
	ID     string     `json:"id"`
	Number int        `json:"number"`
	Title  string     `json:"title"`
	URL    string     `json:"url"`
	Author *actorJSON `json:"author"`
	Labels struct {
		connInfo
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	HeadRefName       string    `json:"headRefName"`
	BaseRefName       string    `json:"baseRefName"`
	IsCrossRepository bool      `json:"isCrossRepository"`
	State             string    `json:"state"`
	Merged            bool      `json:"merged"`
	MergedAt          time.Time `json:"mergedAt"`
	ClosedAt          time.Time `json:"closedAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
	AuthorAssociation string    `json:"authorAssociation"`
	IsDraft           bool      `json:"isDraft"`
	HeadRefOid        string    `json:"headRefOid"`
	BaseRefOid        string    `json:"baseRefOid"`
	Additions         int       `json:"additions"`
	Deletions         int       `json:"deletions"`
	ChangedFiles      int       `json:"changedFiles"`
	Commits           struct {
		TotalCount int `json:"totalCount"`
	} `json:"commits"`
	Assignees struct {
		Nodes []struct {
			Login string `json:"login"`
		} `json:"nodes"`
	} `json:"assignees"`
	ReviewRequests struct {
		Nodes []struct {
			RequestedReviewer *struct {
				Typename string `json:"__typename"`
				Login    string `json:"login"`
				Slug     string `json:"slug"`
			} `json:"requestedReviewer"`
		} `json:"nodes"`
	} `json:"reviewRequests"`
	LatestReviews struct {
		connInfo
		Nodes []struct {
			State       string     `json:"state"`
			SubmittedAt time.Time  `json:"submittedAt"`
			Author      *actorJSON `json:"author"`
			Commit      *struct {
				Oid string `json:"oid"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"latestReviews"`
	ReviewRequested reviewRequestsJSON `json:"timelineItems"`
}

func (d detailsJSON) details() PRDetails {
	out := PRDetails{
		NodeID:            d.ID,
		Number:            d.Number,
		Title:             d.Title,
		URL:               d.URL,
		Labels:            []string{},
		HeadRefName:       d.HeadRefName,
		BaseRefName:       d.BaseRefName,
		IsCrossRepository: d.IsCrossRepository,
		State:             d.State,
		Merged:            d.Merged,
		MergedAt:          d.MergedAt,
		ClosedAt:          d.ClosedAt,
		UpdatedAt:         d.UpdatedAt,
		IsDraft:           d.IsDraft,
		HeadRefOid:        d.HeadRefOid,
		BaseRefOid:        d.BaseRefOid,
		Assignees:         []string{},
		ReviewRequests:    []Reviewer{},
		LatestReviews:     []LatestReview{},
		Additions:         d.Additions,
		Deletions:         d.Deletions,
		ChangedFiles:      d.ChangedFiles,
		Commits:           d.Commits.TotalCount,
	}
	if d.Author != nil {
		out.AuthorLogin, out.AuthorType = d.Author.Login, d.Author.Typename
	}
	out.AuthorAssociation = d.AuthorAssociation
	out.LabelsComplete = d.Labels.complete(len(d.Labels.Nodes))
	for _, l := range d.Labels.Nodes {
		out.Labels = append(out.Labels, l.Name)
	}
	for _, a := range d.Assignees.Nodes {
		if a.Login != "" {
			out.Assignees = append(out.Assignees, a.Login)
		}
	}
	for _, n := range d.ReviewRequests.Nodes {
		r := n.RequestedReviewer
		if r == nil {
			continue
		}
		login := r.Login
		if r.Typename == "Team" {
			login = r.Slug
		}
		out.ReviewRequests = append(out.ReviewRequests, Reviewer{Type: r.Typename, Login: login})
	}
	out.LatestReviewsComplete = d.LatestReviews.complete(len(d.LatestReviews.Nodes))
	for _, n := range d.LatestReviews.Nodes {
		lr := LatestReview{State: n.State, SubmittedAt: n.SubmittedAt}
		if n.Author != nil {
			lr.AuthorLogin, lr.AuthorType = n.Author.Login, n.Author.Typename
		}
		if n.Commit != nil {
			lr.CommitOid = n.Commit.Oid
		}
		out.LatestReviews = append(out.LatestReviews, lr)
	}
	out.ReviewRequestEvents = d.ReviewRequested.events()
	return out
}

// Details fetches the full view of the given pull requests of owner/repo,
// batched as aliases p<N> (at most 40 per query). Numbers GitHub cannot
// resolve (NOT_FOUND, including a missing repository) are returned in missing
// instead of failing the call; any other error fails it.
func (c *Client) Details(ctx context.Context, owner, repo string, numbers []int) (map[int]PRDetails, []int, error) {
	out := map[int]PRDetails{}
	missing, err := c.batch(ctx, "details", owner, repo, numbers, "PRDetails", detailsFragment, func(n int, raw json.RawMessage) error {
		var d detailsJSON
		if err := json.Unmarshal(raw, &d); err != nil {
			return err
		}
		out[n] = d.details()
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return out, missing, nil
}

// ConfirmStates reads state/merged/mergedAt/closedAt/headRefOid for pull
// requests that disappeared from the radar's OPEN list. Numbers GitHub cannot
// resolve are returned in notFound (callers treat them as UNKNOWN and never
// clean them up); any other error fails the call.
func (c *Client) ConfirmStates(ctx context.Context, owner, repo string, numbers []int) (map[int]PRState, []int, error) {
	out := map[int]PRState{}
	notFound, err := c.batch(ctx, "confirm", owner, repo, numbers, "PRState", stateFragment, func(n int, raw json.RawMessage) error {
		var s struct {
			State      string    `json:"state"`
			Merged     bool      `json:"merged"`
			MergedAt   time.Time `json:"mergedAt"`
			ClosedAt   time.Time `json:"closedAt"`
			HeadRefOid string    `json:"headRefOid"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		out[n] = PRState(s)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return out, notFound, nil
}

// batch runs one aliased query per chunk of batchSize numbers and calls each
// for every resolved alias. It returns the numbers whose alias came back null.
func (c *Client) batch(ctx context.Context, kind, owner, repo string, numbers []int, fragName, fragment string, each func(int, json.RawMessage) error) ([]int, error) {
	if err := checkRepo(owner, repo); err != nil {
		return nil, err
	}
	nums := slices.Clone(numbers)
	slices.Sort(nums)
	nums = slices.Compact(nums)
	if len(nums) > 0 && nums[0] <= 0 {
		return nil, fmt.Errorf("github %s %s/%s: invalid pull request number %d", kind, owner, repo, nums[0])
	}
	var missing []int
	for chunk := range slices.Chunk(nums, batchSize) {
		var q strings.Builder
		q.WriteString("query($owner: String!, $name: String!) {\n  " + rateLimitFields + "\n  repository(owner: $owner, name: $name) {\n")
		for _, n := range chunk {
			fmt.Fprintf(&q, "    p%d: pullRequest(number: %d) { ...%s }\n", n, n, fragName)
		}
		q.WriteString("  }\n}\n" + fragment)

		var data struct {
			Repository map[string]json.RawMessage `json:"repository"`
		}
		op := fmt.Sprintf("%s %s/%s (%d)", kind, owner, repo, len(chunk))
		if _, _, err := c.graphql(ctx, op, q.String(), map[string]any{"owner": owner, "name": repo}, &data); err != nil {
			return nil, err
		}
		for _, n := range chunk {
			raw, ok := data.Repository[fmt.Sprintf("p%d", n)]
			if !ok || isNull(raw) {
				missing = append(missing, n)
				continue
			}
			if err := each(n, raw); err != nil {
				return nil, fmt.Errorf("github %s: decode #%d: %w", op, n, err)
			}
		}
	}
	return missing, nil
}
