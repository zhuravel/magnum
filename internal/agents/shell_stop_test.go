package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
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
// and a sandbox's child of codex in a process group of its own.
const (
	shellPID   = 100
	jobPGID    = 200
	dbLockPID  = 201
	codexPID   = 202
	teePID     = 203
	sandboxPID = 300
	otherPID   = 400 // a process of the same table under nothing of the job
)

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
// line it typed), and the pane of the workspace e.workspace laid out.
func (e *env) shellRun(state string) (store.Run, string) {
	e.t.Helper()
	pane := e.session(RoleCodexReview).HerdrPaneID
	run, err := e.m.NewRun(e.ctx, e.pr, RoleCodexReview, store.RunInitial, 1)
	if err != nil {
		e.t.Fatal(err)
	}
	sess := e.session(RoleCodexReview)
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
	return run, store.Deref(pane)
}

// dbLockArgv is the argv of the db-lock a shell line of role starts for
// run in checkout, as herdr reads it from the process.
func (e *env) dbLockArgv(role Role, checkout, run string) []string {
	return []string{e.m.d.Layout.Binary(), "db-lock", "--checkout", checkout, "--role", string(role), "--run", run, "--",
		"codex", "review", "-c", "model_reasoning_effort=high", "--base", "origin/master"}
}

// runningJob is the process info of a pane whose foreground job runs the
// db-lock argv (the brace group, db-lock, codex and tee in group jobPGID).
func runningJob(pane string, dbLock []string) herdr.ProcessInfo {
	return herdr.ProcessInfo{PaneID: pane, ShellPID: shellPID, ForegroundPGID: jobPGID, Foreground: []herdr.Process{
		{PID: jobPGID, Name: "zsh", Argv: []string{"zsh"}},
		{PID: dbLockPID, Name: "magnum", Argv: dbLock},
		{PID: codexPID, Name: "codex", Argv: []string{"codex", "review", "--base", "origin/master"}},
		{PID: teePID, Name: "tee", Argv: []string{"tee", "/state/reviews/codex-review.md"}},
	}}
}

// idlePane is the process info of an idle shell.
func idlePane(pane string) herdr.ProcessInfo {
	return herdr.ProcessInfo{PaneID: pane, ShellPID: shellPID, ForegroundPGID: shellPID}
}

// psTable answers the manager's ps with the job's processes: codex under
// db-lock, the sandbox's child under codex in a group of its own, and a
// process under nothing of the job.
func (e *env) psTable() {
	rows := []string{
		fmt.Sprintf("%d 1 %d /bin/zsh", shellPID, shellPID),
		fmt.Sprintf("%d %d %d /bin/zsh", jobPGID, shellPID, jobPGID),
		fmt.Sprintf("%d %d %d /opt/magnum/bin/magnum", dbLockPID, jobPGID, jobPGID),
		fmt.Sprintf("%d %d %d /usr/local/bin/codex", codexPID, dbLockPID, jobPGID),
		fmt.Sprintf("%d %d %d /usr/bin/tee", teePID, shellPID, jobPGID),
		fmt.Sprintf("%d %d %d /usr/bin/sandbox-exec", sandboxPID, codexPID, sandboxPID),
		fmt.Sprintf("%d 1 %d /Applications/Some App.app/Contents/MacOS/Some App", otherPID, otherPID),
	}
	e.run.Rules = append(e.run.Rules, execx.Rule{Prefix: []string{"ps"},
		Result: execx.Result{Stdout: []byte(strings.Join(rows, "\n") + "\n")}})
}

// setProcs sets what herdr answers for pane.
func (e *env) setProcs(pane string, pi herdr.ProcessInfo) {
	e.h.mu.Lock()
	defer e.h.mu.Unlock()
	e.h.procs[pane] = pi
}

// endsOn makes the pane's command end (an idle shell) when the job's group
// gets sig.
func (e *env) endsOn(pane string, sig syscall.Signal) {
	e.sigs.on = func(pid int, got syscall.Signal) error {
		if pid == -jobPGID && got == sig {
			e.setProcs(pane, idlePane(pane))
		}
		return nil
	}
}

// signalsTo are the signals sent, as "<pid>:<signal>".
func (e *env) signalsTo() []string {
	var out []string
	for _, s := range e.sigs.all() {
		out = append(out, fmt.Sprintf("%d:%s", s.pid, sigName(s.sig)))
	}
	return out
}

