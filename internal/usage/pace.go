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
