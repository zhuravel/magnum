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
	// Files are the paths the pull request changes at HeadRefOid, in
	// GitHub's order: one page of at most 100 (a rename lists its new path
	// only); nil when GitHub returned no list. FilesComplete is true when
	// they are every changed file.
	Files         []string
	FilesComplete bool
	// CI is the head commit's checks.
	CI CIRollup
	// ActivityAt is the pull request's last activity: the latest of its
	// opening, a description edit, the head commit (its committer date, never
	// past UpdatedAt), a force push, a comment, a review (a reply in a thread
	// is one), a label added or removed, a review requested or removed, ready
	// for review or back to draft, a title rename, a base change, a close, a
	// reopen and a merge (activityTypes; bots count, checks do not). Unlike
	// UpdatedAt it does not move for what a reviewer never sees (a project
	// field, a resolved thread, someone's pending review, a deleted comment).
	// Zero when GitHub returned no timeline.
	ActivityAt time.Time
}

// CIRollup is the status check rollup of a pull request's head commit.
type CIRollup struct {
	SHA   string // the commit; "" when GitHub listed none
	State string // GitHub's rollup: SUCCESS | FAILURE | PENDING | ERROR | EXPECTED; "" when the commit has no checks
	// Total is len(Checks) plus the checks GitHub did not return (past its
	// page of 100).
	Total int
	// Complete is true when Checks is every check: GitHub returned a single
	// page of at most 100.
	Complete bool
	// Checks is the latest run of each (Workflow, Name), in GitHub's order;
	// never nil. A re-run, or a SKIPPED run left from a draft, supersedes or
	// is superseded by the same job's other runs.
	Checks []Check
}

// Check is one check run or commit status of a CIRollup.
type Check struct {
	Name  string // the check run's name or the status's context
	State string // CheckPassed | CheckFailed | CheckPending | CheckSkipped
	// Workflow is the GitHub Actions workflow whose run the check belongs
	// to; "" for a commit status or another app's check run.
	Workflow string
	// At is when the check finished, else started (a check run), or was
	// posted (a commit status); zero for a check run not started yet.
	At time.Time
}

// Check.State values.
const (
	CheckPassed  = "passed"  // a check run's SUCCESS or NEUTRAL, a status's SUCCESS
	CheckFailed  = "failed"  // FAILURE, TIMED_OUT, CANCELLED, ACTION_REQUIRED, STARTUP_FAILURE, STALE; a status's FAILURE or ERROR
	CheckPending = "pending" // a check run not COMPLETED, a status PENDING or EXPECTED
	CheckSkipped = "skipped" // SKIPPED
)

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
	// MergeCommitOid is the commit the merge put on the base branch: the
	// merge commit, the squash commit or the last rebased commit ("" until
	// merged). Its first parent is the base branch before the merge.
	MergeCommitOid string
}

// detailsFragment reads labels, latestReviews, the changed files and the
// head commit's checks in one page of 100 (GitHub's maximum); pageInfo and
// totalCount tell whether that page was everything (PRDetails.LabelsComplete,
// PRDetails.LatestReviewsComplete, PRDetails.FilesComplete,
// CIRollup.Complete). The checks add two connections per pull request (3
// points instead of 2 for a batch of 40); with each check's workflow (three
// objects) a batch stays under 30k of GitHub's 500k nodes. The files add one
// connection and 100 nodes per pull request: GitHub's dry run
// (rateLimit(dryRun: true), 2026-10-06) priced batches of 1, 10, 20 and 40
// at 1, 1, 1 and 3 points without them and 1, 1, 2 and 3 with them. A second
// page would be a query of its own (a point each) for every head of a PR
// with more than 100 files, so there is none: FilesComplete says the list
// was cut. The activity timeline (PRDetails.ActivityAt) adds one connection
// of ten small nodes per pull request: the dry run priced batches of 1, 10,
// 20, 30 and 40 at 1, 1, 2, 3 and 4 points with it and 1, 1, 2, 2 and 3
// without (2026-10-06), at most one point more per batch.
const detailsFragment = `fragment PRDetails on PullRequest {
  id number title url
  author { login __typename } authorAssociation
  labels(first: 100) { totalCount pageInfo { hasNextPage } nodes { name } }
  headRefName baseRefName isCrossRepository
  state merged mergedAt closedAt createdAt updatedAt lastEditedAt isDraft headRefOid baseRefOid
  additions deletions changedFiles commits { totalCount }
  files(first: 100) { totalCount pageInfo { hasNextPage } nodes { path } }
  assignees(first: 10) { nodes { login } }
  reviewRequests(first: 30) { nodes { requestedReviewer { __typename ... on User { login } ... on Bot { login } ... on Mannequin { login } ... on Team { slug } } } }
  latestReviews(first: 100) { totalCount pageInfo { hasNextPage } nodes { state submittedAt author { login __typename } commit { oid } } }
  timelineItems(itemTypes: [REVIEW_REQUESTED_EVENT], last: 10) { nodes { ... on ReviewRequestedEvent { createdAt actor { login } requestedReviewer { __typename ... on User { login } ... on Bot { login } ... on Mannequin { login } ... on Team { slug } } } } }
  activity: timelineItems(last: 10, itemTypes: [` + activityTypes + `]) { nodes { __typename ... on PullRequestReview { submittedAt } ` + activityEvents + ` } }
  headCommit: commits(last: 1) { nodes { commit { oid statusCheckRollup { state contexts(first: 100) { totalCount pageInfo { hasNextPage } nodes { __typename ... on CheckRun { name status conclusion startedAt completedAt checkSuite { workflowRun { workflow { name } } } } ... on StatusContext { context state createdAt } } } } committedDate } } }
}`

