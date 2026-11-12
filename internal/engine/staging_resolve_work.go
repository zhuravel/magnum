package engine

// A staging resolver job's work (staging_resolve.go): its slot, its merge,
// its agent, the daemon's commit and checks, and the slot put back.
//
// Where: a review slot of the PR's repository, which has the repository's
// development environment, so the specs can run. The job holds the PR
// with a slot-work reservation (reserveSlotWork, as an eviction does), so
// no round, open, fix or whiteboard job touches the slot meanwhile, and it
// takes the slot only when none holds the PR: the PR's own slot (claimed
// or held, unpinned, with no guard hold, nobody in it: Slots.Guard, and
// nobody typed into the PR's panes within human_cooldown, as for the
// fixer), else, for a PR with no slot, a free slot of the pool, reserved
// for the PR (Slots.Reserve; none free asks the pool for one, needSlot, as
// a fix or an open does). Either way a slot it cannot use now is a wait,
// never an eviction of a round's slot. A repository without a pool has no
// such checkout: the job fails. From the take on, the slot is the job's:
// its last_used_at moves, as for the fixer, so the guard that puts it back
// reads the job's own merge as magnum's, and only a person who types into
// the PR's panes during the job as theirs.
//
// The merge: Slots.Resync puts the slot, detached, at the target's tip and
// records it as the slot's checkout (so the guard sees no drift while the
// agent works), the head is fetched into the slot's clone when it lacks
// it, `git merge-tree --write-tree <tip> <head>` names the conflicts (the
// deploy's verify compares with its tree), a conflict the agent must give
// up on anyway fails the job before it starts (a binary file, a file both
// sides renamed apart: git leaves no marker there for the daemon's checks
// to find), and `git merge --no-commit --no-ff <head>` leaves the same
// conflicts in the work tree. The untracked files the slot holds before the
// merge are noted, so the job's own are told apart.
//
// The commit: the daemon's, never the agent's. The tree is merge-tree's
// with each conflicted path as the agent left it in the work tree
// (gitx.WriteTreeFrom, through an index of its own); the work tree must
// hold nothing else (the specs ran on it): no other tracked change and no
// new untracked file; no conflict marker may be left in a conflicted path,
// and the commit has the parents [tip, head], the bot as author and
// committer and the clean merge's message; its tree differs from
// merge-tree's only in the conflicted paths. The ref
// refs/magnum/staging-resolved/<effect> keeps it in the slot's main clone,
// the Dir the deploy fetches it from.
//
// The end: the agent is stopped (its workspace closed), the slot goes back
// (the untracked files the job added are removed, then Slots.Resync to the
// commit it held aborts the merge in progress and drops the work tree's
// changes; a slot the job claimed is resynced to the tip it recorded, then
// handed back to the pool), and the reservation goes. A panic in a job
// ends it the same way, failed.

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// resolveGit is the git of the resolver's jobs (*gitx.Client), in a job's
// slot and its main clone.
type resolveGit interface {
	RevParse(ctx context.Context, dir, ref string) (string, error)
	FetchCommit(ctx context.Context, mainClone, sha string, number int) error
	MergeNoCommit(ctx context.Context, dir, rev string, who gitx.Signature) ([]string, error)
	MergeTreeWrite(ctx context.Context, dir, a, b string) (string, []string, error)
	MergeTreeMessages(ctx context.Context, dir, a, b string) ([]gitx.MergeMessage, error)
	UntrackedFiles(ctx context.Context, dir string, paths ...string) ([]string, error)
	WriteTreeFrom(ctx context.Context, dir, base string, paths []string) (string, error)
	WorkTreeDiff(ctx context.Context, dir, rev string) ([]string, error)
	ChangedBetween(ctx context.Context, dir, from, to string) ([]string, error)
	CommitTree(ctx context.Context, dir, tree string, parents []string, message string, who gitx.Signature) (string, error)
	Parents(ctx context.Context, dir, rev string) ([]string, error)
	TreeOf(ctx context.Context, dir, rev string) (string, error)
	UpdateRef(ctx context.Context, dir, ref, sha string) error
	UpdateRefDelete(ctx context.Context, mainClone, ref string) error
}

