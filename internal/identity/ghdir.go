package identity

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	if tok == a.written && fileExists(hosts) && fileExists(cfg) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("write gh config dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("write gh config dir: %w", err)
	}
	if err := writeFileAtomic(cfg, renderConfigYML(a.cfg.Name)); err != nil {
		return err
	}
	if err := writeFileAtomic(hosts, renderHostsYML(a.cfg.Name, tok, a.cfg.Login)); err != nil {
		return err
	}
	a.written = tok
	return nil
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

// writeFileAtomic writes data to path via a 0600 temp file in the same
// directory and a rename, so readers never see a partial file.
func writeFileAtomic(path string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(0o600); err == nil {
		if _, err = f.Write(data); err == nil {
			err = f.Sync()
		}
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }
