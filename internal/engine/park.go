package engine

// Parking idle agents: a reviewed PR's agents sit idle until its next push,
// often for hours (21 processes, about 2.8 GB, idle 10.8 hours on average).
// Once every live agent of a reviewed PR has been idle for [daemon]
// park_idle_after the PR's sessions are parked; the next round resumes them
// as after any park. A PR that waits for its round behind a pause, a drain,
// the daily cap or a wait ending more than park_idle_after away is parked
// the same way (under `magnum pause`, two re-reviews held 8 agents idle for
// up to 2 hours). A park that fails is tried again only after twice the
// last wait, and its cause is recorded once (parkBackoff); after one that
// failed because an agent did not quit, the idle park tries the hard mode.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// parkIdle queues a park for every reviewed PR, and every PR whose round is
// far off (waitsLong), whose live agents have all been idle longer than
// park_idle_after (0 = never) and whose last failed park, if any, has
// waited out its backoff (parkHeld). It runs right after noteWaits recorded
// the waits. The park runs on the heavy worker (parkJob), which checks again
// under the PR's reservation.
func (e *Engine) parkIdle(ctx context.Context, ts tickState) {
	after := e.cfg.Daemon.ParkIdleAfter.Duration
	if after <= 0 || e.d.Agents == nil || !ts.herdrUp {
		return
	}
	live, err := e.st.LiveSessions(ctx)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "park idle agents: sessions")
		return
	}
	byPR := map[int64][]store.Session{}
	for _, s := range live {
		byPR[s.PRID] = append(byPR[s.PRID], s)
	}
	now := e.now()
	e.forgetParkFails(byPR)
	for _, prID := range slices.Sorted(maps.Keys(byPR)) {
		if ts.busyPR[prID] || e.roundActive(prID) {
			continue
		}
		pr, idleSince, ok := e.parkable(ctx, prID, byPR[prID], now, after)
		if !ok || e.parkHeld(pr, byPR[prID], now) {
			continue
		}
		if e.d.DryRun {
			e.rec.Record(ctx, e.prLabelSubject(ctx, pr), "park", fmt.Sprintf("%d agents idle since %s", len(byPR[prID]), idleSince.Local().Format("15:04")))
			continue
		}
		e.enqueueHeavy(fmt.Sprintf("park:%d", prID), e.parkJob(prID, now))
	}
}

// parkable reports whether the PR's live sessions may be parked now: the PR
// is reviewed, or waits for a round that is far off (waitsLong), no human
// typed into its panes within human_cooldown, its slot is neither pinned
// nor held for a human (whatever the PR waits for), and every session is
// idle (not working or blocked) since before now - after. It returns the PR
// and when its last session went idle.
func (e *Engine) parkable(ctx context.Context, prID int64, sessions []store.Session, now time.Time, after time.Duration) (store.PR, time.Time, bool) {
	pr, err := e.st.PRByID(ctx, prID)
	if err != nil {
		return pr, time.Time{}, false
	}
	switch pr.State {
	case store.PRReviewed:
	case store.PRQueued, store.PRRereviewPending:
		if !e.waitsLong(ctx, prID, now, after) {
			return pr, time.Time{}, false
		}
	default:
		return pr, time.Time{}, false
	}
	if now.Before(e.cfg.Daemon.CooldownUntil(pr.HumanActiveAt)) {
		return pr, time.Time{}, false
	}
	if sl, has, err := e.slotOf(ctx, prID); err != nil || (has && (sl.Pinned || sl.HoldReason != nil)) {
		return pr, time.Time{}, false
	}
	var last time.Time
	for _, s := range sessions {
		switch herdr.Status(deref(s.AgentStatus)) {
		case herdr.StatusWorking, herdr.StatusBlocked:
			return pr, time.Time{}, false
		}
		since := s.StartedAt
		for _, t := range []*time.Time{s.AgentStatusAt, s.LastPromptAt} {
			if t != nil && t.After(since) {
				since = *t
			}
		}
		if since.After(last) {
			last = since
		}
	}
	if last.IsZero() || now.Sub(last) < after {
		return pr, last, false
	}
	return pr, last, true
}

