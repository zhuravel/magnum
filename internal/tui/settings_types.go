package tui

import (
	"context"
	"errors"
	"time"
)

// ErrSettingsChanged is a save over a file that changed on disk since the
// screen read it; a source wraps its own error in it, and the screen offers
// to read the file again and keep the staged edits.
var ErrSettingsChanged = errors.New("the config file changed on disk since it was read")

// SettingsSource is the config side of the settings screen: what the user
// config file says, the roles it resolves to, and the one path that writes
// it. The cli adapts internal/settings into it. Every value it hands over is
// display text; the screen never parses TOML.
type SettingsSource interface {
	// Load reads the user config as it is on disk now.
	Load(ctx context.Context) (*SettingsData, error)
	// Preview is the roles the daemon would run after a restart on the file
	// with d applied, computed by magnum's own loader; an error is the
	// loader's refusal of the candidate.
	Preview(ctx context.Context, d SettingsDraft) ([]SettingsRole, error)
	// Plan checks d against the file Load read (refusing a file that changed
	// on disk since) and returns the save it would make: the changed lines,
	// with a dated note on each when notes is true.
	Plan(ctx context.Context, d SettingsDraft, notes bool) (*SettingsPlan, error)
	// Commit writes p, keeping the previous file in the history. The screen
	// calls it only after the operator's y.
	Commit(ctx context.Context, p *SettingsPlan) (SettingsSaved, error)
	// RequestRestart asks the daemon to restart on the saved file at its
	// first idle moment; the text says what happens next.
	RequestRestart(ctx context.Context, s SettingsSaved) (string, error)
	// Restore is the plan that puts the n-th kept version back (1 = the
	// newest), a save like any other.
	Restore(ctx context.Context, n int) (*SettingsPlan, error)
}

// SettingsRuntime is the runtime side of the settings screen (applies now,
// never saved to the file): the pauses `magnum status` lists and the same
// requests or registry writes `magnum pause` and `magnum resume` make.
type SettingsRuntime interface {
	// Runtime reads the automation pause, each agent kind's pause and each
	// watch's identity-leak pause.
	Runtime(ctx context.Context) (RuntimeState, error)
	// Pause pauses automation as `magnum pause` does; the text is its answer.
	Pause(ctx context.Context, p PauseSpec) (string, error)
	// Resume lifts a pause as `magnum resume` does: automation (an empty
	// spec), an agent kind or all of them (Tool), or a watch (Watch).
	Resume(ctx context.Context, r ResumeSpec) (string, error)
}

// PauseSpec is `magnum pause`'s flags: For or Until (a local 15:30 or an
// RFC3339 time), neither for a pause until resumed.
type PauseSpec struct {
	For    time.Duration
	Until  string
	Reason string
}

// ResumeSpec is `magnum resume`'s flags: Tool is an agent kind or "all",
// Watch a watch owner; both empty resume automation.
type ResumeSpec struct {
	Tool  string
	Watch string
}

// RuntimeState is what the Runtime section shows.
type RuntimeState struct {
	// Daemon is true when a daemon runs; otherwise a change applies when it
	// starts.
	Daemon     bool
	Automation RuntimeEntry
	Kinds      []RuntimeEntry // one per agent kind
	Watches    []RuntimeEntry // one per watch owner
}

// RuntimeEntry is one pausable thing: automation, an agent kind or a watch.
type RuntimeEntry struct {
	Name   string // "automation", the kind, the watch owner
	Label  string // what its row says ("watch 1 talkable"); "" = Name
	Paused bool
	// State is the row's text: "running", "no pause", "paused: usage limit
	// until 07:50 · since 02:10".
	State string
	// Detail is the detail strip's line for it: why it is paused and what
	// lifts it by itself.
	Detail string
}

// SettingType is how a setting is edited.
type SettingType string

// The setting types; the list types (Logins, Repos, Roles, List) hold
// SettingField.List.
const (
	SettingEnum     SettingType = "enum"     // one of Options (a Select)
	SettingBool     SettingType = "bool"     // true or false (a Select)
	SettingDuration SettingType = "duration" // "15m", "2h" (an Input)
	SettingInt      SettingType = "int"      // a whole number (an Input)
	SettingPercent  SettingType = "percent"  // 0-100 (an Input)
	SettingText     SettingType = "text"     // free text, 300 characters at most (an Input)
	SettingModel    SettingType = "model"    // a model name: one of Options or another (a Select, then an Input)
	SettingEffort   SettingType = "effort"   // a reasoning effort, one of Options (a Select)
	SettingLogins   SettingType = "logins"   // GitHub logins, in order (the list editor)
	SettingRepos    SettingType = "repos"    // repository names, in order (the list editor)
	SettingRoles    SettingType = "roles"    // role names out of Options (a MultiSelect)
	SettingList     SettingType = "list"     // other strings, in order (the list editor)
)

// IsList reports whether t holds a list.
func (t SettingType) IsList() bool {
	switch t {
	case SettingLogins, SettingRepos, SettingRoles, SettingList:
		return true
	}
	return false
}

