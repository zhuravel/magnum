package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// EventClaudeProjectDeclined is recorded (subject "pr:<owner>/<name>#<N>")
// each time a Claude session of the PR starts with the user's settings
// only because the PR changes .claude/ or .mcp.json.
const EventClaudeProjectDeclined = "agents.claude_project_declined"

// projectTimeout bounds the git commands that compare a checkout's project
// config with the PR's merge base.
const projectTimeout = 30 * time.Second

// codexDir is the directory Codex loads a project's config, hooks and
// rules from: the checkout's own (Codex reads .codex/ in each directory
// from the session's working directory up to the project root, and magnum
// starts its sessions at the checkout's root).
const codexDir = ".codex"

// projectConfig is what one CLI loads from the checkout as its project
// configuration, which a PR controls, and how magnum names keeping it out
// of a session.
type projectConfig struct {
	paths []projectPath
	what  string // the paths, for the event: ".codex/"
	where string // where a changed file is: "under .codex/"
	kept  string // what a session started without them leaves out
	short string // the same for `magnum roles --kinds`
	event string
	// sentence is what the board's card says for the PR's head.
	sentence string
	// servers is the checkout's file declaring the MCP servers project_mcp
	// "off" turns off by name (a Codex config.toml); "" = none.
	servers string
	// reloads: the CLI reloads its project config from the checkout while
	// it runs (ReloadsProject).
	reloads bool
}

// projectPath is one path a CLI loads project configuration from, relative
// to the checkout's root, where magnum starts its sessions.
type projectPath struct {
	name string
	dir  bool // a directory; else a file (one of the other type is not loaded)
}

// projectKinds are the CLIs whose project configuration magnum keeps out of
// a session when the PR changes it (the kind's project_untrust), in the
// order the card names them.
var projectKinds = []string{KindCodex, KindClaude}

var projectConfigs = map[string]projectConfig{
	KindCodex: {
		paths: []projectPath{{codexDir, true}},
		what:  codexDir + "/", where: "under " + codexDir + "/",
		kept:  "the session treats the checkout as an untrusted folder (none of its .codex/ config, MCP servers, hooks or rules load)",
		short: "the checkout untrusted for the session",
		event: EventCodexProjectDeclined, sentence: CodexProjectSentence, servers: filepath.Join(codexDir, "config.toml"),
	},
	// Claude Code reads .claude/settings.json (hooks, env, plugins,
	// permissions), .claude/settings.local.json, the skills, commands,
	// agents and rules under .claude/ and .mcp.json from the directory it
	// starts in, and walks up to the repository root for skills, commands
	// and agents; magnum starts at the checkout's root. It watches the
	// settings files and skills and applies a change (hooks included) to
	// the running session, a .claude/settings.json created later too
	// (code.claude.com/docs settings "When edits take effect").
	KindClaude: {
		reloads: true,
		paths:   []projectPath{{".claude", true}, {".mcp.json", false}},
		what:    ".claude/ and .mcp.json", where: "under .claude/ or in .mcp.json",
		kept: "the session loads the user's settings only (none of the checkout's settings, hooks, MCP servers, skills, " +
			"commands, agents or CLAUDE.md)",
		short: "the session loads your user settings only",
		event: EventClaudeProjectDeclined, sentence: ClaudeProjectSentence,
	},
}

// ProjectRule describes for `magnum roles --kinds` the project config of
// kind magnum compares with the PR's merge base, e.g. ".codex/" or
// ".claude/ or .mcp.json", what a session of a PR that changes it runs
// with (the kind's project_untrust), e.g. "the checkout untrusted for the
// session", and whether project_mcp applies (that config declares MCP
// servers magnum turns off by name); paths is "" for a kind whose project
// config magnum does not compare.
func ProjectRule(kind string) (paths, effect string, servers bool) {
	pc, ok := projectConfigs[kind]
	if !ok {
		return "", "", false
	}
	var names []string
	for _, p := range pc.paths {
		if p.dir {
			names = append(names, p.name+"/")
		} else {
			names = append(names, p.name)
		}
	}
	return strings.Join(names, " or "), pc.short, pc.servers != ""
}

// projectScope is what a session keeps out of the PR's checkout
// (checkoutProject).
type projectScope struct {
	kind     string   // the role's kind (the tool of a shell role)
	checked  bool     // the kind keeps project config out and the checkout was looked at
	declined bool     // the PR changes the project config (or magnum could not tell)
	compared bool     // git compared it with the merge base
	files    int      // the files of the project config that differ from the merge base
	head     string   // the checkout's head, when declined
	paths    []string // the checkout's paths for the kind's UntrustArgs, when declined
	servers  []string // the MCP servers of the checkout's unchanged Codex config.toml (project_mcp "off")
}

