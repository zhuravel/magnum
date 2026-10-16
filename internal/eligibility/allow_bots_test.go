package eligibility

import (
	"testing"

	"github.com/zhuravel/magnum/internal/config"
)

// helper makes the PR a bot's whose login allow_bot_authors lists (the
// engine sets AllowedBot from config.AllowsBotAuthor).
func helper(f *PRFacts) { f.AuthorLogin, f.AuthorIsBot, f.AllowedBot = "helper", true, true }

// A bot allow_bot_authors lists is not skipped as a bot: with
// skip_bot_authors (the default, or set) its PR is eligible as a person's
// would be, and every other rule still applies to it; an unlisted bot is
// skipped as before, and with skip_bot_authors = false the list changes
// nothing.
func TestAnAllowedBotPassesSkipBotAuthors(t *testing.T) {
	tests := []struct {
		name   string
		watch  config.Watch
		mutate func(*PRFacts)
		reason string // "" = eligible
	}{
		{name: "a listed bot is eligible by default", mutate: helper},
		{name: "a listed bot is eligible with skip_bot_authors = true", watch: config.Watch{SkipBotAuthors: new(true)}, mutate: helper},
		{name: "a listed bot in the REST form", mutate: func(f *PRFacts) { f.AuthorLogin, f.AllowedBot = "helper[bot]", true }},
		{name: "an unlisted bot is skipped", mutate: func(f *PRFacts) { f.AuthorLogin, f.AuthorIsBot = "other", true }, reason: "bot author"},
		{name: "skip_bot_authors = false keeps an unlisted bot", watch: config.Watch{SkipBotAuthors: new(false)},
			mutate: func(f *PRFacts) { f.AuthorLogin, f.AuthorIsBot = "other", true }},
		{name: "skip_bot_authors = false keeps a listed bot", watch: config.Watch{SkipBotAuthors: new(false)}, mutate: helper},
		{name: "muted wins over the list", mutate: func(f *PRFacts) { helper(f); f.Muted = true }, reason: "muted"},
		{name: "manual_repos wins over the list", watch: config.Watch{ManualRepos: []string{"example"}},
			mutate: func(f *PRFacts) { helper(f); f.Repo = "example" }, reason: "manual repository (manual_repos)"},
		{name: "skip_labels still applies", watch: config.Watch{SkipLabels: []string{"WIP"}},
			mutate: func(f *PRFacts) { helper(f); f.Labels = []string{"WIP"} }, reason: `label "WIP" is in skip_labels`},
		{name: "include_drafts = false still applies", watch: config.Watch{IncludeDrafts: new(false)},
			mutate: func(f *PRFacts) { helper(f); f.IsDraft = true }, reason: "draft PR (include_drafts = false)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := base()
			tc.mutate(&f)
			got := Classify(tc.watch, f)
			if got.Eligible != (tc.reason == "") || got.Reason != tc.reason {
				t.Fatalf("Classify = %+v, want reason %q", got, tc.reason)
			}
		})
	}
}

// skip_authors wins over allow_bot_authors: a bot on both lists is skipped,
// with skip_authors' reason, whichever form either list writes its login in.
func TestSkipAuthorsWinsOverAllowBotAuthors(t *testing.T) {
	for _, skip := range []string{"helper", "Helper[bot]"} {
		f := base()
		helper(&f)
		got := Classify(config.Watch{SkipAuthors: []string{skip}, AllowBotAuthors: []string{"helper"}}, f)
		if got.Eligible || got.Reason != `author "helper" is in skip_authors` {
			t.Fatalf("skip_authors %q: Classify = %+v, want skip_authors' reason", skip, got)
		}
	}
}

// skip_departed_authors reads only people's association: an App's is NONE
// (it is never an owner, a member or a collaborator, whatever its
// installation writes), so a bot's PR the bot rule lets through, listed or
// under skip_bot_authors = false, never "left" the organization, while a
// person's with the same association still did.
func TestTheDepartedRuleNeverSkipsABot(t *testing.T) {
	tests := []struct {
		name   string
		watch  config.Watch
		mutate func(*PRFacts)
		reason string // "" = eligible
	}{
		{name: "a listed App", watch: config.Watch{Owner: "talkable"},
			mutate: func(f *PRFacts) { helper(f); f.AuthorAssociation = "NONE" }},
		{name: "a listed bot in the REST form", watch: config.Watch{Owner: "talkable"},
			mutate: func(f *PRFacts) {
				f.AuthorLogin, f.AllowedBot, f.AuthorAssociation = "helper[bot]", true, "CONTRIBUTOR"
			}},
		{name: "a bot under skip_bot_authors = false", watch: config.Watch{Owner: "talkable", SkipBotAuthors: new(false)},
			mutate: func(f *PRFacts) { f.AuthorLogin, f.AuthorIsBot, f.AuthorAssociation = "other", true, "NONE" }},
		{name: "a person who left", watch: config.Watch{Owner: "talkable"},
			mutate: func(f *PRFacts) { f.AuthorAssociation = "NONE" },
			reason: "author alice left talkable: no longer a member or collaborator (skip_departed_authors = true)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := base()
			tc.mutate(&f)
			got := Classify(tc.watch, f)
			if got.Eligible != (tc.reason == "") || got.Reason != tc.reason {
				t.Fatalf("Classify = %+v, want reason %q", got, tc.reason)
			}
		})
	}
}
