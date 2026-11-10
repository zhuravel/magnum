package runtime

// The heal of a factory herdr server that macOS denies the copies' volume
// (2026-11-10: a server that had run for days lost its access to the
// external volume the copies are on, and every agent it started exited at
// once, its shell's getcwd and the CLI saying EPERM, while the daemon and
// any process started from your terminal read the volume fine). A launch
// that fails so (the agent never takes its first prompt: herdr refuses the
// prompt, or the pane is lost before it) and whose pane shows the refusal
// (deniedLine), while the daemon reads the copy (seams.readable), is that
// server's: the factory stops its own herdr session's server once (herdr's
// server.stop), only while no agent of another task lives in the session,
// so the engine's reveal starts a fresh one from your terminal, and
// relaunches the task's roles that took no prompt yet there, once. The
// same refusal after that is yours (DeniedError, EventVolumeDenied), with
// one line that says what to allow; your retry starts the heal over.
// A refusal the daemon shares is the copy's or its volume's: no herdr
// server is stopped, and the recoveries' steps go on as for any lost pane.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// deniedRe matches a line a pane shows once its shell and its CLI could not
// read the directory they start in because macOS refused it (EPERM), on
// their stable words: the shell's getcwd, Claude Code's own check, Codex's
// error. "Operation not permitted" alone is any refused call's.
var deniedRe = regexp.MustCompile(`getcwd:.*Operation not permitted|Can't read the current directory \(EPERM\)|` +
	`Operation not permitted \(os error 1\)`)

// deniedLine is the last line of screen that shows such a refusal, trimmed,
// redacted and clipped ("" for none).
func deniedLine(screen string) string {
	last := ""
	for line := range strings.SplitSeq(strings.ReplaceAll(screen, "\r", ""), "\n") {
		if deniedRe.MatchString(line) {
			last = strings.TrimSpace(line)
		}
	}
	return textx.Clip(execx.Redact(last), 200)
}

// DeniedLine is what a task waits on you for once macOS still blocks the
// factory's herdr server from reading volume after its one heal: what to
// allow, then your retry.
func DeniedLine(volume string) string {
	return "macOS blocks the factory's herdr server from reading " + volume + ": allow herdr (and magnum) for removable volumes " +
		"in System Settings > Privacy & Security > Files and Folders, then retry"
}

// volumeOf is the volume a copy's checkout dir is on, as DeniedLine names
// it: /Volumes/<name> for a mounted volume, else dir itself.
func volumeOf(dir string) string {
	if rest, ok := strings.CutPrefix(filepath.Clean(dir), "/Volumes/"); ok {
		if name, _, _ := strings.Cut(rest, "/"); name != "" {
			return "/Volumes/" + name
		}
	}
	return dir
}

// DeniedError is Send's answer while macOS blocks the factory's herdr
// server from reading Volume after the heal, or while the factory may not
// heal it (its herdr session is yours, or its client cannot stop a
// server): the task needs you, with DeniedLine, until your retry.
type DeniedError struct{ Volume string }

// Error is DeniedLine of the volume.
func (e *DeniedError) Error() string { return DeniedLine(e.Volume) }

// serverStopper stops the herdr session's server (herdr.Client.ServerStop).
type serverStopper interface {
	ServerStop(ctx context.Context) error
}

var _ serverStopper = (*herdr.Client)(nil)

// readableWait bounds the daemon's read of a copy's directory: a read macOS
// holds for a permission dialog counts as one that failed.
const readableWait = 5 * time.Second

// readableDir reports whether the daemon reads dir: it opens it and lists
// one name, within readableWait.
func readableDir(dir string) bool {
	done := make(chan error, 1)
	go func() {
		f, err := os.Open(dir)
		if err == nil {
			_, err = f.Readdirnames(1)
			f.Close()
		}
		done <- err
	}()
	t := time.NewTimer(readableWait)
	defer t.Stop()
	select {
	case err := <-done:
		return err == nil || errors.Is(err, io.EOF) // an empty directory reads
	case <-t.C:
		return false
	}
}

