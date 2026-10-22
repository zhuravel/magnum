package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// codexReviewLimit is a codex-review report of a usage limit whose turn
// ended at ended: of a run of the round (carried false), or carried by a
// continue from its paused round (carried true, no run).
func codexReviewLimit(t *testing.T, h *harness, in pipeline.RoundInput, round int, ended time.Time, carried bool) pipeline.RoleReport {
	t.Helper()
	rep := pipeline.RoleReport{Role: string(agents.RoleCodexReview), Kind: agents.KindCodex, Status: string(agents.HealthUsageLimit),
		Detail: "■ You've hit your usage limit"}
	if carried {
		rep.Detail, rep.CarriedFrom = "not available", ended
		return rep
	}
	run, err := h.st.CreateRun(h.ctx, store.Run{PRID: in.PR.ID, Round: round, Role: string(agents.RoleCodexReview), Kind: in.Kind,
		TargetSHA: in.TargetSHA, Identity: in.PR.Identity, State: store.RunFailed, Outcome: new(string(agents.HealthUsageLimit)),
		PromptText: "review", EndedAt: &ended})
	if err != nil {
		t.Error(err)
		return rep
	}
	rep.RunID = run.ID
	return rep
}

// TestAContinuedRoundDoesNotPauseAKindAgainForAReportFromBeforeItsLift is
// talkable#12069 (2026-10-22): round 4 paused on the old Codex account (the
// judge's usage limit, codex-review's too), the codex pause was lifted (by
// the login change, or by `magnum resume --tool codex`), and the round
// continued and posted; its settle paused codex again for an hour from the
// codex-review report the continue carried from the paused round, holding
// two other PRs. Now a carried report whose turn ended before the lift
// pauses nothing, and one info event under the PR says so; the review
// stands.
func TestAContinuedRoundDoesNotPauseAKindAgainForAReportFromBeforeItsLift(t *testing.T) {
	for _, lift := range []string{"login change", "magnum resume"} {
		t.Run(lift, func(t *testing.T) {
			h, fl := newLoginHarness(t)
			fl.set(loginOld, h.clock.Now().Add(-time.Hour))
			pr := h.reviewedPR(2, "b1")
			h.withSessionIDs(pr.ID)
			judgeLimit := judgeLimitRound(t, h)
			var reviewEnded time.Time
			h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
				if in.Kind == pipeline.KindContinue {
					return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: in.Round, ReviewID: 77, Event: "COMMENTED", ReviewCommit: in.TargetSHA,
						Reports: map[agents.Role]pipeline.RoleReport{agents.RoleCodexReview: codexReviewLimit(t, h, in, in.Round, reviewEnded, true)}}, nil
				}
				round, err := nextRound(h, in.PR.ID)
				if err != nil {
					return failRound(t, err)
				}
				reviewEnded = h.clock.Now()
				rep := codexReviewLimit(t, h, in, round, reviewEnded, false)
				res, err := judgeLimit(in)
				res.Reports = map[agents.Role]pipeline.RoleReport{agents.RoleCodexReview: rep}
				return res, err
			}
			pollPR(h, 40*time.Minute, 2, "b2")
			h.advance(5 * time.Minute)
			h.tick()
			h.wantState(2, store.PRPaused)
			if _, ok := h.e.toolPause(h.ctx, agents.KindCodex); !ok {
				t.Fatal("the judge's usage limit paused no codex")
			}

			h.advance(30 * time.Minute)
			lifted := h.clock.Now()
			if lift == "login change" {
				lifted = changeLogin(h, fl, loginNew)
				h.tick()
			} else if res, err := h.e.requestPause(h.ctx, PausePayload{Tool: agents.KindCodex}, false); err != nil || !strings.HasPrefix(res, "resumed codex") {
				t.Fatalf("resume = %q, %v", res, err)
			}
			if _, ok := h.e.toolPause(h.ctx, agents.KindCodex); ok {
				t.Fatal("the lift left codex paused")
			}
			h.advance(time.Minute)
			h.tick()
			h.advance(time.Minute)
			h.tick()
			if last := h.rd.all()[len(h.rd.all())-1]; last.Kind != pipeline.KindContinue {
				t.Fatalf("last round %s, want the continue", last.Kind)
			}
			h.wantState(2, store.PRReviewed)
			if p, ok := h.e.toolPause(h.ctx, agents.KindCodex); ok {
				t.Fatalf("the continue's settle paused codex again: %+v", p)
			}
			if evs := h.events("tool.paused"); len(evs) != 1 {
				t.Fatalf("tool.paused events = %+v, want the paused round's alone", evs)
			}
			evs := approvalEvents(t, h, 2, "round.pause_skipped")
			want := "codex-review's usage_limit is from before the codex pause was lifted at " + lifted.Local().Format("15:04") + "; not paused again"
			if len(evs) != 1 || evs[0].Level != "info" || !strings.Contains(evs[0].Message, want) {
				t.Fatalf("round.pause_skipped events = %+v, want one saying %q", evs, want)
			}
		})
	}
}

