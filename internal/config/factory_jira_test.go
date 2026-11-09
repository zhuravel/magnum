package config

import (
	"slices"
	"testing"
)

// jiraShadowToml is the [jira] site and a repository's Jira keys in
// shadow, as a user writes them.
const jiraShadowToml = `
[jira]
url = "https://example.atlassian.net"
user = "alice@example.com"
token_file = "~/.config/magnum/jira.token"
` + factoryRepoToml + `jira_moves = "shadow"
jira_move_start = { to = "In Development", from = ["To Do", "In Progress"] }
jira_move_published = { to = "In Code Review", from = ["To Do", "In Progress", "In Development"] }
jira_comment = "shadow"
`

// A repository's Jira keys load from its block: the moves' modes, each
// milestone's move (its target and the statuses it moves from) and the
// comment's mode; without them the repository moves and comments on
// nothing (Moves and Comment off).
func TestFactoryRepoJiraKeysLoad(t *testing.T) {
	cfg, err := loadCommittedWithLocal(t, testLocalConfig+jiraShadowToml)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := cfg.FactoryRepo("talkable/talkable")
	if r.JiraMoves != JiraShadow || r.JiraComment != JiraShadow || !r.JiraMovesShadow() || !r.JiraCommentShadow() {
		t.Errorf("jira_moves %q, jira_comment %q, want shadow both", r.JiraMoves, r.JiraComment)
	}
	if m := r.JiraMoveStart; m.To != "In Development" || !slices.Equal(m.From, []string{"To Do", "In Progress"}) {
		t.Errorf("jira_move_start = %+v", m)
	}
	if m, ok := r.JiraMove(JiraMilestonePublished); !ok || m.To != "In Code Review" || len(m.From) != 3 {
		t.Errorf("the published move = %+v, %v", m, ok)
	}
	cfg, err = loadCommittedWithLocal(t, testLocalConfig+factoryRepoToml)
	if err != nil {
		t.Fatal(err)
	}
	r, _ = cfg.FactoryRepo("talkable/talkable")
	if r.JiraMovesShadow() || r.JiraCommentShadow() {
		t.Errorf("a repository without the keys: moves %q, comment %q, want off", r.JiraMoves, r.JiraComment)
	}
	if _, ok := r.JiraMove(JiraMilestoneStart); ok {
		t.Error("a repository without the keys has a start move")
	}
}

// The checks of a repository's Jira keys: the modes are off or shadow
// (ask and auto come after the shadow week, and say so), shadow needs both
// moves and [jira]'s site, a move needs its target and the statuses it
// moves from, none of which is its target, and never a done status (the
// names Jira's done category goes by: the factory never closes an issue).
func TestFactoryRepoJiraKeysAreChecked(t *testing.T) {
	start := FactoryJiraMove{To: "In Development", From: []string{"To Do", "In Progress"}}
	published := FactoryJiraMove{To: "In Code Review", From: []string{"To Do", "In Progress", "In Development"}}
	shadow := func(r *FactoryRepo) { r.JiraMoves, r.JiraMoveStart, r.JiraMovePublished = JiraShadow, start, published }
	for name, tc := range map[string]struct {
		edit func(r *FactoryRepo)
		want []string
	}{
		"moves ask":     {func(r *FactoryRepo) { shadow(r); r.JiraMoves = "ask" }, []string{`jira_moves = "ask"`, "after the shadow week"}},
		"moves auto":    {func(r *FactoryRepo) { shadow(r); r.JiraMoves = "auto" }, []string{`jira_moves = "auto"`, "after the shadow week"}},
		"moves other":   {func(r *FactoryRepo) { r.JiraMoves = "on" }, []string{`jira_moves = "on"`, `"off" or "shadow"`}},
		"comment ask":   {func(r *FactoryRepo) { r.JiraComment = "ask" }, []string{`jira_comment = "ask"`, "after the shadow week"}},
		"comment auto":  {func(r *FactoryRepo) { r.JiraComment = "auto" }, []string{`jira_comment = "auto"`, "after the shadow week"}},
		"comment other": {func(r *FactoryRepo) { r.JiraComment = "live" }, []string{`jira_comment = "live"`, `"off" or "shadow"`}},
		"no start move": {func(r *FactoryRepo) { shadow(r); r.JiraMoveStart = FactoryJiraMove{} },
			[]string{`jira_moves = "shadow" needs jira_move_start`}},
		"no published move": {func(r *FactoryRepo) { shadow(r); r.JiraMovePublished = FactoryJiraMove{} },
			[]string{`jira_moves = "shadow" needs jira_move_published`}},
		"no target": {func(r *FactoryRepo) { shadow(r); r.JiraMoveStart.To = " " }, []string{"jira_move_start", "needs to"}},
		"no from": {func(r *FactoryRepo) { shadow(r); r.JiraMovePublished.From = nil },
			[]string{"jira_move_published", "needs from"}},
		"blank from": {func(r *FactoryRepo) { shadow(r); r.JiraMoveStart.From = []string{"To Do", " "} },
			[]string{"jira_move_start", "blank entry"}},
		"from holds the target": {func(r *FactoryRepo) {
			shadow(r)
			r.JiraMovePublished.From = slices.Concat(published.From, []string{"in code review"})
		},
			[]string{"jira_move_published", `from holds its target "In Code Review"`}},
		"a done target": {func(r *FactoryRepo) { shadow(r); r.JiraMovePublished.To = "Done" },
			[]string{"jira_move_published", `"Done" is a done status`, "never closes or resolves"}},
		"a closed target": {func(r *FactoryRepo) { shadow(r); r.JiraMoveStart.To = "closed" }, []string{"jira_move_start", `"closed" is a done status`}},
		"a target of two lines": {func(r *FactoryRepo) { shadow(r); r.JiraMoveStart.To = "In\nDevelopment" },
			[]string{"jira_move_start", "one line"}},
		"a bad move while off": {func(r *FactoryRepo) { r.JiraMoveStart = FactoryJiraMove{To: "Released", From: []string{"To Do"}} },
			[]string{"jira_move_start", `"Released" is a done status`}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validFactoryConfig()
			cfg.Jira = Jira{URL: "https://example.atlassian.net", User: "alice@example.com", TokenFile: "/p/jira.token"}
			tc.edit(&cfg.Factory.Repos[0])
			wantError(t, cfg.Validate(), append([]string{"factory repo acme/app"}, tc.want...)...)
		})
	}
	cfg := validFactoryConfig()
	shadow(&cfg.Factory.Repos[0])
	wantError(t, cfg.Validate(), "factory repo acme/app", `jira_moves = "shadow" needs [jira]`)
	cfg.Jira = Jira{URL: "https://example.atlassian.net", User: "alice@example.com", TokenFile: "/p/jira.token"}
	cfg.Factory.Repos[0].JiraComment = JiraShadow
	if err := cfg.Validate(); err != nil {
		t.Errorf("shadow with both moves and [jira]: %v", err)
	}
	cfg.Factory.Repos[0].JiraMoves, cfg.Factory.Repos[0].JiraComment = JiraOff, JiraOff
	if err := cfg.Validate(); err != nil {
		t.Errorf("off: %v", err)
	}
}
