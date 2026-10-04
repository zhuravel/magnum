package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

func TestReviewQueuesForcedRequestAndPrintsPosition(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.pid = 4242
	h.onSleep = func(h *actHarness) {
		h.completePending(store.RequestDone, "queued talkable/talkable#5 (rereview_pending, forced), position 1")
	}

	code := h.cmd("review", "talkable#5", "--simplify", "--as", "zhuravel", "--fresh")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqReview {
		t.Fatalf("requests = %+v", reqs)
	}
	p := actDecode[engine.ReviewPayload](t, reqs[0].Payload)
	if p.Repo != "talkable/talkable" || p.Number != 5 || !p.Simplify || !p.Fresh || p.As != "zhuravel" || p.Again {
		t.Fatalf("payload = %+v", p)
	}
	if h.kicks != 1 {
		t.Errorf("kicks = %d", h.kicks)
	}
	actContains(t, h.out.String(), "talkable#5  Fix coupon export (@alice)", pr.URL, "state reviewed", "position 1")
}

func TestReviewImportsUnknownPRWithOneDetailsCall(t *testing.T) {
	h := newActHarness(t)
	h.gh.details[7] = github.PRDetails{NodeID: "PR_w7", Number: 7, Title: "Add widget", URL: "https://github.com/zhuravel/widgets/pull/7",
		AuthorLogin: "bob", AuthorType: "User", HeadRefName: "feature", BaseRefName: "main", State: "OPEN", HeadRefOid: "fff0001",
		UpdatedAt: h.now.Add(-time.Hour), ReviewRequests: []github.Reviewer{{Type: "User", Login: "zhuravel"}}}
	h.run.Rules = []execx.Rule{{Prefix: []string{"gh", "api", "repos/zhuravel/widgets"},
		Result: execx.Result{Stdout: []byte(`{"node_id":"R_widgets","full_name":"zhuravel/widgets","default_branch":"main"}`)}}}

	if code := h.cmd("review", "https://github.com/zhuravel/widgets/pull/7"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if len(h.gh.calls) != 1 || h.gh.calls[0][0] != 7 {
		t.Fatalf("details calls = %v", h.gh.calls)
	}
	repo, err := h.st.RepoByFullName(h.ctx, "zhuravel/widgets")
	if err != nil || repo.NodeID != "R_widgets" || repo.Mode != store.RepoModePerPR || repo.DefaultBranch != "main" || repo.FirstSyncedAt != nil {
		t.Fatalf("repo = %+v err %v", repo, err)
	}
	if repo.ClonePath != nil {
		t.Fatalf("clone path guessed as %q; the daemon discovers it", *repo.ClonePath)
	}
	pr, err := h.st.PRByRepoNumber(h.ctx, repo.ID, 7)
	if err != nil || pr.State != store.PRBaseline || pr.HeadSHA != "fff0001" || !pr.ReviewRequested || pr.Identity != "zhuravel" {
		t.Fatalf("pr = %+v err %v", pr, err)
	}
	evs, _ := h.st.EventsBySubject(h.ctx, "pr:zhuravel/widgets#7", 10)
	if len(evs) != 1 || evs[0].Kind != "pr.added" {
		t.Errorf("events = %+v", evs)
	}
	if reqs := h.requests(); len(reqs) != 1 {
		t.Fatalf("requests = %+v", reqs)
	}
	actContains(t, h.out.String(), "added to the registry from GitHub")
	actContains(t, h.errb.String(), "no daemon is running", "magnum daemon")
}

func TestReviewNoPostQueuesADryRunRound(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	if code := h.cmd("review", "5", "--no-post"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqReview {
		t.Fatalf("requests = %+v", reqs)
	}
	if p := actDecode[engine.ReviewPayload](t, reqs[0].Payload); !p.DryRun || p.Number != 5 {
		t.Fatalf("payload = %+v", p)
	}
	if !strings.Contains(string(reqs[0].Payload), `"dry_run":true`) {
		t.Fatalf("payload JSON = %s", reqs[0].Payload)
	}
}

func TestReviewNoPostWaitEndsWithTheDryRun(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.pid = 1
	step := 0
	h.onSleep = func(h *actHarness) {
		step++
		switch step {
		case 1:
			h.completePending(store.RequestDone, "queued talkable/talkable#5 (rereview_pending, forced, dry run: nothing is posted)")
		case 3:
			h.setPR(pr.ID, store.PRReviewing, nil)
		case 4:
			h.setPR(pr.ID, store.PRReviewed, nil)
			subject := "pr:talkable/talkable#5"
			if _, err := h.st.AppendEvent(h.ctx, store.Event{Level: "info", Subject: &subject, Kind: "round.dry_run",
				Message: "dry run at abc1234: would post CHANGES_REQUESTED (1 P1); nothing was posted (PR back to reviewed)"}); err != nil {
				t.Fatal(err)
			}
		case 20:
			t.Fatal("still following after the dry run ended")
		}
	}
	if code := h.cmd("review", "5", "--no-post", "--wait"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "would post CHANGES_REQUESTED", "nothing was posted")
}

func TestReviewDryRunChangesNothing(t *testing.T) {
	h := newActHarness(t)
	h.gh.details[9] = github.PRDetails{NodeID: "PR_9", Number: 9, Title: "T", State: "OPEN", HeadRefOid: "aaa"}
	h.run.Rules = []execx.Rule{{Prefix: []string{"gh", "api"}, Result: execx.Result{Stdout: []byte(`{"node_id":"R_x","full_name":"zhuravel/x","default_branch":"master"}`)}}}
	if code := h.cmd("review", "zhuravel/x#9", "--dry-run"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if _, err := h.st.RepoByFullName(h.ctx, "zhuravel/x"); err == nil {
		t.Error("dry run recorded the repository")
	}
	if len(h.requests()) != 0 || h.kicks != 0 {
		t.Errorf("dry run queued requests (%d) or kicked (%d)", len(h.requests()), h.kicks)
	}
	actContains(t, h.out.String(), "would be added from GitHub", `dry run: would queue request review {"repo":"zhuravel/x","number":9}`)
}

func TestReviewWaitFollowsTheRoundToThePostedReview(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRQueued)
	h.pid = 1
	step := 0
	h.onSleep = func(h *actHarness) {
		step++
		switch step {
		case 1:
			h.completePending(store.RequestDone, "queued talkable/talkable#5 (queued, forced), position 1")
		case 3:
			h.setPR(pr.ID, store.PRClaiming, nil)
		case 4:
			h.setPR(pr.ID, store.PRReviewing, nil)
			if _, err := h.st.CreateRun(h.ctx, store.Run{PRID: pr.ID, Round: 1, Role: store.RoleJudge, Kind: store.RunInitial,
				TargetSHA: pr.HeadSHA, State: store.RunWorking, ReviewerLogin: "talkable[bot]"}); err != nil {
				t.Fatal(err)
			}
		case 6:
			runs, _ := h.st.RunsByPR(h.ctx, pr.ID)
			_ = h.st.TransitionRun(h.ctx, runs[0].ID, nil, store.RunVerified, func(u *store.RunUpdate) {
				u.Set("review_url", "https://github.com/talkable/talkable/pull/5#pullrequestreview-1")
				u.Set("review_event", "COMMENTED")
			})
			h.setPR(pr.ID, store.PRReviewed, func(u *store.PRUpdate) {
				u.Set("reviewed_at", h.now)
				u.Set("reviewed_sha", pr.HeadSHA)
				u.Set("last_review_event", "COMMENTED")
			})
		}
	}
	if code := h.cmd("review", "5", "--wait"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "queued → claiming", "claiming → reviewing", "judge initial round 1: working",
		"reviewing → reviewed", "reviewed talkable#5: COMMENTED on abc1234 as talkable[bot]", "pullrequestreview-1")
}

func TestReviewWaitStopsOnNeedsAttention(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRQueued)
	h.pid = 1
	step := 0
	h.onSleep = func(h *actHarness) {
		step++
		if step == 1 {
			h.completePending(store.RequestDone, "queued")
		}
		if step == 3 {
			h.setPR(pr.ID, store.PRNeedsAttention, func(u *store.PRUpdate) { u.Set("last_error", "judge posted nothing after a nudge") })
		}
	}
	if code := h.cmd("review", "talkable#5", "--wait"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, h.errb.String(), "needs attention: judge posted nothing after a nudge", "fix: `magnum open talkable#5`")
}

func TestReviewRefusals(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRClosed)
	h.seedPR("talkable/talkable", 6, store.PRReviewing)
	cases := []struct {
		args []string
		code int
		want string
	}{
		{nil, 2, "which PR?"},
		{[]string{"1", "2"}, 2, "one PR at a time"},
		{[]string{"5"}, 1, "only open PRs are reviewed"},
		{[]string{"5", "--as", "nobody"}, 1, `unknown identity "nobody"`},
		{[]string{"owner/unwatched#3"}, 1, "owner/unwatched is not watched"},
		{[]string{"5", "--wait"}, 1, "only open PRs"},
	}
	for _, tc := range cases {
		h.errb.Reset()
		if code := h.cmd("review", tc.args...); code != tc.code || !strings.Contains(h.errb.String(), tc.want) {
			t.Errorf("review %v: exit %d, stderr %q; want %d and %q", tc.args, code, h.errb.String(), tc.code, tc.want)
		}
	}
	h.out.Reset()
	if code := h.cmd("review", "6"); code != 0 {
		t.Fatalf("in-flight review exit %d", code)
	}
	actContains(t, h.out.String(), "a round is already running (reviewing)", "magnum review talkable#6 --wait")
	if len(h.requests()) != 0 {
		t.Error("refused reviews queued requests")
	}
}

