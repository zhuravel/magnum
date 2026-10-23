package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// kvOf is key's value in the test registry, and whether it is there.
func (h *harness) kvOf(key string) (string, bool) {
	h.t.Helper()
	v, ok, err := h.st.GetKV(h.ctx, key)
	if err != nil {
		h.t.Fatal(err)
	}
	return v, ok
}

// setReviewBody sets the body of review id of PR number, as an edit on
// GitHub does.
func (g *fakeGH) setReviewBody(number int, id int64, body string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, r := range g.allReviews[number] {
		if r.DatabaseID == id {
			g.allReviews[number][i].Body = body
		}
	}
}

// writeGH is the fake GitHub whose approval and dismissal, once GitHub took
// them, run onCreate and onDismiss (when set) before they answer: what
// happens while the answer is on its way, which may replace it.
type writeGH struct {
	*fakeGH
	onCreate  func(ctx context.Context, rev github.RESTReview, err error) (github.RESTReview, error)
	onDismiss func(ctx context.Context, err error) error
}

func (g *writeGH) CreateReview(ctx context.Context, owner, repo string, number int, commitID, event, body string) (github.RESTReview, error) {
	rev, err := g.fakeGH.CreateReview(ctx, owner, repo, number, commitID, event, body)
	if g.onCreate != nil {
		return g.onCreate(ctx, rev, err)
	}
	return rev, err
}

func (g *writeGH) DismissReview(ctx context.Context, owner, repo string, number int, reviewID int64, message string) error {
	err := g.fakeGH.DismissReview(ctx, owner, repo, number, reviewID, message)
	if g.onDismiss != nil {
		return g.onDismiss(ctx, err)
	}
	return err
}

// newWriteHarness is newAutoHarness whose operator identity writes through
// a writeGH over its fake GitHub.
func newWriteHarness(t *testing.T) (*harness, *autoPlan, *writeGH) {
	t.Helper()
	g := &writeGH{}
	h, plan := newAutoHarness(t, func(h *harness) {
		g.fakeGH = h.gh
		h.d.GitHub = func(id string) GitHub {
			if id == "zhuravel" {
				return g
			}
			return nil
		}
	})
	return h, plan, g
}

