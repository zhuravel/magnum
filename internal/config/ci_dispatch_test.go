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

// ci_dispatch_as must name a configured [[identity]], with or without
// ci_dispatch: one that names none fails to load, naming the block and why.
// Alone it names who re-runs a run of an own PR's head that started no job
// (the look's rule 11): the live repository's poll identity of 2026-11-09
// was an App with Actions: read only. It applies to a repository with a
// [[pool]], as ci_dispatch does.
func TestABadCIDispatchAsFailsValidation(t *testing.T) {
	cfg := validPipelineConfig()
	cfg.Repos = []Repo{{Repo: "acme/app", CIDispatch: "ci.yml", CIDispatchAs: "ghost"}}
	wantError(t, cfg.Validate(), "repo acme/app: ", `ci_dispatch_as "ghost"`, "no [[identity]]")

	cfg = validPipelineConfig()
	cfg.Repos = []Repo{{Repo: "acme/app", CIDispatchAs: "ghost"}}
	wantError(t, cfg.Validate(), "repo acme/app: ", `ci_dispatch_as "ghost"`, "no [[identity]]")

	cfg = validPipelineConfig()
	cfg.Repos = []Repo{{Repo: "acme/app", CIDispatchAs: "me"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("ci_dispatch_as without ci_dispatch: %v", err)
	}

	cfg = validPipelineConfig()
	cfg.Pools = []Pool{{Repo: "acme/app", MainClone: "/p/app", SlotName: "r{n}", SlotPath: "/p/app.r{n}", Max: 1}}
	cfg.Repos = []Repo{{Repo: "acme/app", CIDispatch: "ci.yml", CIDispatchAs: "me"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("ci_dispatch_as on a pooled repository: %v", err)
	}
}

// A fixer App's tokens carry contents and pull_requests write alone, which
// its panes hold; the daemon's Actions calls as it go over a token of the
// daemon's own with Actions write added (engine ci_actions.go), so a fixer
// App may be ci_dispatch_as, alone and with ci_dispatch, as may an App
// whose token_permissions read Actions, one that writes it, and one with
// its installation's whole access. Before, an App whose token_permissions
// left out actions = "write" failed to load.
func TestCIDispatchAsTakesAFixerApp(t *testing.T) {
	reaching := func(id Identity) Identity {
		id.TokenRepositories = []string{"talkable", "app"}
		return id
	}
	readsActions := reaching(fixerApp("reader"))
	readsActions.TokenPermissions = map[string]string{"actions": "read", "contents": "read"}
	writesActions := reaching(fixerApp("starter"))
	writesActions.TokenPermissions = map[string]string{"actions": "write", "contents": "read"}
	wholeAccess := fixerApp("whole")
	wholeAccess.TokenRepositories, wholeAccess.TokenPermissions, wholeAccess.UserID = nil, nil, 0
	for _, id := range []Identity{reaching(fixerApp("fixer")), readsActions, writesActions, wholeAccess} {
		for _, dispatch := range []string{"", "ci.yml"} {
			cfg := validPipelineConfig()
			cfg.Identities = append(cfg.Identities, id)
			if id.Name == "fixer" {
				cfg.Watches[0].FixIdentity = "fixer"
			}
			cfg.Repos = []Repo{{Repo: "acme/app", CIDispatch: dispatch, CIDispatchAs: id.Name}}
			if err := cfg.Validate(); err != nil {
				t.Errorf("ci_dispatch_as %s (token_permissions %v), ci_dispatch %q: %v", id.Name, id.TokenPermissions, dispatch, err)
			}
		}
	}
}

// An App's tokens reach its token_repositories alone, the daemon's own
// for Actions too, so ci_dispatch_as naming an App whose list leaves the
// repository out would have every Actions call refused: it fails to load,
// naming the key. The list's names match in any case.
func TestCIDispatchAsRefusesAnAppWhoseTokensDoNotReachTheRepository(t *testing.T) {
	cfg := validPipelineConfig()
	cfg.Identities = append(cfg.Identities, fixerApp("fixer"))
	cfg.Watches[0].FixIdentity = "fixer"
	cfg.Repos = []Repo{{Repo: "acme/app", CIDispatchAs: "fixer"}}
	wantError(t, cfg.Validate(), "repo acme/app: ", `ci_dispatch_as "fixer"`, "token_repositories", `"app"`)

	cfg.Repos = []Repo{{Repo: "acme/Example-Repo", CIDispatchAs: "fixer"}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("a repository the list names in another case: %v", err)
	}
}
