package engine

// Parking idle agents: a reviewed PR's agents sit idle until its next push,
// often for hours (on 2026-10-19 47 idle agents of PRs no round used held
// 9.2 GB on a machine that swapped). A live agent is worth keeping only
// while its warmth pays: a Claude role resumes in seconds, so its session
// parks [daemon] park_idle_after (15m) after the PR's last round, fix job
// or whiteboard job ended (kvPRWorkEnded) or its own last activity,
// whichever is later; a judge keeps [pipeline] judge_fresh_after (90m)
// when that is longer, within which the next round reuses a live judge
// instead of a Codex launch, after which the cold-judge rule would start
// it fresh anyway (judge_fresh.go). While some session of the PR is not
// past its threshold, the sessions past theirs that have a conversation to
// resume park alone (an early park, Agents.ParkSessions); once every live
// session is past its own, the PR's park takes them all with the workspace,
// as before. A reviewed PR and an ineligible one (a draft, a mute or a skip
// after its review) are parked; a PR that waits for its round behind a
// pause, a drain, the daily cap or a wait ending more than park_idle_after
// away is parked the same way (under `magnum pause`, two re-reviews held 8
// agents idle for up to 2 hours). A park that fails is tried again only
// after twice the last wait, and its cause is recorded once (parkBackoff);
// after one that failed because an agent did not quit, the PR's park tries
// the hard mode.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// kvPRWorkEnded is when the PR's last round, fix job or whiteboard job
// ended (store.FormatTime), written before its reservation drops
// (workEnded): the idle park counts a session's idle time from there at
// the earliest (idleClock), so a reviewer idle while the judge works is
// not idle until the round ends, and the fix cycle or the whiteboard that
// starts right after a round never meets a park in between.
func kvPRWorkEnded(prID int64) string { return fmt.Sprintf("pr.%d.work_ended", prID) }

// workEnded records that a round, a fix job or a whiteboard job of the PR
// ends now (kvPRWorkEnded, and its copy in workEndedAt that
// reserveParkWork reads). It runs before the job drops its reservation,
// whose context ctx may be, ended or not: the record is written either way.
func (e *Engine) workEnded(ctx context.Context, prID int64) {
	now := e.now()
	e.mu.Lock()
	e.workEndedAt[prID] = now
	e.mu.Unlock()
	e.setKV(context.WithoutCancel(ctx), kvPRWorkEnded(prID), store.FormatTime(now))
}

// reserveParkWork is reserveSlotWork for the idle park planned at `at`,
// refused as well when a round, fix job or whiteboard job of the PR ended
// after `at` (workEndedAt): that plan predates the work, and a park job that
// took the PR in the gap between a round's end and the fix cycle or the
// whiteboard that follows it would refuse their start ("its idle agents are
// being parked"; the fix loop then ends needing attention). The check and
// the reservation share e.mu, so no work can end between them.
func (e *Engine) reserveParkWork(prID int64, at time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.rounds[prID]; ok || e.evicting[prID] != "" || e.workEndedAt[prID].After(at) {
		return false
	}
	e.evicting[prID] = "its idle agents are being parked"
	return true
}

// parkIdle queues a park for every PR whose idle agents the idle park may
// take now (planPark; park_idle_after 0 = never) and whose last failed
// park, if any, has waited out its backoff (parkHeld). It runs right after
// noteWaits recorded the waits. The park runs on the heavy worker
// (parkJob), which plans again under the PR's reservation.
func (e *Engine) parkIdle(ctx context.Context, ts tickState) {
	if e.cfg.Daemon.ParkIdleAfter.Duration <= 0 || e.d.Agents == nil || !ts.herdrUp {
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
		plan, ok := e.planPark(ctx, prID, byPR[prID], now)
		if !ok || e.parkHeld(plan.pr, byPR[prID], now, plan.whole) {
			continue
		}
		if e.d.DryRun {
			e.rec.Record(ctx, e.prLabelSubject(ctx, plan.pr), "park", e.parkPlanned(plan))
			continue
		}
		e.enqueueHeavy(fmt.Sprintf("park:%d", prID), e.parkJob(prID, now))
	}
}

