package staging

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// errHook is what a test's hook returns to stop a Step as a killed process
// would: nothing after it is written.
var errHook = errors.New("stopped by the test's hook")

// stepRun is one run of an effect's step: its repository, mirror, deploys
// dir and journal. Only its git runs under the mirror's flock (git).
type stepRun struct {
	c    *Client
	repo string
	dir  string
	root *os.Root
	g    deployGit
	j    *journal
	// waited is how long the last git waited for the mirror's flock.
	waited time.Duration
	// written is the journal's outbox as its last write (or its load) left
	// it on disk: flush delivers it once the caller let the dir's and the
	// effect's flocks go.
	written []pendingEvent
}

// run runs the effect's step.
func (r *stepRun) run(ctx context.Context) error {
	switch r.j.Step {
	case StepClaim, StepPlan:
		return r.plan(ctx)
	case StepMerge:
		return r.merge(ctx)
	case StepVerify:
		return r.verify(ctx)
	case StepPush:
		return r.push(ctx)
	case StepWatch:
		return r.watch(ctx)
	case StepConfirm:
		return r.confirm(ctx)
	}
	return r.end(ctx, EffectFailed, "the journal names an unknown step "+strconv.Quote(oneLine(r.j.Step)))
}

// git runs fn, the step's git, under the mirror's flock. In a run of
// waits for the flock (BusyAt), a try waits only busyLockWait for it, so a
// busy mirror holds up the Steps once, not at each try; once it has the
// flock, the run of waits ends.
func (r *stepRun) git(ctx context.Context, fn func() error) error {
	wait := cmp.Or(r.c.lockWait, lockWait)
	if !r.j.BusyAt.IsZero() {
		wait = min(wait, cmp.Or(r.c.busyLockWait, busyLockWait))
	}
	r.waited = wait
	return r.c.underMirrorFor(ctx, r.dir, wait, func() error {
		r.j.BusyAt = time.Time{}
		return fn()
	})
}

// at runs the test's hook at a point of step.
func (r *stepRun) at(step, point string) error {
	if r.c.hook == nil {
		return nil
	}
	return r.c.hook(r.j.ID, step, point)
}

// event records step's phase in the journal's outbox (Pending): the next
// write stamps it with its Seq and the effect as it leaves it, and it is
// delivered after that write.
func (r *stepRun) event(step, phase, text string) {
	r.j.Pending = append(r.j.Pending, pendingEvent{Step: step, Phase: phase, Text: text})
}

// save writes the journal: its Seq one more, the outbox's new events
// stamped with it, and the events this client delivered dropped (each
// write keeps only what it has not delivered: at most one effect's
// undelivered events). A failed write changes nothing on disk, and its
// events go with the next write that lands.
func (r *stepRun) save() error {
	prev, prevSeq, prevAt := r.j.Pending, r.j.Seq, r.j.Updated
	r.j.Updated = r.c.d.Now()
	r.j.Seq++
	sent := r.c.sentUpTo(r.j.ID)
	snap := r.j.effect()
	var box []pendingEvent
	stamped := ""
	for _, ev := range prev {
		if ev.Seq == 0 {
			ev.Seq, ev.Effect = r.j.Seq, snap
			stamped = ev.Step
		}
		if ev.Seq > sent {
			box = append(box, ev)
		}
	}
	r.j.Pending = box
	if err := writeJournal(r.root, r.j); err != nil {
		r.j.Pending, r.j.Seq, r.j.Updated = prev, prevSeq, prevAt
		return err
	}
	r.written = slices.Clone(box)
	if stamped != "" {
		return r.at(stamped, "saved")
	}
	return nil
}

// loaded takes the outbox of a journal just read: a process that stopped
// between a write and its delivery, or another client, left those events,
// which flush delivers again (at least once; a repeat has the same key).
func (r *stepRun) loaded() { r.written = slices.Clone(r.j.Pending) }

// flush hands the outbox's events this client has not delivered to
// Deps.Event, in Seq order, the caller holding neither the deploys dir's
// flock nor the effect's, and remembers the highest Seq it delivered.
func (r *stepRun) flush() {
	sent := r.c.sentUpTo(r.j.ID)
	box := slices.Clone(r.written)
	slices.SortStableFunc(box, func(a, b pendingEvent) int { return cmp.Compare(a.Seq, b.Seq) })
	for i, ev := range box {
		if ev.Seq <= sent {
			continue
		}
		e := ev.Effect
		if e.ID == "" {
			e = r.j.effect()
		}
		e.Seq = ev.Seq
		r.c.d.Log.Debug("staging: deploy", "repo", r.repo, "effect", e.ID, "seq", e.Seq, "by", e.By, "step", ev.Step, "phase", ev.Phase, "text", ev.Text)
		if r.c.d.Event != nil {
			r.c.d.Event(e, ev.Step, ev.Phase, ev.Text)
		}
		if i == len(box)-1 || box[i+1].Seq != ev.Seq {
			r.c.delivered(r.j.ID, ev.Seq) // once the write's last event went out
		}
	}
	r.written = nil
}

// begin writes step's begin, then runs the test's hook. A polling step (a
// resolver's job, watch, confirm) writes it once; any other writes it each
// time it runs, so a begin found at the start is a step a stopped process
// left: it runs again ("resumed"), its guard first. A step that failed for
// a passing reason (phase fail) runs again without a new begin.
func (r *stepRun) begin(step string, polling bool, text string) error {
	switch {
	case r.j.Step == step && r.j.Phase == PhaseFail:
	case polling && r.j.Step == step && r.j.Phase == PhaseBegin:
	default:
		if r.j.Step == step && r.j.Phase == PhaseBegin {
			text = "resumed: " + text
		}
		r.j.Step, r.j.Phase = step, PhaseBegin
		r.event(step, PhaseBegin, text)
		if err := r.save(); err != nil {
			return err
		}
	}
	return r.at(step, "begin")
}

// ok writes the step's ok and moves the effect to next.
func (r *stepRun) ok(next, text string) error {
	step := r.j.Step
	r.j.Step, r.j.Phase, r.j.Why = next, "", ""
	r.clearFails()
	r.event(step, PhaseOK, text)
	return r.save()
}

