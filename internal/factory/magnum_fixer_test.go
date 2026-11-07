package factory

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// reportText is a fixer's report of run runID whose "## Needs you" lists
// needs (none: "none").
func reportText(runID string, needs ...string) string {
	body := "none"
	if len(needs) > 0 {
		body = "- " + strings.Join(needs, "\n- ")
	}
	return agents.ReportMarker(runID) + "\n## Pushed\nnothing\n## Replied\n0 threads\n## Needs you\n" + body + "\n"
}

// writeReport writes text at path, its directory made.
func writeReport(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fixAttention is PR #n of example/shop in magnum's registry, parked in
// needs_attention with msg as its last_error and kind as its attention.
func fixAttention(t *testing.T, st *store.Store, n int, kind, msg string) {
	t.Helper()
	pr := magnumPR(t, st, n, strings.Repeat("ab", 20), nil)
	if err := st.TransitionPR(t.Context(), pr.ID, nil, store.PRNeedsAttention, func(u *store.PRUpdate) { u.Set("last_error", msg) }); err != nil {
		t.Fatal(err)
	}
	setKV(t, st, engine.KVPRAttention(pr.ID), kind)
}

// reportOf is where the fixer of PR #n of example/shop writes its report in
// layout l.
func reportOf(l paths.Layout, n int) string {
	return filepath.Join(l.ReviewDir("example", "shop", n, strings.Repeat("ab", 20)), "claude-fix.md")
}

// The factory screen said only "magnum: fix" of an own PR whose fixer left
// one item to the author (talkable#12215): what the item was is in the
// fixer's report, which the fix job's message names. PRStates and OwnPRs
// now read, for a PR whose attention is a fix job's, that report
// (FixReport) and, when the message asks for what the fixer left, its
// Needs you entries in the fixer's words (FixNeeds), token-shaped text
// masked. A cap's report is read for its path alone; a report outside
// magnum's reviews directory, behind a link that leads out of it, missing,
// or named by another kind of attention is not read at all; one that does
// not parse has no entries. The entries are those the attention still asks
// for (engine.FixNeedsAsked): one whose thread the operator resolved while
// the re-review ran is no longer named, and a count that does not match the
// report keeps magnum's words (no entries, the report still there). A
// control character the fixer copied from the PR's text is a space.
func TestPRStatesReadsTheFixersNeedsYouFromItsReport(t *testing.T) {
	l, st := magnumLayout(t)
	ctx := t.Context()
	token := "ghp_" + strings.Repeat("A", 36)
	needs := []string{`5485002407 description: change "fixes the cart total" to "rounds the cart total"`,
		"2101 P1 app/models/offer.rb:42: not fixed. which rounding does the cart want? token " + token}
	writeReport(t, reportOf(l, 9), reportText("r-9", needs...))
	fixAttention(t, st, 9, "fix", "fix: 2 findings need your call: "+reportOf(l, 9))

	writeReport(t, reportOf(l, 10), reportText("r-10", needs[0]))
	fixAttention(t, st, 10, "fix", "2 fix cycles, 2 findings left: "+reportOf(l, 10))

	outside := filepath.Join(t.TempDir(), "claude-fix.md")
	writeReport(t, outside, reportText("r-11", needs[0]))
	fixAttention(t, st, 11, "fix", "fix: 1 finding needs your call: "+outside)

	if err := os.MkdirAll(filepath.Dir(reportOf(l, 12)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, reportOf(l, 12)); err != nil {
		t.Fatal(err)
	}
	fixAttention(t, st, 12, "fix", "fix: 1 finding needs your call: "+reportOf(l, 12))

	fixAttention(t, st, 13, "fix", "fix: 1 finding needs your call: "+reportOf(l, 13)) // never written

	writeReport(t, reportOf(l, 14), reportText("r-14", needs[0]))
	fixAttention(t, st, 14, "blocked", "judge blocked: see "+reportOf(l, 14))

	writeReport(t, reportOf(l, 15), "the fixer ran out of time\n")
	fixAttention(t, st, 15, "fix", "fix: 1 finding needs your call: "+reportOf(l, 15))

	resolved := "2102 P2 app/b.rb:7: not fixed. keep the old rate?\x1b]52;c;cm0gLXJmIH4=\x07 or drop it"
	writeReport(t, reportOf(l, 16), reportText("r-16", needs[1], resolved))
	fixAttention(t, st, 16, "fix", "fix: 1 finding needs your call (2102 P2 app/b.rb:7): "+reportOf(l, 16))

	writeReport(t, reportOf(l, 17), reportText("r-17", needs...))
	fixAttention(t, st, 17, "fix", "fix: 1 finding needs your call: "+reportOf(l, 17))

	var refs []PRRef
	for n := 9; n <= 17; n++ {
		refs = append(refs, PRRef{"example/shop", n})
	}
	views, err := NewMagnum(l, selfConfig()).PRStates(ctx, refs)
	if err != nil {
		t.Fatal(err)
	}
	masked := []string{needs[0], strings.Replace(needs[1], token, "<redacted>", 1)}
	if v := views[refs[0]]; v.FixReport != reportOf(l, 9) || !slices.Equal(v.FixNeeds, masked) {
		t.Errorf("#9, the fixer left 2 items: report %q, needs %q, want %q and %q", v.FixReport, v.FixNeeds, reportOf(l, 9), masked)
	}
	if v := views[refs[1]]; v.FixReport != reportOf(l, 10) || v.FixNeeds != nil {
		t.Errorf("#10, a cap: report %q, needs %q, want the report alone", v.FixReport, v.FixNeeds)
	}
	for i, why := range map[int]string{2: "outside magnum's reviews", 3: "a link out of them", 4: "missing", 5: "another attention"} {
		if v := views[refs[i]]; v.FixReport != "" || v.FixNeeds != nil {
			t.Errorf("#%d, %s: report %q, needs %q, want none", refs[i].Number, why, v.FixReport, v.FixNeeds)
		}
	}
	if v := views[refs[6]]; v.FixReport != reportOf(l, 15) || v.FixNeeds != nil {
		t.Errorf("#15, a report that does not parse: report %q, needs %q, want the report alone", v.FixReport, v.FixNeeds)
	}

	if v := views[refs[7]]; v.FixReport != reportOf(l, 16) ||
		!slices.Equal(v.FixNeeds, []string{"2102 P2 app/b.rb:7: not fixed. keep the old rate? ]52;c;cm0gLXJmIH4=  or drop it"}) {
		t.Errorf("#16, 2101 resolved since: report %q, needs %q, want 2102 alone, its control characters spaces", v.FixReport, v.FixNeeds)
	}
	if v := views[refs[8]]; v.FixReport != reportOf(l, 17) || v.FixNeeds != nil {
		t.Errorf("#17, 1 of 2 counted: report %q, needs %q, want the report alone", v.FixReport, v.FixNeeds)
	}

	own, err := NewMagnum(l, selfConfig()).OwnPRs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(own, func(v PRState) bool { return v.Ref == refs[0] })
	if i < 0 {
		t.Fatalf("OwnPRs lists no #9: %d PRs", len(own))
	}
	if v := own[i]; v.FixReport != reportOf(l, 9) || !slices.Equal(v.FixNeeds, masked) {
		t.Errorf("your PR #9: report %q, needs %q, want its fixer's report and items", v.FixReport, v.FixNeeds)
	}
}
