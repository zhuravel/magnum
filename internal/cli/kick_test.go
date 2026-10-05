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
	h.errb.Reset()
	if code := h.cmd("kick"); code != 0 || h.errb.Len() != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "kicked the daemon (pid 4242): a tick runs now")
	h.errb.Reset()
	if code := h.cmd("kick", "everything"); code != 2 || !strings.Contains(h.errb.String(), "kick takes no arguments") {
		t.Fatalf("bad target: exit %d: %s", code, h.errb.String())
	}
}

// Every tick polls, schedules and reconciles, so the target a kick took was
// only echoed back: it is accepted (an older herdr plugin still runs `kick
// reconcile` at startup), ignored, and says so.
func TestKickTargetIsIgnoredWithANote(t *testing.T) {
	h := newActHarness(t)
	old := actKickDaemon
	actKickDaemon = func(paths.Layout) (int, error) { return 4242, nil }
	t.Cleanup(func() { actKickDaemon = old })
	for _, target := range []string{"poll", "reconcile", "schedule"} {
		h.out.Reset()
		h.errb.Reset()
		if code := h.cmd("kick", target); code != 0 {
			t.Fatalf("kick %s: exit %d: %s", target, code, h.errb.String())
		}
		if got := h.out.String(); got != "kicked the daemon (pid 4242): a tick runs now\n" {
			t.Errorf("kick %s printed %q", target, got)
		}
		actContains(t, h.errb.String(), `"`+target+`" is ignored and will go away`)
	}
	if strings.Contains(kickUsage, "reconcile") {
		t.Errorf("usage still advertises a target: %s", kickUsage)
	}
}
