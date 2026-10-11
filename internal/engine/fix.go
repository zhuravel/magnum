package engine

// The fix job: a fixer agent fixes magnum's own findings on the operator's
// PR, in the PR's slot and as the operator, and the PR then gets a forced
// re-review of what it pushed. `magnum fix <ref>` asks for it (ReqFix). The
// job holds the PR with a reservation of its own (roundHandle.fix) for its
// whole run, so no round, open or eviction touches the slot meanwhile, and
// keeps where the loop is in the PR's KVPRFix row (fix_state.go): while the
// fixer works (phase fixing) dispatch holds the PR's rounds (fixHolds,
// WaitFixing), and while the row exists `magnum review` and a second loop
// are refused. The job's steps are begin/ok/fail steps on the PR's subject
// (package steps): guard (the slot is the PR's, nobody else is in it, it
// holds the reviewed head), start (the fixer's session in the PR's
// workspace, a run of kind fix, its prompt) and wait (the run's end; the
// time-up line on timeout); the settle (fix_settle.go) reads the report,
// puts the slot back in order and ends the cycle. A daemon that stops while
// the fixer works leaves the row in phase fixing, and the next start ends
// that loop (recoverFixes).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/steps"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// ReqFix starts a fix loop on a PR, or switches the fix job off or on for
// it (FixPayload).
const ReqFix = "fix"

// FixPayload is a `magnum fix` request: start a fix loop on the PR (DryRun:
// answer what would happen and do nothing), or switch the fix job off for it
// (Off: autofix leaves it alone, KVPRFixOff) or on again (On). Neither Off
// nor On starts a loop.
type FixPayload struct {
	PRTarget
	DryRun bool `json:"dry_run,omitempty"`
	Off    bool `json:"off,omitempty"`
	On     bool `json:"on,omitempty"`
}

const (
	// fixBy is who starts a loop asked for by hand (FixState.By) and the
	// re-review it forces.
	fixBy = "magnum fix"
	// fixOffBy is who switches the fix job off (FixOff.By).
	fixOffBy = "magnum fix --off"
	// fixPoll is how often the job looks at its run while the fixer works.
	fixPoll = pipeline.DefaultPollInterval
)

// fixTimeUpText is the last call to a fixer whose time ran out: its budget,
// its report file and the marker the report starts with.
const fixTimeUpText = "Time is up: this fix had %s. Stop every background task you started with TaskStop and start nothing new. " +
	"Write the report to %s now (its first line: `%s`) with what you pushed and replied so far, put every finding you did not finish under `## Needs you`, and end your turn."

// errFixTimeout is a fix run that did not end within its time.
var errFixTimeout = errors.New("fix run timed out")

// errFixLoopGone refuses a next cycle whose loop is no longer the one waiting
// for its re-review (a stop ended it, or another cycle took it on): nobody
// asked for that cycle any more, so the hook lets it go quietly.
var errFixLoopGone = errors.New("the fix loop is no longer waiting for its re-review")

// errFixDraining refuses a fix cycle while the daemon drains for a restart,
// which would end its fixer ("fix run lost in a daemon restart"); a loop
// waiting for its next cycle keeps its row for the restart's recovery.
var errFixDraining = errors.New("the daemon is draining; try after the restart")

// fixJob is one cycle of a fix loop: the PR, its watch and slot, the fix
// role, the review the fixer fixes and the loop's row.
type fixJob struct {
	repo   store.Repo
	watch  config.Watch
	pool   *config.Pool
	pr     store.PR
	slot   store.Slot
	role   config.Role
	review store.ReviewSummary // the review the run fixes: its run, head, URL and findings
	round  int                 // the review's round, the fix run's
	login  string              // the login that posted the review, whose threads the fixer answers
	notes  []string            // what the request's answer adds ("this watch posts as you: ...")
	state  FixState            // the loop's row as the job writes it
	run    store.Run           // the fixer's run, once fixStart created it
	// touched: the fixer may have changed the slot (its prompt was sent), so
	// the settle and a stop put the slot back in order.
	touched bool
}

func (j *fixJob) label() string   { return fmt.Sprintf("%s#%d", j.repo.FullName(), j.pr.Number) }
func (j *fixJob) subject() string { return prSubject(j.repo, j.pr.Number) }

