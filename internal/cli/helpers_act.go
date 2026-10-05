package cli

// Shared helpers of the "act" command group (review, open, watch, pin/unpin/
// release/mute/unmute, attention, pick, kick, pause/resume, ui). Every
// identifier here starts with act so the other command groups' helpers never
// collide with it.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/cleanup"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/reveal"
	"github.com/zhuravel/magnum/internal/store"
)

// actReqOpen is the request `magnum open` sends for a parked PR: claim a slot,
// resume its sessions and pin it (engine.OpenPayload). An older daemon
// completes it as an unknown kind, and open then prints the manual resume.
const actReqOpen = engine.ReqOpen

// actDaemonFix is the exact fix when no daemon runs.
const actDaemonFix = "start it with `magnum daemon` (foreground) or `magnum install` (launchd)"

// actHerdr is the part of *herdr.Client the act commands use.
type actHerdr interface {
	Snapshot(ctx context.Context) (herdr.Snapshot, error)
	AgentFocus(ctx context.Context, target string) error
	PaneRead(ctx context.Context, paneID string, o herdr.ReadOptions) (herdr.ReadResult, error)
	PluginPaneOpen(ctx context.Context, o herdr.PluginPaneOptions) (herdr.PluginPane, error)
}

// actGitHub is the read the review command needs for a PR the registry does
// not know yet (*github.Client).
type actGitHub interface {
	Details(ctx context.Context, owner, repo string, numbers []int) (map[int]github.PRDetails, []int, error)
}

// actCleaner plans and applies a release in-process (*cleanup.Planner).
type actCleaner interface {
	Plan(ctx context.Context, opts cleanup.Options) (cleanup.Plan, error)
	Apply(ctx context.Context, plan cleanup.Plan, confirmed bool) (cleanup.Report, error)
}

// actDeps is everything the act commands touch. actNewDeps builds it from
// app.New; tests replace actNewDeps with fakes.
type actDeps struct {
	Cfg    *config.Config
	Layout paths.Layout
	Store  *store.Store // nil in light mode

	Herdr actHerdr
	// GitHub is the read client of an identity; GHEnv its env overlay for
	// `gh api` calls made directly (repository lookup). PrepareIdentity
	// must run first: a GitHub App identity mints or refreshes its
	// installation token and writes its GH_CONFIG_DIR there (identity.Source.Env);
	// nil means nothing to prepare.
	GitHub          func(identity string) actGitHub
	GHEnv           func(identity string) map[string]string
	PrepareIdentity func(ctx context.Context, identity string) error
	// Run runs ordinary subprocesses (gh api, open <url>, reveal); TTY runs
	// interactive ones in the terminal's foreground process group (stty).
	Run     execx.Runner
	TTY     execx.Runner
	Reveal  func(ctx context.Context, opts reveal.Options) (reveal.Outcome, error)
	Cleanup actCleaner

	Kick func() (int, error)                              // engine.KickDaemon
	Lock func() (unlock func(), who opsHolder, err error) // acquireOps(layout)
	// Running is the daemon's pid without waking it (engine.DaemonPID; 0 =
	// none); nil leaves it to the kick. Version is this binary's build,
	// compared with the daemon's (engine.SkewNote).
	Running func() (int, error)
	Version string

	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error

	Stdin     io.Reader
	StdinTTY  bool
	StdoutTTY bool
	Getenv    func(string) string

	// Poll is how often waits re-read the store (2 s); Quick bounds the wait
	// for a request the daemon completes within its tick.
	Poll  time.Duration
	Quick time.Duration

	in    *promptIn
	close func() error
}

// actMode selects how much actNewDeps builds.
type actMode int

const (
	actFull    actMode = iota // app.New: store, clients, layers
	actVerbose                // actFull with info logs on stderr (in-process slot work)
	actLight                  // config and herdr only (ui open)
)

