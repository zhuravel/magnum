package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/identity"
	"github.com/zhuravel/magnum/internal/store"
)

// The pending records of the operator's approval writes
// (autoapprove_pending.go) end with the request they belong to, each at its
// own place (spec 07 of the 2026-10-23 run: the analyst removed each of five
// cleanups in a scratch copy and the suite stayed green). A record that
// outlives its request makes a later approval of the head by hand look like
// magnum's own.

var (
	errBadGateway = &github.APIError{Op: "review", Status: 502, Message: "Bad Gateway"}
	errTimedOut   = fmt.Errorf("gh api: %w", context.DeadlineExceeded)
)

// postsFailingUnanswered runs PR #2's clean review at b1 through all three
// attempts of its automatic approval, each ending with err (an answer
// magnum never got, the post never reached GitHub), and returns the harness.
func postsFailingUnanswered(t *testing.T, err error) *harness {
	t.Helper()
	h, _ := newAutoHarness(t)
	h.gh.createErr = err
	h.reviewedPR(2, "b1")
	h.tick()
	for i := 1; i < autoApproveAttempts; i++ {
		if a := h.autoApproval(2); a.State != store.AutoFailed || a.Attempts != i {
			t.Fatalf("approval after attempt %d = %+v", i, a)
		}
		if v, ok := h.kvOf(kvAutoApprovePending(h.pr(2).ID)); !ok || v != "b1" {
			t.Fatalf("pending record after attempt %d = %q, %v: an unanswered request stays pending", i, v, ok)
		}
		h.advance(autoApproveRetry)
		h.tick()
	}
	return h
}

// An approval whose last attempt ended unanswered (a 5xx, a timeout) leaves
// no pending record: nothing settles it now, and a record kept would make
// an approval of the head found later look like this one's.
func TestAnApprovalOutOfAttemptsLeavesNoPendingRecord(t *testing.T) {
	for name, err := range map[string]error{"a bad gateway": errBadGateway, "a timeout": errTimedOut} {
		t.Run(name, func(t *testing.T) {
			h := postsFailingUnanswered(t, err)
			if a := h.autoApproval(2); a.State != store.AutoFailed || a.Attempts != autoApproveAttempts {
				t.Fatalf("approval after the last attempt = %+v", a)
			}
			if v, ok := h.kvOf(kvAutoApprovePending(h.pr(2).ID)); ok {
				t.Fatalf("the last attempt left its pending record: %q", v)
			}
		})
	}
}

// The path the cleanup above protects: three unanswered attempts, then the
// operator approves the same head by hand, then a later round of that head
// (a new run: the failed row does not block it). Their approval is theirs:
// auto-approval of the PR stops, and the approval is not adopted as
// magnum's, which a later round with findings would dismiss.
func TestAnApprovalByHandAfterThreeUnansweredAttemptsIsNotAdoptedAsMagnums(t *testing.T) {
	h := postsFailingUnanswered(t, errBadGateway)
	h.gh.createErr = nil
	h.gh.addReview(2, github.Review{DatabaseID: 7001, State: "APPROVED", Body: "Approved.", URL: prURL2 + "#pullrequestreview-7001",
		CommitOid: "b1", AuthorLogin: "zhuravel", AuthorType: "User", SubmittedAt: h.clock.Now()})
	h.advance(time.Hour)
	h.reviewAgain(2)
	h.tick()
	if hd := h.hold(2); !hd.Held || !strings.Contains(hd.Reason, "you approved it by hand") {
		t.Fatalf("hold = %+v, want auto-approval stopped by the approval by hand", hd)
	}
	if live, ok, err := h.st.LiveAutoApproval(h.ctx, h.pr(2).ID); err != nil || ok {
		t.Fatalf("the operator's approval was adopted as magnum's: %+v %v", live, err)
	}
	if evs := h.events("review.auto_approved"); len(evs) != 0 {
		t.Fatalf("review.auto_approved events %+v", evs)
	}
}