// parkPlan is what the idle park takes of a PR's live sessions now
// (planPark): with whole, take is all of them, which the PR's park takes
// with its workspace (Agents.Park); else take is the sessions past their
// threshold that have a conversation to resume, which an early park takes
// alone (Agents.ParkSessions), and the others stay live: wait those not
// past their threshold yet, keep those past it without a conversation to
// resume. idle is when the last session the plan takes went idle
// (idleClock).
type parkPlan struct {
	pr    store.PR
	whole bool
	take  []store.Session
	wait  []store.Session
	keep  []store.Session
	idle  time.Time
}

// planPark is what the idle park may take of the PR's live sessions at now,
// the one rule parkIdle and parkJob share; ok is false when it takes
// nothing. The PR must be parkable (parkablePR) and no session's agent at
// work or blocked. A session is past its threshold when it has idled its
// role's parkAfter since its idleClock (its own last activity, or the end
// of the PR's last round, fix job or whiteboard job if later). Every live
// session past its own: the whole PR. Else the sessions past theirs that
// have a conversation the next round resumes (resumableSession) are parked
// early; any other waits for the whole park, and none resumable takes
// nothing.
func (e *Engine) planPark(ctx context.Context, prID int64, sessions []store.Session, now time.Time) (parkPlan, bool) {
	after := e.cfg.Daemon.ParkIdleAfter.Duration
	if after <= 0 || len(sessions) == 0 {
		return parkPlan{}, false
	}
	pr, ok := e.parkablePR(ctx, prID, now, after)
	plan := parkPlan{pr: pr}
	if !ok {
		return plan, false
	}
	ended := e.lastWorkEnd(ctx, pr)
	var past []store.Session
	for _, s := range sessions {
		switch herdr.Status(deref(s.AgentStatus)) {
		case herdr.StatusWorking, herdr.StatusBlocked:
			return plan, false
		}
		if now.Sub(idleClock(s, ended)) < e.parkAfter(s.Role) {
			plan.wait = append(plan.wait, s)
		} else {
			past = append(past, s)
		}
	}
	plan.whole = len(plan.wait) == 0
	for _, s := range past {
		if !plan.whole && !e.resumableSession(s) {
			plan.keep = append(plan.keep, s)
			continue
		}
		plan.take = append(plan.take, s)
		if since := idleClock(s, ended); since.After(plan.idle) {
			plan.idle = since
		}
	}
	return plan, len(plan.take) > 0
}

// lastWorkEnd is when the PR's last round, fix job or whiteboard job ended
// (kvPRWorkEnded). A PR with no record yet (none of its jobs ended since
// the daemon wrote it first) falls back to the latest end of a run of its
// last round, or that round's start: its reviewers, idle since their own
// turns, then do not park from a status change earlier than the round's
// end. Zero when nothing is known.
func (e *Engine) lastWorkEnd(ctx context.Context, pr store.PR) time.Time {
	if ended, ok := e.kvTime(ctx, kvPRWorkEnded(pr.ID)); ok {
		return ended
	}
	ended := roundStarted(pr)
	runs, err := e.st.LatestRoundRuns(ctx, pr.ID)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "idle park: the last round's runs", "pr", pr.ID)
	}
	for _, r := range runs[pr.ID] {
		if r.EndedAt != nil && r.EndedAt.After(ended) {
			ended = *r.EndedAt
		}
	}
	return ended
}

// idleClock is when session s went idle for the idle park: the latest of
// its start, its agent's last status change and its last prompt (its own
// activity), and ended, the end of the PR's last round, fix job or
// whiteboard job (zero: none recorded).
func idleClock(s store.Session, ended time.Time) time.Time {
	since := s.StartedAt
	for _, t := range []*time.Time{s.AgentStatusAt, s.LastPromptAt, &ended} {
		if t != nil && t.After(since) {
			since = *t
		}
	}
	return since
}

