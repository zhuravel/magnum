// Package paths defines magnum's on-disk layout. An installed magnum keeps
// its files where XDG says: the user config in ~/.config/magnum, the
// registry, review reports and notes in ~/.local/share/magnum, logs, locks
// and the identities' gh config in ~/.local/state/magnum. A checkout layout
// (everything under <checkout>/state, gitignored) remains for development
// (MAGNUM_HOME).
package paths

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/zhuravel/magnum/internal/fsx"
)

// Layout resolves every path magnum reads or writes.
type Layout struct {
	// Home is the repository checkout magnum runs from (MAGNUM_HOME, or the
	// checkout holding the binary); "" for an installed binary. In the
	// checkout layout (DataDir and StateDir empty) everything lives under
	// Home/state.
	Home string
	// DataDir holds the registry, the review reports and the repository
	// notes ($XDG_DATA_HOME/magnum); StateDir the logs, locks, pidfile, the
	// identities' gh config, the tab-bar file and the skill copies
	// ($XDG_STATE_HOME/magnum). Both empty: the checkout layout.
	DataDir, StateDir string
	// Exe is the running binary as it was invoked ("" when unknown): what
	// launchd runs when no checkout binary exists (Binary).
	Exe string
	// UserConfig is the user's config file, layered over the built-in
	// defaults: $XDG_CONFIG_HOME/magnum/config.toml, else
	// ~/.config/magnum/config.toml (Resolve sets it). ""
	// means none (tests build layouts without it).
	UserConfig string
	// Scratch, when set, holds the registry, the review reports and the
	// repository notes instead of state/ (magnum eval: a replay never touches
	// the live registry, reports or notes). Logs, locks, the pidfile and the
	// identities' gh config stay under state/.
	Scratch string
}

// modulePath is the module line of magnum's own go.mod: the only go.mod that
// marks the repository root.
const modulePath = "github.com/zhuravel/magnum"

// Resolve picks the layout:
//
//   - MAGNUM_HOME set: the checkout layout under it (development, tests).
//     It is expanded (~), made absolute and symlink-free (see
//     canonicalDir): magnum hands paths under it (GH_CONFIG_DIR, the report
//     directory) to agents whose working directory is a PR checkout, where
//     a relative or symlinked path would mean something else.
//   - Else the XDG layout, with Home the checkout holding the binary, if any
//     (it has config.toml, config.defaults.toml or magnum's own go.mod
//     within four levels up, symlinks resolved).
func Resolve() (Layout, error) {
	exe, _ := os.Executable()
	home, _ := os.UserHomeDir()
	return resolve(os.Getenv, exe, home)
}

func resolve(getenv func(string) string, exe, userHome string) (Layout, error) {
	user := userConfigPath(getenv, userHome)
	if h := getenv("MAGNUM_HOME"); h != "" {
		dir, err := canonicalDir(expandFrom(h, userHome))
		if err != nil {
			return Layout{}, fmt.Errorf("paths: MAGNUM_HOME %q: %w", h, err)
		}
		return Layout{Home: dir, UserConfig: user, Exe: exe}, nil
	}
	checkout := ""
	if exe != "" {
		checkout, _ = homeFromExecutable(exe)
	}
	dataHome, stateHome := xdgDir(getenv, userHome, "XDG_DATA_HOME", ".local/share"), xdgDir(getenv, userHome, "XDG_STATE_HOME", ".local/state")
	if dataHome == "" || stateHome == "" {
		return Layout{}, errors.New("paths: no home directory to put magnum's data in")
	}
	return Layout{Home: checkout, UserConfig: user, Exe: exe,
		DataDir: filepath.Join(dataHome, "magnum"), StateDir: filepath.Join(stateHome, "magnum")}, nil
}

// xdgDir is $<env> when it is an absolute path, else ~/<fallback>; "" without
// a home directory.
func xdgDir(getenv func(string) string, userHome, env, fallback string) string {
	if x := getenv(env); filepath.IsAbs(x) {
		return x
	}
	if userHome == "" {
		return ""
	}
	return filepath.Join(userHome, fallback)
}

// CheckoutLayout reports whether everything lives under Home/state (the
// development layout, MAGNUM_HOME).
func (l Layout) CheckoutLayout() bool { return l.StateDir == "" }

// Valid reports whether the layout names where magnum's files live (a
// zero Layout, as tests build, does not).
func (l Layout) Valid() bool { return l.Home != "" || l.StateDir != "" }