// A post still in doubt when the daemon restarted, and that GitHub's whole
// list does not hold, fails its row and drops its pending record
// (resumeAutoPost): the request never reached GitHub.
func TestARestartThatFindsNoApprovalOnGitHubDropsThePendingRecord(t *testing.T) {
	h := stoppedDuringAPost(t)
	pid := h.pr(2).ID
	if v, ok := h.kvOf(kvAutoApprovePending(pid)); !ok || v != "b1" {
		t.Fatalf("pending record after the stop = %q, %v", v, ok)
	}
	h.tick() // the next daemon: GitHub's whole list has no approval of b1
	if a := h.autoApproval(2); a.State != store.AutoFailed || !strings.Contains(a.Error, "the daemon stopped before GitHub answered") {
		t.Fatalf("approval = %+v", a)
	}
	if v, ok := h.kvOf(kvAutoApprovePending(pid)); ok {
		t.Fatalf("the settled post left its pending record: %q", v)
	}
	// Its row ends failed with an event, as a post GitHub refused does, naming
	// the attempt the stop cut (the first was refused).
	failed := approvalEvents(t, h, 2, "review.auto_approve_failed")
	if len(failed) != 2 || failed[1].Level != "warn" || eventData(t, failed[1])["attempt"] != float64(2) ||
		!strings.Contains(failed[1].Message, "the daemon stopped before GitHub answered") ||
		!strings.Contains(failed[1].Message, "tried again in 5m") {
		t.Fatalf("review.auto_approve_failed events = %+v, want a second one for attempt 2", failed)
	}
	if cut := approvalEvents(t, h, 2, "review.auto_approve_cut"); len(cut) != 1 {
		t.Fatalf("review.auto_approve_cut events = %+v", cut)
	}
	endsOfBegins(t, h, "review.auto_approve_begin", "review.auto_approved", "review.auto_approve_failed", "review.auto_approve_cut")
	// The next attempt begins with the next number.
	h.advance(autoApproveRetry)
	h.tick()
	var attempts []float64
	for _, b := range approvalEvents(t, h, 2, "review.auto_approve_begin") {
		attempts = append(attempts, eventData(t, b)["attempt"].(float64))
	}
	if !slices.Equal(attempts, []float64{1, 2, 3}) {
		t.Fatalf("approval attempts begun = %v", attempts)
	}
}

// endsOfBegins fails for each begin event of PR #2 (kind begin) that no end
// event of ends names: the same approval and attempt. A write the stop cut,
// and a row the next daemon failed, end their begin as a refusal does.
func endsOfBegins(t *testing.T, h *harness, begin string, ends ...string) {
	t.Helper()
	for _, b := range approvalEvents(t, h, 2, begin) {
		bd, n := eventData(t, b), 0
		for _, kind := range ends {
			for _, e := range approvalEvents(t, h, 2, kind) {
				if ed := eventData(t, e); ed["approval_id"] == bd["approval_id"] && ed["attempt"] == bd["attempt"] {
					n++
				}
			}
		}
		if n == 0 {
			t.Errorf("%s event %q (approval %v, attempt %v) has no end event among %v", begin, b.Message, bd["approval_id"], bd["attempt"], ends)
		}
	}
}

// stallGH is the fake GitHub whose approval, once stalled is set, hangs
// until the stop's grace cancels it and answers with the cancellation,
// having reached GitHub never.
type stallGH struct {
	*fakeGH
	stalled atomic.Bool
}

func (g *stallGH) CreateReview(ctx context.Context, owner, repo string, number int, commitID, event, body string) (github.RESTReview, error) {
	if !g.stalled.Load() {
		return g.fakeGH.CreateReview(ctx, owner, repo, number, commitID, event, body)
	}
	<-ctx.Done()
	return github.RESTReview{}, fmt.Errorf("gh api: %w", ctx.Err())
}

// stoppedDuringAPost leaves PR #2's automatic approval of b1 posting and
// pending: the stop came as its retry went out, and the grace ran out on a
// request that never reached GitHub.
func stoppedDuringAPost(t *testing.T) *harness {
	t.Helper()
	g := &stallGH{}
	h, _ := newAutoHarness(t, func(h *harness) {
		g.fakeGH = h.gh
		h.d.GitHub = func(id string) GitHub {
			if id == "zhuravel" {
				return g
			}
			return nil
		}
	})
	h.gh.createErr = &github.APIError{Op: "review", Status: 400, Message: "Bad Request"}
	h.reviewedPR(2, "b1") // the first attempt fails, refused: the row goes failed
	h.gh.createErr = nil
	h.e.writeGrace = 20 * time.Millisecond
	g.stalled.Store(true)
	ctx, stop := context.WithCancel(h.ctx)
	defer stop()
	go func() { // the stop comes as the post goes out
		for len(h.e.workInFlight()) == 0 {
			time.Sleep(time.Millisecond)
		}
		stop()
	}()
	h.advance(autoApproveRetry)
	h.e.autoApprove(ctx)
	g.stalled.Store(false)
	if a := h.autoApproval(2); a.State != store.AutoPosting {
		t.Fatalf("approval after the stop = %+v, want it posting", a)
	}
	return h
}

