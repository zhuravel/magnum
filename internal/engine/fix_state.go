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
	"time"
)

// Phases of a fix loop (FixState.Phase).
const (
	// FixPhaseFixing: the fixer works on the head the cycle fixes.
	FixPhaseFixing = "fixing"
	// FixPhaseReviewing: the fixer pushed, and a review round runs on the new head.
	FixPhaseReviewing = "reviewing"
)

// KVPRFix holds a PR's fix loop (FixState as JSON).
func KVPRFix(prID int64) string { return fmt.Sprintf("pr.%d.fix", prID) }

// KVPRFixOff holds a PR's opt-out of the fix job (FixOff as JSON).
func KVPRFixOff(prID int64) string { return fmt.Sprintf("pr.%d.fix_off", prID) }

// FixState is where a PR's fix loop is: cycle Cycle of at most Cap, in Phase.
// Since is the head the loop started from and Head the head the current cycle
// fixes (in phase reviewing, the head the fixer pushed). By says who started
// it ("magnum fix", "autofix"), Role is the fix role whose fixer runs the
// current cycle, Report is the path of the report the fixer writes, and
// StartedAt is when the loop started. NeedsYou are the findings a fixer that
// pushed left to the author (its report's "## Needs you" entries, in its own
// words, so never put in an event or a log; FixCalls reads what each asks):
// the push is re-reviewed first, and the loop ends after that round as
// needs attention, unless magnum's threads show every call settled by then
// (freshNeedsYou), when the round decides the loop as for a push that left
// nothing to the author. Answered are the comment ids of the threads of
// magnum's among them that showed their answer at the settle, whose calls
// ask only for the resolution; fixReReview writes it with NeedsYou, and an
// id whose entry is gone asks nothing. ForcedAt is when
// the loop forced the re-review of its push (phase reviewing): its
// re-review is the forced round dispatched at or after it, and every other
// round that ends while the loop waits is ignored (fixAfterRound; zero in a
// row written before it was recorded, where any forced round is). Held says
// that re-review ended and called for a next cycle a drain for a restart
// refused: no round that ends afterwards is the loop's re-review, and the
// restart's recovery, or the drain's end without one, starts that cycle
// (recoverReviewingLoop). Context is what the current cycle did to the
// fixer's conversation it resumed before its prompt, in a few words
// (fixCompact: "compacted 82% → 31%", "compacted from 82%" when the pane
// shows no figure after it, "fresh session (82% after /compact)", "fresh
// session (/compact failed)"); "" when it resumed it as it was or started a
// fresh fixer anyway. OthersUnread says that the current cycle's fixer did
// not get all the other reviewers' items (writeExternal): "not read (HTTP
// 502)", "not all read (over 1000 review threads)", a short cause and never
// PR text; the loop's end says so (othersUnread). "" when it got them.
type FixState struct {
	Cycle        int       `json:"cycle"`
	Cap          int       `json:"cap"`
	Phase        string    `json:"phase"`
	Since        string    `json:"since"`
	Head         string    `json:"head"`
	By           string    `json:"by"`
	Report       string    `json:"report"`
	StartedAt    time.Time `json:"started_at"`
	NeedsYou     []string  `json:"needs_you,omitempty"`
	Answered     []string  `json:"answered,omitempty"`
	Role         string    `json:"role,omitempty"`
	ForcedAt     time.Time `json:"forced_at,omitzero"`
	Held         bool      `json:"held,omitempty"`
	Context      string    `json:"context,omitempty"`
	OthersUnread string    `json:"others_unread,omitempty"`
}

// answered is Answered as a set (nil for none), as needsYouCalls reads it.
func (s FixState) answered() map[string]bool {
	if len(s.Answered) == 0 {
		return nil
	}
	set := make(map[string]bool, len(s.Answered))
	for _, id := range s.Answered {
		set[id] = true
	}
	return set
}

// FixOff is a PR's opt-out of the fix job: when it was set and by whom
// ("magnum fix --off").
type FixOff struct {
	Since time.Time `json:"since"`
	By    string    `json:"by"`
}

// ParseFixState reads a KVPRFix value; ok is false for one that is not a
// fix loop.
func ParseFixState(s string) (FixState, bool) {
	var st FixState
	if s == "" || json.Unmarshal([]byte(s), &st) != nil || st.Cycle < 1 || st.Phase == "" {
		return FixState{}, false
	}
	return st, true
}

// Short is the loop while its fixer works, as the board's state cell shows
// it: "fixing 1/3 · claude-fix" ("fixing 1/3" when the role is not known).
func (s FixState) Short() string {
	short := fmt.Sprintf("fixing %d/%d", s.Cycle, s.Cap)
	if s.Role == "" {
		return short
	}
	return short + " · " + s.Role
}

// ParseFixOff reads a KVPRFixOff value; ok is false for one that is not an
// opt-out.
func ParseFixOff(s string) (FixOff, bool) {
	var o FixOff
	if s == "" || json.Unmarshal([]byte(s), &o) != nil {
		return FixOff{}, false
	}
	return o, true
}

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
