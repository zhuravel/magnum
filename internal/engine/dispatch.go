package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// roundJob is one round the dispatcher decided to run.
type roundJob struct {
	pr      store.PR
	repo    store.Repo
	watch   config.Watch
	pool    *config.Pool
	slot    store.Slot
	hasSlot bool
	kind    string // initial | rereview | continue

	// continue
	round         int
	continueRunID string
	target        string
	// continued: the job continues a paused round whose judge was prompted
	// (prepareContinue). That round counted at its start and was not
	// refunded (turnContinues), so this job never counts against the daily
	// cap again, even when it becomes a full round (its judge's session was
	// lost, or its checkout is gone).
	continued bool

	// requested: a review request no round has served yet (pendingRequest)
	// started this round; like a forced one, it does not count against the
	// daily cap.
	requested bool

	// deltaCheck: the re-review was dispatched as a delta check
	// (deltaCheckDue): only the judge runs, unless prepare finds the commits
	// it reviews no longer make one (confirmDeltaCheck). A due full pass
	// clears it (planFullPass).
	deltaCheck bool
	// sameHead: the re-review was dispatched with no commits since the
	// reviewed one (sameHeadDue): only the judge runs, to re-decide its
	// earlier findings, unless the checkout finds a newer head
	// (confirmSameHead).
	sameHead bool
	// replies: the same-head re-review is a reply round (replyRoundDue):
	// that many replies came on the review since its judge read the threads,
	// and it may end with answers in them instead of a review.
	replies int
	// ciRed: the same-head re-review is a judge check of the head's red CI
	// (ciRedDue, ci_red.go): a check failed after the judge of its review was
	// prompted. It is no reply round (its judge reads the replies all the
	// same) and counts against no cap; a checkout that finds a newer head
	// clears it (prepare).
	ciRed bool
	// fullPass says why the re-review runs as a scheduled full pass
	// (fullPassDue, fullpass.go): every role in a fresh session, the whole
	// PR reviewed again; "" = it does not. fullPassRule names the rule that
	// fired: the [pipeline] key, or "requested".
	fullPass     string
	fullPassRule string
	// audit says why the re-review runs as an audit pass (auditDue,
	// audit.go): every session fresh, the judge probing the evidence map's
	// unknown and stale areas first; "" = it does not, as whenever a full
	// pass is due.
	audit string
	// approvalHold says why the round cannot approve (approval_hold.go:
	// planApprovalHold, at the round's setup): its clean verdict posts as a
	// comment and a full pass of the whole PR follows; "" = it may approve.
	approvalHold string
	// noApproval says why the round cannot approve by the approval's gates
	// decided at dispatch (approval_gates.go: planApprovalGates, at the
	// round's setup), the judge prompt's fixed words; "" = they let it.
	// ciGated: the round's judge asks the CI gate when its prompt is
	// written (pipeline.RoundInput.CIGate).
	noApproval string
	ciGated    bool

	// postMerge: GitHub had merged the PR when the round was dispatched, so
	// it is a post-merge review (post_merge.go); mergeBase is the commit it
	// reviews from, the PR's merge base with the base branch before the
	// merge (postMergeBase, set after the checkout; "" = unknown).
	postMerge bool
	mergeBase string

	// dispatchedAt is when dispatch launched the round (startRound): a fix
	// loop takes the forced round dispatched at or after its force for its
	// re-review (FixState.ForcedAt, fixAfterRound).
	dispatchedAt time.Time

	// The round's start (enterReviewing, refund.go): whether it recorded
	// last_round_started_at (started) and counted against the daily cap
	// (counted), on which day, the start time it recorded and the one before.
	started    bool
	counted    bool
	countedDay string
	startedAt  time.Time
	prevStart  *time.Time

	// evalHead is set for a magnum eval replay (RunEval): the caller checked
	// it out in the slot, the round is blind and never restarts. evalNotes
	// gives that round the scratch layout's notes. evalStored: a judge-only
	// replay (EvalCase.JudgeOnly): the judge alone on the reviewers' stored
	// reports of round evalReportsRound.
	evalHead   string
	evalNotes  bool
	evalStored bool
}