func TestReviewWaitNeedsADaemon(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	if code := h.cmd("review", "5", "--wait"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, h.errb.String(), "--wait and --focus need a running daemon", "magnum install")
}

// reviewRecordGH logs the PR reads that reach the fake GitHub client.
type reviewRecordGH struct {
	inner actGitHub
	log   *[]string
}

func (g reviewRecordGH) Details(ctx context.Context, owner, repo string, numbers []int) (map[int]github.PRDetails, []int, error) {
	*g.log = append(*g.log, fmt.Sprintf("details %s/%s#%v", owner, repo, numbers))
	return g.inner.Details(ctx, owner, repo, numbers)
}

// reviewFocusHerdr fails its first failFirst focus calls and logs them all.
type reviewFocusHerdr struct {
	*actFakeHerdr
	calls     []string
	failFirst int
}

func (f *reviewFocusHerdr) AgentFocus(ctx context.Context, target string) error {
	f.calls = append(f.calls, target)
	if len(f.calls) <= f.failFirst {
		return errors.New("herdr busy")
	}
	return f.actFakeHerdr.AgentFocus(ctx, target)
}

// reviewVerify plays the daemon finishing a round: the judge run is verified
// with a review URL and the PR is reviewed again.
func reviewVerify(h *actHarness, pr store.PR, runID, url string) {
	h.t.Helper()
	if runID != "" {
		if err := h.st.TransitionRun(h.ctx, runID, nil, store.RunVerified, func(u *store.RunUpdate) {
			u.Set("review_url", url)
			u.Set("review_event", "COMMENTED")
		}); err != nil {
			h.t.Fatal(err)
		}
	}
	h.setPR(pr.ID, store.PRReviewed, func(u *store.PRUpdate) {
		u.Set("reviewed_at", h.now)
		u.Set("reviewed_sha", pr.HeadSHA)
		u.Set("last_review_event", "COMMENTED")
	})
}

