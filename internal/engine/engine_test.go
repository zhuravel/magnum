package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

func TestFirstSyncIsBaseline(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "a1"}, prSpec{n: 2, head: "b1"})
	h.startup()
	h.tick()

	repo, err := h.st.RepoByFullName(h.ctx, "talkable/talkable")
	if err != nil {
		t.Fatal(err)
	}
	if repo.FirstSyncedAt == nil || repo.Mode != store.RepoModePool || deref(repo.ClonePath) == "" {
		t.Fatalf("repo after first sync: %+v", repo)
	}
	for _, n := range []int{1, 2} {
		pr := h.wantState(n, store.PRBaseline)
		if pr.Identity != "talkable-app" || deref(pr.Title) == "" || deref(pr.AuthorLogin) != "alice" {
			t.Fatalf("PR #%d: %+v", n, pr)
		}
	}
	// Still baseline on the next poll; nothing dispatched.
	h.advance(10 * time.Minute)
	h.tick()
	h.wantState(1, store.PRBaseline)
	if n := len(h.rd.all()); n != 0 {
		t.Fatalf("rounds started for baseline PRs: %d", n)
	}
	if h.ag.count("observe") == 0 {
		t.Fatal("ticks must observe herdr")
	}
}

func TestNewPRQueuedClaimedReviewed(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "a1"})
	h.startup()
	h.tick()

	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "a1"}, prSpec{n: 2, head: "b1", request: true},
		prSpec{n: 3, head: "c1", author: "dependabot"})
	h.tick()
	queued := h.wantState(2, store.PRQueued)
	if !queued.ReviewRequested {
		t.Fatal("review_requested not recorded")
	}
	wantEligible := h.clock.Now().Add(5 * time.Minute)
	if queued.NextEligibleAt == nil || !queued.NextEligibleAt.Equal(wantEligible) || queued.PendingSince == nil {
		t.Fatalf("queued timers: next_eligible_at=%v pending_since=%v want %v", queued.NextEligibleAt, queued.PendingSince, wantEligible)
	}
	if skipped := h.wantState(3, store.PRIneligible); !strings.Contains(deref(skipped.SkipReason), "skip_authors") {
		t.Fatalf("skip reason: %q", deref(skipped.SkipReason))
	}

	// Inside the quiet period: nothing happens.
	h.advance(4 * time.Minute)
	h.tick()
	h.wantState(2, store.PRQueued)
	if len(h.rd.all()) != 0 {
		t.Fatal("dispatched inside the push quiet period")
	}

	h.advance(time.Minute)
	h.tick()
	pr := h.wantState(2, store.PRReviewed)
	ins := h.rd.all()
	if len(ins) != 1 {
		t.Fatalf("rounds: %d", len(ins))
	}
	in := ins[0]
	if in.Kind != pipeline.KindInitial || in.TargetSHA != "b1" || in.SlotPath != h.slot("review1").Path ||
		len(in.Roles) != 4 || len(in.Requested) != 0 || in.BaseRef != "master" || in.BaseSHA != "base0000" || in.Previous != nil {
		t.Fatalf("round input: %+v", in)
	}
	if deref(pr.ReviewedSHA) != "b1" || deref(pr.LastReviewID) != 101 || deref(pr.LastReviewEvent) != "COMMENTED" ||
		pr.RoundsToday != 1 || pr.LastRoundStartedAt == nil || !pr.SimplifyDone || pr.Forced || pr.Attempts != 0 {
		t.Fatalf("reviewed PR: %+v", pr)
	}
	sl := h.slot("review1")
	if sl.State != store.SlotHeld || deref(sl.PRID) != pr.ID || deref(sl.CheckedOutSHA) != "b1" {
		t.Fatalf("slot after round: %+v", sl)
	}
	calls := strings.Join(h.ag.all(), "\n")
	// claude-simplify (runs first, [claude] simplify = "first") runs on the
	// first round, so the workspace lays out all four roles.
	for _, want := range []string{"preflight:codex", "preflight:claude",
		"ensure_workspace:" + itoa(pr.ID) + ":" + sl.Path + ":talkable#2:review1:codex-judge,claude-review,codex-review,claude-simplify",
		"start:" + itoa(pr.ID) + ":codex-judge:", "start:" + itoa(pr.ID) + ":claude-review:", "start:" + itoa(pr.ID) + ":claude-simplify:"} {
		if !strings.Contains(calls, want) {
			t.Fatalf("agents calls lack %q:\n%s", want, calls)
		}
	}
	if !slices.Contains(h.sl.all(), "claim:"+itoa(pr.ID)) {
		t.Fatalf("slot calls: %v", h.sl.all())
	}

	// The posted review is toasted (toast_every_review) once the batch is due.
	h.advance(2 * time.Minute)
	h.tick()
	if got := strings.Join(h.nh.all(), "\n"); !strings.Contains(got, "talkable#2: COMMENTED (1 P2)") {
		t.Fatalf("toasts: %s", got)
	}
	if _, _, line, ok := readTabBarFile(h.layout.TabBar()); !ok || !strings.HasPrefix(line, "magnum") {
		b, err := os.ReadFile(h.layout.TabBar())
		t.Fatalf("tab bar: %q %v", b, err)
	}
}

