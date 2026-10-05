package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/launchd"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/store"
)

type fakeDoctorHerdr struct {
	info    herdr.ServerInfo
	pingErr error
	plugins []string
}

func (f *fakeDoctorHerdr) Ping(context.Context) (herdr.ServerInfo, error) { return f.info, f.pingErr }

func (f *fakeDoctorHerdr) Call(_ context.Context, method string, _, out any) error {
	if f.pingErr != nil {
		return f.pingErr
	}
	if method != "plugin.list" {
		return fmt.Errorf("unexpected method %s", method)
	}
	var ps []map[string]any
	for _, p := range f.plugins {
		ps = append(ps, map[string]any{"plugin_id": p, "enabled": true})
	}
	b, _ := json.Marshal(map[string]any{"plugins": ps})
	return json.Unmarshal(b, out)
}

type fakeDoctorAgents struct {
	wrapper map[string]bool
}

func (f *fakeDoctorAgents) Wrapper(_ context.Context, kind string) (bool, error) {
	return f.wrapper[kind], nil
}

type fakeDoctorMySQL struct {
	pingErr error
	dbs     []mysqlx.Database
}

func (f *fakeDoctorMySQL) Ping(context.Context) error { return f.pingErr }
func (f *fakeDoctorMySQL) ListSuffixed(context.Context) ([]mysqlx.Database, error) {
	return f.dbs, f.pingErr
}

// doctorSchema is a minimal `herdr api schema --json`.
func doctorSchema(methods []string) string {
	var one []string
	for _, m := range methods {
		one = append(one, fmt.Sprintf(`{"properties":{"method":{"const":%q}}}`, m))
	}
	return `{"protocol":22,"schemas":{"request":{"oneOf":[` + strings.Join(one, ",") + `]}}}`
}

