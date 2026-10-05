package reveal

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
)

var appleScriptEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// appleScriptString renders s as an AppleScript string literal.
func appleScriptString(s string) string { return `"` + appleScriptEscaper.Replace(s) + `"` }

// terminalFocusScript selects the Terminal.app tab whose tty is one of ttys and
// prints that tty, or "miss".
func terminalFocusScript(ttys []string) string {
	quoted := make([]string, len(ttys))
	for i, t := range ttys {
		quoted[i] = appleScriptString(t)
	}
	return `set targetTtys to {` + strings.Join(quoted, ", ") + `}
tell application "Terminal"
  repeat with w in windows
    repeat with t in tabs of w
      if targetTtys contains (tty of t) then
        set selected of t to true
        set frontmost of w to true
        activate
        return tty of t
      end if
    end repeat
  end repeat
  return "miss"
end tell`
}

// itermFocusScript selects the iTerm2 session whose tty is one of ttys and
// prints that tty, or "miss".
func itermFocusScript(ttys []string) string {
	clauses := make([]string, len(ttys))
	for i, t := range ttys {
		clauses[i] = "tty is " + appleScriptString(t)
	}
	return `tell application "iTerm"
  set matches to every session of every tab of every window whose ` + strings.Join(clauses, " or ") + `
  repeat with windowIndex from 1 to count matches
    set windowMatches to item windowIndex of matches
    repeat with tabIndex from 1 to count windowMatches
      set tabMatches to item tabIndex of windowMatches
      if (count tabMatches) > 0 then
        set w to item windowIndex of windows
        set tb to item tabIndex of tabs of w
        set s to item 1 of tabMatches
        select w
        select tb
        select s
        activate
        return tty of s
      end if
    end repeat
  end repeat
  return "miss"
end tell`
}

// ghosttyFocusScript focuses the Ghostty terminal whose title is the marker
// that `herdr terminal title set` just wrote; it prints "focused" or "miss".
func ghosttyFocusScript(title string) string {
	return `tell application "Ghostty"
  ignoring case
    repeat 5 times
      repeat with t in terminals
        if (name of t as text) is ` + appleScriptString(title) + ` then
          focus t
          return "focused"
        end if
      end repeat
      delay 0.02
    end repeat
  end ignoring
  return "miss"
end tell`
}

// wezPane is one entry of `wezterm cli list --format json`.
type wezPane struct {
	WindowID *int   `json:"window_id"`
	PaneID   *int   `json:"pane_id"`
	TTYName  string `json:"tty_name"`
}

// parseWezTermPanes decodes the listing; ok is false for anything that is not
// a JSON array so every selector stays total over unexpected output.
func parseWezTermPanes(output string) ([]wezPane, bool) {
	var panes []wezPane
	if err := json.Unmarshal([]byte(output), &panes); err != nil || panes == nil {
		return nil, false
	}
	return panes, true
}

// selectWezTermPane returns the id of the first listed pane whose tty is one of
// ttys, or "".
func selectWezTermPane(output string, ttys []string) string {
	panes, _ := parseWezTermPanes(output)
	for _, p := range panes {
		if p.TTYName != "" && p.PaneID != nil && slices.Contains(ttys, p.TTYName) {
			return strconv.Itoa(*p.PaneID)
		}
	}
	return ""
}

// selectWezTermWindow returns the window id of the first listed pane, or "".
func selectWezTermWindow(output string) string {
	panes, _ := parseWezTermPanes(output)
	for _, p := range panes {
		if p.WindowID != nil {
			return strconv.Itoa(*p.WindowID)
		}
	}
	return ""
}
