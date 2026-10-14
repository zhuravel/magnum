package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// wbSession is the Whiteboard session the fake writer reports.
const wbSession = "6f0c2b9e-4d1a-4c3e-9b7a-2e5d8f1a0c3b"

// wbEnv is a harness for the whiteboard job: its git knows the slot's HEAD
// (fixGit), its waits take a real millisecond, its rounds post a review the
// registry records (wbPostsReviews), and its exec runner answers the
// Whiteboard readiness check (down: Whiteboard is not running).
type wbEnv struct {
	*harness
	run  *execx.Fake
	down atomic.Bool
}

// wbHarness builds a wbEnv; auto switches [pipeline] whiteboard on, so a
// posted review starts the job by itself.
func wbHarness(t *testing.T, auto bool, mods ...func(*harness)) *wbEnv {
	t.Helper()
	w := &wbEnv{}
	w.run = &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"whiteboard", "api", "whiteboard_status"}, Fn: func(execx.Cmd) (execx.Result, error) {
		if w.down.Load() {
			return execx.Result{Code: 1, Stderr: []byte("not ready")}, errors.New("whiteboard: exit status 1")
		}
		return execx.Result{Stdout: []byte(`{"ready":true}`)}, nil
	}}}}
	base := []func(*harness){realSleep, func(h *harness) {
		h.d.Runner = w.run
		h.cfg.Pipeline.Whiteboard = auto
	}}
	h, _ := fixHarness(t, append(base, mods...)...)
	w.harness = h
	wbPostsReviews(h)
	return w
}

// wbPostsReviews makes h's rounds post as the fake pipeline does, with the
// review's judge run recorded as a posted one (its URL and result), which
// the whiteboard job reads (Store.LastReviewSummaries), and the result
// naming the head it reviewed.
func wbPostsReviews(h *harness) {
	h.rd.mu.Lock()
	defer h.rd.mu.Unlock()
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		res, err := h.rd.posted(h.ctx, in, len(h.rd.all()))
		if err != nil {
			return res, err
		}
		res.TargetSHA = in.TargetSHA
		res.ReviewURL = fmt.Sprintf("https://github.com/talkable/talkable/pull/%d#pullrequestreview-%d", in.PR.Number, res.ReviewID)
		run, err := h.st.CreateRun(h.ctx, store.Run{ID: fmt.Sprintf("run-review-%d-%d", in.PR.Number, res.Round), PRID: in.PR.ID, Round: res.Round,
			Role: "codex-judge", Kind: store.RunInitial, TargetSHA: in.TargetSHA, Identity: in.PR.Identity, ReviewerLogin: "talkable[bot]",
			State: store.RunPending, PromptText: "p"})
		if err == nil {
			err = h.st.TransitionRun(h.ctx, run.ID, nil, store.RunVerified, func(u *store.RunUpdate) {
				u.Set("outcome", pipeline.OutcomePosted)
				u.Set("review_id", res.ReviewID)
				u.Set("review_url", res.ReviewURL)
				u.Set("result_json", `{"event":"COMMENTED","findings":{"P2":1}}`)
			})
		}
		if err != nil {
			return failRound(h.t, err)
		}
		return res, nil
	}
}

// wbWriter makes h's fake writer: once its prompt is sent it writes
// report(run) to the run's report path ("" = it writes none) and ends its
// run, as the observer does after two idle ticks.
func wbWriter(h *harness, report func(run store.Run) string) {
	h.ag.mu.Lock()
	defer h.ag.mu.Unlock()
	h.ag.onSubmit = func(run store.Run, _ string) {
		if text := report(run); text != "" {
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
		if err := h.st.TransitionRun(h.ctx, run.ID, []string{store.RunWorking}, store.RunEnded, func(u *store.RunUpdate) {
			u.Set("ended_at", h.clock.Now())
		}); err != nil {
			h.t.Error(err)
		}
	}
}

// wbDone is the report of a writer that wrote session's whiteboard of its
// run's head.
func wbDone(session string) func(run store.Run) string {
	return func(run store.Run) string { return "session: " + session + "\nhead: " + run.TargetSHA + "\n" }
}

// wbReviewed has PR #2 reviewed at fixHead by a posted round.
func (w *wbEnv) wbReviewed() store.PR {
	w.t.Helper()
	w.open(prSpec{n: 1, head: "base1"})
	w.startup()
	w.tick() // first sync: #1 baseline
	w.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead})
	w.tick() // #2 queued
	w.advance(5 * time.Minute)
	w.tick() // dispatched and reviewed; the job, if any, runs to its end
	return w.wantState(2, store.PRReviewed)
}

