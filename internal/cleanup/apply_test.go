package cleanup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
)

func (f *fixture) apply(p Plan, confirmed bool) (Report, error) {
	f.t.Helper()
	return f.p.Apply(f.ctx, p, confirmed)
}

func TestApplyReleaseClosedPR(t *testing.T) {
	f := newFixture(t)
	pr := f.closedPR(f.talkable, 11700, store.GHMerged, -time.Minute)
	sl := f.poolSlot("review5", store.SlotHeld, pr.ID, true, nil)

	rep, err := f.apply(f.plan(Options{}), false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Done != 1 || rep.Failed != 0 || len(rep.Results) != 1 || rep.Results[0].Status != StatusDone {
		t.Fatalf("report = %+v", rep)
	}
	if !slices.Equal(f.slots.calls, []string{"release review5 merged"}) {
		t.Fatalf("slots calls = %v", f.slots.calls)
	}
	if f.slots.pools[0].Repo != "talkable/talkable" || f.slots.pools[0].SlotName != "review{n}" {
		t.Fatalf("pool = %+v", f.slots.pools[0])
	}
	if !slices.Equal(f.parked, []int64{pr.ID}) {
		t.Fatalf("parked = %v", f.parked)
	}
	if got := f.prState(pr.ID); got != store.PRReleased {
		t.Fatalf("PR state = %s, want released", got)
	}
	evs := f.events("slot:" + sl.Name)
	if len(evs) != 1 || evs[0].Kind != "cleanup.release" || evs[0].Level != "info" {
		t.Fatalf("slot events = %+v", evs)
	}
	if evs := f.events(fmt.Sprintf("pr:%d", pr.ID)); len(evs) != 1 || !strings.Contains(evs[0].Message, "released") {
		t.Fatalf("pr events = %+v", evs)
	}
}

func TestApplyFinishesSlotlessClosedPRs(t *testing.T) {
	f := newFixture(t)
	never := f.closedPR(f.talkable, 1, store.GHMerged, -time.Minute)       // closed without ever holding a slot
	crashed := f.pr(f.talkable, 2, store.PRReleasing, store.GHMerged, nil) // slot released, crash before released
	p := f.plan(Options{})
	if len(p.Actions) != 2 {
		t.Fatalf("plan = %+v", p.Actions)
	}
	rep, err := f.apply(p, false)
	if err != nil || rep.Done != 2 {
		t.Fatalf("rep = %+v err = %v", rep, err)
	}
	if len(f.slots.calls) != 0 {
		t.Fatalf("slots calls = %v", f.slots.calls)
	}
	if !slices.Equal(f.parked, []int64{never.ID, crashed.ID}) {
		t.Fatalf("parked = %v", f.parked)
	}
	for _, id := range []int64{never.ID, crashed.ID} {
		if got := f.prState(id); got != store.PRReleased {
			t.Fatalf("PR %d state = %s", id, got)
		}
		if evs := f.events(fmt.Sprintf("pr:%d", id)); len(evs) != 1 || evs[0].Kind != "cleanup.release" {
			t.Fatalf("events = %+v", evs)
		}
	}
	if p := f.plan(Options{}); len(p.Actions) != 0 {
		t.Fatalf("second plan = %+v", p.Actions)
	}
}

func TestApplySlotlessRechecksReopen(t *testing.T) {
	f := newFixture(t)
	pr := f.closedPR(f.talkable, 1, store.GHMerged, -time.Minute)
	p := f.plan(Options{})
	if err := f.st.TransitionPR(f.ctx, pr.ID, nil, store.PRQueued, nil); err != nil {
		t.Fatal(err)
	}
	if rep, err := f.apply(p, false); !errors.Is(err, store.ErrConflict) || rep.Failed != 1 || len(f.parked) != 0 {
		t.Fatalf("rep = %+v err = %v parked = %v", rep, err, f.parked)
	}
	if got := f.prState(pr.ID); got != store.PRQueued {
		t.Fatalf("state = %s", got)
	}
}

func TestApplyOpenPRKeepsState(t *testing.T) {
	f := newFixture(t)
	pr := f.pr(f.talkable, 11800, store.PRReviewed, store.GHOpen, nil)
	f.poolSlot("review2", store.SlotHeld, pr.ID, true, nil)
	if _, err := f.apply(f.plan(Options{PR: &PRRef{Repo: "talkable/talkable", Number: 11800}}), false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.slots.calls, []string{"release review2 cleanup"}) {
		t.Fatalf("calls = %v", f.slots.calls)
	}
	if got := f.prState(pr.ID); got != store.PRReviewed {
		t.Fatalf("open PR state = %s, want reviewed (parked)", got)
	}
}

func TestApplyContinuesPastFailures(t *testing.T) {
	f := newFixture(t)
	a := f.closedPR(f.talkable, 1, store.GHMerged, -2*time.Minute)
	f.poolSlot("review1", store.SlotHeld, a.ID, true, nil)
	b := f.closedPR(f.talkable, 2, store.GHMerged, -time.Minute)
	f.poolSlot("review2", store.SlotHeld, b.ID, true, nil)
	f.slots.errs["release review1"] = errBoom

	rep, err := f.apply(f.plan(Options{}), false)
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if rep.Done != 1 || rep.Failed != 1 || rep.Results[0].Status != StatusFailed || !strings.Contains(rep.Results[0].Error, "boom") {
		t.Fatalf("report = %+v", rep)
	}
	if got := f.prState(a.ID); got != store.PRReleasing {
		t.Fatalf("failed PR state = %s, want releasing (resumable)", got)
	}
	if got := f.prState(b.ID); got != store.PRReleased {
		t.Fatalf("second PR state = %s", got)
	}
	if evs := f.events("slot:review1"); len(evs) != 1 || evs[0].Level != "error" {
		t.Fatalf("failure event = %+v", evs)
	}
	// The next default plan resumes the releasing PR.
	if rel := actionsOf(f.plan(Options{}), KindRelease); len(rel) != 1 || rel[0].PRID != a.ID {
		t.Fatalf("resume plan = %+v", rel)
	}
}

func TestApplyParkBusyLeavesPRClosed(t *testing.T) {
	f := newFixture(t)
	pr := f.closedPR(f.talkable, 1, store.GHMerged, -time.Minute)
	f.poolSlot("review1", store.SlotHeld, pr.ID, true, nil)
	f.parkErr = errBoom
	rep, err := f.apply(f.plan(Options{}), false)
	if !errors.Is(err, errBoom) || rep.Failed != 1 {
		t.Fatalf("rep = %+v err = %v", rep, err)
	}
	if len(f.slots.calls) != 0 {
		t.Fatalf("slots touched: %v", f.slots.calls)
	}
	if got := f.prState(pr.ID); got != store.PRClosed {
		t.Fatalf("PR state = %s, want closed", got)
	}
}

// A review request racing a release (`magnum review` of a merged PR moves
// it closed → rereview_pending with a compare-and-set) loses once the
// release has begun: the release takes the PR from closed to releasing
// before its first side effect (parking the sessions), so the request's
// compare-and-set during the park fails, no round starts in a checkout being
// handed back, and the release completes.
func TestApplyReleaseBeatsAReviewRequestDuringThePark(t *testing.T) {
	f := newFixture(t)
	pr := f.closedPR(f.talkable, 1, store.GHMerged, -time.Minute)
	f.poolSlot("review1", store.SlotHeld, pr.ID, true, nil)
	var requestErr error
	f.parkHook = func(store.PR) {
		requestErr = f.st.TransitionPR(f.ctx, pr.ID, []string{store.PRClosed}, store.PRRereviewPending,
			func(u *store.PRUpdate) { u.Set("forced", true) })
	}
	rep, err := f.apply(f.plan(Options{}), false)
	if err != nil || rep.Done != 1 {
		t.Fatalf("rep = %+v err = %v", rep, err)
	}
	if !errors.Is(requestErr, store.ErrConflict) {
		t.Fatalf("the review request's compare-and-set during the park = %v, want a conflict", requestErr)
	}
	if got := f.prState(pr.ID); got != store.PRReleased {
		t.Fatalf("PR state = %s, want released", got)
	}
}

// A review request that wins (the PR left closed before the release's
// compare-and-set) stops the release before any side effect: no session is
// parked and the slot is not touched.
func TestApplyStopsBeforeAnySideEffectWhenAReviewRequestWon(t *testing.T) {
	f := newFixture(t)
	pr := f.closedPR(f.talkable, 1, store.GHMerged, -time.Minute)
	f.poolSlot("review1", store.SlotHeld, pr.ID, true, nil)
	plan := f.plan(Options{})
	if err := f.st.TransitionPR(f.ctx, pr.ID, []string{store.PRClosed}, store.PRRereviewPending,
		func(u *store.PRUpdate) { u.Set("forced", true) }); err != nil {
		t.Fatal(err)
	}
	rep, err := f.apply(plan, false)
	if !errors.Is(err, store.ErrConflict) || rep.Done != 0 {
		t.Fatalf("rep = %+v err = %v, want the release stopped as stale", rep, err)
	}
	if len(f.parked) != 0 || len(f.slots.calls) != 0 {
		t.Fatalf("side effects: parked %v, slot calls %v", f.parked, f.slots.calls)
	}
	if got := f.prState(pr.ID); got != store.PRRereviewPending {
		t.Fatalf("PR state = %s, want the request's rereview_pending", got)
	}
}

func TestApplyRechecksStaleState(t *testing.T) {
	f := newFixture(t)
	a := f.closedPR(f.talkable, 1, store.GHMerged, -time.Minute)
	s1 := f.poolSlot("review1", store.SlotHeld, a.ID, true, nil)
	b := f.closedPR(f.talkable, 2, store.GHMerged, -time.Minute)
	f.poolSlot("review2", store.SlotHeld, b.ID, true, nil)
	c := f.closedPR(f.talkable, 3, store.GHMerged, -time.Minute)
	f.poolSlot("review3", store.SlotHeld, c.ID, true, nil)
	d := f.closedPR(f.talkable, 4, store.GHMerged, -time.Minute)
	f.poolSlot("review4", store.SlotHeld, d.ID, true, nil)
	p := f.plan(Options{})
	if len(p.Actions) != 4 {
		t.Fatalf("plan = %+v", p)
	}

	// Between plan and apply: review1 pinned, PR 2 reopened, PR 3 got a run, review4 released elsewhere.
	if err := f.st.UpdateSlotFields(f.ctx, s1.ID, func(u *store.SlotUpdate) { u.Set("pinned", true) }); err != nil {
		t.Fatal(err)
	}
	if err := f.st.TransitionPR(f.ctx, b.ID, nil, store.PRReviewed, nil); err != nil {
		t.Fatal(err)
	}
	f.activeRun(c, store.RunPending)
	if err := f.st.ReleaseSlot(f.ctx, f.view("review4").Slot.ID, nil, store.SlotFree, "elsewhere"); err != nil {
		t.Fatal(err)
	}

	rep, err := f.apply(p, false)
	if err == nil || rep.Failed != 4 {
		t.Fatalf("rep = %+v err = %v", rep, err)
	}
	if len(f.slots.calls) != 0 || len(f.parked) != 0 {
		t.Fatalf("calls = %v parked = %v", f.slots.calls, f.parked)
	}
	for i, want := range []string{"pinned", "reopened", "active", "no longer holds"} {
		if !strings.Contains(rep.Results[i].Error, want) {
			t.Errorf("result %d error = %q, want %q", i, rep.Results[i].Error, want)
		}
	}
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale errors should wrap store.ErrConflict: %v", err)
	}
}

