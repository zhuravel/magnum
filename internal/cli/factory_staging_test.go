package cli

import (
	"bytes"
	"log/slog"
	"maps"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// The factory daemon builds its staging client only when a [[factory.repo]]
// has staging entries, and closes nothing of it before its run: the
// engine's stop closes it.
func TestTheFactoryDaemonBuildsItsStagingClientOnlyForStagingEnvs(t *testing.T) {
	cfg := config.Defaults()
	c := &Context{Version: "test", Layout: paths.Layout{Home: t.TempDir()}, Config: cfg, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	storetest.Seed(t, c.Layout.FactoryAgentsDB())
	registrytest.Seed(t, c.Layout.FactoryDB())
	d, err := buildFactoryDaemon(c, slog.LevelInfo)
	if err != nil {
		t.Fatalf("the factory daemon does not build: %v", err)
	}
	if d.Staging != nil {
		t.Fatal("a staging client without a staging env")
	}
	d.Close()

	cfg.Factory.Repos = []config.FactoryRepo{{Repo: "example/shop", Base: "master", MainClone: t.TempDir(), Copies: t.TempDir() + "/shop.factory{n}",
		Staging: []config.FactoryStaging{{Name: "void", Branch: "staging-void"}}}}
	d, err = buildFactoryDaemon(c, slog.LevelInfo)
	if err != nil {
		t.Fatalf("the factory daemon does not build: %v", err)
	}
	defer d.Close()
	if d.Staging == nil {
		t.Fatal("no staging client for a repository with a staging env")
	}
}

// The staging fetch's git env is the repository's poll identity's: a gh
// identity's reads as you, GH_HOST pinned, your gh config dir named and
// every GitHub token the daemon inherited blanked, and its GitHub client
// reads with the same env. A repository whose identity cannot be found
// fetches nothing (nil): said once while the cause stays the same, and
// again after an env that worked.
func TestTheStagingGitEnvIsThePollIdentitysOrNone(t *testing.T) {
	storetest.Serial(t)
	ghDir := t.TempDir()
	t.Setenv("GH_CONFIG_DIR", ghDir)
	t.Setenv("GH_TOKEN", "inherited")
	cfg := config.Defaults()
	cfg.Identities = []config.Identity{{Name: "me", Kind: "gh", Login: "zhuravel"}}
	cfg.Watches = []config.Watch{{Owner: "example", Include: []string{"shop"}, Identity: "me"},
		{Owner: "example", Include: []string{"site"}, Identity: "me", PollIdentity: "gone"}}
	var log bytes.Buffer
	ids := newStagingIdentities(cfg, paths.Layout{Home: t.TempDir()}, &execx.Fake{}, slog.New(slog.NewTextHandler(&log, nil)))
	want := map[string]string{"GH_HOST": "github.com", "GH_CONFIG_DIR": ghDir, "GH_TOKEN": "", "GITHUB_TOKEN": "",
		"GH_ENTERPRISE_TOKEN": "", "GITHUB_ENTERPRISE_TOKEN": ""}
	if env := ids.gitEnv(t.Context(), "example/shop"); !maps.Equal(env, want) {
		t.Errorf("example/shop's git env %v, want %v", env, want)
	}
	if c := ids.client("example/shop"); c == nil || !maps.Equal(c.Env, want) {
		t.Errorf("example/shop's GitHub client %+v, want one with the same env", c)
	}
	warnings := func() int { return strings.Count(log.String(), "the staging status of a repository has no identity") }
	for range 2 {
		if env := ids.gitEnv(t.Context(), "example/site"); env != nil {
			t.Errorf("example/site's git env %v, want none for an identity the config lacks", env)
		}
		if c := ids.client("example/site"); c != nil {
			t.Error("example/site has a GitHub client")
		}
	}
	if n := warnings(); n != 1 {
		t.Errorf("%d warnings, want 1:\n%s", n, log.String())
	}
	cfg.Identities = append(cfg.Identities, config.Identity{Name: "gone", Kind: "gh", Login: "zhuravel"})
	if env := ids.gitEnv(t.Context(), "example/site"); env == nil {
		t.Fatal("example/site has no git env once its identity is there")
	}
	cfg.Identities = cfg.Identities[:1]
	ids.gitEnv(t.Context(), "example/site")
	if n := warnings(); n != 2 {
		t.Errorf("%d warnings, want the cause said again after the env that worked:\n%s", n, log.String())
	}
}
