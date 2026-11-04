package runtime

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/usage"
)

func TestLaunchRecordsTheAttemptBeforeTheWorkspace(t *testing.T) {
	g := newRig(t)
	var atCreate *registry.Attempt
	g.h.onCreate = func(herdr.WorkspaceCreateOptions) {
		a, err := g.reg.AttemptByID(g.ctx, g.attempt.ID)
		if err != nil {
			t.Errorf("AttemptByID at the workspace's creation: %v", err)
			return
		}
		atCreate = &a
	}

	g.launch("fx-repro")

	want := "factory PR-12345 " + g.attempt.ID
	if atCreate == nil {
		t.Fatal("no workspace was created")
	}
	if atCreate.State != registry.AttemptLaunching || atCreate.WorkspaceLabel != want {
		t.Fatalf("when the workspace was created the attempt was %s labelled %q, want launching labelled %q",
			atCreate.State, atCreate.WorkspaceLabel, want)
	}
	if got := g.h.creates[0].Label; got != want {
		t.Fatalf("workspace label %q, want %q (unique per task and attempt)", got, want)
	}
	a, err := g.reg.AttemptByID(g.ctx, g.attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != registry.AttemptRunning {
		t.Fatalf("after the launch the attempt is %s, want running", a.State)
	}
}

func TestTwoRolesOfOneTaskGetTwoPanesInOneWorkspace(t *testing.T) {
	g := newRig(t)

	g.launch("fx-repro", "fx-history")

	if len(g.h.creates) != 1 {
		t.Fatalf("%d workspaces created, want one for the task's attempt", len(g.h.creates))
	}
	starts := g.h.startCalls()
	if len(starts) != 2 {
		t.Fatalf("%d agents started, want 2", len(starts))
	}
	repro, history := g.session("fx-repro"), g.session("fx-history")
	if store.Deref(repro.HerdrWorkspaceID) != store.Deref(history.HerdrWorkspaceID) {
		t.Fatalf("the roles sit in workspaces %s and %s, want one", store.Deref(repro.HerdrWorkspaceID), store.Deref(history.HerdrWorkspaceID))
	}
	if store.Deref(repro.HerdrPaneID) == store.Deref(history.HerdrPaneID) {
		t.Fatalf("both roles share pane %s", store.Deref(repro.HerdrPaneID))
	}
	for _, s := range []store.Session{repro, history} {
		if s.Env["MAGNUM_FACTORY_TASK"] != strconv.FormatInt(g.task.ID, 10) || s.Env["RAILS_ENV"] != "development" {
			t.Errorf("%s pane env %v, want the copy's env and MAGNUM_FACTORY_TASK", s.Role, s.Env)
		}
		if store.Deref(s.Cwd) != g.copy {
			t.Errorf("%s works in %q, want the copy %q", s.Role, store.Deref(s.Cwd), g.copy)
		}
	}
	if args := strings.Join(starts[0].Args, " "); !strings.Contains(args, "--name factory PR-12345 fx-repro - talkable") {
		t.Fatalf("the claude launch %q does not name the task", args)
	}
}

func TestLaunchRecordsTheRoleRunFacts(t *testing.T) {
	g := newRig(t)

	g.launch("fx-judge")

	rr := g.roleRun("fx-judge")
	if rr.CLI != agents.KindClaude || rr.CLIVersion != "2.1.400 (Claude Code)" || rr.Model != "opus" {
		t.Fatalf("role run CLI %q %q model %q, want claude 2.1.400 (Claude Code) on opus", rr.CLI, rr.CLIVersion, rr.Model)
	}
	if rr.MagnumBuild == "" || len(rr.PromptHash) != 12 || len(rr.ConfigHash) != 12 {
		t.Fatalf("role run build %q, prompt hash %q, config hash %q: want all three", rr.MagnumBuild, rr.PromptHash, rr.ConfigHash)
	}
	if !rr.Launched.Equal(t0) {
		t.Fatalf("launched at %v, want %v", rr.Launched, t0)
	}
	if rr.SessionID != "" || rr.Transcript != "" {
		t.Fatalf("a fresh session records session %q and transcript %q before it has either", rr.SessionID, rr.Transcript)
	}

	// The conversation's id comes with the first prompt; the next observe
	// pass records it and the transcript that holds it.
	g.send("fx-judge", "merge the findings")
	g.h.setSession(g.agent("fx-judge"), "sid-judge")
	g.writeTranscript("sid-judge", assistantLine(g.clock.Now(), "reading"))
	g.observe()

	rr = g.roleRun("fx-judge")
	if rr.SessionID != "sid-judge" || !strings.HasSuffix(rr.Transcript, "sid-judge.jsonl") {
		t.Fatalf("role run session %q transcript %q, want sid-judge and its transcript", rr.SessionID, rr.Transcript)
	}
}

func TestObserveEndsARunAfterTwoIdleTicks(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	id := g.send("fx-repro", "reproduce the bug")
	if evs := g.observe(); len(evs) != 0 {
		t.Fatalf("a working agent gave events %v", evs)
	}

	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
	if evs := g.observe(); len(evs) != 0 {
		t.Fatalf("one idle tick gave %v, want nothing yet", evs)
	}
	evs := g.observe()

	want := Event{TaskID: g.task.ID, Role: "fx-repro", RunID: id, Kind: EventEnded}
	if !slices.Equal(evs, []Event{want}) {
		t.Fatalf("the second idle tick gave %+v, want %+v", evs, want)
	}
}

func TestObserveReportsAUsageLimitAsPausedNotFailed(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	id := g.send("fx-repro", "reproduce the bug")
	g.observe()

	g.h.screen(g.agent("fx-repro"), "Reading spec/models/user_spec.rb\nYou've hit your limit · resets 4pm (UTC)\n")
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
	evs := g.observeN(2)

	want := Event{TaskID: g.task.ID, Role: "fx-repro", RunID: id, Kind: EventPaused, Reason: "usage: claude limit until 16:00"}
	if !slices.Equal(evs, []Event{want}) {
		t.Fatalf("events %+v, want only %+v", evs, want)
	}
	if rr := g.roleRun("fx-repro"); rr.Outcome != "paused" {
		t.Fatalf("role run outcome %q, want paused", rr.Outcome)
	}
	if r := g.storeRun(id); r.State == store.RunFailed {
		t.Fatalf("the paused run is %s", r.State)
	}

	_, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "go on")
	var w *WaitError
	if !errors.As(err, &w) || err.Error() != "usage: claude limit until 16:00" {
		t.Fatalf("Send during the pause = %v, want the wait usage: claude limit until 16:00", err)
	}
	if n := len(g.h.promptTexts()); n != 1 {
		t.Fatalf("%d prompts sent, want only the first", n)
	}

	g.clock.Set(time.Date(2026, 11, 4, 16, 1, 0, 0, time.UTC))
	g.observe()
	g.send("fx-repro", "go on")
}

