package agents

import (
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/paths"
)

// mcpServerName is a TOML bare key, which Codex's -c key path carries as
// written; any other name (a quoted key with a dot, a space or a non-ASCII
// letter) would change the path or the shell word, so it is skipped.
var mcpServerName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// mcpServers lists the MCP servers a Codex session of role would load, for
// its kind's mcp_off (config.Kind.MCPOffArgs; the kind of a shell role is
// its tool): the servers the Codex config the session reads declares
// (declaredMCPServers). The file is the config.toml in the CODEX_HOME the
// role's pane env sets (config.Config.RoleEnv, ~ expanded), else
// codexConfigPath's ($CODEX_HOME/config.toml, else ~/.codex/config.toml;
// none in a test binary without Deps.CodexConfig). nil without mcp_off or
// mcp_disable, and for a missing file; an unreadable one and each name that
// is no bare key are logged without the path (pane env values stay out of
// logs). Read at every launch, so a server added later is turned off too.
func (m *Manager) mcpServers(role config.Role) []string {
	k, ok := m.kindSpec(role.AgentKind())
	if !ok || !k.MCPOff || len(k.MCPDisable) == 0 {
		return nil
	}
	path := m.codexConfigPath()
	if h := m.d.Config.RoleEnv(role)["CODEX_HOME"]; h != "" {
		path = filepath.Join(paths.Expand(h), "config.toml")
	}
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	var names, skipped []string
	if err == nil {
		names, skipped, err = declaredMCPServers(string(data))
	}
	if err != nil {
		if pe := (*fs.PathError)(nil); errors.As(err, &pe) {
			err = pe.Err
		}
		m.logAt(slog.LevelWarn, "agents: %s: cannot read the Codex config, so its MCP servers stay on: %v", role.Name, err)
		return nil
	}
	for _, n := range skipped {
		m.logf("agents: %s: MCP server %q of the Codex config is no TOML bare key, so it stays on", role.Name, n)
	}
	return names
}

// declaredMCPServers reads a Codex config: the names of its mcp_servers
// tables that do not set enabled = false, sorted, and those of them that
// are no TOML bare key (mcpServerName), apart and sorted.
func declaredMCPServers(data string) (names, skipped []string, err error) {
	var doc struct {
		MCPServers map[string]any `toml:"mcp_servers"`
	}
	if _, err := toml.Decode(data, &doc); err != nil {
		return nil, nil, err
	}
	for name, v := range doc.MCPServers {
		if t, ok := v.(map[string]any); ok && t["enabled"] == false {
			continue
		}
		if mcpServerName.MatchString(name) {
			names = append(names, name)
		} else {
			skipped = append(skipped, name)
		}
	}
	slices.Sort(names)
	slices.Sort(skipped)
	return names, skipped, nil
}

// pluginName is a Codex plugin's name@marketplace in TOML bare keys, which
// a dotted -c key path carries as written (plugins.<name>.enabled); any
// other name stays on, with a log line.
var pluginName = regexp.MustCompile(`^[A-Za-z0-9_-]+@[A-Za-z0-9_-]+$`)

// codexPlugins lists the Codex plugins a session of role would load, for
// its kind's mcp_off (config.Kind.PluginOffArgs): the plugins installed in
// the Codex home's plugins/cache (one directory per marketplace, one per
// plugin in it: <plugin>@<marketplace>) and those the config.toml declares
// in a [plugins."<name>"] table, except those a table sets enabled = false.
// The home is the CODEX_HOME the role's pane env sets, else the directory
// of codexConfigPath's file (as mcpServers reads it). nil without mcp_off
// or plugin_disable; a config that cannot be read is logged without the
// path and leaves every plugin on. Read at every launch, so a plugin
// installed later is turned off too.
func (m *Manager) codexPlugins(role config.Role) []string {
	k, ok := m.kindSpec(role.AgentKind())
	if !ok || !k.MCPOff || len(k.PluginDisable) == 0 {
		return nil
	}
	path := m.codexConfigPath()
	if h := m.d.Config.RoleEnv(role)["CODEX_HOME"]; h != "" {
		path = filepath.Join(paths.Expand(h), "config.toml")
	}
	if path == "" {
		return nil
	}
	on := map[string]bool{}
	var skipped []string
	for _, p := range installedCodexPlugins(filepath.Dir(path)) {
		if pluginName.MatchString(p) {
			on[p] = true
		} else {
			skipped = append(skipped, p)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		if pe := (*fs.PathError)(nil); errors.As(err, &pe) {
			err = pe.Err
		}
		m.logAt(slog.LevelWarn, "agents: %s: cannot read the Codex config, so its plugins stay on: %v", role.Name, err)
		return nil
	}
	if err == nil {
		declared, off, derr := declaredCodexPlugins(string(data))
		if derr != nil {
			m.logAt(slog.LevelWarn, "agents: %s: cannot read the Codex config, so its plugins stay on: %v", role.Name, derr)
			return nil
		}
		for _, p := range declared {
			if pluginName.MatchString(p) {
				on[p] = true
			} else {
				skipped = append(skipped, p)
			}
		}
		for _, p := range off {
			delete(on, p)
		}
	}
	slices.Sort(skipped)
	for _, p := range slices.Compact(skipped) {
		m.logf("agents: %s: Codex plugin %q is no name@marketplace in bare keys, so it stays on", role.Name, p)
	}
	return slices.Sorted(maps.Keys(on))
}

// installedCodexPlugins reads a Codex home's plugins/cache: one directory
// per marketplace, one per plugin in it, as <plugin>@<marketplace>. Hidden
// entries (Codex's staging directories) and files are not plugins. A
// missing or unreadable directory lists nothing.
func installedCodexPlugins(home string) []string {
	var out []string
	cache := filepath.Join(home, "plugins", "cache")
	markets, err := os.ReadDir(cache)
	if err != nil {
		return nil
	}
	for _, mk := range markets {
		if !mk.IsDir() || strings.HasPrefix(mk.Name(), ".") {
			continue
		}
		plugins, err := os.ReadDir(filepath.Join(cache, mk.Name()))
		if err != nil {
			continue
		}
		for _, p := range plugins {
			if p.IsDir() && !strings.HasPrefix(p.Name(), ".") {
				out = append(out, p.Name()+"@"+mk.Name())
			}
		}
	}
	return out
}

// declaredCodexPlugins reads a Codex config: the names of its [plugins]
// tables (name@marketplace) that do not set enabled = false, and those that
// do, both sorted.
func declaredCodexPlugins(data string) (on, off []string, err error) {
	var doc struct {
		Plugins map[string]any `toml:"plugins"`
	}
	if _, err := toml.Decode(data, &doc); err != nil {
		return nil, nil, err
	}
	for name, v := range doc.Plugins {
		if t, ok := v.(map[string]any); ok && t["enabled"] == false {
			off = append(off, name)
		} else {
			on = append(on, name)
		}
	}
	slices.Sort(on)
	slices.Sort(off)
	return on, off, nil
}
