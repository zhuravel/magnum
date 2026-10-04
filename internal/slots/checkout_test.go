package slots

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/steps"
	"github.com/zhuravel/magnum/internal/store"
)

func TestClaim(t *testing.T) {
	h := newHarness(t)
	repo := h.repo()
	pr := h.pr(repo.ID, 7, h.shaPR7, store.PRQueued)
	if _, err := h.m.Claim(h.ctx, pr, h.pool); !errors.Is(err, ErrNoFreeSlot) {
		t.Fatalf("Claim with no slots: err = %v, want ErrNoFreeSlot", err)
	}
	h.provisioned(1)
	h.provisioned(2)
	sl, err := h.m.Claim(h.ctx, pr, h.pool)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if sl.State != store.SlotClaimed || store.Deref(sl.PRID) != pr.ID || sl.LastUsedAt == nil {
		t.Fatalf("claimed slot = %+v", sl)
	}
	got, _ := h.st.PRByID(h.ctx, pr.ID)
	if got.State != store.PRClaiming {
		t.Fatalf("pr state = %s", got.State)
	}
	a, err := h.st.OpenAssignmentBySlot(h.ctx, sl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.PRID != pr.ID || !slices.Equal(a.DBNames, h.pool.DBNames(sl.Name)) || a.Path != sl.Path {
		t.Fatalf("assignment = %+v", a)
	}
	// A PR that is not claimable is refused without trying every slot.
	if _, err := h.m.Claim(h.ctx, got, h.pool); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second Claim: err = %v, want ErrConflict", err)
	}
	// Second PR gets the other slot; a third finds none.
	pr8 := h.pr(repo.ID, 8, h.shaPR8, store.PRQueued)
	sl8, err := h.m.Claim(h.ctx, pr8, h.pool)
	if err != nil || sl8.ID == sl.ID {
		t.Fatalf("Claim pr8 = %+v, %v", sl8, err)
	}
	pr9 := h.pr(repo.ID, 9, h.shaPR8, store.PRQueued)
	if _, err := h.m.Claim(h.ctx, pr9, h.pool); !errors.Is(err, ErrNoFreeSlot) {
		t.Fatalf("Claim pr9: err = %v", err)
	}
}

func TestCheckout(t *testing.T) {
	h := newHarness(t)
	h.provisioned(1)
	pr := h.pr(h.repo().ID, 7, h.shaPR7, store.PRQueued)
	sl, err := h.m.Claim(h.ctx, pr, h.pool)
	if err != nil {
		t.Fatal(err)
	}
	provisionedLock := store.Deref(sl.LockSHA)
	h.run.reset()
	h.clearScripts()
	h.now = t0.Add(time.Hour)
	if err := h.m.Checkout(h.ctx, sl, pr, h.pool, h.shaPR7); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	got := h.slot(sl.Name)
	if got.State != store.SlotClaimed {
		t.Fatalf("state = %s, want claimed (busy is the engine's job)", got.State)
	}
	if store.Deref(got.CheckedOutSHA) != h.shaPR7 || !got.DirtySchema || got.LastError != nil {
		t.Fatalf("slot = sha %s dirty %v err %v", store.Deref(got.CheckedOutSHA), got.DirtySchema, store.Deref(got.LastError))
	}
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(h.now) {
		t.Fatalf("last_used_at = %v", got.LastUsedAt)
	}
	if head := gitT(t, sl.Path, "rev-parse", "HEAD"); head != h.shaPR7 {
		t.Fatalf("HEAD = %s", head)
	}
	if b := gitT(t, sl.Path, "branch", "--show-current"); b != "" {
		t.Fatalf("not detached: %q", b)
	}
	if ref := gitT(t, h.main, "rev-parse", gitx.PRRef(7)); ref != h.shaPR7 {
		t.Fatalf("PR ref = %s", ref)
	}
	fetch := h.run.gitCalls("fetch")
	if len(fetch) < 1 || !fetch[0].Mutates {
		t.Fatalf("fetch calls = %v", fetch)
	}
	sw := h.run.gitCalls("switch", "--quiet", "--discard-changes", "--detach", gitx.PRRef(7))
	if len(sw) != 1 || !sw[0].Mutates {
		t.Fatalf("switch calls = %v", sw)
	}
	// Gemfile.lock changed → post_checkout through mise exec (30m, Mutates) and lock_sha updated.
	deps := h.scriptCalls(h.pool.PostCheckout[0])
	if len(deps) != 1 {
		t.Fatalf("deps calls = %d", len(deps))
	}
	if c := deps[0].Cmd; c.Timeout != DepsTimeout || DepsTimeout != 30*time.Minute || !c.Mutates || deps[0].Env["WT_BRANCH"] != "review1" {
		t.Fatalf("deps cmd = %+v", c)
	}
	if got.LockSHA == nil || *got.LockSHA == provisionedLock {
		t.Fatalf("lock_sha not updated: %v (provisioned %s)", store.Deref(got.LockSHA), provisionedLock)
	}
	if !strings.Contains(readFile(t, filepath.Join(sl.Path, ".mise.local.toml")), `WT_BRANCH = "review1"`) {
		t.Fatal("mise not rendered")
	}
	// No reset_db during checkout.
	if n := len(h.scriptCalls(h.pool.ResetDB[0])); n != 0 {
		t.Fatalf("schema reset during checkout")
	}

	// Checking out the same head again (crash before the engine moved on) is safe.
	if err := h.m.Checkout(h.ctx, got, pr, h.pool, h.shaPR7); err != nil {
		t.Fatalf("repeat Checkout: %v", err)
	}
	if n := len(h.scriptCalls(h.pool.PostCheckout[0])); n != 1 {
		t.Fatalf("deps re-ran with an unchanged lock (%d)", n)
	}
}