// parkAfter is how long a session of role idles before the idle park takes
// it (parkThreshold).
func (e *Engine) parkAfter(role string) time.Duration {
	_, after := e.parkThreshold(role)
	return after
}

// parkThreshold is the key and the time a session of role idles before the
// idle park takes it: [daemon] park_idle_after, or for a judge role
// [pipeline] judge_fresh_after when that is longer (a live judge is reused
// up to it, and the cold-judge rule starts the judge fresh after it; "0",
// always resume, leaves the judge park_idle_after).
func (e *Engine) parkThreshold(role string) (key string, after time.Duration) {
	after = e.cfg.Daemon.ParkIdleAfter.Duration
	if fresh := e.cfg.Pipeline.JudgeFreshAfter.Duration; fresh > after && e.judgeRole(role) {
		return "judge_fresh_after", fresh
	}
	return "park_idle_after", after
}

// judgeRole reports whether role names a judge role of the config.
func (e *Engine) judgeRole(role string) bool {
	r, ok := e.cfg.RoleByNameOrAlias(nil, role)
	return ok && r.Judge
}

// shellRole reports whether role names a shell role of the config (a
// pane, no agent).
func (e *Engine) shellRole(role string) bool {
	r, ok := e.cfg.RoleByNameOrAlias(nil, role)
	return ok && r.IsShell()
}

// resumableSession reports whether session s has a conversation the next
// round resumes, as the agents' retire decides (agents.Manager.ParkSessions
// refuses any other): a session id recorded, a role that runs an agent,
// and a kind that reports session ids (session_source "none" always starts
// fresh, though herdr may still show an id).
func (e *Engine) resumableSession(s store.Session) bool {
	if deref(s.SessionID) == "" || e.shellRole(s.Role) {
		return false
	}
	kind := deref(s.AgentKind)
	if r, ok := e.cfg.RoleByNameOrAlias(nil, s.Role); kind == "" && ok {
		kind = r.Kind
	}
	k, ok := e.cfg.KindSpec(kind)
	return !ok || k.SessionSource != config.SessionNone
}

// parkablePR reports whether the idle park may take the PR's agents at
// now: the PR is reviewed or ineligible (a draft, a mute or a skip after
// its review; its next round resumes them), or waits for a round that is
// far off (waitsLong), no human typed into its panes within
// human_cooldown, and its slot is neither pinned nor held for a human
// (whatever the PR waits for). A PR that needs attention keeps its agents:
// its remedies send the operator to read the pane. after is
// park_idle_after.
func (e *Engine) parkablePR(ctx context.Context, prID int64, now time.Time, after time.Duration) (store.PR, bool) {
	pr, err := e.st.PRByID(ctx, prID)
	if err != nil {
		return pr, false
	}
	switch pr.State {
	case store.PRReviewed, store.PRIneligible:
	case store.PRQueued, store.PRRereviewPending:
		if !e.waitsLong(ctx, prID, now, after) {
			return pr, false
		}
	default:
		return pr, false
	}
	if now.Before(e.cfg.Daemon.CooldownUntil(pr.HumanActiveAt)) {
		return pr, false
	}
	if sl, has, err := e.slotOf(ctx, prID); err != nil || (has && (sl.Pinned || sl.HoldReason != nil)) {
		return pr, false
	}
	return pr, true
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

// parkJob parks what planPark gives for the PR under its slot-work
// reservation, planned again there (a round may have started, an agent may
// have woken up, the plan may have grown to the whole PR): an early plan
// goes to parkEarly, a whole one to the PR's park. A job planned at `at`
// never takes the PR once a round, fix job or whiteboard job of the PR
// ended after `at` (reserveParkWork): it would hold the PR in the gap
// before the fix cycle or the whiteboard that follows. Park itself refuses a
// busy pane. A failure counts as the tick's try at `at` (parkFailed); a
// park that works forgets the earlier failures and closes the panes it
// retired (closeRetiredPanes). The PR's park after a park that failed
// because an agent did not quit (parkQuitTimeout) is the hard park
// (Agents.ParkHard, as a fresh start's second try is): ctrl+c is the
// cheaper way and quits most agents, but one that ignores it stays in its
// pane, and the plain park would be tried for days (talkable#11932, 21
// failures from 2026-10-14). A hard park that fails is recorded like any
// failed park, and one that fails for another cause hands the next try
// back to the plain park.
func (e *Engine) parkJob(prID int64, at time.Time) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if !e.reserveParkWork(prID, at) {
			return nil
		}
		defer e.endEviction(prID)
		live, err := e.st.LiveSessions(ctx)
		if err != nil {
			return err
		}
		plan, ok := e.planPark(ctx, prID, sessionsOf(live, prID), e.now())
		if !ok {
			return nil
		}
		if !plan.whole {
			e.parkEarly(ctx, plan, at)
			return nil
		}
		pr := plan.pr
		subject := e.prLabelSubject(ctx, pr)
		park, hard := e.d.Agents.Park, e.parkQuitFailed(prID)
		if hard {
			park = e.d.Agents.ParkHard
		}
		if err := park(ctx, pr); err != nil {
			e.parkFailed(ctx, pr, subject, at, err, false)
			return nil
		}
		e.parkWorked(prID)
		msg := fmt.Sprintf("parked %d idle agents (idle since %s); the next round resumes them", len(plan.take), plan.idle.Local().Format("15:04"))
		if hard {
			msg += " (hard: an agent did not quit at the plain park)"
		}
		e.event(ctx, "info", subject, "pr.parked", msg, nil)
		e.closeRetiredPanes(ctx, pr) // retired_panes.go: the next round resumes them in new panes
		return nil
	}
}

