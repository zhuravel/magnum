package engine

import (
	"context"
	"encoding/json"
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

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// heldGitFake is mergeGit's held merge (heldGit): the refs UpdateRef wrote
// ("<dir> <ref>" to the commit) and UpdateRefDelete deleted, a commit's
// parents (parents, by commit; else the handed merge's, else the head's
// and the base's tip), RemergeDiff's and PathsDiff's answers, refErr, which
// fails UpdateRef, and onRef, run inside UpdateRef with no lock held. As
// git does, UpdateRef refuses a ref with a part that starts with a dot.
type heldGitFake struct {
	refs    map[string]string
	deleted []string
	parents map[string][]string
	remerge string
	diff    string
	refErr  error
	onRef   func()
	// pushGate parks PushLeaseGitHubAs, which says so on pushStarted, until
	// the gate closes or the push's context ends; pushPanics makes it panic.
	pushGate    chan struct{}
	pushStarted chan struct{}
	pushPanics  bool
	// diffErr fails RemergeDiff (an output past execx's cap, say);
	// refReadErr fails the read of a held ref (a cancelled context, a clone
	// briefly gone), as no missing ref does.
	diffErr    error
	refReadErr error
}

// RefsUnder lists the refs under prefix (a part of refs/magnum/) that
// UpdateRef wrote in dir.
func (g *mergeGit) RefsUnder(_ context.Context, dir, prefix string) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for key := range g.held.refs {
		if d, ref, _ := strings.Cut(key, " "); d == dir && strings.HasPrefix(ref, prefix) {
			out = append(out, ref)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (g *mergeGit) UpdateRef(_ context.Context, dir, ref, sha string) error {
	g.mu.Lock()
	hook, err := g.held.onRef, g.held.refErr
	g.mu.Unlock()
	if hook != nil {
		hook()
	}
	for part := range strings.SplitSeq(ref, "/") {
		if strings.HasPrefix(part, ".") {
			return fmt.Errorf("fatal: update_ref failed for ref '%s': refusing to update ref with bad name", ref)
		}
	}
	if err != nil {
		return err
	}
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
	return g.held.remerge, g.held.diffErr
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
	// heldHeader is held-merge.diff's first line: the merge it shows.
	heldHeader = "Held merge " + fixMergeHead + ": claude-fix's merge of master at c0c0c0c into the PR's head a1a1a1a.\n"
	// heldAttention is PR #2's attention while claude-fix's merge is held.
	heldAttention = "fix: merge ready: claude-fix merged master at c0c0c0c and resolved 5 conflicts. It changes .github/workflows/ci.yml and " +
		"pronto.yml. " + fixerAppLogin + " pushes it only after your y: P asks (e shows held-merge.diff)."
	// heldPushing is the answer to the y that pushes PR #2's held merge.
	heldPushing = "pushing claude-fix's merge d3d3d3d of master at c0c0c0c into feature as " + fixerAppLogin +
		" with the Workflows token; `magnum abort talkable/talkable#2` stops it"
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
	return h.slot("review1").MainClone + " " + HeldMergeRef(h.pr(2).ID)
}

// heldRow is PR #2's held merge's row as stored ("" for none).
func (h *harness) heldRow() string {
	h.t.Helper()
	raw, _, err := h.st.GetKV(h.ctx, KVPRHeldMerge(h.pr(2).ID))
	if err != nil {
		h.t.Fatal(err)
	}
	return raw
}

// heldMergeOfPR is PR #2's held merge (ok false: none).
func (h *harness) heldMergeOfPR() (HeldMerge, bool) {
	h.t.Helper()
	raw := h.heldRow()
	if raw == "" {
		return HeldMerge{}, false
	}
	held, ok := ParseHeldMerge(raw)
	if !ok {
		h.t.Fatalf("the held merge's row does not read: %q", raw)
	}
	return held, true
}

// setHeld writes PR #2's held merge's row as edit changes it, as another
// build or another hold would have written it.
func (h *harness) setHeld(edit func(*HeldMerge)) string {
	h.t.Helper()
	held, ok := h.heldMergeOfPR()
	if !ok {
		h.t.Fatal("no held merge to change")
	}
	edit(&held)
	b, err := json.Marshal(held)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.SetKV(h.ctx, KVPRHeldMerge(h.pr(2).ID), string(b)); err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

// wantNoPush fails when magnum pushed anything.
func wantNoPush(t *testing.T, mg *mergeGit) {
	t.Helper()
	if _, pushes := mg.made(); len(pushes) != 0 {
		t.Errorf("pushes = %v, want none", pushes)
	}
}

// wantNothingPushed fails when magnum pushed anything or minted a push
// token.
func wantNothingPushed(t *testing.T, h *harness, mg *mergeGit) {
	t.Helper()
	wantNoPush(t, mg)
	if mints, _ := h.ids[fixerAppName].daemonPushes(); len(mints) != 0 {
		t.Errorf("daemon push tokens minted: %v", mints)
	}
}

// wantProbed fails unless the hold minted one Workflows token to see that
// it can, and released it at once, and nothing was pushed.
func wantProbed(t *testing.T, h *harness, mg *mergeGit) {
	t.Helper()
	wantNoPush(t, mg)
	if mints, released := h.ids[fixerAppName].daemonPushes(); !slices.Equal(mints, []bool{true}) || released != 1 {
		t.Errorf("daemon push tokens %v, released %d; want the hold's one, released", mints, released)
	}
}

// wantNoHeld fails unless PR #2 has no held merge: no row, no ref, no
// held-merge.diff.
func wantNoHeld(t *testing.T, h *harness, mg *mergeGit) {
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
}

// wantDropped fails unless PR #2's held merge is gone (wantNoHeld) with a
// fix.held_dropped event that says why.
func wantDropped(t *testing.T, h *harness, mg *mergeGit, why string) {
	t.Helper()
	wantNoHeld(t, h, mg)
	if ev, ok := h.fixEvent("fix.held_dropped"); !ok || !strings.Contains(ev.Message, why) {
		t.Errorf("fix.held_dropped = %q, %v; want it to say %q", ev.Message, ok, why)
	}
}

// The bug: the operator's merge of the parent conflicted in five files, two
// of them workflow files both the base and the PR changed; claude-fix
// resolved them and magnum push handed the merge to the daemon, which
// refused it because it keeps the PR's own workflow changes and dropped it,
// so the operator was told to merge the base by hand. Now the daemon holds
// it for the operator's y: a ref under refs/magnum/held/<PR id> in the
// slot's clone keeps it, the registry records it, held-merge.diff (0600,
// in the PR's report directory) names the merge and shows how the fixer
// resolved the workflow files' conflicts, then what the push changes in
// them, nothing is pushed (the hold mints one Workflows token to see that
// the App can, and releases it), and the PR needs you with "merge ready",
// the files, the bot and that P asks.
func TestAHandedMergeThatKeepsThePRsOwnWorkflowChangesIsHeldForYourY(t *testing.T) {
	h, mg := holdMerged(t)
	wantProbed(t, h, mg)
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
		held.Identity != fixerAppName || held.Clone != h.slot("review1").MainClone || held.Diff != h.heldDiffPath() ||
		held.Attention != heldAttention || held.At.IsZero():
		t.Errorf("held = %+v", held)
	}
	b, err := os.ReadFile(h.heldDiffPath())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != heldHeader+heldRemerge+heldPlain {
		t.Errorf("held-merge.diff = %q, want the merge's line, the remerge diff, then the push's workflow diff", got)
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

// The review's bug: an App without Workflows held a merge it could never
// push, so the attention said "merge ready" and every y failed with "needs
// Workflows on the App", while the advice to merge by hand or grant
// Workflows was lost. The hold mints the Workflows token first; when GitHub
// refuses it, nothing is held, and the PR needs you as before: merge by
// hand, or grant Workflows on the App and ask again.
func TestAnAppWithoutWorkflowsHoldsNoMergeItCannotPush(t *testing.T) {
	h, mg := holdMerged(t, withoutWorkflows)
	wantNoPush(t, mg)
	wantNoHeld(t, h, mg)
	got := deref(h.wantState(2, store.PRNeedsAttention).LastError)
	for _, want := range []string{"fix: magnum did not push claude-fix's merge of the parent of talkable/talkable#2: ", "needs Workflows on the App",
		"merge master into feature by hand", "or grant Workflows on the App and run `magnum merge-parent talkable/talkable#2` again"} {
		if !strings.Contains(got, want) {
			t.Errorf("attention = %q, want it to say %q", got, want)
		}
	}
	if attention.MergeReady(got) {
		t.Errorf("attention = %q: merge ready for a merge the App cannot push", got)
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
	wantProbed(t, h, mg)
	if held, ok := h.heldMergeOfPR(); !ok || !held.Findings || held.Merge != fixMergeHead {
		t.Errorf("held = %+v, %v; want the merge, with findings waiting", held, ok)
	}
	if got, want := deref(h.wantState(2, store.PRNeedsAttention).LastError), heldAttention+" The findings wait for the next fix after the push."; got != want {
		t.Errorf("attention = %q, want %q", got, want)
	}
}

// The review's bug: Findings was true for any job but a merge of the
// parent, a CI fix's too, whose fixer merges the base as well. The
// findings wait only after a fix of findings that did not start for the
// merge alone.
func TestOnlyAFixOfFindingsSaysTheFindingsWait(t *testing.T) {
	for _, c := range []struct {
		job  fixJob
		want bool
	}{
		{fixJob{}, true},
		{fixJob{forMerge: true}, false},
		{fixJob{mode: FixModeMerge}, false},
		{fixJob{mode: FixModeCI}, false},
	} {
		if got := heldFindings(&c.job); got != c.want {
			t.Errorf("mode %q, for the merge %v: findings %v, want %v", c.job.mode, c.job.forMerge, got, c.want)
		}
	}
}

// The owner's note: a merge whose workflow files merged without a conflict
// (git's own merge of them is what the fixer committed) has no remerge
// diff there; held-merge.diff says so in one line after the merge's, then
// shows the push's plain workflow diff.
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
	if got := string(b); got != heldHeader+line+heldPlain {
		t.Errorf("held-merge.diff = %q, want %q", got, heldHeader+line+heldPlain)
	}
}

// Workflow content an agent wrote reaches the pager, which runs with
// LESS=R: held-merge.diff escapes every control character but tab and
// newline (a carriage return, an escape sequence, a DEL) and the bidi and
// invisible format characters a "Trojan Source" line hides behind (U+202E
// among them), so none hides or reorders a line the operator reads before
// the y.
func TestHeldMergeDiffEscapesTheControlsThatCouldHideALine(t *testing.T) {
	h, mg := handedMerge(t)
	heldConflicts(mg)
	mg.mu.Lock()
	mg.held.remerge = "+run: deploy\x1b[2K\r+run: echo ok\x7f\t# tab\n+if: ${{ github.actor == 'alice' \u202e && true }}\u2066\u200b\ufeff\n" +
		"+run: curl 10.0.\u061c0.1\u061c\u2028\n"
	mg.mu.Unlock()
	id := h.fix(FixPayload{Mode: FixModeMerge})
	h.handle()
	h.wantDone(id)
	b, err := os.ReadFile(h.heldDiffPath())
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if want := heldHeader + "+run: deploy\\x1b[2K\\x0d+run: echo ok\\x7f\t# tab\n+if: ${{ github.actor == 'alice' \\u202e && true }}\\u2066\\u200b\\ufeff\n" +
		"+run: curl 10.0.\\u061c0.1\\u061c\\u2028\n" + heldPlain; got != want {
		t.Errorf("held-merge.diff = %q, want %q", got, want)
	}
}

// heldPlanWords are the plan of the push of PR #2's held merge: the merge
// by its SHA, so a merge held again of the same tip asks another question.
const heldPlanWords = "dry run: would merge the parent of talkable/talkable#2: magnum pushes claude-fix's merge d3d3d3d of master at c0c0c0c into " +
	"feature as " + fixerAppLogin + " with the Workflows token; it changes .github/workflows/ci.yml and pronto.yml (held-merge.diff)"

// P on a held merge: the dry run plans its push, naming the merge, the bot,
// the token and the workflow files, and binds it to the merge's SHA besides
// the head and the tip; the y bound to that plan starts the push off the
// daemon's loop, under the PR's reservation, and answers so. The push goes
// with the fixer App's own Workflows token (released after) and a lease on
// the head, from the clone that holds the merge, after a fetch of the base.
// Then the ref, the record and held-merge.diff go, the PR's head is the
// merge, its CI starts after the grace, and fix.parent_pushed and the
// merge's fix.ended say so.
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
	wantProbed(t, h, mg)
	h.advance(9 * time.Minute)
	r := h.mergeBound(a)
	if r.State != store.RequestDone || deref(r.Result) != heldPushing {
		t.Fatalf("%s %q, want done %q", r.State, deref(r.Result), heldPushing)
	}
	_, pushes := mg.made()
	if push := fmt.Sprintf("talkable/talkable:%s:feature:%s:%s", fixMergeHead, fixHead, daemonPushAs); !slices.Equal(pushes, []string{push}) {
		t.Errorf("pushes = %q, want %q", pushes, push)
	}
	if dirs := mg.pushedFrom(); !slices.Equal(dirs, []string{h.slot("review1").MainClone}) {
		t.Errorf("pushed from %q, want the clone that holds the merge", dirs)
	}
	if mints, released := h.ids[fixerAppName].daemonPushes(); !slices.Equal(mints, []bool{true, true}) || released != 2 {
		t.Errorf("daemon push tokens %v, released %d; want the hold's and the push's, released", mints, released)
	}
	if fetches, _ := mg.calls(); len(fetches) != len(fetchesBefore)+1 || fetches[len(fetches)-1] != h.slot("review1").MainClone+":master" {
		t.Errorf("fetches = %v, want one of master into the clone after %v", fetches, fetchesBefore)
	}
	wantNoHeld(t, h, mg)
	if cur := h.pr(2); cur.HeadSHA != fixMergeHead {
		t.Errorf("the PR's head = %s, want the merge", cur.HeadSHA)
	}
	if _, ok, _ := h.st.GetKV(h.ctx, kvPRPushed(h.pr(2).ID)); !ok {
		t.Error("magnum's push is not recorded for its CI (ci_dispatch)")
	}
	ev, ok := h.fixEvent("fix.parent_pushed")
	if !ok || !strings.Contains(ev.Message, "magnum pushed claude-fix's merge d3d3d3d of master at c0c0c0c into talkable/talkable#2 as "+fixerAppLogin+
		" with Workflows after your y") || !strings.Contains(string(ev.Data), `"held":true`) {
		t.Errorf("fix.parent_pushed = %q %s, %v", ev.Message, ev.Data, ok)
	}
	if ev, ok := h.fixEvent("fix.ended"); !ok || !strings.HasPrefix(ev.Message, "the merge of the parent ends: magnum pushed") {
		t.Errorf("fix.ended = %q, %v", ev.Message, ok)
	}
	if _, ok := h.fixEvent("fix.held_dropped"); ok {
		t.Error("a pushed merge was reported dropped")
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the push's loop row stayed")
	}
}

// The y of a held merge's push is bound. Before the push starts it is
// refused, and the merge stays held, when magnum holds another merge than
// the question named or the question is 10 minutes old, as for any merge
// of the parent. The push, off the loop, pushes nothing when the PR's head
// moved on GitHub, or the base's tip moved so the held tip is not on the
// base as GitHub has it: the merge is dropped, and fix.failed says why.
func TestAHeldMergesPushIsRefusedWhenWhatItsYAskedAboutMoved(t *testing.T) {
	storetest.Parallel(t)
	for _, tc := range []struct {
		name    string
		move    func(h *harness, asked *MergeAsked)
		want    string // the request's refusal, or, after the push started, fix.failed's words
		started bool
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
			if !tc.started {
				if r.State != store.RequestFailed || deref(r.Result) != tc.want {
					t.Errorf("%s %q, want failed %q", r.State, deref(r.Result), tc.want)
				}
			} else if r.State != store.RequestDone || deref(r.Result) != heldPushing {
				t.Errorf("%s %q, want the push started", r.State, deref(r.Result))
			} else if ev, ok := h.fixEvent("fix.failed"); !ok || ev.Message != tc.want {
				t.Errorf("fix.failed = %q, %v; want %q", ev.Message, ok, tc.want)
			}
			wantNoPush(t, mg)
			if _, held := h.heldMergeOfPR(); held == tc.started {
				t.Errorf("held = %v, want %v", held, !tc.started)
			}
			if _, ok := h.fixStateOf(2); ok {
				t.Error("the push's loop row stayed")
			}
		})
	}
}

// The review's bug: the y could push as a bot the question did not name. A
// held merge whose fix identity is not the fix role's now (the config
// changed since the hold) is dropped when P plans: the plan is then a new
// merge's, and a y bound to the held merge pushes nothing.
func TestAHeldMergeIsNeverPushedAsAnotherBot(t *testing.T) {
	h, mg := holdMerged(t)
	asked := h.planMerge().Asked
	h.setHeld(func(held *HeldMerge) { held.Identity = "other-app" })
	if r := h.mergeBound(asked); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "magnum no longer holds") {
		t.Errorf("%s %q, want the y refused", r.State, deref(r.Result))
	}
	wantNoPush(t, mg)
	wantDropped(t, h, mg, "the fix identity is fixer-app now, not other-app")
}

// The review's bug: the paths of a held merge came from what its row named.
// A row whose repository or number is not its PR's is dropped when P plans,
// and the plan is a new merge's.
func TestAHeldMergeWhoseRowNamesAnotherPRIsDropped(t *testing.T) {
	h, mg := holdMerged(t)
	h.setHeld(func(held *HeldMerge) { held.Number = 99 })
	if plan := h.planMerge(); plan.Asked.Merge != "" {
		t.Errorf("the plan = %+v, want a new merge's", plan)
	}
	wantNoPush(t, mg)
	wantDropped(t, h, mg, "its row names talkable/talkable#99")
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
	const yes = "talkable/talkable#2: magnum holds claude-fix's merge d3d3d3d of master at c0c0c0c for your y on a terminal: `magnum merge-parent " +
		"talkable/talkable#2` asks it (P on the screens); --yes binds nothing and never pushes a held merge: nothing was pushed"
	if r := h.request(id); r.State != store.RequestFailed || deref(r.Result) != yes {
		t.Errorf("--yes: %s %q, want failed %q", r.State, deref(r.Result), yes)
	}
	asked := h.planMerge().Asked
	asked.Merge = ""
	const unnamed = "talkable/talkable#2: the question named no held merge, and magnum holds claude-fix's merge d3d3d3d of master at c0c0c0c " +
		"for your y: nothing was pushed; ask again (`magnum merge-parent talkable/talkable#2`)"
	if r := h.mergeBound(asked); r.State != store.RequestFailed || deref(r.Result) != unnamed {
		t.Errorf("a plan without the merge: %s %q, want failed %q", r.State, deref(r.Result), unnamed)
	}
	wantProbed(t, h, mg)
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

// heldDroppedNote is PR #2's attention after the poll dropped its held merge
// because master's tip moved.
const heldDroppedNote = "fix: merge dropped: magnum dropped claude-fix's held merge d3d3d3d of master at c0c0c0c: master moved on GitHub " +
	"from c0c0c0c to d1d1d1d; P merges the parent again"

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
		}, attention: heldDroppedNote},
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
			got := h.wantState(2, store.PRNeedsAttention)
			if deref(got.LastError) != tc.attention {
				t.Errorf("attention = %q, want %q", deref(got.LastError), tc.attention)
			}
			if why := attention.Explain(attention.KindFix, deref(got.LastError), "talkable/talkable#2"); why.Fix != attention.MergeDroppedNext("talkable/talkable#2") {
				t.Errorf("the next step = %q, want P", why.Fix)
			}
			if plan := h.planMerge(); plan.Asked.Merge != "" || strings.Contains(plan.Text, "magnum pushes") {
				t.Errorf("a dropped merge was planned again: %+v", plan)
			}
		})
	}
}

