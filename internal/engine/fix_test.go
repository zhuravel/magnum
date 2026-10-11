package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
)

const (
	// fixHead is the head magnum reviewed, which the fixer starts from.
	fixHead = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	// fixPushed is the head the fixer pushes.
	fixPushed = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	// fixReviewURL is the review the fixer fixes.
	fixReviewURL = "https://github.com/talkable/talkable/pull/2#pullrequestreview-101"
	// fixReplied is a report's "## Replied" line.
	fixReplied = "3 threads (2 fixed, 0 already fixed, 1 not a bug, 0 won't fix)"
)

// fixGit is the engine's git for the fix job's tests: HEAD in the slot is
// head (the reviewed head until the fake fixer commits), every other ref
// fakeGit's. lostPush makes the pushes the fake fixer reports never reach
// GitHub; LsRemote answers remote (lsErr fails it) and records its calls.
type fixGit struct {
	fakeGit
	mu       sync.Mutex
	head     string
	lostPush bool
	remote   string
	lsErr    error
	lsCalls  []string
}

// LsRemote answers `git ls-remote origin patterns...` in dir.
func (g *fixGit) LsRemote(_ context.Context, dir string, _ time.Duration, patterns ...string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lsCalls = append(g.lsCalls, dir+":"+strings.Join(patterns, " "))
	return g.remote, g.lsErr
}

func (g *fixGit) pushLost() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lostPush
}

func (g *fixGit) setHead(sha string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.head = sha
}

func (g *fixGit) RevParse(ctx context.Context, dir, ref string) (string, error) {
	g.mu.Lock()
	head := g.head
	g.mu.Unlock()
	if ref == "HEAD" && head != "" {
		return head, nil
	}
	return g.fakeGit.RevParse(ctx, dir, ref)
}

// fixHarness is a harness whose git knows the slot's HEAD. Its watch is
// shaped as `magnum init` writes one, posting as the App and polling as the
// operator's gh login, which the built-in claude-fix, naming no identity,
// acts as.
func fixHarness(t *testing.T, mods ...func(*harness)) (*harness, *fixGit) {
	t.Helper()
	git := &fixGit{head: fixHead}
	h := newHarness(t, append([]func(*harness){func(h *harness) { h.d.Git = git }}, mods...)...)
	return h, git
}

// fixRole is the configured claude-fix role of h, to edit.
func (h *harness) fixRole() *config.Role {
	h.t.Helper()
	for i := range h.cfg.Roles {
		if h.cfg.Roles[i].Fix {
			return &h.cfg.Roles[i]
		}
	}
	h.t.Fatal("no fix role declared")
	return nil
}

// realSleep makes the engine's waits take a real millisecond each: the fix
// job's wait then polls its run until the fake fixer ends it, or the test
// stops the job, without spinning.
func realSleep(h *harness) {
	h.d.Sleep = func(ctx context.Context, _ time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
			return nil
		}
	}
}

// fixablePR opens PR #2 by author at fixHead, has it reviewed and, with
// findings (nil = none recorded), records the posted review's judge run with
// those findings: the state `magnum fix` starts from.
func fixablePR(h *harness, author string, findings map[string]int) store.PR {
	h.t.Helper()
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync: #1 baseline
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead, author: author})
	h.tick() // #2 queued
	h.advance(5 * time.Minute)
	h.tick() // dispatched and reviewed
	pr := h.wantState(2, store.PRReviewed)
	if findings == nil {
		return pr
	}
	result, err := json.Marshal(map[string]any{"event": "COMMENTED", "findings": findings})
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.st.DB().ExecContext(h.ctx, `INSERT INTO runs (id, pr_id, round, role, kind, target_sha, identity, reviewer_login,
  state, outcome, review_id, review_url, prompt_text, result_json, created_at)
  VALUES ('run-review-2', ?, 1, 'codex-judge', 'initial', ?, ?, 'talkable[bot]', 'verified', 'posted', 101, ?, 'p', ?, ?)`,
		pr.ID, pr.HeadSHA, pr.Identity, fixReviewURL, string(result), store.FormatTime(h.clock.Now())); err != nil {
		h.t.Fatal(err)
	}
	return pr
}

// fixReport is a fixer's report of run runID.
func fixReport(runID, pushed, replied, needs string) string {
	return agents.ReportMarker(runID) + "\n## Pushed\n" + pushed + "\n## Replied\n" + replied + "\n## Needs you\n" + needs + "\n"
}

// fixer makes h's fake fixer: once its prompt is sent it writes report(run)
// to the run's report path ("" = it writes none), moves the slot's HEAD to
// head as its commits do ("" = it commits nothing), puts head on GitHub
// when the report lists pushed commits (its push; never with
// fixGit.lostPush) and ends its run, as the observer does after two idle
// ticks.
func fixer(h *harness, git *fixGit, head string, report func(run store.Run) string) {
	fixerBy(h, git, func(run store.Run) (string, string) { return head, report(run) })
}

// fixerBy is fixer with the head and the report of each run from work.
func fixerBy(h *harness, git *fixGit, work func(run store.Run) (head, report string)) {
	h.ag.mu.Lock()
	defer h.ag.mu.Unlock()
	h.ag.onSubmit = func(run store.Run, _ string) {
		head, text := work(run)
		if text != "" {
			path := deref(run.ReportPath)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				h.t.Error(err)
				return
			}
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				h.t.Error(err)
				return
			}
		}
		if head != "" {
			git.setHead(head)
			if rep, err := ParseFixReport(text, run.ID); err == nil && len(rep.Pushed) > 0 && !rep.PushFailed && !git.pushLost() {
				if pr, err := h.st.PRByID(h.ctx, run.PRID); err == nil {
					h.gh.setHead("talkable/talkable", pr.Number, head)
				}
			}
		}
		if err := h.st.TransitionRun(h.ctx, run.ID, []string{store.RunWorking}, store.RunEnded, func(u *store.RunUpdate) {
			u.Set("ended_at", h.clock.Now())
		}); err != nil {
			h.t.Error(err)
		}
	}
}

// fixRuns are the PR's runs of kind fix, oldest first.
func (h *harness) fixRuns(prID int64) []store.Run {
	h.t.Helper()
	runs, err := h.st.RunsByPR(h.ctx, prID)
	if err != nil {
		h.t.Fatal(err)
	}
	return slices.DeleteFunc(runs, func(r store.Run) bool { return r.Kind != store.RunFix })
}