// A withdrawal in doubt (a dismissal that went out unanswered) when the PR
// is merged or closed ends with the row: the pending record goes with it.
func TestAMergedPRsWithdrawalInDoubtDropsItsPendingRecord(t *testing.T) {
	h := approvedThenBlocked(t, errBadGateway, false)
	pid := h.pr(2).ID
	if a := h.autoApproval(2); a.State != store.AutoDismissing {
		t.Fatalf("approval = %+v", a)
	}
	if v, ok := h.kvOf(kvAutoWithdrawPending(pid)); !ok || v != "9001" {
		t.Fatalf("pending record = %q, %v", v, ok)
	}
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}) // #2 left the OPEN list
	h.gh.closed[2] = "MERGED"
	for range 3 {
		h.advance(30 * time.Second)
		h.tick()
	}
	if a := h.autoApproval(2); a.State != store.AutoDismissed || a.EndedBy != store.AutoEndedClosed {
		t.Fatalf("approval = %+v, want it ended with its PR", a)
	}
	if v, ok := h.kvOf(kvAutoWithdrawPending(pid)); ok {
		t.Fatalf("the ended withdrawal left its pending record: %q", v)
	}
}

// A withdrawal that ran out of attempts (all unanswered) ends failed and
// leaves no pending record; before the last attempt it stays pending.
func TestAWithdrawalOutOfAttemptsLeavesNoPendingRecord(t *testing.T) {
	h := approvedThenBlocked(t, errBadGateway, false)
	pid := h.pr(2).ID
	for i := 1; i < autoApproveAttempts; i++ {
		if a := h.autoApproval(2); a.State != store.AutoDismissing || a.Attempts != i {
			t.Fatalf("approval after attempt %d = %+v", i, a)
		}
		if v, ok := h.kvOf(kvAutoWithdrawPending(pid)); !ok || v != "9001" {
			t.Fatalf("pending record after attempt %d = %q, %v: an unanswered request stays pending", i, v, ok)
		}
		h.advance(autoApproveRetry)
		h.tick()
	}
	if a := h.autoApproval(2); a.State != store.AutoFailed || a.Attempts != autoApproveAttempts {
		t.Fatalf("approval after the last attempt = %+v", a)
	}
	if v, ok := h.kvOf(kvAutoWithdrawPending(pid)); ok {
		t.Fatalf("the last attempt left its pending record: %q", v)
	}
}

// checkHookIdentity runs onCheck before each health check of the identity
// it wraps (the start's identity warm-up).
type checkHookIdentity struct {
	identity.Source
	onCheck func()
}

func (c checkHookIdentity) Check(ctx context.Context) (identity.Report, error) {
	c.onCheck()
	return c.Source.Check(ctx)
}

// The first thing a start does about the work in flight is to drop a
// crashed daemon's mark (daemon.work), before it recovers rows and queues
// any job that would rewrite it: the CLI's guards read the mark of a daemon
// that is starting, and a stale one makes them wait for work that is gone.
func TestAStartDropsACrashedDaemonsWorkMarkBeforeItDoesAnythingElse(t *testing.T) {
	h := newHarness(t)
	if err := h.st.SetKV(h.ctx, KVDaemonWork, `{"work":[{"what":"heavy job provision:talkable/talkable"}]}`); err != nil {
		t.Fatal(err)
	}
	var first []string
	var seen bool
	h.e.d.Identities["zhuravel"] = checkHookIdentity{Source: h.e.d.Identities["zhuravel"], onCheck: func() { // the start's identity warm-up
		if !seen {
			seen, first = true, h.workWhat()
		}
	}}
	h.e.startup(h.ctx)
	h.settle()
	if !seen {
		t.Fatal("the start checked no identity: the test sees nothing")
	}
	if first != nil {
		t.Fatalf("the crashed daemon's mark %q was still there when the start went on", first)
	}
}
