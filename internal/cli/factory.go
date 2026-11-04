package cli

// `magnum factory`: the factory daemon's commands. The factory is a second
// daemon in this binary (internal/factory) with its own launchd job, pid,
// lock, log and registry; its commands never touch the review daemon's.

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/launchd"
	"github.com/zhuravel/magnum/internal/paths"
)

const (
	factoryUsage          = "factory <daemon|install|uninstall|status|start|answer>"
	factoryInstallUsage   = "install [--dry-run]"
	factoryUninstallUsage = "uninstall [--dry-run]"
	factoryStatusUsage    = "status [--json]"
)

func newFactoryCmd(c *Context) *cobra.Command {
	cmd := newCommand(groupDaemon, factoryUsage, "the factory: a second daemon that writes code from a Jira key or a prompt",
		"The factory takes a Jira key or a free prompt and works it up to a PR ready for review, with agents in a "+
			"herdr session of its own ([factory] herdr_session). It is a second daemon with its own launchd job "+
			"("+factory.LaunchLabel+"), registry, pid, lock and log (logs/factory.log), apart from the review "+
			"daemon's: `magnum factory install` loads the job (and reloads it once, restarting a running factory "+
			"daemon on this build), `uninstall` removes it, `status` lists the tasks, `start` hands it a task, "+
			"`answer` answers a task's questions, and `daemon` is what the job runs.",
		func(pos []string) int {
			if len(pos) == 0 {
				return inspUsage(c, "factory", "which factory command?", "<daemon|install|uninstall|status|start|answer>")
			}
			return inspUsage(c, "factory", fmt.Sprintf("unknown factory command %q", pos[0]), "<daemon|install|uninstall|status|start|answer>")
		})
	cmd.AddCommand(newFactoryDaemonCmd(c), newFactoryInstallCmd(c), newFactoryUninstallCmd(c), newFactoryStatusCmd(c),
		newFactoryStartCmd(c), newFactoryAnswerCmd(c))
	return cmd
}

func newFactoryInstallCmd(c *Context) *cobra.Command {
	var dry bool
	cmd := newCommand("", factoryInstallUsage, "write and load the factory daemon's launchd agent",
		"Create the state directories, write the factory daemon's launchd agent (~/Library/LaunchAgents/"+
			factory.LaunchLabel+".plist, running `magnum factory daemon`, its launchd output in "+
			"logs/factory-launchd.log) and load it: bootout, then bootstrap, once. A running factory daemon "+
			"drains (it starts no new launch or effect and ends its tick) and launchd starts it again on this "+
			"build. The binary launchd will run checks the configuration first (`<binary> config`); the install "+
			"is refused when it fails. --dry-run prints the plist and the launchctl calls.",
		func(pos []string) int { return runFactoryInstallCmd(c, dry, pos) })
	cmd.Flags().BoolVar(&dry, "dry-run", false, "print what would be written and run; change nothing")
	return cmd
}

func runFactoryInstallCmd(c *Context, dry bool, pos []string) int {
	if !daemonGroupNoArgs(c, "factory install", "factory "+factoryInstallUsage, pos) {
		return 2
	}
	if !loadDaemonGroupConfig(c, "factory install") {
		return 1
	}
	ctx, stop := signalContext()
	defer stop()
	out := c.Stdout
	run := daemonSys.runner(dry, out)
	fail := func(format string, a ...any) int {
		fmt.Fprintf(c.Stderr, "magnum factory install: "+format+"\n", a...)
		return 1
	}
	if !dry {
		if err := c.Layout.EnsureDirs(); err != nil {
			return fail("create %s: %v\nfix: make it writable, then re-run magnum factory install", c.Layout.State(), err)
		}
	}
	bin := c.Layout.Binary()
	if _, err := os.Stat(bin); err != nil && !dry {
		return fail("%s is missing\nfix: build it with `make build` in the checkout (or install magnum), then re-run magnum factory install", bin)
	}
	userHome, err := daemonSys.UserHome()
	if err != nil {
		return fail("find your home directory: %v\nfix: set HOME, then re-run magnum factory install", err)
	}
	prefix := launchPrefix(c.Layout, c.Config, resolveMisePath(ctx, run))
	plist := launchd.Plist(magnumPlistOptions(factory.LaunchLabel, c.Layout, userHome, prefix, c.Config.Factory.HerdrSocket, daemonConfigOverride(c)))
	path := launchd.AgentPath(userHome, factory.LaunchLabel)
	uid := daemonSys.UID()
	if !verifyBuild(ctx, c, run, "factory install") {
		return 1
	}
	if dry {
		fmt.Fprintf(out, "dry-run: would write %s:\n\n%s\n", path, plist)
		fmt.Fprintf(out, "dry-run: would run launchctl bootout gui/%d/%s, launchctl enable gui/%d/%s, launchctl bootstrap gui/%d %s\n",
			uid, factory.LaunchLabel, uid, factory.LaunchLabel, uid, path)
		return 0
	}
	if err := launchd.Install(ctx, run, uid, path, plist); err != nil {
		return fail("%v\nfix: check `launchctl print gui/%d/%s`, %s and %s, then re-run magnum factory install",
			err, uid, factory.LaunchLabel, c.Layout.FactoryLog(), factoryLaunchdLog(c.Layout))
	}
	fmt.Fprintf(out, "launchd: loaded %s from %s\n", factory.LaunchLabel, path)
	if st, err := launchd.Status(ctx, run, uid, factory.LaunchLabel); err == nil {
		fmt.Fprintf(out, "launchd: %s\n", describeLaunchdState(st))
	}
	fmt.Fprintf(out, "logs: %s (launchd's stdout/stderr: %s)\n", c.Layout.FactoryLog(), factoryLaunchdLog(c.Layout))
	return 0
}

