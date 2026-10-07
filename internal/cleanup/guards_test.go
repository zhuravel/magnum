package cleanup

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
)

// --- orphans need complete discovery ---

func TestOrphansUnknownWhenDiscoveryIncomplete(t *testing.T) {
	f := newFixture(t)
	f.orphan("review9", 1) // listed, but ownership facts are incomplete
	f.orphan("pr27087fix", 1)
	f.inv.inv.OrphansKnown = false

	unknown := func(w string) bool { return strings.Contains(w, "orphan databases unknown") }
	p := f.plan(Options{})
	if len(actionsOf(p, KindDropDBs)) != 0 || !slices.ContainsFunc(p.Warnings, unknown) {
		t.Fatalf("default plan: actions = %+v warnings = %q", p.Actions, p.Warnings)
	}
	if len(p.Skipped) != 0 {
		t.Fatalf("default plan skips = %+v (only a warning)", p.Skipped)
	}

	p = f.plan(Options{Orphans: true})
	if len(p.Actions) != 0 {
		t.Fatalf("--orphans actions = %+v", p.Actions)
	}
	if s := mustSkip(t, p, "orphans", SkipIncomplete); !unknown(s.Detail) {
		t.Fatalf("--orphans detail = %q", s.Detail)
	}

	for _, slug := range []string{"pr27087fix", "review9"} {
		p = f.plan(Options{Orphans: true, Slug: slug})
		if len(p.Actions) != 0 {
			t.Fatalf("--slug %s actions = %+v", slug, p.Actions)
		}
		mustSkip(t, p, "orphans", SkipIncomplete)
		mustSkip(t, p, "slug:"+slug, SkipIncomplete)
	}
}

// --- applyDrop re-scans ownership before dropping ---

func droppedBy(t *testing.T, f *fixture) map[string]string {
	t.Helper()
	dbs, err := f.st.ListSlotDatabases(f.ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, d := range dbs {
		if d.DroppedAt != nil {
			out[d.DBName] = store.Deref(d.DroppedBy)
		}
	}
	return out
}

func TestApplyDropRescansOrphans(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		f := newFixture(t)
		names := f.orphan("review9", 1)
		rep, err := f.apply(f.plan(Options{}), false)
		if err != nil || rep.Done != 1 {
			t.Fatalf("rep = %+v err = %v", rep, err)
		}
		if len(f.inv.calls) != 2 || f.inv.calls[1].External {
			t.Fatalf("apply must rescan (non-external) before dropping: %+v", f.inv.calls)
		}
		if len(f.mysql.calls) != 1 || !slices.Equal(f.mysql.calls[0], names) {
			t.Fatalf("drops = %v", f.mysql.calls)
		}
		got := droppedBy(t, f)
		if len(got) != len(names) {
			t.Fatalf("dropped rows = %v", got)
		}
		for _, n := range names {
			if got[n] != DroppedBy {
				t.Fatalf("%s dropped_by = %q, want %q", n, got[n], DroppedBy)
			}
		}
	})

	t.Run("no longer an orphan", func(t *testing.T) {
		f := newFixture(t)
		names := f.orphan("review9", 1)
		p := f.plan(Options{})
		f.inv.inv.OrphanDBs = f.inv.inv.OrphanDBs[1:] // names[0] got an owner since the plan
		rep, err := f.apply(p, false)
		if !errors.Is(err, store.ErrConflict) || rep.Failed != 1 || !strings.Contains(rep.Results[0].Error, names[0]) {
			t.Fatalf("rep = %+v err = %v", rep, err)
		}
		if len(f.mysql.calls) != 0 || len(droppedBy(t, f)) != 0 {
			t.Fatalf("dropped anyway: %v", f.mysql.calls)
		}
	})

	t.Run("discovery incomplete at apply", func(t *testing.T) {
		for name, mut := range map[string]func(*inventory.Inventory){
			"orphans unknown":  func(inv *inventory.Inventory) { inv.OrphansKnown = false },
			"mysql not listed": func(inv *inventory.Inventory) { inv.DatabasesListed = false },
			"both unknown":     func(inv *inventory.Inventory) { inv.OrphansKnown, inv.DatabasesListed = false, false },
		} {
			f := newFixture(t)
			f.orphan("review9", 1)
			p := f.plan(Options{})
			mut(&f.inv.inv)
			rep, err := f.apply(p, false)
			if err == nil || rep.Failed != 1 || !strings.Contains(rep.Results[0].Error, SkipIncomplete) {
				t.Fatalf("%s: rep = %+v err = %v", name, rep, err)
			}
			if len(f.mysql.calls) != 0 {
				t.Fatalf("%s: dropped anyway: %v", name, f.mysql.calls)
			}
		}
	})

	t.Run("name of another slug", func(t *testing.T) {
		f := newFixture(t)
		names := f.orphan("review9", 1)
		other := f.orphan("review8", 1) // an orphan too: the slug check alone refuses it
		for _, extra := range []string{other[0], "talkable_test"} {
			a := Action{Kind: KindDropDBs, Subject: "slug:review9", Slug: "review9", DBNames: append(slices.Clone(names), extra)}
			rep, err := f.apply(Plan{Actions: []Action{a}}, false)
			if !errors.Is(err, ErrOptions) || rep.Failed != 1 || !strings.Contains(rep.Results[0].Error, extra) {
				t.Fatalf("%s: rep = %+v err = %v", extra, rep, err)
			}
		}
		if len(f.mysql.calls) != 0 {
			t.Fatalf("dropped anyway: %v", f.mysql.calls)
		}
	})

	t.Run("no database names", func(t *testing.T) {
		f := newFixture(t)
		f.orphan("review9", 1)
		a := Action{Kind: KindDropDBs, Subject: "slug:review9", Slug: "review9"}
		rep, err := f.apply(Plan{Actions: []Action{a}}, false)
		if !errors.Is(err, ErrOptions) || rep.Failed != 1 {
			t.Fatalf("rep = %+v err = %v", rep, err)
		}
		if len(f.mysql.calls) != 0 || len(f.inv.calls) != 0 {
			t.Fatalf("drops = %v scans = %+v", f.mysql.calls, f.inv.calls)
		}
	})
}

