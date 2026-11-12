package factory

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// The put-back of a control file (a command's undo) races neither a writer
// nor the daemon's read: `magnum pause` writing a newer pause meanwhile
// keeps it, and the daemon's read never finds the file absent, which would
// lift the hold for that tick.
func TestThePutBackOfAControlRacesNoWriterAndNeverLeavesTheFileAbsent(t *testing.T) {
	h := newHarness(t)
	l := h.layout
	if err := WritePause(l, "first"); err != nil { // the file exists from now on
		t.Fatal(err)
	}
	var absent atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if v, err := ReadPause(l); err == nil && v == "" {
				absent.Add(1)
			}
		}
	})
	defer func() {
		close(stop)
		wg.Wait()
	}()
	for i := range 300 {
		if err := WritePause(l, "mine"); err != nil {
			t.Fatal(err)
		}
		other := fmt.Sprintf("other-%d", i)
		done := make(chan error, 1)
		go func() { done <- WritePause(l, other) }()
		if err := ReplacePause(l, "mine", "before"); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if v, _ := ReadPause(l); v != other {
			t.Fatalf("pause.json holds %q after a put-back raced the write of %q: the write was lost", v, other)
		}
	}
	if n := absent.Load(); n != 0 {
		t.Fatalf("the daemon's read found pause.json absent %d times during the put-backs", n)
	}
}

// heldPauseWords is the pause the daemon holds now, as holds.json says it
// ("" for none).
func (h *harness) heldPauseWords() string {
	h.t.Helper()
	if p := h.e.ctl.pause.Load(); p != nil {
		return p.Words()
	}
	return ""
}

// pauseWarnings are the warn events the daemon said about pause.json.
func (h *harness) pauseWarnings() []string {
	h.t.Helper()
	var said []string
	for _, ev := range h.events(0, "pause") {
		if ev.Phase == registry.PhaseFail {
			said = append(said, ev.Text)
		}
	}
	return said
}

// A pause.json the daemon cannot read or parse holds a pause, as a
// reboot-drain.json does: at its start, during its run (one it held stays,
// as it was), and for content `magnum pause` never writes. The warning says
// the pause holds, and `magnum resume --factory` removes the file.
func TestAPauseFileTheDaemonCannotUseHoldsAPause(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *harness)
		words string
	}{
		{name: "unreadable at the start", setup: func(h *harness) {
			if err := os.MkdirAll(h.layout.FactoryPause(), 0o700); err != nil { // EISDIR
				h.t.Fatal(err)
			}
		}, words: "paused (unreadable record)"},
		{name: "unparseable at the start", setup: func(h *harness) {
			if err := WritePause(h.layout, "{not a pause"); err != nil {
				h.t.Fatal(err)
			}
		}, words: "paused (unreadable record)"},
		{name: "turns unreadable during a run", setup: func(h *harness) {
			h.pause(time.Time{}, "freeze")
			h.tickAndWait()
			if err := os.Remove(h.layout.FactoryPause()); err != nil {
				h.t.Fatal(err)
			}
			if err := os.MkdirAll(h.layout.FactoryPause(), 0o700); err != nil {
				h.t.Fatal(err)
			}
		}, words: "paused by you (freeze)"},
		{name: "turns unparseable during a run", setup: func(h *harness) {
			h.pause(time.Time{}, "freeze")
			h.tickAndWait()
			if err := WritePause(h.layout, "{not a pause"); err != nil {
				h.t.Fatal(err)
			}
		}, words: "paused by you (freeze)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.stage(config.FactoryStageAgents, idleStage)
			tc.setup(h)
			h.tickAndWait()
			h.tickAndWait()
			if !h.e.startsHeld() {
				t.Fatal("no pause holds: new work starts")
			}
			if got := h.heldPauseWords(); !strings.HasPrefix(got, tc.words) {
				t.Errorf("the pause held is %q, want %q", got, tc.words)
			}
			said := h.pauseWarnings()
			if len(said) != 1 || !strings.Contains(said[0], "the pause holds until `magnum resume --factory` removes it") ||
				strings.Contains(said[0], "no pause holds") {
				t.Errorf("pause warnings %q, want one that says the pause holds", said)
			}
			if evs := h.events(0, "pause"); len(evs) > 0 && strings.Contains(evs[len(evs)-1].Text, "lifted") {
				t.Errorf("the last pause event says %q: the pause was lifted", evs[len(evs)-1].Text)
			}
		})
	}
}
