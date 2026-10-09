package cleanup

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/store"
)

func TestPlanRequiresDeps(t *testing.T) {
	if _, err := (&Planner{}).Plan(t.Context(), Options{}); err == nil {
		t.Fatal("want error without Store/Slots/Inventory/Config")
	}
}

func TestPlanOptionValidation(t *testing.T) {
	f := newFixture(t)
	for _, opts := range []Options{
		{Remove: true},
		{Slug: "pr27087fix"},
		{External: true},
		{External: true, Slot: "repo3", Remove: true},
		{Orphans: true, Slug: "bad slug"},
		{PR: &PRRef{Repo: "talkable/talkable"}},
		{Shrink: new(-1)},
	} {
		_, err := f.p.Plan(f.ctx, opts)
		if !errors.Is(err, ErrOptions) {
			t.Errorf("Plan(%+v) = %v, want ErrOptions", opts, err)
		}
	}
}

func TestDefaultPlanReleasesClosedPRsPastGrace(t *testing.T) {
	f := newFixture(t)
	merged := f.closedPR(f.talkable, 11700, store.GHMerged, -time.Minute)
	held := f.poolSlot("review5", store.SlotHeld, merged.ID, true, nil)
	closedFoo := f.closedPR(f.foo, 3, store.GHClosed, -time.Hour)
	wt := f.prSlot(closedFoo, store.SlotHeld, true)
	slotless := f.closedPR(f.talkable, 11710, store.GHMerged, -time.Minute) // no slot: only the PR state moves
	open := f.pr(f.talkable, 11720, store.PRReviewed, store.GHOpen, nil)
	f.poolSlot("review6", store.SlotHeld, open.ID, true, nil) // open PR: untouched by default
	resuming := f.pr(f.talkable, 11730, store.PRReleasing, store.GHMerged, nil)
	f.poolSlot("review7", store.SlotReleasing, resuming.ID, true, nil)

	p := f.plan(Options{})
	rel := actionsOf(p, KindRelease)
	if len(rel) != 3 {
		t.Fatalf("release actions = %+v", rel)
	}
	if fin := rel[1]; fin.PRID != slotless.ID || fin.SlotID != 0 || fin.Slot != "" || !strings.Contains(fin.Why, "no slot") {
		t.Fatalf("slot-less closed PR = %+v", fin)
	}
	rel = slices.Delete(rel, 1, 2)
	a := rel[0]
	if a.Subject != "talkable/talkable#11700" || a.Slot != "review5" || a.SlotID != held.ID || a.PRID != merged.ID {
		t.Fatalf("release = %+v", a)
	}
	if a.Why != "merged 14m ago" || a.Reason != "merged" || a.Repo != "talkable/talkable" {
		t.Fatalf("release why/reason/repo = %q %q %q", a.Why, a.Reason, a.Repo)
	}
	if a.Bytes != 0 || len(a.DBNames) != 0 {
		t.Fatalf("release frees nothing on disk: %+v", a)
	}
	if rel[1].Slot != "review7" || rel[1].PRID != resuming.ID {
		t.Fatalf("releasing PR not resumed: %+v", rel[1])
	}
	rm := actionsOf(p, KindRemoveWorktree)
	if len(rm) != 1 || rm[0].SlotID != wt.ID || rm[0].Subject != "zhuravel/foo#3" || rm[0].Why != "closed 2h ago" {
		t.Fatalf("remove_worktree = %+v", rm)
	}
	if rm[0].Bytes != int64(len("pr-3"))*1024*1024 || rm[0].Path != wt.Path {
		t.Fatalf("remove_worktree bytes/path = %d %s", rm[0].Bytes, rm[0].Path)
	}
	if len(p.Actions) != 4 || len(p.Skipped) != 0 {
		t.Fatalf("actions = %+v skipped = %+v", p.Actions, p.Skipped)
	}
	if p.Totals.Actions != 4 || p.Totals.DiskBytes != rm[0].Bytes || p.Totals.MySQLBytes != 0 {
		t.Fatalf("totals = %+v", p.Totals)
	}
	if len(f.inv.calls) != 1 || f.inv.calls[0].External || f.inv.calls[0].Sizes {
		t.Fatalf("inventory scans = %+v (sizes come from du on targets only)", f.inv.calls)
	}
	if du := f.run.CallsWithPrefix("du"); len(du) != 1 || du[0].Args[1] != wt.Path {
		t.Fatalf("du calls = %+v", du)
	}
}

