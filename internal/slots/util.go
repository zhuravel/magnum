package slots

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
)

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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

// lockHash hashes the slot's LockFiles (a missing file hashes as missing).
func lockHash(dir string) (string, error) {
	h := sha256.New()
	for _, name := range LockFiles {
		b, err := os.ReadFile(filepath.Join(dir, name))
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

// canonPath cleans p and resolves symlinks when it exists.
func canonPath(p string) string {
	if p == "" {
		return ""
	}
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// under reports whether p is root or inside it (both canonical).
func under(p, root string) bool {
	if p == "" || root == "" {
		return false
	}
	p = canonPath(p)
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

var schemaVersionRe = regexp.MustCompile(`define\(\s*version:\s*([0-9_]+)\s*\)`)

// schemaVersion returns the version in db/schema.rb with underscores removed
// (the form stored in schema_migrations). ok is false when there is none.
func schemaVersion(dir string) (version string, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(dir, SchemaFile))
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

// slotNumberForPath returns n such that pool.Path(n) == path.
func slotNumberForPath(pool config.Pool, path string) (int, bool) {
	const sentinel = "\x00"
	parts := strings.Split(paths.Expand(strings.ReplaceAll(pool.SlotPath, "{n}", sentinel)), sentinel)
	if len(parts) < 2 {
		return 0, false
	}
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	m := regexp.MustCompile("^" + strings.Join(parts, "([0-9]+)") + "$").FindStringSubmatch(filepath.Clean(path))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 || canonPath(pool.Path(n)) != canonPath(path) {
		return 0, false
	}
	return n, true
}
