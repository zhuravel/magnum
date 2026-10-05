package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/launchd"
)

const daemonUsage = "daemon [--once] [--dry-run [--json]] [--log-level LEVEL]"

func newDaemonCmd(c *Context) *cobra.Command {
	var f daemonFlags
	cmd := newCommand(groupDaemon, daemonUsage, "run the review daemon in the foreground (what launchd runs)",
		"Run the review daemon in the foreground; the launchd agent runs exactly this through `mise exec`. It "+
			"polls GitHub, schedules and runs review rounds in herdr panes, answers CLI requests and releases "+
			"storage of closed PRs until SIGTERM or ctrl+c. Only one daemon runs per home: a second one exits "+
			"at once. --once runs startup and one tick, waits for the rounds and heavy jobs it started, then "+
			"exits; --dry-run makes every decision but changes nothing and prints the planned side effects on exit.",
		func(pos []string) int { return runDaemonCmd(c, f, pos) })
	fs := cmd.Flags()
	fs.BoolVar(&f.once, "once", false, "run startup and one tick, wait for the rounds and heavy jobs it started, then exit")
	fs.BoolVar(&f.dry, "dry-run", false, "make every decision but change nothing; print the planned side effects on exit")
	fs.BoolVar(&f.json, "json", false, "with --dry-run: print the plan as JSON")
	fs.StringVar(&f.level, "log-level", "info", "log level: debug, info, warn or error")
	_ = cmd.RegisterFlagCompletionFunc("log-level", cobra.FixedCompletions([]cobra.Completion{"debug", "info", "warn", "error"}, cobra.ShellCompDirectiveNoFileComp))
	return cmd
}

// daemonFlags are the raw `magnum daemon` flags.
type daemonFlags struct {
	once, dry, json bool
	level           string
}

// daemonOptions are the parsed `magnum daemon` flags.
type daemonOptions struct {
	Once     bool
	DryRun   bool
	JSON     bool
	LogLevel slog.Level
	// Engine are the engine options runDaemonCmd prepares (daemonEngineOptions).
	Engine engine.Options
}

// daemonDryRunReport is what a dry run would have done: the engine's
// planned side effects and the mutating commands execx.DryRun held back
// (redacted).
type daemonDryRunReport struct {
	Ops      []engine.PlannedOp `json:"ops"`
	Commands []string           `json:"commands"`
}

func runDaemonCmd(c *Context, f daemonFlags, pos []string) int {
	if !daemonGroupNoArgs(c, "daemon", daemonUsage, pos) {
		return 2
	}
	opts := daemonOptions{Once: f.once, DryRun: f.dry, JSON: f.json}
	if err := opts.LogLevel.UnmarshalText([]byte(f.level)); err != nil {
		fmt.Fprintf(c.Stderr, "magnum daemon: --log-level %q: use debug, info, warn or error\n", f.level)
		return 2
	}
	if opts.JSON && !opts.DryRun {
		fmt.Fprintln(c.Stderr, "magnum daemon: --json only applies with --dry-run")
		return 2
	}
	if err := checkConfigAndPrompts(c); err != nil {
		refuseDaemonStart(c, err)
		return 1
	}
	opts.Engine = daemonEngineOptions(c, opts)

	// engine.Run installs the SIGTERM/SIGINT/SIGUSR1 handlers and returns nil
	// on a stop signal and when another daemon already holds the lock, so
	// launchd (KeepAlive SuccessfulExit=false) does not restart those exits.
	rep, err := daemonSys.RunEngine(context.Background(), c, opts)
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum daemon: %v\n", err)
		return 1
	}
	if opts.DryRun {
		if opts.JSON {
			return writeDaemonDryRunJSON(c, rep)
		}
		renderDaemonDryRun(c.Stdout, rep)
	}
	return 0
}

// refuseDaemonStart reports why the daemon will not start (an invalid
// configuration, a prompt this build cannot render) where a reader looks
// first: stderr, which launchd appends to launchd.log, and the daemon's own
// log as its newest line. launchd restarts a non-zero exit after its
// throttle, so the error repeats there until the configuration is fixed.
func refuseDaemonStart(c *Context, err error) {
	file := c.Layout.Config()
	if file == "" || !fileExists(file) {
		file = c.Layout.UserConfig
	}
	fix := fmt.Sprintf("correct the config file (%s) or the prompt files, check with `magnum config`, then `magnum daemon-restart`", inspTilde(file))
	fmt.Fprintf(c.Stderr, "magnum daemon: refusing to start: %v\nfix: %s\n", err, fix)
	if !c.Layout.Valid() {
		return
	}
	path := c.Layout.DaemonLog()
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o700); mkErr != nil {
		return
	}
	f, ferr := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if ferr != nil {
		fmt.Fprintf(c.Stderr, "magnum daemon: cannot record this in %s: %v\n", path, ferr)
		return
	}
	defer f.Close()
	slog.New(slog.NewJSONHandler(f, nil)).Error("magnum daemon refusing to start", "err", execx.Redact(err.Error()), "fix", fix)
}

