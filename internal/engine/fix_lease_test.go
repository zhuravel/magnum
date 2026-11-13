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
	"github.com/zhuravel/magnum/internal/store/storetest"
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

	for range fixLeaseGrace / time.Minute { // the daemon's ticks, each of which fails to read the lease
		if got := len(h.rd.all()); got != rounds {
			t.Fatalf("rounds = %d at %s, want %d: a round started within the grace", got, h.clock.Now().Sub(failed), rounds)
		}
		h.advance(time.Minute)
		h.tick()
	}
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
	h.tick() // the PR was reviewed: no dispatch candidate, the tick's own look reads it
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

// While the factory's re-review of its push is still to come on magnum's
// side, `magnum review` is refused: a round of its fix in flight, or its
// re-review forced with its fix kept, waiting in line, whatever the lease
// says (the operator's review would take its place). The factory's loop
// waiting in phase reviewing (as factoryread reads it: no fixer at work)
// refuses nothing by itself (C7): a factory that stopped in that phase
// after magnum's re-review of its push posted held off every review of the
// PR for good, with no way out. The factory's round runs and names the fix.
func TestMagnumReviewIsRefusedWhileTheFactorysReReviewIsToCome(t *testing.T) {
	reviewing := leaseFromFactory(factoryread.Lease{Phase: registry.PRWorkReviewing, Head: "b2", Cycle: 1, Cap: 3, Worked: true}, time.Time{})
	lease := &fakeLease{}
	lease.set(reviewing, nil)
	h := leaseHarness(t, lease)
	pr := h.pr(2)
	rounds := len(h.rd.all())
	review := func() store.Request {
		t.Helper()
		id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
		h.handle()
		return h.request(id)
	}
	if why := h.e.factoryReviewRefusal(h.ctx, pr, "talkable/talkable#2"); why != "" {
		t.Errorf("the factory's loop in phase reviewing, with nothing to come, refuses: %q", why)
	}
	ref := fixwork.FixRef{Work: 7, Cycle: 1, Head: "b2"}
	if !h.e.reserve(pr.ID, &roundHandle{cancel: func() {}, fixRef: &ref}) { // a round of its fix in flight
		t.Fatal("the PR could not be reserved")
	}
	if why := h.e.factoryReviewRefusal(h.ctx, pr, "talkable/talkable#2"); !strings.Contains(why, "the factory's re-review of its push of b2 (cycle 1) runs") {
		t.Errorf("with a round of the factory's fix in flight: %q", why)
	}
	h.e.unreserve(pr.ID)
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
	lease.set(reviewing, nil) // the factory stopped before it read the round's end
	if r := review(); r.State != store.RequestDone {
		t.Errorf("magnum review once the factory's re-review posted: %s %q", r.State, deref(r.Result))
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

// The live configuration until a PR hands over: the daemon reads the
// factory's lease (FromApp wires the reader), and no PR has an owner row.
// Whatever the lease reader says, failing or holding, nothing changes: the
// round runs at once, `magnum review` is taken, no failed read is recorded
// or told, no wait names the factory and the status comment shows no fix of
// the factory's.
func TestAWiredReaderChangesNothingForAPRWithNoOwnerRow(t *testing.T) {
	storetest.Parallel(t)
	for _, tc := range []struct {
		name  string
		lease FixLease
		err   error
	}{
		{"a reader that fails", FixLease{}, errors.New("database is locked")},
		{"a reader whose lease holds", FixLease{Fixing: true, Phase: FixPhaseFixing, Head: "b2", Cycle: 1, Cap: 3}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lease := &fakeLease{}
			lease.set(tc.lease, tc.err)
			h := unownedHarness(t, lease)
			pr := h.pr(2)
			if w, ok := h.factoryWait(); ok {
				t.Errorf("a wait on the factory: %+v", w)
			}
			rounds := len(h.rd.all())
			h.tick()
			if got := len(h.rd.all()); got != rounds+1 {
				t.Errorf("rounds = %d, want %d: the re-review runs at once", got, rounds+1)
			}
			review := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
			h.handle()
			if r := h.request(review); r.State != store.RequestDone {
				t.Errorf("magnum review: %s %q", r.State, deref(r.Result))
			}
			if u, ok := h.leaseUnread(pr.ID); ok {
				t.Errorf("a failed read is recorded: %+v", u)
			}
			for _, kind := range []string{"fix.lease_open", "fix.lease_read"} {
				if ev, ok := h.fixEvent(kind); ok {
					t.Errorf("%s: %q", kind, ev.Message)
				}
			}
			if st, ok := h.e.factoryFix(h.ctx, h.pr(2)); ok {
				t.Errorf("the status comment shows the factory's fix: %+v", st)
			}
		})
	}
}

// When magnum's registry cannot read a PR's owner row, who fixes the PR is
// not known: the failed read gets the lease's grace (the engine owner's
// ruling on R2). Dispatch holds the round from the first failed read and
// `magnum review` is refused, both naming magnum's registry, never the
// factory's lease; the failed read is recorded with its source, so the
// doctor names it and each tick reads it again (sweepLeaseUnreads); once
// the grace ends magnum reviews the PR, with a warn event, and once the row
// reads the record goes.
func TestAnOwnerRowThatCannotBeReadGetsTheGraceThenFailsOpen(t *testing.T) {
	h := unownedHarness(t, &fakeLease{})
	pr := h.pr(2)
	failKVRead(h, fixwork.KVPRFixOwner(pr.ID))
	rounds := len(h.rd.all())
	failed := h.clock.Now()
	h.tick()
	if got := len(h.rd.all()); got != rounds {
		t.Fatalf("rounds = %d, want %d: a round started while who fixes the PR cannot be read", got, rounds)
	}
	if u, ok := h.leaseUnread(pr.ID); !ok || u.Source != LeaseFromRegistry || u.Open || !u.Since.Equal(failed) {
		t.Errorf("the failed read = %+v, %v; want it recorded from magnum's registry, not open", u, ok)
	}
	if w := waitOf(t, h, 2); w.Reason != WaitFixing || strings.Contains(w.Detail, "lease") || !strings.Contains(w.Detail, "its owner row, which magnum's registry cannot read") ||
		!w.Until.Equal(failed.Add(fixLeaseGrace)) {
		t.Errorf("wait = %+v, want magnum's registry named until the grace ends", w)
	}
	review := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	if r := h.request(review); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "magnum's registry cannot read its owner row") ||
		strings.Contains(deref(r.Result), "lease") {
		t.Errorf("magnum review: %s %q", r.State, deref(r.Result))
	}
	for range fixLeaseGrace / time.Minute { // the daemon's ticks, each of which fails to read the row
		h.advance(time.Minute)
		h.tick()
	}
	if got := len(h.rd.all()); got != rounds+1 {
		t.Fatalf("rounds = %d, want %d: magnum reviews after the grace", got, rounds+1)
	}
	if ev, ok := h.fixEvent("fix.lease_open"); !ok || ev.Level != "warn" || strings.Contains(ev.Message, "lease") ||
		!strings.Contains(ev.Message, "magnum's registry cannot read the PR's owner row") {
		t.Errorf("fix.lease_open = %+v, %v; want a warn naming magnum's registry", ev, ok)
	}
	unread, err := FixLeaseUnreads(h.ctx, h.st)
	if u := unread[pr.ID]; err != nil || !u.Open || u.Source != LeaseFromRegistry {
		t.Errorf("the doctor reads %+v (%v), want the failed read of magnum's registry, open", unread, err)
	}
	h.e.delKV(h.ctx, fixwork.KVPRFixOwner(pr.ID)) // the row reads again: none
	h.tick()                                      // the PR was reviewed: no dispatch candidate, the tick's own look reads it
	if u, ok := h.leaseUnread(pr.ID); ok {
		t.Errorf("a read that works left the failed one: %+v", u)
	}
	if ev, ok := h.fixEvent("fix.lease_read"); !ok || strings.Contains(ev.Message, "lease") || !strings.Contains(ev.Message, "owner row") {
		t.Errorf("fix.lease_read = %q, %v; want the owner row named", ev.Message, ok)
	}
}

