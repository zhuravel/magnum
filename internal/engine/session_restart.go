package engine

// Restarting live agent sessions in new processes (DECISIONS "A change of
// Codex login restarts the Codex sessions that run with the old one"): a
// running agent keeps the login it started with, so after the operator
// switched Codex accounts every turn of a live Codex session went on
// spending the old account's budget (2026-10-22: five codex-judge sessions
// started under the old account, four rounds ended at its usage limit). A
// session marked for a restart parks through the early park
// (Agents.ParkSessions: its pane closes, its conversation stays resumable)
// once it is idle, so its next prompt resumes it (`codex resume <id>`) in a
// new process with the login current then; a busy one parks after its
// turn. No round starts on its PR before (prGate), so no prompt reaches
// the old process. A change of Codex login and a usage limit of a session
// older than the login mark them (codex_login.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// kvSessionsRestart holds the sessions marked for a restart, as JSON by
// session id ({"1530":{"pr":12,"why":"the Codex login changed"}}), so a
// daemon restart keeps the ones still waiting for their turn to end.
const kvSessionsRestart = "sessions.restart"

// restartHeld is what dispatch shows of a PR whose marked sessions are
// being parked (heldReason).
const restartHeld = "its sessions restart in new processes"

// restartMark is one session marked for a restart: its PR, and why (the
// events say it).
type restartMark struct {
	PR  int64  `json:"pr"`
	Why string `json:"why"`
}

// restartMarksLocked loads the marks from kv on their first use. The
// caller holds restartMu.
func (e *Engine) restartMarksLocked(ctx context.Context) map[int64]restartMark {
	if e.restarts != nil {
		return e.restarts
	}
	e.restarts = map[int64]restartMark{}
	if v, ok := e.getKV(ctx, kvSessionsRestart); ok && v != "" {
		if err := json.Unmarshal([]byte(v), &e.restarts); err != nil {
			e.log.Warn("sessions to restart: unreadable record dropped", "err", err)
			e.restarts = map[int64]restartMark{}
		}
	}
	return e.restarts
}

// saveRestartsLocked writes the marks to kv (none: the key goes). The
// caller holds restartMu.
func (e *Engine) saveRestartsLocked(ctx context.Context) {
	if len(e.restarts) == 0 {
		e.delKV(ctx, kvSessionsRestart)
		return
	}
	b, err := json.Marshal(e.restarts)
	if err != nil {
		e.log.Warn("sessions to restart: record", "err", err)
		return
	}
	e.setKV(ctx, kvSessionsRestart, string(b))
}

// restartMarks is a copy of the marks, by session id.
func (e *Engine) restartMarks(ctx context.Context) map[int64]restartMark {
	e.restartMu.Lock()
	defer e.restartMu.Unlock()
	return maps.Clone(e.restartMarksLocked(ctx))
}

// markRestart marks sessions for a restart, why saying what for; it
// returns the ids it marked now (a session marked already keeps its first
// mark).
func (e *Engine) markRestart(ctx context.Context, sessions []store.Session, why string) []int64 {
	e.restartMu.Lock()
	defer e.restartMu.Unlock()
	marks := e.restartMarksLocked(ctx)
	var added []int64
	for _, s := range sessions {
		if _, ok := marks[s.ID]; !ok {
			marks[s.ID] = restartMark{PR: s.PRID, Why: why}
			added = append(added, s.ID)
		}
	}
	if len(added) > 0 {
		e.saveRestartsLocked(ctx)
	}
	return added
}

// unmarkRestart drops the marks of ids.
func (e *Engine) unmarkRestart(ctx context.Context, ids ...int64) {
	if len(ids) == 0 {
		return
	}
	e.restartMu.Lock()
	defer e.restartMu.Unlock()
	marks := e.restartMarksLocked(ctx)
	n := len(marks)
	for _, id := range ids {
		delete(marks, id)
	}
	if len(marks) != n {
		e.saveRestartsLocked(ctx)
	}
}

