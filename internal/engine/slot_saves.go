package engine

// The review slots' saves before a discard (slots.Manager.saveChanges):
// the toast of a slot whose save keeps failing, and the prune of old
// saves.

import (
	"context"
	"fmt"
	"time"

	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/slots"
)

// kindSaveFailing counts the slots whose save before a discard fails in a
// batch summary.
var kindSaveFailing = notify.Kind{One: "slot that cannot save", Many: "slots that cannot save"}

// saveFailedWindow: a streak's toast goes out once, its key reserved this
// long (a restart that lost the record of it).
const saveFailedWindow = 7 * 24 * time.Hour

// toastSaveFailed is the toast of a slot whose save before a discard
// fails (slots.Manager.SetSaveAlert, which raises it once per slot and
// kind, never at each retry): it names the slot, what fails, since when
// and that the step retries. Keyed by the streak's start, so the same
// streak raised again toasts nothing.
func (e *Engine) toastSaveFailed(_ context.Context, f slots.SaveFailure) {
	line := f.Slot + " cannot save its changes before a discard"
	if f.Kind == slots.SaveFailedListing {
		line = f.Slot + " cannot list its changes before a discard"
	}
	e.info(notify.Item{
		Key:   fmt.Sprintf("slot-save-failed:%s:%s:%d", f.Slot, f.Kind, f.Since.Unix()),
		Title: "magnum: " + line,
		Body:  f.Sentence() + ". `magnum doctor` names it until it works.",
		Line:  line, Kind: kindSaveFailing, Window: saveFailedWindow,
	})
}