func TestDefaultPlanSkipsGraceAndActiveRuns(t *testing.T) {
	f := newFixture(t)
	young := f.closedPR(f.talkable, 11701, store.GHMerged, 6*time.Minute+30*time.Second)
	f.poolSlot("review1", store.SlotHeld, young.ID, true, nil)
	busy := f.closedPR(f.talkable, 11702, store.GHMerged, -time.Minute)
	f.poolSlot("review2", store.SlotHeld, busy.ID, true, nil)
	f.activeRun(busy, store.RunWorking)
	f.closedPR(f.talkable, 11703, store.GHMerged, 5*time.Minute) // no slot

	p := f.plan(Options{})
	if len(p.Actions) != 0 {
		t.Fatalf("actions = %+v", p.Actions)
	}
	s := mustSkip(t, p, "talkable/talkable#11701", SkipGraceLeft)
	if s.Detail != "6m left" {
		t.Fatalf("grace detail = %q", s.Detail)
	}
	mustSkip(t, p, "talkable/talkable#11702", SkipActiveRun)
	if _, ok := skipFor(p, "talkable/talkable#11703"); ok {
		t.Fatal("a closed PR without a slot is not a skip")
	}
}

func TestSlotGuardSkips(t *testing.T) {
	f := newFixture(t)
	mk := func(n int, name string, set func(*store.Slot)) store.PR {
		pr := f.closedPR(f.talkable, n, store.GHMerged, -time.Minute)
		f.poolSlot(name, store.SlotHeld, pr.ID, true, set)
		return pr
	}
	mk(1, "review1", func(s *store.Slot) { s.Pinned = true })
	mk(2, "review2", func(s *store.Slot) { s.HoldReason = new("head_drift") })
	mk(3, "review3", nil)
	f.agent("review3", inventory.AgentView{Name: "codex", Agent: "codex", Status: herdr.StatusWorking, PaneID: "p1"})
	mk(4, "review4", nil)
	f.agent("review4", inventory.AgentView{Name: "mg-talkable-4-judge", Status: herdr.StatusWorking, Magnum: true})
	f.agent("review4", inventory.AgentView{Name: "claude", Status: herdr.StatusIdle})
	mk(5, "review5", func(s *store.Slot) { s.State = store.SlotBusy })
	mk(6, "review6", func(s *store.Slot) { s.State = store.SlotBroken })
	pinnedPR := f.pr(f.talkable, 7, store.PRClosed, store.GHMerged, func(u *store.PRUpdate) { u.Set("pinned", true) })
	f.poolSlot("review7", store.SlotHeld, pinnedPR.ID, true, nil)

	p := f.plan(Options{})
	mustSkip(t, p, "talkable/talkable#1", SkipPinned)
	if s := mustSkip(t, p, "talkable/talkable#2", SkipHoldReason); s.Detail != "head_drift" {
		t.Fatalf("hold detail = %q", s.Detail)
	}
	mustSkip(t, p, "talkable/talkable#3", SkipAgentWorking)
	if s := mustSkip(t, p, "talkable/talkable#4", SkipAgentWorking); !strings.Contains(s.Detail, "claude is idle") {
		t.Fatalf("idle human agent detail = %q", s.Detail)
	}
	mustSkip(t, p, "talkable/talkable#5", SkipActiveRun)
	if s := mustSkip(t, p, "talkable/talkable#6", SkipSlotState); !strings.Contains(s.Detail, "broken") {
		t.Fatalf("slot_state detail = %q", s.Detail)
	}
	mustSkip(t, p, "talkable/talkable#7", SkipPinned)
	mk(8, "review8", nil)
	f.agent("review8", inventory.AgentView{Name: "mg-talkable-8-judge", Status: herdr.StatusWorking, Magnum: true})
	rel := actionsOf(f.plan(Options{}), KindRelease)
	if len(rel) != 1 || rel[0].Slot != "review8" {
		t.Fatalf("only review8 (magnum's own working agent) should release: %+v", rel)
	}
}

