package agents

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// Claude Code ends its turn while work it started in the background still
// runs (a Bash command run with run_in_background or moved to the background
// by its timeout, an Agent or a workflow launched asynchronously, a skill
// forked into the background such as /code-review) and resumes the session
// when that work finishes: a <task-notification> user message naming the
// tool use that started it. herdr shows the agent idle in between, so two
// idle ticks ended such a run before its report was written (a reviewer
// whose report landed two hours after its round posted). The observer reads
// the session's transcript, the JSONL file Claude Code appends every entry
// to, and holds the run open while background work started during the run
// has neither notified nor been stopped (TaskStop), or while the agent has
// not answered a notification yet. A Monitor is such work until its stream
// ends (each event notifies the agent without ending it; a persistent one,
// which watches for the whole session, is none). A teammate (an Agent tool
// use with a name), which never notifies the agent and idles until a
// message wakes it, holds the run while its own transcript, read on the
// same way, shows a turn in progress, background work of its own or a
// notification it has not answered (matesAtWork). With no run in flight
// the agent still works on its own: on background work an earlier run left
// running (the subagents of a turn the restart's esc cut short) or on the
// turn a task notification began. The observer takes it for a human's only
// when the transcript shows a prompt typed into the pane after magnum's
// last prompt to the session (humanTurn): a task notification and a message
// another Claude session sent (a teammate's, a cross-session one) are
// nobody's typing (notTyped).

// EventBackgroundWait is recorded once per run when background work holds
// an idle claude agent's run open (data: role, run, tasks).
const EventBackgroundWait = "agent.background_wait"

const (
	// transcriptSkew widens a run's part of its transcript before the run
	// was created (the entries carry Claude Code's clock).
	transcriptSkew = 2 * time.Second
	// transcriptChunk is how much transcriptStart reads at a time.
	transcriptChunk = 64 << 10
	// mateRead is about how much of a teammate's transcript one reading
	// reads past where the last one stopped (a line longer than that is
	// read whole): a teammate further behind counts as at work until the
	// readings catch up (readSide).
	mateRead = 1 << 20
	// teammateGrace is how long after its spawn a teammate whose transcript
	// is not there yet holds the run.
	teammateGrace = 2 * time.Minute
)

// bgWatch is what the transcript of one claude session says about the run
// in flight. mu guards everything; the observer and BackgroundTasks read the
// transcript under it, Tell sets released.
type bgWatch struct {
	mu  sync.Mutex
	run string // "" = no run: the observer's watch of an agent working without one (turnWatch)
	// since: entries stamped before it are not the run's (its creation,
	// less transcriptSkew; zero without a run).
	since  time.Time
	sid    string // the Claude session id the transcript is named after
	path   string // "" = not located yet
	offset int64  // read up to here (after the last complete line); -1 = start at the run (or the last turn's start)
	// tasks are the background work started during the run that has not
	// finished: tool_use id -> its task id ("" until its result names one).
	tasks map[string]string
	stops map[string]string // TaskStop/KillShell tool_use id -> the task it stops
	note  time.Time         // the newest task notification
	reply time.Time         // the newest assistant entry
	// mates are the teammates spawned during the run: the spawn's tool_use
	// id -> the teammate. Unlike tasks they never finish by themselves: each
	// reading reads on in their own transcripts (matesAtWork).
	mates map[string]*teammate
	// side marks the watch of a teammate's own transcript (teammate.w): apply
	// takes its entries, sidechain ones, and readSide reads it.
	side bool
	// inTurn: the last user or assistant entry read leaves a turn in
	// progress (endsTurn; what a teammate's watch reads).
	inTurn bool
	// behind: a teammate's watch stopped its last reading at mateRead,
	// before its transcript's end.
	behind bool
	// cut is the newest line Claude Code wrote when its turn was cut short
	// (esc, or a tool use it asked about and was refused): the turn ended
	// there, whatever herdr shows while background work runs on.
	cut time.Time
	// typed is when the newest prompt typed into the pane that the current
	// turn holds was typed: the prompt that began the turn, or one Claude
	// Code took into it while it ran (a queued command). Zero when the turn
	// began with a task notification and took no prompt in, or no turn
	// start was read.
	typed time.Time
	// queued are the texts Claude Code queued (a prompt typed while the
	// agent worked, a task notification) that the agent has not taken yet
	// -> when each was queued, oldest first.
	queued map[string][]time.Time
	// released: the agent was told to stop waiting for its background work
	// (Tell), so that work no longer holds the run.
	released bool
	told     bool   // EventBackgroundWait recorded
	failed   string // the last read error logged ("" = none)
}

// teammate is an in-process teammate the agent spawned: its agent id, which
// names its transcript, when the spawn's result was written, and the watch
// of its own transcript (side; the lead's watch's mu guards it).
type teammate struct {
	agent   string
	spawned time.Time
	w       *bgWatch
}

// newMateWatch is the watch of a teammate's own transcript, which it reads
// from its start (the transcript begins at the spawn).
func newMateWatch() *bgWatch {
	w := &bgWatch{side: true}
	w.reset()
	return w
}

