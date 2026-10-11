package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/paths"
)

// The fixer is a role class beside the judge: declared like a [[role]], but
// it never takes part in a round, so a watch's roles, the stages and a bare
// RolesFor(nil) leave it out, while FixRoleFor finds it by the pipeline's
// (or the watch's) fix_role key.
func TestAFixRoleStandsOutsideRounds(t *testing.T) {
	cfg := Defaults()
	fix, ok := cfg.FixRoleFor(nil)
	if !ok || fix.Name != RoleClaudeFix || !fix.Fix || !fix.IsFix() || fix.Identity != "" || fix.Skill == "" || fix.MaxSessionRounds != 0 {
		t.Fatalf("FixRoleFor(nil) = %+v, %v", fix, ok)
	}
	if fix.Kind != KindClaude || fix.Model != "opus" || fix.Effort != "high" || fix.Prompt != "fix.md" ||
		fix.ReportFile() != "claude-fix.md" || fix.Capture != CaptureFile || fix.Timeout.Duration.Minutes() != 60 || fix.Runs != "" {
		t.Errorf("claude-fix = %+v", fix)
	}
	for _, r := range cfg.RolesFor(nil) {
		if r.Fix {
			t.Errorf("RolesFor lists fix role %s", r.Name)
		}
	}
	for _, stage := range cfg.Stages(nil) {
		for _, r := range stage {
			if r.Fix {
				t.Errorf("Stages lists fix role %s", r.Name)
			}
		}
	}
	for _, r := range cfg.Judges() {
		if r.Fix {
			t.Errorf("Judges lists fix role %s", r.Name)
		}
	}
	if names := roleNames(cfg.FixRoles()); !slices.Equal(names, []string{RoleClaudeFix}) {
		t.Errorf("FixRoles = %v", names)
	}
	if names := roleNames(cfg.AllRoles()); !slices.Contains(names, RoleClaudeFix) {
		t.Errorf("AllRoles = %v, want it to keep the fix role", names)
	}
	// A watch whose roles list names the fix role still runs none of it, and
	// the stored name of a fix run resolves only without a watch.
	w := &Watch{Owner: "example", Roles: []string{"claude-review", "codex-judge", "claude-fix"}}
	if got := roleNames(cfg.RolesFor(w)); !slices.Equal(got, []string{"codex-judge", "claude-review"}) {
		t.Errorf("RolesFor(roles naming claude-fix) = %v", got)
	}
	if _, ok := cfg.RoleByNameOrAlias(w, "claude-fix"); ok {
		t.Error("RoleByNameOrAlias(watch, claude-fix) found a fix role among the watch's roles")
	}
	if r, ok := cfg.RoleByNameOrAlias(nil, "claude-fix"); !ok || !r.Fix {
		t.Errorf("RoleByNameOrAlias(nil, claude-fix) = %+v, %v", r, ok)
	}
	if cfg.FixCyclesFor(nil) != 3 || cfg.AutofixFor(nil) != AutofixNever {
		t.Errorf("defaults: cycles %d autofix %q", cfg.FixCyclesFor(nil), cfg.AutofixFor(nil))
	}
	if j := cfg.JudgeFor(nil); j.Name != RoleCodexJudge {
		t.Errorf("JudgeFor(nil) = %s", j.Name)
	}
}

