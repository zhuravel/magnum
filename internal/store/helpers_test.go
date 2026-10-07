package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPendingRequests(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	var ids []int64
	for _, k := range []string{"kick", "review", "provision"} {
		id, err := st.EnqueueRequest(ctx, k, map[string]int{"n": 1})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := st.CompleteRequest(ctx, ids[0], RequestDone, "ok"); err != nil {
		t.Fatal(err)
	}
	all, err := st.PendingRequests(ctx, 0)
	if err != nil || len(all) != 2 || all[0].ID != ids[1] || all[1].ID != ids[2] || all[1].Kind != "provision" {
		t.Fatalf("PendingRequests = %+v, %v", all, err)
	}
	if string(all[0].Payload) != `{"n":1}` {
		t.Fatalf("payload = %s", all[0].Payload)
	}
	one, err := st.PendingRequests(ctx, 1)
	if err != nil || len(one) != 1 || one[0].ID != ids[1] {
		t.Fatalf("limited = %+v, %v", one, err)
	}
}

func TestSlotByPRAndAssignSlot(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 7, PRReviewed)
	if _, err := st.SlotByPR(ctx, pr.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no slot yet: %v", err)
	}
	sl := mustSlot(t, st, "review2", SlotFree)
	// AssignSlot leaves the PR's state alone (ClaimSlot would refuse a
	// reviewed PR).
	if _, err := st.ClaimSlot(ctx, pr.ID, sl.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("ClaimSlot on a reviewed PR = %v, want ErrConflict", err)
	}
	a, err := st.AssignSlot(ctx, pr.ID, sl.ID, "talkable_development__review2")
	if err != nil {
		t.Fatalf("AssignSlot: %v", err)
	}
	if a.PRID != pr.ID || a.SlotID != sl.ID || Deref(a.HeadSHA) != "sha-7" || len(a.DBNames) != 1 {
		t.Fatalf("assignment = %+v", a)
	}
	cur, _ := st.PRByID(ctx, pr.ID)
	if cur.State != PRReviewed {
		t.Fatalf("PR state = %s, want reviewed", cur.State)
	}
	got, err := st.SlotByPR(ctx, pr.ID)
	if err != nil || got.ID != sl.ID || got.State != SlotClaimed {
		t.Fatalf("SlotByPR = %+v, %v", got, err)
	}
	if _, err := st.AssignSlot(ctx, pr.ID, sl.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("second AssignSlot = %v, want ErrConflict", err)
	}

	if err := st.TransitionSlot(ctx, sl.ID, []string{SlotClaimed}, SlotRemoved, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SlotByPR(ctx, pr.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a removed slot does not count: %v", err)
	}

	if err := st.UpdateAssignmentHead(ctx, a.ID, "sha-new"); err != nil {
		t.Fatal(err)
	}
	if open, _ := st.OpenAssignmentBySlot(ctx, sl.ID); Deref(open.HeadSHA) != "sha-new" {
		t.Fatalf("head = %v", open.HeadSHA)
	}
	closeAssignment(t, st, a.ID, "released")
	if err := st.UpdateAssignmentHead(ctx, a.ID, "x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("ended assignment = %v, want ErrConflict", err)
	}
	if err := st.UpdateAssignmentHead(ctx, 999, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown assignment = %v, want ErrNotFound", err)
	}
}

func TestEventsMatching(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	for _, s := range []string{"slot:review3", "slot:review3:remove", "slot:review30", "slot:Review3:x", "slot:réview3:remove",
		"pr:o/r#1", "x*y", "x_y", "x%y", "xay", "x[1", "x?z"} {
		subject := s
		if _, err := st.AppendEvent(ctx, Event{Level: "info", Subject: &subject, Kind: "k", Message: s}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AppendEvent(ctx, Event{Level: "info", Kind: "k", Message: "no subject"}); err != nil {
		t.Fatal(err)
	}
	messages := func(evs []Event) string {
		var out []string
		for _, e := range evs {
			out = append(out, e.Message)
		}
		return strings.Join(out, " ")
	}
	cases := []struct {
		name    string
		matches []SubjectMatch
		after   int64
		limit   int
		want    string
	}{
		{"prefix without colon", []SubjectMatch{{Prefix: "slot:review3"}}, 0, 0, "slot:review3 slot:review3:remove slot:review30"},
		{"prefix with colon", []SubjectMatch{{Prefix: "slot:review3:"}}, 0, 0, "slot:review3:remove"},
		{"case sensitive", []SubjectMatch{{Prefix: "slot:Review3"}}, 0, 0, "slot:Review3:x"},
		{"non-ASCII prefix is byte exact", []SubjectMatch{{Prefix: "slot:réview3:"}}, 0, 0, "slot:réview3:remove"},
		{"limit keeps the newest, oldest first", []SubjectMatch{{Prefix: "slot:"}}, 0, 2, "slot:Review3:x slot:réview3:remove"},
		{"exact", []SubjectMatch{{Exact: "slot:review3"}}, 0, 0, "slot:review3"},
		{"exact is not a prefix", []SubjectMatch{{Exact: "slot:review"}}, 0, 0, ""},
		{"either match", []SubjectMatch{{Exact: "pr:o/r#1"}, {Prefix: "slot:review30"}}, 0, 0, "slot:review30 pr:o/r#1"},
		{"exact and prefix in one match", []SubjectMatch{{Exact: "pr:o/r#1", Prefix: "slot:review30"}}, 0, 0, "slot:review30 pr:o/r#1"},
		{"star is literal", []SubjectMatch{{Prefix: "x*"}}, 0, 0, "x*y"},
		{"underscore is literal", []SubjectMatch{{Prefix: "x_"}}, 0, 0, "x_y"},
		{"percent is literal", []SubjectMatch{{Prefix: "x%"}}, 0, 0, "x%y"},
		{"question mark is literal", []SubjectMatch{{Prefix: "x?"}}, 0, 0, "x?z"},
		{"bracket is literal", []SubjectMatch{{Prefix: "x["}}, 0, 0, "x[1"},
		{"after skips older ids", []SubjectMatch{{Prefix: "slot:"}}, 3, 0, "slot:Review3:x slot:réview3:remove"},
		{"empty prefix matches nothing", []SubjectMatch{{}}, 0, 0, ""},
		{"no matches", nil, 0, 0, ""},
	}
	for _, c := range cases {
		evs, err := st.EventsMatching(ctx, c.matches, c.after, c.limit)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := messages(evs); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestForgetSend(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	if ok, err := st.ShouldSend(ctx, "attention:1", time.Hour); err != nil || !ok {
		t.Fatalf("first send = %v, %v", ok, err)
	}
	if ok, _ := st.ShouldSend(ctx, "attention:1", time.Hour); ok {
		t.Fatal("repeat inside the window must be suppressed")
	}
	if err := st.ForgetSend(ctx, "attention:1"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.ShouldSend(ctx, "attention:1", time.Hour); !ok {
		t.Fatal("after ForgetSend the next send goes out")
	}
	if err := st.ForgetSend(ctx, "never-sent"); err != nil {
		t.Fatal(err)
	}
}

func TestSetRepoClonePath(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	if err := st.SetRepoClonePath(ctx, repo.ID, "/p/zhuravel-widgets"); err != nil {
		t.Fatal(err)
	}
	got, _ := st.RepoByID(ctx, repo.ID)
	if Deref(got.ClonePath) != "/p/zhuravel-widgets" {
		t.Fatalf("clone path = %v", got.ClonePath)
	}
	if err := st.SetRepoClonePath(ctx, repo.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.RepoByID(ctx, repo.ID); got.ClonePath != nil {
		t.Fatalf("clone path = %q, want NULL", *got.ClonePath)
	}
	if err := st.SetRepoClonePath(ctx, 999, "/x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown repo = %v", err)
	}
}

func TestKVKeys(t *testing.T) {
	cases := map[string]string{
		KVToolPausedUntil("codex"):             "codex.paused_until",
		KVToolPausedReason("claude"):           "claude.paused_reason",
		KVToolPausedDetail("codex"):            "codex.paused_detail",
		KVToolBackoff("codex"):                 "codex.backoff",
		KVIdentityCheck("zhuravel"):            "identity.zhuravel.check",
		KVIdentityError("zhuravel"):            "identity.zhuravel.error",
		KVIdentityTickError("app"):             "identity.app.tick_error",
		KVIdentityTokenExpiry("app"):           "identity.app.token_expiry",
		KVWatchPaused("ZhuraVEL"):              "watch.zhuravel.paused",
		KVWatchPoll("TalKable"):                "watch.talkable.poll",
		KVPRSimplify(3):                        "pr.3.simplify",
		KVPRRoles(3):                           "pr.3.roles",
		KVPRFresh(3):                           "pr.3.fresh",
		KVPRDryRun(3):                          "pr.3.dry_run",
		KVDaemonPaused + KVHerdrUp + KVGHReset: "daemon.pausedherdr.upgh.reset",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("key %q, want %q", got, want)
		}
	}
}

// openAssignmentOf is the PR's open assignment, or ErrNotFound.
func openAssignmentOf(ctx context.Context, st *Store, prID int64) (Assignment, error) {
	return st.assignmentWhere(ctx, "pr_id = ? AND ended_at IS NULL", prID)
}

// closeAssignment ends open assignment id, as a release would.
func closeAssignment(t *testing.T, st *Store, id int64, reason string) {
	t.Helper()
	res, err := st.db.Exec("UPDATE assignments SET ended_at = ?, end_reason = ? WHERE id = ? AND ended_at IS NULL",
		FormatTime(st.now()), reason, id)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("assignment %d is not open", id)
	}
}
