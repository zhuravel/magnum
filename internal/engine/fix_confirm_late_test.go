package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/steps"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// fixFirstPush is the fixer's first push of two (fixPushed is its last).
const fixFirstPush = "a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0"

// outage makes GitHub answer nothing: its reads of the PR's head, its
// listing, the threads and the conversation, and `git ls-remote`; back
// undoes it.
func outage(h *harness, git *fixGit) (back func()) {
	h.gh.mu.Lock()
	h.gh.detailsErr, h.gh.radarErr, h.gh.threadsErr, h.gh.prCommentsErr = ghDown(), ghDown(), ghDown(), ghDown()
	h.gh.mu.Unlock()
	git.mu.Lock()
	git.lsErr = gitDown()
	git.mu.Unlock()
	return func() {
		h.gh.mu.Lock()
		h.gh.detailsErr, h.gh.radarErr, h.gh.threadsErr, h.gh.prCommentsErr = nil, nil, nil, nil
		h.gh.mu.Unlock()
	}
}

// confirmingFix runs a fix on PR #2 whose fixer pushes report's commits
// (its last, fixPushed, its HEAD) and whose settle GitHub then never
// answers: the loop stays in its settle, confirming the push, past the
// settle's own retries. before runs once the push is on GitHub, before the
// outage. It returns the outage's end.
func confirmingFix(t *testing.T, pushed, needs string, before func(h *harness)) (*harness, *fixGit, store.PR, func()) {
	t.Helper()
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	var back func()
	fixerBy(h, git, func(run store.Run) (string, string) {
		defer func() {
			if before != nil {
				before(h)
			}
			back = outage(h, git)
		}()
		return fixPushed, fixReport(run.ID, pushed, fixReplied, needs)
	})
	h.fix(FixPayload{})
	h.handle()
	if st, ok := h.fixStateOf(2); !ok || st.Confirm == nil {
		t.Fatalf("fix state = %+v, %v; want the loop confirming its push", st, ok)
	}
	return h, git, pr, back
}

