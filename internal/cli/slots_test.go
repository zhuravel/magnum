package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/zhuravel/magnum/internal/cleanup"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
)

type fakeSlotOps struct {
	st          *store.Store
	provisioned []int
	next        int
	provErr     error
	repaired    []string
	adopted     []string
	pinned      []string
	unpinned    []string
}

func (f *fakeSlotOps) ProvisionPool(ctx context.Context, pool config.Pool, n int) error {
	if f.provErr != nil {
		return f.provErr
	}
	f.provisioned = append(f.provisioned, n)
	return nil
}

func (f *fakeSlotOps) NextSlotNumber(ctx context.Context, pool config.Pool) (int, error) {
	f.next++
	return f.next, nil
}

func (f *fakeSlotOps) Repair(ctx context.Context, sl store.Slot, pool config.Pool) error {
	f.repaired = append(f.repaired, sl.Name+"@"+pool.Repo)
	return nil
}

func (f *fakeSlotOps) Adopt(ctx context.Context, pool config.Pool, path string) (store.Slot, error) {
	f.adopted = append(f.adopted, path)
	return store.Slot{Name: "review4", Path: path, State: store.SlotFree}, nil
}

func (f *fakeSlotOps) Pin(ctx context.Context, sl store.Slot) error {
	f.pinned = append(f.pinned, sl.Name)
	return nil
}

func (f *fakeSlotOps) Unpin(ctx context.Context, sl store.Slot) error {
	f.unpinned = append(f.unpinned, sl.Name)
	return nil
}

func slotsFixture(t *testing.T) (*inspFixture, *slotsEnv, *fakeSlotOps, *fakePlanner) {
	t.Helper()
	f := newInspFixture(t)
	st := f.store()
	if err := f.Ctx.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	ops := &fakeSlotOps{st: st}
	fp := &fakePlanner{plan: cleanup.Plan{Actions: []cleanup.Action{{Kind: cleanup.KindRemoveSlot, Subject: "slot:review2", Slot: "review2"}}},
		report: cleanup.Report{Done: 1}}
	return f, &slotsEnv{c: f.Ctx, st: st, cfg: f.Ctx.Config, ops: ops, cleaner: fp}, ops, fp
}

func inspSeedSlot(t *testing.T, st *store.Store, home, name, state string, prID *int64) store.Slot {
	t.Helper()
	sl, err := st.CreateSlot(context.Background(), store.Slot{Name: name, Kind: store.SlotKindPool, Path: home + "/talkable." + name,
		MainClone: home + "/talkable", RepoFullName: "talkable/talkable", State: state, PRID: prID})
	if err != nil {
		t.Fatal(err)
	}
	return sl
}

func TestSlotsList(t *testing.T) {
	f, e, _, _ := slotsFixture(t)
	_, pr := inspSeedPR(t, e.st, "talkable/talkable", 11920, store.PRReviewed, nil)
	inspSeedSlot(t, e.st, f.Home, "review1", store.SlotHeld, &pr.ID)
	inspSeedSlot(t, e.st, f.Home, "review2", store.SlotFree, nil)
	gone := inspSeedSlot(t, e.st, f.Home, "review3", store.SlotRemoved, nil)
	_ = gone
	if code := e.list(context.Background(), false, false); code != 0 {
		t.Fatalf("code %d", code)
	}
	out := f.Out.String()
	for _, want := range []string{"SLOT", "review1", "talkable#11920", "held", "review2", "free"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "review3") {
		t.Errorf("removed slot listed without --all:\n%s", out)
	}
	f.Out.Reset()
	if code := e.list(context.Background(), true, true); code != 0 {
		t.Fatal("json list failed")
	}
	var rows []slotsRow
	if err := json.Unmarshal(f.Out.Bytes(), &rows); err != nil || len(rows) != 3 || rows[0].PR != "talkable/talkable#11920" {
		t.Fatalf("json rows = %+v err %v\n%s", rows, err, f.Out.String())
	}
}

