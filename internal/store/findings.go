package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Finding verdicts (findings.verdict).
const (
	FindingPosted   = "posted"
	FindingRejected = "rejected"
)

// Finding is one finding the judge judged in a posted round, from the
// provenance list of its result file (skills/magnum-review/SKILL.md section
// 8): its title, the sources that raised it (reviewer roles, "judge" for the
// judge's own pass), whether it was posted and, for a rejection, the reason
// code and whether it is nearby.
type Finding struct {
	ID        int64  `json:"id"`
	RunID     string `json:"run_id"`
	PRID      int64  `json:"pr_id"`
	Round     int    `json:"round"`
	FindingID string `json:"finding_id"`
	// Title names the problem in a few words ("" in rows recorded before
	// titles, migration 0021).
	Title      string   `json:"title,omitempty"`
	Severity   string   `json:"severity,omitempty"` // P0..P3 as the judge wrote it
	Path       string   `json:"path,omitempty"`
	Line       int      `json:"line,omitempty"` // 0 = none (a finding in the review body)
	Sources    []string `json:"sources"`
	Verdict    string   `json:"verdict"`               // FindingPosted | FindingRejected
	ReasonCode string   `json:"reason_code,omitempty"` // rejections: duplicate, not_reproducible, ...
	// Nearby marks a pre-existing P1 or P2 the judge proved at the head in
	// or near code the PR changes (SKILL.md section 7): `magnum debt`.
	Nearby    bool      `json:"nearby,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// Repo is the PR's repository (owner/name); FindingsSince and
	// PreExistingFindings fill it, and Number (the PR's) the latter.
	Repo   string `json:"repo,omitempty"`
	Number int    `json:"number,omitempty"`
}

var findingColumns = []string{"id", "run_id", "pr_id", "round", "finding_id", "title", "severity", "path", "line",
	"sources_json", "verdict", "reason_code", "nearby", "created_at"}

// RecordFindings replaces the findings recorded for runID with fs in one
// transaction (each row gets runID, prID and round; CreatedAt defaults to
// now), so recording a round's result again is harmless. Every finding needs
// a FindingID unique within fs and a verdict of FindingPosted or
// FindingRejected.
func (s *Store) RecordFindings(ctx context.Context, runID string, prID int64, round int, fs []Finding) error {
	if runID == "" || prID == 0 {
		return fmt.Errorf("record findings: run id and pr id are required")
	}
	for _, f := range fs {
		if f.FindingID == "" {
			return fmt.Errorf("record findings of %s: a finding has no id", runID)
		}
		if f.Verdict != FindingPosted && f.Verdict != FindingRejected {
			return fmt.Errorf("record findings of %s: finding %s: verdict %q is not posted or rejected", runID, f.FindingID, f.Verdict)
		}
	}
	now := s.now()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM findings WHERE run_id = ?", runID); err != nil {
			return err
		}
		for _, f := range fs {
			created := f.CreatedAt
			if created.IsZero() {
				created = now
			}
			sources := f.Sources
			if sources == nil {
				sources = []string{}
			}
			src, err := json.Marshal(sources)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `
INSERT INTO findings (run_id, pr_id, round, finding_id, title, severity, path, line, sources_json, verdict, reason_code, nearby, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				runID, prID, round, f.FindingID, nullString(f.Title), nullString(f.Severity), nullString(f.Path), nullInt(f.Line),
				string(src), f.Verdict, nullString(f.ReasonCode), f.Nearby, FormatTime(created))
			if err != nil {
				return mapErr(err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("record findings of %s: %w", runID, err)
	}
	return nil
}

// FindingsSince returns the findings recorded at or after since, oldest
// first, each with its PR's repository.
func (s *Store) FindingsSince(ctx context.Context, since time.Time) ([]Finding, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+cols("f", findingColumns)+", rp.owner || '/' || rp.name"+
		" FROM findings f JOIN prs p ON p.id = f.pr_id JOIN repos rp ON rp.id = p.repo_id"+
		" WHERE f.created_at >= ? ORDER BY f.created_at, f.id", FormatTime(since))
	if err != nil {
		return nil, fmt.Errorf("findings since %s: %w", FormatTime(since), err)
	}
	out, err := collect(rows, func(sc scanner) (Finding, error) { return scanFinding(sc, true) })
	if err != nil {
		return nil, fmt.Errorf("findings since %s: %w", FormatTime(since), err)
	}
	return out, nil
}

// PreExistingFindings returns the P1 and P2 findings the judge rejected as
// pre_existing, in every PR, newest first (created_at, then id), each with
// its PR's repository and number: what `magnum debt` lists.
func (s *Store) PreExistingFindings(ctx context.Context) ([]Finding, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+cols("f", findingColumns)+", rp.owner || '/' || rp.name, p.number"+
		" FROM findings f JOIN prs p ON p.id = f.pr_id JOIN repos rp ON rp.id = p.repo_id"+
		" WHERE f.verdict = ? AND f.reason_code = 'pre_existing' AND f.severity IN ('P1', 'P2')"+
		" ORDER BY f.created_at DESC, f.id DESC", FindingRejected)
	if err != nil {
		return nil, fmt.Errorf("pre-existing findings: %w", err)
	}
	out, err := collect(rows, func(sc scanner) (Finding, error) {
		var f Finding
		err := scanFindingInto(sc, &f, &f.Repo, &f.Number)
		return f, err
	})
	if err != nil {
		return nil, fmt.Errorf("pre-existing findings: %w", err)
	}
	return out, nil
}

// FindingsByPR returns the findings recorded for prID, oldest first
// (created_at, id); Repo is left empty.
func (s *Store) FindingsByPR(ctx context.Context, prID int64) ([]Finding, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+cols("", findingColumns)+
		" FROM findings WHERE pr_id = ? ORDER BY created_at, id", prID)
	if err != nil {
		return nil, fmt.Errorf("findings of pr %d: %w", prID, err)
	}
	out, err := collect(rows, func(sc scanner) (Finding, error) { return scanFinding(sc, false) })
	if err != nil {
		return nil, fmt.Errorf("findings of pr %d: %w", prID, err)
	}
	return out, nil
}

// scanFinding scans a row of findingColumns, followed by the PR's repository
// when withRepo is set.
func scanFinding(sc scanner, withRepo bool) (Finding, error) {
	var f Finding
	var extra []any
	if withRepo {
		extra = append(extra, &f.Repo)
	}
	err := scanFindingInto(sc, &f, extra...)
	return f, err
}

// scanFindingInto scans a row of findingColumns into f, followed by the
// columns extra points at.
func scanFindingInto(sc scanner, f *Finding, extra ...any) error {
	var title, sev, path, reason sql.NullString
	var line sql.NullInt64
	var src string
	dest := append([]any{&f.ID, &f.RunID, &f.PRID, &f.Round, &f.FindingID, &title, &sev, &path, &line, &src, &f.Verdict, &reason,
		&f.Nearby, timeCol(&f.CreatedAt)}, extra...)
	if err := sc.Scan(dest...); err != nil {
		return err
	}
	f.Title, f.Severity, f.Path, f.ReasonCode, f.Line = title.String, sev.String, path.String, reason.String, int(line.Int64)
	if err := json.Unmarshal([]byte(src), &f.Sources); err != nil {
		return fmt.Errorf("finding %d: sources: %w", f.ID, err)
	}
	return nil
}

// nullString stores "" as NULL.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullInt stores 0 as NULL.
func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}