// fix asks for a fix loop on PR #2 and returns the request.
func (h *harness) fix(p FixPayload) int64 {
	h.t.Helper()
	p.PRTarget = PRTarget{Ref: "2"}
	return h.enqueue(ReqFix, p)
}

// wantDone fails unless request id is done and returns its answer.
func (h *harness) wantDone(id int64) string {
	h.t.Helper()
	r := h.request(id)
	if r.State != store.RequestDone {
		h.t.Fatalf("request %d: %s %q", id, r.State, deref(r.Result))
	}
	return deref(r.Result)
}

// fixStateOf is PR #n's fix loop (ok false: none).
func (h *harness) fixStateOf(n int) (FixState, bool) {
	h.t.Helper()
	st, ok, err := h.e.fixState(h.ctx, h.pr(n).ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return st, ok
}

// fixEvent is PR #2's newest event of kind (ok false: none).
func (h *harness) fixEvent(kind string) (store.Event, bool) {
	h.t.Helper()
	evs, err := h.st.EventsBySubject(h.ctx, "pr:talkable/talkable#2", 0)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, ev := range slices.Backward(evs) {
		if ev.Kind == kind {
			return ev, true
		}
	}
	return store.Event{}, false
}

// wantReportInData fails unless PR #2's event kind names report in its data.
func (h *harness) wantReportInData(kind, report string) {
	h.t.Helper()
	ev, ok := h.fixEvent(kind)
	if !ok {
		h.t.Fatalf("no %s event", kind)
	}
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil || data["report"] != report {
		h.t.Errorf("%s data = %s, want report %s", kind, ev.Data, report)
	}
}

// awaitSubmit waits until the fix job sent its prompt.
func (h *harness) awaitSubmit() {
	h.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); h.ag.count("submit:claude-fix:") == 0; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			h.t.Fatalf("the fixer's prompt was never sent: %v", h.ag.all())
		}
	}
}

// A `magnum fix` on the operator's reviewed PR starts the fix job: the loop's
// row says fixing, cycle 1 of 3, the fixer's run of kind fix is submitted in
// the review's round with the prompt's facts, the fixer's pane acts as the
// operator (not as the posting App), dispatch holds a push to the PR with
// WaitFixing, and `magnum review` and a second `magnum fix` are refused.
func TestAFixRequestStartsTheJobAndHoldsThePR(t *testing.T) {
	h, git := fixHarness(t, realSleep)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 2})
	slot := h.slot("review1")

	id := h.fix(FixPayload{})
	h.e.handleRequests(h.ctx) // the job runs on: the fixer works
	if res := h.wantDone(id); !strings.Contains(res, "cycle 1 of 3") || !strings.Contains(res, "claude-fix") {
		t.Errorf("answer = %q", res)
	}
	h.awaitSubmit()

	st, ok := h.fixStateOf(2)
	if !ok || st.Cycle != 1 || st.Cap != 3 || st.Phase != FixPhaseFixing || st.By != "magnum fix" || st.Head != fixHead || st.Since != fixHead {
		t.Fatalf("fix state = %+v, %v", st, ok)
	}
	runs := h.fixRuns(pr.ID)
	if len(runs) != 1 {
		t.Fatalf("fix runs = %+v", runs)
	}
	run := runs[0]
	wantReport := filepath.Join(h.layout.ReviewDir("talkable", "talkable", 2, fixHead), "claude-fix.md")
	if run.Role != "claude-fix" || run.Round != 1 || run.State != store.RunWorking || deref(run.ReportPath) != wantReport || st.Report != wantReport {
		t.Errorf("fix run = %+v (report %s), state report %s", run, deref(run.ReportPath), st.Report)
	}
	reviewFile := filepath.Join(h.layout.ReviewDir("talkable", "talkable", 2, fixHead), "review.json")
	wantPrompt := fmt.Sprintf("fix_prompt:claude-fix:%s|feature|%s|%s|talkable[bot]|%s", run.ID, fixReviewURL, reviewFile,
		agents.DBLockLine(h.layout.Binary(), slot.Path, "claude-fix"))
	if !slices.Contains(h.ag.all(), wantPrompt) {
		t.Errorf("no %q in %v", wantPrompt, h.ag.all())
	}
	if !h.lastWorkspaceHas(pr.ID, "claude-fix") {
		t.Error("the PR's workspace has no claude-fix pane")
	}
	if judge, fix := h.ag.paneEnv("codex-judge"), h.ag.paneEnv("claude-fix"); judge["GH_CONFIG_DIR"] != "/state/gh/talkable-app" || fix == nil {
		t.Errorf("pane envs: judge %v, fixer %v", judge, fix)
	} else if _, ok := fix["GH_CONFIG_DIR"]; ok || fix["WT_BRANCH"] != "review1" {
		t.Errorf("the fixer's pane env %v acts as the posting App, or lost the slot's env", fix)
	}
	if !h.hasEvent("pr:talkable/talkable#2", "fix.start") {
		t.Error("no fix.start event")
	}
	if got := deref(h.slot("review1").LastUsedAt); !got.Equal(h.clock.Now()) {
		t.Errorf("slot last_used_at = %v, want the fix's start %v", got, h.clock.Now())
	}

	// Someone pushes meanwhile: the re-review waits for the fixer.
	rounds := len(h.rd.all())
	h.advance(time.Hour)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "c3c3c3c", author: "zhuravel"})
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.wantState(2, store.PRRereviewPending)
	if w := waitOf(t, h, 2); w.Reason != WaitFixing || !strings.Contains(w.Sentence("talkable/talkable#2", h.clock.Now()), "magnum abort") {
		t.Errorf("wait = %+v", w)
	}
	if got := len(h.rd.all()); got != rounds {
		t.Errorf("rounds = %d, want %d: a round started while the fixer works", got, rounds)
	}

	review := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	again := h.fix(FixPayload{})
	h.e.handleRequests(h.ctx)
	for _, id := range []int64{review, again} {
		if r := h.request(id); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "fixing, cycle 1 of 3; `magnum abort` stops it") {
			t.Errorf("request %d: %s %q", id, r.State, deref(r.Result))
		}
	}

	// The fixer ends without pushing: the loop ends and the PR is free.
	git.setHead(fixHead)
	if err := os.MkdirAll(filepath.Dir(wantReport), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wantReport, []byte(fixReport(run.ID, "nothing", fixReplied, "none")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.st.TransitionRun(h.ctx, run.ID, []string{store.RunWorking}, store.RunEnded, nil); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop's row outlived the job")
	}
}

