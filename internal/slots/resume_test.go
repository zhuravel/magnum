package slots

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/steps"
	"github.com/zhuravel/magnum/internal/store"
)

// Interrupted operations (a failed step, a crash between two writes) and the
// guard that every resume runs before its next destructive step.

// ownOrigin moves the harness's main clone onto a private, writable copy of
// the talkable origin (the shared one is read-only) and returns a scratch
// clone that can push to it.
func (h *harness) ownOrigin(t *testing.T) (work string) {
	t.Helper()
	fx := fixtures(t).talkable
	origin := filepath.Join(h.root, "remotes", "talkable", "talkable.git")
	work = filepath.Join(h.root, "work-talkable")
	copyRepo(t, fx.origin, origin)
	copyRepo(t, fx.work, work)
	repointOrigin(t, work, fx.origin, origin)
	repointOrigin(t, h.main, fx.origin, origin)
	h.origin = origin
	return work
}

// pushPR commits content to file on top of base in the scratch clone work and
// force-pushes it as refs/pull/<number>/head; it returns the new head.
func pushPR(t *testing.T, work string, number int, base, file, content string) string {
	t.Helper()
	gitT(t, work, "switch", "--quiet", "--detach", base)
	writeFile(t, filepath.Join(work, file), content)
	gitT(t, work, "add", "--", file)
	gitT(t, work, "commit", "--quiet", "-m", "update "+file)
	gitT(t, work, "push", "--quiet", "--force", "origin", "HEAD:refs/pull/"+strconv.Itoa(number)+"/head")
	return gitT(t, work, "rev-parse", "HEAD")
}

// humanCommit commits an edit to README in dir, as a person working in the
// slot would, and returns the new HEAD.
func humanCommit(t *testing.T, dir string) string {
	t.Helper()
	writeFile(t, filepath.Join(dir, "README"), "human commit\n")
	gitT(t, dir, "commit", "--quiet", "-am", "human")
	return gitT(t, dir, "rev-parse", "HEAD")
}

// repoName is the per-PR fixture's repository (owner/name).
func (f *perPRFixture) repoName() string { return f.repo.Owner + "/" + f.repo.Name }

// clearHold drops a persisted hold_reason so the next call detects it afresh.
func (h *harness) clearHold(id int64) {
	h.t.Helper()
	if err := h.st.UpdateSlotFields(h.ctx, id, func(u *store.SlotUpdate) { u.Set("hold_reason", nil) }); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) wantNoGit(t *testing.T, sub ...string) {
	t.Helper()
	if c := h.run.gitCalls(sub...); len(c) != 0 {
		t.Fatalf("git %v ran: %v", sub, c)
	}
}

func (h *harness) kv(key string) (string, bool) {
	h.t.Helper()
	v, ok, err := h.st.GetKV(h.ctx, key)
	if err != nil {
		h.t.Fatal(err)
	}
	return v, ok
}

// humanTyped sets the PR's human_active_at, as the agents package does when
// someone types into the PR's magnum panes.
func (h *harness) humanTyped(prID int64, at time.Time) {
	h.t.Helper()
	if err := h.st.UpdatePR(h.ctx, prID, func(u *store.PRUpdate) { u.Set("human_active_at", at) }); err != nil {
		h.t.Fatal(err)
	}
}

// discarded returns the slot's slot.discarded events.
func (h *harness) discarded(t *testing.T, name string) []store.Event {
	t.Helper()
	return eventKinds(t, h.st, "slot:"+name, "slot.discarded")
}

// wantDiscarded checks the slot recorded exactly one slot.discarded event
// naming every path in want.
func (h *harness) wantDiscarded(t *testing.T, name string, want ...string) {
	t.Helper()
	evs := h.discarded(t, name)
	if len(evs) != 1 {
		t.Fatalf("slot.discarded events = %+v, want one", evs)
	}
	for _, p := range want {
		if !strings.Contains(evs[0].Message, p) {
			t.Fatalf("slot.discarded %q does not name %s", evs[0].Message, p)
		}
	}
}

// foreignAgent puts an idle agent magnum did not start in dir.
func (h *harness) foreignAgent(dir string) {
	h.snap = herdr.Snapshot{
		Panes:  []herdr.Pane{{ID: "w1:p1", WorkspaceID: "w1", Cwd: dir, Agent: "claude", AgentStatus: herdr.StatusIdle}},
		Agents: []herdr.AgentInfo{{PaneID: "w1:p1", Name: "example", Agent: "claude", AgentStatus: herdr.StatusIdle, Cwd: dir}},
	}
}

