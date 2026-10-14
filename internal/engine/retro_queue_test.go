package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// learnStarts lists the starts of retros and notes curations in the order
// they happened.
func learnStarts(t *testing.T, h *harness) []string {
	t.Helper()
	rows, err := h.st.DB().QueryContext(h.ctx, "SELECT kind FROM events WHERE kind IN ('retro.start', 'notes.curate_start') ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatal(err)
		}
		out = append(out, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// `magnum retro` asked while another learn job runs is queued, not refused
// (on 2026-10-14 the operator met "no retro now: the notes curation of ...
// runs" twice and polled by hand): the answer names the job and when it
// started, the retro waits while the job runs and starts once it ended,
// with the lookback and the PRs the request named, once.
func TestARetroAskedDuringACurationQueuesAndRuns(t *testing.T) {
	fc := &fakeClassifier{}
	h := newHarness(t, withClassifier(fc))
	retroPR(h, 7, 25*time.Hour)
	named := retroPR(h, 8, 25*time.Hour)
	seedRetroGitHub(h, 7)
	seedRetroGitHub(h, 8)
	end := fakeLearnJob(h, "curation")
	since := h.clock.Now().Local().Format("15:04")

	req := h.requestRetro(RetroPayload{PRs: []int64{named.ID}, Lookback: "3d"})
	want := "queued: starts when the notes curation of example/tools (started " + since + ") ends; `magnum logs` follows it"
	if req.State != store.RequestDone || !strings.Contains(deref(req.Result), want) {
		t.Fatalf("a retro request during a curation = %s %q, want %q", req.State, deref(req.Result), want)
	}
	if q, ok := h.e.queuedRetro(h.ctx); !ok || q.Lookback != "72h0m0s" || !slices.Equal(q.PRs, []int64{named.ID}) || !q.Again {
		t.Fatalf("queued retro = %+v, %v", q, ok)
	}
	if evs := h.events("retro.queued"); len(evs) != 1 || !strings.Contains(evs[0].Message, "the notes curation of example/tools") {
		t.Fatalf("retro.queued events = %+v", evs)
	}
	h.tick()
	if h.e.retroBusy() || len(h.events("retro.start")) != 0 {
		t.Fatal("the queued retro started while the curation ran")
	}

	end()
	h.tick()
	starts := h.events("retro.start")
	if len(starts) != 1 || !strings.Contains(starts[0].Message, "within 72h0m0s") {
		t.Fatalf("retro.start events once the curation ended = %+v", starts)
	}
	fc.mu.Lock()
	jobs := slices.Clone(fc.jobs)
	fc.mu.Unlock()
	if len(jobs) != 1 || jobs[0].PR.ID != named.ID {
		t.Fatalf("the queued retro classified %d PRs, want only the one it named", len(jobs))
	}
	if q, ok := h.e.queuedRetro(h.ctx); ok {
		t.Fatalf("the retro stayed queued once it started: %+v", q)
	}
	h.tick()
	if n := len(h.events("retro.start")); n != 1 {
		t.Fatalf("%d retros started, want 1", n)
	}
}

// A retro and a notes curation queued behind the same learn job: the retro
// goes first, since it feeds the curations with misses, and the curation
// starts once it ended.
func TestAQueuedRetroRunsBeforeAQueuedCuration(t *testing.T) {
	fc := &fakeClassifier{}
	h, _ := missesHarness(t, fc, &fakeCurator{write: goodProposal}, func(h *harness) { h.cfg.Notes.Curate = config.CurateTriggers{} })
	retroPR(h, 7, 25*time.Hour)
	seedRetroGitHub(h, 7)
	end := fakeLearnJob(h, "curation")
	if msg, err := h.e.requestCurate(h.ctx, NotesCuratePayload{Repo: "talkable/talkable"}); err != nil || !strings.Contains(msg, "queued") {
		t.Fatalf("requestCurate = %q, %v", msg, err)
	}
	if req := h.requestRetro(RetroPayload{}); req.State != store.RequestDone || !strings.Contains(deref(req.Result), "queued") {
		t.Fatalf("requestRetro = %s %q", req.State, deref(req.Result))
	}
	if got := learnStarts(t, h); len(got) != 0 {
		t.Fatalf("started behind a running curation: %v", got)
	}

	end()
	h.tick()
	h.tick()
	if got := learnStarts(t, h); !slices.Equal(got, []string{"retro.start", "notes.curate_start"}) {
		t.Fatalf("starts = %v, want the retro first", got)
	}
	if ps := proposals(t, h); len(ps) != 1 || len(queued(t, h)) != 0 {
		t.Fatalf("proposals = %+v, queue %+v", ps, queued(t, h))
	}
}

// A curation asked for while a queued retro waits for its turn (its job
// ended, the next tick has not started it yet) is queued behind it rather
// than taking its place.
func TestACurationAskedWhileARetroWaitsQueuesBehindIt(t *testing.T) {
	fc := &fakeClassifier{}
	h, _ := missesHarness(t, fc, &fakeCurator{write: goodProposal}, func(h *harness) { h.cfg.Notes.Curate = config.CurateTriggers{} })
	retroPR(h, 7, 25*time.Hour)
	seedRetroGitHub(h, 7)
	end := fakeLearnJob(h, "curation")
	h.requestRetro(RetroPayload{})
	end()
	asked := h.clock.Now().Local().Format("15:04")
	msg, err := h.e.requestCurate(h.ctx, NotesCuratePayload{Repo: "talkable/talkable"})
	if want := "queued: starts after the retro queued at " + asked; err != nil || !strings.Contains(msg, want) {
		t.Fatalf("requestCurate = %q, %v, want %q", msg, err, want)
	}
	if h.e.curateBusy() {
		t.Fatal("the curation started ahead of the queued retro")
	}
	h.tick()
	h.tick()
	if got := learnStarts(t, h); !slices.Equal(got, []string{"retro.start", "notes.curate_start"}) {
		t.Fatalf("starts = %v, want the retro first", got)
	}
}

// One retro waits at a time: a second request while one is queued says so
// and changes nothing; the first one runs, once.
func TestASecondRetroRequestWhileOneIsQueuedSaysSo(t *testing.T) {
	fc := &fakeClassifier{}
	h := newHarness(t, withClassifier(fc))
	retroPR(h, 7, 25*time.Hour)
	seedRetroGitHub(h, 7)
	end := fakeLearnJob(h, "curation")
	asked := h.clock.Now().Local().Format("15:04")
	h.requestRetro(RetroPayload{Lookback: "3d"})
	h.advance(time.Minute)
	second := h.requestRetro(RetroPayload{Lookback: "5d", Again: true})
	if want := "a retro is queued already (asked " + asked + ")"; second.State != store.RequestDone || !strings.Contains(deref(second.Result), want) {
		t.Fatalf("a second request = %s %q, want %q", second.State, deref(second.Result), want)
	}
	if q, ok := h.e.queuedRetro(h.ctx); !ok || q.Lookback != "72h0m0s" || q.Again {
		t.Fatalf("queued retro after the second request = %+v, %v", q, ok)
	}
	if n := len(h.events("retro.queued")); n != 1 {
		t.Fatalf("%d retro.queued events, want 1", n)
	}

	end()
	h.tick()
	h.tick()
	if starts := h.events("retro.start"); len(starts) != 1 || !strings.Contains(starts[0].Message, "within 72h0m0s") {
		t.Fatalf("retro.start events = %+v", starts)
	}
}

// A queued retro whose job ends during a drain stays queued (a request
// during the drain is refused, as before), is kept across the restart the
// drain ends in, waits out a pause of its agent's CLI and starts once
// nothing holds it.
func TestAQueuedRetroWaitsOutADrain(t *testing.T) {
	fc := &fakeClassifier{}
	h := newHarness(t, withClassifier(fc))
	retroPR(h, 7, 25*time.Hour)
	seedRetroGitHub(h, 7)
	end := fakeLearnJob(h, "curation")
	h.requestRetro(RetroPayload{Lookback: "3d"})
	if err := h.st.SetKV(h.ctx, KVDaemonDraining, store.FormatTime(h.clock.Now())); err != nil {
		t.Fatal(err)
	}
	end()
	h.tick()
	if _, ok := h.e.queuedRetro(h.ctx); !ok || len(h.events("retro.start")) != 0 {
		t.Fatal("the queued retro started, or left the queue, during the drain")
	}
	if req := h.requestRetro(RetroPayload{}); req.State != store.RequestFailed || !strings.Contains(deref(req.Result), "no retro now: draining") {
		t.Fatalf("a request during the drain = %s %q", req.State, deref(req.Result))
	}

	h.startup() // the restart the drain waited for
	if _, ok := h.e.queuedRetro(h.ctx); !ok {
		t.Fatal("the restart dropped the queued retro")
	}
	kind := h.cfg.LearnRole().AgentKind()
	h.e.setKV(h.ctx, store.KVToolPausedUntil(kind), store.FormatTime(h.clock.Now().Add(time.Hour)))
	h.e.setKV(h.ctx, store.KVToolPausedReason(kind), "usage_limit")
	h.tick()
	if _, ok := h.e.queuedRetro(h.ctx); !ok || len(h.events("retro.start")) != 0 {
		t.Fatal("the queued retro started, or left the queue, while its CLI was paused")
	}

	h.e.delKV(h.ctx, store.KVToolPausedUntil(kind), store.KVToolPausedReason(kind))
	h.tick()
	if starts := h.events("retro.start"); len(starts) != 1 || !strings.Contains(starts[0].Message, "within 72h0m0s") {
		t.Fatalf("retro.start events once nothing holds it = %+v", starts)
	}
	if _, ok := h.e.queuedRetro(h.ctx); ok {
		t.Fatal("the retro stayed queued once it started")
	}
}
