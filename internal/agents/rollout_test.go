package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

const codexSID = "01a0fbf8-0c6b-7841-aee0-c7e77d668b42"

// codexRollout writes a Codex session's rollout file under home, its lines
// as Codex 0.160 writes them.
func codexRollout(t *testing.T, home string, started time.Time, lines ...map[string]any) {
	t.Helper()
	codexRolloutOf(t, home, codexSID, started, lines...)
}

// codexRolloutOf writes the rollout of Codex session (or helper thread) sid.
func codexRolloutOf(t *testing.T, home, sid string, started time.Time, lines ...map[string]any) {
	t.Helper()
	dir := filepath.Join(home, "sessions", started.Format("2006"), started.Format("01"), started.Format("02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, l := range lines {
		raw, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(append(raw, '\n'))
	}
	name := "rollout-" + started.Format("2006-01-02T15-04-05") + "-" + sid + ".jsonl"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func taskComplete(at time.Time, errMsg, info string) map[string]any {
	payload := map[string]any{"type": "task_complete", "turn_id": "t1", "last_agent_message": nil}
	if errMsg != "" {
		payload["error"] = map[string]any{"message": errMsg, "codex_error_info": info}
	}
	return map[string]any{"timestamp": at.UTC().Format("2006-01-02T15:04:05.000Z"), "type": "event_msg", "payload": payload}
}

// codexRun is a run of role in a live codex session whose Codex session id is
// codexSID, created at the clock's now.
func (e *env) codexRun(role Role, env map[string]string) store.Run {
	e.t.Helper()
	s, err := e.st.CreateSession(e.ctx, store.Session{PRID: e.pr.ID, Role: string(role), AgentName: new("magnum-" + string(role)),
		AgentKind: new(KindCodex), SessionID: new(codexSID), HerdrPaneID: new("p1"), Env: env,
		State: store.SessionLive, StartedAt: e.clock.Now().Add(-time.Hour)})
	if err != nil {
		e.t.Fatal(err)
	}
	run, err := e.m.NewRun(e.ctx, e.pr, role, store.RunInitial, 1)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.TransitionRun(e.ctx, run.ID, []string{store.RunPending}, store.RunSubmitted, func(u *store.RunUpdate) {
		u.Set("session_id", s.ID)
	}); err != nil {
		e.t.Fatal(err)
	}
	run, err = e.st.RunByID(e.ctx, run.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return run
}

// When the pane scrolled past the refusal, the turn's error is read from
// the session's Codex rollout: the last task_complete since the run was
// created, with its message and codex_error_info. An error of an earlier
// turn, a turn that ended without one, a missing rollout and a session of
// another kind give nothing.
func TestTurnErrorReadsTheCodexRollout(t *testing.T) {
	e := newEnv(t)
	home := t.TempDir()
	e.m.d.CodexHome = home
	run := e.codexRun(RoleJudge, nil)
	if _, ok := e.m.TurnError(e.ctx, run); ok {
		t.Fatal("a turn error without a rollout")
	}
	now := e.clock.Now()
	refusal := "This content was flagged for possible cybersecurity risk. If this seems wrong, try rephrasing your request."
	codexRollout(t, home, now.Add(-24*time.Hour),
		taskComplete(now.Add(-time.Hour), "an earlier turn's error", "other"),
		map[string]any{"timestamp": now.Add(time.Minute).UTC().Format(time.RFC3339), "type": "event_msg", "payload": map[string]any{"type": "token_count"}},
		taskComplete(now.Add(7*time.Minute), refusal, CodexCyberPolicy))
	te, ok := e.m.TurnError(e.ctx, run)
	if !ok || te.Info != CodexCyberPolicy || !strings.Contains(te.Message, "flagged for possible cybersecurity risk") {
		t.Fatalf("TurnError = %+v, %v", te, ok)
	}

	codexRollout(t, home, now.Add(-24*time.Hour),
		taskComplete(now.Add(-time.Hour), refusal, CodexCyberPolicy),
		taskComplete(now.Add(5*time.Minute), "", ""))
	if te, ok := e.m.TurnError(e.ctx, run); ok {
		t.Fatalf("a turn that ended cleanly after an earlier refusal: %+v", te)
	}

	// The session's own CODEX_HOME wins over the manager's.
	other := t.TempDir()
	run = e.codexRun(RoleClaude, map[string]string{"CODEX_HOME": other})
	codexRollout(t, other, now, taskComplete(now.Add(time.Minute), refusal, CodexCyberPolicy))
	if _, ok := e.m.TurnError(e.ctx, run); !ok {
		t.Fatal("the session's CODEX_HOME was not read")
	}
}

// helperRollout writes the rollout of a helper agent (a Codex sub-agent)
// whose session_meta names parent, its other lines after it, last written
// at its last line's time.
func helperRollout(t *testing.T, home, id, parent string, started time.Time, lines ...map[string]any) {
	t.Helper()
	meta := map[string]any{"timestamp": started.UTC().Format(time.RFC3339), "type": "session_meta", "payload": map[string]any{
		"id": id, "session_id": parent, "parent_thread_id": parent, "thread_source": "subagent",
		"source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent, "depth": 1}}}}}
	lines = append([]map[string]any{meta}, lines...)
	codexRolloutOf(t, home, id, started, lines...)
	last, err := time.Parse(time.RFC3339Nano, lines[len(lines)-1]["timestamp"].(string))
	if err != nil {
		t.Fatal(err)
	}
	path, err := rolloutIn(home, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, last, last); err != nil {
		t.Fatal(err)
	}
}

// subAgentActivity is the event the parent's rollout records when it starts
// (kind "started") or messages ("interacted") the helper agent thread.
func subAgentActivity(at time.Time, kind, thread string) map[string]any {
	return map[string]any{"timestamp": at.UTC().Format("2006-01-02T15:04:05.000Z"), "type": "event_msg", "payload": map[string]any{
		"type": "item_completed", "thread_id": codexSID, "turn_id": "t1",
		"item": map[string]any{"type": "SubAgentActivity", "id": "call_1", "kind": kind, "agent_thread_id": thread, "agent_path": "/root/qa"}}}
}

// A helper agent the judge spawned runs in a rollout of its own: its
// refused turn left the judge's turn ending ok (a PR on 10-06, flagged by
// hand),
// and the helper went on to a turn that ended ok. TurnError reads the turns
// the session's helpers ended during the run, from the rollout that names
// the session as its parent, one started by an earlier turn too: a
// helper's refusal comes before the judge's own other error. A helper's
// error before the run and a rollout naming another parent give nothing.
func TestTurnErrorReadsTheRefusalOfAHelperAgent(t *testing.T) {
	e := newEnv(t)
	home := t.TempDir()
	e.m.d.CodexHome = home
	run := e.codexRun(RoleJudge, nil)
	now := e.clock.Now()
	refusal := "This content was flagged for possible cybersecurity risk. If this seems wrong, try rephrasing your request."
	const (
		earlier  = "01a0fbf8-0000-7000-8000-000000000001" // started by an earlier run
		qa       = "01a0fbf8-0000-7000-8000-000000000002"
		stranger = "01a0fbf8-0000-7000-8000-000000000003" // its rollout names another parent
	)
	parent := func(own map[string]any) {
		codexRollout(t, home, now.Add(-2*time.Hour),
			subAgentActivity(now.Add(-time.Hour), "started", earlier),
			subAgentActivity(now.Add(time.Minute), "started", stranger),
			subAgentActivity(now.Add(2*time.Minute), "started", qa),
			subAgentActivity(now.Add(3*time.Minute), "interacted", qa),
			own)
	}
	helperRollout(t, home, earlier, codexSID, now.Add(-time.Hour), taskComplete(now.Add(-30*time.Minute), refusal, CodexCyberPolicy))
	helperRollout(t, home, stranger, "01a0fbf8-ffff-7000-8000-00000000000f", now.Add(time.Minute),
		taskComplete(now.Add(4*time.Minute), refusal, CodexCyberPolicy))
	helperRollout(t, home, qa, codexSID, now.Add(2*time.Minute),
		taskComplete(now.Add(-time.Hour), "an earlier turn's error", "other"),
		taskComplete(now.Add(5*time.Minute), refusal, CodexCyberPolicy),
		taskComplete(now.Add(6*time.Minute), "", ""))

	parent(taskComplete(now.Add(8*time.Minute), "", ""))
	te, ok := e.m.TurnError(e.ctx, run)
	if !ok || te.Info != CodexCyberPolicy || te.Helper != qa || !strings.Contains(te.Message, "cybersecurity risk") {
		t.Fatalf("TurnError = %+v, %v: want the refusal of helper %s", te, ok, qa)
	}
	parent(taskComplete(now.Add(8*time.Minute), "unexpected status 404 Not Found", "other"))
	if te, ok := e.m.TurnError(e.ctx, run); !ok || te.Helper != qa || te.Info != CodexCyberPolicy {
		t.Fatalf("TurnError = %+v, %v: a helper's refusal comes before the session's other error", te, ok)
	}

	helperRollout(t, home, qa, codexSID, now.Add(2*time.Minute), taskComplete(now.Add(-time.Hour), refusal, CodexCyberPolicy),
		taskComplete(now.Add(5*time.Minute), "", ""))
	parent(taskComplete(now.Add(8*time.Minute), "", ""))
	if te, ok := e.m.TurnError(e.ctx, run); ok {
		t.Fatalf("TurnError = %+v: no helper turn of the run ended in an error", te)
	}

	// A helper an earlier turn started, still at work in this one.
	helperRollout(t, home, earlier, codexSID, now.Add(-time.Hour), taskComplete(now.Add(-30*time.Minute), "", ""),
		taskComplete(now.Add(4*time.Minute), refusal, CodexCyberPolicy))
	if te, ok := e.m.TurnError(e.ctx, run); !ok || te.Helper != earlier || te.Info != CodexCyberPolicy {
		t.Fatalf("TurnError = %+v, %v: want the refusal of helper %s, started before the run", te, ok, earlier)
	}
}
