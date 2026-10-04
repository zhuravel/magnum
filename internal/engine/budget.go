package engine

// The Codex budget: Codex reports its account's rate-limit windows in its own
// session files (internal/usage). The health tick reads them at most once a
// minute; at [usage] codex_soft first reviews wait, at codex_hard the kinds
// backed by Codex pause until the budget is below the cap again.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/usage"
)

const (
	// usageEvery bounds how often the Codex session files are read.
	usageEvery = time.Minute
	// BudgetPauseReason is the tool-pause reason of a kind paused at the
	// Codex hard cap ([usage] codex_hard).
	BudgetPauseReason = "budget_cap"
	// budgetPauseFallback is how long a hard-cap pause shows as lasting when
	// Codex named no reset time; the budget check renews or lifts it.
	budgetPauseFallback = time.Hour
	budgetToastWindow   = 12 * time.Hour
	budgetToastKey      = "usage:codex"
)

// readBudget returns the newest Codex snapshot, reading the session files
// when the last read is usageEvery old. A read that finds nothing keeps the
// previous snapshot; ok is false while none was ever read.
func (e *Engine) readBudget(ctx context.Context) (usage.Snapshot, bool) {
	e.usageMu.Lock()
	defer e.usageMu.Unlock()
	now := e.now()
	if e.usageRead.IsZero() || now.Sub(e.usageRead) >= usageEvery {
		e.usageRead = now
		snap, err := e.d.Usage(ctx, e.cfg.Usage.CodexHome, now)
		switch {
		case err == nil:
			e.usageSnap = &snap
		case errors.Is(err, usage.ErrNoData):
			e.log.Debug("codex usage: no snapshot", "err", err)
		default:
			if e.changed("usage.codex", err.Error()) {
				e.log.Warn("codex usage", "err", err)
			}
		}
	}
	if e.usageSnap == nil {
		return usage.Snapshot{}, false
	}
	e.recordBudget(ctx, *e.usageSnap, now)
	return currentBudget(*e.usageSnap, now), true
}

// currentBudget is snap with the windows whose reset passed since it was
// read counted as unused (usage.Codex does that at read time only).
func currentBudget(snap usage.Snapshot, now time.Time) usage.Snapshot {
	expire := func(w usage.Window) usage.Window {
		if !w.ResetsAt.IsZero() && !now.Before(w.ResetsAt) {
			w.UsedPercent = 0
		}
		return w
	}
	snap.Window = expire(snap.Window)
	if snap.Secondary != nil {
		w := expire(*snap.Secondary)
		snap.Secondary = &w
	}
	return snap
}

// bindingWindow is the window of snap that runs out first (the higher used
// share; the primary on a tie).
func bindingWindow(snap usage.Snapshot) usage.Window {
	if snap.Secondary != nil && snap.Secondary.UsedPercent > snap.UsedPercent {
		return *snap.Secondary
	}
	return snap.Window
}

// recordBudget writes the snapshot, as of now (a window whose reset passed
// reads 0%), to kv for `magnum status` when that changed.
func (e *Engine) recordBudget(ctx context.Context, snap usage.Snapshot, now time.Time) {
	cur := currentBudget(snap, now)
	w := bindingWindow(cur)
	key := fmt.Sprintf("%.0f %v %v", cur.Used(), w.ResetsAt, snap.At)
	if key == e.usageRecorded {
		return
	}
	e.usageRecorded = key
	e.setKV(ctx, KVUsageCodexPercent, strconv.FormatFloat(cur.Used(), 'f', 0, 64))
	if w.ResetsAt.IsZero() {
		e.delKV(ctx, KVUsageCodexResetsAt)
	} else {
		e.setKV(ctx, KVUsageCodexResetsAt, store.FormatTime(w.ResetsAt))
	}
	e.setKV(ctx, KVUsageCodexWindow, strconv.Itoa(w.WindowMinutes))
	e.setKV(ctx, KVUsageCodexPlan, snap.Plan)
	e.setKV(ctx, KVUsageCodexAt, store.FormatTime(snap.At))
}

// budgetLevel is the cached snapshot judged against [usage] (usage.OK while
// nothing was read).
func (e *Engine) budgetLevel() (usage.Level, usage.Snapshot) {
	e.usageMu.Lock()
	defer e.usageMu.Unlock()
	if e.usageSnap == nil {
		return usage.OK, usage.Snapshot{}
	}
	snap := currentBudget(*e.usageSnap, e.now())
	return usage.Decide(snap, e.cfg.Usage.CodexSoft, e.cfg.Usage.CodexHard), snap
}

