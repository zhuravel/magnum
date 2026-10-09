package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
)

func TestScanRequiresStoreAndGit(t *testing.T) {
	if _, err := (&Scanner{}).Scan(context.Background(), Options{}); err == nil {
		t.Fatal("want error without Store and Git")
	}
}

func TestSlotLiveFacts(t *testing.T) {
	f := newFixture(t)
	f.slot("review1", store.SlotFree, true)
	pr := f.pr(11920, store.GHOpen, sha("pr-head"))
	a := f.claim(pr.ID, "review1")
	f.worktree(gitx.Worktree{Path: f.path("talkable.review1"), Head: sha("pr-head"), Detached: true})
	f.git.status[f.path("talkable.review1")] = gitx.Status{Tracked: 2, Untracked: 1}
	f.db.add("review1", 12.5, talkableDBs[:7]...) // talkable_test_shard_1001 missing
	f.db.add("review10", 1, talkableDBs[0])       // must not attach to review1

	inv := f.scan(Options{})
	v := slotView(t, inv, "review1")
	if !v.Exists || !v.Worktree {
		t.Fatalf("exists=%v worktree=%v, want both", v.Exists, v.Worktree)
	}
	if v.Head != sha("pr-head") || !v.Detached || v.Branch != "" {
		t.Fatalf("head/branch = %q %q detached=%v", v.Head, v.Branch, v.Detached)
	}
	if v.Dirty == nil || !*v.Dirty || v.Tracked != 2 || v.Untracked != 1 {
		t.Fatalf("dirty = %v tracked=%d untracked=%d", v.Dirty, v.Tracked, v.Untracked)
	}
	if v.PR == nil || v.PR.Number != 11920 || v.PR.GHState != store.GHOpen {
		t.Fatalf("PR = %+v", v.PR)
	}
	if v.Assignment == nil || v.Assignment.ID != a.ID {
		t.Fatalf("assignment = %+v, want id %d", v.Assignment, a.ID)
	}
	if v.Slug != "review1" {
		t.Fatalf("slug = %q", v.Slug)
	}
	if len(v.Databases) != 8 {
		t.Fatalf("databases = %d, want 8 expected names: %+v", len(v.Databases), v.Databases)
	}
	var present int
	for _, d := range v.Databases {
		if !d.Expected {
			t.Errorf("%s not marked expected", d.Name)
		}
		if d.Present {
			present++
			if d.SizeMB != 12.5 {
				t.Errorf("%s size = %v", d.Name, d.SizeMB)
			}
		} else if d.Name != "talkable_test_shard_1001__review1" {
			t.Errorf("%s reported missing", d.Name)
		}
	}
	if present != 7 {
		t.Fatalf("present = %d, want 7", present)
	}
	if !inv.DatabasesListed || len(inv.Databases) != 8 {
		t.Fatalf("listed=%v databases=%d", inv.DatabasesListed, len(inv.Databases))
	}
	if len(inv.Drift) != 1 || inv.Drift[0].Kind != KindOrphanDB { // review10 only
		t.Fatalf("drift = %+v", inv.Drift)
	}
}

func TestLostSlot(t *testing.T) {
	f := newFixture(t)
	f.slot("review2", store.SlotFree, false)
	f.slot("review4", store.SlotRemoved, false) // removed slots are history, not lost

	inv := f.scan(Options{})
	lost := findingsOf(inv, KindLostSlot)
	if len(lost) != 1 || lost[0].Subject != "slot:review2" || !lost[0].Safe {
		t.Fatalf("lost findings = %+v", lost)
	}
	v := slotView(t, inv, "review2")
	if v.Exists || !slices.Contains(v.Drift, KindLostSlot) {
		t.Fatalf("review2 view = exists %v drift %v", v.Exists, v.Drift)
	}
	for _, s := range inv.Slots {
		if s.Slot.Name == "review4" {
			t.Fatal("removed slot listed")
		}
	}
}

func TestUnknownDirNamedLikeSlot(t *testing.T) {
	f := newFixture(t)
	f.slot("review1", store.SlotFree, true)
	f.worktree(gitx.Worktree{Path: f.path("talkable.review1"), Head: sha("m"), Branch: "review1"})
	f.mkdir("talkable.review9")    // not in the registry
	f.mkdir("talkable.reviewx")    // not a slot name
	f.mkdir("talkable.review10.x") // not a slot name either
	f.slot("review5", store.SlotRemoved, false)
	f.mkdir("talkable.review5") // registry says removed, dir is back

	inv := f.scan(Options{})
	got := findingsOf(inv, KindUnknownSlotDir)
	var subjects []string
	for _, g := range got {
		subjects = append(subjects, g.Subject)
		if g.Safe {
			t.Errorf("%s marked safe", g.Subject)
		}
	}
	want := []string{"path:" + f.path("talkable.review5"), "path:" + f.path("talkable.review9")}
	if !slices.Equal(subjects, want) {
		t.Fatalf("unknown dirs = %v, want %v", subjects, want)
	}
}

func TestUnknownPerPRDir(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	clone := f.mkrepo("tool")
	if _, err := f.st.UpsertRepo(ctx, store.Repo{NodeID: "R_tool", Owner: "zhuravel", Name: "tool", Mode: store.RepoModePerPR, ClonePath: new(clone)}); err != nil {
		t.Fatal(err)
	}
	f.git.lists[clone] = []gitx.Worktree{{Path: clone, Head: sha("t"), Branch: "main"}}
	f.mkdir("tool__worktrees/pr-7")
	f.mkdir("tool__worktrees/scratch")

	inv := f.scan(Options{})
	got := findingsOf(inv, KindUnknownSlotDir)
	if len(got) != 1 || got[0].Subject != "path:"+f.path("tool__worktrees/pr-7") {
		t.Fatalf("unknown per-PR dirs = %+v", got)
	}
}

