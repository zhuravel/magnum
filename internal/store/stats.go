package store

import (
	"context"
	"fmt"
	"time"
)

// Reads for `magnum stats`: the runs of the rounds in a window and the
// events of a few kinds. FindingsSince (findings.go) is the third input.

// RoundRun is a run with its PR's repository (owner/name) and number.
type RoundRun struct {
	Run
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

// RunsOfRoundsSince returns every run of each round (pr_id, round) that has a
// run created at or after since, oldest first, with its PR's repository and
// number. A round that began before since and went on after it comes whole,
// so the caller can tell when it began.
func (s *Store) RunsOfRoundsSince(ctx context.Context, since time.Time) ([]RoundRun, error) {
	q := "SELECT " + cols("r", runColumns) + ", rp.owner || '/' || rp.name, p.number" +
		" FROM runs r JOIN prs p ON p.id = r.pr_id JOIN repos rp ON rp.id = p.repo_id" +
		" WHERE (r.pr_id, r.round) IN (SELECT pr_id, round FROM runs WHERE created_at >= ?)" +
		" ORDER BY r.created_at, r.rowid"
	rows, err := s.db.QueryContext(ctx, q, FormatTime(since))
	if err != nil {
		return nil, fmt.Errorf("runs since %s: %w", FormatTime(since), err)
	}
	out, err := collect(rows, func(sc scanner) (RoundRun, error) {
		var rr RoundRun
		r, err := scanRun(extraScanner{sc, []any{&rr.Repo, &rr.Number}})
		rr.Run = r
		return rr, err
	})
	if err != nil {
		return nil, fmt.Errorf("runs since %s: %w", FormatTime(since), err)
	}
	return out, nil
}

// EventsOfKindsSince returns the events of the given kinds at or after since,
// oldest first; no kinds returns nil.
func (s *Store) EventsOfKindsSince(ctx context.Context, since time.Time, kinds ...string) ([]Event, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	q := "SELECT " + cols("", eventColumns) + " FROM events WHERE kind IN (" + placeholders(len(kinds)) + ") AND at >= ? ORDER BY id"
	args := append(anys(kinds), FormatTime(since))
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("events since %s: %w", FormatTime(since), err)
	}
	evs, err := collect(rows, scanEvent)
	if err != nil {
		return nil, fmt.Errorf("events since %s: %w", FormatTime(since), err)
	}
	return evs, nil
}

// extraScanner scans a row's leading columns with the caller's destinations
// and the trailing ones into extra.
type extraScanner struct {
	sc    scanner
	extra []any
}

func (e extraScanner) Scan(dest ...any) error { return e.sc.Scan(append(dest, e.extra...)...) }
