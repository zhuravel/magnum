package agents

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// StartAgent launches (or adopts) the agent of an agent role in paneID and
// marks its session live. The role's kind (role.AgentKind(), a declared
// [kinds.<name>]) says how:
//
//   - An agent already carrying the role's name is adopted as is, when it
//     is of the role's kind and its pane works in the PR's checkout (the
//     session's cwd, else paneID's); otherwise StartAgent fails, since the
//     name is taken.
//   - With resume set (ignored for a kind with session_source "none"), a
//     pane whose agent_session.value equals resume (herdr restored it) is
//     adopted instead of starting a second copy, and the agent is renamed to
//     the role's name so the conversation is never forked; a restored
//     agent of another kind or checkout is not adopted (logged) and the
//     conversation is resumed in paneID instead.
//   - Otherwise it waits for an idle shell (ErrBusy after IdleShellTimeout),
//     resolves the kind's wrapper mode (Wrapper), trusts the session's cwd in
//     the CLI's config (EnsureTrust; a failure is only logged; a no-op for
//     kinds other than codex and claude) and runs agent.start with
//     Name=AgentName, Kind=the kind, Timeout AgentStartTimeout and the args
//     Kind.Argv builds: resume args, name args with Title (claude --name;
//     kinds without name args but with a rename command are named later, by
//     ObserveSnapshot), the role's model, effort and subagent-cap args, the
//     args that turn off the MCP servers of the Codex config (the kind's
//     mcp_off, read at every launch: mcpServers), the args that keep the
//     checkout's .codex/ out when the PR changes it, or its servers under
//     project_mcp "off" (codexProject against origin/<the PR's base>,
//     recorded by recordProject), the kind's start
//     args, its args only when it is not a wrapper, then role.Args. While
//     the role's model is limited (NoteModelLimit), the model args name the
//     kind's first fallback model that is not, and the session records it
//     (KVSessionModel, an agent.model_switched event).
//   - An agent that comes up blocked (agent.start agent_not_ready, timeout,
//     or status blocked, also when adopted) gets the trust-dialog fallback:
//     within TrustWindow of its start and before its first prompt, a
//     Codex/Claude first-launch trust dialog on screen is answered and the
//     agent awaited until idle (see AnswerTrustDialog); Codex's "Folder
//     access" for a checkout the session treats as untrusted is opened
//     restricted at any time; any other dialog is left for the human.
//
// herdr's agent.start takes no environment: the role's env
// (Config.RoleEnv) reaches the agent through its pane, which EnsureWorkspace
// or EnsurePane created with it.
//
// The session row (created by EnsureWorkspace, or here if missing) gets the
// pane, agent name, agent_kind = the kind, session_id/resumed_from = resume,
// started_at = the launch (an agent launched here) and state live. A fresh session's id appears only after the first prompt;
// Prompt, Observe and RecordSessionID record it then. Preflight is the
// caller's job.
func (m *Manager) StartAgent(ctx context.Context, pr store.PR, role config.Role, paneID, resume string) error {
	r := Role(role.Name)
	if role.IsShell() {
		return fmt.Errorf("agents: start pr %d %s: %w", pr.Number, r, ErrNotAgent)
	}
	kindName := role.AgentKind()
	kind, ok := m.kindSpec(kindName)
	if !ok {
		return fmt.Errorf("agents: start pr %d %s: unknown agent kind %q", pr.Number, r, kindName)
	}
	if kind.SessionSource == config.SessionNone {
		resume = ""
	}
	repo, err := m.d.Store.RepoByID(ctx, pr.RepoID)
	if err != nil {
		return fmt.Errorf("agents: repo of pr %d: %w", pr.Number, err)
	}
	name, title := TaggedAgentName(m.d.Tag, repo.Owner+"/"+repo.Name, pr.Number, r), TaggedTitle(m.d.Tag, repo.Name, pr.Number, r)
	snap, err := m.d.Herdr.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("agents: start %s: %w", name, err)
	}
	sess, err := m.d.Store.LiveSessionByPRRole(ctx, pr.ID, role.Name)
	if errors.Is(err, store.ErrNotFound) {
		x := store.Session{PRID: pr.ID, Role: role.Name, AgentName: &name, AgentKind: store.Ptr(kindName),
			HerdrPaneID: nonEmpty(paneID), State: store.SessionStarting}
		if p, ok := paneIndex(snap)[paneID]; ok {
			x.HerdrWorkspaceID, x.HerdrTabID, x.Cwd = nonEmpty(p.WorkspaceID), nonEmpty(p.TabID), nonEmpty(p.Cwd)
		}
		sess, err = m.d.Store.CreateSession(ctx, x)
	}
	if err != nil {
		return fmt.Errorf("agents: start %s: %w", name, err)
	}

	checkout := store.Deref(sess.Cwd)
	if checkout == "" {
		checkout = paneIndex(snap)[paneID].Cwd
	}
	if a, ok := snap.AgentByName(name); ok {
		if err := adoptable(snap, a, kindName, checkout); err != nil {
			return fmt.Errorf("agents: start %s: the name is taken: %w", name, err)
		}
		if a.AgentStatus == herdr.StatusBlocked {
			if b, ok := m.startAfterTrustDialog(ctx, pr.ID, r, kindName, paneRef{name: name, pane: a.PaneID}, checkout, sessionGate(sess)); ok {
				a = b
			}
		}
		return m.markStarted(ctx, sess, a, name, kindName, store.Deref(sess.ResumedFrom), time.Time{})
	}
	if resume != "" {
		if p, ok := snap.PaneBySession(resume); ok {
			a := herdr.AgentInfo{PaneID: p.ID, WorkspaceID: p.WorkspaceID, TabID: p.TabID, Agent: p.Agent,
				AgentStatus: p.AgentStatus, AgentSession: p.AgentSession}
			for _, x := range snap.Agents {
				if x.PaneID == p.ID {
					a = x
				}
			}
			if err := adoptable(snap, a, kindName, checkout); err != nil {
				m.logf("agents: start %s: not adopting conversation %s restored in pane %s: %v", name, resume, p.ID, err)
			} else {
				agentName := name
				if a.Name != name {
					if err := m.d.Herdr.AgentRename(ctx, p.ID, name); err != nil {
						agentName = "" // address it by pane id instead
					}
				}
				return m.markStarted(ctx, sess, a, agentName, kindName, resume, time.Time{})
			}
		}
	}

	if _, err := m.d.Herdr.WaitIdleShell(ctx, paneID, IdleShellTimeout); err != nil {
		if herdr.IsTimeout(err) {
			return fmt.Errorf("agents: start %s: pane %s: %w: %w", name, paneID, ErrBusy, err)
		}
		return fmt.Errorf("agents: start %s: pane %s: %w", name, paneID, err)
	}
	wrapper, err := m.Wrapper(ctx, kindName)
	if err != nil {
		return err
	}
	dir := store.Deref(sess.Cwd)
	if dir == "" {
		dir = paneIndex(snap)[paneID].Cwd
	}
	if dir != "" {
		if err := m.EnsureTrust(ctx, kindName, dir); err != nil {
			m.logf("%v (the trust dialog fallback still applies)", err)
		}
	}
	ref := paneRef{name: name, pane: paneID}
	model, onFallback := m.startModel(ctx, role, kind)
	base := cmp.Or(store.Deref(pr.BaseRef), repo.DefaultBranch)
	project := m.codexProject(ctx, role, dir, "origin/"+strings.TrimPrefix(base, "origin/"), "")
	args := kind.Argv(config.LaunchArgs{Session: resume, Title: title, Model: model, Effort: role.Effort,
		Subagents: role.MaxSubagents, MCPServers: m.mcpServers(role), ProjectServers: project.servers, Untrusted: project.paths,
		Wrapper: wrapper, Extra: role.Args})
	m.recordProject(ctx, pr.ID, role, dir, project)
	opts := herdr.AgentStartOptions{Name: name, Kind: kindName, PaneID: paneID, Timeout: AgentStartTimeout, Args: args}
	launched := m.now()
	gate := trustGate{since: launched} // a fresh agent was never prompted
	started := func(a herdr.AgentInfo) error {
		if err := m.markStarted(ctx, sess, a, name, kindName, resume, launched); err != nil {
			return err
		}
		if onFallback {
			m.startedOnFallback(ctx, sess, role, model)
		}
		return nil
	}
	res, err := m.d.Herdr.AgentStart(ctx, opts)
	// agent_pane_busy right after the idle-shell check is a race with the
	// shell's startup (rc files, mise hooks): wait and try the same start
	// again rather than letting the caller fall back to a fresh conversation.
	for attempt := 1; err != nil && herdr.IsCode(err, codeAgentPaneBusy) && attempt <= startBusyRetries && ctx.Err() == nil; attempt++ {
		m.logf("start %s: pane %s busy (attempt %d/%d), retrying", name, paneID, attempt, startBusyRetries)
		if serr := m.sleep(ctx, startBusyDelay); serr != nil {
			break
		}
		if _, werr := m.d.Herdr.WaitIdleShell(ctx, paneID, IdleShellTimeout); werr != nil {
			break
		}
		res, err = m.d.Herdr.AgentStart(ctx, opts)
	}
	if err != nil {
		// agent_not_ready: blocked during startup, e.g. at a trust dialog.
		if ctx.Err() == nil && (herdr.IsCode(err, codeAgentNotReady) || herdr.IsCode(err, herdr.CodeAgentBlocked) || herdr.IsTimeout(err)) {
			if a, ok := m.startAfterTrustDialog(ctx, pr.ID, r, kindName, ref, dir, gate); ok {
				return started(withPane(a, paneID))
			}
		}
		return fmt.Errorf("agents: start %s in %s: %w", name, paneID, mapHerdr(err))
	}
	a := withPane(res.Agent, paneID)
	if a.AgentStatus == herdr.StatusBlocked {
		if b, ok := m.startAfterTrustDialog(ctx, pr.ID, r, kindName, ref, dir, gate); ok {
			a = withPane(b, paneID)
		}
	}
	return started(a)
}

