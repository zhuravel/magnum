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
// limit when what it keeps helps future reviews. Curate lists what starts a
// curation besides `magnum notes <repo> --curate`: a repository marked past a
// limit, once a week, or a retro that recorded misses of the repository for
// its notes. The curator is an interactive agent like the retro's ([learn]):
// Kind, Model, Effort and Args as a role's (NotesRole).
type Notes struct {
	MaxBytes        int64 `toml:"max_bytes"`
	MaxLine         int   `toml:"max_line"`
	MaxHarnessFiles int   `toml:"max_harness_files"`
	MaxHarnessBytes int64 `toml:"max_harness_bytes"`
	// Curate lists the curation triggers (CurateOverLimit, CurateWeekly,
	// CurateMisses); empty: only on demand.
	Curate CurateTriggers `toml:"curate"`
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

// Curation triggers ([notes] curate).
const (
	CurateOverLimit = "over_limit" // a repository past a limit, once its notes changed since the last curation
	CurateWeekly    = "weekly"     // every repository with notes, once a week, once they changed
	CurateMisses    = "misses"     // a retro recorded misses of the repository for its notes (class miss, scope repo)
	// CurateOff, alone, is no trigger: only `magnum notes <repo> --curate`.
	CurateOff = "off"
)

// curateTriggers are the values a CurateTriggers list may hold.
var curateTriggers = []string{CurateOverLimit, CurateWeekly, CurateMisses}

// CurateTriggers is [notes] curate: the triggers of a curation, written as a
// list (`["over_limit", "misses"]`, `[]` for none). The earlier string form
// still reads as it meant: "over_limit" that trigger alone, "weekly"
// over_limit and weekly, "off" none.
type CurateTriggers []string

// UnmarshalTOML reads a list of trigger names or one of the earlier strings.
// Values are checked by Validate.
func (c *CurateTriggers) UnmarshalTOML(v any) error {
	switch x := v.(type) {
	case string:
		switch x {
		case CurateOff:
			*c = CurateTriggers{}
		case CurateWeekly:
			*c = CurateTriggers{CurateOverLimit, CurateWeekly}
		default:
			*c = CurateTriggers{x}
		}
		return nil
	case []any:
		out := CurateTriggers{}
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return fmt.Errorf("notes.curate: want a list of trigger names, got %v", e)
			}
			out = append(out, s)
		}
		if len(out) == 1 && out[0] == CurateOff {
			out = CurateTriggers{}
		}
		*c = out
		return nil
	}
	return fmt.Errorf("notes.curate: want a list of triggers such as [\"over_limit\", \"misses\"], got %v", v)
}

// Has reports whether trigger t is on.
func (c CurateTriggers) Has(t string) bool { return slices.Contains(c, t) }

// String is the list as the config writes it.
func (c CurateTriggers) String() string {
	if len(c) == 0 {
		return "[]"
	}
	return `["` + strings.Join(c, `", "`) + `"]`
}

// validate checks every trigger: a known one, listed once; "off" only alone.
func (c CurateTriggers) validate() []error {
	var errs []error
	for i, t := range c {
		switch {
		case t == CurateOff:
			errs = append(errs, errors.New(`notes.curate: "off" goes alone (or write [])`))
		case !slices.Contains(curateTriggers, t):
			errs = append(errs, fmt.Errorf(`notes.curate: %q is not a trigger (%s; [] or "off" for none)`, t, strings.Join(curateTriggers, ", ")))
		case slices.Contains(c[:i], t):
			errs = append(errs, fmt.Errorf("notes.curate: %q is listed twice", t))
		}
	}
	return errs
}

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
		Curate:          CurateTriggers{CurateOverLimit, CurateMisses},
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
// curate lists known triggers, each once.
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
	errs = append(errs, n.Curate.validate()...)
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