// retryWaits are the waits after a step's failures in a row before it
// tries again; the last repeats.
var retryWaits = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}

// retry records a passing failure of step: phase fail and why, and the
// next try's time (RetryAt, retryWaits), which Why names. A failure
// failBudget or more after the first of the run ends the effect: failed
// before the push, nothing pushed; done, not confirmed, after it (the push
// stands).
func (r *stepRun) retry(ctx context.Context, step, why string) error {
	now := r.c.d.Now()
	if r.j.FailingSince.After(now) {
		// An edit, or a clock stepped back: the budget counts from now.
		r.c.noteKey("future "+string(r.j.ID), "staging: a deploy's first failure lies in the future; taken as now",
			"effect", r.j.ID, "failing_since", r.j.FailingSince)
		r.j.FailingSince = now
	}
	if r.j.Fails == 0 || r.j.FailingSince.IsZero() {
		r.j.FailingSince = now
	}
	r.j.Fails++
	if failing := now.Sub(r.j.FailingSince); failing >= failBudget {
		gave := fmt.Sprintf("%s failed for %s (%d tries): %s", step, minutes(failing.Truncate(time.Minute)), r.j.Fails, why)
		r.j.RetryAt = time.Time{}
		if !r.j.PushedAt.IsZero() {
			return r.endWith(ctx, EffectDone, PhaseFail, NotConfirmed+": "+gave+"; the push stands")
		}
		return r.end(ctx, EffectFailed, gave+"; nothing was pushed")
	}
	r.j.RetryAt = now.Add(retryWaits[min(r.j.Fails, len(retryWaits))-1])
	r.j.Phase, r.j.Why = PhaseFail, fmt.Sprintf("%s (try %d failed; next try at %s)", why, r.j.Fails, r.j.RetryAt.Format("15:04:05"))
	return r.save()
}

// clearFails forgets the step's failures in a row.
func (r *stepRun) clearFails() {
	r.j.Fails, r.j.FailingSince, r.j.RetryAt = 0, time.Time{}, time.Time{}
}

// wait says why the step waits (CI, Jenkins, the resolver): no failure, so
// the failures in a row start again.
func (r *stepRun) wait(why string) error {
	if r.j.Why == why && r.j.Fails == 0 {
		return nil
	}
	r.j.Why = why
	r.clearFails()
	if r.j.Phase == PhaseFail {
		r.j.Phase = PhaseBegin
	}
	return r.save()
}

// end ends the effect in state with why: the journal and its one event (ok
// for done, fail otherwise), and the resolver's job cancelled. Its refs in
// the mirror go after the step (dropRefs).
func (r *stepRun) end(ctx context.Context, state, why string) error {
	phase := PhaseFail
	if state == EffectDone {
		phase = PhaseOK
	}
	return r.endWith(ctx, state, phase, why)
}

// endWith is end with the event's phase: a done that ends a run of
// failures (after the push) is a fail of its step.
func (r *stepRun) endWith(ctx context.Context, state, phase, why string) error {
	r.j.State, r.j.Why, r.j.Phase = state, why, phase
	job := r.j.Job
	r.j.Job = ""
	r.event(r.j.Step, phase, why)
	if err := r.save(); err != nil {
		return err
	}
	r.c.d.Log.Info("staging: a deploy ended", "repo", r.repo, "effect", r.j.ID, "env", r.j.Env.Name, "by", r.j.By,
		"state", state, "step", r.j.Step, "merge", short(r.j.Merge))
	// A hand-off killed in its tree's build left its index: it goes with the
	// effect (no git, so no mirror flock; the effect's flock is held).
	if filepath.IsAbs(r.dir) {
		if err := dropIndex(resolvedIndex(r.dir, r.j.ID)); err != nil {
			r.c.d.Log.Warn("staging: a hand-off's index not removed", "effect", r.j.ID, "err", errLine(err))
		}
	}
	if job != "" && r.c.d.Resolver != nil {
		if err := r.c.d.Resolver.Cancel(ctx, job); err != nil {
			r.c.d.Log.Warn("staging: the resolver's job not cancelled", "effect", r.j.ID, "err", errLine(err))
		}
	}
	return nil
}

// dropRefs deletes the effect's refs in the mirror, under its flock.
func (r *stepRun) dropRefs(ctx context.Context) {
	if r.g == nil {
		return
	}
	err := r.git(ctx, func() error {
		return errors.Join(r.g.deleteRef(ctx, r.dir, mergeRef(r.j.ID)), r.g.deleteRef(ctx, r.dir, resolvedRef(r.j.ID)))
	})
	if err != nil {
		r.c.d.Log.Warn("staging: an effect's refs not deleted", "effect", r.j.ID, "err", errLine(err))
		return
	}
	r.j.Held = false
	if err := r.save(); err != nil {
		r.c.d.Log.Warn("staging: a deploy's journal not written", "effect", r.j.ID, "err", errLine(err))
	}
}

// branch is the effect's staging branch.
func (r *stepRun) branch() string { return r.j.Env.Branch }

// errNoBranch is tips' error when the remote has no such branch: the effect
// fails, since a push would make the branch.
var errNoBranch = errors.New("the remote has no such branch")

// tips fetches and reads the effect's branch tip and, with head, the PR's.
// The caller holds the mirror's flock.
func (r *stepRun) tips(ctx context.Context, head bool) (string, string, error) {
	env, err := r.c.gitEnv(r.repo)
	if err != nil {
		return "", "", err
	}
	n := 0
	if head {
		n = r.j.PR.Number
	}
	tips, h, err := r.c.tipsOf(ctx, r.dir, env, []string{r.branch()}, n)
	if err != nil {
		return "", "", err
	}
	tip := tips[r.branch()]
	if tip == "" {
		return "", "", errNoBranch
	}
	return tip, h, nil
}