// The request names why it will not start a loop.
func TestFixRefusals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		author   string
		findings map[string]int
		mod      func(h *harness, pr store.PR)
		want     string
	}{
		{name: "no review", author: "zhuravel", want: "nothing to fix: no review on a1a1a1a"},
		{name: "found nothing", author: "zhuravel", findings: map[string]int{}, want: "nothing to fix: the review on a1a1a1a found nothing"},
		{name: "another author", author: "alice", findings: map[string]int{"P1": 1},
			want: "PR #2 is alice's, not yours; the fixer pushes and replies as you"},
		{name: "fork", author: "zhuravel", findings: map[string]int{"P1": 1}, mod: func(h *harness, pr store.PR) {
			if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("is_cross_repo", true) }); err != nil {
				h.t.Fatal(err)
			}
		}, want: "the head lives in a fork magnum cannot push to"},
		{name: "running", author: "zhuravel", findings: map[string]int{"P1": 1}, mod: func(h *harness, pr store.PR) {
			if err := h.e.setFixState(h.ctx, pr.ID, FixState{Cycle: 2, Cap: 3, Phase: FixPhaseReviewing}); err != nil {
				h.t.Fatal(err)
			}
		}, want: "fixing, cycle 2 of 3; `magnum abort` stops it"},
		{name: "no fix role", author: "zhuravel", findings: map[string]int{"P1": 1}, mod: func(h *harness, _ store.PR) {
			h.cfg.Watches[0].FixRole = new("")
		}, want: "no fix role for talkable/talkable"},
		{name: "an undeclared identity", author: "zhuravel", findings: map[string]int{"P1": 1}, mod: func(h *harness, _ store.PR) {
			h.fixRole().Identity = "me"
		}, want: "no identity me for the fix role"},
		{name: "a poll identity that is an App", author: "zhuravel", findings: map[string]int{"P1": 1}, mod: func(h *harness, _ store.PR) {
			h.cfg.Watches[0].PollIdentity = "talkable-app"
		}, want: "the fix role's identity talkable-app is an App; name a gh identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := fixHarness(t)
			pr := fixablePR(h, tc.author, tc.findings)
			if tc.mod != nil {
				tc.mod(h, pr)
			}
			id := h.fix(FixPayload{})
			h.handle()
			if r := h.request(id); r.State != store.RequestFailed || deref(r.Result) != tc.want {
				t.Errorf("%s %q, want failed %q", r.State, deref(r.Result), tc.want)
			}
			if h.ag.count("submit:") != 0 {
				t.Error("a refused request started the fixer")
			}
		})
	}
}

// A fixer that pushed with nobody needed counts the cycle: the slot is put
// back in order at the pushed head, the push is recorded on the PR row as
// the poller records one (so the poller's own sight of it changes nothing),
// the loop waits for the re-review (reviewing, cycle 1), and a forced
// re-review of the pushed head runs.
func TestAPushedReportForcesTheReReview(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 2})
	fixer(h, git, fixPushed, func(run store.Run) string {
		return fixReport(run.ID, "b2b2b2b Guard the nil owner", fixReplied, "none")
	})
	id := h.fix(FixPayload{})
	h.handle() // no dispatch yet: the fake slots fetch the head the poller recorded
	h.wantDone(id)
	if !slices.Contains(h.sl.all(), "resync:review1:"+fixPushed) {
		t.Errorf("slot calls %v lack the resync to the pushed head", h.sl.all())
	}
	if sl := h.slot("review1"); deref(sl.CheckedOutSHA) != fixPushed {
		t.Errorf("slot checked out %s", deref(sl.CheckedOutSHA))
	}
	st, ok := h.fixStateOf(2)
	report := deref(h.fixRuns(pr.ID)[0].ReportPath)
	if !ok || st.Phase != FixPhaseReviewing || st.Cycle != 1 || st.Head != fixPushed || st.Report != report || len(st.NeedsYou) != 0 {
		t.Fatalf("fix state = %+v, %v", st, ok)
	}
	if cur := h.wantState(2, store.PRRereviewPending); !cur.Forced {
		t.Error("the re-review is not forced")
	}
	if r := h.fixRuns(pr.ID)[0]; r.State != store.RunVerified {
		t.Errorf("fix run %s", r.State)
	}
	h.wantReportInData("fix.pushed", report)
	h.wantReportInData("fix.replied", report)
	// The push is on the PR row as the poller records one.
	if cur := h.pr(2); cur.HeadSHA != fixPushed || !cur.HeadChangedAt.Equal(h.clock.Now()) {
		t.Errorf("PR head %s changed at %v, want %s now", cur.HeadSHA, cur.HeadChangedAt, fixPushed)
	}
	if n := h.eventCount("pr:talkable/talkable#2", "pr.head_changed"); n != 1 {
		t.Errorf("pr.head_changed events = %d, want 1", n)
	}
	if got := h.e.pushTimes(h.ctx, pr.ID); len(got) == 0 || !got[len(got)-1].Equal(h.clock.Now()) {
		t.Errorf("recorded pushes %v lack the fixer's", got)
	}

	// The poller's own sight of the push changes nothing; the forced
	// re-review runs on the pushed head.
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})
	if n := h.eventCount("pr:talkable/talkable#2", "pr.head_changed"); n != 1 {
		t.Errorf("pr.head_changed events after the poll = %d, want 1", n)
	}
	reqWantRounds(t, h, 2)
	if in := h.rd.all()[1]; in.TargetSHA != fixPushed || !in.PR.Forced {
		t.Errorf("the re-review: target %s forced %v", in.TargetSHA, in.PR.Forced)
	}
}

