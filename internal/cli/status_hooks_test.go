package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/engine"
)

// While the daemon runs, `magnum status` says whether its hook listener is
// up: the socket it listens on, or why it polls alone; without a daemon, or
// before a daemon recorded it, nothing.
func TestStatusSaysWhetherTheDaemonsHookListenerIsUp(t *testing.T) {
	_, st, d, _ := statusFixture(t)
	ctx := context.Background()
	hooksLine := func() (statusReport, string) {
		t.Helper()
		r, err := statusGather(ctx, d, statusOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		statusRender(&b, r)
		for line := range strings.SplitSeq(b.String(), "\n") {
			if strings.HasPrefix(line, "hooks:") {
				return r, line
			}
		}
		return r, ""
	}
	d.DaemonPID = func() (int, error) { return 4242, nil }
	if r, line := hooksLine(); r.Daemon.Hooks != nil || line != "" {
		t.Fatalf("no record: %+v, %q", r.Daemon.Hooks, line)
	}
	if err := st.SetKV(ctx, engine.KVDaemonHooks, `{"socket":"/state/run/hooks.sock"}`); err != nil {
		t.Fatal(err)
	}
	r, line := hooksLine()
	if r.Daemon.Hooks == nil || r.Daemon.Hooks.Socket != "/state/run/hooks.sock" ||
		line != "hooks:    listening on /state/run/hooks.sock: the agents' hooks wake their panes" {
		t.Fatalf("listening: %+v, %q", r.Daemon.Hooks, line)
	}
	if err := st.SetKV(ctx, engine.KVDaemonHooks, `{"off":"paths: socket path /deep/hooks.sock is 120 bytes; a Unix socket's must be under 104"}`); err != nil {
		t.Fatal(err)
	}
	if _, line := hooksLine(); line != "hooks:    off, the daemon polls its agents alone: paths: socket path /deep/hooks.sock is 120 bytes; a Unix socket's must be under 104" {
		t.Fatalf("off: %q", line)
	}
	d.DaemonPID = func() (int, error) { return 0, nil }
	if _, line := hooksLine(); line != "" {
		t.Fatalf("no daemon: %q", line)
	}
}
