package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// rebootAgain is the refusal of a request under a drain for a reboot alone
// that you ask again (again) after it, with the rest of its words.
func rebootAgain(again, rest string) string {
	return "the daemon drains for a reboot (`magnum drain`): " + again + " after the reboot, or after `magnum resume`; " + rest
}

// Under a drain for a reboot the answers name it, never a restart that does
// not come: a `magnum fix` held, a merge of the parent, a CI fix and a
// `magnum fix --decide` refused (before or as the drain began), a retro and
// a curation queued.
func TestTheAnswersUnderARebootDrainNameIt(t *testing.T) {
	storetest.Parallel(t)
	t.Run("fix", func(t *testing.T) {
		h, _ := fixHarness(t)
		fixablePR(h, "zhuravel", map[string]int{"P2": 2})
		h.rebootDrain(70, DrainUntilBoot)
		id := h.fix(FixPayload{})
		h.handle()
		if res := h.wantDone(id); !strings.Contains(res, "drain for a reboot") || strings.Contains(res, "restart") {
			t.Fatalf("magnum fix under the drain = %q, want it held for the drain for a reboot", res)
		}
	})
	t.Run("merge of the parent", func(t *testing.T) {
		h, mg := mergeHarness(t, withFixerApp)
		fixablePR(h, "zhuravel", nil)
		mg.behind = true
		h.rebootDrain(71, DrainUntilBoot)
		id := h.fix(FixPayload{Mode: FixModeMerge})
		h.handle()
		if r := h.request(id); !strings.Contains(deref(r.Result), "drains for a reboot") || strings.Contains(deref(r.Result), "restart") {
			t.Fatalf("merge-parent under the drain = %s %q, want refused for the drain for a reboot", r.State, deref(r.Result))
		}
	})
	t.Run("CI fix", func(t *testing.T) {
		h, _ := ciFixHarness(t, github.CheckPassed)
		h.rebootDrain(75, DrainUntilBoot)
		h.wantCIFixRefused(rebootAgain("ask again", "a CI fix is never held"))
	})
	t.Run("fix --decide", func(t *testing.T) {
		h, _ := fixLeftADecision(t, nil)
		h.rebootDrain(76, DrainUntilBoot)
		id := h.fix(FixPayload{Decide: []FixDecision{{Thread: decideThread, Text: "Keep it."}}})
		h.handle()
		want := rebootAgain("run `magnum fix --decide` on talkable/talkable#2 again", "nothing was posted")
		if r := h.request(id); r.State != store.RequestFailed || deref(r.Result) != want {
			t.Fatalf("--decide under the drain = %s %q, want refused with %q", r.State, deref(r.Result), want)
		}
	})
	t.Run("fix --decide as the drain begins", func(t *testing.T) {
		h, _ := fixLeftADecision(t, nil)
		h.gh.mu.Lock()
		h.gh.onThreads = func() { // decideCheck read no drain; the start after it does
			if err := h.st.SetKV(h.ctx, KVDaemonRebootDrain, RebootDrain{Generation: 77, Since: h.clock.Now(), Until: DrainUntilBoot, Boot: bootOne}.Value()); err != nil {
				t.Error(err)
			}
		}
		h.gh.mu.Unlock()
		id := h.fix(FixPayload{Decide: []FixDecision{{Thread: decideThread, Text: "Keep it."}}})
		h.handle()
		want := rebootAgain("run `magnum fix --decide` again", "nothing was posted")
		if r := h.request(id); r.State != store.RequestFailed || deref(r.Result) != want {
			t.Fatalf("--decide as the drain begins = %s %q, want refused with %q", r.State, deref(r.Result), want)
		}
		if n := h.gh.count("reply:"); n != 0 {
			t.Errorf("a refused --decide posted %d replies", n)
		}
	})
	t.Run("retro", func(t *testing.T) {
		h := newHarness(t, withClassifier(&fakeClassifier{}))
		retroPR(h, 7, 25*time.Hour)
		seedRetroGitHub(h, 7)
		h.rebootDrain(72, DrainUntilBoot)
		r := h.requestRetro(RetroPayload{Lookback: "3d"})
		if !strings.Contains(deref(r.Result), "drain for a reboot") || strings.Contains(deref(r.Result), "restart") {
			t.Fatalf("a retro under the drain = %q, want it queued for the drain for a reboot", deref(r.Result))
		}
	})
	t.Run("curation", func(t *testing.T) {
		h, _, _ := curationHarness(t, goodProposal)
		h.rebootDrain(73, DrainUntilBoot)
		msg, err := h.e.requestCurate(h.ctx, NotesCuratePayload{Repo: "talkable/talkable"})
		if err != nil || !strings.Contains(msg, "drain for a reboot") || strings.Contains(msg, "restart") {
			t.Fatalf("a curation under the drain = %q, %v, want it queued for the drain for a reboot", msg, err)
		}
	})
}

// A review asked for that a drain for a reboot holds toasts once after 15
// minutes, timed from the drain's own record (a restart drain's row is
// another).
func TestARequestedReviewHeldByARebootDrainToastsOnce(t *testing.T) {
	h := newHarness(t, noEveryReview)
	h.reviewedPR(2, "b1")
	h.rebootDrain(74, DrainUntilBoot)
	h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	h.advance(15*time.Minute - 30*time.Second)
	h.tick()
	if len(toastsWith(h, "can't start")) != 0 {
		t.Fatal("toasted before 15 minutes")
	}
	h.advance(time.Minute)
	h.tick()
	h.flush()
	if got := toastsWith(h, "can't start"); len(got) != 1 || !strings.Contains(got[0], "drain for a reboot") {
		t.Fatalf("toasts = %q, want one naming the drain for a reboot", h.nh.shown())
	}
}
