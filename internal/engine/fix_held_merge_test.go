package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// heldGitFake is mergeGit's held merge (heldGit): the refs UpdateRef wrote
// ("<dir> <ref>" to the commit) and UpdateRefDelete deleted, a commit's
// parents (parents, by commit; else the handed merge's, else the head's
// and the base's tip), and RemergeDiff's and PathsDiff's answers.
type heldGitFake struct {
	refs    map[string]string
	deleted []string
	parents map[string][]string
	remerge string
	diff    string
}

func (g *mergeGit) UpdateRef(_ context.Context, dir, ref, sha string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held.refs == nil {
		g.held.refs = map[string]string{}
	}
	g.held.refs[dir+" "+ref] = sha
	return nil
}

func (g *mergeGit) UpdateRefDelete(_ context.Context, dir, ref string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.held.refs, dir+" "+ref)
	g.held.deleted = append(g.held.deleted, dir+" "+ref)
	return nil
}

func (g *mergeGit) Parents(_ context.Context, _, rev string) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if p, ok := g.held.parents[rev]; ok {
		return slices.Clone(p), nil
	}
	if g.handed != nil {
		return slices.Clone(g.handed.Parents), nil
	}
	return []string{fixHead, fixBaseTip}, nil
}

func (g *mergeGit) RemergeDiff(context.Context, string, string, ...string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.held.remerge, nil
}

func (g *mergeGit) PathsDiff(context.Context, string, string, string, ...string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.held.diff, nil
}

// heldRefs are the held merges' refs mergeGit holds.
func (g *mergeGit) heldRefs() map[string]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return maps.Clone(g.held.refs)
}

const (
	// heldRemerge and heldPlain are held-merge.diff's two parts: how the
	// fixer resolved ci.yml's conflict, and what the push changes in the
	// workflow files against the PR's head.
	heldRemerge = "diff --git a/.github/workflows/ci.yml b/.github/workflows/ci.yml\nremerge CONFLICT (content): Merge conflict in .github/workflows/ci.yml\n" +
		"-<<<<<<< a1a1a1a\n-jobs: pr\n-=======\n-jobs: base\n->>>>>>> c0c0c0c\n+jobs: resolved\n"
	heldPlain = "diff --git a/.github/workflows/ci.yml b/.github/workflows/ci.yml\n-jobs: pr\n+jobs: resolved\n" +
		"diff --git a/.github/workflows/pronto.yml b/.github/workflows/pronto.yml\n-on: push\n+on: pull_request\n"
	// heldAttention is PR #2's attention while claude-fix's merge is held.
	heldAttention = "fix: merge ready: claude-fix merged master at c0c0c0c and resolved 5 conflicts. It changes .github/workflows/ci.yml and " +
		"pronto.yml. " + fixerAppLogin + " pushes it only after your y: P asks (e shows held-merge.diff)."
)

// heldConflicts makes the fixer's merge resolve five conflicts, two in
// workflow files both the base and the PR changed, and keep the PR's own
// workflow changes: pushMerge refuses it for those alone.
func heldConflicts(mg *mergeGit) {
	mg.conflict(".github/workflows/ci.yml", ".github/workflows/pronto.yml", "app/a.rb", "app/b.rb", "app/c.rb")
	mg.mu.Lock()
	defer mg.mu.Unlock()
	mg.workflows = []string{".github/workflows/ci.yml", ".github/workflows/pronto.yml"}
	mg.own = []string{".github/workflows/ci.yml"}
	mg.held.remerge, mg.held.diff = heldRemerge, heldPlain
}

// holdMerged runs the operator's merge of PR #2's parent to its end: the
// fixer resolves the conflicts and hands its merge to magnum.
func holdMerged(t *testing.T, mods ...func(*harness)) (*harness, *mergeGit) {
	t.Helper()
	h, mg := handedMerge(t, mods...)
	heldConflicts(mg)
	id := h.fix(FixPayload{Mode: FixModeMerge})
	h.handle()
	h.wantDone(id)
	return h, mg
}

