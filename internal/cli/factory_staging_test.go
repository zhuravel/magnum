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

// The staging fetch's git env is the repository's poll identity's (a gh
// identity's pins GH_HOST alone, gh's own login answering), and a
// repository whose identity cannot be found fetches nothing (nil), said
// once in the log.
func TestTheStagingGitEnvIsThePollIdentitysOrNone(t *testing.T) {
	cfg := config.Defaults()
	cfg.Identities = []config.Identity{{Name: "me", Kind: "gh", Login: "zhuravel"}}
	cfg.Watches = []config.Watch{{Owner: "example", Include: []string{"shop"}, Identity: "me"},
		{Owner: "example", Include: []string{"site"}, Identity: "me", PollIdentity: "gone"}}
	var log bytes.Buffer
	ids := newStagingIdentities(cfg, paths.Layout{Home: t.TempDir()}, &execx.Fake{}, slog.New(slog.NewTextHandler(&log, nil)))
	if env := ids.gitEnv("example/shop"); !maps.Equal(env, map[string]string{"GH_HOST": "github.com"}) {
		t.Errorf("example/shop's git env %v, want the gh identity's", env)
	}
	if ids.client("example/shop") == nil {
		t.Error("example/shop has no GitHub client")
	}
	for range 2 {
		if env := ids.gitEnv("example/site"); env != nil {
			t.Errorf("example/site's git env %v, want none for an identity the config lacks", env)
		}
		if c := ids.client("example/site"); c != nil {
			t.Error("example/site has a GitHub client")
		}
	}
	if n := strings.Count(log.String(), "the staging status of a repository has no identity"); n != 1 {
		t.Errorf("%d warnings, want 1:\n%s", n, log.String())
	}
}
