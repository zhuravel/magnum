package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

func TestFactoryInstallWritesItsOwnLabel(t *testing.T) {
	storetest.Serial(t) // newDaemonGroupTest swaps daemonSys
	dt := newDaemonGroupTest(t, installRules(herdrPluginListEmpty)...)
	dt.writeBinary(t)
	if code := dt.run("factory", "install"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	l := dt.ctx.Layout
	path := filepath.Join(dt.userHome, "Library", "LaunchAgents", "zhuravel.magnum.factory.plist")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("plist not written: %v", err)
	}
	plist := string(b)
	logs := filepath.Join(l.Logs(), "factory-launchd.log")
	for _, want := range []string{
		"<key>Label</key>\n\t<string>zhuravel.magnum.factory</string>",
		"<string>" + l.Binary() + "</string>\n\t\t<string>factory</string>\n\t\t<string>daemon</string>\n\t</array>",
		"<key>StandardOutPath</key>\n\t<string>" + logs + "</string>",
		"<key>StandardErrorPath</key>\n\t<string>" + logs + "</string>",
		"<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q:\n%s", want, plist)
		}
	}
	for _, not := range []string{filepath.Join(l.Logs(), "launchd.log"), l.FactoryLog(), "/tmp/herdr-test.sock"} {
		if strings.Contains(plist, not) {
			t.Errorf("the factory's plist names %q, the review daemon's or the daemon's own:\n%s", not, plist)
		}
	}
	if _, err := os.Stat(filepath.Join(dt.userHome, "Library", "LaunchAgents", "zhuravel.magnum.plist")); !os.IsNotExist(err) {
		t.Fatalf("factory install wrote the review daemon's plist: %v", err)
	}
	if n := len(dt.fake.CallsWithPrefix("launchctl", "bootstrap", "gui/501", path)); n != 1 {
		t.Fatalf("bootstrap calls = %d, want the job reloaded once", n)
	}
	if n := len(dt.fake.CallsWithPrefix("launchctl", "bootout", "gui/501/zhuravel.magnum.factory")); n != 1 {
		t.Fatalf("bootout calls = %d, want one", n)
	}
	if n := len(dt.fake.CallsWithPrefix("launchctl", "bootout", "gui/501/zhuravel.magnum")); n != 0 {
		t.Fatalf("the review daemon's job was booted out %d times", n)
	}
}

