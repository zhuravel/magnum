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
