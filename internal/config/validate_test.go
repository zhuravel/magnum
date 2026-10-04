package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/zhuravel/magnum/prompts"
)

// validateErrs splits Validate's joined error into its parts.
func validateErrs(err error) []error {
	if err == nil {
		return nil
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		return j.Unwrap()
	}
	return []error{err}
}

// wantError fails unless err is non-nil and contains every fragment.
func wantError(t *testing.T, err error, fragments ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want one containing %q", fragments)
	}
	for _, f := range fragments {
		if !strings.Contains(err.Error(), f) {
			t.Fatalf("error %q does not contain %q", err, f)
		}
	}
}

func TestZeroWatchesAndIdentitiesAreValid(t *testing.T) {
	cfg := Defaults()
	cfg.Herdr.Socket = "/tmp/herdr.sock"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("no identity and no watch: %v", err)
	}
	w := cfg.Warnings()
	if len(w) != 2 || !strings.Contains(w[0], "no [[identity]] configured") || !strings.Contains(w[1], "no [[watch]] configured, magnum reviews nothing") {
		t.Fatalf("Warnings = %q", w)
	}

	cfg = validPipelineConfig()
	if w := cfg.Warnings(); len(w) != 0 {
		t.Fatalf("Warnings of a full config = %q", w)
	}
	cfg.Watches = nil
	if err := cfg.Validate(); err != nil {
		t.Fatalf("identity without watch: %v", err)
	}
	if w := cfg.Warnings(); len(w) != 1 || !strings.Contains(w[0], "no [[watch]]") {
		t.Fatalf("Warnings = %q", w)
	}
}

func TestLegacySimplifyIsValidated(t *testing.T) {
	for _, v := range []string{"sometimes", "First", "never ", "off"} {
		_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": minimalConfig + "[claude]\nsimplify = \"" + v + "\"\n"})
		if err == nil || !strings.Contains(err.Error(), "claude.simplify") || !strings.Contains(err.Error(), v) {
			t.Errorf("simplify %q: err = %v, want a claude.simplify error", v, err)
		}
	}
	// The overlay's value is checked too.
	_, err := loadFiles(t, t.TempDir(), map[string]string{
		"config.toml":       minimalConfig,
		"config.local.toml": "[claude]\nsimplify = \"typo\"\n",
	})
	wantError(t, err, "claude.simplify", "typo")

	for v, runs := range map[string]string{"": RunsFirst, "first": RunsFirst, "always": RunsAlways, "never": RunsManual} {
		cfg, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": minimalConfig + "[claude]\nsimplify = \"" + v + "\"\n"})
		if err != nil {
			t.Errorf("simplify %q: %v", v, err)
			continue
		}
		if r, _ := cfg.RoleByNameOrAlias(nil, "simplify"); r.Runs != runs {
			t.Errorf("simplify %q: runs = %q, want %q", v, r.Runs, runs)
		}
	}
}

func TestValidateIdentities(t *testing.T) {
	cases := map[string]struct {
		ids  []Identity
		want string
	}{
		"duplicate name": {[]Identity{{Name: "z", Kind: "gh", Login: "z"}, {Name: "z", Kind: "gh", Login: "other"}}, `identity "z" is declared twice`},
		"blocking event": {[]Identity{{Name: "z", Kind: "gh", Login: "z", BlockingEvent: "BLOCK"}}, "blocking_event must be REQUEST_CHANGES or COMMENT"},
		"blocking lower": {[]Identity{{Name: "z", Kind: "gh", Login: "z", BlockingEvent: "comment"}}, "blocking_event"},
		"no findings":    {[]Identity{{Name: "z", Kind: "gh", Login: "z", NoFindingsEvent: "REQUEST_CHANGES"}}, "no_findings_event must be APPROVE or COMMENT"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validPipelineConfig()
			cfg.Identities = tc.ids
			wantError(t, cfg.Validate(), tc.want)
		})
	}
	for _, ev := range []string{"", "REQUEST_CHANGES", "COMMENT"} {
		cfg := validPipelineConfig()
		cfg.Identities[0].BlockingEvent = ev
		if err := cfg.Validate(); err != nil {
			t.Errorf("blocking_event %q: %v", ev, err)
		}
	}
	// Duplicates across config.toml and the overlay are caught too.
	_, err := loadFiles(t, t.TempDir(), map[string]string{
		"config.toml":       minimalConfig,
		"config.local.toml": "[[identity]]\nname = \"z\"\nkind = \"gh\"\nlogin = \"z2\"\n",
	})
	wantError(t, err, `identity "z" is declared twice`)
}

