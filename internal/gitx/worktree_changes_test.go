package gitx

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// WorkTreeChanges compares the files on disk under one directory with a
// base commit, as a tool reading that directory sees them: committed and
// uncommitted changes, deletions, untracked and ignored files, the path
// taken literally from the repository's top; NUL-separated, so any name
// survives, sorted without duplicates.
func TestWorkTreeChanges(t *testing.T) {
	ctx := context.Background()
	c, f := newFake(
		outRule(".codex/config.toml\x00.codex/a b.toml\x00", "git", "-C", slot, "diff"),
		outRule(".codex/new.toml\x00.codex/config.toml\x00", "git", "-C", slot, "ls-files"),
	)
	got, err := c.WorkTreeChanges(ctx, slot, sha1, ".codex")
	if want := []string{".codex/a b.toml", ".codex/config.toml", ".codex/new.toml"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("WorkTreeChanges = %q, %v; want %q", got, err, want)
	}
	cmd := wantCall(t, f, 0, false, "git", "-C", slot, "diff", "--name-only", "-z", "--no-renames", "--no-ext-diff", sha1, "--", ":(top,literal).codex")
	if cmd.Env["GIT_OPTIONAL_LOCKS"] != "0" {
		t.Errorf("env=%v", cmd.Env)
	}
	wantCall(t, f, 1, false, "git", "-C", slot, "ls-files", "-z", "--others", "--", ":(top,literal).codex")
	for _, bad := range [][2]string{{"-x", ".codex"}, {"a..b", ".codex"}, {sha1, ""}} {
		if _, err := c.WorkTreeChanges(ctx, slot, bad[0], bad[1]); err == nil {
			t.Errorf("WorkTreeChanges(%q, %q) must be refused", bad[0], bad[1])
		}
	}
}

func TestRealWorkTreeChanges(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	dir := fx.origin
	fx.write(dir, ".gitignore", ".codex/local.toml\n")
	base := fx.commit(dir, ".codex/config.toml", "[mcp_servers.docs]\n", "Add the Codex config")
	changes := func(want ...string) {
		t.Helper()
		got, err := fx.c.WorkTreeChanges(ctx, dir, base, ".codex")
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("WorkTreeChanges = %q, %v; want %q", got, err, want)
		}
	}
	changes()
	fx.commit(dir, "app/x.rb", "x\n", "Change another directory")
	fx.write(dir, "sub/.codex/config.toml", "[mcp_servers.nested]\n")
	changes()
	fx.commit(dir, ".codex/hooks.json", "{}\n", "Add hooks")
	changes(".codex/hooks.json")
	fx.write(dir, ".codex/config.toml", "[mcp_servers.evil]\n")
	fx.write(dir, ".codex/local.toml", "ignored\n")
	fx.write(dir, ".codex/rules/x.rules", "untracked\n")
	changes(".codex/config.toml", ".codex/hooks.json", ".codex/local.toml", ".codex/rules/x.rules")
	// On disk, a file the base lacks and the head deleted again is no change.
	if err := os.RemoveAll(filepath.Join(dir, ".codex")); err != nil {
		t.Fatal(err)
	}
	changes(".codex/config.toml")
}

// Claude Code loads its project config from two paths at the checkout's
// top, .claude/ and .mcp.json: one comparison takes both, each literally,
// and a .mcp.json the head changes or deletes counts like a file under
// .claude/; a nested .mcp.json and the root's CLAUDE.md do not.
func TestWorkTreeChangesOfSeveralPaths(t *testing.T) {
	ctx := context.Background()
	c, f := newFake(
		outRule(".mcp.json\x00", "git", "-C", slot, "diff"),
		outRule(".claude/settings.local.json\x00", "git", "-C", slot, "ls-files"),
	)
	got, err := c.WorkTreeChanges(ctx, slot, sha1, ".claude", ".mcp.json")
	if want := []string{".claude/settings.local.json", ".mcp.json"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("WorkTreeChanges = %q, %v; want %q", got, err, want)
	}
	wantCall(t, f, 0, false, "git", "-C", slot, "diff", "--name-only", "-z", "--no-renames", "--no-ext-diff", sha1, "--",
		":(top,literal).claude", ":(top,literal).mcp.json")
	wantCall(t, f, 1, false, "git", "-C", slot, "ls-files", "-z", "--others", "--", ":(top,literal).claude", ":(top,literal).mcp.json")
	for _, paths := range [][]string{nil, {".claude", ""}} {
		if _, err := c.WorkTreeChanges(ctx, slot, sha1, paths...); err == nil {
			t.Errorf("WorkTreeChanges(%q) must be refused", paths)
		}
	}

	fx := newFixture(t)
	dir := fx.origin
	fx.write(dir, ".gitignore", ".claude/settings.local.json\n")
	fx.commit(dir, ".claude/settings.json", "{}\n", "Add the team's Claude settings")
	base := fx.commit(dir, ".mcp.json", "{\"mcpServers\":{}}\n", "Add the team's MCP servers")
	changes := func(want ...string) {
		t.Helper()
		got, err := fx.c.WorkTreeChanges(ctx, dir, base, ".claude", ".mcp.json")
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("WorkTreeChanges = %q, %v; want %q", got, err, want)
		}
	}
	changes()
	fx.commit(dir, "CLAUDE.md", "x\n", "Change the instructions only")
	fx.write(dir, "sub/.mcp.json", "{}\n")
	changes()
	fx.commit(dir, ".mcp.json", "{\"mcpServers\":{\"evil\":{\"command\":\"evil-mcp\"}}}\n", "Add a server")
	fx.write(dir, ".claude/settings.local.json", "{\"hooks\":{}}\n")
	changes(".claude/settings.local.json", ".mcp.json")
	if err := os.Remove(filepath.Join(dir, ".mcp.json")); err != nil {
		t.Fatal(err)
	}
	changes(".claude/settings.local.json", ".mcp.json")
}
