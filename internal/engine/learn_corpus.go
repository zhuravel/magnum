package engine

// The learned corpus (DECISIONS "The daemon keeps a learned eval corpus"):
// after every retro the daemon rewrites paths.Layout.LearnCorpus from the
// registry, one eval case per general miss magnum should have found and per
// general finding an author declined as wrong or ranked too high
// (learn.CaseFromMiss, learn.CaseFromDecline), so `magnum eval` and the
// gate of a skill change replay what magnum learned from.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"

	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/store"
)

// syncLearnedCorpus rewrites the learned corpus from the registry: a case
// per miss and per decline learn makes one of (new or used, general), oldest
// row first, the misses before the declines, leaving out a PR no watch
// covers (a replay would have no roles or identity) and one Codex flagged,
// or whose flag cannot be read (never replayed). A row dismissed since drops
// out. A file that would not change is left as it is; no case removes it.
func (e *Engine) syncLearnedCorpus(ctx context.Context) error {
	misses, err := e.st.Misses(ctx, store.MissFilter{Classes: []string{store.MissMiss}, Scopes: []string{store.MissScopeGeneral},
		States: []string{store.MissNew, store.MissUsed}})
	if err != nil {
		return fmt.Errorf("learned corpus: %w", err)
	}
	declines, err := e.st.Declines(ctx, store.DeclineFilter{Verdicts: []string{store.VerdictWrong, store.VerdictOverRanked},
		Scopes: []string{store.MissScopeGeneral}, States: []string{store.MissNew, store.MissUsed}})
	if err != nil {
		return fmt.Errorf("learned corpus: %w", err)
	}
	flagged := map[int64]bool{}
	replayable := func(prID int64, repo string) bool {
		if e.cfg.WatchFor(repo) == nil {
			return false
		}
		f, seen := flagged[prID]
		if !seen {
			_, f = e.codexFlag(ctx, prID)
			flagged[prID] = f
		}
		return !f
	}
	var cases []eval.Case
	for _, m := range slices.Backward(misses) { // Misses is newest first
		if c, ok := learn.CaseFromMiss(m); ok && replayable(m.PRID, m.Repo) {
			cases = append(cases, c)
		}
	}
	for _, d := range slices.Backward(declines) {
		if c, ok := learn.CaseFromDecline(d); ok && replayable(d.PRID, d.Repo) {
			cases = append(cases, c)
		}
	}
	path := e.d.Layout.LearnCorpus()
	if len(cases) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("learned corpus: %w", err)
		}
		return nil
	}
	return eval.WriteCorpus(path, eval.Corpus{Cases: cases})
}