func doctorFixture(t *testing.T) (*inspFixture, doctorDeps, *fakeDoctorHerdr, *fakeDoctorAgents) {
	t.Helper()
	f := newInspFixture(t)
	st := f.store()
	if err := f.Ctx.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	cfg := f.Ctx.Config
	skill := cfg.JudgeFor(nil).Skill
	// Main clone, skill, one provisioned slot, the user's codex staging dir.
	for _, d := range []string{cfg.Pools[0].MainClone + "/.git", f.Home + "/talkable.review1", f.Home + "/user/.codex/.tmp/marketplaces/.staging",
		filepath.Dir(skill)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(skill, []byte("skill"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspSeedSlot(t, st, f.Home, "review1", store.SlotFree, nil)
	if err := st.SetKV(context.Background(), engine.KVIdentityCheck("zhuravel"), "pass"); err != nil {
		t.Fatal(err)
	}
	f.Runner.Rules = []execx.Rule{
		{Prefix: []string{"herdr", "--version"}, Result: execx.Result{Stdout: []byte("herdr 0.9.3\n")}},
		{Prefix: []string{"herdr", "api", "schema"}, Result: execx.Result{Stdout: []byte(doctorSchema(herdr.RequiredMethods()))}},
		{Prefix: []string{"codex", "--version"}, Result: execx.Result{Stdout: []byte("codex-cli 0.160.0\n")}},
		{Prefix: []string{"claude", "--version"}, Result: execx.Result{Stdout: []byte("2.1.287 (Claude Code)\n")}},
		{Prefix: []string{"zsh", "-ic", "whence -w codex"}, Result: execx.Result{Stdout: []byte("codex: function\n")}},
		{Prefix: []string{"zsh", "-ic", "whence -w claude"}, Result: execx.Result{Stdout: []byte("claude: command\n")}},
		{Prefix: []string{"codex", "login", "status"}, Result: execx.Result{Stderr: []byte("Logged in using ChatGPT\n")}},
		{Prefix: []string{"claude", "auth", "status"}, Result: execx.Result{Stdout: []byte(`{"loggedIn": true, "authMethod": "claude.ai"}`)}},
		{Prefix: []string{"gh", "api", "user"}, Result: execx.Result{Stdout: []byte("zhuravel\n")}},
		{Prefix: []string{"git", "-C", cfg.Pools[0].MainClone, "ls-remote"}, Result: execx.Result{Stdout: []byte("0123456789abcdef\trefs/heads/master\n")}},
		{Prefix: []string{"mise", "-C", f.Home + "/talkable.review1", "exec", "--", "ruby", "-v"}, Result: execx.Result{Stdout: []byte("ruby 3.4.1\n")}},
		{Prefix: []string{"du", "-sk"}, Result: execx.Result{Stdout: []byte("256976\t/x\n")}},
		{Prefix: []string{"zsh", "-lc", "print -r -- $PATH"}, Result: execx.Result{Stdout: []byte(f.Home + "/user/.local/share/mise/shims:/usr/local/bin:/usr/bin:/bin\n")}},
	}
	h := &fakeDoctorHerdr{info: herdr.ServerInfo{Version: "0.9.1", Protocol: 22}, plugins: []string{"other.plugin", "zhuravel.magnum"}}
	ag := &fakeDoctorAgents{wrapper: map[string]bool{"codex": true, "claude": true}}
	d := doctorDeps{
		Config: cfg, Layout: f.Ctx.Layout, Store: st, Run: f.Runner, Herdr: h, Agents: ag,
		MySQL:     &fakeDoctorMySQL{dbs: make([]mysqlx.Database, 16)},
		Inventory: &fakeScanner{inv: inventory.Inventory{DatabasesListed: true}},
		Launchd: func(context.Context) (launchd.Info, error) {
			return launchd.Info{State: launchd.Running, PID: 77}, nil
		},
		DiskFree: func(string) (uint64, error) { return 30 << 30, nil },
		Getenv:   func(string) string { return "" },
		UserHome: f.Home + "/user",
	}
	return f, d, h, ag
}

func doctorByName(cs []doctorCheck) map[string]doctorCheck {
	m := map[string]doctorCheck{}
	for _, c := range cs {
		m[c.Name] = c
	}
	return m
}

// The shell probes run detached from the terminal (NoTTY): from a CLI in a
// terminal an interactive zsh shares it from a background process group,
// where shell startup that touches it stops zsh until the timeout.
func TestDoctorShellProbesHaveNoTerminal(t *testing.T) {
	_, d, _, _ := doctorFixture(t)
	doctorRun(context.Background(), d)
	probes := d.Run.(*execx.Fake).CallsWithPrefix("zsh", "-ic")
	if len(probes) == 0 {
		t.Fatal("no zsh probe ran")
	}
	for _, c := range probes {
		if !c.NoTTY {
			t.Errorf("%s runs on the terminal", c.String())
		}
	}
}

// A zsh probe that does not answer is not a missing CLI: the fix says what
// to check in the shell's startup, and the login check says why it did not run.
func TestDoctorShellProbeTimeoutSaysWhatToCheck(t *testing.T) {
	_, d, _, _ := doctorFixture(t)
	fake := d.Run.(*execx.Fake)
	fake.Rules = append([]execx.Rule{{Prefix: []string{"zsh", "-ic", "whence -w codex"}, Err: context.DeadlineExceeded}}, fake.Rules...)
	m := doctorByName(doctorRun(context.Background(), d))
	if c := m["codex"]; c.Status != doctorFail || !strings.Contains(c.Fix, "must answer within seconds") || strings.Contains(c.Fix, "install") {
		t.Fatalf("codex = %+v", c)
	}
	if c := m["codex login"]; c.Status != "SKIP" || !strings.Contains(c.Detail, "not run: zsh did not say where codex is") {
		t.Fatalf("codex login = %+v", c)
	}
}

func TestDoctorHealthy(t *testing.T) {
	_, d, _, _ := doctorFixture(t)
	cs := doctorRun(context.Background(), d)
	m := doctorByName(cs)
	want := map[string]string{
		"config": "PASS", "herdr": "PASS", "herdr version": "WARN", "herdr methods": "PASS",
		"codex": "PASS", "claude": "PASS", "codex login": "PASS", "claude login": "PASS", "codex wrapper": "PASS", "claude wrapper": "PASS",
		"skill": "PASS", "prompts": "PASS", "gh": "PASS", "identity zhuravel": "PASS", "identity talkable-app": "WARN",
		"mysql": "PASS", "clone talkable/talkable": "PASS", "origin talkable/talkable": "PASS",
		"mise exec": "PASS", "login shell": "PASS", "disk": "PASS", "codex staging": "PASS", "launchd": "PASS", "plugin": "PASS", "registry": "PASS",
	}
	for name, status := range want {
		c, ok := m[name]
		if !ok {
			t.Errorf("missing check %q", name)
			continue
		}
		if c.Status != status {
			t.Errorf("%s = %s %q (fix %q), want %s", name, c.Status, c.Detail, c.Fix, status)
		}
	}
	if !strings.Contains(m["herdr version"].Detail, "0.9.3") || !strings.Contains(m["herdr version"].Detail, "0.9.1") {
		t.Errorf("version detail = %q", m["herdr version"].Detail)
	}
	if !strings.Contains(m["mysql"].Detail, "16") || !strings.Contains(m["mise exec"].Detail, "ruby 3.4.1") ||
		!strings.Contains(m["codex"].Detail, "0.160.0") || !strings.Contains(m["codex staging"].Detail, "251M") {
		t.Errorf("details: mysql=%q mise=%q codex=%q staging=%q", m["mysql"].Detail, m["mise exec"].Detail, m["codex"].Detail, m["codex staging"].Detail)
	}
	if !strings.Contains(m["identity talkable-app"].Fix, "MAGNUM_TEST_NO_KEY") && !strings.Contains(m["identity talkable-app"].Fix, "identities check") {
		t.Errorf("app identity fix = %q", m["identity talkable-app"].Fix)
	}
	for _, c := range cs {
		if _, ok := want[c.Name]; !ok && c.Status == doctorFail {
			t.Errorf("unexpected FAIL outside want: %s %q (fix %q)", c.Name, c.Detail, c.Fix)
		}
	}
	// Ordered as defined.
	if cs[0].Name != "config" || cs[len(cs)-1].Name != "registry" {
		t.Errorf("order: first %s last %s", cs[0].Name, cs[len(cs)-1].Name)
	}
}

func TestDoctorFailures(t *testing.T) {
	_, d, h, ag := doctorFixture(t)
	h.pingErr = fmt.Errorf("dial: %w", herdr.ErrUnavailable)
	ag.wrapper["codex"] = false
	doctorKindArgs(t, d, "codex") // a plain binary without the autonomy flag
	d.MySQL = &fakeDoctorMySQL{pingErr: errors.New("connection refused")}
	d.DiskFree = func(string) (uint64, error) { return 2 << 30, nil }
	d.Config.Daemon.MinFreeDiskGB = 15
	d.Launchd = func(context.Context) (launchd.Info, error) { return launchd.Info{State: launchd.NotLoaded}, nil }
	if err := os.Remove(d.Config.JudgeFor(nil).Skill); err != nil {
		t.Fatal(err)
	}
	d.Inventory = &fakeScanner{inv: inventory.Inventory{Drift: []inventory.Finding{{Kind: "lost_slot", Safe: true}, {Kind: "head_drift"}}}}
	f := d.Run.(*execx.Fake)
	f.Rules = append([]execx.Rule{
		{Prefix: []string{"gh", "api", "user"}, Err: &execx.ExitError{Code: 1, Stderr: "gh: To get started with GitHub CLI, please run:  gh auth login"}},
		{Prefix: []string{"herdr", "api", "schema"}, Result: execx.Result{Stdout: []byte(doctorSchema([]string{"ping"}))}},
		{Prefix: []string{"codex", "login", "status"}, Result: execx.Result{Code: 1, Stderr: []byte("Not logged in\n")}},
		{Prefix: []string{"zsh", "-ic", "whence -w claude"}, Result: execx.Result{Code: 1, Stdout: []byte("claude: none\n")}},
	}, f.Rules...)

	m := doctorByName(doctorRun(context.Background(), d))
	want := map[string]struct{ status, fix string }{
		"herdr":         {"FAIL", "start herdr"},
		"herdr methods": {"FAIL", "upgrade herdr"},
		"codex":         {"PASS", ""},
		"codex login":   {"FAIL", "run `codex login`"},
		"claude":        {"FAIL", "install Claude Code"},
		"claude login":  {"SKIP", ""},
		"codex wrapper": {"FAIL", "--dangerously-bypass-approvals-and-sandbox"},
		"skill":         {"FAIL", "judge role's skill"},
		"gh":            {"FAIL", "gh auth login"},
		"mysql":         {"FAIL", "DBngin"},
		"disk":          {"FAIL", "magnum cleanup"},
		"launchd":       {"WARN", "magnum install"},
		"plugin":        {"WARN", "herdr"},
		"registry":      {"WARN", "magnum status"},
	}
	for name, w := range want {
		c := m[name]
		if c.Status != w.status || !strings.Contains(c.Fix, w.fix) {
			t.Errorf("%s = %s %q fix %q, want %s with fix containing %q", name, c.Status, c.Detail, c.Fix, w.status, w.fix)
		}
	}
	if !strings.Contains(m["registry"].Detail, "2 drift findings") {
		t.Errorf("registry detail = %q", m["registry"].Detail)
	}
}

func TestDoctorRender(t *testing.T) {
	cs := []doctorCheck{
		{Name: "a", Status: doctorPass, Detail: "fine"},
		{Name: "b", Status: doctorWarn, Detail: "meh", Fix: "do x"},
		{Name: "c", Status: doctorFail, Detail: "bad", Fix: "do y"},
	}
	f := newInspFixture(t)
	code := doctorPrint(f.Ctx, cs, false)
	out := f.Out.String()
	if code != 1 || !strings.Contains(out, "PASS  fine") || !strings.Contains(out, "WARN  meh\n      fix: do x") ||
		!strings.Contains(out, "1 PASS, 1 WARN, 1 FAIL") {
		t.Fatalf("code %d out:\n%s", code, out)
	}
	f.Out.Reset()
	if code := doctorPrint(f.Ctx, cs[:2], true); code != 0 {
		t.Fatalf("json code %d", code)
	}
	var got []doctorCheck
	if err := json.Unmarshal(f.Out.Bytes(), &got); err != nil || len(got) != 2 {
		t.Fatalf("json %s err %v", f.Out.String(), err)
	}
}

// doctorSandbox points the command-level doctor at the fixture: HOME, the
// mise data dir, the app key variable and the disk probe are the fixture's, so
// no check reads the real machine. It returns the fixture user's home.
func doctorSandbox(t *testing.T, f *inspFixture) string {
	t.Helper()
	home := filepath.Join(f.Home, "user")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("MISE_DATA_DIR", "")
	t.Setenv("MAGNUM_TEST_NO_KEY", "")
	oldDisk := doctorDiskFree
	doctorDiskFree = func(string) (uint64, error) { return 30 << 30, nil }
	t.Cleanup(func() { doctorDiskFree = oldDisk })
	return home
}

// TestDoctorCommandRuns runs `magnum doctor --json` through the command tree
// with every source faked or unreachable, and checks named results.
func TestDoctorCommandRuns(t *testing.T) {
	f := newInspFixture(t)
	home := doctorSandbox(t, f)
	staging := filepath.Join(home, ".codex", ".tmp", "marketplaces", ".staging")
	skill := filepath.Join(f.Home, "skills", "magnum-review", "SKILL.md")
	for _, dir := range []string{staging, filepath.Dir(skill)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(skill, []byte("skill"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Everything else the runner is asked fails ("no rule"): doctor must
	// still finish and report FAIL or WARN lines rather than crash.
	f.Runner.Rules = []execx.Rule{{Prefix: []string{"du", "-sk"}, Result: execx.Result{Stdout: []byte("256976\t" + staging + "\n")}}}

	code := f.run("doctor", "--json")
	var got []doctorCheck
	if err := json.Unmarshal(f.Out.Bytes(), &got); err != nil {
		t.Fatalf("code %d json err %v out %s stderr %s", code, err, f.Out.String(), f.Err.String())
	}
	if code != 1 {
		t.Fatalf("an unreachable herdr must fail doctor, code %d", code)
	}
	m := doctorByName(got)
	want := map[string]struct{ status, detail string }{
		"config":                  {doctorPass, filepath.Join(f.Home, "config.toml")},
		"herdr":                   {doctorFail, "no-herdr.sock"},
		"mysql":                   {doctorFail, "MySQL does not answer"},
		"gh":                      {doctorFail, "gh cannot read the GitHub API"},
		"clone talkable/talkable": {doctorFail, "missing at " + filepath.Join(f.Home, "talkable")},
		"identity talkable-app":   {doctorWarn, "MAGNUM_TEST_NO_KEY is not set"},
		"codex staging":           {doctorPass, "~/.codex/.tmp/marketplaces/.staging is 251M"},
		"disk":                    {doctorPass, "free on the disk holding ~ "},
		"launchd":                 {doctorWarn, "could not ask launchctl"},
	}
	for name, w := range want {
		c, ok := m[name]
		if !ok {
			t.Errorf("missing check %q in %d checks", name, len(got))
			continue
		}
		if c.Status != w.status || !strings.Contains(c.Detail, w.detail) {
			t.Errorf("%s = %s %q, want %s containing %q", name, c.Status, c.Detail, w.status, w.detail)
		}
	}
	if c, ok := m["skill"]; !ok || c.Status != doctorPass || !strings.Contains(c.Detail, skill) {
		t.Errorf("skill = %+v", c)
	}
	if _, ok := m["registry"]; !ok {
		t.Error("missing check \"registry\"")
	}
	if last := got[len(got)-1].Name; got[0].Name != "config" || !strings.HasPrefix(last, "registry") {
		t.Errorf("order: first %s last %s", got[0].Name, last)
	}
	if code := f.run("doctor", "extra"); code != 2 {
		t.Fatalf("extra arg code %d", code)
	}
}

func TestDoctorCrashedLaunchdJobPointsAtLaunchdLog(t *testing.T) {
	_, d, _, _ := doctorFixture(t)
	d.Launchd = func(context.Context) (launchd.Info, error) {
		return launchd.Info{State: launchd.State("waiting"), Exited: true, LastExitCode: 2}, nil
	}
	c := doctorByName(doctorRun(context.Background(), d))["launchd"]
	if c.Status != "WARN" || !strings.Contains(c.Detail, "last exit code 2") || !strings.Contains(c.Fix, "launchd.log") || !strings.Contains(c.Fix, "magnum logs") {
		t.Fatalf("launchd check = %+v", c)
	}
}

func TestDoctorChecksEveryRoleKind(t *testing.T) {
	_, d, _, ag := doctorFixture(t)
	cfg := d.Config
	cfg.Roles = []config.Role{
		{Name: "omp-judge", Kind: config.KindOMP, Judge: true, Prompt: "judge-initial.md"},
		{Name: "droid-lint", Kind: config.KindShell, Tool: config.KindDroid, Command: "droid exec lint"},
		{Name: "claude-review", Kind: config.KindClaude, Prompt: "nope.md"},
	}
	cfg.Normalize()
	ag.wrapper[config.KindOMP] = true
	f := d.Run.(*execx.Fake)
	f.Rules = append([]execx.Rule{
		{Prefix: []string{"zsh", "-ic", "whence -w omp"}, Result: execx.Result{Stdout: []byte("noise from .zshrc\nomp: command\n")}},
		{Prefix: []string{"zsh", "-ic", "whence -w droid"}, Result: execx.Result{Stdout: []byte("droid: alias\n")}},
		{Prefix: []string{"omp", "--version"}, Result: execx.Result{Stdout: []byte("omp 18.4\n")}},
		{Prefix: []string{"droid", "--version"}, Result: execx.Result{Stdout: []byte("0.232\n")}},
	}, f.Rules...)

	cs := doctorAgentChecks(context.Background(), d)
	var names []string
	for _, c := range cs {
		names = append(names, c.Name+"="+c.Status)
	}
	want := "omp=PASS,omp login=SKIP,droid=PASS,droid login=SKIP,claude=PASS,claude login=PASS,omp wrapper=PASS,droid wrapper=WARN,claude wrapper=PASS"
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("checks = %s\nwant     %s", got, want)
	}
	m := doctorByName(cs)
	if !strings.Contains(m["droid"].Detail, "used by droid-lint") || !strings.Contains(m["omp"].Detail, "omp 18.4") ||
		!strings.Contains(m["omp login"].Detail, "[kinds.omp] login_check") {
		t.Errorf("details: droid=%q omp=%q omp login=%q", m["droid"].Detail, m["omp"].Detail, m["omp login"].Detail)
	}
	if n := len(f.CallsWithPrefix("codex")); n != 0 {
		t.Errorf("codex probed %d times though no role uses it", n)
	}

	pm := doctorByName(doctorPrompts(context.Background(), d))
	if c := pm["prompt nope.md"]; c.Status != doctorFail || !strings.Contains(c.Detail, "role claude-review") || !strings.Contains(c.Fix, "magnum roles") {
		t.Errorf("missing prompt check = %+v", c)
	}
	if _, ok := pm["prompts"]; ok {
		t.Error("prompts PASS despite a missing prompt")
	}
}

// Codex and Claude run tool commands as `zsh -lc`; the mise shims must come
// before /usr/bin there, or reviews see the system Ruby and Node.
func TestDoctorLoginShellSeesMiseShims(t *testing.T) {
	_, d, _, _ := doctorFixture(t)
	fake := d.Run.(*execx.Fake)
	cases := []struct{ name, path, want string }{
		{"shims first", d.UserHome + "/.local/share/mise/shims:/usr/bin:/bin", "PASS"},
		{"shims after /usr/bin", "/usr/local/bin:/usr/bin:/bin:" + d.UserHome + "/.local/share/mise/shims", "FAIL"},
		{"no shims", "/usr/local/bin:/usr/bin:/bin", "FAIL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake.Rules = append([]execx.Rule{{Prefix: []string{"zsh", "-lc", "print -r -- $PATH"}, Result: execx.Result{Stdout: []byte(tc.path + "\n")}}}, fake.Rules...)
			c := doctorLoginShell(context.Background(), d)[0]
			if c.Status != tc.want {
				t.Fatalf("status = %s %q, want %s", c.Status, c.Detail, tc.want)
			}
			if tc.want == "FAIL" && !strings.Contains(c.Fix, "mise activate zsh --shims") {
				t.Fatalf("fix = %q", c.Fix)
			}
		})
	}
}

// A second machine without a pool, MySQL or mise gets SKIP for what it does
// not use, not failures with fixes it does not need.
func TestDoctorChecksOnlyWhatTheConfigUses(t *testing.T) {
	_, d, _, _ := doctorFixture(t)
	cfg := *d.Config
	cfg.Pools = nil
	d.Config = &cfg
	d.MySQL = &fakeDoctorMySQL{pingErr: errors.New("connection refused")} // would FAIL if asked
	fake := d.Run.(*execx.Fake)
	m := doctorByName(doctorRun(context.Background(), d))
	for _, name := range []string{"mysql", "mise exec", "login shell", "launchd mise"} {
		if m[name].Status != doctorSkip {
			t.Errorf("%s = %s %q, want SKIP", name, m[name].Status, m[name].Detail)
		}
	}
	if n := len(fake.CallsWithPrefix("zsh", "-lc")); n != 0 {
		t.Errorf("the login shell was probed %d times without mise", n)
	}

	// A pool without databases still needs mise, never MySQL.
	cfg.Pools = []config.Pool{{Repo: "talkable/talkable", MainClone: d.Config.Watches[0].CloneRoot}}
	m = doctorByName(doctorRun(context.Background(), d))
	if m["mysql"].Status != doctorSkip || m["login shell"].Status != doctorPass {
		t.Errorf("pool without databases: mysql %s, login shell %s %q", m["mysql"].Status, m["login shell"].Status, m["login shell"].Detail)
	}

	// mise on PATH alone turns the login-shell check on.
	cfg.Pools = nil
	fake.Rules = append([]execx.Rule{{Prefix: []string{"/bin/sh", "-c", "command -v mise"}, Result: execx.Result{Stdout: []byte("/opt/homebrew/bin/mise\n")}}}, fake.Rules...)
	if c := doctorLoginShell(context.Background(), d)[0]; c.Status != doctorPass {
		t.Errorf("mise on PATH: login shell %s %q", c.Status, c.Detail)
	}
}

// The LaunchAgent starts the daemon through mise (magnum install); a
// missing mise means launchd cannot start magnum at all.
func TestDoctorChecksTheMiseTheLaunchAgentUses(t *testing.T) {
	_, d, _, _ := doctorFixture(t)
	cfg := *d.Config
	cfg.Pools = nil
	d.Config = &cfg
	plist := launchd.AgentPath(d.UserHome, launchd.DefaultLabel)
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(program string) {
		t.Helper()
		args := []string{program, "-C", d.Layout.Home, "exec", "--", d.Layout.Binary(), "daemon"}
		if !isMiseProgram(program) {
			args = []string{program, "daemon"}
		}
		b := launchd.Plist(launchd.Options{Label: launchd.DefaultLabel, ProgramArguments: args,
			Env: map[string]string{"PATH": "/usr/bin", "ProgramArguments": "decoy"}})
		if err := os.WriteFile(plist, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mise := filepath.Join(t.TempDir(), "mise")

	write(mise) // not installed
	c := doctorLaunchdMise(context.Background(), d)[0]
	if c.Status != doctorFail || !strings.Contains(c.Detail, "does not exist") || !strings.Contains(c.Fix, "magnum install") {
		t.Fatalf("missing mise: %+v", c)
	}
	if ls := doctorLoginShell(context.Background(), d)[0]; ls.Status == doctorSkip {
		t.Errorf("a LaunchAgent that uses mise must turn the login-shell check on: %+v", ls)
	}

	if err := os.WriteFile(mise, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if c := doctorLaunchdMise(context.Background(), d)[0]; c.Status != doctorPass {
		t.Fatalf("installed mise: %+v", c)
	}

	write(d.Layout.Binary()) // a LaunchAgent written without mise
	if c := doctorLaunchdMise(context.Background(), d)[0]; c.Status != doctorSkip {
		t.Fatalf("no mise in the LaunchAgent: %+v", c)
	}
	if err := os.WriteFile(plist, []byte("<plist><dict><key>Label</key>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := doctorLaunchdMise(context.Background(), d)[0]; c.Status != doctorWarn {
		t.Fatalf("unreadable plist: %+v", c)
	}
}

// Doctor never scripts the terminal: an AppleScript call would start a
// terminal that is not running and, before the operator decided, raise
// macOS's Automation dialog, and doctor must not open windows or take
// focus. On macOS a terminal magnum scripts with AppleScript gets a SKIP
// line that names where the permission is granted; nothing runs osascript.
// A terminal it does not script that way, or another OS, gets no line.
func TestDoctorNamesTheAutomationPermissionWithoutScriptingTheTerminal(t *testing.T) {
	_, d, _, _ := doctorFixture(t)
	goos := doctorGOOS
	t.Cleanup(func() { doctorGOOS = goos })
	fake := d.Run.(*execx.Fake)
	for _, tc := range []struct {
		goos, app string
		want      string // the terminal named in the fix; "" = no line
	}{
		{"darwin", "iTerm2", "iTerm"},
		{"darwin", "Terminal", "Terminal"},
		{"darwin", "Ghostty", "Ghostty"},
		{"darwin", "WezTerm", ""},
		{"darwin", "custom", ""},
		{"linux", "iTerm2", ""},
	} {
		t.Run(tc.goos+" "+tc.app, func(t *testing.T) {
			doctorGOOS = tc.goos
			cfg := *d.Config
			cfg.Terminal.App = tc.app
			d := d
			d.Config = &cfg
			calls := len(fake.Calls)
			c, ok := doctorByName(doctorRun(context.Background(), d))["terminal"]
			for _, call := range fake.Calls[calls:] {
				if strings.Contains(call.Name, "osascript") || call.Name == "open" || strings.HasSuffix(call.Name, "/open") {
					t.Errorf("doctor ran %s %q", call.Name, call.Args)
				}
			}
			if tc.want == "" {
				if ok {
					t.Fatalf("terminal check = %+v, want none", c)
				}
				return
			}
			if !ok || c.Status != doctorSkip || !strings.Contains(c.Fix, "Privacy & Security → Automation → allow "+tc.want) {
				t.Fatalf("terminal check = %+v (found %v), want SKIP naming the Automation permission for %s", c, ok, tc.want)
			}
		})
	}
}

// A pool that names schema_paths without reset_db never loads a PR's schema
// (neither before the reviewers nor on release): doctor warns with the fix.
// A pool that has both says when reset_db runs; one without schema_paths
// gets no line.
func TestDoctorWarnsAboutSchemaPathsWithoutResetDB(t *testing.T) {
	_, d, _, _ := doctorFixture(t)
	off := false
	for _, tc := range []struct {
		name       string
		pool       config.Pool
		status     string
		detail     string
		fixMention string
	}{
		{"schema_paths without reset_db", config.Pool{Repo: "talkable/talkable", SchemaPaths: []string{"db/"}}, doctorWarn,
			"schema_paths without reset_db", "reset_db"},
		{"both", config.Pool{Repo: "talkable/talkable", SchemaPaths: []string{"db/"}, ResetDB: []string{"bin/rails db:schema:load"}}, doctorPass,
			"before the reviewers", ""},
		{"turned off", config.Pool{Repo: "talkable/talkable", SchemaPaths: []string{"db/"}, ResetDB: []string{"bin/rails db:schema:load"},
			ResetDBOnSchemaChange: &off}, doctorPass, "on release only", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := *d.Config
			cfg.Pools = []config.Pool{tc.pool}
			d := d
			d.Config = &cfg
			c, ok := doctorByName(doctorRun(context.Background(), d))["schema talkable/talkable"]
			if !ok || c.Status != tc.status || !strings.Contains(c.Detail, tc.detail) || !strings.Contains(c.Fix, tc.fixMention) {
				t.Fatalf("schema check = %+v (found %v), want %s with %q, fix naming %q", c, ok, tc.status, tc.detail, tc.fixMention)
			}
		})
	}
	cfg := *d.Config
	cfg.Pools = []config.Pool{{Repo: "talkable/talkable", ResetDB: []string{"bin/rails db:schema:load"}}}
	d.Config = &cfg
	if c, ok := doctorByName(doctorRun(context.Background(), d))["schema talkable/talkable"]; ok {
		t.Fatalf("a pool without schema_paths got a schema check: %+v", c)
	}
}
