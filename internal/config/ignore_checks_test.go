package config

import (
	"slices"
	"testing"
)

// A third-party app's check that fails on every PR (talkable/talkable,
// 2026-10-26) kept clean reviews from approving and said "CI is red" on
// each. ignore_checks names checks magnum treats as absent wherever it
// reads CI: it loads on a [[watch]] and on a [[repo]] block, the
// repository's list adds to its watch's (as auto_approve_skip_authors
// does), and Config.IgnoreChecks finds both by the repository's name in any
// case; a repository whose watch and block set none ignores nothing.
func TestIgnoreChecksLoadsOnAWatchAndARepoWhoseListAddsToTheWatchs(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"config.toml": minimalConfig + `ignore_checks = ["Vendor Gates"]
[[watch]]
owner = "example"
include = ["*"]
identity = "z"
[[repo]]
repo = "acme/app"
ignore_checks = ["workflow:Vendor *", "lint-*"]
[[repo]]
repo = "acme/web"
stakes = "Internal tooling."
[[repo]]
repo = "example/site"
ignore_checks = ["RSpec"]
`})
	if got := cfg.Watches[0].IgnoreChecks; !slices.Equal(got, []string{"Vendor Gates"}) {
		t.Fatalf("watch ignore_checks = %q", got)
	}
	for repo, want := range map[string][]string{
		"Acme/App":     {"Vendor Gates", "workflow:Vendor *", "lint-*"},
		"acme/web":     {"Vendor Gates"},
		"acme/other":   {"Vendor Gates"},
		"example/site": {"RSpec"},
		"example/blog": nil,
		"talkable/app": nil,
	} {
		if got := cfg.IgnoreChecks(repo); !slices.Equal(got, want) {
			t.Errorf("IgnoreChecks(%s) = %q, want %q", repo, got, want)
		}
	}
	// The repository's list adds to a copy: the watch's stays as written.
	if got := cfg.Watches[0].IgnoreChecks; !slices.Equal(got, []string{"Vendor Gates"}) {
		t.Fatalf("the watch's ignore_checks after a read = %q", got)
	}
}

// An ignore_checks entry is read as a required_checks one is (a check-name
// glob, or "workflow:<glob>"), so a malformed or empty one fails to load,
// naming the block, on a watch and on a repository alike.
func TestAMalformedIgnoreChecksEntryFailsValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		entry string
		want  string
	}{
		"malformed glob":          {"Vendor [", `ignore_checks pattern "Vendor ["`},
		"malformed workflow glob": {"workflow:Vendor [", `ignore_checks pattern "workflow:Vendor ["`},
		"empty entry":             {" ", `empty ignore_checks entry " "`},
		"empty workflow":          {"workflow:", `empty ignore_checks entry "workflow:"`},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validPipelineConfig()
			cfg.Watches[0].IgnoreChecks = []string{"Vendor Gates", tc.entry}
			wantError(t, cfg.Validate(), "watch acme: ", tc.want)

			cfg = validPipelineConfig()
			cfg.Repos = []Repo{{Repo: "acme/app", IgnoreChecks: []string{tc.entry}}}
			wantError(t, cfg.Validate(), "repo acme/app: ", tc.want)
		})
	}
	cfg := validPipelineConfig()
	cfg.Watches[0].IgnoreChecks = []string{"Vendor Gates", "lint-*", "workflow:Vendor *"}
	cfg.Repos = []Repo{{Repo: "acme/app", IgnoreChecks: []string{"rspec (?)", "workflow:vendor.yml"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("well-formed globs: %v", err)
	}
}

// ignore_checks applies to a repository with a [[pool]] too: it is no key
// of the per-PR worktrees.
func TestIgnoreChecksIsValidOnAPooledRepository(t *testing.T) {
	cfg := validPipelineConfig()
	cfg.Pools = []Pool{{Repo: "acme/big", MainClone: "/p/big", SlotName: "r{n}", SlotPath: "/p/big.r{n}", Max: 1}}
	cfg.Repos = []Repo{{Repo: "acme/big", IgnoreChecks: []string{"Vendor Gates"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("ignore_checks on a pooled repository: %v", err)
	}
	if got := cfg.IgnoreChecks("acme/big"); !slices.Equal(got, []string{"Vendor Gates"}) {
		t.Fatalf("IgnoreChecks = %q", got)
	}
}
