package eligibility

import (
	"time"

	"github.com/zhuravel/magnum/internal/config"
)

// QuietHours reports whether now falls inside the quiet-hours window spec, a
// local "HH:MM-HH:MM" such as "01:00-07:00" (config.ParseQuietHours). The
// window includes its start and excludes its end, and wraps past midnight
// when the end is not after the start ("22:00-06:00" covers 22:00 through
// 05:59:59).
//
// The wall-clock time is read from now itself, in now's location; pass
// time.Now() to get local quiet hours. An empty spec means no quiet hours, and
// so does an invalid one (QuietHours cannot report errors; Config.Validate
// rejects one when the config loads).
func QuietHours(spec string, now time.Time) bool {
	w, ok, err := config.ParseQuietHours(spec)
	if err != nil || !ok {
		return false
	}
	m := now.Hour()*60 + now.Minute()
	if w.Start < w.End {
		return w.Start <= m && m < w.End
	}
	return m >= w.Start || m < w.End
}

// QuietHoursHold reports whether the quiet hours spec hold, at now, a round
// nobody forced. Inside them only a re-review of the judge alone starts
// (judgeAlone: a delta check of a small delta, a re-review of the head magnum
// reviewed, a reply round), one turn of a few minutes; a first review, a
// full re-review (a requested one too) and the continue of a paused turn
// wait for their end. A forced round (`magnum review`) is never held, which
// the caller checks.
func QuietHoursHold(spec string, now time.Time, judgeAlone bool) bool {
	return !judgeAlone && QuietHours(spec, now)
}
