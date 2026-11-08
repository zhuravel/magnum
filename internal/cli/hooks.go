package cli

// `magnum hooks install|uninstall|status [claude|codex]` puts magnum's own
// agent hook (hooks/magnum-agent-hook.sh, internal/agenthooks) into Claude
// Code's user settings (~/.claude/settings.json, $CLAUDE_CONFIG_DIR when set)
// and the hooks.json of each Codex home magnum's Codex roles read, on the
// operator's word, like `herdr integration` and installGhShim: a marker, a
// compare, a refusal of a file that is not magnum's, and a dry run. The
// settings are edited by a byte splice (hooks_json.go) that only appends;
// Codex's trust ([hooks.state] in config.toml) is never written: after an
// install the operator trusts the new hooks once.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum"
	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/textx"
)

// hooksSystem holds the seams of `magnum hooks`: the user's home and the
// env (CLAUDE_CONFIG_DIR, CODEX_HOME). Its defaults refuse the home and
// read no env in a test binary, so no test reaches the user's own
// ~/.claude or ~/.codex.
type hooksSystem struct {
	UserHome func() (string, error)
	Getenv   func(string) string
}

var hooksSys = hooksSystem{UserHome: hooksUserHome, Getenv: hooksGetenv}

func hooksUserHome() (string, error) {
	if testing.Testing() {
		return "", errors.New("a test never uses the user's home (set hooksSys)")
	}
	return os.UserHomeDir()
}

func hooksGetenv(key string) string {
	if testing.Testing() {
		return ""
	}
	return os.Getenv(key)
}

// claudeHookDir is Claude Code's config directory: $CLAUDE_CONFIG_DIR, else
// ~/.claude ("" when the home is unknown).
func claudeHookDir(getenv func(string) string, home string) string {
	return hookDir(getenv("CLAUDE_CONFIG_DIR"), home, ".claude")
}

// codexDefaultHome is the Codex home of a role whose env names none:
// $CODEX_HOME, else ~/.codex ("" when the home is unknown).
func codexDefaultHome(getenv func(string) string, home string) string {
	return hookDir(getenv("CODEX_HOME"), home, ".codex")
}

// hookDir is env (a leading ~/ read as home), else home/def.
func hookDir(env, home, def string) string {
	switch {
	case env == "~" || strings.HasPrefix(env, "~/"):
		if home == "" {
			return ""
		}
		return filepath.Join(home, strings.TrimPrefix(env, "~"))
	case env != "":
		return env
	case home == "":
		return ""
	}
	return filepath.Join(home, def)
}

// hookTarget is one place the hook goes: Claude's config directory, or one
// Codex home, with the roles that read it.
type hookTarget struct {
	agent string // agenthooks.AgentClaude or AgentCodex
	dir   string // "" when unknown
	users []string
	// fromShell: the directory comes from this shell's env
	// (CLAUDE_CONFIG_DIR, CODEX_HOME) or the default, not from a role's env.
	fromShell bool
}

// codexForgetStep is the way out of a stale trust record Codex will never
// replace (agents.CodexForgetStaleStep).
const codexForgetStep = agents.CodexForgetStaleStep

// hooksBeforeWrite runs after the install's plan and before its writes (nil:
// nothing); a test writes a settings file there, as Claude may.
var hooksBeforeWrite func()

// hookHomeFix is the fix for t's home that cannot be used (hookHomeProblem):
// in this shell's env when the directory comes from it, else in the role's.
func hookHomeFix(t hookTarget) string {
	if t.fromShell {
		return "set " + hookHomeEnv(t.agent) + " in this shell to an absolute path, or unset it"
	}
	return "give the role an absolute " + hookHomeEnv(t.agent) + " in its env"
}

// script is the hook script's path: <dir>/hooks/ for Claude (beside
// herdr's), the home itself for Codex.
func (t hookTarget) script() string {
	if t.agent == agenthooks.AgentClaude {
		return filepath.Join(t.dir, "hooks", hookScriptName)
	}
	return filepath.Join(t.dir, hookScriptName)
}

// settings is the file the hooks are declared in.
func (t hookTarget) settings() string {
	if t.agent == agenthooks.AgentClaude {
		return filepath.Join(t.dir, "settings.json")
	}
	return filepath.Join(t.dir, "hooks.json")
}

func (t hookTarget) events() []hookEvent {
	if t.agent == agenthooks.AgentClaude {
		return claudeHookEvents
	}
	return codexHookEvents
}

// entries are magnum's handlers in t, one per event.
func (t hookTarget) entries() []hookEntry {
	evs := t.events()
	out := make([]hookEntry, len(evs))
	for i, e := range evs {
		out[i] = hookEntry{Event: e.name, Command: hookCommand(t.script(), t.agent, e.arg), Timeout: e.timeout}
	}
	return out
}

// scriptBody is the script this build writes for t's agent.
func (t hookTarget) scriptBody() []byte {
	b, err := agenthooks.Script(magnum.AgentHookScript, t.agent)
	if err != nil {
		panic(err) // the embedded template and a known agent always render
	}
	return b
}

