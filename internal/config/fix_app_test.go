package config

import (
	"slices"
	"strings"
	"testing"
)

// fixerApp is a GitHub App of the fixer's own, narrowed as a fixer App must
// be: two repositories, contents and pull_requests write, its bot's user id.
func fixerApp(name string) Identity {
	return Identity{Name: name, Kind: "app", Login: name + "[bot]", AppID: 7, InstallationID: 8, ClientID: "Iv1.fix",
		PrivateKeyFile: "/keys/" + name + ".pem", TokenRepositories: []string{"talkable", "example-repo"},
		TokenPermissions: map[string]string{"contents": "write", "pull_requests": "write"}, UserID: 4242}
}

// reviewApp is the App a watch posts its reviews as.
func reviewApp() Identity {
	return Identity{Name: "review-app", Kind: "app", Login: "review-app[bot]", AppID: 1, InstallationID: 2, ClientID: "Iv1.x",
		PrivateKeyEnv: "APP_KEY"}
}

// A GitHub App that no watch reviews, polls or approves as may be the
// fixer's identity, as the fix role's own or as a watch's fix_identity:
// the fixer then commits, pushes and replies as that App's bot, not as the
// operator. Before, validateFixRole refused every App.
func TestValidateFixRoleTakesADedicatedApp(t *testing.T) {
	for name, edit := range map[string]func(*Config){
		"the fix role's identity": func(c *Config) { fixRoleOf(c).Identity = "fixer" },
		"a watch's fix_identity":  func(c *Config) { c.Watches[0].FixIdentity = "fixer" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validPipelineConfig()
			cfg.Identities = append(cfg.Identities, fixerApp("fixer"))
			edit(cfg)
			cfg.Normalize()
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate = %v", err)
			}
			if !cfg.DedicatedFixApp("fixer") {
				t.Error("DedicatedFixApp(fixer) = false")
			}
			r, ok := cfg.FixRoleFor(&cfg.Watches[0])
			if !ok || r.Identity != "fixer" {
				t.Fatalf("FixRoleFor = %+v, %v; want identity fixer", r, ok)
			}
			if got := cfg.FixerLogins(&cfg.Watches[0]); !slices.Equal(got, []string{"fixer[bot]"}) {
				t.Errorf("FixerLogins = %v, want the App's login", got)
			}
			cfg.Pipeline.Autofix = AutofixOpen
			if w := fixWarningsAbout(cfg, "claude-fix"); len(w) != 0 {
				t.Errorf("Warnings = %q, want none for a dedicated App", w)
			}
		})
	}
	// A gh fixer has no fixer login of its own: it posts as the author.
	cfg := validPipelineConfig()
	cfg.Normalize()
	if got := cfg.FixerLogins(&cfg.Watches[0]); len(got) != 0 {
		t.Errorf("FixerLogins of a gh fixer = %v, want none", got)
	}
	if cfg.DedicatedFixApp("z") || cfg.DedicatedFixApp("ghost") {
		t.Error("DedicatedFixApp is true for a gh identity or an undeclared one")
	}
}

