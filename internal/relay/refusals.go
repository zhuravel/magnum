package relay

import (
	"cmp"
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"
)

// refusal is one kind of request the relay answers without serving it: a request refused before it is
// authenticated, or a signed delivery of an installation the relay does not keep. It is a log line's
// message, level, status and class, all the relay's own constants, so the kinds are few whatever comes.
type refusal struct {
	msg    string
	level  slog.Level
	status int
	class  string
}

var (
	// ignoredInstallation is a signed delivery of an installation Options.Installations does not list.
	ignoredInstallation = refusal{msg: "webhook ignored", level: slog.LevelInfo, status: 202, class: "installation"}
	// refusedListingToken is a listing without an accepted token.
	refusedListingToken = refusal{msg: "listing refused", level: slog.LevelWarn, status: 401, class: "token"}
	// refusedHealthToken is a health check whose token is not accepted: it answers without the counters.
	refusedHealthToken = refusal{msg: "health counters refused", level: slog.LevelWarn, status: 200, class: "token"}
)

// refusedWebhook is a delivery refused before its signature was found good.
func refusedWebhook(status int, class string) refusal {
	return refusal{msg: "webhook refused", level: slog.LevelWarn, status: status, class: class}
}

// refusals sums the refusals of each kind until RunRefusalLog logs them.
type refusals struct {
	mu     sync.Mutex
	counts map[refusal]int64
}

func (t *refusals) add(k refusal) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.counts == nil {
		t.counts = map[refusal]int64{}
	}
	t.counts[k]++
}

// take returns the counts and starts new ones.
func (t *refusals) take() map[refusal]int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.counts
	t.counts = nil
	return c
}

// RunRefusalLog logs the refusals counted since its last lines every interval (serve's is a minute), one line
// a kind with its count, and once more when ctx ends. A request the relay refuses before it is authenticated,
// or a delivery of another installation, logs nothing on its own, so a flood of them costs a few lines a
// minute, not one a request.
func (r *Relay) RunRefusalLog(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.logRefusals()
			return
		case <-t.C:
			r.logRefusals()
		}
	}
}

// logRefusals logs one line for each kind counted since the last call, in a fixed order, and resets the
// counts; a kind with none logs nothing.
func (r *Relay) logRefusals() {
	counts := r.refusals.take()
	for _, k := range slices.SortedFunc(maps.Keys(counts), func(a, b refusal) int {
		return cmp.Or(cmp.Compare(a.msg, b.msg), cmp.Compare(a.class, b.class), cmp.Compare(a.status, b.status))
	}) {
		args := []any{"status", k.status, "class", k.class, "count", counts[k]}
		if k.level >= slog.LevelWarn {
			r.log.Warn(k.msg, args...)
		} else {
			r.log.Info(k.msg, args...)
		}
	}
}
