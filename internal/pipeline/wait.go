package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// waitKind is how waiting for an agent turn ended.
type waitKind int

const (
	waitEnded     waitKind = iota // the run ended (idle on two ticks)
	waitResult                    // the judge's result file holds a final status, or settled while the agent stayed busy
	waitFailed                    // the run failed or was abandoned (by Submit or externally)
	waitLost                      // the session's pane or agent disappeared
	waitTimeout                   // the role's timeout passed
	waitRefused                   // Submit refused before sending (no session, human active)
	waitCancelled                 // ctx ended
)

// turn is the end of one prompted agent turn.
type turn struct {
	kind waitKind
	run  store.Run // latest row
	// err is Submit's error (kept also when the text was sent and the run is
	// observed) or the wait's own error.
	err error
	// unsent: Submit failed before the agent got the text, so there is nothing
	// to verify.
	unsent bool
}

// submitAndWait sends text for run and waits for the turn to end. A run that
// Submit left pending (refused) is abandoned here. (Submit itself answers a
// first-launch trust dialog that rejected the prompt and re-sends once.)
func (rd *round) submitAndWait(ctx context.Context, run store.Run, text string, timeout time.Duration, resultFile string, ids map[string]bool) turn {
	serr := rd.r.Agents.Submit(ctx, run, text)
	if serr != nil {
		cur, err := rd.r.Store.RunByID(context.WithoutCancel(ctx), run.ID)
		if ctx.Err() != nil {
			// The prompt may have arrived before ctx ended (Submit then
			// records the run as submitted).
			unsent := err == nil && (cur.State == store.RunPending || cur.State == store.RunFailed || cur.State == store.RunAbandoned)
			return turn{kind: waitCancelled, run: run, err: ctx.Err(), unsent: unsent}
		}
		if err != nil {
			return turn{kind: waitFailed, run: run, err: errors.Join(serr, err), unsent: true}
		}
		switch cur.State {
		case store.RunPending:
			rd.finishRun(ctx, cur.ID, store.RunAbandoned, refusedStatus(serr), serr.Error())
			cur, _ = rd.r.Store.RunByID(context.WithoutCancel(ctx), run.ID)
			return turn{kind: waitRefused, run: cur, err: serr, unsent: true}
		case store.RunFailed, store.RunAbandoned:
			return turn{kind: waitFailed, run: cur, err: serr, unsent: true}
		}
		// Submitted (stalled, timeout, acked as blocked): the text may be in
		// the agent, so the run is observed, never re-sent.
		rd.warn(ctx, "%s run %s: prompt not acknowledged as working: %v", run.Role, run.ID, serr)
	}
	t := rd.wait(ctx, run.ID, timeout, resultFile, ids)
	if t.err == nil {
		t.err = serr
	}
	return t
}

// wait polls run until it ends, its session is lost, the result file holds a
// final status (finalStatus) or, with another status, settles for
// ResultSettle (judge only; a file that does not parse, or names another run
// id, does not count), or timeout passes.
func (rd *round) wait(ctx context.Context, runID string, timeout time.Duration, resultFile string, ids map[string]bool) turn {
	deadline := rd.r.now().Add(timeout)
	var fileSeen time.Time
	var run store.Run
	for {
		var err error
		run, err = rd.r.Store.RunByID(ctx, runID)
		if err != nil {
			if ctx.Err() != nil {
				return turn{kind: waitCancelled, run: run, err: ctx.Err()}
			}
			return turn{kind: waitFailed, run: run, err: fmt.Errorf("pipeline: run %s: %w", runID, err)}
		}
		switch run.State {
		case store.RunEnded, store.RunVerified:
			return turn{kind: waitEnded, run: run}
		case store.RunFailed, store.RunAbandoned:
			return turn{kind: waitFailed, run: run, err: fmt.Errorf("pipeline: %s run %s is %s: %s", run.Role, run.ID, run.State, store.Deref(run.Error))}
		}
		if run.SessionID != nil {
			s, err := rd.r.Store.SessionByID(ctx, *run.SessionID)
			if err == nil && s.State != store.SessionLive && s.State != store.SessionStarting {
				return turn{kind: waitLost, run: run, err: fmt.Errorf("pipeline: %s session %d is %s", run.Role, s.ID, s.State)}
			}
		}
		now := rd.r.now()
		if resultFile != "" {
			if res, ok := readResultFile(resultFile); ok && (res.RunID == "" || ids[res.RunID]) {
				if finalStatus(res.Status) {
					// The judge's last step: the agent may still print its
					// final message, which stays the run's (agents turnTail).
					return turn{kind: waitResult, run: run}
				}
				if fileSeen.IsZero() {
					fileSeen = now
				}
				if now.Sub(fileSeen) >= ResultSettle {
					return turn{kind: waitResult, run: run}
				}
			}
		}
		if timeout > 0 && !now.Before(deadline) {
			return turn{kind: waitTimeout, run: run, err: fmt.Errorf("pipeline: %s run %s: no result after %s", run.Role, run.ID, timeout)}
		}
		rd.noticePush(ctx) // a reviewer's wait: a push cancels ctx, so the sleep below ends the wait
		if err := rd.r.sleep(ctx, rd.r.poll()); err != nil {
			return turn{kind: waitCancelled, run: run, err: err}
		}
	}
}

