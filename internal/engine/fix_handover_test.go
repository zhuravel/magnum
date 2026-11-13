package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/fixwork"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// ownerOf is PR #2's owner row (ok false: none, magnum fixes it).
func ownerOf(h *harness) (fixwork.FixOwner, bool) {
	h.t.Helper()
	raw, ok := kvValue(h, fixwork.KVPRFixOwner(h.pr(2).ID))
	if !ok {
		return fixwork.FixOwner{}, false
	}
	o, err := fixwork.ParseFixOwner(raw)
	if err != nil {
		h.t.Fatal(err)
	}
	return o, true
}

// handOver asks for p's handover and returns the request.
func (h *harness) handOver(p HandoverPayload) store.Request {
	h.t.Helper()
	id := h.enqueue(ReqFixHandover, p)
	h.handle()
	return h.request(id)
}

// handOverPR2 is HandoverPayload naming PR #2.
func handOverPR2(back bool) HandoverPayload {
	return HandoverPayload{PRTarget: PRTarget{Ref: "2"}, Back: back}
}

// factoryFixOn is the harness's [factory] fix = "on": the factory fixes
// own PRs, so magnum hands them over.
func factoryFixOn(h *harness) { h.cfg.Factory.Fix = config.FactoryFixOn }

// While [factory] fix is not "on", nothing on the factory's side fixes a
// PR: `magnum fix --handover` refuses one PR and every own PR, its dry run
// too, naming the key, and writes no row; the way back still answers.
func TestTheHandoverIsRefusedWhileTheFactoryDoesNotFix(t *testing.T) {
	h, _ := fixHarness(t)
	pr := fixablePR(h, "zhuravel", nil)
	for _, p := range []HandoverPayload{handOverPR2(false), {PRTarget: PRTarget{Ref: "2"}, DryRun: true}, {All: true}, {All: true, DryRun: true}} {
		r := h.handOver(p)
		if r.State != store.RequestFailed || !strings.Contains(deref(r.Result), `[factory] fix is "off"`) ||
			!strings.Contains(deref(r.Result), "nothing is handed over") {
			t.Errorf("%+v: %s %q, want refused naming [factory] fix", p, r.State, deref(r.Result))
		}
	}
	if _, ok := ownerOf(h); ok {
		t.Error("the PR flipped to a factory that does not fix")
	}
	for _, key := range []string{KVPRFixHandover(pr.ID), KVFixHandoverAll} {
		if v, ok := kvValue(h, key); ok {
			t.Errorf("%s = %s, want no handover", key, v)
		}
	}
	if r := h.handOver(handOverPR2(true)); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "magnum fixes talkable/talkable#2 already") {
		t.Errorf("back: %s %q", r.State, deref(r.Result))
	}
	factoryFixOn(h)
	if r := h.handOver(handOverPR2(false)); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "the factory fixes talkable/talkable#2 from now on") {
		t.Errorf("with [factory] fix on: %s %q", r.State, deref(r.Result))
	}
}

