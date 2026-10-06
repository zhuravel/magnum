package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/identity"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// Retry policy for failed rounds of one head.
const (
	maxAttempts    = 3
	retryBase      = time.Minute
	retryMax       = 30 * time.Minute
	busyRetry      = time.Minute
	holdRetry      = 5 * time.Minute
	closedRecheck  = 5 * time.Minute
	stopRetryDelay = time.Minute
)

func backoff(attempts int) time.Duration {
	d := retryBase
	for i := 1; i < attempts && d < retryMax; i++ {
		d *= 2
	}
	return min(d, retryMax)
}

// launch starts the round goroutine for job under the PR's reservation
// (reserve, whose context ctx is), which it drops when the round ends.
func (e *Engine) launch(ctx context.Context, job *roundJob) {
	e.roundWG.Add(1)
	go func() {
		defer e.roundWG.Done()
		defer e.unreserve(job.pr.ID)
		e.runRound(ctx, job)
		e.afterRound(ctx, job.pr.ID) // magnum abort / ignore (abort.go)
	}()
}

// setupError is a failure before the pipeline ran.
type setupError struct {
	err      error
	pause    *pipeline.Pause
	retryAt  time.Time
	noCharge bool // not the PR's fault: the retry budget is not used
	// infra names an infrastructure failure (infra.go): dispatch pauses for
	// everything and the PR retries after the probe; probeDir is the clone
	// whose origin the probe asks.
	infra    string
	probeDir string
}

func (s *setupError) Error() string { return s.err.Error() }

// runRound prepares the slot and sessions, runs the pipeline and maps the
// result onto the PR state machine.
func (e *Engine) runRound(ctx context.Context, job *roundJob) {
	fctx := context.WithoutCancel(ctx)
	in, ws, serr := e.prepare(ctx, job)
	if serr != nil {
		if ctx.Err() != nil {
			e.log.Info("round setup interrupted", "pr", job.pr.ID)
			return // recovery on the next start re-evaluates the claiming row
		}
		e.setupFailed(fctx, job, serr)
		return
	}
	res, err := e.d.Rounds(job.pr.Identity).RunRound(ctx, in)
	e.finish(fctx, job, in, ws, res, err, ctx.Err() != nil)
}

// prepare checks the PR out, starts or resumes its agents and moves the PR
// to reviewing; it returns the pipeline input.
func (e *Engine) prepare(ctx context.Context, job *roundJob) (pipeline.RoundInput, agents.Workspace, *setupError) {
	var ws agents.Workspace
	fail := func(err error) (pipeline.RoundInput, agents.Workspace, *setupError) {
		return pipeline.RoundInput{}, ws, classifySetup(err, job.pr, e.cfg, e.now())
	}
	pr := job.pr
	if e.d.Rounds == nil || e.d.Rounds(pr.Identity) == nil {
		return fail(fmt.Errorf("no round runner for identity %q", pr.Identity))
	}
	rs := roundSetup{src: e.d.Identities[pr.Identity]}
	if rs.src == nil {
		return fail(fmt.Errorf("identity %q is not configured", pr.Identity))
	}
	if e.d.Agents == nil || e.d.Slots == nil {
		return fail(errors.New("engine has no agents or slots manager"))
	}
	dryRun, err := e.dryRunRound(ctx, pr)
	if err != nil { // a dry run must never post: no round until the mode is known
		return pipeline.RoundInput{}, ws, &setupError{err: err, noCharge: true, retryAt: e.now().Add(busyRetry)}
	}

	// 1. Checkout (and a post-merge round's base, which the base branch no
	// longer gives), once the sessions that would reload the project
	// config from it are out of its way.
	if !(job.kind == kindContinue && job.target != "") && job.evalHead == "" {
		if _, err := e.parkReloading(ctx, job); err != nil {
			return fail(err)
		}
	}
	if rs.target, err = e.checkout(ctx, job); err != nil {
		return pipeline.RoundInput{}, ws, e.checkoutFailed(err, job)
	}
	if job.postMerge {
		job.mergeBase = e.postMergeBase(ctx, job, rs.target)
	}

	// 2. The roles this round runs (the watch's, per their runs and this
	// round's requests, then the ones triage keeps), then a preflight of
	// their agent kinds before starting or prompting anything.
	rs.roles = e.cfg.RolesFor(&job.watch)
	rs.requested = e.requestedRoles(ctx, pr.ID)
	if job.deltaCheck && job.kind == pipeline.KindRereview {
		rs.delta = e.confirmDeltaCheck(ctx, job, rs.target) // nil: the round runs in full
	}
	if job.sameHead && job.kind == pipeline.KindRereview {
		rs.sameHead = e.confirmSameHead(ctx, job, rs.target) // false: the round runs in full
	}
	if job.kind != kindContinue && job.evalHead == "" && !rs.judgeOnly() {
		rs.requested = append(rs.requested, e.rerunRoles(ctx, job, rs.roles, rs.target)...)
	}
	if rs.toRun, err = pipeline.RolesToRun(ctx, e.st, e.cfg, pr, rs.roles, rs.requested, job.kind); err != nil {
		return fail(err)
	}
	if len(rs.toRun) == 0 {
		return fail(fmt.Errorf("watch %s has no judge among its roles", job.watch.Owner))
	}
	if rs.judgeOnly() {
		// A delta check, or a re-review of the same head: the judge alone,
		// no triage.
		rs.roles, rs.toRun = judgeAlone(rs.roles), judgeAlone(rs.toRun)
	} else {
		// A small diff may not need every reviewer ([triage]): what is
		// dropped here is neither preflighted nor started.
		e.triage(ctx, job, &rs)
	}
	if serr := e.preflight(ctx, agentKinds(rs.toRun)); serr != nil {
		return pipeline.RoundInput{}, ws, serr
	}

	// 3. Workspace and agents, and the round's kind.
	var serr *setupError
	if ws, serr = e.startSessions(ctx, job, &rs); serr != nil {
		return pipeline.RoundInput{}, ws, serr
	}

	// 4. State: slot busy, PR reviewing (a new round counts for the throttle).
	if serr := e.enterReviewing(ctx, job, rs.kind); serr != nil {
		return pipeline.RoundInput{}, ws, serr
	}
	e.finalizeStaleRuns(ctx, pr.ID)
	if rs.kind != kindContinue {
		e.noteDeltaCheckRound(ctx, pr.ID, rs.delta, rs.target) // a continue keeps the paused round's
	}
	switch {
	case rs.kind == kindContinue:
		rs.replies = e.continuedReplies(ctx, pr, rs.target)
	case rs.sameHead && job.replies > 0:
		rs.replies = job.replies
		e.noteReplyRound(ctx, pr.ID, rs.target, rs.replies, job.startedAt)
	}

	in := e.roundInput(ctx, job, rs, ws, dryRun)
	e.sidebar(ctx, ws.WorkspaceID, map[string]string{"magnum": "reviewing " + textx.ShortSHA(rs.target)})
	names := roleNames(rs.toRun)
	what := rs.kind + " round"
	data := map[string]any{"slot": job.slot.Name, "kind": rs.kind, "target_sha": rs.target, "roles": names, "requested": rs.requested,
		"post_merge": job.postMerge}
	if rs.delta != nil {
		what = deltaCheckLabel(rs.delta.Lines)
		data["delta_check"], data["delta_lines"], data["delta_files"] = true, rs.delta.Lines, len(rs.delta.Files)
	}
	if rs.sameHead {
		data["same_head"] = true
		e.event(ctx, "info", prSubject(job.repo, pr.Number), "round.same_head",
			"same head: the judge re-decides the threads (no commits since "+textx.ShortSHA(rs.target)+")", map[string]any{"kind": rs.kind, "target_sha": rs.target})
	}
	if rs.replies > 0 && rs.kind != kindContinue {
		what, data["replies"] = replyLabel(rs.replies), rs.replies
	}
	if job.postMerge {
		what = "post-merge " + what
	}
	e.event(ctx, "info", prSubject(job.repo, pr.Number), "engine.round_start",
		fmt.Sprintf("%s in %s at %s: %s", what, job.slot.Name, textx.ShortSHA(rs.target), strings.Join(names, ", ")), data)
	return in, ws, nil
}

