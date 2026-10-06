package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// seedAutoApprovals seeds #11970 (an automatic approval standing, posted
// at at; GitHub's gate still asked for an approval when last read), #11971
// (one magnum withdrew at at), #11972 (stopped: the operator commented by
// hand) and #11973 (none).
func seedAutoApprovals(t *testing.T, st *store.Store, at time.Time) {
	t.Helper()
	ctx := context.Background()
	_, standing := inspSeedPR(t, st, "talkable/talkable", 11970, store.PRReviewed, approvedBy("APPROVED", nmRequired))
	_, withdrawn := inspSeedPR(t, st, "talkable/talkable", 11971, store.PRReviewed, approvedBy("APPROVED", nil))
	_, stopped := inspSeedPR(t, st, "talkable/talkable", 11972, store.PRReviewed, approvedBy("APPROVED", nil))
	inspSeedPR(t, st, "talkable/talkable", 11973, store.PRReviewed, approvedBy("APPROVED", nil))
	for i, pr := range []store.PR{standing, withdrawn} {
		a, err := st.InsertAutoApproval(ctx, store.AutoApproval{PRID: pr.ID, RunID: "r", HeadSHA: nmHead, Identity: "zhuravel", Login: "zhuravel"})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.TransitionAutoApproval(ctx, a.ID, []string{store.AutoPosting}, store.AutoStanding, func(u *store.AutoApprovalUpdate) {
			u.Set("review_id", int64(9001+i))
			u.Set("review_url", "https://github.com/talkable/talkable/pull/1#pullrequestreview-9001")
			u.Set("posted_at", at)
		}); err != nil {
			t.Fatal(err)
		}
		if pr.ID == withdrawn.ID {
			if err := st.TransitionAutoApproval(ctx, a.ID, nil, store.AutoDismissed, func(u *store.AutoApprovalUpdate) {
				u.Set("ended_by", store.AutoEndedMagnum)
				u.Set("ended_at", at)
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := st.SetAutoApproveHold(ctx, store.AutoApproveHold{PRID: stopped.ID, Held: true, Reason: "you commented on it by hand", At: at}); err != nil {
		t.Fatal(err)
	}
}

// `magnum prs --json` names the automatic approval standing on a PR (its
// review and head) and why magnum stopped approving one as the operator; a
// PR approved as the operator no longer needs them; --auto-approved lists
// only those PRs, and the printed rows flag them.
func TestPRsShowTheAutoApprovals(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	seedAutoApprovals(t, st, time.Now())
	st.Close()

	if code := f.run("prs", "--json"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	var rows []prsJSONRow
	if err := json.Unmarshal(f.Out.Bytes(), &rows); err != nil {
		t.Fatalf("%v\n%s", err, f.Out.String())
	}
	by := map[int]prsJSONRow{}
	for _, r := range rows {
		by[r.Number] = r
	}
	if a := by[11970].AutoApproved; a == nil || a.ReviewID != 9001 || a.Head != nmHead || by[11970].NeedsMe != "" {
		t.Fatalf("#11970 auto_approved %+v, needs_me %q", a, by[11970].NeedsMe)
	}
	for _, n := range []int{11971, 11972, 11973} {
		if a := by[n].AutoApproved; a != nil {
			t.Errorf("#%d auto_approved %+v", n, a)
		}
	}
	if s := by[11972].AutoApproveStopped; s != "you commented on it by hand" || by[11970].AutoApproveStopped != "" {
		t.Errorf("stopped: %q / %q", s, by[11970].AutoApproveStopped)
	}
	if !strings.Contains(f.Out.String(), `"auto_approved": null`) {
		t.Error("a PR without one has no auto_approved: null")
	}

	if code := f.run("prs", "--auto-approved", "--json"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	rows = nil
	if err := json.Unmarshal(f.Out.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	var nums []int
	for _, r := range rows {
		nums = append(nums, r.Number)
	}
	if !slices.Equal(nums, []int{11970}) {
		t.Fatalf("--auto-approved rows %v", nums)
	}
	if code := f.run("prs", "--auto-approved"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	if out := f.Out.String(); !strings.Contains(out, "reviewed,auto-approved") || strings.Contains(out, "needs-you") {
		t.Errorf("printed rows:\n%s", out)
	}
}

// The board's rows carry the standing approval and the stop, and the
// titles count the PRs approved as the operator.
func TestTheScreensCountTheAutoApprovals(t *testing.T) {
	_, st, d, now := statusFixture(t)
	seedAutoApprovals(t, st, now)
	ctx := context.Background()
	if f := screenFacts(ctx, d); f.AutoApproved != 1 || f.NeedsMe != 0 {
		t.Fatalf("facts AutoApproved = %d, NeedsMe = %d", f.AutoApproved, f.NeedsMe)
	}
	rows, err := prsSource(st, d.Config, store.BoardFilter{}, prsSelfLogins(d.Config), d.Layout)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]tui.PRBoardRow{}
	for _, r := range rows {
		got[r.Number] = r
	}
	if a := got[11970].AutoApproved; a == nil || a.ReviewID != 9001 || a.Head != nmHead || !a.At.Equal(now) || got[11970].NeedsMe != "" {
		t.Fatalf("#11970: %+v, needs me %q", a, got[11970].NeedsMe)
	}
	if got[11971].AutoApproved != nil || got[11972].AutoStopped != "you commented on it by hand" {
		t.Fatalf("#11971 %+v, #11972 stopped %q", got[11971].AutoApproved, got[11972].AutoStopped)
	}
}

// `magnum status` says how many PRs magnum approved as the operator today
// and how many of those approvals stand.
func TestStatusCountsTheAutoApprovalsOfTheDay(t *testing.T) {
	_, st, d, now := statusFixture(t)
	seedAutoApprovals(t, st, now)
	r, err := statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a := r.AutoApproved; a == nil || a.Today != 2 || a.Standing != 1 {
		t.Fatalf("auto_approved = %+v", a)
	}
	var b bytes.Buffer
	statusRender(&b, r)
	if !strings.Contains(b.String(), "approvals: auto-approved: 2 today, 1 standing") {
		t.Fatalf("status:\n%s", b.String())
	}
	_, _, d, _ = statusFixture(t) // none, and no watch auto-approves: no line
	r, _ = statusGather(context.Background(), d, statusOptions{})
	b.Reset()
	statusRender(&b, r)
	if r.AutoApproved != nil || strings.Contains(b.String(), "approvals:") {
		t.Fatalf("a status without automatic approvals:\n%s", b.String())
	}
}

// The picker marks a PR approved as the operator "✔ auto", not as one that
// needs them.
func TestPickMarksTheAutoApprovedPRs(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.setPR(pr.ID, store.PRReviewed, func(u *store.PRUpdate) {
		u.Set("reviewed_sha", pr.HeadSHA)
		u.Set("last_review_event", "APPROVED")
		u.Set("review_gate_json", nmRequired)
	})
	h.standingAutoApproval(pr)
	calls := h.withPicker(func([]tui.PickEntry) tui.PickOutcome { return tui.PickOutcome{} })
	if code := h.cmd("pick"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	found := false
	for _, e := range (*calls)[0].entries {
		if e.Ref == "talkable/talkable#5" {
			found = true
			if e.State != "reviewed,✔ auto" {
				t.Fatalf("state %q", e.State)
			}
		}
	}
	if !found {
		t.Fatal("#5 is not listed")
	}
}