// The review's repro: the deps step fails after the switch to a new head that
// changes Gemfile.lock. The retry must take HEAD as magnum's own checkout,
// not as a human's commit.
func TestCheckoutRetryAfterDepsFailureIsNotDrift(t *testing.T) {
	h := newHarness(t)
	sl, pr := h.claimedCheckout(8, h.shaPR8)
	work := h.ownOrigin(t)
	head := pushPR(t, work, 8, h.shaPR8, "Gemfile.lock", "GEM v3\n")
	deps := h.pool.PostCheckout[0]
	h.failScript[deps] = 1

	err := h.m.Checkout(h.ctx, sl, pr, h.pool, head)
	if err == nil {
		t.Fatal("Checkout succeeded with a failing deps script")
	}
	if _, held := AsHold(err); held {
		t.Fatalf("err = %v, want the deps failure, not a hold", err)
	}
	got := h.slot(sl.Name)
	if store.Deref(got.CheckedOutSHA) != head || got.HoldReason != nil {
		t.Fatalf("after the failure: checked_out_sha %s hold %v; want %s recorded right after the switch",
			store.Deref(got.CheckedOutSHA), store.Deref(got.HoldReason), head)
	}
	if v, ok := h.kv(switchingKey(sl.ID)); ok {
		t.Fatalf("pending switch %s left in the kv store after the record", v)
	}

	if err := h.m.Checkout(h.ctx, got, pr, h.pool, head); err != nil {
		t.Fatalf("retry: %v", err)
	}
	got = h.slot(sl.Name)
	if got.HoldReason != nil || store.Deref(got.CheckedOutSHA) != head || got.LastError != nil || got.State != store.SlotClaimed {
		t.Fatalf("after the retry: hold %v sha %s last_error %v state %s",
			store.Deref(got.HoldReason), store.Deref(got.CheckedOutSHA), store.Deref(got.LastError), got.State)
	}
	if cur := gitT(t, sl.Path, "rev-parse", "HEAD"); cur != head {
		t.Fatalf("HEAD = %s, want %s", cur, head)
	}
	lock, err := lockHash(sl.Path)
	if err != nil || store.Deref(got.LockSHA) != lock {
		t.Fatalf("lock_sha = %s, want %s (%v)", store.Deref(got.LockSHA), lock, err)
	}
	if n := len(h.scriptCalls(deps)); n != 2 {
		t.Fatalf("deps ran %d times, want the failure and the retry", n)
	}
	if evs := eventKinds(t, h.st, "slot:"+sl.Name, "slot.hold"); len(evs) != 0 {
		t.Fatalf("hold events = %+v", evs)
	}
	if a, err := h.st.OpenAssignmentBySlot(h.ctx, sl.ID); err != nil || store.Deref(a.HeadSHA) != head {
		t.Fatalf("assignment = %+v, %v; want head %s", a, err, head)
	}
}

// A crash between the switch and its record leaves HEAD at the pending sha
// and checked_out_sha at the old one.
func TestGuardRecordsAnInterruptedSwitch(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.claimedCheckout(8, h.shaPR8)
	key := switchingKey(sl.ID)
	if err := h.st.SetKV(h.ctx, key, h.shaMaster); err != nil {
		t.Fatal(err)
	}
	gitT(t, sl.Path, "switch", "--quiet", "--detach", h.shaMaster)

	if err := h.m.Guard(h.ctx, sl); err != nil {
		t.Fatalf("Guard: %v", err)
	}
	got := h.slot(sl.Name)
	if store.Deref(got.CheckedOutSHA) != h.shaMaster || got.HoldReason != nil {
		t.Fatalf("checked_out_sha %s hold %v; want the pending %s recorded", store.Deref(got.CheckedOutSHA),
			store.Deref(got.HoldReason), h.shaMaster)
	}
	if v, ok := h.kv(key); ok {
		t.Fatalf("pending switch %s not deleted", v)
	}
	if evs := eventKinds(t, h.st, "slot:"+sl.Name, "slot.switch_recorded"); len(evs) != 1 {
		t.Fatalf("switch_recorded events = %+v", evs)
	}
	if evs := eventKinds(t, h.st, "slot:"+sl.Name, "slot.hold"); len(evs) != 0 {
		t.Fatalf("hold events = %+v", evs)
	}

	// A HEAD that is neither checked_out_sha nor the pending sha is a human's.
	if err := h.st.SetKV(h.ctx, key, h.shaPR8); err != nil {
		t.Fatal(err)
	}
	humanCommit(t, sl.Path)
	wantHold(t, h.m.Guard(h.ctx, h.slot(sl.Name)), HoldHeadDrift)
	got = h.slot(sl.Name)
	if store.Deref(got.HoldReason) != HoldHeadDrift || store.Deref(got.CheckedOutSHA) != h.shaMaster {
		t.Fatalf("hold %v checked_out_sha %s; want head_drift persisted, sha unchanged", store.Deref(got.HoldReason),
			store.Deref(got.CheckedOutSHA))
	}
	if v, ok := h.kv(key); !ok || v != h.shaPR8 {
		t.Fatalf("pending switch = %q, %v; want it untouched", v, ok)
	}
}

// What a round leaves behind (a test run rewriting db/schema.rb, an untracked
// scratch file) is magnum's residue: no hold, discarded with an event.
func TestResidueAfterARoundIsDiscarded(t *testing.T) {
	h := newHarness(t)
	sl, pr := h.claimedCheckout(8, h.shaPR8)
	schema := filepath.Join(sl.Path, SchemaFile)
	orig := readFile(t, schema)
	writeFile(t, schema, schemaRB(schemaV2))
	notes := filepath.Join(sl.Path, "coverage.txt")
	writeFile(t, notes, "residue\n")

	if err := h.m.Guard(h.ctx, sl); err != nil {
		t.Fatalf("Guard over residue: %v", err)
	}
	if err := h.m.Checkout(h.ctx, h.slot(sl.Name), pr, h.pool, h.shaPR8); err != nil {
		t.Fatalf("Checkout over residue: %v", err)
	}
	if c := readFile(t, schema); c != orig {
		t.Fatalf("%s = %q, want the residue discarded", SchemaFile, c)
	}
	h.wantDiscarded(t, sl.Name, "1 tracked change(s)", SchemaFile)
	if evs := h.discarded(t, sl.Name); strings.Contains(evs[0].Message, "coverage.txt") {
		t.Fatalf("a switch keeps untracked files; the event names one: %q", evs[0].Message)
	}

	writeFile(t, schema, schemaRB(schemaV2))
	if err := h.m.Release(h.ctx, h.slot(sl.Name), h.pool, "pr_closed"); err != nil {
		t.Fatalf("Release over residue: %v", err)
	}
	got := h.slot(sl.Name)
	if got.State != store.SlotFree || got.HoldReason != nil {
		t.Fatalf("state %s hold %v", got.State, store.Deref(got.HoldReason))
	}
	if evs := h.discarded(t, sl.Name); len(evs) != 2 {
		t.Fatalf("slot.discarded events = %+v, want the checkout's and the release's", evs)
	}
	if c := readFile(t, notes); c != "residue\n" {
		t.Fatalf("coverage.txt = %q, untracked files survive a release", c)
	}
	if evs := eventKinds(t, h.st, "slot:"+sl.Name, "slot.hold"); len(evs) != 0 {
		t.Fatalf("hold events = %+v", evs)
	}
}