func userConfigPath(getenv func(string) string, userHome string) string {
	if dir := xdgDir(getenv, userHome, "XDG_CONFIG_HOME", ".config"); dir != "" {
		return filepath.Join(dir, "magnum", "config.toml")
	}
	return ""
}

// ConfigDir is the user config's directory (~/.config/magnum): eval.toml,
// App keys, prompt overrides; "" without one.
func (l Layout) ConfigDir() string {
	if l.UserConfig == "" {
		return ""
	}
	return filepath.Dir(l.UserConfig)
}

// canonicalDir returns p as an absolute path with every symlink resolved. The
// directory need not exist yet (a fresh checkout location): the longest
// existing ancestor is resolved and the missing tail is appended unchanged.
// Any other failure (permissions, a symlink loop, a dangling symlink in the
// path) is returned, so a MAGNUM_HOME that cannot be pinned down is an error
// instead of a silently different location.
func canonicalDir(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	tail := ""
	for cur := abs; ; {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(resolved, tail), nil
		}
		if _, lerr := os.Lstat(cur); lerr == nil || !errors.Is(err, fs.ErrNotExist) {
			return "", err // exists but cannot be resolved (dangling link, loop, permissions)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

// homeFromExecutable resolves exe's symlinks (a binary invoked through
// ~/bin/magnum lives in the checkout) and looks for the repository root above
// it.
func homeFromExecutable(exe string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", false
	}
	return findHome(filepath.Dir(resolved))
}

// findHome walks up from dir, at most four levels (dir itself, then three
// parents), and returns the first directory that holds config.toml or magnum's
// own go.mod. A go.mod of any other module is skipped: a binary placed inside
// another Go project must not make that project magnum's home.
func findHome(dir string) (string, bool) {
	for range 4 {
		if fsx.Exists(filepath.Join(dir, "config.toml")) || fsx.Exists(filepath.Join(dir, "config.defaults.toml")) ||
			isMagnumModule(filepath.Join(dir, "go.mod")) {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}

// isMagnumModule reports whether the go.mod at path declares
// `module github.com/zhuravel/magnum`.
func isMagnumModule(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "//")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "module" && len(fields) == 2 {
			return strings.Trim(fields[1], "\"`") == modulePath
		}
	}
	return false
}

// Expand replaces a leading ~ with the user's home directory.
func Expand(p string) string {
	home, _ := os.UserHomeDir()
	return expandFrom(p, home)
}

func expandFrom(p, userHome string) string {
	if (p == "~" || strings.HasPrefix(p, "~/")) && userHome != "" {
		return filepath.Join(userHome, strings.TrimPrefix(p, "~"))
	}
	return p
}

// Config is the checkout's legacy full config (Home/config.toml); "" without
// a checkout.
func (l Layout) Config() string { return l.inHome("config.toml") }

// State is where logs, locks, the pidfile and the gh config dirs live:
// StateDir, else Home/state.
func (l Layout) State() string {
	if l.StateDir != "" {
		return l.StateDir
	}
	return filepath.Join(l.Home, "state")
}

// DB is the SQLite registry, magnum.db in the data directory.
func (l Layout) DB() string { return filepath.Join(l.data(), "magnum.db") }

// Lock is the daemon's lock file under State, held for the daemon's whole
// life; a CLI command takes it too before in-process slot or cleanup work.
func (l Layout) Lock() string { return filepath.Join(l.State(), "magnum.lock") }

// OpsLock is the lock a CLI command takes before Lock for in-process work,
// so a daemon that starts meanwhile can tell the command from a daemon.
func (l Layout) OpsLock() string { return filepath.Join(l.State(), "ops.lock") }

// Pid is the daemon's pidfile under State.
func (l Layout) Pid() string { return filepath.Join(l.State(), "daemon.pid") }

// Logs is the log directory under State.
func (l Layout) Logs() string { return filepath.Join(l.State(), "logs") }

// DaemonLog is the daemon's log file in Logs.
func (l Layout) DaemonLog() string { return filepath.Join(l.Logs(), "daemon.log") }

// factoryDir is where the factory daemon keeps its registries and its task
// directories, in the data directory.
func (l Layout) factoryDir() string { return filepath.Join(l.data(), "factory") }

// FactoryDB is the factory's own registry (factory.db in the factory
// directory of the data directory), separate from the review registry.
func (l Layout) FactoryDB() string { return filepath.Join(l.factoryDir(), "factory.db") }

// FactoryAgentsDB is the registry of the factory's agent sessions
// (agents.db), beside FactoryDB.
func (l Layout) FactoryAgentsDB() string { return filepath.Join(l.factoryDir(), "agents.db") }

// FactoryBaseline is baseline.json, the counts of the operator's messages per
// ticket before the factory, frozen once before its first task (`magnum factory
// baseline`), beside FactoryDB.
func (l Layout) FactoryBaseline() string { return filepath.Join(l.factoryDir(), "baseline.json") }

// FactorySeen is seen.json, when you last saw the factory screen's home
// (the digest of what finished while you were away counts from it), beside
// FactoryDB.
func (l Layout) FactorySeen() string { return filepath.Join(l.factoryDir(), "seen.json") }

// FactoryFocus is focus.json, the factory screen's latest focus report as
// the factory daemon keeps it (its toasts hold while a screen that runs
// has focus), beside FactoryDB.
func (l Layout) FactoryFocus() string { return filepath.Join(l.factoryDir(), "focus.json") }

// FactoryAlerts is alerts.json, how far the factory daemon delivered its
// alerts (the newest alert whose toast went out or was dropped, with every
// older one), so a restart neither repeats one nor loses one, beside
// FactoryDB.
func (l Layout) FactoryAlerts() string { return filepath.Join(l.factoryDir(), "alerts.json") }

// FactoryHolds is holds.json, the holds of the machine in force as the
// factory daemon last saw them (the screen draws them), beside FactoryDB.
func (l Layout) FactoryHolds() string { return filepath.Join(l.factoryDir(), "holds.json") }

// FactoryStaging is staging.json, the staging cells of the published
// tasks' PRs as the factory daemon last read them (the screen draws them),
// beside FactoryDB.
func (l Layout) FactoryStaging() string { return filepath.Join(l.factoryDir(), "staging.json") }

// FactoryWork is work.json, the work the factory daemon runs that a restart
// would cut and the drain it holds, as the daemon last wrote them (`magnum
// factory install` reads it first), beside FactoryDB.
func (l Layout) FactoryWork() string { return filepath.Join(l.factoryDir(), "work.json") }

// FactoryDrain is drain.json, the drain `magnum factory install --drain`
// asks of the factory daemon (when it started and the command's pid),
// beside FactoryDB.
func (l Layout) FactoryDrain() string { return filepath.Join(l.factoryDir(), "drain.json") }

// FactoryPause is pause.json, the pause `magnum pause` asks of the factory
// daemon (since when, until when, why), beside FactoryDrain.
func (l Layout) FactoryPause() string { return filepath.Join(l.factoryDir(), "pause.json") }

// FactoryRebootDrain is reboot-drain.json, the drain for a reboot `magnum
// drain` asks of the factory daemon (engine.RebootDrain), beside
// FactoryDrain.
func (l Layout) FactoryRebootDrain() string {
	return filepath.Join(l.factoryDir(), "reboot-drain.json")
}

// FactoryTasks is the root of the factory's task directories.
func (l Layout) FactoryTasks() string { return filepath.Join(l.factoryDir(), "tasks") }

// FactoryTaskDir is one task's directory in FactoryTasks, named by its id.
func (l Layout) FactoryTaskDir(id int64) string {
	return filepath.Join(l.FactoryTasks(), strconv.FormatInt(id, 10))
}

// FactoryPRStatus is the root of the status files the factory daemon writes
// for the PRs its tasks published, pr-status/<owner>/<repo>/<number>.json
// (internal/prstatus names one and holds its contract), beside FactoryDB.
func (l Layout) FactoryPRStatus() string { return filepath.Join(l.factoryDir(), "pr-status") }

// FactoryPid is the factory daemon's pidfile under State, beside the review
// daemon's: the two daemons can run at once.
func (l Layout) FactoryPid() string { return filepath.Join(l.State(), "factory.pid") }

// FactoryLock is the factory daemon's lock file under State, held for the
// daemon's whole life.
func (l Layout) FactoryLock() string { return filepath.Join(l.State(), "factory.lock") }

// FactorySpecsLock is the lock file under State that a factory precheck or
// proof holds (flock) while it runs specs: one at a time on the machine.
func (l Layout) FactorySpecsLock() string { return filepath.Join(l.State(), "factory-specs.lock") }

// FactoryQALock is the lock file under State that a factory QA run holds
// (flock) while it runs: one at a time on the machine.
func (l Layout) FactoryQALock() string { return filepath.Join(l.State(), "factory-qa.lock") }

// FactoryShims is the directory under State that a factory copy's server
// finds first on its PATH: it holds the factory's `open`, which opens
// nothing.
func (l Layout) FactoryShims() string { return filepath.Join(l.State(), "factory-shims") }

// FactoryLog is the factory daemon's log file in Logs.
func (l Layout) FactoryLog() string { return filepath.Join(l.Logs(), "factory.log") }

// HookSocket is the review daemon's hook socket (run/hooks.sock under
// State): the agents' hooks in the panes it starts send their events there
// (internal/agenthooks). The path is the same across restarts, so a pane
// started before a daemon-restart reaches the daemon after it.
func (l Layout) HookSocket() string { return filepath.Join(l.State(), "run", "hooks.sock") }

// FactoryHookSocket is the factory daemon's hook socket
// (run/factory-hooks.sock under State), HookSocket's twin.
func (l Layout) FactoryHookSocket() string {
	return filepath.Join(l.State(), "run", "factory-hooks.sock")
}

// SocketPathLimit is the length at which a Unix socket's path is unusable:
// macOS's sun_path holds 104 bytes, its terminating NUL included.
const SocketPathLimit = 104

// CheckSocketPath reports a path that cannot name a Unix socket: empty, or
// SocketPathLimit bytes or more (a deep MAGNUM_HOME, a long XDG_STATE_HOME).
// A daemon whose hook socket is unusable runs without hooks and polls.
func CheckSocketPath(p string) error {
	switch {
	case p == "":
		return errors.New("paths: no socket path")
	case len(p) >= SocketPathLimit:
		return fmt.Errorf("paths: socket path %s is %d bytes; a Unix socket's must be under %d", p, len(p), SocketPathLimit)
	}
	return nil
}

// Reviews is the root of the review reports in the data directory.
func (l Layout) Reviews() string { return filepath.Join(l.data(), "reviews") }

// Notes is the root of the repository notes in the data directory.
func (l Layout) Notes() string { return filepath.Join(l.data(), "notes") }

// GhRoot holds the identities' gh config directories under State.
func (l Layout) GhRoot() string { return filepath.Join(l.State(), "gh") }

// GhConfigDir is identity id's gh config directory in GhRoot.
func (l Layout) GhConfigDir(id string) string { return filepath.Join(l.GhRoot(), id) }

// TabBar is the herdr tab-bar status file the daemon writes under State.
func (l Layout) TabBar() string { return filepath.Join(l.State(), "tabbar.txt") }

// DBLock is the lock file `magnum db-lock` takes for the databases of
// checkout (an absolute path): db-locks/<base>-<hash>.lock under State,
// the hash of the cleaned path, so every spelling of one checkout shares
// the file and no two checkouts do.
func (l Layout) DBLock(checkout string) string {
	clean := filepath.Clean(checkout)
	sum := sha256.Sum256([]byte(clean))
	return filepath.Join(l.State(), "db-locks", filepath.Base(clean)+"-"+hex.EncodeToString(sum[:8])+".lock")
}

// PushLock is the lock file `magnum push` takes for a push to branch of
// repo (owner/name): push-locks/<owner>-<name>-<hash>.lock under State, the
// hash of the repository (case aside) and the branch, so one repository and
// branch share the file whichever daemon's fixer App pushes through
// `magnum push`, and no two do. `magnum push` alone takes it: a gh fixer's
// plain `git push` and the engine's own merge pushes do not.
func (l Layout) PushLock(repo, branch string) string {
	repo = strings.ToLower(repo)
	sum := sha256.Sum256([]byte(repo + "\x00" + branch))
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '-'
	}, repo)
	return filepath.Join(l.State(), "push-locks", name+"-"+hex.EncodeToString(sum[:8])+".lock")
}