// label is the target as a line begins with it: "claude", "codex".
func (t hookTarget) label() string { return t.agent }

// hookTargets are the targets of which ("claude", "codex", "" = both as the
// configuration uses them): Claude's directory claudeDir while an agent
// role (the review's or the factory's) runs Claude, and each Codex home
// (codexHome of a role) a Codex role reads, in the roles' order. Named
// explicitly, a target with no role is still one: Claude's directory, the
// default Codex home defCodex.
func hookTargets(cfg *config.Config, claudeDir string, codexHome func(config.Role) string, defCodex, which string) []hookTarget {
	roles := slices.Concat(cfg.AllRoles(), cfg.Factory.Roles)
	var claudeUsers, codexHomes []string
	codexUsers, roleSet := map[string][]string{}, map[string]bool{}
	for _, r := range roles {
		switch {
		case !r.IsAgent():
		case r.Kind == config.KindClaude:
			claudeUsers = append(claudeUsers, r.Name)
		case r.Kind == config.KindCodex:
			h := codexHome(r)
			if _, seen := codexUsers[h]; !seen {
				codexHomes = append(codexHomes, h)
			}
			codexUsers[h] = append(codexUsers[h], r.Name)
			roleSet[h] = roleSet[h] || cfg.RoleEnv(r)["CODEX_HOME"] != ""
		}
	}
	var out []hookTarget
	if which == agenthooks.AgentClaude || which == "" && len(claudeUsers) > 0 {
		out = append(out, hookTarget{agent: agenthooks.AgentClaude, dir: claudeDir, users: claudeUsers, fromShell: true})
	}
	if which == agenthooks.AgentCodex && len(codexHomes) == 0 {
		codexHomes = []string{defCodex}
	}
	if which == agenthooks.AgentCodex || which == "" {
		for _, h := range codexHomes {
			out = append(out, hookTarget{agent: agenthooks.AgentCodex, dir: h, users: codexUsers[h], fromShell: !roleSet[h]})
		}
	}
	return out
}

// The states of a target (hookStatus.state).
const (
	hookCurrent    = "current"    // the script is this build's, every event has magnum's entry (and, Codex, its trust)
	hookOutdated   = "outdated"   // the script is magnum's, of an older version
	hookMissing    = "missing"    // no script and no entry
	hookModified   = "modified"   // a file is not magnum's, an entry or the script drifted, or the settings cannot be read
	hookIncomplete = "incomplete" // the script or some events' entries are missing
	hookUntrusted  = "untrusted"  // Codex: an entry of magnum's has no trust entry in config.toml, or one of the hook before its rewrite
	hookRefused    = "refused"    // the home is unknown or no absolute path: nothing is read or written there
)

// hooksStaleTrustDir is where install records the Codex trust entries a
// rewrite of magnum's own hooks left stale (agents.RecordStaleCodexTrust),
// under the state directory. Main points the agents package at it for the
// process (agents.SetStaleCodexTrustDir), so status, doctor and the
// daemon's hooks hold read it.
func hooksStaleTrustDir(l paths.Layout) string { return filepath.Join(l.State(), "codex-stale-trust") }

// hookHomeProblem says why t's home cannot be used ("" when it can): it is
// unknown, or not an absolute path, so a command would name a script
// relative to whatever directory the agent runs in.
func hookHomeProblem(t hookTarget) string {
	who := strings.Join(t.users, ", ")
	if who == "" {
		who = "no role"
	}
	switch {
	case t.dir == "":
		return fmt.Sprintf("the %s home of %s is unknown", agentName(t.agent), who)
	case !filepath.IsAbs(t.dir):
		return fmt.Sprintf("the %s home %q of %s is not an absolute path", agentName(t.agent), t.dir, who)
	}
	return ""
}

// hookStatus is what a target holds of magnum's hook.
type hookStatus struct {
	t       hookTarget
	state   string
	version int    // the installed script's (0: none, or no version line)
	why     string // what is wrong, for every state but current, missing and outdated
	foreign bool   // the file at the script's path is not magnum's: the install refuses it
	newer   bool   // the script is a newer magnum's: the install refuses it
	stale   bool   // Codex: a place is untrusted only by magnum's stale trust record
	places  []string
}

// line is the status as one line, in herdr's form: "claude: current (v1)
// (~/.claude/hooks/magnum-agent-hook.sh)", with what is wrong after a
// colon.
func (s hookStatus) line() string {
	if s.state == hookRefused {
		return s.t.label() + ": " + s.state + ": " + s.why
	}
	var b strings.Builder
	b.WriteString(s.t.label() + ": " + s.state)
	switch {
	case s.state == hookOutdated:
		fmt.Fprintf(&b, " (v%d < v%d)", s.version, agenthooks.ScriptVersion)
	case s.version > 0:
		fmt.Fprintf(&b, " (v%d)", s.version)
	}
	b.WriteString(" (" + inspTilde(s.t.script()) + ")")
	if s.why != "" {
		b.WriteString(": " + s.why)
	}
	return b.String()
}

