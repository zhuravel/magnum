package tui

// A merge of the parent that magnum holds for your y (the engine's
// fix_held_merge.go): claude-fix resolved it, and it keeps the PR's own
// workflow changes, so the fixer App pushes it only after you read it and
// said y. e on its row opens its held-merge.diff in the pager, asking
// nothing (the board's and the dashboard's; the factory screen's e opens
// it before the fixer's report while it is held), P asks the y/N of its
// push, which the y binds to the merge (MergeAsked.Merge), and the board's
// state cell names the attention "merge ready".

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
)

const (
	// heldMergeKey opens a held merge's diff on the board's and the
	// dashboard's rows.
	heldMergeKey = "e"
	// heldMergeName is the file it opens (the engine's HeldMergeFile).
	heldMergeName = "held-merge.diff"
	// mergeReadyCause is the attention's word for a held merge on the
	// board's state cell, and how its attention's words start after "fix: "
	// (the attention package's MergeReadyText).
	mergeReadyCause = "merge ready"
)

// heldPagerMsg ends the pager on a held merge's diff.
type heldPagerMsg struct{ err error }

// mergeReadyShown reports whether r's attention is a held merge's: it
// needs attention for the fixer's work (kind fix), its words say "merge
// ready", and magnum holds the merge (HeldMerge).
func mergeReadyShown(r PRBoardRow) bool {
	return r.HeldMerge != "" && normState(rowState(r)) == "needs_attention" && r.AttentionKind == "fix" &&
		strings.HasPrefix(strings.TrimPrefix(r.LastError, "fix: "), mergeReadyCause+": ")
}

// openHeldMerge runs the pager on path, the held-merge.diff of label's
// merge of the parent that magnum holds for your y, asking nothing; a row
// without one says so in the footer.
func (b *actionBar) openHeldMerge(label, path string) tea.Cmd {
	if path == "" || !filepath.IsAbs(path) {
		return b.note(fmt.Sprintf("no merge of the parent is held for your y on %s: %s opens its %s while magnum holds one",
			cmp.Or(label, "this row"), heldMergeKey, heldMergeName))
	}
	return tea.ExecProcess(b.pager(b.ctx, path), func(err error) tea.Msg { return heldPagerMsg{err: err} })
}

// heldPagerDone is the end of the pager on a held merge's diff: a pager
// that failed says so until a key is pressed.
func (b *actionBar) heldPagerDone(msg heldPagerMsg) tea.Cmd {
	if msg.err != nil {
		return b.failure(heldMergeName + ": the pager failed: " + errLine(msg.err))
	}
	return nil
}

// withHeldHint is sets with e and held-merge.diff first in each, when
// held (the selected row's held merge) names one.
func withHeldHint(held string, sets [][]hint) [][]hint {
	if held == "" {
		return sets
	}
	out := make([][]hint, len(sets))
	for i, s := range sets {
		out[i] = slices.Insert(slices.Clone(s), 0, hint{heldMergeKey, heldMergeName})
	}
	return out
}