// waitsLong reports whether a PR waiting for a round (queued,
// rereview_pending) will not get one soon, by the wait the last dispatch
// recorded for it (KVPRWait, noteWaits): `magnum pause` or an
// infrastructure pause, a drain for a restart, the daily round cap, or any
// wait that ends more than after from now. A wait without an end (the next
// dispatch, a slot or capacity, a mute) is not long: it may end any tick.
func (e *Engine) waitsLong(ctx context.Context, prID int64, now time.Time, after time.Duration) bool {
	v, _ := e.getKV(ctx, KVPRWait(prID))
	w, ok := ParseWait(v)
	if !ok {
		return false
	}
	switch w.Reason {
	case WaitPaused, WaitInfra, WaitDraining, WaitCap:
		return true
	}
	return w.Until.Sub(now) > after
}

// parkJob parks the PR's sessions under its slot-work reservation, after
// checking again that they are still parkable (a round may have started, an
// agent may have woken up). Park itself refuses a busy pane. A failure
// counts as the tick's try at `at` (parkFailed); a park that works forgets
// the earlier failures and closes the panes it retired (closeRetiredPanes).
// The try after a park that failed because an agent did not quit
// (parkQuitTimeout) is the hard park (Agents.ParkHard, as a fresh start's
// second try is): ctrl+c is the cheaper way and quits most agents, but one
// that ignores it stays in its pane, and the plain park would be tried for
// days (talkable#11932, 21 failures from 2026-10-14). A hard park that
// fails is recorded like any failed park, and one that fails for another
// cause hands the next try back to the plain park.
func (e *Engine) parkJob(prID int64, at time.Time) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if !e.reserveSlotWork(prID, "its idle agents are being parked") {
			return nil
		}
		defer e.endEviction(prID)
		live, err := e.st.LiveSessions(ctx)
		if err != nil {
			return err
		}
		sessions := sessionsOf(live, prID)
		if len(sessions) == 0 {
			return nil
		}
		pr, idleSince, ok := e.parkable(ctx, prID, sessions, e.now(), e.cfg.Daemon.ParkIdleAfter.Duration)
		if !ok {
			return nil
		}
		subject := e.prLabelSubject(ctx, pr)
		park, hard := e.d.Agents.Park, e.parkQuitFailed(prID)
		if hard {
			park = e.d.Agents.ParkHard
		}
		if err := park(ctx, pr); err != nil {
			e.parkFailed(ctx, pr, subject, at, err)
			return nil
		}
		e.parkWorked(prID)
		msg := fmt.Sprintf("parked %d idle agents (idle since %s); the next round resumes them", len(sessions), idleSince.Local().Format("15:04"))
		if hard {
			msg += " (hard: an agent did not quit at the plain park)"
		}
		e.event(ctx, "info", subject, "pr.parked", msg, nil)
		e.closeRetiredPanes(ctx, pr) // retired_panes.go: the next round resumes them in new panes
		return nil
	}
}

// parkBackoffCap is the longest wait between two tries of a park that
// failed (parkBackoff).
const parkBackoffCap = 12 * time.Hour

// The classes of a failed park (parkClass): the cause pr.park_failed names,
// recorded once until it changes.
const (
	parkQuitTimeout = "quit_timeout" // an agent's pane was no idle shell QuitIdleTimeout after its ctrl+c
	parkBusy        = "busy"         // a run, an agent or a pane of the PR was at work (agents.ErrBusy)
	parkHerdr       = "herdr"        // herdr could not be reached or failed the request
	parkError       = "error"        // anything else (the registry, say)
)

// parkBackoff is a PR's failed parks since its last park that worked (idle
// parks and Codex-flag releases alike; talkable#11932 failed 1,434 parks
// in 37 hours, one each tick, each two ctrl+c into a live Claude pane): no
// try before next, wait is the last wait (twice the one before, from the
// tick interval, up to parkBackoffCap), cause the class of the last
// failure, and round and sessions what the PR had at the last failure
// (its last round's start; its live sessions, by parkFingerprint). A round
// started or a change of the live sessions since resets the wait and keeps
// the cause, so the cause is recorded again only when it changes.
type parkBackoff struct {
	cause    string
	wait     time.Duration
	next     time.Time
	round    time.Time
	sessions string
	failures int
}

