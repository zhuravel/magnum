package slots

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// fixerCommit is what the fix job's fixer does in a slot: a commit on top of
// HEAD. Each commit gets its own content, so two of them never collapse.
func fixerCommit(t *testing.T, dir, name string) string {
	t.Helper()
	writeFile(t, filepath.Join(dir, "README"), "fix "+name+"\n")
	gitT(t, dir, "commit", "--quiet", "-am", "fix "+name)
	return gitT(t, dir, "rev-parse", "HEAD")
}

// strayEdit leaves a tracked file modified, as a fixer's test run does.
func strayEdit(t *testing.T, dir string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "Gemfile.lock"), "GEM stray\n")
}

// wantResynced checks what Resync promises about slot name: HEAD is detached
// at sha, the tracked tree is clean, checked_out_sha records sha, no switch
// is left pending, and Guard has nothing to say.
func (h *harness) wantResynced(name, sha string) {
	h.t.Helper()
	sl := h.slot(name)
	if head := gitT(h.t, sl.Path, "rev-parse", "HEAD"); head != sha {
		h.t.Fatalf("HEAD = %s, want %s", head, sha)
	}
	if b := gitT(h.t, sl.Path, "branch", "--show-current"); b != "" {
		h.t.Fatalf("HEAD is on branch %q, want detached", b)
	}
	if st := gitT(h.t, sl.Path, "status", "--porcelain", "--untracked-files=no"); st != "" {
		h.t.Fatalf("the tree is not clean:\n%s", st)
	}
	if got := store.Deref(sl.CheckedOutSHA); got != sha {
		h.t.Fatalf("checked_out_sha = %s, want %s", got, sha)
	}
	if v, ok, err := h.st.GetKV(h.ctx, switchingKey(sl.ID)); err != nil || ok {
		h.t.Fatalf("a switch is still pending: %q %v %v", v, ok, err)
	}
	if err := h.m.Guard(h.ctx, sl); err != nil {
		h.t.Fatalf("Guard after Resync: %v", err)
	}
	if got := h.slot(name); got.HoldReason != nil {
		h.t.Fatalf("hold_reason = %q after Resync", store.Deref(got.HoldReason))
	}
}

