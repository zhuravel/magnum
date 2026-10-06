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
// not answered a notification yet.

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
// transcript under it, TimeUp sets timeUp.
type bgWatch struct {
	mu     sync.Mutex
	run    string
	sid    string // the Claude session id the transcript is named after
	path   string // "" = not located yet
	offset int64  // read up to here (after the last complete line); -1 = start at the run
	// tasks are the background work started during the run that has not
	// finished: tool_use id -> its task id ("" until its result names one).
	tasks map[string]string
	stops map[string]string // TaskStop/KillShell tool_use id -> the task it stops
	note  time.Time         // the newest task notification
	reply time.Time         // the newest assistant entry
	// timeUp: the agent was told to stop waiting for its background work
	// (TimeUp), so that work no longer holds the run.
	timeUp bool
	told   bool   // EventBackgroundWait recorded
	failed string // the last read error logged ("" = none)
}

// reset forgets what was read (a new session id, a transcript that shrank).
func (w *bgWatch) reset() {
	w.offset, w.tasks, w.stops, w.note, w.reply = -1, map[string]string{}, map[string]string{}, time.Time{}, time.Time{}
}

// watch is the bgWatch of session s for run, a fresh one when s had none or
// one of another run.
func (m *Manager) watch(sessionID int64, run string) *bgWatch {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.bg[sessionID]
	if w == nil || w.run != run {
		w = &bgWatch{run: run}
		w.reset()
		m.bg[sessionID] = w
	}
	return w
}

