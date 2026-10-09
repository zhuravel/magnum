package eligibility

import (
	"testing"

	"github.com/zhuravel/magnum/internal/config"
)

// base returns an unremarkable PR that every default watch accepts.
func base() PRFacts {
	return PRFacts{
		Number:      42,
		AuthorLogin: "alice",
		Labels:      []string{"backend"},
	}
}

// own makes the PR the operator's: its author is one of the self logins
// (the engine sets Own from config.SelfLogins).
func own(f *PRFacts) { f.AuthorLogin, f.Own = "zhuravel", true }

func TestClassify(t *testing.T) {
	tests := []struct {
		name   string
		watch  config.Watch
		mutate func(*PRFacts)
		want   bool
		reason string
	}{
		{name: "plain PR is eligible with an empty reason", want: true},
		{name: "defaults accept drafts", mutate: func(f *PRFacts) { f.IsDraft = true }, want: true},
		{name: "defaults accept own PRs", mutate: own, want: true},

		// departed authors: a branch PR by someone with no access now
		{name: "a member is eligible", mutate: func(f *PRFacts) { f.AuthorAssociation = "MEMBER" }, want: true},
		{name: "a collaborator is eligible", mutate: func(f *PRFacts) { f.AuthorAssociation = "COLLABORATOR" }, want: true},
		{name: "an owner is eligible", mutate: func(f *PRFacts) { f.AuthorAssociation = "OWNER" }, want: true},
		{name: "an unknown association is eligible", mutate: func(f *PRFacts) { f.AuthorAssociation = "" }, want: true},
		{name: "a former member is skipped by default", watch: config.Watch{Owner: "talkable"},
			mutate: func(f *PRFacts) { f.AuthorAssociation = "CONTRIBUTOR" },
			reason: "author alice left talkable: no longer a member or collaborator (skip_departed_authors = true)"},
		{name: "a first-time contributor's branch is skipped", mutate: func(f *PRFacts) { f.AuthorAssociation = "FIRST_TIME_CONTRIBUTOR" },
			reason: "author alice left the repository: no longer a member or collaborator (skip_departed_authors = true)"},
		{name: "skip_departed_authors = false keeps them", watch: config.Watch{SkipDepartedAuthors: new(false)},
			mutate: func(f *PRFacts) { f.AuthorAssociation = "CONTRIBUTOR" }, want: true},
		{name: "a fork PR is skip_cross_repository's business", watch: config.Watch{SkipCrossRepository: new(false)},
			mutate: func(f *PRFacts) { f.AuthorAssociation, f.IsCrossRepo = "FIRST_TIME_CONTRIBUTOR", true }, want: true},

		// muted
		{name: "muted is ineligible", mutate: func(f *PRFacts) { f.Muted = true }, reason: "muted"},
		{name: "muted wins over every other rule", mutate: func(f *PRFacts) {
			f.Muted, f.AuthorIsBot, f.IsDraft, f.IsCrossRepo = true, true, true, true
		}, reason: "muted"},

		// manual_repos: shown, reviewed only on request
		{name: "a manual repository's PR is ineligible", watch: config.Watch{Owner: "talkable", ManualRepos: []string{"example"}},
			mutate: func(f *PRFacts) { f.Repo = "example" }, reason: "manual repository (manual_repos)"},
		{name: "manual_repos ignores case", watch: config.Watch{Owner: "talkable", ManualRepos: []string{"EXAMPLE"}},
			mutate: func(f *PRFacts) { f.Repo = "example" }, reason: "manual repository (manual_repos)"},
		{name: "another repository of the watch stays eligible", watch: config.Watch{Owner: "talkable", ManualRepos: []string{"example"}},
			mutate: func(f *PRFacts) { f.Repo = "talkable" }, want: true},
		{name: "an unknown repository is not manual", watch: config.Watch{Owner: "talkable", ManualRepos: []string{"example"}}, want: true},
		{name: "muted is reported before manual_repos", watch: config.Watch{ManualRepos: []string{"example"}},
			mutate: func(f *PRFacts) { f.Repo, f.Muted = "example", true }, reason: "muted"},
		{name: "manual_repos is reported before the author rules", watch: config.Watch{ManualRepos: []string{"example"}},
			mutate: func(f *PRFacts) { f.Repo, f.AuthorIsBot = "example", true }, reason: "manual repository (manual_repos)"},
		{name: "Forced does not bypass manual_repos (the engine's call)", watch: config.Watch{ManualRepos: []string{"example"}},
			mutate: func(f *PRFacts) { f.Repo, f.Forced = "example", true }, reason: "manual repository (manual_repos)"},

		// bots
		{name: "AuthorIsBot is skipped by default", mutate: func(f *PRFacts) { f.AuthorIsBot = true }, reason: "bot author"},
		{name: "[bot] login suffix is skipped even without AuthorIsBot", mutate: func(f *PRFacts) { f.AuthorLogin = "renovate[bot]" }, reason: "bot author"},
		{name: "[BOT] suffix is case-insensitive", mutate: func(f *PRFacts) { f.AuthorLogin = "Renovate[BOT]" }, reason: "bot author"},
		{name: "login merely containing bot is not a bot", mutate: func(f *PRFacts) { f.AuthorLogin = "robot" }, want: true},
		{name: "[bot] in the middle of a login is not a suffix", mutate: func(f *PRFacts) { f.AuthorLogin = "a[bot]b" }, want: true},
		{name: "skip_bot_authors=true skips bots", watch: config.Watch{SkipBotAuthors: new(true)}, mutate: func(f *PRFacts) { f.AuthorIsBot = true }, reason: "bot author"},
		{name: "skip_bot_authors=false keeps AuthorIsBot", watch: config.Watch{SkipBotAuthors: new(false)}, mutate: func(f *PRFacts) { f.AuthorIsBot = true }, want: true},
		{name: "skip_bot_authors=false keeps [bot] logins", watch: config.Watch{SkipBotAuthors: new(false)}, mutate: func(f *PRFacts) { f.AuthorLogin = "dependabot[bot]" }, want: true},

		// skip_authors
		{name: "skip_authors exact", watch: config.Watch{SkipAuthors: []string{"mallory"}}, mutate: func(f *PRFacts) { f.AuthorLogin = "mallory" }, reason: `author "mallory" is in skip_authors`},
		{name: "skip_authors case-insensitive", watch: config.Watch{SkipAuthors: []string{"mallory"}}, mutate: func(f *PRFacts) { f.AuthorLogin = "Mallory" }, reason: `author "Mallory" is in skip_authors`},
		{name: "skip_authors entry without [bot] matches login with [bot]", watch: config.Watch{SkipBotAuthors: new(false), SkipAuthors: []string{"dependabot"}}, mutate: func(f *PRFacts) { f.AuthorLogin = "dependabot[bot]" }, reason: `author "dependabot[bot]" is in skip_authors`},
		{name: "skip_authors entry with [bot] matches login without [bot]", watch: config.Watch{SkipAuthors: []string{"dependabot[bot]"}}, mutate: func(f *PRFacts) { f.AuthorLogin = "dependabot" }, reason: `author "dependabot" is in skip_authors`},
		{name: "skip_authors entry with [bot] matches login with [bot] case-insensitively", watch: config.Watch{SkipBotAuthors: new(false), SkipAuthors: []string{"Dependabot[Bot]"}}, mutate: func(f *PRFacts) { f.AuthorLogin = "dependabot[bot]" }, reason: `author "dependabot[bot]" is in skip_authors`},
		{name: "skip_authors second entry matches", watch: config.Watch{SkipAuthors: []string{"x", "mallory"}}, mutate: func(f *PRFacts) { f.AuthorLogin = "mallory" }, reason: `author "mallory" is in skip_authors`},
		{name: "skip_authors is not a prefix match", watch: config.Watch{SkipAuthors: []string{"dependabot"}}, mutate: func(f *PRFacts) { f.AuthorLogin = "dependabot-preview" }, want: true},
		{name: "skip_authors is not a substring match", watch: config.Watch{SkipAuthors: []string{"bot"}}, mutate: func(f *PRFacts) { f.AuthorLogin = "abot" }, want: true},
		{name: "skip_authors applies even when bots are not skipped", watch: config.Watch{SkipBotAuthors: new(false), SkipAuthors: []string{"dependabot"}}, mutate: func(f *PRFacts) { f.AuthorLogin = "dependabot"; f.AuthorIsBot = true }, reason: `author "dependabot" is in skip_authors`},
		{name: "empty skip_authors entry never matches an empty login", watch: config.Watch{SkipAuthors: []string{"", "  "}}, mutate: func(f *PRFacts) { f.AuthorLogin = "" }, want: true},
		{name: "skip_authors entry is trimmed", watch: config.Watch{SkipAuthors: []string{" mallory "}}, mutate: func(f *PRFacts) { f.AuthorLogin = "mallory" }, reason: `author "mallory" is in skip_authors`},

		// skip_labels
		{name: "skip_labels exact match", watch: config.Watch{SkipLabels: []string{"WIP"}}, mutate: func(f *PRFacts) { f.Labels = []string{"backend", "WIP"} }, reason: `label "WIP" is in skip_labels`},
		{name: "skip_labels is case-sensitive", watch: config.Watch{SkipLabels: []string{"WIP"}}, mutate: func(f *PRFacts) { f.Labels = []string{"wip"} }, want: true},
		{name: "skip_labels is not a substring match", watch: config.Watch{SkipLabels: []string{"WIP"}}, mutate: func(f *PRFacts) { f.Labels = []string{"WIP: later", "not-WIP"} }, want: true},
		{name: "skip_labels does not trim the PR's labels", watch: config.Watch{SkipLabels: []string{"WIP"}}, mutate: func(f *PRFacts) { f.Labels = []string{"WIP "} }, want: true},
		{name: "skip_labels with no labels on the PR", watch: config.Watch{SkipLabels: []string{"WIP"}}, mutate: func(f *PRFacts) { f.Labels = nil }, want: true},
		{name: "empty skip_labels keeps everything", mutate: func(f *PRFacts) { f.Labels = []string{"WIP", "LG"} }, want: true},
		{name: "skip_labels second entry matches", watch: config.Watch{SkipLabels: []string{"LG", "WIP"}}, mutate: func(f *PRFacts) { f.Labels = []string{"WIP"} }, reason: `label "WIP" is in skip_labels`},

		// drafts
		{name: "include_drafts=false skips drafts", watch: config.Watch{IncludeDrafts: new(false)}, mutate: func(f *PRFacts) { f.IsDraft = true }, reason: "draft PR (include_drafts = false)"},
		{name: "include_drafts=false keeps ready PRs", watch: config.Watch{IncludeDrafts: new(false)}, want: true},
		{name: "include_drafts=true keeps drafts", watch: config.Watch{IncludeDrafts: new(true)}, mutate: func(f *PRFacts) { f.IsDraft = true }, want: true},
		{name: "include_drafts=false keeps a draft with a pending review request", watch: config.Watch{IncludeDrafts: new(false)},
			mutate: func(f *PRFacts) { f.IsDraft, f.Requested = true, true }, want: true},
		{name: "a review request does not lift the other filters", watch: config.Watch{IncludeDrafts: new(false), SkipLabels: []string{"WIP"}},
			mutate: func(f *PRFacts) { f.IsDraft, f.Requested, f.Labels = true, true, []string{"WIP"} }, reason: `label "WIP" is in skip_labels`},
		{name: "a review request does not lift include_own", watch: config.Watch{IncludeDrafts: new(false), IncludeOwn: new(false)},
			mutate: func(f *PRFacts) { own(f); f.IsDraft, f.Requested = true, true }, reason: "own PR (include_own = false)"},

		// own PRs: "own" is any of the self logins, as for the board's "mine"
		{name: "include_own=false skips own PRs", watch: config.Watch{IncludeOwn: new(false)}, mutate: own, reason: "own PR (include_own = false)"},
		{name: "include_own=false skips a PR by a second self login", watch: config.Watch{IncludeOwn: new(false)},
			mutate: func(f *PRFacts) { f.AuthorLogin, f.Own = "rev-ann", true }, reason: "own PR (include_own = false)"},
		{name: "include_own=false keeps other authors", watch: config.Watch{IncludeOwn: new(false)}, want: true},
		{name: "include_own=false keeps an author none of the self logins matched", watch: config.Watch{IncludeOwn: new(false)},
			mutate: func(f *PRFacts) { f.AuthorLogin = "zhuravel" }, want: true},
		{name: "include_own=true keeps own PRs", watch: config.Watch{IncludeOwn: new(true)}, mutate: own, want: true},

		// cross-repository
		{name: "cross-repo PRs are skipped by default", mutate: func(f *PRFacts) { f.IsCrossRepo = true }, reason: "cross-repository PR (skip_cross_repository = true)"},
		{name: "skip_cross_repository=true skips forks", watch: config.Watch{SkipCrossRepository: new(true)}, mutate: func(f *PRFacts) { f.IsCrossRepo = true }, reason: "cross-repository PR (skip_cross_repository = true)"},
		{name: "skip_cross_repository=false keeps forks", watch: config.Watch{SkipCrossRepository: new(false)}, mutate: func(f *PRFacts) { f.IsCrossRepo = true }, want: true},
		{name: "same-repo PRs are kept", watch: config.Watch{SkipCrossRepository: new(true)}, want: true},

		// manual overrides are the engine's call, not a filter concern
		{name: "Forced does not bypass filters", mutate: func(f *PRFacts) { f.Forced, f.IsCrossRepo = true, true }, reason: "cross-repository PR (skip_cross_repository = true)"},
		{name: "Pinned does not affect eligibility", mutate: func(f *PRFacts) { f.Pinned = true }, want: true},

		// rule order after muted: bot, authors, labels, drafts, own, cross-repo
		{name: "bot is reported before skip_authors", watch: config.Watch{SkipAuthors: []string{"dependabot"}}, mutate: func(f *PRFacts) { f.AuthorLogin = "dependabot[bot]"; f.AuthorIsBot = true }, reason: "bot author"},
		{name: "skip_authors is reported before labels", watch: config.Watch{SkipAuthors: []string{"mallory"}, SkipLabels: []string{"WIP"}}, mutate: func(f *PRFacts) { f.AuthorLogin = "mallory"; f.Labels = []string{"WIP"} }, reason: `author "mallory" is in skip_authors`},
		{name: "labels are reported before drafts", watch: config.Watch{SkipLabels: []string{"WIP"}, IncludeDrafts: new(false)}, mutate: func(f *PRFacts) { f.Labels = []string{"WIP"}; f.IsDraft = true }, reason: `label "WIP" is in skip_labels`},
		{name: "drafts are reported before own PRs", watch: config.Watch{IncludeDrafts: new(false), IncludeOwn: new(false)}, mutate: func(f *PRFacts) { own(f); f.IsDraft = true }, reason: "draft PR (include_drafts = false)"},
		{name: "own PRs are reported before cross-repo", watch: config.Watch{IncludeOwn: new(false)}, mutate: func(f *PRFacts) { own(f); f.IsCrossRepo = true }, reason: "own PR (include_own = false)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := base()
			if tc.mutate != nil {
				tc.mutate(&f)
			}
			wantEligible := tc.want
			got := Classify(tc.watch, f)
			if got.Eligible != wantEligible {
				t.Fatalf("Eligible = %v (%q), want %v", got.Eligible, got.Reason, wantEligible)
			}
			if got.Reason != tc.reason {
				t.Fatalf("Reason = %q, want %q", got.Reason, tc.reason)
			}
		})
	}
}