// asRound is the job as a roundJob, for the helpers rounds share with it
// (paneEnv, needsAttention).
func (j *fixJob) asRound() *roundJob {
	return &roundJob{pr: j.pr, repo: j.repo, watch: j.watch, pool: j.pool, slot: j.slot, hasSlot: j.slot.ID != 0}
}

// findings is the review's findings in words, "2 findings", or "open
// earlier findings" for a next cycle's review that requests changes with no
// new one (fixRemains).
func (j *fixJob) findings() string {
	if n := j.review.Findings(); n > 0 {
		return textx.Count(n, "finding", "findings")
	}
	return "open earlier findings"
}

// fixRunningSentence is the refusal while a loop runs on the PR.
func fixRunningSentence(st FixState) string {
	return fmt.Sprintf("fixing, cycle %d of %d; `magnum abort` stops it", st.Cycle, st.Cap)
}

// requestFix handles `magnum fix`: --off and --on set and clear the PR's
// opt-out of autofix; otherwise a fix loop starts (startFix) unless one of
// prepareFix's refusals applies, and the answer says what runs, with the
// notes that concern it. A dry run (the request's or the daemon's) answers
// what would start.
func (e *Engine) requestFix(ctx context.Context, p FixPayload) (string, error) {
	repo, pr, err := e.resolve(ctx, p.PRTarget)
	if err != nil {
		return "", err
	}
	subject := prSubject(repo, pr.Number)
	switch {
	case p.Off && p.On:
		return "", errors.New("magnum fix: --off and --on together")
	case p.Off:
		if p.DryRun {
			return fmt.Sprintf("dry run: would switch fix off for PR #%d", pr.Number), nil
		}
		if err := e.setFixOff(ctx, pr.ID, FixOff{Since: e.now().UTC(), By: fixOffBy}); err != nil {
			return "", err
		}
		e.event(ctx, "info", subject, "fix.off", "fix switched off by "+fixOffBy+": autofix leaves the PR alone", map[string]any{"by": fixOffBy})
		return fmt.Sprintf("fix is off for PR #%d: autofix leaves it to you; `magnum fix --on` lifts it", pr.Number), nil
	case p.On:
		if p.DryRun {
			return fmt.Sprintf("dry run: would switch autofix on again for PR #%d", pr.Number), nil
		}
		if err := e.clearFixOff(ctx, pr.ID); err != nil {
			return "", err
		}
		e.event(ctx, "info", subject, "fix.on", "fix switched on again by magnum fix --on", nil)
		return fmt.Sprintf("autofix is on again for PR #%d", pr.Number), nil
	}
	_, off, err := e.fixOff(ctx, pr.ID)
	if err != nil {
		return "", err
	}
	var job *fixJob
	if p.DryRun || e.d.DryRun {
		job, err = e.prepareFix(ctx, pr, FixState{})
	} else {
		job, err = e.startFix(ctx, pr, fixBy, FixState{})
	}
	if err != nil {
		return "", err
	}
	what := fmt.Sprintf("%s fixes the %s of the review on %s in %s, cycle %d of %d", job.role.Name, job.findings(),
		textx.ShortSHA(job.review.SHA), job.slot.Name, max(job.state.Cycle, 1), job.state.Cap)
	res := fmt.Sprintf("fixing %s: %s; `magnum abort %s` stops it", job.label(), what, job.label())
	if p.DryRun || e.d.DryRun {
		e.rec.Record(ctx, subject, "fix", what)
		res = fmt.Sprintf("dry run: would start fixing %s: %s, then a forced re-review of what it pushes", job.label(), what)
	}
	notes := job.notes
	if off {
		notes = append(notes, "fix is off for this PR: this requested loop runs anyway")
	}
	for _, n := range notes {
		res += "; " + n
	}
	return res, nil
}

