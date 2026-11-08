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
// the base (the later job checks the base again), a description entry (its
// fixer judged the re-review's own line), another reviewer's thread, review
// body or comment that its external file listed. magnum's own threads are
// read again before each end, as the last cycle's are (freshCarried,
// freshNeedsYou). A stop (`magnum abort`) ends the loop quietly, its carried
// calls with it, as it does the last cycle's.

import (
	"context"
	"path/filepath"
	"slices"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/github"
)

// FixCarry is the calls one earlier cycle of a fix loop left the author
// (FixState.Carried): the cycle's report, beside which its files place the
// threads, the entries of its "## Needs you" still asked (in the fixer's
// own words, so never put in an event or a log), and the comment ids among
// them whose threads showed their answer and ask only for the resolution
// (as FixState.Answered).
type FixCarry struct {
	Report   string   `json:"report"`
	NeedsYou []string `json:"needs_you"`
	Answered []string `json:"answered,omitempty"`
}

// answered is Answered as a set (nil for none).
func (c FixCarry) answered() map[string]bool {
	return FixState{Answered: c.Answered}.answered()
}

// carryOn is carried with the calls of the cycle at report (needs, and the
// threads among them answered asks only to resolve) after it, as the next
// cycle takes them on; carried itself when that cycle left none.
func carryOn(carried []FixCarry, report string, needs []string, answered map[string]bool) []FixCarry {
	if len(needs) == 0 {
		return carried
	}
	return append(slices.Clone(carried), FixCarry{Report: report, NeedsYou: slices.Clone(needs), Answered: answeredOf(needs, answered)})
}

// answeredOf are the comment ids of entries that answered holds, sorted.
func answeredOf(entries []string, answered map[string]bool) []string {
	var out []string
	for _, entry := range entries {
		if n, ok := readNeedsYouEntry(entry); ok && n.id != "" && answered[n.id] && !slices.Contains(out, n.id) {
			out = append(out, n.id)
		}
	}
	slices.Sort(out)
	return out
}

// carriedLeft is carried without what the cycle whose report is report,
// and whose fixer left entries, asks again or was handed again: an entry
// with a comment id entries name (the later entry is the call now), a
// conflict with the base and a description entry, and, when that cycle's
// external file was written, every entry in another reviewer's thread,
// review body or comment (its fixer had every open one, and left again what
// still needs the author). A group left with no entry goes.
func carriedLeft(carried []FixCarry, report string, entries []string) []FixCarry {
	if len(carried) == 0 {
		return nil
	}
	asked := map[string]bool{}
	for _, entry := range entries {
		if n, ok := readNeedsYouEntry(entry); ok && n.id != "" {
			asked[n.id] = true
		}
	}
	_, handed := readExternalFile(filepath.Dir(report))
	var out []FixCarry
	for _, c := range carried {
		owners := readThreadOwners(filepath.Dir(c.Report))
		kept := slices.DeleteFunc(slices.Clone(c.NeedsYou), func(entry string) bool {
			n, ok := readNeedsYouEntry(entry)
			switch {
			case !ok:
				return false
			case n.id != "" && asked[n.id], n.place == conflictPlace, n.place == "description":
				return true
			case !handed:
				return false
			case n.place == remarkReview, n.place == remarkComment:
				return true
			}
			magnums, known := owners.owner(n)
			return known && !magnums
		})
		if len(kept) == 0 {
			continue
		}
		out = append(out, FixCarry{Report: c.Report, NeedsYou: kept, Answered: answeredOf(kept, c.answered())})
	}
	return out
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
		needs, answered := e.freshNeedsYou(ctx, &at, c.NeedsYou, c.answered(), dir, read)
		if len(needs) == 0 {
			continue
		}
		out = append(out, FixCarry{Report: c.Report, NeedsYou: needs, Answered: answeredOf(needs, answered)})
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
		ans := g.answered()
		if i == len(groups)-1 {
			ans = answered
		}
		calls, left := needsYouCallsOf(g.NeedsYou, ans, g.Report, prURL, ref)
		calls = slices.DeleteFunc(calls, func(c attention.FixCall) bool { return c.Thread != "" && seen[c.Thread] })
		left = slices.DeleteFunc(left, func(entry string) bool {
			n, ok := readNeedsYouEntry(entry)
			return ok && n.id != "" && seen[n.id]
		})
		for _, c := range calls {
			seen[c.Thread] = c.Thread != "" || seen[c.Thread]
		}
		for _, entry := range left {
			if n, ok := readNeedsYouEntry(entry); ok && n.id != "" {
				seen[n.id] = true
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
