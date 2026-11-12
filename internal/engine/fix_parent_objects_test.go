package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
)

// objectsFake is mergeGit's isolated reads (gitx.Client.Isolated, which
// runs its func in "iso:<dir>") and its objects' check (VerifyObjects,
// recorded as "<dir> <path> <commits>"): the reads that decide or show a
// merge's workflow files ("<read> <dir>"), and forged, VerifyObjects'
// answer from its call number forgedFrom on (1 for the first; 0 for
// none).
type objectsFake struct {
	reads      []string
	verified   []string
	forged     error
	forgedFrom int
}

// read records a read of the workflow files in dir.
func (o *objectsFake) read(what, dir string) { o.reads = append(o.reads, what+" "+dir) }

func (g *mergeGit) Isolated(_ context.Context, dir string, fn func(iso string) error) error {
	return fn("iso:" + dir)
}

func (g *mergeGit) VerifyObjects(_ context.Context, dir, path string, commits ...string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	o := &g.objects
	o.verified = append(o.verified, fmt.Sprintf("%s %s %s", dir, path, strings.Join(commits, ",")))
	if o.forgedFrom > 0 && len(o.verified) >= o.forgedFrom {
		return o.forged
	}
	return nil
}

// forge makes VerifyObjects find a forged object from its call number
// from on.
func (g *mergeGit) forge(from int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.objects.forged = fmt.Errorf("gitx: blob %s: its content hashes to %s: %w", strings.Repeat("5", 40), strings.Repeat("6", 40), gitx.ErrObjectMismatch)
	g.objects.forgedFrom = from
}

// objectReads are the reads of the workflow files and the objects'
// checks so far.
func (g *mergeGit) objectReads() (reads, verified []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.objects.reads), slices.Clone(g.objects.verified)
}

// forgedWord is what the words of a refusal for a forged object say.
const forgedWord = "is not what its name says"

// The review's bug (K1, K2): what magnum read of a merge's workflow files
// came from the clone, whose objects, commit-graph, config and attributes
// the fixer's pane can write, so a forged object or a merge driver of the
// clone's made the reads differ from what GitHub gets. Each read that
// decides or shows them runs in a repository of the daemon's own that
// shares the clone's objects (Isolated), after every object they go
// through is hashed again (VerifyObjects): the handed merge's push with
// the Workflows token, the hold's diff, and the push after the y.
func TestTheWorkflowReadsOfAMergeRunIsolatedOnObjectsHashedAgain(t *testing.T) {
	h, mg := handedMerge(t)
	id := h.fix(FixPayload{Mode: FixModeMerge})
	h.handle()
	h.wantDone(id)
	slot := h.slot("review1").Path
	reads, verified := mg.objectReads()
	if want := []string{"iso:" + slot + " .github/workflows " + fixHead + "," + fixBaseTip + "," + fixMergeHead}; !slices.Equal(verified, want) {
		t.Errorf("the push's checks = %q, want %q", verified, want)
	}
	// handedMerge's ChangedPaths says only whether the merge is one magnum push handed over: a wrong no pushes nothing
	if want := []string{"ChangedPaths " + slot, "ChangedPaths iso:" + slot, "DifferingPaths iso:" + slot}; !slices.Equal(reads, want) {
		t.Errorf("the push's reads = %q, want %q", reads, want)
	}

	h, mg = holdMerged(t)
	slot, clone := h.slot("review1").Path, h.slot("review1").MainClone
	reads, verified = mg.objectReads()
	if want := "iso:" + clone + " .github/workflows " + fixHead + "," + fixBaseTip + "," + fixMergeHead; len(verified) != 2 || verified[1] != want {
		t.Errorf("the hold's checks = %q, want the push's, then %q", verified, want)
	}
	if want := []string{"ChangedPaths " + slot, "ChangedPaths iso:" + slot, "DifferingPaths iso:" + slot, "RemergeDiff iso:" + clone,
		"PathsDiff iso:" + clone}; !slices.Equal(reads, want) {
		t.Errorf("the hold's reads = %q, want %q", reads, want)
	}
	a := h.planMerge().Asked
	if r := h.mergeBound(a); r.State != store.RequestDone {
		t.Fatalf("the y: %s %q", r.State, deref(r.Result))
	}
	reads, verified = mg.objectReads()
	if want := "iso:" + clone + " .github/workflows " + fixHead + "," + fixBaseTip + "," + fixMergeHead; len(verified) != 3 || verified[2] != want {
		t.Errorf("the held push's checks = %q, want %q last", verified, want)
	}
	if got := reads[len(reads)-1]; got != "Parents iso:"+clone {
		t.Errorf("the held push's last read = %q, want its parents, isolated", got)
	}
}

