package engine

// Codex's login (DECISIONS "A change of Codex login restarts the Codex
// sessions that run with the old one"): a Codex process keeps the login it
// started with, so after the operator switched accounts (clauth makes
// ~/.codex/auth.json a symlink to a profile's file) the live sessions went
// on with the old account at its usage limit. Once a minute, with the
// usage read, the tick reads the login's account (usage.CodexLogin: a
// fingerprint of tokens.account_id, never a token); when it changes, the
// live Codex sessions started before restart in new processes
// (session_restart.go), and the usage-limit pause and the budget reading,
// both the old account's, are lifted and overruled. A usage limit that a
// session started before the current login hits is that old account's too:
// it pauses no kind, the session restarts and its round retries in a minute.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/usage"
)

// kv keys of Codex's login.
const (
	// KVCodexLogin is the fingerprint of Codex's login (usage.Login), the
	// only thing of auth.json magnum keeps.
	KVCodexLogin = "codex.login"
	// KVCodexLoginAt is when the daemon first saw that login
	// (store.FormatTime), or, at the first read with none kept, when
	// auth.json last changed: the Codex sessions started before it run
	// with an older login.
	KVCodexLoginAt = "codex.login_at"
)

// The reasons a session is marked for a restart (restartMark.Why).
const (
	restartLoginChanged = "the Codex login changed"
	restartStaleLimit   = "a usage limit of the Codex login before the current one"
)

// staleLimitRetry is how soon a round that paused on a stale session's
// usage limit retries: by then its session is parked, and the retry
// resumes it in a new process.
const staleLimitRetry = time.Minute

// checkCodexLogin reads Codex's login once a minute (usageEvery) when a
// kind Codex backs is declared, and acts on a change (codexLoginChanged):
// a fingerprint other than the kept one, or, at the first read with none
// kept, live Codex sessions started before auth.json last changed. No
// auth.json, or one without an account, changes nothing.
func (e *Engine) checkCodexLogin(ctx context.Context) {
	if e.d.CodexLogin == nil || len(e.budgetKinds()) == 0 {
		return
	}
	now := e.now()
	if !e.codexLoginRead.IsZero() && now.Sub(e.codexLoginRead) < usageEvery {
		return
	}
	e.codexLoginRead = now
	login, err := e.d.CodexLogin(e.cfg.Usage.CodexHome)
	switch {
	case errors.Is(err, usage.ErrNoLogin):
		e.log.Debug("codex login: none", "err", err)
		return
	case err != nil:
		if e.changed("codex.login", err.Error()) {
			e.log.Warn("codex login", "err", err)
		}
		return
	}
	kept, had := e.getKV(ctx, KVCodexLogin)
	if had && kept == login.Fingerprint {
		return
	}
	since := now // the new login is first seen now
	if !had && !login.ChangedAt.IsZero() && login.ChangedAt.Before(now) {
		since = login.ChangedAt
	}
	stale := e.codexSessionsBefore(ctx, since)
	if !had && len(stale) == 0 {
		e.setKV(ctx, KVCodexLoginAt, store.FormatTime(since))
		e.setKV(ctx, KVCodexLogin, login.Fingerprint)
		return
	}
	e.codexLoginChanged(ctx, login, since, stale, had)
}

