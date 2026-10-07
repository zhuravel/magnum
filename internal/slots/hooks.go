package slots

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/store"
)

// Per-PR worktree setup.
//
// A per-PR worktree gets the same treatment as a pool slot when its
// repository has a setup convention: it is provisioned under the workspace
// name PRSlug(N) (magnum-pr-<N>, exported as WT_BRANCH with every name
// variable that outranks it blanked) and torn down under the same name, so
// an app that derives its databases from the workspace name gets its own.
//
// Where the commands come from, first match wins:
//  1. a [[repo]] block with setup or teardown commands (they replace the
//     worktrunk hooks entirely);
//  2. the main clone's .config/wt.toml (worktrunk): [post-create] then
//     [post-start] after the worktree is created, [pre-remove] before it is
//     removed, each table's commands in file order, {{ branch }} and friends
//     substituted and shell-escaped as worktrunk does (a [[repo]] block with
//     wt_hooks = false skips this);
//  3. nothing.
//
// Setup commands run after `git worktree add` and before the slot is
// claimed; a failure moves the slot to broken (the review does not start).
// Teardown commands run before `git worktree remove`; failures are logged
// and the worktree is removed anyway.

// WTConfigFile is worktrunk's project config, relative to the main clone.
const WTConfigFile = ".config/wt.toml"

// Worktrunk hook types magnum runs (other tables in wt.toml are ignored).
const (
	HookPostCreate = "post-create"
	HookPostStart  = "post-start"
	HookPreRemove  = "pre-remove"
	// HookSetup and HookTeardown label [[repo]] commands.
	HookSetup    = "setup"
	HookTeardown = "teardown"
)

// WorkspaceNameVars are the workspace-name variables bin/worktree-setup and
// bin/worktree-archive read before WT_BRANCH; a per-PR worktree blanks them
// (the pool does the same through [pool.env]) so an orchestrator's value
// inherited from the daemon's environment cannot pick other databases.
var WorkspaceNameVars = []string{
	"CONDUCTOR_WORKSPACE_NAME", "EMDASH_TASK_NAME", "SUPERSET_WORKSPACE_NAME",
	"COMMANDER_CONTEXT_NAME", "CLAUDE_CODE_WORKTREE_NAME", "WM_HANDLE",
}

// DefaultStripEnv is always dropped from a checkout's rendered
// .mise.local.toml, a pool slot's (in addition to [pool] strip_env) as well
// as a per-PR worktree's (in addition to [[repo]] strip_env): a GitHub token
// there would override the reviewing identity's gh selection in the agents'
// panes, and the checkout runs PR-controlled code.
var DefaultStripEnv = []string{"GITHUB_PERSONAL_ACCESS_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"}

// stripKeys is DefaultStripEnv plus extra (a strip_env), sorted and deduplicated.
func stripKeys(extra []string) []string {
	keys := slices.Concat(DefaultStripEnv, extra)
	slices.Sort(keys)
	return slices.Compact(keys)
}

// ErrHook: a per-PR worktree's hook file or template cannot be used.
var ErrHook = errors.New("slots: per-PR hooks")

// Hook is one command of a hook table.
type Hook struct {
	Type    string // HookPostCreate, HookPostStart, HookPreRemove, HookSetup or HookTeardown
	Name    string // the table key ("setup"); the type for a bare string; "1", "2"… for [[repo]] commands
	Command string
}

// WTHooks are the hook tables of a .config/wt.toml, each in file order.
type WTHooks struct {
	PostCreate []Hook
	PostStart  []Hook
	PreRemove  []Hook
}

// ParseWTHooks reads the [post-create], [post-start] and [pre-remove]
// tables of a worktrunk project config (`post-start = "cmd"` is accepted as
// a one-command table named after the hook). Commands keep their
// {{ templates }}; every other key is ignored. A non-string command is an
// error.
func ParseWTHooks(data []byte) (WTHooks, error) {
	var raw map[string]any
	md, err := toml.Decode(string(data), &raw)
	if err != nil {
		return WTHooks{}, fmt.Errorf("%w: parse %s: %w", ErrHook, WTConfigFile, err)
	}
	var h WTHooks
	dst := map[string]*[]Hook{HookPostCreate: &h.PostCreate, HookPostStart: &h.PostStart, HookPreRemove: &h.PreRemove}
	for _, key := range md.Keys() { // document order
		if len(key) == 0 {
			continue
		}
		list, ok := dst[key[0]]
		if !ok {
			continue
		}
		switch len(key) {
		case 1:
			if _, isTable := raw[key[0]].(map[string]any); isTable {
				continue // its entries come as their own keys
			}
			cmd, isString := raw[key[0]].(string)
			if !isString {
				return WTHooks{}, fmt.Errorf("%w: %s: %s must be a string or a table of strings", ErrHook, WTConfigFile, key[0])
			}
			*list = append(*list, Hook{Type: key[0], Name: key[0], Command: cmd})
		case 2:
			cmd, isString := raw[key[0]].(map[string]any)[key[1]].(string)
			if !isString {
				return WTHooks{}, fmt.Errorf("%w: %s: %s.%s must be a string", ErrHook, WTConfigFile, key[0], key[1])
			}
			*list = append(*list, Hook{Type: key[0], Name: key[1], Command: cmd})
		}
	}
	return h, nil
}

