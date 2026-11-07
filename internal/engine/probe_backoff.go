package engine

// The backoff of a probe that times out (DECISIONS "A probe that times out
// waits before it is asked again"): from 2026-11-03 to 11-07 `mise which
// codex` ran out its 15 s 181 times and Whiteboard's status check 153 times,
// in bursts under memory pressure. Each one held its caller for the whole
// wait (the health check holds the tick) and started one more process on a
// machine short of memory, only to learn what the last ask had said. A
// timeout starts a wait that doubles while the asks keep timing out; any
// other outcome, an answer or a quick failure, ends it, since such an ask
// costs nothing. A probe in its wait answers as it did when it timed out.

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Probe backoff timing: the wait after a probe's first timeout, and the
// longest one a run of timeouts doubles it to.
const (
	probeBackoffFirst = 2 * time.Minute
	probeBackoffMax   = 30 * time.Minute
)

// probeBackoff spaces out the asks of one probe while they time out. The
// zero value asks at once.
type probeBackoff struct {
	mu   sync.Mutex
	wait time.Duration // the last wait (0: the last ask did not time out)
	at   time.Time     // when the last ask that timed out started
	next time.Time     // no ask before this
	ok   time.Time     // when the latest ask that did not time out started
}

// waiting reports whether the probe is in its wait at now, and if so when
// the ask that timed out last started and when it may be asked again.
func (b *probeBackoff) waiting(now time.Time) (at, next time.Time, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.at, b.next, b.wait > 0 && now.Before(b.next)
}

// asked records the outcome of an ask that started at now: a timeout
// (probeTimedOut) starts the wait, or doubles it up to probeBackoffMax, and
// returns when the probe may be asked again; anything else ends the wait
// (zero time). An ask that started inside the wait (one that ran beside the
// ask that started it, or `magnum whiteboard`, which does not wait) and
// timed out keeps the wait as it is, from its own start. The timeout of an
// ask that started before one that did not time out (two reviews' checks
// side by side) is stale: the probe answered since, so it starts no wait
// and keeps any a later timeout started.
func (b *probeBackoff) asked(now time.Time, timedOut bool) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !timedOut {
		b.wait, b.at, b.next = 0, time.Time{}, time.Time{}
		if now.After(b.ok) {
			b.ok = now
		}
		return time.Time{}
	}
	if now.Before(b.ok) {
		if b.wait == 0 {
			return time.Time{}
		}
		return b.next
	}
	switch {
	case b.wait == 0:
		b.wait = probeBackoffFirst
	case !now.Before(b.next):
		b.wait = min(b.wait*2, probeBackoffMax)
	}
	b.at, b.next = now, now.Add(b.wait)
	return b.next
}

// probeTimedOut reports whether err is a probe's own timeout: the deadline
// of its command, not ctx ending (the daemon stopping).
func probeTimedOut(ctx context.Context, err error) bool {
	return err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded)
}