// A fixer's push of comments, whitespace or docs alone that failed as
// unconfirmed is settled late as any push, with its forced re-review: the
// poll that finds it settles it as a trivial push first (reviewed at the
// head, no round), which is no review of it.
func TestALateSettleOfATrivialPushForcesItsReReview(t *testing.T) {
	h, _, pr := unconfirmedFix(t)
	h.gh.mu.Lock()
	if h.gh.files == nil {
		h.gh.files = map[string][]github.FileDelta{}
	}
	h.gh.files[fixHead+"..."+fixPushed] = docsOnly
	h.gh.compare[fixHead+"..."+fixPushed] = github.CompareStats{Commits: 1}
	h.gh.mu.Unlock()
	h.gh.setHead("talkable/talkable", 2, fixPushed)
	if err := h.e.poll(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if n := len(trivialEvents(t, h)); n != 1 {
		t.Fatalf("pr.trivial_delta events = %d, want the poll's", n)
	}
	h.wantPushedSettle(pr, 1)
	if n := len(h.fixEvents("fix.push_confirmed_late")); n != 1 {
		t.Errorf("fix.push_confirmed_late events = %d, want 1", n)
	}
}

// The fixer pushed two commits: GitHub listing the first as the PR's head
// says the last is not there yet, never that the push failed, so a loop
// still in its settle waits, and so does an unconfirmed fix; the last push
// settles either. A commit the fixer never pushed fails a loop still in its
// settle at once, as unconfirmed.
func TestAnEarlierPushOfTheFixerIsNotYetItsHead(t *testing.T) {
	storetest.Parallel(t)
	const pushed = "a0a0a0a First\nb2b2b2b Second"
	t.Run("a loop still in its settle", func(t *testing.T) {
		h, _, pr, back := confirmingFix(t, pushed, "none", nil)
		back()
		h.gh.mu.Lock()
		h.gh.radarErr = nil
		h.gh.mu.Unlock()
		h.gh.setHead("talkable/talkable", 2, fixFirstPush)
		if err := h.e.poll(h.ctx); err != nil {
			t.Fatal(err)
		}
		h.settle()
		if st, ok := h.fixStateOf(2); !ok || st.Confirm == nil || len(h.fixEvents("fix.failed")) != 0 {
			t.Fatalf("fix state = %+v, %v; want the loop still confirming its last push", st, ok)
		}
		h.gh.setHead("talkable/talkable", 2, fixPushed)
		if err := h.e.poll(h.ctx); err != nil {
			t.Fatal(err)
		}
		h.settle()
		h.wantPushedSettle(pr, 0)
	})
	t.Run("an unconfirmed fix", func(t *testing.T) {
		h, git := fixHarness(t)
		pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
		git.lostPush = true
		fixerBy(h, git, func(run store.Run) (string, string) { return fixPushed, fixReport(run.ID, pushed, fixReplied, "none") })
		h.fix(FixPayload{})
		h.handle()
		h.gh.setHead("talkable/talkable", 2, fixFirstPush)
		if err := h.e.poll(h.ctx); err != nil {
			t.Fatal(err)
		}
		h.settle()
		if rec, ok := h.fixPushOf(2); !ok || rec.Kind != fixPushUnconfirmed {
			t.Fatalf("the record = %+v, %v; want it kept while GitHub shows the first push", rec, ok)
		}
		h.gh.setHead("talkable/talkable", 2, fixPushed)
		if err := h.e.poll(h.ctx); err != nil {
			t.Fatal(err)
		}
		h.settle()
		h.wantPushedSettle(pr, 1)
	})
	t.Run("a commit the fixer never pushed", func(t *testing.T) {
		h, _, _, back := confirmingFix(t, pushed, "none", nil)
		back()
		h.gh.setHead("talkable/talkable", 2, fixPushed2)
		if err := h.e.poll(h.ctx); err != nil {
			t.Fatal(err)
		}
		h.settle()
		if _, ok := h.fixStateOf(2); ok {
			t.Error("the loop stays in its settle")
		}
		ev, ok := h.fixEvent("fix.failed")
		if !ok || !strings.Contains(ev.Message, "push not confirmed on GitHub: the PR's head is c3c3c3c, not the pushed b2b2b2b") {
			t.Errorf("fix.failed = %+v, %v", ev, ok)
		}
		if data := h.fixData("fix.failed"); data["kind"] != fixPushUnconfirmed {
			t.Errorf("fix.failed data = %v, want the kind %s", data, fixPushUnconfirmed)
		}
		h.wantState(2, store.PRRereviewPending) // the head GitHub shows waits for its review
	})
}

// The late settle's calls are toasted, as a settle's are, though the fix's
// failure toasted already for the same loop and cycle.
func TestTheLateSettlesToastIsNotTheFailuresToast(t *testing.T) {
	h, _, _ := unconfirmedFixWith(t, "- 2102 P2 app/lib/x.rb:7: contested")
	h.wantToasts("needs attention", 1)
	h.gh.setHead("talkable/talkable", 2, fixPushed)
	if err := h.e.poll(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if n := len(h.fixEvents("fix.push_confirmed_late")); n != 1 {
		t.Fatalf("fix.push_confirmed_late events = %d, want 1", n)
	}
	h.wantToasts("needs attention", 2)
}

// No loop stays in its settle without the record of the push it confirms:
// one whose record is gone ends at the next tick, its run failed, and one
// whose record names a run that is gone ends once, never at every tick.
func TestAConfirmingLoopNeverOutlivesItsRecord(t *testing.T) {
	storetest.Parallel(t)
	wantEnded := func(t *testing.T, h *harness, pr store.PR) {
		t.Helper()
		if _, ok := h.fixStateOf(2); ok {
			t.Fatal("the loop stays in its settle")
		}
		if n := len(h.fixEvents("fix.failed")); n != 1 {
			t.Errorf("fix.failed events = %d, want 1", n)
		}
		if r := h.fixRuns(pr.ID)[0]; r.State != store.RunFailed {
			t.Errorf("fix run %s, want it failed", r.State)
		}
		if _, ok := h.fixPushOf(2); ok {
			t.Error("the record stays")
		}
		h.advance(time.Minute)
		_ = h.e.Tick(h.ctx)
		h.settle()
		if n := len(h.fixEvents("fix.failed")); n != 1 {
			t.Errorf("fix.failed events after another tick = %d, want 1", n)
		}
	}
	t.Run("its record is gone", func(t *testing.T) {
		h, _, pr, _ := confirmingFix(t, "b2b2b2b Fix", "none", nil)
		if err := h.st.DeleteKV(h.ctx, kvPRFixPush(pr.ID)); err != nil {
			t.Fatal(err)
		}
		h.advance(time.Minute)
		_ = h.e.Tick(h.ctx)
		h.settle()
		wantEnded(t, h, pr)
	})
	t.Run("its record names a run that is gone", func(t *testing.T) {
		h, _, pr, _ := confirmingFix(t, "b2b2b2b Fix", "none", nil)
		rec, _ := h.fixPushOf(2)
		rec.Run = "r-gone"
		if err := writeKVJSON(h.ctx, h.e, kvPRFixPush(pr.ID), "fix push", rec); err != nil {
			t.Fatal(err)
		}
		h.advance(fixConfirmCap)
		_ = h.e.Tick(h.ctx)
		h.settle()
		wantEnded(t, h, pr)
	})
}

// An outage that cut the settle's look for the author's replies without
// the fix marker does not lose it: the settle at a later tick looks, and
// so does a late settle.
func TestALaterSettleNamesTheUnmarkedRepliesTheOutageHid(t *testing.T) {
	storetest.Parallel(t)
	reply := func(h *harness) {
		h.gh.mu.Lock()
		h.gh.prComments = map[int][]github.PRComment{2: {{ID: 7102, URL: prPage + "#issuecomment-7102", Body: "Won't fix: the flag goes.",
			CreatedAt: h.clock.Now(), AuthorLogin: "zhuravel", AuthorType: "User"}}}
		h.gh.mu.Unlock()
	}
	const named = "claude-fix: 1 reply by the PR's author since the fixer started carries no fix marker, so magnum reads it as the author's own: " +
		prPage + "#issuecomment-7102"
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "the ticks' settle", true: "a late settle"}[late], func(t *testing.T) {
			h, _, pr, back := confirmingFix(t, "b2b2b2b Fix", "none", reply)
			if _, ok := h.fixEvent("fix.unmarked"); ok {
				t.Fatal("fix.unmarked during the outage")
			}
			if late {
				h.advance(fixConfirmCap)
				_ = h.e.Tick(h.ctx)
				h.settle()
				if _, ok := h.fixStateOf(2); ok {
					t.Fatal("the loop outlived the cap")
				}
			}
			back()
			if err := h.e.poll(h.ctx); err != nil {
				t.Fatal(err)
			}
			h.settle()
			h.wantPushedSettle(pr, map[bool]int{false: 0, true: 1}[late])
			if evs := h.fixEvents("fix.unmarked"); len(evs) != 1 || evs[0].Message != named {
				t.Errorf("fix.unmarked events = %+v, want one: %q", evs, named)
			}
		})
	}
}

