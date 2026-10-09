package engine

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// bumpSchema makes the registry look migrated by another binary.
func bumpSchema(t *testing.T, st *store.Store) {
	t.Helper()
	v, err := st.SchemaVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec("PRAGMA user_version = " + strconv.Itoa(v+1)); err != nil {
		t.Fatal(err)
	}
}

func TestTickStopsWhenTheSchemaWasMigrated(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "a1"})
	if err := h.e.Tick(h.ctx); errors.Is(err, ErrSchemaChanged) {
		t.Fatalf("unchanged schema: %v", err)
	}
	radar := h.gh.count("radar:talkable")
	bumpSchema(t, h.st)
	if err := h.e.Tick(h.ctx); !errors.Is(err, ErrSchemaChanged) {
		t.Fatalf("Tick = %v, want ErrSchemaChanged", err)
	}
	if got := h.gh.count("radar:talkable"); got != radar {
		t.Fatalf("a tick on a migrated schema polled GitHub (%d radar calls, want %d)", got, radar)
	}
}

func TestRunExitsNonZeroWhenTheSchemaWasMigrated(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "a1"})
	bumpSchema(t, h.st)
	done := h.goRun(h.ctx, Options{NoSignals: true})
	select {
	case err := <-done:
		if !errors.Is(err, ErrSchemaChanged) || err.Error() != "schema migrated under the daemon; exiting for launchd to restart" {
			t.Fatalf("Run = %v, want ErrSchemaChanged", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run kept going on a migrated schema")
	}

	// --once too.
	h2 := newHarness(t)
	bumpSchema(t, h2.st)
	if err := h2.e.Run(h2.ctx, Options{Once: true, NoSignals: true}); !errors.Is(err, ErrSchemaChanged) {
		t.Fatalf("Run --once = %v, want ErrSchemaChanged", err)
	}
}

func TestRunWithTheDaemonLockHeld(t *testing.T) {
	h := newHarness(t)
	unlock, held, err := AcquireLock(h.layout.Lock())
	if err != nil || held {
		t.Fatalf("lock: held=%v err=%v", held, err)
	}
	defer unlock()

	// Another daemon holds the lock: "already running", exit 0, so
	// launchd does not restart it in a loop.
	if err := h.e.Run(h.ctx, Options{Once: true, NoSignals: true}); err != nil {
		t.Fatalf("another daemon: Run = %v, want nil", err)
	}
	// The probe released ops.lock again.
	if u, held, err := AcquireLock(h.layout.OpsLock()); err != nil || held {
		t.Fatalf("ops.lock after the probe: held=%v err=%v", held, err)
	} else {
		u()
	}

	// A magnum command runs slot work in-process: it holds ops.lock and
	// then the daemon lock. Run exits non-zero for launchd to retry.
	unlockOps, held, err := AcquireLock(h.layout.OpsLock())
	if err != nil || held {
		t.Fatalf("ops lock: held=%v err=%v", held, err)
	}
	defer unlockOps()
	err = h.e.Run(h.ctx, Options{Once: true, NoSignals: true})
	if !errors.Is(err, ErrOpsLockHeld) || err.Error() != "a magnum command holds state/ops.lock for in-process slot work; exiting so launchd starts the daemon again" {
		t.Fatalf("command in-process: Run = %v, want ErrOpsLockHeld", err)
	}
	if h.gh.count("radar:talkable") != 0 {
		t.Fatal("Run ticked without the lock")
	}
}

func TestReconcilePrunesOldEventsAndRequests(t *testing.T) {
	h := newHarness(t)
	h.cfg.Daemon.KeepEvents.Duration = 30 * 24 * time.Hour
	h.cfg.Daemon.KeepRequests.Duration = 7 * 24 * time.Hour
	done, err := h.st.EnqueueRequest(h.ctx, ReqKick, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.CompleteRequest(h.ctx, done, store.RequestDone, "ok"); err != nil {
		t.Fatal(err)
	}
	pending, err := h.st.EnqueueRequest(h.ctx, ReqKick, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	h.advance(8 * 24 * time.Hour)
	now := h.clock.Now()
	subject := "pr:talkable/talkable#1"
	old, err := h.st.AppendEvent(h.ctx, store.Event{At: now.Add(-31 * 24 * time.Hour), Subject: &subject, Kind: "test.old", Message: "old"})
	if err != nil {
		t.Fatal(err)
	}
	recent, err := h.st.AppendEvent(h.ctx, store.Event{At: now.Add(-24 * time.Hour), Subject: &subject, Kind: "test.recent", Message: "recent"})
	if err != nil {
		t.Fatal(err)
	}

	_ = h.e.reconcile(h.ctx) // other steps may report fake errors; the prune runs regardless

	if _, err := h.st.RequestByID(h.ctx, done); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("handled request older than keep_requests: err %v, want not found", err)
	}
	if _, err := h.st.RequestByID(h.ctx, pending); err != nil {
		t.Fatalf("a pending request is never pruned: %v", err)
	}
	evs, err := h.st.EventsBySubject(h.ctx, subject, 0)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int64]bool{}
	for _, ev := range evs {
		ids[ev.ID] = true
	}
	if ids[old] || !ids[recent] {
		t.Fatalf("events after prune: %+v", evs)
	}

	// keep_events = 0 keeps every event.
	h.cfg.Daemon.KeepEvents.Duration = 0
	h.advance(60 * 24 * time.Hour)
	if err := h.e.prune(h.ctx); err != nil {
		t.Fatal(err)
	}
	if evs, _ := h.st.EventsBySubject(h.ctx, subject, 0); len(evs) != 1 {
		t.Fatalf("keep_events = 0 pruned: %+v", evs)
	}
}

// onPosted marks a PR reviewed only while its head is the reviewed commit: a
// push the poller recorded after finish read the PR fails the head_sha guard
// and the PR goes to rereview_pending with the review on record.
func TestOnPostedGuardsReviewedOnTheHead(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	// A new round at b1 is under way.
	if err := h.st.TransitionPR(h.ctx, pr.ID, []string{store.PRReviewed}, store.PRReviewing, nil); err != nil {
		t.Fatal(err)
	}
	stale, err := h.st.PRByID(h.ctx, pr.ID) // finish's read
	if err != nil {
		t.Fatal(err)
	}
	// The poller records a push meanwhile (onHeadChange on an in-flight PR).
	if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) {
		u.Set("head_sha", "b2")
		u.Set("pending_since", h.clock.Now())
	}); err != nil {
		t.Fatal(err)
	}
	repo, err := h.st.RepoByFullName(h.ctx, "talkable/talkable")
	if err != nil {
		t.Fatal(err)
	}
	job := &roundJob{pr: stale, repo: repo, watch: *h.cfg.WatchFor("talkable/talkable")}
	res := pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: 2, ReviewID: 77, Event: "COMMENTED"}
	h.e.onPosted(h.ctx, job, stale, pipeline.RoundInput{TargetSHA: "b1"}, res, []string{store.PRReviewing, store.PRClaiming})

	got := h.wantState(2, store.PRRereviewPending)
	if deref(got.ReviewedSHA) != "b1" || got.LastReviewID == nil || *got.LastReviewID != 77 || got.NextEligibleAt == nil {
		t.Fatalf("after a head move: reviewed_sha %q last_review_id %v next_eligible_at %v",
			deref(got.ReviewedSHA), got.LastReviewID, got.NextEligibleAt)
	}
	// The guard decided it: the PR never passed through reviewed (the
	// requeueMovedHead backstop logs "head moved during the round").
	evs, err := h.st.EventsBySubject(h.ctx, "pr:talkable/talkable#2", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if strings.Contains(ev.Message, "head moved during the round") {
			t.Fatalf("the PR was marked reviewed at an old head first: %s", ev.Message)
		}
	}

	// Without a push the same call marks it reviewed.
	if err := h.st.TransitionPR(h.ctx, pr.ID, []string{store.PRRereviewPending}, store.PRReviewing, nil); err != nil {
		t.Fatal(err)
	}
	cur, err := h.st.PRByID(h.ctx, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	job.pr = cur
	h.e.onPosted(h.ctx, job, cur, pipeline.RoundInput{TargetSHA: "b2"}, res, []string{store.PRReviewing, store.PRClaiming})
	if got := h.wantState(2, store.PRReviewed); deref(got.ReviewedSHA) != "b2" {
		t.Fatalf("reviewed_sha %q, want b2", deref(got.ReviewedSHA))
	}
}

