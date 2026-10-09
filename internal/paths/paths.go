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

// ReviewDir is where one review round's reports live.
func (l Layout) ReviewDir(owner, repo string, number int, sha string) string {
	if len(sha) > 12 {
		sha = sha[:12]
	}
	return filepath.Join(l.Reviews(), owner, repo, fmt.Sprint(number), sha)
}

// EnsureDirs creates the state tree and makes every directory in it private
// (0700). MkdirAll applies its mode only to directories it creates, so a state
// tree that already exists with looser permissions (an older install, a
// manual mkdir) is tightened explicitly: it holds the database, logs and the
// per-identity gh credentials.
func (l Layout) EnsureDirs() error {
	for _, d := range []string{l.State(), l.data(), l.Logs(), l.Reviews(), l.GhRoot()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}
