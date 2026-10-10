package slots

import (
	"path/filepath"
	"strings"
	"testing"

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
