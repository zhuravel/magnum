package herdr

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// SavedFile is the file beside a session's socket in which herdr keeps the
// session's workspaces, tabs and panes, each pane's directory and its agent's
// conversation: a server that starts restores them, and resumes the agents
// in their conversations.
const SavedFile = "session.json"

// savedVersion is the version of SavedFile that Saved reads.
const savedVersion = 3

// SavedPrefix starts the ids Saved gives the workspaces and panes herdr
// saved: they are no server's, and a restore numbers them anew.
const SavedPrefix = "saved:"

// Saver is a herdr session whose saved state a guard reads while no server
// runs in it (*Client).
type Saver interface {
	Saved() (Snapshot, error)
}

// savedState is the part of SavedFile Saved reads.
type savedState struct {
	Version    int `json:"version"`
	Workspaces *[]struct {
		ID   string `json:"id"`
		Tabs []struct {
			Panes map[string]struct {
				Cwd          string        `json:"cwd"`
				AgentSession *AgentSession `json:"agent_session"`
			} `json:"panes"`
		} `json:"tabs"`
	} `json:"workspaces"`
}

// Saved is what the session's server restores when it starts, as a guard
// sees it (SavedFile beside the socket): each pane in which an agent's
// conversation runs (agent_session) is an idle agent in the pane's
// directory, in workspace "saved:<id>" as pane "saved:<id>:p<n>". A shell
// comes back an idle shell, which holds nothing, so it is left out. A
// session that saved nothing (no file) has no panes; a file that cannot be
// read, or of a version this build does not know, is an error: it may hold
// an agent in any checkout.
func (c *Client) Saved() (Snapshot, error) {
	path := filepath.Join(filepath.Dir(c.socket()), SavedFile)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Snapshot{}, nil
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("herdr: saved session: %w", err)
	}
	var st savedState
	if err := json.Unmarshal(raw, &st); err != nil {
		return Snapshot{}, fmt.Errorf("herdr: saved session %s: %w", path, err)
	}
	if st.Version != savedVersion || st.Workspaces == nil {
		return Snapshot{}, fmt.Errorf("herdr: saved session %s is version %d with workspaces %v, not version %d, which magnum reads",
			path, st.Version, st.Workspaces != nil, savedVersion)
	}
	var out Snapshot
	for _, w := range *st.Workspaces {
		ws := SavedPrefix + w.ID
		var panes []Pane
		for _, t := range w.Tabs {
			for n, p := range t.Panes {
				if p.AgentSession == nil || p.AgentSession.Agent == "" {
					continue
				}
				panes = append(panes, Pane{ID: ws + ":p" + n, WorkspaceID: ws, Agent: p.AgentSession.Agent, AgentStatus: StatusIdle,
					AgentSession: p.AgentSession, Cwd: p.Cwd})
			}
		}
		if len(panes) == 0 {
			continue
		}
		slices.SortFunc(panes, func(a, b Pane) int { return strings.Compare(a.ID, b.ID) })
		out.Workspaces = append(out.Workspaces, Workspace{ID: ws})
		for _, p := range panes {
			out.Panes = append(out.Panes, p)
			out.Agents = append(out.Agents, AgentInfo{PaneID: p.ID, WorkspaceID: ws, Agent: p.Agent, AgentStatus: StatusIdle,
				AgentSession: p.AgentSession, Cwd: p.Cwd})
		}
	}
	return out, nil
}