// actNewDeps builds the act commands' dependencies. Tests replace it.
var actNewDeps = func(c *Context, mode actMode) (*actDeps, error) {
	if err := c.LoadConfig(); err != nil {
		return nil, fmt.Errorf("%w\nfix config.toml (`magnum config` validates it)", err)
	}
	cfg, layout := c.Config, c.Layout // config.Load sets cfg.Layout to c.Layout
	d := &actDeps{
		Cfg: cfg, Layout: layout,
		TTY:     actTTYRunner{Stdin: os.Stdin, Stderr: c.Stderr},
		Kick:    func() (int, error) { return engine.KickDaemon(layout) },
		Lock:    func() (func(), opsHolder, error) { return acquireOps(layout) },
		Running: func() (int, error) { return engine.DaemonPID(layout) },
		Version: c.Version,
		Now:     time.Now, Sleep: actSleep,
		Stdin: os.Stdin, StdinTTY: actTerminal(os.Stdin), StdoutTTY: actIsTTY(c.Stdout),
		Getenv: os.Getenv,
		Poll:   2 * time.Second, Quick: reqQuickWait,
		close: func() error { return nil },
	}
	herdrBin := os.Getenv("HERDR_BIN_PATH")
	if mode == actLight {
		d.Herdr = &herdr.Client{Socket: cfg.Herdr.Socket}
		d.Run = &execx.Real{}
		d.Reveal = func(ctx context.Context, opts reveal.Options) (reveal.Outcome, error) {
			return reveal.Reveal(ctx, d.Run, cfg.Terminal, herdrBin, opts)
		}
		return d, nil
	}
	level := slog.LevelWarn
	if mode == actVerbose {
		level = slog.LevelInfo
	}
	a, err := app.New(cfg, layout, app.Options{Logger: app.NewLogger(nil, c.Stderr, level), BeforeMigrate: migrateGuard(layout)})
	if err != nil {
		return nil, err
	}
	d.Store, d.Herdr, d.Run, d.Cleanup, d.close = a.Store, a.Herdr, a.Runner, a.Cleanup, a.Close
	d.GitHub = func(id string) actGitHub {
		if gc := a.GitHub(id); gc != nil {
			return gc
		}
		return nil
	}
	d.GHEnv = func(id string) map[string]string {
		if gc := a.GitHub(id); gc != nil {
			return gc.Env
		}
		return nil
	}
	d.PrepareIdentity = func(ctx context.Context, id string) error {
		src := a.Identities[id]
		if src == nil {
			return fmt.Errorf("unknown identity %q", id)
		}
		if _, err := src.Env(ctx); err != nil {
			return fmt.Errorf("prepare identity %s: %w", id, err)
		}
		return nil
	}
	d.Reveal = func(ctx context.Context, opts reveal.Options) (reveal.Outcome, error) {
		return reveal.Reveal(ctx, a.Runner, cfg.Terminal, herdrBin, opts)
	}
	return d, nil
}

// actKickDaemon sends SIGUSR1 to the daemon (tests replace it).
var actKickDaemon = engine.KickDaemon

func (d *actDeps) Close() error {
	if d == nil || d.close == nil {
		return nil
	}
	return d.close()
}

func (d *actDeps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *actDeps) sleep(ctx context.Context, dur time.Duration) error {
	if d.Sleep != nil {
		return d.Sleep(ctx, dur)
	}
	return actSleep(ctx, dur)
}

func (d *actDeps) getenv(k string) string {
	if d.Getenv != nil {
		return d.Getenv(k)
	}
	return ""
}

func (d *actDeps) refs() app.RefParser { return app.RefParser{DefaultRepo: d.Cfg.Daemon.DefaultRepo} }

// prompt is the one reader over stdin (prompts and key presses).
func (d *actDeps) prompt() *promptIn {
	if d.in == nil {
		d.in = newPromptIn(d.Stdin)
	}
	return d.in
}

// readLine reads one trimmed line from stdin; io.EOF only when nothing was
// typed, ctx.Err() on ctrl+c.
func (d *actDeps) readLine(ctx context.Context) (string, error) { return d.prompt().line(ctx) }