// resyncEvents are the slot.resync events of slot name.
func (h *harness) resyncEvents(name string) []store.Event {
	h.t.Helper()
	evs, err := h.st.EventsBySubject(h.ctx, "slot:"+name, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []store.Event
	for _, e := range evs {
		if e.Kind == "slot.resync" {
			out = append(out, e)
		}
	}
	return out
}

// After a fixer's run the slot's HEAD is no longer the recorded head, and the
// next round's guard would hold it for head drift. Resync switches the slot,
// detached, to the head the job settled on, drops the tracked changes the
// fixer left, and records that head, so the guard passes.
func TestResyncSwitchesDetachedAndRecordsTheHead(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.claimedCheckout(8, h.shaPR8)
	pushed := fixerCommit(t, sl.Path, "one")
	fixerCommit(t, sl.Path, "two") // a later commit that was never pushed
	strayEdit(t, sl.Path)
	h.run.reset()

	if err := h.m.Resync(h.ctx, sl, pushed); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	h.wantResynced(sl.Name, pushed)
	if sw := h.run.gitCalls("switch", "--quiet", "--discard-changes", "--detach"); len(sw) != 1 || !sw[0].Mutates {
		t.Fatalf("switch calls = %v, want one mutating switch", sw)
	}
	evs := h.resyncEvents(sl.Name)
	if len(evs) != 1 || !strings.Contains(evs[0].Message, sl.Name) || !strings.Contains(evs[0].Message, pushed[:7]) {
		t.Fatalf("slot.resync events = %+v, want one naming the slot and %s", evs, pushed[:7])
	}
	// Each step wrote its begin and ok rows under the resync's subject.
	for _, step := range []string{"guard", "fetch", "switch"} {
		done, err := h.st.StepDone(h.ctx, "slot:"+sl.Name+":resync:"+pushed[:7], step)
		if err != nil || !done {
			t.Fatalf("step %s done = %v, %v", step, done, err)
		}
	}
}

// A fixer that committed locally but pushed nothing leaves the slot ahead of
// the head the review ran on: the job puts the slot back at that head.
func TestResyncSwitchesBackToTheOldHeadWhenNothingWasPushed(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.claimedCheckout(8, h.shaPR8)
	fixerCommit(t, sl.Path, "one")
	strayEdit(t, sl.Path)

	if err := h.m.Resync(h.ctx, sl, h.shaPR8); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	h.wantResynced(sl.Name, h.shaPR8)
}

// The usual outcome is a fixer that pushed what it committed: HEAD already is
// the pushed head. Resync records it and, with nothing to discard, runs no
// switch and no fetch; the same call twice changes nothing.
func TestResyncOfTheHeadAlreadyCheckedOutSwitchesNothing(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.claimedCheckout(8, h.shaPR8)
	pushed := fixerCommit(t, sl.Path, "one")
	h.run.reset()

	for i := range 2 {
		if err := h.m.Resync(h.ctx, sl, pushed); err != nil {
			t.Fatalf("Resync #%d: %v", i+1, err)
		}
		h.wantResynced(sl.Name, pushed)
	}
	if sw := h.run.gitCalls("switch"); len(sw) != 0 {
		t.Fatalf("switch calls = %v, want none", sw)
	}
	if fe := h.run.gitCalls("fetch"); len(fe) != 0 {
		t.Fatalf("fetch calls = %v, want none: the commit is in the slot", fe)
	}
	// The second call ran its steps afresh instead of skipping them.
	if n := len(h.resyncEvents(sl.Name)); n != 2 {
		t.Fatalf("slot.resync events = %d, want 2", n)
	}
}

// HEAD at the pushed head is not enough to skip the switch when the fixer
// left tracked changes behind: they go, as a checkout drops them.
func TestResyncOfTheHeadAlreadyCheckedOutStillDropsStrayEdits(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.claimedCheckout(8, h.shaPR8)
	pushed := fixerCommit(t, sl.Path, "one")
	strayEdit(t, sl.Path)

	if err := h.m.Resync(h.ctx, sl, pushed); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	h.wantResynced(sl.Name, pushed)
}

// A head the slot does not have (pushed from another clone) is fetched by its
// id, only because it is missing, and then checked out.
func TestResyncFetchesAHeadTheSlotLacks(t *testing.T) {
	h := newHarness(t)
	work := h.ownOrigin(t)
	sl, _ := h.claimedCheckout(8, h.shaPR8)
	fixerCommit(t, sl.Path, "local")
	pushed := pushPR(t, work, 8, h.shaPR8, "README", "pushed elsewhere\n")
	if _, err := h.m.git.RevParse(h.ctx, sl.Path, pushed); err == nil {
		t.Fatal("the slot already has the pushed commit; the test needs it missing")
	}
	h.run.reset()

	if err := h.m.Resync(h.ctx, sl, pushed); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	h.wantResynced(sl.Name, pushed)
	if fe := h.run.gitCalls("fetch"); len(fe) != 1 || !fe[0].Mutates {
		t.Fatalf("fetch calls = %v, want one", fe)
	}
}

// A sha that is not a commit id, or a commit nowhere to be found, fails
// before the slot is touched.
func TestResyncRefusesAnUnusableSha(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.claimedCheckout(8, h.shaPR8)
	local := fixerCommit(t, sl.Path, "one")

	for name, sha := range map[string]string{
		"a branch name":  "master",
		"an option":      "--detach",
		"too short":      "abc",
		"unknown commit": strings.Repeat("ab", 20),
	} {
		t.Run(name, func(t *testing.T) {
			if err := h.m.Resync(h.ctx, sl, sha); err == nil {
				t.Fatal("Resync succeeded")
			}
			if head := gitT(t, sl.Path, "rev-parse", "HEAD"); head != local {
				t.Fatalf("HEAD moved to %s", head)
			}
			if got := h.slot(sl.Name); store.Deref(got.CheckedOutSHA) != h.shaPR8 {
				t.Fatalf("checked_out_sha = %s", store.Deref(got.CheckedOutSHA))
			}
		})
	}
}

// The fix job never runs on a pinned slot, and a pin set while the fixer
// worked stops the resync: the person who pinned it may be at work in it.
// A held slot and a person's agent in it stop it the same way.
func TestResyncRefusesASlotAPersonHolds(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *harness, sl store.Slot)
		hold  string
	}{
		{"a pinned slot", func(h *harness, sl store.Slot) {
			if err := h.m.Pin(h.ctx, sl); err != nil {
				h.t.Fatal(err)
			}
		}, HoldPinned},
		{"a held slot", func(h *harness, sl store.Slot) {
			if err := h.st.UpdateSlotFields(h.ctx, sl.ID, func(u *store.SlotUpdate) { u.Set("hold_reason", HoldDirtyWorktree) }); err != nil {
				h.t.Fatal(err)
			}
		}, HoldDirtyWorktree},
		{"a person's agent in the slot", func(h *harness, sl store.Slot) {
			h.snap = herdr.Snapshot{
				Panes:  []herdr.Pane{{ID: "w1:p1", WorkspaceID: "w1", Cwd: sl.Path, Agent: "claude", AgentStatus: herdr.StatusIdle}},
				Agents: []herdr.AgentInfo{{PaneID: "w1:p1", Name: "alice", Agent: "claude", AgentStatus: herdr.StatusIdle, Cwd: sl.Path}},
			}
		}, HoldForeignAgent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			sl, _ := h.claimedCheckout(8, h.shaPR8)
			local := fixerCommit(t, sl.Path, "one")
			strayEdit(t, sl.Path)
			tc.setup(h, sl)
			h.run.reset()

			err := h.m.Resync(h.ctx, sl, h.shaPR8)
			hold := wantHold(t, err, tc.hold)
			if tc.hold == HoldPinned && !strings.Contains(hold.Detail, sl.Name) {
				t.Fatalf("detail %q does not name the slot", hold.Detail)
			}
			if head := gitT(t, sl.Path, "rev-parse", "HEAD"); head != local {
				t.Fatalf("HEAD moved to %s", head)
			}
			if !strings.Contains(readFile(t, filepath.Join(sl.Path, "Gemfile.lock")), "stray") {
				t.Fatal("the stray edit was discarded")
			}
			if got := h.slot(sl.Name); store.Deref(got.CheckedOutSHA) != h.shaPR8 {
				t.Fatalf("checked_out_sha = %s, want it untouched", store.Deref(got.CheckedOutSHA))
			}
			if sw := h.run.gitCalls("switch", "--quiet", "--discard-changes"); len(sw) != 0 {
				t.Fatalf("switch calls = %v, want none", sw)
			}
		})
	}
}