// reset forgets what was read (a new session id, a transcript that shrank).
func (w *bgWatch) reset() {
	w.offset, w.tasks, w.stops, w.note, w.reply = -1, map[string]string{}, map[string]string{}, time.Time{}, time.Time{}
	w.mates, w.inTurn, w.behind = map[string]*teammate{}, false, false
	w.cut, w.typed, w.queued = time.Time{}, time.Time{}, map[string][]time.Time{}
}

// watch is the bgWatch of session s for run, a fresh one when s had none or
// one of another run.
func (m *Manager) watch(sessionID int64, run store.Run) *bgWatch {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.bg[sessionID]
	if w == nil || w.run != run.ID {
		w = &bgWatch{run: run.ID, since: run.CreatedAt.Add(-transcriptSkew)}
		w.reset()
		m.bg[sessionID] = w
	}
	return w
}

// turnWatch is the bgWatch the observer reads for session s while no run
// is in flight: its last run's, which goes on from where that run's reads
// stopped, else one without a run, which starts at the transcript's last
// turn.
func (m *Manager) turnWatch(sessionID int64) *bgWatch {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.bg[sessionID]
	if w == nil {
		w = &bgWatch{}
		w.reset()
		m.bg[sessionID] = w
	}
	return w
}

// backgroundWait reads the transcript of session s, a claude agent seen idle
// while run is in flight, as Claude Code session sid. tasks is the
// background work started during run that has neither notified nor been
// stopped, with the teammates spawned during run whose own transcripts show
// a turn in progress now (read again on every call: no notification ever
// says a teammate is done); hold is true while there is any (until Tell) or
// while a task notification is not answered yet. A transcript it cannot
// find or read holds nothing: the run ends on herdr's status, as before.
func (m *Manager) backgroundWait(ctx context.Context, s store.Session, sid string, run store.Run) (tasks int, hold bool) {
	if m.sessionKind(s) != KindClaude || sid == "" {
		return 0, false
	}
	w := m.watch(s.ID, run)
	w.mu.Lock()
	defer w.mu.Unlock()
	if !m.readTranscript(s, sid, w) {
		return 0, false
	}
	tasks = len(w.tasks) + w.matesAtWork(m.now())
	hold = (tasks > 0 && !w.released) || w.note.After(w.reply)
	if tasks > 0 && !w.released && !w.told {
		w.told = true
		noun := "tasks"
		if tasks == 1 {
			noun = "task"
		}
		m.event(ctx, m.prSubject(ctx, s.PRID), "info", EventBackgroundWait,
			fmt.Sprintf("%s: waiting for %d background %s", s.Role, tasks, noun),
			map[string]any{"role": s.Role, "run": run.ID, "tasks": tasks})
	}
	return tasks, hold
}

// BackgroundTasks counts the background work the claude agent of run's
// session started during run and left running, and the teammates it
// spawned during run that are at work, as the transcripts show now (ok
// false: not a claude session, or no transcript to read).
func (m *Manager) BackgroundTasks(ctx context.Context, run store.Run) (int, bool) {
	n := 0
	ok := m.readRun(ctx, run, func(w *bgWatch) { n = len(w.tasks) + w.matesAtWork(m.now()) })
	return n, ok
}

// BackgroundProgress is a short text that changes whenever the transcript
// of a teammate the claude agent of run's session spawned during run, and
// still tracks, grows: each teammate's agent id and its transcript's size
// (0 while there is none), in the order of the ids. It is "" with no
// teammate tracked, for another kind and when the transcript cannot be
// read. A watchdog that sees no progress of the agent itself reads it to
// tell a lead waiting on its teammates from a stuck one.
func (m *Manager) BackgroundProgress(ctx context.Context, run store.Run) string {
	var parts []string
	m.readRun(ctx, run, func(w *bgWatch) {
		for _, mt := range w.mates {
			size := int64(0)
			if st, err := os.Stat(w.matePath(mt.agent)); err == nil {
				size = st.Size()
			}
			parts = append(parts, fmt.Sprintf("%s:%d", mt.agent, size))
		}
	})
	slices.Sort(parts)
	return strings.Join(parts, " ")
}

// readRun brings the watch of run's session up to date with its transcript
// and calls f with it, under its mu. It reports false, without calling f,
// for a run without a claude session that has a Claude session id, and
// when the transcript cannot be read.
func (m *Manager) readRun(ctx context.Context, run store.Run, f func(w *bgWatch)) bool {
	if run.SessionID == nil {
		return false
	}
	s, err := m.d.Store.SessionByID(ctx, *run.SessionID)
	if err != nil || m.sessionKind(s) != KindClaude || store.Deref(s.SessionID) == "" {
		return false
	}
	w := m.watch(s.ID, run)
	w.mu.Lock()
	defer w.mu.Unlock()
	if !m.readTranscript(s, store.Deref(s.SessionID), w) {
		return false
	}
	f(w)
	return true
}