// Each record of magnum's fix work in flight on a PR keeps it from handing
// over, and names itself with the operator's action: a loop's row, a
// fixer's push record, a held merge, magnum's push waiting for its CI, a
// pending fix or abort request, a fix run whose verdict is not written and
// a fix job's reservation. With none, the PR is quiet. (A held fix is none:
// TestAHeldFixDoesNotBlockTheFlipAndStaysForTheImport.)
func TestFixQuietIsFalseForEachRecordInFlight(t *testing.T) {
	h, _ := fixHarness(t)
	pr := fixablePR(h, "zhuravel", nil)
	repo := h.heldRepo()
	quiet := func() (bool, []FixRecord) {
		t.Helper()
		q, recs, err := h.e.fixQuiet(h.ctx, repo, pr)
		if err != nil {
			t.Fatal(err)
		}
		return q, recs
	}
	if q, recs := quiet(); !q {
		t.Fatalf("a PR with no fix work is not quiet: %+v", recs)
	}
	set := func(key, value string) func() {
		if err := h.st.SetKV(h.ctx, key, value); err != nil {
			t.Fatal(err)
		}
		return func() { h.e.delKV(h.ctx, key) }
	}
	now := h.clock.Now().UTC()
	for _, tc := range []struct {
		name, kind string
		add        func() (undo func())
	}{
		{"a loop's row", FixRecLoop, func() func() {
			return set(KVPRFix(pr.ID), rowJSON(FixState{Cycle: 1, Cap: 3, Phase: FixPhaseFixing, Head: fixHead, StartedAt: now}))
		}},
		{"a push record", FixRecPush, func() func() {
			return set(kvPRFixPush(pr.ID), rowJSON(fixPush{Kind: fixPushUnconfirmed, Run: "fix-1", Pushed: fixPushed, Failed: now}))
		}},
		{"a held merge", FixRecHeldMerge, func() func() {
			return set(KVPRHeldMerge(pr.ID), rowJSON(HeldMerge{Repo: "talkable/talkable", Number: 2, Merge: fixMergeHead, Head: fixHead, Base: "master", Tip: fixBaseTip}))
		}},
		{"magnum's push waiting for its CI", FixRecPushed, func() func() {
			return set(kvPRPushed(pr.ID), rowJSON(ciPush{Head: fixPushed, At: now}))
		}},
		{"a pending fix request", FixRecRequest, func() func() {
			id := h.fix(FixPayload{})
			return func() { _ = h.st.CompleteRequest(h.ctx, id, store.RequestDone, "") }
		}},
		{"a pending abort request", FixRecRequest, func() func() {
			id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Repo: "talkable/talkable", Number: 2}})
			return func() { _ = h.st.CompleteRequest(h.ctx, id, store.RequestDone, "") }
		}},
		{"a fix run not ended", FixRecRun, func() func() {
			run, err := h.st.CreateRun(h.ctx, store.Run{ID: "fix-run-1", PRID: pr.ID, Round: 1, Role: "claude-fix", Kind: store.RunFix,
				TargetSHA: fixHead, Identity: "zhuravel", State: store.RunWorking})
			if err != nil {
				t.Fatal(err)
			}
			return func() { h.e.abandonFixRun(h.ctx, run, "the test ends it") }
		}},
		{"a fix job's reservation", FixRecReservation, func() func() {
			if !h.e.reserve(pr.ID, &roundHandle{cancel: func() {}, fix: true}) {
				t.Fatal("the PR could not be reserved")
			}
			return func() { h.e.unreserve(pr.ID) }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			undo := tc.add()
			q, recs := quiet()
			if q || len(recs) != 1 || recs[0].Kind != tc.kind || recs[0].What == "" || recs[0].Action == "" {
				t.Errorf("quiet %v, records %+v; want one %s record with its action", q, recs, tc.kind)
			}
			undo()
			if q, recs := quiet(); !q {
				t.Errorf("still not quiet once it ended: %+v", recs)
			}
		})
	}
}

