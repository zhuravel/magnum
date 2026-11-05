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

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/slots"
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
// it ended, and sends SIGKILL after serverStopGrace.
const (
	serverStopGrace = 10 * time.Second
	serverPoll      = 250 * time.Millisecond
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
		if err := c.st.DeleteKV(ctx, serverKey(cp.Name)); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(c.cfg.Layout.Logs(), 0o700); err != nil {
		return fmt.Errorf("start the server of %s: %w", cp.Name, err)
	}
	if err := os.Remove(c.serverPidFile(cp.Name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("start the server of %s: %w", cp.Name, err)
	}
	log := c.serverLog(cp.Name)
	pid, err := c.spawn(ctx, cp.Path, cp.Env, repo.Server, log, c.serverPidFile(cp.Name))
	if err != nil {
		return fmt.Errorf("start the server of %s: %w", cp.Name, err)
	}
	table, err := c.processes(ctx)
	if err != nil {
		_ = c.kill(pid, syscall.SIGTERM) // a server no record names would never be stopped
		return fmt.Errorf("start the server of %s: %w", cp.Name, err)
	}
	i := slices.IndexFunc(table, func(p execx.Process) bool { return p.PID == pid && !p.Zombie && p.Start != "" })
	if i < 0 {
		return fmt.Errorf("the server of %s (pid %d) ended at once; its log is %s", cp.Name, pid, log)
	}
	b, err := json.Marshal(serverRecord{PID: pid, Start: table[i].Start, Path: cp.Path, Since: c.now()})
	if err == nil {
		err = c.st.SetKV(ctx, serverKey(cp.Name), string(b))
	}
	if err != nil {
		_ = c.kill(pid, syscall.SIGTERM)
		return fmt.Errorf("record the server of %s: %w", cp.Name, err)
	}
	c.event(ctx, cp.Name, "info", "factory.server_started", fmt.Sprintf("the server of %s runs as pid %d; its log is %s", cp.Name, pid, log))
	return nil
}

// StopServer stops copy cp's server, if one is recorded and still runs, and
// forgets it.
func (c *copySet) StopServer(ctx context.Context, cp Copy) error {
	c.srv.Lock()
	defer c.srv.Unlock()
	_, _, err := c.stopServer(ctx, cp.Name, "stopped")
	return err
}

// Reconcile ends the servers a stopped daemon left. After a restart no stage
// owns a server (the engine runs it before its first tick), so each one is
// a stray: a recorded one is killed only while the process at its pid has
// the start its record names, never by its pid alone (that pid may be
// another process's now), and every record is forgotten; one the daemon
// spawned just before it crashed, which no record names, is found by its
// pidfile (strayServers).
func (c *copySet) Reconcile(ctx context.Context) error {
	c.srv.Lock()
	defer c.srv.Unlock()
	recs, err := c.st.KVValues(ctx, serverKeyPrefix+"*")
	if err != nil {
		return err
	}
	var errs []error
	for _, key := range slices.Sorted(maps.Keys(recs)) {
		if _, _, err := c.stopServer(ctx, strings.TrimPrefix(key, serverKeyPrefix), "a stray the last daemon left"); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(append(errs, c.strayServers(ctx)...)...)
}

// lstartLayout is a process's start as ps prints it (lstart, the local
// time in the C locale).
const lstartLayout = "Mon Jan _2 15:04:05 2006"

// strayServers ends the servers the daemon spawned and never recorded (it
// stopped in between): the pidfile of a copy no record names, whose pid
// runs with a start the pidfile's time matches (the server's shell writes
// it first thing: pidStartSlack), names that server, and its group is
// ended; a pid another process took since has another start and is never
// killed. Every such pidfile is forgotten. The caller holds c.srv.
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
			if err := c.end(ctx, serverRecord{PID: p.PID, Start: p.Start}); err != nil {
				errs = append(errs, fmt.Errorf("stop the server of %s (pid %d), which no record names: %w", name, p.PID, err))
				continue
			}
			c.event(ctx, name, "info", "factory.server_stopped", fmt.Sprintf("stopped the server of %s (pid %d): started just "+
				"before the last daemon stopped, it was never recorded", name, p.PID))
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
// did, for handoff.md.
func (c *copySet) stopForRelease(ctx context.Context, cp Copy) (string, error) {
	c.srv.Lock()
	defer c.srv.Unlock()
	rec, ran, err := c.stopServer(ctx, cp.Name, "its task left the copy")
	if err != nil || !ran {
		return "not running", err
	}
	return fmt.Sprintf("stopped at the release (it ran as pid %d since %s)", rec.PID, rec.Since.UTC().Format(time.RFC3339)), nil
}

// stopServer ends copy name's recorded server while it still runs (end) and
// forgets it; ran says it was running. The caller holds c.srv.
func (c *copySet) stopServer(ctx context.Context, name, why string) (rec serverRecord, ran bool, err error) {
	rec, ok, err := c.serverOf(ctx, name)
	if err != nil || !ok {
		return rec, false, err
	}
	_, ran, err = c.running(ctx, rec)
	if err != nil {
		return rec, false, err
	}
	if ran {
		if err := c.end(ctx, rec); err != nil {
			return rec, true, fmt.Errorf("stop the server of %s (pid %d): %w", name, rec.PID, err)
		}
		c.event(ctx, name, "info", "factory.server_stopped", fmt.Sprintf("stopped the server of %s (pid %d): %s", name, rec.PID, why))
	}
	if err := os.Remove(c.serverPidFile(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return rec, ran, err
	}
	return rec, ran, c.st.DeleteKV(ctx, serverKey(name))
}

// end sends the server's process group SIGTERM and, when a member of the
// group still runs after serverStopGrace, SIGKILL. The caller found the
// group's leader to be the server (its pid and its start): a group outlives
// its leader while a member runs, and no other process takes its id
// meanwhile, so a member that ignores SIGTERM is not left behind.
func (c *copySet) end(ctx context.Context, rec serverRecord) error {
	if err := c.kill(rec.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	for range int(serverStopGrace / serverPoll) {
		if alive, err := c.groupRuns(ctx, rec.PID); err != nil || !alive {
			return err
		}
		if err := c.sleep(ctx, serverPoll); err != nil {
			return err
		}
	}
	if alive, err := c.groupRuns(ctx, rec.PID); err != nil || !alive {
		return err
	}
	if err := c.kill(rec.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// groupRuns reports whether a process of process group pgid still runs.
func (c *copySet) groupRuns(ctx context.Context, pgid int) (bool, error) {
	table, err := c.processes(ctx)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(table, func(p execx.Process) bool { return p.PGID == pgid && !p.Zombie }), nil
}

// serverOf is copy name's recorded server.
func (c *copySet) serverOf(ctx context.Context, name string) (serverRecord, bool, error) {
	v, ok, err := c.st.GetKV(ctx, serverKey(name))
	if err != nil || !ok {
		return serverRecord{}, false, err
	}
	var rec serverRecord
	if err := json.Unmarshal([]byte(v), &rec); err != nil || rec.PID <= 0 || rec.Start == "" {
		// A record that names no process kills nothing: forget it.
		return serverRecord{}, false, c.st.DeleteKV(ctx, serverKey(name))
	}
	return rec, true, nil
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
// (unset), in a process group it leads, so a stop ends the processes it
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
	unset := c.unset()
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return slices.Contains(unset, k)
	})
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