// The 2026-10-22 case of a PR whose approval as the operator magnum
// withdrew as a restart came: GitHub dismissed the review, the answer was
// lost (the stop's cancelled context; a timeout, a 5xx alike), and the
// retry got 422 and found the review dismissed, by the operator's login in
// its timeline, since magnum acts as them. The pending record of the first
// request says it was magnum's own: the withdrawal ends as magnum's with
// "the first request's answer was lost", toasted as one, and auto-approval
// stays on (the next clean round approves again); nothing says "you
// dismissed it".
func TestAWithdrawalWhoseAnswerWasLostIsMagnumsOwnWhenTheRetryFindsTheReviewDismissed(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"a cancelled context", fmt.Errorf("gh api: %w", context.Canceled)},
		{"a timeout", fmt.Errorf("gh api: %w", context.DeadlineExceeded)},
		{"a bad gateway", &github.APIError{Op: "dismiss review", Status: 502, Message: "Bad Gateway"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, plan := newAutoHarness(t)
			plan.set(2, cleanRound, blockingRound, cleanRound)
			h.reviewedPR(2, "b1")
			h.tick()
			h.gh.mu.Lock()
			h.gh.dismissals = map[int][]github.ReviewDismissal{2: {{ReviewID: 9001, Actor: "zhuravel"}}}
			h.gh.mu.Unlock()
			h.gh.dismissErr, h.gh.dismissTakes = tc.err, true // GitHub takes it, the answer is lost
			h.push(2, "b2")
			h.reviewTo(2, "b2")
			h.tick()
			if a := h.autoApproval(2); a.State != store.AutoDismissing || a.Attempts != 1 {
				t.Fatalf("approval after the lost answer = %+v", a)
			}
			if v, ok := h.kvOf(kvAutoWithdrawPending(h.pr(2).ID)); !ok || v != "9001" {
				t.Fatalf("pending record = %q, %v; want the review's id", v, ok)
			}
			h.gh.dismissErr, h.gh.dismissTakes = errUnprocessable, false // GitHub refuses a dismissed review's dismissal
			h.advance(autoApproveRetry)
			h.tick()
			if d := h.ghCalls("dismiss:"); len(d) != 2 {
				t.Fatalf("dismissals = %q", d)
			}
			a := h.autoApproval(2)
			if a.State != store.AutoDismissed || a.EndedBy != store.AutoEndedMagnum || a.EndedAt == nil {
				t.Fatalf("approval = %+v, want withdrawn by magnum", a)
			}
			if hd := h.hold(2); hd.Held {
				t.Fatalf("hold = %+v: magnum's own withdrawal stopped auto-approval", hd)
			}
			evs := h.events("review.auto_approval_withdrawn")
			if len(evs) != 1 || !strings.Contains(evs[0].Message, "(the first request's answer was lost)") {
				t.Fatalf("withdrawn events = %+v", evs)
			}
			if evs := h.events("review.auto_approve_stopped"); len(evs) != 0 {
				t.Fatalf("stopped = %+v", evs)
			}
			if _, ok := h.kvOf(kvAutoWithdrawPending(h.pr(2).ID)); ok {
				t.Fatal("the pending record outlived the known outcome")
			}
			h.flush()
			if toasts := toastsWith(h, "withdrew your approval"); len(toasts) != 1 {
				t.Fatalf("toasts = %q", toasts)
			}
			h.push(2, "b3")
			h.reviewTo(2, "b3")
			h.tick()
			if got := h.created(); len(got) != 2 || !strings.HasPrefix(got[1], "APPROVE@b3:") {
				t.Fatalf("posted = %q, want b3 approved again", got)
			}
			rows, err := h.st.DB().QueryContext(h.ctx, "SELECT message FROM events WHERE message LIKE '%you dismissed it%'")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			if rows.Next() {
				var m string
				_ = rows.Scan(&m)
				t.Fatalf("an event says the operator dismissed it: %q", m)
			}
		})
	}
}

// A dismissal whose outcome is known leaves no pending record: one GitHub
// refused (a 403: it did nothing), or one whose answer was lost when the
// retry's 422 finds the review still standing (a dismissal is for good, so
// none of magnum's took). The operator's own dismissal on GitHub before the
// next retry still stops auto-approval, as one magnum finds with no request
// of its own out.
func TestTheOperatorsDismissalAfterAKnownOutcomeStillStopsAutoApproval(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first error
		lost  bool
	}{
		{"after a refusal", &github.APIError{Op: "dismiss review", Status: 403, Message: "Forbidden"}, false},
		{"after a lost answer GitHub shows did nothing", &github.APIError{Op: "dismiss review", Status: 502, Message: "Bad Gateway"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := approvedThenBlocked(t, tc.first, false)
			if a := h.autoApproval(2); a.State != store.AutoDismissing || a.Attempts != 1 {
				t.Fatalf("approval after the first request = %+v", a)
			}
			if _, ok := h.kvOf(kvAutoWithdrawPending(h.pr(2).ID)); ok != tc.lost {
				t.Fatalf("pending record after the first request: %v, want %v", ok, tc.lost)
			}
			h.gh.dismissErr = errUnprocessable
			if tc.lost { // the retry finds the review standing
				h.advance(autoApproveRetry)
				h.tick()
				if a := h.autoApproval(2); a.State != store.AutoDismissing || a.Attempts != 2 {
					t.Fatalf("approval after the retry = %+v", a)
				}
			}
			if _, ok := h.kvOf(kvAutoWithdrawPending(h.pr(2).ID)); ok {
				t.Fatal("a request whose outcome is known left its pending record")
			}
			h.gh.setReviewState(2, 9001, "DISMISSED") // the operator dismisses it by hand
			h.gh.mu.Lock()
			h.gh.dismissals = map[int][]github.ReviewDismissal{2: {{ReviewID: 9001, Actor: "zhuravel"}}}
			h.gh.mu.Unlock()
			h.advance(autoApproveRetry)
			h.tick()
			if a := h.autoApproval(2); a.State != store.AutoDismissed || a.EndedBy != store.AutoEndedOperator {
				t.Fatalf("approval = %+v, want ended by the operator", a)
			}
			if hd := h.hold(2); !hd.Held || hd.Reason != "you dismissed it (review 9001)" {
				t.Fatalf("hold = %+v", hd)
			}
			h.flush()
			if toasts := toastsWith(h, "withdrew your approval"); len(toasts) != 0 {
				t.Fatalf("toasts = %q", toasts)
			}
		})
	}
}

