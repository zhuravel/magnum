package slots

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

func TestGuardHerdrCases(t *testing.T) {
	h := newHarness(t)
	sl := h.provisioned(1)
	pr := h.pr(h.repo().ID, 7, h.shaPR7, store.PRQueued)
	if _, err := h.st.CreateSession(h.ctx, store.Session{PRID: pr.ID, Role: store.RoleCodexReview,
		HerdrWorkspaceID: new("wOurs"), HerdrPaneID: new("wOurs:p3"), State: store.SessionLive}); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(sl.Path, "app", "models")
	busy := herdr.ProcessInfo{ShellPID: 10, ForegroundPGID: 22, Foreground: []herdr.Process{{PID: 22, Name: "ruby", Argv: []string{"bin/rails", "c"}}}}

	cases := []struct {
		name   string
		panes  []herdr.Pane
		agents []herdr.AgentInfo
		procs  map[string]herdr.ProcessInfo
		snapEr error
		hold   string // "" = allowed
		isErr  bool
	}{
		{name: "empty session"},
		{
			name:   "foreign agent working",
			panes:  []herdr.Pane{{ID: "w1:p1", WorkspaceID: "w1", Cwd: sl.Path, Agent: "codex", AgentStatus: herdr.StatusWorking}},
			agents: []herdr.AgentInfo{{PaneID: "w1:p1", Agent: "codex", AgentStatus: herdr.StatusWorking, Cwd: sl.Path}},
			hold:   HoldForeignAgent,
		},
		{
			name:   "foreign agent blocked in a subdirectory",
			agents: []herdr.AgentInfo{{PaneID: "w1:p1", Name: "bohdan", Agent: "claude", AgentStatus: herdr.StatusBlocked, Cwd: sub}},
			hold:   HoldForeignAgent,
		},
		{
			// A human's idle session still holds the slot.
			name:   "foreign agent idle",
			panes:  []herdr.Pane{{ID: "w1:p1", WorkspaceID: "w1", Cwd: sl.Path, Agent: "codex", AgentStatus: herdr.StatusIdle}},
			agents: []herdr.AgentInfo{{PaneID: "w1:p1", Agent: "codex", AgentStatus: herdr.StatusIdle, Cwd: sl.Path}},
			procs:  map[string]herdr.ProcessInfo{"w1:p1": busy},
			hold:   HoldForeignAgent,
		},
		{
			name:  "idle agent pane missing from the agents list",
			panes: []herdr.Pane{{ID: "w1:p4", WorkspaceID: "w1", Cwd: sub, Agent: "claude"}},
			hold:  HoldForeignAgent,
		},
		{
			name:   "magnum agent working",
			panes:  []herdr.Pane{{ID: "w2:p1", WorkspaceID: "w2", Cwd: sl.Path, Agent: "codex", AgentStatus: herdr.StatusWorking}},
			agents: []herdr.AgentInfo{{PaneID: "w2:p1", Name: "mg-talkable-7-judge", Agent: "codex", AgentStatus: herdr.StatusWorking, Cwd: sl.Path}},
		},
		{
			name:  "shell running a foreground process",
			panes: []herdr.Pane{{ID: "w1:p2", WorkspaceID: "w1", Cwd: sub, AgentStatus: herdr.StatusUnknown}},
			procs: map[string]herdr.ProcessInfo{"w1:p2": busy},
			hold:  HoldForegroundProcess,
		},
		{
			name:  "idle shell",
			panes: []herdr.Pane{{ID: "w1:p2", WorkspaceID: "w1", Cwd: sl.Path, AgentStatus: herdr.StatusUnknown}},
		},
		{
			name:  "foreground cwd under the slot",
			panes: []herdr.Pane{{ID: "w1:p2", WorkspaceID: "w1", Cwd: "/tmp", ForegroundCwd: sl.Path}},
			procs: map[string]herdr.ProcessInfo{"w1:p2": busy},
			hold:  HoldForegroundProcess,
		},
		{
			name:   "sibling slot with a shared prefix is ignored",
			panes:  []herdr.Pane{{ID: "w1:p2", WorkspaceID: "w1", Cwd: sl.Path + "0"}},
			agents: []herdr.AgentInfo{{PaneID: "w1:p9", Agent: "codex", AgentStatus: herdr.StatusWorking, Cwd: sl.Path + "0/x"}},
			procs:  map[string]herdr.ProcessInfo{"w1:p2": busy},
		},
		{
			name:  "pane of a magnum session",
			panes: []herdr.Pane{{ID: "wOurs:p3", WorkspaceID: "wOurs", Cwd: sl.Path}},
			procs: map[string]herdr.ProcessInfo{"wOurs:p3": busy},
		},
		{
			name:  "other pane in a magnum workspace",
			panes: []herdr.Pane{{ID: "wOurs:p9", WorkspaceID: "wOurs", Cwd: sl.Path}},
			procs: map[string]herdr.ProcessInfo{"wOurs:p9": busy},
		},
		{name: "herdr unavailable", snapEr: herdr.ErrUnavailable, isErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h.snap = herdr.Snapshot{Panes: tc.panes, Agents: tc.agents}
			h.snapErr = tc.snapEr
			h.procs = tc.procs
			if h.procs == nil {
				h.procs = map[string]herdr.ProcessInfo{}
			}
			err := h.m.Guard(h.ctx, sl)
			switch {
			case tc.isErr:
				var hold ErrHold
				if err == nil || errors.As(err, &hold) {
					t.Fatalf("err = %v, want a non-hold error", err)
				}
				if !errors.Is(err, herdr.ErrUnavailable) {
					t.Fatalf("err = %v, want it to wrap herdr.ErrUnavailable", err)
				}
			case tc.hold == "":
				if err != nil {
					t.Fatalf("Guard: %v", err)
				}
			default:
				wantHold(t, err, tc.hold)
				// Live holds are transient: nothing is persisted as hold_reason.
				if got := h.slot(sl.Name); got.HoldReason != nil {
					t.Fatalf("hold_reason persisted for a transient hold: %q", *got.HoldReason)
				}
			}
		})
	}
}

