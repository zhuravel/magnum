package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

func (h *harness) enqueue(kind string, payload any) int64 {
	h.t.Helper()
	id, err := h.st.EnqueueRequest(h.ctx, kind, payload)
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) request(id int64) store.Request {
	h.t.Helper()
	r, err := h.st.RequestByID(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

// parkedPR reviews PR #2 and then releases its slot (sessions parked), the
// state `magnum open` restores from.
func (h *harness) parkedPR() store.PR {
	h.t.Helper()
	h.reviewedPR(2, "b1")
	rel := h.enqueue(ReqRelease, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	if r := h.request(rel); r.State != store.RequestDone {
		h.t.Fatalf("release: %+v %q", r, deref(r.Result))
	}
	if sl := h.slot("review1"); sl.State != store.SlotFree || sl.PRID != nil {
		h.t.Fatalf("slot not released: %+v", sl)
	}
	pr := h.wantState(2, store.PRReviewed)
	if _, err := h.st.LiveSessionByPRRole(h.ctx, pr.ID, store.RoleJudge); !errors.Is(err, store.ErrNotFound) {
		h.t.Fatalf("judge still live after release: %v", err)
	}
	return pr
}

func TestOpenRequestRestoresAParkedPR(t *testing.T) {
	h := newHarness(t)
	pr := h.parkedPR()
	h.ag.resumeIDs = map[agents.Role]string{agents.RoleJudge: "uuid-judge"}
	earlier := len(h.ag.all())
	id := h.enqueue(ReqOpen, OpenPayload{PRTarget: PRTarget{Repo: "talkable/talkable", Number: 2}})
	h.tick()

	r := h.request(id)
	if r.State != store.RequestDone || !strings.Contains(deref(r.Result), "restored talkable/talkable#2 in review1") ||
		!strings.Contains(deref(r.Result), "codex-judge pane p-judge") {
		t.Fatalf("open request: %+v %q", r, deref(r.Result))
	}
	cur := h.wantState(2, store.PRReviewed) // the PR's state is untouched
	if !cur.Pinned {
		t.Fatal("PR not pinned")
	}
	sl := h.slot("review1")
	if deref(sl.PRID) != pr.ID || sl.State != store.SlotHeld || !sl.Pinned || deref(sl.CheckedOutSHA) != "b1" {
		t.Fatalf("slot = %+v", sl)
	}
	for _, want := range []string{"reserve:" + itoa(pr.ID), "checkout:review1:" + itoa(pr.ID) + ":b1", "pin:review1"} {
		if !slices.Contains(h.sl.all(), want) {
			t.Fatalf("slot calls %v lack %s", h.sl.all(), want)
		}
	}
	opened := h.ag.all()[earlier:]
	if !slices.Contains(opened, "start:"+itoa(pr.ID)+":codex-judge:uuid-judge") {
		t.Fatalf("judge not resumed: %v", opened)
	}
	if slices.ContainsFunc(opened, func(c string) bool { return strings.HasPrefix(c, "start:"+itoa(pr.ID)+":claude-review") }) {
		t.Fatalf("a fresh Claude was started although none was asked for: %v", opened)
	}
	if _, err := h.st.LiveSessionByPRRole(h.ctx, pr.ID, store.RoleJudge); err != nil {
		t.Fatalf("judge session: %v", err)
	}
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("open started a review round (%d rounds)", n)
	}

	// Asking again finds the live session: nothing to do.
	again := h.enqueue(ReqOpen, OpenPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	if r := h.request(again); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "is live") {
		t.Fatalf("second open: %+v %q", r, deref(r.Result))
	}
}

func TestOpenRequestStartsTheAskedRole(t *testing.T) {
	h := newHarness(t)
	pr := h.parkedPR()
	id := h.enqueue(ReqOpen, OpenPayload{PRTarget: PRTarget{Ref: "2"}, Role: store.RoleClaude})
	h.tick()
	if r := h.request(id); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "claude-review pane p-claude") {
		t.Fatalf("open claude: %+v %q", r, deref(r.Result))
	}
	for _, want := range []string{"start:" + itoa(pr.ID) + ":codex-judge:", "start:" + itoa(pr.ID) + ":claude-review:", "preflight:claude"} {
		if !slices.Contains(h.ag.all(), want) {
			t.Fatalf("agent calls %v lack %s", h.ag.all(), want)
		}
	}
	bad := h.enqueue(ReqOpen, OpenPayload{PRTarget: PRTarget{Ref: "2"}, Role: "bogus"})
	h.tick()
	if r := h.request(bad); r.State != store.RequestFailed {
		t.Fatalf("unknown role: %+v", r)
	}
}

func TestOpenRequestWithoutAFreeSlotAsksForOne(t *testing.T) {
	h := newHarness(t)
	h.parkedPR()
	// Another PR takes the only slot.
	h.advance(31 * time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"}, prSpec{n: 3, head: "c1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	h.wantState(3, store.PRReviewed)
	h.advance(31 * time.Minute) // #3's slot is past min_warm: evictable
	id := h.enqueue(ReqOpen, OpenPayload{PRTarget: PRTarget{Ref: "2"}})
	h.tick()
	r := h.request(id)
	if r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "no free talkable/talkable slot") {
		t.Fatalf("open without a slot: %+v %q", r, deref(r.Result))
	}
	if !slices.Contains(h.sl.all(), "release:review1:evicted") {
		t.Fatalf("no slot was freed for the open: %v", h.sl.all())
	}
}

func TestOpenRequestPerPRRepoAndDryRun(t *testing.T) {
	h := newHarness(t)
	h.gh.set("zhuravel/app", prSpec{n: 1, head: "x1", updated: h.clock.Now()})
	h.startup()
	h.tick() // baseline, never reviewed: no slot, no sessions
	id := h.enqueue(ReqOpen, OpenPayload{PRTarget: PRTarget{Repo: "zhuravel/app", Number: 1}})
	h.tick()
	r := h.request(id)
	if r.State != store.RequestDone || !strings.Contains(deref(r.Result), "restored zhuravel/app#1 in zhuravel/app#1") {
		t.Fatalf("per-PR open: %+v %q", r, deref(r.Result))
	}
	if !slices.Contains(h.sl.all(), "worktree:zhuravel/app#1") {
		t.Fatalf("slot calls: %v", h.sl.all())
	}
	repo, _ := h.st.RepoByFullName(h.ctx, "zhuravel/app")
	pr, _ := h.st.PRByRepoNumber(h.ctx, repo.ID, 1)
	if pr.State != store.PRBaseline || !pr.Pinned {
		t.Fatalf("PR after open: %s pinned=%v", pr.State, pr.Pinned)
	}

	d := newHarness(t, func(h *harness) { h.d.DryRun = true })
	d.gh.set("zhuravel/app", prSpec{n: 1, head: "x1", updated: d.clock.Now()})
	d.startup()
	d.tick()
	did := d.enqueue(ReqOpen, OpenPayload{PRTarget: PRTarget{Ref: "zhuravel/app#1"}})
	d.tick()
	if r := d.request(did); r.State != store.RequestDone || !strings.HasPrefix(deref(r.Result), "dry run") {
		t.Fatalf("dry-run open: %+v %q", r, deref(r.Result))
	}
	if len(d.sl.all()) != 0 {
		t.Fatalf("dry-run open touched slots: %v", d.sl.all())
	}
}

func TestProvisionRepairAdoptRequestsRunOnTheHeavyWorker(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.cfg.Pools[0].Max = 3 })
	pool := h.cfg.Pools[0]
	prov := h.enqueue(ReqProvision, ProvisionPayload{Count: 1})
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	if r := h.request(prov); r.State != store.RequestPending {
		t.Fatalf("provision completed on the tick goroutine: %+v", r)
	}
	h.settle()
	if r := h.request(prov); r.State != store.RequestDone || deref(r.Result) != "provisioned review2 (free)" {
		t.Fatalf("provision: %+v %q", r, deref(r.Result))
	}
	tooMany := h.enqueue(ReqProvision, ProvisionPayload{Pool: "talkable/talkable", Count: 5})
	unknown := h.enqueue(ReqProvision, ProvisionPayload{Pool: "nobody/nothing"})
	h.tick()
	if r := h.request(tooMany); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "at most 3") {
		t.Fatalf("over max: %+v %q", r, deref(r.Result))
	}
	if r := h.request(unknown); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "no [[pool]]") {
		t.Fatalf("unknown pool: %+v %q", r, deref(r.Result))
	}

	broken := h.slot("review2")
	if err := h.st.TransitionSlot(h.ctx, broken.ID, []string{store.SlotFree}, store.SlotBroken, nil); err != nil {
		t.Fatal(err)
	}
	rep := h.enqueue(ReqRepair, RepairPayload{Slot: "review2"})
	missing := h.enqueue(ReqRepair, RepairPayload{Slot: "review9"})
	adopt := h.enqueue(ReqAdopt, AdoptPayload{Path: pool.Path(3)})
	notSlot := h.enqueue(ReqAdopt, AdoptPayload{Path: "/somewhere/else"})
	h.tick()
	if r := h.request(rep); r.State != store.RequestDone || h.slot("review2").State != store.SlotFree {
		t.Fatalf("repair: %+v %q", r, deref(r.Result))
	}
	if r := h.request(missing); r.State != store.RequestFailed {
		t.Fatalf("repair of an unknown slot: %+v", r)
	}
	if r := h.request(adopt); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "as review3") {
		t.Fatalf("adopt: %+v %q", r, deref(r.Result))
	}
	if r := h.request(notSlot); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "not a pool slot path") {
		t.Fatalf("adopt elsewhere: %+v %q", r, deref(r.Result))
	}
	for _, want := range []string{"provision:2", "repair:review2", "adopt:" + pool.Path(3)} {
		if !slices.Contains(h.sl.all(), want) {
			t.Fatalf("slot calls %v lack %s", h.sl.all(), want)
		}
	}
}