// roundHandle tracks a round goroutine, a `magnum open` restore, a fix job
// or a whiteboard job.
type roundHandle struct {
	cancel context.CancelFunc
	open   bool      // a magnum open restore, not a review round
	fix    bool      // a fix job (fix.go), not a review round
	stop   roundStop // magnum abort / ignore requests (abort.go)
	// whiteboard: a whiteboard job (whiteboard.go), not a review round,
	// which a close on GitHub stops (stopClosedRound).
	whiteboard bool
	// asked: a round someone asked for (forced by `magnum review` or the
	// board's r, or started by a review request) or a post-merge review,
	// which a close on GitHub never stops (stopClosedRound).
	asked bool
}

// activeRounds counts the round and open goroutines.
func (e *Engine) activeRounds() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.rounds)
}

// reviewRounds counts the review rounds (opens, fix and whiteboard jobs
// left out): what max_concurrent_reviews limits and the tab bar shows.
func (e *Engine) reviewRounds() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, h := range e.rounds {
		if !h.open && !h.fix && !h.whiteboard {
			n++
		}
	}
	return n
}

func (e *Engine) roundActive(prID int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.rounds[prID]
	return ok
}

// reserve registers h for the PR unless a round, an open or an eviction
// already holds it. Every slot operation on a PR's slot (a round's checkout,
// a restore, an eviction's park and release) runs under such a reservation,
// so they never overlap.
func (e *Engine) reserve(prID int64, h *roundHandle) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.rounds[prID]; ok || e.evicting[prID] != "" {
		return false
	}
	e.rounds[prID] = h
	return true
}

// unreserve drops the PR's round reservation and cancels its context.
func (e *Engine) unreserve(prID int64) {
	e.mu.Lock()
	h := e.rounds[prID]
	delete(e.rounds, prID)
	e.mu.Unlock()
	if h != nil {
		h.cancel()
	}
}

// reserveEviction marks the PR's slot as being evicted unless a round, an
// open or other slot work holds the PR (reserve then refuses the PR until
// endEviction).
func (e *Engine) reserveEviction(prID int64) bool {
	return e.reserveSlotWork(prID, "its slot is being evicted")
}

// reserveSlotWork is reserveEviction for any background work on the PR's
// slot or sessions; why is what dispatch shows meanwhile (heldReason).
func (e *Engine) reserveSlotWork(prID int64, why string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.rounds[prID]; ok || e.evicting[prID] != "" {
		return false
	}
	e.evicting[prID] = why
	return true
}

func (e *Engine) endEviction(prID int64) {
	e.mu.Lock()
	delete(e.evicting, prID)
	e.mu.Unlock()
}

// heldReason is why reserve refuses the PR now.
func (e *Engine) heldReason(prID int64) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if why := e.evicting[prID]; why != "" {
		return why
	}
	if h := e.rounds[prID]; h != nil && h.fix {
		return "the fixer is at work (`magnum abort` stops it)"
	}
	if h := e.rounds[prID]; h != nil && h.whiteboard {
		return "the whiteboard is being written"
	}
	return "round in progress"
}

