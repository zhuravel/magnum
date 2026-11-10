package factory

// The publish (spec 6.6): after your push on the ready card the factory,
// never an agent, pushes the approved head with a lease and opens the PR,
// ready for review, under your login, with the drafted body; then it reads
// the PR back. It is one effect under the key publish:<task>:<sha>, so a
// head is published once: a second pass, or a restart, finds the effect
// ended ok and only finishes the task. Each step writes begin and ok or
// fail events and checks again what it needs before it acts: the card's
// conditions (readyProblem), your identity, the branch in the copy at the
// head you approved and the PR body as the card showed it. A daemon
// stopped in the middle resumes from the effect row: it reads the remote
// branch (git ls-remote) and the PRs of the branch first, so a branch at
// the approved head is not pushed again and a PR is opened once. It never
// forces: a remote branch at a head the factory did not push, or a PR of
// the branch that is not the one you approved, needs you, naming it. The
// PR read back must be yours, on the base, at the approved head and not a
// draft, or the task needs you naming the field. Then the PR is linked,
// the task is published, its agents are parked, its copy is released and
// the queued task behind it starts. The factory sends magnum no review
// request: the PR is no draft, and magnum reviews it by itself.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/textx"
)

// lsRemoteTimeout bounds the read of the remote branch.
const lsRemoteTimeout = time.Minute

// runEffect runs one pass of an effect stage (the engine's runner for
// config.FactoryStageEffect).
func (e *Engine) runEffect(ctx context.Context, t registry.Task, st config.FactoryStage) (bool, error) {
	if st.Effect == config.FactoryEffectPublish {
		return e.runPublish(ctx, t)
	}
	return false, nil
}

// publishKey is the key of the publish of task id at head sha.
func publishKey(id int64, sha string) string { return fmt.Sprintf("publish:%d:%s", id, sha) }

// runPublish publishes task t's head that you approved (your push on its
// ready card), once, to the branch and under the title the card showed
// (its y/N sentence named them; a key linked since changes neither): see
// the file's comment. A card that is not the one you pushed needs you. A
// build without GitHub or git leaves the task where it is.
func (e *Engine) runPublish(ctx context.Context, t registry.Task) (bool, error) {
	if e.d.GitHub == nil || e.d.Git == nil {
		return false, nil
	}
	if t.State == registry.TaskPublished {
		return true, nil
	}
	d, ok, err := e.lastDecision(ctx, t.ID)
	switch {
	case err != nil:
		return false, err
	case !ok || d.Choice != ChoicePush:
		return false, e.stageFailed(ctx, t, needsYou{"nothing to publish: you pushed no ready card of " + TaskKey(t)})
	}
	sha := d.HeadSHA
	key := publishKey(t.ID, sha)
	if eff, err := e.reg.EffectByKey(ctx, key); err == nil && eff.State == registry.EffectOK {
		return e.finishPublish(ctx, t, sha) // published; the task's move was cut
	} else if err != nil && !errors.Is(err, registry.ErrNotFound) {
		return false, err
	}
	login, err := e.prLogin(t)
	if err != nil {
		return false, e.stageFailed(ctx, t, err)
	}
	if _, why := e.readyProblem(t); why != "" {
		if back, err := e.qaBack(ctx, t, why); err != nil || back { // QA at the head first, then a card again
			return false, err
		}
		return false, e.stageFailed(ctx, t, needsYou{"nothing is pushed: " + why})
	}
	if t.HeadSHA != sha {
		return false, e.stageFailed(ctx, t, needsYou{fmt.Sprintf("nothing is pushed: the task's head is %s, not %s, which you approved",
			short(t.HeadSHA), short(sha))})
	}
	c, err := e.claimCopy(ctx, t)
	if err != nil {
		return false, err
	}
	repo, err := e.factoryRepo(t)
	if err != nil {
		return false, err
	}
	head, err := e.d.Git.RevParse(ctx, c.Path, "refs/heads/"+t.Branch)
	if err != nil {
		return false, err
	}
	if head != sha {
		return false, e.stageFailed(ctx, t, needsYou{fmt.Sprintf("the branch changed after you read it: %s is at %s, not at %s, which you "+
			"approved; nothing is pushed", t.Branch, short(head), short(sha))})
	}
	if rh, err := reportHash(e.d.Layout.FactoryTaskDir(t.ID)); err != nil {
		return false, err
	} else if rh != d.ReportHash {
		return false, e.stageFailed(ctx, t, needsYou{"the branch changed after you read it: " + reportFile + " or " + prBodyFile +
			" changed since your push; nothing is pushed"})
	}
	card, err := readCard(e.d.Layout.FactoryTaskDir(t.ID))
	if err != nil || card.Head != sha || card.ReportHash != d.ReportHash || card.DiffHash != d.DiffHash {
		return false, e.stageFailed(ctx, t, needsYou{"the ready card is not the one you pushed: nothing is pushed"})
	}
	pub := publishing{base: baseOf(repo), sha: sha, login: login, branch: cmp.Or(card.Remote, t.RemoteBranch, t.Branch),
		title: cmp.Or(card.Title, prTitle(t, e.titleFormat(t.Repo)))}
	git, err := e.operatorGit(ctx, t.Repo)
	if err != nil {
		return false, err
	}
	eff, run, err := e.beginEffect(ctx, key, "publish", t.ID)
	if err != nil {
		return false, err
	}
	if !run {
		return e.finishPublish(ctx, t, sha)
	}
	if eff.Resumed {
		e.event(ctx, t.ID, "publish", registry.PhaseInfo, "resumes the publish of "+short(sha)+" to "+pub.branch+
			": it reads the remote branch and its PRs first")
	}
	view, problem, err := e.publish(ctx, t, git, c.Path, pub)
	if err != nil || problem != "" {
		why := problem
		if err != nil {
			why = textx.Clip(textx.FirstLine(execx.Redact(err.Error())), 300)
		}
		if eerr := e.reg.EndEffect(ctx, key, registry.EffectFail, why); eerr != nil {
			e.log.Warn("end the publish effect", "task", t.ID, "err", eerr)
		}
		if err == nil {
			delete(e.publishFails, t.ID)
			return false, e.stageFailed(ctx, t, needsYou{problem})
		}
		// The effect's own bound, whatever the cause (the network's too, which
		// the stage's count leaves to the infrastructure hold): each try of a
		// publish costs GitHub calls.
		if n := e.publishFailed(t.ID, key); n >= stageFailLimit {
			delete(e.publishFails, t.ID)
			return false, e.stageFailed(ctx, t, needsYou{fmt.Sprintf("the publish of %s failed %d times in a row: %s", short(sha), n, why)})
		}
		return false, err
	}
	delete(e.publishFails, t.ID)
	if err := e.reg.LinkPR(ctx, registry.PRLink{TaskID: t.ID, Repo: t.Repo, Number: view.Number, Branch: pub.branch, OpenedHead: view.HeadSHA,
		ApprovedHead: sha}); err != nil {
		return false, err
	}
	if err := e.reg.EndEffect(ctx, key, registry.EffectOK, view.URL); err != nil {
		return false, err
	}
	return e.finishPublish(ctx, t, sha)
}

