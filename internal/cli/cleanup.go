package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/cleanup"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

const cleanupUsage = "[--dry-run] [--yes] [--json] [--wait] [--pr <ref>] [--slot <name> [--remove]] " +
	"[--orphans [--slug X]] [--shrink[=N] [--idle]] [--external --slot <repoN>] [--force]"

// inspHandoffWait is how long a command waits for the daemon to finish a
// request it handed over before printing "queued".
var inspHandoffWait = 3 * time.Second

func newCleanupCmd(c *Context) *cobra.Command {
	f := &cleanupFlags{cmd: "cleanup", review: true}
	cmd := newCommand(groupAct, "cleanup "+cleanupUsage, "plan and apply storage cleanup (closed PRs, orphan DBs, slots)",
		"Plan and apply storage cleanup: release or remove the slots of closed PRs, drop orphan databases, "+
			"shrink the pool, reset a manual worktree. Without options it plans the routine cleanup. On a terminal "+
			"the plan opens in a review screen: space toggles an action, a/n select all or none, enter confirms (y, "+
			"or the slug typed when another slug's databases would be dropped) and applies the selection, q "+
			"cancels. With --yes, --dry-run or --json, or off a terminal, the plan is printed instead: --dry-run "+
			"stops there, a terminal asks y/N before applying it, and --yes applies it without the question "+
			"(typed slugs are still required). While the daemon runs it applies the plan (--wait follows it).",
		func(pos []string) int {
			if err := cleanupArgs(c, pos); err != nil {
				return 2
			}
			return runCleanup(c, f)
		})
	cleanupDefineFlags(cmd.Flags(), f)
	_ = cmd.RegisterFlagCompletionFunc("pr", completeFlag(c.completePRs))
	_ = cmd.RegisterFlagCompletionFunc("slot", completeFlag(c.completeSlots))
	return cmd
}

func runCleanup(c *Context, f *cleanupFlags) int {
	a, err := inspOpenApp(c, false)
	if err != nil {
		return cmdFail(c, "cleanup", err)
	}
	defer a.Close()
	ctx, cancel := signalContext()
	defer cancel()
	a.Cleanup.Log = app.Printf{Logger: app.NewLogger(nil, c.Stderr, slog.LevelInfo), Level: slog.LevelInfo, Src: "cleanup"}
	return cleanupExec(ctx, c, a.Cleanup, a.Store, f, a.Config.Daemon.DefaultRepo)
}

// cleanupPlanner is the part of *cleanup.Planner the command drives.
type cleanupPlanner interface {
	Plan(ctx context.Context, opts cleanup.Options) (cleanup.Plan, error)
	Apply(ctx context.Context, plan cleanup.Plan, confirmed bool) (cleanup.Report, error)
}

// cleanupFlags are the parsed command-line options.
type cleanupFlags struct {
	cmd      string // command name for messages ("cleanup", "slots remove")
	review   bool   // on a terminal, review the plan in the screen (cleanup; slots remove keeps its y/N)
	dryRun   bool
	yes      bool
	asJSON   bool
	wait     bool
	pr       string
	slot     string
	remove   bool
	orphans  bool
	slug     string
	shrink   *int
	idle     bool
	external bool
	force    bool
}

// cleanupShrink implements --shrink (bare: keep pool.min) and --shrink=N.
type cleanupShrink struct{ v **int }

func (s cleanupShrink) String() string {
	if s.v == nil || *s.v == nil {
		return ""
	}
	return strconv.Itoa(**s.v)
}

func (s cleanupShrink) Set(x string) error {
	switch x {
	case "true":
		n := 0
		*s.v = &n
		return nil
	case "false":
		*s.v = nil
		return nil
	}
	n, err := strconv.Atoi(x)
	if err != nil || n < 0 {
		return errors.New("--shrink takes a non-negative slot count (--shrink=N)")
	}
	*s.v = &n
	return nil
}

func (s cleanupShrink) Type() string { return "int" }

func cleanupDefineFlags(fs *pflag.FlagSet, f *cleanupFlags) {
	fs.BoolVar(&f.dryRun, "dry-run", false, "print the plan only")
	fs.BoolVar(&f.yes, "yes", false, "apply without the y/N question (typed slug confirmation is still required)")
	fs.BoolVar(&f.asJSON, "json", false, "print the plan (and report) as JSON; applies only with --yes")
	fs.BoolVar(&f.wait, "wait", false, "when the daemon applies the plan, wait until it is done")
	fs.StringVar(&f.pr, "pr", "", "release (pool) or remove (per-PR worktree) this PR's slot")
	fs.StringVar(&f.slot, "slot", "", "release this managed slot (with --remove: remove it; with --external: reset this manual worktree)")
	fs.BoolVar(&f.remove, "remove", false, "with --slot: remove the slot instead of releasing it")
	fs.BoolVar(&f.orphans, "orphans", false, "drop orphan databases of magnum's own slugs (review<N>)")
	fs.StringVar(&f.slug, "slug", "", "with --orphans: also drop this slug's orphan databases (typed confirmation)")
	fs.Var(cleanupShrink{&f.shrink}, "shrink", "remove free pool slots beyond max(pool.min, `N`) (bare: pool.min)")
	fs.Lookup("shrink").NoOptDefVal = "0" // bare --shrink: keep pool.min
	fs.BoolVar(&f.idle, "idle", false, "with --shrink: only slots idle longer than pool.idle_remove_after")
	fs.BoolVar(&f.external, "external", false, "with --slot repoN: reset a manual worktree to origin/<base> (keeps its databases)")
	fs.BoolVar(&f.force, "force", false, "discard tracked changes, accept unpushed commits on --external, release inside the close grace")
}