// parkClass is a failed park's class (parkQuitTimeout ...) and its words
// for the event.
func parkClass(err error) (class, what string) {
	_, herdrErr := errors.AsType[*herdr.Error](err)
	switch {
	case errors.Is(err, agents.ErrBusy) && herdr.IsTimeout(err):
		return parkQuitTimeout, "an agent did not quit: its pane was no idle shell after ctrl+c"
	case errors.Is(err, agents.ErrBusy):
		return parkBusy, "a run, an agent or a pane of the PR is at work"
	case herdrErr || errors.Is(err, herdr.ErrUnavailable) || herdr.IsTimeout(err):
		return parkHerdr, "herdr failed"
	}
	return parkError, "an error"
}

// parkFingerprint is what parkBackoff compares of a PR's live sessions:
// each one's id and last prompt. Their agents' statuses are left out, since
// the park's own ctrl+c can move them.
func parkFingerprint(sessions []store.Session) string {
	keys := make([]string, 0, len(sessions))
	for _, s := range sessions {
		at := ""
		if s.LastPromptAt != nil {
			at = s.LastPromptAt.UTC().Format(time.RFC3339Nano)
		}
		keys = append(keys, fmt.Sprintf("%d@%s", s.ID, at))
	}
	slices.Sort(keys)
	return strings.Join(keys, ",")
}

// roundStarted is the start of the PR's last round (zero: none).
func roundStarted(pr store.PR) time.Time {
	if pr.LastRoundStartedAt == nil {
		return time.Time{}
	}
	return *pr.LastRoundStartedAt
}

// sessionsOf is the sessions of PR prID among live.
func sessionsOf(live []store.Session, prID int64) []store.Session {
	var out []store.Session
	for _, s := range live {
		if s.PRID == prID {
			out = append(out, s)
		}
	}
	return out
}

// parkHeld reports whether the PR's park waits out the backoff of its last
// failure at now (sessions: its live sessions). A round started or a change
// of the live sessions since that failure resets the wait: the next try
// goes ahead.
func (e *Engine) parkHeld(pr store.PR, sessions []store.Session, now time.Time) bool {
	e.parkMu.Lock()
	defer e.parkMu.Unlock()
	b, ok := e.parkFails[pr.ID]
	if !ok {
		return false
	}
	if round, fp := roundStarted(pr), parkFingerprint(sessions); !round.Equal(b.round) || fp != b.sessions {
		b.wait, b.next, b.round, b.sessions = 0, time.Time{}, round, fp
		e.parkFails[pr.ID] = b
		return false
	}
	return now.Before(b.next)
}

// parkFailed records the failed park of pr tried at `at`: the next try
// waits twice the last wait (two ticks after a first failure, up to
// parkBackoffCap), and a failure whose class differs from the last one's
// (the first one included) records pr.park_failed. Every failure is
// logged. A park the daemon's stop cut short counts no failure.
func (e *Engine) parkFailed(ctx context.Context, pr store.PR, subject string, at time.Time, err error) {
	if ctx.Err() != nil {
		e.log.Info("park agents", "subject", subject, "err", err)
		return
	}
	var sessions []store.Session // after the failure: a session the park did quit is gone
	if live, lerr := e.st.LiveSessions(ctx); lerr == nil {
		sessions = sessionsOf(live, pr.ID)
	}
	class, what := parkClass(err)
	e.parkMu.Lock()
	if e.parkFails == nil {
		e.parkFails = map[int64]parkBackoff{}
	}
	b := e.parkFails[pr.ID]
	last := b.wait
	if last == 0 {
		last = e.cfg.Daemon.PollInterval.Duration
	}
	b.wait = min(2*last, parkBackoffCap)
	b.next = at.Add(b.wait)
	b.round, b.sessions = roundStarted(pr), parkFingerprint(sessions)
	b.failures++
	changed := b.cause != class
	b.cause = class
	e.parkFails[pr.ID] = b
	e.parkMu.Unlock()
	e.log.Info("park agents", "subject", subject, "class", class, "failures", b.failures, "next_try_in", b.wait, "err", err)
	if changed {
		e.event(ctx, "warn", subject, "pr.park_failed",
			fmt.Sprintf("its agents could not be parked (%s: %s): %v; next try in %s, each failure doubling the wait up to %s",
				class, what, err, humanDuration(b.wait), humanDuration(parkBackoffCap)),
			map[string]any{"class": class, "backoff": humanDuration(b.wait), "failures": b.failures})
	}
}

