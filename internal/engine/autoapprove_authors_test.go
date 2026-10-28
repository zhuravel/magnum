package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// notAllowedReason is why auto-approval refuses a PR whose author the
// auto_approve_authors that applies does not list (the card's and the
// JSON's words).
const notAllowedReason = "the author is not in auto_approve_authors"

// allowBobRev has the watch auto-approve every repository it covers, but
// only the PRs bob-rev authored: not alice's (the fake GitHub's default
// author).
func allowBobRev(h *harness) {
	h.cfg.Watches[0].AutoApprove = []string{"*"}
	h.cfg.Watches[0].AutoApproveAuthors = []string{"bob-rev"}
}

// A clean review of a PR whose author auto_approve_authors does not list
// posts no approval as the operator: the refusal is recorded for the card
// and in one review.auto_approve_refused event per head, a second clean
// round of the same head records nothing new, and nothing on GitHub names
// the list. A listed author's PR on the same watch is approved.
func TestAnAuthorOutsideTheAllowlistIsNeverAutoApproved(t *testing.T) {
	h, _ := newAutoHarness(t, allowBobRev)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"}, prSpec{n: 3, head: "c1", author: "Bob-Rev"})
	h.tick()
	h.reviewTo(2, "b1")
	h.reviewTo(3, "c1")
	h.flush()
	h.flush()
	if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@c1:") {
		t.Fatalf("posted = %q, want bob-rev's PR approved and alice's not", got)
	}
	if got := h.autoRefused(2); got != notAllowedReason {
		t.Fatalf("alice's PR held back for %q, want %q", got, notAllowedReason)
	}
	want := "magnum does not approve talkable/talkable#2 as you: " + notAllowedReason
	refusals := func() []string {
		var out []string
		for _, ev := range h.events("review.auto_approve_refused") {
			if strings.Contains(ev.Message, "talkable/talkable#2 ") {
				out = append(out, ev.Message)
			}
		}
		return out
	}
	if evs := refusals(); len(evs) != 1 || evs[0] != want {
		t.Fatalf("refusal events = %q, want one %q", evs, want)
	}
	if got := h.autoRefused(3); got != "" {
		t.Fatalf("bob-rev's PR held back for %q", got)
	}

	h.advance(31 * time.Minute) // #3 holds the one slot: past min_warm it can be evicted for #2
	rounds := len(h.rd.all())
	h.reviewAgain(2)
	h.flush()
	h.flush()
	if n := len(h.rd.all()); n != rounds+1 {
		t.Fatalf("rounds = %d, want a second round of b1", n-rounds)
	}
	if got := h.created(); len(got) != 1 {
		t.Fatalf("posted after the second round = %q", got)
	}
	if evs := refusals(); len(evs) != 1 {
		t.Fatalf("refusal events after the second round of the same head = %q, want the first alone", evs)
	}
	for _, c := range h.created() {
		if strings.Contains(c, "auto_approve") || strings.Contains(c, "alice") {
			t.Fatalf("a review on GitHub names the list or the author: %q", c)
		}
	}
}

// A PR whose author magnum does not know (GitHub names none: a deleted
// account) is not on any allowlist: authorRefusal refuses it, while
// without an allowlist it holds back no author.
func TestAnUnknownAuthorIsNotOnTheAllowlist(t *testing.T) {
	h, _ := newAutoHarness(t, allowBobRev)
	unknown := store.RepoPR{Repo: "talkable/talkable", PR: store.PR{Number: 2}}
	if got := h.e.authorRefusal(unknown); got != (autoRefusal{why: notAllowedReason}) {
		t.Fatalf("an unknown author with an allowlist: %+v, want %q of no class", got, notAllowedReason)
	}
	h.cfg.Watches[0].AutoApproveAuthors = nil
	if got := h.e.authorRefusal(unknown); got != (autoRefusal{}) {
		t.Fatalf("an unknown author without an allowlist: %+v", got)
	}
}

// The allowlist matches the PR's author in any case and with or without
// "[bot]": Alice[bot] in the config is the author alice.
func TestTheAllowlistMatchesAnyCase(t *testing.T) {
	h, _ := newAutoHarness(t, func(h *harness) {
		h.cfg.Watches[0].AutoApproveAuthors = []string{"Alice[bot]"}
	})
	h.reviewedPR(2, "b1")
	h.flush()
	if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:") {
		t.Fatalf("posted = %q, want alice's PR approved", got)
	}
	if got := h.autoRefused(2); got != "" {
		t.Fatalf("held back for %q", got)
	}
}

