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