// An owner row this build cannot parse (a newer build's, or a damaged one)
// is a failed read within the grace, as the factory's lease is, but it is
// named as magnum's own row, never as the factory's lease: in the wait, in
// `magnum review`'s refusal, in the row the doctor reads, in the warn the
// daemon logs, in fix.lease_open after the grace and in fix.lease_read once
// the row reads (C3).
func TestAnOwnerRowThatDoesNotParseIsNamedAsMagnumsOwn(t *testing.T) {
	logs := &syncBuffer{}
	h := newHarness(t, func(h *harness) {
		h.d.FixLeaseOf = (&fakeLease{}).reader(h.clock.Now)
		h.d.Logger = newTeeLogger(t, logs)
	})
	h.reviewedPR(2, "b1")
	factoryOwns(h)
	pollPR(h, time.Hour, 2, "b2")
	h.advance(6 * time.Minute) // past the push quiet period
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
	if l := logs.String(); strings.Contains(l, "the factory's lease") || !strings.Contains(l, "owner row of a PR cannot be parsed by this build; its round waits") {
		t.Errorf("the warn names the factory's lease, or not the owner row:\n%s", l)
	}
	for range fixLeaseGrace / time.Minute { // the daemon's ticks, each of which fails to parse the row
		h.advance(time.Minute)
		h.tick()
	}
	if got := len(h.rd.all()); got != rounds+1 {
		t.Fatalf("rounds = %d, want %d: magnum reviews after the grace", got, rounds+1)
	}
	if ev, ok := h.fixEvent("fix.lease_open"); !ok || strings.Contains(ev.Message, "lease") || !strings.Contains(ev.Message, "owner row") {
		t.Errorf("fix.lease_open = %q, %v; want the owner row named, not the lease", ev.Message, ok)
	}
	h.e.delKV(h.ctx, fixwork.KVPRFixOwner(pr.ID)) // the row goes: magnum fixes the PR, and the read works
	h.tick()
	if ev, ok := h.fixEvent("fix.lease_read"); !ok || strings.Contains(ev.Message, "lease") || !strings.Contains(ev.Message, "owner row") {
		t.Errorf("fix.lease_read = %q, %v; want the owner row named, not the lease", ev.Message, ok)
	}
}

