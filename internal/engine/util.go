package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store"
)

// kv keys the engine maintains (never secrets). The names live in store
// (store.KV*) so the CLI reads exactly what the daemon writes.
const (
	kvLastTick      = store.KVDaemonLastTick
	kvLastPoll      = store.KVDaemonLastPoll
	kvLastReconcile = store.KVDaemonLastReconcile
	kvStartedAt     = store.KVDaemonStartedAt
	kvPid           = store.KVDaemonPid
	// KVDaemonPaused is "1" while automation is paused (magnum pause);
	// KVDaemonPausedReason and KVDaemonPausedUntil (optional) explain it.
	KVDaemonPaused       = store.KVDaemonPaused
	KVDaemonPausedReason = store.KVDaemonPausedReason
	KVDaemonPausedUntil  = store.KVDaemonPausedUntil
	kvGHRemaining        = store.KVGHRemaining
	kvGHLimit            = store.KVGHLimit
	kvGHReset            = store.KVGHReset
	kvGHPausedUntil      = store.KVGHPollPausedUntil
	kvHerdrUp            = store.KVHerdrUp
)

// kv keys of the daemon's own operations, read by `magnum status`.
const (
	// KVDaemonDraining is set (to the time it started) by `magnum
	// daemon-restart --drain`: no new round starts; the restart, or the
	// command giving up, deletes it.
	KVDaemonDraining = "daemon.draining"
	// KVInfraPausedUntil holds when the infrastructure probe runs next while
	// dispatch is paused for an infrastructure failure (infra.go);
	// KVInfraPausedReason names the cause, KVInfraPausedDetail is the
	// failure's redacted text and KVInfraBackoff the current wait.
	KVInfraPausedUntil  = "daemon.infra_paused_until"
	KVInfraPausedReason = "daemon.infra_paused_reason"
	KVInfraPausedDetail = "daemon.infra_paused_detail"
	KVInfraBackoff      = "daemon.infra_backoff"
	kvInfraProbeDir     = "daemon.infra_probe_dir"
	// kvDriftLogged holds, as JSON, the day (store.DayKey) each drift finding
	// the reconcile logs was last logged on, by kind and subject
	// (driftDue, maintenance.go).
	kvDriftLogged = "daemon.drift_logged"
	// KVUsageCodexPercent is the Codex budget used (percent, no decimals)
	// as the newest Codex session reported it; KVUsageCodexResetsAt is when
	// the binding window resets (store.FormatTime), KVUsageCodexWindow its
	// length in minutes, KVUsageCodexPlan the plan and KVUsageCodexAt when
	// Codex reported it (budget.go). KVUsageCodexPace24h is the binding
	// window's pace over the last 24 hours (the share of the budget used in
	// them over the share of the window they are, two decimals); absent
	// when Codex's reading from a day ago is unknown or of another window.
	KVUsageCodexPercent  = "usage.codex_percent"
	KVUsageCodexResetsAt = "usage.codex_resets_at"
	KVUsageCodexWindow   = "usage.codex_window_minutes"
	KVUsageCodexPlan     = "usage.codex_plan"
	KVUsageCodexAt       = "usage.codex_at"
	KVUsageCodexPace24h  = "usage.codex_pace_24h"
)

// KVToolPausedUntil is the kv key holding when a tool's pause ends ("codex",
// "claude"; store.FormatTime). Same as store.KVToolPausedUntil.
func KVToolPausedUntil(tool string) string { return store.KVToolPausedUntil(tool) }

// KVToolPausedReason is the kv key holding why a tool is paused
// (usage_limit | login_required). Same as store.KVToolPausedReason.
func KVToolPausedReason(tool string) string { return store.KVToolPausedReason(tool) }
func kvToolPausedDetail(tool string) string { return store.KVToolPausedDetail(tool) }
func kvToolBackoff(tool string) string      { return store.KVToolBackoff(tool) }

// KVIdentityCheck is the kv key holding "pass" or "fail" from the identity's
// last Check. A tick-time token refresh failure is kept under
// identity.<name>.tick_error; an identity is healthy unless either says so.
// Same as store.KVIdentityCheck.
func KVIdentityCheck(name string) string { return store.KVIdentityCheck(name) }

// KVIdentityError is the kv key holding why the identity's Check failed.
// Same as store.KVIdentityError.
func KVIdentityError(name string) string     { return store.KVIdentityError(name) }
func kvIdentityTickError(name string) string { return store.KVIdentityTickError(name) }
func kvIdentityExpiry(name string) string    { return store.KVIdentityTokenExpiry(name) }

// KVWatchPaused holds the reason a watch owner's automation was paused
// (identity leak); magnum resume --watch clears it. Same as
// store.KVWatchPaused.
func KVWatchPaused(owner string) string { return store.KVWatchPaused(owner) }

