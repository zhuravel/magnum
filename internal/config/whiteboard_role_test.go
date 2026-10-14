package config

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/paths"
)

// whiteboardArgs is what the built-in claude-whiteboard passes the claude
// CLI: the Whiteboard MCP server alone, its JSON one element.
var whiteboardArgs = []string{"--mcp-config", `{"mcpServers":{"whiteboard":{"command":"whiteboard","args":["mcp"]}}}`}

// The whiteboard role is a role class beside the fix role: declared like a
// [[role]], but it never takes part in a round, so a watch's roles, the
// stages and a bare RolesFor(nil) leave it out, while WhiteboardRoleFor
// finds it by the pipeline's (or the watch's) whiteboard_role key. The job
// itself is off until [pipeline] whiteboard (or a watch's) switches it on.
func TestAWhiteboardRoleStandsOutsideRounds(t *testing.T) {
	cfg := Defaults()
	wb, ok := cfg.WhiteboardRoleFor(nil)
	if !ok || wb.Name != RoleClaudeWhiteboard || !wb.Whiteboard || !wb.IsWhiteboard() || wb.Fix || wb.IsFix() ||
		wb.Identity != "" || wb.Skill != "" || wb.MaxSessionRounds != 0 {
		t.Fatalf("WhiteboardRoleFor(nil) = %+v, %v", wb, ok)
	}
	if wb.Kind != KindClaude || wb.Model != "opus" || wb.Effort != "medium" || wb.Prompt != "whiteboard.md" ||
		wb.PromptFile(PromptRereview) != "whiteboard.md" || wb.Restart != "" || wb.ReportFile() != "claude-whiteboard.md" ||
		wb.Capture != CaptureFile || wb.Timeout.Duration != 30*time.Minute || wb.Runs != "" || !slices.Equal(wb.Args, whiteboardArgs) {
		t.Errorf("claude-whiteboard = %+v", wb)
	}
	if cfg.WhiteboardFor(nil) {
		t.Error("the whiteboard job is on by default")
	}
	if cfg.Pipeline.WhiteboardRole != RoleClaudeWhiteboard || cfg.WhiteboardRoleName(nil) != RoleClaudeWhiteboard {
		t.Errorf("[pipeline] whiteboard_role = %q, WhiteboardRoleName(nil) = %q", cfg.Pipeline.WhiteboardRole, cfg.WhiteboardRoleName(nil))
	}
	for _, r := range cfg.RolesFor(nil) {
		if r.Whiteboard {
			t.Errorf("RolesFor lists whiteboard role %s", r.Name)
		}
	}
	for _, stage := range cfg.Stages(nil) {
		for _, r := range stage {
			if r.Whiteboard {
				t.Errorf("Stages lists whiteboard role %s", r.Name)
			}
		}
	}
	for _, r := range cfg.Judges() {
		if r.Whiteboard {
			t.Errorf("Judges lists whiteboard role %s", r.Name)
		}
	}
	if names := roleNames(cfg.WhiteboardRoles()); !slices.Equal(names, []string{RoleClaudeWhiteboard}) {
		t.Errorf("WhiteboardRoles = %v", names)
	}
	if names := roleNames(cfg.FixRoles()); slices.Contains(names, RoleClaudeWhiteboard) {
		t.Errorf("FixRoles = %v lists the whiteboard role", names)
	}
	if names := roleNames(cfg.AllRoles()); !slices.Contains(names, RoleClaudeWhiteboard) {
		t.Errorf("AllRoles = %v, want it to keep the whiteboard role", names)
	}
	if names := roleNames(cfg.Roles); !slices.Equal(names[len(names)-2:], []string{RoleClaudeFix, RoleClaudeWhiteboard}) {
		t.Errorf("Defaults roles = %v, want claude-whiteboard right after claude-fix", names)
	}
	// A watch whose roles list names the whiteboard role still runs none of
	// it, and the stored name of a whiteboard run resolves only without a
	// watch.
	w := &Watch{Owner: "example", Roles: []string{"claude-review", "codex-judge", "claude-whiteboard"}}
	if got := roleNames(cfg.RolesFor(w)); !slices.Equal(got, []string{"codex-judge", "claude-review"}) {
		t.Errorf("RolesFor(roles naming claude-whiteboard) = %v", got)
	}
	if _, ok := cfg.RoleByNameOrAlias(w, "claude-whiteboard"); ok {
		t.Error("RoleByNameOrAlias(watch, claude-whiteboard) found a whiteboard role among the watch's roles")
	}
	if r, ok := cfg.RoleByNameOrAlias(nil, "claude-whiteboard"); !ok || !r.Whiteboard {
		t.Errorf("RoleByNameOrAlias(nil, claude-whiteboard) = %+v, %v", r, ok)
	}
	if fix, ok := cfg.FixRoleFor(nil); !ok || fix.Name != RoleClaudeFix {
		t.Errorf("FixRoleFor(nil) = %+v, %v; the fix role must stand", fix, ok)
	}
}

