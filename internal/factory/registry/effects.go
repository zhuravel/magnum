package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// Effect states: begin while the effect may be half done, then ok or fail.
const (
	EffectBegin = "begin"
	EffectOK    = "ok"
	EffectFail  = "fail"
)

// Effect is one external side effect (a push, a PR, a comment, a merge) under
// an idempotency key. Result is what an ok effect made: a PR url, a commit
// sha, the reason a failed one failed.
type Effect struct {
	Key, Kind string
	TaskID    int64
	State     string
	Result    string
	// Begun is when the effect was first begun, whatever ran it again later;
	// Ended is the zero time while the effect is begun.
	Begun, Ended time.Time
	// Resumed says the effect was begun again after a crash or a failure.
	Resumed bool
}

const effectColumns = "key, kind, task_id, state, result, begun_at, ended_at, resumed"

func scanEffect(sc scanner) (Effect, error) {
	var e Effect
	err := sc.Scan(&e.Key, &e.Kind, &e.TaskID, &e.State, &e.Result, timeCol{&e.Begun}, timeCol{&e.Ended}, &e.Resumed)
	return e, err
}

// BeginEffect records that the effect under key is about to run, and reports
// whether the caller should run it. The bool is false when the key already has
// an ok result (the returned Effect holds it): the effect is never repeated.
// A key that was begun and never ended (a crash) or that failed runs again, so
// the caller first looks for what the effect may already have made; the
// effect is then Resumed, keeps the time it first began, and the reason of a
// failed run goes into an info event of kind effect. A key belongs to one kind
// and one task: reusing it for another is ErrConflict.
func (r *Registry) BeginEffect(ctx context.Context, key, kind string, taskID int64) (Effect, bool, error) {
	if key == "" || kind == "" {
		return Effect{}, false, errors.New("begin effect: key and kind are required")
	}
	var (
		out Effect
		run bool
	)
	err := r.tx(ctx, func(tx *sql.Tx) error {
		now := store.FormatTime(r.now())
		cur, err := scanEffect(tx.QueryRowContext(ctx, "SELECT "+effectColumns+" FROM effects WHERE key = ?", key))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, "INSERT INTO effects (key, kind, task_id, state, begun_at) VALUES (?, ?, ?, ?, ?)", key, kind, taskID, EffectBegin, now); err != nil {
				return fmt.Errorf("begin effect %s: %w", key, mapErr(err))
			}
		case err != nil:
			return fmt.Errorf("begin effect %s: %w", key, err)
		case cur.Kind != kind || cur.TaskID != taskID:
			return fmt.Errorf("begin effect %s: %w: the key belongs to a %s effect of task %d", key, ErrConflict, cur.Kind, cur.TaskID)
		case cur.State == EffectOK:
			out = cur
			return nil
		default:
			if cur.State == EffectFail {
				_, err := tx.ExecContext(ctx, "INSERT INTO events (at, task_id, kind, phase, text) VALUES (?, ?, ?, ?, ?)",
					now, taskID, "effect", PhaseInfo, fmt.Sprintf("%s runs again after it failed: %s", key, cur.Result))
				if err != nil {
					return fmt.Errorf("begin effect %s: %w", key, mapErr(err))
				}
			}
			if _, err := tx.ExecContext(ctx, "UPDATE effects SET state = ?, result = '', ended_at = NULL, resumed = 1 WHERE key = ?", EffectBegin, key); err != nil {
				return fmt.Errorf("begin effect %s: %w", key, err)
			}
		}
		run = true
		out, err = scanEffect(tx.QueryRowContext(ctx, "SELECT "+effectColumns+" FROM effects WHERE key = ?", key))
		return err
	})
	if err != nil {
		return Effect{}, false, err
	}
	return out, run, nil
}

// EndEffect settles a begun effect as ok or fail with its result. Ending an
// effect that is not begun is ErrConflict, and an unknown key ErrNotFound.
func (r *Registry) EndEffect(ctx context.Context, key, state, result string) error {
	if state != EffectOK && state != EffectFail {
		return fmt.Errorf("end effect %s: state must be %s or %s, got %q", key, EffectOK, EffectFail, state)
	}
	return r.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE effects SET state = ?, result = ?, ended_at = ? WHERE key = ? AND state = ?",
			state, result, store.FormatTime(r.now()), key, EffectBegin)
		if err != nil {
			return fmt.Errorf("end effect %s: %w", key, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
		var current string
		if err := tx.QueryRowContext(ctx, "SELECT state FROM effects WHERE key = ?", key).Scan(&current); err != nil {
			return notFound(err, "effect", key)
		}
		return fmt.Errorf("end effect %s: %w: state is %s, not %s", key, ErrConflict, current, EffectBegin)
	})
}

// EffectByKey looks up an effect; a missing one is ErrNotFound.
func (r *Registry) EffectByKey(ctx context.Context, key string) (Effect, error) {
	e, err := scanEffect(r.db.QueryRowContext(ctx, "SELECT "+effectColumns+" FROM effects WHERE key = ?", key))
	if err != nil {
		return Effect{}, notFound(err, "effect", key)
	}
	return e, nil
}

// OpenEffects returns the effects still begun, oldest first: the ones a
// restart must settle by running each one's guard again.
func (r *Registry) OpenEffects(ctx context.Context) ([]Effect, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT "+effectColumns+" FROM effects WHERE state = ? ORDER BY begun_at, key", EffectBegin)
	if err != nil {
		return nil, fmt.Errorf("open effects: %w", err)
	}
	es, err := collect(rows, scanEffect)
	if err != nil {
		return nil, fmt.Errorf("open effects: %w", err)
	}
	return es, nil
}

// EffectsOfKind returns the effects of kind, oldest first.
func (r *Registry) EffectsOfKind(ctx context.Context, kind string) ([]Effect, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT "+effectColumns+" FROM effects WHERE kind = ? ORDER BY begun_at, key", kind)
	if err != nil {
		return nil, fmt.Errorf("effects of kind %s: %w", kind, err)
	}
	es, err := collect(rows, scanEffect)
	if err != nil {
		return nil, fmt.Errorf("effects of kind %s: %w", kind, err)
	}
	return es, nil
}
