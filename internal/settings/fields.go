// Package settings is the core of the settings screen (`magnum settings`):
// the typed allowlist of the settings it may change (Fields), what the user
// config says for each (Snapshot), the roles a draft resolves to after a
// restart (Preview), and the save: the draft becomes in-place edits of
// ~/.config/magnum/config.toml (internal/confedit), checked as `magnum
// config` checks a file, shown as a diff (Plan), then written atomically with
// the previous file kept in the config history (Commit), and the daemon
// asked to restart on it when idle (RequestRestart). Identities,
// auto_approve_as, keys, paths, commands, args, env, hooks, MCP, the sandbox
// and pools are never in the allowlist: they are edited by hand.
package settings

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/config"
)

// FieldID names a setting: a template's ("pipeline.spend", "watch.roles",
// "spend.model") or a concrete grid or kind field's
// ("spend.high.claude-review.model", "role.claude-review.model",
// "kinds.codex.spend").
type FieldID string

// Scope is what a setting belongs to.
type Scope int

// The scopes.
const (
	ScopeGlobal     Scope = iota // a key of a [section]
	ScopeWatch                   // a key of a [[watch]] (Target.Watch picks it)
	ScopeRepo                    // a key of a [[repo]] (Target.Repo picks it)
	ScopeRole                    // a key of a [[role]] (Field.Role)
	ScopeSpendLevel              // a role's key in a [spend.<level>] table (Field.Level, Field.Role)
	ScopeKind                    // a key of a [kinds.<name>] table (Field.Kind)
)

// Type is a setting's value type: how its text is read, checked and
// written.
type Type int

// The types.
const (
	TypeEnum     Type = iota // one of Options
	TypeBool                 // "true" or "false"
	TypeDuration             // Go syntax with days: "15m", "2h", "1d"
	TypeInt
	TypePercent // 0-100, a fraction allowed
	TypeLogins  // a list of GitHub logins
	TypeRepos   // a list of repository names (no owner)
	TypeRoles   // a list of role names
	TypeText    // one line
	TypeModel   // a model name the agent CLI takes
	TypeEffort  // an effort level the agent CLI takes
	TypeChecks  // a list of required check names (workflow:<glob> for a workflow)
)

// IsList reports whether values of t are lists (DraftValue.List).
func (t Type) IsList() bool {
	return t == TypeLogins || t == TypeRepos || t == TypeRoles || t == TypeChecks
}

// Applies is when a saved value takes effect.
type Applies int

// When a value applies.
const (
	AppliesAfterRestart Applies = iota // the daemon reads the config at start
	AppliesNow
)

// The sections, in the order the screen lists them (its Runtime section is
// not config and is the screen's own).
const (
	SectionSpend    = "Spend & models"
	SectionCaps     = "Usage caps"
	SectionWatches  = "Watches"
	SectionRepos    = "Repositories"
	SectionSchedule = "Schedule"
	SectionReviews  = "Reviews & agents"
)

// Sections lists the sections in order.
func Sections() []string {
	return []string{SectionSpend, SectionCaps, SectionWatches, SectionRepos, SectionSchedule, SectionReviews}
}

// Grid cells of a role in a [spend.<level>] table or its [[role]] block.
const (
	CellModel          = "model"
	CellEffort         = "effort"
	CellRereviewEffort = "rereview_effort"
)

// Field is one setting.
type Field struct {
	ID      FieldID
	Section string
	Label   string
	// Key is the TOML key path, a template for the scoped ones:
	// "pipeline.spend", "kinds.<kind>.spend", "spend.<level>.<role>.model",
	// "role.<role>.model", "watch.roles", "repo.stakes".
	Key   string
	Scope Scope
	Type  Type
	// Role, Level and Cell place a grid cell (ScopeSpendLevel, ScopeRole):
	// Level "" is the role's base model in its [[role]] block. Kind is the
	// agent kind of a [kinds.<kind>] spend. All four are "" in a template.
	Role, Level, Cell, Kind string
	// Options are an enum's members when they are fixed; Snapshot.Options
	// computes the others (spend levels, roles, models seen in the config).
	Options      []string
	Validate     func(string) error   // a scalar's own check (nil: none)
	ValidateList func([]string) error // a list's own check (nil: none)
	Description  string
	DefaultText  string // the built-in default, as the screen shows it
	Applies      Applies
}

