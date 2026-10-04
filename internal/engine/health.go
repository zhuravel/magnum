package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/identity"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// Pause timing.
const (
	loginRecheck      = 60 * time.Second // logged-out CLI: preflight again after this
	usageFallback     = time.Hour        // usage limit without a parsed reset: 1h, doubling
	usageFallbackMax  = 8 * time.Hour
	attentionWindow   = 30 * time.Minute // dedupe window of attention toasts
	identityToastSpan = 6 * time.Hour
)

// tickState is what one tick learned from herdr.
type tickState struct {
	herdrUp      bool
	snap         herdr.Snapshot
	workingCodex int
	busyPR       map[int64]bool // a live agent of the PR is working or blocked
}

// observe takes the tick's herdr snapshot and feeds agents.ObserveSnapshotAt
// with the time it was taken (the completion signal rounds poll the store
// for; a session started after the snapshot is not judged lost from it).
func (e *Engine) observe(ctx context.Context) tickState {
	ts := tickState{busyPR: map[int64]bool{}}
	if e.d.Herdr == nil {
		return ts
	}
	capturedAt := e.now()
	snap, err := e.d.Herdr.Snapshot(ctx)
	e.noteHerdr(ctx, err)
	if err != nil {
		return ts
	}
	ts.herdrUp, ts.snap = true, snap
	ts.workingCodex = agents.CountWorking(snap.Agents, agents.KindCodex)
	if !e.d.DryRun && e.d.Agents != nil {
		obs, err := e.d.Agents.ObserveSnapshotAt(ctx, snap, capturedAt)
		if err != nil {
			e.log.Warn("observe agents", "err", err)
		}
		for _, o := range obs {
			e.onObservation(ctx, o)
		}
	}
	if live, err := e.st.LiveSessions(ctx); err == nil {
		for _, s := range live {
			switch herdr.Status(deref(s.AgentStatus)) {
			case herdr.StatusWorking, herdr.StatusBlocked:
				ts.busyPR[s.PRID] = true
			}
		}
	}
	return ts
}

// noteHerdr logs herdr going down or coming back once per transition.
func (e *Engine) noteHerdr(ctx context.Context, err error) {
	up := err == nil
	if e.herdrUp != nil && *e.herdrUp == up {
		return
	}
	e.herdrUp = &up
	if up {
		e.setKV(ctx, kvHerdrUp, "1")
		e.log.Info("herdr reachable")
		return
	}
	e.setKV(ctx, kvHerdrUp, "0")
	e.event(ctx, "warn", "", "herdr.down", fmt.Sprintf("herdr unreachable: %v; dispatch waits", err), nil)
}

func (e *Engine) onObservation(ctx context.Context, o agents.Observation) {
	if o.Kind == "" {
		return
	}
	subject := fmt.Sprintf("pr:%d", o.PRID)
	label := fmt.Sprintf("PR %d", o.PRID)
	if pr, err := e.st.PRByID(ctx, o.PRID); err == nil {
		if repo, err := e.st.RepoByID(ctx, pr.RepoID); err == nil {
			subject = prSubject(repo, pr.Number)
			label = fmt.Sprintf("%s#%d", repo.Name, pr.Number)
		}
	}
	switch o.Kind {
	case agents.ObsBlocked:
		e.log.Warn("agent waits on a dialog", "subject", subject, "role", o.Role)
		key := fmt.Sprintf("blocked:%d", o.Session.ID)
		e.urgent(key, "magnum: "+label+" "+o.Role.Label()+" needs you",
			"The agent waits on a dialog in herdr (magnum open "+label+").", attentionWindow)
		sess := o.Session
		e.revealAttention(ctx, o.PRID, &sess, key)
	case agents.ObsLost:
		e.event(ctx, "warn", subject, "agent.lost", fmt.Sprintf("%s session lost (pane or agent gone)", o.Role.Label()), nil)
	case agents.ObsHumanActive:
		e.log.Info("someone is typing into a magnum pane; prompts wait", "subject", subject, "role", o.Role,
			"cooldown", e.cfg.Daemon.HumanCooldown.Duration)
	case agents.ObsCompleted:
		e.log.Debug("agent turn completed", "subject", subject, "role", o.Role)
	}
}

// toolPause is one tool's pause record.
type toolPause struct {
	Until  time.Time
	Reason string
	Detail string
}

