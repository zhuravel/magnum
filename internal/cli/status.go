package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/launchd"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

const statusUsage = "[<ref>|<slot>] [--all] [--sizes] [--json] [--watch]"

func newStatusCmd(c *Context) *cobra.Command {
	var f statusFlags
	cmd := newCommand(groupInspect, "status "+statusUsage, "daemon, slots, queue, pauses; a PR's detail card with <ref>",
		"Show the daemon, the review slots and what they hold, the queue and any pauses. With a PR reference or "+
			"a slot name, print that PR's or slot's detail card instead: folder, databases, agent sessions and the "+
			"last review. --all adds the manual worktrees (talkable.repoN, *__worktrees/*) and --sizes measures folder "+
			"sizes with du.\n\n"+
			"--watch on a terminal opens the live dashboard, refreshed every 2s: j/k select a slot or PR, enter "+
			"opens its judge pane, r reviews (R fresh, i with /simplify), p/u pin and unpin, x releases, M/U mute "+
			"and unmute (every review, release, mute and unmute key asks y/N first; only y confirms), ctrl+r or F5 "+
			"refreshes now, b opens the PR in the browser, a jumps to what needs you, w shows "+
			"the manual worktrees, m turns the mouse off and on (wheel, click, double click to open, right click "+
			"for the row's actions), tab (or t) switches to the PR board of `magnum prs` and back, ? lists every key "+
			"and q quits. Elsewhere, or with a <ref>, --watch redraws "+
			"the text every 2s until interrupted.",
		func(pos []string) int { return runStatus(c, f, pos) })
	fs := cmd.Flags()
	fs.BoolVar(&f.all, "all", false, "also list manual worktrees (talkable.repoN, *__worktrees/*)")
	fs.BoolVar(&f.sizes, "sizes", false, "measure folder sizes with du (slow)")
	fs.BoolVar(&f.json, "json", false, "print JSON")
	fs.BoolVar(&f.watch, "watch", false, "live dashboard on a terminal (else redraw every 2s until interrupted)")
	cmd.ValidArgsFunction = completeFirst(c.completePRs, c.completeSlots)
	return cmd
}

// statusFlags are the parsed `magnum status` flags.
type statusFlags struct{ all, sizes, json, watch bool }

func runStatus(c *Context, f statusFlags, pos []string) int {
	if len(pos) > 1 {
		return inspUsage(c, "status", "at most one <ref> or <slot>", statusUsage)
	}
	if f.watch && f.json {
		return inspUsage(c, "status", "--watch redraws tables; drop --json", statusUsage)
	}
	a, err := inspOpenApp(c, false)
	if err != nil {
		return cmdFail(c, "status", err)
	}
	defer a.Close()
	ctx, cancel := signalContext()
	defer cancel()

	d := newStatusDeps(a, c.Version)
	o := statusOptions{All: f.all, Sizes: f.sizes, NoGitHub: f.watch}
	if len(pos) == 1 {
		o.Ref = pos[0]
	}
	if f.watch && o.Ref == "" && inspScreen(c) {
		return statusDashboard(ctx, c, d, o)
	}
	once := func() (string, error) {
		rep, err := statusGather(ctx, d, o)
		if err != nil {
			return "", err
		}
		var b bytes.Buffer
		if f.json {
			err = writeJSON(&b, rep)
		} else {
			statusRender(&b, rep)
		}
		return b.String(), err
	}
	if !f.watch {
		out, err := once()
		if err != nil {
			return cmdFail(c, "status", err)
		}
		io.WriteString(c.Stdout, out)
		return 0
	}
	for {
		out, err := once()
		if ctx.Err() != nil {
			return 0
		}
		if err != nil {
			out = "magnum status: " + err.Error() + "\n"
		}
		// Clear the screen and home the cursor, then draw.
		fmt.Fprintf(c.Stdout, "\033[H\033[2J%s\n(every 2s, %s; ctrl+c to stop)\n", out, inspNow().Format("15:04:05"))
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(statusRefresh):
		}
	}
}

// newStatusDeps reads everything status shows from a.
func newStatusDeps(a *app.App, version string) statusDeps {
	return statusDeps{
		Store: a.Store, Config: a.Config, Layout: a.Layout, Inventory: a.Inventory, Herdr: a.Herdr,
		Launchd: func(ctx context.Context) (launchd.Info, error) {
			return launchd.Status(ctx, a.Runner, os.Getuid(), launchd.DefaultLabel)
		},
		DaemonPID: func() (int, error) { return engine.DaemonPID(a.Layout) },
		DiskFree:  inspDiskFree, DiskPath: inspHome(), Now: inspNow,
		Version: version,
	}
}

// statusScanner is the part of *inventory.Scanner status reads.
type statusScanner interface {
	Scan(ctx context.Context, opts inventory.Options) (inventory.Inventory, error)
}

