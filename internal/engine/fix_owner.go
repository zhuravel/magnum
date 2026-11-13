package engine

// Who fixes a PR (DECISIONS "The fix loop moves into the factory", the
// interlock): magnum's half. The PR's owner row (fixwork.KVPRFixOwner, a
// key-value row: no migration) says the factory fixes it, or that it is
// returning to magnum; no row, magnum fixes it. Magnum is the arbiter of the
// row and the fixer side fails closed: every path that can create a fix row
// (a loop's, a held fix's, a held merge's push, the late settle of an
// unconfirmed push) reads the owner row first and refuses a PR magnum does
// not own, and a row that cannot be read refuses too. The refusal is read
// again under the PR's reservation, right before a loop's row is written
// (claimFixRow), where it serializes with the handover's flip, which takes
// the same reservation.

import (
	"context"
	"errors"
	"fmt"

	"github.com/zhuravel/magnum/internal/fixwork"
)

// errNotMagnums is a fixer start refused because magnum does not fix the
// PR (ownerRefusal): the factory does, the PR is returning to magnum, or
// who fixes it cannot be read.
var errNotMagnums = errors.New("magnum does not fix this PR")

// ownerRefused is ownerRefusal's error: its sentence, errNotMagnums.
type ownerRefused struct{ why string }

func (e ownerRefused) Error() string { return e.why }
func (ownerRefused) Unwrap() error   { return errNotMagnums }

// fixOwner reads PR prID's owner row: ok false for none (magnum fixes it),
// raw the row as stored, for a compare-and-set. A row that does not read is
// an error, never none.
func (e *Engine) fixOwner(ctx context.Context, prID int64) (o fixwork.FixOwner, raw string, ok bool, err error) {
	raw, found, err := e.st.GetKV(ctx, fixwork.KVPRFixOwner(prID))
	if err != nil || !found {
		return fixwork.FixOwner{}, "", false, err
	}
	o, err = fixwork.ParseFixOwner(raw)
	if err != nil {
		return fixwork.FixOwner{}, raw, false, err
	}
	return o, raw, true, nil
}

// ownerRefusal is the refusal of a fixer start on PR prID (label, its
// owner/name#N) while magnum does not own its fixes, nil while it does (no
// owner row): the factory fixes it, it is returning to magnum, or its row
// cannot be read, which refuses as well (errNotMagnums).
func (e *Engine) ownerRefusal(ctx context.Context, label string, prID int64) error {
	o, _, ok, err := e.fixOwner(ctx, prID)
	switch {
	case err != nil:
		return ownerRefused{fmt.Sprintf("%s: who fixes this PR cannot be read (%v); magnum starts no fixer on it", label, err)}
	case !ok:
		return nil
	case o.Factory():
		return ownerRefused{fmt.Sprintf("%s: the factory fixes this PR since %s; magnum starts no fixer on it", label, waitClock(o.At, e.now()))}
	}
	return ownerRefused{fmt.Sprintf("%s is returning from the factory to magnum (since %s); no fixer starts on it until magnum takes it back",
		label, waitClock(o.At, e.now()))}
}