// heldDiffPath is where PR #2's held-merge.diff is.
func (h *harness) heldDiffPath() string {
	return filepath.Join(h.d.Layout.PRReviews("talkable", "talkable", 2), HeldMergeFile)
}

// heldRefKey is how mergeGit names PR #2's held ref in the slot's clone.
func (h *harness) heldRefKey() string {
	return h.slot("review1").MainClone + " " + HeldMergeRef("talkable", "talkable", 2)
}

// heldMergeOfPR is PR #2's held merge (ok false: none).
func (h *harness) heldMergeOfPR() (HeldMerge, bool) {
	h.t.Helper()
	raw, ok, err := h.st.GetKV(h.ctx, KVPRHeldMerge(h.pr(2).ID))
	if err != nil {
		h.t.Fatal(err)
	}
	if !ok {
		return HeldMerge{}, false
	}
	held, ok := ParseHeldMerge(raw)
	if !ok {
		h.t.Fatalf("the held merge's row does not read: %q", raw)
	}
	return held, true
}

// wantNothingPushed fails when magnum pushed anything or minted a push
// token.
func wantNothingPushed(t *testing.T, h *harness, mg *mergeGit) {
	t.Helper()
	if _, pushes := mg.made(); len(pushes) != 0 {
		t.Errorf("pushes = %v, want none", pushes)
	}
	if mints, _ := h.ids[fixerAppName].daemonPushes(); len(mints) != 0 {
		t.Errorf("daemon push tokens minted: %v", mints)
	}
}

// wantDropped fails unless PR #2's held merge is gone: its ref, its row
// and its held-merge.diff, with a fix.held_dropped event that says why.
func wantDropped(t *testing.T, h *harness, mg *mergeGit, why string) {
	t.Helper()
	if held, ok := h.heldMergeOfPR(); ok {
		t.Errorf("the held merge stayed: %+v", held)
	}
	if refs := mg.heldRefs(); len(refs) != 0 {
		t.Errorf("held refs = %v, want none", refs)
	}
	if _, err := os.Stat(h.heldDiffPath()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("held-merge.diff stayed: %v", err)
	}
	if ev, ok := h.fixEvent("fix.held_dropped"); !ok || !strings.Contains(ev.Message, why) {
		t.Errorf("fix.held_dropped = %q, %v; want it to say %q", ev.Message, ok, why)
	}
}