// The handover flips a PR's owner row to the factory only while none of
// magnum's fix work is in flight on it, by compare-and-set against no row,
// in the request's own step. A PR with a loop at work does not flip: it
// hands over once the loop ends, and the answer says what holds it and
// your action. A row another writer added since is never overwritten, and
// a PR a round holds does not flip until the round ends.
func TestTheHandoverFlipsOnlyAQuietPRByCAS(t *testing.T) {
	storetest.Parallel(t)
	t.Run("quiet", func(t *testing.T) {
		h, _ := fixHarness(t, factoryFixOn)
		fixablePR(h, "zhuravel", nil)
		r := h.handOver(handOverPR2(false))
		if r.State != store.RequestDone || !strings.Contains(deref(r.Result), "the factory fixes talkable/talkable#2 from now on") {
			t.Fatalf("handover: %s %q", r.State, deref(r.Result))
		}
		if o, ok := ownerOf(h); !ok || !o.Factory() || !o.At.Equal(h.clock.Now()) || o.By != "magnum fix --handover" {
			t.Errorf("owner = %+v, %v; want the factory since now", o, ok)
		}
		if _, ok := h.fixEvent("fix.handed_over"); !ok {
			t.Error("no fix.handed_over event")
		}
		if _, ok := kvValue(h, KVPRFixHandover(h.pr(2).ID)); ok {
			t.Error("a handover still waits for a PR that flipped")
		}
	})
	t.Run("a loop at work", func(t *testing.T) {
		h, _ := fixHarness(t, realSleep, factoryFixOn)
		pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
		h.fix(FixPayload{})
		h.e.handleRequests(h.ctx) // the fixer works on
		h.awaitSubmit()
		st, _ := h.fixStateOf(2)
		run := h.fixRuns(pr.ID)[0]
		id := h.enqueue(ReqFixHandover, handOverPR2(false))
		h.e.handleRequests(h.ctx) // the fixer still works
		r := h.request(id)
		want := "talkable/talkable#2 hands over to the factory once this ends (magnum starts no new loop on it meanwhile): " +
			"a fix loop at work: its fixer runs cycle 1 of 3 (nothing: it ends at its cycle boundary, with no next cycle)"
		if r.State != store.RequestDone || !strings.HasPrefix(deref(r.Result), want) {
			t.Errorf("handover: %s %q, want %q", r.State, deref(r.Result), want)
		}
		if o, ok := ownerOf(h); ok {
			t.Errorf("a PR with a loop at work flipped: %+v", o)
		}
		if _, ok := kvValue(h, KVPRFixHandover(pr.ID)); !ok {
			t.Error("no handover waits for the PR")
		}
		// The fixer ends with nothing pushed: the loop ends, and the next
		// tick flips the PR.
		if err := os.MkdirAll(filepath.Dir(st.Report), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(st.Report, []byte(fixReport(run.ID, "nothing", fixReplied, "none")), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := h.st.TransitionRun(h.ctx, run.ID, []string{store.RunWorking}, store.RunEnded, nil); err != nil {
			t.Fatal(err)
		}
		h.settle()
		h.tick()
		if o, ok := ownerOf(h); !ok || !o.Factory() {
			t.Errorf("owner after the loop ended = %+v, %v; want the factory", o, ok)
		}
	})
	t.Run("a row another wrote", func(t *testing.T) {
		h, _ := fixHarness(t)
		pr := fixablePR(h, "zhuravel", nil)
		raw, _ := fixwork.FixOwner{Owner: fixwork.OwnerReturning, At: factoryOwnsAt, Flipped: factoryOwnsAt}.Value()
		if err := h.st.SetKV(h.ctx, fixwork.KVPRFixOwner(pr.ID), raw); err != nil {
			t.Fatal(err)
		}
		flipped, _, err := h.e.flipToFactory(h.ctx, h.heldRepo(), pr, handoverBy)
		if flipped || err == nil {
			t.Errorf("flip over another's row = %v, %v; want refused", flipped, err)
		}
		if v, _ := kvValue(h, fixwork.KVPRFixOwner(pr.ID)); v != raw {
			t.Errorf("the row = %s, want the other writer's %s", v, raw)
		}
	})
	t.Run("a round holds the PR", func(t *testing.T) {
		h, _ := fixHarness(t)
		pr := fixablePR(h, "zhuravel", nil)
		if !h.e.reserve(pr.ID, &roundHandle{cancel: func() {}}) {
			t.Fatal("the PR could not be reserved")
		}
		flipped, recs, err := h.e.flipToFactory(h.ctx, h.heldRepo(), pr, handoverBy)
		if flipped || err != nil || len(recs) != 1 || recs[0].Kind != FixRecReservation {
			t.Errorf("flip under a round = %v, %+v, %v; want none, held by the round", flipped, recs, err)
		}
		h.e.unreserve(pr.ID)
		if flipped, _, err := h.e.flipToFactory(h.ctx, h.heldRepo(), pr, handoverBy); !flipped || err != nil {
			t.Errorf("flip after the round = %v, %v", flipped, err)
		}
	})
}

// A loop whose PR hands over to the factory ends at its cycle boundary:
// its re-review, which finds more, starts no next cycle, the loop's row
// goes and fix.ended says the factory takes what is left. Once magnum's
// push left the registry, the tick flips the PR to the factory.
func TestTheDrainEndsALoopAtItsCycleBoundary(t *testing.T) {
	h, git := fixHarness(t, factoryFixOn)
	pr := loopToReReview(t, h, git, findings(map[string]int{"P2": 1}, "COMMENTED"), fixPushed2)
	r := h.handOver(handOverPR2(false))
	if r.State != store.RequestDone || !strings.Contains(deref(r.Result), "a fix loop waiting for the re-review of its push (cycle 1 of 3) "+
		"(nothing: it ends at its cycle boundary, with no next cycle)") {
		t.Fatalf("handover: %s %q", r.State, deref(r.Result))
	}
	reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"}) // the re-review finds a P2
	if st, ok := h.fixStateOf(2); ok {
		t.Fatalf("the loop goes on: %+v", st)
	}
	if n := len(h.fixRuns(pr.ID)); n != 1 {
		t.Errorf("fix runs = %d, want the first cycle's alone", n)
	}
	if ev, ok := h.fixEvent("fix.ended"); !ok || !strings.Contains(ev.Message, "ends at its cycle boundary: the PR hands over to the factory") {
		t.Errorf("fix.ended = %q, %v", ev.Message, ok)
	}
	if o, ok := ownerOf(h); ok {
		t.Errorf("the PR flipped while magnum's push waits for its CI: %+v", o)
	}
	h.advance(3 * time.Minute)
	h.tick()
	if o, ok := ownerOf(h); !ok || !o.Factory() {
		t.Errorf("owner = %+v, %v; want the factory once the fix work ended", o, ok)
	}
	if _, ok := kvValue(h, KVPRFixHandover(pr.ID)); ok {
		t.Error("the handover still waits")
	}
}

// failKVRead makes every read of the registry's key-value row key fail, as
// an I/O error would: the table is rebuilt to take a NULL value, which the
// row then holds and no read can scan into a string (Store.GetKV errs).
func failKVRead(h *harness, key string) {
	h.t.Helper()
	for _, q := range []string{
		`CREATE TABLE kv_nullable (key TEXT PRIMARY KEY, value TEXT, updated_at TEXT NOT NULL)`,
		`INSERT INTO kv_nullable SELECT key, value, updated_at FROM kv`,
		`DROP TABLE kv`,
		`ALTER TABLE kv_nullable RENAME TO kv`,
	} {
		if _, err := h.st.DB().ExecContext(h.ctx, q); err != nil {
			h.t.Fatal(err)
		}
	}
	if _, err := h.st.DB().ExecContext(h.ctx, `INSERT OR REPLACE INTO kv (key, value, updated_at) VALUES (?, NULL, '')`, key); err != nil {
		h.t.Fatal(err)
	}
	if _, _, err := h.st.GetKV(h.ctx, key); err == nil {
		h.t.Fatalf("%s still reads", key)
	}
}

// A loop's next cycle whose read of the handover rows fails ends as a
// failed cycle, with the PR's attention, as any next cycle that cannot
// start: no handover was read, so none is named, and the operator hears
// that findings are left.
func TestAFailedReadOfTheHandoverEndsTheLoopWithItsAttention(t *testing.T) {
	h, git := fixHarness(t)
	pr := loopToReReview(t, h, git, findings(map[string]int{"P2": 1}, "COMMENTED"), fixPushed2)
	failKVRead(h, KVFixHandoverAll)
	reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"}) // the re-review finds a P2: the next cycle would start
	if st, ok := h.fixStateOf(2); ok {
		t.Fatalf("the loop goes on: %+v", st)
	}
	if ev, ok := h.fixEvent("fix.ended"); ok {
		t.Errorf("fix.ended = %q: no handover was read", ev.Message)
	}
	ev, ok := h.fixEvent("fix.failed")
	if !ok || ev.Level != "warn" || !strings.Contains(ev.Message, "cycle 2 did not start") ||
		!strings.Contains(ev.Message, "whether it hands over to the factory cannot be read") {
		t.Errorf("fix.failed = %+v, %v", ev, ok)
	}
	if got := h.wantState(2, store.PRNeedsAttention); !strings.Contains(deref(got.LastError), "cycle 2 did not start") {
		t.Errorf("attention = %q", deref(got.LastError))
	}
	if n := len(h.fixRuns(pr.ID)); n != 1 {
		t.Errorf("fix runs = %d, want the first cycle's alone", n)
	}
}

// fakeLease is the factory's lease of PR #2 as the tests' reader gives it.
type fakeLease struct {
	mu    sync.Mutex
	lease FixLease
	err   error
}

func (f *fakeLease) set(l FixLease, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lease, f.err = l, err
}

// reader is Deps.FixLeaseOf over f, read at now.
func (f *fakeLease) reader(now func() time.Time) func(ctx context.Context, repo string, number int) (FixLease, error) {
	return func(_ context.Context, _ string, _ int) (FixLease, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		l := f.lease
		l.ReadAt = now()
		return l, f.err
	}
}

// Only a PR the factory has not worked since its import goes back to
// magnum: back refuses one it worked, one it fixes now, one whose lease
// cannot be read and every one under a daemon with no lease reader. A PR it
// may return goes to returning, where neither daemon starts a fixer, and
// the tick takes the row away while the lease still shows no work; a PR the
// factory worked before it saw the return stays the factory's, under its
// first flip.
func TestOnlyAPRTheFactoryHasNotWorkedReturnsToMagnum(t *testing.T) {
	storetest.Parallel(t)
	t.Run("no reader", func(t *testing.T) {
		h, _ := fixHarness(t)
		fixablePR(h, "zhuravel", nil)
		factoryOwns(h)
		r := h.handOver(handOverPR2(true))
		if r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "reads no factory lease") {
			t.Errorf("back with no reader: %s %q", r.State, deref(r.Result))
		}
		if o, _ := ownerOf(h); !o.Factory() {
			t.Errorf("owner = %+v, want the factory still", o)
		}
	})
	lease := &fakeLease{}
	h, _ := fixHarness(t, func(h *harness) { h.d.FixLeaseOf = lease.reader(h.clock.Now) }, factoryFixOn)
	fixablePR(h, "zhuravel", nil)
	if r := h.handOver(handOverPR2(false)); r.State != store.RequestDone {
		t.Fatalf("handover: %s %q", r.State, deref(r.Result))
	}
	flip, _ := ownerOf(h)
	for _, tc := range []struct {
		name  string
		lease FixLease
		err   error
		want  string
	}{
		{"worked", FixLease{Worked: true}, nil, "the factory worked it since its import"},
		{"fixing", FixLease{Fixing: true, Phase: FixPhaseFixing, Cycle: 1}, nil, "the factory fixes it now"},
		{"unread", FixLease{}, errors.New("database is locked"), "the factory's lease cannot be read (database is locked)"},
	} {
		lease.set(tc.lease, tc.err)
		r := h.handOver(handOverPR2(true))
		if r.State != store.RequestFailed || !strings.Contains(deref(r.Result), tc.want) {
			t.Errorf("%s: back = %s %q, want refused with %q", tc.name, r.State, deref(r.Result), tc.want)
		}
		if o, _ := ownerOf(h); o != flip {
			t.Errorf("%s: owner = %+v, want %+v", tc.name, o, flip)
		}
	}

	// Not worked: it returns, and the tick gives it back to magnum.
	lease.set(FixLease{}, nil)
	h.advance(time.Minute)
	if r := h.handOver(handOverPR2(true)); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "returns to magnum") {
		t.Fatalf("back = %s %q", r.State, deref(r.Result))
	}
	if o, _ := ownerOf(h); o.Owner != fixwork.OwnerReturning || !o.Flipped.Equal(flip.At) {
		t.Fatalf("owner = %+v, want returning from the flip at %v", o, flip.At)
	}
	if said := h.refusedFix(FixPayload{}); !strings.Contains(said, "is returning from the factory to magnum") {
		t.Errorf("a fix while it returns = %q", said)
	}
	h.tick()
	if o, ok := ownerOf(h); ok {
		t.Errorf("owner = %+v; want magnum's again (no row)", o)
	}
	if _, ok := h.fixEvent("fix.returned"); !ok {
		t.Error("no fix.returned event")
	}

	// Worked before it saw the return: it stays the factory's, under the
	// first flip, so the factory imports it no second time.
	if r := h.handOver(handOverPR2(false)); r.State != store.RequestDone {
		t.Fatalf("handover again: %s %q", r.State, deref(r.Result))
	}
	again, _ := ownerOf(h)
	h.advance(time.Minute)
	if r := h.handOver(handOverPR2(true)); r.State != store.RequestDone {
		t.Fatalf("back again: %s %q", r.State, deref(r.Result))
	}
	lease.set(FixLease{Worked: true}, nil)
	h.tick()
	if o, _ := ownerOf(h); !o.Factory() || !o.At.Equal(again.At) {
		t.Errorf("owner = %+v; want the factory under its flip at %v", o, again.At)
	}
	if ev, ok := h.fixEvent("fix.return_refused"); !ok || ev.Level != "warn" {
		t.Errorf("fix.return_refused = %+v, %v", ev, ok)
	}
}

