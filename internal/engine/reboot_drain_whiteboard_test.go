package engine

import (
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// A whiteboard asked for checks the drain, then asks Whiteboard whether it
// runs (a CLI call off the tick), then reserves its PR: a drain that began
// during that call holds it too. The job checks the drain again once it
// holds the PR, lets go of it and is refused, with no writer started.
func TestAWhiteboardWhoseDrainBeganDuringItsChecksDoesNotStart(t *testing.T) {
	w := wbHarness(t, false)
	wbWriter(w.harness, wbDone(wbSession))
	pr := w.wbReviewed()
	drained := false
	w.run.Rules = append([]execx.Rule{{Prefix: []string{"whiteboard", "api", "whiteboard_status"}, Fn: func(execx.Cmd) (execx.Result, error) {
		if !drained {
			drained = true
			w.rebootDrain(60, DrainUntilBoot) // the drain begins during the status check
		}
		return execx.Result{Stdout: []byte(`{"ready":true}`)}, nil
	}}}, w.run.Rules...)

	id := w.wbRequest()
	w.wbAnswered(id)
	w.settle()

	w.wbFailed(id, "drains for a reboot")
	if runs := w.wbRuns(pr.ID); len(runs) != 0 {
		t.Fatalf("a writer started under the drain: %+v", runs)
	}
	if w.e.roundActive(pr.ID) {
		t.Fatal("the PR is still reserved after the refusal")
	}
}
