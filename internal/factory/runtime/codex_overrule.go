package runtime

// Your overrule of a stale Codex reading. Codex's newest session file can
// still carry limits that no longer hold (a reset, a switch of Codex
// account), and no Codex session runs to refresh it while the caps hold
// Codex. `magnum resume --tool codex` (or `all`) marks the time in magnum's
// registry (engine.KVUsageCodexOverruledAt), and the review daemon's caps
// wait for a reading stamped after it (engine.budgetLevel). The factory's
// waits on Codex's reading, the launch's gate here and the engine's local
// review and stage yields, honor the same mark (CodexOverrule), and each
// names the command (CodexOverruleRemedy).

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/zhuravel/magnum/internal/usage"
)

// CodexOverruleRemedy names the command that overrules a stale Codex
// reading, the words that end each wait of the factory's on that reading.
const CodexOverruleRemedy = "`magnum resume --tool codex` overrules a stale reading"

// CodexOverrule judges a Codex reading by your overrule (Covers). Read is
// magnum's mark, zero when none is set (the review daemon deletes it once
// Codex reports again); nil reads none. Log takes the failure of a read,
// once; nil logs nothing. It is safe for concurrent use, and is used
// through a pointer.
type CodexOverrule struct {
	Read   func(ctx context.Context) (time.Time, error)
	Log    *slog.Logger
	unread atomic.Bool
}

// Covers reports whether snap is a Codex reading you overruled: a mark is
// set and snap is stamped at or before it, so the caps take Codex as under
// them, as the review daemon's do, until a reading stamped after the mark
// comes, which they judge as before. A read of the mark that fails covers
// nothing, so the caps hold as they did, and is logged once until a read
// goes through.
func (o *CodexOverrule) Covers(ctx context.Context, snap usage.Snapshot) bool {
	if o == nil || o.Read == nil {
		return false
	}
	mark, err := o.Read(ctx)
	if err != nil {
		if !o.unread.Swap(true) && o.Log != nil {
			o.Log.Warn("read your overrule of Codex's reading in magnum's registry; Codex's reading holds as before", "err", err)
		}
		return false
	}
	o.unread.Store(false)
	return !mark.IsZero() && !snap.At.After(mark)
}
