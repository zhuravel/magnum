package engine

// The optional calls a fixer leaves the author. A P3 of magnum's whose Fix
// ends "This is the author's call." asks nothing before the merge: its
// title says "(optional, no reply needed)", and P3s never block. The fix
// skill leaves the code as it is, replies nowhere and lists it under the
// report's "## Optional calls" (skills/magnum-fix/SKILL.md), so it holds no
// PR: the settle raises no attention for it (settleOptional) and keeps it as
// the operator's call, by its thread (kvPROptionalCalls). The look of
// autofix = "open" reads that record: such a P3 is no fixer's work any
// more, so no loop starts on it again (reviewWork, magnumItemsOf), and the
// idle verdict names it ("1 optional call: <link>") while its thread stays
// open and unanswered, so the operator sees it at the merge. A P0 to P2
// author's call still holds as a Needs you entry does.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/zhuravel/magnum/internal/textx"
)

// optionalCall is a P3 of magnum's that a fixer left the author as an
// optional call: the id of its thread's first comment, and the thread's
// link on the PR's page.
type optionalCall struct {
	Thread int64  `json:"thread"`
	URL    string `json:"url"`
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
// readNeedsYouEntry) holds as a Needs you entry does, since only a P3 is
// optional. The P3s are recorded as the PR's optional calls, merged by
// thread with those recorded before, and a fix.optional event names their
// links, never the fixer's words.
func (e *Engine) settleOptional(ctx context.Context, job *fixJob, entries, needs []string) []string {
	if len(entries) == 0 {
		return needs
	}
	var found []optionalCall
	for _, entry := range entries {
		n, ok := readNeedsYouEntry(entry)
		id, err := strconv.ParseInt(n.id, 10, 64)
		if !ok || err != nil || n.priority != "P3" || !strings.Contains(n.place, ":") || job.pr.URL == "" {
			needs = append(needs, entry)
			continue
		}
		found = append(found, optionalCall{Thread: id, URL: job.pr.URL + "#discussion_r" + n.id})
	}
	if len(found) == 0 {
		return needs
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
	return needs
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