// activityTypes are the timeline items that are activity on a pull request
// (PRDetails.ActivityAt): what a reviewer sees happen to it. A review counts
// when submitted (submittedAt; a pending one is null), every other item when
// it happened (createdAt, activityEvents).
const activityTypes = "ISSUE_COMMENT, PULL_REQUEST_REVIEW, LABELED_EVENT, UNLABELED_EVENT, " +
	"REVIEW_REQUESTED_EVENT, REVIEW_REQUEST_REMOVED_EVENT, READY_FOR_REVIEW_EVENT, CONVERT_TO_DRAFT_EVENT, RENAMED_TITLE_EVENT, " +
	"BASE_REF_CHANGED_EVENT, AUTOMATIC_BASE_CHANGE_SUCCEEDED_EVENT, CLOSED_EVENT, REOPENED_EVENT, MERGED_EVENT, HEAD_REF_FORCE_PUSHED_EVENT"

// activityEvents selects when each activityTypes item but the review
// happened.
const activityEvents = "... on IssueComment { createdAt } ... on LabeledEvent { createdAt } ... on UnlabeledEvent { createdAt } " +
	"... on ReviewRequestedEvent { createdAt } ... on ReviewRequestRemovedEvent { createdAt } ... on ReadyForReviewEvent { createdAt } " +
	"... on ConvertToDraftEvent { createdAt } ... on RenamedTitleEvent { createdAt } ... on BaseRefChangedEvent { createdAt } " +
	"... on AutomaticBaseChangeSucceededEvent { createdAt } ... on ClosedEvent { createdAt } ... on ReopenedEvent { createdAt } " +
	"... on MergedEvent { createdAt } ... on HeadRefForcePushedEvent { createdAt }"

