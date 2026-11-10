package agents

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Claude Code's in-process teammates: an Agent tool use with a name (and
// run_in_background) spawns one, and its result says teammate_spawned. A
// teammate sends no <task-notification>: it writes its own transcript at
// <the lead's transcript dir>/<lead session id>/subagents/agent-<agentId>.jsonl
// (entries with isSidechain and agentId), ends each turn with an end_turn
// assistant entry and idles until a mailbox message wakes it. Ids and names
// are placeholders.

const (
	mateA = "adrafts-0123456789abcdef"
	mateB = "aui-fedcba9876543210"
)

// teammateSpawned is the lead's Agent tool use that spawns teammate agentID
// and its result.
func teammateSpawned(at time.Time, id, agentID string) []tline {
	use := toolUse(at, id, "Agent", map[string]any{"description": "Draft integration", "name": "drafts",
		"model": "opus", "prompt": "Build the drafts part", "run_in_background": true})
	res := toolResult(at.Add(time.Second), id, "", map[string]any{"status": "teammate_spawned",
		"agentId": agentID, "agent_id": agentID, "name": "drafts", "teammate_id": "drafts@session-0a1b2c3d",
		"team_name": "session-0a1b2c3d", "tmux_pane_id": "in-process", "model": "opus", "prompt": "Build the drafts part"}, false)
	res["message"].(map[string]any)["content"].([]any)[0].(map[string]any)["content"] = []any{map[string]any{"type": "text",
		"text": "Spawned successfully.\nagent_id: " + agentID + "\nname: drafts\nThe agent is now running and will receive instructions via mailbox."}}
	return []tline{use, res}
}

// teammate is the transcript of the lead's teammate agentID, beside tr.
func (tr *transcript) teammate(agentID string) *transcript {
	return &transcript{t: tr.t, path: filepath.Join(filepath.Dir(tr.path), claudeSID, "subagents", "agent-"+agentID+".jsonl")}
}

// mate marks l as an entry of teammate agentID's own transcript.
func mate(agentID string, l tline) tline {
	l["isSidechain"], l["agentId"] = true, agentID
	return l
}

// stopped sets the stop_reason of assistant entry l.
func stopped(l tline, reason any) tline {
	l["message"].(map[string]any)["stop_reason"] = reason
	return l
}

// mateMail is a message from the lead that wakes teammate agentID.
func mateMail(at time.Time, agentID string) tline {
	return mate(agentID, userPrompt(at, `<teammate-message teammate_id="team-lead" summary="Next step">`+"\nBuild the next part.\n</teammate-message>"))
}

// mateWorks is a tool use of teammate agentID and its result: a turn in
// progress, then an attachment Claude Code adds after the result.
func mateWorks(at time.Time, agentID, id string) []tline {
	return []tline{
		mate(agentID, stopped(toolUse(at, id, "Bash", map[string]any{"command": "bin/rspec", "description": "Run specs"}), "tool_use")),
		mate(agentID, toolResult(at.Add(time.Second), id, "12 examples, 0 failures", map[string]any{"stdout": "12 examples, 0 failures"}, false)),
		mate(agentID, tline{"type": "attachment", "timestamp": stamp(at.Add(time.Second)), "attachment": map[string]any{"type": "output_style"}}),
	}
}

// mateDone is teammate agentID's last answer of a turn (end_turn) and the
// entries Claude Code writes after it.
func mateDone(at time.Time, agentID string) []tline {
	return []tline{
		mate(agentID, stopped(assistantSays(at, "The drafts part is done; I sent the summary to the lead."), "end_turn")),
		mate(agentID, tline{"type": "attachment", "timestamp": stamp(at), "attachment": map[string]any{"type": "hook_success", "hookName": "Stop"}}),
		mate(agentID, tline{"type": "system", "subtype": "stop_hook_summary", "timestamp": stamp(at)}),
		{"type": "api-request", "timestamp": stamp(at), "agentId": agentID, "sessionId": claudeSID},
		{"type": "file-history-snapshot", "agentId": agentID},
	}
}

