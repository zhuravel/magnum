package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// CreateSession inserts a session and returns it. Role is any non-empty name
// (config-defined; see RoleJudge for the defaults). Generation 0 means "next
// generation for (pr, role)"; StartedAt defaults to now; a nil Env stores {}.
// A second starting/live session for the same (pr, role) or agent name is
// ErrConflict.
func (s *Store) CreateSession(ctx context.Context, x Session) (Session, error) {
	if x.PRID == 0 || x.Role == "" || x.State == "" {
		return Session{}, fmt.Errorf("create session: pr_id, role and state are required")
	}
	started := x.StartedAt
	if started.IsZero() {
		started = s.now()
	}
	env := x.Env
	if env == nil {
		env = map[string]string{}
	}
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		gen := x.Generation
		if gen == 0 {
			if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(generation), 0) + 1 FROM sessions WHERE pr_id = ? AND role = ?",
				x.PRID, x.Role).Scan(&gen); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `
INSERT INTO sessions (pr_id, role, generation, agent_name, agent_kind, session_id, resumed_from, herdr_workspace_id,
  herdr_tab_id, herdr_pane_id, cwd, env_json, state, agent_status, agent_status_at, idle_ticks, started_at,
  last_prompt_at, closed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			x.PRID, x.Role, gen, x.AgentName, x.AgentKind, x.SessionID, x.ResumedFrom, x.HerdrWorkspaceID,
			x.HerdrTabID, x.HerdrPaneID, x.Cwd, mustDB(env), x.State, x.AgentStatus, mustDB(x.AgentStatusAt),
			x.IdleTicks, FormatTime(started), mustDB(x.LastPromptAt), mustDB(x.ClosedAt))
		if err != nil {
			return mapErr(err)
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return Session{}, fmt.Errorf("create session pr %d %s: %w", x.PRID, x.Role, err)
	}
	return s.SessionByID(ctx, id)
}

// SessionByID looks up a session by id.
func (s *Store) SessionByID(ctx context.Context, id int64) (Session, error) {
	x, err := scanSession(s.db.QueryRowContext(ctx, "SELECT "+cols("", sessionColumns)+" FROM sessions WHERE id = ?", id))
	if err != nil {
		return Session{}, notFound(err, "session", id)
	}
	return x, nil
}

// LiveSessionByPRRole returns the starting/live session of a PR's role.
func (s *Store) LiveSessionByPRRole(ctx context.Context, prID int64, role string) (Session, error) {
	x, err := scanSession(s.db.QueryRowContext(ctx, "SELECT "+cols("", sessionColumns)+
		" FROM sessions WHERE pr_id = ? AND role = ? AND state IN ("+placeholders(len(liveSessionStates))+")",
		append([]any{prID, role}, anys(liveSessionStates)...)...))
	if err != nil {
		return Session{}, notFound(err, "live session", fmt.Sprintf("pr %d %s", prID, role))
	}
	return x, nil
}

// SessionsByPR returns every session of a PR, oldest first.
func (s *Store) SessionsByPR(ctx context.Context, prID int64) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+cols("", sessionColumns)+" FROM sessions WHERE pr_id = ? ORDER BY id", prID)
	if err != nil {
		return nil, fmt.Errorf("sessions of pr %d: %w", prID, err)
	}
	return collect(rows, scanSession)
}

// LiveSessions returns every starting/live session, oldest first.
func (s *Store) LiveSessions(ctx context.Context) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+cols("", sessionColumns)+
		" FROM sessions WHERE state IN ("+placeholders(len(liveSessionStates))+") ORDER BY id", anys(liveSessionStates)...)
	if err != nil {
		return nil, fmt.Errorf("live sessions: %w", err)
	}
	return collect(rows, scanSession)
}

// CreateRun inserts a run and returns it. Role is any non-empty name. An
// empty ID is generated as "r-<UTC yyyymmddThhmmss>-<n>"; CreatedAt defaults
// to now.
func (s *Store) CreateRun(ctx context.Context, r Run) (Run, error) {
	if r.PRID == 0 || r.Role == "" || r.Kind == "" || r.State == "" || r.TargetSHA == "" {
		return Run{}, fmt.Errorf("create run: pr_id, role, kind, state and target_sha are required")
	}
	created := r.CreatedAt
	if created.IsZero() {
		created = s.now()
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if r.ID == "" {
			var n int64
			if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(rowid), 0) + 1 FROM runs").Scan(&n); err != nil {
				return err
			}
			r.ID = fmt.Sprintf("r-%s-%d", created.UTC().Format("20060102T150405"), n)
		}
		_, err := tx.ExecContext(ctx, `
