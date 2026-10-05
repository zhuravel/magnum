package cli

// Helpers shared by every command group (the act, inspect and daemon groups
// keep their own prefixed helpers in helpers_act.go, helpers_inspect.go and
// helpers_daemon.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/zhuravel/magnum/internal/store"
)

// pluginID is magnum's herdr plugin id (herdr-plugin.toml).
const pluginID = "zhuravel.magnum"

// prInFlight are the PR states of a running review round (the daemon refuses
// a second one, and stopping it abandons them): store.InFlightStates.
var prInFlight = store.InFlightStates

// cmdFail prints "magnum <cmd>: <err>" to stderr and returns exit code 1.
func cmdFail(c *Context, cmd string, err error) int {
	fmt.Fprintf(c.Stderr, "magnum %s: %v\n", cmd, err)
	return 1
}

// signalContext ends on ctrl+c, SIGTERM or SIGHUP (the terminal closed).
// Blocking reads must watch it (promptIn): the handler keeps the signal from
// killing the process, so a command can undo what it holds first (a drain
// lifts itself instead of holding every round).
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
}

// writeJSON writes v as indented JSON and a newline.
func writeJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// sha7 shortens a commit sha.
func sha7(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

// trunc cuts s to n runes with an ellipsis.
func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// slotOfPR returns the live (not removed) slot holding prID, or nil.
func slotOfPR(ctx context.Context, st *store.Store, prID int64) (*store.Slot, error) {
	slots, err := st.ListSlots(ctx, store.SlotFilter{})
	if err != nil {
		return nil, err
	}
	for i := range slots {
		if slots[i].PRID != nil && *slots[i].PRID == prID && slots[i].State != store.SlotRemoved {
			return &slots[i], nil
		}
	}
	return nil, nil
}
