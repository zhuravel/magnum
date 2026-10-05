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
	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/identity"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
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
	// longer gives).
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
	if job.kind != kindContinue && job.evalHead == "" {
		rs.requested = append(rs.requested, e.rerunRoles(ctx, job, rs.roles, rs.target)...)
	}
	if rs.toRun, err = pipeline.RolesToRun(ctx, e.st, e.cfg, pr, rs.roles, rs.requested, job.kind); err != nil {
		return fail(err)
	}
	if len(rs.toRun) == 0 {
		return fail(fmt.Errorf("watch %s has no judge among its roles", job.watch.Owner))
	}
	// A small diff may not need every reviewer ([triage]): what is dropped
	// here is neither preflighted nor started.
	e.triage(ctx, job, &rs)
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

	in := e.roundInput(ctx, job, rs, ws, dryRun)
	e.sidebar(ctx, ws.WorkspaceID, map[string]string{"magnum": "reviewing " + short(rs.target)})
	names := roleNames(rs.toRun)
	what := rs.kind
	if job.postMerge {
		what = "post-merge " + what
	}
	e.event(ctx, "info", prSubject(job.repo, pr.Number), "engine.round_start",
		fmt.Sprintf("%s round in %s at %s: %s", what, job.slot.Name, short(rs.target), strings.Join(names, ", ")),
		map[string]any{"slot": job.slot.Name, "kind": rs.kind, "target_sha": rs.target, "roles": names, "requested": rs.requested,
			"post_merge": job.postMerge})
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
}

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
		e.event(ctx, "info", prSubject(job.repo, pr.Number), "round.head_moved", fmt.Sprintf("GitHub head moved to %s; reviewing that", short(target)), nil)
	}
	return target, nil
}