func TestPushRereviewHonorsQuietPeriodAndInterval(t *testing.T) {
	h := newHarness(t)
	reviewed := h.reviewedPR(2, "b1")
	roundStart := *reviewed.LastRoundStartedAt

	h.advance(time.Minute)
	pushAt := h.clock.Now()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	pr := h.wantState(2, store.PRRereviewPending)
	if pr.PendingSince == nil || !pr.PendingSince.Equal(pushAt) {
		t.Fatalf("pending_since = %v, want %v", pr.PendingSince, pushAt)
	}
	// Quiet period ends at push+5m, but the 30m interval since the last round wins.
	if want := roundStart.Add(30 * time.Minute); pr.NextEligibleAt == nil || !pr.NextEligibleAt.Equal(want) {
		t.Fatalf("next_eligible_at = %v, want %v", pr.NextEligibleAt, want)
	}

	h.advance(5 * time.Minute) // quiet period over, interval not
	h.tick()
	h.wantState(2, store.PRRereviewPending)
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("re-review before the min interval: %d rounds", n)
	}

	h.advance(roundStart.Add(30 * time.Minute).Sub(h.clock.Now()))
	claims := len(h.sl.all())
	h.tick()
	pr = h.wantState(2, store.PRReviewed)
	ins := h.rd.all()
	if len(ins) != 2 {
		t.Fatalf("rounds: %d", len(ins))
	}
	in := ins[1]
	if in.Kind != pipeline.KindRereview || in.TargetSHA != "b2" || h.lastWorkspaceHas(pr.ID, store.RoleSimplify) || in.Previous == nil ||
		in.Previous.ID != 101 || in.Previous.SHA != "b1" || in.Since.IsZero() || in.ForcePushed {
		t.Fatalf("re-review input: %+v (previous %+v)", in, in.Previous)
	}
	if deref(pr.ReviewedSHA) != "b2" || pr.RoundsToday != 2 {
		t.Fatalf("after re-review: %+v", pr)
	}
	for _, c := range h.sl.all()[claims:] {
		if strings.HasPrefix(c, "claim:") {
			t.Fatalf("re-review claimed another slot: %v", h.sl.all())
		}
	}
}

func TestHeadMovedDuringRoundQueuesRereview(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.rd.gate = make(chan struct{})
	before := len(h.rd.all())
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.awaitRound(before)
	// While the round reviews b1, the poller sees a push.
	h.advance(time.Minute)
	pushAt := h.clock.Now()
	h.gh.compare["b1...b2"] = github.CompareStats{Commits: 1}
	h.gh.files = map[string][]github.FileDelta{"b1...b2": codePatch(40)} // above the re-review threshold
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.wantState(2, store.PRReviewing)
	close(h.rd.gate)
	h.settle()
	ins := h.rd.all()
	if len(ins) != 1 || ins[0].TargetSHA != "b1" {
		t.Fatalf("round target: %+v", ins)
	}
	// b1 is on record, b2 waits for its re-review: the push arrived during
	// the review, so only the quiet period from the push holds it, not the
	// re-review interval from the round's start.
	pr := h.wantState(2, store.PRRereviewPending)
	want := pushAt.Add(5 * time.Minute)
	if want.After(pr.LastRoundStartedAt.Add(30 * time.Minute)) {
		t.Fatal("the test needs the interval to end after the quiet period")
	}
	if got := h.rd.appended(); len(got) != 1 || got[0] != "101:_Reviewed b1; 1 commit arrived during the review, re-review follows after the quiet period._" {
		t.Errorf("review notes = %q", got)
	}
	if deref(pr.ReviewedSHA) != "b1" || pr.PendingSince == nil || !pr.PendingSince.Equal(pushAt) ||
		pr.NextEligibleAt == nil || !pr.NextEligibleAt.Equal(want) {
		t.Fatalf("after the round: reviewed_sha %q pending_since %v next_eligible_at %v (want %v)",
			deref(pr.ReviewedSHA), pr.PendingSince, pr.NextEligibleAt, want)
	}
	h.advance(want.Sub(h.clock.Now()))
	h.tick()
	if ins := h.rd.all(); len(ins) != 2 || ins[1].TargetSHA != "b2" {
		t.Fatalf("re-review: %+v", ins)
	}
	h.wantState(2, store.PRReviewed)
}

func TestClosedConfirmedTwiceThenReleasedAfterGrace(t *testing.T) {
	h := newHarness(t)
	pr := h.reviewedPR(2, "b1")

	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}) // #2 left the OPEN list
	h.gh.closed[2] = "MERGED"
	h.tick()
	p := h.wantState(2, store.PRReviewed)
	if p.MissingSince == nil || p.ConfirmCount != 1 {
		t.Fatalf("first confirm: missing_since=%v count=%d", p.MissingSince, p.ConfirmCount)
	}
	h.advance(30 * time.Second)
	h.tick()
	h.wantState(2, store.PRReviewed) // second confirmation, but not 60s apart yet

	h.advance(30 * time.Second)
	closedAt := h.clock.Now()
	h.tick()
	p = h.wantState(2, store.PRClosed)
	if deref(p.PrevState) != store.PRReviewed || p.GHState != store.GHMerged || p.ReleaseAfter == nil ||
		!p.ReleaseAfter.Equal(closedAt.Add(10*time.Minute)) || p.MergedAt == nil {
		t.Fatalf("closed PR: %+v", p)
	}

	h.advance(5 * time.Minute)
	h.tick()
	h.wantState(2, store.PRClosed) // inside the grace
	if h.slot("review1").State != store.SlotHeld {
		t.Fatal("slot released inside the close grace")
	}

	h.advance(6 * time.Minute)
	h.tick()
	h.wantState(2, store.PRReleased)
	sl := h.slot("review1")
	if sl.State != store.SlotFree || sl.PRID != nil {
		t.Fatalf("slot after release: %+v", sl)
	}
	if !slices.Contains(h.ag.all(), "park:"+itoa(pr.ID)) {
		t.Fatalf("sessions not parked before release: %v", h.ag.all())
	}
	if !slices.Contains(h.sl.all(), "release:review1:merged") {
		t.Fatalf("slot calls: %v", h.sl.all())
	}
}