// confirm asks a yes/no question on the terminal; default no.
func (d *actDeps) confirm(ctx context.Context, w io.Writer, question string) bool {
	return d.prompt().confirm(ctx, w, question)
}

func actSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func actIsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && actTerminal(f)
}

// actTerminal reports whether f is a terminal (app.Interactive: the termios
// ioctl, so /dev/null, which background jobs and launchd hand out as stdin,
// is not one).
func actTerminal(f *os.File) bool { return app.Interactive(f) }

// --- flags and output ---

func actUsage(c *Context, cmd, msg, usage string) int {
	fmt.Fprintf(c.Stderr, "magnum %s: %s\nusage: magnum %s\n", cmd, msg, usage)
	return 2
}

func actTable(w io.Writer, header []string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if len(header) > 0 {
		fmt.Fprintln(tw, strings.Join(header, "\t"))
	}
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	_ = tw.Flush()
}

// actClean makes untrusted text (PR titles, logins) safe for one terminal
// line: control characters become spaces.
func actClean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// actAgo renders how long ago t was: 45s, 12m, 3h, 5d.
func actAgo(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func actClock(t time.Time) string { return t.Local().Format("15:04:05") }

// actShellQuote quotes s for sh.
func actShellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r == '/' || r == '.' || r == '-' || r == '_' || r == '=' || r == ':' || r == ',' ||
			r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// actOpenURL opens url in the default browser.
func actOpenURL(ctx context.Context, d *actDeps, url string) error {
	if _, err := d.Run.Run(ctx, execx.Cmd{Name: "open", Args: []string{url}, Mutates: true, Timeout: 30 * time.Second}); err != nil {
		return fmt.Errorf("open %s in the browser: %w", url, err)
	}
	return nil
}

// --- PR references ---

// actLabel is the short reference users type back: name#N for the default
// owner, owner/name#N otherwise.
func (d *actDeps) actLabel(fullName string, number int) string {
	return actRefLabel(d.Cfg.Daemon.DefaultRepo, fullName, number)
}

// actRefLabel is actLabel for a default repository (daemon.default_repo).
func actRefLabel(defaultRepo, fullName string, number int) string {
	owner, name, ok := strings.Cut(fullName, "/")
	defOwner, _, _ := strings.Cut(defaultRepo, "/")
	if ok && defOwner != "" && strings.EqualFold(owner, defOwner) {
		return fmt.Sprintf("%s#%d", name, number)
	}
	return fmt.Sprintf("%s#%d", fullName, number)
}

// resolveRefRepo resolves a typed PR reference to its repository's full
// name and the PR number the way `open` does (app.LookupPR): a repo#N whose
// repository the registry knows under another watched owner keeps that
// owner; anything else keeps the default-repository coordinates. st may be
// nil (no registry: parse only).
func resolveRefRepo(ctx context.Context, st *store.Store, refs app.RefParser, ref string) (string, int, error) {
	owner, name, n, err := refs.ResolvePR(ctx, ref)
	if err != nil {
		return "", 0, err
	}
	if st != nil {
		if repo, _, _ := app.LookupPR(ctx, st, refs, ref); repo.Owner != "" {
			return repo.FullName(), n, nil
		}
	}
	return owner + "/" + name, n, nil
}

// actTarget is a resolved PR (and, for slot targets, the slot).
type actTarget struct {
	Repo store.Repo
	PR   store.PR
	Slot *store.Slot // set when the target is a slot (or the PR's slot was looked up)
	// BySlot is set when the user named the slot itself (`pin review3`):
	// requests then name the slot, so the daemon acts on whatever the slot
	// holds when it handles them, not on a PR that may have left it.
	BySlot bool
}

func (t actTarget) hasPR() bool { return t.PR.ID != 0 }

func (t actTarget) full() string { return t.Repo.FullName() }

// prTarget is the request payload naming the PR exactly.
func (t actTarget) prTarget() engine.PRTarget {
	return engine.PRTarget{Repo: t.Repo.FullName(), Number: t.PR.Number}
}

// resolve finds the PR a command names: a reference, else the herdr
// workspace it runs in (plugin context), else the directory.
func (d *actDeps) resolve(ctx context.Context, ref, workspace, cwd string) (actTarget, error) {
	if ref != "" {
		return d.resolveRef(ctx, ref)
	}
	if workspace != "" {
		t, ok, err := d.resolveWorkspace(ctx, workspace)
		if err != nil || ok {
			return t, err
		}
	}
	if cwd != "" {
		t, ok, err := d.resolveCwd(ctx, cwd)
		if err != nil || ok {
			return t, err
		}
	}
	switch {
	case workspace != "" && cwd != "":
		return actTarget{}, fmt.Errorf("no magnum PR in herdr workspace %s or %s: pass a PR (URL, owner/repo#N, repo#N or N)", workspace, cwd)
	case workspace != "":
		return actTarget{}, fmt.Errorf("no magnum PR in herdr workspace %s: pass a PR (URL, owner/repo#N, repo#N or N)", workspace)
	case cwd != "":
		return actTarget{}, fmt.Errorf("no magnum slot contains %s: pass a PR (URL, owner/repo#N, repo#N or N)", cwd)
	}
	return actTarget{}, errors.New("which PR? pass a URL, owner/repo#N, repo#N or N")
}

func (d *actDeps) resolveRef(ctx context.Context, ref string) (actTarget, error) {
	owner, name, number, err := d.refs().ResolvePR(ctx, ref)
	if err != nil {
		return actTarget{}, err
	}
	full := owner + "/" + name
	repo, pr, err := app.LookupPR(ctx, d.Store, d.refs(), ref)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return actTarget{}, err
		}
		// A repo#N shorthand may resolve to a registered repository under
		// another owner than the default one: that is the repository to name.
		if repo.ID != 0 || repo.Owner != "" {
			full = repo.FullName()
		}
		if d.Cfg.WatchFor(full) == nil {
			return actTarget{Repo: repo}, fmt.Errorf("%s is not watched: add it to a [[watch]] in ~/.config/magnum/config.toml (`magnum init` writes one): %w", full, store.ErrNotFound)
		}
		return actTarget{Repo: repo}, fmt.Errorf("%s#%d is not in the registry yet (the daemon records open PRs on its next poll): %w\nfix: `magnum review %s` adds it now", full, number, store.ErrNotFound, ref)
	}
	return actTarget{Repo: repo, PR: pr}, nil
}

