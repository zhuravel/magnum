package github

import "testing"

func TestParseRef(t *testing.T) {
	const def = "talkable/talkable"
	cases := []struct {
		in, def     string
		owner, repo string
		number      int
	}{
		{"https://github.com/talkable/talkable/pull/11973", def, "talkable", "talkable", 11973},
		{"https://github.com/zhuravel/site/pull/37/files", def, "zhuravel", "site", 37},
		{"https://github.com/talkable/talkable/pull/11973/", def, "talkable", "talkable", 11973},
		{"https://github.com/talkable/talkable/pull/11973#discussion_r123", def, "talkable", "talkable", 11973},
		{"https://github.com/talkable/talkable/pull/11973?w=1", def, "talkable", "talkable", 11973},
		{"http://www.github.com/o/r.js/pull/5/commits/abc", def, "o", "r.js", 5},
		{"github.com/o/r/pull/5", def, "o", "r", 5},
		{"  zhuravel/zhuravel#728 ", def, "zhuravel", "zhuravel", 728},
		{"talkable-ui-kit#12", def, "talkable", "talkable-ui-kit", 12},
		{"#11983", def, "talkable", "talkable", 11983},
		{"11983", def, "talkable", "talkable", 11983},
		{"o/r#1", "", "o", "r", 1},
	}
	for _, c := range cases {
		owner, repo, n, err := ParseRef(c.in, c.def)
		if err != nil {
			t.Errorf("ParseRef(%q): %v", c.in, err)
			continue
		}
		if owner != c.owner || repo != c.repo || n != c.number {
			t.Errorf("ParseRef(%q) = %s/%s#%d, want %s/%s#%d", c.in, owner, repo, n, c.owner, c.repo, c.number)
		}
	}
}

func TestParseRefErrors(t *testing.T) {
	cases := []struct{ in, def string }{
		{"", "talkable/talkable"},
		{"abc", "talkable/talkable"},
		{"#0", "talkable/talkable"},
		{"-3", "talkable/talkable"},
		{"o/r#", "talkable/talkable"},
		{"o/r/s#1", "talkable/talkable"},
		{"https://github.com/o/r/issues/5", "talkable/talkable"},
		{"https://example.com/o/r/pull/5", "talkable/talkable"},
		{"https://github.com/o/r/pull/5x", "talkable/talkable"},
		{"99999999999999999999", "talkable/talkable"},
		{"4294967296", "talkable/talkable"},
		{"r#1", ""},
		{"#1", ""},
		{"1", "not-a-repo"},
		{"1", "a/b/c"},
	}
	for _, c := range cases {
		if o, r, n, err := ParseRef(c.in, c.def); err == nil {
			t.Errorf("ParseRef(%q, %q) = %s/%s#%d, want error", c.in, c.def, o, r, n)
		}
	}
}

func TestNormalizeLogin(t *testing.T) {
	cases := map[string]string{
		"talkable[bot]":                "talkable",
		"dependabot[bot]":              "dependabot",
		"chatgpt-codex-connector[bot]": "chatgpt-codex-connector",
		"zhuravel":                     "zhuravel",
		"":                             "",
		"[bot]x":                       "[bot]x",
	}
	for in, want := range cases {
		if got := NormalizeLogin(in); got != want {
			t.Errorf("NormalizeLogin(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSameLogin(t *testing.T) {
	if !SameLogin("talkable[bot]", "talkable") || !SameLogin("Zhuravel", "zhuravel") {
		t.Error("SameLogin should ignore [bot] and case")
	}
	if SameLogin("talkable", "zhuravel") || SameLogin("", "x") {
		t.Error("SameLogin matched different logins")
	}
}

func TestIsBot(t *testing.T) {
	if !IsBot("Bot", "dependabot") {
		t.Error("GraphQL Bot typename must be a bot")
	}
	if !IsBot("", "dependabot[bot]") || !IsBot("User", "talkable[bot]") {
		t.Error("REST [bot] login must be a bot")
	}
	if IsBot("User", "zhuravel") || IsBot("", "") {
		t.Error("user detected as bot")
	}
}

// An author is an account when the name and the kind match: an App's review
// in GraphQL ("zhuravel", Bot) or REST ("zhuravel[bot]") form is the App's,
// never the user's of the same name, and the other way round.
func TestIsAccount(t *testing.T) {
	for _, tc := range []struct {
		author, typ, login string
		bot, want          bool
	}{
		{"zhuravel", "Bot", "zhuravel[bot]", true, true}, {"zhuravel[bot]", "", "zhuravel[bot]", true, true},
		{"zhuravel", "Bot", "zhuravel", true, true}, // an App configured without its suffix
		{"Zhuravel", "User", "zhuravel", false, true},
		{"zhuravel", "User", "zhuravel[bot]", true, false}, {"zhuravel", "Bot", "zhuravel", false, false},
		{"alice", "User", "zhuravel", false, false}, {"", "", "", false, false},
	} {
		if got := IsAccount(tc.author, tc.typ, tc.login, tc.bot); got != tc.want {
			t.Errorf("IsAccount(%q, %q, %q, %v) = %v, want %v", tc.author, tc.typ, tc.login, tc.bot, got, tc.want)
		}
	}
}

// A bot's GraphQL login gets REST's "[bot]"; an App and the user it is named
// after are different accounts, while SameLogin (name only) matches them.
func TestAccount(t *testing.T) {
	for _, tc := range []struct{ login, typ, want string }{
		{"zhuravel", "Bot", "zhuravel[bot]"}, {"zhuravel", "User", "zhuravel"}, {"zhuravel[bot]", "Bot", "zhuravel[bot]"},
		{"zhuravel[bot]", "", "zhuravel[bot]"}, {"", "Bot", ""}, {"ghost", "Mannequin", "ghost"},
	} {
		if got := Account(tc.login, tc.typ); got != tc.want {
			t.Errorf("Account(%q, %q) = %q, want %q", tc.login, tc.typ, got, tc.want)
		}
	}
	if SameAccount("zhuravel", "zhuravel[bot]") || !SameAccount("Zhuravel[bot]", "zhuravel[BOT]") || SameAccount("", "") {
		t.Fatal("SameAccount")
	}
	if !SameLogin("zhuravel", "zhuravel[bot]") {
		t.Fatal("SameLogin compares names only")
	}
}
