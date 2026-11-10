package engine

import (
	"errors"
	"fmt"
	"maps"
	"testing"
)

// resolveConfigLinks follows a tree's links that steer the review agents
// as macOS's filesystem would: a link in a text's path before its "..",
// names in any case, both ways where links differ in case only, the links
// under a directory one leads to; each link passed through is a target
// too, a text that leaves the tree leads nowhere, a loop ends, and a link
// that steers nothing is not followed. Too many steps fail rather than
// leave a link out.
func TestResolveConfigLinksFollowsLinksAsTheFilesystemDoes(t *testing.T) {
	kinds := defaultKinds()
	isConfig := func(p string) (string, bool) { return agentConfigPath(kinds, p) }
	for _, c := range []struct {
		name  string
		links map[string]string
		want  map[string]string
	}{
		{"a root link", map[string]string{"CLAUDE.md": "docs/guide.md", "lib/x": "docs/other.md"},
			map[string]string{"CLAUDE.md": "CLAUDE.md", "docs/guide.md": "CLAUDE.md"}},
		{"a directory link before ..", map[string]string{"CLAUDE.md": "docs/../x.md", "docs": "a/b"},
			map[string]string{"CLAUDE.md": "CLAUDE.md", "docs": "CLAUDE.md", "a/x.md": "CLAUDE.md"}},
		{"out of the tree", map[string]string{"CLAUDE.md": "../../etc/passwd", ".claude/home": "/Users/x"},
			map[string]string{"CLAUDE.md": "CLAUDE.md", ".claude/home": ".claude/home"}},
		{"a loop", map[string]string{"AGENTS.md": "a", "a": "b", "b": "a"},
			map[string]string{"AGENTS.md": "AGENTS.md", "a": "AGENTS.md", "b": "AGENTS.md"}},
		{"names that differ in case", map[string]string{"CLAUDE.md": "docs/g.md", "Docs": "one", "docs": "two"},
			map[string]string{"CLAUDE.md": "CLAUDE.md", "Docs": "CLAUDE.md", "docs": "CLAUDE.md", "one/g.md": "CLAUDE.md", "two/g.md": "CLAUDE.md"}},
		{"links under a linked directory", map[string]string{".claude/skills": "../.agents/skills", ".agents/skills/lint": "../../tools/lint",
			".agents/other": "../elsewhere"},
			map[string]string{".claude/skills": ".claude/skills", ".agents/skills": ".claude/skills", ".agents/skills/lint": ".claude/skills",
				"tools/lint": ".claude/skills"}},
		{"a skill directory below the root", map[string]string{"apps/web/.claude/skills": "../../../shared/skills"},
			map[string]string{"apps/web/.claude/skills": "apps/web/.claude/skills", "shared/skills": "apps/web/.claude/skills"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveConfigLinks(c.links, isConfig)
			if err != nil || !maps.Equal(got, c.want) {
				t.Fatalf("resolveConfigLinks = %q, %v\nwant %q", got, err, c.want)
			}
		})
	}

	many := map[string]string{}
	for i := range 400 {
		many[fmt.Sprintf(".claude/l%d", i)] = fmt.Sprintf("../d%d", i)
		many[fmt.Sprintf("x%d", i)] = "y"
	}
	if _, err := resolveConfigLinks(many, isConfig); !errors.Is(err, errLinksTooMany) {
		t.Fatalf("a tree whose links take more than %d steps: %v, want errLinksTooMany", maxLinkSteps, err)
	}
}