// A human typing into the PR's magnum panes after the checkout makes the
// tree's changes theirs: held (persisted) and kept by Guard, Checkout and
// Release. Typing before the checkout does not.
func TestGuardHoldsTrackedChangesAfterHumanActivity(t *testing.T) {
	h := newHarness(t)
	sl, pr := h.claimedCheckout(8, h.shaPR8)
	if sl.LastUsedAt == nil || !sl.LastUsedAt.Equal(h.now) {
		t.Fatalf("last_used_at = %v, want the checkout time", sl.LastUsedAt)
	}
	readme := filepath.Join(sl.Path, "README")
	writeFile(t, readme, "human edit\n")

	h.humanTyped(pr.ID, h.now.Add(-time.Minute))
	if err := h.m.Guard(h.ctx, sl); err != nil {
		t.Fatalf("Guard with human activity before the checkout: %v", err)
	}

	h.humanTyped(pr.ID, h.now.Add(time.Minute))
	wantHeld := func(what string) {
		t.Helper()
		got := h.slot(sl.Name)
		if store.Deref(got.HoldReason) != HoldDirtyWorktree || got.State != store.SlotClaimed || store.Deref(got.PRID) != pr.ID {
			t.Fatalf("%s: hold %v state %s pr %v; want dirty_worktree persisted on the claimed slot", what,
				store.Deref(got.HoldReason), got.State, got.PRID)
		}
		if c := readFile(t, readme); c != "human edit\n" {
			t.Fatalf("%s: README = %q, the change was discarded", what, c)
		}
		h.wantNoGit(t, "switch")
		h.clearHold(sl.ID)
	}
	h.run.reset()
	hold := wantHold(t, h.m.Guard(h.ctx, sl), HoldDirtyWorktree)
	if !strings.Contains(hold.Detail, "1 tracked change(s)") || !strings.Contains(hold.Detail, "PR #8") {
		t.Fatalf("detail %q does not name the change and the evidence", hold.Detail)
	}
	wantHeld("Guard")
	wantHold(t, h.m.Checkout(h.ctx, h.slot(sl.Name), pr, h.pool, h.shaPR8), HoldDirtyWorktree)
	wantHeld("Checkout")
	wantHold(t, h.m.Release(h.ctx, h.slot(sl.Name), h.pool, "pr_closed"), HoldDirtyWorktree)
	wantHeld("Release")
	if evs := h.discarded(t, sl.Name); len(evs) != 0 {
		t.Fatalf("slot.discarded events = %+v", evs)
	}

	// Untracked files alone hold neither a checkout nor a release (and survive both).
	gitT(t, sl.Path, "checkout", "--", "README")
	notes := filepath.Join(sl.Path, "notes.txt")
	writeFile(t, notes, "scratch\n")
	if err := h.m.Checkout(h.ctx, h.slot(sl.Name), pr, h.pool, h.shaPR8); err != nil {
		t.Fatalf("Checkout with an untracked file: %v", err)
	}
	if err := h.m.Release(h.ctx, h.slot(sl.Name), h.pool, "pr_closed"); err != nil {
		t.Fatalf("Release with an untracked file: %v", err)
	}
	if got := h.slot(sl.Name); got.State != store.SlotFree || got.HoldReason != nil {
		t.Fatalf("state %s hold %v", got.State, store.Deref(got.HoldReason))
	}
	if c := readFile(t, notes); c != "scratch\n" {
		t.Fatalf("notes.txt = %q", c)
	}
}

// A human's agent in the slot holds it; with changes in the tree the hold is
// persisted, so it outlives the agent.
func TestGuardForeignAgentWithChangesPersistsTheHold(t *testing.T) {
	h := newHarness(t)
	sl, pr := h.claimedCheckout(8, h.shaPR8)
	readme := filepath.Join(sl.Path, "README")
	writeFile(t, readme, "agent edit\n")
	h.foreignAgent(sl.Path)
	hold := wantHold(t, h.m.Guard(h.ctx, sl), HoldDirtyWorktree)
	if !strings.Contains(hold.Detail, `agent "example"`) {
		t.Fatalf("detail %q does not name the agent", hold.Detail)
	}
	h.snap = herdr.Snapshot{} // the agent exits
	h.run.reset()
	wantHold(t, h.m.Release(h.ctx, h.slot(sl.Name), h.pool, "pr_closed"), HoldDirtyWorktree)
	h.wantNoGit(t, "switch")
	if got := h.slot(sl.Name); got.State != store.SlotClaimed || store.Deref(got.PRID) != pr.ID ||
		store.Deref(got.HoldReason) != HoldDirtyWorktree {
		t.Fatalf("state %s pr %v hold %v", got.State, got.PRID, store.Deref(got.HoldReason))
	}
	if c := readFile(t, readme); c != "agent edit\n" {
		t.Fatalf("README = %q", c)
	}
}