// roundSetup is what prepare learns on its way to the pipeline input.
type roundSetup struct {
	src       identity.Source
	target    string        // the commit the round reviews
	roles     []config.Role // the watch's roles
	requested []string      // roles requested for this round
	toRun     []config.Role // the roles that run (pipeline.RolesToRun)
	kind      string        // the round's kind once the judge's conversation is known
	round     int           // the round number a continue keeps (0 = the pipeline's next)
	// delta is the delta check the round runs (confirmDeltaCheck); nil = a
	// round of every role that runs.
	delta *pipeline.DeltaCheck
	// sameHead: the round re-reviews the reviewed head, the judge alone
	// (confirmSameHead).
	sameHead bool
	// coldJudge: the judge alone started in a fresh session because its
	// prompt cache was cold (coldJudge); the reviewers kept theirs.
	coldJudge bool
	// replies: the round is a reply round (pipeline.RoundInput.Replies), or
	// continues one.
	replies int
}

// judgeOnly reports whether the round runs its judge alone: a delta check
// or a re-review of the same head.
func (rs *roundSetup) judgeOnly() bool { return rs.delta != nil || rs.sameHead }

// checkout checks the PR's head out in its slot (a per-PR worktree is
// created, or recreated, first) and returns the commit the round reviews:
// the one the fetch found, which may be newer than the radar's. A continue
// keeps the paused round's target.
func (e *Engine) checkout(ctx context.Context, job *roundJob) (string, error) {
	pr := job.pr
	if job.kind == kindContinue && job.target != "" {
		return job.target, nil
	}
	if job.evalHead != "" {
		return job.evalHead, nil // RunEval's caller checked the pinned head out
	}
	var pool config.Pool
	if job.pool != nil {
		pool = *job.pool
	}
	if !job.hasSlo || (job.slot.Kind == store.SlotKindPerPR && slices.Contains(recreatedPerPR, job.slot.State)) {
		sl, err := e.d.Slots.CreatePRWorktree(ctx, job.watch, job.repo.FullName(), pr, pr.HeadSHA)
		if err != nil {
			return "", fmt.Errorf("per-PR worktree: %w", err)
		}
		job.slot, job.hasSlo = sl, true
	} else if err := e.d.Slots.Checkout(ctx, job.slot, pr, pool, pr.HeadSHA); err != nil {
		return "", fmt.Errorf("checkout in %s: %w", job.slot.Name, err)
	}
	sl, err := e.st.SlotByID(ctx, job.slot.ID)
	if err != nil {
		return "", err
	}
	job.slot = sl
	target := deref(sl.CheckedOutSHA)
	if target == "" {
		target = pr.HeadSHA
	}
	if target != pr.HeadSHA {
		e.event(ctx, "info", prSubject(job.repo, pr.Number), "round.head_moved", fmt.Sprintf("GitHub head moved to %s; reviewing that", textx.ShortSHA(target)), nil)
	}
	return target, nil
}