// cleanupArgs refuses positional arguments: cleanup names everything with
// flags.
func cleanupArgs(c *Context, pos []string) error {
	if len(pos) > 0 {
		fmt.Fprintf(c.Stderr, "magnum cleanup: unexpected argument %q (name a PR with --pr, a slot with --slot)\n", pos[0])
		return fmt.Errorf("unexpected argument %q", pos[0])
	}
	return nil
}

// options maps the flags onto cleanup.Options (Plan validates combinations).
func (f *cleanupFlags) options(ctx context.Context, st *store.Store, defaultRepo string) (cleanup.Options, error) {
	o := cleanup.Options{
		DryRun: f.dryRun, Slot: f.slot, Remove: f.remove, Orphans: f.orphans, Slug: f.slug,
		Shrink: f.shrink, Idle: f.idle, External: f.external, Force: f.force,
	}
	if f.pr != "" {
		full, n, err := resolveRefRepo(ctx, st, app.RefParser{DefaultRepo: defaultRepo}, f.pr)
		if err != nil {
			return o, fmt.Errorf("--pr: %w", err)
		}
		o.PR = &cleanup.PRRef{Repo: full, Number: n}
	}
	return o, nil
}

// cleanupJSONOut is the --json document.
type cleanupJSONOut struct {
	Plan    cleanup.Plan    `json:"plan"`
	Report  *cleanup.Report `json:"report,omitempty"`
	Request *store.Request  `json:"request,omitempty"`
}

// cleanupExec plans, prints, confirms and applies (in-process under the
// locks of acquireOps, or through the running daemon).
func cleanupExec(ctx context.Context, c *Context, pl cleanupPlanner, st *store.Store, f *cleanupFlags, defaultRepo string) int {
	cmd := f.cmd
	opts, err := f.options(ctx, st, defaultRepo)
	if err != nil {
		return inspUsage(c, cmd, err.Error(), cleanupUsage)
	}
	plan, err := pl.Plan(ctx, opts)
	if errors.Is(err, cleanup.ErrOptions) {
		return inspUsage(c, cmd, strings.TrimPrefix(err.Error(), "cleanup: invalid options: "), cleanupUsage)
	}
	if err != nil {
		return cmdFail(c, cmd, err)
	}
	applying := !f.dryRun && len(plan.Actions) > 0
	confirmed := false
	screen := f.review && applying && !f.yes && !f.asJSON && inspScreen(c)
	if screen {
		var apply bool
		plan, confirmed, apply, err = cleanupReview(ctx, plan)
		if err != nil {
			return cmdFail(c, cmd, err)
		}
		if !apply {
			fmt.Fprintln(c.Stdout, "Not applied.")
			return 0
		}
	} else if f.asJSON {
		if !applying || !f.yes {
			if err := writeJSON(c.Stdout, cleanupJSONOut{Plan: plan}); err != nil {
				return cmdFail(c, cmd, err)
			}
			return 0
		}
	} else {
		fmt.Fprint(c.Stdout, cleanupRender(plan))
	}
	if !applying {
		return 0
	}

	if !f.yes && !screen {
		if !inspIsTTY() {
			fmt.Fprintf(c.Stderr, "magnum %s: not applied: stdin is not a terminal; re-run with --yes to apply this plan\n", cmd)
			return 1
		}
		if !inspConfirm(ctx, c, fmt.Sprintf("Apply %d action(s)?", len(plan.Actions))) {
			fmt.Fprintln(c.Stdout, "Not applied.")
			if ctx.Err() != nil {
				return 130
			}
			return 0
		}
	}
	// Typed confirmation is never asked in --json mode (the prompt would
	// corrupt the output): such actions report unconfirmed. The review screen
	// has asked already.
	if plan.NeedsConfirmation() && !f.asJSON && !screen {
		confirmed = true
		var slugs []string
		for _, a := range plan.Actions {
			if a.Confirm && !slices.Contains(slugs, a.Slug) {
				slugs = append(slugs, a.Slug)
			}
		}
		for _, slug := range slugs {
			q := fmt.Sprintf("Dropping the databases of slug %q cannot be undone.", slug)
			if !inspConfirmTyped(ctx, c, q, slug) {
				confirmed = false
			}
			if ctx.Err() != nil {
				fmt.Fprintln(c.Stdout, "Not applied.")
				return 130
			}
		}
		if !confirmed {
			fmt.Fprintln(c.Stdout, "Slug not confirmed: its databases stay (reported as unconfirmed).")
		}
	}

	unlock, who, err := acquireOps(c.Layout)
	switch {
	case err != nil:
		return cmdFail(c, cmd, err)
	case who == opsBusy:
		return cmdFail(c, cmd, opsBusyErr(c.Layout))
	case who == opsDaemon:
		wait := inspHandoffWait
		if f.wait {
			wait = 24 * time.Hour
		}
		out, err := inspHandOff(ctx, c, st, engine.ReqCleanup, engine.CleanupPayload{Plan: &plan, Confirmed: confirmed}, wait)
		if err != nil {
			return cmdFail(c, cmd, err)
		}
		if f.asJSON {
			if err := writeJSON(c.Stdout, cleanupJSONOut{Plan: plan, Request: &out.Req}); err != nil {
				return cmdFail(c, cmd, err)
			}
			return out.jsonCode()
		}
		return out.print(c.Stdout, c.Stderr)
	}
	defer unlock()
	rep, aerr := pl.Apply(ctx, plan, confirmed)
	if f.asJSON {
		if err := writeJSON(c.Stdout, cleanupJSONOut{Plan: plan, Report: &rep}); err != nil {
			return cmdFail(c, cmd, err)
		}
	} else {
		fmt.Fprint(c.Stdout, cleanupRenderReport(rep))
	}
	if aerr != nil || rep.Failed > 0 {
		if !f.asJSON && aerr != nil {
			fmt.Fprintf(c.Stderr, "magnum %s: %d action(s) failed; `magnum logs` has the details\n", cmd, max(rep.Failed, 1))
		}
		return 1
	}
	return 0
}

