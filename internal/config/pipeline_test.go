package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/paths"
)

const minimalConfig = `
[[identity]]
name = "z"
kind = "gh"
login = "z"
[[identity]]
name = "bot"
kind = "gh"
login = "bot"
[[watch]]
owner = "acme"
include = ["*"]
identity = "z"
`

// loadFiles writes files (relative to home) and loads home/config.toml.
func loadFiles(t *testing.T, home string, files map[string]string) (*Config, error) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(home, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return Load(paths.Layout{Home: home, UserConfig: filepath.Join(home, "user.toml")}, filepath.Join(home, "config.toml"))
}

func mustLoad(t *testing.T, files map[string]string) *Config {
	t.Helper()
	cfg, err := loadFiles(t, t.TempDir(), files)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func roleNames(roles []Role) []string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, r.Name)
	}
	return out
}

// The four [[role]] blocks written out in config.defaults.toml are exactly
// the built-in defaults: loading that file and a config without any
// pipeline keys (same home) yields the same kinds and roles.
func TestDefaultsEqualExplicitConfig(t *testing.T) {
	root := repoRoot(t)
	// NoOverlay: the developer's user config must not leak in.
	explicit, err := LoadWithOptions(paths.Layout{Home: root}, filepath.Join(root, "config.defaults.toml"), LoadOptions{NoOverlay: true})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(minimalConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	implicit, err := LoadWithOptions(paths.Layout{Home: root}, filepath.Join(dir, "config.toml"), LoadOptions{NoOverlay: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(explicit.Roles, implicit.Roles) {
		t.Errorf("roles differ:\nexplicit %+v\nimplicit %+v", explicit.Roles, implicit.Roles)
	}
	if !reflect.DeepEqual(explicit.Kinds, implicit.Kinds) {
		t.Errorf("kinds differ:\nexplicit %+v\nimplicit %+v", explicit.Kinds, implicit.Kinds)
	}
	if explicit.Daemon.JudgeTimeout != implicit.Daemon.JudgeTimeout || explicit.Daemon.ReviewerTimeout != implicit.Daemon.ReviewerTimeout {
		t.Errorf("timeouts differ: %v/%v vs %v/%v", explicit.Daemon.JudgeTimeout, explicit.Daemon.ReviewerTimeout,
			implicit.Daemon.JudgeTimeout, implicit.Daemon.ReviewerTimeout)
	}
	if explicit.Pipeline.PromptsDir != filepath.Join(root, "prompts") {
		t.Errorf("prompts_dir = %q", explicit.Pipeline.PromptsDir)
	}

	// The resolved built-in roles reproduce today's pipeline (the committed
	// file declares no watch, so RolesFor(nil) lists every role).
	var w *Watch
	if got := roleNames(explicit.RolesFor(w)); !slices.Equal(got, []string{"codex-judge", "claude-review", "codex-review", "claude-simplify"}) {
		t.Fatalf("roles = %v", got)
	}
	want := map[string]struct {
		kind, mode, runs, capture, output, initial, rereview, agentKind string
		timeout                                                         time.Duration
	}{
		"codex-judge":     {"codex", "session", "always", "file", "codex-judge.json", "judge-initial.md", "judge-rereview.md", "codex", 90 * time.Minute},
		"claude-review":   {"claude", "session", "always", "file", "claude-review.md", "claude-review.md", "claude-rereview.md", "claude", 40 * time.Minute},
		"codex-review":    {"shell", "shell", "always", "stdout", "codex-review.md", "", "", "codex", 40 * time.Minute},
		"claude-simplify": {"claude", "session", "first", "file", "claude-simplify.md", "claude-simplify.md", "claude-simplify.md", "claude", 40 * time.Minute},
	}
	for _, r := range explicit.Roles {
		x := want[r.Name]
		if r.Kind != x.kind || r.Mode != x.mode || r.Runs != x.runs || r.Capture != x.capture || r.ReportFile() != x.output ||
			r.PromptFile(PromptInitial) != x.initial || r.PromptFile(PromptRereview) != x.rereview ||
			r.AgentKind() != x.agentKind || r.Timeout.Duration != x.timeout {
			t.Errorf("role %s = %+v", r.Name, r)
		}
	}
	j := explicit.JudgeFor(w)
	if j.Name != "codex-judge" || j.Skill != filepath.Join(root, "skills/magnum-review/SKILL.md") || j.Effort != "xhigh" ||
		j.PromptFile(PromptNudge) != "judge-nudge.md" || j.PromptFile(PromptContinue) != "judge-continue.md" {
		t.Fatalf("judge = %+v", j)
	}
	if cr, _ := explicit.RoleByNameOrAlias(w, "codex"); cr.Command != defaultCodexReviewCommand || cr.Args != nil ||
		!slices.Equal(cr.OKStatus, []int{0}) || !cr.StatusOK(0) || cr.StatusOK(1) {
		t.Fatalf("codex-review = %+v", cr)
	}
	if s, _ := explicit.RoleByNameOrAlias(w, "simplify"); s.Runs != RunsFirst {
		t.Fatalf("claude-simplify = %+v", s)
	}
	if r, _ := explicit.RoleByNameOrAlias(w, "claude"); r.Effort != "high" {
		t.Fatalf("claude-review = %+v", r)
	}
	if k, _ := explicit.KindSpec(KindCodex); k.Wrapper != WrapperAuto {
		t.Fatalf("codex kind = %+v", k)
	}
}

func TestDefaultsValidate(t *testing.T) {
	cfg := validPipelineConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := roleNames(Defaults().Roles); !slices.Equal(got, []string{"codex-judge", "claude-review", "codex-review", "claude-simplify"}) {
		t.Fatalf("Defaults roles = %v", got)
	}
	// A Config built without Kinds/Roles falls back to the defaults.
	bare := &Config{}
	if j := bare.JudgeFor(nil); j.Name != RoleCodexJudge || j.Timeout.Duration != 90*time.Minute {
		t.Fatalf("bare judge = %+v", j)
	}
	if k, ok := bare.KindSpec("omp"); !ok || k.Wrapper != WrapperAuto || k.Model[0] != "--model={model}" {
		t.Fatalf("bare omp = %+v %v", k, ok)
	}
}

func validPipelineConfig() *Config {
	cfg := Defaults()
	cfg.Herdr.Socket = "/tmp/herdr.sock"
	cfg.Identities = []Identity{{Name: "z", Kind: "gh", Login: "z"}}
	cfg.Watches = []Watch{{Owner: "acme", Include: []string{"*"}, Identity: "z", PollIdentity: "z"}}
	return cfg
}

func TestValidatePipeline(t *testing.T) {
	role := func(cfg *Config, name string) *Role {
		for i := range cfg.Roles {
			if cfg.Roles[i].Name == name {
				return &cfg.Roles[i]
			}
		}
		panic(name)
	}
	add := func(r Role) func(*Config) { return func(c *Config) { c.Roles = append(c.Roles, r) } }
	cases := map[string]struct {
		edit func(*Config)
		want string
	}{
		"duplicate name":     {add(Role{Name: "claude-review", Kind: "claude"}), `alias "claude-review" is already used`},
		"alias collision":    {add(Role{Name: "x-review", Kind: "claude", Prompt: "claude-review.md", Aliases: []string{"claude"}}), `alias "claude" is already used`},
		"bad name":           {func(c *Config) { role(c, "claude-review").Name = "Claude" }, "name must match"},
		"long name":          {func(c *Config) { role(c, "claude-review").Name = strings.Repeat("a", 25) }, "name must match"},
		"undeclared kind":    {add(Role{Name: "gem", Kind: "gemini", Prompt: "claude-review.md"}), `kind "gemini" is not declared`},
		"shell as session":   {func(c *Config) { role(c, "codex-review").Mode = ModeSession }, "kind shell needs mode shell"},
		"agent as shell":     {func(c *Config) { role(c, "claude-review").Mode = ModeShell }, "needs mode session"},
		"two judges":         {add(Role{Name: "omp-judge", Kind: "omp", Judge: true}), "needs exactly one judge among its roles, has 2"},
		"no judge":           {func(c *Config) { c.Watches[0].Roles = []string{"claude-review"} }, "has 0"},
		"watch unknown role": {func(c *Config) { c.Watches[0].Roles = []string{"judge", "nope"} }, `unknown role "nope"`},
		"watch role twice":   {func(c *Config) { c.Watches[0].Roles = []string{"judge", "codex-judge"} }, "listed twice"},
		"unknown identity":   {func(c *Config) { role(c, "claude-review").Identity = "ghost" }, `unknown identity "ghost"`},
		"explicit prompt":    {func(c *Config) { role(c, "claude-review").Prompt = "missing.md" }, "missing.md"},
		"derived prompt":     {add(Role{Name: "droid-simplify", Kind: "droid"}), "droid-simplify.md"},
		"prompt path":        {func(c *Config) { role(c, "claude-review").Rereview = "../x.md" }, "plain file name"},
		"after unknown":      {func(c *Config) { role(c, "claude-simplify").After = []string{"nope"} }, `unknown role "nope"`},
		"after itself":       {func(c *Config) { role(c, "claude-simplify").After = []string{"claude-simplify"} }, "names itself"},
		"after judge":        {func(c *Config) { role(c, "claude-simplify").After = []string{"codex-judge"} }, "the judge always runs last"},
		"after cycle": {func(c *Config) {
			role(c, "claude-simplify").After = []string{"claude-review"}
			role(c, "claude-review").After = []string{"claude-simplify"}
		}, "cycle"},
		"judge after":        {func(c *Config) { role(c, "codex-judge").After = []string{"claude-review"} }, "a judge takes no after"},
		"judge runs first":   {func(c *Config) { role(c, "codex-judge").Runs = RunsFirst }, "runs = always"},
		"judge stdout":       {func(c *Config) { role(c, "codex-judge").Capture = CaptureStdout }, "shell roles only"},
		"capture git-diff":   {func(c *Config) { role(c, "claude-simplify").Capture = "git-diff" }, `capture must be file or stdout, got "git-diff"`},
		"judge shell":        {add(Role{Name: "sh-judge", Kind: KindShell, Judge: true, Command: "x"}), "not a shell command"},
		"bad runs":           {func(c *Config) { role(c, "claude-review").Runs = "sometimes" }, "runs must be"},
		"stdout on session":  {func(c *Config) { role(c, "claude-review").Capture = CaptureStdout }, "shell roles only"},
		"bad capture":        {func(c *Config) { role(c, "claude-review").Capture = "tmux" }, "capture must be"},
		"shell no command":   {func(c *Config) { role(c, "codex-review").Command = "" }, "needs command"},
		"shell both":         {func(c *Config) { role(c, "codex-review").Prompt = "codex-review.sh" }, "not both"},
		"shell bad tool":     {func(c *Config) { role(c, "codex-review").Tool = "gemini" }, `tool "gemini"`},
		"shell model":        {func(c *Config) { role(c, "codex-review").Model = "o3" }, "agent roles only"},
		"agent command":      {func(c *Config) { role(c, "claude-review").Command = "ls" }, "command is for shell roles only"},
		"agent tool":         {func(c *Config) { role(c, "claude-review").Tool = "codex" }, "tool is for shell roles only"},
		"model without args": {add(Role{Name: "droid-review", Kind: "droid", Prompt: "claude-review.md", Model: "x"}), "has no model args"},
		"skill on reviewer":  {func(c *Config) { role(c, "claude-review").Skill = "/s.md" }, "skill is for judges only"},
		"output path":        {func(c *Config) { role(c, "claude-review").Output = "a/b.md" }, "plain file name"},
		"duplicate output":   {func(c *Config) { role(c, "claude-review").Output = "codex-review.md" }, "already written by"},
		"zero timeout":       {func(c *Config) { role(c, "claude-review").Timeout = Duration{-time.Second} }, "timeout must be positive"},
		"role env key":       {func(c *Config) { role(c, "claude-review").Env = map[string]string{"A-B": "x"} }, `env key "A-B"`},
		"no roles":           {func(c *Config) { c.Roles = []Role{} }, "at least one [[role]]"},
		"kind wrapper":       {func(c *Config) { k := c.Kinds["codex"]; k.Wrapper = "yes"; c.Kinds["codex"] = k }, "wrapper must be"},
		"kind session src":   {func(c *Config) { k := c.Kinds["codex"]; k.SessionSource = "disk"; c.Kinds["codex"] = k }, "session_source"},
		"kind permission":    {func(c *Config) { k := c.Kinds["claude"]; k.OnPermissionPrompt = "approve"; c.Kinds["claude"] = k }, "on_permission_prompt must be deny or wait"},
		"kind resume ph":     {func(c *Config) { k := c.Kinds["codex"]; k.Resume = []string{"resume"}; c.Kinds["codex"] = k }, "must contain {session}"},
		"kind model ph":      {func(c *Config) { k := c.Kinds["omp"]; k.Model = []string{"--model"}; c.Kinds["omp"] = k }, "must contain {model}"},
		"kind rename ph":     {func(c *Config) { k := c.Kinds["codex"]; k.Rename = "/rename"; c.Kinds["codex"] = k }, "must contain {title}"},
		"kind login_ok":      {func(c *Config) { k := c.Kinds["codex"]; k.LoginOK = "exit:0"; c.Kinds["codex"] = k }, "login_ok"},
		"kind login regex":   {func(c *Config) { k := c.Kinds["codex"]; k.LoginOK = "regex:("; c.Kinds["codex"] = k }, "login_ok"},
		"login_ok no check":  {func(c *Config) { k := c.Kinds["droid"]; k.LoginOK = "text:ok"; c.Kinds["droid"] = k }, "needs login_check"},
		"kind health regex": {func(c *Config) {
			k := c.Kinds["codex"]
			k.HealthPatterns.UsageLimit = []string{"("}
			c.Kinds["codex"] = k
		}, "health_patterns.usage_limit"},
		"kind env key":   {func(c *Config) { k := c.Kinds["codex"]; k.Env = map[string]string{"1X": "y"}; c.Kinds["codex"] = k }, `env key "1X"`},
		"kind shell":     {func(c *Config) { c.Kinds["shell"] = Kind{} }, "kinds.shell"},
		"kind bad name":  {func(c *Config) { c.Kinds["Pi"] = Kind{} }, "kinds.Pi"},
		"alias format":   {func(c *Config) { role(c, "claude-review").Aliases = []string{"Claude Code"} }, "alias"},
		"judge on shell": {func(c *Config) { role(c, "codex-review").Judge = true }, "not a shell command"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validPipelineConfig()
			tc.edit(cfg)
			cfg.Normalize()
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestKindMerging(t *testing.T) {
	cfg := mustLoad(t, map[string]string{
		"config.toml": minimalConfig + `
[kinds.claude]
wrapper = "false"
[kinds.codex]
wrapper = "false"
start = ["--search"]
  [kinds.codex.health_patterns]
  usage_limit = ["custom limit"]
  [kinds.codex.env]
  CODEX_HOME = "/x"
[kinds.pi]
resume = ["--session", "{session}"]
login_check = "pi auth"
login_ok = "regex:^ok"
`,
		"user.toml": `
[kinds.codex]
rename = ""
[kinds.droid]
session_source = "none"
`,
	})
	codex, ok := cfg.KindSpec("codex")
	def := DefaultKinds()["codex"]
	if !ok || codex.Wrapper != "false" || !slices.Equal(codex.Start, []string{"--search"}) || !slices.Equal(codex.Args, def.Args) ||
		!slices.Equal(codex.Resume, def.Resume) || !slices.Equal(codex.Effort, def.Effort) || codex.Rename != "" ||
		codex.LoginCheck != "codex login status" || codex.Env["CODEX_HOME"] != "/x" {
		t.Fatalf("codex = %+v", codex)
	}
	if !slices.Equal(codex.HealthPatterns.UsageLimit, []string{"custom limit"}) ||
		!slices.Equal(codex.HealthPatterns.LoginRequired, DefaultHealthPatterns().LoginRequired) {
		t.Fatalf("codex health = %+v", codex.HealthPatterns)
	}
	if claude, _ := cfg.KindSpec("claude"); claude.Wrapper != "false" || claude.Args[0] != "--dangerously-skip-permissions" || claude.Name[0] != "--name" {
		t.Fatalf("claude = %+v", claude)
	}
	pi, ok := cfg.KindSpec("pi")
	if !ok || pi.Wrapper != WrapperAuto || pi.SessionSource != SessionHerdr || pi.Resume[1] != "{session}" ||
		!reflect.DeepEqual(pi.HealthPatterns, DefaultHealthPatterns()) {
		t.Fatalf("pi = %+v", pi)
	}
	if droid, _ := cfg.KindSpec("droid"); droid.SessionSource != SessionNone || droid.Resume[0] != "--resume" {
		t.Fatalf("droid = %+v", droid)
	}
	if _, ok := cfg.KindSpec("shell"); ok {
		t.Fatal("shell is not a kind")
	}
	if got := cfg.KindNames(); !slices.Equal(got, []string{"claude", "codex", "droid", "omp", "pi"}) {
		t.Fatalf("KindNames = %v", got)
	}
	h, err := codex.HealthPatterns.Compile()
	if err != nil || len(h.UsageLimit) != 1 || !h.UsageLimit[0].MatchString("CUSTOM LIMIT hit") {
		t.Fatalf("compile: %v %+v", err, h)
	}
	if env := cfg.RoleEnv(cfg.JudgeFor(nil)); env["CODEX_HOME"] != "/x" {
		t.Fatalf("judge env = %v", env)
	}
	if env := cfg.RoleEnv(Role{Name: "cr", Kind: KindShell, Tool: "codex", Env: map[string]string{"CODEX_HOME": "/y"}}); env["CODEX_HOME"] != "/y" {
		t.Fatalf("role env must win: %v", env)
	}
}

func TestOnPermissionPrompt(t *testing.T) {
	for name, k := range DefaultKinds() {
		if k.OnPermissionPrompt != PermissionDeny {
			t.Errorf("default kinds.%s on_permission_prompt = %q, want deny", name, k.OnPermissionPrompt)
		}
	}
	cfg := mustLoad(t, map[string]string{
		"config.toml": minimalConfig + `
[kinds.claude]
on_permission_prompt = "wait"
[kinds.pi]
resume = ["--session", "{session}"]
`,
		"user.toml": `
[kinds.codex]
on_permission_prompt = " WAIT "
[kinds.claude]
wrapper = "false"
`,
	})
	want := map[string]string{"claude": PermissionWait, "codex": PermissionWait, "droid": PermissionDeny, "omp": PermissionDeny, "pi": PermissionDeny}
	for name, w := range want {
		if k, ok := cfg.KindSpec(name); !ok || k.OnPermissionPrompt != w {
			t.Errorf("kinds.%s on_permission_prompt = %q, want %q", name, k.OnPermissionPrompt, w)
		}
	}
}

func TestAfterDenyPrompt(t *testing.T) {
	for name, k := range DefaultKinds() {
		if k.AfterDenyPrompt != DefaultAfterDenyPrompt {
			t.Errorf("default kinds.%s after_deny_prompt = %q", name, k.AfterDenyPrompt)
		}
	}
	cfg := mustLoad(t, map[string]string{
		"config.toml": minimalConfig + `
[kinds.claude]
after_deny_prompt = """
Denied. Carry on without it.
"""
[kinds.droid]
after_deny_prompt = ""
[kinds.pi]
resume = ["--session", "{session}"]
`,
	})
	want := map[string]string{"claude": "Denied. Carry on without it.", "codex": DefaultAfterDenyPrompt, "droid": "", "pi": DefaultAfterDenyPrompt}
	for name, w := range want {
		if k, ok := cfg.KindSpec(name); !ok || k.AfterDenyPrompt != w {
			t.Errorf("kinds.%s after_deny_prompt = %q, want %q", name, k.AfterDenyPrompt, w)
		}
	}
}

func TestArgv(t *testing.T) {
	k := DefaultKinds()
	if got := k["codex"].Argv(LaunchArgs{Session: "u-1", Title: "PR #1 codex-judge - r", Effort: "xhigh", Wrapper: true}); !slices.Equal(got,
		[]string{"resume", "u-1", "-c", "model_reasoning_effort=xhigh"}) {
		t.Errorf("codex = %q", got)
	}
	if got := k["claude"].Argv(LaunchArgs{Session: "u-2", Title: "PR #1 claude-review - r", Effort: "high", Wrapper: true}); !slices.Equal(got,
		[]string{"--resume", "u-2", "--name", "PR #1 claude-review - r", "--effort", "high"}) {
		t.Errorf("claude = %q", got)
	}
	claude := k["claude"]
	claude.Args, claude.Start = []string{"--bare"}, []string{"--always"}
	if got := claude.Argv(LaunchArgs{Title: "t", Model: "opus", Extra: []string{"--role"}}); !slices.Equal(got,
		[]string{"--name", "t", "--model", "opus", "--always", "--bare", "--role"}) {
		t.Errorf("claude without wrapper = %q", got)
	}
	if got := k["omp"].Argv(LaunchArgs{Session: "s", Model: "opus", Effort: "high", Wrapper: true}); !slices.Equal(got,
		[]string{"--resume=s", "--model=opus", "--thinking=high"}) {
		t.Errorf("omp = %q", got)
	}
	if got := k["droid"].Argv(LaunchArgs{Title: "t", Model: "m", Effort: "e", Wrapper: true}); len(got) != 0 {
		t.Errorf("droid fresh = %q", got)
	}
	if got := k["codex"].RenameCommand("PR #2 codex-judge - r"); got != "/rename PR #2 codex-judge - r" {
		t.Errorf("rename = %q", got)
	}
	if got := k["claude"].RenameCommand("x"); got != "" {
		t.Errorf("claude rename = %q", got)
	}
	if got := k["codex"].LoginArgv(); !slices.Equal(got, []string{"codex", "login", "status"}) {
		t.Errorf("login argv = %q", got)
	}
}

func TestLoggedIn(t *testing.T) {
	k := DefaultKinds()
	for _, tc := range []struct {
		kind           Kind
		stdout, stderr string
		exitOK         bool
		in, readable   bool
	}{
		{k["codex"], "", "Logged in using ChatGPT\n", false, true, true},
		{k["codex"], "Not logged in\n", "", false, false, true},
		{k["claude"], `{"loggedIn": true, "authMethod": "claude.ai"}`, "", true, true, true},
		{k["claude"], "warning\n{\"loggedIn\": false}\n", "", true, false, true},
		{k["claude"], "garbage", "", false, false, false},
		{k["claude"], `{"other": 1}`, "", true, false, false},
		// Only the first JSON object is read, whatever follows it.
		{k["claude"], "{\"loggedIn\": true}\n{\"other\": 1}\n", "", true, true, true},
		{k["claude"], "{\"loggedIn\": false}\n{\"loggedIn\": true}\n", "", true, false, true},
		{k["claude"], "{\"loggedIn\": true}\nsee {docs} for more }", "", true, true, true},
		{k["claude"], "warning: x\n{\"loggedIn\": true, \"nested\": {\"a\": \"}\"}}\ntrailing }", "", true, true, true},
		{k["claude"], "{\"loggedIn\": true", "", true, false, false},
		{Kind{LoginOK: "json:auth.ok"}, `{"auth": {"ok": true}}`, "", true, true, true},
		{Kind{LoginOK: "regex:(?i)^signed in"}, "Signed in as x", "", false, true, true},
		{Kind{}, "", "", true, true, true},
		{Kind{}, "", "", false, false, true},
	} {
		in, readable := tc.kind.LoggedIn(tc.stdout, tc.stderr, tc.exitOK)
		if in != tc.in || readable != tc.readable {
			t.Errorf("LoggedIn(%q via %q) = %v, %v; want %v, %v", tc.stdout+tc.stderr, tc.kind.LoginOK, in, readable, tc.in, tc.readable)
		}
	}
}

func TestRolesForJudgeAndAliases(t *testing.T) {
	cfg := mustLoad(t, map[string]string{
		"prompts/droid-simplify.md":      "simplify {{.URL}}",
		"prompts/omp-review.md":          "review {{.URL}}",
		"prompts/omp-review-rereview.md": "again {{.URL}}",
		"config.toml": `
[[identity]]
name = "z"
kind = "gh"
login = "z"
[[identity]]
name = "bot"
kind = "gh"
login = "bot"
[[watch]]
owner = "acme"
include = ["*"]
identity = "z"
roles = ["judge", "claude"]
[[watch]]
owner = "other"
include = ["*"]
identity = "z"
roles = ["omp-judge", "omp-review", "droid-simplify"]

[[role]]
name = "codex-judge"
kind = "codex"
judge = true
[[role]]
name = "claude-review"
kind = "claude"
model = "opus"
[[role]]
name = "omp-review"
kind = "omp"
model = "gpt-5.2"
effort = "high"
identity = "bot"
aliases = ["omp"]
[[role]]
name = "droid-simplify"
kind = "droid"
runs = "manual"
after = ["omp"]
aliases = ["dsimp"]
[[role]]
name = "omp-judge"
kind = "omp"
judge = true
`,
	})
	a, b := &cfg.Watches[0], &cfg.Watches[1]
	if got := roleNames(cfg.RolesFor(a)); !slices.Equal(got, []string{"codex-judge", "claude-review"}) {
		t.Fatalf("RolesFor(acme) = %v", got)
	}
	if got := roleNames(cfg.RolesFor(b)); !slices.Equal(got, []string{"omp-review", "droid-simplify", "omp-judge"}) {
		t.Fatalf("RolesFor(other) = %v", got)
	}
	if got := roleNames(cfg.RolesFor(nil)); len(got) != 5 {
		t.Fatalf("RolesFor(nil) = %v", got)
	}
	if cfg.JudgeFor(a).Name != "codex-judge" || cfg.JudgeFor(b).Name != "omp-judge" {
		t.Fatalf("judges = %s, %s", cfg.JudgeFor(a).Name, cfg.JudgeFor(b).Name)
	}
	// Built-in names inherit the built-in fields they do not set.
	cr, ok := cfg.RoleByNameOrAlias(a, " Claude ")
	if !ok || cr.Name != "claude-review" || cr.Model != "opus" || cr.Effort != "high" || cr.Rereview != "claude-rereview.md" {
		t.Fatalf("claude-review = %+v %v", cr, ok)
	}
	if j, ok := cfg.RoleByNameOrAlias(a, ""); !ok || j.Name != "codex-judge" || j.Skill == "" || j.Effort != "xhigh" {
		t.Fatalf("judge = %+v %v", j, ok)
	}
	if _, ok := cfg.RoleByNameOrAlias(a, "omp-review"); ok {
		t.Fatal("omp-review is not one of acme's roles")
	}
	ds, ok := cfg.RoleByNameOrAlias(b, "DSIMP")
	if !ok || ds.Output != "droid-simplify.md" || ds.Prompt != "droid-simplify.md" || ds.Rereview != "droid-simplify.md" ||
		!slices.Equal(ds.After, []string{"omp-review"}) {
		t.Fatalf("droid-simplify = %+v", ds)
	}
	if ds.ShouldRun(false, false) || !ds.ShouldRun(true, true) {
		t.Fatal("manual role runs only on request")
	}
	or, _ := cfg.RoleByNameOrAlias(b, "omp-review")
	if or.Rereview != "omp-review-rereview.md" || or.Identity != "bot" || or.Output != "omp-review.md" || or.Timeout.Duration != 40*time.Minute {
		t.Fatalf("omp-review = %+v", or)
	}
	oj := cfg.JudgeFor(b)
	if oj.Prompt != "judge-initial.md" || oj.Output != "omp-judge.json" || oj.Timeout.Duration != 90*time.Minute {
		t.Fatalf("omp-judge = %+v", oj)
	}
	stages := cfg.Stages(b)
	if len(stages) != 3 || roleNames(stages[0])[0] != "omp-review" || roleNames(stages[1])[0] != "droid-simplify" || roleNames(stages[2])[0] != "omp-judge" {
		t.Fatalf("stages = %v", stages)
	}
}

// A user config that pins only claude-simplify's model and effort (as the
// maintainer's does) keeps the built-in read-only role: its report file, no
// after, so it still runs alongside the reviewers.
func TestUserModelAndEffortKeepTheReadOnlyParallelSimplify(t *testing.T) {
	t.Setenv("MAGNUM_CONFIG", "")
	dir := t.TempDir()
	user := filepath.Join(dir, "user.toml")
	if err := os.WriteFile(user, []byte("[[role]]\nname = \"claude-simplify\"\nmodel = \"opus\"\neffort = \"medium\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(paths.Layout{Home: dir, UserConfig: user}, "")
	if err != nil {
		t.Fatal(err)
	}
	s, ok := cfg.RoleByNameOrAlias(nil, "simplify")
	if !ok || s.Model != "opus" || s.Effort != "medium" || s.Capture != CaptureFile || s.ReportFile() != "claude-simplify.md" ||
		len(s.After) != 0 || s.Runs != RunsFirst || s.RerunMinLines != DefaultSimplifyRerunLines || s.Prompt != "claude-simplify.md" {
		t.Fatalf("claude-simplify = %+v", s)
	}
	if first := roleNames(cfg.Stages(nil)[0]); !slices.Contains(first, "claude-simplify") || !slices.Contains(first, "claude-review") {
		t.Fatalf("first stage = %v", first)
	}
}

// The read-only simplify runs alongside the reviewers instead of after them
// (it used to wait for both, about 10 minutes of every first review).
func TestDefaultStagesRunSimplifyAlongsideTheReviewers(t *testing.T) {
	cfg := validPipelineConfig()
	var got [][]string
	for _, s := range cfg.Stages(nil) {
		got = append(got, roleNames(s))
	}
	want := [][]string{{"claude-review", "codex-review", "claude-simplify"}, {"codex-judge"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Stages = %v, want %v", got, want)
	}
	// A dependency outside the watch's set is ignored.
	for i := range cfg.Roles {
		if cfg.Roles[i].Name == "claude-simplify" {
			cfg.Roles[i].After = []string{"claude-review"}
		}
	}
	cfg.Watches[0].Roles = []string{"codex-judge", "claude-simplify"}
	got = nil
	for _, s := range cfg.Stages(&cfg.Watches[0]) {
		got = append(got, roleNames(s))
	}
	if !reflect.DeepEqual(got, [][]string{{"claude-simplify"}, {"codex-judge"}}) {
		t.Fatalf("Stages(subset) = %v", got)
	}
}

func TestShouldRun(t *testing.T) {
	for _, tc := range []struct {
		runs                 string
		ranBefore, requested bool
		want                 bool
	}{
		{RunsAlways, true, false, true},
		{RunsFirst, false, false, true},
		{RunsFirst, true, false, false},
		{RunsFirst, true, true, true},
		{RunsManual, false, false, false},
		{RunsManual, false, true, true},
		{RunsNever, false, true, false},
	} {
		if got := (Role{Runs: tc.runs}).ShouldRun(tc.ranBefore, tc.requested); got != tc.want {
			t.Errorf("%s ShouldRun(%v, %v) = %v", tc.runs, tc.ranBefore, tc.requested, got)
		}
	}
}

// magnum never sent a stop prompt, so no role names one and judge-stop.md is
// gone. A config that still sets a role's stop key loads (an unknown key
// would stop the daemon) and the key is ignored, even when the file it names
// no longer exists.
func TestStopPromptKeyIsAcceptedAndIgnored(t *testing.T) {
	cfg := mustLoad(t, map[string]string{
		"config.toml": minimalConfig,
		"user.toml":   "[[role]]\nname = \"codex-judge\"\nstop = \"judge-stop.md\"\n",
	})
	if slices.Contains(PromptKinds, "stop") {
		t.Fatalf("PromptKinds = %v", PromptKinds)
	}
	if f := cfg.JudgeFor(nil).PromptFile("stop"); f != "" {
		t.Fatalf("the judge names stop prompt %q", f)
	}
	if _, err := cfg.ResolvePrompt("judge-stop.md"); !errors.Is(err, ErrPromptNotFound) {
		t.Fatalf("judge-stop.md: %v, want it gone", err)
	}
	for _, r := range Defaults().Roles {
		if r.Stop != "" {
			t.Errorf("default role %s names stop prompt %q", r.Name, r.Stop)
		}
	}
}

func TestPromptResolution(t *testing.T) {
	home := t.TempDir()
	cfg, err := loadFiles(t, home, map[string]string{
		"config.toml":              minimalConfig,
		"prompts/judge-initial.md": "custom judge {{.URL}}",
		"prompts/extra.md":         "extra",
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := cfg.ResolvePrompt("judge-initial.md")
	if err != nil || p.Embedded || p.Path != filepath.Join(home, "prompts", "judge-initial.md") || p.Text != "custom judge {{.URL}}" {
		t.Fatalf("on disk: %+v %v", p, err)
	}
	p, err = cfg.ResolvePrompt("judge-nudge.md")
	if err != nil || !p.Embedded || p.Path != "" || !strings.Contains(p.Text, "{{.ResultFile}}") {
		t.Fatalf("embedded: %+v %v", p, err)
	}
	if p, err := cfg.ResolvePrompt("extra.md"); err != nil || p.Text != "extra" {
		t.Fatalf("user prompt: %+v %v", p, err)
	}
	if _, err := cfg.ResolvePrompt("nope.md"); !errors.Is(err, ErrPromptNotFound) {
		t.Fatalf("missing: %v", err)
	}
	for _, bad := range []string{"", "..", "../config.toml", "sub/x.md"} {
		if _, err := cfg.ResolvePrompt(bad); err == nil || errors.Is(err, ErrPromptNotFound) {
			t.Errorf("ResolvePrompt(%q) = %v, want a name error", bad, err)
		}
	}
	j := cfg.JudgeFor(nil)
	if p, err := cfg.RolePrompt(j, PromptInitial); err != nil || p.Text != "custom judge {{.URL}}" {
		t.Fatalf("RolePrompt: %+v %v", p, err)
	}
	cr, _ := cfg.RoleByNameOrAlias(nil, "codex-review")
	if _, err := cfg.RolePrompt(cr, PromptInitial); !errors.Is(err, ErrPromptNotFound) {
		t.Fatalf("codex-review has no prompt file: %v", err)
	}

	// prompts_dir can point elsewhere; the overlay may move it too.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "claude-review.md"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadFiles(t, t.TempDir(), map[string]string{
		"config.toml": minimalConfig,
		"user.toml":   "[pipeline]\nprompts_dir = \"" + other + "\"\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p, err := cfg.ResolvePrompt("claude-review.md"); err != nil || p.Text != "mine" || cfg.Pipeline.PromptsDir != other {
		t.Fatalf("overlay prompts_dir: %+v %v", p, err)
	}

	// A relative prompts_dir is relative to magnum's home.
	home = t.TempDir()
	cfg, err = loadFiles(t, home, map[string]string{
		"config.toml":        "[pipeline]\nprompts_dir = \"my-prompts\"\n" + minimalConfig,
		"my-prompts/stop.md": "halt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p, err := cfg.ResolvePrompt("stop.md"); err != nil || p.Path != filepath.Join(home, "my-prompts", "stop.md") {
		t.Fatalf("relative prompts_dir: %+v %v", p, err)
	}
}

// [daemon] reviewer_timeout and judge_timeout are the roles' default
// timeouts; a [[role]] key wins over them.
func TestDaemonTimeoutsMapOntoRoles(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"config.toml": minimalConfig + `
[daemon]
reviewer_timeout = "50m"
judge_timeout = "2h"
`})
	get := func(name string) Role {
		r, ok := cfg.RoleByNameOrAlias(nil, name)
		if !ok {
			t.Fatalf("no role %s", name)
		}
		return r
	}
	if r := get("codex-review"); r.Timeout.Duration != 50*time.Minute {
		t.Fatalf("codex-review = %+v", r)
	}
	if r := get("judge"); r.Timeout.Duration != 2*time.Hour {
		t.Fatalf("judge = %+v", r)
	}

	cfg = mustLoad(t, map[string]string{"config.toml": minimalConfig + `
[[role]]
name = "codex-judge"
kind = "codex"
judge = true
[[role]]
name = "claude-review"
kind = "claude"
effort = "max"
timeout = "10m"
`})
	if r, _ := cfg.RoleByNameOrAlias(nil, "claude"); r.Effort != "max" || r.Timeout.Duration != 10*time.Minute {
		t.Fatalf("claude-review = %+v", r)
	}
	if got := roleNames(cfg.Roles); !slices.Equal(got, []string{"codex-judge", "claude-review"}) {
		t.Fatalf("declared roles replace the built-in list: %v", got)
	}
	if cfg.Daemon.ReviewerTimeout.Duration != 40*time.Minute {
		t.Fatalf("daemon.reviewer_timeout = %v, want the default", cfg.Daemon.ReviewerTimeout)
	}
}

func TestOverlayRoles(t *testing.T) {
	cfg := mustLoad(t, map[string]string{
		"prompts/droid-review.md": "review",
		"config.toml":             minimalConfig,
		"user.toml": `
[[role]]
name = "claude-review"
model = "sonnet"
[[role]]
name = "droid-review"
kind = "droid"
[[watch]]
owner = "solo"
include = ["*"]
identity = "z"
roles = ["judge", "droid-review"]
`,
	})
	if got := roleNames(cfg.Roles); !slices.Equal(got, []string{"codex-judge", "claude-review", "codex-review", "claude-simplify", "droid-review"}) {
		t.Fatalf("roles = %v", got)
	}
	cr, _ := cfg.RoleByNameOrAlias(nil, "claude-review")
	if cr.Model != "sonnet" || cr.Effort != "high" || cr.Prompt != "claude-review.md" {
		t.Fatalf("overlay must merge by key: %+v", cr)
	}
	if got := roleNames(cfg.RolesFor(cfg.WatchFor("solo/x"))); !slices.Equal(got, []string{"codex-judge", "droid-review"}) {
		t.Fatalf("solo roles = %v", got)
	}
}

func TestUnknownPipelineKeys(t *testing.T) {
	for name, body := range map[string]string{
		"role key":  "[[role]]\nname = \"x\"\nkind = \"claude\"\nflavour = \"y\"\n",
		"kind key":  "[kinds.codex]\nresum = [\"resume\"]\n",
		"pipeline":  "[pipeline]\nprompt_dir = \"/x\"\n",
		"health":    "[kinds.codex.health_patterns]\nstalled = [\"x\"]\n",
		"watch key": "[[watch]]\nowner = \"o\"\ninclude = [\"*\"]\nidentity = \"z\"\nrole = [\"x\"]\n",
	} {
		if _, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": minimalConfig + body}); err == nil ||
			!strings.Contains(err.Error(), "unknown keys") {
			t.Errorf("%s: err = %v, want unknown keys", name, err)
		}
	}
}

// A plain codex or claude binary (no zsh wrapper) must run without approval
// prompts like the wrappers do: the built-in args carry the autonomy flag,
// and only without a wrapper, so a wrapper's own flags are never doubled.
func TestDefaultKindsCarryAutonomyArgsWithoutAWrapper(t *testing.T) {
	k := DefaultKinds()
	for kind, flag := range map[string]string{"codex": "--dangerously-bypass-approvals-and-sandbox", "claude": "--dangerously-skip-permissions"} {
		if !slices.Equal(k[kind].Args, []string{flag}) {
			t.Errorf("%s args = %q, want [%s]", kind, k[kind].Args, flag)
		}
		if got := k[kind].Argv(LaunchArgs{}); !slices.Contains(got, flag) {
			t.Errorf("%s without a wrapper = %q, want %s", kind, got, flag)
		}
		if got := k[kind].Argv(LaunchArgs{Wrapper: true}); slices.Contains(got, flag) {
			t.Errorf("%s with a wrapper = %q, must not repeat %s", kind, got, flag)
		}
	}
	for _, kind := range []string{"droid", "omp"} {
		if len(k[kind].Args) != 0 {
			t.Errorf("%s args = %q, want none (no known autonomy flag)", kind, k[kind].Args)
		}
	}
}