// Efforts are the effort levels magnum's levels use (the agent CLIs take
// these).
var Efforts = []string{"low", "medium", "high", "xhigh"}

// Fields is the allowlist: every setting of the screen's v1, in screen
// order, the role grid and the kind spends as templates (Role, Level and
// Kind empty; Snapshot.Fields expands them for a configuration).
func Fields() []Field {
	return []Field{
		// Spend & models.
		{ID: "pipeline.spend", Section: SectionSpend, Label: "spend level", Key: "pipeline.spend", Type: TypeEnum,
			Description: "the level every role's model and effort follows", DefaultText: config.SpendMedium},
		{ID: "kinds.spend", Section: SectionSpend, Label: "<kind> spend", Key: "kinds.<kind>.spend", Scope: ScopeKind, Type: TypeEnum,
			Description: "the level this agent kind's roles follow instead of [pipeline] spend", DefaultText: "inherit ([pipeline] spend)"},
		{ID: "spend.model", Section: SectionSpend, Label: "<role> model", Key: "spend.<level>.<role>.model", Scope: ScopeSpendLevel,
			Type: TypeModel, Cell: CellModel, Validate: validateWord,
			Description: "the model the role runs while its spend level is this one", DefaultText: "the role's base model"},
		{ID: "spend.effort", Section: SectionSpend, Label: "<role> effort", Key: "spend.<level>.<role>.effort", Scope: ScopeSpendLevel,
			Type: TypeEffort, Cell: CellEffort, Validate: validateWord,
			Description: "the role's effort on a first review while its spend level is this one", DefaultText: "the role's own, after the level's notch"},
		{ID: "spend.rereview_effort", Section: SectionSpend, Label: "<role> re-review effort", Key: "spend.<level>.<role>.rereview_effort",
			Scope: ScopeSpendLevel, Type: TypeEffort, Cell: CellRereviewEffort, Validate: validateWord,
			Description: "the role's effort on a re-review while its spend level is this one", DefaultText: "the role's own, after the level's notch"},
		{ID: "role.model", Section: SectionSpend, Label: "<role> base model", Key: "role.<role>.model", Scope: ScopeRole,
			Type: TypeModel, Cell: CellModel, Validate: validateWord,
			Description: "the model the role runs under a level whose table does not name it", DefaultText: "the kind's default model, else the CLI's"},

		// Usage caps.
		{ID: "usage.codex_soft", Section: SectionCaps, Label: "Codex soft cap", Key: "usage.codex_soft", Type: TypePercent,
			Validate: validatePercent, DefaultText: "80",
			Description: "from this % of the Codex week full re-reviews wait (0 = off); below the hard cap"},
		{ID: "usage.codex_hard", Section: SectionCaps, Label: "Codex hard cap", Key: "usage.codex_hard", Type: TypePercent,
			Validate: validatePercent, DefaultText: "95",
			Description: "from this % of the Codex week every round that needs Codex waits (0 = off)"},

		// Watches.
		{ID: "watch.roles", Section: SectionWatches, Label: "roles", Key: "watch.roles", Scope: ScopeWatch, Type: TypeRoles,
			ValidateList: validateNames, DefaultText: "every round role, the first judge",
			Description: "the roles that review this watch's PRs: exactly one judge"},
		{ID: "watch.auto_approve", Section: SectionWatches, Label: "auto-approve repos", Key: "watch.auto_approve", Scope: ScopeWatch,
			Type: TypeRepos, ValidateList: validateRepoNames, DefaultText: "none",
			Description: "repositories on whose PRs magnum posts YOUR approval once its review leaves nothing to fix (* = all)"},
		{ID: "watch.auto_approve_authors", Section: SectionWatches, Label: "auto-approve authors", Key: "watch.auto_approve_authors",
			Scope: ScopeWatch, Type: TypeLogins, ValidateList: validateLogins, DefaultText: "every author",
			Description: "only these authors' PRs are auto-approved (allowlist); a [[repo]] list replaces it, skip authors still win"},
		{ID: "watch.auto_approve_skip_authors", Section: SectionWatches, Label: "skip authors", Key: "watch.auto_approve_skip_authors",
			Scope: ScopeWatch, Type: TypeLogins, ValidateList: validateLogins, DefaultText: "none",
			Description: "authors whose PRs are never auto-approved (blocklist)"},
		{ID: "watch.manual_repos", Section: SectionWatches, Label: "manual repos", Key: "watch.manual_repos", Scope: ScopeWatch,
			Type: TypeRepos, ValidateList: validateRepoNames, DefaultText: "none",
			Description: "repositories shown on the board but reviewed only when asked"},
		{ID: "watch.ignore_checks", Section: SectionWatches, Label: "ignored checks", Key: "watch.ignore_checks", Scope: ScopeWatch,
			Type: TypeChecks, ValidateList: validateChecks, DefaultText: "none",
			Description: "checks magnum treats as absent wherever it reads CI (workflow:<glob> names a workflow)"},
		{ID: "watch.include_drafts", Section: SectionWatches, Label: "include drafts", Key: "watch.include_drafts", Scope: ScopeWatch,
			Type: TypeBool, Options: []string{"true", "false"}, DefaultText: "false",
			Description: "false: a draft waits until it is ready or a review is requested"},
		{ID: "watch.stakes", Section: SectionWatches, Label: "stakes", Key: "watch.stakes", Scope: ScopeWatch, Type: TypeText,
			Validate: validateStakes, DefaultText: "none",
			Description: "what the repositories put at risk; the judge sizes harm against it (a [[repo]] stakes wins)"},
		{ID: "watch.own_min_rereview_interval", Section: SectionWatches, Label: "own re-review interval",
			Key: "watch.own_min_rereview_interval", Scope: ScopeWatch, Type: TypeDuration, Validate: validateDuration,
			DefaultText: "inherit ([daemon] own_min_rereview_interval)",
			Description: "least time between rounds on your own PRs in this watch"},

		// Repositories.
		{ID: "repo.stakes", Section: SectionRepos, Label: "stakes", Key: "repo.stakes", Scope: ScopeRepo, Type: TypeText,
			Validate: validateStakes, DefaultText: "inherit (the watch's)",
			Description: "what the repository puts at risk (wins over the watch's)"},
		{ID: "repo.max_simplifications", Section: SectionRepos, Label: "max simplifications", Key: "repo.max_simplifications",
			Scope: ScopeRepo, Type: TypeInt, Validate: validateInt(0, config.MaxSimplificationsMax),
			DefaultText: "inherit ([pipeline] max_simplifications)",
			Description: "simplifications the judge posts at most on this repository's PRs (0 = [pipeline]'s)"},
		{ID: "repo.auto_approve_authors", Section: SectionRepos, Label: "auto-approve authors", Key: "repo.auto_approve_authors",
			Scope: ScopeRepo, Type: TypeLogins, ValidateList: validateLogins, DefaultText: "inherit (the watch's list)",
			Description: "only these authors' PRs are auto-approved here; replaces the watch's list"},
		{ID: "repo.auto_approve_skip_authors", Section: SectionRepos, Label: "skip authors", Key: "repo.auto_approve_skip_authors",
			Scope: ScopeRepo, Type: TypeLogins, ValidateList: validateLogins, DefaultText: "none (adds to the watch's)",
			Description: "authors whose PRs are never auto-approved here; adds to the watch's list"},
		{ID: "repo.allow_bot_authors", Section: SectionRepos, Label: "allowed bot authors", Key: "repo.allow_bot_authors",
			Scope: ScopeRepo, Type: TypeLogins, ValidateList: validateLogins, DefaultText: "none (adds to the watch's)",
			Description: "bots whose PRs are reviewed although bots are skipped"},
		{ID: "repo.required_checks", Section: SectionRepos, Label: "required checks", Key: "repo.required_checks", Scope: ScopeRepo,
			Type: TypeChecks, ValidateList: validateChecks, DefaultText: "GitHub's required checks",
			Description: "checks that must pass before magnum approves (workflow:<glob> names a workflow)"},
		{ID: "repo.ignore_checks", Section: SectionRepos, Label: "ignored checks", Key: "repo.ignore_checks", Scope: ScopeRepo,
			Type: TypeChecks, ValidateList: validateChecks, DefaultText: "none (adds to the watch's)",
			Description: "checks magnum treats as absent wherever it reads CI here; adds to the watch's list"},

		// Schedule.
		{ID: "daemon.quiet_hours", Section: SectionSchedule, Label: "quiet hours", Key: "daemon.quiet_hours", Type: TypeText,
			Validate: validateQuietHours, DefaultText: "none",
			Description: "local HH:MM-HH:MM in which first reviews and full re-reviews wait (\"\" = none)"},
		{ID: "daemon.push_quiet_period", Section: SectionSchedule, Label: "push quiet period", Key: "daemon.push_quiet_period",
			Type: TypeDuration, Validate: validateDuration, DefaultText: "5m",
			Description: "a new head is reviewable after no push for this long (0 = at once)"},
		{ID: "daemon.min_rereview_interval", Section: SectionSchedule, Label: "re-review interval", Key: "daemon.min_rereview_interval",
			Type: TypeDuration, Validate: validateDuration, DefaultText: "30m",
			Description: "least time between rounds on a PR"},
		{ID: "daemon.own_min_rereview_interval", Section: SectionSchedule, Label: "own re-review interval",
			Key: "daemon.own_min_rereview_interval", Type: TypeDuration, Validate: validateDuration, DefaultText: "0 (the re-review interval)",
			Description: "least time between rounds on your own PRs (0 = the re-review interval)"},

		// Reviews & agents.
		{ID: "pipeline.status_comment", Section: SectionReviews, Label: "status comment", Key: "pipeline.status_comment", Type: TypeBool,
			Options: []string{"true", "false"}, DefaultText: "false",
			Description: "one public comment per PR, as the posting identity, edited as the review goes"},
		{ID: "pipeline.whiteboard", Section: SectionReviews, Label: "whiteboards", Key: "pipeline.whiteboard", Type: TypeBool,
			Options: []string{"true", "false"}, DefaultText: "false",
			Description: "a Whiteboard of every PR after each posted review"},
		{ID: "pipeline.judge_fallback", Section: SectionReviews, Label: "fallback judge", Key: "pipeline.judge_fallback", Type: TypeEnum,
			DefaultText: config.DefaultJudgeFallback + " (off where it does not fit)",
			Description: "the judge that takes over a PR the watch's judge refused (\"\" = none: such a PR is never reviewed again)"},
		{ID: "pipeline.fix_cycles", Section: SectionReviews, Label: "fix cycles", Key: "pipeline.fix_cycles", Type: TypeInt,
			Validate: validateInt(1, math.MaxInt32), DefaultText: strconv.Itoa(config.DefaultFixCycles),
			Description: "fix runs one fix loop may take before the PR needs you"},
		{ID: "pipeline.autofix", Section: SectionReviews, Label: "autofix", Key: "pipeline.autofix", Type: TypeEnum,
			Options: []string{config.AutofixNever, config.AutofixOwn}, DefaultText: config.AutofixNever,
			Description: "own: a fix loop starts by itself after a review with findings on your own PR"},
		{ID: "pipeline.max_simplifications", Section: SectionReviews, Label: "max simplifications", Key: "pipeline.max_simplifications",
			Type: TypeInt, Validate: validateInt(1, config.MaxSimplificationsMax), DefaultText: strconv.Itoa(config.DefaultMaxSimplifications),
			Description: "simplifications the judge posts at most"},
		{ID: "daemon.park_idle_after", Section: SectionReviews, Label: "park idle agents after", Key: "daemon.park_idle_after",
			Type: TypeDuration, Validate: validateParkIdle, DefaultText: "15m",
			Description: "an idle agent parks after this (0 = never); a judge waits judge_fresh_after when longer"},
		{ID: "pipeline.judge_fresh_after", Section: SectionReviews, Label: "fresh judge after", Key: "pipeline.judge_fresh_after",
			Type: TypeDuration, Validate: validateDuration, DefaultText: "1h30m",
			Description: "a judge idle longer starts the next round fresh (0 = always resume)"},
		{ID: "daemon.max_concurrent_reviews", Section: SectionReviews, Label: "concurrent reviews", Key: "daemon.max_concurrent_reviews",
			Type: TypeInt, Validate: validateInt(1, math.MaxInt32), DefaultText: "3",
			Description: "review rounds running at once"},
		{ID: "daemon.max_total_working_codex", Section: SectionReviews, Label: "working Codex agents", Key: "daemon.max_total_working_codex",
			Type: TypeInt, Validate: validateInt(0, math.MaxInt32), DefaultText: "5",
			Description: "working Codex agents at once, your own included (0 = no cap)"},
	}
}