// dispatch starts rounds while every gate allows it: paused PRs whose pause
// ended first (they continue their judge turn), then store.Candidates. A PR
// that waits records why (store.KVPRGate) for `magnum status`.
func (e *Engine) dispatch(ctx context.Context, ts tickState) {
	e.dryRounds = 0
	if reason := e.holdReason(ctx); reason != "" {
		e.log.Debug("dispatch paused", "reason", reason)
		return
	}
	paused := e.userPause(ctx) // only reviews the user asked for start
	if !ts.herdrUp {
		e.log.Debug("dispatch waits for herdr")
		return
	}
	now := e.now()

	cands, err := e.continueCandidates(ctx, now)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "paused PRs")
	}
	params := store.CandidateParams{
		Now:              now,
		QuietPeriod:      e.cfg.Daemon.PushQuietPeriod.Duration,
		MinInterval:      e.backstopInterval(),
		DraftMinInterval: e.cfg.Daemon.DraftMinRereviewInterval.Duration,
		MaxRoundsPerDay:  e.cfg.Daemon.MaxRoundsPerPRPerDay,
		Day:              store.DayKey(now),
	}
	queued, err := e.st.Candidates(ctx, params)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "candidates")
	}
	cands = append(cands, queued...)
	cands = append(cands, e.relaxedCandidates(ctx, params, queued, now)...)

	working := ts.workingCodex
	for _, pr := range cands {
		if e.flagHolds(ctx, pr) {
			continue // Codex flagged it and no fallback judge takes over: no round of any kind (codex_flag.go)
		}
		if paused != "" && !pr.Forced {
			continue // waitFor says why
		}
		if limit := e.cfg.Daemon.MaxConcurrentReviews; limit > 0 && e.reviewRounds()+e.plannedRounds() >= limit {
			// The rest wait for a running round to end; say so (noteWaits).
			e.noteGate(ctx, pr.ID, gate{reason: WaitCapacity,
				text: fmt.Sprintf("%s%d rounds running (max_concurrent_reviews %d)", gateCapacity, e.reviewRounds()+e.plannedRounds(), limit)})
			continue
		}
		if e.snoozeHolds(ctx, pr, now) {
			continue // waitFor says why (snooze.go)
		}
		if e.fixHolds(ctx, pr) {
			continue // waitFor says why (fix.go)
		}
		repo, err := e.st.RepoByID(ctx, pr.RepoID)
		if err != nil {
			e.warnUnlessStopped(ctx, err, "dispatch: repo", "pr", pr.ID)
			continue
		}
		w := e.cfg.WatchFor(repo.FullName())
		if e.reclassify(ctx, pr, w, now) {
			continue
		}
		e.migrateIdentity(ctx, &pr, repo, w)
		g := e.prGate(ctx, pr, repo, w, ts, now)
		started, codex := false, 0
		if g.text == "" {
			started, codex, g = e.startRound(ctx, pr, repo, w, working)
		}
		switch {
		case g.text != "":
			e.log.Debug("candidate skipped", "pr", pr.ID, "reason", g.text)
			e.noteGate(ctx, pr.ID, g)
		default:
			e.delKV(ctx, store.KVPRGate(pr.ID), kvPRGateReason(pr.ID), KVPRWait(pr.ID))
		}
		if started {
			working += codex
		}
	}
}

// relaxedCandidates lists the rereview_pending PRs that store.Candidates'
// backstop (the daemon's quiet period, re-review interval and daily cap)
// holds back although the throttle lets them through, and that the
// candidates in have lack: a requested round (pendingRequest) skips all of
// those rules, and so do a reply round (replyTrigger) and a judge check of
// red CI (ciRedWait), while a head that arrived during the PR's last review
// (arrivedDuringReview) skips the interval. They are read again without the
// backstop and kept when eligibility.Throttle, under their watch, says ready.
func (e *Engine) relaxedCandidates(ctx context.Context, p store.CandidateParams, have []store.PR, now time.Time) []store.PR {
	if p.QuietPeriod == 0 && p.MinInterval == 0 && p.DraftMinInterval == 0 && p.MaxRoundsPerDay <= 0 {
		return nil
	}
	p.QuietPeriod, p.MinInterval, p.DraftMinInterval, p.MaxRoundsPerDay = 0, 0, 0, 0
	all, err := e.st.Candidates(ctx, p)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "candidates without the backstop")
		return nil
	}
	var out []store.PR
	for _, pr := range all {
		if pr.State != store.PRRereviewPending || slices.ContainsFunc(have, func(x store.PR) bool { return x.ID == pr.ID }) {
			continue
		}
		if _, requested := e.pendingRequest(ctx, pr); !requested && !arrivedDuringReview(pr) {
			_, _, replied := e.replyTrigger(ctx, pr)
			if _, red := e.ciRedWait(ctx, pr); !replied && !red {
				continue
			}
		}
		repo, err := e.st.RepoByID(ctx, pr.RepoID)
		if err != nil {
			continue
		}
		w := e.cfg.WatchFor(repo.FullName())
		if w == nil || !e.throttle(ctx, *w, pr, e.factsFor(ctx, pr, *w, now), now).Ready {
			continue
		}
		out = append(out, pr)
	}
	return out
}

