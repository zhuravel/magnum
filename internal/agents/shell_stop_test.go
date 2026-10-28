package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// The checkout e.workspace lays the panes out in, as the shell line's
// db-lock names it.
const shellCheckout = "/Users/x/Projects/talkable.review1"

// The process ids of a fake codex-review command: the shell of the pane,
// the job's process group (the brace group's subshell leads it), its
// db-lock with the run token, codex under db-lock, the tee of the report,
// a sandbox's child of codex in a process group of its own, and a process
// of the same table under nothing of the job.
const (
	shellPID   = 100
	jobPGID    = 200
	dbLockPID  = 201
	codexPID   = 202
	teePID     = 203
	sandboxPID = 300
	otherPID   = 400
)

// fakeProc is one process of a procWorld: its row, its argv as herdr reads
// it, whether herdr lists it in the pane's foreground, and which signals
// end it (closed: the pane's close does).
type fakeProc struct {
	p                   execx.Process
	argv                []string
	fg                  bool
	term, kill, closing bool
}

// procWorld is the process table and the pane's foreground job of a fake
// codex-review command: the manager's ps reads it (execx.ProcessTable), its
// signals (Deps.Signal) end what they end, and herdr's process info for
// the pane follows: busy while a foreground process of the job's group
// lives, an idle shell after.
type procWorld struct {
	e    *env
	pane string

	mu      sync.Mutex
	procs   []*fakeProc
	psReads int
	psErr   map[int]error             // the ps read that fails
	onPS    func(w *procWorld, n int) // before the n-th ps read answers
}

