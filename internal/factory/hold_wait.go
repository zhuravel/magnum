package factory

// A hold that stops every stage as the wait of a task. The tick lets no stage
// advance while herdr does not answer or an infrastructure hold (SSH, DNS,
// TLS, a MySQL ping, the same failure on two tasks) is between its tries, so
// a task whose stage had no wait to name (a local review between rounds, a
// stage that is about to start) had none to say: `magnum factory status`
// printed "-" under WAITS ON for it and the screen drew the step it would
// run, while the daemon held every stage and printed the hold above the
// table. The hold is that task's wait now, in one place for the status
// command and the screen: "held: MySQL does not answer (since 17:03)". A task
// with a wait of its own keeps it, which is more specific; the holds that
// stop launches (disk, memory) or one repository's copies, and the pause and
// the drains, name themselves in the wait of the launch they stop (heldError),
// since the tick still runs its stages.

import (
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
)

// waitHeld is the who of a wait that is a hold of the daemon's.
const waitHeld = "held"

// StopsEveryStage reports whether hold h stops the next step of every task:
// herdr's hold and the machine's infrastructure hold, which the tick answers
// by advancing no task (Tick's advance). A repository's infrastructure hold
// (Repo set) holds its copies alone.
func (h Hold) StopsEveryStage() bool {
	return h.Repo == "" && (h.Kind == HoldInfra || h.Kind == HoldHerdr)
}

// StoppingHold is the hold in hs that stops the next step of task t: t is a
// task the loop advances (active, or waiting) that names no wait of its own.
// false for a task with one, and for a task the loop leaves alone (it needs
// you, it is published, it ended).
func StoppingHold(hs []Hold, t registry.Task) (Hold, bool) {
	if !isLive(t) || t.WaitsWho != "" || t.WaitsWhat != "" {
		return Hold{}, false
	}
	for _, h := range hs {
		if h.StopsEveryStage() {
			return h, true
		}
	}
	return Hold{}, false
}

// HoldWait is the wait a hold gives the task it stops, at now: its who
// ("held") and its what, the hold's reason and since when ("MySQL does not
// answer (since 17:03)"; a hold of another day says the day too).
func HoldWait(h Hold, now time.Time) (who, what string) {
	return waitHeld, h.Reason + " (since " + holdClock(now, h.Since) + ")"
}

// holdClock is when a hold began as a person reads it: the time of day, with
// the date before it unless it is today's.
func holdClock(now, t time.Time) string {
	if sameDay(now, t) {
		return clock(t)
	}
	return t.Local().Format("Jan 2 15:04")
}