// A human's changes in a per-PR worktree (tracked or untracked: the removal
// deletes the directory) need --force; magnum's residue does not.
func TestPerPRDirtyNeedsForce(t *testing.T) {
	f := newFixture(t)
	pr := f.closedPR(f.foo, 4, store.GHClosed, -time.Minute)
	sl := f.prSlot(pr, store.SlotHeld, true)
	f.git.status[sl.Path] = gitx.Status{Tracked: 2}
	f.slots.evidence[sl.Name] = "someone typed into PR #4's magnum panes"

	p := f.plan(Options{})
	if s := mustSkip(t, p, "zhuravel/foo#4", SkipDirty); !strings.Contains(s.Detail, "PR #4's magnum panes") {
		t.Fatalf("detail = %q, want the evidence", s.Detail)
	}

	// Untracked files are deleted with the directory: they need force too.
	f.git.status[sl.Path] = gitx.Status{Untracked: 5}
	if s := mustSkip(t, f.plan(Options{}), "zhuravel/foo#4", SkipDirty); !strings.Contains(s.Detail, "5 untracked") {
		t.Fatalf("untracked detail = %q", s.Detail)
	}

	f.git.status[sl.Path] = gitx.Status{Tracked: 2, Untracked: 1}
	p = f.plan(Options{Force: true})
	rm := actionsOf(p, KindRemoveWorktree)
	if len(rm) != 1 || !rm[0].Force || !strings.Contains(rm[0].Why, "--force discards") {
		t.Fatalf("force plan = %+v", p)
	}

	// Without evidence the same changes are residue: removed without --force.
	delete(f.slots.evidence, sl.Name)
	rm = actionsOf(f.plan(Options{}), KindRemoveWorktree)
	if len(rm) != 1 || rm[0].Force || !strings.Contains(rm[0].Why, "magnum left") {
		t.Fatalf("residue plan = %+v", rm)
	}

	// Evidence that cannot be read counts as a human's.
	f.slots.errs["evidence "+sl.Name] = errBoom
	if s := mustSkip(t, f.plan(Options{}), "zhuravel/foo#4", SkipDirty); !strings.Contains(s.Detail, "could not tell") {
		t.Fatalf("evidence error detail = %q", s.Detail)
	}
}

func TestPoolReleaseRefusesAHumansTrackedChanges(t *testing.T) {
	// A release resets the slot: the slots guard refuses a human's tracked
	// changes, so the plan skips them (even with --force); untracked files
	// stay put.
	f := newFixture(t)
	pr := f.closedPR(f.talkable, 9, store.GHMerged, -time.Minute)
	f.poolSlot("review1", store.SlotHeld, pr.ID, true, nil)
	f.dirty("review1", 3, 0)
	f.slots.evidence["review1"] = "someone typed into PR #9's magnum panes"
	for _, force := range []bool{false, true} {
		if s := mustSkip(t, f.plan(Options{Force: force}), "talkable/talkable#9", SkipDirty); !strings.Contains(s.Detail, "3 tracked") {
			t.Fatalf("force=%v: detail = %q", force, s.Detail)
		}
	}
	f.dirty("review1", 0, 4)
	if p := f.plan(Options{}); len(actionsOf(p, KindRelease)) != 1 {
		t.Fatalf("untracked files only: plan = %+v", p)
	}
}