func TestApplyDropRefusesSlugOfPerPRSlotName(t *testing.T) {
	// A per-PR slot has no db_slug: its slug is the sanitized slot name
	// (slots.SlotDBSlug), which still claims the databases.
	f := newFixture(t)
	slug := slots.DBSlug(slots.PRSlotName(f.foo.FullName(), 7))
	names := f.orphan(slug, 1)
	p := f.plan(Options{Orphans: true, Slug: slug})
	if d := actionsOf(p, KindDropDBs); len(d) != 1 || !d[0].Confirm || !slices.Equal(d[0].DBNames, names) {
		t.Fatalf("plan = %+v", p)
	}
	sl := f.prSlot(f.pr(f.foo, 7, store.PRReviewed, store.GHOpen, nil), store.SlotProvisioning, false)
	if sl.DBSlug != nil || slots.SlotDBSlug(sl) != slug {
		t.Fatalf("precondition: slot %s db_slug = %v, SlotDBSlug = %q, want %q", sl.Name, sl.DBSlug, slots.SlotDBSlug(sl), slug)
	}
	rep, err := f.apply(p, true)
	if !errors.Is(err, store.ErrConflict) || rep.Failed != 1 || !strings.Contains(rep.Results[0].Error, sl.Name) {
		t.Fatalf("rep = %+v err = %v", rep, err)
	}
	if len(f.mysql.calls) != 0 {
		t.Fatalf("dropped anyway: %v", f.mysql.calls)
	}
}

// --- external reset ---