// cleanupReview shows the plan in the review screen and returns the plan
// narrowed to the actions the user selected. confirmed is true when a
// selected action needed its slug typed (the screen applies only after
// every required text was typed); apply is false when the user cancelled.
func cleanupReview(ctx context.Context, plan cleanup.Plan) (_ cleanup.Plan, confirmed, apply bool, _ error) {
	out, err := tuiCleanupPlan(ctx, cleanupScreenPlan(plan))
	if err != nil || !out.Apply {
		return plan, false, false, err
	}
	var picked []cleanup.Action
	for _, id := range out.SelectedIDs {
		i, err := strconv.Atoi(id)
		if err != nil || i < 0 || i >= len(plan.Actions) {
			return plan, false, false, fmt.Errorf("review screen returned unknown action %q", id)
		}
		picked = append(picked, plan.Actions[i])
		confirmed = confirmed || plan.Actions[i].Confirm
	}
	if len(picked) == 0 {
		return plan, false, false, nil
	}
	plan.Actions = picked
	plan.Totals = cleanupTotals(picked)
	return plan, confirmed, true, nil
}

// cleanupScreenPlan maps a plan onto the review screen; an action's ID is
// its index in plan.Actions.
func cleanupScreenPlan(p cleanup.Plan) tui.CleanupPlan {
	out := tui.CleanupPlan{Totals: tui.CleanupTotals{Disk: p.Totals.DiskBytes, MySQL: p.Totals.MySQLBytes}}
	for i, a := range p.Actions {
		ta := tui.CleanupAction{ID: strconv.Itoa(i), Kind: a.Kind, Subject: a.Subject, Why: a.Why,
			Bytes: a.Bytes, DBBytes: a.DBBytes, DBNames: slices.Clone(a.DBNames)}
		if a.Confirm {
			ta.NeedsTypedConfirm = a.Slug
		}
		out.Actions = append(out.Actions, ta)
	}
	for _, s := range p.Skipped {
		reason := s.Reason
		if s.Detail != "" {
			reason += " (" + s.Detail + ")"
		}
		out.Skipped = append(out.Skipped, tui.CleanupSkip{Subject: s.Subject, Reason: reason})
	}
	return out
}

// cleanupTotals sums the estimates of actions (cleanup.Plan.Totals).
func cleanupTotals(actions []cleanup.Action) cleanup.Totals {
	t := cleanup.Totals{Actions: len(actions)}
	for _, a := range actions {
		t.DiskBytes += a.Bytes
		t.MySQLBytes += a.DBBytes
		t.Databases += len(a.DBNames)
	}
	return t
}

func cleanupRender(p cleanup.Plan) string {
	s := cleanup.Render(p)
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s
}

func cleanupRenderReport(r cleanup.Report) string {
	s := cleanup.RenderReport(r)
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s
}
