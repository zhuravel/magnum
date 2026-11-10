package agents

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
)

// EventCodexProjectDeclined is recorded (subject "pr:<owner>/<name>#<N>")
// each time a Codex session of the PR starts without the PR's changes to
// the project config Codex loads from the checkout: with the checkout
// untrusted because the PR changes .codex/, or with no AGENTS.md when it
// changes only those; for its data's residue, the checkout's files there
// differ though the PR's commits do not change them (ProjectNote.Residue).
const EventCodexProjectDeclined = "agents.codex_project_declined"

// EventClaudeProjectDeclined is recorded (subject "pr:<owner>/<name>#<N>")
// each time a Claude session of the PR starts with the user's settings
// only because the PR changes .claude/, .mcp.json, a CLAUDE.md,
// CLAUDE.local.md or AGENTS.md; for its data's residue, the checkout's
// files there differ though the PR's commits do not change them
// (ProjectNote.Residue).
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
	cli   string // the CLI's name in a sentence: "Codex"
	paths []projectPath
	kept  string // what a session started without them leaves out
	short string // the same for `magnum roles --kinds`
	// docsKept, docsShort: the same for a session started with the kind's
	// project_docs_off (the PR changes only instruction files).
	docsKept, docsShort string
	// how, docsHow: how the card says the sessions ran without them.
	how, docsHow string
	event        string
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
	// anyDir: a file of that name in any directory below the root too
	// (Claude Code loads the CLAUDE.md of each directory it reads files in),
	// or, for a directory, one of that name in any directory and what lies
	// under it (Claude Code loads the skills of a .claude/skills/ below the
	// root the first time it reads a file there).
	anyDir bool
	// docs: an instruction file (AGENTS.md, CLAUDE.md), which the kind's
	// project_docs_off keeps out on its own.
	docs bool
}

// label names p in a sentence: ".codex/", "AGENTS.md" (never a path a PR
// named: an AGENTS.md in a subdirectory is "AGENTS.md" too).
func (p projectPath) label() string {
	if p.dir {
		return p.name + "/"
	}
	return p.name
}

// projectKinds are the CLIs whose project configuration magnum keeps out of
// a session when the PR changes it (the kind's project_untrust), in the
// order the card names them.
var projectKinds = []string{KindCodex, KindClaude}

var projectConfigs = map[string]projectConfig{
	// Codex 0.160 loads a trusted folder's .codex/, and the AGENTS.md (or
	// AGENTS.override.md) of each directory from the project root down to
	// the session's working directory (core/src/agents_md.rs), which is the
	// checkout's root: an AGENTS.md below it is not loaded (the model may
	// still read one as a file). An untrusted folder loads neither, and
	// project_doc_max_bytes=0 no AGENTS.md alone.
	KindCodex: {
		cli:   "Codex",
		paths: []projectPath{{name: codexDir, dir: true}, {name: "AGENTS.md", docs: true}, {name: "AGENTS.override.md", docs: true}},
		kept: "the session treats the checkout as an untrusted folder (none of its .codex/ config, MCP servers, hooks or rules, " +
			"nor its AGENTS.md, load)",
		short:     "the checkout untrusted for the session",
		docsKept:  "the session loads none of the checkout's AGENTS.md, and the rest of its project config as before",
		docsShort: "no AGENTS.md of the checkout",
		how:       "the checkout was untrusted in its sessions", docsHow: "its sessions loaded no AGENTS.md",
		event: EventCodexProjectDeclined, servers: filepath.Join(codexDir, "config.toml"),
	},
	// Claude Code reads .claude/settings.json (hooks, env, plugins,
	// permissions), .claude/settings.local.json, the skills, commands,
	// agents and rules under .claude/ and .mcp.json from the directory it
	// starts in, and walks up to the repository root for skills, commands
	// and agents; magnum starts at the checkout's root. A .claude/skills/
	// below it loads the first time the session reads or edits a file in
	// that directory (code.claude.com/docs skills "Load skills in monorepos
	// and subdirectories"), and a skill's frontmatter can declare hooks, so
	// a .claude/ in any directory is its config (anyDir); .mcp.json and the
	// settings load from the start directory only. It watches the
	// settings files and skills and applies a change (hooks included) to
	// the running session, a .claude/settings.json created later too
	// (code.claude.com/docs settings "When edits take effect"). 2.1.292
	// loads the CLAUDE.md and CLAUDE.local.md of the root at start and of a
	// subdirectory when it reads files there, and, where the project has no
	// CLAUDE.md, its AGENTS.md files instead (the built-in agents-md
	// plugin; another instructionFiles mode loads them beside CLAUDE.md): a
	// CLAUDE.md that links to AGENTS.md loads the PR's AGENTS.md either way.
	KindClaude: {
		cli:     "Claude",
		reloads: true,
		paths: []projectPath{{name: ".claude", dir: true, anyDir: true}, {name: ".mcp.json"},
			{name: "CLAUDE.md", anyDir: true, docs: true}, {name: "CLAUDE.local.md", anyDir: true, docs: true},
			{name: "AGENTS.md", anyDir: true, docs: true}},
		kept: "the session loads the user's settings only (none of the checkout's settings, hooks, MCP servers, skills, " +
			"commands, agents, CLAUDE.md or AGENTS.md)",
		short: "the session loads your user settings only",
		how:   "its sessions loaded your user settings only",
		event: EventClaudeProjectDeclined,
	},
}