func TestExternalResetNeedsAgentsListed(t *testing.T) {
	f := newFixture(t)
	ev := f.external("repo3", nil)
	f.inv.inv.AgentsListed = false
	for _, force := range []bool{false, true} {
		p := f.plan(Options{External: true, Slot: "repo3", Force: force})
		if len(p.Actions) != 0 {
			t.Fatalf("force=%v: actions = %+v", force, p.Actions)
		}
		if s := mustSkip(t, p, "path:"+ev.Path, SkipIncomplete); !strings.Contains(s.Detail, "herdr") {
			t.Fatalf("force=%v: detail = %q", force, s.Detail)
		}
	}

	// Planned while herdr answered, applied after it stopped answering.
	for _, force := range []bool{false, true} {
		f.inv.inv.AgentsListed = true
		p := f.plan(Options{External: true, Slot: "repo3", Force: force})
		if len(actionsOf(p, KindResetExternal)) != 1 {
			t.Fatalf("force=%v: plan = %+v", force, p)
		}
		f.inv.inv.AgentsListed = false
		rep, err := f.apply(p, false)
		if !errors.Is(err, store.ErrConflict) || rep.Failed != 1 || !strings.Contains(rep.Results[0].Error, SkipIncomplete) {
			t.Fatalf("force=%v: rep = %+v err = %v", force, rep, err)
		}
	}
	if len(f.git.calls) != 0 {
		t.Fatalf("git calls = %v", f.git.calls)
	}
}

func TestExternalResetPlaceholderBranchUnpushed(t *testing.T) {
	f := newFixture(t)
	ev := f.external("repo3", func(e *inventory.ExternalView) { e.Branch = "feature-x" })
	ref := ev.Path + " refs/heads/repo3"

	// The placeholder branch does not exist: nothing to orphan.
	if p := f.plan(Options{External: true, Slot: "repo3"}); len(actionsOf(p, KindResetExternal)) != 1 || len(p.Skipped) != 0 {
		t.Fatalf("no placeholder branch: plan = %+v", p)
	}
	// It exists with every commit pushed.
	f.git.branches[ref] = 0
	if p := f.plan(Options{External: true, Slot: "repo3"}); len(actionsOf(p, KindResetExternal)) != 1 {
		t.Fatalf("pushed placeholder branch: plan = %+v", p)
	}
	stalePlan := f.plan(Options{External: true, Slot: "repo3"})

	// Another branch is checked out; the reset force-moves repo3 and would orphan its 2 commits.
	f.git.branches[ref] = 2
	p := f.plan(Options{External: true, Slot: "repo3"})
	s := mustSkip(t, p, "path:"+ev.Path, SkipUnpushed)
	if !strings.Contains(s.Detail, "branch repo3") || !strings.Contains(s.Detail, "2 commits") {
		t.Fatalf("detail = %q", s.Detail)
	}
	if len(p.Actions) != 0 {
		t.Fatalf("actions = %+v", p.Actions)
	}
	p = f.plan(Options{External: true, Slot: "repo3", Force: true})
	if r := actionsOf(p, KindResetExternal); len(r) != 1 || !r[0].Force || r[0].Branch != "repo3" {
		t.Fatalf("forced plan = %+v", p)
	}

	// Apply re-checks the branch: the plan made before the commits refuses.
	rep, err := f.apply(stalePlan, false)
	if !errors.Is(err, store.ErrConflict) || rep.Failed != 1 || !strings.Contains(rep.Results[0].Error, "branch repo3") {
		t.Fatalf("rep = %+v err = %v", rep, err)
	}
	if len(f.git.calls) != 0 {
		t.Fatalf("git calls = %v", f.git.calls)
	}

	// A branch lookup that fails is a refusal, not a guess.
	f.git.errs["rev-parse "+ev.Path] = errBoom
	if s := mustSkip(t, f.plan(Options{External: true, Slot: "repo3"}), "path:"+ev.Path, SkipUnpushed); !strings.Contains(s.Detail, "could not count") {
		t.Fatalf("rev-parse failure detail = %q", s.Detail)
	}
}

func TestExternalResetDetachedChecksHeadAndBranch(t *testing.T) {
	f := newFixture(t)
	ev := f.external("repo5", func(e *inventory.ExternalView) { e.Detached, e.Branch = true, "" })

	f.git.unpushed[ev.Path] = 3
	if s := mustSkip(t, f.plan(Options{External: true, Slot: "repo5"}), "path:"+ev.Path, SkipUnpushed); !strings.Contains(s.Detail, "detached HEAD") {
		t.Fatalf("HEAD detail = %q", s.Detail)
	}
	f.git.unpushed[ev.Path] = 0
	f.git.branches[ev.Path+" refs/heads/repo5"] = 1
	if s := mustSkip(t, f.plan(Options{External: true, Slot: "repo5"}), "path:"+ev.Path, SkipUnpushed); !strings.Contains(s.Detail, "branch repo5") {
		t.Fatalf("branch detail = %q", s.Detail)
	}
	f.git.branches[ev.Path+" refs/heads/repo5"] = 0
	if p := f.plan(Options{External: true, Slot: "repo5"}); len(actionsOf(p, KindResetExternal)) != 1 {
		t.Fatalf("clean detached plan = %+v", p)
	}
}

