package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
)

const slotsUsage = "[list [--all] [--json]] | provision [--count N] [--repo owner/name] | remove <slot> [--force] [--yes] [--dry-run] | " +
	"repair <slot> | adopt <path> | pin <slot> | unpin <slot>"

func newSlotsCmd(c *Context) *cobra.Command {
	var all, asJSON bool
	cmd := newCommand(groupInspect, "slots [--all] [--json]", "review slot pool: list, provision, remove, repair, adopt, pin, unpin",
		"Manage the review slot pool: the long-lived worktrees (talkable.reviewN, each with its own databases) "+
			"that review rounds claim. Without a subcommand it lists the slots. While the daemon runs, provision, "+
			"repair and adopt are handed to it; follow them with `magnum logs request:<id> -f`.",
		func(pos []string) int {
			if len(pos) > 0 {
				if pos[0] == "help" {
					fmt.Fprintf(c.Stdout, "usage: magnum slots %s\n", slotsUsage)
					return 0
				}
				return inspUsage(c, "slots", fmt.Sprintf("unknown slots subcommand %q", pos[0]), slotsUsage)
			}
			return slotsRun(c, false, func(ctx context.Context, e *slotsEnv) int { return e.list(ctx, all, asJSON) })
		})
	slotsListFlags(cmd, &all, &asJSON)
	cmd.AddCommand(newSlotsListCmd(c), newSlotsProvisionCmd(c), newSlotsRemoveCmd(c),
		newSlotsTargetCmd(c, "repair", "<slot>", "re-run provisioning of a slot",
			"Re-run provisioning (render, setup, verification) of a free, broken, provisioning or lost slot; a "+
				"missing worktree is added again. The slot ends free."),
		newSlotsTargetCmd(c, "adopt", "<path>", "register an existing checkout as a pool slot",
			"Register an existing checkout at <path> as a free pool slot. The path must be the pool's slot path "+
				"for some N, a worktree of the main clone whose tmp/.worktree-db-slug names that slot, and all its "+
				"databases must exist. A removed, lost or broken row of that name is revived."),
		newSlotsTargetCmd(c, "pin", "<slot>", "keep a slot for you (no automatic claim, checkout, release or removal)",
			"Pin a slot: automation does not claim, check out, release or remove it until `magnum slots unpin`. "+
				"The PR it holds is pinned too."),
		newSlotsTargetCmd(c, "unpin", "<slot>", "hand a pinned slot back to automation",
			"Hand a pinned slot back to automation. A head_drift or unpushed_commits hold is acknowledged by "+
				"taking the current HEAD, so the next release resets the slot."),
	)
	return cmd
}

func slotsListFlags(cmd *cobra.Command, all, asJSON *bool) {
	cmd.Flags().BoolVar(all, "all", false, "include removed slots")
	cmd.Flags().BoolVar(asJSON, "json", false, "print JSON")
}

func newSlotsListCmd(c *Context) *cobra.Command {
	var all, asJSON bool
	cmd := newCommand("", "list [--all] [--json]", "list the slots (the default)",
		"List the slots with their kind, state, the PR they hold, their databases and last use. --all includes "+
			"removed slots.",
		func(pos []string) int {
			if len(pos) > 0 {
				return inspUsage(c, "slots", "list takes no arguments", slotsUsage)
			}
			return slotsRun(c, false, func(ctx context.Context, e *slotsEnv) int { return e.list(ctx, all, asJSON) })
		})
	slotsListFlags(cmd, &all, &asJSON)
	return cmd
}