func TestReleaseResumeRunsTheGuardFirst(t *testing.T) {
	for _, human := range []bool{true, false} {
		name := "untouched"
		if human {
			name = "human commit"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			sl, _ := h.claimedCheckout(8, h.shaPR8)
			crash := errors.New("crash")
			ctx := steps.WithFailpoint(h.ctx, func(subject, name string, at steps.Point) error {
				if name == "fetch_base" && at == steps.BeforeRun {
					return crash
				}
				return nil
			})
			if err := h.m.Release(ctx, sl, h.pool, "pr_closed"); !errors.Is(err, crash) {
				t.Fatalf("err = %v, want crash", err)
			}
			if got := h.slot(sl.Name); got.State != store.SlotReleasing {
				t.Fatalf("state after the crash = %s", got.State)
			}
			if !human {
				if err := h.m.Release(h.ctx, h.slot(sl.Name), h.pool, "pr_closed"); err != nil {
					t.Fatalf("resume: %v", err)
				}
				if got := h.slot(sl.Name); got.State != store.SlotFree {
					t.Fatalf("state = %s", got.State)
				}
				if head := gitT(t, sl.Path, "rev-parse", "HEAD"); head != h.shaMaster {
					t.Fatalf("HEAD = %s, want master", head)
				}
				return
			}
			head := humanCommit(t, sl.Path)
			h.run.reset()
			wantHold(t, h.m.Release(h.ctx, h.slot(sl.Name), h.pool, "pr_closed"), HoldHeadDrift)
			h.wantNoGit(t, "switch")
			h.wantNoGit(t, "fetch")
			if got := gitT(t, sl.Path, "rev-parse", "HEAD"); got != head {
				t.Fatalf("HEAD = %s, want the human commit %s", got, head)
			}
			if got := h.slot(sl.Name); got.State != store.SlotReleasing || store.Deref(got.HoldReason) != HoldHeadDrift {
				t.Fatalf("state %s hold %v", got.State, store.Deref(got.HoldReason))
			}
		})
	}
}

// dirt is what a human leaves in a slot between two removal attempts.
var dirt = []struct {
	name, file, content string
}{
	{"tracked change", "README", "human edit\n"},
	{"untracked file", "notes.txt", "scratch\n"},
}

// A human (here their agent) who left changes between two removal attempts
// holds the resumed removal.
func TestRemoveResumeRunsTheGuardFirst(t *testing.T) {
	for _, d := range dirt {
		t.Run(d.name, func(t *testing.T) {
			h := newHarness(t)
			sl := h.provisioned(1)
			h.failScript["bin/worktree-archive"] = 1
			err := h.m.Remove(h.ctx, sl, h.pool, false)
			if _, held := AsHold(err); err == nil || held {
				t.Fatalf("err = %v, want the teardown failure", err)
			}
			if got := h.slot(sl.Name); got.State != store.SlotRemoving {
				t.Fatalf("state after the failure = %s", got.State)
			}
			file := filepath.Join(sl.Path, d.file)
			writeFile(t, file, d.content)
			h.foreignAgent(sl.Path)
			h.run.reset()
			h.clearScripts()

			wantHold(t, h.m.Remove(h.ctx, h.slot(sl.Name), h.pool, false), HoldDirtyWorktree)
			h.wantNoGit(t, "worktree", "remove")
			if n := len(h.scriptCalls("")); n != 0 {
				t.Fatalf("ran %d scripts before the guard", n)
			}
			if c := readFile(t, file); c != d.content {
				t.Fatalf("%s = %q", d.file, c)
			}
			if got := h.slot(sl.Name); got.State != store.SlotRemoving || store.Deref(got.HoldReason) != HoldDirtyWorktree {
				t.Fatalf("state %s hold %v", got.State, store.Deref(got.HoldReason))
			}

			// force removes it all the same.
			if err := h.m.Remove(h.ctx, h.slot(sl.Name), h.pool, true); err != nil {
				t.Fatalf("forced Remove: %v", err)
			}
			if rm := h.run.gitCalls("worktree", "remove"); len(rm) != 1 || !slices.Contains(rm[0].Args, "--force") {
				t.Fatalf("worktree remove = %v", rm)
			}
			if got := h.slot(sl.Name); got.State != store.SlotRemoved || fsx.Exists(sl.Path) {
				t.Fatalf("state %s, dir exists %v", got.State, fsx.Exists(sl.Path))
			}
		})
	}
}

// A human (here typing into the PR's panes) who left changes between two
// removal attempts holds the resumed removal.
func TestRemovePRWorktreeResumeRunsTheGuardFirst(t *testing.T) {
	for _, d := range dirt {
		t.Run(d.name, func(t *testing.T) {
			f := newPerPR(t, true)
			h := f.h
			h.m = h.newManager(Deps{Repos: []config.Repo{{Repo: f.repoName(), Teardown: []string{"bin/teardown"}}}})
			sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
			if err != nil {
				t.Fatal(err)
			}
			crash := errors.New("crash")
			ctx := steps.WithFailpoint(h.ctx, func(subject, name string, at steps.Point) error {
				if name == "worktree_remove" && at == steps.BeforeRun {
					return crash
				}
				return nil
			})
			if err := h.m.RemovePRWorktree(ctx, sl, false); !errors.Is(err, crash) {
				t.Fatalf("err = %v, want crash", err)
			}
			if got := h.slot(sl.Name); got.State != store.SlotRemoving {
				t.Fatalf("state after the crash = %s", got.State)
			}
			file := filepath.Join(sl.Path, d.file)
			writeFile(t, file, d.content)
			h.humanTyped(f.pr.ID, h.now.Add(time.Minute))
			h.run.reset()
			h.clearScripts()

			wantHold(t, h.m.RemovePRWorktree(h.ctx, h.slot(sl.Name), false), HoldDirtyWorktree)
			h.wantNoGit(t, "worktree", "remove")
			if c := readFile(t, file); c != d.content {
				t.Fatalf("%s = %q", d.file, c)
			}
			if got := h.slot(sl.Name); got.State != store.SlotRemoving || store.Deref(got.HoldReason) != HoldDirtyWorktree {
				t.Fatalf("state %s hold %v", got.State, store.Deref(got.HoldReason))
			}

			if err := h.m.RemovePRWorktree(h.ctx, h.slot(sl.Name), true); err != nil {
				t.Fatalf("forced RemovePRWorktree: %v", err)
			}
			if got := h.slot(sl.Name); got.State != store.SlotRemoved || fsx.Exists(sl.Path) {
				t.Fatalf("state %s, dir exists %v", got.State, fsx.Exists(sl.Path))
			}
		})
	}
}