// startSessions gives the roles that run their panes (one workspace) and
// starts or resumes their agents, the judge first, then settles the round's
// kind in rs: a judge without its conversation re-reads the history
// (recovery, or initial before any review; a delta check or a same-head
// re-review stays one, checkFresh), and a continue that became a full
// round starts the other roles too.
func (e *Engine) startSessions(ctx context.Context, job *roundJob, rs *roundSetup) (agents.Workspace, *setupError) {
	var ws agents.Workspace
	fail := func(err error) (agents.Workspace, *setupError) {
		return ws, classifySetup(err, job.pr, e.cfg, e.now())
	}
	pr := job.pr
	fresh, why := false, "" // why: what made the sessions fresh, for the events
	if v, _ := e.getKV(ctx, kvPRFresh(pr.ID)); v == "1" {
		fresh, why = true, "fresh sessions were requested"
	}
	// Sessions post as the identity their panes were created for; a PR whose
	// identity changed since (watch config, magnum review --as) must not
	// continue in them, or the review is posted by the wrong login.
	if v := e.sessionsIdentity(ctx, pr.ID); v != "" && v != pr.Identity {
		fresh, why = true, fmt.Sprintf("identity %s → %s", v, pr.Identity)
		e.event(ctx, "info", prSubject(job.repo, pr.Number), "pr.identity_changed", why+": starting fresh sessions", nil)
	}
	if fresh {
		if err := e.d.Agents.Park(ctx, pr); err != nil {
			return fail(fmt.Errorf("park for a fresh start: %w", err))
		}
	}
	env, err := e.paneEnv(ctx, job, rs.src, rs.target)
	if err != nil {
		return fail(err)
	}
	label := fmt.Sprintf("%s#%d", job.repo.Name, pr.Number)
	if job.evalHead != "" {
		label = "eval " + label
	}
	if ws, err = e.d.Agents.EnsureWorkspace(ctx, pr, job.slot.Path, env, label, rs.toRun); err != nil {
		return fail(fmt.Errorf("workspace: %w", err))
	}
	e.setKV(ctx, kvPRSessionsIdentity(pr.ID), pr.Identity)
	effort := effortOf(job.kind)
	if rs.judgeOnly() && e.hasOwnReview(ctx, pr, rs.src.Login()) {
		effort = effortCheck // a fresh judge checks the delta, or the same head, too (checkFresh)
	}
	if !fresh {
		var cold string
		if rs.coldJudge, cold = e.coldJudge(ctx, job, rs); rs.coldJudge {
			why = cold
		}
	}
	recovered := false
	if ws, recovered, err = e.startRoles(ctx, pr, ws, job.slot.Path, env, rs.toRun, fresh, rs.coldJudge, effort); err != nil {
		return fail(err)
	}

	rs.kind, rs.round = job.kind, 0
	if rs.kind == kindContinue {
		rs.round = job.round
	}
	if recovered && rs.kind != pipeline.KindInitial {
		rs.round = 0
		rs.kind = pipeline.KindRecovery
		if deref(pr.ReviewedSHA) == "" {
			rs.kind = pipeline.KindInitial
		}
	}
	full := job.kind == kindContinue && rs.kind != kindContinue
	if rs.judgeOnly() && rs.kind != pipeline.KindRereview && !e.checkFresh(ctx, job, rs, effort, cmp.Or(why, "the judge's session is gone")) {
		rs.delta, rs.sameHead, rs.roles, full = nil, false, e.cfg.RolesFor(&job.watch), true
	}
	if full {
		// The continued round (or the judge alone) became a full one: the
		// other roles that run need their panes and agents too.
		all, err := pipeline.RolesToRun(ctx, e.st, e.cfg, pr, rs.roles, rs.requested, rs.kind)
		if err != nil {
			return fail(err)
		}
		extra := slices.DeleteFunc(all, func(r config.Role) bool {
			return slices.ContainsFunc(rs.toRun, func(x config.Role) bool { return x.Name == r.Name })
		})
		if serr := e.preflight(ctx, agentKinds(extra)); serr != nil {
			return ws, serr
		}
		if ws, _, err = e.startRoles(ctx, pr, ws, job.slot.Path, env, extra, fresh, false, effortOf(rs.kind)); err != nil {
			return fail(err)
		}
		rs.toRun = append(rs.toRun, extra...)
	}
	return ws, nil
}