func TestReviewDryRunRequestPostsNothing(t *testing.T) {
	h := newHarness(t)
	before := h.reviewedPR(2, "b1")
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		if !in.DryRun {
			return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, ReviewID: 999, Event: "APPROVED"}, nil
		}
		return pipeline.RoundResult{Outcome: pipeline.OutcomeDryRun, Event: "CHANGES_REQUESTED", Findings: map[string]int{"P1": 1}}, nil
	}
	id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}, DryRun: true})
	h.tick()
	if r := h.request(id); r.State != store.RequestDone || !strings.Contains(deref(r.Result), "dry run") {
		t.Fatalf("request: %+v %q", r, deref(r.Result))
	}
	ins := h.rd.all()
	if len(ins) != 2 || !ins[1].DryRun || ins[0].DryRun {
		t.Fatalf("rounds: %+v", ins)
	}
	pr := h.wantState(2, store.PRReviewed)
	if deref(pr.LastReviewID) != deref(before.LastReviewID) || deref(pr.LastReviewEvent) != deref(before.LastReviewEvent) || pr.Forced {
		t.Fatalf("a dry run must not record a review: %+v", pr)
	}
	if _, ok := h.e.getKV(h.ctx, kvPRDryRun(pr.ID)); ok {
		t.Fatal("dry-run marker kept")
	}
	evs, _ := h.st.EventsBySubject(h.ctx, "pr:talkable/talkable#2", 0)
	if !slices.ContainsFunc(evs, func(e store.Event) bool {
		return e.Kind == "round.dry_run" && strings.Contains(e.Message, "nothing was posted")
	}) {
		t.Fatal("no round.dry_run event")
	}
}