func refusedStatus(err error) string {
	switch {
	case errors.Is(err, agents.ErrHumanActive):
		return ReportHumanActive
	case errors.Is(err, agents.ErrNoSession):
		return ReportNoSession
	}
	return ReportFailed
}

// finishRun moves a run to its final state (verified, failed or abandoned)
// with outcome and error. A run already final keeps its state; outcome and
// error are still recorded.
func (rd *round) finishRun(ctx context.Context, id, to, outcome, errMsg string, extra ...func(*store.RunUpdate)) {
	ctx = context.WithoutCancel(ctx)
	now := rd.r.now()
	cur, err := rd.r.Store.RunByID(ctx, id)
	if err != nil {
		rd.logf("pipeline: finish run %s: %v", id, err)
		return
	}
	set := func(u *store.RunUpdate) {
		u.Set("outcome", outcome)
		if errMsg != "" {
			u.Set("error", execx.Redact(errMsg))
		}
		if cur.EndedAt == nil {
			u.Set("ended_at", now)
		}
		if to == store.RunVerified {
			u.Set("verified_at", now)
		}
		for _, f := range extra {
			f(u)
		}
	}
	from := []string{store.RunPending, store.RunSubmitted, store.RunWorking, store.RunEnded}
	if to == store.RunVerified {
		from = []string{store.RunSubmitted, store.RunWorking, store.RunEnded}
	}
	err = rd.r.Store.TransitionRun(ctx, id, from, to, set)
	if errors.Is(err, store.ErrConflict) {
		err = rd.r.Store.UpdateRun(ctx, id, func(u *store.RunUpdate) {
			u.Set("outcome", outcome)
			if errMsg != "" {
				u.Set("error", execx.Redact(errMsg))
			}
		})
	}
	if err != nil {
		rd.logf("pipeline: finish run %s as %s: %v", id, to, err)
	}
}

// session returns a run's session row (zero when it has none).
func (rd *round) session(ctx context.Context, run store.Run) (store.Session, bool) {
	if run.SessionID == nil {
		return store.Session{}, false
	}
	s, err := rd.r.Store.SessionByID(context.WithoutCancel(ctx), *run.SessionID)
	if err != nil {
		return store.Session{}, false
	}
	return s, true
}

// interrupt stops the turn in flight on run: esc for an agent, which keeps
// its session, and ctrl+c for a shell role's pane. The judge gets ctrl+c
// twice, a second apart, which quits Codex and so ends its session
// (docs/spikes.md, probe 5): a judge that timed out or whose round ended
// must not go on and post (stopJudge, and cut at the round's end). With keep
// it gets ctrl+c once, which aborts Codex's turn and keeps the session: a
// push cut its own pass short, and the restart prompts the same session,
// with the context it built, on the new head (restart). A cut shell role's
// command is then made sure to stop (stopShell).
func (rd *round) interrupt(ctx context.Context, role config.Role, run store.Run, keep bool) {
	if rd.r.Keys == nil {
		return
	}
	s, ok := rd.session(ctx, run)
	if !ok {
		return
	}
	ctx = context.WithoutCancel(ctx)
	var err error
	if role.IsAgent() {
		target := store.Deref(s.AgentName)
		if target == "" {
			target = store.Deref(s.HerdrPaneID)
		}
		if target == "" {
			return
		}
		presses := []string{"esc"}
		switch {
		case role.Judge && keep:
			presses = []string{"ctrl+c"}
		case role.Judge:
			presses = []string{"ctrl+c", "ctrl+c"}
		}
		for i, key := range presses {
			if i > 0 {
				_ = rd.r.sleep(ctx, time.Second)
			}
			if err = rd.r.Keys.AgentSendKeys(ctx, target, key); err != nil {
				break
			}
		}
	} else if pane := store.Deref(s.HerdrPaneID); pane != "" {
		err = rd.r.Keys.PaneSendKeys(ctx, pane, "ctrl+c")
	}
	if err != nil {
		rd.warn(ctx, "interrupt %s: %v", role.Name, err)
	}
}