// wbRuns are the PR's runs of kind whiteboard, oldest first.
func (w *wbEnv) wbRuns(prID int64) []store.Run {
	w.t.Helper()
	runs, err := w.st.RunsByPR(w.ctx, prID)
	if err != nil {
		w.t.Fatal(err)
	}
	return slices.DeleteFunc(runs, func(r store.Run) bool { return r.Kind != store.RunWhiteboard })
}

// wbRecord is PR #2's whiteboard record (ok false: none).
func (w *wbEnv) wbRecord() (WhiteboardRecord, bool) {
	w.t.Helper()
	v, ok := kvValue(w.harness, KVPRWhiteboard(w.pr(2).ID))
	if !ok {
		return WhiteboardRecord{}, false
	}
	rec, parsed := ParseWhiteboardRecord(v)
	if !parsed {
		w.t.Fatalf("whiteboard record %q does not parse", v)
	}
	return rec, true
}

// wbAwaitSubmit waits until the writer's prompt was sent.
func (w *wbEnv) wbAwaitSubmit() {
	w.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); w.ag.count("submit:claude-whiteboard:") == 0; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			w.t.Fatalf("the writer's prompt was never sent: %v", w.ag.all())
		}
	}
}

// wbRequest asks for the whiteboard of PR #2 and returns the request.
func (w *wbEnv) wbRequest() int64 {
	w.t.Helper()
	return w.enqueue(ReqWhiteboard, WhiteboardPayload{PRTarget: PRTarget{Ref: "2"}})
}

// wbFailed fails unless request id failed with an answer that has want.
func (w *wbEnv) wbFailed(id int64, want string) {
	w.t.Helper()
	if r := w.request(id); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), want) {
		w.t.Errorf("request %d: %s %q, want a refusal with %q", id, r.State, deref(r.Result), want)
	}
}

