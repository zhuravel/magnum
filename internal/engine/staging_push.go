package engine

// What a staging deploy (internal/staging) takes of the review daemon's
// identities: who its merge commits are by (CommitIdentity) and the env of
// its push (StagingPushEnv), the shapes of the staging package's
// Deps.CommitIdentity and Deps.PushEnv, so the daemon's staging wiring
// passes them as they are. Both act as the fix identity of the watch that
// covers the repository, a dedicated fixer App: every push magnum makes on
// a PR's behalf is its bot's.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/identity"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// stagingFixer is the fix identity repo's pushes and merge commits go out
// as: the watch's fix role's, a dedicated fixer App with its bot's user id
// whose tokens reach repo, and its bot's signature.
func (e *Engine) stagingFixer(repo string) (*config.Identity, gitx.Signature, error) {
	w := e.cfg.WatchFor(repo)
	if w == nil {
		return nil, gitx.Signature{}, fmt.Errorf("%s is not watched (no [[watch]] covers it)", repo)
	}
	role, ok := e.cfg.FixRoleFor(w)
	if !ok {
		return nil, gitx.Signature{}, fmt.Errorf("no fix role for %s", repo)
	}
	id, who, app := e.fixerSignature(role.Identity)
	if !app {
		return nil, gitx.Signature{}, fmt.Errorf("the fix role's identity %s is no fixer App with its bot's user id: magnum commits and pushes for %s as one", role.Identity, repo)
	}
	_, name, _ := strings.Cut(repo, "/")
	if !slices.ContainsFunc(id.TokenRepositories, func(r string) bool { return strings.EqualFold(r, name) }) {
		return nil, gitx.Signature{}, fmt.Errorf("the fixer App %s's tokens do not reach %s: add %s to its token_repositories", id.Name, repo, name)
	}
	return id, who, nil
}

// CommitIdentity is who a merge magnum commits for repo ("owner/name") is
// by, author and committer alike: the fix identity's bot, its login and
// its noreply email (<user_id>+<login>@users.noreply.github.com), which
// GitHub links to the bot, so the commit is attributed. It is the staging
// package's Deps.CommitIdentity; an error says why there is no such bot.
func (e *Engine) CommitIdentity(_ context.Context, repo string) (name, email string, err error) {
	_, who, err := e.stagingFixer(repo)
	if err != nil {
		return "", "", err
	}
	return who.Name, who.Email, nil
}

// StagingPushEnv is the staging package's Deps.PushEnv over mirror (the
// dir of each repository's mirror, where the merge is: staging.MirrorDir):
// the git env of one push of mergeSHA, a merge of a PR's head onto
// target's tip ([tip, head]), to target, as the fix identity's bot. It
// refuses a target the bot never pushes to (master, main, the default
// branch), a merge that is not on target's tip in the mirror, and a merge
// carrying a .github/workflows file that differs from both the tip's and
// the PR's base tip's (the PR whose head is the merge's second parent; a
// PR that changes its own workflow files is never pushed by the bot). A
// merge whose workflow files are the tip's goes with the App's token; one
// that brings the base's goes with the daemon's own token with Workflows
// write (identity.App.DaemonPushEnv, never a pane's gh config dir), which
// the App revokes once its hold is over, since the push's caller has no
// release; while the App lacks Workflows it is refused with "needs
// Workflows on the App".
func (e *Engine) StagingPushEnv(mirror func(repo string) (dir string, ok bool)) func(ctx context.Context, repo, mergeSHA, target string) (map[string]string, error) {
	return func(ctx context.Context, repo, mergeSHA, target string) (map[string]string, error) {
		env, err := e.stagingPushEnv(ctx, mirror, repo, mergeSHA, target)
		if err != nil {
			return nil, fmt.Errorf("push of %s to %s: %w", textx.ShortSHA(mergeSHA), target, err)
		}
		return env, nil
	}
}