// whiteboard and whiteboard_role resolve per watch: the watch's value when
// it sets one, else the pipeline's. A watch's whiteboard_role of "" switches
// the whiteboard job off for it (by hand and automatic alike); a
// whiteboard_role that matches no whiteboard role resolves to none.
func TestWhiteboardKeysResolvePerWatch(t *testing.T) {
	cfg := validPipelineConfig()
	cfg.Roles = append(cfg.Roles, Role{Name: "alt-whiteboard", Kind: KindClaude, Whiteboard: true, Identity: "z"})
	cfg.Normalize()

	w := cfg.Watches[0]
	if cfg.WhiteboardFor(&w) {
		t.Error("a watch without whiteboard has it on; want the pipeline's false")
	}
	if r, ok := cfg.WhiteboardRoleFor(&w); !ok || r.Name != RoleClaudeWhiteboard {
		t.Fatalf("a watch without whiteboard_role = %+v, %v; want the pipeline's", r, ok)
	}

	w.Whiteboard, w.WhiteboardRole = new(true), new(" Alt-Whiteboard ")
	if !cfg.WhiteboardFor(&w) {
		t.Error("the watch's whiteboard = true did not switch it on")
	}
	if r, ok := cfg.WhiteboardRoleFor(&w); !ok || r.Name != "alt-whiteboard" {
		t.Errorf("the watch's whiteboard_role (case and space ignored) = %+v, %v", r, ok)
	}
	cfg.Watches[0] = w
	if errs := cfg.Validate(); errs != nil {
		t.Fatalf("a watch's valid whiteboard keys fail validation: %v", errs)
	}

	cfg.Pipeline.Whiteboard = true
	w.Whiteboard = new(false)
	if cfg.WhiteboardFor(&w) || !cfg.WhiteboardFor(nil) || !cfg.WhiteboardFor(&Watch{Owner: "other"}) {
		t.Error("the watch's whiteboard = false did not win over the pipeline's true, or the pipeline's true did not reach the others")
	}

	w.WhiteboardRole = new("")
	if _, ok := cfg.WhiteboardRoleFor(&w); ok {
		t.Error("a watch's empty whiteboard_role still resolved a whiteboard role")
	}
	for _, name := range []string{"claude-review", "claude-fix", "codex-judge", "nope"} {
		w.WhiteboardRole = new(name)
		if _, ok := cfg.WhiteboardRoleFor(&w); ok {
			t.Errorf("whiteboard_role %q resolved; it is not a whiteboard role", name)
		}
	}

	cfg.Pipeline.WhiteboardRole = "alt-whiteboard"
	w = Watch{Owner: "acme"}
	if r, ok := cfg.WhiteboardRoleFor(&w); !ok || r.Name != "alt-whiteboard" {
		t.Errorf("[pipeline] whiteboard_role = %+v, %v", r, ok)
	}
	if r, ok := cfg.WhiteboardRoleFor(nil); !ok || r.Name != "alt-whiteboard" || r.Identity != "z" {
		t.Errorf("WhiteboardRoleFor(nil) = %+v, %v", r, ok)
	}
	cfg.Pipeline.WhiteboardRole = ""
	if _, ok := cfg.WhiteboardRoleFor(nil); ok {
		t.Error("an empty [pipeline] whiteboard_role resolved a whiteboard role")
	}
	if cfg.WhiteboardRoleName(nil) != "" || cfg.WhiteboardRoleName(&Watch{Owner: "x", WhiteboardRole: new("y")}) != "y" {
		t.Error("WhiteboardRoleName does not read the watch's key over the pipeline's")
	}
	// A configuration built without the keys has the job off and no role.
	bare := &Config{}
	if bare.WhiteboardFor(nil) {
		t.Error("bare config: whiteboard on")
	}
	if _, ok := bare.WhiteboardRoleFor(nil); ok {
		t.Error("bare config: a whiteboard role resolved")
	}
}