// The default cleanup releases a closed PR's slot over what a round left in
// it (a rewritten db/schema.rb, Gemfile.lock): the slots package discards
// that residue, so the pool does not starve.
func TestDefaultCleanupReleasesResidue(t *testing.T) {
	f := newFixture(t)
	pr := f.closedPR(f.talkable, 9, store.GHMerged, -time.Minute)
	f.poolSlot("review1", store.SlotHeld, pr.ID, true, nil)
	f.dirty("review1", 2, 1)

	p := f.plan(Options{})
	rel := actionsOf(p, KindRelease)
	if len(rel) != 1 || rel[0].Slot != "review1" || rel[0].Force || !strings.Contains(rel[0].Why, "discards 2 tracked changes magnum left") {
		t.Fatalf("plan = %+v skipped = %+v", p.Actions, p.Skipped)
	}
	rep, err := f.apply(p, false)
	if err != nil || rep.Done != 1 {
		t.Fatalf("rep = %+v err = %v", rep, err)
	}
	if !slices.Equal(f.slots.calls, []string{"release review1 merged"}) {
		t.Fatalf("slots calls = %v", f.slots.calls)
	}
	if got := f.prState(pr.ID); got != store.PRReleased {
		t.Fatalf("PR state = %s", got)
	}
}

func TestDefaultPlanOrphans(t *testing.T) {
	f := newFixture(t)
	names := f.orphan("review9", 100)
	f.orphan("pr27087fix", 50)

	p := f.plan(Options{})
	drops := actionsOf(p, KindDropDBs)
	if len(drops) != 1 {
		t.Fatalf("drops = %+v", drops)
	}
	d := drops[0]
	if d.Subject != "slug:review9" || d.Slug != "review9" || d.Confirm || !slices.Equal(d.DBNames, names) {
		t.Fatalf("drop = %+v", d)
	}
	if d.DBBytes != 8*100*1024*1024 || d.Bytes != 0 {
		t.Fatalf("drop bytes = %d / %d", d.DBBytes, d.Bytes)
	}
	s := mustSkip(t, p, "slug:pr27087fix", SkipNotManaged)
	if !strings.Contains(s.Detail, "--slug pr27087fix") {
		t.Fatalf("not_managed detail = %q", s.Detail)
	}
	if p.Totals.MySQLBytes != d.DBBytes || p.Totals.Databases != 8 {
		t.Fatalf("totals = %+v", p.Totals)
	}
}

func TestOrphansUnknownWhenMySQLNotListed(t *testing.T) {
	f := newFixture(t)
	f.inv.inv.DatabasesListed = false
	f.inv.inv.Warnings = []string{"mysql: connection refused"}
	p := f.plan(Options{Orphans: true})
	if len(p.Actions) != 0 {
		t.Fatalf("actions = %+v", p.Actions)
	}
	if !slices.Contains(p.Warnings, "mysql: connection refused") || !slices.ContainsFunc(p.Warnings, func(w string) bool {
		return strings.Contains(w, "orphan databases unknown")
	}) {
		t.Fatalf("warnings = %q", p.Warnings)
	}
}

func TestOrphansSelectorOnly(t *testing.T) {
	f := newFixture(t)
	pr := f.closedPR(f.talkable, 1, store.GHMerged, -time.Minute)
	f.poolSlot("review1", store.SlotHeld, pr.ID, true, nil)
	f.orphan("review9", 1)
	p := f.plan(Options{Orphans: true})
	if len(p.Actions) != 1 || p.Actions[0].Kind != KindDropDBs {
		t.Fatalf("--orphans plans only orphan drops: %+v", p.Actions)
	}
}

func TestManualSlugNeedsConfirmation(t *testing.T) {
	f := newFixture(t)
	f.orphan("review9", 1)
	names := f.orphan("pr27087fix", 10)
	f.poolSlot("review1", store.SlotFree, 0, true, nil)
	f.slotDBs("review1", 5)

	p := f.plan(Options{Orphans: true, Slug: "pr27087fix"})
	drops := actionsOf(p, KindDropDBs)
	if len(drops) != 2 {
		t.Fatalf("drops = %+v", drops)
	}
	m := drops[1]
	if m.Slug != "pr27087fix" || !m.Confirm || !slices.Equal(m.DBNames, names) {
		t.Fatalf("manual drop = %+v", m)
	}
	if _, ok := skipFor(p, "slug:pr27087fix"); ok {
		t.Fatal("named slug must not also be reported as not_managed")
	}
	if !p.NeedsConfirmation() {
		t.Fatal("NeedsConfirmation = false")
	}

	p = f.plan(Options{Orphans: true, Slug: "review1"})
	mustSkip(t, p, "slug:review1", SkipInUse)
	p = f.plan(Options{Orphans: true, Slug: "nothing"})
	mustSkip(t, p, "slug:nothing", SkipNotFound)

	// An allowlisted slug named explicitly is planned once, without confirmation.
	p = f.plan(Options{Orphans: true, Slug: "review9"})
	if d := actionsOf(p, KindDropDBs); len(d) != 1 || d[0].Confirm {
		t.Fatalf("drops = %+v", d)
	}
}