// The bug: the operator's merge of the parent conflicted in five files, two
// of them workflow files both the base and the PR changed; claude-fix
// resolved them and magnum push handed the merge to the daemon, which
// refused it because it keeps the PR's own workflow changes and dropped it,
// so the operator was told to merge the base by hand. Now the daemon holds
// it for the operator's y: a ref under refs/magnum/held/ in the slot's
// clone keeps it, the registry records it, held-merge.diff (0600, in the
// PR's report directory) shows how the fixer resolved the workflow files'
// conflicts and then what the push changes in them, nothing is pushed and
// no token is minted, and the PR needs you with "merge ready", the files,
// the bot and that P asks.
func TestAHandedMergeThatKeepsThePRsOwnWorkflowChangesIsHeldForYourY(t *testing.T) {
	h, mg := holdMerged(t)
	wantNothingPushed(t, h, mg)
	if refs := mg.heldRefs(); refs[h.heldRefKey()] != fixMergeHead || len(refs) != 1 {
		t.Errorf("held refs = %v, want %s at %s", refs, h.heldRefKey(), fixMergeHead)
	}
	held, ok := h.heldMergeOfPR()
	switch {
	case !ok:
		t.Fatal("no held merge")
	case held.Merge != fixMergeHead || held.Head != fixHead || held.Base != "master" || held.Tip != fixBaseTip || held.Conflicts != 5:
		t.Errorf("held = %+v, want %s merging master at %s into %s, 5 conflicts", held, fixMergeHead, fixBaseTip, fixHead)
	case !slices.Equal(held.Workflows, []string{".github/workflows/ci.yml", ".github/workflows/pronto.yml"}) || held.Findings:
		t.Errorf("held = %+v, want ci.yml and pronto.yml and no findings", held)
	case held.Repo != "talkable/talkable" || held.Number != 2 || held.Role != "claude-fix" || held.Login != fixerAppLogin ||
		held.Clone != h.slot("review1").MainClone || held.Diff != h.heldDiffPath() || held.At.IsZero():
		t.Errorf("held = %+v", held)
	}
	b, err := os.ReadFile(h.heldDiffPath())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != heldRemerge+heldPlain {
		t.Errorf("held-merge.diff = %q, want the remerge diff, then the push's workflow diff", got)
	}
	if fi, err := os.Stat(h.heldDiffPath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("held-merge.diff's mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	if got := deref(h.wantState(2, store.PRNeedsAttention).LastError); got != heldAttention {
		t.Errorf("attention = %q, want %q", got, heldAttention)
	}
	if ev, ok := h.fixEvent("fix.merge_held"); !ok || !strings.Contains(ev.Message, "keeps the PR's own workflow changes (.github/workflows/ci.yml)") {
		t.Errorf("fix.merge_held = %q, %v", ev.Message, ok)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the merge's loop row stayed")
	}
	if cur := h.slot("review1"); deref(cur.CheckedOutSHA) != fixHead {
		t.Errorf("slot review1 holds %s, want the PR's head back", deref(cur.CheckedOutSHA))
	}
}

// A fix run's fixer merges the base before it fixes the findings, and
// magnum push hands that merge over alone: it is held the same way, and
// its attention says the findings wait for the next fix after the push.
func TestAFixRunsHeldMergeSaysTheFindingsWaitForTheNextFix(t *testing.T) {
	h, mg := mergeHarness(t, withFixerApp)
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	mg.behind, mg.lostPush = true, true
	heldConflicts(mg)
	fixer(h, mg.fixGit, fixMergeHead, func(run store.Run) string {
		return fixReport(run.ID, "d3d3d3d Merge c0c0c0c: not run: environment", "0 threads (0 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none")
	})
	id := h.fix(FixPayload{})
	h.handle()
	h.wantDone(id)
	wantNothingPushed(t, h, mg)
	if held, ok := h.heldMergeOfPR(); !ok || !held.Findings || held.Merge != fixMergeHead {
		t.Errorf("held = %+v, %v; want the merge, with findings waiting", held, ok)
	}
	if got, want := deref(h.wantState(2, store.PRNeedsAttention).LastError), heldAttention+" The findings wait for the next fix after the push."; got != want {
		t.Errorf("attention = %q, want %q", got, want)
	}
}

// The owner's note: a merge whose workflow files merged without a conflict
// (git's own merge of them is what the fixer committed) has no remerge
// diff there; held-merge.diff says so in one line, then shows the push's
// plain workflow diff.
func TestAHeldMergeWithNoConflictInAWorkflowFileWritesOneLineThenThePlainDiff(t *testing.T) {
	h, mg := handedMerge(t)
	heldConflicts(mg)
	mg.mu.Lock()
	mg.held.remerge = ""
	mg.mu.Unlock()
	id := h.fix(FixPayload{Mode: FixModeMerge})
	h.handle()
	h.wantDone(id)
	b, err := os.ReadFile(h.heldDiffPath())
	if err != nil {
		t.Fatal(err)
	}
	const line = "No conflict in a workflow file was resolved: the workflow changes come unchanged from master and the PR.\n"
	if got := string(b); got != line+heldPlain {
		t.Errorf("held-merge.diff = %q, want %q", got, line+heldPlain)
	}
}

// heldPlanWords are the plan of the push of PR #2's held merge.
const heldPlanWords = "dry run: would merge the parent of talkable/talkable#2: magnum pushes claude-fix's merge of master at c0c0c0c into feature as " +
	fixerAppLogin + " with the Workflows token; it changes .github/workflows/ci.yml and pronto.yml (held-merge.diff)"

// P on a held merge: the dry run plans its push, naming the bot, the token
// and the workflow files, and binds it to the merge's SHA besides the
// head and the tip; the y bound to that plan pushes the merge with the
// fixer App's own Workflows token (released after) and a lease on the
// head, from the clone that holds it, after a fetch of the base. Then the
// ref, the record and held-merge.diff go, the PR's head is the merge, its
// CI starts after the grace, and fix.parent_pushed and the merge's
// fix.ended say so.
func TestPOnAHeldMergePushesItWithTheWorkflowsTokenAfterTheBoundY(t *testing.T) {
	h, mg := holdMerged(t)
	fetchesBefore, _ := mg.calls()
	plan := h.planMerge()
	a := plan.Asked
	if plan.Text != heldPlanWords {
		t.Errorf("the plan = %q, want %q", plan.Text, heldPlanWords)
	}
	if a.Merge != fixMergeHead || a.Head != fixHead || a.Base != "master" || a.Tip != fixBaseTip || a.Conflicts != 5 || !a.At.Equal(h.clock.Now()) {
		t.Errorf("the plan asks about %+v, want the held merge %s", a, fixMergeHead)
	}
	wantNothingPushed(t, h, mg)
	h.advance(9 * time.Minute)
	r := h.mergeBound(a)
	const want = "pushed claude-fix's merge of master at c0c0c0c into feature as " + fixerAppLogin + " with the Workflows token: the head is d3d3d3d"
	if r.State != store.RequestDone || deref(r.Result) != want {
		t.Fatalf("%s %q, want done %q", r.State, deref(r.Result), want)
	}
	_, pushes := mg.made()
	if push := fmt.Sprintf("talkable/talkable:%s:feature:%s:%s", fixMergeHead, fixHead, daemonPushAs); !slices.Equal(pushes, []string{push}) {
		t.Errorf("pushes = %q, want %q", pushes, push)
	}
	if mints, released := h.ids[fixerAppName].daemonPushes(); !slices.Equal(mints, []bool{true}) || released != 1 {
		t.Errorf("daemon push tokens %v, released %d; want one with Workflows, released", mints, released)
	}
	if fetches, _ := mg.calls(); len(fetches) != len(fetchesBefore)+1 || fetches[len(fetches)-1] != h.slot("review1").MainClone+":master" {
		t.Errorf("fetches = %v, want one of master into the clone after %v", fetches, fetchesBefore)
	}
	if held, ok := h.heldMergeOfPR(); ok {
		t.Errorf("the held merge stayed: %+v", held)
	}
	if refs := mg.heldRefs(); len(refs) != 0 {
		t.Errorf("held refs = %v, want none", refs)
	}
	if _, err := os.Stat(h.heldDiffPath()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("held-merge.diff stayed: %v", err)
	}
	if cur := h.pr(2); cur.HeadSHA != fixMergeHead {
		t.Errorf("the PR's head = %s, want the merge", cur.HeadSHA)
	}
	if _, ok, _ := h.st.GetKV(h.ctx, kvPRPushed(h.pr(2).ID)); !ok {
		t.Error("magnum's push is not recorded for its CI (ci_dispatch)")
	}
	ev, ok := h.fixEvent("fix.parent_pushed")
	if !ok || !strings.Contains(ev.Message, "magnum pushed claude-fix's merge of master at c0c0c0c into talkable/talkable#2 as "+fixerAppLogin+
		" with Workflows after your y") || !strings.Contains(string(ev.Data), `"held":true`) {
		t.Errorf("fix.parent_pushed = %q %s, %v", ev.Message, ev.Data, ok)
	}
	if ev, ok := h.fixEvent("fix.ended"); !ok || !strings.HasPrefix(ev.Message, "the merge of the parent ends: magnum pushed") {
		t.Errorf("fix.ended = %q, %v", ev.Message, ok)
	}
	if _, ok := h.fixEvent("fix.held_dropped"); ok {
		t.Error("a pushed merge was reported dropped")
	}
}

// The y of a held merge's push is bound: it pushes nothing when the PR's
// head moved on GitHub since (the merge is dropped), when magnum holds
// another merge than the question named, when the base's tip moved so the
// held tip is not on the base as GitHub has it (dropped), or when the
// question is 10 minutes old, as for any merge of the parent.
func TestAHeldMergesPushIsRefusedWhenWhatItsYAskedAboutMoved(t *testing.T) {
	storetest.Parallel(t)
	for _, tc := range []struct {
		name    string
		move    func(h *harness, asked *MergeAsked)
		want    string
		dropped bool
	}{
		{"the head", func(h *harness, _ *MergeAsked) { h.gh.setHead("talkable/talkable", 2, fixPushed) },
			"talkable/talkable#2: its head moved on GitHub from a1a1a1a to b2b2b2b since magnum held the merge; magnum dropped the held merge: nothing was pushed", true},
		{"the held merge", func(_ *harness, asked *MergeAsked) { asked.Merge = "f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0" },
			"talkable/talkable#2: magnum holds the merge d3d3d3d now, and you were asked about f0f0f0f: nothing was merged; ask again", false},
		{"the base's tip", func(h *harness, _ *MergeAsked) {
			h.gh.mu.Lock()
			h.gh.baseTip = fixMovedTip // a force-push of master: c0c0c0c is not on it
			h.gh.mu.Unlock()
		}, "talkable/talkable#2: master moved on GitHub from c0c0c0c to d1d1d1d since magnum held the merge; magnum dropped the held merge: nothing was pushed", true},
		{"the wait", func(h *harness, _ *MergeAsked) { h.advance(10 * time.Minute) },
			"talkable/talkable#2: asked 10 minutes ago, longer than a merge of the parent waits for its y (10 minutes): nothing was merged; ask again", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, mg := holdMerged(t)
			asked := h.planMerge().Asked
			tc.move(h, &asked)
			r := h.mergeBound(asked)
			if r.State != store.RequestFailed || deref(r.Result) != tc.want {
				t.Errorf("%s %q, want failed %q", r.State, deref(r.Result), tc.want)
			}
			wantNothingPushed(t, h, mg)
			if _, held := h.heldMergeOfPR(); held == tc.dropped {
				t.Errorf("held = %v, want %v", held, !tc.dropped)
			}
		})
	}
}

