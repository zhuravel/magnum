package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/identity"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// Pause timing.
const (
	loginRecheck = 60 * time.Second // logged-out CLI: preflight again after this
	// loginStampHold caps how long a proven login pause holds on an
	// unchanged login file while the login check passes: a login pattern
	// that matched a failed run's own output (a 401 in a reviewer's stdout
	// on a PR about authentication) costs one wasted run every 2h, not the
	// kind for good.
	loginStampHold    = 2 * time.Hour
	usageFallback     = time.Hour // usage limit without a parsed reset: 1h, doubling
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
			e.warnUnlessStopped(ctx, err, "observe agents")
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

// noteHerdr logs herdr going down or coming back once per transition. A
// snapshot that failed because the daemon is stopping (isStop) says nothing
// about herdr: nothing is noted, and the next live tick decides.
func (e *Engine) noteHerdr(ctx context.Context, err error) {
	if err != nil && isStop(ctx, err) {
		e.log.Debug("herdr snapshot cut short", "err", err)
		return
	}
	up := err == nil
	if e.herdrUp != nil && *e.herdrUp == up {
		return
	}
	e.herdrUp = &up
	if up {
		e.setKV(ctx, store.KVHerdrUp, "1")
		e.log.Info("herdr reachable")
		return
	}
	e.setKV(ctx, store.KVHerdrUp, "0")
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
		e.revealAttention(ctx, o.PRID, &sess, "", key)
	case agents.ObsLost:
		e.event(ctx, "warn", subject, "agent.lost", fmt.Sprintf("%s session lost (pane or agent gone)", o.Role.Label()), nil)
		e.closeLostPanes(o.PRID) // retired_panes.go: its pane, a shell now, closes unless a round holds the PR
	case agents.ObsHumanActive:
		e.log.Info("someone is typing into a magnum pane; prompts wait", "subject", subject, "role", o.Role,
			"cooldown", e.cfg.Daemon.HumanCooldown.Duration)
		if !o.CooldownUntil.IsZero() { // a new cooldown, not one this tick extends
			e.event(ctx, "warn", subject, agents.EventHumanActive,
				fmt.Sprintf("%s works with no magnum prompt in flight, taken for someone typing: prompts to this PR wait until %s (human_cooldown %s)",
					o.Role.Label(), o.CooldownUntil.Local().Format("15:04:05"), humanDuration(e.cfg.Daemon.HumanCooldown.Duration)),
				map[string]any{"role": string(o.Role), "until": store.FormatTime(o.CooldownUntil)})
		}
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
	until, ok := e.kvTime(ctx, store.KVToolPausedUntil(tool))
	if !ok {
		return toolPause{}, false
	}
	reason, _ := e.getKV(ctx, store.KVToolPausedReason(tool))
	detail, _ := e.getKV(ctx, store.KVToolPausedDetail(tool))
	return toolPause{Until: until, Reason: reason, Detail: detail}, true
}

// PauseTool is pauseTool for the pipeline (Runner.OnPause): a reviewer's
// run that proved its kind logged out or over its limit pauses the kind
// while the round goes on.
func (e *Engine) PauseTool(ctx context.Context, p pipeline.Pause) { e.pauseTool(ctx, p) }

// pauseTool pauses an agent kind (pipeline.Pause.Tool) after a health
// failure: usage limits until the parsed reset (else 1h, doubling), logins
// for loginRecheck (re-checked with a preflight; a login pause a run proved
// also waits for the kind's login file to change, stampLogin), overload for
// the fallback backoff. A pause of the same reason still running is kept,
// without a second event or toast and without touching the backoff, and
// extended only to a later reset the new pause names: the round's settle
// pauses again what a reviewer's failure paused at once, and doubling the
// fallback for it paused one failure for 2.5h instead of 1h (2026-10-12).
// A usage limit that a Codex session older than the current Codex login hit
// pauses nothing: that session restarts instead (staleLimit, restartStale).
func (e *Engine) pauseTool(ctx context.Context, p pipeline.Pause) {
	if s, stale := e.staleLimit(ctx, p); stale {
		e.restartStale(ctx, s, p) // codex_login.go
		return
	}
	tool := p.Tool
	if tool == "" {
		tool = agents.KindCodex
	}
	now := e.now()
	proven := p.Proven && p.Kind == string(agents.HealthLoginRequired)
	if prev, had := e.toolPause(ctx, tool); had && prev.Reason == p.Kind && prev.Until.After(now) {
		if p.Until.After(prev.Until) {
			e.setKV(ctx, store.KVToolPausedUntil(tool), store.FormatTime(p.Until))
		}
		if proven {
			e.stampLogin(ctx, tool, p.ProvenAt)
		}
		return
	}
	until := p.Until
	switch p.Kind {
	case string(agents.HealthLoginRequired):
		until = now.Add(loginRecheck)
	default:
		if until.IsZero() || !until.After(now) {
			d := usageFallback
			if v, ok := e.getKV(ctx, store.KVToolBackoff(tool)); ok {
				if prev, err := time.ParseDuration(v); err == nil && prev > 0 {
					d = min(prev*2, usageFallbackMax)
				}
			}
			e.setKV(ctx, store.KVToolBackoff(tool), d.String())
			until = now.Add(d)
		}
	}
	// A new pause holds on its own proof: a stamp an earlier pause left
	// (`magnum resume` keeps it) is not this one's.
	e.delKV(ctx, store.KVToolPausedLogin(tool), kvToolPausedLoginAt(tool))
	e.setToolPause(ctx, tool, p.Kind, p.Detail, until)
	if proven {
		e.stampLogin(ctx, tool, p.ProvenAt)
	}
	e.event(ctx, "warn", "tool:"+tool, "tool.paused", fmt.Sprintf("%s paused (%s) until %s: %s", tool, p.Kind, until.Local().Format("15:04"), p.Detail), nil)
	title, body := toolToast(tool, p.Kind, until)
	e.urgent("pause:"+tool+":"+p.Kind, title, body, time.Hour)
}

// setToolPause records a tool's pause where dispatch reads it
// (kindPauseReason) and status shows it.
func (e *Engine) setToolPause(ctx context.Context, tool, reason, detail string, until time.Time) {
	e.setKV(ctx, store.KVToolPausedUntil(tool), store.FormatTime(until))
	e.setKV(ctx, store.KVToolPausedReason(tool), reason)
	e.setKV(ctx, store.KVToolPausedDetail(tool), detail)
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
	if reason, ok := e.getKV(ctx, store.KVToolPausedReason(tool)); ok && reason != "" {
		e.forgetSend(ctx, "pause:"+tool+":"+reason) // the next pause toasts at once
	}
	e.delKV(ctx, store.KVToolPausedUntil(tool), store.KVToolPausedReason(tool), store.KVToolPausedDetail(tool),
		store.KVToolPausedLogin(tool), kvToolPausedLoginAt(tool))
}

// noteLift records that tool's pause is lifted now on purpose
// (store.KVToolPauseLiftedAt): `magnum resume --tool` and, for the kinds
// Codex backs, a change of Codex login; never a pause that expires or a
// login check that passes. A reviewer's failure from before pauses the kind
// no more at a round's settle (overruledFailure).
func (e *Engine) noteLift(ctx context.Context, tool string) {
	e.setKV(ctx, store.KVToolPauseLiftedAt(tool), store.FormatTime(e.now()))
}

// liftedSince is when tool's pause was last lifted on purpose (noteLift),
// true when that was at failed, the time a health failure happened, or
// later. A zero failed (not known) is after every lift.
func (e *Engine) liftedSince(ctx context.Context, tool string, failed time.Time) (time.Time, bool) {
	if failed.IsZero() {
		return time.Time{}, false
	}
	lifted, ok := e.kvTime(ctx, store.KVToolPauseLiftedAt(tool))
	return lifted, ok && !failed.After(lifted)
}

// kvToolPausedLoginAt holds when the login stamp (store.KVToolPausedLogin)
// was recorded (store.FormatTime): where the hold loginStampHold caps began.
func kvToolPausedLoginAt(tool string) string { return tool + ".paused_login_at" }

// stampLogin records the tool's login file stamp (Agents.LoginStamp) for a
// login pause a run proved at provenAt (zero: now), once per pause, with the
// time it was recorded (kvToolPausedLoginAt): health lifts the pause only
// after the stamp changes (the operator logged in again, or a refresh got
// through) or loginStampHold passed, since the login check alone says logged
// in while a revoked refresh token fails every call (2026-10-12: two rounds
// posted without codex-review, and the pause that followed ended on the next
// check). A login file written after provenAt makes the proof stale, and
// nothing is recorded, so the login check decides: the round's settle
// paused Codex again half an hour after the failure, after the operator had
// logged in and health had lifted the first pause, stamped the new login,
// and held Codex until someone touched auth.json (2026-10-12).
func (e *Engine) stampLogin(ctx context.Context, tool string, provenAt time.Time) {
	if e.d.Agents == nil {
		return
	}
	if _, ok := e.getKV(ctx, store.KVToolPausedLogin(tool)); ok {
		return
	}
	s := e.d.Agents.LoginStamp(tool)
	if s == "" {
		return
	}
	now := e.now()
	if provenAt.IsZero() {
		provenAt = now
	}
	if changed, ok := loginStampTime(s); ok && changed.After(provenAt) {
		e.log.Info("login file changed after the run that proved the login gone; the preflight decides", "tool", tool,
			"proven_at", provenAt, "changed_at", changed)
		return
	}
	e.setKV(ctx, store.KVToolPausedLogin(tool), s)
	e.setKV(ctx, kvToolPausedLoginAt(tool), store.FormatTime(now))
}

// loginStampTime is the login file's mtime a LoginStamp names ("<mtime unix
// nanos>:<size>"); false for "absent" and anything else.
func loginStampTime(stamp string) (time.Time, bool) {
	ns, _, ok := strings.Cut(stamp, ":")
	if !ok {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(ns, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(0, n), true
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
// else the PR's live agent of the role the attention's kind names,
// attentionRole) in herdr and reveals the herdr client in the terminal, at
// most once per key and attentionWindow (the daemon must not keep stealing
// focus). Dry runs, a missing herdr client and a PR without a live session
// do nothing.
func (e *Engine) revealAttention(ctx context.Context, prID int64, sess *store.Session, kind, key string) {
	if !e.cfg.Terminal.RevealOnAttention || e.d.DryRun || e.d.Focus == nil {
		return
	}
	if sess == nil {
		s, err := e.st.LiveSessionByPRRole(ctx, prID, e.attentionRole(ctx, prID, kind))
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

// attentionRole is the role whose pane shows why the PR needs the operator,
// by the attention's kind: the fix role of the PR's watch for the fix job's
// (attention.KindFix: the fixer's report and work), else the PR's judge.
func (e *Engine) attentionRole(ctx context.Context, prID int64, kind string) string {
	if kind == attention.KindFix {
		if pr, err := e.st.PRByID(ctx, prID); err == nil {
			if repo, err := e.st.RepoByID(ctx, pr.RepoID); err == nil {
				if role, ok := e.cfg.FixRoleFor(e.cfg.WatchFor(repo.FullName())); ok {
					return role.Name
				}
			}
		}
	}
	return e.judgeOf(ctx, prID)
}

// health ends expired pauses: a logged-out CLI must pass its preflight
// first, and a login pause a run proved also needs its login file changed
// (stampLogin), or loginStampHold passed on an unchanged one; usage limits
// are lifted optimistically (the next round pauses again, with a doubled
// fallback, if the limit still holds); the hooks review's pause is
// checkCodexHooksTrust's to renew and end. It also reads the agent CLIs'
// versions (checkCLIVersions), says when PRs' launches end the same way
// (noteSharedFailures) and samples the machine's memory (checkMemory).
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
		if p.Reason == HooksReviewPauseReason {
			continue // checkCodexHooksTrust renews it and lifts it once the hooks are trusted
		}
		if e.d.DryRun {
			e.rec.Record(ctx, "tool:"+tool, "resume", "pause ("+p.Reason+") expired")
			continue
		}
		if p.Reason == string(agents.HealthLoginRequired) && e.d.Agents != nil {
			if err := e.d.Agents.Preflight(ctx, tool); err != nil {
				e.setKV(ctx, store.KVToolPausedUntil(tool), store.FormatTime(now.Add(loginRecheck)))
				e.log.Info("still paused", "tool", tool, "reason", p.Reason, "err", err)
				continue
			}
			if stamp, ok := e.getKV(ctx, store.KVToolPausedLogin(tool)); ok && stamp != "" && e.d.Agents.LoginStamp(tool) == stamp {
				since, known := e.kvTime(ctx, kvToolPausedLoginAt(tool))
				if !known { // a stamp recorded without its time: the hold counts from now
					since = now
					e.setKV(ctx, kvToolPausedLoginAt(tool), store.FormatTime(now))
				}
				if now.Sub(since) < loginStampHold {
					e.setKV(ctx, store.KVToolPausedUntil(tool), store.FormatTime(now.Add(loginRecheck)))
					e.log.Info("still paused", "tool", tool, "reason", p.Reason, "err", "the login file has not changed since a run proved the login gone")
					continue
				}
				e.clearToolPause(ctx, tool)
				e.event(ctx, "warn", "tool:"+tool, "tool.resumed", fmt.Sprintf("%s pause (%s) lifted after %s with the login file unchanged: the preflight passes; a run that fails again pauses it again",
					tool, p.Reason, humanDuration(loginStampHold)), nil)
				continue
			}
		}
		e.clearToolPause(ctx, tool)
		e.event(ctx, "info", "tool:"+tool, "tool.resumed", fmt.Sprintf("%s pause (%s) ended", tool, p.Reason), nil)
	}
	if until, ok := e.kvTime(ctx, store.KVDaemonPausedUntil); ok && !now.Before(until) {
		e.delKV(ctx, store.KVDaemonPaused, store.KVDaemonPausedReason, store.KVDaemonPausedUntil, KVDaemonPausedAt, KVDaemonPausedHeld)
		e.event(ctx, "info", "", "daemon.resumed", "automation pause ended", nil)
	}
	e.checkDrain(ctx)
	e.continueAfterDrain(ctx)
	e.probeInfra(ctx)
	e.checkCodexLogin(ctx) // codex_login.go: before the budget, whose reading a change overrules
	e.checkCodexHook(ctx)  // codex_session_hook.go: the sessions' ids and hooks.json, once a minute
	e.checkBudget(ctx)
	e.checkCLIVersions(ctx)   // cli_version.go
	e.noteSharedFailures(ctx) // shared_failure.go: after the versions it names
	e.checkMemory(ctx)        // memory.go: at most once a minute
}

// userPause is `magnum pause` ("" = none): it holds automatic reviews,
// while a review the user asks for (a forced PR: `magnum review`, the board,
// the picker) still runs.
func (e *Engine) userPause(ctx context.Context) string {
	if v, ok := e.getKV(ctx, store.KVDaemonPaused); ok && v == "1" {
		r, _ := e.getKV(ctx, store.KVDaemonPausedReason)
		return strings.TrimSpace("daemon paused " + r)
	}
	return ""
}

// stopping says why the daemon starts nothing new now ("" = it may): a
// shutdown cancelled ctx, the daemon's (a registry read then fails, so a
// pause, a drain or the day's retro already done would all read as absent,
// and a retro started on that stopped at once), or it drains for a restart.
func (e *Engine) stopping(ctx context.Context) string {
	if ctx.Err() != nil {
		return "the daemon is stopping"
	}
	if v, ok := e.getKV(ctx, KVDaemonDraining); ok && v != "" {
		return "draining for a restart (magnum daemon-restart --drain)"
	}
	return ""
}

// holdReason holds every new round, forced or not, and every retro and notes
// curation ("" = none): a shutdown or a drain for a restart (stopping), or
// an infrastructure pause.
func (e *Engine) holdReason(ctx context.Context) string {
	if why := e.stopping(ctx); why != "" {
		return why
	}
	return e.infraHold(ctx)
}

// refusingHold is holdReason less the drain ("" = none): a shutdown or an
// infrastructure pause, which `magnum retro` and `magnum notes --curate`
// refuse, since nobody can say when either ends. A drain for a restart ends
// with the restart, so those requests wait it out in their queues
// (drainStarts), as `magnum fix` waits it out held (holdFix).
func (e *Engine) refusingHold(ctx context.Context) string {
	if ctx.Err() != nil {
		return "the daemon is stopping"
	}
	return e.infraHold(ctx)
}

// infraHold is the infrastructure pause as a hold ("" = none).
func (e *Engine) infraHold(ctx context.Context) string {
	if p, ok := e.infraPause(ctx); ok {
		return fmt.Sprintf("infrastructure paused (%s), next probe %s", p.Reason, p.Until.Local().Format("15:04"))
	}
	return ""
}

// drainStarts is when a request queued or held behind a drain for a
// restart starts, for its answer and its event.
const drainStarts = "starts after the restart (the daemon is draining)"

// configDirEnsurer is implemented by *identity.App.
type configDirEnsurer interface {
	EnsureConfigDir(ctx context.Context) (string, error)
}

type expirer interface{ Expiry() time.Time }

// warmIdentities runs every identity's health Check (all when full, else
// only unhealthy ones) and records the verdict (checkIdentity: one that
// could not reach GitHub is retried before it counts).
func (e *Engine) warmIdentities(ctx context.Context, full bool) {
	if e.d.DryRun {
		return
	}
	for _, name := range e.identityNames() {
		if !full {
			if ok, _ := e.identityHealthy(ctx, name); ok {
				continue
			}
		}
		e.checkIdentity(ctx, name)
	}
}

// recordIdentityVerdict stores an identity check's verdict where dispatch
// reads it (store.KVIdentityCheck "pass" or "fail", store.KVIdentityError the
// reason). A fail that turns into a pass forgets the "identity unhealthy"
// toast's dedup record, so the next failure notifies at once; a new failure
// reason is logged as identity.unhealthy. The daemon's own checks
// (checkIdentity, from warmIdentities and retryIdentities) and `magnum
// identities check` (ReqIdentityVerdict) both record through it.
func (e *Engine) recordIdentityVerdict(ctx context.Context, name string, pass bool, reason string) {
	if pass {
		if prev, _ := e.getKV(ctx, store.KVIdentityCheck(name)); prev == "fail" {
			e.forgetSend(ctx, "identity:"+name)
		}
		e.setKV(ctx, store.KVIdentityCheck(name), "pass")
		e.delKV(ctx, store.KVIdentityError(name))
		return
	}
	if reason == "" {
		reason = "check failed"
	}
	prev, _ := e.getKV(ctx, store.KVIdentityError(name))
	e.setKV(ctx, store.KVIdentityCheck(name), "fail")
	e.setKV(ctx, store.KVIdentityError(name), reason)
	if prev != reason {
		e.event(ctx, "warn", "identity:"+name, "identity.unhealthy", "identity "+name+" unhealthy: "+reason, nil)
	}
}

func firstFail(rep identity.Report) string {
	for _, l := range rep.Lines {
		if after, ok := strings.CutPrefix(l, "FAIL "); ok {
			return after
		}
	}
	return "check failed"
}

// refreshIdentities keeps App tokens and GH_CONFIG_DIR files fresh (every
// tick, so long-lived panes never see an expired token). A refresh that
// could not reach GitHub is retried on its backoff (identity_net.go), not
// every tick.
func (e *Engine) refreshIdentities(ctx context.Context) {
	if e.d.DryRun {
		return
	}
	now := e.now()
	for _, name := range e.identityNames() {
		d, ok := e.d.Identities[name].(configDirEnsurer)
		if !ok {
			continue
		}
		if next, ok := e.netNext(netTokenKey(name)); ok && now.Before(next) {
			continue
		}
		if _, err := d.EnsureConfigDir(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			e.tokenFailed(ctx, name, err)
			continue
		}
		e.netEnded(netTokenKey(name))
		if _, had := e.getKV(ctx, store.KVIdentityTickError(name)); had {
			e.forgetSend(ctx, "identity-token:"+name)
			e.delKV(ctx, store.KVIdentityTickError(name))
		}
		if x, ok := e.d.Identities[name].(expirer); ok && !x.Expiry().IsZero() {
			e.setKV(ctx, store.KVIdentityTokenExpiry(name), store.FormatTime(x.Expiry()))
		}
	}
}

// tokenFailed records a failed token refresh of name where dispatch reads it
// (store.KVIdentityTickError), with the "token refresh failed" toast: a real
// failure at once, a connection-class one only once it lasted
// identityNetGrace (netRetrying).
func (e *Engine) tokenFailed(ctx context.Context, name string, err error) {
	msg := err.Error()
	key := netTokenKey(name)
	prev, had := e.getKV(ctx, store.KVIdentityTickError(name))
	if cause := github.ConnectionCause(msg); cause != "" {
		r, retried := e.netRetrying(ctx, name, "token refresh", key, cause, msg, had)
		if retried {
			return
		}
		msg = r
	} else {
		e.netEnded(key)
	}
	if prev != msg {
		e.setKV(ctx, store.KVIdentityTickError(name), msg)
		e.event(ctx, "warn", "identity:"+name, "identity.token_error", "token refresh failed: "+msg, nil)
	}
	e.urgent("identity-token:"+name, "magnum: identity "+name+" token refresh failed", msg, identityToastSpan)
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
	if v, _ := e.getKV(ctx, store.KVIdentityCheck(name)); v == "fail" {
		r, _ := e.getKV(ctx, store.KVIdentityError(name))
		return false, "identity " + name + " unhealthy: " + r
	}
	if v, ok := e.getKV(ctx, store.KVIdentityTickError(name)); ok {
		return false, "identity " + name + " token refresh failed: " + v
	}
	return true, ""
}

// isLogin reports a login-required error from Preflight.
func isLogin(err error) bool { return errors.Is(err, agents.ErrLoginRequired) }
