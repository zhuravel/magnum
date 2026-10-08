package usage

import "time"

// Pace is how fast a rate-limit window's budget is being spent: the share
// used against the share of the window elapsed.
type Pace struct {
	Used     float64   // percent of the budget used, 0–100
	Start    time.Time // when the window began: ResetsAt minus its length
	ResetsAt time.Time
	Now      time.Time
}

// PaceOf is the pace at now of a window windowMinutes long that resets at
// resetsAt, with used percent of it used. ok is false when the length or the
// reset is unknown (<= 0, zero) or now is not inside the window (before its
// start, or at/after the reset).
func PaceOf(used float64, windowMinutes int, resetsAt, now time.Time) (Pace, bool) {
	if windowMinutes <= 0 || resetsAt.IsZero() {
		return Pace{}, false
	}
	start := resetsAt.Add(-time.Duration(windowMinutes) * time.Minute)
	if now.Before(start) || !now.Before(resetsAt) {
		return Pace{}, false
	}
	return Pace{Used: used, Start: start, ResetsAt: resetsAt, Now: now}, true
}

// Elapsed is the share of the window elapsed, 0–100.
func (p Pace) Elapsed() float64 {
	length := p.ResetsAt.Sub(p.Start)
	if length <= 0 {
		return 0
	}
	return 100 * float64(p.Now.Sub(p.Start)) / float64(length)
}

// Ratio is Used over Elapsed: 1 spends the whole budget exactly at the reset,
// 2.7 runs out at 37% of the window. 0 when nothing elapsed.
func (p Pace) Ratio() float64 {
	elapsed := p.Elapsed()
	if elapsed <= 0 {
		return 0
	}
	return p.Used / elapsed
}

// sameWindow is how far apart two readings' reset times of one window may
// be: Codex's reset time of a window jitters by a second between readings.
const sameWindow = time.Hour

// PaceSince is the pace of window cur from from to now: the share of its
// budget used since past, Codex's reading at or before from (which stands
// for the usage at from), over the share of the window between from and now.
// The window of past as long as cur (primary or secondary) is compared, and
// it is cur's window when its reset is within an hour of cur's. ok is false
// when past has no such window or it is another one (a reset in between: the
// budget before it is not comparable), cur's length or reset is unknown, its
// window began after from or ended by now, the used share went down, or now
// is not after from.
func PaceSince(cur Window, past Snapshot, from, now time.Time) (float64, bool) {
	if cur.WindowMinutes <= 0 || cur.ResetsAt.IsZero() || !now.After(from) || !now.Before(cur.ResetsAt) {
		return 0, false
	}
	length := time.Duration(cur.WindowMinutes) * time.Minute
	if cur.ResetsAt.Add(-length).After(from) {
		return 0, false
	}
	prev, ok := past.windowOf(cur.WindowMinutes)
	if !ok || prev.ResetsAt.IsZero() || prev.ResetsAt.Sub(cur.ResetsAt).Abs() > sameWindow {
		return 0, false
	}
	spent := cur.UsedPercent - prev.UsedPercent
	if spent < 0 {
		return 0, false
	}
	return spent / (100 * float64(now.Sub(from)) / float64(length)), true
}

// windowOf is the window of s that is minutes long, the primary first.
func (s Snapshot) windowOf(minutes int) (Window, bool) {
	switch {
	case s.WindowMinutes == minutes:
		return s.Window, true
	case s.Secondary != nil && s.Secondary.WindowMinutes == minutes:
		return *s.Secondary, true
	}
	return Window{}, false
}

// Reach is when the used share reaches pct at the current pace (the average
// since the window began: Start + (Now-Start)*pct/Used). ok is false when it
// is reached already (Used >= pct), nothing is used yet (Used <= 0), pct <= 0,
// nothing has elapsed to extrapolate from, or the time is not before
// ResetsAt.
func (p Pace) Reach(pct float64) (time.Time, bool) {
	elapsed := p.Now.Sub(p.Start)
	if pct <= 0 || p.Used <= 0 || p.Used >= pct || elapsed <= 0 {
		return time.Time{}, false
	}
	at := p.Start.Add(time.Duration(float64(elapsed) * pct / p.Used))
	if !at.Before(p.ResetsAt) {
		return time.Time{}, false
	}
	return at, true
}
