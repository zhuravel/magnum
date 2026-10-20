package config

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum"
	"github.com/zhuravel/magnum/internal/paths"
)

// The settings screen checks a candidate config before it writes it: the
// user layer comes from LoadOptions.UserData, exactly as Load would read it
// from the file, which still names it.
func TestLoadReadsTheUserLayerFromUserDataInsteadOfTheFile(t *testing.T) {
	home := t.TempDir()
	user := filepath.Join(home, "user.toml") // not config.toml: in the home that is a base file
	if err := os.WriteFile(user, []byte("[pipeline]\nspend = \"low\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	layout := paths.Layout{Home: home, UserConfig: user}
	data := []byte("[pipeline]\nspend = \"high\"\n")
	cfg, err := LoadWithOptions(layout, "", LoadOptions{UserData: data})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pipeline.Spend != SpendHigh {
		t.Errorf("spend = %q, want the data's high", cfg.Pipeline.Spend)
	}
	if !slices.Contains(cfg.Sources, user) {
		t.Errorf("Sources = %v, want the user config's path", cfg.Sources)
	}
	if sum, ok := cfg.UserSum(); !ok || sum != sha256.Sum256(data) {
		t.Errorf("UserSum = %x, %v; want the data's", sum, ok)
	}
	if cfg.BaseFile() != "" {
		t.Errorf("BaseFile = %q, want none (the built-in defaults)", cfg.BaseFile())
	}

	// From the file when no data is given.
	cfg, err = Load(layout, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pipeline.Spend != SpendLow {
		t.Errorf("spend = %q, want the file's low", cfg.Pipeline.Spend)
	}
	file, _ := os.ReadFile(user)
	if sum, ok := cfg.UserSum(); !ok || sum != sha256.Sum256(file) {
		t.Errorf("UserSum = %x, %v; want the file's", sum, ok)
	}

	// Data for a file that does not exist yet, and an empty layer.
	layout.UserConfig = filepath.Join(home, "missing.toml")
	if cfg, err = LoadWithOptions(layout, "", LoadOptions{UserData: data}); err != nil || cfg.Pipeline.Spend != SpendHigh {
		t.Errorf("data for a missing file: %v, %v", err, cfg)
	}
	if cfg, err = LoadWithOptions(layout, "", LoadOptions{UserData: []byte{}}); err != nil || cfg.Pipeline.Spend != SpendMedium {
		t.Errorf("an empty layer: %v", err)
	}
	if cfg, err = Load(layout, ""); err != nil {
		t.Fatal(err)
	} else if _, ok := cfg.UserSum(); ok {
		t.Error("UserSum is set without a user layer")
	}

	// A refused candidate names the user config.
	_, err = LoadWithOptions(layout, "", LoadOptions{UserData: []byte("[pipeline]\nspend = \"nope\"\n")})
	if err == nil || !strings.Contains(err.Error(), "missing.toml") {
		t.Errorf("a refused candidate: %v, want the path named", err)
	}
	_, err = LoadWithOptions(layout, "", LoadOptions{UserData: []byte("[pipeline]\nunknown_key = 1\n")})
	if err == nil || !strings.Contains(err.Error(), "missing.toml") || !strings.Contains(err.Error(), "unknown keys") {
		t.Errorf("an unknown key: %v", err)
	}

	// A base file is named by BaseFile.
	base := filepath.Join(home, "base.toml")
	if err := os.WriteFile(base, magnum.DefaultConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = Load(paths.Layout{Home: home}, base); err != nil || cfg.BaseFile() != base {
		t.Errorf("BaseFile = %q, %v; want %s", cfg.BaseFile(), err, base)
	}
}
