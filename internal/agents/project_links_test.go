package agents

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store"
)

// A Claude decline records the targets of the checkout's root links into it
// (CLAUDE.md -> docs/guide.md) with the label of the link's path, read from
// the disk, not from git, so also when git could not compare: a change of
// such a target is the one change of the agents' config that a PR's file
// list names by another path, and the auto-approval's gate matches the
// targets against that list (ChangedLinks) instead of trusting git's compare
// of the slot. The event leaves them out (a link the PR added names a path
// it chose).
func TestAClaudeDeclineRecordsTheCheckoutsRootLinkTargets(t *testing.T) {
	for _, c := range []struct {
		name string
		fail bool
	}{{"git compared", false}, {"git could not compare", true}} {
		t.Run(c.name, func(t *testing.T) {
			e, dir, ws := projectEnv(t, map[string]string{".claude/settings.json": "{}", "docs/guide.md": "x\n"}, []string{"docs/guide.md"}, nil)
			if err := os.Symlink("docs/guide.md", filepath.Join(dir, "CLAUDE.md")); err != nil {
				t.Fatal(err)
			}
			if c.fail {
				rule := execx.Rule{Prefix: []string{"git", "-C", dir, "merge-base"}, Err: errors.New("git broke")}
				e.run.Rules = append([]execx.Rule{rule}, e.run.Rules...)
			}
			if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], ""); err != nil {
				t.Fatal(err)
			}
			n, ok := ProjectDeclined(e.ctx, e.st, e.pr.ID, KindClaude)
			if raw, _, _ := e.st.GetKV(e.ctx, store.KVPRProject(e.pr.ID, KindClaude)); !ok || n.Compared == c.fail ||
				!maps.Equal(n.Links, map[string]string{"docs/guide.md": "CLAUDE.md"}) {
				t.Fatalf("record %s: want the link's target docs/guide.md, for CLAUDE.md", raw)
			}
			if got := n.ChangedLinks([]string{"app/x.rb", "docs/guide.md"}); !slices.Equal(got, []string{"CLAUDE.md"}) {
				t.Fatalf("ChangedLinks of the target = %q, want CLAUDE.md", got)
			}
			if got := n.ChangedLinks([]string{"app/x.rb", "docs/guide.md.bak", "docs/guide.mdx", "docs"}); len(got) != 0 {
				t.Fatalf("ChangedLinks of files beside the target = %q, want none", got)
			}
			for _, ev := range claudeProjectEvents(t, e) {
				var data map[string]any
				if err := json.Unmarshal(ev.Data, &data); err != nil {
					t.Fatal(err)
				}
				if _, named := data["links"]; named {
					t.Fatalf("the event names the link's target: %s", ev.Data)
				}
			}
		})
	}
}

// ChangedLinks names each link whose target, a file or a directory, the
// file list changes, once, in the order of the targets; a record without
// links (an older build's) changes none.
func TestChangedLinksNamesTheLinksWhoseTargetsTheFilesChange(t *testing.T) {
	n := ProjectNote{Links: map[string]string{"docs/guide.md": "CLAUDE.md", "shared/claude": ".claude/", "docs/local.md": "CLAUDE.md"}}
	for _, c := range []struct {
		name  string
		files []string
		want  []string
	}{
		{"a file under a linked directory", []string{"shared/claude/settings.json"}, []string{".claude/"}},
		{"two targets of one label", []string{"docs/local.md", "docs/guide.md"}, []string{"CLAUDE.md"}},
		{"both", []string{"shared/claude/skills/x/SKILL.md", "docs/guide.md"}, []string{"CLAUDE.md", ".claude/"}},
		{"a sibling of the directory", []string{"shared/claude.json", "shared/claudex/a"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := n.ChangedLinks(c.files); !slices.Equal(got, c.want) {
				t.Fatalf("ChangedLinks(%q) = %q, want %q", c.files, got, c.want)
			}
		})
	}
	if got := (ProjectNote{}).ChangedLinks([]string{"docs/guide.md"}); got != nil {
		t.Fatalf("a record without links: %q", got)
	}
}