// The App magnum reviews as can never be the fixer: its replies would read
// as magnum's own, and its rebuttals would count as magnum's. A watch that
// names it as identity, poll_identity or auto_approve_as makes it a review
// App, refused as the fix role's identity and as any watch's fix_identity.
func TestValidateFixRoleRefusesTheReviewApp(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Config)
		want string
	}{
		"the watch's identity": {func(c *Config) {
			c.Watches[0].Identity = "review-app"
			fixRoleOf(c).Identity = "review-app"
		}, "identity review-app is the App watch acme posts its reviews as"},
		"the watch's poll_identity": {func(c *Config) {
			c.Watches[0].PollIdentity = "review-app"
			c.Watches[0].FixIdentity = "review-app"
		}, "identity review-app is the App watch acme polls as"},
		"the watch's auto_approve_as": {func(c *Config) {
			c.Watches[0].AutoApproveAs = "review-app"
			c.Watches[0].FixIdentity = "review-app"
		}, "identity review-app is the App watch acme approves as"},
		"another watch's identity": {func(c *Config) {
			c.Watches = append(c.Watches, Watch{Owner: "other", Include: []string{"*"}, Identity: "review-app", PollIdentity: "z"})
			c.Watches[0].FixIdentity = "review-app"
		}, "identity review-app is the App watch other posts its reviews as"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validPipelineConfig()
			app := reviewApp()
			// Narrowed as a fixer App must be: the refusal is for being the
			// review App, not for a missing key.
			app.TokenRepositories, app.TokenPermissions, app.UserID = []string{"talkable"}, map[string]string{"contents": "write", "pull_requests": "write"}, 9
			cfg.Identities = append(cfg.Identities, app)
			tc.edit(cfg)
			cfg.Normalize()
			wantError(t, cfg.Validate(), tc.want, "Give the fixer a gh identity or an App of its own")
			if cfg.DedicatedFixApp("review-app") {
				t.Error("DedicatedFixApp(review-app) = true")
			}
		})
	}
}

// A fixer App's token is narrowed at its mint: it must name the
// repositories it pushes to and ask for contents and pull_requests write
// and nothing else (metadata read is GitHub's own), and say its bot's user
// id, which the commits' noreply email carries.
func TestAFixerAppMustBeNarrowed(t *testing.T) {
	const want = "identity fixer: a fixer App needs token_repositories, token_permissions (contents and pull_requests write, nothing else) and user_id"
	for name, edit := range map[string]func(*Identity){
		"no repositories":    func(id *Identity) { id.TokenRepositories = nil },
		"no permissions":     func(id *Identity) { id.TokenPermissions = nil },
		"workflows write":    func(id *Identity) { id.TokenPermissions["workflows"] = "write" },
		"contents read":      func(id *Identity) { id.TokenPermissions["contents"] = "read" },
		"pull_requests read": func(id *Identity) { id.TokenPermissions["pull_requests"] = "read" },
		"no pull_requests":   func(id *Identity) { delete(id.TokenPermissions, "pull_requests") },
		"no user_id":         func(id *Identity) { id.UserID = 0 },
	} {
		for where, use := range map[string]func(*Config){
			"role":         func(c *Config) { fixRoleOf(c).Identity = "fixer" },
			"fix_identity": func(c *Config) { c.Watches[0].FixIdentity = "fixer" },
		} {
			t.Run(name+"/"+where, func(t *testing.T) {
				cfg := validPipelineConfig()
				app := fixerApp("fixer")
				edit(&app)
				cfg.Identities = append(cfg.Identities, app)
				use(cfg)
				cfg.Normalize()
				wantError(t, cfg.Validate(), want)
			})
		}
	}
	t.Run("metadata read is allowed", func(t *testing.T) {
		cfg := validPipelineConfig()
		app := fixerApp("fixer")
		app.TokenPermissions["metadata"] = "read"
		cfg.Identities = append(cfg.Identities, app)
		cfg.Watches[0].FixIdentity = "fixer"
		cfg.Normalize()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate = %v", err)
		}
	})
}