// failed is a step's answer to err: a branch the remote lacks fails the
// effect, a busy mirror is a wait (busy), anything else is one failure of
// step.
func (r *stepRun) failed(ctx context.Context, step, what string, err error) error {
	switch {
	case errors.Is(err, errNoBranch):
		return r.end(ctx, EffectFailed, "the remote has no branch "+r.branch()+"; nothing was pushed")
	case errors.Is(err, errMirrorBusy):
		return r.busy(ctx)
	}
	return r.retry(ctx, step, what+": "+gitReason(err))
}

// busyRetry is how long a step waits to try again after the mirror's flock
// stayed busy.
const busyRetry = 30 * time.Second

// busyBudget caps the time an effect spends in all waiting for the
// mirror's flock, which the failure budget does not count: past it, the
// next wait ends the effect, as a failure would.
const busyBudget = 60 * time.Minute

// busyGap bounds the time one try after the first adds to the lock waits:
// the try's own wait plus its busyRetry and a tick, never the time with no
// Step (a sleeping laptop, a daemon down).
const busyGap = busyRetry + time.Minute

// busyLockWait is how long a try waits for the mirror's flock once a run of
// waits began: the retry, not the Step, does the waiting.
const busyLockWait = 5 * time.Second

// busy is a step that waited for the mirror's flock in vain: no failure
// (the failure budget neither counts it nor starts again), the step runs
// again after busyRetry, Why says so. Once the effect has waited for the
// flock busyBudget in all, it ends: failed before the push, done, not
// confirmed, after it (the push stands).
func (r *stepRun) busy(ctx context.Context) error {
	now := r.c.d.Now()
	waited := r.waited // the first try of a run: the wait it spent
	if !r.j.BusyAt.IsZero() && !r.j.BusyAt.After(now) {
		waited = min(now.Sub(r.j.BusyAt), busyGap)
	}
	r.j.BusyAt = now
	r.j.BusyFor = min(max(r.j.BusyFor, 0), busyBudget) + waited // an edit cannot defeat the cap
	if r.j.BusyFor >= busyBudget {
		gave := "the mirror stayed busy for " + strconv.Itoa(int(busyBudget.Minutes())) + " min (a Refresh's fetch or a clone held its lock)"
		r.j.RetryAt = time.Time{}
		if !r.j.PushedAt.IsZero() {
			return r.endWith(ctx, EffectDone, PhaseFail, NotConfirmed+": "+gave+"; the push stands")
		}
		return r.end(ctx, EffectFailed, gave+"; nothing was pushed")
	}
	r.j.RetryAt = now.Add(busyRetry)
	r.j.Phase, r.j.Why = PhaseFail, "waiting for the mirror's lock (a Refresh's fetch or a clone holds it); next try at "+r.j.RetryAt.Format("15:04:05")
	return r.save()
}

// headMoved is the Why of a PR whose head is no longer the plan's.
func (r *stepRun) headMoved(now string) string {
	return fmt.Sprintf("PR #%d's head moved since the plan (%s, now %s): plan again; nothing was pushed", r.j.PR.Number, short(r.j.PR.Head), short(now))
}

// plan re-reads the tip and the PR's head: a moved head fails, a head the
// tip holds already is done.
func (r *stepRun) plan(ctx context.Context) error {
	if err := r.begin(StepPlan, false, "reading "+r.branch()+"'s tip and PR #"+strconv.Itoa(r.j.PR.Number)+"'s head"); err != nil {
		return err
	}
	var tip, head string
	var there bool
	err := r.git(ctx, func() error {
		var err error
		if tip, head, err = r.tips(ctx, true); err != nil || head != r.j.PR.Head {
			return err
		}
		there, err = r.g.isAncestor(ctx, r.dir, head, tip)
		return err
	})
	switch {
	case err != nil:
		return r.failed(ctx, StepPlan, "the tips could not be read", err)
	case head != r.j.PR.Head:
		return r.end(ctx, EffectFailed, r.headMoved(head))
	case there:
		return r.end(ctx, EffectDone, alreadyThere(r.j.PR.Number, r.branch())+"; nothing was pushed")
	}
	r.j.Tip = tip
	text := "tip " + short(tip)
	if tip != r.j.PlanTip {
		text += ", moved since the plan's " + short(r.j.PlanTip)
	}
	return r.ok(StepMerge, text)
}

// unseen is why a merge on a tip other than the plan's stops for you: it
// adds migrations, or changes workflow paths, the plan did not show; "" when
// it does not.
func (r *stepRun) unseen(tip string, m mergeRead) string {
	if tip == r.j.PlanTip {
		return ""
	}
	if u := without(m.migrations, r.j.PlanMigrations); len(u) > 0 {
		return "the tip moved since your plan, and the merge now adds migrations you did not see: " + listed(u) + "; plan again"
	}
	if len(m.workflows) > 0 && !r.j.PlanWorkflows {
		return "the tip moved since your plan, and the merge now changes .github/workflows paths you did not see: " + listed(m.workflows) + "; plan again"
	}
	return ""
}

// merge merges the head into the tip: a clean merge's commit (kept at the
// effect's merge ref), or a resolver's job; with a job, it reads the job's
// result.
func (r *stepRun) merge(ctx context.Context) error {
	if r.j.Job != "" {
		return r.resolution(ctx)
	}
	if err := r.begin(StepMerge, false, "merging "+short(r.j.PR.Head)+" into "+short(r.j.Tip)); err != nil {
		return err
	}
	env, err := r.c.gitEnv(r.repo)
	if err != nil {
		return r.retry(ctx, StepMerge, err.Error())
	}
	var m mergeRead
	var there bool
	err = r.git(ctx, func() error {
		var err error
		if there, err = r.g.isAncestor(ctx, r.dir, r.j.PR.Head, r.j.Tip); err != nil || there {
			return err
		}
		m, err = readMerge(ctx, r.g, r.dir, env, r.j.Tip, r.j.PR.Head)
		return err
	})
	switch {
	case err != nil:
		return r.failed(ctx, StepMerge, "the merge could not be made", err)
	case there:
		return r.end(ctx, EffectDone, alreadyThere(r.j.PR.Number, r.branch())+"; nothing was pushed")
	}
	if why := r.unseen(r.j.Tip, m); why != "" {
		return r.end(ctx, EffectNeedsYou, why)
	}
	r.j.MergeTree, r.j.Conflicted = m.tree, len(m.conflicts)
	r.j.Conflicts = slices.Clone(m.conflicts[:min(len(m.conflicts), maxStored)])
	if len(m.conflicts) > 0 {
		return r.resolve(ctx)
	}
	if r.c.d.CommitIdentity == nil {
		return r.end(ctx, EffectFailed, "commit identity not configured")
	}
	name, email, err := r.c.d.CommitIdentity(ctx, r.repo)
	if err == nil {
		err = checkIdentity(name, email)
	}
	if err != nil {
		return r.end(ctx, EffectFailed, "commit identity not configured: "+errLine(err))
	}
	message := r.message(ctx)
	var sha string
	err = r.git(ctx, func() error {
		var err error
		if sha, err = r.g.commitTree(ctx, r.dir, m.tree, []string{r.j.Tip, r.j.PR.Head}, message, name, email); err != nil {
			return err
		}
		r.j.Held = true
		return r.g.keepMerge(ctx, r.dir, mergeRef(r.j.ID), sha)
	})
	if err != nil {
		return r.failed(ctx, StepMerge, "the merge commit could not be made", err)
	}
	r.j.Merge = sha
	return r.ok(StepVerify, "merge "+short(sha)+" on "+short(r.j.Tip))
}

