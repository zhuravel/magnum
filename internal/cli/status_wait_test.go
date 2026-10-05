package cli

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

// The daemon's wait reason replaces the generic "eligible in ..." for a
// waiting PR: compact in the queue (and the dashboard's NEXT column), the
// sentence with the override hint on `magnum status <ref>`.
func TestStatusShowsWhyAPRWaits(t *testing.T) {
	_, st, d, now := statusFixture(t)
	ctx := context.Background()
	pr, err := st.PRByRepoNumber(ctx, mustRepoID(t, st, "talkable/talkable"), 11930)
	if err != nil {
		t.Fatal(err)
	}
	midnight := time.Date(2026, 10, 4, 0, 0, 0, 0, time.Local)
	w := engine.Wait{Reason: engine.WaitCap, Rereview: true, Until: midnight, Count: 6, Max: 6, Detail: "the daily round cap (6 of 6 today)"}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(ctx, engine.KVPRWait(pr.ID), string(b)); err != nil {
		t.Fatal(err)
	}

	r, err := statusGather(ctx, d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var next string
	for _, q := range r.Queue {
		if q.Number == 11930 {
			next = q.Next
		}
	}
	if want := w.Short(now); next != want || next == "" {
		t.Fatalf("queue next = %q, want %q", next, want)
	}

	r, err = statusGather(ctx, d, statusOptions{Ref: "11930"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Detail == nil || r.Detail.Next != w.Sentence("talkable#11930", now) {
		t.Fatalf("detail next = %q, want %q", r.Detail.Next, w.Sentence("talkable#11930", now))
	}

	// Another state ignores a leftover wait.
	if err := st.TransitionPR(ctx, pr.ID, []string{store.PRQueued}, store.PRReviewed, nil); err != nil {
		t.Fatal(err)
	}
	if r, err = statusGather(ctx, d, statusOptions{Ref: "11930"}); err != nil || r.Detail.Next != "watching for new pushes" {
		t.Fatalf("reviewed PR next = %q (%v)", r.Detail.Next, err)
	}
}

func mustRepoID(t *testing.T, st *store.Store, full string) int64 {
	t.Helper()
	repo, err := st.RepoByFullName(context.Background(), full)
	if err != nil {
		t.Fatal(err)
	}
	return repo.ID
}

// A merged PR waiting for its post-merge round says so in the queue, on
// `magnum status <ref>` (the daemon's wait, else the state's own words) and
// while its round runs.
func TestStatusDescribesAPostMergeReview(t *testing.T) {
	_, st, d, _ := statusFixture(t)
	ctx := context.Background()
	pr, err := st.PRByRepoNumber(ctx, mustRepoID(t, st, "talkable/talkable"), 11930)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) { u.Set("gh_state", store.GHMerged); u.Set("forced", true) }); err != nil {
		t.Fatal(err)
	}
	if r, err := statusGather(ctx, d, statusOptions{Ref: "11930"}); err != nil || r.Detail.Next != "post-merge review, forced, waiting for a slot" {
		t.Fatalf("detail next without a wait = %q (%v)", r.Detail.Next, err)
	}

	w := engine.Wait{Reason: engine.WaitNext, Forced: true, PostMerge: true, Detail: "the next dispatch"}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(ctx, engine.KVPRWait(pr.ID), string(b)); err != nil {
		t.Fatal(err)
	}
	r, err := statusGather(ctx, d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var next string
	for _, q := range r.Queue {
		if q.Number == 11930 {
			next = q.Next
		}
	}
	if next != "post-merge review · next tick" {
		t.Fatalf("queue next = %q", next)
	}
	r, err = statusGather(ctx, d, statusOptions{Ref: "11930"})
	if err != nil || r.Detail.Next != "forced post-merge review starts at the next dispatch" {
		t.Fatalf("detail next = %q (%v)", r.Detail.Next, err)
	}
	if err := st.TransitionPR(ctx, pr.ID, []string{store.PRQueued}, store.PRReviewing, nil); err != nil {
		t.Fatal(err)
	}
	if r, err = statusGather(ctx, d, statusOptions{Ref: "11930"}); err != nil || r.Detail.Next != "post-merge round in progress" {
		t.Fatalf("running round next = %q (%v)", r.Detail.Next, err)
	}
}
