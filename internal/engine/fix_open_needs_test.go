package engine

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// loopLeaves runs, on openHarness's PR #2 (a clean review of magnum's),
// one autofix loop handed ts, whose fixer pushes nothing and answers
// nothing on GitHub, and the look after it.
func loopLeaves(t *testing.T, ts ...github.Thread) *harness {
	t.Helper()
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.setThreads(ts...)
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "nothing", "1 threads (0 fixed, 0 already fixed, 1 not a bug, 0 won't fix)", "none")
	})
	h.tick() // reviewed
	h.pastSettle()
	h.tick() // the loop runs and ends
	if n := len(h.fixRuns(h.pr(2).ID)); n != 1 {
		t.Fatalf("fix runs = %d, want the loop", n)
	}
	h.advance(time.Minute)
	h.tick() // the look after the loop
	return h
}

// A loop was handed a colleague's thread, and the fixer neither replied nor
// listed it: the next look moves the PR to needs_attention, kind fix, with
// the thread's link. Nothing raised it before: the loop's settle sees only
// the fixer's own Needs you entries.
func TestAPersonsItemTheRunLeftUnansweredNeedsYou(t *testing.T) {
	h := loopLeaves(t, extThread(4322458360, "lib/sandbox.rb", 185, time.Date(2026, 10, 5, 9, 0, 0, 0, time.Local), "MINOR: `run` drops stderr", "rev-ann"))
	pr := h.wantState(2, store.PRNeedsAttention)
	if kind, _ := h.e.getKV(h.ctx, KVPRAttention(pr.ID)); kind != attention.KindFix {
		t.Errorf("attention = %q, want fix", kind)
	}
	if msg := deref(pr.LastError); !strings.Contains(msg, "1 item the fixer left needs you: "+prPage+"#discussion_r4322458360") {
		t.Errorf("last_error = %q", msg)
	}
	if _, ok := h.fixEvent("fix.open_needs_you"); !ok {
		t.Error("no fix.open_needs_you event")
	}
	h.wantOpen(OpenNeedsYou, "needs your attention (fix)")
}

// A bot's receipt on the conversation that the loop left unanswered is
// seen: no attention, and the PR is idle.
func TestABotReceiptTheRunLeftUnansweredIsSeen(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.gh.mu.Lock()
	h.gh.prComments = map[int][]github.PRComment{2: {{ID: 701, URL: prPage + "#issuecomment-701", Body: "Review done.",
		CreatedAt: h.clock.Now(), AuthorLogin: "review-tool", AuthorType: "Bot"}}}
	h.gh.mu.Unlock()
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", "0 threads", "none") })
	h.tick()
	h.pastSettle()
	h.tick() // the loop runs and ends
	h.advance(time.Minute)
	h.tick()
	h.wantState(2, store.PRReviewed)
	h.wantOpen(OpenIdle, "nothing open")
}

// A bot's inline thread is a finding: one the loop left unanswered needs
// you, as a person's does.
func TestABotThreadTheRunLeftUnansweredNeedsYou(t *testing.T) {
	h := loopLeaves(t, extThread(4322458500, "app/b.rb", 3, time.Date(2026, 10, 5, 9, 0, 0, 0, time.Local), "P1: unsafe call", "review-bot"))
	h.wantState(2, store.PRNeedsAttention)
}

// You answer the thread the fixer left: the look's next whole read sees it
// answered and puts the PR back to reviewed. Your own comment is no item.
// When the reviewer answers you in turn, that is a new item, and a loop can
// start for it.
func TestYourAnswerToEveryItemLiftsTheNeedsYou(t *testing.T) {
	th := extThread(4322458360, "lib/sandbox.rb", 185, time.Date(2026, 10, 5, 9, 0, 0, 0, time.Local), "MINOR: `run` drops stderr", "rev-ann")
	h := loopLeaves(t, th)
	h.wantState(2, store.PRNeedsAttention)
	answered := extThread(4322458360, "lib/sandbox.rb", 185, time.Date(2026, 10, 5, 9, 0, 0, 0, time.Local), "MINOR: `run` drops stderr", "rev-ann", "zhuravel")
	h.setThreads(answered)
	h.advance(time.Minute)
	h.touch(fixHead)
	h.wantState(2, store.PRReviewed)
	if _, ok := h.fixEvent("fix.open_answered"); !ok {
		t.Error("no fix.open_answered event")
	}
	if _, ok := h.e.getKV(h.ctx, KVPRAttention(h.pr(2).ID)); ok {
		t.Error("the attention row outlived the lift")
	}
	again := extThread(4322458360, "lib/sandbox.rb", 185, time.Date(2026, 10, 5, 9, 0, 0, 0, time.Local), "MINOR: `run` drops stderr", "rev-ann", "zhuravel", "rev-ann")
	h.setThreads(again)
	h.advance(time.Minute)
	h.touch(fixHead)
	if n := len(h.fixRuns(h.pr(2).ID)); n != 2 {
		t.Errorf("fix runs = %d, want a second loop for the reviewer's new answer", n)
	}
}