// Without a human, what is left in the tree when a removal resumes is
// magnum's residue: removed with --force after a slot.discarded event.
func TestRemoveResumeDiscardsResidue(t *testing.T) {
	for _, d := range dirt {
		t.Run("pool "+d.name, func(t *testing.T) {
			h := newHarness(t)
			sl := h.provisioned(1)
			h.failScript["bin/worktree-archive"] = 1
			if err := h.m.Remove(h.ctx, sl, h.pool, false); err == nil {
				t.Fatal("Remove succeeded with a failing teardown")
			}
			writeFile(t, filepath.Join(sl.Path, d.file), d.content)
			h.run.reset()
			if err := h.m.Remove(h.ctx, h.slot(sl.Name), h.pool, false); err != nil {
				t.Fatalf("resumed Remove over residue: %v", err)
			}
			if rm := h.run.gitCalls("worktree", "remove"); len(rm) != 1 || !slices.Contains(rm[0].Args, "--force") {
				t.Fatalf("worktree remove = %v", rm)
			}
			if got := h.slot(sl.Name); got.State != store.SlotRemoved || fsx.Exists(sl.Path) {
				t.Fatalf("state %s, dir exists %v", got.State, fsx.Exists(sl.Path))
			}
			h.wantDiscarded(t, sl.Name, d.file)
		})
		t.Run("per-PR "+d.name, func(t *testing.T) {
			f := newPerPR(t, true)
			h := f.h
			sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
			if err != nil {
				t.Fatal(err)
			}
			// Typed before the checkout: not evidence about this tree.
			h.humanTyped(f.pr.ID, h.now.Add(-time.Minute))
			writeFile(t, filepath.Join(sl.Path, d.file), d.content)
			h.run.reset()
			if err := h.m.RemovePRWorktree(h.ctx, sl, false); err != nil {
				t.Fatalf("RemovePRWorktree over residue: %v", err)
			}
			if rm := h.run.gitCalls("worktree", "remove"); len(rm) != 1 || !slices.Contains(rm[0].Args, "--force") {
				t.Fatalf("worktree remove = %v", rm)
			}
			if got := h.slot(sl.Name); got.State != store.SlotRemoved || fsx.Exists(sl.Path) {
				t.Fatalf("state %s, dir exists %v", got.State, fsx.Exists(sl.Path))
			}
			h.wantDiscarded(t, sl.Name, d.file)
		})
	}
}

func TestReleaseRefusesAnIdleForeignAgent(t *testing.T) {
	h := newHarness(t)
	sl, pr := h.claimedCheckout(8, h.shaPR8)
	h.foreignAgent(sl.Path)
	h.run.reset()
	wantHold(t, h.m.Release(h.ctx, sl, h.pool, "pr_closed"), HoldForeignAgent)
	h.wantNoGit(t, "switch")
	got := h.slot(sl.Name)
	if got.State != store.SlotClaimed || store.Deref(got.PRID) != pr.ID || got.HoldReason != nil {
		t.Fatalf("state %s pr %v hold %v; want claimed, nothing persisted", got.State, got.PRID, store.Deref(got.HoldReason))
	}
	if head := gitT(t, sl.Path, "rev-parse", "HEAD"); head != h.shaPR8 {
		t.Fatalf("HEAD = %s", head)
	}
}

func TestReleaseRefusesHeadDrift(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.claimedCheckout(8, h.shaPR8)
	head := humanCommit(t, sl.Path)
	h.run.reset()
	wantHold(t, h.m.Release(h.ctx, sl, h.pool, "pr_closed"), HoldHeadDrift)
	h.wantNoGit(t, "switch")
	if got := gitT(t, sl.Path, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD = %s, want the human commit", got)
	}
	if got := h.slot(sl.Name); got.State != store.SlotClaimed || store.Deref(got.HoldReason) != HoldHeadDrift {
		t.Fatalf("state %s hold %v", got.State, store.Deref(got.HoldReason))
	}
}