func sigName(s syscall.Signal) string {
	switch s {
	case syscall.SIGTERM:
		return "TERM"
	case syscall.SIGKILL:
		return "KILL"
	}
	return s.String()
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

// A cut codex-review command that its ctrl+c did not end, in its own pane
// and with its run token, gets one SIGTERM to its process group and to the
// tree under its db-lock, and nothing else once the shell is idle again:
// no SIGKILL, no close. The begin event names the process group and the
// tree (pid, group and name, never argv), the stopped one the step.
func TestStopShellTermsTheProcessGroupOfItsOwnRun(t *testing.T) {
	e := newEnv(t)
	e.shellWorkspace()
	run, pane := e.shellRun(store.RunSubmitted)
	e.setProcs(pane, runningJob(pane, e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID)))
	e.psTable()
	e.endsOn(pane, syscall.SIGTERM)

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
	if !slices.Contains(got, fmt.Sprintf("%d:TERM", codexPID)) {
		t.Errorf("signals = %v, want codex under db-lock too", got)
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
		strings.Contains(raw, "db-lock") || strings.Contains(raw, "/usr/") {
		t.Errorf("begin data %s, want the tree by pid, group and name, never argv", raw)
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
	e.setProcs(pane, runningJob(pane, e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID)))
	e.psTable()
	e.endsOn(pane, syscall.SIGKILL)

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
	if !slices.Contains(e.signalsTo(), fmt.Sprintf("%d:KILL", codexPID)) {
		t.Errorf("signals = %v, want SIGKILL to codex too", e.signalsTo())
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
// closed: ErrNotOwn, and the failed event says not_own.
func TestStopShellNeverSignalsACommandWithoutItsRunToken(t *testing.T) {
	for name, argv := range map[string]func(e *env, run string) []string{
		"no run token": func(e *env, run string) []string {
			return []string{e.m.d.Layout.Binary(), "db-lock", "--checkout", shellCheckout, "--role", "codex-review", "--", "codex", "review"}
		},
		"no db-lock": func(e *env, run string) []string {
			return []string{"codex", "review", "--run", run}
		},
		"another program": func(e *env, run string) []string {
			return []string{"/usr/local/bin/other", "db-lock", "--checkout", shellCheckout, "--role", "codex-review", "--run", run, "--", "x"}
		},
		"another role": func(e *env, run string) []string {
			return e.dbLockArgv(RoleClaude, shellCheckout, run)
		},
		"another checkout": func(e *env, run string) []string {
			return e.dbLockArgv(RoleCodexReview, "/Users/x/Projects/talkable.review2", run)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.shellWorkspace()
			run, pane := e.shellRun(store.RunSubmitted)
			e.setProcs(pane, runningJob(pane, argv(e, run.ID)))
			e.psTable()
			if err := e.m.StopShell(e.ctx, run); !errors.Is(err, ErrNotOwn) {
				t.Fatalf("StopShell = %v, want ErrNotOwn", err)
			}
			if got := e.signalsTo(); len(got) != 0 {
				t.Errorf("signals = %v, want none", got)
			}
			if len(e.h.paneCloses) != 0 || len(e.h.splits) != 0 {
				t.Errorf("closes %v, splits %v, want none", e.h.paneCloses, e.h.splits)
			}
			failed := e.eventsOf(EventShellStopFailed)
			if len(failed) != 1 || eventData(t, failed[0])["cause"] != "not_own" {
				t.Errorf("failed events = %+v, want one with the cause not_own", failed)
			}
			if b := e.eventsOf(EventShellStopBegin); len(b) != 0 {
				t.Errorf("begin events = %+v", b)
			}
		})
	}
}

// The run token must name a run of this PR and role, the cut run or one no
// longer in flight: another PR's run, a run the registry does not know and
// another run still in flight are never signalled.
func TestStopShellNeverSignalsAnotherPRsOrAnInFlightRun(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token func(e *env, cut store.Run) string
	}{
		{"another PR's run", func(e *env, _ store.Run) string {
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
		}},
		{"an unknown run", func(*env, store.Run) string { return "r-20261028T000000-99" }},
		{"another run in flight", func(e *env, _ store.Run) string {
			r, err := e.m.NewRun(e.ctx, e.pr, RoleCodexReview, store.RunInitial, 2)
			if err != nil {
				e.t.Fatal(err)
			}
			return r.ID // pending: in flight
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.shellWorkspace()
			cut, pane := e.shellRun(store.RunSubmitted)
			e.setProcs(pane, runningJob(pane, e.dbLockArgv(RoleCodexReview, shellCheckout, tc.token(e, cut))))
			e.psTable()
			if err := e.m.StopShell(e.ctx, cut); !errors.Is(err, ErrNotOwn) {
				t.Fatalf("StopShell = %v, want ErrNotOwn", err)
			}
			if got := e.signalsTo(); len(got) != 0 {
				t.Errorf("signals = %v, want none", got)
			}
			if len(e.h.paneCloses) != 0 {
				t.Errorf("closes = %v", e.h.paneCloses)
			}
		})
	}
}

// herdr is asked again right before each signal: a process group or a
// token process that changed since the last look (the command ended and
// something else took the foreground) gets nothing more.
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
			e.setProcs(pane, runningJob(pane, e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID)))
			e.psTable()
			e.h.onProcInfo = func(f *fakeHerdr, p string, n int) {
				if n == tc.changed {
					f.procs[p] = herdr.ProcessInfo{PaneID: p, ShellPID: shellPID, ForegroundPGID: 500,
						Foreground: []herdr.Process{{PID: 500, Name: "vim", Argv: []string{"vim", "notes.md"}}}}
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
		})
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
	e.setProcs(pane, runningJob(pane, e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID)))
	e.psTable()
	e.endsOn(pane, syscall.SIGKILL)

	if err := e.m.StopShell(e.ctx, run); err != nil {
		t.Fatalf("StopShell = %v", err)
	}
	got := e.signalsTo()
	for _, want := range []string{fmt.Sprintf("%d:TERM", sandboxPID), fmt.Sprintf("%d:KILL", sandboxPID)} {
		if !slices.Contains(got, want) {
			t.Errorf("signals = %v, want %s", got, want)
		}
	}
	for _, s := range got {
		pid, _, _ := strings.Cut(strings.TrimPrefix(s, "-"), ":")
		if pid == fmt.Sprint(otherPID) || pid == fmt.Sprint(shellPID) || pid == "1" {
			t.Errorf("signal %s outside the run's tree", s)
		}
	}
}

