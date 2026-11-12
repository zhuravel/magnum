package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/factory"
)

// The factory's status line agrees with its daemon, which holds a pause for
// a pause.json it cannot read or parse, at its start and during its run:
// "paused (unreadable record)" with the remedy that works, whether the
// daemon runs or not, for both kinds of file; a pause it can read says who
// paused it.
func TestTheFactorysStatusSaysPausedForAPauseFileItCannotUse(t *testing.T) {
	const want = "paused (unreadable record): magnum resume --factory removes it"
	for _, tc := range []struct {
		name    string
		setup   func(h *actHarness)
		running bool
	}{
		{name: "unreadable, not running", setup: func(h *actHarness) {
			if err := os.MkdirAll(h.c.Layout.FactoryPause(), 0o700); err != nil {
				h.t.Fatal(err)
			}
		}},
		{name: "unparseable, not running", setup: func(h *actHarness) {
			if err := factory.WritePause(h.c.Layout, "{not a pause"); err != nil {
				h.t.Fatal(err)
			}
		}},
		{name: "unreadable, running", running: true, setup: func(h *actHarness) {
			if err := os.MkdirAll(h.c.Layout.FactoryPause(), 0o700); err != nil {
				h.t.Fatal(err)
			}
		}},
		{name: "unparseable, running", running: true, setup: func(h *actHarness) {
			if err := factory.WritePause(h.c.Layout, "{not a pause"); err != nil {
				h.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, f := newControlHarness(t)
			pid := 0
			if tc.running {
				h.runDaemons(f)
				pid = f.pid
			}
			tc.setup(h)
			if got := factoryControlOf(h.c.Layout, pid).holdText(h.now, true); !strings.Contains(got, want) {
				t.Fatalf("the factory's status says %q, want %q", got, want)
			}
		})
	}
	h, _ := newControlHarness(t)
	if err := factory.WritePause(h.c.Layout, factory.Pause{Generation: 1, Since: h.now, Reason: "freeze"}.Value()); err != nil {
		t.Fatal(err)
	}
	if got := factoryControlOf(h.c.Layout, 0).holdText(h.now, true); !strings.Contains(got, "paused by you (freeze): magnum resume") {
		t.Fatalf("the factory's status says %q for a pause it can read", got)
	}
}
