package cli

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

const nmHead = "abcdef0123456789abcdef0123456789abcdef01" // inspSeedPR's head

var (
	nmRequired = &store.ReviewGate{Decision: "REVIEW_REQUIRED", Opinions: []store.LatestReview{}, Complete: true}
	nmMine     = &store.ReviewGate{Decision: "CHANGES_REQUESTED", Complete: true,
		Opinions: []store.LatestReview{{Login: "zhuravel", State: "CHANGES_REQUESTED", CommitSHA: "0ld"}}}
	nmOthers = &store.ReviewGate{Decision: "CHANGES_REQUESTED", Complete: true,
		Opinions: []store.LatestReview{{Login: "bob-rev", State: "CHANGES_REQUESTED", CommitSHA: nmHead}}}
)

// approvedBy sets what magnum's latest review of the head was and what
// GitHub's merge gate says.
func approvedBy(event string, gate *store.ReviewGate) func(*store.PRUpdate) {
	return func(u *store.PRUpdate) {
		u.Set("reviewed_sha", nmHead)
		u.Set("last_review_event", event)
		u.Set("review_gate_json", gate)
	}
}

// seedNeedsMe seeds #11960 (magnum approved, a review that counts is
// required), #11961 (approved, only zhuravel's changes request blocks it),
// #11962 (approved, someone else's changes request), #11963 (the App's
// clean comment: talkable-app comments when clean) and #11964 (queued, not
// reviewed).
func seedNeedsMe(t *testing.T, st *store.Store) {
	t.Helper()
	inspSeedPR(t, st, "talkable/talkable", 11960, store.PRReviewed, approvedBy("APPROVED", nmRequired))
	inspSeedPR(t, st, "talkable/talkable", 11961, store.PRReviewed, approvedBy("APPROVED", nmMine))
	inspSeedPR(t, st, "talkable/talkable", 11962, store.PRReviewed, approvedBy("APPROVED", nmOthers))
	_, clean := inspSeedPR(t, st, "talkable/talkable", 11963, store.PRReviewed, approvedBy("COMMENTED", nmRequired))
	inspSeedPR(t, st, "talkable/talkable", 11964, store.PRQueued, func(u *store.PRUpdate) { u.Set("review_gate_json", nmRequired) })
	if _, err := st.DB().ExecContext(context.Background(), `INSERT INTO runs (id, pr_id, round, role, kind, target_sha, identity, reviewer_login, state, outcome, review_id, prompt_text, result_json, created_at)
		VALUES ('r-clean', ?, 1, 'codex-judge', 'initial', ?, 'talkable-app', 'talkable[bot]', 'verified', 'posted', 7, 'p', '{"event":"COMMENT","verdict":"clean","findings":{}}', ?)`,
		clean.ID, nmHead, store.FormatTime(clean.CreatedAt)); err != nil {
		t.Fatal(err)
	}
}

