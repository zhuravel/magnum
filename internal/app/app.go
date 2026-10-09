// Package app wires magnum's components from config.toml: the store, the
// subprocess runner (wrapped in execx.DryRun under --dry-run), the herdr,
// GitHub, MySQL and git clients, the identities, and the slots, inventory,
// agents, pipeline, cleanup and notify layers built on top of them. The daemon
// (package engine) and the CLI share one App per process.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/cleanup"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/identity"
	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
)

// Options tune New.
type Options struct {
	// DryRun wraps the runner in execx.DryRun, puts slots in dry-run mode,
	// disables toasts and opens a private copy of the registry, so nothing
	// outside the process changes.
	DryRun bool
	// Daemon marks the process as the daemon, which owns daemon.log rotation.
	// Any other process (CLI commands) appends to daemon.log without ever
	// rotating it.
	Daemon bool
	// Logger replaces the default logger (JSON to state/logs/daemon.log,
	// rotated by size when Daemon is set, plus text to Stderr). Under DryRun
	// the default logs to Stderr only.
	Logger *slog.Logger
	// Stderr receives human-readable logs; nil means os.Stderr when it is a
	// terminal (or always under DryRun).
	Stderr io.Writer
	// LogLevel defaults to info.
	LogLevel slog.Leveler
	// Runner replaces the real subprocess runner (tests).
	Runner execx.Runner
	// MySQLDSN defaults to $MAGNUM_MYSQL_DSN, else mysqlx.DefaultDSN.
	MySQLDSN string
	// HTTPClient and Getenv are passed to App identities (tests). A nil
	// HTTPClient means the [github] transport client (see githubHTTPClient).
	HTTPClient *http.Client
	Getenv     func(string) string
	// HerdrSocket overrides config [herdr] socket (tests).
	HerdrSocket string
	// AgentTag tags the agents this App starts (agents.Deps.Tag,
	// pipeline.Runner.AgentTag) and turns its toasts off: magnum eval uses
	// "eval" with a scratch layout, so its rounds never meet the PR's own
	// agents or notify like a real review.
	AgentTag string
	// Mise is the mise executable that runs the pool's scripts: the slots
	// manager's (slots.Deps.Mise) and the readiness step's reset_db commands'
	// (pipeline.Runner.Mise). "" means "mise" on PATH.
	Mise string
	// BeforeMigrate is store.Options.BeforeMigrate for the real registry
	// (the CLI refuses to migrate it under a running daemon); a dry-run copy
	// migrates freely.
	BeforeMigrate func(from, to int) error
}

// App is the wired set of components one process uses.
type App struct {
	Config *config.Config
	Layout paths.Layout
	Store  *store.Store
	// Runner is the subprocess runner every component shares; under DryRun it
	// is DryRunner.
	Runner    execx.Runner
	DryRunner *execx.DryRun // non-nil under DryRun: the planned mutating commands
	DryRun    bool
	// AgentTag is Options.AgentTag.
	AgentTag string
	// Mise is Options.Mise.
	Mise string

	Herdr *herdr.Client
	// GitHub returns the gh client acting as the named identity (nil for an
	// unknown name). Clients are created once and shared.
	GitHub     func(identity string) *github.Client
	Identities map[string]identity.Source
	// MySQL is the local DBngin client. mysqlx.Open never connects, so it is
	// non-nil even while MySQL is down (calls then fail and consumers degrade);
	// Ping probes it. Nil only when the DSN is invalid.
	MySQL *mysqlx.Client
	Git   *gitx.Client

	Slots     *slots.Manager
	Inventory *inventory.Scanner
	Agents    *agents.Manager
	// Pipeline holds one round runner per identity name (its GitHub client
	// acts as that identity).
	Pipeline map[string]*pipeline.Runner
	Cleanup  *cleanup.Planner
	Notify   *notify.Notifier

	Logger *slog.Logger

	storePath string
	closers   []func() error
	// gh is filled by New and read-only afterwards, so it needs no lock.
	gh map[string]*github.Client
}

