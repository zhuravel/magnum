// Package agents runs magnum's review agents inside herdr: one workspace per
// PR with a pane per configured role (config.Role: the judge plus reviewers
// such as claude-review, the codex-review shell and claude-simplify), agent
// start (fresh or resumed) on any configured kind (config.Kind: codex, claude,
// droid, omp, ...), prompts, per-tick completion tracking, pane-output health
// classification, login preflight, quit, park and crash recovery.
//
// Durable state lives in the store's sessions and runs rows; herdr is only
// observed. Prompts are rendered from the configured prompt files (see
// RenderPrompt and Manager.RolePrompt); PR titles and bodies are never
// interpolated into them.
//
// Agent CLIs are often launched through the user's zsh wrapper functions
// (herdr types the agent command into the pane's shell), so magnum passes
// only the args Kind.Argv builds; a kind's args are appended only when it is
// not a wrapper (see Manager.Wrapper).
package agents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// Role is a review role's configured name (config.Role.Name), the value
// stored in sessions.role and runs.role. Resolve what users type with
// config.Config.RoleByNameOrAlias; the role's definition (kind, prompts,
// output, ...) is the config.Role of that name.
type Role string

// The built-in roles (config.DefaultRoles), for defaults and tests.
const (
	RoleJudge       Role = config.RoleCodexJudge     // codex-judge: persistent Codex session that posts the review
	RoleClaude      Role = config.RoleClaudeReview   // claude-review: persistent Claude session running /code-review
	RoleCodexReview Role = config.RoleCodexReview    // codex-review: shell pane running `command codex review`
	RoleSimplify    Role = config.RoleClaudeSimplify // claude-simplify: on-demand Claude session running /simplify
)

// Agent kinds (herdr agent.start --kind, sessions.agent_kind) of the
// built-in kinds; any [kinds.<name>] is a kind too.
const (
	KindCodex  = config.KindCodex
	KindClaude = config.KindClaude
	KindDroid  = config.KindDroid
	KindOMP    = config.KindOMP
	KindShell  = config.KindShell // sessions.agent_kind of a shell role's pane
)

// Timeouts used against herdr and the CLIs.
const (
	AgentStartTimeout   = 120 * time.Second // agent.start timeout_ms
	PromptAckTimeout    = 60 * time.Second  // agent.prompt --wait --until working|blocked
	IdleShellTimeout    = 60 * time.Second  // a fresh pane's shell (oh-my-zsh + mise) must be idle by then
	QuitIdleTimeout     = 10 * time.Second  // after two ctrl+c the pane must be a shell again
	PreflightTimeout    = 30 * time.Second
	WrapperProbeTimeout = 20 * time.Second
)

// CompletionIdleTicks is how many consecutive idle|done observations end a turn.
const CompletionIdleTicks = 2

// humanGrace keeps a session from being flagged as human-driven right after
// magnum started or prompted it (startup output and acks can read as working).
const humanGrace = 2 * time.Minute

// Sentinel errors; match with errors.Is.
var (
	// ErrBlocked: the agent waits on an approval/trust dialog (herdr
	// agent_blocked, or the prompt ack came back blocked). The CLIs'
	// first-launch folder-trust dialogs are answered Yes (EnsureTrust
	// prevents them; AnswerTrustDialog is the fallback); a permission prompt
	// during a run is answered No by the observer when the kind's
	// on_permission_prompt is "deny"; nothing is ever approved otherwise.
	ErrBlocked = errors.New("agent blocked")
	// ErrStalled: herdr accepted the text but the agent never started working
	// (agent_prompt_stalled). The run is left submitted, never re-sent.
	ErrStalled = errors.New("agent prompt stalled")
	// ErrTimeout: herdr's own timeout or a client deadline.
	ErrTimeout = errors.New("agent timed out")
	// ErrLoginRequired: Preflight found the CLI logged out.
	ErrLoginRequired = errors.New("agent login required")
	// ErrBusy: an agent is working/blocked, a shell runs a command, or a run is
	// in flight, so the requested change (park, quit, move, shell command) waits.
	ErrBusy = errors.New("agent busy")
	// ErrHumanActive: a human typed into the session within daemon.human_cooldown.
	ErrHumanActive = errors.New("human active in agent pane")
	// ErrNoSession: the role has no live session (EnsureWorkspace/StartAgent first).
	ErrNoSession = errors.New("no live agent session")
	// ErrNotAgent: the role is a shell role (kind "shell", e.g. codex-review),
	// whose pane has no agent.
	ErrNotAgent = errors.New("role has no agent")
	// ErrUnknownTemplate: a prompt name exists neither in pipeline.prompts_dir
	// nor among the embedded defaults (the same sentinel as
	// config.ErrPromptNotFound).
	ErrUnknownTemplate = config.ErrPromptNotFound
)

