package runtime

// The heal of a factory herdr server that macOS denies the copies' volume
// (2026-11-10: a server that had run for days lost its access to the
// external volume the copies are on, and every agent it started exited at
// once, its shell's getcwd and the CLI saying EPERM, while the daemon and
// any process started from your terminal read the volume fine). A launch
// that fails so (the agent never takes its first prompt: herdr refuses the
// prompt, or the pane is lost before it) and whose pane printed the refusal
// since the launch (deniedLine, sinceLaunch), while the daemon reads the
// copy (seams.readable), is that server's: the factory stops its own herdr
// session's server once (herdr's server.stop) while nothing else lives in
// the session and every hold a launch passes allows it, so the engine's
// reveal starts a fresh one from your terminal, and relaunches the task's
// roles that took no prompt yet there, once. One stop serves every task
// within stopWindow. The same refusal after that is yours (DeniedError,
// EventVolumeDenied), with one line that says what to allow; your retry
// starts the heal over. A refusal the daemon shares is the copy's or its
// volume's: no herdr server is stopped, and the recoveries' steps go on as
// for any lost pane.

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
	"sync"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// The heal's bounds: stopWindow is how long one stop of the factory's herdr
// server serves every task (a fresh server denied too is yours, not
// stopped again, unless you retried the task since); holdLimit how long
// anything else in the session (another task's agent, a pane of yours,
// this task's own liaison) may hold the stop before the task goes to you,
// with a line that names it;
// readableWait how long the daemon's read of a copy may take (a read macOS
// holds for a permission dialog counts as one that failed).
const (
	stopWindow   = time.Hour
	holdLimit    = 30 * time.Minute
	readableWait = 5 * time.Second
)

// deniedRe matches a line a pane printed once its shell and its CLI could
// not read the directory they start in because macOS refused it (EPERM), on
// their stable words, from column 0: the shell's getcwd, Claude Code's own
// check, Codex's error, whole. What a TUI renders of a tool's output is
// indented or boxed and never matches, and "Operation not permitted" alone
// is any refused call's.
var deniedRe = regexp.MustCompile(`^(?:shell-init: .*getcwd: .*Operation not permitted|Can't read the current directory \(EPERM\)|` +
	`Error: Operation not permitted \(os error 1\)$)`)

// deniedLine is the last line of screen that shows such a refusal, trimmed,
// redacted and clipped ("" for none).
func deniedLine(screen string) string {
	last := ""
	for line := range strings.SplitSeq(strings.ReplaceAll(screen, "\r", ""), "\n") {
		if line = strings.TrimRight(line, " \t"); deniedRe.MatchString(line) {
			last = line
		}
	}
	return textx.Clip(execx.Redact(last), 200)
}

// paneMark is what a pane showed before a launch: its blank last lines
// left out (a read that ends with a newline, or padded with blank rows),
// then its last line (the shell's prompt, which the launch's command
// extends); "" for a pane that showed nothing.
func paneMark(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	trim := func() {
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
	}
	trim()
	if len(lines) > 0 {
		lines = lines[:len(lines)-1]
	}
	trim()
	return strings.Join(lines, "\n")
}

// sinceLaunch is what screen shows that the launch printed: a pane's recent
// lines begin inside what it showed before (mark, paneMark), so the longest
// run of screen's first lines that ends mark is cut. A screen that begins
// with none of mark is the launch's whole (the old text scrolled off). The
// same lines printed again by the launch stay the launch's.
func sinceLaunch(screen, mark string) string {
	screen = strings.ReplaceAll(screen, "\r", "")
	if mark == "" {
		return screen
	}
	old, now := strings.Split(mark, "\n"), strings.Split(screen, "\n")
	for m := min(len(old), len(now)); m > 0; m-- {
		if slices.Equal(now[:m], old[len(old)-m:]) {
			return strings.Join(now[m:], "\n")
		}
	}
	return screen
}

// markPane notes what pane shows before an agent starts in it (paneMark),
// so a refusal is read only in what the launch printed: never in a shell's
// earlier output nor in the scrollback herdr restores. A read that fails
// leaves the pane with no mark: that launch says nothing of a refusal, and
// the recoveries take it.
func (r *Runtime) markPane(ctx context.Context, pane string) {
	if pane == "" {
		return
	}
	res, err := r.d.Herdr.PaneRead(ctx, pane, herdr.ReadOptions{Source: herdr.SourceRecentUnwrapped, Lines: agents.HealthLines})
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		delete(r.marks, pane)
		return
	}
	r.marks[pane] = paneMark(res.Text)
}

