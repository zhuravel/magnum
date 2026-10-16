package config

import (
	"fmt"
	"testing"
)

// allow_bot_authors lists GitHub logins, on a [[watch]] and as an addition
// on a [[repo]] block, validated as auto_approve_skip_authors is: an entry
// that is empty or has whitespace in it is refused, naming the block; logins
// that differ only in case or a "[bot]" suffix are one login, not an error.
func TestAllowBotAuthorsRefusesAnEmptyOrSpacedLogin(t *testing.T) {
	for _, bad := range []string{"", " ", "help er", " helper", "helper[bot]\t"} {
		cfg := autoApproveConfig()
		cfg.Watches[0].AllowBotAuthors = []string{"helper", bad}
		wantError(t, cfg.Validate(), fmt.Sprintf("watch acme: allow_bot_authors entry %q must be a GitHub login", bad))

		cfg = autoApproveConfig()
		cfg.Repos = []Repo{{Repo: "acme/app", AllowBotAuthors: []string{bad}}}
		wantError(t, cfg.Validate(), fmt.Sprintf("repo acme/app: allow_bot_authors entry %q must be a GitHub login", bad))
	}
	cfg := autoApproveConfig()
	cfg.Watches[0].AllowBotAuthors = []string{"helper", "Helper", "helper[bot]"}
	cfg.Repos = []Repo{{Repo: "acme/app", AllowBotAuthors: []string{"other[bot]", "OTHER"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("logins that differ in case or [bot]: %v", err)
	}
}

// AllowsBotAuthor matches a PR's author against the watch's allow_bot_authors
// and its repository's [[repo]] addition the way the other login lists match
// (github.SameLogin): any case, with or without "[bot]", so a bot's GraphQL
// login ("helper", __typename Bot) and its REST login ("helper[bot]") are
// one. A [[repo]] block that adds nothing keeps the watch's list, a block's
// list adds to the watch's for that repository alone, and a blank login
// matches nothing. The keys load from the user's config.toml.
func TestAllowsBotAuthorMatchesTheListedBots(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"config.toml": minimalConfig + `allow_bot_authors = ["Helper"]
[[repo]]
repo = "acme/app"
stakes = "Internal tooling."
[[repo]]
repo = "acme/web"
allow_bot_authors = ["other[bot]"]
`})
	if got := cfg.Watches[0].AllowBotAuthors; len(got) != 1 || got[0] != "Helper" {
		t.Fatalf("watch allow_bot_authors = %q", got)
	}
	for _, c := range []struct {
		repo, login string
		want        bool
	}{
		{"acme/site", "helper[bot]", true}, {"acme/site", "helper", true}, {"acme/site", "HELPER[BOT]", true},
		{"acme/app", "helper[bot]", true}, {"acme/web", "helper[bot]", true},
		{"acme/web", "other", true}, {"Acme/Web", "Other[bot]", true}, {"acme/app", "other[bot]", false},
		{"acme/site", "other[bot]", false}, {"acme/site", "helper-two[bot]", false}, {"acme/site", "", false},
		{"example/app", "helper[bot]", false},
	} {
		if got := cfg.AllowsBotAuthor(c.repo, c.login); got != c.want {
			t.Errorf("AllowsBotAuthor(%q, %q) = %v, want %v", c.repo, c.login, got, c.want)
		}
	}
}