// backstopInterval is the re-review interval store.Candidates' backstop
// holds a non-draft PR to: min_rereview_interval, or a shorter
// own_min_rereview_interval (the daemon's or a watch's), which the backstop
// cannot tell the operator's PRs by and must not hold them past.
func (e *Engine) backstopInterval() time.Duration {
	d := e.cfg.Daemon.MinRereviewInterval.Duration
	own := []time.Duration{e.cfg.Daemon.OwnMinRereviewInterval.Duration}
	for i := range e.cfg.Watches {
		own = append(own, e.cfg.ThrottleFor(&e.cfg.Watches[i]).OwnMinRereviewInterval.Duration)
	}
	for _, o := range own {
		if o > 0 {
			d = min(d, o)
		}
	}
	return d
}

// arrivedDuringReview reports whether the head a PR waits to have reviewed
// was pushed before its last review was recorded: the review covers an
// older head, and the re-review interval does not apply.
func arrivedDuringReview(pr store.PR) bool {
	return pr.PendingSince != nil && pr.ReviewedAt != nil && !pr.PendingSince.After(*pr.ReviewedAt)
}

// noteGate records why the PR waits when that changed: the gate's sentence
// (store.KVPRGate) and its code (kvPRGateReason).
func (e *Engine) noteGate(ctx context.Context, prID int64, g gate) {
	if prev, _ := e.getKV(ctx, store.KVPRGate(prID)); prev != g.text {
		e.setKV(ctx, store.KVPRGate(prID), g.text)
	}
	b, err := json.Marshal(gateCode{Reason: g.reason, Kind: g.kind, By: g.by, At: g.at, Identity: g.identity, Until: g.until})
	if err != nil {
		return
	}
	if prev, _ := e.getKV(ctx, kvPRGateReason(prID)); prev != string(b) {
		e.setKV(ctx, kvPRGateReason(prID), string(b))
	}
}

// noteLastError shows why on the PR (prs.last_error) when it changed; dry
// runs only decide.
func (e *Engine) noteLastError(ctx context.Context, pr store.PR, why string) {
	if deref(pr.LastError) != why && !e.d.DryRun {
		_ = e.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) { u.Set("last_error", why) })
	}
}

// reclassify applies the watch's current filters to a waiting candidate (the
// watch config may have changed since the poller classified it, and the
// poller only re-classifies on GitHub changes): a PR they now reject moves
// to ineligible and reports true. Forced and paused PRs, PRs of unwatched
// repositories and PRs without their Details yet (prGate holds those) are
// left alone.
func (e *Engine) reclassify(ctx context.Context, pr store.PR, w *config.Watch, now time.Time) bool {
	if w == nil || pr.Forced || pr.State == store.PRPaused || pr.DetailsAt == nil {
		return false
	}
	dec := e.classify(ctx, *w, pr, now)
	if dec.Eligible {
		return false
	}
	if err := e.markIneligible(ctx, pr, []string{pr.State}, dec.Reason, false, now); err != nil {
		e.log.Info("dispatch: reclassify", "pr", pr.ID, "err", err)
	}
	return true
}

// plannedRounds counts dry-run dispatch decisions this tick (they take
// capacity like real rounds).
func (e *Engine) plannedRounds() int {
	if !e.d.DryRun {
		return 0
	}
	return e.dryRounds
}

// prGate is why one PR may not start a round now (the zero gate = go).
func (e *Engine) prGate(ctx context.Context, pr store.PR, repo store.Repo, w *config.Watch, ts tickState, now time.Time) gate {
	if e.roundActive(pr.ID) {
		return otherGate(e.heldReason(pr.ID)) // a round, or the fix job
	}
	if ts.busyPR[pr.ID] {
		return otherGate("an agent of the PR is working or blocked")
	}
	if why := e.restartWait(ctx, pr.ID); why != "" {
		return otherGate(why) // session_restart.go: no prompt reaches a process the restart replaces
	}
	if now.Before(e.cfg.Daemon.CooldownUntil(pr.HumanActiveAt)) {
		return otherGate("human active in the PR's panes")
	}
	if w == nil {
		return otherGate("repository " + repo.FullName() + " is no longer watched (no [[watch]] covers it)")
	}
	if pr.DetailsAt == nil && !pr.Forced {
		// Author, labels and fork status are unknown until a Details fetch
		// succeeds: the watch's filters cannot be applied yet.
		return otherGate("waiting for the PR's details from GitHub")
	}
	if ok, why := e.identityHealthy(ctx, pr.Identity); !ok {
		e.noteLastError(ctx, pr, why)
		return gate{reason: WaitIdentity, text: why}
	}
	if v, ok := e.getKV(ctx, store.KVWatchPaused(repo.WatchOwner)); ok && v != "" {
		return otherGate("watch " + repo.WatchOwner + " paused: " + v)
	}
	return gate{}
}