func (e *env) world(pane string) *procWorld {
	w := &procWorld{e: e, pane: pane, psErr: map[int]error{}}
	e.run.Rules = append(e.run.Rules, execx.Rule{Prefix: []string{"ps"}, Fn: func(execx.Cmd) (execx.Result, error) { return w.ps() }})
	e.sigs.on = w.signal
	e.h.onPaneClose = func(p string) { // f.mu held: the world alone changes
		if p != w.pane {
			return
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		w.procs = slices.DeleteFunc(w.procs, func(fp *fakeProc) bool { return fp.closing })
	}
	return w
}

// job adds the standard job of dbLock's line: the brace group, db-lock and
// codex and tee in group jobPGID, all in the foreground, and the sandbox's
// child in a group of its own under codex; each ends on SIGTERM.
func (w *procWorld) job(dbLock []string) {
	w.add(jobPGID, shellPID, jobPGID, "zsh", []string{"zsh"}, true)
	w.add(dbLockPID, jobPGID, jobPGID, "magnum", dbLock, true)
	w.add(codexPID, dbLockPID, jobPGID, "codex", []string{"codex", "review", "--base", "origin/master"}, true)
	w.add(teePID, shellPID, jobPGID, "tee", []string{"tee", "/state/reviews/codex-review.md"}, true)
	w.add(sandboxPID, codexPID, sandboxPID, "sandbox-exec", []string{"sandbox-exec", "bin/rspec"}, false)
	w.sync()
}

func (w *procWorld) add(pid, ppid, pgid int, name string, argv []string, fg bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.procs = append(w.procs, &fakeProc{p: execx.Process{PID: pid, PPID: ppid, PGID: pgid, Name: name,
		Start: fmt.Sprintf("Wed Oct 28 10:15:%02d 2026", pid%60)}, argv: argv, fg: fg, term: true, kill: true, closing: true})
}

// set changes how the processes matching keep end.
func (w *procWorld) set(keep func(p execx.Process) bool, term, kill, closing bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, fp := range w.procs {
		if keep(fp.p) {
			fp.term, fp.kill, fp.closing = term, kill, closing
		}
	}
}

func inGroup(p execx.Process) bool   { return p.PGID == jobPGID }
func anyProc(execx.Process) bool     { return true }
func isSandbox(p execx.Process) bool { return p.PID == sandboxPID }

func (w *procWorld) ps() (execx.Result, error) {
	w.mu.Lock()
	w.psReads++
	n, hook := w.psReads, w.onPS
	err := w.psErr[n]
	w.mu.Unlock()
	if hook != nil {
		hook(w, n)
	}
	if err != nil {
		return execx.Result{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "%d 1 %d Ss %s /bin/zsh\n", shellPID, shellPID, "Wed Oct 28 09:00:00 2026")
	fmt.Fprintf(&b, "%d 1 %d S %s /Applications/Some App.app/Contents/MacOS/Some App\n", otherPID, otherPID, "Wed Oct 28 09:00:01 2026")
	for _, fp := range w.procs {
		fmt.Fprintf(&b, "%d %d %d S+ %s /usr/bin/%s\n", fp.p.PID, fp.p.PPID, fp.p.PGID, fp.p.Start, fp.p.Name)
	}
	return execx.Result{Stdout: []byte(b.String())}, nil
}

// signal is Deps.Signal in the world: the targets the signal ends are gone.
func (w *procWorld) signal(pid int, sig syscall.Signal) error {
	w.mu.Lock()
	w.procs = slices.DeleteFunc(w.procs, func(fp *fakeProc) bool {
		hit := fp.p.PID == pid || (pid < 0 && fp.p.PGID == -pid)
		return hit && ((sig == syscall.SIGTERM && fp.term) || (sig == syscall.SIGKILL && fp.kill))
	})
	w.mu.Unlock()
	w.sync()
	return nil
}

// sync sets herdr's process info of the pane from the world.
func (w *procWorld) sync() {
	w.mu.Lock()
	var fg []herdr.Process
	for _, fp := range w.procs {
		if fp.fg && fp.p.PGID == jobPGID {
			fg = append(fg, herdr.Process{PID: fp.p.PID, Name: fp.p.Name, Argv: fp.argv})
		}
	}
	w.mu.Unlock()
	if len(fg) == 0 {
		w.e.setProcs(w.pane, idlePane(w.pane))
		return
	}
	w.e.setProcs(w.pane, herdr.ProcessInfo{PaneID: w.pane, ShellPID: shellPID, ForegroundPGID: jobPGID, Foreground: fg})
}

// alive reports whether pid still runs in the world.
func (w *procWorld) alive(pid int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.ContainsFunc(w.procs, func(fp *fakeProc) bool { return fp.p.PID == pid })
}

// shellWorkspace is e.workspace with the calls of its layout (splits,
// renames) forgotten, so a test sees only what came after.
func (e *env) shellWorkspace() {
	e.t.Helper()
	e.workspace()
	e.h.mu.Lock()
	defer e.h.mu.Unlock()
	e.h.calls, e.h.splits, e.h.paneCloses, e.h.runs, e.h.keys = nil, nil, nil, nil, nil
}

// shellRun is a run of codex-review on the PR in state (submitted on the
// pane's session for an in-flight one, as the pipeline leaves a run whose
// line it typed) whose line RunShell typed (launched), and the pane of the
// workspace e.workspace laid out.
func (e *env) shellRun(state string) (store.Run, string) {
	e.t.Helper()
	sess := e.session(RoleCodexReview)
	run, err := e.m.NewRun(e.ctx, e.pr, RoleCodexReview, store.RunInitial, 1)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.TransitionRun(e.ctx, run.ID, []string{store.RunPending}, store.RunSubmitted, func(u *store.RunUpdate) {
		u.Set("session_id", sess.ID)
		u.Set("submitted_at", e.clock.Now())
	}); err != nil {
		e.t.Fatal(err)
	}
	if state != store.RunSubmitted {
		if err := e.st.TransitionRun(e.ctx, run.ID, []string{store.RunSubmitted}, state, nil); err != nil {
			e.t.Fatal(err)
		}
	}
	run, err = e.st.RunByID(e.ctx, run.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	e.m.markLaunched(run.ID)
	return run, store.Deref(sess.HerdrPaneID)
}

// dbLockArgv is the argv of the db-lock a shell line of role starts for
// run in checkout, as herdr reads it from the process.
func (e *env) dbLockArgv(role Role, checkout, run string) []string {
	return []string{e.m.d.Layout.Binary(), "db-lock", "--checkout", checkout, "--role", string(role), "--run", run, "--",
		"codex", "review", "-c", "model_reasoning_effort=high", "--base", "origin/master"}
}

// idlePane is the process info of an idle shell.
func idlePane(pane string) herdr.ProcessInfo {
	return herdr.ProcessInfo{PaneID: pane, ShellPID: shellPID, ForegroundPGID: shellPID}
}

// otherJob is the process info of a pane whose foreground runs a command
// that is no run of magnum's.
func otherJob(pane string) herdr.ProcessInfo {
	return herdr.ProcessInfo{PaneID: pane, ShellPID: shellPID, ForegroundPGID: 500,
		Foreground: []herdr.Process{{PID: 500, Name: "vim", Argv: []string{"vim", "notes.md"}}}}
}

// setProcs sets what herdr answers for pane.
func (e *env) setProcs(pane string, pi herdr.ProcessInfo) {
	e.h.mu.Lock()
	defer e.h.mu.Unlock()
	e.h.procs[pane] = pi
}

// signalsTo are the signals sent, as "<pid>:<TERM|KILL>".
func (e *env) signalsTo() []string {
	var out []string
	for _, s := range e.sigs.all() {
		out = append(out, fmt.Sprintf("%d:%s", s.pid, strings.TrimPrefix(sigName(s.sig), "SIG")))
	}
	return out
}

// eventData decodes an event's data.
func eventData(t *testing.T, ev store.Event) map[string]any {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatalf("%s data %s: %v", ev.Kind, ev.Data, err)
	}
	return d
}

// humanActive reports whether the PR's human cooldown started.
func (e *env) humanActive() bool {
	e.t.Helper()
	pr, err := e.st.PRByID(e.ctx, e.pr.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return pr.HumanActiveAt != nil
}

// A cut codex-review command that its ctrl+c did not end, in its own pane
// and with its run token, gets one SIGTERM to its process group and to
// each process of its job the process table shows as itself, and nothing
// else once the shell is idle and the job gone: no SIGKILL, no close. The
// begin event names the process group and the job (pid, group and name,
// never argv), the stopped one the step.
func TestStopShellTermsTheProcessGroupOfItsOwnRun(t *testing.T) {
	e := newEnv(t)
	e.shellWorkspace()
	run, pane := e.shellRun(store.RunSubmitted)
	w := e.world(pane)
	w.job(e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID))

	if err := e.m.StopShell(e.ctx, run); err != nil {
		t.Fatalf("StopShell = %v", err)
	}
	got := e.signalsTo()
	if want := fmt.Sprintf("-%d:TERM", jobPGID); len(got) == 0 || got[0] != want || slices.Index(got[1:], want) >= 0 {
		t.Errorf("signals = %v, want one SIGTERM to the group first", got)
	}
	if slices.ContainsFunc(got, func(s string) bool { return strings.HasSuffix(s, ":KILL") }) {
		t.Errorf("signals = %v, want no SIGKILL", got)
	}
	if !slices.Contains(got, fmt.Sprintf("%d:TERM", sandboxPID)) {
		t.Errorf("signals = %v, want the sandbox's child too", got)
	}
	if len(e.h.paneCloses) != 0 || len(e.h.splits) != 0 {
		t.Errorf("closes %v, splits %v, want none", e.h.paneCloses, e.h.splits)
	}
	begin := e.eventsOf(EventShellStopBegin)
	if len(begin) != 1 {
		t.Fatalf("begin events = %+v", begin)
	}
	d := eventData(t, begin[0])
	if d["role"] != "codex-review" || d["pane"] != pane || d["run"] != run.ID || d["pgid"] != float64(jobPGID) {
		t.Errorf("begin data = %v", d)
	}
	raw := string(begin[0].Data)
	if !strings.Contains(raw, `"name":"codex"`) || !strings.Contains(raw, fmt.Sprintf(`"pid":%d`, sandboxPID)) ||
		strings.Contains(raw, "--base") || strings.Contains(raw, "origin/master") || strings.Contains(raw, "argv") ||
		strings.Contains(raw, "db-lock") || strings.Contains(raw, "/usr/") || strings.Contains(raw, "Oct 28") {
		t.Errorf("begin data %s, want the job by pid, group and name, never argv", raw)
	}
	stopped := e.eventsOf(EventShellStopped)
	if len(stopped) != 1 || eventData(t, stopped[0])["step"] != "term" {
		t.Errorf("stopped events = %+v, want one with the step term", stopped)
	}
	if f := e.eventsOf(EventShellStopFailed); len(f) != 0 {
		t.Errorf("failed events = %+v", f)
	}
}

// A command that ignores SIGTERM gets SIGKILL the same way, after
// shellStopWait (5 seconds) on the clock.
func TestStopShellKillsWhenTermIsIgnored(t *testing.T) {
	e := newEnv(t)
	e.shellWorkspace()
	run, pane := e.shellRun(store.RunSubmitted)
	w := e.world(pane)
	w.job(e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID))
	w.set(anyProc, false, true, true)

	if err := e.m.StopShell(e.ctx, run); err != nil {
		t.Fatalf("StopShell = %v", err)
	}
	var term, kill time.Time
	for _, s := range e.sigs.all() {
		switch {
		case s.pid == -jobPGID && s.sig == syscall.SIGTERM:
			term = s.at
		case s.pid == -jobPGID && s.sig == syscall.SIGKILL:
			kill = s.at
		}
	}
	if term.IsZero() || kill.IsZero() || kill.Sub(term) != 5*time.Second {
		t.Errorf("signals = %v (SIGTERM at %v, SIGKILL at %v), want SIGKILL 5s after SIGTERM", e.signalsTo(), term, kill)
	}
	if !slices.Contains(e.signalsTo(), fmt.Sprintf("%d:KILL", sandboxPID)) {
		t.Errorf("signals = %v, want SIGKILL to the sandbox's child too", e.signalsTo())
	}
	if stopped := e.eventsOf(EventShellStopped); len(stopped) != 1 || eventData(t, stopped[0])["step"] != "kill" {
		t.Errorf("stopped events = %+v, want the step kill", stopped)
	}
	if len(e.h.paneCloses) != 0 {
		t.Errorf("closes = %v", e.h.paneCloses)
	}
}

// A command whose db-lock carries no run token of this PR's role in this
// checkout (a run from before the token, the operator's own db-lock, another
// program, another role's or checkout's line) is never signalled nor
// closed: ErrNotOwn, and the failed event says not_own in words that carry
// nothing of the command's argv. It is the human's command: the PR's
// cooldown starts.
func TestStopShellNeverSignalsACommandWithoutItsRunToken(t *testing.T) {
	const argvRun = "r-20261028T000000-argv7"
	for name, argv := range map[string]func(e *env) []string{
		"no run token": func(e *env) []string {
			return []string{e.m.d.Layout.Binary(), "db-lock", "--checkout", shellCheckout, "--role", "codex-review", "--", "codex", "review"}
		},
		"no db-lock": func(e *env) []string {
			return []string{"codex", "review", "--run", argvRun}
		},
		"another program": func(e *env) []string {
			return []string{"/usr/local/bin/other", "db-lock", "--checkout", shellCheckout, "--role", "codex-review", "--run", argvRun, "--", "x"}
		},
		"another role": func(e *env) []string {
			return e.dbLockArgv(RoleClaude, shellCheckout, argvRun)
		},
		"another checkout": func(e *env) []string {
			return e.dbLockArgv(RoleCodexReview, "/Users/x/Projects/talkable.review2", argvRun)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.shellWorkspace()
			run, pane := e.shellRun(store.RunSubmitted)
			e.world(pane).job(argv(e))
			if err := e.m.StopShell(e.ctx, run); !errors.Is(err, ErrNotOwn) {
				t.Fatalf("StopShell = %v, want ErrNotOwn", err)
			}
			if got := e.signalsTo(); len(got) != 0 {
				t.Errorf("signals = %v, want none", got)
			}
			if len(e.h.paneCloses) != 0 || len(e.h.splits) != 0 || len(e.keysSent()) != 0 {
				t.Errorf("closes %v, splits %v, keys %v, want none", e.h.paneCloses, e.h.splits, e.keysSent())
			}
			failed := e.eventsOf(EventShellStopFailed)
			if len(failed) != 1 || eventData(t, failed[0])["cause"] != "not_own" {
				t.Fatalf("failed events = %+v, want one with the cause not_own", failed)
			}
			for _, word := range []string{argvRun, "claude-review", "talkable.review2", "/usr/local/bin/other"} {
				if strings.Contains(failed[0].Message, word) || strings.Contains(string(failed[0].Data), word) {
					t.Errorf("the failed event carries %q from the argv: %s %s", word, failed[0].Message, failed[0].Data)
				}
			}
			if b := e.eventsOf(EventShellStopBegin); len(b) != 0 {
				t.Errorf("begin events = %+v", b)
			}
			if !e.humanActive() {
				t.Error("a command magnum does not own in its pane did not start the human cooldown")
			}
		})
	}
}