// prepareFix decides whether a fix cycle may start on pr and gathers what it
// needs; the error is the refusal, a sentence for the request's answer. prev
// is the loop a next cycle continues (zero for a new loop, which a loop
// already running refuses).
func (e *Engine) prepareFix(ctx context.Context, pr store.PR, prev FixState) (*fixJob, error) {
	repo, err := e.st.RepoByID(ctx, pr.RepoID)
	if err != nil {
		return nil, err
	}
	full := repo.FullName()
	label := fmt.Sprintf("%s#%d", full, pr.Number)
	if e.draining(ctx) {
		return nil, errFixDraining
	}
	w := e.cfg.WatchFor(full)
	if w == nil {
		return nil, fmt.Errorf("%s is not watched (no [[watch]] covers it)", full)
	}
	role, ok := e.cfg.FixRoleFor(w)
	if !ok {
		return nil, fmt.Errorf("no fix role for %s", full)
	}
	fixer := e.cfg.IdentityByName(role.Identity)
	if fixer == nil {
		return nil, fmt.Errorf("no identity %s for the fix role", role.Identity)
	}
	cur, running, err := e.fixState(ctx, pr.ID)
	switch {
	case err != nil:
		return nil, err // unreadable: never a second loop over it
	case running && prev.Cycle == 0:
		return nil, errors.New(fixRunningSentence(cur))
	case prev.Cycle > 0 && !(running && sameWaitingLoop(cur, prev)):
		return nil, fmt.Errorf("%s: %w", label, errFixLoopGone)
	}
	switch author := deref(pr.AuthorLogin); {
	case author == "":
		return nil, fmt.Errorf("PR #%d's author is not known yet (no details from GitHub); the fixer pushes and replies as you", pr.Number)
	case !e.ownPR(pr):
		return nil, fmt.Errorf("PR #%d is %s's, not yours; the fixer pushes and replies as you", pr.Number, author)
	}
	if pr.IsCrossRepo {
		return nil, errors.New("the head lives in a fork magnum cannot push to")
	}
	if pr.GHState != store.GHOpen {
		return nil, fmt.Errorf("%s is not open (GitHub %s): nothing to fix", label, pr.GHState)
	}
	if slices.Contains(store.InFlightStates, pr.State) || pr.State == store.PRPaused || e.roundActive(pr.ID) {
		return nil, fmt.Errorf("%s: a round is already in progress (%s)", label, pr.State)
	}
	sums, err := e.st.LastReviewSummaries(ctx, []int64{pr.ID})
	if err != nil {
		return nil, err
	}
	head := textx.ShortSHA(pr.HeadSHA)
	sum, ok := sums[pr.ID]
	switch {
	case !ok || sum.SHA != pr.HeadSHA || deref(pr.ReviewedSHA) != pr.HeadSHA:
		return nil, fmt.Errorf("nothing to fix: no review on %s", head)
	case sum.Findings() == 0 && prev.Cycle == 0: // a next cycle's review may keep earlier findings open with no new one (fixRemains)
		return nil, fmt.Errorf("nothing to fix: the review on %s found nothing", head)
	case sum.URL == "":
		return nil, fmt.Errorf("%s: the review on %s has no URL to fix from", label, head)
	case deref(pr.HeadRef) == "":
		return nil, fmt.Errorf("%s: its branch is not known yet (no details from GitHub)", label)
	}
	src := e.d.Identities[pr.Identity]
	if src == nil || src.Login() == "" {
		return nil, fmt.Errorf("identity %q is not configured", pr.Identity)
	}
	reviewRun, err := e.st.RunByID(ctx, sum.RunID)
	if err != nil {
		return nil, fmt.Errorf("%s: the review's run %s: %w", label, sum.RunID, err)
	}
	slot, has, err := e.slotOf(ctx, pr.ID)
	switch {
	case err != nil:
		return nil, err
	case !has:
		return nil, fmt.Errorf("%s has no checkout of its reviewed head: its slot was handed back", label)
	case slot.State != store.SlotClaimed && slot.State != store.SlotHeld:
		return nil, fmt.Errorf("%s: slot %s is %s", label, slot.Name, slot.State)
	case slot.Pinned:
		return nil, fmt.Errorf("%s: slot %s is pinned (magnum unpin)", label, slot.Name)
	case slot.HoldReason != nil:
		return nil, fmt.Errorf("%s: %s", label, slotHeldReason(slot))
	}
	now := e.now()
	if s, ok := e.snoozeOf(ctx, pr.ID, now); ok {
		return nil, fmt.Errorf("%s is snoozed until %s; `magnum snooze %s --off` lifts it", label, waitClock(s.Until, now), label)
	}
	if until := e.cfg.Daemon.CooldownUntil(pr.HumanActiveAt); now.Before(until) {
		return nil, fmt.Errorf("%s: someone typed into its panes; run `magnum fix %s` again after %s (human_cooldown)", label, label, waitClock(until, now))
	}
	if why := e.kindPauseReason(ctx, []string{role.AgentKind()}); why != "" {
		return nil, fmt.Errorf("%s: %s", label, why)
	}
	job := &fixJob{repo: repo, watch: *w, pool: e.cfg.PoolFor(full), pr: pr, slot: slot, role: role, review: sum,
		round: reviewRun.Round, login: src.Login(), state: prev}
	if job.state.Cap == 0 {
		job.state.Cap = e.cfg.FixCyclesFor(w)
	}
	if github.SameAccount(src.Login(), fixer.Login) {
		job.notes = append(job.notes, "this watch posts as you: the re-review will not read the fixer's replies as yours")
	}
	return job, nil
}

