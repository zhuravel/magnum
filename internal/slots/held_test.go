package slots

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/steps"
	"github.com/zhuravel/magnum/internal/store"
)

const checkHold = "merge-check"

func TestHoldFreeNeverTakesAPinnedHeldOrPRSlot(t *testing.T) {
	h := newHarness(t)
	s1, s2, s3 := h.provisioned(1), h.provisioned(2), h.provisioned(3)
	if err := h.m.Pin(h.ctx, s1); err != nil {
		t.Fatal(err)
	}
	if err := h.st.UpdateSlotFields(h.ctx, s2.ID, func(u *store.SlotUpdate) { u.Set("hold_reason", HoldDirtyWorktree) }); err != nil {
		t.Fatal(err)
	}
	pr := h.pr(h.repo().ID, 7, h.shaPR7, store.PRQueued)
	if got, err := h.m.Claim(h.ctx, pr, h.pool); err != nil || got.ID != s3.ID {
		t.Fatalf("Claim = %s, %v; want %s", got.Name, err, s3.Name)
	}

	if _, err := h.m.HoldFree(h.ctx, h.pool, "", checkHold); !errors.Is(err, ErrNoFreeSlot) {
		t.Fatalf("HoldFree with no free slot = %v, want ErrNoFreeSlot", err)
	}
	_, err := h.m.HoldFree(h.ctx, h.pool, s1.Name, checkHold)
	wantHold(t, err, HoldPinned)
	_, err = h.m.HoldFree(h.ctx, h.pool, s2.Name, checkHold)
	wantHold(t, err, HoldDirtyWorktree)
	if _, err := h.m.HoldFree(h.ctx, h.pool, s3.Name, checkHold); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("HoldFree(%s holding PR #7) = %v, want ErrConflict", s3.Name, err)
	}

	if got := h.slot(s1.Name); got.State != store.SlotFree || !got.Pinned || got.HoldReason != nil {
		t.Fatalf("%s = %s pinned=%v hold=%v", got.Name, got.State, got.Pinned, store.Deref(got.HoldReason))
	}
	if got := h.slot(s2.Name); got.State != store.SlotFree || store.Deref(got.HoldReason) != HoldDirtyWorktree {
		t.Fatalf("%s = %s hold=%q", got.Name, got.State, store.Deref(got.HoldReason))
	}
	if got := h.slot(s3.Name); got.State != store.SlotClaimed || got.PRID == nil || *got.PRID != pr.ID || got.HoldReason != nil {
		t.Fatalf("%s = %s pr=%v hold=%q", got.Name, got.State, got.PRID, store.Deref(got.HoldReason))
	}
}

func TestHoldFreeHoldsASlotTheDaemonLeavesAlone(t *testing.T) {
	h := newHarness(t)
	h.provisioned(1)
	sl, err := h.m.HoldFree(h.ctx, h.pool, "", checkHold)
	if err != nil {
		t.Fatal(err)
	}
	got := h.slot(sl.Name)
	if got.State != store.SlotHeld || store.Deref(got.HoldReason) != checkHold || got.PRID != nil {
		t.Fatalf("held slot = %s hold=%q pr=%v", got.State, store.Deref(got.HoldReason), got.PRID)
	}
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(h.now) {
		t.Fatalf("last_used_at = %v, want %v", got.LastUsedAt, h.now)
	}

	// No round claims it, no eviction takes it, no release touches it.
	if free, err := h.st.FreeSlots(h.ctx, h.pool.Repo, 0); err != nil || len(free) != 0 {
		t.Fatalf("FreeSlots = %d, %v", len(free), err)
	}
	if ev, err := h.st.EvictableSlots(h.ctx, h.pool.Repo, h.now.Add(48*time.Hour), 0); err != nil || len(ev) != 0 {
		t.Fatalf("EvictableSlots = %d, %v", len(ev), err)
	}
	pr := h.pr(h.repo().ID, 8, h.shaPR8, store.PRQueued)
	if _, err := h.m.Claim(h.ctx, pr, h.pool); !errors.Is(err, ErrNoFreeSlot) {
		t.Fatalf("a round's Claim = %v, want ErrNoFreeSlot", err)
	}
	wantHold(t, h.m.Release(h.ctx, got, h.pool, "evicted"), checkHold)
	_, err = h.m.HoldFree(h.ctx, h.pool, sl.Name, checkHold)
	wantHold(t, err, checkHold)
	if got := h.slot(sl.Name); got.State != store.SlotHeld || store.Deref(got.HoldReason) != checkHold {
		t.Fatalf("after the refusals: %s hold=%q", got.State, store.Deref(got.HoldReason))
	}
}