// The run token must name a run of this PR and role, the cut run or one no
// longer in flight: another PR's run and a run the registry does not know
// are the human's (never signalled, the cooldown starts); another run of
// the role still in flight is magnum's own, never signalled either.
func TestStopShellNeverSignalsAnotherPRsOrAnInFlightRun(t *testing.T) {
	for _, tc := range []struct {
		name    string
		token   func(e *env) string
		foreign bool
	}{
		{"another PR's run", func(e *env) string {
			other, err := e.st.UpsertPRFromGitHub(e.ctx, store.GitHubPR{RepoID: e.repo.ID, NodeID: "PR_11921", Number: 11921,
				URL: "https://github.com/talkable/talkable/pull/11921", HeadSHA: "def5678abc1234def5678abc1234def5678abc12",
				GHState: store.GHOpen, InitialState: store.PRReviewing, Identity: "talkable-app"})
			if err != nil {
				e.t.Fatal(err)
			}
			r, err := e.m.NewRun(e.ctx, other.PR, RoleCodexReview, store.RunInitial, 1)
			if err != nil {
				e.t.Fatal(err)
			}
			return r.ID
		}, true},
		{"an unknown run", func(*env) string { return "r-20261028T000000-99" }, true},
		{"another run in flight", func(e *env) string {
			r, err := e.m.NewRun(e.ctx, e.pr, RoleCodexReview, store.RunInitial, 2)
			if err != nil {
				e.t.Fatal(err)
			}
			return r.ID // pending: in flight
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.shellWorkspace()
			cut, pane := e.shellRun(store.RunSubmitted)
			e.world(pane).job(e.dbLockArgv(RoleCodexReview, shellCheckout, tc.token(e)))
			if err := e.m.StopShell(e.ctx, cut); !errors.Is(err, ErrNotOwn) {
				t.Fatalf("StopShell = %v, want ErrNotOwn", err)
			}
			if got := e.signalsTo(); len(got) != 0 {
				t.Errorf("signals = %v, want none", got)
			}
			if len(e.h.paneCloses) != 0 {
				t.Errorf("closes = %v", e.h.paneCloses)
			}
			if got := e.humanActive(); got != tc.foreign {
				t.Errorf("human cooldown = %t, want %t", got, tc.foreign)
			}
		})
	}
}

// A run whose session is no longer the live shell session of its role (it
// was closed, or it is another role's) is never signalled nor keyed: its
// pane is not the role's pane any more. That is magnum's own state, so no
// cooldown starts.
func TestStopShellNeverTouchesAPaneThatIsNotTheRolesLiveOne(t *testing.T) {
	for name, move := range map[string]func(e *env, run store.Run, pane string) (store.Run, string){
		"its session closed": func(e *env, run store.Run, pane string) (store.Run, string) {
			if err := e.st.TransitionSession(e.ctx, *run.SessionID, nil, store.SessionClosed, nil); err != nil {
				e.t.Fatal(err)
			}
			return run, pane
		},
		"another role's session": func(e *env, run store.Run, _ string) (store.Run, string) {
			claude := e.session(RoleClaude)
			run.SessionID = &claude.ID
			return run, store.Deref(claude.HerdrPaneID)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.shellWorkspace()
			run, pane := e.shellRun(store.RunSubmitted)
			run, pane = move(e, run, pane)
			e.world(pane).job(e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID))
			if err := e.m.StopShell(e.ctx, run); err == nil {
				t.Fatal("StopShell = nil, want a refusal")
			}
			if err := e.m.InterruptShell(e.ctx, run); err == nil {
				t.Fatal("InterruptShell = nil, want a refusal")
			}
			if got := e.signalsTo(); len(got) != 0 || len(e.keysSent()) != 0 || len(e.h.paneCloses) != 0 {
				t.Errorf("signals %v, keys %v, closes %v: want none", got, e.keysSent(), e.h.paneCloses)
			}
			if e.humanActive() {
				t.Error("magnum's own state started the human cooldown")
			}
		})
	}
}

