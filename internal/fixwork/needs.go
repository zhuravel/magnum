package fixwork

// The settle's filters of what a fixer left the author (its report's "##
// Needs you"): the rules that decide which entry still asks the author for
// something, and what for. The engine's settle reads what they need (the
// threads on GitHub, the files beside the report) and says what changed in
// its events; these decide.

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/zhuravel/magnum/internal/github"
)

// IsConflictEntry reports whether entry is a conflict of the base merge
// the fixer aborted (ConflictPlace), which the settle drops once the push
// holds the merged tip.
func IsConflictEntry(entry string) bool {
	n, ok := ReadNeedsYouEntry(entry)
	return ok && n.Place == ConflictPlace
}

// FixerReplied are the threads of threads, by their first comment's id,
// that look holds (nil: every thread) and in which a comment after the
// first is the fixer's reply (by: a fix-marked reply the fixer could have
// posted in the run).
func FixerReplied(threads []github.Thread, look func(root int64) bool, by func(github.ThreadComment) bool) map[int64]bool {
	replied := map[int64]bool{}
	for _, t := range threads {
		if all := t.AllComments(); len(all) > 0 && (look == nil || look(all[0].ID)) && slices.ContainsFunc(all[1:], by) {
			replied[all[0].ID] = true
		}
	}
	return replied
}

// Decision is a decision of yours that the run's external file handed the
// fixer: its thread's first comment, path and line.
type Decision struct {
	CommentID int64
	Path      string
	Line      int
}

// DecisionsLeft is entries, the fixer's "## Needs you", with an entry of
// magnum's for each of decided that the fixer neither built (replied: its
// thread shows the fixer's reply) nor left there, and the decisions it
// built; back counts the decisions that go back to the author, listed by
// the fixer or added here. A person's decision never closes on the
// fixer's word alone: magnum's entry reads as the fixer's own ("<id>
// <path>:<line>: not fixed. …"), so its call asks the author to decide
// again.
func DecisionsLeft(entries []string, decided []Decision, replied map[int64]bool) (out []string, built map[int64]bool, back int) {
	listed := map[string]bool{}
	for _, entry := range entries {
		if n, ok := ReadNeedsYouEntry(entry); ok {
			listed[n.ID] = true
		}
	}
	out, built = slices.Clone(entries), map[int64]bool{}
	for _, t := range decided {
		id := strconv.FormatInt(t.CommentID, 10)
		switch {
		case replied[t.CommentID]:
			built[t.CommentID] = true
		case listed[id]:
			back++
		default:
			out = append(out, fmt.Sprintf("%s %s:%d: not fixed. magnum: GitHub shows no reply of the fixer's that builds your decision", id, t.Path, t.Line))
			back++
		}
	}
	return out, built, back
}

// ThreadNow is one of magnum's threads as it is now: whether it is
// resolved, and the class its answer carries ("" for none; the engine's
// answerOf).
type ThreadNow struct {
	Resolved bool
	Answer   string
}

// FreshEntries is entries as magnum's own threads show them now: an entry
// with a call in one of magnum's threads (mine: the entry, its thread's
// first comment and whether the thread is magnum's) whose thread (threads,
// by that id) is resolved asks nothing and goes; one whose thread is open
// with the answer its call asks for (answers, of the thread's answer)
// asks only to resolve it, and is in answered by its comment id. Every
// other entry stays as it is; resolved counts what went.
func FreshEntries(entries []string, mine func(entry string) (NeedsYouEntry, int64, bool), threads map[int64]ThreadNow,
	answers func(n NeedsYouEntry, answer string) bool) (kept []string, answered map[string]bool, resolved int) {
	kept, answered = []string{}, map[string]bool{}
	for _, entry := range entries {
		n, id, ok := mine(entry)
		t, found := threads[id]
		switch {
		case !ok || !found:
		case t.Resolved:
			resolved++
			continue
		case answers(n, t.Answer):
			answered[n.ID] = true
		}
		kept = append(kept, entry)
	}
	return kept, answered, resolved
}

