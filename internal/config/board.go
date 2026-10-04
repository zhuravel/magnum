package config

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Board tunes the PR board ([board]).
type Board struct {
	// Badges map a GitHub label to what the board shows for it, e.g.
	// { "Flagged" = "🚩" }: the text goes before the title, the card shows
	// it with the label, the summary counts it. A key matches a label
	// whatever their case and leading emoji or symbols ("Flagged" matches
	// "🚩 Flagged").
	Badges map[string]string `toml:"badges"`
}

// validate checks the badges: a label and a short text each.
func (b Board) validate() []string {
	var out []string
	for label, text := range b.Badges {
		switch {
		case strings.TrimSpace(label) == "":
			out = append(out, "board.badges: a badge needs a label name")
		case strings.TrimSpace(text) == "":
			out = append(out, fmt.Sprintf("board.badges[%q]: the badge text is empty", label))
		case utf8.RuneCountInString(text) > 8:
			out = append(out, fmt.Sprintf("board.badges[%q]: %q is longer than 8 characters (a badge is an emoji, an icon or a short word)", label, text))
		}
	}
	return out
}