// A whiteboard role that names no identity acts as each watch's poll
// identity (the operator's own gh login in a `magnum init` config, whose
// pane reads the PR as them): WhiteboardRoleFor fills it in, and a role's
// own identity wins. Without a watch there is no poll identity to take.
func TestAWhiteboardRoleWithoutAnIdentityTakesThePollIdentity(t *testing.T) {
	cfg := validPipelineConfig() // watch acme posts and polls as z
	w := &cfg.Watches[0]
	if r, ok := cfg.WhiteboardRoleFor(w); !ok || r.Identity != "z" {
		t.Fatalf("WhiteboardRoleFor(acme) = %+v, %v; want identity z, the poll identity", r, ok)
	}
	w.PollIdentity = "me"
	if r, _ := cfg.WhiteboardRoleFor(w); r.Identity != "me" {
		t.Errorf("WhiteboardRoleFor with poll_identity me = %q", r.Identity)
	}
	if r, _ := cfg.WhiteboardRoleFor(&Watch{Owner: "bare", Identity: "z"}); r.Identity != "z" {
		t.Errorf("WhiteboardRoleFor of a watch not normalized (no poll_identity) = %q, want its identity", r.Identity)
	}
	if r, _ := cfg.WhiteboardRoleFor(nil); r.Identity != "" {
		t.Errorf("WhiteboardRoleFor(nil) = %q, want none", r.Identity)
	}
	for i := range cfg.Roles {
		if cfg.Roles[i].Whiteboard {
			cfg.Roles[i].Identity = "z"
		}
	}
	if r, _ := cfg.WhiteboardRoleFor(w); r.Identity != "z" {
		t.Errorf("WhiteboardRoleFor with the role's own identity z = %q", r.Identity)
	}
}

