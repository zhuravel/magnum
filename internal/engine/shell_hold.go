package engine

// The shell hold (DECISIONS "A cut shell command is killed, and a review
// that missed a reviewer is completed"): nothing switches a slot's checkout
// while a command still runs in one of its PR's shell panes there. Before a
// round's checkout, the fixer's guard, a checkout for `magnum open` or
// `magnum fix` and a release, clearShells closes the runs a crashed daemon
// left open and runs the agents manager's cleanup (ClearShellPanes: an own
// leftover is stopped). A pane still busy holds the switch: a command
// magnum did not start, a custom role's command without the run token, or
// its own command it cannot stop. The hold costs no attempt and no round of
// the daily cap, retries after 1, 2 and 5 minutes, then every 10, says so
// once when it starts (pr.shell_hold, one toast) and once when it ends
// (pr.shell_hold_ended), and after shellHoldAttention of a round's hold
// moves the PR to needs_attention with its plain reason; the reconcile then
// looks again every 10 minutes, and looks at a hold that started outside a
// round until it ends. It never marks a human at work: nothing mid-round
// starts a cooldown. A herdr that cannot say what the panes run is no hold:
// the switch waits a minute, with no event and no toast.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/store"
)

// Events of the shell hold, on the PR.
const (
	eventShellHold      = "pr.shell_hold"       // warn: the hold starts (its reason, the slot, the role and pane, who looked)
	eventShellHoldEnded = "pr.shell_hold_ended" // info: the slot's shell panes are clear again
)

// What looked at the slot's shell panes (shellHold.Origin): a round's setup
// (and the reconcile's look at a held round), else the job that would
// switch the checkout outside a round.
const (
	holdRound   = "round"
	holdFix     = "fix"
	holdClaim   = "claim" // a checkout for magnum open or magnum fix
	holdRelease = "release"
)

// shellHoldAttention is how long a round's hold lasts before the PR needs
// the operator's attention.
const shellHoldAttention = 15 * time.Minute

// shellHoldStale is how long after its last look a hold record counts as
// over: longer than the longest wait between two looks of one hold (10
// minutes, plus a reconcile's interval), so a look after it starts a hold
// of its own.
const shellHoldStale = 30 * time.Minute

// shellHoldToastWindow dedupes the hold's one toast (its start, and the
// attention that follows) for the hold's life.
const shellHoldToastWindow = 24 * time.Hour

// kvPRShellHold is the PR's shell hold (shellHold, JSON).
func kvPRShellHold(prID int64) string { return fmt.Sprintf("pr.%d.shell_hold", prID) }

// shellHold is a PR's hold: since when, on which slot, whether the command
// is one magnum did not start, the plain reason, how many looks found it,
// the last look, when the next is due, what started it (holdRound or the
// job outside a round), and whether the PR went to needs_attention for it.
type shellHold struct {
	Since     time.Time `json:"since"`
	Slot      string    `json:"slot"`
	Foreign   bool      `json:"foreign"`
	Reason    string    `json:"reason"`
	Tries     int       `json:"tries"`
	Last      time.Time `json:"last"`
	Next      time.Time `json:"next"`
	Origin    string    `json:"origin,omitempty"`
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
	switch {
	case h.Foreign:
		return "a command magnum did not start keeps the shell pane of " + slot + " busy"
	case h.Tokenless:
		return "the " + h.Role + " command (no run token) still runs in the shell pane of " + slot
	}
	return "magnum cannot stop its own command in " + slot + ": " + h.Cause
}