func kvPRSimplify(id int64) string         { return store.KVPRSimplify(id) }
func kvPRFresh(id int64) string            { return store.KVPRFresh(id) }
func kvPRSessionsIdentity(id int64) string { return store.KVPRSessionsIdentity(id) }
func kvPRGate(id int64) string             { return store.KVPRGate(id) }
func kvPRDryRun(id int64) string           { return store.KVPRDryRun(id) }

// kvPRGateReason holds the code of the PR's dispatch gate (gateCode as
// JSON) next to its sentence (store.KVPRGate): its wait is built from the
// code, never from the words (gateWait).
func kvPRGateReason(id int64) string { return fmt.Sprintf("pr.%d.gate_reason", id) }

// isStop reports whether a failure is the daemon stopping rather than a
// fault: ctx ended (a shutdown cancels the tick's), or err says the work was
// canceled. Every registry read then fails, so what it returned ("" for a
// key, nothing for a list) says nothing; callers that decide on it check
// ctx.Err() as well.
func isStop(ctx context.Context, err error) bool { return execx.IsStop(ctx, err) }

// warnUnlessStopped logs err under msg with args at Warn, or at Debug when
// the failure is the daemon stopping (isStop): every stop used to log dozens
// of warnings for the tick it cut short.
func (e *Engine) warnUnlessStopped(ctx context.Context, err error, msg string, args ...any) {
	level := slog.LevelWarn
	if isStop(ctx, err) {
		level = slog.LevelDebug
	}
	e.log.Log(ctx, level, msg, append(args, "err", err)...)
}

// getKV reads key; a failed read answers "" and false, and is logged unless
// the daemon is stopping (warnUnlessStopped).
func (e *Engine) getKV(ctx context.Context, key string) (string, bool) {
	v, ok, err := e.st.GetKV(ctx, key)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "kv read", "key", key)
		return "", false
	}
	return v, ok
}

func (e *Engine) setKV(ctx context.Context, key, value string) {
	if err := e.st.SetKV(ctx, key, value); err != nil {
		e.warnUnlessStopped(ctx, err, "kv write", "key", key)
	}
}

func (e *Engine) delKV(ctx context.Context, keys ...string) {
	for _, k := range keys {
		if err := e.st.DeleteKV(ctx, k); err != nil {
			e.warnUnlessStopped(ctx, err, "kv delete", "key", k)
		}
	}
}

func (e *Engine) kvTime(ctx context.Context, key string) (time.Time, bool) {
	v, ok := e.getKV(ctx, key)
	if !ok || v == "" {
		return time.Time{}, false
	}
	t, err := store.ParseTime(v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// event appends an audit row (redacted) and mirrors it to the log.
func (e *Engine) event(ctx context.Context, level, subject, kind, msg string, data map[string]any) {
	msg = execx.Redact(msg)
	ev := store.Event{Level: level, Kind: kind, Message: msg}
	if subject != "" {
		ev.Subject = &subject
	}
	if len(data) > 0 {
		if b, err := json.Marshal(data); err == nil {
			ev.Data = json.RawMessage(execx.Redact(string(b)))
		}
	}
	if _, err := e.st.AppendEvent(ctx, ev); err != nil {
		e.warnUnlessStopped(ctx, err, "append event", "kind", kind)
	}
	e.log.Log(ctx, execx.EventLevel(level), msg, "subject", subject, "kind", kind)
}

// prSubject is the audit subject of a PR, shared with the pipeline
// ("pr:owner/name#N").
func prSubject(repo store.Repo, number int) string {
	return fmt.Sprintf("pr:%s/%s#%d", repo.Owner, repo.Name, number)
}

// PlannedOp is one side effect a dry run decided on but did not perform.
type PlannedOp struct {
	At      time.Time `json:"at"`
	Subject string    `json:"subject"`
	Action  string    `json:"action"`
	Detail  string    `json:"detail,omitempty"`
}

// Recorder collects a dry run's planned side effects: it logs "would …",
// keeps the ops for Engine.Planned and writes a dryrun.<action> event (into
// the dry run's private store copy).
type Recorder struct {
	log *slog.Logger
	st  *store.Store
	now func() time.Time

	mu  sync.Mutex
	ops []PlannedOp
}

// Record notes one planned side effect.
func (r *Recorder) Record(ctx context.Context, subject, action, detail string) {
	op := PlannedOp{At: r.now(), Subject: subject, Action: action, Detail: execx.Redact(detail)}
	r.mu.Lock()
	r.ops = append(r.ops, op)
	r.mu.Unlock()
	r.log.Info("dry-run: would "+action, "subject", subject, "detail", op.Detail)
	if r.st != nil {
		s := subject
		_, _ = r.st.AppendEvent(ctx, store.Event{Kind: "dryrun." + action, Subject: &s,
			Message: strings.TrimSpace("would " + action + " " + subject + ": " + op.Detail)})
	}
}

// Ops returns a copy of the recorded ops.
func (r *Recorder) Ops() []PlannedOp {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]PlannedOp(nil), r.ops...)
}