// A fixer that pushed nothing and needs nobody answered every finding: the
// slot goes back to the reviewed head, the loop ends and an info toast says
// so.
func TestAFixThatPushedNothingResyncsTheSlot(t *testing.T) {
	h, git := fixHarness(t)
	fixablePR(h, "zhuravel", map[string]int{"P2": 3})
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	h.fix(FixPayload{})
	h.tick()
	if !slices.Contains(h.sl.all(), "resync:review1:"+fixHead) {
		t.Errorf("slot calls %v lack the resync to the reviewed head", h.sl.all())
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on")
	}
	h.wantState(2, store.PRReviewed)
	h.flush()
	if got := toastsWith(h, "PR #2: claude-fix pushed nothing, replied to 3 findings"); len(got) != 1 {
		t.Errorf("toasts = %v", h.nh.shown())
	}
	h.wantToasts("needs attention", 0)
}

// Findings the fixer leaves to the author without a push end the loop as
// needs attention, the fix job's kind, naming how many and the report; the
// slot goes back to the reviewed head.
func TestNeedsYouEndsTheLoopAsNeedsAttention(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P1": 1, "P2": 2})
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "nothing", fixReplied, "- 2101 P1 app/models/offer.rb:42: wants a schema change\n- 2102 P2 app/lib/x.rb:7: contested")
	})
	h.fix(FixPayload{})
	h.tick()
	report := deref(h.fixRuns(pr.ID)[0].ReportPath)
	cur := h.wantState(2, store.PRNeedsAttention)
	if want := "fix: 2 findings need your call: " + report; deref(cur.LastError) != want {
		t.Errorf("last_error = %q, want %q", deref(cur.LastError), want)
	}
	if kind, _ := h.e.getKV(h.ctx, KVPRAttention(pr.ID)); kind != attention.KindFix {
		t.Errorf("attention kind = %q", kind)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on")
	}
	if !slices.Contains(h.sl.all(), "resync:review1:"+fixHead) {
		t.Errorf("slot calls %v", h.sl.all())
	}
	h.wantToasts("needs attention", 1)
	h.wantReportInData("fix.needs_you", report)
}

// Findings the fixer leaves to the author after a push wait for the push's
// re-review: the slot moves to the pushed head, the cycle counts, the loop
// waits in phase reviewing with the entries kept for the round's end, and
// the re-review is forced; nobody is asked yet.
func TestAPushWithNeedsYouIsReReviewedFirst(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P1": 1, "P2": 2})
	fixer(h, git, fixPushed, func(run store.Run) string {
		return fixReport(run.ID, "b2b2b2b Guard the nil owner", fixReplied, "- 2102 P2 app/lib/x.rb:7: contested")
	})
	h.fix(FixPayload{})
	h.handle()
	report := deref(h.fixRuns(pr.ID)[0].ReportPath)
	st, ok := h.fixStateOf(2)
	if !ok || st.Phase != FixPhaseReviewing || st.Cycle != 1 || st.Head != fixPushed ||
		!slices.Equal(st.NeedsYou, []string{"2102 P2 app/lib/x.rb:7: contested"}) {
		t.Fatalf("fix state = %+v, %v", st, ok)
	}
	if cur := h.wantState(2, store.PRRereviewPending); !cur.Forced || deref(cur.LastError) != "" {
		t.Errorf("the re-review: forced %v, last_error %q", cur.Forced, deref(cur.LastError))
	}
	if !slices.Contains(h.sl.all(), "resync:review1:"+fixPushed) {
		t.Errorf("slot calls %v", h.sl.all())
	}
	h.wantReportInData("fix.needs_you", report)
	h.wantToasts("needs attention", 0)
}

// A run that ends without a report of its own fails the job: the loop ends
// as needs attention, the slot goes back to the reviewed head.
func TestAMissingFixReportFailsTheJob(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report func(run store.Run) string
		want   string
	}{
		{name: "no report", report: func(store.Run) string { return "" }, want: "wrote no report at "},
		{name: "another run's", report: func(store.Run) string { return fixReport("r-older", "nothing", fixReplied, "none") },
			want: "the first line is not the run's marker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, git := fixHarness(t)
			pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
			fixer(h, git, "", tc.report)
			h.fix(FixPayload{})
			h.tick()
			cur := h.wantState(2, store.PRNeedsAttention)
			if le := deref(cur.LastError); !strings.HasPrefix(le, "fix run failed: ") || !strings.Contains(le, tc.want) {
				t.Errorf("last_error = %q", le)
			}
			if _, ok := h.fixStateOf(2); ok {
				t.Error("the loop goes on")
			}
			if !slices.Contains(h.sl.all(), "resync:review1:"+fixHead) {
				t.Errorf("slot calls %v", h.sl.all())
			}
			if r := h.fixRuns(pr.ID)[0]; r.State != store.RunFailed {
				t.Errorf("fix run %s", r.State)
			}
			h.wantReportInData("fix.failed", deref(h.fixRuns(pr.ID)[0].ReportPath))
		})
	}
}

// A push the fixer reports failed (someone pushed meanwhile) is the failed
// case: the fixer's local commits go, the slot is back at the reviewed head.
func TestAFixWhosePushFailedEndsAsFailed(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	fixer(h, git, "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3", func(run store.Run) string {
		return fixReport(run.ID, "push failed: ! [rejected] HEAD -> feature (non-fast-forward)", fixReplied, "- 2101 P2 a.rb:1: committed, not pushed")
	})
	h.fix(FixPayload{})
	h.tick()
	report := deref(h.fixRuns(pr.ID)[0].ReportPath)
	cur := h.wantState(2, store.PRNeedsAttention)
	if want := "fix run failed: the fixer's push failed: " + report; deref(cur.LastError) != want {
		t.Errorf("last_error = %q, want %q", deref(cur.LastError), want)
	}
	if !slices.Contains(h.sl.all(), "resync:review1:"+fixHead) {
		t.Errorf("slot calls %v", h.sl.all())
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on")
	}
	h.wantReportInData("fix.failed", report)
}

