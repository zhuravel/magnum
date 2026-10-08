package engine

// Parking idle agents: a reviewed PR's agents sit idle until its next push,
// often for hours (21 processes, about 2.8 GB, idle 10.8 hours on average).
// Once every live agent of a reviewed PR has been idle for [daemon]
// park_idle_after the PR's sessions are parked; the next round resumes them
// as after any park. A PR that waits for its round behind a pause, a drain,
// the daily cap or a wait ending more than park_idle_after away is parked
// the same way (under `magnum pause`, two re-reviews held 8 agents idle for
// up to 2 hours).

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// parkIdle queues a park for every reviewed PR, and every PR whose round is
// far off (waitsLong), whose live agents have all been idle longer than
// park_idle_after (0 = never). It runs right after noteWaits recorded the
// waits. The park runs on the heavy worker (parkJob), which checks again
// under the PR's reservation.
func (e *Engine) parkIdle(ctx context.Context, ts tickState) {
	after := e.cfg.Daemon.ParkIdleAfter.Duration
	if after <= 0 || e.d.Agents == nil || !ts.herdrUp {
		return
	}
	live, err := e.st.LiveSessions(ctx)
	if err != nil {
		e.log.Warn("park idle agents: sessions", "err", err)
		return
	}
	byPR := map[int64][]store.Session{}
	for _, s := range live {
		byPR[s.PRID] = append(byPR[s.PRID], s)
	}
	now := e.now()
	for _, prID := range slices.Sorted(maps.Keys(byPR)) {
		if ts.busyPR[prID] || e.roundActive(prID) {
			continue
		}
		pr, idleSince, ok := e.parkable(ctx, prID, byPR[prID], now, after)
		if !ok {
			continue
		}
		if e.d.DryRun {
			e.rec.Record(ctx, e.prLabelSubject(ctx, pr), "park", fmt.Sprintf("%d agents idle since %s", len(byPR[prID]), idleSince.Local().Format("15:04")))
			continue
		}
		e.enqueueHeavy(fmt.Sprintf("park:%d", prID), e.parkJob(prID))
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
// agent may have woken up). Park itself refuses a busy pane.
func (e *Engine) parkJob(prID int64) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if !e.reserveSlotWork(prID, "its idle agents are being parked") {
			return nil
		}
		defer e.endEviction(prID)
		live, err := e.st.LiveSessions(ctx)
		if err != nil {
			return err
		}
		var sessions []store.Session
		for _, s := range live {
			if s.PRID == prID {
				sessions = append(sessions, s)
			}
		}
		if len(sessions) == 0 {
			return nil
		}
		pr, idleSince, ok := e.parkable(ctx, prID, sessions, e.now(), e.cfg.Daemon.ParkIdleAfter.Duration)
		if !ok {
			return nil
		}
		subject := e.prLabelSubject(ctx, pr)
		if err := e.d.Agents.Park(ctx, pr); err != nil {
			e.log.Info("park idle agents", "subject", subject, "err", err)
			return nil
		}
		e.event(ctx, "info", subject, "pr.parked",
			fmt.Sprintf("parked %d idle agents (idle since %s); the next round resumes them", len(sessions), idleSince.Local().Format("15:04")), nil)
		return nil
	}
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
		pr, _, _, ok := e.flagReleasable(ctx, prID, now)
		if !ok {
			continue
		}
		if e.d.DryRun {
			e.rec.Record(ctx, e.prLabelSubject(ctx, pr), "release", "Codex flagged: park its agents and release its slot")
			continue
		}
		e.enqueueHeavy(fmt.Sprintf("flag-release:%d", prID), e.flagReleaseJob(prID))
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

// flagReleaseJob parks the flagged PR's live sessions and then releases its
// held pool slot, under its slot-work reservation, after checking again
// that it is still flagged and releasable. Park refuses a busy pane, and a
// failed park keeps the slot (the agents work in it): the next tick tries
// again.
func (e *Engine) flagReleaseJob(prID int64) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if !e.reserveSlotWork(prID, "it is Codex-flagged: its agents are parked and its slot released") {
			return nil
		}
		defer e.endEviction(prID)
		if _, flagged := e.codexFlag(ctx, prID); !flagged {
			return nil
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
				e.log.Info("park a flagged PR's agents", "subject", subject, "err", err)
				return nil
			}
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
