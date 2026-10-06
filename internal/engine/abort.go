package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// Request kinds of `magnum abort` and `magnum ignore` (TargetPayload with a
// PR): see requestAbort.
const (
	ReqAbort  = "abort"
	ReqIgnore = "ignore"
)

// SkipIgnored is the skip_reason of a PR `magnum ignore` muted: the board
// shows such a PR as ignored.
const SkipIgnored = skipIgnored

// abortedBy is the error an aborted round's runs carry.
const abortedBy = "aborted by user"

const (
	// abortQuietWait bounds the wait for the PR's agents to stop working
	// after they were interrupted (Park refuses while one works).
	abortQuietWait = 20 * time.Second
	// abortParkTries is how often a stop tries Park while it reports busy.
	abortParkTries = 3
	// abortTimeout bounds a stop that runs after its round's context ended.
	abortTimeout = 3 * time.Minute
)

// keySender is the herdr capability a stop interrupts agents with
// (*herdr.Client has it; a Herdr port without it skips the interrupt, and
// Park then waits for the agents to finish on their own).
type keySender interface {
	AgentSendKeys(ctx context.Context, target string, keys ...string) error
	PaneSendKeys(ctx context.Context, paneID string, keys ...string) error
}

var _ keySender = (*herdr.Client)(nil)

// roundStop is the abort state of a round handle (roundHandle.stop),
// guarded by e.mu.
type roundStop struct {
	ended bool        // the round goroutine is past its round: too late to stop it
	reqs  []stopOrder // abort and ignore requests the round serves when it ends
}

// stopOrder is one abort (ignore=false) or ignore request.
type stopOrder struct {
	id     int64
	ignore bool
}

// requestAbort handles `magnum abort` (ignore=false) and `magnum ignore`.
//
// abort kills the PR's running review round: its context is cancelled and,
// once the round goroutine is back (still holding the PR's reservation, so
// nothing else touches the slot), stopPR interrupts the agents, abandons the
// runs, parks the sessions, hands a pool slot back (a per-PR worktree is
// kept) and puts the PR in reviewed (it has a reviewed head) or baseline. A
// paused round is stopped the same way. A review that waits in line (queued
// or rereview_pending: forced by `magnum review`, or automatic) is taken back
// before it starts (dropQueued). A PR with no review running or waiting is an
// error.
//
// ignore does the same when a round runs, and also mutes the PR with
// skip_reason "ignored" (state ineligible), so the daemon never queues it
// again until `magnum unmute`; without a round it stops the PR under a
// reservation of its own (like `magnum open`).
func (e *Engine) requestAbort(ctx context.Context, id int64, p TargetPayload, ignore bool) {
	repo, pr, err := e.resolve(ctx, p.PRTarget)
	if err != nil {
		e.complete(ctx, id, err, "")
		return
	}
	label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	verb := map[bool]string{false: "abort", true: "ignore"}[ignore]

	e.mu.Lock()
	h, held := e.rounds[pr.ID]
	switch {
	case held && !h.open && !h.stop.ended:
		h.stop.reqs = append(h.stop.reqs, stopOrder{id: id, ignore: ignore})
		e.inflight[id] = true
		cancel := h.cancel
		e.mu.Unlock()
		cancel()
		e.event(ctx, "info", prSubject(repo, pr.Number), "pr.abort_requested", fmt.Sprintf("magnum %s: stopping the running review", verb), nil)
		return
	case held:
		why := "its round is ending"
		if h.open {
			why = "a `magnum open` restore is in progress"
		}
		e.mu.Unlock()
		e.complete(ctx, id, fmt.Errorf("%s: %s; run `magnum %s %s` again shortly", label, why, verb, label), "")
		return
	}
	e.mu.Unlock()

	// A round's state without its round, or a round paused mid-way: stopped
	// as a running one is. A review waiting in line is taken back.
	stale := slices.Contains(store.InFlightStates, pr.State) || pr.State == store.PRPaused
	queued := pr.State == store.PRQueued || pr.State == store.PRRereviewPending
	switch {
	case !ignore && !stale && !queued:
		e.complete(ctx, id, fmt.Errorf("no review of %s is running or queued (state %s)", label, pr.State), "")
		return
	case e.d.DryRun:
		e.rec.Record(ctx, prSubject(repo, pr.Number), verb, "stop the PR's agents, park its sessions and hand back its slot")
		e.complete(ctx, id, nil, fmt.Sprintf("dry run: would %s %s", verb, label))
		return
	}
	rctx, cancel := context.WithCancel(ctx)
	if !e.reserve(pr.ID, &roundHandle{cancel: cancel}) {
		cancel()
		e.complete(ctx, id, fmt.Errorf("%s: %s; run `magnum %s %s` again shortly", label, e.heldReason(pr.ID), verb, label), "")
		return
	}
	if queued && !ignore {
		res, err := e.dropQueued(ctx, repo, pr)
		e.unreserve(pr.ID)
		cancel()
		e.complete(ctx, id, err, res)
		return
	}
	e.mu.Lock()
	e.inflight[id] = true
	e.mu.Unlock()
	e.roundWG.Add(1)
	go func() {
		defer e.roundWG.Done()
		defer e.unreserve(pr.ID)
		e.serveStop(rctx, pr.ID, []stopOrder{{id: id, ignore: ignore}}, false)
	}()
}

