package store

// Automatic approvals (migration 0020): approvals magnum posts as the
// operator's own account ([[watch]] auto_approve) on a PR whose review found
// nothing blocking, because GitHub does not count a GitHub App's approval,
// and the PRs on which the operator stopped them.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AutoApproval states (auto_approvals.state).
const (
	AutoPosting    = "posting"    // the approval is being posted
	AutoStanding   = "standing"   // posted, not withdrawn
	AutoDismissing = "dismissing" // magnum is withdrawing it
	AutoDismissed  = "dismissed"  // withdrawn (EndedBy says by whom)
	AutoFailed     = "failed"     // GitHub did not take it
)

// AutoLiveStates are the states of a PR's one live automatic approval.
var AutoLiveStates = []string{AutoPosting, AutoStanding, AutoDismissing}

// Who withdrew an automatic approval (auto_approvals.ended_by).
const (
	AutoEndedMagnum   = "magnum"   // a later review of magnum's found blocking problems
	AutoEndedOperator = "operator" // the operator: magnum unapprove, the board's D, or on GitHub
	AutoEndedSomeone  = "someone"  // someone else dismissed it on GitHub
	AutoEndedPush     = "push"     // GitHub dismissed it as stale when commits were pushed
	AutoEndedGone     = "gone"     // GitHub no longer has it
)

// AutoApproval is one approval magnum posted, or tries to post, as the
// operator's own account.
type AutoApproval struct {
	ID   int64 `json:"id"`
	PRID int64 `json:"pr_id"`
	// RunID is the judge run whose review it follows: one approval per
	// review. SourceReviewID and SourceURL are that review's.
	RunID          string `json:"run_id"`
	SourceReviewID int64  `json:"source_review_id,omitempty"`
	SourceURL      string `json:"source_url,omitempty"`
	HeadSHA        string `json:"head_sha"` // the commit approved: the head magnum reviewed
	Identity       string `json:"identity"` // auto_approve_as
	Login          string `json:"login"`    // its login
	State          string `json:"state"`
	ReviewID       int64  `json:"review_id,omitempty"` // GitHub's id of the approval, once posted
	ReviewURL      string `json:"review_url,omitempty"`
	// Attempts counts the posts tried; Error is the last failure.
	Attempts int    `json:"attempts"`
	Error    string `json:"error,omitempty"`
	// EndedBy (AutoEnded*) and EndReason say who withdrew it and why, set
	// when magnum starts withdrawing it or finds it withdrawn.
	EndedBy   string     `json:"ended_by,omitempty"`
	EndReason string     `json:"end_reason,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	PostedAt  *time.Time `json:"posted_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
}

var autoApprovalColumns = []string{"id", "pr_id", "run_id", "source_review_id", "source_url", "head_sha", "identity", "login", "state",
	"review_id", "review_url", "attempts", "error", "ended_by", "end_reason", "created_at", "posted_at", "ended_at", "updated_at"}

var autoApprovalTable = newTableSpec("auto_approvals", autoApprovalColumns)

// AutoApprovalUpdate is an Update on the auto_approvals table.
type AutoApprovalUpdate struct{ Update }

func scanAutoApproval(sc scanner) (AutoApproval, error) {
	var (
		a                               AutoApproval
		srcID, revID                    *int64
		srcURL, revURL, msg, by, reason *string
	)
	err := sc.Scan(&a.ID, &a.PRID, &a.RunID, &srcID, &srcURL, &a.HeadSHA, &a.Identity, &a.Login, &a.State,
		&revID, &revURL, &a.Attempts, &msg, &by, &reason, timeCol(&a.CreatedAt), nullTime(&a.PostedAt), nullTime(&a.EndedAt), timeCol(&a.UpdatedAt))
	if err != nil {
		return AutoApproval{}, err
	}
	a.SourceReviewID, a.SourceURL, a.ReviewID, a.ReviewURL = Deref(srcID), Deref(srcURL), Deref(revID), Deref(revURL)
	a.Error, a.EndedBy, a.EndReason = Deref(msg), Deref(by), Deref(reason)
	return a, nil
}

// InsertAutoApproval records a post about to be tried (state posting, one
// attempt). It returns ErrConflict when the PR already has a live one.
func (s *Store) InsertAutoApproval(ctx context.Context, a AutoApproval) (AutoApproval, error) {
	now := s.now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO auto_approvals (pr_id, run_id, source_review_id, source_url, head_sha, identity, login,
  state, attempts, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		a.PRID, a.RunID, nullInt(a.SourceReviewID), nullString(a.SourceURL), a.HeadSHA, a.Identity, a.Login, AutoPosting, FormatTime(now), FormatTime(now))
	if err != nil {
		return AutoApproval{}, fmt.Errorf("auto approval of pr %d: %w", a.PRID, mapErr(err))
	}
	id, err := res.LastInsertId()
	if err != nil {
		return AutoApproval{}, err
	}
	return s.AutoApprovalByID(ctx, id)
}