// An approval whose answer was lost (a 502 after GitHub took it) is
// pending, so the retry that finds an approval of the head by the
// operator's login records it as magnum's, posted once, "the first
// request's answer was lost", even when GitHub lists it without magnum's
// marker (its body edited on GitHub), and auto-approval goes on.
func TestAnApprovalWhoseAnswerWasLostIsMagnumsOwnWithoutItsMarker(t *testing.T) {
	h, _ := newAutoHarness(t)
	h.gh.createErr, h.gh.createTakes = &github.APIError{Op: "review", Status: 502, Message: "Bad Gateway"}, true
	h.reviewedPR(2, "b1")
	h.tick()
	if a := h.autoApproval(2); a.State != store.AutoFailed || a.Attempts != 1 {
		t.Fatalf("approval after the lost answer = %+v", a)
	}
	if v, ok := h.kvOf(kvAutoApprovePending(h.pr(2).ID)); !ok || v != "b1" {
		t.Fatalf("pending record = %q, %v; want the head", v, ok)
	}
	h.gh.setReviewBody(2, 9001, "Approved.")
	h.gh.createErr, h.gh.createTakes = nil, false
	h.advance(autoApproveRetry)
	h.tick()
	if n := len(h.ghCalls("review:")); n != 1 {
		t.Fatalf("posted %d times, want the lost one found", n)
	}
	if a := h.autoApproval(2); a.State != store.AutoStanding || a.ReviewID != 9001 {
		t.Fatalf("approval = %+v", a)
	}
	if hd := h.hold(2); hd.Held {
		t.Fatalf("hold = %+v: magnum's own approval stopped auto-approval", hd)
	}
	evs := h.events("review.auto_approved")
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "(review 9001) (the first request's answer was lost)") {
		t.Fatalf("approved events = %+v", evs)
	}
	if _, ok := h.kvOf(kvAutoApprovePending(h.pr(2).ID)); ok {
		t.Fatal("the pending record outlived the known outcome")
	}
}

// A post GitHub refused (a 400: it did nothing) leaves no pending record,
// so an approval of the head by the operator before the retry is theirs by
// hand, as today: auto-approval of the PR stops and nothing is posted.
func TestAnApprovalByHandAfterARefusedPostStillStopsAutoApproval(t *testing.T) {
	h, _ := newAutoHarness(t)
	h.gh.createErr = &github.APIError{Op: "review", Status: 400, Message: "Bad Request"}
	h.reviewedPR(2, "b1")
	h.tick()
	if _, ok := h.kvOf(kvAutoApprovePending(h.pr(2).ID)); ok {
		t.Fatal("a refused request left its pending record")
	}
	h.gh.addReview(2, github.Review{DatabaseID: 7001, State: "APPROVED", Body: "Approved.", URL: prURL2 + "#pullrequestreview-7001",
		CommitOid: "b1", AuthorLogin: "zhuravel", AuthorType: "User", SubmittedAt: h.clock.Now()})
	h.gh.createErr = nil
	h.advance(autoApproveRetry)
	h.tick()
	if n := len(h.ghCalls("review:")); n != 1 {
		t.Fatalf("posted %d times after the approval by hand", n)
	}
	if hd := h.hold(2); !hd.Held || !strings.Contains(hd.Reason, "you approved it by hand") {
		t.Fatalf("hold = %+v", hd)
	}
}

