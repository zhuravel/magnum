package cli

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/paths"
)

func TestKick(t *testing.T) {
	h := newActHarness(t)
	pid := 0
	var gotLayout paths.Layout
	old := actKickDaemon
	actKickDaemon = func(l paths.Layout) (int, error) { gotLayout = l; return pid, nil }
	t.Cleanup(func() { actKickDaemon = old })

	if code := h.cmd("kick"); code != 1 || !strings.Contains(h.errb.String(), "no daemon is running: start it with `magnum daemon`") {
		t.Fatalf("no daemon: exit %d: %s", code, h.errb.String())
	}
	if gotLayout.Home != h.home {
		t.Errorf("kicked layout %q", gotLayout.Home)
	}
	pid = 4242
	if code := h.cmd("kick", "reconcile"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "kicked the daemon (pid 4242): a tick runs now (it reconciles when reconcile_interval has passed)")
	h.errb.Reset()
	if code := h.cmd("kick", "everything"); code != 2 || !strings.Contains(h.errb.String(), `unknown target "everything"`) {
		t.Fatalf("bad target: exit %d: %s", code, h.errb.String())
	}
}