func TestCheckoutWithoutSchemaOrLockChanges(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.claimedCheckout(8, h.shaPR8)
	if sl.DirtySchema {
		t.Fatal("dirty_schema set for a README-only PR")
	}
	if n := len(h.scriptCalls(h.pool.PostCheckout[0])); n != 0 {
		t.Fatalf("deps ran %d times with unchanged lockfiles", n)
	}
}

func TestCheckoutUsesFetchedHeadWhenTargetMoved(t *testing.T) {
	h := newHarness(t)
	h.provisioned(1)
	stale := strings.Repeat("a", 40)
	pr := h.pr(h.repo().ID, 8, stale, store.PRQueued)
	sl, err := h.m.Claim(h.ctx, pr, h.pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.m.Checkout(h.ctx, sl, pr, h.pool, stale); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if got := h.slot(sl.Name); store.Deref(got.CheckedOutSHA) != h.shaPR8 {
		t.Fatalf("checked_out_sha = %s, want fetched %s", store.Deref(got.CheckedOutSHA), h.shaPR8)
	}
	evs, err := h.st.EventsBySubject(h.ctx, "slot:review1:pr:8:aaaaaaa", 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range evs {
		if e.Kind == "slot.head_moved" && strings.Contains(e.Message, h.shaPR8[:7]) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no head_moved event in %+v", evs)
	}
}

func TestCheckoutRefusals(t *testing.T) {
	h := newHarness(t)
	sl, pr := h.claimedCheckout(8, h.shaPR8)
	other := h.pr(h.repo().ID, 7, h.shaPR7, store.PRQueued)
	if err := h.m.Checkout(h.ctx, sl, other, h.pool, h.shaPR7); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Checkout for another PR: err = %v, want ErrConflict", err)
	}
	// Re-review in a held slot after someone committed there → head_drift hold, HEAD untouched.
	if err := h.st.TransitionSlot(h.ctx, sl.ID, []string{store.SlotClaimed}, store.SlotHeld, nil); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sl.Path, "README"), "human\n")
	gitT(t, sl.Path, "commit", "--quiet", "-am", "human")
	human := gitT(t, sl.Path, "rev-parse", "HEAD")
	wantHold(t, h.m.Checkout(h.ctx, h.slot(sl.Name), pr, h.pool, h.shaPR8), HoldHeadDrift)
	if head := gitT(t, sl.Path, "rev-parse", "HEAD"); head != human {
		t.Fatalf("HEAD moved to %s despite the hold", head)
	}
	got := h.slot(sl.Name)
	if store.Deref(got.HoldReason) != HoldHeadDrift || got.State != store.SlotHeld {
		t.Fatalf("slot = hold %v state %s", store.Deref(got.HoldReason), got.State)
	}
	// busy slots are never switched under a running round.
	if err := h.st.UpdateSlotFields(h.ctx, sl.ID, func(u *store.SlotUpdate) { u.Set("hold_reason", nil) }); err != nil {
		t.Fatal(err)
	}
	if err := h.st.TransitionSlot(h.ctx, sl.ID, nil, store.SlotBusy, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Checkout(h.ctx, h.slot(sl.Name), pr, h.pool, h.shaPR8); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Checkout of a busy slot: err = %v", err)
	}
}