// restartWait is why the PR waits for its marked sessions to restart ("" =
// none is marked): prGate holds its rounds until they are parked, so the
// next prompt resumes them in new processes and none reaches the old ones.
func (e *Engine) restartWait(ctx context.Context, prID int64) string {
	var roles []string
	why := ""
	for id, m := range e.restartMarks(ctx) {
		if m.PR != prID {
			continue
		}
		if s, err := e.st.SessionByID(ctx, id); err == nil && isLive(s) {
			roles = append(roles, s.Role)
			why = m.Why
		}
	}
	if len(roles) == 0 {
		return ""
	}
	slices.Sort(roles)
	return fmt.Sprintf("%s restarts in a new process first (%s)", strings.Join(roles, ", "), why)
}

// isLive reports whether a session is starting or live (store's live
// states).
func isLive(s store.Session) bool {
	return s.State == store.SessionLive || s.State == store.SessionStarting
}

// restartSessions, on every tick before dispatch, drops the marks of
// sessions no longer live (something else parked or closed them) and queues
// the park of each PR's marked sessions that may park now (queueRestart).
func (e *Engine) restartSessions(ctx context.Context, ts tickState) {
	marks := e.restartMarks(ctx)
	if len(marks) == 0 || e.d.Agents == nil {
		return
	}
	live, err := e.st.LiveSessions(ctx)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "restart sessions: live sessions")
		return
	}
	byPR := map[int64][]store.Session{}
	alive := map[int64]bool{}
	for _, s := range live {
		alive[s.ID] = true
		if _, ok := marks[s.ID]; ok {
			byPR[s.PRID] = append(byPR[s.PRID], s)
		}
	}
	var gone []int64
	for id := range marks {
		if !alive[id] {
			gone = append(gone, id)
		}
	}
	e.unmarkRestart(ctx, gone...)
	if !ts.herdrUp {
		return
	}
	now := e.now()
	for _, prID := range slices.Sorted(maps.Keys(byPR)) {
		e.queueRestart(ctx, prID, byPR[prID], sessionsOf(live, prID), ts.busyPR[prID], now)
	}
}

// restartBlocked says why the PR's marked sessions cannot park now ("" =
// they can): a round, a fix or a whiteboard job holds the PR, or another
// park does; one of its agents works or is blocked (busy: the tick saw one
// of its live agents at work); the PR or its slot is pinned; a human typed
// into its panes within human_cooldown; its last park failed and waits out
// its backoff (parkHeld). all are the PR's live sessions.
func (e *Engine) restartBlocked(ctx context.Context, pr store.PR, marked, all []store.Session, busy bool, now time.Time) string {
	if e.roundActive(pr.ID) || busy {
		return "its agents are at work"
	}
	for _, s := range marked {
		switch st := herdr.Status(deref(s.AgentStatus)); st {
		case herdr.StatusWorking, herdr.StatusBlocked:
			return s.Role + " is " + string(st)
		}
	}
	if e.pinnedPR(ctx, pr) {
		return "it is pinned"
	}
	if now.Before(e.cfg.Daemon.CooldownUntil(pr.HumanActiveAt)) {
		return "a human is active in its panes"
	}
	if e.parkHeld(pr, all, now, false) {
		return "its last park failed"
	}
	return ""
}

// pinnedPR reports whether pr or its slot is pinned (`magnum pin`, `magnum
// open`): its sessions are the operator's, never restarted.
func (e *Engine) pinnedPR(ctx context.Context, pr store.PR) bool {
	if pr.Pinned {
		return true
	}
	pinned, err := prPinned(ctx, e.st, pr.ID)
	return err != nil || pinned
}

// queueRestart queues the park of the PR's marked sessions (marked; all
// are its live sessions) on the heavy worker when nothing blocks it
// (restartBlocked), under the PR's slot-work reservation taken here, so a
// round dispatch would start in the same tick waits for it; it reports
// whether it queued one. A dry run only records it.
func (e *Engine) queueRestart(ctx context.Context, prID int64, marked, all []store.Session, busy bool, now time.Time) bool {
	pr, err := e.st.PRByID(ctx, prID)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "restart sessions: pr", "pr", prID)
		return false
	}
	if e.restartBlocked(ctx, pr, marked, all, busy, now) != "" {
		return false
	}
	if e.d.DryRun {
		e.rec.Record(ctx, e.prLabelSubject(ctx, pr), "restart", strings.Join(sessionRoles(marked), ", ")+" in new processes")
		return false
	}
	if !e.reserveSlotWork(prID, restartHeld) {
		return false
	}
	if !e.enqueueHeavy(fmt.Sprintf("restart:%d", prID), e.restartJob(prID, now)) {
		e.endEviction(prID)
		return false
	}
	return true
}

