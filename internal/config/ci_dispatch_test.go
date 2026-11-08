package config

import (
	"testing"
)

// A repository whose CI workflow runs only when a PR opens gives a later
// push no run of its required checks. [[repo]] ci_dispatch names the
// workflow file that produces them, which autofix = "open" may start once
// per head on an own PR whose required check did not run; it is off unless
// a [[repo]] block names it (the built-in defaults name none), and
// Config.CIDispatch finds it by the repository's name in any case.
func TestCIDispatchIsOffUnlessARepoNamesItsWorkflow(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"config.toml": minimalConfig + `
[[watch]]
owner = "example"
include = ["*"]
identity = "z"
[[repo]]
repo = "acme/app"
ci_dispatch = "ci.yml"
[[repo]]
repo = "acme/web"
stakes = "Internal tooling."
`})
	for repo, want := range map[string]string{"acme/app": "ci.yml", "Acme/App": "ci.yml", "acme/web": "", "example/site": ""} {
		if got := cfg.CIDispatch(repo); got != want {
			t.Errorf("CIDispatch(%s) = %q, want %q", repo, got, want)
		}
	}
	for _, r := range mustLoadBuiltin(t).Repos {
		if r.CIDispatch != "" {
			t.Errorf("the built-in defaults dispatch %s on %s", r.CIDispatch, r.Repo)
		}
	}
}

// ci_dispatch names a file of .github/workflows: a path, a name without
// .yml or .yaml, or one with ".." fails to load, naming the block.
func TestABadCIDispatchFailsValidation(t *testing.T) {
	for _, bad := range []string{"ci", ".github/workflows/ci.yml", "../ci.yml", "ci..yml", " ci.yml"} {
		cfg := validPipelineConfig()
		cfg.Repos = []Repo{{Repo: "acme/app", CIDispatch: bad}}
		wantError(t, cfg.Validate(), "repo acme/app: ", "ci_dispatch")
	}
	cfg := validPipelineConfig()
	cfg.Repos = []Repo{{Repo: "acme/app", CIDispatch: "ci-main.yaml"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a workflow file: %v", err)
	}
}

// A watch's poll identity may be an App installation that can only read
// Actions, which GitHub refuses a workflow start (HTTP 403). [[repo]]
// ci_dispatch_as names the [[identity]] that starts the ci_dispatch
// workflow instead; Config.CIDispatchAs finds it by the repository's name
// in any case, and "" (unset, or no block) keeps the poll identity.
func TestCIDispatchAsNamesTheIdentityThatStartsTheWorkflow(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"config.toml": minimalConfig + `
[[repo]]
repo = "acme/app"
ci_dispatch = "ci.yml"
ci_dispatch_as = "bot"
[[repo]]
repo = "acme/web"
ci_dispatch = "ci.yml"
`})
	for repo, want := range map[string]string{"acme/app": "bot", "Acme/App": "bot", "acme/web": "", "acme/other": ""} {
		if got := cfg.CIDispatchAs(repo); got != want {
			t.Errorf("CIDispatchAs(%s) = %q, want %q", repo, got, want)
		}
	}
}

// ci_dispatch_as must name a configured [[identity]], and it means nothing
// without ci_dispatch (it names who starts that workflow): either fails to
// load, naming the block and why. It applies to a repository with a
// [[pool]], as ci_dispatch does.
func TestABadCIDispatchAsFailsValidation(t *testing.T) {
	cfg := validPipelineConfig()
	cfg.Repos = []Repo{{Repo: "acme/app", CIDispatch: "ci.yml", CIDispatchAs: "ghost"}}
	wantError(t, cfg.Validate(), "repo acme/app: ", `ci_dispatch_as "ghost"`, "no [[identity]]")

	cfg = validPipelineConfig()
	cfg.Repos = []Repo{{Repo: "acme/app", CIDispatchAs: "me"}}
	wantError(t, cfg.Validate(), "repo acme/app: ", `ci_dispatch_as "me" without ci_dispatch`, "magnum starts no workflow")

	cfg = validPipelineConfig()
	cfg.Pools = []Pool{{Repo: "acme/app", MainClone: "/p/app", SlotName: "r{n}", SlotPath: "/p/app.r{n}", Max: 1}}
	cfg.Repos = []Repo{{Repo: "acme/app", CIDispatch: "ci.yml", CIDispatchAs: "me"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("ci_dispatch_as on a pooled repository: %v", err)
	}
}

// Every token magnum mints for an App that sets token_permissions carries
// those permissions alone, the daemon's client's too, so an App whose
// token_permissions leave out actions = "write" gets HTTP 403 on every
// start: ci_dispatch_as naming one fails to load, saying why. A fixer App
// is one (its tokens carry contents and pull_requests write alone, and
// adding actions = "write" makes it no fixer App), and the error says so.
// An App that sets actions = "write", or no token_permissions (the
// installation's whole access), is accepted.
func TestCIDispatchAsRefusesAnAppWhoseTokensCannotStartWorkflows(t *testing.T) {
	readsActions := fixerApp("reader")
	readsActions.TokenPermissions = map[string]string{"actions": "read", "contents": "read"}
	for name, id := range map[string]Identity{"a fixer App": fixerApp("fixer"), "an App that reads Actions": readsActions} {
		t.Run(name, func(t *testing.T) {
			cfg := validPipelineConfig()
			cfg.Identities = append(cfg.Identities, id)
			cfg.Watches[0].FixIdentity = "fixer"
			if id.Name != "fixer" {
				cfg.Identities = append(cfg.Identities, fixerApp("fixer"))
			}
			cfg.Repos = []Repo{{Repo: "acme/app", CIDispatch: "ci.yml", CIDispatchAs: id.Name}}
			wantError(t, cfg.Validate(), "repo acme/app: ", `ci_dispatch_as "`+id.Name+`"`,
				`token_permissions leave out actions = "write"`, "HTTP 403", "a fixer App cannot", "name a gh identity")
		})
	}

	writesActions := fixerApp("starter")
	writesActions.TokenPermissions = map[string]string{"actions": "write", "contents": "read"}
	wholeAccess := fixerApp("whole")
	wholeAccess.TokenRepositories, wholeAccess.TokenPermissions, wholeAccess.UserID = nil, nil, 0
	for _, id := range []Identity{writesActions, wholeAccess} {
		cfg := validPipelineConfig()
		cfg.Identities = append(cfg.Identities, id)
		cfg.Repos = []Repo{{Repo: "acme/app", CIDispatch: "ci.yml", CIDispatchAs: id.Name}}
		if err := cfg.Validate(); err != nil {
			t.Errorf("ci_dispatch_as %s (token_permissions %v): %v", id.Name, id.TokenPermissions, err)
		}
	}
}
