package store

import (
	"context"
	"fmt"
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
	Author             string         `json:"author"`
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
	UpdatedAt          time.Time      `json:"updated_at"` // GitHub's updatedAt (prs.gh_updated_at)
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
}

// closedPRStates are the automation states IncludeClosed adds.
var closedPRStates = []string{PRClosed, PRReleasing, PRReleased}

// Board returns the PRs the board shows, most recently updated on GitHub
// first (PRs never fetched last). It reads only the registry.
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
		where = append(where, "p.state NOT IN ("+placeholders(len(closedPRStates))+")", "p.gh_state NOT IN (?, ?)")
		args = append(args, anys(closedPRStates)...)
		args = append(args, GHClosed, GHMerged)
	}
	q := `SELECT p.id, r.owner, r.name, p.number, p.title, p.author_login, p.url, p.is_draft, p.labels_json,
  p.assignees_json, p.requested_reviewers_json, p.review_requested, p.latest_reviews_json, p.since_review_json,
  p.state, p.skip_reason, p.gh_state, p.gh_updated_at, p.head_sha, p.reviewed_sha, p.last_review_event,
  p.reviewed_at, p.last_review_login, p.identity, sl.name, sl.path, p.pinned, p.muted, p.next_eligible_at,
  p.last_error, p.rounds_today, p.rounds_day
FROM prs p
JOIN repos r ON r.id = p.repo_id
LEFT JOIN slots sl ON sl.pr_id = p.id AND sl.state <> ?`
	args = append([]any{SlotRemoved}, args...)
	if len(where) > 0 {
		q += "\nWHERE " + strings.Join(where, " AND ")
	}
	q += "\nORDER BY p.gh_updated_at IS NULL, p.gh_updated_at DESC, p.id DESC"
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
		roundsDay                                                      *string
		updated, reviewedAt, nextAt                                    *time.Time
	)
	err := sc.Scan(&b.PRID, &b.Owner, &b.Name, &b.Number, &title, &author, &b.URL, &b.Draft, jsonCol(&b.Labels),
		jsonCol(&b.Assignees), jsonCol(&b.RequestedReviewers), &b.ReviewRequested, jsonCol(&b.LatestReviews),
		jsonCol(&b.SinceReview), &b.State, &skip, &b.GHState, nullTime(&updated), &b.HeadSHA, &reviewed, &event,
		nullTime(&reviewedAt), &login, &b.Identity, &slot, &path, &b.Pinned, &b.Muted, nullTime(&nextAt),
		&lastE, &b.RoundsToday, &roundsDay)
	if err != nil {
		return BoardRow{}, err
	}
	b.Ref = fmt.Sprintf("%s/%s#%d", b.Owner, b.Name, b.Number)
	b.Title, b.Author, b.SkipReason = Deref(title), Deref(author), Deref(skip)
	b.ReviewedSHA, b.LastReviewEvent, b.LastReviewLogin = Deref(reviewed), Deref(event), Deref(login)
	b.Slot, b.SlotPath, b.LastError = Deref(slot), Deref(path), Deref(lastE)
	b.UpdatedAt, b.LastReviewAt, b.NextEligibleAt = Deref(updated), Deref(reviewedAt), Deref(nextAt)
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
	return b, nil
}
