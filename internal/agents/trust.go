package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/gitx"
)

// gitRootTimeout bounds the `git rev-parse --git-common-dir` EnsureTrust runs.
const gitRootTimeout = 10 * time.Second

// trustMu serializes EnsureTrust's read-modify-write of the CLI config files
// (several PRs may start agents at once).
var trustMu sync.Mutex

// EnsureTrust records dir as trusted in the agent CLI's own config, so a
// fresh agent started there does not stop at its first-launch trust dialog
// (which blocks every prompt with agent_blocked). StartAgent calls it before
// agent.start; slots/engine may call it when they prepare a checkout.
//
//   - codex: [projects."<path>"] trust_level = "trusted" in the Codex config
//     (Deps.CodexConfig, else $CODEX_HOME/config.toml, else
//     ~/.codex/config.toml) for dir and for its repository root (the main
//     clone: the parent of `git -C dir rev-parse --path-format=absolute
//     --git-common-dir` when that ends in /.git), which is where Codex applies
//     the trust. A missing table is appended as two lines; a table without
//     trust_level gets the line after its header; an explicit other
//     trust_level is left alone. The file is otherwise untouched.
//   - claude: projects["<dir>"].hasTrustDialogAccepted = true in the Claude
//     config (Deps.ClaudeConfig, else $CLAUDE_CONFIG_DIR/.claude.json, else
//     ~/.claude.json), merged into an existing project object or created as
//     {"allowedTools":[],"hasTrustDialogAccepted":true}. Every other key and
//     the key order are kept; the file stays indented (two spaces) when it
//     was, compact otherwise. A missing file is left missing (Claude has
//     not been set up there).
//
// The symlink-resolved form of each path is trusted too when it differs.
// Writes are atomic (temp file + rename, keeping the file mode, through a
// symlinked config to its target) and happen only when an entry is missing,
// with one log line naming what was added. In a test binary the default
// paths are never used: without Deps.CodexConfig / Deps.ClaudeConfig
// EnsureTrust does nothing.
//
// Trusting magnum's own checkouts (and, for codex, the repository root, i.e.
// the user's main clone) is deliberate, and the entries are never removed:
// the agents work there like the user's own sessions do, project settings
// included, and a round runs the PR's code anyway (setup hooks, the judge's
// specs); watches skip cross-repository PRs by default.
//
// Every other kind (shell, droid, omp, any user-declared kind) is a no-op
// that returns nil: how droid and omp record trusted folders, and whether
// they ask at all, is unknown, so magnum neither edits their config nor
// answers their dialogs (see AnswerTrustDialog); a blocked start or prompt
// is left for the human.
func (m *Manager) EnsureTrust(ctx context.Context, kind, dir string) error {
	if kind != KindCodex && kind != KindClaude {
		return nil
	}
	if dir == "" || !filepath.IsAbs(dir) {
		return fmt.Errorf("agents: trust %s: want an absolute dir, got %q", kind, dir)
	}
	dir = filepath.Clean(dir)
	if kind == KindCodex {
		path := m.codexConfigPath()
		if path == "" {
			return nil
		}
		added, err := ensureCodexTrust(path, m.codexTrustDirs(ctx, dir))
		if err != nil {
			return fmt.Errorf("agents: trust %s for codex: %w", dir, err)
		}
		if len(added) > 0 {
			m.logf("agents: trusted %s for codex in %s", strings.Join(added, ", "), path)
		}
		return nil
	}
	path := m.claudeConfigPath()
	if path == "" {
		return nil
	}
	added, err := ensureClaudeTrust(path, withRealPath(dir))
	if err != nil {
		return fmt.Errorf("agents: trust %s for claude: %w", dir, err)
	}
	if len(added) > 0 {
		m.logf("agents: trusted %s for claude in %s", strings.Join(added, ", "), path)
	}
	return nil
}

