package cli

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/paths"
)

// A dry run of the daemon claims nothing: it never takes the daemon lock,
// nor the hook socket, whose stale-socket removal would cut a live daemon
// off its agents' hooks. A real run claims both.
func TestADryRunDaemonClaimsNeitherTheLockNorTheHookSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "mg") //nolint:usetesting // t.TempDir's path is too long for a Unix socket's
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l := paths.Layout{Home: dir}
	if err := os.MkdirAll(filepath.Dir(l.HookSocket()), 0o700); err != nil {
		t.Fatal(err)
	}
	live, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: l.HookSocket(), Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()

	c, err := claimDaemon(l, true)
	if err != nil || c != nil {
		t.Fatalf("dry run: claim %v, %v; want none", c, err)
	}
	if err := agenthooks.Send(l.HookSocket(), agenthooks.Event{Agent: agenthooks.AgentClaude, Name: agenthooks.NameStop, Pane: "w1:p1"}); err != nil {
		t.Fatalf("the live daemon's socket after a dry run: %v", err)
	}

	c, err = claimDaemon(l, false)
	if err != nil || c == nil {
		t.Fatalf("real run: claim %v, %v", c, err)
	}
	defer c.Release()
	if c.Held() || c.HookSocket() != l.HookSocket() {
		t.Fatalf("real run: held %v, socket %q; want the lock and %s", c.Held(), c.HookSocket(), l.HookSocket())
	}
}