// Someone typing into the PR's magnum panes after the slot's checkout makes
// the tracked changes in its tree theirs, and Guard holds them. Resync's
// switch would drop them for good, so it holds them the same way (the dirty
// worktree hold, persisted) and leaves the tree and the record alone. Typing
// before the checkout, or no changes to drop, lets it through.
func TestResyncHoldsTrackedChangesAfterHumanActivity(t *testing.T) {
	h := newHarness(t)
	sl, pr := h.claimedCheckout(8, h.shaPR8)
	local := fixerCommit(t, sl.Path, "one")
	strayEdit(t, sl.Path)

	h.humanTyped(pr.ID, h.now.Add(time.Minute))
	h.run.reset()
	hold := wantHold(t, h.m.Resync(h.ctx, sl, h.shaPR8), HoldDirtyWorktree)
	if !strings.Contains(hold.Detail, "1 tracked change") || !strings.Contains(hold.Detail, "someone typed") {
		t.Fatalf("detail %q names neither the changes nor the activity", hold.Detail)
	}
	got := h.slot(sl.Name)
	if store.Deref(got.HoldReason) != HoldDirtyWorktree {
		t.Fatalf("hold_reason = %q, want the dirty worktree hold persisted", store.Deref(got.HoldReason))
	}
	if store.Deref(got.CheckedOutSHA) != h.shaPR8 {
		t.Fatalf("checked_out_sha = %s, want it untouched", store.Deref(got.CheckedOutSHA))
	}
	if head := gitT(t, sl.Path, "rev-parse", "HEAD"); head != local {
		t.Fatalf("HEAD moved to %s", head)
	}
	if !strings.Contains(readFile(t, filepath.Join(sl.Path, "Gemfile.lock")), "stray") {
		t.Fatal("the changes were discarded")
	}
	if sw := h.run.gitCalls("switch", "--quiet", "--discard-changes"); len(sw) != 0 {
		t.Fatalf("switch calls = %v, want none", sw)
	}
	// The hold is persisted: a second call is refused as a held slot.
	wantHold(t, h.m.Resync(h.ctx, sl, h.shaPR8), HoldDirtyWorktree)

	// Without changes the activity is no reason to refuse.
	h.clearHold(sl.ID)
	gitT(t, sl.Path, "checkout", "--quiet", "--", "Gemfile.lock")
	if err := h.m.Resync(h.ctx, h.slot(sl.Name), h.shaPR8); err != nil {
		t.Fatalf("Resync of a clean tree after human activity: %v", err)
	}
	h.wantResynced(sl.Name, h.shaPR8)
}

