package config

import (
	"fmt"
	"testing"
)

// allowCase is one AutoApproveAllowsAuthor question and its answer.
type allowCase struct {
	repo, login string
	want        bool
}

// wantAllows checks AutoApproveAllowsAuthor's answers on cfg.
func wantAllows(t *testing.T, cfg *Config, cases []allowCase) {
	t.Helper()
	for _, c := range cases {
		if got := cfg.AutoApproveAllowsAuthor(c.repo, c.login); got != c.want {
			t.Errorf("AutoApproveAllowsAuthor(%q, %q) = %v, want %v", c.repo, c.login, got, c.want)
		}
	}
}

// autoApproveEverywhere is autoApproveConfig with its watch auto-approving
// every repository it covers as z.
func autoApproveEverywhere() *Config {
	cfg := autoApproveConfig()
	cfg.Watches[0].AutoApprove, cfg.Watches[0].AutoApproveAs = []string{"*"}, "z"
	return cfg
}

// Without auto_approve_authors on the watch or the repository there is no
// allowlist: every author is allowed (auto_approve_skip_authors alone holds
// some back), a blank login too. An empty list is no list.
func TestWithoutAutoApproveAuthorsEveryAuthorIsAllowed(t *testing.T) {
	cfg := autoApproveEverywhere()
	cfg.Repos = []Repo{{Repo: "acme/app", AutoApproveAuthors: []string{}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	wantAllows(t, cfg, []allowCase{
		{"acme/app", "alice", true}, {"acme/web-site", "bob-rev", true}, {"acme/app", "", true}, {"other/app", "alice", true},
	})
}

// A watch's auto_approve_authors decides on every repository the watch
// auto-approves: only the logins it names are allowed, matched as
// auto_approve_skip_authors matches them (any case, with or without
// "[bot]"); anyone else, and a PR whose author magnum does not know, is not.
func TestAWatchsAutoApproveAuthorsAllowsOnlyItsLoginsOnEveryRepository(t *testing.T) {
	cfg := autoApproveEverywhere()
	cfg.Watches[0].AutoApproveAuthors = []string{"Alice", "helper[bot]"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	wantAllows(t, cfg, []allowCase{
		{"acme/app", "alice", true}, {"acme/web-site", "ALICE", true}, {"Acme/App", "alice[bot]", true},
		{"acme/app", "helper", true}, {"acme/web-site", "Helper[bot]", true},
		{"acme/app", "bob-rev", false}, {"acme/web-site", "rev-ann", false}, {"acme/app", "", false}, {"acme/app", " ", false},
	})
}

// A [[repo]] block's auto_approve_authors replaces the watch's list for that
// repository alone (it does not add to it): there only its logins are
// allowed, and the watch's other repositories keep the watch's list. A
// repository's list with no watch list narrows that repository alone.
func TestARepositorysAutoApproveAuthorsReplacesTheWatchsForItAlone(t *testing.T) {
	cfg := autoApproveEverywhere()
	cfg.Watches[0].AutoApproveAuthors = []string{"alice"}
	cfg.Repos = []Repo{{Repo: "acme/app", AutoApproveAuthors: []string{"bob-rev"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	wantAllows(t, cfg, []allowCase{
		{"acme/app", "bob-rev", true}, {"ACME/app", "Bob-Rev", true}, {"acme/app", "alice", false}, {"acme/app", "", false},
		{"acme/web-site", "alice", true}, {"acme/web-site", "bob-rev", false},
	})

	cfg.Watches[0].AutoApproveAuthors = nil
	wantAllows(t, cfg, []allowCase{
		{"acme/app", "bob-rev", true}, {"acme/app", "alice", false},
		{"acme/web-site", "alice", true}, {"acme/web-site", "rev-ann", true},
	})
}

// A login on both auto_approve_authors and auto_approve_skip_authors is
// skipped: the allowlist never takes a login off the skip list, on the
// watch or the repository (the engine asks the skip list first).
func TestAutoApproveSkipAuthorsWinsOverAutoApproveAuthors(t *testing.T) {
	cfg := autoApproveEverywhere()
	cfg.Watches[0].AutoApproveAuthors = []string{"alice", "rev-ann"}
	cfg.Watches[0].AutoApproveSkipAuthors = []string{"alice"}
	cfg.Repos = []Repo{{Repo: "acme/app", AutoApproveAuthors: []string{"rev-ann"}, AutoApproveSkipAuthors: []string{"REV-ANN"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ repo, login string }{{"acme/web-site", "alice"}, {"acme/app", "rev-ann"}} {
		if !cfg.AutoApproveSkipsAuthor(c.repo, c.login) {
			t.Errorf("AutoApproveSkipsAuthor(%q, %q) = false with the login on both lists, want it skipped", c.repo, c.login)
		}
	}
}

// auto_approve_authors lists GitHub logins on a [[watch]] and a [[repo]]
// block, validated as auto_approve_skip_authors is: an empty entry or one
// with whitespace in it is refused, naming the block; logins that differ
// only in case or "[bot]" are one login, not an error (TestAnAppPrefixedLoginIsRefused
// refuses an App's URL form).
func TestAutoApproveAuthorsRefusesAnEmptyOrSpacedLogin(t *testing.T) {
	for _, bad := range []string{"", " ", "al ice", " alice", "alice\t"} {
		cfg := autoApproveEverywhere()
		cfg.Watches[0].AutoApproveAuthors = []string{"alice", bad}
		wantError(t, cfg.Validate(), fmt.Sprintf("watch acme: auto_approve_authors entry %q must be a GitHub login", bad))

		cfg = autoApproveEverywhere()
		cfg.Repos = []Repo{{Repo: "acme/app", AutoApproveAuthors: []string{bad}}}
		wantError(t, cfg.Validate(), fmt.Sprintf("repo acme/app: auto_approve_authors entry %q must be a GitHub login", bad))
	}
	cfg := autoApproveEverywhere()
	cfg.Watches[0].AutoApproveAuthors = []string{"alice", "Alice", "alice[bot]", "rev-ann"}
	cfg.Repos = []Repo{{Repo: "acme/app", AutoApproveAuthors: []string{"bob-rev", "BOB-REV"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("logins that differ in case or [bot]: %v", err)
	}
}

// auto_approve_authors is not a per-PR worktree key: a [[repo]] block for a
// repository with a [[pool]] may set it, and it decides there.
func TestAPooledRepositoryMaySetAutoApproveAuthors(t *testing.T) {
	cfg := validPipelineConfig()
	cfg.Watches = []Watch{{Owner: "example", Include: []string{"*"}, Identity: "z", PollIdentity: "z", AutoApprove: []string{"*"}, AutoApproveAs: "z"}}
	cfg.Pools = []Pool{{Repo: "example/big", MainClone: "/p/big", SlotName: "r{n}", SlotPath: "/p/big.r{n}", Max: 1}}
	cfg.Repos = []Repo{{Repo: "example/big", AutoApproveAuthors: []string{"alice"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a [[repo]] for a pooled repository with auto_approve_authors: %v", err)
	}
	wantAllows(t, cfg, []allowCase{{"example/big", "alice", true}, {"example/big", "bob-rev", false}})
}

// The lists load from the user's config.toml, on a [[watch]] and a [[repo]].
func TestAutoApproveAuthorsLoads(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"config.toml": minimalConfig + `auto_approve = ["*"]
auto_approve_as = "z"
auto_approve_authors = ["alice"]
[[repo]]
repo = "acme/app"
auto_approve_authors = ["rev-ann"]
`})
	if got := cfg.Watches[0].AutoApproveAuthors; len(got) != 1 || got[0] != "alice" {
		t.Fatalf("watch auto_approve_authors = %q", got)
	}
	wantAllows(t, cfg, []allowCase{
		{"acme/app", "rev-ann", true}, {"acme/app", "alice", false}, {"acme/web", "alice", true}, {"acme/web", "rev-ann", false},
	})
}

// AutoApproveRefusesAuthor is the two lists together: true for a login
// auto_approve_skip_authors names, allowed or not, and for one an
// auto_approve_authors that applies leaves out (a blank login included);
// false for a listed login and, without lists, for everyone. A nil
// configuration refuses nobody.
func TestAutoApproveRefusesAuthorJoinsTheSkipListAndTheAllowlist(t *testing.T) {
	cfg := autoApproveEverywhere()
	cfg.Watches[0].AutoApproveSkipAuthors = []string{"alice"}
	cfg.Watches[0].AutoApproveAuthors = []string{"alice", "rev-ann"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []allowCase{
		{"acme/app", "alice", true}, {"acme/app", "bob-rev", true}, {"acme/app", "", true},
		{"acme/app", "rev-ann", false}, {"acme/web-site", "Rev-Ann[bot]", false},
	} {
		if got := cfg.AutoApproveRefusesAuthor(c.repo, c.login); got != c.want {
			t.Errorf("AutoApproveRefusesAuthor(%q, %q) = %v, want %v", c.repo, c.login, got, c.want)
		}
	}
	cfg.Watches[0].AutoApproveSkipAuthors, cfg.Watches[0].AutoApproveAuthors = nil, nil
	if cfg.AutoApproveRefusesAuthor("acme/app", "bob-rev") || cfg.AutoApproveRefusesAuthor("acme/app", "") {
		t.Error("without lists an author is refused")
	}
	var none *Config
	if none.AutoApproveRefusesAuthor("acme/app", "alice") {
		t.Error("a nil configuration refuses an author")
	}
}

// An identity that approves when clean comments instead on the PR of an
// author the lists refuse (the judge's no-findings event is COMMENT there:
// pipeline.JudgeEvents), so its clean comment counts as magnum's approval
// for the PRs that need the operator; an allowed author's PR keeps the
// identity's APPROVE, and an identity that comments when clean always does.
func TestCommentsWhenCleanCountsAnAuthorTheListsRefuse(t *testing.T) {
	cfg := autoApproveEverywhere()
	cfg.Watches[0].AutoApproveSkipAuthors = []string{"alice"}
	cfg.Watches[0].AutoApproveAuthors = []string{"rev-ann"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		identity, author string
		want             bool
	}{
		{"z", "alice", true}, {"z", "bob-rev", true}, {"z", "", true}, {"z", "rev-ann", false},
		{"app", "rev-ann", true}, {"app", "alice", true},
	} {
		if got := cfg.CommentsWhenClean("acme/app", c.identity, c.author); got != c.want {
			t.Errorf("CommentsWhenClean(acme/app, %s, %q) = %v, want %v", c.identity, c.author, got, c.want)
		}
	}
}
