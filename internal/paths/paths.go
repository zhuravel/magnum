// Package paths defines magnum's on-disk layout. Everything lives inside the
// repository checkout (MAGNUM_HOME, default ~/Projects/magnum):
// config.toml next to the code, runtime state under state/ (gitignored).
package paths

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Layout resolves every path magnum reads or writes.
type Layout struct {
	Home string // repository root (MAGNUM_HOME)
	// Scratch, when set, holds the registry, the review reports and the
	// repository notes instead of state/ (magnum eval: a replay never touches
	// the live registry, reports or notes). Logs, locks, the pidfile and the
	// identities' gh config stay under state/.
	Scratch string
}

// modulePath is the module line of magnum's own go.mod: the only go.mod that
// marks the repository root.
const modulePath = "github.com/zhuravel/magnum"

// Resolve picks the home directory: MAGNUM_HOME, else the directory that
// contains config.toml or magnum's own go.mod walking up from the executable
// (symlinks resolved), else the default checkout location.
//
// MAGNUM_HOME is expanded (~), made absolute and symlink-free (see
// canonicalDir): magnum hands paths under Home (GH_CONFIG_DIR, the report
// directory) to agents whose working directory is a PR checkout, where a
// relative or symlinked path would mean something else.
func Resolve() (Layout, error) {
	if h := os.Getenv("MAGNUM_HOME"); h != "" {
		home, err := canonicalDir(Expand(h))
		if err != nil {
			return Layout{}, fmt.Errorf("paths: MAGNUM_HOME %q: %w", h, err)
		}
		return Layout{Home: home}, nil
	}
	if exe, err := os.Executable(); err == nil {
		if home, ok := homeFromExecutable(exe); ok {
			return Layout{Home: home}, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Layout{}, err
	}
	return Layout{Home: filepath.Join(home, "Projects", "magnum")}, nil
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
		if exists(filepath.Join(dir, "config.toml")) || isMagnumModule(filepath.Join(dir, "go.mod")) {
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

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// Expand replaces a leading ~ with the user's home directory.
func Expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

func (l Layout) Config() string               { return filepath.Join(l.Home, "config.toml") }
func (l Layout) State() string                { return filepath.Join(l.Home, "state") }
func (l Layout) DB() string                   { return filepath.Join(l.data(), "magnum.db") }
func (l Layout) Lock() string                 { return filepath.Join(l.State(), "magnum.lock") }
func (l Layout) OpsLock() string              { return filepath.Join(l.State(), "ops.lock") }
func (l Layout) Pid() string                  { return filepath.Join(l.State(), "daemon.pid") }
func (l Layout) Logs() string                 { return filepath.Join(l.State(), "logs") }
func (l Layout) DaemonLog() string            { return filepath.Join(l.Logs(), "daemon.log") }
func (l Layout) Reviews() string              { return filepath.Join(l.data(), "reviews") }
func (l Layout) Notes() string                { return filepath.Join(l.data(), "notes") }
func (l Layout) GhRoot() string               { return filepath.Join(l.State(), "gh") }
func (l Layout) GhConfigDir(id string) string { return filepath.Join(l.GhRoot(), id) }
func (l Layout) TabBar() string               { return filepath.Join(l.State(), "tabbar.txt") }
func (l Layout) Skill() string                { return filepath.Join(l.Home, "skills", "magnum-review", "SKILL.md") }
func (l Layout) Binary() string               { return filepath.Join(l.Home, "bin", "magnum") }
func (l Layout) Plugin() string               { return filepath.Join(l.Home, "herdr-plugin.toml") }

// data is where the registry, reports and notes live: Scratch when set, else
// state/.
func (l Layout) data() string {
	if l.Scratch != "" {
		return l.Scratch
	}
	return l.State()
}

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
	for _, d := range []string{l.State(), l.Logs(), l.Reviews(), l.GhRoot()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}