// New builds the App: it creates the state tree, opens the store (a private
// copy under DryRun), builds every client from cfg and wires the layers.
func New(cfg *config.Config, layout paths.Layout, opts Options) (*App, error) {
	if cfg == nil {
		return nil, errors.New("app: config is required")
	}
	if !layout.Valid() {
		layout = cfg.Layout
	}
	if err := layout.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("app: state dirs: %w", err)
	}
	a := &App{Config: cfg, Layout: layout, DryRun: opts.DryRun, gh: map[string]*github.Client{}}
	ok := false
	defer func() {
		if !ok {
			_ = a.Close()
		}
	}()

	if err := a.initLogger(opts); err != nil {
		return nil, err
	}

	// Store (a scratch copy under dry-run, so decisions can be recorded
	// without touching the real registry).
	a.storePath = layout.DB()
	if opts.DryRun {
		p, cleanupCopy, err := copyStore(context.Background(), layout.DB())
		if err != nil {
			return nil, fmt.Errorf("app: dry-run store copy: %w", err)
		}
		a.storePath = p
		a.closers = append(a.closers, cleanupCopy)
	}
	sopts := store.Options{}
	if !opts.DryRun {
		sopts.BeforeMigrate = opts.BeforeMigrate
	}
	st, err := store.OpenWith(a.storePath, sopts)
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	a.Store = st
	a.closers = append(a.closers, st.Close)

	// Runner.
	base := opts.Runner
	if base == nil {
		base = &execx.Real{Log: Printf{Logger: a.Logger, Level: slog.LevelInfo, Src: "exec"}}
	}
	a.Runner = base
	if opts.DryRun {
		a.DryRunner = &execx.DryRun{Inner: base, Out: Printf{Logger: a.Logger, Level: slog.LevelInfo, Src: "dry-run"}}
		a.Runner = a.DryRunner
	}

	// Clients.
	socket := opts.HerdrSocket
	if socket == "" {
		socket = cfg.Herdr.Socket
	}
	a.Herdr = &herdr.Client{Socket: socket, Logger: Printf{Logger: a.Logger, Level: slog.LevelDebug, Src: "herdr"}}
	a.Git = gitx.New(a.Runner)
	a.Git.HTTPSFetch = true // fetch over HTTPS through gh, independent of the user's SSH agent
	dsn := opts.MySQLDSN
	if dsn == "" {
		dsn = os.Getenv("MAGNUM_MYSQL_DSN")
	}
	if my, err := mysqlx.Open(dsn); err != nil {
		a.Logger.Warn("mysql client unavailable", "err", err)
	} else {
		a.MySQL = my
		a.closers = append(a.closers, my.Close)
	}

	// Identities and their gh clients.
	a.Identities = map[string]identity.Source{}
	httpClient := githubHTTPClient(cfg.GitHub.Transport, a.Runner, opts.HTTPClient)
	for _, id := range cfg.Identities {
		switch id.Kind {
		case "app":
			app := identity.NewApp(id, layout, httpClient, opts.Getenv, nil,
				identity.WithRunner(a.Runner), identity.WithRepos(identity.WatchedRepos(cfg, id.Name)...))
			a.Identities[id.Name] = app
			// A 401 means the installation token was revoked: mint a new
			// one and retry the call once.
			a.gh[id.Name] = &github.Client{Run: a.Runner, Reauth: app.Reauth, Env: map[string]string{
				"GH_CONFIG_DIR": app.ConfigDir(), "GH_TOKEN": "", "GITHUB_TOKEN": ""}}
		default:
			a.Identities[id.Name] = identity.NewGH(id, a.Runner)
			a.gh[id.Name] = &github.Client{Run: a.Runner, Env: map[string]string{}}
		}
	}
	a.GitHub = a.githubFor

	a.AgentTag, a.Mise = opts.AgentTag, opts.Mise
	a.wire()
	ok = true
	return a, nil
}

// githubHTTPClient is the client App identities use for their own REST calls
// (JWT, installation token, installation checks). By default those calls run
// as `gh api` through run, so they get the same redacted exec log lines as
// every other gh call and pass outbound firewalls that only allow gh; with
// [github] transport = "direct" (nil here) identity.NewApp falls back to its
// 30 s net/http client. override (tests) wins.
func githubHTTPClient(transport string, run execx.Runner, override *http.Client) *http.Client {
	if override != nil {
		return override
	}
	if transport == "direct" {
		return nil
	}
	return identity.NewGhClient(run, nil)
}

func (a *App) initLogger(opts Options) error {
	if opts.Logger != nil {
		a.Logger = opts.Logger
		return nil
	}
	stderr := opts.Stderr
	if stderr == nil && (opts.DryRun || Interactive(os.Stderr)) {
		stderr = os.Stderr
	}
	var file io.Writer
	if !opts.DryRun {
		lf, err := openDaemonLog(a.Layout.DaemonLog(), opts.Daemon)
		if err != nil {
			return fmt.Errorf("app: %w", err)
		}
		a.closers = append(a.closers, lf.Close)
		file = lf
	}
	a.Logger = NewLogger(file, stderr, opts.LogLevel)
	return nil
}

