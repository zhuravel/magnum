package factory

// The one place the engine records an input of the operator's.

import (
	"context"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/factory/stats"
)

// longPaste is why your words were not kept: a long pasted block (a log, a
// stack trace, a file), which runtime.Scrub does not keep.
const longPaste = "a long paste: more than 40 lines or 4000 bytes"

// withWords is in with your words text, scrubbed (runtime.Scrub), or, for a
// long paste, only why they were dropped: the one rule for every input of
// your words (a start, an answer, a send-back's note, a pane's turn).
func withWords(in registry.Input, text string) registry.Input {
	if clean, kept := runtime.Scrub(text); kept {
		in.Text = clean
	} else {
		in.DroppedReason = longPaste
	}
	return in
}

// recordInput records in as an input of the operator's and returns its id.
// Every input the engine records goes through here, so the words you typed
// (stats.HasWords: the task's text, guidance, a send-back's note, a pane's
// turn, one line said, an answer) are classified once, the same way, wherever
// they were typed: the class (stats.Classify) is stored with the input in one
// write, which is what registry.ClassifyInput would set at once. A class the
// caller already knows stays. A decision's text is the machine's choice, a
// retry has none and a dropped paste has none left, so they get no class. The
// turns typed into a pane (typed.go) go through here as well.
func (e *Engine) recordInput(ctx context.Context, in registry.Input) (int64, error) {
	if in.Class == "" && in.Text != "" && stats.HasWords(in.Kind) {
		in.Class = stats.Classify(in.Text)
	}
	return e.reg.RecordInput(ctx, in)
}
