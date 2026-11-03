package cli

// `magnum factory daemon`: what the factory's launchd job runs. The daemon
// builder (buildFactoryDaemon) wires the engine's collaborators; a task
// that brings one wires it here, with a test that the daemon still builds.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/reveal"
	"github.com/zhuravel/magnum/internal/store"
)

const factoryDaemonUsage = "daemon [--log-level LEVEL]"

func newFactoryDaemonCmd(c *Context) *cobra.Command {
	var level string
	cmd := newCommand("", factoryDaemonUsage, "run the factory daemon in the foreground (what its launchd job runs)",
		"Run the factory daemon in the foreground; the factory's launchd job runs exactly this. It ticks every "+
			"[factory] poll and on SIGUSR1, answers the factory's requests, advances its tasks and reads its panes "+
			"every [factory] observe, until SIGTERM or ctrl+c, which drain it: no new launch or effect starts and the "+
			"tick in progress ends. It loads its prompts once, at start: an edit reaches it after a restart "+
			"(`magnum factory install`). Only one factory daemon runs per home: a second one says so, naming the "+
			"first one's pid, and exits 0 before it opens a log or a store. It logs to logs/factory.log.",
		func(pos []string) int { return runFactoryDaemonCmd(c, level, pos) })
	cmd.Flags().StringVar(&level, "log-level", "info", "log level: debug, info, warn or error")
	_ = cmd.RegisterFlagCompletionFunc("log-level", cobra.FixedCompletions([]cobra.Completion{"debug", "info", "warn", "error"}, cobra.ShellCompDirectiveNoFileComp))
	return cmd
}

func runFactoryDaemonCmd(c *Context, level string, pos []string) int {
	if !daemonGroupNoArgs(c, "factory daemon", "factory "+factoryDaemonUsage, pos) {
		return 2
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory daemon: --log-level %q: use debug, info, warn or error\n", level)
		return 2
	}
	if err := checkConfigAndPrompts(c); err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory daemon: refusing to start: %v\nfix: correct the config file or the prompt files, "+
			"check with `magnum config`, then `magnum factory install`\n", err)
		return 1
	}
	// The lock comes first: a second daemon (one run by hand, or a newer
	// build's) must not open the log or migrate a store under the running
	// one. It exits 0, as the review daemon does, so launchd (KeepAlive on a
	// failed exit only) does not start it again and again.
	unlock, err := factory.Lock(c.Layout)
	if running, ok := errors.AsType[factory.ErrRunning](err); ok {
		fmt.Fprintf(c.Stderr, "magnum factory daemon: %v; exiting\n", strings.TrimPrefix(running.Error(), "factory: "))
		return 0
	}
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory daemon: %v\n", err)
		return 1
	}
	defer unlock()
	d, err := buildFactoryDaemon(c, lvl)
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory daemon: %v\n", err)
		return 1
	}
	defer d.Close()
	if err := d.Engine.RunLocked(context.Background()); err != nil {
		d.Log.Error("magnum factory daemon stopped", "err", err)
		fmt.Fprintf(c.Stderr, "magnum factory daemon: %v\n", err)
		return 1
	}
	return 0
}

// factoryDaemon is the factory daemon's process: its engine and what it
// holds open, closed by Close.
type factoryDaemon struct {
	Engine   *factory.Engine
	Registry *registry.Registry
	// Agents is the factory's agent sessions store (magnum's schema, its own
	// file, owned by the factory: no migration guard), for the runtime.
	Agents *store.Store
	Log    *slog.Logger

	closers []func() error
}

// Close closes what the daemon opened, the newest first.
func (d *factoryDaemon) Close() error {
	var errs []error
	for _, f := range slices.Backward(d.closers) {
		errs = append(errs, f())
	}
	d.closers = nil
	return errors.Join(errs...)
}