// conflict is "conflict in N paths" for a Why.
func (r *stepRun) conflict() string { return fmt.Sprintf("conflict in %d paths", r.j.Conflicted) }

// resolve starts a resolver's job on the conflict.
func (r *stepRun) resolve(ctx context.Context) error {
	paths := listed(r.j.Conflicts)
	if r.j.Conflicted > len(r.j.Conflicts) && len(r.j.Conflicts) <= maxListed {
		paths += fmt.Sprintf(" and %d more", r.j.Conflicted-len(r.j.Conflicts))
	}
	switch {
	case r.c.d.Resolver == nil:
		return r.end(ctx, EffectFailed, "conflict: resolver not configured: "+paths)
	case r.j.Resolves >= maxResolves:
		return r.end(ctx, EffectFailed, fmt.Sprintf("conflict: the resolver had its %d tries: %s", maxResolves, paths))
	}
	owner, name, _ := strings.Cut(r.repo, "/")
	job, err := r.c.d.Resolver.Start(ctx, ResolveRequest{Repo: r.repo, Ref: owner + "/" + name + "#" + strconv.Itoa(r.j.PR.Number),
		Target: r.branch(), TargetTip: r.j.Tip, Head: r.j.PR.Head, Effect: string(r.j.ID), Try: r.j.Resolves + 1})
	if err == nil && job == "" {
		err = errors.New("no job")
	}
	if err != nil {
		return r.end(ctx, EffectFailed, "conflict: the resolver did not start: "+oneLine(errLine(err))+": "+paths)
	}
	r.j.Job, r.j.JobAt = job, r.c.d.Now()
	r.j.Resolves++
	r.j.Why = fmt.Sprintf("%s: the resolver works on it (try %d)", r.conflict(), r.j.Resolves)
	r.event(StepMerge, PhaseBegin, r.j.Why)
	return r.save()
}

// resolution reads the resolver's job: running waits (at most
// resolveWait), failed fails, done hands its merge off into the mirror
// (handOver) for verify; a merge handOver refuses fails at once.
func (r *stepRun) resolution(ctx context.Context) error {
	if r.c.d.Resolver == nil {
		return r.end(ctx, EffectFailed, "conflict: resolver not configured: "+listed(r.j.Conflicts))
	}
	res, err := r.c.d.Resolver.Result(ctx, r.j.Job)
	if err != nil {
		return r.retry(ctx, StepMerge, "the resolver's result could not be read: "+oneLine(errLine(err)))
	}
	switch res.State {
	case ResolveRunning:
		if r.c.d.Now().Sub(r.j.JobAt) >= resolveWait {
			return r.end(ctx, EffectFailed, "conflict: the resolver ran over "+minutes(resolveWait)+"; nothing was pushed")
		}
		return r.wait(fmt.Sprintf("%s: the resolver works on it (try %d)", r.conflict(), r.j.Resolves))
	case ResolveFailed:
		return r.end(ctx, EffectFailed, "conflict: the resolver failed: "+oneLine(res.Why))
	case ResolveDone:
	default:
		return r.end(ctx, EffectFailed, "conflict: the resolver answered "+strconv.Quote(oneLine(res.State)))
	}
	if !isOID(res.MergeSHA) {
		return r.end(ctx, EffectFailed, "the resolver's merge is no sha")
	}
	env, err := r.c.gitEnv(r.repo)
	if err != nil {
		return r.retry(ctx, StepMerge, err.Error())
	}
	var refused string
	err = r.git(ctx, func() error {
		src, err := r.g.checkResolverDir(ctx, res.Dir, r.dir)
		if err != nil {
			refused = "the resolver's merge is refused: " + oneLine(errLine(err))
			return nil
		}
		refused, err = r.handOver(ctx, env, src, res.MergeSHA)
		return err
	})
	switch {
	case errors.Is(err, errHook):
		return err
	case err != nil:
		return r.failed(ctx, StepMerge, "the resolver's merge could not be handed off", err)
	case refused != "":
		return r.end(ctx, EffectFailed, refused+"; nothing was pushed")
	}
	r.j.Merge, r.j.Job = res.MergeSHA, ""
	return r.ok(StepVerify, "resolved merge "+short(res.MergeSHA)+" on "+short(r.j.Tip)+", rebuilt in the mirror")
}

