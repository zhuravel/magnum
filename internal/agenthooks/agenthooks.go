// Package agenthooks carries an agent's own hook events (a Claude Code or
// Codex Stop, StopFailure, PermissionRequest, SessionStart, SessionEnd,
// Interrupt or UserPromptSubmit) from a magnum pane to the daemon that
// started the pane. The
// hook script (hooks/magnum-agent-hook.sh, Script) runs `magnum hook-event
// <agent> <event>`, which reads the hook's payload (ReadPayload), keeps a
// few of its fields (Parse) and sends one Unix datagram (Send) to the
// socket the pane's env names (EnvSocket). The daemon's Listener reads it
// (Serve) and hands it on.
//
// An event is a hint, never the source of truth: the daemon reads herdr,
// the transcript or the rollout again before it acts, and polling stays the
// floor. An agent in the pane has the env and can send any datagram, so a
// forged event costs no more than an early look.
//
// The package uses the standard library alone (no config, no registry, no
// other package of magnum's), so the hook's process stays light.
package agenthooks

import "time"

// Version is the Event's V. The fields are additive under it: a later field
// never changes the meaning of an older one, so a daemon reads an event with
// a field it does not know as the event it knows.
const Version = 1

// The agents that send events (Event.Agent, the hook script's first
// argument).
const (
	AgentClaude = "claude"
	AgentCodex  = "codex"
)

// The events (Event.Name, the hook script's second argument).
const (
	NameStop              = "stop"               // the agent ended its turn
	NameStopFailure       = "stop_failure"       // Claude: the turn ended on an API error (Event.Error)
	NamePermissionRequest = "permission_request" // the agent waits on an approval now
	NameSessionStart      = "session_start"      // the session's id and transcript (Event.Source: startup, resume, ...)
	NameSessionEnd        = "session_end"        // Claude: the session ended
	NameInterrupt         = "interrupt"          // Codex: the turn was cut
	NameUserPromptSubmit  = "user_prompt_submit" // a prompt was submitted: a turn begins (the prompt itself is never kept)
)

// PingName is the event `magnum doctor` sends a daemon's hook socket to see
// that it takes datagrams. No hook sends it, and every receiver drops it:
// Serve hands on only the events above.
const PingName = "ping"

// The env of a magnum pane, and the agents' own env the parser reads.
const (
	// EnvSocket is the socket of the daemon that started the pane; without
	// it the hook script exits at once.
	EnvSocket = "MAGNUM_HOOK_SOCKET"
	// EnvBin is the magnum binary the hook script runs.
	EnvBin = "MAGNUM_HOOK_BIN"
	// EnvPane is herdr's id of the pane (Event.Pane).
	EnvPane = "HERDR_PANE_ID"
	// EnvAgent is magnum's own key of the pane (Event.PaneKey): the name
	// of the agent magnum starts there (its session row's agent_name),
	// which needs no herdr.
	EnvAgent = "MAGNUM_AGENT"
	// EnvCodexThread is the Codex thread the hook runs in
	// (Event.CodexThread): a helper's differs from its session's id.
	EnvCodexThread = "CODEX_THREAD_ID"
)

// The transport's bounds.
const (
	// MaxDatagram bounds an event's datagram: 2 KiB, macOS's
	// net.local.dgram.maxdgram.
	MaxDatagram = 2 << 10
	// SendTimeout bounds Send.
	SendTimeout = 200 * time.Millisecond
	// MaxPayload bounds how much of the hook's stdin ReadPayload reads.
	MaxPayload = 64 << 10
	// ReadTimeout bounds ReadPayload.
	ReadTimeout = 100 * time.Millisecond
	// HookBound bounds `magnum hook-event` as a whole: Claude does not
	// enforce a timeout on an async hook.
	HookBound = 300 * time.Millisecond
	// SocketPathLimit is the length at which a Unix socket's path is too
	// long: sun_path holds 104 bytes on macOS, its terminating NUL
	// included.
	SocketPathLimit = 104
)

// Event is one hook event, encoded as JSON with lower-case snake keys. It
// never has a field for the agent's last message, a prompt or a tool's
// input: they carry PR text.
type Event struct {
	V     int    `json:"v"`     // Version
	Agent string `json:"agent"` // AgentClaude or AgentCodex
	Name  string `json:"name"`  // NameStop, ...
	// SessionID is the agent's session id, TranscriptPath its transcript
	// (Claude) or rollout (Codex) as the agent names it, TurnID the turn
	// (Codex), Source what started the session (startup, resume, clear,
	// compact), Error the API error a StopFailure ended on (rate_limit,
	// overloaded, authentication_failed, billing_error, ...), ToolName the
	// tool a permission request is for, AgentID the subagent the event is
	// from (Claude; empty for the session's own).
	SessionID      string `json:"session_id,omitempty"`
	TranscriptPath string `json:"transcript_path,omitempty"`
	TurnID         string `json:"turn_id,omitempty"`
	Source         string `json:"source,omitempty"`
	Error          string `json:"error,omitempty"`
	ToolName       string `json:"tool_name,omitempty"`
	AgentID        string `json:"agent_id,omitempty"`
	// Pane is the pane's HERDR_PANE_ID, PaneKey its MAGNUM_AGENT, CodexThread
	// its CODEX_THREAD_ID. An event names a pane, a pane key or both.
	Pane        string `json:"pane"`
	PaneKey     string `json:"pane_key,omitempty"`
	CodexThread string `json:"codex_thread,omitempty"`
	// Background is how many background tasks a Claude Stop reports still
	// running.
	Background int `json:"background,omitempty"`
}

// knownAgent and knownName report whether a and n are an agent and an event
// this version sends.
func knownAgent(a string) bool { return a == AgentClaude || a == AgentCodex }

func knownName(n string) bool {
	switch n {
	case NameStop, NameStopFailure, NamePermissionRequest, NameSessionStart, NameSessionEnd, NameInterrupt, NameUserPromptSubmit:
		return true
	}
	return false
}