// PRSlug is the workspace name of per-PR worktree number: magnum-pr-<N>. It
// is WT_BRANCH for the worktree's hooks and agents.
func PRSlug(number int) string { return "magnum-pr-" + strconv.Itoa(number) }

var dbSlugRe = regexp.MustCompile(`[^A-Za-z0-9_]`)

// DBSlug sanitizes a workspace name the way talkable's database.yml does
// (WORKTREE_NAME.gsub(/[^a-zA-Z0-9_]/, "_")[0, 30]): magnum-pr-7 →
// magnum_pr_7.
func DBSlug(name string) string {
	s := dbSlugRe.ReplaceAllString(name, "_")
	if len(s) > 30 {
		s = s[:30]
	}
	return s
}

// PerPREnv is the environment of per-PR worktree number (path, main clone
// clone) for its hooks and its agents: WT_BRANCH=PRSlug(number), every
// WorkspaceNameVars entry blank, then the [[repo]] env (rc may be nil) on top.
func PerPREnv(rc *config.Repo, number int, path, clone string) map[string]string {
	slug := PRSlug(number)
	env := map[string]string{"WT_BRANCH": slug}
	for _, k := range WorkspaceNameVars {
		env[k] = ""
	}
	if rc != nil {
		maps.Copy(env, rc.RenderEnv(slug, path, clone))
	}
	return env
}

// templateRe matches worktrunk's {{ var }} and {{ var | filter }} (with the
// whitespace-trimming {{- -}} forms).
var templateRe = regexp.MustCompile(`\{\{-?\s*([A-Za-z_][A-Za-z0-9_]*)\s*(?:\|\s*([A-Za-z_][A-Za-z0-9_]*)\s*)?-?\}\}`)

