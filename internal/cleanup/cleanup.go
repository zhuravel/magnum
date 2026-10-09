// Package cleanup builds and applies storage cleanup plans: releasing the
// slots of closed PRs, removing per-PR worktrees and surplus pool slots,
// dropping orphan per-worktree databases and resetting a manual worktree
// (talkable.repoN). The daemon and the CLI share it: the daemon applies the
// default plan every tick, the CLI prints a plan (Render) and applies it after
// confirmation.
//
// Plan is read-only: it reads the registry, one inventory scan and, for the
// directories it would delete, `du -sk`. Apply re-checks every action against
// the current registry (a plan may be minutes old by the time a human
// confirms it), scans the inventory again before a database drop or an
// external reset, executes it through the slots manager, MySQL or git,
// writes an audit event per action and keeps going past failures. Nothing
// is dropped or reset on incomplete facts (a source that could not be read).
package cleanup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/store"
)

// Action kinds.
const (
	KindRelease        = "release"         // pool slot back to the pool (placeholder reset, schema reload)
	KindRemoveSlot     = "remove_slot"     // pool slot torn down (teardown, databases, worktree, branch)
	KindRemoveWorktree = "remove_worktree" // per-PR worktree removed (its teardown/pre-remove hooks run first)
	KindDropDBs        = "drop_dbs"        // orphan databases of one slug dropped
	KindResetExternal  = "reset_external"  // manual worktree reset to origin/<base>; databases kept
)

// Skip reasons: why something the options selected is not in Actions.
const (
	SkipPinned       = "pinned"               // slot or PR pinned (unpin first)
	SkipHoldReason   = "hold_reason"          // slots.hold_reason is set (Detail: the reason)
	SkipGraceLeft    = "grace_left"           // closed PR inside daemon.close_grace (Detail: time left)
	SkipNotManaged   = "not_managed"          // not magnum's to touch (external slot, non-allowlisted slug)
	SkipDirty        = "dirty_without_force"  // a human's changes would be lost; --force discards them (removals)
	SkipActiveRun    = "active_run"           // a review round is in flight (run, PR state or busy slot)
	SkipAgentWorking = "agent_working"        // a human's herdr agent is in the slot, or any agent works in a manual worktree
	SkipUnpushed     = "unpushed_commits"     // the reset would orphan commits; --force accepts that
	SkipSlotState    = "slot_state"           // the slot's state does not allow the operation
	SkipInUse        = "in_use"               // the slug's databases belong to a slot or worktree
	SkipNotFound     = "not_found"            // the named PR, slot, worktree or slug does not exist
	SkipIncomplete   = "discovery_incomplete" // a source (MySQL, git worktree lists, herdr) could not be read
)

// Result statuses.
const (
	StatusDone        = "done"
	StatusFailed      = "failed"
	StatusUnconfirmed = "unconfirmed" // needs typed confirmation; Apply was called without it
	StatusPlanned     = "planned"     // dry-run plan: nothing executed
)

// DroppedBy is slot_databases.dropped_by for databases cleanup dropped.
const DroppedBy = "cleanup"

var (
	// ErrOptions: the Options combination is invalid.
	ErrOptions = errors.New("cleanup: invalid options")
	// ErrUnconfirmed: an action needs typed confirmation that Apply was not given.
	ErrUnconfirmed = errors.New("cleanup: needs typed confirmation")
)

// Slots is the part of *slots.Manager cleanup drives.
type Slots interface {
	Release(ctx context.Context, slot store.Slot, pool config.Pool, reason string) error
	Remove(ctx context.Context, slot store.Slot, pool config.Pool, force bool) error
	RemovePRWorktree(ctx context.Context, slot store.Slot, force bool) error
	// GuardLive refuses a pinned or held slot and a human's agent or
	// process in it, and saves nothing (a forced removal's guard).
	GuardLive(ctx context.Context, slot store.Slot) error
	// HumanEvidence says why changes in the slot's tree are a human's (its
	// PR's human_active_at after the checkout), "" when they are magnum's
	// residue, which a release or removal discards.
	HumanEvidence(ctx context.Context, slot store.Slot) (string, error)
}

// Inventory is the part of *inventory.Scanner cleanup reads.
type Inventory interface {
	Scan(ctx context.Context, opts inventory.Options) (inventory.Inventory, error)
}

