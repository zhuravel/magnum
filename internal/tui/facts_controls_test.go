package tui

import (
	"testing"
	"time"
)

// A drain for a reboot is loud: the titles say it first, before an older
// build or a pause, whole and in its short form.
func TestTheTitlesSayADaemonsDrainFirst(t *testing.T) {
	f := DaemonFacts{Paused: true, SkewOld: "v1", SkewNew: "v2 built",
		Controls: []ControlFact{{Full: "factory draining since 14:02: magnum resume ends it", Short: "factory draining"}}}
	got := f.list(time.Now())
	if len(got) != 3 || got[0].full != "factory draining since 14:02: magnum resume ends it" || got[0].short != "factory draining" {
		t.Fatalf("facts %+v, want the drain first", got)
	}
}
