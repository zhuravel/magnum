// Package prompts embeds magnum's default prompt templates: the files of
// this directory that config.Role prompt names refer to (judge-*.md,
// claude-*.md, codex-*.sh) and model-fallback.md, the continuation after a
// session switched models on a per-model limit. The same files are also read
// from disk: a prompt name resolves to <pipeline.prompts_dir>/<name> when
// that file exists (prompts_dir defaults to this directory), else to the
// embedded copy of the same name (see config.Config.ResolvePrompt). The
// daemon reads them once, at startup (config.Config.SnapshotPrompts); the
// CLI whenever it resolves one. README.md documents the template variables
// and the role/kind configuration.
package prompts

import (
	"embed"
	"io/fs"
	"sort"
)

//go:embed judge-*.md claude-*.md codex-*.sh model-fallback.md
var embedded embed.FS

// FS holds the embedded default prompts, named as in this directory.
var FS fs.FS = embedded

// Names lists the embedded default prompts, sorted.
func Names() []string {
	ents, err := fs.ReadDir(embedded, ".")
	if err != nil {
		panic(err)
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
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