// TransitionAutoApproval moves approval id to state to only if its state is
// one of from (nil = any; empty to keeps the state), with set's assignments
// in the same UPDATE: ErrConflict when the state did not match.
func (s *Store) TransitionAutoApproval(ctx context.Context, id int64, from []string, to string, set func(*AutoApprovalUpdate)) error {
	u := &AutoApprovalUpdate{Update{table: autoApprovalTable}}
	if set != nil {
		set(u)
	}
	return s.transition(ctx, autoApprovalTable, id, from, to, &u.Update)
}

// AutoApprovalByID reads one approval.
func (s *Store) AutoApprovalByID(ctx context.Context, id int64) (AutoApproval, error) {
	a, err := scanAutoApproval(s.db.QueryRowContext(ctx, "SELECT "+cols("", autoApprovalColumns)+" FROM auto_approvals WHERE id = ?", id))
	if err != nil {
		return AutoApproval{}, notFound(err, "auto approval", id)
	}
	return a, nil
}

// LiveAutoApproval is the PR's live approval (posting, standing or
// dismissing), if any.
func (s *Store) LiveAutoApproval(ctx context.Context, prID int64) (AutoApproval, bool, error) {
	return s.oneAutoApproval(ctx, "pr_id = ? AND state IN ("+placeholders(len(AutoLiveStates))+")", append([]any{prID}, anys(AutoLiveStates)...)...)
}

// AutoApprovalOfRun is the latest approval that followed run runID's review
// of the PR, if any.
func (s *Store) AutoApprovalOfRun(ctx context.Context, prID int64, runID string) (AutoApproval, bool, error) {
	return s.oneAutoApproval(ctx, "pr_id = ? AND run_id = ?", prID, runID)
}

func (s *Store) oneAutoApproval(ctx context.Context, where string, args ...any) (AutoApproval, bool, error) {
	a, err := scanAutoApproval(s.db.QueryRowContext(ctx, "SELECT "+cols("", autoApprovalColumns)+" FROM auto_approvals WHERE "+where+
		" ORDER BY id DESC LIMIT 1", args...))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return AutoApproval{}, false, nil
	case err != nil:
		return AutoApproval{}, false, fmt.Errorf("auto approval: %w", err)
	}
	return a, true, nil
}

// LatestAutoApprovals is each PR's latest approval, for the PRs that have
// one.
func (s *Store) LatestAutoApprovals(ctx context.Context, prIDs []int64) (map[int64]AutoApproval, error) {
	out := map[int64]AutoApproval{}
	if len(prIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+cols("", autoApprovalColumns)+" FROM auto_approvals WHERE pr_id IN ("+placeholders(len(prIDs))+
		") ORDER BY pr_id, id DESC", int64Args(prIDs)...)
	if err != nil {
		return nil, fmt.Errorf("auto approvals: %w", err)
	}
	list, err := collect(rows, scanAutoApproval)
	if err != nil {
		return nil, fmt.Errorf("auto approvals: %w", err)
	}
	for _, a := range list {
		if _, seen := out[a.PRID]; !seen {
			out[a.PRID] = a
		}
	}
	return out, nil
}

// AutoApprovalPR is an approval with its PR and repository (owner/name).
type AutoApprovalPR struct {
	AutoApproval
	PR   PR
	Repo string
}

// LiveAutoApprovals lists every live approval with its PR, by repository
// and number.
func (s *Store) LiveAutoApprovals(ctx context.Context) ([]AutoApprovalPR, error) {
	return s.autoApprovalPRs(ctx, "a.state IN ("+placeholders(len(AutoLiveStates))+")", anys(AutoLiveStates)...)
}

// StandingAutoApprovals lists the approvals standing on open PRs, by
// repository and number.
func (s *Store) StandingAutoApprovals(ctx context.Context) ([]AutoApprovalPR, error) {
	return s.autoApprovalPRs(ctx, "a.state = ? AND p.gh_state = ?", AutoStanding, GHOpen)
}