// herdr is asked again right before each signal: a process group or a
// token process that changed since the last look (the command ended and
// something else took the foreground) gets nothing more, and it is the
// human's.
func TestStopShellRechecksTheProcessGroupBeforeEachSignal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changed int      // the read of the pane's process info that shows something else
		want    []string // the signals to the group
	}{
		{"before the SIGTERM", 2, nil},
		{"before the SIGKILL", 3, []string{fmt.Sprintf("-%d:TERM", jobPGID)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.shellWorkspace()
			run, pane := e.shellRun(store.RunSubmitted)
			w := e.world(pane)
			w.job(e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID))
			w.set(anyProc, false, true, true)
			e.h.onProcInfo = func(f *fakeHerdr, p string, n int) {
				if n == tc.changed {
					f.procs[p] = otherJob(p)
				}
			}
			if err := e.m.StopShell(e.ctx, run); !errors.Is(err, ErrNotOwn) {
				t.Fatalf("StopShell = %v, want ErrNotOwn", err)
			}
			var group []string
			for _, s := range e.signalsTo() {
				if strings.HasPrefix(s, "-") {
					group = append(group, s)
				}
				if strings.HasPrefix(s, "-500:") || strings.HasPrefix(s, "500:") {
					t.Errorf("signal %s to what took the foreground", s)
				}
			}
			if !slices.Equal(group, tc.want) {
				t.Errorf("signals to a group = %v, want %v", group, tc.want)
			}
			if len(e.h.paneCloses) != 0 {
				t.Errorf("closes = %v", e.h.paneCloses)
			}
			if !e.humanActive() {
				t.Error("another job in the pane did not start the human cooldown")
			}
		})
	}
}

// The process table is read before the look at herdr that allows a signal:
// a job that changes while ps runs gets nothing.
func TestStopShellSendsNothingWhenTheJobChangesDuringThePSRead(t *testing.T) {
	e := newEnv(t)
	e.shellWorkspace()
	run, pane := e.shellRun(store.RunSubmitted)
	w := e.world(pane)
	w.job(e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID))
	w.onPS = func(w *procWorld, n int) {
		if n == 2 { // the step's read, after the capture's
			e.setProcs(pane, otherJob(pane))
		}
	}
	if err := e.m.StopShell(e.ctx, run); !errors.Is(err, ErrNotOwn) {
		t.Fatalf("StopShell = %v, want ErrNotOwn", err)
	}
	if got := e.signalsTo(); len(got) != 0 {
		t.Errorf("signals = %v, want none", got)
	}
}

// codex's sandbox may run its child in a process group (and session) of
// its own, which the group's signal misses: every process under the token's
// db-lock in the process table gets the signal too, and nothing outside
// that tree, nor the pane's shell.
func TestStopShellKillsDescendantsThatLeftTheGroup(t *testing.T) {
	e := newEnv(t)
	e.shellWorkspace()
	run, pane := e.shellRun(store.RunSubmitted)
	e.world(pane).job(e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID))

	if err := e.m.StopShell(e.ctx, run); err != nil {
		t.Fatalf("StopShell = %v", err)
	}
	got := e.signalsTo()
	if !slices.Contains(got, fmt.Sprintf("%d:TERM", sandboxPID)) {
		t.Errorf("signals = %v, want %d:TERM", got, sandboxPID)
	}
	for _, s := range got {
		pid, _, _ := strings.Cut(strings.TrimPrefix(s, "-"), ":")
		if pid == fmt.Sprint(otherPID) || pid == fmt.Sprint(shellPID) || pid == "1" {
			t.Errorf("signal %s outside the run's job", s)
		}
	}
}

// The parent ends on SIGTERM and the pane is an idle shell, but a captured
// child that ignores SIGTERM (a spec run cleaning up, in its own session)
// still runs in the checkout: no success yet. It gets SIGKILL by its own
// identity once the grace is over, and the stop succeeds only when it is
// gone; one that outlives SIGKILL too fails the stop, so the restart keeps
// the checkout.
func TestStopShellKillsADescendantThatOutlivesSIGTERMAfterItsParentExits(t *testing.T) {
	for _, tc := range []struct {
		name     string
		killable bool
	}{{"gone on SIGKILL", true}, {"outlives SIGKILL", false}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.shellWorkspace()
			run, pane := e.shellRun(store.RunSubmitted)
			w := e.world(pane)
			w.job(e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID))
			w.set(isSandbox, false, tc.killable, true)

			err := e.m.StopShell(e.ctx, run)
			if !slices.Contains(e.signalsTo(), fmt.Sprintf("%d:KILL", sandboxPID)) {
				t.Errorf("signals = %v, want SIGKILL to the child that outlived SIGTERM", e.signalsTo())
			}
			if slices.Contains(e.signalsTo(), fmt.Sprintf("-%d:KILL", jobPGID)) {
				t.Errorf("signals = %v, want no SIGKILL to a group that is gone", e.signalsTo())
			}
			if tc.killable {
				if err != nil {
					t.Fatalf("StopShell = %v", err)
				}
				if stopped := e.eventsOf(EventShellStopped); len(stopped) != 1 || eventData(t, stopped[0])["step"] != "kill" {
					t.Errorf("stopped events = %+v, want the step kill", stopped)
				}
				return
			}
			if err == nil || !w.alive(sandboxPID) {
				t.Fatalf("StopShell = %v with the child alive %t, want a failure while it runs", err, w.alive(sandboxPID))
			}
			if f := e.eventsOf(EventShellStopFailed); len(f) != 1 || eventData(t, f[0])["cause"] != "signal" {
				t.Errorf("failed events = %+v, want the cause signal", f)
			}
		})
	}
}

