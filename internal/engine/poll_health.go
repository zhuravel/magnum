package engine

// How each watch's polls go: the radar call of a watch owner fails while
// GitHub answers 502 and 504 (on 10-05 one watch's did 166 times in seven
// hours), and the daemon's last poll, the time of its attempt, kept saying
// "1m ago". Each watch owner keeps its last good poll and, while its radar
// calls fail, since when, how many in a row and why (store.KVWatchPoll); the
// screens show a watch failing for PollFailingShown, and one toast goes out
// per failure streak once it lasts pollFailingToast and pollFailingPolls
// polls. A tick after a sleep of the Mac (sleptBefore) starts no streak: the
// polls of its dark wakes failed while its network came up.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

const (
	// PollFailingShown is how long a watch's radar calls must have failed
	// before the screens and `magnum status` show it: a 502 or two between
	// good polls is GitHub's weather, not news.
	PollFailingShown = 10 * time.Minute
	// pollFailingToast is how long they must have failed before the one
	// toast of the streak, and pollFailingPolls how many polls in a row: 3
	// failed polls over 35 minutes of dark wakes were toasted.
	pollFailingToast = 15 * time.Minute
	pollFailingPolls = 10
	// sleepTicks: a wait between ticks longer than this many poll intervals
	// was a sleep (sleptBefore).
	sleepTicks = 3
	// pollFailingWindow: a streak is toasted again, at most, a day later.
	pollFailingWindow = 24 * time.Hour
	// pollCauseRunes bounds the cause a failure keeps.
	pollCauseRunes = 60
)

// kindPollFailing counts the watches whose polls fail in a batch summary.
var kindPollFailing = notify.Kind{One: "watch failing to poll", Many: "watches failing to poll"}

// WatchPoll is how a watch owner's radar calls went, the store.KVWatchPoll
// value: when one last answered (zero: never since it was recorded), and,
// while they fail, since when, how many polls in a row failed and the last
// failure's cause ("HTTP 502", or the error's redacted first line);
// FailingSince is zero while they answer.
type WatchPoll struct {
	LastOK       time.Time `json:"last_ok,omitzero"`
	FailingSince time.Time `json:"failing_since,omitzero"`
	Failures     int       `json:"failures,omitempty"`
	Error        string    `json:"error,omitempty"`
}

// Value is p as store.KVWatchPoll stores it (times in UTC).
func (p WatchPoll) Value() string {
	p.LastOK, p.FailingSince = utcOrZero(p.LastOK), utcOrZero(p.FailingSince)
	b, _ := json.Marshal(p)
	return string(b)
}

func utcOrZero(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC()
}

// ParseWatchPoll reads a store.KVWatchPoll value; false for "" or anything
// that is not one.
func ParseWatchPoll(v string) (WatchPoll, bool) {
	var p WatchPoll
	if v = strings.TrimSpace(v); !strings.HasPrefix(v, "{") || json.Unmarshal([]byte(v), &p) != nil {
		return WatchPoll{}, false
	}
	p.LastOK, p.FailingSince = utcOrZero(p.LastOK), utcOrZero(p.FailingSince)
	return p, true
}

// Failing is how long the radar calls have failed at now; 0 while they
// answer.
func (p WatchPoll) Failing(now time.Time) time.Duration {
	if p.FailingSince.IsZero() {
		return 0
	}
	return max(now.Sub(p.FailingSince), 0)
}

// pollCause is a radar failure as a watch keeps it: "HTTP 502" when GitHub
// answered with a status, else the error's text on one line, redacted and
// clipped to pollCauseRunes.
func pollCause(err error) string {
	if apiErr, ok := errors.AsType[*github.APIError](err); ok && apiErr.Status != 0 {
		return fmt.Sprintf("HTTP %d", apiErr.Status)
	}
	return textx.Clip(strings.Join(strings.Fields(execx.Redact(err.Error())), " "), pollCauseRunes)
}

// radarResults are the radar calls of one poll by watch owner (lower case),
// in the order of the watches: nil when every call for the owner answered,
// else the first failure.
type radarResults struct {
	owners []string
	names  map[string]string // lower owner -> the owner as configured
	errs   map[string]error
}

func (r *radarResults) add(owner string, err error) {
	k := strings.ToLower(owner)
	if r.errs == nil {
		r.errs, r.names = map[string]error{}, map[string]string{}
	}
	if _, ok := r.errs[k]; !ok {
		r.owners, r.names[k] = append(r.owners, k), owner
	}
	if r.errs[k] == nil {
		r.errs[k] = err
	}
}

// recordWatchPolls writes each polled owner's WatchPoll when it changed
// and toasts a streak that reached pollFailingToast and pollFailingPolls. A
// poll cut short by shutdown records nothing: its failures are the
// daemon's, not GitHub's; nor does a failure right after a sleep
// (afterSleep): the Mac's network was not up yet.
func (e *Engine) recordWatchPolls(ctx context.Context, r radarResults, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	for _, owner := range r.owners {
		err := r.errs[owner]
		if err != nil && e.afterSleep {
			continue
		}
		key := store.KVWatchPoll(owner)
		old, _ := e.getKV(ctx, key)
		p, _ := ParseWatchPoll(old)
		if err == nil {
			p = WatchPoll{LastOK: now}
			delete(e.pollToasted, owner)
		} else {
			if p.FailingSince.IsZero() {
				p.FailingSince, p.Failures = now, 0
			}
			p.Failures++
			p.Error = pollCause(err)
		}
		if v := p.Value(); v != old {
			e.setKV(ctx, key, v)
		}
		if d := p.Failing(now); d >= pollFailingToast && p.Failures >= pollFailingPolls {
			e.toastPollFailing(r.names[owner], p, d)
		}
	}
}

// sleptBefore reports whether the wait since the last tick ended (none
// before the first) lasted more than sleepTicks poll intervals by the wall
// clock: the Mac slept, and the polls of its dark wakes fail while the
// network comes up. The run loop waits one poll interval between ticks
// (less after a kick), so only a sleep stretches it; a long tick (GitHub
// timing out) does not count. The wall clock, because Go's monotonic clock
// stops while macOS sleeps.
func (e *Engine) sleptBefore(now time.Time) bool {
	return !e.tickEnded.IsZero() && now.Round(0).Sub(e.tickEnded) > sleepTicks*e.cfg.Daemon.PollInterval.Duration
}

// toastPollFailing offers the batcher the one toast of a watch's failure
// streak (keyed by its start, so a new streak toasts anew; the registry's
// dedupe keeps a restarted daemon quiet about the same one).
func (e *Engine) toastPollFailing(owner string, p WatchPoll, d time.Duration) {
	lower := strings.ToLower(owner)
	key := fmt.Sprintf("poll-failing:%s:%d", lower, p.FailingSince.Unix())
	if e.pollToasted[lower] == key {
		return
	}
	if e.pollToasted == nil {
		e.pollToasted = map[string]string{}
	}
	e.pollToasted[lower] = key
	what := owner + " polls failing " + pauseAge(d)
	if p.Error != "" {
		what += " (" + p.Error + ")"
	}
	last := ""
	if !p.LastOK.IsZero() {
		last = " (last good poll " + p.LastOK.Local().Format("15:04") + ")"
	}
	body := fmt.Sprintf("The radar calls of watch %s have failed since %s%s: magnum sees no new PRs or pushes of it until GitHub answers.",
		owner, p.FailingSince.Local().Format("15:04"), last)
	e.info(notify.Item{Key: key, Title: "magnum: " + what, Body: body, Line: what, Kind: kindPollFailing, Window: pollFailingWindow})
}