// enterReviewing moves the slot to busy and the PR from claiming to
// reviewing; a round other than a continue records its start for the
// throttle (last_round_started_at), and an automatic one (neither forced
// nor requested nor a reply round, which reply_min_interval spaces) counts
// against the daily cap (rounds_today), unless it continues a round that
// counted already (roundJob.continued).
func (e *Engine) enterReviewing(ctx context.Context, job *roundJob, kind string) *setupError {
	pr := job.pr
	if err := e.st.TransitionSlot(ctx, job.slot.ID, []string{store.SlotClaimed, store.SlotHeld, store.SlotBusy}, store.SlotBusy, nil); err != nil {
		return classifySetup(fmt.Errorf("slot %s to busy: %w", job.slot.Name, err), pr, e.cfg, e.now())
	}
	now := e.now()
	day := store.DayKey(now)
	counted := kind != kindContinue && !job.continued && !pr.Forced && !job.requested && job.replies == 0
	err := e.st.TransitionPR(ctx, pr.ID, []string{store.PRClaiming}, store.PRReviewing, func(u *store.PRUpdate) {
		u.Set("last_error", nil)
		if kind != kindContinue {
			u.Set("last_round_started_at", now)
		}
		switch {
		case !counted:
		case deref(pr.RoundsDay) == day:
			u.Inc("rounds_today", 1)
		default:
			u.Set("rounds_today", 1)
			u.Set("rounds_day", day)
		}
	})
	if err != nil {
		_ = e.st.TransitionSlot(ctx, job.slot.ID, []string{store.SlotBusy}, store.SlotHeld, nil)
		return &setupError{err: fmt.Errorf("PR to reviewing: %w", err), noCharge: true}
	}
	if kind != kindContinue {
		job.recordStart(day, now, pr.LastRoundStartedAt, counted)
	}
	return nil
}

// roundInput builds the pipeline input from the PR as it is now (reviewing):
// base ref and merge base, and for a re-review the previous review, its
// time and whether the head was force-pushed since.
func (e *Engine) roundInput(ctx context.Context, job *roundJob, rs roundSetup, ws agents.Workspace, dryRun bool) pipeline.RoundInput {
	cur, err := e.st.PRByID(ctx, job.pr.ID)
	if err != nil {
		cur = job.pr
	}
	base := prBase(job.repo, cur)
	in := pipeline.RoundInput{
		PR: cur, Repo: job.repo, SlotPath: job.slot.Path, Round: rs.round, Kind: rs.kind,
		TargetSHA: rs.target, BaseRef: base, Roles: rs.roles, Requested: rs.requested, MovedFrom: ws.MovedFrom,
		ContinueRunID: job.continueRunID, DryRun: dryRun, PostMerge: job.postMerge,
		NotesPath: e.roundNotes(job.repo), Readiness: e.readinessPlan(ctx, job, rs.kind), DeltaCheck: rs.delta, SameHead: rs.sameHead, Replies: rs.replies,
		OwnPass: e.cfg.JudgeOwnPassFor(&job.watch) == config.OwnPassParallel, Related: e.cfg.RelatedFor(&job.watch),
		ColdJudge: rs.coldJudge && rs.kind == pipeline.KindRecovery,
	}
	if job.evalHead != "" {
		in.Blind = true
		if !job.evalNotes {
			in.NotesPath = ""
		}
	}
	if rs.kind != kindContinue {
		in.ContinueRunID = ""
		in.MaxRestarts, in.DispatchedHead = e.cfg.Daemon.MaxRoundRestarts, job.pr.HeadSHA
		if job.evalHead != "" || job.postMerge || rs.judgeOnly() {
			// A pinned head, or a merged one, never moves; a delta check
			// measured its commits, and a same-head judge re-decides its
			// review of the head it checked out.
			in.MaxRestarts = 0
		}
		if job.kind == kindContinue {
			in.DispatchedHead = rs.target // a continue that became a full round kept the paused round's checkout
		}
		in.Switch = e.switchHead(job, rs, base, ws)
	}
	in.FormerLogins = e.formerLogins(ctx, cur)
	reviewed := ""
	if rev := deref(cur.ReviewedSHA); rev != "" && rs.kind != pipeline.KindInitial {
		reviewed = rev
		in.Previous = e.previousReview(ctx, cur, rs.src.Login(), in.FormerLogins)
		if cur.ReviewedAt != nil {
			in.Since = *cur.ReviewedAt
		}
	}
	in.BaseSHA, in.ForcePushed = e.headContext(ctx, job.slot.Path, base, reviewed, rs.target)
	if sinceReview(rs.kind) && job.evalHead == "" {
		in.BaseMerged = e.baseMerged(ctx, job, reviewed, rs.target)
	}
	if job.postMerge {
		in.BaseSHA = job.mergeBase // origin/<base> may hold the merged head: postMergeBase
	}
	return in
}

// sinceReview: a round of kind reviews the commits since the previous
// review (a re-review, or a recovery whose fresh judge rebuilds it), so its
// prompts are told when they merged the base branch (baseMerged).
func sinceReview(kind string) bool {
	return kind == pipeline.KindRereview || kind == pipeline.KindRecovery
}

