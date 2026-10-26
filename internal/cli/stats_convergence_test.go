package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// Judge results the convergence tests post: a clean review, one with only
// P3s, a new P2, a new P1, an earlier P2 still open and an earlier P3 still
// open.
const (
	convClean   = `{"verdict":"clean","findings":{"P0":0,"P1":0,"P2":0,"P3":0}}`
	convP3Only  = `{"verdict":"non_blocking","findings":{"P3":2}}`
	convNewP2   = `{"verdict":"non_blocking","findings":{"P2":1}}`
	convNewP1   = `{"verdict":"blocking","findings":{"P1":1}}`
	convOpenP2  = `{"verdict":"non_blocking","findings":{},"previous_findings":{"fixed":1,"open":{"P0":0,"P1":0,"P2":1,"P3":0}}}`
	convOpenP3s = `{"verdict":"non_blocking","findings":{},"previous_findings":{"fixed":1,"open":{"P3":1}}}`
)

// statsConvFixture is a magnum home whose CLI clock is statsNow, with its
// registry open for seeding.
func statsConvFixture(t *testing.T) (*inspFixture, *store.Store) {
	t.Helper()
	f := newInspFixture(t)
	prev := inspNow
	inspNow = func() time.Time { return statsNow }
	t.Cleanup(func() { inspNow = prev })
	st := f.store()
	st.Clock = func() time.Time { return statsNow }
	return f, st
}

// statsConvPR seeds PR number of fullName by author in GitHub state ghState:
// merged (and closed, as GitHub closes a merged PR) or closed at ended.
func statsConvPR(t *testing.T, st *store.Store, fullName string, number int, author, ghState string, ended time.Time) store.PR {
	t.Helper()
	_, pr := inspSeedPR(t, st, fullName, number, store.PRReviewed, func(u *store.PRUpdate) {
		u.Set("author_login", author)
		u.Set("gh_state", ghState)
		switch ghState {
		case store.GHMerged:
			u.Set("merged_at", ended)
			u.Set("closed_at", ended)
		case store.GHClosed:
			u.Set("closed_at", ended)
		}
	})
	return pr
}

// statsConvReview seeds the judge run of pr's round that posted a review of
// head sha (GitHub's review number*100+round) at at, with result.
func statsConvReview(t *testing.T, st *store.Store, pr store.PR, round int, sha string, at time.Time, result string) store.Run {
	t.Helper()
	return statsConvRun(t, st, pr, round, sha, at, result, int64(pr.Number*100+round))
}

