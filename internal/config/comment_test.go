package config

import (
	"slices"
	"strings"
	"testing"
)

// commentConfig has two watches of the owner talkable, each with a fixer
// App of its own (two installations), beside the operator's gh identity
// and the App magnum reviews as.
func commentConfig() *Config {
	cfg := validPipelineConfig()
	first, second := fixerApp("fixer-app"), fixerApp("fixer-two")
	first.TokenRepositories, second.TokenRepositories = []string{"talkable"}, []string{"example-repo"}
	second.InstallationID = 9
	cfg.Identities = append(cfg.Identities, reviewApp(), first, second)
	cfg.Watches = []Watch{
		{Owner: "talkable", Include: []string{"talkable"}, Identity: "review-app", PollIdentity: "review-app", FixIdentity: "fixer-app"},
		{Owner: "talkable", Include: []string{"example-*"}, Identity: "review-app", PollIdentity: "review-app", FixIdentity: "fixer-two"},
	}
	return cfg
}

// magnum comment posts as the fixer App of the watch that covers the
// repository, in any case: with two watches of one owner, each repository
// gets its own watch's App.
func TestCommentIdentityIsTheWatchsFixerApp(t *testing.T) {
	cfg := commentConfig()
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate = %v", err)
	}
	for full, want := range map[string]string{"talkable/talkable": "fixer-app", "Talkable/Example-Repo": "fixer-two"} {
		w, id, err := cfg.CommentIdentity(full)
		if err != nil || id == nil || id.Name != want || w == nil || w.FixIdentity != want {
			t.Errorf("CommentIdentity(%s) = %v, %+v, %v; want %s", full, w, id, err, want)
		}
	}
}

// Every refusal says which key to set, and none falls back to another
// identity: not the poll identity (the App magnum reviews as), not the
// operator's gh login.
func TestCommentIdentityRefusesWithTheKeyToSet(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Config)
		repo string
		want []string
	}{
		"an unwatched repository": {func(*Config) {}, "example/other",
			[]string{"magnum watches no repository example/other", "[[watch]]"}},
		"no fix_identity": {func(c *Config) { c.Watches[0].FixIdentity = "" }, "talkable/talkable",
			[]string{"sets no fix_identity", "never as your login", "set fix_identity", `kind = "app"`}},
		"an undeclared fix_identity": {func(c *Config) { c.Watches[0].FixIdentity = "ghost" }, "talkable/talkable",
			[]string{`fix_identity "ghost"`, "[[identity]]"}},
		"a gh fix_identity": {func(c *Config) { c.Watches[0].FixIdentity = "z" }, "talkable/talkable",
			[]string{`fix_identity "z"`, `is kind = "gh", your own login`, `kind = "app"`}},
		"the review App": {func(c *Config) { c.Watches[0].FixIdentity = "review-app" }, "talkable/talkable",
			[]string{`fix_identity "review-app"`, "is the App magnum reviews as", "would read as magnum's review", `kind = "app"`}},
		"outside token_repositories": {func(c *Config) { c.Watches[1].Include = []string{"example-*", "other"} }, "talkable/other",
			[]string{`the App fixer-two's tokens do not reach talkable/other`, `add "other" to token_repositories of [[identity]] fixer-two`}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := commentConfig()
			tc.edit(cfg)
			w, id, err := cfg.CommentIdentity(tc.repo)
			if err == nil || id != nil {
				t.Fatalf("CommentIdentity = %v, %+v, nil error; want a refusal", w, id)
			}
			for _, s := range tc.want {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error = %q, want %q in it", err, s)
				}
			}
		})
	}
}

// The logins whose threads magnum's review started: every watch's posting
// identity and every App, in Account form (an App's login with its
// "[bot]", even when the config leaves it out); a gh identity no watch
// posts as is the operator's alone and stays out.
func TestMagnumThreadLoginsHasEveryPostingAndAppLogin(t *testing.T) {
	cfg := commentConfig()
	cfg.Identities = append(cfg.Identities, Identity{Name: "old-app", Kind: "app", Login: "old-app"})
	got := cfg.MagnumThreadLogins()
	for _, want := range []string{"review-app[bot]", "fixer-app[bot]", "fixer-two[bot]", "old-app[bot]"} {
		if !slices.Contains(got, want) {
			t.Errorf("MagnumThreadLogins = %q, want %q in it", got, want)
		}
	}
	if slices.Contains(got, "z") || slices.Contains(got, "me") || len(got) != 4 {
		t.Errorf("MagnumThreadLogins = %q: want each App once and no gh identity no watch posts as", got)
	}
	cfg.Watches[0].Identity = "z"
	if got := cfg.MagnumThreadLogins(); !slices.Contains(got, "z") {
		t.Errorf("MagnumThreadLogins = %q, want the gh identity a watch posts as", got)
	}
}