// Herdr is the subset of *herdr.Client this package uses.
type Herdr interface {
	Snapshot(ctx context.Context) (herdr.Snapshot, error)
	WorkspaceCreate(ctx context.Context, o herdr.WorkspaceCreateOptions) (herdr.WorkspaceCreated, error)
	WorkspaceClose(ctx context.Context, workspaceID string) error
	PaneSplit(ctx context.Context, paneID string, o herdr.SplitOptions) (herdr.Pane, error)
	PaneRename(ctx context.Context, paneID, label string) error
	PaneRun(ctx context.Context, paneID, command string) error
	PaneWaitOutput(ctx context.Context, paneID string, o herdr.WaitOutputOptions) (herdr.OutputMatch, error)
	PaneRead(ctx context.Context, paneID string, o herdr.ReadOptions) (herdr.ReadResult, error)
	PaneGet(ctx context.Context, paneID string) (herdr.Pane, error)
	PaneSendKeys(ctx context.Context, paneID string, keys ...string) error
	PaneProcessInfo(ctx context.Context, paneID string) (herdr.ProcessInfo, error)
	WaitIdleShell(ctx context.Context, paneID string, timeout time.Duration) (herdr.ProcessInfo, error)
	AgentStart(ctx context.Context, o herdr.AgentStartOptions) (herdr.AgentStarted, error)
	AgentPrompt(ctx context.Context, target, text string, wait *herdr.PromptWait) (herdr.AgentInfo, error)
	AgentRead(ctx context.Context, target string, o herdr.ReadOptions) (herdr.ReadResult, error)
	AgentRename(ctx context.Context, target, name string) error
	AgentSendKeys(ctx context.Context, target string, keys ...string) error
}

var _ Herdr = (*herdr.Client)(nil)

// Deps are the Manager's collaborators.
type Deps struct {
	Herdr  Herdr
	Store  *store.Store
	Runner execx.Runner // wrapper probe and preflight (read-only commands)
	Config *config.Config
	Layout paths.Layout
	Clock  func() time.Time // nil = time.Now
	// Sleep waits between polls (quit, trust-dialog readiness); nil = a
	// timer honouring ctx.
	Sleep func(ctx context.Context, d time.Duration) error

	// CodexConfig and ClaudeConfig are the CLI config files EnsureTrust
	// edits; "" = the CLIs' defaults ($CODEX_HOME/config.toml or
	// ~/.codex/config.toml; $CLAUDE_CONFIG_DIR/.claude.json or
	// ~/.claude.json). A test binary never falls back to the defaults.
	CodexConfig  string
	ClaudeConfig string
	// ClaudeSettings is the Claude Code settings file whose default model
	// SwitchModel restores after a /model switch; "" = the CLI's default
	// ($CLAUDE_CONFIG_DIR/settings.json or ~/.claude/settings.json). A test
	// binary never falls back to the defaults.
	ClaudeSettings string
	// Log receives one line per trust entry added and per trust dialog
	// answered (optional).
	Log execx.Logger
	// Tag marks agents started outside the PR's own rounds ("eval" for
	// magnum eval): it joins their names' hash (TaggedAgentName) and leads
	// their titles and pane labels (TaggedTitle). "" = the PR's own agents.
	Tag string
}