// adoptable checks that running agent a may serve a role of kind in the PR's
// checkout: it is of that kind (when herdr names one) and its pane works in
// checkout (both directories known).
func adoptable(snap herdr.Snapshot, a herdr.AgentInfo, kind, checkout string) error {
	if a.Agent != "" && a.Agent != kind {
		return fmt.Errorf("pane %s runs a %s agent, the role runs %s", a.PaneID, a.Agent, kind)
	}
	p, ok := paneIndex(snap)[a.PaneID]
	switch {
	case checkout == "":
		return errors.New("the PR's checkout is unknown")
	case !ok || p.Cwd == "":
		return fmt.Errorf("the working directory of pane %s is unknown", a.PaneID)
	case !sameTree(p.Cwd, checkout):
		return fmt.Errorf("pane %s works in %s, not in the PR's checkout %s", a.PaneID, p.Cwd, checkout)
	}
	return nil
}

func withPane(a herdr.AgentInfo, paneID string) herdr.AgentInfo {
	if a.PaneID == "" {
		a.PaneID = paneID
	}
	return a
}

// markStarted records a running agent of kind on its session row and makes
// it live. An empty name clears agent_name (the agent is addressed by pane
// id). launched is when StartAgent launched the agent (zero for an adopted
// one): it becomes started_at, the start the trust-dialog window and the
// human-activity grace count from.
func (m *Manager) markStarted(ctx context.Context, sess store.Session, a herdr.AgentInfo, name, kind, resume string, launched time.Time) error {
	now := m.now()
	sid := resume
	if a.AgentSession != nil && a.AgentSession.Value != "" {
		sid = a.AgentSession.Value
	}
	err := m.d.Store.TransitionSession(ctx, sess.ID, liveStates, store.SessionLive, func(u *store.SessionUpdate) {
		u.Set("agent_name", nonEmpty(name))
		u.Set("agent_kind", nonEmpty(kind))
		u.Set("herdr_pane_id", a.PaneID)
		if a.WorkspaceID != "" {
			u.Set("herdr_workspace_id", a.WorkspaceID)
		}
		if a.TabID != "" {
			u.Set("herdr_tab_id", a.TabID)
		}
		if sid != "" {
			u.Set("session_id", sid)
		}
		if resume != "" {
			u.Set("resumed_from", resume)
		}
		u.Set("idle_ticks", 0)
		if a.AgentStatus != "" {
			u.Set("agent_status", string(a.AgentStatus))
			u.Set("agent_status_at", now)
		}
		if !launched.IsZero() {
			u.Set("started_at", launched)
		}
	})
	if err != nil {
		return fmt.Errorf("agents: mark session %d live: %w", sess.ID, err)
	}
	m.markedLive(sess.ID, now)
	// A fresh conversation of a kind without name args (codex) starts
	// untitled and would show as "Codex" in herdr until its first turn; the
	// rename command works on an idle fresh thread (probe 21), so type it
	// now. The namer repeats it during the first turn, when codex titles the
	// thread itself. A resumed conversation keeps its name.
	if resume == "" && a.AgentStatus == herdr.StatusIdle && a.PaneID != "" {
		if cur, err := m.d.Store.SessionByID(ctx, sess.ID); err == nil {
			m.nameAgent(ctx, cur, a.PaneID, a.TerminalTitleStripped)
		}
	}
	return nil
}