// A review magnum posts on a head starts the whiteboard job when the watch
// asks for whiteboards: the job holds the PR, runs the whiteboard role in
// the slot with a run of kind whiteboard in the review's round, whose
// prompt names the checkout, the base and the review, and records
// whiteboard.start; the writer's two-line report then records the
// whiteboard of the head (the kv record), whiteboard.done, a verified run
// and the slot resynced at the head.
func TestAPostedReviewStartsTheWhiteboardJob(t *testing.T) {
	w := wbHarness(t, true)
	reserved := atomic.Bool{}
	wbWriter(w.harness, func(run store.Run) string {
		reserved.Store(w.e.roundActive(run.PRID))
		return wbDone(wbSession)(run)
	})
	pr := w.wbReviewed()
	if !reserved.Load() {
		t.Error("the writer ran without the job holding the PR")
	}
	runs := w.wbRuns(pr.ID)
	if len(runs) != 1 {
		t.Fatalf("whiteboard runs = %+v", runs)
	}
	run := runs[0]
	wantReport := filepath.Join(w.layout.ReviewDir("talkable", "talkable", 2, fixHead), "claude-whiteboard.md")
	if run.Role != "claude-whiteboard" || run.Round != 1 || run.State != store.RunVerified || deref(run.Outcome) != "done" || deref(run.ReportPath) != wantReport {
		t.Errorf("whiteboard run = %+v (report %s, outcome %s)", run, deref(run.ReportPath), deref(run.Outcome))
	}
	slot := w.slot("review1")
	reviewFile := filepath.Join(w.layout.ReviewDir("talkable", "talkable", 2, fixHead), "review.json")
	wantPrompt := fmt.Sprintf("whiteboard_prompt:claude-whiteboard:%s|talkable/talkable|%s|origin/master|%s|%s|", run.ID, slot.Path,
		"https://github.com/talkable/talkable/pull/2#pullrequestreview-101", reviewFile)
	if !slices.Contains(w.ag.all(), wantPrompt) {
		t.Errorf("no %q in %v", wantPrompt, w.ag.all())
	}
	if ev, ok := w.fixEvent("whiteboard.start"); !ok || !strings.Contains(string(ev.Data), run.ID) || !strings.Contains(string(ev.Data), fixHead) {
		t.Errorf("whiteboard.start = %+v, %v", ev, ok)
	}
	rec, ok := w.wbRecord()
	if !ok || rec.Session != wbSession || rec.Head != fixHead || rec.Role != "claude-whiteboard" || !rec.At.Equal(w.clock.Now()) {
		t.Errorf("whiteboard record = %+v, %v", rec, ok)
	}
	if ev, ok := w.fixEvent("whiteboard.done"); !ok || !strings.Contains(string(ev.Data), wbSession) || !strings.Contains(ev.Message, wbSession) {
		t.Errorf("whiteboard.done = %+v, %v", ev, ok)
	}
	if !slices.Contains(w.sl.all(), "resync:review1:"+fixHead) {
		t.Errorf("slot calls %v, want the slot resynced at the head", w.sl.all())
	}
	if fix := w.ag.paneEnv("claude-whiteboard"); fix == nil || fix["WT_BRANCH"] != "review1" {
		t.Errorf("the writer's pane env = %v", fix)
	} else if _, ok := fix["GH_CONFIG_DIR"]; ok {
		t.Errorf("the writer's pane env %v acts as the posting App, not as the poll identity", fix)
	}
	if w.e.roundActive(pr.ID) {
		t.Error("the job still holds the PR")
	}
	w.wantState(2, store.PRReviewed)
}

// Whiteboard down (its status check fails) skips the whiteboard: one
// whiteboard.skipped event that says how to start it, no run, no hold on
// the PR, no prompt.
func TestTheWhiteboardJobSkipsWhenWhiteboardIsNotRunning(t *testing.T) {
	w := wbHarness(t, true)
	w.down.Store(true)
	pr := w.wbReviewed()
	if runs := w.wbRuns(pr.ID); len(runs) != 0 {
		t.Errorf("whiteboard runs = %+v", runs)
	}
	ev, ok := w.fixEvent("whiteboard.skipped")
	if !ok || ev.Message != whiteboardDownText || ev.Level != "info" {
		t.Errorf("whiteboard.skipped = %+v, %v", ev, ok)
	}
	if w.e.roundActive(pr.ID) || w.ag.count("whiteboard_prompt:") != 0 {
		t.Errorf("the job held the PR or prompted: %v", w.ag.all())
	}
	if calls := w.run.CallsWithPrefix("whiteboard", "api", "whiteboard_status"); len(calls) != 1 || !calls[0].Probe || calls[0].Timeout != 15*time.Second {
		t.Errorf("status checks = %+v", calls)
	}
	if _, ok := w.wbRecord(); ok {
		t.Error("a skipped whiteboard left a record")
	}
}

