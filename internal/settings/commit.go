package settings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"syscall"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/store"
)

// HistoryKeep is how many earlier versions of the config the history keeps.
const HistoryKeep = 20

// Result is a written save: the history file holding the previous bytes
// ("" when there were none: the file did not exist) and the new file's
// hash.
type Result struct {
	History string
	Sum     [32]byte
}

// HistoryEntry is an earlier version of the config: a file of the history
// directory, when it was replaced, the hash and size of its content.
type HistoryEntry struct {
	Path  string
	Saved time.Time
	Sum   [32]byte
	Size  int64
}

// historyName is a history file's name: the UTC time the version was
// replaced and the first 12 hex digits of its SHA-256.
var historyName = regexp.MustCompile(`^(\d{8}T\d{6}Z)-[0-9a-f]{12}\.toml$`)

// checkWritable refuses a config the atomic write (a rename over it) would
// break: a symlink (it would become a file and a dotfiles checkout would
// silently lose the link), a file that is not a regular one, and a file
// with more than one hard link (the other names would keep the old
// content). A file that does not exist yet is writable.
func checkWritable(path string) error {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("settings: %w", err)
	case fi.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("settings: %s is a symlink; the settings screen writes only a regular file, so edit it by hand (or make it a file)", path)
	case !fi.Mode().IsRegular():
		return fmt.Errorf("settings: %s is not a regular file", path)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
		return fmt.Errorf("settings: %s has %d hard links; a save replaces the file, which would leave the other links on the old "+
			"content, so edit it by hand", path, st.Nlink)
	}
	return nil
}

// commitHook, when set (tests), runs inside Commit at each named stage:
// "locked" once the lock is held, "rename" right before the write.
var commitHook func(stage string)

func hook(stage string) {
	if commitHook != nil {
		commitHook(stage)
	}
}

// lockWait is how long Commit waits for another save's lock.
const lockWait = 10 * time.Second

// lockFile is the lock file saves of the config at path take: beside it,
// in its directory resolved to its canonical path, so every writer of the
// same file takes the same lock whatever layout (MAGNUM_HOME) it runs with
// and however it names the directory. It is created 0600 and kept.
func lockFile(path string) (string, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("settings: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("settings: %w", err)
	}
	return filepath.Join(canonical, "."+filepath.Base(path)+".lock"), nil
}

