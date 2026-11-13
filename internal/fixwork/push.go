package fixwork

// The checks of a push of a merge of the PR's parent, which the daemon
// makes as the fixer App (the engine's pushMerge, fix_parent.go): never to
// a branch magnum does not push to, a merge that brings the base's
// workflow changes only as that base's tip on GitHub has them, read in a
// repository of the daemon's own on objects hashed again, and why nothing
// was pushed (PushRefusal).

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// WorkflowsPath is where a repository's GitHub workflows live: a merge
// that changes it is not the fixer App's to push.
const WorkflowsPath = ".github/workflows"

// IsolatedGit reads a clone's objects where none of the clone's own
// settings apply, and checks them (*gitx.Client has it): Isolated runs its
// func in a bare repository of its own that shares the clone's objects
// (no config, attribute, merge driver or ref of the clone's, which the
// fixer's pane can write), and VerifyObjects hashes again every object a
// read of a path at some commits goes through (gitx.ErrObjectMismatch for
// one whose content is not its name: git never hashes an object again
// when it reads it, while GitHub has the real one).
type IsolatedGit interface {
	Isolated(ctx context.Context, dir string, fn func(iso string) error) error
	VerifyObjects(ctx context.Context, dir, path string, commits ...string) error
}

// WorkflowRead is read of a merge's workflow files in dir's objects, in a
// repository of the daemon's own (IsolatedGit.Isolated), after every
// object of the workflow files at commits is hashed again
// (VerifyObjects): what decides or shows a push with the Workflows token
// is what GitHub would get of it. A forged object is
// gitx.ErrObjectMismatch (ForgedWords).
func WorkflowRead[T any](ctx context.Context, g IsolatedGit, dir string, commits []string, read func(iso string) (T, error)) (T, error) {
	var out T
	err := g.Isolated(ctx, dir, func(iso string) error {
		if err := g.VerifyObjects(ctx, iso, WorkflowsPath, commits...); err != nil {
			return err
		}
		var err error
		out, err = read(iso)
		return err
	})
	return out, err
}

// ForgedWords says why magnum pushes or holds nothing of at (the merge in
// words): err, an object a read of its workflow files went through, is not
// what its name says (gitx.ErrObjectMismatch), so what magnum read of the
// clone is not what GitHub would get.
func ForgedWords(at string, err error) string {
	return fmt.Sprintf("an object of the workflow files of %s in the clone is not what its name says (%v), so what magnum read of it is "+
		"not what GitHub would get", at, err)
}

// PushRefusal is why the engine's pushMerge pushed nothing (Why; "" when
// it pushed): for a merge of the tip GitHub names for the base whose only
// fault is that it keeps the PR's own workflow changes, Own names them
// (the workflow files that differ from that tip) and Files the workflow
// files the push changes against the head, so the settle may hold it for
// your y (holdMerge); both are empty for any other refusal.
type PushRefusal struct {
	Why   string
	Own   []string
	Files []string
}

// OwnWorkflowsRefusal is pushMerge's refusal of at (the merge in words),
// a merge of b that brings the base's workflow changes (workflows: its
// workflow files, against the head), whose workflow files own differ from
// tip, the base's tip on GitHub, as login, the identity that would push
// it: one that is not of that tip is never pushed; one of that tip whose
// only fault is that it keeps the PR's own workflow changes names them and
// its workflow files, so the settle may hold it for your y. Zero when own
// is empty: nothing to refuse.
func OwnWorkflowsRefusal(at string, b Conflict, tip, login string, own, workflows []string) PushRefusal {
	switch {
	case len(own) > 0 && tip != b.SHA:
		return PushRefusal{Why: fmt.Sprintf("%s has workflow files that are not as the tip %s of %s on GitHub has them (%s), which %s never pushes",
			at, textx.ShortSHA(tip), b.Base, strings.Join(own[:min(3, len(own))], ", "), login)}
	case len(own) > 0:
		return PushRefusal{Why: fmt.Sprintf("%s keeps the PR's own workflow changes (%s), which %s never pushes", at,
			strings.Join(own[:min(3, len(own))], ", "), login), Own: own, Files: workflows}
	}
	return PushRefusal{}
}

// RefusedHeadBranch says why magnum never pushes to pr's branch, "" when
// it may: master, main, repo's default branch and the PR's base (its
// row's, and base, the one the job merges), as `magnum push` refuses them
// (internal/cli/push.go). A PR from main into production is not the bot's
// to push to.
func RefusedHeadBranch(repo store.Repo, pr store.PR, base string) string {
	branch := store.Deref(pr.HeadRef)
	if slices.ContainsFunc([]string{"master", "main", repo.DefaultBranch, store.Deref(pr.BaseRef), base}, func(b string) bool { return b != "" && strings.EqualFold(b, branch) }) {
		return fmt.Sprintf("magnum never pushes to %s (master, main, the default branch or the PR's base)", branch)
	}
	return ""
}