func TestReviewWaitPrintsWhyAQueuedPRStaysQueuedAndTimesOut(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRQueued)
	h.pid = 1
	gate := func(v string) {
		if v == "" {
			_ = h.st.DeleteKV(h.ctx, store.KVPRGate(pr.ID))
		} else if err := h.st.SetKV(h.ctx, store.KVPRGate(pr.ID), v); err != nil {
			t.Fatal(err)
		}
	}
	step := 0
	h.onSleep = func(h *actHarness) {
		step++
		switch step {
		case 1:
			h.completePending(store.RequestDone, "queued talkable/talkable#5 (queued, forced), position 1")
		case 3:
			gate("an agent is still working in the slot")
		case 4: // unchanged: printed once
			gate("an agent is still working in the slot")
		case 6:
			gate("max concurrent reviews reached")
		case 8:
			gate("")
		case 10:
			gate("max concurrent reviews reached") // back again after a clear: printed again
		}
	}
	code := h.cmd("review", "5", "--wait", "--timeout", "1m")
	if code != 1 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	out := h.out.String()
	actContains(t, out, "waiting: an agent is still working in the slot", "waiting: max concurrent reviews reached")
	if n := strings.Count(out, "waiting: an agent is still working in the slot"); n != 1 {
		t.Errorf("first gate printed %d times:\n%s", n, out)
	}
	if n := strings.Count(out, "waiting: max concurrent reviews reached"); n != 2 {
		t.Errorf("second gate printed %d times:\n%s", n, out)
	}
	actContains(t, h.errb.String(), "the round goes on", "magnum review talkable#5 --wait")
}