// startSessions gives the roles that run their panes (one workspace) and
// starts or resumes their agents, the judge first, then settles the round's
// kind in rs: a judge without its conversation re-reads the history
// (recovery, or initial before any review), and a continue that became a
// full round starts the other roles too.
func (e *Engine) startSessions(ctx context.Context, job *roundJob, rs *roundSetup) (agents.Workspace, *setupError) {
	var ws agents.Workspace
	fail := func(err error) (agents.Workspace, *setupError) {
		return ws, classifySetup(err, job.pr, e.cfg, e.now())
	}
	pr := job.pr
	fresh := false
	if v, _ := e.getKV(ctx, kvPRFresh(pr.ID)); v == "1" {
		fresh = true
	}
	// Sessions post as the identity their panes were created for; a PR whose
	// identity changed since (watch config, magnum review --as) must not
	// continue in them, or the review is posted by the wrong login.
	if v := e.sessionsIdentity(ctx, pr.ID); v != "" && v != pr.Identity {
		fresh = true
		e.event(ctx, "info", prSubject(job.repo, pr.Number), "pr.identity_changed",
			fmt.Sprintf("identity %s → %s: starting fresh sessions", v, pr.Identity), nil)
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
	recovered := false
	if ws, recovered, err = e.startRoles(ctx, pr, ws, job.slot.Path, env, rs.toRun, fresh, job.kind == pipeline.KindRereview); err != nil {
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
	if job.kind == kindContinue && rs.kind != kindContinue {
		// The continued round became a full one: the other roles that run
		// need their panes and agents too.
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
		if ws, _, err = e.startRoles(ctx, pr, ws, job.slot.Path, env, extra, fresh, rs.kind == pipeline.KindRereview); err != nil {
			return fail(err)
		}
	}
	return ws, nil
}

// enterReviewing moves the slot to busy and the PR from claiming to
// reviewing; a round other than a continue records its start for the
// throttle (last_round_started_at), and an automatic one (neither forced
// nor requested) counts against the daily cap (rounds_today), unless it
// continues a round that counted already (roundJob.continued).
func (e *Engine) enterReviewing(ctx context.Context, job *roundJob, kind string) *setupError {
	pr := job.pr
	if err := e.st.TransitionSlot(ctx, job.slot.ID, []string{store.SlotClaimed, store.SlotHeld, store.SlotBusy}, store.SlotBusy, nil); err != nil {
		return classifySetup(fmt.Errorf("slot %s to busy: %w", job.slot.Name, err), pr, e.cfg, e.now())
	}
	now := e.now()
	day := store.DayKey(now)
	counted := kind != kindContinue && !job.continued && !pr.Forced && !job.requested
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
		NotesPath: e.roundNotes(job.repo), Readiness: e.readinessPlan(job),
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
		if job.evalHead != "" || job.postMerge {
			in.MaxRestarts = 0 // a pinned head, or a merged one, never moves
		}
		if job.kind == kindContinue {
			in.DispatchedHead = rs.target // a continue that became a full round kept the paused round's checkout
		}
		in.Switch = e.switchHead(job, rs.kind, base, ws.WorkspaceID)
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
	if rs.kind == pipeline.KindRereview && job.evalHead == "" {
		in.BaseMerged = e.baseMerged(ctx, job, reviewed, rs.target)
	}
	if job.postMerge {
		in.BaseSHA = job.mergeBase // origin/<base> may hold the merged head: postMergeBase
	}
	return in
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
// rounds away meanwhile.
func (e *Engine) switchHead(job *roundJob, kind, base, workspaceID string) func(context.Context, string) (pipeline.Switched, error) {
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
		if err := e.d.Slots.Checkout(ctx, job.slot, pr, pool, sha); err != nil {
			return pipeline.Switched{}, fmt.Errorf("checkout in %s: %w", job.slot.Name, err)
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
		if kind == pipeline.KindRereview {
			out.BaseMerged = e.baseMerged(ctx, job, reviewed, out.TargetSHA)
		}
		e.sidebar(ctx, workspaceID, map[string]string{"magnum": "reviewing " + short(out.TargetSHA)})
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

// startRoles gives every role a pane in ws (EnsurePane for one the
// workspace lacks) and starts or resumes the agent roles' agents, the judge
// first (at their rereview effort for a re-review round, see ensureAgent,
// unless the judge had to start fresh: the round becomes a recovery).
// recovered is true when the judge started without a conversation to
// resume.
func (e *Engine) startRoles(ctx context.Context, pr store.PR, ws agents.Workspace, slotPath string, env map[string]string, roles []config.Role, fresh, rereview bool) (agents.Workspace, bool, error) {
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
		freshStart, err := e.ensureAgent(ctx, pr, role, ws, fresh, rereview)
		if err != nil {
			return ws, false, fmt.Errorf("start %s: %w", role.Name, err)
		}
		if role.Judge && freshStart {
			recovered, rereview = true, false
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
// session_source "none"). For a re-review round (rereview) the agent starts
// at the role's rereview effort (config.Role.EffortFor), except a judge
// without a conversation to resume: its round becomes a recovery, which
// re-reads the history at the full effort. A live session keeps the effort
// it started with; the prompt asks for the round's (pipeline).
func (e *Engine) ensureAgent(ctx context.Context, pr store.PR, role config.Role, ws agents.Workspace, fresh, rereview bool) (bool, error) {
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
		if rereview && (resume != "" || !role.Judge) {
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
// checkout's environment (slotEnv).
func (e *Engine) readinessPlan(job *roundJob) pipeline.ReadinessPlan {
	r := e.cfg.ReadinessFor(job.repo.FullName())
	return pipeline.ReadinessPlan{Prepare: r.Prepare, Ready: r.Ready, Timeout: r.Timeout, Env: e.slotEnv(job)}
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

// setupFailed puts the PR back in line (or in needs_attention after
// maxAttempts) after a failure before the pipeline ran. An infrastructure
// failure pauses dispatch (pauseInfra) and the PR, not charged, waits for
// the probe.
func (e *Engine) setupFailed(ctx context.Context, job *roundJob, se *setupError) {
	if se.pause != nil {
		e.pauseTool(ctx, *se.pause)
	}
	if se.infra != "" {
		se.retryAt = e.pauseInfra(ctx, se.infra, se.err, se.probeDir)
		e.refund(ctx, job, job.round, RefundInfra, "setup failed")
	}
	pr, err := e.st.PRByID(ctx, job.pr.ID)
	if err != nil {
		e.log.Warn("setup failed and PR is gone", "pr", job.pr.ID, "err", err)
		return
	}
	subject := prSubject(job.repo, pr.Number)
	e.event(ctx, "warn", subject, "engine.setup_failed", "round setup failed: "+se.err.Error(), nil)
	if job.hasSlo {
		// The slot stays the PR's, idle (held): claimed would keep it from
		// eviction while the PR waits for its retry.
		_ = e.st.TransitionSlot(ctx, job.slot.ID, []string{store.SlotClaimed}, store.SlotHeld, nil)
	}
	to := claimableState(pr)
	if job.kind == kindContinue {
		to = store.PRPaused
	}
	e.retryOrAttention(ctx, job, pr, []string{store.PRClaiming, store.PRReviewing}, to, se.err.Error(), !se.noCharge, se.retryAt)
	// Not charged is a wait (a person in the panes, a busy agent), not a
	// failure; an infrastructure pause has its own toast.
	if !se.noCharge && se.infra == "" {
		e.toastRequestedFailed(ctx, job, pr, "setup failed", se.err.Error()) // operator.go
	}
}

// retryOrAttention charges one attempt (when charge) and moves the PR to to
// with a backoff, or to needs_attention once the budget is spent.
func (e *Engine) retryOrAttention(ctx context.Context, job *roundJob, pr store.PR, from []string, to, msg string, charge bool, retryAt time.Time) {
	now := e.now()
	attempts := pr.Attempts
	if charge {
		attempts++
	}
	if retryAt.IsZero() && charge {
		retryAt = now.Add(backoff(attempts))
	}
	if attempts >= maxAttempts {
		e.needsAttention(ctx, job, pr, from, "failed", fmt.Sprintf("%d attempts on %s: %s", attempts, short(pr.HeadSHA), msg), func(u *store.PRUpdate) {
			u.Set("attempts", attempts)
		})
		return
	}
	err := e.st.TransitionPR(ctx, pr.ID, from, to, func(u *store.PRUpdate) {
		u.Set("attempts", attempts)
		u.Set("next_attempt_at", retryAt)
		u.Set("last_error", msg)
	})
	if err != nil {
		e.log.Info("PR moved on during the round", "pr", pr.ID, "err", err)
	}
}

// needsAttention parks the PR for a human and toasts. A post-merge round
// puts its PR back in closed instead (postMergeFailed).
func (e *Engine) needsAttention(ctx context.Context, job *roundJob, pr store.PR, from []string, why, msg string, extra func(*store.PRUpdate)) {
	if job.postMerge {
		e.postMergeFailed(ctx, job, pr, from, why, msg, extra)
		return
	}
	err := e.st.TransitionPR(ctx, pr.ID, from, store.PRNeedsAttention, func(u *store.PRUpdate) {
		u.Set("last_error", msg)
		u.Set("forced", false)
		u.Set("next_attempt_at", nil)
		if extra != nil {
			extra(u)
		}
	})
	if err != nil {
		e.log.Info("PR moved on during the round", "pr", pr.ID, "err", err)
		return
	}
	e.delKV(ctx, kvPRDryRun(pr.ID)) // the forced request ended with it
	e.setKV(ctx, KVPRAttention(pr.ID), why)
	if job.hasSlo {
		_ = e.st.TransitionSlot(ctx, job.slot.ID, []string{store.SlotClaimed, store.SlotBusy}, store.SlotHeld, nil)
	}
	label := fmt.Sprintf("%s#%d", job.repo.Name, pr.Number)
	key := fmt.Sprintf("attention:%d:%s", pr.ID, why)
	reason := attention.Explain(why, msg, label)
	e.event(ctx, "warn", prSubject(job.repo, pr.Number), "pr.needs_attention", why+": "+msg,
		map[string]any{"stage": reason.Stage, "cause": reason.Cause, "fix": reason.Fix})
	e.urgent(key, "magnum: "+label+" needs attention", reason.Summary, attentionWindow)
	e.revealAttention(ctx, pr.ID, nil, key)
}

// finish maps a round result onto the PR state machine.
func (e *Engine) finish(ctx context.Context, job *roundJob, in pipeline.RoundInput, ws agents.Workspace, res pipeline.RoundResult, runErr error, cancelled bool) {
	subject := prSubject(job.repo, job.pr.Number)
	if res.TargetSHA != "" {
		in.TargetSHA = res.TargetSHA // the head of the round's last restart
	}
	if err := e.st.TransitionSlot(ctx, job.slot.ID, []string{store.SlotBusy}, store.SlotHeld, nil); err != nil {
		// The next dispatch of the PR repairs a slot left busy (slotGate).
		e.log.Warn("round finished: slot back to held", "slot", job.slot.Name, "err", err)
	}
	e.delKV(ctx, kvPRFresh(job.pr.ID))
	pr, err := e.st.PRByID(ctx, job.pr.ID)
	if err != nil {
		e.log.Warn("round finished and PR is gone", "pr", job.pr.ID, "err", err)
		return
	}
	outcome := res.Outcome
	if outcome == "" {
		outcome = pipeline.OutcomeError
	}
	msg := res.Error
	if msg == "" && runErr != nil {
		msg = runErr.Error()
	}
	e.event(ctx, "info", subject, "engine.round_result", fmt.Sprintf("round %d: %s %s", res.Round, outcome, res.Event),
		map[string]any{"outcome": outcome, "event": res.Event, "review_id": res.ReviewID, "target_sha": in.TargetSHA,
			"restarts": res.Restarts, "error": msg})

	// Role health → pauses of the roles' agent kinds (the round itself may
	// have posted). A model limit never pauses a kind: the pipeline switched
	// models, and reports a usage limit when no fallback was left.
	paused := map[string]bool{}
	for _, role := range slices.Sorted(maps.Keys(res.Reports)) {
		rep := res.Reports[role]
		if rep.Status != string(agents.HealthLoginRequired) && rep.Status != string(agents.HealthUsageLimit) {
			continue
		}
		tool := e.reportKind(&job.watch, rep)
		if tool == "" || paused[tool] || (res.Pause != nil && res.Pause.Tool == tool) {
			continue
		}
		paused[tool] = true
		p := pipeline.Pause{Kind: rep.Status, Tool: tool, Detail: rep.Detail}
		if rep.Health != nil && rep.Health.ResetAt != nil {
			p.Until = *rep.Health.ResetAt
		}
		e.pauseTool(ctx, p)
	}
	if res.Pause != nil && res.Pause.Kind != string(agents.HealthOverloaded) && res.Pause.Kind != string(agents.HealthModelLimit) {
		e.pauseTool(ctx, *res.Pause) // overload is a per-PR backoff (below)
	}
	if why := e.refundReason(ctx, job, res, outcome, msg); why != "" && !e.turnContinues(ctx, job, pr, outcome, cancelled) {
		e.refund(ctx, job, res.Round, why, outcome)
	}
	if cancelled && outcome == pipeline.OutcomeStopped {
		e.log.Info("round stopped by shutdown; recovery re-evaluates it on the next start", "subject", subject)
		return
	}

	from := []string{store.PRReviewing, store.PRClaiming}
	now := e.now()
	switch outcome {
	case pipeline.OutcomeDryRun:
		e.onDryRun(ctx, job, pr, in, res, from)
	case pipeline.OutcomePosted:
		e.onPosted(ctx, job, pr, in, res, from)
	case pipeline.OutcomeUsageLimit, pipeline.OutcomeLoginRequired:
		e.toPaused(ctx, pr, from, outcome+": "+msg, time.Time{}, nil)
	case pipeline.OutcomeOverloaded:
		attempts := pr.Attempts + 1
		setAttempts := func(u *store.PRUpdate) { u.Set("attempts", attempts) }
		if attempts >= maxAttempts {
			e.needsAttention(ctx, job, pr, from, outcome, msg, setAttempts)
			break
		}
		e.toPaused(ctx, pr, from, outcome+": "+msg, now.Add(backoff(attempts)), setAttempts)
	case pipeline.OutcomeBlocked, pipeline.OutcomeNeedsAttention:
		e.needsAttention(ctx, job, pr, from, outcome, msg, nil)
	case pipeline.OutcomeIdentityError:
		e.setKV(ctx, KVIdentityCheck(pr.Identity), "fail")
		e.setKV(ctx, KVIdentityError(pr.Identity), "the judge's identity check failed: "+msg)
		e.needsAttention(ctx, job, pr, from, outcome, msg, nil)
	case pipeline.OutcomeIdentityLeak:
		// msg is the pipeline's sentence: review N carrying the run's marker was posted as "x", not "y".
		e.setKV(ctx, KVWatchPaused(job.repo.WatchOwner), fmt.Sprintf("identity leak on %s#%d: %s", job.repo.FullName(), pr.Number, msg))
		e.needsAttention(ctx, job, pr, from, outcome, msg, nil)
		e.urgent(fmt.Sprintf("leak:%d", pr.ID), "magnum: IDENTITY LEAK on "+job.repo.Name+fmt.Sprintf("#%d", pr.Number),
			msg+". Automation for "+job.repo.WatchOwner+" is paused (magnum resume --watch "+job.repo.WatchOwner+").", 0)
	case pipeline.OutcomeClosed:
		if job.postMerge { // merged: no poll will ever close it again
			e.postMergeFailed(ctx, job, pr, from, outcome, "the judge found the PR closed: "+msg, nil)
			break
		}
		// confirmMissing closes the PR if it really closed; until then it
		// waits closedRecheck in line (reviewed only when its head was).
		to := claimableState(pr)
		if deref(pr.ReviewedSHA) == pr.HeadSHA {
			to = store.PRReviewed
		}
		_ = e.st.TransitionPR(ctx, pr.ID, from, to, func(u *store.PRUpdate) {
			u.Set("next_attempt_at", now.Add(closedRecheck))
			u.Set("last_error", "the judge found the PR closed")
		})
	case pipeline.OutcomeStopped:
		e.retryOrAttention(ctx, job, pr, from, claimableState(pr), "round stopped: "+msg, false, now.Add(stopRetryDelay))
	default: // timeout, error
		if errors.Is(runErr, agents.ErrHumanActive) {
			e.retryOrAttention(ctx, job, pr, from, claimableState(pr), msg, false, classifySetup(runErr, pr, e.cfg, now).retryAt)
			break
		}
		e.retryOrAttention(ctx, job, pr, from, claimableState(pr), outcome+": "+msg, true, time.Time{})
	}
	e.requestedRoundFailed(ctx, job, pr, outcome, msg, runErr) // operator.go
	token := outcome
	if res.Event != "" {
		token = res.Event
	}
	e.sidebar(ctx, ws.WorkspaceID, map[string]string{"magnum": token + " " + short(in.TargetSHA)})
}

// onPosted records a verified review and queues the next one when the head
// moved during the round (noteMovedHead tells the review's readers). The PR
// becomes reviewed only while its head is still the reviewed commit (the
// transition is guarded on head_sha); a push the poller recorded after the
// read below fails that guard and the PR waits for a re-review of the new
// head instead. A post-merge review puts the PR back in closed, released
// after a fresh close grace: a merged head never moves, and the round's
// target is the merged head GitHub keeps (head_sha follows it when the
// poller never saw the last push).
func (e *Engine) onPosted(ctx context.Context, job *roundJob, pr store.PR, in pipeline.RoundInput, res pipeline.RoundResult, from []string) {
	now := e.now()
	target := in.TargetSHA
	to := store.PRReviewed
	if job.postMerge {
		to = store.PRClosed
	}
	// reviewed is the commit the review stands for: the round's target, or
	// the head that moved during the round when the delta is trivial
	// (comments, whitespace, docs, a base merge: no re-review follows).
	reviewed := target
	var trivial []string
	var settled deltaCheck
	var next time.Time
	if pr.HeadSHA != target && !job.postMerge {
		if dc := e.checkDelta(ctx, job.repo, job.watch, prBase(job.repo, pr), target, pr.HeadSHA); dc.trivial {
			reviewed, trivial, settled = pr.HeadSHA, dc.classes, dc
		} else {
			e.recordDelta(ctx, pr.ID, target, pr.HeadSHA, dc, pr.HeadChangedAt)
			to, next = store.PRRereviewPending, e.rereviewAt(ctx, pr, job.watch, target, now)
		}
	}
	// simplify_done (shown by the board) follows the role answering to
	// "simplify" (claude-simplify by default).
	simplified := false
	if r, ok := e.cfg.RoleByNameOrAlias(&job.watch, "simplify"); ok {
		if rep, ok := res.Reports[agents.Role(r.Name)]; ok {
			simplified = rep.Status == pipeline.ReportOK || rep.Status == pipeline.ReportEmpty || rep.Status == ""
		}
	}
	login := e.reviewerLogin(pr.Identity)
	recordReview := func(u *store.PRUpdate) {
		u.Set("reviewed_sha", reviewed)
		if res.ReviewID != 0 {
			u.Set("last_review_id", res.ReviewID)
		}
		if res.Event != "" {
			u.Set("last_review_event", res.Event)
		}
		u.Set("reviewed_at", now)
		u.Set("last_review_login", login)
	}
	// set reads pr, to and next when it runs: the head-moved retry below
	// reassigns them.
	set := func(u *store.PRUpdate) {
		if to == store.PRReviewed {
			u.Where("head_sha", reviewed)
		}
		recordReview(u)
		u.Set("attempts", 0)
		u.Set("next_attempt_at", nil)
		u.Set("last_error", nil)
		u.Set("forced", false)
		u.Set("skip_reason", nil)
		if simplified {
			u.Set("simplify_done", true)
		}
		if to == store.PRRereviewPending {
			u.Set("next_eligible_at", next)
			if pr.PendingSince == nil {
				u.Set("pending_since", pr.HeadChangedAt)
			}
		} else {
			u.Set("next_eligible_at", nil)
		}
		if job.postMerge {
			u.Set("release_after", e.releaseAfter())
			if pr.HeadSHA != reviewed {
				u.Set("head_sha", reviewed)
			}
		}
	}
	err := e.st.TransitionPR(ctx, pr.ID, from, to, set)
	if errors.Is(err, store.ErrConflict) && to == store.PRReviewed {
		// The head moved after the read above: this review covers an
		// older head, so the PR waits for a re-review of the new one.
		if cur, rerr := e.st.PRByID(ctx, pr.ID); rerr == nil && slices.Contains(from, cur.State) && cur.HeadSHA != reviewed {
			pr, to, next = cur, store.PRRereviewPending, e.rereviewAt(ctx, cur, job.watch, target, now)
			reviewed, trivial = target, nil
			err = e.st.TransitionPR(ctx, pr.ID, from, to, set)
		}
	}
	if err != nil {
		if !errors.Is(err, store.ErrConflict) {
			e.log.Warn("record review", "pr", pr.ID, "err", err)
			return
		}
		// The PR closed during the round: keep the review on record anyway.
		if err := e.st.UpdatePR(ctx, pr.ID, recordReview); err != nil {
			e.log.Warn("record review", "pr", pr.ID, "err", err)
		}
	} else if to == store.PRReviewed {
		e.requeueMovedHead(ctx, job.watch, pr.ID, now)
		if trivial != nil {
			e.recordTrivial(ctx, job.repo, pr, TrivialSkip{From: target, To: reviewed, Classes: trivial, Files: settled.files, At: now},
				fmt.Sprintf("the push to %s during the review %s since %s; no re-review, the review stands",
					short(reviewed), settled.change(), short(target)))
			e.noteMovedHead(ctx, job, pr, target, res.ReviewID, trivial)
		}
	} else if to == store.PRRereviewPending {
		e.noteMovedHead(ctx, job, pr, target, res.ReviewID, nil)
	} else {
		e.event(ctx, "info", prSubject(job.repo, pr.Number), "pr.post_merge_reviewed",
			fmt.Sprintf("post-merge review %s posted on %s; closed again, slot released after %s", reviewSummary(res), short(reviewed),
				e.cfg.Daemon.CloseGrace.Duration),
			map[string]any{"review_id": res.ReviewID, "event": res.Event, "reviewed_sha": reviewed, "previous_sha": deref(pr.ReviewedSHA)})
		if res.Event != "" && res.Event != "COMMENTED" { // the judge was asked for a comment: say so, undo nothing
			e.event(ctx, "warn", prSubject(job.repo, pr.Number), "pr.post_merge_event",
				fmt.Sprintf("post-merge review %d was posted as %s, not as a comment; it stands as posted", res.ReviewID, res.Event),
				map[string]any{"review_id": res.ReviewID, "event": res.Event})
		}
	}
	if trivial == nil {
		e.delKV(ctx, KVPRTrivial(pr.ID)) // a review of the head itself replaces the note
	}
	e.dismissFormer(ctx, job, pr, target, res)
	e.delKV(ctx, kvPRDryRun(pr.ID)) // the forced request is served
	// The request is served; the kinds that just worked lose their backoff.
	e.clearRequested(ctx, pr.ID)
	if k := e.cfg.JudgeFor(&job.watch).AgentKind(); k != "" {
		e.delKV(ctx, kvToolBackoff(k))
	}
	for _, role := range slices.Sorted(maps.Keys(res.Reports)) {
		rep := res.Reports[role]
		if rep.Status != pipeline.ReportOK {
			continue
		}
		if k := e.reportKind(&job.watch, rep); k != "" {
			e.delKV(ctx, kvToolBackoff(k))
		}
		if rep.Capture == config.CaptureGitDiff && rep.Path != "" {
			e.event(ctx, "info", prSubject(job.repo, pr.Number), "round.patch", fmt.Sprintf("%s produced a patch: %s", role, rep.Path),
				map[string]any{"role": string(role), "path": rep.Path})
		}
	}
	switch {
	case pr.Forced: // the operator asked for it: always toasted (operator.go)
		e.toastRequestedPosted(job, pr, res, now)
	case e.cfg.Herdr.ToastEveryReview && e.batch != nil:
		label := fmt.Sprintf("%s#%d", job.repo.Name, pr.Number)
		title := label + ": " + reviewSummary(res)
		e.batch.Add(notify.Item{
			Key:   fmt.Sprintf("%s#%d@%s", job.repo.FullName(), pr.Number, short(target)),
			Title: title, Body: "by " + e.reviewerLogin(pr.Identity), Line: title,
			Kind: notify.KindReviewPosted,
		})
	}
}

// rereviewAt is when a PR whose review covered target (not its head) may
// get its next round: eligibility.Throttle with target as the reviewed
// commit and the push quiet period (burst-aware) counted from the head's
// last change. The commits arrived during the review, so the re-review
// interval does not hold it (dispatch's backstop agrees,
// arrivedDuringReview); the daily cap still does.
func (e *Engine) rereviewAt(ctx context.Context, pr store.PR, w config.Watch, target string, now time.Time) time.Time {
	f := e.factsFor(pr, w, now)
	f.ReviewedSHA = target
	f.LastRoundStartedAt = time.Time{}
	if f.PendingSince.IsZero() {
		f.PendingSince = pr.HeadChangedAt
	}
	return e.throttle(ctx, w, pr, f, now).NextEligibleAt
}

// noteMovedHead appends a line to the review the round posted when commits
// arrived during it (GitHub's compare of the reviewed commit and the head
// counts them): which commit it covers and that a re-review follows, or,
// when the commits were trivial (classes, see TrivialDelta), what they
// changed and that none is needed. Best effort: an identity that cannot
// edit its own review leaves it as posted (a warning event).
func (e *Engine) noteMovedHead(ctx context.Context, job *roundJob, pr store.PR, target string, reviewID int64, trivial []string) {
	if e.d.DryRun || reviewID == 0 || e.d.Rounds == nil || pr.HeadSHA == target {
		return
	}
	rounds := e.d.Rounds(job.pr.Identity)
	if rounds == nil {
		return
	}
	// Only commits GitHub confirms: a head the poller has not caught up
	// with yet (the round fetched a newer one) adds none.
	gh := e.gh(job.watch.PollIdentity)
	if gh == nil {
		return
	}
	st, err := e.compareStats(ctx, gh, job.repo, target, pr.HeadSHA) // checkDelta's comparison, when it made one
	if err != nil || st.Commits == 0 {
		if err != nil {
			e.log.Info("note on the review: compare", "pr", pr.ID, "err", err)
		}
		return
	}
	what, are := fmt.Sprintf("%d commits", st.Commits), "are"
	if st.Commits == 1 {
		what, are = "1 commit", "is"
	}
	// The re-review is promised only when it starts by itself (rereviewFollows).
	follows := "re-review follows"
	line := fmt.Sprintf("_Reviewed %s; %s arrived during the review, re-review follows._", short(target), what)
	if trivial != nil {
		follows = DeltaLabel(trivial) + ", no re-review needed"
		line = fmt.Sprintf("_Reviewed %s; %s arrived during the review (%s), no re-review needed._", short(target), what, DeltaLabel(trivial))
	} else if when, ok := e.rereviewFollows(ctx, job.watch, pr.ID); !ok {
		follows = "not reviewed yet"
		line = fmt.Sprintf("_Reviewed %s; %s arrived during the review and %s not reviewed yet._", short(target), what, are)
	} else if when != "" {
		follows += " " + when
		line = fmt.Sprintf("_Reviewed %s; %s arrived during the review, re-review follows %s._", short(target), what, when)
	}
	subject := prSubject(job.repo, pr.Number)
	if err := rounds.AppendToReview(ctx, job.repo.Owner, job.repo.Name, pr.Number, reviewID, line); err != nil {
		e.event(ctx, "warn", subject, "round.review_note_failed",
			fmt.Sprintf("review %d: could not add that %s arrived during it: %v", reviewID, what, err), nil)
		return
	}
	e.event(ctx, "info", subject, "round.review_noted", fmt.Sprintf("review %d: %s arrived during it, %s", reviewID, what, follows),
		map[string]any{"review_id": reviewID, "reviewed_sha": target, "head_sha": pr.HeadSHA, "trivial": trivial})
}

// rereviewFollows reports whether the re-review of the commits that arrived
// during a review starts by itself, from the PR's wait as it is right after
// the review was recorded (waitFor), and when: "" at the next dispatch,
// "after the quiet period" when the push quiet period (or its burst form)
// holds it. It does not when a longer timing rule holds the PR (the daily
// cap, the small-delta threshold, an interval), or when something would hold
// it once its timing clears: `magnum pause`, a drain or an infrastructure
// pause, quiet hours, a mute, an agent kind the watch's roles use paused.
func (e *Engine) rereviewFollows(ctx context.Context, w config.Watch, prID int64) (string, bool) {
	pr, err := e.st.PRByID(ctx, prID)
	if err != nil || (pr.State != store.PRRereviewPending && pr.State != store.PRQueued) {
		return "", false
	}
	now := e.now()
	wait := e.waitFor(ctx, pr, e.globalWait(ctx, tickState{herdrUp: true}, now), now)
	when := ""
	switch wait.Reason {
	case WaitNext, WaitRequested, WaitCapacity:
	case WaitQuiet, WaitBurst:
		when = "after the quiet period"
	default:
		return "", false
	}
	at := now // when the quiet period ends (quiet hours then hold it)
	if wait.Until.After(at) {
		at = wait.Until
	}
	// The roles dispatch would run (their kinds' pauses hold the round).
	roles := e.cfg.RolesFor(&w)
	if toRun, err := pipeline.RolesToRun(ctx, e.st, e.cfg, pr, roles, e.requestedRoles(ctx, pr.ID), kindFor(pr)); err == nil {
		roles = toRun
	}
	switch {
	case e.holdReason(ctx) != "":
	case !pr.Forced && (e.userPause(ctx) != "" || pr.Muted || quietHoursNow(e.cfg.Daemon.QuietHours, at)):
	case e.kindPauseReason(ctx, agentKinds(roles)) != "":
	default:
		return when, true
	}
	return "", false
}

// requeueMovedHead is the backstop after onPosted's transition to reviewed
// (whose head_sha guard already sends a head moved before it to
// rereview_pending): a reviewed PR whose head is not the reviewed commit
// goes to rereview_pending (a lost compare-and-set means the poller queued
// it already).
func (e *Engine) requeueMovedHead(ctx context.Context, w config.Watch, prID int64, now time.Time) {
	cur, err := e.st.PRByID(ctx, prID)
	if err != nil || cur.State != store.PRReviewed || cur.HeadSHA == deref(cur.ReviewedSHA) {
		return
	}
	if err := e.queue(ctx, cur, w, []string{store.PRReviewed}, false, now, "head moved during the round"); err != nil {
		e.log.Info("requeue a head that moved during the round", "pr", prID, "err", err)
	}
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

// reportKind is the agent kind a role report's health concerns: its Kind,
// else the configured role's (config.Role.AgentKind); "" = none.
func (e *Engine) reportKind(w *config.Watch, rep pipeline.RoleReport) string {
	if rep.Kind != "" {
		return rep.Kind
	}
	if r, ok := e.cfg.RoleByNameOrAlias(w, rep.Role); ok && rep.Role != "" {
		return r.AgentKind()
	}
	return ""
}

// onDryRun ends a dry-run round (review request with dry_run): nothing was
// posted, so reviewed_sha stays and the PR returns to the state it had
// before the request (its claimable state when that was in flight, the head
// moved meanwhile, or reviewed no longer describes it). A post-merge dry run
// returns to closed, released after a fresh close grace.
func (e *Engine) onDryRun(ctx context.Context, job *roundJob, pr store.PR, in pipeline.RoundInput, res pipeline.RoundResult, from []string) {
	to, _ := e.getKV(ctx, kvPRDryRun(pr.ID))
	e.delKV(ctx, kvPRDryRun(pr.ID))
	switch {
	case job.postMerge:
		to = store.PRClosed
	case to == "" || pr.HeadSHA != in.TargetSHA || to == store.PRPaused ||
		slices.Contains(store.InFlightStates, to) || slices.Contains(closingStates, to) ||
		(to == store.PRReviewed && deref(pr.ReviewedSHA) != pr.HeadSHA):
		to = claimableState(pr)
		if rs := deref(pr.ReviewedSHA); rs != "" && rs == pr.HeadSHA {
			to = store.PRReviewed
		}
	}
	err := e.st.TransitionPR(ctx, pr.ID, from, to, func(u *store.PRUpdate) {
		u.Set("forced", false)
		u.Set("attempts", 0)
		u.Set("next_attempt_at", nil)
		u.Set("last_error", nil)
		if job.postMerge {
			u.Set("release_after", e.releaseAfter())
		}
	})
	if err != nil {
		e.log.Info("PR moved on during the dry-run round", "pr", pr.ID, "err", err)
	}
	e.event(ctx, "info", prSubject(job.repo, pr.Number), "round.dry_run",
		fmt.Sprintf("dry run at %s: would post %s; nothing was posted (PR back to %s)", short(in.TargetSHA), reviewSummary(res), to),
		map[string]any{"event": res.Event, "findings": res.Findings, "report_dir": res.ReportDir})
}

// toPaused parks a PR whose round hit a tool pause; it continues (kind
// continue) once the pause ends. extra adds assignments to the same
// transition.
func (e *Engine) toPaused(ctx context.Context, pr store.PR, from []string, msg string, retryAt time.Time, extra func(*store.PRUpdate)) {
	err := e.st.TransitionPR(ctx, pr.ID, from, store.PRPaused, func(u *store.PRUpdate) {
		u.Set("last_error", msg)
		u.Set("next_attempt_at", retryAt)
		if extra != nil {
			extra(u)
		}
	})
	if err != nil {
		e.log.Info("PR moved on during the round", "pr", pr.ID, "err", err)
	}
}

func (e *Engine) reviewerLogin(identityName string) string {
	if id := e.cfg.IdentityByName(identityName); id != nil {
		return id.Login
	}
	return identityName
}

// reviewSummary is "CHANGES_REQUESTED (2 P2)" for a toast.
func reviewSummary(res pipeline.RoundResult) string {
	var parts []string
	for _, k := range []string{"P0", "P1", "P2", "P3"} {
		if n := res.Findings[k]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, k))
		}
	}
	ev := res.Event
	if ev == "" {
		ev = res.Outcome
	}
	if len(parts) == 0 {
		return ev
	}
	return ev + " (" + strings.Join(parts, ", ") + ")"
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