// A head that has its whiteboard gets no second one: a later review posted
// on the same head starts nothing, and `magnum whiteboard` says where the
// whiteboard is.
func TestAHeadWithAWhiteboardGetsNoSecond(t *testing.T) {
	w := wbHarness(t, true)
	wbWriter(w.harness, wbDone(wbSession))
	pr := w.wbReviewed()
	if len(w.wbRuns(pr.ID)) != 1 {
		t.Fatalf("whiteboard runs = %+v", w.wbRuns(pr.ID))
	}
	w.e.whiteboardAfterRound(w.ctx, pr, pipeline.RoundResult{Outcome: pipeline.OutcomePosted, TargetSHA: fixHead})
	w.settle()
	if runs := w.wbRuns(pr.ID); len(runs) != 1 {
		t.Errorf("whiteboard runs = %+v, want the first alone", runs)
	}
	if w.hasEvent("pr:talkable/talkable#2", "whiteboard.skipped") {
		t.Error("a head that has its whiteboard recorded a skip")
	}
	id := w.wbRequest()
	w.e.handleRequests(w.ctx)
	w.wbFailed(id, "a1a1a1a has its whiteboard "+wbSession+"; magnum whiteboard talkable/talkable#2 --open opens it")
}

// A review of a new head moves the whiteboard of the earlier one: the
// recorded session reaches the prompt, and the record names the new head.
func TestANewHeadUpdatesTheWhiteboard(t *testing.T) {
	w := wbHarness(t, true)
	wbWriter(w.harness, wbDone(wbSession))
	pr := w.wbReviewed()
	w.advance(time.Hour)
	w.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixPushed})
	w.tick() // the push: queued
	w.advance(time.Hour)
	w.tick() // reviewed again; the job updates the whiteboard
	w.wantState(2, store.PRReviewed)
	runs := w.wbRuns(pr.ID)
	if len(runs) != 2 {
		t.Fatalf("whiteboard runs = %+v", runs)
	}
	prefix := "whiteboard_prompt:claude-whiteboard:" + runs[1].ID + "|"
	i := slices.IndexFunc(w.ag.all(), func(c string) bool { return strings.HasPrefix(c, prefix) })
	if i < 0 || !strings.HasSuffix(w.ag.all()[i], "|"+wbSession) {
		t.Errorf("the update's prompt input lacks the session %s: %v", wbSession, w.ag.all())
	}
	if rec, ok := w.wbRecord(); !ok || rec.Head != fixPushed || rec.Session != wbSession {
		t.Errorf("whiteboard record = %+v, %v", rec, ok)
	}
}

// A push while the writer works ends the job: whiteboard.stopped names the
// new head, the run is abandoned, no record is written, and the PR's next
// round can take it.
func TestAPushEndsTheWhiteboardJob(t *testing.T) {
	w := wbHarness(t, false)
	pr := w.wbReviewed()
	id := w.wbRequest()
	w.e.handleRequests(w.ctx)
	if res := w.wantDone(id); res != "writing the whiteboard of a1a1a1a in review1" {
		t.Errorf("answer = %q", res)
	}
	w.wbAwaitSubmit()
	rounds := len(w.rd.all())
	w.advance(time.Hour)
	w.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixPushed})
	w.tick() // the push; the job sees it at its next look and stops
	ev, ok := w.fixEvent("whiteboard.stopped")
	if !ok || !strings.Contains(ev.Message, "the head moved to b2b2b2b") {
		t.Errorf("whiteboard.stopped = %+v, %v", ev, ok)
	}
	if runs := w.wbRuns(pr.ID); len(runs) != 1 || runs[0].State != store.RunAbandoned {
		t.Errorf("whiteboard runs = %+v", runs)
	}
	if _, ok := w.wbRecord(); ok {
		t.Error("a stopped job left a record")
	}
	w.advance(time.Hour)
	w.tick()
	if got := len(w.rd.all()); got <= rounds {
		t.Errorf("rounds = %d, want the push's round after %d", got, rounds)
	}
}

