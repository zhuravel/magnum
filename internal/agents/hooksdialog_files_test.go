package agents

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const noHooksConfig = "[mcp_servers.github]\nurl = \"https://example.com/mcp\"\n"

// plantConfig makes path a .codex/config.toml of kind: a symlink to a plain
// config without hooks outside the checkout, a symlink to /dev/zero, a
// FIFO, or a valid config without hooks past codexConfigMax.
func plantConfig(t *testing.T, kind, path string) {
	t.Helper()
	var err error
	switch kind {
	case "symlink outside":
		outside := filepath.Join(t.TempDir(), "config.toml")
		if err = os.WriteFile(outside, []byte(noHooksConfig), 0o600); err == nil {
			err = os.Symlink(outside, path)
		}
	case "symlink to /dev/zero":
		err = os.Symlink("/dev/zero", path)
	case "fifo":
		err = syscall.Mkfifo(path, 0o600)
	case "valid TOML past the cap":
		err = os.WriteFile(path, []byte(noHooksConfig+strings.Repeat("# padding\n", codexConfigMax/10+1)), 0o600)
	default:
		t.Fatalf("unknown kind %q", kind)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// A checkout's .codex/config.toml belongs to the pull request. One that is a
// symlink (even to a plain file without hooks outside the checkout), a FIFO,
// /dev/zero or past its cap is not read: magnum cannot tell whether the
// checkout declares hooks, so the hooks review is declined. Nothing blocks.
func TestCheckoutHooksCannotTellFromAConfigItDoesNotRead(t *testing.T) {
	for kind, wantErr := range map[string]string{
		"symlink outside":         "not a regular file",
		"symlink to /dev/zero":    "not a regular file",
		"fifo":                    "not a regular file",
		"valid TOML past the cap": "too large",
	} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dir, ".codex"), 0o755); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(dir, ".codex", "config.toml")
			plantConfig(t, kind, p)
			done := make(chan struct{})
			var (
				got string
				err error
			)
			go func() { defer close(done); got, err = checkoutHooks(dir) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("checkoutHooks blocked")
			}
			if err == nil || got != "" || !strings.Contains(err.Error(), wantErr) || !strings.Contains(err.Error(), "cannot read "+p) {
				t.Fatalf("checkoutHooks = %q, %v; want cannot read %s: ...%s", got, err, p, wantErr)
			}
		})
	}
}