// labels names paths in a sentence, each once, in the kind's order:
// ".codex/ and AGENTS.md"; all of the kind's paths when there are none (a
// session that could not compare them, or a record written before the
// record named them).
func (pc projectConfig) labels(names []string) string {
	var out []string
	for _, p := range pc.paths {
		if l := p.label(); (len(names) == 0 || slices.Contains(names, l)) && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return joinList(out, "and")
}

// joinList joins words as a sentence lists them: "a", "a and b", "a, b and
// c" (conj "and").
func joinList(words []string, conj string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " " + conj + " " + words[len(words)-1]
}

// ProjectRule describes for `magnum roles --kinds` the project config of
// kind magnum compares with the PR's merge base, e.g. ".codex/, AGENTS.md
// or AGENTS.override.md", what a session of a PR that changes it runs
// with (the kind's project_untrust), e.g. "the checkout untrusted for the
// session", the instruction files among them and what a session of a PR
// that changes only those runs with (the kind's project_docs_off; "" for a
// kind without such args), and whether project_mcp applies (that config
// declares MCP servers magnum turns off by name); paths is "" for a kind
// whose project config magnum does not compare.
func ProjectRule(kind string) (paths, effect, docs, docsEffect string, servers bool) {
	pc, ok := projectConfigs[kind]
	if !ok {
		return "", "", "", "", false
	}
	var root, anyDir, instructions []string
	for _, p := range pc.paths {
		if p.anyDir {
			anyDir = append(anyDir, p.label())
		} else {
			root = append(root, p.label())
		}
		if p.docs {
			instructions = append(instructions, p.label())
		}
	}
	paths = joinList(root, "or")
	if len(anyDir) > 0 {
		paths += ", or a " + joinList(anyDir, "or") + " in any directory"
	}
	if pc.docsShort != "" {
		docs, docsEffect = joinList(instructions, "or"), pc.docsShort
	}
	return paths, pc.short, docs, docsEffect, pc.servers != ""
}

// projectScope is what a session keeps out of the PR's checkout
// (checkoutProject).
type projectScope struct {
	kind     string   // the role's kind (the tool of a shell role)
	checked  bool     // the kind keeps project config out and the checkout was looked at
	declined bool     // the PR changes the project config (or magnum could not tell)
	compared bool     // git compared it with the merge base
	files    int      // the files of the project config that differ from the merge base
	names    []string // the labels of the paths those files fall in (projectPath.label)
	head     string   // the checkout's head, when declined
	paths    []string // the checkout's paths for the kind's UntrustArgs, when declined
	// docsOff: declined, but the files are all instruction files and the
	// kind has project_docs_off, which the session gets instead of
	// project_untrust (paths stays empty).
	docsOff bool
	// residue: compared, and none of those files is one the commits since
	// the merge base change (committedChanges): they are untracked or
	// ignored files a round left in the slot, or uncommitted edits.
	residue bool
	servers []config.ProjectServer // the MCP servers of the checkout's unchanged Codex config.toml (project_mcp "off")
}

// checkoutProject decides how a session of role's kind treats the project
// configuration its CLI loads from the checkout dir (projectConfigs:
// Codex's .codex/, for a trusted folder, and magnum trusts its checkouts,
// EnsureTrust: its config.toml with MCP servers Codex starts or sends
// tokens from the environment to, model and shell settings, hooks and
// rules, and the AGENTS.md at its root; Claude's .claude/ and .mcp.json:
// settings with hooks, env and plugins, MCP servers, skills, commands,
// agents, and the CLAUDE.md, CLAUDE.local.md and AGENTS.md of any
// directory). A PR controls them, so when the files on disk there differ
// from the merge base of HEAD and base (gitx.WorkTreeChanges: the PR's
// commits, and files a round left there, untracked logs aside: logFile;
// the paths in any case, onDisk, and what a link at the root, under a
// project directory or of the kind's paths below the root leads to,
// trackedLinks; base is the default branch's for a stacked PR,
// projectBase), or git cannot tell (a git that fails is a warning; a launch
// without a base and a dir that is no git checkout, as the retro's and the
// curation's are, never reach git and log at debug: errNoBase,
// errNoCheckout), the session gets the kind's
// project_untrust args for dir and its symlink-resolved form and loads none
// of it, or, when the files are all instruction files and the kind has
// project_docs_off, those args, and loads the rest. Otherwise it is the
// base branch's, the team's own, and loads as before, except that
// project_mcp "off" turns its Codex MCP servers off (servers). Nothing for
// a kind magnum does not compare or without project_untrust and
// project_mcp "off", for an unknown dir and for a checkout without any of
// the kind's paths (no git runs then; a kind with a path in any directory
// compares every checkout that is there); head is the checkout's head when
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
	disk, present := pc.onDisk(dir)
	if !present {
		return s
	}
	if untrust {
		ctx, cancel := context.WithTimeout(ctx, projectTimeout)
		defer cancel()
		g := gitx.New(m.d.Runner)
		var files []string
		var mb string
		err := m.trackedLinks(ctx, g, dir, base, pc, &disk)
		if err == nil {
			files, mb, err = m.projectChanges(ctx, g, dir, base, pc.logFile, disk.git)
		}
		if err == nil && len(disk.links) > 0 {
			var all []string
			if all, err = g.WorkTreeNames(ctx, dir, mb); err == nil {
				files = append(files, linkedChanges(all, disk.links)...)
				slices.Sort(files)
				files = slices.Compact(files)
			}
		}
		changed := pc.changed(files, disk.links)
		if disk.partial {
			m.logAt(slog.LevelWarn, "agents: %s: more than %d entries to look through for links under %s, so the session leaves them out",
				role.Name, maxLinkWalk, pc.labels(nil))
		}
		if err != nil || len(changed) > 0 || disk.partial {
			switch {
			case errors.Is(err, errNoBase), errors.Is(err, errNoCheckout):
				// Decided without asking git: the retro and the notes
				// curation run in magnum's own directories, and nothing
				// is wrong. The config stays out all the same.
				m.logAt(slog.LevelDebug, "agents: %s: %v: the project config (%s) is left out", role.Name, err, pc.labels(nil))
			case err != nil:
				m.logErr(ctx, err, "agents: %s: cannot compare %s with the merge base, so the session leaves them out: %v", role.Name, pc.labels(nil), err)
			}
			s.declined, s.compared, s.files = true, err == nil, len(changed)
			if err == nil {
				for _, p := range changed {
					if l := p.label(); !slices.Contains(s.names, l) {
						s.names = append(s.names, l)
					}
				}
				s.docsOff = len(k.ProjectDocsOff) > 0 && !slices.ContainsFunc(changed, func(p projectPath) bool { return !p.docs })
				s.residue = !m.committedChanges(ctx, g, role, dir, mb, pc, disk)
			}
			if s.head = head; s.head == "" && m.d.Runner != nil && !errors.Is(err, errNoCheckout) {
				s.head, _ = g.RevParse(ctx, dir, "HEAD")
			}
			if !s.docsOff {
				s.paths = withRealPath(dir)
				return s
			}
		}
	}
	if serversOff {
		s.servers = m.projectServers(role, filepath.Join(dir, pc.servers))
	}
	return s
}

