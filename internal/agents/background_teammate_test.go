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

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
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

// lines is the transcript text of ls.
func lines(t *testing.T, ls ...tline) []byte {
	t.Helper()
	var b bytes.Buffer
	for _, l := range ls {
		b.Write(mustJSON(t, l))
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// A teammate's watch reads its transcript on from where the last reading
// stopped, about mateRead bytes per reading (a longer line whole, once),
// and counts the teammate at work while it is behind: a launch at the
// transcript's start is still pending megabytes later, and what is appended
// later costs only its own bytes.
func TestATeammateWatchReadsABoundedPartOfItsTranscriptPerReading(t *testing.T) {
	var file bytes.Buffer
	file.Write(lines(t, mates(mateA, bashInBackground(t0, "toolu_m1", "bgm1"))...))
	for i := range 4000 {
		at := t0.Add(time.Duration(i) * time.Second)
		file.Write(lines(t, mateMail(at, mateA), mate(mateA, stopped(assistantSays(at, strings.Repeat("x", 300)), "end_turn"))))
	}
	file.Write(lines(t, mate(mateA, stopped(toolUse(t0.Add(2*time.Hour), "toolu_r1", "Read", map[string]any{"file_path": "/tmp/shot.png"}), "tool_use"))))
	long := lines(t, mate(mateA, toolResult(t0.Add(2*time.Hour), "toolu_r1", strings.Repeat("z", 2*mateRead), nil, false)))
	file.Write(long)
	file.Write(lines(t, mate(mateA, stopped(assistantSays(t0.Add(3*time.Hour), "Waiting for the specs."), "end_turn"))))
	data := file.Bytes()

	w := newMateWatch()
	r := &countingReader{r: bytes.NewReader(data)}
	readings := 0
	for {
		before := r.read.Load()
		w.readSide(r, int64(len(data)))
		readings++
		if n := r.read.Load() - before; n > mateRead+int64(len(long))+transcriptChunk {
			t.Fatalf("reading %d read %d bytes, want about %d at most (and the long line)", readings, n, mateRead)
		}
		if !w.atWork(t0) {
			t.Fatalf("reading %d: not at work, want at work (behind %v, tasks %v)", readings, w.behind, w.tasks)
		}
		if !w.behind {
			break
		}
		if readings > 50 {
			t.Fatal("the readings never caught up")
		}
	}
	if readings < 3 || w.offset != int64(len(data)) {
		t.Fatalf("caught up in %d readings at %d of %d bytes, want a bounded part per reading and all of it", readings, w.offset, len(data))
	}
	if len(w.tasks) != 1 || w.inTurn {
		t.Fatalf("tasks %v in turn %v, want the launch at the start still pending and the turn ended", w.tasks, w.inTurn)
	}

	// A message wakes the teammate, which takes the launch's notification in
	// (the queued command, the live shape) and answers.
	more := lines(t, append([]tline{mateMail(t0.Add(4*time.Hour), mateA),
		mateTookIn(t0.Add(4*time.Hour), mateA, notificationText("toolu_m1", "bgm1", "completed"))},
		mateDone(t0.Add(4*time.Hour+time.Second), mateA)...)...)
	data = append(data, more...)
	r = &countingReader{r: bytes.NewReader(data)}
	w.readSide(r, int64(len(data)))
	if n := r.read.Load(); n > int64(len(more)) {
		t.Fatalf("read %d bytes, want only the %d appended", n, len(more))
	}
	if w.atWork(t0) {
		t.Fatalf("at work after the launch's notification and its answer (tasks %v, in turn %v)", w.tasks, w.inTurn)
	}
}

// A teammate's turn ends where Claude Code waits for its next prompt: an
// assistant entry that stopped on end_turn, a stop sequence or a refusal,
// or an API error Claude Code wrote itself. A tool use whose result is not
// written yet, pause_turn, max_tokens, a null or missing stop_reason, and a
// user entry (a tool result, a message from the lead) are a turn in
// progress. Whatever Claude Code writes after the deciding entry does not
// decide.
func TestATeammatesTurnEndsOnlyWhereClaudeCodeWaitsForItsNextPrompt(t *testing.T) {
	says := func(reason any) tline {
		l := mate(mateA, assistantSays(t0.Add(time.Minute), "Done."))
		if reason != "none" {
			stopped(l, reason)
		}
		return l
	}
	apiError := func(reason any) tline {
		l := says(reason)
		l["isApiErrorMessage"], l["apiError"] = true, "connection_failed"
		l["message"].(map[string]any)["model"] = "<synthetic>"
		return l
	}
	for _, tc := range []struct {
		name   string
		last   tline
		atWork bool
	}{
		{"end_turn", says("end_turn"), false},
		{"stop_sequence", says("stop_sequence"), false},
		{"refusal", says("refusal"), false},
		{"an API error Claude Code wrote", apiError("stop_sequence"), false},
		{"an API error with no stop_reason", apiError(nil), false},
		{"a tool use whose result is not written yet", mate(mateA, stopped(toolUse(t0.Add(time.Minute), "toolu_m1", "Bash",
			map[string]any{"command": "bin/rspec spec"}), "tool_use")), true},
		{"pause_turn", says("pause_turn"), true},
		{"max_tokens", says("max_tokens"), true},
		{"a null stop_reason", says(nil), true},
		{"no stop_reason", says("none"), true},
		{"a tool result", mate(mateA, toolResult(t0.Add(time.Minute), "toolu_m1", "ok", map[string]any{"stdout": "ok"}, false)), true},
		{"a message from the lead", mateMail(t0.Add(time.Minute), mateA), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := lines(t, mateMail(t0, mateA), tc.last,
				mate(mateA, tline{"type": "attachment", "timestamp": stamp(t0.Add(time.Minute)), "attachment": map[string]any{"type": "hook_success"}}),
				mate(mateA, tline{"type": "system", "subtype": "stop_hook_summary", "timestamp": stamp(t0.Add(time.Minute))}),
				tline{"type": "api-request", "timestamp": stamp(t0.Add(time.Minute)), "agentId": mateA},
				tline{"type": "file-history-snapshot", "agentId": mateA})
			w := newMateWatch()
			w.readSide(bytes.NewReader(data), int64(len(data)))
			if got := w.atWork(t0); got != tc.atWork {
				t.Fatalf("at work = %v, want %v", got, tc.atWork)
			}
		})
	}
}

// BackgroundProgress changes whenever a teammate's transcript grows, and
// only then: what tells a lead waiting on its teammates from a stuck one.
// With no teammate it is "".
func TestBackgroundProgressChangesWhileATeammatesTranscriptGrows(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	id := e.promptClaude()
	now := e.clock.Now()
	tr.add(userPrompt(now, "build it"))
	if p := e.m.BackgroundProgress(e.ctx, e.run1(id)); p != "" {
		t.Fatalf("with no teammate = %q, want empty", p)
	}
	tr.addAll(teammateSpawned(now.Add(time.Second), "toolu_mate1", mateA))
	missing := e.m.BackgroundProgress(e.ctx, e.run1(id))
	if !strings.Contains(missing, mateA) {
		t.Fatalf("with a teammate = %q, want its agent id", missing)
	}
	mt := tr.teammate(mateA)
	mt.addAll(mateWorks(now.Add(2*time.Second), mateA, "toolu_m1"))
	working := e.m.BackgroundProgress(e.ctx, e.run1(id))
	if working == missing {
		t.Fatalf("after the teammate's transcript grew = %q, want a change", working)
	}
	if again := e.m.BackgroundProgress(e.ctx, e.run1(id)); again != working {
		t.Fatalf("with nothing new = %q, want %q", again, working)
	}
	mt.addAll(mateDone(now.Add(3*time.Second), mateA))
	if done := e.m.BackgroundProgress(e.ctx, e.run1(id)); done == working {
		t.Fatalf("after the teammate's end_turn = %q, want a change", done)
	}
	if p := e.m.BackgroundProgress(e.ctx, store.Run{ID: "r-none"}); p != "" {
		t.Fatalf("without a session = %q, want empty", p)
	}
}

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
	if mt, ok := w.mates["toolu_mate1"]; !ok || mt.agent != mateA || !mt.spawned.Equal(t0.Add(time.Second)) || mt.w == nil || !mt.w.side {
		t.Fatalf("mates = %+v, want %s spawned at the result", w.mates, mateA)
	}
}