// fix_role, fix_cycles and autofix resolve per watch: the watch's value,
// else the pipeline's. A watch's fix_role of "" switches the fix job off for
// it; a fix_role that matches no fix role resolves to none.
func TestFixKeysResolvePerWatch(t *testing.T) {
	cfg := validPipelineConfig()
	cfg.Roles = append(cfg.Roles, Role{Name: "alt-fix", Kind: KindClaude, Fix: true, Identity: "z"})
	cfg.Normalize()

	w := cfg.Watches[0]
	if r, ok := cfg.FixRoleFor(&w); !ok || r.Name != RoleClaudeFix {
		t.Fatalf("a watch without fix_role = %+v, %v; want the pipeline's", r, ok)
	}
	if cfg.FixCyclesFor(&w) != 3 || cfg.AutofixFor(&w) != AutofixNever {
		t.Errorf("a watch without the keys: cycles %d autofix %q", cfg.FixCyclesFor(&w), cfg.AutofixFor(&w))
	}

	w.FixRole, w.FixCycles, w.Autofix = new(" Alt-Fix "), new(5), AutofixOwn
	if r, ok := cfg.FixRoleFor(&w); !ok || r.Name != "alt-fix" {
		t.Errorf("the watch's fix_role (case and space ignored) = %+v, %v", r, ok)
	}
	if cfg.FixCyclesFor(&w) != 5 || cfg.AutofixFor(&w) != AutofixOwn {
		t.Errorf("the watch's keys: cycles %d autofix %q", cfg.FixCyclesFor(&w), cfg.AutofixFor(&w))
	}
	cfg.Watches[0] = w
	if errs := cfg.Validate(); errs != nil {
		t.Fatalf("a watch's valid fix keys fail validation: %v", errs)
	}

	w.FixRole = new("")
	if _, ok := cfg.FixRoleFor(&w); ok {
		t.Error("a watch's empty fix_role still resolved a fix role")
	}
	w.FixRole = new("claude-review")
	if _, ok := cfg.FixRoleFor(&w); ok {
		t.Error("a role that is not a fix role resolved")
	}
	w.FixRole = new("nope")
	if _, ok := cfg.FixRoleFor(&w); ok {
		t.Error("an unknown role resolved")
	}

	cfg.Pipeline.FixRole, cfg.Pipeline.FixCycles, cfg.Pipeline.Autofix = "alt-fix", 7, AutofixOwn
	w = Watch{Owner: "acme"}
	if r, ok := cfg.FixRoleFor(&w); !ok || r.Name != "alt-fix" {
		t.Errorf("[pipeline] fix_role = %+v, %v", r, ok)
	}
	if cfg.FixCyclesFor(&w) != 7 || cfg.FixCyclesFor(nil) != 7 || cfg.AutofixFor(nil) != AutofixOwn {
		t.Errorf("[pipeline] keys: cycles %d/%d autofix %q", cfg.FixCyclesFor(&w), cfg.FixCyclesFor(nil), cfg.AutofixFor(nil))
	}
	cfg.Pipeline.FixRole = ""
	if _, ok := cfg.FixRoleFor(nil); ok {
		t.Error("an empty [pipeline] fix_role resolved a fix role")
	}
	// A configuration built without the keys behaves as the defaults.
	bare := &Config{}
	if bare.FixCyclesFor(nil) != DefaultFixCycles || bare.AutofixFor(nil) != AutofixNever {
		t.Errorf("bare config: cycles %d autofix %q", bare.FixCyclesFor(nil), bare.AutofixFor(nil))
	}
}

