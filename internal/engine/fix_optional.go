package engine

// The optional calls a fixer leaves the author. A P3 of magnum's whose Fix
// ends "This is the author's call." asks nothing before the merge: its
// title says "(optional, no reply needed)", and P3s never block. The fix
// skill leaves the code as it is, replies nowhere and lists it under the
// report's "## Optional calls" (skills/magnum-fix/SKILL.md), so it holds no
// PR: the settle raises no attention for it (settleOptional, once magnum's
// own comment shows the finding is such a P3) and keeps it as the
// operator's call, by its thread (kvPROptionalCalls). The look of autofix =
// "open" reads that record: such a P3 is no fixer's work any more, so no
// loop starts on it again (reviewWork, against its own review's P3s alone:
// optionalSplit; magnumItemsOf), and the idle verdict names it ("1 optional
// call: <link>") while its thread stays open and unanswered since the call
// (optionalWaits), so the operator sees it at the merge. A P0 to P2
// author's call still holds as a Needs you entry does. A run that leaves
// nothing else lifts the PR's earlier attention when its optional calls
// cover every call of it (liftDecided): a fixer from before optional calls
// left such a P3 as a call to decide, which held the PR.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// optionalCall is a P3 of magnum's that a fixer left the author as an
// optional call: the id of its thread's first comment, the thread's link
// on the PR's page, and when the settle took the call (At): the author's
// own word in the thread after it is a decision on it, as on any call the
// fixer left (fixerCalls), which `magnum fix --decide` posts.
type optionalCall struct {
	Thread int64     `json:"thread"`
	URL    string    `json:"url"`
	At     time.Time `json:"at,omitzero"`
}

// kvPROptionalCalls holds the PR's optional calls (JSON: []optionalCall,
// by thread, oldest first), which the settle adds to and the look prunes.
func kvPROptionalCalls(prID int64) string { return fmt.Sprintf("pr.%d.optional_calls", prID) }

// optionalCalls are the PR's optional calls (kvPROptionalCalls); none when
// there is no record or it cannot be read.
func (e *Engine) optionalCalls(ctx context.Context, prID int64) []optionalCall {
	raw, ok := e.getKV(ctx, kvPROptionalCalls(prID))
	if !ok {
		return nil
	}
	var calls []optionalCall
	if json.Unmarshal([]byte(raw), &calls) != nil {
		return nil
	}
	return calls
}

// setOptionalCalls records calls as the PR's optional calls; none removes
// the record.
func (e *Engine) setOptionalCalls(ctx context.Context, prID int64, calls []optionalCall) {
	if len(calls) == 0 {
		e.delKV(ctx, kvPROptionalCalls(prID))
		return
	}
	b, err := json.Marshal(calls)
	if err != nil {
		return
	}
	e.setKV(ctx, kvPROptionalCalls(prID), string(b))
}

// settleOptional is needs, the fixer's "## Needs you", with the entries of
// its "## Optional calls" (entries) that are no optional call: one that is
// not a P3 of a thread (an id, a priority and a path:line magnum can read:
// readNeedsYouEntry), or that magnum's own comment does not show to be one
// (optionalFinding: the entry is the fixer's word alone), holds as a Needs
// you entry does, since only a P3 whose Fix ends with the author's call is
// optional. The P3s are recorded as the PR's optional calls, merged by
// thread with those recorded before, and a fix.optional event names their
// links, never the fixer's words. read is the settle's one read of the
// PR's threads (threadsReader). It returns needs and the places (path:line)
// of the run's optional calls by thread (the first comment's id of each),
// which liftDecided counts against the calls of the PR's earlier attention.
func (e *Engine) settleOptional(ctx context.Context, job *fixJob, entries, needs []string, read func() ([]github.Thread, error)) ([]string, map[int64]string) {
	if len(entries) == 0 {
		return needs, nil
	}
	var found []optionalCall
	threads := map[int64]string{}
	for _, entry := range entries {
		n, ok := readNeedsYouEntry(entry)
		id, err := strconv.ParseInt(n.id, 10, 64)
		if !ok || err != nil || n.priority != "P3" || !strings.Contains(n.place, ":") || job.pr.URL == "" || !e.optionalFinding(ctx, job, n, id, read) {
			needs = append(needs, entry)
			continue
		}
		found, threads[id] = append(found, optionalCall{Thread: id, URL: job.pr.URL + "#discussion_r" + n.id, At: e.now().UTC()}), n.place
	}
	if len(found) == 0 {
		return needs, nil
	}
	calls := e.optionalCalls(ctx, job.pr.ID)
	for _, c := range found {
		if !slices.ContainsFunc(calls, func(o optionalCall) bool { return o.Thread == c.Thread }) {
			calls = append(calls, c)
		}
	}
	e.setOptionalCalls(ctx, job.pr.ID, calls)
	data := job.eventData()
	data["optional"] = len(found)
	e.event(ctx, "info", job.subject(), "fix.optional", fmt.Sprintf("%s left you %s, which %s nothing before the merge: %s",
		job.role.Name, optionalWords(found), textx.Plural(len(found), "holds", "hold"), strings.Join(optionalLinks(found), ", ")), data)
	return needs, threads
}

// authorsCallSentence ends the Fix of a finding of magnum's whose options
// are the author's to choose between (skills/magnum-review/SKILL.md).
const authorsCallSentence = "This is the author's call."

// optionalBody reports whether body, a comment of magnum's, is a P3 whose
// Fix ends with the author's call.
func optionalBody(body string) bool {
	return strings.HasPrefix(strings.TrimSpace(body), "**[P3]") && strings.Contains(body, authorsCallSentence)
}

