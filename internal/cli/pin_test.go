package cli

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/cleanup"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

func TestPinFromThePluginWorkspace(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_1", "w_7")
	h.pid = 10
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestDone, "pinned talkable/talkable#5 (slot review3)") }
	if code := h.cmd("pin", "--workspace", "w_7", "--cwd", ""); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqPin {
		t.Fatalf("requests %+v", reqs)
	}
	p := actDecode[engine.TargetPayload](t, reqs[0].Payload)
	if p.Repo != "talkable/talkable" || p.Number != 5 || p.Slot != "" {
		t.Fatalf("payload %+v", p)
	}
	actContains(t, h.out.String(), "pinned talkable/talkable#5 (slot review3)")
}

func TestTargetsBySlotAndDirectory(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	slotDir := filepath.Join(h.home, "talkable.review3")
	sl, err := h.st.CreateSlot(h.ctx, store.Slot{Name: "review3", RepoFullName: "talkable/talkable", Kind: store.SlotKindPool, Path: slotDir, MainClone: h.home, State: store.SlotFree})
	if err != nil {
		t.Fatal(err)
	}
	free, err := h.st.CreateSlot(h.ctx, store.Slot{Name: "review4", RepoFullName: "talkable/talkable", Kind: store.SlotKindPool, Path: filepath.Join(h.home, "talkable.review4"), MainClone: h.home, State: store.SlotFree})
	if err != nil {
		t.Fatal(err)
	}
	_ = free
	if err := h.st.TransitionSlot(h.ctx, sl.ID, nil, store.SlotHeld, func(u *store.SlotUpdate) { u.Set("pr_id", pr.ID) }); err != nil {
		t.Fatal(err)
	}

	// A directory inside the slot names its PR.
	if code := h.cmd("mute", "--cwd", filepath.Join(slotDir, "app", "models")); code != 0 {
		t.Fatalf("mute exit %d: %s", code, h.errb.String())
	}
	// A free slot by name is a slot target for pin, but not for mute.
	if code := h.cmd("pin", "review4"); code != 0 {
		t.Fatalf("pin exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.errb.String(), "pin slot review4 is queued as request 2 and applies when the daemon starts")
	h.errb.Reset()
	if code := h.cmd("unmute", "review4"); code != 1 || !strings.Contains(h.errb.String(), "slot review4 holds no PR") {
		t.Fatalf("unmute slot exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests %+v", reqs)
	}
	if p := actDecode[engine.TargetPayload](t, reqs[0].Payload); reqs[0].Kind != engine.ReqMute || p.Number != 5 {
		t.Errorf("mute %s %+v", reqs[0].Kind, p)
	}
	if p := actDecode[engine.TargetPayload](t, reqs[1].Payload); reqs[1].Kind != engine.ReqPin || p.Slot != "review4" || p.Number != 0 {
		t.Errorf("pin %s %+v", reqs[1].Kind, p)
	}
	h.errb.Reset()
	if code := h.cmd("pin", "nonsense-slot"); code != 1 || !strings.Contains(h.errb.String(), "neither a PR") {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
}

// `magnum mute <ref> [reason…]` sends the words after the PR as the mute's
// reason; the other target verbs still take one target.
func TestMuteSendsItsReason(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRQueued)
	if code := h.cmd("mute", "5", "waits", "for", "the", "author's", "rework"); code != 0 {
		t.Fatalf("mute exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqMute {
		t.Fatalf("requests %+v", reqs)
	}
	if p := actDecode[engine.TargetPayload](t, reqs[0].Payload); p.Number != 5 || p.Reason != "waits for the author's rework" {
		t.Fatalf("mute payload %+v", p)
	}
	h.errb.Reset()
	if code := h.cmd("pin", "5", "because"); code != 2 || !strings.Contains(h.errb.String(), "one target at a time") {
		t.Fatalf("pin with two words: exit %d: %s", code, h.errb.String())
	}
}

func TestReleaseHandsOffToTheDaemonAndWaits(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.held, h.pid = true, 7
	h.onSleep = func(h *actHarness) {
		if h.sleeps == 3 {
			h.completePending(store.RequestDone, "Plan: 1 action\nreleased review3")
		}
	}
	if code := h.cmd("release", "5", "--force", "--wait"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	p := actDecode[engine.TargetPayload](t, reqs[0].Payload)
	if reqs[0].Kind != engine.ReqRelease || !p.Force || p.Number != 5 {
		t.Fatalf("request %s %+v", reqs[0].Kind, p)
	}
	if h.cleaner.applied != 0 {
		t.Error("released in-process although the daemon runs")
	}
	actContains(t, h.out.String(), "released review3")
}

func TestReleaseRunsInProcessWithoutDaemon(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.cleaner.plan = cleanup.Plan{Actions: []cleanup.Action{{Kind: "release", Slot: "review3"}}}
	if code := h.cmd("release", "talkable#5"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if len(h.cleaner.opts) != 1 || h.cleaner.opts[0].PR == nil || h.cleaner.opts[0].PR.Number != 5 || h.cleaner.applied != 1 {
		t.Fatalf("cleanup opts %+v applied %d", h.cleaner.opts, h.cleaner.applied)
	}
	if h.locked != 0 {
		t.Error("lock not released")
	}
	if len(h.requests()) != 0 {
		t.Error("queued a request although no daemon runs")
	}
	actContains(t, h.errb.String(), "no daemon is running: releasing here")

	// A failed apply exits 1; a terminal asks first.
	h.cleaner.err = errors.New("slot busy")
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.stdin("n\n")
	if code := h.cmd("release", "5"); code != 1 || h.cleaner.applied != 1 {
		t.Fatalf("declined release: exit %d applied %d", code, h.cleaner.applied)
	}
	h.stdin("y\n")
	if code := h.cmd("release", "5"); code != 1 || h.cleaner.applied != 2 {
		t.Fatalf("confirmed failing release: exit %d applied %d", code, h.cleaner.applied)
	}
	actContains(t, h.errb.String(), "release talkable#5?", "nothing released", "slot busy")
}

// Naming a slot that holds a PR targets the slot, not the PR it holds now:
// the assignment can change before the daemon handles the request.
func TestTargetsKeepAnExplicitSlot(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	sl, err := h.st.CreateSlot(h.ctx, store.Slot{Name: "review3", RepoFullName: "talkable/talkable", Kind: store.SlotKindPool,
		Path: filepath.Join(h.home, "talkable.review3"), MainClone: h.home, State: store.SlotFree})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.TransitionSlot(h.ctx, sl.ID, nil, store.SlotHeld, func(u *store.SlotUpdate) { u.Set("pr_id", pr.ID) }); err != nil {
		t.Fatal(err)
	}
	if code := h.cmd("pin", "review3"); code != 0 {
		t.Fatalf("pin exit %d: %s", code, h.errb.String())
	}
	if code := h.cmd("mute", "review3"); code != 0 {
		t.Fatalf("mute exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if p := actDecode[engine.TargetPayload](t, reqs[0].Payload); reqs[0].Kind != engine.ReqPin || p.Slot != "review3" || p.Number != 0 {
		t.Errorf("pin %s %+v", reqs[0].Kind, p)
	}
	// mute is a PR property: the slot's PR is the target.
	if p := actDecode[engine.TargetPayload](t, reqs[1].Payload); reqs[1].Kind != engine.ReqMute || p.Number != 5 || p.Slot != "" {
		t.Errorf("mute %s %+v", reqs[1].Kind, p)
	}
	h.cleaner.plan = cleanup.Plan{Actions: []cleanup.Action{{Kind: "release", Slot: "review3"}}}
	if code := h.cmd("release", "review3", "--yes"); code != 0 {
		t.Fatalf("release exit %d: %s", code, h.errb.String())
	}
	if len(h.cleaner.opts) != 1 || h.cleaner.opts[0].Slot != "review3" || h.cleaner.opts[0].PR != nil {
		t.Fatalf("release opts %+v", h.cleaner.opts)
	}
}
