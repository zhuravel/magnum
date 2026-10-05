package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// Muting a PR GitHub no longer lists as open dismisses its merged-unreviewed
// flag: the flag needs a muted PR to be forced, and the forced mark a request
// left on a PR that merged before its round ran outlives the close.

// staleForced marks PR #n forced, the way a review request left it before the
// PR merged, and muted as asked.
func (h *harness) staleForced(n int, muted bool) store.PR {
	h.t.Helper()
	pr := h.pr(n)
	if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) {
		u.Set("forced", true)
		u.Set("muted", muted)
	}); err != nil {
		h.t.Fatal(err)
	}
	return h.pr(n)
}

// muteRequest runs a mute or unmute request for PR #n through the daemon and
// returns the answer.
func (h *harness) muteRequest(kind string, n int) string {
	h.t.Helper()
	id := h.enqueue(kind, TargetPayload{PRTarget: PRTarget{Ref: itoa(int64(n))}})
	h.tick()
	r := h.request(id)
	if r.State != store.RequestDone {
		h.t.Fatalf("%s #%d: %s %q", kind, n, r.State, deref(r.Result))
	}
	return deref(r.Result)
}

// boardRow is PR #n's row on the board, closed PRs included.
func (h *harness) boardRow(n int) store.BoardRow {
	h.t.Helper()
	rows, err := h.st.Board(h.ctx, store.BoardFilter{IncludeClosed: true})
	if err != nil {
		h.t.Fatal(err)
	}
	for _, r := range rows {
		if r.Number == n {
			return r
		}
	}
	h.t.Fatalf("PR #%d is not on the board", n)
	return store.BoardRow{}
}

// A mute request on a merged PR that still carries the forced mark of a
// request made before the merge clears the mark, so the PR is no longer
// merged unreviewed (its row on the board says so), whether it is closed or
// already released and whether it was muted before or not. An unmute brings
// the flag back and does not restore the mark.
func TestMuteOfAMergedPRDismissesItsMergedUnreviewedFlag(t *testing.T) {
	for _, tc := range []struct {
		name     string
		released bool
		muted    bool // muted (and forced) before the merge: flagged all the same
	}{
		{"closed", false, false},
		{"released", true, false},
		{"muted and requested before the merge", false, true},
		{"released, muted and requested before the merge", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.mergedBeforeReReview()
			if tc.released {
				h.advance(10*time.Minute + time.Second)
				h.tick()
				h.wantState(2, store.PRReleased)
			}
			before := h.staleForced(2, tc.muted)
			if !before.MergedUnreviewed() || !h.boardRow(2).MergedUnreviewed {
				t.Fatalf("setup: the PR is not flagged: %+v", before)
			}

			if got := h.muteRequest(ReqMute, 2); !strings.HasPrefix(got, "muted talkable/talkable#2") || !strings.Contains(got, "merged-unreviewed flag dismissed") {
				t.Errorf("mute answered %q", got)
			}
			cur := h.pr(2)
			if !cur.Muted || cur.Forced || cur.MergedUnreviewed() || !cur.FlagDismissed() {
				t.Fatalf("after the mute: muted %v forced %v flagged %v dismissed %v", cur.Muted, cur.Forced, cur.MergedUnreviewed(), cur.FlagDismissed())
			}
			if row := h.boardRow(2); row.MergedUnreviewed || !row.FlagDismissed || !row.Muted {
				t.Errorf("board row after the mute: flagged %v dismissed %v muted %v", row.MergedUnreviewed, row.FlagDismissed, row.Muted)
			}
			if cur.State != before.State {
				t.Errorf("the mute moved the PR from %s to %s", before.State, cur.State)
			}

			if got := h.muteRequest(ReqUnmute, 2); got != "unmuted talkable/talkable#2" {
				t.Errorf("unmute answered %q", got)
			}
			cur = h.pr(2)
			if cur.Muted || cur.Forced || !cur.MergedUnreviewed() || cur.FlagDismissed() {
				t.Fatalf("after the unmute: muted %v forced %v flagged %v dismissed %v (the forced mark must not come back)", cur.Muted, cur.Forced, cur.MergedUnreviewed(), cur.FlagDismissed())
			}
			if row := h.boardRow(2); !row.MergedUnreviewed || row.FlagDismissed {
				t.Errorf("board row after the unmute: flagged %v dismissed %v", row.MergedUnreviewed, row.FlagDismissed)
			}
		})
	}
}