// checkoutProject decides how a session of role's kind treats the project
// configuration its CLI loads from the checkout dir (projectConfigs:
// Codex's .codex/, for a trusted folder, and magnum trusts its checkouts,
// EnsureTrust: its config.toml with MCP servers Codex starts or sends
// tokens from the environment to, model and shell settings, hooks and
// rules; Claude's .claude/ and .mcp.json: settings with hooks, env and
// plugins, MCP servers, skills, commands, agents). A PR controls them, so
// when the files on disk there differ from the merge base of HEAD and base
// (gitx.WorkTreeChanges: the PR's commits, and files a round left there,
// untracked logs aside: logFile; the paths in any case, onDisk), or git
// cannot tell, the session gets the kind's project_untrust args for
// dir and its symlink-resolved form and loads none of it. Otherwise it is
// the base branch's, the team's own, and loads as before, except that
// project_mcp "off" turns its Codex MCP servers off (servers). Nothing for
// a kind magnum does not compare or without project_untrust and
// project_mcp "off", for an unknown dir and for a checkout without any of
// the kind's paths (no git runs then); head is the checkout's head when
// the caller knows it.
func (m *Manager) checkoutProject(ctx context.Context, role config.Role, dir, base, head string) projectScope {
	kind := role.AgentKind()
	k, ok := m.kindSpec(kind)
	pc, known := projectConfigs[kind]
	if !ok || !known || dir == "" {
		return projectScope{}
	}
	untrust := len(k.ProjectUntrust) > 0
	serversOff := k.ProjectMCP == config.ProjectMCPOff && len(k.MCPDisable) > 0 && pc.servers != ""
	if !untrust && !serversOff {
		return projectScope{}
	}
	s := projectScope{kind: kind, checked: true}
	names, present := pc.onDisk(dir)
	if !present {
		return s
	}
	if untrust {
		ctx, cancel := context.WithTimeout(ctx, projectTimeout)
		defer cancel()
		g := gitx.New(m.d.Runner)
		files, err := m.projectChanges(ctx, g, dir, base, pc.logFile, names)
		if err != nil || len(files) > 0 {
			if err != nil {
				m.logf("agents: %s: cannot compare %s with the merge base, so the session leaves them out: %v", role.Name, pc.what, err)
			}
			s.declined, s.compared, s.files, s.paths = true, err == nil, len(files), withRealPath(dir)
			if s.head = head; s.head == "" && m.d.Runner != nil {
				s.head, _ = g.RevParse(ctx, dir, "HEAD")
			}
			return s
		}
	}
	if serversOff {
		s.servers = m.projectServers(role, filepath.Join(dir, pc.servers))
	}
	return s
}

// onDisk lists the names at dir's root the CLI may open for pc's paths:
// each path, and every root entry other than it whose name folds to it
// (strings.EqualFold: .Claude, .mcp.jſon), which a case-insensitive
// filesystem (macOS's, which folds Unicode too) opens for it and git's
// ASCII-only icase pathspec would not always match (gitx.WorkTreeChanges
// compares each by its own name). present reports whether one of them is
// there as its path's type (a directory for .claude) or cannot be told; a
// root that cannot be read adds no entries.
func (pc projectConfig) onDisk(dir string) (names []string, present bool) {
	entries, _ := os.ReadDir(dir)
	for _, p := range pc.paths {
		found := []string{p.name}
		for _, e := range entries {
			if e.Name() != p.name && strings.EqualFold(e.Name(), p.name) {
				found = append(found, e.Name())
			}
		}
		for _, n := range found {
			if fi, err := os.Stat(filepath.Join(dir, n)); err == nil && fi.IsDir() == p.dir || err != nil && !errors.Is(err, fs.ErrNotExist) {
				present = true
			}
		}
		names = append(names, found...)
	}
	return names, present
}

// pathOf returns the project path f (relative to the checkout's root) is,
// or lies under, and the rest of f under it ("" for the path itself),
// matched in any case and under Unicode folding (strings.EqualFold): a
// case-insensitive filesystem (macOS's) opens .Claude/settings.json or
// .mcp.jſon for .claude/settings.json or .mcp.json. Nothing lies under a
// file's path.
func (pc projectConfig) pathOf(f string) (p projectPath, rest string, ok bool) {
	top, rest, nested := strings.Cut(f, "/")
	for _, p := range pc.paths {
		if strings.EqualFold(top, p.name) && (p.dir || !nested) {
			return p, rest, true
		}
	}
	return projectPath{}, "", false
}

// kvSessionProjectOut marks a session (by id) launched with its kind's
// project_untrust: the checkout's project config was kept out
// (noteSessionProject, ReloadsProject).
func kvSessionProjectOut(sessionID int64) string {
	return fmt.Sprintf("session.%d.project_out", sessionID)
}

