package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// miniCodexSID is the Codex session id miniHerdr reports for its judge.
const miniCodexSID = "01a0fe66-uuid"

// ownPassOnManager is a round's agents whose judge's own pass runs on the
// real agents.Manager (miniHerdr's one agent), every other turn on the
// fakes.
type ownPassOnManager struct {
	*fakeAgents
	m *agents.Manager
}

func (a ownPassOnManager) Submit(ctx context.Context, run store.Run, text string) error {
	if run.Kind != store.RunOwnPass {
		return a.fakeAgents.Submit(ctx, run, text)
	}
	a.noteMark(agents.Role(run.Role), text)
	return a.m.Submit(ctx, run, text)
}

// fullSnapshot is h's snapshot with every other session of the round's
// panes and agents, idle, so an observation marks none of them lost.
func (e *env) fullSnapshot(h *miniHerdr) herdr.Snapshot {
	snap := h.snapshot()
	for _, s := range e.sess {
		pane := store.Deref(s.HerdrPaneID)
		if pane == h.pane {
			continue
		}
		snap.Panes = append(snap.Panes, herdr.Pane{ID: pane, WorkspaceID: "w1"})
		if name := store.Deref(s.AgentName); name != "" {
			snap.Agents = append(snap.Agents, herdr.AgentInfo{Name: name, PaneID: pane, WorkspaceID: "w1",
				Agent: store.Deref(s.AgentKind), AgentStatus: herdr.StatusIdle})
		}
	}
	return snap
}

// codexRollout writes the judge's Codex rollout under home, its events as
// JSON lines, last written at the clock's now.
func (e *env) codexRollout(home string, events ...map[string]any) {
	e.t.Helper()
	dir := filepath.Join(home, "sessions", "2026", "10", "03")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		e.t.Fatal(err)
	}
	var b strings.Builder
	for _, ev := range events {
		raw, err := json.Marshal(ev)
		if err != nil {
			e.t.Fatal(err)
		}
		b.Write(append(raw, '\n'))
	}
	path := filepath.Join(dir, "rollout-2026-10-03T09-00-00-"+miniCodexSID+".jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		e.t.Fatal(err)
	}
	now := e.clock.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		e.t.Fatal(err)
	}
}

func rolloutEvent(at time.Time, typ string) map[string]any {
	return map[string]any{"timestamp": at.UTC().Format("2006-01-02T15:04:05.000Z"), "type": "event_msg",
		"payload": map[string]any{"type": typ}}
}