func validateWord(s string) error {
	if s == "" || strings.ContainsFunc(s, unicode.IsSpace) || strings.ContainsFunc(s, unicode.IsControl) {
		return errors.New("must be one word")
	}
	return nil
}

func validatePercent(s string) error {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || v < 0 || v > 100 {
		return errors.New("must be a percentage from 0 (off) to 100")
	}
	return nil
}

func validateInt(lo, hi int) func(string) error {
	return func(s string) error {
		n, err := strconv.Atoi(s)
		if err != nil || n < lo || n > hi {
			if hi >= math.MaxInt32 {
				return fmt.Errorf("must be a whole number of at least %d", lo)
			}
			return fmt.Errorf("must be a whole number from %d to %d", lo, hi)
		}
		return nil
	}
}

func validateDuration(s string) error {
	d, err := config.ParseDuration(s)
	if err != nil || d < 0 {
		return errors.New(`must be a duration such as "15m", "2h" or "1d" (0 = none)`)
	}
	return nil
}

func validateParkIdle(s string) error {
	if err := validateDuration(s); err != nil {
		return err
	}
	if d, _ := config.ParseDuration(s); d != 0 && d.Minutes() < 1 {
		return errors.New("must be 0 (never) or at least 1m")
	}
	return nil
}

func validateStakes(s string) error {
	if n := utf8.RuneCountInString(s); n > config.StakesMax {
		return fmt.Errorf("must be at most %d characters (it has %d)", config.StakesMax, n)
	}
	if strings.ContainsFunc(s, unicode.IsControl) {
		return errors.New("must be one line")
	}
	return nil
}

