package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/store"
)

// optionalThread is magnum's thread on PR #2 whose P3's Fix ends "This is
// the author's call.".
const optionalThread = 4302866759

// optionalEntry is the fixer's "## Optional calls" entry for it.
const optionalEntry = "4302866759 P3 app/models/offer.rb:42: option A keeps the cap, option B lifts it"

// withOptional is a fix report with an "## Optional calls" section of
// entries after its Needs you.
func withOptional(report string, entries ...string) string {
	body := "none"
	if len(entries) > 0 {
		body = "- " + strings.Join(entries, "\n- ")
	}
	return report + "## Optional calls\n" + body + "\n"
}

// A P3 whose Fix ends "This is the author's call." held an own PR for the
// operator (2026-11-07: an optional P3 kept it in Needs you for a day, and
// R47's CI start waited behind it). The fixer now leaves it as it is, with
// no reply, under "## Optional calls": the settle raises no attention for
// it, the loop ends without the operator, and the PR keeps the call as the
// operator's (its thread and link) for the look and the merge's words, with
// a fix.optional event. A P0 to P2 there, or an entry magnum cannot read,
// still holds as Needs you does: only a P3 is optional.
func TestAnOptionalCallHoldsNothing(t *testing.T) {
	t.Run("a P3", func(t *testing.T) {
		h, git := fixHarness(t)
		pr := fixablePR(h, "zhuravel", map[string]int{"P3": 1})
		fixer(h, git, "", func(run store.Run) string {
			writeFixReviewFiles(h.t, deref(run.ReportPath), []int64{optionalThread}, nil)
			return withOptional(fixReport(run.ID, "nothing", noReplies, "none"), optionalEntry)
		})
		h.fix(FixPayload{})
		h.handle()
		if cur := h.wantState(2, store.PRReviewed); cur.LastError != nil {
			t.Errorf("last_error = %q, want none", deref(cur.LastError))
		}
		if _, ok := h.fixStateOf(2); ok {
			t.Error("the loop goes on")
		}
		if ev, ok := h.fixEvent("fix.needs_you"); ok {
			t.Errorf("fix.needs_you = %q, want none", ev.Message)
		}
		link := prPage + "#discussion_r4302866759"
		if ev, ok := h.fixEvent("fix.optional"); !ok || !strings.Contains(ev.Message, "1 optional call") || !strings.Contains(ev.Message, link) ||
			strings.Contains(ev.Message, "option A") {
			t.Errorf("fix.optional = %q, %v; want the call's link, never the fixer's words", ev.Message, ok)
		}
		if calls := h.e.optionalCalls(h.ctx, pr.ID); !slices.Equal(calls, []optionalCall{{Thread: optionalThread, URL: link}}) {
			t.Errorf("the PR's optional calls = %+v", calls)
		}
		h.flush()
		h.wantToasts("needs attention", 0)
	})
	t.Run("a P2 holds", func(t *testing.T) {
		h, git := fixHarness(t)
		pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
		fixer(h, git, "", func(run store.Run) string {
			writeFixReviewFiles(h.t, deref(run.ReportPath), []int64{optionalThread}, nil)
			return withOptional(fixReport(run.ID, "nothing", noReplies, "none"), strings.Replace(optionalEntry, " P3 ", " P2 ", 1), "my file.rb: spaces")
		})
		h.fix(FixPayload{})
		h.handle()
		cur := h.wantState(2, store.PRNeedsAttention)
		if msg := deref(cur.LastError); !strings.HasPrefix(msg, "fix: 2 findings need your call: ") {
			t.Errorf("last_error = %q, want the P2 and the unread entry to need you", msg)
		}
		if calls := h.e.optionalCalls(h.ctx, pr.ID); len(calls) != 0 {
			t.Errorf("the PR's optional calls = %+v, want none", calls)
		}
	})
}
