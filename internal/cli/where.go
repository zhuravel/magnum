package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/store"
)

const whereUsage = "<ref>|<slot>"

func newWhereCmd(c *Context) *cobra.Command {
	cmd := newCommand(groupInspect, "where "+whereUsage, "print the folder of a PR or slot (cd $(magnum where N))",
		"Print the folder of a PR or slot, for `cd $(magnum where 123)`. A PR is found in the slot that holds it, "+
			"else in a manual worktree whose PR number is confirmed (git config branch.<b>.pr or a matching head); "+
			"a note on stderr explains such answers.",
		func(pos []string) int { return runWhere(c, pos) })
	cmd.ValidArgsFunction = completeFirst(c.completePRs, c.completeSlots)
	return cmd
}

func runWhere(c *Context, pos []string) int {
	if len(pos) != 1 {
		return inspUsage(c, "where", "need exactly one PR reference or slot name", whereUsage)
	}
	a, err := inspOpenApp(c, false)
	if err != nil {
		return cmdFail(c, "where", err)
	}
	defer a.Close()
	path, note, err := whereFind(context.Background(), a.Store, a.Refs(), a.Inventory, pos[0])
	if err != nil {
		return cmdFail(c, "where", err)
	}
	if note != "" {
		fmt.Fprintln(c.Stderr, note)
	}
	fmt.Fprintln(c.Stdout, path)
	return 0
}

// whereFind returns the folder ref lives in: a slot by name, the slot
// holding a PR, else a manual worktree whose PR number is confirmed (git
// config branch.<b>.pr or a matching head). note explains non-slot answers.
func whereFind(ctx context.Context, st *store.Store, refs app.RefParser, scan statusScanner, ref string) (path, note string, err error) {
	t, rerr := inspResolveIn(ctx, st, refs, ref)
	if rerr == nil && t.Slot != nil {
		return whereSlotPath(*t.Slot)
	}
	var repo string
	var number int
	if rerr == nil {
		repo, number = t.Repo.FullName(), t.PR.Number
		sl, err := slotOfPR(ctx, st, t.PR.ID)
		if err != nil {
			return "", "", err
		}
		if sl != nil {
			return whereSlotPath(*sl)
		}
	} else {
		if !errors.Is(rerr, store.ErrNotFound) {
			return "", "", rerr
		}
		owner, name, n, perr := refs.ResolvePR(ctx, ref)
		if perr != nil {
			return "", "", rerr
		}
		repo, number = owner+"/"+name, n
	}
	if scan != nil {
		if inv, err := scan.Scan(ctx, inventory.Options{External: true}); err == nil {
			for _, x := range inv.External {
				if x.Exists && x.PRConfirmed && x.PRNumber == number && strings.EqualFold(x.Repo, repo) {
					return x.Path, fmt.Sprintf("%s#%d is checked out in the manual worktree %s (magnum does not manage it)",
						repo, number, inspTilde(x.Path)), nil
				}
			}
		}
	}
	if rerr != nil {
		return "", "", rerr
	}
	msg := fmt.Sprintf("%s has no folder (state %s)", t.Label(), t.PR.State)
	if as, err := st.AssignmentsByPR(ctx, t.PR.ID); err == nil && len(as) > 0 {
		msg += "; it was last in " + inspTilde(as[len(as)-1].Path)
	}
	if t.PR.GHState == store.GHOpen {
		msg += fmt.Sprintf("; its next round checks it out again (`magnum review %s#%d` forces one)", t.Repo.FullName(), t.PR.Number)
	}
	return "", "", errors.New(msg)
}

// whereSlotPath returns a slot's folder when it exists on disk.
func whereSlotPath(sl store.Slot) (string, string, error) {
	if sl.State == store.SlotRemoved {
		return "", "", fmt.Errorf("slot %s was removed", sl.Name)
	}
	if _, err := os.Stat(sl.Path); err != nil {
		fix := "`magnum slots repair " + sl.Name + "`"
		if sl.Kind == store.SlotKindPerPR {
			fix = "`magnum cleanup --slot '" + sl.Name + "'` and review it again"
		}
		return "", "", fmt.Errorf("slot %s: folder %s does not exist (state %s); fix: %s", sl.Name, inspTilde(sl.Path), sl.State, fix)
	}
	return sl.Path, "", nil
}
