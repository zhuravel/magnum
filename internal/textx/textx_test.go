package textx

import (
	"testing"
	"unicode/utf8"
)

func TestClipKeepsAtMostNRunesWithTheEllipsisLast(t *testing.T) {
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly", 7, "exactly"},
		{"hello world", 6, "hello…"},
		{"hello world", 7, "hello…"}, // the space before the ellipsis goes
		{"hello world", 8, "hello w…"},
		{"привіт світ", 5, "прив…"},
		{"日本語のテキスト", 4, "日本語…"},
		{"anything", 1, "…"},
		{"no limit", 0, "no limit"},
		{"no limit", -1, "no limit"},
		{"", 3, ""},
	}
	for _, c := range cases {
		got := Clip(c.s, c.n)
		if got != c.want {
			t.Errorf("Clip(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
		}
		if c.n > 0 && utf8.RuneCountInString(got) > c.n {
			t.Errorf("Clip(%q, %d) = %q: more than %d runes", c.s, c.n, got, c.n)
		}
	}
}

func TestFirstLineIsTheFirstNonEmptyLineTrimmed(t *testing.T) {
	cases := map[string]string{
		"one\ntwo":           "one",
		"\n\n  two  \nthree": "two",
		"  only  ":           "only",
		"crlf\r\nnext":       "crlf",
		"":                   "",
		" \n\t\n":            "",
	}
	for s, want := range cases {
		if got := FirstLine(s); got != want {
			t.Errorf("FirstLine(%q) = %q, want %q", s, got, want)
		}
	}
}

func TestShortSHAIsSevenCharacters(t *testing.T) {
	cases := map[string]string{
		"0123456789abcdef0123456789abcdef01234567": "0123456",
		"abcdef0":  "abcdef0",
		"abc":      "abc",
		"":         "",
		"abcdef01": "abcdef0",
	}
	for s, want := range cases {
		if got := ShortSHA(s); got != want {
			t.Errorf("ShortSHA(%q) = %q, want %q", s, got, want)
		}
	}
}

func TestPluralAndCount(t *testing.T) {
	for n, want := range map[int]string{0: "files", 1: "file", 2: "files", -1: "files"} {
		if got := Plural(n, "file", "files"); got != want {
			t.Errorf("Plural(%d) = %q, want %q", n, got, want)
		}
	}
	for n, want := range map[int]string{0: "0 commits", 1: "1 commit", 12: "12 commits"} {
		if got := Count(n, "commit", "commits"); got != want {
			t.Errorf("Count(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestFoldLoginDropsCaseAtAndBotSuffix(t *testing.T) {
	cases := map[string]string{
		"zhuravel":          "zhuravel",
		" @Zhuravel ":       "zhuravel",
		"Talkable[bot]":     "talkable",
		"@talkable[BOT]":    "talkable",
		"rev-ann":           "rev-ann",
		"":                  "",
		"[bot]":             "",
		"alice@example":     "alice@example",
		"@@alice":           "@alice",
		"bob-rev[bot][bot]": "bob-rev[bot]",
	}
	for s, want := range cases {
		if got := FoldLogin(s); got != want {
			t.Errorf("FoldLogin(%q) = %q, want %q", s, got, want)
		}
	}
}
