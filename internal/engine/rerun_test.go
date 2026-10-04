package engine

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
)

// codePatch is a Ruby patch adding n lines of code.
func codePatch(n int) []github.FileDelta {
	var b strings.Builder
	fmt.Fprintf(&b, "@@ -1,1 +1,%d @@\n class Coupon\n", n+1)
	for i := range n {
		fmt.Fprintf(&b, "+  def rule_%d = %d\n", i, i)
	}
	return []github.FileDelta{{Path: "app/models/coupon.rb", Status: "modified", Patch: b.String()}}
}

// TestSimplifyRerunsAfterSignificantChanges: claude-simplify (runs = "first",
// rerun_min_lines 150) runs on the first review, then again only when the
// code lines changed since the head of its last run reach the threshold;
// comments and smaller changes leave it on request.
func TestSimplifyRerunsAfterSignificantChanges(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1") // the first round runs claude-simplify on b1
	requested := func(i int) []string {
		h.rd.mu.Lock()
		defer h.rd.mu.Unlock()
		if i >= len(h.rd.inputs) {
			t.Fatalf("round %d did not run (%d rounds)", i, len(h.rd.inputs))
		}
		return h.rd.inputs[i].Requested
	}

	h.gh.files = map[string][]github.FileDelta{"b1...b2": codePatch(40)}
	pollPR(h, time.Minute, 2, "b2")
	h.advance(time.Hour)
	h.tick()
	if slices.Contains(requested(1), "claude-simplify") {
		t.Fatalf("40 lines since its run reran simplify: %v", requested(1))
	}

	h.gh.files = map[string][]github.FileDelta{"b1...b3": codePatch(160), "b2...b3": codePatch(120)}
	pollPR(h, time.Minute, 2, "b3")
	h.advance(time.Hour)
	h.tick()
	if !slices.Contains(requested(2), "claude-simplify") {
		t.Fatalf("160 code lines since its run at b1 did not rerun simplify: %v", requested(2))
	}
}
