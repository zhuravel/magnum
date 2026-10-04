package herdr

// Status is an agent status as reported by herdr.
type Status string

// Agent statuses.
const (
	StatusIdle    Status = "idle"
	StatusWorking Status = "working"
	StatusBlocked Status = "blocked"
	StatusDone    Status = "done"
	StatusUnknown Status = "unknown"
)

// ServerInfo is the ping reply.
type ServerInfo struct {
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
}

// Workspace is a herdr workspace (WorkspaceInfo).
type Workspace struct {
	ID          string             `json:"workspace_id"`
	Number      int                `json:"number"`
	Label       string             `json:"label"`
	Focused     bool               `json:"focused"`
	PaneCount   int                `json:"pane_count"`
	TabCount    int                `json:"tab_count"`
	ActiveTabID string             `json:"active_tab_id"`
	AgentStatus Status             `json:"agent_status"`
	Tokens      map[string]string  `json:"tokens,omitempty"`
	Worktree    *WorkspaceWorktree `json:"worktree,omitempty"`
}

// WorkspaceWorktree describes the git checkout a worktree workspace shows.
type WorkspaceWorktree struct {
	RepoKey          string `json:"repo_key"`
	RepoName         string `json:"repo_name"`
	RepoRoot         string `json:"repo_root"`
	CheckoutPath     string `json:"checkout_path"`
	IsLinkedWorktree bool   `json:"is_linked_worktree"`
}

// Tab is a herdr tab (TabInfo).
type Tab struct {
	ID          string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Number      int    `json:"number"`
	Label       string `json:"label"`
	Focused     bool   `json:"focused"`
	PaneCount   int    `json:"pane_count"`
	AgentStatus Status `json:"agent_status"`
}

// AgentSession identifies the agent conversation running in a pane:
// Kind "id" with a Codex/Claude session uuid in Value, or "path".
type AgentSession struct {
	Agent  string `json:"agent"`
	Kind   string `json:"kind"`
	Source string `json:"source"` // e.g. "herdr:codex"
	Value  string `json:"value"`
}

// Pane is a herdr pane (PaneInfo). Nullable strings decode to "".
type Pane struct {
	ID                    string            `json:"pane_id"`
	TerminalID            string            `json:"terminal_id"`
	WorkspaceID           string            `json:"workspace_id"`
	TabID                 string            `json:"tab_id"`
	Focused               bool              `json:"focused"`
	Label                 string            `json:"label"`
	Agent                 string            `json:"agent"` // "" when no agent detected
	DisplayAgent          string            `json:"display_agent"`
	AgentStatus           Status            `json:"agent_status"`
	AgentSession          *AgentSession     `json:"agent_session"`
	Cwd                   string            `json:"cwd"`
	ForegroundCwd         string            `json:"foreground_cwd"`
	Title                 string            `json:"title"`
	TerminalTitle         string            `json:"terminal_title"`
	TerminalTitleStripped string            `json:"terminal_title_stripped"`
	RestoreError          string            `json:"restore_error"`
	Revision              uint64            `json:"revision"`
	Tokens                map[string]string `json:"tokens,omitempty"`
}

// AgentInfo is a pane that carries an agent (agent.get/list, snapshot agents).
type AgentInfo struct {
	PaneID                 string            `json:"pane_id"`
	TerminalID             string            `json:"terminal_id"`
	WorkspaceID            string            `json:"workspace_id"`
	TabID                  string            `json:"tab_id"`
	Name                   string            `json:"name"` // set by agent.start / agent.rename
	Agent                  string            `json:"agent"`
	DisplayAgent           string            `json:"display_agent"`
	AgentStatus            Status            `json:"agent_status"`
	AgentSession           *AgentSession     `json:"agent_session"`
	Cwd                    string            `json:"cwd"`
	ForegroundCwd          string            `json:"foreground_cwd"`
	Focused                bool              `json:"focused"`
	InteractiveReady       bool              `json:"interactive_ready"`
	LaunchPending          bool              `json:"launch_pending"`
	ScreenDetectionSkipped bool              `json:"screen_detection_skipped"`
	CompletionSeq          *uint64           `json:"completion_seq"` // null on live agents in 0.9.x
	StateChangeSeq         uint64            `json:"state_change_seq"`
	Revision               uint64            `json:"revision"`
	Title                  string            `json:"title"`
	TerminalTitle          string            `json:"terminal_title"`
	TerminalTitleStripped  string            `json:"terminal_title_stripped"`
	Tokens                 map[string]string `json:"tokens,omitempty"`
}