// Validation refuses a fix_role that names no role, a judge, a shell role or
// a role that is not a fix role, a fix role a watch's roles list, a fix role
// that is not an agent session of a gh identity, and fix keys out of range.
// A fix role's skill file missing is no error (the snapshot warns, as for a
// judge), and an identity not declared is a warning (Config.Warnings).
func TestValidateFixRole(t *testing.T) {
	role := func(c *Config, name string) *Role {
		for i := range c.Roles {
			if c.Roles[i].Name == name {
				return &c.Roles[i]
			}
		}
		panic(name)
	}
	app := Identity{Name: "app", Kind: "app", Login: "app-bot", AppID: 1, InstallationID: 2, ClientID: "Iv1.x", PrivateKeyEnv: "APP_KEY"}
	cases := map[string]struct {
		edit func(*Config)
		want string
	}{
		"unknown fix_role":     {func(c *Config) { c.Pipeline.FixRole = "nope" }, `pipeline: fix_role names unknown role "nope"`},
		"fix_role is a judge":  {func(c *Config) { c.Pipeline.FixRole = "codex-judge" }, "pipeline: fix_role role codex-judge is a judge"},
		"fix_role is a shell":  {func(c *Config) { c.Pipeline.FixRole = "codex-review" }, "pipeline: fix_role role codex-review must be an agent session"},
		"fix_role is a review": {func(c *Config) { c.Pipeline.FixRole = "claude-review" }, "pipeline: fix_role role claude-review is not a fix role (fix = true)"},
		"watch fix_role unknown": {func(c *Config) {
			c.Watches[0].FixRole = new("nope")
		}, `watch acme: fix_role names unknown role "nope"`},
		"watch fix_role is a judge": {func(c *Config) {
			c.Watches[0].FixRole = new("claude-judge")
		}, "watch acme: fix_role role claude-judge is a judge"},
		"fix role in the watch's roles": {func(c *Config) {
			c.Watches[0].Roles = []string{"codex-judge", "claude-review", "claude-fix"}
		}, "watch acme: role claude-fix is a fix role"},
		"fix role of an App identity": {func(c *Config) {
			c.Identities = append(c.Identities, app)
			role(c, RoleClaudeFix).Identity = "app"
		}, "role claude-fix: identity app must be a gh identity"},
		"fix role runs always": {func(c *Config) { role(c, RoleClaudeFix).Runs = RunsAlways }, "role claude-fix: a fix role has no runs"},
		"fix role runs first":  {func(c *Config) { role(c, RoleClaudeFix).Runs = RunsFirst }, "role claude-fix: a fix role has no runs"},
		"fix role after":       {func(c *Config) { role(c, RoleClaudeFix).After = []string{"claude-review"} }, "role claude-fix: a fix role takes no after"},
		"fix role own_pass":    {func(c *Config) { role(c, RoleClaudeFix).OwnPass = "judge-own-pass.md" }, "role claude-fix: a fix role takes no own_pass"},
		"fix role judge":       {func(c *Config) { role(c, RoleClaudeFix).Judge = true }, "role claude-fix: a fix role cannot also be a judge"},
		"fix role shell": {func(c *Config) {
			r := role(c, RoleClaudeFix)
			r.Kind, r.Mode, r.Model, r.Command, r.Prompt = KindShell, ModeShell, "", "true", ""
		}, "role claude-fix: a fix role must be an agent session"},
		"after names a fix role": {func(c *Config) { role(c, "claude-simplify").After = []string{"claude-fix"} }, "after names fix role claude-fix"},
		"autofix unknown":        {func(c *Config) { c.Pipeline.Autofix = "sometimes" }, `pipeline: autofix must be never or own, got "sometimes"`},
		"watch autofix unknown":  {func(c *Config) { c.Watches[0].Autofix = "sometimes" }, `watch acme: autofix must be never or own, got "sometimes"`},
		"fix_cycles zero":        {func(c *Config) { c.Pipeline.FixCycles = 0 }, "pipeline: fix_cycles must be at least 1, got 0"},
		"watch fix_cycles zero":  {func(c *Config) { c.Watches[0].FixCycles = new(0) }, "watch acme: fix_cycles must be at least 1, got 0"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validPipelineConfig()
			tc.edit(cfg)
			wantError(t, cfg.Validate(), tc.want)
		})
	}

	valid := map[string]func(*Config){
		"the defaults":                    func(*Config) {},
		"a skill file that is missing":    func(c *Config) { role(c, RoleClaudeFix).Skill = "/nonexistent/magnum-fix/SKILL.md" },
		"a watch without a fix job":       func(c *Config) { c.Watches[0].FixRole = new("") },
		"no fix job at all":               func(c *Config) { c.Pipeline.FixRole = "" },
		"no identity (the poll identity)": func(c *Config) { role(c, RoleClaudeFix).Identity = "" },
		"a poll identity that is an App": func(c *Config) {
			c.Identities = append(c.Identities, app)
			c.Watches[0].PollIdentity = "app"
		},
		"an identity that is declared":   func(c *Config) { role(c, RoleClaudeFix).Identity = "z" },
		"an identity that is undeclared": func(c *Config) { role(c, RoleClaudeFix).Identity = "ghost" },
		"autofix own":                    func(c *Config) { c.Pipeline.Autofix = AutofixOwn },
		"a watch's autofix never":        func(c *Config) { c.Watches[0].Autofix = AutofixNever },
		"a second fix role": func(c *Config) {
			c.Roles = append(c.Roles, Role{Name: "alt-fix", Kind: KindClaude, Fix: true, Identity: "z"})
		},
	}
	for name, edit := range valid {
		t.Run("valid/"+name, func(t *testing.T) {
			cfg := validPipelineConfig()
			edit(cfg)
			cfg.Normalize()
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate = %v", err)
			}
		})
	}
}

// fixWarnings is the warnings of cfg that name the fix role.
func fixWarnings(cfg *Config) []string {
	var got []string
	for _, w := range cfg.Warnings() {
		if strings.Contains(w, "claude-fix") {
			got = append(got, w)
		}
	}
	return got
}

