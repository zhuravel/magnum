package engine

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factoryread"
	"github.com/zhuravel/magnum/internal/store"
)

// leaseHarness is a harness whose factory lease reader is lease's, with
// PR #2 reviewed at b1, fixed by the factory, then pushed to b2 an hour
// later: its re-review is due once the push quiet period ends.
func leaseHarness(t *testing.T, lease *fakeLease) *harness {
	t.Helper()
	h := newHarness(t, func(h *harness) {
		if lease != nil {
			h.d.FixLeaseOf = lease.reader(h.clock.Now)
		}
	})
	h.reviewedPR(2, "b1")
	factoryOwns(h)
	pollPR(h, time.Hour, 2, "b2")
	h.wantState(2, store.PRRereviewPending)
	h.advance(6 * time.Minute) // past the push quiet period
	return h
}

// A PR the factory fixes whose lease cannot be read: its round waits for
// the grace, from the first failed read, and `magnum review` is refused
// meanwhile; then the review side fails open: magnum reviews, a warn event
// says so once, and the PR's failed read is what the doctor names, until a
// read works again.
func TestMagnumReviewsAfterTheGraceWithAWarnAndADoctorLine(t *testing.T) {
	lease := &fakeLease{}
	lease.set(FixLease{}, errors.New("database is locked"))
	h := leaseHarness(t, lease)
	pr := h.pr(2)
	rounds := len(h.rd.all())
	failed := h.clock.Now()
	h.tick()
	if got := len(h.rd.all()); got != rounds {
		t.Fatalf("rounds = %d, want %d: a round started on a lease that cannot be read", got, rounds)
	}
	w := waitOf(t, h, 2)
	if w.Reason != WaitFixing || w.Subject != waitFactory || !w.Until.Equal(failed.Add(fixLeaseGrace)) {
		t.Errorf("wait = %+v, want the factory's lease until %v", w, failed.Add(fixLeaseGrace))
	}
	if s := w.Sentence("talkable/talkable#2", h.clock.Now()); !strings.Contains(s, "the factory's lease, which cannot be read (database is locked) until") ||
		strings.Contains(s, "magnum abort") {
		t.Errorf("sentence = %q", s)
	}
	if u, ok := h.leaseUnread(pr.ID); !ok || u.Open || !u.Since.Equal(failed) {
		t.Errorf("failed read = %+v, %v; want recorded, not open", u, ok)
	}
	review := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	if r := h.request(review); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "the factory's lease of it cannot be read") {
		t.Errorf("magnum review within the grace: %s %q", r.State, deref(r.Result))
	}
	if _, ok := h.fixEvent("fix.lease_open"); ok {
		t.Error("the warn came within the grace")
	}

	h.advance(fixLeaseGrace)
	h.tick()
	if got := len(h.rd.all()); got != rounds+1 {
		t.Errorf("rounds = %d, want %d: magnum reviews after the grace", got, rounds+1)
	}
	ev, ok := h.fixEvent("fix.lease_open")
	if !ok || ev.Level != "warn" || !strings.Contains(ev.Message, "magnum reviews it without the lease") {
		t.Errorf("fix.lease_open = %+v, %v", ev, ok)
	}
	unread, err := FixLeaseUnreads(h.ctx, h.st)
	if u := unread[pr.ID]; err != nil || !u.Open || u.Err != "database is locked" {
		t.Errorf("the doctor reads %+v (%v), want the failed read, open", unread, err)
	}
	h.tick()
	if n := len(h.fixEvents("fix.lease_open")); n != 1 {
		t.Errorf("fix.lease_open events = %d, want 1", n)
	}

	lease.set(FixLease{}, nil)
	h.e.factoryHolds(h.ctx, h.pr(2))
	if u, ok := h.leaseUnread(pr.ID); ok {
		t.Errorf("a read that works left the failed one: %+v", u)
	}
	if _, ok := h.fixEvent("fix.lease_read"); !ok {
		t.Error("no fix.lease_read event")
	}
}

// With no lease reader wired, a PR the factory fixes is reviewed exactly as
// before: no round waits, `magnum review` is not refused, no warn event,
// no failed read for the doctor.
func TestAnUnwiredLeaseReaderChangesNothing(t *testing.T) {
	h := leaseHarness(t, nil)
	pr := h.pr(2)
	rounds := len(h.rd.all())
	h.tick()
	if got := len(h.rd.all()); got != rounds+1 {
		t.Errorf("rounds = %d, want %d: the re-review runs", got, rounds+1)
	}
	if _, ok := h.factoryWait(); ok {
		t.Error("a round waits on the factory")
	}
	review := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	if r := h.request(review); r.State != store.RequestDone {
		t.Errorf("magnum review: %s %q", r.State, deref(r.Result))
	}
	for _, kind := range []string{"fix.lease_open", "fix.lease_read"} {
		if _, ok := h.fixEvent(kind); ok {
			t.Errorf("a %s event", kind)
		}
	}
	if u, ok := h.leaseUnread(pr.ID); ok {
		t.Errorf("a failed read is recorded: %+v", u)
	}
}

