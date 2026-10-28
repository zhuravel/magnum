package pipeline

// Completions (DECISIONS "A review that missed a reviewer is completed"):
// from 2026-10-25 to 2026-10-28 six clean
// reviews went without codex-review, whose pane a cut `codex review` kept
// busy, and auto-approval refused each for the reviewer it did not hear.
// The engine now completes such a review on its head (its complete.go): a
// same-head re-review that runs, besides the judge in its session, only the
// roles the review went without (RoundInput.Complete). They get the prompts
// the round whose reports the review rests on gave them (Completion.Since:
// the commits since the review before it, or the whole PR), and the judge
// gets the other roles' reports of that round, as a continue reads its
// paused round's (existingReports), and the late ones, marked late
// (agents.Report.Late): it decides the late reports' findings, and the
// others stand as its last review judged them. Its record of missing
// reports lists what it still went without, so auto-approval's gates
// decide as after any round. When every late role misses again, the judge
// has nothing new to decide: it is not prompted, and the round ends
// unheard (OutcomeUnheard) with nothing posted.

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
)

// Completion is what a completion round completes (RoundInput.Complete):
// the clean review of round Round, whose judge went without the reports of
// Roles. Base is the round whose reviewers' reports that review rests on:
// Round itself, the round a round of the judge alone carried the missing
// reports from, or, for a later completion, the first one's Base. The
// completion runs Roles alone, with the reviewer prompts Base gave them:
// a re-review of the commits since Since (with ForcePushed and BaseMerged
// of those commits), or of the whole PR when Since is "". The judge gets
// the reports of Base's other roles (their latest runs of rounds Base to
// Round on the head), and the late ones. A continue of a paused completion
// carries it too (KindContinue): its judge's reports come from rounds Base
// to the paused round.
type Completion struct {
	Round int
	Base  int
	Roles []string
	// Since is the head the reviewers of Base re-reviewed from; "" = they
	// reviewed the whole PR (a first review, a recovery).
	Since string
	// ForcePushed: Since is no longer in the branch; BaseMerged: the
	// commits since it merged a branch in (RoundInput's, for Since).
	ForcePushed, BaseMerged bool
	// Resume: the round resumes the completion round RoundInput.Round, which
	// a stop of the daemon cut short before its judge was prompted, when the
	// head has no completion round left: no role runs again, and the judge
	// gets the late reports that round's runs verified and the standing ones
	// (none verified: the round ends unheard).
	Resume bool
}

// completes is in's completion: a same-head re-review or recovery that
// completes a review, or the continue of a paused one; nil otherwise.
func completes(in RoundInput) *Completion {
	switch {
	case in.Complete == nil:
		return nil
	case in.Kind == KindContinue:
		return in.Complete
	case in.SameHead && (in.Kind == KindRereview || in.Kind == KindRecovery):
		return in.Complete
	}
	return nil
}

// completion is the round's completion (completes); nil when it is none.
func (rd *round) completion() *Completion { return completes(rd.in) }

// late reports whether role's report is one the completion's judge gets
// late: the completion runs it because the review it completes went
// without it.
func (rd *round) late(role string) bool {
	c := rd.completion()
	return c != nil && slices.Contains(c.Roles, role)
}

// CompletionRoles is the roles a completion runs: the first judge of toRun
// (RolesToRun's) and the roles of roles (the round's candidates) that names
// lists (Completion.Roles), whatever their runs say: they ran in the round
// it completes. The engine lays out the round's panes by it, and RunRound
// runs it, so both agree.
func CompletionRoles(roles, toRun []config.Role, names []string) []config.Role {
	var out []config.Role
	for _, r := range toRun {
		if r.Judge {
			out = append(out, r)
			break
		}
	}
	for _, r := range roles {
		if !r.Judge && slices.Contains(names, r.Name) {
			out = append(out, r)
		}
	}
	return out
}

// reruns names the roles c's round runs again: its Roles, none when it
// resumes a round that ran them (Resume).
func (c *Completion) reruns() []string {
	if c.Resume {
		return nil
	}
	return c.Roles
}

// lateUnheard reports whether every late role of the completion (one it
// runs, or, resumed, one its round ran) left no usable report: its judge
// has nothing new to decide.
func (rd *round) lateUnheard() bool {
	if rd.completion() == nil || rd.in.Kind == KindContinue {
		return false
	}
	for _, role := range rd.order {
		if !rd.late(role.Name) {
			continue
		}
		if rep, ok := rd.report(role); ok && rep.Status == ReportOK {
			return false
		}
	}
	return true
}

// unheard ends a completion whose late roles all missed again
// (lateUnheard): its judge is not prompted, nothing is posted, and no
// record of missing reports is written, so the review it completes keeps
// its own (OutcomeUnheard).
func (rd *round) unheard(ctx context.Context) (RoundResult, error) {
	rd.event(ctx, "info", "round.unheard", fmt.Sprintf("round %d heard none of %s again: no judge prompt, nothing posted",
		rd.in.Round, strings.Join(rd.in.Complete.Roles, ", ")), map[string]any{"roles": rd.in.Complete.Roles})
	return rd.done(ctx, OutcomeUnheard, nil)
}

// reviewersSince is what the round's reviewers re-review from: the head the
// previous review covered, and whether the commits since were force-pushed
// or merged a branch in; a completion's reviewers re-review from what the
// round it completes gave them (Completion.Since, "" = the whole PR).
func (rd *round) reviewersSince() (prev string, forcePushed, baseMerged bool) {
	if c := rd.completion(); c != nil && rd.in.Kind != KindContinue {
		return c.Since, c.ForcePushed, c.BaseMerged
	}
	return rd.previousHead(), rd.in.ForcePushed, rd.in.BaseMerged
}