// heal is a task's heal of the factory's herdr server in one attempt: the
// volume its server cannot read, the refusal's line and the role that
// showed it first; when the server was stopped (zero: not yet), the roles
// that took no prompt yet and are due their relaunch on the fresh server,
// those it relaunched, whether the task's rows were reconciled with that
// server (agents.Manager.Recover), the wait it last recorded, and when the
// line went to you (zero: not yet).
type heal struct {
	attempt, volume, line, role string
	stopped                     time.Time
	relaunch, relaunched        map[string]bool
	reconciled                  bool
	waits                       string
	told                        time.Time
}

// healOf is task t's heal in attempt a, a new one when it has none there:
// one you were told of is dropped once you retried since (your retry starts
// the heal over).
func (r *Runtime) healOf(ctx context.Context, t registry.Task, a registry.Attempt) *heal {
	r.mu.Lock()
	h := r.heals[t.ID]
	ours := h != nil && h.attempt == a.ID
	var told time.Time
	if ours {
		told = h.told
	}
	r.mu.Unlock()
	if ours && (told.IsZero() || !r.retriedSince(ctx, t.ID, told)) {
		return h
	}
	h = &heal{attempt: a.ID, relaunch: map[string]bool{}, relaunched: map[string]bool{}}
	r.mu.Lock()
	r.heals[t.ID] = h
	r.mu.Unlock()
	return h
}

// healing is task t's heal in attempt a when it holds role (due its
// relaunch, or told to you), else nil.
func (r *Runtime) healing(taskID int64, attempt, role string) *heal {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.heals[taskID]
	if h == nil || h.attempt != attempt || (!h.relaunch[role] && h.told.IsZero()) {
		return nil
	}
	return h
}

// healDue reports whether task t's heal in attempt is to relaunch role
// (not told to you).
func (r *Runtime) healDue(taskID int64, attempt, role string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.heals[taskID]
	return h != nil && h.attempt == attempt && h.relaunch[role] && h.told.IsZero()
}

// retriedSince reports whether you retried task id at or after at (a retry
// input the engine recorded): the ladders that ran out, and a heal told to
// you, start over.
func (r *Runtime) retriedSince(ctx context.Context, id int64, at time.Time) bool {
	ins, err := r.reg.Inputs(ctx, id)
	if err != nil {
		r.log.Warn("read a task's retries", "task", id, "err", err)
		return false
	}
	return slices.ContainsFunc(ins, func(in registry.Input) bool { return in.Kind == registry.InputRetry && !in.At.Before(at) })
}

// hadPrompt reports whether the ledger holds a prompt to role in attempt
// of task id that may have reached it (a ledger that cannot be read counts
// as one: the recoveries then take the steps they took before).
func (r *Runtime) hadPrompt(ctx context.Context, id int64, attempt, role string) bool {
	entries, err := r.reg.LedgerOf(ctx, id)
	return err != nil || hasPrompt(entries, attempt, role)
}

// launchDenied classifies a launch of role st in attempt a of task t that
// failed in session s before the role's first prompt: its pane shows macOS
// refusing the copy's directory (deniedLine) while the daemon reads it. It
// reports whether the heal takes it, with what the caller answers then: a
// wait (*WaitError), you in the pane, or the line for you (*DeniedError).
// A refusal the daemon shares is not taken (an event says so once): the
// copy's or its volume's, no herdr server's.
func (r *Runtime) launchDenied(ctx context.Context, t registry.Task, a registry.Attempt, st *roleState, s store.Session) (bool, error) {
	line := ""
	if screen, err := r.mgr.ReadRecent(ctx, s, agents.HealthLines); err == nil { // a pane that cannot be read shows no refusal
		line = deniedLine(screen)
	}
	path, _ := r.copyOf(st)
	dir := cmp.Or(path, store.Deref(s.Cwd))
	if line == "" || dir == "" {
		return false, nil
	}
	if !r.s.readable(dir) {
		if r.first("unreadable:" + a.ID + ":" + dir) {
			r.log.Warn("an agent's pane cannot read its copy, nor can the daemon: no herdr server is restarted", "task", t.ID,
				"role", st.role.Name, "copy", dir)
			r.event(ctx, t.ID, registry.PhaseInfo, st.role.Name+"'s agent exited at its start ("+line+"), and the factory cannot read "+
				dir+" either: the copy's or its volume's, so its herdr server is not restarted")
		}
		return false, nil
	}
	return true, r.denied(ctx, t, a, st, s, volumeOf(dir), line)
}