var _ resolveGit = (*gitx.Client)(nil)

// resolveHeld is why dispatch holds the PR of a job at work in its slot.
const resolveHeld = "a staging merge is resolved in its slot"

// resolveFailure ends a job failed with its why (a definite failure: no
// wait, no retry).
type resolveFailure struct{ why string }

func (f resolveFailure) Error() string { return f.why }

// resolveFail is a resolveFailure with why.
func resolveFail(format string, args ...any) error {
	return resolveFailure{why: fmt.Sprintf(format, args...)}
}

// resolveWork is one run of a job by the worker.
type resolveWork struct {
	e     *Engine
	r     *stagingResolver
	job   *resolveJob
	git   resolveGit
	agent stagingAgent
	// held: the job holds its PR's slot-work reservation in this process
	// (of PR heldPR, or job.PRID when 0); keepSlot: its end leaves the slot
	// alone (other work holds the PR).
	held, keepSlot bool
	heldPR         int64
}

// runResolveJob runs job until it ends, waits for its slot (true: the next
// wake looks again) or the daemon stops (life ends: a job whose agent runs
// goes on after the restart). ctx is the job's own: Cancel ends it. A panic
// in it ends it failed, saying so, and lets go of what it held.
func (e *Engine) runResolveJob(ctx, life context.Context, r *stagingResolver, job *resolveJob) (waits bool) {
	w := &resolveWork{e: e, r: r, job: job, git: r.git}
	e.safely(life, "staging resolver", resolveSubject(*job), func() { waits = w.runToEnd(ctx, life) }, func(sctx context.Context, msg string) {
		w.finish(sctx, life, resolveFailed, "the resolver failed: a panic: "+msg)
	})
	return waits
}

// runToEnd is runResolveJob's body.
func (w *resolveWork) runToEnd(ctx, life context.Context) (waits bool) {
	e, job := w.e, w.job
	var err error
	if job.State == resolveRunning && !e.holdForResolve(job.PRID) {
		// The recovery held the PR again at the start (recoverStagingResolves):
		// other work holding it now owns the slot.
		w.keepSlot = true
		err = resolveFail("PR #%d is held by other work since the restart: slot %s is left as it is", job.Number, job.Slot)
	} else {
		w.held = job.State == resolveRunning
		err = w.run(ctx)
	}
	switch {
	case errors.Is(err, errResolveWaits):
		return true
	case life.Err() != nil:
		// The daemon stops: the agent goes on (the restart finds it), and
		// the slot stays the job's until then.
		if w.agent != nil {
			w.agent.Detach()
		}
		return false
	case errors.Is(err, errResolveEnded):
		// A Cancel ended the row before this run took it: what it took goes.
		if note := w.release(ctx); note != "" {
			e.log.Warn("staging resolver: the slot was not put back", "job", job.ID, "note", note)
		}
		return false
	}
	state, why := resolveFailed, ""
	var f resolveFailure
	switch {
	case err == nil:
		state, why = resolveDone, job.Why
	case ctx.Err() != nil || job.Cancel:
		why = whyCancelled
	case errors.As(err, &f):
		why = f.why
	default:
		why = "the resolver failed: " + oneLine(err.Error(), 200)
	}
	w.finish(ctx, life, state, why)
	return false
}

// finish lets go of what the job holds (release) and ends it in state with
// why, which says what was not put back of a failed job.
func (w *resolveWork) finish(ctx, life context.Context, state, why string) {
	e, job := w.e, w.job
	if note := w.release(ctx); note != "" && state == resolveFailed {
		why += "; " + note
	} else if note != "" {
		e.log.Warn("staging resolver: the slot was not put back", "job", job.ID, "note", note)
	}
	if err := w.r.end(ctx, e, job, state, why); err != nil {
		e.warnUnlessStopped(life, err, "staging resolver: end a job", "job", job.ID)
	}
}

// errResolveWaits is run's answer for a job that waits for its slot.
var errResolveWaits = errors.New("the job waits for a slot")

