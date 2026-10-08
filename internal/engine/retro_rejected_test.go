package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/store"
)

// TestRetroStoresTheRejectedFindingTheClassifierNamed: the classifier gets
// the PR's rejected findings in the candidates file and its job; a miss on
// the code whose test gap the judge raised on the spec file and rejected
// as speculative is stored as raised by that finding when the classifier
// names it, not by the pre-existing one 2 lines from the comment; a
// candidate whose item names none keeps the finding near it.
func TestRetroStoresTheRejectedFindingTheClassifierNamed(t *testing.T) {
	fc := &fakeClassifier{}
	h := newHarness(t, withClassifier(fc))
	pr := retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	if err := h.st.RecordFindings(h.ctx, "r-7", pr.ID, 1, []store.Finding{
		{FindingID: "F1", Severity: "P2", Path: "app/models/coupon.rb", Line: 81, Verdict: store.FindingPosted, Title: "Coupon totals round twice"},
		{FindingID: "F2", Severity: "P3", Path: "spec/models/coupon_spec.rb", Line: 30, Verdict: store.FindingRejected, ReasonCode: "speculative",
			Title: "No example applies a coupon of another site"},
		{FindingID: "F3", Severity: "P3", Path: "app/models/coupon.rb", Line: 44, Verdict: store.FindingRejected, ReasonCode: "pre_existing",
			Title: "Coupon codes compare case-sensitively"},
		{FindingID: "F4", Severity: "P2", Path: "app/models/same.rb", Line: 10, Verdict: store.FindingRejected, ReasonCode: "not_reproducible",
			Title: "Cache key collides across sites"},
	}); err != nil {
		t.Fatal(err)
	}
	base := fc.answer
	fc.answer = func(job ClassifyJob) (ClassifyResult, error) {
		res, err := (&fakeClassifier{answer: base}).Classify(context.Background(), job)
		if err != nil {
			return res, err
		}
		b, _ := os.ReadFile(job.OutputPath)
		var out learn.Output
		_ = json.Unmarshal(b, &out)
		for i := range out.Items {
			if out.Items[i].ID == "t101" {
				out.Items[i].Rejected = "r-7/F2"
			}
		}
		b, _ = json.Marshal(out)
		return ClassifyResult{}, os.WriteFile(job.OutputPath, b, 0o600)
	}
	h.requestRetro(RetroPayload{})

	if rp, _ := h.retroRecord(pr.ID); rp.Status != store.RetroClassified {
		t.Fatalf("retro_prs = %+v", rp)
	}
	ms := h.misses(pr.ID)
	if m := ms["101"]; m.Class != store.MissMiss || m.Raised != store.MissRaisedRejected || m.FindingRef != "r-7/F2" || m.ReasonCode != "speculative" {
		t.Fatalf("t101 = %+v, want raised by r-7/F2 (speculative)", m)
	}
	if m := ms["103"]; m.Raised != store.MissRaisedRejected || m.FindingRef != "r-7/F4" || m.ReasonCode != "not_reproducible" {
		t.Fatalf("t103 = %+v, want the finding near it", m)
	}
	if m := ms["201"]; m.Raised != store.MissRaisedNone || m.FindingRef != "" {
		t.Fatalf("r201 = %+v", m)
	}

	want := []learn.Rejected{
		{ID: "r-7/F2", Title: "No example applies a coupon of another site", Path: "spec/models/coupon_spec.rb", Reason: "speculative", Priority: "P3"},
		{ID: "r-7/F3", Title: "Coupon codes compare case-sensitively", Path: "app/models/coupon.rb", Reason: "pre_existing", Priority: "P3"},
		{ID: "r-7/F4", Title: "Cache key collides across sites", Path: "app/models/same.rb", Reason: "not_reproducible", Priority: "P2"},
	}
	if len(fc.jobs) != 1 || !slices.Equal(fc.jobs[0].Rejected, want) {
		t.Fatalf("jobs = %d, rejected %+v", len(fc.jobs), fc.jobs)
	}
	cands, err := readCandidates(filepath.Join(fc.jobs[0].Dir, learn.CandidatesFile))
	if err != nil || !slices.Equal(cands.Rejected, want) {
		t.Fatalf("candidates file rejected = %+v, %v", cands.Rejected, err)
	}
}