// SettingsData is one load of the user config.
type SettingsData struct {
	// File is the config file as the title names it
	// ("~/.config/magnum/config.toml").
	File string
	// HistoryDir is where a save keeps the previous file
	// ("~/.local/share/magnum/config-history/").
	HistoryDir string
	// RestartPending is true while the daemon runs an older file than this
	// one (the daemon's config hash differs from the file's).
	RestartPending bool
	// Sections are the rail's sections in order; Runtime, the screen's own,
	// comes first whether or not they name it.
	Sections []string
	Fields   []SettingField  // every editable setting, per scope instance
	Scopes   []SettingsScope // the watches, then the repositories
	Roles    []SettingsRole  // the roles as the saved file resolves them
	Warnings []string        // what the loader says of the saved file
}

// SettingsScope is a watch or a repository whose settings a section edits.
type SettingsScope struct {
	// ID names it in SettingField.Scope: "watch:1" (by position, from 1),
	// "repo:owner/name".
	ID    string
	Watch bool     // a [[watch]]; else a repository
	Index int      // a watch's position, from 1
	Name  string   // a watch's owner, a repository's owner/name
	Repos []string // a watch's repositories; the title shows the first ones
	// NoBlock is a watched repository the file has no [[repo]] block for: a
	// save of one of its settings creates a minimal block.
	NoBlock bool
}

// SettingField is one editable setting of one scope instance.
type SettingField struct {
	// Target is the draft's key: the setting and its scope instance, unique.
	Target  string
	Section string // the rail's section it lists under ("Spend & models")
	Scope   string // "" for a global setting, else a SettingsScope.ID
	Label   string
	// Key is the TOML path as the detail strip shows it ("[pipeline] spend",
	// "[spend.high].claude-simplify.model").
	Key         string
	Type        SettingType
	Options     []string          // the choices of an enum, effort, model or roles setting
	Notes       map[string]string // a note on an option ("burn": "starts from medium, then its table (7 roles)"; a role: "judge")
	Description string
	Default     string // what applies without the key ("medium", "inherit (high)")
	AppliesNow  bool   // applies at once; else after a daemon restart
	// Text and List are what the file says ("" and nil when it does not set
	// the key); Value is the value in effect as the row shows it.
	Text  string
	List  []string
	Value string
	Set   bool   // the file sets the key; else the default applies
	Line  int    // the key's line in the file (0 = not in it)
	From  string // where the value comes from ("your file, line 96", "built-in default")
	// Role, Level and Cell place a spend setting in the roles grid: the role,
	// its spend level ("" = the role's base [[role]] block) and the cell
	// ("model", "effort" or "rereview_effort").
	Role, Level, Cell string
	// Path is the setting's key path as magnum's loader names it in its
	// messages ("usage.codex_soft"), so a refusal marks its row.
	Path string
	// Inherits is the Target of the setting this one takes its value from
	// while unset ("inherit (high)" from [pipeline] spend), so the row
	// follows a staged edit of that one; "" for none.
	Inherits string
	// Validate checks a scalar value and ValidateList a list before the edit
	// is staged; nil accepts anything.
	Validate     func(string) error
	ValidateList func([]string) error
}

// SettingsEdit is one staged change: a value, a list, or back to the
// default (the key goes; its comment stays as a comment line).
type SettingsEdit struct {
	Text    string
	List    []string
	Default bool
}

// SettingsDraft is the staged changes by SettingField.Target.
type SettingsDraft map[string]SettingsEdit

// SettingsRole is one role as a config resolves it: its model and its
// first and re-review efforts under the spend level it follows.
type SettingsRole struct {
	Role, Kind, Model, First, Again, Level string
}

// SettingsPlan is the save of a draft (or a restore), checked and not yet
// written.
type SettingsPlan struct {
	Count    int            // the settings it changes
	Hunks    []SettingsHunk // the changed lines, file order
	Warnings []string       // what the loader says of the new file
	Notes    bool           // the changed lines carry dated notes
	Restore  int            // the kept version it puts back (0 = a save of the draft)
	// Opaque is the source's own plan, handed back to Commit.
	Opaque any
}

// SettingsHunk is one changed place of the file: its table heading, its
// line in the file as saved, and the lines it removes and adds.
type SettingsHunk struct {
	Table          string
	Line           int
	Removed, Added []string
}

// SettingsSaved is what a commit did.
type SettingsSaved struct {
	History string // the file that keeps the previous version
	Sum     string // the new file's hash
	// AfterRestart is true when the save changed settings the daemon reads
	// only when it starts.
	AfterRestart bool
	// Opaque is the source's own result, handed back to RequestRestart.
	Opaque any
}

// SettingsOptions tune the settings screen.
type SettingsOptions struct {
	// Scope opens the screen on a watch or a repository ("watch:2",
	// "repo:talkable/web"); "" opens the global settings.
	Scope string
	// Restore opens the save view of the n-th kept version (1 = the newest).
	Restore int
}

// SettingsScreen is what the dashboard and the PR board need to open the
// settings screen on `,`.
type SettingsScreen struct {
	Source  SettingsSource
	Runtime SettingsRuntime
	Options SettingsOptions
}