// lockConfig takes the lock of saves of the config at config (lockFile;
// flock, exclusive), waiting up to lockWait for another save to end.
func lockConfig(config string) (func(), error) {
	path, err := lockFile(config)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(lockWait)
	for {
		unlock, held, err := engine.AcquireLock(path)
		switch {
		case err != nil:
			return nil, fmt.Errorf("settings: lock %s: %w", path, err)
		case !held:
			return unlock, nil
		case time.Now().After(deadline):
			return nil, errors.New("settings: another save of the config is in progress; try again")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Commit writes a plan under the saves' lock (a flock on a file beside the
// config, lockFile, so two screens or a screen and a script never
// interleave, whatever layout each runs with): the
// file must be one the write cannot break (checkWritable) and still hash as
// the plan's old bytes (ErrChanged otherwise), the previous bytes go to the
// config history (the last HistoryKeep kept), then, both checks made again
// right before the write, the new bytes replace the file atomically,
// keeping its mode (0600 for a new one).
func Commit(p *Plan) (Result, error) {
	if p == nil || p.New == nil || p.Path == "" {
		return Result{}, errors.New("settings: no plan to write")
	}
	unlock, err := lockConfig(p.Path)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	hook("locked")
	if err := checkWritable(p.Path); err != nil {
		return Result{}, err
	}
	cur, existed, err := readConfig(p.Path)
	if err != nil {
		return Result{}, err
	}
	if sha256.Sum256(cur) != p.Sum {
		return Result{}, ErrChanged
	}
	mode := fs.FileMode(0o600)
	if fi, err := os.Stat(p.Path); err == nil {
		mode = fi.Mode().Perm()
	}
	var hist string
	if existed && len(cur) > 0 {
		if hist, err = keepHistory(p.history, cur, time.Now()); err != nil {
			return Result{}, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(p.Path), 0o700); err != nil {
		return Result{}, fmt.Errorf("settings: %w", err)
	}
	// Again right before the write: keeping the history took a moment, and
	// an editor's save in it (editors take no lock) must not be overwritten.
	hook("rename")
	if err := checkWritable(p.Path); err != nil {
		return Result{}, err
	}
	if again, _, err := readConfig(p.Path); err != nil || sha256.Sum256(again) != p.Sum {
		if err != nil {
			return Result{}, err
		}
		return Result{}, ErrChanged
	}
	if err := fsx.WriteFileAtomic(p.Path, p.New, mode); err != nil {
		return Result{}, fmt.Errorf("settings: write %s: %w", p.Path, err)
	}
	return Result{History: hist, Sum: sha256.Sum256(p.New)}, nil
}

// keepHistory writes data, the version a save replaces, into dir (unless
// the newest entry holds it already) and prunes all but the newest
// HistoryKeep entries.
func keepHistory(dir string, data []byte, now time.Time) (string, error) {
	if dir == "" {
		return "", errors.New("settings: no config-history directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("settings: %w", err)
	}
	sum := sha256.Sum256(data)
	entries, err := listHistory(dir)
	if err != nil {
		return "", err
	}
	if len(entries) > 0 && entries[0].Sum == sum {
		return entries[0].Path, nil
	}
	// The names sort by time: a second save within the same second takes
	// the next second.
	stamp := now.UTC().Truncate(time.Second)
	if len(entries) > 0 && !stamp.After(entries[0].Saved) {
		stamp = entries[0].Saved.Add(time.Second)
	}
	path := filepath.Join(dir, stamp.Format("20060102T150405Z")+"-"+hex.EncodeToString(sum[:])[:12]+".toml")
	if err := fsx.WriteFileAtomic(path, data, 0o600); err != nil {
		return "", fmt.Errorf("settings: keep the previous config: %w", err)
	}
	if entries, err = listHistory(dir); err == nil && len(entries) > HistoryKeep {
		for _, e := range entries[HistoryKeep:] {
			_ = os.Remove(e.Path) // the next save prunes what this one could not
		}
	}
	return path, nil
}

// listHistory lists dir's history files, newest first.
func listHistory(dir string) ([]HistoryEntry, error) {
	des, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}
	var out []HistoryEntry
	for _, de := range des {
		m := historyName.FindStringSubmatch(de.Name())
		if m == nil || !de.Type().IsRegular() {
			continue
		}
		saved, err := time.Parse("20060102T150405Z", m[1])
		if err != nil {
			continue
		}
		path := filepath.Join(dir, de.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		out = append(out, HistoryEntry{Path: path, Saved: saved, Sum: sha256.Sum256(data), Size: int64(len(data))})
	}
	slices.SortFunc(out, func(a, b HistoryEntry) int { return -compareNames(a.Path, b.Path) })
	return out, nil
}

func compareNames(a, b string) int {
	switch {
	case filepath.Base(a) < filepath.Base(b):
		return -1
	case filepath.Base(a) > filepath.Base(b):
		return 1
	}
	return 0
}

// History lists the earlier versions of the config the saves kept, newest
// first.
func (s *Snapshot) History() ([]HistoryEntry, error) { return listHistory(s.historyDir()) }

// Restore is the save that puts an earlier version back: a Plan like any
// save (the file must not have changed since Load, the version must load
// and render, and the version it replaces goes to the history in turn).
func (s *Snapshot) Restore(e HistoryEntry, now time.Time) (*Plan, error) {
	data, err := readHistory(s.historyDir(), e.Path)
	if err != nil {
		return nil, err
	}
	if err := checkWritable(s.Path); err != nil {
		return nil, err
	}
	cur, _, err := readConfig(s.Path)
	if err != nil {
		return nil, err
	}
	if sha256.Sum256(cur) != s.Sum {
		return nil, ErrChanged
	}
	p, err := s.plan(data)
	if err != nil {
		return nil, err
	}
	p.Count = len(p.Hunks)
	_ = now // a restore writes no notes; now is kept for the screen's symmetry with Plan
	return p, nil
}

// readHistory reads a history file: only a direct child of the history
// directory with a history file's name, a regular file and not a symlink,
// read through an os.Root of the directory, so no path or link leads it
// elsewhere.
func readHistory(dir, path string) ([]byte, error) {
	name := filepath.Base(path)
	if filepath.Clean(path) != filepath.Join(dir, name) || !historyName.MatchString(name) {
		return nil, fmt.Errorf("settings: %s is not a file of the config history", path)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}
	defer root.Close()
	fi, err := root.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("settings: %s is not a regular file of the config history", path)
	}
	data, err := root.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}
	return data, nil
}

// KVDaemonConfigSHA is the registry key holding the hex SHA-256 of the
// user config the running daemon loaded ("" when it read none).
const KVDaemonConfigSHA = engine.KVDaemonConfigSum

// RequestRestart asks the daemon to restart on the config whose hash is
// sum once nothing is in flight (engine.KVConfigRestart): it checks the
// file still loads first, and the next start clears the mark.
func RequestRestart(ctx context.Context, st *store.Store, sum [32]byte) error {
	b, err := json.Marshal(engine.ConfigRestartMark{SHA256: hex.EncodeToString(sum[:]), SavedAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	return st.SetKV(ctx, engine.KVConfigRestart, string(b))
}

// RestartPending reports whether the config on disk differs from the one
// the daemon runs: its content's hash against the one the daemon recorded
// at start. False when no daemon recorded one.
func RestartPending(ctx context.Context, st *store.Store, s *Snapshot) (bool, error) {
	running, ok := engine.ReadDaemonConfigSum(ctx, st)
	if !ok {
		return false, nil
	}
	cur, existed, err := readConfig(s.Path)
	if err != nil {
		return false, err
	}
	now := ""
	if existed {
		sum := sha256.Sum256(cur)
		now = hex.EncodeToString(sum[:])
	}
	return running != now, nil
}