// leadIdle is the lead's own end of the turn after it spawned its teammates.
func leadIdle(tr *transcript, at time.Time) {
	tr.add(assistantSays(at, "The teammates build the rest."), turnEnded(at))
}

// The live case: a factory writer spawned two teammates, both ended their
// last turn long before the writer ended its own, and the writer's run
// stayed held until the watchdog's nudge, since nothing ever finished a
// teammate. A teammate whose own transcript shows its turn ended holds
// nothing, whatever Claude Code wrote after the end_turn: two idle readings
// end the run as before.
func TestObserveIsNotHeldByATeammateWhoseTurnEnded(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	id := e.promptClaude()
	now := e.clock.Now()
	tr.add(userPrompt(now, "build it"))
	tr.addAll(teammateSpawned(now.Add(time.Second), "toolu_mate1", mateA))
	mt := tr.teammate(mateA)
	mt.add(mateMail(now.Add(2*time.Second), mateA))
	mt.addAll(mateWorks(now.Add(3*time.Second), mateA, "toolu_m1"), mateDone(now.Add(5*time.Second), mateA))
	leadIdle(tr, now.Add(6*time.Second))

	if o := e.idleTicks(1); o.Kind != "" || o.Background != 0 || o.Session.IdleTicks != 1 {
		t.Fatalf("first idle tick = kind %q background %d idle_ticks %d, want an idle reading", o.Kind, o.Background, o.Session.IdleTicks)
	}
	if o := e.idleTicks(1); o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != id {
		t.Fatalf("second idle tick = %+v, want completed", o)
	}
	if evs := e.eventsOf(EventBackgroundWait); len(evs) != 0 {
		t.Fatalf("%s events: %+v, want none", EventBackgroundWait, evs)
	}
}

// A teammate in the middle of a turn (its last user or assistant entry a
// tool result) holds the lead's idle run, and lets go once its own
// transcript shows the turn ended: the lead's transcript gains nothing then,
// so each reading looks at the teammate's file again.
func TestObserveHoldsTheRunWhileATeammateWorks(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	id := e.promptClaude()
	now := e.clock.Now()
	tr.add(userPrompt(now, "build it"))
	tr.addAll(teammateSpawned(now.Add(time.Second), "toolu_mate1", mateA))
	mt := tr.teammate(mateA)
	mt.add(mateMail(now.Add(2*time.Second), mateA))
	mt.addAll(mateWorks(now.Add(3*time.Second), mateA, "toolu_m1"))
	leadIdle(tr, now.Add(6*time.Second))

	for tick := 1; tick <= 4; tick++ {
		if o := e.idleTicks(1); o.Kind != "" || o.Background != 1 || o.Session.IdleTicks != 0 {
			t.Fatalf("idle tick %d while the teammate works = kind %q background %d idle_ticks %d, want held",
				tick, o.Kind, o.Background, o.Session.IdleTicks)
		}
	}
	if evs := e.eventsOf(EventBackgroundWait); len(evs) != 1 || evs[0].Message != "claude-review: waiting for 1 background task" {
		t.Fatalf("%s events: %+v, want one", EventBackgroundWait, evs)
	}

	mt.addAll(mateDone(e.clock.Now(), mateA))
	if o := e.idleTicks(1); o.Kind != "" || o.Background != 0 || o.Session.IdleTicks != 1 {
		t.Fatalf("first idle tick after the teammate's end_turn = kind %q background %d idle_ticks %d", o.Kind, o.Background, o.Session.IdleTicks)
	}
	if o := e.idleTicks(1); o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != id {
		t.Fatalf("second idle tick = %+v, want completed", o)
	}
}