// A held merge needs your y on a terminal: --yes binds nothing and never
// pushes it, and neither does a merge bound to a plan that names no held
// merge (an agent's shell, a screen from before the held merge). Nothing
// is pushed, no fixer starts, and the merge stays held.
func TestYesAndAPlanThatNamesNoHeldMergeNeverPushIt(t *testing.T) {
	h, mg := holdMerged(t)
	runs := len(h.fixRuns(h.pr(2).ID))
	id := h.fix(FixPayload{Mode: FixModeMerge})
	h.handle()
	const yes = "talkable/talkable#2: magnum holds claude-fix's merge of master at c0c0c0c for your y on a terminal: `magnum merge-parent " +
		"talkable/talkable#2` asks it (P on the screens); --yes binds nothing and never pushes a held merge: nothing was pushed"
	if r := h.request(id); r.State != store.RequestFailed || deref(r.Result) != yes {
		t.Errorf("--yes: %s %q, want failed %q", r.State, deref(r.Result), yes)
	}
	asked := h.planMerge().Asked
	asked.Merge = ""
	const unnamed = "talkable/talkable#2: the question named no held merge, and magnum holds claude-fix's merge of master at c0c0c0c for your y: " +
		"nothing was pushed; ask again (`magnum merge-parent talkable/talkable#2`)"
	if r := h.mergeBound(asked); r.State != store.RequestFailed || deref(r.Result) != unnamed {
		t.Errorf("a plan without the merge: %s %q, want failed %q", r.State, deref(r.Result), unnamed)
	}
	wantNothingPushed(t, h, mg)
	if _, ok := h.heldMergeOfPR(); !ok {
		t.Error("the held merge went")
	}
	if got := len(h.fixRuns(h.pr(2).ID)); got != runs {
		t.Errorf("fix runs = %d, want %d: a fixer started", got, runs)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("a loop started")
	}
}

