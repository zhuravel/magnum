package cli

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/paths"
)

// shortHome is a layout whose hook socket path fits a Unix socket's:
// t.TempDir's can pass the 104 bytes on macOS.
func shortHome(t *testing.T) paths.Layout {
	t.Helper()
	dir, err := os.MkdirTemp("", "mg") //nolint:usetesting // t.TempDir's path is too long for a Unix socket's
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return paths.Layout{Home: dir}
}

// A dry run of the daemon claims nothing: it never takes the daemon lock,
// nor the hook socket, whose stale-socket removal would cut a live daemon
// off its agents' hooks. A real run claims both.
func TestADryRunDaemonClaimsNeitherTheLockNorTheHookSocket(t *testing.T) {
	l := shortHome(t)
	if err := os.MkdirAll(filepath.Dir(l.HookSocket()), 0o700); err != nil {
		t.Fatal(err)
	}
	live, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: l.HookSocket(), Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	c := &Context{Layout: l, Stderr: io.Discard}

	claim, eo, ao, err := claimDaemon(c, daemonOptions{DryRun: true}, engine.Options{})
	if err != nil || claim != nil {
		t.Fatalf("dry run: claim %v, %v; want none", claim, err)
	}
	if eo.Claim != nil || ao.HookSocket != "" {
		t.Fatalf("dry run: Run's claim %v, the panes' socket %q; want neither", eo.Claim, ao.HookSocket)
	}
	if err := agenthooks.Send(l.HookSocket(), agenthooks.Event{Agent: agenthooks.AgentClaude, Name: agenthooks.NameStop, Pane: "w1:p1"}); err != nil {
		t.Fatalf("the live daemon's socket after a dry run: %v", err)
	}

	claim, _, _, err = claimDaemon(c, daemonOptions{}, engine.Options{})
	if err != nil || claim == nil {
		t.Fatalf("real run: claim %v, %v", claim, err)
	}
	defer claim.Release()
	if claim.Held() || claim.HookSocket() != l.HookSocket() {
		t.Fatalf("real run: held %v, socket %q; want the lock and %s", claim.Held(), claim.HookSocket(), l.HookSocket())
	}
}

// The daemon's claim goes to Run, which holds its lock instead of taking it
// a second time (engine.Options.Claim: a second take in the same process
// finds the lock held, and Run would exit as a second daemon with nothing
// done), and its listener's socket goes to the App, whose panes get it
// (app.Options.HookSocket). A claim without a listener (a path too long)
// still goes to Run, and the panes get no socket. The options the flags
// made are kept.
func TestTheDaemonsClaimGoesToRunAndItsSocketToThePanes(t *testing.T) {
	l := shortHome(t)
	c := &Context{Layout: l, Stderr: io.Discard}
	claim, eo, ao, err := claimDaemon(c, daemonOptions{Once: true}, engine.Options{Once: true, Supervised: true})
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Release()
	if eo.Claim != claim || !eo.Once || !eo.Supervised {
		t.Fatalf("Run's options %+v, want the claim %p with the flags' options", eo, claim)
	}
	if ao.HookSocket == "" || ao.HookSocket != claim.HookSocket() || !ao.Daemon {
		t.Fatalf("the App's options: socket %q, daemon %v; want the listener's %s", ao.HookSocket, ao.Daemon, claim.HookSocket())
	}

	long := paths.Layout{Home: filepath.Join(t.TempDir(), strings.Repeat("x", paths.SocketPathLimit))}
	off, eo, ao, err := claimDaemon(&Context{Layout: long, Stderr: io.Discard}, daemonOptions{}, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer off.Release()
	if off.Held() || off.Off() == "" || eo.Claim != off || ao.HookSocket != "" {
		t.Fatalf("a path too long: held %v, off %q, Run's claim %p (want %p), the panes' socket %q", off.Held(), off.Off(), eo.Claim, off, ao.HookSocket)
	}
}
