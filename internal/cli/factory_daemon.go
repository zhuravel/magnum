package cli

// `magnum factory daemon`: what the factory's launchd job runs. The daemon
// builder (buildFactoryDaemon) wires the engine's collaborators; a task
// that brings one wires it here, with a test that the daemon still builds.

import (
	"cmp"
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
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/jira"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/reveal"
	"github.com/zhuravel/magnum/internal/slots"
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
	// Runtime drives the factory's agents in its herdr session over Agents.
	Runtime *runtime.Runtime
	// Copies are the repositories' copies the tasks work in: magnum's slot
	// code over Agents, never the review registry.
	Copies factory.Copies
	// Guard is the herdr the copies' guard sees: the factory's session and
	// yours.
	Guard factory.GuardPanes
	Jira  *jira.Client
	// GitHub opens the PRs the factory publishes and reads them back: gh as
	// your gh identity (factory.NewGitHub).
	GitHub factory.GitHub
	// Notifier shows the alerts' toasts on your herdr session (osascript
	// after), deduplicated in Agents; Sidebar reports the tasks' sidebar
	// tokens on the factory's session.
	Notifier, Sidebar *notify.Notifier
	Log               *slog.Logger

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
// (config.SnapshotPrompts: the factory's prompts, factory.PromptNames, beside
// the review roles', with factory-shared.md) and rendered once with this build
// (factory.CheckPrompts), its registry (Layout.FactoryDB, migrated by the daemon) and its
// agents store (Layout.FactoryAgentsDB), and the engine with the
// collaborators this build has: the memory and disk reads, a MySQL ping when
// a factory repository has databases, the agent runtime over the agents
// store in the factory's herdr session (factoryHerdrSocket), whose snapshot
// is the herdr ping, the reveal of that session, and the copies (a second
// slot manager over the agents store, whose guard sees the panes of the
// factory's herdr session and of yours, factory.GuardPanes, with the
// runtime's owner rows), the [jira] site's client when [jira] is on (intake's
// tickets), and the runner of the overlap line's git and gh reads and of the
// coding stage's proof and precheck, and the local review: magnum's
// registry read for its real rounds (factory.NewMagnum, never migrated) and
// the local rounds (factory.NewLocalReviewer: evalrun.RunLocal with the
// review candidate this build writes at startup, factory.WriteReviewCandidate,
// as its config's candidate), the publish's GitHub (factory.NewGitHub:
// gh as your gh identity), and the alerts' notify (toasts on your herdr
// session, deduplicated in the agents store; sidebar tokens on the
// factory's). Every command of the daemon's runner, git's and
// gh's included, goes through the deny list (factory.Guard).
// The caller holds the factory lock (factory.Lock); the registry's migration
// is still refused
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

	snap, err := cfg.SnapshotPrompts("", time.Now(), factory.PromptNames(cfg)...)
	if err != nil {
		return nil, fmt.Errorf("factory: %w", err)
	}
	if err := factory.CheckPrompts(cfg); err != nil {
		return nil, fmt.Errorf("factory: the factory roles' prompts do not render with this build "+
			"(fix them, check with `magnum config`, then restart the factory daemon):\n%w", err)
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

	// Every command the factory runs goes through the deny list: no remote
	// console, kubectl exec or gh-ost, whatever wraps it (factory.Guard).
	run := factory.Guard(&execx.Real{Log: app.Printf{Logger: d.Log, Level: slog.LevelInfo, Src: "exec"}}, cfg)
	quiet := &execx.Real{} // the reads of every tick (sysctl) stay out of the log
	git := gitx.New(run)
	git.HTTPSFetch = true
	memory := func(ctx context.Context) engine.MemorySample { return engine.ReadMemory(ctx, quiet, time.Now()) }
	h := &herdr.Client{Socket: factoryHerdrSocket(cfg), Logger: app.Printf{Logger: d.Log, Level: slog.LevelDebug, Src: "herdr"}}
	if d.Runtime, err = runtime.New(runtime.Deps{Config: cfg, Layout: l, Herdr: h, Store: d.Agents, Registry: d.Registry,
		Runner: run, Memory: memory, Log: d.Log}); err != nil {
		return nil, fmt.Errorf("factory: runtime: %w", err)
	}
	deps := factory.Deps{
		Config: cfg, Layout: l, Registry: d.Registry, Runtime: d.Runtime, Git: git, Run: run, Log: d.Log,
		Memory:  memory,
		Disk:    inspDiskFree,
		HerdrUp: d.Runtime.HerdrUp,
		Reveal:  factoryReveal(cfg, run),
	}
	// The tickets of intake: the [jira] site's client, reading with curl
	// (the token on stdin, never in a pane).
	if cfg.Jira.On() {
		d.Jira = &jira.Client{Runner: run, URL: cfg.Jira.URL, User: cfg.Jira.User, TokenFile: cfg.Jira.TokenFile}
		deps.Jira = d.Jira
	}
	var copyDBs slots.MySQL
	if my := factoryMySQL(cfg, d.Log); my != nil {
		deps.MySQL, copyDBs = my.Ping, my
		d.closers = append(d.closers, my.Close)
	}
	// The guard of a copy sees the panes of both herdr sessions, yours with
	// their ids prefixed: each session numbers its own from w1, and the
	// agents store knows only the factory's.
	// A factory in your own session (the same socket) is one session.
	d.Guard = factory.GuardPanes{Factory: h}
	yours := h
	if ys := cmp.Or(cfg.Herdr.Socket, herdr.DefaultSocket()); ys != h.Socket {
		yours = &herdr.Client{Socket: ys, Logger: app.Printf{Logger: d.Log, Level: slog.LevelDebug, Src: "herdr"}}
		d.Guard.Yours = yours
	}
	// The alerts (spec 9.F): magnum's toasts on your session, where your
	// terminal shows them (osascript when herdr cannot), as [herdr] notify
	// allows, each deduplicated in the agents store so a restart repeats
	// none; the tasks' sidebar tokens on the factory's session, where their
	// workspaces are.
	d.Notifier = &notify.Notifier{Herdr: yours, Store: d.Agents, Runner: run, Enabled: cfg.Herdr.Notify,
		Log: app.Printf{Logger: d.Log, Level: slog.LevelInfo, Src: "notify"}}
	d.Sidebar = &notify.Notifier{Herdr: h, Log: app.Printf{Logger: d.Log, Level: slog.LevelInfo, Src: "notify"}}
	deps.Notifier, deps.Sidebar = d.Notifier, d.Sidebar.Sidebar
	copySlots := slots.New(slots.Deps{Store: d.Agents, Run: run, Git: git, MySQL: copyDBs,
		Snapshot: d.Guard.Snapshot, ProcessInfo: d.Guard.ProcessInfo, Layout: l,
		Log: app.Printf{Logger: d.Log, Level: slog.LevelInfo, Src: "copies"}, SecretEnv: identityKeyEnvs(cfg)})
	d.Copies = factory.NewCopies(cfg, copySlots, d.Agents, git, run, copyDBs, h.Snapshot, d.Runtime.Owner)
	deps.Copies = d.Copies
	// The publish: gh as your gh identity, the one the local review runs as.
	d.GitHub = factory.NewGitHub(cfg, run)
	deps.GitHub = d.GitHub
	// The local review: this build's prompts and judge skill as the review
	// candidate, written once at startup as the prompts are loaded.
	candidate, err := factory.WriteReviewCandidate(l)
	if err != nil {
		return nil, err
	}
	reviewCfg, err := cfg.WithCandidate(candidate)
	if err != nil {
		return nil, fmt.Errorf("factory: review candidate: %w", err)
	}
	deps.Magnum, deps.ReviewCandidate = factory.NewMagnum(l, cfg), candidate
	deps.Review = factory.NewLocalReviewer(evalrun.Deps{Layout: l, Config: reviewCfg, Logger: d.Log, Stderr: io.Discard, Runner: run,
		Warn: func(format string, args ...any) { d.Log.Warn("local review: " + fmt.Sprintf(format, args...)) }})
	d.Engine = factory.New(deps)
	built = true
	return d, nil
}

// factoryHerdrSocket is the socket of the factory's herdr session: [factory]
// herdr_socket, else the named session's own,
// ~/.config/herdr/sessions/<herdr_session>/herdr.sock (herdr's default
// session, "" or "default", keeps herdr's default socket).
func factoryHerdrSocket(cfg *config.Config) string {
	if s := cfg.Factory.HerdrSocket; s != "" {
		return s
	}
	switch name := cfg.Factory.HerdrSession; name {
	case "", "default":
		return paths.Expand("~/.config/herdr/herdr.sock")
	default:
		return paths.Expand("~/.config/herdr/sessions/" + name + "/herdr.sock")
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

// factoryMySQL is the client of the local MySQL server the copies'
// databases live on ($MAGNUM_MYSQL_DSN, else mysqlx's default), when a
// factory repository has databases: the engine pings it, and the copies
// read and verify their databases through it. nil otherwise.
func factoryMySQL(cfg *config.Config, log *slog.Logger) *mysqlx.Client {
	if !slices.ContainsFunc(cfg.Factory.Repos, func(r config.FactoryRepo) bool { return len(r.Databases) > 0 }) {
		return nil
	}
	my, err := mysqlx.Open(os.Getenv("MAGNUM_MYSQL_DSN"))
	if err != nil {
		log.Warn("mysql client unavailable; the factory pings no database", "err", err)
		return nil
	}
	return my
}

// identityKeyEnvs are the variables of the daemon's environment that hold an
// App identity's private key ([[identity]] private_key_env): no copy's
// command inherits them.
func identityKeyEnvs(cfg *config.Config) []string {
	var out []string
	for _, id := range cfg.Identities {
		if id.PrivateKeyEnv != "" {
			out = append(out, id.PrivateKeyEnv)
		}
	}
	return out
}