func TestReviewDryRunReturnsABaselinePRToBaseline(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "a1"})
	h.startup()
	h.tick()
	h.wantState(1, store.PRBaseline)
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomeDryRun, Event: "COMMENTED"}, nil
	}
	h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "1"}, DryRun: true})
	h.tick()
	pr := h.wantState(1, store.PRBaseline)
	if pr.ReviewedSHA != nil {
		t.Fatalf("reviewed_sha = %q", *pr.ReviewedSHA)
	}
}

func TestReconcileScansTheInventoryOnce(t *testing.T) {
	h := newHarness(t)
	h.startup() // queues a reconcile (default cleanup + one pool's shrink)
	if h.inv.scans != 1 {
		t.Fatalf("reconcile scanned the inventory %d times, want 1", h.inv.scans)
	}
}

func TestRevealOnAttention(t *testing.T) {
	var mu sync.Mutex
	var focused []string
	reveals := 0
	h := newHarness(t, func(h *harness) {
		h.cfg.Terminal.RevealOnAttention = true
		h.d.Focus = func(_ context.Context, target string) error {
			mu.Lock()
			defer mu.Unlock()
			focused = append(focused, target)
			return nil
		}
		h.d.Reveal = func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			reveals++
			return nil
		}
	})
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomeBlocked, Error: "judge needs a decision"}, nil
	}
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	pr := h.wantState(2, store.PRNeedsAttention)
	mu.Lock()
	got, n := slices.Clone(focused), reveals
	mu.Unlock()
	if len(got) != 1 || got[0] != "mg-t-2-codex-judge" || n != 1 {
		t.Fatalf("focused %v, reveals %d; want the judge once", got, n)
	}
	// The same attention again inside the window does not steal focus.
	h.e.revealAttention(h.ctx, pr.ID, nil, "attention:"+itoa(pr.ID)+":"+pipeline.OutcomeBlocked)
	mu.Lock()
	defer mu.Unlock()
	if len(focused) != 1 {
		t.Fatalf("revealed again inside the window: %v", focused)
	}
}

