package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// BoardFilter selects the rows of Board. The zero value lists every open PR.
type BoardFilter struct {
	Repo string // "owner/name" or "name", case-insensitive; "" = every repository
	// States, when non-empty, lists exactly the PRs in these automation
	// states (prs.state) and IncludeClosed is ignored.
	States []string
	Limit  int // 0 = no limit
	// IncludeClosed also lists PRs in closed/releasing/released and PRs
	// GitHub reports CLOSED or MERGED.
	IncludeClosed bool
	// ClosedSince, when set, also lists the PRs GitHub merged or closed at
	// or after it (merged_at, else closed_at), without IncludeClosed: the
	// board's recently closed section ([board] recent_closed).
	ClosedSince time.Time
}

// BoardRow is one PR as the board shows it: the prs row flattened with its
// repository and its current slot. Empty strings, zero times and nil
// pointers mean "none".
type BoardRow struct {
	PRID               int64          `json:"pr_id"`
	Ref                string         `json:"ref"` // owner/name#N
	Owner              string         `json:"owner"`
	Name               string         `json:"name"`
	Number             int            `json:"number"`
	Title              string         `json:"title"`
	Author             string         `json:"author"` // Account form: a bot's keeps "[bot]"
	URL                string         `json:"url"`
	Draft              bool           `json:"draft"`
	Labels             []string       `json:"labels"`
	Assignees          []string       `json:"assignees"`
	RequestedReviewers []string       `json:"requested_reviewers"` // teams as "team:<slug>"
	ReviewRequested    bool           `json:"review_requested"`    // the watch's own login is requested
	LatestReviews      []LatestReview `json:"latest_reviews"`
	SinceReview        *SinceReview   `json:"since_review"` // nil until the poller computed it
	State              string         `json:"state"`        // automation state (prs.state)
	SkipReason         string         `json:"skip_reason"`
	GHState            string         `json:"gh_state"`
	UpdatedAt          time.Time      `json:"updated_at"`  // GitHub's updatedAt (prs.gh_updated_at)
	ActivityAt         time.Time      `json:"activity_at"` // the last activity (PR.Activity): prs.activity_at, else UpdatedAt
	HeadSHA            string         `json:"head_sha"`
	ReviewedSHA        string         `json:"reviewed_sha"`
	LastReviewEvent    string         `json:"last_review_event"`
	LastReviewAt       time.Time      `json:"last_review_at"` // prs.reviewed_at
	LastReviewLogin    string         `json:"last_review_login"`
	Identity           string         `json:"identity"`
	Slot               string         `json:"slot"` // name of the slot holding the PR
	SlotPath           string         `json:"slot_path"`
	Pinned             bool           `json:"pinned"`
	Muted              bool           `json:"muted"`
	NextEligibleAt     time.Time      `json:"next_eligible_at"`
	LastError          string         `json:"last_error"`
	RoundsToday        int            `json:"rounds_today"` // 0 when the stored count is from an earlier day
	// CIState is the head's check rollup as last seen (prs.ci_state; "" =
	// none or not seen yet) and CI the last Details' checks (nil until
	// fetched). CI.SHA differs from HeadSHA until the Details of a new head
	// are fetched; CI.State trails CIState while a Details fetch fails.
	CIState string    `json:"ci_state"`
	CI      *CIStatus `json:"ci"`
	// ReviewRequests are the newest review requests of the PR (at most 10,
	// oldest first; empty until the next Details fetch).
	ReviewRequests []ReviewRequest `json:"review_requests"`
	// PrevState is the automation state the PR left when magnum confirmed
	// it closed ("" while open); MergedAt and ClosedAt are GitHub's (zero
	// while open; MergedAt stays zero for a PR closed unmerged).
	PrevState string    `json:"prev_state"`
	MergedAt  time.Time `json:"merged_at"`
	ClosedAt  time.Time `json:"closed_at"`
	// MergedUnreviewed: GitHub merged the PR before magnum reviewed its last
	// push (IsMergedUnreviewed). FlagDismissed: the PR was muted after that
	// merge, so it is not flagged but would be unmuted (IsFlagDismissed).
	MergedUnreviewed bool `json:"merged_unreviewed"`
	FlagDismissed    bool `json:"flag_dismissed"`
	// ReviewGate is what GitHub's merge gate said of the reviews at the last
	// Details fetch (prs.review_gate_json); nil until then.
	ReviewGate *ReviewGate `json:"review_gate"`
}