// Manager drives agent sessions. Safe for concurrent use: the daemon tick
// (Observe) and per-round goroutines (Prompt, RunShell) share it.
type Manager struct {
	d     Deps
	sleep func(ctx context.Context, d time.Duration) error

	mu      sync.Mutex
	wrapper map[string]bool // kind -> wrapper function detected (auto mode cache)
	// health holds each kind's compiled classifier rules ("" = the defaults).
	health map[string][]healthRule
	// tick counts ObserveSnapshot calls; titles is the namer's state per
	// session row (see nameAgent), denies the permission-prompt policy's
	// (see answerPermission).
	tick   uint64
	titles map[int64]*titleState
	denies map[int64]*denyState
	// liveAt is when this Manager marked each session live (markStarted, a
	// rebind in Submit): an observe tick never judges a session lost from a
	// snapshot taken before that (see LostGrace).
	liveAt map[int64]time.Time
	// switching marks the sessions SwitchModel is switching right now.
	switching map[int64]bool
	// settingsLock (capacity 1) serializes the Claude switches, which share
	// one settings file (see holdDefaultModel).
	settingsLock chan struct{}
}

// New returns a Manager.
func New(d Deps) *Manager {
	sleep := sleepCtx
	if d.Sleep != nil {
		sleep = d.Sleep
	}
	return &Manager{d: d, sleep: sleep, wrapper: map[string]bool{}, health: map[string][]healthRule{},
		titles: map[int64]*titleState{}, denies: map[int64]*denyState{}, liveAt: map[int64]time.Time{},
		switching: map[int64]bool{}, settingsLock: make(chan struct{}, 1)}
}