// A PR GitHub closed without merging has no flag to dismiss, but its forced
// mark is as stale: a mute clears it, so a later reopen finds a muted PR, not
// a forced one that runs at once.
func TestMuteOfAClosedPRClearsItsStaleForcedMark(t *testing.T) {
	h := newHarness(t)
	h.reviewedThenPushed()
	h.advance(time.Minute)
	h.closeOnGitHub(2, "CLOSED", prSpec{n: 1, head: "base1"})
	h.staleForced(2, false)

	h.muteRequest(ReqMute, 2)
	if cur := h.pr(2); !cur.Muted || cur.Forced {
		t.Fatalf("after the mute: muted %v forced %v", cur.Muted, cur.Forced)
	}
}

// A post-merge round that is due or running needs the forced mark (the
// dispatcher takes a merged PR only when it is forced, and a muted one only
// then): the mute keeps it in every state a round is in.
func TestMuteOfAMergedPRWithAPostMergeRoundKeepsItForced(t *testing.T) {
	for _, state := range []string{store.PRQueued, store.PRRereviewPending, store.PRClaiming, store.PRReviewing, store.PRVerifying, store.PRPaused} {
		t.Run(state, func(t *testing.T) {
			h := newHarness(t)
			pr := h.mergedBeforeReReview()
			h.staleForced(2, false)
			if err := h.st.TransitionPR(h.ctx, pr.ID, []string{store.PRClosed}, state, nil); err != nil {
				t.Fatal(err)
			}

			got, err := h.e.requestMute(h.ctx, TargetPayload{PRTarget: PRTarget{Ref: "2"}}, true)
			if err != nil {
				t.Fatal(err)
			}
			if got != "muted talkable/talkable#2" {
				t.Errorf("answer %q: the flag was not dismissed, the answer must not say so", got)
			}
			cur := h.wantState(2, state)
			if !cur.Muted || !cur.Forced {
				t.Fatalf("muted %v forced %v, want both: the round is due", cur.Muted, cur.Forced)
			}
		})
	}
}

// A PR that is open on GitHub is muted as before: its forced mark is a
// request to review it that the mute leaves to the dispatcher's own rule.
func TestMuteOfAnOpenPRKeepsItsForcedMark(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.staleForced(2, false)

	if got := h.muteRequest(ReqMute, 2); got != "muted talkable/talkable#2" {
		t.Errorf("answer %q", got)
	}
	cur := h.pr(2)
	if !cur.Muted || !cur.Forced {
		t.Fatalf("muted %v forced %v, want both", cur.Muted, cur.Forced)
	}
	if cur.FlagDismissed() || h.boardRow(2).FlagDismissed {
		t.Error("an open PR has no flag to dismiss")
	}
}

// The flag of a PR muted and forced while it was open stays: closing does not
// clear the mark (a muted PR the user asked a review of is flagged when it
// merges first), and only a mute after the merge dismisses it.
func TestCloseKeepsTheForcedMarkOfAMutedPR(t *testing.T) {
	h := newHarness(t)
	h.reviewedThenPushed()
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"})
	h.gh.closed[2] = "MERGED"
	h.tick() // first confirmation
	h.advance(30 * time.Second)
	h.tick() // second, but not a minute after the first
	h.advance(30 * time.Second)
	h.staleForced(2, true) // asked for before the PR closed, and muted; the round would run at once
	h.tick()
	cur := h.wantState(2, store.PRClosed)
	if !cur.Muted || !cur.Forced || !cur.MergedUnreviewed() {
		t.Fatalf("after the close: muted %v forced %v flagged %v", cur.Muted, cur.Forced, cur.MergedUnreviewed())
	}

	if got := h.muteRequest(ReqMute, 2); !strings.Contains(got, "flag dismissed") {
		t.Errorf("mute answered %q", got)
	}
	if cur = h.pr(2); cur.Forced || cur.MergedUnreviewed() || !cur.FlagDismissed() {
		t.Fatalf("after the mute: forced %v flagged %v dismissed %v", cur.Forced, cur.MergedUnreviewed(), cur.FlagDismissed())
	}
}