// A stop during a dismissal request lets it finish within the grace: its
// context outlives the stop, its answer is recorded, and nothing is left
// pending or in flight.
func TestAStopDuringAWithdrawalLetsTheRequestFinishWithinTheGrace(t *testing.T) {
	h, plan, g := newWriteHarness(t)
	plan.set(2, cleanRound, blockingRound)
	h.reviewedPR(2, "b1")
	h.tick()
	h.gh.dismissErr = &github.APIError{Op: "dismiss review", Status: 403, Message: "Forbidden"}
	h.push(2, "b2")
	h.reviewTo(2, "b2")
	h.tick()
	if a := h.autoApproval(2); a.State != store.AutoDismissing {
		t.Fatalf("approval = %+v", a)
	}
	h.gh.dismissErr = nil
	ctx, stop := context.WithCancel(h.ctx)
	defer stop()
	var during error
	g.onDismiss = func(rctx context.Context, err error) error {
		stop()
		time.Sleep(20 * time.Millisecond) // the answer takes a while
		during = rctx.Err()
		return err
	}
	h.advance(autoApproveRetry)
	h.e.autoApprove(ctx)
	if during != nil {
		t.Fatalf("the stop cut the request short: %v", during)
	}
	a := h.autoApproval(2)
	if a.State != store.AutoDismissed || a.EndedBy != store.AutoEndedMagnum {
		t.Fatalf("approval = %+v, want its withdrawal recorded after the stop", a)
	}
	if evs := h.events("review.auto_approval_withdrawn"); len(evs) != 1 {
		t.Fatalf("withdrawn events = %+v", evs)
	}
	if _, ok := h.kvOf(kvAutoWithdrawPending(h.pr(2).ID)); ok {
		t.Fatal("a recorded withdrawal left its pending record")
	}
	if _, ok := h.kvOf(KVDaemonWork); ok || len(h.e.workInFlight()) != 0 {
		t.Fatalf("work in flight after the write: %+v", h.e.workInFlight())
	}
}

// The grace is bounded: a request the stop's grace runs out on is cancelled
// then, nothing failed is recorded and the request stays pending, so the
// next daemon's retry, refused (422) for a review GitHub dismissed, counts
// the withdrawal as magnum's own.
func TestAWithdrawalPastTheStopsGraceIsCancelledAndSettledByTheNextDaemon(t *testing.T) {
	h, plan, g := newWriteHarness(t)
	plan.set(2, cleanRound, blockingRound)
	h.reviewedPR(2, "b1")
	h.tick()
	h.gh.dismissErr = &github.APIError{Op: "dismiss review", Status: 403, Message: "Forbidden"}
	h.push(2, "b2")
	h.reviewTo(2, "b2")
	h.tick()
	h.gh.dismissErr, h.gh.dismissTakes = nil, false
	h.e.writeGrace = 50 * time.Millisecond
	ctx, stop := context.WithCancel(h.ctx)
	defer stop()
	var waited time.Duration
	g.onDismiss = func(rctx context.Context, _ error) error {
		stop()
		start := time.Now()
		select {
		case <-rctx.Done():
		case <-time.After(10 * time.Second):
			t.Error("the grace never ended")
		}
		waited = time.Since(start)
		return fmt.Errorf("gh api: %w", rctx.Err())
	}
	h.advance(autoApproveRetry)
	h.e.autoApprove(ctx)
	if waited < 40*time.Millisecond {
		t.Fatalf("cancelled after %s, before the grace", waited)
	}
	if a := h.autoApproval(2); a.State != store.AutoDismissing || a.Attempts != 2 {
		t.Fatalf("approval = %+v, want it still dismissing, the cut attempt counted (the 403's was the first)", a)
	}
	if v, ok := h.kvOf(kvAutoWithdrawPending(h.pr(2).ID)); !ok || v != "9001" {
		t.Fatalf("pending record = %q, %v", v, ok)
	}
	if evs := h.events("review.auto_approval_withdraw_failed"); len(evs) != 1 {
		t.Fatalf("failed events = %+v, want only the 403's", evs)
	}
	cut := approvalEvents(t, h, 2, "review.auto_approval_withdraw_cut")
	if len(cut) != 1 || cut[0].Level != "info" || eventData(t, cut[0])["attempt"] != float64(2) {
		t.Fatalf("cut events = %+v, want one info event for attempt 2", cut)
	}
	g.onDismiss = nil
	h.gh.dismissErr = errUnprocessable
	h.advance(autoApproveRetry)
	h.tick()
	a := h.autoApproval(2)
	if a.State != store.AutoDismissed || a.EndedBy != store.AutoEndedMagnum {
		t.Fatalf("approval = %+v", a)
	}
	if hd := h.hold(2); hd.Held {
		t.Fatalf("hold = %+v", hd)
	}
	var attempts []float64
	for _, b := range approvalEvents(t, h, 2, "review.auto_approval_withdraw_begin") {
		attempts = append(attempts, eventData(t, b)["attempt"].(float64))
	}
	if !slices.Equal(attempts, []float64{1, 2, 3}) {
		t.Fatalf("withdrawal attempts begun = %v: the next daemon's began again with the cut one's number", attempts)
	}
	endsOfBegins(t, h, "review.auto_approval_withdraw_begin", "review.auto_approval_withdrawn", "review.auto_approval_withdraw_failed",
		"review.auto_approval_withdraw_cut")
}