// run takes the job's slot, prepares the merge, has the agent resolve it
// and makes the commit, each phase checked against the job's bounds.
func (w *resolveWork) run(ctx context.Context) error {
	job, e := w.job, w.e
	if job.Cancel {
		return context.Canceled
	}
	if job.State == resolveQueued {
		if waited := e.now().Sub(job.Started); waited >= resolveWaitFor {
			return resolveFail("waited over %s for its turn and a slot (%s); nothing was resolved", textx.DurationWords(resolveWaitFor), job.Why)
		}
		if err := w.take(ctx); err != nil {
			return err
		}
	}
	deadline := job.Began.Add(resolveRunFor)
	over := func() error {
		if !e.now().Before(deadline) {
			return resolveFail("ran over %s; nothing was resolved", textx.DurationWords(resolveRunFor))
		}
		return ctx.Err()
	}
	if job.Phase == resolvePhaseSlot {
		if err := over(); err != nil {
			return err
		}
		if err := w.prepare(ctx); err != nil {
			return err
		}
	}
	var res resolveResultFile
	if job.Phase == resolvePhaseAgent {
		if err := over(); err != nil {
			return err
		}
		var err error
		if res, err = w.resolveWithAgent(ctx, deadline); err != nil {
			return err
		}
	}
	if err := over(); err != nil {
		return err
	}
	return w.commit(ctx, res)
}

// save writes the job's row, keeping a Cancel that came meanwhile.
func (w *resolveWork) save(ctx context.Context) error {
	w.job.Updated = w.e.now()
	return w.r.save(ctx, w.e, w.job)
}

// say records what the job does now as its Why.
func (w *resolveWork) say(ctx context.Context, why string) error {
	w.job.Why = why
	return w.save(ctx)
}