// resolveWorkspace maps a herdr workspace to the PR whose session lives in it.
func (d *actDeps) resolveWorkspace(ctx context.Context, workspace string) (actTarget, bool, error) {
	var prID int64
	err := d.Store.DB().QueryRowContext(ctx,
		`SELECT pr_id FROM sessions WHERE herdr_workspace_id = ? ORDER BY CASE WHEN state IN ('starting','live') THEN 0 ELSE 1 END, id DESC LIMIT 1`,
		workspace).Scan(&prID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actTarget{}, false, nil
		}
		return actTarget{}, false, fmt.Errorf("sessions of workspace %s: %w", workspace, err)
	}
	t, err := d.targetByPRID(ctx, prID)
	return t, err == nil, err
}

// resolveCwd maps a directory to the slot containing it (and its PR).
func (d *actDeps) resolveCwd(ctx context.Context, cwd string) (actTarget, bool, error) {
	slot, ok, err := d.slotContaining(ctx, cwd)
	if err != nil || !ok {
		return actTarget{}, false, err
	}
	if slot.PRID == nil {
		return actTarget{Slot: &slot}, true, nil
	}
	t, err := d.targetByPRID(ctx, *slot.PRID)
	t.Slot = &slot
	return t, err == nil, err
}

func (d *actDeps) slotContaining(ctx context.Context, dir string) (store.Slot, bool, error) {
	slots, err := d.Store.ListSlots(ctx, store.SlotFilter{})
	if err != nil {
		return store.Slot{}, false, err
	}
	dir = actCanon(dir)
	best, found := store.Slot{}, false
	for _, s := range slots {
		if s.State == store.SlotRemoved || s.Path == "" {
			continue
		}
		p := actCanon(s.Path)
		if (dir == p || strings.HasPrefix(dir, p+string(filepath.Separator))) && (!found || len(p) > len(actCanon(best.Path))) {
			best, found = s, true
		}
	}
	return best, found, nil
}

