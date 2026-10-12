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

// codexHomeFor is where the Codex CLI of a pane environment env reads its
// config.toml, its plugins/cache and its auth.json: the CODEX_HOME of env
// (~ expanded; a role's pane env, config.Config.RoleEnv, or a kind's,
// config.Kind.Env), else the directory of codexConfigPath ($CODEX_HOME of
// the daemon's own environment, else ~/.codex; none in a test binary
// without Deps.CodexConfig). It returns the home and the config.toml file
// to read ("" and "" when neither is known). Every reader of a Codex home
// (codexOff, LoginStamp) goes through it, so that they agree.
func (m *Manager) codexHomeFor(env map[string]string) (home, configFile string) {
	if h := env["CODEX_HOME"]; h != "" {
		home = paths.Expand(h)
		return home, filepath.Join(home, "config.toml")
	}
	if p := m.codexConfigPath(); p != "" {
		return filepath.Dir(p), p
	}
	return "", ""
}

// mcpServerName is a TOML bare key, which Codex's -c key path carries as
// written; any other name (a quoted key with a dot, a space or a non-ASCII
// letter) would change the path or the shell word, so it is skipped.
var mcpServerName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// pluginName is a Codex plugin's name@marketplace in TOML bare keys, which
// a dotted -c key path carries as written (plugins.<name>.enabled); any
// other name stays on, with a log line.
var pluginName = regexp.MustCompile(`^[A-Za-z0-9_-]+@[A-Za-z0-9_-]+$`)

// codexConfig is what a launch reads of a Codex config.toml: the tables of
// its mcp_servers and of its plugins.
type codexConfig struct {
	MCPServers map[string]any `toml:"mcp_servers"`
	Plugins    map[string]any `toml:"plugins"`
}

// codexOff lists what a Codex session of role would load of the operator's
// Codex home and its kind's mcp_off turns off (config.Kind.ConfigOffArgs;
// the kind of a shell role is its tool): servers, the MCP servers the
// home's config.toml declares (MCPOffArgs), and plugins, those installed
// in its plugins/cache (one directory per marketplace, one per plugin in
// it: <plugin>@<marketplace>) and declared in a [plugins."<name>"] table,
// except those a table sets enabled = false (PluginOffArgs). The home is
// that of the role's pane env (codexHomeFor). It reads config.toml once, and
// only for what the kind asks: servers are nil without mcp_off or
// mcp_disable and for a missing file; plugins are nil without mcp_off or
// plugin_disable, and a home without a config still lists its cache. A
// config that cannot be read or decoded is logged once, without the path
// (pane env values stay out of logs), and leaves everything on, as does
// each name that is no bare key (logged too). Read at every launch, so a
// server or plugin added later is turned off too.
func (m *Manager) codexOff(role config.Role) (servers, plugins []string) {
	k, ok := m.kindSpec(role.AgentKind())
	if !ok || !k.MCPOff {
		return nil, nil
	}
	wantServers, wantPlugins := len(k.MCPDisable) > 0, len(k.PluginDisable) > 0
	if !wantServers && !wantPlugins {
		return nil, nil
	}
	home, path := m.codexHomeFor(m.d.Config.RoleEnv(role))
	if path == "" {
		return nil, nil
	}
	var doc codexConfig
	data, err := os.ReadFile(path)
	missing := errors.Is(err, fs.ErrNotExist)
	if err == nil {
		_, err = toml.Decode(string(data), &doc)
	}
	if err != nil && !missing {
		if pe := (*fs.PathError)(nil); errors.As(err, &pe) {
			err = pe.Err
		}
		what := "MCP servers and plugins"
		if !wantPlugins {
			what = "MCP servers"
		} else if !wantServers {
			what = "plugins"
		}
		m.logAt(slog.LevelWarn, "agents: %s: cannot read the Codex config, so its %s stay on: %v", role.Name, what, err)
		return nil, nil
	}
	if wantServers {
		var skipped []string
		servers, skipped = mcpServerNames(doc.MCPServers)
		for _, n := range skipped {
			m.logf("agents: %s: MCP server %q of the Codex config is no TOML bare key, so it stays on", role.Name, n)
		}
	}
	if wantPlugins {
		plugins = m.enabledPlugins(role, home, doc.Plugins)
	}
	return servers, plugins
}

// mcpServerNames reads the mcp_servers tables of a Codex config: the names
// of those that do not set enabled = false, sorted, and those of them that
// are no TOML bare key (mcpServerName), apart and sorted.
func mcpServerNames(tables map[string]any) (names, skipped []string) {
	for name, v := range tables {
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
	return names, skipped
}

// declaredMCPServers reads a Codex config (the operator's or a checkout's):
// the names of its mcp_servers tables that do not set enabled = false,
// sorted, and those of them that are no TOML bare key (mcpServerName),
// apart and sorted. Only the mcp_servers table is decoded.
func declaredMCPServers(data string) (names, skipped []string, err error) {
	var doc struct {
		MCPServers map[string]any `toml:"mcp_servers"`
	}
	if _, err := toml.Decode(data, &doc); err != nil {
		return nil, nil, err
	}
	names, skipped = mcpServerNames(doc.MCPServers)
	return names, skipped, nil
}

// enabledPlugins lists the Codex plugins a session of role would load, for
// codexOff: the plugins installed in home's plugins/cache and those the
// config's [plugins] tables declare, except those a table sets
// enabled = false; names that are no name@marketplace in bare keys stay
// on, with a log line.
func (m *Manager) enabledPlugins(role config.Role, home string, tables map[string]any) []string {
	on := map[string]bool{}
	var skipped []string
	for _, p := range installedCodexPlugins(home) {
		if pluginName.MatchString(p) {
			on[p] = true
		} else {
			skipped = append(skipped, p)
		}
	}
	declared, off := pluginTables(tables)
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

// pluginTables reads the [plugins] tables of a Codex config
// (name@marketplace): the names of those that do not set enabled = false,
// and those that do, both sorted.
func pluginTables(tables map[string]any) (on, off []string) {
	for name, v := range tables {
		if t, ok := v.(map[string]any); ok && t["enabled"] == false {
			off = append(off, name)
		} else {
			on = append(on, name)
		}
	}
	slices.Sort(on)
	slices.Sort(off)
	return on, off
}
