// Package prompts embeds magnum's default prompt templates: the files of
// this directory that config.Role prompt names refer to (judge-*.md,
// claude-*.md, codex-*.sh, fix.md, the fix role's, whiteboard.md, the whiteboard role's), model-fallback.md, the continuation after a
// session switched models on a per-model limit, triage.md, the question a
// cheap model answers about which reviewers a small diff needs, retro.md,
// the classification of other reviewers' comments a retro asks for
// ([learn]), notes-curate.md, the curation of a repository's notes
// ([notes]), and skill-edit.md, the self-improving loop's edit of the judge
// skill and these prompts ([learn] editor_*). The same files are also read from disk: a prompt name
// resolves to <pipeline.prompts_dir>/<name> when that file exists
// (prompts_dir defaults to this directory), else to the embedded copy of
// the same name (see config.Config.ResolvePrompt). The daemon reads them
// once, at startup (config.Config.SnapshotPrompts); the CLI whenever it
// resolves one. README.md documents the template variables and the
// role/kind configuration. Shared, judge-shared.md, is no prompt of its own:
// it holds the blocks the judge prompts share, and every judge prompt is
// parsed with it (config.SharedPrompt).
package prompts

import (
	"embed"
	"io/fs"
	"sort"
)

// Shared is the judge prompts' partials file: the blocks two or more judge
// prompts (judge-*.md) share word for word, one {{define}} each, which a
// judge prompt includes with {{template}}. It is embedded with them but is
// not a prompt: Names leaves it out, Read returns it.
const Shared = "judge-shared.md"

//go:embed judge-*.md claude-*.md codex-*.sh fix.md whiteboard.md model-fallback.md triage.md retro.md notes-curate.md notes-critic.md skill-edit.md
var embedded embed.FS

// Names lists the embedded default prompts, sorted: every embedded file but
// the partials file (Shared).
func Names() []string {
	ents, err := fs.ReadDir(embedded, ".")
	if err != nil {
		panic(err)
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		if e.Name() != Shared {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// Read returns the embedded default prompt name; ok is false when there is
// none.
func Read(name string) (text string, ok bool) {
	b, err := fs.ReadFile(embedded, name)
	if err != nil {
		return "", false
	}
	return string(b), true
}