func actCanon(p string) string {
	p = filepath.Clean(paths.Expand(p))
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func (d *actDeps) targetByPRID(ctx context.Context, id int64) (actTarget, error) {
	pr, err := d.Store.PRByID(ctx, id)
	if err != nil {
		return actTarget{}, err
	}
	repo, err := d.Store.RepoByID(ctx, pr.RepoID)
	if err != nil {
		return actTarget{}, err
	}
	return actTarget{Repo: repo, PR: pr}, nil
}

// actKnownRole checks a --role value before the PR is known: "" (the
// judge) or a role name or alias of any watch (config.Role.Matches).
func actKnownRole(cfg *config.Config, s string) error {
	if strings.TrimSpace(s) == "" || slices.ContainsFunc(cfg.RolesFor(nil), func(r config.Role) bool { return r.Matches(s) }) {
		return nil
	}
	return fmt.Errorf("unknown role %q: use %s", s, actRoleChoices(cfg.RolesFor(nil)))
}

// actRoleFor resolves a --role value for a PR of repository full: a role
// name or alias of the repository's watch, "" = its judge
// (config.Config.RoleByNameOrAlias).
func actRoleFor(cfg *config.Config, full, s string) (config.Role, error) {
	w := cfg.WatchFor(full)
	if r, ok := cfg.RoleByNameOrAlias(w, s); ok {
		return r, nil
	}
	if strings.TrimSpace(s) == "" {
		return config.Role{}, fmt.Errorf("the watch of %s has no judge role (`magnum config` validates config.toml)", full)
	}
	return config.Role{}, fmt.Errorf("%s does not run role %q: use %s", full, s, actRoleChoices(cfg.RolesFor(w)))
}

// actRoleChoices lists roles for a message: the names, then the aliases.
func actRoleChoices(roles []config.Role) string {
	var names, aliases []string
	for _, r := range roles {
		names = append(names, r.Name)
		aliases = append(aliases, r.Aliases...)
	}
	out := actJoinOr(names)
	if len(aliases) > 0 {
		out += " (aliases " + strings.Join(aliases, ", ") + ")"
	}
	return out
}

// actJoinOr joins words as "a, b or c".
func actJoinOr(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " or " + words[len(words)-1]
}

// actJudgeFor is the judge role name of the watch covering repository full
// (store.RoleJudge when no judge is configured).
func actJudgeFor(cfg *config.Config, full string) string {
	if cfg != nil {
		if j := cfg.JudgeFor(cfg.WatchFor(full)); j.Name != "" {
			return j.Name
		}
	}
	return store.RoleJudge
}

// actIsJudge reports whether a sessions.role or runs.role value names a
// configured judge (any watch's).
func actIsJudge(cfg *config.Config, role string) bool {
	if cfg == nil {
		return role == store.RoleJudge
	}
	return slices.ContainsFunc(cfg.RolesFor(nil), func(r config.Role) bool { return r.Judge && r.Matches(role) })
}

// actResumeArgv is the command that resumes a session's conversation by
// hand: its agent kind (the session's, else its role's) with the kind's
// resume args (config.Kind.Resume); nil when there is no session id, the
// role is a shell role or the kind cannot resume. A nil cfg means the
// built-in roles and kinds.
func actResumeArgv(cfg *config.Config, s store.Session) []string {
	if cfg == nil {
		cfg = config.Defaults()
	}
	id := store.Deref(s.SessionID)
	kind := store.Deref(s.AgentKind)
	if r, ok := cfg.RoleByNameOrAlias(nil, s.Role); kind == "" && ok && r.IsAgent() {
		kind = r.Kind
	}
	k, ok := cfg.KindSpec(kind)
	if id == "" || !ok || len(k.Resume) == 0 {
		return nil
	}
	return append([]string{kind}, k.Argv(config.LaunchArgs{Session: id, Wrapper: true})...)
}

// actRoleName is a session role as users see it (agents.Role.Label: the
// configured role name).
func actRoleName(role string) string { return agents.Role(role).Label() }

// --- requests to the daemon ---

// reqs is the request client (request_client.go) over d's registry and
// daemon.
func (d *actDeps) reqs() reqClient {
	return reqClient{st: d.Store, kick: d.Kick, running: d.Running, now: d.now, sleep: d.sleep, version: d.Version}
}

// quick is a send that waits up to Quick for an answer the daemon gives
// within its tick.
func (d *actDeps) quick() reqSend { return reqSend{Wait: d.Quick, Poll: d.quickPoll()} }

// quickPoll is the poll interval while waiting for a tick-time request.
func (d *actDeps) quickPoll() time.Duration {
	if d.Poll > 0 && d.Poll < 250*time.Millisecond {
		return d.Poll
	}
	return 250 * time.Millisecond
}

// --- herdr focus + terminal reveal ---

// actFocusResult is what open/attention did.
type actFocusResult struct {
	PR          string          `json:"pr"`
	URL         string          `json:"url,omitempty"`
	Role        string          `json:"role"`
	Agent       string          `json:"agent,omitempty"`
	PaneID      string          `json:"pane_id,omitempty"`
	WorkspaceID string          `json:"workspace_id,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	Note        string          `json:"note,omitempty"`
	Reveal      *reveal.Outcome `json:"reveal,omitempty"`
}

// herdrErr turns a herdr failure into an error with the fix.
func (d *actDeps) herdrErr(err error) error {
	if errors.Is(err, herdr.ErrUnavailable) {
		return fmt.Errorf("herdr is not running (socket %s): start herdr, then retry: %w", d.Cfg.Herdr.Socket, err)
	}
	return err
}

// focus focuses the session's agent (or pane) in herdr, then reveals the
// herdr client in the configured terminal unless noReveal.
func (d *actDeps) focus(ctx context.Context, res *actFocusResult, sess store.Session, noReveal, newWindow bool) error {
	target := store.Deref(sess.AgentName)
	if target == "" {
		target = store.Deref(sess.HerdrPaneID)
	}
	if target == "" {
		return fmt.Errorf("the %s session of %s has no herdr pane recorded", actRoleName(sess.Role), res.PR)
	}
	res.Agent, res.PaneID, res.WorkspaceID = store.Deref(sess.AgentName), store.Deref(sess.HerdrPaneID), store.Deref(sess.HerdrWorkspaceID)
	if err := d.Herdr.AgentFocus(ctx, target); err != nil {
		return fmt.Errorf("focus %s in herdr: %w", target, d.herdrErr(err))
	}
	if noReveal {
		return nil
	}
	out, err := d.Reveal(ctx, reveal.Options{NewWindow: newWindow})
	if err != nil {
		return fmt.Errorf("focused %s in herdr, but revealing it in %s failed: %w (fix [terminal] in config.toml, or pass --no-reveal)", target, d.Cfg.Terminal.App, err)
	}
	res.Reveal = &out
	return nil
}

// actFocusLine is the one-line human summary of a focus.
func actFocusLine(r actFocusResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", r.PR, actRoleName(r.Role))
	if r.Reason != "" {
		fmt.Fprintf(&b, " (%s)", r.Reason)
	}
	who := r.Agent
	if who == "" {
		who = "pane " + r.PaneID
	}
	fmt.Fprintf(&b, ": focused %s", who)
	if r.Reveal != nil {
		fmt.Fprintf(&b, "; %s", r.Reveal.String())
	}
	if r.Note != "" {
		fmt.Fprintf(&b, " (%s)", r.Note)
	}
	return b.String()
}

// --- interactive subprocesses ---

// actTTYRunner runs interactive subprocesses (stty) in the terminal's
// foreground process group. execx.Real puts every child in its own process
// group, where touching the terminal stops it with SIGTTIN/SIGTTOU. Stdin
// is Cmd.Stdin when set, else the terminal; stdout is captured; stderr goes
// to the terminal. It never runs under --dry-run (the act commands that use
// it are interactive by nature).
type actTTYRunner struct {
	Stdin  *os.File
	Stderr io.Writer
}

func (r actTTYRunner) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	if len(c.Env) > 0 || len(c.Unset) > 0 {
		cmd.Env = execx.MergeEnv(os.Environ(), c.Env, c.Unset)
	}
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	} else if r.Stdin != nil {
		cmd.Stdin = r.Stdin
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = r.Stderr
	start := time.Now()
	err := cmd.Run()
	res := execx.Result{Stdout: out.Bytes(), Duration: time.Since(start)}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.Code = ee.ExitCode()
			return res, &execx.ExitError{Cmd: c, Code: res.Code}
		}
		return res, fmt.Errorf("%s: %w", c.Name, err)
	}
	return res, nil
}

// cbreak puts the terminal in single-key mode (no line buffering, no echo;
// ctrl+c still interrupts) and returns the restore function. A no-op when
// stdin is not a terminal.
func (d *actDeps) cbreak() func() {
	if !d.StdinTTY || d.TTY == nil {
		return func() {}
	}
	bg := context.Background()
	saved, err := d.TTY.Run(bg, execx.Cmd{Name: "stty", Args: []string{"-g"}, Timeout: 5 * time.Second})
	if err != nil {
		return func() {}
	}
	if _, err := d.TTY.Run(bg, execx.Cmd{Name: "stty", Args: []string{"-icanon", "-echo", "min", "1", "time", "0"}, Mutates: true, Timeout: 5 * time.Second}); err != nil {
		return func() {}
	}
	state := strings.TrimSpace(string(saved.Stdout))
	return func() {
		_, _ = d.TTY.Run(bg, execx.Cmd{Name: "stty", Args: []string{state}, Mutates: true, Timeout: 5 * time.Second})
	}
}

// termRows is the terminal height (stty size), or fallback.
func (d *actDeps) termRows(fallback int) int {
	if !d.StdinTTY || d.TTY == nil {
		return fallback
	}
	res, err := d.TTY.Run(context.Background(), execx.Cmd{Name: "stty", Args: []string{"size"}, Timeout: 5 * time.Second})
	if err != nil {
		return fallback
	}
	var rows, cols int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(res.Stdout)), "%d %d", &rows, &cols); err != nil || rows <= 0 {
		return fallback
	}
	return rows
}

// pressAnyKey waits for one key (or ctrl+c) when running in the herdr
// plugin popup, so the result stays readable before the pane closes.
func (d *actDeps) pressAnyKey(ctx context.Context, w io.Writer) {
	if d.getenv("HERDR_PLUGIN_ID") == "" || !d.StdinTTY {
		return
	}
	fmt.Fprint(w, "\npress any key to close")
	restore := d.cbreak()
	_, _ = d.prompt().key(ctx)
	restore()
	fmt.Fprintln(w)
}

// actPluginContext reads HERDR_PLUGIN_CONTEXT_JSON (plugin actions and panes).
func (d *actDeps) pluginContext() map[string]any {
	raw := d.getenv("HERDR_PLUGIN_CONTEXT_JSON")
	if raw == "" {
		return nil
	}
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil {
		return nil
	}
	return m
}

func actString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}