func TestReopenedPRReturnsToPreviousState(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.open(prSpec{n: 1, head: "base1"})
	h.gh.closed[2] = "CLOSED"
	for range 3 {
		h.advance(30 * time.Second)
		h.tick()
	}
	h.wantState(2, store.PRClosed)
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	p := h.wantState(2, store.PRReviewed)
	if p.PrevState != nil || p.ReleaseAfter != nil || p.MissingSince != nil || p.GHState != store.GHOpen {
		t.Fatalf("reopened PR: %+v", p)
	}
}

func TestNotFoundBecomesUnknown(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.open(prSpec{n: 1, head: "base1"})
	h.gh.notFound = []int{2}
	h.advance(time.Minute)
	h.tick()
	p := h.wantState(2, store.PRReviewed)
	if p.GHState != store.GHUnknown {
		t.Fatalf("gh_state = %s", p.GHState)
	}
	h.advance(time.Hour)
	h.tick()
	h.wantState(2, store.PRReviewed) // never cleaned up
}

func TestUsageLimitPauseBlocksDispatchThenContinues(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		// Two slots so a second PR could run.
		h.cfg.Pools[0].Max = 2
	})
	pool := h.cfg.Pools[0]
	if _, err := h.st.CreateSlot(h.ctx, store.Slot{Name: pool.Slot(2), RepoFullName: pool.Repo, Kind: store.SlotKindPool,
		Path: pool.Path(2), MainClone: pool.MainClone, State: store.SlotFree}); err != nil {
		t.Fatal(err)
	}
	resetAt := h.clock.Now().Add(2 * time.Hour)
	var judgeRun string
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		if in.Kind == pipeline.KindContinue || in.PR.Number != 2 {
			return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: max(in.Round, 1), ReviewID: 7, Event: "COMMENTED"}, nil
		}
		run, err := h.st.CreateRun(h.ctx, store.Run{PRID: in.PR.ID, Round: 1, Role: store.RoleJudge, Kind: in.Kind,
			TargetSHA: in.TargetSHA, Identity: in.PR.Identity, ReviewerLogin: "talkable[bot]", State: store.RunPending, PromptText: "judge"})
		if err != nil {
			return failRound(t, err)
		}
		judgeRun = run.ID
		now := h.clock.Now()
		if err := h.st.TransitionRun(h.ctx, run.ID, nil, store.RunFailed, func(u *store.RunUpdate) {
			u.Set("submitted_at", now)
			u.Set("outcome", pipeline.OutcomeUsageLimit)
			u.Set("error", "judge pane: usage limit") // as the pipeline finishes a paused run
		}); err != nil {
			return failRound(t, err)
		}
		return pipeline.RoundResult{Outcome: pipeline.OutcomeUsageLimit, Round: 1, JudgeRunID: run.ID,
			Pause: &pipeline.Pause{Kind: string(agents.HealthUsageLimit), Tool: agents.KindCodex, Until: resetAt}}, nil
	}

	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	h.wantState(2, store.PRPaused)
	if until, ok := h.e.kvTime(h.ctx, store.KVToolPausedUntil("codex")); !ok || !until.Equal(resetAt) {
		t.Fatalf("codex paused until %v (%v)", until, ok)
	}
	if !strings.Contains(strings.Join(h.nh.all(), "\n"), "Codex usage limit") {
		t.Fatalf("no usage-limit toast: %v", h.nh.all())
	}

	// A new eligible PR waits while Codex is paused.
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"}, prSpec{n: 3, head: "c1"})
	h.tick()
	h.advance(10 * time.Minute)
	h.tick()
	h.wantState(3, store.PRQueued)
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("dispatched during a usage-limit pause: %d rounds", n)
	}

	// The reset passes: the paused PR continues its judge turn, #3 starts.
	h.advance(2 * time.Hour)
	h.tick()
	ins := h.rd.all()
	if len(ins) != 3 {
		t.Fatalf("rounds after the pause: %d", len(ins))
	}
	var cont *pipeline.RoundInput
	for i := range ins[1:] {
		if ins[1+i].PR.Number == 2 {
			cont = &ins[1+i]
		}
	}
	if cont == nil || cont.Kind != pipeline.KindContinue || cont.Round != 1 || cont.ContinueRunID != judgeRun || cont.TargetSHA != "b1" {
		t.Fatalf("continue round: %+v", cont)
	}
	h.wantState(2, store.PRReviewed)
	if _, ok := h.e.toolPause(h.ctx, "codex"); ok {
		t.Fatal("codex pause not cleared")
	}
	if p := h.pr(2); p.RoundsToday != 1 {
		t.Fatalf("a continue must not count as a new round: %d", p.RoundsToday)
	}
}

func TestLoginRequiredPreflightPausesAndRechecks(t *testing.T) {
	h := newHarness(t)
	h.ag.loggedOut = map[string]bool{"codex": true}
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	pr := h.wantState(2, store.PRQueued)
	if pr.Attempts != 0 {
		t.Fatalf("a logged-out CLI must not use the PR's retry budget: %d", pr.Attempts)
	}
	if p, ok := h.e.toolPause(h.ctx, "codex"); !ok || p.Reason != "login_required" {
		t.Fatalf("codex pause: %+v %v", p, ok)
	}
	if len(h.rd.all()) != 0 {
		t.Fatal("round ran while codex was logged out")
	}
	h.advance(2 * time.Minute)
	h.tick() // still logged out: pause extended
	if p, _ := h.e.toolPause(h.ctx, "codex"); !p.Until.After(h.clock.Now()) {
		t.Fatalf("pause not extended: %v", p.Until)
	}
	h.ag.mu.Lock()
	h.ag.loggedOut = nil
	h.ag.mu.Unlock()
	h.advance(2 * time.Minute)
	h.tick()
	h.wantState(2, store.PRReviewed)
}