// expandTemplate substitutes worktrunk template variables in cmd, each
// value shell-escaped after its filter (shellQuote), as worktrunk does for
// hook commands: the PR's base ref and the paths cannot add shell syntax to
// the command. Unknown variables or filters, and any {{ or {% left over, are
// ErrHook: a command is never run with a template magnum could not fill in.
func expandTemplate(cmd string, vars map[string]string) (string, error) {
	var bad error
	out := templateRe.ReplaceAllStringFunc(cmd, func(m string) string {
		sub := templateRe.FindStringSubmatch(m)
		v, ok := vars[sub[1]]
		if !ok {
			if bad == nil {
				bad = fmt.Errorf("%w: unsupported template variable %q in %q", ErrHook, sub[1], cmd)
			}
			return m
		}
		switch sub[2] {
		case "":
		case "sanitize":
			v = strings.NewReplacer("/", "-", `\`, "-").Replace(v)
		default:
			if bad == nil {
				bad = fmt.Errorf("%w: unsupported template filter %q in %q", ErrHook, sub[2], cmd)
			}
			return m
		}
		return execx.ShellQuote(v) // one shell word, as worktrunk escapes template values
	})
	if bad != nil {
		return "", bad
	}
	if strings.Contains(out, "{{") || strings.Contains(out, "{%") {
		return "", fmt.Errorf("%w: unsupported template in %q", ErrHook, cmd)
	}
	return out, nil
}

// templateVars are the worktrunk variables magnum fills in for a per-PR
// worktree: branch is the workspace name (magnum-pr-<N>), repo the
// repository name, worktree_path/path/worktree the worktree,
// repo_path/repo_root/primary_worktree_path the main clone and
// base/default_branch the PR's base ref (chosen by whoever can push to the
// repository, hence expandTemplate's quoting).
func templateVars(slug, repo, path, clone, base string) map[string]string {
	_, name, _ := strings.Cut(repo, "/")
	return map[string]string{
		"branch": slug, "repo": name,
		"worktree_path": path, "path": path, "worktree": path, "worktree_name": filepath.Base(path),
		"repo_path": clone, "repo_root": clone, "primary_worktree_path": clone,
		"base": base, "default_branch": base,
	}
}

// perPRPlan is what magnum runs for one per-PR worktree.
type perPRPlan struct {
	source   string // "[[repo]] owner/name", WTConfigFile or "" (no commands)
	slug     string // PRSlug(number)
	setup    []Hook // templates expanded
	teardown []Hook
	copy     []string
	strip    []string
	env      map[string]string
}

// repoConfig returns the [[repo]] block of repo from Deps.Repos, or nil.
func (m *Manager) repoConfig(repo string) *config.Repo {
	for i := range m.d.Repos {
		if strings.EqualFold(m.d.Repos[i].Repo, repo) {
			return &m.d.Repos[i]
		}
	}
	return nil
}

// planPerPR resolves the setup/teardown commands, files and environment of
// per-PR worktree number of sl.RepoFullName (path sl.Path, main clone
// sl.MainClone) for a PR based on base. A wt.toml that cannot be read,
// parsed or templated is ErrHook.
func (m *Manager) planPerPR(sl store.Slot, number int, base string) (perPRPlan, error) {
	rc := m.repoConfig(sl.RepoFullName)
	p := perPRPlan{slug: PRSlug(number), env: PerPREnv(rc, number, sl.Path, sl.MainClone), strip: stripKeys(nil)}
	if rc != nil {
		p.copy = slices.Clone(rc.CopyFiles)
		p.strip = stripKeys(rc.StripEnv)
	}
	switch {
	case rc != nil && rc.HasCommands():
		p.source = "[[repo]] " + rc.Repo
		for i, c := range rc.Setup {
			p.setup = append(p.setup, Hook{Type: HookSetup, Name: strconv.Itoa(i + 1), Command: c})
		}
		for i, c := range rc.Teardown {
			p.teardown = append(p.teardown, Hook{Type: HookTeardown, Name: strconv.Itoa(i + 1), Command: c})
		}
		return p, nil
	case rc != nil && !rc.WTHooksEnabled():
		return p, nil
	}
	data, err := os.ReadFile(filepath.Join(sl.MainClone, WTConfigFile))
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return p, fmt.Errorf("%w: read %s: %w", ErrHook, WTConfigFile, err)
	}
	wt, err := ParseWTHooks(data)
	if err != nil {
		return p, err
	}
	vars := templateVars(p.slug, sl.RepoFullName, sl.Path, sl.MainClone, base)
	expand := func(hooks []Hook) ([]Hook, error) {
		out := make([]Hook, 0, len(hooks))
		for _, h := range hooks {
			cmd, err := expandTemplate(h.Command, vars)
			if err != nil {
				return nil, fmt.Errorf("%s [%s] %s: %w", WTConfigFile, h.Type, h.Name, err)
			}
			h.Command = cmd
			out = append(out, h)
		}
		return out, nil
	}
	if p.setup, err = expand(append(slices.Clone(wt.PostCreate), wt.PostStart...)); err != nil {
		return p, err
	}
	if p.teardown, err = expand(wt.PreRemove); err != nil {
		return p, err
	}
	if len(p.setup) > 0 || len(p.teardown) > 0 {
		p.source = WTConfigFile
	}
	return p, nil
}

// perPRLog is the transcript of a per-PR worktree's hooks under layout.Logs().
func perPRLog(slug string) string { return "perpr-" + slug + ".log" }

// miseAvailable reports whether the mise executable can be found; hooks of
// per-PR worktrees fall back to /bin/sh without it.
func (m *Manager) miseAvailable() bool {
	look := m.d.LookPath
	if look == nil {
		look = exec.LookPath
	}
	_, err := look(m.mise)
	return err == nil
}

// shellCmd runs script in dir with env: through `mise -C dir exec -- env
// K=V… /bin/sh -c script` when mise is available (as pool scripts do), else
// /bin/sh -c script with env overlaid (blank values exported blank).
func (m *Manager) shellCmd(dir string, env map[string]string, script, label string, timeout time.Duration) execx.Cmd {
	if m.miseAvailable() {
		return execx.Cmd{Name: m.mise, Args: MiseExecArgs(dir, env, script), Dir: dir, Timeout: timeout, Mutates: true, Label: label}
	}
	return execx.Cmd{Name: "/bin/sh", Args: []string{"-c", script}, Env: maps.Clone(env), Dir: dir,
		Timeout: timeout, Mutates: true, Label: label}
}

// runHooks runs hooks in sl.Path one after the other through the heavy
// lock, logging each to the slot's perpr log and recording a slot.hook_ran
// or slot.hook_failed event. With keepGoing a failure is recorded and the
// next hook still runs (teardown); otherwise the first failure is returned.
// The joined failures are returned either way.
func (m *Manager) runHooks(ctx context.Context, sl store.Slot, p perPRPlan, hooks []Hook, timeout time.Duration, keepGoing bool) error {
	subject := "slot:" + sl.Name
	var errs []error
	for _, h := range hooks {
		label := fmt.Sprintf("%s %s %s: %s", h.Type, sl.Name, h.Name, h.Command)
		err := m.runLogged(ctx, m.shellCmd(sl.Path, p.env, h.Command, label, timeout), perPRLog(p.slug))
		if err == nil {
			m.event(ctx, subject, "info", "slot.hook_ran",
				fmt.Sprintf("%s hook %q (%s) ran as %s: %s", h.Type, h.Name, p.source, p.slug, h.Command))
			continue
		}
		level := "error"
		if keepGoing {
			level = "warn"
		}
		m.event(ctx, subject, level, "slot.hook_failed",
			fmt.Sprintf("%s hook %q (%s) failed as %s: %s: %v (log: %s)", h.Type, h.Name, p.source, p.slug, h.Command, err, perPRLog(p.slug)))
		errs = append(errs, fmt.Errorf("%s hook %q: %w", h.Type, h.Name, err))
		if !keepGoing || ctx.Err() != nil {
			break
		}
	}
	return errors.Join(errs...)
}

// preparePerPR renders the main clone's .mise.local.toml into the worktree
// (strip list removed, the per-PR env set; trusted with mise when it is
// available) and copies the [[repo]] copy_files that exist, at the
// worktree's creation and at each round's checkout (render_mise), every write
// confined to the worktree (WriteCheckoutFile). Without a main
// .mise.local.toml nothing is rendered.
func (m *Manager) preparePerPR(ctx context.Context, sl store.Slot, p perPRPlan) error {
	src, err := os.ReadFile(filepath.Join(sl.MainClone, MiseLocal))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return fmt.Errorf("slots: read main %s: %w", MiseLocal, err)
	default:
		out, err := RenderMiseLocal(src, p.strip, p.env)
		if err != nil {
			return err
		}
		if err := WriteCheckoutFile(sl.Path, MiseLocal, out, 0o600); err != nil {
			return err
		}
		if m.miseAvailable() {
			if _, err := m.d.Run.Run(ctx, execx.Cmd{
				Name: m.mise, Args: []string{"-C", sl.Path, "trust", filepath.Join(sl.Path, MiseLocal)}, Dir: sl.Path,
				Mutates: true, Label: "mise trust " + sl.Name,
			}); err != nil {
				return fmt.Errorf("slots: mise trust %s: %w", sl.Name, err)
			}
		}
	}
	return copyIntoCheckout(sl.MainClone, sl.Path, p.copy)
}

// perPRDBSlug is the database slug a per-PR worktree's setup used: the
// marker bin/worktree-setup writes (tmp/.worktree-db-slug) when present,
// else DBSlug(slug); "" when no setup command ran.
func perPRDBSlug(sl store.Slot, p perPRPlan) string {
	if len(p.setup) == 0 {
		return ""
	}
	if b, err := os.ReadFile(filepath.Join(sl.Path, MarkerFile)); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	return DBSlug(p.slug)
}

// teardownPerPR runs the per-PR worktree's teardown commands before it is
// removed. A worktree directory that is already gone, an unknown PR number
// or an unusable wt.toml skip it (logged); failed commands are logged and
// tolerated. Only a cancelled ctx is returned, so the step runs again.
func (m *Manager) teardownPerPR(ctx context.Context, sl store.Slot, number int, base string) error {
	if !fsx.Exists(sl.Path) {
		return nil
	}
	subject := "slot:" + sl.Name
	if number <= 0 {
		m.event(ctx, subject, "warn", "slot.hook_failed", "teardown skipped: the PR number of "+sl.Name+" is unknown")
		return nil
	}
	p, err := m.planPerPR(sl, number, base)
	if err != nil {
		m.event(ctx, subject, "warn", "slot.hook_failed", fmt.Sprintf("teardown skipped: %v", err))
		return nil
	}
	if len(p.teardown) == 0 {
		return nil
	}
	if err := m.runHooks(ctx, sl, p, p.teardown, TeardownTimeout, true); err != nil {
		m.logf("slots: teardown of %s failed (removing it anyway): %v", sl.Name, err)
	}
	return ctx.Err()
}