func (m *Manager) codexConfigPath() string {
	if m.d.CodexConfig != "" {
		return m.d.CodexConfig
	}
	if testing.Testing() {
		return ""
	}
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return filepath.Join(h, "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "config.toml")
}

func (m *Manager) claudeConfigPath() string {
	if m.d.ClaudeConfig != "" {
		return m.d.ClaudeConfig
	}
	if testing.Testing() {
		return ""
	}
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude.json")
}

// codexTrustDirs is the repository root Codex applies trust to, then dir
// (each with its symlink-resolved form), without duplicates.
func (m *Manager) codexTrustDirs(ctx context.Context, dir string) []string {
	return withRealPath(m.repoRoot(ctx, dir), dir)
}

// repoRoot is the main clone's root of the repository dir belongs to (also
// for a linked worktree), or dir when git cannot tell.
func (m *Manager) repoRoot(ctx context.Context, dir string) string {
	if m.d.Runner == nil {
		return dir
	}
	// An inherited GIT_DIR or GIT_COMMON_DIR would answer for another repository.
	res, err := m.d.Runner.Run(ctx, execx.Cmd{Name: "git", Args: []string{"-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir"},
		Unset: gitx.ScrubbedEnv(), Timeout: gitRootTimeout, Label: "git common dir"})
	out := strings.TrimSpace(firstLine(string(res.Stdout)))
	if err != nil || out == "" || !filepath.IsAbs(out) {
		return dir
	}
	if out = filepath.Clean(out); filepath.Base(out) == ".git" {
		return filepath.Dir(out)
	}
	return dir
}

// withRealPath lists each path and, when it differs, its symlink-resolved
// form, without duplicates.
func withRealPath(paths ...string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range paths {
		add(p)
		if r, err := filepath.EvalSymlinks(p); err == nil {
			add(r)
		}
	}
	return out
}

// ensureCodexTrust makes every dir trusted in the Codex config at path and
// returns the dirs it added.
func ensureCodexTrust(path string, dirs []string) ([]string, error) {
	trustMu.Lock()
	defer trustMu.Unlock()
	cf, err := readConfigFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if _, err := toml.Decode(string(cf.data), &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	projects, _ := doc["projects"].(map[string]any)
	inline := inlineProjects.Match(cf.data)
	out := cf.data
	var added []string
	for _, d := range dirs {
		entry, ok := projects[d]
		if !ok {
			if inline {
				// An inline table cannot take [projects."…"] headers
				// (inline tables are closed in TOML).
				return nil, fmt.Errorf("%s: projects is an inline table; add %s by hand", path, d)
			}
			out = appendCodexProject(out, d)
			added = append(added, d)
			continue
		}
		tbl, _ := entry.(map[string]any)
		if _, has := tbl["trust_level"]; has || tbl == nil {
			continue // trusted already, or an explicit decision magnum keeps
		}
		var inserted bool
		if out, inserted = insertCodexTrustLevel(out, d); inserted {
			added = append(added, d)
		}
	}
	if len(added) == 0 {
		return nil, nil
	}
	// Never write a file Codex could no longer read.
	var check map[string]any
	if _, err := toml.Decode(string(out), &check); err != nil {
		return nil, fmt.Errorf("edit of %s would not parse: %w", path, err)
	}
	cp, _ := check["projects"].(map[string]any)
	for _, d := range added {
		if tbl, _ := cp[d].(map[string]any); tbl["trust_level"] != "trusted" {
			return nil, fmt.Errorf("edit of %s does not trust %s", path, d)
		}
	}
	if err := writeFileAtomic(cf.real, out, cf.mode); err != nil {
		return nil, err
	}
	return added, nil
}

// inlineProjects spots `projects = {…}`, which [projects."…"] cannot extend.
var inlineProjects = regexp.MustCompile(`(?m)^[ \t]*projects[ \t]*=`)

// codexTrustLine is the line Codex writes when the user trusts a folder.
const codexTrustLine = `trust_level = "trusted"`

// appendCodexProject appends a [projects."<dir>"] table (a blank line first
// unless the file is empty or already ends with one).
func appendCodexProject(data []byte, dir string) []byte {
	var b bytes.Buffer
	b.Write(data)
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		b.WriteByte('\n')
	}
	if len(bytes.TrimSpace(data)) > 0 && !bytes.HasSuffix(b.Bytes(), []byte("\n\n")) {
		b.WriteByte('\n')
	}
	b.WriteString("[projects." + tomlQuote(dir) + "]\n" + codexTrustLine + "\n")
	return b.Bytes()
}

// insertCodexTrustLevel adds the trust line right after an existing
// [projects."<dir>"] header line; false when there is no such line (the
// table is spelled another way, e.g. inline).
func insertCodexTrustLevel(data []byte, dir string) ([]byte, bool) {
	headers := []string{"[projects." + tomlQuote(dir) + "]", "[projects.'" + dir + "']"}
	isHeader := func(line string) bool {
		line = strings.TrimSpace(line)
		for _, h := range headers {
			if rest, ok := strings.CutPrefix(line, h); ok {
				if rest = strings.TrimSpace(rest); rest == "" || strings.HasPrefix(rest, "#") {
					return true
				}
			}
		}
		return false
	}
	lines := strings.SplitAfter(string(data), "\n")
	for i, l := range lines {
		if !isHeader(l) {
			continue
		}
		var b strings.Builder
		for j, x := range lines {
			b.WriteString(x)
			if j == i {
				if !strings.HasSuffix(x, "\n") {
					b.WriteByte('\n')
				}
				b.WriteString(codexTrustLine + "\n")
			}
		}
		return []byte(b.String()), true
	}
	return data, false
}

// tomlQuote renders s as a TOML basic string.
func tomlQuote(s string) string { return config.TOMLString(s) }

// ensureClaudeTrust sets projects[dir].hasTrustDialogAccepted for every dir
// in the Claude config at path and returns the dirs it changed. A missing
// config is left alone.
func ensureClaudeTrust(path string, dirs []string) ([]string, error) {
	trustMu.Lock()
	defer trustMu.Unlock()
	cf, err := readConfigFile(path)
	if err != nil || !cf.exists {
		return nil, err
	}
	top, err := parseObject(cf.data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var projects members
	pi := top.index("projects")
	if pi >= 0 && string(top[pi].val) != "null" {
		if projects, err = parseObject(top[pi].val); err != nil {
			return nil, fmt.Errorf("parse %s: projects: %w", path, err)
		}
	}
	var added []string
	for _, d := range dirs {
		di := projects.index(d)
		if di < 0 {
			projects = append(projects, member{d, json.RawMessage(`{"allowedTools":[],"hasTrustDialogAccepted":true}`)})
			added = append(added, d)
			continue
		}
		entry, err := parseObject(projects[di].val)
		if err != nil {
			return nil, fmt.Errorf("parse %s: projects[%q]: %w", path, d, err)
		}
		ai := entry.index("hasTrustDialogAccepted")
		switch {
		case ai >= 0 && string(entry[ai].val) == "true":
			continue
		case ai >= 0:
			entry[ai].val = json.RawMessage("true")
		default:
			entry = append(entry, member{"hasTrustDialogAccepted", json.RawMessage("true")})
		}
		projects[di].val = entry.encode()
		added = append(added, d)
	}
	if len(added) == 0 {
		return nil, nil
	}
	if pi >= 0 {
		top[pi].val = projects.encode()
	} else {
		top = append(top, member{"projects", projects.encode()})
	}
	out, err := formatLike(cf.data, top.encode())
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", path, err)
	}
	if err := writeFileAtomic(cf.real, out, cf.mode); err != nil {
		return nil, err
	}
	return added, nil
}

// member is one key of a JSON object, its value kept verbatim.
type member struct {
	key string
	val json.RawMessage
}

// members is a JSON object in document order.
type members []member

// index is the position of the last member named key (the one JSON parsers
// keep), -1 when absent.
func (ms members) index(key string) int {
	for i := len(ms) - 1; i >= 0; i-- {
		if ms[i].key == key {
			return i
		}
	}
	return -1
}

// encode renders the object compactly, keys in order.
func (ms members) encode() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range ms {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(jsonString(m.key))
		b.WriteByte(':')
		b.Write(m.val)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// jsonString encodes s without HTML escaping (as JSON.stringify does).
func jsonString(s string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimRight(b.Bytes(), "\n")
}

// parseObject reads a JSON object keeping its keys in order and its values
// verbatim.
func parseObject(data []byte) (members, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("want a JSON object, got %v", tok)
	}
	var out members
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("want an object key, got %v", tok)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		out = append(out, member{key, raw})
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the JSON object")
	}
	return out, nil
}

