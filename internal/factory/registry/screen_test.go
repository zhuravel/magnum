package registry_test

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
)

// The let-go line reads only abandoned prompts: the newest prompt of each
// role of the task, when it is abandoned; a role whose newer prompt went
// out has none, and another task's prompts are not read.
func TestLetGoneIsTheNewestPromptOfEachRoleWhenAbandoned(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	ctx := t.Context()
	task := createTask(t, r, "PR-12371")
	other := createTask(t, r, "PR-12372")
	a, _ := r.BeginAttempt(ctx, task.ID, "investigate")
	b, _ := r.BeginAttempt(ctx, other.ID, "investigate")
	add := func(id int64, attempt, role, hash, delivered string) registry.LedgerEntry {
		t.Helper()
		e, err := r.AppendLedger(ctx, registry.LedgerEntry{TaskID: id, AttemptID: attempt, Role: role, Hash: hash, Text: "prompt " + hash})
		if err != nil {
			t.Fatal(err)
		}
		if delivered != "" {
			if err := r.MarkDelivered(ctx, e.ID, delivered); err != nil {
				t.Fatal(err)
			}
			e.Delivered = delivered
		}
		return e
	}
	add(task.ID, a.ID, "fx-repro", "h1", registry.DeliveredAbandoned)
	add(task.ID, a.ID, "fx-repro", "h2", registry.DeliveredYes) // a newer prompt went out
	add(task.ID, a.ID, "fx-cause", "h3", registry.DeliveredYes)
	cause := add(task.ID, a.ID, "fx-cause", "h4", registry.DeliveredAbandoned)
	add(other.ID, b.ID, "fx-cause", "h5", registry.DeliveredAbandoned)

	got, err := r.LetGone(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != cause {
		t.Fatalf("LetGone = %+v, want the cause finder's newest prompt only", got)
	}
}

// The screen's reads every few seconds go through indexes: the events of a
// kind (the alerts) and one task's ledger.
func TestTheScreensReadsUseIndexes(t *testing.T) {
	t.Parallel()
	r := registrytest.Open(t)
	db := r.DBForTest()
	for _, q := range []struct{ sql, index string }{
		{"EXPLAIN QUERY PLAN SELECT id FROM events WHERE kind IN ('alert') ORDER BY id DESC LIMIT 200", "events_kind"},
		{"EXPLAIN QUERY PLAN SELECT id FROM ledger WHERE task_id = 1 AND delivered = 'abandoned'", "ledger_task"},
	} {
		plan := queryPlan(t, db, q.sql)
		if !strings.Contains(strings.Join(plan, "; "), q.index) {
			t.Errorf("%s: plan %q, want index %s", q.sql, plan, q.index)
		}
	}
}

// The status rule's news is the newest request of the given kinds the
// daemon answered done since a time: typed, never parsed from an event, and
// never crowded out by newer requests of other kinds or by a failure.
func TestLastDoneIsTheNewestAnsweredRequestOfItsKinds(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	complete := func(kind, state, result string) int64 {
		t.Helper()
		id, err := r.EnqueueRequest(ctx, kind, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.CompleteRequest(ctx, id, state, result); err != nil {
			t.Fatal(err)
		}
		clk.Add(time.Second)
		return id
	}
	complete("start", registry.RequestDone, "started PR-12344 as task 1")
	start := complete("start", registry.RequestDone, "started PR-12345 as task 2")
	for range 30 {
		complete("focus", registry.RequestDone, "the screen has focus")
	}
	complete("retry", registry.RequestFailed, "PR-12345 is active")
	q, ok, err := r.LastDone(ctx, t0, "start", "retry")
	if err != nil || !ok || q.ID != start || q.Result != "started PR-12345 as task 2" {
		t.Fatalf("LastDone = %+v %v %v, want the second start", q, ok, err)
	}
	if _, ok, err := r.LastDone(ctx, clk.Now(), "start"); err != nil || ok {
		t.Errorf("LastDone since now = %v %v, want none", ok, err)
	}
}

// The screen's own requests do not pile up: a completed focus report or
// preview older than the cutoff goes, with its event; a pending one, a
// newer one and the other kinds stay. A completed preview forgets the text
// it carried.
func TestPruneDropsTheScreensOldRequestsAndTheirEvents(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	enqueue := func(kind string, done bool) int64 {
		t.Helper()
		id, err := r.EnqueueRequest(ctx, kind, map[string]string{"Text": "PR-12345 my pasted draft"})
		if err != nil {
			t.Fatal(err)
		}
		if done {
			if err := r.CompleteRequest(ctx, id, registry.RequestDone, "ok"); err != nil {
				t.Fatal(err)
			}
			if err := r.AppendEvent(ctx, 0, "request", registry.PhaseOK, "request "+itoa(id)+" ("+kind+"): ok"); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	oldFocus, oldPreview, oldStart := enqueue("focus", true), enqueue("preview", true), enqueue("start", true)
	pending := enqueue("preview", false)
	clk.Add(25 * time.Hour)
	fresh := enqueue("focus", true)

	if err := r.ForgetPayload(ctx, oldPreview); err != nil {
		t.Fatal(err)
	}
	if q, _ := r.RequestByID(ctx, oldPreview); string(q.Payload) != "{}" {
		t.Errorf("a completed preview keeps its payload %s", q.Payload)
	}
	if err := r.ForgetPayload(ctx, pending); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("forgetting a pending request's payload: %v, want ErrConflict", err)
	}

	n, err := r.PruneRequests(ctx, clk.Now().Add(-24*time.Hour), "focus", "preview")
	if err != nil || n != 2 {
		t.Fatalf("pruned %d (%v), want the old focus report and the old preview", n, err)
	}
	for id, want := range map[int64]bool{oldFocus: false, oldPreview: false, oldStart: true, pending: true, fresh: true} {
		if _, err := r.RequestByID(ctx, id); (err == nil) != want {
			t.Errorf("request %d kept %v, want %v", id, err == nil, want)
		}
	}
	evs, err := r.Events(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, ev := range evs {
		texts = append(texts, ev.Text)
	}
	if got := strings.Join(texts, "|"); got != "request "+itoa(oldStart)+" (start): ok|request "+itoa(fresh)+" (focus): ok" {
		t.Errorf("events %q, want the start's and the fresh focus report's", got)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// queryPlan is the details of SQLite's plan of query.
func queryPlan(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan
}
