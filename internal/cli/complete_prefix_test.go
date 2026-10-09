package cli

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// PR completion took the completeLimit newest PRs before the shell filtered
// them by what was typed, so an older PR never completed: with 205 merged
// PRs, `magnum misses talkable/talkable#1<TAB>` and `misses 3<TAB>` offered
// neither #1 nor #3. The typed text filters the PRs before the limit.
func TestPRCompletionFindsOldPRsPastTheLimit(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	n := completeLimit + 5
	for i := 1; i <= n; i++ {
		closed := start.Add(time.Duration(i) * time.Hour) // #1 closed first, so it is the oldest
		inspSeedPR(t, st, "talkable/talkable", i, store.PRReleased, func(u *store.PRUpdate) {
			u.Set("gh_state", store.GHMerged)
			u.Set("closed_at", closed)
		})
	}
	st.Close()
	refs := func(args ...string) []string {
		var out []string
		for _, c := range complete(t, f.Ctx, args...) {
			ref, _, _ := strings.Cut(c, "\t")
			out = append(out, ref)
		}
		return out
	}
	head := func(refs []string) []string { return refs[:min(4, len(refs))] }
	if got := refs("misses", "talkable/talkable#1"); !slices.Contains(got, "talkable/talkable#1") || slices.Contains(got, "talkable/talkable#2") {
		t.Errorf("misses talkable/talkable#1: %d candidates %q...", len(got), head(got))
	}
	if got := refs("misses", "TALKABLE/talkable#2"); !slices.Contains(got, "talkable/talkable#2") {
		t.Errorf("a prefix in other case: %d candidates %q...", len(got), head(got))
	}
	if got := refs("misses", "3"); !slices.Contains(got, "3") || slices.Contains(got, "4") {
		t.Errorf("misses 3: %d candidates %q...", len(got), head(got))
	}
	if got := refs("misses", "talkable/talkable#_"); len(got) != 0 {
		t.Errorf("a LIKE wildcard in the prefix matched: %d candidates %q...", len(got), head(got))
	}
	if got := refs("misses", ""); len(got) != completeLimit || got[0] != "talkable/talkable#205" {
		t.Errorf("nothing typed: %d candidates %q...", len(got), head(got))
	}
}