// baseMerged reports whether the commits from reviewed to target have a
// merge commit (comparePush as the watch's poll identity, usually this
// tick's comparison triage or the rerun made): the push merged a branch
// in, usually the base, so the re-review prompts compare the PR's own diff
// before and after it (pipeline.RoundInput.BaseMerged). Unknown (no
// client, a failed call) is false.
func (e *Engine) baseMerged(ctx context.Context, job *roundJob, reviewed, target string) bool {
	if reviewed == "" || reviewed == target {
		return false
	}
	gh := e.gh(job.watch.PollIdentity)
	if gh == nil {
		return false
	}
	pc, err := e.comparePush(ctx, gh, job.repo, reviewed, target)
	if err != nil {
		e.log.Info("base merge: compare failed; the re-review reads the commits since the review", "pr", job.pr.ID, "err", err)
		return false
	}
	return pc.Merge
}

// headContext is target's merge base with the base branch ("" when unknown)
// and whether reviewed, the previous review's commit ("" = none), is no
// longer an ancestor of target (a force push).
func (e *Engine) headContext(ctx context.Context, slotPath, base, reviewed, target string) (baseSHA string, forcePushed bool) {
	if e.d.Git == nil {
		return "", false
	}
	if mb, err := e.d.Git.MergeBase(ctx, slotPath, "origin/"+strings.TrimPrefix(base, "origin/"), target); err == nil {
		baseSHA = mb
	}
	if reviewed != "" && reviewed != target {
		mb, err := e.d.Git.MergeBase(ctx, slotPath, reviewed, target)
		forcePushed = err != nil || mb != reviewed
	}
	return baseSHA, forcePushed
}

// switchHead is a round's RoundInput.Switch: a push arrived while its
// reviewers ran, so the round's slot checks the new head out through
// Slots.Checkout (whose guard knows magnum's own earlier switch). Checkout
// refuses a busy slot, so the slot is held for it and busy again after,
// whatever the outcome; the round's reservation keeps evictions and other
// rounds away meanwhile. The sessions that would reload the project config
// from the checkout are parked first (parkReloading) and those of the
// round's roles resumed after it, so the restart prompts them on the new
// head with what it allows.
func (e *Engine) switchHead(job *roundJob, rs roundSetup, base string, ws agents.Workspace) func(context.Context, string) (pipeline.Switched, error) {
	kind, workspaceID := rs.kind, ws.WorkspaceID
	return func(ctx context.Context, sha string) (pipeline.Switched, error) {
		if err := e.st.TransitionSlot(ctx, job.slot.ID, []string{store.SlotBusy}, store.SlotHeld, nil); err != nil {
			return pipeline.Switched{}, fmt.Errorf("slot %s to held for the checkout: %w", job.slot.Name, err)
		}
		defer func() {
			if err := e.st.TransitionSlot(context.WithoutCancel(ctx), job.slot.ID, []string{store.SlotHeld}, store.SlotBusy, nil); err != nil {
				e.log.Warn("restart: slot back to busy", "slot", job.slot.Name, "err", err)
			}
		}()
		pr, err := e.st.PRByID(ctx, job.pr.ID)
		if err != nil {
			return pipeline.Switched{}, err
		}
		var pool config.Pool
		if job.pool != nil {
			pool = *job.pool
		}
		parked, err := e.parkReloading(ctx, job)
		if err != nil {
			return pipeline.Switched{}, err
		}
		if err := e.d.Slots.Checkout(ctx, job.slot, pr, pool, sha); err != nil {
			return pipeline.Switched{}, fmt.Errorf("checkout in %s: %w", job.slot.Name, err)
		}
		// The restart prompts every role on the same session: resume the
		// ones parked for the checkout, which now decide with the new head.
		for _, role := range rs.toRun {
			if slices.Contains(parked, role.Name) {
				if _, err := e.ensureAgent(ctx, pr, role, ws, false, effortOf(kind)); err != nil {
					return pipeline.Switched{}, fmt.Errorf("resume %s after the checkout: %w", role.Name, err)
				}
			}
		}
		sl, err := e.st.SlotByID(ctx, job.slot.ID)
		if err != nil {
			return pipeline.Switched{}, err
		}
		job.slot = sl
		out := pipeline.Switched{TargetSHA: cmp.Or(deref(sl.CheckedOutSHA), sha)}
		reviewed := ""
		if kind != pipeline.KindInitial {
			reviewed = deref(pr.ReviewedSHA)
		}
		out.BaseSHA, out.ForcePushed = e.headContext(ctx, sl.Path, base, reviewed, out.TargetSHA)
		if sinceReview(kind) {
			out.BaseMerged = e.baseMerged(ctx, job, reviewed, out.TargetSHA)
		}
		e.sidebar(ctx, workspaceID, map[string]string{"magnum": "reviewing " + textx.ShortSHA(out.TargetSHA)})
		return out, nil
	}
}