// `magnum identities check` hands its verdict to the running daemon, which
// records it as its own checks do (kv, toast dedup, events).
func TestIdentityVerdictRequest(t *testing.T) {
	h := newHarness(t)
	h.e.recordIdentityVerdict(h.ctx, "zhuravel", false, "token belongs to someone else")
	// The failure toasted: its dedup record now holds the next one back.
	if ok, err := h.st.ShouldSend(h.ctx, "identity:zhuravel", time.Hour); err != nil || !ok {
		t.Fatalf("first send: %v %v", ok, err)
	}

	pass := h.enqueue(ReqIdentityVerdict, IdentityVerdictPayload{Name: "zhuravel", Pass: true})
	h.e.handleRequests(h.ctx)
	if r := h.request(pass); r.State != store.RequestDone || deref(r.Result) != "identities check: pass" {
		t.Fatalf("pass request: %+v", r)
	}
	if v, _ := h.e.getKV(h.ctx, store.KVIdentityCheck("zhuravel")); v != "pass" {
		t.Fatalf("identity check kv = %q", v)
	}
	if v, ok := h.e.getKV(h.ctx, store.KVIdentityError("zhuravel")); ok {
		t.Fatalf("identity error kv kept: %q", v)
	}
	// fail → pass forgot the dedup record: the next failure notifies at once.
	if ok, err := h.st.ShouldSend(h.ctx, "identity:zhuravel", time.Hour); err != nil || !ok {
		t.Fatalf("after a pass the next failure must toast: %v %v", ok, err)
	}

	fail := h.enqueue(ReqIdentityVerdict, IdentityVerdictPayload{Name: "talkable-app", Reason: "no pull_requests write"})
	h.e.handleRequests(h.ctx)
	if r := h.request(fail); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "no pull_requests write") {
		t.Fatalf("fail request: %+v", r)
	}
	if v, _ := h.e.getKV(h.ctx, store.KVIdentityCheck("talkable-app")); v != "fail" {
		t.Fatalf("identity check kv = %q", v)
	}
	if v, _ := h.e.getKV(h.ctx, store.KVIdentityError("talkable-app")); v != "no pull_requests write" {
		t.Fatalf("identity error kv = %q", v)
	}

	unknown := h.enqueue(ReqIdentityVerdict, IdentityVerdictPayload{Name: "nobody", Pass: true})
	h.e.handleRequests(h.ctx)
	if r := h.request(unknown); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), `unknown identity "nobody"`) {
		t.Fatalf("unknown identity: %+v", r)
	}
	if _, ok := h.e.getKV(h.ctx, store.KVIdentityCheck("nobody")); ok {
		t.Fatal("an unknown identity got a verdict")
	}
}