func newSlotsProvisionCmd(c *Context) *cobra.Command {
	count, repo := -1, ""
	cmd := newCommand("", "provision [--count N] [--repo owner/name]", "create pool slots now",
		"Create pool slots now: up to pool.min, or --count more, for the pool of --repo (default "+
			"daemon.default_repo, or the only pool). Each slot gets a worktree on origin/<base>, its databases and "+
			"the pool's setup commands. While the daemon runs the request is handed to it.",
		func(pos []string) int {
			if len(pos) > 0 || count == 0 || count < -1 {
				return inspUsage(c, "slots", "provision takes --count N (N >= 1) and --repo only", slotsUsage)
			}
			return slotsRun(c, true, func(ctx context.Context, e *slotsEnv) int {
				pool, err := e.pool(repo)
				if err != nil {
					return cmdFail(c, "slots provision", err)
				}
				return e.provision(ctx, pool, count)
			})
		})
	cmd.Flags().IntVar(&count, "count", -1, "slots to provision now (default: up to pool.min)")
	cmd.Flags().StringVar(&repo, "repo", "", "pool repository (default: daemon.default_repo)")
	_ = cmd.RegisterFlagCompletionFunc("repo", completeFlag(c.completePools))
	return cmd
}

func newSlotsRemoveCmd(c *Context) *cobra.Command {
	f := &cleanupFlags{cmd: "slots remove", remove: true}
	cmd := newCommand("", "remove <slot> [--force] [--yes] [--dry-run] [--json] [--wait]", "remove a slot through the cleanup planner",
		"Remove a slot (worktree and databases) with the same guards, plan, confirmation and daemon hand-off as "+
			"`magnum cleanup --slot <slot> --remove`. --force also removes a claimed or held slot and discards "+
			"tracked changes.",
		func(pos []string) int {
			if len(pos) != 1 {
				return inspUsage(c, "slots", "remove needs a slot name", slotsUsage)
			}
			return slotsRun(c, false, func(ctx context.Context, e *slotsEnv) int { return e.remove(ctx, f, pos[0]) })
		})
	slotsRemoveFlags(cmd.Flags(), f)
	cmd.ValidArgsFunction = completeFirst(c.completeSlots)
	return cmd
}

func slotsRemoveFlags(fs *pflag.FlagSet, f *cleanupFlags) {
	fs.BoolVar(&f.force, "force", false, "remove a claimed/held slot and discard tracked changes")
	fs.BoolVar(&f.yes, "yes", false, "apply without the y/N question")
	fs.BoolVar(&f.dryRun, "dry-run", false, "print the plan only")
	fs.BoolVar(&f.asJSON, "json", false, "print JSON")
	fs.BoolVar(&f.wait, "wait", false, "when the daemon applies it, wait until it is done")
}

// remove removes a slot through the cleanup planner (same guards, plan,
// confirmation and daemon hand-off as `magnum cleanup --slot X --remove`).
func (e *slotsEnv) remove(ctx context.Context, f *cleanupFlags, slot string) int {
	f.slot = slot
	return cleanupExec(ctx, e.c, e.cleaner, e.st, f, e.cfg.Daemon.DefaultRepo)
}

// newSlotsTargetCmd is `slots repair|adopt|pin|unpin <arg>`.
func newSlotsTargetCmd(c *Context, sub, arg, short, long string) *cobra.Command {
	cmd := newCommand("", sub+" "+arg, short, long, func(pos []string) int {
		if len(pos) != 1 {
			what := "a slot name"
			if sub == "adopt" {
				what = "the checkout path"
			}
			return inspUsage(c, "slots", sub+" needs "+what, slotsUsage)
		}
		target := pos[0]
		return slotsRun(c, sub != "pin" && sub != "unpin", func(ctx context.Context, e *slotsEnv) int {
			switch sub {
			case "repair":
				return e.repair(ctx, target)
			case "adopt":
				return e.adopt(ctx, target)
			case "pin":
				return e.pin(ctx, target, true)
			}
			return e.pin(ctx, target, false)
		})
	})
	if sub == "adopt" {
		cmd.ValidArgsFunction = func(_ *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveFilterDirs
		}
	} else {
		cmd.ValidArgsFunction = completeFirst(c.completeSlots)
	}
	return cmd
}