func TestHoldFreeHandsBackASlotAPersonIsIn(t *testing.T) {
	h := newHarness(t)
	s1, s2 := h.provisioned(1), h.provisioned(2)
	h.snap = herdr.Snapshot{Agents: []herdr.AgentInfo{{PaneID: "w1:p1", WorkspaceID: "w1", Agent: "claude", Name: "alice",
		AgentStatus: herdr.StatusIdle, Cwd: s1.Path}}}

	sl, err := h.m.HoldFree(h.ctx, h.pool, "", checkHold)
	if err != nil || sl.ID != s2.ID {
		t.Fatalf("HoldFree = %s, %v; want %s (a person is in %s)", sl.Name, err, s2.Name, s1.Name)
	}
	if got := h.slot(s1.Name); got.State != store.SlotFree || got.HoldReason != nil {
		t.Fatalf("%s was not handed back: %s hold=%q", s1.Name, got.State, store.Deref(got.HoldReason))
	}
	_, err = h.m.HoldFree(h.ctx, h.pool, s1.Name, checkHold)
	wantHold(t, err, HoldForeignAgent)
	if got := h.slot(s1.Name); got.State != store.SlotFree || got.HoldReason != nil {
		t.Fatalf("%s after a named refusal: %s hold=%q", s1.Name, got.State, store.Deref(got.HoldReason))
	}
}

// heldCheckout holds slot 1 for a check and checks PR 7's head out in it,
// fetched into the check's ref.
func (h *harness) heldCheckout() (store.Slot, string) {
	h.t.Helper()
	h.provisioned(1)
	sl, err := h.m.HoldFree(h.ctx, h.pool, "", checkHold)
	if err != nil {
		h.t.Fatal(err)
	}
	ref := gitx.MergeCheckRefPrefix + sl.Name + "/head"
	sha, err := h.m.git.FetchInto(h.ctx, h.main, "refs/pull/7/head", ref)
	if err != nil || sha != h.shaPR7 {
		h.t.Fatalf("FetchInto = %s, %v", sha, err)
	}
	if err := h.m.CheckoutHeld(h.ctx, sl, h.pool, checkHold, sha); err != nil {
		h.t.Fatalf("CheckoutHeld: %v", err)
	}
	return h.slot(sl.Name), ref
}

func TestCheckoutHeldUsesTheCheckoutStepsAndKeepsTheHold(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.heldCheckout()
	if sl.State != store.SlotHeld || store.Deref(sl.HoldReason) != checkHold {
		t.Fatalf("slot = %s hold=%q", sl.State, store.Deref(sl.HoldReason))
	}
	if head := gitT(t, sl.Path, "rev-parse", "HEAD"); head != h.shaPR7 || store.Deref(sl.CheckedOutSHA) != h.shaPR7 {
		t.Fatalf("HEAD = %s, checked_out_sha = %s; want %s", head, store.Deref(sl.CheckedOutSHA), h.shaPR7)
	}
	if !sl.DirtySchema {
		t.Fatal("PR 7 changes db/schema.rb: dirty_schema is not set")
	}
	if n := len(h.scriptCalls(h.pool.PostCheckout[0])); n == 0 {
		t.Fatal("PR 7 changes Gemfile.lock: post_checkout did not run")
	}
	// The PR's ref the daemon uses is untouched.
	if _, err := h.m.git.RevParse(h.ctx, h.main, gitx.PRRef(7)); err == nil {
		t.Fatalf("%s was written", gitx.PRRef(7))
	}
	// A slot not held for the check is refused.
	if err := h.m.CheckoutHeld(h.ctx, sl, h.pool, "other", h.shaPR7); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("CheckoutHeld for another hold = %v", err)
	}
}