// dropQueued takes back a review that waits in line, before any of it ran:
// the PR leaves the line for reviewed or baseline (a merged PR's post-merge
// review: closed, released after a fresh close grace), its forced mark goes
// and so does what the request asked for this round (fresh sessions, a dry
// run, on-request roles), while its agents, sessions and slot are left as they
// are: nothing started, and a person may be working in its panes. A --as
// identity switch stays, as `magnum review --as` documents it for good. The
// caller holds the PR's reservation, so no round claims it meanwhile.
func (e *Engine) dropQueued(ctx context.Context, repo store.Repo, pr store.PR) (string, error) {
	label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	to, err := e.settleStopped(ctx, pr, false)
	if err != nil {
		return "", err
	}
	if to == "" {
		return "", fmt.Errorf("%s moved on meanwhile; `magnum status %s` says where it is", label, label)
	}
	e.delKV(ctx, kvPRDryRun(pr.ID), kvPRFresh(pr.ID), kvPRRedecide(pr.ID), kvPRGate(pr.ID), KVPRWait(pr.ID))
	e.clearRequested(ctx, pr.ID)
	what := "queued review"
	if pr.Forced {
		what = "forced review"
	}
	res := fmt.Sprintf("took back the %s of %s before it started: PR %s", what, label, to)
	e.event(ctx, "info", prSubject(repo, pr.Number), "pr.aborted", res, map[string]any{"queued": true, "forced": pr.Forced, "state": to})
	return res, nil
}

// afterRound runs at the end of a round goroutine, before it drops the PR's
// reservation: when abort or ignore requests arrived meanwhile, the PR is
// stopped (stopPR) and the requests are completed.
func (e *Engine) afterRound(ctx context.Context, prID int64) {
	e.mu.Lock()
	var orders []stopOrder
	if h := e.rounds[prID]; h != nil {
		h.stop.ended = true
		orders = h.stop.reqs
	}
	e.mu.Unlock()
	if len(orders) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortTimeout)
	defer cancel()
	e.serveStop(ctx, prID, orders, true)
}

// serveStop stops the PR for orders and completes them with the outcome.
func (e *Engine) serveStop(ctx context.Context, prID int64, orders []stopOrder, roundRan bool) {
	ignore := slices.ContainsFunc(orders, func(o stopOrder) bool { return o.ignore })
	res, err := e.stopPR(ctx, prID, ignore, roundRan)
	if ctx.Err() != nil && !roundRan {
		// Daemon shutdown: the requests stay pending for the next start.
		e.mu.Lock()
		for _, o := range orders {
			delete(e.inflight, o.id)
		}
		e.mu.Unlock()
		return
	}
	fctx := context.WithoutCancel(ctx)
	for _, o := range orders {
		e.complete(fctx, o.id, err, res)
		e.mu.Lock()
		delete(e.inflight, o.id)
		e.mu.Unlock()
	}
}