// A process the stop captured that ends during the grace may leave its pid
// to another process: a later start, so not the one captured, and it gets
// nothing.
func TestStopShellNeverSignalsAProcessThatTookACapturedPID(t *testing.T) {
	e := newEnv(t)
	e.shellWorkspace()
	run, pane := e.shellRun(store.RunSubmitted)
	w := e.world(pane)
	w.job(e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID))
	w.set(inGroup, false, true, true) // the job outlives SIGTERM: a SIGKILL step follows
	prev := e.sigs.on
	e.sigs.on = func(pid int, sig syscall.Signal) error {
		err := prev(pid, sig)
		if pid == sandboxPID && sig == syscall.SIGTERM {
			// It ended; a later process of the operator's got its pid.
			w.mu.Lock()
			w.procs = append(w.procs, &fakeProc{p: execx.Process{PID: sandboxPID, PPID: 1, PGID: 777, Name: "sandbox-exec",
				Start: "Wed Oct 28 10:16:30 2026"}, term: true, kill: true})
			w.mu.Unlock()
		}
		return err
	}
	if err := e.m.StopShell(e.ctx, run); err != nil {
		t.Fatalf("StopShell = %v", err)
	}
	if slices.Contains(e.signalsTo(), fmt.Sprintf("%d:KILL", sandboxPID)) {
		t.Errorf("signals = %v: SIGKILL went to the process that took a captured pid", e.signalsTo())
	}
	if !w.alive(sandboxPID) {
		t.Error("the other process with the reused pid is gone")
	}
}

// Without a process table there is no proof of the job's processes: the
// stop sends nothing more (no fallback to an older look) and fails.
func TestStopShellSendsNothingWithoutAProcessTable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		failAt int
		want   []string
	}{
		{"at the capture", 1, nil},
		{"after the SIGTERM", 3, []string{fmt.Sprintf("-%d:TERM", jobPGID)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.shellWorkspace()
			run, pane := e.shellRun(store.RunSubmitted)
			w := e.world(pane)
			w.job(e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID))
			w.set(anyProc, false, true, true)
			for n := tc.failAt; n < tc.failAt+30; n++ {
				w.psErr[n] = errors.New("ps: operation not permitted")
			}
			if err := e.m.StopShell(e.ctx, run); err == nil {
				t.Fatal("StopShell = nil, want a failure")
			}
			var group []string
			for _, s := range e.signalsTo() {
				if strings.HasPrefix(s, "-") {
					group = append(group, s)
				}
			}
			if !slices.Equal(group, tc.want) || slices.ContainsFunc(e.signalsTo(), func(s string) bool { return strings.HasSuffix(s, ":KILL") }) {
				t.Errorf("signals = %v, want only %v", e.signalsTo(), tc.want)
			}
			if f := e.eventsOf(EventShellStopFailed); len(f) != 1 || eventData(t, f[0])["cause"] != "signal" {
				t.Errorf("failed events = %+v, want the cause signal", f)
			}
		})
	}
}

// A pane that SIGKILL left busy is replaced: a new pane split with the
// recorded working directory and environment, the session pointed at it
// before the old pane closes (an observe tick in between finds the session
// live in a pane herdr has), and the old pane closed. The stop succeeds
// once the job is gone after the close.
func TestStopShellReplacesThePaneWhenKillLeavesItBusy(t *testing.T) {
	e := newEnv(t)
	e.shellWorkspace()
	run, pane := e.shellRun(store.RunSubmitted)
	w := e.world(pane)
	w.job(e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID))
	w.set(anyProc, false, false, true) // in uninterruptible sleep until the pane closes
	before := e.session(RoleCodexReview)
	var atClose string
	prevClose := e.h.onPaneClose
	e.h.onPaneClose = func(p string) {
		if s, err := e.st.SessionByID(e.ctx, before.ID); err == nil && p == pane {
			atClose = store.Deref(s.HerdrPaneID)
		}
		prevClose(p)
	}

	if err := e.m.StopShell(e.ctx, run); err != nil {
		t.Fatalf("StopShell = %v", err)
	}
	if len(e.h.splits) != 1 || e.h.splits[0].From != pane || e.h.splits[0].Opts.Cwd != shellCheckout ||
		e.h.splits[0].Opts.Env["WT_BRANCH"] != "review1" {
		t.Fatalf("splits = %+v, want one from %s with the session's cwd and env", e.h.splits, pane)
	}
	repl := e.h.splits[0].New
	if !slices.Equal(e.h.paneCloses, []string{pane}) {
		t.Errorf("closes = %v, want the old pane", e.h.paneCloses)
	}
	if atClose != repl {
		t.Errorf("the session was in pane %q at the close, want the replacement %s", atClose, repl)
	}
	if s := e.session(RoleCodexReview); s.ID != before.ID || store.Deref(s.HerdrPaneID) != repl {
		t.Errorf("session = %d in %s, want %d in %s", s.ID, store.Deref(s.HerdrPaneID), before.ID, repl)
	}
	if stopped := e.eventsOf(EventShellStopped); len(stopped) != 1 || eventData(t, stopped[0])["step"] != "replace" ||
		eventData(t, stopped[0])["replacement"] != repl {
		t.Errorf("stopped events = %+v, want the step replace", stopped)
	}
}

// Without a recorded environment there is no replacement (a pane without
// the role's identity env would act as the wrong account): the busy pane
// closes all the same, and the session is lost, for the next round to lay
// a pane out.
func TestStopShellClosesThePaneWithoutAnEnvironmentAndLosesTheSession(t *testing.T) {
	e := newEnv(t)
	e.shellWorkspace()
	for _, s := range e.sessions() {
		if err := e.st.UpdateSession(e.ctx, s.ID, func(u *store.SessionUpdate) { u.Set("env_json", map[string]string{}) }); err != nil {
			t.Fatal(err)
		}
	}
	run, pane := e.shellRun(store.RunSubmitted)
	w := e.world(pane)
	w.job(e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID))
	w.set(anyProc, false, false, true)

	if err := e.m.StopShell(e.ctx, run); err != nil {
		t.Fatalf("StopShell = %v", err)
	}
	if len(e.h.splits) != 0 || !slices.Equal(e.h.paneCloses, []string{pane}) {
		t.Errorf("splits %v, closes %v: want the pane closed with no replacement", e.h.splits, e.h.paneCloses)
	}
	s, err := e.st.SessionByID(e.ctx, *run.SessionID)
	if err != nil || s.State != store.SessionLost {
		t.Errorf("session = %+v, %v, want lost", s, err)
	}
	if stopped := e.eventsOf(EventShellStopped); len(stopped) != 1 || eventData(t, stopped[0])["step"] != "close" {
		t.Errorf("stopped events = %+v, want the step close", stopped)
	}
}