// TestClassifyShippedTalkableWatch pins the config.toml shipped in
// the repository: dependabot, bots and forks are out, everything else is in.
func TestClassifyShippedTalkableWatch(t *testing.T) {
	w := config.Watch{
		Owner:               "talkable",
		IncludeDrafts:       new(true),
		IncludeOwn:          new(true),
		SkipBotAuthors:      new(true),
		SkipAuthors:         []string{"dependabot"},
		SkipLabels:          []string{},
		SkipCrossRepository: new(true),
	}
	cases := []struct {
		name string
		f    PRFacts
		want bool
	}{
		{"colleague", PRFacts{AuthorLogin: "alice"}, true},
		{"own draft with LG label", PRFacts{AuthorLogin: "zhuravel", Own: true, IsDraft: true, Labels: []string{"LG"}}, true},
		{"dependabot as GraphQL bot", PRFacts{AuthorLogin: "dependabot", AuthorIsBot: true}, false},
		{"dependabot as REST login", PRFacts{AuthorLogin: "dependabot[bot]"}, false},
		{"fork", PRFacts{AuthorLogin: "stranger", IsCrossRepo: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(w, tc.f); got.Eligible != tc.want {
				t.Fatalf("Classify = %+v, want eligible=%v", got, tc.want)
			}
		})
	}
}