// A push the fixer reports but GitHub does not show (the branch's head is
// still the reviewed one) is no push: the settle fails with "push not
// confirmed on GitHub", the PR row keeps GitHub's head, no re-review of a
// commit GitHub lacks is forced, and the slot goes back to the reviewed head.
func TestAPushGitHubDoesNotShowFailsTheFix(t *testing.T) {
	h, git := fixHarness(t)
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	git.lostPush = true
	git.lsErr = errors.New("git ls-remote: not reached")
	fixer(h, git, fixPushed, func(run store.Run) string { return fixReport(run.ID, "b2b2b2b Fix", fixReplied, "none") })
	h.fix(FixPayload{})
	h.handle()

	cur := h.wantState(2, store.PRNeedsAttention)
	if le := deref(cur.LastError); !strings.Contains(le, "push not confirmed on GitHub") || !strings.Contains(le, "a1a1a1a") {
		t.Errorf("last_error = %q", le)
	}
	if cur.HeadSHA != fixHead || cur.Forced {
		t.Errorf("PR head %s forced %v, want GitHub's %s and no re-review", cur.HeadSHA, cur.Forced, fixHead)
	}
	if n := h.eventCount("pr:talkable/talkable#2", "pr.head_changed"); n != 0 {
		t.Errorf("pr.head_changed events = %d, want none", n)
	}
	if !slices.Contains(h.sl.all(), "resync:review1:"+fixHead) || slices.Contains(h.sl.all(), "resync:review1:"+fixPushed) {
		t.Errorf("slot calls %v, want the slot back at the reviewed head only", h.sl.all())
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on")
	}
	if _, ok := h.fixEvent("fix.pushed"); ok {
		t.Error("fix.pushed recorded for a push GitHub does not show")
	}
}

// When the poll identity's GitHub client cannot say where the PR's head is,
// the branch on origin answers (git ls-remote): a push it shows is
// confirmed and re-reviewed; with neither answering, the push is not
// confirmed and the loop fails.
func TestAPushIsConfirmedOnTheBranchWhenGitHubCannotSay(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remote string
		lsErr  error
		want   string // the PR's state after the settle
	}{
		{name: "the branch shows it", remote: fixPushed + "\trefs/heads/feature\n", want: store.PRRereviewPending},
		{name: "nothing answers", lsErr: errors.New("git ls-remote: not reached"), want: store.PRNeedsAttention},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, git := fixHarness(t)
			fixablePR(h, "zhuravel", map[string]int{"P2": 1})
			h.gh.fail(errors.New("graphql: 502 Bad Gateway"), nil)
			git.lostPush, git.remote, git.lsErr = true, tc.remote, tc.lsErr
			fixer(h, git, fixPushed, func(run store.Run) string { return fixReport(run.ID, "b2b2b2b Fix", fixReplied, "none") })
			h.fix(FixPayload{})
			h.handle()

			cur := h.wantState(2, tc.want)
			git.mu.Lock()
			calls := slices.Clone(git.lsCalls)
			git.mu.Unlock()
			if len(calls) == 0 || calls[0] != h.slot("review1").Path+":refs/heads/feature" {
				t.Errorf("ls-remote calls %v", calls)
			}
			if tc.want == store.PRRereviewPending {
				if cur.HeadSHA != fixPushed || !cur.Forced {
					t.Errorf("PR head %s forced %v, want the confirmed push re-reviewed", cur.HeadSHA, cur.Forced)
				}
				return
			}
			if le := deref(cur.LastError); !strings.Contains(le, "push not confirmed on GitHub") {
				t.Errorf("last_error = %q", le)
			}
		})
	}
}