// noteSessionProject marks session id as launched with the checkout's
// project config kept out (s.declined), or clears the mark, for a kind that
// reloads it while it runs. Best effort: a store error is logged, and a
// session without the mark counts as one that loaded it.
func (m *Manager) noteSessionProject(ctx context.Context, id int64, kind string, s projectScope) {
	if !projectConfigs[kind].reloads {
		return
	}
	ctx = context.WithoutCancel(ctx)
	key := kvSessionProjectOut(id)
	var err error
	if s.declined {
		err = m.d.Store.SetKV(ctx, key, "1")
	} else {
		err = m.d.Store.DeleteKV(ctx, key)
	}
	if err != nil {
		m.logf("agents: session %d: %s: %v", id, key, err)
	}
}

// ProjectPaths are the paths, relative to the checkout's root, that kind
// loads its project config from (claude: .claude and .mcp.json), for a
// comparison limited to them (gitx.Client.ChangedUnder); nil for a kind
// magnum does not compare.
func ProjectPaths(kind string) []string {
	var out []string
	for _, p := range projectConfigs[kind].paths {
		out = append(out, p.name)
	}
	return out
}

// ProjectTouched reports whether paths (a PR's changed files, relative to
// the repository root) name the project config kind loads from the
// checkout: a file at or under one of its paths, in any case (pathOf:
// macOS opens .Claude/settings.json for .claude/settings.json). A kind
// magnum does not compare touches nothing.
func ProjectTouched(kind string, paths []string) bool {
	pc, ok := projectConfigs[kind]
	if !ok {
		return false
	}
	for _, f := range paths {
		if _, _, ok := pc.pathOf(f); ok {
			return true
		}
	}
	return false
}

// ReloadsProject reports whether session s runs an agent CLI that reloads
// its project config from the checkout while it runs (Claude Code watches
// its settings files and skills, and loads a .claude/settings.json created
// later) and started with that config loaded: magnum launched it without
// the kind's project_untrust, or did not launch it (an adopted agent; no
// mark of noteSessionProject). Such a session would take what a commit
// checked out under it brings there, so the engine quits it (parking its
// conversation) before it moves the checkout, and its next start decides
// with the new head (checkoutProject). False for a kind without
// project_untrust: a changed config would load anyway.
func (m *Manager) ReloadsProject(ctx context.Context, s store.Session) bool {
	kind := m.sessionKind(s)
	if !projectConfigs[kind].reloads {
		return false
	}
	if k, ok := m.kindSpec(kind); !ok || len(k.ProjectUntrust) == 0 {
		return false
	}
	v, ok, err := m.d.Store.GetKV(ctx, kvSessionProjectOut(s.ID))
	return err != nil || !ok || v != "1"
}

// projectChanges lists the files at or under paths in dir that differ from
// the merge base of HEAD and base, but the untracked ones skip reports.
func (m *Manager) projectChanges(ctx context.Context, g *gitx.Client, dir, base string, skip func(string) bool, paths []string) ([]string, error) {
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
	return g.WorkTreeChanges(ctx, dir, mb, skip, paths...)
}

