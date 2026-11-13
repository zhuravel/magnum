package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/fixwork"
	"github.com/zhuravel/magnum/internal/store"
)

// The phases of a PR's fix work (PRWork.Phase, migrations/0009_pr_work.sql).
// Only PRWorkFixing, PRWorkLocalReview and PRWorkPushing hold an agent or a
// push (PRWorkBusy).
const (
	// PRWorkIdle: no work on the PR.
	PRWorkIdle = "idle"
	// PRWorkQueued: the fix waits for [factory] fix_max_active or a copy.
	PRWorkQueued = "queued"
	// PRWorkFixing: the fixer's turn on the head the cycle fixes.
	PRWorkFixing = "fixing"
	// PRWorkLocalReview: a local review round of the fix cycle.
	PRWorkLocalReview = "local_review"
	// PRWorkPushing: the push and its confirmation on GitHub, the late
	// settle included.
	PRWorkPushing = "pushing"
	// PRWorkReviewing: the fix pushed, and it waits for magnum's re-review
	// of the pushed head.
	PRWorkReviewing = "reviewing"
	// PRWorkHeld: it waits for you (a held merge, a needs-you entry, the
	// cap).
	PRWorkHeld = "held"
)

var prWorkPhases = map[string]bool{PRWorkIdle: true, PRWorkQueued: true, PRWorkFixing: true, PRWorkLocalReview: true, PRWorkPushing: true,
	PRWorkReviewing: true, PRWorkHeld: true}

// PRWorkBusy reports whether phase holds an agent or a push (fixing,
// local_review or pushing): the work a restart of the factory daemon would
// land in the middle of, which `magnum factory install` waits for, and the
// phases in which the factory fixes the PR under its lease. Queued,
// reviewing and held hold neither, and idle is no work.
func PRWorkBusy(phase string) bool {
	return phase == PRWorkFixing || phase == PRWorkLocalReview || phase == PRWorkPushing
}

// PRWork is the factory's record of the fix work on one of your PRs
// (pr_work): its repository (owner/name) and number, its task (0 for a PR
// no factory task published, most of them), your opt-out of its fixes
// (FixOff; nil while it fixes the PR), where its fix loop is (Phase since
// PhaseAt, at Head, cycle Cycle of at most Cap), the factory's lease on it
// while it works (LeaseHolder until LeaseUntil; both empty for none), the
// heads it pushed (Pushed, oldest first), its CI records (CIFix,
// CIDispatch, CIRerun: zero for none) and the open ledger (Ledger), all in
// internal/fixwork's types, so the records magnum imports keep their
// format; and when magnum handed the PR's fixes to the factory (FlippedAt),
// when the factory read magnum's records of it (ImportedAt) and when it
// first worked the PR since (WorkedAt; zero: never, and the PR may go back
// to magnum).
type PRWork struct {
	ID                    int64
	Repo                  string
	Number                int
	TaskID                int64
	FixOff                *fixwork.FixOff
	Phase                 string
	PhaseAt               time.Time
	Head                  string
	Cycle, Cap            int
	LeaseHolder           string
	LeaseUntil            time.Time
	Pushed                []fixwork.CIPush
	CIFix                 fixwork.CIFixRecord
	CIDispatch            fixwork.CIStart
	CIRerun               fixwork.CIRerun
	Ledger                fixwork.Ledger
	FlippedAt, ImportedAt time.Time
	WorkedAt              time.Time
	Created, Updated      time.Time
}

// Leased reports whether the factory's lease on the PR runs at now: it has
// a holder and its end is after now. A lease that ran out (a daemon that
// stopped while it worked) holds nothing.
func (w PRWork) Leased(now time.Time) bool { return w.LeaseHolder != "" && w.LeaseUntil.After(now) }

const prWorkColumns = "id, repo, number, task_id, fix_off, phase, phase_at, head, cycle, cap, lease_holder, lease_until, pushed_json, " +
	"ci_fix_json, ci_dispatch_json, ci_rerun_json, ledger_json, flipped_at, imported_at, worked_at, created_at, updated_at"