// `magnum prs --json` says which PRs magnum approved still need the
// operator (needs_me: approve, lift or "") and GitHub's review decision;
// --needs-me lists only them, first in the updated sort, and the printed
// rows flag them.
func TestPRsNeedsMeListsThePRsWaitingForYourApproval(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	seedNeedsMe(t, st)
	st.Close()

	if code := f.run("prs", "--json"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	var raw []map[string]any
	if err := json.Unmarshal(f.Out.Bytes(), &raw); err != nil {
		t.Fatalf("%v\n%s", err, f.Out.String())
	}
	got := map[float64][2]any{}
	for _, r := range raw {
		got[r["number"].(float64)] = [2]any{r["needs_me"], r["review_decision"]}
	}
	want := map[float64][2]any{
		11960: {"approve", "REVIEW_REQUIRED"}, 11961: {"lift", "CHANGES_REQUESTED"}, 11962: {"", "CHANGES_REQUESTED"},
		11963: {"approve", "REVIEW_REQUIRED"}, 11964: {"", "REVIEW_REQUIRED"},
	}
	for n, w := range want {
		if got[n] != w {
			t.Errorf("#%v: needs_me, review_decision = %v, want %v", n, got[n], w)
		}
	}

	if code := f.run("prs", "--needs-me", "--json"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	var rows []prsJSONRow
	if err := json.Unmarshal(f.Out.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	var nums []int
	for _, r := range rows {
		nums = append(nums, r.Number)
	}
	slices.Sort(nums)
	if !slices.Equal(nums, []int{11960, 11961, 11963}) {
		t.Fatalf("--needs-me rows %v", nums)
	}

	if code := f.run("prs", "--needs-me"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	out := f.Out.String()
	for _, want := range []string{"reviewed,needs-you", "reviewed,lift-yours"} {
		if !strings.Contains(out, want) {
			t.Errorf("printed rows lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "11962") || strings.Contains(out, "11964") {
		t.Errorf("--needs-me printed a PR that does not need you:\n%s", out)
	}

	if code := f.run("prs"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	lines := strings.Split(strings.TrimSpace(f.Out.String()), "\n")[1:]
	var first []string
	for _, l := range lines[:3] {
		first = append(first, strings.Fields(l)[0])
	}
	slices.Sort(first)
	if !slices.Equal(first, []string{"talkable#11960", "talkable#11961", "talkable#11963"}) {
		t.Errorf("the PRs that need you come first in the updated sort:\n%s", f.Out.String())
	}
}

// The board's rows carry NeedsMe and the titles count them.
func TestTheScreensCountThePRsThatNeedYou(t *testing.T) {
	_, st, d, _ := statusFixture(t)
	seedNeedsMe(t, st)
	ctx := context.Background()
	if f := screenFacts(ctx, d); f.NeedsMe != 3 {
		t.Fatalf("facts NeedsMe = %d, want 3", f.NeedsMe)
	}
	rows, err := prsSource(st, d.Config, store.BoardFilter{}, prsSelfLogins(d.Config), d.Layout)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[int]string{}
	for _, r := range rows {
		kinds[r.Number] = r.NeedsMe
	}
	if kinds[11960] != tui.NeedsMeApprove || kinds[11961] != tui.NeedsMeLift || kinds[11962] != "" || kinds[11963] != tui.NeedsMeApprove || kinds[11920] != "" {
		t.Fatalf("board rows NeedsMe %v", kinds)
	}
}

// [board] shimmer = false keeps the board's needs-you cells still; without
// a config, and by default, they shimmer.
func TestBoardShimmerFollowsTheConfig(t *testing.T) {
	_, _, d, _ := statusFixture(t)
	if prsBoardOptions(nil, prsOptions{}).NoShimmer || prsBoardOptions(d.Config, prsOptions{}).NoShimmer {
		t.Fatal("the shimmer is off by default")
	}
	d.Config.Board.Shimmer = false
	if !prsBoardOptions(d.Config, prsOptions{}).NoShimmer {
		t.Fatal("[board] shimmer = false left it on")
	}
}

// The picker marks the PRs that need you in their state.
func TestPickMarksThePRsThatNeedYou(t *testing.T) {
	h := newActHarness(t)
	approve := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	lift := h.seedPR("talkable/talkable", 6, store.PRReviewed)
	h.seedPR("talkable/talkable", 7, store.PRQueued)
	for pr, gate := range map[*store.PR]*store.ReviewGate{&approve: nmRequired, &lift: nmMine} {
		h.setPR(pr.ID, store.PRReviewed, func(u *store.PRUpdate) {
			u.Set("reviewed_sha", pr.HeadSHA)
			u.Set("last_review_event", "APPROVED")
			u.Set("review_gate_json", gate)
		})
	}
	calls := h.withPicker(func([]tui.PickEntry) tui.PickOutcome { return tui.PickOutcome{} })
	if code := h.cmd("pick"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	states := map[string]string{}
	for _, e := range (*calls)[0].entries {
		states[e.Ref] = e.State
	}
	want := map[string]string{"talkable/talkable#5": "reviewed,✓ needs you", "talkable/talkable#6": "reviewed,✓ lift your ✗",
		"talkable/talkable#7": "queued"}
	for ref, w := range want {
		if states[ref] != w {
			t.Errorf("%s state %q, want %q", ref, states[ref], w)
		}
	}
}