// While the factory's fixer works on a PR it fixes, the PR's round waits
// for it, as for magnum's own fixer, and `magnum review` is refused; its
// push's re-review, in phase reviewing, passes.
func TestTheFactorysFixerHoldsTheRoundAndRefusesAReview(t *testing.T) {
	lease := &fakeLease{}
	lease.set(FixLease{Fixing: true, Phase: FixPhaseFixing, Head: "b2", Cycle: 1, Cap: 3}, nil)
	h := leaseHarness(t, lease)
	rounds := len(h.rd.all())
	h.tick()
	if got := len(h.rd.all()); got != rounds {
		t.Fatalf("rounds = %d, want %d: a round started while the factory's fixer works", got, rounds)
	}
	if w := waitOf(t, h, 2); w.Reason != WaitFixing || w.Subject != waitFactory || w.Detail != "the factory's fixer (fix cycle 1 of 3)" {
		t.Errorf("wait = %+v", w)
	}
	review := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	if r := h.request(review); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "the factory fixes it (cycle 1 of 3, its fixer at work") {
		t.Errorf("magnum review: %s %q", r.State, deref(r.Result))
	}
	lease.set(FixLease{Fixing: true, Phase: FixPhaseReviewing, Head: "b2", Cycle: 1, Cap: 3}, nil)
	h.tick()
	if got := len(h.rd.all()); got != rounds+1 {
		t.Errorf("rounds = %d, want %d: the re-review of the factory's push runs", got, rounds+1)
	}
}

// leaseUnread is PR prID's recorded failed lease read (ok false: none).
func (h *harness) leaseUnread(prID int64) (LeaseUnread, bool) {
	h.t.Helper()
	return h.e.leaseUnreadOf(h.ctx, prID)
}

// factoryWait is PR #2's wait on the factory, read now.
func (h *harness) factoryWait() (Wait, bool) {
	h.t.Helper()
	return h.e.factoryWaitOf(h.ctx, h.pr(2))
}

// The daemon reads the factory's lease through factoryread: a lease that
// holds an agent or a push is the factory's fixer at work, which holds the
// PR's round, a loop waiting for magnum's re-review holds nothing, and so
// does every other phase and a lease that ran out; the cycle, the cap, the
// head and the work since the import go as read. A registry that is not
// there is the zero lease, and no error.
func TestTheFactorysLeaseIsReadAsTheReviewSideTakesIt(t *testing.T) {
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		lease factoryread.Lease
		want  FixLease
	}{
		{factoryread.Lease{Fixing: true, Phase: registry.PRWorkLocalReview, Head: "c3c3c3c", Cycle: 2, Cap: 3, Worked: true},
			FixLease{Fixing: true, Phase: FixPhaseFixing, Head: "c3c3c3c", Cycle: 2, Cap: 3, Worked: true, ReadAt: now}},
		{factoryread.Lease{Phase: registry.PRWorkReviewing, Head: "c3c3c3c", Cycle: 1, Cap: 3, Worked: true},
			FixLease{Phase: FixPhaseReviewing, Head: "c3c3c3c", Cycle: 1, Cap: 3, Worked: true, ReadAt: now}},
		{factoryread.Lease{Phase: registry.PRWorkFixing, Cycle: 1, Cap: 3}, FixLease{Cycle: 1, Cap: 3, ReadAt: now}}, // the lease ran out
		{factoryread.Lease{Phase: registry.PRWorkQueued}, FixLease{ReadAt: now}},
	} {
		if got := leaseFromFactory(tc.lease, now); got != tc.want {
			t.Errorf("leaseFromFactory(%+v) = %+v, want %+v", tc.lease, got, tc.want)
		}
		if l := leaseFromFactory(tc.lease, now); (leaseView{owned: true, lease: l}).holds() != (tc.lease.Fixing) {
			t.Errorf("%s: holds the round %v, want %v", tc.lease.Phase, !tc.lease.Fixing, tc.lease.Fixing)
		}
	}
	read := factoryLeaseReader(filepath.Join(t.TempDir(), "factory", "factory.db"), func() time.Time { return now })
	if l, err := read(t.Context(), "talkable/talkable", 2); err != nil || l != (FixLease{ReadAt: now}) {
		t.Errorf("a registry that is not there = %+v, %v; want the zero lease", l, err)
	}
}