func TestReviewWaitTimeoutDefaultsToTwoHours(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRQueued)
	h.pid = 1
	h.d.Poll = 30 * time.Minute
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestDone, "queued") }
	before := h.now
	if code := h.cmd("review", "5", "--wait"); code != 1 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if waited := h.now.Sub(before); waited < 2*time.Hour || waited > 3*time.Hour {
		t.Errorf("gave up after %s, want about 2h", waited)
	}
	actContains(t, h.errb.String(), "the round goes on", "magnum review talkable#5 --wait")
}

func TestReviewWaitTimeoutZeroMeansNoLimit(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRQueued)
	h.pid = 1
	h.d.Poll = 30 * time.Minute
	step := 0
	h.onSleep = func(h *actHarness) {
		step++
		switch step {
		case 1:
			h.completePending(store.RequestDone, "queued")
		case 14: // 6.5 hours of polling later
			reviewVerify(h, pr, "", "")
		}
	}
	if code := h.cmd("review", "5", "--wait", "--timeout", "0"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "reviewed talkable#5: COMMENTED")
}

func TestReviewAttachFollowsTheRunAlreadyInFlight(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewing)
	run, err := h.st.CreateRun(h.ctx, store.Run{PRID: pr.ID, Round: 1, Role: store.RoleJudge, Kind: store.RunInitial,
		TargetSHA: pr.HeadSHA, State: store.RunWorking, ReviewerLogin: "talkable[bot]"})
	if err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(10 * time.Minute) // the attach comes after the judge run started
	step := 0
	h.onSleep = func(h *actHarness) {
		step++
		if step == 2 {
			reviewVerify(h, pr, run.ID, "https://github.com/talkable/talkable/pull/5#pullrequestreview-1")
		}
	}
	if code := h.cmd("review", "5", "--wait"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "a round is already running (reviewing)", "judge initial round 1: working",
		"reviewed talkable#5: COMMENTED on abc1234 as talkable[bot]", "pullrequestreview-1")
}