// TestOnlyAReviewersLimitAfterTheLastLiftPausesItsKind: after `magnum
// resume --tool codex`, a reviewer's usage limit pauses codex at the settle
// as before when it came after the resume, of a run of the round or carried
// by a continue, and when no lift was ever recorded; one of a run of the
// round that ended before the resume (the operator resumed while the round
// went on) pauses nothing, as a carried one does. A reviewer the round's
// login check skipped has neither a run nor a paused round's turn: its
// failure is the settle's, after every lift, and pauses the kind.
func TestOnlyAReviewersLimitAfterTheLastLiftPausesItsKind(t *testing.T) {
	for _, tc := range []struct {
		name    string
		lift    bool
		since   time.Duration // the report's turn ended this long after the resume
		carried bool
		skipped bool // a logged-out reviewer the login check skipped (no run)
		paused  bool
	}{
		{name: "a run of the round after the lift", lift: true, since: time.Minute, paused: true},
		{name: "carried, after the lift", lift: true, since: time.Minute, carried: true, paused: true},
		{name: "a run of the round before the lift", lift: true, since: -time.Minute},
		{name: "carried, before the lift", lift: true, since: -time.Minute, carried: true},
		{name: "no lift ever", since: -time.Minute, carried: true, paused: true},
		{name: "a logged-out reviewer skipped, after the lift", lift: true, skipped: true, paused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reviewedPR(2, "b1")
			h.advance(time.Hour)
			resumed := h.clock.Now()
			if tc.lift {
				h.e.setToolPause(h.ctx, agents.KindCodex, string(agents.HealthUsageLimit), "You've hit your usage limit", resumed.Add(time.Hour))
				if _, err := h.e.requestPause(h.ctx, PausePayload{Tool: agents.KindCodex}, false); err != nil {
					t.Fatal(err)
				}
			}
			h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
				round, err := nextRound(h, in.PR.ID)
				if err != nil {
					return failRound(t, err)
				}
				rep := pipeline.RoleReport{Role: string(agents.RoleCodexReview), Kind: agents.KindCodex, Status: string(agents.HealthLoginRequired),
					Detail: "Not logged in", Health: &agents.Health{Kind: agents.HealthLoginRequired, Detail: "Not logged in"}}
				if !tc.skipped {
					rep = codexReviewLimit(t, h, in, round, resumed.Add(tc.since), tc.carried)
				}
				return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: round, ReviewID: 9, Event: "COMMENTED", ReviewCommit: in.TargetSHA,
					Reports: map[agents.Role]pipeline.RoleReport{agents.RoleCodexReview: rep}}, nil
			}
			pollPR(h, 2*time.Minute, 2, "b2")
			h.advance(5 * time.Minute)
			h.tick()
			if n := len(h.rd.all()); n != 2 {
				t.Fatalf("rounds = %d, want the re-review of b2", n)
			}
			p, paused := h.e.toolPause(h.ctx, agents.KindCodex)
			want := string(agents.HealthUsageLimit)
			if tc.skipped {
				want = string(agents.HealthLoginRequired)
			}
			if paused != tc.paused || paused && p.Reason != want {
				t.Fatalf("codex paused = %v (%+v), want %v (%s)", paused, p, tc.paused, want)
			}
			if skipped := approvalEvents(t, h, 2, "round.pause_skipped"); len(skipped) != map[bool]int{true: 0, false: 1}[tc.paused] {
				t.Fatalf("round.pause_skipped events = %+v", skipped)
			}
		})
	}
}

