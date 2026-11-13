package fixwork

// The owner of a PR's fixes (DECISIONS "The fix loop moves into the
// factory", the interlock): magnum's registry holds one row per PR that
// magnum does not fix itself, KVPRFixOwner, and magnum is the arbiter of
// it. No row means magnum fixes the PR. A row says OwnerFactory, the
// factory fixes it, or OwnerReturning, the PR goes back to magnum and
// neither daemon starts a fixer on it until magnum takes the row away.
// Magnum writes the row by compare-and-set, only at a quiet point, and
// reads it locally; the factory reads it across processes with this type,
// read only. A row that does not read is a failed read: either daemon
// refuses a fixer start on it, so no PR gets two fixers.

import (
	"encoding/json"
	"fmt"
	"time"
)

// KVPRFixOwner holds who fixes the PR prID when it is not magnum
// (FixOwner as JSON); no row: magnum.
func KVPRFixOwner(prID int64) string { return fmt.Sprintf("pr.%d.fix_owner", prID) }

// Owners of a PR's fixes (FixOwner.Owner).
const (
	// OwnerFactory: the factory fixes the PR, and magnum starts no fixer
	// on it.
	OwnerFactory = "factory"
	// OwnerReturning: the PR goes back to magnum, which takes the row away
	// once the factory's lease shows no work of the factory's since its
	// import; meanwhile neither daemon starts a fixer on it.
	OwnerReturning = "returning"
)

// FixOwner is a PR's KVPRFixOwner row: Owner, since At (when magnum
// flipped the row to Owner: the factory keys its one import of the PR's
// records by the At of the flip to OwnerFactory), and By, what flipped it
// ("magnum fix --handover", "magnum fix --handover all"). Flipped is, for
// OwnerReturning, the At of the flip to the factory it returns from: a PR
// the factory worked before it saw the return stays the factory's under
// that flip again, so the factory imports it no second time.
type FixOwner struct {
	Owner   string    `json:"owner"`
	At      time.Time `json:"at"`
	By      string    `json:"by,omitempty"`
	Flipped time.Time `json:"flipped,omitzero"`
}

// Factory reports whether the factory fixes the PR (OwnerFactory): the only
// owner a factory's fixer may start on.
func (o FixOwner) Factory() bool { return o.Owner == OwnerFactory }

// Value is the row as this build writes it.
func (o FixOwner) Value() (string, error) {
	if o.Owner != OwnerFactory && o.Owner != OwnerReturning {
		return "", fmt.Errorf("fix owner: no owner %q", o.Owner)
	}
	b, err := json.Marshal(o)
	if err != nil {
		return "", fmt.Errorf("fix owner: %w", err)
	}
	return string(b), nil
}

// ParseFixOwner reads a KVPRFixOwner row. A row that is no JSON, names an
// owner this build does not know or no time is an error: a reader that
// took it for no row would let magnum fix a PR the factory may fix.
func ParseFixOwner(raw string) (FixOwner, error) {
	var o FixOwner
	if err := json.Unmarshal([]byte(raw), &o); err != nil {
		return FixOwner{}, fmt.Errorf("fix owner %q: %w", clipRow(raw), err)
	}
	switch {
	case o.Owner != OwnerFactory && o.Owner != OwnerReturning:
		return FixOwner{}, fmt.Errorf("fix owner %q: no owner %q", clipRow(raw), o.Owner)
	case o.At.IsZero():
		return FixOwner{}, fmt.Errorf("fix owner %q: no time", clipRow(raw))
	}
	return o, nil
}

// clipRow is at most 80 bytes of a row, for an error.
func clipRow(raw string) string {
	if len(raw) > 80 {
		return raw[:80] + "…"
	}
	return raw
}