// Only the daemon's log named what started a human cooldown, which holds
// every prompt to the PR. A tick whose observation begins one records one
// agent.human_active event on the PR naming the role and when prompts may
// go out again; ticks that only extend it record none, and activity after
// it ran out records another.
func TestANewHumanCooldownIsAnEvent(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	pr := h.pr(1)
	cd := h.cfg.Daemon.HumanCooldown.Duration
	observe := func(begins bool) {
		t.Helper()
		o := agents.Observation{Kind: agents.ObsHumanActive, Role: agents.Role(store.RoleClaude), PRID: pr.ID}
		if begins {
			o.CooldownUntil = h.clock.Now().Add(cd)
		}
		h.ag.mu.Lock()
		h.ag.obs = []agents.Observation{o}
		h.ag.mu.Unlock()
		h.tick()
	}
	events := func(want int) []store.Event {
		t.Helper()
		evs, err := h.st.EventsOfKindsSince(h.ctx, time.Time{}, agents.EventHumanActive)
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) != want {
			t.Fatalf("%s events: %+v, want %d", agents.EventHumanActive, evs, want)
		}
		return evs
	}
	check := func(ev store.Event, until time.Time) {
		t.Helper()
		msg := "claude-review works with no magnum prompt in flight, taken for someone typing: prompts to this PR wait until " +
			until.Local().Format("15:04:05") + " (human_cooldown 15m)"
		if ev.Message != msg || ev.Subject == nil || *ev.Subject != "pr:talkable/talkable#1" {
			t.Fatalf("event %q on %v, want %q on the PR", ev.Message, ev.Subject, msg)
		}
		var data map[string]string
		if err := json.Unmarshal(ev.Data, &data); err != nil || data["role"] != store.RoleClaude || data["until"] != store.FormatTime(until) {
			t.Fatalf("event data %s (%v), want the role and %s", ev.Data, err, store.FormatTime(until))
		}
	}

	observe(true)
	check(events(1)[0], h.clock.Now().Add(cd))
	for range 3 {
		h.advance(30 * time.Second)
		observe(false)
	}
	events(1)
	h.advance(cd)
	observe(true)
	evs := events(2)
	last := evs[0]
	if evs[1].ID > last.ID {
		last = evs[1]
	}
	check(last, h.clock.Now().Add(cd))
}

func TestForcedRequestBypassesThrottle(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.advance(time.Minute)
	id, err := h.st.EnqueueRequest(h.ctx, ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}, Simplify: true})
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	req, err := h.st.RequestByID(h.ctx, id)
	if err != nil || req.State != store.RequestDone || !strings.Contains(deref(req.Result), "queued talkable/talkable#2") {
		t.Fatalf("request: %+v %v", req, err)
	}
	ins := h.rd.all()
	if len(ins) != 2 {
		t.Fatalf("forced round did not run inside the min interval: %d rounds", len(ins))
	}
	if ins[1].Kind != pipeline.KindRereview || !slices.Equal(ins[1].Requested, []string{store.RoleSimplify}) ||
		!h.lastWorkspaceHas(ins[1].PR.ID, store.RoleSimplify) {
		t.Fatalf("forced round: %+v", ins[1])
	}
	pr := h.wantState(2, store.PRReviewed)
	if pr.Forced || pr.NextEligibleAt != nil {
		t.Fatalf("forced flag not cleared: %+v", pr)
	}
	if v, ok := h.e.getKV(h.ctx, store.KVPRRoles(pr.ID)); ok {
		t.Fatalf("simplify request kept: %q", v)
	}
}

func TestForcedRequestBypassesBaselineAndQuietHours(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		now := h.clock.Now()
		h.cfg.Daemon.QuietHours = now.Add(-time.Hour).Format("15:04") + "-" + now.Add(3*time.Hour).Format("15:04")
	})
	h.open(prSpec{n: 1, head: "a1"})
	h.startup()
	h.tick()
	h.wantState(1, store.PRBaseline)
	if _, err := h.st.EnqueueRequest(h.ctx, ReqReview, ReviewPayload{PRTarget: PRTarget{Repo: "talkable/talkable", Number: 1}}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	h.wantState(1, store.PRReviewed)
}