func TestObserveReportsAnUnknownDialogAsBlocked(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.send("fx-repro", "reproduce the bug")
	g.observe()
	before := len(g.h.keyCalls())

	g.h.screen(g.agent("fx-repro"), "Select a theme\n❯ 1. Dark\n  2. Light\nPress enter to confirm\n")
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusBlocked)
	evs := g.observeN(3)

	blocked := only(evs, EventBlocked)
	if len(blocked) != 1 || blocked[0].Reason != "dialog: Press enter to confirm" {
		t.Fatalf("events %+v, want one blocked event naming the dialog", evs)
	}
	if keys := g.h.keyCalls()[before:]; len(keys) != 0 {
		t.Fatalf("keys %+v went to a dialog the factory does not answer (the retro's esc included)", keys)
	}
}

func TestACodexHooksReviewHoldsTheTaskWithItsReason(t *testing.T) {
	g := newRig(t)
	g.launch("fx-cause")
	g.send("fx-cause", "find the cause")
	g.observe()
	before := len(g.h.keyCalls())

	g.h.screen(g.agent("fx-cause"), "\n  Hooks need review\n\n  3 hooks are new or changed.\n\n› 1. Review hooks\n"+
		"  2. Trust all and continue\n  3. Continue without trusting (hooks won't run)\n\n  enter confirm · esc cancel\n")
	g.h.setStatus(g.agent("fx-cause"), herdr.StatusBlocked)
	evs := g.observeN(2)

	trust := only(evs, EventHooksTrust)
	if len(trust) != 1 || trust[0].Role != "fx-cause" || !strings.Contains(trust[0].Reason, "hooks need review") {
		t.Fatalf("events %+v, want one hooks-trust hold for fx-cause", evs)
	}
	if len(only(evs, EventBlocked)) != 0 {
		t.Fatalf("the hooks review also came as a plain dialog: %+v", evs)
	}
	if keys := g.h.keyCalls()[before:]; len(keys) != 0 {
		t.Fatalf("keys %+v went to the hooks review", keys)
	}
}

