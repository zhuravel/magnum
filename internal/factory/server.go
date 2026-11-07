package factory

// The copies' local servers (spec 7.1): the factory starts a copy's server
// itself, only for the stages that need it (the engine's startServer holds a
// heavy slot for it), and stops it after. It records the server's pid and
// its start (the process table's lstart) in the agents store, so after a
// restart it tells the server from a later process that got its pid:
// Reconcile kills a server only while both match its record, never by its
// pid alone. The server writes its own pid to a pidfile at its start, so a
// server the daemon spawned just before it crashed, before its record, is
// found too: by that pid and a start the pidfile's time matches. A stop
// signals the server's process group once its leader is the server, and
// waits for every member of the group, not the leader only.
//
// A stop proves the copy's port free (proveStopped), and kills only what the
// factory started: the server's group, and a listener on the copy's port
// whose working directory is in the copy (overmind's tmux panes leave the
// group). A process outside the copy is never signalled, whatever port it
// holds: one on the copy's port fails the stop (portHeld); neither is a
// helper of the factory's agents in the copy on another port. A copy whose
// server the factory never started has nothing stopped or proven, and a
// start that failed leaves no pidfile that a later stop would read so. A
// stop runs on its own bound (stopContext), whatever its caller's context.
// A server starts with BROWSER=true and the factory's `open` first on its
// PATH, so it opens no tab in your browser.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
)

// serverRecord is a copy's server as StartServer recorded it (serverKey):
// its pid, its start as the process table shows it (the two are its
// lifetime identity, execx.Process.Same), its checkout and since when it
// runs. The server leads its process group (spawnServer), so a stop
// signals the group its pid names.
type serverRecord struct {
	PID   int       `json:"pid"`
	Start string    `json:"start"`
	Path  string    `json:"path"`
	Since time.Time `json:"since"`
}

// serverKeyPrefix starts the kv key of every recorded server.
const serverKeyPrefix = "factory.server."

// serverKey is the kv key of copy name's server.
func serverKey(name string) string { return serverKeyPrefix + name }

// A stop sends the server's group SIGTERM, looks every serverPoll whether
// it ended, and sends SIGKILL after serverStopGrace. The repository's
// server_stop runs at most serverStopCap, and one lsof at most lsofTimeout.
const (
	serverStopGrace = 10 * time.Second
	serverPoll      = 250 * time.Millisecond
	serverStopCap   = 30 * time.Second
	lsofTimeout     = 15 * time.Second
	// stopBound bounds one stop of a server or a browser: its server_stop,
	// two graces and two lsof runs, with room.
	stopBound = 2 * time.Minute
)

// stopContext is the context a stop runs on: ctx's values without its
// cancellation or its deadline (QA's cap and the daemon's shutdown end the
// caller's context, and the stop must still run), bounded by stopBound.
func stopContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), stopBound)
}

// The kinds of the begin, ok and fail events of the servers' and the
// browsers' steps, in the agents store (copySet.phase).
const (
	kindServerStart       = "factory.server_start"
	kindServerStop        = "factory.server_stop"
	kindServerStopCommand = "factory.server_stop_command"
	kindServerStray       = "factory.server_stray"
	kindBrowserStart      = "factory.browser_start"
	kindBrowserStop       = "factory.browser_stop"
)

// serverLog is the log a copy's server writes to, in the logs directory.
func (c *copySet) serverLog(name string) string {
	return filepath.Join(c.cfg.Layout.Logs(), "factory-server-"+name+".log")
}

// serverPidFile is the file a copy's server writes its pid to at its start
// (spawnServer), beside its log.
func (c *copySet) serverPidFile(name string) string {
	return filepath.Join(c.cfg.Layout.Logs(), serverFilePrefix+name+serverPidSuffix)
}

// The names of the servers' pidfiles: factory-server-<copy>.pid.
const (
	serverFilePrefix = "factory-server-"
	serverPidSuffix  = ".pid"
)

// pidStartSlack is how much later than its process's start (lstart, cut to
// the second) a server's pidfile may be written: its shell writes it first
// thing.
const pidStartSlack = 10 * time.Second