// matesAtWork counts the teammates w recorded that are at work at now. Each
// call reads on in every teammate's transcript (matePath, readSide: about
// mateRead bytes at most past the last call) and counts the teammate while
// its watch shows it at work (atWork). A transcript not there yet counts
// within teammateGrace of the spawn (Claude Code may create it late); one
// that cannot be read counts nothing.
func (w *bgWatch) matesAtWork(now time.Time) int {
	n := 0
	for _, mt := range w.mates {
		if w.mateAtWork(mt, now) {
			n++
		}
	}
	return n
}

func (w *bgWatch) mateAtWork(mt *teammate, now time.Time) bool {
	f, err := os.Open(w.matePath(mt.agent))
	if errors.Is(err, fs.ErrNotExist) {
		return now.Before(mt.spawned.Add(teammateGrace))
	}
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	mt.w.readSide(f, st.Size())
	return mt.w.atWork()
}

// matePath is where Claude Code keeps the transcript of teammate agent of
// w's session: <the session's transcript dir>/<sid>/subagents/agent-<agent>.jsonl.
// agent is one path segment (oneSegment).
func (w *bgWatch) matePath(agent string) string {
	return filepath.Join(filepath.Dir(w.path), w.sid, "subagents", "agent-"+agent+".jsonl")
}

// readSide reads on in a teammate's transcript r (size bytes) from where
// its watch w stopped, a complete line at a time, as apply reads the lead's
// (sidechain entries included), and stops once it read mateRead bytes (a
// line longer than that is read whole, once): behind says it stopped
// before the end. A transcript that shrank is read again from its start.
func (w *bgWatch) readSide(r io.ReaderAt, size int64) {
	if w.offset > size {
		w.reset()
	}
	w.offset = max(w.offset, 0)
	br := bufio.NewReaderSize(io.NewSectionReader(r, w.offset, size-w.offset), transcriptChunk)
	var n int64
	w.behind = false
	for {
		if n >= mateRead {
			w.behind = w.offset+n < size
			break
		}
		line, err := br.ReadBytes('\n')
		if err != nil {
			break // the end, or a line still being written
		}
		n += int64(len(line))
		w.apply(line[:len(line)-1])
	}
	w.offset += n
}

// atWork reports whether a teammate's watch shows it at work: its last user
// or assistant entry leaves a turn in progress (a user entry, or an
// assistant entry that does not end the turn: endsTurn), background work it
// started has not finished, a notification it got is not answered yet, or
// the readings are behind its transcript.
func (w *bgWatch) atWork() bool {
	return w.inTurn || len(w.tasks) > 0 || w.note.After(w.reply) || w.behind
}

// humanTurn reports whether the agent of session s (Claude Code session
// sid for a claude agent), seen working with no run in flight past the
// grace after its start and its last prompt, works for someone who typed
// into its pane. A claude agent does only when its transcript shows a
// prompt typed after magnum's last prompt to the session (last_prompt_at,
// else started_at, plus transcriptSkew) in the turn it is in: the prompt
// that began the turn, unless a task notification or another Claude
// session's message did (notTyped; when it was typed
// is the stamp of its enqueue when Claude Code queued it, else of its
// entry), or one Claude Code took into the turn. Otherwise the work is
// magnum's turn going on (cut short by esc, whose interrupt line begins no
// turn, while its subagents run) or a turn a task notification began. Any
// other kind, and a transcript it cannot read, report true: an agent at
// work with no run is a human, as before.
func (m *Manager) humanTurn(s store.Session, sid string) bool {
	if m.sessionKind(s) != KindClaude || sid == "" {
		return true
	}
	typed, ok := m.typedAfterPrompt(s, sid, m.turnWatch(s.ID))
	return typed || !ok
}

// typedAfterPrompt reads session s's transcript (Claude Code session sid)
// into w and reports whether the turn it is in holds a prompt typed into
// the pane after magnum's last prompt to the session (last_prompt_at, else
// started_at, plus transcriptSkew): bgWatch.typed. ok is false when the
// transcript cannot be read.
func (m *Manager) typedAfterPrompt(s store.Session, sid string, w *bgWatch) (typed, ok bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !m.readTranscript(s, sid, w) {
		return false, false
	}
	last := s.StartedAt
	if s.LastPromptAt != nil {
		last = *s.LastPromptAt
	}
	return w.typed.After(last.Add(transcriptSkew)), true
}

// TypedDuringRun reports whether someone typed into the pane of run's
// session while run was in flight, for the fix job's settle: the run's part
// of a claude agent's transcript (from the run's creation, as the observer
// reads it while the run is in flight) ends in a turn that began with, or
// took in, a prompt typed after magnum's last prompt to the session, as
// humanTurn reads it (a turn a task notification began after the typed one
// hides it). Any other kind, a run without a session or a Claude session
// id, and a transcript it cannot read report false: only Claude Code keeps a
// transcript magnum reads.
func (m *Manager) TypedDuringRun(ctx context.Context, run store.Run) bool {
	if run.SessionID == nil {
		return false
	}
	s, err := m.d.Store.SessionByID(ctx, *run.SessionID)
	sid := store.Deref(s.SessionID)
	if err != nil || m.sessionKind(s) != KindClaude || sid == "" {
		return false
	}
	typed, _ := m.typedAfterPrompt(s, sid, m.watch(s.ID, run))
	return typed
}

