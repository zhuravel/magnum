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
	trackers := b.ParsedTrackers()
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
		if !strings.Contains(got[i], want) || !strings.HasPrefix(got[i], "board.trackers: ") {
			t.Errorf("problem %d = %q, want %q", i, got[i], want)
		}
	}
	if len(b.ParsedTrackers()) != 1 {
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
	if len(cfg.Board.Badges) == 0 || len(cfg.Board.ParsedTrackers()) != len(cfg.Board.Trackers) || len(cfg.Board.Trackers) == 0 {
		t.Fatalf("[board] = %+v", cfg.Board)
	}
	if problems := cfg.Board.validate(); len(problems) > 0 {
		t.Fatalf("[board]: %q", problems)
	}
}