func TestGuardHeadDriftPersistsHold(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.claimedCheckout(8, h.shaPR8)
	if store.Deref(sl.CheckedOutSHA) != h.shaPR8 {
		t.Fatalf("checked_out_sha = %v", store.Deref(sl.CheckedOutSHA))
	}
	if err := h.m.Guard(h.ctx, sl); err != nil {
		t.Fatalf("Guard on a clean checkout: %v", err)
	}
	// Someone commits in the slot.
	writeFile(t, filepath.Join(sl.Path, "README"), "human edit\n")
	gitT(t, sl.Path, "commit", "--quiet", "-am", "human")

	hold := wantHold(t, h.m.Guard(h.ctx, sl), HoldHeadDrift)
	if !strings.Contains(hold.Detail, h.shaPR8[:7]) {
		t.Fatalf("detail %q does not name the expected sha", hold.Detail)
	}
	got := h.slot(sl.Name)
	if store.Deref(got.HoldReason) != HoldHeadDrift {
		t.Fatalf("hold_reason = %q, want head_drift", store.Deref(got.HoldReason))
	}
	// The persisted hold keeps refusing, and blocks claims.
	wantHold(t, h.m.Guard(h.ctx, got), HoldHeadDrift)

	// Unpin acknowledges the drift: hold cleared, HEAD becomes the new baseline.
	if err := h.m.Unpin(h.ctx, got); err != nil {
		t.Fatal(err)
	}
	got = h.slot(sl.Name)
	if got.HoldReason != nil || got.Pinned {
		t.Fatalf("after Unpin: hold=%v pinned=%v", store.Deref(got.HoldReason), got.Pinned)
	}
	head := gitT(t, sl.Path, "rev-parse", "HEAD")
	if store.Deref(got.CheckedOutSHA) != head {
		t.Fatalf("checked_out_sha = %s, want new HEAD %s", store.Deref(got.CheckedOutSHA), head)
	}
	if err := h.m.Guard(h.ctx, got); err != nil {
		t.Fatalf("Guard after Unpin: %v", err)
	}
}