// handOver puts the resolver's merge sha, a commit in its repository's git
// dir src, into the mirror without a fetch, which cannot work: the
// treeless mirror holds commits, the trees and blobs its merges read, and
// no other blob; the full clone sends a thin pack whose deltas are against
// blobs of the tip or the head it takes the mirror to hold, and the mirror
// never fetches a missing one lazily there. So it reads, in src alone and
// never writing there, the commit (whose parents must be [tip, head]
// before anything is written), its tree and its entry at each path
// merge-tree's merge of the tip and the head conflicts in. It writes those
// blobs into the mirror, rebuilds merge-tree's tree with the resolver's
// entries at those paths (absent ones removed), checks the rebuilt tree
// differs from merge-tree's at those paths alone (update-index would let
// a file take a directory's place silently), and only when it is the
// commit's tree writes the commit itself, the same bytes and so the same
// sha, and holds it at the effect's resolved ref. The equal tree is the
// proof that the merge changes nothing but the conflicted paths; a tree
// that differs is refused, naming the paths. refused says why the merge is
// refused; err is a passing failure. The caller holds the mirror's flock.
func (r *stepRun) handOver(ctx context.Context, env map[string]string, src, sha string) (string, error) {
	if v := r.g.gitVersion(ctx, r.dir); gitAtLeast(v, 2, 51) < 0 {
		return oldGit(v), nil // cat-file's %(objectmode) came in 2.51
	}
	typ, err := r.g.srcType(ctx, src, r.dir, sha)
	switch {
	case err != nil:
		return "", err
	case typ == "":
		return "the resolver's repository does not hold its merge " + short(sha), nil
	case typ != "commit":
		return "the resolver's merge " + short(sha) + " is not a commit (a " + typ + ")", nil
	}
	raw, err := r.g.srcObject(ctx, src, r.dir, "commit", sha)
	if err != nil {
		return "", err
	}
	tree, parents := commitFields(raw)
	switch want := []string{r.j.Tip, r.j.PR.Head}; {
	case !isOID(tree):
		return "the resolver's merge " + short(sha) + " names no tree", nil
	case !slices.Equal(parents, want):
		return fmt.Sprintf("the resolver's merge's parents are %s, not [tip %s, head %s]", shorts(parents), short(want[0]), short(want[1])), nil
	}
	base, conflicts, err := r.g.mergeTree(ctx, r.dir, env, r.j.Tip, r.j.PR.Head)
	if err != nil {
		return "", fmt.Errorf("merge-tree: %w", err)
	}
	entries, err := r.g.srcEntries(ctx, src, r.dir, sha, conflicts)
	switch {
	case errors.Is(err, errGitTooOld):
		return err.Error(), nil
	case err != nil:
		return "", err
	}
	for _, e := range entries {
		switch {
		case e.Type == "":
			continue // removed: buildTree takes it out
		case e.Type == "tree":
			return "the resolver's merge holds a directory at the conflicted path " + safePath(e.Path), nil
		case e.Type == "commit":
			return "the resolver's merge holds a submodule at the conflicted path " + safePath(e.Path), nil
		case e.Type != "blob" || !slices.Contains(blobModes, e.Mode):
			return fmt.Sprintf("the resolver's merge holds a %s of mode %s at the conflicted path %s", oneLine(e.Type), oneLine(e.Mode), safePath(e.Path)), nil
		}
		data, err := r.g.srcObject(ctx, src, r.dir, "blob", e.OID)
		switch {
		case errors.Is(err, errObjectTooLarge):
			return "the resolved " + safePath(e.Path) + ": " + err.Error(), nil
		case err != nil:
			return "", err
		}
		id, err := r.g.writeObject(ctx, r.dir, "blob", data)
		switch {
		case err != nil:
			return "", err
		case id != e.OID:
			return "the resolved " + safePath(e.Path) + " is " + short(id) + " in the mirror, not the resolver's " + short(e.OID), nil
		}
	}
	if err := r.at(StepMerge, "blobs written"); err != nil {
		return "", err
	}
	built, err := r.g.buildTree(ctx, r.dir, resolvedIndex(r.dir, r.j.ID), base, entries)
	if err != nil {
		return "", err
	}
	inMirror := func(ctx context.Context, t string) ([]treeEntry, error) { return r.g.lsTree(ctx, r.dir, t) }
	changed, _, err := treeChanges(ctx, inMirror, inMirror, base, built, len(conflicts)+1, 0)
	if err != nil {
		return "", err
	}
	if extra := without(changed, conflicts); len(extra) > 0 {
		return "the resolver's entries at the conflicted paths also change merge-tree's " + listed(extra), nil
	}
	if built != tree {
		inSrc := func(ctx context.Context, t string) ([]treeEntry, error) { return r.g.srcTree(ctx, src, r.dir, t) }
		return mismatch(treeChanges(ctx, inMirror, inSrc, built, tree, maxListed+1, maxTreeReads))(tree, built, conflicts), nil
	}
	id, err := r.g.writeObject(ctx, r.dir, "commit", raw)
	switch {
	case err != nil:
		return "", err
	case id != sha:
		return "the resolver's merge is " + short(id) + " in the mirror, not " + short(sha), nil
	}
	r.j.Held = true // its ref goes when the effect ends
	return "", r.g.keepMerge(ctx, r.dir, resolvedRef(r.j.ID), sha)
}

// mismatch is the why of a resolver's tree that is not the rebuilt one,
// from treeChanges' walk of the two (rebuilt, the resolver's): the paths
// beyond the conflicted ones it found, at most maxListed, "and more" when
// there were more or the walk was cut; the conflicted paths alone when
// only they differ (cat-file answered "missing" for them: the resolver's
// repository lacks their content); the two trees' ids when the walk named
// nothing or failed.
func mismatch(paths []string, cut bool, err error) func(tree, built string, conflicts []string) string {
	return func(tree, built string, conflicts []string) string {
		beyond := "the resolver's merge differs from merge-tree's merge of the tip and the head beyond the conflicted paths"
		more := cut || len(paths) > maxListed
		paths = paths[:min(len(paths), maxListed)]
		extra := without(paths, conflicts)
		switch {
		case err == nil && len(extra) > 0 && more:
			return beyond + ": " + listed(extra) + " and more"
		case err == nil && len(extra) > 0:
			return beyond + ": " + listed(extra)
		case err == nil && len(paths) > 0 && !more:
			return "the resolver's repository lacks the content of the conflicted paths its merge names: " + listed(paths)
		}
		return beyond + " (its tree " + short(tree) + ", rebuilt " + short(built) + ")"
	}
}