func TestApplyRemoveWorktree(t *testing.T) {
	f := newFixture(t)
	pr := f.closedPR(f.foo, 3, store.GHClosed, -time.Minute)
	sl := f.prSlot(pr, store.SlotHeld, true)
	if _, err := f.apply(f.plan(Options{}), false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.slots.calls, []string{"remove_pr " + sl.Name + " force=false"}) {
		t.Fatalf("calls = %v", f.slots.calls)
	}
	if got := f.prState(pr.ID); got != store.PRReleased {
		t.Fatalf("PR state = %s", got)
	}
}

func TestApplyForceRunsGuardFirst(t *testing.T) {
	f := newFixture(t)
	mk := func(n int) store.Slot {
		pr := f.closedPR(f.foo, n, store.GHClosed, -time.Minute)
		sl := f.prSlot(pr, store.SlotHeld, true)
		f.git.status[sl.Path] = gitx.Status{Tracked: 1}
		return sl
	}
	agent, drift, ok := mk(1), mk(2), mk(3)
	f.slots.errs["guard "+agent.Name] = slots.ErrHold{Reason: slots.HoldForeignAgent, Detail: "pane p1"}
	f.slots.errs["guard "+drift.Name] = slots.ErrHold{Reason: slots.HoldHeadDrift, Detail: "HEAD moved"}

	rep, err := f.apply(f.plan(Options{Force: true}), false)
	if err == nil || rep.Failed != 1 || rep.Done != 2 {
		t.Fatalf("rep = %+v err = %v", rep, err)
	}
	if h, isHold := slots.AsHold(err); !isHold || h.Reason != slots.HoldForeignAgent {
		t.Fatalf("err = %v, want foreign_agent hold", err)
	}
	want := []string{
		"guard " + agent.Name,
		"guard " + drift.Name, "remove_pr " + drift.Name + " force=true",
		"guard " + ok.Name, "remove_pr " + ok.Name + " force=true",
	}
	if !slices.Equal(f.slots.calls, want) {
		t.Fatalf("calls = %v\nwant %v", f.slots.calls, want)
	}
}