// take takes the job's slot (the file's comment says how) and moves the
// job to running, phase slot; errResolveWaits when it must wait, with Why
// saying for what.
func (w *resolveWork) take(ctx context.Context) error {
	job, e := w.job, w.e
	repo, err := e.st.RepoByFullName(ctx, job.Repo)
	if errors.Is(err, store.ErrNotFound) {
		return resolveFail("%s is not in magnum's registry", job.Repo)
	} else if err != nil {
		return err
	}
	pr, err := e.st.PRByRepoNumber(ctx, repo.ID, job.Number)
	if errors.Is(err, store.ErrNotFound) {
		return resolveFail("%s is not in magnum's registry", job.label())
	} else if err != nil {
		return err
	}
	switch {
	case pr.GHState != store.GHOpen:
		return resolveFail("%s is not open", job.label())
	case !e.ownPR(pr):
		return resolveFail("%s is not yours: the resolver works on your own PRs only for now", job.label())
	case pr.IsCrossRepo:
		return resolveFail("%s comes from a fork", job.label())
	}
	wait := func(why string) error {
		if err := w.say(ctx, "waits for a slot: "+why); err != nil {
			return err
		}
		return errResolveWaits
	}
	if e.rebootDraining(ctx) && !e.deployLive(*job) { // reboot_drain.go: the drain waits for a live deploy, so its resolve goes on; no other starts
		return wait("the daemon drains for a reboot")
	}
	role, err := e.resolveRole(job.Repo, resolveRunFor)
	if err != nil {
		return err
	}
	if why := e.kindPauseReason(ctx, []string{role.AgentKind()}); why != "" {
		return resolveFail("its agent cannot start: %s", why)
	}
	if slices.Contains(store.InFlightStates, pr.State) || pr.State == store.PRPaused {
		// A paused round's sessions go on in its slot later.
		return wait(fmt.Sprintf("PR #%d's round is in progress (%s)", pr.Number, pr.State))
	}
	full := repo.FullName()
	pool := e.cfg.PoolFor(full)
	slot, has, err := e.slotOf(ctx, pr.ID)
	if err != nil {
		return err
	}
	if !has && pool == nil {
		return resolveFail("%s has no review slot pool ([[pool]]): no checkout with its development environment to run the specs in", full)
	}
	if until := e.cfg.Daemon.CooldownUntil(pr.HumanActiveAt); has && e.now().Before(until) {
		// The PR's panes work in its slot, and they are a person's for now.
		return wait(fmt.Sprintf("someone typed into PR #%d's panes, which are theirs until %s (human_cooldown)", pr.Number, until.Local().Format("15:04")))
	}
	if !e.reserveSlotWork(pr.ID, resolveHeld) {
		return wait(fmt.Sprintf("PR #%d's slot is busy: %s", pr.Number, e.heldReason(pr.ID)))
	}
	w.held, w.heldPR = true, pr.ID
	let := func(err error) error { // the slot is not the job's after all
		e.endEviction(pr.ID)
		w.held, w.heldPR = false, 0
		return err
	}
	claimed := false
	if has {
		switch {
		case slot.State == store.SlotBroken || slot.State == store.SlotLost:
			return let(resolveFail("%s", unusableSlotSentence(slot)))
		case slot.State != store.SlotClaimed && slot.State != store.SlotHeld:
			return let(wait(fmt.Sprintf("slot %s is %s", slot.Name, slot.State)))
		case slot.Pinned:
			return let(wait(fmt.Sprintf("slot %s is pinned (magnum unpin)", slot.Name)))
		case slot.HoldReason != nil:
			return let(wait(slotHeldReason(slot)))
		case !fullSHA.MatchString(deref(slot.CheckedOutSHA)):
			return let(wait(fmt.Sprintf("slot %s records no checkout", slot.Name)))
		}
		if err := e.clearShells(ctx, pr, slot, holdClaim); err != nil {
			return let(wait(oneLine(err.Error(), 200)))
		}
		if err := e.d.Slots.Guard(ctx, slot); err != nil {
			if _, held := slots.AsHold(err); held {
				return let(wait(oneLine(err.Error(), 200)))
			}
			return let(err)
		}
		// The slot is the job's from here on, as the fixer's is: what the
		// tree holds is magnum's until someone types into the PR's panes.
		if err := e.st.UpdateSlotFields(ctx, slot.ID, func(u *store.SlotUpdate) { u.Set("last_used_at", e.now()) }); err != nil {
			return let(err)
		}
	} else {
		if slot, err = e.d.Slots.Reserve(ctx, pr, *pool); err != nil {
			if errors.Is(err, slots.ErrNoFreeSlot) {
				e.needSlot(ctx, *pool, prSubject(repo, pr.Number))
				return let(wait("no free slot in the pool yet"))
			}
			return let(err)
		}
		claimed = true
		e.event(ctx, "info", prSubject(repo, pr.Number), "slot.claimed", "reserved "+slot.Name+" for the staging resolver", nil)
	}
	dir := filepath.Clean(slot.MainClone)
	if !filepath.IsAbs(dir) {
		w.slotTaken(slot, claimed)
		return resolveFail("slot %s's clone %q is no absolute path", slot.Name, slot.MainClone)
	}
	w.slotTaken(slot, claimed)
	job.State, job.Phase, job.Dir, job.Began = resolveRunning, resolvePhaseSlot, dir, e.now()
	return w.say(ctx, fmt.Sprintf("prepares the merge in slot %s", slot.Name))
}

// slotTaken records slot as the job's (claimed: from the pool).
func (w *resolveWork) slotTaken(slot store.Slot, claimed bool) {
	job := w.job
	job.PRID, job.SlotID, job.Slot, job.Claimed = deref(slot.PRID), slot.ID, slot.Name, claimed
	if !claimed {
		job.SlotWas = deref(slot.CheckedOutSHA)
	}
}