func TestExternalResetIgnoresIdleAgent(t *testing.T) {
	// Unlike a managed slot (TestSlotGuardSkips), a manual worktree is only
	// blocked by a working or blocked agent: an idle human session is not.
	f := newFixture(t)
	ev := f.external("repo3", func(e *inventory.ExternalView) {
		e.Agents = []inventory.AgentView{{Name: "claude", Agent: "claude", Status: herdr.StatusIdle, PaneID: "p1"}}
	})
	p := f.plan(Options{External: true, Slot: "repo3"})
	if len(actionsOf(p, KindResetExternal)) != 1 || len(p.Skipped) != 0 {
		t.Fatalf("plan = %+v", p)
	}
	rep, err := f.apply(p, false)
	if err != nil || rep.Done != 1 {
		t.Fatalf("rep = %+v err = %v", rep, err)
	}
	want := []string{"fetch " + ev.MainClone + " master", "reset " + ev.Path + " repo3 master"}
	if !slices.Equal(f.git.calls, want) {
		t.Fatalf("git calls = %v", f.git.calls)
	}
}

// --- shrink capacity ---

func TestShrinkIgnoresUnusableSlots(t *testing.T) {
	f := newFixture(t)
	f.poolSlot("review1", store.SlotFree, 0, true, nil)
	f.poolSlot("review2", store.SlotFree, 0, true, nil)
	f.poolSlot("review3", store.SlotLost, 0, false, nil)
	f.poolSlot("review4", store.SlotLost, 0, false, nil)
	f.poolSlot("review5", store.SlotBroken, 0, true, nil)
	f.poolSlot("review6", store.SlotRemoving, 0, true, nil)

	// Only the two free slots hold capacity: pool.min 2 keeps both.
	if p := f.plan(Options{Shrink: store.Ptr(0)}); len(p.Actions) != 0 {
		t.Fatalf("shrink 0 over 2 usable slots = %+v", p.Actions)
	}
	f.poolSlot("review7", store.SlotFree, 0, true, nil)
	if rm := actionsOf(f.plan(Options{Shrink: store.Ptr(0)}), KindRemoveSlot); len(rm) != 1 {
		t.Fatalf("shrink 0 over 3 usable slots = %+v", rm)
	}
}

func TestShrinkCountsExplicitRemoval(t *testing.T) {
	f := newFixture(t)
	f.poolSlot("review1", store.SlotFree, 0, true, nil)
	f.poolSlot("review2", store.SlotFree, 0, true, nil)
	f.poolSlot("review3", store.SlotFree, 0, true, nil)

	p := f.plan(Options{Slot: "review2", Remove: true, Shrink: store.Ptr(2)})
	rm := actionsOf(p, KindRemoveSlot)
	if len(p.Actions) != 1 || len(rm) != 1 || rm[0].Slot != "review2" || rm[0].Why != "requested" {
		t.Fatalf("plan = %+v", p.Actions)
	}
}

// --- failed per-PR worktree of a closed PR ---

// unclaimedPRSlot registers the per-PR worktree row of pr that never got its
// pr_id (provisioning failed before the claim).
func (f *fixture) unclaimedPRSlot(pr store.PR, state string) store.Slot {
	f.t.Helper()
	return f.createSlot(store.Slot{
		Name: slots.PRSlotName(f.foo.FullName(), pr.Number), RepoID: &f.foo.ID, RepoFullName: f.foo.FullName(),
		Kind: store.SlotKindPerPR, Path: filepath.Join(f.root, "foo__worktrees", fmt.Sprintf("pr-%d", pr.Number)),
		MainClone: filepath.Join(f.root, "foo"), State: state,
	}, false)
}

