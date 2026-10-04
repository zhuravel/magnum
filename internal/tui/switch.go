package tui

import "errors"

// The errors RunDashboard and RunPRBoard return when the user pressed tab to
// switch to the other screen: the caller runs that screen next (with the
// same source and actions it already holds). A caller that does not switch
// treats them as any other error, so it sees them only if it asked for
// both screens.
var (
	ErrSwitchToBoard     = errors.New("tui: switch to the PR board")
	ErrSwitchToDashboard = errors.New("tui: switch to the status dashboard")
)