// The morning list's drops end the records that keep a PR from handing
// over and do not end by themselves: `magnum fix --drop-push` drops the
// record of a fixer's push kept for its late settle, and `magnum
// merge-parent --drop` the merge of the parent held for your y, each by a
// compare with the row read, also on a PR the factory fixes since.
func TestTheMorningListsDropsEndAPushRecordAndAHeldMerge(t *testing.T) {
	storetest.Parallel(t)
	t.Run("push", func(t *testing.T) {
		h, _, pr := unconfirmedFix(t)
		factoryOwns(h)
		id := h.fix(FixPayload{DropPush: true, DryRun: true})
		h.handle()
		if res := h.wantDone(id); !strings.HasPrefix(res, "dry run: would drop the record of the fixer's push of b2b2b2b") {
			t.Errorf("dry run = %q", res)
		}
		id = h.fix(FixPayload{DropPush: true})
		h.handle()
		if res := h.wantDone(id); res != "dropped the record of the fixer's push of b2b2b2b on talkable/talkable#2: magnum settles it no more" {
			t.Errorf("drop = %q", res)
		}
		if _, ok := h.fixPushOf(2); ok {
			t.Error("the push record stayed")
		}
		if _, ok := h.fixEvent("fix.push_dropped"); !ok {
			t.Error("no fix.push_dropped event")
		}
		if q, recs, err := h.e.fixQuiet(h.ctx, h.heldRepo(), pr); err != nil || !q {
			t.Errorf("quiet = %v, %+v, %v; want quiet once the record went", q, recs, err)
		}
	})
	t.Run("held merge", func(t *testing.T) {
		h, mg := holdMerged(t)
		id := h.fix(FixPayload{Mode: FixModeMerge, Drop: true})
		h.handle()
		if res := h.wantDone(id); !strings.HasPrefix(res, "dropped the held merge d3d3d3d of master at c0c0c0c on talkable/talkable#2") {
			t.Errorf("drop = %q", res)
		}
		wantDropped(t, h, mg, "dropped by magnum merge-parent --drop")
		wantNoPush(t, mg)
	})
	t.Run("alone", func(t *testing.T) {
		h, _ := fixHarness(t)
		fixablePR(h, "zhuravel", nil)
		for _, p := range []FixPayload{{DropPush: true, Off: true}, {Drop: true}, {Drop: true, Mode: FixModeMerge, Asked: &MergeAsked{}}} {
			if said := h.refusedFix(p); !strings.Contains(said, "goes alone") {
				t.Errorf("%+v = %q", p, said)
			}
		}
	})
}