// heldRepo is talkable/talkable's registry row.
func (h *harness) heldRepo() store.Repo {
	h.t.Helper()
	repo, err := h.st.RepoByFullName(h.ctx, "talkable/talkable")
	if err != nil {
		h.t.Fatal(err)
	}
	return repo
}

// The review's races: the poll reads the held merges before its GitHub
// reads, which can serve a y (requestsMidPoll), and a settle can hold a new
// merge meanwhile. Its drop deletes only the row it read (a compare), and
// touches the ref and the file only then: (a) a merge pushed after the
// poll's read is not reported dropped, and nothing of the PR's attention
// changes; (b) a merge held again after the poll's read stays whole, its
// ref and file too; (c) a hold writes its row only after the attention, so
// a poll during the hold drops nothing, and "merge ready" never stands
// without a row: the next poll drops the stale merge and its words.
func TestThePollDropsOnlyTheHeldMergeItRead(t *testing.T) {
	storetest.Parallel(t)
	t.Run("a push after the poll's read", func(t *testing.T) {
		h, mg := holdMerged(t)
		read := h.e.heldMergesIn(h.ctx, h.heldRepo())
		r := h.mergeBound(h.planMerge().Asked)
		if r.State != store.RequestDone {
			t.Fatalf("the push: %s %q", r.State, deref(r.Result))
		}
		before := deref(h.pr(2).LastError)
		h.e.dropStaleHeldMerges(h.ctx, h.heldRepo(), read, nil)
		if ev, ok := h.fixEvent("fix.held_dropped"); ok {
			t.Errorf("fix.held_dropped = %q after magnum's own push", ev.Message)
		}
		if got := deref(h.pr(2).LastError); got != before {
			t.Errorf("attention = %q, want %q kept", got, before)
		}
		wantNoHeld(t, h, mg)
	})
	t.Run("a merge held again after the poll's read", func(t *testing.T) {
		h, mg := holdMerged(t)
		read := h.e.heldMergesIn(h.ctx, h.heldRepo())
		again := h.setHeld(func(held *HeldMerge) { held.At = held.At.Add(time.Minute) }) // a new hold of the same merge
		h.execSQL(`UPDATE prs SET head_sha = ? WHERE id = ?`, fixPushed, h.pr(2).ID)
		h.e.dropStaleHeldMerges(h.ctx, h.heldRepo(), read, nil)
		if got := h.heldRow(); got != again {
			t.Errorf("the row = %q, want the new hold's %q", got, again)
		}
		if refs := mg.heldRefs(); refs[h.heldRefKey()] != fixMergeHead {
			t.Errorf("held refs = %v, want the new hold's", refs)
		}
		if _, err := os.Stat(h.heldDiffPath()); err != nil {
			t.Errorf("held-merge.diff: %v", err)
		}
		if ev, ok := h.fixEvent("fix.held_dropped"); ok {
			t.Errorf("fix.held_dropped = %q", ev.Message)
		}
	})
	t.Run("a drop during the hold", func(t *testing.T) {
		h, mg := handedMerge(t)
		heldConflicts(mg)
		var during map[int64]heldRow
		mg.mu.Lock()
		mg.held.onRef = func() { // the poll, while the settle holds the merge: GitHub's head moved
			h.gh.setHead("talkable/talkable", 2, fixPushed)
			during = h.e.heldMergesIn(context.WithoutCancel(h.ctx), h.heldRepo())
		}
		mg.mu.Unlock()
		// what the PR's attention said when the held merge's row was written
		h.execSQL(fmt.Sprintf(`CREATE TEMP TRIGGER held_seen AFTER INSERT ON kv WHEN NEW.key = '%s' BEGIN
  INSERT INTO kv (key, value, updated_at) SELECT 'test.attention_at_hold', COALESCE(last_error, ''), NEW.updated_at FROM prs WHERE id = %d;
END`, KVPRHeldMerge(h.pr(2).ID), h.pr(2).ID))
		id := h.fix(FixPayload{Mode: FixModeMerge})
		h.handle()
		h.wantDone(id)
		if len(during) != 0 {
			t.Errorf("a poll during the hold read %v, want no row before the attention", during)
		}
		if seen, _, _ := h.st.GetKV(h.ctx, "test.attention_at_hold"); !attention.MergeReady(seen) {
			t.Errorf("the attention when the row was written = %q, want merge ready: the row comes after it", seen)
		}
		if _, ok := h.heldMergeOfPR(); !ok || !attention.MergeReady(deref(h.pr(2).LastError)) {
			t.Fatalf("after the hold: held %v, attention %q", ok, deref(h.pr(2).LastError))
		}
		h.tick() // the next poll sees the head GitHub has
		wantDropped(t, h, mg, "its head moved")
		if got := deref(h.pr(2).LastError); attention.MergeReady(got) {
			t.Errorf("attention = %q: merge ready with no held merge", got)
		}
	})
}

