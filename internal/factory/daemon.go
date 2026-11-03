package factory

import (
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/paths"
)

// DaemonPID returns the pid of the running factory daemon from its pidfile
// (paths.Layout.FactoryPid), or 0 when there is no pidfile or that process is
// gone. Like the review daemon's it never touches the lock: probing it could
// make a starting daemon exit as "already running".
func DaemonPID(l paths.Layout) (int, error) {
	return DaemonPIDWith(l, engine.SystemProcesses())
}

// DaemonPIDWith is DaemonPID with the processes it asks about given, so a
// test needs no real process.
func DaemonPIDWith(l paths.Layout, procs engine.Processes) (int, error) {
	return procs.PidFromFile(l.FactoryPid())
}

// Kick asks the running factory daemon for a pass now (SIGUSR1); it returns
// the daemon's pid, or 0 when none runs. The pidfile can outlive the daemon
// and its pid be reused, and SIGUSR1 terminates a process that does not
// handle it: the pid is signalled only when ps shows `magnum factory daemon`
// (not the review daemon, `magnum daemon`, whose pidfile is another file).
func Kick(l paths.Layout) (int, error) {
	return KickWith(l, engine.SystemProcesses())
}

// KickWith is Kick with the processes it asks about given, so a test needs no
// real process.
func KickWith(l paths.Layout, procs engine.Processes) (int, error) {
	return procs.Kick(l.FactoryPid(), "magnum factory daemon", LooksLikeFactoryDaemon)
}

// LooksLikeFactoryDaemon reports whether a process is a magnum factory
// daemon from what engine.ProcessCommand read: the executable is magnum and
// its first arguments that are not flags are "factory daemon".
func LooksLikeFactoryDaemon(comm, args string) bool {
	return engine.LooksLikeMagnum(comm, args, "factory", "daemon")
}
