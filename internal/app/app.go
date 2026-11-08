// Package app wires magnum's components from config.toml: the store, the
// subprocess runner (wrapped in execx.DryRun under --dry-run), the herdr,
// GitHub, MySQL and git clients, the identities, and the slots, inventory,
// agents, pipeline, cleanup and notify layers built on top of them. The daemon
// (package engine) and the CLI share one App per process.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/cleanup"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/identity"
	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/jira"
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
	// HerdrSocket overrides the socket of the herdr session magnum's panes
	// are in (tests).
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

	// Herdr is the client of the herdr session magnum's panes are in,
	// PaneSession: the agents, the rounds' keys and the engine drive it,
	// and the registry's herdr ids are its server's.
	Herdr       *herdr.Client
	PaneSession herdr.Session
	// Yours is a client of your herdr session ([herdr] socket), nil while
	// the panes are in it (Herdr is then yours).
	Yours *herdr.Client
	// Guard is the herdr the slot guard and the inventory read: the panes'
	// session and yours, its ids prefixed (herdr.GuardView), so your agents
	// and consoles in a checkout hold it wherever magnum's panes are.
	Guard herdr.GuardView
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
	// Notify toasts in your session (Yours, else Herdr): the one you look
	// at. Sidebar labels magnum's workspaces (herdr's sidebar tokens) in
	// the panes' session.
	Notify  *notify.Notifier
	Sidebar *notify.Notifier

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
	if a.PaneSession, err = paneSession(context.Background(), st, cfg, opts.AgentTag); err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	if opts.HerdrSocket != "" {
		a.PaneSession.Socket = opts.HerdrSocket
	}
	herdrLog := Printf{Logger: a.Logger, Level: slog.LevelDebug, Src: "herdr"}
	a.Herdr = &herdr.Client{Socket: a.PaneSession.Socket, Logger: herdrLog}
	a.Guard = herdr.GuardView{Own: a.Herdr}
	if filepath.Clean(cfg.Herdr.Socket) != filepath.Clean(a.PaneSession.Socket) {
		a.Yours = &herdr.Client{Socket: cfg.Herdr.Socket, Logger: herdrLog}
		a.Guard.Yours = a.Yours
	}
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
			app := identity.NewApp(id, layout, httpClient, opts.Getenv, nil, identity.WithRunner(a.Runner),
				identity.WithRepos(identity.WatchedRepos(cfg, id.Name)...), identity.WithFixer(identity.FixRepos(cfg, id.Name)...))
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
		Snapshot: a.Guard.Snapshot, ProcessInfo: a.Guard.ProcessInfo,
		Layout: a.Layout, Log: Printf{Logger: a.Logger, Level: slog.LevelInfo, Src: "slots"},
		Repos: cfg.Repos, DryRun: a.DryRun, Mise: a.Mise, SecretEnv: keyEnvs(cfg.Identities),
	})
	var invGH inventory.GitHub
	if c := a.pollClient(); c != nil {
		invGH = c
	}
	a.Inventory = &inventory.Scanner{
		Store: a.Store, Git: a.Git, MySQL: invMy, Herdr: a.Guard, GitHub: invGH,
		Runner: a.Runner, Config: cfg,
	}
	a.Agents = agents.New(agents.Deps{
		Herdr: a.Herdr, Store: a.Store, Runner: a.Runner, Config: cfg, Layout: a.Layout, Tag: a.AgentTag,
		IdentityEnv: a.identityEnv, FixerEnv: identity.FixerEnv(cfg), Log: Printf{Logger: a.Logger, Level: slog.LevelInfo, Src: "agents"},
		Toast:        a.toastAsync,
		Signal:       syscall.Kill, // StopShell's, for a command it proved its own
		SkipGoneDirs: true,
	})
	// One Jira client for every identity's rounds: a token file problem
	// warns once per daemon start (jira.Client.WarnTokenFile).
	var tickets pipeline.Tickets
	if cfg.Jira.On() {
		tickets = &jira.Client{Runner: a.Runner, URL: cfg.Jira.URL, User: cfg.Jira.User, TokenFile: cfg.Jira.TokenFile}
	}
	a.Pipeline = map[string]*pipeline.Runner{}
	for name, src := range a.Identities {
		a.Pipeline[name] = &pipeline.Runner{
			Tickets: tickets,
			Agents:  a.Agents, GitHub: a.gh[name], Git: a.Git, Exec: a.Runner, Keys: a.Herdr,
			Store: a.Store, Identity: src, SelfLogin: selfLogin(cfg, name),
			Config: cfg, Layout: a.Layout, AgentTag: a.AgentTag, Mise: a.Mise,
			Logger: Printf{Logger: a.Logger, Level: slog.LevelInfo, Src: "pipeline"},
			Ended:  a.Agents.Ended, // a turn's wait ends with the observe pass that ended its run
		}
	}
	a.Cleanup = &cleanup.Planner{
		Store: a.Store, Slots: a.Slots, Inventory: a.Inventory, MySQL: cleanMy, Git: a.Git,
		Runner: a.Runner, Config: cfg, Park: a.Agents.ParkClosingPanes,
		Log: Printf{Logger: a.Logger, Level: slog.LevelInfo, Src: "cleanup"},
	}
	toasts := a.Herdr
	if a.Yours != nil {
		toasts = a.Yours
	}
	notifyLog := Printf{Logger: a.Logger, Level: slog.LevelInfo, Src: "notify"}
	a.Notify = &notify.Notifier{
		Herdr: toasts, Store: a.Store, Runner: a.Runner,
		Enabled: cfg.Herdr.Notify && !a.DryRun && a.AgentTag == "",
		Log:     notifyLog,
	}
	a.Sidebar = &notify.Notifier{Herdr: a.Herdr, Log: notifyLog}
}

