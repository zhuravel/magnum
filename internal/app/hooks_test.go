package app

import (
	"testing"

	"github.com/zhuravel/magnum/internal/agenthooks"
)

// The daemon's agents manager gives its panes the hook socket and the
// binary only when the daemon command passes the socket of a listener it
// started (Options.HookSocket); without one, no pane gets either, so its
// agents' hooks send nothing and the daemon polls.
func TestTheAppsPanesGetTheHookEnvOnlyWithItsListenersSocket(t *testing.T) {
	const socket = "/state/run/hooks.sock"
	for _, tc := range []struct {
		name, socket string
	}{{"listening", socket}, {"no listener", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			a, layout := testApp(t, Options{HookSocket: tc.socket})
			if a.HookSocket != tc.socket {
				t.Fatalf("App.HookSocket = %q, want %q", a.HookSocket, tc.socket)
			}
			env, err := a.Agents.PaneEnv(nil, a.Config.Roles[0])
			if err != nil {
				t.Fatal(err)
			}
			got, has := env[agenthooks.EnvSocket]
			bin, hasBin := env[agenthooks.EnvBin]
			switch {
			case tc.socket != "" && (got != tc.socket || bin != layout.Binary()):
				t.Fatalf("pane env socket %q, bin %q; want %q and %q", got, bin, tc.socket, layout.Binary())
			case tc.socket == "" && (has || hasBin):
				t.Fatalf("pane env without a listener: %v", env)
			}
		})
	}
}