// buildFactoryDaemon builds the factory daemon of c's config and layout: its
// own log (logs/factory.log, rotated), its prompts loaded once
// (config.SnapshotPrompts: the factory roles' prompts beside the review
// roles'), its registry (Layout.FactoryDB, migrated by the daemon) and its
// agents store (Layout.FactoryAgentsDB), and the engine with the
// collaborators this build has: the memory and disk reads, a MySQL ping when
// a factory repository has databases, herdr's ping when [factory]
// herdr_socket names the session's socket, and the reveal of the factory's
// herdr session. The runtime, the copies, Jira, GitHub, magnum and the local
// reviewer are nil until the builds that bring them. The caller holds the
// factory lock (factory.Lock); the registry's migration is still refused
// while a factory daemon's pidfile names a live factory daemon
// (refuseWhileDaemonRuns: this daemon writes its own pidfile later, in
// RunLocked).
func buildFactoryDaemon(c *Context, level slog.Level) (*factoryDaemon, error) {
	cfg, l := c.Config, c.Layout
	if err := l.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("factory: state dirs: %w", err)
	}
	// A failed build closes what it opened. The daemon is a local, not the
	// named result: `return nil, err` would set that to nil before the
	// deferred cleanup ran.
	d := &factoryDaemon{}
	built := false
	defer func() {
		if !built {
			_ = d.Close()
		}
	}()
	logf, err := app.OpenRotating(l.FactoryLog(), app.LogMaxBytes, app.LogKeep)
	if err != nil {
		return nil, fmt.Errorf("factory: log: %w", err)
	}
	d.closers = append(d.closers, logf.Close)
	var mirror io.Writer
	if c.Stderr != os.Stderr || app.Interactive(os.Stderr) {
		mirror = c.Stderr
	}
	d.Log = app.NewLogger(logf, mirror, level)

	snap, err := cfg.SnapshotPrompts("", time.Now(), factoryPromptNames(cfg)...)
	if err != nil {
		return nil, fmt.Errorf("factory: %w", err)
	}
	d.Log.Info("prompts loaded", "files", snap.Files())

	if d.Registry, err = registry.Open(l.FactoryDB(), registry.Options{BeforeMigrate: refuseWhileDaemonRuns(l)}); err != nil {
		return nil, err
	}
	d.closers = append(d.closers, d.Registry.Close)
	if d.Agents, err = store.Open(l.FactoryAgentsDB()); err != nil {
		return nil, fmt.Errorf("factory: agents store: %w", err)
	}
	d.closers = append(d.closers, d.Agents.Close)

	run := &execx.Real{Log: app.Printf{Logger: d.Log, Level: slog.LevelInfo, Src: "exec"}}
	quiet := &execx.Real{} // the reads of every tick (sysctl) stay out of the log
	git := gitx.New(run)
	git.HTTPSFetch = true
	deps := factory.Deps{
		Config: cfg, Layout: l, Registry: d.Registry, Git: git, Log: d.Log,
		Memory:  func(ctx context.Context) engine.MemorySample { return engine.ReadMemory(ctx, quiet, time.Now()) },
		Disk:    inspDiskFree,
		HerdrUp: factoryHerdrUp(cfg),
		Reveal:  factoryReveal(cfg, run),
	}
	if ping, closer := factoryMySQL(cfg, d.Log); ping != nil {
		deps.MySQL = ping
		d.closers = append(d.closers, closer)
	}
	d.Engine = factory.New(deps)
	built = true
	return d, nil
}

// factoryPromptNames are the prompt files the factory roles name, each
// once, for the daemon's prompt snapshot.
func factoryPromptNames(cfg *config.Config) []string {
	var names []string
	for _, r := range cfg.Factory.Roles {
		for _, kind := range config.PromptKinds {
			if n := r.PromptFile(kind); n != "" && !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
	}
	return names
}

// factoryHerdrUp pings the factory's herdr session at [factory]
// herdr_socket; nil (herdr taken as up) while the setting is empty: the
// runtime, which resolves the named session's own socket, brings its ping.
func factoryHerdrUp(cfg *config.Config) func(ctx context.Context) error {
	socket := cfg.Factory.HerdrSocket
	if socket == "" {
		return nil
	}
	cl := &herdr.Client{Socket: socket}
	return func(ctx context.Context) error {
		_, err := cl.Ping(ctx)
		return err
	}
}

// factoryReveal opens a terminal on the factory's herdr session
// (`herdr --session <herdr_session>`) through internal/reveal, in the
// configured terminal app, in a new window.
func factoryReveal(cfg *config.Config, run execx.Runner) func(ctx context.Context) error {
	term := cfg.Terminal
	term.Session = cfg.Factory.HerdrSession
	return func(ctx context.Context) error {
		_, err := reveal.Reveal(ctx, run, term, "", reveal.Options{NewWindow: true})
		return err
	}
}

// factoryMySQL pings the local MySQL server the copies' databases live on
// ($MAGNUM_MYSQL_DSN, else mysqlx's default), when a factory repository has
// databases; nil otherwise.
func factoryMySQL(cfg *config.Config, log *slog.Logger) (ping func(ctx context.Context) error, closer func() error) {
	if !slices.ContainsFunc(cfg.Factory.Repos, func(r config.FactoryRepo) bool { return len(r.Databases) > 0 }) {
		return nil, nil
	}
	my, err := mysqlx.Open(os.Getenv("MAGNUM_MYSQL_DSN"))
	if err != nil {
		log.Warn("mysql client unavailable; the factory pings no database", "err", err)
		return nil, nil
	}
	return my.Ping, my.Close
}
