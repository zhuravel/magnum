package agents

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

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
// not answered a notification yet. With no run in flight the agent still
// works on its own: on background work an earlier run left running (the
// subagents of a turn the restart's esc cut short) or on the turn a task
// notification began. The observer takes it for a human's only when the
// transcript shows a prompt typed into the pane after magnum's last prompt
// to the session (humanTurn).

// EventBackgroundWait is recorded once per run when background work holds
// an idle claude agent's run open (data: role, run, tasks).
const EventBackgroundWait = "agent.background_wait"

const (
	// transcriptSkew widens a run's part of its transcript before the run
	// was created (the entries carry Claude Code's clock).
	transcriptSkew = 2 * time.Second
	// transcriptChunk is how much transcriptStart reads at a time.
	transcriptChunk = 64 << 10
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

// reset forgets what was read (a new session id, a transcript that shrank).
func (w *bgWatch) reset() {
	w.offset, w.tasks, w.stops, w.note, w.reply = -1, map[string]string{}, map[string]string{}, time.Time{}, time.Time{}
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
// stopped; hold is true while there is any (until Tell) or while a task
// notification is not answered yet. A transcript it cannot find or read
// holds nothing: the run ends on herdr's status, as before.
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
	tasks = len(w.tasks)
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
// session started during run and left running, as its transcript shows now
// (ok false: not a claude session, or no transcript to read).
func (m *Manager) BackgroundTasks(ctx context.Context, run store.Run) (int, bool) {
	if run.SessionID == nil {
		return 0, false
	}
	s, err := m.d.Store.SessionByID(ctx, *run.SessionID)
	if err != nil || m.sessionKind(s) != KindClaude || store.Deref(s.SessionID) == "" {
		return 0, false
	}
	w := m.watch(s.ID, run)
	w.mu.Lock()
	defer w.mu.Unlock()
	if !m.readTranscript(s, store.Deref(s.SessionID), w) {
		return 0, false
	}
	return len(w.tasks), true
}

// humanTurn reports whether the agent of session s (Claude Code session
// sid for a claude agent), seen working with no run in flight past the
// grace after its start and its last prompt, works for someone who typed
// into its pane. A claude agent does only when its transcript shows a
// prompt typed after magnum's last prompt to the session (last_prompt_at,
// else started_at, plus transcriptSkew) in the turn it is in: the prompt
// that began the turn, unless a task notification did (when it was typed
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
	w := m.turnWatch(s.ID)
	w.mu.Lock()
	defer w.mu.Unlock()
	if !m.readTranscript(s, sid, w) {
		return true
	}
	last := s.StartedAt
	if s.LastPromptAt != nil {
		last = *s.LastPromptAt
	}
	return w.typed.After(last.Add(transcriptSkew))
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
		m.logf("agents: tell %s: session %d: %v", run.ID, s.ID, err)
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
			m.logf("agents: %s: cannot read its Claude transcript, so its turns end when herdr shows it idle and count as typed: %v", s.Role, err)
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
	if strings.ContainsAny(sid, `/\`) || sid == "." || sid == ".." {
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
// and ends (past its newline, at most size).
func lastLine(r io.ReaderAt, size int64, match func(line []byte) bool) (start, end int64, ok bool, err error) {
	pos := size
	var tail []byte // the bytes after pos not looked at yet: the start of a line that began before pos
	for pos > 0 {
		n := min(int64(transcriptChunk), pos)
		pos -= n
		buf := make([]byte, n, n+int64(len(tail)))
		if _, err := r.ReadAt(buf, pos); err != nil && !errors.Is(err, io.EOF) {
			return 0, 0, false, err
		}
		buf = append(buf, tail...)
		first := 0 // buf[first:] holds whole lines
		if pos > 0 {
			i := bytes.IndexByte(buf, '\n')
			if i < 0 {
				tail = buf // one line longer than what was read: read further back
				continue
			}
			first = i + 1
		}
		lines := buf[first:]
		for len(lines) > 0 {
			body := bytes.TrimSuffix(lines, []byte("\n"))
			i := bytes.LastIndexByte(body, '\n')
			if match(body[i+1:]) {
				at := pos + int64(first)
				return at + int64(i+1), min(at+int64(len(lines)), size), true, nil
			}
			lines = body[:i+1]
		}
		tail = buf[:first]
	}
	return 0, 0, false, nil
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

// byNotification reports whether the turn prompt e starts (text) began
// with a task notification: as Claude Code records the turn's origin, else
// (older versions) when the text is one.
func (e *tEntry) byNotification(text string) bool {
	return fromNotification(str(e.TurnOrigin), e.Origin, "", text)
}

// fromNotification reports whether a prompt (text) came from a task
// notification: by Claude Code's turnOrigin, origin.kind or, for a queued
// command, commandMode, else by its text.
func fromNotification(turnOrigin string, origin any, mode, text string) bool {
	o, _ := origin.(map[string]any)
	switch kind := str(o["kind"]); {
	case turnOrigin != "":
		return turnOrigin == "task_notification"
	case kind != "":
		return kind == "task-notification"
	case mode != "":
		return mode == "task-notification"
	}
	return strings.HasPrefix(strings.TrimSpace(text), "<task-notification>")
}

// takenIn is when a prompt typed into the pane that Claude Code took into
// the turn it was in (an attachment entry holding a queued command that is
// no task notification) was queued.
func (e *tEntry) takenIn() (time.Time, bool) {
	a, _ := e.Attachment.(map[string]any)
	if e.Type != "attachment" || str(a["type"]) != "queued_command" ||
		fromNotification("", a["origin"], str(a["commandMode"]), str(a["prompt"])) {
		return time.Time{}, false
	}
	return e.Timestamp, true
}

// apply reads one transcript line into w. Entries stamped before w.since,
// entries of a subagent's own thread (isSidechain) and lines that are not
// JSON are skipped.
func (w *bgWatch) apply(line []byte) {
	var e tEntry
	if json.Unmarshal(line, &e) != nil || e.IsSidechain || e.Timestamp.Before(w.since) {
		return
	}
	var blocks []tBlock
	if e.Type == "assistant" || e.Type == "user" {
		blocks = contentBlocks(e.Message.Content)
	}
	if text, ok := e.prompt(blocks); ok {
		typed := e.Timestamp
		if q := w.queued[text]; len(q) > 0 {
			typed = q[0] // typed while the agent worked: queued then, taken now
			w.unqueue(text)
		}
		w.typed = time.Time{}
		if !e.byNotification(text) {
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
				w.result(b, result, i == 0)
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
// toolUseResult, which belongs to its first block): a background launch
// becomes a task, a failed launch is none, a TaskStop that worked ends its
// task.
func (w *bgWatch) result(b tBlock, res map[string]any, first bool) {
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
	case str(res["backgroundTaskId"]) != "":
		w.tasks[id] = str(res["backgroundTaskId"])
	case res["background"] == true || res["status"] == "async_launched":
		w.tasks[id] = cmp.Or(str(res["agentId"]), str(res["taskId"]), str(res["task_id"]), id)
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
// id) unless its status says the task still runs, and the newest is what
// the agent must answer.
func (w *bgWatch) notified(text string, at time.Time) {
	if !strings.Contains(text, "<task-notification>") {
		return
	}
	for _, m := range notificationBlock.FindAllStringSubmatch(text, -1) {
		block, _, _ := strings.Cut(m[1], "<result>") // the task's own output may quote anything
		if st := notifiedStatus.FindStringSubmatch(block); st != nil && (st[1] == "running" || st[1] == "pending") {
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
		if at.After(w.note) {
			w.note = at
		}
	}
}

// finish ends the task started by tool use toolUse, or known as task.
func (w *bgWatch) finish(toolUse, task string) {
	if toolUse != "" {
		delete(w.tasks, toolUse)
	}
	if task == "" {
		return
	}
	for id, t := range w.tasks {
		if t == task || id == task {
			delete(w.tasks, id)
		}
	}
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
