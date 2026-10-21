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
//
// The GitHub floor holds a fix the same way (github_floor.go): a `magnum
// fix` or an autofix that would start while the budget of the identity it
// reads as is below [github] budget_floor, or whose read of the other
// reviewers' items GitHub refused for the rate limit, is held (heldFix.Why
// heldBudget), and the tick that finds the budget recovered starts it
// (continueBudgetHeld), as does the restart, with the same checks.
//
// A held fix whose fixer's agent kind is paused (a usage limit, a login)
// when it would start waits out the pause instead of being dropped
// (heldPause, with the kind): talkable#12054's fix, held through a drain on
// 2026-10-19, was dropped after the restart for claude's usage limit. The
// tick that finds the kind no longer paused starts it (continuePauseHeld),
// with the same checks. One row holds whichever of the three holds applies
// now: a pause hold whose start finds the budget low becomes a budget
// hold, and the reverse.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// KVPRFixHeld holds a fix that could not start yet (heldFix as JSON): a
// `magnum fix` a drain for a restart held until the restart, or the drain's
// end without one, starts it; a `magnum fix` or an autofix the GitHub
// budget held until it recovers; one the fixer's paused kind held until the
// pause ends.
func KVPRFixHeld(prID int64) string { return fmt.Sprintf("pr.%d.fix_held", prID) }

// heldFix is a held fix: when it was asked, the PR's head then (the head
// the review it would fix stood at), who asked (fixBy, whose loop unpins a
// pinned PR as the request would have; autofixBy for an autofix the GitHub
// floor held) and why it is held ("" for a drain, heldBudget for the GitHub
// budget, heldPause for the fixer's paused agent kind, Kind).
type heldFix struct {
	At   time.Time `json:"at"`
	Head string    `json:"head"`
	By   string    `json:"by"`
	Why  string    `json:"why,omitempty"`
	Kind string    `json:"kind,omitempty"`
}

const (
	// heldBudget is the heldFix.Why of a fix the GitHub budget held.
	heldBudget = "github_budget"
	// heldPause is the heldFix.Why of a fix the pause of its fixer's agent
	// kind (heldFix.Kind) held.
	heldPause = "kind_pause"
)

// HeldFixStarts is when the held fix raw (a KVPRFixHeld value) starts, for
// `magnum fix --wait`: "after the restart" for a drain's, "when the GitHub
// budget recovers" for the GitHub budget's, "when claude's pause ends" for
// the fixer's pause.
func HeldFixStarts(raw string) string {
	var h heldFix
	if json.Unmarshal([]byte(raw), &h) != nil {
		return "after the restart"
	}
	switch h.Why {
	case heldBudget:
		return "when the GitHub budget recovers"
	case heldPause:
		return "when " + h.kind() + "'s pause ends"
	}
	return "after the restart"
}

// kind is the paused agent kind a pause hold waits for, in words.
func (h heldFix) kind() string { return cmp.Or(h.Kind, "the fixer") }

// fixHold is why holdFix holds a fix: a drain for a restart (the zero
// value) or a GitHub budget below the floor (low), with the job startFix
// prepared before it refused (nil: holdFix prepares it).
type fixHold struct {
	low *githubLowBudget
	job *fixJob
}

// during is what a held fix was held during, for its drop.
func (h heldFix) during() string {
	switch h.Why {
	case heldBudget:
		return "while the GitHub budget was low"
	case heldPause:
		return "while " + h.kind() + " was paused"
	}
	return "during a drain"
}

// kindFixDropped counts the held fixes dropped after a drain.
var kindFixDropped = notify.Kind{One: "held fix dropped", Many: "held fixes dropped"}