// prepare leaves the merge in the job's slot: detached at the tip (the
// slot's recorded checkout), the head fetched when the clone lacks it,
// merge-tree's conflicts read (and refused when the agent would have to give
// up on them: refuseConflicts), the slot's untracked files noted, then `git
// merge --no-commit --no-ff <head>`, whose conflicts must be merge-tree's;
// the job then moves to its agent.
func (w *resolveWork) prepare(ctx context.Context) error {
	job, e := w.job, w.e
	if w.git == nil {
		return resolveFail("no git for the resolver")
	}
	slot, err := e.st.SlotByID(ctx, job.SlotID)
	if err != nil {
		return err
	}
	if err := e.d.Slots.Resync(ctx, slot, job.TargetTip); err != nil {
		if _, held := slots.AsHold(err); held {
			return resolveFail("slot %s cannot be put at %s: %s", slot.Name, textx.ShortSHA(job.TargetTip), oneLine(err.Error(), 200))
		}
		return err
	}
	if _, err := w.git.RevParse(ctx, slot.Path, job.Head+"^{commit}"); err != nil {
		if err := w.git.FetchCommit(ctx, slot.MainClone, job.Head, job.Number); err != nil {
			return resolveFail("the head %s could not be fetched into slot %s: %s", textx.ShortSHA(job.Head), slot.Name, oneLine(err.Error(), 200))
		}
	}
	who, err := w.bot(ctx)
	if err != nil {
		return err
	}
	tree, want, err := w.git.MergeTreeWrite(ctx, slot.Path, job.TargetTip, job.Head)
	switch {
	case err != nil:
		return err
	case len(want) == 0:
		return resolveFail("the merge does not conflict in slot %s: nothing to resolve", slot.Name)
	}
	if err := w.refuseConflicts(ctx, slot.Path); err != nil {
		return err
	}
	untracked, err := w.git.UntrackedFiles(ctx, slot.Path)
	if err != nil {
		return err
	}
	job.Untracked, job.UntrackedRead = untracked, true
	conflicts, err := w.git.MergeNoCommit(ctx, slot.Path, job.Head, who)
	switch {
	case err != nil:
		return err
	case !slices.Equal(conflicts, want):
		return resolveFail("the checkout's merge conflicts in %s, merge-tree's in %s", listedPaths(conflicts), listedPaths(want))
	}
	job.MergeTree, job.Conflicts, job.Phase = tree, want, resolvePhaseAgent
	return w.say(ctx, fmt.Sprintf("the merge conflicts in %s in slot %s; its agent starts", textx.Count(len(want), "path", "paths"), slot.Name))
}

// refuseConflicts fails the job on a conflict whose rules say give up,
// whatever its agent would answer: a binary file (git leaves the tip's
// version in the work tree and in merge-tree's tree, with no marker, so a
// "resolved" one would drop the PR's change without a word) and a file both
// sides renamed to different paths. merge-tree's messages name them.
func (w *resolveWork) refuseConflicts(ctx context.Context, dir string) error {
	msgs, err := w.git.MergeTreeMessages(ctx, dir, w.job.TargetTip, w.job.Head)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		switch m.Kind {
		case gitx.ConflictBinary:
			return resolveFail("a binary file conflicts: %s; no merge keeps both sides, so nothing was resolved", listedPaths(m.Paths))
		case gitx.ConflictRenameRename:
			return resolveFail("both sides renamed one file to different paths: %s; no merge keeps both, so nothing was resolved", listedPaths(m.Paths))
		}
	}
	return nil
}

// bot is the commit identity of the job's repository: the fixer App's bot.
func (w *resolveWork) bot(ctx context.Context) (gitx.Signature, error) {
	name, email, err := w.e.CommitIdentity(ctx, w.job.Repo)
	if err != nil {
		return gitx.Signature{}, resolveFail("commit identity not configured: %s", oneLine(err.Error(), 200))
	}
	return gitx.Signature{Name: name, Email: email}, nil
}