func TestPlanForPR(t *testing.T) {
	f := newFixture(t)
	open := f.pr(f.talkable, 11800, store.PRReviewed, store.GHOpen, nil)
	f.poolSlot("review2", store.SlotHeld, open.ID, true, nil)
	young := f.closedPR(f.talkable, 11801, store.GHMerged, 8*time.Minute)
	f.poolSlot("review3", store.SlotHeld, young.ID, true, nil)
	active := f.pr(f.talkable, 11802, store.PRReviewing, store.GHOpen, nil)
	f.poolSlot("review4", store.SlotBusy, active.ID, true, nil)
	f.pr(f.talkable, 11803, store.PRReviewed, store.GHOpen, nil) // parked, no slot

	p := f.plan(Options{PR: &PRRef{Repo: "talkable/talkable", Number: 11800}})
	rel := actionsOf(p, KindRelease)
	if len(rel) != 1 || rel[0].Slot != "review2" || rel[0].Why != "requested (PR open)" || rel[0].Reason != "cleanup" {
		t.Fatalf("release = %+v", p)
	}

	p = f.plan(Options{PR: &PRRef{Repo: "talkable/talkable", Number: 11801}})
	mustSkip(t, p, "talkable/talkable#11801", SkipGraceLeft)
	p = f.plan(Options{PR: &PRRef{Repo: "talkable/talkable", Number: 11801}, Force: true})
	if rel := actionsOf(p, KindRelease); len(rel) != 1 || rel[0].Slot != "review3" {
		t.Fatalf("force inside grace = %+v", p)
	}

	p = f.plan(Options{PR: &PRRef{Repo: "talkable/talkable", Number: 11802}})
	mustSkip(t, p, "talkable/talkable#11802", SkipActiveRun)
	p = f.plan(Options{PR: &PRRef{Repo: "talkable/talkable", Number: 11803}})
	mustSkip(t, p, "talkable/talkable#11803", SkipNotFound)
	p = f.plan(Options{PR: &PRRef{Repo: "talkable/talkable", Number: 1}})
	mustSkip(t, p, "talkable/talkable#1", SkipNotFound)
	p = f.plan(Options{PR: &PRRef{Repo: "nobody/nothing", Number: 1}})
	mustSkip(t, p, "nobody/nothing#1", SkipNotFound)
}