// Skill is the checkout's judge skill; "" without a checkout (the binary's
// embedded copy is used).
func (l Layout) Skill() string { return l.inHome("skills", "magnum-review", "SKILL.md") }

// Plugin is the checkout's herdr plugin manifest; "" without a checkout.
func (l Layout) Plugin() string { return l.inHome("herdr-plugin.toml") }

// Binary is what launchd runs: the checkout's bin/magnum when there is one,
// else the running binary (a Homebrew install's stable bin/ link rather
// than its versioned Cellar path, which an upgrade removes).
func (l Layout) Binary() string {
	if b := l.inHome("bin", "magnum"); b != "" && (fsx.Exists(b) || l.Exe == "") {
		return b
	}
	return stableExe(l.Exe)
}

// stableExe maps a Homebrew Cellar path (<prefix>/Cellar/magnum/<v>/bin/magnum)
// to <prefix>/bin/magnum, which survives upgrades.
func stableExe(exe string) string {
	if prefix, rest, ok := strings.Cut(exe, "/Cellar/"); ok {
		if parts := strings.Split(rest, "/"); len(parts) >= 3 {
			return filepath.Join(prefix, "bin", parts[len(parts)-1])
		}
	}
	return exe
}

func (l Layout) inHome(elem ...string) string {
	if l.Home == "" {
		return ""
	}
	return filepath.Join(append([]string{l.Home}, elem...)...)
}

