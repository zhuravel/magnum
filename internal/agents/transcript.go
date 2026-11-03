package agents

// The text of the turns a person gave an agent, read back from the CLI's own
// record of its session: a Claude Code transcript, a Codex rollout. The
// factory reads them to prove that a prompt it sent arrived, and to count
// what the operator typed to a task's agents.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// UserTurn is a prompt typed into an agent's pane, by a person or by magnum
// through herdr, as its transcript records it.
type UserTurn struct {
	At   time.Time // when the CLI stamped it
	Text string
	// Origin is what began the turn, as Claude Code records it ("human" for a
	// typed prompt); "" when the transcript names none (an older Claude Code,
	// every Codex rollout).
	Origin string
}

// ClaudeUserTurns reads the user turns of the Claude Code transcript at path
// that are stamped at or after since (the zero time: all of them), in order:
// the user entries that start a turn (a string, or the first text block, is
// the Text; a tool result and the line esc writes are no turns), not the
// entries of a subagent's own thread, not text Claude Code adds itself
// (isMeta) and not task notifications. The whole file is read, a line at a
// time; a line that is no JSON, such as the half-written last one, is
// skipped. A prompt typed while the agent worked and taken into the turn it
// was in is an attachment entry, not a user entry, and is not returned.
func ClaudeUserTurns(path string, since time.Time) ([]UserTurn, error) {
	var out []UserTurn
	err := eachRolloutLine(path, `"user"`, func(line []byte) bool {
		if t, ok := claudeUserTurn(line); ok && !t.At.Before(since) {
			out = append(out, t)
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// claudeUserTurn is the user turn a transcript line records.
func claudeUserTurn(line []byte) (UserTurn, bool) {
	var e tEntry
	if json.Unmarshal(line, &e) != nil || e.IsSidechain {
		return UserTurn{}, false
	}
	text, ok := e.prompt(contentBlocks(e.Message.Content))
	if !ok || e.byNotification(text) {
		return UserTurn{}, false
	}
	return UserTurn{At: e.Timestamp, Text: text, Origin: e.originName()}, true
}

// originName is what began the turn the entry starts: its turnOrigin, else
// its origin's kind, else "".
func (e *tEntry) originName() string {
	if o := str(e.TurnOrigin); o != "" {
		return o
	}
	o, _ := e.Origin.(map[string]any)
	return str(o["kind"])
}

// CodexUserTurns reads the user turns of the Codex rollout at path that are
// stamped at or after since (the zero time: all of them), in order: its
// user_message events, with the text of payload.message. Unlike the tail
// scan TurnError makes, it reads the whole file, so a long turn's output
// never pushes a prompt out of reach. An empty message and a line that is no
// JSON are skipped.
func CodexUserTurns(path string, since time.Time) ([]UserTurn, error) {
	var out []UserTurn
	err := eachRolloutLine(path, `"user_message"`, func(line []byte) bool {
		var ev rolloutEvent
		if json.Unmarshal(line, &ev) != nil || ev.Payload.Type != "user_message" || ev.Payload.Message == "" ||
			ev.Timestamp.Before(since) {
			return true
		}
		out = append(out, UserTurn{At: ev.Timestamp, Text: ev.Payload.Message})
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// TranscriptPath is the file the turns of session s are read from: the
// transcript of a Claude session (under the session's CLAUDE_CONFIG_DIR,
// else Deps.ClaudeDir; see transcriptPath), the rollout of a Codex session
// (under its CODEX_HOME, else Deps.CodexHome; see rolloutPath). It is an
// error for a shell pane, a kind with no such record, a session whose id is
// not recorded yet and a file that is not there.
func (m *Manager) TranscriptPath(ctx context.Context, s store.Session) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	kind := m.sessionKind(s)
	sid := store.Deref(s.SessionID)
	if sid == "" {
		return "", fmt.Errorf("agents: %s (session %d) has no session id", s.Role, s.ID)
	}
	switch kind {
	case KindClaude:
		return m.transcriptPath(s, sid)
	case KindCodex:
		return m.rolloutPath(s, sid)
	}
	return "", fmt.Errorf("agents: %s (session %d) is of kind %q, which keeps no transcript magnum reads", s.Role, s.ID, kind)
}