// Tell sends text to the agent of run's session within that run, without a
// new run (as continueAfterDeny sends after_deny_prompt). The pipeline tells
// a reviewer three things this way, each asking it to stop its background
// tasks: the last call when its time ran out (and to write its report
// now), and, once it was interrupted, to stop the background work it left
// running at the end of a round or before a push restarts the round. From
// then on the agent's background work no longer holds the run open; a task
// notification it has not answered still does. The session's idle_ticks
// are reset and its last_prompt_at set (so the turn the text starts is not
// taken for someone typing). A run without a live agent session fails with
// ErrNoSession (ErrNotAgent for a shell role's pane).
func (m *Manager) Tell(ctx context.Context, run store.Run, text string) error {
	if run.SessionID == nil {
		return fmt.Errorf("agents: tell %s: %w", run.ID, ErrNoSession)
	}
	s, err := m.d.Store.SessionByID(ctx, *run.SessionID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && s.State != store.SessionLive) {
		return fmt.Errorf("agents: tell %s: %w", run.ID, ErrNoSession)
	}
	if err != nil {
		return fmt.Errorf("agents: tell %s: %w", run.ID, err)
	}
	if !m.isAgentSession(s) {
		return fmt.Errorf("agents: tell %s: %w", run.ID, ErrNotAgent)
	}
	// The text ends with Enter: never type it into a dialog (herdr refuses
	// an agent it sees blocked; this also covers a screen it does not).
	ref := paneRef{name: store.Deref(s.AgentName), pane: store.Deref(s.HerdrPaneID)}
	if screen, err := m.readVisible(ctx, ref); err == nil {
		_, trust := detectTrustDialog(screen)
		_, prompt := detectPermissionPrompt(screen)
		if trust || prompt {
			return fmt.Errorf("agents: tell %s: a dialog is on screen: %w", run.ID, ErrBlocked)
		}
	}
	if _, err := m.d.Herdr.AgentPrompt(ctx, target(s), text, nil); err != nil {
		return fmt.Errorf("agents: tell %s: %w", run.ID, mapHerdr(err))
	}
	w := m.watch(s.ID, run)
	w.mu.Lock()
	w.released = true
	w.mu.Unlock()
	now := m.now()
	if err := m.d.Store.UpdateSession(context.WithoutCancel(ctx), s.ID, func(u *store.SessionUpdate) {
		u.Set("idle_ticks", 0)
		u.Set("last_prompt_at", now)
	}); err != nil {
		m.logErr(ctx, err, "agents: tell %s: session %d: %v", run.ID, s.ID, err)
	}
	return nil
}

// readTranscript brings w up to date with what session sid's transcript
// gained since the last read, starting at w's run's creation (without a
// run, at the transcript's last turn). It reports whether the transcript
// could be read (a failure is logged once).
func (m *Manager) readTranscript(s store.Session, sid string, w *bgWatch) bool {
	if w.sid != sid {
		w.sid, w.path = sid, ""
		w.reset()
	}
	err := m.readNew(s, w)
	if err != nil {
		if msg := err.Error(); msg != w.failed {
			w.failed = msg
			m.logAt(slog.LevelWarn, "agents: %s: cannot read its Claude transcript, so its turns end when herdr shows it idle and count as typed: %v", s.Role, err)
		}
		return false
	}
	w.failed = ""
	return true
}