// publishFail is how many tries in a row of the publish effect key failed.
type publishFail struct {
	key string
	n   int
}

// publishFailed counts a failed try of task id's publish effect key and
// returns the tries in a row that failed (a new key starts again).
func (e *Engine) publishFailed(id int64, key string) int {
	pf := e.publishFails[id]
	if pf.key != key {
		pf = publishFail{key: key}
	}
	pf.n++
	e.publishFails[id] = pf
	return pf.n
}

// publishing is what a publish pushes and opens, as your push decided it:
// the head (sha) to the branch on the repository's base, the PR's title,
// and your login it opens as.
type publishing struct {
	base, sha, login, branch, title string
}

// publish pushes pub.sha to pub.branch from the copy at dir with git (as
// you: operatorGit) and opens its PR on pub.base as pub.login, each step
// guarded and evented: gh must act as your login first (ActiveLogin, before
// anything is pushed or opened), the remote branch and the PRs of the
// branch are read before any step, so a step done before is not done
// again, and nothing is forced. problem is what needs you (another gh
// account, a branch or a PR that is not the one you approved, the PR read
// back); err a failure to run, which the effect runs again after.
func (e *Engine) publish(ctx context.Context, t registry.Task, git *gitx.Client, dir string, pub publishing) (view PRView, problem string,
	err error) {
	gh, branch, base, sha, login := e.d.GitHub, pub.branch, pub.base, pub.sha, pub.login
	active, err := gh.ActiveLogin(ctx, t.Repo)
	if err != nil {
		return view, "", err
	}
	if !strings.EqualFold(active, login) {
		return view, fmt.Sprintf("gh acts as %s in your gh config dir, not as %s, the login the PR opens as: nothing is pushed "+
			"(gh auth switch --hostname github.com --user %s)", active, login, login), nil
	}
	remote, err := remoteHead(ctx, git, dir, branch)
	if err != nil {
		return view, "", err
	}
	pr, found, err := gh.PRByBranch(ctx, t.Repo, branch)
	if err != nil {
		return view, "", err
	}
	if found {
		if field := prMismatch(pr, login, base, sha); field != "" {
			return view, fmt.Sprintf("a PR of %s is open already, #%d, and it is not what you approved: %s; nothing is pushed over it",
				branch, pr.Number, field), nil
		}
	}

	step := func(name, begin string, run func() (string, error)) error {
		e.event(ctx, t.ID, "publish", registry.PhaseBegin, name+": "+begin)
		ok, err := run()
		if err != nil {
			e.event(ctx, t.ID, "publish", registry.PhaseFail, name+": "+textx.Clip(textx.FirstLine(execx.Redact(err.Error())), 300))
			return err
		}
		e.event(ctx, t.ID, "publish", registry.PhaseOK, name+": "+ok)
		return nil
	}
	switch remote {
	case sha:
		e.event(ctx, t.ID, "publish", registry.PhaseOK, "push: "+branch+" is at "+short(sha)+" on origin already")
	case "":
		if err := step("push", "pushes "+short(sha)+" of "+t.Branch+" to "+branch+" (a new branch)", func() (string, error) {
			return "pushed " + short(sha), git.PushLease(ctx, dir, sha, branch, "")
		}); err != nil {
			return view, "", err
		}
	default:
		return view, fmt.Sprintf("the remote branch %s is at %s, a head the factory did not push: nothing is pushed over it", branch,
			short(remote)), nil
	}
	if !found {
		title := pub.title
		if err := step("pr", "opens the PR of "+branch+" on "+base+", ready for review", func() (string, error) {
			var err error
			pr, err = gh.CreatePR(ctx, t.Repo, base, branch, title, filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), prBodyFile))
			return fmt.Sprintf("opened #%d", pr.Number), err
		}); err != nil {
			return view, "", err
		}
	}
	if err := step("read back", fmt.Sprintf("reads #%d back", pr.Number), func() (string, error) {
		var err error
		view, err = gh.ViewPR(ctx, t.Repo, pr.Number)
		return fmt.Sprintf("#%d by %s on %s at %s, draft %v", view.Number, view.Author, view.Base, short(view.HeadSHA), view.Draft), err
	}); err != nil {
		return view, "", err
	}
	if field := prMismatch(view, login, base, sha); field != "" {
		return view, field, nil
	}
	return view, "", nil
}

