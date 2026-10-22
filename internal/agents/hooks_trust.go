package agents

// Codex saves the trust of the hooks a hooks review lists in its home's
// config.toml ([hooks.state], a trusted hash per hook) when the review is
// answered "Trust all and continue". On 2026-10-22 only one of the seven
// answers magnum gave after the user's hooks.json changed wrote the file
// (the one whose session's prompt Codex took), so every later session
// asked again. After magnum trusts the hooks it checks that
// config.toml was written (hooksTrustSaved), and when it was not it tells
// the operator, once per hooks.json: magnum never edits the hook trust
// itself.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// EventHooksTrustUnsaved is recorded (warn, subject tool:<kind>) when magnum
// trusted a Codex hooks review and Codex's config.toml did not change, once
// per hooks.json (data: role, agent, pane, hooks: the hooks.json
// fingerprint). A toast says the same (Deps.Toast).
const EventHooksTrustUnsaved = "agents.hooks_trust_unsaved"

// KVHooksTrustUnsaved is the kv key holding the fingerprint of the
// hooks.json whose unsaved trust magnum last told the operator about
// (EventHooksTrustUnsaved): "kind.<kind>.hooks_trust_unsaved". A hooks.json
// with another fingerprint is told about again.
func KVHooksTrustUnsaved(kind string) string { return "kind." + kind + ".hooks_trust_unsaved" }

// Hook trust toast: its title and body, and the notifier key it is sent
// under (with the hooks.json fingerprint).
const (
	hooksTrustTitle = "magnum: Codex did not save the hook trust"
	hooksTrustBody  = "Every new Codex session will ask again. Run `codex` once in a terminal and choose Trust all."
	hooksTrustToast = "agents.hooks_trust_unsaved:"
)

// codexTrustFiles are the config.toml Codex saves the trust of hooks in and
// the hooks.json beside it, in the Codex home of role's panes
// (codexHomeFor); "" and "" when the home is unknown.
func (m *Manager) codexTrustFiles(role Role) (configFile, hooksFile string) {
	var env map[string]string
	if spec, ok := m.roleSpec(role); ok {
		env = m.d.Config.RoleEnv(spec)
	}
	home, configFile := m.codexHomeFor(env)
	if home == "" {
		return "", ""
	}
	return configFile, filepath.Join(home, "hooks.json")
}

// modTime is path's modification time, zero when it cannot be read.
func modTime(path string) time.Time {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

// hooksFingerprint is the fingerprint of the hooks.json at path: its path
// and content (none when unreadable), hashed.
func hooksFingerprint(path string) string {
	b, _ := os.ReadFile(path)
	h := sha256.New()
	h.Write([]byte(path + "\n"))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// hooksTrustSaved checks, after magnum answered a Codex hooks review of role
// at ref "Trust all and continue" and the agent came back, that Codex
// saved the trust: configFile, modified at before when Enter went out,
// changed since. When it did not, it records EventHooksTrustUnsaved and
// sends the toast (Deps.Toast), unless it already told the operator about
// this hooks.json (KVHooksTrustUnsaved).
func (m *Manager) hooksTrustSaved(ctx context.Context, kind string, role Role, ref paneRef, configFile, hooksFile string, before time.Time) {
	if after := modTime(configFile); !after.IsZero() && !after.Equal(before) {
		return
	}
	fp := hooksFingerprint(hooksFile)
	m.hooksTrustMu.Lock()
	defer m.hooksTrustMu.Unlock()
	key := KVHooksTrustUnsaved(kind)
	switch prev, ok, err := m.d.Store.GetKV(ctx, key); {
	case err != nil:
		m.logErr(ctx, err, "agents: hook trust of %s: %v", role, err)
		return
	case ok && prev == fp:
		return // told already
	}
	if err := m.d.Store.SetKV(ctx, key, fp); err != nil {
		m.logErr(ctx, err, "agents: hook trust of %s: %v", role, err)
		return
	}
	m.event(ctx, "tool:"+kind, "warn", EventHooksTrustUnsaved,
		fmt.Sprintf("%s did not save the hook trust: magnum answered the hooks review of %s in %s \"%s\", and $CODEX_HOME/config.toml "+
			"did not change. %s", KindName(kind), role, ref, hooksTrust, hooksTrustBody),
		map[string]any{"role": string(role), "agent": ref.name, "pane": ref.pane, "hooks": fp})
	if m.d.Toast != nil {
		m.d.Toast(ctx, hooksTrustToast+fp[:16], hooksTrustTitle, hooksTrustBody)
	}
}