// data is where the registry, reports and notes live: Scratch when set, else
// DataDir, else Home/state.
func (l Layout) data() string {
	switch {
	case l.Scratch != "":
		return l.Scratch
	case l.DataDir != "":
		return l.DataDir
	}
	return l.State()
}

// Data is where the registry, reports and notes live (see data).
func (l Layout) Data() string { return l.data() }

// Learn is where the learning loop keeps its inputs and outputs: the
// retro's runs live under Learn()/retro/<run>/.
func (l Layout) Learn() string { return filepath.Join(l.data(), "learn") }

// LearnCorpus is the learned eval corpus the daemon writes from the
// registry's misses and declines, which `magnum eval` reads after the
// operator's corpus.
func (l Layout) LearnCorpus() string { return filepath.Join(l.Learn(), "corpus.toml") }

// LearnLocalCases is where the eval corpus's local cases keep their frozen
// inputs, one directory per case, LearnLocalCases()/<case name>
// (`magnum eval add --local-task`, evalrun.LocalCaseDir): beside the learned
// corpus, outside any checkout, since they hold a task's ticket and answers.
func (l Layout) LearnLocalCases() string { return filepath.Join(l.Learn(), "local") }

// LearnLocalCorpus is the eval corpus of the local cases,
// LearnLocalCases()/cases.toml (no case name has a dot), which the operator
// writes from what `magnum eval add` prints and `magnum eval run` reads
// after the learned corpus: never the operator's eval.toml, which builds
// from before local cases (the daemon's skill gate on one, eval-at.sh's
// older builds) read and refuse whole for a key they do not know.
func (l Layout) LearnLocalCorpus() string { return filepath.Join(l.LearnLocalCases(), "cases.toml") }

