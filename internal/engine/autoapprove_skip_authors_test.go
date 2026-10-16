package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// skipAuthorReason is why auto-approval refuses a PR whose author the
// watch's auto_approve_skip_authors lists (the card's and the JSON's words).
const skipAuthorReason = "the author is in auto_approve_skip_authors"

// skipAlice has the watch auto-approve every repository it covers but no PR
// alice authored (the fake GitHub's default author).
func skipAlice(h *harness) {
	h.cfg.Watches[0].AutoApprove = []string{"*"}
	h.cfg.Watches[0].AutoApproveSkipAuthors = []string{"alice"}
}

// authorRefusals lists the review.auto_approve_refused events of PR #n that
// name the listed author's refusal.
func (h *harness) authorRefusals(n int) []string {
	h.t.Helper()
	var out []string
	for _, ev := range h.events("review.auto_approve_refused") {
		if strings.Contains(ev.Message, "talkable/talkable#"+itoa(int64(n))+" ") && strings.Contains(ev.Message, skipAuthorReason) {
			out = append(out, ev.Message)
		}
	}
	return out
}

// A clean review of a PR whose author auto_approve_skip_authors lists posts
// no approval as the operator: the refusal is recorded for the card and in
// one review.auto_approve_refused event per head, and a second clean round
// of the same head records nothing new. Another author's PR on the same
// watch is approved as before.
func TestAListedAuthorsPRIsNeverAutoApproved(t *testing.T) {
	h, _ := newAutoHarness(t, skipAlice)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"}, prSpec{n: 3, head: "c1", author: "bob-rev"})
	h.tick()
	h.reviewTo(2, "b1")
	h.reviewTo(3, "c1")
	h.flush()
	h.flush()
	if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@c1:") {
		t.Fatalf("posted = %q, want bob-rev's PR approved and alice's not", got)
	}
	if got := h.autoRefused(2); got != skipAuthorReason {
		t.Fatalf("alice's PR held back for %q, want %q", got, skipAuthorReason)
	}
	want := "magnum does not approve talkable/talkable#2 as you: " + skipAuthorReason
	if evs := h.authorRefusals(2); len(evs) != 1 || evs[0] != want {
		t.Fatalf("refusal events = %q, want one %q", evs, want)
	}
	if got := h.autoRefused(3); got != "" {
		t.Fatalf("bob-rev's PR held back for %q", got)
	}
	before := mustKV(t, h, KVPRAutoApproveRefused(h.pr(2).ID))

	rounds := len(h.rd.all())
	h.advance(31 * time.Minute) // #3 holds the one slot: past min_warm it can be evicted for #2
	h.reviewAgain(2)
	h.flush()
	h.flush()
	if n := len(h.rd.all()); n != rounds+1 {
		t.Fatalf("rounds = %d, want a second round of b1", n-rounds)
	}
	if got := h.created(); len(got) != 1 {
		t.Fatalf("posted after the second round = %q", got)
	}
	if evs := h.authorRefusals(2); len(evs) != 1 {
		t.Fatalf("refusal events after the second round of the same head = %q, want the first alone", evs)
	}
	if got := mustKV(t, h, KVPRAutoApproveRefused(h.pr(2).ID)); got != before {
		t.Fatalf("the second round rewrote the refusal: %s, was %s", got, before)
	}
	for _, c := range h.created() {
		if strings.Contains(c, "auto_approve_skip_authors") {
			t.Fatalf("a review on GitHub names the exclusion: %q", c)
		}
	}
}

// The list matches the PR's author in any case: Alice in the config is the
// author alice.
func TestAListedAuthorMatchesAnyCase(t *testing.T) {
	h, _ := newAutoHarness(t, func(h *harness) { h.cfg.Watches[0].AutoApproveSkipAuthors = []string{"Alice"} })
	h.reviewedPR(2, "b1")
	h.flush()
	h.wantRefused(2, skipAuthorReason)
}

// An approval magnum posted as the operator before the author was listed
// stands: the gates a later clean round is held to (standingGateRefusal) do
// not ask about the author, so neither a clean round of the same head nor
// one of a new head withdraws it.
func TestAStandingApprovalOfAListedAuthorStays(t *testing.T) {
	h, _ := newAutoHarness(t)
	h.reviewedPR(2, "b1")
	h.tick()
	if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:") {
		t.Fatalf("posted = %q, want the approval of b1", got)
	}
	h.cfg.Watches[0].AutoApproveSkipAuthors = []string{"alice"}

	h.reviewAgain(2)
	h.flush()
	h.flush()
	h.push(2, "b2")
	h.reviewTo(2, "b2")
	h.flush()
	h.flush()
	if d := h.ghCalls("dismiss:"); len(d) != 0 {
		t.Fatalf("dismissals = %q, want the approval left standing", d)
	}
	if a := h.autoApproval(2); a.State != store.AutoStanding || a.HeadSHA != "b1" {
		t.Fatalf("approval = %+v, want the one of b1 standing", a)
	}
	if got := h.created(); len(got) != 1 {
		t.Fatalf("posted = %q, want the first approval alone", got)
	}
}

// The board's A (magnum approve --as the watch's auto_approve_as) still
// approves a listed author's PR as the operator: the list keeps magnum from
// deciding alone, and A is the operator's decision. A later clean round of
// the head leaves that approval standing.
func TestTheOperatorsOwnApproveKeyStillApprovesAListedAuthor(t *testing.T) {
	h, _ := newAutoHarness(t, skipAlice)
	h.reviewedPR(2, "b1")
	h.flush()
	h.wantRefused(2, skipAuthorReason)

	if r := h.verdictAs(ReqApprove, 2, "zhuravel"); r.State != store.RequestDone {
		t.Fatalf("A as the operator: %s %q", r.State, deref(r.Result))
	}
	if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:") {
		t.Fatalf("posted = %q, want the operator's approval of b1", got)
	}
	h.reviewAgain(2)
	h.flush()
	h.flush()
	if d := h.ghCalls("dismiss:"); len(d) != 0 {
		t.Fatalf("dismissals = %q, want the operator's approval left standing", d)
	}
	if a := h.autoApproval(2); a.State != store.AutoStanding {
		t.Fatalf("approval = %+v", a)
	}
}

// The review status comment on GitHub names neither the author nor the
// exclusion: on a listed author's PR it reads as it does on a watch without
// auto-approval.
func TestTheStatusCommentNamesNoListedAuthor(t *testing.T) {
	comments := func(mods ...func(*harness)) []string {
		h, _ := newAutoHarness(t, append([]func(*harness){statusOn}, mods...)...)
		h.reviewedPR(2, "b1")
		h.flush()
		h.flush()
		var out []string
		for _, c := range h.gh.commentsOn(2) {
			out = append(out, c.body)
		}
		return out
	}
	plain := comments(func(h *harness) { h.cfg.Watches[0].AutoApprove = nil })
	listed := comments(skipAlice)
	if len(listed) == 0 || !slices.Equal(listed, plain) {
		t.Fatalf("status comments of a listed author's PR:\n%q\nwant those of a watch without auto-approval:\n%q", listed, plain)
	}
	for _, b := range listed {
		if strings.Contains(b, "alice") || strings.Contains(b, "auto_approve") {
			t.Fatalf("the status comment names the author or the exclusion: %q", b)
		}
	}
}
