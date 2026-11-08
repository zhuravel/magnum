package staging

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
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

// git runs fn, the step's git, under the mirror's flock.
func (r *stepRun) git(ctx context.Context, fn func() error) error {
	return underMirror(ctx, r.dir, fn)
}

// at runs the test's hook at a point of step.
func (r *stepRun) at(step, point string) error {
	if r.c.hook == nil {
		return nil
	}
	return r.c.hook(r.j.ID, step, point)
}

// event records step's phase: it goes with the journal's next write
// (Pending), and is delivered after it.
func (r *stepRun) event(step, phase, text string) {
	r.j.Pending = append(r.j.Pending, pendingEvent{Step: step, Phase: phase, Text: text})
}

// save writes the journal, then delivers its pending events.
func (r *stepRun) save() error {
	r.j.Updated = r.c.d.Now()
	r.j.Seq++
	if err := writeJournal(r.root, r.j); err != nil {
		return err
	}
	if n := len(r.j.Pending); n > 0 {
		if err := r.at(r.j.Pending[n-1].Step, "saved"); err != nil {
			return err
		}
	}
	r.deliver()
	return nil
}

// deliver hands the pending events to Deps.Event and forgets them (the
// next write drops them from the journal); this client remembers the write
// it delivered.
func (r *stepRun) deliver() {
	r.c.delivered(r.j.ID, r.j.Seq)
	for _, ev := range r.j.Pending {
		r.c.d.Log.Debug("staging: deploy", "repo", r.repo, "effect", r.j.ID, "by", r.j.By, "step", ev.Step, "phase", ev.Phase, "text", ev.Text)
		if r.c.d.Event != nil {
			r.c.d.Event(r.j.effect(), ev.Step, ev.Phase, ev.Text)
		}
	}
	r.j.Pending = nil
}

// redeliver delivers the pending events of a journal just loaded, unless
// this client delivered that write already: a process that stopped between
// a write and its delivery left them (at least once).
func (r *stepRun) redeliver() {
	if len(r.j.Pending) > 0 && !r.c.wasDelivered(r.j.ID, r.j.Seq) {
		r.deliver()
	}
	r.j.Pending = nil
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
// effect, anything else is one failure of step.
func (r *stepRun) failed(ctx context.Context, step, what string, err error) error {
	if errors.Is(err, errNoBranch) {
		return r.end(ctx, EffectFailed, "the remote has no branch "+r.branch()+"; nothing was pushed")
	}
	return r.retry(ctx, step, what+": "+gitReason(err))
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
		return r.retry(ctx, StepMerge, "the merge could not be made: "+err.Error())
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
		return r.retry(ctx, StepMerge, "the merge commit could not be made: "+gitReason(err))
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
// resolveWait), failed fails, done fetches its merge into the mirror for
// verify; a merge that is no commit fails at once.
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
	var refused string
	err = r.git(ctx, func() error {
		src, err := r.g.checkResolverDir(ctx, res.Dir, r.dir)
		if err != nil {
			refused = "the resolver's merge is refused: " + oneLine(errLine(err))
			return nil
		}
		ref := resolvedRef(r.j.ID)
		r.j.Held = true // its ref goes when the effect ends
		ferr := r.g.deleteRef(ctx, r.dir, ref)
		if ferr == nil {
			ferr = r.g.fetchResolved(ctx, r.dir, src, res.MergeSHA, ref)
		}
		typ, err := r.g.objectType(ctx, r.dir, res.MergeSHA)
		switch {
		case err != nil:
			return err
		case typ == "":
			refused = "the resolver's merge is not in the mirror"
			if ferr != nil {
				refused += ": " + gitReason(ferr)
			}
		case typ != "commit":
			refused = "the resolver's merge " + short(res.MergeSHA) + " is not a commit (a " + typ + ")"
		}
		return nil
	})
	switch {
	case err != nil:
		return r.retry(ctx, StepMerge, "the resolver's merge could not be read: "+gitReason(err))
	case refused != "":
		return r.end(ctx, EffectFailed, refused+"; nothing was pushed")
	}
	r.j.Merge, r.j.Job = res.MergeSHA, ""
	return r.ok(StepVerify, "resolved merge "+short(res.MergeSHA)+" on "+short(r.j.Tip))
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
		return r.pushed("pushed " + short(r.j.Merge) + " to " + r.branch())
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
			return r.retry(ctx, StepConfirm, "the deployment's history could not be read: "+gitReason(err))
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