// formatLike lays compact JSON out the way orig was: indented (with orig's
// indent unit, two spaces by default) when orig's first member starts on a
// new line, compact otherwise; a trailing newline is kept.
func formatLike(orig []byte, compact []byte) ([]byte, error) {
	var b bytes.Buffer
	trimmed := bytes.TrimLeft(orig, " \t\r\n")
	if indent, ok := indentUnit(trimmed); ok {
		if err := json.Indent(&b, compact, "", indent); err != nil {
			return nil, err
		}
	} else if err := json.Compact(&b, compact); err != nil {
		return nil, err
	}
	if bytes.HasSuffix(orig, []byte("\n")) {
		b.WriteByte('\n')
	}
	return b.Bytes(), nil
}

// indentUnit reports whether the object in data puts its first member on a
// new line, and the whitespace that indents it.
func indentUnit(data []byte) (string, bool) {
	if len(data) == 0 || data[0] != '{' {
		return "", false
	}
	rest := data[1:]
	i := bytes.IndexFunc(rest, func(r rune) bool { return r != ' ' && r != '\t' && r != '\r' && r != '\n' })
	if i < 0 {
		return "", false
	}
	gap := rest[:i]
	nl := bytes.LastIndexByte(gap, '\n')
	if nl < 0 || rest[i] == '}' {
		return "", false
	}
	if unit := string(gap[nl+1:]); unit != "" {
		return unit, true
	}
	return "  ", true
}

// configFile is a config file as read: its resolved path, content and mode.
type configFile struct {
	real   string // symlinks resolved: where the atomic write goes
	data   []byte
	mode   fs.FileMode
	exists bool
}

func readConfigFile(path string) (configFile, error) {
	cf := configFile{real: path, mode: 0o600}
	if r, err := filepath.EvalSymlinks(path); err == nil {
		cf.real = r
	}
	st, err := os.Stat(cf.real)
	if errors.Is(err, fs.ErrNotExist) {
		return cf, nil
	}
	if err != nil {
		return cf, err
	}
	if !st.Mode().IsRegular() {
		return cf, fmt.Errorf("%s: not a regular file", path)
	}
	cf.mode, cf.exists = st.Mode().Perm(), true
	cf.data, err = os.ReadFile(cf.real)
	return cf, err
}

// writeFileAtomic replaces path with data through a temp file in the same
// directory (created if missing), with mode perm.
func writeFileAtomic(path string, data []byte, perm fs.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".magnum-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if err = f.Chmod(perm); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (m *Manager) logf(format string, args ...any) {
	if m.d.Log != nil {
		m.d.Log.Printf("%s", execx.Redact(fmt.Sprintf(format, args...)))
	}
}
