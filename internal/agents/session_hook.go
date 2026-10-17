package agents

// herdr learns a Codex session's id (the pane's agent_session, which
// resumable and TurnError need) from a SessionStart hook it installs in
// $CODEX_HOME/hooks.json ("bash ~/.codex/herdr-agent-state.sh session").
// Codex keeps each hook's trust in config.toml's [hooks.state] table, one
// entry per hook keyed "<hooks.json path>:session_start:<group>:<index>"
// with trusted_hash and enabled, and a hooks dialog the operator answers can
// set enabled = false: on 2026-10-16 that turned herdr's hook off, and no
// Codex session started since had an id. magnum never changes that state (it
// is the operator's trust decision): `magnum doctor` reads it
// (ReadCodexSessionHook), and the observer says once per kind and daemon
// start when a session's first turn ends without an id (sessionIDMissing).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// EventSessionIDMissing is recorded (warn, subject tool:<kind>) once per kind
// and Manager when a session's first turn ends without a session id from
// herdr (see sessionIDMissing).
const EventSessionIDMissing = "agents.session_id_missing"

// SessionIDBlind says what goes blind without a Codex session id: resumable
// never resumes the judge and TurnError cannot read the rollout, which the
// pipeline's check for a helper's refusal reads.
const SessionIDBlind = "no session id: judges are never resumed, and a helper's Codex refusal is not seen"

// HerdrSessionHook is the script herdr's Codex SessionStart hook runs; a
// hook whose command names it is herdr's.
const HerdrSessionHook = "herdr-agent-state.sh"

// codexSessionStart is the SessionStart event as hooks.json names it, and
// as the keys of config.toml's [hooks.state] entries do.
const (
	codexSessionStart      = "SessionStart"
	codexSessionStartState = "session_start"
)

// CodexSessionHook is what a Codex home says of herdr's SessionStart hook
// (ReadCodexSessionHook).
type CodexSessionHook struct {
	HooksFile  string // <home>/hooks.json
	ConfigFile string // <home>/config.toml
	// Found: a SessionStart hook of HooksFile names HerdrSessionHook.
	Found bool
	// Disabled are the keys of the [hooks.state] entries that set enabled =
	// false on each such hook, when every one of them has such an entry;
	// nil when one of them runs (its entry enables it, or it has none).
	Disabled []string
}

// hookEntryName is a [hooks.state] key without its hooks.json path
// ("session_start:0:0"), for a message that must not name a pane env value
// (the path is under CODEX_HOME).
func hookEntryName(key string) string {
	if i := strings.LastIndex(key, ":"+codexSessionStartState+":"); i >= 0 {
		return key[i+1:]
	}
	return key
}

// codexHooks is what ReadCodexSessionHook reads of a hooks.json: the
// command of each hook, by event and group.
type codexHooks struct {
	Hooks map[string][]struct {
		Hooks []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	} `json:"hooks"`
}

// codexHookState is what ReadCodexSessionHook reads of a config.toml: its
// [hooks.state] entries.
type codexHookState struct {
	Hooks struct {
		State map[string]struct {
			Enabled *bool `toml:"enabled"`
		} `toml:"state"`
	} `toml:"hooks"`
}

// ReadCodexSessionHook reads herdr's SessionStart hook in the Codex home
// home: whether a SessionStart hook of its hooks.json names
// HerdrSessionHook, and whether config.toml's [hooks.state] entry of each
// such hook (keyed by the hooks.json path, as written or with its symlinks
// resolved) sets enabled = false. ok is false when there is nothing to
// judge: home unknown or no hooks.json there (herdr not installed is
// another check). A missing config.toml, [hooks.state] table or entry
// disables nothing. err names a file that cannot be read or decoded,
// without the decoder's message, which may quote the file. It only reads.
func ReadCodexSessionHook(home string) (h CodexSessionHook, ok bool, err error) {
	if home == "" {
		return h, false, nil
	}
	h.HooksFile, h.ConfigFile = filepath.Join(home, "hooks.json"), filepath.Join(home, "config.toml")
	raw, err := os.ReadFile(h.HooksFile)
	if errors.Is(err, fs.ErrNotExist) {
		return h, false, nil
	}
	if err != nil {
		return h, false, fmt.Errorf("cannot read %s: %w", h.HooksFile, unwrapPath(err))
	}
	var hooks codexHooks
	if err := json.Unmarshal(raw, &hooks); err != nil {
		return h, false, fmt.Errorf("%s is not valid JSON", h.HooksFile)
	}
	var herdrHooks []string // "<group>:<index>" of each herdr hook
	for g, group := range hooks.Hooks[codexSessionStart] {
		for i, hook := range group.Hooks {
			if strings.Contains(hook.Command, HerdrSessionHook) {
				herdrHooks = append(herdrHooks, strconv.Itoa(g)+":"+strconv.Itoa(i))
			}
		}
	}
	h.Found = len(herdrHooks) > 0
	if !h.Found {
		return h, true, nil
	}
	raw, err = os.ReadFile(h.ConfigFile)
	if errors.Is(err, fs.ErrNotExist) {
		return h, true, nil
	}
	if err != nil {
		return h, false, fmt.Errorf("cannot read %s: %w", h.ConfigFile, unwrapPath(err))
	}
	var state codexHookState
	if _, err := toml.Decode(string(raw), &state); err != nil {
		return h, false, fmt.Errorf("%s is not valid TOML", h.ConfigFile)
	}
	prefixes := []string{h.HooksFile}
	if resolved, err := filepath.EvalSymlinks(h.HooksFile); err == nil && resolved != h.HooksFile {
		prefixes = append(prefixes, resolved)
	}
	var disabled []string
	for _, at := range herdrHooks {
		off := ""
		for _, p := range prefixes {
			key := p + ":" + codexSessionStartState + ":" + at
			if e, ok := state.Hooks.State[key]; ok && e.Enabled != nil && !*e.Enabled {
				off = key
				break
			}
		}
		if off == "" {
			return h, true, nil // this one runs
		}
		disabled = append(disabled, off)
	}
	h.Disabled = disabled
	return h, true, nil
}

