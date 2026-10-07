package engine

// A judge whose prompt cache has gone cold starts fresh (DECISIONS "A cold
// judge starts in a fresh session"): Codex's prompt cache lasts about 1.5
// hours and sessions park after 2, so 25 of 39 resumed judge turns started
// cold, their first turn re-reading the whole conversation uncached (4.7M
// tokens in 2.3 days). A judge whose last turn on the PR ended longer than
// [pipeline] judge_fresh_after ago starts the next round in a fresh session
// instead, through the path a lost session takes: a re-review becomes a
// recovery, whose prompt reads the earlier reviews and threads from GitHub,
// and a delta check or a same-head re-review runs with a fresh judge
// (checkFresh). Only the judge; the reviewers keep their conversations, and
// a continue finishes its paused turn where it was.

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// coldJudge reports whether the round's judge starts in a fresh session
// because its conversation's prompt cache has gone cold: judge_fresh_after
// is set, the round is not a continue, the judge has a conversation to
// resume (a live session, or ResumeID's) and its last turn on the PR ended
// longer than judge_fresh_after ago (judgeLastTurn). A live judge is quit
// first, which parks its conversation, and starts fresh once herdr no
// longer lists the quit agent (quitAgentGone); an agent that works or is
// blocked, one Quit cannot stop and one herdr keeps listing are resumed as
// before. A cold re-review's judge starts and works at its rereview effort
// (judgeEffort, pipeline.RoundInput.ColdJudge). why says why, for
// checkFresh; the decision is a round.judge_fresh_cold event with the idle
// time.
func (e *Engine) coldJudge(ctx context.Context, job *roundJob, rs *roundSetup) (cold bool, why string) {
	after := e.cfg.Pipeline.JudgeFreshAfter.Duration
	if after <= 0 || job.kind == kindContinue || e.d.Agents == nil {
		return false, ""
	}
	i := slices.IndexFunc(rs.toRun, func(r config.Role) bool { return r.Judge && r.IsAgent() })
	if i < 0 {
		return false, ""
	}
	judge, pr := rs.toRun[i], job.pr
	last := e.judgeLastTurn(ctx, pr.ID, judge)
	idle := e.now().Sub(last)
	if last.IsZero() || idle <= after {
		return false, ""
	}
	live, err := e.st.LiveSessionByPRRole(ctx, pr.ID, judge.Name)
	isLive := err == nil && live.State == store.SessionLive && deref(live.AgentName) != ""
	if !isLive {
		if id, _ := e.d.Agents.ResumeID(ctx, pr.ID, agents.Role(judge.Name)); id == "" {
			return false, "" // nothing to resume: the judge starts fresh anyway
		}
	} else {
		switch herdr.Status(deref(live.AgentStatus)) {
		case herdr.StatusWorking, herdr.StatusBlocked:
			return false, ""
		}
		if err := e.d.Agents.Quit(ctx, live); err != nil {
			e.log.Warn("cold judge: quit failed; resuming it", "pr", pr.ID, "err", err)
			return false, ""
		}
		if !e.quitAgentGone(ctx, deref(live.AgentName)) {
			e.log.Warn("cold judge: herdr still lists the agent it quit; resuming it", "pr", pr.ID, "agent", deref(live.AgentName))
			return false, ""
		}
	}
	ended := fmt.Sprintf("last turn ended %s ago (judge_fresh_after %s)", humanDuration(idle.Round(time.Minute)), humanDuration(after))
	e.event(ctx, "info", prSubject(job.repo, pr.Number), "round.judge_fresh_cold",
		"the judge starts in a fresh session: its "+ended+", so its prompt cache is cold",
		map[string]any{"idle_seconds": int64(idle.Seconds()), "last_turn_at": last.UTC().Format(time.RFC3339),
			"fresh_after": after.String(), "kind": job.kind, "was_live": isLive})
	return true, "the judge's " + ended
}

// How long coldJudge waits for herdr to drop the agent it quit
// (quitAgentGone): quitGoneChecks snapshots quitGonePoll apart, about 10 s.
const (
	quitGonePoll   = 500 * time.Millisecond
	quitGoneChecks = 20
)

// quitAgentGone waits until herdr no longer lists the agent named name,
// which Quit just stopped. herdr may still list a quitting agent for a
// moment, and a fresh start under the same name adopts it
// (agents.StartAgent): of the 3 cold judges that were live, 2 adopted the
// quitting agent and lost it within a minute. It reports false when the
// name is still listed after quitGoneChecks snapshots, or herdr could not
// say: the caller resumes the judge instead.
func (e *Engine) quitAgentGone(ctx context.Context, name string) bool {
	if name == "" || e.d.Herdr == nil {
		return true
	}
	for i := 1; ; i++ {
		if snap, err := e.d.Herdr.Snapshot(ctx); err == nil {
			if _, listed := snap.AgentByName(name); !listed {
				return true
			}
		}
		if i >= quitGoneChecks || e.d.Sleep(ctx, quitGonePoll) != nil {
			return false
		}
	}
}

// judgeLastTurn is when the PR's last turn of judge ended (its run rows'
// ended_at, under the role's name or an alias); zero when none ended. Only
// a run that reached the judge (submitted_at) is a turn: a prompt refused
// before it was sent ends its run too, and the conversation it never
// reached stays as cold as it was.
func (e *Engine) judgeLastTurn(ctx context.Context, prID int64, judge config.Role) time.Time {
	runs, err := e.st.RunsByPR(ctx, prID)
	if err != nil {
		e.log.Warn("cold judge: runs", "pr", prID, "err", err)
		return time.Time{}
	}
	var last time.Time
	for _, r := range runs {
		if r.SubmittedAt != nil && r.EndedAt != nil && r.EndedAt.After(last) && judge.Matches(r.Role) {
			last = *r.EndedAt
		}
	}
	return last
}
