package engine

// The shell hold (DECISIONS "A cut shell command is killed, and a review
// that missed a reviewer is completed"): nothing switches a slot's checkout
// while a command still runs in one of its PR's shell panes there. Before a
// round's checkout, the fixer's guard, `magnum open`'s checkout and a
// release, clearShells runs the agents manager's cleanup
// (ClearShellPanes: an own leftover is stopped). A pane still busy holds
// the switch: a command magnum did not start, or its own command it cannot
// stop. The hold costs no attempt and no round of the daily cap, retries
// after 1, 2 and 5 minutes, then every 10, says so once when it starts
// (pr.shell_hold, one toast) and once when it ends (pr.shell_hold_ended),
// and after shellHoldAttention moves the PR to needs_attention with its
// plain reason; the reconcile then looks again every 10 minutes. It never
// marks a human at work: nothing mid-round starts a cooldown.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/store"
)

// Events of the shell hold, on the PR.
const (
	eventShellHold      = "pr.shell_hold"       // warn: the hold starts (its reason, the slot, the role and pane)
	eventShellHoldEnded = "pr.shell_hold_ended" // info: the slot's shell panes are clear again
)

// shellHoldAttention is how long a hold lasts before the PR needs the
// operator's attention.
const shellHoldAttention = 15 * time.Minute

// shellHoldToastWindow dedupes the hold's one toast (its start, and the
// attention that follows) for the hold's life.
const shellHoldToastWindow = 24 * time.Hour

// kvPRShellHold is the PR's shell hold (shellHold, JSON).
func kvPRShellHold(prID int64) string { return fmt.Sprintf("pr.%d.shell_hold", prID) }

// shellHold is a PR's hold: since when, on which slot, whether the command
// is one magnum did not start, the plain reason, how many looks found it,
// when the next is due, and whether the PR went to needs_attention for it.
type shellHold struct {
	Since     time.Time `json:"since"`
	Slot      string    `json:"slot"`
	Foreign   bool      `json:"foreign"`
	Reason    string    `json:"reason"`
	Tries     int       `json:"tries"`
	Next      time.Time `json:"next"`
	Attention bool      `json:"attention,omitempty"`
}

// shellHoldDelay is the wait before the next look after tries looks found
// the pane busy: 1, 2 and 5 minutes, then every 10.
func shellHoldDelay(tries int) time.Duration {
	switch tries {
	case 1:
		return time.Minute
	case 2:
		return 2 * time.Minute
	case 3:
		return 5 * time.Minute
	}
	return 10 * time.Minute
}

// shellHoldReason is the hold's plain reason for slot.
func shellHoldReason(slot string, h *agents.ShellHoldError) string {
	if h.Foreign {
		return "a command magnum did not start keeps the shell pane of " + slot + " busy"
	}
	return "magnum cannot stop its own command in " + slot + ": " + h.Cause
}

// shellHoldError is a hold as the switch's error: its plain reason, when to
// look again, and whether the hold lasted shellHoldAttention.
type shellHoldError struct {
	reason    string
	retryAt   time.Time
	attention bool
	key       string // the hold's toast key
	err       error
}

func (e *shellHoldError) Error() string { return e.reason }
func (e *shellHoldError) Unwrap() error { return e.err }

// shellHoldOf reads the PR's hold.
func (e *Engine) shellHoldOf(ctx context.Context, prID int64) (shellHold, bool) {
	raw, ok := e.getKV(ctx, kvPRShellHold(prID))
	if !ok || raw == "" {
		return shellHold{}, false
	}
	var h shellHold
	if json.Unmarshal([]byte(raw), &h) != nil {
		return shellHold{}, false
	}
	return h, true
}

func (e *Engine) saveShellHold(ctx context.Context, prID int64, h shellHold) {
	b, err := json.Marshal(h)
	if err != nil {
		return
	}
	e.setKV(ctx, kvPRShellHold(prID), string(b))
}

// shellHoldKey is the hold's toast key: the PR and the hold's start.
func shellHoldKey(prID int64, since time.Time) string {
	return fmt.Sprintf("shell_hold:%d:%s", prID, store.FormatTime(since))
}