// waitIdle waits up to InterruptWait for the agent of run's session to be
// seen idle or done (the engine's Observe records agent statuses), or the
// session to be no longer live. It reports whether it was; a shell role's
// pane (no agent status) and a run without a session report true.
func (rd *round) waitIdle(ctx context.Context, role config.Role, run store.Run) bool {
	if !role.IsAgent() {
		return true
	}
	ctx = context.WithoutCancel(ctx)
	deadline := rd.r.now().Add(InterruptWait)
	for {
		s, ok := rd.session(ctx, run)
		if !ok || (s.State != store.SessionLive && s.State != store.SessionStarting) {
			return true
		}
		switch herdr.Status(store.Deref(s.AgentStatus)) {
		case herdr.StatusIdle, herdr.StatusDone:
			return true
		}
		if !rd.r.now().Before(deadline) || rd.r.sleep(ctx, rd.r.poll()) != nil {
			return false
		}
	}
}

// stopShell makes sure that the command a shell role ran for run in its
// pane stopped after its interrupt (one ctrl+c): it waits up to
// shellStopWait for an idle shell (Keys.WaitIdleShell) and presses ctrl+c
// again while the command runs, shellStopPresses in all (a command may go
// on after one). Left running, it would hold the pane: the restart's line
// would find it busy. It reports false when the command still runs after
// the last press; true when the shell is idle or nothing says otherwise (no
// Keys or pane, an error that is no timeout: a pane that is gone).
func (rd *round) stopShell(ctx context.Context, run store.Run) bool {
	s, ok := rd.session(ctx, run)
	pane := store.Deref(s.HerdrPaneID)
	if rd.r.Keys == nil || !ok || pane == "" {
		return true
	}
	ctx = context.WithoutCancel(ctx)
	for press := 1; ; press++ {
		_, err := rd.r.Keys.WaitIdleShell(ctx, pane, shellStopWait)
		switch {
		case err == nil:
			return true
		case !herdr.IsTimeout(err):
			rd.logf("pipeline: %s: wait for an idle shell in pane %s: %v", run.Role, pane, err)
			return true
		case press == shellStopPresses:
			return false
		}
		if err := rd.r.Keys.PaneSendKeys(ctx, pane, "ctrl+c"); err != nil {
			rd.warn(ctx, "interrupt %s: %v", run.Role, err)
			return true
		}
	}
}

// classifyAfter classifies the pane text that follows the last occurrence
// of anchor (this turn's prompt, or a shell line's agents.CommandAnchor), so
// errors from earlier turns in the same pane, including an earlier run on
// the same head with the same report path, are ignored. Only the last
// agents.HealthLines lines count; kind's health patterns apply (see
// classify).
func (rd *round) classifyAfter(kind, text, anchor string) agents.Health {
	if anchor != "" {
		if i := strings.LastIndex(text, anchor); i >= 0 {
			text = text[i+len(anchor):]
		}
	}
	return rd.classify(kind, tailLines(text, agents.HealthLines))
}

// classify classifies pane text with the health patterns of an agent kind
// ([kinds.<kind>] health_patterns), the defaults for "" or an undeclared
// kind.
func (rd *round) classify(kind, text string) agents.Health {
	now := rd.r.now()
	if rx := rd.patterns(kind); rx != nil {
		return agents.ClassifyWith(*rx, text, now)
	}
	return agents.ClassifyAt(text, now)
}

// patterns compiles kind's health patterns once per round (nil = use the
// defaults).
func (rd *round) patterns(kind string) *config.HealthRegexps {
	if kind == "" || rd.r.Config == nil {
		return nil
	}
	rd.mu.Lock()
	defer rd.mu.Unlock()
	if rx, ok := rd.health[kind]; ok {
		return rx
	}
	var out *config.HealthRegexps
	if k, ok := rd.r.Config.KindSpec(kind); ok {
		if rx, err := k.HealthPatterns.Compile(); err == nil {
			out = &rx
		}
	}
	if rd.health == nil {
		rd.health = map[string]*config.HealthRegexps{}
	}
	rd.health[kind] = out
	return out
}

func tailLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	idx := len(s)
	for i := 0; i < n; i++ {
		j := strings.LastIndexByte(s[:idx], '\n')
		if j < 0 {
			return s
		}
		idx = j
	}
	return s[idx+1:]
}