// Typing before the slot's checkout is not evidence about the changes now in
// its tree: they are the fixer's residue, and Resync drops them.
func TestResyncDropsTrackedChangesWhenTheActivityPredatesTheCheckout(t *testing.T) {
	h := newHarness(t)
	sl, pr := h.claimedCheckout(8, h.shaPR8)
	fixerCommit(t, sl.Path, "one")
	strayEdit(t, sl.Path)
	h.humanTyped(pr.ID, h.now.Add(-time.Minute))

	if err := h.m.Resync(h.ctx, sl, h.shaPR8); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	h.wantResynced(sl.Name, h.shaPR8)
}

// midMerge leaves dir in the middle of a merge, as a fixer stopped during
// the fix skill's base merge (a timeout, `magnum abort`, a crash) leaves its
// slot: HEAD at head, and a commit on base that changed README as head
// changed it merged into it, the conflict unresolved, so MERGE_HEAD is there.
func midMerge(t *testing.T, dir, base, head string) {
	t.Helper()
	gitT(t, dir, "switch", "--quiet", "--detach", base)
	tip := fixerCommit(t, dir, "on the base")
	gitT(t, dir, "switch", "--quiet", "--detach", head)
	if _, err := gitRun(dir, "merge", "--no-edit", tip); err == nil {
		t.Fatal("the merge applied cleanly; the test needs a conflict")
	}
	if _, err := gitRun(dir, "rev-parse", "--quiet", "--verify", "MERGE_HEAD"); err != nil {
		t.Fatalf("no merge in progress: %v", err)
	}
}

// wantNoMerge fails while dir is in the middle of a merge.
func wantNoMerge(t *testing.T, dir string) {
	t.Helper()
	if _, err := gitRun(dir, "rev-parse", "--quiet", "--verify", "MERGE_HEAD"); err == nil {
		t.Fatal("the merge is still in progress")
	}
}

