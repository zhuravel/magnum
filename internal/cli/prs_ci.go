package cli

// The board's badges from the registry: the configured label badges
// ([board] badges). Its CI is prview.CI's, the factory screen's too.

import (
	"cmp"
	"slices"
	"strings"
	"unicode"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/tui"
)

// prsBadges are the configured badges ([board] badges) a PR's labels carry,
// in the badges' label order. A badge key matches a label whatever their
// case and leading emoji or symbols ("Flagged" matches "🚩 Flagged").
func prsBadges(labels []string, badges map[string]config.BadgeSpec) []tui.Badge {
	if len(badges) == 0 || len(labels) == 0 {
		return nil
	}
	keys := make([]string, 0, len(badges))
	for k := range badges {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int { return cmp.Compare(badgeKey(a), badgeKey(b)) })
	var out []tui.Badge
	for _, k := range keys {
		for _, l := range labels {
			if badgeKey(l) == badgeKey(k) {
				out = append(out, tui.Badge{Label: l, Text: badges[k].Text, Color: badges[k].Color})
				break
			}
		}
	}
	return out
}

// badgeKey is a label without its case, variation selectors and leading
// emoji, symbols or spaces.
func badgeKey(s string) string {
	s = strings.ReplaceAll(s, "\ufe0f", "")
	s = strings.TrimLeftFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	return strings.ToLower(strings.TrimSpace(s))
}