// `magnum abort` ends a whiteboard job: the writer is interrupted, its run
// abandoned, the slot resynced at the head, whiteboard.stopped says
// aborted, and the PR stays reviewed with nothing for the operator.
func TestAbortEndsAWhiteboardJob(t *testing.T) {
	var kh *keyHerdr
	w := wbHarness(t, false, withKeys(&kh))
	pr := w.wbReviewed()
	start := w.wbRequest()
	w.e.handleRequests(w.ctx)
	w.wantDone(start)
	w.wbAwaitSubmit()
	w.hd.mu.Lock()
	w.hd.agents = append(w.hd.agents, herdr.AgentInfo{PaneID: "p-claude-whiteboard", Name: "mg-t-2-claude-whiteboard", Agent: "claude",
		AgentStatus: herdr.StatusWorking})
	w.hd.mu.Unlock()

	id := w.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	w.e.handleRequests(w.ctx)
	w.settle()
	res := w.wantDone(id)
	if !strings.Contains(res, "aborted the whiteboard of talkable/talkable#2") || !strings.Contains(res, "writer interrupted") {
		t.Errorf("answer = %q", res)
	}
	if got := kh.all(); !slices.Contains(got, "agent:mg-t-2-claude-whiteboard:ctrl+c") {
		t.Errorf("keys %v", got)
	}
	if runs := w.wbRuns(pr.ID); len(runs) != 1 || runs[0].State != store.RunAbandoned || deref(runs[0].Error) != abortedBy {
		t.Errorf("whiteboard runs = %+v", runs)
	}
	if ev, ok := w.fixEvent("whiteboard.stopped"); !ok || !strings.Contains(ev.Message, "aborted") {
		t.Errorf("whiteboard.stopped = %+v, %v", ev, ok)
	}
	if !slices.Contains(w.sl.all(), "resync:review1:"+fixHead) {
		t.Errorf("slot calls %v", w.sl.all())
	}
	w.wantState(2, store.PRReviewed)
	w.flush()
	w.wantToasts("needs attention", 0)
}