func TestCloneDiscovery(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.cfg.Watches = append(f.cfg.Watches, config.Watch{Owner: "zhuravel", Include: []string{"*"}, CloneRoot: f.root})
	// The registry still records ~/Projects/widgets, a folder that is not a
	// clone; the real clone is ~/Projects/zhuravel-widgets.
	f.mkdir("widgets")
	real := f.mkrepo("zhuravel-widgets")
	for _, r := range []store.Repo{
		{NodeID: "R_tb", Owner: "zhuravel", Name: "widgets", Mode: store.RepoModePerPR, ClonePath: new(f.path("widgets"))},
		{NodeID: "R_ms", Owner: "zhuravel", Name: "marketplace", Mode: store.RepoModePerPR}, // never cloned
	} {
		if _, err := f.st.UpsertRepo(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	f.git.clones["zhuravel/widgets"] = real
	f.git.lists[real] = []gitx.Worktree{{Path: real, Head: sha("tb"), Branch: "main"}}
	f.mkdir("zhuravel-widgets__worktrees/pr-7")

	inv := f.scan(Options{})
	for _, w := range inv.Warnings {
		if strings.Contains(w, "widgets") || strings.Contains(w, "marketplace") || strings.Contains(w, "not a git") {
			t.Fatalf("clone discovery warned: %v", inv.Warnings)
		}
	}
	for _, c := range f.git.calls {
		if c == "worktree-list "+f.path("widgets") {
			t.Fatal("listed worktrees of the folder that is not a clone")
		}
	}
	if !slices.Contains(f.git.calls, "worktree-list "+real) {
		t.Fatalf("the discovered clone was not listed: %v", f.git.calls)
	}
	// The per-PR worktree dir next to the discovered clone is found.
	got := findingsOf(inv, KindUnknownSlotDir)
	if len(got) != 1 || got[0].Subject != "path:"+f.path("zhuravel-widgets__worktrees/pr-7") {
		t.Fatalf("unknown per-PR dirs = %+v", got)
	}

	// A per-PR slot whose recorded main clone is that folder is not listed
	// (no "not a git repository" warning on every scan).
	if _, err := f.st.CreateSlot(ctx, store.Slot{Name: "zhuravel/widgets#3", RepoFullName: "zhuravel/widgets", Kind: store.SlotKindPerPR,
		Path: f.path("widgets__worktrees/pr-3"), MainClone: f.path("widgets"), State: store.SlotProvisioning}); err != nil {
		t.Fatal(err)
	}
	f.git.calls = nil
	inv = f.scan(Options{})
	if slices.Contains(f.git.calls, "worktree-list "+f.path("widgets")) {
		t.Fatalf("listed the non-clone: %v", f.git.calls)
	}
	for _, w := range inv.Warnings {
		if strings.Contains(w, "widgets") {
			t.Fatalf("warned: %v", inv.Warnings)
		}
	}

	// A failing lookup (not "no clone") is a warning.
	f.git.findErr = errors.New("permission denied")
	inv = f.scan(Options{})
	if !slices.ContainsFunc(inv.Warnings, func(w string) bool { return strings.Contains(w, "find the clone of zhuravel/marketplace") }) {
		t.Fatalf("warnings = %v", inv.Warnings)
	}
}

func TestForeignAgentInManagedSlot(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.slot("review1", store.SlotFree, true)
	pr := f.pr(11920, store.GHOpen, sha("h"))
	f.claim(pr.ID, "review1")
	f.worktree(gitx.Worktree{Path: f.path("talkable.review1"), Head: sha("h"), Detached: true})
	if _, err := f.st.CreateSession(ctx, store.Session{
		PRID: pr.ID, Role: store.RoleJudge, AgentName: new("mg-talkable-11920-judge"),
		HerdrPaneID: new("w9:p1"), State: store.SessionLive,
	}); err != nil {
		t.Fatal(err)
	}
	f.hd.snap = herdr.Snapshot{Agents: []herdr.AgentInfo{
		{PaneID: "w9:p1", WorkspaceID: "w9", Name: "mg-talkable-11920-judge", Agent: "codex", AgentStatus: herdr.StatusWorking,
			Cwd: f.path("talkable.review1"), AgentSession: &herdr.AgentSession{Value: "uuid-1"}},
		{PaneID: "w9:p2", WorkspaceID: "w9", Agent: "claude", AgentStatus: herdr.StatusIdle, Cwd: f.path("talkable.review1/app/models")},
		{PaneID: "w9:p3", WorkspaceID: "w9", Name: "mg-talkable-11000-claude", Agent: "claude", AgentStatus: herdr.StatusIdle, Cwd: f.path("talkable.review1")},
		{PaneID: "w8:p1", WorkspaceID: "w8", Agent: "codex", AgentStatus: herdr.StatusWorking, Cwd: f.path("talkable.review10")},
	}}

	inv := f.scan(Options{})
	v := slotView(t, inv, "review1")
	if len(v.Agents) != 3 {
		t.Fatalf("agents = %+v, want 3 (review10 excluded)", v.Agents)
	}
	byPane := map[string]AgentView{}
	for _, a := range v.Agents {
		byPane[a.PaneID] = a
	}
	if a := byPane["w9:p1"]; !a.Magnum || a.SessionID != "uuid-1" || a.Status != herdr.StatusWorking {
		t.Fatalf("judge = %+v", a)
	}
	if byPane["w9:p2"].Magnum {
		t.Fatal("unnamed claude agent counted as magnum")
	}
	if !byPane["w9:p3"].Magnum {
		t.Fatal("mg- prefixed agent counted as foreign")
	}
	got := findingsOf(inv, KindForeignAgent)
	if len(got) != 1 || got[0].Subject != "slot:review1" || got[0].Safe || !strings.Contains(got[0].Message, "w9:p2") {
		t.Fatalf("foreign agents = %+v", got)
	}
}

func TestHeadDrift(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.slot("review1", store.SlotFree, true)
	f.slot("review2", store.SlotFree, true)
	pr := f.pr(11920, store.GHOpen, sha("h"))
	f.claim(pr.ID, "review1")
	if err := f.st.UpdateSlotFields(ctx, f.slots["review1"].ID, func(u *store.SlotUpdate) { u.Set("checked_out_sha", sha("h")) }); err != nil {
		t.Fatal(err)
	}
	// free slot with a stale checked_out_sha is not drift
	if err := f.st.UpdateSlotFields(ctx, f.slots["review2"].ID, func(u *store.SlotUpdate) { u.Set("checked_out_sha", sha("old")) }); err != nil {
		t.Fatal(err)
	}
	f.worktree(gitx.Worktree{Path: f.path("talkable.review1"), Head: sha("someone-committed"), Branch: "hack"})
	f.worktree(gitx.Worktree{Path: f.path("talkable.review2"), Head: sha("master"), Branch: "review2"})

	inv := f.scan(Options{})
	got := findingsOf(inv, KindHeadDrift)
	if len(got) != 1 || got[0].Subject != "slot:review1" || got[0].Safe {
		t.Fatalf("head drift = %+v", got)
	}
}

func TestOpenAssignmentForMissingPR(t *testing.T) {
	f := newFixture(t)
	f.slot("review3", store.SlotFree, true)
	f.worktree(gitx.Worktree{Path: f.path("talkable.review3"), Head: sha("x"), Detached: true})
	pr := f.pr(11999, store.GHOpen, sha("x"))
	f.claim(pr.ID, "review3")
	f.pr(11999, store.GHUnknown, sha("x")) // poller: GitHub said NOT_FOUND

	inv := f.scan(Options{})
	got := findingsOf(inv, KindMissingPR)
	if len(got) != 1 || got[0].Subject != "slot:review3" || got[0].Safe || !strings.Contains(got[0].Message, "11999") {
		t.Fatalf("missing PR findings = %+v", got)
	}
}

func TestOrphanDatabases(t *testing.T) {
	f := newFixture(t)
	f.slot("review1", store.SlotFree, true)
	f.slot("review4", store.SlotRemoved, false)
	f.worktree(gitx.Worktree{Path: f.path("talkable.review1"), Head: sha("m"), Branch: "review1"})
	// external worktrees: slug from file, slug inferred from the dir name, slug inferred from the branch
	f.writeSlug("talkable.repo1", "repo1")
	f.worktree(gitx.Worktree{Path: f.path("talkable.repo1"), Head: sha("r1"), Branch: "zhuravel-PR-27206-x"})
	f.mkdir("talkable.repo6")
	f.worktree(gitx.Worktree{Path: f.path("talkable.repo6"), Head: sha("r6"), Branch: "vo-PR-27510-read"})
	f.mkdir("talkable__worktrees/ubuntu_specs")
	f.worktree(gitx.Worktree{Path: f.path("talkable__worktrees/ubuntu_specs"), Head: sha("u"), Branch: "zhuravel-ubuntu26_ci"})
	f.mkdir("talkable.review9") // unknown dir named like a slot protects its slug
	f.writeSlug("talkable", "mainslug")

	f.db.add("review1", 1, talkableDBs...)
	f.db.add("repo1", 1, talkableDBs[:2]...)
	f.db.add("repo6", 1, talkableDBs[:2]...)
	f.db.add("zhuravel_ubuntu26_ci", 1, talkableDBs[:1]...)
	f.db.add("review9", 1, talkableDBs[:1]...)
	f.db.add("mainslug", 1, talkableDBs[:1]...)
	f.db.add("review4", 2, talkableDBs[:2]...)    // removed slot left DBs behind: magnum-owned orphan
	f.db.add("review7", 3, talkableDBs[:1]...)    // never a slot: magnum-owned orphan
	f.db.add("pr27087fix", 4, talkableDBs[:2]...) // someone's manual slug: orphan, not safe

	inv := f.scan(Options{}) // orphan detection does not need External views
	var names []string
	for _, d := range inv.OrphanDBs {
		names = append(names, d.Name)
	}
	want := []string{
		"talkable_development__pr27087fix", "talkable_development__review4", "talkable_development__review7",
		"talkable_development_reporting__pr27087fix", "talkable_development_reporting__review4",
	}
	if !slices.Equal(names, want) {
		t.Fatalf("orphans = %v\nwant %v", names, want)
	}
	got := findingsOf(inv, KindOrphanDB)
	safe := map[string]bool{}
	for _, g := range got {
		safe[g.Subject] = g.Safe
	}
	wantSafe := map[string]bool{"slug:pr27087fix": false, "slug:review4": true, "slug:review7": true}
	if len(safe) != len(wantSafe) {
		t.Fatalf("orphan findings = %+v", got)
	}
	for k, v := range wantSafe {
		if s, ok := safe[k]; !ok || s != v {
			t.Errorf("%s safe=%v ok=%v, want %v", k, s, ok, v)
		}
	}
	if len(inv.External) != 0 {
		t.Fatalf("External listed without Options.External: %d", len(inv.External))
	}
}

func TestExternalWorktrees(t *testing.T) {
	f := newFixture(t)
	f.slot("review1", store.SlotFree, true)
	f.worktree(gitx.Worktree{Path: f.path("talkable.review1"), Head: sha("m"), Branch: "review1"})
	f.writeSlug("talkable.repo1", "repo1")
	f.worktree(gitx.Worktree{Path: f.path("talkable.repo1"), Head: sha("r1"), Branch: "zhuravel-PR-27206-x"})
	f.git.prs["zhuravel-PR-27206-x"] = 11881 // git config wins over the branch name
	f.mkdir("talkable.repo6")
	f.worktree(gitx.Worktree{Path: f.path("talkable.repo6"), Head: sha("r6"), Branch: "vo-PR-27510-read"})
	f.mkdir("talkable.repo3")
	f.worktree(gitx.Worktree{Path: f.path("talkable.repo3"), Head: sha("r3"), Detached: true})
	f.worktree(gitx.Worktree{Path: f.path("talkable.gone"), Head: sha("g"), Branch: "gone", Prunable: true})
	f.db.add("repo1", 5, talkableDBs[:2]...)
	f.db.add("repo6", 6, talkableDBs[:1]...)
	f.hd.snap = herdr.Snapshot{Agents: []herdr.AgentInfo{
		{PaneID: "w1:p1", Agent: "codex", AgentStatus: herdr.StatusWorking, Cwd: f.path("talkable.repo1")},
	}}

	inv := f.scan(Options{External: true})
	var paths []string
	for _, e := range inv.External {
		paths = append(paths, e.Path)
	}
	want := []string{f.path("talkable.gone"), f.path("talkable.repo1"), f.path("talkable.repo3"), f.path("talkable.repo6")}
	if !slices.Equal(paths, want) {
		t.Fatalf("external = %v\nwant %v (main checkout and managed slots excluded)", paths, want)
	}

	r1 := externalView(t, inv, f.path("talkable.repo1"))
	if r1.Repo != "talkable/talkable" || r1.MainClone != f.main || !r1.Exists {
		t.Fatalf("repo1 = %+v", r1)
	}
	if r1.PRNumber != 11881 || r1.PRSource != PRSourceGitConfig || !r1.PRConfirmed {
		t.Fatalf("repo1 PR = %d %s confirmed=%v", r1.PRNumber, r1.PRSource, r1.PRConfirmed)
	}
	if r1.Slug != "repo1" || r1.SlugSource != SlugSourceFile || len(r1.Databases) != 2 || !r1.Databases[0].Present {
		t.Fatalf("repo1 slug/dbs = %q %q %+v", r1.Slug, r1.SlugSource, r1.Databases)
	}
	if len(r1.Agents) != 1 || r1.Agents[0].Magnum {
		t.Fatalf("repo1 agents = %+v", r1.Agents)
	}

	r6 := externalView(t, inv, f.path("talkable.repo6"))
	if r6.PRNumber != 27510 || r6.PRSource != PRSourceBranchName || r6.PRConfirmed {
		t.Fatalf("repo6 PR = %d %s confirmed=%v", r6.PRNumber, r6.PRSource, r6.PRConfirmed)
	}
	if r6.Slug != "repo6" || r6.SlugSource != SlugSourceInferred {
		t.Fatalf("repo6 slug = %q %q", r6.Slug, r6.SlugSource)
	}

	r3 := externalView(t, inv, f.path("talkable.repo3"))
	if r3.PRNumber != 0 || r3.PRSource != "" || !r3.Detached || r3.Slug != "" {
		t.Fatalf("repo3 = %+v", r3)
	}
	gone := externalView(t, inv, f.path("talkable.gone"))
	if gone.Exists || !gone.Prunable {
		t.Fatalf("gone = %+v", gone)
	}
	if len(f.gh.calls) != 0 {
		t.Fatalf("GitHub called without GitHubStates: %+v", f.gh.calls)
	}
}

func TestBranchPRNumber(t *testing.T) {
	cases := []struct {
		branch string
		want   int
		ok     bool
	}{
		{"zhuravel-PR-27206-nil_incentive_amount_comparisons", 27206, true},
		{"bohdan/PR-26788-campaign-snapshots", 26788, true},
		{"PR-27208-async-segmentize", 27208, true},
		{"vo-PR-27313", 27313, true},
		{"zhuravel-PS-39518-bayport_geo_ip_report", 0, false},
		{"repo10", 0, false},
		{"pr-12", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := branchPRNumber(c.branch)
		if got != c.want || ok != c.ok {
			t.Errorf("branchPRNumber(%q) = %d,%v want %d,%v", c.branch, got, ok, c.want, c.ok)
		}
	}
}

func TestExternalGitHubStatesBatchedPerRepo(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.mkdir("talkable.repo1")
	f.worktree(gitx.Worktree{Path: f.path("talkable.repo1"), Head: sha("r1"), Branch: "a"})
	f.git.prs["a"] = 11881
	f.mkdir("talkable.repo2")
	f.worktree(gitx.Worktree{Path: f.path("talkable.repo2"), Head: sha("r2"), Branch: "b"})
	f.git.prs["b"] = 11932
	f.mkdir("talkable.repo5")
	f.worktree(gitx.Worktree{Path: f.path("talkable.repo5"), Head: sha("r5"), Branch: "c-PR-27143-x"}) // guess, head matches
	f.mkdir("talkable.repo6")
	f.worktree(gitx.Worktree{Path: f.path("talkable.repo6"), Head: sha("r6"), Branch: "d-PR-27510-x"}) // guess, head differs
	f.mkdir("talkable.repo7")
	f.worktree(gitx.Worktree{Path: f.path("talkable.repo7"), Head: sha("r7"), Branch: "e-PR-99999-x"}) // guess, not found
	// a per-PR repo with its own clone and one external worktree
	clone := f.mkrepo("tool")
	if _, err := f.st.UpsertRepo(ctx, store.Repo{NodeID: "R_tool", Owner: "zhuravel", Name: "tool", Mode: store.RepoModePerPR, ClonePath: new(clone)}); err != nil {
		t.Fatal(err)
	}
	f.mkdir("tool-feature")
	f.git.lists[clone] = []gitx.Worktree{{Path: clone, Head: sha("t"), Branch: "main"}, {Path: f.path("tool-feature"), Head: sha("tf"), Branch: "feat"}}
	f.git.prs["feat"] = 3

	merged := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	f.gh.states = map[int]github.PRState{
		11881: {State: "MERGED", Merged: true, MergedAt: merged, HeadRefOid: sha("other")},
		11932: {State: "OPEN", HeadRefOid: sha("r2")},
		27143: {State: "CLOSED", ClosedAt: merged, HeadRefOid: sha("r5")},
		27510: {State: "CLOSED", ClosedAt: merged, HeadRefOid: sha("not-r6")},
		3:     {State: "MERGED", Merged: true, MergedAt: merged},
	}
	f.gh.notFound = []int{99999}

	inv := f.scan(Options{External: true, GitHubStates: true})
	if len(f.gh.calls) != 2 {
		t.Fatalf("ConfirmStates calls = %+v, want one per repo", f.gh.calls)
	}
	byRepo := map[string][]int{}
	for _, c := range f.gh.calls {
		byRepo[c.Owner+"/"+c.Repo] = c.Numbers
	}
	if got := byRepo["talkable/talkable"]; !slices.Equal(got, []int{11881, 11932, 27143, 27510, 99999}) {
		t.Fatalf("talkable numbers = %v", got)
	}
	if got := byRepo["zhuravel/tool"]; !slices.Equal(got, []int{3}) {
		t.Fatalf("tool numbers = %v", got)
	}

	r1 := externalView(t, inv, f.path("talkable.repo1"))
	if r1.GHState != "MERGED" || r1.StateSource != StateSourceGitHub || r1.MergedAt == nil || !r1.MergedAt.Equal(merged) {
		t.Fatalf("repo1 state = %+v", r1)
	}
	if r7 := externalView(t, inv, f.path("talkable.repo7")); r7.GHState != store.GHUnknown || r7.PRConfirmed {
		t.Fatalf("repo7 = %s confirmed=%v", r7.GHState, r7.PRConfirmed)
	}
	if r5 := externalView(t, inv, f.path("talkable.repo5")); !r5.PRConfirmed {
		t.Fatal("branch-name guess with matching head not confirmed")
	}
	if r6 := externalView(t, inv, f.path("talkable.repo6")); r6.PRConfirmed || r6.GHState != "CLOSED" {
		t.Fatalf("repo6 = %s confirmed=%v", r6.GHState, r6.PRConfirmed)
	}

	var subjects []string
	for _, g := range findingsOf(inv, KindExternalClosed) {
		subjects = append(subjects, g.Subject)
		if g.Safe {
			t.Errorf("%s marked safe", g.Subject)
		}
	}
	want := []string{"path:" + f.path("talkable.repo1"), "path:" + f.path("talkable.repo5"), "path:" + f.path("tool-feature")}
	if !slices.Equal(subjects, want) {
		t.Fatalf("closed externals = %v\nwant %v", subjects, want)
	}
	if v := externalView(t, inv, f.path("talkable.repo1")); !slices.Contains(v.Drift, KindExternalClosed) {
		t.Fatalf("repo1 drift flags = %v", v.Drift)
	}
}

func TestExternalStateFromStoreWithoutGitHub(t *testing.T) {
	f := newFixture(t)
	f.mkdir("talkable.repo2")
	f.worktree(gitx.Worktree{Path: f.path("talkable.repo2"), Head: sha("r2"), Branch: "b"})
	f.git.prs["b"] = 11932
	f.pr(11932, store.GHMerged, sha("r2"))

	inv := f.scan(Options{External: true})
	v := externalView(t, inv, f.path("talkable.repo2"))
	if v.GHState != store.GHMerged || v.StateSource != StateSourceStore {
		t.Fatalf("state = %q from %q", v.GHState, v.StateSource)
	}
	if len(findingsOf(inv, KindExternalClosed)) != 1 {
		t.Fatalf("drift = %+v", inv.Drift)
	}
	if len(f.gh.calls) != 0 {
		t.Fatal("GitHub called")
	}
}

func TestGitHubErrorIsAWarning(t *testing.T) {
	f := newFixture(t)
	f.mkdir("talkable.repo2")
	f.worktree(gitx.Worktree{Path: f.path("talkable.repo2"), Head: sha("r2"), Branch: "b"})
	f.git.prs["b"] = 11932
	f.gh.err = errors.New("gh: token ghp_abcdefghijklmnopqrstuvwxyz0123456789 rejected")

	inv := f.scan(Options{External: true, GitHubStates: true})
	if len(inv.Warnings) != 1 || !strings.HasPrefix(inv.Warnings[0], "github:") {
		t.Fatalf("warnings = %v", inv.Warnings)
	}
	if strings.Contains(inv.Warnings[0], "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("token leaked into warning: %s", inv.Warnings[0])
	}
	if v := externalView(t, inv, f.path("talkable.repo2")); v.GHState != "" {
		t.Fatalf("state = %q", v.GHState)
	}
}

func TestDegradedSources(t *testing.T) {
	f := newFixture(t)
	f.slot("review1", store.SlotFree, true)
	f.worktree(gitx.Worktree{Path: f.path("talkable.review1"), Head: sha("m"), Branch: "review1"})
	f.db.err = errors.New("dial tcp 127.0.0.1:3306: connection refused")
	f.hd.err = herdr.ErrUnavailable
	f.git.statusErr[f.path("talkable.review1")] = errors.New("index locked")

	inv := f.scan(Options{})
	if inv.DatabasesListed || len(inv.OrphanDBs) != 0 || len(findingsOf(inv, KindOrphanDB)) != 0 {
		t.Fatalf("listed=%v orphans=%v", inv.DatabasesListed, inv.OrphanDBs)
	}
	joined := strings.Join(inv.Warnings, "\n")
	for _, w := range []string{"mysql:", "herdr:", "git:"} {
		if !strings.Contains(joined, w) {
			t.Errorf("warnings missing %q: %v", w, inv.Warnings)
		}
	}
	if v := slotView(t, inv, "review1"); v.Dirty != nil {
		t.Fatalf("dirty = %v, want unknown", *v.Dirty)
	}
	if _, err := f.scanner().UpsertSlotDatabases(context.Background(), inv); !errors.Is(err, ErrNotListed) {
		t.Fatalf("UpsertSlotDatabases err = %v, want ErrNotListed", err)
	}
}

func TestMissingMainCloneIsAWarning(t *testing.T) {
	f := newFixture(t)
	f.slot("review1", store.SlotFree, true)
	f.git.listErr[f.main] = errors.New("not a git repository")

	inv := f.scan(Options{External: true})
	v := slotView(t, inv, "review1")
	if !v.Exists || v.Worktree {
		t.Fatalf("exists=%v worktree=%v", v.Exists, v.Worktree)
	}
	if len(findingsOf(inv, KindHeadDrift)) != 0 {
		t.Fatal("head drift without git facts")
	}
	if !strings.Contains(strings.Join(inv.Warnings, "\n"), "git:") {
		t.Fatalf("warnings = %v", inv.Warnings)
	}
}

func TestUpsertSlotDatabases(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.slot("review1", store.SlotFree, true)
	f.worktree(gitx.Worktree{Path: f.path("talkable.review1"), Head: sha("m"), Branch: "review1"})
	f.db.add("review1", 10, talkableDBs[:2]...)
	f.db.add("review7", 3, talkableDBs[:1]...)
	s := f.scanner()

	inv := f.scan(Options{})
	res, err := s.UpsertSlotDatabases(ctx, inv)
	if err != nil {
		t.Fatal(err)
	}
	if res.Seen != 3 || len(res.Dropped) != 0 {
		t.Fatalf("result = %+v", res)
	}
	rows, err := f.st.ListSlotDatabases(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	for _, r := range rows {
		switch r.Slug {
		case "review1":
			if r.SlotID == nil || *r.SlotID != f.slots["review1"].ID || r.SizeMB == nil || *r.SizeMB != 10 {
				t.Errorf("%s = slot %v size %v", r.DBName, r.SlotID, r.SizeMB)
			}
		case "review7":
			if r.SlotID != nil {
				t.Errorf("orphan %s attached to slot %d", r.DBName, *r.SlotID)
			}
		}
	}

	f.db.remove("talkable_development__review7")
	inv = f.scan(Options{})
	res, err = s.UpsertSlotDatabases(ctx, inv)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Dropped, []string{"talkable_development__review7"}) {
		t.Fatalf("dropped = %v", res.Dropped)
	}
	all, err := f.st.ListSlotDatabases(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all {
		if r.DBName == "talkable_development__review7" {
			if r.DroppedAt == nil || store.Deref(r.DroppedBy) != droppedBy {
				t.Fatalf("review7 row = %+v", r)
			}
		} else if r.DroppedAt != nil {
			t.Fatalf("%s marked dropped", r.DBName)
		}
	}
	// a second pass does not re-mark already dropped rows
	if res, err = s.UpsertSlotDatabases(ctx, inv); err != nil || len(res.Dropped) != 0 {
		t.Fatalf("second pass = %+v, %v", res, err)
	}
}

func TestSizes(t *testing.T) {
	f := newFixture(t)
	f.slot("review1", store.SlotFree, true)
	f.slot("review2", store.SlotFree, false) // lost: no du
	f.worktree(gitx.Worktree{Path: f.path("talkable.review1"), Head: sha("m"), Branch: "review1"})
	f.mkdir("talkable.repo1")
	f.worktree(gitx.Worktree{Path: f.path("talkable.repo1"), Head: sha("r1"), Branch: "x"})
	run := &execx.Fake{Rules: []execx.Rule{{
		Prefix: []string{"du", "-sk"},
		Fn: func(c execx.Cmd) (execx.Result, error) {
			p := c.Args[len(c.Args)-1]
			if strings.HasSuffix(p, "repo1") {
				// permission errors: du exits 1 but still prints the total
				res := execx.Result{Stdout: []byte("2048\t" + p + "\n"), Code: 1}
				return res, &execx.ExitError{Cmd: c, Code: 1, Stderr: "du: x: Permission denied"}
			}
			return execx.Result{Stdout: []byte("1613544\t" + p + "\n")}, nil
		},
	}}}
	s := f.scanner()
	s.Runner = run

	inv, err := s.Scan(context.Background(), Options{Sizes: true, External: true})
	if err != nil {
		t.Fatal(err)
	}
	if v := slotView(t, inv, "review1"); v.SizeKB == nil || *v.SizeKB != 1613544 {
		t.Fatalf("review1 size = %v", v.SizeKB)
	}
	if v := slotView(t, inv, "review2"); v.SizeKB != nil {
		t.Fatalf("lost slot sized: %v", *v.SizeKB)
	}
	if v := externalView(t, inv, f.path("talkable.repo1")); v.SizeKB == nil || *v.SizeKB != 2048 {
		t.Fatalf("repo1 size = %v", v.SizeKB)
	}
	calls := run.CallsWithPrefix("du")
	if len(calls) != 2 {
		t.Fatalf("du calls = %d", len(calls))
	}
	for _, c := range calls {
		if c.Mutates {
			t.Fatalf("du marked mutating: %s", c)
		}
	}

	// no sizes without the option
	run.Calls = nil
	if _, err := s.Scan(context.Background(), Options{External: true}); err != nil {
		t.Fatal(err)
	}
	if len(run.Calls) != 0 {
		t.Fatalf("du ran without Options.Sizes: %v", run.Calls)
	}
}

func TestParseDU(t *testing.T) {
	cases := map[string]int64{"1613544\t/x\n": 1613544, "12 /a b\n": 12, "": -1, "abc\t/x": -1}
	for in, want := range cases {
		got, ok := parseDU([]byte(in))
		if (want < 0) == ok || (ok && got != want) {
			t.Errorf("parseDU(%q) = %d,%v want %d", in, got, ok, want)
		}
	}
}

func TestDeterministicOrderAndJSON(t *testing.T) {
	f := newFixture(t)
	for _, n := range []string{"review10", "review2", "review1"} {
		f.slot(n, store.SlotFree, true)
		f.worktree(gitx.Worktree{Path: f.path("talkable." + n), Head: sha(n), Branch: n})
	}
	for _, n := range []string{"repo10", "repo2", "repo1"} {
		f.mkdir("talkable." + n)
		f.worktree(gitx.Worktree{Path: f.path("talkable." + n), Head: sha(n), Branch: n})
	}
	f.db.add("zeta", 1, talkableDBs[1], talkableDBs[0])
	f.db.add("alpha", 1, talkableDBs[1], talkableDBs[0])
	f.slot("review3", store.SlotFree, false)

	inv := f.scan(Options{External: true})
	var names []string
	for _, s := range inv.Slots {
		names = append(names, s.Slot.Name)
	}
	if !slices.Equal(names, []string{"review1", "review2", "review3", "review10"}) {
		t.Fatalf("slot order = %v", names)
	}
	var ext []string
	for _, e := range inv.External {
		ext = append(ext, e.Path)
	}
	if want := []string{f.path("talkable.repo1"), f.path("talkable.repo2"), f.path("talkable.repo10")}; !slices.Equal(ext, want) {
		t.Fatalf("external order = %v\nwant %v", ext, want)
	}
	wantDBs := []string{
		"talkable_development__alpha", "talkable_development__zeta",
		"talkable_development_reporting__alpha", "talkable_development_reporting__zeta",
	}
	if got := dbNames(inv.Databases); !slices.Equal(got, wantDBs) {
		t.Fatalf("databases order = %v\nwant %v", got, wantDBs)
	}
	if got := dbNames(inv.OrphanDBs); !slices.Equal(got, wantDBs) {
		t.Fatalf("orphan order = %v\nwant %v", got, wantDBs)
	}
	var kinds []string
	for _, d := range inv.Drift {
		kinds = append(kinds, d.Kind+" "+d.Subject)
	}
	if !slices.IsSorted(kinds) {
		t.Fatalf("drift not sorted: %v", kinds)
	}
	if !inv.ScannedAt.Equal(f.now) {
		t.Fatalf("scanned at = %v", inv.ScannedAt)
	}
	b, err := json.Marshal(inv)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"slots"`, `"external"`, `"drift"`, `"orphan_dbs"`, `"databases_listed"`, `"agents_listed"`, `"clones_listed"`, `"orphans_known"`, `"scanned_at"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("JSON missing %s", key)
		}
	}
	again, err := json.Marshal(f.scan(Options{External: true}))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, again) {
		t.Fatalf("second scan differs:\n%s\n%s", b, again)
	}
}

func TestNaturalLess(t *testing.T) {
	in := []string{"review10", "review2", "review1", "repo12", "repo1", "repo9", "a", ""}
	slices.SortFunc(in, func(a, b string) int {
		switch {
		case NaturalLess(a, b):
			return -1
		case NaturalLess(b, a):
			return 1
		}
		return 0
	})
	want := []string{"", "a", "repo1", "repo9", "repo12", "review1", "review2", "review10"}
	if !slices.Equal(in, want) {
		t.Fatalf("got %v", in)
	}
}

func TestPoolGuard(t *testing.T) {
	f := newFixture(t)
	g := slots.DropGuard(f.cfg.Pools[0])
	if g.Prefix != "talkable_" {
		t.Fatalf("prefix = %q", g.Prefix)
	}
	if err := g.Check("talkable_test__review12"); err != nil {
		t.Fatalf("review12 refused: %v", err)
	}
	for _, n := range []string{"talkable_test__repo1", "talkable_test__review", "talkable_test__review1x", "talkable_test"} {
		if err := g.Check(n); !errors.Is(err, mysqlx.ErrGuard) {
			t.Errorf("%s allowed (err %v)", n, err)
		}
	}
}

func TestPathUnder(t *testing.T) {
	root := filepath.FromSlash("/p/talkable.review1")
	cases := map[string]bool{
		"/p/talkable.review1":      true,
		"/p/talkable.review1/app":  true,
		"/p/talkable.review1/":     true,
		"/p/talkable.review10":     false,
		"/p/talkable.review10/app": false,
		"/p/talkable":              false,
		"":                         false,
	}
	for p, want := range cases {
		if got := under(p, root); got != want {
			t.Errorf("under(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestDatabasesListedByPoolPrefixes(t *testing.T) {
	t.Run("listed by the pool's prefixes", func(t *testing.T) {
		f := newFixture(t)
		f.cfg.Pools[0].Databases = []string{"example_test__{slug}", "example_development__{slug}"}
		my := &fakeMySQL{}
		my.add("review1", 1, "example_test", "example_development")
		my.add("review1", 1, "talkable_development") // not a template of this pool
		f.slot("review1", store.SlotFree, true)
		f.worktree(gitx.Worktree{Path: f.path("talkable.review1"), Head: sha("m"), Branch: "review1"})
		s := f.scanner()
		s.MySQL = my

		inv, err := s.Scan(context.Background(), Options{})
		if err != nil {
			t.Fatal(err)
		}
		if len(my.prefixes) != 1 || !slices.Equal(my.prefixes[0], []string{"example_development__", "example_test__"}) {
			t.Fatalf("ListPrefixed calls = %v", my.prefixes)
		}
		if got, want := dbNames(inv.Databases), []string{"example_development__review1", "example_test__review1"}; !inv.DatabasesListed || !slices.Equal(got, want) {
			t.Fatalf("listed=%v databases = %v, want %v", inv.DatabasesListed, got, want)
		}
		if !inv.OrphansKnown || len(inv.OrphanDBs) != 0 {
			t.Fatalf("orphans known=%v %v", inv.OrphansKnown, inv.OrphanDBs)
		}
	})
	t.Run("a database no template names is not listed", func(t *testing.T) {
		f := newFixture(t)
		f.db.add("review7", 1, talkableDBs[0])
		f.db.add("review7", 1, "talkable_other") // no pool template names it

		inv := f.scan(Options{})
		want := []string{"talkable_development__review7"}
		if got := dbNames(inv.Databases); !slices.Equal(got, want) {
			t.Fatalf("databases = %v, want %v", got, want)
		}
		if got := dbNames(inv.OrphanDBs); !slices.Equal(got, want) {
			t.Fatalf("orphans = %v, want %v", got, want)
		}
	})
	t.Run("bad template is a warning", func(t *testing.T) {
		f := newFixture(t)
		f.cfg.Pools[0].Databases = append(dbTemplates(), "talkable_cache_{slug}")
		f.db.add("review7", 1, talkableDBs[0])

		inv := f.scan(Options{})
		if !hasWarning(inv, `config: database template "talkable_cache_{slug}" does not end in __{slug}; its databases are not listed`, "") {
			t.Fatalf("warnings = %v", inv.Warnings)
		}
		if !inv.DatabasesListed || len(inv.Databases) != 1 {
			t.Fatalf("listed=%v databases=%v", inv.DatabasesListed, inv.Databases)
		}
	})
}

func TestOrphansNeedCompleteDiscovery(t *testing.T) {
	setup := func(t *testing.T) *fixture {
		f := newFixture(t)
		f.slot("review1", store.SlotFree, true)
		f.worktree(gitx.Worktree{Path: f.path("talkable.review1"), Head: sha("m"), Branch: "review1"})
		f.db.add("review1", 1, talkableDBs[0])
		f.db.add("review7", 1, talkableDBs[0]) // an orphan when discovery is complete
		return f
	}
	suppressed := func(t *testing.T, inv Inventory, missing string) {
		t.Helper()
		if inv.OrphansKnown || len(inv.OrphanDBs) != 0 || len(findingsOf(inv, KindOrphanDB)) != 0 {
			t.Fatalf("orphans computed on incomplete facts: known=%v %v %+v", inv.OrphansKnown, inv.OrphanDBs, inv.Drift)
		}
		if !inv.DatabasesListed {
			t.Fatal("databases not listed")
		}
		if !hasWarning(inv, "discovery incomplete:", missing) {
			t.Fatalf("no discovery incomplete warning naming %q: %v", missing, inv.Warnings)
		}
	}

	t.Run("complete", func(t *testing.T) {
		f := setup(t)
		inv := f.scan(Options{})
		if !inv.OrphansKnown || !inv.ClonesListed || !inv.AgentsListed || !slices.Equal(dbNames(inv.OrphanDBs), []string{"talkable_development__review7"}) {
			t.Fatalf("known=%v clones=%v agents=%v orphans=%v", inv.OrphansKnown, inv.ClonesListed, inv.AgentsListed, inv.OrphanDBs)
		}
		if hasWarning(inv, "discovery incomplete:", "") {
			t.Fatalf("warnings = %v", inv.Warnings)
		}
	})
	t.Run("no pool configured", func(t *testing.T) {
		f := newFixture(t)
		f.cfg.Pools = nil
		inv := f.scan(Options{})
		if !inv.OrphansKnown || !inv.DatabasesListed || !inv.ClonesListed || len(inv.Warnings) != 0 {
			t.Fatalf("known=%v listed=%v clones=%v warnings=%v", inv.OrphansKnown, inv.DatabasesListed, inv.ClonesListed, inv.Warnings)
		}
	})
	t.Run("mysql not listed", func(t *testing.T) {
		f := setup(t)
		f.db.err = errors.New("connection refused")
		inv := f.scan(Options{})
		if inv.OrphansKnown || inv.DatabasesListed || len(inv.OrphanDBs) != 0 || !hasWarning(inv, "discovery incomplete:", "databases") {
			t.Fatalf("known=%v listed=%v orphans=%v warnings=%v", inv.OrphansKnown, inv.DatabasesListed, inv.OrphanDBs, inv.Warnings)
		}
	})
	t.Run("clone listing fails", func(t *testing.T) {
		f := setup(t)
		f.git.listErr[f.main] = errors.New("not a git repository")
		inv := f.scan(Options{})
		if inv.ClonesListed {
			t.Fatal("ClonesListed with a failed worktree list")
		}
		suppressed(t, inv, "git worktree list of "+f.main+" failed")
	})
	t.Run("clone discovery fails", func(t *testing.T) {
		f := setup(t)
		f.cfg.Watches = append(f.cfg.Watches, config.Watch{Owner: "example", Include: []string{"*"}, CloneRoot: f.root})
		if _, err := f.st.UpsertRepo(context.Background(), store.Repo{NodeID: "R_ex", Owner: "example", Name: "tool", Mode: store.RepoModePerPR}); err != nil {
			t.Fatal(err)
		}
		f.git.findErr = errors.New("permission denied")
		inv := f.scan(Options{})
		if inv.ClonesListed {
			t.Fatal("ClonesListed with a failed clone lookup")
		}
		suppressed(t, inv, "example/tool")
	})
	t.Run("slug marker unreadable", func(t *testing.T) {
		f := setup(t)
		f.writeSlug("talkable.repo1", "repo1")
		f.worktree(gitx.Worktree{Path: f.path("talkable.repo1"), Head: sha("r1"), Branch: "x"})
		unreadable(t, f.path("talkable.repo1/tmp"))
		inv := f.scan(Options{})
		if !inv.ClonesListed {
			t.Fatal("ClonesListed false")
		}
		if !hasWarning(inv, "fs:", "talkable.repo1") {
			t.Fatalf("warnings = %v", inv.Warnings)
		}
		suppressed(t, inv, "talkable.repo1")
	})
}

func TestAgentsListed(t *testing.T) {
	f := newFixture(t)
	if inv := f.scan(Options{}); !inv.AgentsListed {
		t.Fatal("AgentsListed false after a snapshot")
	}
	f.hd.err = herdr.ErrUnavailable
	if inv := f.scan(Options{}); inv.AgentsListed {
		t.Fatal("AgentsListed with a failed snapshot")
	}
	s := f.scanner()
	s.Herdr = nil
	inv, err := s.Scan(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if inv.AgentsListed {
		t.Fatal("AgentsListed without a herdr client")
	}
}

func TestLostSlotSkipsProvisioningAndRemoving(t *testing.T) {
	f := newFixture(t)
	f.slot("review1", store.SlotProvisioning, false) // row written before the directory
	f.slot("review2", store.SlotRemoving, false)     // directory goes away mid-removal
	f.slot("review3", store.SlotBroken, false)

	inv := f.scan(Options{})
	lost := findingsOf(inv, KindLostSlot)
	if len(lost) != 1 || lost[0].Subject != "slot:review3" {
		t.Fatalf("lost findings = %+v", lost)
	}
	for _, n := range []string{"review1", "review2"} {
		if v := slotView(t, inv, n); v.Exists || len(v.Drift) != 0 {
			t.Fatalf("%s = exists %v drift %v", n, v.Exists, v.Drift)
		}
	}
}

func TestUnreadableSlotDirIsNotLost(t *testing.T) {
	f := newFixture(t)
	locked := f.mkdir("locked")
	f.mkdir("locked/talkable.review1")
	f.slotAt("review1", filepath.Join(locked, "talkable.review1"), store.SlotFree)
	f.db.add("review1", 1, talkableDBs[0])
	f.db.add("review7", 1, talkableDBs[0])
	unreadable(t, locked)

	inv := f.scan(Options{})
	if v := slotView(t, inv, "review1"); !v.Exists {
		t.Fatal("unreadable slot directory reported missing")
	}
	if lost := findingsOf(inv, KindLostSlot); len(lost) != 0 {
		t.Fatalf("lost findings = %+v", lost)
	}
	if inv.OrphansKnown || len(inv.OrphanDBs) != 0 {
		t.Fatalf("orphans known=%v %v", inv.OrphansKnown, inv.OrphanDBs)
	}
	if !hasWarning(inv, "fs:", "talkable.review1") || !hasWarning(inv, "discovery incomplete:", "talkable.review1") {
		t.Fatalf("warnings = %v", inv.Warnings)
	}
	if _, err := os.Stat(filepath.Join(locked, "talkable.review1")); err == nil {
		t.Fatal("directory readable: the test does not exercise permissions")
	}
}

func TestUnknownPerPRDirProtectsItsDatabases(t *testing.T) {
	f := newFixture(t)
	clone := f.mkrepo("tool")
	if _, err := f.st.UpsertRepo(context.Background(), store.Repo{NodeID: "R_tool", Owner: "example", Name: "tool", Mode: store.RepoModePerPR, ClonePath: new(clone)}); err != nil {
		t.Fatal(err)
	}
	f.git.lists[clone] = []gitx.Worktree{{Path: clone, Head: sha("t"), Branch: "main"}}
	f.writeSlug("tool__worktrees/pr-7", "custom7")
	f.mkdir("tool__worktrees/pr-9")
	f.db.add("custom7", 1, talkableDBs[0])     // pr-7's marker
	f.db.add("magnum_pr_7", 1, talkableDBs[0]) // pr-7's default per-PR slug
	f.db.add("magnum_pr_9", 1, talkableDBs[0]) // pr-9's default per-PR slug
	f.db.add("magnum_pr_8", 1, talkableDBs[0]) // no directory: orphan

	inv := f.scan(Options{})
	var subjects []string
	for _, g := range findingsOf(inv, KindUnknownSlotDir) {
		subjects = append(subjects, g.Subject)
	}
	if want := []string{"path:" + f.path("tool__worktrees/pr-7"), "path:" + f.path("tool__worktrees/pr-9")}; !slices.Equal(subjects, want) {
		t.Fatalf("unknown dirs = %v, want %v", subjects, want)
	}
	if got, want := dbNames(inv.OrphanDBs), []string{"talkable_development__magnum_pr_8"}; !inv.OrphansKnown || !slices.Equal(got, want) {
		t.Fatalf("known=%v orphans = %v, want %v", inv.OrphansKnown, got, want)
	}
}

// filepath.Glob skips unreadable directories; an unreadable
// <clone>__worktrees may hold per-PR worktrees that own databases.
func TestUnreadableWorktreesDirSuppressesOrphans(t *testing.T) {
	f := newFixture(t)
	clone := f.mkrepo("tool")
	if _, err := f.st.UpsertRepo(context.Background(), store.Repo{NodeID: "R_tool", Owner: "example", Name: "tool", Mode: store.RepoModePerPR, ClonePath: new(clone)}); err != nil {
		t.Fatal(err)
	}
	f.git.lists[clone] = []gitx.Worktree{{Path: clone, Head: sha("t"), Branch: "main"}}
	f.writeSlug("tool__worktrees/pr-7", "custom7")
	f.db.add("custom7", 1, talkableDBs[0])
	unreadable(t, f.path("tool__worktrees"))

	inv := f.scan(Options{})
	if inv.OrphansKnown || len(inv.OrphanDBs) != 0 {
		t.Fatalf("orphans computed under an unreadable directory: known=%v %v", inv.OrphansKnown, inv.OrphanDBs)
	}
	if !hasWarning(inv, "discovery incomplete:", "tool__worktrees unreadable") {
		t.Fatalf("warnings = %v", inv.Warnings)
	}
}