// optionalFinding reports whether magnum's own comment shows n, an entry
// the fixer listed under Optional calls in thread id, to be a P3 whose Fix
// ends with the author's call: its comment in the review file beside the
// job's report (the review the fixer was handed, by path and line), else
// the first comment of that thread as GitHub shows it (read), started by
// one of magnum's logins (an earlier review's P3 kept open). The id is the
// fixer's word, which the review file, placing its comments by path and
// line alone, cannot check: for an id of another reviewer's thread (the
// external file), or one the threads file places elsewhere, the review file
// shows nothing, and GitHub's first comment of that thread decides. A read
// that fails, or a thread it does not list, shows nothing.
func (e *Engine) optionalFinding(ctx context.Context, job *fixJob, n needsYouEntry, id int64, read func() ([]github.Thread, error)) bool {
	owners := readThreadOwners(filepath.Dir(deref(job.run.ReportPath)))
	at, listed := owners.places[id]
	if i := strings.LastIndexByte(n.place, ':'); i > 0 && !owners.others[id] && (!listed || at == n.place) {
		path, line := n.place[:i], n.place[i+1:]
		if slices.ContainsFunc(owners.comments, func(c ownComment) bool { return c.Path == path && strconv.Itoa(c.Line) == line && optionalBody(c.Body) }) {
			return true
		}
	}
	threads, err := read()
	if err != nil {
		return false
	}
	own := e.fixLogins(ctx, job)
	return slices.ContainsFunc(threads, func(t github.Thread) bool {
		all := t.AllComments()
		if len(all) == 0 || all[0].ID != id {
			return false
		}
		root := github.Account(all[0].AuthorLogin, all[0].AuthorType)
		return slices.ContainsFunc(own, func(o string) bool { return github.SameAccount(o, root) }) && optionalBody(all[0].Body)
	})
}

// optionalWords counts calls: "1 optional call", "2 optional calls".
func optionalWords(calls []optionalCall) string {
	return textx.Count(len(calls), "optional call", "optional calls")
}

// optionalLinks are the calls' links.
func optionalLinks(calls []optionalCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.URL
	}
	return out
}

// openOptional are the PR's optional calls whose thread mine (magnum's
// unresolved threads, as the look's whole read lists them) shows open and
// waiting (optionalWaits), in the record's order, and their threads: the
// operator's calls still waiting. A call whose thread is resolved, or gone,
// is dropped from the record (a dry run writes nothing), and so is one of an
// earlier review's P3 (its thread in no review of sum's) that still waits
// when sum, magnum's review that stands at the head, keeps no earlier P3
// open: that review counts it fixed (the operator took it with a push) or
// answered. One answered since the call (a reply of the author's, which may
// be a decision on it, or the fixer's) stays recorded but waits no more.
func (e *Engine) openOptional(ctx context.Context, prID int64, mine []magnumThread, sum *store.ReviewSummary) ([]optionalCall, map[int64]bool) {
	calls := e.optionalCalls(ctx, prID)
	if len(calls) == 0 {
		return nil, nil
	}
	byRoot := make(map[int64]magnumThread, len(mine))
	for _, mt := range mine {
		byRoot[mt.RootID] = mt
	}
	taken := sum != nil && sum.OpenCounts != nil && sum.OpenCounts[3] == 0 // no earlier P3 open at the head
	var open []optionalCall
	threads := map[int64]bool{}
	kept := slices.DeleteFunc(slices.Clone(calls), func(c optionalCall) bool {
		mt, ok := byRoot[c.Thread]
		return !ok || taken && optionalWaits(c, mt) && (mt.ReviewID == 0 || mt.ReviewID != sum.ReviewID)
	})
	for _, c := range kept {
		if optionalWaits(c, byRoot[c.Thread]) {
			open, threads[c.Thread] = append(open, c), true
		}
	}
	if len(kept) != len(calls) && !e.d.DryRun {
		e.setOptionalCalls(ctx, prID, kept)
	}
	return open, threads
}

// optionalWaits reports whether call c still waits in mt, its unresolved
// thread: nobody answered it since the call (magnumThread.answeredSince),
// the rule decisionIn reads a decision by. A question of the author's that
// magnum answered before the call takes nothing.
func optionalWaits(c optionalCall, mt magnumThread) bool { return !mt.answeredSince(c.At) }

// openOptionalIn is how many of calls mine shows open and waiting
// (optionalWaits), as the look counts them (openOptional).
func openOptionalIn(calls []optionalCall, mine []magnumThread) int {
	n := 0
	for _, c := range calls {
		if i := slices.IndexFunc(mine, func(mt magnumThread) bool { return mt.RootID == c.Thread }); i >= 0 && optionalWaits(c, mine[i]) {
			n++
		}
	}
	return n
}

// openOptionalAt is how many of the job's PR's optional calls a loop's
// re-review leaves open: those that magnum's unresolved threads, as read
// shows them now (the settle's or the loop end's one read of the PR's
// threads, threadsReader), show waiting by the look's rule
// (openOptionalIn), or all of them when the read fails (a call counted that
// is answered already only ends the loop as done, and the look, which reads
// the threads, starts the next).
func (e *Engine) openOptionalAt(ctx context.Context, job *fixJob, read func() ([]github.Thread, error)) int {
	calls := e.optionalCalls(ctx, job.pr.ID)
	if len(calls) == 0 {
		return 0
	}
	threads, err := read()
	if err != nil {
		return len(calls)
	}
	author := github.Account(deref(job.pr.AuthorLogin), deref(job.pr.AuthorType))
	return openOptionalIn(calls, magnumThreadsOf(threads, e.fixLogins(ctx, job), author))
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