func TestReleaseHeldStaysHeldUntilTheSlotIsFree(t *testing.T) {
	h := newHarness(t)
	sl, ref := h.heldCheckout()
	var seen []string
	ctx := steps.WithFailpoint(h.ctx, func(subject, name string, at steps.Point) error {
		if at == steps.BeforeRun {
			cur := h.slot(sl.Name)
			seen = append(seen, name+"="+cur.State+"/"+store.Deref(cur.HoldReason))
		}
		return nil
	})
	if err := h.m.ReleaseHeld(ctx, sl, h.pool, checkHold, ref); err != nil {
		t.Fatalf("ReleaseHeld: %v", err)
	}
	want := []string{"guard", "fetch_base", "reset", "delete_ref", "render_mise", "deps", "schema_check", "mark_free"}
	if len(seen) != len(want) {
		t.Fatalf("steps = %v, want %v", seen, want)
	}
	for i, s := range seen {
		if s != want[i]+"="+store.SlotHeld+"/"+checkHold {
			t.Fatalf("step %d: %s, want %s held for %s (never releasing or dirty_schema, which the daemon resumes)", i, s, want[i], checkHold)
		}
	}
	got := h.slot(sl.Name)
	if got.State != store.SlotFree || got.HoldReason != nil || got.CheckedOutSHA != nil || got.DirtySchema || got.LastError != nil {
		t.Fatalf("released slot = %s hold=%q sha=%v dirty=%v err=%q", got.State, store.Deref(got.HoldReason),
			got.CheckedOutSHA, got.DirtySchema, store.Deref(got.LastError))
	}
	if head := gitT(t, sl.Path, "rev-parse", "HEAD"); head != h.shaMaster {
		t.Fatalf("HEAD = %s, want origin/master %s", head, h.shaMaster)
	}
	if b := gitT(t, sl.Path, "symbolic-ref", "--short", "HEAD"); b != sl.Name {
		t.Fatalf("branch = %s, want the placeholder %s", b, sl.Name)
	}
	if _, err := h.m.git.RevParse(h.ctx, h.main, ref); !errors.Is(err, gitx.ErrNoSuchRef) {
		t.Fatalf("%s after the release: %v", ref, err)
	}
	if !stepOK(t, h, "slot:"+sl.Name+":release", "mark_free") {
		t.Fatal("no ok row for mark_free")
	}
	// Free again: a round can claim it.
	pr := h.pr(h.repo().ID, 8, h.shaPR8, store.PRQueued)
	if _, err := h.m.Claim(h.ctx, pr, h.pool); err != nil {
		t.Fatalf("Claim after the release: %v", err)
	}
}

func TestReleaseHeldThatFailsLeavesTheSlotHeldForTheCheck(t *testing.T) {
	h := newHarness(t)
	sl, ref := h.heldCheckout()
	crash := errors.New("crash")
	ctx := steps.WithFailpoint(h.ctx, func(subject, name string, at steps.Point) error {
		if name == "deps" && at == steps.BeforeRun {
			return crash
		}
		return nil
	})
	if err := h.m.ReleaseHeld(ctx, sl, h.pool, checkHold, ref); !errors.Is(err, crash) {
		t.Fatalf("err = %v, want crash", err)
	}
	if got := h.slot(sl.Name); got.State != store.SlotHeld || store.Deref(got.HoldReason) != checkHold {
		t.Fatalf("after the failure: %s hold=%q", got.State, store.Deref(got.HoldReason))
	}
	if err := h.m.ReleaseHeld(h.ctx, h.slot(sl.Name), h.pool, checkHold, ref); err != nil {
		t.Fatalf("second ReleaseHeld: %v", err)
	}
	if got := h.slot(sl.Name); got.State != store.SlotFree || got.HoldReason != nil {
		t.Fatalf("after the second release: %s hold=%q", got.State, store.Deref(got.HoldReason))
	}
}

func TestReleaseHeldRefusesARefOutsideItsNamespace(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.heldCheckout()
	if err := h.m.ReleaseHeld(h.ctx, sl, h.pool, checkHold, gitx.PRRef(7)); err == nil || !strings.Contains(err.Error(), "not under") {
		t.Fatalf("ReleaseHeld(%s) = %v", gitx.PRRef(7), err)
	}
	if got := h.slot(sl.Name); got.State != store.SlotHeld {
		t.Fatalf("state = %s", got.State)
	}
}

func TestReleaseHeldSchemaReloadFailingTwiceBreaksTheSlotWithoutTheHold(t *testing.T) {
	h := newHarness(t)
	off := false
	h.pool.ResetDBOnSchemaChange = &off // the release reloads a dirty schema
	sl, ref := h.heldCheckout()
	h.mu.Lock()
	h.failScript[h.pool.ResetDB[0]] = 2
	h.mu.Unlock()
	if err := h.m.ReleaseHeld(h.ctx, sl, h.pool, checkHold, ref); !errors.Is(err, ErrBroken) {
		t.Fatalf("err = %v, want ErrBroken", err)
	}
	got := h.slot(sl.Name)
	if got.State != store.SlotBroken || got.HoldReason != nil {
		t.Fatalf("slot = %s hold=%q; want broken without a hold (`magnum slots repair` takes it)", got.State, store.Deref(got.HoldReason))
	}
}

// stepOK reports whether subject has an ok row for step name.
func stepOK(t *testing.T, h *harness, subject, name string) bool {
	t.Helper()
	evs, err := h.st.EventsBySubject(context.Background(), subject, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Kind == store.KindStep && store.Deref(e.Step) == name && store.Deref(e.Phase) == store.PhaseOK {
			return true
		}
	}
	return false
}
