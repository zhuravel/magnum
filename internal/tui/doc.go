// Package tui holds magnum's interactive terminal screens, built on Bubble
// Tea: the status dashboard (RunDashboard), the PR board (RunPRBoard), the
// PR picker (RunPicker), the cleanup plan review (RunCleanupPlan) and the
// read-only pane mirror (RunWatch).
//
// Every screen takes plain data structs and small interfaces defined here,
// so the cli adapts its own types into them; the plain-text renderings stay
// in the cli for output that is not a terminal (IsTerminal tells them
// apart). Run* functions block until the user leaves the screen or ctx ends;
// an ended ctx is not an error (the screen just closes). The dashboard and
// the PR board switch to each other on tab: they return ErrSwitchToBoard or
// ErrSwitchToDashboard and the caller runs the other screen.
//
// Built on Charm v2 (charm.land/bubbletea/v2, bubbles/v2, lipgloss/v2).
// Styles use the 16 ANSI colors only, in a light and a dark variant picked
// from the background the terminal reports (tea.RequestBackgroundColor);
// every state that color shows is also spelled out in text, so the screens
// read the same on a monochrome terminal. No screen needs a mouse, but the
// board and the dashboard take one (mouse.go): the wheel scrolls, a click
// selects, a double click opens, a heading click sorts (the board), a
// heading gap drags a column wider and a right click opens the row's
// actions, each through its key's path; m turns it off and on.
package tui
