package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/paths"
)

// The first issue key in a title, of any configured tracker, names the
// issue; a key is whole ("XPS-1" and "PS-1a" are not PS-1).
func TestIssue(t *testing.T) {
	b := Board{Trackers: []string{
		"https://linear.app/example/issue/DMA-{num}",
		"https://example.atlassian.net/browse/PS-{num}",
		"https://example.atlassian.net/browse/PR-{num}",
	}}
	trackers := parseTrackers(b.Trackers)
	if len(trackers) != 3 || trackers[0].Prefix != "DMA-" {
		t.Fatalf("trackers %+v", trackers)
	}
	for title, want := range map[string]string{
		"[DMA-458] Raise DNS notifications":          "https://linear.app/example/issue/DMA-458",
		"PS-38553: fix the widget":                   "https://example.atlassian.net/browse/PS-38553",
		"[PR-26788] Campaign snapshots":              "https://example.atlassian.net/browse/PR-26788",
		"Follow-up of PR-1234 for PS-38553":          "https://example.atlassian.net/browse/PR-1234",
		"PS-2 then DMA-1":                            "https://example.atlassian.net/browse/PS-2",
		"XPS-12 and PS-12a and ps-12 are not issues": "",
		"No issue here":                              "",
	} {
		key, url := Issue(title, trackers)
		if url != want || (want != "" && !strings.HasSuffix(want, "/"+key)) {
			t.Errorf("%q: %q %q, want %q", title, key, url, want)
		}
	}
	if key, url := Issue("PS-1", nil); key != "" || url != "" {
		t.Fatalf("no trackers: %q %q", key, url)
	}
}

// A template is an http(s) URL with {num} once, after the key's prefix.
func TestTrackersValidate(t *testing.T) {
	b := Board{Trackers: []string{
		"https://example.atlassian.net/browse/PS-{num}",
		"example.atlassian.net/browse/PS-{num}",
		"https://example.atlassian.net/browse/PS-",
		"https://example.atlassian.net/browse/{num}",
		"https://example.atlassian.net/browse/PS-{num}?n={num}",
	}}
	got := b.validate()
	if len(got) != 4 {
		t.Fatalf("problems %q, want 4", got)
	}
	for i, want := range []string{"not an http(s) URL", "must contain {num} once", "must follow the issue key's prefix", "must contain {num} once"} {
		if !strings.Contains(got[i], want) || !strings.HasPrefix(got[i], "board: trackers: ") {
			t.Errorf("problem %d = %q, want %q", i, got[i], want)
		}
	}
	if len(parseTrackers(b.Trackers)) != 1 {
		t.Fatal("only the valid template is used")
	}
}

// config.full.example.toml is a working user config: it loads over the
// built-in defaults, and its [board] badges and trackers are valid.
func TestFullExampleLoads(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "config.full.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	user := filepath.Join(home, "user.toml")
	if err := os.WriteFile(user, src, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(paths.Layout{Home: home, UserConfig: user}, "")
	if err != nil {
		t.Fatalf("load config.full.example.toml: %v", err)
	}
	if len(cfg.Board.Badges) == 0 || len(parseTrackers(cfg.Board.Trackers)) != len(cfg.Board.Trackers) || len(cfg.Board.Trackers) == 0 {
		t.Fatalf("[board] = %+v", cfg.Board)
	}
	if problems := cfg.Board.validate(); len(problems) > 0 {
		t.Fatalf("[board]: %q", problems)
	}
}

// Two owners may use one issue key prefix in different trackers: a
// [[watch]]'s trackers win over [board]'s for its PRs, a [[repo]]'s over
// its watch's, prefix by prefix; the prefixes a level does not name come
// from the broader ones. Templates that do not parse fail validation where
// they are configured.
func TestTrackersFor(t *testing.T) {
	c := &Config{
		Board: Board{Trackers: []string{"https://example.atlassian.net/browse/PR-{num}", "https://example.atlassian.net/browse/PS-{num}"}},
		Watches: []Watch{
			{Owner: "talkable", Include: []string{"*"}},
			{Owner: "example", Include: []string{"*"}, Trackers: []string{"https://linear.app/example/issue/PR-{num}"}},
		},
		Repos: []Repo{{Repo: "example/legacy", Trackers: []string{"https://example.youtrack.cloud/issue/PR-{num}"}}},
	}
	for _, tc := range []struct{ repo, title, want string }{
		{"talkable/talkable", "[PR-12] x", "https://example.atlassian.net/browse/PR-12"},
		{"example/app", "[PR-12] x", "https://linear.app/example/issue/PR-12"},
		{"example/app", "[PS-7] x", "https://example.atlassian.net/browse/PS-7"}, // not named by the watch: [board]'s
		{"example/legacy", "[PR-12] x", "https://example.youtrack.cloud/issue/PR-12"},
		{"other/repo", "[PR-12] x", "https://example.atlassian.net/browse/PR-12"}, // no watch: [board]'s
	} {
		if _, url := Issue(tc.title, c.TrackersFor(tc.repo)); url != tc.want {
			t.Errorf("%s %q: %q, want %q", tc.repo, tc.title, url, tc.want)
		}
	}

	c.Watches[1].Trackers = append(c.Watches[1].Trackers, "linear.app/example/issue/ENG-{num}")
	c.Repos[0].Trackers = append(c.Repos[0].Trackers, "https://example.youtrack.cloud/issue/{num}")
	err := c.Validate()
	for _, want := range []string{"watch example: trackers: \"linear.app/example/issue/ENG-{num}\" is not an http(s) URL",
		"repo example/legacy: trackers: \"https://example.youtrack.cloud/issue/{num}\": {num} must follow the issue key's prefix"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Validate: %v\nwant %s", err, want)
		}
	}
}