// slotsRun opens the App (verbose: log its steps to stderr) and runs fn.
func slotsRun(c *Context, verbose bool, fn func(ctx context.Context, e *slotsEnv) int) int {
	ctx, cancel := signalContext()
	defer cancel()
	a, err := inspOpenApp(c, verbose)
	if err != nil {
		return cmdFail(c, "slots", err)
	}
	defer a.Close()
	// remove's cleanup logs its steps.
	a.Cleanup.Log = app.Printf{Logger: app.NewLogger(nil, c.Stderr, slog.LevelInfo), Level: slog.LevelInfo, Src: "cleanup"}
	return fn(ctx, &slotsEnv{c: c, st: a.Store, cfg: a.Config, ops: a.Slots, cleaner: a.Cleanup})
}

// slotsOps is the part of *slots.Manager the slots command drives.
type slotsOps interface {
	ProvisionPool(ctx context.Context, pool config.Pool, n int) error
	NextSlotNumber(ctx context.Context, pool config.Pool) (int, error)
	Repair(ctx context.Context, slot store.Slot, pool config.Pool) error
	Adopt(ctx context.Context, pool config.Pool, path string) (store.Slot, error)
	Pin(ctx context.Context, slot store.Slot) error
	Unpin(ctx context.Context, slot store.Slot) error
}

// slotsEnv is what the subcommands work with.
type slotsEnv struct {
	c       *Context
	st      *store.Store
	cfg     *config.Config
	ops     slotsOps
	cleaner cleanupPlanner
}

// slotsRow is one `slots list` line (and its JSON form).
type slotsRow struct {
	Name     string     `json:"name"`
	Kind     string     `json:"kind"`
	State    string     `json:"state"`
	PR       string     `json:"pr,omitempty"`
	PRState  string     `json:"pr_state,omitempty"`
	Path     string     `json:"path"`
	LastUsed string     `json:"last_used,omitempty"`
	Note     string     `json:"note,omitempty"`
	Slot     store.Slot `json:"slot"`
}