// LaterWork is what a later cycle's job was handed again of what an
// earlier cycle may have left (CarriedLeft): the description, when the
// review it fixes has a `Description: ✗` line, and the base, when the
// job's guard checked it or the job was given a conflict all the same.
type LaterWork struct{ Description, Base bool }

// CarriedLeft is carried without what the cycle whose fixer left entries
// asks again or was handed again (took): an entry with a comment id
// entries name (the later entry is the call now), a conflict with the base
// and a description entry when took says so, and an entry in another
// reviewer's thread, review body or comment that the cycle's external file
// gave its fixer open (heldOpen; nil when the cycle had no external file).
// whose says whose thread an entry of a group is in, by the files beside
// that group's report (magnums: magnum's; known: the files place it).
// Every other entry was never handed again, and goes on: a call in
// magnum's own thread, and another reviewer's item the file lists
// answered, waiting, or not at all. A group left with no entry goes.
func CarriedLeft(carried []FixCarry, entries []string, took LaterWork, heldOpen func(NeedsYouEntry) bool,
	whose func(report string) func(NeedsYouEntry) (magnums, known bool)) []FixCarry {
	if len(carried) == 0 {
		return nil
	}
	asked := map[string]bool{}
	for _, entry := range entries {
		if n, ok := ReadNeedsYouEntry(entry); ok && n.ID != "" {
			asked[n.ID] = true
		}
	}
	var out []FixCarry
	for _, c := range carried {
		owner := whose(c.Report)
		kept := slices.DeleteFunc(slices.Clone(c.NeedsYou), func(entry string) bool {
			n, ok := ReadNeedsYouEntry(entry)
			switch {
			case !ok:
				return false
			case n.ID != "" && asked[n.ID]:
				return true
			case n.Place == ConflictPlace:
				return took.Base
			case n.Place == "description":
				return took.Description
			case heldOpen == nil:
				return false
			case n.Place == ItemReview, n.Place == ItemComment:
				return heldOpen(n)
			}
			magnums, known := owner(n)
			return known && !magnums && heldOpen(n)
		})
		if len(kept) == 0 {
			continue
		}
		out = append(out, FixCarry{Report: c.Report, NeedsYou: kept, Answered: AnsweredOf(kept, c.AnsweredSet())})
	}
	return out
}

// AnsweredOf are the comment ids of entries that answered holds, sorted.
func AnsweredOf(entries []string, answered map[string]bool) []string {
	var out []string
	for _, entry := range entries {
		if n, ok := ReadNeedsYouEntry(entry); ok && n.ID != "" && answered[n.ID] && !slices.Contains(out, n.ID) {
			out = append(out, n.ID)
		}
	}
	slices.Sort(out)
	return out
}

// authorsCallSentence ends the Fix of a finding of magnum's whose options
// are the author's to choose between (skills/magnum-review/SKILL.md).
const authorsCallSentence = "This is the author's call."

// OptionalBody reports whether body, a comment of magnum's, is a P3 whose
// Fix ends with the author's call.
func OptionalBody(body string) bool {
	return strings.HasPrefix(strings.TrimSpace(body), "**[P3]") && strings.Contains(body, authorsCallSentence)
}

// OptionalEntry reads entry, one of a fixer's "## Optional calls", as an
// optional call may be one: a P3 at a path and line, with its thread's
// first comment's id (id); ok is false for any other entry, which holds
// as a Needs you entry does. Whether magnum's own comment shows it to be
// one is the engine's to read (optionalFinding).
func OptionalEntry(entry string) (n NeedsYouEntry, id int64, ok bool) {
	n, ok = ReadNeedsYouEntry(entry)
	id, err := strconv.ParseInt(n.ID, 10, 64)
	if !ok || err != nil || n.Priority != "P3" || !strings.Contains(n.Place, ":") {
		return n, id, false
	}
	return n, id, true
}