// StartServer starts the repository's server in copy cp with the copy's
// env (rendered from the configuration again: a copy named by a task after a
// restart carries no env), unless the server it recorded still runs, and
// records its pid and its start. A server that is gone at once is an error
// naming its log.
func (c *copySet) StartServer(ctx context.Context, cp Copy) error {
	repo, n, ok := c.repoOf(cp.Name)
	if !ok {
		return fmt.Errorf("start the server of %s: no [[factory.repo]] has that copy", cp.Name)
	}
	cp.Env = copyEnv(repo, n)
	if cp.Path == "" {
		cp.Path = repo.Pool().Path(n)
	}
	if strings.TrimSpace(repo.Server) == "" {
		return fmt.Errorf("start the server of %s: %s has no server ([[factory.repo]] server)", cp.Name, repo.Repo)
	}
	c.srv.Lock()
	defer c.srv.Unlock()
	if rec, ok, err := c.serverOf(ctx, cp.Name); err != nil {
		return err
	} else if ok {
		if _, alive, err := c.running(ctx, rec); err != nil || alive {
			return err
		}
		// Its leader is gone, but what it left in the copy may still hold the
		// port (overmind's tmux panes): the whole stop runs first.
		sctx, cancel := stopContext(ctx)
		_, _, _, err := c.stopServer(sctx, cp.Name, "its leader is gone, and a new server starts")
		cancel()
		if err != nil {
			return fmt.Errorf("start the server of %s: %w", cp.Name, err)
		}
	}
	if err := ensureOpenShim(c.cfg.Layout.FactoryShims()); err != nil {
		return fmt.Errorf("start the server of %s: %w", cp.Name, err)
	}
	if err := os.MkdirAll(c.cfg.Layout.Logs(), 0o700); err != nil {
		return fmt.Errorf("start the server of %s: %w", cp.Name, err)
	}
	if err := os.Remove(c.serverPidFile(cp.Name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("start the server of %s: %w", cp.Name, err)
	}
	log := c.serverLog(cp.Name)
	env := maps.Clone(cp.Env)
	if env == nil {
		env = map[string]string{}
	}
	env["BROWSER"] = "true" // what opens a page by $BROWSER runs true (serverEnv)
	c.phase(ctx, cp.Name, kindServerStart, store.PhaseBegin, fmt.Sprintf("start the server of %s: %s", cp.Name, repo.Server))
	// fail records a start that failed after the spawn: the group it
	// spawned is ended (rec, the pid alone when the table could not be
	// read), and its pidfile goes, so no later stop takes it for a server
	// the factory started.
	fail := func(err error, rec serverRecord) error {
		if rec.PID > 0 {
			sctx, cancel := stopContext(ctx)
			if serr := c.end(sctx, rec); serr != nil {
				err = errors.Join(err, fmt.Errorf("and its stop failed: %w", serr))
			}
			cancel()
		}
		if rerr := os.Remove(c.serverPidFile(cp.Name)); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			err = errors.Join(err, rerr)
		}
		c.phase(ctx, cp.Name, kindServerStart, store.PhaseFail, err.Error())
		return err
	}
	pid, err := c.spawn(ctx, cp.Path, env, repo.Server, log, c.serverPidFile(cp.Name))
	if err != nil {
		return fail(fmt.Errorf("start the server of %s: %w", cp.Name, err), serverRecord{})
	}
	table, err := c.processes(ctx)
	if err != nil {
		return fail(fmt.Errorf("start the server of %s: %w", cp.Name, err), serverRecord{PID: pid})
	}
	i := slices.IndexFunc(table, func(p execx.Process) bool { return p.PID == pid && !p.Zombie && p.Start != "" })
	if i < 0 {
		return fail(fmt.Errorf("the server of %s (pid %d) ended at once; its log is %s", cp.Name, pid, log), serverRecord{})
	}
	rec := serverRecord{PID: pid, Start: table[i].Start, Path: cp.Path, Since: c.now()}
	if err := c.setRecord(ctx, serverKey(cp.Name), rec); err != nil {
		return fail(fmt.Errorf("record the server of %s: %w", cp.Name, err), rec)
	}
	c.phase(ctx, cp.Name, kindServerStart, store.PhaseOK, fmt.Sprintf("the server of %s runs as pid %d; its log is %s", cp.Name, pid, log))
	return nil
}