// heldCall is the call cycle 1's fixer leaves the author (leavesCall).
const heldCall = "app/models/offer.rb:42"

// heldWithCalls runs a fix loop on PR #2 whose cycle 1 leaves the author
// the call on 2101 at call and pushes, and whose cycle 2's fixer merges the
// base: magnum holds that merge, and the attention says merge ready, then
// asks the carried call.
func heldWithCalls(t *testing.T, call string) (*harness, *mergeGit) {
	t.Helper()
	h, mg := mergeHarness(t, withFixerApp)
	fixablePR(h, "zhuravel", map[string]int{"P1": 1, "P2": 1})
	cycleFixer(h, mg.fixGit, leavesCall(fixPushed, 42), func(id string) (string, string) {
		return fixMergeHead, fixReport(id, "d3d3d3d Merge c0c0c0c: not run: environment", "0 threads (0 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none")
	})
	h.fix(FixPayload{})
	h.handle()
	reviewWith(h, byHead(map[string]map[string]int{fixPushed: {"P2": 1}}))
	mg.behind, mg.lostPush = true, true // cycle 2's head lacks master's tip, and its merge conflicts
	heldConflicts(mg)
	mg.mu.Lock()
	mg.held.parents = map[string][]string{fixMergeHead: {fixPushed, fixBaseTip}}
	mg.mu.Unlock()
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"}) // the re-review's new finding: cycle 2
	held, ok := h.heldMergeOfPR()
	if !ok || held.Head != fixPushed || !held.Findings {
		t.Fatalf("held = %+v, %v; want cycle 2's merge into b2b2b2b, findings waiting", held, ok)
	}
	got := deref(h.wantState(2, store.PRNeedsAttention).LastError)
	if !strings.HasPrefix(got, strings.TrimSuffix(held.Attention, ".")+"; ") || !strings.Contains(got, call) {
		t.Fatalf("attention = %q, want merge ready, then the carried call on %s", got, call)
	}
	return h, mg
}

