package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// waitHead waits until the PR's head is sha (a push another role's turn
// records in its own goroutine).
func (e *env) waitHead(sha string) {
	e.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if pr, err := e.st.PRByID(e.ctx, e.pr.ID); err == nil && pr.HeadSHA == sha {
			return
		}
	}
	e.t.Errorf("the PR head never moved to %s", textx.ShortSHA(sha))
}

// waitBusy waits until a command runs in a shell pane.
func (e *env) waitBusy(pane string) {
	e.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if e.keys.busy(pane) {
			return
		}
	}
	e.t.Errorf("no command ever ran in pane %s", pane)
}

// pane is role's shell pane; agent its agent's name.
func (e *env) pane(role agents.Role) string  { return store.Deref(e.sess[role].HerdrPaneID) }
func (e *env) agent(role agents.Role) string { return store.Deref(e.sess[role].AgentName) }

// keySend is one interrupt key and when it went out.
type keySend struct {
	to string // "agent:<name>:<keys>" / "pane:<id>:<keys>"
	at time.Time
}

func (e *env) keySends() []keySend {
	e.keys.mu.Lock()
	defer e.keys.mu.Unlock()
	out := make([]keySend, len(e.keys.sends))
	for i, s := range e.keys.sends {
		out[i] = keySend{s, e.keys.at[i]}
	}
	return out
}

// codexUntilCut makes codex-review's first command run in its pane until the
// stage is cut (it stops at the presses-th ctrl+c), pushing sha first when
// sha is set, and its later ones write their report on the head checked out
// unless the pane is still busy: the line is then refused as the real
// RunShell refuses it once IdleShellTimeout passed.
func (e *env) codexUntilCut(sw *switcher, presses int, sha string) {
	var mu sync.Mutex
	calls := 0
	e.ag.codex = func(f *fakeAgents, c codexCall) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			e.keys.run(c.Pane, presses)
			if sha != "" {
				e.push(sha)
			}
			select {
			case <-c.Ctx.Done():
				return fmt.Errorf("wait for %s: %w", c.Marker, c.Ctx.Err())
			case <-time.After(10 * time.Second):
				return fmt.Errorf("fake codex-review: never cut")
			}
		}
		if e.keys.busy(c.Pane) {
			return fmt.Errorf("agents: run codex-review pr 11920: pane %s: %w: shell not idle after 1m0s (foreground: codex)", c.Pane, agents.ErrBusy)
		}
		dir := e.dirOf(sw.head())
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "codex-review.md"), []byte("[P2] codex finding\n"), 0o600)
	}
}

// A push while claude-review (with background work that never stops),
// codex-review's command and the judge's own pass all work: every cut turn
// gets its interrupt at the cut, before any wait (claude-review's for its
// background work) moves the clock; the waits and the stop message come
// after. The restarted round then posts on the new head.
func TestAPushInterruptsEveryCutTurnBeforeAnyWait(t *testing.T) {
	e := newEnv(t)
	in := e.ownInput(KindInitial)
	sw := e.withRestarts(&in, 2)
	e.codexUntilCut(sw, 1, "")
	e.ag.background = map[agents.Role]int{agents.RoleClaude: 2} // never stops
	var stopAt []time.Time
	e.ag.onTimeUp = func(f *fakeAgents, run store.Run, text string) error {
		stopAt = append(stopAt, e.clock.Now())
		return nil
	}
	e.ag.hangs = map[agents.Role]bool{agents.RoleJudge: true} // its own pass works until the push cuts it
	pushWhenAllWork := func(f *fakeAgents, run store.Run, text string) error {
		e.waitBusy(e.pane(agents.RoleCodexReview))
		e.waitRun(agents.RoleJudge, target, store.RunWorking)
		f.mu.Lock()
		f.hangs[agents.RoleJudge] = false // the restart's prompts end as scripted
		f.mu.Unlock()
		e.push(head2)
		return nil // at work until the push cuts it
	}
	e.ag.behaviors[agents.RoleClaude] = []behavior{pushWhenAllWork, writeReport("## P2 on the new head\n")}
	post := e.judgePosts(901, "COMMENTED", "COMMENT")
	post.commit = head2
	// The cut own pass takes the first behavior (hangs: never run).
	e.ag.behaviors[agents.RoleJudge] = []behavior{hang(), writeOwn(), post.behavior(t)}

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.Restarts != 1 || res.TargetSHA != head2 {
		t.Fatalf("result = %+v", res)
	}
	want := []string{
		"agent:" + e.agent(agents.RoleClaude) + ":esc",
		"pane:" + e.pane(agents.RoleCodexReview) + ":ctrl+c",
		"agent:" + e.agent(agents.RoleJudge) + ":ctrl+c",
	}
	sends := e.keySends()
	if len(sends) != len(want) {
		t.Fatalf("keys = %+v, want %v", sends, want)
	}
	for i, s := range sends {
		if s.to != want[i] {
			t.Errorf("key %d = %s, want %s", i, s.to, want[i])
		}
		if !s.at.Equal(sends[0].at) {
			t.Errorf("%s went out %s after the cut's first key", s.to, s.at.Sub(sends[0].at))
		}
	}
	if len(stopAt) != 1 || stopAt[0].Before(sends[len(sends)-1].at) {
		t.Errorf("stop message at %v, want one after every key (last at %v)", stopAt, sends[len(sends)-1].at)
	}
	if !slices.Contains(res.Warnings, "interrupted claude-review for the restart; asked it to stop the 2 background tasks it started: 2 still run after 2 minutes") {
		t.Errorf("warnings = %q", res.Warnings)
	}
	for _, role := range []agents.Role{agents.RoleClaude, agents.RoleCodexReview} {
		for _, r := range e.runsOf(role, store.RunInitial) {
			if r.TargetSHA == target {
				wantRun(t, r, store.RunAbandoned, ReportHeadMoved)
			}
		}
	}
	wantRun(t, e.runsOf(agents.RoleJudge, store.RunOwnPass)[0], store.RunAbandoned, ReportHeadMoved)
	if rep := res.Reports[agents.RoleCodexReview]; rep.Status != ReportOK {
		t.Errorf("codex-review on the new head = %+v", rep)
	}
}

