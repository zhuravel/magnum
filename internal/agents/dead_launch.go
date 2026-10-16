package agents

// A launch that never came up holds its agent name (DECISIONS "A start
// clears a dead launch that holds its agent name, and a fresh start closes
// the panes it replaces"): on talkable#11932 codex-cli died at its TUI
// bootstrap in every judge pane, and herdr kept each failed launch listed as
// an agent (launch_pending, status unknown) on a pane that showed only a
// shell prompt. Every later start of the judge under that name failed with
// agent_name_taken, and the cold judge's quit waited for a name that never
// went, until the operator closed the pane by hand. herdr drops such a name
// only with its pane: agent.rename would keep the launch listed, and
// pane.release_agent releases the lifecycle authority an integration's
// hooks report, not a launch agent.start registered.

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// EventLaunchCleared is recorded (warn) when magnum closed the pane of a
// launch that never came up which held a role's agent name
// (clearDeadLaunch); it names the role, the agent, the pane, its
// replacement and the CLI's own last error line there (paneError), never
// the TUI's text.
const EventLaunchCleared = "pr.launch_cleared"

// maxReplacements bounds how far replacement follows the panes that took
// one another's place.
const maxReplacements = 16

// deadLaunch reports whether herdr lists agent a as a launch that never
// came up: launch_pending or status unknown, never working or blocked.
// Whether its pane runs anything is the caller's to check.
func deadLaunch(a herdr.AgentInfo) bool {
	switch a.AgentStatus {
	case herdr.StatusWorking, herdr.StatusBlocked:
		return false
	}
	return a.LaunchPending || a.AgentStatus == herdr.StatusUnknown
}

// replacement is the pane that took pane's place when magnum closed it to
// clear a launch that never came up (clearDeadLaunch), followed through any
// later replacement; pane itself when none did. herdr never reuses a pane
// id, so the map only grows by the panes this daemon replaced.
func (m *Manager) replacement(pane string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for range maxReplacements {
		next, ok := m.replaced[pane]
		if !ok {
			break
		}
		pane = next
	}
	return pane
}

// clearDeadLaunch closes the pane of the agent herdr lists under name, the
// PR role's agent name, when it is a launch that never came up (deadLaunch)
// and its pane's foreground process is the shell (pane.process_info):
// nothing runs there, and herdr keeps the name for as long as the pane
// lives. A pane whose agent works or is blocked, or whose foreground is
// anything but the shell, is never touched (nothing cleared, no error); nor
// is anything when herdr cannot say what the pane runs (logged).
//
// When that pane is keep (the pane the role is about to start in, or the
// one the session being renewed holds), a replacement is split from it
// first, with the cwd and the pane environment the PR's sessions recorded
// for it (the newest one that recorded that pane with an environment, else
// the role's newest), and named like it; after the close the starting and
// live sessions that pointed at the closed pane point at the replacement,
// and the pane id forwards to it (replacement), so a caller that still
// holds the old id starts in the new pane. Without a recorded environment
// there is no replacement (a pane without the role's identity env would act
// as the wrong account): the pane is closed all the same and those sessions
// become lost, for the next workspace to lay a pane out. A pane herdr
// refuses to close is ErrBusy (the replacement closed again). Each close
// records EventLaunchCleared. closed is the pane closed ("" when nothing
// was), repl its replacement.
func (m *Manager) clearDeadLaunch(ctx context.Context, pr store.PR, role Role, snap herdr.Snapshot, name, keep string) (closed, repl string, err error) {
	a, ok := snap.AgentByName(name)
	if !ok || a.PaneID == "" || !deadLaunch(a) {
		return "", "", nil
	}
	pane := a.PaneID
	pi, err := m.d.Herdr.PaneProcessInfo(ctx, pane)
	switch {
	case isGone(err):
		return "", "", nil
	case err != nil:
		m.logErr(ctx, err, "agents: %s: cannot tell what pane %s runs, where herdr lists %s as a launch that never came up: %v", role, pane, name, err)
		return "", "", nil
	case !herdr.IdleShell(pi):
		m.logf("%s: herdr lists %s in pane %s as a launch that never came up, but the pane runs something: left alone", role, name, pane)
		return "", "", nil
	}
	sessions, err := m.d.Store.SessionsByPR(ctx, pr.ID)
	if err != nil {
		return "", "", fmt.Errorf("agents: %s: clear the launch in pane %s: %w", role, pane, err)
	}
	line := m.paneError(ctx, pane) // before the close: the CLI's own error line
	if pane == keep {
		if repl, err = m.splitReplacement(ctx, snap, sessions, role, pane); err != nil {
			return "", "", fmt.Errorf("agents: %s: replace pane %s of a launch that never came up: %w", role, pane, err)
		}
	}
	if err := m.d.Herdr.PaneClose(ctx, pane); err != nil && !isGone(err) {
		if repl != "" {
			if cerr := m.d.Herdr.PaneClose(context.WithoutCancel(ctx), repl); cerr != nil && !isGone(cerr) {
				m.logErr(ctx, cerr, "agents: close the replacement %s again: %v", repl, cerr)
			}
		}
		return "", "", fmt.Errorf("agents: %s: close pane %s of a launch that never came up: %w: %w", role, pane, ErrBusy, err)
	}
	if err := m.afterClear(ctx, pr, role, sessions, pane, repl); err != nil {
		return pane, repl, err
	}
	why := "status " + string(a.AgentStatus)
	if a.LaunchPending {
		why = "launch pending"
	}
	msg := fmt.Sprintf("%s: herdr listed %s in pane %s as a launch that never came up (%s) over an idle shell; pane closed", role, name, pane, why)
	data := map[string]any{"role": string(role), "agent": name, "pane": pane, "launch_pending": a.LaunchPending, "status": string(a.AgentStatus)}
	if repl != "" {
		msg += ", the role starts in " + repl
		data["replacement"] = repl
	}
	if line != "" {
		data["last_line"] = line
	}
	m.event(ctx, m.prSubject(ctx, pr.ID), "warn", EventLaunchCleared, msg, data)
	return pane, repl, nil
}