INSERT INTO runs (id, pr_id, round, role, session_id, kind, target_sha, prev_reviewed_sha, identity, reviewer_login,
  state, outcome, report_path, review_id, review_event, review_commit, review_url, result_json, prompt_text,
  created_at, submitted_at, working_seen_at, ended_at, verified_at, error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.PRID, r.Round, r.Role, r.SessionID, r.Kind, r.TargetSHA, r.PrevReviewedSHA, r.Identity,
			r.ReviewerLogin, r.State, r.Outcome, r.ReportPath, r.ReviewID, r.ReviewEvent, r.ReviewCommit,
			r.ReviewURL, r.ResultJSON, r.PromptText, FormatTime(created), mustDB(r.SubmittedAt),
			mustDB(r.WorkingSeenAt), mustDB(r.EndedAt), mustDB(r.VerifiedAt), r.Error)
		return mapErr(err)
	})
	if err != nil {
		return Run{}, fmt.Errorf("create run pr %d %s: %w", r.PRID, r.Role, err)
	}
	return s.RunByID(ctx, r.ID)
}

// RunByID looks up a run by id.
func (s *Store) RunByID(ctx context.Context, id string) (Run, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx, "SELECT "+cols("", runColumns)+" FROM runs WHERE id = ?", id))
	if err != nil {
		return Run{}, notFound(err, "run", id)
	}
	return r, nil
}

// RunsByPR returns every run of a PR, oldest first.
func (s *Store) RunsByPR(ctx context.Context, prID int64) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+cols("", runColumns)+" FROM runs WHERE pr_id = ? ORDER BY created_at, rowid", prID)
	if err != nil {
		return nil, fmt.Errorf("runs of pr %d: %w", prID, err)
	}
	return collect(rows, scanRun)
}

// ActiveRuns returns runs still in flight (pending, submitted, working,
// ended-but-unverified), oldest first.
func (s *Store) ActiveRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+cols("", runColumns)+
		" FROM runs WHERE state IN ("+placeholders(len(activeRunStates))+") ORDER BY created_at, rowid", anys(activeRunStates)...)
	if err != nil {
		return nil, fmt.Errorf("active runs: %w", err)
	}
	return collect(rows, scanRun)
}

// LastEndedRun is the run of session sessionID (of PR prID) that ended last
// (by ended_at, whatever its state now); ErrNotFound when none has ended.
func (s *Store) LastEndedRun(ctx context.Context, prID, sessionID int64) (Run, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx, "SELECT "+cols("", runColumns)+
		" FROM runs WHERE pr_id = ? AND session_id = ? AND ended_at IS NOT NULL ORDER BY ended_at DESC, rowid DESC LIMIT 1", prID, sessionID))
	if err != nil {
		return Run{}, notFound(err, "ended run of session", sessionID)
	}
	return r, nil
}

// RoleRanBefore reports whether a PR already has an ended or verified run of
// role, the "runs = first" test: a role configured to run once per PR is
// skipped after its first completed run. Pending, in-flight, failed and
// abandoned runs do not count.
func (s *Store) RoleRanBefore(ctx context.Context, prID int64, role string) (bool, error) {
	var ran bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM runs WHERE pr_id = ? AND role = ? AND state IN (?, ?))",
		prID, role, RunEnded, RunVerified).Scan(&ran)
	if err != nil {
		return false, fmt.Errorf("role ran before pr %d %s: %w", prID, role, err)
	}
	return ran, nil
}

// LastRoleRunHead is the head of the PR's latest completed run of role
// (ended or verified), "" when the role never completed one.
func (s *Store) LastRoleRunHead(ctx context.Context, prID int64, role string) (string, error) {
	var sha string
	err := s.db.QueryRowContext(ctx, `SELECT target_sha FROM runs WHERE pr_id = ? AND role = ? AND state IN (?, ?)
ORDER BY created_at DESC LIMIT 1`, prID, role, RunEnded, RunVerified).Scan(&sha)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("last run of %s on pr %d: %w", role, prID, err)
	}
	return sha, nil
}
