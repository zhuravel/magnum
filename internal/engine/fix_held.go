package engine

// `magnum fix` while the daemon drains for a restart. A fixer started under
// a drain would end with the restart ("fix run lost in a daemon restart"),
// so no cycle starts then (errFixDraining), but the request is no longer
// refused: from 2026-10-14 on such refusals cost two fixes one and two days.
// It is held instead, in a registry row of its own (KVPRFixHeld: when it was
// asked and on which head), not in the loop's row: no loop runs yet, and
// every reader of KVPRFix (dispatch's hold, `magnum review`'s refusal, the
// board, `magnum fix --wait`) would read a held request as one. The restart
// starts it (startHeldFixes, at startup), and so does a drain lifted without
// a restart (continueDrainHeldLoops, as for the loops a drain held), when
// the review still stands at the head it was asked on; otherwise it is
// dropped with fix.dropped. `magnum abort` and the other stops of the PR
// take it back (dropHeldFix).

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// KVPRFixHeld holds a `magnum fix` a drain for a restart held (heldFix as
// JSON) until the restart, or the drain's end without one, starts it.
func KVPRFixHeld(prID int64) string { return fmt.Sprintf("pr.%d.fix_held", prID) }

// heldFix is a `magnum fix` held through a drain: when it was asked, the
// PR's head then (the head the review it would fix stood at) and who asked
// (fixBy, whose loop unpins a pinned PR as the request would have).
type heldFix struct {
	At   time.Time `json:"at"`
	Head string    `json:"head"`
	By   string    `json:"by"`
}

// kindFixDropped counts the held fixes dropped after a drain.
var kindFixDropped = notify.Kind{One: "held fix dropped", Many: "held fixes dropped"}

// holdFix answers `magnum fix` on pr while the daemon drains for a restart:
// every refusal of prepareFix, which leaves the drain to startFix, answers
// at once (a loop already running, nothing to fix, a PR that is not yours,
// ...), and
// otherwise the request is held (KVPRFixHeld) and answered so, with a
// fix.held event. The PR joins drainHeld, so a drain lifted without a
// restart starts it too. One request is held per PR: another answers that
// it is held already. A dry run (the request's or the daemon's) says it
// would hold it and holds nothing.
func (e *Engine) holdFix(ctx context.Context, pr store.PR, dry bool) (string, error) {
	job, err := e.prepareFix(ctx, pr, FixState{}, fixPinWouldUnpin, nil)
	if err != nil {
		return "", err
	}
	label := job.label()
	stands := fmt.Sprintf("the fix of %s starts after the restart if the review on %s still stands", label, textx.ShortSHA(job.review.SHA))
	if dry {
		e.rec.Record(ctx, job.subject(), "fix", "hold the fix of "+label+" for the restart")
		return "dry run: would hold the fix for the restart (the daemon is draining): " + stands, nil
	}
	key := KVPRFixHeld(pr.ID)
	if h, ok, err := readKVJSON[heldFix](ctx, e, key, "held fix"); err == nil && ok {
		return fmt.Sprintf("held already (asked %s); %s", h.At.Local().Format("15:04"), stands), nil
	}
	if err := writeKVJSON(ctx, e, key, "held fix", heldFix{At: e.now().UTC(), Head: job.head, By: fixBy}); err != nil {
		return "", fmt.Errorf("%s: the daemon is draining, and the fix could not be held for the restart: %w", label, err)
	}
	e.mu.Lock()
	e.drainHeld[pr.ID] = true
	e.mu.Unlock()
	e.event(ctx, "info", job.subject(), "fix.held", fmt.Sprintf("magnum fix held: the daemon drains for a restart; after the restart, %s %s if the review still stands",
		job.role.Name, job.fixes()), map[string]any{"head": job.head, "by": fixBy})
	return "held; " + stands + " (the daemon is draining)", nil
}

// startHeldFixes starts, once the daemon started, the fixes a drain held
// for the restart (startHeldFix). A dry run starts none.
func (e *Engine) startHeldFixes(ctx context.Context) {
	if e.d.DryRun {
		return
	}
	keys, err := e.st.KVKeys(ctx, "pr.*.fix_held")
	if err != nil {
		e.log.Warn("recovery: held fixes", "err", err)
		return
	}
	for _, key := range keys {
		var id int64
		if _, err := fmt.Sscanf(key, "pr.%d.fix_held", &id); err != nil || KVPRFixHeld(id) != key {
			continue
		}
		e.startHeldFix(ctx, id, "after the restart")
	}
}