// The review's bug: a fix loop's calls were lost when its merge was held,
// though a loop's end asks them wherever it ends. Cycle 1's fixer left the
// author a call; cycle 2's fixer merged the base, and its merge is held: the
// attention says merge ready and asks the carried call. A drop keeps the
// call and replaces the merge's words alone; a push says the findings wait
// for the next fix (`magnum fix`, f on the screens) and keeps the call.
func TestAHeldMergeKeepsTheCallsTheLoopCarried(t *testing.T) {
	storetest.Parallel(t)
	const call = heldCall
	t.Run("dropped", func(t *testing.T) {
		h, mg := heldWithCalls(t, call)
		h.gh.mu.Lock()
		h.gh.baseTip = fixMovedTip
		h.gh.mu.Unlock()
		h.tick()
		wantDropped(t, h, mg, "master moved on GitHub")
		got := deref(h.wantState(2, store.PRNeedsAttention).LastError)
		if !attention.MergeDropped(got) || !strings.Contains(got, call) {
			t.Errorf("attention = %q, want the drop's words and the carried call on %s", got, call)
		}
	})
	t.Run("pushed", func(t *testing.T) {
		h, mg := heldWithCalls(t, call)
		if r := h.mergeBound(h.planMerge().Asked); r.State != store.RequestDone {
			t.Fatalf("the push: %s %q", r.State, deref(r.Result))
		}
		if _, pushes := mg.made(); len(pushes) != 1 {
			t.Fatalf("pushes = %v, want the held merge's", pushes)
		}
		// the merge is no trivial delta here: the push queued its re-review, which f needs on the head
		h.wantState(2, store.PRRereviewPending)
		ev, ok := h.fixEvent("fix.needs_you")
		for _, want := range []string{"magnum pushed claude-fix's held merge d3d3d3d after your y", "the findings wait: the re-review of d3d3d3d runs first",
			"`magnum fix talkable/talkable#2` (f on the screens) runs claude-fix again", call} {
			if !ok || !strings.Contains(ev.Message, want) {
				t.Errorf("fix.needs_you = %q, %v; want it to say %q", ev.Message, ok, want)
			}
		}
		if toasts := toastsWith(h, "findings wait"); len(toasts) == 0 {
			t.Errorf("toasts = %v, want one that says the findings wait", h.nh.shown())
		}
	})
}