// restartJob parks the PR's marked sessions under the reservation
// queueRestart took (and ends it), checked again there: the resumable ones
// through the early park (Agents.ParkSessions: each one's pane closes, its
// conversation stays for the next prompt to resume), the others, which
// have no conversation to resume, by Quit (closed: they start fresh). The
// sessions it parked lose their mark and get one agent.restarted event; a
// failure counts as the tick's try at `at` (parkFailed), whose backoff the
// next try waits out, the mark kept.
func (e *Engine) restartJob(prID int64, at time.Time) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		defer e.endEviction(prID)
		pr, err := e.st.PRByID(ctx, prID)
		if err != nil {
			return err
		}
		live, err := e.st.LiveSessions(ctx)
		if err != nil {
			return err
		}
		marks := e.restartMarks(ctx)
		all := sessionsOf(live, prID)
		var take []store.Session
		for _, s := range all {
			if _, ok := marks[s.ID]; ok {
				take = append(take, s)
			}
		}
		if len(take) == 0 || e.restartBlocked(ctx, pr, take, all, false, e.now()) != "" {
			return nil
		}
		subject := e.prLabelSubject(ctx, pr)
		parked, err := e.parkToRestart(ctx, pr, take)
		if len(parked) > 0 {
			ids := make([]int64, len(parked))
			var what []string
			for i, s := range parked {
				ids[i] = s.ID
				what = append(what, fmt.Sprintf("%s (%s)", s.Role, marks[s.ID].Why))
			}
			e.unmarkRestart(ctx, ids...)
			if err == nil {
				e.parkWorked(prID)
			}
			e.event(ctx, "info", subject, "agent.restarted",
				fmt.Sprintf("parked %s: the next prompt resumes them in new processes", strings.Join(what, ", ")),
				map[string]any{"roles": sessionRoles(parked), "sessions": ids})
			e.closeRetiredPanes(ctx, pr) // retired_panes.go: a pane the park had to leave, a quit session's
		}
		if err != nil {
			e.parkFailed(ctx, pr, subject, at, err, true)
		}
		return nil
	}
}

// parkToRestart parks sessions of pr: the resumable ones (resumableSession)
// through the early park, the others by Quit once the early park parked
// all of its own. It returns the sessions it parked beside the failures.
func (e *Engine) parkToRestart(ctx context.Context, pr store.PR, sessions []store.Session) ([]store.Session, error) {
	var resumable, other []store.Session
	for _, s := range sessions {
		if e.resumableSession(s) {
			resumable = append(resumable, s)
		} else {
			other = append(other, s)
		}
	}
	var parked []store.Session
	if len(resumable) > 0 {
		ids := make([]int64, len(resumable))
		for i, s := range resumable {
			ids[i] = s.ID
		}
		got, err := e.d.Agents.ParkSessions(ctx, pr, ids)
		parked = append(parked, got...)
		if err != nil {
			return parked, err
		}
	}
	var errs []error
	for _, s := range other {
		if err := e.d.Agents.Quit(ctx, s); err != nil {
			errs = append(errs, err)
			continue
		}
		parked = append(parked, s)
	}
	return parked, errors.Join(errs...)
}

// restartRequested is the mark of a session `magnum restart-sessions`
// restarts (restartMark.Why).
const restartRequested = "magnum restart-sessions"

// RestartSessionsPayload is a `magnum restart-sessions` request: the live
// sessions of agent kind Tool ("all": every kind) park so that their next
// prompt resumes them in new processes, only the PR's when it names one.
type RestartSessionsPayload struct {
	PRTarget
	Tool string `json:"tool"`
}