func validateQuietHours(s string) error {
	if _, _, err := config.ParseQuietHours(s); err != nil {
		return fmt.Errorf("must be local HH:MM-HH:MM, or empty for none: %w", err)
	}
	return nil
}

// validateEach checks every entry of a list: none empty, none with a
// space, none twice.
func validateEach(what string, check func(string) error) func([]string) error {
	return func(list []string) error {
		seen := map[string]bool{}
		for _, s := range list {
			if s == "" || strings.ContainsFunc(s, unicode.IsSpace) || strings.ContainsFunc(s, unicode.IsControl) {
				return fmt.Errorf("%s %q must be one word", what, s)
			}
			if seen[strings.ToLower(s)] {
				return fmt.Errorf("%s %q is listed twice", what, s)
			}
			seen[strings.ToLower(s)] = true
			if check != nil {
				if err := check(s); err != nil {
					return err
				}
			}
		}
		return nil
	}
}

var (
	validateNames  = validateEach("role", nil)
	validateLogins = validateEach("login", func(s string) error {
		if strings.HasPrefix(strings.ToLower(s), "app/") {
			return fmt.Errorf("login %q: write an App's login as <name>[bot], not app/<name>", s)
		}
		return nil
	})
	validateRepoNames = validateEach("repository", func(s string) error {
		if strings.Contains(s, "/") {
			return fmt.Errorf("repository %q: a name without the owner", s)
		}
		return nil
	})
	validateChecks = func(list []string) error {
		for _, s := range list {
			if strings.TrimSpace(s) == "" || strings.ContainsFunc(s, unicode.IsControl) {
				return errors.New("a check name must not be empty")
			}
		}
		return nil
	}
)