// MySQL is the part of *mysqlx.Client cleanup uses.
type MySQL interface {
	DropAll(ctx context.Context, names []string, g mysqlx.Guard) []mysqlx.DropResult
}

// Git is the part of *gitx.Client cleanup uses (dirty checks and the external
// reset).
type Git interface {
	FetchBranch(ctx context.Context, mainClone, base string) error
	ResetPlaceholder(ctx context.Context, dir, branch, base string) error
	Status(ctx context.Context, dir string) (gitx.Status, error)
	StatusPaths(ctx context.Context, dir string) ([]gitx.StatusEntry, error)
	Unpushed(ctx context.Context, dir string) (int, error)
	// UnpushedRef counts the commits reachable from ref that are on no
	// remote and no magnum ref, like Unpushed for HEAD.
	UnpushedRef(ctx context.Context, dir, ref string) (int, error)
	RevParse(ctx context.Context, dir, ref string) (string, error)
}

// Planner builds and applies plans. Store, Slots, Inventory and Config are
// required; MySQL is needed to apply drop_dbs, Git for dirty checks of
// per-PR worktrees without inventory facts and for reset_external.
type Planner struct {
	Store     *store.Store
	Slots     Slots
	Inventory Inventory
	MySQL     MySQL
	Git       Git
	// Runner runs `du -sk` on directories an action deletes (read-only);
	// nil leaves Action.Bytes at 0 (unknown).
	Runner execx.Runner
	Config *config.Config
	// Clock defaults to time.Now.
	Clock func() time.Time
	// Park, when set, is called before the slot of a PR is released or
	// removed (e.g. (*agents.Manager).Park); an error fails that action and
	// leaves the PR's state as it was.
	Park func(ctx context.Context, pr store.PR) error
	// Log receives one line per applied action (optional).
	Log execx.Logger
}

// PRRef names a pull request: Repo is "owner/name".
type PRRef struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

func (r PRRef) String() string { return fmt.Sprintf("%s#%d", r.Repo, r.Number) }

// Options selects what a plan covers. With no selector (PR, Slot, Orphans,
// Shrink) the plan is the default one: slots of closed PRs past their grace
// plus orphan databases on the automatic allowlist. Selectors combine.
type Options struct {
	DryRun bool   `json:"dry_run,omitempty"` // Apply executes nothing
	PR     *PRRef `json:"pr,omitempty"`      // release (pool) or remove (per-PR) this PR's slot
	// Slot names a managed slot to release (or, with Remove, to remove); with
	// External it names a manual worktree (repoN, its directory name or path).
	Slot     string `json:"slot,omitempty"`
	Remove   bool   `json:"remove,omitempty"`
	Orphans  bool   `json:"orphans,omitempty"` // orphan databases on the automatic allowlist
	Slug     string `json:"slug,omitempty"`    // with Orphans: also this slug's orphans (typed confirmation)
	Shrink   *int   `json:"shrink,omitempty"`  // remove free slots beyond max(pool.min, N), oldest idle first
	Idle     bool   `json:"idle,omitempty"`    // with Shrink: only slots idle longer than pool.idle_remove_after
	External bool   `json:"external,omitempty"`
	// Force discards a human's tracked changes and untracked files on a
	// removal (magnum's residue goes without it) and, for reset_external,
	// any change and unpushed commits; it also lets an explicitly
	// named PR be released inside its close grace. It never overrides pins,
	// hold reasons, agents, in-flight rounds or incomplete discovery.
	Force bool `json:"force,omitempty"`
}