// parkQuitFailed reports whether the PR's last failed park (an idle park or
// a Codex-flag release, which share the record) was an agent that did not
// quit (parkQuitTimeout): the class parkFailed keeps in parkFails until a
// park works. The record lives in the daemon's memory, so the first try
// after a restart is a plain one again.
func (e *Engine) parkQuitFailed(prID int64) bool {
	e.parkMu.Lock()
	defer e.parkMu.Unlock()
	return e.parkFails[prID].cause == parkQuitTimeout
}

// parkWorked forgets the PR's failed parks: its park worked.
func (e *Engine) parkWorked(prID int64) {
	e.parkMu.Lock()
	delete(e.parkFails, prID)
	e.parkMu.Unlock()
}

// forgetParkFails drops the failed parks of the PRs that have no live
// session left (byPR: the live sessions by PR): something else parked or
// closed them.
func (e *Engine) forgetParkFails(byPR map[int64][]store.Session) {
	e.parkMu.Lock()
	defer e.parkMu.Unlock()
	maps.DeleteFunc(e.parkFails, func(prID int64, _ parkBackoff) bool { return len(byPR[prID]) == 0 })
}

// releaseFlagged gives back what each Codex-flagged PR holds (DECISIONS "A
// Codex-flagged PR gives back its slot and its agents"): magnum never
// reviews it again, yet it kept its pool slot and three live agents (all 18
// slots were in use, two held by flagged PRs). A flagged PR with no round
// in flight, no agent at work and no human within human_cooldown, whose
// slot `magnum open` did not pin (nor holds for a human), gets
// flagReleaseJob on the heavy worker when it has live sessions or a held
// pool slot (while herdr answers: a slot is released only after its agents
// are parked). A per-PR worktree stays (it takes no pool place), and a PR
// whose slot release failed waits flagReleaseRetry. The registry's flags
// and the unwritten ones count; a read failure skips the tick.
func (e *Engine) releaseFlagged(ctx context.Context, ts tickState) {
	if e.d.Agents == nil || e.d.Slots == nil {
		return
	}
	flagged, err := e.st.CodexFlaggedPRs(ctx)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "Codex-flagged PRs")
		return
	}
	now := e.now()
	e.flagMu.Lock()
	for id := range e.unwritten {
		flagged[id] = true
	}
	for id, at := range e.releaseFailed {
		if now.Sub(at) < flagReleaseRetry {
			delete(flagged, id)
		}
	}
	e.flagMu.Unlock()
	if len(flagged) == 0 {
		return
	}
	live, err := e.st.LiveSessions(ctx)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "release flagged PRs: sessions")
		return
	}
	held, err := e.st.ListSlots(ctx, store.SlotFilter{Kind: store.SlotKindPool, States: []string{store.SlotHeld}})
	if err != nil {
		e.warnUnlessStopped(ctx, err, "release flagged PRs: slots")
		return
	}
	holds := map[int64]int{} // live sessions and held pool slots, by PR
	for _, s := range live {
		holds[s.PRID]++
	}
	for _, sl := range held {
		if sl.PRID != nil {
			holds[*sl.PRID]++
		}
	}
	for _, prID := range slices.Sorted(maps.Keys(flagged)) {
		if holds[prID] == 0 || ts.busyPR[prID] || e.roundActive(prID) || !ts.herdrUp {
			continue
		}
		if f, ok := e.codexFlag(ctx, prID); ok {
			if _, ok := e.fallbackRole(f); ok {
				continue // its rounds go on with the fallback judge: it keeps its slot and agents (judge_fallback.go)
			}
		}
		pr, _, _, ok := e.flagReleasable(ctx, prID, now)
		if !ok {
			continue
		}
		if sessions := sessionsOf(live, prID); len(sessions) > 0 && e.parkHeld(pr, sessions, now) {
			continue
		}
		if e.d.DryRun {
			e.rec.Record(ctx, e.prLabelSubject(ctx, pr), "release", "Codex flagged: park its agents and release its slot")
			continue
		}
		e.enqueueHeavy(fmt.Sprintf("flag-release:%d", prID), e.flagReleaseJob(prID, now))
	}
}