// reviewOf records magnum's posted review of head on PR pr, now.
func reviewOf(h *harness, pr store.PR, head string) {
	h.t.Helper()
	if _, err := h.st.DB().ExecContext(h.ctx, `INSERT INTO runs (id, pr_id, round, role, kind, target_sha, identity, reviewer_login,
  state, outcome, review_id, review_url, prompt_text, result_json, created_at)
  VALUES ('run-review-3', ?, 2, 'codex-judge', 'rereview', ?, ?, 'talkable[bot]', 'verified', 'posted', 102, ?, 'p', '{"event":"COMMENTED","findings":{}}', ?)`,
		pr.ID, head, pr.Identity, fixReviewURL, store.FormatTime(h.clock.Now())); err != nil {
		h.t.Fatal(err)
	}
}

// An unconfirmed fix is settled late only on its PR as the failure left
// it: a loop that runs on the PR, a PR no longer open, a review of the push
// posted since, or a day without the push on GitHub drop its record, and
// no settle follows.
func TestALateSettleLeavesAPRThatMovedOn(t *testing.T) {
	storetest.Parallel(t)
	for _, tc := range []struct {
		name  string
		moved func(h *harness, pr store.PR)
	}{
		{name: "a loop runs", moved: func(h *harness, pr store.PR) {
			if err := h.e.setFixState(h.ctx, pr.ID, FixState{Cycle: 1, Cap: 3, Phase: FixPhaseFixing, Report: "/r/other.md", StartedAt: h.clock.Now()}); err != nil {
				h.t.Fatal(err)
			}
		}},
		{name: "a review of the push since", moved: func(h *harness, pr store.PR) {
			h.advance(time.Minute)
			reviewOf(h, pr, fixPushed)
		}},
		{name: "a day without the push", moved: func(h *harness, pr store.PR) {
			h.advance(fixPushKeep + time.Minute)
			_ = h.e.Tick(h.ctx)
			h.settle()
			if _, ok := h.fixPushOf(2); ok {
				h.t.Fatal("the record outlived a day")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, pr := unconfirmedFix(t)
			tc.moved(h, pr)
			h.gh.setHead("talkable/talkable", 2, fixPushed)
			if err := h.e.poll(h.ctx); err != nil {
				t.Fatal(err)
			}
			h.settle()
			if n := len(h.fixEvents("fix.push_confirmed_late")); n != 0 {
				t.Errorf("fix.push_confirmed_late events = %d, want none", n)
			}
			if _, ok := h.fixPushOf(2); ok {
				t.Error("the record stays")
			}
			if r := h.fixRuns(pr.ID)[0]; r.State != store.RunFailed {
				t.Errorf("fix run %s, want it still failed", r.State)
			}
		})
	}
	t.Run("a PR no longer open", func(t *testing.T) {
		h, _, pr := unconfirmedFix(t)
		repo, err := h.st.RepoByID(h.ctx, pr.RepoID)
		if err != nil {
			t.Fatal(err)
		}
		cur := h.pr(2)
		cur.GHState = store.GHClosed
		row, ok := h.e.fixPushes(h.ctx, repo)[pr.ID]
		if !ok {
			t.Fatal("no record")
		}
		h.e.decidePush(h.ctx, repo, cur, row, fixPushed, true)
		if _, ok := h.fixPushOf(2); ok {
			t.Error("the record stays for a closed PR")
		}
		if n := len(h.fixEvents("fix.push_confirmed_late")); n != 0 {
			t.Errorf("fix.push_confirmed_late events = %d, want none", n)
		}
	})
}

// A restart after the settle of a confirmed push began leaves it to the
// poll, which ends it: the start fails no such loop, and nothing is done
// twice.
func TestARestartAfterTheSettleOfAConfirmedPushBeganResumesIt(t *testing.T) {
	h, _, pr, back := confirmingFix(t, "b2b2b2b Fix", "none", nil)
	back()
	crash := steps.WithFailpoint(h.ctx, func(_, name string, at steps.Point) error {
		if name == "fix.push_settle" && at == steps.BeforeRun {
			return errCrash
		}
		return nil
	})
	_ = h.e.poll(crash)
	h.settle()
	if n := len(h.fixEvents("fix.push_confirmed")); n != 1 {
		t.Fatalf("fix.push_confirmed events = %d, want 1", n)
	}
	h.e = New(h.d) // a restart
	h.startup()
	if n := len(h.fixEvents("fix.failed")); n != 0 {
		t.Fatalf("fix.failed events = %d after the restart, want none", n)
	}
	if err := h.e.poll(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.settle()
	h.wantPushedSettle(pr, 0)
	if n := len(h.fixEvents("fix.push_confirmed")); n != 1 {
		t.Errorf("fix.push_confirmed events = %d, want 1", n)
	}
}

// `magnum abort` while the settle confirms the push ends the loop as it
// ends any loop the daemon holds no job for: no fix.failed, the run no
// longer active, the record gone at the next tick, and no settle when
// GitHub then shows the push.
func TestAnAbortWhileTheSettleConfirmsThePushEndsTheLoop(t *testing.T) {
	h, _, pr, back := confirmingFix(t, "b2b2b2b Fix", "none", nil)
	id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	if r := h.request(id); r.State != store.RequestDone {
		t.Fatalf("abort: %s %q", r.State, deref(r.Result))
	}
	back()
	for range 2 {
		h.advance(time.Minute)
		_ = h.e.Tick(h.ctx)
		h.settle()
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on after the abort")
	}
	if _, ok := h.fixPushOf(2); ok {
		t.Error("the record stays after the abort")
	}
	for _, kind := range []string{"fix.failed", "fix.push_confirmed", "fix.pushed"} {
		if n := len(h.fixEvents(kind)); n != 0 {
			t.Errorf("%s events = %d, want none", kind, n)
		}
	}
	if r := h.fixRuns(pr.ID)[0]; slices.Contains([]string{store.RunPending, store.RunSubmitted, store.RunWorking, store.RunEnded}, r.State) {
		t.Errorf("fix run %s, want it no longer active", r.State)
	}
}