func TestFailedPerPRWorktreeOfClosedPR(t *testing.T) {
	f := newFixture(t)
	pr := f.closedPR(f.foo, 8, store.GHClosed, -time.Minute)
	sl := f.unclaimedPRSlot(pr, store.SlotBroken)

	p := f.plan(Options{})
	rm := actionsOf(p, KindRemoveWorktree)
	if len(p.Actions) != 1 || len(rm) != 1 {
		t.Fatalf("plan = %+v (want a remove_worktree, not a slot-less release)", p.Actions)
	}
	if a := rm[0]; a.SlotID != sl.ID || a.Slot != sl.Name || a.PRID != pr.ID || a.Subject != "zhuravel/foo#8" || a.PRState != store.PRClosed {
		t.Fatalf("remove_worktree = %+v", a)
	}

	// A failed removal leaves the PR releasing; the next plan resumes it.
	f.slots.errs["remove_pr "+sl.Name] = errBoom
	if rep, err := f.apply(p, false); !errors.Is(err, errBoom) || rep.Failed != 1 {
		t.Fatalf("rep = %+v err = %v", rep, err)
	}
	if got := f.prState(pr.ID); got != store.PRReleasing {
		t.Fatalf("PR state after failed removal = %s, want releasing", got)
	}
	delete(f.slots.errs, "remove_pr "+sl.Name)
	p = f.plan(Options{})
	if rm := actionsOf(p, KindRemoveWorktree); len(p.Actions) != 1 || len(rm) != 1 || rm[0].SlotID != sl.ID {
		t.Fatalf("resume plan = %+v", p.Actions)
	}
	if rep, err := f.apply(p, false); err != nil || rep.Done != 1 {
		t.Fatalf("rep = %+v err = %v", rep, err)
	}
	want := []string{"remove_pr " + sl.Name + " force=false", "remove_pr " + sl.Name + " force=false"}
	if !slices.Equal(f.slots.calls, want) {
		t.Fatalf("slots calls = %v", f.slots.calls)
	}
	if got := f.prState(pr.ID); got != store.PRReleased {
		t.Fatalf("PR state = %s, want released", got)
	}
	if evs := f.events(prSubject(pr.ID)); len(evs) != 1 || evs[0].Kind != "cleanup.pr_released" {
		t.Fatalf("pr events = %+v", evs)
	}
}

func TestFailedPerPRWorktreeStates(t *testing.T) {
	for _, state := range []string{store.SlotProvisioning, store.SlotBroken, store.SlotLost, store.SlotRemoving} {
		f := newFixture(t)
		pr := f.closedPR(f.foo, 8, store.GHClosed, -time.Minute)
		sl := f.unclaimedPRSlot(pr, state)
		if rm := actionsOf(f.plan(Options{}), KindRemoveWorktree); len(rm) != 1 || rm[0].SlotID != sl.ID {
			t.Errorf("%s: remove_worktree = %+v", state, rm)
		}
	}
	// A free or claimed row without pr_id is not a failed checkout of the PR:
	// the PR gets a slot-less release and the row is left alone.
	for _, state := range []string{store.SlotFree, store.SlotClaimed} {
		f := newFixture(t)
		pr := f.closedPR(f.foo, 8, store.GHClosed, -time.Minute)
		f.unclaimedPRSlot(pr, state)
		p := f.plan(Options{})
		rel := actionsOf(p, KindRelease)
		if len(p.Actions) != 1 || len(rel) != 1 || rel[0].SlotID != 0 || rel[0].PRID != pr.ID {
			t.Errorf("%s: plan = %+v", state, p.Actions)
		}
	}
}

