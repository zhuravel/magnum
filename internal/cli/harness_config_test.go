package cli

import (
	"testing"

	"github.com/zhuravel/magnum/internal/store/storetest"
)

// The cli tests' harnesses leave Config nil, as Main does, and every
// command they run starts so: the test's config is the preset that
// LoadConfig loads. A command that reads c.Config without LoadConfig then
// fails its tests. The harnesses built their Context with the config
// loaded, which hid finish-review's (fixed in 82a04f3f): from a terminal
// every task read as outside its local review, while its tests passed.
func TestTheHarnessesLeaveTheConfigToTheCommand(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	act := newActHarness(t)
	insp := newInspFixture(t)
	for name, h := range map[string]struct {
		c   *Context
		run func() int
	}{
		"daemon group": {dt.ctx, func() int { return dt.run("version") }},
		"act":          {act.c, func() int { return act.cmd("version") }},
		"inspect":      {insp.Ctx, func() int { return insp.run("version") }},
	} {
		if h.c.Config != nil {
			t.Errorf("%s: the harness loaded the config before any command", name)
		}
		h.c.Config = h.c.preset // as a command before this one left it
		if code := h.run(); code != 0 || h.c.Config != nil {
			t.Errorf("%s: exit %d, config %p after `magnum version`; want the run to start with none loaded", name, code, h.c.Config)
		}
	}
}
