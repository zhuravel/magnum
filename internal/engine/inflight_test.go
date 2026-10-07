package engine

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
)

// awaitSignal waits for a goroutine to reach a fake's blocking point.
func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never started", what)
	}
}

// A restart for a new build counted only review rounds as in flight, so it
// cut a running notes curation or retro short. It waits for them as it
// waits for rounds; the CLI reads them from KVNotesCurating and
// KVRetroRunning, which go once they ended.
func TestANewBuildWaitsForARunningCurationOrRetro(t *testing.T) {
	t.Run("a notes curation", func(t *testing.T) {
		release, entered := make(chan struct{}), make(chan struct{}, 1)
		h, _, _ := curationHarness(t, func(s notes.Scratch) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			goodProposal(s)
		})
		bin := restartOnNewBuild(h, func() error { return nil })
		if _, err := h.e.requestCurate(h.ctx, NotesCuratePayload{Repo: "talkable/talkable"}); err != nil {
			t.Fatal(err)
		}
		awaitSignal(t, entered, "the curation")
		waitsThenRestarts(t, h, bin, release, KVNotesCurating)
	})
	t.Run("a retro", func(t *testing.T) {
		release, entered := make(chan struct{}), make(chan struct{}, 1)
		fc := &fakeClassifier{}
		h := newHarness(t, withClassifier(fc))
		fc.answer = func(ClassifyJob) (ClassifyResult, error) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			return ClassifyResult{}, errors.New("no answer")
		}
		retroPR(h, 7, 24*time.Hour)
		seedRetroGitHub(h, 7)
		bin := restartOnNewBuild(h, func() error { return nil })
		h.enqueue(ReqRetro, RetroPayload{})
		h.e.handleRequests(h.ctx)
		awaitSignal(t, entered, "the retro")
		waitsThenRestarts(t, h, bin, release, KVRetroRunning)
	})
}

// waitsThenRestarts checks that a checked new build waits while the work
// marked by key runs, then restarts the daemon once release lets it end.
func waitsThenRestarts(t *testing.T, h *harness, bin string, release chan struct{}, key string) {
	t.Helper()
	rebuild(t, bin, time.Now().Add(time.Hour))
	h.e.lastReconcile = time.Time{}
	for range 2 {
		if err := h.e.Tick(h.ctx); err != nil {
			close(release)
			t.Fatalf("tick while it runs: %v", err)
		}
	}
	if evs := h.events("daemon.restart_pending"); len(evs) != 1 {
		close(release)
		t.Fatalf("restart_pending events = %+v", evs)
	}
	if v, ok := kvValue(h, key); !ok || v == "" {
		close(release)
		t.Fatalf("%s is not set while it runs", key)
	}
	close(release)
	h.settle()
	if err := h.e.Tick(h.ctx); !errors.Is(err, ErrRestartForBuild) {
		t.Fatalf("tick after it ended: %v, want ErrRestartForBuild", err)
	}
	if v, ok := kvValue(h, key); ok {
		t.Fatalf("%s outlived it: %q", key, v)
	}
}

// `magnum notes <repo> --curate` promises a toast when the proposal is
// ready; a requested curation that stopped (its curator could not start)
// or ended invalid wrote only a warn event. Each toasts once; a curation
// the daemon started on its own stays quiet, it is tried again.
func TestARequestedCurationThatStopsOrFailsSaysSo(t *testing.T) {
	toasts := func(h *harness) string {
		h.advance(2 * time.Minute)
		h.tick()
		return strings.Join(h.nh.all(), "\n")
	}
	t.Run("stopped", func(t *testing.T) {
		h, _, _ := curationHarness(t, goodProposal, func(h *harness) {
			h.d.Curator = func(context.Context, CurateRun) (Curator, error) { return nil, errors.New("herdr is not running") }
		})
		if _, err := h.e.requestCurate(h.ctx, NotesCuratePayload{Repo: "talkable/talkable"}); err != nil {
			t.Fatal(err)
		}
		h.settle()
		if got := toasts(h); strings.Count(got, "notes curation for talkable/talkable stopped") != 1 || !strings.Contains(got, "herdr is not running") {
			t.Fatalf("toasts: %s", got)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		h, _, _ := curationHarness(t, func(s notes.Scratch) {
			goodProposal(s)
			_ = os.WriteFile(s.Proposal(), []byte(curatedNotes+"- see #11920\n"), 0o600)
		})
		if _, err := h.e.requestCurate(h.ctx, NotesCuratePayload{Repo: "talkable/talkable"}); err != nil {
			t.Fatal(err)
		}
		h.settle()
		ps := proposals(t, h)
		if len(ps) != 1 || ps[0].State != store.ProposalInvalid {
			t.Fatalf("proposals = %+v", ps)
		}
		got := toasts(h)
		if strings.Count(got, "notes curation for talkable/talkable is invalid") != 1 || strings.Contains(got, "11920") {
			t.Fatalf("toasts: %s", got)
		}
	})
	t.Run("on its own", func(t *testing.T) {
		h, _, _ := curationHarness(t, goodProposal, func(h *harness) {
			h.d.Curator = func(context.Context, CurateRun) (Curator, error) { return nil, errors.New("herdr is not running") }
		})
		h.advance(curateCheckEvery)
		h.tick() // the scan finds the repository past its limits
		if evs := notesEvents(t, h, "notes.curate_stopped"); len(evs) != 1 {
			t.Fatalf("curate_stopped events = %+v", evs)
		}
		if got := toasts(h); strings.Contains(got, "notes curation") {
			t.Fatalf("toasts: %s", got)
		}
	})
}
