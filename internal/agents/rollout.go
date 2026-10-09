package agents

// A Codex turn that ends in error records it in the session's rollout, the
// JSONL file Codex appends every event to
// ($CODEX_HOME/sessions/YYYY/MM/DD/rollout-<start>-<session id>.jsonl): its
// task_complete event carries the error's message and codex_error_info
// ("cyber_policy" for the cybersecurity refusal). The pane shows the same
// message, but a turn that printed a lot after it, or a pane read that
// starts below it, loses it there: TurnError reads it back.
//
// A helper agent (a Codex sub-agent the session spawns: spawn_agent) runs in
// a rollout of its own, whose session_meta names the session as its
// parent_thread_id, and the session's rollout records each helper it starts
// or messages as an item_completed event of type SubAgentActivity naming
// the helper's thread (agent_thread_id). A helper's refused turn does not
// end the session's turn: the session goes on and its turn ends ok (on
// 10-06 the operator flagged such a PR by hand), and the
// helper may go on to a turn that ends ok too. So TurnError also reads every
// turn the session's helpers ended within the run (helperTurnError).

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// CodexCyberPolicy is the codex_error_info of Codex's cybersecurity refusal
// ("This content was flagged for possible cybersecurity risk").
const CodexCyberPolicy = "cyber_policy"

// KindName is an agent kind as a sentence names it: "Codex" for codex,
// "Claude" for claude ("The agent" for none).
func KindName(kind string) string {
	if kind == "" {
		return "The agent"
	}
	return strings.ToUpper(kind[:1]) + kind[1:]
}

// rolloutTail is how much of a rollout's end TurnError reads: the turn's
// task_complete is among its last events.
const rolloutTail = 1 << 20

// TurnError is the error a Codex turn ended with, as its rollout records it.
type TurnError struct {
	Message string // the error's message, redacted, at most 300 bytes
	Info    string // codex_error_info: cyber_policy, usage_limit_exceeded, server_overloaded, unauthorized, other
	// Helper is the thread id of the helper agent (a Codex sub-agent the
	// session spawned) whose turn ended with the error; "" for the
	// session's own turn.
	Helper string
}

// TurnError reads the error the turn of run ended with from its Codex
// session's rollout: the last task_complete event stamped at or after the
// run's creation (less transcriptSkew), when it carries an error. A
// cybersecurity refusal (cyber_policy) of a turn a helper agent of the
// session ended within the run (helperTurnError) comes before any error of
// the session's own but its own refusal, and a helper's other error stands
// in when the session's turn has none (Helper names the helper). The
// rollouts are looked for under the session's CODEX_HOME (its pane env),
// else Deps.CodexHome, else $CODEX_HOME, else ~/.codex (a test binary never
// falls back to those). ok is false for a session of another kind or
// without a Codex session id, a rollout it cannot find or read, and a turn
// that ended without an error, its helpers' turns too.
func (m *Manager) TurnError(ctx context.Context, run store.Run) (TurnError, bool) {
	if run.SessionID == nil {
		return TurnError{}, false
	}
	s, err := m.d.Store.SessionByID(ctx, *run.SessionID)
	if err != nil || m.sessionKind(s) != KindCodex {
		return TurnError{}, false
	}
	sid := store.Deref(s.SessionID)
	path, err := m.rolloutPath(s, sid)
	if err != nil {
		m.logAt(slog.LevelWarn, "agents: %s: no Codex rollout to read the turn's error from: %v", s.Role, err)
		return TurnError{}, false
	}
	since := run.CreatedAt.Add(-transcriptSkew)
	te, ok, err := lastTurnError(path, since)
	if err != nil {
		m.logAt(slog.LevelWarn, "agents: %s: read the Codex rollout: %v", s.Role, err)
	}
	if ok && te.Info == CodexCyberPolicy {
		return te, true
	}
	if h, found := m.helperTurnError(s, sid, path, since); found && (h.Info == CodexCyberPolicy || !ok) {
		return h, true
	}
	return te, ok
}

// rolloutPath finds the rollout of Codex session sid of s.
func (m *Manager) rolloutPath(s store.Session, sid string) (string, error) {
	home := m.codexHome(s)
	if home == "" {
		return "", errors.New("no Codex home")
	}
	return rolloutIn(home, sid)
}

// rolloutIn finds the rollout of Codex session (or helper thread) sid under
// the Codex home home.
func rolloutIn(home, sid string) (string, error) {
	if !rolloutID(sid) {
		return "", fmt.Errorf("session id %q is no file name", sid)
	}
	matches, err := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", "rollout-*-"+sid+".jsonl"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no rollout of session %s under %s", sid, filepath.Join(home, "sessions"))
	}
	return matches[len(matches)-1], nil
}

// rolloutID reports whether sid may name a rollout file: not empty, no
// path or glob characters.
func rolloutID(sid string) bool {
	return sid != "" && !strings.ContainsAny(sid, `/\*?[`) && sid != "." && sid != ".."
}