func TestReviewAttachWhileClaimingIgnoresThePreviousRound(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	oldURL, oldEvent := "https://github.com/talkable/talkable/pull/5#pullrequestreview-1", "COMMENTED"
	if _, err := h.st.CreateRun(h.ctx, store.Run{PRID: pr.ID, Round: 1, Role: store.RoleJudge, Kind: store.RunInitial,
		TargetSHA: pr.HeadSHA, State: store.RunVerified, ReviewerLogin: "talkable[bot]", ReviewURL: &oldURL, ReviewEvent: &oldEvent,
		CreatedAt: h.now.Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	h.setPR(pr.ID, store.PRClaiming, func(u *store.PRUpdate) {
		u.Set("reviewed_at", h.now.Add(-time.Hour))
		u.Set("reviewed_sha", pr.HeadSHA)
		u.Set("last_review_event", "COMMENTED")
	})
	var newRun store.Run
	step := 0
	h.onSleep = func(h *actHarness) {
		step++
		switch step {
		case 2:
			h.setPR(pr.ID, store.PRReviewing, nil)
			var err error
			if newRun, err = h.st.CreateRun(h.ctx, store.Run{PRID: pr.ID, Round: 2, Role: store.RoleJudge, Kind: store.RunRereview,
				TargetSHA: pr.HeadSHA, State: store.RunWorking, ReviewerLogin: "talkable[bot]"}); err != nil {
				t.Fatal(err)
			}
		case 4:
			reviewVerify(h, pr, newRun.ID, "https://github.com/talkable/talkable/pull/5#pullrequestreview-2")
		}
	}
	if code := h.cmd("review", "5", "--wait"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	out := h.out.String()
	actContains(t, out, "judge rereview round 2: working", "pullrequestreview-2")
	if strings.Contains(out, "pullrequestreview-1") || strings.Contains(out, "round 1") {
		t.Errorf("followed the finished round instead of the new one:\n%s", out)
	}
}

func TestReviewAttachToADryRunRoundEndsWithTheDryRun(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewing)
	step := 0
	h.onSleep = func(h *actHarness) {
		step++
		switch step {
		case 2:
			h.setPR(pr.ID, store.PRReviewed, nil)
			subject := "pr:talkable/talkable#5"
			if _, err := h.st.AppendEvent(h.ctx, store.Event{Level: "info", Subject: &subject, Kind: "round.dry_run",
				Message: "dry run at abc1234: would post COMMENTED; nothing was posted (PR back to reviewed)"}); err != nil {
				t.Fatal(err)
			}
		case 20:
			t.Fatal("still following after the dry run ended")
		}
	}
	if code := h.cmd("review", "5", "--wait"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "would post COMMENTED", "nothing was posted")
}

func TestReviewFreshFocusWaitsForTheNewJudgeSession(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRQueued)
	old, err := h.st.CreateSession(h.ctx, store.Session{PRID: pr.ID, Role: store.RoleJudge, State: store.SessionLive,
		AgentName: store.Ptr("judge-old"), HerdrPaneID: store.Ptr("p_old"), StartedAt: h.now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	h.pid = 1
	step := 0
	h.onSleep = func(h *actHarness) {
		step++
		switch step {
		case 1:
			h.completePending(store.RequestDone, "queued")
		case 3:
			h.setPR(pr.ID, store.PRClaiming, nil)
		case 5:
			h.setPR(pr.ID, store.PRReviewing, nil)
		case 7: // the round parks the old conversation and starts a new one
			if len(h.hd.focused) != 0 {
				t.Errorf("focused %v before the new judge session existed", h.hd.focused)
			}
			if err := h.st.TransitionSession(h.ctx, old.ID, nil, store.SessionParked, nil); err != nil {
				t.Fatal(err)
			}
			h.session(pr.ID, store.RoleJudge, store.SessionLive, "judge-new", "p_new", "w1")
		}
	}
	if code := h.cmd("review", "5", "--fresh", "--focus"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if len(h.hd.focused) != 1 || h.hd.focused[0] != "judge-new" {
		t.Fatalf("focused %v, want only judge-new", h.hd.focused)
	}
}

func TestReviewFocusWaitsForTheRoundToSelectItsSession(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRQueued)
	h.session(pr.ID, store.RoleJudge, store.SessionLive, "judge-1", "p1", "w1")
	h.pid = 1
	step := 0
	h.onSleep = func(h *actHarness) {
		step++
		switch step {
		case 1:
			h.completePending(store.RequestDone, "queued")
		case 4:
			if len(h.hd.focused) != 0 {
				t.Errorf("focused %v while the PR was still queued", h.hd.focused)
			}
			h.setPR(pr.ID, store.PRClaiming, nil)
		}
	}
	if code := h.cmd("review", "5", "--focus"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if step < 4 {
		t.Errorf("focused after %d polls, before the round started", step)
	}
	if len(h.hd.focused) != 1 || h.hd.focused[0] != "judge-1" {
		t.Fatalf("focused %v", h.hd.focused)
	}
}

func TestReviewFocusFailureIsRetriedThenFails(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewing)
	h.session(pr.ID, store.RoleJudge, store.SessionLive, "judge-1", "p1", "w1")
	hd := &reviewFocusHerdr{actFakeHerdr: h.hd, failFirst: 99}
	h.d.Herdr = hd
	if code := h.cmd("review", "5", "--focus"); code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, h.errb.String())
	}
	if len(hd.calls) != 3 {
		t.Errorf("focus attempts = %d, want 3", len(hd.calls))
	}
	actContains(t, h.errb.String(), "herdr busy")
}

func TestReviewFocusRetriesUntilItWorks(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewing)
	h.session(pr.ID, store.RoleJudge, store.SessionLive, "judge-1", "p1", "w1")
	hd := &reviewFocusHerdr{actFakeHerdr: h.hd, failFirst: 1}
	h.d.Herdr = hd
	if code := h.cmd("review", "5", "--focus"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if len(hd.calls) != 2 || len(h.reveals) != 1 {
		t.Errorf("focus attempts = %d, reveals = %d", len(hd.calls), len(h.reveals))
	}
	actContains(t, h.out.String(), "focused judge-1")
}

func TestReviewWaitFocusNeverSucceedingFailsTheExit(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewing)
	h.session(pr.ID, store.RoleJudge, store.SessionLive, "judge-1", "p1", "w1")
	hd := &reviewFocusHerdr{actFakeHerdr: h.hd, failFirst: 99}
	h.d.Herdr = hd
	step := 0
	h.onSleep = func(h *actHarness) {
		step++
		if step == 5 {
			reviewVerify(h, pr, "", "")
		}
	}
	if code := h.cmd("review", "5", "--focus", "--wait"); code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, h.errb.String())
	}
	if len(hd.calls) != 3 {
		t.Errorf("focus attempts = %d, want 3", len(hd.calls))
	}
	actContains(t, h.out.String(), "reviewed talkable#5: COMMENTED")
	actContains(t, h.errb.String(), "herdr busy")
}

