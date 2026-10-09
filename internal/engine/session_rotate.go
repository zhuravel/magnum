package engine

// A role's session is started fresh once it served the role's
// max_session_rounds (DECISIONS "A role's session starts fresh after
// max_session_rounds rounds"): the Codex judge of a PR was refused by
// Codex's cybersecurity classifier in round 15 of a session resumed across
// all fifteen rounds (29M tokens of history, 272k in the refused turn), and
// such a refusal flags the PR for good. When a round starts, a role whose
// conversation on the PR served that many rounds is started in a fresh
// session instead of resumed: the judge the way a cold judge is (the round
// becomes a recovery whose prompt has it read its earlier reviews and
// threads from GitHub, at its rereview effort; the reviewers keep theirs),
// another role alone, with the round's prompt and effort.

import (
	"context"
	"fmt"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// renewal is a role whose session rotateSessions retired: the round starts
// it fresh. rounds is how many rounds the old session served.
type renewal struct {
	role   config.Role
	rounds int
}

// renewRoles retires the sessions of roles that served their
// max_session_rounds (rotateSessions) and records them in rs: a judge among
// them is the round's fresh judge (roundSetup.freshJudge), whom startRoles
// starts the way it starts a cold one, the others go to rs.renew. why says
// why the judge starts fresh, for checkFresh's events ("" when it does
// not).
func (e *Engine) renewRoles(ctx context.Context, job *roundJob, rs *roundSetup, roles []config.Role) (why string, err error) {
	renewed, err := e.rotateSessions(ctx, job, roles)
	for _, r := range renewed {
		if r.role.Judge {
			rs.freshJudge = true
			why = fmt.Sprintf("the judge's session served %d rounds (max_session_rounds %d)", r.rounds, r.role.MaxSessionRounds)
			continue
		}
		rs.renew = append(rs.renew, r.role.Name)
	}
	return why, err
}

// rotateSessions retires, when a round starts, the session of each of
// roles whose conversation on the PR served the role's max_session_rounds
// (roundsServed), so that the round starts the role fresh (startRoles):
// a live session is quit first, which parks its conversation (history
// kept), and the role starts once herdr no longer lists the quit agent; a
// parked or lost one is not resumed. A session that works or is blocked,
// or that Quit cannot stop, serves this round as before (quitToRenew), and
// herdr still listing a quit agent fails the setup as busy. A continue, or
// a round that continues a paused one, rotates nothing: its paused turn
// ends in the session it began in. Each rotation is an
// agent.session_rotated event ("<role>: fresh session after N rounds").
func (e *Engine) rotateSessions(ctx context.Context, job *roundJob, roles []config.Role) ([]renewal, error) {
	if job.kind == kindContinue || job.continued || e.d.Agents == nil {
		return nil, nil
	}
	pr := job.pr
	var out []renewal
	var sessions []store.Session
	var runs []store.Run
	loaded := false
	for _, role := range roles {
		if role.MaxSessionRounds <= 0 || !role.IsAgent() {
			continue
		}
		live, err := e.st.LiveSessionByPRRole(ctx, pr.ID, role.Name)
		isLive := err == nil && live.State == store.SessionLive && deref(live.AgentName) != ""
		var from *store.Session
		resume := ""
		if isLive {
			from = &live
		} else if resume, _ = e.d.Agents.ResumeID(ctx, pr.ID, agents.Role(role.Name)); resume == "" {
			continue // nothing to resume: the role starts fresh anyway
		}
		if !loaded {
			if sessions, err = e.st.SessionsByPR(ctx, pr.ID); err == nil {
				runs, err = e.st.RunsByPR(ctx, pr.ID)
			}
			if err != nil {
				e.log.Warn("session rotation: the PR's sessions and runs", "pr", pr.ID, "err", err)
				return out, nil // the roles resume as before
			}
			loaded = true
		}
		n := roundsServed(sessions, runs, role.Name, from, resume)
		if n < role.MaxSessionRounds {
			continue
		}
		if isLive {
			if quit, err := e.quitToRenew(ctx, live, "session rotation of "+role.Name); !quit || err != nil {
				if err != nil {
					return out, err
				}
				continue
			}
		}
		e.event(ctx, "info", prSubject(job.repo, pr.Number), "agent.session_rotated",
			fmt.Sprintf("%s: fresh session after %d rounds", role.Name, n),
			map[string]any{"role": role.Name, "rounds": n, "max_session_rounds": role.MaxSessionRounds,
				"was_live": isLive, "kind": job.kind})
		out = append(out, renewal{role: role, rounds: n})
	}
	return out, nil
}

// roundsServed counts the review rounds role's conversation on the PR
// served: the distinct round numbers of the runs that reached it
// (submitted_at) on any session row of that conversation. A continue, a
// nudge and the judge's own pass are runs of the round they belong to, so a
// round counts once. The conversation is from's (the live session row) or
// resume's (the agent session id a parked or lost one resumes, ResumeID):
// every row of the role that carries its agent session id, and the rows of
// the ids those were resumed from (resumed_from), since each resume is a
// row of its own (a new generation) and a CLI may give the resumed
// conversation a new id.
func roundsServed(sessions []store.Session, runs []store.Run, role string, from *store.Session, resume string) int {
	rows, ids := map[int64]bool{}, map[string]bool{}
	add := func(id string) {
		if id != "" {
			ids[id] = true
		}
	}
	if from != nil {
		rows[from.ID] = true
		add(deref(from.SessionID))
		add(deref(from.ResumedFrom))
	}
	add(resume)
	for grew := true; grew; {
		grew = false
		for _, s := range sessions {
			if s.Role != role || rows[s.ID] || !ids[deref(s.SessionID)] {
				continue
			}
			rows[s.ID], grew = true, true
			add(deref(s.ResumedFrom))
		}
	}
	served := map[int]bool{}
	for _, r := range runs {
		if r.SessionID != nil && rows[*r.SessionID] && r.SubmittedAt != nil {
			served[r.Round] = true
		}
	}
	return len(served)
}