func TestRequestsPauseResumeMutePin(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	enqueue := func(kind string, payload any) int64 {
		id, err := h.st.EnqueueRequest(h.ctx, kind, payload)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	result := func(id int64) store.Request {
		r, err := h.st.RequestByID(h.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	p := enqueue(ReqPause, PausePayload{Reason: "lunch"})
	m := enqueue(ReqMute, TargetPayload{PRTarget: PRTarget{Ref: "talkable#2"}})
	pin := enqueue(ReqPin, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	bad := enqueue("bogus", nil)
	h.tick()
	for _, id := range []int64{p, m, pin} {
		if r := result(id); r.State != store.RequestDone {
			t.Fatalf("request %d: %+v %q", id, r, deref(r.Result))
		}
	}
	if r := result(bad); r.State != store.RequestFailed {
		t.Fatalf("unknown kind: %+v", r)
	}
	pr := h.pr(2)
	if !pr.Muted || !pr.Pinned || !h.slot("review1").Pinned {
		t.Fatalf("mute/pin: %+v slot pinned=%v", pr, h.slot("review1").Pinned)
	}
	if reason := h.e.pauseReason(h.ctx); !strings.Contains(reason, "daemon paused") {
		t.Fatalf("pause reason %q", reason)
	}
	r := enqueue(ReqResume, PausePayload{})
	u := enqueue(ReqUnpin, TargetPayload{Slot: "review1"})
	h.tick()
	if result(r).State != store.RequestDone || result(u).State != store.RequestDone {
		t.Fatal("resume/unpin failed")
	}
	if h.e.pauseReason(h.ctx) != "" || h.slot("review1").Pinned || h.pr(2).Pinned {
		t.Fatal("resume/unpin had no effect")
	}
}

func TestReleaseRequestRunsOnHeavyWorker(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	id, err := h.st.EnqueueRequest(h.ctx, ReqRelease, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	if r, _ := h.st.RequestByID(h.ctx, id); r.State != store.RequestPending {
		t.Fatalf("release completed on the tick goroutine: %+v", r)
	}
	h.settle()
	r, _ := h.st.RequestByID(h.ctx, id)
	if r.State != store.RequestDone {
		t.Fatalf("release request: %+v %q", r, deref(r.Result))
	}
	if sl := h.slot("review1"); sl.State != store.SlotFree {
		t.Fatalf("slot after release: %+v", sl)
	}
	h.wantState(2, store.PRReviewed) // parked: reviewed without a slot
}

func TestIdentityUnhealthyKeepsPRQueued(t *testing.T) {
	h := newHarness(t)
	h.app.checkErr = "pull_requests permission is read, want write"
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	pr := h.wantState(2, store.PRQueued)
	if !strings.Contains(deref(pr.LastError), "pull_requests permission") {
		t.Fatalf("visible reason missing: %q", deref(pr.LastError))
	}
	if len(h.rd.all()) != 0 {
		t.Fatal("dispatched with an unhealthy identity")
	}
	// The identity is fixed: the reconcile re-check clears it.
	h.app.mu.Lock()
	h.app.checkErr = ""
	h.app.mu.Unlock()
	h.advance(10 * time.Minute)
	h.tick()
	h.tick()
	h.wantState(2, store.PRReviewed)
	if h.app.ensures == 0 {
		t.Fatal("EnsureConfigDir must run every tick")
	}
}

func TestFailuresBackOffThenNeedAttention(t *testing.T) {
	h := newHarness(t)
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomeTimeout, Round: 1, Error: "judge_timeout"}, nil
	}
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	pr := h.wantState(2, store.PRQueued)
	if pr.Attempts != 1 || pr.NextAttemptAt == nil || !pr.NextAttemptAt.Equal(h.clock.Now().Add(time.Minute)) {
		t.Fatalf("first failure: attempts=%d next=%v", pr.Attempts, pr.NextAttemptAt)
	}
	h.tick() // backoff not over
	if len(h.rd.all()) != 1 {
		t.Fatal("retried inside the backoff")
	}
	h.advance(time.Minute)
	h.tick()
	h.wantState(2, store.PRQueued)
	h.advance(2 * time.Minute)
	h.tick()
	pr = h.wantState(2, store.PRNeedsAttention)
	if pr.Attempts != 3 || h.slot("review1").State != store.SlotHeld {
		t.Fatalf("attention: %+v", pr)
	}
	if !strings.Contains(strings.Join(h.nh.all(), "\n"), "needs attention") {
		t.Fatalf("no attention toast: %v", h.nh.all())
	}
	// A new push gives the PR a fresh budget.
	h.rd.script = nil
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	pr = h.wantState(2, store.PRQueued)
	if pr.Attempts != 0 {
		t.Fatalf("attempts not reset on push: %d", pr.Attempts)
	}
}

func TestIdentityLeakPausesWatch(t *testing.T) {
	h := newHarness(t)
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomeIdentityLeak, Round: 1, ReviewID: 5, Error: "zhuravel"}, nil
	}
	h.reviewedPRExpect(2, "b1", store.PRNeedsAttention)
	if v, ok := h.e.getKV(h.ctx, store.KVWatchPaused("talkable")); !ok || !strings.Contains(v, "identity leak") {
		t.Fatalf("watch not paused: %q", v)
	}
	if !strings.Contains(strings.Join(h.nh.all(), "\n"), "IDENTITY LEAK") {
		t.Fatalf("no urgent toast: %v", h.nh.all())
	}
	// Forced reviews of that watch wait as well.
	h.rd.script = nil
	if _, err := h.st.EnqueueRequest(h.ctx, ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if len(h.rd.all()) != 1 {
		t.Fatal("dispatched while the watch is paused")
	}
}

func (h *harness) reviewedPRExpect(n int, head, state string) {
	h.t.Helper()
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: n, head: head})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	h.wantState(n, state)
}

func TestNoFreeSlotEvictsIdleHeldSlot(t *testing.T) {
	h := newHarness(t)
	first := h.reviewedPR(2, "b1")
	h.advance(31 * time.Minute) // past min_warm
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"}, prSpec{n: 3, head: "c1"})
	h.tick() // #3 queued
	h.advance(5 * time.Minute)
	h.tick() // no free slot (max 1): evict review1 from #2
	if !slices.Contains(h.ag.all(), "park:"+itoa(first.ID)) || !slices.Contains(h.sl.all(), "release:review1:evicted") {
		t.Fatalf("eviction: agents %v slots %v", h.ag.all(), h.sl.all())
	}
	h.wantState(2, store.PRReviewed)
	h.tick() // the freed slot goes to #3
	h.wantState(3, store.PRReviewed)
	if sl := h.slot("review1"); deref(sl.PRID) != h.pr(3).ID {
		t.Fatalf("slot holder: %+v", sl)
	}
}

