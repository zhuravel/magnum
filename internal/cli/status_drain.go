package cli

import (
	"fmt"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
)

// statusDrainPause is the pause a drain holds every round with: who drains
// (the command's pid) and how to lift it. A drainer that is gone no longer
// holds anything: the daemon lifts its drain on the next tick.
func statusDrainPause(d statusDeps, dm statusDaemon, now, since time.Time) statusPause {
	p := statusPause{Scope: "daemon", Reason: "draining for a restart since " + inspClock(now, since),
		Detail: "no new round starts; the restart follows once none is in flight"}
	pid := dm.DrainerPID
	if pid <= 0 {
		p.Fix = "wait for `magnum daemon-restart --drain` (ctrl+c there lifts the drain), or `magnum daemon-restart --now` to restart at once (the new daemon lifts it)"
		return p
	}
	alive := engine.DrainerAlive
	if d.DrainerAlive != nil {
		alive = d.DrainerAlive
	}
	p.Reason += fmt.Sprintf(" (drained by pid %d)", pid)
	if !alive(pid) {
		p.Detail = fmt.Sprintf("the command that drains (pid %d) is gone: the daemon lifts the drain on its next tick", pid)
		p.Fix = "`magnum kick` lifts it now"
		return p
	}
	p.Fix = fmt.Sprintf("wait for it, or lift the drain without a restart: ctrl+c in its terminal, or `kill %d`", pid)
	return p
}