func scanPRWork(sc scanner) (PRWork, error) {
	var w PRWork
	var task sql.NullInt64
	var off, pushed, ciFix, ciDispatch, ciRerun, ledger string
	err := sc.Scan(&w.ID, &w.Repo, &w.Number, &task, &off, &w.Phase, timeCol{&w.PhaseAt}, &w.Head, &w.Cycle, &w.Cap, &w.LeaseHolder,
		timeCol{&w.LeaseUntil}, &pushed, &ciFix, &ciDispatch, &ciRerun, &ledger, timeCol{&w.FlippedAt}, timeCol{&w.ImportedAt},
		timeCol{&w.WorkedAt}, timeCol{&w.Created}, timeCol{&w.Updated})
	if err != nil {
		return PRWork{}, err
	}
	w.TaskID = task.Int64
	if off != "" {
		w.FixOff = &fixwork.FixOff{}
		if err := json.Unmarshal([]byte(off), w.FixOff); err != nil {
			return PRWork{}, fmt.Errorf("pr work %d: fix_off: %w", w.ID, err)
		}
	}
	for _, c := range []struct {
		col, raw string
		v        any
	}{{"pushed_json", pushed, &w.Pushed}, {"ci_fix_json", ciFix, &w.CIFix}, {"ci_dispatch_json", ciDispatch, &w.CIDispatch},
		{"ci_rerun_json", ciRerun, &w.CIRerun}, {"ledger_json", ledger, &w.Ledger}} {
		if err := json.Unmarshal([]byte(c.raw), c.v); err != nil {
			return PRWork{}, fmt.Errorf("pr work %d: %s: %w", w.ID, c.col, err)
		}
	}
	return w, nil
}

// prRepo is repo as pr_work keys it: owner/name, trimmed (the column
// compares case aside).
func prRepo(repo string, number int) (string, error) {
	repo = strings.TrimSpace(repo)
	if !strings.Contains(repo, "/") || number <= 0 {
		return "", fmt.Errorf("pr work: %q#%d names no pull request", repo, number)
	}
	return repo, nil
}

// EnsurePRWork returns the PR work row of PR number of repo (owner/name,
// any case), first inserting it idle, with no task, when there is none.
func (r *Registry) EnsurePRWork(ctx context.Context, repo string, number int) (PRWork, error) {
	repo, err := prRepo(repo, number)
	if err != nil {
		return PRWork{}, err
	}
	var out PRWork
	err = r.tx(ctx, func(tx *sql.Tx) error {
		stamp := store.FormatTime(r.now())
		if _, err := tx.ExecContext(ctx, "INSERT INTO pr_work (repo, number, created_at, updated_at) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING",
			repo, number, stamp, stamp); err != nil {
			return mapErr(err)
		}
		out, err = scanPRWork(tx.QueryRowContext(ctx, "SELECT "+prWorkColumns+" FROM pr_work WHERE repo = ? AND number = ?", repo, number))
		return err
	})
	if err != nil {
		return PRWork{}, fmt.Errorf("pr work of %s#%d: %w", repo, number, err)
	}
	return out, nil
}

// PRWorkOf is the PR work row of PR number of repo (any case); ErrNotFound
// when the factory has none.
func (r *Registry) PRWorkOf(ctx context.Context, repo string, number int) (PRWork, error) {
	w, err := scanPRWork(r.db.QueryRowContext(ctx, "SELECT "+prWorkColumns+" FROM pr_work WHERE repo = ? AND number = ?",
		strings.TrimSpace(repo), number))
	if err != nil {
		return PRWork{}, notFound(err, "pr work of", fmt.Sprintf("%s#%d", repo, number))
	}
	return w, nil
}

