package agents

// Codex saves the trust of the hooks a hooks review lists in its home's
// config.toml ([hooks.state], a trusted hash per hook place) when the
// review is answered "Trust all and continue". On 2026-10-22 only one of the
// seven answers magnum gave after the user's hooks.json changed wrote the
// file (the one whose session's prompt Codex took), so every later session
// asked again. After magnum trusts the hooks it checks that Codex saved
// them (hooksTrustSaved): a hook whose place had no trust entry before
// Enter and still has none afterwards was not saved, and magnum tells the
// operator, once per hooks.json. It reads the entries, not config.toml's
// modification time, which another session's answer, a project's trust or
// a /model switch also moves, and which a second answer to a trust a first
// one saved leaves alone (2026-10-23). magnum never edits the hook trust
// itself.

import (
	"context"
	"fmt"
	"slices"
)

// EventHooksTrustUnsaved is recorded (warn, subject tool:<kind>) when magnum
// trusted a Codex hooks review and a hook whose place had no trust entry in
// Codex's config.toml before still has none, once per hooks.json (data:
// role, agent, pane, places: those hooks' places, hooks: the hooks.json
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

// codexTrustHome is the Codex home of role's panes (codexHomeFor), where
// Codex saves the trust of the hooks of its hooks.json; "" when unknown.
func (m *Manager) codexTrustHome(role Role) string {
	var env map[string]string
	if spec, ok := m.roleSpec(role); ok {
		env = m.d.Config.RoleEnv(spec)
	}
	home, _ := m.codexHomeFor(env)
	return home
}

// hooksTrustSaved checks, after magnum answered a Codex hooks review of role
// at ref "Trust all and continue" and the agent came back, that Codex
// saved the trust of the hooks of home's hooks.json whose places had no
// trust entry before Enter (before, ReadCodexHooksTrust's): a hook among
// them whose place still has none (ReadCodexHooksTrust again) records
// EventHooksTrustUnsaved and sends the toast (Deps.Toast), unless magnum
// already told the operator about this hooks.json (KVHooksTrustUnsaved).
// A file it cannot read says nothing.
func (m *Manager) hooksTrustSaved(ctx context.Context, kind string, role Role, ref paneRef, home string, before CodexHooksTrust) {
	after, ok, err := ReadCodexHooksTrust(home)
	if err != nil || !ok {
		return
	}
	var still []CodexHookAt
	for _, h := range after.Untrusted {
		if slices.ContainsFunc(before.Untrusted, func(b CodexHookAt) bool { return b.Place == h.Place }) {
			still = append(still, h)
		}
	}
	if len(still) == 0 {
		return
	}
	fp := after.Fingerprint
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
	places := make([]string, len(still))
	for i, h := range still {
		places[i] = h.Place
	}
	m.event(ctx, "tool:"+kind, "warn", EventHooksTrustUnsaved,
		fmt.Sprintf("%s did not save the hook trust: magnum answered the hooks review of %s in %s \"%s\", and $CODEX_HOME/config.toml "+
			"still has no trust entry for %s. %s", KindName(kind), role, ref, hooksTrust, hookPlaces(still), hooksTrustBody),
		map[string]any{"role": string(role), "agent": ref.name, "pane": ref.pane, "places": places, "hooks": fp})
	if m.d.Toast != nil {
		m.d.Toast(ctx, hooksTrustToast+fp[:16], hooksTrustTitle, hooksTrustBody)
	}
}