func (s *Store) autoApprovalPRs(ctx context.Context, where string, args ...any) ([]AutoApprovalPR, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+cols("a", autoApprovalColumns)+`, r.owner || '/' || r.name
FROM auto_approvals a JOIN prs p ON p.id = a.pr_id JOIN repos r ON r.id = p.repo_id
WHERE `+where+`
ORDER BY r.owner, r.name, p.number, a.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("auto approvals: %w", err)
	}
	list, err := collect(rows, func(sc scanner) (AutoApprovalPR, error) {
		var out AutoApprovalPR
		a, err := scanAutoApproval(rowWithTail{sc, &out.Repo})
		out.AutoApproval = a
		return out, err
	})
	if err != nil {
		return nil, fmt.Errorf("auto approvals: %w", err)
	}
	for i := range list {
		if list[i].PR, err = s.PRByID(ctx, list[i].PRID); err != nil {
			return nil, fmt.Errorf("auto approvals: %w", err)
		}
	}
	return list, nil
}

// CountAutoApprovedSince counts the approvals posted at or after since,
// withdrawn or not.
func (s *Store) CountAutoApprovedSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM auto_approvals WHERE posted_at >= ?", FormatTime(since)).Scan(&n); err != nil {
		return 0, fmt.Errorf("count auto approvals: %w", err)
	}
	return n, nil
}

// AutoApproveHold is the operator's word on a PR's automatic approvals:
// Held stops them (Reason says why: they dismissed one, reviewed the PR by
// hand, or withdrew it with `magnum unapprove`); a hold lifted with
// `magnum unapprove --resume` stays with Held false, and only what the
// operator did after At counts again.
type AutoApproveHold struct {
	PRID   int64     `json:"pr_id"`
	Held   bool      `json:"held"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// SetAutoApproveHold records h, replacing the PR's earlier hold.
func (s *Store) SetAutoApproveHold(ctx context.Context, h AutoApproveHold) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO auto_approve_holds (pr_id, held, reason, at) VALUES (?, ?, ?, ?)
ON CONFLICT (pr_id) DO UPDATE SET held = excluded.held, reason = excluded.reason, at = excluded.at`,
		h.PRID, boolInt(h.Held), h.Reason, FormatTime(h.At))
	if err != nil {
		return fmt.Errorf("auto approve hold of pr %d: %w", h.PRID, err)
	}
	return nil
}

// AutoApproveHold is the PR's hold, if any.
func (s *Store) AutoApproveHold(ctx context.Context, prID int64) (AutoApproveHold, bool, error) {
	all, err := s.AutoApproveHolds(ctx, []int64{prID})
	if err != nil {
		return AutoApproveHold{}, false, err
	}
	h, ok := all[prID]
	return h, ok, nil
}

// AutoApproveHolds are the holds of the PRs that have one.
func (s *Store) AutoApproveHolds(ctx context.Context, prIDs []int64) (map[int64]AutoApproveHold, error) {
	out := map[int64]AutoApproveHold{}
	if len(prIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, "SELECT pr_id, held, reason, at FROM auto_approve_holds WHERE pr_id IN ("+placeholders(len(prIDs))+")",
		int64Args(prIDs)...)
	if err != nil {
		return nil, fmt.Errorf("auto approve holds: %w", err)
	}
	list, err := collect(rows, func(sc scanner) (AutoApproveHold, error) {
		var h AutoApproveHold
		err := sc.Scan(&h.PRID, &h.Held, &h.Reason, timeCol(&h.At))
		return h, err
	})
	if err != nil {
		return nil, fmt.Errorf("auto approve holds: %w", err)
	}
	for _, h := range list {
		out[h.PRID] = h
	}
	return out, nil
}

// RepoPR is a PR with its repository (owner/name).
type RepoPR struct {
	PR   PR
	Repo string
}

// AutoApproveCandidates lists the open PRs magnum may approve as the
// operator: not drafts, not muted, reviewed (state reviewed, the review on
// the head) and without a live automatic approval, by repository and number.
// Whether the configuration and the review allow it is the caller's.
func (s *Store) AutoApproveCandidates(ctx context.Context) ([]RepoPR, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+cols("p", prColumns)+`, r.owner || '/' || r.name
FROM prs p JOIN repos r ON r.id = p.repo_id
WHERE p.gh_state = ? AND p.state = ? AND p.is_draft = 0 AND p.muted = 0 AND p.reviewed_sha = p.head_sha
  AND NOT EXISTS (SELECT 1 FROM auto_approvals a WHERE a.pr_id = p.id AND a.state IN (`+placeholders(len(AutoLiveStates))+`))
ORDER BY r.owner, r.name, p.number`, append([]any{GHOpen, PRReviewed}, anys(AutoLiveStates)...)...)
	if err != nil {
		return nil, fmt.Errorf("auto approve candidates: %w", err)
	}
	list, err := collect(rows, func(sc scanner) (RepoPR, error) {
		var c RepoPR
		p, err := scanPR(rowWithTail{sc, &c.Repo})
		c.PR = p
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("auto approve candidates: %w", err)
	}
	return list, nil
}