func newFactoryUninstallCmd(c *Context) *cobra.Command {
	var dry bool
	cmd := newCommand("", factoryUninstallUsage, "unload and delete the factory daemon's launchd agent (keeps its state)",
		"Unload and delete the factory daemon's launchd agent, which stops the factory daemon it runs (it drains "+
			"first). The factory's registry, task directories, copies and log are kept, and the review daemon's job "+
			"is left alone.",
		func(pos []string) int { return runFactoryUninstallCmd(c, dry, pos) })
	cmd.Flags().BoolVar(&dry, "dry-run", false, "print what would be removed; change nothing")
	return cmd
}

func runFactoryUninstallCmd(c *Context, dry bool, pos []string) int {
	if !daemonGroupNoArgs(c, "factory uninstall", "factory "+factoryUninstallUsage, pos) {
		return 2
	}
	userHome, err := daemonSys.UserHome()
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory uninstall: find your home directory: %v\nfix: set HOME, then re-run magnum factory uninstall\n", err)
		return 1
	}
	uid := daemonSys.UID()
	path := launchd.AgentPath(userHome, factory.LaunchLabel)
	if dry {
		fmt.Fprintf(c.Stdout, "dry-run: would unload gui/%d/%s and delete %s\n", uid, factory.LaunchLabel, path)
		return 0
	}
	if err := launchd.Uninstall(context.Background(), daemonSys.runner(false, c.Stdout), uid, factory.LaunchLabel, path); err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory uninstall: %v\nfix: run `launchctl bootout gui/%d/%s`, then re-run magnum factory uninstall\n",
			err, uid, factory.LaunchLabel)
		return 1
	}
	fmt.Fprintf(c.Stdout, "launchd: unloaded gui/%d/%s and removed %s\n", uid, factory.LaunchLabel, path)
	return 0
}

