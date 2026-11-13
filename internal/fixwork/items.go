package fixwork

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// Conflict is a merge of the PR's base into its head that conflicts:
// the base branch, the tip of it magnum fetched and the conflicting paths,
// sorted (zero: none).
type Conflict struct {
	Base, SHA string
	Paths     []string
}

// Words say what the fixer does about c: "merges master at c0c0c0c (2
// conflicting files)".
func (c Conflict) Words() string {
	return fmt.Sprintf("merges %s at %s (%s)", c.Base, textx.ShortSHA(c.SHA), textx.Count(len(c.Paths), "conflicting file", "conflicting files"))
}

// MagnumItemsOf are magnum's items: its threads of mine whose rebuttal after
// sum (magnum's last review that stands at the head; nil: none) waits for
// an answer, the findings of sum the fixer skipped once a finished loop was
// handed it (handed), sum itself when it leaves the fixer work, and the
// conflict of head with its base (zero: none). optional are the threads,
// by their first comment's id, of the operator's open optional calls
// (fix_optional.go): no skipped finding, and no work in sum. described: sum's
// body has a `Description: ✗` line, which is work (ReviewWork).
func MagnumItemsOf(mine []MagnumThread, sum *store.ReviewSummary, handed bool, conflict Conflict, head string, optional map[int64]bool,
	described bool) []Item {
	var out []Item
	var since time.Time
	if sum != nil {
		since = sum.At
	}
	for _, mt := range mine {
		if mt.Rebuttal && mt.LatestAt.After(since) {
			out = append(out, Item{Key: fmt.Sprintf("mthread:%d@%d", mt.RootID, mt.LatestID), Class: ClassMagnum, Kind: ItemMagnum, URL: mt.URL})
		}
	}
	if handed && sum != nil {
		for _, mt := range mine {
			if mt.ReviewID != 0 && mt.ReviewID == sum.ReviewID && !mt.Answered && !optional[mt.RootID] {
				out = append(out, Item{Key: fmt.Sprintf("rthread:%d", mt.RootID), Class: ClassMagnum, Kind: ItemFinding, URL: mt.URL})
			}
		}
	}
	if sum != nil && ReviewWork(*sum, OptionalSplit(mine, sum, optional), described) {
		out = append(out, Item{Key: ReviewKey(sum.RunID), Class: ClassMagnum, Kind: ItemFindings, URL: sum.URL})
	}
	if conflict.SHA != "" && head != "" {
		out = append(out, Item{Key: conflictKey(head), Class: ClassConflict, Kind: ItemConflict})
	}
	return out
}

// conflictKey is the key of a conflict of head with its base.
func conflictKey(head string) string { return "conflict:" + head[:min(12, len(head))] }

// DecisionOf reports whether items hold your decision in the thread whose
// first comment is root (Item.Decision).
func DecisionOf(items []Item, root int64) bool {
	prefix := fmt.Sprintf("thread:%d@", root)
	return slices.ContainsFunc(items, func(it Item) bool { return it.Decision && strings.HasPrefix(it.Key, prefix) })
}

// ReviewWork reports whether sum, magnum's review, leaves the fixer work of
// its own: new findings (P0 to P3), simplifications, or earlier findings it
// keeps open, the optional P3s among them (rule A1: an optional finding
// counts as open until a fix or a fix-marked reply), but for calls of its
// P3s that a fixer left the operator as optional calls (fix_optional.go):
// the operator's, never the fixer's again. A call counts against the P3s
// of its own review alone (calls.fresh against sum's new P3s, calls.kept
// against the earlier ones sum keeps open), so a call of an earlier review
// never hides a P3 a later review posts. described: its body's
// `Description: ✗` line (reviewDescribes), which the fixer of the
// operator's own PR fixes (skills/magnum-fix/SKILL.md, section 5), is work
// too. prepareFix starts a job on such a review without asking the other
// reviewers (no calls: a start asked for may hand the fixer a call again),
// and autofix = "open" counts the review as an item (openItemsOf) by the
// same rule with the calls its read shows open, so the look never starts
// what prepareFix refuses.
func ReviewWork(sum store.ReviewSummary, calls P3Calls, described bool) bool {
	p3 := max(sum.Counts[3]-calls.Fresh, 0) + max(OpenP3(sum)-calls.Kept, 0)
	return described || sum.Counts[0]+sum.Counts[1]+sum.Counts[2]+sum.Simplifications+sum.OpenProblems()+p3 > 0
}

// P3Calls counts the operator's open optional calls against a review's
// P3s (ReviewWork): fresh are those in threads of the review's own new
// P3s, kept those of an earlier review's, which it may keep open.
type P3Calls struct{ Fresh, Kept int }

// OptionalSplit counts the optional calls whose threads optional holds
// (by their first comment's id) against sum (P3Calls): a call whose thread
// mine places in sum's review is fresh, any other kept.
func OptionalSplit(mine []MagnumThread, sum *store.ReviewSummary, optional map[int64]bool) P3Calls {
	var c P3Calls
	for _, mt := range mine {
		switch {
		case !optional[mt.RootID]:
		case sum != nil && mt.ReviewID != 0 && mt.ReviewID == sum.ReviewID:
			c.Fresh++
		default:
			c.Kept++
		}
	}
	return c
}

// OpenP3 is the earlier P3s sum keeps open; 0 when its result gave only the
// number of open findings (OpenProblems counts them all then).
func OpenP3(sum store.ReviewSummary) int {
	if sum.OpenCounts == nil {
		return 0
	}
	return sum.OpenCounts[3]
}

// OptionalLinks are the calls' links.
func OptionalLinks(calls []OptionalCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.URL
	}
	return out
}

// OptionalCallsWords names the optional calls whose links are links, as
// the idle verdict and the factory screen's merge move end with them: "1
// optional call: <link>", "2 optional calls: <link>, <link>"; "" for none.
func OptionalCallsWords(links []string) string {
	if len(links) == 0 {
		return ""
	}
	return textx.Count(len(links), "optional call", "optional calls") + ": " + strings.Join(links, ", ")
}