// statusHerdr is the part of *herdr.Client status reads.
type statusHerdr interface {
	Snapshot(ctx context.Context) (herdr.Snapshot, error)
}

// statusDeps are the sources statusGather reads; nil ones are skipped.
type statusDeps struct {
	Store     *store.Store
	Config    *config.Config
	Layout    paths.Layout
	Inventory statusScanner
	Herdr     statusHerdr
	Launchd   func(ctx context.Context) (launchd.Info, error)
	DaemonPID func() (int, error)
	DiskFree  func(path string) (uint64, error)
	DiskPath  string
	Now       func() time.Time
	// Version is this binary's build and ModTime reads a binary's mtime
	// (nil = engine.FileModTime): the daemon's build skew.
	Version string
	ModTime func(path string) (time.Time, bool)
	// DrainerAlive reports whether a drain's drainer still runs (nil =
	// engine.DrainerAlive).
	DrainerAlive func(pid int) bool
}

type statusOptions struct {
	Ref   string
	All   bool
	Sizes bool
	// NoGitHub keeps --all from asking GitHub for manual worktree PR states
	// (--watch would otherwise spend rate budget every redraw).
	NoGitHub bool
}

// statusReport is everything `magnum status` shows (and prints with --json).
type statusReport struct {
	GeneratedAt time.Time                `json:"generated_at"`
	Daemon      statusDaemon             `json:"daemon"`
	GitHub      statusGitHub             `json:"github"`
	Codex       *statusCodexUsage        `json:"codex_usage,omitempty"`
	Retro       *statusRetro             `json:"retro,omitempty"`
	Rounds      statusRounds             `json:"rounds"`
	Agents      *statusAgents            `json:"agents,omitempty"`
	Pauses      []statusPause            `json:"pauses"`
	Disk        statusDisk               `json:"disk"`
	Slots       []inventory.SlotView     `json:"slots"`
	External    []inventory.ExternalView `json:"external,omitempty"`
	Databases   bool                     `json:"databases_listed"`
	OrphanDBs   int                      `json:"orphan_dbs"`
	Queue       []statusPRLine           `json:"queue"`
	Closing     []statusPRLine           `json:"closing"`
	Attention   []statusAttention        `json:"attention"`
	Drift       []inventory.Finding      `json:"drift"`
	Warnings    []string                 `json:"warnings,omitempty"`
	Detail      *statusDetail            `json:"detail,omitempty"`
}