// codexRefusesWhenAllWork makes codex-review's command end on Codex's
// refusal once claude-review and the judge's own pass both work.
func (e *env) codexRefusesWhenAllWork() {
	e.ag.exitStatus[agents.RoleCodexReview] = 1
	e.ag.codex = func(f *fakeAgents, c codexCall) error {
		e.waitRun(agents.RoleClaude, target, store.RunWorking)
		e.waitRun(agents.RoleJudge, target, store.RunWorking)
		return os.WriteFile(filepath.Join(e.reportDir(), "codex-review.md"),
			[]byte("ERROR: This content was flagged for possible cybersecurity risk.\n"), 0o600)
	}
}

// A refusal of codex-review while claude-review and the judge's own pass
// work: the turns of the refused kind (Codex) are interrupted first, so the
// own pass gets its two ctrl+c (the round is over: they quit Codex) at the
// cut, before claude-review's esc and before the wait for claude-review,
// which never goes idle, runs its InterruptWait.
func TestARefusalInterruptsTheRefusedKindsTurnsFirst(t *testing.T) {
	e := newEnv(t)
	e.codexRefusesWhenAllWork()
	e.ag.hangs = map[agents.Role]bool{agents.RoleClaude: true, agents.RoleJudge: true} // at work until the round cancels them

	res, err := e.r.RunRound(e.ctx, e.ownInput(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	wantRefused(t, res, string(agents.RoleCodexReview), e.runOf(agents.RoleCodexReview, store.RunInitial).ID)
	judge, claude := "agent:"+e.agent(agents.RoleJudge)+":", "agent:"+e.agent(agents.RoleClaude)+":"
	want := []string{judge + "ctrl+c", judge + "ctrl+c", claude + "esc"}
	sends := e.keySends()
	if len(sends) != len(want) {
		t.Fatalf("keys = %+v, want %v", sends, want)
	}
	for i, s := range sends {
		if s.to != want[i] {
			t.Errorf("key %d = %s, want %s", i, s.to, want[i])
		}
		if d := s.at.Sub(sends[0].at); d > 5*time.Second {
			t.Errorf("%s went out %s after the cut's first key, want within seconds", s.to, d)
		}
	}
	own := e.runOf(agents.RoleJudge, store.RunOwnPass)
	wantRun(t, own, store.RunFailed, ReportCancelled)
	mustContain(t, "own pass error", store.Deref(own.Error), "the round ended before the own pass did", "Codex refused the review")
	wantRun(t, e.runOf(agents.RoleClaude, store.RunInitial), store.RunAbandoned, ReportCancelled)
	for _, w := range []string{
		"codex-judge still works 1m0s after it was interrupted for the round's end",
		"claude-review still works 1m0s after it was interrupted for the round's end",
	} {
		if !slices.Contains(res.Warnings, w) {
			t.Errorf("warnings = %q, want %q", res.Warnings, w)
		}
	}
}

// A push cuts codex-review while its command runs: the command ignores the
// interrupt's ctrl+c (Codex may ask for a second), so after shellStopWait it
// gets another and stops, and the restart's line finds an idle shell: the
// restarted round has codex-review's report. A command that ignores all
// shellStopPresses presses is named in a round.warning, and the restart's
// line is refused as busy, as before. The presses do not wait for
// claude-review's background work to stop (2 minutes here): shell roles are
// settled first.
func TestACutShellCommandIsPressedUntilItStops(t *testing.T) {
	for _, tc := range []struct {
		name    string
		presses int // the ctrl+c that stops the command
		keys    int
		warn    bool
		status  string
	}{
		{"stops at the second ctrl+c", 2, 2, false, ReportOK},
		{"ignores three", 4, shellStopPresses, true, ReportBusy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			in := e.input(KindInitial)
			sw := e.withRestarts(&in, 2)
			e.codexUntilCut(sw, tc.presses, head2)
			e.ag.background = map[agents.Role]int{agents.RoleClaude: 1} // never stops
			var stopAt []time.Time
			e.ag.onTimeUp = func(f *fakeAgents, run store.Run, text string) error {
				stopAt = append(stopAt, e.clock.Now())
				return nil
			}
			atPush := func(f *fakeAgents, run store.Run, text string) error {
				e.waitHead(head2)
				return nil // its wait notices the push and cuts the stage
			}
			e.ag.behaviors[agents.RoleClaude] = []behavior{atPush, writeReport("## P2 on the new head\n")}
			post := e.judgePosts(902, "COMMENTED", "COMMENT")
			post.commit = head2
			e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(t)}

			res, err := e.r.RunRound(e.ctx, in)
			if err != nil {
				t.Fatalf("RunRound: %v", err)
			}
			if res.Outcome != OutcomePosted || res.Restarts != 1 {
				t.Fatalf("result = %+v", res)
			}
			var presses []string
			for _, s := range e.keySends() {
				if strings.HasPrefix(s.to, "pane:"+e.pane(agents.RoleCodexReview)+":") {
					presses = append(presses, s.to)
					if len(stopAt) == 0 || s.at.After(stopAt[0]) {
						t.Errorf("%s at %v, want it no later than claude-review's stop message (at %v)", s.to, s.at, stopAt)
					}
				}
			}
			if len(presses) != tc.keys {
				t.Errorf("codex-review's pane got %v, want %d ctrl+c", presses, tc.keys)
			}
			if rep := res.Reports[agents.RoleCodexReview]; rep.Status != tc.status {
				t.Errorf("codex-review on the new head = %+v, want %s", rep, tc.status)
			}
			warned := false
			for _, ev := range e.eventsOf("round.warning") {
				if strings.Contains(ev.Message, "codex-review still runs") {
					warned = true
					mustContain(t, "warning", ev.Message, fmt.Sprintf("after %d ctrl+c", shellStopPresses), "for the restart")
				}
			}
			if warned != tc.warn {
				t.Errorf("warning about codex-review = %t, want %t: %q", warned, tc.warn, res.Warnings)
			}
		})
	}
}