func TestPerPRWorktreeRepo(t *testing.T) {
	h := newHarness(t)
	h.gh.set("zhuravel/app", prSpec{n: 1, head: "x1", updated: h.clock.Now()})
	h.startup()
	h.tick()
	h.gh.set("zhuravel/app", prSpec{n: 1, head: "x1", updated: h.clock.Now()}, prSpec{n: 2, head: "y1", updated: h.clock.Now()})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	repo, _ := h.st.RepoByFullName(h.ctx, "zhuravel/app")
	if repo.Mode != store.RepoModePerPR {
		t.Fatalf("mode %s", repo.Mode)
	}
	pr, _ := h.st.PRByRepoNumber(h.ctx, repo.ID, 2)
	if pr.State != store.PRReviewed || pr.Identity != "zhuravel" {
		t.Fatalf("per-PR repo PR: %+v", pr)
	}
	if !slices.Contains(h.sl.all(), "worktree:zhuravel/app#2") {
		t.Fatalf("slot calls: %v", h.sl.all())
	}
}

func TestCrashRecovery(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"}, prSpec{n: 3, head: "c1"})
	h.startup()
	h.tick()
	// Simulate a crash: #2 mid-claim, #3 mid-round with its judge prompted.
	p2, p3 := h.pr(2), h.pr(3)
	if err := h.st.TransitionPR(h.ctx, p2.ID, nil, store.PRClaiming, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.st.TransitionPR(h.ctx, p3.ID, nil, store.PRReviewing, func(u *store.PRUpdate) { u.Set("reviewed_sha", "c0") }); err != nil {
		t.Fatal(err)
	}
	sl := h.slot("review1")
	if err := h.st.TransitionSlot(h.ctx, sl.ID, nil, store.SlotBusy, func(u *store.SlotUpdate) { u.Set("pr_id", p3.ID) }); err != nil {
		t.Fatal(err)
	}
	now := h.clock.Now()
	run, err := h.st.CreateRun(h.ctx, store.Run{PRID: p3.ID, Round: 1, Role: store.RoleJudge, Kind: store.RunInitial,
		TargetSHA: "c1", Identity: "talkable-app", ReviewerLogin: "talkable[bot]", State: store.RunPending, PromptText: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.TransitionRun(h.ctx, run.ID, nil, store.RunWorking, func(u *store.RunUpdate) { u.Set("submitted_at", now) }); err != nil {
		t.Fatal(err)
	}

	for _, role := range []string{store.RoleJudge, store.RoleClaude} {
		name := "mg-t-3-" + role
		if _, err := h.st.CreateSession(h.ctx, store.Session{PRID: p3.ID, Role: role, AgentName: &name,
			SessionID: store.Ptr("sess-" + role), State: store.SessionLive}); err != nil {
			t.Fatal(err)
		}
	}

	e2 := New(h.d) // a new daemon process
	e2.startup(h.ctx)
	h.wantState(2, store.PRQueued)
	p := h.wantState(3, store.PRPaused)
	if !strings.Contains(deref(p.LastError), "restarted") {
		t.Fatalf("last_error %q", deref(p.LastError))
	}
	if h.slot("review1").State != store.SlotHeld {
		t.Fatal("busy slot not held after recovery")
	}
	if !slices.Contains(h.ag.all(), "recover:"+itoa(p3.ID)) {
		t.Fatalf("sessions not recovered: %v", h.ag.all())
	}

	// The paused PR continues its judge's turn; the old run is closed out.
	h.e = e2
	h.tick()
	var cont *pipeline.RoundInput
	for _, in := range h.rd.all() {
		if in.PR.Number == 3 {
			in := in
			cont = &in
		}
	}
	if cont == nil || cont.Kind != pipeline.KindContinue || cont.ContinueRunID != run.ID || cont.Round != 1 {
		t.Fatalf("continue after restart: %+v", cont)
	}
	if r, _ := h.st.RunByID(h.ctx, run.ID); r.State != store.RunFailed {
		t.Fatalf("stale run state %s", r.State)
	}
}

func TestDryRunHasNoSideEffects(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.d.DryRun = true })
	h.open(prSpec{n: 1, head: "base1"})
	if err := h.e.Run(h.ctx, Options{Once: true, NoSignals: true}); err != nil {
		t.Fatal(err)
	}
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.wantState(2, store.PRQueued) // decisions are recorded (in the dry run's private store)
	h.advance(5 * time.Minute)
	h.tick()
	h.wantState(2, store.PRQueued)

	if calls := h.ag.all(); len(calls) != 0 {
		t.Fatalf("agents called in a dry run: %v", calls)
	}
	if n := len(h.rd.all()); n != 0 {
		t.Fatalf("rounds run in a dry run: %d", n)
	}
	if calls := h.sl.all(); len(calls) != 0 {
		t.Fatalf("slots called in a dry run: %v", calls)
	}
	if got := h.nh.all(); len(got) != 0 {
		t.Fatalf("herdr notifications in a dry run: %v", got)
	}
	for name, id := range h.ids {
		if id.checks != 0 {
			t.Fatalf("identity %s checked in a dry run", name)
		}
	}
	if h.app.ensures != 0 {
		t.Fatal("token minted in a dry run")
	}
	if h.slot("review1").State != store.SlotFree {
		t.Fatal("slot claimed in a dry run")
	}
	for _, p := range []string{h.layout.Pid(), h.layout.TabBar()} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s written in a dry run: %v", p, err)
		}
	}
	var planned []string
	for _, op := range h.e.Planned() {
		planned = append(planned, op.Action+" "+op.Subject+" "+op.Detail)
	}
	if !slices.ContainsFunc(planned, func(s string) bool {
		return strings.HasPrefix(s, "review pr:talkable/talkable#2 initial round in review1")
	}) {
		t.Fatalf("planned ops: %v", planned)
	}
}