// stopPR ends whatever the PR's agents do and hands its resources back:
// working agents are interrupted (ctrl+c twice) and shell role panes get
// ctrl+c, active runs are abandoned ("aborted by user"), the sessions are
// parked (conversations stay resumable), the slot goes back to held and a
// pool slot is released (a per-PR worktree is kept; a pinned slot stays),
// and the PR moves to reviewed or baseline (ignore: ineligible, muted, with
// skip_reason "ignored"). The caller holds the PR's reservation. The result
// says what happened; the error is set when a step left something behind
// (the steps after it still ran).
func (e *Engine) stopPR(ctx context.Context, prID int64, ignore, roundRan bool) (string, error) {
	pr, err := e.st.PRByID(ctx, prID)
	if err != nil {
		return "", err
	}
	repo, err := e.st.RepoByID(ctx, pr.RepoID)
	if err != nil {
		return "", err
	}
	w := e.cfg.WatchFor(repo.FullName())
	label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	subject := prSubject(repo, pr.Number)

	if !roundRan && slices.Contains(closingStates, pr.State) {
		// Closed: cleanup owns its slot and sessions; ignore only mutes it.
		if _, err := e.settleStopped(ctx, pr, ignore); err != nil {
			return "", err
		}
		res := fmt.Sprintf("ignored %s (%s: muted only, cleanup releases it)", label, pr.State)
		e.event(ctx, "info", subject, "pr.ignored", res, nil)
		return res, nil
	}

	var done []string
	var problems []error
	if roundRan {
		done = append(done, "round stopped")
	}
	if n := e.interruptPR(ctx, pr, w); n > 0 {
		done = append(done, fmt.Sprintf("%d %s interrupted", n, textx.Plural(n, "agent", "agents")))
		if !e.waitAgentsQuiet(ctx, pr.ID) {
			e.log.Info("agents still working after the interrupt", "subject", subject)
		}
	}
	if n := e.abandonRuns(ctx, pr.ID); n > 0 {
		done = append(done, fmt.Sprintf("%d %s abandoned", n, textx.Plural(n, "run", "runs")))
	}
	parked := true
	if err := e.parkForStop(ctx, pr); err != nil {
		parked = false
		problems = append(problems, fmt.Errorf("sessions not parked: %w", err))
	} else if e.d.Agents != nil {
		done = append(done, "sessions parked")
	}

	to, err := e.settleStopped(ctx, pr, ignore)
	switch {
	case err != nil:
		problems = append(problems, err)
	case to != "":
		done = append(done, "PR "+to)
	}
	e.delKV(ctx, kvPRDryRun(pr.ID), kvPRFresh(pr.ID), kvPRRedecide(pr.ID), kvPRGate(pr.ID), kvPRGateReason(pr.ID), KVPRWait(pr.ID))
	e.clearRequested(ctx, pr.ID)

	if note, err := e.handBack(ctx, repo, pr, parked, label); err != nil {
		problems = append(problems, err)
	} else if note != "" {
		done = append(done, note)
	}

	what, kind := "aborted", "pr.aborted"
	if ignore {
		what, kind = "ignored", "pr.ignored"
	}
	res := what + " " + label
	if len(done) > 0 {
		res += ": " + strings.Join(done, ", ")
	}
	if ignore {
		res += "; muted until `magnum unmute " + label + "`"
	}
	perr := errors.Join(problems...)
	level := "info"
	if perr != nil {
		level = "warn"
		res += "; " + perr.Error()
	}
	e.event(ctx, level, subject, kind, res, map[string]any{"round_ran": roundRan, "parked": parked, "state": to})
	if perr != nil {
		return "", errors.New(res)
	}
	return res, nil
}

// interruptPR interrupts the PR's live sessions: an agent herdr shows
// working or blocked gets ctrl+c twice, a second apart (an idle one is left
// alone: ctrl+c twice would quit it), and a shell role's pane gets ctrl+c.
// It returns how many sessions it sent keys to.
func (e *Engine) interruptPR(ctx context.Context, pr store.PR, w *config.Watch) int {
	if e.d.Herdr == nil {
		return 0
	}
	keys, ok := e.d.Herdr.(keySender)
	if !ok {
		return 0
	}
	sessions, err := e.st.SessionsByPR(ctx, pr.ID)
	if err != nil {
		e.log.Warn("stop: sessions", "pr", pr.ID, "err", err)
		return 0
	}
	snap, err := e.d.Herdr.Snapshot(ctx)
	if err != nil {
		e.log.Warn("stop: herdr snapshot", "pr", pr.ID, "err", err)
		return 0
	}
	n := 0
	for _, s := range sessions {
		if s.State != store.SessionLive && s.State != store.SessionStarting {
			continue
		}
		pane := deref(s.HerdrPaneID)
		if a, ok := sessionAgent(snap, s); ok {
			if a.AgentStatus != herdr.StatusWorking && a.AgentStatus != herdr.StatusBlocked {
				continue
			}
			target := deref(s.AgentName)
			if target == "" {
				target = a.PaneID
			}
			var err error
			for i := range 2 {
				if i > 0 {
					_ = e.d.Sleep(ctx, time.Second)
				}
				if err = keys.AgentSendKeys(ctx, target, "ctrl+c"); err != nil {
					break
				}
			}
			if err != nil {
				e.log.Warn("stop: interrupt", "pr", pr.ID, "role", s.Role, "err", err)
				continue
			}
			n++
			continue
		}
		if pane == "" || !e.shellSession(w, s) {
			continue
		}
		if err := keys.PaneSendKeys(ctx, pane, "ctrl+c"); err != nil {
			e.log.Warn("stop: interrupt", "pr", pr.ID, "role", s.Role, "err", err)
			continue
		}
		n++
	}
	return n
}