// mates marks every entry of ls as teammate agentID's.
func mates(agentID string, ls []tline) []tline {
	for _, l := range ls {
		mate(agentID, l)
	}
	return ls
}

// monitorStarted is a Monitor tool use and its result: a watch of a
// command's output that notifies the agent of each event, until the stream
// ends or the watch expires (persistent: never, for the whole session).
func monitorStarted(at time.Time, id, task string, persistent bool) []tline {
	return []tline{
		toolUse(at, id, "Monitor", map[string]any{"command": "tail -f log/test.log", "description": "Follow the spec run",
			"timeout_ms": 900000, "persistent": persistent}),
		toolResult(at.Add(time.Second), id, "Monitor started (task "+task+", expires in 15m unless the source ends first). You will be notified on each event.",
			map[string]any{"taskId": task, "timeoutMs": 900000, "persistent": persistent}, false),
	}
}

// monitorEventText is the notification of one event of Monitor task: no
// status, the Monitor still watches.
func monitorEventText(task string) string {
	return "<task-notification>\n<task-id>" + task + "</task-id>\n<summary>Monitor event: \"Follow the spec run\"</summary>\n" +
		"<event>120 examples, 0 failures so far</event>\n</task-notification>"
}

// monitorEndedText is the notification of the end of Monitor task, started
// by tool use id.
func monitorEndedText(id, task string) string {
	return "<task-notification>\n<task-id>" + task + "</task-id>\n<tool-use-id>" + id + "</tool-use-id>\n" +
		"<output-file>/tmp/claude/tasks/" + task + ".output</output-file>\n<status>completed</status>\n" +
		"<summary>Monitor \"Follow the spec run\" stream ended</summary>\n<event>spec run ended</event>\n</task-notification>"
}

// heldFor runs n idle ticks and fails unless each holds the run for want
// background tasks.
func (e *env) heldFor(n, want int, what string) {
	e.t.Helper()
	for tick := 1; tick <= n; tick++ {
		if o := e.idleTicks(1); o.Kind != "" || o.Background != want || o.Session.IdleTicks != 0 {
			e.t.Fatalf("idle tick %d %s = kind %q background %d idle_ticks %d, want held by %d",
				tick, what, o.Kind, o.Background, o.Session.IdleTicks, want)
		}
	}
}

// ends runs two idle ticks and fails unless they end run id.
func (e *env) ends(id, what string) {
	e.t.Helper()
	if o := e.idleTicks(1); o.Kind != "" || o.Background != 0 || o.Session.IdleTicks != 1 {
		e.t.Fatalf("first idle tick %s = kind %q background %d idle_ticks %d, want an idle reading", what, o.Kind, o.Background, o.Session.IdleTicks)
	}
	if o := e.idleTicks(1); o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != id {
		e.t.Fatalf("second idle tick %s = %+v, want completed", what, o)
	}
}

// mateTookIn is the queued command Claude Code 2.1.322 writes into
// teammate agentID's transcript when the teammate takes the notification
// (text) of its background work into a turn, stamped when the notification
// was queued (at).
func mateTookIn(at time.Time, agentID, text string) tline {
	return mate(agentID, tline{"type": "attachment", "timestamp": stamp(at), "attachment": map[string]any{
		"type": "queued_command", "prompt": text, "commandMode": "task-notification",
		"origin": map[string]any{"kind": "task-notification", "producer": "session-task"}, "timestamp": stamp(at)}})
}

// mateNotified is how Claude Code 2.1.322 delivers the notification (text)
// of a teammate's background work: the enqueue goes to the lead's
// transcript tr, not marked as a sidechain, and an idle teammate is not
// woken by it. Only when the teammate takes it into a turn (taken: one in
// progress, or one a message began) does the teammate's own transcript mt
// get the queued command and the lead's the remove.
func mateNotified(tr, mt *transcript, at time.Time, agentID, text string, taken bool) {
	tr.add(tline{"type": "queue-operation", "operation": "enqueue", "timestamp": stamp(at), "sessionId": claudeSID, "content": text})
	if taken {
		tr.add(tline{"type": "queue-operation", "operation": "remove", "timestamp": stamp(at.Add(3 * time.Second)), "sessionId": claudeSID, "content": text})
		mt.add(mateTookIn(at, agentID, text))
	}
}