// unwrapPath drops the path of a *fs.PathError, which the caller names.
func unwrapPath(err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err
	}
	return err
}

// CodexHome is the Codex home a session of role reads (codexHomeFor of its
// pane env: its CODEX_HOME, else the daemon's $CODEX_HOME, else ~/.codex;
// "" in a test binary without Deps.CodexConfig).
func (m *Manager) CodexHome(role config.Role) string {
	if m.d.Config == nil {
		return ""
	}
	home, _ := m.codexHomeFor(m.d.Config.RoleEnv(role))
	return home
}

// promptedTurnEnded reports whether this tick ends a turn of session s
// (its row) that magnum prompted: the agent is idle now and was working
// before (prev, the stored status, set at prevAt, no earlier than its last
// prompt). The observer asks it of a session that has no session id yet,
// none stored and none from herdr this tick.
func promptedTurnEnded(s store.Session, prev herdr.Status, prevAt *time.Time, idle bool) bool {
	return idle && prev == herdr.StatusWorking && s.LastPromptAt != nil && prevAt != nil && !prevAt.Before(*s.LastPromptAt)
}

// sessionIDMissing records EventSessionIDMissing for session s, whose turn
// ended without a session id from herdr (promptedTurnEnded), once per kind
// and Manager (a daemon start), for a kind whose session ids come from
// herdr (session_source "herdr"). The message names herdr's hook and what
// goes blind; for codex it says what the Codex home shows of the hook
// (ReadCodexSessionHook), never its path, a pane env value.
func (m *Manager) sessionIDMissing(ctx context.Context, s store.Session) {
	kind := m.sessionKind(s)
	if k, ok := m.kindSpec(kind); !ok || k.SessionSource != config.SessionHerdr {
		return
	}
	if !firstFor(m, m.sidMissing, kind) {
		return
	}
	data := map[string]any{"kind": kind, "role": s.Role, "session": s.ID}
	msg := fmt.Sprintf("%s: the first turn of %s (session %d) ended without a session id from herdr", kind, s.Role, s.ID)
	if kind != KindCodex {
		msg += fmt.Sprintf(", which learns it from its %s integration's session hook; no session id: its sessions are never resumed", kind)
		m.event(ctx, "tool:"+kind, "warn", EventSessionIDMissing, msg, data)
		return
	}
	data["hook"] = HerdrSessionHook
	hook := "herdr's SessionStart hook (" + HerdrSessionHook + " in $CODEX_HOME/hooks.json)"
	home, _ := m.codexHomeFor(s.Env)
	h, ok, err := ReadCodexSessionHook(home)
	switch {
	case err == nil && ok && !h.Found:
		data["state"] = "missing"
		msg += fmt.Sprintf(": %s is missing: no SessionStart hook there names %s; %s. Reinstall herdr's Codex integration",
			hook, HerdrSessionHook, SessionIDBlind)
	case err == nil && ok && len(h.Disabled) > 0:
		entries := make([]string, len(h.Disabled))
		for i, key := range h.Disabled {
			entries[i] = hookEntryName(key)
		}
		data["state"], data["entries"] = "disabled", entries
		msg += fmt.Sprintf(": %s is disabled: its [hooks.state] entry %s in $CODEX_HOME/config.toml has enabled = false; %s. "+
			"Set enabled = true there (`magnum doctor` names the entry); magnum never changes Codex's hook trust",
			hook, strings.Join(entries, ", "), SessionIDBlind)
	default:
		msg += fmt.Sprintf(", which it learns from %s; %s. `magnum doctor` checks the hook", hook, SessionIDBlind)
	}
	m.event(ctx, "tool:"+kind, "warn", EventSessionIDMissing, msg, data)
}

// firstFor marks key in set (one of the Manager's, under m.mu) and reports
// whether it was not marked yet.
func firstFor[K comparable](m *Manager, set map[K]bool, key K) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if set[key] {
		return false
	}
	set[key] = true
	return true
}
