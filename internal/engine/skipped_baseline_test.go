package engine

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/store"
)

// TestStartupSkipsBaselinePRsTheConfigSkips: a PR open before the first sync
// whose author the watch skips reads as skipped (ineligible with the rule's
// reason) after a daemon start, not as "not reviewed"; once the config no
// longer skips it, and nobody pushed to it, it goes back to baseline rather
// than in line for a review.
func TestStartupSkipsBaselinePRsTheConfigSkips(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "a1", author: "dependabot"}, prSpec{n: 2, head: "b1"})
	h.startup()
	h.tick() // first sync: both baseline
	h.wantState(1, store.PRBaseline)

	h.e.reclassifyIneligible(h.ctx) // what the next daemon start runs
	if pr := h.wantState(1, store.PRIneligible); !strings.Contains(deref(pr.SkipReason), "skip_authors") {
		t.Fatalf("skip reason %q", deref(pr.SkipReason))
	}
	h.wantState(2, store.PRBaseline)

	h.cfg.Watches[0].SkipAuthors = nil
	h.e.reclassifyIneligible(h.ctx)
	if pr := h.wantState(1, store.PRBaseline); deref(pr.SkipReason) != "" {
		t.Fatalf("restored with skip reason %q", deref(pr.SkipReason))
	}
	if _, ok := h.e.getKV(h.ctx, KVPRSkippedBaseline(h.pr(1).ID)); ok {
		t.Fatal("the skipped-baseline mark outlived the restore")
	}
}