// A loop that ends in its own Needs you (here a push that failed) next to a
// person's leftover: the look leaves last_error as the loop wrote it, and
// after you answer the thread the PR stays in needs_attention, which only
// you can lift.
func TestTheLookNeverRewritesOrLiftsALoopsOwnNeedsYou(t *testing.T) {
	th := extThread(4322458360, "lib/sandbox.rb", 185, time.Date(2026, 10, 5, 9, 0, 0, 0, time.Local), "MINOR: `run` drops stderr", "rev-ann")
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.setThreads(th)
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "push failed: remote rejected", "0 threads", "none")
	})
	h.tick()
	h.pastSettle()
	h.tick() // the loop fails on its push
	pr := h.wantState(2, store.PRNeedsAttention)
	msg := deref(pr.LastError)
	h.advance(time.Minute)
	h.tick()
	if got := deref(h.pr(2).LastError); got != msg {
		t.Errorf("last_error = %q, want the loop's %q", got, msg)
	}
	h.setThreads(extThread(4322458360, "lib/sandbox.rb", 185, time.Date(2026, 10, 5, 9, 0, 0, 0, time.Local), "MINOR: `run` drops stderr", "rev-ann", "zhuravel"))
	h.advance(time.Minute)
	h.touch(fixHead)
	h.wantState(2, store.PRNeedsAttention)
}

// The look raised a thread (its message in last_error), then an f loop on
// the PR ended in its own Needs you (a push that failed), whose words
// replaced the look's: your answer to the raised thread lifts nothing, as
// the loop's Needs you is yours to act on. The look does not even try the
// lift (whose compare-and-set would refuse it) and log a PR that moved on.
func TestYourAnswerNeverLiftsALoopsNeedsYouThatFollowedTheLooksRaise(t *testing.T) {
	th := extThread(4322458360, "lib/sandbox.rb", 185, time.Date(2026, 10, 5, 9, 0, 0, 0, time.Local), "MINOR: `run` drops stderr", "rev-ann")
	logs := &syncBuffer{}
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"), func(h *harness) { h.d.Logger = newTeeLogger(h.t, logs) })
	h.setThreads(th)
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "nothing", "1 threads (0 fixed, 0 already fixed, 1 not a bug, 0 won't fix)", "none")
	})
	h.tick() // reviewed
	h.pastSettle()
	h.tick() // the loop runs and ends, the thread unanswered
	h.advance(time.Minute)
	h.tick() // the look raises it
	raised := deref(h.wantState(2, store.PRNeedsAttention).LastError)
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "push failed: remote rejected", "0 threads", "none")
	})
	id := h.fix(FixPayload{})
	h.handle()
	h.wantDone(id)
	msg := deref(h.wantState(2, store.PRNeedsAttention).LastError)
	if msg == raised {
		t.Fatalf("last_error = %q: the f loop did not end in its own Needs you", msg)
	}
	h.setThreads(extThread(4322458360, "lib/sandbox.rb", 185, time.Date(2026, 10, 5, 9, 0, 0, 0, time.Local), "MINOR: `run` drops stderr", "rev-ann", "zhuravel"))
	h.advance(time.Minute)
	h.touch(fixHead)
	if got := deref(h.wantState(2, store.PRNeedsAttention).LastError); got != msg {
		t.Errorf("last_error = %q, want the loop's %q", got, msg)
	}
	if _, ok := h.fixEvent("fix.open_answered"); ok {
		t.Error("the look lifted the loop's Needs you")
	}
	if strings.Contains(logs.String(), "moved on before the lift") {
		t.Error("the look tried to lift the loop's Needs you")
	}
}

