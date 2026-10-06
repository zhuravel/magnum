package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// Transcript fixtures shaped like Claude Code's session JSONL: one entry per
// line, an assistant entry per content block, a user entry per tool result
// with the tool's toolUseResult, and a <task-notification> user message when
// background work finishes. Paths and ids are placeholders.

const (
	claudeSID = "0a1b2c3d-0000-4000-8000-00000000c1a0"
	// claudeProject is the project directory Claude Code names for the
	// checkout the test workspace uses (/Users/x/Projects/talkable.review1).
	claudeProject = "-Users-x-Projects-talkable-review1"
)

// tline is one transcript entry.
type tline map[string]any

func stamp(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05.000Z") }

func toolUse(at time.Time, id, name string, input map[string]any) tline {
	return tline{"type": "assistant", "isSidechain": false, "timestamp": stamp(at), "sessionId": claudeSID,
		"message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}}}}
}

func toolResult(at time.Time, id, text string, result map[string]any, isError bool) tline {
	block := map[string]any{"tool_use_id": id, "type": "tool_result", "content": text}
	if isError {
		block["is_error"] = true
	}
	return tline{"type": "user", "isSidechain": false, "timestamp": stamp(at), "sessionId": claudeSID,
		"message": map[string]any{"role": "user", "content": []any{block}}, "toolUseResult": result}
}

func assistantSays(at time.Time, text string) tline {
	return tline{"type": "assistant", "isSidechain": false, "timestamp": stamp(at), "sessionId": claudeSID,
		"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}}
}

func turnEnded(at time.Time) tline {
	return tline{"type": "system", "subtype": "turn_duration", "timestamp": stamp(at), "sessionId": claudeSID}
}

func userPrompt(at time.Time, text string) tline {
	return tline{"type": "user", "isSidechain": false, "timestamp": stamp(at), "sessionId": claudeSID,
		"message": map[string]any{"role": "user", "content": text}}
}

func notificationText(toolUseID, taskID, status string) string {
	return fmt.Sprintf("<task-notification>\n<task-id>%s</task-id>\n<tool-use-id>%s</tool-use-id>\n"+
		"<output-file>/tmp/claude/tasks/%s.output</output-file>\n<status>%s</status>\n"+
		"<summary>Background command \"Run specs\" %s</summary>\n</task-notification>", taskID, toolUseID, taskID, status, status)
}

// notification is the queue entry and the user message Claude Code writes
// when background work finishes while the agent is idle.
func notification(at time.Time, toolUseID, taskID, status string) []tline {
	text := notificationText(toolUseID, taskID, status)
	return []tline{
		{"type": "queue-operation", "operation": "enqueue", "timestamp": stamp(at), "sessionId": claudeSID, "content": text},
		{"type": "queue-operation", "operation": "dequeue", "timestamp": stamp(at), "sessionId": claudeSID},
		{"type": "user", "isSidechain": false, "timestamp": stamp(at), "sessionId": claudeSID,
			"message": map[string]any{"role": "user", "content": text},
			"origin":  map[string]any{"kind": "task-notification", "producer": "session-task"}},
	}
}

// Launches of background work, each as its tool use and its result.

func bashInBackground(at time.Time, id, task string) []tline {
	return []tline{
		toolUse(at, id, "Bash", map[string]any{"command": "bin/rspec spec/models", "description": "Run specs", "run_in_background": true}),
		toolResult(at.Add(time.Second), id, "Command running in background with ID: "+task+".",
			map[string]any{"stdout": "", "stderr": "", "interrupted": false, "backgroundTaskId": task}, false),
	}
}

func bashTimedOut(at time.Time, id, task string) []tline {
	return []tline{
		toolUse(at, id, "Bash", map[string]any{"command": "bin/rspec spec", "description": "Run all specs", "timeout": 600000}),
		toolResult(at.Add(time.Second), id, "Command did not complete within its 600s timeout and was moved to the background (ID: "+task+").",
			map[string]any{"stdout": "", "stderr": "", "interrupted": false, "backgroundTaskId": task, "timedOutAfterMs": 600000}, false),
	}
}

func agentAsync(at time.Time, id, task string) []tline {
	return []tline{
		toolUse(at, id, "Agent", map[string]any{"description": "Check the callers", "prompt": "Find the callers", "run_in_background": true}),
		toolResult(at.Add(time.Second), id, "Async agent launched.",
			map[string]any{"isAsync": true, "status": "async_launched", "agentId": task, "outputFile": "/tmp/claude/tasks/" + task + ".output"}, false),
	}
}

func skillForked(at time.Time, id, task string) []tline {
	return []tline{
		toolUse(at, id, "Skill", map[string]any{"skill": "code-review", "args": "https://github.com/talkable/talkable/pull/11920 high"}),
		toolResult(at.Add(time.Second), id, "Skill \"code-review\" launched (forked execution, running in the background).",
			map[string]any{"success": true, "commandName": "code-review", "status": "forked", "background": true, "agentId": task}, false),
	}
}

func taskStopped(at time.Time, id, task string) []tline {
	return []tline{
		toolUse(at, id, "TaskStop", map[string]any{"task_id": task}),
		toolResult(at.Add(time.Second), id, `{"message":"Successfully stopped task: `+task+`"}`,
			map[string]any{"message": "Successfully stopped task: " + task, "task_id": task, "task_type": "local_bash"}, false),
	}
}

// transcript is a session's transcript file under a Claude config dir.
type transcript struct {
	t    *testing.T
	path string
}

// claudeTranscript points the manager at a fresh Claude config dir and
// returns the transcript file of the claude-review session there.
func (e *env) claudeTranscript() *transcript {
	e.t.Helper()
	dir := e.t.TempDir()
	e.m.d.ClaudeDir = dir
	return &transcript{t: e.t, path: filepath.Join(dir, "projects", claudeProject, claudeSID+".jsonl")}
}

func (tr *transcript) add(lines ...tline) {
	tr.t.Helper()
	if err := os.MkdirAll(filepath.Dir(tr.path), 0o700); err != nil {
		tr.t.Fatal(err)
	}
	f, err := os.OpenFile(tr.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		tr.t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		b, err := json.Marshal(l)
		if err != nil {
			tr.t.Fatal(err)
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			tr.t.Fatal(err)
		}
	}
}

func (tr *transcript) addAll(groups ...[]tline) {
	tr.t.Helper()
	for _, g := range groups {
		tr.add(g...)
	}
}

// promptClaude starts the agents, gives the claude agent its session id and
// prompts it; it returns the run id.
func (e *env) promptClaude() string {
	e.t.Helper()
	e.started()
	e.h.setAgentSession(claudeAgent, claudeSID)
	e.clock.Add(3 * time.Minute)
	id, err := e.m.Prompt(e.ctx, e.pr, RoleClaude, store.RunInitial, "/code-review https://github.com/talkable/talkable/pull/11920 high")
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// idleTicks reports the claude agent idle and runs n ticks 30 s apart,
// returning the last observation.
func (e *env) idleTicks(n int) Observation {
	e.t.Helper()
	e.h.setAgentStatus(claudeAgent, herdr.StatusIdle)
	var o Observation
	for range n {
		e.clock.Add(30 * time.Second)
		o = e.observe()[RoleClaude]
	}
	return o
}

// The live case: Claude Code ended its turn while a spec run it started in
// the background was still running, herdr showed the agent idle, and two
// idle ticks ended the run without a report. Background work of every
// shape holds the run open until its notification arrives (or a TaskStop
// ends it) and the agent answers; then two idle ticks end it as before.
func TestObserveHoldsAnIdleClaudeAgentWhileItsBackgroundWorkRuns(t *testing.T) {
	for _, tc := range []struct {
		name   string
		launch func(at time.Time) []tline
		finish func(at time.Time) []tline
	}{
		{"a Bash command run in the background",
			func(at time.Time) []tline { return bashInBackground(at, "toolu_bash1", "bg1") },
			func(at time.Time) []tline { return notification(at, "toolu_bash1", "bg1", "completed") }},
		{"a Bash command its timeout moved to the background",
			func(at time.Time) []tline { return bashTimedOut(at, "toolu_bash2", "bg2") },
			func(at time.Time) []tline { return notification(at, "toolu_bash2", "bg2", "completed") }},
		{"a subagent launched asynchronously",
			func(at time.Time) []tline { return agentAsync(at, "toolu_agent1", "a1f00d") },
			func(at time.Time) []tline { return notification(at, "toolu_agent1", "a1f00d", "completed") }},
		{"a skill forked into the background",
			func(at time.Time) []tline { return skillForked(at, "toolu_skill1", "a2f00d") },
			func(at time.Time) []tline { return notification(at, "toolu_skill1", "a2f00d", "completed") }},
		{"a failed background command",
			func(at time.Time) []tline { return bashInBackground(at, "toolu_bash3", "bg3") },
			func(at time.Time) []tline { return notification(at, "toolu_bash3", "bg3", "failed") }},
		{"a background command stopped with TaskStop",
			func(at time.Time) []tline { return bashInBackground(at, "toolu_bash4", "bg4") },
			func(at time.Time) []tline { return taskStopped(at, "toolu_stop1", "bg4") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			tr := e.claudeTranscript()
			id := e.promptClaude()
			now := e.clock.Now()
			tr.add(userPrompt(now, "/code-review https://github.com/talkable/talkable/pull/11920 high"))
			tr.addAll(tc.launch(now.Add(5 * time.Second)))
			tr.add(assistantSays(now.Add(10*time.Second), "Waiting for the spec run."), turnEnded(now.Add(10*time.Second)))

			for tick := 1; tick <= 4; tick++ {
				o := e.idleTicks(1)
				if o.Kind != "" || o.Background != 1 || o.Session.IdleTicks != 0 {
					t.Fatalf("idle tick %d with background work = kind %q background %d idle_ticks %d, want held",
						tick, o.Kind, o.Background, o.Session.IdleTicks)
				}
			}
			if r := e.run1(id); r.State != store.RunWorking {
				t.Fatalf("run = %s, want still working", r.State)
			}
			evs := e.eventsOf(EventBackgroundWait)
			if len(evs) != 1 || evs[0].Message != "claude-review: waiting for 1 background task" {
				t.Fatalf("%s events: %+v, want one", EventBackgroundWait, evs)
			}

			done := e.clock.Now()
			tr.addAll(tc.finish(done))
			tr.add(assistantSays(done.Add(2*time.Second), "Specs pass. Writing the report."), turnEnded(done.Add(3*time.Second)))
			if o := e.idleTicks(1); o.Kind != "" || o.Background != 0 || o.Session.IdleTicks != 1 {
				t.Fatalf("first idle tick after the work finished = kind %q background %d idle_ticks %d", o.Kind, o.Background, o.Session.IdleTicks)
			}
			if o := e.idleTicks(1); o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != id {
				t.Fatalf("second idle tick = %+v, want completed", o)
			}
			if evs := e.eventsOf(EventBackgroundWait); len(evs) != 1 {
				t.Fatalf("%s events: %+v, want still one", EventBackgroundWait, evs)
			}
		})
	}
}

// The notification resumes the agent's turn: until the agent answers it the
// run is held, so it never ends in the instant between the notification and
// the agent's next step.
func TestObserveHoldsAnIdleClaudeAgentUntilItAnswersANotification(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	id := e.promptClaude()
	now := e.clock.Now()
	tr.add(userPrompt(now, "review"))
	tr.addAll(bashInBackground(now.Add(time.Second), "toolu_bash1", "bg1"))
	tr.add(turnEnded(now.Add(2 * time.Second)))
	e.idleTicks(1)

	tr.addAll(notification(e.clock.Now(), "toolu_bash1", "bg1", "completed"))
	for tick := 1; tick <= 3; tick++ {
		if o := e.idleTicks(1); o.Kind != "" || o.Background != 0 || o.Session.IdleTicks != 0 {
			t.Fatalf("idle tick %d before the answer = kind %q background %d idle_ticks %d, want held", tick, o.Kind, o.Background, o.Session.IdleTicks)
		}
	}
	tr.add(assistantSays(e.clock.Now(), "Done."), turnEnded(e.clock.Now()))
	if o := e.idleTicks(2); o.Kind != ObsCompleted || o.Run.ID != id {
		t.Fatalf("after the answer = %+v, want completed", o)
	}
}

// Without background work, or without a transcript magnum can read, an idle
// claude agent ends its run on two idle ticks as before.
func TestObserveEndsAClaudeRunAsBeforeWithoutBackgroundWork(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(tr *transcript, now time.Time)
	}{
		{"a turn with foreground tools only", func(tr *transcript, now time.Time) {
			tr.add(userPrompt(now, "review"),
				toolUse(now.Add(time.Second), "toolu_fg1", "Bash", map[string]any{"command": "git diff", "description": "Diff"}),
				toolResult(now.Add(2*time.Second), "toolu_fg1", "diff", map[string]any{"stdout": "diff", "stderr": "", "interrupted": false}, false),
				assistantSays(now.Add(3*time.Second), "Report written."), turnEnded(now.Add(3*time.Second)))
		}},
		{"a background launch that failed", func(tr *transcript, now time.Time) {
			tr.add(userPrompt(now, "review"),
				toolUse(now.Add(time.Second), "toolu_bad", "Bash", map[string]any{"command": "x", "run_in_background": true}),
				toolResult(now.Add(2*time.Second), "toolu_bad", "Error: permission denied", nil, true),
				assistantSays(now.Add(3*time.Second), "Report written."), turnEnded(now.Add(3*time.Second)))
		}},
		{"background work started before the run", func(tr *transcript, now time.Time) {
			tr.addAll(bashInBackground(now.Add(-10*time.Minute), "toolu_old", "bgold"))
			tr.add(userPrompt(now, "review"), assistantSays(now.Add(3*time.Second), "Report written."), turnEnded(now.Add(3*time.Second)))
		}},
		{"background work of a subagent's own (a sidechain entry)", func(tr *transcript, now time.Time) {
			side := toolUse(now.Add(time.Second), "toolu_side", "Bash", map[string]any{"command": "x", "run_in_background": true})
			side["isSidechain"] = true
			tr.add(userPrompt(now, "review"), side, assistantSays(now.Add(3*time.Second), "Report written."), turnEnded(now.Add(3*time.Second)))
		}},
		{"no transcript", func(tr *transcript, now time.Time) {}},
		{"a transcript that is not JSON", func(tr *transcript, now time.Time) {
			if err := os.MkdirAll(filepath.Dir(tr.path), 0o700); err != nil {
				tr.t.Fatal(err)
			}
			if err := os.WriteFile(tr.path, []byte("not json\n{\"type\":\n"), 0o600); err != nil {
				tr.t.Fatal(err)
			}
		}},
		{"a directory where the transcript should be", func(tr *transcript, now time.Time) {
			if err := os.MkdirAll(tr.path, 0o700); err != nil {
				tr.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			tr := e.claudeTranscript()
			id := e.promptClaude()
			tc.write(tr, e.clock.Now())
			if o := e.idleTicks(1); o.Kind != "" || o.Session.IdleTicks != 1 {
				t.Fatalf("first idle tick = kind %q idle_ticks %d", o.Kind, o.Session.IdleTicks)
			}
			if o := e.idleTicks(1); o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != id {
				t.Fatalf("second idle tick = %+v, want completed", o)
			}
			if evs := e.eventsOf(EventBackgroundWait); len(evs) != 0 {
				t.Fatalf("%s events: %+v", EventBackgroundWait, evs)
			}
		})
	}
}

// A codex agent's run ends on herdr's status alone: only Claude Code
// resumes a turn by itself when background work finishes.
func TestObserveReadsNoTranscriptForACodexAgent(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	tr.addAll(bashInBackground(t0.Add(4*time.Minute), "toolu_bash1", "bg1")) // would hold a claude agent
	e.started()
	const judge = "mg-11920-codex-judge-5d01cf"
	e.h.setAgentSession(judge, claudeSID)
	e.clock.Add(3 * time.Minute)
	id, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "review")
	if err != nil {
		t.Fatal(err)
	}
	e.h.setAgentStatus(judge, herdr.StatusIdle)
	var o Observation
	for range 2 {
		e.clock.Add(30 * time.Second)
		o = e.observe()[RoleJudge]
	}
	if o.Kind != ObsCompleted || o.Run.ID != id {
		t.Fatalf("codex judge = %+v, want completed", o)
	}
}

// Claude Code keeps a session's transcript under the config dir its pane
// sets (CLAUDE_CONFIG_DIR in the role's env), and names the project
// directory after the checkout; a directory named otherwise (Claude Code
// shortens long paths) is found by the session id.
func TestObserveFindsTheTranscriptWhereClaudeCodeKeepsIt(t *testing.T) {
	t.Run("the role's CLAUDE_CONFIG_DIR", func(t *testing.T) {
		e := newEnv(t)
		e.claudeTranscript() // the default dir holds nothing
		own := t.TempDir()
		for i := range e.cfg.Roles {
			if e.cfg.Roles[i].Name == string(RoleClaude) {
				e.cfg.Roles[i].Env = map[string]string{"CLAUDE_CONFIG_DIR": own}
			}
		}
		tr := &transcript{t: t, path: filepath.Join(own, "projects", claudeProject, claudeSID+".jsonl")}
		e.promptClaude()
		tr.addAll(bashInBackground(e.clock.Now(), "toolu_bash1", "bg1"))
		if o := e.idleTicks(3); o.Kind != "" || o.Background != 1 {
			t.Fatalf("observation = kind %q background %d, want held by the transcript in the role's config dir", o.Kind, o.Background)
		}
	})
	t.Run("a project directory named otherwise", func(t *testing.T) {
		e := newEnv(t)
		tr := e.claudeTranscript()
		tr.path = filepath.Join(filepath.Dir(filepath.Dir(tr.path)), "-Users-x-Projects-talkable-review1-3f9a2c", claudeSID+".jsonl")
		e.promptClaude()
		tr.addAll(bashInBackground(e.clock.Now(), "toolu_bash1", "bg1"))
		if o := e.idleTicks(3); o.Kind != "" || o.Background != 1 {
			t.Fatalf("observation = kind %q background %d, want held", o.Kind, o.Background)
		}
	})
}

// Only the run's part of a long transcript is read: the reader starts at
// the run and reads what was appended since on later ticks, so a session
// with hours of history costs its tail.
func TestTranscriptStartSkipsWhatPrecedesTheRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	tr := &transcript{t: t, path: path}
	since := t0.Add(time.Hour)
	var old []tline
	for i := range 3000 {
		old = append(old, assistantSays(t0.Add(time.Duration(i)*time.Second), strings.Repeat("x", 200)))
	}
	tr.add(old...)
	tr.add(tline{"type": "custom-title", "customTitle": "PR #11920 claude-review", "sessionId": claudeSID}) // no timestamp
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	cut := st.Size()
	tr.add(userPrompt(since, "review"), assistantSays(since.Add(time.Second), "ok"))
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	end, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	off, err := transcriptStart(f, end.Size(), since)
	if err != nil {
		t.Fatal(err)
	}
	// The offset is past every line stamped before since; the untimed title
	// line between them may or may not be skipped.
	before := cut - int64(len(mustJSON(t, tline{"type": "custom-title", "customTitle": "PR #11920 claude-review", "sessionId": claudeSID}))+1)
	if off != before && off != cut {
		t.Fatalf("transcriptStart = %d, want %d or %d (file %d bytes)", off, before, cut, end.Size())
	}
	if off, err := transcriptStart(f, end.Size(), t0.Add(-time.Hour)); err != nil || off != 0 {
		t.Fatalf("transcriptStart before everything = %d, %v; want 0", off, err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TimeUp is the pipeline's last call to a reviewer whose time ran out: the
// text reaches the agent within its run, and from then on the agent's
// background work no longer holds the run open (it was told to stop waiting
// for it), so its run ends once it is idle again.
func TestTimeUpStopsHoldingTheRunForBackgroundWork(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	id := e.promptClaude()
	tr.addAll(bashInBackground(e.clock.Now(), "toolu_bash1", "bg1"))
	if o := e.idleTicks(3); o.Kind != "" || o.Background != 1 {
		t.Fatalf("before TimeUp = kind %q background %d, want held", o.Kind, o.Background)
	}
	if n, ok := e.m.BackgroundTasks(e.ctx, e.run1(id)); !ok || n != 1 {
		t.Fatalf("BackgroundTasks = %d, %v; want 1", n, ok)
	}

	const text = "Time is up: write the report now."
	if err := e.m.TimeUp(e.ctx, e.run1(id), text); err != nil {
		t.Fatalf("TimeUp: %v", err)
	}
	e.h.mu.Lock()
	last := e.h.prompts[len(e.h.prompts)-1]
	e.h.mu.Unlock()
	if last.Target != claudeAgent || last.Text != text || last.Wait != nil {
		t.Fatalf("prompt = %+v, want the text to the claude agent", last)
	}
	if s := e.session(RoleClaude); s.IdleTicks != 0 || s.LastPromptAt == nil || !s.LastPromptAt.Equal(e.clock.Now()) {
		t.Fatalf("session after TimeUp: idle_ticks %d last_prompt_at %v", s.IdleTicks, s.LastPromptAt)
	}
	if r := e.run1(id); r.State != store.RunWorking {
		t.Fatalf("run = %s, want the same run still in flight", r.State)
	}
	tr.add(assistantSays(e.clock.Now(), "Report written; specs still running are listed as pending."), turnEnded(e.clock.Now()))
	if o := e.idleTicks(2); o.Kind != ObsCompleted || o.Run.ID != id || o.Background != 1 {
		t.Fatalf("after TimeUp = %+v, want completed with the task still listed", o)
	}
	if n, ok := e.m.BackgroundTasks(e.ctx, e.run1(id)); !ok || n != 1 {
		t.Fatalf("BackgroundTasks after the run = %d, %v; want the task it left running", n, ok)
	}
}

// TimeUp refuses a run whose session is gone.
func TestTimeUpNeedsALiveAgent(t *testing.T) {
	e := newEnv(t)
	id := e.promptClaude()
	run := e.run1(id)
	if err := e.st.TransitionSession(e.ctx, *run.SessionID, []string{store.SessionLive}, store.SessionLost, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.m.TimeUp(e.ctx, run, "x"); err == nil || !strings.Contains(err.Error(), ErrNoSession.Error()) {
		t.Fatalf("TimeUp on a lost session = %v, want %v", err, ErrNoSession)
	}
	if n, ok := e.m.BackgroundTasks(e.ctx, store.Run{ID: "r-none"}); ok || n != 0 {
		t.Fatalf("BackgroundTasks without a session = %d, %v", n, ok)
	}
}

// The time-up text ends with Enter, so it is never typed into a dialog: a
// permission prompt on screen refuses it, and the agent's background work
// keeps holding the run.
func TestTimeUpIsNeverTypedIntoADialog(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	id := e.promptClaude()
	tr.addAll(bashInBackground(e.clock.Now(), "toolu_bash1", "bg1"))
	e.h.mu.Lock()
	e.h.reads[claudeAgent] = claudeRmPrompt
	sent := len(e.h.prompts)
	e.h.mu.Unlock()
	if err := e.m.TimeUp(e.ctx, e.run1(id), "Time is up."); !errors.Is(err, ErrBlocked) {
		t.Fatalf("TimeUp over a permission prompt = %v, want %v", err, ErrBlocked)
	}
	e.h.mu.Lock()
	n := len(e.h.prompts)
	e.h.mu.Unlock()
	if n != sent {
		t.Fatalf("prompts sent: %d, want none", n-sent)
	}
	if o := e.idleTicks(3); o.Kind != "" || o.Background != 1 {
		t.Fatalf("after a refused TimeUp = kind %q background %d, want still held", o.Kind, o.Background)
	}
}