func TestRelease(t *testing.T) {
	h := newHarness(t)
	sl, pr := h.claimedCheckout(7, h.shaPR7)
	if !sl.DirtySchema {
		t.Fatal("precondition: dirty_schema")
	}
	if err := h.st.TransitionSlot(h.ctx, sl.ID, []string{store.SlotClaimed}, store.SlotHeld, nil); err != nil {
		t.Fatal(err)
	}
	h.run.reset()
	h.clearScripts()
	if err := h.m.Release(h.ctx, h.slot(sl.Name), h.pool, "pr_closed"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	got := h.slot(sl.Name)
	if got.State != store.SlotFree || got.PRID != nil || got.CheckedOutSHA != nil || got.DirtySchema || got.LastError != nil {
		t.Fatalf("released slot = %+v", got)
	}
	if b := gitT(t, sl.Path, "branch", "--show-current"); b != "review1" {
		t.Fatalf("branch = %q", b)
	}
	if head := gitT(t, sl.Path, "rev-parse", "HEAD"); head != h.shaMaster {
		t.Fatalf("HEAD = %s, want master", head)
	}
	if _, err := gitx.New(h.run).RevParse(h.ctx, h.main, gitx.PRRef(7)); !errors.Is(err, gitx.ErrNoSuchRef) {
		t.Fatalf("PR ref still present: %v", err)
	}
	reset := h.run.gitCalls("switch", "--quiet", "--discard-changes", "--no-track", "-C", "review1", "origin/master")
	if len(reset) != 1 || !reset[0].Mutates {
		t.Fatalf("placeholder reset = %v", reset)
	}
	// Lock went back to master's → deps again; dirty schema → reset_db scripts in order.
	if n := len(h.scriptCalls(h.pool.PostCheckout[0])); n != 1 {
		t.Fatalf("deps calls = %d", n)
	}
	var scripts []string
	for _, c := range h.scriptCalls("") {
		scripts = append(scripts, c.Script)
		if strings.Contains(c.Script, "db:") && (c.Cmd.Timeout != ResetDBTimeout || !c.Cmd.Mutates || c.Env["WT_BRANCH"] != "review1") {
			t.Fatalf("reset cmd = %+v", c.Cmd)
		}
	}
	want := []string{h.pool.PostCheckout[0], h.pool.ResetDB[0], h.pool.ResetDB[1]}
	if !slices.Equal(scripts, want) {
		t.Fatalf("scripts = %q, want %q", scripts, want)
	}
	as, err := h.st.AssignmentsByPR(h.ctx, pr.ID)
	if err != nil || len(as) != 1 || as[0].EndedAt == nil || store.Deref(as[0].EndReason) != "pr_closed" {
		t.Fatalf("assignments = %+v, %v", as, err)
	}

	// Release of a free slot is refused.
	if err := h.m.Release(h.ctx, got, h.pool, "again"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Release of a free slot: err = %v", err)
	}
}

func TestReleaseSchemaVersionMismatch(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.claimedCheckout(8, h.shaPR8)
	// Someone migrated the slot's dev database by hand.
	h.my.add("20991231000000", "talkable_development__review1")
	h.clearScripts()
	if err := h.m.Release(h.ctx, sl, h.pool, "evicted"); err != nil {
		t.Fatal(err)
	}
	if n := len(h.scriptCalls(h.pool.ResetDB[0])); n != 1 {
		t.Fatalf("schema reset ran %d times, want 1", n)
	}
	if got := h.slot(sl.Name); got.State != store.SlotFree {
		t.Fatalf("state = %s", got.State)
	}

	// Matching version and clean flag → no reset.
	sl2, _ := h.claimAgain(t, 8, h.shaPR8)
	h.clearScripts()
	if err := h.m.Release(h.ctx, sl2, h.pool, "evicted"); err != nil {
		t.Fatal(err)
	}
	if n := len(h.scriptCalls(h.pool.ResetDB[0])); n != 0 {
		t.Fatalf("schema reset ran with a matching version")
	}
}

// claimAgain re-queues PR number and claims/checks out the free slot again.
func (h *harness) claimAgain(t *testing.T, number int, sha string) (store.Slot, store.PR) {
	t.Helper()
	pr := h.pr(h.repo().ID, number, sha, store.PRQueued)
	if err := h.st.TransitionPR(h.ctx, pr.ID, nil, store.PRQueued, nil); err != nil {
		t.Fatal(err)
	}
	pr, _ = h.st.PRByID(h.ctx, pr.ID)
	sl, err := h.m.Claim(h.ctx, pr, h.pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.m.Checkout(h.ctx, sl, pr, h.pool, sha); err != nil {
		t.Fatal(err)
	}
	return h.slot(sl.Name), pr
}

func TestReleaseSchemaResetFailsTwiceBreaksSlot(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.claimedCheckout(7, h.shaPR7)
	h.failScript[h.pool.ResetDB[0]] = 2
	h.clearScripts()
	err := h.m.Release(h.ctx, sl, h.pool, "pr_closed")
	if !errors.Is(err, ErrBroken) {
		t.Fatalf("err = %v, want ErrBroken", err)
	}
	if n := len(h.scriptCalls(h.pool.ResetDB[0])); n != 2 {
		t.Fatalf("reset attempts = %d, want 2", n)
	}
	got := h.slot(sl.Name)
	if got.State != store.SlotBroken || got.LastError == nil || strings.Contains(*got.LastError, "ghp_") {
		t.Fatalf("slot = %s %v", got.State, store.Deref(got.LastError))
	}
}

func TestReleaseSchemaResetRetriesOnce(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.claimedCheckout(7, h.shaPR7)
	h.failScript[h.pool.ResetDB[1]] = 1
	if err := h.m.Release(h.ctx, sl, h.pool, "pr_closed"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := h.slot(sl.Name); got.State != store.SlotFree || got.DirtySchema {
		t.Fatalf("slot = %s dirty=%v", got.State, got.DirtySchema)
	}
}

func TestReleaseResumesAfterCrash(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.claimedCheckout(7, h.shaPR7)
	crash := errors.New("crash")
	ctx := steps.WithFailpoint(h.ctx, func(subject, name string, at steps.Point) error {
		if name == "deps" && at == steps.BeforeRun {
			return crash
		}
		return nil
	})
	if err := h.m.Release(ctx, sl, h.pool, "pr_closed"); !errors.Is(err, crash) {
		t.Fatalf("err = %v, want crash", err)
	}
	if got := h.slot(sl.Name); got.State != store.SlotReleasing {
		t.Fatalf("state after crash = %s", got.State)
	}
	h.run.reset()
	if err := h.m.Release(h.ctx, h.slot(sl.Name), h.pool, "pr_closed"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n := len(h.run.gitCalls("switch")); n != 0 {
		t.Fatalf("resume repeated the placeholder reset (%d)", n)
	}
	if n := len(h.run.gitCalls("fetch")); n != 0 {
		t.Fatalf("resume repeated fetch_base (%d)", n)
	}
	if got := h.slot(sl.Name); got.State != store.SlotFree {
		t.Fatalf("state = %s", got.State)
	}
}

func TestReleaseRefusesPinned(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.claimedCheckout(8, h.shaPR8)
	if err := h.m.Pin(h.ctx, sl); err != nil {
		t.Fatal(err)
	}
	wantHold(t, h.m.Release(h.ctx, h.slot(sl.Name), h.pool, "evict"), HoldPinned)
	if got := h.slot(sl.Name); got.State != store.SlotClaimed {
		t.Fatalf("state = %s", got.State)
	}
}

func TestReserveLeavesThePRStateAlone(t *testing.T) {
	h := newHarness(t)
	sl := h.provisioned(1)
	pr := h.pr(h.repo().ID, 7, h.shaPR7, store.PRReviewed)
	got, err := h.m.Reserve(h.ctx, pr, h.pool)
	if err != nil || got.ID != sl.ID || got.State != store.SlotClaimed || store.Deref(got.PRID) != pr.ID {
		t.Fatalf("Reserve = %+v, %v", got, err)
	}
	if cur, _ := h.st.PRByID(h.ctx, pr.ID); cur.State != store.PRReviewed {
		t.Fatalf("PR state = %s, want reviewed", cur.State)
	}
	a, err := h.st.OpenAssignmentByPR(h.ctx, pr.ID)
	if err != nil || a.SlotID != sl.ID || len(a.DBNames) != 2 {
		t.Fatalf("assignment = %+v, %v", a, err)
	}
	// The PR's own slot is returned again; Checkout then works on it.
	again, err := h.m.Reserve(h.ctx, pr, h.pool)
	if err != nil || again.ID != sl.ID {
		t.Fatalf("second Reserve = %+v, %v", again, err)
	}
	if err := h.m.Checkout(h.ctx, again, pr, h.pool, h.shaPR7); err != nil {
		t.Fatalf("Checkout of a reserved slot: %v", err)
	}
	other := h.pr(h.repo().ID, 8, h.shaPR8, store.PRReviewed)
	if _, err := h.m.Reserve(h.ctx, other, h.pool); !errors.Is(err, ErrNoFreeSlot) {
		t.Fatalf("no free slot: err = %v", err)
	}
}

func TestDropGuardDerivation(t *testing.T) {
	g := DropGuard(config.Pool{SlotName: "review{n}",
		Databases: []string{"talkable_development__{slug}", "talkable_test__{slug}", "talkable_test_shard_1__{slug}"}})
	if g.Prefix != "talkable_" {
		t.Fatalf("prefix = %q, want the common start talkable_", g.Prefix)
	}
	for _, n := range []string{"talkable_test__review3", "talkable_development__review12", "talkable_test_shard_1__review1"} {
		if err := g.Check(n); err != nil {
			t.Errorf("%s refused: %v", n, err)
		}
	}
	for _, n := range []string{"talkable_test__repo1", "other_test__review1", "talkable_test__review", "talkable_test__review1x"} {
		if err := g.Check(n); err == nil {
			t.Errorf("%s allowed", n)
		}
	}
	one := DropGuard(config.Pool{SlotName: "review{n}", Databases: []string{"app_dev__{slug}"}})
	if one.Prefix != "app_dev__" || one.Check("app_dev__review4") != nil || one.Check("app_test__review4") == nil {
		t.Fatalf("single template guard = %+v", one)
	}
	for _, p := range []config.Pool{
		{SlotName: "review", Databases: []string{"talkable_test__{slug}"}}, // no {n}
		{SlotName: "review{n}"}, // no databases
		{SlotName: "review{n}", Databases: []string{"a__{slug}", "b__{slug}"}}, // no common prefix
	} {
		if g := DropGuard(p); g.AllowRegexp != nil || g.Check("talkable_test__review1") == nil {
			t.Errorf("DropGuard(%+v) = %+v, want the zero guard", p, g)
		}
	}
}