// readHookStatus reads t's state from its files (and, for Codex, its
// trust in config.toml). It writes nothing but what
// agents.ReadCodexHooksTrust removes: magnum's own stale trust records whose
// entry changed or is gone.
func readHookStatus(t hookTarget) hookStatus {
	s := hookStatus{t: t}
	if why := hookHomeProblem(t); why != "" {
		s.state, s.why = hookRefused, why
		return s
	}
	settings, file := t.settings(), inspTilde(t.settings())
	script, serr := os.ReadFile(t.script())
	agent, version, managed := agenthooks.ScriptHeader(script)
	if serr == nil && managed {
		s.version = version
	}
	doc, rerr := os.ReadFile(settings)
	if errors.Is(rerr, fs.ErrNotExist) {
		doc, rerr = nil, nil
	}
	var states []hookEntryState
	if rerr == nil {
		states, rerr = readHookEntries(doc, t.agent, t.entries())
		if rerr != nil {
			rerr = fmt.Errorf("%s %w", file, rerr)
		}
	}
	var present bool
	var absent, drifted []string
	for _, e := range states {
		switch e.state {
		case entryAbsent:
			absent = append(absent, e.event)
		case entryDrifted:
			drifted = append(drifted, e.event)
		}
		present = present || e.state != entryAbsent
		s.places = append(s.places, e.places...)
	}
	set := func(state, why string) hookStatus { s.state, s.why = state, why; return s }
	switch {
	case serr != nil && !errors.Is(serr, fs.ErrNotExist):
		return set(hookModified, "cannot read the script: "+unwrapPathErr(serr).Error())
	case rerr != nil:
		return set(hookModified, rerr.Error())
	case serr != nil && !present:
		return set(hookMissing, "")
	case serr == nil && !managed:
		s.foreign = true
		return set(hookModified, "the file there is not magnum's (no `"+agenthooks.ScriptMarker+"` line)")
	case len(drifted) > 0:
		return set(hookModified, fmt.Sprintf("the entry for %s in %s is not magnum's command, timeout or async", strings.Join(drifted, ", "), file))
	case serr == nil && agent != t.agent:
		return set(hookModified, fmt.Sprintf("the script is made for %q, not %s", agent, t.agent))
	case serr == nil && version < agenthooks.ScriptVersion:
		return set(hookOutdated, "")
	case serr == nil && version > agenthooks.ScriptVersion:
		s.newer = true
		return set(hookModified, fmt.Sprintf("the script is a newer magnum's (v%d > v%d)", version, agenthooks.ScriptVersion))
	case serr == nil && !bytes.Equal(script, t.scriptBody()):
		return set(hookModified, fmt.Sprintf("the script differs from this build's (v%d)", agenthooks.ScriptVersion))
	case serr != nil:
		return set(hookModified, fmt.Sprintf("the entries in %s run a script that is missing, so each event's hook fails", file))
	case len(absent) > 0:
		return set(hookIncomplete, fmt.Sprintf("no entry for %s in %s", strings.Join(absent, ", "), file))
	case t.agent == agenthooks.AgentCodex:
		if why, stale := codexHooksUntrusted(t.dir, s.places); why != "" {
			s.stale = stale
			return set(hookUntrusted, why)
		}
	}
	return set(hookCurrent, "")
}

// codexHooksUntrusted says which of places (magnum's entries in the Codex
// home's hooks.json) have no trust entry in its config.toml, or only the
// one of the hook before magnum rewrote it (agents.ReadCodexHooksTrust),
// with the way out when Codex asks nothing for those; "" when all are
// trusted. anyStale says that such a one is among them.
func codexHooksUntrusted(home string, places []string) (why string, anyStale bool) {
	trust, ok, err := agents.ReadCodexHooksTrust(home)
	switch {
	case err != nil:
		return "cannot tell whether Codex trusts them: " + err.Error(), false
	case !ok:
		return "", false
	}
	var none, stale []string
	for _, p := range places {
		i := slices.IndexFunc(trust.Untrusted, func(h agents.CodexHookAt) bool { return h.Place == p })
		switch {
		case i < 0:
		case trust.Untrusted[i].Stale:
			stale = append(stale, p+" (its trust is of the hook before magnum rewrote it)")
		default:
			none = append(none, p)
		}
	}
	var parts []string
	if len(none) > 0 {
		parts = append(parts, "no trust entry for "+strings.Join(none, ", "))
	}
	if len(stale) > 0 {
		parts = append(parts, "a trust entry Codex no longer accepts for "+strings.Join(stale, ", "))
	}
	if len(parts) == 0 {
		return "", false
	}
	why = fmt.Sprintf("Codex has %s in %s", strings.Join(parts, " and "), inspTilde(trust.ConfigFile))
	if len(stale) > 0 {
		why += "; " + codexForgetStep
	}
	return why, len(stale) > 0
}

// danglingLink says that path is a symbolic link to nothing ("" when it is
// not): magnum never replaces such a link with a file, since it may stand
// for a dotfiles file to restore.
func danglingLink(path string) string {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		return ""
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	target, _ := os.Readlink(path)
	return fmt.Sprintf("%s is a link to %s, which does not exist", path, target)
}

