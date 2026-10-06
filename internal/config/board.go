package config

import (
	"fmt"
	"regexp"
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
	// Trackers link a PR to its issue: URL templates with {num} right after
	// the issue key's prefix ("https://linear.app/example/issue/DMA-{num}",
	// "https://example.atlassian.net/browse/PS-{num}"). The first issue key
	// of any of them in a PR's title ("[PS-38553] …") names the issue: t on
	// the board opens it, the card shows it. A [[watch]] or [[repo]] block's
	// trackers win for its PRs (Config.TrackersFor).
	Trackers []string `toml:"trackers"`
	// RecentClosed is how long a merged or closed PR stays on the board, in
	// its own section after the open PRs (merged_at, else closed_at, within
	// it); 0 turns the section off. `magnum prs --all` lists every closed PR
	// whatever it says.
	RecentClosed Duration `toml:"recent_closed"`
	// Shimmer slides a rainbow across the state cell of a PR magnum approved
	// that GitHub still blocks on the operator's approval ("✔ needs you",
	// "✔ lift your ✗") while one is on screen; false keeps it still. A
	// terminal without colors (NO_COLOR) shows it in reverse video either way.
	Shimmer bool `toml:"shimmer"`
}

// PlaceholderNum is the issue number in a [board] trackers template.
const PlaceholderNum = "{num}"

// trackerPrefix is the issue key's prefix right before {num}: a letter,
// then letters, digits or "_", then "-" ("DMA-", "PS-").
var trackerPrefix = regexp.MustCompile(`([A-Za-z][A-Za-z0-9_]*-)` + regexp.QuoteMeta(PlaceholderNum))

// Tracker is one [board] trackers template, ready to find its issue keys.
type Tracker struct {
	Prefix string // the issue key before the number: "DMA-"
	URL    string // the template
	key    *regexp.Regexp
}

// ParseTracker reads a [board] trackers template: an http(s) URL with
// {num} once, right after the issue key's prefix.
func ParseTracker(tmpl string) (Tracker, error) {
	u := strings.TrimSpace(tmpl)
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return Tracker{}, fmt.Errorf("%q is not an http(s) URL", tmpl)
	}
	if strings.Count(u, PlaceholderNum) != 1 {
		return Tracker{}, fmt.Errorf("%q must contain %s once", tmpl, PlaceholderNum)
	}
	m := trackerPrefix.FindStringSubmatch(u)
	if m == nil {
		return Tracker{}, fmt.Errorf("%q: %s must follow the issue key's prefix, as in PS-%s", tmpl, PlaceholderNum, PlaceholderNum)
	}
	// A whole key only: "XPS-1" and "PS-1a" are not PS-1.
	return Tracker{Prefix: m[1], URL: u, key: regexp.MustCompile(`\b` + regexp.QuoteMeta(m[1]) + `([0-9]+)\b`)}, nil
}

// parseTrackers are the templates that parse, in order (Validate reports
// the others).
func parseTrackers(templates []string) []Tracker {
	var out []Tracker
	for _, tmpl := range templates {
		if t, err := ParseTracker(tmpl); err == nil {
			out = append(out, t)
		}
	}
	return out
}

// trackerProblems are the templates that do not parse, labelled by where
// they are configured ("board", "watch talkable", "repo talkable/x").
func trackerProblems(where string, templates []string) []string {
	var out []string
	for _, tmpl := range templates {
		if _, err := ParseTracker(tmpl); err != nil {
			out = append(out, where+": trackers: "+err.Error())
		}
	}
	return out
}

// Issue finds the first issue key in title that one of trackers knows
// (leftmost; at one position, the earlier tracker) and returns the key
// ("PS-38553") and its URL; "" and "" when there is none.
func Issue(title string, trackers []Tracker) (key, url string) {
	at := -1
	for _, t := range trackers {
		loc := t.key.FindStringSubmatchIndex(title)
		if loc == nil || (at >= 0 && loc[0] >= at) {
			continue
		}
		at = loc[0]
		num := title[loc[2]:loc[3]]
		key, url = t.Prefix+num, strings.Replace(t.URL, PlaceholderNum, num, 1)
	}
	return key, url
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

// validate checks the badges (a label, a short text and a known color each),
// the trackers and recent_closed.
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
	if b.RecentClosed.Duration < 0 {
		out = append(out, "board.recent_closed must not be negative (0 turns the recently closed section off)")
	}
	return append(out, trackerProblems("board", b.Trackers)...)
}