func TestReviewDryRunIsHonouredBeforeAttaching(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewing)
	h.session(pr.ID, store.RoleJudge, store.SessionLive, "judge-1", "p1", "w1")
	if code := h.cmd("review", "5", "--dry-run", "--focus", "--wait"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if len(h.hd.focused) != 0 || len(h.reveals) != 0 || h.sleeps != 0 {
		t.Errorf("dry run focused %v, revealed %d, slept %d times", h.hd.focused, len(h.reveals), h.sleeps)
	}
	if len(h.requests()) != 0 {
		t.Error("dry run queued a request")
	}
	actContains(t, h.out.String(), "a round is already running (reviewing)", "dry run:", "nothing was changed")
}

func TestReviewShorthandKeepsTheResolvedRepository(t *testing.T) {
	h := newActHarness(t)
	if _, err := h.st.UpsertRepo(h.ctx, store.Repo{NodeID: "R_widgets", Owner: "zhuravel", Name: "widgets", Mode: store.RepoModePerPR}); err != nil {
		t.Fatal(err)
	}
	h.gh.details[7] = github.PRDetails{NodeID: "PR_w7", Number: 7, Title: "Add widget", AuthorLogin: "bob", AuthorType: "User",
		HeadRefName: "feature", BaseRefName: "main", State: "OPEN", HeadRefOid: "fff0001", UpdatedAt: h.now.Add(-time.Hour)}
	var log []string
	h.d.GitHub = func(id string) actGitHub {
		if id == "zhuravel" {
			return reviewRecordGH{inner: h.gh, log: &log}
		}
		return nil
	}
	if code := h.cmd("review", "widgets#7"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if len(log) != 1 || log[0] != "details zhuravel/widgets#[7]" {
		t.Fatalf("GitHub reads = %v", log)
	}
	repo, err := h.st.RepoByFullName(h.ctx, "zhuravel/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.PRByRepoNumber(h.ctx, repo.ID, 7); err != nil {
		t.Errorf("PR not recorded under zhuravel/widgets: %v", err)
	}
	if _, err := h.st.RepoByFullName(h.ctx, "talkable/widgets"); err == nil {
		t.Error("recorded a repository under the default owner")
	}
	reqs := h.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %+v", reqs)
	}
	if p := actDecode[engine.ReviewPayload](t, reqs[0].Payload); p.Repo != "zhuravel/widgets" || p.Number != 7 {
		t.Errorf("payload = %+v", p)
	}
}

