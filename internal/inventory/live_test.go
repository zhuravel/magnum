package inventory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// TestLiveScan runs a read-only scan of this machine (git worktree lists,
// MySQL schemata, herdr snapshot) against an empty temp registry, so every
// worktree shows up as external. It runs only with MAGNUM_LIVE_INVENTORY=1;
// GitHub states are confirmed (GraphQL reads) only with MAGNUM_LIVE_GITHUB=1.
func TestLiveScan(t *testing.T) {
	if os.Getenv("MAGNUM_LIVE_INVENTORY") != "1" {
		t.Skip("set MAGNUM_LIVE_INVENTORY=1 to scan this machine (read-only)")
	}
	home, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(paths.Layout{Home: home}, filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "magnum.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	my, err := mysqlx.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer my.Close()
	s := &Scanner{
		Store: st, Git: gitx.New(&execx.Real{}), MySQL: my, Herdr: &herdr.Client{Timeout: 5 * time.Second},
		Runner: &execx.Real{}, Config: cfg,
	}
	opts := Options{External: true}
	if os.Getenv("MAGNUM_LIVE_GITHUB") == "1" {
		s.GitHub = &github.Client{Run: &execx.Real{}}
		opts.GitHubStates = true
	}
	inv, err := s.Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range inv.External {
		t.Logf("external %-60s pr=%d(%s,confirmed=%v) state=%s(%s) slug=%s(%s) dbs=%d agents=%d drift=%v",
			e.Path, e.PRNumber, e.PRSource, e.PRConfirmed, e.GHState, e.StateSource, e.Slug, e.SlugSource, len(e.Databases), len(e.Agents), e.Drift)
	}
	for _, d := range inv.Drift {
		t.Logf("drift %s %s safe=%v: %s", d.Kind, d.Subject, d.Safe, d.Message)
	}
	t.Logf("databases=%d listed=%v orphans=%d warnings=%v", len(inv.Databases), inv.DatabasesListed, len(inv.OrphanDBs), inv.Warnings)
}
