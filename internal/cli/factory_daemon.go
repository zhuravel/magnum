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
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
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
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/identity"
	"github.com/zhuravel/magnum/internal/jira"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/reveal"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/staging"
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
		fmt.Fprintf(c.Stderr, "magnum factory daemon: refusing to start: %v\nfix: correct the file the error names (`magnum config` "+
			"checks the config file, `magnum factory daemon` in a terminal the factory's prompts), then `magnum factory install`\n", err)
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
	d, err := startFactoryDaemon(context.Background(), c, lvl)
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

// startFactoryDaemon readies the daemon's environment as the review daemon
// does (daemonEnv: a locale when launchd gave none, and the login shell's
// PATH with [daemon] shell_path) before the factory daemon is built,
// because the copies' setup, reset and precheck commands run in the
// daemon's own environment and need what only the shell's exports name
// (DBngin's redis-server) and a UTF-8 locale (rubocop). The factory log then
// says what it set and what the shell added.
func startFactoryDaemon(ctx context.Context, c *Context, lvl slog.Level) (*factoryDaemon, error) {
	locale, s := daemonEnv(ctx, c.Config)
	d, err := buildFactoryDaemon(c, lvl)
	if err == nil {
		locale.log(d.Log)
	}
	if err != nil || s == nil {
		return d, err
	}
	if s.Failed != "" {
		d.Log.Warn(s.Message())
	} else {
		d.Log.Info(s.Message())
	}
	if len(s.Added) > 0 {
		d.Log.Debug("PATH from the login shell", "shell", s.Shell, "added", execx.Redact(strings.Join(s.Added, ":")))
	}
	return d, nil
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
	Guard herdr.GuardView
	Jira  *jira.Client
	// GitHub opens the PRs the factory publishes and reads them back: gh as
	// your gh identity (factory.NewGitHub).
	GitHub factory.GitHub
	// Notifier shows the alerts' toasts on your herdr session (osascript
	// after), deduplicated in Agents; Sidebar reports the tasks' sidebar
	// tokens on the factory's session.
	Notifier, Sidebar *notify.Notifier
	Log               *slog.Logger
	// GitRun runs the factory's git (its gitx client, the copies' slot
	// code): the guarded runner in the C locale (cLocale).
	GitRun execx.Runner
	// Staging reads where the published tasks' PRs stand on their
	// repositories' staging envs (internal/staging's read side,
	// factoryStaging): nil when no [[factory.repo]] has staging entries.
	// The engine closes it as the daemon stops.
	Staging factory.Staging

	closers []func() error
}

// cLocale runs every command of its runner with LC_ALL=C. The factory reads
// git's refusals from its English messages (a switch refused over your
// untracked file, a branch that already exists), and a daemon started in a
// terminal of another language with a git that ships translations would
// read them as plain failures. Only the factory's git runs in it; magnum's
// own git keeps the environment it has.
type cLocale struct{ execx.Runner }