// backgroundWait reads the transcript of session s, a claude agent seen idle
// while run is in flight, as Claude Code session sid. tasks is the
// background work started during run that has neither notified nor been
// stopped; hold is true while there is any (until TimeUp) or while a task
// notification is not answered yet. A transcript it cannot find or read
// holds nothing: the run ends on herdr's status, as before.
func (m *Manager) backgroundWait(ctx context.Context, s store.Session, sid string, run store.Run) (tasks int, hold bool) {
	if m.sessionKind(s) != KindClaude || sid == "" {
		return 0, false
	}
	w := m.watch(s.ID, run.ID)
	w.mu.Lock()
	defer w.mu.Unlock()
	if !m.readTranscript(s, sid, run, w) {
		return 0, false
	}
	tasks = len(w.tasks)
	hold = (tasks > 0 && !w.timeUp) || w.note.After(w.reply)
	if tasks > 0 && !w.timeUp && !w.told {
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
	w := m.watch(s.ID, run.ID)
	w.mu.Lock()
	defer w.mu.Unlock()
	if !m.readTranscript(s, store.Deref(s.SessionID), run, w) {
		return 0, false
	}
	return len(w.tasks), true
}

// TimeUp sends text to the agent of run's session within that run, without
// a new run (as continueAfterDeny sends after_deny_prompt): the pipeline's
// last call to a reviewer whose time ran out to write its report now. From
// then on the agent's background work no longer holds the run open (the
// text tells it to stop waiting for that work); a task notification it has
// not answered still does. The session's idle_ticks are reset and its
// last_prompt_at set. A run without a live agent session fails with
// ErrNoSession (ErrNotAgent for a shell role's pane).
func (m *Manager) TimeUp(ctx context.Context, run store.Run, text string) error {
	if run.SessionID == nil {
		return fmt.Errorf("agents: time up %s: %w", run.ID, ErrNoSession)
	}
	s, err := m.d.Store.SessionByID(ctx, *run.SessionID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && s.State != store.SessionLive) {
		return fmt.Errorf("agents: time up %s: %w", run.ID, ErrNoSession)
	}
	if err != nil {
		return fmt.Errorf("agents: time up %s: %w", run.ID, err)
	}
	if !m.isAgentSession(s) {
		return fmt.Errorf("agents: time up %s: %w", run.ID, ErrNotAgent)
	}
	// The text ends with Enter: never type it into a dialog (herdr refuses
	// an agent it sees blocked; this also covers a screen it does not).
	ref := paneRef{name: store.Deref(s.AgentName), pane: store.Deref(s.HerdrPaneID)}
	if screen, err := m.readVisible(ctx, ref); err == nil {
		_, trust := detectTrustDialog(screen)
		_, prompt := detectPermissionPrompt(screen)
		if trust || prompt {
			return fmt.Errorf("agents: time up %s: a dialog is on screen: %w", run.ID, ErrBlocked)
		}
	}
	if _, err := m.d.Herdr.AgentPrompt(ctx, target(s), text, nil); err != nil {
		return fmt.Errorf("agents: time up %s: %w", run.ID, mapHerdr(err))
	}
	w := m.watch(s.ID, run.ID)
	w.mu.Lock()
	w.timeUp = true
	w.mu.Unlock()
	now := m.now()
	if err := m.d.Store.UpdateSession(context.WithoutCancel(ctx), s.ID, func(u *store.SessionUpdate) {
		u.Set("idle_ticks", 0)
		u.Set("last_prompt_at", now)
	}); err != nil {
		m.logf("agents: time up %s: session %d: %v", run.ID, s.ID, err)
	}
	return nil
}

// readTranscript brings w up to date with what session sid's transcript
// gained since the last read, starting at run's creation. It reports
// whether the transcript could be read (a failure is logged once).
func (m *Manager) readTranscript(s store.Session, sid string, run store.Run, w *bgWatch) bool {
	if w.sid != sid {
		w.sid, w.path = sid, ""
		w.reset()
	}
	err := m.readNew(s, run, w)
	if err != nil {
		if msg := err.Error(); msg != w.failed {
			w.failed = msg
			m.logf("agents: %s: cannot read its Claude transcript, so its run ends when herdr shows it idle: %v", s.Role, err)
		}
		return false
	}
	w.failed = ""
	return true
}

func (m *Manager) readNew(s store.Session, run store.Run, w *bgWatch) error {
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
	since := run.CreatedAt.Add(-transcriptSkew)
	size := st.Size()
	if w.offset > size {
		w.reset()
	}
	if w.offset < 0 {
		if w.offset, err = transcriptStart(f, size, since); err != nil {
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
		w.apply(line, since)
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
// complete line is stamped before since: it reads backwards,
// transcriptChunk at a time, to the last line stamped before since (lines
// without a timestamp never stop it), so a long session costs only its
// tail. 0 when no line is older.
func transcriptStart(r io.ReaderAt, size int64, since time.Time) (int64, error) {
	pos := size
	var tail []byte // the bytes after pos not looked at yet: the start of a line that began before pos
	for pos > 0 {
		n := min(int64(transcriptChunk), pos)
		pos -= n
		buf := make([]byte, n, n+int64(len(tail)))
		if _, err := r.ReadAt(buf, pos); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
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
			if at, ok := lineTime(body[i+1:]); ok && at.Before(since) {
				return min(pos+int64(first)+int64(len(lines)), size), nil
			}
			lines = body[:i+1]
		}
		tail = buf[:first]
	}
	return 0, nil
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
		Operation   string    `json:"operation"` // queue-operation: enqueue, dequeue, ...
		Content     any       `json:"content"`   // queue-operation: the queued text
		Message     struct {
			Content json.RawMessage `json:"content"` // a string, or content blocks
		} `json:"message"`
		ToolUseResult any `json:"toolUseResult"` // an object, or a string for an error
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

// apply reads one transcript line into w. Entries stamped before since,
// entries of a subagent's own thread (isSidechain) and lines that are not
// JSON are skipped.
func (w *bgWatch) apply(line []byte, since time.Time) {
	var e tEntry
	if json.Unmarshal(line, &e) != nil || e.IsSidechain || e.Timestamp.Before(since) {
		return
	}
	switch e.Type {
	case "assistant":
		if e.Timestamp.After(w.reply) {
			w.reply = e.Timestamp
		}
		for _, b := range contentBlocks(e.Message.Content) {
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
		for i, b := range contentBlocks(e.Message.Content) {
			switch b.Type {
			case "text":
				w.notified(b.Text, e.Timestamp)
			case "tool_result":
				w.result(b, result, i == 0)
			}
		}
	case "queue-operation":
		if text, ok := e.Content.(string); ok && e.Operation == "enqueue" {
			w.notified(text, e.Timestamp)
		}
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