// A fixer stopped in the middle of the base merge leaves MERGE_HEAD in the
// slot, and git refuses every switch then ("cannot switch branch while
// merging"): the settle's Resync, and every later checkout or release of
// the slot, failed until someone aborted the merge by hand. Resync aborts a
// merge it finds in progress before its switch, in a step of its own with a
// slot.aborted event, and the slot resyncs to the reviewed head. A
// slot in no merge runs no abort.
func TestResyncAbortsAMergeTheFixerLeftInProgress(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.claimedCheckout(8, h.shaPR8)
	midMerge(t, sl.Path, h.shaMaster, h.shaPR8)

	if err := h.m.Resync(h.ctx, sl, h.shaPR8); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	wantNoMerge(t, sl.Path)
	h.wantResynced(sl.Name, h.shaPR8)
	if done, err := h.st.StepDone(h.ctx, "slot:"+sl.Name+":resync:"+h.shaPR8[:7], "abort_in_progress"); err != nil || !done {
		t.Fatalf("step abort_in_progress done = %v, %v", done, err)
	}
	evs, err := h.st.EventsBySubject(h.ctx, "slot:"+sl.Name, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(slices.DeleteFunc(evs, func(e store.Event) bool { return e.Kind != "slot.aborted" })); n != 1 {
		t.Fatalf("slot.aborted events = %d, want 1", n)
	}

	h.run.reset()
	if err := h.m.Resync(h.ctx, h.slot(sl.Name), h.shaPR8); err != nil {
		t.Fatalf("Resync again: %v", err)
	}
	if ab := h.run.gitCalls("merge", "--abort"); len(ab) != 0 {
		t.Fatalf("merge --abort calls = %v on a slot in no merge", ab)
	}
}

// The release of a slot a fixer left in the middle of a merge aborts the
// merge before it resets the placeholder branch, which git would refuse
// too, and the slot is free on the base.
func TestReleaseAbortsAMergeLeftInProgress(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.claimedCheckout(8, h.shaPR8)
	if err := h.st.TransitionSlot(h.ctx, sl.ID, []string{store.SlotClaimed}, store.SlotHeld, nil); err != nil {
		t.Fatal(err)
	}
	midMerge(t, sl.Path, h.shaMaster, h.shaPR8)
	h.clearScripts()

	if err := h.m.Release(h.ctx, h.slot(sl.Name), h.pool, "pr_closed"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	wantNoMerge(t, sl.Path)
	if got := h.slot(sl.Name); got.State != store.SlotFree {
		t.Fatalf("released slot = %+v", got)
	}
	if head := gitT(t, sl.Path, "rev-parse", "HEAD"); head != h.shaMaster {
		t.Fatalf("HEAD = %s, want master", head)
	}
}

// midOperation leaves dir in the middle of op, a rebase or a cherry-pick
// stopped at a conflict, as `magnum fix-sync` leaves a slot whose replay's
// abort failed or that was killed mid-replay (exit 3): HEAD at head with a
// fixer's commit on top, replayed onto (rebase) or picking (cherry-pick) a
// commit on base that changed README as the fixer's did.
func midOperation(t *testing.T, dir, op, base, head string) {
	t.Helper()
	gitT(t, dir, "switch", "--quiet", "--detach", base)
	tip := fixerCommit(t, dir, "on the base")
	gitT(t, dir, "switch", "--quiet", "--detach", head)
	fixerCommit(t, dir, "the fixer's")
	args := []string{"cherry-pick", tip}
	if op == "rebase" {
		args = []string{"rebase", "--onto", tip, head}
	}
	if _, err := gitRun(dir, args...); err == nil {
		t.Fatalf("the %s applied cleanly; the test needs a conflict", op)
	}
	if got := inProgressOf(t, dir); got != op {
		t.Fatalf("in progress: %q, want %q", got, op)
	}
}

// inProgressOf names the operation dir is in the middle of ("" for none).
func inProgressOf(t *testing.T, dir string) string {
	t.Helper()
	for _, p := range []struct{ op, path string }{
		{"rebase", "rebase-merge"}, {"rebase", "rebase-apply"}, {"merge", "MERGE_HEAD"}, {"cherry-pick", "CHERRY_PICK_HEAD"},
	} {
		if _, err := os.Lstat(gitT(t, dir, "rev-parse", "--path-format=absolute", "--git-path", p.path)); err == nil {
			return p.op
		}
	}
	return ""
}

// A rebase fix-sync left in progress (its abort failed, or it was killed
// mid-replay: exit 3) held the slot: the step that ended a merge left the
// rebase alone, the skill forbade the fixer to rebase, and the settle's
// Resync failed at its switch ("cannot switch branch while rebasing"), as
// every later checkout and release did, until someone aborted by hand. The
// step aborts whatever is in progress, a rebase or a cherry-pick as a
// merge, with an event that names it, after a slot.discarded event for the
// tracked changes the abort drops; the slot resyncs to the reviewed head.
func TestResyncAbortsARebaseOrACherryPickLeftInProgress(t *testing.T) {
	for _, op := range []string{"rebase", "cherry-pick"} {
		t.Run(op, func(t *testing.T) {
			h := newHarness(t)
			sl, _ := h.claimedCheckout(8, h.shaPR8)
			midOperation(t, sl.Path, op, h.shaMaster, h.shaPR8)

			if err := h.m.Resync(h.ctx, sl, h.shaPR8); err != nil {
				t.Fatalf("Resync: %v", err)
			}
			if got := inProgressOf(t, sl.Path); got != "" {
				t.Fatalf("a %s is still in progress", got)
			}
			h.wantResynced(sl.Name, h.shaPR8)
			evs, err := h.st.EventsBySubject(h.ctx, "slot:"+sl.Name, 0)
			if err != nil {
				t.Fatal(err)
			}
			aborted := slices.DeleteFunc(slices.Clone(evs), func(e store.Event) bool { return e.Kind != "slot.aborted" })
			if len(aborted) != 1 || !strings.Contains(aborted[0].Message, "aborted a "+op+" left in progress") {
				t.Fatalf("slot.aborted events = %+v, want one that names the %s", aborted, op)
			}
			// Newest first: the abort's event comes after the one that names
			// what it drops.
			if discarded := slices.IndexFunc(evs, func(e store.Event) bool {
				return e.Kind == "slot.discarded" && strings.Contains(e.Message, "README") && e.ID < aborted[0].ID
			}); discarded < 0 {
				t.Fatalf("events %+v: want a slot.discarded naming README before the abort", evs)
			}
		})
	}
}
