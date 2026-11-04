package factory

// The one place the engine records an input of the operator's.

import (
	"context"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/stats"
)

// recordInput records in as an input of the operator's and returns its id.
// Every input the engine records goes through here, so the words you typed
// (stats.HasWords: the task's text, guidance, a send-back's note, a pane's
// turn, one line said, an answer) are classified once, the same way, wherever
// they were typed: the class (stats.Classify) is stored with the input in one
// write, which is what registry.ClassifyInput would set at once. A class the
// caller already knows stays. A decision's text is the machine's choice, a
// retry has none and a dropped paste has none left, so they get no class. The
// turns typed into a pane, once the engine records them, go through here as
// well.
func (e *Engine) recordInput(ctx context.Context, in registry.Input) (int64, error) {
	if in.Class == "" && in.Text != "" && stats.HasWords(in.Kind) {
		in.Class = stats.Classify(in.Text)
	}
	return e.reg.RecordInput(ctx, in)
}