// continueCandidates are paused PRs whose retry time passed: they continue
// the judge's turn (or restart the round when the judge was never prompted).
// A paused post-merge round (a merged PR, forced) continues too.
func (e *Engine) continueCandidates(ctx context.Context, now time.Time) ([]store.PR, error) {
	prs, err := e.st.ListPRs(ctx, store.PRFilter{States: []string{store.PRPaused}})
	if err != nil {
		return nil, err
	}
	var out []store.PR
	for _, pr := range prs {
		if !(pr.GHState == store.GHOpen || postMerge(pr) && pr.Forced) || (pr.Muted && !pr.Forced) {
			continue
		}
		if pr.NextAttemptAt != nil && now.Before(*pr.NextAttemptAt) {
			continue
		}
		out = append(out, pr)
	}
	slices.SortStableFunc(out, func(a, b store.PR) int { return a.UpdatedAt.Compare(b.UpdatedAt) })
	return out, nil
}

// slotOf returns the PR's current (not removed) slot.
func (e *Engine) slotOf(ctx context.Context, prID int64) (store.Slot, bool, error) {
	sl, err := e.st.SlotByPR(ctx, prID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Slot{}, false, nil
	}
	if err != nil {
		return store.Slot{}, false, err
	}
	return sl, true, nil
}