// DueStates are the automation states in which magnum means to review a PR:
// a round is due or running. Callers must not modify the slice.
var DueStates = []string{PRQueued, PRRereviewPending, PRClaiming, PRReviewing, PRVerifying, PRPaused, PRNeedsAttention}

// IsMergedUnreviewed reports whether GitHub merged a PR before magnum
// reviewed its last push: ghState is MERGED, prevState (the state the PR
// closed in) is one of DueStates, and headSHA is not reviewedSHA ("" =
// never reviewed). A muted PR waits for no round unless it was forced, so it
// is not flagged; baseline, reviewed, skipped and ignored PRs close outside
// DueStates and never are. The forced mark outlives the close, so a PR
// forced before it merged stays flagged after it is muted, until the daemon
// clears the mark on a mute of a PR GitHub no longer lists as open
// (IsFlagDismissed).
func IsMergedUnreviewed(ghState, prevState, headSHA, reviewedSHA string, muted, forced bool) bool {
	return mergedWhileDue(ghState, prevState, headSHA, reviewedSHA) && (!muted || forced)
}

// IsFlagDismissed reports whether a PR is muted without being flagged, though
// it would be flagged unmuted: GitHub merged it in a state where magnum meant
// to review it, before its head was reviewed, and nothing forces a round of
// it. Muting such a PR is how the flag is dismissed; unmuting restores it.
func IsFlagDismissed(ghState, prevState, headSHA, reviewedSHA string, muted, forced bool) bool {
	return mergedWhileDue(ghState, prevState, headSHA, reviewedSHA) && muted && !forced
}

// mergedWhileDue is what IsMergedUnreviewed and IsFlagDismissed share: the
// merge came while a round was due or running, with the head not reviewed.
func mergedWhileDue(ghState, prevState, headSHA, reviewedSHA string) bool {
	return ghState == GHMerged && slices.Contains(DueStates, prevState) && headSHA != reviewedSHA
}

// MergedUnreviewed is IsMergedUnreviewed for p.
func (p PR) MergedUnreviewed() bool {
	return IsMergedUnreviewed(p.GHState, Deref(p.PrevState), p.HeadSHA, Deref(p.ReviewedSHA), p.Muted, p.Forced)
}

// FlagDismissed is IsFlagDismissed for p.
func (p PR) FlagDismissed() bool {
	return IsFlagDismissed(p.GHState, Deref(p.PrevState), p.HeadSHA, Deref(p.ReviewedSHA), p.Muted, p.Forced)
}

// closedPRStates are the automation states IncludeClosed adds.
var closedPRStates = []string{PRClosed, PRReleasing, PRReleased}

// Board returns the PRs the board shows, the latest activity first
// (BoardRow.ActivityAt; PRs never fetched last). It reads only the registry.
func (s *Store) Board(ctx context.Context, f BoardFilter) ([]BoardRow, error) {
	var where []string
	var args []any
	if f.Repo != "" {
		if owner, name, ok := strings.Cut(f.Repo, "/"); ok {
			where = append(where, "r.owner = ? COLLATE NOCASE AND r.name = ? COLLATE NOCASE")
			args = append(args, owner, name)
		} else {
			where = append(where, "r.name = ? COLLATE NOCASE")
			args = append(args, f.Repo)
		}
	}
	switch {
	case len(f.States) > 0:
		where = append(where, "p.state IN ("+placeholders(len(f.States))+")")
		args = append(args, anys(f.States)...)
	case !f.IncludeClosed:
		open := "p.state NOT IN (" + placeholders(len(closedPRStates)) + ") AND p.gh_state NOT IN (?, ?)"
		args = append(args, anys(closedPRStates)...)
		args = append(args, GHClosed, GHMerged)
		if !f.ClosedSince.IsZero() {
			open = "(" + open + ") OR (p.gh_state IN (?, ?) AND COALESCE(p.merged_at, p.closed_at) >= ?)"
			args = append(args, GHClosed, GHMerged, FormatTime(f.ClosedSince))
		}
		where = append(where, "("+open+")")
	}
	q := `SELECT p.id, r.owner, r.name, p.number, p.title,
  CASE WHEN p.author_type = 'Bot' AND p.author_login NOT LIKE '%[bot]' THEN p.author_login || '[bot]' ELSE p.author_login END, p.url, p.is_draft, p.labels_json,
  p.assignees_json, p.requested_reviewers_json, p.review_requested, p.latest_reviews_json, p.since_review_json,
  p.state, p.skip_reason, p.gh_state, p.gh_updated_at, p.head_sha, p.reviewed_sha, p.last_review_event,
  p.reviewed_at, p.last_review_login, p.identity, sl.name, sl.path, p.pinned, p.muted, p.next_eligible_at,
  p.last_error, p.rounds_today, p.rounds_day, p.ci_state, p.ci_json, p.review_requests_json,
  p.prev_state, p.merged_at, p.closed_at, p.forced, COALESCE(p.activity_at, p.gh_updated_at), p.review_gate_json
FROM prs p
JOIN repos r ON r.id = p.repo_id
LEFT JOIN slots sl ON sl.pr_id = p.id AND sl.state <> ?`
	args = append([]any{SlotRemoved}, args...)
	if len(where) > 0 {
		q += "\nWHERE " + strings.Join(where, " AND ")
	}
	q += "\nORDER BY COALESCE(p.activity_at, p.gh_updated_at) IS NULL, COALESCE(p.activity_at, p.gh_updated_at) DESC, p.id DESC"
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("board: %w", err)
	}
	today := DayKey(s.now())
	out, err := collect(rows, func(sc scanner) (BoardRow, error) { return scanBoardRow(sc, today) })
	if err != nil {
		return nil, fmt.Errorf("board: %w", err)
	}
	return out, nil
}