// The 2026-10-22 case: herdr showed the judge idle a tick into its own
// pass while Codex still worked, its rollout growing, and two idle ticks
// ended the run before the judge wrote judge-own.md ("finished without
// writing judge-own.md"). With the rollout read, the run stays open while
// the rollout grows without a task_complete since the prompt; the judge
// then writes judge-own.md and completes its turn, and the own pass is
// verified.
func TestAnOwnPassWhosePaneGoesIdleWhileItsRolloutGrowsIsNotFailed(t *testing.T) {
	e := newEnv(t)
	judge := e.sess[agents.RoleJudge]
	h := &miniHerdr{name: store.Deref(judge.AgentName), pane: store.Deref(judge.HerdrPaneID), status: herdr.StatusIdle}
	run := &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"codex", "login", "status"}, Result: execx.Result{Stdout: []byte("Logged in using ChatGPT\n")}},
	}}
	home := t.TempDir()
	m := agents.New(agents.Deps{Herdr: h, Store: e.st, Runner: run, Config: e.cfg, Layout: e.layout, Clock: e.clock.Now, CodexHome: home,
		Sleep: noAgentSleep})
	e.r.Agents = ownPassOnManager{fakeAgents: e.ag, m: m}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(801, "CHANGES_REQUESTED", "REQUEST_CHANGES").behavior(t)}

	var mu sync.Mutex
	ticks := 0
	var prompted time.Time
	e.r.Sleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		e.clock.Add(d)
		h.mu.Lock()
		sent := len(h.prompts) > 0
		h.mu.Unlock()
		if !sent {
			return nil
		}
		ticks++
		now := e.clock.Now()
		switch {
		case ticks == 1:
			prompted = now.Add(-d)
		case ticks <= 6: // herdr shows the agent idle while Codex works on
			h.setStatus(herdr.StatusIdle)
			e.codexRollout(home, rolloutEvent(prompted, "user_message"), rolloutEvent(now, "agent_message"))
		case ticks == 7: // the own pass is written, the turn completes
			own := filepath.Join(e.reportDir(), agents.OwnFindingsFile)
			body := "## Own pass\n- [P2] app/models/order.rb:42 retries twice\n"
			if id := e.ag.markOf(agents.RoleJudge); id != "" {
				body = agents.ReportMarker(id) + "\n" + body
			}
			if err := os.WriteFile(own, []byte(body), 0o600); err != nil {
				t.Errorf("write the own pass: %v", err)
			}
			e.codexRollout(home, rolloutEvent(prompted, "user_message"), rolloutEvent(now, "agent_message"), rolloutEvent(now, "task_complete"))
		}
		if _, err := m.ObserveSnapshot(ctx, e.fullSnapshot(h)); err != nil {
			t.Errorf("ObserveSnapshot: %v", err)
		}
		return nil
	}

	res, err := e.r.RunRound(e.ctx, e.ownInput(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	own := e.runOf(agents.RoleJudge, store.RunOwnPass)
	if res.OwnPass == nil || res.OwnPass.Status != ReportOK {
		t.Fatalf("own pass = %+v (run %+v), want ok", res.OwnPass, own)
	}
	wantRun(t, own, store.RunVerified, ReportOK)
	if res.Outcome != OutcomePosted || res.ReviewID != 801 {
		t.Fatalf("result = %+v", res)
	}
	if ticks < 8 {
		t.Errorf("ticks = %d: the own pass ended before its task_complete", ticks)
	}
}

// The role's timeout still ends a judge turn whose rollout keeps growing
// while herdr shows it idle: the rollout keeps the run open, never past the
// timeout.
func TestTheJudgesTimeoutEndsATurnWhoseRolloutKeepsGrowing(t *testing.T) {
	e := newEnv(t)
	for i := range e.cfg.Roles {
		if e.cfg.Roles[i].Judge {
			e.cfg.Roles[i].Timeout = config.Duration{Duration: 3 * time.Minute}
		}
	}
	judge := e.sess[agents.RoleJudge]
	h := &miniHerdr{name: store.Deref(judge.AgentName), pane: store.Deref(judge.HerdrPaneID), status: herdr.StatusIdle}
	run := &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"codex", "login", "status"}, Result: execx.Result{Stdout: []byte("Logged in using ChatGPT\n")}},
	}}
	home := t.TempDir()
	m := agents.New(agents.Deps{Herdr: h, Store: e.st, Runner: run, Config: e.cfg, Layout: e.layout, Clock: e.clock.Now, CodexHome: home,
		Sleep: noAgentSleep})
	e.r.Agents = m
	start := e.clock.Now()
	ticks := 0
	e.r.Sleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		e.clock.Add(d)
		ticks++
		if ticks > 1 {
			h.setStatus(herdr.StatusIdle)
			e.codexRollout(home, rolloutEvent(start, "user_message"), rolloutEvent(e.clock.Now(), "agent_message"))
		}
		if _, err := m.ObserveSnapshot(ctx, e.fullSnapshot(h)); err != nil {
			t.Errorf("ObserveSnapshot: %v", err)
		}
		return nil
	}

	res, _ := e.r.RunRound(e.ctx, e.input(KindContinue))
	if res.Outcome != OutcomeTimeout {
		t.Fatalf("outcome = %s, want %s", res.Outcome, OutcomeTimeout)
	}
	if r := e.runOf(agents.RoleJudge, store.RunContinue); r.State == store.RunEnded || r.State == store.RunVerified {
		t.Fatalf("judge run = %s: it ended on herdr's idle while its rollout grew", r.State)
	}
}