// maxTreeReads bounds the lists of a walk of the resolver's tree, which
// runs under the mirror's flock: a tree tampered with to hold hundreds of
// empty subtrees, which name nothing, costs no more than these.
const maxTreeReads = 256

// treeChanges walks the trees a and b from their roots, listing each with
// its own lister, descending only where the ids differ, and returns the
// paths whose entries differ, at most limit: a file, a symlink or a
// submodule is named; a tree on one side alone is walked against nothing,
// so each path under it is named (a directory that the removal of its last
// file took away names that file). It makes at most budget lists (0: no
// bound); cut reports that the budget stopped it.
func treeChanges(ctx context.Context, listA, listB func(context.Context, string) ([]treeEntry, error), a, b string, limit, budget int) ([]string, bool, error) {
	lists, cut := 0, false
	list := func(l func(context.Context, string) ([]treeEntry, error), t string) ([]treeEntry, error) {
		if t == "" {
			return nil, nil
		}
		lists++
		return l(ctx, t)
	}
	var out []string
	var walk func(a, b, prefix string) error
	walk = func(a, b, prefix string) error {
		if a == b || len(out) >= limit || cut {
			return nil
		}
		if budget > 0 && lists+2 > budget {
			cut = true
			return nil
		}
		ea, err := list(listA, a)
		if err != nil {
			return err
		}
		eb, err := list(listB, b)
		if err != nil {
			return err
		}
		pairs := map[string]*[2]treeEntry{}
		var names []string
		for i, entries := range [][]treeEntry{ea, eb} {
			for _, e := range entries {
				if pairs[e.Path] == nil {
					pairs[e.Path] = &[2]treeEntry{}
					names = append(names, e.Path)
				}
				pairs[e.Path][i] = e
			}
		}
		slices.Sort(names)
		for _, name := range names {
			x, y := pairs[name][0], pairs[name][1]
			if x == y || len(out) >= limit || cut {
				continue
			}
			if x.Type != "tree" && y.Type != "tree" {
				out = append(out, prefix+name)
				continue
			}
			if x.Type != "" && x.Type != "tree" || y.Type != "" && y.Type != "tree" {
				out = append(out, prefix+name) // a file where the other side has a directory
			}
			var ta, tb string
			if x.Type == "tree" {
				ta = x.OID
			}
			if y.Type == "tree" {
				tb = y.OID
			}
			if err := walk(ta, tb, prefix+name+"/"); err != nil {
				return err
			}
		}
		return nil
	}
	err := walk(a, b, "")
	return out, cut, err
}

// checked is what checkMerge found: ok, why it fails the effect, why it
// needs you, or a moved tip to merge again on.
type checked struct {
	fail, needsYou string
	moved          string
}

// checkMerge checks the effect's merge against what the remote and the
// mirror say now, never the journal alone: the tip just read (tip) is the
// one the merge was made on, or a descendant of it (moved: merge again);
// the merge is a commit object (a missing one fails at once); its parents
// are exactly [tip, the plan's head]; its tree is a fresh merge-tree's of
// them or differs from it only in that merge-tree's conflicted paths; and
// on a tip other than the plan's it adds nothing the plan did not show. The
// caller holds the mirror's flock.
func (r *stepRun) checkMerge(ctx context.Context, tip string) (checked, error) {
	if !isOID(r.j.Merge) {
		return checked{fail: "the journal's merge " + strconv.Quote(oneLine(r.j.Merge)) + " is no sha"}, nil
	}
	if tip != r.j.Tip {
		on, err := r.g.isAncestor(ctx, r.dir, r.j.Tip, tip)
		switch {
		case err != nil:
			return checked{}, err
		case !on:
			return checked{fail: fmt.Sprintf("the branch's tip %s is not a descendant of %s, the tip the merge was made on", short(tip), short(r.j.Tip))}, nil
		}
		return checked{moved: tip}, nil
	}
	typ, err := r.g.objectType(ctx, r.dir, r.j.Merge)
	switch {
	case err != nil:
		return checked{}, err
	case typ == "":
		return checked{fail: "the merge " + short(r.j.Merge) + " is not in the mirror"}, nil
	case typ != "commit":
		return checked{fail: "the merge " + short(r.j.Merge) + " is not a commit (a " + typ + ")"}, nil
	}
	tree, parents, err := r.g.commitInfo(ctx, r.dir, r.j.Merge)
	if err != nil {
		return checked{}, err
	}
	if want := []string{tip, r.j.PR.Head}; !slices.Equal(parents, want) {
		return checked{fail: fmt.Sprintf("the merge's parents are %s, not [tip %s, head %s]", shorts(parents), short(want[0]), short(want[1]))}, nil
	}
	env, err := r.c.gitEnv(r.repo)
	if err != nil {
		return checked{}, err
	}
	m, err := readMerge(ctx, r.g, r.dir, env, tip, r.j.PR.Head)
	if err != nil {
		return checked{}, err
	}
	if why := r.unseen(tip, m); why != "" {
		return checked{needsYou: why}, nil
	}
	if tree == m.tree {
		return checked{}, nil
	}
	if len(m.conflicts) == 0 {
		return checked{fail: "the merge's tree is not merge-tree's"}, nil
	}
	changes, err := r.g.treeDiff(ctx, r.dir, env, m.tree, tree)
	if err != nil {
		return checked{}, err
	}
	var extra []string
	for _, ch := range changes {
		if !slices.Contains(m.conflicts, ch.Path) {
			extra = append(extra, ch.Path)
		}
	}
	if len(extra) > 0 {
		return checked{fail: "the resolved merge changes paths outside the conflicts: " + listed(extra)}, nil
	}
	return checked{}, nil
}

// apply answers a checkMerge that did not pass: it fails the effect, stops
// it for you, or merges again on the moved tip; done reports it did.
func (r *stepRun) apply(ctx context.Context, ch checked, movedWhy string) (bool, error) {
	switch {
	case ch.fail != "":
		return true, r.end(ctx, EffectFailed, ch.fail+": nothing was pushed")
	case ch.needsYou != "":
		return true, r.end(ctx, EffectNeedsYou, ch.needsYou)
	case ch.moved != "":
		return true, r.remerge(ctx, ch.moved, movedWhy)
	}
	return false, nil
}