// Snapshot is the whole live session (session.snapshot). Layouts are omitted.
type Snapshot struct {
	Version            string      `json:"version"`
	Protocol           int         `json:"protocol"`
	Workspaces         []Workspace `json:"workspaces"`
	Tabs               []Tab       `json:"tabs"`
	Panes              []Pane      `json:"panes"`
	Agents             []AgentInfo `json:"agents"`
	FocusedWorkspaceID string      `json:"focused_workspace_id"`
	FocusedTabID       string      `json:"focused_tab_id"`
	FocusedPaneID      string      `json:"focused_pane_id"`
}

// PaneBySession returns the pane whose agent_session.value equals value
// (used to rebind sessions herdr restored on its own).
func (s Snapshot) PaneBySession(value string) (Pane, bool) {
	for _, p := range s.Panes {
		if p.AgentSession != nil && p.AgentSession.Value == value {
			return p, true
		}
	}
	return Pane{}, false
}

// AgentByName returns the agent started under name.
func (s Snapshot) AgentByName(name string) (AgentInfo, bool) {
	for _, a := range s.Agents {
		if a.Name == name {
			return a, true
		}
	}
	return AgentInfo{}, false
}

// ProcessInfo describes a pane's shell and foreground process group.
type ProcessInfo struct {
	PaneID         string    `json:"pane_id"`
	ShellPID       int       `json:"shell_pid"`
	ForegroundPGID int       `json:"foreground_process_group_id"`
	TTY            string    `json:"tty"`
	Foreground     []Process `json:"foreground_processes"`
}

// Process is one foreground process of a pane.
type Process struct {
	PID     int      `json:"pid"`
	Name    string   `json:"name"`
	Argv    []string `json:"argv"`
	Argv0   string   `json:"argv0"`
	Cmdline string   `json:"cmdline"`
	Cwd     string   `json:"cwd"`
}

// ReadResult is pane/agent output (pane.read, agent.read).
type ReadResult struct {
	PaneID      string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
	TabID       string `json:"tab_id"`
	Source      string `json:"source"`
	Format      string `json:"format"`
	Text        string `json:"text"`
	Revision    uint64 `json:"revision"`
	Truncated   bool   `json:"truncated"`
}

// OutputMatch is the pane.wait_for_output reply.
type OutputMatch struct {
	PaneID      string     `json:"pane_id"`
	Revision    uint64     `json:"revision"`
	MatchedLine string     `json:"matched_line"`
	Read        ReadResult `json:"read"`
}

// WorkspaceCreated is the workspace.create reply.
type WorkspaceCreated struct {
	Workspace Workspace `json:"workspace"`
	Tab       Tab       `json:"tab"`
	RootPane  Pane      `json:"root_pane"`
}

// TabCreated is the tab.create reply.
type TabCreated struct {
	Tab      Tab  `json:"tab"`
	RootPane Pane `json:"root_pane"`
}

// AgentStarted is the agent.start reply; Argv is what herdr typed/launched.
type AgentStarted struct {
	Agent AgentInfo `json:"agent"`
	Argv  []string  `json:"argv"`
}

// NotificationResult is the notification.show reply. Reason is one of
// shown, disabled, rate_limited, no_foreground_client, busy.
type NotificationResult struct {
	Shown  bool   `json:"shown"`
	Reason string `json:"reason"`
}

// WindowTitleResult is the client.window_title.* reply. Reason is one of
// set, cleared, no_foreground_client.
type WindowTitleResult struct {
	Changed bool   `json:"changed"`
	Reason  string `json:"reason"`
}

// PluginPane is the plugin.pane.open reply.
type PluginPane struct {
	PluginID   string `json:"plugin_id"`
	Entrypoint string `json:"entrypoint"`
	Pane       Pane   `json:"pane"`
}

// Worktree is one git worktree known to herdr.
type Worktree struct {
	Path             string `json:"path"`
	Label            string `json:"label"`
	Branch           string `json:"branch"` // "" when detached
	IsBare           bool   `json:"is_bare"`
	IsDetached       bool   `json:"is_detached"`
	IsLinkedWorktree bool   `json:"is_linked_worktree"`
	IsPrunable       bool   `json:"is_prunable"`
	OpenWorkspaceID  string `json:"open_workspace_id"`
}

// WorktreeSource is the repository a worktree.list call resolved.
type WorktreeSource struct {
	RepoKey            string `json:"repo_key"`
	RepoName           string `json:"repo_name"`
	RepoRoot           string `json:"repo_root"`
	SourceCheckoutPath string `json:"source_checkout_path"`
	SourceWorkspaceID  string `json:"source_workspace_id"`
}

// WorktreeListing is the worktree.list reply.
type WorktreeListing struct {
	Source    WorktreeSource `json:"source"`
	Worktrees []Worktree     `json:"worktrees"`
}
