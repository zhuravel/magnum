package engine

// The fix loop's rows. The fix job (a fixer agent that fixes magnum's own
// findings on the operator's PR, then a review round on what it pushed) keeps
// where it is in one registry row per PR, KVPRFix, so it survives a restart
// and `magnum status` and the board can show it; a PR with no row has no loop.
// KVPRFixOff is the PR's opt-out, `magnum fix --off`: a row of its own, so a
// PR can be switched off while a loop runs and clearing the loop leaves it.
// Both are the engine's own keys, read and written here as snooze.go does for
// the snooze, except that a row that cannot be read is an error, not "none":
// a caller that took it for no loop would start a second one over a running
// loop.

import (
	"context"
	"encoding/json"
	"fmt"
)

// KVPRFix holds a PR's fix loop (FixState as JSON).
func KVPRFix(prID int64) string { return fmt.Sprintf("pr.%d.fix", prID) }

// KVPRFixOff holds a PR's opt-out of the fix job (FixOff as JSON).
func KVPRFixOff(prID int64) string { return fmt.Sprintf("pr.%d.fix_off", prID) }

// fixState reads the PR's fix loop; ok is false when it has none.
func (e *Engine) fixState(ctx context.Context, prID int64) (FixState, bool, error) {
	st, _, ok, err := e.fixRow(ctx, prID)
	return st, ok, err
}

// fixRow is fixState with the row as stored (raw, also for a row that
// cannot be read), for a compare-and-set of the loop (Store.SetKVIf,
// Store.DeleteKVIf): a writer without the PR's reservation, such as `magnum
// abort` or the hook after a round, acts only on the row it read.
func (e *Engine) fixRow(ctx context.Context, prID int64) (st FixState, raw string, ok bool, err error) {
	key := KVPRFix(prID)
	raw, found, err := e.st.GetKV(ctx, key)
	if err != nil || !found {
		return st, "", false, err
	}
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return FixState{}, raw, false, fmt.Errorf("fix state %s: %w", key, err)
	}
	return st, raw, true, nil
}

// setFixState records the PR's fix loop, replacing the one it had.
func (e *Engine) setFixState(ctx context.Context, prID int64, s FixState) error {
	return writeKVJSON(ctx, e, KVPRFix(prID), "fix state", s)
}

// clearFixState ends the PR's fix loop (no error when it has none).
func (e *Engine) clearFixState(ctx context.Context, prID int64) error {
	return e.st.DeleteKV(ctx, KVPRFix(prID))
}

// fixOff reads the PR's opt-out of the fix job; ok is false when it has none.
func (e *Engine) fixOff(ctx context.Context, prID int64) (FixOff, bool, error) {
	return readKVJSON[FixOff](ctx, e, KVPRFixOff(prID), "fix opt-out")
}

// setFixOff switches the fix job off for the PR.
func (e *Engine) setFixOff(ctx context.Context, prID int64, o FixOff) error {
	return writeKVJSON(ctx, e, KVPRFixOff(prID), "fix opt-out", o)
}

// clearFixOff switches the fix job back on for the PR (no error when it was on).
func (e *Engine) clearFixOff(ctx context.Context, prID int64) error {
	return e.st.DeleteKV(ctx, KVPRFixOff(prID))
}

// readKVJSON reads key as JSON into a T: ok is false for a key that is not
// there, and a failed read or a value that is no T is an error naming what.
func readKVJSON[T any](ctx context.Context, e *Engine, key, what string) (v T, ok bool, err error) {
	raw, found, err := e.st.GetKV(ctx, key)
	if err != nil || !found {
		return v, false, err
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		var zero T
		return zero, false, fmt.Errorf("%s %s: %w", what, key, err)
	}
	return v, true, nil
}

// writeKVJSON stores v under key as JSON.
func writeKVJSON(ctx context.Context, e *Engine, key, what string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("%s %s: %w", what, key, err)
	}
	return e.st.SetKV(ctx, key, string(b))
}
