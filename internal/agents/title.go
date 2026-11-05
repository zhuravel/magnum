package agents

import (
	"context"
	"fmt"
	"strings"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// Title is the terminal title of a PR role's pane, which herdr's agents
// sidebar shows (terminal_title_stripped): "PR #<N> <role> - <repo>", e.g.
// "PR #729 codex-judge - talkable", "PR #729 claude-review - talkable".
// repo is the name part of "owner/name" (or a bare name); without one the
// title is the pane label "PR #<N> <role>".
//
// A kind with name args gets it at launch (claude --name, see StartAgent), a
// kind with only a rename command through that command typed while it works
// (codex /rename, see nameAgent), and a shell role from the OSC 0 prefix of
// its command line (ShellData.Title). A Manager's agents take agentTitle,
// with Deps.Tag and Deps.OwnerName's owner in place of "PR #<N>".
func Title(repo string, number int, role Role) string {
	return ownerTitle("", repo, prOwner(number), role)
}

// TaggedTitle is Title under a tag (Deps.Tag): "eval PR #729 codex-judge -
// talkable". An empty tag is Title.
func TaggedTitle(tag, repo string, number int, role Role) string {
	return ownerTitle(tag, repo, prOwner(number), role)
}

// prOwner is how a PR's agents are named by default: "PR #729".
func prOwner(number int) string { return fmt.Sprintf("PR #%d", number) }

// ownerTitle is TaggedTitle for an owner named owner ("PR #729", or a
// factory's "PR-12345" or "task 12"): "<tag> <owner> <role> - <repo>", the
// tag and the repo left out when empty.
func ownerTitle(tag, repo, owner string, role Role) string {
	if i := strings.LastIndexByte(repo, '/'); i >= 0 {
		repo = repo[i+1:]
	}
	title := owner + " " + role.Label()
	if repo != "" {
		title += " - " + repo
	}
	if tag == "" {
		return title
	}
	return tag + " " + title
}

// ownerName is how pr is named in the titles and pane labels of its agents:
// Deps.OwnerName's answer, else "PR #<number>".
func (m *Manager) ownerName(pr store.PR) string {
	if m.d.OwnerName != nil {
		if name := m.d.OwnerName(pr); name != "" {
			return name
		}
	}
	return prOwner(pr.Number)
}

// agentTitle is the title of role r's agent in pr's repository repo (name
// part), under Deps.Tag and Deps.OwnerName: TaggedTitle for the PRs of
// magnum's own reviews.
func (m *Manager) agentTitle(pr store.PR, repo string, r Role) string {
	return ownerTitle(m.d.Tag, repo, m.ownerName(pr), r)
}

// paneLabel is the herdr pane name of role r in pr's workspace, under
// Deps.Tag and Deps.OwnerName: TaggedPaneLabel for the PRs of magnum's own
// reviews.
func (m *Manager) paneLabel(pr store.PR, r Role) string {
	return m.agentTitle(pr, "", r)
}

// TitleAttempts caps the rename attempts per session row (one generation; a
// restart creates a new row and a fresh budget).
const TitleAttempts = 5

// titleState is the namer's progress for one session row. Kept in memory
// only: after a daemon restart a mistitled agent gets a new budget.
type titleState struct {
	title    string // Title of the session's PR role, resolved once
	command  string // the kind's rename command for title
	attempts int
	tick     uint64 // observe tick of the last attempt (valid when attempts > 0)
}

// nameAgent types the kind's rename command (Kind.RenameCommand, e.g.
// `/rename <Title>` for codex) into an agent's pane when current (its
// terminal title) does not contain Title. Only kinds without name args
// ([kinds.<kind>] name) that have a rename command are named this way. Codex
// has no --name flag and names a fresh thread itself a few seconds into the
// first turn, overwriting an earlier rename, so this runs on every tick
// while magnum's turn works (and once right after Submit sees it working),
// never while blocked or idle (an idle composer may hold a half-typed
// prompt). At most one attempt per observe tick and TitleAttempts per
// session row; a title that already matches (a resumed conversation keeps
// its name) costs nothing. Best effort: failures are only logged.
func (m *Manager) nameAgent(ctx context.Context, s store.Session, paneID, current string) {
	if paneID == "" || !m.isAgentSession(s) {
		return
	}
	kind, ok := m.kindSpec(m.sessionKind(s))
	if !ok || len(kind.Name) > 0 || kind.Rename == "" {
		return
	}
	m.mu.Lock()
	st := m.titles[s.ID]
	if st == nil {
		st = &titleState{}
		m.titles[s.ID] = st
	}
	title, command, spent := st.title, st.command, st.attempts >= TitleAttempts || (st.attempts > 0 && st.tick == m.tick)
	m.mu.Unlock()
	if spent {
		return
	}
	if title == "" {
		pr, err := m.d.Store.PRByID(ctx, s.PRID)
		if err != nil {
			m.logErr(ctx, err, "agents: title of session %d: %v", s.ID, err)
			return
		}
		repo, err := m.repoName(ctx, pr)
		if err != nil {
			m.logErr(ctx, err, "agents: title of session %d: %v", s.ID, err)
			return
		}
		title = m.agentTitle(pr, repo, Role(s.Role))
		command = kind.RenameCommand(title)
		m.mu.Lock()
		st.title, st.command = title, command
		m.mu.Unlock()
	}
	if strings.Contains(current, title) {
		return
	}
	m.mu.Lock()
	if st.attempts >= TitleAttempts || (st.attempts > 0 && st.tick == m.tick) {
		m.mu.Unlock()
		return // a concurrent Submit/Observe took this tick's attempt
	}
	st.attempts++
	st.tick = m.tick
	n := st.attempts
	m.mu.Unlock()
	if err := m.d.Herdr.PaneRun(ctx, paneID, command); err != nil {
		m.logErr(ctx, err, "agents: rename %s pane %s to %q (attempt %d/%d): %v", s.Role, paneID, title, n, TitleAttempts, err)
	}
}

// nextObserveTick starts an observe tick for the namer and the
// permission-prompt policy and forgets the sessions that are no longer
// starting/live.
func (m *Manager) nextObserveTick(live []store.Session) {
	keep := make(map[int64]bool, len(live))
	for _, s := range live {
		keep[s.ID] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tick++
	for id := range m.titles {
		if !keep[id] {
			delete(m.titles, id)
		}
	}
	for id := range m.denies {
		if !keep[id] {
			delete(m.denies, id)
		}
	}
	for id := range m.liveAt {
		if !keep[id] {
			delete(m.liveAt, id)
		}
	}
	for id := range m.bg {
		if !keep[id] {
			delete(m.bg, id)
		}
	}
}

// terminalTitle is the title herdr shows for an agent's pane: the stripped
// form (no spinner or status glyph) when herdr reports one, else the raw one.
func terminalTitle(a herdr.AgentInfo, p herdr.Pane) string {
	for _, t := range []string{a.TerminalTitleStripped, p.TerminalTitleStripped, a.TerminalTitle, p.TerminalTitle} {
		if t != "" {
			return t
		}
	}
	return ""
}