// A teammate that ends its turn while its own background work runs (specs
// in the background, a Monitor on a gate, an async subagent) holds the run
// until that work ends. Claude Code 2.1.322 writes the work's notification
// into the lead's transcript, and an idle teammate is not woken by it (one
// recorded teammate sat idle 73 minutes with three finished tasks, until a
// message woke it): the notification there ends the teammate's work, and
// with the teammate idle the run ends, its lead not woken either. Whether
// the lead's watch read the notification after the teammate's launch or
// before (the lead's first reading came after both), the work ends. The
// first version read only the teammate's last entry and ended this run
// while the work ran (the review's probe); the second waited for the
// notification in the teammate's own transcript, where it never comes.
func TestObserveHoldsTheRunWhileATeammateWaitsOnItsOwnBackgroundWork(t *testing.T) {
	for _, tc := range []struct {
		name   string
		launch func(at time.Time) []tline
		text   string
	}{
		{"a Bash in the background", func(at time.Time) []tline { return bashInBackground(at, "toolu_m1", "bgm1") },
			notificationText("toolu_m1", "bgm1", "completed")},
		{"a Monitor", func(at time.Time) []tline { return monitorStarted(at, "toolu_mon1", "bmon1", false) },
			monitorEndedText("toolu_mon1", "bmon1")},
		{"a subagent launched asynchronously", func(at time.Time) []tline { return agentAsync(at, "toolu_m2", "a3f00d") },
			notificationText("toolu_m2", "a3f00d", "completed")},
	} {
		for _, readFirst := range []bool{true, false} {
			name := tc.name + ", notified before the lead's first reading"
			if readFirst {
				name = tc.name + ", notified after the lead's readings held the run"
			}
			t.Run(name, func(t *testing.T) {
				e := newEnv(t)
				tr := e.claudeTranscript()
				id := e.promptClaude()
				now := e.clock.Now()
				tr.add(userPrompt(now, "build it"))
				tr.addAll(teammateSpawned(now.Add(time.Second), "toolu_mate1", mateA))
				mt := tr.teammate(mateA)
				mt.add(mateMail(now.Add(2*time.Second), mateA))
				mt.addAll(mates(mateA, tc.launch(now.Add(3*time.Second))))
				mt.add(mate(mateA, stopped(assistantSays(now.Add(5*time.Second), "Waiting for the specs to report."), "end_turn")),
					mate(mateA, tline{"type": "system", "subtype": "stop_hook_summary", "timestamp": stamp(now.Add(5 * time.Second))}))
				leadIdle(tr, now.Add(6*time.Second))
				if readFirst {
					e.heldFor(3, 1, "while the teammate's own work runs")
				}
				mateNotified(tr, mt, e.clock.Now(), mateA, tc.text, false)
				e.ends(id, "after the teammate's work ended, the teammate idle")
			})
		}
	}
}

// The re-review's probe: a teammate takes the notification of its work into
// a turn a message began, works, and ends its turn; its idle message wakes
// the lead, which answers. The run then ends.
func TestObserveEndsTheRunOnceATeammatesWorkIsNotifiedTheLiveWay(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	id := e.promptClaude()
	now := e.clock.Now()
	tr.add(userPrompt(now, "build it"))
	tr.addAll(teammateSpawned(now.Add(time.Second), "toolu_mate1", mateA))
	mt := tr.teammate(mateA)
	mt.add(mateMail(now.Add(2*time.Second), mateA))
	mt.addAll(mates(mateA, bashInBackground(now.Add(3*time.Second), "toolu_m1", "bgm1")))
	mt.add(mate(mateA, stopped(assistantSays(now.Add(5*time.Second), "Waiting for the specs."), "end_turn")))
	leadIdle(tr, now.Add(6*time.Second))
	e.heldFor(2, 1, "while the teammate's specs run")

	at := e.clock.Now()
	mt.add(mateMail(at, mateA)) // a message wakes it; it takes the notification in
	mateNotified(tr, mt, at, mateA, notificationText("toolu_m1", "bgm1", "completed"), true)
	mt.addAll(mateWorks(at.Add(4*time.Second), mateA, "toolu_m2"), mateDone(at.Add(6*time.Second), mateA))
	tr.add(userPrompt(at.Add(7*time.Second), "Another Claude session sent a message:\n<teammate-message teammate_id=\"drafts\" color=\"blue\">\n{\"type\":\"idle_notification\",\"from\":\"drafts\"}\n</teammate-message>"),
		stopped(assistantSays(at.Add(8*time.Second), "The drafts teammate is idle; done."), "end_turn"), turnEnded(at.Add(8*time.Second)))
	e.ends(id, "after the teammate took its notification, worked and ended its turn, and the lead answered its idle message")
}

// The live ai-drafts case, by BackgroundTasks (the re-review's probe): the
// teammate launched specs in the background, took their notification into
// the turn it was in, and ended the turn. No work of its is left.
func TestBackgroundTasksCountsNoTeammateWorkItsLiveNotificationEnded(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	id := e.promptClaude()
	now := e.clock.Now()
	tr.add(userPrompt(now, "build it"))
	tr.addAll(teammateSpawned(now.Add(time.Second), "toolu_mate1", mateA))
	mt := tr.teammate(mateA)
	mt.add(mateMail(now.Add(2*time.Second), mateA))
	mt.addAll(mates(mateA, bashInBackground(now.Add(3*time.Second), "toolu_m1", "bgm1")))
	mateNotified(tr, mt, now.Add(10*time.Second), mateA, notificationText("toolu_m1", "bgm1", "completed"), true)
	mt.addAll(mateDone(now.Add(12*time.Second), mateA))
	leadIdle(tr, now.Add(13*time.Second))
	if n, ok := e.m.BackgroundTasks(e.ctx, e.run1(id)); !ok || n != 0 {
		t.Fatalf("BackgroundTasks = %d, %v; want 0 (the teammate's work ended and it is idle)", n, ok)
	}
}