// The event counts teammates at work with the background tasks, in the
// same words, and no teammate whose turn ended.
func TestBackgroundWaitCountsTeammatesAtWorkWithTheTasks(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	e.promptClaude()
	now := e.clock.Now()
	tr.add(userPrompt(now, "build it"))
	tr.addAll(bashInBackground(now.Add(time.Second), "toolu_bash1", "bg1"),
		teammateSpawned(now.Add(2*time.Second), "toolu_mate1", mateA), teammateSpawned(now.Add(3*time.Second), "toolu_mate2", mateB))
	tr.teammate(mateA).addAll(mateWorks(now.Add(4*time.Second), mateA, "toolu_m1"))
	tr.teammate(mateB).addAll(mateWorks(now.Add(4*time.Second), mateB, "toolu_m2"), mateDone(now.Add(5*time.Second), mateB))
	leadIdle(tr, now.Add(6*time.Second))

	if o := e.idleTicks(2); o.Kind != "" || o.Background != 2 {
		t.Fatalf("observation = kind %q background %d, want held by 2", o.Kind, o.Background)
	}
	if evs := e.eventsOf(EventBackgroundWait); len(evs) != 1 || evs[0].Message != "claude-review: waiting for 2 background tasks" {
		t.Fatalf("%s events: %+v, want one for 2 tasks", EventBackgroundWait, evs)
	}
}

// A teammate's transcript may appear after its spawn: a missing one holds
// the run within teammateGrace of the spawn's result, and nothing after; one
// that appears late holds the run while it shows a turn in progress.
func TestObserveHoldsTheRunForAMissingTeammateFileOnlyWithinItsGrace(t *testing.T) {
	t.Run("never written", func(t *testing.T) {
		e := newEnv(t)
		tr := e.claudeTranscript()
		id := e.promptClaude()
		now := e.clock.Now()
		tr.add(userPrompt(now, "build it"))
		tr.addAll(teammateSpawned(now.Add(9*time.Second), "toolu_mate1", mateA)) // its result at now+10s: grace to now+130s
		leadIdle(tr, now.Add(11*time.Second))

		for tick := 1; tick <= 4; tick++ { // now+30s .. now+120s
			if o := e.idleTicks(1); o.Kind != "" || o.Background != 1 || o.Session.IdleTicks != 0 {
				t.Fatalf("idle tick %d within the grace = kind %q background %d idle_ticks %d, want held",
					tick, o.Kind, o.Background, o.Session.IdleTicks)
			}
		}
		if o := e.idleTicks(1); o.Kind != "" || o.Background != 0 || o.Session.IdleTicks != 1 { // now+150s
			t.Fatalf("first idle tick past the grace = kind %q background %d idle_ticks %d", o.Kind, o.Background, o.Session.IdleTicks)
		}
		if o := e.idleTicks(1); o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != id {
			t.Fatalf("second idle tick past the grace = %+v, want completed", o)
		}
	})
	t.Run("written late", func(t *testing.T) {
		e := newEnv(t)
		tr := e.claudeTranscript()
		id := e.promptClaude()
		now := e.clock.Now()
		tr.add(userPrompt(now, "build it"))
		tr.addAll(teammateSpawned(now.Add(9*time.Second), "toolu_mate1", mateA))
		leadIdle(tr, now.Add(11*time.Second))
		if o := e.idleTicks(3); o.Kind != "" || o.Background != 1 {
			t.Fatalf("within the grace = kind %q background %d, want held", o.Kind, o.Background)
		}
		mt := tr.teammate(mateA)
		mt.add(mateMail(e.clock.Now(), mateA))
		mt.addAll(mateWorks(e.clock.Now(), mateA, "toolu_m1"))
		for tick := 1; tick <= 4; tick++ { // to now+210s, past the grace
			if o := e.idleTicks(1); o.Kind != "" || o.Background != 1 || o.Session.IdleTicks != 0 {
				t.Fatalf("idle tick %d with the teammate at work = kind %q background %d idle_ticks %d, want held",
					tick, o.Kind, o.Background, o.Session.IdleTicks)
			}
		}
		mt.addAll(mateDone(e.clock.Now(), mateA))
		if o := e.idleTicks(2); o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != id {
			t.Fatalf("after the teammate's end_turn = %+v, want completed", o)
		}
	})
}