func TestValidateWatchGlobs(t *testing.T) {
	for _, bad := range []string{"[", "[a-", "a[", `\`, "[]a]", "w-*["} {
		for field, edit := range map[string]func(*Watch){
			"include": func(w *Watch) { w.Include = []string{"ok-*", bad} },
			"exclude": func(w *Watch) { w.Exclude = []string{bad} },
		} {
			cfg := validPipelineConfig()
			edit(&cfg.Watches[0])
			err := cfg.Validate()
			wantError(t, err, "watch acme", field, bad)
		}
	}
	cfg := validPipelineConfig()
	cfg.Watches[0].Include = []string{"app", "web-*", "[a-c]*"}
	cfg.Watches[0].Exclude = []string{"*-docs"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestWatchMatchesIgnoresCase(t *testing.T) {
	w := Watch{Owner: "Acme", Include: []string{"Widget-*", "App"}, Exclude: []string{"*-DOCS"}}
	for _, tc := range []struct {
		owner, name string
		want        bool
	}{
		{"acme", "widget-api", true},
		{"ACME", "WIDGET-API", true},
		{"acme", "app", true},
		{"acme", "APP", true},
		{"acme", "widget-docs", false},
		{"acme", "Widget-Docs", false},
		{"acme", "other", false},
		{"other", "app", false},
	} {
		if got := w.Matches(tc.owner, tc.name); got != tc.want {
			t.Errorf("Matches(%q, %q) = %v, want %v", tc.owner, tc.name, got, tc.want)
		}
	}
	cfg := validPipelineConfig()
	cfg.Watches[0].Include = []string{"Widget"}
	if cfg.WatchFor("ACME/widget") == nil {
		t.Fatal("WatchFor must fold case in the name")
	}
}

func TestValidateQuietHours(t *testing.T) {
	for _, ok := range []string{"", "  ", "01:00-07:00", "22:00-06:00", "00:00-23:59", "1:00-7:05", " 22:00 - 06:00 ", "23:59-00:00"} {
		cfg := validPipelineConfig()
		cfg.Daemon.QuietHours = ok
		if err := cfg.Validate(); err != nil {
			t.Errorf("quiet_hours %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"01:00", "01:00-", "-07:00", "25:00-26:00", "24:00-07:00", "01:60-07:00", "ab-cd", "01:00-01:00",
		"01:00-07:00-08:00", "01:00:00-07:00:00", "1am-7am", "01:00 to 07:00"} {
		cfg := validPipelineConfig()
		cfg.Daemon.QuietHours = bad
		wantError(t, cfg.Validate(), "daemon.quiet_hours", bad)
	}
	_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": "[daemon]\nquiet_hours = \"nonsense\"\n" + minimalConfig})
	wantError(t, err, "daemon.quiet_hours")
}

func TestValidateTemplatesParse(t *testing.T) {
	base := minimalConfig + `
[[role]]
name = "codex-judge"
kind = "codex"
judge = true
`
	cases := map[string]struct {
		files map[string]string
		role  string
		want  []string
	}{
		"prompt": {
			files: map[string]string{"prompts/bad.md": "review {{.URL"},
			role:  "[[role]]\nname = \"bad-review\"\nkind = \"claude\"\nprompt = \"bad.md\"\n",
			want:  []string{"role bad-review", "initial prompt bad.md", "unclosed action"},
		},
		"rereview": {
			files: map[string]string{"prompts/ok.md": "fine", "prompts/worse.md": "{{if}}x{{end}}"},
			role:  "[[role]]\nname = \"bad-review\"\nkind = \"claude\"\nprompt = \"ok.md\"\nrereview = \"worse.md\"\n",
			want:  []string{"role bad-review", "rereview prompt worse.md"},
		},
		"judge prompt": {
			files: map[string]string{"prompts/judge-stop.md": "stop {{end}}"},
			role:  "",
			want:  []string{"role codex-judge", "stop prompt judge-stop.md"},
		},
		"shell command": {
			files: map[string]string{},
			role:  "[[role]]\nname = \"lint\"\nkind = \"shell\"\ncommand = \"golint {{.BaseRef\"\n",
			want:  []string{"role lint", "command", "unclosed action"},
		},
		"shell line template": {
			files: map[string]string{"prompts/lint.sh": "golint {{range}}"},
			role:  "[[role]]\nname = \"lint\"\nkind = \"shell\"\nprompt = \"lint.sh\"\n",
			want:  []string{"role lint", "prompt lint.sh"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tc.files["config.toml"] = base + tc.role
			_, err := loadFiles(t, t.TempDir(), tc.files)
			wantError(t, err, tc.want...)
		})
	}

	// A prompt (or command) with valid template syntax passes, whatever
	// fields it names: only the syntax is checked, nothing is executed.
	cfg := mustLoad(t, map[string]string{
		"prompts/lint.sh":    "golint {{.Anything.AtAll}} {{if .X}}y{{end}}",
		"prompts/mine.md":    "mine {{.Missing}}",
		"prompts/mine-re.md": "again",
		"config.toml": base + `
[[role]]
name = "lint"
kind = "shell"
prompt = "lint.sh"
[[role]]
name = "mine"
kind = "claude"
prompt = "mine.md"
rereview = "mine-re.md"
[[role]]
name = "echo"
kind = "shell"
command = "echo {{.BaseRef}} {{.Nope}}"
`})
	if len(cfg.Roles) != 4 {
		t.Fatalf("roles = %v", roleNames(cfg.Roles))
	}

	// One broken file shared by prompt and rereview is reported once.
	cfg = mustLoad(t, map[string]string{
		"prompts/ok.md": "fine",
		"config.toml":   base + "[[role]]\nname = \"dup\"\nkind = \"claude\"\nprompt = \"ok.md\"\n",
	})
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Roles[len(cfg.Roles)-1].Prompt = "dup.md"
	cfg.Roles[len(cfg.Roles)-1].Rereview = "dup.md"
	if err := os.WriteFile(filepath.Join(cfg.Pipeline.PromptsDir, "dup.md"), []byte("{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := validateErrs(cfg.Validate()); len(got) != 1 {
		t.Fatalf("errors = %v, want exactly one", got)
	}
}

// Every embedded default prompt must pass the template parse check Validate
// applies to the prompts a role names.
func TestEmbeddedPromptsParse(t *testing.T) {
	for _, name := range prompts.Names() {
		text, ok := prompts.Read(name)
		if !ok {
			t.Fatalf("prompts.Read(%q) failed", name)
		}
		if _, err := template.New(name).Option("missingkey=error").Parse(text); err != nil {
			t.Errorf("embedded prompt %s: %v", name, err)
		}
	}
}

func TestAfterNamingUnknownRoleIsRejectedEverywhere(t *testing.T) {
	role := func(cfg *Config, name string) *Role {
		i := slices.IndexFunc(cfg.Roles, func(r Role) bool { return r.Name == name })
		return &cfg.Roles[i]
	}
	// validateAfter checks every declared role, not just the ones a watch
	// runs, so a typo is caught even when the role is outside the watch's set.
	cfg := validPipelineConfig()
	cfg.Watches[0].Roles = []string{"codex-judge", "claude-review"}
	role(cfg, "claude-simplify").After = []string{"nope"}
	wantError(t, cfg.Validate(), "role claude-simplify", `after names unknown role "nope"`)

	// An alias is accepted only once Normalize has rewritten it to the name.
	cfg = validPipelineConfig()
	role(cfg, "claude-simplify").After = []string{"claude"}
	cfg.Normalize()
	if got := role(cfg, "claude-simplify").After; !slices.Equal(got, []string{"claude-review"}) {
		t.Fatalf("after = %v", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	// A known role outside the watch's set is fine (ignored at run time).
	cfg = validPipelineConfig()
	cfg.Watches[0].Roles = []string{"codex-judge", "claude-simplify"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	// Through Load, in config.toml and in the overlay.
	judge := "[[role]]\nname = \"codex-judge\"\nkind = \"codex\"\njudge = true\n"
	_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": minimalConfig + judge +
		"[[role]]\nname = \"claude-review\"\nkind = \"claude\"\nafter = [\"ghost\"]\n"})
	wantError(t, err, `after names unknown role "ghost"`)
	_, err = loadFiles(t, t.TempDir(), map[string]string{
		"config.toml":       minimalConfig,
		"config.local.toml": "[[role]]\nname = \"claude-simplify\"\nafter = [\"claude-review\", \"ghost\"]\n",
	})
	wantError(t, err, `after names unknown role "ghost"`)
}

func TestValidatePools(t *testing.T) {
	pool := func(repo string) Pool {
		return Pool{Repo: repo, MainClone: "/p/big", SlotName: "r{n}", SlotPath: "/p/big.r{n}", Max: 1}
	}
	valid := func() *Config {
		cfg := validPipelineConfig()
		cfg.Pools = []Pool{pool("acme/big")}
		return cfg
	}
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		edit func(*Config)
		want string
	}{
		"duplicate":  {func(c *Config) { c.Pools = append(c.Pools, pool("acme/big")) }, "pool acme/big: configured twice"},
		"fold case":  {func(c *Config) { c.Pools = append(c.Pools, pool("ACME/Big")) }, "pool ACME/Big: configured twice"},
		"abs copy":   {func(c *Config) { c.Pools[0].CopyFiles = []string{"/etc/passwd"} }, `copy_files entry "/etc/passwd" must be relative`},
		"dotdot":     {func(c *Config) { c.Pools[0].CopyFiles = []string{"../other/.env"} }, `copy_files entry "../other/.env" must be relative`},
		"dotdot mid": {func(c *Config) { c.Pools[0].CopyFiles = []string{"a/../../x"} }, "must be relative"},
		"bare dots":  {func(c *Config) { c.Pools[0].CopyFiles = []string{".."} }, "must be relative"},
		"empty":      {func(c *Config) { c.Pools[0].CopyFiles = []string{"a", " "} }, "pool acme/big: empty copy_files entry"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := valid()
			tc.edit(cfg)
			wantError(t, cfg.Validate(), tc.want)
		})
	}
	cfg := valid()
	cfg.Pools[0].CopyFiles = []string{".mise.local.toml", "config/initializers/local.rb", "a/../b"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("relative copy_files: %v", err)
	}
	// Repo copy_files keep the same rules through the shared helper.
	cfg = valid()
	cfg.Repos = []Repo{{Repo: "acme/app", CopyFiles: []string{"../x", ""}}}
	got := validateErrs(cfg.Validate())
	if len(got) != 2 || !strings.Contains(got[0].Error(), "repo acme/app: ") || !strings.Contains(got[1].Error(), "repo acme/app: ") {
		t.Fatalf("repo copy_files errors = %v", got)
	}
}

func TestValidateOKStatus(t *testing.T) {
	role := func(cfg *Config, name string) *Role {
		i := slices.IndexFunc(cfg.Roles, func(r Role) bool { return r.Name == name })
		return &cfg.Roles[i]
	}
	if got := role(Defaults(), "codex-review").OKStatus; !slices.Equal(got, []int{0}) {
		t.Fatalf("built-in codex-review ok_status = %v", got)
	}
	cases := map[string]struct {
		edit func(*Config)
		want string
	}{
		"agent role":     {func(c *Config) { role(c, "claude-review").OKStatus = []int{0} }, "ok_status is for shell roles only"},
		"judge":          {func(c *Config) { role(c, "codex-judge").OKStatus = []int{0, 1} }, "ok_status is for shell roles only"},
		"negative":       {func(c *Config) { role(c, "codex-review").OKStatus = []int{0, -1} }, "ok_status -1 must be 0..255"},
		"too big":        {func(c *Config) { role(c, "codex-review").OKStatus = []int{256} }, "ok_status 256 must be 0..255"},
		"shell exit 256": {func(c *Config) { role(c, "codex-review").OKStatus = []int{1000} }, "0..255"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validPipelineConfig()
			tc.edit(cfg)
			wantError(t, cfg.Validate(), tc.want)
		})
	}
	for _, ok := range [][]int{nil, {0}, {0, 1}, {255}, {2, 0}} {
		cfg := validPipelineConfig()
		role(cfg, "codex-review").OKStatus = ok
		if err := cfg.Validate(); err != nil {
			t.Errorf("ok_status %v: %v", ok, err)
		}
	}
}

func TestRoleStatusOK(t *testing.T) {
	for _, tc := range []struct {
		ok     []int
		status int
		want   bool
	}{
		{nil, 0, true},
		{nil, 1, false},
		{[]int{}, 0, true},
		{[]int{}, 2, false},
		{[]int{0}, 0, true},
		{[]int{0}, 1, false},
		{[]int{0, 1}, 1, true},
		{[]int{1}, 0, false},
		{[]int{1}, 1, true},
		{[]int{0, 1}, 2, false},
	} {
		if got := (Role{OKStatus: tc.ok}).StatusOK(tc.status); got != tc.want {
			t.Errorf("OKStatus %v: StatusOK(%d) = %v, want %v", tc.ok, tc.status, got, tc.want)
		}
	}
}

func TestOKStatusLoadsAndMerges(t *testing.T) {
	// Written out in config.toml and inherited from the built-in role alike.
	cfg := mustLoad(t, map[string]string{"config.toml": minimalConfig})
	cr, _ := cfg.RoleByNameOrAlias(nil, "codex-review")
	if !slices.Equal(cr.OKStatus, []int{0}) {
		t.Fatalf("inherited ok_status = %v", cr.OKStatus)
	}
	// The overlay replaces the list like any other slice field.
	cfg = mustLoad(t, map[string]string{
		"config.toml":       minimalConfig,
		"config.local.toml": "[[role]]\nname = \"codex-review\"\nok_status = [0, 1]\n",
	})
	cr, _ = cfg.RoleByNameOrAlias(nil, "codex-review")
	if !slices.Equal(cr.OKStatus, []int{0, 1}) || !cr.StatusOK(1) || cr.Command == "" {
		t.Fatalf("overlay ok_status = %+v", cr)
	}
	// An explicit empty list normalizes to nil (only 0 counts).
	cfg = mustLoad(t, map[string]string{
		"config.toml":       minimalConfig,
		"config.local.toml": "[[role]]\nname = \"codex-review\"\nok_status = []\n",
	})
	cr, _ = cfg.RoleByNameOrAlias(nil, "codex-review")
	if cr.OKStatus != nil || !cr.StatusOK(0) || cr.StatusOK(1) {
		t.Fatalf("empty ok_status = %#v", cr.OKStatus)
	}
	// Other roles are untouched.
	if r, _ := cfg.RoleByNameOrAlias(nil, "claude"); r.OKStatus != nil {
		t.Fatalf("claude-review ok_status = %v", r.OKStatus)
	}
	// An agent role cannot set it.
	_, err := loadFiles(t, t.TempDir(), map[string]string{
		"config.toml":       minimalConfig,
		"config.local.toml": "[[role]]\nname = \"claude-review\"\nok_status = [0]\n",
	})
	wantError(t, err, "role claude-review", "ok_status is for shell roles only")
}

func TestParseDurationDays(t *testing.T) {
	ok := map[string]time.Duration{
		"30s": 30 * time.Second, "2h": 2 * time.Hour, "0": 0,
		"7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour, "0d": 0, "1d12h": 36 * time.Hour, "1d30m": 24*time.Hour + 30*time.Minute,
	}
	for in, want := range ok {
		if got, err := ParseDuration(in); err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"d", "-1d", "1.5d", "1d-1h", "1d+1h", "1dd", "1d2d", "x1d", "1w"} {
		if got, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) = %v, want an error", in, got)
		}
	}
	var d Duration
	if err := d.UnmarshalText([]byte("30d")); err != nil || d.Duration != 30*24*time.Hour {
		t.Fatalf("UnmarshalText 30d = %v, %v", d.Duration, err)
	}
}

func TestValidateRetention(t *testing.T) {
	cfg := Defaults()
	if cfg.Daemon.KeepEvents.Duration != 30*24*time.Hour || cfg.Daemon.KeepRequests.Duration != 7*24*time.Hour {
		t.Fatalf("defaults keep_events %v keep_requests %v", cfg.Daemon.KeepEvents, cfg.Daemon.KeepRequests)
	}
	for _, tc := range []struct {
		events, requests time.Duration
		want             string
	}{
		{0, 0, ""},
		{24 * time.Hour, 90 * 24 * time.Hour, ""},
		{time.Hour, 7 * 24 * time.Hour, "daemon.keep_events must be 0 (keep forever) or at least 1d, got 1h0m0s"},
		{30 * 24 * time.Hour, -time.Hour, "daemon.keep_requests must be 0 (keep forever) or at least 1d"},
	} {
		cfg := validPipelineConfig()
		cfg.Daemon.KeepEvents.Duration, cfg.Daemon.KeepRequests.Duration = tc.events, tc.requests
		err := cfg.Validate()
		if tc.want == "" {
			if err != nil {
				t.Errorf("keep %v/%v: %v", tc.events, tc.requests, err)
			}
			continue
		}
		wantError(t, err, tc.want)
	}
}

func TestValidatePoolDatabaseTemplates(t *testing.T) {
	for tmpl, ok := range map[string]bool{
		"app_development__{slug}": true,
		"app_test__{slug}":        true,
		"app_{slug}":              false, // no "__" before the slug
		"__{slug}":                false, // nothing before it: would match every slot database
		"{slug}__{slug}":          false,
		"app__{slug}_x":           false,
		"app_development":         false,
	} {
		cfg := validPipelineConfig()
		cfg.Pools = []Pool{{Repo: "acme/big", MainClone: "/p/big", SlotName: "r{n}", SlotPath: "/p/big.r{n}", Max: 1,
			Databases: []string{tmpl}}}
		err := cfg.Validate()
		if ok {
			if err != nil {
				t.Errorf("%q: %v", tmpl, err)
			}
			continue
		}
		wantError(t, err, `pool acme/big: database template "`+tmpl+`" must be <name>__{slug}`)
	}
}
