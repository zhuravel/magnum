package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// appPolls makes the talkable watch poll as the App it posts as, the
// operator's planned switch of 2026-10-22, and leaves it the only watch, as
// every watch of his posts as an App: no posting identity is the gh one.
// The gh identity zhuravel stays declared, and the fake answers both
// identities' calls.
func appPolls(h *harness) {
	h.cfg.Watches = h.cfg.Watches[:1]
	h.cfg.Watches[0].PollIdentity = "talkable-app"
	h.d.GitHub = func(id string) GitHub {
		if id == "zhuravel" || id == "talkable-app" {
			return h.gh
		}
		return nil
	}
}

// Under an App poll identity a review request for the operator's own login
// (the gh identity) still starts a requested round: before
// config.OperatorIdentity requestLogins took the App's login in its place,
// and 61 requests in 14 days to the login would have started nothing.
func TestUnderAnAppPollerARequestForTheOperatorsLoginStartsARound(t *testing.T) {
	h := newHarness(t, appPolls)
	h.reviewedPR(2, "b1")
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{reqAsk(h, "alice", askedPoll)}})
	if evs := reqEvents(t, h); len(evs) != 1 || !strings.Contains(evs[0].Message, "review requested from zhuravel by alice") {
		t.Fatalf("pr.review_requested events: %+v", evs)
	}
	h.advance(time.Minute) // the debounce passed, the 30m interval did not
	h.tick()
	reqWantRounds(t, h, 2)

	// The App's own login still counts, as it did when it was the poll login.
	h2 := newHarness(t, appPolls)
	h2.reviewedPR(2, "b1")
	h2.advance(time.Minute)
	reqPoll(h2, prSpec{n: 2, head: "b1", requests: []github.ReviewRequestEvent{reqAsk(h2, "alice", askedBot)}})
	h2.advance(time.Minute)
	h2.tick()
	reqWantRounds(t, h2, 2)
}

// Under an App poll identity a pending review request for the operator's
// login sets the PR's requested-reviewer flag, which dispatch priority and
// the hold of `magnum pause` read.
func TestUnderAnAppPollerARequestForTheOperatorsLoginSetsTheRequestedFlag(t *testing.T) {
	h := newHarness(t, appPolls)
	h.open(prSpec{n: 1, head: "a1"})
	h.startup()
	h.tick()
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "a1"}, prSpec{n: 2, head: "b1", request: true}, prSpec{n: 3, head: "c1"})
	h.tick()
	if pr := h.wantState(2, store.PRQueued); !pr.ReviewRequested {
		t.Fatal("review_requested not recorded for a request to the operator's login")
	}
	if pr := h.pr(3); pr.ReviewRequested {
		t.Fatal("review_requested recorded for a PR nobody asked the operator to review")
	}
}

// Under an App poll identity the panes' MAGNUM_SELF_LOGIN is the
// operator's login, never the App's; under a gh poll identity it is the
// poll login, as before.
func TestThePanesSelfLoginIsTheOperatorsLogin(t *testing.T) {
	for _, tc := range []struct {
		name string
		mods []func(*harness)
	}{
		{"a gh poll identity", nil},
		{"an App poll identity", []func(*harness){appPolls}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.mods...)
			job := &roundJob{pr: store.PR{Number: 2, URL: "https://github.com/talkable/talkable/pull/2"},
				repo: store.Repo{Owner: "talkable", Name: "talkable"}, watch: h.cfg.Watches[0]}
			env, err := h.e.paneEnv(h.ctx, job, h.app, "b1")
			if err != nil {
				t.Fatal(err)
			}
			if got := env["MAGNUM_SELF_LOGIN"]; got != "zhuravel" {
				t.Fatalf("MAGNUM_SELF_LOGIN = %q, want zhuravel", got)
			}
			if got := env["MAGNUM_REVIEWER_LOGIN"]; got != "talkable[bot]" {
				t.Fatalf("MAGNUM_REVIEWER_LOGIN = %q, want the posting App's", got)
			}
		})
	}
}

// With a gh poll identity the logins a request may name are the ones they
// always were: the poll login, then the posting identities'.
func TestAGhPollersRequestLoginsAreUnchanged(t *testing.T) {
	h := newHarness(t)
	for i, want := range [][]string{{"zhuravel", "talkable[bot]"}, {"zhuravel", "zhuravel"}} {
		w := h.cfg.Watches[i]
		if got := h.e.requestLogins(w, w.Identity); !slices.Equal(got, []string{"zhuravel", "talkable[bot]"}) {
			t.Errorf("watch %s: requestLogins = %v", w.Owner, got)
		}
		if got := h.e.watchLogins(w); !slices.Equal(got, want) {
			t.Errorf("watch %s: watchLogins = %v, want %v", w.Owner, got, want)
		}
		if got := h.e.selfLogin(w); got != "zhuravel" {
			t.Errorf("watch %s: selfLogin = %q", w.Owner, got)
		}
	}
}