// unwrapPathErr drops the path of a *fs.PathError, which the caller names.
func unwrapPathErr(err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err
	}
	return err
}

// codexTrustStep is what to do once after Codex's hooks change: Codex asks
// to review them when a session starts, and the daemon holds new Codex
// sessions meanwhile.
const codexTrustStep = "run `codex` once and choose Trust all (or `/hooks`), or `magnum resume --tool codex`"

// hooksFlags are the parsed `magnum hooks` flags.
type hooksFlags struct{ dry, yes, forget bool }

const hooksUsage = "[status|install [--dry-run] [--forget-stale]|uninstall [--dry-run] [--yes]] [claude|codex]"

func newHooksCmd(c *Context) *cobra.Command {
	var f hooksFlags
	cmd := newCommand(groupDaemon, "hooks [status|install|uninstall] [claude|codex]", "install magnum's agent hook into Claude's and Codex's hook settings",
		"magnum's own panes report a turn's end, a permission prompt and a session's id at once through magnum's agent hook "+
			"(hooks/magnum-agent-hook.sh), which runs `magnum hook-event` only in a pane magnum started and exits at once in "+
			"every other session. `install` writes the script (mode 0755) to ~/.claude/hooks/ ($CLAUDE_CONFIG_DIR when set) "+
			"and to each Codex home the Codex roles read, and appends one entry per event (`sh '<script>' <agent> <event>`, "+
			"async, timeout 5 s, 1 s for Claude's SessionEnd and Codex's Interrupt) to ~/.claude/settings.json (Stop, StopFailure, PermissionRequest, SessionStart, SessionEnd) "+
			"and to each Codex home's hooks.json (Stop, SessionStart, PermissionRequest, Interrupt). It only appends, at the end "+
			"of each event's list, and keeps every other byte; an entry already there is rewritten in its place only when its "+
			"command, timeout or async drifted. It refuses a file at the script's path that is not magnum's and a settings file "+
			"that is not valid JSON, and changes nothing then. Codex asks to trust new hooks once: "+codexTrustStep+". "+
			"After a rewrite of a Codex entry, its place reads untrusted while Codex keeps the trust of the hook before it; "+
			"when `codex` asks nothing, `install codex --forget-stale` forgets those records. "+
			"`uninstall` removes magnum's entries (a group with nothing else in it too) and its scripts; when that moves another "+
			"Codex hook to a new place, where Codex no longer trusts it, it names the hooks and asks y/N (--yes answers for a "+
			"script). `status` (the default) prints one line per target and exits 1 unless every target is current. With no "+
			"agent named, the targets are those the configured roles use.",
		func(pos []string) int { return runHooks(c, "status", f, pos) })
	install := newCommand("", "install [claude|codex] [--dry-run] [--forget-stale]", "write the hook script and append magnum's entries",
		"Write the hook script and append magnum's entries to the agents' hook settings; --dry-run prints each file and what "+
			"would be added or changed, and writes nothing. After a rewrite of a Codex entry, status counts its place untrusted "+
			"while Codex keeps the trust entry of the hook before the rewrite; when `codex` asks nothing (it trusts the hook as "+
			"it is), --forget-stale forgets those records for the Codex homes first.",
		func(pos []string) int { return runHooks(c, "install", f, pos) })
	install.Flags().BoolVar(&f.dry, "dry-run", false, "print the plan; change nothing")
	install.Flags().BoolVar(&f.forget, "forget-stale", false, "also forget magnum's records of Codex trust entries a rewrite left stale, for when `codex` asks nothing")
	uninstall := newCommand("", "uninstall [claude|codex] [--dry-run] [--yes]", "remove magnum's entries and the hook script",
		"Remove magnum's entries from the agents' hook settings and the hook scripts; asks y/N before it moves another Codex "+
			"hook to a new place (it loses its trust there).",
		func(pos []string) int { return runHooks(c, "uninstall", f, pos) })
	uninstall.Flags().BoolVar(&f.dry, "dry-run", false, "print the plan; change nothing")
	uninstall.Flags().BoolVar(&f.yes, "yes", false, "move other Codex hooks without the y/N question")
	status := newCommand("", "status [claude|codex]", "print each target's state; exit 1 unless every one is current",
		"Print one line per target: current, outdated (vM < vN), missing, modified, incomplete, refused (a home that is "+
			"not an absolute path) or, for Codex, untrusted, with what is wrong. Exits 1 unless every target is current.",
		func(pos []string) int { return runHooks(c, "status", f, pos) })
	cmd.AddCommand(install, uninstall, status)
	return cmd
}

