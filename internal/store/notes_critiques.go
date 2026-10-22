package store

// The notes critic's record (migration 0028): every critique of a curation
// proposal with its outcome, decision, reason and the critic's checked and
// losses lists, kept with no retention, so the operator reads them after
// the run directory is pruned (`magnum notes <repo> --decisions`).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Critique outcomes (notes_critiques.outcome; "" while it runs).
const (
	CritiqueApplied   = "applied"   // the critic applied the proposal
	CritiqueRejected  = "rejected"  // the critic rejected it
	CritiqueRecurated = "recurated" // the critic superseded it for a new curation
	CritiqueUndecided = "undecided" // no valid decision after a nudge: an attempt
	CritiqueStopped   = "stopped"   // a shutdown, a pause, an agent that went away: no attempt
	CritiqueChanged   = "changed"   // the notes changed before the apply: critiqued again, no attempt
	CritiqueOvertaken = "overtaken" // the operator decided the proposal first
)

var critiqueOutcomes = []string{CritiqueApplied, CritiqueRejected, CritiqueRecurated, CritiqueUndecided, CritiqueStopped, CritiqueChanged,
	CritiqueOvertaken}

// NotesCritique is one critique of a notes proposal.
type NotesCritique struct {
	ID         int64      `json:"id"`
	ProposalID int64      `json:"proposal_id"`
	RepoID     int64      `json:"repo_id"`
	RunID      string     `json:"run_id"`
	RunDir     string     `json:"run_dir,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	Model      string     `json:"model,omitempty"`
	Target     string     `json:"target_sha256,omitempty"`
	Outcome    string     `json:"outcome,omitempty"`
	Decision   string     `json:"decision,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	Checked    []string   `json:"checked"`
	Losses     []string   `json:"losses"`
	Detail     string     `json:"detail,omitempty"`
}

const critiqueColumns = `id, proposal_id, repo_id, run_id, run_dir, started_at, ended_at, model, target_sha256, outcome, decision, reason,
checked_json, losses_json, detail`

func scanCritique(sc scanner) (NotesCritique, error) {
	var c NotesCritique
	var dir, model, target, outcome, decision, reason, checked, losses, detail sql.NullString
	err := sc.Scan(&c.ID, &c.ProposalID, &c.RepoID, &c.RunID, &dir, timeCol(&c.StartedAt), nullTime(&c.EndedAt), &model, &target, &outcome,
		&decision, &reason, &checked, &losses, &detail)
	if err != nil {
		return c, err
	}
	c.RunDir, c.Model, c.Target, c.Outcome, c.Decision = dir.String, model.String, target.String, outcome.String, decision.String
	c.Reason, c.Detail = reason.String, detail.String
	c.Checked, c.Losses = []string{}, []string{}
	for _, l := range []struct {
		raw sql.NullString
		out *[]string
	}{{checked, &c.Checked}, {losses, &c.Losses}} {
		if l.raw.Valid && l.raw.String != "" {
			if err := json.Unmarshal([]byte(l.raw.String), l.out); err != nil {
				return c, fmt.Errorf("critique %d: %w", c.ID, err)
			}
		}
	}
	return c, nil
}

// CritiqueStart is a critique that starts (StartCritique).
type CritiqueStart struct {
	ProposalID int64
	RepoID     int64
	RunID      string
	RunDir     string
	Model      string
	Target     string    // the fingerprint of the state it reviews
	At         time.Time // zero = now
}