func (r cLocale) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	env := make(map[string]string, len(c.Env)+1)
	maps.Copy(env, c.Env)
	env["LC_ALL"] = "C"
	c.Env = env
	return r.Runner.Run(ctx, c)
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
// store in the factory's herdr session (Config.FactoryHerdr), whose snapshot
// is the herdr ping, the reveal of that session, and the copies (a second
// slot manager over the agents store, whose guard sees the panes of the
// factory's herdr session and of yours, herdr.GuardView, with the
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
		return nil, fmt.Errorf("factory: the factory roles' prompts do not render with this build (fix the file the error "+
			"below names, check it with `magnum factory daemon` in a terminal, then restart the daemon with `magnum factory install`):\n%w", err)
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
	d.GitRun = cLocale{run}
	git := gitx.New(d.GitRun)
	git.HTTPSFetch = true
	memory := func(ctx context.Context) engine.MemorySample { return engine.ReadMemory(ctx, quiet, time.Now()) }
	h := &herdr.Client{Socket: cfg.FactoryHerdr().Socket, Logger: app.Printf{Logger: d.Log, Level: slog.LevelDebug, Src: "herdr"}}
	if d.Runtime, err = runtime.New(runtime.Deps{Config: cfg, Layout: l, Herdr: h, Store: d.Agents, Registry: d.Registry,
		Runner: run, Memory: memory, Log: d.Log}); err != nil {
		return nil, fmt.Errorf("factory: runtime: %w", err)
	}
	deps := factory.Deps{
		Config: cfg, Layout: l, Registry: d.Registry, Runtime: d.Runtime, Git: git, Run: run, Log: d.Log,
		Memory: memory,
		// The memory hold names what holds the memory: two ps reads, never a kill.
		MemProcs: func(ctx context.Context) ([]factory.MemProc, error) { return factory.ReadMemProcs(ctx, quiet) },
		Disk:     inspDiskFree,
		HerdrUp:  d.Runtime.HerdrUp,
		Reveal:   factoryReveal(cfg, run),
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
	// The guard of a copy sees the panes of the herdr sessions, the others'
	// with their ids prefixed: each session numbers its own from w1, and the
	// agents store knows only the factory's. Your server not running shows
	// the agents herdr saved, which it restores when it starts.
	// A factory in your own session (the same socket) is one session. The
	// local review's rounds open in magnum's review session, in the copies:
	// a pane you open there holds a copy too, so a review session that is
	// neither is a third view.
	herdrLog := app.Printf{Logger: d.Log, Level: slog.LevelDebug, Src: "herdr"}
	d.Guard = herdr.GuardView{Own: h}
	yours := h
	if ys := cmp.Or(cfg.Herdr.Socket, herdr.DefaultSocket()); ys != h.Socket {
		yours = &herdr.Client{Socket: ys, Logger: herdrLog}
		d.Guard.Yours = yours
	}
	if rs := cfg.ReviewHerdr().Socket; rs != "" && filepath.Clean(rs) != filepath.Clean(h.Socket) &&
		filepath.Clean(rs) != filepath.Clean(yours.Socket) {
		d.Guard.Review = &herdr.Client{Socket: rs, Logger: herdrLog}
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
	d.Copies = factory.NewCopies(cfg, copySlots, d.Agents, git, run, copyDBs, h.Snapshot, d.Runtime.Owner, d.Runtime.AtWork)
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
	// Its rounds open their panes in magnum's review session: a round waits
	// while that does not answer, and the factory opens a named one.
	deps.ReviewHerdrUp, deps.ReviewReveal = factoryReviewSession(reviewCfg, run,
		app.Printf{Logger: d.Log, Level: slog.LevelDebug, Src: "herdr"})
	deps.Review = factory.NewLocalReviewer(evalrun.Deps{Layout: l, Config: reviewCfg, Logger: d.Log, Stderr: io.Discard, Runner: run,
		Warn: func(format string, args ...any) { d.Log.Warn("local review: " + fmt.Sprintf(format, args...)) }})
	// The staging status of the published tasks' PRs, when a repository
	// has staging envs: read only, no deploy.
	if d.Staging = factoryStaging(cfg, l, d.Log); d.Staging != nil {
		deps.Staging = d.Staging
	}
	d.Engine = factory.New(deps)
	built = true
	return d, nil
}

// factoryStaging is the factory daemon's staging client
// (factory.NewStaging over internal/staging), when a [[factory.repo]] has
// staging entries, nil otherwise: its git in the mirrors under [staging]
// mirror_dir, through the deny list in the C locale (staging reads git's
// English refusals), logged at debug (it runs every minute); GitHub and the
// fetch's git env as each repository's identity (factory.StagingIdentity,
// stagingIdentities). No resolver, push or commit identity: it deploys
// nothing.
func factoryStaging(cfg *config.Config, l paths.Layout, log *slog.Logger) factory.Staging {
	if !slices.ContainsFunc(cfg.Factory.Repos, func(r config.FactoryRepo) bool { return len(r.Staging) > 0 }) {
		return nil
	}
	run := cLocale{factory.Guard(&execx.Real{Log: app.Printf{Logger: log, Level: slog.LevelDebug, Src: "staging"}}, cfg)}
	ids := newStagingIdentities(cfg, l, run, log)
	return factory.NewStaging(cfg, staging.Deps{
		Git: staging.NewGit(run), GitHub: staging.NewGitHub(ids.client),
		Mirror: staging.MirrorDir(cfg.Staging.MirrorDir), MirrorRoot: staging.MirrorRoot(cfg.Staging.MirrorDir),
		Log: log,
	}, ids.gitEnv)
}

// stagingIdentityTimeout bounds the minting of an App's token for a
// staging fetch.
const stagingIdentityTimeout = time.Minute

// stagingIdentities are the identities the staging status reads as, one
// source and GitHub client per identity (app.NewIdentity), made at their
// first use: an App reads as itself, a gh identity as you
// (factory.OperatorReadEnv: your gh config dir, no inherited GitHub token).
// A repository whose identity cannot be found, or whose env cannot be made,
// is logged once while its cause stays the same (numbers aside), again
// after an env that worked.
type stagingIdentities struct {
	cfg *config.Config
	l   paths.Layout
	run execx.Runner
	log *slog.Logger

	mu     sync.Mutex
	src    map[string]identity.Source
	gh     map[string]*github.Client
	env    map[string]map[string]string // a gh identity's env, as made once
	warned map[string]string            // by repository: the cause last logged
}

func newStagingIdentities(cfg *config.Config, l paths.Layout, run execx.Runner, log *slog.Logger) *stagingIdentities {
	return &stagingIdentities{cfg: cfg, l: l, run: run, log: log, src: map[string]identity.Source{}, gh: map[string]*github.Client{},
		env: map[string]map[string]string{}, warned: map[string]string{}}
}

// of is repo's identity's source and client, and its env when it is a gh
// identity's (nil for an App's, which its source makes); ok is false when
// the config names none for it, or a gh identity's env cannot be made.
func (s *stagingIdentities) of(repo string) (identity.Source, *github.Client, map[string]string, bool) {
	id, err := factory.StagingIdentity(s.cfg, repo)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.warnLocked(repo, "the staging status of a repository has no identity", err)
		return nil, nil, nil, false
	}
	if _, ok := s.src[id.Name]; !ok {
		src, c := app.NewIdentity(s.cfg, s.l, s.run, id)
		if id.Kind != "app" {
			env, err := factory.OperatorReadEnv(context.Background(), id, s.run)
			if err != nil {
				s.warnLocked(repo, "the staging status of a repository has no git env", err)
				return nil, nil, nil, false
			}
			c.Env, s.env[id.Name] = env, env
		}
		s.src[id.Name], s.gh[id.Name] = src, c
	}
	return s.src[id.Name], s.gh[id.Name], s.env[id.Name], true
}