func TestReviewResolveRefNamesTheResolvedRepositoryInItsErrors(t *testing.T) {
	h := newActHarness(t)
	for _, r := range []store.Repo{
		{NodeID: "R_w", Owner: "zhuravel", Name: "widgets", Mode: store.RepoModePerPR},
		{NodeID: "R_g", Owner: "stranger", Name: "gadgets", Mode: store.RepoModePerPR},
	} {
		if _, err := h.st.UpsertRepo(h.ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	tg, err := h.d.resolveRef(h.ctx, "widgets#9")
	if !errors.Is(err, store.ErrNotFound) || !strings.Contains(err.Error(), "zhuravel/widgets#9 is not in the registry yet") || tg.Repo.FullName() != "zhuravel/widgets" {
		t.Errorf("watched shorthand: repo %q, err %v", tg.Repo.FullName(), err)
	}
	_, err = h.d.resolveRef(h.ctx, "gadgets#3")
	if !errors.Is(err, store.ErrNotFound) || !strings.Contains(err.Error(), "stranger/gadgets is not watched") {
		t.Errorf("unwatched shorthand: %v", err)
	}
	_, err = h.d.resolveRef(h.ctx, "talkable/nowhere#3")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown repository: %v", err)
	}
}

func TestReviewPreparesTheIdentityBeforeReadingGitHub(t *testing.T) {
	h := newActHarness(t)
	h.gh.details[7] = github.PRDetails{NodeID: "PR_w7", Number: 7, Title: "Add widget", State: "OPEN", HeadRefOid: "fff0001"}
	h.run.Rules = []execx.Rule{{Prefix: []string{"gh", "api", "repos/zhuravel/widgets"},
		Result: execx.Result{Stdout: []byte(`{"node_id":"R_widgets","full_name":"zhuravel/widgets","default_branch":"main"}`)}}}
	var log []string
	h.d.GitHub = func(id string) actGitHub {
		if id == "zhuravel" {
			return reviewRecordGH{inner: h.gh, log: &log}
		}
		return nil
	}
	h.d.PrepareIdentity = func(_ context.Context, id string) error {
		log = append(log, "prepare "+id)
		return nil
	}
	if code := h.cmd("review", "zhuravel/widgets#7"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if strings.Join(log, "|") != "prepare zhuravel|details zhuravel/widgets#[7]" {
		t.Fatalf("order = %v", log)
	}
}

func TestReviewPrepareIdentityFailureFailsTheCommand(t *testing.T) {
	h := newActHarness(t)
	h.gh.details[7] = github.PRDetails{NodeID: "PR_w7", Number: 7, State: "OPEN", HeadRefOid: "fff0001"}
	h.d.PrepareIdentity = func(context.Context, string) error { return errors.New("app token: installation not found") }
	if code := h.cmd("review", "zhuravel/widgets#7"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, h.errb.String(), "zhuravel", "installation not found")
	if len(h.gh.calls) != 0 || len(h.requests()) != 0 {
		t.Errorf("read GitHub (%v) or queued (%d) after the identity failed", h.gh.calls, len(h.requests()))
	}
}

func TestReviewAgainIsHiddenButStillAccepted(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.setPR(pr.ID, store.PRReviewed, func(u *store.PRUpdate) {
		u.Set("reviewed_sha", pr.HeadSHA)
		u.Set("last_review_event", "COMMENTED")
	})
	if code := h.cmd("review", "--help"); code != 0 {
		t.Fatalf("help exit %d", code)
	}
	if strings.Contains(h.out.String(), "--again") || strings.Contains(reviewUsage, "--again") {
		t.Errorf("--again is still advertised:\n%s", h.out.String())
	}
	for _, args := range [][]string{{"5"}, {"5", "--again"}} {
		h.out.Reset()
		if code := h.cmd("review", args...); code != 0 {
			t.Fatalf("review %v: exit %d: %s", args, code, h.errb.String())
		}
		actContains(t, h.out.String(), "head abc1234 was already reviewed (COMMENTED); reviewing it again")
		h.completePending(store.RequestFailed, "reset") // so the next round may be queued
		h.setPR(pr.ID, store.PRReviewed, nil)
	}
	reqs := h.requests()
	if len(reqs) != 2 || actDecode[engine.ReviewPayload](t, reqs[0].Payload).Again || !actDecode[engine.ReviewPayload](t, reqs[1].Payload).Again {
		t.Errorf("payloads = %s / %s", reqs[0].Payload, reqs[1].Payload)
	}
}