// Wrapper reports whether the kind's command is a zsh wrapper function (or
// alias) that supplies its own flags, per [kinds.<kind>] wrapper: "true" and
// "false" are taken as is; "auto" (or empty) probes
// `zsh -ic 'whence -w <kind>'` once per kind and caches the answer. A probe
// that cannot tell assumes a wrapper (the M0 finding) and is retried next
// time. An undeclared kind is an error.
func (m *Manager) Wrapper(ctx context.Context, kind string) (bool, error) {
	k, ok := m.kindSpec(kind)
	if !ok {
		return false, fmt.Errorf("agents: wrapper: unknown agent kind %q", kind)
	}
	switch mode := strings.ToLower(strings.TrimSpace(k.Wrapper)); mode {
	case config.WrapperTrue:
		return true, nil
	case config.WrapperFalse:
		return false, nil
	case "", config.WrapperAuto:
	default:
		return false, fmt.Errorf("agents: kinds.%s.wrapper = %q: want auto, true or false", kind, k.Wrapper)
	}
	m.mu.Lock()
	v, ok := m.wrapper[kind]
	m.mu.Unlock()
	if ok {
		return v, nil
	}
	res, _ := m.d.Runner.Run(ctx, execx.Cmd{Name: "zsh", Args: []string{"-ic", "whence -w " + kind},
		Timeout: WrapperProbeTimeout, NoTTY: true, Label: "probe " + kind + " wrapper"})
	v, ok = parseWhence(string(res.Stdout), kind)
	if !ok {
		return true, nil
	}
	m.mu.Lock()
	m.wrapper[kind] = v
	m.mu.Unlock()
	return v, nil
}