// The notification of a teammate's work reaches the lead's transcript but
// does not wake the lead: it is not the lead's to answer, so it holds the
// lead's run no more than the teammate's work does (the re-review's
// probes; before, the lead's watch held the run until something woke the
// lead, past even a teammate's Monitor's bound).
func TestATeammatesNotificationInTheLeadsTranscriptIsNotTheLeadsToAnswer(t *testing.T) {
	t.Run("taken into a turn the teammate ended", func(t *testing.T) {
		e := newEnv(t)
		tr := e.claudeTranscript()
		id := e.promptClaude()
		now := e.clock.Now()
		tr.add(userPrompt(now, "build it"))
		tr.addAll(teammateSpawned(now.Add(time.Second), "toolu_mate1", mateA))
		mt := tr.teammate(mateA)
		mt.add(mateMail(now.Add(2*time.Second), mateA))
		mt.addAll(mates(mateA, bashInBackground(now.Add(3*time.Second), "toolu_m1", "bgm1")))
		mt.add(mate(mateA, stopped(assistantSays(now.Add(5*time.Second), "Waiting for the specs."), "end_turn")))
		leadIdle(tr, now.Add(6*time.Second))
		at := now.Add(20 * time.Second)
		mt.add(mateMail(at, mateA))
		mateNotified(tr, mt, at, mateA, notificationText("toolu_m1", "bgm1", "completed"), true)
		mt.addAll(mateWorks(at.Add(4*time.Second), mateA, "toolu_m2"), mateDone(at.Add(6*time.Second), mateA))
		e.ends(id, "after the teammate took its notification and ended its turn, with no idle message to the lead")
	})
	t.Run("a Monitor's end past its bound", func(t *testing.T) {
		e := newEnv(t)
		tr := e.claudeTranscript()
		id := e.promptClaude()
		now := e.clock.Now()
		tr.add(userPrompt(now, "build it"))
		tr.addAll(teammateSpawned(now.Add(time.Second), "toolu_mate1", mateA))
		mt := tr.teammate(mateA)
		mt.add(mateMail(now.Add(2*time.Second), mateA))
		mt.addAll(mates(mateA, monitorStarted(now.Add(3*time.Second), "toolu_mon1", "bmon1", false)), mateDone(now.Add(5*time.Second), mateA))
		leadIdle(tr, now.Add(6*time.Second))
		e.heldFor(1, 1, "while the teammate's Monitor watches")
		mateNotified(tr, mt, e.clock.Now().Add(time.Minute), mateA, monitorEndedText("toolu_mon1", "bmon1"), false)
		e.clock.Add(monitorTimeout + monitorGrace + time.Minute)
		e.ends(id, "past the teammate's Monitor's bound, its end enqueued in the lead's transcript")
	})
}

// A teammate whose turn esc cut short, or whose tool use was refused
// (Claude Code writes "[Request interrupted by user..." as a user entry),
// is idle, as the lead's cut turn is.
func TestObserveIsNotHeldByATeammateWhoseTurnWasInterrupted(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	id := e.promptClaude()
	now := e.clock.Now()
	tr.add(userPrompt(now, "build it"))
	tr.addAll(teammateSpawned(now.Add(time.Second), "toolu_mate1", mateA))
	mt := tr.teammate(mateA)
	mt.add(mateMail(now.Add(2*time.Second), mateA),
		mate(mateA, stopped(toolUse(now.Add(3*time.Second), "toolu_m1", "Bash", map[string]any{"command": "rm -rf tmp/cache"}), "tool_use")),
		mate(mateA, toolResult(now.Add(4*time.Second), "toolu_m1", "The user doesn't want to proceed with this tool use.", nil, true)),
		mate(mateA, interruption(now.Add(4*time.Second), "p1")))
	leadIdle(tr, now.Add(5*time.Second))
	e.ends(id, "with the teammate's turn interrupted")
}

// A teammate in the middle of a long foreground command (its last entry the
// tool use, whose result is not written yet) is at work.
func TestObserveHoldsTheRunWhileATeammateRunsAForegroundCommand(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	id := e.promptClaude()
	now := e.clock.Now()
	tr.add(userPrompt(now, "build it"))
	tr.addAll(teammateSpawned(now.Add(time.Second), "toolu_mate1", mateA))
	mt := tr.teammate(mateA)
	mt.add(mateMail(now.Add(2*time.Second), mateA),
		mate(mateA, stopped(toolUse(now.Add(3*time.Second), "toolu_m1", "Bash", map[string]any{"command": "bin/rspec spec", "description": "Run all specs"}), "tool_use")),
		mate(mateA, tline{"type": "attachment", "timestamp": stamp(now.Add(3 * time.Second)), "attachment": map[string]any{"type": "hook_success", "hookName": "PreToolUse:Bash"}}),
		tline{"type": "api-request", "timestamp": stamp(now.Add(3 * time.Second)), "agentId": mateA, "sessionId": claudeSID})
	leadIdle(tr, now.Add(4*time.Second))
	e.heldFor(4, 1, "while the teammate's command runs")

	at := e.clock.Now()
	mt.add(mate(mateA, toolResult(at, "toolu_m1", "4000 examples, 0 failures", map[string]any{"stdout": "4000 examples, 0 failures"}, false)))
	mt.addAll(mateDone(at.Add(time.Second), mateA))
	e.ends(id, "after the teammate's command and turn ended")
}

// A Monitor that a claude agent or its teammate starts watches until its
// stream ends: each event wakes the agent, which answers and waits on, so
// the Monitor is background work until its end's notification. A
// persistent one watches for the whole session and holds nothing (it would
// hold the run until Tell).
func TestObserveHoldsTheRunWhileAMonitorItStartedWatches(t *testing.T) {
	t.Run("the agent's", func(t *testing.T) {
		e := newEnv(t)
		tr := e.claudeTranscript()
		id := e.promptClaude()
		now := e.clock.Now()
		tr.add(userPrompt(now, "review"))
		tr.addAll(monitorStarted(now.Add(time.Second), "toolu_mon1", "bmon1", false))
		tr.add(assistantSays(now.Add(2*time.Second), "Waiting for the spec run."), turnEnded(now.Add(2*time.Second)))
		e.heldFor(2, 1, "while the Monitor watches")
		if evs := e.eventsOf(EventBackgroundWait); len(evs) != 1 || evs[0].Message != "claude-review: waiting for 1 background task" {
			t.Fatalf("%s events: %+v, want one", EventBackgroundWait, evs)
		}

		at := e.clock.Now()
		tr.addAll(noticed(at, monitorEventText("bmon1")))
		tr.add(assistantSays(at.Add(time.Second), "Still running."), turnEnded(at.Add(time.Second)))
		e.heldFor(2, 1, "after an event the agent answered")

		at = e.clock.Now()
		tr.addAll(noticed(at, monitorEndedText("toolu_mon1", "bmon1")))
		tr.add(assistantSays(at.Add(time.Second), "Specs pass."), turnEnded(at.Add(time.Second)))
		e.ends(id, "after the Monitor's stream ended")
	})
	t.Run("a teammate's", func(t *testing.T) {
		e := newEnv(t)
		tr := e.claudeTranscript()
		id := e.promptClaude()
		now := e.clock.Now()
		tr.add(userPrompt(now, "build it"))
		tr.addAll(teammateSpawned(now.Add(time.Second), "toolu_mate1", mateA))
		mt := tr.teammate(mateA)
		mt.add(mateMail(now.Add(2*time.Second), mateA))
		mt.addAll(mates(mateA, monitorStarted(now.Add(3*time.Second), "toolu_mon1", "bmon1", false)), mateDone(now.Add(4*time.Second), mateA))
		leadIdle(tr, now.Add(5*time.Second))
		e.heldFor(2, 1, "while the teammate's Monitor watches")

		// Its events and its end reach the lead's transcript; the idle
		// teammate takes none of them, and they are not the lead's to answer.
		mateNotified(tr, mt, e.clock.Now(), mateA, monitorEventText("bmon1"), false)
		e.heldFor(2, 1, "after an event the idle teammate did not take")
		mateNotified(tr, mt, e.clock.Now(), mateA, monitorEndedText("toolu_mon1", "bmon1"), false)
		e.ends(id, "after the teammate's Monitor ended")
	})
	t.Run("a persistent one", func(t *testing.T) {
		e := newEnv(t)
		tr := e.claudeTranscript()
		id := e.promptClaude()
		now := e.clock.Now()
		tr.add(userPrompt(now, "review"))
		tr.addAll(monitorStarted(now.Add(time.Second), "toolu_mon1", "bmon1", true), teammateSpawned(now.Add(2*time.Second), "toolu_mate1", mateA))
		tr.teammate(mateA).addAll(mates(mateA, monitorStarted(now.Add(3*time.Second), "toolu_mon2", "bmon2", true)), mateDone(now.Add(4*time.Second), mateA))
		leadIdle(tr, now.Add(5*time.Second))
		e.ends(id, "with persistent Monitors only")
	})
}

