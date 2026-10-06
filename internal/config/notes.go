package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Notes is the [notes] section: the repository notes every review role reads
// first and the judge rewrites (engine.NotesPath). The four limits are
// curation triggers, not caps: after each judge round (and at startup) magnum
// measures the notes and the harness directory of QA scripts beside them, and
// a repository past any limit is marked for curation (a notes.over_limit
// event). Nothing blocks a review, and a curated proposal may stay above a
// limit when what it keeps helps future reviews. Curate says when a curation
// runs: for a marked repository, also once a week, or only on demand (`magnum
// notes <repo> --curate`). The curator is an interactive agent like the
// retro's ([learn]): Kind, Model, Effort and Args as a role's (NotesRole).
type Notes struct {
	MaxBytes        int64  `toml:"max_bytes"`
	MaxLine         int    `toml:"max_line"`
	MaxHarnessFiles int    `toml:"max_harness_files"`
	MaxHarnessBytes int64  `toml:"max_harness_bytes"`
	Curate          string `toml:"curate"`
	// Kind is the curator's [kinds.<name>]; a config that names a kind but no
	// model gets DefaultLearnModel(kind), as [learn] does.
	Kind   string   `toml:"kind"`
	Model  string   `toml:"model"`
	Effort string   `toml:"effort"`
	Args   []string `toml:"args"`
	// Prompt is the curator's template file (Config.ResolvePrompt).
	Prompt string `toml:"prompt"`
	// Timeout bounds the curator's turn.
	Timeout Duration `toml:"timeout"`
}

// Curation schedules ([notes] curate).
const (
	CurateOverLimit = "over_limit" // a repository past a limit, once its notes changed since the last curation
	CurateWeekly    = "weekly"     // that, and every repository with notes once a week
	CurateOff       = "off"        // only `magnum notes <repo> --curate`
)

// DefaultNotesPrompt is the curator's prompt file.
const DefaultNotesPrompt = "notes-curate.md"

// NotesRoleName is the name of the role the curator runs as (NotesRole).
const NotesRoleName = "notes"

// DefaultNotes returns the built-in [notes] values.
func DefaultNotes() Notes {
	return Notes{
		MaxBytes:        16384,
		MaxLine:         300,
		MaxHarnessFiles: 15,
		MaxHarnessBytes: 131072,
		Curate:          CurateOverLimit,
		Kind:            KindClaude,
		Model:           DefaultLearnModel(KindClaude),
		Prompt:          DefaultNotesPrompt,
		Timeout:         Duration{30 * time.Minute},
	}
}

// notesModelFollowsKind is learnModelFollowsKind for [notes].
func (c *Config) notesModelFollowsKind(md toml.MetaData) {
	if md.IsDefined("notes", "kind") && !md.IsDefined("notes", "model") {
		c.Notes.Model = DefaultLearnModel(c.Notes.Kind)
	}
}

// NotesRole is the role the curator runs as: NotesRoleName, the [notes] kind
// in session mode with its model, effort, args, prompt and timeout,
// normalized like a configured role.
func (c *Config) NotesRole() Role {
	n := c.Notes
	r := Role{
		Name: NotesRoleName, Kind: n.Kind, Mode: ModeSession,
		Model: n.Model, Effort: n.Effort, Args: n.Args,
		Prompt: n.Prompt, Capture: CaptureFile, Output: "changes.json",
		Timeout: n.Timeout,
	}
	return c.normalizedRoles([]Role{r})[0]
}

// validateNotes checks [notes] always (`magnum notes --curate` runs whatever
// curate says): the limits are positive, curate is one of its values, the
// kind is a declared agent kind whose role (NotesRole) passes a [[role]]'s
// checks, the prompt resolves and parses and the timeout is positive.
func (c *Config) validateNotes() []error {
	n := c.Notes
	var errs []error
	for _, l := range []struct {
		key string
		v   int64
	}{{"max_bytes", n.MaxBytes}, {"max_line", int64(n.MaxLine)}, {"max_harness_files", int64(n.MaxHarnessFiles)}, {"max_harness_bytes", n.MaxHarnessBytes}} {
		if l.v <= 0 {
			errs = append(errs, fmt.Errorf("notes.%s must be positive, got %d", l.key, l.v))
		}
	}
	if !slices.Contains([]string{CurateOverLimit, CurateWeekly, CurateOff}, n.Curate) {
		errs = append(errs, fmt.Errorf("notes.curate must be over_limit, weekly or off, got %q", n.Curate))
	}
	if _, ok := c.KindSpec(n.Kind); !ok {
		errs = append(errs, fmt.Errorf("notes.kind %q is not a declared agent kind (declared: %s)", n.Kind, strings.Join(c.KindNames(), ", ")))
	} else {
		r := c.NotesRole()
		r.Prompt, r.Rereview, r.Timeout = "", "", Duration{time.Minute}
		for _, err := range c.validateRole(r, c.kinds(), nil) {
			errs = append(errs, errors.New("notes: "+strings.TrimPrefix(err.Error(), "role "+NotesRoleName+": ")))
		}
	}
	if n.Prompt == "" {
		errs = append(errs, errors.New("notes.prompt must name a prompt file"))
	} else if p, err := c.ResolvePrompt(n.Prompt); err != nil {
		errs = append(errs, fmt.Errorf("notes.prompt: %w", err))
	} else if err := parseTemplate(n.Prompt, p.Text); err != nil {
		errs = append(errs, fmt.Errorf("notes.prompt %s: %w", n.Prompt, err))
	}
	if n.Timeout.Duration <= 0 {
		errs = append(errs, fmt.Errorf("notes.timeout must be positive, got %s", n.Timeout.Duration))
	}
	return errs
}