func TestApplyRemoveSlot(t *testing.T) {
	f := newFixture(t)
	f.poolSlot("review2", store.SlotFree, 0, true, nil)
	rep, err := f.apply(f.plan(Options{Slot: "review2", Remove: true}), false)
	if err != nil || rep.Done != 1 {
		t.Fatalf("rep = %+v err = %v", rep, err)
	}
	if !slices.Equal(f.slots.calls, []string{"remove review2 force=false"}) || f.slots.pools[0].Repo != "talkable/talkable" {
		t.Fatalf("calls = %v", f.slots.calls)
	}
	if evs := f.events("slot:review2"); len(evs) != 1 || evs[0].Kind != "cleanup.remove_slot" {
		t.Fatalf("events = %+v", evs)
	}
}

func TestApplyDropAllowlisted(t *testing.T) {
	f := newFixture(t)
	names := f.orphan("review9", 1)
	if _, err := f.st.UpsertSlotDatabase(f.ctx, store.SlotDatabase{DBName: names[0], Slug: "review9"}); err != nil {
		t.Fatal(err)
	}
	f.mysql.fail[names[7]] = errBoom

	rep, err := f.apply(f.plan(Options{}), false)
	if !errors.Is(err, errBoom) || rep.Failed != 1 {
		t.Fatalf("rep = %+v err = %v", rep, err)
	}
	if len(f.mysql.calls) != 1 || !slices.Equal(f.mysql.calls[0], names) {
		t.Fatalf("drops = %v", f.mysql.calls)
	}
	g := f.mysql.guards[0]
	if g.Prefix != "talkable_" || g.AllowRegexp == nil || g.AllowRegexp.String() != "^review[0-9]+$" {
		t.Fatalf("guard = %+v", g)
	}
	dbs, err := f.st.ListSlotDatabases(f.ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	dropped := map[string]string{}
	for _, d := range dbs {
		if d.DroppedAt != nil {
			dropped[d.DBName] = store.Deref(d.DroppedBy)
		}
	}
	if len(dropped) != 7 || dropped[names[0]] != DroppedBy || dropped[names[7]] != "" {
		t.Fatalf("dropped = %v", dropped)
	}
	if !slices.Equal(rep.DroppedDBs, names[:7]) {
		t.Fatalf("report dropped = %v", rep.DroppedDBs)
	}
	if evs := f.events("slug:review9"); len(evs) != 1 || evs[0].Level != "error" {
		t.Fatalf("events = %+v", evs)
	}
}

func TestApplyDropRefusesSlugNowInUse(t *testing.T) {
	f := newFixture(t)
	f.orphan("review9", 1)
	p := f.plan(Options{})
	f.poolSlot("review9", store.SlotProvisioning, 0, true, nil) // provisioned after the plan
	rep, err := f.apply(p, false)
	if !errors.Is(err, store.ErrConflict) || rep.Failed != 1 {
		t.Fatalf("rep = %+v err = %v", rep, err)
	}
	if len(f.mysql.calls) != 0 {
		t.Fatalf("dropped anyway: %v", f.mysql.calls)
	}
}

func TestApplyManualSlugNeedsConfirmation(t *testing.T) {
	f := newFixture(t)
	names := f.orphan("pr27087fix", 1)
	p := f.plan(Options{Orphans: true, Slug: "pr27087fix"})

	rep, err := f.apply(p, false)
	if err != nil || rep.Unconfirmed != 1 || rep.Results[0].Status != StatusUnconfirmed || len(f.mysql.calls) != 0 {
		t.Fatalf("unconfirmed apply: rep = %+v err = %v drops = %v", rep, err, f.mysql.calls)
	}
	rep, err = f.apply(p, true)
	if err != nil || rep.Done != 1 {
		t.Fatalf("confirmed apply: rep = %+v err = %v", rep, err)
	}
	g := f.mysql.guards[0]
	if g.Prefix != "talkable_" || g.AllowRegexp.String() != "^pr27087fix$" || !slices.Equal(f.mysql.calls[0], names) {
		t.Fatalf("guard = %+v calls = %v", g, f.mysql.calls)
	}
	// The manual guard still refuses anything else.
	if g.Check("talkable_test__review1") == nil || g.Check("other_test__pr27087fix") == nil {
		t.Fatal("manual guard too wide")
	}
}

func TestApplyDryRun(t *testing.T) {
	f := newFixture(t)
	pr := f.closedPR(f.talkable, 1, store.GHMerged, -time.Minute)
	f.poolSlot("review1", store.SlotHeld, pr.ID, true, nil)
	f.orphan("review9", 1)
	rep, err := f.apply(f.plan(Options{DryRun: true}), true)
	if err != nil || rep.Planned != 2 || rep.Done != 0 {
		t.Fatalf("rep = %+v err = %v", rep, err)
	}
	for _, r := range rep.Results {
		if r.Status != StatusPlanned {
			t.Fatalf("status = %s", r.Status)
		}
	}
	if len(f.slots.calls)+len(f.mysql.calls)+len(f.parked) != 0 {
		t.Fatal("dry run had side effects")
	}
	if got := f.prState(pr.ID); got != store.PRClosed {
		t.Fatalf("PR state = %s", got)
	}
}

func TestApplyResetExternal(t *testing.T) {
	f := newFixture(t)
	ev := f.external("repo3", nil)
	mustWrite(t, filepath.Join(ev.MainClone, ".mise.local.toml"), "[env]\nSECRET = \"x\"\n", 0o600)
	mustWrite(t, filepath.Join(ev.Path, ".mise.local.toml"), "old\n", 0o644)

	p := f.plan(Options{External: true, Slot: "repo3"})
	rep, err := f.apply(p, false)
	if err != nil || rep.Done != 1 {
		t.Fatalf("rep = %+v err = %v", rep, err)
	}
	if len(f.inv.calls) != 2 || !f.inv.calls[1].External {
		t.Fatalf("apply must rescan the external worktree: %+v", f.inv.calls)
	}
	want := []string{"fetch " + ev.MainClone + " master", "reset " + ev.Path + " repo3 master"}
	if !slices.Equal(f.git.calls, want) {
		t.Fatalf("git calls = %v", f.git.calls)
	}
	b, err := os.ReadFile(filepath.Join(ev.Path, ".mise.local.toml"))
	if err != nil || string(b) != "[env]\nSECRET = \"x\"\n" {
		t.Fatalf("mise file = %q %v", b, err)
	}
	if st, _ := os.Stat(filepath.Join(ev.Path, ".mise.local.toml")); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode().Perm())
	}
	if len(f.mysql.calls) != 0 {
		t.Fatal("external reset dropped databases")
	}
	if evs := f.events("path:" + ev.Path); len(evs) != 1 || evs[0].Kind != "cleanup.reset_external" {
		t.Fatalf("events = %+v", evs)
	}
}