// startHeldFix starts the fix a drain held on the PR prID (KVPRFixHeld),
// once the daemon restarted (startHeldFixes) or the drain ended without a
// restart (continueDrainHeldLoops); after says which, for the event. The
// request's row goes first, so it is acted on once. Its loop starts at
// cycle 1 (startFix, by `magnum fix`) while the PR still sits at the head it
// was asked on and the review there still stands with something to fix
// (prepareFix's refusals); otherwise the request is dropped with fix.dropped
// at warn, naming why, and an info toast: the operator was told it would
// start. A drain that began again meanwhile holds it again. It reports
// whether a fix was held.
func (e *Engine) startHeldFix(ctx context.Context, prID int64, after string) bool {
	key := KVPRFixHeld(prID)
	raw, found, err := e.st.GetKV(ctx, key)
	if err != nil || !found {
		return false
	}
	var h heldFix
	readErr := json.Unmarshal([]byte(raw), &h)
	if gone, err := e.st.DeleteKVIf(ctx, key, raw); err != nil || !gone {
		if err != nil {
			e.log.Warn("fix: take the held fix", "pr", prID, "err", err)
		}
		return true
	}
	pr, err := e.st.PRByID(ctx, prID)
	if err != nil {
		e.log.Warn("fix: the PR of a held fix", "pr", prID, "err", err)
		return true
	}
	repo, err := e.st.RepoByID(ctx, pr.RepoID)
	if err != nil {
		e.log.Warn("fix: the repository of a held fix", "pr", prID, "err", err)
		return true
	}
	subject := prSubject(repo, pr.Number)
	var why string
	switch {
	case readErr != nil:
		why = "its record does not read"
	case pr.HeadSHA != h.Head:
		why = fmt.Sprintf("the head moved to %s since it was asked on %s", textx.ShortSHA(pr.HeadSHA), textx.ShortSHA(h.Head))
	default:
		job, err := e.startFix(ctx, pr, cmp.Or(h.By, fixBy), FixState{}, nil)
		switch {
		case err == nil:
			e.log.Info("fix: the fix a drain held starts "+after, "subject", subject, "role", job.role.Name, "cycles", job.state.Cap)
			return true
		case errors.Is(err, errFixDraining): // another drain began: it holds the request again
			e.setKV(ctx, key, raw)
			e.mu.Lock()
			e.drainHeld[prID] = true
			e.mu.Unlock()
			return true
		}
		why = err.Error()
	}
	e.event(ctx, "warn", subject, "fix.dropped", fmt.Sprintf("the fix asked at %s during a drain is dropped %s: %s", h.At.Local().Format("15:04"), after, why),
		map[string]any{"asked_at": h.At, "head": h.Head, "why": why})
	title := fmt.Sprintf("magnum: fix dropped: %s#%d", repo.Name, pr.Number)
	e.info(notify.Item{Key: fmt.Sprintf("fix-dropped:%d:%s", pr.ID, store.FormatTime(h.At)), Title: title,
		Body: pr.URL + "\nthe fix asked during the drain: " + why, Line: strings.TrimPrefix(title, "magnum: ") + " " + pr.URL, Kind: kindFixDropped})
	return true
}

// dropHeldFix takes back the fix a drain held on pr (KVPRFixHeld) for a
// stop of the PR by by ("magnum abort", "magnum ignore", a close): its row
// goes and fix.dropped says so at info. It returns what it did, for the
// stop's answer ("" = nothing was held).
func (e *Engine) dropHeldFix(ctx context.Context, repo store.Repo, pr store.PR, by string) string {
	key := KVPRFixHeld(pr.ID)
	raw, found, err := e.st.GetKV(ctx, key)
	if err != nil || !found {
		return ""
	}
	if gone, err := e.st.DeleteKVIf(ctx, key, raw); err != nil || !gone {
		return ""
	}
	const note = "the fix held for the restart is dropped"
	e.event(ctx, "info", prSubject(repo, pr.Number), "fix.dropped", note+" by "+by, map[string]any{"by": by})
	return note
}