// The fixer's replies are told from magnum's own threads by login, so a
// watch that posts as the identity the fix role uses draws a warning naming
// the watch (not an error: the configuration still works), but only where
// autofix = "own" starts the fix job by itself: a configuration that never
// switches autofix on gets none, and `magnum fix` answers for a job asked by
// hand.
func TestValidateFixRoleWarnsWhenThePosterIsTheFixer(t *testing.T) {
	cfg := validPipelineConfig() // watch acme posts as z; claude-fix fixes as me
	fix := func() *Role {
		for i := range cfg.Roles {
			if cfg.Roles[i].Fix {
				return &cfg.Roles[i]
			}
		}
		panic("no fix role")
	}
	fix().Identity = "me"
	if w := cfg.Warnings(); len(w) != 0 {
		t.Fatalf("Warnings of a full config = %q", w)
	}
	fix().Identity = "z" // the identity the watch posts as
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate = %v, want a warning only", err)
	}
	if w := fixWarnings(cfg); len(w) != 0 {
		t.Fatalf("Warnings with autofix never = %q, want none", w)
	}

	cfg.Watches[0].Autofix = AutofixOwn
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate = %v, want a warning only", err)
	}
	got := fixWarnings(cfg)
	if len(got) != 1 || !strings.Contains(got[0], "watch acme") || !strings.Contains(got[0], `identity "z"`) {
		t.Fatalf("Warnings about the fix role = %q, want one naming watch acme and identity z", got)
	}

	// The pipeline's autofix reaches a watch that sets none; a watch's own
	// value wins over it.
	cfg.Watches[0].Autofix = ""
	cfg.Pipeline.Autofix = AutofixOwn
	if got := fixWarnings(cfg); len(got) != 1 {
		t.Errorf("Warnings with [pipeline] autofix = own = %q, want one", got)
	}
	cfg.Watches[0].Autofix = AutofixNever
	if got := fixWarnings(cfg); len(got) != 0 {
		t.Errorf("Warnings with the watch's autofix = never = %q, want none", got)
	}

	// A watch without a fix job has no one to confuse; so has a fix role of
	// another identity.
	cfg.Watches[0].Autofix = AutofixOwn
	cfg.Watches[0].FixRole = new("")
	if w := cfg.Warnings(); len(w) != 0 {
		t.Errorf("Warnings with the fix job off for the watch = %q", w)
	}
	cfg.Watches[0].FixRole = nil
	fix().Identity = "me"
	if w := cfg.Warnings(); len(w) != 0 {
		t.Errorf("Warnings with the fixer on its own identity = %q", w)
	}
}

// An undeclared identity cannot be an error (a fix role may name one the
// configuration declares later), so it is a warning where autofix = "own"
// starts the fix job, once the configuration declares identities: before,
// "no [[identity]] configured" already says it.
func TestAFixRoleWithAnUndeclaredIdentityWarns(t *testing.T) {
	cfg := validPipelineConfig()
	cfg.Identities = cfg.Identities[:1] // z only; claude-fix wants "me"
	for i := range cfg.Roles {
		if cfg.Roles[i].Fix {
			cfg.Roles[i].Identity = "me"
		}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate = %v", err)
	}
	if w := cfg.Warnings(); len(w) != 0 {
		t.Fatalf("Warnings with autofix never = %q, want none", w)
	}

	cfg.Watches[0].Autofix = AutofixOwn
	got := fixWarnings(cfg)
	if len(got) != 1 || !strings.Contains(got[0], `identity "me" is not declared`) {
		t.Fatalf("Warnings = %q, want one about the undeclared identity", got)
	}

	cfg.Watches[0].FixRole = new("")
	if w := cfg.Warnings(); len(w) != 0 {
		t.Errorf("Warnings without a fix job for the watch = %q", w)
	}

	none := Defaults()
	none.Herdr.Socket = "/tmp/herdr.sock"
	none.Pipeline.Autofix = AutofixOwn
	if err := none.Validate(); err != nil {
		t.Fatalf("Validate without identities = %v", err)
	}
	if w := none.Warnings(); len(w) != 2 {
		t.Errorf("Warnings of a configuration without identities = %q, want the two about identities and watches", w)
	}
}