// previousReview is the PR's last review as the pipeline's Previous: the
// commit and time of the latest magnum review (the re-review's diff base),
// with the review id and event only when that review was posted by login,
// the identity posting now, or by one of former, the logins the PR posted
// as before its identity migrated: their reviews are the round's own
// history, marked Former so the pipeline never dismisses them with its own
// credentials. After `magnum review --as <other>` the latest review may
// belong to another login; the newest review an own login posted stands in
// (or none).
func (e *Engine) previousReview(ctx context.Context, pr store.PR, login string, former []string) *pipeline.PreviousReview {
	prev := &pipeline.PreviousReview{ID: deref(pr.LastReviewID), Event: deref(pr.LastReviewEvent), SHA: deref(pr.ReviewedSHA),
		Login: deref(pr.LastReviewLogin)}
	if pr.ReviewedAt != nil {
		prev.SubmittedAt = *pr.ReviewedAt
	}
	prev.Former = slices.ContainsFunc(former, func(f string) bool { return github.SameAccount(prev.Login, f) })
	if v, ok := e.getKV(ctx, KVPRManualVerdict(pr.ID)); ok && prev.ID != 0 && v == strconv.FormatInt(prev.ID, 10) {
		prev.Manual = true
	}
	if prev.ID == 0 || login == "" {
		return prev
	}
	// A run records the login in its configured (REST) form, so a user "x"
	// and the App "x[bot]" are told apart: neither's review is the other's.
	isFormer := func(l string) bool {
		return !sameAccount(l, login) && slices.ContainsFunc(former, func(f string) bool { return sameAccount(l, f) })
	}
	own := func(l string) bool { return sameAccount(l, login) || isFormer(l) }
	runs, err := e.st.RunsByPR(ctx, pr.ID)
	if err != nil {
		return prev
	}
	var lastBy string
	var newest *store.Run
	for i := range runs {
		r := runs[i]
		if r.ReviewID == nil || *r.ReviewID == 0 {
			continue
		}
		if *r.ReviewID == prev.ID {
			lastBy = r.ReviewerLogin
		}
		if own(r.ReviewerLogin) && (newest == nil || *r.ReviewID > *newest.ReviewID) {
			newest = &r
		}
	}
	if lastBy == "" || own(lastBy) {
		if lastBy != "" {
			prev.Login, prev.Former = lastBy, isFormer(lastBy)
		}
		return prev // posted by an own login (or no record says otherwise)
	}
	prev.ID, prev.Event, prev.Login, prev.Former = 0, "", "", false
	if newest != nil {
		prev.ID, prev.Event, prev.Login, prev.Former = *newest.ReviewID, deref(newest.ReviewEvent), newest.ReviewerLogin, isFormer(newest.ReviewerLogin)
	}
	return prev
}

// preflight checks that the agent kinds are logged in; a logged-out kind is
// a setup failure that pauses it (not charged to the PR). A check that could
// not run is only logged.
func (e *Engine) preflight(ctx context.Context, kinds []string) *setupError {
	for _, kind := range kinds {
		err := e.d.Agents.Preflight(ctx, kind)
		switch {
		case err == nil:
		case isLogin(err):
			return &setupError{err: err, noCharge: true,
				pause: &pipeline.Pause{Kind: string(agents.HealthLoginRequired), Tool: kind, Detail: err.Error()}}
		default:
			e.log.Warn("preflight could not run", "tool", kind, "err", err)
		}
	}
	return nil
}

// startEffort is the effort a round starts its agents at (ensureAgent).
type startEffort int

const (
	// effortFull: every role at its effort.
	effortFull startEffort = iota
	// effortRereview (a re-review round): at the roles' rereview effort
	// (config.Role.EffortFor), except a judge without a conversation to
	// resume: its round becomes a recovery, which re-reads the history at
	// the full effort.
	effortRereview
	// effortCheck (a delta check that may run with a fresh judge,
	// checkFresh): the judge at its rereview effort, in a fresh session
	// too, since it reviews the delta alone either way.
	effortCheck
)

// effortOf is the start effort of a round of kind.
func effortOf(kind string) startEffort {
	if kind == pipeline.KindRereview {
		return effortRereview
	}
	return effortFull
}

// startRoles gives every role a pane in ws (EnsurePane for one the
// workspace lacks) and starts or resumes the agent roles' agents, the judge
// first, at effort (see startEffort; once the judge had to start fresh, the
// other roles start at their full effort: the round becomes a recovery).
// coldJudge starts the judge alone fresh (its prompt cache is cold,
// coldJudge): the others keep their conversations and effort, since they
// re-review as before (pipeline.RoundInput.ColdJudge). recovered is true
// when the judge started without a conversation to resume.
func (e *Engine) startRoles(ctx context.Context, pr store.PR, ws agents.Workspace, slotPath string, env map[string]string, roles []config.Role, fresh, coldJudge bool, effort startEffort) (agents.Workspace, bool, error) {
	recovered := false
	for _, role := range judgeFirst(roles) {
		if ws.Panes[agents.Role(role.Name)] == "" {
			var err error
			if ws, err = e.d.Agents.EnsurePane(ctx, pr, ws, slotPath, env, role); err != nil {
				return ws, false, fmt.Errorf("%s pane: %w", role.Name, err)
			}
		}
		if role.IsShell() {
			continue
		}
		freshStart, err := e.ensureAgent(ctx, pr, role, ws, fresh || role.Judge && coldJudge, effort)
		if err != nil {
			return ws, false, fmt.Errorf("start %s: %w", role.Name, err)
		}
		if role.Judge && freshStart {
			recovered = true
			if !coldJudge {
				effort = effortFull
			}
		}
	}
	return ws, recovered, nil
}

// judgeFirst is roles with the judge moved to the front.
func judgeFirst(roles []config.Role) []config.Role {
	out := make([]config.Role, 0, len(roles))
	for _, r := range roles {
		if r.Judge {
			out = append(out, r)
		}
	}
	for _, r := range roles {
		if !r.Judge {
			out = append(out, r)
		}
	}
	return out
}