// PRWorkByID is PR work row id; ErrNotFound when there is none.
func (r *Registry) PRWorkByID(ctx context.Context, id int64) (PRWork, error) {
	w, err := scanPRWork(r.db.QueryRowContext(ctx, "SELECT "+prWorkColumns+" FROM pr_work WHERE id = ?", id))
	if err != nil {
		return PRWork{}, notFound(err, "pr work", id)
	}
	return w, nil
}

// BusyPRWork are the PR work rows whose phase holds an agent or a push
// (PRWorkBusy: fixing, local_review or pushing), by id: the fix loops at
// work, which `magnum factory install --drain` waits for.
func (r *Registry) BusyPRWork(ctx context.Context) ([]PRWork, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT "+prWorkColumns+" FROM pr_work WHERE phase IN (?, ?, ?) ORDER BY id",
		PRWorkFixing, PRWorkLocalReview, PRWorkPushing)
	if err != nil {
		return nil, fmt.Errorf("busy pr work: %w", err)
	}
	out, err := collect(rows, scanPRWork)
	if err != nil {
		return nil, fmt.Errorf("busy pr work: %w", err)
	}
	return out, nil
}

// PRWorkUpdate collects the changes of UpdatePRWork. Settable columns
// (Set): head, cycle, cap, flipped_at and imported_at; the task, your
// opt-out and the JSON records through their own setters, worked_at
// through Worked. The phase moves through UpdatePRWork's `to` (its
// phase_at with it), the lease through TakePRWorkLease and
// ReleasePRWorkLease, and updated_at is the registry's.
type PRWorkUpdate struct {
	update
	worked bool
}

var prWorkSettable = map[string]bool{"head": true, "cycle": true, "cap": true, "flipped_at": true, "imported_at": true}

// Task sets the PR's task (0: none).
func (u *PRWorkUpdate) Task(id int64) {
	if id <= 0 {
		u.raw("task_id = NULL")
		return
	}
	u.raw("task_id = ?", id)
}

// FixOff sets your opt-out of the factory's fixes on the PR (nil: none).
func (u *PRWorkUpdate) FixOff(o *fixwork.FixOff) {
	if o == nil {
		u.raw("fix_off = ''")
		return
	}
	u.setJSON("fix_off", o)
}

// Pushed sets the heads the factory pushed to the PR.
func (u *PRWorkUpdate) Pushed(p []fixwork.CIPush) {
	if p == nil {
		p = []fixwork.CIPush{}
	}
	u.setJSON("pushed_json", p)
}

// CIFix sets the PR's CI fixes (fixwork.CIFixRecord).
func (u *PRWorkUpdate) CIFix(rec fixwork.CIFixRecord) { u.setJSON("ci_fix_json", rec) }

// CIDispatch sets the look's start of the PR's CI workflow (zero: none).
func (u *PRWorkUpdate) CIDispatch(s fixwork.CIStart) { u.setJSON("ci_dispatch_json", s) }

// CIRerun sets the look's re-run of the PR's runs (zero: none).
func (u *PRWorkUpdate) CIRerun(rr fixwork.CIRerun) { u.setJSON("ci_rerun_json", rr) }

// Ledger sets the PR's open ledger.
func (u *PRWorkUpdate) Ledger(l fixwork.Ledger) { u.setJSON("ledger_json", l) }

// Worked notes that the factory worked the PR (a start, a push, a reply, a
// ledger change): worked_at keeps the first time after the import.
func (u *PRWorkUpdate) Worked() { u.worked = true }

// setJSON assigns v, as JSON, to the column col, once.
func (u *PRWorkUpdate) setJSON(col string, v any) {
	if u.touched[col] {
		u.fail(fmt.Errorf("column %q set twice", col))
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		u.fail(fmt.Errorf("column %q: %w", col, err))
		return
	}
	if u.touched == nil {
		u.touched = map[string]bool{}
	}
	u.touched[col] = true
	u.raw(col+" = ?", string(b))
}

