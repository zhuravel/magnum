package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/paths"
)

// initTest runs `magnum init` in a temporary checkout holding the
// committed config.toml, answering from input; gh answers through fake.
type initTest struct {
	home     string
	out, err bytes.Buffer
	fake     *execx.Fake
	c        *Context
}

func newInitTest(t *testing.T, ghLogin string) *initTest {
	t.Helper()
	t.Setenv("MAGNUM_CONFIG", "")
	it := &initTest{home: t.TempDir(), fake: &execx.Fake{}}
	base, err := os.ReadFile(filepath.Join("..", "..", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(it.home, "config.toml"), base, 0o644); err != nil {
		t.Fatal(err)
	}
	if ghLogin != "" {
		it.fake.Rules = []execx.Rule{{Prefix: []string{"gh", "api", "user"}, Result: execx.Result{Stdout: []byte(ghLogin + "\n")}}}
	} else {
		it.fake.Rules = []execx.Rule{{Prefix: []string{"gh"}, Result: execx.Result{Code: 1, Stderr: []byte("not logged in")}}}
	}
	oldSys, oldStdin := daemonSys, inspStdin
	t.Cleanup(func() { daemonSys, inspStdin = oldSys, oldStdin })
	daemonSys.Runner = it.fake
	daemonSys.Getenv = func(string) string { return "" }
	it.c = &Context{Version: "test", Layout: paths.Layout{Home: it.home}, Stdout: &it.out, Stderr: &it.err}
	return it
}

func (it *initTest) run(t *testing.T, input string, args ...string) int {
	t.Helper()
	it.out.Reset()
	it.err.Reset()
	inspStdin = strings.NewReader(input)
	it.c.Config = nil
	return execute(it.c, append([]string{"init"}, args...))
}

func (it *initTest) local() string { return filepath.Join(it.home, "config.local.toml") }

func TestInitWritesAMinimalConfigForTheGHLogin(t *testing.T) {
	it := newInitTest(t, "octo-cat")
	// Enter takes gh's login; a URL is accepted for the repository; enter
	// keeps posting as the login.
	if code := it.run(t, "\nhttps://github.com/example/widgets.git\n\n"); code != 0 {
		t.Fatalf("code %d\nstdout %s\nstderr %s", code, it.out.String(), it.err.String())
	}
	if !strings.Contains(it.out.String(), "Your GitHub login (gh polls GitHub as it) [octo-cat]:") {
		t.Errorf("the gh login is not the default:\n%s", it.out.String())
	}
	b, err := os.ReadFile(it.local())
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(it.local()); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	cfg, err := config.Load(it.c.Layout, filepath.Join(it.home, "config.toml"))
	if err != nil {
		t.Fatalf("written config does not load: %v\n%s", err, b)
	}
	if len(cfg.Identities) != 1 || cfg.Identities[0].Name != "octo-cat" || cfg.Identities[0].Kind != "gh" || cfg.Identities[0].Login != "octo-cat" {
		t.Errorf("identities %+v", cfg.Identities)
	}
	if len(cfg.Watches) != 1 || cfg.Watches[0].Owner != "example" || len(cfg.Watches[0].Include) != 1 || cfg.Watches[0].Include[0] != "widgets" ||
		cfg.Watches[0].Identity != "octo-cat" || cfg.Watches[0].PollIdentity != "octo-cat" {
		t.Errorf("watches %+v", cfg.Watches)
	}
	if len(cfg.Pools) != 0 || cfg.Daemon.DefaultRepo != "example/widgets" {
		t.Errorf("pools %d, default_repo %q", len(cfg.Pools), cfg.Daemon.DefaultRepo)
	}
	for _, want := range []string{"config is valid", "  bin/magnum doctor ", "  bin/magnum daemon ", "  bin/magnum review example/widgets#<N> --wait ", "make install"} {
		if !strings.Contains(it.out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, it.out.String())
		}
	}
	if strings.Contains(it.out.String(), ".mise.local.toml") {
		t.Errorf("no App, no key line:\n%s", it.out.String())
	}

	// A second run refuses to overwrite; --force keeps the old file.
	if code := it.run(t, "\nexample/other\n\n"); code != 1 || !strings.Contains(it.err.String(), "--force") {
		t.Fatalf("overwrite without --force: code %d %s", code, it.err.String())
	}
	if code := it.run(t, "\nexample/other\n\n", "--force"); code != 0 {
		t.Fatalf("--force: code %d %s", code, it.err.String())
	}
	if bak, err := os.ReadFile(it.local() + ".bak"); err != nil || string(bak) != string(b) {
		t.Fatalf("the old file was not kept: %v", err)
	}
	if now, _ := os.ReadFile(it.local()); !strings.Contains(string(now), `include = ["other"]`) {
		t.Fatalf("--force did not write the new answers:\n%s", now)
	}
}