// startRound acquires a slot for pr (its own claimed/held slot, a free pool
// slot, or a per-PR worktree created by the round) and launches the round
// goroutine. It reports whether a round was started (or planned), the Codex
// agents its roles add, and why the PR waits (the zero gate when it started
// or waits silently, for example for a slot being provisioned).
func (e *Engine) startRound(ctx context.Context, pr store.PR, repo store.Repo, w *config.Watch, workingCodex int) (bool, int, gate) {
	full := repo.FullName()
	subject := prSubject(repo, pr.Number)
	job := &roundJob{pr: pr, repo: repo, watch: *w, pool: e.cfg.PoolFor(full), kind: kindFor(pr), postMerge: postMerge(pr), dispatchedAt: e.now()}
	if !pr.Forced {
		_, job.requested = e.pendingRequest(ctx, pr)
	}
	if job.kind == pipeline.KindRereview {
		f := e.factsFor(ctx, pr, *w, e.now())
		e.deltaFacts(ctx, pr, &f)
		job.deltaCheck = e.deltaCheckDue(ctx, *w, pr, f, job.requested)
		job.sameHead = e.sameHeadDue(ctx, pr)
		if job.ciRed = e.ciRedDue(ctx, pr, job.sameHead, job.requested); !job.ciRed {
			job.replies = e.replyRoundDue(ctx, pr, job.sameHead, job.requested)
		}
		e.planFullPass(ctx, job)         // fullpass.go: a due full pass clears the delta check
		job.audit = e.auditDue(ctx, job) // audit.go: never with a full pass
	}
	from := store.ClaimableStates
	if pr.State == store.PRPaused {
		from = []string{store.PRPaused}
		if !e.prepareContinue(ctx, job) {
			return false, 0, gate{}
		}
	}
	if g := e.quietHoursGate(job); g.text != "" {
		return false, 0, g
	}
	if job.kind != kindContinue { // github_floor.go: a continued round finishes the judge turn of a round that ran
		if g := e.githubGate(job); g.text != "" {
			return false, 0, g
		}
	}

	rctx, cancel := context.WithCancel(ctx)
	if !e.reserve(pr.ID, &roundHandle{cancel: cancel, asked: pr.Forced || job.requested || job.postMerge}) {
		cancel()
		return false, 0, otherGate(e.heldReason(pr.ID))
	}
	launched := false
	defer func() {
		if !launched {
			e.unreserve(pr.ID)
		}
	}()

	slot, has, err := e.slotOf(ctx, pr.ID)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "dispatch: slots")
		return false, 0, gate{}
	}
	if has {
		if g := e.slotGate(ctx, job, &slot, subject); g.text != "" {
			return false, 0, g
		}
		job.slot, job.hasSlot = slot, true
	}
	if job.kind == kindContinue && !has {
		// The paused round's checkout is gone: start over on the current head.
		job.kind = kindFor(pr)
		job.continueRunID, job.round, job.target = "", 0, ""
	}

	// The roles the round runs decide which pauses and Codex limit apply
	// (the judge alone of a delta check or a re-review of the same head).
	toRun, err := pipeline.RolesToRun(ctx, e.st, e.cfg, pr, e.rolesForPR(ctx, w, pr.ID), e.requestedRoles(ctx, pr.ID), job.kind)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "dispatch: roles", "subject", subject)
		return false, 0, gate{}
	}
	if job.deltaCheck || job.sameHead {
		toRun = judgeAlone(toRun)
	}
	if kind, why := e.kindPause(ctx, agentKinds(toRun)); why != "" { // the wait's sentence names the command (Wait.Sentence)
		return false, 0, gate{reason: WaitKind, kind: kind, text: why}
	}
	codex := codexRoles(toRun)
	if why := e.budgetGate(job, codex); why != "" {
		return false, 0, gate{reason: WaitBudget, text: why}
	}
	if limit := e.cfg.Daemon.MaxTotalWorkingCodex; limit > 0 && codex > 0 && workingCodex+min(codex, limit) > limit {
		return false, 0, gate{reason: WaitCapacity,
			text: fmt.Sprintf("working Codex agents at the limit (%d working, max_total_working_codex %d)", workingCodex, limit)}
	}

	if e.d.DryRun {
		return e.planStart(ctx, job, subject), codex, gate{}
	}

	switch {
	case has || job.pool == nil:
		if err := e.st.TransitionPR(ctx, pr.ID, from, store.PRClaiming, nil); err != nil {
			e.log.Debug("dispatch: claim PR", "subject", subject, "err", err)
			return false, 0, gate{}
		}
	default:
		if pr.State == store.PRPaused {
			if err := e.st.TransitionPR(ctx, pr.ID, from, claimableState(pr), nil); err != nil {
				return false, 0, gate{}
			}
			pr.State = claimableState(pr)
			job.pr = pr
		}
		sl, err := e.d.Slots.Claim(ctx, pr, *job.pool)
		switch {
		case errors.Is(err, slots.ErrNoFreeSlot):
			e.needSlot(ctx, *job.pool, subject)
			return false, 0, gate{reason: WaitSlot, text: gateNoFreeSlot + job.pool.Repo + " pool"}
		case err != nil:
			e.log.Info("dispatch: claim", "subject", subject, "err", err)
			return false, 0, gate{}
		}
		job.slot, job.hasSlot = sl, true
		e.event(ctx, "info", subject, "slot.claimed", "claimed "+sl.Name, nil)
	}
	launched = true
	e.launch(ctx, rctx, job)
	return true, codex, gate{}
}