// paneSession is the herdr session the App opens magnum's panes in. A
// tagged App (an eval replay, the factory's local review) runs on a
// registry of its own and opens them in the configured review session. The
// untagged one, on the real registry (the daemon, the CLI), opens them where
// the registry's herdr ids came from (store.KVHerdrPanes), your session when
// it records none, as every build before the key: each herdr server numbers
// its own ids, and only the daemon's move, which parks every session first,
// takes the panes to the session the config names. A record that names no
// session is an error: the ids could be any server's.
func paneSession(ctx context.Context, st *store.Store, cfg *config.Config, tag string) (herdr.Session, error) {
	if tag != "" {
		return cfg.ReviewHerdr(), nil
	}
	v, ok, err := st.GetKV(ctx, store.KVHerdrPanes)
	if err != nil || !ok {
		return cfg.YourHerdr(), err
	}
	var s herdr.Session
	if err := json.Unmarshal([]byte(v), &s); err != nil || s.Name == "" || !filepath.IsAbs(s.Socket) {
		return herdr.Session{}, fmt.Errorf("kv %s holds no herdr session (%q): the registry's herdr ids are no known "+
			"server's", store.KVHerdrPanes, v)
	}
	return s, nil
}

// agentToastWindow dedupes the agents' toasts by key (agents.Deps.Toast),
// and agentToastTimeout bounds one.
const (
	agentToastWindow  = 24 * time.Hour
	agentToastTimeout = time.Minute
)

// toastAsync implements agents.Deps.Toast: the toast goes out on a goroutine
// of its own (a round waits for no notification), best effort, through the
// App's notifier (a no-op when it is off: dry run, a tagged App).
func (a *App) toastAsync(ctx context.Context, key, title, body string) {
	n := a.Notify
	if n == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentToastTimeout)
		defer cancel()
		if _, err := n.Toast(ctx, key, title, body, agentToastWindow); err != nil {
			a.Logger.Warn("toast failed", "key", key, "err", err)
		}
	}()
}

// identityEnv implements agents.Deps.IdentityEnv: the GitHub env of the
// configured identity name, false for an identity that is not declared or
// whose env cannot be made. A role's identity is a gh one, whose env needs
// no token and no network, or a dedicated fixer App (config validation
// refuses any other App for the fix role), whose env is its gh config dir:
// a path, the token kept fresh in it by the daemon's refresh.
func (a *App) identityEnv(name string) (map[string]string, bool) {
	src, ok := a.Identities[name]
	if !ok {
		return nil, false
	}
	env, err := src.Env(context.Background())
	if err != nil {
		a.Logger.Warn("identity env unavailable", "identity", name, "err", err)
		return nil, false
	}
	return env, true
}

// githubFor implements App.GitHub.
func (a *App) githubFor(name string) *github.Client { return a.gh[name] }

// pollClient is the client the inventory reads with, a gh identity's
// when one is declared: the first watch's operator identity
// (config.OperatorIdentity: its poll identity when a gh identity polls),
// else the first gh identity, else the first watch's poll identity (an App).
func (a *App) pollClient() *github.Client {
	for _, w := range a.Config.Watches {
		if id := a.Config.OperatorIdentity(w); id != nil && a.gh[id.Name] != nil {
			return a.gh[id.Name]
		}
	}
	for _, id := range a.Config.Identities {
		if id.Kind == "gh" && a.gh[id.Name] != nil {
			return a.gh[id.Name]
		}
	}
	for _, w := range a.Config.Watches {
		if c := a.gh[w.PollIdentity]; c != nil {
			return c
		}
	}
	return nil
}

// selfLogin is the user's own login for an identity's rounds: the
// operator's login (config.OperatorIdentity) of a watch that posts as name,
// else the first gh identity's.
func selfLogin(cfg *config.Config, name string) string {
	for _, w := range cfg.Watches {
		if w.Identity == name {
			if l := cfg.OperatorLogin(w); l != "" {
				return l
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