func (e *Engine) toolPause(ctx context.Context, tool string) (toolPause, bool) {
	until, ok := e.kvTime(ctx, KVToolPausedUntil(tool))
	if !ok {
		return toolPause{}, false
	}
	reason, _ := e.getKV(ctx, KVToolPausedReason(tool))
	detail, _ := e.getKV(ctx, kvToolPausedDetail(tool))
	return toolPause{Until: until, Reason: reason, Detail: detail}, true
}

// pauseTool pauses an agent kind (pipeline.Pause.Tool) after a health
// failure: usage limits until the parsed reset (else 1h, doubling), logins
// for loginRecheck (re-checked with a preflight), overload for the fallback
// backoff.
func (e *Engine) pauseTool(ctx context.Context, p pipeline.Pause) {
	tool := p.Tool
	if tool == "" {
		tool = agents.KindCodex
	}
	now := e.now()
	until := p.Until
	switch p.Kind {
	case string(agents.HealthLoginRequired):
		until = now.Add(loginRecheck)
	default:
		if until.IsZero() || !until.After(now) {
			d := usageFallback
			if v, ok := e.getKV(ctx, kvToolBackoff(tool)); ok {
				if prev, err := time.ParseDuration(v); err == nil && prev > 0 {
					d = min(prev*2, usageFallbackMax)
				}
			}
			e.setKV(ctx, kvToolBackoff(tool), d.String())
			until = now.Add(d)
		}
	}
	prev, had := e.toolPause(ctx, tool)
	if had && prev.Until.After(until) && prev.Reason == p.Kind {
		until = prev.Until
	}
	e.setToolPause(ctx, tool, p.Kind, p.Detail, until)
	e.event(ctx, "warn", "tool:"+tool, "tool.paused", fmt.Sprintf("%s paused (%s) until %s: %s", tool, p.Kind, until.Local().Format("15:04"), p.Detail), nil)
	title, body := toolToast(tool, p.Kind, until)
	e.urgent("pause:"+tool+":"+p.Kind, title, body, time.Hour)
}

// setToolPause records a tool's pause where dispatch reads it
// (kindPauseReason) and status shows it.
func (e *Engine) setToolPause(ctx context.Context, tool, reason, detail string, until time.Time) {
	e.setKV(ctx, KVToolPausedUntil(tool), store.FormatTime(until))
	e.setKV(ctx, KVToolPausedReason(tool), reason)
	e.setKV(ctx, kvToolPausedDetail(tool), detail)
}

func toolToast(tool, kind string, until time.Time) (string, string) {
	name := strings.ToUpper(tool[:1]) + tool[1:]
	switch kind {
	case string(agents.HealthLoginRequired):
		cmd, ok := map[string]string{agents.KindCodex: "codex login", agents.KindClaude: "claude auth login"}[tool]
		if !ok {
			return "magnum: " + name + " needs a login", "Reviews are paused until " + name + " is logged in again."
		}
		return "magnum: " + name + " needs `" + cmd + "`", "Reviews are paused until " + name + " is logged in again."
	case string(agents.HealthUsageLimit):
		return "magnum: " + name + " usage limit", "Reviews resume at " + until.Local().Format("15:04") + "."
	}
	return "magnum: " + name + " " + kind, "Reviews pause until " + until.Local().Format("15:04") + "."
}

func (e *Engine) clearToolPause(ctx context.Context, tool string) {
	if reason, ok := e.getKV(ctx, KVToolPausedReason(tool)); ok && reason != "" {
		e.forgetSend(ctx, "pause:"+tool+":"+reason) // the next pause toasts at once
	}
	e.delKV(ctx, KVToolPausedUntil(tool), KVToolPausedReason(tool), kvToolPausedDetail(tool))
}

// forgetSend drops a toast's dedup record once its condition cleared.
func (e *Engine) forgetSend(ctx context.Context, key string) {
	if e.d.DryRun {
		return
	}
	if err := e.st.ForgetSend(ctx, key); err != nil {
		e.log.Warn("forget notification", "key", key, "err", err)
	}
}