// StopServer stops copy cp's server, if the factory started one, and
// forgets it (stopServer), on its own bound (stopContext). A stop that
// succeeded while a process outside the copy holds the copy's port returns
// that portHeld alone: the server is stopped and forgotten, the holder left
// alone.
func (c *copySet) StopServer(ctx context.Context, cp Copy) error {
	ctx, cancel := stopContext(ctx)
	defer cancel()
	c.srv.Lock()
	defer c.srv.Unlock()
	_, _, held, err := c.stopServer(ctx, cp.Name, "stopped")
	if err != nil {
		return err
	}
	return held
}

// Reconcile ends the servers a stopped daemon left. After a restart no stage
// owns a server (the engine runs it before its first tick), so each one is
// a stray: a recorded one is killed only while the process at its pid has
// the start its record names, never by its pid alone (that pid may be
// another process's now), and every record is forgotten; one the daemon
// spawned just before it crashed, which no record names, is found by its
// pidfile (strayServers). A process outside a copy that holds its port is
// no failure of the reconcile: its stop event says so. Every recorded
// browser is a stray too, ended the same way (reconcileBrowsers).
func (c *copySet) Reconcile(ctx context.Context) error {
	ctx = context.WithoutCancel(ctx) // each stop has its own bound
	errs := c.reconcileServers(ctx)
	return errors.Join(append(errs, c.reconcileBrowsers(ctx)...)...)
}

// reconcileServers is Reconcile's part for the servers, each stop on its own
// bound.
func (c *copySet) reconcileServers(ctx context.Context) []error {
	c.srv.Lock()
	defer c.srv.Unlock()
	recs, err := c.st.KVValues(ctx, serverKeyPrefix+"*")
	if err != nil {
		return []error{err}
	}
	var errs []error
	for _, key := range slices.Sorted(maps.Keys(recs)) {
		sctx, cancel := stopContext(ctx)
		if _, _, _, err := c.stopServer(sctx, strings.TrimPrefix(key, serverKeyPrefix), "a stray the last daemon left"); err != nil {
			errs = append(errs, err)
		}
		cancel()
	}
	return append(errs, c.strayServers(ctx)...)
}

// lstartLayout is a process's start as ps prints it (lstart, the local
// time in the C locale).
const lstartLayout = "Mon Jan _2 15:04:05 2006"

// strayServers ends the servers the daemon spawned and never recorded (it
// stopped in between): the pidfile of a copy no record names, whose pid
// runs with a start the pidfile's time matches (the server's shell writes
// it first thing: pidStartSlack), names that server, and its group is
// ended; a pid another process took since has another start and is never
// killed. A server the pidfile names that still runs is the factory's, so
// its stop runs the rest of halt too: the repository's server_stop and the
// proof, which ends a listener it left in the copy on its port. A pidfile
// whose server no longer runs names nothing of the factory's (a start that
// failed under an older build left it, and a later task in the copy started
// nothing): it stops nothing and is forgotten. Every such pidfile is
// forgotten once its stop succeeds. The caller holds c.srv.
func (c *copySet) strayServers(ctx context.Context) []error {
	files, err := filepath.Glob(filepath.Join(c.cfg.Layout.Logs(), serverFilePrefix+"*"+serverPidSuffix))
	if err != nil || len(files) == 0 {
		return nil
	}
	table, err := c.processes(ctx)
	if err != nil {
		return []error{err}
	}
	var errs []error
	for _, f := range files {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), serverFilePrefix), serverPidSuffix)
		if _, recorded, err := c.serverOf(ctx, name); err != nil || recorded {
			continue // a record names it: stopServer's
		}
		if p, ok := pidfileServer(f, table); ok {
			sctx, cancel := stopContext(ctx)
			_, _, err := c.halt(sctx, name, serverRecord{PID: p.PID, Start: p.Start},
				"started just before the last daemon stopped, it was never recorded")
			cancel()
			if err != nil {
				errs = append(errs, fmt.Errorf("stop the server of %s (pid %d), which no record names: %w", name, p.PID, err))
				continue
			}
		}
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errs
}

// pidfileServer is the process of table the server's pidfile at path
// names: its pid, running, with a start at most pidStartSlack before the
// pidfile was written and not after it (lstart is cut to the second).
func pidfileServer(path string, table []execx.Process) (execx.Process, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return execx.Process{}, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	fi, serr := os.Stat(path)
	if err != nil || serr != nil || pid <= 0 {
		return execx.Process{}, false
	}
	for _, p := range table {
		if p.PID != pid || p.Zombie {
			continue
		}
		start, err := time.ParseInLocation(lstartLayout, p.Start, time.Local)
		if d := fi.ModTime().Sub(start); err == nil && d >= -time.Second && d <= pidStartSlack {
			return p, true
		}
	}
	return execx.Process{}, false
}