// helperTurnError reads the turns that the helper agents of Codex session
// sid of s (its rollout at path, spawnedHelpers) ended at or after since: a
// helper started by an earlier turn may still work in this one. It returns
// the first helper turn that ended in a cybersecurity refusal, else the
// first that ended in another error, with Helper set. A helper's rollout
// must name sid as its parent; one that does not, or that cannot be found
// or read, is skipped (logged), and so is one last written before since.
// found is false when no helper turn ended in an error.
func (m *Manager) helperTurnError(s store.Session, sid, path string, since time.Time) (te TurnError, found bool) {
	helpers, err := spawnedHelpers(path)
	if err != nil {
		m.logAt(slog.LevelWarn, "agents: %s: read the helper agents from the Codex rollout: %v", s.Role, err)
	}
	home := m.codexHome(s)
	for _, id := range helpers {
		hp, err := rolloutIn(home, id)
		if err != nil {
			m.logAt(slog.LevelWarn, "agents: %s: helper agent %s: %v", s.Role, id, err)
			continue
		}
		if st, err := os.Stat(hp); err == nil && st.ModTime().Before(since) {
			continue // no turn of it ended since
		}
		errs, err := helperTurnErrors(hp, sid, since)
		if err != nil {
			m.logAt(slog.LevelWarn, "agents: %s: helper agent %s: %v", s.Role, id, err)
		}
		for _, e := range errs {
			e.Helper = id
			if e.Info == CodexCyberPolicy {
				return e, true
			}
			if !found {
				te, found = e, true
			}
		}
	}
	return te, found
}

// eachRolloutLine calls fn with every line of the rollout at path that
// contains marker (most lines are never decoded), in order, until fn
// returns false.
func eachRolloutLine(path, marker string, fn func(line []byte) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && bytes.Contains(line, []byte(marker)) && !fn(line) {
			return nil
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// spawnedHelpers lists the helper agents the session whose rollout is at
// path started or messaged: the agent_thread_id of its SubAgentActivity
// items, each once, in order.
func spawnedHelpers(path string) ([]string, error) {
	var out []string
	err := eachRolloutLine(path, `"SubAgentActivity"`, func(line []byte) bool {
		var ev rolloutEvent
		if json.Unmarshal(line, &ev) != nil || ev.Payload.Type != "item_completed" || ev.Payload.Item == nil ||
			ev.Payload.Item.Type != "SubAgentActivity" {
			return true
		}
		if id := ev.Payload.Item.Thread; rolloutID(id) && !slices.Contains(out, id) {
			out = append(out, id)
		}
		return true
	})
	return out, err
}

// helperTurnErrors reads the errors of the helper's turns that ended at or
// after since from its rollout at path, which must name parent as its
// parent session (the session_meta of its first line).
func helperTurnErrors(path, parent string, since time.Time) ([]TurnError, error) {
	first := true
	var out []TurnError
	var orphan error
	err := eachRolloutLine(path, "", func(line []byte) bool {
		if first {
			first = false
			var ev rolloutEvent
			if json.Unmarshal(line, &ev) != nil || ev.Type != "session_meta" || ev.Payload.ParentThreadID != parent {
				orphan = fmt.Errorf("its rollout does not name session %s as its parent", parent)
				return false
			}
			return true
		}
		if !bytes.Contains(line, []byte(`"task_complete"`)) {
			return true
		}
		var ev rolloutEvent
		if json.Unmarshal(line, &ev) != nil || ev.Payload.Type != "task_complete" || ev.Timestamp.Before(since) || ev.Payload.Error == nil {
			return true
		}
		out = append(out, turnError(ev))
		return true
	})
	return out, cmp.Or(err, orphan)
}

// codexHome is the Codex home of session s (see TurnError).
func (m *Manager) codexHome(s store.Session) string {
	if h := s.Env["CODEX_HOME"]; h != "" {
		return h
	}
	if m.d.CodexHome != "" {
		return m.d.CodexHome
	}
	if testing.Testing() {
		return ""
	}
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// rolloutEvent is the part of a rollout line TurnError reads.
type rolloutEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Payload   struct {
		Type  string `json:"type"`
		Error *struct {
			Message string `json:"message"`
			Info    any    `json:"codex_error_info"`
		} `json:"error"`
		// Item is an item_completed event's item: a SubAgentActivity names
		// the thread of the helper agent started or messaged.
		Item *struct {
			Type   string `json:"type"`
			Thread string `json:"agent_thread_id"`
		} `json:"item"`
		// ParentThreadID is a helper's session_meta's parent session.
		ParentThreadID string `json:"parent_thread_id"`
	} `json:"payload"`
}

// lastTurnError reads the end of the rollout at path for its last
// task_complete stamped at or after since: its error, when it has one.
func lastTurnError(path string, since time.Time) (TurnError, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return TurnError{}, false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return TurnError{}, false, err
	}
	off := max(st.Size()-rolloutTail, 0)
	b, err := io.ReadAll(io.NewSectionReader(f, off, st.Size()-off))
	if err != nil {
		return TurnError{}, false, err
	}
	lines := strings.Split(string(b), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if !strings.Contains(line, `"task_complete"`) {
			continue // most lines: no JSON decoding
		}
		var ev rolloutEvent
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Payload.Type != "task_complete" {
			continue
		}
		if ev.Timestamp.Before(since) {
			return TurnError{}, false, nil
		}
		if ev.Payload.Error == nil {
			return TurnError{}, false, nil
		}
		return turnError(ev), true, nil
	}
	return TurnError{}, false, nil
}

// turnError is the error of task_complete event ev (Error set).
func turnError(ev rolloutEvent) TurnError {
	info := ""
	switch v := ev.Payload.Error.Info.(type) {
	case string:
		info = v
	case map[string]any: // an info with fields ({"http_connection_failed": {...}}): its name
		if len(v) == 1 {
			for k := range v {
				info = k
			}
		}
	}
	return TurnError{Message: detail(ev.Payload.Error.Message), Info: info}
}