// revealAttention brings a PR that needs a human to the front when
// [terminal] reveal_on_attention is on: it focuses the session's agent (sess,
// else the PR's live judge) in herdr and reveals the herdr client in the
// terminal, at most once per key and attentionWindow (the daemon must not
// keep stealing focus). Dry runs, a missing herdr client and a PR without a
// live session do nothing.
func (e *Engine) revealAttention(ctx context.Context, prID int64, sess *store.Session, key string) {
	if !e.cfg.Terminal.RevealOnAttention || e.d.DryRun || e.d.Focus == nil {
		return
	}
	if sess == nil {
		s, err := e.st.LiveSessionByPRRole(ctx, prID, e.judgeOf(ctx, prID))
		if err != nil {
			return
		}
		sess = &s
	}
	target := deref(sess.AgentName)
	if target == "" {
		target = deref(sess.HerdrPaneID)
	}
	if target == "" {
		return
	}
	if ok, err := e.st.ShouldSend(ctx, "reveal:"+key, attentionWindow); err != nil || !ok {
		return
	}
	if err := e.d.Focus(ctx, target); err != nil {
		e.log.Warn("reveal on attention: focus", "target", target, "err", err)
		return
	}
	if e.d.Reveal != nil {
		if err := e.d.Reveal(ctx); err != nil {
			e.log.Warn("reveal on attention: terminal", "err", err)
			return
		}
	}
	e.log.Info("revealed a PR that needs attention", "pr", prID, "target", target)
}

// health ends expired pauses: a logged-out CLI must pass its preflight
// first; usage limits are lifted optimistically (the next round pauses
// again, with a doubled fallback, if the limit still holds).
func (e *Engine) health(ctx context.Context) {
	now := e.now()
	for _, tool := range e.kinds() {
		p, ok := e.toolPause(ctx, tool)
		if !ok || now.Before(p.Until) {
			continue
		}
		if p.Reason == BudgetPauseReason && e.budgetKnown() {
			continue // checkBudget lifts it once the budget is below the cap
		}
		if e.d.DryRun {
			e.rec.Record(ctx, "tool:"+tool, "resume", "pause ("+p.Reason+") expired")
			continue
		}
		if p.Reason == string(agents.HealthLoginRequired) && e.d.Agents != nil {
			if err := e.d.Agents.Preflight(ctx, tool); err != nil {
				e.setKV(ctx, KVToolPausedUntil(tool), store.FormatTime(now.Add(loginRecheck)))
				e.log.Info("still paused", "tool", tool, "reason", p.Reason, "err", err)
				continue
			}
		}
		e.clearToolPause(ctx, tool)
		e.event(ctx, "info", "tool:"+tool, "tool.resumed", fmt.Sprintf("%s pause (%s) ended", tool, p.Reason), nil)
	}
	if until, ok := e.kvTime(ctx, KVDaemonPausedUntil); ok && !now.Before(until) {
		e.delKV(ctx, KVDaemonPaused, KVDaemonPausedReason, KVDaemonPausedUntil)
		e.event(ctx, "info", "", "daemon.resumed", "automation pause ended", nil)
	}
	e.probeInfra(ctx)
	e.checkBudget(ctx)
}

// pauseReason is why dispatch is closed for everything ("" = open): the
// daemon pause, a drain before a restart, an infrastructure pause. A paused
// agent kind only holds the rounds whose roles use it (kindPauseReason).
func (e *Engine) pauseReason(ctx context.Context) string {
	if v, ok := e.getKV(ctx, KVDaemonPaused); ok && v == "1" {
		r, _ := e.getKV(ctx, KVDaemonPausedReason)
		return strings.TrimSpace("daemon paused " + r)
	}
	if v, ok := e.getKV(ctx, KVDaemonDraining); ok && v != "" {
		return "draining for a restart (magnum daemon-restart --drain)"
	}
	if p, ok := e.infraPause(ctx); ok {
		return fmt.Sprintf("infrastructure paused (%s), next probe %s", p.Reason, p.Until.Local().Format("15:04"))
	}
	return ""
}

// configDirEnsurer is implemented by *identity.App.
type configDirEnsurer interface {
	EnsureConfigDir(ctx context.Context) (string, error)
}

type expirer interface{ Expiry() time.Time }