// logFile reports whether path (relative to the checkout's root), an
// untracked file, is a log the CLI never loads as configuration: a file
// under one of its project directories whose base name ends in ".log" or
// ".log.<digits>" (a rotated log), or that lies under the log/ or logs/
// directory right inside it (.claude/log/, .codex/logs/). A team's own
// hook, the base branch's, writes such logs (gitignored) into every
// checkout it runs in, which would keep each later session from the
// team's config. Any other untracked file, ignored or not, still counts:
// settings.local.json, a skill, command, agent or hook, a skill named
// logs (.claude/skills/logs/), anything a CLI may load now or later; a
// file of the kind's (.mcp.json) is never a log. Names match in any case,
// as the project paths do (pathOf): .CLAUDE/LOG/x.LOG is a log, while
// .Claude/Settings.Local.JSON still counts.
func (pc projectConfig) logFile(path string) bool {
	p, rest, ok := pc.pathOf(path)
	if !ok || !p.dir || rest == "" {
		return false
	}
	if top, _, nested := strings.Cut(rest, "/"); nested && (strings.EqualFold(top, "log") || strings.EqualFold(top, "logs")) {
		return true
	}
	name := strings.ToLower(rest[strings.LastIndex(rest, "/")+1:])
	if strings.HasSuffix(name, ".log") {
		return true
	}
	i := strings.LastIndex(name, ".log.")
	return i >= 0 && i+len(".log.") < len(name) && strings.Trim(name[i+len(".log."):], "0123456789") == ""
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

// ProjectNote is the PR's record of a session of one kind launched without
// the checkout's project configuration (store.KVPRProject).
type ProjectNote struct {
	Head     string    `json:"head"`     // the checkout's head at that launch ("" = unknown)
	At       time.Time `json:"at"`       // the launch
	Files    int       `json:"files"`    // the files of the project config that differ from the merge base
	Compared bool      `json:"compared"` // false: git could not compare, so the config was left out anyway
}

// CodexProjectSentence is what the board's card says for a PR whose head's
// Codex sessions ran with its checkout untrusted.
const CodexProjectSentence = "Codex ran without the PR's .codex/ changes (the checkout was untrusted in its sessions)"

// ClaudeProjectSentence is what the board's card says for a PR whose
// head's Claude sessions loaded the user's settings only.
const ClaudeProjectSentence = "Claude ran without the PR's .claude/ and .mcp.json changes (its sessions loaded your user settings only)"

// ProjectDeclined reads the PR's record of a session of kind launched
// without the checkout's project configuration; false when there is none
// (or it cannot be read).
func ProjectDeclined(ctx context.Context, st *store.Store, prID int64, kind string) (ProjectNote, bool) {
	if st == nil {
		return ProjectNote{}, false
	}
	v, ok, err := st.GetKV(ctx, store.KVPRProject(prID, kind))
	if err != nil || !ok {
		return ProjectNote{}, false
	}
	var n ProjectNote
	if json.Unmarshal([]byte(v), &n) != nil {
		return ProjectNote{}, false
	}
	return n, true
}

// declinedFor reports whether the PR's record of kind names head.
func declinedFor(ctx context.Context, st *store.Store, prID int64, kind, head string) bool {
	n, ok := ProjectDeclined(ctx, st, prID, kind)
	return ok && head != "" && n.Head == head
}

// ProjectSentences is what the board's card says of the PR's head: one
// sentence per CLI whose sessions of that head ran without the PR's
// project configuration (CodexProjectSentence, ClaudeProjectSentence), in
// projectKinds' order; "" when none did.
func ProjectSentences(ctx context.Context, st *store.Store, prID int64, head string) string {
	var out []string
	for _, kind := range projectKinds {
		if declinedFor(ctx, st, prID, kind, head) {
			out = append(out, projectConfigs[kind].sentence)
		}
	}
	return strings.Join(out, " ")
}

// NoteDeclinedProjects sets the judge's CodexProjectDeclined and
// ClaudeProjectDeclined from the PR's records of head (the round's), which
// each launch of those CLIs writes, for the kinds among the round's roles
// only (kinds: their agent kinds, the judge's included): a record names
// the kind's last launch on the head, which may be a session the round did
// not run (a reviewer paused, or left out of the round).
func NoteDeclinedProjects(ctx context.Context, st *store.Store, prID int64, head string, kinds []string, jd *JudgeData) {
	jd.CodexProjectDeclined = slices.Contains(kinds, KindCodex) && declinedFor(ctx, st, prID, KindCodex, head)
	jd.ClaudeProjectDeclined = slices.Contains(kinds, KindClaude) && declinedFor(ctx, st, prID, KindClaude, head)
}

// recordProject keeps the PR's record of how role's session treats its
// checkout's project configuration (one per kind): a declined one sets it
// and is the kind's event (counts and the head, never a path the PR named);
// one that found the config unchanged (or absent) clears it. Best effort: a
// store error is logged.
func (m *Manager) recordProject(ctx context.Context, prID int64, role config.Role, dir string, s projectScope) {
	if !s.checked {
		return
	}
	pc := projectConfigs[s.kind]
	ctx = context.WithoutCancel(ctx)
	key := store.KVPRProject(prID, s.kind)
	if !s.declined {
		if err := m.d.Store.DeleteKV(ctx, key); err != nil {
			m.logf("agents: %s: clear %s: %v", role.Name, key, err)
		}
		return
	}
	note, _ := json.Marshal(ProjectNote{Head: s.head, At: m.now().UTC(), Files: s.files, Compared: s.compared})
	if err := m.d.Store.SetKV(ctx, key, string(note)); err != nil {
		m.logf("agents: %s: record %s: %v", role.Name, key, err)
	}
	why := fmt.Sprintf("%d files %s differ from the merge base", s.files, pc.where)
	if s.files == 1 {
		why = fmt.Sprintf("1 file %s differs from the merge base", pc.where)
	}
	if !s.compared {
		why = fmt.Sprintf("magnum could not compare %s with the merge base", pc.what)
	}
	msg := fmt.Sprintf("%s runs without the PR's %s changes: %s, so %s", role.Name, pc.what, why, pc.kept)
	data, _ := json.Marshal(map[string]any{"role": role.Name, "checkout": dir, "files": s.files, "compared": s.compared, "head": s.head})
	subject := m.prSubject(ctx, prID)
	if _, err := m.d.Store.AppendEvent(ctx, store.Event{Level: "info", Subject: &subject, Kind: pc.event,
		Message: execx.Redact(msg), Data: data}); err != nil {
		m.logf("agents: event %s: %v", pc.event, err)
	}
}
