package agents

// What a Claude Code transcript or a Codex rollout says of the session that
// wrote it, beside its turns: how it was started, where, and on which git
// branches. The factory's baseline reads it to tell the operator's
// interactive sessions from the runs nobody typed in, and to tie a session
// to the ticket its branch names.

import (
	"bytes"
	"encoding/json"
	"slices"
)

// CodexSourceSubagent is the Source of a Codex subagent's rollout, whose
// session_meta source is an object naming the subagent ({"subagent":
// "review"}, {"subagent": {"thread_spawn": ...}}).
const CodexSourceSubagent = "subagent"

// SessionFacts is what a transcript or a rollout says of its session.
type SessionFacts struct {
	// Entrypoint is how Claude Code was started, as its entries record it:
	// "cli" for its terminal UI, "sdk-cli" for `claude -p`, "sdk-ts" for
	// the TypeScript SDK; "" when no entry names one, and in a rollout.
	Entrypoint string
	// Originator is the client a Codex rollout's session_meta names
	// ("codex-tui", "codex_exec", "Claude Code"), and Source its source
	// ("cli", "exec", "vscode", or CodexSourceSubagent); both "" in a Claude
	// transcript.
	Originator, Source string
	// Cwd is the directory the session started in: the first entry's cwd,
	// the session_meta's.
	Cwd string
	// Branches are the git branches the session ran on, each once, in the
	// order the file first names them: the entries' gitBranch, the
	// session_meta's git.branch.
	Branches []string
}

// branch adds b to the branches, once; "" is none.
func (f *SessionFacts) branch(b string) {
	if b != "" && !slices.Contains(f.Branches, b) {
		f.Branches = append(f.Branches, b)
	}
}

// ClaudeSessionFacts reads the facts of the Claude Code transcript at path:
// the first entrypoint and the first cwd its entries name, and every
// gitBranch. The whole file is read, a line at a time; a line that is no
// JSON is skipped.
func ClaudeSessionFacts(path string) (SessionFacts, error) {
	var f SessionFacts
	err := eachRolloutLine(path, "", func(line []byte) bool {
		if !bytes.Contains(line, []byte(`"cwd"`)) && !bytes.Contains(line, []byte(`"gitBranch"`)) &&
			!bytes.Contains(line, []byte(`"entrypoint"`)) {
			return true
		}
		var e struct {
			Entrypoint string `json:"entrypoint"`
			Cwd        string `json:"cwd"`
			GitBranch  string `json:"gitBranch"`
		}
		if json.Unmarshal(line, &e) != nil {
			return true
		}
		if f.Entrypoint == "" {
			f.Entrypoint = e.Entrypoint
		}
		if f.Cwd == "" {
			f.Cwd = e.Cwd
		}
		f.branch(e.GitBranch)
		return true
	})
	if err != nil {
		return SessionFacts{}, err
	}
	return f, nil
}

// CodexSessionFacts reads the facts of the Codex rollout at path from its
// first session_meta: none for a rollout without one (an older Codex wrote
// none).
func CodexSessionFacts(path string) (SessionFacts, error) {
	var f SessionFacts
	err := eachRolloutLine(path, `"session_meta"`, func(line []byte) bool {
		var ev struct {
			Type    string `json:"type"`
			Payload struct {
				Originator string          `json:"originator"`
				Source     json.RawMessage `json:"source"`
				Cwd        string          `json:"cwd"`
				Git        struct {
					Branch string `json:"branch"`
				} `json:"git"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &ev) != nil || ev.Type != "session_meta" {
			return true
		}
		p := ev.Payload
		f = SessionFacts{Originator: p.Originator, Source: codexSource(p.Source), Cwd: p.Cwd}
		f.branch(p.Git.Branch)
		return false
	})
	if err != nil {
		return SessionFacts{}, err
	}
	return f, nil
}

// codexSource is a session_meta's source: its string, CodexSourceSubagent
// for an object that names a subagent, else "".
func codexSource(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		if _, ok := obj["subagent"]; ok {
			return CodexSourceSubagent
		}
	}
	return ""
}