// projectBase is what a session of pr compares the checkout's project
// config with (checkoutProject): base, the PR's base ref or its merge base
// with the head, when the PR is based on the repository's default branch
// (or names no base), else origin/<default branch>. The team's config is
// the default branch's: a stacked PR's base branch is another PR's head,
// whose own .claude/ or .codex/ changes nobody reviewed, and compared with
// origin/<that branch> once it is fresh (the operator's fetch, a fix's: a
// pool slot fetches only the pool's base) they would load as the team's,
// hooks and all, outside the sandbox. Against the default branch they
// count as the PR's, and the session leaves them out.
func projectBase(pr store.PR, repo store.Repo, base string) string {
	def := cmp.Or(strings.TrimPrefix(repo.DefaultBranch, "origin/"), "master")
	if b := strings.TrimPrefix(store.Deref(pr.BaseRef), "origin/"); b == "" || b == def {
		return base
	}
	return "origin/" + def
}

// diskPaths are what git compares for one checkout (projectConfig.onDisk).
type diskPaths struct {
	// git: each path (a path in any directory as gitx.AnyDir and its
	// globName, a directory's with gitx.AnyDirTree too), each root entry
	// folding to one, and each link target.
	git []string
	// links: the target, relative to the root, of each symbolic link into
	// the checkout that a path at the root is, or that lies under a
	// directory path (linkWalk), and that path.
	links map[string]projectPath
	// partial: the walk for links under a directory path stopped at
	// maxLinkWalk entries, so one may lead where git compares nothing.
	partial bool
}

// maxLinkWalk caps the entries onDisk reads under a checkout's project
// directories, and the directories inside it their links lead to, for
// symbolic links (linkWalk); past it, the session leaves the config out
// (diskPaths.partial).
const maxLinkWalk = 10000