// Messages from another Claude session: a teammate's to its lead (a user
// entry with no origin), a cross-session one recorded the same way, and one
// Claude Code took into a turn as a queued command whose origin is a peer.
// Nobody typed them into the pane.
const (
	teammateMessageText     = "Another Claude session sent a message:\n<teammate-message teammate_id=\"drafts\" color=\"blue\" summary=\"Drafts done\">\nThe drafts part is done.\n</teammate-message>"
	crossSessionMessageText = "Another Claude session sent a message:\n<cross-session-message from=\"worker\">\nThe gate is green.\n</cross-session-message>"
	peerQueuedText          = "<cross-session-message from=\"uds:/tmp/cc-socks/101.sock\">\nThe gate is green.\n</cross-session-message>"
)

// peerTakenIn is a peer's message (text) queued at typed and taken into the
// turn the agent was in at at.
func peerTakenIn(typed, at time.Time, text string) []tline {
	return []tline{
		{"type": "queue-operation", "operation": "enqueue", "timestamp": stamp(typed), "sessionId": claudeSID, "content": text},
		{"type": "queue-operation", "operation": "remove", "reason": "absorbed_mid_turn", "timestamp": stamp(at), "sessionId": claudeSID, "content": text},
		{"type": "attachment", "isSidechain": false, "timestamp": stamp(typed), "sessionId": claudeSID,
			"attachment": map[string]any{"type": "queued_command", "prompt": text, "commandMode": "prompt",
				"origin": map[string]any{"kind": "peer", "from": "uds:/tmp/cc-socks/101.sock", "name": "worker"}, "timestamp": stamp(typed)}},
	}
}

// peerPrompt is a peer's message (text) as a user entry whose origin says
// so.
func peerPrompt(at time.Time, text string) tline {
	l := userPrompt(at, text)
	l["origin"] = map[string]any{"kind": "peer", "from": "worker", "name": "worker"}
	l["turnOrigin"], l["promptSource"] = "peer", "system"
	return l
}

// relayedShapes are the messages from another session, as each reaches the
// transcript at at.
var relayedShapes = []struct {
	name  string
	lines func(at time.Time) []tline
}{
	{"a teammate's message", func(at time.Time) []tline { return []tline{userPrompt(at, teammateMessageText)} }},
	{"a cross-session message with no origin", func(at time.Time) []tline { return []tline{userPrompt(at, crossSessionMessageText)} }},
	{"a peer's message taken into the turn", func(at time.Time) []tline { return peerTakenIn(at.Add(-10*time.Second), at, peerQueuedText) }},
	// What the origin alone tells, whatever the text (the queue's enqueue
	// line records no origin, so an untagged text's is left out here).
	{"a peer's message by its origin", func(at time.Time) []tline { return []tline{peerPrompt(at, "The gate is green.")} }},
	{"a peer's message taken in, by its origin", func(at time.Time) []tline {
		return peerTakenIn(at.Add(-10*time.Second), at, "The gate is green.")[1:]
	}},
}