// herdr is asked again right before the old pane's close, after the split:
// a pane whose job changed during the split (it runs another job now) is
// left alone, the session kept on it and the unused replacement closed (not
// own); one that is an idle shell by then keeps the role too, and the stop
// is done.
func TestStopShellRechecksThePaneBeforeItsClose(t *testing.T) {
	for _, tc := range []struct {
		name string
		then func(e *env, w *procWorld, pane string)
		ok   bool
	}{
		{"another job", func(e *env, w *procWorld, pane string) {
			w.mu.Lock()
			w.procs = nil
			w.mu.Unlock()
			e.h.procs[pane] = otherJob(pane)
		}, false},
		{"an idle shell", func(e *env, w *procWorld, pane string) {
			w.mu.Lock()
			w.procs = nil
			w.mu.Unlock()
			e.h.procs[pane] = idlePane(pane)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.shellWorkspace()
			run, pane := e.shellRun(store.RunSubmitted)
			w := e.world(pane)
			w.job(e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID))
			w.set(anyProc, false, false, true)
			e.h.onSplit = func(f *fakeHerdr, from, _ string) {
				if from == pane {
					tc.then(e, w, from)
				}
			}
			err := e.m.StopShell(e.ctx, run)
			if (err == nil) != tc.ok {
				t.Fatalf("StopShell = %v, want ok %t", err, tc.ok)
			}
			if len(e.h.splits) != 1 {
				t.Fatalf("splits = %+v", e.h.splits)
			}
			repl := e.h.splits[0].New
			if !slices.Equal(e.h.paneCloses, []string{repl}) {
				t.Errorf("closes = %v, want only the unused replacement %s", e.h.paneCloses, repl)
			}
			if s := e.session(RoleCodexReview); store.Deref(s.HerdrPaneID) != pane {
				t.Errorf("session in %s, want it kept in %s", store.Deref(s.HerdrPaneID), pane)
			}
			if !tc.ok && !e.humanActive() {
				t.Error("another job in the pane did not start the human cooldown")
			}
		})
	}
}

// codex-review's pane still runs the line of an earlier run of the PR's
// role, which is over (a restart cut it before the token's kill existed,
// or its stop failed): RunShell stops it at once and types the new line
// into the same pane, without waiting out IdleShellTimeout first.
func TestRunShellReclaimsAPaneItsEarlierRunHolds(t *testing.T) {
	e := newEnv(t)
	e.shellWorkspace()
	earlier, pane := e.shellRun(store.RunAbandoned)
	e.world(pane).job(e.dbLockArgv(RoleCodexReview, shellCheckout, earlier.ID))
	e.h.waitLine = "MAGNUM_DONE_r-2 0"
	start := e.clock.Now()

	status, err := e.m.RunShell(e.ctx, e.pr, e.spec(RoleCodexReview), pane, "the line", DoneMarker("r-2"), time.Minute)
	if err != nil || status != 0 {
		t.Fatalf("RunShell = %d, %v", status, err)
	}
	if len(e.h.runs) != 1 || e.h.runs[0].Pane != pane {
		t.Fatalf("runs = %+v, want the line in %s", e.h.runs, pane)
	}
	if took := e.clock.Now().Sub(start); took >= IdleShellTimeout {
		t.Errorf("the line ran %s after RunShell began, want before %s", took, IdleShellTimeout)
	}
	if got := e.signalsTo(); !slices.Contains(got, fmt.Sprintf("-%d:TERM", jobPGID)) {
		t.Errorf("signals = %v, want SIGTERM to the earlier run's group", got)
	}
	if len(e.h.splits) != 0 || len(e.h.paneCloses) != 0 {
		t.Errorf("splits %v, closes %v, want none", e.h.splits, e.h.paneCloses)
	}
	if !e.m.ShellLaunched("r-2") {
		t.Error("the typed line's run is not marked launched")
	}
}

// When the earlier run's job outlives SIGKILL, its pane is replaced, and
// the new line goes into the replacement.
func TestRunShellTypesIntoTheReplacementOfAPaneItsEarlierRunHeld(t *testing.T) {
	e := newEnv(t)
	e.shellWorkspace()
	earlier, pane := e.shellRun(store.RunAbandoned)
	w := e.world(pane)
	w.job(e.dbLockArgv(RoleCodexReview, shellCheckout, earlier.ID))
	w.set(anyProc, false, false, true)
	e.h.waitLine = "MAGNUM_DONE_r-2 0"

	if _, err := e.m.RunShell(e.ctx, e.pr, e.spec(RoleCodexReview), pane, "the line", DoneMarker("r-2"), time.Minute); err != nil {
		t.Fatalf("RunShell = %v", err)
	}
	if len(e.h.splits) != 1 {
		t.Fatalf("splits = %+v", e.h.splits)
	}
	repl := e.h.splits[0].New
	if len(e.h.runs) != 1 || e.h.runs[0].Pane != repl {
		t.Errorf("runs = %+v, want the line in the replacement %s", e.h.runs, repl)
	}
}

// Something that is no run of magnum's keeps codex-review's pane busy past
// IdleShellTimeout (the operator's command, a run from before the token):
// RunShell splits a new pane with the session's environment, points the
// session at it and runs the line there. The busy pane gets no key, no
// signal and no close; it stays on a retired row of its own.
func TestRunShellMovesToANewPaneWhenSomethingElseRuns(t *testing.T) {
	e := newEnv(t)
	e.shellWorkspace()
	before := e.session(RoleCodexReview)
	pane := store.Deref(before.HerdrPaneID)
	e.setProcs(pane, otherJob(pane))
	e.world(pane)
	e.h.waitLine = "MAGNUM_DONE_r-2 0"

	if _, err := e.m.RunShell(e.ctx, e.pr, e.spec(RoleCodexReview), pane, "the line", DoneMarker("r-2"), time.Minute); err != nil {
		t.Fatalf("RunShell = %v", err)
	}
	if len(e.h.splits) != 1 || e.h.splits[0].From != pane || e.h.splits[0].Opts.Env["WT_BRANCH"] != "review1" ||
		e.h.splits[0].Opts.Cwd != shellCheckout {
		t.Fatalf("splits = %+v, want one from %s with the session's environment", e.h.splits, pane)
	}
	repl := e.h.splits[0].New
	if len(e.h.runs) != 1 || e.h.runs[0].Pane != repl {
		t.Errorf("runs = %+v, want the line in %s", e.h.runs, repl)
	}
	if got := e.signalsTo(); len(got) != 0 {
		t.Errorf("signals = %v, want none", got)
	}
	if len(e.h.paneCloses) != 0 {
		t.Errorf("closes = %v, want none", e.h.paneCloses)
	}
	for _, k := range e.keysSent() {
		if strings.HasPrefix(k, pane+":") {
			t.Errorf("key %s to the busy pane", k)
		}
	}
	if s := e.session(RoleCodexReview); s.ID != before.ID || store.Deref(s.HerdrPaneID) != repl {
		t.Errorf("session = %d in %s, want %d in %s", s.ID, store.Deref(s.HerdrPaneID), before.ID, repl)
	}
	if !slices.ContainsFunc(e.sessions(), func(s store.Session) bool {
		return retired(s) && s.Role == "codex-review" && store.Deref(s.HerdrPaneID) == pane
	}) {
		t.Errorf("sessions = %+v, want a retired row of codex-review in %s", e.sessions(), pane)
	}
	if b := e.eventsOf(EventShellMoveBegin); len(b) != 1 || eventData(t, b[0])["pane"] != pane {
		t.Errorf("move begin events = %+v", b)
	}
	if m := e.eventsOf(EventShellMoved); len(m) != 1 || eventData(t, m[0])["to"] != repl {
		t.Errorf("moved events = %+v", m)
	}
}

