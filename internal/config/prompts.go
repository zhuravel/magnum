package config

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/zhuravel/magnum/prompts"
)

// ErrPromptNotFound: a prompt name exists neither in prompts_dir nor among
// the embedded defaults.
var ErrPromptNotFound = errors.New("prompt not found")

// Prompt is a resolved prompt template.
type Prompt struct {
	Name     string // as configured, e.g. "judge-initial.md"
	Path     string // the file read (<prompts_dir>/<name>); "" when embedded
	Embedded bool   // the embedded default (package prompts) was used
	Text     string // the template source, unrendered
}

// ResolvePrompt reads a prompt by name: <pipeline.prompts_dir>/<name> when
// that file exists, else the embedded default of the same name; an unknown
// name wraps ErrPromptNotFound. Names are plain file names (no directories).
// In the daemon, which loaded its prompts at startup (SnapshotPrompts), a
// name the snapshot holds resolves to the text loaded then, whatever the
// file holds now.
func (c *Config) ResolvePrompt(name string) (Prompt, error) {
	if err := validPromptName(name); err != nil {
		return Prompt{}, err
	}
	if p, ok := c.snapshot.prompt(name); ok {
		return p, nil
	}
	p, _, err := c.readPrompt(name)
	return p, err
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
