package engine

// The daemon's PATH from the login shell ([daemon] shell_path). launchd gives
// the daemon a fixed PATH (mise's shims and the system directories), while
// the operator's tools, DBngin's MySQL, PostgreSQL and Redis among them, are
// on the PATH only their shell's exports build. With the key on, the daemon
// command asks the login shell for its PATH before anything runs and appends
// the directories the daemon's own lacks (internal/cli); the engine records
// what it took at its start: one event and a kv row `magnum status` reads.

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// KVDaemonShellPATH is what the running daemon took from the login shell's
// PATH at its start, as ShellPATH's JSON (the count, never a directory);
// absent with [daemon] shell_path off.
const KVDaemonShellPATH = "daemon.shell_path"

// ShellPATH is what the daemon's start took from the operator's login
// shell's PATH ([daemon] shell_path).
type ShellPATH struct {
	// Shell is the login shell's base name ("zsh").
	Shell string `json:"shell"`
	// Added are the directories appended to the daemon's PATH, in the
	// shell's order. Only the debug log names them: a directory's name can
	// carry a secret's path, so the event and the kv row keep Count.
	Added []string `json:"-"`
	// Count is how many directories were appended.
	Count int `json:"added"`
	// Failed says why the shell gave no PATH ("answered nothing in 20s",
	// "exited with status 1", "printed no PATH", "could not start"); "" when
	// it gave one. The daemon's PATH is then launchd's alone.
	Failed string `json:"failed,omitempty"`
}

// Message is the daemon.shell_path event's text: "PATH from zsh: 4
// directories added", or why the launchd PATH stays.
func (s ShellPATH) Message() string {
	if s.Failed != "" {
		return "the login shell (" + s.Shell + ") " + s.Failed + ". The launchd PATH stays"
	}
	return "PATH from " + s.Shell + ": " + textx.Count(s.Count, "directory", "directories") + " added"
}

// ReadShellPATH is what the running daemon took from the login shell's PATH
// at its start (KVDaemonShellPATH); false with [daemon] shell_path off or
// before a daemon recorded it.
func ReadShellPATH(ctx context.Context, st *store.Store) (ShellPATH, bool) {
	v, ok, err := st.GetKV(ctx, KVDaemonShellPATH)
	if err != nil || !ok || v == "" {
		return ShellPATH{}, false
	}
	var s ShellPATH
	if json.Unmarshal([]byte(v), &s) != nil {
		return ShellPATH{}, false
	}
	return s, true
}

// recordShellPATH writes what this start took from the login shell's PATH
// (Options.ShellPATH) for `magnum status` and records it in one event (warn
// when the shell gave none); the directories go to the debug log alone.
// With [daemon] shell_path off it removes an earlier start's row.
func (e *Engine) recordShellPATH(ctx context.Context) {
	s := e.shellPATH
	if s == nil {
		e.delKV(ctx, KVDaemonShellPATH)
		return
	}
	if raw, err := json.Marshal(s); err == nil {
		e.setKV(ctx, KVDaemonShellPATH, string(raw))
	}
	level := "info"
	if s.Failed != "" {
		level = "warn"
	}
	e.event(ctx, level, "", "daemon.shell_path", s.Message(), map[string]any{"shell": s.Shell, "added": s.Count})
	if len(s.Added) > 0 {
		e.log.Debug("PATH from the login shell", "shell", s.Shell, "added", execx.Redact(strings.Join(s.Added, ":")))
	}
}