// UpdatePRWork changes PR work row id: its phase moves to `to` (with
// phase_at; "" keeps the phase) only when it is one of from (nil: any), and
// set's changes apply in the same UPDATE. A row whose phase is not one of
// from is ErrConflict, a missing one ErrNotFound. Any phase but idle needs
// a cycle of 1 or more (the table's check).
func (r *Registry) UpdatePRWork(ctx context.Context, id int64, from []string, to string, set func(*PRWorkUpdate)) error {
	if to != "" && !prWorkPhases[to] {
		return fmt.Errorf("pr work %d: unknown phase %q", id, to)
	}
	u := &PRWorkUpdate{update: update{settable: prWorkSettable}}
	if set != nil {
		set(u)
	}
	if u.err != nil {
		return fmt.Errorf("update pr work %d: %w", id, u.err)
	}
	now := store.FormatTime(r.now())
	sets, args := u.sets, u.args
	if to != "" {
		sets = append(sets, "phase = ?", "phase_at = CASE WHEN phase = ? THEN phase_at ELSE ? END")
		args = append(args, to, to, now)
	}
	if u.worked {
		sets = append(sets, "worked_at = COALESCE(worked_at, ?)")
		args = append(args, now)
	}
	if len(sets) == 0 {
		return fmt.Errorf("update pr work %d: nothing to set", id)
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, now, id)
	q := "UPDATE pr_work SET " + strings.Join(sets, ", ") + " WHERE id = ?"
	if from != nil {
		q += " AND phase IN (" + placeholders(len(from)) + ")"
		for _, p := range from {
			args = append(args, p)
		}
	}
	return r.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("update pr work %d: %w", id, mapErr(err))
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
		var phase string
		if err := tx.QueryRowContext(ctx, "SELECT phase FROM pr_work WHERE id = ?", id).Scan(&phase); err != nil {
			return notFound(err, "pr work", id)
		}
		return fmt.Errorf("update pr work %d -> %s: %w: phase is %s, want one of %v", id, to, ErrConflict, phase, from)
	})
}

// ErrLeaseHeld is TakePRWorkLease's error when another holder's lease on
// the PR runs.
var ErrLeaseHeld = errors.New("factory registry: another holder's lease runs")

// TakePRWorkLease gives holder the lease on PR work row id until until: when
// the row has no lease, its lease ran out, or holder holds it already (a
// renewal). Another holder's lease that runs is ErrLeaseHeld (wrapping
// ErrConflict), a missing row ErrNotFound.
func (r *Registry) TakePRWorkLease(ctx context.Context, id int64, holder string, until time.Time) error {
	if holder == "" || until.IsZero() {
		return fmt.Errorf("pr work %d: a lease needs a holder and an end", id)
	}
	now := store.FormatTime(r.now())
	return r.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE pr_work SET lease_holder = ?, lease_until = ?, updated_at = ?
			WHERE id = ? AND (lease_holder = '' OR lease_holder = ? OR lease_until <= ?)`,
			holder, store.FormatTime(until), now, id, holder, now)
		if err != nil {
			return fmt.Errorf("lease pr work %d: %w", id, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
		var other string
		if err := tx.QueryRowContext(ctx, "SELECT lease_holder FROM pr_work WHERE id = ?", id).Scan(&other); err != nil {
			return notFound(err, "pr work", id)
		}
		return fmt.Errorf("lease pr work %d: %w (%s holds it): %w", id, ErrLeaseHeld, other, ErrConflict)
	})
}

// ReleasePRWorkLease ends holder's lease on PR work row id; a lease of
// another holder, or none, is left as it is.
func (r *Registry) ReleasePRWorkLease(ctx context.Context, id int64, holder string) error {
	_, err := r.db.ExecContext(ctx, "UPDATE pr_work SET lease_holder = '', lease_until = NULL, updated_at = ? WHERE id = ? AND lease_holder = ?",
		store.FormatTime(r.now()), id, holder)
	if err != nil {
		return fmt.Errorf("release the lease of pr work %d: %w", id, err)
	}
	return nil
}
