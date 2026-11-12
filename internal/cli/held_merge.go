package cli

// The merge of the parent that magnum holds for your y (the engine's
// fix_held_merge.go), as the screens show it: its held-merge.diff, which
// e opens in the pager, and the "merge ready" of the row that needs you
// for it.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// heldMergeIDs are the PRs, by registry id, whose merge of the parent
// magnum holds for your y (engine.KVPRHeldMerge rows that read); nil when
// the registry cannot say.
func heldMergeIDs(ctx context.Context, st *store.Store) map[int64]bool {
	if st == nil {
		return nil
	}
	vals, err := st.KVValues(ctx, "pr.*.held_merge")
	if err != nil {
		return nil
	}
	var out map[int64]bool
	for key, raw := range vals {
		var id int64
		if _, err := fmt.Sscanf(key, "pr.%d.", &id); err != nil || engine.KVPRHeldMerge(id) != key {
			continue
		}
		if _, ok := engine.ParseHeldMerge(raw); !ok {
			continue
		}
		if out == nil {
			out = map[int64]bool{}
		}
		out[id] = true
	}
	return out
}

// heldMergeDiff is the held-merge.diff of PR number of owner/name in
// layout's report directory of the PR (by the PR's and the repository's
// rows, never a path a held merge's row names), when a regular file is
// there, looked at through an os.Root of the reviews directory so that no
// link leads out of it, as the factory screen looks; "" for none.
func heldMergeDiff(layout paths.Layout, owner, name string, number int) string {
	if !layout.Valid() {
		return ""
	}
	path := filepath.Join(layout.PRReviews(owner, name, number), engine.HeldMergeFile)
	rel, err := filepath.Rel(layout.Reviews(), path)
	if err != nil || !filepath.IsLocal(rel) {
		return ""
	}
	root, err := os.OpenRoot(layout.Reviews())
	if err != nil {
		return ""
	}
	defer root.Close()
	if fi, err := root.Lstat(rel); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	return path
}
