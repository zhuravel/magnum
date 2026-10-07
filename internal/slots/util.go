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
