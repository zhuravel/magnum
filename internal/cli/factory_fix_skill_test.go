package cli

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// The factory daemon copies its fixer's skill (the fix skill) when it
// starts, as magnum's daemon copies its own fix role's: the fixer reads the
// copy, so an edit of the skill reaches it only at the daemon's next start
// (`magnum factory install --drain`). The copy is the factory's own,
// never in the review daemon's directory, whose prune keeps only its own
// roles' copies.
func TestTheFactoryDaemonSnapshotsTheFixSkill(t *testing.T) {
	storetest.Serial(t)
	skill := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(skill, []byte("fix skill v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	start := func() (*Context, *factoryDaemon) {
		t.Helper()
		cfg := config.Defaults()
		i := slices.IndexFunc(cfg.Factory.Roles, func(r config.Role) bool { return r.Name == config.FactoryFixerRole })
		cfg.Factory.Roles[i].Skill = skill
		c := &Context{Version: "test", Layout: paths.Layout{Home: home}, Config: cfg, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
		storetest.Seed(t, c.Layout.FactoryAgentsDB())
		registrytest.Seed(t, c.Layout.FactoryDB())
		d, err := buildFactoryDaemon(c, slog.LevelInfo)
		if err != nil {
			t.Fatalf("the factory daemon does not build: %v", err)
		}
		return c, d
	}
	read := func(c *Context) (string, string) {
		t.Helper()
		fixer, ok := c.Config.FactoryRole(config.FactoryFixerRole)
		if !ok {
			t.Fatal("no fixer role")
		}
		copied := c.Config.SkillFile(config.RoleSkillPath(fixer, c.Layout))
		b, err := os.ReadFile(copied)
		if err != nil {
			t.Fatalf("the fixer's skill copy %s: %v", copied, err)
		}
		return copied, string(b)
	}

	c, d := start()
	if err := os.WriteFile(skill, []byte("fix skill v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	copied, text := read(c)
	if text != "fix skill v1\n" {
		t.Fatalf("the running daemon's fixer reads %q: an edit reached it before a restart", text)
	}
	if !strings.HasPrefix(copied, factory.SkillCopyDir(c.Layout)+string(filepath.Separator)) {
		t.Errorf("the copy %s is not in the factory's own directory %s", copied, factory.SkillCopyDir(c.Layout))
	}
	if _, err := os.Stat(engine.SkillCopyDir(c.Layout.State())); !os.IsNotExist(err) {
		t.Errorf("the factory daemon wrote into the review daemon's skill directory: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	c, d = start() // the restart
	defer d.Close()
	if _, text := read(c); text != "fix skill v2\n" {
		t.Fatalf("after the restart the fixer reads %q", text)
	}
}
