package pipeline

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// paneSends are the keys sent to pane.
func (e *env) paneSends(pane string) []string {
	var out []string
	for _, s := range e.keySends() {
		if strings.HasPrefix(s.to, "pane:"+pane+":") {
			out = append(out, s.to)
		}
	}
	return out
}

// A push while codex-review's RunShell still waits for a pane something
// else keeps busy (the operator's command): its line was never typed, so
// the cut sends that pane nothing, no ctrl+c and no stop, and the restart
// goes on to the new head.
func TestACutRunWhoseLineWasNeverTypedGetsNoKeys(t *testing.T) {
	e := newEnv(t)
	in := e.input(KindInitial)
	sw := e.withRestarts(&in, 2)
	pane := e.pane(agents.RoleCodexReview)
	e.ag.unsent = map[agents.Role]bool{agents.RoleCodexReview: true}
	e.keys.run(pane, 99) // the operator's command, which no ctrl+c ends
	var mu sync.Mutex
	calls := 0
	e.ag.codex = func(f *fakeAgents, c codexCall) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			e.push(head2)
			<-c.Ctx.Done() // RunShell's wait for an idle shell, cut
			return fmt.Errorf("pane %s: wait for idle shell: %w", c.Pane, c.Ctx.Err())
		}
		return fmt.Errorf("pane %s: %w: still busy", c.Pane, agents.ErrBusy)
	}
	e.ag.behaviors[agents.RoleClaude] = []behavior{func(f *fakeAgents, run store.Run, text string) error {
		e.waitHead(head2)
		return nil
	}, writeReport("## P2 on the new head\n")}
	post := e.judgePosts(903, "COMMENTED", "COMMENT")
	post.commit = head2
	e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(t)}

	if _, err := e.r.RunRound(e.ctx, in); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if got := e.paneSends(pane); len(got) != 0 {
		t.Errorf("keys to the busy pane = %v, want none", got)
	}
	if got := e.ag.stopped(); len(got) != 0 {
		t.Errorf("StopShell calls = %+v, want none for a line never typed", got)
	}
	if got := sw.asked(); !slices.Equal(got, []string{head2}) {
		t.Errorf("switches = %v, want %s", got, head2)
	}
}

// Only a positively idle shell, a pane herdr confirms gone or a verified
// stop counts as stopped: a wait or a ctrl+c that herdr fails on, or a
// settle that panics, leaves the command possibly running, so the restart
// keeps the checkout.
func TestHerdrErrorsWhileTheCommandRunsKeepTheCheckout(t *testing.T) {
	for _, tc := range []struct {
		name     string
		set      func(e *env, pane string)
		switched bool
	}{
		{"a failed wait", func(e *env, pane string) {
			e.keys.waitErr = map[string]error{pane: errors.New("herdr: connection reset")}
		}, false},
		{"a failed ctrl+c", func(e *env, pane string) {
			e.keys.sendErr = map[string]error{pane: errors.New("herdr: connection reset")}
		}, false},
		{"a pane herdr confirms gone", func(e *env, pane string) {
			e.keys.waitErr = map[string]error{pane: &herdr.Error{Method: "pane.process_info", Code: herdr.CodePaneNotFound, Message: pane}}
			e.keys.gone = map[string]bool{pane: true}
		}, true},
		{"a settle that panics", func(e *env, pane string) {
			e.ag.onStopShell = func(*fakeAgents, store.Run) error { panic("settle broke") }
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			in := e.input(KindInitial)
			sw := e.withRestarts(&in, 2)
			e.codexUntilCut(sw, shellStopPresses+1, head2)
			pane := e.pane(agents.RoleCodexReview)
			tc.set(e, pane)
			e.ag.behaviors[agents.RoleClaude] = []behavior{pushThen(e, head2, hang()), writeReport("## P2 on the new head\n")}
			post := e.judgePosts(904, "COMMENTED", "COMMENT")
			post.commit = head2
			e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(t)}

			_, err := e.r.RunRound(e.ctx, in)
			if switched := len(sw.asked()) > 0; switched != tc.switched {
				t.Fatalf("switches = %v (err %v), want a switch %t", sw.asked(), err, tc.switched)
			}
			if !tc.switched && !errors.Is(err, ErrShellRunning) {
				t.Errorf("RunRound = %v, want ErrShellRunning", err)
			}
		})
	}
}