func TestPlanForSlot(t *testing.T) {
	f := newFixture(t)
	open := f.pr(f.talkable, 11900, store.PRReviewed, store.GHOpen, nil)
	f.poolSlot("review1", store.SlotHeld, open.ID, true, nil)
	free := f.poolSlot("review2", store.SlotFree, 0, true, func(s *store.Slot) { s.DBSlug = nil })
	f.slotDBs("review2", 10)
	f.poolSlot("review3", store.SlotBusy, 0, true, nil)
	f.createSlot(store.Slot{Name: "repo3", RepoFullName: "talkable/talkable", Kind: store.SlotKindExternal,
		Path: filepath.Join(f.root, "talkable.repo3"), MainClone: filepath.Join(f.root, "talkable"), State: store.SlotObserved}, true)
	f.poolSlot("review4", store.SlotHeld, 0, true, nil)
	f.dirty("review4", 1, 0)

	p := f.plan(Options{Slot: "review1"})
	rel := actionsOf(p, KindRelease)
	if len(rel) != 1 || rel[0].Subject != "slot:review1" || rel[0].PRID != open.ID || !strings.Contains(rel[0].Why, "talkable#11900") {
		t.Fatalf("release = %+v", p)
	}

	p = f.plan(Options{Slot: "review2"})
	if s := mustSkip(t, p, "slot:review2", SkipSlotState); !strings.Contains(s.Detail, "free") {
		t.Fatalf("detail = %q", s.Detail)
	}

	p = f.plan(Options{Slot: "review2", Remove: true})
	rm := actionsOf(p, KindRemoveSlot)
	if len(rm) != 1 || rm[0].SlotID != free.ID || rm[0].Slug != "review2" {
		t.Fatalf("remove = %+v", p)
	}
	if rm[0].Bytes != int64(len("talkable.review2"))*1024*1024 || rm[0].DBBytes != 8*10*1024*1024 || len(rm[0].DBNames) != 8 {
		t.Fatalf("remove sizes = %d %d %v", rm[0].Bytes, rm[0].DBBytes, rm[0].DBNames)
	}

	p = f.plan(Options{Slot: "review3", Remove: true})
	mustSkip(t, p, "slot:review3", SkipActiveRun)
	p = f.plan(Options{Slot: "repo3"})
	mustSkip(t, p, "slot:repo3", SkipNotManaged)
	p = f.plan(Options{Slot: "review99"})
	mustSkip(t, p, "slot:review99", SkipNotFound)

	// review4 is held without a PR: release is fine, removal needs --force.
	p = f.plan(Options{Slot: "review4", Remove: true})
	mustSkip(t, p, "slot:review4", SkipSlotState)
	p = f.plan(Options{Slot: "review4", Remove: true, Force: true})
	if rm := actionsOf(p, KindRemoveSlot); len(rm) != 1 || !rm[0].Force {
		t.Fatalf("forced remove = %+v", p)
	}
	f.view("review4").Slot.State = store.SlotFree
	if err := f.st.TransitionSlot(f.ctx, f.view("review4").Slot.ID, nil, store.SlotFree, nil); err != nil {
		t.Fatal(err)
	}
	p = f.plan(Options{Slot: "review4", Remove: true})
	if rm := actionsOf(p, KindRemoveSlot); len(rm) != 1 || rm[0].Force {
		t.Fatalf("remove over residue = %+v", p)
	}
	f.slots.evidence["review4"] = "someone typed into its PR's magnum panes"
	p = f.plan(Options{Slot: "review4", Remove: true})
	mustSkip(t, p, "slot:review4", SkipDirty)
}

func TestPlanForPerPRSlotByName(t *testing.T) {
	f := newFixture(t)
	pr := f.pr(f.foo, 5, store.PRReviewed, store.GHOpen, nil)
	sl := f.prSlot(pr, store.SlotHeld, true)
	p := f.plan(Options{Slot: sl.Name})
	rm := actionsOf(p, KindRemoveWorktree)
	if len(rm) != 1 || rm[0].SlotID != sl.ID || rm[0].PRID != pr.ID {
		t.Fatalf("per-PR slot release = %+v", p)
	}
}