// A failed lease read the doctor names lasts only while it does: each tick
// reads again the lease of every PR it is recorded for, round or none (a
// PR reviewed after it failed open is no dispatch candidate any more), and
// drops it once the lease reads, the factory no longer fixes the PR, or the
// PR closed.
func TestAFailedLeaseReadGoesOnceItReadsOrThePRLeaves(t *testing.T) {
	storetest.Parallel(t)
	for _, tc := range []struct {
		name string
		act  func(h *harness, lease *fakeLease)
		gone bool
	}{
		{"the lease still fails", func(*harness, *fakeLease) {}, false},
		{"the lease reads again", func(_ *harness, lease *fakeLease) { lease.set(FixLease{}, nil) }, true},
		{"the factory no longer fixes the PR", func(h *harness, _ *fakeLease) { h.e.delKV(h.ctx, fixwork.KVPRFixOwner(h.pr(2).ID)) }, true},
		{"the PR closed", func(h *harness, _ *fakeLease) {
			h.open(prSpec{n: 1, head: "base1"}) // #2 left the OPEN list: closed, which the next tick confirms
			h.gh.closed[2] = "CLOSED"
			h.tick()
			h.advance(confirmGap)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lease := &fakeLease{}
			lease.set(FixLease{}, errors.New("database is locked"))
			h := newHarness(t, func(h *harness) { h.d.FixLeaseOf = lease.reader(h.clock.Now) })
			pr := h.reviewedPR(2, "b1")
			factoryOwns(h)
			// It failed open, and the round it let through left the PR reviewed.
			h.e.setKV(h.ctx, KVPRFixLeaseUnread(pr.ID), rowJSON(LeaseUnread{Since: h.clock.Now().Add(-time.Hour).UTC(), Err: "database is locked", Open: true}))
			tc.act(h, lease)
			h.tick()
			if _, ok := h.leaseUnread(pr.ID); ok == tc.gone {
				t.Errorf("the failed read recorded: %v, want %v", ok, !tc.gone)
			}
		})
	}
}

// The grace counts from the first of failed reads that follow each other: a
// read that fails after a gap with none (the daemon stopped, the Mac
// slept) is a first failure again, which holds the round for a new grace,
// rather than one that fails open at once on a first failure hours old.
func TestAFailureAfterAGapStartsTheGraceAgain(t *testing.T) {
	lease := &fakeLease{}
	lease.set(FixLease{}, errors.New("database is locked"))
	h := leaseHarness(t, lease)
	pr := h.pr(2)
	rounds := len(h.rd.all())
	h.tick()
	if u, ok := h.leaseUnread(pr.ID); !ok || u.Open {
		t.Fatalf("the first failed read = %+v, %v", u, ok)
	}
	h.advance(2 * time.Hour) // no tick: the daemon was stopped
	h.tick()
	if got := len(h.rd.all()); got != rounds {
		t.Errorf("rounds = %d, want %d: a failure after a gap holds the round for a new grace", got, rounds)
	}
	if u, ok := h.leaseUnread(pr.ID); !ok || u.Open || !u.Since.Equal(h.clock.Now()) {
		t.Errorf("the failed read = %+v, %v; want a first failure now, not open", u, ok)
	}
	if ev, ok := h.fixEvent("fix.lease_open"); ok {
		t.Errorf("it failed open at once: %q", ev.Message)
	}
}