// Unpin clears the last_error the hold wrote ("slots: held: …", which
// `magnum slots` shows), and only that: another failure stays until the next
// operation clears it.
func TestUnpinClearsTheHoldsLastError(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.claimedCheckout(8, h.shaPR8)
	humanCommit(t, sl.Path)
	wantHold(t, h.m.Guard(h.ctx, sl), HoldHeadDrift)
	if got := h.slot(sl.Name); !strings.Contains(store.Deref(got.LastError), "held: head_drift") {
		t.Fatalf("last_error = %q, want the hold", store.Deref(got.LastError))
	}
	if err := h.m.Unpin(h.ctx, h.slot(sl.Name)); err != nil {
		t.Fatal(err)
	}
	if got := h.slot(sl.Name); got.LastError != nil || got.HoldReason != nil {
		t.Fatalf("after Unpin: last_error %q hold %v", store.Deref(got.LastError), store.Deref(got.HoldReason))
	}

	const other = "slots: deps review1: bundle install: exit 1"
	if err := h.st.UpdateSlotFields(h.ctx, sl.ID, func(u *store.SlotUpdate) {
		u.Set("pinned", true)
		u.Set("last_error", other)
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Unpin(h.ctx, h.slot(sl.Name)); err != nil {
		t.Fatal(err)
	}
	if got := h.slot(sl.Name); store.Deref(got.LastError) != other || got.Pinned {
		t.Fatalf("after Unpin: last_error %q pinned %v; want the failure kept", store.Deref(got.LastError), got.Pinned)
	}
}

func TestGuardUnpushedCommitsOnFreeSlot(t *testing.T) {
	h := newHarness(t)
	sl := h.provisioned(1)
	if err := h.m.Guard(h.ctx, sl); err != nil {
		t.Fatalf("Guard on a fresh slot: %v", err)
	}
	writeFile(t, filepath.Join(sl.Path, "README"), "human edit\n")
	gitT(t, sl.Path, "commit", "--quiet", "-am", "human")
	wantHold(t, h.m.Guard(h.ctx, sl), HoldUnpushed)
	if got := h.slot(sl.Name); store.Deref(got.HoldReason) != HoldUnpushed {
		t.Fatalf("hold_reason = %q", store.Deref(got.HoldReason))
	}
	// A held slot is not claimable.
	pr := h.pr(h.repo().ID, 7, h.shaPR7, store.PRQueued)
	if _, err := h.m.Claim(h.ctx, pr, h.pool); !errors.Is(err, ErrNoFreeSlot) {
		t.Fatalf("Claim of a held slot: err = %v, want ErrNoFreeSlot", err)
	}
}

// Release resets the placeholder branch (git switch -C) wherever HEAD is, so a
// free slot whose HEAD sits on a pushed commit but whose placeholder branch
// holds local commits must be held, or the reset would orphan them.
func TestGuardUnpushedCommitsOnPlaceholderBranch(t *testing.T) {
	// parkHead commits on the placeholder branch, then detaches HEAD onto the
	// pushed base, leaving that commit reachable only from the branch.
	parkHead := func(t *testing.T, h *harness, sl store.Slot) string {
		t.Helper()
		writeFile(t, filepath.Join(sl.Path, "README"), "human edit\n")
		gitT(t, sl.Path, "commit", "--quiet", "-am", "human")
		gitT(t, sl.Path, "switch", "--quiet", "--detach", "origin/"+h.pool.Base)
		return placeholder(sl)
	}

	t.Run("held", func(t *testing.T) {
		h := newHarness(t)
		sl := h.provisioned(1)
		branch := parkHead(t, h, sl)
		if n, err := h.m.git.Unpushed(h.ctx, sl.Path); err != nil || n != 0 {
			t.Fatalf("Unpushed(HEAD) = %d, %v; the fixture needs a pushed HEAD", n, err)
		}
		hold := wantHold(t, h.m.Guard(h.ctx, sl), HoldUnpushed)
		if !strings.Contains(hold.Detail, "branch "+branch) || !strings.Contains(hold.Detail, "1 commit(s)") {
			t.Fatalf("detail %q does not name the branch and its commits", hold.Detail)
		}
		if got := h.slot(sl.Name); store.Deref(got.HoldReason) != HoldUnpushed {
			t.Fatalf("hold_reason = %q, want %q", store.Deref(got.HoldReason), HoldUnpushed)
		}
		// Release refuses before it resets (and so before it orphans the commit).
		if err := h.m.Release(h.ctx, h.slot(sl.Name), h.pool, "test"); err == nil {
			t.Fatal("Release of a slot with an unpushed placeholder branch succeeded")
		}
		if gitT(t, sl.Path, "rev-list", "--count", "origin/"+h.pool.Base+".."+branch) != "1" {
			t.Fatal("the placeholder branch lost its commit")
		}
	})

	t.Run("branch gone", func(t *testing.T) {
		h := newHarness(t)
		sl := h.provisioned(1)
		branch := parkHead(t, h, sl)
		gitT(t, sl.Path, "branch", "-D", "--quiet", branch)
		if err := h.m.Guard(h.ctx, sl); err != nil {
			t.Fatalf("Guard with no placeholder branch: %v", err)
		}
	})

	t.Run("branch pushed", func(t *testing.T) {
		h := newHarness(t)
		sl := h.provisioned(1)
		branch := placeholder(sl)
		gitT(t, sl.Path, "switch", "--quiet", "--detach", "origin/"+h.pool.Base)
		if err := h.m.Guard(h.ctx, sl); err != nil {
			t.Fatalf("Guard with %s at the pushed base: %v", branch, err)
		}
	})

	t.Run("HEAD on the branch costs one rev-list", func(t *testing.T) {
		h := newHarness(t)
		sl := h.provisioned(1)
		h.run.reset()
		if err := h.m.Guard(h.ctx, sl); err != nil {
			t.Fatalf("Guard on a fresh slot: %v", err)
		}
		if got := len(h.run.gitCalls("rev-list")); got != 1 {
			t.Fatalf("rev-list ran %d times, want 1 (HEAD is the placeholder branch)", got)
		}
	})
}

func TestGuardPinnedAndManualHold(t *testing.T) {
	h := newHarness(t)
	sl := h.provisioned(1)
	if err := h.m.Pin(h.ctx, sl); err != nil {
		t.Fatal(err)
	}
	wantHold(t, h.m.Guard(h.ctx, sl), HoldPinned)
	if err := h.m.Unpin(h.ctx, sl); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Guard(h.ctx, sl); err != nil {
		t.Fatalf("Guard after Unpin: %v", err)
	}
	if err := h.st.UpdateSlotFields(h.ctx, sl.ID, func(u *store.SlotUpdate) { u.Set("hold_reason", "manual") }); err != nil {
		t.Fatal(err)
	}
	wantHold(t, h.m.Guard(h.ctx, sl), "manual")
}

// A review request hands a pinned slot back with ClearPin: the pin goes, a
// hold a guard persisted (a person's changes, commits or drift) stays, so the
// guard still refuses the slot and the head stays where the person left it.
func TestClearPinKeepsAPersistedHold(t *testing.T) {
	h := newHarness(t)
	sl := h.provisioned(1)
	if err := h.m.Pin(h.ctx, sl); err != nil {
		t.Fatal(err)
	}
	if err := h.st.UpdateSlotFields(h.ctx, sl.ID, func(u *store.SlotUpdate) { u.Set("hold_reason", HoldDirtyWorktree) }); err != nil {
		t.Fatal(err)
	}
	before := h.slot(sl.Name)
	if err := h.m.ClearPin(h.ctx, before); err != nil {
		t.Fatal(err)
	}
	after := h.slot(sl.Name)
	if after.Pinned || store.Deref(after.HoldReason) != HoldDirtyWorktree || store.Deref(after.CheckedOutSHA) != store.Deref(before.CheckedOutSHA) {
		t.Fatalf("after ClearPin: pinned %v hold %q checked_out %q (was %q)", after.Pinned, store.Deref(after.HoldReason),
			store.Deref(after.CheckedOutSHA), store.Deref(before.CheckedOutSHA))
	}
	wantHold(t, h.m.Guard(h.ctx, after), HoldDirtyWorktree)

	// Without a hold, the slot goes back to automation.
	if err := h.m.Unpin(h.ctx, after); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Pin(h.ctx, after); err != nil {
		t.Fatal(err)
	}
	if err := h.m.ClearPin(h.ctx, h.slot(sl.Name)); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Guard(h.ctx, h.slot(sl.Name)); err != nil {
		t.Fatalf("Guard after ClearPin of a slot without a hold: %v", err)
	}
}

func TestGuardMissingDirectorySkipsGit(t *testing.T) {
	h := newHarness(t)
	sl := store.Slot{Name: "ghost", Path: filepath.Join(h.root, "nope"), CheckedOutSHA: new(h.shaPR7)}
	if err := h.m.Guard(h.ctx, sl); err != nil {
		t.Fatalf("Guard on a missing dir: %v", err)
	}
}

// A human's agent in the PR's herdr workspace holds the slot even when its
// cwd is elsewhere; a workspace of another PR does not.
func TestGuardForeignAgentInThePRWorkspace(t *testing.T) {
	h := newHarness(t)
	sl, pr := h.claimedCheckout(8, h.shaPR8)
	other := h.pr(h.repo().ID, 7, h.shaPR7, store.PRQueued)
	for _, s := range []store.Session{
		{PRID: pr.ID, Role: store.RoleCodexReview, HerdrWorkspaceID: new("wPR8"), HerdrPaneID: new("wPR8:p1"), State: store.SessionLive},
		{PRID: other.ID, Role: store.RoleCodexReview, HerdrWorkspaceID: new("wPR7"), HerdrPaneID: new("wPR7:p1"), State: store.SessionLive},
	} {
		if _, err := h.st.CreateSession(h.ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	elsewhere := filepath.Join(h.root, "elsewhere")
	for _, tc := range []struct {
		ws   string
		hold bool
	}{{"wPR8", true}, {"wPR7", false}, {"w1", false}} {
		h.snap = herdr.Snapshot{
			Panes:  []herdr.Pane{{ID: tc.ws + ":p2", WorkspaceID: tc.ws, Cwd: elsewhere, Agent: "claude"}},
			Agents: []herdr.AgentInfo{{PaneID: tc.ws + ":p2", WorkspaceID: tc.ws, Agent: "claude", Name: "example", Cwd: elsewhere}},
		}
		err := h.m.Guard(h.ctx, sl)
		if !tc.hold {
			if err != nil {
				t.Fatalf("%s: Guard: %v", tc.ws, err)
			}
			continue
		}
		hold := wantHold(t, err, HoldForeignAgent)
		if !strings.Contains(hold.Detail, "workspace wPR8") {
			t.Fatalf("detail %q does not name the workspace", hold.Detail)
		}
		// The agents list may miss it: the pane alone holds too.
		h.snap.Agents = nil
		wantHold(t, h.m.Guard(h.ctx, sl), HoldForeignAgent)
	}
}

func TestChangesOf(t *testing.T) {
	entries := []gitx.StatusEntry{
		{X: ' ', Y: 'M', Path: "db/schema.rb"},
		{X: '?', Y: '?', Path: "notes with space.txt"},
		{X: 'R', Y: ' ', Path: "new.rb", OrigPath: "old.rb"},
		{X: '!', Y: '!', Path: "tmp/cache"},
		{X: 'A', Y: ' ', Path: "added.rb"},
	}
	ch := changesOf(entries)
	// A rename counts once, by its destination; ignored files not at all.
	if want := []string{"db/schema.rb", "new.rb", "added.rb"}; !slices.Equal(ch.tracked, want) {
		t.Fatalf("tracked = %q, want %q", ch.tracked, want)
	}
	if want := []string{"notes with space.txt"}; !slices.Equal(ch.untracked, want) {
		t.Fatalf("untracked = %q, want %q", ch.untracked, want)
	}
	if got := ch.describe(false); got != "3 tracked change(s)" {
		t.Fatalf("describe(false) = %q", got)
	}
	if got := ch.describe(true); got != "3 tracked change(s) and 1 untracked file(s)" {
		t.Fatalf("describe(true) = %q", got)
	}
	if got := changesOf(nil); len(got.paths(true)) != 0 {
		t.Fatalf("empty status = %+v", got)
	}
}

// worktreeChanges runs gitx.StatusPaths on a real work tree: a rename (staged
// with git mv) is one tracked change, a new file and a wholly untracked
// directory are untracked, and an ignored file is not a change.
func TestWorktreeChangesReal(t *testing.T) {
	h := newHarness(t)
	sl := h.provisioned(1)
	gitT(t, sl.Path, "mv", "README", "README.md")
	writeFile(t, filepath.Join(sl.Path, "notes with space.txt"), "x\n")
	writeFile(t, filepath.Join(sl.Path, "scratch", "a.txt"), "x\n")
	writeFile(t, filepath.Join(sl.Path, ".mise.local.toml"), "# ignored by the repo\n")
	h.run.reset()
	ch, err := h.m.worktreeChanges(h.ctx, sl.Path)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"README.md"}; !slices.Equal(ch.tracked, want) {
		t.Fatalf("tracked = %q, want %q", ch.tracked, want)
	}
	slices.Sort(ch.untracked)
	if want := []string{"notes with space.txt", "scratch/"}; !slices.Equal(ch.untracked, want) {
		t.Fatalf("untracked = %q, want %q", ch.untracked, want)
	}
	calls := h.run.gitCalls("status")
	if len(calls) != 1 || calls[0].Env["GIT_OPTIONAL_LOCKS"] != "0" || calls[0].Env["GIT_TERMINAL_PROMPT"] != "0" ||
		!slices.Equal(calls[0].Unset, gitx.ScrubbedEnv()) {
		t.Fatalf("status calls = %+v, want one read-only call with the scrubbed env", calls)
	}
}