// shellSession reports whether s is a shell role's pane (no agent).
func (e *Engine) shellSession(w *config.Watch, s store.Session) bool {
	if deref(s.AgentKind) == config.KindShell {
		return true
	}
	r, ok := e.cfg.RoleByNameOrAlias(w, s.Role)
	return ok && r.IsShell()
}

// sessionAgent is the herdr agent of a session: by agent name, else by pane.
func sessionAgent(snap herdr.Snapshot, s store.Session) (herdr.AgentInfo, bool) {
	name, pane := deref(s.AgentName), deref(s.HerdrPaneID)
	for _, a := range snap.Agents {
		if (name != "" && a.Name == name) || (pane != "" && a.PaneID == pane) {
			return a, true
		}
	}
	return herdr.AgentInfo{}, false
}

// waitAgentsQuiet waits up to abortQuietWait (a check a second) for no
// agent of the PR to be working or blocked; it reports whether that
// happened.
func (e *Engine) waitAgentsQuiet(ctx context.Context, prID int64) bool {
	for range int(abortQuietWait / time.Second) {
		if e.agentsQuiet(ctx, prID) {
			return true
		}
		if e.d.Sleep(ctx, time.Second) != nil {
			return false
		}
	}
	return e.agentsQuiet(ctx, prID)
}

// agentsQuiet reports whether no live session of the PR has an agent herdr
// shows working or blocked (an unreadable snapshot counts as not quiet).
func (e *Engine) agentsQuiet(ctx context.Context, prID int64) bool {
	sessions, err := e.st.SessionsByPR(ctx, prID)
	if err != nil {
		return false
	}
	snap, err := e.d.Herdr.Snapshot(ctx)
	if err != nil {
		return false
	}
	for _, s := range sessions {
		if s.State != store.SessionLive && s.State != store.SessionStarting {
			continue
		}
		if a, ok := sessionAgent(snap, s); ok && (a.AgentStatus == herdr.StatusWorking || a.AgentStatus == herdr.StatusBlocked) {
			return false
		}
	}
	return true
}

// abandonRuns marks the PR's active runs (pending, submitted, working,
// ended) abandoned with "aborted by user" and returns how many it marked.
func (e *Engine) abandonRuns(ctx context.Context, prID int64) int {
	runs, err := e.st.RunsByPR(ctx, prID)
	if err != nil {
		e.log.Warn("stop: runs", "pr", prID, "err", err)
		return 0
	}
	active := []string{store.RunPending, store.RunSubmitted, store.RunWorking, store.RunEnded}
	now := e.now()
	n := 0
	for _, r := range runs {
		if !slices.Contains(active, r.State) {
			continue
		}
		err := e.st.TransitionRun(ctx, r.ID, active, store.RunAbandoned, func(u *store.RunUpdate) {
			u.Set("error", abortedBy)
			u.Set("ended_at", now)
		})
		switch {
		case err == nil:
			n++
		case !errors.Is(err, store.ErrConflict):
			e.log.Warn("stop: abandon run", "run", r.ID, "err", err)
		}
	}
	return n
}

// parkForStop parks the PR's sessions, retrying while Park reports an agent
// still busy.
func (e *Engine) parkForStop(ctx context.Context, pr store.PR) error {
	if e.d.Agents == nil {
		return nil
	}
	var err error
	for try := 1; try <= abortParkTries; try++ {
		if err = e.d.Agents.Park(ctx, pr); err == nil || !errors.Is(err, agents.ErrBusy) || try == abortParkTries {
			break
		}
		if e.d.Sleep(ctx, 2*time.Second) != nil {
			break
		}
	}
	return err
}

// abortFrom are the states an aborted PR leaves for reviewed or baseline:
// a round's, and the ones its result may have put the PR in meanwhile (back
// in line, paused). needs_attention stays: it is for a human.
var abortFrom = []string{store.PRClaiming, store.PRReviewing, store.PRVerifying, store.PRPaused,
	store.PRQueued, store.PRRereviewPending}