func TestRemoveRefusesDriftAndForeignAgents(t *testing.T) {
	t.Run("pool head drift", func(t *testing.T) {
		h := newHarness(t)
		sl, _ := h.claimedCheckout(8, h.shaPR8)
		// A slot left broken while it held a checkout.
		if err := h.st.TransitionSlot(h.ctx, sl.ID, nil, store.SlotBroken, nil); err != nil {
			t.Fatal(err)
		}
		head := humanCommit(t, sl.Path)
		h.run.reset()
		h.clearScripts()
		wantHold(t, h.m.Remove(h.ctx, h.slot(sl.Name), h.pool, false), HoldHeadDrift)
		h.wantNoGit(t, "worktree", "remove")
		if n := len(h.scriptCalls("")); n != 0 {
			t.Fatalf("teardown ran (%d scripts)", n)
		}
		if got := gitT(t, sl.Path, "rev-parse", "HEAD"); got != head {
			t.Fatalf("HEAD = %s", got)
		}
		if got := h.slot(sl.Name); got.State != store.SlotBroken || store.Deref(got.HoldReason) != HoldHeadDrift {
			t.Fatalf("state %s hold %v", got.State, store.Deref(got.HoldReason))
		}
	})
	t.Run("pool foreign agent", func(t *testing.T) {
		h := newHarness(t)
		sl := h.provisioned(1)
		h.foreignAgent(sl.Path)
		h.run.reset()
		h.clearScripts()
		wantHold(t, h.m.Remove(h.ctx, sl, h.pool, false), HoldForeignAgent)
		h.wantNoGit(t, "worktree", "remove")
		if n := len(h.scriptCalls("")); n != 0 {
			t.Fatalf("teardown ran (%d scripts)", n)
		}
		if got := h.slot(sl.Name); got.State != store.SlotFree || got.HoldReason != nil || !fsx.Exists(sl.Path) {
			t.Fatalf("state %s hold %v dir %v", got.State, store.Deref(got.HoldReason), fsx.Exists(sl.Path))
		}
	})
	perPR := func(t *testing.T) (*perPRFixture, store.Slot) {
		f := newPerPR(t, true)
		f.h.m = f.h.newManager(Deps{Repos: []config.Repo{{Repo: f.repoName(), Teardown: []string{"bin/teardown"}}}})
		sl, err := f.h.m.CreatePRWorktree(f.h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
		if err != nil {
			t.Fatal(err)
		}
		f.h.run.reset()
		f.h.clearScripts()
		return f, sl
	}
	untouched := func(t *testing.T, h *harness, sl store.Slot, hold string) {
		t.Helper()
		h.wantNoGit(t, "worktree", "remove")
		if n := len(h.scriptCalls("bin/teardown")); n != 0 {
			t.Fatalf("teardown ran %d times", n)
		}
		got := h.slot(sl.Name)
		if got.State != store.SlotClaimed || store.Deref(got.HoldReason) != hold || !fsx.Exists(sl.Path) {
			t.Fatalf("state %s hold %v dir %v", got.State, store.Deref(got.HoldReason), fsx.Exists(sl.Path))
		}
	}
	t.Run("per-PR head drift", func(t *testing.T) {
		f, sl := perPR(t)
		humanCommit(t, sl.Path)
		wantHold(t, f.h.m.RemovePRWorktree(f.h.ctx, sl, false), HoldHeadDrift)
		untouched(t, f.h, sl, HoldHeadDrift)
	})
	t.Run("per-PR foreign agent", func(t *testing.T) {
		f, sl := perPR(t)
		f.h.foreignAgent(sl.Path)
		wantHold(t, f.h.m.RemovePRWorktree(f.h.ctx, sl, false), HoldForeignAgent)
		untouched(t, f.h, sl, "")
	})
}

func TestRemoveNeverTakesABusySlot(t *testing.T) {
	t.Run("pool", func(t *testing.T) {
		h := newHarness(t)
		sl, _ := h.claimedCheckout(8, h.shaPR8)
		if err := h.st.TransitionSlot(h.ctx, sl.ID, nil, store.SlotBusy, nil); err != nil {
			t.Fatal(err)
		}
		h.run.reset()
		h.clearScripts()
		for _, force := range []bool{false, true} {
			if err := h.m.Remove(h.ctx, h.slot(sl.Name), h.pool, force); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("Remove(force=%v) of a busy slot: err = %v, want ErrConflict", force, err)
			}
		}
		h.wantNoGit(t, "worktree", "remove")
		if n := len(h.scriptCalls("")); n != 0 {
			t.Fatalf("ran %d scripts", n)
		}
		if got := h.slot(sl.Name); got.State != store.SlotBusy || !fsx.Exists(sl.Path) {
			t.Fatalf("state %s dir %v", got.State, fsx.Exists(sl.Path))
		}
	})
	t.Run("per-PR", func(t *testing.T) {
		f := newPerPR(t, true)
		h := f.h
		sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.st.TransitionSlot(h.ctx, sl.ID, nil, store.SlotBusy, nil); err != nil {
			t.Fatal(err)
		}
		h.run.reset()
		for _, force := range []bool{false, true} {
			if err := h.m.RemovePRWorktree(h.ctx, h.slot(sl.Name), force); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("RemovePRWorktree(force=%v) of a busy slot: err = %v, want ErrConflict", force, err)
			}
		}
		h.wantNoGit(t, "worktree", "remove")
		if got := h.slot(sl.Name); got.State != store.SlotBusy || !fsx.Exists(sl.Path) {
			t.Fatalf("state %s dir %v", got.State, fsx.Exists(sl.Path))
		}
	})
}