func (e *slotsEnv) list(ctx context.Context, all, asJSON bool) int {
	sls, err := e.st.ListSlots(ctx, store.SlotFilter{})
	if err != nil {
		return cmdFail(e.c, "slots", err)
	}
	now := inspNow()
	rows := []slotsRow{}
	for _, sl := range sls {
		if sl.State == store.SlotRemoved && !all {
			continue
		}
		r := slotsRow{Name: sl.Name, Kind: sl.Kind, State: sl.State, Path: sl.Path, Slot: sl}
		if sl.PRID != nil {
			if pr, err := e.st.PRByID(ctx, *sl.PRID); err == nil {
				r.PRState = pr.State
				if repo, err := e.st.RepoByID(ctx, pr.RepoID); err == nil {
					r.PR = fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
				}
			}
		}
		if sl.LastUsedAt != nil {
			r.LastUsed = inspAgo(now, sl.LastUsedAt)
		}
		var notes []string
		if sl.Pinned {
			notes = append(notes, "pinned")
		}
		if sl.HoldReason != nil {
			notes = append(notes, "hold: "+*sl.HoldReason)
		}
		if sl.DirtySchema {
			notes = append(notes, "dirty schema")
		}
		if le := store.Deref(sl.LastError); le != "" && sl.State != store.SlotFree {
			notes = append(notes, "error: "+trunc(le, 60))
		}
		r.Note = strings.Join(notes, "; ")
		rows = append(rows, r)
	}
	if asJSON {
		if err := writeJSON(e.c.Stdout, rows); err != nil {
			return cmdFail(e.c, "slots", err)
		}
		return 0
	}
	if len(rows) == 0 {
		fmt.Fprintln(e.c.Stdout, "no slots yet; `magnum slots provision` creates the pool")
		return 0
	}
	tw := inspTable(e.c.Stdout)
	fmt.Fprintln(tw, "SLOT\tKIND\tSTATE\tPR\tFOLDER\tLAST USED\tNOTE")
	for _, r := range rows {
		pr := "-"
		if r.PR != "" {
			pr = r.PR[strings.LastIndex(r.PR, "/")+1:]
			if r.PRState != "" {
				pr += " (" + r.PRState + ")"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, r.Kind, r.State, pr, inspTilde(r.Path), inspOrDash(r.LastUsed), inspOrDash(r.Note))
	}
	tw.Flush()
	return 0
}

// pool picks the pool for repo ("" = daemon.default_repo, else the only pool).
func (e *slotsEnv) pool(repo string) (config.Pool, error) {
	if len(e.cfg.Pools) == 0 {
		return config.Pool{}, errors.New("config.toml has no [[pool]]; add one to provision slots")
	}
	if repo == "" {
		if p := e.cfg.PoolFor(e.cfg.Daemon.DefaultRepo); p != nil {
			return *p, nil
		}
		if len(e.cfg.Pools) == 1 {
			return e.cfg.Pools[0], nil
		}
		return config.Pool{}, errors.New("several pools are configured; pass --repo owner/name")
	}
	if p := e.cfg.PoolFor(repo); p != nil {
		return *p, nil
	}
	return config.Pool{}, fmt.Errorf("no [[pool]] for %s in config.toml", repo)
}

// slotsNumber finds n with pool.Slot(n) == name.
func slotsNumber(pool config.Pool, name string) (int, bool) {
	for n := 1; n <= 999; n++ {
		if pool.Slot(n) == name {
			return n, true
		}
	}
	return 0, false
}

// inProcess takes the locks for in-process slot work (acquireOps). When the
// daemon runs the work is handed to it instead as a request of kind with
// payload; done then reports that the hand-off (or a failure) already
// produced the exit code.
func (e *slotsEnv) inProcess(ctx context.Context, cmd, kind string, payload any) (unlock func(), code int, done bool) {
	unlock, who, err := acquireOps(e.c.Layout)
	switch {
	case err != nil:
		return nil, cmdFail(e.c, cmd, err), true
	case who == opsBusy:
		return nil, cmdFail(e.c, cmd, opsBusyErr(e.c.Layout)), true
	case who == opsDaemon:
		return nil, e.handOff(ctx, cmd, kind, payload), true
	}
	return unlock, 0, false
}

// handOff queues slot work for the daemon, which owns slots while it runs,
// and waits briefly for it; long work (setup takes up to 45 minutes) is
// reported as queued with how to follow it.
func (e *slotsEnv) handOff(ctx context.Context, cmd, kind string, payload any) int {
	out, err := inspHandOff(ctx, e.c, e.st, kind, payload, inspHandoffWait)
	if err != nil {
		return cmdFail(e.c, cmd, err)
	}
	if out.Pending() && kind != engine.ReqAdopt {
		fmt.Fprintf(e.c.Stdout, "the daemon runs `magnum %s` in the background; setup logs: %s\n", cmd,
			inspTilde(filepath.Join(e.c.Layout.Logs(), "provision-<slot>.log")))
	}
	return out.print(e.c.Stdout, e.c.Stderr)
}

// provision provisions count pool slots (count < 0: up to pool.min),
// resuming interrupted ones first: in-process under the locks, or through
// the running daemon (request provision).
func (e *slotsEnv) provision(ctx context.Context, pool config.Pool, count int) int {
	const cmd = "slots provision"
	sls, err := e.st.ListSlots(ctx, store.SlotFilter{RepoFullName: pool.Repo, Kind: store.SlotKindPool})
	if err != nil {
		return cmdFail(e.c, cmd, err)
	}
	live := 0
	var resumable []store.Slot
	for _, sl := range sls {
		switch sl.State {
		case store.SlotRemoved:
		case store.SlotProvisioning:
			resumable = append(resumable, sl)
		default:
			live++
		}
	}
	if count < 0 {
		count = max(pool.Min-live, len(resumable))
		if count == 0 {
			fmt.Fprintf(e.c.Stdout, "%s already has %d slots (pool.min %d); pass --count N to add more\n", pool.Repo, live, pool.Min)
			return 0
		}
	}
	if pool.Max > 0 && live+count > pool.Max {
		return cmdFail(e.c, cmd, fmt.Errorf("%s allows at most %d slots and has %d; lower --count or raise max in config.toml",
			pool.Repo, pool.Max, live))
	}
	unlock, code, done := e.inProcess(ctx, cmd, engine.ReqProvision, engine.ProvisionPayload{Pool: pool.Repo, Count: count})
	if done {
		return code
	}
	defer unlock()
	for i := 0; i < count; i++ {
		var n int
		if len(resumable) > 0 {
			sl := resumable[0]
			resumable = resumable[1:]
			var found bool
			if n, found = slotsNumber(pool, sl.Name); !found {
				return cmdFail(e.c, cmd, fmt.Errorf("slot %s does not match slot_name %q; remove it with `magnum slots remove %s`",
					sl.Name, pool.SlotName, sl.Name))
			}
		} else if n, err = e.ops.NextSlotNumber(ctx, pool); err != nil {
			return cmdFail(e.c, cmd, err)
		}
		name := pool.Slot(n)
		logPath := filepath.Join(e.c.Layout.Logs(), "provision-"+name+".log")
		fmt.Fprintf(e.c.Stdout, "provisioning %s in %s (%d of %d)\n  setup log: %s (follow with `tail -f %s`; setup can take 45 minutes)\n",
			name, inspTilde(pool.Path(n)), i+1, count, inspTilde(logPath), inspTilde(logPath))
		if err := e.ops.ProvisionPool(ctx, pool, n); err != nil {
			return cmdFail(e.c, cmd, fmt.Errorf("%s: %w\n  fix: %s", name, err, slotsProvisionFix(err, name, logPath)))
		}
		fmt.Fprintf(e.c.Stdout, "%s is ready (free)\n", name)
	}
	return 0
}

func slotsProvisionFix(err error, name, logPath string) string {
	switch {
	case errors.Is(err, slots.ErrLowDisk):
		return "free disk space first (`magnum cleanup`, `magnum cleanup --shrink`) or lower min_free_disk_gb in config.toml"
	case errors.Is(err, slots.ErrVerify):
		return fmt.Sprintf("read %s, fix the setup, then `magnum slots repair %s`", inspTilde(logPath), name)
	case errors.Is(err, slots.ErrMiseLocal):
		return "fix the main clone's .mise.local.toml (no root-level env table), then run `magnum slots provision` again"
	case errors.Is(err, context.Canceled):
		return "interrupted; `magnum slots provision` resumes from the failed step"
	}
	return fmt.Sprintf("read %s, then run `magnum slots provision` again; it resumes from the failed step", inspTilde(logPath))
}

func (e *slotsEnv) slot(cmd, name string) (store.Slot, bool) {
	sl, err := e.st.SlotByName(context.Background(), name)
	if errors.Is(err, store.ErrNotFound) {
		cmdFail(e.c, cmd, fmt.Errorf("no slot named %q (`magnum slots list` shows them)", name))
		return sl, false
	}
	if err != nil {
		cmdFail(e.c, cmd, err)
		return sl, false
	}
	return sl, true
}

func (e *slotsEnv) repair(ctx context.Context, name string) int {
	const cmd = "slots repair"
	sl, ok := e.slot(cmd, name)
	if !ok {
		return 1
	}
	pool := e.cfg.PoolFor(sl.RepoFullName)
	if sl.Kind != store.SlotKindPool || pool == nil {
		return cmdFail(e.c, cmd, fmt.Errorf("%s is not a pool slot; per-PR worktrees are recreated by their next round", name))
	}
	unlock, code, done := e.inProcess(ctx, cmd, engine.ReqRepair, engine.RepairPayload{Slot: name})
	if done {
		return code
	}
	defer unlock()
	logPath := filepath.Join(e.c.Layout.Logs(), "provision-"+name+".log")
	fmt.Fprintf(e.c.Stdout, "repairing %s (%s)\n  setup log: %s\n", name, sl.State, inspTilde(logPath))
	if err := e.ops.Repair(ctx, sl, *pool); err != nil {
		return cmdFail(e.c, cmd, fmt.Errorf("%s: %w\n  fix: %s", name, err, slotsProvisionFix(err, name, logPath)))
	}
	fmt.Fprintf(e.c.Stdout, "%s is repaired (free)\n", name)
	return 0
}

func (e *slotsEnv) adopt(ctx context.Context, path string) int {
	const cmd = "slots adopt"
	abs, err := filepath.Abs(path)
	if err != nil {
		return cmdFail(e.c, cmd, err)
	}
	if _, err := os.Stat(abs); err != nil && !os.IsNotExist(err) {
		return cmdFail(e.c, cmd, err)
	}
	var pool *config.Pool
	for i := range e.cfg.Pools {
		for n := 1; n <= 999 && pool == nil; n++ {
			if filepath.Clean(e.cfg.Pools[i].Path(n)) == filepath.Clean(abs) {
				pool = &e.cfg.Pools[i]
			}
		}
	}
	switch {
	case len(e.cfg.Pools) == 0:
		return cmdFail(e.c, cmd, errors.New("config.toml has no [[pool]]: adopting a checkout needs a pool whose slot_path names it"))
	case pool == nil:
		return cmdFail(e.c, cmd, fmt.Errorf("%s is not a pool slot path (slot_path in config.toml, e.g. %s)", inspTilde(abs),
			inspTilde(e.cfg.Pools[0].Path(1))))
	}
	unlock, code, done := e.inProcess(ctx, cmd, engine.ReqAdopt, engine.AdoptPayload{Pool: pool.Repo, Path: abs})
	if done {
		return code
	}
	defer unlock()
	sl, err := e.ops.Adopt(ctx, *pool, abs)
	if err != nil {
		fix := "the folder must be a worktree of " + inspTilde(pool.MainClone) + " set up with bin/worktree-setup under its slot name"
		if errors.Is(err, store.ErrConflict) {
			fix = "it is already registered (`magnum slots list`)"
		}
		return cmdFail(e.c, cmd, fmt.Errorf("%w\n  fix: %s", err, fix))
	}
	fmt.Fprintf(e.c.Stdout, "adopted %s as %s (%s)\n", inspTilde(abs), sl.Name, sl.State)
	return 0
}

// pin pins or unpins a slot: through the daemon when it runs, else in
// process under the locks (mirroring the daemon: the slot's PR too).
func (e *slotsEnv) pin(ctx context.Context, name string, pin bool) int {
	cmd := map[bool]string{true: "slots pin", false: "slots unpin"}[pin]
	sl, ok := e.slot(cmd, name)
	if !ok {
		return 1
	}
	kind := map[bool]string{true: engine.ReqPin, false: engine.ReqUnpin}[pin]
	handOff := func() int {
		out, err := inspHandOff(ctx, e.c, e.st, kind, engine.TargetPayload{Slot: sl.Name}, inspHandoffWait)
		if err != nil {
			return cmdFail(e.c, cmd, err)
		}
		return out.print(e.c.Stdout, e.c.Stderr)
	}
	unlock, who, err := acquireOps(e.c.Layout)
	switch {
	case err != nil:
		return cmdFail(e.c, cmd, err)
	case who == opsBusy:
		return cmdFail(e.c, cmd, opsBusyErr(e.c.Layout))
	case who == opsDaemon:
		return handOff()
	}
	defer unlock()
	if pin {
		err = e.ops.Pin(ctx, sl)
	} else {
		err = e.ops.Unpin(ctx, sl)
	}
	if err != nil {
		return cmdFail(e.c, cmd, err)
	}
	if sl.PRID != nil {
		if err := e.st.UpdatePR(ctx, *sl.PRID, func(u *store.PRUpdate) { u.Set("pinned", pin) }); err != nil {
			return cmdFail(e.c, cmd, err)
		}
	}
	verb := map[bool]string{true: "pinned", false: "unpinned"}[pin]
	fmt.Fprintf(e.c.Stdout, "%s %s\n", verb, sl.Name)
	return 0
}