// An approval the stop's grace runs out on stays posting and pending; the
// next daemon finds it on GitHub (resumeAutoPost) as magnum's own, its
// answer lost.
func TestAnApprovalPastTheStopsGraceIsFoundByTheNextDaemonAsItsOwn(t *testing.T) {
	h, _, g := newWriteHarness(t)
	h.gh.createErr = &github.APIError{Op: "review", Status: 400, Message: "Bad Request"}
	h.reviewedPR(2, "b1")
	h.tick()
	h.gh.createErr = nil
	h.e.writeGrace = 50 * time.Millisecond
	ctx, stop := context.WithCancel(h.ctx)
	defer stop()
	g.onCreate = func(rctx context.Context, _ github.RESTReview, _ error) (github.RESTReview, error) {
		stop()
		select {
		case <-rctx.Done():
		case <-time.After(10 * time.Second):
			t.Error("the grace never ended")
		}
		return github.RESTReview{}, fmt.Errorf("gh api: %w", rctx.Err())
	}
	h.advance(autoApproveRetry)
	h.e.autoApprove(ctx)
	if a := h.autoApproval(2); a.State != store.AutoPosting {
		t.Fatalf("approval = %+v, want it still posting", a)
	}
	if v, ok := h.kvOf(kvAutoApprovePending(h.pr(2).ID)); !ok || v != "b1" {
		t.Fatalf("pending record = %q, %v", v, ok)
	}
	cut := approvalEvents(t, h, 2, "review.auto_approve_cut")
	if len(cut) != 1 || cut[0].Level != "info" || eventData(t, cut[0])["attempt"] != float64(2) {
		t.Fatalf("cut events = %+v, want one info event for attempt 2", cut)
	}
	g.onCreate = nil
	h.gh.setReviewBody(2, 9001, "Approved.")
	h.tick()
	if a := h.autoApproval(2); a.State != store.AutoStanding || a.ReviewID != 9001 {
		t.Fatalf("approval = %+v", a)
	}
	evs := h.events("review.auto_approved")
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "(the first request's answer was lost)") {
		t.Fatalf("approved events = %+v", evs)
	}
	if hd := h.hold(2); hd.Held {
		t.Fatalf("hold = %+v", hd)
	}
	endsOfBegins(t, h, "review.auto_approve_begin", "review.auto_approved", "review.auto_approve_failed", "review.auto_approve_cut")
}

// workWhat lists the work in flight KVDaemonWork names.
func (h *harness) workWhat() []string {
	h.t.Helper()
	v, ok := h.kvOf(KVDaemonWork)
	if !ok {
		return nil
	}
	var m WorkMark
	if err := json.Unmarshal([]byte(v), &m); err != nil {
		h.t.Fatalf("%s = %q: %v", KVDaemonWork, v, err)
	}
	var out []string
	for _, w := range m.Work {
		out = append(out, w.What)
	}
	return out
}