func TestRepairClosesTheOpenAssignment(t *testing.T) {
	h := newHarness(t)
	sl, pr := h.claimedCheckout(8, h.shaPR8)
	// Broken while it held PR 8 (pr_id and the open assignment are still there).
	if err := h.st.TransitionSlot(h.ctx, sl.ID, nil, store.SlotBroken, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Repair(h.ctx, h.slot(sl.Name), h.pool); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	got := h.slot(sl.Name)
	if got.State != store.SlotFree || got.PRID != nil || got.CheckedOutSHA != nil {
		t.Fatalf("repaired slot = state %s pr %v sha %v", got.State, got.PRID, store.Deref(got.CheckedOutSHA))
	}
	if _, err := h.st.OpenAssignmentBySlot(h.ctx, sl.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("open assignment after Repair: %v", err)
	}
	as, err := h.st.AssignmentsByPR(h.ctx, pr.ID)
	if err != nil || len(as) != 1 || as[0].EndedAt == nil || store.Deref(as[0].EndReason) != "repaired" {
		t.Fatalf("assignments = %+v, %v", as, err)
	}
	other := h.pr(h.repo().ID, 7, h.shaPR7, store.PRQueued)
	claimed, err := h.m.Claim(h.ctx, other, h.pool)
	if err != nil || claimed.ID != sl.ID || store.Deref(claimed.PRID) != other.ID {
		t.Fatalf("Claim after Repair = %+v, %v", claimed, err)
	}
}

func TestRepairRefusesPinnedAndHeldSlots(t *testing.T) {
	h := newHarness(t)
	sl := h.provisioned(1)
	if err := h.st.TransitionSlot(h.ctx, sl.ID, nil, store.SlotBroken, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Pin(h.ctx, sl); err != nil {
		t.Fatal(err)
	}
	h.run.reset()
	h.clearScripts()
	nothingRan := func(what string) {
		t.Helper()
		if len(h.run.calls) != 0 || len(h.scriptCalls("")) != 0 {
			t.Fatalf("%s: ran %v", what, h.run.calls)
		}
		if got := h.slot(sl.Name); got.State != store.SlotBroken {
			t.Fatalf("%s: state = %s", what, got.State)
		}
	}
	wantHold(t, h.m.Repair(h.ctx, h.slot(sl.Name), h.pool), HoldPinned)
	nothingRan("pinned")

	if err := h.st.UpdateSlotFields(h.ctx, sl.ID, func(u *store.SlotUpdate) {
		u.Set("pinned", false)
		u.Set("hold_reason", HoldDirtyWorktree)
	}); err != nil {
		t.Fatal(err)
	}
	wantHold(t, h.m.Repair(h.ctx, h.slot(sl.Name), h.pool), HoldDirtyWorktree)
	nothingRan("held")
}

func TestAdoptRevivesARowWithAnOpenAssignment(t *testing.T) {
	for _, state := range []string{store.SlotLost, store.SlotBroken} {
		t.Run(state, func(t *testing.T) {
			h := newHarness(t)
			sl, pr := h.claimedCheckout(8, h.shaPR8)
			if err := h.st.TransitionSlot(h.ctx, sl.ID, nil, state, nil); err != nil {
				t.Fatal(err)
			}
			got, err := h.m.Adopt(h.ctx, h.pool, sl.Path)
			if err != nil {
				t.Fatalf("Adopt: %v", err)
			}
			lock, err := lockHash(sl.Path)
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != sl.ID || got.State != store.SlotFree || got.PRID != nil || store.Deref(got.LockSHA) != lock || got.CheckedOutSHA != nil {
				t.Fatalf("adopted = %+v", got)
			}
			as, err := h.st.AssignmentsByPR(h.ctx, pr.ID)
			if err != nil || len(as) != 1 || as[0].EndedAt == nil || store.Deref(as[0].EndReason) != "adopted" {
				t.Fatalf("assignments = %+v, %v", as, err)
			}
			other := h.pr(h.repo().ID, 7, h.shaPR7, store.PRQueued)
			if claimed, err := h.m.Claim(h.ctx, other, h.pool); err != nil || claimed.ID != sl.ID {
				t.Fatalf("Claim after Adopt = %+v, %v", claimed, err)
			}
		})
	}
}

// A crash between the per-PR claim's two writes (slot claimed, assignment
// not yet open) is repaired by calling CreatePRWorktree again.
func TestCreatePRWorktreeReopensAMissingAssignment(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
	if err != nil {
		t.Fatal(err)
	}
	a, err := h.openAssignment(f.pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.DB().ExecContext(h.ctx, "UPDATE assignments SET ended_at = ?, end_reason = 'crash' WHERE id = ?",
		store.FormatTime(time.Now()), a.ID); err != nil {
		t.Fatal(err)
	}
	h.run.reset()
	again, err := h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
	if err != nil || again.ID != sl.ID || again.State != store.SlotClaimed {
		t.Fatalf("second CreatePRWorktree = %+v, %v", again, err)
	}
	h.wantNoGit(t, "worktree", "add")
	b, err := h.openAssignment(f.pr.ID)
	if err != nil || b.ID == a.ID || b.SlotID != sl.ID || b.Path != sl.Path || store.Deref(b.HeadSHA) != store.Deref(sl.CheckedOutSHA) ||
		store.Deref(b.HeadSHA) != f.sha7 {
		t.Fatalf("reopened assignment = %+v, %v; want slot %d at %s", b, err, sl.ID, f.sha7)
	}
}

// A worktree kept after a failed setup is brought to the PR's new head on
// the next attempt, unless a human has changed it.
func TestCreatePRWorktreeResyncsARetainedWorktree(t *testing.T) {
	for _, tc := range []struct {
		name          string
		edited, human bool
	}{{name: "clean"}, {name: "residue", edited: true}, {name: "human edit", edited: true, human: true}} {
		edited, human := tc.edited, tc.human
		t.Run(tc.name, func(t *testing.T) {
			f := newPerPR(t, true)
			h := f.h
			work := f.ownOrigin(t)
			h.m = h.newManager(Deps{Repos: []config.Repo{{Repo: f.repoName(), Setup: []string{"bin/setup"}}}})
			h.failScript["bin/setup"] = 1
			if _, err := h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, f.sha7); !errors.Is(err, ErrBroken) {
				t.Fatalf("err = %v, want ErrBroken", err)
			}
			sl := h.slot(PRSlotName(f.repoName(), 7))
			if sl.State != store.SlotBroken || gitT(t, sl.Path, "rev-parse", "HEAD") != f.sha7 {
				t.Fatalf("after the failed setup: state %s", sl.State)
			}
			head := pushPR(t, work, 7, f.sha7, "NEWS", "news\n")
			readme := filepath.Join(sl.Path, "README")
			if edited {
				writeFile(t, readme, "human edit\n")
			}
			if human { // the worktree was never checked out for a round: any activity counts
				h.humanTyped(f.pr.ID, h.now.Add(-time.Hour))
			}
			h.run.reset()
			got, err := h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, head)
			h.wantNoGit(t, "worktree", "add")
			if human {
				wantHold(t, err, HoldDirtyWorktree)
				h.wantNoGit(t, "switch")
				if c := readFile(t, readme); c != "human edit\n" {
					t.Fatalf("README = %q", c)
				}
				if cur := gitT(t, sl.Path, "rev-parse", "HEAD"); cur != f.sha7 {
					t.Fatalf("HEAD = %s, want it untouched at %s", cur, f.sha7)
				}
				if cur := h.slot(sl.Name); cur.State == store.SlotClaimed || store.Deref(cur.HoldReason) != HoldDirtyWorktree {
					t.Fatalf("state %s hold %v", cur.State, store.Deref(cur.HoldReason))
				}
				return
			}
			if err != nil {
				t.Fatalf("CreatePRWorktree after the head moved: %v", err)
			}
			if sw := h.run.gitCalls("switch", "--quiet", "--discard-changes", "--detach"); len(sw) != 1 {
				t.Fatalf("switch calls = %v", sw)
			}
			if edited {
				h.wantDiscarded(t, sl.Name, "README")
			} else if evs := h.discarded(t, sl.Name); len(evs) != 0 {
				t.Fatalf("slot.discarded on a clean tree: %+v", evs)
			}
			if got.ID != sl.ID || got.State != store.SlotClaimed || store.Deref(got.CheckedOutSHA) != head || got.LastError != nil {
				t.Fatalf("slot = %+v", got)
			}
			if cur := gitT(t, sl.Path, "rev-parse", "HEAD"); cur != head || readFile(t, filepath.Join(sl.Path, "NEWS")) != "news\n" {
				t.Fatalf("HEAD = %s, want %s", cur, head)
			}
			if a, err := h.openAssignment(f.pr.ID); err != nil || store.Deref(a.HeadSHA) != head {
				t.Fatalf("assignment = %+v, %v", a, err)
			}
		})
	}
}

func TestRemovePRWorktreeWithCheckoutAndCloneGone(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, f.repoName(), f.pr, f.sha7)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{sl.Path, f.main} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	h.run.reset()
	if err := h.m.RemovePRWorktree(h.ctx, h.slot(sl.Name), false); err != nil {
		t.Fatalf("RemovePRWorktree: %v", err)
	}
	if len(h.run.calls) != 0 {
		t.Fatalf("ran commands with nothing left on disk: %v", h.run.calls)
	}
	got := h.slot(sl.Name)
	if got.State != store.SlotRemoved || got.PRID != nil {
		t.Fatalf("slot = %+v", got)
	}
	if _, err := h.openAssignment(f.pr.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("assignment still open: %v", err)
	}
}