// parkEarly parks the sessions of an early plan alone
// (Agents.ParkSessions: never the hard mode, whatever the last failure
// was, since an agent that does not quit waits for the PR's park; it
// closes each session's pane before it parks the session, and records each
// session's begin, ok and fail). The sessions it parked, even beside one
// that failed, get pr.parked with their roles and "partial": true, and a
// pane it had to leave is tried again (closeRetiredPanes); a failure counts
// as the tick's try at `at` (parkFailed), and a park without one forgets
// the earlier failures.
func (e *Engine) parkEarly(ctx context.Context, plan parkPlan, at time.Time) {
	subject := e.prLabelSubject(ctx, plan.pr)
	ids := make([]int64, len(plan.take))
	for i, s := range plan.take {
		ids[i] = s.ID
	}
	parked, err := e.d.Agents.ParkSessions(ctx, plan.pr, ids)
	if len(parked) > 0 {
		if err == nil {
			e.parkWorked(plan.pr.ID)
		}
		roles := sessionRoles(parked)
		msg := fmt.Sprintf("parked %s, idle since %s", e.parkedThresholds(parked), plan.idle.Local().Format("15:04"))
		if stays := e.parkStays(plan); len(stays) > 0 {
			msg += "; " + strings.Join(stays, "; ")
		}
		e.event(ctx, "info", subject, "pr.parked", msg+"; the next round resumes them", map[string]any{"roles": roles, "partial": true})
		e.closeRetiredPanes(ctx, plan.pr) // retired_panes.go: a pane the early park had to leave, the workspace stays
	}
	if err != nil {
		e.parkFailed(ctx, plan.pr, subject, at, err, true)
	}
}

// parkedThresholds names the roles of sessions, each group with the
// threshold its sessions idled past (parkThreshold): "claude-review,
// claude-simplify (park_idle_after 15m)", " and codex-judge
// (judge_fresh_after 1h30m)" after it when a judge is among them.
func (e *Engine) parkedThresholds(sessions []store.Session) string {
	var labels []string
	roles := map[string][]string{}
	for _, s := range sessions {
		key, after := e.parkThreshold(s.Role)
		label := key + " " + humanDuration(after)
		if _, ok := roles[label]; !ok {
			labels = append(labels, label)
		}
		roles[label] = append(roles[label], s.Role)
	}
	groups := make([]string, len(labels))
	for i, label := range labels {
		groups[i] = fmt.Sprintf("%s (%s)", strings.Join(roles[label], ", "), label)
	}
	return strings.Join(groups, " and ")
}