// A fix role that names no identity acts as each watch's poll identity
// (the operator's own gh login in a `magnum init` config, which polls as it
// and may post as an App): FixRoleFor fills it in, and a role's own identity
// wins. Without a watch there is no poll identity to take.
func TestAFixRoleWithoutAnIdentityTakesThePollIdentity(t *testing.T) {
	cfg := validPipelineConfig() // watch acme posts and polls as z
	w := &cfg.Watches[0]
	if r, ok := cfg.FixRoleFor(w); !ok || r.Identity != "z" {
		t.Fatalf("FixRoleFor(acme) = %+v, %v; want identity z, the poll identity", r, ok)
	}
	w.PollIdentity = "me"
	if r, _ := cfg.FixRoleFor(w); r.Identity != "me" {
		t.Errorf("FixRoleFor with poll_identity me = %q", r.Identity)
	}
	if r, _ := cfg.FixRoleFor(&Watch{Owner: "bare", Identity: "z"}); r.Identity != "z" {
		t.Errorf("FixRoleFor of a watch not normalized (no poll_identity) = %q, want its identity", r.Identity)
	}
	if r, _ := cfg.FixRoleFor(nil); r.Identity != "" {
		t.Errorf("FixRoleFor(nil) = %q, want none", r.Identity)
	}
	for i := range cfg.Roles {
		if cfg.Roles[i].Fix {
			cfg.Roles[i].Identity = "z"
		}
	}
	if r, _ := cfg.FixRoleFor(w); r.Identity != "z" {
		t.Errorf("FixRoleFor with the role's own identity z = %q", r.Identity)
	}
}

// initShapedTOML is the user config `magnum init` writes for a login that
// posts as a GitHub App: a gh identity named after the login, the App, and a
// watch that posts as the App and polls as the login.
const initShapedTOML = `
[[identity]]
name = "zhuravel"
kind = "gh"
login = "zhuravel"

[[identity]]
name = "example-reviewer-app"
kind = "app"
login = "example-reviewer[bot]"
app_id = 1
client_id = "Iv1.example"
installation_id = 2
private_key_file = "~/.config/magnum/keys/example-reviewer-app.pem"

[[watch]]
owner = "example"
include = ["widgets"]
identity = "example-reviewer-app"
poll_identity = "zhuravel"
`

// A freshly initialised config runs the fix job without renaming its
// identity: the built-in claude-fix acts as the watch's poll identity, the
// operator's gh login, so neither validation nor the warnings of autofix =
// "own" object, and the fixer is not the App that posts the reviews.
func TestAnInitShapedConfigFixesAsTheLogin(t *testing.T) {
	cfg, _, err := loadUser(t, initShapedTOML)
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	w := &cfg.Watches[0]
	r, ok := cfg.FixRoleFor(w)
	if !ok || r.Name != RoleClaudeFix || r.Identity != "zhuravel" {
		t.Fatalf("FixRoleFor = %+v, %v; want claude-fix as zhuravel", r, ok)
	}
	if id := cfg.IdentityByName(r.Identity); id == nil || id.Kind != "gh" {
		t.Fatalf("the fixer's identity = %+v, want the gh login", id)
	}
	w.Autofix = AutofixOwn
	if got := fixWarnings(cfg); len(got) != 0 {
		t.Errorf("Warnings with autofix = own = %q, want none", got)
	}
}

// Under autofix = "own" the fix job starts by itself, so what would keep it
// from running is said in Warnings: a watch whose poll identity, which the
// fix role takes when it names none, is an App (the job refuses it), a
// watch that posts as the login the fixer acts as (compared by login, not by
// identity name), and a watch whose fix role resolves to none.
func TestAutofixOwnWarnsAboutAFixJobThatCannotRun(t *testing.T) {
	app := Identity{Name: "app", Kind: "app", Login: "app-bot[bot]", AppID: 1, InstallationID: 2, ClientID: "Iv1.x", PrivateKeyEnv: "APP_KEY"}
	cases := map[string]struct {
		edit func(*Config)
		want string // "" = no warning about the fix role
	}{
		"the poll identity is an App": {func(c *Config) {
			c.Identities = append(c.Identities, app)
			c.Watches[0].PollIdentity = "app"
		}, `fix role claude-fix takes watch acme's poll identity "app", an App`},
		"the poster is the fixer's login under another name": {func(c *Config) {
			c.Identities = append(c.Identities, Identity{Name: "me-too", Kind: "gh", Login: "ME"})
			c.Watches[0].Identity, c.Watches[0].PollIdentity = "me-too", "me"
		}, `watch acme posts as login "ME"`},
		"no fix role resolves": {func(c *Config) {
			c.Roles = slices.DeleteFunc(c.AllRoles(), func(r Role) bool { return r.Fix })
		}, `watch acme: autofix = "own" but no fix role resolves for it (fix_role "claude-fix"`},
		"the fix job switched off": {func(c *Config) {
			c.Watches[0].FixRole = new("")
		}, ""},
		"the poll identity is the operator's, posting as another": {func(c *Config) {
			c.Watches[0].PollIdentity = "me"
		}, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validPipelineConfig()
			tc.edit(cfg)
			cfg.Watches[0].Autofix = AutofixOwn
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate = %v, want warnings only", err)
			}
			var got []string
			for _, w := range cfg.Warnings() {
				if strings.Contains(w, "fix") {
					got = append(got, w)
				}
			}
			switch {
			case tc.want == "" && len(got) != 0:
				t.Errorf("Warnings = %q, want none about the fix job", got)
			case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)):
				t.Errorf("Warnings = %q, want one with %q", got, tc.want)
			}
			cfg.Watches[0].Autofix = AutofixNever
			for _, w := range cfg.Warnings() {
				if strings.Contains(w, "fix") {
					t.Errorf("Warnings with autofix = never: %q", w)
				}
			}
		})
	}
}