func TestInitAppIdentity(t *testing.T) {
	it := newInitTest(t, "")
	input := strings.Join([]string{
		"",                 // no default from gh: asked again
		"octo cat",         // not a login: asked again
		"@octo-cat",        // "@" is dropped
		"example/widgets",  //
		"3",                // neither 1 nor 2
		"2",                // an App
		"example-reviewer", // "[bot]" is added
		"abc",              // not a number
		"123456",           // app id
		"Iv23liEXAMPLE",    // client id
		"7890",             // installation id
		"",                 // the default key variable
	}, "\n") + "\n"
	if code := it.run(t, input); code != 0 {
		t.Fatalf("code %d\nstdout %s\nstderr %s", code, it.out.String(), it.err.String())
	}
	out := it.out.String()
	for _, want := range []string{"an answer is needed", "a GitHub login", "answer 1 or 2", "a positive number",
		"[MAGNUM_EXAMPLE_REVIEWER_APP_PRIVATE_KEY]",
		"  [env]\n  MAGNUM_EXAMPLE_REVIEWER_APP_PRIVATE_KEY = \"~/.config/magnum/example-reviewer.pem\"",
		"mise exec -- bin/magnum identities check", "mise exec -- bin/magnum review example/widgets#<N> --wait"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	cfg, err := config.Load(it.c.Layout, filepath.Join(it.home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	app := cfg.IdentityByName("example-reviewer-app")
	if len(cfg.Identities) != 2 || app == nil || app.Kind != "app" || app.Login != "example-reviewer[bot]" || app.AppID != 123456 ||
		app.ClientID != "Iv23liEXAMPLE" || app.InstallationID != 7890 || app.PrivateKeyEnv != "MAGNUM_EXAMPLE_REVIEWER_APP_PRIVATE_KEY" {
		t.Fatalf("identities %+v", cfg.Identities)
	}
	if w := cfg.Watches[0]; w.Identity != "example-reviewer-app" || w.PollIdentity != "octo-cat" {
		t.Fatalf("watch posts as %q, polls as %q", w.Identity, w.PollIdentity)
	}
	if b, _ := os.ReadFile(it.local()); strings.Contains(string(b), "BEGIN") {
		t.Fatal("a key reached the config")
	}
}

func TestInitStopsWithoutWriting(t *testing.T) {
	it := newInitTest(t, "octo-cat")
	// Input ends after the login: nothing is written.
	if code := it.run(t, "\n"); code != 1 || !strings.Contains(it.err.String(), "nothing was written") {
		t.Fatalf("code %d %s", code, it.err.String())
	}
	if _, err := os.Stat(it.local()); !os.IsNotExist(err) {
		t.Fatalf("config.local.toml written: %v", err)
	}
	if code := it.run(t, "", "extra"); code != 2 {
		t.Fatalf("argument: code %d", code)
	}
}

func TestInitRenderQuotesAndKeyEnv(t *testing.T) {
	if got := initKeyEnv("my.app-2[bot]"); got != "MAGNUM_MY_APP_2_APP_PRIVATE_KEY" {
		t.Errorf("initKeyEnv = %q", got)
	}
	for in, want := range map[string]string{
		"example/widgets": "example/widgets", "github.com/example/widgets/": "example/widgets",
		"git@github.com:example/widgets.git": "example/widgets", "http://github.com/example/w.x": "example/w.x",
	} {
		if got, err := initCheckRepo(in); err != nil || got != want {
			t.Errorf("initCheckRepo(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"widgets", "a/b/c", "example/wid gets", `example/"x`} {
		if _, err := initCheckRepo(bad); err == nil {
			t.Errorf("initCheckRepo(%q) accepted", bad)
		}
	}
	if _, err := initCheckBot(`x"[bot]`); err == nil {
		t.Error(`a quote in the bot login was accepted`)
	}
}

// config.local.toml.example is what a second machine copies: it must load
// with the committed config.toml as it is.
func TestConfigExampleLoads(t *testing.T) {
	it := newInitTest(t, "")
	ex, err := os.ReadFile(filepath.Join("..", "..", "config.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(it.local(), ex, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(it.c.Layout, filepath.Join(it.home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Identities) != 1 || len(cfg.Watches) != 1 || len(cfg.Pools) != 0 || len(cfg.Warnings()) != 0 {
		t.Fatalf("example: %d identities, %d watches, %d pools, warnings %v", len(cfg.Identities), len(cfg.Watches), len(cfg.Pools), cfg.Warnings())
	}
}