func TestApplyRefusesPerPRRowOfAnotherPR(t *testing.T) {
	f := newFixture(t)
	pr := f.closedPR(f.foo, 8, store.GHClosed, -time.Minute)
	f.unclaimedPRSlot(pr, store.SlotBroken)
	other := f.closedPR(f.foo, 9, store.GHClosed, -time.Minute)
	rm := actionsOf(f.plan(Options{}), KindRemoveWorktree)
	if len(rm) != 1 {
		t.Fatalf("remove_worktree = %+v", rm)
	}
	a := rm[0]
	a.PRID, a.Subject = other.ID, "zhuravel/foo#9"

	rep, err := f.apply(Plan{Actions: []Action{a}}, false)
	if !errors.Is(err, store.ErrConflict) || rep.Failed != 1 || !strings.Contains(rep.Results[0].Error, "does not belong") {
		t.Fatalf("rep = %+v err = %v", rep, err)
	}
	if len(f.slots.calls) != 0 || len(f.parked) != 0 {
		t.Fatalf("slots calls = %v parked = %v", f.slots.calls, f.parked)
	}
	for _, id := range []int64{pr.ID, other.ID} {
		if got := f.prState(id); got != store.PRClosed {
			t.Fatalf("PR %d state = %s, want closed", id, got)
		}
	}
}

// --- forced removal and the slots guard ---

func TestApplyForcedRemoveSlotGuardHolds(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"review1", "review2", "review3"} {
		f.poolSlot(name, store.SlotFree, 0, true, nil)
		f.dirty(name, 1, 2) // a human's changes: a removal needs --force
		f.slots.evidence[name] = "someone typed into its PR's magnum panes"
	}
	// review1's changes are what the force is for: the live guard passes it.
	f.slots.errs["guard_live review2"] = slots.ErrHold{Reason: slots.HoldForeignAgent, Detail: "pane p1"}
	f.slots.errs["guard_live review3"] = errBoom

	var plan Plan
	for _, name := range []string{"review1", "review2", "review3"} {
		mustSkip(t, f.plan(Options{Slot: name, Remove: true}), "slot:"+name, SkipDirty)
		rm := actionsOf(f.plan(Options{Slot: name, Remove: true, Force: true}), KindRemoveSlot)
		if len(rm) != 1 || !rm[0].Force {
			t.Fatalf("%s: forced remove = %+v", name, rm)
		}
		plan.Actions = append(plan.Actions, rm[0])
	}
	rep, err := f.apply(plan, false)
	if rep.Done != 1 || rep.Failed != 2 {
		t.Fatalf("rep = %+v err = %v", rep, err)
	}
	if h, ok := slots.AsHold(err); !ok || h.Reason != slots.HoldForeignAgent {
		t.Fatalf("err = %v, want the foreign_agent hold", err)
	}
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want the guard failure too", err)
	}
	want := []string{"guard_live review1", "remove review1 force=true", "guard_live review2", "guard_live review3"}
	if !slices.Equal(f.slots.calls, want) {
		t.Fatalf("slots calls = %v\nwant %v", f.slots.calls, want)
	}
}

// --- release with tracked changes ---

func TestApplyReleaseDirtiness(t *testing.T) {
	f := newFixture(t)
	pr := f.closedPR(f.talkable, 9, store.GHMerged, -time.Minute)
	f.poolSlot("review1", store.SlotHeld, pr.ID, true, nil)
	f.dirty("review1", 3, 0)
	f.slots.evidence["review1"] = "someone typed into PR #9's magnum panes"

	// A human's tracked changes: nothing to apply, even forced.
	if rep, err := f.apply(f.plan(Options{Force: true}), false); err != nil || len(rep.Results) != 0 {
		t.Fatalf("tracked: rep = %+v err = %v", rep, err)
	}
	if len(f.slots.calls) != 0 || f.prState(pr.ID) != store.PRClosed {
		t.Fatalf("tracked: calls = %v state = %s", f.slots.calls, f.prState(pr.ID))
	}

	// Untracked files only: released, and a forced plan does not bypass the
	// slots guard for a release (no Guard-then-force path).
	f.dirty("review1", 0, 4)
	p := f.plan(Options{Force: true})
	if rel := actionsOf(p, KindRelease); len(rel) != 1 || rel[0].Force {
		t.Fatalf("untracked: plan = %+v", p.Actions)
	}
	if rep, err := f.apply(p, false); err != nil || rep.Done != 1 {
		t.Fatalf("untracked: rep = %+v err = %v", rep, err)
	}
	if !slices.Equal(f.slots.calls, []string{"release review1 merged"}) {
		t.Fatalf("slots calls = %v", f.slots.calls)
	}
	if got := f.prState(pr.ID); got != store.PRReleased {
		t.Fatalf("PR state = %s", got)
	}
}