// stopForRelease stops copy cp's server before its release and says what it
// did, for handoff.md. A process outside the copy on its port does not hold
// the release up (it is not the copy's to hand back): handoff.md names it.
func (c *copySet) stopForRelease(ctx context.Context, cp Copy) (string, error) {
	ctx, cancel := stopContext(ctx)
	defer cancel()
	c.srv.Lock()
	defer c.srv.Unlock()
	rec, ran, held, err := c.stopServer(ctx, cp.Name, "its task left the copy")
	if err != nil {
		return "not running", err
	}
	note := ""
	if held != nil {
		note = "; " + held.Error()
	}
	if !ran {
		return "not running" + note, nil
	}
	return fmt.Sprintf("stopped at the release (it ran as pid %d since %s)%s", rec.PID, rec.Since.UTC().Format(time.RFC3339), note), nil
}

// stopServer ends copy name's recorded server (halt) and forgets it once
// nothing of it is left; ran says its leader was running, held is the
// portHeld of a process outside the copy that holds its port (the server is
// forgotten all the same: that process is not the server's). A stop that
// failed, or that left a listener of the copy running, keeps the record, so
// the next stop, or a restart's reconcile, ends it. A copy no record names
// had no server the factory started: nothing is stopped, signalled or
// proven, and a pidfile left there is forgotten (the reconcile at the
// daemon's start already read the pidfiles: one now is stale). The caller
// holds c.srv.
func (c *copySet) stopServer(ctx context.Context, name, why string) (rec serverRecord, ran bool, held, err error) {
	rec, ok, err := c.serverOf(ctx, name)
	if err != nil {
		return rec, false, nil, err
	}
	if !ok {
		if err := os.Remove(c.serverPidFile(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return rec, false, nil, err
		}
		return rec, false, nil, nil
	}
	ran, held, err = c.halt(ctx, name, rec, why)
	if err != nil {
		return rec, ran, held, err
	}
	if err := os.Remove(c.serverPidFile(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return rec, ran, held, err
	}
	return rec, ran, held, c.st.DeleteKV(ctx, serverKey(name))
}

// halt ends what the factory started as copy name's server, each step even
// when one before it failed (errors.Join): the repository's server_stop in
// the copy, bounded by serverStopCap (overmind's tmux server leaves the
// group: its own stop ends it), then the server's process group while its
// leader is rec (its pid and its start; a zero rec has none), then the
// proof (proveStopped). ran says the leader was running; held is the
// proof's portHeld, apart from the failures. Its begin, ok and fail events
// are factory.server_stop's. The caller holds c.srv.
func (c *copySet) halt(ctx context.Context, name string, rec serverRecord, why string) (ran bool, held, err error) {
	cp, repo, known := c.copyNamed(name, rec.Path)
	c.phase(ctx, name, kindServerStop, store.PhaseBegin, fmt.Sprintf("stop the server of %s: %s", name, why))
	var errs []error
	if known && repo.QA != nil && strings.TrimSpace(repo.QA.ServerStop) != "" {
		c.serverStop(ctx, cp, repo.QA.ServerStop)
	}
	if rec.PID > 0 {
		var err error
		if _, ran, err = c.running(ctx, rec); err != nil {
			errs = append(errs, err)
		} else if ran {
			if err := c.end(ctx, rec); err != nil {
				errs = append(errs, fmt.Errorf("stop the server of %s (pid %d): %w", name, rec.PID, err))
			} else {
				c.event(ctx, name, "info", "factory.server_stopped", fmt.Sprintf("stopped the server of %s (pid %d): %s", name, rec.PID, why))
			}
		}
	}
	if known {
		proof := c.proveStopped(ctx, cp)
		if _, ok := errors.AsType[portHeld](proof); ok {
			held = proof
		} else if proof != nil {
			errs = append(errs, proof)
		}
	}
	err = errors.Join(errs...)
	switch {
	case err != nil:
		c.phase(ctx, name, kindServerStop, store.PhaseFail, errors.Join(err, held).Error())
	case held != nil:
		c.phase(ctx, name, kindServerStop, store.PhaseFail, fmt.Sprintf("stopped the server of %s, but %s", name, held))
	case !known:
		c.phase(ctx, name, kindServerStop, store.PhaseOK, fmt.Sprintf("stopped the server of %s; no [[factory.repo]] has the copy now, "+
			"so nothing of it was proven free", name))
	default:
		c.phase(ctx, name, kindServerStop, store.PhaseOK, fmt.Sprintf("stopped the server of %s; nothing of the copy listens, and its port is free", name))
	}
	return ran, held, err
}

// serverStop runs the repository's server_stop command in copy cp, bounded
// by serverStopCap. A failure is its event, and the stop goes on.
func (c *copySet) serverStop(ctx context.Context, cp Copy, command string) {
	c.phase(ctx, cp.Name, kindServerStopCommand, store.PhaseBegin, fmt.Sprintf("run %s in %s before its server's group is killed", command, cp.Name))
	if err := c.script(ctx, cp, command, serverStopCap); err != nil {
		c.phase(ctx, cp.Name, kindServerStopCommand, store.PhaseFail, err.Error()+"; the stop goes on")
		return
	}
	c.phase(ctx, cp.Name, kindServerStopCommand, store.PhaseOK, fmt.Sprintf("ran %s in %s", command, cp.Name))
}

// copyNamed is copy name as its repository renders it: its checkout (path,
// a record's, when set), its number, its port and its env, and the
// repository; false when no [[factory.repo]] has the copy any more.
func (c *copySet) copyNamed(name, path string) (Copy, config.FactoryRepo, bool) {
	repo, n, ok := c.repoOf(name)
	if !ok {
		return Copy{}, repo, false
	}
	env := copyEnv(repo, n)
	port, _ := strconv.Atoi(env["PORT"])
	return Copy{Name: name, Path: cmp.Or(path, repo.Pool().Path(n)), Number: n, Port: port, Env: env}, repo, true
}

// end sends the server's process group SIGTERM and, when a member of the
// group still runs after serverStopGrace, SIGKILL, and waits for the group
// to end once more. The caller found the group's leader to be the server
// (its pid and its start): a group outlives its leader while a member runs,
// and no other process takes its id meanwhile, so a member that ignores
// SIGTERM is not left behind. A group that outlives SIGKILL is an error:
// its record stays.
func (c *copySet) end(ctx context.Context, rec serverRecord) error {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if err := c.kill(rec.PID, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		if gone, err := c.waitGone(ctx, func(table []execx.Process) bool {
			return !slices.ContainsFunc(table, func(p execx.Process) bool { return p.PGID == rec.PID && !p.Zombie })
		}); err != nil || gone {
			return err
		}
	}
	return fmt.Errorf("its process group %d outlived SIGKILL", rec.PID)
}

// waitGone looks at the process table every serverPoll, for at most
// serverStopGrace, until gone says what it waits for has ended.
func (c *copySet) waitGone(ctx context.Context, gone func([]execx.Process) bool) (bool, error) {
	for i := 0; ; i++ {
		table, err := c.processes(ctx)
		if err != nil || gone(table) {
			return err == nil, err
		}
		if i >= int(serverStopGrace/serverPoll) {
			return false, nil
		}
		if err := c.sleep(ctx, serverPoll); err != nil {
			return false, err
		}
	}
}

// listener is a process listening on a TCP port as lsof shows it: its pid,
// its port, its command (lsof cuts it to 9 characters) and its working
// directory, and its start as the process table showed it just before
// (listening).
type listener struct {
	PID, Port           int
	Start, Command, Cwd string
}

// portHeld is a stop's proof that found the copy's port held by a process
// outside the copy, after the factory's own were gone: that process is
// never signalled, and QA never walks it. pid is 0 when lsof shows no
// process on the port that answers there.
type portHeld struct {
	port, pid int
	command   string
}

func (e portHeld) Error() string {
	if e.pid <= 0 {
		return fmt.Sprintf("port %d is held by a process outside the copy (it answers there, and lsof shows no process of yours on it); "+
			"the factory leaves it alone", e.port)
	}
	return fmt.Sprintf("port %d is held by a process outside the copy (pid %d, %s); the factory leaves it alone", e.port, e.pid, e.command)
}

// proveStopped proves, after the server's group ended, that the copy's
// port is free. A listener on the copy's port whose working directory is
// the copy or below it is the server's, left outside its group (overmind's
// tmux panes): it is stopped while its pid and its start match the process
// table (endListener), never by its pid alone. Nothing else is signalled: a
// process outside the copy, whatever port it listens on, and a listener in
// the copy on another port (a helper of the factory's own agents, a test
// server or a debugger, which a QA stop must not end). Then it looks again:
// a listener of the copy still on the port is the error that names it;
// else one outside the copy on the port, or anything lsof does not show
// that answers there (IPv4 or IPv6), is a portHeld, returned bare. A copy
// with no port has nothing to prove.
func (c *copySet) proveStopped(ctx context.Context, cp Copy) error {
	if cp.Port <= 0 {
		return nil
	}
	server := func(l listener) bool { return l.Port == cp.Port && inCopy(l.Cwd, cp.Path) }
	ls, err := c.listening(ctx)
	if err != nil {
		return fmt.Errorf("prove the server of %s stopped: %w", cp.Name, err)
	}
	for _, l := range ls {
		if server(l) && l.Start != "" {
			c.endListener(ctx, cp, l)
		}
	}
	if ls, err = c.listening(ctx); err != nil {
		return fmt.Errorf("prove the server of %s stopped: %w", cp.Name, err)
	}
	var left []error
	for _, l := range ls {
		if server(l) {
			left = append(left, fmt.Errorf("the server of %s left pid %d (%s) listening on :%d", cp.Name, l.PID, l.Command, l.Port))
		}
	}
	if len(left) > 0 {
		return errors.Join(left...)
	}
	if i := slices.IndexFunc(ls, func(l listener) bool { return l.Port == cp.Port }); i >= 0 {
		return portHeld{port: cp.Port, pid: ls[i].PID, command: ls[i].Command}
	}
	port := strconv.Itoa(cp.Port)
	for _, host := range []string{"127.0.0.1", "::1"} {
		if c.dial(ctx, net.JoinHostPort(host, port)) == nil {
			return portHeld{port: cp.Port}
		}
	}
	return nil
}

// listening is every TCP listener (c.listeners) with its start: the process
// table is read first, so a pid a later process takes after it never gets
// the earlier one's start, and a listener the table does not show has none
// (it is never signalled).
func (c *copySet) listening(ctx context.Context) ([]listener, error) {
	table, err := c.processes(ctx)
	if err != nil {
		return nil, err
	}
	ls, err := c.listeners(ctx)
	if err != nil {
		return nil, err
	}
	for i, l := range ls {
		if j := slices.IndexFunc(table, func(p execx.Process) bool { return p.PID == l.PID && !p.Zombie }); j >= 0 {
			ls[i].Start = table[j].Start
		}
	}
	return ls, nil
}

// endListener stops l, a listener the server's group left in copy cp: that
// one process, never its group, SIGTERM then SIGKILL after serverStopGrace,
// each only while its pid and its start match the process table. Its begin,
// ok and fail events are factory.server_stray's; one that outlives SIGKILL
// is the proof's error.
func (c *copySet) endListener(ctx context.Context, cp Copy, l listener) {
	what := fmt.Sprintf("pid %d (%s), a listener the group left in %s on :%d", l.PID, l.Command, cp.Name, l.Port)
	c.phase(ctx, cp.Name, kindServerStray, store.PhaseBegin, "stop "+what)
	p := execx.Process{PID: l.PID, Start: l.Start}
	gone := func(table []execx.Process) bool { _, runs := execx.Find(table, p); return !runs }
	table, err := c.processes(ctx)
	if err == nil && gone(table) {
		c.phase(ctx, cp.Name, kindServerStray, store.PhaseOK, what+": it had ended")
		return
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if err == nil {
			err = c.signal(l.PID, sig) // the table showed it with its start just now
		}
		ended := false
		if err == nil || errors.Is(err, syscall.ESRCH) {
			ended, err = c.waitGone(ctx, gone)
		}
		if err != nil {
			c.phase(ctx, cp.Name, kindServerStray, store.PhaseFail, "stop "+what+": "+err.Error())
			return
		}
		if ended {
			c.phase(ctx, cp.Name, kindServerStray, store.PhaseOK, "stopped "+what)
			return
		}
	}
	c.phase(ctx, cp.Name, kindServerStray, store.PhaseFail, "stop "+what+": it outlived SIGKILL")
}

// inCopy reports whether cwd is the copy's checkout at path or below it, by
// the path or by its target when it is a link (lsof shows the target).
func inCopy(cwd, path string) bool {
	if inDir(cwd, path) {
		return true
	}
	target, err := filepath.EvalSymlinks(path)
	return err == nil && target != path && inDir(cwd, target)
}

// inDir reports whether dir is root or below it; an empty one is neither.
func inDir(dir, root string) bool {
	if dir == "" || root == "" {
		return false
	}
	rel, err := filepath.Rel(root, dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../") && !filepath.IsAbs(rel)
}

// lsofListeners is every TCP listener on the Mac with its working directory:
// `lsof -nP -iTCP -sTCP:LISTEN -Fpcn`, then `lsof -a -d cwd -p <pids> -Fpn`.
// lsof exits 1 when it finds nothing, or a pid it was given has ended, and
// says nothing but warnings then; what it printed is still read. An exit 1
// with an error on stderr fails the listing.
func (c *copySet) lsofListeners(ctx context.Context) ([]listener, error) {
	lsof := func(label string, args ...string) ([]byte, error) {
		res, err := c.run.Run(ctx, execx.Cmd{Name: "lsof", Args: args, Env: map[string]string{"LC_ALL": "C"}, Timeout: lsofTimeout,
			Label: label, Probe: true})
		if e, ok := errors.AsType[*execx.ExitError](err); ok && e.Code == 1 && onlyWarnings(res.Stderr) {
			err = nil
		}
		return res.Stdout, err
	}
	out, err := lsof("lsof TCP listeners", "-nP", "-iTCP", "-sTCP:LISTEN", "-Fpcn")
	if err != nil {
		return nil, fmt.Errorf("list the TCP listeners: %w", err)
	}
	ls := parseLsofListeners(out)
	if len(ls) == 0 {
		return nil, nil
	}
	var pids []string
	for _, l := range ls {
		if p := strconv.Itoa(l.PID); !slices.Contains(pids, p) {
			pids = append(pids, p)
		}
	}
	if out, err = lsof("lsof listeners' cwd", "-a", "-d", "cwd", "-p", strings.Join(pids, ","), "-Fpn"); err != nil {
		return nil, fmt.Errorf("read the TCP listeners' working directories: %w", err)
	}
	cwd := parseLsofCwd(out)
	for i := range ls {
		ls[i].Cwd = cwd[ls[i].PID]
	}
	return ls, nil
}

// onlyWarnings reports whether lsof's stderr holds nothing but its
// warnings ("lsof: WARNING: can't stat() …").
func onlyWarnings(stderr []byte) bool {
	for line := range strings.SplitSeq(string(stderr), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "lsof: WARNING") {
			return false
		}
	}
	return true
}

// parseLsofListeners reads lsof's -Fpcn output of the listening sockets: a
// p line starts a process, a c line is its command, an n line one socket
// (*:3110, 127.0.0.1:3110, [::1]:3110); a process's port is listed once
// however many sockets it has on it.
func parseLsofListeners(out []byte) []listener {
	var ls []listener
	pid, command := 0, ""
	for line := range strings.SplitSeq(string(out), "\n") {
		if line == "" {
			continue
		}
		v := line[1:]
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(v)
			command = ""
		case 'c':
			command = v
		case 'n':
			i := strings.LastIndexByte(v, ':')
			if i < 0 || pid <= 0 {
				continue
			}
			port, err := strconv.Atoi(v[i+1:])
			if l := (listener{PID: pid, Port: port, Command: command}); err == nil && port > 0 && !slices.Contains(ls, l) {
				ls = append(ls, l)
			}
		}
	}
	return ls
}

// parseLsofCwd reads lsof's -Fpn output of the processes' cwd: each pid's
// working directory.
func parseLsofCwd(out []byte) map[int]string {
	cwd := map[int]string{}
	pid := 0
	for line := range strings.SplitSeq(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "p"):
			pid, _ = strconv.Atoi(line[1:])
		case strings.HasPrefix(line, "n") && pid > 0:
			if _, ok := cwd[pid]; !ok {
				cwd[pid] = line[1:]
			}
		}
	}
	return cwd
}