// A push by the operator while the fixer works makes the fixer's push fail.
// The poller records the operator's head, so the PR waits for its re-review
// (the fixer holds it); the failed push then ends the loop with the urgent
// toast alone, no needs_attention (the re-review of the operator's head
// still runs), the slot back at the reviewed head and the loop's row gone;
// the operator's head is re-reviewed next.
func TestAnOperatorPushDuringTheFixIsReviewedAfterTheFixersFailedPush(t *testing.T) {
	const operator = "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
	h, git := fixHarness(t, realSleep)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	h.fix(FixPayload{})
	h.e.handleRequests(h.ctx)
	h.awaitSubmit()

	h.advance(time.Hour)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: operator, author: "zhuravel"})
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.wantState(2, store.PRRereviewPending)
	rounds := len(h.rd.all())

	run := h.fixRuns(pr.ID)[0]
	if err := os.MkdirAll(filepath.Dir(deref(run.ReportPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	report := fixReport(run.ID, "push failed: ! [rejected] HEAD -> feature (fetch first)", fixReplied, "- 2101 P2 a.rb:1: committed, not pushed")
	if err := os.WriteFile(deref(run.ReportPath), []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	git.setHead(fixPushed) // the fixer's local commit
	if err := h.st.TransitionRun(h.ctx, run.ID, []string{store.RunWorking}, store.RunEnded, nil); err != nil {
		t.Fatal(err)
	}
	h.settle()

	cur := h.wantState(2, store.PRRereviewPending)
	if cur.HeadSHA != operator || deref(cur.LastError) != "" {
		t.Errorf("PR head %s, last_error %q: want the operator's head waiting for its re-review", cur.HeadSHA, deref(cur.LastError))
	}
	if got := toastsWith(h, "talkable#2 needs attention"); len(got) != 1 {
		t.Errorf("urgent toasts = %v, want one", h.nh.shown())
	}
	if !slices.Contains(h.sl.all(), "resync:review1:"+fixHead) {
		t.Errorf("slot calls %v lack the resync to the reviewed head", h.sl.all())
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop's row outlived the failed push")
	}
	if ev, ok := h.fixEvent("fix.failed"); !ok || !strings.Contains(ev.Message, "the fixer's push failed") {
		t.Errorf("fix.failed = %+v, %v", ev, ok)
	}

	h.advance(time.Hour)
	h.tick()
	if got := h.rd.all(); len(got) != rounds+1 || got[len(got)-1].TargetSHA != operator {
		t.Fatalf("rounds after the fix: %d (want %d), the last one's target %v", len(got), rounds+1, got[len(got)-1].TargetSHA)
	}
	h.wantState(2, store.PRReviewed)
}

// `magnum abort` during the fixer's run interrupts the fixer, abandons its
// run, puts the slot back and ends the loop quietly: no attention.
func TestAbortEndsAFix(t *testing.T) {
	var kh *keyHerdr
	h, _ := fixHarness(t, realSleep, withKeys(&kh))
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	h.fix(FixPayload{})
	h.e.handleRequests(h.ctx)
	h.awaitSubmit()
	h.hd.mu.Lock()
	h.hd.agents = append(h.hd.agents, herdr.AgentInfo{PaneID: "p-claude-fix", Name: "mg-t-2-claude-fix", Agent: "claude",
		AgentStatus: herdr.StatusWorking})
	h.hd.mu.Unlock()

	id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.e.handleRequests(h.ctx)
	h.settle()
	res := h.wantDone(id)
	if !strings.Contains(res, "aborted the fix of talkable/talkable#2") || !strings.Contains(res, "fixer interrupted") {
		t.Errorf("answer = %q", res)
	}
	if got, want := kh.all(), []string{"agent:mg-t-2-claude-fix:ctrl+c", "agent:mg-t-2-claude-fix:ctrl+c"}; !slices.Equal(got, want) {
		t.Errorf("keys %v, want %v", got, want)
	}
	if r := h.fixRuns(pr.ID)[0]; r.State != store.RunAbandoned || deref(r.Error) != abortedBy {
		t.Errorf("fix run %s %q", r.State, deref(r.Error))
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on")
	}
	if !slices.Contains(h.sl.all(), "resync:review1:"+fixHead) {
		t.Errorf("slot calls %v", h.sl.all())
	}
	h.wantState(2, store.PRReviewed)
	h.flush()
	h.wantToasts("needs attention", 0)
	if got := toastsWith(h, "fix stopped"); len(got) != 1 {
		t.Errorf("toasts = %v", h.nh.shown())
	}
}

// `magnum fix --off` switches autofix off for the PR and `--on` back on;
// neither starts a loop, and a loop asked for by hand runs anyway, saying
// so.
func TestFixOffAndOnToggleTheFlag(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	off := h.fix(FixPayload{Off: true})
	h.handle()
	if res := h.wantDone(off); res != "fix is off for PR #2: autofix leaves it to you; `magnum fix --on` lifts it" {
		t.Errorf("--off answer = %q", res)
	}
	o, ok, err := h.e.fixOff(h.ctx, pr.ID)
	if err != nil || !ok || o.By != "magnum fix --off" || !o.Since.Equal(h.clock.Now()) {
		t.Fatalf("fix off = %+v, %v, %v", o, ok, err)
	}
	if _, ok := h.fixStateOf(2); ok || h.ag.count("submit:") != 0 {
		t.Fatal("--off started a loop")
	}

	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	id := h.fix(FixPayload{})
	h.tick()
	if res := h.wantDone(id); !strings.Contains(res, "fix is off for this PR: this requested loop runs anyway") {
		t.Errorf("answer = %q", res)
	}
	if h.ag.count("submit:claude-fix:") != 1 {
		t.Error("the requested loop did not run")
	}

	on := h.fix(FixPayload{On: true})
	h.handle()
	if res := h.wantDone(on); res != "autofix is on again for PR #2" {
		t.Errorf("--on answer = %q", res)
	}
	if _, ok, err := h.e.fixOff(h.ctx, pr.ID); ok || err != nil {
		t.Errorf("fix off after --on: %v, %v", ok, err)
	}
	if h.ag.count("submit:claude-fix:") != 1 {
		t.Error("--on started a loop")
	}
}

// A dry run says what would happen and does nothing; on a watch that posts
// as the operator it says that the re-review will not read the fixer's
// replies as the author's.
func TestAFixDryRunOnAWatchThatPostsAsYouSaysSo(t *testing.T) {
	h, _ := fixHarness(t, func(h *harness) { h.cfg.Watches[0].Identity = "zhuravel" })
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	id := h.fix(FixPayload{DryRun: true})
	h.handle()
	res := h.wantDone(id)
	if !strings.HasPrefix(res, "dry run: would") || !strings.Contains(res, "this watch posts as you: the re-review will not read the fixer's replies as yours") {
		t.Errorf("answer = %q", res)
	}
	if _, ok := h.fixStateOf(2); ok || h.ag.count("submit:") != 0 || len(h.fixRuns(pr.ID)) != 0 {
		t.Error("a dry run started the fixer")
	}
}

// A failed guard (a person's agent in the slot) ends the job before the
// fixer starts, as needs attention with the reason; the slot is not touched.
func TestAFailedGuardEndsTheFixAsNeedsAttention(t *testing.T) {
	h, _ := fixHarness(t)
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	h.sl.mu.Lock()
	h.sl.guardErr = slots.ErrHold{Reason: slots.HoldForeignAgent, Detail: `pane p9: claude agent "x" is idle in /tmp/review1`}
	h.sl.mu.Unlock()
	h.fix(FixPayload{})
	h.tick()
	cur := h.wantState(2, store.PRNeedsAttention)
	if le := deref(cur.LastError); !strings.HasPrefix(le, "fix guard failed: ") || !strings.Contains(le, "pane p9") {
		t.Errorf("last_error = %q", le)
	}
	if h.ag.count("submit:") != 0 || slices.ContainsFunc(h.sl.all(), func(c string) bool { return strings.HasPrefix(c, "resync:") }) {
		t.Errorf("the fixer started or the slot was touched: %v, %v", h.ag.all(), h.sl.all())
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on")
	}
}

// A resync that fails (herdr down) is the job's failure, never swallowed:
// the loop ends as needs attention and no re-review is forced.
func TestAResyncFailureEndsTheFixAsNeedsAttention(t *testing.T) {
	h, git := fixHarness(t)
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	h.sl.mu.Lock()
	h.sl.resyncErr = errors.New("slots: guard review1: herdr snapshot: connection refused")
	h.sl.mu.Unlock()
	fixer(h, git, fixPushed, func(run store.Run) string { return fixReport(run.ID, "b2b2b2b Fix", fixReplied, "none") })
	h.fix(FixPayload{})
	h.tick()
	cur := h.wantState(2, store.PRNeedsAttention)
	if le := deref(cur.LastError); !strings.HasPrefix(le, "fix settle failed: ") || !strings.Contains(le, "connection refused") {
		t.Errorf("last_error = %q", le)
	}
	if cur.Forced {
		t.Error("a re-review was forced")
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on")
	}
}

// A fixer whose time runs out is told to write its report now; one that
// still writes none fails the job, and the slot goes back to the reviewed
// head (the fixer's local commits would hold it for head drift).
func TestAFixThatTimesOutIsAskedForItsReportThenFails(t *testing.T) {
	h, _ := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	h.e.d.Sleep = func(ctx context.Context, d time.Duration) error { h.clock.Add(d); return ctx.Err() }
	h.fix(FixPayload{})
	h.tick()
	report := deref(h.fixRuns(pr.ID)[0].ReportPath)
	if !slices.ContainsFunc(h.ag.all(), func(c string) bool {
		return strings.HasPrefix(c, "tell:claude-fix:Time is up") && strings.Contains(c, report)
	}) {
		t.Errorf("no time-up line in %v", h.ag.all())
	}
	cur := h.wantState(2, store.PRNeedsAttention)
	if le := deref(cur.LastError); !strings.HasPrefix(le, "fix run failed: ") || !strings.Contains(le, "ran out of its 1h") {
		t.Errorf("last_error = %q", le)
	}
	if !slices.Contains(h.sl.all(), "resync:review1:"+fixHead) {
		t.Errorf("slot calls %v lack the resync to the reviewed head: the fixer's commits stay in the slot", h.sl.all())
	}
	if r := h.fixRuns(pr.ID)[0]; r.State != store.RunFailed && r.State != store.RunAbandoned {
		t.Errorf("fix run %s", r.State)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on")
	}
}

// After a restart no fixer run survives: a loop whose fixer was at work ends
// as needs attention and its run fails; a loop waiting for its re-review
// stays (the round's own recovery re-reviews).
func TestAFixRunLostInARestartEndsTheLoop(t *testing.T) {
	h, _ := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	run, err := h.st.CreateRun(h.ctx, store.Run{ID: "r-fix-lost", PRID: pr.ID, Round: 1, Role: "claude-fix", Kind: store.RunFix,
		TargetSHA: fixHead, Identity: pr.Identity, ReviewerLogin: "x", State: store.RunPending})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.TransitionRun(h.ctx, run.ID, []string{store.RunPending}, store.RunWorking, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.e.setFixState(h.ctx, pr.ID, FixState{Cycle: 1, Cap: 3, Phase: FixPhaseFixing, Head: fixHead}); err != nil {
		t.Fatal(err)
	}
	other := h.pr(1) // waiting for its loop's re-review
	if err := h.st.TransitionPR(h.ctx, other.ID, nil, store.PRQueued, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.e.setFixState(h.ctx, other.ID, FixState{Cycle: 2, Cap: 3, Phase: FixPhaseReviewing, Head: other.HeadSHA}); err != nil {
		t.Fatal(err)
	}

	h.startup()
	cur := h.wantState(2, store.PRNeedsAttention)
	if deref(cur.LastError) != "fix run lost in a daemon restart" {
		t.Errorf("last_error = %q", deref(cur.LastError))
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the lost loop goes on")
	}
	if r, err := h.st.RunByID(h.ctx, run.ID); err != nil || r.State != store.RunFailed {
		t.Errorf("the lost run: %+v, %v", r, err)
	}
	if st, ok := h.fixStateOf(1); !ok || st.Phase != FixPhaseReviewing {
		t.Errorf("the reviewing loop = %+v, %v", st, ok)
	}
}

// liveFixer gives PR pr a live session of the fix role whose agent herdr
// shows working: a fixer the engine did not start, as one that outlived a
// daemon (herdr keeps its pane after a crash or `daemon-restart --now`).
func liveFixer(h *harness, pr store.PR) {
	h.t.Helper()
	name := "mg-t-2-claude-fix"
	if _, err := h.st.CreateSession(h.ctx, store.Session{PRID: pr.ID, Role: "claude-fix", AgentName: &name, AgentKind: new("claude"),
		HerdrPaneID: new("p-claude-fix"), HerdrWorkspaceID: new("w1"), State: store.SessionLive}); err != nil {
		h.t.Fatal(err)
	}
	h.hd.mu.Lock()
	h.hd.agents = append(h.hd.agents, herdr.AgentInfo{PaneID: "p-claude-fix", Name: name, Agent: "claude", AgentStatus: herdr.StatusWorking})
	h.hd.mu.Unlock()
}

// A fixer that outlived its daemon is stopped before the loop is declared
// lost: it is interrupted until quiet and the slot goes back to the reviewed
// head, so it cannot push or reply as the operator after magnum said "fix
// run lost", and the next round's guard finds no drift.
func TestARestartStopsAFixerThatSurvivedIt(t *testing.T) {
	var kh *keyHerdr
	h, _ := fixHarness(t, withKeys(&kh))
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	liveFixer(h, pr)
	if err := h.e.setFixState(h.ctx, pr.ID, FixState{Cycle: 1, Cap: 3, Phase: FixPhaseFixing, Head: fixHead, Role: "claude-fix"}); err != nil {
		t.Fatal(err)
	}

	h.startup()
	if got, want := kh.all(), []string{"agent:mg-t-2-claude-fix:ctrl+c", "agent:mg-t-2-claude-fix:ctrl+c"}; !slices.Equal(got, want) {
		t.Errorf("keys %v, want %v: the surviving fixer was not interrupted", got, want)
	}
	if !slices.Contains(h.sl.all(), "resync:review1:"+fixHead) {
		t.Errorf("slot calls %v lack the resync to the reviewed head", h.sl.all())
	}
	if cur := h.wantState(2, store.PRNeedsAttention); deref(cur.LastError) != "fix run lost in a daemon restart" {
		t.Errorf("last_error = %q", deref(cur.LastError))
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the lost loop goes on")
	}
}

// A fix job that panics after the fixer got its prompt stops the fixer and
// puts the slot back at the reviewed head before it fails the loop, as a
// failed run does: the fixer must not go on acting as the operator.
func TestAFixJobThatPanicsStopsTheFixer(t *testing.T) {
	var kh *keyHerdr
	h, git := fixHarness(t, withKeys(&kh))
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	h.ag.mu.Lock()
	h.ag.onSubmit = func(store.Run, string) {
		h.hd.mu.Lock()
		h.hd.agents = append(h.hd.agents, herdr.AgentInfo{PaneID: "p-claude-fix", Name: "mg-t-2-claude-fix", Agent: "claude",
			AgentStatus: herdr.StatusWorking})
		h.hd.mu.Unlock()
		git.setHead("c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3") // the fixer committed
		panic("boom in the fix job")
	}
	h.ag.mu.Unlock()
	h.fix(FixPayload{})
	h.tick()

	if got := kh.all(); !slices.Contains(got, "agent:mg-t-2-claude-fix:ctrl+c") {
		t.Errorf("keys %v: the fixer was not interrupted", got)
	}
	if !slices.Contains(h.sl.all(), "resync:review1:"+fixHead) {
		t.Errorf("slot calls %v lack the resync to the reviewed head", h.sl.all())
	}
	if cur := h.wantState(2, store.PRNeedsAttention); !strings.Contains(deref(cur.LastError), "panic: boom in the fix job") {
		t.Errorf("last_error = %q", deref(cur.LastError))
	}
	if r := h.fixRuns(pr.ID)[0]; r.State != store.RunAbandoned && r.State != store.RunFailed {
		t.Errorf("fix run %s", r.State)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on")
	}
}

// `magnum abort` between the fixer's push and its re-review takes the
// forced re-review back and ends the loop, so no row is left that would
// refuse `magnum review` with no loop running.
func TestAbortEndsALoopWaitingForItsReReview(t *testing.T) {
	h, git := fixHarness(t)
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	fixer(h, git, fixPushed, func(run store.Run) string { return fixReport(run.ID, "b2b2b2b Fix", fixReplied, "none") })
	h.fix(FixPayload{})
	h.handle()
	if st, ok := h.fixStateOf(2); !ok || st.Phase != FixPhaseReviewing {
		t.Fatalf("fix state = %+v, %v", st, ok)
	}
	id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	if res := h.wantDone(id); !strings.Contains(res, "took back the forced review") || !strings.Contains(res, "fix loop ended (cycle 1 of 3, reviewing)") {
		t.Errorf("answer = %q", res)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop's row outlived the abort")
	}
	h.wantState(2, store.PRReviewed)
	review := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	h.wantDone(review)
}

// A loop's row holds the PR's rounds by itself, with or without its job:
// in phase fixing, and when it cannot be read (no round starts over a loop
// that may run); the wait says why.
func TestAFixingRowHoldsDispatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  string
	}{
		{name: "fixing", row: `{"cycle":1,"cap":3,"phase":"fixing"}`},
		{name: "unreadable", row: "not json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := fixHarness(t)
			pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
			h.e.setKV(h.ctx, KVPRFix(pr.ID), tc.row)
			rounds := len(h.rd.all())
			h.advance(time.Hour)
			reqPoll(h, prSpec{n: 2, head: "c3c3c3c", author: "zhuravel"})
			h.advance(time.Hour)
			h.tick()
			h.wantState(2, store.PRRereviewPending)
			if got := len(h.rd.all()); got != rounds {
				t.Errorf("rounds = %d, want %d", got, rounds)
			}
			if w := waitOf(t, h, 2); w.Reason != WaitFixing {
				t.Errorf("wait = %+v", w)
			}
		})
	}
}

// A second loop on a PR the first one left in needs_attention says why again:
// its failure replaces the PR's message and toasts, although the first
// loop's attention toast was within the dedupe window.
func TestASecondLoopOnANeedsAttentionPRSaysWhyAgain(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P1": 1})
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "nothing", fixReplied, "- 2101 P1 app/models/offer.rb:42: wants a schema change")
	})
	h.fix(FixPayload{})
	h.tick()
	h.wantState(2, store.PRNeedsAttention)
	h.wantToasts("needs attention", 1)

	h.advance(10 * time.Minute)
	fixer(h, git, "", func(store.Run) string { return "" }) // the second run writes no report of its own
	id := h.fix(FixPayload{})
	h.tick()
	h.wantDone(id)
	runs := h.fixRuns(pr.ID)
	if len(runs) != 2 {
		t.Fatalf("fix runs = %d, want 2", len(runs))
	}
	cur := h.wantState(2, store.PRNeedsAttention)
	if le := deref(cur.LastError); !strings.HasPrefix(le, "fix run failed: ") || !strings.Contains(le, deref(runs[1].ReportPath)) {
		t.Errorf("last_error = %q, want the second run's failure", le)
	}
	h.wantToasts("needs attention", 2)
}