// codexLoginChanged acts on a new Codex login first seen (or, at the first
// read, made) at since: it keeps the login, marks stale (the live Codex
// sessions started before since) for a restart, lifts a usage-limit pause
// of each kind Codex backs, a lift a settle honours whether a pause was
// there or not (noteLift), and overrules the budget reading
// (overruleBudget), both the old account's, and says so in one
// codex.login_changed event and one toast. A dry run only records it.
func (e *Engine) codexLoginChanged(ctx context.Context, login usage.Login, since time.Time, stale []store.Session, had bool) {
	what := restartCount(len(stale))
	if e.d.DryRun {
		if e.changed("codex.login_dry_run", login.Fingerprint) {
			e.rec.Record(ctx, "tool:"+agents.KindCodex, "restart", "Codex login changed: "+what)
		}
		return
	}
	e.setKV(ctx, KVCodexLoginAt, store.FormatTime(since))
	e.setKV(ctx, KVCodexLogin, login.Fingerprint)
	e.markRestart(ctx, stale, restartLoginChanged)
	var lifted []string
	for _, kind := range e.budgetKinds() {
		e.noteLift(ctx, kind) // the old account's limits, a pause or not, pause the kind no more at a settle
		if p, ok := e.toolPause(ctx, kind); ok && p.Reason == string(agents.HealthUsageLimit) {
			e.clearToolPause(ctx, kind)
			e.event(ctx, "info", "tool:"+kind, "tool.resumed", fmt.Sprintf("%s pause (%s) lifted: the Codex login changed", kind, p.Reason), nil)
			lifted = append(lifted, kind)
		}
	}
	e.overruleBudget(ctx)
	msg := "Codex login changed: " + what
	if len(lifted) > 0 {
		msg += fmt.Sprintf("; the usage-limit pause of %s is lifted", lifted[0])
	}
	e.event(ctx, "info", "tool:"+agents.KindCodex, "codex.login_changed", msg,
		map[string]any{"sessions": len(stale), "first_read": !had, "lifted": lifted, "since": store.FormatTime(since)})
	e.info(notify.Item{Key: "codex-login:" + store.FormatTime(since), Title: "magnum: Codex login changed", Body: what + ".",
		Line: "Codex login changed: " + what})
}

// restartCount is the toast's "N Codex sessions restart with the new one".
func restartCount(n int) string {
	switch n {
	case 0:
		return "no live Codex session ran with the old one"
	case 1:
		return "1 Codex session restarts with the new one"
	}
	return fmt.Sprintf("%d Codex sessions restart with the new one", n)
}

// codexSessionsBefore are the live sessions of the kinds Codex backs, any
// role that runs an agent, started before since, but those of pinned PRs
// (RestartableSessions).
func (e *Engine) codexSessionsBefore(ctx context.Context, since time.Time) []store.Session {
	var out []store.Session
	for _, kind := range e.budgetKinds() {
		sessions, _, err := RestartableSessions(ctx, e.st, e.cfg, kind, 0)
		if err != nil {
			e.warnUnlessStopped(ctx, err, "codex login: live sessions")
			return nil
		}
		for _, s := range sessions {
			if s.StartedAt.Before(since) {
				out = append(out, s)
			}
		}
	}
	return out
}

// staleLimit reports whether pause p is a usage limit that a session of a
// kind Codex backs hit while it ran with a login older than the current
// one: the session (of p's run) started before KVCodexLoginAt. That limit
// is the old account's: no kind pauses for it.
func (e *Engine) staleLimit(ctx context.Context, p pipeline.Pause) (store.Session, bool) {
	tool := cmp.Or(p.Tool, agents.KindCodex)
	if p.Kind != string(agents.HealthUsageLimit) || p.RunID == "" || !slices.Contains(e.budgetKinds(), tool) {
		return store.Session{}, false
	}
	since, ok := e.kvTime(ctx, KVCodexLoginAt)
	if !ok {
		return store.Session{}, false
	}
	run, err := e.st.RunByID(ctx, p.RunID)
	if err != nil || run.SessionID == nil {
		return store.Session{}, false
	}
	s, err := e.st.SessionByID(ctx, *run.SessionID)
	if err != nil {
		return store.Session{}, false
	}
	return s, s.StartedAt.Before(since)
}

// restartStale marks session s, whose usage limit p is the old login's
// (staleLimit), for a restart instead of pausing its kind, and says so in
// one codex.stale_usage_limit event when it marks it.
func (e *Engine) restartStale(ctx context.Context, s store.Session, p pipeline.Pause) {
	if len(e.markRestart(ctx, []store.Session{s}, restartStaleLimit)) == 0 {
		return
	}
	tool := cmp.Or(p.Tool, agents.KindCodex)
	since, _ := e.kvTime(ctx, KVCodexLoginAt)
	subject := fmt.Sprintf("pr:%d", s.PRID)
	if pr, err := e.st.PRByID(ctx, s.PRID); err == nil {
		subject = e.prLabelSubject(ctx, pr)
	}
	e.event(ctx, "info", subject, "codex.stale_usage_limit",
		fmt.Sprintf("%s hit a usage limit in a session started at %s, before the current Codex login (since %s): %s is not paused; "+
			"the session restarts in a new process with the current login, and its paused round retries in a minute",
			s.Role, s.StartedAt.Local().Format("15:04"), since.Local().Format("15:04"), tool),
		map[string]any{"role": s.Role, "session": s.ID, "tool": tool})
}