func TestSlotsProvision(t *testing.T) {
	f, e, ops, _ := slotsFixture(t)
	inspSeedSlot(t, e.st, f.Home, "review3", store.SlotProvisioning, nil)
	pool, _ := e.pool("")
	// min is 2 and no slot is live yet: resume review3, then one new slot.
	if code := e.provision(context.Background(), pool, -1); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	if fmt.Sprint(ops.provisioned) != "[3 1]" {
		t.Fatalf("provisioned %v", ops.provisioned)
	}
	if !strings.Contains(f.Out.String(), "provision-review3.log") || !strings.Contains(f.Out.String(), "review1 is ready") {
		t.Fatalf("out:\n%s", f.Out.String())
	}
	// max is 3.
	f.Out.Reset()
	if code := e.provision(context.Background(), pool, 5); code != 1 || !strings.Contains(f.Err.String(), "at most 3") {
		t.Fatalf("over max: code %d err %s", code, f.Err.String())
	}
}

func TestSlotsProvisionErrors(t *testing.T) {
	f, e, ops, _ := slotsFixture(t)
	pool, _ := e.pool("talkable/talkable")
	ops.provErr = fmt.Errorf("provision: %w", slots.ErrLowDisk)
	if code := e.provision(context.Background(), pool, 1); code != 1 || !strings.Contains(f.Err.String(), "magnum cleanup") {
		t.Fatalf("low disk: code %d err %s", code, f.Err.String())
	}
	ops.provErr = nil
	unlock, held, err := engine.AcquireLock(f.Ctx.Layout.Lock())
	if err != nil || held {
		t.Fatal("lock")
	}
	defer unlock()
	f.Err.Reset()
	// The lock is held (a daemon runs): the work is handed to it.
	if code := e.provision(context.Background(), pool, 1); code != 0 || !strings.Contains(f.Out.String(), "queued as request") {
		t.Fatalf("daemon running: code %d out %s err %s", code, f.Out.String(), f.Err.String())
	}
	if len(ops.provisioned) != 0 {
		t.Fatal("must not provision without the lock")
	}
	req, err := e.st.NextPendingRequest(context.Background())
	if err != nil || req.Kind != engine.ReqProvision {
		t.Fatalf("request %+v err %v", req, err)
	}
	var p engine.ProvisionPayload
	if err := json.Unmarshal(req.Payload, &p); err != nil || p.Pool != "talkable/talkable" || p.Count != 1 {
		t.Fatalf("payload %s (%v)", req.Payload, err)
	}
	if _, err := e.pool("nobody/else"); err == nil {
		t.Fatal("unknown repo must fail")
	}
}

func TestSlotsRepairAdopt(t *testing.T) {
	f, e, ops, _ := slotsFixture(t)
	inspSeedSlot(t, e.st, f.Home, "review2", store.SlotBroken, nil)
	if code := e.repair(context.Background(), "review2"); code != 0 || fmt.Sprint(ops.repaired) != "[review2@talkable/talkable]" {
		t.Fatalf("repair code %d ops %v err %s", code, ops.repaired, f.Err.String())
	}
	if code := e.repair(context.Background(), "review9"); code != 1 {
		t.Fatalf("unknown slot repair code %d", code)
	}
	if code := e.adopt(context.Background(), f.Home+"/talkable.review4"); code != 0 || len(ops.adopted) != 1 {
		t.Fatalf("adopt code %d err %s", code, f.Err.String())
	}
	if code := e.adopt(context.Background(), f.Home+"/elsewhere"); code != 1 || !strings.Contains(f.Err.String(), "slot_path") {
		t.Fatalf("adopt foreign path code %d err %s", code, f.Err.String())
	}
}