// runHooks runs `magnum hooks <verb>` on the targets pos names.
func runHooks(c *Context, verb string, f hooksFlags, pos []string) int {
	which := ""
	switch {
	case len(pos) > 1:
		return inspUsage(c, "hooks", "takes at most one agent: claude or codex", hooksUsage)
	case len(pos) == 1 && pos[0] != agenthooks.AgentClaude && pos[0] != agenthooks.AgentCodex:
		return inspUsage(c, "hooks", fmt.Sprintf("unknown agent %q: claude or codex", pos[0]), hooksUsage)
	case len(pos) == 1:
		which = pos[0]
	}
	cmdName := "hooks " + verb
	home, err := hooksSys.UserHome()
	if err != nil {
		return cmdFail(c, cmdName, fmt.Errorf("find your home directory: %w", err))
	}
	if err := c.LoadConfig(); err != nil {
		return cmdFail(c, cmdName, fmt.Errorf("%w (fix config.toml; `magnum config` validates it)", err))
	}
	defCodex := codexDefaultHome(hooksSys.Getenv, home)
	mgr := agents.New(agents.Deps{Config: c.Config, Layout: c.Layout, CodexConfig: filepath.Join(defCodex, "config.toml")})
	targets := hookTargets(c.Config, claudeHookDir(hooksSys.Getenv, home), mgr.CodexHome, defCodex, which)
	if len(targets) == 0 {
		fmt.Fprintln(c.Stdout, "no role runs Claude or Codex: there is nothing to hook")
		return 0
	}
	ctx, stop := signalContext()
	defer stop()
	switch verb {
	case "install":
		return hooksInstall(c, targets, f)
	case "uninstall":
		return hooksUninstall(ctx, c, targets, f)
	}
	code := 0
	for _, t := range targets {
		s := readHookStatus(t)
		fmt.Fprintln(c.Stdout, s.line())
		if s.state != hookCurrent {
			code = 1
		}
	}
	return code
}

// hookWrite is one file the install or uninstall writes: data at path (its
// symlink resolved, so a link to a dotfiles checkout stays one) with mode;
// data nil removes it.
type hookWrite struct {
	path string
	data []byte
	mode fs.FileMode
}

// apply writes (or removes) the file, making its directory.
func (w hookWrite) apply() error {
	if w.data == nil {
		if err := os.Remove(w.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(w.path), 0o700); err != nil {
		return err
	}
	return fsx.WriteFileAtomic(w.path, w.data, w.mode)
}

// resolvedPath is path with its symlinks resolved, when it exists.
func resolvedPath(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	return path
}

// hookInstall is the install's plan for one target.
type hookInstall struct {
	t        hookTarget
	script   *hookWrite // nil: the script is this build's
	settings *hookWrite // nil: every entry is in place
	before   []byte     // the settings the plan read (nil: none)
	changes  []hookChange
}

// hookSettingsChanged says that t's settings file is no longer the before a
// plan read ("" when it is the same): another tool wrote it meanwhile, and
// writing the plan would undo that write.
func hookSettingsChanged(t hookTarget, before []byte, when string) string {
	now, err := os.ReadFile(t.settings())
	switch {
	case errors.Is(err, fs.ErrNotExist) && before == nil:
		return ""
	case err == nil && before != nil && bytes.Equal(now, before):
		return ""
	}
	return fmt.Sprintf("%s: %s changed %s; nothing changed\nfix: run the command again", t.label(), t.settings(), when)
}

// planHookInstall plans t's install: the script, unless it is this build's
// at mode 0755, and the settings, unless every entry is in place. A file at
// the script's path that is not magnum's, and settings that cannot be read
// or are not valid JSON, are refused (the error says what and the fix).
func planHookInstall(t hookTarget) (hookInstall, error) {
	p := hookInstall{t: t}
	if why := hookHomeProblem(t); why != "" {
		return p, fmt.Errorf("%s: %s; nothing changed\nfix: %s", t.label(), why, hookHomeFix(t))
	}
	if link := danglingLink(t.script()); link != "" {
		return p, fmt.Errorf("%s: %s; nothing changed\nfix: restore the link's target or remove the link, then re-run magnum hooks install %s", t.label(), link, t.agent)
	}
	want := t.scriptBody()
	switch old, err := os.ReadFile(t.script()); {
	case errors.Is(err, fs.ErrNotExist):
		p.script = &hookWrite{path: t.script(), data: want, mode: 0o755}
	case err != nil:
		return p, fmt.Errorf("%s: cannot read %s: %w", t.label(), t.script(), unwrapPathErr(err))
	default:
		_, version, managed := agenthooks.ScriptHeader(old)
		switch {
		case !managed:
			return p, fmt.Errorf("%s: %s exists and is not magnum's hook script (it has no `%s` line); nothing changed\nfix: move it away, then re-run magnum hooks install %s",
				t.label(), t.script(), agenthooks.ScriptMarker, t.agent)
		case version > agenthooks.ScriptVersion:
			return p, fmt.Errorf("%s: %s is a newer magnum's (v%d > v%d); nothing changed\nfix: run `magnum hooks install` of that newer magnum",
				t.label(), t.script(), version, agenthooks.ScriptVersion)
		}
		if fi, err := os.Stat(t.script()); !bytes.Equal(old, want) || err != nil || fi.Mode().Perm() != 0o755 {
			p.script = &hookWrite{path: resolvedPath(t.script()), data: want, mode: 0o755}
		}
	}
	doc, mode, err := readSettings(t)
	if err != nil {
		return p, err
	}
	p.before = doc
	out, changes, err := spliceHooks(doc, t.agent, t.entries())
	if err != nil {
		return p, fmt.Errorf("%s: %s %w; nothing changed\nfix: fix the file (%s reads it at every start), then re-run magnum hooks install %s",
			t.label(), t.settings(), err, agentName(t.agent), t.agent)
	}
	if len(changes) > 0 {
		p.settings, p.changes = &hookWrite{path: resolvedPath(t.settings()), data: out, mode: mode}, changes
	}
	return p, nil
}

// hookHomeEnv is the env variable that names agent's home.
func hookHomeEnv(agent string) string {
	if agent == agenthooks.AgentClaude {
		return "CLAUDE_CONFIG_DIR"
	}
	return "CODEX_HOME"
}

// readSettings reads t's settings file (nil when there is none) and its
// mode (0600 for one to make). A link to nothing is refused.
func readSettings(t hookTarget) ([]byte, fs.FileMode, error) {
	if link := danglingLink(t.settings()); link != "" {
		return nil, 0, fmt.Errorf("%s: %s; nothing changed\nfix: restore the link's target or remove the link", t.label(), link)
	}
	doc, err := os.ReadFile(t.settings())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, 0o600, nil
	case err != nil:
		return nil, 0, fmt.Errorf("%s: cannot read %s: %w", t.label(), t.settings(), unwrapPathErr(err))
	}
	mode := fs.FileMode(0o600)
	if fi, err := os.Stat(t.settings()); err == nil {
		mode = fi.Mode().Perm()
	}
	return doc, mode, nil
}