// Action is one planned change. The identifying fields (SlotID, PRID, Path,
// Slug, ...) let Apply re-check it against the current state.
type Action struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"` // owner/name#N, slot:<name>, slug:<slug>, path:<dir>
	Why     string `json:"why"`
	// Bytes is the disk space freed (du estimate, 0 = none or unknown);
	// DBBytes the MySQL space freed.
	Bytes   int64    `json:"bytes"`
	DBBytes int64    `json:"db_bytes"`
	DBNames []string `json:"db_names,omitempty"`

	Repo   string `json:"repo,omitempty"` // owner/name (pool lookup)
	Slot   string `json:"slot,omitempty"`
	SlotID int64  `json:"slot_id,omitempty"`
	PRID   int64  `json:"pr_id,omitempty"`
	// PRState is the PR's state at plan time; a closed/releasing PR moves to
	// releasing and then released around the slot operation.
	PRState string `json:"pr_state,omitempty"`
	Reason  string `json:"reason,omitempty"` // assignment end reason: merged, closed or cleanup
	Path    string `json:"path,omitempty"`
	Slug    string `json:"slug,omitempty"`
	// reset_external
	MainClone string `json:"main_clone,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Base      string `json:"base,omitempty"`

	Force   bool `json:"force,omitempty"`
	Confirm bool `json:"confirm,omitempty"` // needs typed confirmation (Apply's confirmed)
}

// Skip is something the options selected that the plan leaves alone.
type Skip struct {
	Subject string `json:"subject"`
	Reason  string `json:"reason"`
	Detail  string `json:"detail,omitempty"`
}

// Totals sums a plan's estimates.
type Totals struct {
	Actions    int   `json:"actions"`
	DiskBytes  int64 `json:"disk_bytes"`
	MySQLBytes int64 `json:"mysql_bytes"`
	Databases  int   `json:"databases"`
}

// Plan is a deterministic list of actions plus what was skipped and why.
type Plan struct {
	CreatedAt time.Time `json:"created_at"`
	DryRun    bool      `json:"dry_run,omitempty"`
	Options   Options   `json:"options"`
	Actions   []Action  `json:"actions"`
	Skipped   []Skip    `json:"skipped"`
	Totals    Totals    `json:"totals"`
	Warnings  []string  `json:"warnings,omitempty"` // inventory sources that could not be read
}

// NeedsConfirmation reports whether any action needs typed confirmation.
func (p Plan) NeedsConfirmation() bool {
	for _, a := range p.Actions {
		if a.Confirm {
			return true
		}
	}
	return false
}

// Result is the outcome of one action.
type Result struct {
	Action Action `json:"action"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// Report is the outcome of Apply.
type Report struct {
	Results     []Result `json:"results"`
	Done        int      `json:"done"`
	Failed      int      `json:"failed"`
	Unconfirmed int      `json:"unconfirmed"`
	Planned     int      `json:"planned"`
	// Freed sums the estimates of the done actions.
	DiskBytes  int64    `json:"disk_bytes"`
	MySQLBytes int64    `json:"mysql_bytes"`
	DroppedDBs []string `json:"dropped_dbs,omitempty"`
}

func (p *Planner) now() time.Time {
	if p.Clock != nil {
		return p.Clock()
	}
	return time.Now()
}

func (p *Planner) logf(format string, args ...any) { p.logAt(slog.LevelInfo, format, args...) }

// logErr logs a line that reports err, a failure cleanup goes on after: at
// Warn, or at Info when err is the daemon stopping (execx.FailLevel).
func (p *Planner) logErr(ctx context.Context, err error, format string, args ...any) {
	p.logAt(execx.FailLevel(ctx, err), format, args...)
}

// logAt logs one line at level (execx.LogAt).
func (p *Planner) logAt(level slog.Level, format string, args ...any) {
	if p.Log != nil {
		execx.LogAt(p.Log, level, format, args...)
	}
}

// mirror logs an audit event cleanup recorded when it is a warn or error
// one, at its level (execx.LogEvent); the others stay in the registry only,
// as before.
func (p *Planner) mirror(level, subject, kind, msg string) {
	if p.Log != nil && execx.EventLevel(level) >= slog.LevelWarn {
		execx.LogEvent(p.Log, level, subject, kind, fmt.Sprintf("cleanup: %s %s: %s", subject, kind, msg))
	}
}

func (p *Planner) check() error {
	switch {
	case p.Store == nil:
		return errors.New("cleanup: Planner.Store is required")
	case p.Slots == nil:
		return errors.New("cleanup: Planner.Slots is required")
	case p.Inventory == nil:
		return errors.New("cleanup: Planner.Inventory is required")
	case p.Config == nil:
		return errors.New("cleanup: Planner.Config is required")
	}
	return nil
}

func totals(actions []Action) Totals {
	t := Totals{Actions: len(actions)}
	for _, a := range actions {
		t.DiskBytes += a.Bytes
		t.MySQLBytes += a.DBBytes
		t.Databases += len(a.DBNames)
	}
	return t
}