// denied takes a refusal of role st's launch in session s: the line for
// you once the heal relaunched the role, or once the role's session started
// after the server's stop; a wait for the relaunch while the stop went
// through; else the stop (stopFor). The heal holds the role from here: no
// recovery takes it (healing).
func (r *Runtime) denied(ctx context.Context, t registry.Task, a registry.Attempt, st *roleState, s store.Session, volume, line string) error {
	h := r.healOf(ctx, t, a)
	name := st.role.Name
	r.mu.Lock()
	if h.volume == "" {
		h.volume, h.line, h.role = volume, line, name
	}
	switch {
	case !h.told.IsZero():
		volume := h.volume
		r.mu.Unlock()
		return &DeniedError{Volume: volume}
	case h.relaunched[name] || (!h.stopped.IsZero() && s.StartedAt.After(h.stopped)): // it came back after the heal
		now := r.now()
		h.told = now
		delete(h.relaunch, name)
		st.lost, st.ranOut, st.recovering, st.pending = true, now, false, false
		volume := h.volume
		r.mu.Unlock()
		r.log.Warn("the herdr server the heal started cannot read the copies' volume either: the task is yours", "task", t.ID,
			"role", name, "volume", volume)
		r.event(ctx, t.ID, registry.PhaseFail, name+"'s agent exited at its start on the fresh herdr server too ("+line+"): "+
			DeniedLine(volume))
		return &DeniedError{Volume: volume}
	}
	h.relaunch[name] = true
	st.recovering, st.pending = false, false
	stopped := !h.stopped.IsZero()
	r.mu.Unlock()
	if stopped { // a launch from before the stop, read after it (herdr keeps a pane's scrollback)
		return r.restartWait(h)
	}
	return r.stopFor(ctx, t, a, st, h)
}

// deniedKey keys the one EventVolumeDenied of task id's heal, told to you
// at told (once per telling: your retry starts a new heal).
func deniedKey(id int64, told time.Time) string {
	return fmt.Sprintf("denied:%d:%d", id, told.UnixNano())
}

// toldAt is when task id's heal went to you (zero: it did not).
func (r *Runtime) toldAt(id int64) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h := r.heals[id]; h != nil {
		return h.told
	}
	return time.Time{}
}

// restartWait is the wait of a task's roles while heal h's fresh herdr
// server comes up and they are relaunched there.
func (r *Runtime) restartWait(h *heal) *WaitError {
	r.mu.Lock()
	defer r.mu.Unlock()
	return &WaitError{Who: pauseAgent, What: "the factory restarts its herdr server, which macOS blocked from reading " + h.volume +
		", and relaunches " + strings.Join(slices.Sorted(maps.Keys(h.relaunch)), " and ") + " there"}
}

// deniedErr is heal h's line for you.
func (r *Runtime) deniedErr(h *heal) *DeniedError {
	r.mu.Lock()
	defer r.mu.Unlock()
	return &DeniedError{Volume: h.volume}
}