// A held merge that went stale goes, at the poll that sees it: the PR's
// head moved (someone pushed), its base moved (a retarget), its base's tip
// moved on GitHub (which only its Details name, so the poll reads them for
// a PR with a held merge), or it merged or closed. Its ref, its row and
// held-merge.diff go and fix.held_dropped says why; a PR that still needs
// you for it is told that magnum dropped it and that P merges the parent
// again, so a stale merge never comes back as "merge ready".
func TestAHeldMergeThatWentStaleIsDroppedAtThePoll(t *testing.T) {
	storetest.Parallel(t)
	other := prSpec{n: 1, head: "base1"}
	for _, tc := range []struct {
		name, why string
		move      func(h *harness)
		attention string // the PR's attention after the drop; "" for no check
	}{
		{name: "a push", why: "its head moved from a1a1a1a to b2b2b2b", move: func(h *harness) {
			h.open(other, prSpec{n: 2, head: fixPushed, author: "zhuravel"})
			h.tick()
		}},
		{name: "a retarget", why: "its base moved from master to release", move: func(h *harness) {
			h.open(other, prSpec{n: 2, head: fixHead, author: "zhuravel", base: "release"})
			h.tick()
		}},
		{name: "the base's tip", why: "master moved on GitHub from c0c0c0c to d1d1d1d", move: func(h *harness) {
			h.gh.mu.Lock()
			h.gh.baseTip = fixMovedTip
			h.gh.mu.Unlock()
			h.tick()
		}, attention: "fix: magnum dropped claude-fix's held merge of master at c0c0c0c: master moved on GitHub from c0c0c0c to d1d1d1d; " +
			"P (`magnum merge-parent talkable/talkable#2`) merges the parent again"},
		{name: "a merge", why: "the PR merged", move: func(h *harness) { h.closeOnGitHub(2, "MERGED", other) }},
		{name: "a close", why: "the PR closed", move: func(h *harness) { h.closeOnGitHub(2, "CLOSED", other) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, mg := holdMerged(t)
			h.tick() // a poll that sees nothing moved keeps it
			if _, ok := h.heldMergeOfPR(); !ok {
				t.Fatal("a poll dropped a held merge that did not move")
			}
			tc.move(h)
			wantDropped(t, h, mg, tc.why)
			if tc.attention == "" {
				return
			}
			if got := deref(h.wantState(2, store.PRNeedsAttention).LastError); got != tc.attention {
				t.Errorf("attention = %q, want %q", got, tc.attention)
			}
			if plan := h.planMerge(); plan.Asked.Merge != "" || strings.Contains(plan.Text, "magnum pushes") {
				t.Errorf("a dropped merge was planned again: %+v", plan)
			}
		})
	}
}

