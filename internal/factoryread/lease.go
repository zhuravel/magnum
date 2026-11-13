package factoryread

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/store"
)

// Lease is the factory's hold on the fixes of one of your PRs, as magnum
// reads it from the factory's PR work row (pr_work, registry.PRWork) to
// hold its rounds while the factory's fixer works (DECISIONS "The factory
// becomes the only fixer"): the row (ID; 0 when the factory has none),
// whether the factory fixes the PR now (Fixing: its fix loop is in a phase
// that holds an agent or a push, fixing, local_review or pushing
// (registry.PRWorkBusy), and its lease runs at the read; a lease that ran
// out, a factory that stopped while it worked, holds nothing, and a loop
// that is queued, waits for magnum's re-review or waits for you fixes
// nothing now), the loop's phase, the head its cycle fixes (from pushing on,
// the head it pushed), its cycle of at
// most Cap, who holds the lease and until when, when magnum handed the
// PR's fixes to the factory and when the factory read magnum's records of
// it, and whether the factory worked the PR since (Worked, at WorkedAt): a
// PR it has not worked may go back to magnum.
type Lease struct {
	ID                    int64
	Fixing                bool
	Phase, Head           string
	Cycle, Cap            int
	Holder                string
	Until                 time.Time
	FlippedAt, ImportedAt time.Time
	Worked                bool
	WorkedAt              time.Time
}

// LeaseOf reads the lease of PR number of repo ("owner/name", any case) in
// the factory's registry at path, read only and in one snapshot, at the
// time of the call. A PR the factory has no row of, a registry without the
// PR work table (a factory older than it) and a registry that does not
// exist (no factory ever ran here) give the zero Lease and no error: the
// factory fixes none of them. A registry that cannot be read is an error,
// which the caller decides about (magnum's review side holds its round for
// a grace, then reviews).
func LeaseOf(ctx context.Context, path, repo string, number int) (Lease, error) {
	db, ok, err := openPRWork(ctx, path)
	if err != nil || !ok {
		return Lease{}, err
	}
	defer db.Close()
	var l Lease
	var until, flipped, imported, worked sql.NullString
	err = db.QueryRowContext(ctx, `SELECT id, phase, head, cycle, cap, lease_holder, lease_until, flipped_at, imported_at, worked_at
		FROM pr_work WHERE repo = ? COLLATE NOCASE AND number = ?`, repo, number).
		Scan(&l.ID, &l.Phase, &l.Head, &l.Cycle, &l.Cap, &l.Holder, &until, &flipped, &imported, &worked)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Lease{}, nil
	case err != nil:
		return Lease{}, fmt.Errorf("read the factory's lease of %s#%d: %w", repo, number, err)
	}
	for _, c := range []struct {
		raw sql.NullString
		t   *time.Time
	}{{until, &l.Until}, {flipped, &l.FlippedAt}, {imported, &l.ImportedAt}, {worked, &l.WorkedAt}} {
		if !c.raw.Valid || c.raw.String == "" {
			continue
		}
		if *c.t, err = store.ParseTime(c.raw.String); err != nil {
			return Lease{}, fmt.Errorf("read the factory's lease of %s#%d: %w", repo, number, err)
		}
	}
	l.Worked = !l.WorkedAt.IsZero()
	l.Fixing = registry.PRWorkBusy(l.Phase) && l.Holder != "" && l.Until.After(time.Now())
	return l, nil
}

// FixJob is a fix loop the factory runs on one of your PRs, as its PR
// work row says (a phase that holds an agent or a push: fixing,
// local_review or pushing, registry.PRWorkBusy): the PR (owner/name and number),
// the phase, the cycle and since when the loop is in its phase (zero:
// unknown).
type FixJob struct {
	Repo   string
	Number int
	Phase  string
	Cycle  int
	Since  time.Time
}

// FixJobs are the fix loops at work in the factory's registry at path
// (registry.BusyPRWork, read only): what `magnum factory install --drain`
// waits for besides the work its daemon records. A loop that is queued,
// waits for magnum's re-review or waits for you holds no agent and no push,
// and is none of them. A registry that does not
// exist, or one without the PR work table (a factory older than it), has
// none; one that cannot be read is an error.
func FixJobs(ctx context.Context, path string) ([]FixJob, error) {
	db, ok, err := openPRWork(ctx, path)
	if err != nil || !ok {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "SELECT repo, number, phase, cycle, phase_at FROM pr_work WHERE phase IN (?, ?, ?) ORDER BY id",
		registry.PRWorkFixing, registry.PRWorkLocalReview, registry.PRWorkPushing)
	if err != nil {
		return nil, fmt.Errorf("read the factory's fix loops: %w", err)
	}
	defer rows.Close()
	var out []FixJob
	for rows.Next() {
		var j FixJob
		var since sql.NullString
		if err := rows.Scan(&j.Repo, &j.Number, &j.Phase, &j.Cycle, &since); err != nil {
			return nil, fmt.Errorf("read the factory's fix loops: %w", err)
		}
		if since.Valid && since.String != "" {
			if j.Since, err = store.ParseTime(since.String); err != nil {
				return nil, fmt.Errorf("read the factory's fix loops: %w", err)
			}
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the factory's fix loops: %w", err)
	}
	return out, nil
}

// openPRWork opens the factory's registry at path read only for a read of
// its PR work table; ok is false, with no error and nothing open, when the
// registry does not exist or has no such table (a factory older than it).
func openPRWork(ctx context.Context, path string) (*sql.DB, bool, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, fmt.Errorf("the factory's registry: %w", err)
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, false, fmt.Errorf("open the factory's registry: %w", err)
	}
	db.SetMaxOpenConns(1)
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'pr_work'").Scan(&n); err != nil {
		db.Close()
		return nil, false, fmt.Errorf("read the factory's registry: %w", err)
	}
	if n == 0 {
		db.Close()
		return nil, false, nil
	}
	return db, true, nil
}