// holdFix answers `magnum fix` on pr while the daemon drains for a restart,
// or while the GitHub budget of the identity it reads as is below the floor
// (hold.low; also when GitHub refused prepareFix's read of the other
// reviewers' items for the rate limit): every refusal of prepareFix, which
// leaves the drain and the floor to startFix, answers at once (a loop
// already running, nothing to fix, a PR that is not yours, ...), and
// otherwise the request is held
// (KVPRFixHeld) and answered so, with a fix.held event. by is who asks
// (fixBy; autofixBy for an autofix the floor held, which answers nobody).
// The PR joins drainHeld (budgetHeld for the floor), so a drain lifted
// without a restart (the budget's recovery) starts it too. One request is
// held per PR: another answers that it is held already. A dry run (the
// request's or the daemon's) says it would hold it and holds nothing.
func (e *Engine) holdFix(ctx context.Context, pr store.PR, dry bool, by string, hold fixHold) (string, error) {
	job := hold.job
	if job == nil {
		var err error
		if job, err = e.prepareFix(ctx, pr, FixState{}, fixPinWouldUnpin, nil); err != nil {
			return "", err
		}
	}
	label := job.label()
	// when it starts, why it is held (the answer's and the event's words)
	// and what the hold waits for.
	when, why, cause, what := "after the restart", "the daemon is draining", "the daemon drains for a restart", "for the restart"
	rec := heldFix{At: e.now().UTC(), Head: job.head, By: by}
	if hold.low != nil {
		when, why, cause, what = "once the GitHub budget of "+hold.low.identity+" recovers", hold.low.text(), hold.low.text(), "until the GitHub budget recovers"
		rec.Why = heldBudget
	}
	stands := fmt.Sprintf("the fix of %s starts %s if the review on %s still stands", label, when, textx.ShortSHA(job.review.SHA))
	if dry {
		e.rec.Record(ctx, job.subject(), "fix", "hold the fix of "+label+" "+what)
		return "dry run: would hold the fix " + what + " (" + why + "): " + stands, nil
	}
	key := KVPRFixHeld(pr.ID)
	if h, ok, err := readKVJSON[heldFix](ctx, e, key, "held fix"); err == nil && ok {
		return fmt.Sprintf("held already (asked %s); %s", h.At.Local().Format("15:04"), stands), nil
	}
	if err := writeKVJSON(ctx, e, key, "held fix", rec); err != nil {
		return "", fmt.Errorf("%s: %s, and the fix could not be held %s: %w", label, why, what, err)
	}
	e.mu.Lock()
	if hold.low != nil {
		e.budgetHeld[pr.ID] = true
	} else {
		e.drainHeld[pr.ID] = true
	}
	e.mu.Unlock()
	asked := "magnum fix"
	if by == autofixBy {
		asked = "autofix"
	}
	e.event(ctx, "info", job.subject(), "fix.held", fmt.Sprintf("%s held: %s; %s, %s %s if the review still stands",
		asked, cause, when, job.role.Name, job.fixes()), map[string]any{"head": job.head, "by": by, "why": rec.Why})
	return "held; " + stands + " (" + why + ")", nil
}

// startHeldFixes starts, once the daemon started, the fixes held for the
// restart, and decides again those the GitHub budget or the fixer's pause
// held (startHeldFix: one still held is held again, in budgetHeld or
// pauseHeld). A dry run starts none.
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

// startHeldFix starts the fix held on the PR prID (KVPRFixHeld), once the
// daemon restarted (startHeldFixes), the drain ended without a restart
// (continueDrainHeldLoops), the GitHub budget recovered
// (continueBudgetHeld) or the fixer's pause ended (continuePauseHeld);
// after says which, for the event. The request's row goes first, so it is
// acted on once. Its loop starts at cycle 1 (startFix, by who asked) while
// the PR still sits at the head it was asked on and the review there still
// stands with something to fix (prepareFix's refusals); otherwise the
// request is dropped with fix.dropped at warn, naming why, and an info
// toast: the operator was told it would start. A drain that began again
// meanwhile holds it again, and so does a GitHub budget below the floor (as
// a fix the floor held, budgetHeld) and a paused agent kind of the fix role
// (heldPause, pauseHeld): the daemon waits those out. An autofix's drop
// toasts nothing: the operator asked for none. It reports whether a fix was
// held.
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
	var why, gone string
	if readErr == nil && h.By == autofixBy {
		gone = e.autofixGone(ctx, repo, pr) // nobody asked: autofix's own checks again (fix_loop.go)
	}
	switch {
	case readErr != nil:
		why = "its record does not read"
	case pr.HeadSHA != h.Head:
		why = fmt.Sprintf("the head moved to %s since it was asked on %s", textx.ShortSHA(pr.HeadSHA), textx.ShortSHA(h.Head))
	case gone != "":
		why = gone
	default:
		job, err := e.startFix(ctx, pr, cmp.Or(h.By, fixBy), FixState{}, nil)
		var paused *kindPausedError
		switch {
		case err == nil:
			e.log.Info("fix: the held fix starts "+after, "subject", subject, "role", job.role.Name, "cycles", job.state.Cap)
			return true
		case errors.Is(err, errFixDraining): // another drain began: it holds the request again
			e.setKV(ctx, key, raw)
			e.mu.Lock()
			e.drainHeld[prID] = true
			e.mu.Unlock()
			return true
		case errors.Is(err, errGitHubBudget), errors.Is(err, github.ErrRateLimited): // github_floor.go: it waits for the budget
			h.Why = heldBudget
			if err := writeKVJSON(ctx, e, key, "held fix", h); err != nil {
				e.setKV(ctx, key, raw)
			}
			e.holdForBudget(prID)
			e.log.Info("fix: the held fix waits for the GitHub budget", "subject", subject, "why", err.Error())
			return true
		case errors.As(err, &paused): // the fixer's kind is paused: it waits for the pause's end
			h.Why, h.Kind = heldPause, paused.kind
			if werr := writeKVJSON(ctx, e, key, "held fix", h); werr != nil {
				e.setKV(ctx, key, raw)
			}
			e.holdForPause(prID)
			e.log.Info("fix: the held fix waits for the pause of "+paused.kind, "subject", subject, "why", err.Error())
			return true
		}
		why = err.Error()
	}
	asked := "the fix asked"
	if h.By == autofixBy {
		asked = "the autofix held"
	}
	e.event(ctx, "warn", subject, "fix.dropped", fmt.Sprintf("%s at %s %s is dropped %s: %s", asked, h.At.Local().Format("15:04"), h.during(), after, why),
		map[string]any{"asked_at": h.At, "head": h.Head, "why": why, "by": h.By})
	if h.By == autofixBy {
		return true
	}
	title := fmt.Sprintf("magnum: fix dropped: %s#%d", repo.Name, pr.Number)
	e.info(notify.Item{Key: fmt.Sprintf("fix-dropped:%d:%s", pr.ID, store.FormatTime(h.At)), Title: title,
		Body: pr.URL + "\nthe fix asked " + strings.Replace(h.during(), "during a drain", "during the drain", 1) + ": " + why, Line: strings.TrimPrefix(title, "magnum: ") + " " + pr.URL, Kind: kindFixDropped})
	return true
}

