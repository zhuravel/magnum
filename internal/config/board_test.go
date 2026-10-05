package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/paths"
)

// A badge is a text or { text, color }; colors are the board's names.
func TestBoardBadgesTakeATextOrATextWithAColor(t *testing.T) {
	user := filepath.Join(t.TempDir(), "config.toml")
	write := func(body string) {
		if err := os.WriteFile(user, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("[board]\nbadges = { \"Flagged\" = \"🚩\", \"Schema Migration\" = { text = \"\\uf1c0\", color = \"Yellow\" } }\n")
	cfg, err := Load(paths.Layout{Home: t.TempDir(), UserConfig: user}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Board.Badges["Flagged"]; got != (BadgeSpec{Text: "🚩"}) {
		t.Fatalf("flagged %+v", got)
	}
	if got := cfg.Board.Badges["Schema Migration"]; got != (BadgeSpec{Text: "\uf1c0", Color: "yellow"}) {
		t.Fatalf("schema migration %+v", got)
	}
	for body, want := range map[string]string{
		"[board]\nbadges = { \"x\" = { text = \"a\", color = \"pink\" } }\n": "color \"pink\" is not one of",
		"[board]\nbadges = { \"x\" = { text = \"a\", size = \"big\" } }\n":   "badge key \"size\" is unknown",
		"[board]\nbadges = { \"x\" = \"\" }\n":                               "the badge text is empty",
	} {
		write(body)
		if _, err := Load(paths.Layout{Home: t.TempDir(), UserConfig: user}, ""); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", body, err, want)
		}
	}
}

// [board] recent_closed is the board's window for merged and closed PRs:
// 24h by default, "0" turns the section off, a negative one is refused.
func TestBoardRecentClosedWindow(t *testing.T) {
	user := filepath.Join(t.TempDir(), "config.toml")
	load := func(body string) (*Config, error) {
		t.Helper()
		if err := os.WriteFile(user, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(paths.Layout{Home: t.TempDir(), UserConfig: user}, "")
	}
	if got := Defaults().Board.RecentClosed.Duration; got != 24*time.Hour {
		t.Fatalf("built-in recent_closed = %v, want 24h", got)
	}
	cfg, err := load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Board.RecentClosed.Duration; got != 24*time.Hour {
		t.Fatalf("default recent_closed = %v, want 24h", got)
	}
	for body, want := range map[string]time.Duration{
		"[board]\nrecent_closed = \"0\"\n":  0,
		"[board]\nrecent_closed = \"3d\"\n": 72 * time.Hour,
		"[board]\nrecent_closed = \"6h\"\n": 6 * time.Hour,
	} {
		cfg, err := load(body)
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		if got := cfg.Board.RecentClosed.Duration; got != want {
			t.Errorf("%q: recent_closed = %v, want %v", body, got, want)
		}
	}
	if _, err := load("[board]\nrecent_closed = \"-1h\"\n"); err == nil || !strings.Contains(err.Error(), "board.recent_closed must not be negative") {
		t.Fatalf("negative recent_closed: %v", err)
	}
}