func TestRevealOnAttentionOffByDefault(t *testing.T) {
	called := false
	h := newHarness(t, func(h *harness) {
		h.d.Focus = func(context.Context, string) error { called = true; return nil }
	})
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomeNeedsAttention, Error: "x"}, nil
	}
	h.reviewedPRWithoutCheck(2, "b1")
	h.wantState(2, store.PRNeedsAttention)
	if called {
		t.Fatal("focused although reveal_on_attention is off")
	}
}

// reviewedPRWithoutCheck runs the standard flow without asserting the outcome.
func (h *harness) reviewedPRWithoutCheck(n int, head string) {
	h.t.Helper()
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: n, head: head})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
}

func TestPreviousReviewOnlyFromTheCurrentLogin(t *testing.T) {
	h := newHarness(t)
	// #2 is reviewed by the App (talkable[bot]) with CHANGES_REQUESTED.
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, ReviewID: 500, Event: "CHANGES_REQUESTED", ReviewCommit: in.TargetSHA}, nil
	}
	pr := h.reviewedPR(2, "b1")
	if _, err := h.st.CreateRun(h.ctx, store.Run{ID: "run-app", PRID: pr.ID, Round: 1, Role: store.RoleJudge, Kind: "initial",
		TargetSHA: "b1", Identity: "talkable-app", ReviewerLogin: "talkable[bot]", State: store.RunVerified,
		ReviewID: store.Ptr(int64(500)), ReviewEvent: store.Ptr("CHANGES_REQUESTED")}); err != nil {
		t.Fatal(err)
	}
	// Same login: the previous review is the App's own.
	prev := h.e.previousReview(h.ctx, h.pr(2), "talkable[bot]", nil)
	if prev.ID != 500 || prev.Event != "CHANGES_REQUESTED" || prev.SHA != "b1" {
		t.Fatalf("own previous = %+v", prev)
	}
	// `magnum review --as zhuravel`: the App's review is not zhuravel's to dismiss.
	prev = h.e.previousReview(h.ctx, h.pr(2), "zhuravel", nil)
	if prev.ID != 0 || prev.Event != "" || prev.SHA != "b1" {
		t.Fatalf("other login's previous = %+v", prev)
	}
	// An older review by zhuravel stands in.
	if _, err := h.st.CreateRun(h.ctx, store.Run{ID: "run-z", PRID: pr.ID, Round: 0, Role: store.RoleJudge, Kind: "initial",
		TargetSHA: "a0", Identity: "zhuravel", ReviewerLogin: "zhuravel", State: store.RunVerified,
		ReviewID: store.Ptr(int64(400)), ReviewEvent: store.Ptr("CHANGES_REQUESTED")}); err != nil {
		t.Fatal(err)
	}
	prev = h.e.previousReview(h.ctx, h.pr(2), "zhuravel", nil)
	if prev.ID != 400 || prev.Event != "CHANGES_REQUESTED" || prev.SHA != "b1" {
		t.Fatalf("own older previous = %+v", prev)
	}

	// The round after `review --as zhuravel` gets that Previous.
	h.rd.script = nil
	h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}, As: "zhuravel"})
	h.tick()
	ins := h.rd.all()
	last := ins[len(ins)-1]
	if last.Previous == nil || last.Previous.ID != 400 {
		t.Fatalf("round after --as: Previous = %+v", last.Previous)
	}
}