// startFix starts a fix cycle on pr under prepareFix's refusals: a new loop
// (prev zero, started by by: "magnum fix", "autofix") at cycle 1 of the
// watch's fix_cycles, or the next cycle of the loop prev waits in. The job
// reserves the PR (roundHandle.fix), writes the loop's row in phase fixing
// and runs on a goroutine of its own under the reservation (runFix), whose
// context derives from ctx: pass the daemon's, never a round's (its
// reservation's end cancels it). It returns a copy of the job as it
// started, for the caller's answer (the job's own goes on changing).
func (e *Engine) startFix(ctx context.Context, pr store.PR, by string, prev FixState) (*fixJob, error) {
	job, err := e.prepareFix(ctx, pr, prev)
	if err != nil {
		return nil, err
	}
	if e.d.Agents == nil || e.d.Slots == nil {
		return nil, errors.New("the daemon has no agents or slots manager")
	}
	st := job.state
	if prev.Cycle == 0 {
		st = FixState{Cycle: 1, Cap: job.state.Cap, Since: job.review.SHA, By: by, StartedAt: e.now().UTC()}
	} else {
		st.Cycle++
	}
	st.Phase, st.Head, st.Report, st.Role, st.NeedsYou = FixPhaseFixing, job.review.SHA, "", job.role.Name, nil
	job.state = st
	rctx, cancel := context.WithCancel(ctx)
	if !e.reserve(pr.ID, &roundHandle{cancel: cancel, fix: true}) {
		cancel()
		return nil, fmt.Errorf("%s: %s; run `magnum fix %s` again shortly", job.label(), e.heldReason(pr.ID), job.label())
	}
	if err := e.claimFixRow(ctx, job, prev, st); err != nil {
		e.unreserve(pr.ID)
		return nil, err
	}
	started := *job
	e.roundWG.Go(func() {
		defer e.unreserve(pr.ID)
		e.safely(rctx, "fix", job.subject(), func() { e.runFix(rctx, job) },
			func(ctx context.Context, msg string) { e.fixPanicked(ctx, job, msg) })
	})
	return &started, nil
}

