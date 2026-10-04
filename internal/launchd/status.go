package launchd

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// State is launchd's own description of a loaded job, or NotLoaded.
type State string

const (
	// NotLoaded means launchd does not know the job (`launchctl print` exited
	// 113 or said "Could not find service"). Status also returns it next to
	// an error, as the zero answer when the state could not be read.
	NotLoaded State = "not loaded"
	// Running means the job has a live process.
	Running State = "running"
	// Unknown means the job is loaded but `print` output had no state line.
	Unknown State = "unknown"
	// Other values are passed through verbatim from launchd, for example
	// "not running" and "spawn scheduled".
)

// Info is the parsed result of `launchctl print gui/<uid>/<label>`. It
// deliberately carries no raw output: `print` echoes the job's arguments and
// environment.
type Info struct {
	State State
	// PID is the running process, 0 when none.
	PID int
	// Runs is how many times launchd has started the job.
	Runs int
	// LastExitCode is meaningful only when Exited is true; launchd prints
	// "(never exited)" or a non-numeric exit reason otherwise.
	LastExitCode int
	Exited       bool
}

// Loaded reports whether launchd knows the job.
func (s Info) Loaded() bool { return s.State != NotLoaded }

// notFoundExit is the exit code `launchctl print` uses for an unknown service.
const notFoundExit = 113

// Status runs `launchctl print gui/<uid>/<label>`. Only launchd's "no such
// service" answer (exit 113, or "Could not find service" on stderr) yields
// State NotLoaded and a nil error. Every other failure (a domain or
// permission error, a missing binary, a timeout) is returned as an error,
// alongside NotLoaded as the zero Info: the job may well still be loaded, so
// callers such as Bootout and Uninstall must not read it as "gone".
func Status(ctx context.Context, run execx.Runner, uid int, label string) (Info, error) {
	res, err := run.Run(ctx, execx.Cmd{
		Name:    "launchctl",
		Args:    []string{"print", target(uid, label)},
		Timeout: 15 * time.Second,
		Label:   "launchctl print",
	})
	if err != nil {
		var ee *execx.ExitError
		if errors.As(err, &ee) && isNotFound(ee, res) {
			return Info{State: NotLoaded}, nil
		}
		return Info{State: NotLoaded}, fmt.Errorf("launchctl print %s: %w", target(uid, label), err)
	}
	return parsePrint(string(res.Stdout)), nil
}

// isNotFound reports whether a failed `launchctl print` said the service does
// not exist.
func isNotFound(ee *execx.ExitError, res execx.Result) bool {
	return ee.Code == notFoundExit ||
		strings.Contains(ee.Stderr, "Could not find service") ||
		strings.Contains(string(res.Stderr), "Could not find service")
}

// parsePrint reads the first-level "key = value" lines of the service block.
// Nested blocks (environment, LWCR, endpoints...) are indented deeper and are
// skipped, so a "pid =" inside them cannot be mistaken for the job's own.
func parsePrint(out string) Info {
	st := Info{State: Unknown}
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	var topIndent string
	seenHeader := false
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !seenHeader {
			seenHeader = true // "gui/<uid>/<label> = {"
			continue
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		if topIndent == "" {
			topIndent = indent
		}
		if indent != topIndent {
			continue
		}
		key, val, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "state":
			if val != "" {
				st.State = State(val)
			}
		case "pid":
			if n, err := strconv.Atoi(val); err == nil {
				st.PID = n
			}
		case "runs":
			if n, err := strconv.Atoi(val); err == nil {
				st.Runs = n
			}
		case "last exit code":
			if n, ok := leadingInt(val); ok {
				st.LastExitCode, st.Exited = n, true
			}
		}
	}
	return st
}

// leadingInt parses an optional minus sign and the digits that follow ("78",
// "78: EX_CONFIG"); "(never exited)" and the like are not numbers.
func leadingInt(s string) (int, bool) {
	end := 0
	for end < len(s) && (s[end] >= '0' && s[end] <= '9' || end == 0 && s[end] == '-') {
		end++
	}
	n, err := strconv.Atoi(s[:end])
	return n, err == nil
}