func TestShrink(t *testing.T) {
	f := newFixture(t)
	used := func(d time.Duration) func(*store.Slot) {
		return func(s *store.Slot) { s.LastUsedAt = new(now.Add(-d)) }
	}
	pr := f.pr(f.talkable, 1, store.PRReviewed, store.GHOpen, nil)
	f.poolSlot("review1", store.SlotHeld, pr.ID, true, used(time.Hour))
	f.poolSlot("review2", store.SlotFree, 0, true, used(time.Hour))
	f.poolSlot("review3", store.SlotFree, 0, true, func(s *store.Slot) { used(100 * time.Hour)(s); s.Pinned = true })
	f.poolSlot("review4", store.SlotFree, 0, true, used(72*time.Hour))
	f.poolSlot("review5", store.SlotFree, 0, true, used(50*time.Hour))
	f.poolSlot("review6", store.SlotRemoved, 0, false, nil)

	// 5 live slots, min 2: shrink to max(2, 0) removes up to 3 free ones, oldest idle first.
	p := f.plan(Options{Shrink: new(0)})
	rm := actionsOf(p, KindRemoveSlot)
	var got []string
	for _, a := range rm {
		got = append(got, a.Slot)
		if !strings.Contains(a.Why, "shrink") {
			t.Errorf("why = %q", a.Why)
		}
	}
	if !slices.Equal(got, []string{"review4", "review5", "review2"}) {
		t.Fatalf("shrink order = %v", got)
	}
	mustSkip(t, p, "slot:review3", SkipPinned)

	// max(min, 4) keeps 4 of 5.
	p = f.plan(Options{Shrink: new(4)})
	if rm := actionsOf(p, KindRemoveSlot); len(rm) != 1 || rm[0].Slot != "review4" {
		t.Fatalf("shrink 4 = %+v", p.Actions)
	}
	// N below min keeps min.
	p = f.plan(Options{Shrink: new(1)})
	if rm := actionsOf(p, KindRemoveSlot); len(rm) != 3 {
		t.Fatalf("shrink 1 = %+v", p.Actions)
	}
	// Idle: only slots idle longer than idle_remove_after (48h).
	p = f.plan(Options{Shrink: new(0), Idle: true})
	got = nil
	for _, a := range actionsOf(p, KindRemoveSlot) {
		got = append(got, a.Slot)
	}
	if !slices.Equal(got, []string{"review4", "review5"}) {
		t.Fatalf("idle shrink = %v", got)
	}
}

func TestExternalReset(t *testing.T) {
	f := newFixture(t)
	ev := f.external("repo3", func(e *inventory.ExternalView) {
		e.PRNumber, e.GHState, e.Branch = 11932, store.GHMerged, "feature-x"
	})
	f.git.unpushed[ev.Path] = 2 // stay on feature-x, which the reset does not move

	p := f.plan(Options{External: true, Slot: "repo3"})
	if len(f.inv.calls) != 1 || !f.inv.calls[0].External {
		t.Fatalf("scan options = %+v", f.inv.calls)
	}
	r := actionsOf(p, KindResetExternal)
	if len(r) != 1 {
		t.Fatalf("plan = %+v", p)
	}
	a := r[0]
	if a.Subject != "path:"+ev.Path || a.Path != ev.Path || a.MainClone != ev.MainClone || a.Branch != "repo3" || a.Base != "master" {
		t.Fatalf("reset = %+v", a)
	}
	if len(a.DBNames) != 0 || a.DBBytes != 0 || !strings.Contains(a.Why, "#11932 MERGED") {
		t.Fatalf("reset never drops databases; why = %q", a.Why)
	}
	// Path and full directory name also resolve.
	if p := f.plan(Options{External: true, Slot: ev.Path}); len(actionsOf(p, KindResetExternal)) != 1 {
		t.Fatalf("by path = %+v", p)
	}
	if p := f.plan(Options{External: true, Slot: "talkable.repo3"}); len(actionsOf(p, KindResetExternal)) != 1 {
		t.Fatalf("by dir name = %+v", p)
	}
}