func TestNextSlotNumberSkipsARemovedSlotsPathThatIsBack(t *testing.T) {
	h := newHarness(t)
	sl := h.provisioned(1)
	if err := h.m.Remove(h.ctx, sl, h.pool, false); err != nil {
		t.Fatal(err)
	}
	if n, err := h.m.NextSlotNumber(h.ctx, h.pool); err != nil || n != 1 {
		t.Fatalf("NextSlotNumber = %d, %v; want the removed slot's number", n, err)
	}
	writeFile(t, filepath.Join(sl.Path, "mine.txt"), "a human's\n")
	if n, err := h.m.NextSlotNumber(h.ctx, h.pool); err != nil || n != 2 {
		t.Fatalf("NextSlotNumber = %d, %v; want 2 (review1's path is back)", n, err)
	}
	if err := h.m.ProvisionPool(h.ctx, h.pool, 1); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("ProvisionPool over the directory: err = %v, want ErrConflict", err)
	}
	if got := h.slot(sl.Name); got.State != store.SlotRemoved {
		t.Fatalf("state = %s", got.State)
	}
	if c := readFile(t, filepath.Join(sl.Path, "mine.txt")); c != "a human's\n" {
		t.Fatalf("mine.txt = %q", c)
	}
}

func TestProvisionPoolRefusesAnExistingPath(t *testing.T) {
	h := newHarness(t)
	path := h.pool.Path(3)
	writeFile(t, filepath.Join(path, "mine.txt"), "a human's\n")
	h.run.reset()
	if err := h.m.ProvisionPool(h.ctx, h.pool, 3); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if _, err := h.st.SlotByName(h.ctx, h.pool.Slot(3)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("slot row created: %v", err)
	}
	if len(h.run.calls) != 0 || len(h.scriptCalls("")) != 0 {
		t.Fatalf("ran %v", h.run.calls)
	}
	if c := readFile(t, filepath.Join(path, "mine.txt")); c != "a human's\n" {
		t.Fatalf("mine.txt = %q", c)
	}
}
