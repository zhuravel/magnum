package identity

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zhuravel/magnum/internal/fsx"
)

// syncConfigDir writes the current token into ConfigDir unless it is already
// there. It always writes the latest token (read under a.mu inside a.wmu), so
// a slow writer can never leave an older token behind.
func (a *App) syncConfigDir() error {
	a.wmu.Lock()
	defer a.wmu.Unlock()
	a.mu.Lock()
	tok := a.token
	a.mu.Unlock()
	if tok == "" {
		return errors.New("write gh config dir: no installation token yet")
	}
	dir := a.ConfigDir()
	hosts, cfg := filepath.Join(dir, "hosts.yml"), filepath.Join(dir, "config.yml")
	if tok == a.written && fsx.Exists(hosts) && fsx.Exists(cfg) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("write gh config dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("write gh config dir: %w", err)
	}
	if err := fsx.WriteFileAtomic(cfg, renderConfigYML(a.cfg.Name), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", cfg, err)
	}
	if err := fsx.WriteFileAtomic(hosts, renderHostsYML(a.cfg.Name, tok, a.cfg.Login), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", hosts, err)
	}
	a.written = tok
	return nil
}

// callDirPrefix is the name prefix of the gh config dir of one REST call of
// App name's own through gh (GhTransport), in the gh root.
func callDirPrefix(name string) string { return ".call-" + name + "-" }

// freshConfigDir makes a gh config dir in root, named prefix and a random
// number: fresh (0700, a name no pane's env holds, so nothing a pane wrote
// is in it), with magnum's config.yml for identity name alone and no
// token (the caller's GH_TOKEN carries it).
func freshConfigDir(root, prefix, name string) (string, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("gh config dir: %w", err)
	}
	dir, err := os.MkdirTemp(root, prefix)
	if err != nil {
		return "", fmt.Errorf("gh config dir: %w", err)
	}
	if err := fsx.WriteFileAtomic(filepath.Join(dir, "config.yml"), renderConfigYML(name), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("gh config dir: %w", err)
	}
	return dir, nil
}

// renderHostsYML is gh's hosts.yml with a plain oauth_token (gh 2.102 reads it
// without migrating it to the keyring; docs/spikes.md probe 7).
func renderHostsYML(name, token, login string) []byte {
	return []byte("# Managed by magnum for identity " + name + "; rewritten whenever the token rotates.\n" +
		"github.com:\n" +
		"    oauth_token: " + yamlString(token) + "\n" +
		"    user: " + yamlString(login) + "\n" +
		"    git_protocol: \"ssh\"\n")
}

func renderConfigYML(name string) []byte {
	return []byte("# Managed by magnum for identity " + name + ".\n" +
		"git_protocol: \"ssh\"\n" +
		"prompt: \"disabled\"\n")
}

// yamlString renders s as a YAML double-quoted scalar (a JSON string is one),
// so logins like "talkable[bot]" never need flow-indicator care.
func yamlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