// The lift is compare-and-set on the look's own message: a Needs you a
// loop writes while the look reads GitHub (after the look read the PR's
// row) is never lifted by your answer to what the look raised.
func TestALoopsNeedsYouWrittenDuringTheLooksReadIsNeverLifted(t *testing.T) {
	th := extThread(4322458360, "lib/sandbox.rb", 185, time.Date(2026, 10, 5, 9, 0, 0, 0, time.Local), "MINOR: `run` drops stderr", "rev-ann")
	h := loopLeaves(t, th)
	pr := h.wantState(2, store.PRNeedsAttention)
	const loops = "fix: claude-fix's push failed: remote rejected"
	var once sync.Once
	h.gh.mu.Lock()
	h.gh.onThreads = func() {
		once.Do(func() {
			if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("last_error", loops) }); err != nil {
				h.t.Error(err)
			}
		})
	}
	h.gh.mu.Unlock()
	h.setThreads(extThread(4322458360, "lib/sandbox.rb", 185, time.Date(2026, 10, 5, 9, 0, 0, 0, time.Local), "MINOR: `run` drops stderr", "rev-ann", "zhuravel"))
	h.advance(time.Minute)
	h.touch(fixHead)
	if got := deref(h.wantState(2, store.PRNeedsAttention).LastError); got != loops {
		t.Errorf("last_error = %q, want the loop's %q", got, loops)
	}
}

// A failed run's Needs you has no item behind it: the look never lifts it.
func TestAFailedRunsNeedsYouIsNeverLiftedByTheLook(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{"P2": 1}, "COMMENTED"))
	fixer(h, git, "", func(run store.Run) string { return "" }) // no report: the run fails
	h.tick()
	h.pastSettle()
	h.tick()
	h.wantState(2, store.PRNeedsAttention)
	for range 3 {
		h.advance(40 * time.Minute)
		h.tick()
	}
	h.wantState(2, store.PRNeedsAttention)
}

// magnum's late review of a head is toasted, and recorded, once per head.
func TestMagnumLateToastsOncePerHead(t *testing.T) {
	h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.tick()
	pr := h.pr(2)
	old := "0000000000000000000000000000000000000000"
	if err := h.st.TransitionPR(h.ctx, pr.ID, []string{store.PRReviewed}, store.PRRereviewPending, func(u *store.PRUpdate) {
		u.Set("reviewed_sha", old)
		u.Set("next_eligible_at", h.clock.Now().Add(time.Hour))
	}); err != nil {
		t.Fatal(err)
	}
	h.advance(3*time.Hour + time.Minute)
	if err := h.e.autofixOpen(h.ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.e.autofixOpen(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.flush()
	if got := toastsWith(h, "magnum's review of a1a1a1a is late"); len(got) != 1 {
		t.Errorf("toasts = %v, want one", h.nh.shown())
	}
	if n := len(h.fixEvents("fix.open_magnum_late")); n != 1 {
		t.Errorf("fix.open_magnum_late events = %d, want one for the head", n)
	}
}

// No urgent toast of a late review goes out inside quiet hours: the review
// due an hour before they began, late an hour into them, is toasted once
// at their end, when it is still late.
func TestMagnumLateToastsAtTheEndOfQuietHoursNotInsideThem(t *testing.T) {
	h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.tick()
	pr := h.pr(2)
	old := "0000000000000000000000000000000000000000"
	now := h.clock.Now()
	if err := h.st.TransitionPR(h.ctx, pr.ID, []string{store.PRReviewed}, store.PRRereviewPending, func(u *store.PRUpdate) {
		u.Set("reviewed_sha", old)
		u.Set("next_eligible_at", now.Add(time.Hour))
	}); err != nil {
		t.Fatal(err)
	}
	start, end := now.Add(2*time.Hour).Truncate(time.Minute), now.Add(6*time.Hour).Truncate(time.Minute)
	h.cfg.Daemon.QuietHours = start.Format("15:04") + "-" + end.Format("15:04")
	look := func() {
		t.Helper()
		if err := h.e.autofixOpen(h.ctx); err != nil {
			t.Fatal(err)
		}
		h.flush()
	}
	h.advance(3*time.Hour + time.Minute) // past due + magnum_late_after, inside quiet hours
	look()
	look()
	if got := toastsWith(h, "magnum's review of a1a1a1a is late"); len(got) != 0 {
		t.Errorf("toasts inside quiet hours = %v, want none", got)
	}
	if n := len(h.fixEvents("fix.open_magnum_late")); n != 0 {
		t.Errorf("fix.open_magnum_late events inside quiet hours = %d, want none", n)
	}
	h.clock.Set(end)
	look()
	look()
	if got := toastsWith(h, "magnum's review of a1a1a1a is late"); len(got) != 1 {
		t.Errorf("toasts at the end of quiet hours = %v, want one", h.nh.shown())
	}
}
