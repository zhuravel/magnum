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
