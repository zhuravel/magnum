package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
	"github.com/zhuravel/magnum/internal/tui"
)

// mergedPR seeds talkable/talkable#n as a PR GitHub merged before magnum
// reviewed it: closed in the registry, MERGED on GitHub.
func (h *actHarness) mergedPR(n int) store.PR {
	h.t.Helper()
	pr := h.seedPR("talkable/talkable", n, store.PRQueued)
	h.setPR(pr.ID, store.PRClosed, func(u *store.PRUpdate) { u.Set("gh_state", store.GHMerged) })
	return pr
}

// The list holds open PRs only, so a query naming a merged one offers the
// registry's row for it (its state, title and GitHub state), never the stub
// that says magnum does not know it.
func TestPickQueryForAMergedPRListsTheRegistryRow(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 4, store.PRReviewed)
	h.mergedPR(5)
	calls := h.withPicker(func([]tui.PickEntry) tui.PickOutcome { return tui.PickOutcome{Action: tui.PickActionCancel} })
	if code := h.cmd("pick", "--query", "https://github.com/talkable/talkable/pull/5"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	got := (*calls)[0]
	if len(got.entries) != 2 || got.opts.Query != "talkable#5" {
		t.Fatalf("entries %+v, query %q", got.entries, got.opts.Query)
	}
	e := got.entries[0] // the offered PR comes first
	if e.Ref != "talkable/talkable#5" || e.State != "closed" || e.Title != "Fix coupon export" || e.GHState != store.GHMerged ||
		e.Review == nil || e.URL != "https://github.com/talkable/talkable/pull/5" || strings.Contains(e.Title, "not in magnum yet") {
		t.Errorf("offered entry %+v", e)
	}
	if got.entries[1].Ref != "talkable/talkable#4" || got.entries[1].GHState != store.GHOpen {
		t.Errorf("listed entry %+v, want the open PR with its GitHub state", got.entries[1])
	}
}

// A PR the registry does not know keeps the offer to review it.
func TestPickQueryForAnUnknownPRKeepsTheStub(t *testing.T) {
	h := newActHarness(t)
	h.mergedPR(5)
	calls := h.withPicker(func([]tui.PickEntry) tui.PickOutcome { return tui.PickOutcome{Action: tui.PickActionCancel} })
	if code := h.cmd("pick", "--query", "talkable#6"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if e := (*calls)[0].entries[0]; e.Ref != "talkable/talkable#6" || e.State != "new" || e.GHState != "" || !strings.HasPrefix(e.Title, "(not in magnum yet") {
		t.Errorf("offered entry %+v", e)
	}
}

// A typed reference the list lacks is resolved to the PR the registry knows
// (a merged one), and only to that: an unknown PR is not found. Lookup runs
// while the picker does, like the screen's own calls.
func TestPickScreenLookupFindsAMergedPR(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 4, store.PRReviewed)
	h.mergedPR(5)
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	old := tuiPicker
	t.Cleanup(func() { tuiPicker = old })
	tuiPicker = func(_ context.Context, entries []tui.PickEntry, opts tui.PickerOptions) (tui.PickOutcome, error) {
		if len(entries) != 1 || entries[0].Ref != "talkable/talkable#4" {
			t.Errorf("the list holds open PRs only: %+v", entries)
		}
		if opts.Lookup == nil {
			t.Fatal("the picker got no Lookup")
		}
		e, ok := opts.Lookup("talkable#5")
		if !ok || e.Ref != "talkable/talkable#5" || e.GHState != store.GHMerged || e.State != "closed" || e.Title != "Fix coupon export" {
			t.Errorf("Lookup(talkable#5) = %+v, %v", e, ok)
		}
		if e, ok := opts.Lookup("https://github.com/talkable/talkable/pull/5"); !ok || e.Ref != "talkable/talkable#5" {
			t.Errorf("Lookup(URL) = %+v, %v", e, ok)
		}
		if e, ok := opts.Lookup("talkable#99"); ok {
			t.Errorf("Lookup found a PR the registry does not know: %+v", e)
		}
		if e, ok := opts.Lookup("nothing like it"); ok {
			t.Errorf("Lookup found %+v for text that is no reference", e)
		}
		return tui.PickOutcome{Action: tui.PickActionCancel}, nil
	}
	if code := h.cmd("pick"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
}

// pickRefEntry fills the PR's GitHub state and a trimmed title from the
// registry.
func TestPickRefEntryCarriesGitHubStateAndTitle(t *testing.T) {
	h := newActHarness(t)
	pr := h.mergedPR(5)
	long := strings.Repeat("long title ", 20) + "\x1b[31m"
	h.setPR(pr.ID, store.PRClosed, func(u *store.PRUpdate) { u.Set("title", long) })
	e, ok := pickRefEntry(h.ctx, h.d, "talkable#5")
	if !ok || !e.Known || e.GHState != store.GHMerged || e.State != "closed" || e.Title != textx.Clip(actClean(long), 90) || len([]rune(e.Title)) > 90 {
		t.Fatalf("entry %+v, %v", e, ok)
	}
	if e, ok := pickRefEntry(h.ctx, h.d, "talkable#6"); !ok || e.Known || e.GHState != "" || e.Title != "" {
		t.Errorf("unknown PR entry %+v, %v", e, ok)
	}
}