// splitReplacement splits a pane from pane (about to be closed) for role,
// with the cwd and environment the PR's sessions recorded (see
// clearDeadLaunch), and names it; "" when no session recorded an
// environment.
func (m *Manager) splitReplacement(ctx context.Context, snap herdr.Snapshot, sessions []store.Session, role Role, pane string) (string, error) {
	src, ok := latest(sessions, role, func(s store.Session) bool { return store.Deref(s.HerdrPaneID) == pane && len(s.Env) > 0 })
	if !ok {
		src, ok = latest(sessions, role, func(s store.Session) bool { return len(s.Env) > 0 })
	}
	if !ok {
		return "", nil
	}
	cwd := cmp.Or(store.Deref(src.Cwd), paneIndex(snap)[pane].Cwd)
	p, err := m.d.Herdr.PaneSplit(ctx, pane, herdr.SplitOptions{Direction: herdr.SplitDown, Cwd: cwd, Env: src.Env, Focus: false})
	if err != nil {
		return "", err
	}
	return p.ID, nil
}

// afterClear records what the close of pane did to the PR's sessions: with
// a replacement, the pane forwards to it, it gets the role's pane label and
// the starting and live sessions that pointed at pane point at it; without
// one, those sessions are lost.
func (m *Manager) afterClear(ctx context.Context, pr store.PR, role Role, sessions []store.Session, pane, repl string) error {
	if repl != "" {
		m.mu.Lock()
		m.replaced[pane] = repl
		m.mu.Unlock()
		if err := m.d.Herdr.PaneRename(ctx, repl, TaggedPaneLabel(m.d.Tag, pr.Number, role)); err != nil {
			m.logErr(ctx, err, "agents: name the replacement %s: %v", repl, err)
		}
	}
	var errs []error
	for _, s := range sessions {
		if !isLive(s) || store.Deref(s.HerdrPaneID) != pane {
			continue
		}
		if repl == "" {
			errs = append(errs, m.markLost(ctx, s))
			continue
		}
		err := m.d.Store.UpdateSession(ctx, s.ID, func(u *store.SessionUpdate) { u.Set("herdr_pane_id", repl) })
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			errs = append(errs, fmt.Errorf("agents: point session %d at the replacement %s: %w", s.ID, repl, err))
		}
	}
	return errors.Join(errs...)
}

// ClearDeadLaunch is clearDeadLaunch for the quit that renews session s
// (the engine's quitAgentGone, once herdr kept listing the agent it quit):
// the name s's agent ran under, and s's pane as the one to keep, so the
// role's start that follows, still holding that pane's id, starts in its
// replacement. cleared reports that herdr no longer lists the name and the
// role keeps a pane: false when herdr lists no launch that never came up
// under the name, its pane runs something, or the pane closed without a
// replacement.
func (m *Manager) ClearDeadLaunch(ctx context.Context, s store.Session) (bool, error) {
	name := store.Deref(s.AgentName)
	if name == "" {
		return false, nil
	}
	pr, err := m.d.Store.PRByID(ctx, s.PRID)
	if err != nil {
		return false, fmt.Errorf("agents: clear the launch of session %d: %w", s.ID, err)
	}
	snap, err := m.d.Herdr.Snapshot(ctx)
	if err != nil {
		return false, fmt.Errorf("agents: clear the launch of session %d: %w", s.ID, err)
	}
	keep := m.replacement(store.Deref(s.HerdrPaneID))
	closed, repl, err := m.clearDeadLaunch(ctx, pr, Role(s.Role), snap, name, keep)
	if err != nil || closed == "" {
		return false, err
	}
	return closed != keep || repl != "", nil
}