// dropHeldFix takes back the fix held on pr (KVPRFixHeld) for a stop of
// the PR by by ("magnum abort", "magnum ignore", a close): its row goes and
// fix.dropped says so at info, naming the hold it was in ("the fix held
// while the GitHub budget was low is dropped"). It returns what it did, for
// the stop's answer ("" = nothing was held).
func (e *Engine) dropHeldFix(ctx context.Context, repo store.Repo, pr store.PR, by string) string {
	key := KVPRFixHeld(pr.ID)
	raw, found, err := e.st.GetKV(ctx, key)
	if err != nil || !found {
		return ""
	}
	if gone, err := e.st.DeleteKVIf(ctx, key, raw); err != nil || !gone {
		return ""
	}
	var h heldFix
	_ = json.Unmarshal([]byte(raw), &h) // a record that does not read says "during a drain", the hold it had before Why
	note := "the fix held " + h.during() + " is dropped"
	e.event(ctx, "info", prSubject(repo, pr.Number), "fix.dropped", note+" by "+by, map[string]any{"by": by, "why": h.Why})
	return note
}

// holdForPause marks the PR prID as holding a fix for its fixer's paused
// agent kind: the tick that finds the kind no longer paused starts it
// (continuePauseHeld).
func (e *Engine) holdForPause(prID int64) {
	e.mu.Lock()
	e.pauseHeld[prID] = true
	e.mu.Unlock()
}

// continuePauseHeld starts, once a tick finds the agent kind of a held
// fix's fix role no longer paused, the fixes that pause held (pauseHeld),
// as the restart would start them (startHeldFix: the head, the review, an
// autofix's own checks, and a GitHub budget below the floor or a pause
// begun again holding it anew). A held fix whose watch or fix role is gone
// is started too, for startHeldFix to drop it with the refusal. Nothing
// starts while the daemon drains: the drain's end or the restart decides
// them then.
func (e *Engine) continuePauseHeld(ctx context.Context) {
	if e.d.DryRun || e.draining(ctx) || ctx.Err() != nil {
		return
	}
	e.mu.Lock()
	held := slices.Sorted(maps.Keys(e.pauseHeld))
	e.mu.Unlock()
	for _, id := range held {
		pr, err := e.st.PRByID(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				e.mu.Lock()
				delete(e.pauseHeld, id)
				e.mu.Unlock()
			}
			continue
		}
		repo, err := e.st.RepoByID(ctx, pr.RepoID)
		if err != nil {
			continue
		}
		kind := ""
		if w := e.cfg.WatchFor(repo.FullName()); w != nil {
			if role, ok := e.cfg.FixRoleFor(w); ok {
				kind = role.AgentKind()
			}
		}
		if kind != "" && e.kindPauseReason(ctx, []string{kind}) != "" {
			continue
		}
		e.mu.Lock()
		delete(e.pauseHeld, id)
		e.mu.Unlock()
		e.startHeldFix(ctx, id, "after "+cmp.Or(kind, "the fixer")+"'s pause ended")
	}
}