// parkPlanned is what a dry run records of plan: how many agents the PR's
// park takes and since when they idle, or the roles an early park takes
// and the agents that stay.
func (e *Engine) parkPlanned(plan parkPlan) string {
	since := plan.idle.Local().Format("15:04")
	if plan.whole {
		return fmt.Sprintf("%d agents idle since %s", len(plan.take), since)
	}
	msg := fmt.Sprintf("%s idle since %s", strings.Join(sessionRoles(plan.take), ", "), since)
	if stays := e.parkStays(plan); len(stays) > 0 {
		msg += " (" + strings.Join(stays, "; ") + ")"
	}
	return msg
}

// parkStays says why each agent session an early plan leaves live stays:
// one not past its threshold until that threshold (parkThreshold: a judge
// until judge_fresh_after when that is longer), and one past it without a
// conversation to resume until the PR's park. A shell role's pane is left
// out: it runs no agent.
func (e *Engine) parkStays(plan parkPlan) []string {
	var out []string
	for _, s := range plan.wait {
		if !e.shellRole(s.Role) {
			key, after := e.parkThreshold(s.Role)
			out = append(out, fmt.Sprintf("%s stays until %s (%s)", s.Role, key, humanDuration(after)))
		}
	}
	for _, s := range plan.keep {
		if !e.shellRole(s.Role) {
			out = append(out, s.Role+" stays until the PR's park (no conversation to resume)")
		}
	}
	return out
}

// sessionRoles are the roles of sessions, in order.
func sessionRoles(sessions []store.Session) []string {
	out := make([]string, len(sessions))
	for i, s := range sessions {
		out[i] = s.Role
	}
	return out
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
// the cause, so the cause is recorded again only when it changes. early is
// whether the last failure was an early park's (parkEarly): the PR's park,
// once every session is past its threshold, resets the wait too, so a
// reviewer that will not quit does not hold the judge's park and its hard
// mode past judge_fresh_after.
type parkBackoff struct {
	cause    string
	wait     time.Duration
	next     time.Time
	round    time.Time
	sessions string
	failures int
	early    bool
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
// failure at now (sessions: its live sessions; whole: the park would take
// them all, the PR's park or a release). A round started or a change of the
// live sessions since that failure resets the wait, as does a whole park
// after an early park's failure: the next try goes ahead.
func (e *Engine) parkHeld(pr store.PR, sessions []store.Session, now time.Time, whole bool) bool {
	e.parkMu.Lock()
	defer e.parkMu.Unlock()
	b, ok := e.parkFails[pr.ID]
	if !ok {
		return false
	}
	if round, fp := roundStarted(pr), parkFingerprint(sessions); !round.Equal(b.round) || fp != b.sessions || (b.early && whole) {
		b.wait, b.next, b.round, b.sessions, b.early = 0, time.Time{}, round, fp, false
		e.parkFails[pr.ID] = b
		return false
	}
	return now.Before(b.next)
}

// parkFailed records the failed park of pr tried at `at` (early: an early
// park's, parkEarly): the next try waits twice the last wait (two ticks
// after a first failure, up to parkBackoffCap), and a failure whose class
// differs from the last one's (the first one included) records
// pr.park_failed. Every failure is logged. A park the daemon's stop cut
// short counts no failure.
func (e *Engine) parkFailed(ctx context.Context, pr store.PR, subject string, at time.Time, err error, early bool) {
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
	b.round, b.sessions, b.early = roundStarted(pr), parkFingerprint(sessions), early
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
		if sessions := sessionsOf(live, prID); len(sessions) > 0 && e.parkHeld(pr, sessions, now, true) {
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
				e.parkFailed(ctx, pr, subject, at, err, false)
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
