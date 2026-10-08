package engine

// Identity checks and token refreshes that cannot reach GitHub. A network
// blip is no verdict on an identity: the failure is retried with backoff
// while the identity keeps its previous verdict (a token still valid stays in
// use), and only one that lasts identityNetGrace marks it unhealthy, saying
// it is the network. An identity marked unhealthy for a connection-class
// reason, by this daemon or anyone else, is re-checked on the same backoff;
// a real verdict (401, 403, 404, a wrong login, a missing permission, a bad
// key) is recorded at once and re-checked at the reconcile, as before.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/identity"
)

const (
	// identityNetGrace: connection-class failures of an identity's check or
	// token refresh mark it unhealthy only once they lasted this long.
	identityNetGrace = 5 * time.Minute
	// identityRetryFirst is the first retry's wait; each failed retry
	// doubles it up to identityRetryMax, the wait between re-checks of an
	// identity marked unhealthy for the network.
	identityRetryFirst = 30 * time.Second
	identityRetryMax   = 4 * time.Minute
)

// checkConnectionCause names the connection-class failure of a failed
// identity check ("" = a real verdict): every FAIL line, and the error when
// the check could not complete, must be one. A check that found a real
// problem before the network failed is a real verdict.
func checkConnectionCause(rep identity.Report, err error) string {
	var msgs []string
	for _, l := range rep.Lines {
		if m, ok := strings.CutPrefix(l, "FAIL "); ok {
			msgs = append(msgs, m)
		}
	}
	if err != nil {
		msgs = append(msgs, err.Error())
	}
	first := ""
	for _, m := range msgs {
		cause := github.ConnectionCause(m)
		if cause == "" {
			return ""
		}
		if first == "" {
			first = cause
		}
	}
	return first
}

// netRun is a run of connection-class failures of an identity's check or
// token refresh (Engine.netRuns, keyed by netCheckKey or netTokenKey).
type netRun struct {
	since time.Time     // the run's first failure
	wait  time.Duration // the wait before next
	next  time.Time     // when to try again
}

func netCheckKey(name string) string { return "check:" + name }
func netTokenKey(name string) string { return "token:" + name }

// netFailed records a connection-class failure of key at now and returns its
// run: the first retry waits identityRetryFirst, each further one twice as
// long up to identityRetryMax, and a retry within the run's first
// identityNetGrace is never put after it, so a failure that lasts is marked
// when the grace ends.
func (e *Engine) netFailed(key string, now time.Time) netRun {
	e.netMu.Lock()
	defer e.netMu.Unlock()
	r, ok := e.netRuns[key]
	if !ok {
		r = netRun{since: now}
	}
	r.wait = min(max(r.wait*2, identityRetryFirst), identityRetryMax)
	r.next = now.Add(r.wait)
	if grace := r.since.Add(identityNetGrace); now.Before(grace) && r.next.After(grace) {
		r.next = grace
	}
	e.netRuns[key] = r
	return r
}

// netNext reports when key's run tries again (false: no run).
func (e *Engine) netNext(key string) (time.Time, bool) {
	e.netMu.Lock()
	defer e.netMu.Unlock()
	r, ok := e.netRuns[key]
	return r.next, ok
}

// netEnded ends key's run: GitHub answered.
func (e *Engine) netEnded(key string) {
	e.netMu.Lock()
	delete(e.netRuns, key)
	e.netMu.Unlock()
}

// networkReason is the recorded reason of an identity marked unhealthy for
// the network.
func networkReason(cause string, since time.Time, msg string) string {
	return fmt.Sprintf("network failure (%s) since %s: %s", cause, since.Local().Format("15:04"), msg)
}

// netRetrying handles a connection-class failure of key, an identity
// signal whose recorded failure is prev (had: one is recorded): it reports
// whether the failure is only retried (the verdict stands: a pass, or a
// failure recorded before) and otherwise the reason to record now that the
// run lasted identityNetGrace. The first failure of a run is an event.
func (e *Engine) netRetrying(ctx context.Context, name, what, key, cause, msg string, had bool) (string, bool) {
	now := e.now()
	run := e.netFailed(key, now)
	if run.since.Equal(now) && run.wait == identityRetryFirst {
		e.event(ctx, "info", "identity:"+name, "identity.unreachable", fmt.Sprintf(
			"identity %s %s could not reach GitHub (%s); its verdict stands, retried until it answers, unhealthy if this lasts %s",
			name, what, cause, identityNetGrace), nil)
	}
	switch {
	case had:
		e.log.Info("identity "+what+" could not reach GitHub; its recorded failure stands", "identity", name, "cause", cause, "retry", run.next, "err", msg)
		return "", true
	case now.Sub(run.since) < identityNetGrace:
		e.log.Info("identity "+what+" could not reach GitHub; retrying", "identity", name, "cause", cause, "retry", run.next, "err", msg)
		return "", true
	}
	return networkReason(cause, run.since, msg), false
}

// checkIdentity runs name's health Check and records the verdict: a pass, a
// real failure at once (with the "identity unhealthy" toast), and a
// connection-class failure only once it lasted identityNetGrace (netRetrying).
// A cancelled check (the daemon stopping) records nothing.
func (e *Engine) checkIdentity(ctx context.Context, name string) {
	rep, err := e.d.Identities[name].Check(ctx)
	if ctx.Err() != nil {
		return
	}
	key := netCheckKey(name)
	if err == nil && rep.Pass {
		e.netEnded(key)
		e.recordIdentityVerdict(ctx, name, true, "")
		return
	}
	reason := firstFail(rep)
	if err != nil {
		reason = err.Error()
	}
	if cause := checkConnectionCause(rep, err); cause != "" {
		prev, _ := e.getKV(ctx, KVIdentityCheck(name))
		r, retried := e.netRetrying(ctx, name, "check", key, cause, reason, prev == "fail")
		if retried {
			return
		}
		reason = r
	} else {
		e.netEnded(key)
	}
	e.recordIdentityVerdict(ctx, name, false, reason)
	e.urgent("identity:"+name, "magnum: identity "+name+" unhealthy", reason+" (magnum identities check)", identityToastSpan)
}

// retryIdentities re-checks, each tick and on netFailed's backoff, the
// identities whose last check could not reach GitHub, and those recorded
// unhealthy for a connection-class reason by anyone (`magnum identities
// check`, a judge's own check, a previous daemon): the first of those is
// checked at once. Real failures wait for the reconcile (warmIdentities).
func (e *Engine) retryIdentities(ctx context.Context) {
	if e.d.DryRun {
		return
	}
	now := e.now()
	for _, name := range e.identityNames() {
		if next, ok := e.netNext(netCheckKey(name)); ok {
			if !now.Before(next) {
				e.checkIdentity(ctx, name)
			}
			continue
		}
		if v, _ := e.getKV(ctx, KVIdentityCheck(name)); v != "fail" {
			continue
		}
		if r, _ := e.getKV(ctx, KVIdentityError(name)); github.ConnectionCause(r) != "" {
			e.checkIdentity(ctx, name)
		}
	}
}