func newFactoryStatusCmd(c *Context) *cobra.Command {
	var asJSON bool
	cmd := newCommand("", factoryStatusUsage, "the factory daemon and its tasks",
		"Print whether the factory daemon runs and list the factory's tasks: each one's key (its Jira key, or "+
			"`task <id>` for a free prompt), kind, state, stage and what it waits on. --json prints the same as "+
			"JSON. It never creates the factory's registry, and it refuses to migrate one while a factory daemon "+
			"of an older build runs on it.",
		func(pos []string) int { return runFactoryStatusCmd(c, asJSON, pos) })
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// factoryStatusJSON is `magnum factory status --json`.
type factoryStatusJSON struct {
	Daemon factoryDaemonJSON `json:"daemon"`
	Tasks  []factoryTaskJSON `json:"tasks"`
}

type factoryDaemonJSON struct {
	Running bool `json:"running"`
	PID     int  `json:"pid,omitempty"`
}

type factoryTaskJSON struct {
	ID        int64     `json:"id"`
	Key       string    `json:"key"`
	JiraKey   string    `json:"jira_key,omitempty"`
	Kind      string    `json:"kind"`
	Repo      string    `json:"repo"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	Stage     string    `json:"stage"`
	WaitsWho  string    `json:"waits_who,omitempty"`
	WaitsWhat string    `json:"waits_what,omitempty"`
	Branch    string    `json:"branch,omitempty"`
	Copy      string    `json:"copy,omitempty"`
	PRNumber  int       `json:"pr_number,omitempty"`
	Updated   time.Time `json:"updated"`
}

func runFactoryStatusCmd(c *Context, asJSON bool, pos []string) int {
	if !daemonGroupNoArgs(c, "factory status", "factory "+factoryStatusUsage, pos) {
		return 2
	}
	pid := runningFactoryDaemon(c)
	var tasks []registry.Task
	if fsx.Exists(c.Layout.FactoryDB()) {
		reg, err := openFactoryRegistry(c.Layout)
		if err != nil {
			fmt.Fprintf(c.Stderr, "magnum factory status: %v\n", err)
			return 1
		}
		tasks, err = reg.Tasks(context.Background())
		reg.Close()
		if err != nil {
			fmt.Fprintf(c.Stderr, "magnum factory status: %v\n", err)
			return 1
		}
	}
	if asJSON {
		out := factoryStatusJSON{Daemon: factoryDaemonJSON{Running: pid > 0, PID: pid}, Tasks: []factoryTaskJSON{}}
		for _, t := range tasks {
			out.Tasks = append(out.Tasks, factoryTaskJSON{ID: t.ID, Key: factory.TaskKey(t), JiraKey: t.JiraKey, Kind: t.Kind,
				Repo: t.Repo, Title: t.Title, State: t.State, Stage: t.Stage, WaitsWho: t.WaitsWho, WaitsWhat: t.WaitsWhat,
				Branch: t.Branch, Copy: t.Copy, PRNumber: t.PRNumber, Updated: t.Updated})
		}
		if err := writeJSON(c.Stdout, out); err != nil {
			fmt.Fprintf(c.Stderr, "magnum factory status: %v\n", err)
			return 1
		}
		return 0
	}
	if pid > 0 {
		fmt.Fprintf(c.Stdout, "factory daemon: running (pid %d)\n", pid)
	} else {
		fmt.Fprintln(c.Stdout, "factory daemon: not running (`magnum factory install` loads it)")
	}
	if len(tasks) == 0 {
		fmt.Fprintln(c.Stdout, "no tasks")
		return 0
	}
	tw := inspTable(c.Stdout)
	fmt.Fprintln(tw, "\nID\tKEY\tKIND\tSTATE\tSTAGE\tWAITS ON")
	for _, t := range tasks {
		waits := "-"
		if t.WaitsWho != "" || t.WaitsWhat != "" {
			waits = t.WaitsWho + ": " + t.WaitsWhat
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", t.ID, factory.TaskKey(t), t.Kind, t.State, orDash(t.Stage), waits)
	}
	_ = tw.Flush()
	return 0
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// runningFactoryDaemon is the pid of the running factory daemon, 0 for
// none: a pidfile whose process ps shows as something else is stale (its
// pid was reused).
func runningFactoryDaemon(c *Context) int {
	pid, err := daemonSys.factoryPID(c.Layout)
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory status: warning: %v\n", err)
		return 0
	}
	if pid == 0 {
		return 0
	}
	comm, args, err := engine.ProcessCommand(context.Background(), daemonSys.runner(false, nil), pid)
	if err == nil && !factory.LooksLikeFactoryDaemon(comm, args) {
		return 0
	}
	return pid
}

// openFactoryRegistry opens the factory's registry for a CLI command: one
// that needs a migration is refused while a factory daemon runs on it
// (refuseWhileDaemonRuns).
func openFactoryRegistry(l paths.Layout) (*registry.Registry, error) {
	return registry.Open(l.FactoryDB(), registry.Options{BeforeMigrate: refuseWhileDaemonRuns(l)})
}

// refuseWhileDaemonRuns is the factory CLI's registry.Options.BeforeMigrate:
// a build that needs a newer factory schema must not migrate the registry
// under a running factory daemon, whose queries target the old one. A stale
// pidfile naming a process that is not a factory daemon does not count; a
// live pid that ps cannot identify does; a pidfile that cannot be read
// names no daemon, as the review CLI's migrateGuard reads it. The factory
// daemon migrates the file itself when it starts on the new build.
func refuseWhileDaemonRuns(l paths.Layout) func(from, to int) error {
	return func(from, to int) error {
		pid, _ := daemonSys.factoryPID(l) // an unreadable pidfile names no daemon
		if pid == 0 {
			return nil
		}
		comm, args, err := engine.ProcessCommand(context.Background(), daemonSys.runner(false, nil), pid)
		if err == nil && !factory.LooksLikeFactoryDaemon(comm, args) {
			return nil
		}
		return fmt.Errorf("the factory registry is at schema %d and this build needs %d, but the factory daemon (pid %d) "+
			"still runs on it; migrating now would break the daemon's queries\nfix: run `magnum factory install` (it "+
			"restarts the factory daemon on this build, which migrates), then retry", from, to, pid)
	}
}