func scanBoardRow(sc scanner, today string) (BoardRow, error) {
	var (
		b                                                              BoardRow
		title, author, skip, reviewed, event, login, slot, path, lastE *string
		roundsDay, ciState, prev                                       *string
		updated, reviewedAt, nextAt, mergedAt, closedAt, activity      *time.Time
		forced                                                         bool
	)
	err := sc.Scan(&b.PRID, &b.Owner, &b.Name, &b.Number, &title, &author, &b.URL, &b.Draft, jsonCol(&b.Labels),
		jsonCol(&b.Assignees), jsonCol(&b.RequestedReviewers), &b.ReviewRequested, jsonCol(&b.LatestReviews),
		jsonCol(&b.SinceReview), &b.State, &skip, &b.GHState, nullTime(&updated), &b.HeadSHA, &reviewed, &event,
		nullTime(&reviewedAt), &login, &b.Identity, &slot, &path, &b.Pinned, &b.Muted, nullTime(&nextAt),
		&lastE, &b.RoundsToday, &roundsDay, &ciState, jsonCol(&b.CI), jsonCol(&b.ReviewRequests),
		&prev, nullTime(&mergedAt), nullTime(&closedAt), &forced, nullTime(&activity), jsonCol(&b.ReviewGate))
	if err != nil {
		return BoardRow{}, err
	}
	b.Ref = fmt.Sprintf("%s/%s#%d", b.Owner, b.Name, b.Number)
	b.Title, b.Author, b.SkipReason = Deref(title), Deref(author), Deref(skip)
	b.ReviewedSHA, b.LastReviewEvent, b.LastReviewLogin = Deref(reviewed), Deref(event), Deref(login)
	b.Slot, b.SlotPath, b.LastError = Deref(slot), Deref(path), Deref(lastE)
	b.UpdatedAt, b.ActivityAt, b.LastReviewAt, b.NextEligibleAt = Deref(updated), Deref(activity), Deref(reviewedAt), Deref(nextAt)
	b.CIState = Deref(ciState)
	b.PrevState, b.MergedAt, b.ClosedAt = Deref(prev), Deref(mergedAt), Deref(closedAt)
	b.MergedUnreviewed = IsMergedUnreviewed(b.GHState, b.PrevState, b.HeadSHA, b.ReviewedSHA, b.Muted, forced)
	b.FlagDismissed = IsFlagDismissed(b.GHState, b.PrevState, b.HeadSHA, b.ReviewedSHA, b.Muted, forced)
	if Deref(roundsDay) != today {
		b.RoundsToday = 0
	}
	for _, p := range []*[]string{&b.Labels, &b.Assignees, &b.RequestedReviewers} {
		if *p == nil {
			*p = []string{}
		}
	}
	if b.LatestReviews == nil {
		b.LatestReviews = []LatestReview{}
	}
	if b.ReviewRequests == nil {
		b.ReviewRequests = []ReviewRequest{}
	}
	return b, nil
}
