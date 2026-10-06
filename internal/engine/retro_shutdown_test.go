package engine

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// retroEvents are the retro's start and done events so far.
func retroEvents(t *testing.T, h *harness) []string {
	t.Helper()
	evs, err := h.st.EventsOfKindsSince(h.ctx, time.Time{}, "retro.start", "retro.done")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		out = append(out, ev.Kind)
	}
	return out
}

// Every daemon restart logged a retro that started during the shutdown and
// stopped at once ("retro …: 0 PR(s) … stopped by the daemon's shutdown",
// with "append event retro.start: context canceled"), and `magnum status`
// then reported the retro as stopped: the tick a shutdown cancels reads no
// pause, no drain and no day already done (every registry read fails), so
// it found the day's retro due. A tick on a cancelled context starts no
// retro and leaves the last summary alone; the day's retro still runs on the
// next live tick.
func TestAShutdownStartsNoRetro(t *testing.T) {
	h := newHarness(t, withClassifier(&fakeClassifier{}), func(h *harness) {
		h.cfg.Learn.Enabled = true
		h.cfg.Learn.DailyAt = "09:00" // the clock is past it
	})
	retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	stopped, cancel := context.WithCancel(h.ctx)
	cancel()
	_ = h.e.Tick(stopped)
	h.settle()
	if _, ok, _ := h.st.GetKV(h.ctx, KVRetroLast); ok {
		t.Fatal("a retro ran on the tick the shutdown cancelled")
	}
	if evs := retroEvents(t, h); len(evs) != 0 {
		t.Fatalf("retro events on the cancelled tick: %v", evs)
	}
	if started, _ := h.e.startRetro(stopped, retroSpec{daily: true, lookback: 7 * 24 * time.Hour}); started {
		h.settle()
		t.Fatal("startRetro started a retro on a cancelled context")
	}

	h.tick()
	if evs := retroEvents(t, h); !slices.Equal(evs, []string{"retro.start", "retro.done"}) {
		t.Fatalf("retro events after the restart: %v", evs)
	}
	if sum := h.retroLast(); sum.PRs != 1 || sum.Stopped != "" {
		t.Fatalf("the day's retro after the restart: %+v", sum)
	}
	if day, _, _ := h.st.GetKV(h.ctx, KVRetroDay); day != store.DayKey(h.clock.Now()) {
		t.Fatalf("retro day = %q", day)
	}
}

// A daily retro the shutdown cuts short is neither done nor failed: the day
// is not recorded, the last summary is not replaced by a stopped one (which
// `magnum status` would show until the next retro), its done event is no
// warning, and the next live tick runs the day's retro again.
func TestADailyRetroCutShortByAShutdownRunsAgain(t *testing.T) {
	fc := &fakeClassifier{}
	var h *harness
	h = newHarness(t, withClassifier(fc), func(h *harness) {
		h.cfg.Learn.Enabled = true
		h.cfg.Learn.DailyAt = "09:00"
	})
	retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	fc.answer = func(ClassifyJob) (ClassifyResult, error) {
		h.e.stopRetro()
		return ClassifyResult{}, context.Canceled
	}
	h.tick()
	if v, ok, _ := h.st.GetKV(h.ctx, KVRetroLast); ok {
		t.Fatalf("the cut-short retro left a summary: %s", v)
	}
	if day, ok, _ := h.st.GetKV(h.ctx, KVRetroDay); ok {
		t.Fatalf("the cut-short retro recorded the day %q", day)
	}
	evs, err := h.st.EventsOfKindsSince(h.ctx, time.Time{}, "retro.done")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Level != "info" || !strings.Contains(evs[0].Message, "stopped by the daemon's shutdown") {
		t.Fatalf("retro.done = %+v", evs)
	}

	fc.mu.Lock()
	fc.answer = nil
	fc.mu.Unlock()
	h.tick()
	if sum := h.retroLast(); sum.Classified != 1 || sum.Stopped != "" {
		t.Fatalf("the day's retro on the next tick: %+v", sum)
	}
	if day, _, _ := h.st.GetKV(h.ctx, KVRetroDay); day != store.DayKey(h.clock.Now()) {
		t.Fatalf("retro day = %q", day)
	}
}

// A daemon draining for a restart starts no background job either: neither
// the day's retro nor a notes curation, whoever asks (startRetro and
// startCurate refuse), and a curation due when the daemon stops waits for
// the next live tick.
func TestADrainingOrStoppingDaemonStartsNoRetroOrCuration(t *testing.T) {
	h, _, _ := curationHarness(t, goodProposal)
	h.e.syncNotes(h.ctx, false) // a reconcile measures and marks: the notes are due a curation
	h.advance(curateCheckEvery)
	if err := h.st.SetKV(h.ctx, KVDaemonDraining, store.FormatTime(h.clock.Now())); err != nil {
		t.Fatal(err)
	}
	repo, err := h.st.RepoByFullName(h.ctx, "talkable/talkable")
	if err != nil {
		t.Fatal(err)
	}
	if started, _ := h.e.startRetro(h.ctx, retroSpec{lookback: time.Hour}); started {
		h.settle()
		t.Fatal("a retro started while draining")
	}
	if started, _, _ := h.e.startCurate(h.ctx, repo, CurateTriggerRequest); started {
		h.settle()
		t.Fatal("a curation started while draining")
	}
	_ = h.st.DeleteKV(h.ctx, KVDaemonDraining)

	stopped, cancel := context.WithCancel(h.ctx)
	cancel()
	_ = h.e.Tick(stopped)
	h.settle()
	if started, _, _ := h.e.startCurate(stopped, repo, CurateTriggerRequest); started {
		h.settle()
		t.Fatal("startCurate started a curation on a cancelled context")
	}
	if n := len(proposals(t, h)); n != 0 {
		t.Fatalf("%d proposals from the stopping daemon", n)
	}
	evs, err := h.st.EventsBySubject(h.ctx, "notes:talkable/talkable", 0)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(evs, func(ev store.Event) bool { return ev.Kind == "notes.curate_start" }) {
		t.Fatal("a curation started while the daemon stopped")
	}

	h.advance(curateCheckEvery)
	h.tick()
	if n := len(proposals(t, h)); n != 1 {
		t.Fatalf("%d proposals on the next live tick, want 1", n)
	}
}