// The review's bugs: a hold that failed left held-merge.diff behind and
// failed the settle ("fix settle failed") instead of falling back to the
// merge by hand, and the ref named the repository, which git refuses for
// one named .github. The ref is the PR's registry id now, the file is
// written last, and any failure of the hold leaves nothing and needs you
// as before.
func TestAHoldThatFailsLeavesNothingAndNeedsYouAsBefore(t *testing.T) {
	storetest.Parallel(t)
	for _, tc := range []struct {
		name string
		fail func(h *harness, mg *mergeGit)
	}{
		{"the ref", func(_ *harness, mg *mergeGit) { mg.held.refErr = errors.New("fatal: cannot lock ref") }},
		{"the file", func(h *harness, _ *mergeGit) { // a directory where held-merge.diff goes
			if err := os.MkdirAll(filepath.Join(h.heldDiffPath(), "x"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, mg := handedMerge(t)
			heldConflicts(mg)
			mg.mu.Lock()
			tc.fail(h, mg)
			mg.mu.Unlock()
			id := h.fix(FixPayload{Mode: FixModeMerge})
			h.handle()
			h.wantDone(id)
			wantNoPush(t, mg)
			if held, ok := h.heldMergeOfPR(); ok {
				t.Errorf("held: %+v", held)
			}
			if refs := mg.heldRefs(); len(refs) != 0 {
				t.Errorf("held refs = %v", refs)
			}
			if fi, err := os.Stat(h.heldDiffPath()); err == nil && fi.Mode().IsRegular() {
				t.Error("held-merge.diff stayed")
			}
			got := deref(h.wantState(2, store.PRNeedsAttention).LastError)
			if !strings.HasPrefix(got, "fix: magnum did not push claude-fix's merge of the parent of talkable/talkable#2: ") ||
				!strings.Contains(got, "keeps the PR's own workflow changes") || !strings.Contains(got, "merge master into feature by hand") {
				t.Errorf("attention = %q, want the merge by hand", got)
			}
			if _, ok := h.fixEvent("fix.failed"); ok {
				t.Error("the settle failed")
			}
		})
	}
	t.Run("a repository named .github", func(t *testing.T) {
		h, mg := handedMerge(t)
		heldConflicts(mg)
		pr := h.pr(2)
		role, ok := h.e.cfg.FixRoleFor(h.e.cfg.WatchFor("talkable/talkable"))
		if !ok {
			t.Fatal("no fix role")
		}
		job := &fixJob{repo: store.Repo{ID: pr.RepoID, Owner: "example", Name: ".github"}, pr: pr, slot: h.slot("review1"),
			role: role, head: fixHead, mode: FixModeMerge}
		id, _, _ := h.e.fixerSignature(job.role.Identity)
		held, why := h.e.holdMerge(h.ctx, job, id, fixHead, fixMergeHead, fixConflict{base: "master", sha: fixBaseTip},
			pushRefusal{why: "keeps the PR's own workflow changes", own: []string{".github/workflows/ci.yml"}, files: []string{".github/workflows/ci.yml"}})
		if why != "" || held.Merge != fixMergeHead {
			t.Fatalf("holdMerge = %+v, %q; want it held", held, why)
		}
		if refs := mg.heldRefs(); refs[job.slot.MainClone+" refs/magnum/held/"+fmt.Sprint(pr.ID)] != fixMergeHead {
			t.Errorf("held refs = %v, want refs/magnum/held/%d", refs, pr.ID)
		}
		if _, err := os.Stat(filepath.Join(h.d.Layout.PRReviews("example", ".github", 2), HeldMergeFile)); err != nil {
			t.Errorf("held-merge.diff: %v", err)
		}
	})
}

// A merge of the parent on a PR magnum never reviewed (a baseline) is held
// all the same, but needs_attention is not its state to take: the PR would
// then be queued for a review at its next push. An urgent toast says merge
// ready instead, and its row's e and P act on it.
func TestAHeldMergeOnAPRMagnumNeverReviewedToastsMergeReady(t *testing.T) {
	h, mg := mergeHarness(t, withFixerApp)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead, author: "zhuravel"})
	h.startup()
	h.tick() // first sync: both baseline
	h.wantState(2, store.PRBaseline)
	mg.behind, mg.lostPush = true, true
	heldConflicts(mg)
	fixer(h, mg.fixGit, fixMergeHead, func(run store.Run) string {
		return fixReport(run.ID, "d3d3d3d Merge c0c0c0c: not run: environment", "0 threads (0 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none")
	})
	id := h.fix(FixPayload{Mode: FixModeMerge})
	h.handle()
	h.wantDone(id)
	if _, ok := h.heldMergeOfPR(); !ok {
		t.Fatal("no held merge")
	}
	h.wantState(2, store.PRBaseline)
	if toasts := toastsWith(h, "merge ready"); len(toasts) == 0 {
		t.Errorf("toasts = %v, want one that says merge ready", h.nh.shown())
	}
}