// stopFor stops the factory's herdr server for heal h of task t (role st's
// refusal): never a session that is yours or magnum's review panes'
// (ownSession; the line goes to you), nor one a client cannot stop; while
// you are in the role's pane or its kind pauses, the stop waits
// (mayRestart), and while an agent of another task (or a liaison, or one
// of yours) lives in the session it waits too, saying whose, and is tried
// again on the role's next prompt. The stop's begin, end or failure are
// events; once it went through, every role of the attempt that took no
// prompt yet is due its relaunch, and EventHerdrStopped has the engine open
// the session again at once.
func (r *Runtime) stopFor(ctx context.Context, t registry.Task, a registry.Attempt, st *roleState, h *heal) error {
	r.mu.Lock()
	volume, line, role := h.volume, h.line, h.role
	r.mu.Unlock()
	stopper, ok := r.d.Herdr.(serverStopper)
	if !ok || !ownSession(r.d.Config) {
		now := r.now()
		r.mu.Lock()
		h.told = now
		st.lost, st.ranOut, st.recovering, st.pending = true, now, false, false
		r.mu.Unlock()
		r.event(ctx, t.ID, registry.PhaseFail, st.role.Name+"'s agent exited at its start ("+line+"); the factory's herdr session is "+
			"not its own, so it never stops its server: "+DeniedLine(volume))
		return &DeniedError{Volume: volume}
	}
	if err := r.mayRestart(ctx, t, st); err != nil {
		return err
	}
	pr, err := r.owner(ctx, t, a.ID)
	if err != nil {
		return err
	}
	others, err := r.othersLive(ctx, pr)
	if err != nil {
		return &WaitError{Who: pauseAgent, What: "the factory's herdr server cannot read " + volume + " (macOS blocks it), and the " +
			"factory cannot tell whether another task's agent lives in it: " + textx.Clip(textx.FirstLine(execx.Redact(err.Error())), 200)}
	}
	if len(others) > 0 {
		what := "the factory's herdr server cannot read " + volume + " (macOS blocks it): the factory restarts it once no other " +
			"task's agent lives in it (" + strings.Join(others, ", ") + ")"
		r.mu.Lock()
		said := h.waits == what
		h.waits = what
		r.mu.Unlock()
		if !said {
			r.event(ctx, t.ID, registry.PhaseInfo, what)
		}
		return &WaitError{Who: pauseAgent, What: what}
	}
	r.event(ctx, t.ID, registry.PhaseBegin, "stops the factory's herdr server: macOS blocks it from reading "+volume+" ("+
		role+"'s pane: "+line+"), which the factory reads")
	if err := stopper.ServerStop(ctx); err != nil && !herdr.NoServer(err) {
		why := textx.Clip(textx.FirstLine(execx.Redact(err.Error())), 200)
		r.event(ctx, t.ID, registry.PhaseFail, "the stop of the factory's herdr server failed: "+why)
		return &WaitError{Who: pauseAgent, What: "the factory's herdr server cannot read " + volume + " (macOS blocks it), and its " +
			"stop failed (" + why + "): the factory tries again"}
	}
	entries, lerr := r.reg.LedgerOf(ctx, t.ID) // unread: the refused role alone is due, the others' recoveries take them
	r.mu.Lock()
	h.stopped = r.now()
	for _, x := range r.roles {
		if lerr == nil && x.taskID == t.ID && x.key.attempt == a.ID && !x.lost && !x.stuck && !hasPrompt(entries, a.ID, x.role.Name) {
			h.relaunch[x.role.Name] = true
			x.recovering, x.pending = false, false
		}
	}
	roles := strings.Join(slices.Sorted(maps.Keys(h.relaunch)), " and ")
	r.mu.Unlock()
	r.log.Warn("stopped the factory's herdr server: macOS blocks it from reading the copies' volume", "task", t.ID, "volume", volume)
	r.emit("", Event{TaskID: t.ID, Role: st.role.Name, Kind: EventHerdrStopped, Reason: volume})
	r.event(ctx, t.ID, registry.PhaseOK, "stopped the factory's herdr server: the factory opens it again and relaunches "+roles+" there")
	return r.restartWait(h)
}

// healStep is the heal's part of a prompt to role st of task t (Send): the
// line for you once told (until your retry), the stop's next try while it
// waits, and, once the server was stopped and answers again, the role's
// relaunch there, the task's rows reconciled with it first; nil when the
// prompt may go.
func (r *Runtime) healStep(ctx context.Context, t registry.Task, a registry.Attempt, st *roleState, pr store.PR) error {
	r.mu.Lock()
	h := r.heals[t.ID]
	ours := h != nil && h.attempt == a.ID
	r.mu.Unlock()
	if !ours {
		return nil
	}
	h = r.healOf(ctx, t, a) // your retry since the line drops it
	name := st.role.Name
	r.mu.Lock()
	told, stopped, due := !h.told.IsZero(), !h.stopped.IsZero(), h.relaunch[name]
	r.mu.Unlock()
	switch {
	case told:
		return r.deniedErr(h)
	case !due:
		return nil
	case !stopped:
		return r.stopFor(ctx, t, a, st, h)
	}
	if _, err := r.d.Herdr.Snapshot(ctx); err != nil {
		return r.restartWait(h)
	}
	r.mu.Lock()
	reconciled, stoppedAt := h.reconciled, h.stopped
	r.mu.Unlock()
	if !reconciled {
		if _, err := r.mgr.Recover(ctx, pr); err != nil {
			r.log.Warn("reconcile a task's sessions with the fresh herdr server", "task", t.ID, "err", err)
			return r.restartWait(h)
		}
		r.mu.Lock()
		h.reconciled = true
		r.mu.Unlock()
	}
	if err := r.mayRestart(ctx, t, st); err != nil {
		return err
	}
	if s, ok := r.liveSession(ctx, st); !ok || !s.StartedAt.After(stoppedAt) {
		if err := r.relaunchRole(ctx, t, a, st); err != nil {
			r.log.Warn("relaunch a role on the fresh herdr server", "task", t.ID, "role", name, "err", err)
			return &WaitError{Who: pauseAgent, What: "the factory relaunches " + name + " on its fresh herdr server once its start " +
				"goes through: " + textx.Clip(textx.FirstLine(execx.Redact(err.Error())), 200)}
		}
	}
	r.mu.Lock()
	delete(h.relaunch, name)
	h.relaunched[name] = true
	r.mu.Unlock()
	r.event(ctx, t.ID, registry.PhaseInfo, "relaunched "+name+" on the factory's fresh herdr server")
	return nil
}