// A fix held for a restart, a pause or the GitHub budget does not keep its
// PR from handing over (plan section 3: one left at the switch is imported
// as a queued request): the PR flips at once, its held fix's row stays in
// magnum's registry for the factory's import, and from then on nothing of
// magnum's starts it or drops it: not the restart, a drain's end, a pause's
// or the budget's, nor `magnum abort`.
func TestAHeldFixDoesNotBlockTheFlipAndStaysForTheImport(t *testing.T) {
	h, _ := fixHarness(t, factoryFixOn)
	pr := heldFixDuringADrain(t, h)
	held, _ := kvValue(h, KVPRFixHeld(pr.ID))
	if q, recs, err := h.e.fixQuiet(h.ctx, h.heldRepo(), pr); err != nil || !q {
		t.Fatalf("quiet = %v, %+v, %v; want a held fix to hold nothing back", q, recs, err)
	}
	if r := h.handOver(handOverPR2(false)); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "the factory fixes talkable/talkable#2 from now on") {
		t.Fatalf("handover: %s %q", r.State, deref(r.Result))
	}
	h.e.delKV(h.ctx, KVDaemonDraining)
	h.e.startHeldFixes(h.ctx)
	h.e.continueDrainHeldLoops(h.ctx)
	h.e.holdForPause(pr.ID)
	h.e.continuePauseHeld(h.ctx)
	h.e.holdForBudget(pr.ID)
	h.e.continueBudgetHeld(h.ctx)
	abort := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	h.tick()
	if r := h.request(abort); r.State != store.RequestFailed {
		t.Errorf("magnum abort: %s %q, want nothing to stop", r.State, deref(r.Result))
	}
	if now, ok := kvValue(h, KVPRFixHeld(pr.ID)); !ok || now != held {
		t.Errorf("the held fix = %q, %v; want it kept as %s for the factory's import", now, ok, held)
	}
	if _, ok := h.fixEvent("fix.dropped"); ok {
		t.Error("the held fix was dropped")
	}
	if st, ok := h.fixStateOf(2); ok || len(h.fixRuns(pr.ID)) != 0 {
		t.Errorf("a fix loop started: %+v, runs %d", st, len(h.fixRuns(pr.ID)))
	}
	if o, ok := ownerOf(h); !ok || !o.Factory() {
		t.Errorf("owner = %+v, %v; want the factory", o, ok)
	}
}