// A merge whose workflow objects are forged is neither pushed with the
// Workflows token nor held: the PR needs you, merge by hand, and the
// words say why.
func TestAMergeWhoseWorkflowObjectsAreForgedIsNeitherPushedNorHeld(t *testing.T) {
	h, mg := handedMerge(t)
	mg.forge(1)
	id := h.fix(FixPayload{Mode: FixModeMerge})
	h.handle()
	h.wantDone(id)
	wantNothingPushed(t, h, mg)
	if got := deref(h.wantState(2, store.PRNeedsAttention).LastError); !strings.Contains(got, forgedWord) || !strings.Contains(got, "by hand") {
		t.Errorf("the push's attention = %q, want the forged object and the merge by hand", got)
	}

	h, mg = handedMerge(t)
	heldConflicts(mg)
	mg.forge(2) // the push's check passes, and the hold's finds the forged object
	id = h.fix(FixPayload{Mode: FixModeMerge})
	h.handle()
	h.wantDone(id)
	wantNoPush(t, mg)
	wantNoHeld(t, h, mg)
	if got := deref(h.wantState(2, store.PRNeedsAttention).LastError); !strings.Contains(got, forgedWord) || attention.MergeReady(got) {
		t.Errorf("the hold's attention = %q, want the forged object and no merge ready", got)
	}
}

// A held merge whose workflow objects are forged after the hold is
// dropped at the y, never pushed.
func TestAHeldMergeWhoseObjectsAreForgedIsDroppedAtTheY(t *testing.T) {
	h, mg := holdMerged(t)
	mg.forge(3)
	r := h.mergeBound(h.planMerge().Asked)
	if r.State != store.RequestDone {
		t.Fatalf("the y: %s %q", r.State, deref(r.Result))
	}
	wantNoPush(t, mg)
	wantDropped(t, h, mg, forgedWord)
}

// The review's bug (K5): a fix on a PR whose head the poll had recorded
// as the held merge (GitHub took the push, and its job ended before it
// said so) dropped the merge as stale ("its head moved") instead of
// settling it as pushed, so no fix.parent_pushed, no CI after the push.
// The fix settles it as pushed first, as the poll and P do.
func TestAFixSettlesAHeldMergeGitHubTookAsPushed(t *testing.T) {
	h, mg := heldWithCalls(t, heldCall)
	if _, err := h.st.DB().ExecContext(h.ctx, `UPDATE prs SET head_sha = ? WHERE id = ?`, fixMergeHead, h.pr(2).ID); err != nil {
		t.Fatal(err)
	}
	id := h.fix(FixPayload{})
	h.handle()
	if r := h.request(id); strings.Contains(deref(r.Result), "magnum holds") {
		t.Errorf("the fix: %s %q, refused over a merge on GitHub", r.State, deref(r.Result))
	}
	if ev, ok := h.fixEvent("fix.held_dropped"); ok {
		t.Errorf("fix.held_dropped = %q over a merge on GitHub", ev.Message)
	}
	if ev, ok := h.fixEvent("fix.parent_pushed"); !ok || !strings.Contains(ev.Message, "as a fix found it on GitHub") {
		t.Errorf("fix.parent_pushed = %q, %v", ev.Message, ok)
	}
	wantNoHeld(t, h, mg)
}