func (e *Engine) stagingPushEnv(ctx context.Context, mirror func(string) (string, bool), repo, merge, target string) (map[string]string, error) {
	id, _, err := e.stagingFixer(repo)
	if err != nil {
		return nil, err
	}
	r, err := e.st.RepoByFullName(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", repo, err)
	}
	pg, ok := e.d.Git.(parentGit)
	dir, has := "", false
	if mirror != nil {
		dir, has = mirror(repo)
	}
	switch {
	case !fullSHA.MatchString(merge):
		return nil, fmt.Errorf("%q is no full commit id", merge)
	case !gitx.ShellSafeRef(target):
		return nil, fmt.Errorf("%q is no branch name", target)
	case slices.ContainsFunc([]string{"master", "main", r.DefaultBranch}, func(b string) bool { return b != "" && strings.EqualFold(b, target) }):
		return nil, fmt.Errorf("magnum never pushes to %s (master, main or the default branch)", target)
	case !ok:
		return nil, errors.New("magnum has no git that compares a merge's files")
	case !has:
		return nil, fmt.Errorf("no mirror of %s", repo)
	}
	tip, err := e.d.Git.RevParse(ctx, dir, "refs/heads/"+target)
	if err != nil {
		return nil, fmt.Errorf("the tip of %s: %w", target, err)
	}
	first, err := e.d.Git.RevParse(ctx, dir, merge+"^1")
	switch {
	case err != nil:
		return nil, fmt.Errorf("the merge's first parent: %w", err)
	case first != tip:
		return nil, fmt.Errorf("the merge is on %s, not on %s's tip %s", textx.ShortSHA(first), target, textx.ShortSHA(tip))
	}
	differ, err := pg.DifferingPaths(ctx, dir, merge, tip, workflowsPath)
	if err != nil {
		return nil, fmt.Errorf("the merge's workflow files: %w", err)
	}
	src := e.d.Identities[id.Name]
	if src == nil {
		return nil, fmt.Errorf("identity %q is not configured", id.Name)
	}
	if len(differ) == 0 { // it changes no workflow file on target
		env, err := src.Env(ctx)
		if err != nil {
			return nil, fmt.Errorf("the token of %s: %w", id.Name, err)
		}
		return env, nil
	}
	base, baseTip := e.stagingBase(ctx, dir, r, merge)
	if baseTip != "" {
		if differ, err = pg.DifferingPaths(ctx, dir, merge, baseTip, differ...); err != nil {
			return nil, fmt.Errorf("the merge's workflow files against %s: %w", base, err)
		}
	}
	if len(differ) > 0 {
		against := "the PR's base, which magnum does not know"
		if baseTip != "" {
			against = base + "'s"
		}
		return nil, fmt.Errorf("the merge carries workflow files that differ from both %s's and %s (%s): the bot never pushes a PR's own workflow changes",
			target, against, strings.Join(differ[:min(3, len(differ))], ", "))
	}
	dp, ok := src.(daemonPusher)
	if !ok {
		return nil, fmt.Errorf("the merge brings %s's workflow changes, and %s mints no token that may write them", base, id.Name)
	}
	env, _, err := dp.DaemonPushEnv(ctx, true) // the App revokes it after its hold: PushEnv's caller has no release
	switch {
	case errors.Is(err, identity.ErrWorkflowsNotGranted):
		return nil, fmt.Errorf("the merge brings %s's workflow changes: needs Workflows on the App (%s lacks it)", base, id.Login)
	case err != nil:
		return nil, fmt.Errorf("a push token with Workflows for %s: %w", id.Name, err)
	}
	return env, nil
}

// stagingBase is the base of the open PR of r whose head is merge's second
// parent, and that base's tip in the mirror at dir ("" when either is not
// known: no such PR, no base, a mirror without the branch).
func (e *Engine) stagingBase(ctx context.Context, dir string, r store.Repo, merge string) (string, string) {
	head, err := e.d.Git.RevParse(ctx, dir, merge+"^2")
	if err != nil {
		return "", ""
	}
	prs, err := e.st.ListPRs(ctx, store.PRFilter{RepoID: r.ID, GHOpen: true})
	if err != nil {
		return "", ""
	}
	i := slices.IndexFunc(prs, func(p store.PR) bool { return p.HeadSHA == head && deref(p.BaseRef) != "" })
	if i < 0 {
		return "", ""
	}
	base := deref(prs[i].BaseRef)
	if !gitx.ShellSafeRef(base) {
		return "", ""
	}
	tip, err := e.d.Git.RevParse(ctx, dir, "refs/heads/"+base)
	if err != nil {
		return base, ""
	}
	return base, tip
}