// client is staging.Deps' GitHub client of repo: its identity's, nil for
// none.
func (s *stagingIdentities) client(repo string) *github.Client {
	_, c, _, _ := s.of(repo)
	return c
}

// gitEnv is factory.NewStaging's git env: the env of repo's fetch on ctx
// (its Refresh's, which the daemon's stop cancels), its identity's (an
// App's gh config dir with a fresh token, a gh identity's
// OperatorReadEnv), which gh, git's only credential helper, reads its login
// from; nil when there is none, and the fetch then reads nothing.
func (s *stagingIdentities) gitEnv(ctx context.Context, repo string) map[string]string {
	src, _, env, ok := s.of(repo)
	if !ok {
		return nil
	}
	if env == nil {
		ctx, cancel := context.WithTimeout(ctx, stagingIdentityTimeout)
		defer cancel()
		var err error
		if env, err = src.Env(ctx); err != nil {
			s.mu.Lock()
			s.warnLocked(repo, "the staging status of a repository has no git env", err)
			s.mu.Unlock()
			return nil
		}
	}
	s.mu.Lock()
	delete(s.warned, repo) // the next failure says its cause again
	s.mu.Unlock()
	return env
}

// warnLocked logs what about repo, err its cause, once while the cause
// stays the same, its numbers aside; s.mu is held.
func (s *stagingIdentities) warnLocked(repo, what string, err error) {
	msg := execx.Redact(err.Error())
	key := what + ": " + stagingNumbers.ReplaceAllString(msg, "#")
	if s.warned[repo] == key {
		return
	}
	s.warned[repo] = key
	s.log.Warn(what, "repo", repo, "err", msg)
}

// stagingNumbers are the numbers of a cause's words, which change from one
// try to the next (a timeout's seconds, a temporary file's name).
var stagingNumbers = regexp.MustCompile(`[0-9]+(?:[.,][0-9]+)*`)

// factoryReviewSession is how the local review sees magnum's review session
// (Config.ReviewHerdr: [herdr] review_session, yours by default), where its
// rounds open their panes: up pings it, and open opens a client of it in a
// new terminal window (internal/reveal), nil while that session is yours,
// which the factory never opens.
func factoryReviewSession(cfg *config.Config, run execx.Runner, log execx.Logger) (up, open func(ctx context.Context) error) {
	s := cfg.ReviewHerdr()
	h := &herdr.Client{Socket: s.Socket, Logger: log}
	up = func(ctx context.Context) error {
		_, err := h.Ping(ctx)
		return err
	}
	if filepath.Clean(s.Socket) != filepath.Clean(cfg.YourHerdr().Socket) {
		open = func(ctx context.Context) error {
			_, err := reveal.Reveal(ctx, run, cfg.Terminal, "", reveal.Options{Session: s.Name, NewWindow: true})
			return err
		}
	}
	return up, open
}

// factoryReveal opens a terminal on the factory's herdr session
// (`herdr --session <herdr_session>`) through internal/reveal, in the
// configured terminal app, in a new window.
func factoryReveal(cfg *config.Config, run execx.Runner) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		_, err := reveal.Reveal(ctx, run, cfg.Terminal, "", reveal.Options{Session: cfg.FactoryHerdr().Name, NewWindow: true})
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