// onDisk lists what git compares for pc's paths in the checkout dir: each
// path; every root entry other than it whose name folds to it
// (strings.EqualFold: .Claude, .mcp.jſon), which a case-insensitive
// filesystem (macOS's, which folds Unicode too) opens for it and git's
// ASCII-only icase pathspec would not always match (gitx.WorkTreeChanges
// compares each by its own name); a path in any directory as a glob
// (gitx.AnyDir, globName; a directory's with what lies under it); the
// target of a root entry that is a symbolic link into the checkout (a
// CLAUDE.md linking to AGENTS.md loads the PR's AGENTS.md: the CLIs follow
// links); and the target of each link into the
// checkout under a directory path (.claude/skills -> ../.agents/skills,
// linkWalk). present reports whether one of them is there as its path's
// type (a directory for .claude) or cannot be told, and, for a path in any
// directory, whether the root is there; a root that cannot be read adds no
// entries.
func (pc projectConfig) onDisk(dir string) (d diskPaths, present bool) {
	entries, rerr := os.ReadDir(dir)
	root := ""
	realRoot := func() string {
		if root == "" {
			if root, _ = filepath.EvalSymlinks(dir); root == "" {
				root = dir
			}
		}
		return root
	}
	w := linkWalk{pc: pc, d: &d, seen: map[string]bool{}, budget: maxLinkWalk}
	for _, p := range pc.paths {
		found := []string{p.name}
		for _, e := range entries {
			if e.Name() != p.name && strings.EqualFold(e.Name(), p.name) {
				found = append(found, e.Name())
			}
		}
		var targets, walk []string
		for _, n := range found {
			full := filepath.Join(dir, n)
			if fi, err := os.Stat(full); err == nil && fi.IsDir() == p.dir || err != nil && !errors.Is(err, fs.ErrNotExist) {
				present = true
			}
			fi, err := os.Lstat(full)
			switch {
			case err != nil:
				continue
			case fi.Mode()&fs.ModeSymlink == 0:
				if p.dir && fi.IsDir() {
					walk = append(walk, n)
				}
				continue
			}
			// A target that is one of pc's paths is compared, and named, as itself.
			if t, ok := linkTarget(realRoot(), full); ok {
				if w.add(t, p) {
					targets = append(targets, t)
				}
				if p.dir {
					walk = append(walk, t)
				}
			}
		}
		if len(walk) > 0 {
			w.root = realRoot()
			targets = append(targets, w.walk(walk, p)...)
		}
		if p.anyDir {
			present = present || !errors.Is(rerr, fs.ErrNotExist)
			found = append(found, p.anyDirSpecs()...)
		}
		d.git = append(append(d.git, found...), targets...)
	}
	return d, present
}

// trackedLinks adds to disk the links of pc's paths in any directory that
// the checkout's index tracks (gitx.TrackedSymlinks, one git ls-files of
// the kind's paths in any directory: an app/CLAUDE.md -> ../docs/app.md,
// a packages/web/.claude/skills -> ../../../shared/skills), as onDisk adds
// the root's (addLinks): Claude loads a CLAUDE.md or a .claude/skills/
// below the root once it reads a file there, through the link. Nothing for
// a kind without such paths, and no git where projectChanges asks none (no
// runner, no base, no checkout: it answers for the launch). An untracked
// link there is an untracked file the compare counts anyway.
func (m *Manager) trackedLinks(ctx context.Context, g *gitx.Client, dir, base string, pc projectConfig, disk *diskPaths) error {
	var specs []string
	for _, p := range pc.paths {
		specs = append(specs, p.anyDirSpecs()...)
	}
	if len(specs) == 0 || m.d.Runner == nil || base == "" || !inGitCheckout(dir) {
		return nil
	}
	links, err := g.TrackedSymlinks(ctx, dir, specs...)
	if err != nil {
		return err
	}
	pc.addLinks(dir, disk, links)
	return nil
}

// addLinks adds to d the target of each of links (paths relative to the
// checkout dir's root) that is one of pc's paths and a symbolic link into
// the checkout, labelled by that path, and the links under the directory
// it leads to (linkWalk), as onDisk does for the root's entries.
func (pc projectConfig) addLinks(dir string, d *diskPaths, links []string) {
	if len(links) == 0 {
		return
	}
	root, _ := filepath.EvalSymlinks(dir)
	if root == "" {
		root = dir
	}
	w := linkWalk{pc: pc, d: d, root: root, seen: map[string]bool{}, budget: maxLinkWalk}
	for _, l := range links {
		p, _, ok := pc.pathOf(l)
		if !ok {
			continue
		}
		full := filepath.Join(root, filepath.FromSlash(l))
		t, ok := linkTarget(root, full)
		if !ok {
			continue
		}
		if w.add(t, p) {
			d.git = append(d.git, t)
		}
		if fi, err := os.Stat(full); err == nil && fi.IsDir() {
			d.git = append(d.git, w.walk([]string{t}, p)...)
		}
	}
}

// linkWalk finds the symbolic links under a checkout's project directories
// (onDisk): Claude and Codex follow them (.claude/skills ->
// ../.agents/skills loads the skills there), while git compares the link
// alone, so what the CLI loads through one is compared, and recorded, as
// its target.
type linkWalk struct {
	pc     projectConfig
	root   string     // the checkout's real path
	d      *diskPaths // links and partial are written here
	seen   map[string]bool
	budget int // entries left to read (maxLinkWalk)
}

// add records t, the target of a link of path p, unless it is one of pc's
// paths (compared, and named, as itself) or recorded already; it reports
// whether it recorded it.
func (w *linkWalk) add(t string, p projectPath) bool {
	if _, _, own := w.pc.pathOf(t); own {
		return false
	}
	if _, dup := w.d.links[t]; dup {
		return false
	}
	if w.d.links == nil {
		w.d.links = map[string]projectPath{}
	}
	w.d.links[t] = p
	return true
}

