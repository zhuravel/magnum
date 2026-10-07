package agents

// A Codex turn that ends in error records it in the session's rollout, the
// JSONL file Codex appends every event to
// ($CODEX_HOME/sessions/YYYY/MM/DD/rollout-<start>-<session id>.jsonl): its
// task_complete event carries the error's message and codex_error_info
// ("cyber_policy" for the cybersecurity refusal). The pane shows the same
// message, but a turn that printed a lot after it, or a pane read that
// starts below it, loses it there: TurnError reads it back.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
}

// TurnError reads the error the turn of run ended with from its Codex
// session's rollout: the last task_complete event stamped at or after the
// run's creation (less transcriptSkew), when it carries an error. The
// rollout is looked for under the session's CODEX_HOME (its pane env), else
// Deps.CodexHome, else $CODEX_HOME, else ~/.codex (a test binary never
// falls back to those). ok is false for a session of another kind or
// without a Codex session id, a rollout it cannot find or read, and a turn
// that ended without an error.
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
		m.logf("agents: %s: no Codex rollout to read the turn's error from: %v", s.Role, err)
		return TurnError{}, false
	}
	te, ok, err := lastTurnError(path, run.CreatedAt.Add(-transcriptSkew))
	if err != nil {
		m.logf("agents: %s: read the Codex rollout: %v", s.Role, err)
	}
	return te, ok
}

// rolloutPath finds the rollout of Codex session sid of s.
func (m *Manager) rolloutPath(s store.Session, sid string) (string, error) {
	if sid == "" || strings.ContainsAny(sid, `/\*?[`) || sid == "." || sid == ".." {
		return "", fmt.Errorf("session id %q is no file name", sid)
	}
	home := m.codexHome(s)
	if home == "" {
		return "", errors.New("no Codex home")
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
		return TurnError{Message: detail(ev.Payload.Error.Message), Info: info}, true, nil
	}
	return TurnError{}, false, nil
}