// agentName is the agent's product name.
func agentName(agent string) string {
	if agent == agenthooks.AgentClaude {
		return "Claude Code"
	}
	return "Codex"
}

// hooksInstall installs every target, or none: a refusal of one changes
// nothing anywhere.
func hooksInstall(c *Context, targets []hookTarget, f hooksFlags) int {
	dry := f.dry
	plans := make([]hookInstall, 0, len(targets))
	var refused []error
	for _, t := range targets {
		p, err := planHookInstall(t)
		if err != nil {
			refused = append(refused, err)
			continue
		}
		plans = append(plans, p)
	}
	if len(refused) > 0 {
		for _, err := range refused {
			fmt.Fprintf(c.Stderr, "magnum hooks install: %v\n", err)
		}
		return 1
	}
	out := c.Stdout
	if hooksBeforeWrite != nil && !dry {
		hooksBeforeWrite()
	}
	if !dry {
		var changed []string
		for _, p := range plans {
			if p.settings == nil {
				continue
			}
			if why := hookSettingsChanged(p.t, p.before, "since magnum read it"); why != "" {
				changed = append(changed, why)
			}
		}
		if len(changed) > 0 {
			for _, why := range changed {
				fmt.Fprintf(c.Stderr, "magnum hooks install: %s\n", why)
			}
			return 1
		}
	}
	code := 0
	for _, p := range plans {
		label := p.t.label()
		if f.forget && p.t.agent == agenthooks.AgentCodex {
			if err := forgetStaleTrust(out, p.t, dry); err != nil {
				fmt.Fprintf(c.Stderr, "magnum hooks install: %s: %v\n", label, err)
				code = 1
				continue
			}
		}
		switch {
		case p.script == nil && p.settings == nil:
			fmt.Fprintf(out, "%s: nothing to change in %s\n", label, inspTilde(p.t.dir))
		case dry:
			hooksPrintInstallPlan(out, p)
			continue
		}
		if p.script != nil {
			if err := p.script.apply(); err != nil {
				fmt.Fprintf(c.Stderr, "magnum hooks install: %s: write %s: %v\n", label, p.script.path, err)
				code = 1
				continue
			}
			fmt.Fprintf(out, "%s: wrote %s (v%d, mode 0755)\n", label, inspTilde(p.script.path), agenthooks.ScriptVersion)
		}
		if p.settings != nil {
			if err := recordStaleTrust(p); err != nil {
				fmt.Fprintf(c.Stderr, "magnum hooks install: %s: %v; %s left as it was\n", label, err, p.t.settings())
				code = 1
				continue
			}
			if err := p.settings.apply(); err != nil {
				fmt.Fprintf(c.Stderr, "magnum hooks install: %s: write %s: %v\n", label, p.settings.path, err)
				code = 1
				continue
			}
			added, rewrote := hookChangeNames(p.changes)
			if len(added) > 0 {
				fmt.Fprintf(out, "%s: %s: added %s\n", label, inspTilde(p.t.settings()), strings.Join(added, ", "))
			}
			if len(rewrote) > 0 {
				fmt.Fprintf(out, "%s: %s: rewrote %s in place (its command, timeout or async had drifted)", label, inspTilde(p.t.settings()), strings.Join(rewrote, ", "))
				if p.t.agent == agenthooks.AgentCodex && slices.ContainsFunc(p.changes, func(ch hookChange) bool { return ch.rewrote && ch.newTrust }) {
					fmt.Fprint(out, "; Codex will ask for trust again")
				}
				fmt.Fprintln(out)
			}
		}
		s := readHookStatus(p.t)
		fmt.Fprintln(out, s.line())
		switch {
		case s.state == hookUntrusted:
			fmt.Fprintf(out, "  Codex asks you once to trust magnum's new hooks in %s, and until then the daemon holds new Codex sessions: %s\n",
				inspTilde(p.t.settings()), codexTrustStep)
		case p.t.agent == agenthooks.AgentClaude && s.state == hookCurrent:
			fmt.Fprintln(out, "  live Claude sessions pick the hooks up; outside magnum's panes the script exits at once")
		}
	}
	return code
}