func TestSlotsRepairAdoptViaDaemon(t *testing.T) {
	f, e, ops, _ := slotsFixture(t)
	inspSeedSlot(t, e.st, f.Home, "review2", store.SlotBroken, nil)
	unlock, held, err := engine.AcquireLock(f.Ctx.Layout.Lock())
	if err != nil || held {
		t.Fatal("lock")
	}
	defer unlock()
	ctx := context.Background()
	if code := e.repair(ctx, "review2"); code != 0 || len(ops.repaired) != 0 {
		t.Fatalf("repair via daemon: code %d ops %v err %s", code, ops.repaired, f.Err.String())
	}
	if code := e.adopt(ctx, f.Home+"/talkable.review4"); code != 0 || len(ops.adopted) != 0 {
		t.Fatalf("adopt via daemon: code %d ops %v err %s", code, ops.adopted, f.Err.String())
	}
	reqs, err := e.st.PendingRequests(ctx, 0)
	if err != nil || len(reqs) != 2 || reqs[0].Kind != engine.ReqRepair || reqs[1].Kind != engine.ReqAdopt {
		t.Fatalf("requests %+v err %v", reqs, err)
	}
	var rp engine.RepairPayload
	var ap engine.AdoptPayload
	if json.Unmarshal(reqs[0].Payload, &rp) != nil || rp.Slot != "review2" {
		t.Fatalf("repair payload %s", reqs[0].Payload)
	}
	if json.Unmarshal(reqs[1].Payload, &ap) != nil || ap.Pool != "talkable/talkable" || ap.Path != f.Home+"/talkable.review4" {
		t.Fatalf("adopt payload %s", reqs[1].Payload)
	}
	// Validation still happens locally.
	if code := e.repair(ctx, "review9"); code != 1 {
		t.Fatalf("unknown slot repair code %d", code)
	}
}

func TestSlotsPinInProcessAndViaDaemon(t *testing.T) {
	f, e, ops, _ := slotsFixture(t)
	_, pr := inspSeedPR(t, e.st, "talkable/talkable", 11920, store.PRReviewed, nil)
	inspSeedSlot(t, e.st, f.Home, "review1", store.SlotHeld, &pr.ID)
	ctx := context.Background()
	if code := e.pin(ctx, "review1", true); code != 0 || fmt.Sprint(ops.pinned) != "[review1]" {
		t.Fatalf("pin code %d ops %v err %s", code, ops.pinned, f.Err.String())
	}
	got, _ := e.st.PRByID(ctx, pr.ID)
	if !got.Pinned {
		t.Fatal("the slot's PR must be pinned too")
	}
	unlock, _, _ := engine.AcquireLock(f.Ctx.Layout.Lock())
	defer unlock()
	if code := e.pin(ctx, "review1", false); code != 0 || len(ops.unpinned) != 0 {
		t.Fatalf("unpin via daemon code %d ops %v", code, ops.unpinned)
	}
	req, err := e.st.NextPendingRequest(ctx)
	if err != nil || req.Kind != engine.ReqUnpin || !strings.Contains(string(req.Payload), `"slot":"review1"`) {
		t.Fatalf("request %+v err %v", req, err)
	}
	if code := e.pin(ctx, "nope", true); code != 1 {
		t.Fatalf("unknown slot pin code %d", code)
	}
}

func TestSlotsRemoveUsesCleanup(t *testing.T) {
	f, e, _, fp := slotsFixture(t)
	rf := &cleanupFlags{cmd: "slots remove", remove: true}
	fs := pflag.NewFlagSet("slots remove", pflag.ContinueOnError)
	slotsRemoveFlags(fs, rf)
	if err := fs.Parse([]string{"review2", "--force", "--yes"}); err != nil || fs.NArg() != 1 {
		t.Fatalf("parse: %v %v", err, fs.Args())
	}
	if code := e.remove(context.Background(), rf, fs.Arg(0)); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	if fp.gotOpts.Slot != "review2" || !fp.gotOpts.Remove || !fp.gotOpts.Force || !fp.applied {
		t.Fatalf("opts %+v applied %v", fp.gotOpts, fp.applied)
	}
	if code := f.run("slots", "remove"); code != 2 || !strings.Contains(f.Err.String(), "remove needs a slot name") {
		t.Fatalf("missing slot code %d: %s", code, f.Err.String())
	}
}

func TestSlotsCommandDispatch(t *testing.T) {
	f := newInspFixture(t)
	if code := f.run("slots", "bogus"); code != 2 || !strings.Contains(f.Err.String(), "unknown slots subcommand") {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	if code := f.run("slots"); code != 0 || !strings.Contains(f.Out.String(), "no slots") {
		t.Fatalf("bare slots: code %d out %s err %s", code, f.Out.String(), f.Err.String())
	}
	if code := f.run("slots", "pin"); code != 2 {
		t.Fatalf("pin without slot: code %d", code)
	}
}