// A message another Claude session sent is no prompt typed into the pane:
// with no run in flight the turn it begins or joins is no human activity,
// it is no typing during a run (the fix job's settle), and ClaudeUserTurns
// and ClaudeQueuedTurns (what the factory counts as your messages) return
// none of it.
func TestAnotherSessionsMessageIsNoPromptTypedIntoThePane(t *testing.T) {
	for _, sh := range relayedShapes {
		t.Run("humanTurn: "+sh.name, func(t *testing.T) {
			e := newEnv(t)
			tr := e.claudeTranscript()
			id := e.promptClaude()
			now := e.clock.Now()
			tr.add(humanPrompt(now, "/code-review https://github.com/talkable/talkable/pull/11920 high"))
			tr.addAll(bashInBackground(now.Add(time.Second), "toolu_bash1", "bg1"))
			tr.add(assistantSays(now.Add(2*time.Second), "Waiting for the spec run."), turnEnded(now.Add(2*time.Second)))
			if err := e.m.Tell(e.ctx, e.run1(id), "Time is up."); err != nil {
				t.Fatal(err)
			}
			tr.add(assistantSays(e.clock.Now(), "Report written."), turnEnded(e.clock.Now()))
			if o := e.idleTicks(2); o.Kind != ObsCompleted {
				t.Fatalf("run = %+v, want completed", o)
			}

			e.clock.Add(10 * time.Minute) // well past the grace after magnum's last prompt
			at := e.clock.Now()
			tr.addAll(notification(at, "toolu_bash1", "bg1", "completed"), sh.lines(at.Add(time.Second)))
			tr.add(toolUse(at.Add(2*time.Second), "toolu_cat1", "Bash", map[string]any{"command": "cat /tmp/claude/tasks/bg1.output"}))
			e.h.setAgentStatus(claudeAgent, herdr.StatusWorking)
			if o, pr := e.observe()[RoleClaude], e.reloadPR(); o.Kind != "" || pr.HumanActiveAt != nil {
				t.Fatalf("observation %q, human_active_at %v; want no human activity", o.Kind, pr.HumanActiveAt)
			}
		})
		t.Run("TypedDuringRun: "+sh.name, func(t *testing.T) {
			e := newEnv(t)
			tr := e.claudeTranscript()
			id := e.promptClaude()
			at := e.clock.Now()
			tr.add(humanPrompt(at, reviewPrompt), assistantSays(at.Add(time.Minute), "Pushed."))
			tr.addAll(sh.lines(at.Add(2 * time.Minute)))
			tr.add(assistantSays(at.Add(3*time.Minute), "Noted."), turnEnded(at.Add(3*time.Minute)))
			e.clock.Add(4 * time.Minute)
			if o := e.idleTicks(2); o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != id {
				t.Fatalf("the run's end = %+v, want completed", o)
			}
			if e.m.TypedDuringRun(e.ctx, e.run1(id)) {
				t.Error("TypedDuringRun = true, want false")
			}
		})
		t.Run("ClaudeUserTurns: "+sh.name, func(t *testing.T) {
			e := newEnv(t)
			tr := e.claudeTranscript()
			tr.add(humanPrompt(t0, "first prompt"), assistantSays(t0.Add(time.Second), "working"))
			tr.addAll(sh.lines(t0.Add(time.Minute)))
			turns, err := ClaudeUserTurns(tr.path, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if len(turns) != 1 || turns[0].Text != "first prompt" {
				t.Fatalf("ClaudeUserTurns = %+v, want the first prompt only", turns)
			}
			queued, err := ClaudeQueuedTurns(tr.path, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if len(queued) != 0 {
				t.Fatalf("ClaudeQueuedTurns = %+v, want none", queued)
			}
		})
	}
}

// A prompt the operator types stays a human's when it quotes another
// session's message: Claude Code's origin for it (human) decides, and with
// no origin (an older Claude Code) the text is such a message only when it
// starts with one, never when it quotes the tag or the line before it
// further on. humanTurn (the review daemon's ObsHumanActive), TypedDuringRun
// and ClaudeUserTurns read it as the operator's.
func TestAPromptTheOperatorTypesThatQuotesAnotherSessionsMessageIsHuman(t *testing.T) {
	const (
		tagMidText  = "see this: <teammate-message teammate_id=\"drafts\" summary=\"Drafts done\">The drafts part is done.</teammate-message>"
		lineMidText = "what does Another Claude session sent a message: mean"
	)
	quoted := "Why did the drafts teammate say this?\n" + teammateMessageText
	var cases []struct {
		name   string
		prompt func(at time.Time) tline
		text   string
	}
	add := func(name, text string, origin bool) {
		prompt := func(at time.Time) tline { return userPrompt(at, text) }
		if origin {
			prompt = func(at time.Time) tline { return humanPrompt(at, text) }
		}
		cases = append(cases, struct {
			name   string
			prompt func(at time.Time) tline
			text   string
		}{name, prompt, text})
	}
	for _, origin := range []bool{true, false} {
		with := "with no origin"
		if origin {
			with = "with its origin"
		}
		add(with+", a teammate's tag mid-text", tagMidText, origin)
		add(with+", the relay line mid-text", lineMidText, origin)
		add(with+", a teammate's message after a line", quoted, origin)
		add(with+", a cross-session message after a line", "See below.\n"+crossSessionMessageText, origin)
	}
	// Claude Code's human origin decides even for a prompt that starts with one.
	add("with its origin, starting with a teammate's message", teammateMessageText+"\nIs this right?", true)
	add("with its origin, starting with a cross-session message", crossSessionMessageText+"\nAnswer it.", true)

	for _, tc := range cases {
		t.Run("humanTurn: "+tc.name, func(t *testing.T) {
			e := newEnv(t)
			tr := e.claudeTranscript()
			id := e.promptClaude()
			now := e.clock.Now()
			tr.add(humanPrompt(now, "/code-review https://github.com/talkable/talkable/pull/11920 high"),
				assistantSays(now.Add(time.Second), "Report written."), turnEnded(now.Add(time.Second)))
			if o := e.idleTicks(2); o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != id {
				t.Fatalf("run = %+v, want completed", o)
			}

			e.clock.Add(10 * time.Minute) // well past the grace after magnum's last prompt
			at := e.clock.Now()
			tr.add(tc.prompt(at), toolUse(at.Add(time.Second), "toolu_look1", "Bash", map[string]any{"command": "git log -1"}))
			e.h.setAgentStatus(claudeAgent, herdr.StatusWorking)
			if o, pr := e.observe()[RoleClaude], e.reloadPR(); o.Kind != ObsHumanActive || pr.HumanActiveAt == nil {
				t.Fatalf("observation %q, human_active_at %v; want human activity", o.Kind, pr.HumanActiveAt)
			}
		})
		t.Run("TypedDuringRun: "+tc.name, func(t *testing.T) {
			e := newEnv(t)
			tr := e.claudeTranscript()
			id := e.promptClaude()
			at := e.clock.Now()
			tr.add(humanPrompt(at, reviewPrompt), assistantSays(at.Add(time.Minute), "Pushed."), turnEnded(at.Add(time.Minute)),
				tc.prompt(at.Add(2*time.Minute)), assistantSays(at.Add(3*time.Minute), "It is a teammate's message."), turnEnded(at.Add(3*time.Minute)))
			e.clock.Add(4 * time.Minute)
			if o := e.idleTicks(2); o.Kind != ObsCompleted || o.Run == nil || o.Run.ID != id {
				t.Fatalf("the run's end = %+v, want completed", o)
			}
			if !e.m.TypedDuringRun(e.ctx, e.run1(id)) {
				t.Error("TypedDuringRun = false, want true")
			}
		})
		t.Run("ClaudeUserTurns: "+tc.name, func(t *testing.T) {
			e := newEnv(t)
			tr := e.claudeTranscript()
			tr.add(humanPrompt(t0, "first prompt"), assistantSays(t0.Add(time.Second), "working"), tc.prompt(t0.Add(time.Minute)))
			turns, err := ClaudeUserTurns(tr.path, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if len(turns) != 2 || turns[1].Text != tc.text {
				t.Fatalf("ClaudeUserTurns = %+v, want the first prompt and the operator's", turns)
			}
		})
	}
}

// monitorTimeout is the timeout monitorStarted gives a Monitor (its
// timeoutMs).
const monitorTimeout = 15 * time.Minute

// A Monitor whose notifications never come (none of its stream's end, none
// at its expiry) holds the run no longer than its timeout and
// monitorGrace past its result, the lead's and a teammate's alike: the
// notification is its usual end, the bound is the last.
func TestAMonitorHoldsTheRunNoLongerThanItsTimeoutAndAGrace(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start func(e *env, tr *transcript, at time.Time) (result time.Time)
	}{
		{"the agent's", func(e *env, tr *transcript, at time.Time) time.Time {
			tr.addAll(monitorStarted(at, "toolu_mon1", "bmon1", false))
			tr.add(assistantSays(at.Add(2*time.Second), "Waiting for the spec run."), turnEnded(at.Add(2*time.Second)))
			return at.Add(time.Second)
		}},
		{"a teammate's", func(e *env, tr *transcript, at time.Time) time.Time {
			tr.addAll(teammateSpawned(at, "toolu_mate1", mateA))
			mt := tr.teammate(mateA)
			mt.add(mateMail(at, mateA))
			mt.addAll(mates(mateA, monitorStarted(at.Add(time.Second), "toolu_mon1", "bmon1", false)), mateDone(at.Add(3*time.Second), mateA))
			leadIdle(tr, at.Add(4*time.Second))
			return at.Add(2 * time.Second)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			tr := e.claudeTranscript()
			id := e.promptClaude()
			now := e.clock.Now()
			tr.add(userPrompt(now, "review"))
			end := tc.start(e, tr, now.Add(time.Second)).Add(monitorTimeout + monitorGrace)
			e.heldFor(1, 1, "while the Monitor watches")
			e.clock.Add(end.Sub(e.clock.Now()) - 40*time.Second)
			e.heldFor(1, 1, "just before the Monitor's bound")
			e.ends(id, "past the Monitor's timeout and grace")
		})
	}
}

// At most maxMates teammates are watched per run: a further spawn holds
// nothing, however busy its transcript, so the readings of one observe
// pass stay bounded.
func TestObserveWatchesAtMostSixteenTeammatesPerRun(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	id := e.promptClaude()
	now := e.clock.Now()
	tr.add(userPrompt(now, "build it"))
	for i := range 17 {
		agent := fmt.Sprintf("amate%02d-0123456789abcdef", i)
		tr.addAll(teammateSpawned(now.Add(time.Duration(i)*time.Second), fmt.Sprintf("toolu_mate%02d", i), agent))
		mt := tr.teammate(agent)
		mt.addAll(mateWorks(now.Add(20*time.Second), agent, "toolu_m1"))
		if i < 16 {
			mt.addAll(mateDone(now.Add(21*time.Second), agent))
		}
	}
	leadIdle(tr, now.Add(22*time.Second))
	if p := e.m.BackgroundProgress(e.ctx, e.run1(id)); strings.Count(p, ":") != 16 || strings.Contains(p, "amate16") {
		t.Fatalf("BackgroundProgress = %q, want the first 16 teammates", p)
	}
	e.ends(id, "with the 17th teammate at work")
}

// A teammate's own teammates are never watched: a spawn's result in a
// teammate's transcript is neither a teammate nor a task of its watch, so
// it holds nothing.
func TestATeammatesOwnTeammatesAreNotWatched(t *testing.T) {
	w := newMateWatch()
	for _, l := range mates(mateA, teammateSpawned(t0, "toolu_nested1", mateB)) {
		w.apply(mustJSON(t, l))
	}
	if len(w.mates) != 0 || len(w.tasks) != 0 {
		t.Fatalf("a teammate's watch after its own spawn: mates %v, tasks %v; want none", w.mates, w.tasks)
	}

	e := newEnv(t)
	tr := e.claudeTranscript()
	id := e.promptClaude()
	now := e.clock.Now()
	tr.add(userPrompt(now, "build it"))
	tr.addAll(teammateSpawned(now.Add(time.Second), "toolu_mate1", mateA))
	mt := tr.teammate(mateA)
	mt.add(mateMail(now.Add(2*time.Second), mateA))
	mt.addAll(mates(mateA, teammateSpawned(now.Add(3*time.Second), "toolu_nested1", mateB)), mateDone(now.Add(5*time.Second), mateA))
	tr.teammate(mateB).addAll(mateWorks(now.Add(4*time.Second), mateB, "toolu_m1")) // where its transcript would be
	leadIdle(tr, now.Add(6*time.Second))
	e.ends(id, "with only a teammate's own teammate at work")
}

// enqueuedOnly is a notification that only reached the lead's queue: the
// enqueue in the lead's transcript, nothing else (a teammate's, as Claude
// Code writes it for an idle teammate).
func enqueuedOnly(tr *transcript, at time.Time, text string) {
	tr.add(tline{"type": "queue-operation", "operation": "enqueue", "timestamp": stamp(at), "sessionId": claudeSID, "content": text})
}

// A notification that only reached the lead's queue, of work no watched
// teammate launched (an earlier run's teammate's, the 17th teammate's),
// holds the lead's run for queueGrace from its enqueue, then nothing:
// neither the lead nor the idle teammate is woken by it (the re-review's
// probes, which held the run until Tell).
func TestObserveHoldsTheRunAtMostAGraceForANotificationOnlyQueued(t *testing.T) {
	t.Run("an earlier run's teammate's", func(t *testing.T) {
		e := newEnv(t)
		tr := e.claudeTranscript()
		id1 := e.promptClaude()
		now := e.clock.Now()
		tr.add(userPrompt(now, "build it"))
		tr.addAll(teammateSpawned(now.Add(time.Second), "toolu_mate1", mateA))
		mt := tr.teammate(mateA)
		mt.add(mateMail(now.Add(2*time.Second), mateA))
		mt.addAll(mateDone(now.Add(3*time.Second), mateA))
		leadIdle(tr, now.Add(4*time.Second))
		e.ends(id1, "run 1")

		e.clock.Add(time.Minute)
		id2, err := e.m.Prompt(e.ctx, e.pr, RoleClaude, store.RunInitial, "fix the findings")
		if err != nil {
			t.Fatal(err)
		}
		at := e.clock.Now()
		tr.add(userPrompt(at, "fix the findings"),
			toolUse(at.Add(time.Second), "toolu_send1", "SendMessage", map[string]any{"to": "drafts", "message": "fix the drafts finding"}),
			toolResult(at.Add(2*time.Second), "toolu_send1", "sent", map[string]any{"success": true}, false))
		mt.add(mateMail(at.Add(3*time.Second), mateA))
		mt.addAll(mates(mateA, bashInBackground(at.Add(4*time.Second), "toolu_m1", "bgm1")))
		mt.add(mate(mateA, stopped(assistantSays(at.Add(6*time.Second), "Waiting for the specs."), "end_turn")))
		tr.add(userPrompt(at.Add(7*time.Second), "Another Claude session sent a message:\n<teammate-message teammate_id=\"drafts\" color=\"blue\">\n{\"type\":\"idle_notification\",\"from\":\"drafts\"}\n</teammate-message>"),
			stopped(assistantSays(at.Add(8*time.Second), "The drafts teammate is idle; done."), "end_turn"), turnEnded(at.Add(8*time.Second)))
		queued := at.Add(20 * time.Second)
		enqueuedOnly(tr, queued, notificationText("toolu_m1", "bgm1", "completed"))
		e.clock.Add(queued.Add(queueGrace).Sub(e.clock.Now()) - 40*time.Second)
		e.heldFor(1, 0, "within the grace of a notification only queued")
		e.ends(id2, "past the grace of an earlier run's teammate's notification")
	})
	t.Run("the 17th teammate's", func(t *testing.T) {
		e := newEnv(t)
		tr := e.claudeTranscript()
		id := e.promptClaude()
		now := e.clock.Now()
		tr.add(userPrompt(now, "build it"))
		var last *transcript
		var agent string
		for i := range 17 {
			agent = fmt.Sprintf("amate%02d-0123456789abcdef", i)
			tr.addAll(teammateSpawned(now.Add(time.Duration(i)*time.Second), fmt.Sprintf("toolu_mate%02d", i), agent))
			last = tr.teammate(agent)
			last.add(mateMail(now.Add(20*time.Second), agent))
			last.addAll(mateDone(now.Add(21*time.Second), agent))
		}
		last.addAll(mates(agent, bashInBackground(now.Add(20*time.Second), "toolu_m17", "bgm17")))
		leadIdle(tr, now.Add(22*time.Second))
		queued := now.Add(40 * time.Second)
		enqueuedOnly(tr, queued, notificationText("toolu_m17", "bgm17", "completed"))
		e.clock.Add(queued.Add(queueGrace).Sub(e.clock.Now()) - 40*time.Second)
		e.heldFor(1, 0, "within the grace of a notification only queued")
		e.ends(id, "past the grace of the 17th teammate's notification")
	})
}

// A notification the lead took into a turn (its user entry, or the queued
// command of one taken into the turn it was in) holds the run until the
// lead answers it, past queueGrace: an idle lead takes its own in at once.
func TestALeadsOwnNotificationItTookInHoldsUntilItAnswers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		taken func(tr *transcript, at time.Time)
	}{
		{"its user entry", func(tr *transcript, at time.Time) {
			tr.addAll(notification(at, "toolu_bash1", "bg1", "completed"))
		}},
		{"its queued command", func(tr *transcript, at time.Time) {
			text := notificationText("toolu_bash1", "bg1", "completed")
			tr.add(enqueued(at, text),
				tline{"type": "queue-operation", "operation": "remove", "timestamp": stamp(at.Add(time.Second)), "sessionId": claudeSID, "content": text},
				tline{"type": "attachment", "isSidechain": false, "timestamp": stamp(at), "sessionId": claudeSID, "attachment": map[string]any{
					"type": "queued_command", "prompt": text, "commandMode": "task-notification",
					"origin": map[string]any{"kind": "task-notification", "producer": "session-task"}, "timestamp": stamp(at)}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			tr := e.claudeTranscript()
			id := e.promptClaude()
			now := e.clock.Now()
			tr.add(userPrompt(now, "review"))
			tr.addAll(bashInBackground(now.Add(time.Second), "toolu_bash1", "bg1"))
			tr.add(assistantSays(now.Add(2*time.Second), "Waiting for the spec run."), turnEnded(now.Add(2*time.Second)))
			e.heldFor(1, 1, "while the spec run runs")
			tc.taken(tr, e.clock.Now())
			e.heldFor(6, 0, "with the notification taken in and not answered")
			tr.add(assistantSays(e.clock.Now(), "Specs pass."), turnEnded(e.clock.Now()))
			e.ends(id, "after the lead answered")
		})
	}
}

// The events of a teammate's persistent Monitor reach the lead's
// transcript; they are the teammate's work, not the lead's to answer (the
// re-review's probe, which held the run with every event).
func TestObserveIsNotHeldByEventsOfATeammatesPersistentMonitor(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	id := e.promptClaude()
	now := e.clock.Now()
	tr.add(userPrompt(now, "build it"))
	tr.addAll(teammateSpawned(now.Add(time.Second), "toolu_mate1", mateA))
	mt := tr.teammate(mateA)
	mt.add(mateMail(now.Add(2*time.Second), mateA))
	mt.addAll(mates(mateA, monitorStarted(now.Add(3*time.Second), "toolu_mon1", "bmon1", true)), mateDone(now.Add(5*time.Second), mateA))
	leadIdle(tr, now.Add(6*time.Second))
	enqueuedOnly(tr, now.Add(20*time.Second), monitorEventText("bmon1"))
	e.clock.Add(time.Minute)
	e.ends(id, "after an event of the teammate's persistent Monitor")
}

// esc in a teammate's tool use (the re-review's probe): the line esc writes
// ends its turn.
func TestObserveIsNotHeldByATeammateWhoseTurnEscCut(t *testing.T) {
	e := newEnv(t)
	tr := e.claudeTranscript()
	id := e.promptClaude()
	now := e.clock.Now()
	tr.add(userPrompt(now, "build it"))
	tr.addAll(teammateSpawned(now.Add(time.Second), "toolu_mate1", mateA))
	mt := tr.teammate(mateA)
	mt.add(mateMail(now.Add(2*time.Second), mateA),
		mate(mateA, stopped(toolUse(now.Add(3*time.Second), "toolu_m1", "Bash", map[string]any{"command": "bin/rspec"}), "tool_use")),
		mate(mateA, toolResult(now.Add(4*time.Second), "toolu_m1", "Interrupted", map[string]any{"interrupted": true}, true)),
		mate(mateA, userPrompt(now.Add(4*time.Second), "[Request interrupted by user]")))
	leadIdle(tr, now.Add(5*time.Second))
	e.ends(id, "after esc ended the teammate's turn")
}
