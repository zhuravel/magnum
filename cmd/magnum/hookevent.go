package main

import (
	"io"
	"os"
	"time"

	"github.com/zhuravel/magnum/internal/agenthooks"
)

// hookEventCmd is the hidden command the agent hook script runs
// (hooks/magnum-agent-hook.sh): `magnum hook-event <agent> <event>`, the
// hook's payload on stdin.
const hookEventCmd = "hook-event"

// runHookEvent is the binary's hook-event: it runs before cli.Main and
// cobra and reads no config and no registry, so a hook costs the binary's
// start and a datagram. It prints nothing (a Codex hook's plain stdout is
// invalid) and always exits 0, within agenthooks.HookBound: Claude does not
// enforce a timeout on an async hook.
func runHookEvent(args []string) {
	time.AfterFunc(agenthooks.HookBound, func() { os.Exit(0) })
	defer func() {
		_ = recover() // a hook never fails its agent: a panic exits 0 too, silently
		os.Exit(0)
	}()
	hookEvent(args, os.Stdin, os.Getenv)
}

// hookEvent sends the event of hook args (agent, event) to the daemon that
// started the pane (agenthooks.EnvSocket): its payload read from stdin
// (agenthooks.ReadPayload) and parsed with the pane's env
// (agenthooks.Parse). Outside a magnum pane (no socket), with arguments it
// does not know or outside a herdr pane, it returns at once, stdin unread;
// a daemon that is not there fails the send at once.
func hookEvent(args []string, stdin io.Reader, getenv func(string) string) {
	socket := getenv(agenthooks.EnvSocket)
	if socket == "" || len(args) != 2 {
		return
	}
	if _, err := agenthooks.Parse(args[0], args[1], nil, getenv); err != nil {
		return
	}
	ev, err := agenthooks.Parse(args[0], args[1], agenthooks.ReadPayload(stdin), getenv)
	if err != nil {
		return
	}
	_ = agenthooks.Send(socket, ev) // a hint: the daemon polls without it
}