// ensureAgent starts (or resumes, or adopts) an agent role's agent unless
// its session is already live. freshStart is true when the agent started
// without a conversation to resume (ResumeID is "" for a kind with
// session_source "none"). The agent starts at effort (startEffort). A live
// session keeps the effort it started with; the prompt asks for the round's
// (pipeline).
func (e *Engine) ensureAgent(ctx context.Context, pr store.PR, role config.Role, ws agents.Workspace, fresh bool, effort startEffort) (bool, error) {
	if s, err := e.st.LiveSessionByPRRole(ctx, pr.ID, role.Name); err == nil && s.State == store.SessionLive && deref(s.AgentName) != "" {
		return false, nil
	}
	pane := ws.Panes[agents.Role(role.Name)]
	if pane == "" {
		return false, fmt.Errorf("no pane for %s", role.Name)
	}
	resume := ""
	if !fresh {
		resume, _ = e.d.Agents.ResumeID(ctx, pr.ID, agents.Role(role.Name))
	}
	release, err := e.waitStart(ctx, role.AgentKind() == agents.KindCodex)
	if err != nil {
		return false, err
	}
	defer release()
	at := func(resume string) config.Role {
		r := role
		if effort == effortCheck || effort == effortRereview && (resume != "" || !role.Judge) {
			r.Effort = role.EffortFor(true)
		}
		return r
	}
	err = e.d.Agents.StartAgent(ctx, pr, at(resume), pane, resume)
	if err != nil && resume != "" {
		e.log.Warn("resume failed; starting fresh", "pr", pr.ID, "role", role.Name, "err", err)
		return true, e.d.Agents.StartAgent(ctx, pr, at(""), pane, "")
	}
	return resume == "", err
}

// paneEnv is the non-secret environment of the PR's workspace: the
// identity's gh selection, the pool's slot env (or, for a per-PR worktree,
// WT_BRANCH=magnum-pr-<N> with the outranking name variables blanked and the
// [[repo]] env, the same environment its setup hooks ran with) and MAGNUM_*
// context.
func (e *Engine) paneEnv(ctx context.Context, job *roundJob, src identity.Source, target string) (map[string]string, error) {
	env := map[string]string{}
	idEnv, err := src.Env(ctx)
	if err != nil {
		return nil, fmt.Errorf("identity %s env: %w", src.Name(), err)
	}
	maps.Copy(env, idEnv)
	maps.Copy(env, e.slotEnv(job))
	idCfg := e.cfg.IdentityByName(src.Name())
	env["MAGNUM_PR_URL"] = job.pr.URL
	env["MAGNUM_IDENTITY"] = src.Kind()
	env["MAGNUM_REVIEWER_LOGIN"] = src.Login()
	env["MAGNUM_SELF_LOGIN"] = e.selfLogin(job.watch)
	nf, be := e.cfg.VerdictsFor(job.repo.FullName(), idCfg) // [[repo]] override, else the identity's policy
	env["MAGNUM_NO_FINDINGS_EVENT"] = nf
	env["MAGNUM_BLOCKING_EVENT"] = be
	env["MAGNUM_REPORT_DIR"] = e.d.Layout.ReviewDir(job.repo.Owner, job.repo.Name, job.pr.Number, target)
	return env, nil
}

// slotEnv is the environment of the job's checkout: its pool's env for a
// pool slot, the [[repo]] env of a per-PR worktree (WT_BRANCH included).
func (e *Engine) slotEnv(job *roundJob) map[string]string {
	switch {
	case job.pool != nil && job.slot.Kind == store.SlotKindPool:
		return job.pool.SlotEnv(job.slot.Name)
	case job.slot.Kind == store.SlotKindPerPR:
		return slots.PerPREnv(e.cfg.RepoFor(job.repo.FullName()), job.pr.Number, job.slot.Path, job.slot.MainClone)
	}
	return nil
}

// readinessPlan is the round's readiness step: the repository's prepare
// commands and ready probes (config.Config.ReadinessFor), run with the
// checkout's environment (slotEnv), after the pool's reset_db when the pool
// slot's databases carry another schema than the checkout's
// (Slots.CheckSchema; a pool that is slots.LazySchema, so not with
// reset_db_on_schema_change off). The reset has the release's budget
// (slots.ResetDBTimeout), apart from ready_timeout; the slot's schema is
// unknown until it passed (ForgetSchema, then Loaded records the
// checkout's). A continue runs no readiness step, so no check either.
func (e *Engine) readinessPlan(ctx context.Context, job *roundJob, kind string) pipeline.ReadinessPlan {
	r := e.cfg.ReadinessFor(job.repo.FullName())
	plan := pipeline.ReadinessPlan{Prepare: r.Prepare, Ready: r.Ready, Timeout: r.Timeout, Env: e.slotEnv(job)}
	p := job.pool
	if p == nil || job.slot.Kind != store.SlotKindPool || kind == kindContinue || !slots.LazySchema(*p) || e.d.Slots == nil {
		return plan
	}
	slot := job.slot
	subject := "slot:" + slot.Name
	check, err := e.d.Slots.CheckSchema(ctx, slot, *p)
	if err != nil {
		e.log.Warn("schema check failed; reloading", "slot", slot.Name, "err", err)
		check = slots.SchemaCheck{Need: true, Why: "the schema could not be compared"}
	}
	if !check.Need {
		plan.SchemaNote = check.Why
		e.event(ctx, "info", subject, "slot.schema_unchanged", check.Why, map[string]any{"pr": prSubject(job.repo, job.pr.Number), "sha": check.SHA})
		return plan
	}
	if err := e.d.Slots.ForgetSchema(ctx, slot); err != nil {
		e.log.Warn("forget the slot's schema", "slot", slot.Name, "err", err)
	}
	plan.ResetDB, plan.ResetDBTimeout = p.ResetDB, slots.ResetDBTimeout
	plan.SchemaNote = "reloading the schema: " + check.Why
	plan.Loaded = func(ctx context.Context) {
		if err := e.d.Slots.RecordSchema(ctx, slot, check); err != nil {
			e.log.Warn("record the slot's schema", "slot", slot.Name, "err", err)
		}
	}
	return plan
}