// claimFixRow writes the cycle's row (st, phase fixing) once the PR is
// reserved, compare-and-set against what a writer without the reservation
// may have done since prepareFix looked: `magnum abort` on a reviewed PR
// ends a loop waiting for its re-review without one (endFixLoop). A new loop
// needs the PR still without a row; a next cycle needs the row still to be
// prev's loop waiting at prev's cycle, and replaces that very row
// (Store.SetKVIf), or it is refused with errFixLoopGone. From the
// reservation on, a stop goes to the job instead.
func (e *Engine) claimFixRow(ctx context.Context, job *fixJob, prev, st FixState) error {
	cur, raw, ok, err := e.fixRow(ctx, job.pr.ID)
	switch {
	case err != nil:
		return err
	case prev.Cycle == 0 && ok:
		return errors.New(fixRunningSentence(cur))
	case prev.Cycle == 0:
		return e.setFixState(ctx, job.pr.ID, st)
	case !ok || !sameWaitingLoop(cur, prev):
		return fmt.Errorf("%s: %w", job.label(), errFixLoopGone)
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	swapped, err := e.st.SetKVIf(ctx, KVPRFix(job.pr.ID), raw, string(b))
	switch {
	case err != nil:
		return err
	case !swapped:
		return fmt.Errorf("%s: %w", job.label(), errFixLoopGone)
	}
	return nil
}

// sameWaitingLoop reports whether cur is still the loop prev was read as,
// waiting for its re-review at the same cycle.
func sameWaitingLoop(cur, prev FixState) bool {
	return cur.Phase == FixPhaseReviewing && cur.Cycle == prev.Cycle && cur.StartedAt.Equal(prev.StartedAt)
}

// runFix runs one fix cycle under its reservation: guard, start and wait,
// then the settle (fix_settle.go). `magnum abort` cancels ctx and leaves
// its orders on the reservation: the fixer is stopped and the loop ends
// quietly (stopFix). A daemon shutdown cancels ctx with no order: the
// loop's row stays in phase fixing, which the next start ends
// (recoverFixes).
func (e *Engine) runFix(ctx context.Context, job *fixJob) {
	subject := job.subject()
	if err := steps.ResetSubject(ctx, e.st, subject); err != nil {
		e.log.Warn("fix: new step generation", "subject", subject, "err", err)
	}
	failed := ""
	err := steps.Step(ctx, e.st, subject, "fix.guard", func(ctx context.Context) error { return e.fixGuard(ctx, job) })
	if err != nil {
		failed = "fix guard failed: "
	} else if err = steps.Step(ctx, e.st, subject, "fix.start", func(ctx context.Context) error { return e.fixStart(ctx, job) }); err != nil {
		failed = "fix run failed: "
	} else if err = steps.Step(ctx, e.st, subject, "fix.wait", func(ctx context.Context) error { return e.fixWait(ctx, job) }); err != nil {
		failed = "fix run failed: "
	}
	fctx := context.WithoutCancel(ctx)
	if orders := e.closeFixHandle(job.pr.ID); len(orders) > 0 {
		e.stopFix(fctx, job, orders)
		return
	}
	if ctx.Err() != nil {
		e.log.Info("fix interrupted by a shutdown; the next start ends its loop", "subject", subject)
		return
	}
	if err != nil {
		msg := failed + err.Error()
		if job.touched {
			msg += e.putFixSlotBack(fctx, job)
		}
		e.failFix(fctx, job, msg, "")
		return
	}
	e.settleFix(fctx, job)
}

// putFixSlotBack stops a fixer whose run failed or timed out and puts the
// slot back at the reviewed head (spec 3.6: the fixer's local commits would
// hold the slot for head drift at the next round's guard): the fixer is
// interrupted, its agent waited for until quiet, and the slot resynced. It
// returns what to add to the failure's message ("" when the slot is back).
func (e *Engine) putFixSlotBack(ctx context.Context, job *fixJob) string {
	if e.interruptSessions(ctx, job.pr, &job.watch, job.role.Name) > 0 && !e.waitAgentsQuiet(ctx, job.pr.ID) {
		e.log.Info("the fixer still works after the interrupt", "subject", job.subject())
	}
	if err := e.resyncFixSlot(ctx, job, job.review.SHA); err != nil {
		return fmt.Sprintf("; slot %s not put back: %v", job.slot.Name, err)
	}
	return ""
}

// closeFixHandle ends the job's part that an abort can stop (the
// reservation's stop.ended, as afterRound marks a round's) and returns the
// abort and ignore orders that arrived before: from now on an abort is told
// to try again once the settle let go of the PR.
func (e *Engine) closeFixHandle(prID int64) []stopOrder {
	e.mu.Lock()
	defer e.mu.Unlock()
	h := e.rounds[prID]
	if h == nil {
		return nil
	}
	h.stop.ended = true
	return slices.Clone(h.stop.reqs)
}

// fixGuard checks that the fixer may work in the job's slot: the slot is
// still the PR's, no round is in flight, the head is still the reviewed one
// and the slot holds it, nobody typed into the PR's panes within
// human_cooldown, and Slots.Guard finds no person in the slot (their agent
// or process, their changes) and no HEAD drift. From then on the slot is
// the fixer's: its last_used_at moves, so a person who types into the PR's
// panes during the fix is told apart from the fixer.
func (e *Engine) fixGuard(ctx context.Context, job *fixJob) error {
	pr, err := e.st.PRByID(ctx, job.pr.ID)
	if err != nil {
		return err
	}
	job.pr = pr
	slot, has, err := e.slotOf(ctx, pr.ID)
	switch {
	case err != nil:
		return err
	case !has || slot.ID != job.slot.ID:
		return fmt.Errorf("slot %s is no longer the PR's", job.slot.Name)
	case slot.State != store.SlotClaimed && slot.State != store.SlotHeld:
		return fmt.Errorf("slot %s is %s", slot.Name, slot.State)
	}
	job.slot = slot
	review := job.review.SHA
	switch {
	case slices.Contains(store.InFlightStates, pr.State) || pr.State == store.PRPaused:
		return fmt.Errorf("a round is in progress (%s)", pr.State)
	case pr.HeadSHA != review:
		return fmt.Errorf("the head moved to %s since the review on %s", textx.ShortSHA(pr.HeadSHA), textx.ShortSHA(review))
	case deref(pr.ReviewedSHA) != review:
		return fmt.Errorf("the reviewed head is %s, not %s", textx.ShortSHA(deref(pr.ReviewedSHA)), textx.ShortSHA(review))
	case deref(slot.CheckedOutSHA) != review:
		return fmt.Errorf("slot %s holds %s, not the reviewed head %s", slot.Name, textx.ShortSHA(deref(slot.CheckedOutSHA)), textx.ShortSHA(review))
	}
	if until := e.cfg.Daemon.CooldownUntil(pr.HumanActiveAt); e.now().Before(until) {
		return fmt.Errorf("someone typed into the PR's panes, which are theirs until %s (human_cooldown)", until.Local().Format("15:04"))
	}
	if err := e.d.Slots.Guard(ctx, slot); err != nil {
		return err
	}
	return e.st.UpdateSlotFields(ctx, slot.ID, func(u *store.SlotUpdate) { u.Set("last_used_at", e.now()) })
}

// fixStart starts or resumes the fixer in the PR's workspace (its pane gets
// the fix role's identity's env, agents.Manager.paneEnv), creates its run of
// kind fix in the review's round and sends it the fix prompt. A run that
// Submit refused before sending is abandoned; one whose prompt may have
// arrived is waited for.
func (e *Engine) fixStart(ctx context.Context, job *fixJob) error {
	pr := job.pr
	src := e.d.Identities[pr.Identity]
	if src == nil {
		return fmt.Errorf("identity %q is not configured", pr.Identity)
	}
	if err := e.d.Agents.Preflight(ctx, job.role.AgentKind()); err != nil && isLogin(err) {
		return err
	}
	env, err := e.paneEnv(ctx, job.asRound(), src, job.review.SHA)
	if err != nil {
		return err
	}
	var layout []config.Role
	for _, r := range e.cfg.RolesFor(&job.watch) {
		if r.Judge || r.Runs == config.RunsAlways {
			layout = append(layout, r)
		}
	}
	layout = append(layout, job.role)
	ws, err := e.d.Agents.EnsureWorkspace(ctx, pr, job.slot.Path, env, fmt.Sprintf("%s#%d", job.repo.Name, pr.Number), layout)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	if ws.Panes[agents.Role(job.role.Name)] == "" {
		if ws, err = e.d.Agents.EnsurePane(ctx, pr, ws, job.slot.Path, env, job.role); err != nil {
			return fmt.Errorf("%s pane: %w", job.role.Name, err)
		}
	}
	if _, err := e.ensureAgent(ctx, pr, job.role, ws, false, effortFull); err != nil {
		return fmt.Errorf("start %s: %w", job.role.Name, err)
	}
	run, err := e.d.Agents.NewRun(ctx, pr, agents.Role(job.role.Name), store.RunFix, job.round)
	if err != nil {
		return err
	}
	job.run = run
	job.state.Report = deref(run.ReportPath)
	if err := e.setFixState(ctx, pr.ID, job.state); err != nil {
		e.abandonFixRun(ctx, run, err.Error())
		return err
	}
	text, err := e.d.Agents.FixPrompt(pr, agents.Role(job.role.Name), run, agents.FixPromptInput{
		HeadRef:       deref(pr.HeadRef),
		ReviewURL:     job.review.URL,
		ReviewFile:    filepath.Join(e.d.Layout.ReviewDir(job.repo.Owner, job.repo.Name, pr.Number, job.review.SHA), agents.PostReviewFile),
		ReviewerLogin: job.login,
		DBLockCommand: agents.DBLockLine(e.d.Layout.Binary(), job.slot.Path, job.role.Name),
	})
	if err != nil {
		e.abandonFixRun(ctx, run, err.Error())
		return err
	}
	job.touched = true
	if serr := e.d.Agents.Submit(ctx, run, text); serr != nil {
		cur, err := e.st.RunByID(context.WithoutCancel(ctx), run.ID)
		switch {
		case err != nil:
			return errors.Join(serr, err)
		case cur.State == store.RunPending: // refused before sending
			job.touched = false
			e.abandonFixRun(ctx, run, serr.Error())
			return serr
		case cur.State == store.RunFailed || cur.State == store.RunAbandoned || ctx.Err() != nil:
			return serr
		}
		// Submitted (stalled, timed out, acked as blocked): the text may be
		// in the agent, so the run is observed, never sent again.
		e.log.Warn("fix: prompt not acknowledged as working; waiting for the run", "subject", job.subject(), "run", run.ID, "err", serr)
	}
	e.event(ctx, "info", job.subject(), "fix.start", fmt.Sprintf("%s fixes the %s of the review on %s in %s (run %s, cycle %d of %d)",
		job.role.Name, job.findings(), textx.ShortSHA(job.review.SHA), job.slot.Name, run.ID, job.state.Cycle, job.state.Cap),
		map[string]any{"run": run.ID, "role": job.role.Name, "slot": job.slot.Name, "report": job.state.Report,
			"cycle": job.state.Cycle, "cap": job.state.Cap, "by": job.state.By})
	return nil
}

// fixWait waits for the fixer's run to end, as the pipeline waits for a
// reviewer's: a run ended (the agent idle on two ticks) is the fixer's
// report to read; a run failed or abandoned, or a session lost, fails the
// job. A run past the role's timeout gets the time-up line within the run
// and TimeUpGrace more; one that still runs is interrupted and fails.
func (e *Engine) fixWait(ctx context.Context, job *fixJob) error {
	timeout := job.role.Timeout.Duration
	err := e.waitFixRun(ctx, job.run.ID, timeout)
	if !errors.Is(err, errFixTimeout) {
		return err
	}
	budget := humanDuration(timeout)
	report := deref(job.run.ReportPath)
	if terr := e.d.Agents.Tell(ctx, job.run, fmt.Sprintf(fixTimeUpText, budget, report, agents.ReportMarker(job.run.ID))); terr != nil {
		return fmt.Errorf("%s ran out of its %s and could not be asked for its report: %w", job.role.Name, budget, terr)
	}
	e.event(ctx, "warn", job.subject(), "fix.time_up", fmt.Sprintf("%s ran out of its %s: asked it to write its report now, waiting %s more (run %s)",
		job.role.Name, budget, humanDuration(pipeline.TimeUpGrace), job.run.ID), map[string]any{"run": job.run.ID, "report": report})
	if err := e.waitFixRun(ctx, job.run.ID, pipeline.TimeUpGrace); !errors.Is(err, errFixTimeout) {
		return err
	}
	return fmt.Errorf("%s ran out of its %s and wrote no report in %s more", job.role.Name, budget, humanDuration(pipeline.TimeUpGrace))
}

// waitFixRun polls run runID every fixPoll until it ends (nil), fails or
// loses its session (an error), timeout passes (errFixTimeout; 0 = none) or
// ctx ends.
func (e *Engine) waitFixRun(ctx context.Context, runID string, timeout time.Duration) error {
	deadline := e.now().Add(timeout)
	for {
		run, err := e.st.RunByID(ctx, runID)
		if err != nil {
			return err
		}
		switch run.State {
		case store.RunEnded, store.RunVerified:
			return nil
		case store.RunFailed, store.RunAbandoned:
			return fmt.Errorf("%s run %s is %s: %s", run.Role, run.ID, run.State, deref(run.Error))
		}
		if run.SessionID != nil {
			if s, err := e.st.SessionByID(ctx, *run.SessionID); err == nil && s.State != store.SessionLive && s.State != store.SessionStarting {
				return fmt.Errorf("%s session %d is %s", run.Role, s.ID, s.State)
			}
		}
		if timeout > 0 && !e.now().Before(deadline) {
			return errFixTimeout
		}
		if err := e.d.Sleep(ctx, fixPoll); err != nil {
			return err
		}
	}
}

// abandonFixRun marks the fixer's run abandoned with why (a run already
// final keeps its state) and reports whether it did.
func (e *Engine) abandonFixRun(ctx context.Context, run store.Run, why string) bool {
	active := []string{store.RunPending, store.RunSubmitted, store.RunWorking, store.RunEnded}
	err := e.st.TransitionRun(context.WithoutCancel(ctx), run.ID, active, store.RunAbandoned, func(u *store.RunUpdate) {
		u.Set("error", oneLine(why, 300))
		u.Set("ended_at", e.now())
	})
	if err != nil && !errors.Is(err, store.ErrConflict) {
		e.log.Warn("fix: abandon run", "run", run.ID, "err", err)
	}
	return err == nil
}

// fixHolds reports whether the PR's fix loop holds its next round: the
// fixer works on it (phase fixing), or its row cannot be read (no round
// starts over a loop that may run). The loop's own re-review (phase
// reviewing) passes.
func (e *Engine) fixHolds(ctx context.Context, pr store.PR) bool {
	st, ok, err := e.fixState(ctx, pr.ID)
	if err != nil {
		if e.logOnce(fmt.Sprintf("fix-state:%d", pr.ID), err.Error(), e.now()) {
			e.log.Warn("dispatch holds a PR whose fix loop cannot be read", "pr", pr.ID, "err", err)
		}
		return true
	}
	return ok && st.Phase == FixPhaseFixing
}

// fixWaitOf is the wait of a PR whose fix loop holds it (fixHolds; ok
// false when none does).
func (e *Engine) fixWaitOf(ctx context.Context, prID int64) (Wait, bool) {
	st, ok, err := e.fixState(ctx, prID)
	switch {
	case err != nil:
		return Wait{Reason: WaitFixing, Detail: "its fix loop, whose row cannot be read (`magnum abort` clears it)"}, true
	case !ok || st.Phase != FixPhaseFixing:
		return Wait{}, false
	}
	return Wait{Reason: WaitFixing, Count: st.Cycle, Max: st.Cap, Detail: fmt.Sprintf("the fixer (fix cycle %d of %d)", st.Cycle, st.Cap)}, true
}

// fixPanicked settles a fix job that panicked as a failed one; the abort
// and ignore orders it held hear why (stopsPanicked).
func (e *Engine) fixPanicked(ctx context.Context, job *fixJob, msg string) {
	if orders := e.closeFixHandle(job.pr.ID); len(orders) > 0 {
		e.stopsPanicked(ctx, orders, msg)
	}
	if job.run.ID != "" {
		e.abandonFixRun(ctx, job.run, "the fix job panicked")
	}
	e.failFix(ctx, job, "fix run failed: panic: "+msg, "")
}

// recoverFixes ends the fix loops a stopped daemon left with the fixer at
// work (phase fixing): no fixer run survives a restart, so its run fails
// and the loop ends as needs attention. A loop in phase reviewing goes on
// (recoverReviewingLoop): one whose re-review is still to run waits for it
// (the round's own recovery re-reviews), one whose re-review posted gets
// the decision the stop kept the hook from making.
func (e *Engine) recoverFixes(ctx context.Context) {
	if e.d.DryRun {
		return
	}
	keys, err := e.st.KVKeys(ctx, "pr.*.fix")
	if err != nil {
		e.log.Warn("recovery: fix loops", "err", err)
		return
	}
	const why = "fix run lost in a daemon restart"
	for _, key := range keys {
		var id int64
		if _, err := fmt.Sscanf(key, "pr.%d.fix", &id); err != nil || KVPRFix(id) != key {
			continue
		}
		st, raw, ok, err := e.fixRow(ctx, id)
		if err != nil {
			e.log.Warn("recovery: a fix loop that cannot be read stays for magnum abort", "key", key, "err", err)
			continue
		}
		if !ok || (st.Phase != FixPhaseFixing && st.Phase != FixPhaseReviewing) {
			continue
		}
		pr, err := e.st.PRByID(ctx, id)
		if err != nil {
			continue
		}
		repo, err := e.st.RepoByID(ctx, pr.RepoID)
		if err != nil {
			continue
		}
		job := e.loopJob(ctx, repo, pr, st)
		if st.Phase == FixPhaseReviewing {
			e.recoverReviewingLoop(ctx, job, raw) // fix_loop.go
			continue
		}
		if runs, err := e.st.RunsByPR(ctx, pr.ID); err == nil {
			for _, r := range runs {
				if r.Kind == store.RunFix {
					e.finishFixRun(ctx, r, store.RunFailed, "lost", why)
				}
			}
		}
		e.failFix(ctx, job, why, st.Report)
	}
}
