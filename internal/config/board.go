package config

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// Board tunes the PR board ([board]).
type Board struct {
	// Badges map a GitHub label to what the board shows for it: a text
	// ({ "Flagged" = "🚩" }) or a text with a color ({ "Schema Migration" =
	// { text = "\uf1c0", color = "yellow" } }). The badge goes before the
	// title, the card shows it with the label, the summary counts it. A key
	// matches a label whatever their case and leading emoji or symbols
	// ("Flagged" matches "🚩 Flagged").
	Badges map[string]BadgeSpec `toml:"badges"`
}

// BadgeSpec is one badge: its text and, optionally, its color.
type BadgeSpec struct {
	Text  string
	Color string // BadgeColors; "" = the terminal's (an emoji keeps its own)
}

// BadgeColors are the colors a badge may ask for: the board's ANSI colors.
var BadgeColors = []string{"red", "green", "yellow", "blue", "magenta", "cyan", "gray", "grey"}

// UnmarshalTOML reads a badge written as a text or as { text, color }.
func (b *BadgeSpec) UnmarshalTOML(v any) error {
	switch x := v.(type) {
	case string:
		b.Text = x
		return nil
	case map[string]any:
		for k, val := range x {
			s, ok := val.(string)
			if !ok {
				return fmt.Errorf("badge %s must be a string", k)
			}
			switch k {
			case "text":
				b.Text = s
			case "color":
				b.Color = strings.ToLower(strings.TrimSpace(s))
			default:
				return fmt.Errorf("badge key %q is unknown (text, color)", k)
			}
		}
		return nil
	}
	return fmt.Errorf("a badge is a text or { text = \"…\", color = \"…\" }, got %T", v)
}

// validate checks the badges: a label, a short text and a known color each.
func (b Board) validate() []string {
	var out []string
	for label, spec := range b.Badges {
		switch {
		case strings.TrimSpace(label) == "":
			out = append(out, "board.badges: a badge needs a label name")
		case strings.TrimSpace(spec.Text) == "":
			out = append(out, fmt.Sprintf("board.badges[%q]: the badge text is empty", label))
		case utf8.RuneCountInString(spec.Text) > 8:
			out = append(out, fmt.Sprintf("board.badges[%q]: %q is longer than 8 characters (a badge is an emoji, an icon or a short word)", label, spec.Text))
		case spec.Color != "" && !slices.Contains(BadgeColors, spec.Color):
			out = append(out, fmt.Sprintf("board.badges[%q]: color %q is not one of %s", label, spec.Color, strings.Join(BadgeColors[:7], ", ")))
		}
	}
	return out
}