func TestACodexRefusalMovesTheRolesToClaudeWithNoRetry(t *testing.T) {
	g := newRig(t)
	g.launch("fx-cause")
	id := g.send("fx-cause", "find the cause")
	g.observe()

	g.h.screen(g.agent("fx-cause"), "■ This content was flagged for possible cybersecurity risk.\n")
	g.h.setStatus(g.agent("fx-cause"), herdr.StatusIdle)
	evs := g.observeN(3)

	refused := only(evs, EventRefused)
	if len(refused) != 1 || refused[0].RunID != id || refused[0].Role != "fx-cause" {
		t.Fatalf("events %+v, want one refusal of fx-cause's run", evs)
	}
	if len(only(evs, EventEnded)) != 0 {
		t.Fatalf("a refused run ended as a normal one: %+v", evs)
	}
	if n := len(g.h.promptTexts()); n != 1 {
		t.Fatalf("%d prompts, want no retry", n)
	}

	// The engine marks the task never for Codex; its Codex roles run as
	// their Claude fallback from then on.
	if err := g.reg.TransitionTask(g.ctx, g.task.ID, nil, "", func(u *registry.TaskUpdate) { u.Set("codex_never", true) }); err != nil {
		t.Fatal(err)
	}
	task, err := g.reg.TaskByID(g.ctx, g.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	g.task, g.attempt = task, g.begin(task, "investigate")
	g.launch("fx-cause")
	starts := g.h.startCalls()
	last := starts[len(starts)-1]
	if last.Kind != agents.KindClaude || last.Name != g.agent("fx-cause-claude") {
		t.Fatalf("the relaunch started %s %s, want the Claude fallback fx-cause-claude", last.Kind, last.Name)
	}
	g.send("fx-cause", "find the cause")
	prompts := g.h.prompts
	if got := prompts[len(prompts)-1].Target; got != g.agent("fx-cause-claude") {
		t.Fatalf("the prompt went to %s, want fx-cause-claude", got)
	}
}

func TestAModelLimitSwitchesToTheFallbackModel(t *testing.T) {
	g := newRig(t)
	g.launch("fx-judge")
	first := g.send("fx-judge", "merge the findings")
	g.observe()
	g.h.onRun = func(f *fakeHerdr, pane, command string) {
		if command == "/model sonnet" {
			f.screens[pane] = "Switched.\nSonnet 5 | copy | 12% context\n"
		}
	}

	g.h.screen(g.agent("fx-judge"), "You've hit your Opus limit · resets 3:45pm\n")
	g.h.setStatus(g.agent("fx-judge"), herdr.StatusIdle)
	evs := g.observeN(2)

	if len(evs) != 0 {
		t.Fatalf("a model limit with a fallback gave events %+v, want none (the runtime heals it)", evs)
	}
	runs := g.h.runCalls()
	if len(runs) == 0 || runs[len(runs)-1].Command != "/model sonnet" {
		t.Fatalf("pane runs %+v, want /model sonnet", runs)
	}
	prompts := g.h.promptTexts()
	if len(prompts) != 2 || !strings.Contains(prompts[1], "switched this session to sonnet") {
		t.Fatalf("prompts %q, want the model-fallback continuation", prompts)
	}
	if r := g.storeRun(first); r.State != store.RunAbandoned {
		t.Fatalf("the limited run is %s, want abandoned for its continuation", r.State)
	}
}

func TestALoginPauseLiftsOnceTheCLIIsLoggedInAgain(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.send("fx-repro", "reproduce the bug")
	g.observe()
	g.mu.Lock()
	g.loggedOut[agents.KindClaude] = true
	g.mu.Unlock()
	g.h.screen(g.agent("fx-repro"), "API Error: 401 · Please run /login\n")
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
	if paused := only(g.observeN(2), EventPaused); len(paused) != 1 || paused[0].Reason != "login: claude is logged out" {
		t.Fatalf("events %+v, want the login pause", paused)
	}
	g.clock.Add(time.Minute)
	g.observe()
	if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-repro"), "go on"); err == nil {
		t.Fatal("the login pause lifted while claude auth status says logged out")
	}

	g.mu.Lock()
	g.loggedOut[agents.KindClaude] = false // the operator ran claude auth login
	g.mu.Unlock()
	g.clock.Add(time.Minute)
	g.observe()

	g.send("fx-repro", "go on")
}

