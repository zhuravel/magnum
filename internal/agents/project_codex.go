package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
)

// EventCodexProjectDeclined is recorded (subject "pr:<owner>/<name>#<N>")
// each time a Codex session of the PR starts with its checkout untrusted
// because the PR changes .codex/.
const EventCodexProjectDeclined = "agents.codex_project_declined"

// codexProjectTimeout bounds the git commands that compare a checkout's
// .codex/ with the PR's merge base.
const codexProjectTimeout = 30 * time.Second

// codexDir is the directory Codex loads a project's config, hooks and
// rules from: the checkout's own (Codex reads .codex/ in each directory
// from the session's working directory up to the project root, and magnum
// starts its sessions at the checkout's root).
const codexDir = ".codex"

// projectScope is what a Codex session keeps out of the PR's checkout
// (codexProject).
type projectScope struct {
	checked  bool     // the kind keeps project config out and the checkout was looked at
	declined bool     // the PR changes .codex/ (or magnum could not tell)
	compared bool     // git compared .codex/ with the merge base
	files    int      // the files under .codex/ that differ from the merge base
	head     string   // the checkout's head, when declined
	paths    []string // the checkout's paths for the kind's UntrustArgs, when declined
	servers  []string // the MCP servers of the checkout's unchanged .codex/config.toml (project_mcp "off")
}

// codexProject decides how a session of role's kind treats the checkout
// dir's .codex/, the project config Codex loads for a trusted folder (and
// magnum trusts its checkouts, EnsureTrust): its config.toml (MCP servers
// Codex starts or sends tokens from the environment to, model and shell
// settings), hooks and rules. A PR controls that directory, so when the
// files on disk under it differ from the merge base of HEAD and base
// (gitx.WorkTreeChanges: the PR's commits, and files a round left there),
// or git cannot tell, the session gets the kind's project_untrust args for
// dir and its symlink-resolved form and loads none of it. Otherwise it is
// the base branch's, the team's own, and loads as before, except that
// project_mcp "off" turns its MCP servers off (servers). Nothing for a kind
// without project_untrust and project_mcp "off", for an unknown dir and for
// a checkout without a .codex directory (no git runs then); head is the
// checkout's head when the caller knows it.
func (m *Manager) codexProject(ctx context.Context, role config.Role, dir, base, head string) projectScope {
	k, ok := m.kindSpec(role.AgentKind())
	if !ok || dir == "" {
		return projectScope{}
	}
	untrust := len(k.ProjectUntrust) > 0
	serversOff := k.ProjectMCP == config.ProjectMCPOff && len(k.MCPDisable) > 0
	if !untrust && !serversOff {
		return projectScope{}
	}
	s := projectScope{checked: true}
	if fi, err := os.Stat(filepath.Join(dir, codexDir)); errors.Is(err, fs.ErrNotExist) || err == nil && !fi.IsDir() {
		return s
	}
	if untrust {
		ctx, cancel := context.WithTimeout(ctx, codexProjectTimeout)
		defer cancel()
		g := gitx.New(m.d.Runner)
		files, err := m.codexChanges(ctx, g, dir, base)
		if err != nil || len(files) > 0 {
			if err != nil {
				m.logf("agents: %s: cannot compare %s with the merge base, so the session treats the checkout as untrusted: %v", role.Name, codexDir, err)
			}
			s.declined, s.compared, s.files, s.paths = true, err == nil, len(files), withRealPath(dir)
			if s.head = head; s.head == "" && m.d.Runner != nil {
				s.head, _ = g.RevParse(ctx, dir, "HEAD")
			}
			return s
		}
	}
	if serversOff {
		s.servers = m.projectServers(role, filepath.Join(dir, codexDir, "config.toml"))
	}
	return s
}

// codexChanges lists the files under dir's .codex/ that differ from the
// merge base of HEAD and base.
func (m *Manager) codexChanges(ctx context.Context, g *gitx.Client, dir, base string) ([]string, error) {
	switch {
	case m.d.Runner == nil:
		return nil, errors.New("no command runner")
	case base == "":
		return nil, errors.New("no base to compare with")
	}
	mb, err := g.MergeBase(ctx, dir, "HEAD", base)
	if err != nil {
		return nil, err
	}
	return g.WorkTreeChanges(ctx, dir, mb, codexDir)
}

