package engine

// Parking idle agents: a reviewed PR's agents sit idle until its next push,
// often for hours (21 processes, about 2.8 GB, idle 10.8 hours on average).
// Once every live agent of a reviewed PR has been idle for [daemon]
// park_idle_after the PR's sessions are parked; the next round resumes them
// as after any park.

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// parkIdle queues a park for every reviewed PR whose live agents have all
// been idle longer than park_idle_after (0 = never). The park runs on the
// heavy worker (parkJob), which checks again under the PR's reservation.
func (e *Engine) parkIdle(ctx context.Context, ts tickState) {
	after := e.cfg.Daemon.ParkIdleAfter.Duration
	if after <= 0 || e.d.Agents == nil || !ts.herdrUp {
		return
	}
	live, err := e.st.LiveSessions(ctx)
	if err != nil {
		e.log.Warn("park idle agents: sessions", "err", err)
		return
	}
	byPR := map[int64][]store.Session{}
	for _, s := range live {
		byPR[s.PRID] = append(byPR[s.PRID], s)
	}
	now := e.now()
	for _, prID := range slices.Sorted(maps.Keys(byPR)) {
		if ts.busyPR[prID] || e.roundActive(prID) {
			continue
		}
		pr, idleSince, ok := e.parkable(ctx, prID, byPR[prID], now, after)
		if !ok {
			continue
		}
		if e.d.DryRun {
			e.rec.Record(ctx, e.prLabelSubject(ctx, pr), "park", fmt.Sprintf("%d agents idle since %s", len(byPR[prID]), idleSince.Local().Format("15:04")))
			continue
		}
		e.enqueueHeavy(fmt.Sprintf("park:%d", prID), e.parkJob(prID))
	}
}

// parkable reports whether the PR's live sessions may be parked now: the PR
// is reviewed, no human typed into its panes within human_cooldown, its
// slot is neither pinned nor held for a human, and every session is idle
// (not working or blocked) since before now - after. It returns the PR and
// when its last session went idle.
func (e *Engine) parkable(ctx context.Context, prID int64, sessions []store.Session, now time.Time, after time.Duration) (store.PR, time.Time, bool) {
	pr, err := e.st.PRByID(ctx, prID)
	if err != nil || pr.State != store.PRReviewed {
		return pr, time.Time{}, false
	}
	if pr.HumanActiveAt != nil && now.Before(pr.HumanActiveAt.Add(e.cfg.Daemon.HumanCooldown.Duration)) {
		return pr, time.Time{}, false
	}
	if sl, has, err := e.slotOf(ctx, prID); err != nil || (has && (sl.Pinned || sl.HoldReason != nil)) {
		return pr, time.Time{}, false
	}
	var last time.Time
	for _, s := range sessions {
		switch herdr.Status(deref(s.AgentStatus)) {
		case herdr.StatusWorking, herdr.StatusBlocked:
			return pr, time.Time{}, false
		}
		since := s.StartedAt
		for _, t := range []*time.Time{s.AgentStatusAt, s.LastPromptAt} {
			if t != nil && t.After(since) {
				since = *t
			}
		}
		if since.After(last) {
			last = since
		}
	}
	if last.IsZero() || now.Sub(last) < after {
		return pr, last, false
	}
	return pr, last, true
}

// parkJob parks the PR's sessions under its slot-work reservation, after
// checking again that they are still parkable (a round may have started, an
// agent may have woken up). Park itself refuses a busy pane.
func (e *Engine) parkJob(prID int64) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if !e.reserveSlotWork(prID, "its idle agents are being parked") {
			return nil
		}
		defer e.endEviction(prID)
		live, err := e.st.LiveSessions(ctx)
		if err != nil {
			return err
		}
		var sessions []store.Session
		for _, s := range live {
			if s.PRID == prID {
				sessions = append(sessions, s)
			}
		}
		if len(sessions) == 0 {
			return nil
		}
		pr, idleSince, ok := e.parkable(ctx, prID, sessions, e.now(), e.cfg.Daemon.ParkIdleAfter.Duration)
		if !ok {
			return nil
		}
		subject := e.prLabelSubject(ctx, pr)
		if err := e.d.Agents.Park(ctx, pr); err != nil {
			e.log.Info("park idle agents", "subject", subject, "err", err)
			return nil
		}
		e.event(ctx, "info", subject, "pr.parked",
			fmt.Sprintf("parked %d idle agents (idle since %s); the next round resumes them", len(sessions), idleSince.Local().Format("15:04")), nil)
		return nil
	}
}

// prLabelSubject is the PR's audit subject ("pr:owner/name#N"), or
// "pr:<id>" when its repository cannot be read.
func (e *Engine) prLabelSubject(ctx context.Context, pr store.PR) string {
	if repo, err := e.st.RepoByID(ctx, pr.RepoID); err == nil {
		return prSubject(repo, pr.Number)
	}
	return fmt.Sprintf("pr:%d", pr.ID)
}
