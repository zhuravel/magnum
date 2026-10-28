package engine

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// The live case (talkable#12106, 2026-10-28): an empty commit was queued as a
// 0-line small delta by a build that had no class empty; the build that
// replaced it knew the class, but a queued re-review was never measured
// again, so it ran after its two-hour wait. Right before a round that waited
// for a small delta starts, dispatch measures the push once more
// (settleSmallDelta, delta.go): a push that is trivial now settles as one
// the poller sees (the review stands, no round).

// recheckHarness is newHarness whose GitHub fake notes the op of each read,
// so a test tells the dispatch's second look (opPushDelta) from the reads
// the round's own setup makes.
func recheckHarness(t *testing.T, mods ...func(*harness)) (*harness, *opsGH) {
	t.Helper()
	var gh *opsGH
	h := newHarness(t, append([]func(*harness){func(h *harness) {
		gh = &opsGH{fakeGH: h.gh}
		h.d.GitHub = func(id string) GitHub {
			if id == "zhuravel" {
				return gh
			}
			return nil
		}
	}}, mods...)...)
	return h, gh
}

// looks counts the push comparisons read as the poll's trivial-delta check
// (opPushDelta) so far: the poller's look at a new head and dispatch's second
// look, which a test tells apart by counting from the setup's.
func (g *opsGH) looks() int {
	n := 0
	for _, s := range g.seen() {
		if s == "ComparePush:"+opPushDelta {
			n++
		}
	}
	return n
}

// queuedEmptyPush is PR #2 reviewed at b1 and pushed b2, an empty commit
// (GitHub: ahead, one commit, no file) that an older build, whose
// skip_trivial_deltas had no class empty, queued as a 0-line small delta.
// The running build knows the class (the setting is back to the default
// when it returns).
func queuedEmptyPush(t *testing.T, h *harness) store.PR {
	t.Helper()
	full := slices.Clone(h.cfg.Daemon.SkipTrivialDeltas)
	if !slices.Contains(full, DeltaEmpty) {
		t.Fatalf("the default skip_trivial_deltas %v lacks the class empty", full)
	}
	h.cfg.Daemon.SkipTrivialDeltas = slices.DeleteFunc(slices.Clone(full), func(c string) bool { return c == DeltaEmpty })
	pr := pushedEmpty(t, h, "ahead", 1)
	h.cfg.Daemon.SkipTrivialDeltas = full
	if pr.State != store.PRRereviewPending || deref(pr.ReviewedSHA) != "b1" {
		t.Fatalf("setup: state %s reviewed_sha %q, want the queued re-review of b2", pr.State, deref(pr.ReviewedSHA))
	}
	if w := waitOf(t, h, 2); w.Reason != WaitDelta {
		t.Fatalf("setup: wait = %+v, want the small delta's hold", w)
	}
	return pr
}

// The queued re-review of an empty push settles when its wait ends: the
// review stands for the head, as when the poller sees the push, and no round
// starts. The second look is one comparison, and a head it settled is not
// measured again.
func TestAQueuedSmallDeltaThatIsAnEmptyPushSettlesAtDispatch(t *testing.T) {
	h, gh := recheckHarness(t)
	pr := queuedEmptyPush(t, h)
	roundsToday, polled := pr.RoundsToday, gh.looks()
	h.advance(2 * time.Hour) // the small delta's wait ends
	h.tick()

	pr = h.pr(2)
	if pr.State != store.PRReviewed || deref(pr.ReviewedSHA) != "b2" || deref(pr.LastReviewID) != 101 {
		t.Fatalf("after the wait: state %s reviewed_sha %q review %v, want the review settled on b2", pr.State, deref(pr.ReviewedSHA), deref(pr.LastReviewID))
	}
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d, want only the first: the push is empty", n)
	}
	if pr.RoundsToday != roundsToday || pr.NextEligibleAt != nil || pr.PendingSince != nil {
		t.Fatalf("rounds_today %d (was %d), next_eligible_at %v, pending_since %v, want the PR settled with nothing queued",
			pr.RoundsToday, roundsToday, pr.NextEligibleAt, pr.PendingSince)
	}
	evs := trivialEvents(t, h)
	want := "rereview_pending → reviewed: the push to b2 changes no file (1 commit) since the review of b1; no re-review, the review stands"
	if len(evs) != 1 || evs[0].Message != want {
		t.Fatalf("pr.trivial_delta events: %+v\nwant %q", evs, want)
	}
	v, _ := kvValue(h, KVPRTrivial(pr.ID))
	if skip, ok := ParseTrivialSkip(v); !ok || skip.Note() != "empty-commit push skipped (b1 → b2)" {
		t.Fatalf("%s = %q", KVPRTrivial(pr.ID), v)
	}
	if n := gh.looks() - polled; n != 1 {
		t.Fatalf("second looks at the push = %d, want one", n)
	}
	h.advance(time.Hour)
	h.tick()
	if n := len(h.rd.all()); n != 1 || gh.looks()-polled != 1 || h.pr(2).State != store.PRReviewed {
		t.Fatalf("a later tick: rounds %d, second looks %d, state %s, want the settled PR left alone", n, gh.looks()-polled, h.pr(2).State)
	}
}