type statusDaemon struct {
	PID           int        `json:"pid"`
	Running       bool       `json:"running"`
	Launchd       string     `json:"launchd"`
	LaunchdPID    int        `json:"launchd_pid,omitempty"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	LastTick      *time.Time `json:"last_tick,omitempty"`
	LastPoll      *time.Time `json:"last_poll,omitempty"`
	LastReconcile *time.Time `json:"last_reconcile,omitempty"`
	HerdrUp       *bool      `json:"herdr_up,omitempty"` // as the daemon last saw it
	// DrainingSince is when `magnum daemon-restart --drain` stopped new
	// rounds (engine.KVDaemonDraining); nil when no drain is in progress.
	// DrainerPID is the pid of the command that drains (0 = not recorded).
	DrainingSince *time.Time `json:"draining_since,omitempty"`
	DrainerPID    int        `json:"drainer_pid,omitempty"`
	// Build is what the daemon recorded running at its start
	// (engine.KVDaemonBuild); Skew says when this binary or the one on disk
	// is newer (engine.SkewNote; only while a daemon runs).
	Build *engine.Build `json:"build,omitempty"`
	Skew  string        `json:"build_skew,omitempty"`
	// PromptsLoadedAt is when the running daemon took its prompt snapshot
	// (engine.KVPromptsLoadedAt). PromptsChanged counts the prompt and skill
	// files that differ on disk since (engine.KVPromptsChanged; they take
	// effect at the next restart) and PromptsChangedFiles names them.
	PromptsLoadedAt     *time.Time `json:"prompts_loaded_at,omitempty"`
	PromptsChanged      int        `json:"prompts_changed,omitempty"`
	PromptsChangedFiles []string   `json:"prompts_changed_files,omitempty"`
}

// statusCodexUsage is the Codex budget as the daemon last read it from Codex's
// session files, with the [usage] thresholds it is judged against. Pace is
// the share of the budget used over the share of the window elapsed (2.7 runs
// out at 37% of the window); SoftAt and HardAt are when the caps are reached
// at that average pace, left out once reached and when the reset comes first.
type statusCodexUsage struct {
	Percent       int        `json:"percent"`
	WindowMinutes int        `json:"window_minutes,omitempty"`
	ResetsAt      *time.Time `json:"resets_at,omitempty"`
	Plan          string     `json:"plan,omitempty"`
	ReportedAt    *time.Time `json:"reported_at,omitempty"`
	Soft          float64    `json:"soft"`
	Hard          float64    `json:"hard"`
	Pace          float64    `json:"pace,omitempty"`
	SoftAt        *time.Time `json:"soft_at,omitempty"`
	HardAt        *time.Time `json:"hard_at,omitempty"`
}

// statusRetro is the learning loop: whether [learn] schedules the daily
// retro, what the last retro did (engine.KVRetroLast), how many real misses
// (class miss) no lesson was drawn from yet and how many closed PRs wait
// for [learn] settle before a retro takes them. It is left out when [learn]
// is off and no retro ever ran.
type statusRetro struct {
	Enabled   bool                 `json:"enabled"`
	Last      *engine.RetroSummary `json:"last,omitempty"`
	NewMisses *int                 `json:"new_misses,omitempty"` // nil when the registry could not be read
	Settling  *int                 `json:"settling,omitempty"`   // nil when none wait (or no settle delay)
	Settle    string               `json:"settle,omitempty"`     // [learn] settle, when PRs wait for it
}

type statusGitHub struct {
	Remaining       *int       `json:"remaining,omitempty"`
	Limit           *int       `json:"limit,omitempty"`
	ResetAt         *time.Time `json:"reset_at,omitempty"`
	PollPausedUntil *time.Time `json:"poll_paused_until,omitempty"`
}

type statusRounds struct {
	Active int      `json:"active"`
	Max    int      `json:"max"`
	PRs    []string `json:"prs"`
}

type statusAgents struct {
	HerdrUp       bool   `json:"herdr_up"`
	Error         string `json:"error,omitempty"`
	WorkingCodex  int    `json:"working_codex"`
	WorkingClaude int    `json:"working_claude"`
	MaxCodex      int    `json:"max_codex"`
	// WorkingOther counts the other agent kinds the configured roles use
	// (droid, omp, any [kinds.<name>]), in [[role]] order.
	WorkingOther []statusKindCount `json:"working_other,omitempty"`
}

// statusKindCount is the number of working panes of one agent kind.
type statusKindCount struct {
	Kind    string `json:"kind"`
	Working int    `json:"working"`
}

// statusPause is one reason automation (or part of it) is held back.
type statusPause struct {
	Scope  string     `json:"scope"` // daemon, infra, codex, claude, watch:<owner>, identity:<name>, github
	Reason string     `json:"reason"`
	Detail string     `json:"detail,omitempty"`
	Until  *time.Time `json:"until,omitempty"`
	// Since is when `magnum pause` began and Held how many review requests
	// it holds (engine.KVDaemonPausedAt, KVDaemonPausedHeld); daemon scope
	// only.
	Since *time.Time `json:"since,omitempty"`
	Held  int        `json:"held,omitempty"`
	// Using is the model the kind's sessions run while a model is limited
	// (Reason "<model> limited").
	Using string `json:"using,omitempty"`
	Fix   string `json:"fix,omitempty"`
}

type statusDisk struct {
	Path      string `json:"path"`
	FreeBytes uint64 `json:"free_bytes"`
	MinGB     int    `json:"min_gb"`
	Error     string `json:"error,omitempty"`
}

// statusPRLine is one PR in the QUEUE or CLOSED-pending list.
type statusPRLine struct {
	Repo            string     `json:"repo"`
	Number          int        `json:"number"`
	Title           string     `json:"title"`
	State           string     `json:"state"`
	Next            string     `json:"next"`
	Forced          bool       `json:"forced,omitempty"`
	ReviewRequested bool       `json:"review_requested,omitempty"`
	UpdatedAt       *time.Time `json:"updated_at,omitempty"`

	url, author string    // for the dashboard (not printed)
	rec         *store.PR // the registry row, for the dashboard's y/N question (not printed)
}

// statusAttention is one thing that needs a human.
type statusAttention struct {
	Subject string `json:"subject"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

// statusDetail is the card for one PR (or a slot without a PR).
type statusDetail struct {
	Repo       string              `json:"repo,omitempty"`
	PR         *store.PR           `json:"pr,omitempty"`
	Next       string              `json:"next,omitempty"`
	Slot       *inventory.SlotView `json:"slot,omitempty"`
	LastFolder string              `json:"last_folder,omitempty"` // latest assignment when no slot holds it
	Sessions   []statusSession     `json:"sessions"`
	Runs       []store.Run         `json:"runs"`
	Notes      bool                `json:"notes"`                // the repository has a notes file (magnum notes)
	LastRound  *tui.RoundTimings   `json:"last_round,omitempty"` // stage timings of the last round
	// Attention explains why a PR in needs_attention needs the user.
	Attention *attention.Reason `json:"attention,omitempty"`
	// Findings is what magnum's latest posted review concluded.
	Findings *store.ReviewSummary `json:"findings,omitempty"`

	isJudge func(role string) bool // a configured judge's role name (actIsJudge)
}

type statusSession struct {
	store.Session
	Live   string `json:"live_status,omitempty"` // herdr's current agent status
	Resume string `json:"resume,omitempty"`      // copy-paste resume command
}