// daemonAppOptions are the App options of the daemon process. Daemon is set
// here and nowhere else: the daemon owns daemon.log and is the only process
// that rotates it.
func daemonAppOptions(c *Context, o daemonOptions) app.Options {
	opts := app.Options{DryRun: o.DryRun, LogLevel: o.LogLevel, Daemon: true}
	if c.Stderr != os.Stderr {
		opts.Stderr = c.Stderr // otherwise app mirrors to os.Stderr only when it is a terminal
	}
	return opts
}

// runDaemonEngine is the real RunEngine: one App for the process, the engine
// in the foreground until a stop signal (or after one tick with --once).
func runDaemonEngine(ctx context.Context, c *Context, o daemonOptions) (daemonDryRunReport, error) {
	// engine.Run installs its own handlers; this one covers a stop signal
	// that arrives while app.New is still wiring (Run then returns nil at
	// once instead of the process dying with "killed by signal").
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	a, err := app.New(c.Config, c.Layout, daemonAppOptions(c, o))
	if err != nil {
		return daemonDryRunReport{}, err
	}
	// An installed binary's herdr plugin copy follows the binary: an upgrade
	// brings new actions to a plugin `magnum install --plugin` wrote.
	if dir := pluginDir(c.Layout); !o.DryRun && dir != c.Layout.Home && fileExists(filepath.Join(dir, "herdr-plugin.toml")) {
		if _, err := writePluginFiles(dir); err != nil {
			fmt.Fprintf(c.Stderr, "magnum daemon: refresh the herdr plugin in %s: %v\n", dir, err)
		}
	}
	defer a.Close()
	e := engine.FromApp(a)
	eo := o.Engine
	eo.Once = o.Once
	err = e.Run(ctx, eo)
	var rep daemonDryRunReport
	if o.DryRun {
		rep.Ops = e.Planned()
		if a.DryRunner != nil {
			for _, cmd := range a.DryRunner.Planned {
				rep.Commands = append(rep.Commands, execx.Redact(cmd.String()))
			}
		}
	}
	return rep, err
}

// daemonEngineOptions are the engine options of the daemon process: its
// build (recorded for the CLI's skew check: the version stamped into this
// binary and the binary launchd starts), the check a new build on disk passes
// before the daemon restarts on it, and whether launchd runs this daemon
// (it sets XPC_SERVICE_NAME to the job's label), which restart_on_new_build
// needs to start it again.
func daemonEngineOptions(c *Context, o daemonOptions) engine.Options {
	run := daemonSys.runner(false, c.Stdout)
	opts := engine.Options{Once: o.Once, Build: engine.CurrentBuild(c.Version, c.Layout.Binary()),
		Supervised: daemonSys.Getenv("XPC_SERVICE_NAME") == launchd.DefaultLabel}
	if !o.DryRun {
		opts.CheckBuild = newBuildCheck(c, run)
	}
	return opts
}

func writeDaemonDryRunJSON(c *Context, rep daemonDryRunReport) int {
	if rep.Ops == nil {
		rep.Ops = []engine.PlannedOp{}
	}
	if rep.Commands == nil {
		rep.Commands = []string{}
	}
	enc := json.NewEncoder(c.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		fmt.Fprintf(c.Stderr, "magnum daemon: %v\n", err)
		return 1
	}
	return 0
}

// renderDaemonDryRun prints the planned side effects as tables.
func renderDaemonDryRun(w io.Writer, rep daemonDryRunReport) {
	fmt.Fprintf(w, "dry run: %s, %s (nothing was changed)\n",
		daemonPlural(len(rep.Ops), "planned action", "planned actions"),
		daemonPlural(len(rep.Commands), "command", "commands"))
	if len(rep.Ops) > 0 {
		fmt.Fprintln(w)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "TIME\tSUBJECT\tACTION\tDETAIL")
		for _, op := range rep.Ops {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", op.At.Local().Format("15:04:05"), op.Subject, op.Action,
				strings.ReplaceAll(execx.Redact(op.Detail), "\n", " "))
		}
		_ = tw.Flush()
	}
	if len(rep.Commands) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "COMMANDS")
		for _, cmd := range rep.Commands {
			fmt.Fprintln(w, "  "+cmd)
		}
	}
}

func daemonPlural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