// finalizeStaleRuns closes runs earlier rounds left behind (a crash, a
// pause): pending → abandoned, sent ones → failed. Without this they would
// count as active forever and block the close-grace release.
func (e *Engine) finalizeStaleRuns(ctx context.Context, prID int64) {
	runs, err := e.st.RunsByPR(ctx, prID)
	if err != nil {
		return
	}
	for _, r := range runs {
		switch r.State {
		case store.RunPending:
			_ = e.st.TransitionRun(ctx, r.ID, []string{store.RunPending}, store.RunAbandoned, nil)
		case store.RunSubmitted, store.RunWorking, store.RunEnded:
			sent := []string{store.RunSubmitted, store.RunWorking, store.RunEnded}
			_ = e.st.TransitionRun(ctx, r.ID, sent, store.RunFailed, func(u *store.RunUpdate) {
				u.Set("error", "superseded by a new round")
			})
		}
	}
}

// classifySetup maps a setup failure onto the retry policy.
func classifySetup(err error, pr store.PR, cfg *config.Config, now time.Time) *setupError {
	se := &setupError{err: err}
	switch {
	case errors.Is(err, agents.ErrHumanActive):
		se.noCharge = true
		if pr.HumanActiveAt != nil {
			se.retryAt = pr.HumanActiveAt.Add(cfg.Daemon.HumanCooldown.Duration)
		} else {
			se.retryAt = now.Add(cfg.Daemon.HumanCooldown.Duration)
		}
	case errors.Is(err, agents.ErrBusy):
		se.noCharge, se.retryAt = true, now.Add(busyRetry)
	case errors.Is(err, store.ErrConflict):
		se.noCharge, se.retryAt = true, now.Add(busyRetry)
	default:
		if _, ok := slots.AsHold(err); ok {
			se.retryAt = now.Add(holdRetry)
		}
	}
	return se
}

// checkoutFailed classifies a failed checkout: an infrastructure failure
// (checkoutInfra) is not the PR's, everything else goes through
// classifySetup.
func (e *Engine) checkoutFailed(err error, job *roundJob) *setupError {
	if cause := e.checkoutInfra(err, job.pr.ID, job.slot.Name, job.slot.Path); cause != "" {
		dir := cmp.Or(job.slot.MainClone, deref(job.repo.ClonePath))
		if dir == "" && job.pool != nil {
			dir = job.pool.MainClone
		}
		return &setupError{err: fmt.Errorf("infrastructure (%s): %w", cause, err), noCharge: true, infra: cause, probeDir: dir}
	}
	return classifySetup(err, job.pr, e.cfg, e.now())
}

// dryRunRound reports whether the PR's next round is a `magnum review
// --dry-run` round: its marker (kvPRDryRun) is set and the forced request
// that set it is still pending. A marker without a pending forced request is
// stale (its round ended) and is removed, so an automatic round never
// inherits it. A read error is returned: the caller must not start a round
// that might post.
func (e *Engine) dryRunRound(ctx context.Context, pr store.PR) (bool, error) {
	_, ok, err := e.st.GetKV(ctx, kvPRDryRun(pr.ID))
	if err != nil {
		return false, fmt.Errorf("read the dry-run marker: %w", err)
	}
	if ok && !pr.Forced {
		e.delKV(ctx, kvPRDryRun(pr.ID))
		return false, nil
	}
	return ok, nil
}

// sidebar publishes display-only workspace tokens (best effort).
func (e *Engine) sidebar(ctx context.Context, workspaceID string, tokens map[string]string) {
	if e.d.DryRun || e.d.Notifier == nil || workspaceID == "" {
		return
	}
	if err := e.d.Notifier.Sidebar(ctx, workspaceID, tokens); err != nil {
		e.log.Debug("sidebar tokens", "err", err)
	}
}

// sessionsIdentity is the identity the PR's sessions were created for: the
// kv written at workspace creation, else (sessions from before that key
// existed) the identity of the PR's latest run.
func (e *Engine) sessionsIdentity(ctx context.Context, prID int64) string {
	if v, _ := e.getKV(ctx, kvPRSessionsIdentity(prID)); v != "" {
		return v
	}
	runs, err := e.st.RunsByPR(ctx, prID)
	if err != nil || len(runs) == 0 {
		return ""
	}
	return runs[len(runs)-1].Identity // oldest first
}