func TestApplyResetExternalRechecks(t *testing.T) {
	f := newFixture(t)
	ev := f.external("repo3", nil)
	p := f.plan(Options{External: true, Slot: "repo3"})

	f.inv.inv.External[0].Agents = []inventory.AgentView{{Name: "claude", Status: herdr.StatusWorking}}
	if rep, err := f.apply(p, false); err == nil || !strings.Contains(rep.Results[0].Error, SkipAgentWorking) {
		t.Fatalf("agent at apply: rep = %+v err = %v", rep, err)
	}
	f.inv.inv.External[0].Agents = nil
	f.git.status[ev.Path] = gitx.Status{Tracked: 1}
	if rep, err := f.apply(p, false); err == nil || !strings.Contains(rep.Results[0].Error, SkipDirty) {
		t.Fatalf("dirty at apply: rep = %+v err = %v", rep, err)
	}
	f.inv.inv.External = nil
	if rep, err := f.apply(p, false); err == nil || !strings.Contains(rep.Results[0].Error, "not found") {
		t.Fatalf("gone at apply: rep = %+v err = %v", rep, err)
	}
	if len(f.git.calls) != 0 {
		t.Fatalf("git calls = %v", f.git.calls)
	}
}

func TestResetExternalArgvThroughGitx(t *testing.T) {
	f := newFixture(t)
	ev := f.external("repo3", nil)
	for _, d := range []string{ev.Path, ev.MainClone} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fake := &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"git", "-C", ev.Path, "rev-list"}, Result: execx.Result{Stdout: []byte("0\n")}},
		{Prefix: []string{"git"}},
	}}
	f.p.Git = gitx.New(fake)
	if _, err := f.apply(f.plan(Options{External: true, Slot: "repo3"}), false); err != nil {
		t.Fatal(err)
	}
	var mut []string
	for _, c := range fake.Calls {
		if c.Mutates {
			mut = append(mut, strings.Join(c.Args, " "))
		}
	}
	want := []string{
		"-C " + ev.MainClone + " fetch --no-tags origin +refs/heads/master:refs/remotes/origin/master",
		"-C " + ev.Path + " switch --quiet --discard-changes --no-track -C repo3 origin/master",
	}
	if !slices.Equal(mut, want) {
		t.Fatalf("mutating git calls =\n%s\nwant\n%s", strings.Join(mut, "\n"), strings.Join(want, "\n"))
	}
}

func TestApplyRequiresMySQLForDrops(t *testing.T) {
	f := newFixture(t)
	f.orphan("review9", 1)
	f.p.MySQL = nil
	rep, err := f.apply(f.plan(Options{}), false)
	if err == nil || rep.Failed != 1 {
		t.Fatalf("rep = %+v err = %v", rep, err)
	}
}

func TestDropGuardsForPools(t *testing.T) {
	f := newFixture(t)
	g, ok := autoGuard(f.cfg, "review12")
	if !ok || g.Check("talkable_test_shard_1001__review12") != nil {
		t.Fatalf("auto guard = %+v ok=%v", g, ok)
	}
	if _, ok := autoGuard(f.cfg, "pr27087fix"); ok {
		t.Fatal("pr27087fix is not allowlisted")
	}
	if g.Check("talkable_test") == nil || !errors.Is(g.Check("other_test__review1"), mysqlx.ErrGuard) {
		t.Fatal("auto guard too wide")
	}
}

func mustWrite(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