// flagReleaseRetry spaces out the releases of a flagged PR's slot after one
// failed (its guard refused, a step failed), as provisioning does.
const flagReleaseRetry = provisionRetry

// flagReleasable reads the flagged PR and its slot (has: it holds one) and
// reports whether releaseFlagged may touch them now: not pinned, its slot
// neither pinned nor held for a human, no human within human_cooldown.
func (e *Engine) flagReleasable(ctx context.Context, prID int64, now time.Time) (pr store.PR, sl store.Slot, has, ok bool) {
	pr, err := e.st.PRByID(ctx, prID)
	if err != nil || pr.Pinned {
		return pr, sl, false, false
	}
	if now.Before(e.cfg.Daemon.CooldownUntil(pr.HumanActiveAt)) {
		return pr, sl, false, false
	}
	sl, has, err = e.slotOf(ctx, prID)
	if err != nil || (has && (sl.Pinned || sl.HoldReason != nil)) {
		return pr, sl, has, false
	}
	return pr, sl, has, true
}

// slotReleasable: a pool slot a flagged PR holds idle (held), which
// releaseFlagged hands back to the pool.
func slotReleasable(sl store.Slot) bool {
	return sl.Kind == store.SlotKindPool && sl.State == store.SlotHeld
}

// flagReleaseJob parks the flagged PR's live sessions, closes the panes the
// park retired (closeRetiredPanes) and then releases its held pool slot,
// under its slot-work reservation, after checking again that it is still
// flagged and releasable. Park refuses a busy pane, and a failed park keeps
// the slot (the agents work in it): it is tried again once the park's
// backoff has passed (parkFailed), as an idle park is.
func (e *Engine) flagReleaseJob(prID int64, at time.Time) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if !e.reserveSlotWork(prID, "it is Codex-flagged: its agents are parked and its slot released") {
			return nil
		}
		defer e.endEviction(prID)
		f, flagged := e.codexFlag(ctx, prID)
		if !flagged {
			return nil
		}
		if _, ok := e.fallbackRole(f); ok {
			return nil // its rounds go on with the fallback judge (judge_fallback.go)
		}
		pr, sl, has, ok := e.flagReleasable(ctx, prID, e.now())
		if !ok {
			return nil
		}
		subject := e.prLabelSubject(ctx, pr)
		live, err := e.st.LiveSessions(ctx)
		if err != nil {
			return err
		}
		n := 0
		for _, s := range live {
			if s.PRID == prID {
				n++
			}
		}
		var did []string
		if n > 0 {
			if err := e.d.Agents.Park(ctx, pr); err != nil {
				e.parkFailed(ctx, pr, subject, at, err)
				return nil
			}
			e.parkWorked(prID)
			e.closeRetiredPanes(ctx, pr)
			did = append(did, fmt.Sprintf("parked its %d agents", n))
		}
		if has && slotReleasable(sl) {
			repo, err := e.st.RepoByID(ctx, pr.RepoID)
			if err != nil {
				return err
			}
			if pool := e.cfg.PoolFor(repo.FullName()); pool != nil {
				if err := e.d.Slots.Release(ctx, sl, *pool, "codex-flagged"); err != nil {
					e.flagMu.Lock()
					if e.releaseFailed == nil {
						e.releaseFailed = map[int64]time.Time{}
					}
					e.releaseFailed[prID] = e.now()
					e.flagMu.Unlock()
					e.event(ctx, "warn", "slot:"+sl.Name, "slot.release_failed", err.Error(), nil)
					return err
				}
				did = append(did, "released "+sl.Name)
			}
		}
		if len(did) > 0 {
			e.event(ctx, "info", subject, "pr.codex_flag_released", "Codex-flagged, never reviewed again: "+strings.Join(did, " and "),
				map[string]any{"agents": n, "slot": sl.Name})
		}
		return nil
	}
}

// prLabelSubject is the PR's audit subject ("pr:owner/name#N"), or
// "pr:<id>" when its repository cannot be read.
func (e *Engine) prLabelSubject(ctx context.Context, pr store.PR) string {
	if repo, err := e.st.RepoByID(ctx, pr.RepoID); err == nil {
		return prSubject(repo, pr.Number)
	}
	return fmt.Sprintf("pr:%d", pr.ID)
}