// walk reads the directories start (relative to the root, with forward
// slashes) of the directory path p, and each directory inside the checkout
// a link under them leads to, each once, and returns the targets it
// recorded (add) for the links it found there, labelled p: a link out of
// the checkout or to nothing is left out (linkTarget), a directory with a
// .git entry (a nested checkout: Claude Code's .claude/worktrees/<name>) is
// not entered, and a directory that cannot be read is skipped. Past the
// budget it stops and marks the paths partial.
func (w *linkWalk) walk(start []string, p projectPath) []string {
	var targets []string
	queue := slices.Clone(start)
	for len(queue) > 0 {
		rel := queue[0]
		queue = queue[1:]
		if w.seen[rel] {
			continue
		}
		w.seen[rel] = true
		entries, err := os.ReadDir(filepath.Join(w.root, filepath.FromSlash(rel)))
		if err != nil || slices.ContainsFunc(entries, func(e fs.DirEntry) bool { return e.Name() == ".git" }) {
			continue
		}
		for _, e := range entries {
			if w.budget--; w.budget < 0 {
				w.d.partial = true
				return targets
			}
			child := rel + "/" + e.Name()
			switch {
			case e.Type()&fs.ModeSymlink != 0:
				full := filepath.Join(w.root, filepath.FromSlash(child))
				t, ok := linkTarget(w.root, full)
				if !ok {
					continue
				}
				if w.add(t, p) {
					targets = append(targets, t)
				}
				if fi, err := os.Stat(full); err == nil && fi.IsDir() {
					queue = append(queue, t)
				}
			case e.IsDir():
				queue = append(queue, child)
			}
		}
	}
	return targets
}

