package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/textx"
)

// No role may edit the checkout: the reviewers and the read-only simplify
// write only their reports, and the judge reads the code the PR's head has.
// A role that edits it anyway (a stray fix, a branch switch) is caught after
// its stage and the checkout restored before anything else runs on it.

// treeState is the checkout's HEAD and how many entries `git status` lists.
type treeState struct {
	head   string
	status gitx.Status
}

// noteTree records the checkout as the stages find it, which checkTree
// compares against: a readiness command that left a file modified is not
// taken for a role's edit. Without Git, or when git fails, nothing is
// recorded and the stages go unchecked.
func (rd *round) noteTree(ctx context.Context) {
	rd.tree = nil
	if rd.r.Git == nil {
		return
	}
	if t, err := rd.readTree(ctx); err != nil {
		rd.logErr(ctx, err, "pipeline: %s: read the checkout before the reviewers: %v", rd.subject, err)
	} else {
		rd.tree = &t
	}
}

func (rd *round) readTree(ctx context.Context) (treeState, error) {
	ctx = context.WithoutCancel(ctx)
	head, err := rd.r.Git.RevParse(ctx, rd.in.SlotPath, "HEAD")
	if err != nil {
		return treeState{}, err
	}
	st, err := rd.r.Git.Status(ctx, rd.in.SlotPath)
	if err != nil {
		return treeState{}, err
	}
	return treeState{head: head, status: st}, nil
}

// checkTree verifies that roles left HEAD and the tree as noteTree found
// them. When not, a round.checkout_dirty event names them and what changed,
// and the checkout is restored (restoreTree); the error is a restore that
// failed.
func (rd *round) checkTree(ctx context.Context, roles []string) error {
	if rd.tree == nil || len(roles) == 0 {
		return nil
	}
	now, err := rd.readTree(ctx)
	if err != nil {
		rd.logErr(ctx, err, "pipeline: %s: read the checkout after %s: %v", rd.subject, strings.Join(roles, ", "), err)
		return nil
	}
	if now == *rd.tree {
		return nil
	}
	var what []string
	if now.head != rd.tree.head {
		what = append(what, "HEAD moved to "+textx.ShortSHA(now.head))
	}
	if now.status != rd.tree.status {
		what = append(what, fmt.Sprintf("%d tracked and %d untracked changes", now.status.Tracked, now.status.Untracked))
	}
	rd.event(ctx, "warn", "round.checkout_dirty",
		fmt.Sprintf("%s left the checkout modified (%s): restoring %s", strings.Join(roles, ", "), strings.Join(what, ", "), textx.ShortSHA(rd.in.TargetSHA)),
		map[string]any{"roles": roles, "head": now.head, "tracked": now.status.Tracked, "untracked": now.status.Untracked})
	if err := rd.restoreTree(ctx); err != nil {
		return err
	}
	rd.tree = &treeState{head: rd.in.TargetSHA}
	return nil
}

// restoreTree discards edits to the checkout, staged ones included, and puts
// HEAD back on TargetSHA: `git reset --hard` and `git clean -fd` (relative to
// HEAD, so a branch a role switched to is never moved), then HEAD switched
// back to TargetSHA when it moved. A checkout still dirty afterwards is an
// error.
func (rd *round) restoreTree(ctx context.Context) error {
	ctx = context.WithoutCancel(ctx)
	if rd.r.Exec == nil {
		return fmt.Errorf("pipeline: restore the checkout: no Exec")
	}
	slot, target := rd.in.SlotPath, rd.in.TargetSHA
	for _, args := range [][]string{{"reset", "--hard", "--quiet"}, {"clean", "-fd"}} {
		if _, err := rd.r.Exec.Run(ctx, rd.git(true, args...)); err != nil {
			return fmt.Errorf("pipeline: restore the checkout: git %s: %w", strings.Join(args, " "), err)
		}
	}
	head, err := rd.r.Git.RevParse(ctx, slot, "HEAD")
	if err != nil {
		return fmt.Errorf("pipeline: restore the checkout: %w", err)
	}
	if head != target {
		if err := rd.r.Git.SwitchDetach(ctx, slot, target); err != nil {
			return fmt.Errorf("pipeline: restore the checkout: %w", err)
		}
		if head, err = rd.r.Git.RevParse(ctx, slot, "HEAD"); err != nil {
			return fmt.Errorf("pipeline: restore the checkout: %w", err)
		}
		if head != target {
			return fmt.Errorf("pipeline: restore the checkout: HEAD is %s, want %s", textx.ShortSHA(head), textx.ShortSHA(target))
		}
	}
	if st, err := rd.r.Git.Status(ctx, slot); err != nil {
		rd.warn(ctx, "status after restoring the checkout: %v", err)
	} else if st.Dirty() {
		return fmt.Errorf("pipeline: restore the checkout: still dirty (%d tracked, %d untracked)", st.Tracked, st.Untracked)
	}
	return nil
}

// git builds a `git -C <slot>` command. It unsets the variables that would
// point git at another repository, as gitx does for its own commands.
func (rd *round) git(mutates bool, args ...string) execx.Cmd {
	return execx.Cmd{
		Name:    "git",
		Args:    append([]string{"-C", rd.in.SlotPath}, args...),
		Env:     map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		Unset:   gitx.ScrubbedEnv(),
		Mutates: mutates,
		Label:   "pipeline checkout",
	}
}
