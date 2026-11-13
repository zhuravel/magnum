package engine

// The calls a fix loop carries from cycle to cycle (FixState.Carried). A
// re-review that posts new findings, or a `Description: ✗` line, runs the
// next cycle while cycles are left (continueFixLoop), even when the fixer
// of the cycle before left the author calls: those calls go on with the
// loop, each cycle's under its own report (whose files place their
// threads), and are asked at the loop's end with the last cycle's, wherever
// it ends: after a re-review, at a settle whose fixer pushed nothing, or on
// a failed run (failFixTyped). A call is asked once. A later cycle's entry
// with the same comment id is that call now (the place it names may have
// moved with a push), and what a later cycle was handed again is that
// cycle's fixer's to leave again or answer (carriedLeft): a conflict with
// the base its job checked again, a description entry when its review has
// the description line, another reviewer's thread, review body or comment
// that its external file gave its fixer open. Anything else goes on, and
// the loop's end dates each carried call at the run that made it
// (carriedCallTimes), so the author's word in its thread while the loop
// ran is a decision on it. magnum's own threads are read again before each
// end, as the last cycle's are (freshCarried, freshNeedsYou). A stop
// (`magnum abort`) ends the loop quietly, its carried calls with it, as it
// does the last cycle's.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/fixwork"
	"github.com/zhuravel/magnum/internal/github"
)

// carryOn is carried with the calls of the cycle at report (needs, and the
// threads among them answered asks only to resolve) after it, as the next
// cycle takes them on; carried itself when that cycle left none.
func carryOn(carried []FixCarry, report string, needs []string, answered map[string]bool) []FixCarry {
	if len(needs) == 0 {
		return carried
	}
	return append(slices.Clone(carried), FixCarry{Report: report, NeedsYou: slices.Clone(needs), Answered: answeredOf(needs, answered)})
}

// carriedLeft is carried without what the cycle whose report is report,
// and whose fixer left entries, asks again or was handed again (took), by
// fixwork.CarriedLeft's rule: the cycle's external file beside report says
// which items of other reviewers it gave its fixer open (heldOpen), and
// the files beside each group's report whose thread an entry is in
// (threadOwners). A group left with no entry goes.
func carriedLeft(carried []FixCarry, report string, entries []string, took laterWork) []FixCarry {
	if len(carried) == 0 {
		return nil
	}
	var heldOpen func(needsYouEntry) bool
	if x, handed := readExternalFile(filepath.Dir(report)); handed {
		heldOpen = x.heldOpen
	}
	return fixwork.CarriedLeft(carried, entries, took, heldOpen, func(report string) func(needsYouEntry) (bool, bool) {
		return readThreadOwners(filepath.Dir(report)).owner
	})
}

// heldOpen reports whether x, an external file, gave its fixer the item of
// another reviewer that n names open: the thread of its comment id, or the
// review body or comment of its id and kind, unanswered or holding the
// author's decision, and not waiting for the author.
func (x externalItems) heldOpen(n needsYouEntry) bool {
	id, err := strconv.ParseInt(n.ID, 10, 64)
	if err != nil {
		return false
	}
	if n.Place == remarkReview || n.Place == remarkComment {
		return slices.ContainsFunc(x.Conversation, func(r externalRemark) bool {
			return r.ID == id && r.Kind == n.Place && !r.Answered && !r.Waits
		})
	}
	return slices.ContainsFunc(x.Threads, func(t externalThread) bool {
		return t.CommentID == id && (!t.Answered || t.Decision) && !t.Waits
	})
}

// freshCarried is carried as magnum's own threads show it now, each group
// by its own report's files (freshNeedsYou: a resolved thread asks nothing,
// one with the answer its call asks for only the resolution); dir and read
// are freshNeedsYou's. A group left with no entry goes.
func (e *Engine) freshCarried(ctx context.Context, job *fixJob, carried []FixCarry, dir string, read func() ([]github.Thread, error)) []FixCarry {
	var out []FixCarry
	for _, c := range carried {
		at := *job
		at.state.Report = c.Report
		needs, answered := e.freshNeedsYou(ctx, &at, c.NeedsYou, c.AnsweredSet(), dir, read)
		if len(needs) == 0 {
			continue
		}
		out = append(out, FixCarry{Report: c.Report, NeedsYou: needs, Answered: answeredOf(needs, answered)})
	}
	return out
}

