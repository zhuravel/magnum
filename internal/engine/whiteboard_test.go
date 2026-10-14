package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
// Whiteboard readiness check (down: Whiteboard is not running; hold, when
// set, keeps the check from answering until it is closed).
type wbEnv struct {
	*harness
	run  *execx.Fake
	down atomic.Bool
	hold atomic.Pointer[chan struct{}]
}

// wbHarness builds a wbEnv; auto switches [pipeline] whiteboard on, so a
// posted review starts the job by itself.
func wbHarness(t *testing.T, auto bool, mods ...func(*harness)) *wbEnv {
	t.Helper()
	w := &wbEnv{}
	w.run = &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"whiteboard", "api", "whiteboard_status"}, Fn: func(execx.Cmd) (execx.Result, error) {
		if hold := w.hold.Load(); hold != nil {
			<-*hold
		}
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
	return w.wbReviewedBy("")
}

// wbReviewedBy is wbReviewed with PR #2 by author ("" = the fake's default).
func (w *wbEnv) wbReviewedBy(author string) store.PR {
	w.t.Helper()
	w.open(prSpec{n: 1, head: "base1"})
	w.startup()
	w.tick() // first sync: #1 baseline
	w.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead, author: author})
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

// wbAnswered handles the pending requests and waits until request id has
// its answer: a whiteboard request is answered off the tick, and the job it
// starts may still run when the answer comes.
func (w *wbEnv) wbAnswered(id int64) {
	w.t.Helper()
	w.e.handleRequests(w.ctx)
	w.wbAwaitAnswer(id)
}

// wbAwaitAnswer waits until request id is no longer pending.
func (w *wbEnv) wbAwaitAnswer(id int64) {
	w.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); w.request(id).State == store.RequestPending; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			w.t.Fatalf("request %d was never answered", id)
		}
	}
}

// wbAtWork starts the whiteboard job on PR #2 by request and waits until
// its writer got its prompt and herdr shows it working, as a writer is
// until it ends its run (the test sets no wbWriter).
func (w *wbEnv) wbAtWork() {
	w.t.Helper()
	start := w.wbRequest()
	w.wbAnswered(start)
	w.wantDone(start)
	w.wbAwaitSubmit()
	w.hd.mu.Lock()
	w.hd.agents = append(w.hd.agents, herdr.AgentInfo{PaneID: "p-claude-whiteboard", Name: "mg-t-2-claude-whiteboard", Agent: "claude",
		AgentStatus: herdr.StatusWorking})
	w.hd.mu.Unlock()
}

// wbAnswers are the request.* events of request id: a request is answered
// once.
func (w *wbEnv) wbAnswers(id int64) []store.Event {
	w.t.Helper()
	evs, err := w.st.EventsBySubject(w.ctx, fmt.Sprintf("request:%d", id), 0)
	if err != nil {
		w.t.Fatal(err)
	}
	return slices.DeleteFunc(evs, func(ev store.Event) bool { return !strings.HasPrefix(ev.Kind, "request.") })
}

// wbYielded hands request id, which a whiteboard job at work on PR #2
// yields to, through the yield: the first pass leaves it unanswered while
// the job stops, the job's end hands it back, and the next pass answers it.
func (w *wbEnv) wbYielded(id int64) {
	w.t.Helper()
	w.e.handleRequests(w.ctx)
	if r := w.request(id); r.State != store.RequestPending {
		w.t.Fatalf("request %d answered while the whiteboard job held the PR: %s %q", id, r.State, deref(r.Result))
	}
	w.settle() // the job stops and lets go of the PR
	w.e.handleRequests(w.ctx)
	w.settle()
}