func TestRunExitsWhenAlreadyRunning(t *testing.T) {
	h := newHarness(t)
	f, err := os.OpenFile(h.layout.Lock(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := h.e.Run(h.ctx, Options{Once: true, NoSignals: true}); err != nil {
		t.Fatalf("already running must exit nil: %v", err)
	}
	if h.gh.count("radar:") != 0 {
		t.Fatal("ticked while another daemon holds the lock")
	}
}

func TestRunOnceTicksAndCleansUp(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "a1"})
	if err := h.e.Run(h.ctx, Options{Once: true, NoSignals: true}); err != nil {
		t.Fatal(err)
	}
	if h.gh.count("radar:talkable") != 1 {
		t.Fatalf("radar calls: %v", h.gh.calls)
	}
	if _, err := os.Stat(h.layout.Pid()); !os.IsNotExist(err) {
		t.Fatalf("pidfile left behind: %v", err)
	}
	if h.inv.scans == 0 {
		t.Fatal("startup must reconcile the inventory")
	}
	h.wantState(1, store.PRBaseline)
}

func TestRunStopsOnCancelAndKick(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "a1"})
	ctx, cancel := context.WithCancel(h.ctx)
	done := h.goRun(ctx, Options{NoSignals: true})
	deadline := time.Now().Add(5 * time.Second)
	for h.gh.count("radar:talkable") < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	h.e.Kick()
	for h.gh.count("radar:talkable") < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if h.gh.count("radar:talkable") < 2 {
		t.Fatal("kick did not tick")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestHerdrDownBlocksDispatch(t *testing.T) {
	h := newHarness(t)
	h.hd.err = errors.New("herdr unavailable")
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	h.wantState(2, store.PRQueued)
	h.hd.mu.Lock()
	h.hd.err = nil
	h.hd.mu.Unlock()
	h.tick()
	h.wantState(2, store.PRReviewed)
}

func TestWorkingCodexLimitBlocksDispatch(t *testing.T) {
	h := newHarness(t)
	for i := range 5 {
		h.hd.agents = append(h.hd.agents, agentInfo(i))
	}
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	pr := h.wantState(2, store.PRQueued)
	if v, _ := h.e.getKV(h.ctx, store.KVPRGate(pr.ID)); !strings.HasPrefix(v, "working Codex agents at the limit") || len(h.rd.all()) != 0 {
		t.Fatalf("gate %q, rounds %d", v, len(h.rd.all()))
	}
}

func TestEventsUseThePipelineSubject(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	evs, err := h.st.EventsBySubject(h.ctx, "pr:talkable/talkable#2", 0)
	if err != nil || len(evs) == 0 {
		t.Fatalf("events: %v %v", evs, err)
	}
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	for _, want := range []string{"pr.queued", "engine.round_start", "engine.round_result"} {
		if !slices.Contains(kinds, want) {
			t.Fatalf("event kinds %v lack %s", kinds, want)
		}
	}
	b, _ := json.Marshal(h.e.Planned())
	if string(b) != "null" {
		t.Fatalf("planned ops outside a dry run: %s", b)
	}
}

func TestPinnedSlotWaitsWithoutUsingRetries(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	if _, err := h.st.EnqueueRequest(h.ctx, ReqPin, TargetPayload{Slot: "review1"}); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	h.advance(40 * time.Minute)
	h.tick()
	pr := h.wantState(2, store.PRRereviewPending)
	if pr.Attempts != 0 || deref(pr.LastError) != "" || len(h.rd.all()) != 1 {
		t.Fatalf("pinned slot: attempts=%d last_error=%q rounds=%d", pr.Attempts, deref(pr.LastError), len(h.rd.all()))
	}
	if w := waitOf(t, h, 2); w.Reason != WaitPinned || w.Subject != "magnum slots pin" {
		t.Fatalf("pinned slot: wait %+v, want the pin", w)
	}
	if _, err := h.st.EnqueueRequest(h.ctx, ReqUnpin, TargetPayload{Slot: "review1"}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	h.wantState(2, store.PRReviewed)
}

func TestOverloadBacksOffWithoutPausingTheTool(t *testing.T) {
	h := newHarness(t)
	calls := 0
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		calls++
		if calls == 1 {
			return pipeline.RoundResult{Outcome: pipeline.OutcomeOverloaded, Round: 1,
				Pause: &pipeline.Pause{Kind: string(agents.HealthOverloaded), Tool: agents.KindCodex}}, nil
		}
		return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: max(in.Round, 1), ReviewID: 9, Event: "APPROVED"}, nil
	}
	h.reviewedPRExpect(2, "b1", store.PRPaused)
	if _, ok := h.e.toolPause(h.ctx, "codex"); ok {
		t.Fatal("overload must not pause codex for everyone")
	}
	pr := h.pr(2)
	if pr.Attempts != 1 || pr.NextAttemptAt == nil {
		t.Fatalf("overload backoff: %+v", pr)
	}
	h.tick()
	if calls != 1 {
		t.Fatal("retried inside the backoff")
	}
	h.advance(time.Minute)
	h.tick()
	h.wantState(2, store.PRReviewed)
}

func TestReleasedPRReopens(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	h.open(prSpec{n: 1, head: "base1"})
	h.gh.closed[2] = "CLOSED"
	for range 3 {
		h.advance(30 * time.Second)
		h.tick()
	}
	h.advance(11 * time.Minute)
	h.tick()
	h.wantState(2, store.PRReleased)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick()
	h.wantState(2, store.PRReviewed)
}

