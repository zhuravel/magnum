package engine

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factoryread"
	"github.com/zhuravel/magnum/internal/fixwork"
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
	// The lease of a loop waiting for its re-review, as factoryread reads it.
	lease.set(leaseFromFactory(factoryread.Lease{Phase: registry.PRWorkReviewing, Head: "b2", Cycle: 1, Cap: 3}, time.Time{}), nil)
	h.tick()
	if got := len(h.rd.all()); got != rounds+1 {
		t.Errorf("rounds = %d, want %d: the re-review of the factory's push runs", got, rounds+1)
	}
}

// While the factory waits for magnum's re-review of its push (its lease in
// phase reviewing, as factoryread reads it: no fixer at work), `magnum
// review` is refused, and so it is while the factory's re-review waits in
// line (forced, its fix kept), whatever the lease says: the operator's
// review would take its place and drop its fix. The factory's round then
// runs and names the fix.
func TestMagnumReviewIsRefusedWhileTheFactoryWaitsForItsReReview(t *testing.T) {
	lease := &fakeLease{}
	lease.set(leaseFromFactory(factoryread.Lease{Phase: registry.PRWorkReviewing, Head: "b2", Cycle: 1, Cap: 3, Worked: true}, time.Time{}), nil)
	h := leaseHarness(t, lease)
	pr := h.pr(2)
	rounds := len(h.rd.all())
	review := func() store.Request {
		t.Helper()
		id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
		h.handle()
		return h.request(id)
	}
	if r := review(); r.State != store.RequestFailed ||
		!strings.Contains(deref(r.Result), "the factory fixes it (cycle 1 of 3, waiting for the re-review of its push at b2)") {
		t.Errorf("magnum review while the factory waits in reviewing: %s %q", r.State, deref(r.Result))
	}
	ref := fixwork.FixRef{Work: 7, Cycle: 1, Head: "b2"}
	if r := h.reReviewOf(ref); r.State != store.RequestDone {
		t.Fatalf("the factory's re-review: %s %q", r.State, deref(r.Result))
	}
	lease.set(FixLease{}, nil) // a lease that says nothing: the fix in line still refuses
	if r := review(); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "the factory's re-review of its push of b2 (cycle 1) waits in line") {
		t.Errorf("magnum review while the factory's re-review waits: %s %q", r.State, deref(r.Result))
	}
	if kept, raw := h.e.fixRefOf(h.ctx, pr.ID); kept == nil || kept.FixRef != ref {
		t.Fatalf("the factory's fix = %s, want %+v kept", raw, ref)
	}
	h.tick()
	if got := len(h.rd.all()); got != rounds+1 {
		t.Fatalf("rounds = %d, want %d: the factory's re-review runs", got, rounds+1)
	}
	if ev, ok := h.fixEvent("fix.factory_round"); !ok || !strings.Contains(ev.Message, "the re-review of the factory's push of b2 (cycle 1)") {
		t.Errorf("fix.factory_round = %+v, %v", ev, ok)
	}
}

// unownedHarness is a harness whose factory lease reader is lease's, with
// PR #2 reviewed at b1 and pushed to b2 an hour later, past its push quiet
// period, and no owner row: magnum fixes it.
func unownedHarness(t *testing.T, lease *fakeLease) *harness {
	t.Helper()
	h := newHarness(t, func(h *harness) { h.d.FixLeaseOf = lease.reader(h.clock.Now) })
	h.reviewedPR(2, "b1")
	pollPR(h, time.Hour, 2, "b2")
	h.wantState(2, store.PRRereviewPending)
	h.advance(6 * time.Minute)
	return h
}

// When magnum's registry cannot read a PR's owner row, who fixes the PR is
// not known: dispatch holds its round, as for a fix loop's row that cannot
// be read, and `magnum review` is refused naming that read, but no failed
// read of the factory's lease is recorded or told: the factory may never
// have had the PR. Once the row reads, the round runs, with no lease event.
func TestAnOwnerRowThatCannotBeReadIsNoFailedLeaseRead(t *testing.T) {
	h := unownedHarness(t, &fakeLease{})
	pr := h.pr(2)
	failKVRead(h, fixwork.KVPRFixOwner(pr.ID))
	rounds := len(h.rd.all())
	h.tick()
	if got := len(h.rd.all()); got != rounds {
		t.Fatalf("rounds = %d, want %d: a round started while who fixes the PR cannot be read", got, rounds)
	}
	if u, ok := h.leaseUnread(pr.ID); ok {
		t.Errorf("a failed lease read is recorded: %+v", u)
	}
	if w := waitOf(t, h, 2); w.Reason != WaitFixing || strings.Contains(w.Detail, "lease") || !strings.Contains(w.Detail, "owner row") {
		t.Errorf("wait = %+v, want the owner row named, not the factory's lease", w)
	}
	review := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	if r := h.request(review); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "who fixes it cannot be read") ||
		strings.Contains(deref(r.Result), "lease") {
		t.Errorf("magnum review: %s %q", r.State, deref(r.Result))
	}
	h.e.delKV(h.ctx, fixwork.KVPRFixOwner(pr.ID)) // the row reads again: none
	h.tick()
	if got := len(h.rd.all()); got != rounds+1 {
		t.Errorf("rounds = %d, want %d once the row reads", got, rounds+1)
	}
	for _, kind := range []string{"fix.lease_open", "fix.lease_read"} {
		if ev, ok := h.fixEvent(kind); ok {
			t.Errorf("%s on a PR the factory never had: %q", kind, ev.Message)
		}
	}
}

// An owner row this build cannot parse (a newer build's, or a damaged one)
// is a failed read within the grace, as the factory's lease is, but it is
// named as magnum's own row, never as the factory's registry: in the wait,
// in `magnum review`'s refusal and in the row the doctor reads.
func TestAnOwnerRowThatDoesNotParseIsNamedAsMagnumsOwn(t *testing.T) {
	h := leaseHarness(t, &fakeLease{})
	pr := h.pr(2)
	if err := h.st.SetKV(h.ctx, fixwork.KVPRFixOwner(pr.ID), `{"owner":"someone newer"}`); err != nil {
		t.Fatal(err)
	}
	rounds := len(h.rd.all())
	h.tick()
	if got := len(h.rd.all()); got != rounds {
		t.Fatalf("rounds = %d, want %d: the grace holds the round", got, rounds)
	}
	if raw, _ := kvValue(h, KVPRFixLeaseUnread(pr.ID)); !strings.Contains(raw, `"source":"owner"`) {
		t.Errorf("the failed read = %s, want its source the owner row", raw)
	}
	if w := waitOf(t, h, 2); !strings.Contains(w.Detail, "its owner row, which this build cannot read") || strings.Contains(w.Detail, "lease") {
		t.Errorf("wait = %q, want the owner row named", w.Detail)
	}
	review := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	if r := h.request(review); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "its owner row cannot be read by this build") {
		t.Errorf("magnum review: %s %q", r.State, deref(r.Result))
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
