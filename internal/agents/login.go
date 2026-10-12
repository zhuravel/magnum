package agents

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/zhuravel/magnum/internal/paths"
)

// LoginStamp fingerprints an agent kind's login file (config.Kind.LoginFile:
// its mtime and size, "absent" for a missing file). The engine records it
// when a run proved the kind logged out and lifts that pause only once the
// stamp changes, because a login check that reads the saved tokens (`codex
// login status`) still says logged in while a revoked refresh token fails
// every call. "" for a kind without a login file, or one whose home magnum
// cannot tell: a relative path is under the kind's home, which only codex
// has (the CODEX_HOME of the kind's env, else codexConfigPath's directory).
func (m *Manager) LoginStamp(kind string) string {
	k, ok := m.kindSpec(kind)
	if !ok || k.LoginFile == "" {
		return ""
	}
	path := paths.Expand(k.LoginFile)
	if !filepath.IsAbs(path) {
		if kind != KindCodex {
			return ""
		}
		home := ""
		if h := k.Env["CODEX_HOME"]; h != "" {
			home = paths.Expand(h)
		} else if p := m.codexConfigPath(); p != "" {
			home = filepath.Dir(p)
		}
		if home == "" {
			return ""
		}
		path = filepath.Join(home, path)
	}
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "absent"
	}
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.ModTime().UnixNano(), st.Size())
}
