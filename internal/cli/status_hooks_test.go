package cli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
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

// `magnum status` prints how often the agents' own hooks' state agreed with
// herdr's status at the observe passes of the last 24 hours, per kind, the
// measure that decides whether the hooks can stand in for herdr; nothing
// while no pass compared them, and an hour older than the window does not
// count.
func TestStatusShowsHowOftenTheHookStateAgreedWithHerdr(t *testing.T) {
	_, st, d, now := statusFixture(t)
	ctx := context.Background()
	shadowLine := func() string {
		t.Helper()
		r, err := statusGather(ctx, d, statusOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		statusRender(&b, r)
		for line := range strings.SplitSeq(b.String(), "\n") {
			if strings.HasPrefix(line, "shadow:") {
				return line
			}
		}
		return ""
	}
	if line := shadowLine(); line != "" {
		t.Fatalf("no readings: %q", line)
	}
	hour := now.UTC().Truncate(time.Hour)
	tally := fmt.Sprintf(`[{"hour":%q,"kind":"claude","agreed":90,"differed":10},{"hour":%q,"kind":"claude","agreed":97,"differed":3},`+
		`{"hour":%q,"kind":"codex","agreed":1,"differed":2},{"hour":%q,"kind":"codex","agreed":50,"differed":0}]`,
		store.FormatTime(hour.Add(-2*time.Hour)), store.FormatTime(hour), store.FormatTime(hour), store.FormatTime(hour.Add(-24*time.Hour)))
	if err := st.SetKV(ctx, store.KVHookAgreement, tally); err != nil {
		t.Fatal(err)
	}
	const want = "shadow:   the hooks' state agreed with herdr's status at the observe passes of the last 24 h: claude 93% of 200 readings, codex 33% of 3 readings"
	if line := shadowLine(); line != want {
		t.Fatalf("line = %q\nwant   %q", line, want)
	}
}