// recordStaleTrust records, before p rewrites magnum's Codex entries in
// their places, the trust entries Codex keeps there for the hooks before
// the rewrite (agents.RecordStaleCodexTrust): status, doctor and the
// daemon's hooks hold then count those places untrusted until Codex trusts
// the new hooks. Claude has no trust.
func recordStaleTrust(p hookInstall) error {
	if p.t.agent != agenthooks.AgentCodex {
		return nil
	}
	var places []string
	for _, ch := range p.changes {
		if ch.rewrote && ch.newTrust {
			places = append(places, ch.place)
		}
	}
	if len(places) == 0 {
		return nil
	}
	return agents.RecordStaleCodexTrust(p.t.dir, places)
}

// forgetStaleTrust forgets magnum's stale trust records of the Codex home
// t (agents.ForgetStaleCodexTrust), or says how many it would with dry.
func forgetStaleTrust(out io.Writer, t hookTarget, dry bool) error {
	file := inspTilde(t.settings())
	if dry {
		fmt.Fprintf(out, "dry-run: %s: would forget %s of %s\n", t.label(), textx.Count(agents.StaleCodexTrustCount(t.dir), "stale trust record", "stale trust records"), file)
		return nil
	}
	n, err := agents.ForgetStaleCodexTrust(t.dir)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s: forgot %s of %s\n", t.label(), textx.Count(n, "stale trust record", "stale trust records"), file)
	return nil
}

// hookChangeNames are the events changes added and rewrote.
func hookChangeNames(changes []hookChange) (added, rewrote []string) {
	for _, ch := range changes {
		if ch.rewrote {
			rewrote = append(rewrote, ch.event)
		} else {
			added = append(added, ch.event)
		}
	}
	return added, rewrote
}

// hooksPrintInstallPlan prints what the install of p would write.
func hooksPrintInstallPlan(out io.Writer, p hookInstall) {
	label := p.t.label()
	if p.script != nil {
		fmt.Fprintf(out, "dry-run: %s: would write %s (v%d, mode 0755)\n", label, inspTilde(p.script.path), agenthooks.ScriptVersion)
	}
	if p.settings == nil {
		return
	}
	if _, err := os.Stat(p.t.settings()); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(out, "dry-run: %s: would write %s (a new file)\n", label, inspTilde(p.t.settings()))
	}
	added, rewrote := hookChangeNames(p.changes)
	if len(added) > 0 {
		fmt.Fprintf(out, "dry-run: %s: would add to %s: %s\n", label, inspTilde(p.t.settings()), strings.Join(added, ", "))
	}
	if len(rewrote) > 0 {
		fmt.Fprintf(out, "dry-run: %s: would rewrite in %s: %s", label, inspTilde(p.t.settings()), strings.Join(rewrote, ", "))
		if p.t.agent == agenthooks.AgentCodex && slices.ContainsFunc(p.changes, func(ch hookChange) bool { return ch.rewrote && ch.newTrust }) {
			fmt.Fprint(out, " (Codex will ask for trust again)")
		}
		fmt.Fprintln(out)
	}
	for _, e := range p.t.entries() {
		if slices.ContainsFunc(p.changes, func(ch hookChange) bool { return ch.event == e.Event }) {
			fmt.Fprintf(out, "  %s: %s (async, timeout %d s)\n", e.Event, e.Command, e.Timeout)
		}
	}
	if p.t.agent == agenthooks.AgentCodex {
		fmt.Fprintf(out, "  then Codex asks you once to trust the new hooks: %s\n", codexTrustStep)
	}
}

// hookRemoval is the uninstall's plan for one target.
type hookRemoval struct {
	t        hookTarget
	before   []byte     // the settings the plan read (nil: none)
	settings *hookWrite // nil: no entry of magnum's
	removed  int
	shifts   []hookShift // Codex: the hooks the removal moves
	script   *hookWrite  // nil: no script of magnum's
	foreign  bool        // a file at the script's path that is not magnum's, left alone
}