// slotGate checks the PR's own slot before a round: the zero gate when the
// round can use it, else why the PR waits. A busy slot is left over from a
// round whose busy → held write failed (the PR is reserved, so no round runs
// in it) and is repaired; a per-PR worktree that is provisioning, lost or
// broken is (re)created by the round; a pool slot whose release stalled gets
// the release resumed; a broken or lost pool slot needs `magnum slots
// repair`. A pin is the operator's own choice, so it is a wait (WaitPinned,
// pinWait), never the PR's error; a hold a guard persisted is.
func (e *Engine) slotGate(ctx context.Context, job *roundJob, slot *store.Slot, subject string) gate {
	slotWait := func(why string) gate { return gate{reason: WaitSlot, text: why} }
	perPR := slot.Kind == store.SlotKindPerPR
	switch {
	case slot.State == store.SlotClaimed || slot.State == store.SlotHeld:
	case perPR && slices.Contains(recreatedPerPR, slot.State):
	case slot.State == store.SlotBusy:
		if !e.d.DryRun {
			if err := e.st.TransitionSlot(ctx, slot.ID, []string{store.SlotBusy}, store.SlotHeld, nil); err != nil {
				return slotWait(fmt.Sprintf("slot %s is busy without a round: %v", slot.Name, err))
			}
			e.event(ctx, "warn", "slot:"+slot.Name, "slot.repaired", slot.Name+" was busy without a round; back to held", nil)
		}
		slot.State = store.SlotHeld
	case !perPR && (slot.State == store.SlotReleasing || slot.State == store.SlotDirtySchema):
		if job.pool != nil {
			e.resumeRelease(ctx, *job.pool, *slot, subject)
		}
		return slotWait("slot " + slot.Name + " is being released")
	case slot.State == store.SlotBroken || slot.State == store.SlotLost:
		why := unusableSlotSentence(*slot)
		e.noteLastError(ctx, job.pr, why)
		return slotWait(why)
	default:
		return slotWait(fmt.Sprintf("slot %s is %s", slot.Name, slot.State))
	}
	switch {
	case slot.HoldReason != nil:
		why := slotHeldReason(*slot)
		e.noteLastError(ctx, job.pr, why)
		return slotWait(why)
	case slot.Pinned:
		return e.pinWait(ctx, job.pr, *slot, subject)
	}
	return gate{}
}

// pinWait is the wait of a PR whose slot is pinned: who pinned it and since
// when, from the newest pin event (pinOrigin), or, for an open's hold
// (open_hold.go), when the hold ends (until). The error an older daemon
// recorded for the pin ("slot X is pinned (magnum unpin)") goes, so the
// screens show the wait without an error mark.
func (e *Engine) pinWait(ctx context.Context, pr store.PR, slot store.Slot, subject string) gate {
	if deref(pr.LastError) == "slot "+slot.Name+" is pinned (magnum unpin)" && !e.d.DryRun {
		_ = e.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) { u.Set("last_error", nil) })
	}
	if h, held := e.openHold(ctx, pr.ID); held && pr.Pinned { // open_hold.go: a hold ends by itself
		end := h.End(e.cfg.Daemon, pr.HumanActiveAt)
		return gate{reason: WaitPinned, text: "slot " + slot.Name + " is " + heldWords(end, e.now()), by: "magnum open", at: h.At, until: end}
	}
	why := "slot " + slot.Name + " is pinned"
	by, at := e.pinOrigin(ctx, subject) // review_unpin.go
	if by != "" {
		why += " by " + by + " since " + pastClock(at, e.now())
	}
	return gate{reason: WaitPinned, text: why, by: by, at: at}
}

// unusableSlotSentence is why a broken or lost slot cannot be used, as a
// dispatch wait and a refused request both say it: the slot, its state, the
// last error it recorded (when it has one) and the command that repairs it,
// "slot review1 is broken: schema reset failed (magnum slots repair
// review1)". The last error is magnum's own text (a failed setup step),
// never a PR's.
func unusableSlotSentence(slot store.Slot) string {
	why := fmt.Sprintf("slot %s is %s", slot.Name, slot.State)
	if le := deref(slot.LastError); le != "" {
		why += ": " + le
	}
	return why + " (magnum slots repair " + slot.Name + ")"
}

// recreatedPerPR are the states of a per-PR worktree row the round's
// CreatePRWorktree (re)creates.
var recreatedPerPR = []string{store.SlotProvisioning, store.SlotLost, store.SlotBroken}

// kindPauseReason is why work whose roles use kinds must wait: the first
// of them that is paused, with the command that lifts its pause ("" =
// none): "claude paused (usage_limit) until 14:00; `magnum resume --tool
// claude` lifts it", which the refusals that name it end with. A pause of
// one kind leaves rounds that do not use it alone.
func (e *Engine) kindPauseReason(ctx context.Context, kinds []string) string {
	_, why := e.pausedKind(ctx, kinds)
	return why
}

// pausedKind is kindPauseReason with the paused kind it names.
func (e *Engine) pausedKind(ctx context.Context, kinds []string) (kind, why string) {
	if kind, why = e.kindPause(ctx, kinds); why != "" {
		why += "; " + resumeToolHint(kind)
	}
	return kind, why
}