// Only the refusal for the PR's own workflow changes, of the tip GitHub
// names, holds a merge: every other refusal of a handed merge goes to the
// operator as before (merge it by hand), with no ref, no row and no
// held-merge.diff. An App without Workflows; a merge of a tip the head has
// though GitHub's base tip is another (its workflow files then differ
// from a tip that moved); a tip that is not on the base as GitHub has it.
func TestTheOtherRefusalsOfAHandedMergeStillNeedYouAsBefore(t *testing.T) {
	storetest.Parallel(t)
	const older = "0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b" // an older commit of master the head lacks
	for _, tc := range []struct {
		name string
		mods []func(*harness)
		set  func(h *harness, mg *mergeGit)
		want string
	}{
		{name: "an App without Workflows", mods: []func(*harness){withoutWorkflows}, set: func(_ *harness, mg *mergeGit) { mg.own = nil },
			want: "needs Workflows on the App"},
		{name: "workflow files that differ from a tip that moved", set: func(_ *harness, mg *mergeGit) {
			mg.handed = &gitx.CommitIdentity{SHA: fixMergeHead, Parents: []string{fixHead, older}, AuthorEmail: fixerAppEmail, CommitterEmail: fixerAppEmail}
			mg.ancestors = map[string]bool{older + ".." + fixBaseTip: true}
			mg.differ = map[string][]string{fixBaseTip: {".github/workflows/ci.yml"}}
		}, want: "has workflow files that are not as the tip c0c0c0c of master on GitHub has them (.github/workflows/ci.yml)"},
		{name: "a tip that is not on the base as GitHub has it", set: func(h *harness, _ *mergeGit) {
			h.gh.baseTip = fixMovedTip
		}, want: "c0c0c0c is not on master as GitHub has it (its tip is d1d1d1d)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, mg := handedMerge(t, tc.mods...)
			heldConflicts(mg)
			h.gh.mu.Lock()
			mg.mu.Lock()
			tc.set(h, mg)
			mg.mu.Unlock()
			h.gh.mu.Unlock()
			id := h.fix(FixPayload{Mode: FixModeMerge})
			h.handle()
			h.wantDone(id)
			if _, pushes := mg.made(); len(pushes) != 0 { // the App without Workflows tried to mint its token
				t.Errorf("pushes = %v, want none", pushes)
			}
			if held, ok := h.heldMergeOfPR(); ok {
				t.Errorf("held: %+v", held)
			}
			if refs := mg.heldRefs(); len(refs) != 0 {
				t.Errorf("held refs = %v", refs)
			}
			if _, err := os.Stat(h.heldDiffPath()); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("held-merge.diff: %v", err)
			}
			got := deref(h.wantState(2, store.PRNeedsAttention).LastError)
			if !strings.HasPrefix(got, "fix: magnum did not push claude-fix's merge of the parent of talkable/talkable#2: ") || !strings.Contains(got, tc.want) ||
				!strings.Contains(got, "merge master into feature by hand") {
				t.Errorf("attention = %q, want the merge by hand for %q", got, tc.want)
			}
		})
	}
}