// shellHoldError is a hold as the switch's error: its plain reason, when to
// look again, and whether a round's hold lasted shellHoldAttention.
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
// (Agents.ClearShellPanes) before something switches slot's checkout,
// origin saying what (holdRound or a job outside a round), and keeps the
// PR's hold: nil when nothing runs there any more (a hold that was on
// ends), a *shellHoldError while a command keeps a pane busy, an error
// wrapping agents.ErrBusy (and agents.ErrHerdrUnread) when herdr cannot
// say, which leaves the hold as it was, any other error as it came. The
// runs a crashed daemon left open are closed first (none of a paused
// round's), so magnum's own leftover there counts as over and is stopped,
// not held for. A round's look at a hold that started outside a round, or
// at one whose last look is shellHoldStale old, starts a hold of its own.
func (e *Engine) clearShells(ctx context.Context, pr store.PR, slot store.Slot, origin string) error {
	if e.d.Agents == nil || slot.Path == "" {
		return nil
	}
	if cur, err := e.st.PRByID(ctx, pr.ID); err == nil && cur.State != store.PRPaused {
		e.finalizeRuns(ctx, pr.ID, "closed before its slot's checkout switched: no round runs it")
	}
	err := e.d.Agents.ClearShellPanes(ctx, pr, slot.Path)
	var held *agents.ShellHoldError
	switch {
	case err == nil:
		e.endShellHold(ctx, pr)
		return nil
	case errors.Is(err, agents.ErrHerdrUnread):
		return fmt.Errorf("herdr cannot say what the shell panes of %s run, so its checkout waits (%w): %w", slot.Name, err, agents.ErrBusy)
	case !errors.As(err, &held):
		return err
	}
	now := e.now()
	h, on := e.shellHoldOf(ctx, pr.ID)
	fresh := !on || origin == holdRound && (h.Origin != holdRound || now.Sub(cmpTime(h.Last, h.Since)) > shellHoldStale)
	if fresh {
		h = shellHold{Since: now, Slot: slot.Name, Origin: origin}
	}
	h.Foreign, h.Reason, h.Last = held.Foreign, shellHoldReason(slot.Name, held), now
	h.Tries++
	h.Next = now.Add(shellHoldDelay(h.Tries))
	e.saveShellHold(ctx, pr.ID, h)
	key := shellHoldKey(pr.ID, h.Since)
	if fresh {
		repo, _ := e.st.RepoByID(ctx, pr.RepoID)
		label := fmt.Sprintf("%s#%d", repo.Name, pr.Number)
		e.event(ctx, "warn", prSubject(repo, pr.Number), eventShellHold, h.Reason+": nothing switches its checkout until it is free",
			map[string]any{"slot": slot.Name, "role": held.Role, "pane": held.Pane, "foreign": held.Foreign, "origin": origin})
		e.urgent(key, "magnum: "+label+" waits for a shell pane", h.Reason, shellHoldToastWindow)
	}
	attention := origin == holdRound && h.Origin == holdRound && now.Sub(h.Since) >= shellHoldAttention
	return &shellHoldError{reason: h.Reason, retryAt: h.Next, attention: attention, key: key, err: err}
}

// cmpTime is t, else alt when t is zero (a record an older build wrote
// without its last look).
func cmpTime(t, alt time.Time) time.Time {
	if t.IsZero() {
		return alt
	}
	return t
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

// recheckShellHolds looks again, at the reconcile, once their next look is
// due, at the PRs a round's hold moved to needs_attention
// (attention.KindShellHold): one whose shell panes are free again (or that
// has no slot any more) goes back in line at once; one still held waits
// for its next look. It looks the same way at a hold that started outside
// a round (the fixer's, a claim's, a release's), whose job does not try
// again, so the hold ends once the panes are free: never while a round,
// the fixer or a paused round has the PR (a round looks for itself).
func (e *Engine) recheckShellHolds(ctx context.Context) {
	prs, err := e.st.ListPRs(ctx, store.PRFilter{States: []string{store.PRNeedsAttention}})
	if err != nil {
		e.warnUnlessStopped(ctx, err, "reconcile: held PRs")
		return
	}
	now := e.now()
	looked := map[int64]bool{}
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
		looked[pr.ID] = true
		e.recheckShellHold(ctx, pr)
		e.endEviction(pr.ID)
	}
	holds, err := e.st.KVValues(ctx, "pr.*.shell_hold")
	if err != nil {
		e.warnUnlessStopped(ctx, err, "reconcile: shell holds")
		return
	}
	for key, raw := range holds {
		var h shellHold
		var id int64
		if json.Unmarshal([]byte(raw), &h) != nil || h.Origin == holdRound || now.Before(h.Next) {
			continue
		}
		if _, err := fmt.Sscanf(key, "pr.%d.shell_hold", &id); err != nil || looked[id] {
			continue
		}
		pr, err := e.st.PRByID(ctx, id)
		if err != nil || slices.Contains(store.InFlightStates, pr.State) || pr.State == store.PRPaused {
			continue
		}
		if !e.reserveSlotWork(pr.ID, "its shell panes are being looked at") {
			continue
		}
		slot, has, err := e.slotOf(ctx, pr.ID)
		switch {
		case err != nil:
		case !has:
			e.endShellHold(ctx, pr)
		default:
			_ = e.clearShells(ctx, pr, slot, h.Origin) // still held: it set the next look
		}
		e.endEviction(pr.ID)
	}
}

func (e *Engine) recheckShellHold(ctx context.Context, pr store.PR) {
	slot, has, err := e.slotOf(ctx, pr.ID)
	if err != nil {
		return
	}
	if has {
		if err := e.clearShells(ctx, pr, slot, holdRound); err != nil {
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