// heavyJob is one unit of slow slot work for the background worker.
type heavyJob struct {
	key string
	fn  func(ctx context.Context) error
}

// enqueueHeavy queues fn unless a job with the same key is queued or
// running. It reports whether the job was queued.
func (e *Engine) enqueueHeavy(key string, fn func(ctx context.Context) error) bool {
	e.heavyMu.Lock()
	if e.heavyKeys[key] {
		e.heavyMu.Unlock()
		return false
	}
	e.heavyKeys[key] = true
	e.heavyMu.Unlock()
	select {
	case e.heavy <- heavyJob{key: key, fn: fn}:
		return true
	default:
		e.heavyMu.Lock()
		delete(e.heavyKeys, key)
		e.heavyMu.Unlock()
		e.log.Warn("heavy queue full; job dropped until the next tick", "job", key)
		return false
	}
}

func (e *Engine) runHeavy(ctx context.Context, j heavyJob) {
	defer func() {
		e.heavyMu.Lock()
		delete(e.heavyKeys, j.key)
		e.heavyMu.Unlock()
	}()
	if err := j.fn(ctx); err != nil && !errors.Is(err, context.Canceled) {
		e.log.Warn("heavy job failed", "job", j.key, "err", err)
	}
}

// heavyWorker runs heavy jobs one at a time until ctx ends.
func (e *Engine) heavyWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-e.heavy:
			e.runHeavy(ctx, j)
		}
	}
}

// heavyWorkerUntil is heavyWorker for --once: once idle is closed it runs
// what is still queued and returns.
func (e *Engine) heavyWorkerUntil(ctx context.Context, idle <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-e.heavy:
			e.runHeavy(ctx, j)
		case <-idle:
			e.drainHeavy(ctx)
			return
		}
	}
}

// drainHeavy runs every queued heavy job on the calling goroutine (tests).
func (e *Engine) drainHeavy(ctx context.Context) {
	for {
		select {
		case j := <-e.heavy:
			e.runHeavy(ctx, j)
		default:
			return
		}
	}
}

// maxCodexStarts bounds concurrent Codex launches (each clones its
// marketplaces).
const maxCodexStarts = 3

// starter staggers agent starts by daemon.agent_start_stagger and allows at
// most maxCodexStarts Codex launches at once.
type starter struct {
	mu    sync.Mutex
	next  time.Time
	codex chan struct{}
}

// wait blocks until this start's slot in the stagger schedule; for Codex it
// also takes a launch token that release returns.
func (e *Engine) waitStart(ctx context.Context, codex bool) (release func(), err error) {
	stagger := e.cfg.Daemon.AgentStartStagger.Duration
	e.starts.mu.Lock()
	now := e.now()
	at := now
	if e.starts.next.After(at) {
		at = e.starts.next
	}
	e.starts.next = at.Add(stagger)
	e.starts.mu.Unlock()
	if err := e.d.Sleep(ctx, at.Sub(now)); err != nil {
		return nil, err
	}
	if !codex {
		return func() {}, nil
	}
	select {
	case e.starts.codex <- struct{}{}:
		return func() { <-e.starts.codex }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func deref[T any](p *T) T { return store.Deref(p) }

// changed records msg as the latest value for key (tick goroutine only) and
// reports whether it differs from the previous one, so repeating conditions
// produce one audit row instead of one per tick.
func (e *Engine) changed(key, msg string) bool {
	prev, ok := e.lastSeen[key]
	e.lastSeen[key] = msg
	return !ok || prev != msg
}

// errorLogWindow is how long an error logged once stays quiet while it
// repeats: a poll failing every 30 s is one warning an hour, not 120.
const errorLogWindow = time.Hour

// volatileRe matches what differs between two occurrences of the same
// failure: a TCP connection's address and port.
var volatileRe = regexp.MustCompile(`\d{1,3}(?:\.\d{1,3}){3}:\d+`)

// logOnce reports whether msg should be logged under key: true when the
// same message (addresses and ports aside) was not logged for key within
// errorLogWindow, which it then records (tick goroutine only). Unlike
// changed, a success in between does not make a repeat new, and two errors
// that alternate are each logged once a window.
func (e *Engine) logOnce(key, msg string, now time.Time) bool {
	k := key + "\x00" + volatileRe.ReplaceAllString(msg, "<addr>")
	if at, ok := e.logged[k]; ok && now.Sub(at) < errorLogWindow {
		return false
	}
	for old, at := range e.logged {
		if now.Sub(at) >= errorLogWindow {
			delete(e.logged, old)
		}
	}
	e.logged[k] = now
	return true
}

// warnTick logs a tick's errors, each distinct one once an hour (logOnce).
func (e *Engine) warnTick(err error) {
	if e.logOnce("tick", err.Error(), e.now()) {
		e.log.Warn("tick finished with errors", "err", err)
	}
}