// Validation refuses a whiteboard_role that names no role, a judge, a shell
// role, a fix role or a role that is not a whiteboard role (at pipeline and
// at watch level), a whiteboard role a watch's roles list, and a whiteboard
// role that is also a judge or a fix role, is a shell command, has after,
// own_pass, max_session_rounds, runs or a skill, or names an App identity.
// An identity not declared is no error (as for a fix role).
func TestValidateWhiteboardRole(t *testing.T) {
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
		"unknown whiteboard_role":       {func(c *Config) { c.Pipeline.WhiteboardRole = "nope" }, `pipeline: whiteboard_role names unknown role "nope"`},
		"whiteboard_role is a judge":    {func(c *Config) { c.Pipeline.WhiteboardRole = "codex-judge" }, "pipeline: whiteboard_role role codex-judge is a judge"},
		"whiteboard_role is a shell":    {func(c *Config) { c.Pipeline.WhiteboardRole = "codex-review" }, "pipeline: whiteboard_role role codex-review must be an agent session"},
		"whiteboard_role is a fix role": {func(c *Config) { c.Pipeline.WhiteboardRole = "claude-fix" }, "pipeline: whiteboard_role role claude-fix is a fix role"},
		"whiteboard_role is a review":   {func(c *Config) { c.Pipeline.WhiteboardRole = "claude-review" }, "pipeline: whiteboard_role role claude-review is not a whiteboard role (whiteboard = true)"},
		"watch whiteboard_role unknown": {func(c *Config) { c.Watches[0].WhiteboardRole = new("nope") }, `watch acme: whiteboard_role names unknown role "nope"`},
		"watch whiteboard_role is a judge": {func(c *Config) {
			c.Watches[0].WhiteboardRole = new("claude-judge")
		}, "watch acme: whiteboard_role role claude-judge is a judge"},
		"watch whiteboard_role is a fix role": {func(c *Config) {
			c.Watches[0].WhiteboardRole = new("claude-fix")
		}, "watch acme: whiteboard_role role claude-fix is a fix role"},
		"watch whiteboard_role is a review": {func(c *Config) {
			c.Watches[0].WhiteboardRole = new("claude-simplify")
		}, "watch acme: whiteboard_role role claude-simplify is not a whiteboard role (whiteboard = true)"},
		"whiteboard role in the watch's roles": {func(c *Config) {
			c.Watches[0].Roles = []string{"codex-judge", "claude-review", "claude-whiteboard"}
		}, "watch acme: role claude-whiteboard is a whiteboard role, which takes no part in a round; whiteboard_role names it, roles must not list it"},
		"whiteboard role of an App identity": {func(c *Config) {
			c.Identities = append(c.Identities, app)
			role(c, RoleClaudeWhiteboard).Identity = "app"
		}, "role claude-whiteboard: identity app must be a gh identity"},
		"whiteboard role runs always":        {func(c *Config) { role(c, RoleClaudeWhiteboard).Runs = RunsAlways }, "role claude-whiteboard: a whiteboard role has no runs"},
		"whiteboard role runs first":         {func(c *Config) { role(c, RoleClaudeWhiteboard).Runs = RunsFirst }, "role claude-whiteboard: a whiteboard role has no runs"},
		"whiteboard role after":              {func(c *Config) { role(c, RoleClaudeWhiteboard).After = []string{"claude-review"} }, "role claude-whiteboard: a whiteboard role takes no after"},
		"whiteboard role own_pass":           {func(c *Config) { role(c, RoleClaudeWhiteboard).OwnPass = "judge-own-pass.md" }, "role claude-whiteboard: a whiteboard role takes no own_pass"},
		"whiteboard role judge":              {func(c *Config) { role(c, RoleClaudeWhiteboard).Judge = true }, "role claude-whiteboard: a whiteboard role cannot also be a judge"},
		"whiteboard role fix":                {func(c *Config) { role(c, RoleClaudeWhiteboard).Fix = true }, "role claude-whiteboard: a whiteboard role cannot also be a fix role"},
		"fix role whiteboard":                {func(c *Config) { role(c, RoleClaudeFix).Whiteboard = true }, "role claude-fix: a whiteboard role cannot also be a fix role"},
		"whiteboard role max_session_rounds": {func(c *Config) { role(c, RoleClaudeWhiteboard).MaxSessionRounds = 4 }, "role claude-whiteboard: a whiteboard role takes no max_session_rounds"},
		"whiteboard role skill":              {func(c *Config) { role(c, RoleClaudeWhiteboard).Skill = "/x/SKILL.md" }, "role claude-whiteboard: skill is for judges and fix roles only"},
		"whiteboard role shell": {func(c *Config) {
			r := role(c, RoleClaudeWhiteboard)
			r.Kind, r.Mode, r.Model, r.Command, r.Prompt, r.Args = KindShell, ModeShell, "", "true", "", nil
		}, "role claude-whiteboard: a whiteboard role must be an agent session"},
		"after names a whiteboard role": {func(c *Config) { role(c, "claude-simplify").After = []string{"claude-whiteboard"} }, "after names whiteboard role claude-whiteboard"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validPipelineConfig()
			tc.edit(cfg)
			wantError(t, cfg.Validate(), tc.want)
		})
	}

	valid := map[string]func(*Config){
		"the defaults":                      func(*Config) {},
		"the job on":                        func(c *Config) { c.Pipeline.Whiteboard = true },
		"a watch that switches the job on":  func(c *Config) { c.Watches[0].Whiteboard = new(true) },
		"a watch that switches the job off": func(c *Config) { c.Pipeline.Whiteboard = true; c.Watches[0].Whiteboard = new(false) },
		"a watch without a whiteboard job":  func(c *Config) { c.Watches[0].WhiteboardRole = new("") },
		"no whiteboard job at all":          func(c *Config) { c.Pipeline.WhiteboardRole = "" },
		"the watch's whiteboard_role":       func(c *Config) { c.Watches[0].WhiteboardRole = new(" Claude-Whiteboard ") },
		"no identity (the poll identity)":   func(c *Config) { role(c, RoleClaudeWhiteboard).Identity = "" },
		"a poll identity that is an App": func(c *Config) {
			c.Identities = append(c.Identities, app)
			c.Watches[0].PollIdentity = "app"
		},
		"an identity that is declared":   func(c *Config) { role(c, RoleClaudeWhiteboard).Identity = "z" },
		"an identity that is undeclared": func(c *Config) { role(c, RoleClaudeWhiteboard).Identity = "ghost" },
		"a second whiteboard role": func(c *Config) {
			c.Roles = append(c.Roles, Role{Name: "alt-whiteboard", Kind: KindClaude, Whiteboard: true, Identity: "z"})
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

// config.defaults.toml, the embedded defaults, declares the whiteboard job
// off, names the built-in claude-whiteboard, and writes that role out as
// DefaultRoles has it: its args carry the Whiteboard MCP server's JSON as
// one element.
func TestTheDefaultsFileDeclaresTheWhiteboardRole(t *testing.T) {
	root := repoRoot(t)
	explicit, err := LoadWithOptions(paths.Layout{Home: root}, filepath.Join(root, "config.defaults.toml"), LoadOptions{NoOverlay: true})
	if err != nil {
		t.Fatal(err)
	}
	if explicit.Pipeline.Whiteboard || explicit.Pipeline.WhiteboardRole != RoleClaudeWhiteboard || explicit.WhiteboardFor(nil) {
		t.Fatalf("config.defaults.toml [pipeline] whiteboard = %v, whiteboard_role = %q", explicit.Pipeline.Whiteboard, explicit.Pipeline.WhiteboardRole)
	}
	r, ok := explicit.WhiteboardRoleFor(nil)
	if !ok || !r.Whiteboard || r.Fix || r.Judge || r.Identity != "" || r.Skill != "" {
		t.Fatalf("WhiteboardRoleFor(nil) = %+v, %v", r, ok)
	}
	if r.Kind != KindClaude || r.Model != "opus" || r.Effort != "medium" || r.Prompt != "whiteboard.md" || r.ReportFile() != "claude-whiteboard.md" ||
		r.Timeout.Duration != 30*time.Minute || r.Runs != "" || r.Capture != CaptureFile || !slices.Equal(r.Args, whiteboardArgs) {
		t.Errorf("config.defaults.toml claude-whiteboard = %+v", r)
	}
	if DefaultWhiteboardPrompt != "whiteboard.md" || RoleClaudeWhiteboard != "claude-whiteboard" {
		t.Errorf("DefaultWhiteboardPrompt = %q, RoleClaudeWhiteboard = %q", DefaultWhiteboardPrompt, RoleClaudeWhiteboard)
	}
}

// Load fills a whiteboard role's prompt and report name and leaves it no
// skill (it has none; the key stays refused on it), no runs and no re-review
// or restart prompt: its one prompt serves every whiteboard run.
func TestAWhiteboardRoleGetsThePromptDefaults(t *testing.T) {
	cfg, _, err := loadUser(t, snapTOML+`
[[role]]
name = "my-whiteboard"
kind = "claude"
whiteboard = true
identity = "z"
`)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{RoleClaudeWhiteboard, "my-whiteboard"} {
		r, ok := cfg.RoleByNameOrAlias(nil, name)
		if !ok || !r.Whiteboard {
			t.Fatalf("%s = %+v, %v", name, r, ok)
		}
		wantOut := name + ".md"
		if r.Skill != "" || r.Prompt != "whiteboard.md" || r.PromptFile(PromptRereview) != "whiteboard.md" || r.Restart != "" ||
			r.ReportFile() != wantOut || r.Runs != "" || r.Capture != CaptureFile || r.Timeout.Duration <= 0 {
			t.Errorf("%s: skill %q prompt %q rereview %q restart %q output %q runs %q capture %q timeout %v; want no skill, prompt whiteboard.md, output %q",
				name, r.Skill, r.Prompt, r.PromptFile(PromptRereview), r.Restart, r.ReportFile(), r.Runs, r.Capture, r.Timeout.Duration, wantOut)
		}
	}
	if got := roleNames(cfg.WhiteboardRoles()); !slices.Equal(got, []string{RoleClaudeWhiteboard, "my-whiteboard"}) {
		t.Errorf("WhiteboardRoles = %v", got)
	}
	if got := roleNames(cfg.RolesFor(nil)); slices.Contains(got, "my-whiteboard") || slices.Contains(got, RoleClaudeWhiteboard) {
		t.Errorf("RolesFor(nil) = %v lists a whiteboard role", got)
	}
	if w := cfg.Warnings(); len(w) != 0 {
		t.Errorf("Warnings = %q, want none", w)
	}

	_, _, err = loadUser(t, snapTOML+`
[[role]]
name = "x-whiteboard"
kind = "claude"
whiteboard = true
skill = "/x/SKILL.md"
`)
	wantError(t, err, "role x-whiteboard: skill is for judges and fix roles only")
}

// A base configuration that declares its own [[role]] list has no
// claude-whiteboard: the pipeline's default whiteboard_role then names
// nothing, which is no error (it has no whiteboard job), while a name that
// is not the default is, and so is the default at watch level.
func TestADefaultWhiteboardRoleMayBeMissingFromAReplacedRoleList(t *testing.T) {
	cfg := validPipelineConfig()
	cfg.Roles = slices.DeleteFunc(slices.Clone(cfg.Roles), func(r Role) bool { return r.Whiteboard })
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate = %v", err)
	}
	if _, ok := cfg.WhiteboardRoleFor(nil); ok {
		t.Error("a whiteboard role resolved without one declared")
	}
	cfg.Pipeline.Whiteboard = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate with the job on and no role = %v, want none", err)
	}
	cfg.Pipeline.WhiteboardRole = "claude-wb"
	wantError(t, cfg.Validate(), `pipeline: whiteboard_role names unknown role "claude-wb"`)
	cfg.Pipeline.WhiteboardRole = RoleClaudeWhiteboard
	cfg.Watches[0].WhiteboardRole = new(RoleClaudeWhiteboard)
	wantError(t, cfg.Validate(), `watch acme: whiteboard_role names unknown role "claude-whiteboard"`)
}

