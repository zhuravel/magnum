package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/slots"
)

// A slot whose save before a discard fails gets one toast for its streak
// (the slots package raises it once per slot and kind): it names the slot,
// what fails and that the step retries; the same streak raised again, as a
// restarted daemon could, toasts nothing more.
func TestASlotsFailingSaveToastsOnceAndSaysTheStepRetries(t *testing.T) {
	h := newHarness(t)
	f := slots.SaveFailure{Slot: "review3", Kind: slots.SaveFailedSave, What: "the switch to PR #8's head 1a2b3c4",
		Error: "mkdir /x/saves: not a directory", Since: h.clock.Now()}
	h.e.toastSaveFailed(h.ctx, f)
	if _, err := h.e.batch.Flush(h.ctx); err != nil {
		t.Fatal(err)
	}
	got := toastsWith(h, "review3")
	if len(got) != 1 || !strings.Contains(got[0], "cannot save") || !strings.Contains(got[0], "retries") ||
		!strings.Contains(got[0], "magnum doctor") {
		t.Fatalf("toasts %q, want one that names review3, its save and the retry", h.nh.shown())
	}

	h.advance(time.Minute)
	h.e.toastSaveFailed(h.ctx, f)
	if _, err := h.e.batch.Flush(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := toastsWith(h, "review3"); len(got) != 1 {
		t.Fatalf("toasts %q, want the streak's one alone", h.nh.shown())
	}
}