// launchMark is the mark of pane's last launch, and whether this runtime
// launched in it.
func (r *Runtime) launchMark(pane string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	mark, ok := r.marks[pane]
	return mark, ok
}

// DeniedLine is what a task waits on you for once macOS still blocks the
// factory's herdr server from reading volume after its one heal: whom to
// allow (herdr, magnum and app, the [terminal] app the engine's reveal
// starts the server from, when it is one) and where, as macOS names the
// setting: Removable Volumes under Files & Folders for a mounted volume,
// Full Disk Access where that toggle is absent; then your retry.
func DeniedLine(volume, app string) string {
	who := "herdr and magnum"
	switch app {
	case "", "custom", "generic":
	default:
		who += " (and " + app + ", which starts the server)"
	}
	return "macOS blocks the factory's herdr server from reading " + volume + ": allow " + who + " in " + setting(volume) +
		" (or Full Disk Access, if that toggle is absent), then retry"
}

// setting is the setting of macOS that allows reading volume, as macOS names
// it: Removable Volumes under Files & Folders for a mounted volume.
func setting(volume string) string {
	where := "System Settings > Privacy & Security > Files & Folders"
	if strings.HasPrefix(volume, "/Volumes/") {
		where += " > Removable Volumes"
	}
	return where
}

// ownLiaison names task t's own liaison among what holds the stop.
const ownLiaison = "this task's liaison"