// token_repositories, token_permissions and user_id are an App's: a gh
// identity is the operator's own login, whose token magnum never mints.
// Their values are checked wherever they are set.
func TestTokenKeysAreForAppsOnly(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Identity)
		want string
	}{
		"gh token_repositories": {func(id *Identity) { id.Kind, id.TokenRepositories = "gh", []string{"talkable"} },
			"identity x: token_repositories is for kind app"},
		"gh token_permissions": {func(id *Identity) { id.Kind, id.TokenPermissions = "gh", map[string]string{"contents": "write"} },
			"identity x: token_permissions is for kind app"},
		"gh user_id": {func(id *Identity) { id.Kind, id.UserID = "gh", 5 }, "identity x: user_id is for kind app"},
		"a repository with an owner": {func(id *Identity) { id.TokenRepositories = []string{"talkable/talkable"} },
			`identity x: token_repositories entry "talkable/talkable" must be a repository name of the installation's account`},
		"a blank repository": {func(id *Identity) { id.TokenRepositories = []string{""} },
			`identity x: token_repositories entry "" must be a repository name`},
		"too many repositories": {func(id *Identity) { id.TokenRepositories = slices.Repeat([]string{"r"}, 501) },
			"identity x: token_repositories names 501 repositories, at most 500"},
		"a permission that is no name": {func(id *Identity) { id.TokenPermissions = map[string]string{"Pull Requests": "write"} },
			`identity x: token_permissions key "Pull Requests" must be a permission name`},
		"a permission level that is none": {func(id *Identity) { id.TokenPermissions = map[string]string{"contents": "admin"} },
			`identity x: token_permissions contents must be read or write, got "admin"`},
		"a negative user_id": {func(id *Identity) { id.UserID = -1 }, "identity x: user_id must be at least 1"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validPipelineConfig()
			id := Identity{Name: "x", Kind: "app", Login: "x[bot]", AppID: 1, InstallationID: 2, ClientID: "Iv1.x", PrivateKeyEnv: "K"}
			if strings.HasPrefix(name, "gh ") {
				id = Identity{Name: "x", Kind: "gh", Login: "x"}
			}
			tc.edit(&id)
			cfg.Identities = append(cfg.Identities, id)
			wantError(t, cfg.Validate(), tc.want)
		})
	}
}

// fix_identity picks the fixer's identity per watch, over the fix role's
// own: two installations of one App are two [[identity]] blocks, and one
// fix role serves both watches. FixerLogins is the login of each watch's
// fixer App, AllFixerLogins their union. A fix_identity nobody declared is
// an error.
func TestFixIdentityPicksTheFixersIdentityPerWatch(t *testing.T) {
	cfg := validPipelineConfig()
	one, two := fixerApp("fixer-one"), fixerApp("fixer-two")
	two.Login = "fixer-one[bot]" // the same App, another installation
	cfg.Identities = append(cfg.Identities, one, two)
	cfg.Watches = append(cfg.Watches, Watch{Owner: "example", Include: []string{"*"}, Identity: "z", PollIdentity: "z"},
		Watch{Owner: "third", Include: []string{"*"}, Identity: "z", PollIdentity: "z"})
	cfg.Watches[0].FixIdentity, cfg.Watches[1].FixIdentity = "fixer-one", "fixer-two"
	fixRoleOf(cfg).Identity = "me"
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate = %v", err)
	}
	for i, want := range []string{"fixer-one", "fixer-two", "me"} {
		if r, ok := cfg.FixRoleFor(&cfg.Watches[i]); !ok || r.Identity != want {
			t.Errorf("watch %s: FixRoleFor = %+v, %v; want identity %s", cfg.Watches[i].Owner, r, ok, want)
		}
	}
	if got := cfg.FixerLogins(&cfg.Watches[2]); len(got) != 0 {
		t.Errorf("FixerLogins of the watch with a gh fixer = %v", got)
	}
	if got := cfg.AllFixerLogins(); !slices.Equal(got, []string{"fixer-one[bot]"}) {
		t.Errorf("AllFixerLogins = %v, want the App's login once", got)
	}

	cfg.Watches[0].FixIdentity = "ghost"
	wantError(t, cfg.Validate(), `watch acme: unknown fix_identity "ghost"`)
}

// fixRoleOf is cfg's claude-fix role, to edit.
func fixRoleOf(c *Config) *Role {
	for i := range c.Roles {
		if c.Roles[i].Name == RoleClaudeFix {
			return &c.Roles[i]
		}
	}
	panic("no claude-fix")
}