// The keys read from a user config: a watch that sets fix_role = "" has no
// fix job (a pointer to the empty string, not an unset key), the others keep
// the pipeline's.
func TestAWatchSwitchesTheFixJobOffInTOML(t *testing.T) {
	cfg, _, err := loadUser(t, `
[pipeline]
fix_cycles = 4
autofix = "own"

[[identity]]
name = "z"
kind = "gh"
login = "z"

[[identity]]
name = "me"
kind = "gh"
login = "me"

[[watch]]
owner = "quiet"
include = ["*"]
identity = "z"
poll_identity = "z"
fix_role = ""
fix_cycles = 2
autofix = "never"

[[watch]]
owner = "busy"
include = ["*"]
identity = "z"
poll_identity = "me"
`)
	if err != nil {
		t.Fatal(err)
	}
	quiet, busy := cfg.WatchFor("quiet/x"), cfg.WatchFor("busy/x")
	if _, ok := cfg.FixRoleFor(quiet); ok {
		t.Error("a watch with fix_role = \"\" still has a fix job")
	}
	if cfg.FixCyclesFor(quiet) != 2 || cfg.AutofixFor(quiet) != AutofixNever {
		t.Errorf("quiet: cycles %d autofix %q", cfg.FixCyclesFor(quiet), cfg.AutofixFor(quiet))
	}
	if r, ok := cfg.FixRoleFor(busy); !ok || r.Name != RoleClaudeFix {
		t.Errorf("busy: fix role %+v, %v", r, ok)
	}
	if cfg.FixCyclesFor(busy) != 4 || cfg.AutofixFor(busy) != AutofixOwn {
		t.Errorf("busy: cycles %d autofix %q, want the pipeline's 4 and own", cfg.FixCyclesFor(busy), cfg.AutofixFor(busy))
	}
	if w := cfg.Warnings(); len(w) != 0 {
		t.Errorf("Warnings = %q", w)
	}
}

// A base configuration that declares its own [[role]] list has no claude-fix:
// the pipeline's default fix_role then names nothing, which is no error (it
// has no fix job), while a name that is not the default is.
func TestADefaultFixRoleMayBeMissingFromAReplacedRoleList(t *testing.T) {
	cfg := validPipelineConfig()
	cfg.Roles = slices.DeleteFunc(slices.Clone(cfg.Roles), func(r Role) bool { return r.Fix })
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate = %v", err)
	}
	if _, ok := cfg.FixRoleFor(nil); ok {
		t.Error("a fix role resolved without one declared")
	}
	cfg.Pipeline.FixRole = "claude-fx"
	wantError(t, cfg.Validate(), `pipeline: fix_role names unknown role "claude-fx"`)
	cfg.Pipeline.FixRole = RoleClaudeFix
	cfg.Watches[0].FixRole = new(RoleClaudeFix)
	wantError(t, cfg.Validate(), `watch acme: fix_role names unknown role "claude-fix"`)
}

// fixUserTOML is a user config (layered over the built-in defaults) that
// adds a second fix role.
const fixUserTOML = snapTOML + `
[[role]]
name = "my-fix"
kind = "claude"
fix = true
identity = "z"
`