func TestKickDaemonSignalsThePidfileProcess(t *testing.T) {
	storetest.Serial(t) // swaps processCommand, and SIGUSR1 reaches the whole test binary
	h := newHarness(t)
	if pid, err := KickDaemon(h.layout); err != nil || pid != 0 {
		t.Fatalf("no daemon: pid=%d err=%v", pid, err)
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGUSR1)
	defer signal.Stop(sigs)
	if err := writePid(h.layout.Pid()); err != nil {
		t.Fatal(err)
	}
	comm, args := "/usr/bin/vim", "/usr/bin/vim notes.txt" // a reused pid: never signalled
	defer func(orig func(int) (string, string, error)) { processCommand = orig }(processCommand)
	processCommand = func(pid int) (string, string, error) {
		if pid != os.Getpid() {
			t.Fatalf("ps of pid %d", pid)
		}
		return comm, args, nil
	}
	if pid, err := KickDaemon(h.layout); err == nil || pid != 0 || !strings.Contains(err.Error(), "not a magnum daemon") {
		t.Fatalf("kick of a non-magnum pid: pid=%d err=%v", pid, err)
	}
	select {
	case <-sigs:
		t.Fatal("signalled a process that is not the daemon")
	case <-time.After(100 * time.Millisecond):
	}
	comm, args = "/Users/example/My Tools/magnum", "/Users/example/My Tools/magnum daemon --once"
	pid, err := KickDaemon(h.layout)
	if err != nil || pid != os.Getpid() {
		t.Fatalf("kick: pid=%d err=%v", pid, err)
	}
	select {
	case <-sigs:
	case <-time.After(2 * time.Second):
		t.Fatal("SIGUSR1 not delivered")
	}
	if err := os.WriteFile(h.layout.Pid(), []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pid, err := DaemonPID(h.layout); err != nil || pid != 0 {
		t.Fatalf("stale pidfile: pid=%d err=%v", pid, err)
	}
	unlock, held, err := AcquireLock(h.layout.Lock())
	if err != nil || held {
		t.Fatalf("lock: held=%v err=%v", held, err)
	}
	if _, held, _ := AcquireLock(h.layout.Lock()); !held {
		t.Fatal("second lock must report held")
	}
	unlock()
}

func TestReviewRequestRefusals(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "a1"})
	h.startup()
	h.tick()
	ids := map[string]int64{}
	for name, p := range map[string]ReviewPayload{
		"unknown pr":       {PRTarget: PRTarget{Ref: "99"}},
		"unknown repo":     {PRTarget: PRTarget{Ref: "nope/nope#1"}},
		"unknown identity": {PRTarget: PRTarget{Ref: "1"}, As: "ghost"},
	} {
		id, err := h.st.EnqueueRequest(h.ctx, ReqReview, p)
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = id
	}
	h.tick()
	for name, id := range ids {
		r, _ := h.st.RequestByID(h.ctx, id)
		if r.State != store.RequestFailed {
			t.Fatalf("%s: %+v", name, r)
		}
	}
	h.wantState(1, store.PRBaseline)
}

// A PR the dispatcher skips records why (store.KVPRGate) so `magnum status`
// can show "waiting: <reason>"; the key goes away when its round starts.
func TestDispatchRecordsTheGateReason(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync: #1 baseline
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
	h.tick() // #2 queued
	pr := h.wantState(2, store.PRQueued)
	if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("human_active_at", h.clock.Now()) }); err != nil {
		t.Fatal(err)
	}
	h.advance(5 * time.Minute)
	h.tick()
	h.wantState(2, store.PRQueued)
	if v, _ := h.e.getKV(h.ctx, store.KVPRGate(pr.ID)); v != "human active in the PR's panes" {
		t.Fatalf("gate kv = %q", v)
	}
	h.advance(20 * time.Minute) // past daemon.human_cooldown
	h.tick()
	h.wantState(2, store.PRReviewed)
	if v, ok := h.e.getKV(h.ctx, store.KVPRGate(pr.ID)); ok {
		t.Fatalf("gate kv kept after the round started: %q", v)
	}
}

// `magnum pause` holds automatic reviews only: a review the user asks for
// (a forced PR) still runs, and the request's answer does not say it waits;
// a PR that is not forced waits and says why. A drain for a restart holds
// every round, forced or not.
func TestPauseHoldsAutomaticReviewsNotRequestedOnes(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "a1"}, prSpec{n: 2, head: "b1"})
	h.startup()
	h.tick()
	h.enqueue(ReqPause, PausePayload{Reason: "lunch"})
	h.tick()
	h.open(prSpec{n: 1, head: "a1"}, prSpec{n: 2, head: "b2"}) // a push: an automatic round would follow
	h.advance(time.Hour)
	id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Repo: "talkable/talkable", Number: 1}})
	h.tick()
	h.wantState(1, store.PRReviewed)
	if r, err := h.st.RequestByID(h.ctx, id); err != nil || strings.Contains(deref(r.Result), "waiting") {
		t.Fatalf("request answer %q (%v)", deref(r.Result), err)
	}
	h.advance(10 * time.Minute) // past the push's quiet period: only the pause holds PR #2
	h.tick()
	if pr := h.pr(2); pr.State == store.PRReviewed || pr.State == store.PRReviewing {
		t.Fatalf("PR #2 reviewed while paused: %s", pr.State)
	}
	if w := waitOf(t, h, 2); w.Reason != WaitPaused {
		t.Fatalf("PR #2 wait = %+v, want the pause", w)
	}

	h.e.setKV(h.ctx, KVDaemonDraining, store.FormatTime(h.clock.Now()))
	h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Repo: "talkable/talkable", Number: 2}})
	h.tick()
	if pr := h.pr(2); pr.State == store.PRReviewed || pr.State == store.PRReviewing {
		t.Fatalf("a forced PR ran during a drain: %s", pr.State)
	}
}
