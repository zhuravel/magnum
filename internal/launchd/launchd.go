// Package launchd writes and manages magnum's LaunchAgent (label
// zhuravel.magnum): rendering the plist, installing it into the user's GUI
// domain, and querying, restarting and removing the job.
//
// All launchctl calls go through execx.Runner. The one file this package
// writes is ~/Library/LaunchAgents/<label>.plist; magnum keeps everything else
// where internal/paths puts it (~/.config/magnum, ~/.local/share/magnum and
// ~/.local/state/magnum, or <checkout>/state under MAGNUM_HOME).
//
// The plist is world-readable (0644), so never put secrets in Options.Env. The
// daemon gets its credentials from mise (`mise -C <repo> exec -- ...`) instead.
package launchd

import (
	"bytes"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

// DefaultLabel is the launchd label of the magnum daemon.
const DefaultLabel = "zhuravel.magnum"

// LogName is the file (in state/logs) that receives the job's stdout and
// stderr: launchd's own capture, kept apart from the daemon.log the daemon
// writes and rotates itself (two writers on one file would interleave and
// fight over rotation). Only crashes and pre-logger output end up here.
const LogName = "launchd.log"

// LogPath is LogName inside a logs directory (paths.Layout.Logs()).
func LogPath(logsDir string) string { return filepath.Join(logsDir, LogName) }

// Options describes the LaunchAgent job.
type Options struct {
	// Label is the job label; required. By convention it is also the plist's
	// file name stem (see AgentPath).
	Label string
	// ProgramArguments is the full argv, including argv[0]; required.
	ProgramArguments []string
	// WorkingDir sets WorkingDirectory when non-empty.
	WorkingDir string
	// Env becomes EnvironmentVariables when non-empty, rendered in sorted key order.
	Env map[string]string
	// StdoutPath and StderrPath set StandardOutPath / StandardErrorPath when
	// non-empty. launchd creates the file but not its parent directory.
	StdoutPath, StderrPath string
	// KeepAlive restarts the job when it exits abnormally. It renders
	// KeepAlive = { SuccessfulExit = false }: a clean exit (the daemon exits 0
	// on SIGTERM and when another instance already holds the lock) is not
	// restarted, so launchd does not spin. False omits the key.
	KeepAlive bool
	// ThrottleSeconds sets ThrottleInterval (minimum seconds between launches)
	// when greater than zero; launchd's own default is 10.
	ThrottleSeconds int
}

// ExitTimeOut is the ExitTimeOut value (seconds) every plist carries: how long
// launchd waits after SIGTERM before SIGKILLing the job. launchd's own default
// is 5 s, far shorter than the daemon needs to cancel its rounds and kill the
// subprocesses it started (the CLI budgets 30 s for a SIGTERMed daemon), so a
// plain stop or restart would otherwise SIGKILL it mid-cleanup.
const ExitTimeOut = 45

// AgentPath returns the per-user LaunchAgent plist path for a label.
func AgentPath(home, label string) string {
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist")
}

const plistHeader = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`

// Plist renders opts as an XML property list. The output is deterministic:
// keys appear in a fixed order and Env is sorted, so unchanged options produce
// byte-identical files. RunAtLoad is always true and ExitTimeOut is always
// ExitTimeOut seconds.
func Plist(opts Options) []byte {
	var b bytes.Buffer
	b.WriteString(plistHeader)

	key := func(k string) { fmt.Fprintf(&b, "\t<key>%s</key>\n", escapeXML(k)) }
	str := func(k, v string) {
		key(k)
		fmt.Fprintf(&b, "\t<string>%s</string>\n", escapeXML(v))
	}

	str("Label", opts.Label)
	key("ProgramArguments")
	b.WriteString("\t<array>\n")
	for _, a := range opts.ProgramArguments {
		fmt.Fprintf(&b, "\t\t<string>%s</string>\n", escapeXML(a))
	}
	b.WriteString("\t</array>\n")
	if opts.WorkingDir != "" {
		str("WorkingDirectory", opts.WorkingDir)
	}
	if len(opts.Env) > 0 {
		key("EnvironmentVariables")
		b.WriteString("\t<dict>\n")
		for _, k := range slices.Sorted(maps.Keys(opts.Env)) {
			fmt.Fprintf(&b, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", escapeXML(k), escapeXML(opts.Env[k]))
		}
		b.WriteString("\t</dict>\n")
	}
	key("RunAtLoad")
	b.WriteString("\t<true/>\n")
	if opts.KeepAlive {
		key("KeepAlive")
		b.WriteString("\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	}
	if opts.ThrottleSeconds > 0 {
		key("ThrottleInterval")
		fmt.Fprintf(&b, "\t<integer>%d</integer>\n", opts.ThrottleSeconds)
	}
	key("ExitTimeOut")
	fmt.Fprintf(&b, "\t<integer>%d</integer>\n", ExitTimeOut)
	if opts.StdoutPath != "" {
		str("StandardOutPath", opts.StdoutPath)
	}
	if opts.StderrPath != "" {
		str("StandardErrorPath", opts.StderrPath)
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

// escapeXML escapes text for use between tags. Characters XML 1.0 cannot
// represent at all (most control characters, invalid UTF-8) become U+FFFD so
// the document stays well-formed.
func escapeXML(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		case r == '\t' || r == '\n' || r == '\r',
			r >= 0x20 && r <= 0xD7FF && r != utf8.RuneError,
			r >= 0xE000 && r <= 0xFFFD && r != utf8.RuneError,
			r >= 0x10000 && r <= 0x10FFFF:
			b.WriteRune(r)
		default:
			b.WriteRune(utf8.RuneError)
		}
	}
	return b.String()
}