const stateFragment = `fragment PRState on PullRequest { number state merged mergedAt closedAt headRefOid mergeCommit { oid } }`

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
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
	LastEditedAt      time.Time `json:"lastEditedAt"` // zero (null) when never edited
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
	Files *struct {
		connInfo
		Nodes []struct {
			Path string `json:"path"`
		} `json:"nodes"`
	} `json:"files"` // null: GitHub listed no files
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
	Activity        *struct {
		Nodes []struct {
			CreatedAt   time.Time `json:"createdAt"`   // every item but a review
			SubmittedAt time.Time `json:"submittedAt"` // a review; null while pending
		} `json:"nodes"`
	} `json:"activity"` // null: GitHub returned no timeline
	HeadCommit struct {
		Nodes []struct {
			Commit struct {
				Oid               string    `json:"oid"`
				CommittedDate     time.Time `json:"committedDate"`
				StatusCheckRollup *struct {
					State    string `json:"state"`
					Contexts struct {
						connInfo
						Nodes []struct {
							Typename    string    `json:"__typename"`
							Name        string    `json:"name"`        // CheckRun
							Status      string    `json:"status"`      // CheckRun
							Conclusion  string    `json:"conclusion"`  // CheckRun
							StartedAt   time.Time `json:"startedAt"`   // CheckRun
							CompletedAt time.Time `json:"completedAt"` // CheckRun
							CheckSuite  *struct {
								WorkflowRun *struct {
									Workflow struct {
										Name string `json:"name"`
									} `json:"workflow"`
								} `json:"workflowRun"`
							} `json:"checkSuite"` // CheckRun
							Context   string    `json:"context"`   // StatusContext
							State     string    `json:"state"`     // StatusContext
							CreatedAt time.Time `json:"createdAt"` // StatusContext
						} `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"headCommit"`
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
	if d.Files != nil {
		out.Files = make([]string, 0, len(d.Files.Nodes))
		for _, f := range d.Files.Nodes {
			out.Files = append(out.Files, f.Path)
		}
		out.FilesComplete = d.Files.complete(len(d.Files.Nodes))
	}
	out.ReviewRequestEvents = d.ReviewRequested.events()
	out.CI = d.ci()
	out.ActivityAt = d.activityAt()
	return out
}

// activityAt is PRDetails.ActivityAt: the latest of the activity timeline's
// items, the latest reviews (the timeline may list a review where it was
// begun, so it can fall out of the last ten), the description edit, the
// merge, the close, the opening and the head commit, whose committer date
// is no push time but never after one: capped at updatedAt, which every push
// moves, in case the committer's clock ran ahead. Zero when the timeline is
// missing.
func (d detailsJSON) activityAt() time.Time {
	if d.Activity == nil {
		return time.Time{}
	}
	at := latest(d.CreatedAt, d.LastEditedAt, d.MergedAt, d.ClosedAt)
	for _, n := range d.Activity.Nodes {
		at = latest(at, n.CreatedAt, n.SubmittedAt)
	}
	for _, r := range d.LatestReviews.Nodes {
		at = latest(at, r.SubmittedAt)
	}
	if nodes := d.HeadCommit.Nodes; len(nodes) > 0 {
		c := nodes[len(nodes)-1].Commit.CommittedDate
		if !d.UpdatedAt.IsZero() && c.After(d.UpdatedAt) {
			c = d.UpdatedAt
		}
		at = latest(at, c)
	}
	return at
}

// latest is the latest of ts (zero when all are zero).
func latest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}

// ci is the rollup of the head commit (the last of commits(last: 1)).
func (d detailsJSON) ci() CIRollup {
	out := CIRollup{Complete: true, Checks: []Check{}}
	nodes := d.HeadCommit.Nodes
	if len(nodes) == 0 {
		return out
	}
	c := nodes[len(nodes)-1].Commit
	out.SHA = c.Oid
	r := c.StatusCheckRollup
	if r == nil {
		return out
	}
	out.State = r.State
	out.Complete = r.Contexts.complete(len(r.Contexts.Nodes))
	type key struct{ workflow, name string }
	at := map[key]int{} // index in out.Checks
	for _, n := range r.Contexts.Nodes {
		var c Check
		switch n.Typename {
		case "CheckRun":
			c = Check{Name: n.Name, State: checkRunState(n.Status, n.Conclusion), At: n.CompletedAt}
			if c.At.IsZero() {
				c.At = n.StartedAt
			}
			if n.CheckSuite != nil && n.CheckSuite.WorkflowRun != nil {
				c.Workflow = n.CheckSuite.WorkflowRun.Workflow.Name
			}
		case "StatusContext":
			c = Check{Name: n.Context, State: statusState(n.State), At: n.CreatedAt}
		default:
			continue
		}
		k := key{c.Workflow, c.Name}
		if i, ok := at[k]; !ok {
			at[k] = len(out.Checks)
			out.Checks = append(out.Checks, c)
		} else if !olderRun(c, out.Checks[i]) {
			out.Checks[i] = c
		}
	}
	out.Total = len(out.Checks) + max(0, r.Contexts.TotalCount-len(r.Contexts.Nodes))
	return out
}

// olderRun reports whether check a ran before b, two runs of the same job: a
// run not started yet (zero At) is the newest, and of two equal times the
// later listed wins.
func olderRun(a, b Check) bool {
	switch {
	case a.At.IsZero():
		return false
	case b.At.IsZero():
		return true
	}
	return a.At.Before(b.At)
}

// checkRunState normalizes a check run: not COMPLETED is pending; a
// conclusion that is neither a pass nor SKIPPED (including one GitHub adds
// later) is a failure.
func checkRunState(status, conclusion string) string {
	if status != "COMPLETED" {
		return CheckPending
	}
	switch conclusion {
	case "SUCCESS", "NEUTRAL":
		return CheckPassed
	case "SKIPPED":
		return CheckSkipped
	}
	return CheckFailed
}

// statusState normalizes a commit status (StatusContext.state).
func statusState(state string) string {
	switch state {
	case "SUCCESS":
		return CheckPassed
	case "PENDING", "EXPECTED":
		return CheckPending
	}
	return CheckFailed
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
			State       string    `json:"state"`
			Merged      bool      `json:"merged"`
			MergedAt    time.Time `json:"mergedAt"`
			ClosedAt    time.Time `json:"closedAt"`
			HeadRefOid  string    `json:"headRefOid"`
			MergeCommit *struct {
				Oid string `json:"oid"`
			} `json:"mergeCommit"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		st := PRState{State: s.State, Merged: s.Merged, MergedAt: s.MergedAt, ClosedAt: s.ClosedAt, HeadRefOid: s.HeadRefOid}
		if s.MergeCommit != nil {
			st.MergeCommitOid = s.MergeCommit.Oid
		}
		out[n] = st
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
