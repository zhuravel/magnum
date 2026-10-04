package eligibility

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// QuietHours reports whether now falls inside the quiet-hours window spec, a
// local "HH:MM-HH:MM" such as "01:00-07:00". The window includes its start and
// excludes its end, and wraps past midnight when the end is not after the start
// ("22:00-06:00" covers 22:00 through 05:59:59).
//
// The wall-clock time is read from now itself, in now's location; pass
// time.Now() to get local quiet hours. An empty spec means no quiet hours, and
// so does an invalid one (QuietHours cannot report errors; use
// ValidateQuietHours when loading the config).
func QuietHours(spec string, now time.Time) bool {
	w, ok, err := parseQuietHours(spec)
	if err != nil || !ok {
		return false
	}
	m := now.Hour()*60 + now.Minute()
	if w.start < w.end {
		return w.start <= m && m < w.end
	}
	return m >= w.start || m < w.end
}

// ValidateQuietHours checks a daemon.quiet_hours value: empty is valid (no
// quiet hours), otherwise it must be "HH:MM-HH:MM" (00:00 through 23:59) with
// different start and end.
func ValidateQuietHours(spec string) error {
	_, _, err := parseQuietHours(spec)
	return err
}

// quietWindow holds minutes since midnight.
type quietWindow struct{ start, end int }

// parseQuietHours returns ok=false (and no error) for a blank spec.
func parseQuietHours(spec string) (w quietWindow, ok bool, err error) {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		return quietWindow{}, false, nil
	}
	from, to, found := strings.Cut(trimmed, "-")
	if !found {
		return quietWindow{}, false, fmt.Errorf("quiet_hours %q: want HH:MM-HH:MM", spec)
	}
	if w.start, err = parseClock(from); err != nil {
		return quietWindow{}, false, fmt.Errorf("quiet_hours %q: start: %w", spec, err)
	}
	if w.end, err = parseClock(to); err != nil {
		return quietWindow{}, false, fmt.Errorf("quiet_hours %q: end: %w", spec, err)
	}
	if w.start == w.end {
		return quietWindow{}, false, fmt.Errorf("quiet_hours %q: %w", spec, errEmptyWindow)
	}
	return w, true, nil
}

var errEmptyWindow = errors.New("start and end are the same, so the window is empty")

// parseClock reads "H:MM" or "HH:MM" as minutes since midnight. Anything else,
// including a second "-" or a seconds field, is an error.
func parseClock(s string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%q is not HH:MM: %w", strings.TrimSpace(s), err)
	}
	return t.Hour()*60 + t.Minute(), nil
}
