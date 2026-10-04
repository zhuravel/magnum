package cli

import (
	"fmt"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/paths"
)

// opsHolder is who decides where in-process slot work runs.
type opsHolder int

const (
	opsMine   opsHolder = iota // both locks are ours: run the work here, then unlock
	opsDaemon                  // the daemon holds its lock: hand the work to it
	opsBusy                    // another magnum command runs in-process slot work
)

// acquireOps takes ops.lock (layout.OpsLock()), then the daemon's lock, for
// in-process slot, cleanup or release work. unlock is non-nil only for
// opsMine. A held daemon lock is the liveness signal for "the daemon runs"
// (the pidfile can outlive a crashed daemon). The daemon holds its lock for
// its whole life; holding ops.lock first lets a daemon that starts meanwhile
// tell a command's in-process job (ops.lock held) from another daemon
// (ops.lock free) and exit non-zero, for launchd to start it again once the
// job is done (engine.Run).
func acquireOps(l paths.Layout) (unlock func(), who opsHolder, err error) {
	unlockOps, held, err := engine.AcquireLock(l.OpsLock())
	if err != nil {
		return nil, opsBusy, fmt.Errorf("lock %s: %w", inspTilde(l.OpsLock()), err)
	}
	if held {
		return nil, opsBusy, nil
	}
	unlockDaemon, held, err := engine.AcquireLock(l.Lock())
	if err != nil {
		unlockOps()
		return nil, opsBusy, fmt.Errorf("lock %s: %w", inspTilde(l.Lock()), err)
	}
	if held {
		unlockOps()
		return nil, opsDaemon, nil
	}
	return func() {
		unlockDaemon()
		unlockOps()
	}, opsMine, nil
}

// opsBusyErr is the refusal when another command holds ops.lock.
func opsBusyErr(l paths.Layout) error {
	return fmt.Errorf("another magnum command is running slot work in-process (it holds %s); retry when it finishes",
		inspTilde(l.OpsLock()))
}