// dialTCP connects to addr and hangs up: nil says something answers there.
func dialTCP(ctx context.Context, addr string) error {
	d := net.Dialer{Timeout: time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

// isFile reports whether path is a file (not a directory) that exists.
func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// serverOf is copy name's recorded server.
func (c *copySet) serverOf(ctx context.Context, name string) (serverRecord, bool, error) {
	return c.recordOf(ctx, serverKey(name))
}

// recordOf is the server or browser record under kv key.
func (c *copySet) recordOf(ctx context.Context, key string) (serverRecord, bool, error) {
	v, ok, err := c.st.GetKV(ctx, key)
	if err != nil || !ok {
		return serverRecord{}, false, err
	}
	var rec serverRecord
	if err := json.Unmarshal([]byte(v), &rec); err != nil || rec.PID <= 0 || rec.Start == "" {
		// A record that names no process kills nothing: forget it.
		return serverRecord{}, false, c.st.DeleteKV(ctx, key)
	}
	return rec, true, nil
}

// setRecord records rec under kv key.
func (c *copySet) setRecord(ctx context.Context, key string, rec serverRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return c.st.SetKV(ctx, key, string(b))
}

// running is the process a server record names while it runs: the same pid
// with the same start (execx.Find), never a process that only took its pid.
func (c *copySet) running(ctx context.Context, rec serverRecord) (execx.Process, bool, error) {
	table, err := c.processes(ctx)
	if err != nil {
		return execx.Process{}, false, err
	}
	p, ok := execx.Find(table, execx.Process{PID: rec.PID, Start: rec.Start})
	return p, ok, nil
}

// serverShell is the shell line a server starts with: it writes its own
// pid ($$) to the pidfile its first argument names, then becomes the
// server's command (exec keeps the pid), so a server is found by its pid
// whenever the daemon stops after the spawn.
const serverShell = `printf '%s\n' "$$" > "$1" || exit 1; shift; exec "$@"`

// spawnServer starts command in the copy at dir as a pool script runs
// (`mise -C <dir> exec -- env <env> /bin/sh -c <command>`), with the
// daemon's environment less what the copies' commands never inherit
// (unset), the factory's shims first on its PATH and BROWSER=true
// (serverEnv), in a process group it leads, so a stop ends the processes it
// starts too; its shell writes the group leader's pid to pidfile first
// (serverShell). Its output goes to log. It does not wait for the server,
// which outlives the call and its context; a goroutine reaps it when it
// ends.
func (c *copySet) spawnServer(ctx context.Context, dir string, env map[string]string, command, log, pidfile string) (int, error) {
	f, err := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	args := append([]string{"-c", serverShell, "sh", pidfile, "mise"}, slots.MiseExecArgs(dir, env, command)...)
	cmd := exec.CommandContext(context.WithoutCancel(ctx), "/bin/sh", args...)
	cmd.Dir = dir
	cmd.Env = serverEnv(os.Environ(), c.cfg.Layout.FactoryShims(), c.unset())
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		return 0, err
	}
	go func() {
		_ = cmd.Wait()
		_ = f.Close()
	}()
	return cmd.Process.Pid, nil
}

// serverEnv is the environment a copy's server starts with: base less the
// variables the copies never inherit (unset), with the factory's shims
// first on its PATH (their `open` opens nothing: openShim) and BROWSER=true
// (what opens a page by $BROWSER runs `true`), so a server that opens a
// page at its start opens no tab in your browser.
func serverEnv(base []string, shims string, unset []string) []string {
	path := ""
	env := make([]string, 0, len(base)+2)
	for _, kv := range base {
		switch k, v, _ := strings.Cut(kv, "="); {
		case k == "PATH":
			path = v
		case k == "BROWSER", slices.Contains(unset, k):
		default:
			env = append(env, kv)
		}
	}
	if path != "" {
		shims += ":" + path
	}
	return append(env, "PATH="+shims, "BROWSER=true")
}

// openShim is the factory's `open` in FactoryShims: it says on stderr what
// it refused and exits 0, so a server whose start opens a page goes on.
const openShim = "#!/bin/sh\nprintf 'magnum factory: refused `open %s`: a copy'\\''s server opens no browser tab\\n' \"$*\" >&2; exit 0\n"

// ensureOpenShim writes the factory's `open` (openShim) to dir, executable.
func ensureOpenShim(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return fsx.WriteFileAtomic(filepath.Join(dir, "open"), []byte(openShim), 0o755)
}