// resumeToolHint is the command that lifts the pause of agent kind, as a
// refusal or a resume's answer ends with it: "`magnum resume --tool
// claude` lifts it".
func resumeToolHint(kind string) string { return "`magnum resume --tool " + kind + "` lifts it" }

// kindPause is pausedKind without the command, for a round's dispatch
// gate, whose wait sentence ends with it (Wait.Sentence): "claude paused
// (usage_limit) until 14:00".
func (e *Engine) kindPause(ctx context.Context, kinds []string) (kind, why string) {
	now := e.now()
	for _, kind := range kinds {
		if p, ok := e.toolPause(ctx, kind); ok && now.Before(p.Until) {
			return kind, pausedWords(kind, p)
		}
	}
	return "", ""
}

// pausedWords says kind's pause p as a wait does: "claude paused
// (usage_limit) until 14:00", and for the hooks review, which ends when the
// hooks are trusted, what holds it instead of the time the check renews
// ("codex paused (hooks_review): 1 hook in ~/.codex/hooks.json has no trust
// entry: run `codex` once and choose Trust all").
func pausedWords(kind string, p toolPause) string {
	if p.Reason == HooksReviewPauseReason {
		return fmt.Sprintf("%s paused (%s): %s", kind, p.Reason, p.Detail)
	}
	return fmt.Sprintf("%s paused (%s) until %s", kind, p.Reason, p.Until.Local().Format("15:04"))
}

// codexRoles counts the roles that run a Codex agent.
func codexRoles(roles []config.Role) int {
	n := 0
	for _, r := range roles {
		if r.AgentKind() == agents.KindCodex {
			n++
		}
	}
	return n
}

// planStart records what a dry run would do for job.
func (e *Engine) planStart(ctx context.Context, job *roundJob, subject string) bool {
	what := job.kind + " round"
	switch {
	case job.deltaCheck:
		what = "delta check"
	case job.ciRed:
		what = "judge check of the red CI (the judge alone)"
	case job.replies > 0:
		what = replyLabel(job.replies) + " (the judge alone)"
	case job.sameHead:
		what = "re-review of the same head (the judge alone)"
	case job.fullPass != "":
		what = fullReReviewLabel
	case job.audit != "":
		what = auditReReviewLabel
	}
	switch {
	case job.hasSlot:
		e.rec.Record(ctx, subject, "review", fmt.Sprintf("%s in %s at %s", what, job.slot.Name, textx.ShortSHA(job.pr.HeadSHA)))
	case job.pool == nil:
		e.rec.Record(ctx, subject, "review", fmt.Sprintf("%s in a new per-PR worktree at %s", what, textx.ShortSHA(job.pr.HeadSHA)))
	default:
		free, err := e.st.FreeSlots(ctx, job.pool.Repo, job.pr.ID)
		if err != nil || len(free) == 0 {
			e.needSlot(ctx, *job.pool, subject)
			return false
		}
		e.rec.Record(ctx, subject, "review", fmt.Sprintf("%s in %s (claim) at %s", what, free[0].Name, textx.ShortSHA(job.pr.HeadSHA)))
	}
	e.dryRounds++
	return true
}

// prepareContinue fills the paused round's identity: its round number,
// marker (the judge run whose id the review carries) and target. A pause
// before the judge was prompted restarts the round instead.
func (e *Engine) prepareContinue(ctx context.Context, job *roundJob) bool {
	runs, err := e.st.RunsByPR(ctx, job.pr.ID)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "continue: runs")
		return false
	}
	j := e.latestJudge(runs)
	if j.prompted == nil {
		job.kind = kindFor(job.pr)
		return true
	}
	job.kind, job.round, job.continueRunID, job.target = kindContinue, j.round, j.marker.ID, j.marker.TargetSHA
	job.continued = true
	return true
}

// diffBase is the base the round's comparisons take the PR's own diff
// against (measureRange): its base branch, or, for a post-merge round, the
// base before the merge ("" when unknown: the base branch holds the merged
// head, so no own diff is compared).
func (job *roundJob) diffBase() string {
	if job.postMerge {
		return job.mergeBase
	}
	return prBase(job.repo, job.pr)
}

const kindContinue = "continue"

func kindFor(pr store.PR) string {
	if deref(pr.ReviewedSHA) != "" {
		return "rereview"
	}
	return "initial"
}