// StartCritique records a critique that starts and returns its id.
func (s *Store) StartCritique(ctx context.Context, in CritiqueStart) (int64, error) {
	if in.ProposalID == 0 || in.RepoID == 0 || in.RunID == "" {
		return 0, errors.New("start critique: a proposal, a repository and a run are required")
	}
	at := in.At
	if at.IsZero() {
		at = s.now()
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO notes_critiques (proposal_id, repo_id, run_id, run_dir, started_at, model, target_sha256)
VALUES (?, ?, ?, ?, ?, ?, ?)`, in.ProposalID, in.RepoID, in.RunID, nullString(in.RunDir), FormatTime(at), nullString(in.Model), nullString(in.Target))
	if err != nil {
		return 0, fmt.Errorf("start critique: %w", mapErr(err))
	}
	return res.LastInsertId()
}

// CritiqueEnd is how a critique ended (EndCritique).
type CritiqueEnd struct {
	Outcome  string // Critique*
	Decision string // the decision it wrote ("" = none valid)
	Reason   string
	Checked  []string
	Losses   []string
	Model    string // the model the answer came from ("" keeps the one it started on)
	Detail   string
	At       time.Time // zero = now
}

// EndCritique records how critique id ended, once: ErrConflict when it
// ended already.
func (s *Store) EndCritique(ctx context.Context, id int64, in CritiqueEnd) error {
	if err := oneOf("critique outcome", in.Outcome, critiqueOutcomes); err != nil {
		return err
	}
	at := in.At
	if at.IsZero() {
		at = s.now()
	}
	list := func(xs []string) (any, error) {
		if len(xs) == 0 {
			return nil, nil
		}
		b, err := json.Marshal(xs)
		return string(b), err
	}
	checked, err := list(in.Checked)
	if err != nil {
		return err
	}
	losses, err := list(in.Losses)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE notes_critiques SET ended_at = ?, outcome = ?, decision = ?, reason = ?, checked_json = ?,
  losses_json = ?, model = COALESCE(?, model), detail = ? WHERE id = ? AND outcome IS NULL`, FormatTime(at), in.Outcome, nullString(in.Decision),
		nullString(in.Reason), checked, losses, nullString(in.Model), nullString(in.Detail), id)
	if err != nil {
		return fmt.Errorf("end critique %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("end critique %d: %w: not running", id, ErrConflict)
	}
	return nil
}

// StopOpenCritiques ends every critique still running (a daemon that
// stopped mid-critique: no critique survives a restart) as stopped, with
// detail, and reports how many.
func (s *Store) StopOpenCritiques(ctx context.Context, detail string, at time.Time) (int64, error) {
	if at.IsZero() {
		at = s.now()
	}
	res, err := s.db.ExecContext(ctx, "UPDATE notes_critiques SET ended_at = ?, outcome = ?, detail = ? WHERE outcome IS NULL",
		FormatTime(at), CritiqueStopped, nullString(detail))
	if err != nil {
		return 0, fmt.Errorf("stop open critiques: %w", err)
	}
	return res.RowsAffected()
}

// NotesCritiqueFilter selects critiques: of a repository and of a proposal
// (0 = any), newest first, at most Limit (0 = all).
type NotesCritiqueFilter struct {
	RepoID     int64
	ProposalID int64
	Limit      int
}

// NotesCritiques lists the critiques f selects.
func (s *Store) NotesCritiques(ctx context.Context, f NotesCritiqueFilter) ([]NotesCritique, error) {
	q := "SELECT " + critiqueColumns + " FROM notes_critiques WHERE 1=1"
	var args []any
	if f.RepoID != 0 {
		q += " AND repo_id = ?"
		args = append(args, f.RepoID)
	}
	if f.ProposalID != 0 {
		q += " AND proposal_id = ?"
		args = append(args, f.ProposalID)
	}
	q += " ORDER BY id DESC"
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("notes critiques: %w", err)
	}
	out, err := collect(rows, scanCritique)
	if err != nil {
		return nil, fmt.Errorf("notes critiques: %w", err)
	}
	return out, nil
}

// CountCritiquesSince counts the critiques started at or after since (the
// daily cap: since is the local day's start).
func (s *Store) CountCritiquesSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM notes_critiques WHERE started_at >= ?", FormatTime(since)).Scan(&n)
	return n, err
}