// commit makes the merge of the agent's resolution (res, which resolved
// the conflicts and passed its specs) as the bot, checks it and keeps it
// at the job's ref in its repository; the job's Why says what was checked.
func (w *resolveWork) commit(ctx context.Context, res resolveResultFile) error {
	job, e := w.job, w.e
	if res.Outcome != resolveOutcomeResolved {
		return resolveFail("the agent gave up: %s", res.Why)
	}
	job.Phase = resolvePhaseCommit
	if err := w.say(ctx, "makes and checks the merge"); err != nil {
		return err
	}
	slot, err := e.st.SlotByID(ctx, job.SlotID)
	if err != nil {
		return err
	}
	if marked := conflictMarked(slot.Path, job.Conflicts); len(marked) > 0 {
		return resolveFail("conflict markers are left in %s", listedPaths(marked))
	}
	tree, err := w.git.WriteTreeFrom(ctx, slot.Path, job.MergeTree, job.Conflicts)
	if err != nil {
		return err
	}
	extra, err := w.git.WorkTreeDiff(ctx, slot.Path, tree)
	if err != nil {
		return err
	}
	if len(extra) > 0 {
		return resolveFail("the checkout also changes %s, outside the conflicts: the specs ran on what the merge would not hold", listedPaths(extra))
	}
	added, err := w.addedFiles(ctx, slot.Path)
	if err != nil {
		return err
	}
	if len(added) > 0 {
		return resolveFail("the checkout also adds %s, outside the conflicts: the specs ran on what the merge would not hold", listedPaths(added))
	}
	if why, err := w.check(ctx, slot.Path, tree); err != nil || why != "" {
		return cmp.Or(err, resolveFail("%s", why))
	}
	who, err := w.bot(ctx)
	if err != nil {
		return err
	}
	merge, err := w.git.CommitTree(ctx, slot.Path, tree, []string{job.TargetTip, job.Head}, w.message(ctx), who)
	if err != nil {
		return err
	}
	parents, err := w.git.Parents(ctx, slot.Path, merge)
	if err != nil {
		return err
	}
	if !slices.Equal(parents, []string{job.TargetTip, job.Head}) {
		return resolveFail("the merge's parents are %v, not [tip %s, head %s]", parents, textx.ShortSHA(job.TargetTip), textx.ShortSHA(job.Head))
	}
	if got, err := w.git.TreeOf(ctx, slot.Path, merge); err != nil || got != tree {
		return cmp.Or(err, resolveFail("the merge's tree is %s, not the resolved %s", textx.ShortSHA(got), textx.ShortSHA(tree)))
	}
	if err := w.git.UpdateRef(ctx, job.Dir, job.ref(), merge); err != nil {
		return fmt.Errorf("keep the merge at %s in %s: %w", job.ref(), job.Dir, err)
	}
	job.Merge, job.Kept = merge, true
	job.Why = fmt.Sprintf("resolved %s in slot %s (try %d): %s; %s; checked: parents [tip, head], the tree differs from merge-tree's only in "+
		"the conflicted paths, no conflict marker left", listedPaths(job.Conflicts), job.Slot, job.Try, res.specsWords(), whoWords(who))
	return nil
}

// addedFiles lists the untracked files of the slot at dir that it did not
// hold before the merge (git does not ignore them, and they are no
// conflicted path): the job's own, which no merge commit holds. None when
// the slot's untracked files were not noted.
func (w *resolveWork) addedFiles(ctx context.Context, dir string) ([]string, error) {
	if !w.job.UntrackedRead {
		return nil, nil
	}
	now, err := w.git.UntrackedFiles(ctx, dir)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(now, func(p string) bool {
		return slices.Contains(w.job.Untracked, p) || slices.Contains(w.job.Conflicts, p)
	}), nil
}

// removeAdded removes the files the job added to the slot (addedFiles),
// through an os.Root of the slot: files and symlinks only, never a
// directory or a repository inside the work tree, and none once someone
// typed into the PR's panes during the job. It returns what it left there
// ("" when nothing stays).
func (w *resolveWork) removeAdded(ctx context.Context, dir string) string {
	added, err := w.addedFiles(ctx, dir)
	if err != nil {
		return "its added files were not read: " + oneLine(err.Error(), 200)
	}
	if len(added) == 0 {
		return ""
	}
	if pr, err := w.e.st.PRByID(ctx, w.job.PRID); err != nil || pr.HumanActiveAt != nil && pr.HumanActiveAt.After(w.job.Began) {
		// Someone typed into the PR's panes, which work in the slot, during
		// the job: a new file may be theirs.
		return "the files it added stay (someone typed into the PR's panes meanwhile): " + listedPaths(added)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "its added files stay: " + oneLine(err.Error(), 200)
	}
	defer root.Close()
	var stay []string
	for _, p := range added {
		fi, err := root.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil || fi.IsDir() || strings.HasSuffix(p, "/"):
			stay = append(stay, p)
		default:
			if err := root.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				stay = append(stay, p)
			}
		}
	}
	if len(stay) > 0 {
		return "the files it added stay: " + listedPaths(stay)
	}
	return ""
}

