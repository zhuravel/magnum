package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/zhuravel/magnum/internal/paths"
)

// A staging entry names a branch a deploy pushes to (R46, later): only a
// staging-<name> branch, never master, main, the repository's base or
// anything that starts with production, whatever the name says.
func TestAStagingBranchMustBeAStagingBranchAndNeverTheBaseOrProduction(t *testing.T) {
	for _, tc := range []struct {
		branch, base string
		want         []string
	}{
		{"", "master", []string{`staging "void" needs a branch`}},
		{"master", "master", []string{`staging "void": branch "master"`, "never master, main, the base or production"}},
		{"main", "master", []string{`staging "void": branch "main"`, "never master, main, the base or production"}},
		{"production-x", "master", []string{`staging "void": branch "production-x"`, "never master, main, the base or production"}},
		{"Production", "master", []string{`staging "void": branch "Production"`, "never master, main, the base or production"}},
		{"staging-trunk", "staging-trunk", []string{`staging "void": branch "staging-trunk"`, "never master, main, the base or production"}},
		{"staging_void", "master", []string{`staging "void": branch "staging_void" must match ^staging-[a-z0-9-]+$`}},
		{"staging-", "master", []string{`branch "staging-" must match`}},
		{"staging-Void", "master", []string{`branch "staging-Void" must match`}},
		{"void", "master", []string{`branch "void" must match`}},
	} {
		cfg := validFactoryConfig()
		cfg.Factory.Repos[0].Base = tc.base
		cfg.Factory.Repos[0].Staging = []FactoryStaging{{Name: "void", Branch: tc.branch}}
		err := cfg.Validate()
		if err == nil {
			t.Errorf("branch %q on base %q: valid, want an error", tc.branch, tc.base)
			continue
		}
		wantError(t, err, append([]string{"factory repo acme/app"}, tc.want...)...)
	}
	for _, ok := range []string{"staging-void", "staging-bart", "staging-eu-2"} {
		cfg := validFactoryConfig()
		cfg.Factory.Repos[0].Staging = []FactoryStaging{{Name: "void", Branch: ok}}
		if err := cfg.Validate(); err != nil {
			t.Errorf("branch %q: %v", ok, err)
		}
	}
}

// Stagings is a repository's staging environments in the order its
// [[factory.repo]] lists them, the repository named in any case; none for a
// repository without the block or without staging entries.
func TestStagingsAreTheRepositorysEntriesInOrder(t *testing.T) {
	cfg := validFactoryConfig()
	want := []FactoryStaging{
		{Name: "void", Branch: "staging-void", QA: "t:qa"},
		{Name: "bart", Branch: "staging-bart"},
		{Name: "bastion", Branch: "staging-bastion", After: "void"},
	}
	cfg.Factory.Repos[0].Staging = slices.Clone(want)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"acme/app", "Acme/App"} {
		if got := cfg.Stagings(repo); !slices.Equal(got, want) {
			t.Errorf("Stagings(%q) = %+v, want %+v", repo, got, want)
		}
	}
	got := cfg.Stagings("acme/app")
	got[0].Name = "changed"
	if cfg.Factory.Repos[0].Staging[0].Name != "void" {
		t.Error("Stagings returned the config's own slice: a caller's edit reached the config")
	}
	if got := cfg.Stagings("acme/other"); len(got) != 0 {
		t.Errorf("Stagings of a repository without a block = %+v, want none", got)
	}
	cfg.Factory.Repos[0].Staging = nil
	if got := cfg.Stagings("acme/app"); len(got) != 0 {
		t.Errorf("Stagings of a block without staging entries = %+v, want none", got)
	}
}

// [staging] mirror_dir defaults to <state dir>/staging, expands ~, and must
// be absolute.
func TestTheStagingMirrorDirDefaultsUnderTheStateDir(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	layout := paths.Layout{Home: dir, StateDir: state, DataDir: filepath.Join(dir, "data"), UserConfig: filepath.Join(dir, "user.toml")}
	cfg, err := LoadWithOptions(layout, "", LoadOptions{NoOverlay: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(state, "staging"); cfg.Staging.MirrorDir != want {
		t.Errorf("mirror_dir = %q, want %q", cfg.Staging.MirrorDir, want)
	}

	if err := os.WriteFile(layout.UserConfig, []byte("[staging]\nmirror_dir = \"/Volumes/External/magnum-staging\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = Load(layout, ""); err != nil {
		t.Fatal(err)
	}
	if want := "/Volumes/External/magnum-staging"; cfg.Staging.MirrorDir != want {
		t.Errorf("mirror_dir = %q, want the user's %q", cfg.Staging.MirrorDir, want)
	}

	if err := os.WriteFile(layout.UserConfig, []byte("[staging]\nmirror_dir = \"~/mirrors\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = Load(layout, ""); err != nil {
		t.Fatal(err)
	}
	if want := paths.Expand("~/mirrors"); cfg.Staging.MirrorDir != want {
		t.Errorf("mirror_dir = %q, want ~ expanded: %q", cfg.Staging.MirrorDir, want)
	}

	if err := os.WriteFile(layout.UserConfig, []byte("[staging]\nmirror_dir = \"mirrors\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(layout, "")
	wantError(t, err, "staging.mirror_dir must be absolute")
}