func TestALoginChangeLiftsALoginPause(t *testing.T) {
	g := newRig(t)
	g.launch("fx-cause")
	g.send("fx-cause", "find the cause")
	g.observe()

	g.h.screen(g.agent("fx-cause"), "■ Your access token could not be refreshed. Please run codex login.\n")
	g.h.setStatus(g.agent("fx-cause"), herdr.StatusIdle)
	evs := g.observeN(2)

	paused := only(evs, EventPaused)
	if len(paused) != 1 || paused[0].Reason != "login: codex is logged out" {
		t.Fatalf("events %+v, want the login pause", evs)
	}
	_, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-cause"), "go on")
	if err == nil || err.Error() != "login: codex is logged out" {
		t.Fatalf("Send during the login pause = %v", err)
	}
	g.mu.Lock()
	g.loggedOut[agents.KindCodex] = true // codex login status agrees
	g.mu.Unlock()
	g.clock.Add(time.Minute)
	g.observe()
	if _, err := g.r.Send(g.ctx, g.task, g.attempt, g.role("fx-cause"), "go on"); err == nil {
		t.Fatal("the login pause lifted with Codex still logged out")
	}

	g.setLogin("acct-2")
	g.clock.Add(time.Minute)
	g.observe()

	g.send("fx-cause", "go on")
}

func TestAnAccountSwitchRestartsIdleCodexSessions(t *testing.T) {
	g := newRig(t)
	g.launch("fx-cause", "fx-critic")
	g.send("fx-cause", "find the cause")
	g.send("fx-critic", "read the plan")
	g.h.setSession(g.agent("fx-cause"), "11111111-1111-4111-8111-111111111111")
	g.h.setSession(g.agent("fx-critic"), "22222222-2222-4222-8222-222222222222")
	g.observe()
	g.h.setStatus(g.agent("fx-cause"), herdr.StatusIdle)
	if ended := only(g.observeN(2), EventEnded); len(ended) != 1 {
		t.Fatalf("fx-cause's turn did not end: %+v", ended)
	}
	g.h.setStatus(g.agent("fx-critic"), herdr.StatusIdle) // idle for a tick, its run still in flight
	startsBefore, keysBefore := len(g.h.startCalls()), len(g.h.keyCalls())
	criticPane := store.Deref(g.session("fx-critic").HerdrPaneID)

	g.setLogin("acct-2")
	g.clock.Add(time.Minute)
	g.observe()

	starts := g.h.startCalls()[startsBefore:]
	if len(starts) != 1 || starts[0].Name != g.agent("fx-cause") ||
		!strings.Contains(strings.Join(starts[0].Args, " "), "resume 11111111-1111-4111-8111-111111111111") {
		t.Fatalf("starts after the switch %+v, want fx-cause resumed in a new process", starts)
	}
	for _, k := range g.h.keyCalls()[keysBefore:] {
		if k.Target == criticPane || k.Target == g.agent("fx-critic") {
			t.Fatalf("fx-critic, its run in flight, got keys %v", k.Keys)
		}
	}

	// Someone types into the restarted fx-cause: a working agent is not
	// restarted at the next switch.
	g.h.setStatus(g.agent("fx-cause"), herdr.StatusWorking)
	g.observe()
	startsBefore = len(g.h.startCalls())
	g.setLogin("acct-3")
	g.clock.Add(time.Minute)
	g.observe()
	for _, s := range g.h.startCalls()[startsBefore:] {
		if s.Name == g.agent("fx-cause") {
			t.Fatalf("the working fx-cause was restarted: %+v", s)
		}
	}
}