// check checks the resolved tree as the deploy's verify does: it differs
// from merge-tree's only in the conflicted paths ("" when it does).
func (w *resolveWork) check(ctx context.Context, dir, tree string) (string, error) {
	changed, err := w.git.ChangedBetween(ctx, dir, w.job.MergeTree, tree)
	if err != nil {
		return "", err
	}
	if extra := slices.DeleteFunc(changed, func(p string) bool { return slices.Contains(w.job.Conflicts, p) }); len(extra) > 0 {
		return "the resolved tree changes paths outside the conflicts: " + listedPaths(extra), nil
	}
	return "", nil
}

// whoWords names the merge's author and committer.
func whoWords(who gitx.Signature) string { return "committed as " + who.Name }

// message is the merge's message, the one internal/staging gives a clean
// merge: "Merge branch '<head branch>' into <target>", or "Merge pull
// request #N into <target>" when the PR's branch is not known or no name a
// branch may have.
func (w *resolveWork) message(ctx context.Context) string {
	job := w.job
	ref := ""
	if pr, err := w.e.st.PRByID(ctx, job.PRID); err == nil {
		ref = deref(pr.HeadRef)
	}
	if !stagingBranchName(ref) {
		return fmt.Sprintf("Merge pull request #%d into %s", job.Number, job.Target)
	}
	return "Merge branch '" + ref + "' into " + job.Target
}

// stagingBranchName is internal/staging's rule for a head branch its merge
// message names (git's ref name rules, no control character).
func stagingBranchName(name string) bool {
	switch {
	case name == "" || name == "@" || name == "HEAD" || strings.HasPrefix(name, "-"):
		return false
	case strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) }):
		return false
	case strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.Contains(name, "//"):
		return false
	case strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, "."):
		return false
	}
	for part := range strings.SplitSeq(name, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// maxMarkedFile bounds a conflicted file the marker check reads: a bigger
// one counts as marked (the agent gives up on a binary file anyway).
const maxMarkedFile = 32 << 20

// conflictMarked lists the paths among paths whose file in the work tree
// at dir still holds a conflict marker line (<<<<<<<, ||||||| or >>>>>>>,
// then a space or the line's end), read through an os.Root of dir: a
// symlink is not followed, and a deleted path holds none.
func conflictMarked(dir string, paths []string) []string {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return slices.Clone(paths)
	}
	defer root.Close()
	var out []string
	for _, p := range paths {
		fi, err := root.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			out = append(out, p)
			continue
		case !fi.Mode().IsRegular():
			continue // a symlink or a submodule's gitlink: no lines
		case fi.Size() > maxMarkedFile:
			out = append(out, p)
			continue
		}
		f, err := root.Open(p)
		if err != nil {
			out = append(out, p)
			continue
		}
		if hasConflictMarker(f) {
			out = append(out, p)
		}
		f.Close()
	}
	return out
}

// hasConflictMarker reports whether r holds a line that starts with a
// conflict marker of git's size (7), then a space or the line's end.
func hasConflictMarker(r io.Reader) bool {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxMarkedFile)
	for sc.Scan() {
		line := bytes.TrimSuffix(sc.Bytes(), []byte("\r"))
		for _, m := range [][]byte{[]byte("<<<<<<<"), []byte("|||||||"), []byte(">>>>>>>")} {
			if rest, ok := bytes.CutPrefix(line, m); ok && (len(rest) == 0 || rest[0] == ' ') {
				return true
			}
		}
	}
	return sc.Err() != nil
}

// release ends what the job holds: its agent is stopped, its slot put back
// (a claimed one handed back to the pool) and its PR's reservation goes.
// It returns what did not go as it should ("" when all did).
func (w *resolveWork) release(ctx context.Context) string {
	ctx = context.WithoutCancel(ctx)
	job, e := w.job, w.e
	var notes []string
	if w.agent == nil && job.Agent != "" {
		// A restart found the job's agent; it is stopped all the same.
		if a, err := w.newAgent(); err == nil {
			w.agent = a
		}
	}
	if w.agent != nil {
		if err := w.agent.Close(ctx); err != nil {
			where := ""
			if job.Pane != "" {
				where = " in pane " + job.Pane
			}
			// Close says why first, so the clip keeps it.
			notes = append(notes, "its agent may still run"+where+": "+oneLine(err.Error(), 200))
		}
	}
	if job.SlotID != 0 && !w.keepSlot {
		if why := w.putSlotBack(ctx); why != "" {
			notes = append(notes, why)
		}
	}
	if w.held {
		e.endEviction(cmp.Or(w.heldPR, job.PRID))
		w.held, w.heldPR = false, 0
	}
	return strings.Join(notes, "; ")
}