// The agent id names a file under the lead's subagents directory, so it is
// taken only as one path segment: an id that is empty, "." or "..", or
// holds a slash, a backslash or a NUL holds nothing, and no file it would
// name is read (each such file here shows a turn in progress).
func TestObserveATeammateWhoseAgentIDIsNoFileNameHoldsNothing(t *testing.T) {
	for _, agentID := range []string{"../../../planted", "drafts/../" + mateA, "", ".", "..", `drafts\x`, "drafts\x00x"} {
		t.Run(fmt.Sprintf("%q", agentID), func(t *testing.T) {
			e := newEnv(t)
			tr := e.claudeTranscript()
			id := e.promptClaude()
			now := e.clock.Now()
			if !strings.Contains(agentID, "\x00") {
				planted := tr.teammate(agentID)
				planted.add(mateMail(now, mateA))
				planted.addAll(mateWorks(now, mateA, "toolu_m1"))
			}
			tr.add(userPrompt(now, "build it"))
			tr.addAll(teammateSpawned(now.Add(time.Second), "toolu_mate1", agentID))
			leadIdle(tr, now.Add(2*time.Second))

			if o := e.idleTicks(1); o.Kind != "" || o.Background != 0 || o.Session.IdleTicks != 1 {
				t.Fatalf("first idle tick = kind %q background %d idle_ticks %d, want an idle reading", o.Kind, o.Background, o.Session.IdleTicks)
			}
			if o := e.idleTicks(1); o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != id {
				t.Fatalf("second idle tick = %+v, want completed", o)
			}
		})
	}
}

// A teammate transcript magnum cannot read, or that holds no user or
// assistant entry it can parse, holds nothing.
func TestObserveATeammateFileItCannotReadHoldsNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(mt *transcript)
	}{
		{"a directory where the file should be", func(mt *transcript) {
			if err := os.MkdirAll(mt.path, 0o700); err != nil {
				mt.t.Fatal(err)
			}
		}},
		{"a file that is not JSON", func(mt *transcript) {
			if err := os.MkdirAll(filepath.Dir(mt.path), 0o700); err != nil {
				mt.t.Fatal(err)
			}
			if err := os.WriteFile(mt.path, []byte("not json\n{\"type\": \"user\",\n"), 0o600); err != nil {
				mt.t.Fatal(err)
			}
		}},
		{"an empty file", func(mt *transcript) { mt.add() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			tr := e.claudeTranscript()
			id := e.promptClaude()
			now := e.clock.Now()
			tc.write(tr.teammate(mateA))
			tr.add(userPrompt(now, "build it"))
			tr.addAll(teammateSpawned(now.Add(time.Second), "toolu_mate1", mateA))
			leadIdle(tr, now.Add(2*time.Second))
			if o := e.idleTicks(2); o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != id {
				t.Fatalf("after two idle ticks = %+v, want completed", o)
			}
		})
	}
}