// parseWhence reads `whence -w <kind>` output ("codex: function"); ok is
// false when no line names kind (zsh failed, noisy rc output only).
func parseWhence(out, kind string) (wrapper, ok bool) {
	for _, line := range strings.Split(out, "\n") {
		rest, found := strings.CutPrefix(strings.TrimSpace(line), kind+":")
		if !found {
			continue
		}
		switch strings.TrimSpace(rest) {
		case "function", "alias":
			return true, true
		default: // command, builtin, none, ...
			return false, true
		}
	}
	return false, false
}

// RecordSessionID reads the session's pane and stores agent_session.value
// as session_id when herdr reports one (after the first prompt) and it
// differs. It returns the id now known ("" before the first prompt).
func (m *Manager) RecordSessionID(ctx context.Context, s store.Session) (string, error) {
	pane := store.Deref(s.HerdrPaneID)
	if pane == "" {
		return store.Deref(s.SessionID), nil
	}
	p, err := m.d.Herdr.PaneGet(ctx, pane)
	if err != nil {
		return store.Deref(s.SessionID), fmt.Errorf("agents: session id of %s: %w", pane, err)
	}
	if p.AgentSession == nil || p.AgentSession.Value == "" {
		return store.Deref(s.SessionID), nil
	}
	if err := m.setSessionID(ctx, s, p.AgentSession.Value); err != nil {
		return store.Deref(s.SessionID), err
	}
	return p.AgentSession.Value, nil
}

func (m *Manager) setSessionID(ctx context.Context, s store.Session, id string) error {
	if id == "" || id == store.Deref(s.SessionID) {
		return nil
	}
	if err := m.d.Store.UpdateSession(ctx, s.ID, func(u *store.SessionUpdate) { u.Set("session_id", id) }); err != nil {
		return fmt.Errorf("agents: record session id %d: %w", s.ID, err)
	}
	return nil
}