// Without an environment to give a new pane (it would act as the wrong
// account), a pane something else keeps busy stays the role's, and the run
// is refused as busy, as before: nothing split, sent or closed.
func TestRunShellStaysBusyWithoutAnEnvironmentToMoveWith(t *testing.T) {
	e := newEnv(t)
	e.shellWorkspace()
	for _, s := range e.sessions() {
		if err := e.st.UpdateSession(e.ctx, s.ID, func(u *store.SessionUpdate) { u.Set("env_json", map[string]string{}) }); err != nil {
			t.Fatal(err)
		}
	}
	pane := store.Deref(e.session(RoleCodexReview).HerdrPaneID)
	e.setProcs(pane, otherJob(pane))

	if _, err := e.m.RunShell(e.ctx, e.pr, e.spec(RoleCodexReview), pane, "the line", DoneMarker("r-2"), time.Minute); !errors.Is(err, ErrBusy) {
		t.Fatalf("RunShell = %v, want ErrBusy", err)
	}
	if len(e.h.splits) != 0 || len(e.h.paneCloses) != 0 || len(e.h.runs) != 0 || len(e.keysSent()) != 0 || len(e.signalsTo()) != 0 {
		t.Errorf("splits %v, closes %v, runs %v, keys %v, signals %v: want none", e.h.splits, e.h.paneCloses, e.h.runs, e.keysSent(), e.signalsTo())
	}
}

// A push while RunShell waits for a pane something else keeps busy cancels
// the wait before the line was typed: the run is not launched, and its
// interrupt and its stop send that pane nothing (no key, no signal, no
// close). The operator's command runs on undisturbed.
func TestAPushDuringTheWaitForABusyPaneSendsItNothing(t *testing.T) {
	e := newEnv(t)
	e.shellWorkspace()
	sess := e.session(RoleCodexReview)
	pane := store.Deref(sess.HerdrPaneID)
	e.setProcs(pane, otherJob(pane))
	run, err := e.m.NewRun(e.ctx, e.pr, RoleCodexReview, store.RunInitial, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.TransitionRun(e.ctx, run.ID, []string{store.RunPending}, store.RunSubmitted, func(u *store.RunUpdate) { u.Set("session_id", sess.ID) }); err != nil {
		t.Fatal(err)
	}
	run, _ = e.st.RunByID(e.ctx, run.ID)
	ctx, cancel := context.WithCancel(e.ctx)
	e.h.onWaitIdle = func(*fakeHerdr, string, time.Duration) { cancel() } // the push cuts the stage
	if _, err := e.m.RunShell(ctx, e.pr, e.spec(RoleCodexReview), pane, "the line", DoneMarker(run.ID), time.Minute); err == nil {
		t.Fatal("RunShell = nil, want the cut")
	}
	if e.m.ShellLaunched(run.ID) {
		t.Fatal("a line never typed counts as launched")
	}
	if err := e.m.InterruptShell(e.ctx, run); err != nil {
		t.Errorf("InterruptShell = %v, want nothing to do", err)
	}
	if err := e.m.StopShell(e.ctx, run); err != nil {
		t.Errorf("StopShell = %v, want nothing to do", err)
	}
	if len(e.keysSent()) != 0 || len(e.signalsTo()) != 0 || len(e.h.paneCloses) != 0 || len(e.h.splits) != 0 || len(e.h.runs) != 0 {
		t.Errorf("keys %v, signals %v, closes %v, splits %v, runs %v: want none", e.keysSent(), e.signalsTo(), e.h.paneCloses, e.h.splits, e.h.runs)
	}
}

// The interrupt of a shell role's run is ctrl+c to its pane only while the
// pane's foreground carries this run's token: a line never typed, an
// earlier run's command, the operator's and an idle shell get nothing.
func TestInterruptShellKeysOnlyItsOwnRunsCommand(t *testing.T) {
	for _, tc := range []struct {
		name     string
		launched bool
		token    func(e *env, run store.Run) []string // nil: an idle shell
		keyed    bool
		notOwn   bool
	}{
		{"its own command", true, func(e *env, run store.Run) []string { return e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID) }, true, false},
		{"never typed", false, func(e *env, run store.Run) []string { return e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID) }, false, false},
		{"an earlier run's command", true, func(e *env, _ store.Run) []string {
			r, _ := e.m.NewRun(e.ctx, e.pr, RoleCodexReview, store.RunInitial, 1)
			_ = e.st.TransitionRun(e.ctx, r.ID, nil, store.RunAbandoned, nil)
			return e.dbLockArgv(RoleCodexReview, shellCheckout, r.ID)
		}, false, true},
		{"the operator's command", true, func(*env, store.Run) []string { return []string{"vim", "notes.md"} }, false, true},
		{"an idle shell", true, nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.shellWorkspace()
			run, pane := e.shellRun(store.RunSubmitted)
			if !tc.launched {
				e.m.mu.Lock()
				delete(e.m.launched, run.ID)
				e.m.mu.Unlock()
			}
			if tc.token != nil {
				e.world(pane).job(tc.token(e, run))
			} else {
				e.setProcs(pane, idlePane(pane))
			}
			err := e.m.InterruptShell(e.ctx, run)
			if errors.Is(err, ErrNotOwn) != tc.notOwn || (!tc.notOwn && err != nil) {
				t.Fatalf("InterruptShell = %v, want not own %t", err, tc.notOwn)
			}
			keyed := slices.Contains(e.keysSent(), pane+":ctrl+c")
			if keyed != tc.keyed || len(e.keysSent()) > 1 {
				t.Errorf("keys = %v, want ctrl+c %t", e.keysSent(), tc.keyed)
			}
		})
	}
}

