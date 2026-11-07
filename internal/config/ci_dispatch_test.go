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
