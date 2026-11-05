package config

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/zhuravel/magnum/prompts"
)

// ErrPromptNotFound: a prompt name exists neither in prompts_dir nor among
// the embedded defaults.
var ErrPromptNotFound = errors.New("prompt not found")

// ErrSharedPrompt means a prompt name is a partials file (SharedPrompt, the
// judge prompts', or FactorySharedPrompt, the factory prompts'), which holds
// blocks for its prompts and is never a prompt of its own.
var ErrSharedPrompt = errors.New("holds the blocks its prompts share and is not a prompt of its own")

// SharedPrompt is the judge prompts' partials file (prompts.Shared): the
// blocks two or more judge prompts share word for word, one {{define}} each,
// which a judge prompt includes with {{template}}. Every judge prompt
// (UsesShared) is parsed with it, resolved as a prompt is: <prompts_dir>/
// judge-shared.md when that file exists, else the embedded copy, and in the
// daemon the copy its snapshot loaded at startup. ResolvePrompt refuses the
// name itself.
const SharedPrompt = prompts.Shared

// FactorySharedPrompt is the factory prompts' partials file
// (prompts.FactoryShared), as SharedPrompt is the judge prompts': the
// blocks the factory-*.md prompts share, the factory mode's rules among
// them. Every factory prompt is parsed with it, resolved as SharedPrompt
// is; ResolvePrompt refuses the name itself (ErrSharedPrompt).
const FactorySharedPrompt = prompts.FactoryShared

// UsesShared reports whether the prompt named name is a judge prompt
// (judge-*.md, the learn editor's glob), which is parsed with SharedPrompt;
// SharedPrompt itself is not.
func UsesShared(name string) bool {
	return strings.HasPrefix(name, "judge-") && strings.HasSuffix(name, ".md") && name != SharedPrompt
}

// PartialsOf is the partials file the prompt named name is parsed with:
// SharedPrompt for a judge prompt (UsesShared), FactorySharedPrompt for a
// factory prompt (factory-*.md), "" for any other prompt and for the two
// partials files themselves.
func PartialsOf(name string) string {
	switch {
	case UsesShared(name):
		return SharedPrompt
	case strings.HasPrefix(name, "factory-") && strings.HasSuffix(name, ".md") && name != FactorySharedPrompt:
		return FactorySharedPrompt
	}
	return ""
}

// isPartials reports whether name is one of the partials files, never a
// prompt of its own.
func isPartials(name string) bool { return name == SharedPrompt || name == FactorySharedPrompt }

// Prompt is a resolved prompt template.
type Prompt struct {
	Name     string // as configured, e.g. "judge-initial.md"
	Path     string // the file read (<prompts_dir>/<name>); "" when embedded
	Embedded bool   // the embedded default (package prompts) was used
	Text     string // the template source, unrendered
	// Shared is the source of the prompt's partials file (PartialsOf:
	// SharedPrompt for a judge prompt, FactorySharedPrompt for a factory
	// prompt) as resolved with the prompt, whose blocks the prompt's
	// {{template}} calls include; "" for a prompt that takes none.
	// ParseTemplate parses it first.
	Shared string
}

// ResolvePrompt reads a prompt by name: <pipeline.prompts_dir>/<name> when
// that file exists, else the embedded default of the same name; an unknown
// name wraps ErrPromptNotFound. Names are plain file names (no directories).
// A judge prompt (UsesShared) comes with the partials file (Prompt.Shared),
// resolved the same way, and a factory prompt with the factory's
// (PartialsOf); a partials file's own name wraps ErrSharedPrompt. In the
// daemon, which loaded its prompts at startup (SnapshotPrompts), a name the
// snapshot holds resolves to the text loaded then, whatever the file holds
// now, and so does the partials file.
func (c *Config) ResolvePrompt(name string) (Prompt, error) {
	if err := validPromptName(name); err != nil {
		return Prompt{}, err
	}
	if isPartials(name) {
		return Prompt{}, fmt.Errorf("prompt %s: %w", name, ErrSharedPrompt)
	}
	p, err := c.resolve(name)
	partials := PartialsOf(name)
	if err != nil || partials == "" {
		return p, err
	}
	shared, err := c.resolve(partials)
	if err != nil {
		return Prompt{}, fmt.Errorf("prompt %s: the shared blocks of %s: %w", name, partials, err)
	}
	p.Shared = shared.Text
	return p, nil
}

// resolve is ResolvePrompt's lookup of one file: the snapshot's copy, else
// the file on disk or the embedded default (readPrompt).
func (c *Config) resolve(name string) (Prompt, error) {
	if p, ok := c.snapshot.prompt(name); ok {
		return p, nil
	}
	p, _, err := c.readPrompt(name)
	return p, err
}

// ParseTemplate parses a prompt or command template the way magnum renders
// it (internal/agents.RenderPrompt), without executing it: no funcs,
// missingkey=error, and shared (a prompt's Prompt.Shared; "" for none)
// parsed first as its own template, named for its partials file
// (PartialsOf: SharedPrompt for a judge prompt, FactorySharedPrompt for a
// factory one), so that text's {{template}} calls find its blocks, a block
// text defines itself replaces the shared one, and an error in the shared
// text names the file it is in.
func ParseTemplate(name, text, shared string) (*template.Template, error) {
	t := template.New(name).Option("missingkey=error")
	if shared != "" {
		if _, err := t.New(cmp.Or(PartialsOf(name), SharedPrompt)).Parse(shared); err != nil {
			return nil, err
		}
	}
	return t.Parse(text)
}

// readPrompt is ResolvePrompt from disk and the embedded defaults, with
// the stamp of the file read (zero for an embedded default).
func (c *Config) readPrompt(name string) (Prompt, fileStamp, error) {
	if dir := c.Pipeline.PromptsDir; dir != "" && filepath.IsAbs(dir) {
		p := filepath.Join(dir, name)
		b, st, err := readStamped(p)
		switch {
		case err == nil:
			return Prompt{Name: name, Path: p, Text: string(b)}, st, nil
		case !errors.Is(err, fs.ErrNotExist):
			return Prompt{}, fileStamp{}, fmt.Errorf("prompt %s: %w", name, err)
		}
	}
	if text, ok := prompts.Read(name); ok {
		return Prompt{Name: name, Embedded: true, Text: text}, fileStamp{}, nil
	}
	return Prompt{}, fileStamp{}, fmt.Errorf("prompt %s: %w in %s or the embedded defaults", name, ErrPromptNotFound, c.Pipeline.PromptsDir)
}

// RolePrompt resolves the role's prompt of a prompt kind (see PromptFile);
// a role without one wraps ErrPromptNotFound.
func (c *Config) RolePrompt(r Role, kind string) (Prompt, error) {
	name := r.PromptFile(kind)
	if name == "" {
		return Prompt{}, fmt.Errorf("role %s has no %s prompt: %w", r.Name, kind, ErrPromptNotFound)
	}
	return c.ResolvePrompt(name)
}

func validPromptName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("prompt %q: want a plain file name in prompts_dir", name)
	}
	return nil
}

func (c *Config) promptExists(name string) bool {
	_, err := c.ResolvePrompt(name)
	return err == nil
}
