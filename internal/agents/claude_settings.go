package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/zhuravel/magnum/internal/store"
)

// Claude Code's `/model <x>` switches the session and also saves x as the
// user's default model for new interactive sessions (the "model" key of its
// settings.json). magnum switches one session because one model hit its own
// limit, so SwitchModel snapshots that key before it types the command and
// restores exactly it afterwards: the same value, or no key when there was
// none, every other key and the file's layout kept.

// EventDefaultModelRestored is recorded when SwitchModel put the Claude
// settings' default model back (data: session, role, file, model, found;
// model and found are null when the key was absent).
const EventDefaultModelRestored = "agent.default_model_restored"

// claudeSettingsKey is the settings.json key /model writes.
const claudeSettingsKey = "model"

// claudeSettingsPath is the Claude settings file SwitchModel guards:
// Deps.ClaudeSettings, else $CLAUDE_CONFIG_DIR/settings.json, else
// ~/.claude/settings.json. A test binary never falls back to the defaults.
func (m *Manager) claudeSettingsPath() string {
	if m.d.ClaudeSettings != "" {
		return m.d.ClaudeSettings
	}
	if testing.Testing() {
		return ""
	}
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "settings.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "settings.json")
}

// holdDefaultModel runs before SwitchModel types its command into session
// s of kind: for claude it takes the settings lock (one switch at a time,
// so a second switch never snapshots the first one's choice) and
// snapshots the default model. restore puts the snapshot back, records
// EventDefaultModelRestored when that changed the file, and releases the
// lock; call it exactly once. Other kinds, no settings path or an
// unreadable file get a restore that does nothing (logged). err is ctx's
// when it ended while waiting for the lock.
func (m *Manager) holdDefaultModel(ctx context.Context, s store.Session, kind string) (restore func(), err error) {
	noop := func() {}
	if kind != KindClaude {
		return noop, nil
	}
	path := m.claudeSettingsPath()
	if path == "" {
		return noop, nil
	}
	select {
	case m.settingsLock <- struct{}{}:
	case <-ctx.Done():
		return noop, ctx.Err()
	}
	unlock := func() { <-m.settingsLock }
	snap, err := readSettingsKey(path, claudeSettingsKey)
	if err != nil {
		unlock()
		m.logAt(slog.LevelWarn, "agents: %s: cannot read the default model in %s, so a /model switch may change it: %v", s.Role, path, err)
		return noop, nil
	}
	return func() {
		defer unlock()
		found, changed, err := restoreSettingsKey(path, claudeSettingsKey, snap)
		switch {
		case err != nil:
			m.logAt(slog.LevelWarn, "agents: %s: restore the default model in %s: %v", s.Role, path, err)
		case changed:
			bctx := context.WithoutCancel(ctx)
			m.event(bctx, m.prSubject(bctx, s.PRID), "info", EventDefaultModelRestored,
				fmt.Sprintf("%s: restored the default model in %s to %s (the switch had set %s)", s.Role, path, snap, found),
				map[string]any{"session": s.ID, "role": s.Role, "file": path, "model": snap.value(), "found": found.value()})
		}
	}, nil
}

// settingsKey is one top-level key of a JSON settings file as found.
type settingsKey struct {
	present bool
	raw     json.RawMessage // the value verbatim
}

// value is the key's value for an event (nil when absent).
func (k settingsKey) value() any {
	if !k.present {
		return nil
	}
	return k.raw
}

func (k settingsKey) String() string {
	if !k.present {
		return "no default"
	}
	return string(k.raw)
}

// sameJSON reports whether two JSON values are equal ignoring layout.
func sameJSON(a, b json.RawMessage) bool {
	var ca, cb bytes.Buffer
	if json.Compact(&ca, a) != nil || json.Compact(&cb, b) != nil {
		return bytes.Equal(a, b)
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}

// readSettingsKey reads key from the JSON object in path; a missing or
// empty file has no key.
func readSettingsKey(path, key string) (settingsKey, error) {
	cf, err := readConfigFile(path)
	if err != nil || !cf.exists || len(bytes.TrimSpace(cf.data)) == 0 {
		return settingsKey{}, err
	}
	ms, err := parseObject(cf.data)
	if err != nil {
		return settingsKey{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if i := ms.index(key); i >= 0 {
		return settingsKey{present: true, raw: ms[i].val}, nil
	}
	return settingsKey{}, nil
}

// restoreSettingsKey makes key in the JSON object in path equal want (want
// absent: no such key) with an atomic write that keeps every other member,
// their order and the file's layout. found is what the file held; changed
// is false when it already matched (nothing written).
func restoreSettingsKey(path, key string, want settingsKey) (found settingsKey, changed bool, err error) {
	cf, err := readConfigFile(path)
	if err != nil {
		return found, false, err
	}
	data := cf.data
	if !cf.exists || len(bytes.TrimSpace(data)) == 0 {
		if !want.present {
			return found, false, nil
		}
		data = []byte("{}\n")
	}
	ms, err := parseObject(data)
	if err != nil {
		return found, false, fmt.Errorf("parse %s: %w", path, err)
	}
	i := ms.index(key)
	if i >= 0 {
		found = settingsKey{present: true, raw: ms[i].val}
	}
	switch {
	case want.present && found.present && sameJSON(found.raw, want.raw):
		return found, false, nil
	case want.present && found.present:
		ms[i].val = want.raw
	case want.present:
		ms = append(ms, member{key, want.raw})
	case found.present:
		ms = slices.DeleteFunc(ms, func(m member) bool { return m.key == key })
	default:
		return found, false, nil
	}
	out, err := formatLike(data, ms.encode())
	if err != nil {
		return found, false, err
	}
	if err := writeFileAtomic(cf.real, out, cf.mode); err != nil {
		return found, false, err
	}
	return found, true, nil
}
