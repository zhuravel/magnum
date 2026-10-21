package engine

// Auto-approval and the GitHub budget (DECISIONS "Auto-approval reads
// nothing while its GitHub budget is spent"). The pass reads GitHub's
// reviews of a PR as the identity it approves as (and follows a standing
// approval as) and a base tip's checks as the watch's poll identity, and a
// read that failed kept no fingerprint, so the next tick read again: from
// 10-19 to 10-21 GitHub refused 127 of them for its rate limit (96 reads of
// the reviews, 31 of a base tip's checks). Now the pass reads nothing as an
// identity below [github] budget_floor (githubLow) or while the poll waits
// for GitHub's rate limit (KVGHPollPausedUntil), and a read GitHub refused
// for its rate limit (github.ErrRateLimited) waits for the identity's reset
// before it is made again: by PR for the reviews (autoRetry, beside the
// fingerprints), by tip for the checks (baseTipRead.retry). Each hold is
// logged once, not on every tick.

import (
	"context"
	"errors"
	"maps"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// autoReadsPass readies a pass of auto-approval's GitHub reads (under
// autoMu): the poll's rate-limit pause, read once per pass, and the maps,
// whose retry times that passed go (a PR no longer read leaves none).
func (e *Engine) autoReadsPass(ctx context.Context) {
	e.autoPollPaused, _ = e.kvTime(ctx, store.KVGHPollPausedUntil)
	if e.autoRetry == nil {
		e.autoRetry, e.autoHeld = map[int64]time.Time{}, map[string]time.Time{}
	}
	now := e.now()
	maps.DeleteFunc(e.autoRetry, func(_ int64, until time.Time) bool { return !now.Before(until) })
}

// autoReadHeld reports whether auto-approval reads nothing from GitHub as
// identity now, for PR prID (0 for no PR's read, a base tip's): a read of
// the PR GitHub refused for its rate limit waits for its retry time
// (autoRetry), and every read waits while identity's budget is below the
// floor or the poll waits for GitHub's rate limit. The first of those
// holds for identity and its end is logged once.
func (e *Engine) autoReadHeld(identity string, prID int64) bool {
	now := e.now()
	if now.Before(e.autoRetry[prID]) {
		return true // the refusal was logged
	}
	var why string
	var until time.Time
	if low, ok := e.githubLow(identity); ok {
		why, until = low.text(), low.until
	} else if now.Before(e.autoPollPaused) {
		why, until = "the poll waits for GitHub's rate limit until "+e.autoPollPaused.Local().Format("15:04"), e.autoPollPaused
	} else {
		return false
	}
	if !e.autoHeld[identity].Equal(until) {
		e.autoHeld[identity] = until
		e.log.Info("auto approval: no GitHub reads until the budget recovers", "identity", identity, "why", why)
	}
	return true
}

// autoReadFailed notes a read of PR prID as identity that failed with err:
// one GitHub refused for its rate limit is made again only after
// identity's reset (autoRetryAt), and the log line, the refusal's only
// one, says when.
func (e *Engine) autoReadFailed(ctx context.Context, what, identity string, prID int64, err error) {
	if ctx.Err() != nil {
		return
	}
	if !errors.Is(err, github.ErrRateLimited) {
		e.log.Info("auto approval: "+what, "pr", prID, "err", err)
		return
	}
	until := e.autoRetryAt(identity)
	e.autoRetry[prID] = until
	e.log.Info("auto approval: "+what+"; GitHub refused it for its rate limit, read again after the reset", "pr", prID,
		"identity", identity, "until", until, "err", err)
}

// autoRetryAt is when auto-approval reads GitHub as identity again after
// GitHub refused a read for its rate limit: the reset the meter knows for
// it, ahead of now (the refusal marked its budget spent until then,
// github_cost.go), else githubRefusedProbe from now.
func (e *Engine) autoRetryAt(identity string) time.Time {
	now := e.now()
	m := &e.ghMeter
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.ids[identity]; c != nil && c.Rate != nil && c.Rate.ResetAt.After(now) {
		return c.Rate.ResetAt
	}
	return now.Add(githubRefusedProbe)
}
