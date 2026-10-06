package agents

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"

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
		m.logf("agents: %s: cannot read the Codex config, so its MCP servers stay on: %v", role.Name, err)
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