// verify checks the merge before any push: the PR's head, then checkMerge.
func (r *stepRun) verify(ctx context.Context) error {
	if err := r.begin(StepVerify, false, "verifying "+short(r.j.Merge)); err != nil {
		return err
	}
	var head string
	var ch checked
	err := r.git(ctx, func() error {
		tip, h, err := r.tips(ctx, true)
		if err != nil || h != r.j.PR.Head {
			head = h
			return err
		}
		head = h
		ch, err = r.checkMerge(ctx, tip)
		return err
	})
	switch {
	case err != nil:
		return r.failed(ctx, StepVerify, "the merge could not be checked", err)
	case head != r.j.PR.Head:
		return r.end(ctx, EffectFailed, r.headMoved(head))
	}
	if done, err := r.apply(ctx, ch, "the tip moved before the push"); done {
		return err
	}
	return r.ok(StepPush, "merge "+short(r.j.Merge)+": parents and tree checked")
}

// remerge goes back to merge on the new tip, at most maxRemerges times.
func (r *stepRun) remerge(ctx context.Context, tip, why string) error {
	if r.j.Remerges >= maxRemerges {
		return r.end(ctx, EffectFailed, fmt.Sprintf("%s again (to %s): it moved %d times while deploying; nothing was pushed", why, short(tip), maxRemerges+1))
	}
	step := r.j.Step
	r.j.Remerges++
	r.j.Tip, r.j.Merge, r.j.MergeTree, r.j.Conflicts, r.j.Conflicted = tip, "", "", nil, 0
	r.clearFails()
	r.j.Step, r.j.Phase = StepMerge, ""
	r.j.Why = fmt.Sprintf("%s (to %s): merging again (try %d)", why, short(tip), 1+r.j.Remerges)
	r.event(step, PhaseFail, r.j.Why)
	return r.save()
}

// push pushes the verified merge, once. First, under the mirror's flock, a
// branch that holds it already is not pushed again and a moved tip merges
// again; then PushEnv, outside it; then, right before the push and under
// the flock, the head, checkMerge and the allowlist again from the live
// state, and the push. A failed push reads the tip again: holding the merge
// is pushed, moved merges again, else one failure.
func (r *stepRun) push(ctx context.Context) error {
	if err := r.begin(StepPush, false, "pushing "+short(r.j.Merge)+" to "+r.branch()); err != nil {
		return err
	}
	var landed bool
	var ch checked
	err := r.git(ctx, func() error {
		if !isOID(r.j.Merge) {
			ch.fail = "the journal's merge " + strconv.Quote(oneLine(r.j.Merge)) + " is no sha"
			return nil
		}
		if typ, err := r.g.objectType(ctx, r.dir, r.j.Merge); err != nil || typ != "commit" {
			if err == nil {
				ch.fail = "the merge " + short(r.j.Merge) + " is not in the mirror, or not a commit"
			}
			return err
		}
		tip, _, err := r.tips(ctx, false)
		if err != nil {
			return err
		}
		if landed, err = r.landed(ctx, tip); err != nil || landed {
			return err
		}
		if tip != r.j.Tip {
			ch, err = r.checkMerge(ctx, tip)
		}
		return err
	})
	switch {
	case err != nil:
		return r.failed(ctx, StepPush, "the branch could not be read", err)
	case landed:
		return r.pushed(r.branch() + " holds " + short(r.j.Merge) + " already: no push")
	}
	if done, err := r.apply(ctx, ch, "the tip moved before the push"); done {
		return err
	}
	if r.c.d.PushEnv == nil {
		return r.end(ctx, EffectFailed, "push not configured: nothing was pushed")
	}
	env, err := r.c.d.PushEnv(ctx, r.repo, r.j.Merge, r.branch())
	if err != nil {
		return r.end(ctx, EffectFailed, "push refused: "+oneLine(errLine(err)))
	}
	var refused, head string
	var perr error
	err = r.git(ctx, func() error {
		tip, h, err := r.tips(ctx, true)
		if err != nil {
			return err
		}
		if head = h; h != r.j.PR.Head {
			return nil
		}
		if landed, err = r.landed(ctx, tip); err != nil || landed {
			return err
		}
		if ch, err = r.checkMerge(ctx, tip); err != nil || ch != (checked{}) {
			return err
		}
		base, err := r.c.d.Git.Base(ctx, r.dir, r.repo)
		if err != nil {
			return err
		}
		if err := r.c.allowed(r.repo, r.j.Env, base); err != nil {
			refused = errLine(err)
			return nil
		}
		allowed := func(branch string) bool { return branch == r.branch() && r.c.allowed(r.repo, r.j.Env, base) == nil }
		perr = r.g.push(ctx, r.dir, env, r.j.Merge, r.branch(), allowed)
		if err := r.at(StepPush, "pushed"); err != nil {
			return err
		}
		if perr == nil {
			return nil
		}
		// Read the tip again: a push that landed before its error, or a
		// rejection because another push came first (non-fast-forward). When
		// that read fails too, the push's own failure counts.
		if again, _, err := r.tips(ctx, false); err == nil {
			if landed, err = r.landed(ctx, again); err != nil || landed {
				return err
			}
			ch, err = r.checkMerge(ctx, again)
			return err
		}
		return nil
	})
	switch {
	case errors.Is(err, errHook):
		return err
	case err != nil && perr == nil:
		return r.failed(ctx, StepPush, "the merge could not be checked before the push", err)
	case head != r.j.PR.Head:
		return r.end(ctx, EffectFailed, r.headMoved(head))
	case refused != "":
		return r.end(ctx, EffectFailed, refused+": nothing was pushed")
	case landed:
		return r.pushed(r.branch() + " holds " + short(r.j.Merge) + " already")
	case perr == nil:
		if done, err := r.apply(ctx, ch, "the tip moved before the push"); done {
			return err
		}
		return r.pushed("pushed " + short(r.j.Merge) + " to " + r.branch())
	}
	if done, err := r.apply(ctx, ch, "the push was rejected: the tip moved"); done {
		return err
	}
	return r.retry(ctx, StepPush, "the push failed: "+gitReason(perr))
}