func TestExternalSkips(t *testing.T) {
	f := newFixture(t)
	f.external("repo1", func(e *inventory.ExternalView) {
		e.Agents = []inventory.AgentView{{Name: "mg-x", Status: herdr.StatusBlocked, Magnum: true}}
	})
	dirty := f.external("repo2", nil)
	f.git.status[dirty.Path] = gitx.Status{Tracked: 1}
	onBranch := f.external("repo4", nil) // on the branch the reset moves
	f.git.unpushed[onBranch.Path] = 1
	detached := f.external("repo5", func(e *inventory.ExternalView) { e.Detached = true; e.Branch = "" })
	f.git.unpushed[detached.Path] = 3
	f.external("repo6", func(e *inventory.ExternalView) { e.Exists = false })
	f.poolSlot("review1", store.SlotFree, 0, true, nil)

	cases := map[string]string{
		"repo1": SkipAgentWorking, "repo2": SkipDirty, "repo4": SkipUnpushed, "repo5": SkipUnpushed,
		"repo6": SkipNotFound, "repo7": SkipNotFound, "review1": SkipNotFound,
	}
	for name, reason := range cases {
		p := f.plan(Options{External: true, Slot: name})
		if len(p.Actions) != 0 || len(p.Skipped) != 1 || p.Skipped[0].Reason != reason {
			t.Errorf("%s: plan = %+v, want skip %s", name, p, reason)
		}
	}
	for _, name := range []string{"repo2", "repo4", "repo5"} {
		p := f.plan(Options{External: true, Slot: name, Force: true})
		if r := actionsOf(p, KindResetExternal); len(r) != 1 || !r[0].Force {
			t.Errorf("%s forced: %+v", name, p)
		}
	}
	// A working agent is never overridden.
	if p := f.plan(Options{External: true, Slot: "repo1", Force: true}); len(p.Actions) != 0 {
		t.Errorf("forced reset with a busy agent = %+v", p)
	}
}

// A forced reset names what it discards in the question (the plan's line):
// the tracked changes and the commits on no remote it would have refused.
func TestExternalForcedResetNamesWhatItDiscards(t *testing.T) {
	f := newFixture(t)
	dirty := f.external("repo2", func(e *inventory.ExternalView) { e.Detached = true; e.Branch = "" })
	f.git.status[dirty.Path] = gitx.Status{Tracked: 2, Untracked: 5}
	f.git.unpushed[dirty.Path] = 3
	other := f.external("repo4", func(e *inventory.ExternalView) { e.Branch = "feature-x" })
	f.git.branches[other.Path+" refs/heads/repo4"] = 1
	f.git.errs["status "+other.Path] = errBoom
	f.external("repo5", nil)

	why := func(name string) string {
		t.Helper()
		r := actionsOf(f.plan(Options{External: true, Slot: name, Force: true}), KindResetExternal)
		if len(r) != 1 || !r[0].Force {
			t.Fatalf("%s: forced reset = %+v", name, r)
		}
		return r[0].Why
	}
	if w := why("repo2"); !strings.Contains(w, "--force discards 2 tracked changes; 3 commits on detached HEAD are on no remote") ||
		strings.Contains(w, "untracked") {
		t.Errorf("repo2 why = %q", w)
	}
	if w := why("repo4"); !strings.Contains(w, "could not read git status") ||
		!strings.Contains(w, "1 commits on branch repo4 (which the reset overwrites) are on no remote") {
		t.Errorf("repo4 why = %q", w)
	}
	if w := why("repo5"); strings.Contains(w, "--force") {
		t.Errorf("clean repo5 why = %q", w)
	}
}

func TestPlanDryRunFlag(t *testing.T) {
	f := newFixture(t)
	p := f.plan(Options{DryRun: true})
	if !p.DryRun || !p.CreatedAt.Equal(now) {
		t.Fatalf("plan = %+v", p)
	}
}

func TestPlanInventoryError(t *testing.T) {
	f := newFixture(t)
	f.inv.err = errBoom
	if _, err := f.p.Plan(f.ctx, Options{}); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
}

func TestPlanFromUsesTheGivenInventory(t *testing.T) {
	f := newFixture(t)
	f.orphan("review9", 100)
	inv := f.inv.inv
	f.inv.inv = inventory.Inventory{} // a scan now would see nothing
	p, err := f.p.PlanFrom(f.ctx, Options{}, inv)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.inv.calls) != 0 {
		t.Fatalf("PlanFrom scanned the inventory %d times", len(f.inv.calls))
	}
	if drops := actionsOf(p, KindDropDBs); len(drops) != 1 || drops[0].Slug != "review9" {
		t.Fatalf("drops = %+v", drops)
	}
	if _, err := f.p.PlanFrom(f.ctx, Options{Idle: true}, inv); !errors.Is(err, ErrOptions) {
		t.Fatalf("invalid options = %v", err)
	}
}