// The repository's auto_approve_authors replaces the watch's for its PRs:
// alice, on the repository's list but not the watch's, is approved there,
// and a repository list without her refuses her although the watch lists
// her.
func TestARepositorysAllowlistReplacesTheWatchs(t *testing.T) {
	h, _ := newAutoHarness(t, allowBobRev, func(h *harness) {
		h.cfg.Repos = append(h.cfg.Repos, config.Repo{Repo: "talkable/talkable", AutoApproveAuthors: []string{"alice"}})
	})
	h.reviewedPR(2, "b1")
	h.flush()
	if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:") {
		t.Fatalf("posted = %q, want alice's PR approved by the repository's list", got)
	}

	h, _ = newAutoHarness(t, func(h *harness) {
		h.cfg.Watches[0].AutoApproveAuthors = []string{"alice"}
		h.cfg.Repos = append(h.cfg.Repos, config.Repo{Repo: "talkable/talkable", AutoApproveAuthors: []string{"bob-rev"}})
	})
	h.reviewedPR(2, "b1")
	h.flush()
	h.wantRefused(2, notAllowedReason)
}

// A login on both auto_approve_authors and auto_approve_skip_authors is
// skipped: the skip list's refusal is the one recorded.
func TestTheSkipListWinsOverTheAllowlist(t *testing.T) {
	h, _ := newAutoHarness(t, func(h *harness) {
		h.cfg.Watches[0].AutoApproveAuthors = []string{"alice"}
		h.cfg.Watches[0].AutoApproveSkipAuthors = []string{"alice"}
	})
	h.reviewedPR(2, "b1")
	h.flush()
	h.wantRefused(2, skipAuthorReason)
	if got := h.autoRefused(2); got != skipAuthorReason {
		t.Fatalf("held back for %q, want the skip list's %q alone", got, skipAuthorReason)
	}
}

// An approval magnum posted as the operator before the allowlist left its
// author out stands: the gates a later clean round is held to
// (standingGateRefusal) do not ask about the author, so neither a clean
// round of the same head nor one of a new head withdraws it.
func TestAStandingApprovalStaysWhenTheAllowlistLeavesItsAuthorOut(t *testing.T) {
	h, _ := newAutoHarness(t)
	h.reviewedPR(2, "b1")
	h.tick()
	if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:") {
		t.Fatalf("posted = %q, want the approval of b1", got)
	}
	h.cfg.Watches[0].AutoApproveAuthors = []string{"bob-rev"}

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
// approves the PR of an author outside the allowlist as the operator: the
// list keeps magnum from deciding alone, and A is the operator's decision.
func TestTheOperatorsOwnApproveKeyStillApprovesAnAuthorOutsideTheAllowlist(t *testing.T) {
	h, _ := newAutoHarness(t, allowBobRev)
	h.reviewedPR(2, "b1")
	h.flush()
	h.wantRefused(2, notAllowedReason)

	if r := h.verdictAs(ReqApprove, 2, "zhuravel"); r.State != store.RequestDone {
		t.Fatalf("A as the operator: %s %q", r.State, deref(r.Result))
	}
	if got := h.created(); len(got) != 1 || !strings.HasPrefix(got[0], "APPROVE@b1:") {
		t.Fatalf("posted = %q, want the operator's approval of b1", got)
	}
}

// The review status comment on GitHub names neither the author nor the
// allowlist: on a PR it refuses it reads as it does on a watch without
// auto-approval.
func TestTheStatusCommentNamesNoAllowlist(t *testing.T) {
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
	refused := comments(allowBobRev)
	if len(refused) == 0 || !slices.Equal(refused, plain) {
		t.Fatalf("status comments of a PR the allowlist refuses:\n%q\nwant those of a watch without auto-approval:\n%q", refused, plain)
	}
	for _, b := range refused {
		if strings.Contains(b, "auto_approve") {
			t.Fatalf("the status comment names the allowlist: %q", b)
		}
	}
}
