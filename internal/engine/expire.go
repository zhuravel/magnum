package engine

import (
	"context"
	"errors"
	"time"
)

// requestExpiry is how old a request queued while no daemon ran may be when
// a daemon starts: older ones fail instead of acting days late.
const requestExpiry = time.Hour

// expireMessage is the result of a request expireRequests fails.
const expireMessage = "expired: queued while no daemon ran"

// expireRequests fails, at startup, the pending requests older than
// requestExpiry that no daemon ever saw: queued after the previous daemon's
// last tick (lastTick; zero = no daemon ever ticked). A request queued while
// a daemon ran stays pending: one handed to the heavy worker when that daemon
// stopped resumes in this one.
func (e *Engine) expireRequests(ctx context.Context, lastTick time.Time) {
	reqs, err := e.st.PendingRequests(ctx, 0)
	if err != nil {
		e.log.Warn("requests", "err", err)
		return
	}
	cutoff := e.now().Add(-requestExpiry)
	for _, r := range reqs {
		if r.CreatedAt.Before(cutoff) && (lastTick.IsZero() || r.CreatedAt.After(lastTick)) {
			e.complete(ctx, r.ID, errors.New(expireMessage), "")
		}
	}
}