func TestFactoryUninstallRemovesOnlyItsOwnJob(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t, installRules(herdrPluginListEmpty)...)
	agents := filepath.Join(dt.userHome, "Library", "LaunchAgents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"zhuravel.magnum.plist", "zhuravel.magnum.factory.plist"} {
		if err := os.WriteFile(filepath.Join(agents, name), []byte("plist"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if code := dt.run("factory", "uninstall"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if _, err := os.Stat(filepath.Join(agents, "zhuravel.magnum.factory.plist")); !os.IsNotExist(err) {
		t.Fatalf("the factory's plist is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agents, "zhuravel.magnum.plist")); err != nil {
		t.Fatalf("factory uninstall removed the review daemon's plist: %v", err)
	}
}

func TestFactoryStatusJSONListsTasks(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	l := dt.ctx.Layout
	registrytest.Seed(t, l.FactoryDB())
	reg, err := registry.Open(l.FactoryDB(), registry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := reg.CreateTask(ctx, registry.Task{JiraKey: "PR-12340", Kind: "bug", Repo: "example/shop", Title: "Points vanish",
		State: registry.TaskNeedsYou, Stage: "questions", WaitsWho: "you", WaitsWhat: "2 questions before coding"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.CreateTask(ctx, registry.Task{Prompt: "rename the helper", Kind: "chore", Repo: "example/shop", Title: "Rename"}); err != nil {
		t.Fatal(err)
	}
	reg.Close()

	if code := dt.run("factory", "status", "--json"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	var got struct {
		Daemon struct {
			Running bool `json:"running"`
			PID     int  `json:"pid"`
		} `json:"daemon"`
		Tasks []struct {
			ID        int64  `json:"id"`
			Key       string `json:"key"`
			JiraKey   string `json:"jira_key"`
			Kind      string `json:"kind"`
			State     string `json:"state"`
			Stage     string `json:"stage"`
			WaitsWho  string `json:"waits_who"`
			WaitsWhat string `json:"waits_what"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(dt.stdout.Bytes(), &got); err != nil {
		t.Fatalf("%v:\n%s", err, dt.stdout)
	}
	if got.Daemon.Running || got.Daemon.PID != 0 {
		t.Fatalf("daemon %+v, want not running", got.Daemon)
	}
	if len(got.Tasks) != 2 {
		t.Fatalf("tasks %+v, want 2", got.Tasks)
	}
	first, second := got.Tasks[0], got.Tasks[1]
	if first.Key != "PR-12340" || first.JiraKey != "PR-12340" || first.State != registry.TaskNeedsYou || first.Stage != "questions" ||
		first.WaitsWho != "you" || first.WaitsWhat != "2 questions before coding" {
		t.Fatalf("first task %+v", first)
	}
	if second.Key != "task 2" || second.JiraKey != "" || second.Kind != "chore" || second.State != registry.TaskQueued {
		t.Fatalf("second task %+v", second)
	}
}

func TestFactoryStatusWithoutARegistryCreatesNone(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	if code := dt.run("factory", "status"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, dt.stderr)
	}
	if _, err := os.Stat(dt.ctx.Layout.FactoryDB()); !os.IsNotExist(err) {
		t.Fatalf("status created the factory registry: %v", err)
	}
	if out := dt.stdout.String(); !strings.Contains(out, "not running") || !strings.Contains(out, "no tasks") {
		t.Fatalf("status output:\n%s", out)
	}
}

// writeFactoryRegistryAt writes a factory registry migrated only to version.
func writeFactoryRegistryAt(t *testing.T, path string, version int) {
	t.Helper()
	ms, err := store.LoadMigrations(os.DirFS(filepath.Join("..", "factory", "registry")), "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, _, err := store.ApplyMigrations(context.Background(), db, ms[:version], nil); err != nil {
		t.Fatal(err)
	}
}

func factorySchema(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRowContext(context.Background(), "PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestTheCLIRefusesToMigrateUnderARunningDaemon(t *testing.T) {
	storetest.Serial(t)
	comm, args := daemonRulePS("/opt/bin/magnum", "/opt/bin/magnum factory daemon")
	dt := newDaemonGroupTest(t, comm, args)
	daemonSys.FactoryPID = func(paths.Layout) (int, error) { return 250, nil }
	db := dt.ctx.Layout.FactoryDB()
	writeFactoryRegistryAt(t, db, 1)

	if code := dt.run("factory", "status"); code != 1 {
		t.Fatalf("exit %d, want 1; stdout %s stderr %s", code, dt.stdout, dt.stderr)
	}
	for _, want := range []string{"schema 1", "pid 250", "magnum factory install"} {
		if !strings.Contains(dt.stderr.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, dt.stderr)
		}
	}
	if v := factorySchema(t, db); v != 1 {
		t.Fatalf("user_version %d after the refusal, want 1 (untouched)", v)
	}

	// With the daemon gone, the same command migrates.
	daemonSys.FactoryPID = func(paths.Layout) (int, error) { return 0, nil }
	dt.stderr.Reset()
	if code := dt.run("factory", "status"); code != 0 {
		t.Fatalf("exit %d without a daemon, stderr %s", code, dt.stderr)
	}
	if v := factorySchema(t, db); v != registry.LatestSchemaVersion() {
		t.Fatalf("user_version %d, want %d", v, registry.LatestSchemaVersion())
	}
}

func TestASecondFactoryDaemonMigratesNothingAndExitsZero(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	l := dt.ctx.Layout
	writeFactoryRegistryAt(t, l.FactoryDB(), 1) // a newer build's second daemon must not migrate the first one's file
	if err := os.MkdirAll(l.State(), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, held, err := engine.AcquireLock(l.FactoryLock()) // the running daemon's lock
	if err != nil || held {
		t.Fatalf("lock: held %v, %v", held, err)
	}
	defer unlock()

	if code := dt.run("factory", "daemon"); code != 0 {
		t.Fatalf("exit %d, want 0 (launchd must not relaunch it in a loop); stderr %s", code, dt.stderr)
	}
	if !strings.Contains(dt.stderr.String(), "already") {
		t.Fatalf("stderr %q: want one line saying a factory daemon already runs", dt.stderr)
	}
	if v := factorySchema(t, l.FactoryDB()); v != 1 {
		t.Fatalf("user_version %d, want 1: the second daemon migrated the registry", v)
	}
	if _, err := os.Stat(l.FactoryAgentsDB()); !os.IsNotExist(err) {
		t.Fatalf("the second daemon created the agents store: %v", err)
	}
}

func TestTheFactoryDaemonBuilderRefusesToMigrateUnderAnotherDaemon(t *testing.T) {
	storetest.Serial(t)
	comm, args := daemonRulePS("/opt/bin/magnum", "/opt/bin/magnum factory daemon")
	dt := newDaemonGroupTest(t, comm, args)
	daemonSys.FactoryPID = func(paths.Layout) (int, error) { return 250, nil }
	l := dt.ctx.Layout
	writeFactoryRegistryAt(t, l.FactoryDB(), 1)
	storetest.Seed(t, l.FactoryAgentsDB())

	d, err := buildFactoryDaemon(dt.ctx, slog.LevelInfo)
	if err == nil {
		d.Close()
		t.Fatal("the daemon built and migrated under another factory daemon")
	}
	if !strings.Contains(err.Error(), "pid 250") {
		t.Fatalf("err %v, want the refusal naming pid 250", err)
	}
	if v := factorySchema(t, l.FactoryDB()); v != 1 {
		t.Fatalf("user_version %d, want 1", v)
	}
}

func TestFactoryDaemonReadsPromptsOnceAtStart(t *testing.T) {
	storetest.Serial(t)
	dir := t.TempDir()
	writer := filepath.Join(dir, "factory-writer.md")
	if err := os.WriteFile(writer, []byte("writer v1 {{.TaskID}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	start := func() (*Context, *factoryDaemon) {
		t.Helper()
		cfg := config.Defaults()
		cfg.Pipeline.PromptsDir = dir
		c := &Context{Version: "test", Layout: paths.Layout{Home: home}, Config: cfg, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
		storetest.Seed(t, c.Layout.FactoryAgentsDB())
		registrytest.Seed(t, c.Layout.FactoryDB())
		d, err := buildFactoryDaemon(c, slog.LevelInfo)
		if err != nil {
			t.Fatalf("the factory daemon does not build: %v", err)
		}
		if d.Engine == nil {
			t.Fatal("no engine")
		}
		return c, d
	}

	c, d := start()
	if err := os.WriteFile(writer, []byte("writer v2 {{.TaskID}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := c.Config.ResolvePrompt("factory-writer.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.Text, "writer v1") {
		t.Fatalf("the running daemon reads %q: an edit reached it before a restart", p.Text)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	c, d = start() // the restart
	defer d.Close()
	p, err = c.Config.ResolvePrompt("factory-writer.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.Text, "writer v2") {
		t.Fatalf("after the restart the daemon reads %q", p.Text)
	}
	if _, err := os.Stat(c.Layout.FactoryLog()); err != nil {
		t.Fatalf("the daemon wrote no log of its own: %v", err)
	}
}

func TestTheFactoryOpensItsOwnHerdrSession(t *testing.T) {
	cfg := config.Defaults()
	cfg.Terminal.App, cfg.Terminal.Launcher, cfg.Terminal.Session = "custom", "myterm -e {herdr} {args}", "default"
	fake := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"/bin/sh"}}}}

	if err := factoryReveal(cfg, fake)(context.Background()); err != nil {
		t.Fatal(err)
	}

	calls := fake.CallsWithPrefix("/bin/sh")
	if len(calls) != 1 {
		t.Fatalf("calls %+v, want one launch", fake.Calls)
	}
	argv := strings.Join(calls[0].Args, " ")
	if !strings.HasSuffix(argv, "myterm -e herdr --session factory") {
		t.Fatalf("the launch runs %q, want the factory's session (not the review panes' %q)", argv, cfg.Terminal.Session)
	}
}

func TestFactoryKeyNamesATaskWithoutAJiraKeyByItsNumber(t *testing.T) {
	if got := factory.TaskKey(registry.Task{ID: 7, Prompt: "x"}); got != "task 7" {
		t.Fatalf("TaskKey = %q", got)
	}
	if got := factory.TaskKey(registry.Task{ID: 7, JiraKey: "PR-12345"}); got != "PR-12345" {
		t.Fatalf("TaskKey = %q", got)
	}
}

// The daemon builds with its agent runtime, and herdr's ping (the herdr
// hold) is the runtime's: on even with [factory] herdr_socket unset.
func TestTheFactoryDaemonBuildsWithItsRuntime(t *testing.T) {
	cfg := config.Defaults()
	c := &Context{Version: "test", Layout: paths.Layout{Home: t.TempDir()}, Config: cfg, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	storetest.Seed(t, c.Layout.FactoryAgentsDB())
	registrytest.Seed(t, c.Layout.FactoryDB())

	d, err := buildFactoryDaemon(c, slog.LevelInfo)
	if err != nil {
		t.Fatalf("the factory daemon does not build: %v", err)
	}
	defer d.Close()
	if d.Runtime == nil {
		t.Fatal("the daemon has no agent runtime")
	}
}

// The factory's git runs in the C locale: its refusals are read from git's
// English messages (a switch refused over your untracked file), and a
// daemon started in a terminal of another language with a git that ships
// translations would read none. A command's own environment stays; magnum's
// git is not the factory's and keeps its environment.
func TestTheFactorysGitRunsInTheCLocale(t *testing.T) {
	cfg := config.Defaults()
	c := &Context{Version: "test", Layout: paths.Layout{Home: t.TempDir()}, Config: cfg, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	storetest.Seed(t, c.Layout.FactoryAgentsDB())
	registrytest.Seed(t, c.Layout.FactoryDB())
	d, err := buildFactoryDaemon(c, slog.LevelInfo)
	if err != nil {
		t.Fatalf("the factory daemon does not build: %v", err)
	}
	defer d.Close()
	if _, ok := d.GitRun.(cLocale); !ok {
		t.Fatalf("the factory's git runs on %T, want the C locale's runner", d.GitRun)
	}

	fake := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"git"}}}}
	if _, err := (cLocale{fake}).Run(context.Background(), execx.Cmd{Name: "git", Args: []string{"status"},
		Env: map[string]string{"GIT_TERMINAL_PROMPT": "0"}}); err != nil {
		t.Fatal(err)
	}
	calls := fake.CallsWithPrefix("git")
	if len(calls) != 1 || calls[0].Env["LC_ALL"] != "C" || calls[0].Env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Fatalf("git ran with %+v, want LC_ALL=C beside its own environment", calls)
	}
}

// The daemon builds with its GitHub, the publish's: gh as your gh identity.
func TestTheFactoryDaemonBuildsWithItsGitHub(t *testing.T) {
	cfg := config.Defaults()
	c := &Context{Version: "test", Layout: paths.Layout{Home: t.TempDir()}, Config: cfg, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	storetest.Seed(t, c.Layout.FactoryAgentsDB())
	registrytest.Seed(t, c.Layout.FactoryDB())

	d, err := buildFactoryDaemon(c, slog.LevelInfo)
	if err != nil {
		t.Fatalf("the factory daemon does not build: %v", err)
	}
	defer d.Close()
	if d.GitHub == nil {
		t.Fatal("the daemon has no GitHub: nothing would publish")
	}
}

// The daemon builds with its copies: magnum's slot code over the factory's
// agents store, never the review registry.
func TestTheFactoryDaemonBuildsWithItsCopies(t *testing.T) {
	cfg := config.Defaults()
	c := &Context{Version: "test", Layout: paths.Layout{Home: t.TempDir()}, Config: cfg, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	storetest.Seed(t, c.Layout.FactoryAgentsDB())
	registrytest.Seed(t, c.Layout.FactoryDB())

	d, err := buildFactoryDaemon(c, slog.LevelInfo)
	if err != nil {
		t.Fatalf("the factory daemon does not build: %v", err)
	}
	defer d.Close()
	if d.Copies == nil {
		t.Fatal("the daemon has no copies")
	}
	if _, err := os.Stat(c.Layout.DB()); !os.IsNotExist(err) {
		t.Fatalf("the factory daemon opened the review registry: %v", err)
	}
}

// The copies' guard reads both herdr sessions: the factory's, whose panes
// the agents store records, and yours (the [herdr] socket), where your
// agents and consoles run.
func TestTheCopiesGuardReadsTheFactorysHerdrSessionAndYours(t *testing.T) {
	cfg := config.Defaults()
	cfg.Herdr.Socket = "/run/herdr/yours.sock"
	cfg.Factory.HerdrSocket = "/run/herdr/factory.sock"
	c := &Context{Version: "test", Layout: paths.Layout{Home: t.TempDir()}, Config: cfg, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	storetest.Seed(t, c.Layout.FactoryAgentsDB())
	registrytest.Seed(t, c.Layout.FactoryDB())

	d, err := buildFactoryDaemon(c, slog.LevelInfo)
	if err != nil {
		t.Fatalf("the factory daemon does not build: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() }) // a second Close does nothing
	factorySide, ok1 := d.Guard.Factory.(*herdr.Client)
	yoursSide, ok2 := d.Guard.Yours.(*herdr.Client)
	if !ok1 || !ok2 || factorySide.Socket != "/run/herdr/factory.sock" || yoursSide.Socket != "/run/herdr/yours.sock" {
		t.Fatalf("the copies' guard reads %+v and %+v, want the factory's session and yours", d.Guard.Factory, d.Guard.Yours)
	}
	d.Close()

	// A factory that runs in your session reads it once.
	cfg.Factory.HerdrSocket = cfg.Herdr.Socket
	one, err := buildFactoryDaemon(c, slog.LevelInfo)
	if err != nil {
		t.Fatalf("the factory daemon does not build: %v", err)
	}
	defer one.Close()
	if one.Guard.Yours != nil {
		t.Fatalf("the copies' guard reads your session twice: %+v", one.Guard.Yours)
	}
}

// The daemon delivers its alerts through magnum's notify: the toasts on
// your herdr session (osascript after, as [herdr] notify allows),
// deduplicated in the factory's agents store, never the review registry;
// the sidebar tokens on the factory's session. In your own session it is
// one herdr.
func TestTheFactoryDaemonBuildsWithItsAlerts(t *testing.T) {
	cfg := config.Defaults()
	cfg.Herdr.Socket = "/run/herdr/yours.sock"
	cfg.Factory.HerdrSocket = "/run/herdr/factory.sock"
	c := &Context{Version: "test", Layout: paths.Layout{Home: t.TempDir()}, Config: cfg, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	storetest.Seed(t, c.Layout.FactoryAgentsDB())
	registrytest.Seed(t, c.Layout.FactoryDB())

	d, err := buildFactoryDaemon(c, slog.LevelInfo)
	if err != nil {
		t.Fatalf("the factory daemon does not build: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() }) // a second Close does nothing
	n, side := d.Notifier, d.Sidebar
	if n == nil || side == nil {
		t.Fatalf("notifier %+v, sidebar %+v: no alert would be delivered", n, side)
	}
	toasts, ok1 := n.Herdr.(*herdr.Client)
	tokens, ok2 := side.Herdr.(*herdr.Client)
	if !ok1 || !ok2 || toasts.Socket != "/run/herdr/yours.sock" || tokens.Socket != "/run/herdr/factory.sock" {
		t.Fatalf("toasts on %+v and sidebar tokens on %+v, want your session and the factory's", n.Herdr, side.Herdr)
	}
	if n.Store != d.Agents || n.Runner == nil || !n.Enabled {
		t.Errorf("notifier store %v (want the agents store), runner %v, enabled %v", n.Store, n.Runner, n.Enabled)
	}
	if _, err := os.Stat(c.Layout.DB()); !os.IsNotExist(err) {
		t.Fatalf("the factory daemon opened the review registry: %v", err)
	}
	d.Close()

	cfg.Factory.HerdrSocket = cfg.Herdr.Socket
	cfg.Herdr.Notify = false
	one, err := buildFactoryDaemon(c, slog.LevelInfo)
	if err != nil {
		t.Fatalf("the factory daemon does not build: %v", err)
	}
	defer one.Close()
	if toasts, ok := one.Notifier.Herdr.(*herdr.Client); !ok || toasts.Socket != cfg.Herdr.Socket || one.Notifier.Enabled {
		t.Fatalf("in your own session: toasts on %+v, enabled %v; want that session, off as [herdr] notify says", one.Notifier.Herdr,
			one.Notifier.Enabled)
	}
}

// The factory's panes live in its own herdr session: the socket [factory]
// herdr_socket names, else the named session's own, never the review panes'
// default one.
func TestTheFactorySessionHasItsOwnHerdrSocket(t *testing.T) {
	cfg := config.Defaults()
	if got := factoryHerdrSocket(cfg); !strings.HasSuffix(got, "/.config/herdr/sessions/factory/herdr.sock") {
		t.Fatalf("socket %q, want the factory session's own", got)
	}
	cfg.Factory.HerdrSocket = "/run/herdr/factory.sock"
	if got := factoryHerdrSocket(cfg); got != "/run/herdr/factory.sock" {
		t.Fatalf("socket %q, want the configured one", got)
	}
}
