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
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/eligibility"
)

// judgeOnly reports whether the job is a re-review of the judge alone: a
// delta check, a re-review of the same head or a reply round (one of the
// same head). A checkout that turns it into a full round (confirmDeltaCheck,
// confirmSameHead) does so after it started.
func (job *roundJob) judgeOnly() bool {
	return job.deltaCheck || job.sameHead || job.replies > 0
}

// quietHoursGate is why [daemon] quiet_hours hold job now (the zero gate =
// go): a round nobody forced waits inside them, unless it is a re-review of
// the judge alone that continues no paused turn
// (eligibility.QuietHoursHold).
func (e *Engine) quietHoursGate(job *roundJob) gate {
	spec := e.cfg.Daemon.QuietHours
	if job.pr.Forced || !quietHoursHold(spec, e.now(), job.judgeOnly() && !job.continued) {
		return gate{}
	}
	return gate{reason: WaitQuietHours,
		text: fmt.Sprintf("quiet hours (%s): first reviews and full re-reviews wait; delta checks, reply rounds and `magnum review` still run", spec)}
}

// quietHoursHold reports whether [daemon] quiet_hours (spec) hold, at now, a
// round nobody forced; judgeAlone: it is a re-review of the judge alone,
// which they let start (eligibility.QuietHoursHold).
func quietHoursHold(spec string, now time.Time, judgeAlone bool) bool {
	return spec != "" && eligibility.QuietHoursHold(spec, now.Local(), judgeAlone)
}

// quietHoursEnd is the next local time quiet_hours ("HH:MM-HH:MM") end
// after now; zero when spec cannot be read.
func quietHoursEnd(spec string, now time.Time) time.Time {
	w, ok, err := config.ParseQuietHours(spec)
	if err != nil || !ok {
		return time.Time{}
	}
	n := now.Local()
	at := time.Date(n.Year(), n.Month(), n.Day(), w.End/60, w.End%60, 0, 0, n.Location())
	if !at.After(n) {
		at = at.AddDate(0, 0, 1)
	}
	return at
}