// A shell reviewer that timed out may still run its command: it gets the
// stop a cut gets before its run is finished (the run keeps its timeout),
// and when the stop cannot prove it gone, a push that comes later restarts
// nothing under it, though its run is no longer in flight. A stop that
// works the second time lets the restart go on.
func TestAShellTimeoutThenAPushKeepsTheCheckout(t *testing.T) {
	for _, tc := range []struct {
		name       string
		secondStop error
	}{
		{"still runs", errors.New("2 processes of its job still run after SIGKILL (sandbox-exec)")},
		{"stopped the second time", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			in := e.input(KindInitial)
			sw := e.withRestarts(&in, 2)
			pane := e.pane(agents.RoleCodexReview)
			var mu sync.Mutex
			stops := 0
			e.ag.onStopShell = func(f *fakeAgents, run store.Run) error {
				mu.Lock()
				defer mu.Unlock()
				stops++
				if stops == 1 {
					return errors.New("3 processes of its job still run after SIGKILL (codex,sandbox-exec)")
				}
				if tc.secondStop == nil {
					e.keys.run(pane, 0)
				}
				return tc.secondStop
			}
			e.ag.codex = func(f *fakeAgents, c codexCall) error {
				if sw.head() == target {
					e.keys.run(c.Pane, 99)
					return fmt.Errorf("wait for %s: %w", c.Marker, agents.ErrTimeout)
				}
				dir := e.dirOf(sw.head())
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(dir, "codex-review.md"), []byte("[P2] codex finding\n"), 0o600)
			}
			e.ag.behaviors[agents.RoleClaude] = []behavior{func(f *fakeAgents, run store.Run, text string) error {
				e.waitRun(agents.RoleCodexReview, target, store.RunFailed)
				e.push(head2)
				return nil
			}, writeReport("## P2 on the new head\n")}
			post := e.judgePosts(905, "COMMENTED", "COMMENT")
			post.commit = head2
			e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(t)}

			_, err := e.r.RunRound(e.ctx, in)
			for _, r := range e.runs() {
				if r.Role == "codex-review" && r.TargetSHA == target &&
					(r.State != store.RunFailed || store.Deref(r.Outcome) != ReportTimeout) {
					t.Errorf("the timed-out run = %s %s, want failed with its timeout", r.State, store.Deref(r.Outcome))
				}
			}
			if tc.secondStop != nil {
				if len(sw.asked()) != 0 || !errors.Is(err, ErrShellRunning) {
					t.Fatalf("switches %v, err %v: want none under the command that may run", sw.asked(), err)
				}
				return
			}
			if err != nil || !slices.Equal(sw.asked(), []string{head2}) {
				t.Fatalf("switches %v, err %v: want the restart on %s", sw.asked(), err, head2)
			}
		})
	}
}

// A round that ends itself (the judge's own pass was refused) while
// codex-review's command outlives its ctrl+c stops that command as a
// restart does: the presses, then StopShell.
func TestTheRoundsEndStopsAShellCommandItCut(t *testing.T) {
	e := newEnv(t)
	e.ag.reads[agents.RoleJudge] = codexRefusal
	in := e.ownInput(KindInitial)
	sw := e.withRestarts(&in, 2)
	e.codexUntilCut(sw, shellStopPresses+1, "")
	pane := e.pane(agents.RoleCodexReview)
	e.ag.onStopShell = func(f *fakeAgents, run store.Run) error {
		e.keys.run(pane, 0)
		return nil
	}
	end := endSilently()
	e.ag.behaviors[agents.RoleJudge] = []behavior{func(f *fakeAgents, run store.Run, text string) error {
		e.waitRun(agents.RoleCodexReview, target, store.RunSubmitted)
		return end(f, run, text)
	}}
	e.ag.hangs = map[agents.Role]bool{agents.RoleClaude: true}

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomeRefused {
		t.Fatalf("result = %+v, %v; want refused", res, err)
	}
	if got := e.ag.stopped(); len(got) != 1 || got[0].Role != "codex-review" {
		t.Errorf("StopShell calls = %+v, want codex-review's run", got)
	}
	if n := len(e.paneSends(pane)); n != shellStopPresses {
		t.Errorf("codex-review's pane got %d ctrl+c, want %d", n, shellStopPresses)
	}
}

// A cut command whose ctrl+c left the pane an idle shell is stopped only
// once the manager finds what the interrupt captured of its job gone (a
// sandbox child may outlive codex): while StopShell says a captured
// process still runs, the restart keeps the checkout.
func TestAnIdlePaneIsAStopOnlyOnceTheCapturedJobIsGone(t *testing.T) {
	e := newEnv(t)
	in := e.input(KindInitial)
	sw := e.withRestarts(&in, 2)
	e.codexUntilCut(sw, 1, head2) // the interrupt's ctrl+c leaves an idle shell
	e.ag.onStopShell = func(*fakeAgents, store.Run) error {
		return errors.New("agents: stop codex-review in pane p3: 1 processes of its job still run after SIGKILL (sandbox-exec)")
	}
	e.ag.behaviors[agents.RoleClaude] = []behavior{pushThen(e, head2, hang())}

	_, err := e.r.RunRound(e.ctx, in)
	if got := sw.asked(); len(got) != 0 || !errors.Is(err, ErrShellRunning) {
		t.Fatalf("switches %v, err %v: want the checkout kept while a captured process runs", got, err)
	}
	if n := len(e.ag.stopped()); n != 1 {
		t.Errorf("StopShell calls = %d, want the one look after the idle shell", n)
	}
}

// A restart whose stop replaced codex-review's pane (SIGKILL left it busy)
// goes on there: the restarted line runs in the replacement the session
// points at.
func TestARestartGoesOnInTheReplacementPane(t *testing.T) {
	e := newEnv(t)
	in := e.input(KindInitial)
	sw := e.withRestarts(&in, 2)
	e.codexUntilCut(sw, shellStopPresses+1, head2)
	pane := e.pane(agents.RoleCodexReview)
	e.ag.onStopShell = func(f *fakeAgents, run store.Run) error {
		if err := e.st.UpdateSession(e.ctx, *run.SessionID, func(u *store.SessionUpdate) { u.Set("herdr_pane_id", "p9") }); err != nil {
			return err
		}
		e.keys.run(pane, 0)
		return nil
	}
	e.ag.behaviors[agents.RoleClaude] = []behavior{pushThen(e, head2, hang()), writeReport("## P2 on the new head\n")}
	post := e.judgePosts(906, "COMMENTED", "COMMENT")
	post.commit = head2
	e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(t)}

	if _, err := e.r.RunRound(e.ctx, in); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	calls := e.ag.shellCallsFor(agents.RoleCodexReview)
	if len(calls) != 2 || calls[1].Pane != "p9" {
		t.Errorf("codex-review lines = %+v, want the restarted one in the replacement p9", calls)
	}
}
