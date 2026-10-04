package tui

import (
	"os"

	"github.com/charmbracelet/x/term"
)

// IsTerminal reports whether f is a terminal (the termios ioctl answers), so
// a caller picks a screen of this package over plain-text output. /dev/null,
// pipes and regular files are not terminals.
func IsTerminal(f *os.File) bool { return f != nil && term.IsTerminal(f.Fd()) }
