package engine

// The Codex budget: Codex reports its account's rate-limit windows in its own
// session files (internal/usage). The health tick reads them at most once a
// minute; at [usage] codex_soft full re-reviews wait, at codex_hard the kinds
// backed by Codex pause until the budget is below the cap again. A budget
// spent so fast that codex_soft comes before the window resets is told once
// per window (notePace). Codex's reading from a day ago, read at most every
// quarter hour, gives the last day's pace shown next to the window's average.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/pipeline"
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
	// paceMinElapsed is the share (percent) of a window that must have
	// elapsed before its pace is judged: the average over the first hours is
	// one burst, and the one warning a window gets is not for that.
	paceMinElapsed = 10
	// recentSpan is the stretch whose pace is shown next to the window's
	// average, which counts one-off spending days ago (eval replays, held
	// re-reviews); recentEvery bounds how often Codex's reading from
	// recentSpan ago is read.
	recentSpan  = 24 * time.Hour
	recentEvery = 15 * time.Minute
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
	e.readPast(ctx, now)
	e.recordBudget(ctx, *e.usageSnap, now)
	e.recordRecentPace(ctx, now)
	return currentBudget(*e.usageSnap, now), true
}

// readPast reads Codex's reading from recentSpan ago (Deps.UsageAt), the
// start of the recent pace, when the last read is recentEvery old. A read
// that fails keeps the previous reading, which is from before then too. The
// caller holds usageMu.
func (e *Engine) readPast(ctx context.Context, now time.Time) {
	if e.d.UsageAt == nil || (!e.usagePastRead.IsZero() && now.Sub(e.usagePastRead) < recentEvery) {
		return
	}
	e.usagePastRead = now
	past, err := e.d.UsageAt(ctx, e.cfg.Usage.CodexHome, now.Add(-recentSpan))
	switch {
	case err == nil:
		e.usagePast = &past
	case errors.Is(err, usage.ErrNoData):
		e.log.Debug("codex usage a day ago: no snapshot", "err", err)
	default:
		if e.changed("usage.codex_past", err.Error()) {
			e.log.Warn("codex usage a day ago", "err", err)
		}
	}
}

// recentPace is the pace of the cached budget's binding window over the
// last recentSpan, from Codex's reading then to now (usage.PaceSince); ok is
// false without such a reading or when it is not comparable. The caller
// holds usageMu.
func (e *Engine) recentPace(now time.Time) (float64, bool) {
	if e.usageSnap == nil || e.usagePast == nil {
		return 0, false
	}
	return usage.PaceSince(bindingWindow(currentBudget(*e.usageSnap, now)), *e.usagePast, now.Add(-recentSpan), now)
}

// recordRecentPace writes the recent pace (two decimals) to kv for `magnum
// status` when it changed, and deletes it when it is not known. The caller
// holds usageMu.
func (e *Engine) recordRecentPace(ctx context.Context, now time.Time) {
	pace, ok := e.recentPace(now)
	v := "none"
	if ok {
		v = strconv.FormatFloat(pace, 'f', 2, 64)
	}
	if v == e.usagePaceRecorded {
		return
	}
	e.usagePaceRecorded = v
	if !ok {
		e.delKV(ctx, KVUsageCodexPace24h)
		return
	}
	e.setKV(ctx, KVUsageCodexPace24h, v)
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
	e.notePace(ctx, snap, now)
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

// notePace warns, once per window, that the budget is spent faster than the
// window lasts: still below codex_soft, but at the average pace since the
// window began codex_soft comes before the reset, and full re-reviews wait from
// then on. One info toast on the batcher, whose key is reserved in the
// registry for the window's length (so a restarted daemon stays quiet), and
// one usage.pace event, guarded the same way. A dry run only records it. The
// text names the average pace and, when known, the last day's (recentPace),
// which leaves out one-off spending days ago; the warning goes by the
// average.
func (e *Engine) notePace(ctx context.Context, snap usage.Snapshot, now time.Time) {
	soft := e.cfg.Usage.CodexSoft
	if soft <= 0 || snap.Used() >= soft {
		return
	}
	w := bindingWindow(snap)
	pace, ok := usage.PaceOf(w.UsedPercent, w.WindowMinutes, w.ResetsAt, now)
	if !ok || pace.Elapsed() < paceMinElapsed {
		return
	}
	reach, ok := pace.Reach(soft)
	if !ok {
		return
	}
	window := strconv.FormatInt(w.ResetsAt.Unix(), 10)
	if !e.changed("usage.pace", window) { // tick goroutine; the registry decides across restarts
		return
	}
	subject := "tool:" + agents.KindCodex
	detail := fmt.Sprintf("At this pace it reaches %g%% (codex_soft) %s, before the reset %s: full re-reviews will wait. Pace %.1fx since the reset",
		soft, reach.Local().Format("Mon 15:04"), w.ResetsAt.Local().Format("Mon 15:04"), pace.Ratio())
	e.usageMu.Lock()
	recent, ok := e.recentPace(now)
	e.usageMu.Unlock()
	if ok {
		detail += fmt.Sprintf(", %.1fx in the last 24h", recent)
	}
	detail += "."
	if e.d.DryRun {
		e.rec.Record(ctx, subject, "pace", fmt.Sprintf("Codex budget %.0f%% used: %s", snap.Used(), detail))
		return
	}
	span := time.Duration(w.WindowMinutes) * time.Minute
	e.info(notify.Item{Key: "usage:codex-pace:" + window, Title: fmt.Sprintf("magnum: Codex budget at %.0f%%", snap.Used()),
		Body: detail, Window: span})
	switch first, err := e.st.ShouldSend(ctx, "usage:codex-pace-event:"+window, span); {
	case err != nil:
		e.log.Warn("codex pace event", "err", err)
	case first:
		e.event(ctx, "info", subject, "usage.pace", fmt.Sprintf("Codex budget %.0f%% used: %s", snap.Used(), detail), nil)
	}
}

// budgetGate is why a round must wait for the Codex budget ("" = go): at
// the soft cap an automatic full re-review whose roles run Codex waits
// (softCapHolds). The hard cap holds rounds through the kind pause
// (checkBudget).
func (e *Engine) budgetGate(job *roundJob, codexRoles int) string {
	if codexRoles == 0 || !softCapHolds(job) {
		return ""
	}
	level, snap := e.budgetLevel()
	if level != usage.Soft {
		return ""
	}
	return fmt.Sprintf("Codex budget %.0f%% used (soft cap %g%%): full re-reviews wait; first reviews, delta checks and `magnum review` still run",
		snap.Used(), e.cfg.Usage.CodexSoft)
}

// softCapHolds reports whether the soft cap holds job: a full re-review
// nobody asked for. A full re-review costs about twice a first review's
// Codex points (1.04 against 0.52-0.69) and posts no more findings, so it
// is the round to hold; a first review, a delta check, a re-review of the
// same head and a reply round (the judge alone), a continue of a paused
// round, and a round someone asked for (`magnum review`, a review request)
// still start. A delta check that the checkout turns into a full round
// (confirmDeltaCheck, confirmSameHead) has started and runs as one.
func softCapHolds(job *roundJob) bool {
	switch {
	case job.kind != pipeline.KindRereview, job.continued:
		return false
	case job.judgeOnly():
		return false
	case job.pr.Forced, job.requested:
		return false
	}
	return true
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