// RestartableSessions are the live sessions `magnum restart-sessions --tool
// kind` restarts: each one runs an agent of kind (its recorded kind, else
// its role's; "all" = any kind; a shell role's pane never), of PR prID
// unless it is 0, and pinned counts those it leaves alone because their PR
// or its slot is pinned. The settings screen counts them for its question.
func RestartableSessions(ctx context.Context, st *store.Store, cfg *config.Config, kind string, prID int64) (sessions []store.Session, pinned int, err error) {
	live, err := st.LiveSessions(ctx)
	if err != nil {
		return nil, 0, err
	}
	pins := map[int64]bool{}
	for _, s := range live {
		if prID != 0 && s.PRID != prID {
			continue
		}
		r, ok := cfg.RoleByNameOrAlias(nil, s.Role)
		if ok && r.IsShell() {
			continue
		}
		k := deref(s.AgentKind)
		if k == "" && ok {
			k = r.AgentKind()
		}
		if kind != "all" && k != kind {
			continue
		}
		p, seen := pins[s.PRID]
		if !seen {
			if p, err = prPinned(ctx, st, s.PRID); err != nil {
				return nil, 0, err
			}
			pins[s.PRID] = p
		}
		if p {
			pinned++
			continue
		}
		sessions = append(sessions, s)
	}
	return sessions, pinned, nil
}

// prPinned reports whether PR prID or its slot is pinned (`magnum pin`,
// `magnum open`): its sessions are the operator's.
func prPinned(ctx context.Context, st *store.Store, prID int64) (bool, error) {
	pr, err := st.PRByID(ctx, prID)
	if err != nil {
		return false, err
	}
	if pr.Pinned {
		return true, nil
	}
	sl, err := st.SlotByPR(ctx, prID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return sl.Pinned, nil
}

// requestRestartSessions is `magnum restart-sessions`: it marks the
// sessions RestartableSessions gives for a restart and queues the park of
// each PR's that may park now (queueRestart); the others park after their
// turn (restartSessions). The answer counts both, and the sessions of
// pinned PRs it left alone.
func (e *Engine) requestRestartSessions(ctx context.Context, p RestartSessionsPayload) (string, error) {
	switch {
	case p.Tool == "":
		return "", errors.New("which agent kind? --tool <kind> or all")
	case p.Tool != "all" && !e.isKind(p.Tool):
		return "", fmt.Errorf("unknown tool %q (%s or all)", p.Tool, strings.Join(e.kinds(), ", "))
	}
	var prID int64
	of := ""
	if p.Ref != "" || p.Repo != "" {
		repo, pr, err := e.resolve(ctx, p.PRTarget)
		if err != nil {
			return "", err
		}
		prID, of = pr.ID, fmt.Sprintf(" of %s#%d", repo.FullName(), pr.Number)
	}
	sessions, pinned, err := RestartableSessions(ctx, e.st, e.cfg, p.Tool, prID)
	if err != nil {
		return "", err
	}
	what := "live " + p.Tool + " session"
	if p.Tool == "all" {
		what = "live agent session"
	}
	note := ""
	if pinned > 0 {
		note = "; " + textx.Count(pinned, "session", "sessions") + " of pinned PRs left alone"
	}
	if len(sessions) == 0 {
		return "no " + what + of + " to restart" + note, nil
	}
	e.markRestart(ctx, sessions, restartRequested)
	live, err := e.st.LiveSessions(ctx)
	if err != nil {
		return "", err
	}
	byPR := map[int64][]store.Session{}
	for _, s := range sessions {
		byPR[s.PRID] = append(byPR[s.PRID], s)
	}
	now, later := 0, 0
	at := e.now()
	herdrDown := e.herdrUp != nil && !*e.herdrUp
	for _, id := range slices.Sorted(maps.Keys(byPR)) {
		all := sessionsOf(live, id)
		busy := slices.ContainsFunc(all, func(s store.Session) bool {
			st := herdr.Status(deref(s.AgentStatus))
			return st == herdr.StatusWorking || st == herdr.StatusBlocked
		})
		if !herdrDown && e.queueRestart(ctx, id, byPR[id], all, busy, at) {
			now += len(byPR[id])
		} else {
			later += len(byPR[id])
		}
	}
	turn := "their turn"
	if later == 1 {
		turn = "its turn"
	}
	return fmt.Sprintf("restarting %s%s: %d park now, %d after %s; each resumes in a new process at its next prompt%s",
		textx.Count(len(sessions), what, what+"s"), of, now, later, turn, note), nil
}