// carriedCallTimes are the calls the job's loop carries from its earlier
// cycles (FixState.Carried), by the comment id of each entry, at the time
// of the run whose report holds it (fixRunTime): the loop's end dates them
// there (fixAttention, recordFixCalls), not at the last report's run, so
// the author's word in such a thread while the loop ran came after the
// call and is a decision on it. A report or run that cannot be read dates
// nothing (the last report's run then does).
func (e *Engine) carriedCallTimes(ctx context.Context, job *fixJob) map[int64]time.Time {
	out := map[int64]time.Time{}
	for _, c := range job.state.Carried {
		b, err := os.ReadFile(c.Report)
		if err != nil {
			continue
		}
		runID, _ := agents.ReportRun(b)
		at := e.fixRunTime(ctx, job.pr.ID, runID)
		if runID == "" || at.IsZero() {
			continue
		}
		for _, entry := range c.NeedsYou {
			n, ok := readNeedsYouEntry(entry)
			if id, err := strconv.ParseInt(n.ID, 10, 64); ok && err == nil {
				if _, seen := out[id]; !seen {
					out[id] = at
				}
			}
		}
	}
	return out
}

// loopCalls are the calls of a loop's end: those carried from its earlier
// cycles (carried, oldest first), then the last cycle's entries at report
// with the threads answered names, each by needsYouCallsOf, with the
// entries magnum cannot read as calls (left). A call is named once, by its
// comment id: a later cycle's entry for it wins.
func loopCalls(carried []FixCarry, entries []string, answered map[string]bool, report, prURL, ref string) ([]attention.FixCall, []string) {
	if len(carried) == 0 {
		return needsYouCallsOf(entries, answered, report, prURL, ref)
	}
	groups := append(slices.Clone(carried), FixCarry{Report: report, NeedsYou: entries})
	seen := map[string]bool{}
	type part struct {
		calls []attention.FixCall
		left  []string
	}
	parts := make([]part, len(groups))
	for i := len(groups) - 1; i >= 0; i-- { // the newest first, so that its entry of a call wins
		g := groups[i]
		ans := g.AnsweredSet()
		if i == len(groups)-1 {
			ans = answered
		}
		calls, left := needsYouCallsOf(g.NeedsYou, ans, g.Report, prURL, ref)
		calls = slices.DeleteFunc(calls, func(c attention.FixCall) bool { return c.Thread != "" && seen[c.Thread] })
		left = slices.DeleteFunc(left, func(entry string) bool {
			n, ok := readNeedsYouEntry(entry)
			return ok && n.ID != "" && seen[n.ID]
		})
		for _, c := range calls {
			seen[c.Thread] = c.Thread != "" || seen[c.Thread]
		}
		for _, entry := range left {
			if n, ok := readNeedsYouEntry(entry); ok && n.ID != "" {
				seen[n.ID] = true
			}
		}
		parts[i] = part{calls: calls, left: left}
	}
	var calls []attention.FixCall
	var left []string
	for _, p := range parts {
		calls, left = append(calls, p.calls...), append(left, p.left...)
	}
	return calls, left
}

// loopNeedsText is needsYouText over a loop's calls (loopCalls): the
// carried ones and the last cycle's, each once, with the last report as the
// details.
func loopNeedsText(carried []FixCarry, entries []string, answered map[string]bool, report, prURL, ref string, named bool) string {
	if len(carried) == 0 {
		return needsYouText(entries, answered, report, prURL, ref, named)
	}
	calls, left := loopCalls(carried, entries, answered, report, prURL, ref)
	switch {
	case len(calls) > 0:
		return attention.FixCallsText(calls, moreWords(left), report)
	case named:
		return needsYourCall(left, report)
	}
	return needYourCall(left) + ": " + report
}

// carriedText is the calls the job's loop carries (FixState.Carried) in a
// failure's words ("1 finding needs your call (…): <report>", the last
// carried cycle's report as the details); "" when it carries none.
func carriedText(job *fixJob) string {
	carried := job.state.Carried
	if len(carried) == 0 {
		return ""
	}
	return loopNeedsText(carried, nil, nil, carried[len(carried)-1].Report, job.pr.URL, job.label(), true)
}