// A push and a refusal of codex-review in one stage: the push cancels the
// stage first, and codex-review's command, which ended as it was cut, ends
// on Codex's refusal. The refusal wins: the round ends refused, claude-review
// is stopped as at any round's end, and nothing is prompted again on the new
// head.
func TestARefusalThatRacesAPushEndsTheRound(t *testing.T) {
	e := newEnv(t)
	in := e.input(KindInitial)
	sw := e.withRestarts(&in, 2)
	e.ag.exitStatus[agents.RoleCodexReview] = 1
	e.ag.codex = func(f *fakeAgents, c codexCall) error {
		select {
		case <-c.Ctx.Done(): // the command ends as the push cuts the stage
		case <-time.After(10 * time.Second):
			return fmt.Errorf("fake codex-review: never cut")
		}
		return os.WriteFile(filepath.Join(e.reportDir(), "codex-review.md"),
			[]byte("ERROR: This content was flagged for possible cybersecurity risk.\n"), 0o600)
	}
	pushWhenCodexRuns := func(f *fakeAgents, run store.Run, text string) error {
		e.waitRun(agents.RoleCodexReview, target, store.RunSubmitted)
		e.push(head2)
		return nil // its wait notices the push and cuts the stage
	}
	e.ag.behaviors[agents.RoleClaude] = []behavior{pushWhenCodexRuns, writeReport("## P2 on the new head\n")}
	post := e.judgePosts(903, "COMMENTED", "COMMENT")
	post.commit = head2
	e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(t)}

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	run := e.runOf(agents.RoleCodexReview, store.RunInitial)
	wantRefused(t, res, string(agents.RoleCodexReview), run.ID)
	if res.Restarts != 0 || len(sw.asked()) != 0 {
		t.Errorf("restarts = %d, switches %v, want none", res.Restarts, sw.asked())
	}
	if want := []string{"shell:codex-review", "submit:claude-review"}; !slices.Equal(slices.Sorted(slices.Values(e.ag.order)), want) {
		t.Errorf("prompts = %v, want only the first ones", e.ag.order)
	}
	wantRun(t, e.runOf(agents.RoleClaude, store.RunInitial), store.RunAbandoned, ReportCancelled)
	if len(e.eventsOf("round.restarted")) != 0 {
		t.Error("the round restarted")
	}
}
