package engine

// Whiteboards a drain skipped. A drain for a restart (`magnum
// daemon-restart --drain`) waits for the rounds in flight, so their reviews
// post inside it, and the whiteboard job never starts under a drain
// (prepareWhiteboard). That skip was final, the next posted review the next
// chance, so a landing day left a quarter of the reviews without a
// whiteboard. Now the posted review's skip remembers the PR and the
// reviewed head (kvPRWhiteboardPending, rememberWhiteboard), and the
// drain's end makes it: the start the drain waited for (startup) and a
// drain lifted without a restart (continueAfterDrain) start every pending
// whiteboard whose PR still sits at that head as the posted review would
// have (autoWhiteboard), and drop one whose PR moved on (whiteboard.stopped).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// whiteboardDrainText is the whiteboard.skipped message of a posted
// review's whiteboard that a drain skipped and the drain's end makes.
const whiteboardDrainText = "the daemon is draining; it is made after the restart"

// kvPRWhiteboardPending holds the whiteboard a drain skipped on a PR
// (whiteboardPending as JSON).
func kvPRWhiteboardPending(prID int64) string { return fmt.Sprintf("pr.%d.whiteboard_pending", prID) }

// whiteboardPending is a whiteboard a drain skipped: the reviewed head and
// when.
type whiteboardPending struct {
	Head string    `json:"head"`
	At   time.Time `json:"at"`
}

// parseWhiteboardPending reads a kvPRWhiteboardPending value; ok is false
// for one that names no head.
func parseWhiteboardPending(s string) (whiteboardPending, bool) {
	var p whiteboardPending
	if s == "" || json.Unmarshal([]byte(s), &p) != nil || p.Head == "" {
		return whiteboardPending{}, false
	}
	return p, true
}

// rememberWhiteboard records that a drain skipped the whiteboard of the
// PR's reviewed head (a later skip of the PR's newer head replaces it) and
// marks the drain as holding one (drainWhiteboards), so that a drain that
// ends without a restart makes it. It reports whether it did: a write that
// failed leaves the skip final, as it was.
func (e *Engine) rememberWhiteboard(ctx context.Context, subject string, prID int64, head string) bool {
	p := whiteboardPending{Head: head, At: e.now().UTC()}
	if err := writeKVJSON(ctx, e, kvPRWhiteboardPending(prID), "pending whiteboard", p); err != nil {
		e.log.Warn("whiteboard: the drain's skip cannot be remembered; the next posted review is the next chance", "subject", subject, "err", err)
		return false
	}
	e.mu.Lock()
	e.drainWhiteboards = true
	e.mu.Unlock()
	return true
}

// startPendingWhiteboards makes the whiteboards drains skipped (when says
// when: "after the restart", "after the drain") on a goroutine of its own
// under roundWG, as a `magnum whiteboard` request is answered off the tick:
// Whiteboard's readiness check may take up to whiteboardCmdTimeout. ctx is
// the daemon's, which the jobs derive from. The startup calls it after the
// recovery of the fix loops and of the whiteboard runs a stop left, and
// continueAfterDrain after the loops the drain held went on, so a fix cycle
// those start holds the PR first, as a posted review's fix loop does.
func (e *Engine) startPendingWhiteboards(ctx context.Context, when string) {
	e.mu.Lock()
	e.drainWhiteboards = false // the pass reads every entry, those of a skip from now on included
	e.mu.Unlock()
	if e.d.DryRun {
		return
	}
	e.roundWG.Go(func() {
		e.safely(ctx, "pending whiteboards", "", func() { e.pendingWhiteboards(ctx, when) }, nil)
	})
}

// pendingWhiteboards goes through the pending whiteboards, one pass at a
// time (pendingWhiteboardsMu), in key order.
func (e *Engine) pendingWhiteboards(ctx context.Context, when string) {
	e.pendingWhiteboardsMu.Lock()
	defer e.pendingWhiteboardsMu.Unlock()
	vals, err := e.st.KVValues(ctx, "pr.*.whiteboard_pending")
	if err != nil {
		e.warnUnlessStopped(ctx, err, "pending whiteboards")
		return
	}
	for _, key := range slices.Sorted(maps.Keys(vals)) {
		if ctx.Err() != nil {
			return
		}
		var id int64
		if _, err := fmt.Sscanf(key, "pr.%d.whiteboard_pending", &id); err != nil || kvPRWhiteboardPending(id) != key {
			continue
		}
		e.pendingWhiteboard(ctx, id, vals[key], when)
	}
}

// pendingWhiteboard makes or drops the whiteboard a drain skipped on PR
// prID (raw: its entry as read). The entry is taken first, only while it
// still holds raw (a newer skip of the PR replaced it: the next pass takes
// that one). A PR that closed or whose head moved since drops it with
// whiteboard.stopped; any other goes through the posted review's start
// (autoWhiteboard), whose refusals record whiteboard.skipped (a new drain
// remembers it again) and whose quiet cases (the watch no longer asks for
// whiteboards, the PR muted, the head has its whiteboard) are logged.
func (e *Engine) pendingWhiteboard(ctx context.Context, prID int64, raw, when string) {
	key := kvPRWhiteboardPending(prID)
	pr, err := e.st.PRByID(ctx, prID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		e.delKV(ctx, key)
		return
	case err != nil:
		e.warnUnlessStopped(ctx, err, "pending whiteboard: the PR", "pr", prID)
		return
	}
	repo, err := e.st.RepoByID(ctx, pr.RepoID)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "pending whiteboard: the PR's repository", "pr", prID)
		return
	}
	subject := prSubject(repo, pr.Number)
	switch taken, err := e.st.DeleteKVIf(ctx, key, raw); {
	case err != nil:
		e.warnUnlessStopped(ctx, err, "pending whiteboard: take the entry", "subject", subject)
		return
	case !taken:
		return
	}
	p, ok := parseWhiteboardPending(raw)
	if !ok {
		e.log.Warn("whiteboard: a pending whiteboard that cannot be read is dropped", "subject", subject)
		return
	}
	label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	switch {
	case pr.GHState != store.GHOpen:
		e.dropPendingWhiteboard(ctx, subject, label, pr, p, "the PR closed")
	case pr.HeadSHA != p.Head:
		e.dropPendingWhiteboard(ctx, subject, label, pr, p, "superseded by a push to "+textx.ShortSHA(pr.HeadSHA))
	default:
		e.log.Info("whiteboard: the whiteboard a drain skipped starts "+when, "subject", subject, "head", p.Head, "skipped_at", p.At)
		if none := e.autoWhiteboard(ctx, repo, pr, p.Head); none != "" {
			e.log.Info("whiteboard: the whiteboard a drain skipped is dropped", "subject", subject, "head", p.Head, "why", none)
		}
	}
}

// dropPendingWhiteboard records whiteboard.stopped for the whiteboard a
// drain skipped on pr at p.Head, which no job makes: why is "the PR
// closed" or "superseded by a push to <sha7>".
func (e *Engine) dropPendingWhiteboard(ctx context.Context, subject, label string, pr store.PR, p whiteboardPending, why string) {
	msg := fmt.Sprintf("the whiteboard of %s at %s, skipped by a drain, stopped: %s", label, textx.ShortSHA(p.Head), why)
	e.event(ctx, "info", subject, "whiteboard.stopped", msg,
		map[string]any{"head": p.Head, "pr_head": pr.HeadSHA, "skipped_at": store.FormatTime(p.At)})
}