func TestASessionThatNeverTookItsFirstPromptStartsFresh(t *testing.T) {
	g := newRig(t)
	g.launch("fx-cause")
	g.send("fx-cause", "find the cause")
	g.observe()

	g.clock.Add(2 * time.Minute)
	g.observe()

	starts := g.h.startCalls()
	if len(starts) != 2 || strings.Contains(strings.Join(starts[1].Args, " "), "resume") {
		t.Fatalf("starts %+v, want a second, fresh start", starts)
	}
	if prompts := g.h.promptTexts(); !slices.Equal(prompts, []string{"find the cause", "find the cause"}) {
		t.Fatalf("prompts %q, want the first prompt again in the fresh session", prompts)
	}
	if evs := g.observe(); len(evs) != 0 {
		t.Fatalf("the fresh start gave events %+v", evs)
	}

	// The fresh session takes none either: the operator looks, and nothing
	// starts a third time.
	g.clock.Add(2 * time.Minute)
	evs := g.observe()
	if b := only(evs, EventBlocked); len(b) != 1 || !strings.Contains(b[0].Reason, "took no prompt") {
		t.Fatalf("events %+v, want the operator told once", evs)
	}
	if n := len(g.h.startCalls()); n != 2 {
		t.Fatalf("%d starts: the fresh session started fresh again", n)
	}
}

// Two Codex roles in one copy leave Codex's rollouts unable to say which is
// whose, so neither gets a session id from them; their rollouts still prove
// both took their prompts, and neither starts again.
func TestCodexSessionsWhoseRolloutsShowTheirTurnsAreLeftAlone(t *testing.T) {
	g := newRig(t)
	g.launch("fx-cause", "fx-critic")
	g.send("fx-cause", "find the cause")
	g.send("fx-critic", "read the plan")
	g.metaRollout("11111111-1111-4111-8111-111111111111", t0.Add(5*time.Second))
	g.metaRollout("22222222-2222-4222-8222-222222222222", t0.Add(6*time.Second))
	g.observe()

	g.clock.Add(2 * time.Minute)
	g.observeN(2)

	if n := len(g.h.startCalls()); n != 2 {
		t.Fatalf("%d starts: a Codex session whose rollout shows its turn started again", n)
	}
}

func TestLaunchReadsMemoryAndUsageEveryTime(t *testing.T) {
	g := newRig(t)

	g.launch("fx-repro", "fx-cause")
	if mem, use := g.reads(); mem != 2 || use != 1 {
		t.Fatalf("first launch read memory %d and usage %d times, want 2 (each role) and 1 (the Codex role)", mem, use)
	}

	g.attempt = g.begin(g.task, "investigate")
	g.launch("fx-repro", "fx-cause")
	if mem, use := g.reads(); mem != 4 || use != 2 {
		t.Fatalf("after the second launch memory %d and usage %d reads, want 4 and 2", mem, use)
	}
}