// planHookRemoval plans t's uninstall; settings that cannot be read or are
// not valid JSON are refused.
func planHookRemoval(t hookTarget) (hookRemoval, error) {
	p := hookRemoval{t: t}
	if why := hookHomeProblem(t); why != "" {
		return p, fmt.Errorf("%s: %s; nothing changed\nfix: %s", t.label(), why, hookHomeFix(t))
	}
	doc, mode, err := readSettings(t)
	if err != nil {
		return p, err
	}
	p.before = doc
	if doc != nil {
		out, removed, err := unspliceHooks(doc, t.agent)
		if err != nil {
			return p, fmt.Errorf("%s: %s %w; nothing changed\nfix: fix the file (%s reads it at every start), then re-run magnum hooks uninstall %s",
				t.label(), t.settings(), err, agentName(t.agent), t.agent)
		}
		if removed > 0 {
			p.settings, p.removed = &hookWrite{path: resolvedPath(t.settings()), data: out, mode: mode}, removed
			if t.agent == agenthooks.AgentCodex {
				if p.shifts, err = hookShifts(doc, out); err != nil {
					return p, fmt.Errorf("%s: %s: %w", t.label(), t.settings(), err)
				}
			}
		}
	}
	switch b, err := os.ReadFile(t.script()); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return p, fmt.Errorf("%s: cannot read %s: %w", t.label(), t.script(), unwrapPathErr(err))
	default:
		if _, _, managed := agenthooks.ScriptHeader(b); managed {
			p.script = &hookWrite{path: t.script()}
		} else {
			p.foreign = true
		}
	}
	return p, nil
}

// hooksUninstall removes magnum's hooks from every target, or from none: a
// refusal, or an n to the question about the Codex hooks it would move,
// changes nothing anywhere.
func hooksUninstall(ctx context.Context, c *Context, targets []hookTarget, f hooksFlags) int {
	plans := make([]hookRemoval, 0, len(targets))
	var refused []error
	var shifted []string
	for _, t := range targets {
		p, err := planHookRemoval(t)
		if err != nil {
			refused = append(refused, err)
			continue
		}
		plans = append(plans, p)
		if len(p.shifts) > 0 {
			names := make([]string, len(p.shifts))
			for i, s := range p.shifts {
				names[i] = s.String()
			}
			shifted = append(shifted, fmt.Sprintf("in %s, %s", inspTilde(t.settings()), strings.Join(names, ", ")))
		}
	}
	if len(refused) > 0 {
		for _, err := range refused {
			fmt.Fprintf(c.Stderr, "magnum hooks uninstall: %v\n", err)
		}
		return 1
	}
	out := c.Stdout
	asked := false
	if len(shifted) > 0 {
		moves := fmt.Sprintf("magnum's Codex hooks are not last, so removing them moves your other hooks to a new place, where Codex no longer trusts them "+
			"and asks you to review them at the next session's start: %s", strings.Join(shifted, "; "))
		switch {
		case f.dry:
			fmt.Fprintf(out, "dry-run: %s\n", moves)
		case f.yes:
			fmt.Fprintln(out, moves)
		case !inspCanAsk(c):
			fmt.Fprintf(c.Stderr, "magnum hooks uninstall: %s; nothing changed\nfix: run it on a terminal to answer y/N, or pass --yes\n", moves)
			return 1
		default:
			fmt.Fprintln(c.Stderr, moves)
			asked = true
			if !inspConfirm(ctx, c, "Remove them?") {
				fmt.Fprintln(c.Stderr, "nothing changed")
				if ctx.Err() != nil {
					return 130
				}
				return 1
			}
		}
	}
	if !f.dry {
		when := "since magnum read it"
		if asked {
			when = "while magnum asked"
		}
		var changed []string
		for _, p := range plans {
			if p.settings == nil {
				continue
			}
			if why := hookSettingsChanged(p.t, p.before, when); why != "" {
				changed = append(changed, why)
			}
		}
		if len(changed) > 0 {
			for _, why := range changed {
				fmt.Fprintf(c.Stderr, "magnum hooks uninstall: %s\n", why)
			}
			return 1
		}
	}
	code := 0
	for _, p := range plans {
		label := p.t.label()
		if p.settings == nil && p.script == nil {
			fmt.Fprintf(out, "%s: no hook of magnum's in %s\n", label, inspTilde(p.t.dir))
		}
		if p.settings != nil {
			if f.dry {
				fmt.Fprintf(out, "dry-run: %s: would remove magnum's %s from %s\n", label, textx.Count(p.removed, "entry", "entries"), inspTilde(p.t.settings()))
			} else if err := p.settings.apply(); err != nil {
				fmt.Fprintf(c.Stderr, "magnum hooks uninstall: %s: write %s: %v\n", label, p.settings.path, err)
				code = 1
				continue
			} else {
				fmt.Fprintf(out, "%s: %s: removed magnum's %s\n", label, inspTilde(p.t.settings()), textx.Count(p.removed, "entry", "entries"))
			}
		}
		if p.script != nil {
			if f.dry {
				fmt.Fprintf(out, "dry-run: %s: would remove %s\n", label, inspTilde(p.t.script()))
			} else if err := p.script.apply(); err != nil {
				fmt.Fprintf(c.Stderr, "magnum hooks uninstall: %s: remove %s: %v\n", label, p.t.script(), err)
				code = 1
			} else {
				fmt.Fprintf(out, "%s: removed %s\n", label, inspTilde(p.t.script()))
			}
		}
		if p.foreign {
			fmt.Fprintf(out, "%s: %s is not magnum's; left alone\n", label, inspTilde(p.t.script()))
		}
		if len(p.shifts) > 0 && !f.dry {
			fmt.Fprintf(out, "  Codex asks you to trust the hooks that moved: %s\n", codexTrustStep)
		}
	}
	return code
}