// TestAStaleSessionsLimitFromBeforeTheLiftStillRestartsItsSession: a
// reviewer's usage limit from before the login change, hit in a Codex
// session started under the old login, goes the stale limit's way as
// before (spec 119): it pauses nothing, its session is marked for a
// restart, and codex.stale_usage_limit says so, not round.pause_skipped.
func TestAStaleSessionsLimitFromBeforeTheLiftStillRestartsItsSession(t *testing.T) {
	h, fl := newLoginHarness(t)
	fl.set(loginOld, h.clock.Now().Add(-time.Hour))
	pr := h.reviewedPR(2, "b1")
	h.withSessionIDs(pr.ID)
	reviewer := h.liveSession(pr.ID, string(agents.RoleCodexReview), agents.KindCodex, h.clock.Now(), herdr.StatusIdle)
	h.advance(time.Hour)
	failed := h.clock.Now().Add(-time.Minute)
	h.e.setKV(h.ctx, KVCodexLoginAt, store.FormatTime(h.clock.Now())) // the login changed after the session started
	h.e.noteLift(h.ctx, agents.KindCodex)
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		round, err := nextRound(h, in.PR.ID)
		if err != nil {
			return failRound(t, err)
		}
		run, err := h.st.CreateRun(h.ctx, store.Run{PRID: in.PR.ID, Round: round, Role: string(agents.RoleCodexReview), SessionID: &reviewer.ID,
			Kind: in.Kind, TargetSHA: in.TargetSHA, Identity: in.PR.Identity, State: store.RunFailed,
			Outcome: new(string(agents.HealthUsageLimit)), PromptText: "review", EndedAt: &failed})
		if err != nil {
			return failRound(t, err)
		}
		rep := pipeline.RoleReport{Role: string(agents.RoleCodexReview), Kind: agents.KindCodex, Status: string(agents.HealthUsageLimit),
			Detail: "■ You've hit your usage limit", RunID: run.ID}
		return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: round, ReviewID: 9, Event: "COMMENTED", ReviewCommit: in.TargetSHA,
			Reports: map[agents.Role]pipeline.RoleReport{agents.RoleCodexReview: rep}}, nil
	}
	pollPR(h, 2*time.Minute, 2, "b2")
	h.advance(5 * time.Minute)
	h.tick()
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d, want the re-review of b2", n)
	}
	if p, ok := h.e.toolPause(h.ctx, agents.KindCodex); ok {
		t.Fatalf("codex paused: %+v", p)
	}
	evs := subjectEvents(t, h, "pr:talkable/talkable#2")
	if got := kindOf(evs, "codex.stale_usage_limit"); len(got) != 1 {
		t.Fatalf("codex.stale_usage_limit events = %+v, want the stale session's", got)
	}
	if got := kindOf(evs, "round.pause_skipped"); len(got) != 0 {
		t.Fatalf("round.pause_skipped events = %+v, want none for a stale session's limit", got)
	}
	if _, marked := h.e.restartMarks(h.ctx)[reviewer.ID]; !marked {
		t.Fatal("the stale reviewer session is not marked for a restart")
	}
}