// BackgroundTasks, which the pipeline asks before it tells a reviewer to
// stop its background work, counts a teammate at work with the tasks and
// not one whose turn ended: the same number the observer holds the run on.
func TestBackgroundTasksCountsATeammateAtWorkAndNotAnIdleOne(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	id := e.promptClaude()
	now := e.clock.Now()
	tr.add(userPrompt(now, "build it"))
	tr.addAll(teammateSpawned(now.Add(time.Second), "toolu_mate1", mateA), teammateSpawned(now.Add(2*time.Second), "toolu_mate2", mateB))
	tr.teammate(mateA).addAll(mateWorks(now.Add(3*time.Second), mateA, "toolu_m1"))
	tr.teammate(mateB).addAll(mateWorks(now.Add(3*time.Second), mateB, "toolu_m2"), mateDone(now.Add(4*time.Second), mateB))
	leadIdle(tr, now.Add(5*time.Second))

	if n, ok := e.m.BackgroundTasks(e.ctx, e.run1(id)); !ok || n != 1 {
		t.Fatalf("BackgroundTasks = %d, %v; want 1 (the teammate at work)", n, ok)
	}
	if o := e.idleTicks(1); o.Kind != "" || o.Background != 1 {
		t.Fatalf("observation = kind %q background %d, want held by 1", o.Kind, o.Background)
	}
	tr.teammate(mateA).addAll(mateDone(e.clock.Now(), mateA))
	if n, ok := e.m.BackgroundTasks(e.ctx, e.run1(id)); !ok || n != 0 {
		t.Fatalf("BackgroundTasks after both ended their turn = %d, %v; want 0", n, ok)
	}
}

// A teammate whose turn ended wakes on the next message in its mailbox (a
// SendMessage from the lead or another teammate): from then on it holds the
// run again, until that turn ends too.
func TestObserveHoldsTheRunAgainWhenAnIdleTeammateWakes(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	id := e.promptClaude()
	now := e.clock.Now()
	tr.add(userPrompt(now, "build it"))
	tr.addAll(teammateSpawned(now.Add(time.Second), "toolu_mate1", mateA))
	mt := tr.teammate(mateA)
	mt.addAll(mateWorks(now.Add(2*time.Second), mateA, "toolu_m1"), mateDone(now.Add(3*time.Second), mateA))
	leadIdle(tr, now.Add(4*time.Second))

	if o := e.idleTicks(1); o.Kind != "" || o.Background != 0 || o.Session.IdleTicks != 1 {
		t.Fatalf("idle tick with the teammate idle = kind %q background %d idle_ticks %d, want an idle reading", o.Kind, o.Background, o.Session.IdleTicks)
	}
	mt.add(mateMail(e.clock.Now(), mateA))
	for tick := 1; tick <= 3; tick++ {
		if o := e.idleTicks(1); o.Kind != "" || o.Background != 1 || o.Session.IdleTicks != 0 {
			t.Fatalf("idle tick %d after the teammate woke = kind %q background %d idle_ticks %d, want held",
				tick, o.Kind, o.Background, o.Session.IdleTicks)
		}
	}
	mt.addAll(mateWorks(e.clock.Now(), mateA, "toolu_m2"), mateDone(e.clock.Now().Add(time.Second), mateA))
	if o := e.idleTicks(2); o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != id {
		t.Fatalf("after its second turn ended = %+v, want completed", o)
	}
}