// warmIdentities runs every identity's health Check (all when full, else
// only unhealthy ones) and records the verdict.
func (e *Engine) warmIdentities(ctx context.Context, full bool) {
	if e.d.DryRun {
		return
	}
	for _, name := range e.identityNames() {
		src := e.d.Identities[name]
		if !full {
			if ok, _ := e.identityHealthy(ctx, name); ok {
				continue
			}
		}
		rep, err := src.Check(ctx)
		pass, reason := err == nil && rep.Pass, firstFail(rep)
		if err != nil {
			reason = err.Error()
		}
		e.recordIdentityVerdict(ctx, name, pass, reason)
		if !pass {
			e.urgent("identity:"+name, "magnum: identity "+name+" unhealthy", reason+" (magnum identities check)", identityToastSpan)
		}
	}
}

// recordIdentityVerdict stores an identity check's verdict where dispatch
// reads it (KVIdentityCheck "pass" or "fail", KVIdentityError the reason).
// A fail that turns into a pass forgets the "identity unhealthy" toast's
// dedup record, so the next failure notifies at once; a new failure reason
// is logged as identity.unhealthy. The daemon's own checks
// (warmIdentities) and `magnum identities check` (ReqIdentityVerdict) both
// record through it.
func (e *Engine) recordIdentityVerdict(ctx context.Context, name string, pass bool, reason string) {
	if pass {
		if prev, _ := e.getKV(ctx, KVIdentityCheck(name)); prev == "fail" {
			e.forgetSend(ctx, "identity:"+name)
		}
		e.setKV(ctx, KVIdentityCheck(name), "pass")
		e.delKV(ctx, KVIdentityError(name))
		return
	}
	if reason == "" {
		reason = "check failed"
	}
	prev, _ := e.getKV(ctx, KVIdentityError(name))
	e.setKV(ctx, KVIdentityCheck(name), "fail")
	e.setKV(ctx, KVIdentityError(name), reason)
	if prev != reason {
		e.event(ctx, "warn", "identity:"+name, "identity.unhealthy", "identity "+name+" unhealthy: "+reason, nil)
	}
}

func firstFail(rep identity.Report) string {
	for _, l := range rep.Lines {
		if strings.HasPrefix(l, "FAIL ") {
			return strings.TrimPrefix(l, "FAIL ")
		}
	}
	return "check failed"
}

// refreshIdentities keeps App tokens and GH_CONFIG_DIR files fresh (every
// tick, so long-lived panes never see an expired token).
func (e *Engine) refreshIdentities(ctx context.Context) {
	if e.d.DryRun {
		return
	}
	for _, name := range e.identityNames() {
		d, ok := e.d.Identities[name].(configDirEnsurer)
		if !ok {
			continue
		}
		if _, err := d.EnsureConfigDir(ctx); err != nil {
			if prev, _ := e.getKV(ctx, kvIdentityTickError(name)); prev != err.Error() {
				e.setKV(ctx, kvIdentityTickError(name), err.Error())
				e.event(ctx, "warn", "identity:"+name, "identity.token_error", "token refresh failed: "+err.Error(), nil)
			}
			e.urgent("identity-token:"+name, "magnum: identity "+name+" token refresh failed", err.Error(), identityToastSpan)
			continue
		}
		if _, had := e.getKV(ctx, kvIdentityTickError(name)); had {
			e.forgetSend(ctx, "identity-token:"+name)
			e.delKV(ctx, kvIdentityTickError(name))
		}
		if x, ok := e.d.Identities[name].(expirer); ok && !x.Expiry().IsZero() {
			e.setKV(ctx, kvIdentityExpiry(name), store.FormatTime(x.Expiry()))
		}
	}
}

func (e *Engine) identityNames() []string {
	var out []string
	for n := range e.d.Identities {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// identityHealthy reports whether name may post (no failed check, no token
// refresh failure) and why not.
func (e *Engine) identityHealthy(ctx context.Context, name string) (bool, string) {
	if _, ok := e.d.Identities[name]; !ok && len(e.d.Identities) > 0 {
		return false, "identity " + name + " is not configured"
	}
	if v, _ := e.getKV(ctx, KVIdentityCheck(name)); v == "fail" {
		r, _ := e.getKV(ctx, KVIdentityError(name))
		return false, "identity " + name + " unhealthy: " + r
	}
	if v, ok := e.getKV(ctx, kvIdentityTickError(name)); ok {
		return false, "identity " + name + " token refresh failed: " + v
	}
	return true, ""
}

// isLogin reports a login-required error from Preflight.
func isLogin(err error) bool { return errors.Is(err, agents.ErrLoginRequired) }