// ignoreFrom are the states an ignored PR leaves for ineligible.
var ignoreFrom = []string{store.PRBaseline, store.PRIneligible, store.PRQueued, store.PRClaiming,
	store.PRReviewing, store.PRVerifying, store.PRReviewed, store.PRRereviewPending, store.PRPaused,
	store.PRNeedsAttention}

// settleStopped moves a stopped PR out of line: reviewed when it has a
// reviewed head, else baseline (ignore: ineligible, muted, skip_reason
// "ignored"); a merged PR (its post-merge round) goes back to closed, muted
// too for ignore, and is released after a fresh close grace. It returns the
// state it set ("" when the PR was elsewhere: an abort leaves a PR that
// needs attention, is reviewed or closed alone).
func (e *Engine) settleStopped(ctx context.Context, pr store.PR, ignore bool) (string, error) {
	to, from := store.PRBaseline, abortFrom
	if deref(pr.ReviewedSHA) != "" {
		to = store.PRReviewed
	}
	if ignore {
		to, from = store.PRIneligible, ignoreFrom
	}
	cur, err := e.st.PRByID(ctx, pr.ID)
	if err != nil {
		return "", err
	}
	merged := postMerge(cur)
	if merged {
		to = store.PRClosed
	}
	set := func(u *store.PRUpdate) {
		u.Set("forced", false)
		u.Set("attempts", 0)
		u.Set("next_attempt_at", nil)
		u.Set("next_eligible_at", nil)
		u.Set("last_error", nil)
		if ignore {
			u.Set("muted", true)
			u.Set("skip_reason", skipIgnored)
		}
		if merged {
			u.Set("release_after", e.releaseAfter())
		}
	}
	if !slices.Contains(from, cur.State) {
		if !ignore {
			return "", nil
		}
		// Closing: mute it all the same, so a reopened PR stays ignored.
		return "", e.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) {
			u.Set("muted", true)
			u.Set("skip_reason", skipIgnored)
		})
	}
	if err := e.st.TransitionPR(ctx, pr.ID, []string{cur.State}, to, set); err != nil {
		return "", fmt.Errorf("PR %s → %s: %w", cur.State, to, err)
	}
	return to, nil
}

// handBack returns a stopped PR's slot: claimed or busy back to held, then a
// pool slot is released (sessions parked, nothing pinned) while a per-PR
// worktree is kept. It returns what it did for the result.
func (e *Engine) handBack(ctx context.Context, repo store.Repo, pr store.PR, parked bool, label string) (string, error) {
	slot, has, err := e.slotOf(ctx, pr.ID)
	if err != nil {
		return "", err
	}
	if !has {
		return "", nil
	}
	if err := e.st.TransitionSlot(ctx, slot.ID, []string{store.SlotClaimed, store.SlotBusy}, store.SlotHeld, nil); err == nil {
		slot.State = store.SlotHeld
	} else if !errors.Is(err, store.ErrConflict) {
		return "", fmt.Errorf("slot %s to held: %w", slot.Name, err)
	}
	cur, err := e.st.PRByID(ctx, pr.ID)
	if err == nil {
		pr = cur
	}
	pool := e.cfg.PoolFor(repo.FullName())
	switch {
	case slot.Kind != store.SlotKindPool:
		return "worktree " + slot.Name + " kept", nil
	case pr.Pinned || slot.Pinned || slot.HoldReason != nil:
		return "slot " + slot.Name + " kept: pinned (magnum unpin)", nil
	case !parked:
		return "slot " + slot.Name + " kept until its sessions are parked", nil
	case slot.State != store.SlotHeld:
		return fmt.Sprintf("slot %s kept (%s)", slot.Name, slot.State), nil
	case pool == nil || e.d.Slots == nil:
		return "slot " + slot.Name + " kept: no pool for " + repo.FullName(), nil
	}
	if err := e.d.Slots.Release(ctx, slot, *pool, "aborted"); err != nil {
		e.event(ctx, "warn", "slot:"+slot.Name, "slot.release_failed", err.Error(), nil)
		return "", fmt.Errorf("release %s: %w", slot.Name, err)
	}
	e.event(ctx, "info", "slot:"+slot.Name, "slot.released", slot.Name+" handed back: "+label+" was stopped", nil)
	return "slot " + slot.Name + " released", nil
}
