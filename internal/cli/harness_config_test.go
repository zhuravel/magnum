package cli

import (
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// The cli tests' harnesses leave Config nil, as Main does, and every
// command they run starts so: the test's config is the preset that
// LoadConfig loads. A command that reads c.Config without LoadConfig then
// fails its tests. The harnesses built their Context with the config
// loaded, which hid finish-review's (fixed in 82a04f3f): from a terminal
// every task read as outside its local review, while its tests passed.
// Each run starts from a config a command before it left, one no run
// loads, which a run that keeps it still holds after: `magnum version`
// loads nothing, and `magnum comment` loads the harness's preset. The
// check set the preset and looked for none after `magnum version`, which
// held for the inspect fixture whatever its run did, as that fixture has
// no preset, and the comment harness was not checked.
func TestTheHarnessesLeaveTheConfigToTheCommand(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	act := newActHarness(t)
	insp := newInspFixture(t)
	com := newCommentHarness(t, nil)
	for name, h := range map[string]struct {
		c    *Context
		run  func() int
		exit int
	}{
		"daemon group": {dt.ctx, func() int { return dt.run("version") }, 0},
		"act":          {act.c, func() int { return act.cmd("version") }, 0},
		"inspect":      {insp.Ctx, func() int { return insp.run("version") }, 0},
		"comment": {com.c, func() int {
			code, _ := com.exec("Renamed.", "example/other#3") // refused: no watch covers it
			return code
		}, 2},
	} {
		if h.c.Config != nil {
			t.Errorf("%s: the harness loaded the config before any command", name)
		}
		left := config.Defaults() // as a command before this one left it
		h.c.Config = left
		if code := h.run(); code != h.exit || h.c.Config == left {
			t.Errorf("%s: exit %d (want %d), the config a command before it left kept %v; want the run to start with none loaded",
				name, code, h.exit, h.c.Config == left)
		}
	}
}