// clearShells runs the cleanup of PR pr's shell panes in slot
// (Agents.ClearShellPanes) before something switches slot's checkout, and
// keeps the PR's hold: nil when nothing runs there any more (a hold that
// was on ends), a *shellHoldError while a command keeps a pane busy, any
// other error as it came.
func (e *Engine) clearShells(ctx context.Context, pr store.PR, slot store.Slot) error {
	if e.d.Agents == nil || slot.Path == "" {
		return nil
	}
	err := e.d.Agents.ClearShellPanes(ctx, pr, slot.Path)
	var held *agents.ShellHoldError
	switch {
	case err == nil:
		e.endShellHold(ctx, pr)
		return nil
	case !errors.As(err, &held):
		return err
	}
	now := e.now()
	h, on := e.shellHoldOf(ctx, pr.ID)
	if !on {
		h = shellHold{Since: now, Slot: slot.Name}
	}
	h.Foreign, h.Reason = held.Foreign, shellHoldReason(slot.Name, held)
	h.Tries++
	h.Next = now.Add(shellHoldDelay(h.Tries))
	e.saveShellHold(ctx, pr.ID, h)
	key := shellHoldKey(pr.ID, h.Since)
	if !on {
		repo, _ := e.st.RepoByID(ctx, pr.RepoID)
		label := fmt.Sprintf("%s#%d", repo.Name, pr.Number)
		e.event(ctx, "warn", prSubject(repo, pr.Number), eventShellHold, h.Reason+": nothing switches its checkout until it is free",
			map[string]any{"slot": slot.Name, "role": held.Role, "pane": held.Pane, "foreign": held.Foreign})
		e.urgent(key, "magnum: "+label+" waits for a shell pane", h.Reason, shellHoldToastWindow)
	}
	return &shellHoldError{reason: h.Reason, retryAt: h.Next, attention: now.Sub(h.Since) >= shellHoldAttention, key: key, err: err}
}

// endShellHold ends the PR's hold, if one is on: pr.shell_hold_ended.
func (e *Engine) endShellHold(ctx context.Context, pr store.PR) {
	h, on := e.shellHoldOf(ctx, pr.ID)
	if !on {
		return
	}
	e.delKV(ctx, kvPRShellHold(pr.ID))
	repo, _ := e.st.RepoByID(ctx, pr.RepoID)
	took := e.now().Sub(h.Since).Round(time.Second)
	e.event(ctx, "info", prSubject(repo, pr.Number), eventShellHoldEnded,
		fmt.Sprintf("the shell panes of %s are free again after %s: its checkout may switch", h.Slot, took),
		map[string]any{"slot": h.Slot, "tries": h.Tries, "seconds": int(took / time.Second)})
}

// recheckShellHolds looks again, at the reconcile, at the PRs a hold moved
// to needs_attention (attention.KindShellHold) once their next look is
// due: one whose shell panes are free again (or that has no slot any more)
// goes back in line at once; one still held waits for its next look.
func (e *Engine) recheckShellHolds(ctx context.Context) {
	prs, err := e.st.ListPRs(ctx, store.PRFilter{States: []string{store.PRNeedsAttention}})
	if err != nil {
		e.warnUnlessStopped(ctx, err, "reconcile: held PRs")
		return
	}
	now := e.now()
	for _, pr := range prs {
		if kind, _ := e.getKV(ctx, KVPRAttention(pr.ID)); kind != attention.KindShellHold {
			continue
		}
		if h, on := e.shellHoldOf(ctx, pr.ID); on && now.Before(h.Next) {
			continue
		}
		if !e.reserveSlotWork(pr.ID, "its shell panes are being looked at") {
			continue
		}
		e.recheckShellHold(ctx, pr)
		e.endEviction(pr.ID)
	}
}

func (e *Engine) recheckShellHold(ctx context.Context, pr store.PR) {
	slot, has, err := e.slotOf(ctx, pr.ID)
	if err != nil {
		return
	}
	if has {
		if err := e.clearShells(ctx, pr, slot); err != nil {
			return // still held: clearShells set its next look
		}
	} else {
		e.endShellHold(ctx, pr)
	}
	if err := e.st.TransitionPR(ctx, pr.ID, []string{store.PRNeedsAttention}, claimableState(pr), func(u *store.PRUpdate) {
		u.Set("next_attempt_at", e.now())
		u.Set("last_error", nil)
	}); err != nil {
		e.log.Info("held PR moved on", "pr", pr.ID, "err", err)
		return
	}
	e.delKV(ctx, KVPRAttention(pr.ID))
}
