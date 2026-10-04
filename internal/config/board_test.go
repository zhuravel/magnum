package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
