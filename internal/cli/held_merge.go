package cli

// The merge of the parent that magnum holds for your y (the engine's
// fix_held_merge.go), as the screens show it: its held-merge.diff, which
// e opens in the pager, and the "merge ready" of the row that needs you
// for it.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// heldMergeDiffs are the held-merge.diff files of the merges magnum holds
// for your y, by PR id: each in layout's report directory of its PR
// (paths.Layout.PRReviews), never a path the registry's row names; nil
// when the registry cannot say or the layout names no directory.
func heldMergeDiffs(ctx context.Context, st *store.Store, layout paths.Layout) map[int64]string {
	if st == nil || !layout.Valid() {
		return nil
	}
	vals, err := st.KVValues(ctx, "pr.*.held_merge")
	if err != nil {
		return nil
	}
	var out map[int64]string
	for key, raw := range vals {
		var id int64
		h, ok := engine.ParseHeldMerge(raw)
		if _, err := fmt.Sscanf(key, "pr.%d.", &id); !ok || err != nil || engine.KVPRHeldMerge(id) != key {
			continue
		}
		owner, name, ok := strings.Cut(h.Repo, "/")
		if !ok || h.Number <= 0 {
			continue
		}
		if out == nil {
			out = map[int64]string{}
		}
		out[id] = filepath.Join(layout.PRReviews(owner, name, h.Number), engine.HeldMergeFile)
	}
	return out
}