// The gap that starts the grace anew is a few poll intervals at least: under
// a poll_interval of 6 minutes, each tick 6 minutes after the last, the
// failed reads follow each other, so the grace counts from the first, the
// round fails open once it ends, and the warn is logged once, not at every
// tick (C2).
func TestALongPollIntervalStillFailsOpenAfterTheGrace(t *testing.T) {
	logs := &syncBuffer{}
	lease := &fakeLease{}
	lease.set(FixLease{}, errors.New("database is locked"))
	const interval = 6 * time.Minute
	h := newHarness(t, func(h *harness) {
		h.cfg.Daemon.PollInterval.Duration = interval
		h.d.FixLeaseOf = lease.reader(h.clock.Now)
		h.d.Logger = newTeeLogger(t, logs)
	})
	h.reviewedPR(2, "b1")
	factoryOwns(h)
	pollPR(h, time.Hour, 2, "b2")
	h.advance(6 * time.Minute) // past the push quiet period
	pr := h.pr(2)
	rounds := len(h.rd.all())
	first := h.clock.Now()
	h.tick()
	if got := len(h.rd.all()); got != rounds {
		t.Fatalf("rounds = %d, want %d: the grace holds the round", got, rounds)
	}
	h.advance(interval)
	h.tick()
	if u, ok := h.leaseUnread(pr.ID); !ok || !u.Since.Equal(first) {
		t.Errorf("the failed read = %+v, %v; want its grace from the first failure at %v", u, ok, first)
	}
	h.advance(interval)
	h.tick()
	if got := len(h.rd.all()); got != rounds+1 {
		t.Errorf("rounds = %d, want %d: magnum reviews once the grace ends", got, rounds+1)
	}
	if n := len(h.fixEvents("fix.lease_open")); n != 1 {
		t.Errorf("fix.lease_open events = %d, want 1", n)
	}
	if n := strings.Count(logs.String(), "its round waits"); n != 1 {
		t.Errorf("the warn was logged %d times, want once", n)
	}
}

// A dry-run daemon, whose registry is a private copy, records a failed
// lease read as the real one does: it warns once, not at every tick, and
// shows the fail-open the real daemon would do once the grace ends.
func TestADryRunDaemonRecordsAFailedLeaseRead(t *testing.T) {
	logs := &syncBuffer{}
	lease := &fakeLease{}
	lease.set(FixLease{}, errors.New("database is locked"))
	h := newHarness(t, func(h *harness) {
		h.d.FixLeaseOf = lease.reader(h.clock.Now)
		h.d.Logger = newTeeLogger(t, logs)
	})
	h.reviewedPR(2, "b1")
	factoryOwns(h)
	pollPR(h, time.Hour, 2, "b2")
	h.advance(6 * time.Minute)
	pr := h.pr(2)
	h.e.d.DryRun = true
	for range fixLeaseGrace/time.Minute + 1 {
		h.tick()
		h.advance(time.Minute)
	}
	if n := strings.Count(logs.String(), "the factory's lease of a PR cannot be read; its round waits"); n != 1 {
		t.Errorf("the warn was logged %d times, want once", n)
	}
	if u, ok := h.leaseUnread(pr.ID); !ok || !u.Open {
		t.Errorf("the failed read = %+v, %v; want recorded and open after the grace", u, ok)
	}
	if _, ok := h.fixEvent("fix.lease_open"); !ok {
		t.Error("no fix.lease_open after the grace")
	}
}

// A PR the factory fixes whose lease cannot be read waits on the factory
// only once a failed read is recorded, which dispatch and `magnum review`
// do: before that (the PR is no dispatch candidate yet, inside its push
// quiet period) its wait is what holds it, and it stays put from tick to
// tick instead of a grace that starts anew at each one.
func TestAnUnrecordedFailedLeaseReadIsNoWait(t *testing.T) {
	lease := &fakeLease{}
	lease.set(FixLease{}, errors.New("database is locked"))
	h := newHarness(t, func(h *harness) { h.d.FixLeaseOf = lease.reader(h.clock.Now) })
	h.reviewedPR(2, "b1")
	factoryOwns(h)
	pollPR(h, time.Hour, 2, "b2") // inside the push quiet period: no candidate
	pr := h.wantState(2, store.PRRereviewPending)
	if u, ok := h.leaseUnread(pr.ID); ok {
		t.Fatalf("a failed read was recorded for a PR no round looked at: %+v", u)
	}
	w := waitOf(t, h, 2)
	if w.Subject == waitFactory {
		t.Errorf("wait = %+v, want what holds the PR, not the factory's lease", w)
	}
	first, _ := kvValue(h, KVPRWait(pr.ID))
	h.advance(time.Minute)
	h.tick()
	if again, _ := kvValue(h, KVPRWait(pr.ID)); again != first {
		t.Errorf("the wait moved from %s to %s", first, again)
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