// linkTarget is where the symbolic link path leads, relative to root (the
// checkout's real path), with forward slashes; false when it leads
// nowhere or outside the checkout, where no commit of the PR reaches.
func linkTarget(root, path string) (string, bool) {
	t, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(root, t)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// globName is name as a glob git matches in any (ASCII) case for every
// name that folds to it, as macOS opens it: each letter with a case
// variant outside ASCII (s: ſ, k: the Kelvin sign) becomes *, since git's
// wildcards count bytes, so AGENTS.md is AGENT*.md. What the glob matches
// beyond the name, pathOf leaves out.
func globName(name string) string {
	var b strings.Builder
	for _, r := range name {
		wide := false
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			wide = wide || f > unicode.MaxASCII
		}
		if wide {
			b.WriteByte('*')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// changed are the project paths the files git listed fall in, one per file
// (pathOf, or a link target of disk, UnderTarget: a name outside ASCII
// that may be one counts), leaving out what a glob matched beyond its name.
func (pc projectConfig) changed(files []string, links map[string]projectPath) []projectPath {
	targets := slices.Sorted(maps.Keys(links))
	var out []projectPath
	for _, f := range files {
		if p, _, ok := pc.pathOf(f); ok {
			out = append(out, p)
			continue
		}
		for _, t := range targets {
			if UnderTarget(f, t) {
				out = append(out, links[t])
				break
			}
		}
	}
	return out
}

// linkedChanges are the files of all (every file git lists, with no
// pathspec) at or under a link target of links (UnderTarget), which git's
// pathspec may have left out: its icase folds ASCII only, so a literal
// .agents/skills misses a tracked .agents/ſkills/x/SKILL.md that macOS
// writes into .agents/skills/x/.
func linkedChanges(all []string, links map[string]projectPath) []string {
	return slices.DeleteFunc(slices.Clone(all), func(f string) bool {
		for t := range links {
			if UnderTarget(f, t) {
				return false
			}
		}
		return true
	})
}

// TargetMatch says whether a path is a link's target or lies under it
// (MatchTarget).
type TargetMatch int

const (
	// NotUnder: the path is neither the target nor under it.
	NotUnder TargetMatch = iota
	// MaybeUnder: it may be, and magnum cannot tell: an element of the path
	// and the target's that differ have a byte outside ASCII, which a
	// case-insensitive filesystem folds by rules of its own (APFS opens
	// claß.md for class.md, ﬁrst.md for first.md and a decomposed name for
	// its composed spelling; HFS+ also ignores some code points).
	MaybeUnder
	// Under: the path is the target or lies under it.
	Under
)

// MatchTarget says whether f, a path relative to the checkout's root, is
// the link target t or lies under it, element by element, in ASCII case:
// git names a tracked file in its own case, while a link's text, which its
// target keeps (linkTarget), may spell it in another, and every
// case-insensitive filesystem folds ASCII alike. An element with a byte
// outside ASCII matches only one of the same bytes for sure; where it
// differs, the answer is MaybeUnder.
func MatchTarget(f, t string) TargetMatch {
	fp, tp := strings.Split(f, "/"), strings.Split(t, "/")
	if len(fp) < len(tp) {
		return NotUnder
	}
	m := Under
	for i, e := range tp {
		switch {
		case asciiEqualFold(fp[i], e):
		case !isASCII(fp[i]) || !isASCII(e):
			m = MaybeUnder
		default:
			return NotUnder
		}
	}
	return m
}

// UnderTarget reports whether f may be the link target t or lie under it
// (MatchTarget: Under or MaybeUnder): a name magnum cannot fold counts, so
// the session and the reload check err toward the config.
func UnderTarget(f, t string) bool { return MatchTarget(f, t) != NotUnder }

// asciiEqualFold reports whether a and b are equal with ASCII letters
// folded, every other byte as it is.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// isASCII reports whether s has no byte outside ASCII.
func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// pathOf returns the project path f (relative to the checkout's root) is,
// or lies under, and the rest of f under it ("" for the path itself),
// matched in any case and under Unicode folding (strings.EqualFold): a
// case-insensitive filesystem (macOS's) opens .Claude/settings.json or
// .mcp.jſon for .claude/settings.json or .mcp.json. Nothing lies under a
// file's path; a path in any directory (anyDir) matches f's base name
// there too, and a directory's matches the first directory of f below the
// root of its name (packages/web/.claude/skills/x/SKILL.md lies under
// .claude/, the rest skills/x/SKILL.md).
func (pc projectConfig) pathOf(f string) (p projectPath, rest string, ok bool) {
	top, rest, nested := strings.Cut(f, "/")
	for _, p := range pc.paths {
		if strings.EqualFold(top, p.name) && (p.dir || !nested) {
			return p, rest, true
		}
	}
	base := f[strings.LastIndex(f, "/")+1:]
	for _, p := range pc.paths {
		if p.anyDir && strings.EqualFold(base, p.name) {
			return p, "", true
		}
	}
	elems := strings.Split(f, "/")
	for i := 1; i < len(elems)-1; i++ {
		for _, p := range pc.paths {
			if p.anyDir && p.dir && strings.EqualFold(elems[i], p.name) {
				return p, strings.Join(elems[i+1:], "/"), true
			}
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
// project config kept out (s.declined, by project_untrust), or clears the
// mark, for a kind that reloads it while it runs. Best effort: a store
// error is logged, and a session without the mark counts as one that
// loaded it.
func (m *Manager) noteSessionProject(ctx context.Context, id int64, kind string, s projectScope) {
	if !projectConfigs[kind].reloads {
		return
	}
	ctx = context.WithoutCancel(ctx)
	key := kvSessionProjectOut(id)
	var err error
	if s.declined && !s.docsOff {
		err = m.d.Store.SetKV(ctx, key, "1")
	} else {
		err = m.d.Store.DeleteKV(ctx, key)
	}
	if err != nil {
		m.logErr(ctx, err, "agents: session %d: %s: %v", id, key, err)
	}
}

// ProjectPaths are the paths, relative to the checkout's root, that kind
// loads its project config from (claude: .claude, .mcp.json and CLAUDE.md,
// CLAUDE.local.md and AGENTS.md at the root and, as gitx.AnyDir globs, in
// any directory, .claude/ and what lies under it too: anyDirSpecs), for a
// comparison limited to them (gitx.Client.ChangedUnder, whose answer
// ProjectTouched reads); nil for a kind magnum does not compare.
func ProjectPaths(kind string) []string {
	var out []string
	for _, p := range projectConfigs[kind].paths {
		out = append(append(out, p.name), p.anyDirSpecs()...)
	}
	return out
}

// anyDirSpecs are p in any directory as gitx.AnyDir globs (globName): the
// file's name, or the directory's name and every file under it
// (gitx.AnyDirTree); none for a path at the root only.
func (p projectPath) anyDirSpecs() []string {
	if !p.anyDir {
		return nil
	}
	if p.dir {
		return []string{gitx.AnyDir + globName(p.name), gitx.AnyDir + globName(p.name) + gitx.AnyDirTree}
	}
	return []string{gitx.AnyDir + globName(p.name)}
}

// ProjectTouched reports whether paths (a PR's changed files, relative to
// the repository root) name the project config kind loads from the
// checkout: a file at or under one of its paths, or of the name of a path
// in any directory, or under a directory of such a name (a .claude/ below
// the root), in any case (pathOf: macOS opens .Claude/settings.json for
// .claude/settings.json). A kind magnum does not compare touches nothing.
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

// ProjectLinks are the targets, relative to the checkout dir's root, of
// the links among kind's paths at its root and under its project
// directories, and under a directory such a link leads to, as the disk has
// them (onDisk: what a session in dir loaded through them), sorted; partial
// reports that the walk stopped at maxLinkWalk entries, so one may be
// missing. None for a kind magnum does not compare.
func ProjectLinks(kind, dir string) (targets []string, partial bool) {
	pc, ok := projectConfigs[kind]
	if !ok || dir == "" {
		return nil, false
	}
	d, _ := pc.onDisk(dir)
	return slices.Sorted(maps.Keys(d.links)), d.partial
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

// errNoBase and errNoCheckout are what projectChanges answers, without
// running git, for a launch that has nothing to compare: no base ref (a
// stage without a PR base), or a directory that is no git checkout (the
// retro and the notes curation run an agent in magnum's own directories).
// checkoutProject leaves the project config out for them all the same, and
// logs it at debug: nothing is wrong, unlike a git that fails in a checkout.
var (
	errNoBase     = errors.New("no base to compare with")
	errNoCheckout = errors.New("no git checkout")
)

// inGitCheckout reports whether dir is in a git work tree the way git finds
// one, without running git: a .git (a directory, or a file in a worktree)
// in dir or in any parent of its real path. What cannot be told (an error
// other than a missing .git) counts as a checkout, so that git decides.
func inGitCheckout(dir string) bool {
	d := filepath.Clean(dir)
	if resolved, err := filepath.EvalSymlinks(d); err == nil {
		d = resolved
	}
	for {
		if _, err := os.Lstat(filepath.Join(d, ".git")); !errors.Is(err, fs.ErrNotExist) {
			return true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return false
		}
		d = parent
	}
}

// projectChanges lists the files paths name in dir that differ from the
// merge base of HEAD and base, but the untracked ones skip reports, with
// that merge base. It answers errNoBase or errNoCheckout, without running
// git, for a launch that has no base ref or a dir that is no git checkout.
func (m *Manager) projectChanges(ctx context.Context, g *gitx.Client, dir, base string, skip func(string) bool, paths []string) ([]string, string, error) {
	switch {
	case m.d.Runner == nil:
		return nil, "", errors.New("no command runner")
	case base == "":
		return nil, "", errNoBase
	case !inGitCheckout(dir):
		return nil, "", errNoCheckout
	}
	mb, err := g.MergeBase(ctx, dir, "HEAD", base)
	if err != nil {
		return nil, "", err
	}
	files, err := g.WorkTreeChanges(ctx, dir, mb, skip, paths...)
	return files, mb, err
}

// committedChanges reports whether the commits from the merge base mb to
// HEAD change the project config disk names (git diff mb...HEAD over the
// same paths, and with links every file it lists, matched to their
// targets: linkedChanges; a link's target mapped to the link as for the
// work tree), which a PR's file list shows; false when only the work tree
// differs (untracked or ignored files, uncommitted edits: what a round left
// in the slot). A git that cannot tell counts as a change, so the record
// keeps blaming the PR (it is only a warning).
func (m *Manager) committedChanges(ctx context.Context, g *gitx.Client, role config.Role, dir, mb string, pc projectConfig, disk diskPaths) bool {
	files, err := g.ChangedUnder(ctx, dir, mb, "HEAD", disk.git...)
	if err == nil && len(disk.links) > 0 {
		var all []string
		if all, err = g.ChangedNames(ctx, dir, mb, "HEAD"); err == nil {
			files = append(files, linkedChanges(all, disk.links)...)
		}
	}
	if err != nil {
		m.logErr(ctx, err, "agents: %s: cannot list the commits' changes of %s since the merge base, so they count as the PR's: %v", role.Name, pc.labels(nil), err)
		return true
	}
	return len(pc.changed(files, disk.links)) > 0
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
// file of the kind's (.mcp.json, CLAUDE.md) is never a log. Names match in
// any case, as the project paths do (pathOf): .CLAUDE/LOG/x.LOG is a log,
// while .Claude/Settings.Local.JSON still counts.
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
// declares (declaredMCPServers) that the session turns off. A server the
// user's Codex config declares too (userMCPServers) is always one, whatever
// its table in the checkout holds, with no transport of its own: the user's
// table carries it, and a file the PR controls must never switch a server
// on. Any other server needs the transport of its table (config.ProjectServer):
// one with neither a url nor a command, or with both, has none to be given
// and stays on, as do a name that is no bare key and a file that cannot be
// read; each is logged without the path or any value of the file.
func (m *Manager) projectServers(role config.Role, path string) []config.ProjectServer {
	data, err := fsx.ReadRegular(path, codexConfigMax) // the PR's file: never a symlink, a FIFO or more than the cap
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	var servers []config.ProjectServer
	var skipped []string
	if err == nil {
		servers, skipped, err = declaredMCPServers(string(data))
	}
	if err != nil {
		if pe := (*fs.PathError)(nil); errors.As(err, &pe) {
			err = pe.Err
		}
		m.logAt(slog.LevelWarn, "agents: %s: cannot read the checkout's Codex config, so its MCP servers stay on: %v", role.Name, err)
		return nil
	}
	for _, n := range skipped {
		m.logf("agents: %s: MCP server %q of the checkout's Codex config is no TOML bare key, so it stays on", role.Name, n)
	}
	var own map[string]bool
	if len(servers) > 0 {
		own = m.userMCPServers(role)
	}
	var out []config.ProjectServer
	for _, s := range servers {
		switch {
		case own[s.Name]:
			s.Transport = ""
		case s.Transport == "":
			m.logf("agents: %s: MCP server %q of the checkout's Codex config has no single url or command, so it stays on", role.Name, s.Name)
			continue
		}
		out = append(out, s)
	}
	return out
}

// ProjectNote is the PR's record of a session of one kind launched without
// the checkout's project configuration (store.KVPRProject).
type ProjectNote struct {
	Head     string    `json:"head"`     // the checkout's head at that launch ("" = unknown)
	At       time.Time `json:"at"`       // the launch
	Files    int       `json:"files"`    // the files of the project config that differ from the merge base
	Compared bool      `json:"compared"` // false: git could not compare, so the config was left out anyway
	// Paths name the kind's paths those files fall in, as a sentence does
	// (".codex/", "AGENTS.md", never a path the PR named); none = all of
	// them (git could not compare, or a record written before they were
	// named).
	Paths []string `json:"paths,omitempty"`
	// DocsOnly: they were all instruction files, kept out by the kind's
	// project_docs_off while the rest of the project config loaded.
	DocsOnly bool `json:"docs_only,omitempty"`
	// Residue: git compared them, and none is a file the PR's commits
	// since the merge base change: they are untracked or ignored files a
	// round left in the slot, or uncommitted edits, so they say nothing of
	// the PR (the card and the review's Checks say so). False when git
	// could not tell, and in a record written before it was kept. The
	// auto-approval's gate reads no record: the PR's file list decides
	// there, with the links of the head's tree.
	Residue bool `json:"residue,omitempty"`
}

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

// declinedOf reads the PR's record of kind when it names head.
func declinedOf(ctx context.Context, st *store.Store, prID int64, kind, head string) (ProjectNote, bool) {
	n, ok := ProjectDeclined(ctx, st, prID, kind)
	return n, ok && head != "" && n.Head == head
}

// ProjectSentences is what the board's card says of the PR's head: one
// sentence per CLI whose sessions of that head ran without the PR's
// project configuration, in projectKinds' order, naming the files and how
// ("Codex ran without the PR's AGENTS.md changes (its sessions loaded no
// AGENTS.md)"; for a record of residue, "Claude ran without the checkout's
// .claude/ files, which the PR's commits do not change (…)": projectCheckOf);
// "" when none did.
func ProjectSentences(ctx context.Context, st *store.Store, prID int64, head string) string {
	var out []string
	for _, kind := range projectKinds {
		if n, ok := declinedOf(ctx, st, prID, kind, head); ok {
			pc := projectConfigs[kind]
			how := pc.how
			if n.DocsOnly {
				how = pc.docsHow
			}
			out = append(out, projectCheckOf(n, pc.cli, pc.labels(n.Paths))+" ("+how+")")
		}
	}
	return strings.Join(out, " ")
}

// NoteDeclinedProjects sets the judge's ProjectChecks from the PR's
// records of head (the round's), which each launch of those CLIs writes,
// for the kinds among the round's roles only (kinds: their agent kinds,
// the judge's included), in projectKinds' order: a record names the
// kind's last launch on the head, which may be a session the round did not
// run (a reviewer paused, or left out of the round).
func NoteDeclinedProjects(ctx context.Context, st *store.Store, prID int64, head string, kinds []string, jd *JudgeData) {
	var checks []string
	for _, kind := range projectKinds {
		if !slices.Contains(kinds, kind) {
			continue
		}
		if n, ok := declinedOf(ctx, st, prID, kind, head); ok {
			pc := projectConfigs[kind]
			checks = append(checks, projectCheckOf(n, pc.cli, pc.labels(n.Paths)))
		}
	}
	jd.ProjectChecks = projectChecks(checks)
}

// recordProject keeps the PR's record of how role's session treats its
// checkout's project configuration (one per kind): a declined one sets it
// and is the kind's event (counts, the paths' labels and the head, never a
// path the PR named); one that found the config unchanged (or absent)
// clears it. Best effort: a store error is logged.
func (m *Manager) recordProject(ctx context.Context, prID int64, role config.Role, dir string, s projectScope) {
	if !s.checked {
		return
	}
	pc := projectConfigs[s.kind]
	ctx = context.WithoutCancel(ctx)
	key := store.KVPRProject(prID, s.kind)
	if !s.declined {
		if err := m.d.Store.DeleteKV(ctx, key); err != nil {
			m.logErr(ctx, err, "agents: %s: clear %s: %v", role.Name, key, err)
		}
		return
	}
	note, _ := json.Marshal(ProjectNote{Head: s.head, At: m.now().UTC(), Files: s.files, Compared: s.compared, Paths: s.names, DocsOnly: s.docsOff,
		Residue: s.residue})
	if err := m.d.Store.SetKV(ctx, key, string(note)); err != nil {
		m.logErr(ctx, err, "agents: %s: record %s: %v", role.Name, key, err)
	}
	names := pc.labels(s.names)
	why := fmt.Sprintf("%d files differ from the merge base", s.files)
	if s.files == 1 {
		why = "1 file differs from the merge base"
	}
	if !s.compared {
		why = fmt.Sprintf("magnum could not compare %s with the merge base", names)
	}
	kept := pc.kept
	if s.docsOff {
		kept = pc.docsKept
	}
	msg := fmt.Sprintf("%s runs without the PR's %s changes: %s, so %s", role.Name, names, why, kept)
	if s.residue {
		msg = fmt.Sprintf("%s runs without the checkout's %s files: %s, but not in the PR's commits, so %s", role.Name, names, why, kept)
	}
	data, _ := json.Marshal(map[string]any{"role": role.Name, "checkout": dir, "files": s.files, "compared": s.compared, "head": s.head,
		"paths": s.names, "docs_only": s.docsOff, "residue": s.residue})
	subject := m.prSubject(ctx, prID)
	if _, err := m.d.Store.AppendEvent(ctx, store.Event{Level: "info", Subject: &subject, Kind: pc.event,
		Message: execx.Redact(msg), Data: data}); err != nil {
		m.logErr(ctx, err, "agents: event %s: %v", pc.event, err)
	}
}
