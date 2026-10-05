package config

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum"
	"github.com/zhuravel/magnum/internal/paths"
)

func repoRoot(t *testing.T) string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(f), "..", ".."))
}

// The built-in defaults (config.defaults.toml, embedded) carry no identity,
// watch or pool (those live in the user's config), and that is valid.
func TestLoadBuiltinDefaults(t *testing.T) {
	root := t.TempDir()
	cfg, err := LoadWithOptions(paths.Layout{Home: root}, "", LoadOptions{NoOverlay: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Identities) != 0 || len(cfg.Watches) != 0 || len(cfg.Pools) != 0 || len(cfg.Repos) != 0 {
		t.Fatalf("committed config declares private blocks: %d identities, %d watches, %d pools, %d repos",
			len(cfg.Identities), len(cfg.Watches), len(cfg.Pools), len(cfg.Repos))
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if w := cfg.Warnings(); len(w) != 2 || !strings.Contains(w[0], "[[identity]]") || !strings.Contains(w[1], "[[watch]]") {
		t.Fatalf("warnings = %q", w)
	}
	if got := cfg.JudgeFor(nil).Skill; got != filepath.Join(root, "skills/magnum-review/SKILL.md") {
		t.Fatal(got)
	}
	if cfg.Daemon.PushQuietPeriod.Minutes() != 5 {
		t.Fatal(cfg.Daemon.PushQuietPeriod)
	}
	if len(cfg.Sources) != 1 || cfg.Sources[0] != BuiltinDefaults {
		t.Fatalf("sources %q", cfg.Sources)
	}
}

// The embedded defaults are the repository's config.defaults.toml, byte for
// byte: what a distributed binary runs with is what the checkout documents.
func TestBuiltinDefaultsAreTheCommittedFile(t *testing.T) {
	committed, err := os.ReadFile(filepath.Join(repoRoot(t), "config.defaults.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(committed) != string(magnum.DefaultConfig) {
		t.Fatal("the embedded defaults differ from config.defaults.toml (rebuild)")
	}
}

// The user's ~/.config/magnum/config.toml layers over the built-in defaults;
// without it a legacy config.local.toml in the home still applies, and the
// user config wins over it when both exist.
func TestUserConfigLayersOverTheBuiltinDefaults(t *testing.T) {
	home, cfgDir := t.TempDir(), t.TempDir()
	user := filepath.Join(cfgDir, "magnum", "config.toml")
	if err := os.MkdirAll(filepath.Dir(user), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(p, body string) {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	legacy := strings.Replace(testLocalConfig, `name = "me"`, `name = "legacy-me"`, 1)
	legacy = strings.Replace(legacy, `poll_identity = "me"`, `poll_identity = "legacy-me"`, 1)
	write(filepath.Join(home, "config.local.toml"), legacy)

	cfg, err := Load(paths.Layout{Home: home, UserConfig: user}, "") // the user config does not exist yet
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sources) != 2 || cfg.Sources[0] != BuiltinDefaults || cfg.Sources[1] != filepath.Join(home, "config.local.toml") ||
		cfg.IdentityByName("legacy-me") == nil {
		t.Fatalf("legacy layer: sources %q", cfg.Sources)
	}

	write(user, testLocalConfig)
	cfg, err = Load(paths.Layout{Home: home, UserConfig: user}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sources) != 2 || cfg.Sources[1] != user || cfg.IdentityByName("me") == nil || cfg.IdentityByName("legacy-me") != nil {
		t.Fatalf("user layer: sources %q, identities %+v", cfg.Sources, cfg.Identities)
	}
	if len(cfg.Watches) != 1 || len(cfg.RolesFor(nil)) != 4 {
		t.Fatalf("watches %d, roles %d", len(cfg.Watches), len(cfg.RolesFor(nil)))
	}
}

// A deterministic stand-in for a developer's config.local.toml.
const testLocalConfig = `
[[identity]]
name = "me"
kind = "gh"
login = "me-login"
no_findings_event = "APPROVE"

[[identity]]
name = "reviewer-app"
kind = "app"
login = "reviewer-app[bot]"
app_id = 123456
client_id = "Iv23liEXAMPLE"
installation_id = 12345678
private_key_env = "MAGNUM_TEST_APP_PRIVATE_KEY"
no_findings_event = "COMMENT"
blocking_event = "REQUEST_CHANGES"

[[watch]]
owner = "talkable"
include = ["talkable", "widget-*"]
exclude = ["widget-docs"]
identity = "reviewer-app"
poll_identity = "me"

[[pool]]
repo = "talkable/talkable"
main_clone = "~/Projects/talkable"
slot_name = "review{n}"
slot_path = "~/Projects/talkable.review{n}"
min = 1
max = 3
databases = ["app_development__{slug}", "app_test__{slug}"]
  [pool.env]
  WT_BRANCH = "{slot}"
  WM_HANDLE = ""
`

// loadCommittedWithLocal loads the built-in defaults with local as the
// user's config, with the repository as magnum's home, so {{repo}} expands
// as in production.
func loadCommittedWithLocal(t *testing.T, local string) (*Config, error) {
	t.Helper()
	root := repoRoot(t)
	user := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(user, []byte(local), 0o600); err != nil {
		t.Fatal(err)
	}
	base := ""
	if _, err := os.Stat(filepath.Join(root, "config.toml")); err == nil {
		base = filepath.Join(root, "config.defaults.toml") // a checkout still holding the old config.toml
	}
	return Load(paths.Layout{Home: root, UserConfig: user}, base)
}

// skip_trivial_deltas is commented out in config.defaults.toml, so the
// built-in default list applies, base merges included; a user's own list
// replaces it, so one written before base existed keeps base off.
func TestSkipTrivialDeltasDefaultsToEveryClassButKeepsAnExplicitList(t *testing.T) {
	cfg, err := loadCommittedWithLocal(t, testLocalConfig)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.TrivialDeltas(nil); !slices.Equal(got, []string{"comments", "whitespace", "docs", "base"}) {
		t.Fatalf("default skip_trivial_deltas = %q", got)
	}
	cfg, err = loadCommittedWithLocal(t, testLocalConfig+"\n[daemon]\nskip_trivial_deltas = [\"comments\", \"docs\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.TrivialDeltas(nil); !slices.Equal(got, []string{"comments", "docs"}) {
		t.Fatalf("explicit skip_trivial_deltas = %q", got)
	}
	if _, err := loadCommittedWithLocal(t, testLocalConfig+"\n[daemon]\nskip_trivial_deltas = [\"base\", \"merges\"]\n"); err == nil ||
		!strings.Contains(err.Error(), `"merges" is not one of comments, whitespace, docs, base`) {
		t.Fatalf("an unknown class: %v", err)
	}
}

func TestLoadCommittedConfigWithLocalOverlay(t *testing.T) {
	root := repoRoot(t)
	cfg, err := loadCommittedWithLocal(t, testLocalConfig)
	if err != nil {
		t.Fatal(err)
	}
	if w := cfg.Warnings(); len(w) != 0 {
		t.Fatalf("warnings = %q", w)
	}
	if w := cfg.WatchFor("talkable/talkable"); w == nil || w.Identity != "reviewer-app" || w.PollIdentity != "me" {
		t.Fatalf("watch: %+v", w)
	}
	if cfg.WatchFor("talkable/widget-api") == nil || cfg.WatchFor("talkable/widget-docs") != nil || cfg.WatchFor("talkable/other") != nil {
		t.Fatal("watch include/exclude")
	}
	p := cfg.PoolFor("talkable/talkable")
	if p == nil || p.Slot(3) != "review3" || filepath.Base(p.Path(3)) != "talkable.review3" || len(p.DBNames("review3")) != 2 {
		t.Fatalf("pool: %+v", p)
	}
	if env := p.SlotEnv("review3"); env["WT_BRANCH"] != "review3" || env["WM_HANDLE"] != "" {
		t.Fatalf("env: %v", env)
	}
	if !cfg.IdentityByName("reviewer-app").DismissStale() || cfg.IdentityByName("me").DismissStale() {
		t.Fatal("dismiss defaults")
	}
	if j := cfg.JudgeFor(cfg.WatchFor("talkable/talkable")); j.Name != RoleCodexJudge || j.Skill != filepath.Join(root, "skills/magnum-review/SKILL.md") {
		t.Fatalf("judge = %+v", j)
	}
}

func TestLoadOptionsNoOverlay(t *testing.T) {
	dir := t.TempDir()
	base := `
[[identity]]
name = "me"
kind = "gh"
login = "me"
[[watch]]
owner = "example"
include = ["*"]
identity = "me"
`
	local := `
[[watch]]
owner = "talkable"
include = ["*"]
identity = "me"
`
	for name, body := range map[string]string{"config.toml": base, "config.local.toml": local} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(dir, "config.toml")
	layout := paths.Layout{Home: dir}
	with, err := Load(layout, file)
	if err != nil || len(with.Watches) != 2 {
		t.Fatalf("Load: %v %+v", err, with)
	}
	same, err := LoadWithOptions(layout, file, LoadOptions{})
	if err != nil || !reflect.DeepEqual(with, same) {
		t.Fatalf("LoadWithOptions(zero) differs from Load: %v", err)
	}
	without, err := LoadWithOptions(layout, file, LoadOptions{NoOverlay: true})
	if err != nil || len(without.Watches) != 1 || without.Watches[0].Owner != "example" {
		t.Fatalf("NoOverlay: %v %+v", err, without.Watches)
	}
}

func TestValidateRejectsUnknownIdentity(t *testing.T) {
	cfg := Defaults()
	cfg.Watches = []Watch{{Owner: "x", Include: []string{"*"}, Identity: "nope", PollIdentity: "nope"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error")
	}
}

func TestLocalOverlay(t *testing.T) {
	dir := t.TempDir()
	base := `
[daemon]
poll_interval = "30s"
max_concurrent_reviews = 3
[[identity]]
name = "zhuravel"
kind = "gh"
login = "zhuravel"
[[watch]]
owner = "talkable"
include = ["talkable"]
identity = "zhuravel"
`
	local := `
[daemon]
poll_interval = "45s"
[[watch]]
owner = "zhuravel"
include = ["*"]
identity = "zhuravel"
[github]
transport = "direct"
`
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.local.toml"), []byte(local), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(paths.Layout{Home: dir}, filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Watches) != 2 || cfg.Watches[1].Owner != "zhuravel" {
		t.Fatalf("watches: %+v", cfg.Watches)
	}
	if cfg.Daemon.PollInterval.Seconds() != 45 || cfg.Daemon.MaxConcurrentReviews != 3 {
		t.Fatalf("daemon overlay: %+v", cfg.Daemon)
	}
	if cfg.WatchFor("zhuravel/anything") == nil {
		t.Fatal("overlay watch must match")
	}
	if cfg.GitHub.Transport != "direct" {
		t.Fatalf("github overlay: %+v", cfg.GitHub)
	}
}

func TestTerminalMouse(t *testing.T) {
	if !Defaults().Terminal.Mouse {
		t.Fatal("Defaults().Terminal.Mouse = false, want true")
	}
	load := func(t *testing.T, base, local string) (*Config, error) {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(base), 0o600); err != nil {
			t.Fatal(err)
		}
		if local != "" {
			if err := os.WriteFile(filepath.Join(dir, "config.local.toml"), []byte(local), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return Load(paths.Layout{Home: dir}, filepath.Join(dir, "config.toml"))
	}
	for _, tc := range []struct {
		name, base, local string
		want              bool
	}{
		{"absent keeps the default", "[terminal]\napp = \"Terminal\"\n", "", true},
		{"false in config.toml", "[terminal]\nmouse = false\n", "", false},
		{"true in config.toml", "[terminal]\nmouse = true\n", "", true},
		{"false in the overlay only", "[terminal]\napp = \"Terminal\"\n", "[terminal]\nmouse = false\n", false},
		{"overlay without the key keeps config.toml", "[terminal]\nmouse = false\n", "[terminal]\napp = \"Ghostty\"\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := load(t, tc.base, tc.local)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Terminal.Mouse != tc.want {
				t.Fatalf("Terminal.Mouse = %v, want %v", cfg.Terminal.Mouse, tc.want)
			}
		})
	}
	if _, err := load(t, "[terminal]\nmouse = \"yes\"\n", ""); err == nil || !strings.Contains(err.Error(), "mouse") {
		t.Fatalf("non-boolean mouse: err = %v, want one naming mouse", err)
	}
}

func TestGitHubTransport(t *testing.T) {
	if got := Defaults().GitHub.Transport; got != "gh" {
		t.Fatalf("default transport = %q, want gh", got)
	}
	valid := func(transport string) *Config {
		cfg := Defaults()
		cfg.Herdr.Socket = "/tmp/herdr.sock"
		cfg.Identities = []Identity{{Name: "z", Kind: "gh", Login: "z"}}
		cfg.Watches = []Watch{{Owner: "acme", Include: []string{"*"}, Identity: "z", PollIdentity: "z"}}
		cfg.GitHub.Transport = transport
		return cfg
	}
	for _, ok := range []string{"gh", "direct"} {
		if err := valid(ok).Validate(); err != nil {
			t.Errorf("transport %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "curl", "GH"} {
		if err := valid(bad).Validate(); err == nil {
			t.Errorf("transport %q: expected a validation error", bad)
		}
	}
}

func TestRepoBlocks(t *testing.T) {
	dir := t.TempDir()
	base := `
[[identity]]
name = "zhuravel"
kind = "gh"
login = "zhuravel"
[[watch]]
owner = "talkable"
include = ["*"]
identity = "zhuravel"
[[pool]]
repo = "talkable/talkable"
main_clone = "~/Projects/talkable"
slot_name = "review{n}"
slot_path = "~/Projects/talkable.review{n}"
max = 1
[[repo]]
repo = "talkable/widget"
setup = ["bin/setup"]
teardown = ["bin/teardown"]
copy_files = ["config/master.key"]
strip_env = ["SECRET"]
  [repo.env]
  DATABASE_NAME = "widget_{slug}"
  ROOT = "{path}:{clone}"
`
	local := `
[[repo]]
repo = "talkable/gadget"
wt_hooks = false
`
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.local.toml"), []byte(local), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(paths.Layout{Home: dir}, filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.RepoFor("Talkable/Widget")
	if r == nil || !r.HasCommands() || !r.WTHooksEnabled() || r.Setup[0] != "bin/setup" || r.Teardown[0] != "bin/teardown" ||
		r.CopyFiles[0] != "config/master.key" || r.StripEnv[0] != "SECRET" {
		t.Fatalf("repo = %+v", r)
	}
	if env := r.RenderEnv("magnum-pr-7", "/w", "/c"); env["DATABASE_NAME"] != "widget_magnum-pr-7" || env["ROOT"] != "/w:/c" {
		t.Fatalf("env = %v", env)
	}
	g := cfg.RepoFor("talkable/gadget")
	if g == nil || g.WTHooksEnabled() || g.HasCommands() {
		t.Fatalf("overlay repo = %+v", g)
	}
	if cfg.RepoFor("talkable/other") != nil {
		t.Fatal("unconfigured repo must be nil")
	}
}

func TestValidateRepoBlocks(t *testing.T) {
	valid := func() *Config {
		cfg := Defaults()
		cfg.Herdr.Socket = "/tmp/herdr.sock"
		cfg.Identities = []Identity{{Name: "z", Kind: "gh", Login: "z"}}
		cfg.Watches = []Watch{{Owner: "acme", Include: []string{"*"}, Identity: "z", PollIdentity: "z"}}
		cfg.Pools = []Pool{{Repo: "acme/big", MainClone: "/p/big", SlotName: "r{n}", SlotPath: "/p/big.r{n}", Max: 1}}
		return cfg
	}
	cfg := valid()
	cfg.Repos = []Repo{{Repo: "acme/app", Setup: []string{"bin/setup"}, Env: map[string]string{"A_B": "{slug}"}, CopyFiles: []string{"config/x.yml"},
		NoFindingsEvent: "COMMENT"},
		// A pool repository may have a block carrying only its verdicts.
		{Repo: "acme/big", NoFindingsEvent: "APPROVE", BlockingEvent: "COMMENT"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	for name, r := range map[string][]Repo{
		"no owner":     {{Repo: "app"}},
		"unwatched":    {{Repo: "other/app"}},
		"pool repo":    {{Repo: "acme/big", Setup: []string{"bin/setup"}}},
		"pool env":     {{Repo: "acme/big", Env: map[string]string{"A": "b"}, NoFindingsEvent: "COMMENT"}},
		"pool hooks":   {{Repo: "acme/big", WTHooks: new(false)}},
		"bad verdict":  {{Repo: "acme/app", NoFindingsEvent: "REQUEST_CHANGES"}},
		"bad blocking": {{Repo: "acme/big", BlockingEvent: "APPROVE"}},
		"lower case":   {{Repo: "acme/app", NoFindingsEvent: "comment"}},
		"duplicate":    {{Repo: "acme/app"}, {Repo: "ACME/app"}},
		"bad env key":  {{Repo: "acme/app", Env: map[string]string{"A-B": "x"}}},
		"abs copy":     {{Repo: "acme/app", CopyFiles: []string{"/etc/passwd"}}},
		"escape copy":  {{Repo: "acme/app", CopyFiles: []string{"../other/.env"}}},
		"empty setup":  {{Repo: "acme/app", Setup: []string{" "}}},
		"empty strips": {{Repo: "acme/app", StripEnv: []string{""}}},
	} {
		cfg := valid()
		cfg.Repos = r
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func TestVerdictsFor(t *testing.T) {
	cfg := Defaults()
	cfg.Repos = []Repo{
		{Repo: "talkable/talkable", NoFindingsEvent: "COMMENT", BlockingEvent: "COMMENT"},
		{Repo: "talkable/widget", BlockingEvent: "COMMENT"}, // only one key
		{Repo: "talkable/gadget", Setup: []string{"bin/setup"}},
	}
	gh := &Identity{Name: "me", Kind: "gh", Login: "me"}
	app := &Identity{Name: "bot", Kind: "app", Login: "bot[bot]"}
	appApproves := &Identity{Name: "bot2", Kind: "app", Login: "bot2[bot]", NoFindingsEvent: "APPROVE", BlockingEvent: "COMMENT"}
	cases := []struct {
		repo         string
		id           *Identity
		wantNF, want string
	}{
		{"talkable/talkable", gh, "COMMENT", "COMMENT"},          // the repo overrides both
		{"Talkable/TALKABLE", appApproves, "COMMENT", "COMMENT"}, // case is ignored; repo beats identity
		{"talkable/widget", gh, "APPROVE", "COMMENT"},            // repo blocking, default no-findings
		{"talkable/widget", appApproves, "APPROVE", "COMMENT"},   // identity no-findings
		{"talkable/gadget", gh, "APPROVE", "REQUEST_CHANGES"},    // a block without verdicts
		{"talkable/other", appApproves, "APPROVE", "COMMENT"},    // identity values
		{"talkable/other", gh, "APPROVE", "REQUEST_CHANGES"},     // defaults for gh
		{"talkable/other", app, "COMMENT", "REQUEST_CHANGES"},    // defaults for an app
		{"talkable/other", nil, "COMMENT", "REQUEST_CHANGES"},    // unknown identity: never approve
	}
	for _, tc := range cases {
		nf, be := cfg.VerdictsFor(tc.repo, tc.id)
		if nf != tc.wantNF || be != tc.want {
			name := "<nil>"
			if tc.id != nil {
				name = tc.id.Name
			}
			t.Errorf("VerdictsFor(%s, %s) = %s, %s; want %s, %s", tc.repo, name, nf, be, tc.wantNF, tc.want)
		}
	}
}

// The verdict keys load from a [[repo]] block, also for a pool repository.
func TestLoadRepoVerdicts(t *testing.T) {
	cfg, err := loadCommittedWithLocal(t, testLocalConfig+`
[[repo]]
repo = "talkable/talkable"
no_findings_event = "APPROVE"
blocking_event = "COMMENT"
`)
	if err != nil {
		t.Fatal(err)
	}
	app := cfg.IdentityByName("reviewer-app")
	if nf, be := cfg.VerdictsFor("talkable/talkable", app); nf != "APPROVE" || be != "COMMENT" {
		t.Fatalf("pool repo verdicts = %s, %s", nf, be)
	}
	if nf, be := cfg.VerdictsFor("talkable/widget-api", app); nf != "COMMENT" || be != "REQUEST_CHANGES" {
		t.Fatalf("other repo verdicts = %s, %s", nf, be)
	}
	if _, err := loadCommittedWithLocal(t, testLocalConfig+"\n[[repo]]\nrepo = \"talkable/talkable\"\nblocking_event = \"APPROVE\"\n"); err == nil ||
		!strings.Contains(err.Error(), "blocking_event") {
		t.Fatalf("bad blocking_event: err = %v", err)
	}
}