// heldLine is what a task waits on you for once held (what lives in the
// factory's herdr session: another task's agent, a pane of yours, this
// task's own liaison) kept the restart of the server that cannot read
// volume waiting holdLimit: the setting at stake and what held it, which
// you end before your retry.
func heldLine(volume, held string) string {
	end := "close or end it"
	if held == ownLiaison {
		end = "exit the liaison"
	}
	return fmt.Sprintf("macOS blocks the factory's herdr server from reading %s (%s), and its restart waited %d minutes for %s: "+
		"%s, then retry, and the factory restarts the server", volume, setting(volume), int(holdLimit/time.Minute), held, end)
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
// heal it (its herdr session is yours, its herdr has no server.stop), or
// once Held (what lives in its session) held the stop for holdLimit: the
// task needs you until your retry, with DeniedLine (App is the [terminal]
// app), or with heldLine once something held it.
type DeniedError struct{ Volume, App, Held string }

// Error is the line the task waits on you for: heldLine when Held is set,
// else DeniedLine.
func (e *DeniedError) Error() string {
	if e.Held != "" {
		return heldLine(e.Volume, e.Held)
	}
	return DeniedLine(e.Volume, e.App)
}

// serverStopper stops the herdr session's server (herdr.Client.ServerStop).
type serverStopper interface {
	ServerStop(ctx context.Context) error
}

var _ serverStopper = (*herdr.Client)(nil)

// dirProber reads directories for the daemon (readable), one read of a
// directory in flight at a time: a read macOS holds (a permission dialog)
// is waited for once, wait at most, and a check while it still blocks
// answers no at once, starting no second read, so no goroutine and no
// thread piles up behind it.
type dirProber struct {
	open func(dir string) error
	wait time.Duration

	mu       sync.Mutex
	inFlight map[string]*probe
}

// probe is one read in flight: done closes once err is set.
type probe struct {
	done chan struct{}
	err  error
}

func newDirProber(open func(dir string) error, wait time.Duration) *dirProber {
	return &dirProber{open: open, wait: wait, inFlight: map[string]*probe{}}
}

// readable reports whether the daemon reads dir (open's answer within
// wait; false while a read of dir that macOS holds is in flight).
func (p *dirProber) readable(dir string) bool {
	p.mu.Lock()
	pr, busy := p.inFlight[dir]
	if !busy {
		pr = &probe{done: make(chan struct{})}
		p.inFlight[dir] = pr
		go func() {
			pr.err = p.open(dir)
			close(pr.done)
			p.mu.Lock()
			delete(p.inFlight, dir)
			p.mu.Unlock()
		}()
	}
	p.mu.Unlock()
	if busy {
		select {
		case <-pr.done:
			return pr.err == nil
		default:
			return false
		}
	}
	t := time.NewTimer(p.wait)
	defer t.Stop()
	select {
	case <-pr.done:
		return pr.err == nil
	case <-t.C:
		return false
	}
}

// openDir opens dir and lists one name (an empty directory reads).
func openDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// mayStart reports whether role may be started now outside a launch (a role
// brought back, a heal's relaunch, the heal's stop before it), as a launch
// would be: not while the daemon drains (ErrDraining), nor under a hold of
// the engine's (Deps.MayStart: a disk or memory hold, a drain for an
// install; a *WaitError in its words), its kind's pause, the Mac's critical
// memory or Codex's budget (gate), nor with its CLI logged out (preflight).
// stagger reports the memory at warn, when starts take turns Stagger apart.
func (r *Runtime) mayStart(ctx context.Context, role config.Role) (stagger bool, err error) {
	if err := r.mayHold(); err != nil {
		return false, err
	}
	level, err := r.gate(ctx, role)
	if err != nil {
		return false, err
	}
	if err := r.preflight(ctx, role); err != nil {
		return false, err
	}
	return level == engine.MemoryWarn, nil
}

// mayHold is mayStart's cheap part, which reads no CLI and no budget: the
// daemon's drain (ErrDraining) and the engine's holds (Deps.MayStart).
func (r *Runtime) mayHold() error {
	select {
	case <-r.drained:
		return ErrDraining
	default:
	}
	if r.d.MayStart != nil {
		if who, what, ok := r.d.MayStart(); !ok {
			return &WaitError{Who: who, What: what}
		}
	}
	return nil
}

// heal is a task's heal of the factory's herdr server in one attempt: the
// volume its server cannot read, the refusal's line, the role that showed
// it first and its pane; when the server was stopped for it (by its own stop
// or another task's within stopWindow; zero: not yet), the roles that took
// no prompt yet and are due their relaunch on the fresh server, those it
// relaunched, whether the task's rows were reconciled with that server
// (agents.Manager.Recover), the wait it last recorded, since when
// something else in the session holds the stop and what held it once that
// went to you, and when the line went to you (zero: not yet).
type heal struct {
	attempt, volume, line, role, pane string
	stopped                           time.Time
	relaunch, relaunched              map[string]bool
	reconciled                        bool
	waits                             string
	heldSince                         time.Time
	held                              string
	told                              time.Time
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

// healing is task t's heal in attempt when it holds role: due its relaunch,
// or told to you and not retried since (a heal you retried after is
// dropped); else nil.
func (r *Runtime) healing(ctx context.Context, taskID int64, attempt, role string) *heal {
	r.mu.Lock()
	h := r.heals[taskID]
	if h == nil || h.attempt != attempt {
		r.mu.Unlock()
		return nil
	}
	told, due := h.told, h.relaunch[role]
	r.mu.Unlock()
	if !told.IsZero() {
		if r.retriedSince(ctx, taskID, told) {
			r.mu.Lock()
			if r.heals[taskID] == h {
				delete(r.heals, taskID)
			}
			r.mu.Unlock()
			return nil
		}
		return h
	}
	if !due {
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

// recentStop is the factory's herdr server's last stop by a heal, when it
// serves task id: within stopWindow, and you did not retry the task since
// (zero otherwise).
func (r *Runtime) recentStop(ctx context.Context, id int64) time.Time {
	r.mu.Lock()
	last := r.lastStop
	r.mu.Unlock()
	if last.IsZero() || r.now().Sub(last) >= stopWindow || r.retriedSince(ctx, id, last) {
		return time.Time{}
	}
	return last
}

// launchDenied classifies a launch of role st in attempt a of task t that
// failed in session s before the role's first prompt: what its pane printed
// since the launch (sinceLaunch; a pane this runtime did not launch in says
// nothing) shows macOS refusing the copy's directory (deniedLine) while the
// daemon reads it. It reports whether the heal takes it, with what the
// caller answers then: a wait (*WaitError, ErrDraining), you in the pane, or
// the line for you (*DeniedError). A refusal the daemon shares is not taken
// (an event says so once): the copy's or its volume's, no herdr server's.
func (r *Runtime) launchDenied(ctx context.Context, t registry.Task, a registry.Attempt, st *roleState, s store.Session) (bool, error) {
	mark, launched := r.launchMark(store.Deref(s.HerdrPaneID))
	if !launched {
		return false, nil
	}
	line := ""
	if screen, err := r.mgr.ReadRecent(ctx, s, agents.HealthLines); err == nil { // a pane that cannot be read shows no refusal
		line = deniedLine(sinceLaunch(screen, mark))
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
// after a stop that serves the task (recentStop: one stop serves every
// task); a wait for the relaunch while a stop went through (the task's own,
// or another task's it adopts); else the stop (stopFor). The heal holds the
// role from here: no recovery takes it (healing).
func (r *Runtime) denied(ctx context.Context, t registry.Task, a registry.Attempt, st *roleState, s store.Session, volume, line string) error {
	h := r.healOf(ctx, t, a)
	recent := r.recentStop(ctx, t.ID)
	name := st.role.Name
	r.mu.Lock()
	if h.volume == "" {
		h.volume, h.line, h.role, h.pane = volume, line, name, store.Deref(s.HerdrPaneID)
	}
	switch {
	case !h.told.IsZero():
		r.mu.Unlock()
		return r.deniedErr(h)
	case h.relaunched[name] || (!recent.IsZero() && s.StartedAt.After(recent)): // the fresh server is denied too
		now := r.now()
		h.told = now
		delete(h.relaunch, name)
		st.lost, st.ranOut, st.recovering, st.pending = true, now, false, false
		volume := h.volume
		r.mu.Unlock()
		r.log.Warn("the herdr server the heal started cannot read the copies' volume either: the task is yours", "task", t.ID,
			"role", name, "volume", volume)
		r.event(ctx, t.ID, registry.PhaseFail, name+"'s agent exited at its start on the fresh herdr server too ("+line+"): "+
			DeniedLine(volume, r.d.Config.Terminal.App))
		return r.deniedErr(h)
	}
	h.relaunch[name] = true
	st.recovering, st.pending = false, false
	adopt := h.stopped.IsZero() && !recent.IsZero()
	if adopt {
		h.stopped = recent
	}
	stopped := !h.stopped.IsZero()
	r.mu.Unlock()
	if adopt {
		r.event(ctx, t.ID, registry.PhaseInfo, name+"'s agent exited at its start ("+line+") on the herdr server the factory stopped "+
			"at "+r.clock(recent)+" for another task: no second stop; it is relaunched on the fresh server")
	}
	if stopped { // a launch from before the stop, read after it
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
	return &DeniedError{Volume: h.volume, App: r.d.Config.Terminal.App, Held: h.held}
}

// tell sends heal h of task t (role st's refusal) to you now, held being
// what held its stop for holdLimit ("" for none): the role takes no
// recovery, and an event says why.
func (r *Runtime) tell(ctx context.Context, t registry.Task, st *roleState, h *heal, why, held string) *DeniedError {
	now := r.now()
	r.mu.Lock()
	h.told, h.held = now, held
	st.lost, st.ranOut, st.recovering, st.pending = true, now, false, false
	r.mu.Unlock()
	d := r.deniedErr(h)
	r.event(ctx, t.ID, registry.PhaseFail, why+": "+d.Error())
	return d
}

// stopFor stops the factory's herdr server for heal h of task t (role st's
// refusal), once every guard allows it:
//   - never a session that is yours or magnum's review panes' (ownSession),
//     nor with a herdr that has no server.stop (before 0.9.5): the line goes
//     to you;
//   - not while you are in the role's pane, its kind pauses, the daemon
//     drains or a hold of the engine's stops a launch (mayRestart,
//     mayHold), nor, checked once nothing else holds it, right before the
//     stop, under critical memory, Codex's budget or a logged-out CLI
//     (mayStart): the relaunch would wait anyway;
//   - not while a talk starts a liaison (talkMu), which would die unseen;
//   - not when the refused pane is gone from the session: somebody
//     restarted the server meanwhile, which is the fresh server the role is
//     relaunched on, unstopped;
//   - not while an agent of another task, a liaison of another task, or a
//     pane of yours that runs something lives in the session (othersLive):
//     the wait names them, and the next prompt tries again; this task's own
//     liaison holds it with words of its own. Whatever holds it does so for
//     holdLimit at most: then the task goes to you with heldLine, which
//     names it.
//
// The stop's begin, end or failure are events; once it went through,
// every role of the attempt that took no prompt yet is due its relaunch,
// the stop serves every task for stopWindow, and EventHerdrStopped has the
// engine open the session again at once.
func (r *Runtime) stopFor(ctx context.Context, t registry.Task, a registry.Attempt, st *roleState, h *heal) error {
	r.mu.Lock()
	volume, line, role, pane := h.volume, h.line, h.role, h.pane
	r.mu.Unlock()
	stopper, ok := r.d.Herdr.(serverStopper)
	if !ok || !ownSession(r.d.Config) {
		return r.tell(ctx, t, st, h, st.role.Name+"'s agent exited at its start ("+line+"); the factory's herdr session is not its own, "+
			"so it never stops its server", "")
	}
	if err := r.mayRestart(ctx, t, st); err != nil {
		return err
	}
	if err := r.mayHold(); err != nil {
		return err
	}
	if !r.talkMu.TryLock() {
		return &WaitError{Who: pauseAgent, What: "the factory's herdr server cannot read " + volume + " (macOS blocks it): the factory " +
			"restarts it once the talk that starts a liaison there is over"}
	}
	defer r.talkMu.Unlock()
	pr, err := r.owner(ctx, t, a.ID)
	if err != nil {
		return err
	}
	snap, err := r.d.Herdr.Snapshot(ctx)
	if err != nil {
		return r.restartWait(h) // the server is down: whoever stopped it, the engine opens it again
	}
	if pane != "" && !slices.ContainsFunc(snap.Panes, func(p herdr.Pane) bool { return p.ID == pane }) {
		r.mu.Lock()
		h.stopped = r.now()
		r.mu.Unlock()
		r.event(ctx, t.ID, registry.PhaseInfo, "the refused pane "+pane+" is gone from the factory's herdr session (its server was "+
			"restarted meanwhile): no stop; "+role+" is relaunched on it")
		return r.restartWait(h)
	}
	others, liaison, err := r.othersLive(ctx, t, pr, snap)
	if err != nil {
		return &WaitError{Who: pauseAgent, What: "the factory's herdr server cannot read " + volume + " (macOS blocks it), and the " +
			"factory cannot tell what else lives in it: " + textx.Clip(textx.FirstLine(execx.Redact(err.Error())), 200)}
	}
	if len(others) > 0 || liaison {
		holders := others
		what := "the factory's herdr server cannot read " + volume + " (macOS blocks it): the factory restarts it once nothing " +
			"else lives in it (" + strings.Join(others, ", ") + ")"
		if len(others) == 0 {
			holders = []string{ownLiaison}
			what = "the factory's herdr server cannot read " + volume + " (macOS blocks it), and this task's liaison lives in its " +
				"session: exit the liaison, and the factory restarts the server"
		}
		now := r.now()
		r.mu.Lock()
		if h.heldSince.IsZero() {
			h.heldSince = now
		}
		since, said := h.heldSince, h.waits == what
		h.waits = what
		r.mu.Unlock()
		if now.Sub(since) >= holdLimit {
			held := strings.Join(holders, ", ")
			return r.tell(ctx, t, st, h, held+" held the restart of the factory's herdr server for "+textx.DurationWords(holdLimit), held)
		}
		if !said {
			r.event(ctx, t.ID, registry.PhaseInfo, what)
		}
		return &WaitError{Who: pauseAgent, What: what}
	}
	r.mu.Lock()
	h.heldSince = time.Time{}
	r.mu.Unlock()
	if _, err := r.mayStart(ctx, st.role); err != nil { // the budget and the CLI's login, right before the stop
		return err
	}
	r.event(ctx, t.ID, registry.PhaseBegin, "stops the factory's herdr server: macOS blocks it from reading "+volume+" ("+
		role+"'s pane: "+line+"), which the factory reads")
	if err := stopper.ServerStop(ctx); err != nil && !herdr.NoServer(err) {
		if herdr.IsCode(err, herdr.CodeUnknownMethod) {
			return r.tell(ctx, t, st, h, "the factory's herdr has no server.stop (herdr 0.9.5 and later have it), so the factory "+
				"cannot restart its server", "")
		}
		why := textx.Clip(textx.FirstLine(execx.Redact(err.Error())), 200)
		r.event(ctx, t.ID, registry.PhaseFail, "the stop of the factory's herdr server failed: "+why)
		return &WaitError{Who: pauseAgent, What: "the factory's herdr server cannot read " + volume + " (macOS blocks it), and its " +
			"stop failed (" + why + "): the factory tries again"}
	}
	entries, lerr := r.reg.LedgerOf(ctx, t.ID) // unread: the refused role alone is due, the others' recoveries take them
	r.mu.Lock()
	h.stopped = r.now()
	r.lastStop = h.stopped
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
// relaunch there, under the holds a launch passes (mayStart), the task's
// rows reconciled with it first; nil when the prompt may go.
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
	stagger, err := r.mayStart(ctx, st.role)
	if err != nil {
		return err
	}
	if s, ok := r.liveSession(ctx, st); !ok || !s.StartedAt.After(stoppedAt) {
		if err := r.relaunchRole(ctx, t, a, st, stagger); err != nil {
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

// othersLive names what a stop of the factory's herdr server would end
// besides owner row pr's own agents (task t's): another task's agent ("PR-12
// fx-repro"), another task's liaison ("PR-12's liaison"), an agent the
// factory did not start ("an agent in pane w3:p1"), and a pane with no
// agent that no row of t records and that runs something (herdr's
// process_info: anything but an idle shell), named by whose it is ("a pane of
// yours, w3:p2, runs ruby bin/rails c", "PR-12's pane w4:p1 runs …").
// liaison reports t's own liaison live in the session: you may be talking
// with it.
func (r *Runtime) othersLive(ctx context.Context, t registry.Task, pr store.PR, snap herdr.Snapshot) (others []string, liaison bool,
	err error) {
	live, err := r.st.LiveSessions(ctx)
	if err != nil {
		return nil, false, err
	}
	own := map[string]bool{}
	rows, err := r.st.SessionsByPR(ctx, pr.ID)
	if err != nil {
		return nil, false, err
	}
	for _, s := range rows {
		own[store.Deref(s.HerdrPaneID)] = true
	}
	mine, lerr := liaisonRowIn(ctx, r.st, t.ID)
	agentIn := map[string]bool{}
	for _, ag := range snap.Agents {
		agentIn[ag.PaneID] = true
		i := slices.IndexFunc(live, func(s store.Session) bool {
			name := store.Deref(s.AgentName)
			return (name != "" && name == ag.Name) || (ag.PaneID != "" && store.Deref(s.HerdrPaneID) == ag.PaneID)
		})
		switch {
		case i >= 0 && live[i].PRID == pr.ID:
			continue
		case i >= 0 && lerr == nil && live[i].PRID == mine.ID:
			liaison = true
			continue
		}
		who := "an agent in pane " + ag.PaneID
		if i >= 0 {
			who = r.whose(ctx, live[i], who)
		}
		others = append(others, who)
	}
	for _, p := range snap.Panes {
		if agentIn[p.ID] || own[p.ID] {
			continue
		}
		pi, err := r.d.Herdr.PaneProcessInfo(ctx, p.ID)
		switch {
		case herdr.IsCode(err, herdr.CodePaneNotFound): // gone, or restored with no process
			continue
		case err != nil:
			others = append(others, "pane "+p.ID+" (herdr cannot say what runs in it)")
			continue
		case herdr.IdleShell(pi):
			continue
		}
		cmd := "a process"
		if len(pi.Foreground) > 0 {
			cmd = textx.Clip(execx.Redact(cmp.Or(pi.Foreground[0].Cmdline, pi.Foreground[0].Name)), 60)
		}
		others = append(others, r.paneWhose(ctx, p.ID)+" runs "+cmd)
	}
	slices.Sort(others)
	return slices.Compact(others), liaison, nil
}

// paneWhose names pane id by the task whose rows record it ("PR-12's pane
// w4:p1"), else as yours ("a pane of yours, w3:p2,").
func (r *Runtime) paneWhose(ctx context.Context, id string) string {
	ids, err := r.st.PRsWithPanes(ctx, []string{id})
	if err != nil {
		return "a pane of yours, " + id + ","
	}
	for _, prID := range ids {
		pr, err := r.st.PRByID(ctx, prID)
		if err != nil {
			continue
		}
		if tid, ok := OwnerTask(pr.NodeID); ok {
			return r.keyOf(ctx, tid) + "'s pane " + id
		}
		if tid, ok := LiaisonTask(pr.NodeID); ok {
			return r.keyOf(ctx, tid) + "'s liaison's pane " + id
		}
	}
	return "a pane of yours, " + id + ","
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