// A pane that SIGKILL left busy is replaced: a new pane split with the
// recorded working directory and environment, the session pointed at it,
// and the old pane closed after the split.
func TestStopShellReplacesThePaneWhenKillLeavesItBusy(t *testing.T) {
	e := newEnv(t)
	e.shellWorkspace()
	run, pane := e.shellRun(store.RunSubmitted)
	e.setProcs(pane, runningJob(pane, e.dbLockArgv(RoleCodexReview, shellCheckout, run.ID)))
	e.psTable()
	before := e.session(RoleCodexReview)

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
	split, closed := slices.Index(e.h.calls, "PaneSplit "+pane+" down"), slices.Index(e.h.calls, "PaneClose "+pane)
	if split < 0 || closed < split {
		t.Errorf("calls = %v, want the split before the close", e.h.calls)
	}
	if s := e.session(RoleCodexReview); s.ID != before.ID || store.Deref(s.HerdrPaneID) != repl {
		t.Errorf("session = %d in %s, want %d in %s", s.ID, store.Deref(s.HerdrPaneID), before.ID, repl)
	}
	if stopped := e.eventsOf(EventShellStopped); len(stopped) != 1 || eventData(t, stopped[0])["step"] != "replace" {
		t.Errorf("stopped events = %+v, want the step replace", stopped)
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
	e.setProcs(pane, runningJob(pane, e.dbLockArgv(RoleCodexReview, shellCheckout, earlier.ID)))
	e.psTable()
	e.endsOn(pane, syscall.SIGTERM)
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
	e.setProcs(pane, herdr.ProcessInfo{PaneID: pane, ShellPID: shellPID, ForegroundPGID: 500,
		Foreground: []herdr.Process{{PID: 500, Name: "vim", Argv: []string{"vim", "notes.md"}}}})
	e.psTable()
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
	e.setProcs(pane, herdr.ProcessInfo{PaneID: pane, ShellPID: shellPID, ForegroundPGID: 500,
		Foreground: []herdr.Process{{PID: 500, Name: "vim", Argv: []string{"vim"}}}})

	if _, err := e.m.RunShell(e.ctx, e.pr, e.spec(RoleCodexReview), pane, "the line", DoneMarker("r-2"), time.Minute); !errors.Is(err, ErrBusy) {
		t.Fatalf("RunShell = %v, want ErrBusy", err)
	}
	if len(e.h.splits) != 0 || len(e.h.paneCloses) != 0 || len(e.h.runs) != 0 || len(e.keysSent()) != 0 || len(e.signalsTo()) != 0 {
		t.Errorf("splits %v, closes %v, runs %v, keys %v, signals %v: want none", e.h.splits, e.h.paneCloses, e.h.runs, e.keysSent(), e.signalsTo())
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

// The pane RunShell left busy closes with the retired panes, and only once
// it is an idle shell.
func TestALeftBusyPaneClosesOnlyOnceIdle(t *testing.T) {
	e := newEnv(t)
	e.shellWorkspace()
	pane := store.Deref(e.session(RoleCodexReview).HerdrPaneID)
	e.setProcs(pane, herdr.ProcessInfo{PaneID: pane, ShellPID: shellPID, ForegroundPGID: 500,
		Foreground: []herdr.Process{{PID: 500, Name: "vim", Argv: []string{"vim"}}}})
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