// A merge into a PR that is not the operator's own (its author, as the
// registry has it when the settle holds the merge) is never held: the
// refusal goes to the operator as before.
func TestAMergeIntoAPRThatIsNotYoursIsNeverHeld(t *testing.T) {
	h, mg := handedMerge(t)
	heldConflicts(mg)
	fixer(h, mg.fixGit, fixMergeHead, func(run store.Run) string {
		if _, err := h.st.DB().ExecContext(h.ctx, `UPDATE prs SET author_login = 'alice' WHERE id = ?`, run.PRID); err != nil {
			t.Errorf("the PR's author: %v", err) // the engine's goroutine: no t.Fatal
		}
		return fixReport(run.ID, "d3d3d3d Merge c0c0c0c: not run: environment", "0 threads (0 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none")
	})
	id := h.fix(FixPayload{Mode: FixModeMerge})
	h.handle()
	h.wantDone(id)
	wantNothingPushed(t, h, mg)
	if held, ok := h.heldMergeOfPR(); ok {
		t.Errorf("held: %+v", held)
	}
	if refs := mg.heldRefs(); len(refs) != 0 {
		t.Errorf("held refs = %v", refs)
	}
	if _, err := os.Stat(h.heldDiffPath()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("held-merge.diff: %v", err)
	}
	const why = "keeps the PR's own workflow changes (.github/workflows/ci.yml), which " + fixerAppLogin + " never pushes"
	if got := deref(h.wantState(2, store.PRNeedsAttention).LastError); !strings.Contains(got, why) || !strings.Contains(got, "by hand") {
		t.Errorf("attention = %q, want the refusal %q and the merge by hand", got, why)
	}
}