// keyEnvs are the identities' private_key_env names: the variables of the
// daemon's environment that hold an App's private key.
func keyEnvs(ids []config.Identity) []string {
	var out []string
	for _, id := range ids {
		if id.PrivateKeyEnv != "" {
			out = append(out, id.PrivateKeyEnv)
		}
	}
	return out
}

// wire builds the layers on top of the clients.
func (a *App) wire() {
	cfg := a.Config
	var my slots.MySQL
	var invMy inventory.MySQL
	var cleanMy cleanup.MySQL
	if a.MySQL != nil {
		my, invMy, cleanMy = a.MySQL, a.MySQL, a.MySQL
	}
	a.Slots = slots.New(slots.Deps{
		Store: a.Store, Run: a.Runner, Git: a.Git, MySQL: my,
		Snapshot: a.Herdr.Snapshot, ProcessInfo: a.Herdr.PaneProcessInfo,
		Layout: a.Layout, Log: Printf{Logger: a.Logger, Level: slog.LevelInfo, Src: "slots"},
		Repos: cfg.Repos, DryRun: a.DryRun, Mise: a.Mise, SecretEnv: keyEnvs(cfg.Identities),
	})
	var invGH inventory.GitHub
	if c := a.pollClient(); c != nil {
		invGH = c
	}
	a.Inventory = &inventory.Scanner{
		Store: a.Store, Git: a.Git, MySQL: invMy, Herdr: a.Herdr, GitHub: invGH,
		Runner: a.Runner, Config: cfg,
	}
	a.Agents = agents.New(agents.Deps{
		Herdr: a.Herdr, Store: a.Store, Runner: a.Runner, Config: cfg, Layout: a.Layout, Tag: a.AgentTag,
		Log: Printf{Logger: a.Logger, Level: slog.LevelInfo, Src: "agents"},
	})
	a.Pipeline = map[string]*pipeline.Runner{}
	for name, src := range a.Identities {
		a.Pipeline[name] = &pipeline.Runner{
			Agents: a.Agents, GitHub: a.gh[name], Git: a.Git, Exec: a.Runner, Keys: a.Herdr,
			Store: a.Store, Identity: src, SelfLogin: selfLogin(cfg, name),
			Config: cfg, Layout: a.Layout, AgentTag: a.AgentTag, Mise: a.Mise,
			Logger: Printf{Logger: a.Logger, Level: slog.LevelInfo, Src: "pipeline"},
		}
	}
	a.Cleanup = &cleanup.Planner{
		Store: a.Store, Slots: a.Slots, Inventory: a.Inventory, MySQL: cleanMy, Git: a.Git,
		Runner: a.Runner, Config: cfg, Park: a.Agents.Park,
		Log: Printf{Logger: a.Logger, Level: slog.LevelInfo, Src: "cleanup"},
	}
	a.Notify = &notify.Notifier{
		Herdr: a.Herdr, Store: a.Store, Runner: a.Runner,
		Enabled: cfg.Herdr.Notify && !a.DryRun && a.AgentTag == "",
		Log:     Printf{Logger: a.Logger, Level: slog.LevelInfo, Src: "notify"},
	}
}

// githubFor implements App.GitHub.
func (a *App) githubFor(name string) *github.Client { return a.gh[name] }

// pollClient is the gh client of the first watch's poll identity (reads).
func (a *App) pollClient() *github.Client {
	for _, w := range a.Config.Watches {
		if c := a.gh[w.PollIdentity]; c != nil {
			return c
		}
	}
	for _, id := range a.Config.Identities {
		if id.Kind == "gh" {
			return a.gh[id.Name]
		}
	}
	return nil
}

// selfLogin is the user's own login for an identity's rounds: the poll
// identity of a watch that posts as name, else the first gh identity.
func selfLogin(cfg *config.Config, name string) string {
	for _, w := range cfg.Watches {
		if w.Identity == name {
			if id := cfg.IdentityByName(w.PollIdentity); id != nil {
				return id.Login
			}
		}
	}
	for _, id := range cfg.Identities {
		if id.Kind == "gh" {
			return id.Login
		}
	}
	return ""
}

// IdentityNames lists the configured identity names in config order.
func (a *App) IdentityNames() []string {
	var out []string
	for _, id := range a.Config.Identities {
		out = append(out, id.Name)
	}
	return slices.Clip(out)
}

// Close releases everything New opened (store, MySQL, log file, the dry-run
// store copy), in reverse order.
func (a *App) Close() error {
	var errs []error
	for _, v := range slices.Backward(a.closers) {
		if err := v(); err != nil {
			errs = append(errs, err)
		}
	}
	a.closers = nil
	return errors.Join(errs...)
}