// An approval and a withdrawal as the operator are work in flight while
// they go: the daemon's idle restart waits (restartBusy reads the same
// list) and the CLI's guards read them in KVDaemonWork; both are gone once
// the answer is recorded.
func TestARestartWaitsForAnApprovalOrAWithdrawalInFlight(t *testing.T) {
	h, plan, g := newWriteHarness(t)
	plan.set(2, cleanRound, blockingRound)
	var inProcess, marks [][]string
	note := func() {
		var names []string
		for _, w := range h.e.workInFlight() {
			names = append(names, w.What)
		}
		inProcess, marks = append(inProcess, names), append(marks, h.workWhat())
	}
	g.onCreate = func(_ context.Context, rev github.RESTReview, err error) (github.RESTReview, error) {
		note()
		return rev, err
	}
	g.onDismiss = func(_ context.Context, err error) error {
		note()
		return err
	}
	h.reviewedPR(2, "b1")
	h.tick()
	h.push(2, "b2")
	h.reviewTo(2, "b2")
	h.tick()
	want := [][]string{{"approval of talkable/talkable#2 as zhuravel"}, {"withdrawal of approval 9001 of talkable/talkable#2 by zhuravel"}}
	for _, got := range [][][]string{inProcess, marks} {
		if len(got) != 2 || !slices.Equal(got[0], want[0]) || !slices.Equal(got[1], want[1]) {
			t.Fatalf("during the writes: in process %q, marked %q; want %q", inProcess, marks, want)
		}
	}
	if a := h.autoApproval(2); a.State != store.AutoDismissed {
		t.Fatalf("approval = %+v", a)
	}
	if h.e.restartBusy(h.ctx) || h.workWhat() != nil {
		t.Fatalf("after the writes: busy %v, mark %q", h.e.restartBusy(h.ctx), h.workWhat())
	}
}

// A job on the heavy worker (a slot's provision here) is work in flight
// from the time it is queued until it ends: the daemon's idle restart waits
// for it and the CLI's guards read it in KVDaemonWork. A job the shutdown
// drops unrun, and a startup after a crash, leave no mark behind.
func TestARestartWaitsForAHeavyJobQueuedOrRunning(t *testing.T) {
	h := newHarness(t)
	var running bool
	var during []string
	if !h.e.enqueueHeavy("provision:talkable/talkable", func(ctx context.Context) error {
		running, during = h.e.restartBusy(ctx), h.workWhat()
		return nil
	}) {
		t.Fatal("not queued")
	}
	want := []string{"heavy job provision:talkable/talkable"}
	if !h.e.restartBusy(h.ctx) || !slices.Equal(h.workWhat(), want) {
		t.Fatalf("queued: busy %v, mark %q", h.e.restartBusy(h.ctx), h.workWhat())
	}
	h.e.drainHeavy(h.ctx)
	if !running || !slices.Equal(during, want) {
		t.Fatalf("running: busy %v, mark %q", running, during)
	}
	if h.e.restartBusy(h.ctx) || h.workWhat() != nil {
		t.Fatalf("ended: busy %v, mark %q", h.e.restartBusy(h.ctx), h.workWhat())
	}
	h.e.enqueueHeavy("evict:talkable/talkable", func(context.Context) error { return nil })
	h.e.dropHeavy()
	if h.e.restartBusy(h.ctx) || h.workWhat() != nil {
		t.Fatalf("dropped: busy %v, mark %q", h.e.restartBusy(h.ctx), h.workWhat())
	}
	if !h.e.enqueueHeavy("evict:talkable/talkable", func(context.Context) error { return nil }) {
		t.Fatal("a dropped job's key is still taken")
	}
	h.e.dropHeavy()
	if err := h.st.SetKV(h.ctx, KVDaemonWork, `{"work":[{"what":"heavy job reconcile"}]}`); err != nil { // a crashed daemon's
		t.Fatal(err)
	}
	h.startup() // queues its reconcile, which runs
	if h.workWhat() != nil {
		t.Fatalf("after the startup: mark %q", h.workWhat())
	}
}