// Before a round's checkout switches the slot, the PR's shell panes there
// are made sure to run nothing: an own leftover (an earlier run of the role,
// over) is stopped, and the round goes on; a command magnum does not own,
// in the live pane or in one the role moved away from, holds the round
// under the human cooldown (ErrHumanActive) with EventShellHold; an idle
// pane, or a pane in another checkout, holds nothing.
func TestClearShellPanesStopsAnOwnLeftoverAndHoldsForAnythingElse(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(e *env, pane string)
		hold bool
	}{
		{"an own leftover", func(e *env, pane string) {
			earlier, _ := e.shellRun(store.RunAbandoned)
			e.world(pane).job(e.dbLockArgv(RoleCodexReview, shellCheckout, earlier.ID))
		}, false},
		{"the operator's command", func(e *env, pane string) { e.setProcs(pane, otherJob(pane)) }, true},
		{"a pane the role moved away from", func(e *env, pane string) {
			s := e.session(RoleCodexReview)
			if _, err := e.st.CreateSession(e.ctx, store.Session{PRID: e.pr.ID, Role: s.Role, AgentKind: new(KindShell),
				HerdrWorkspaceID: s.HerdrWorkspaceID, HerdrPaneID: new("w1:p9"), Cwd: s.Cwd, State: store.SessionClosed}); err != nil {
				e.t.Fatal(err)
			}
			e.setProcs("w1:p9", otherJob("w1:p9"))
		}, true},
		{"an idle pane", func(e *env, pane string) { e.setProcs(pane, idlePane(pane)) }, false},
		{"another checkout", func(e *env, pane string) {
			s := e.session(RoleCodexReview)
			if err := e.st.UpdateSession(e.ctx, s.ID, func(u *store.SessionUpdate) { u.Set("cwd", "/Users/x/Projects/talkable.review2") }); err != nil {
				e.t.Fatal(err)
			}
			e.setProcs(pane, otherJob(pane))
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.shellWorkspace()
			pane := store.Deref(e.session(RoleCodexReview).HerdrPaneID)
			tc.set(e, pane)
			err := e.m.ClearShellPanes(e.ctx, e.pr, shellCheckout)
			if tc.hold != errors.Is(err, ErrHumanActive) || (!tc.hold && err != nil) {
				t.Fatalf("ClearShellPanes = %v, want a hold %t", err, tc.hold)
			}
			if got := len(e.eventsOf(EventShellHold)) == 1; got != tc.hold {
				t.Errorf("hold events = %+v, want %t", e.eventsOf(EventShellHold), tc.hold)
			}
			if tc.hold && !e.humanActive() {
				t.Error("the hold did not start the human cooldown")
			}
			if tc.hold && (len(e.signalsTo()) != 0 || len(e.keysSent()) != 0 || len(e.h.paneCloses) != 0) {
				t.Errorf("signals %v, keys %v, closes %v to a command magnum does not own", e.signalsTo(), e.keysSent(), e.h.paneCloses)
			}
		})
	}
}

// The pane RunShell left busy closes with the retired panes, and only once
// it is an idle shell.
func TestALeftBusyPaneClosesOnlyOnceIdle(t *testing.T) {
	e := newEnv(t)
	e.shellWorkspace()
	pane := store.Deref(e.session(RoleCodexReview).HerdrPaneID)
	e.setProcs(pane, otherJob(pane))
	e.h.waitLine = "MAGNUM_DONE_r-2 0"
	if _, err := e.m.RunShell(e.ctx, e.pr, e.spec(RoleCodexReview), pane, "the line", DoneMarker("r-2"), time.Minute); err != nil {
		t.Fatalf("RunShell = %v", err)
	}
	if err := e.m.CloseRetiredPanes(e.ctx, e.pr); err != nil {
		t.Fatal(err)
	}
	if len(e.h.paneCloses) != 0 {
		t.Fatalf("closes = %v while the pane is busy", e.h.paneCloses)
	}
	e.setProcs(pane, idlePane(pane))
	if err := e.m.CloseRetiredPanes(e.ctx, e.pr); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(e.h.paneCloses, []string{pane}) {
		t.Errorf("closes = %v, want the left pane once idle", e.h.paneCloses)
	}
	if s := e.session(RoleCodexReview); store.Deref(s.HerdrPaneID) == pane || s.State != store.SessionLive {
		t.Errorf("session = %+v, want it live in its new pane", s)
	}
}

// A fresh pane's shell (oh-my-zsh and mise starting) is busy for a moment:
// it still gets IdleShellTimeout to become idle, and the line runs in it,
// with no signal and no move.
func TestAFreshPaneStillGetsItsMinuteToStart(t *testing.T) {
	e := newEnv(t)
	e.shellWorkspace()
	pane := store.Deref(e.session(RoleCodexReview).HerdrPaneID)
	e.setProcs(pane, herdr.ProcessInfo{PaneID: pane, ShellPID: shellPID, ForegroundPGID: 600,
		Foreground: []herdr.Process{{PID: 600, Name: "mise", Argv: []string{"mise", "activate", "zsh"}}}})
	e.h.onWaitIdle = func(f *fakeHerdr, p string, timeout time.Duration) {
		if p == pane && timeout >= 20*time.Second {
			f.clock.Add(20 * time.Second)
			f.procs[p] = idlePane(p)
		}
	}
	e.h.waitLine = "MAGNUM_DONE_r-2 0"

	if _, err := e.m.RunShell(e.ctx, e.pr, e.spec(RoleCodexReview), pane, "the line", DoneMarker("r-2"), time.Minute); err != nil {
		t.Fatalf("RunShell = %v", err)
	}
	if len(e.h.runs) != 1 || e.h.runs[0].Pane != pane {
		t.Errorf("runs = %+v, want the line in %s", e.h.runs, pane)
	}
	if !slices.Contains(e.h.idleWaits, IdleShellTimeout) {
		t.Errorf("idle waits = %v, want one of %s", e.h.idleWaits, IdleShellTimeout)
	}
	if len(e.h.splits) != 0 || len(e.h.paneCloses) != 0 || len(e.signalsTo()) != 0 {
		t.Errorf("splits %v, closes %v, signals %v: want none", e.h.splits, e.h.paneCloses, e.signalsTo())
	}
}

// The shell line's db-lock names the run (--run): the run token StopShell
// reads from the process's argv, for this role and checkout.
func TestTheShellLineCarriesTheRunInItsDBLockLine(t *testing.T) {
	role := defaultRole(t, RoleCodexReview)
	const run = "r-20261028T101500-3"
	line, err := ShellLine(role, ShellData{BaseRef: "origin/master", ReportPath: "/tmp/codex-review.md", Marker: DoneMarker(run),
		Checkout: "/slots/talkable review3", Magnum: "/opt/magnum/bin/magnum"})
	if err != nil {
		t.Fatal(err)
	}
	want := "/opt/magnum/bin/magnum db-lock --checkout '/slots/talkable review3' --role codex-review --run " + run + " -- codex review"
	if !strings.Contains(line, want) {
		t.Fatalf("line = %s\nwant it to contain %s", line, want)
	}
	argv := []string{"/opt/magnum/bin/magnum", "db-lock", "--checkout", "/slots/talkable review3", "--role", "codex-review", "--run", run,
		"--", "codex", "review", "--base", "origin/master"}
	tok, ok := runToken(argv, "/opt/magnum/bin/magnum")
	if !ok || tok.role != "codex-review" || tok.checkout != "/slots/talkable review3" || tok.run != run {
		t.Errorf("runToken = %+v, %t", tok, ok)
	}
	if _, ok := runToken(argv[:len(argv)-5], "/opt/magnum/bin/magnum"); ok {
		t.Error("runToken read a token from a line without its --")
	}
}
