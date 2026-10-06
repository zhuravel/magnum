package store

// The retro's misses in the notes curation (migration 0014): a curation is
// given its repository's new misses (class miss, scope repo), and its
// proposal says what it did with each (notes_proposal_misses). The misses
// follow the proposal's decision (DecideNotesProposal): applied, they are
// used; rejected or expired, they stay new for the next curation, which
// reads the rejections' reasons; a miss in MissDismissRejections rejected
// proposals is dismissed. The links are never pruned.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// What a proposal did with a miss it was given (notes_proposal_misses.outcome).
const (
	MissNoted   = "noted"   // a note covers it now, in Section
	MissSkipped = "skipped" // no note helps a future review: Reason says why
)

// MissDismissRejections is how many rejected proposals a miss may be in
// before it is dismissed.
const MissDismissRejections = 2

var missOutcomes = []string{MissNoted, MissSkipped}

// ProposalMiss is what a proposal did with one miss it was given.
type ProposalMiss struct {
	MissID  int64  `json:"miss_id"`
	Outcome string `json:"outcome"`           // MissNoted | MissSkipped
	Section string `json:"section,omitempty"` // noted: the "## " section of the proposed notes
	Reason  string `json:"reason,omitempty"`  // skipped: why; noted: optional
	// Miss is the miss as it is now (ProposalMisses fills it).
	Miss *Miss `json:"miss,omitempty"`
}

// linkProposalMisses stores what proposal id did with its misses.
func linkProposalMisses(ctx context.Context, tx *sql.Tx, id int64, links []ProposalMiss) error {
	for _, l := range links {
		if err := oneOf("miss outcome", l.Outcome, missOutcomes); err != nil {
			return err
		}
		if l.MissID == 0 {
			return errors.New("a proposal's miss without an id")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notes_proposal_misses (proposal_id, miss_id, outcome, section, reason)
VALUES (?, ?, ?, ?, ?)`, id, l.MissID, l.Outcome, nullString(l.Section), nullString(l.Reason)); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

// decideProposalMisses moves the misses of proposal id as its new state to
// says: applied, the new ones are used; rejected, a new one that is now in
// MissDismissRejections rejected proposals is dismissed (the others stay
// new); expired or invalid, nothing changes (they stay new).
func decideProposalMisses(ctx context.Context, tx *sql.Tx, id int64, to string, at time.Time) error {
	ofProposal := "state = '" + MissNew + "' AND id IN (SELECT miss_id FROM notes_proposal_misses WHERE proposal_id = ?)"
	var err error
	switch to {
	case ProposalApplied:
		_, err = tx.ExecContext(ctx, "UPDATE misses SET state = ?, updated_at = ? WHERE "+ofProposal, MissUsed, FormatTime(at), id)
	case ProposalRejected:
		_, err = tx.ExecContext(ctx, "UPDATE misses SET state = ?, updated_at = ? WHERE "+ofProposal+` AND
  (SELECT COUNT(*) FROM notes_proposal_misses l JOIN notes_proposals p ON p.id = l.proposal_id
   WHERE l.miss_id = misses.id AND p.state = ?) >= ?`, MissDismissed, FormatTime(at), id, ProposalRejected, MissDismissRejections)
	}
	return err
}

// ProposalMisses lists what proposal id did with the misses it was given,
// by miss id, each with the miss as it is now.
func (s *Store) ProposalMisses(ctx context.Context, id int64) ([]ProposalMiss, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT l.miss_id, l.outcome, l.section, l.reason, `+cols("m", missColumns)+`, `+missListColumns+`
FROM notes_proposal_misses l JOIN misses m ON m.id = l.miss_id JOIN prs p ON p.id = m.pr_id JOIN repos rp ON rp.id = p.repo_id
WHERE l.proposal_id = ? ORDER BY l.miss_id`, id)
	if err != nil {
		return nil, fmt.Errorf("proposal misses: %w", err)
	}
	out, err := collect(rows, func(sc scanner) (ProposalMiss, error) {
		var l ProposalMiss
		var section, reason sql.NullString
		m, err := scanMiss(prefixScanner{sc, []any{&l.MissID, &l.Outcome, &section, &reason}}, true)
		l.Section, l.Reason, l.Miss = section.String, reason.String, &m
		return l, err
	})
	if err != nil {
		return nil, fmt.Errorf("proposal misses: %w", err)
	}
	return out, nil
}

// prefixScanner scans the columns in front of a row before handing the rest
// to another scanner's destinations.
type prefixScanner struct {
	sc     scanner
	prefix []any
}

func (p prefixScanner) Scan(dest ...any) error { return p.sc.Scan(append(p.prefix, dest...)...) }

// MissRejections returns, per miss of ids, the operator's reasons of the
// rejected proposals it was in, oldest first (an empty reason is left out).
func (s *Store) MissRejections(ctx context.Context, ids []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, ProposalRejected)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT l.miss_id, p.reason FROM notes_proposal_misses l JOIN notes_proposals p ON p.id = l.proposal_id
WHERE p.state = ? AND p.reason IS NOT NULL AND p.reason <> '' AND l.miss_id IN (`+placeholders(len(ids))+`) ORDER BY p.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("miss rejections: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var reason string
		if err := rows.Scan(&id, &reason); err != nil {
			return nil, fmt.Errorf("miss rejections: %w", err)
		}
		out[id] = append(out[id], reason)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("miss rejections: %w", err)
	}
	return out, nil
}
