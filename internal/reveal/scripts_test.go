package reveal

import (
	"strings"
	"testing"
)

func TestAppleScriptString(t *testing.T) {
	cases := map[string]string{
		`plain`:          `"plain"`,
		`say "hi"`:       `"say \"hi\""`,
		`back\slash`:     `"back\\slash"`,
		"two\nlines":     `"two\nlines"`,
		`'/a b/herdr' x`: `"'/a b/herdr' x"`,
	}
	for in, want := range cases {
		if got := appleScriptString(in); got != want {
			t.Errorf("appleScriptString(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestTerminalAndITermFocusScripts(t *testing.T) {
	ts := terminalFocusScript([]string{"/dev/ttys001"})
	for _, want := range []string{
		`set targetTtys to {"/dev/ttys001"}`,
		`tell application "Terminal"`,
		`if targetTtys contains (tty of t) then`,
		`set selected of t to true`,
		`set frontmost of w to true`,
		`return "miss"`,
	} {
		if !strings.Contains(ts, want) {
			t.Errorf("Terminal script missing %q:\n%s", want, ts)
		}
	}

	is := itermFocusScript([]string{"/dev/ttys001", "/dev/ttys002"})
	for _, want := range []string{
		`tell application "iTerm"`,
		`whose tty is "/dev/ttys001" or tty is "/dev/ttys002"`,
		"select w", "select tb", "select s", "activate",
		"return tty of s",
		`return "miss"`,
	} {
		if !strings.Contains(is, want) {
			t.Errorf("iTerm script missing %q:\n%s", want, is)
		}
	}
}

func TestGhosttyFocusScript(t *testing.T) {
	s := ghosttyFocusScript("herdr-magnum-session")
	if !strings.Contains(s, `if (name of t as text) is "herdr-magnum-session"`) {
		t.Errorf("must match on the marker title:\n%s", s)
	}
	if strings.Contains(s, `is "herdr"`) || strings.Contains(s, "id of t") {
		t.Errorf("must not match a bare title or terminal id:\n%s", s)
	}
	if !strings.Contains(s, "focus t") || !strings.Contains(s, `return "focused"`) {
		t.Errorf("must focus and report:\n%s", s)
	}
}

const weztermListing = `[
 {"window_id": 4, "pane_id": 1, "tty_name": "/dev/ttys001"},
 {"window_id": 5, "pane_id": 2, "tty_name": "/dev/ttys002"},
 {"window_id": 5, "pane_id": 3, "tty_name": "/dev/ttys003"},
 {"window_id": 6, "pane_id": 4}
]`

func TestSelectWezTermPane(t *testing.T) {
	if got := selectWezTermPane(weztermListing, []string{"/dev/ttys002"}); got != "2" {
		t.Errorf("pane: got %q", got)
	}
	// The first pane (in listing order) whose tty matches wins.
	if got := selectWezTermPane(weztermListing, []string{"/dev/ttys003", "/dev/ttys001"}); got != "1" {
		t.Errorf("first listed match: got %q", got)
	}
	if got := selectWezTermPane(weztermListing, []string{"/dev/ttys041"}); got != "" {
		t.Errorf("no match: got %q", got)
	}
	if got := selectWezTermWindow(weztermListing); got != "4" {
		t.Errorf("window: got %q", got)
	}
}

func TestWezTermSelectorsAreTotal(t *testing.T) {
	for _, out := range []string{"not json", "9", `{"pane_id":1}`, "", "null"} {
		if got := selectWezTermPane(out, []string{"/dev/ttys001"}); got != "" {
			t.Errorf("pane(%q) = %q", out, got)
		}
		if got := selectWezTermWindow(out); got != "" {
			t.Errorf("window(%q) = %q", out, got)
		}
	}
}