// markedLive records that session id became live at t (see liveAt).
func (m *Manager) markedLive(id int64, t time.Time) {
	m.mu.Lock()
	m.liveAt[id] = t
	m.mu.Unlock()
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (m *Manager) now() time.Time {
	if m.d.Clock != nil {
		return m.d.Clock()
	}
	return time.Now()
}

// Label is the role's user-visible name: the role name itself (pane labels,
// titles, agent names and CLI output use it).
func (r Role) Label() string { return string(r) }

// TaggedPaneLabel is the herdr pane name for a role under a tag (Deps.Tag):
// "PR #N <role>", e.g. "PR #729 codex-judge" (TaggedTitle without a repo).
func TaggedPaneLabel(tag string, number int, r Role) string { return TaggedTitle(tag, "", number, r) }

const agentNameMax = 32 // herdr: [a-z][a-z0-9_-]{0,31}

// agentHashLen is the length of AgentName's repository hash (hex digits).
const agentHashLen = 6

var nameJunk = regexp.MustCompile(`[^a-z0-9_-]+`)
var nameDashes = regexp.MustCompile(`-{2,}`)

// sanitizeName lowercases s, replaces every run of characters outside
// [a-z0-9_-] with "-", collapses dashes and trims "-" and "_" at both ends.
func sanitizeName(s string) string {
	s = nameJunk.ReplaceAllString(strings.ToLower(s), "-")
	return strings.Trim(nameDashes.ReplaceAllString(s, "-"), "-_")
}

// AgentName is the herdr agent name of a PR role: "mg-<N>-<role>-<hash>"
// (e.g. mg-11920-codex-judge-1a2b3c), where repo is "owner/name" and hash is
// the first 6 hex digits of the SHA-256 of its lower-cased form. herdr agent
// names are global, so the hash keeps the same PR number of two repositories
// apart. The role is sanitized (lowercase, other characters than
// [a-z0-9_-] become "-") and trimmed when the name would exceed 32
// characters; the hash of a trimmed role covers the role's full name too, so
// the name stays unique per (owner, repo, number, role) and always matches
// herdr's ^[a-z][a-z0-9_-]{0,31}$.
func AgentName(repo string, number int, role Role) string {
	return TaggedAgentName("", repo, number, role)
}

// TaggedAgentName is AgentName for agents started under a tag (Deps.Tag,
// "eval" for magnum eval): the tag joins the hashed key, so a replay of a PR
// never shares, adopts or collides with the agents of the PR's own rounds.
// An empty tag is AgentName.
func TaggedAgentName(tag string, repo string, number int, role Role) string {
	label := sanitizeName(string(role))
	if label == "" {
		label = "role"
	}
	key := strings.ToLower(repo)
	if tag != "" {
		key += "\x00tag:" + tag
	}
	prefix := "mg-" + strconv.Itoa(number) + "-"
	if budget := agentNameMax - len(prefix) - 1 - agentHashLen; len(label) > budget {
		label = strings.TrimRight(label[:max(budget, 0)], "-_")
		key += "\x00" + string(role)
	}
	sum := sha256.Sum256([]byte(key))
	hash := hex.EncodeToString(sum[:])[:agentHashLen]
	if label == "" {
		return prefix + hash
	}
	return prefix + label + "-" + hash
}

// roleSpec is the configured role named r (or aliased r); false when the
// configuration has no such role.
func (m *Manager) roleSpec(r Role) (config.Role, bool) {
	if strings.TrimSpace(string(r)) == "" || m.d.Config == nil {
		return config.Role{}, false
	}
	return m.d.Config.RoleByNameOrAlias(nil, string(r))
}

// shellRole reports whether r is a configured shell role.
func (m *Manager) shellRole(r Role) bool {
	spec, ok := m.roleSpec(r)
	return ok && spec.IsShell()
}

// kindSpec is the merged [kinds.<name>] spec.
func (m *Manager) kindSpec(kind string) (config.Kind, bool) {
	if kind == "" || kind == KindShell || m.d.Config == nil {
		return config.Kind{}, false
	}
	return m.d.Config.KindSpec(kind)
}

// sessionKind is a session row's kind: its agent_kind ("shell" for a shell
// role's pane), else the configured role's kind, else "".
func (m *Manager) sessionKind(s store.Session) string {
	if k := store.Deref(s.AgentKind); k != "" {
		return k
	}
	if spec, ok := m.roleSpec(Role(s.Role)); ok {
		return spec.Kind
	}
	if store.Deref(s.AgentName) != "" {
		return "" // an agent of an unknown kind
	}
	return KindShell
}

// isAgentSession reports whether a session row runs an agent (not a shell
// role's pane).
func (m *Manager) isAgentSession(s store.Session) bool {
	return m.sessionKind(s) != KindShell
}

// healthKind is the kind whose health patterns classify a session's pane:
// the agent kind, or the Tool of a shell role ("" = the defaults).
func (m *Manager) healthKind(s store.Session) string {
	if k := m.sessionKind(s); k != KindShell {
		return k
	}
	if spec, ok := m.roleSpec(Role(s.Role)); ok {
		return spec.AgentKind()
	}
	return ""
}

// resumable reports whether a session's conversation can be resumed: an
// agent session with a recorded session id whose kind reports ids
// (session_source "herdr").
func (m *Manager) resumable(s store.Session) bool {
	if !m.isAgentSession(s) || store.Deref(s.SessionID) == "" {
		return false
	}
	if k, ok := m.kindSpec(m.sessionKind(s)); ok && k.SessionSource == config.SessionNone {
		return false
	}
	return true
}

// agentName is AgentName for a stored PR.
func (m *Manager) agentName(ctx context.Context, pr store.PR, role Role) (string, error) {
	repo, err := m.d.Store.RepoByID(ctx, pr.RepoID)
	if err != nil {
		return "", fmt.Errorf("agents: repo of pr %d: %w", pr.Number, err)
	}
	return TaggedAgentName(m.d.Tag, repo.Owner+"/"+repo.Name, pr.Number, role), nil
}

// repoName is the name part of a stored PR's repository.
func (m *Manager) repoName(ctx context.Context, pr store.PR) (string, error) {
	repo, err := m.d.Store.RepoByID(ctx, pr.RepoID)
	if err != nil {
		return "", fmt.Errorf("agents: repo of pr %d: %w", pr.Number, err)
	}
	return repo.Name, nil
}

// target is how herdr addresses a session's agent: its name, else its pane.
func target(s store.Session) string {
	if n := store.Deref(s.AgentName); n != "" {
		return n
	}
	return store.Deref(s.HerdrPaneID)
}

// mapHerdr turns herdr's prompt/wait error codes into this package's sentinels,
// keeping the original error in the chain.
func mapHerdr(err error) error {
	switch {
	case err == nil:
		return nil
	case herdr.IsCode(err, herdr.CodeAgentBlocked):
		return fmt.Errorf("%w: %w", ErrBlocked, err)
	case herdr.IsCode(err, herdr.CodeAgentPromptStalled):
		return fmt.Errorf("%w: %w", ErrStalled, err)
	case herdr.IsTimeout(err):
		return fmt.Errorf("%w: %w", ErrTimeout, err)
	}
	return err
}

func isGone(err error) bool {
	return herdr.IsCode(err, herdr.CodePaneNotFound) || herdr.IsCode(err, herdr.CodeAgentNotFound) ||
		herdr.IsCode(err, herdr.CodeWorkspaceNotFound)
}

// findAgent locates a session's agent in a snapshot: by name, then by pane.
// An agent whose pane works outside the session's checkout is not the
// session's and is skipped (a name from before AgentName carried the
// repository hash may belong to another PR's agent).
func findAgent(snap herdr.Snapshot, s store.Session) (herdr.AgentInfo, bool) {
	if n := store.Deref(s.AgentName); n != "" {
		if a, ok := snap.AgentByName(n); ok && !elsewhere(snap, a, store.Deref(s.Cwd)) {
			return a, true
		}
	}
	if p := store.Deref(s.HerdrPaneID); p != "" {
		for _, a := range snap.Agents {
			if a.PaneID == p && !elsewhere(snap, a, store.Deref(s.Cwd)) {
				return a, true
			}
		}
	}
	return herdr.AgentInfo{}, false
}

// elsewhere reports whether agent a's pane is known to work outside
// checkout (both directories known and a's not inside checkout).
func elsewhere(snap herdr.Snapshot, a herdr.AgentInfo, checkout string) bool {
	if checkout == "" {
		return false
	}
	p, ok := paneIndex(snap)[a.PaneID]
	return ok && p.Cwd != "" && !sameTree(p.Cwd, checkout)
}

// sameTree reports whether dir is checkout or below it, lexically or after
// resolving symlinks (a pane reports its process's resolved cwd).
func sameTree(dir, checkout string) bool {
	if withinDir(dir, checkout) {
		return true
	}
	rd, err1 := filepath.EvalSymlinks(dir)
	rc, err2 := filepath.EvalSymlinks(checkout)
	return err1 == nil && err2 == nil && withinDir(rd, rc)
}

// withinDir reports whether path is dir or below it (lexically).
func withinDir(path, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func paneIndex(snap herdr.Snapshot) map[string]herdr.Pane {
	out := make(map[string]herdr.Pane, len(snap.Panes))
	for _, p := range snap.Panes {
		out[p.ID] = p
	}
	return out
}

func hasWorkspace(snap herdr.Snapshot, id string) bool {
	for _, w := range snap.Workspaces {
		if w.ID == id {
			return true
		}
	}
	return false
}

func isLive(s store.Session) bool {
	return s.State == store.SessionStarting || s.State == store.SessionLive
}

var liveStates = []string{store.SessionStarting, store.SessionLive}

// CountWorking counts agents of kind (codex, claude, ...) whose status is
// working, magnum's or not (daemon.max_total_working_codex gate).
func CountWorking(agents []herdr.AgentInfo, kind string) int {
	n := 0
	for _, a := range agents {
		if a.Agent == kind && a.AgentStatus == herdr.StatusWorking {
			n++
		}
	}
	return n
}