// statsConvRun seeds a judge run of pr's round on head sha at at, with
// result, that posted GitHub's review reviewID (0 = it posted none).
func statsConvRun(t *testing.T, st *store.Store, pr store.PR, round int, sha string, at time.Time, result string, reviewID int64) store.Run {
	t.Helper()
	kind := store.RunRereview
	if round == 1 {
		kind = store.RunInitial
	}
	run := store.Run{PRID: pr.ID, Round: round, Role: store.RoleJudge, Kind: kind, State: store.RunVerified, TargetSHA: sha,
		Identity: "talkable-app", ReviewerLogin: "talkable[bot]", PromptText: "x", CreatedAt: at, EndedAt: new(at.Add(10 * time.Minute)),
		VerifiedAt: new(at.Add(11 * time.Minute)), Outcome: new("posted"), ResultJSON: statsStrOpt(result)}
	if reviewID != 0 {
		run.ReviewID = &reviewID
	} else {
		run.State, run.Outcome = store.RunFailed, new("error")
	}
	r, err := st.CreateRun(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// statsConvChain seeds n reviews of pr, one head each (h1..hn), an hour
// apart from at: results(k) is the k-th's.
func statsConvChain(t *testing.T, st *store.Store, pr store.PR, n int, at time.Time, results func(k int) string) {
	t.Helper()
	for k := 1; k <= n; k++ {
		statsConvReview(t, st, pr, k, fmt.Sprintf("h%d", k), at.Add(time.Duration(k-1)*time.Hour), results(k))
	}
}

// A PR's convergence is the number of distinct heads magnum reviewed until a
// review left no P0, P1 or P2, new or still open, over the PRs that merged
// or closed in the window: a P3-only review passes and an earlier P2 still
// open does not; a second review of a head already counted (a reply round,
// a nudge of the same review) adds nothing; a round after the merge is
// dropped; a PR that ended before the window, or without a posted review,
// is not in it. The operator's own PRs have their own rows, a PR that
// merged while its last review left a P0-P2 is MERGED OPEN, and the open
// PRs that already have five reviewed heads are counted apart.
func TestStatsConvergenceCountsHeadsUntilNoSeriousFindingIsLeft(t *testing.T) {
	f, st := statsConvFixture(t)
	const tk, wd = "talkable/talkable", "example/widgets"
	merged := statsOct(3, 12, 0)
	day1 := statsOct(1, 10, 0)

	// Passes at head 1.
	statsConvReview(t, st, statsConvPR(t, st, tk, 201, "alice", store.GHMerged, merged), 1, "h1", day1, convClean)
	// A review with only P3s passes.
	statsConvReview(t, st, statsConvPR(t, st, tk, 202, "rev-ann", store.GHMerged, merged), 1, "h1", day1, convP3Only)
	// A new P2, then an earlier P2 still open, then only a P3 left: head 3.
	statsConvChain(t, st, statsConvPR(t, st, tk, 203, "alice", store.GHMerged, merged), 3, day1,
		func(k int) string { return []string{convNewP2, convOpenP2, convOpenP3s}[k-1] })
	// A P1 at h1, a clean reply round of h1 that adds nothing, a P1 at h2,
	// then h3 clean, whose nudge (the same review, a later run) counts once,
	// by its first run: head 3.
	pr204 := statsConvPR(t, st, tk, 204, "alice", store.GHMerged, merged)
	statsConvReview(t, st, pr204, 1, "h1", day1, convNewP1)
	statsConvReview(t, st, pr204, 2, "h1", day1.Add(time.Hour), convClean)
	statsConvReview(t, st, pr204, 3, "h2", day1.Add(2*time.Hour), convNewP1)
	statsConvReview(t, st, pr204, 4, "h3", day1.Add(3*time.Hour), convClean)
	statsConvRun(t, st, pr204, 4, "h3", day1.Add(4*time.Hour), convNewP1, 204*100+4)
	// A P2 at h1, merged, then a clean round after the merge: dropped, so it
	// never passed and merged with the P2 open.
	pr205 := statsConvPR(t, st, tk, 205, "bob-rev", store.GHMerged, statsOct(2, 12, 0))
	statsConvReview(t, st, pr205, 1, "h1", statsOct(2, 10, 0), convNewP2)
	statsConvReview(t, st, pr205, 2, "h2", statsOct(2, 13, 0), convClean)
	// The operator's own PR.
	statsConvReview(t, st, statsConvPR(t, st, tk, 206, "zhuravel", store.GHMerged, merged), 1, "h1", day1, convClean)
	// Closed after five heads with a P2 each: never, 5+, not merged.
	statsConvChain(t, st, statsConvPR(t, st, tk, 207, "alice", store.GHClosed, merged), 5, day1, func(int) string { return convNewP2 })
	// A P1 at four heads, clean at the fifth: 5+, passed at 5.
	statsConvChain(t, st, statsConvPR(t, st, tk, 208, "alice", store.GHMerged, merged), 5, day1, func(k int) string {
		if k == 5 {
			return convClean
		}
		return convNewP1
	})
	// Merged before the window.
	statsConvReview(t, st, statsConvPR(t, st, tk, 209, "alice", store.GHMerged, statsAt(time.September, 20, 12, 0)), 1, "h1",
		statsAt(time.September, 19, 12, 0), convNewP2)
	// Open: five heads (others) and two heads (own).
	statsConvChain(t, st, statsConvPR(t, st, tk, 210, "alice", store.GHOpen, time.Time{}), 5, day1, func(int) string { return convNewP2 })
	statsConvChain(t, st, statsConvPR(t, st, tk, 211, "zhuravel", store.GHOpen, time.Time{}), 2, day1, func(int) string { return convNewP2 })
	// Merged without a posted review: its judge failed.
	statsConvRun(t, st, statsConvPR(t, st, tk, 212, "alice", store.GHMerged, merged), 1, "h1", day1, "", 0)
	// Another repository: passes at head 2.
	statsConvChain(t, st, statsConvPR(t, st, wd, 7, "alice", store.GHMerged, merged), 2, day1,
		func(k int) string { return []string{convNewP2, convClean}[k-1] })
	st.Close()
	if err := f.Ctx.LoadConfig(); err != nil {
		t.Fatal(err)
	}

	since := statsNow.Add(-7 * 24 * time.Hour)
	r, err := statsGather(context.Background(), f.store(), f.Ctx.Config, since, statsNow, "")
	if err != nil {
		t.Fatal(err)
	}
	row := func(repo, authors string, prs, at1, by2, five, never int, mean float64, p90, open int) statsConvergenceRow {
		return statsConvergenceRow{Repo: repo, Authors: authors, PRs: prs, PassAt1: at1, PassBy2: by2, FivePlus: five, Never: never,
			MeanHeads: mean, P90Heads: p90, MergedOpen: open}
	}
	want := []statsConvergenceRow{
		row(wd, statsOthers, 1, 0, 1, 0, 0, 2, 2, 0),
		// 201 and 202 at 1, 203 and 204 at 3, 208 at 5; 205 and 207 never.
		row(tk, statsOthers, 7, 2, 2, 2, 2, 2.6, 5, 1),
		row(tk, statsOwn, 1, 1, 1, 0, 0, 1, 1, 0),
		row("", statsOthers, 8, 2, 3, 2, 2, 2.5, 5, 1),
		row("", statsOwn, 1, 1, 1, 0, 0, 1, 1, 0),
	}
	if got, w := statsJSON(t, r.Convergence.Rows), statsJSON(t, want); got != w {
		t.Fatalf("rows =\n%s\nwant\n%s", got, w)
	}
	if got := r.Convergence.OpenFivePlus; got != (statsOwnSplit{Others: 1, Own: 0}) {
		t.Errorf("open PRs with 5+ heads = %+v, want 1 of others'", got)
	}

	// --repo keeps one repository's rows and its total.
	one, err := statsGather(context.Background(), f.store(), f.Ctx.Config, since, statsNow, wd)
	if err != nil {
		t.Fatal(err)
	}
	if got, w := statsJSON(t, one.Convergence.Rows), statsJSON(t, []statsConvergenceRow{want[0], row("", statsOthers, 1, 0, 1, 0, 0, 2, 2, 0)}); got != w {
		t.Errorf("--repo rows =\n%s\nwant\n%s", got, w)
	}
	if one.Convergence.OpenFivePlus != (statsOwnSplit{}) {
		t.Errorf("--repo %s open PRs with 5+ heads = %+v", wd, one.Convergence.OpenFivePlus)
	}

	// The text report: a row per repository and authors, then the totals.
	if code := f.run("stats"); code != 0 {
		t.Fatalf("code %d, stderr %s", code, f.Err.String())
	}
	lines := statsLines(f.Out.String())
	wantLines := []string{
		"CONVERGENCE",
		"REPO AUTHORS PRS PASS@1 PASS<=2 5+ NEVER MEAN P90 MERGED OPEN",
		"example/widgets others 1 0 (0%) 1 (100%) 0 (0%) 0 (0%) 2.00 2 0",
		"talkable/talkable others 7 2 (29%) 2 (29%) 2 (29%) 2 (29%) 2.60 5 1",
		"talkable/talkable own 1 1 (100%) 1 (100%) 0 (0%) 0 (0%) 1.00 1 0",
		"all others 8 2 (25%) 3 (38%) 2 (25%) 2 (25%) 2.50 5 1",
		"all own 1 1 (100%) 1 (100%) 0 (0%) 0 (0%) 1.00 1 0",
		"open PRs with 5 or more reviewed heads: 1 others', 0 own",
	}
	at := 0
	for _, w := range wantLines {
		i := slices.Index(lines[at:], w)
		if i < 0 {
			t.Fatalf("missing line %q (after line %d) in\n%s", w, at, f.Out.String())
		}
		at += i + 1
	}

	// --json: a row per repository and authors, the totals without a repo.
	if code := f.run("stats", "--json"); code != 0 {
		t.Fatalf("code %d, stderr %s", code, f.Err.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(f.Out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	rows, _ := statsObj(t, doc["convergence"])["rows"].([]any)
	if len(rows) != len(want) {
		t.Fatalf("convergence rows = %v", rows)
	}
	keys := []string{"authors", "five_plus", "mean_heads", "merged_open", "never", "p90_heads", "pass_at_1", "pass_by_2", "prs", "repo"}
	if got := statsKeys(statsObj(t, rows[0])); !slices.Equal(got, keys) {
		t.Errorf("row keys = %v, want %v", got, keys)
	}
	if got := statsKeys(statsObj(t, rows[3])); !slices.Equal(got, slices.DeleteFunc(slices.Clone(keys), func(k string) bool { return k == "repo" })) {
		t.Errorf("total row keys = %v", got)
	}
}

// statsConvStart seeds the engine.round_start event of a round of pr (of
// fullName) on head sha at at, a full pass when full is set.
func statsConvStart(t *testing.T, st *store.Store, pr store.PR, fullName, sha string, at time.Time, full bool) {
	t.Helper()
	data := fmt.Sprintf(`{"kind":"rereview","target_sha":%q}`, sha)
	if full {
		data = fmt.Sprintf(`{"kind":"rereview","target_sha":%q,"full_pass":true,"full_pass_reason":"re-review 10"}`, sha)
	}
	statsEvent(t, st, at, fmt.Sprintf("pr:%s#%d", fullName, pr.Number), statsKindRoundStart, "re-review round", data)
}

// statsConvFindings records the findings of run at at: posted, title to
// priority, and rejected P1s by title.
func statsConvFindings(t *testing.T, st *store.Store, run store.Run, at time.Time, posted map[string]string, rejected ...string) {
	t.Helper()
	var fs []store.Finding
	for title, sev := range posted {
		fs = append(fs, store.Finding{FindingID: fmt.Sprintf("F%d", len(fs)+1), Title: title, Severity: sev, Sources: []string{"judge"},
			Verdict: store.FindingPosted, CreatedAt: at})
	}
	for _, title := range rejected {
		fs = append(fs, store.Finding{FindingID: fmt.Sprintf("F%d", len(fs)+1), Title: title, Severity: "P1", Sources: []string{"judge"},
			Verdict: store.FindingRejected, ReasonCode: "duplicate", CreatedAt: at})
	}
	if err := st.RecordFindings(context.Background(), run.ID, run.PRID, run.Round, fs); err != nil {
		t.Fatal(err)
	}
}

// The window's posted P0-P2 findings of re-reviews split by where they come
// from, by the judge's title suffixes (store.FindingOrigin): new, missed
// earlier and fallout of the fixes, with the rounds that were full passes
// (their engine.round_start event says so) counted apart. A first review's
// findings, P3s, rejected findings, a round's findings recorded again by an
// earlier run and findings before the window are not counted.
func TestStatsConvergenceSplitsReReviewFindingsByOrigin(t *testing.T) {
	f, st := statsConvFixture(t)
	const tk, wd = "talkable/talkable", "example/widgets"
	pr := statsConvPR(t, st, tk, 301, "alice", store.GHOpen, time.Time{})
	// Round 1, the first review: its suffix is no re-review's.
	statsConvStart(t, st, pr, tk, "h1", statsOct(2, 9, 59), false)
	r1 := statsConvReview(t, st, pr, 1, "h1", statsOct(2, 10, 0), convNewP2)
	statsConvFindings(t, st, r1, statsOct(2, 10, 11), map[string]string{"Thing (missed earlier)": "P2"})
	// Round 2, a re-review: one of each, a P3 and a rejected finding; an
	// earlier run of the round recorded findings the last one replaced.
	statsConvStart(t, st, pr, tk, "h2", statsOct(2, 11, 59), false)
	statsMkRun(t, st, statsRunSpec{pr: pr, round: 2, role: store.RoleClaude, kind: store.RunRereview, state: store.RunVerified, outcome: "ok",
		created: statsOct(2, 12, 0)})
	stale := statsConvRun(t, st, pr, 2, "h2", statsOct(2, 12, 10), "", 0)
	statsConvFindings(t, st, stale, statsOct(2, 12, 20), map[string]string{"Stale (missed earlier)": "P1"})
	r2 := statsConvReview(t, st, pr, 2, "h2", statsOct(2, 12, 30), convNewP1)
	statsConvFindings(t, st, r2, statsOct(2, 12, 41), map[string]string{
		"New problem":                                      "P1",
		"Old problem (missed earlier)":                     "P2",
		"Fix broke it (in the fix for Old problem":         "P2",
		"Nit (missed earlier) (optional, no reply needed)": "P3",
	}, "Rejected (missed earlier)")
	// Round 3, a full pass.
	statsConvStart(t, st, pr, tk, "h3", statsOct(2, 13, 59), true)
	statsMkRun(t, st, statsRunSpec{pr: pr, round: 3, role: store.RoleClaude, state: store.RunVerified, outcome: "ok", created: statsOct(2, 14, 0)})
	r3 := statsConvReview(t, st, pr, 3, "h3", statsOct(2, 15, 0), convNewP1)
	statsConvFindings(t, st, r3, statsOct(2, 15, 11), map[string]string{"Deep one (missed earlier)": "P1", "Another": "P2"})
	// Round 4, a reply round of the same head after the full pass.
	statsConvStart(t, st, pr, tk, "h3", statsOct(2, 16, 59), false)
	r4 := statsConvReview(t, st, pr, 4, "h3", statsOct(2, 17, 0), convNewP2)
	statsConvFindings(t, st, r4, statsOct(2, 17, 11), map[string]string{"Later (in the fix for Deep one)": "P2"})
	// A re-review before the window.
	old := statsConvPR(t, st, tk, 302, "alice", store.GHOpen, time.Time{})
	statsConvReview(t, st, old, 1, "h1", statsAt(time.September, 20, 10, 0), convNewP2)
	o2 := statsConvReview(t, st, old, 2, "h2", statsAt(time.September, 21, 10, 0), convNewP1)
	statsConvFindings(t, st, o2, statsAt(time.September, 21, 10, 11), map[string]string{"Old (missed earlier)": "P1"})
	// Another repository's re-review.
	w := statsConvPR(t, st, wd, 8, "alice", store.GHOpen, time.Time{})
	statsConvReview(t, st, w, 1, "h1", statsOct(2, 10, 0), convNewP2)
	w2 := statsConvReview(t, st, w, 2, "h2", statsOct(2, 12, 0), convNewP2)
	statsConvFindings(t, st, w2, statsOct(2, 12, 11), map[string]string{"W (missed earlier)": "P2"})
	st.Close()
	if err := f.Ctx.LoadConfig(); err != nil {
		t.Fatal(err)
	}

	since := statsNow.Add(-7 * 24 * time.Hour)
	for _, tc := range []struct {
		repo           string
		rereview, full statsOrigins
	}{
		{"", statsOrigins{New: 1, MissedEarlier: 2, Fallout: 2, MissedShare: 0.4}, statsOrigins{New: 1, MissedEarlier: 1, MissedShare: 0.5}},
		{tk, statsOrigins{New: 1, MissedEarlier: 1, Fallout: 2, MissedShare: 0.25}, statsOrigins{New: 1, MissedEarlier: 1, MissedShare: 0.5}},
	} {
		r, err := statsGather(context.Background(), f.store(), f.Ctx.Config, since, statsNow, tc.repo)
		if err != nil {
			t.Fatal(err)
		}
		if got := r.Convergence.Rereviews; got != tc.rereview {
			t.Errorf("repo %q: re-review findings = %+v, want %+v", tc.repo, got, tc.rereview)
		}
		if got := r.Convergence.FullPasses; got != tc.full {
			t.Errorf("repo %q: full pass findings = %+v, want %+v", tc.repo, got, tc.full)
		}
	}

	if code := f.run("stats", "--repo", tk); code != 0 {
		t.Fatalf("code %d, stderr %s", code, f.Err.String())
	}
	lines := statsLines(f.Out.String())
	for _, w := range []string{
		"re-review P0-P2 posted: 4: 1 new, 1 missed earlier (25%), 2 fallout of fixes",
		"full pass P0-P2 posted: 2: 1 new, 1 missed earlier (50%), 0 fallout of fixes",
	} {
		if !slices.Contains(lines, w) {
			t.Errorf("missing line %q in\n%s", w, f.Out.String())
		}
	}
}