// ownSession reports whether the factory's herdr session is its own: a
// named session that is neither yours ([terminal] session, [herdr] socket)
// nor magnum's review panes'. Only its server may a heal stop.
func ownSession(c *config.Config) bool {
	f, y, rv := c.FactoryHerdr(), c.YourHerdr(), c.ReviewHerdr()
	if f.Name == "default" || f.Name == y.Name || f.Name == rv.Name {
		return false
	}
	same := func(a, b string) bool { return a != "" && b != "" && filepath.Clean(a) == filepath.Clean(b) }
	return !same(f.Socket, y.Socket) && !same(f.Socket, rv.Socket) && !same(f.Socket, herdr.SessionSocket(y.Name))
}

// othersLive names the agents the factory's herdr session shows that are
// none of owner row pr's (a task's own agents): another task's ("PR-12
// fx-repro"), a liaison's, its own task's too ("PR-12's liaison": you may be
// talking with it), or one the factory did not start ("an agent in pane
// w3:p1"). A stop of the session's server would end them all.
func (r *Runtime) othersLive(ctx context.Context, pr store.PR) ([]string, error) {
	snap, err := r.d.Herdr.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	live, err := r.st.LiveSessions(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ag := range snap.Agents {
		i := slices.IndexFunc(live, func(s store.Session) bool {
			name := store.Deref(s.AgentName)
			return (name != "" && name == ag.Name) || (ag.PaneID != "" && store.Deref(s.HerdrPaneID) == ag.PaneID)
		})
		if i >= 0 && live[i].PRID == pr.ID {
			continue
		}
		who := "an agent in pane " + ag.PaneID
		if i >= 0 {
			who = r.whose(ctx, live[i], who)
		}
		out = append(out, who)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// whose names live session s by its task: "<key> <role>" for a task's
// row, "<key>'s liaison" for a liaison's, else fallback.
func (r *Runtime) whose(ctx context.Context, s store.Session, fallback string) string {
	pr, err := r.st.PRByID(ctx, s.PRID)
	if err != nil {
		return fallback
	}
	if id, ok := OwnerTask(pr.NodeID); ok {
		return r.keyOf(ctx, id) + " " + s.Role
	}
	if id, ok := LiaisonTask(pr.NodeID); ok {
		return r.keyOf(ctx, id) + "'s liaison"
	}
	return fallback
}

// keyOf is task id's key, as the runtime knows it or the registry has it.
func (r *Runtime) keyOf(ctx context.Context, id int64) string {
	r.mu.Lock()
	key := r.keys[id]
	r.mu.Unlock()
	if key != "" {
		return key
	}
	if t, err := r.reg.TaskByID(ctx, id); err == nil {
		return taskKey(t)
	}
	return "task " + fmt.Sprint(id)
}

// event records one of the heal's steps on task id (kind herdr, redacted);
// a failed write is logged.
func (r *Runtime) event(ctx context.Context, id int64, phase, text string) {
	if err := r.reg.AppendEvent(context.WithoutCancel(ctx), id, "herdr", phase, execx.Redact(text)); err != nil &&
		!errors.Is(err, registry.ErrNotFound) {
		r.log.Warn("record a herdr heal's event", "task", id, "err", err)
	}
}