// A re-review that cannot be forced after the fixer's push moved the PR's
// head still leaves the push seen: the head change is recorded as the
// poller records one and the normal re-review path takes over (the PR waits
// for its re-review, the loop ends), so the poller's later sight of the
// head changes nothing and the push is not lost unreviewed.
func TestAFailedForceAfterAPushStillQueuesItsReReview(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	// The force's compare-and-set fails: the registry refuses forced = 1 on the PR.
	if _, err := h.st.DB().ExecContext(h.ctx, fmt.Sprintf(`CREATE TRIGGER refuse_force BEFORE UPDATE OF forced ON prs
  WHEN NEW.id = %d AND NEW.forced = 1 BEGIN SELECT RAISE(ABORT, 'the force is refused'); END`, pr.ID)); err != nil {
		t.Fatal(err)
	}
	fixer(h, git, fixPushed, func(run store.Run) string { return fixReport(run.ID, "b2b2b2b Fix", fixReplied, "none") })
	h.fix(FixPayload{})
	h.handle()
	if _, err := h.st.DB().ExecContext(h.ctx, "DROP TRIGGER refuse_force"); err != nil {
		t.Fatal(err)
	}

	cur := h.pr(2)
	if cur.HeadSHA != fixPushed || cur.Forced || (cur.State != store.PRRereviewPending && cur.State != store.PRNeedsAttention) {
		t.Fatalf("PR = state %s head %s forced %v, want waiting for the re-review of %s", cur.State, cur.HeadSHA, cur.Forced, fixPushed)
	}
	if n := h.eventCount("pr:talkable/talkable#2", "pr.head_changed"); n != 1 {
		t.Errorf("pr.head_changed events = %d, want 1", n)
	}
	if got := h.e.pushTimes(h.ctx, pr.ID); len(got) == 0 || !got[len(got)-1].Equal(h.clock.Now()) {
		t.Errorf("recorded pushes %v lack the fixer's", got)
	}
	if ev, ok := h.fixEvent("fix.failed"); !ok || !strings.Contains(ev.Message, "the re-review could not be queued") {
		t.Errorf("fix.failed = %+v, %v", ev, ok)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on")
	}

	h.advance(time.Minute)
	reqPollNoSettle(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})
	h.settle()
	if n := h.eventCount("pr:talkable/talkable#2", "pr.head_changed"); n != 1 {
		t.Errorf("pr.head_changed events after the poll = %d, want 1", n)
	}
}
