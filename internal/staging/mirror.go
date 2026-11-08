package staging

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/zhuravel/magnum/internal/fsx"
)

// volumesDir is where macOS mounts the external volumes.
const volumesDir = "/Volumes"

// lockPoll is how often a Refresh tries the mirror's flock while another
// holds it.
const lockPoll = 50 * time.Millisecond

// maxStateFile bounds the state file Refresh reads.
const maxStateFile = 64 << 20

// repoNamePart is an owner or a repository name as GitHub allows it.
var repoNamePart = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9_.-]*$`)

// MirrorDir is Deps.Mirror over [staging] mirror_dir root: owner/name's
// mirror is <root>/<owner>/<name>.git, in lower case. It reports ok false
// for no root, a repository name that is not owner/name, and a root under
// /Volumes/<volume> while that volume is not mounted (then with the dir,
// which Refresh reads as WhyVolumeNotMounted). It creates nothing.
func MirrorDir(root string) func(repo string) (dir string, ok bool) {
	return mirrorDir(volumesDir, root)
}

// mirrorDir is MirrorDir with the volumes' directory (tests use their own).
func mirrorDir(volumes, root string) func(repo string) (string, bool) {
	return func(repo string) (string, bool) {
		owner, name, ok := strings.Cut(repo, "/")
		if root == "" || !ok || !validPart(owner) || !validPart(name) {
			return "", false
		}
		dir := filepath.Join(root, strings.ToLower(owner), strings.ToLower(name)+".git")
		if !volumeMounted(volumes, root) {
			return dir, false
		}
		return dir, true
	}
}

// validPart reports whether s is an owner or repository name, never . or ..
func validPart(s string) bool {
	return s != "." && s != ".." && repoNamePart.MatchString(s)
}

// volumeMounted reports whether path's volume is mounted: true for a path
// outside volumes; for /Volumes/<name>/..., <name> must be a directory on
// another device than volumes (a mount point), or the link to / that
// macOS keeps for the system volume. A plain directory there is not the
// volume: writing into it would fill the system disk.
func volumeMounted(volumes, path string) bool {
	rel, err := filepath.Rel(volumes, filepath.Clean(path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return true
	}
	name, _, _ := strings.Cut(rel, string(filepath.Separator))
	root := filepath.Join(volumes, name)
	fi, err := os.Stat(root)
	if err != nil || !fi.IsDir() {
		return false
	}
	if target, err := filepath.EvalSymlinks(root); err == nil && target == "/" {
		return true
	}
	vi, err := os.Stat(volumes)
	if err != nil {
		return false
	}
	return device(fi) != device(vi)
}

// device is the device a file is on.
func device(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev) //nolint:gosec // Dev is int32 on darwin: a device number, never negative
	}
	return 0
}

// lockMirror takes the flock beside the mirror (<dir>.lock), creating the
// mirror's parent directory, and waits for it at most as long as ctx.
func lockMirror(ctx context.Context, dir string) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(dir+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}

// statePath is the state file beside the mirror.
func statePath(dir string) string { return dir + ".json" }

// readState reads the state file beside the mirror; nil when there is none,
// or one this build does not read.
func readState(dir string, log *slog.Logger) *state {
	data, err := fsx.ReadRegular(statePath(dir), maxStateFile)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Warn("staging: state file not read", "path", statePath(dir), "err", errLine(err))
		}
		return nil
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil || st.V != stateVersion {
		log.Info("staging: state file of another build or broken; it is read again", "path", statePath(dir))
		return nil
	}
	return &st
}

// writeState writes st beside the mirror, atomically.
func writeState(dir string, st *state) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return fsx.WriteFileAtomic(statePath(dir), data, 0o600)
}