// holdForResolve holds PR prID for a resolver job's slot work: it takes
// the reservation (reserveSlotWork), or finds it the job's already (the
// recovery took it at the start); false while other work holds the PR.
func (e *Engine) holdForResolve(prID int64) bool {
	if prID == 0 {
		return false
	}
	e.mu.Lock()
	held := e.evicting[prID] == resolveHeld
	e.mu.Unlock()
	return held || e.reserveSlotWork(prID, resolveHeld)
}

// putSlotBack resyncs the job's slot to the commit it held (aborting the
// merge, dropping the work tree's changes), or, for a slot the job
// claimed, to the tip it recorded (a HEAD the agent moved would hold the
// release for drift) and then hands it back to the pool.
func (w *resolveWork) putSlotBack(ctx context.Context) string {
	job, e := w.job, w.e
	slot, err := e.st.SlotByID(ctx, job.SlotID)
	if err != nil {
		return fmt.Sprintf("slot %s not put back: %v", job.Slot, err)
	}
	at := job.SlotWas
	if job.Claimed {
		at = deref(slot.CheckedOutSHA) // the tip, once prepare put the slot there
	}
	var notes []string
	if fullSHA.MatchString(at) && deref(slot.PRID) == job.PRID && (slot.State == store.SlotClaimed || slot.State == store.SlotHeld) {
		if why := w.removeAdded(ctx, slot.Path); why != "" {
			notes = append(notes, "slot "+slot.Name+": "+why)
		}
		if err := e.d.Slots.Resync(ctx, slot, at); err != nil {
			return strings.Join(append(notes, fmt.Sprintf("slot %s not put back at %s: %s", slot.Name, textx.ShortSHA(at), oneLine(err.Error(), 200))), "; ")
		}
	}
	if len(notes) > 0 && !job.Claimed {
		return strings.Join(notes, "; ")
	}
	if !job.Claimed {
		return ""
	}
	pool := e.cfg.PoolFor(slot.RepoFullName)
	if pool == nil {
		return fmt.Sprintf("slot %s not handed back: no pool for %s", slot.Name, slot.RepoFullName)
	}
	if slot, err = e.st.SlotByID(ctx, job.SlotID); err != nil {
		return fmt.Sprintf("slot %s not handed back: %v", job.Slot, err)
	}
	if err := e.handBackSlot(ctx, slot, *pool, "the staging resolver is done"); err != nil {
		e.event(ctx, "warn", "slot:"+slot.Name, "slot.release_failed", "release after the staging resolver: "+err.Error(), nil)
		return strings.Join(append(notes, fmt.Sprintf("slot %s not handed back: %s", slot.Name, oneLine(err.Error(), 200))), "; ")
	}
	e.event(ctx, "info", "slot:"+slot.Name, "slot.released", slot.Name+" released after the staging resolver", nil)
	return strings.Join(notes, "; ")
}

// resolveDir is the job's directory: its prompt's result file and its
// agent's scratch registry.
func (e *Engine) resolveDir(id string) string {
	return filepath.Join(e.d.Layout.State(), "staging-resolve", id)
}

// resolveRole is the job's agent's role: the fix role of the PR's watch
// (its kind, model, effort and launch) with no identity, so its pane gets
// no GitHub env, and no skill or prompt of the fixer's.
func (e *Engine) resolveRole(repo string, until time.Duration) (config.Role, error) {
	w := e.cfg.WatchFor(repo)
	if w == nil {
		return config.Role{}, resolveFail("%s is not watched (no [[watch]] covers it)", repo)
	}
	role, ok := e.cfg.FixRoleFor(w)
	if !ok {
		return config.Role{}, resolveFail("no fix role for %s: the resolver's agent is the fix role's", repo)
	}
	role.Identity, role.Fix, role.Skill, role.Prompt = "", false, "", ""
	role.Timeout = config.Duration{Duration: until}
	return role, nil
}