// magnum never deletes a PR's row, but a held merge whose row is gone is
// dropped at the poll all the same, its ref with it.
func TestAHeldMergeOfAPRRowThatIsGoneIsDropped(t *testing.T) {
	h, mg := holdMerged(t)
	held, _ := h.heldMergeOfPR()
	b, err := json.Marshal(held)
	if err != nil {
		t.Fatal(err)
	}
	const gone = 990
	if err := h.st.SetKV(h.ctx, KVPRHeldMerge(gone), string(b)); err != nil {
		t.Fatal(err)
	}
	if err := mg.UpdateRef(h.ctx, held.Clone, HeldMergeRef(gone), held.Merge); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if raw, ok, _ := h.st.GetKV(h.ctx, KVPRHeldMerge(gone)); ok {
		t.Errorf("the row of a PR that is gone stayed: %q", raw)
	}
	if refs := mg.heldRefs(); len(refs) != 1 || refs[h.heldRefKey()] != fixMergeHead {
		t.Errorf("held refs = %v, want PR #2's alone", refs)
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
			wantNoPush(t, mg) // the App without Workflows tried to mint its token
			wantNoHeld(t, h, mg)
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
	wantNoHeld(t, h, mg)
	const why = "keeps the PR's own workflow changes (.github/workflows/ci.yml), which " + fixerAppLogin + " never pushes"
	if got := deref(h.wantState(2, store.PRNeedsAttention).LastError); !strings.Contains(got, why) || !strings.Contains(got, "by hand") {
		t.Errorf("attention = %q, want the refusal %q and the merge by hand", got, why)
	}
}

// startParkedPush plans the push of PR #2's held merge and sends its y with
// the fake push parked, the request handled with no wait for the job, and
// waits until the push starts; closing the gate it returns lets it go on.
func startParkedPush(t *testing.T, h *harness, mg *mergeGit) chan struct{} {
	t.Helper()
	gate, started := make(chan struct{}), make(chan struct{}, 1)
	asked := h.planMerge().Asked
	mg.mu.Lock()
	mg.held.pushGate, mg.held.pushStarted = gate, started
	mg.mu.Unlock()
	id := h.fix(FixPayload{Mode: FixModeMerge, Asked: &asked})
	h.e.handleRequests(h.ctx)
	if r := h.request(id); r.State != store.RequestDone || deref(r.Result) != heldPushing {
		t.Fatalf("the y: %s %q", r.State, deref(r.Result))
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the push never started")
	}
	return gate
}

// The review's bug: the push ran under the job's context, which `magnum
// abort` cancels, so an abort could kill git push after GitHub took the
// merge, and the job then said "aborted" and skipped what follows a push.
// The push itself runs to its end now (an abort acts only before it
// starts): an abort during a parked push lets it finish and settle as
// pushed, and the abort hears that it came too late.
func TestAnAbortDuringTheHeldPushLetsItFinishAsPushed(t *testing.T) {
	h, mg := holdMerged(t)
	gate := startParkedPush(t, h, mg)
	abort := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.e.handleRequests(h.ctx) // the abort cancels the job's context while git push runs
	close(gate)
	h.settle()
	if _, pushes := mg.made(); len(pushes) != 1 {
		t.Fatalf("pushes = %v, want the held merge's", pushes)
	}
	wantNoHeld(t, h, mg)
	if cur := h.pr(2); cur.HeadSHA != fixMergeHead {
		t.Errorf("the PR's head = %s, want the merge", cur.HeadSHA)
	}
	if _, ok, _ := h.st.GetKV(h.ctx, kvPRPushed(h.pr(2).ID)); !ok {
		t.Error("magnum's push is not recorded for its CI")
	}
	if _, ok := h.fixEvent("fix.parent_pushed"); !ok {
		t.Error("no fix.parent_pushed")
	}
	for _, kind := range []string{"fix.held_dropped", "pr.aborted"} {
		if ev, ok := h.fixEvent(kind); ok {
			t.Errorf("%s = %q after a push that went", kind, ev.Message)
		}
	}
	const want = "nothing to abort: magnum pushed the held merge of talkable/talkable#2 already"
	if r := h.request(abort); r.State != store.RequestDone || deref(r.Result) != want {
		t.Errorf("the abort: %s %q, want done %q", r.State, deref(r.Result), want)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the push's loop row stayed")
	}
}

// A poll while the push's job holds the PR leaves its held merge to the
// job, even when the merge looks stale to it (master's tip moved): the row
// and the attention stay, and nothing says it was dropped.
func TestThePollLeavesTheHeldMergeOfAPushAtWork(t *testing.T) {
	h, mg := holdMerged(t)
	gate := startParkedPush(t, h, mg)
	h.gh.mu.Lock()
	h.gh.baseTip = fixMovedTip
	h.gh.mu.Unlock()
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	row, attn := h.heldRow(), deref(h.pr(2).LastError)
	_, dropped := h.fixEvent("fix.held_dropped")
	close(gate)
	h.settle()
	if row == "" || !attention.MergeReady(attn) || dropped {
		t.Errorf("after a poll during the push: row %q, attention %q, dropped %v; want the row, merge ready and no drop", row, attn, dropped)
	}
}

// The review's bug: a restart that cut the push left its loop row, and the
// start's recovery ran failFix, which put "fix run lost in a daemon
// restart" in the place of the held merge's attention and its calls. The
// start ends a held push's row quietly now: the held merge, its attention
// and the calls the loop carried stay, and P asks its push again.
func TestARestartThatCutTheHeldPushKeepsItsAttentionAndCalls(t *testing.T) {
	h, _ := heldWithCalls(t, heldCall)
	before := deref(h.pr(2).LastError)
	held, _ := h.heldMergeOfPR()
	if err := h.e.setFixState(h.ctx, h.pr(2).ID, FixState{Cycle: 1, Cap: 1, Phase: FixPhaseFixing, Mode: FixModeMerge, By: heldPushBy,
		Head: held.Head, Since: held.Head, Role: "claude-fix", StartedAt: h.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	h.e.recoverFixes(h.ctx)
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the cut push's loop row stayed")
	}
	if got := deref(h.wantState(2, store.PRNeedsAttention).LastError); got != before {
		t.Errorf("attention = %q, want %q kept", got, before)
	}
	if plan := h.planMerge(); plan.Asked.Merge != fixMergeHead {
		t.Errorf("the plan = %+v, want the held merge's push", plan)
	}
}

// A panic during the push ends its job without failFix too: the held
// merge, its row and its attention stay, the loop row goes, and fix.failed
// says what happened.
func TestAPanicDuringTheHeldPushKeepsTheHeldMerge(t *testing.T) {
	h, mg := holdMerged(t)
	mg.mu.Lock()
	mg.held.pushPanics = true
	mg.mu.Unlock()
	if r := h.mergeBound(h.planMerge().Asked); r.State != store.RequestDone {
		t.Fatalf("the y: %s %q", r.State, deref(r.Result))
	}
	if _, ok := h.heldMergeOfPR(); !ok {
		t.Error("the held merge went")
	}
	if got := deref(h.wantState(2, store.PRNeedsAttention).LastError); got != heldAttention {
		t.Errorf("attention = %q, want %q kept", got, heldAttention)
	}
	if ev, ok := h.fixEvent("fix.failed"); !ok || !strings.Contains(ev.Message, "panicked") {
		t.Errorf("fix.failed = %q, %v", ev.Message, ok)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the push's loop row stayed")
	}
}

// A push that reached GitHub before a crash cut its job is no stale merge:
// the poll that finds the PR's head on the held merge settles it as pushed
// (its row, ref and file go, its CI is noted, fix.parent_pushed says so),
// never "merge dropped … P merges the parent again" over a merge that is on
// GitHub.
func TestAHeldMergeThePollFindsOnGitHubSettlesAsPushed(t *testing.T) {
	h, mg := holdMerged(t)
	h.gh.setHead("talkable/talkable", 2, fixMergeHead) // the push went, and the daemon stopped before it said so
	h.tick()
	wantNoHeld(t, h, mg)
	if ev, ok := h.fixEvent("fix.held_dropped"); ok {
		t.Errorf("fix.held_dropped = %q over a merge on GitHub", ev.Message)
	}
	if ev, ok := h.fixEvent("fix.parent_pushed"); !ok || !strings.Contains(ev.Message, "d3d3d3d") {
		t.Errorf("fix.parent_pushed = %q, %v", ev.Message, ok)
	}
	if _, ok, _ := h.st.GetKV(h.ctx, kvPRPushed(h.pr(2).ID)); !ok {
		t.Error("magnum's push is not recorded for its CI")
	}
	if got := deref(h.pr(2).LastError); attention.MergeDropped(got) || attention.MergeReady(got) {
		t.Errorf("attention = %q after the merge is on GitHub", got)
	}
}

// The review's bug: holdMerge writes the ref and held-merge.diff before the
// settle writes the row, and a settle whose slot could not be put back
// failed before the held case, leaving both with no row. They go now.
func TestAFailedSettleLeavesNoHeldRefOrFile(t *testing.T) {
	h, mg := handedMerge(t)
	heldConflicts(mg)
	fixer(h, mg.fixGit, fixMergeHead, func(run store.Run) string {
		h.sl.mu.Lock()
		h.sl.resyncErr = errors.New("slots: guard review1: herdr snapshot: connection refused")
		h.sl.mu.Unlock()
		return fixReport(run.ID, "d3d3d3d Merge c0c0c0c: not run: environment", "0 threads (0 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none")
	})
	id := h.fix(FixPayload{Mode: FixModeMerge})
	h.handle()
	h.wantDone(id)
	wantNoHeld(t, h, mg)
	if got := deref(h.wantState(2, store.PRNeedsAttention).LastError); !strings.HasPrefix(got, "fix settle failed: ") {
		t.Errorf("attention = %q, want the failed settle", got)
	}
}

// A crash between the hold and its row (or any row that went while its
// files stayed) leaves a ref under refs/magnum/held/ and a held-merge.diff
// that no row names: the start removes them, and leaves those a row names.
func TestTheStartRemovesHeldRefsAndFilesNoRowNames(t *testing.T) {
	h, mg := holdMerged(t)
	clone := h.slot("review1").MainClone
	if err := mg.UpdateRef(h.ctx, clone, HeldMergeRef(77), fixMergeHead); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(h.d.Layout.PRReviews("talkable", "talkable", 3), HeldMergeFile)
	if err := writeHeldDiff(filepath.Dir(orphan), orphan, []byte("Held merge …\n")); err != nil {
		t.Fatal(err)
	}
	h.e.sweepHeldOrphans(h.ctx)
	if refs := mg.heldRefs(); len(refs) != 1 || refs[h.heldRefKey()] != fixMergeHead {
		t.Errorf("held refs = %v, want PR #2's alone", refs)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the orphan held-merge.diff stayed: %v", err)
	}
	if _, err := os.Stat(h.heldDiffPath()); err != nil {
		t.Errorf("PR #2's held-merge.diff: %v", err)
	}
	if err := h.st.DeleteKV(h.ctx, KVPRHeldMerge(h.pr(2).ID)); err != nil { // the crash: the files and no row
		t.Fatal(err)
	}
	h.e.sweepHeldOrphans(h.ctx)
	wantNoHeld(t, h, mg)
}

// The review's bug (H1): a diff past execx's cap came back cut and was
// held, so the y could push workflow content past the cut that nobody
// read. A diff that cannot be read whole holds nothing: the PR needs you
// as before, to merge by hand.
func TestADiffThatCannotBeReadWholeHoldsNothing(t *testing.T) {
	h, mg := handedMerge(t)
	heldConflicts(mg)
	mg.mu.Lock()
	mg.held.diffErr = errors.New("gitx: show --remerge-diff: more than 8388608 bytes")
	mg.mu.Unlock()
	id := h.fix(FixPayload{Mode: FixModeMerge})
	h.handle()
	h.wantDone(id)
	wantNoPush(t, mg)
	wantNoHeld(t, h, mg)
	if got := deref(h.wantState(2, store.PRNeedsAttention).LastError); !strings.Contains(got, "keeps the PR's own workflow changes") ||
		!strings.Contains(got, "merge master into feature by hand") {
		t.Errorf("attention = %q, want the merge by hand", got)
	}
}

// The review's bug (H3): the workflow files' names came from the fixer's
// merge as they were, and reached the y/N, the attention and the toast
// with their bidi controls, which can reverse the rest of the question on
// a terminal that does bidi. Their words escape them, as held-merge.diff
// does.
func TestTheWorkflowFilesNamesAreEscapedInTheWords(t *testing.T) {
	h, mg := handedMerge(t)
	heldConflicts(mg)
	mg.mu.Lock()
	mg.workflows = []string{".github/workflows/ci.yml", ".github/workflows/\u202elmy.yrotsih"}
	mg.mu.Unlock()
	id := h.fix(FixPayload{Mode: FixModeMerge})
	h.handle()
	h.wantDone(id)
	attn := deref(h.wantState(2, store.PRNeedsAttention).LastError)
	plan := h.planMerge().Text
	for what, words := range map[string]string{"attention": attn, "plan": plan} {
		if strings.ContainsRune(words, '\u202e') || !strings.Contains(words, "and \\u202elmy.yrotsih") {
			t.Errorf("the %s = %q, want the file's U+202E escaped", what, words)
		}
	}
}

// The review's bug (H5): P on a held merge GitHub already took (a stop cut
// its push's job, or git push failed after GitHub updated the branch)
// called it stale, dropped it with "merge dropped … P merges the parent
// again" and skipped what follows a push. The plan, the y's push and a
// head the registry already has all settle it as pushed: no new push, its
// row, ref and file go, fix.parent_pushed says so and its CI is noted.
func TestPOnAHeldMergeGitHubTookSettlesItAsPushed(t *testing.T) {
	storetest.Parallel(t)
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, h *harness) // P, with the merge on GitHub
	}{
		{"the plan", func(t *testing.T, h *harness) {
			t.Helper()
			h.gh.setHead("talkable/talkable", 2, fixMergeHead)
			id := h.fix(FixPayload{Mode: FixModeMerge, DryRun: true})
			h.handle()
			if res := h.wantDone(id); !strings.Contains(res, "is on GitHub") {
				t.Errorf("the plan's answer = %q, want the merge on GitHub", res)
			}
		}},
		{"the y", func(t *testing.T, h *harness) {
			t.Helper()
			asked := h.planMerge().Asked
			h.gh.setHead("talkable/talkable", 2, fixMergeHead)
			if r := h.mergeBound(asked); r.State != store.RequestDone {
				t.Errorf("the y: %s %q", r.State, deref(r.Result))
			}
		}},
		{"the registry's head", func(t *testing.T, h *harness) {
			t.Helper()
			h.gh.setHead("talkable/talkable", 2, fixMergeHead)
			h.execSQL(`UPDATE prs SET head_sha = ? WHERE id = ?`, fixMergeHead, h.pr(2).ID)
			id := h.fix(FixPayload{Mode: FixModeMerge, DryRun: true})
			h.handle()
			if res := h.wantDone(id); !strings.Contains(res, "is on GitHub") {
				t.Errorf("the plan's answer = %q, want the merge on GitHub", res)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, mg := holdMerged(t)
			tc.run(t, h)
			wantNoPush(t, mg)
			wantNoHeld(t, h, mg)
			if ev, ok := h.fixEvent("fix.held_dropped"); ok {
				t.Errorf("fix.held_dropped = %q over a merge on GitHub", ev.Message)
			}
			if _, ok := h.fixEvent("fix.parent_pushed"); !ok {
				t.Error("no fix.parent_pushed")
			}
			if _, ok, _ := h.st.GetKV(h.ctx, kvPRPushed(h.pr(2).ID)); !ok {
				t.Error("magnum's push is not recorded for its CI")
			}
			if got := deref(h.pr(2).LastError); attention.MergeDropped(got) {
				t.Errorf("attention = %q over a merge on GitHub", got)
			}
		})
	}
}

// The review's bug (H6): the push read any failure to read the held ref as
// a ref that is gone, a cancelled context among them, and dropped the
// held merge with a false reason. A ref git cannot read now keeps the hold:
// the push fails, and P asks again.
func TestAGitErrorReadingTheHeldRefKeepsTheHold(t *testing.T) {
	h, mg := holdMerged(t)
	mg.mu.Lock()
	mg.held.refReadErr = context.Canceled
	mg.mu.Unlock()
	if r := h.mergeBound(h.planMerge().Asked); r.State != store.RequestDone {
		t.Fatalf("the y: %s %q", r.State, deref(r.Result))
	}
	wantNoPush(t, mg)
	if _, ok := h.heldMergeOfPR(); !ok {
		t.Error("the held merge went")
	}
	if ev, ok := h.fixEvent("fix.held_dropped"); ok {
		t.Errorf("fix.held_dropped = %q", ev.Message)
	}
	if ev, ok := h.fixEvent("fix.failed"); !ok || !strings.Contains(ev.Message, "nothing was pushed") {
		t.Errorf("fix.failed = %q, %v", ev.Message, ok)
	}
}

// The review's bug (H7): `magnum ignore` during a held push that went was
// answered "nothing to abort", and the PR was never muted. The push goes
// on, and the ignore then mutes the PR, as it does after a fixer's push.
func TestAnIgnoreDuringTheHeldPushMutesThePRAfterIt(t *testing.T) {
	h, mg := holdMerged(t)
	gate := startParkedPush(t, h, mg)
	ignore := h.enqueue(ReqIgnore, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.e.handleRequests(h.ctx)
	close(gate)
	h.settle()
	if _, pushes := mg.made(); len(pushes) != 1 {
		t.Fatalf("pushes = %v, want the held merge's", pushes)
	}
	if r := h.request(ignore); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "muted") {
		t.Errorf("the ignore: %s %q, want it muted", r.State, deref(r.Result))
	}
	if cur := h.pr(2); !cur.Muted {
		t.Errorf("the PR is %s, not muted", cur.State)
	}
}

// The review's bug (H8): a fix on a PR whose merge magnum holds could
// merge the base again and move or delete the held merge's ref and file,
// leaving its row to plan a push of a ref that is gone. A fix is refused
// while a merge is held: P pushes it first (or a push to the branch drops
// it).
func TestAFixIsRefusedWhileAMergeIsHeld(t *testing.T) {
	h, mg := holdMerged(t)
	runs := len(h.fixRuns(h.pr(2).ID))
	id := h.fix(FixPayload{})
	h.handle()
	r := h.request(id)
	if r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "holds claude-fix's merge d3d3d3d") ||
		!strings.Contains(deref(r.Result), "merge-parent") {
		t.Errorf("the fix: %s %q, want it refused while the merge is held", r.State, deref(r.Result))
	}
	if got := len(h.fixRuns(h.pr(2).ID)); got != runs {
		t.Errorf("fix runs = %d, want %d", got, runs)
	}
	if refs := mg.heldRefs(); refs[h.heldRefKey()] != fixMergeHead {
		t.Errorf("held refs = %v, want the hold's", refs)
	}
}