// budgetKinds are the declared agent kinds backed by Codex (their pause
// holds every role whose AgentKind is one of them, shell roles with tool =
// "codex" included).
func (e *Engine) budgetKinds() []string {
	if e.isKind(agents.KindCodex) {
		return []string{agents.KindCodex}
	}
	return nil
}

// checkBudget reads the budget (readBudget) and applies the hard cap: the
// Codex kinds pause through the tool-pause path with one urgent toast, and
// resume on their own once the budget is below the cap (or the cap is
// turned off). A pause with another reason is left alone.
func (e *Engine) checkBudget(ctx context.Context) {
	kinds := e.budgetKinds()
	if len(kinds) == 0 || e.d.Usage == nil {
		return
	}
	if _, ok := e.readBudget(ctx); !ok {
		return
	}
	level, snap := e.budgetLevel()
	now := e.now()
	for _, kind := range kinds {
		p, paused := e.toolPause(ctx, kind)
		ours := paused && p.Reason == BudgetPauseReason
		switch {
		case level == usage.Hard && (!paused || ours):
			until := bindingWindow(snap).ResetsAt
			if until.IsZero() || !until.After(now) {
				until = now.Add(budgetPauseFallback)
			}
			if ours && p.Until.Equal(until) {
				continue
			}
			detail := fmt.Sprintf("Codex budget %.0f%% used (hard cap %g%%)", snap.Used(), e.cfg.Usage.CodexHard)
			if e.d.DryRun {
				e.rec.Record(ctx, "tool:"+kind, "pause", detail)
				continue
			}
			e.setToolPause(ctx, kind, BudgetPauseReason, detail, until)
			if !ours {
				e.event(ctx, "warn", "tool:"+kind, "tool.paused", fmt.Sprintf("%s paused (%s) until %s: %s", kind, BudgetPauseReason, until.Local().Format("15:04"), detail), nil)
				e.urgent(budgetToastKey, fmt.Sprintf("magnum: Codex budget at %.0f%%", snap.Used()),
					fmt.Sprintf("Rounds that need Codex pause until it is below %g%% (resets %s).", e.cfg.Usage.CodexHard, until.Local().Format("Mon 15:04")),
					budgetToastWindow)
			}
		case level != usage.Hard && ours:
			if e.d.DryRun {
				e.rec.Record(ctx, "tool:"+kind, "resume", "Codex budget below the hard cap")
				continue
			}
			e.clearToolPause(ctx, kind)
			e.forgetSend(ctx, budgetToastKey)
			e.event(ctx, "info", "tool:"+kind, "tool.resumed", fmt.Sprintf("%s pause (%s) ended: Codex budget %.0f%% used", kind, BudgetPauseReason, snap.Used()), nil)
		}
	}
}

// budgetGate is why a round must wait for the Codex budget ("" = go): at
// the soft cap a first review (kind initial) whose roles run Codex waits;
// re-reviews, continues and forced reviews do not. The hard cap holds rounds
// through the kind pause (checkBudget).
func (e *Engine) budgetGate(kind string, forced bool, codexRoles int) string {
	if kind != "initial" || forced || codexRoles == 0 {
		return ""
	}
	level, snap := e.budgetLevel()
	if level != usage.Soft {
		return ""
	}
	return fmt.Sprintf("Codex budget %.0f%% used (soft cap %g%%): first reviews wait; re-reviews and `magnum review` still run",
		snap.Used(), e.cfg.Usage.CodexSoft)
}

// budgetKnown reports whether a Codex snapshot was ever read.
func (e *Engine) budgetKnown() bool {
	e.usageMu.Lock()
	defer e.usageMu.Unlock()
	return e.usageSnap != nil
}

// codexGauge is the tab bar's "codex 87%" ("" while no snapshot was read).
func (e *Engine) codexGauge() string {
	e.usageMu.Lock()
	defer e.usageMu.Unlock()
	if e.usageSnap == nil {
		return ""
	}
	return fmt.Sprintf("codex %.0f%%", currentBudget(*e.usageSnap, e.now()).Used())
}
