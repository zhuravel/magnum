package factory

// Where a change starts on GitHub (F2, the follow-up of the base merge's
// check, 2026-11-14). The copies are worktrees of the main clone, and the
// writer's pane can rewrite the main clone's remote config
// (remote.origin.url, a url.<x>.insteadOf): a fetch of origin/<base> then
// brings a commit of the writer's own, and a local round's diff from the
// merge base with it would leave that commit's change unreviewed, as the
// ready card's flags would read the change and its conflicts against it.
// GitHub's PR diff starts at the real base, so magnum's review on GitHub
// still sees it, but the factory's own checks would not. So the local
// rounds and the card's flags take the commit the base is at on GitHub,
// read as you (githubBase, the base merge's read), and the merge base of
// the head with it, in a repository of the factory's own with the copy's
// objects (gitx.Isolated: no commit-graph, replace ref or attribute of the
// copy's). A read that fails, and a commit the copy has no object of after
// the fetch, start no round and draw no card: the stage fails with why, is
// tried again a poll later, and the task needs you after a few tries
// (taskFailed), never a wait in silence; a read GitHub's connection or
// server failed (gitHubDown) needs you only after an infrastructure hold's
// wait (holdNeedsYou).

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
)

// roundBase is the base round `round` of task t's local review diffs head
// from in copy c: the merge base of head with the commit the base is at on
// GitHub (baseOnGitHub). Without one the round does not start on a guessed
// base: the task waits saying why, and the error fails the stage's pass,
// which is tried again a poll later and needs you after a few tries
// (taskFailed).
func (e *Engine) roundBase(ctx context.Context, t registry.Task, c Copy, repo config.FactoryRepo, head string, round int) (string, error) {
	b, err := e.baseOnGitHub(ctx, repo, c, head, nil)
	switch {
	case err == nil:
		return b.base, nil
	case ctx.Err() != nil:
		return "", ctx.Err()
	}
	err = fmt.Errorf("round %d of the local review cannot start: %w", round, err)
	e.waitOn(ctx, t, waitReview, roundError(err.Error())+"; the factory tries again at the next poll")
	return "", err
}

// gitHubDown is a read of the base on GitHub that failed for GitHub's
// connection or its server, not for the task (github.ConnectionCause: the
// network, a 5xx, a 499, a rate limit; cause names it): the stage whose
// pass it failed waits for GitHub as long as an infrastructure hold waits
// before it needs you (holdNeedsYou: taskFailed), not stageFailLimit's few
// tries, which would send every task at a round's start or at its card to
// you after five minutes of a GitHub outage. A cause of the network's
// alone (DNS, a refused connection) is still the infrastructure hold's,
// which reads the same text (infraCause) and holds every stage.
type gitHubDown struct {
	cause string
	err   error
}

func (g gitHubDown) Error() string { return g.err.Error() }
func (g gitHubDown) Unwrap() error { return g.err }

// gitHubBase is where a change at a head starts on GitHub: tip, the commit
// the repository's base is at there, name that commit in words ("master on
// GitHub at <sha>"), and base, the merge base of the head with tip.
type gitHubBase struct{ tip, name, base string }

// baseOnGitHub is where the change at head in copy c starts on GitHub: the
// commit repo's base is at on GitHub (githubBase, read as you), fetched to
// the main clone after the read (a commit GitHub has and the copy does not
// yet, as after the base moved, comes with the forced fetch of
// origin/<base>), and the merge base of head with it, read in a repository
// of the factory's own with the copy's objects (Isolated); also, when set,
// runs there too, with the base found. The error says why there is none:
// GitHub not read, the fetch failed, a commit the copy has no object of
// after the fetch (the main clone's remote reaches another repository than
// GitHub's), no merge base.
func (e *Engine) baseOnGitHub(ctx context.Context, repo config.FactoryRepo, c Copy, head string,
	also func(iso string, b gitHubBase) error) (gitHubBase, error) {
	tip, why, err := e.githubBase(ctx, repo)
	switch {
	case why != "" && err != nil:
		err = fmt.Errorf("%s: %w", why, err)
		if cause := github.ConnectionCause(err.Error()); cause != "" {
			return gitHubBase{}, gitHubDown{cause: cause, err: err}
		}
		return gitHubBase{}, err
	case why != "":
		return gitHubBase{}, errors.New(why)
	}
	git, base := e.d.Git, baseOf(repo)
	if err := git.FetchBranch(ctx, cmp.Or(repo.MainClone, c.Path), base); err != nil {
		return gitHubBase{}, fmt.Errorf("git cannot fetch origin/%s to read %s: %w", base, tip.name, err)
	}
	b := gitHubBase{tip: tip.sha, name: tip.name}
	err = git.Isolated(ctx, c.Path, func(iso string) error {
		switch _, err := git.RevParse(ctx, iso, b.tip); {
		case errors.Is(err, gitx.ErrNoSuchRef):
			return fmt.Errorf("the copy has no commit of %s, even after a fetch of origin/%s: the main clone's remote reaches another "+
				"repository than GitHub's", b.name, base)
		case err != nil:
			return fmt.Errorf("git cannot read %s: %w", b.name, err)
		}
		mb, err := git.MergeBase(ctx, iso, head, b.tip)
		if err != nil {
			return fmt.Errorf("git cannot find where %s starts from %s: %w", short(head), b.name, err)
		}
		b.base = mb
		if also == nil {
			return nil
		}
		return also(iso, b)
	})
	return b, err
}