// While the writer works, a round of the PR waits: dispatch starts none,
// the wait reads "whiteboard in progress · claude-whiteboard", and the job
// is no review round (max_concurrent_reviews does not count it).
func TestTheWhiteboardJobHoldsDispatch(t *testing.T) {
	w := wbHarness(t, false)
	pr := w.wbReviewed()
	start := w.wbRequest()
	w.e.handleRequests(w.ctx)
	w.wantDone(start)
	w.wbAwaitSubmit()
	if err := w.st.TransitionPR(w.ctx, pr.ID, []string{store.PRReviewed}, store.PRRereviewPending, func(u *store.PRUpdate) {
		u.Set("forced", true)
	}); err != nil {
		t.Fatal(err)
	}
	rounds := len(w.rd.all())
	if err := w.e.Tick(w.ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(w.rd.all()); got != rounds {
		t.Errorf("rounds = %d, want %d: a round started while the writer works", got, rounds)
	}
	wait := waitOf(t, w.harness, 2)
	if wait.Reason != WaitWhiteboard || !strings.Contains(wait.Short(w.clock.Now()), "whiteboard in progress · claude-whiteboard") {
		t.Errorf("wait = %+v (%s)", wait, wait.Short(w.clock.Now()))
	}
	if got := w.e.heldReason(pr.ID); got != "the whiteboard is being written" {
		t.Errorf("held reason = %q", got)
	}
	if n := w.e.reviewRounds(); n != 0 {
		t.Errorf("review rounds = %d, want the job left out", n)
	}
}

// `magnum whiteboard` starts the job on the reviewed head whatever the
// watch's whiteboard key, and refuses in one line when there is no
// whiteboard role, no review on the head, a round or a job holds the PR,
// Whiteboard is down, or the slot does not hold the head.
func TestAWhiteboardRequestStartsTheJobAndItsRefusals(t *testing.T) {
	w := wbHarness(t, false)
	wbWriter(w.harness, wbDone(wbSession))
	pr := w.wbReviewed()
	if runs := w.wbRuns(pr.ID); len(runs) != 0 {
		t.Fatalf("whiteboard = false started a job: %+v", runs)
	}

	w.down.Store(true)
	id := w.wbRequest()
	w.e.handleRequests(w.ctx)
	w.wbFailed(id, whiteboardDownText)
	w.down.Store(false)

	if !w.e.reserve(pr.ID, &roundHandle{cancel: func() {}}) {
		t.Fatal("reserve")
	}
	id = w.wbRequest()
	w.e.handleRequests(w.ctx)
	w.wbFailed(id, "round in progress; try again after it")
	w.e.unreserve(pr.ID)

	slot := w.slot("review1")
	if err := w.st.UpdateSlotFields(w.ctx, slot.ID, func(u *store.SlotUpdate) { u.Set("checked_out_sha", "c3c3c3c3") }); err != nil {
		t.Fatal(err)
	}
	id = w.wbRequest()
	w.e.handleRequests(w.ctx)
	w.wbFailed(id, "slot review1 holds c3c3c3c, not the reviewed head a1a1a1a")
	if err := w.st.UpdateSlotFields(w.ctx, slot.ID, func(u *store.SlotUpdate) { u.Set("checked_out_sha", fixHead) }); err != nil {
		t.Fatal(err)
	}

	no := w.enqueue(ReqWhiteboard, WhiteboardPayload{PRTarget: PRTarget{Ref: "1"}})
	w.e.handleRequests(w.ctx)
	w.wbFailed(no, "no review on base1")

	w.cfg.Pipeline.WhiteboardRole = ""
	id = w.wbRequest()
	w.e.handleRequests(w.ctx)
	w.wbFailed(id, "no whiteboard role: whiteboard_role is empty")
	w.cfg.Pipeline.WhiteboardRole = "claude-whiteboard"

	id = w.wbRequest()
	w.e.handleRequests(w.ctx)
	w.settle()
	if res := w.wantDone(id); res != "writing the whiteboard of a1a1a1a in review1" {
		t.Errorf("answer = %q", res)
	}
	if rec, ok := w.wbRecord(); !ok || rec.Head != fixHead {
		t.Errorf("whiteboard record = %+v, %v", rec, ok)
	}
	if runs := w.wbRuns(pr.ID); len(runs) != 1 || runs[0].State != store.RunVerified {
		t.Errorf("whiteboard runs = %+v", runs)
	}
}

// A writer that a restart left at work is lost: the next start abandons its
// run and records whiteboard.failed, asking nothing of the operator.
func TestAWhiteboardRunLostInARestartFails(t *testing.T) {
	w := wbHarness(t, false)
	pr := w.wbReviewed()
	run, err := w.st.CreateRun(w.ctx, store.Run{ID: "r-wb-lost", PRID: pr.ID, Round: 1, Role: "claude-whiteboard", Kind: store.RunWhiteboard,
		TargetSHA: fixHead, Identity: pr.Identity, ReviewerLogin: "x", State: store.RunPending})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.st.TransitionRun(w.ctx, run.ID, []string{store.RunPending}, store.RunWorking, nil); err != nil {
		t.Fatal(err)
	}
	w.startup()
	if r, err := w.st.RunByID(w.ctx, run.ID); err != nil || r.State != store.RunAbandoned {
		t.Errorf("the lost run: %+v, %v", r, err)
	}
	if ev, ok := w.fixEvent("whiteboard.failed"); !ok || !strings.Contains(ev.Message, "lost in a restart") {
		t.Errorf("whiteboard.failed = %+v, %v", ev, ok)
	}
	w.wantState(2, store.PRReviewed)
}

// Whiteboards are off by default: a posted review starts nothing and asks
// Whiteboard nothing.
func TestWhiteboardOffStartsNothing(t *testing.T) {
	run := &execx.Fake{}
	h, _ := fixHarness(t, realSleep, func(h *harness) { h.d.Runner = run })
	if h.cfg.Pipeline.Whiteboard || h.cfg.WhiteboardFor(h.cfg.WatchFor("talkable/talkable")) {
		t.Fatal("whiteboards are on by default")
	}
	wbPostsReviews(h)
	pr := h.reviewedPR(2, fixHead)
	runs, err := h.st.RunsByPR(h.ctx, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(runs, func(r store.Run) bool { return r.Kind == store.RunWhiteboard }) || h.hasEvent("pr:talkable/talkable#2", "whiteboard.skipped") {
		t.Errorf("runs %+v", runs)
	}
	if calls := run.CallsWithPrefix("whiteboard"); len(calls) != 0 {
		t.Errorf("whiteboard calls = %+v", calls)
	}
}

// The job lays out the writer's pane alone: a pane laid out for a round
// role nobody starts would leave a starting session behind.
func TestTheWhiteboardJobLaysOutOnlyItsPane(t *testing.T) {
	w := wbHarness(t, true)
	wbWriter(w.harness, wbDone(wbSession))
	pr := w.wbReviewed()
	last := "" // the round's own workspace call comes first; the job's is the last
	for _, c := range w.ag.all() {
		if strings.HasPrefix(c, fmt.Sprintf("ensure_workspace:%d:", pr.ID)) {
			last = c
		}
	}
	if layout := last[strings.LastIndex(last, ":")+1:]; last == "" || layout != "claude-whiteboard" {
		t.Errorf("the whiteboard job's workspace layout = %q, want only the writer's pane", layout)
	}
}

// A whiteboard that fails asks nothing of the operator: the run fails,
// whiteboard.failed carries the writer's sentence (or "no report"), the slot
// is resynced, and the PR stays reviewed, with no record and no toast.
func TestAFailedWhiteboardAsksNothingOfTheOperator(t *testing.T) {
	for _, tc := range []struct {
		name, report, want string
	}{
		{"failed", "failed: Whiteboard answered that it is not running.\n", "Whiteboard answered that it is not running."},
		{"no report", "", "no report"},
		{"another head", "session: " + wbSession + "\nhead: " + fixPushed + "\n", "not the two lines"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := wbHarness(t, true)
			wbWriter(w.harness, func(store.Run) string { return tc.report })
			pr := w.wbReviewed()
			if runs := w.wbRuns(pr.ID); len(runs) != 1 || runs[0].State != store.RunFailed {
				t.Errorf("whiteboard runs = %+v", runs)
			}
			if ev, ok := w.fixEvent("whiteboard.failed"); !ok || !strings.Contains(ev.Message, tc.want) {
				t.Errorf("whiteboard.failed = %+v, %v", ev, ok)
			}
			if _, ok := w.wbRecord(); ok {
				t.Error("a failed whiteboard left a record")
			}
			if !slices.Contains(w.sl.all(), "resync:review1:"+fixHead) {
				t.Errorf("slot calls %v", w.sl.all())
			}
			w.wantState(2, store.PRReviewed)
			w.flush()
			w.wantToasts("needs attention", 0)
			if got := toastsWith(w.harness, "whiteboard"); len(got) != 0 {
				t.Errorf("toasts = %v", got)
			}
		})
	}
}

// A close on GitHub stops a whiteboard job, which a closed PR's reader
// never reads: the writer's run is abandoned and whiteboard.stopped says
// the PR closed.
func TestAClosedPRStopsTheWhiteboardJob(t *testing.T) {
	w := wbHarness(t, false)
	pr := w.wbReviewed()
	start := w.wbRequest()
	w.e.handleRequests(w.ctx)
	w.wantDone(start)
	w.wbAwaitSubmit()
	cancel := w.e.stopClosedRound(pr.ID, closeStop{state: "CLOSED", count: 2})
	if cancel == nil {
		t.Fatal("a close does not stop the whiteboard job")
	}
	cancel()
	w.settle()
	if ev, ok := w.fixEvent("whiteboard.stopped"); !ok || !strings.Contains(ev.Message, "the PR closed") {
		t.Errorf("whiteboard.stopped = %+v, %v", ev, ok)
	}
	if runs := w.wbRuns(pr.ID); len(runs) != 1 || runs[0].State != store.RunAbandoned {
		t.Errorf("whiteboard runs = %+v", runs)
	}
	if w.e.roundActive(pr.ID) {
		t.Error("the job still holds the PR")
	}
}