// A queued small delta that still changes code starts its round as it did:
// the second look is one comparison of the push, made before the round, and
// nothing else changes.
func TestAQueuedSmallDeltaThatChangesCodeStartsItsRound(t *testing.T) {
	h, gh := recheckHarness(t, withoutDeltaCheck)
	pr := pushedWithDelta(t, h, rubyMixed)
	if w := waitOf(t, h, 2); w.Reason != WaitDelta {
		t.Fatalf("setup: wait = %+v, want the small delta's hold", w)
	}
	roundsToday, polled := pr.RoundsToday, gh.looks()
	h.advance(2 * time.Hour)
	h.tick()

	ins := h.rd.all()
	if len(ins) != 2 || ins[1].TargetSHA != "b2" {
		t.Fatalf("rounds = %d, want the re-review of b2", len(ins))
	}
	if pr = h.pr(2); pr.State != store.PRReviewed || deref(pr.ReviewedSHA) != "b2" || pr.RoundsToday != roundsToday+1 || len(trivialEvents(t, h)) != 0 {
		t.Fatalf("state %s reviewed_sha %q rounds_today %d (was %d), want the round's review counted and no trivial settle",
			pr.State, deref(pr.ReviewedSHA), pr.RoundsToday, roundsToday)
	}
	if n := gh.looks() - polled; n != 1 {
		t.Fatalf("second looks at the push = %d, want one", n)
	}
}

// A comparison that fails measures nothing: the round starts as it did.
func TestAFailedSecondLookStartsTheRound(t *testing.T) {
	h, gh := recheckHarness(t)
	queuedEmptyPush(t, h)
	polled := gh.looks()
	h.advance(2 * time.Hour)
	h.gh.mu.Lock()
	h.gh.filesErr = errors.New("HTTP 502")
	h.gh.mu.Unlock()
	h.tick()

	ins := h.rd.all()
	if len(ins) != 2 || ins[1].TargetSHA != "b2" {
		t.Fatalf("rounds = %d, want the re-review of b2: the push could not be measured", len(ins))
	}
	if len(trivialEvents(t, h)) != 0 {
		t.Fatal("a push that could not be measured settled as trivial")
	}
	if n := gh.looks() - polled; n != 1 {
		t.Fatalf("second looks at the push = %d, want the failed one", n)
	}
}

// A round someone asked for (a review request, `magnum review`) is not the
// re-review that waited for a small delta: it runs in full and measures
// nothing again.
func TestAReviewSomeoneAskedForDoesNotMeasureThePushAgain(t *testing.T) {
	for _, tc := range []struct {
		name string
		ask  func(h *harness)
	}{
		{"review request", func(h *harness) {
			ev := reqAsk(h, "alice", askedPoll)
			reqPoll(h, prSpec{n: 2, head: "b2", requests: []github.ReviewRequestEvent{ev}})
			h.advance(time.Minute) // the debounce ends
		}},
		{"magnum review", func(h *harness) {
			h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, gh := recheckHarness(t)
			queuedEmptyPush(t, h)
			polled := gh.looks()
			h.advance(time.Minute)
			tc.ask(h)
			h.tick()

			ins := h.rd.all()
			if len(ins) != 2 || ins[1].TargetSHA != "b2" {
				t.Fatalf("rounds = %d, want the asked review of b2", len(ins))
			}
			if n := gh.looks() - polled; n != 0 || len(trivialEvents(t, h)) != 0 {
				t.Fatalf("second looks %d, trivial events %d, want none for a review someone asked for", n, len(trivialEvents(t, h)))
			}
		})
	}
}

// A head is measured once per daemon run: a round that cannot start for
// another reason (here a paused Codex) is not measured again at the next
// ticks, and starts when the pause ends.
func TestTheSecondLookAtASmallDeltaIsMadeOncePerHead(t *testing.T) {
	h, gh := recheckHarness(t, withoutDeltaCheck)
	pushedWithDelta(t, h, rubyMixed)
	polled := gh.looks()
	h.advance(2 * time.Hour)
	until := h.clock.Now().Add(time.Hour)
	h.e.setToolPause(h.ctx, "codex", "usage_limit", "", until)
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	if n, rounds := gh.looks()-polled, len(h.rd.all()); n != 1 || rounds != 1 {
		t.Fatalf("second looks %d, rounds %d, want one look and the round waiting for the pause", n, rounds)
	}
	h.advance(time.Hour)
	h.tick()
	if ins := h.rd.all(); len(ins) != 2 || ins[1].TargetSHA != "b2" || gh.looks()-polled != 1 {
		t.Fatalf("rounds %d, second looks %d, want the round after the pause and no new look", len(ins), gh.looks()-polled)
	}
}

// Another push is a new head: its small delta is measured again.
func TestAnotherHeadOfASmallDeltaGetsItsOwnSecondLook(t *testing.T) {
	h, gh := recheckHarness(t, withoutDeltaCheck)
	pushedWithDelta(t, h, rubyMixed) // b2: two lines of code
	polled := gh.looks()
	h.advance(2 * time.Hour)
	h.e.setToolPause(h.ctx, "codex", "usage_limit", "", h.clock.Now().Add(24*time.Hour))
	h.tick()
	if n := gh.looks() - polled; n != 1 {
		t.Fatalf("second looks at b2 = %d, want one", n)
	}
	// A push on top of it: from the reviewed b1 the same two lines of code.
	h.gh.mu.Lock()
	h.gh.files["b1...b3"] = rubyMixed
	h.gh.compare["b1...b3"] = github.CompareStats{Commits: 2}
	h.gh.mu.Unlock()
	pollPR(h, time.Minute, 2, "b3")
	polled = gh.looks() // the poller's look at b3
	h.advance(2 * time.Hour)
	h.tick()
	if n := gh.looks() - polled; n != 1 {
		t.Fatalf("second looks at b3 = %d, want one for the new head", n)
	}
}