// The keys read from a user config: a watch that sets whiteboard = false or
// whiteboard_role = "" has no whiteboard job (pointers, not unset keys), the
// others keep the pipeline's, with the role acting as each watch's poll
// identity.
func TestAWatchSwitchesTheWhiteboardOffInTOML(t *testing.T) {
	cfg, _, err := loadUser(t, `
[pipeline]
whiteboard = true

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
whiteboard = false

[[watch]]
owner = "unnamed"
include = ["*"]
identity = "z"
poll_identity = "z"
whiteboard_role = ""

[[watch]]
owner = "busy"
include = ["*"]
identity = "z"
poll_identity = "me"
`)
	if err != nil {
		t.Fatal(err)
	}
	quiet, unnamed, busy := cfg.WatchFor("quiet/x"), cfg.WatchFor("unnamed/x"), cfg.WatchFor("busy/x")
	if cfg.WhiteboardFor(quiet) {
		t.Error("a watch with whiteboard = false still has the job on")
	}
	if r, ok := cfg.WhiteboardRoleFor(quiet); !ok || r.Name != RoleClaudeWhiteboard || r.Identity != "z" {
		t.Errorf("quiet: whiteboard role %+v, %v; want the pipeline's as z (the job off, the role still named for the record)", r, ok)
	}
	if !cfg.WhiteboardFor(unnamed) {
		t.Error("a watch with whiteboard_role = \"\" lost the pipeline's whiteboard = true")
	}
	if _, ok := cfg.WhiteboardRoleFor(unnamed); ok {
		t.Error("a watch with whiteboard_role = \"\" still has a whiteboard role")
	}
	if !cfg.WhiteboardFor(busy) {
		t.Error("busy: the pipeline's whiteboard = true did not reach the watch")
	}
	if r, ok := cfg.WhiteboardRoleFor(busy); !ok || r.Name != RoleClaudeWhiteboard || r.Identity != "me" {
		t.Errorf("busy: whiteboard role %+v, %v; want claude-whiteboard as me, the poll identity", r, ok)
	}
	if w := cfg.Warnings(); len(w) != 0 {
		t.Errorf("Warnings = %q", w)
	}
}
