package factory

import (
	"time"

	"github.com/zhuravel/magnum/internal/slots"
)

// pruneSaves removes the saves of the factory's checkouts past
// slots.SaveKeep: its tasks' copies' (FactoryTaskSaves) and its PRs' fix
// checkouts' (FactoryPRWorkSaves), every folder of them
// (slots.PruneSaves over the layout's FactorySaveFolders), at most once
// per pruneEvery. A folder that saves again prunes its own saves at that
// save too.
func (e *Engine) pruneSaves(now time.Time) {
	if !e.d.Layout.Valid() || !e.savesPruned.IsZero() && now.Sub(e.savesPruned) < pruneEvery {
		return
	}
	e.savesPruned = now
	n, err := slots.PruneSaves(e.d.Layout.FactorySaveFolders(), now)
	if err != nil {
		e.log.Warn("prune the checkouts' saves", "err", err)
	}
	if n > 0 {
		e.log.Info("pruned the checkouts' saves", "saves", n, "keep", slots.SaveKeep.String())
	}
}