// A teammate at work stops holding the run as background work does: a task
// notification or a TaskStop that names its agent id or its spawn's tool
// use (once the lead answered it), or Tell.
func TestObserveStopsHoldingForATeammateItsNotificationOrStopNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(e *env, tr *transcript, id string, at time.Time)
	}{
		{"a notification naming its spawn's tool use", func(e *env, tr *transcript, _ string, at time.Time) {
			tr.addAll(notification(at, "toolu_mate1", "", "completed"))
		}},
		{"a notification naming its agent id", func(e *env, tr *transcript, _ string, at time.Time) {
			tr.addAll(notification(at, "", mateA, "completed"))
		}},
		{"a TaskStop naming its agent id", func(e *env, tr *transcript, _ string, at time.Time) {
			tr.addAll(taskStopped(at, "toolu_stop1", mateA))
		}},
		{"Tell", func(e *env, tr *transcript, id string, at time.Time) {
			if err := e.m.Tell(e.ctx, e.run1(id), "Time is up: write the report now."); err != nil {
				e.t.Fatalf("Tell: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			tr := e.claudeTranscript()
			id := e.promptClaude()
			now := e.clock.Now()
			tr.add(userPrompt(now, "build it"))
			tr.addAll(teammateSpawned(now.Add(time.Second), "toolu_mate1", mateA))
			tr.teammate(mateA).addAll(mateWorks(now.Add(2*time.Second), mateA, "toolu_m1"))
			leadIdle(tr, now.Add(3*time.Second))
			if o := e.idleTicks(2); o.Kind != "" || o.Background != 1 {
				t.Fatalf("before = kind %q background %d, want held", o.Kind, o.Background)
			}
			tc.end(e, tr, id, e.clock.Now())
			tr.add(assistantSays(e.clock.Now(), "Done."), turnEnded(e.clock.Now()))
			if o := e.idleTicks(2); o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != id {
				t.Fatalf("after = %+v, want completed", o)
			}
		})
	}
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r    io.ReaderAt
	read atomic.Int64
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.read.Add(int64(n))
	return n, err
}

// teammateAtWork reads only the last teammateTail bytes of a teammate's
// transcript, every observe reading: the last complete user or assistant
// entry there decides, a line still being written does not, and a tail
// without such an entry is no turn in progress.
func TestTeammateAtWorkReadsOnlyTheTailOfItsFile(t *testing.T) {
	lines := func(ls ...tline) []byte {
		var b bytes.Buffer
		for _, l := range ls {
			b.Write(mustJSON(t, l))
			b.WriteByte('\n')
		}
		return b.Bytes()
	}
	var old []tline
	for i := range 4000 {
		old = append(old, mate(mateA, stopped(assistantSays(t0.Add(time.Duration(i)*time.Second), strings.Repeat("x", 200)), "end_turn")))
	}
	history := lines(old...)
	working := lines(mateWorks(t0.Add(time.Hour), mateA, "toolu_m1")...)
	done := lines(mateDone(t0.Add(2*time.Hour), mateA)...)
	var filler []tline
	for i := range 2000 {
		filler = append(filler, mate(mateA, tline{"type": "attachment", "timestamp": stamp(t0.Add(3 * time.Hour)), "attachment": map[string]any{"type": "output_style", "n": i, "pad": strings.Repeat("y", 150)}}))
	}
	padding := lines(filler...)
	if len(padding) <= teammateTail {
		t.Fatalf("padding is %d bytes, want more than the tail", len(padding))
	}
	half := lines(stopped(mate(mateA, assistantSays(t0.Add(4*time.Hour), "Done.")), "end_turn"))
	half = half[:len(half)/2] // still being written

	for _, tc := range []struct {
		name string
		file []byte
		want bool
	}{
		{"a turn in progress after a long history", joined(history, working), true},
		{"an ended turn after a long history", joined(history, working, done), false},
		{"an end_turn still being written", joined(history, working, half), true},
		{"a tail with no user or assistant entry", joined(history, working, padding), false},
		{"an empty file", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &countingReader{r: bytes.NewReader(tc.file)}
			if got := teammateAtWork(r, int64(len(tc.file))); got != tc.want {
				t.Fatalf("teammateAtWork = %v, want %v", got, tc.want)
			}
			if n := r.read.Load(); n > teammateTail+1 {
				t.Fatalf("read %d bytes of %d, want at most the tail (%d)", n, len(tc.file), teammateTail+1)
			}
		})
	}
}

// joined joins byte slices.
func joined(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// A spawn's result is recorded as a teammate, never as a background task:
// the Agent tool use's run_in_background no longer leaves it in the tasks.
func TestTeammateSpawnIsNoBackgroundTask(t *testing.T) {
	w := &bgWatch{}
	w.reset()
	for _, l := range teammateSpawned(t0, "toolu_mate1", mateA) {
		w.apply(mustJSON(t, l))
	}
	if len(w.tasks) != 0 {
		t.Fatalf("tasks = %v, want none", w.tasks)
	}
	if mt, ok := w.mates["toolu_mate1"]; !ok || mt.agent != mateA || !mt.spawned.Equal(t0.Add(time.Second)) {
		t.Fatalf("mates = %+v, want %s spawned at the result", w.mates, mateA)
	}
}