func TestPollPersistsTheDiscoveredClone(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.d.Git = fakeGit{clones: map[string]string{"zhuravel/widgets": "/p/zhuravel-widgets"}}
	})
	// A stale clone path from an earlier version (a folder, not a clone).
	if _, err := h.st.UpsertRepo(h.ctx, store.Repo{NodeID: "R_zhuravel/app", Owner: "zhuravel", Name: "app",
		WatchOwner: "zhuravel", Mode: store.RepoModePerPR, ClonePath: store.Ptr(t.TempDir())}); err != nil {
		t.Fatal(err)
	}
	h.open(prSpec{n: 1, head: "a1"})
	h.gh.set("zhuravel/widgets", prSpec{n: 1, head: "t1", updated: h.clock.Now()})
	h.gh.set("zhuravel/app", prSpec{n: 1, head: "x1", updated: h.clock.Now()})
	h.startup()
	h.tick()
	tb, err := h.st.RepoByFullName(h.ctx, "zhuravel/widgets")
	if err != nil || deref(tb.ClonePath) != "/p/zhuravel-widgets" {
		t.Fatalf("widgets clone path = %v, %v", tb.ClonePath, err)
	}
	app, _ := h.st.RepoByFullName(h.ctx, "zhuravel/app")
	if app.ClonePath != nil {
		t.Fatalf("stale clone path kept: %q", *app.ClonePath)
	}
	tt, _ := h.st.RepoByFullName(h.ctx, "talkable/talkable")
	if deref(tt.ClonePath) != h.cfg.Pools[0].MainClone {
		t.Fatalf("pool repo clone path = %v", tt.ClonePath)
	}
}

// A forced review re-checks an identity marked unhealthy (for example by a
// judge whose own check hit a transient network error) instead of leaving
// the PR gated until the periodic re-check.
func TestForcedReviewRechecksAnUnhealthyIdentity(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	h.e.setKV(h.ctx, KVIdentityCheck(pr.Identity), "fail")
	h.e.setKV(h.ctx, KVIdentityError(pr.Identity), "the judge's identity check failed: connection reset")
	src := h.ids[pr.Identity]
	src.mu.Lock()
	checksBefore := src.checks
	src.mu.Unlock()
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, ReviewID: 1000, Event: "APPROVED"}, nil
	}
	id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}, Again: true})
	h.tick()
	if r := h.request(id); r.State != store.RequestDone {
		t.Fatalf("request: %+v %q", r, deref(r.Result))
	}
	src.mu.Lock()
	checks := src.checks
	src.mu.Unlock()
	if checks != checksBefore+1 {
		t.Fatalf("identity checks = %d, want %d (one re-check on the forced review)", checks, checksBefore+1)
	}
	if v, _ := h.e.getKV(h.ctx, KVIdentityCheck(pr.Identity)); v != "pass" {
		t.Fatalf("identity check kv = %q, want pass", v)
	}
	if ok, why := h.e.identityHealthy(h.ctx, pr.Identity); !ok {
		t.Fatalf("identity still gated: %s", why)
	}
}

// A PR whose identity changed after its sessions were created (watch config
// or magnum review --as) gets fresh sessions: the old panes carry the old
// identity's gh config and would post as the wrong login.
func TestRoundParksSessionsWhenTheIdentityChanged(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")
	h.e.setKV(h.ctx, kvPRSessionsIdentity(pr.ID), "zhuravel")
	parksBefore := h.ag.count(fmt.Sprintf("park:%d", pr.ID))
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, ReviewID: 1001, Event: "APPROVED"}, nil
	}
	id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}, Again: true, As: "talkable-app"})
	h.tick()
	if r := h.request(id); r.State != store.RequestDone {
		t.Fatalf("request: %+v %q", r, deref(r.Result))
	}
	if n := h.ag.count(fmt.Sprintf("park:%d", pr.ID)); n != parksBefore+1 {
		t.Fatalf("parks = %d, want %d (fresh sessions for the new identity)", n, parksBefore+1)
	}
	if v, _ := h.e.getKV(h.ctx, kvPRSessionsIdentity(pr.ID)); v != "talkable-app" {
		t.Fatalf("sessions identity kv = %q, want talkable-app", v)
	}
	evs, _ := h.st.EventsBySubject(h.ctx, "pr:talkable/talkable#2", 0)
	if !slices.ContainsFunc(evs, func(e store.Event) bool { return e.Kind == "pr.identity_changed" }) {
		t.Fatal("no pr.identity_changed event")
	}
}