// The review's bug (J3): the refusal of a fix while a merge is held fired
// on a row the registry already showed stale (its head moved by a push of
// the operator's while a job held the PR, so the poll had left it), and an
// autofix refused so is never tried again. A fix now drops a stale held
// merge, as P does, and goes on.
func TestAFixDropsAStaleHeldMergeAndGoesOn(t *testing.T) {
	storetest.Parallel(t)
	for _, tc := range []struct {
		name  string
		stale func(*HeldMerge)
		why   string
	}{
		{"head moved", func(held *HeldMerge) { held.Head = strings.Repeat("e", 40) }, "its head moved from eeeeeee"},
		{"another PR", func(held *HeldMerge) { held.Number = 99 }, "its row names talkable/talkable#99"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := heldWithCalls(t, heldCall)
			h.setHeld(tc.stale)
			runs := len(h.fixRuns(h.pr(2).ID))
			id := h.fix(FixPayload{})
			h.handle()
			if r := h.request(id); r.State != store.RequestDone || strings.Contains(deref(r.Result), "magnum holds") {
				t.Errorf("the fix: %s %q, want it started over the stale held merge", r.State, deref(r.Result))
			}
			if ev, ok := h.fixEvent("fix.held_dropped"); !ok || !strings.Contains(ev.Message, tc.why) {
				t.Errorf("fix.held_dropped = %q, %v; want %q in it", ev.Message, ok, tc.why)
			}
			if got := len(h.fixRuns(h.pr(2).ID)); got != runs+1 {
				t.Errorf("fix runs = %d, want %d: the fix did not start", got, runs+1)
			}
		})
	}
}
