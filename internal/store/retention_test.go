package store

import (
	"context"
	"testing"
	"time"
)

const day = 24 * time.Hour

func TestPruneEventsAndRequests(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	event := func(ageDays int, subject, kind, step, phase string) int64 {
		t.Helper()
		e := Event{At: t0.Add(-time.Duration(ageDays) * day), Level: "info", Subject: Ptr(subject), Kind: kind, Message: kind + "/" + step}
		if step != "" {
			e.Step, e.Phase = Ptr(step), Ptr(phase)
		}
		id, err := st.AppendEvent(ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	oldPlain := event(40, "pr:1", "poll", "", "")
	recentPlain := event(10, "pr:1", "poll", "", "")
	// slot:a had two step generations; only the current one survives.
	oldReset := event(50, "slot:a", KindStepReset, "", "")
	oldStep := event(49, "slot:a", KindStep, "provision", PhaseOK)
	curReset := event(45, "slot:a", KindStepReset, "", "")
	curStep := event(44, "slot:a", KindStep, "provision", PhaseOK)
	oldNote := event(44, "slot:a", "slot.note", "", "")
	// slot:b never reset: all its step rows are in the current generation.
	noResetStep := event(60, "slot:b", KindStep, "provision", PhaseOK)
	// An old step row of another subject must not be saved by slot:a's reset.
	otherOld := event(41, "slot:c", "poll", "", "")

	// Requests: created 10 days ago (r1 pending, r2 done, r3 failed) and an hour ago (r4 done, r5 failed).
	clk.Set(t0.Add(-10 * day))
	r1, _ := st.EnqueueRequest(ctx, "kick", nil)
	r2, _ := st.EnqueueRequest(ctx, "kick", nil)
	r3, _ := st.EnqueueRequest(ctx, "kick", nil)
	clk.Set(t0.Add(-time.Hour))
	r4, _ := st.EnqueueRequest(ctx, "kick", nil)
	r5, _ := st.EnqueueRequest(ctx, "kick", nil)
	for id, state := range map[int64]string{r2: RequestDone, r3: RequestFailed, r4: RequestDone, r5: RequestFailed} {
		if err := st.CompleteRequest(ctx, id, state, "r"); err != nil {
			t.Fatal(err)
		}
	}
	clk.Set(t0)

	// Both knobs off: nothing happens.
	if res, err := st.Prune(ctx, 0, 0); err != nil || res != (PruneResult{}) {
		t.Fatalf("Prune(0, 0) = %+v, %v", res, err)
	}

	res, err := st.Prune(ctx, 30*day, 7*day)
	if err != nil {
		t.Fatal(err)
	}
	if want := (PruneResult{Events: 5, Requests: 2}); res != want {
		t.Fatalf("Prune = %+v, want %+v", res, want)
	}

	var left []int64
	rows, err := st.DB().QueryContext(ctx, "SELECT id FROM events ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		left = append(left, id)
	}
	rows.Close()
	gone := map[int64]bool{oldPlain: true, oldReset: true, oldStep: true, oldNote: true, otherOld: true}
	kept := []int64{recentPlain, curReset, curStep, noResetStep}
	for _, id := range left {
		if gone[id] {
			t.Errorf("event %d should have been pruned", id)
		}
	}
	for _, id := range kept {
		found := false
		for _, l := range left {
			found = found || l == id
		}
		if !found {
			t.Errorf("event %d should have been kept", id)
		}
	}
	if len(left) != len(kept) {
		t.Errorf("%d events left, want %d: %v", len(left), len(kept), left)
	}

	// Resuming a sequence still sees its current generation's ok rows.
	for _, c := range []struct{ subject, step string }{{"slot:a", "provision"}, {"slot:b", "provision"}} {
		if done, err := st.StepDone(ctx, c.subject, c.step); err != nil || !done {
			t.Errorf("StepDone(%s, %s) = %v, %v after Prune; want true", c.subject, c.step, done, err)
		}
	}

	for id, wantGone := range map[int64]bool{r1: false, r2: true, r3: true, r4: false, r5: false} {
		_, err := st.RequestByID(ctx, id)
		if wantGone != (err != nil) {
			t.Errorf("request %d: err = %v, want gone = %v", id, err, wantGone)
		}
	}

	// A second pass finds nothing more.
	if res, err := st.Prune(ctx, 30*day, 7*day); err != nil || res != (PruneResult{}) {
		t.Fatalf("second Prune = %+v, %v", res, err)
	}
	// Later, the pending request is still never pruned and the rest ages out.
	clk.Set(t0.Add(100 * day))
	res, err = st.Prune(ctx, 30*day, 7*day)
	if err != nil {
		t.Fatal(err)
	}
	if res.Requests != 2 { // r4, r5
		t.Fatalf("Prune after 100 days = %+v", res)
	}
	if got, err := st.RequestByID(ctx, r1); err != nil || got.State != RequestPending {
		t.Fatalf("pending request must survive Prune: %+v, %v", got, err)
	}
}