// GitHub cut a PR's label list short: a skip label on the page still skips
// it; without one the PR is reviewed and the truncation is reported once.
func TestTruncatedLabelsSkipOnlyOnAVisibleSkipLabel(t *testing.T) {
	h := newHarness(t)
	h.cfg.Watches[0].SkipLabels = []string{"wip"}
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync: #1 baseline
	skipped := prSpec{n: 2, head: "b1", labels: []string{"wip"}, labelsTruncated: true}
	unsure := prSpec{n: 3, head: "c1", labels: []string{"backend"}, labelsTruncated: true}
	h.open(prSpec{n: 1, head: "base1"}, skipped, unsure)
	h.tick()
	h.wantState(2, store.PRIneligible)
	h.wantState(3, store.PRQueued)

	// A later Details fetch (updatedAt moved) does not report it again.
	h.advance(time.Minute)
	unsure.updated = h.clock.Now()
	h.open(prSpec{n: 1, head: "base1"}, skipped, unsure)
	h.tick()
	evs, err := h.st.EventsBySubject(h.ctx, "pr:talkable/talkable#3", 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range evs {
		if ev.Kind == "poll.labels_truncated" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("labels_truncated events for #3 = %d, want 1: %+v", n, evs)
	}
	// A complete list says nothing.
	if evs, _ := h.st.EventsBySubject(h.ctx, "pr:talkable/talkable#1", 0); slices.ContainsFunc(evs, func(ev store.Event) bool {
		return ev.Kind == "poll.labels_truncated"
	}) {
		t.Fatal("a complete label list was reported as truncated")
	}
}

// GitHub cut the latestReviews page short and the App's review is not on it:
// since_review measures from that review, found in the PR's last reviews.
func TestSinceReviewFallsBackToReviewsWhenLatestReviewsAreTruncated(t *testing.T) {
	h := newHarness(t)
	sub := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	h.gh.allReviews = map[int][]github.Review{1: {
		{State: "COMMENTED", SubmittedAt: sub.Add(-time.Hour), AuthorLogin: "talkable", AuthorType: "Bot", CommitOid: "a00"},
		{State: "APPROVED", SubmittedAt: sub, AuthorLogin: "someone", AuthorType: "User", CommitOid: "a05"},
		{State: "COMMENTED", SubmittedAt: sub, AuthorLogin: "talkable", AuthorType: "Bot", CommitOid: "a0"},
		{State: "PENDING", AuthorLogin: "talkable", AuthorType: "Bot", CommitOid: "a09"},
	}}
	compareRange(h, "a0...a1", github.CompareStats{Commits: 2, Files: 3, Additions: 30, Deletions: 4})
	h.open(prSpec{n: 1, head: "a1", reviewsTruncated: true})
	h.startup()
	h.tick()
	pr := h.pr(1)
	if s := pr.SinceReview; s == nil || s.Source != store.SinceFromReview || s.Base != "a0" || s.Additions != 30 {
		t.Fatalf("since review = %+v", pr.SinceReview)
	}
	if n := h.gh.count("reviews:talkable/talkable#1"); n != 1 {
		t.Fatalf("reviews calls = %d, want 1", n)
	}

	// A complete page needs no extra call: the whole PR is the base.
	h2 := newHarness(t)
	h2.open(prSpec{n: 1, head: "a1"})
	h2.startup()
	h2.tick()
	if n := h2.gh.count("reviews:"); n != 0 {
		t.Fatalf("reviews calls with a complete page = %d", n)
	}
	if s := h2.pr(1).SinceReview; s == nil || s.Source != store.SinceFromBase {
		t.Fatalf("since review = %+v", s)
	}
}
