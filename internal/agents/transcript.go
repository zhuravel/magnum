package agents

// The text of the turns a person gave an agent, read back from the CLI's own
// record of its session: a Claude Code transcript, a Codex rollout. The
// factory reads them to prove that a prompt it sent arrived, to record what
// the operator typed into its panes (with what the agent said before), and
// to count what the operator typed to a task's agents.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
	// Before is what the agent said in its two turns before this one, oldest
	// first, each turn's texts joined by a line break: the text of the
	// assistant's messages (Claude) or of the agent_message events (Codex)
	// between two prompts. A turn that said nothing is skipped, and a
	// session's first turns have fewer. ClaudeUserTurns and CodexUserTurns
	// read it in the same pass over the file; ClaudeQueuedTurns leaves it nil.
	Before []string
}

// replies keeps what the agent said in its last two turns that said
// anything, as a transcript is read a line at a time (UserTurn.Before).
type replies struct {
	last [2]string // the older first
	cur  []string  // the texts of the turn in progress
}

// say adds a text of the agent's to the turn in progress.
func (r *replies) say(text string) {
	if strings.TrimSpace(text) != "" {
		r.cur = append(r.cur, text)
	}
}

// turn ends the agent's turn in progress at a new prompt and returns what
// the agent said in its two turns before that prompt, oldest first.
func (r *replies) turn() []string {
	if len(r.cur) > 0 {
		r.last[0], r.last[1] = r.last[1], strings.Join(r.cur, "\n")
		r.cur = nil
	}
	var out []string
	for _, s := range r.last {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ClaudeUserTurns reads the user turns of the Claude Code transcript at path
// that are stamped at or after since (the zero time: all of them), in order:
// the user entries that start a turn (a string, or the first text block, is
// the Text; a tool result and the line esc writes are no turns), not the
// entries of a subagent's own thread, not text Claude Code adds itself
// (isMeta) and not task notifications. Each carries the text of the
// assistant's messages in the agent's two turns before it (Before; a turn a
// task notification began is one of the agent's turns too). The whole file
// is read, a line at a time; a line that is no JSON, such as the
// half-written last one, is skipped. A prompt typed while the agent worked
// and taken into the turn it was in is an attachment entry, not a user
// entry, and is not returned.
func ClaudeUserTurns(path string, since time.Time) ([]UserTurn, error) {
	var out []UserTurn
	var said replies
	err := eachRolloutLine(path, "", func(line []byte) bool {
		if !bytes.Contains(line, []byte(`"user"`)) && !bytes.Contains(line, []byte(`"assistant"`)) {
			return true // most lines: no JSON decoding
		}
		var e tEntry
		if json.Unmarshal(line, &e) != nil || e.IsSidechain {
			return true
		}
		blocks := contentBlocks(e.Message.Content)
		if e.Type == "assistant" {
			for _, b := range blocks {
				if b.Type == "text" {
					said.say(b.Text)
				}
			}
			return true
		}
		text, ok := e.prompt(blocks)
		if !ok {
			return true
		}
		before := said.turn()
		if !e.byNotification(text) && !e.Timestamp.Before(since) {
			out = append(out, UserTurn{At: e.Timestamp, Text: text, Origin: e.originName(), Before: before})
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// OriginQueued is the Origin of a prompt typed while the agent worked
// (ClaudeQueuedTurns).
const OriginQueued = "queued"

// ClaudeQueuedTurns reads the prompts typed into a Claude Code agent while
// it worked that the transcript at path records, stamped at or after since
// (the zero time: all of them), in order: the text of each queue-operation
// enqueue line, and the prompt of each queued_command attachment (the entry
// Claude Code writes when it takes a queued prompt into the turn it was in,
// stamped when the prompt was queued), task notifications left out. Origin
// is OriginQueued. ClaudeUserTurns returns neither, so what proves a prompt
// arrived, or counts what someone typed, reads both; one prompt may appear
// twice (queued, then taken in). Lines that are no JSON are skipped.
func ClaudeQueuedTurns(path string, since time.Time) ([]UserTurn, error) {
	var out []UserTurn
	err := eachRolloutLine(path, `"queue`, func(line []byte) bool {
		var e tEntry
		if json.Unmarshal(line, &e) != nil || e.IsSidechain || e.Timestamp.Before(since) {
			return true
		}
		text := ""
		switch {
		case e.Type == "queue-operation" && e.Operation == "enqueue":
			text, _ = e.Content.(string)
			if fromNotification("", nil, "", text) {
				text = ""
			}
		case e.Type == "attachment":
			if _, ok := e.takenIn(); ok {
				a, _ := e.Attachment.(map[string]any)
				text = str(a["prompt"])
			}
		}
		if text != "" {
			out = append(out, UserTurn{At: e.Timestamp, Text: text, Origin: OriginQueued})
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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
// user_message events, with the text of payload.message, each with the
// agent_message texts of the agent's two turns before it (Before). Unlike
// the tail scan TurnError makes, it reads the whole file, so a long turn's
// output never pushes a prompt out of reach. An empty message and a line
// that is no JSON are skipped.
func CodexUserTurns(path string, since time.Time) ([]UserTurn, error) {
	var out []UserTurn
	var said replies
	err := eachRolloutLine(path, `_message"`, func(line []byte) bool { // "user_message" and "agent_message"
		var ev rolloutEvent
		if json.Unmarshal(line, &ev) != nil || ev.Payload.Message == "" {
			return true
		}
		switch ev.Payload.Type {
		case "agent_message":
			said.say(ev.Payload.Message)
		case "user_message":
			before := said.turn()
			if !ev.Timestamp.Before(since) {
				out = append(out, UserTurn{At: ev.Timestamp, Text: ev.Payload.Message, Before: before})
			}
		}
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