// Eval is where the eval runs live, Eval()/<run id>/ (`magnum eval run`
// and the gate of a skill proposal), with the flagged cases and the cases'
// worktrees (wt/).
func (l Layout) Eval() string { return filepath.Join(l.State(), "eval") }

// LearnSkill holds the skill editor's run directories,
// LearnSkill()/<run>/: its inputs, the pins and a copy of its answer.
func (l Layout) LearnSkill() string { return filepath.Join(l.Learn(), "skill") }

// LearnWorktrees holds the skill editor's git worktrees of the checkout,
// LearnWorktrees()/<run>, each on its branch learn/<run> until the proposal
// lands or goes.
func (l Layout) LearnWorktrees() string { return filepath.Join(l.State(), "learn", "wt") }

// LearnEvalWorktrees holds the worktrees of the cases the gate of a skill
// proposal replays, LearnEvalWorktrees()/<case> in the case's clone: its
// own, so a `magnum eval run` of the same case (Eval()/wt/<case>) never
// removes the gate's.
func (l Layout) LearnEvalWorktrees() string { return filepath.Join(l.State(), "learn", "eval", "wt") }

// ReviewDir is where one review round's reports live: a directory per head
// in PRReviews.
func (l Layout) ReviewDir(owner, repo string, number int, sha string) string {
	if len(sha) > 12 {
		sha = sha[:12]
	}
	return filepath.Join(l.PRReviews(owner, repo, number), sha)
}

// PRReviews is the root of one PR's review reports: its per-head
// directories (ReviewDir) and what spans its heads (the evidence map).
func (l Layout) PRReviews(owner, repo string, number int) string {
	return filepath.Join(l.Reviews(), owner, repo, fmt.Sprint(number))
}

// EnsureDirs creates the state tree and makes every directory in it private
// (0700). MkdirAll applies its mode only to directories it creates, so a state
// tree that already exists with looser permissions (an older install, a
// manual mkdir) is tightened explicitly: it holds the database, logs and the
// per-identity gh credentials.
func (l Layout) EnsureDirs() error {
	for _, d := range []string{l.State(), l.data(), l.Logs(), l.Reviews(), l.GhRoot(), l.factoryDir(), l.FactoryTasks()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}
