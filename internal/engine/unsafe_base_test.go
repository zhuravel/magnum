package engine

import (
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// A PR whose base branch name a shell would read (`main$(x)`: git allows it,
// and the judge prompts put origin/<base> into a git command when the merge
// base is unknown) is ineligible from the poll that finds it, and so is a
// queued PR retargeted to such a branch; a plain name, slashes and dots
// included, is reviewed.
func TestABaseBranchNameAShellWouldReadMakesThePRIneligible(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "a1"})
	h.startup()
	h.tick() // first sync: baseline

	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "a1"}, prSpec{n: 2, head: "b1", base: "main$(x)"}, prSpec{n: 3, head: "c1", base: "release/2026.10_rc-1"})
	h.tick()
	if pr := h.wantState(2, store.PRIneligible); deref(pr.SkipReason) != reasonUnsafeBase {
		t.Fatalf("#2 skip reason %q, want %q", deref(pr.SkipReason), reasonUnsafeBase)
	}
	h.wantState(3, store.PRQueued)

	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "a1"}, prSpec{n: 2, head: "b1", base: "main$(x)"}, prSpec{n: 3, head: "c1", base: "a|b"})
	h.tick()
	if pr := h.wantState(3, store.PRIneligible); deref(pr.SkipReason) != reasonUnsafeBase {
		t.Fatalf("retargeted #3 skip reason %q, want %q", deref(pr.SkipReason), reasonUnsafeBase)
	}
}