func (m *Manager) readNew(s store.Session, w *bgWatch) error {
	if w.path == "" {
		p, err := m.transcriptPath(s, w.sid)
		if err != nil {
			return err
		}
		w.path = p
	}
	f, err := os.Open(w.path)
	if err != nil {
		w.path = "" // look again next time (a session resumed under another directory)
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		w.path = ""
		return fmt.Errorf("%s is not a file", f.Name())
	}
	size := st.Size()
	if w.offset > size {
		w.reset()
	}
	if w.offset < 0 {
		if w.run == "" {
			w.offset, err = lastTurnStart(f, size)
		} else {
			w.offset, err = transcriptStart(f, size, w.since)
		}
		if err != nil {
			return err
		}
	}
	if size == w.offset {
		return nil
	}
	buf := make([]byte, size-w.offset)
	n, err := f.ReadAt(buf, w.offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	buf = buf[:n]
	end := bytes.LastIndexByte(buf, '\n')
	if end < 0 {
		return nil // one entry still being written
	}
	for line := range bytes.SplitSeq(buf[:end], []byte("\n")) {
		w.apply(line)
	}
	w.offset += int64(end + 1)
	return nil
}

// transcriptPath is where Claude Code keeps session sid of s:
// <config dir>/projects/<the session's cwd with every character but
// [A-Za-z0-9] turned into "-">/<sid>.jsonl, the config dir being the
// CLAUDE_CONFIG_DIR of the session's pane env, else Deps.ClaudeDir, else
// $CLAUDE_CONFIG_DIR, else ~/.claude (a test binary never falls back to the
// defaults). When that file is missing (Claude Code shortens a long path,
// or resolves a symlinked checkout), the one file named <sid>.jsonl under
// any project directory is.
func (m *Manager) transcriptPath(s store.Session, sid string) (string, error) {
	if !config.SessionArgOK(sid) { // no path out of the config dir, no glob that matches another session's file
		return "", fmt.Errorf("session id %q is no file name", sid)
	}
	dir := m.claudeDir(s)
	if dir == "" {
		return "", errors.New("no Claude config dir")
	}
	projects := filepath.Join(dir, "projects")
	name := sid + ".jsonl"
	if cwd := store.Deref(s.Cwd); cwd != "" {
		p := filepath.Join(projects, projectDirName.ReplaceAllString(cwd, "-"), name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	matches, err := filepath.Glob(filepath.Join(projects, "*", name))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no %s under %s", name, projects)
	}
	return matches[0], nil
}

// projectDirName matches what Claude Code replaces in a cwd to name its
// project directory.
var projectDirName = regexp.MustCompile(`[^A-Za-z0-9]`)

// claudeDir is the Claude config dir of session s (see transcriptPath).
func (m *Manager) claudeDir(s store.Session) string {
	if d := s.Env["CLAUDE_CONFIG_DIR"]; d != "" {
		return d
	}
	if m.d.ClaudeDir != "" {
		return m.d.ClaudeDir
	}
	if testing.Testing() {
		return ""
	}
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// transcriptStart is an offset of r (size bytes) before which every
// complete line is stamped before since: the end of the last line stamped
// before since (lines without a timestamp never stop it), so a long
// session costs only its tail. 0 when no line is older.
func transcriptStart(r io.ReaderAt, size int64, since time.Time) (int64, error) {
	_, end, ok, err := lastLine(r, size, func(line []byte) bool {
		at, ok := lineTime(line)
		return ok && at.Before(since)
	})
	if !ok {
		return 0, err
	}
	return end, nil
}

// lastTurnStart is where a watch without a run starts reading r (size
// bytes): the transcript's last turn-starting user entry (see
// tEntry.prompt), or the enqueue of its prompt when Claude Code queued it
// (typed while the agent worked, the prompt is taken later; its enqueue,
// after the turn before began, says when it was typed); 0 when there is
// none.
func lastTurnStart(r io.ReaderAt, size int64) (int64, error) {
	var text string
	start, _, ok, err := lastLine(r, size, func(line []byte) bool {
		t, ok := turnPrompt(line)
		if ok {
			text = t
		}
		return ok
	})
	if !ok || err != nil {
		return start, err
	}
	queued := false
	at, _, ok, err := lastLine(r, start, func(line []byte) bool {
		if t, ok := enqueuedText(line); ok {
			queued = t == text
			return queued
		}
		_, ok := turnPrompt(line)
		return ok // the turn before began: the prompt was not queued during it
	})
	if err != nil {
		return 0, err
	}
	if ok && queued {
		return at, nil
	}
	return start, nil
}

// turnPrompt is the prompt of a transcript line that starts a turn (see
// tEntry.prompt).
func turnPrompt(line []byte) (string, bool) {
	if !bytes.Contains(line, []byte(`"user"`)) {
		return "", false // most lines: no JSON decoding
	}
	var e tEntry
	if json.Unmarshal(line, &e) != nil || e.IsSidechain {
		return "", false
	}
	return e.prompt(contentBlocks(e.Message.Content))
}

// enqueuedText is the text a queue-operation enqueue line queued.
func enqueuedText(line []byte) (string, bool) {
	if !bytes.Contains(line, []byte(`"enqueue"`)) {
		return "", false
	}
	var e tEntry
	if json.Unmarshal(line, &e) != nil || e.Type != "queue-operation" || e.Operation != "enqueue" {
		return "", false
	}
	text, ok := e.Content.(string)
	return text, ok
}

// lastLine finds the last line of r (size bytes) that match accepts,
// reading backwards transcriptChunk at a time, and returns where it starts
// and ends (past its newline, at most size). A line within one chunk is
// matched in place; a longer one, found once the chunk holding its start
// is read, is read whole once, so a line of megabytes costs its size, not
// a copy of it per chunk. match must not keep line.
func lastLine(r io.ReaderAt, size int64, match func(line []byte) bool) (start, end int64, ok bool, err error) {
	chunk := make([]byte, min(int64(transcriptChunk), size))
	var in, long []byte        // the bytes of chunk read at pos; the last line read whole
	pos, lineEnd := size, size // lineEnd: where the line whose start is looked for ends
	// text is the line [from, lineEnd) without its newline: in place when in
	// holds it, else read whole into long.
	text := func(from int64) ([]byte, error) {
		var b []byte
		if lineEnd <= pos+int64(len(in)) {
			b = in[from-pos : lineEnd-pos]
		} else {
			if int64(cap(long)) < lineEnd-from {
				long = make([]byte, lineEnd-from)
			}
			b = long[:lineEnd-from]
			if _, err := r.ReadAt(b, from); err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
		}
		return bytes.TrimSuffix(b, []byte("\n")), nil
	}
	for pos > 0 {
		n := min(int64(transcriptChunk), pos)
		pos -= n
		in = chunk[:n]
		if _, err := r.ReadAt(in, pos); err != nil && !errors.Is(err, io.EOF) {
			return 0, 0, false, err
		}
		for i := len(in); i > 0; {
			if i = bytes.LastIndexByte(in[:i], '\n'); i < 0 {
				break // the line starts in an earlier chunk
			}
			from := pos + int64(i) + 1
			if from == size {
				continue // the newline ending the last line
			}
			b, err := text(from)
			if err != nil {
				return 0, 0, false, err
			}
			if match(b) {
				return from, lineEnd, true, nil
			}
			lineEnd = from
		}
	}
	if lineEnd == 0 {
		return 0, 0, false, nil
	}
	b, err := text(0) // the first line
	if err != nil || !match(b) {
		return 0, 0, false, err
	}
	return 0, lineEnd, true, nil
}

// lineTime is the timestamp of a transcript entry.
func lineTime(line []byte) (time.Time, bool) {
	var e struct {
		Timestamp time.Time `json:"timestamp"`
	}
	if json.Unmarshal(line, &e) != nil || e.Timestamp.IsZero() {
		return time.Time{}, false
	}
	return e.Timestamp, true
}

// transcript entries: only the fields the watch reads.
type (
	tEntry struct {
		Type        string    `json:"type"`
		Timestamp   time.Time `json:"timestamp"`
		IsSidechain bool      `json:"isSidechain"`
		IsMeta      bool      `json:"isMeta"`    // text Claude Code adds itself (a skill's body), no prompt
		Operation   string    `json:"operation"` // queue-operation: enqueue, dequeue, remove, popAll
		Content     any       `json:"content"`   // queue-operation: the queued text
		Message     struct {
			Content json.RawMessage `json:"content"` // a string, or content blocks
			// StopReason is why the model stopped, on an assistant entry
			// ("" for null or none): see endsTurn.
			StopReason string `json:"stop_reason"`
		} `json:"message"`
		ToolUseResult any `json:"toolUseResult"` // an object, or a string for an error
		// Attachment of an "attachment" entry: a queued prompt Claude Code
		// took into the turn it was in is {"type": "queued_command",
		// "prompt", "commandMode": "prompt" or "task-notification",
		// "origin"}, the entry stamped when the prompt was queued.
		Attachment any `json:"attachment"`
		// What began the turn a prompt starts: turnOrigin "human" or
		// "task_notification", origin {"kind": "human" or
		// "task-notification"} (absent before Claude Code recorded them;
		// read loosely, so a shape of its own never drops the entry).
		TurnOrigin any `json:"turnOrigin"`
		Origin     any `json:"origin"`
		// IsCompactSummary marks the user entry a compaction leaves (its
		// summary of the conversation): ClaudeUserTurns returns no such turn.
		IsCompactSummary bool `json:"isCompactSummary"`
		// IsAPIErrorMessage marks the assistant entry Claude Code writes
		// itself when it gives up on an API call: the turn ends there.
		IsAPIErrorMessage bool `json:"isApiErrorMessage"`
	}
	tBlock struct {
		Type      string         `json:"type"`
		Text      string         `json:"text"`
		ID        string         `json:"id"` // tool_use
		Name      string         `json:"name"`
		Input     map[string]any `json:"input"`
		ToolUseID string         `json:"tool_use_id"` // tool_result
		IsError   bool           `json:"is_error"`
	}
)

// stopTools name a task to stop in their input (Claude Code's TaskStop,
// earlier KillShell and KillBash).
var stopTools = map[string]bool{"TaskStop": true, "KillShell": true, "KillBash": true}

// turnEnds are the stop reasons after which Claude Code waits for the next
// prompt. After any other (tool_use, pause_turn, max_tokens, none) the turn
// goes on, or may.
var turnEnds = map[string]bool{"end_turn": true, "stop_sequence": true, "refusal": true}

// endsTurn reports whether e, an assistant entry, ends its turn: a stop
// reason in turnEnds, or an API error Claude Code wrote itself.
func (e *tEntry) endsTurn() bool {
	return e.IsAPIErrorMessage || turnEnds[e.Message.StopReason]
}

// prompt is the text of a user entry that starts a turn: a prompt typed
// into the pane or a task notification, as a string or a first text block
// (not a tool result, not text Claude Code adds itself, not the line esc
// writes into the turn it ends).
func (e *tEntry) prompt(blocks []tBlock) (string, bool) {
	if e.Type != "user" || e.IsMeta || len(blocks) == 0 || blocks[0].Type != "text" || e.interruption(blocks[0].Text) {
		return "", false
	}
	return blocks[0].Text, true
}

// interruption reports whether text, a user entry's, is the line Claude
// Code writes when esc ends a turn ("[Request interrupted by user]", "...
// for tool use]"): it begins no turn. A prompt carries an origin, the line
// none. Its promptId, usually the one of the turn it ends, sometimes is the
// next prompt's, so it does not tell.
func (e *tEntry) interruption(text string) bool {
	return e.TurnOrigin == nil && e.Origin == nil && strings.HasPrefix(text, "[Request interrupted by user")
}

// notTyped reports whether the turn prompt e starts (text) is nobody's
// typing into the pane (see notTyped).
func (e *tEntry) notTyped(text string) bool {
	return notTyped(str(e.TurnOrigin), e.Origin, "", text)
}

// notTyped reports whether a prompt (text) is nobody's typing into the
// pane: a message another Claude session sent (relayed), whatever origin
// Claude Code records for it (a peer's, or none for a teammate's); else a
// task notification or a peer's message as Claude Code records the
// prompt's turnOrigin, origin.kind or, for a queued command, commandMode;
// else (versions that record none) a text that is a task notification.
func notTyped(turnOrigin string, origin any, mode, text string) bool {
	if relayed(text) {
		return true
	}
	o, _ := origin.(map[string]any)
	switch kind := str(o["kind"]); {
	case turnOrigin != "":
		return turnOrigin == "task_notification" || turnOrigin == "peer"
	case kind != "":
		return kind == "task-notification" || kind == "peer"
	case mode != "":
		return mode == "task-notification"
	}
	return strings.HasPrefix(strings.TrimSpace(text), "<task-notification>")
}

// relayed reports whether text is a message another Claude session sent: a
// teammate's (<teammate-message>), a cross-session one
// (<cross-session-message>) or a named agent's (<agent-message>), as is or
// after the line Claude Code puts before it ("Another Claude session sent a
// message:").
func relayed(text string) bool {
	text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "Another Claude session sent a message:"))
	return strings.HasPrefix(text, "<teammate-message") || strings.HasPrefix(text, "<cross-session-message") ||
		strings.HasPrefix(text, "<agent-message")
}

// takenIn is when a prompt typed into the pane that Claude Code took into
// the turn it was in (an attachment entry holding a queued command that is
// someone's typing: notTyped) was queued.
func (e *tEntry) takenIn() (time.Time, bool) {
	a, _ := e.Attachment.(map[string]any)
	if e.Type != "attachment" || str(a["type"]) != "queued_command" ||
		notTyped("", a["origin"], str(a["commandMode"]), str(a["prompt"])) {
		return time.Time{}, false
	}
	return e.Timestamp, true
}

// apply reads one transcript line into w. Entries stamped before w.since,
// entries of a subagent's own thread (isSidechain) unless w is a teammate's
// (side), and lines that are not JSON are skipped.
func (w *bgWatch) apply(line []byte) {
	var e tEntry
	if json.Unmarshal(line, &e) != nil || (e.IsSidechain && !w.side) || e.Timestamp.Before(w.since) {
		return
	}
	var blocks []tBlock
	if e.Type == "assistant" || e.Type == "user" {
		blocks = contentBlocks(e.Message.Content)
		w.inTurn = e.Type == "user" || !e.endsTurn()
	}
	if text, ok := e.prompt(blocks); ok {
		typed := e.Timestamp
		if q := w.queued[text]; len(q) > 0 {
			typed = q[0] // typed while the agent worked: queued then, taken now
			w.unqueue(text)
		}
		w.typed = time.Time{}
		if !e.notTyped(text) {
			w.typed = typed
		}
	}
	if at, ok := e.takenIn(); ok && at.After(w.typed) {
		w.typed = at
	}
	switch e.Type {
	case "assistant":
		if e.Timestamp.After(w.reply) {
			w.reply = e.Timestamp
		}
		for _, b := range blocks {
			if b.Type != "tool_use" || b.ID == "" {
				continue
			}
			if b.Input["run_in_background"] == true {
				w.tasks[b.ID] = ""
			}
			if stopTools[b.Name] {
				if id := cmp.Or(str(b.Input["task_id"]), str(b.Input["shell_id"]), str(b.Input["bash_id"])); id != "" {
					w.stops[b.ID] = id
				}
			}
		}
	case "user":
		result, _ := e.ToolUseResult.(map[string]any)
		for i, b := range blocks {
			switch b.Type {
			case "text":
				w.notified(b.Text, e.Timestamp)
				if e.interruption(b.Text) && e.Timestamp.After(w.cut) {
					w.cut = e.Timestamp
				}
			case "tool_result":
				w.result(b, result, i == 0, e.Timestamp)
			}
		}
	case "queue-operation":
		text, ok := e.Content.(string)
		switch {
		case e.Operation == "popAll": // the queue went back into the input box
			clear(w.queued)
		case !ok:
		case e.Operation == "enqueue":
			w.notified(text, e.Timestamp)
			w.queued[text] = append(w.queued[text], e.Timestamp)
		case e.Operation == "remove": // taken into the turn (its attachment says when it was queued), or dropped
			w.unqueue(text)
		}
	}
}

// unqueue forgets the oldest queuing of text.
func (w *bgWatch) unqueue(text string) {
	if q := w.queued[text]; len(q) > 1 {
		w.queued[text] = q[1:]
	} else {
		delete(w.queued, text)
	}
}

// result reads the result of tool use b.ToolUseID (res is the entry's
// toolUseResult, which belongs to its first block; at is the entry's
// stamp): a teammate's spawn becomes a teammate and no task (the Agent tool
// use's run_in_background made it one), a background launch becomes a task,
// and so does a Monitor (a result with taskId and timeoutMs) unless it is
// persistent (it watches for the whole session, so it would hold the run
// until Tell), a failed launch is none, a TaskStop that worked ends its
// task.
func (w *bgWatch) result(b tBlock, res map[string]any, first bool, at time.Time) {
	id := b.ToolUseID
	if id == "" {
		return
	}
	if b.IsError {
		delete(w.tasks, id)
		delete(w.stops, id)
		return
	}
	if task, ok := w.stops[id]; ok {
		delete(w.stops, id)
		w.finish(id, task)
	}
	if !first || res == nil {
		return
	}
	switch {
	case res["status"] == "teammate_spawned":
		delete(w.tasks, id)
		if agent := cmp.Or(str(res["agentId"]), str(res["agent_id"])); oneSegment(agent) {
			w.mates[id] = &teammate{agent: agent, spawned: at, w: newMateWatch()}
		}
	case str(res["backgroundTaskId"]) != "":
		w.tasks[id] = str(res["backgroundTaskId"])
	case res["background"] == true || res["status"] == "async_launched":
		w.tasks[id] = cmp.Or(str(res["agentId"]), str(res["taskId"]), str(res["task_id"]), id)
	case str(res["taskId"]) != "" && res["timeoutMs"] != nil: // a Monitor
		if res["persistent"] != true {
			w.tasks[id] = str(res["taskId"])
		}
	}
}

// notificationBlock is one <task-notification>: a finished task, its tool
// use and its task id.
var notificationBlock = regexp.MustCompile(`(?s)<task-notification>(.*?)(?:</task-notification>|$)`)

var (
	notifiedToolUse = regexp.MustCompile(`<tool-use-id>\s*([^<\s]+)\s*</tool-use-id>`)
	notifiedTask    = regexp.MustCompile(`<task-id>\s*([^<\s]+)\s*</task-id>`)
	notifiedStatus  = regexp.MustCompile(`<status>\s*([^<\s]+)\s*</status>`)
)

// notified reads the task notifications in text, a user message or a
// queued one, stamped at: each ends the task it names (by tool use or task
// id) unless its status says the task still runs or it is one event of a
// Monitor that watches on (an <event> and no status), and the newest is
// what the agent must answer.
func (w *bgWatch) notified(text string, at time.Time) {
	if !strings.Contains(text, "<task-notification>") {
		return
	}
	for _, m := range notificationBlock.FindAllStringSubmatch(text, -1) {
		block, _, _ := strings.Cut(m[1], "<result>")     // the task's own output may quote anything
		block, _, event := strings.Cut(block, "<event>") // and so may a Monitor's event
		st := notifiedStatus.FindStringSubmatch(block)
		if st != nil && (st[1] == "running" || st[1] == "pending") {
			continue
		}
		if at.After(w.note) {
			w.note = at
		}
		if st == nil && event {
			continue
		}
		toolUse, task := "", ""
		if x := notifiedToolUse.FindStringSubmatch(block); x != nil {
			toolUse = x[1]
		}
		if x := notifiedTask.FindStringSubmatch(block); x != nil {
			task = x[1]
		}
		w.finish(toolUse, task)
	}
}

// finish ends the task (or the teammate) started by tool use toolUse, or
// known as task (a teammate by its agent id).
func (w *bgWatch) finish(toolUse, task string) {
	if toolUse != "" {
		delete(w.tasks, toolUse)
		delete(w.mates, toolUse)
	}
	if task == "" {
		return
	}
	for id, t := range w.tasks {
		if t == task || id == task {
			delete(w.tasks, id)
		}
	}
	for id, mt := range w.mates {
		if mt.agent == task || id == task {
			delete(w.mates, id)
		}
	}
}

// oneSegment reports whether s names a file in a directory and nothing
// else: not empty, "." or "..", and no slash, backslash or NUL. A teammate's
// agent id that is not holds nothing.
func oneSegment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00")
}

// contentBlocks decodes a message's content: content blocks, or a string
// (one text block).
func contentBlocks(raw json.RawMessage) []tBlock {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil
		}
		return []tBlock{{Type: "text", Text: s}}
	}
	var out []tBlock
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