// projectServers are the MCP servers the checkout's config.toml at path
// declares (declaredMCPServers); a file that cannot be read and a name that
// is no bare key are logged without the path, and leave those servers on.
func (m *Manager) projectServers(role config.Role, path string) []string {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	var names, skipped []string
	if err == nil {
		names, skipped, err = declaredMCPServers(string(data))
	}
	if err != nil {
		if pe := (*fs.PathError)(nil); errors.As(err, &pe) {
			err = pe.Err
		}
		m.logf("agents: %s: cannot read the checkout's Codex config, so its MCP servers stay on: %v", role.Name, err)
		return nil
	}
	for _, n := range skipped {
		m.logf("agents: %s: MCP server %q of the checkout's Codex config is no TOML bare key, so it stays on", role.Name, n)
	}
	return names
}

// CodexProjectNote is the PR's record of a Codex session launched with its
// checkout untrusted (store.KVPRCodexProject).
type CodexProjectNote struct {
	Head     string    `json:"head"`     // the checkout's head at that launch ("" = unknown)
	At       time.Time `json:"at"`       // the launch
	Files    int       `json:"files"`    // the files under .codex/ that differ from the merge base
	Compared bool      `json:"compared"` // false: git could not compare, so the checkout was untrusted anyway
}

// CodexProjectSentence is what the board's card says for a PR whose head's
// Codex sessions ran with its checkout untrusted.
const CodexProjectSentence = "Codex ran without the PR's .codex/ changes (the checkout was untrusted in its sessions)"

// CodexProjectDeclined reads the PR's record of a Codex session launched
// with its checkout untrusted; false when there is none (or it cannot be
// read).
func CodexProjectDeclined(ctx context.Context, st *store.Store, prID int64) (CodexProjectNote, bool) {
	if st == nil {
		return CodexProjectNote{}, false
	}
	v, ok, err := st.GetKV(ctx, store.KVPRCodexProject(prID))
	if err != nil || !ok {
		return CodexProjectNote{}, false
	}
	var n CodexProjectNote
	if json.Unmarshal([]byte(v), &n) != nil {
		return CodexProjectNote{}, false
	}
	return n, true
}

// recordProject keeps the PR's record of how role's session treats its
// checkout's .codex/: a declined one sets it and is an
// EventCodexProjectDeclined event (counts and the head, never a path the PR
// named); one that found .codex/ unchanged (or absent) clears it. Best
// effort: a store error is logged.
func (m *Manager) recordProject(ctx context.Context, prID int64, role config.Role, dir string, s projectScope) {
	if !s.checked {
		return
	}
	ctx = context.WithoutCancel(ctx)
	key := store.KVPRCodexProject(prID)
	if !s.declined {
		if err := m.d.Store.DeleteKV(ctx, key); err != nil {
			m.logf("agents: %s: clear %s: %v", role.Name, key, err)
		}
		return
	}
	note, _ := json.Marshal(CodexProjectNote{Head: s.head, At: m.now().UTC(), Files: s.files, Compared: s.compared})
	if err := m.d.Store.SetKV(ctx, key, string(note)); err != nil {
		m.logf("agents: %s: record %s: %v", role.Name, key, err)
	}
	why := fmt.Sprintf("%d files under %s/ differ from the merge base", s.files, codexDir)
	if s.files == 1 {
		why = fmt.Sprintf("1 file under %s/ differs from the merge base", codexDir)
	}
	if !s.compared {
		why = fmt.Sprintf("magnum could not compare %s/ with the merge base", codexDir)
	}
	msg := fmt.Sprintf("%s runs without the PR's %s/ changes: %s, so the session treats the checkout as an untrusted folder "+
		"(none of its %s/ config, MCP servers, hooks or rules load)", role.Name, codexDir, why, codexDir)
	data, _ := json.Marshal(map[string]any{"role": role.Name, "checkout": dir, "files": s.files, "compared": s.compared, "head": s.head})
	subject := m.prSubject(ctx, prID)
	if _, err := m.d.Store.AppendEvent(ctx, store.Event{Level: "info", Subject: &subject, Kind: EventCodexProjectDeclined,
		Message: execx.Redact(msg), Data: data}); err != nil {
		m.logf("agents: event %s: %v", EventCodexProjectDeclined, err)
	}
}
