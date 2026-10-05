package store

import (
	"context"
	"slices"
	"testing"
	"time"
)

// stepEvent appends an event of a subject aged ago before t0 (a step row when
// step is set) and returns its id.
func stepEvent(t *testing.T, st *Store, ago time.Duration, subject, kind, step, phase string) int64 {
	t.Helper()
	e := Event{At: t0.Add(-ago), Level: "info", Subject: Ptr(subject), Kind: kind, Message: kind + "/" + step}
	if step != "" {
		e.Step, e.Phase = Ptr(step), Ptr(phase)
	}
	id, err := st.AppendEvent(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func eventIDs(t *testing.T, st *Store) []int64 {
	t.Helper()
	rows, err := st.DB().QueryContext(context.Background(), "SELECT id FROM events ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// keep_events promises that events older than the window are pruned. The step
// rows of a subject's current generation were exempt for good, and subjects
// carry the head's sha, so a checkout's rows (about 350 a day) were never
// pruned. A subject with no event inside the window is not mid-sequence: its
// step rows go with the rest of its history.
func TestPruneDropsTheStepRowsOfASubjectSilentForTheWindow(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()

	// slot:review3:pr:1:abc1234 finished 45 days ago and has been silent since.
	silent := []int64{
		stepEvent(t, st, 45*day, "slot:review3:pr:1:abc1234", KindStepReset, "", ""),
		stepEvent(t, st, 45*day, "slot:review3:pr:1:abc1234", KindStep, "worktree_add", PhaseBegin),
		stepEvent(t, st, 45*day, "slot:review3:pr:1:abc1234", KindStep, "worktree_add", PhaseOK),
	}
	if done, err := st.StepDone(ctx, "slot:review3:pr:1:abc1234", "worktree_add"); err != nil || !done {
		t.Fatalf("before Prune: StepDone = %v, %v", done, err)
	}

	res, err := st.Prune(ctx, 30*day, 7*day)
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != int64(len(silent)) {
		t.Fatalf("Prune = %+v, want the %d step rows of the silent subject gone", res, len(silent))
	}
	if left := eventIDs(t, st); len(left) != 0 {
		t.Fatalf("events left: %v", left)
	}
}

// A subject that had any event inside the window is in use: its current
// generation is what a resumed sequence reads, so it stays, whatever the age
// of its rows; its superseded generations are history and go.
func TestPruneKeepsTheCurrentStepGenerationOfAnActiveSubject(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	const subject = "slot:review3"

	oldReset := stepEvent(t, st, 50*day, subject, KindStepReset, "", "")
	oldStep := stepEvent(t, st, 49*day, subject, KindStep, "provision", PhaseOK)
	curReset := stepEvent(t, st, 45*day, subject, KindStepReset, "", "")
	curBegin := stepEvent(t, st, 44*day, subject, KindStep, "worktree_add", PhaseBegin)
	curStep := stepEvent(t, st, 44*day, subject, KindStep, "worktree_add", PhaseOK)
	oldPlain := stepEvent(t, st, 44*day, subject, "slot.note", "", "")
	recent := stepEvent(t, st, 2*day, subject, "slot.note", "", "") // any kind of event keeps the subject active

	res, err := st.Prune(ctx, 30*day, 7*day)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(3); res.Events != want { // oldReset, oldStep, oldPlain
		t.Fatalf("Prune = %+v, want %d events", res, want)
	}
	if got, want := eventIDs(t, st), []int64{curReset, curBegin, curStep, recent}; !slices.Equal(got, want) {
		t.Fatalf("events left = %v, want %v (not %d, %d, %d)", got, want, oldReset, oldStep, oldPlain)
	}
	if done, err := st.StepDone(ctx, subject, "worktree_add"); err != nil || !done {
		t.Fatalf("an active subject must still resume where it stopped: StepDone = %v, %v", done, err)
	}
	if done, err := st.StepDone(ctx, subject, "provision"); err != nil || done {
		t.Fatalf("a step of a superseded generation is not done: StepDone = %v, %v", done, err)
	}
}

// The window is measured on the subject's newest event, not on its step rows:
// a subject whose last event is just inside it keeps everything, and one whose
// last event is just outside loses everything.
func TestPruneStepRowsFollowTheSubjectsNewestEvent(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()

	inside := []int64{
		stepEvent(t, st, 60*day, "slot:inside", KindStepReset, "", ""),
		stepEvent(t, st, 60*day, "slot:inside", KindStep, "setup", PhaseOK),
		stepEvent(t, st, 30*day-time.Minute, "slot:inside", "slot.note", "", ""),
	}
	stepEvent(t, st, 60*day, "slot:outside", KindStepReset, "", "")
	stepEvent(t, st, 60*day, "slot:outside", KindStep, "setup", PhaseOK)
	stepEvent(t, st, 30*day+time.Minute, "slot:outside", "slot.note", "", "")
	// Another subject's recent event does not keep a silent subject's rows.
	stepEvent(t, st, time.Hour, "slot:elsewhere", "slot.note", "", "")
	elsewhere := eventIDs(t, st)[len(eventIDs(t, st))-1]

	if _, err := st.Prune(ctx, 30*day, 7*day); err != nil {
		t.Fatal(err)
	}
	if got, want := eventIDs(t, st), append(slices.Clone(inside), elsewhere); !slices.Equal(got, want) {
		t.Fatalf("events left = %v, want %v", got, want)
	}
	if done, err := st.StepDone(ctx, "slot:inside", "setup"); err != nil || !done {
		t.Fatalf("slot:inside: StepDone = %v, %v", done, err)
	}
	if done, err := st.StepDone(ctx, "slot:outside", "setup"); err != nil || done {
		t.Fatalf("slot:outside forgot its steps: StepDone = %v, %v", done, err)
	}

	// A second pass finds nothing more.
	if res, err := st.Prune(ctx, 30*day, 7*day); err != nil || res.Events != 0 {
		t.Fatalf("second Prune = %+v, %v", res, err)
	}
}

// A step row inside the window is never pruned, silent subject or not: only age
// makes a row eligible.
func TestPruneNeverTouchesStepRowsInsideTheWindow(t *testing.T) {
	st, _ := newStore(t)
	ids := []int64{
		stepEvent(t, st, 29*day, "slot:fresh", KindStepReset, "", ""),
		stepEvent(t, st, 29*day, "slot:fresh", KindStep, "setup", PhaseOK),
	}
	if res, err := st.Prune(context.Background(), 30*day, 7*day); err != nil || res.Events != 0 {
		t.Fatalf("Prune = %+v, %v", res, err)
	}
	if got := eventIDs(t, st); !slices.Equal(got, ids) {
		t.Fatalf("events left = %v, want %v", got, ids)
	}
}