// wbWantSuperseded fails unless the whiteboard job on PR #2 stopped for a
// request by: whiteboard.stopped names it, the writer got ctrl+c, its run is
// abandoned, the slot is back at the head, the job let go of the PR and
// request id was answered once. It returns the answer.
func (w *wbEnv) wbWantSuperseded(kh *keyHerdr, id int64, by string) string {
	w.t.Helper()
	pr := w.pr(2)
	ev, ok := w.fixEvent("whiteboard.stopped")
	if !ok || !strings.Contains(ev.Message, "stopped: superseded by "+by) || !strings.Contains(ev.Message, "writer interrupted") {
		w.t.Errorf("whiteboard.stopped = %+v, %v", ev, ok)
	}
	if got := kh.all(); !slices.Contains(got, "agent:mg-t-2-claude-whiteboard:ctrl+c") {
		w.t.Errorf("keys %v: the writer was not interrupted", got)
	}
	if runs := w.wbRuns(pr.ID); len(runs) != 1 || runs[0].State != store.RunAbandoned || deref(runs[0].Error) != "stopped: superseded by "+by {
		w.t.Errorf("whiteboard runs = %+v", runs)
	}
	if !slices.Contains(w.sl.all(), "resync:review1:"+fixHead) {
		w.t.Errorf("slot calls %v, want the slot back at the head", w.sl.all())
	}
	if w.e.whiteboardHolds(pr.ID) {
		w.t.Error("the whiteboard job still holds the PR")
	}
	res := w.wantDone(id)
	if got := w.wbAnswers(id); len(got) != 1 {
		w.t.Errorf("request %d answered %d times: %+v", id, len(got), got)
	}
	return res
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
	w.wbAnswered(id)
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
	w.wbAnswered(id)
	if res := w.wantDone(id); res != "writing the whiteboard of a1a1a1a in review1" {
		t.Errorf("answer = %q", res)
	}
	w.wbAwaitSubmit()
	rounds := len(w.rd.all())
	w.advance(10 * time.Minute) // within the writer's 30m: past it, the job's next look could time the run out before the push is seen
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
	w.wbAtWork()

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
	w.wbAnswered(start)
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
	w.wbAnswered(id)
	w.wbFailed(id, whiteboardDownText)
	w.down.Store(false)

	if !w.e.reserve(pr.ID, &roundHandle{cancel: func() {}}) {
		t.Fatal("reserve")
	}
	id = w.wbRequest()
	w.wbAnswered(id)
	w.wbFailed(id, "round in progress; try again after it")
	w.e.unreserve(pr.ID)

	slot := w.slot("review1")
	if err := w.st.UpdateSlotFields(w.ctx, slot.ID, func(u *store.SlotUpdate) { u.Set("checked_out_sha", "c3c3c3c3") }); err != nil {
		t.Fatal(err)
	}
	id = w.wbRequest()
	w.wbAnswered(id)
	w.wbFailed(id, "slot review1 holds c3c3c3c, not the reviewed head a1a1a1a")
	if err := w.st.UpdateSlotFields(w.ctx, slot.ID, func(u *store.SlotUpdate) { u.Set("checked_out_sha", fixHead) }); err != nil {
		t.Fatal(err)
	}

	no := w.enqueue(ReqWhiteboard, WhiteboardPayload{PRTarget: PRTarget{Ref: "1"}})
	w.wbAnswered(no)
	w.wbFailed(no, "no review on base1")

	w.cfg.Pipeline.WhiteboardRole = ""
	id = w.wbRequest()
	w.wbAnswered(id)
	w.wbFailed(id, "no whiteboard role: whiteboard_role is empty")
	w.cfg.Pipeline.WhiteboardRole = "claude-whiteboard"

	id = w.wbRequest()
	w.wbAnswered(id)
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
// run, interrupts the writer, which herdr kept at work in its pane, until
// quiet, puts the slot back at the run's head and records whiteboard.failed,
// asking nothing of the operator.
func TestAWhiteboardRunLostInARestartFails(t *testing.T) {
	var kh *keyHerdr
	w := wbHarness(t, false, withKeys(&kh))
	pr := w.wbReviewed()
	run, err := w.st.CreateRun(w.ctx, store.Run{ID: "r-wb-lost", PRID: pr.ID, Round: 1, Role: "claude-whiteboard", Kind: store.RunWhiteboard,
		TargetSHA: fixHead, Identity: pr.Identity, ReviewerLogin: "x", State: store.RunPending})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.st.TransitionRun(w.ctx, run.ID, []string{store.RunPending}, store.RunWorking, nil); err != nil {
		t.Fatal(err)
	}
	name := "mg-t-2-claude-whiteboard"
	if _, err := w.st.CreateSession(w.ctx, store.Session{PRID: pr.ID, Role: "claude-whiteboard", AgentName: &name, AgentKind: new("claude"),
		HerdrPaneID: new("p-claude-whiteboard"), HerdrWorkspaceID: new("w1"), State: store.SessionLive}); err != nil {
		t.Fatal(err)
	}
	w.hd.mu.Lock()
	w.hd.agents = append(w.hd.agents, herdr.AgentInfo{PaneID: "p-claude-whiteboard", Name: name, Agent: "claude", AgentStatus: herdr.StatusWorking})
	w.hd.mu.Unlock()
	resyncs := slices.Clone(w.sl.all())

	w.startup()
	if r, err := w.st.RunByID(w.ctx, run.ID); err != nil || r.State != store.RunAbandoned {
		t.Errorf("the lost run: %+v, %v", r, err)
	}
	if got, want := kh.all(), []string{"agent:" + name + ":ctrl+c", "agent:" + name + ":ctrl+c"}; !slices.Equal(got, want) {
		t.Errorf("keys %v, want %v: the writer that outlived the daemon was not interrupted", got, want)
	}
	if got := w.sl.all()[len(resyncs):]; !slices.Contains(got, "resync:review1:"+fixHead) {
		t.Errorf("slot calls since the restart %v, want the slot back at the run's head", got)
	}
	if ev, ok := w.fixEvent("whiteboard.failed"); !ok || ev.Message != whiteboardLostText || !strings.Contains(string(ev.Data), run.ID) {
		t.Errorf("whiteboard.failed = %+v, %v", ev, ok)
	}
	w.wantState(2, store.PRReviewed)
	w.flush()
	w.wantToasts("needs attention", 0)
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
// whiteboard.failed says why (the writer's `failed:` line: that it failed
// and where its report is, never the line's sentence, which may quote the
// PR; "no report"), the slot is resynced, and the PR stays reviewed, with no
// record and no toast.
func TestAFailedWhiteboardAsksNothingOfTheOperator(t *testing.T) {
	const sentence = "The PR's title says DROP TABLE accounts."
	for _, tc := range []struct {
		name, report, want string
	}{
		{"failed", "failed: " + sentence + "\n", "claude-whiteboard failed (its report: "},
		{"no report", "", "no report"},
		{"another head", "session: " + wbSession + "\nhead: " + fixPushed + "\n", "not the two lines"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := wbHarness(t, true)
			wbWriter(w.harness, func(store.Run) string { return tc.report })
			pr := w.wbReviewed()
			runs := w.wbRuns(pr.ID)
			if len(runs) != 1 || runs[0].State != store.RunFailed {
				t.Fatalf("whiteboard runs = %+v", runs)
			}
			ev, ok := w.fixEvent("whiteboard.failed")
			if !ok || !strings.Contains(ev.Message, tc.want) {
				t.Errorf("whiteboard.failed = %+v, %v", ev, ok)
			}
			report := deref(runs[0].ReportPath)
			if tc.name == "failed" && (ev.Message != "claude-whiteboard failed (its report: "+report+")" || !strings.Contains(string(ev.Data), report)) {
				t.Errorf("whiteboard.failed = %q (data %s), want the role and the report %s", ev.Message, ev.Data, report)
			}
			evs, err := w.st.EventsBySubject(w.ctx, "pr:talkable/talkable#2", 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, ev := range evs {
				if strings.Contains(ev.Message, "DROP TABLE") || strings.Contains(string(ev.Data), "DROP TABLE") {
					t.Errorf("event %s carries the report's sentence: %q %s", ev.Kind, ev.Message, ev.Data)
				}
			}
			if strings.Contains(deref(runs[0].Error), "DROP TABLE") {
				t.Errorf("the run's error carries the report's sentence: %q", deref(runs[0].Error))
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
	w.wbAnswered(start)
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

// A review request on a PR whose whiteboard is being written stops the
// writer and goes on as an ordinary request: the whiteboard is optional, and
// the moment after a review posts is the operator's. whiteboard.stopped says
// the request superseded it, the writer's session got ctrl+c, the slot is
// back at the head, and the request, answered once, queued the PR's forced
// round.
func TestAReviewRequestStopsTheWhiteboardJob(t *testing.T) {
	var kh *keyHerdr
	w := wbHarness(t, false, withKeys(&kh))
	w.wbReviewed()
	w.wbAtWork()

	id := w.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	w.wbYielded(id)
	if res := w.wbWantSuperseded(kh, id, "magnum review"); !strings.HasPrefix(res, "queued talkable/talkable#2 (rereview_pending, forced)") {
		t.Errorf("answer = %q, want the review request's own", res)
	}
	if cur := w.wantState(2, store.PRRereviewPending); !cur.Forced {
		t.Errorf("PR #2 = %+v, want its round forced", cur)
	}
}

// A fix request on a PR whose whiteboard is being written stops the writer
// and starts the fix loop it asks for, with the fix request's own answer.
func TestAFixRequestStopsTheWhiteboardJob(t *testing.T) {
	var kh *keyHerdr
	w := wbHarness(t, false, withKeys(&kh))
	w.wbReviewedBy("zhuravel")
	w.wbAtWork()
	w.ag.mu.Lock()
	w.ag.onSubmit = func(run store.Run, _ string) { // the fixer ends its run at once, with no report
		if run.Kind != store.RunFix {
			return
		}
		if err := w.st.TransitionRun(w.ctx, run.ID, []string{store.RunWorking}, store.RunEnded, nil); err != nil {
			t.Error(err)
		}
	}
	w.ag.mu.Unlock()

	id := w.fix(FixPayload{})
	w.wbYielded(id)
	if res := w.wbWantSuperseded(kh, id, "magnum fix"); !strings.HasPrefix(res, "fixing talkable/talkable#2: claude-fix fixes the 1 finding of the review on a1a1a1a in review1") {
		t.Errorf("answer = %q, want the fix request's own", res)
	}
	if runs := w.fixRuns(w.pr(2).ID); len(runs) != 1 {
		t.Errorf("fix runs = %+v, want the requested cycle's", runs)
	}
}

// A fix request's dry run on a PR whose whiteboard is being written says
// what the request would do, stopping the writer first, and stops nothing.
func TestAFixDryRunLeavesTheWhiteboardJobAtWork(t *testing.T) {
	w := wbHarness(t, false)
	w.wbReviewedBy("zhuravel")
	w.wbAtWork()
	id := w.fix(FixPayload{DryRun: true})
	w.e.handleRequests(w.ctx)
	if res := w.wantDone(id); !strings.HasPrefix(res, "dry run: would start fixing talkable/talkable#2") || !strings.Contains(res, "would stop the whiteboard writer first") {
		t.Errorf("answer = %q", res)
	}
	if !w.e.whiteboardHolds(w.pr(2).ID) {
		t.Error("a dry run stopped the whiteboard job")
	}
	stop := w.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	w.e.handleRequests(w.ctx)
	w.settle()
	w.wantDone(stop)
}

// `magnum open` on a PR whose whiteboard is being written stops the writer
// and restores the PR for the operator, with the open request's own answer.
func TestAnOpenRequestStopsTheWhiteboardJob(t *testing.T) {
	var kh *keyHerdr
	w := wbHarness(t, false, withKeys(&kh))
	pr := w.wbReviewed()
	w.wbAtWork()
	judge, err := w.st.LiveSessionByPRRole(w.ctx, pr.ID, "codex-judge") // parked since the round: nothing live to focus
	if err != nil {
		t.Fatal(err)
	}
	if err := w.st.TransitionSession(w.ctx, judge.ID, []string{store.SessionLive}, store.SessionParked, nil); err != nil {
		t.Fatal(err)
	}

	id := w.enqueue(ReqOpen, OpenPayload{PRTarget: PRTarget{Ref: "2"}})
	w.wbYielded(id)
	if res := w.wbWantSuperseded(kh, id, "magnum open"); !strings.HasPrefix(res, "restored talkable/talkable#2 in review1 at a1a1a1a (pinned;") {
		t.Errorf("answer = %q, want the open request's own", res)
	}
	if cur := w.pr(pr.Number); !cur.Pinned {
		t.Error("the restored PR is not pinned")
	}
}

// `magnum whiteboard` on a PR whose slot is broken or lost refuses with the
// slot's last error and the command that repairs it, as `magnum fix` and
// dispatch's wait name them, not with the bare state.
func TestAWhiteboardRefusedForABrokenSlotNamesTheRepair(t *testing.T) {
	for _, tc := range []struct{ state, lastError, want string }{
		{store.SlotBroken, "schema reset failed",
			"talkable/talkable#2: slot review1 is broken: schema reset failed (magnum slots repair review1)"},
		{store.SlotLost, "", "talkable/talkable#2: slot review1 is lost (magnum slots repair review1)"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			w := wbHarness(t, false)
			w.wbReviewed()
			sl := w.slot("review1")
			if err := w.st.TransitionSlot(w.ctx, sl.ID, []string{sl.State}, tc.state, func(u *store.SlotUpdate) {
				if tc.lastError != "" {
					u.Set("last_error", tc.lastError)
				}
			}); err != nil {
				t.Fatal(err)
			}
			id := w.wbRequest()
			w.wbAnswered(id)
			if r := w.request(id); r.State != store.RequestFailed || deref(r.Result) != tc.want {
				t.Errorf("%s %q, want failed %q", r.State, deref(r.Result), tc.want)
			}
			if w.ag.count("submit:") != 0 {
				t.Error("the writer started in an unusable slot")
			}
		})
	}
}

// The Whiteboard readiness check of `magnum whiteboard` runs off the tick:
// with the check hanging, the tick goes on and the request stays pending,
// and once the check answers the request gets its answer and the job runs.
func TestAWhiteboardRequestDoesNotBlockTheTick(t *testing.T) {
	w := wbHarness(t, false)
	wbWriter(w.harness, wbDone(wbSession))
	w.wbReviewed()
	hold := make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release)
	w.hold.Store(&hold)

	id := w.wbRequest()
	ticked := make(chan error, 1)
	go func() { ticked <- w.e.Tick(w.ctx) }()
	select {
	case err := <-ticked:
		if err != nil {
			t.Fatalf("tick: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the tick waits for the Whiteboard readiness check")
	}
	if r := w.request(id); r.State != store.RequestPending {
		t.Errorf("request %d answered before the readiness check did: %s %q", id, r.State, deref(r.Result))
	}
	release()
	w.wbAwaitAnswer(id)
	w.settle()
	if res := w.wantDone(id); res != "writing the whiteboard of a1a1a1a in review1" {
		t.Errorf("answer = %q", res)
	}
	if rec, ok := w.wbRecord(); !ok || rec.Head != fixHead {
		t.Errorf("whiteboard record = %+v, %v", rec, ok)
	}
}
