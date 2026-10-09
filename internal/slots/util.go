package slots

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
)

func sortedKeys(m map[string]string) []string {
	return slices.Sorted(maps.Keys(m))
}

// freeDiskBytes reports the bytes available to unprivileged users on the
// filesystem holding path (or its nearest existing ancestor).
func freeDiskBytes(path string) (uint64, error) {
	p := filepath.Clean(path)
	for !fsx.Exists(p) {
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(p, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", p, err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil //nolint:unconvert // field types differ per OS
}

// Caps on the checkout files a slot reads (fsx.ReadRegular): the pull
// request controls them. A large application's Gemfile.lock, pnpm-lock.yaml
// or db/schema.rb is under 1 MiB; the slug marker is one short line.
const (
	lockFileMax   = 16 << 20
	schemaFileMax = 16 << 20
	markerMax     = 4 << 10
)

// lockHash hashes the slot's LockFiles (a missing file hashes as missing).
// A lockfile that is a symlink or a special file (fsx.ErrNotRegular) or past
// lockFileMax (fsx.ErrTooLarge) is an error, never a hash: deps would
// otherwise skip post_checkout for the next checkout refused the same way,
// or run it against a file magnum would not read, and lock_sha would record
// a hash of nothing the checkout holds.
func lockHash(dir string) (string, error) {
	h := sha256.New()
	for _, name := range LockFiles {
		b, err := fsx.ReadRegular(filepath.Join(dir, name), lockFileMax)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			fmt.Fprintf(h, "%s\x00<missing>\x00", name)
		case err != nil:
			return "", fmt.Errorf("slots: hash %s: %w", name, err)
		default:
			fmt.Fprintf(h, "%s\x00%d\x00", name, len(b))
			h.Write(b)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// under reports whether p is root or inside it (both canonical).
func under(p, root string) bool {
	if p == "" || root == "" {
		return false
	}
	p = fsx.Canon(p)
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

var schemaVersionRe = regexp.MustCompile(`define\(\s*version:\s*([0-9_]+)\s*\)`)

// schemaVersion returns the version in db/schema.rb with underscores removed
// (the form stored in schema_migrations). ok is false when there is none. A
// schema.rb that is a symlink or a special file, or past schemaFileMax, is
// an error (fsx.ReadRegular).
func schemaVersion(dir string) (version string, ok bool, err error) {
	b, err := fsx.ReadRegular(filepath.Join(dir, SchemaFile), schemaFileMax)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("slots: read %s: %w", SchemaFile, err)
	}
	m := schemaVersionRe.FindSubmatch(b)
	if m == nil {
		return "", false, nil
	}
	return strings.ReplaceAll(string(m[1]), "_", ""), true, nil
}

// ReadMarker returns dir's MarkerFile, the database slug its setup wrote,
// trimmed. The checkout's code writes it, so it is read with
// fsx.ReadRegular: a symlink or a special file is fsx.ErrNotRegular, one past
// 4 KiB fsx.ErrTooLarge, and a missing marker is fs.ErrNotExist.
func ReadMarker(dir string) (string, error) {
	b, err := fsx.ReadRegular(filepath.Join(dir, MarkerFile), markerMax)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// slotNumberForPath returns n such that pool.Path(n) == path.
func slotNumberForPath(pool config.Pool, path string) (int, bool) {
	tmpl, p := paths.Expand(pool.SlotPath), filepath.Clean(path)
	pre, _, ok := strings.Cut(tmpl, "{n}")
	if !ok || !TemplateRegexp(tmpl).MatchString(p) {
		return 0, false
	}
	digits := p[len(pre):]
	if i := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		digits = digits[:i]
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n <= 0 || fsx.Canon(pool.Path(n)) != fsx.Canon(path) {
		return 0, false
	}
	return n, true
}