// loadUser loads the built-in defaults with a user config holding body
// layered over them, in a fresh home.
func loadUser(t *testing.T, body string) (*Config, string, error) {
	t.Helper()
	home := t.TempDir()
	user := filepath.Join(home, "user.toml")
	if err := os.WriteFile(user, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(paths.Layout{Home: home, UserConfig: user}, "")
	return cfg, home, err
}

// Load fills a fix role's skill (expanded, like a judge's), prompt and
// report name; the skill key stays refused on every other role.
func TestAFixRoleGetsTheSkillAndPromptDefaults(t *testing.T) {
	cfg, home, err := loadUser(t, fixUserTOML)
	if err != nil {
		t.Fatal(err)
	}
	wantSkill := filepath.Join(home, "skills", "magnum-fix", "SKILL.md")
	for _, name := range []string{RoleClaudeFix, "my-fix"} {
		r, ok := cfg.RoleByNameOrAlias(nil, name)
		if !ok || !r.Fix {
			t.Fatalf("%s = %+v, %v", name, r, ok)
		}
		wantOut := name + ".md"
		if r.Skill != wantSkill || r.Prompt != "fix.md" || r.ReportFile() != wantOut || r.Runs != "" || r.Capture != CaptureFile {
			t.Errorf("%s: skill %q prompt %q output %q runs %q capture %q; want skill %q, prompt fix.md, output %q",
				name, r.Skill, r.Prompt, r.ReportFile(), r.Runs, r.Capture, wantSkill, wantOut)
		}
	}
	if got := roleNames(cfg.FixRoles()); !slices.Equal(got, []string{RoleClaudeFix, "my-fix"}) {
		t.Errorf("FixRoles = %v", got)
	}
	if got := roleNames(cfg.RolesFor(nil)); slices.Contains(got, "my-fix") || slices.Contains(got, RoleClaudeFix) {
		t.Errorf("RolesFor(nil) = %v lists a fix role", got)
	}
	if DefaultFixSkill != "{{repo}}/skills/magnum-fix/SKILL.md" {
		t.Errorf("DefaultFixSkill = %q", DefaultFixSkill)
	}
	if w := cfg.Warnings(); len(w) != 0 {
		t.Errorf("Warnings = %q, want none (the user config declares z only, and autofix is never)", w)
	}

	_, _, err = loadUser(t, snapTOML+`
[[role]]
name = "x-review"
kind = "claude"
skill = "/x/SKILL.md"
`)
	wantError(t, err, "role x-review: skill is for judges and fix roles only")
}

// The daemon copies a fix role's skill at startup, as a judge's, and warns
// when it cannot read it.
func TestSnapshotCopiesTheFixSkillAndWarnsWhenItIsMissing(t *testing.T) {
	cfg, home := snapLoad(t, map[string]string{
		"skills/magnum-review/SKILL.md": "judge skill",
		"skills/magnum-fix/SKILL.md":    "fix skill",
	})
	skillDir := filepath.Join(t.TempDir(), "copies")
	snap, err := cfg.SnapshotPrompts(skillDir, snapNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Warnings) != 0 {
		t.Fatalf("Warnings = %q", snap.Warnings)
	}
	src := filepath.Join(home, "skills", "magnum-fix", "SKILL.md")
	if got, want := cfg.SkillFile(src), snapCopyPath(skillDir, "fix skill"); got != want {
		t.Fatalf("SkillFile(fix skill) = %q, want %q", got, want)
	}

	cfg, home = snapLoad(t, map[string]string{"skills/magnum-review/SKILL.md": "judge skill"})
	snap, err = cfg.SnapshotPrompts(filepath.Join(t.TempDir(), "copies"), snapNow)
	if err != nil {
		t.Fatalf("a missing fix skill must not fail the snapshot: %v", err)
	}
	src = filepath.Join(home, "skills", "magnum-fix", "SKILL.md")
	if len(snap.Warnings) != 1 || !strings.Contains(snap.Warnings[0], src) || !strings.Contains(snap.Warnings[0], RoleClaudeFix) {
		t.Fatalf("Warnings = %q, want one naming %s and %s", snap.Warnings, RoleClaudeFix, src)
	}
	if got := cfg.SkillFile(src); got != src {
		t.Fatalf("SkillFile(missing fix skill) = %q, want the configured path", got)
	}
}