// landed reports whether the branch's tip holds the merge already (a push
// before a restart, or one whose answer was lost): no second push. The
// caller holds the mirror's flock.
func (r *stepRun) landed(ctx context.Context, tip string) (bool, error) {
	return r.g.isAncestor(ctx, r.dir, r.j.Merge, tip)
}

// pushed records the push and moves on to watch.
func (r *stepRun) pushed(text string) error {
	if r.j.PushedAt.IsZero() {
		r.j.PushedAt = r.c.d.Now()
	}
	return r.ok(StepWatch, text)
}

// watch reads ci.yml's run on the merge: running waits (at most watchFor
// after the push), a failure fails, a pass moves on to confirm, and so do a
// cancel (a later push's run holds it) and no run within ConfirmAfterCI of
// the push, each kept in the end's Why.
func (r *stepRun) watch(ctx context.Context) error {
	if err := r.begin(StepWatch, true, "watching ci.yml on "+short(r.j.Merge)); err != nil {
		return err
	}
	runs, err := r.c.d.GitHub.RunsOf(ctx, r.repo, Workflow, r.j.Merge)
	if err != nil {
		return r.retry(ctx, StepWatch, "ci.yml's runs on the merge could not be read: "+oneLine(errLine(err)))
	}
	now := r.c.d.Now()
	on := func(note, text string) error {
		r.j.CIAt, r.j.CINote = now, note
		return r.ok(StepConfirm, text)
	}
	if len(runs) == 0 {
		if now.Sub(r.j.PushedAt) >= ConfirmAfterCI {
			note := "no ci.yml run on the merge within " + minutes(ConfirmAfterCI)
			return on(note, note)
		}
		return r.wait("ci.yml has not started on the merge yet")
	}
	newest := slices.MaxFunc(runs, func(a, b Run) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	r.j.RunURL = newest.URL
	switch {
	case newest.Status != "completed" && now.Sub(r.j.PushedAt) >= watchFor:
		note := "ci.yml still ran " + minutes(watchFor) + " after the push"
		return on(note, note)
	case newest.Status != "completed":
		return r.wait("ci.yml runs on the merge")
	case passed(&newest):
		return on("", "ci.yml passed")
	case newest.Conclusion == "cancelled":
		note := "ci.yml's run on the merge was cancelled (a later push's run holds it)"
		return on(note, note)
	}
	return r.end(ctx, EffectFailed, "ci.yml failed on the merge ("+oneLine(newest.Conclusion)+")")
}

// confirm reads the env's deployments (with their ETag): one whose history
// holds the merge ends the effect done; none ConfirmAfterCI after watch let
// it go, or deployments not readable, end it done, not confirmed.
func (r *stepRun) confirm(ctx context.Context) error {
	if err := r.begin(StepConfirm, true, "waiting for a deployment of "+r.j.Env.Name+" that holds "+short(r.j.Merge)); err != nil {
		return err
	}
	note := ""
	if r.j.CINote != "" {
		note = "; " + r.j.CINote
	}
	now := r.c.d.Now()
	d := r.c.deploymentsOf(ctx, r.repo, r.j.Env.Name, r.j.Deploys, now)
	r.j.Deploys = d
	switch {
	case d.Why == WhyNotReadable:
		return r.end(ctx, EffectDone, NotConfirmed+": "+WhyNotReadable+note)
	case !d.Readable:
		return r.retry(ctx, StepConfirm, oneLine(d.Why))
	}
	if i := slices.IndexFunc(d.List, func(x Deployment) bool { return x.Success }); i >= 0 {
		dep := cmp.Or(d.List[i].SHA, d.List[i].Ref)
		var in bool
		err := r.git(ctx, func() error {
			var err error
			in, err = r.holds(ctx, dep)
			return err
		})
		if err != nil {
			return r.failed(ctx, StepConfirm, "the deployment's history could not be read", err)
		}
		if in {
			return r.end(ctx, EffectDone, "live: the deployment of "+short(dep)+" holds the merge"+note)
		}
	}
	if now.Sub(r.j.CIAt) >= ConfirmAfterCI {
		return r.end(ctx, EffectDone, NotConfirmed+": no deployment of "+r.j.Env.Name+" holds the merge "+minutes(ConfirmAfterCI)+" after ci.yml"+note)
	}
	return r.wait("waiting for Jenkins to deploy the merge")
}

// holds reports whether the deployed commit dep's history holds the
// merge, fetching the branch when the mirror lacks it. The caller holds the
// mirror's flock.
func (r *stepRun) holds(ctx context.Context, dep string) (bool, error) {
	if !isOID(dep) {
		return false, nil
	}
	if dep == r.j.Merge {
		return true, nil
	}
	typ, err := r.g.objectType(ctx, r.dir, dep)
	if err == nil && typ == "" {
		if _, _, err = r.tips(ctx, false); err == nil {
			typ, err = r.g.objectType(ctx, r.dir, dep)
		}
	}
	if err != nil || typ != "commit" {
		return false, err
	}
	return r.g.isAncestor(ctx, r.dir, r.j.Merge, dep)
}

// message is the merge's message: "Merge branch '<head branch>' into
// staging-<env>", the head branch from PR.HeadRef or GitHub, else "Merge
// pull request #N into staging-<env>". It reads GitHub outside the
// mirror's flock.
func (r *stepRun) message(ctx context.Context) string {
	ref := r.j.PR.HeadRef
	if !validBranch(ref) {
		ref = ""
		if hr, ok := r.c.d.GitHub.(headRefs); ok {
			if got, err := hr.headRefOf(ctx, r.repo, r.j.PR.Number); err == nil && validBranch(got) {
				ref = got
			}
		}
	}
	if ref == "" {
		return fmt.Sprintf("Merge pull request #%d into %s", r.j.PR.Number, r.branch())
	}
	return "Merge branch '" + ref + "' into " + r.branch()
}

// headRefs is a GitHub that reads a PR's head branch: the adapter's.
type headRefs interface {
	headRefOf(ctx context.Context, repo string, number int) (string, error)
}
