package engine

// Quiet hours (DECISIONS "Quiet hours hold full rounds, not the judge
// alone"): [daemon] quiet_hours keep automatic rounds from spending the
// operator's subscriptions while he does not watch. They held every one,
// about 11% of colleagues' pushes until 09:00 UTC with them, though a delta
// check of a small delta and a reply round run the judge alone for a few
// minutes (about 0.2 Codex points). Inside them dispatch now holds only a
// round that is not a re-review of the judge alone (roundJob.judgeOnly), the
// soft cap's distinction (softCapHolds): first reviews, full re-reviews and
// the continue of a paused turn wait for their end, and their wait says
// "quiet hours" (waitFor).

import (
	"fmt"
)

// judgeOnly reports whether the job is a re-review of the judge alone: a
// delta check, a re-review of the same head, a reply round or a judge check
// of red CI (both of the same head). A checkout that turns it into a full
// round (confirmDeltaCheck, confirmSameHead) does so after it started.
func (job *roundJob) judgeOnly() bool {
	return job.deltaCheck || job.sameHead || job.replies > 0 || job.ciRed
}

// quietHoursGate is why [daemon] quiet_hours hold job now (the zero gate =
// go): a round nobody forced waits inside them, unless it is a re-review of
// the judge alone that continues no paused turn
// (eligibility.QuietHoursHold); a completion runs reviewers too, so it
// waits.
func (e *Engine) quietHoursGate(job *roundJob) gate {
	spec := e.cfg.Daemon.QuietHours
	if job.pr.Forced || !quietHoursHold(spec, e.now(), job.judgeOnly() && job.complete == nil && !job.continued) {
		return gate{}
	}
	return gate{reason: WaitQuietHours,
		text: fmt.Sprintf("quiet hours (%s): first reviews and full re-reviews wait; delta checks, reply rounds, CI checks and `magnum review` still run", spec)}
}