// prMismatch is the first field of PR v that is not what you approved
// ("" when it is): its author (your login), its base, its head (sha) and its
// ready state.
func prMismatch(v PRView, login, base, sha string) string {
	switch {
	case !strings.EqualFold(v.Author, login):
		return fmt.Sprintf("the PR's author is not what you approved: #%d is %s's, not %s's", v.Number, v.Author, login)
	case v.Base != base:
		return fmt.Sprintf("the PR's base is not what you approved: #%d targets %s, not %s", v.Number, v.Base, base)
	case v.HeadSHA != sha:
		return fmt.Sprintf("the PR's head is not what you approved: #%d is at %s, you approved %s", v.Number, short(v.HeadSHA), short(sha))
	case v.Draft:
		return fmt.Sprintf("the PR's draft is not what you approved: #%d is a draft, not ready for review", v.Number)
	}
	return ""
}

// remoteHead is branch's head on origin, read from the copy at dir with git
// (ls-remote): "" when origin has no such branch.
func remoteHead(ctx context.Context, git *gitx.Client, dir, branch string) (string, error) {
	ref := "refs/heads/" + branch
	out, err := git.LsRemote(ctx, dir, lsRemoteTimeout, ref)
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(out, "\n") {
		if sha, name, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok && name == ref {
			return sha, nil
		}
	}
	return "", nil
}

// finishPublish ends task t's publish of sha: the task is published with
// its PR's number, its agents are parked, its copy is released (and no
// longer on the task once released; a release refused is tried again,
// payReleaseDebts) and the queued tasks there is room for now start. A
// task published already is left as it is.
func (e *Engine) finishPublish(ctx context.Context, t registry.Task, sha string) (bool, error) {
	link, err := e.reg.LinkedPR(ctx, t.ID)
	if err != nil {
		return false, err
	}
	if t.State != registry.TaskPublished {
		if err := e.reg.TransitionTask(ctx, t.ID, liveStates, registry.TaskPublished, func(u *registry.TaskUpdate) {
			u.Set("pr_number", link.Number)
			u.Set("waits_who", "")
			u.Set("waits_what", "")
			u.Because(fmt.Sprintf("published #%d at %s", link.Number, short(sha)))
		}); err != nil {
			return false, err
		}
		e.event(ctx, t.ID, "publish", registry.PhaseOK, fmt.Sprintf("%s#%d is open, ready for review, at %s", link.Repo, link.Number, short(sha)))
	}
	if e.d.Runtime != nil {
		if err := e.d.Runtime.Park(ctx, t); err != nil {
			e.event(ctx, t.ID, "park", registry.PhaseFail, err.Error())
		}
	}
	if e.releaseCopy(ctx, t, "published") {
		e.copyReleased(ctx, t.ID)
	}
	e.admit(ctx)
	return true, nil
}

// copyReleased takes the copy off published task id once its release went
// through: it no longer counts as at work (atWork).
func (e *Engine) copyReleased(ctx context.Context, id int64) {
	if err := e.reg.TransitionTask(ctx, id, []string{registry.TaskPublished}, "", func(u *registry.TaskUpdate) { u.Set("copy", "") }); err != nil {
		e.log.Warn("take a released copy off its task", "task", id, "err", err)
	}
}