func TestLaunchesAreStaggeredAtWarn(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro", "fx-history")
	if slices.Contains(g.sleeps(), Stagger) {
		t.Fatalf("launches at normal pressure waited: %v", g.sleeps())
	}

	w := newRig(t)
	w.setMemory(engine.MemoryWarn)
	w.launch("fx-repro", "fx-history", "fx-cause")
	n := 0
	for _, d := range w.sleeps() {
		if d == Stagger {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("sleeps %v, want two waits of %s between three launches at warn", w.sleeps(), Stagger)
	}
}

func TestLaunchWaitsWhenCodexIsOverItsSoftCap(t *testing.T) {
	g := newRig(t)
	g.setUsage(usage.Snapshot{Window: usage.Window{UsedPercent: 96, WindowMinutes: 300, ResetsAt: t0.Add(7 * time.Hour)}})

	err := g.r.Launch(g.ctx, g.task, g.attempt, g.copy, g.roles("fx-cause"))

	var w *WaitError
	if !errors.As(err, &w) || err.Error() != "usage: codex at 96%, waits till 16:00" {
		t.Fatalf("Launch = %v, want the wait usage: codex at 96%%, waits till 16:00", err)
	}
	if len(g.h.creates) != 0 || len(g.h.startCalls()) != 0 {
		t.Fatalf("a held launch made workspaces %d and starts %d", len(g.h.creates), len(g.h.startCalls()))
	}
	g.launch("fx-repro") // Claude's roles do not wait on Codex's budget
}

func TestVerifyMovesEndedRunsOutOfActiveRuns(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	id := g.send("fx-repro", "reproduce the bug")
	g.observe()
	g.clock.Add(5 * time.Minute)
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
	g.observeN(2)
	if r := g.storeRun(id); r.State != store.RunEnded {
		t.Fatalf("run %s, want ended", r.State)
	}

	if err := g.r.Verify(g.ctx, id, true, "findings.md is written"); err != nil {
		t.Fatal(err)
	}

	active, err := g.st.ActiveRuns(g.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(active, func(r store.Run) bool { return r.ID == id }) {
		t.Fatalf("the verified run %s is still among the active runs", id)
	}
	if r := g.storeRun(id); r.State != store.RunVerified {
		t.Fatalf("run %s, want verified", r.State)
	}
	rr := g.roleRun("fx-repro")
	if rr.Outcome != "verified" || rr.EndFact != "findings" || rr.EndWhy != "findings.md is written" || rr.Verified.IsZero() ||
		rr.Ended.IsZero() || rr.ActiveSecs < 300 {
		t.Fatalf("role run %+v, want verified on findings with its end, verification and active seconds", rr)
	}

	again := g.send("fx-repro", "write findings.md")
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
	g.observeN(3)
	if err := g.r.Verify(g.ctx, again, false, "no findings.md"); err != nil {
		t.Fatal(err)
	}
	if r := g.storeRun(again); r.State != store.RunFailed {
		t.Fatalf("run %s, want failed", r.State)
	}
	if rr := g.roleRun("fx-repro"); rr.Outcome != "failed:no findings.md" {
		t.Fatalf("outcome %q, want failed:no findings.md", rr.Outcome)
	}
}

func TestTasksOfTwoRepositoriesOfOneNameEachGetAnOwnerRow(t *testing.T) {
	g := newRig(t)
	other, err := g.reg.CreateTask(g.ctx, registry.Task{JiraKey: "PR-12346", Kind: "bug", Repo: "example/talkable", State: registry.TaskActive})
	if err != nil {
		t.Fatal(err)
	}

	a := g.owner()
	b, err := g.r.Owner(g.ctx, other)
	if err != nil {
		t.Fatalf("Owner of a task of example/talkable: %v", err)
	}
	if a.ID == b.ID || b.Number != int(other.ID) {
		t.Fatalf("owner rows %d and %d (#%d), want one per task", a.ID, b.ID, b.Number)
	}
}

func TestOwnerRowHeadNeverReachesTheTask(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.send("fx-repro", "reproduce the bug")

	pr := g.owner()
	if pr.HeadSHA != g.attempt.ID || pr.Number != int(g.task.ID) || pr.URL != "file://"+g.layout.FactoryTaskDir(g.task.ID) {
		t.Fatalf("owner row #%d head %q url %q, want the task's number, its attempt and its directory", pr.Number, pr.HeadSHA, pr.URL)
	}
	repo, err := g.st.RepoByID(g.ctx, pr.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Owner+"/"+repo.Name != "factory/talkable" {
		t.Fatalf("owner repo %s/%s, want factory/talkable", repo.Owner, repo.Name)
	}
	task, err := g.reg.TaskByID(g.ctx, g.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.HeadSHA != "" {
		t.Fatalf("the task's head is %q: the owner row's head reached it", task.HeadSHA)
	}

	if err := g.reg.TransitionTask(g.ctx, g.task.ID, nil, "", func(u *registry.TaskUpdate) { u.Set("head_sha", "abc1234") }); err != nil {
		t.Fatal(err)
	}
	g.attempt = g.begin(g.task, "investigate")
	g.launch("fx-repro")
	if task, _ = g.reg.TaskByID(g.ctx, g.task.ID); task.HeadSHA != "abc1234" {
		t.Fatalf("the task's head is %q after a launch, want the engine's abc1234", task.HeadSHA)
	}
	if pr := g.owner(); pr.HeadSHA != g.attempt.ID {
		t.Fatalf("owner head %q, want the new attempt %s", pr.HeadSHA, g.attempt.ID)
	}
}
