package runtime

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
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

// A pane's environment is its copy's, as the copies rendered it (the copy's
// Redis offset and name), with the task's id, and never a secret:
// sessions.env_json keeps it. A pane that takes a lost one's place after a
// restart gets the same.
func TestAPaneCarriesItsCopysRedisOffsetAndNoSecret(t *testing.T) {
	g := newRig(t)
	g.copyEnv = map[string]string{"REDIS_DB_OFFSET": "32", "WT_BRANCH": "factory2",
		"GITHUB_TOKEN": "ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"DEPLOY_KEY":   "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----",
		"SEED_NOTE":    "seeded with ghs_abcdefghijklmnopqrstuvwxyz0123456789"}
	check := func(when string, s store.Session) {
		t.Helper()
		if s.Env["REDIS_DB_OFFSET"] != "32" || s.Env["WT_BRANCH"] != "factory2" ||
			s.Env["MAGNUM_FACTORY_TASK"] != strconv.FormatInt(g.task.ID, 10) {
			t.Errorf("%s: pane env %v, want the copy's Redis offset 32, its name and the task", when, s.Env)
		}
		for k, v := range s.Env {
			if k == "GITHUB_TOKEN" || k == "DEPLOY_KEY" || k == "SEED_NOTE" || execx.Redact(v) != v {
				t.Errorf("%s: pane env keeps the secret %s", when, k)
			}
		}
	}
	g.launch("fx-repro")
	check("at the launch", g.session("fx-repro"))

	g.h.setSession(g.agent("fx-repro"), "sid-1")
	g.send("fx-repro", "reproduce the bug")
	g.observeN(2)
	g.restart()
	g.h.losePane(g.agent("fx-repro"))
	g.observe()
	g.observeN(2)
	if s := g.h.startCalls(); len(s) != 2 || !strings.Contains(strings.Join(s[1].Args, " "), "--resume sid-1") {
		t.Fatalf("starts %+v, want the lost conversation resumed", s)
	}
	check("in the pane that took the lost one's place", g.session("fx-repro"))
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

// The observe pass records a session's conversation on the row of its
// role in the attempt the owner row is at only for a role that attempt
// launched: a pane an earlier stage's role left live adds no row to a
// later stage's attempt, whose rows are its own roles'.
func TestObserveAddsNoRowForARoleOutsideTheStage(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.setSession(g.agent("fx-repro"), "sid-repro")
	g.send("fx-repro", "reproduce the bug")
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
	g.observeN(3) // its run ended: its live session has no run in flight

	g.attempt = g.begin(g.task, "judge")
	g.launch("fx-judge")
	g.h.setSession(g.agent("fx-judge"), "sid-judge")
	g.observeN(2)

	runs, err := g.reg.RoleRuns(g.ctx, g.attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Role != "fx-judge" || runs[0].SessionID != "sid-judge" {
		t.Fatalf("the judge attempt's rows %+v, want fx-judge's alone, with its conversation", runs)
	}
}

// A launch and a role's first prompt in the attempt record the hash of the
// prompt file the role's turns come from, as the stage runs the role: a
// stage's own prompt (the PR body's, the amend's, a review's fix turn) and
// not the role's. A later prompt and a recovery's start after a restart
// (its role read afresh from the configuration) keep the hash recorded.
func TestTheRoleRunRecordsThePromptTheStageRuns(t *testing.T) {
	g := newRig(t)
	judge := g.role("fx-judge")
	own := g.r.promptHash(judge)
	staged := judge
	staged.Prompt = "factory-pr-body.md"
	want := g.r.promptHash(staged)
	if own == "" || want == "" || own == want {
		t.Fatalf("prompt hashes %q (the role's) and %q (the stage's), want two", own, want)
	}

	if err := g.r.Launch(g.ctx, g.task, g.attempt, g.claimed(), []config.Role{staged}); err != nil {
		t.Fatal(err)
	}
	if rr := g.roleRun("fx-judge"); rr.PromptHash != want {
		t.Fatalf("the launch recorded prompt hash %q, want the stage's %q", rr.PromptHash, want)
	}
	if err := g.reg.RecordRoleRun(g.ctx, registry.RoleRun{AttemptID: g.attempt.ID, Role: "fx-judge", PromptHash: "older-build"}); err != nil {
		t.Fatal(err)
	}
	g.h.setSession(g.agent("fx-judge"), "sid-judge")
	if _, err := g.r.Send(g.ctx, g.task, g.attempt, staged, "write the PR body"); err != nil {
		t.Fatal(err)
	}
	if rr := g.roleRun("fx-judge"); rr.PromptHash != want {
		t.Fatalf("after the first prompt the record holds prompt hash %q, want the stage's %q", rr.PromptHash, want)
	}
	// A later prompt (a nudge, the answers) is no turn rendered from the
	// prompt file: it leaves the hash the first turn ran on, whatever an
	// install changed since.
	if err := g.reg.RecordRoleRun(g.ctx, registry.RoleRun{AttemptID: g.attempt.ID, Role: "fx-judge", PromptHash: "first-turn"}); err != nil {
		t.Fatal(err)
	}
	g.h.setStatus(g.agent("fx-judge"), herdr.StatusIdle)
	if _, err := g.r.Send(g.ctx, g.task, g.attempt, staged, "the factory checked your output"); err != nil {
		t.Fatal(err)
	}
	if rr := g.roleRun("fx-judge"); rr.PromptHash != "first-turn" {
		t.Fatalf("a later prompt rewrote the prompt hash to %q, want the first turn's kept", rr.PromptHash)
	}
	want = "first-turn"

	g.restart()
	g.h.losePane(g.agent("fx-judge"))
	g.observeN(3) // the lost pane's conversation is resumed
	if s := g.h.startCalls(); len(s) != 2 {
		t.Fatalf("starts %+v, want the lost pane recovered", s)
	}
	if rr := g.roleRun("fx-judge"); rr.PromptHash != want {
		t.Fatalf("after a recovery the record holds prompt hash %q, want the one recorded, %q", rr.PromptHash, want)
	}
}

// A turn that ended on questions is set aside, neither verified nor failed:
// the agents store keeps its run failed (a stage rebuilt after a restart
// takes its end again) with the outcome voided, its record says voided and
// why, and it is no failure of a loop without progress.
func TestVoidSetsAsideATurnThatAskedQuestions(t *testing.T) {
	g := newRig(t)
	g.launch("fx-judge")
	why := "it asked questions: the stage waits for your answers"
	for i := 1; i <= SameFailures; i++ {
		id := g.send("fx-judge", "merge the findings, turn "+strconv.Itoa(i))
		g.h.setStatus(g.agent("fx-judge"), herdr.StatusIdle)
		g.observeN(2)
		if err := g.r.Void(g.ctx, id, why); err != nil {
			t.Fatal(err)
		}
		if r := g.storeRun(id); r.State != store.RunFailed || store.Deref(r.Outcome) != "voided" || store.Deref(r.Error) != why {
			t.Fatalf("the voided run is %s, outcome %q, error %q; want failed, voided, %q", r.State, store.Deref(r.Outcome), store.Deref(r.Error), why)
		}
	}
	if rr := g.roleRun("fx-judge"); rr.Outcome != "voided" || rr.EndWhy != why || rr.Ended.IsZero() {
		t.Fatalf("role run %+v, want voided with why and its end", rr)
	}

	g.send("fx-judge", "the operator answered the questions")
	if slices.ContainsFunc(g.h.runCalls(), func(r paneRunCall) bool { return r.Command == agents.CompactCommand }) {
		t.Fatal("three turns that asked counted as the same failure three times: the session was compacted")
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
	if !reflect.DeepEqual(evs, []Event{want}) {
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
	if !reflect.DeepEqual(evs, []Event{want}) {
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
	// The engine reads the pause as Paused says it: a stage yields a Codex
	// role to its fallback while Codex is so paused.
	if p := g.r.Paused("claude"); p == nil || p.Who != "usage" || p.What != "claude limit until 16:00" || g.r.Paused("codex") != nil {
		t.Fatalf("Paused(claude) = %+v and Paused(codex) = %+v, want claude's usage pause alone", p, g.r.Paused("codex"))
	}

	g.clock.Set(time.Date(2026, 11, 4, 16, 1, 0, 0, time.UTC))
	g.observe()
	if p := g.r.Paused("claude"); p != nil {
		t.Fatalf("Paused(claude) = %+v past the limit's reset, want none", p)
	}
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

// Review Focus 5: an overload screen on a pane herdr shows as blocked
// pauses the kind as a usage limit does (the stage waits with the reason,
// at no cost), never "a dialog the factory does not answer", which would
// send the task to you as an unknown prompt.
func TestAnOverloadOnABlockedScreenPauses(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.send("fx-repro", "reproduce the bug")
	g.observe()

	g.h.screen(g.agent("fx-repro"), `  ⎿  API Error: 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`+"\n")
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusBlocked)
	evs := g.observeN(3)

	if b := only(evs, EventBlocked); len(b) != 0 {
		t.Fatalf("the overload came as a dialog: %+v", b)
	}
	paused := only(evs, EventPaused)
	if len(paused) != 1 || !strings.Contains(paused[0].Reason, "overload") {
		t.Fatalf("events %+v, want one pause naming the overload", evs)
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

// modelLimited has fx-judge's turn, its first run returned, end on Opus's
// own limit with sonnet free as its fallback; the next two observe passes
// switch it, and fail answers the prompt to go on (nil: it goes).
func (g *rig) modelLimited(fail func(text string) error) string {
	g.t.Helper()
	g.launch("fx-judge")
	first := g.send("fx-judge", "merge the findings")
	g.observe()
	g.h.onRun = func(f *fakeHerdr, pane, command string) {
		if command == "/model sonnet" {
			f.screens[pane] = "Switched.\nSonnet 5 | copy | 12% context\n"
		}
	}
	g.h.promptErr = func(text string) error {
		if strings.Contains(text, "switched this session to sonnet") {
			return fail(text)
		}
		return nil
	}
	g.h.screen(g.agent("fx-judge"), "You've hit your Opus limit · resets 3:45pm\n")
	g.h.setStatus(g.agent("fx-judge"), herdr.StatusIdle)
	return first
}

// A continuation on the fallback model that herdr refuses before it left is
// no turn that goes on: the stage hears its role paused (the agent's, no
// pause of its kind), its limited run is settled as a pause, so the stage
// sends the prompt to go on once it may, after a restart too, and its
// record says paused.
func TestAFallbackContinuationThatNeverLeftPausesTheRole(t *testing.T) {
	g := newRig(t)
	first := g.modelLimited(func(string) error {
		return &herdr.Error{Method: "agent.prompt", Code: herdr.CodeAgentNotFound, Message: "no such agent"}
	})

	evs := g.observeN(2)

	if len(evs) != 1 || evs[0].Kind != EventPaused || evs[0].RunID != first || evs[0].Role != "fx-judge" ||
		!strings.HasPrefix(evs[0].Reason, pauseAgent+": ") || !strings.Contains(evs[0].Reason, "sonnet") {
		t.Fatalf("events %+v, want fx-judge's limited run paused for the agent, on sonnet now", evs)
	}
	state, why, err := g.r.RunState(g.ctx, first)
	if err != nil || state != store.RunAbandoned || why != PausedPrefix+evs[0].Reason {
		t.Fatalf("RunState of the limited run = %q, %q, %v; want abandoned, %s%s", state, why, err, PausedPrefix, evs[0].Reason)
	}
	if rr := g.roleRun("fx-judge"); rr.Outcome != "paused" {
		t.Fatalf("role run outcome %q, want paused", rr.Outcome)
	}
	if w := g.r.paused(agents.KindClaude); w != nil {
		t.Fatalf("Claude pauses (%v): a prompt herdr refused is no limit of the kind", w)
	}
	g.restart()
	if state, after, err := g.r.RunState(g.ctx, first); err != nil || state != store.RunAbandoned || after != why {
		t.Fatalf("after a restart RunState of the limited run = %q, %q, %v; want the pause the rebuilt stage reads, %q", state, after, err, why)
	}
	g.h.promptErr = nil
	g.send("fx-judge", "Your turn stopped, and that is over now. Go on.")
}

// A continuation whose run cannot even be made (the agents store refuses
// it) is a continuation that never left: the role pauses for the agent as
// when herdr refused the prompt.
func TestAFallbackContinuationWithoutARunPausesTheRole(t *testing.T) {
	g := newRig(t)
	first := g.modelLimited(func(string) error { return nil })
	if _, err := g.st.DB().ExecContext(g.ctx, `CREATE TRIGGER no_continuation BEFORE INSERT ON runs WHEN NEW.kind = 'continue'
		BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}

	evs := g.observeN(2)

	if len(evs) != 1 || evs[0].Kind != EventPaused || evs[0].RunID != first || !strings.HasPrefix(evs[0].Reason, pauseAgent+": ") {
		t.Fatalf("events %+v, want fx-judge's limited run paused for the agent", evs)
	}
	for _, p := range g.h.promptTexts() {
		if strings.Contains(p, "switched this session to sonnet") {
			t.Fatalf("prompts %q: a continuation went without its run", g.h.promptTexts())
		}
	}
	if _, why, err := g.r.RunState(g.ctx, first); err != nil || why != PausedPrefix+evs[0].Reason {
		t.Fatalf("RunState of the limited run = %q, %v; want %s%s", why, err, PausedPrefix, evs[0].Reason)
	}
}

// A continuation herdr may have typed (its delivery unknown) is the
// ledger's to settle, as any prompt's: no event, and its entry stays
// unknown. Its limited run is settled as the agent's pause all the same:
// the stored reason is what a stage, rebuilt once the continuation is
// proven never to have arrived, reads as a pause (the stage's side is
// lastRunEvent's, tested with the stage).
func TestAFallbackContinuationOfUnknownDeliveryLeavesAPauseBehind(t *testing.T) {
	g := newRig(t)
	first := g.modelLimited(func(string) error {
		return fmt.Errorf("herdr agent.prompt: connection closed without a response: %w", io.EOF)
	})

	if evs := g.observeN(2); len(evs) != 0 {
		t.Fatalf("events %+v, want none while the continuation's delivery is unknown", evs)
	}
	if _, why, _ := g.r.RunState(g.ctx, first); !strings.HasPrefix(why, PausedPrefix+pauseAgent+": ") {
		t.Fatalf("the limited run was given up with %q, want a pause for the agent, %s%s: ...", why, PausedPrefix, pauseAgent)
	}
	es := g.ledger()
	if last := es[len(es)-1]; last.Delivered != registry.DeliveredUnknown || !strings.Contains(last.Text, "sonnet") {
		t.Fatalf("the continuation's entry %q is %s, want unknown", last.Text, last.Delivered)
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

// A drain cuts the stagger: a launch that would wait for it returns
// ErrDraining at once and starts no agent, so a SIGTERM drain never sleeps
// 20 seconds a role past launchd's ExitTimeOut. The launches before the
// drain stand.
func TestADrainCutsTheStaggerAndLaunchesNothing(t *testing.T) {
	g := newRig(t)
	g.setMemory(engine.MemoryWarn)
	g.launch("fx-repro")
	g.r.BeginDrain()
	err := g.r.Launch(g.ctx, g.task, g.attempt, g.claimed(), g.roles("fx-history"))
	if !errors.Is(err, ErrDraining) {
		t.Fatalf("a launch during the drain = %v, want ErrDraining", err)
	}
	if slices.Contains(g.sleeps(), Stagger) {
		t.Fatalf("the drained launch slept for the stagger: %v", g.sleeps())
	}
	if n := len(g.h.startCalls()); n != 1 {
		t.Fatalf("%d agents started, want fx-repro's only", n)
	}
}

func TestLaunchWaitsWhenCodexIsOverItsSoftCap(t *testing.T) {
	g := newRig(t)
	g.setUsage(usage.Snapshot{Window: usage.Window{UsedPercent: 96, WindowMinutes: 300, ResetsAt: t0.Add(7 * time.Hour)}})

	err := g.r.Launch(g.ctx, g.task, g.attempt, g.claimed(), g.roles("fx-cause"))

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

// An agent's start compares the checkout's project config with the merge
// base of HEAD and origin/<the row's base>: a repository whose base is main
// has no origin/master, and its sessions lost the project config.
func TestAnOwnerRowCarriesItsRepositorysBase(t *testing.T) {
	g := newRig(t)
	g.cfg.Factory.Repos = append(g.cfg.Factory.Repos, config.FactoryRepo{Repo: "example/app", Base: "main"})
	g.restart()
	other, err := g.reg.CreateTask(g.ctx, registry.Task{JiraKey: "PR-12347", Kind: "bug", Repo: "Example/App", State: registry.TaskActive})
	if err != nil {
		t.Fatal(err)
	}

	if got := store.Deref(g.owner().BaseRef); got != config.DefaultFactoryBase {
		t.Fatalf("owner row base %q for a repository without base, want %q", got, config.DefaultFactoryBase)
	}
	b, err := g.r.Owner(g.ctx, other)
	if err != nil {
		t.Fatalf("Owner of a task of example/app: %v", err)
	}
	if got := store.Deref(b.BaseRef); got != "main" {
		t.Fatalf("owner row base %q, want the repository's main", got)
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

// A run's state, as the agents store keeps it, tells a stage rebuilding
// after a restart a run its CLI's pause ended (abandoned, with the pause)
// from one that ended and one still at work.
func TestRunStateTellsAPausedRunFromAnEndedOne(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro", "fx-history")
	paused := g.send("fx-repro", "reproduce the bug")
	ended := g.send("fx-history", "read the history of the code")
	g.observe()
	if state, _, err := g.r.RunState(g.ctx, ended); err != nil || (state != store.RunSubmitted && state != store.RunWorking) {
		t.Fatalf("RunState of a run at work = %q, %v; want submitted or working", state, err)
	}

	g.h.screen(g.agent("fx-repro"), "Reading spec/models/user_spec.rb\nYou've hit your limit · resets 4pm (UTC)\n")
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
	g.h.setStatus(g.agent("fx-history"), herdr.StatusIdle)
	g.observeN(2)

	if state, why, err := g.r.RunState(g.ctx, paused); err != nil || state != store.RunAbandoned || why != PausedPrefix+"usage: claude limit until 16:00" {
		t.Fatalf("RunState of the paused run = %q, %q, %v; want abandoned, %susage: claude limit until 16:00", state, why, err, PausedPrefix)
	}
	if state, _, err := g.r.RunState(g.ctx, ended); err != nil || state != store.RunEnded {
		t.Fatalf("RunState of the ended run = %q, %v; want ended", state, err)
	}
	if _, _, err := g.r.RunState(g.ctx, "no-such-run"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("RunState of an unknown run = %v, want store.ErrNotFound", err)
	}
}

// AtWork names who is at work in the task's copy, so the factory's own
// commands there wait: the role whose run is in flight, then the role whose
// pane you are in (your edits are there); a task whose runs ended and whose
// panes you left, and one that never launched, has nobody at work. herdr's
// focus resting on a pane is no proof of you.
func TestAtWorkNamesTheRoleWhoseRunIsInFlight(t *testing.T) {
	g := newRig(t)
	if role, you, err := g.r.AtWork(g.ctx, g.task); err != nil || role != "" || you {
		t.Fatalf("AtWork before a launch = %q, %v, %v", role, you, err)
	}
	g.launch("fx-repro")
	g.send("fx-repro", "reproduce the bug")
	if role, you, err := g.r.AtWork(g.ctx, g.task); err != nil || role != "fx-repro" || you {
		t.Fatalf("AtWork with a prompt in flight = %q, %v, %v; want the fx-repro agent", role, you, err)
	}
	g.observe()
	g.h.setStatus(g.agent("fx-repro"), herdr.StatusIdle)
	g.observeN(2)
	if role, you, err := g.r.AtWork(g.ctx, g.task); err != nil || role != "" || you {
		t.Fatalf("AtWork after the run ended = %q, %v, %v; want nobody", role, you, err)
	}
	g.h.focus(g.agent("fx-repro"))
	g.observe()
	if role, you, err := g.r.AtWork(g.ctx, g.task); err != nil || role != "" || you {
		t.Fatalf("AtWork with herdr's focus resting on the pane = %q, %v, %v; want nobody", role, you, err)
	}
	g.clock.Add(2 * time.Minute)
	g.youAtWork("fx-repro")
	if role, you, err := g.r.AtWork(g.ctx, g.task); err != nil || role != "fx-repro" || !you {
		t.Fatalf("AtWork with you in the pane = %q, %v, %v; want you, in fx-repro's pane", role, you, err)
	}
	g.h.focus("")
	g.clock.Add(g.cfg.Daemon.HumanCooldown.Duration + time.Second)
	if role, you, err := g.r.AtWork(g.ctx, g.task); err != nil || role != "" || you {
		t.Fatalf("AtWork after you left the pane = %q, %v, %v; want nobody", role, you, err)
	}
}

// You are in a pane herdr shows focused only while herdr showed you at work
// in a factory pane within the human cooldown: its focus rests where it was
// when nobody is attached, and a task's copy waited on it for good.
func TestHerdrsFocusCountsAsYouOnlyWhileYouWereAtWorkLately(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.h.focus(g.agent("fx-repro"))
	g.observeN(3)
	if _, you, err := g.r.AtWork(g.ctx, g.task); err != nil || you {
		t.Fatalf("AtWork with the focus resting on the pane = %v, %v; want not you", you, err)
	}
	g.clock.Add(2 * time.Minute)
	g.youAtWork("fx-repro")
	old := g.clock.Now().Add(-time.Hour) // your input proves nothing more: only the focus and the observe pass's proof
	if err := g.st.UpdatePR(g.ctx, g.owner().ID, func(u *store.PRUpdate) { u.Set("human_active_at", old) }); err != nil {
		t.Fatal(err)
	}
	if role, you, err := g.r.AtWork(g.ctx, g.task); err != nil || role != "fx-repro" || !you {
		t.Fatalf("AtWork with the focus on the pane you were at work in = %q, %v, %v; want you", role, you, err)
	}
	g.h.focus("")
	if _, you, err := g.r.AtWork(g.ctx, g.task); err != nil || you {
		t.Fatalf("AtWork with the focus off the pane = %v, %v; want not you", you, err)
	}
	g.h.focus(g.agent("fx-repro"))
	g.clock.Add(g.cfg.Daemon.HumanCooldown.Duration)
	if _, you, err := g.r.AtWork(g.ctx, g.task); err != nil || you {
		t.Fatalf("AtWork a cooldown after you were at work = %v, %v; want not you", you, err)
	}
}

// TouchedSince names who could have changed the task's copy since a time:
// a prompt to one of its agents after it, or your input into one of its
// panes after it; nobody for a task that never launched or was left alone.
func TestTouchedSinceNamesAPromptOrYourInputAfterTheTime(t *testing.T) {
	g := newRig(t)
	since := g.clock.Now()
	if who, err := g.r.TouchedSince(g.ctx, g.task, since); err != nil || who != "" {
		t.Fatalf("TouchedSince before a launch = %q, %v", who, err)
	}
	g.launch("fx-repro")
	g.clock.Add(time.Minute)
	if who, err := g.r.TouchedSince(g.ctx, g.task, since); err != nil || who != "" {
		t.Fatalf("TouchedSince of a task left alone = %q, %v", who, err)
	}
	g.send("fx-repro", "reproduce the bug")
	if who, err := g.r.TouchedSince(g.ctx, g.task, since); err != nil || !strings.Contains(who, "fx-repro") {
		t.Fatalf("TouchedSince after a prompt = %q, %v; want the fx-repro agent", who, err)
	}
	later := g.clock.Now().Add(time.Second)
	g.clock.Add(time.Minute)
	if who, err := g.r.TouchedSince(g.ctx, g.task, later); err != nil || who != "" {
		t.Fatalf("TouchedSince after the prompt's time = %q, %v; want nobody", who, err)
	}
	typed := g.clock.Now()
	if err := g.st.UpdatePR(g.ctx, g.owner().ID, func(u *store.PRUpdate) { u.Set("human_active_at", typed) }); err != nil {
		t.Fatal(err)
	}
	if who, err := g.r.TouchedSince(g.ctx, g.task, later); err != nil || !strings.Contains(who, "you typed") {
		t.Fatalf("TouchedSince after your input = %q, %v; want you", who, err)
	}
}

// Once Codex never works on a task, its live Codex sessions are quit and
// their runs abandoned; its Claude sessions go on, and a session whose
// pane you are in waits for you to leave.
func TestQuitCodexQuitsOnlyTheTasksCodexSessions(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro", "fx-cause")
	cause := g.send("fx-cause", "find the cause")
	g.send("fx-repro", "reproduce it")

	typed := func(at time.Time) {
		t.Helper()
		if err := g.st.UpdatePR(g.ctx, g.owner().ID, func(u *store.PRUpdate) { u.Set("human_active_at", at) }); err != nil {
			t.Fatal(err)
		}
	}
	typed(g.clock.Now())
	if err := g.r.QuitCodex(g.ctx, g.task); !errors.Is(err, ErrOperatorInPane) {
		t.Fatalf("QuitCodex with you in the pane = %v, want ErrOperatorInPane", err)
	}
	g.session("fx-cause") // still live
	typed(g.clock.Now().Add(-time.Hour))
	g.h.focus(g.agent("fx-cause")) // herdr's focus resting there is no proof of you

	if err := g.r.QuitCodex(g.ctx, g.task); err != nil {
		t.Fatalf("QuitCodex = %v", err)
	}
	if s, err := g.st.LiveSessionByPRRole(g.ctx, g.owner().ID, "fx-cause"); err == nil {
		t.Fatalf("fx-cause's Codex session %d is still %s", s.ID, s.State)
	}
	if run := g.storeRun(cause); run.State != store.RunAbandoned {
		t.Fatalf("fx-cause's run is %s, want abandoned", run.State)
	}
	if s := g.session("fx-repro"); s.State != store.SessionLive {
		t.Fatalf("fx-repro's Claude session is %s, want live", s.State)
	}
	if err := g.r.QuitCodex(g.ctx, g.newTask("PR-12399")); err != nil {
		t.Fatalf("QuitCodex of a task that never launched = %v, want nothing to do", err)
	}
}

// A QA run that is cut stops its walker: QuitRole quits the live sessions of
// that one role of the task, whatever its kind, and abandons its runs in
// flight, also of a session whose pane you are in, which itself waits for
// you to leave; the task's other roles go on.
func TestQuitRoleQuitsOnlyThatRolesSessions(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro", "fx-cause")
	repro := g.send("fx-repro", "reproduce it")
	g.send("fx-cause", "find the cause")
	if err := g.st.UpdatePR(g.ctx, g.owner().ID, func(u *store.PRUpdate) { u.Set("human_active_at", g.clock.Now()) }); err != nil {
		t.Fatal(err)
	}
	if err := g.r.QuitRole(g.ctx, g.task, "fx-repro", "its QA run was cut"); !errors.Is(err, ErrOperatorInPane) {
		t.Fatalf("QuitRole with you in the pane = %v, want ErrOperatorInPane", err)
	}
	g.session("fx-repro")                                          // still live
	if run := g.storeRun(repro); run.State != store.RunAbandoned { // no one waits for it: the registry's, no key in your pane
		t.Fatalf("fx-repro's run is %s with you in its pane, want abandoned all the same", run.State)
	}
	if err := g.st.UpdatePR(g.ctx, g.owner().ID, func(u *store.PRUpdate) { u.Set("human_active_at", g.clock.Now().Add(-time.Hour)) }); err != nil {
		t.Fatal(err)
	}
	if err := g.r.QuitRole(g.ctx, g.task, "fx-repro", "its QA run was cut"); err != nil {
		t.Fatalf("QuitRole = %v", err)
	}
	if s, err := g.st.LiveSessionByPRRole(g.ctx, g.owner().ID, "fx-repro"); err == nil {
		t.Fatalf("fx-repro's session %d is still %s", s.ID, s.State)
	}
	if run := g.storeRun(repro); run.State != store.RunAbandoned {
		t.Fatalf("fx-repro's run is %s, want abandoned", run.State)
	}
	if s := g.session("fx-cause"); s.State != store.SessionLive {
		t.Fatalf("fx-cause's session is %s, want live", s.State)
	}
	if err := g.r.QuitRole(g.ctx, g.newTask("PR-12398"), "fx-repro", "its QA run was cut"); err != nil {
		t.Fatalf("QuitRole of a task that never launched = %v, want nothing to do", err)
	}
}

// The observe pass says, from the snapshot it reads anyway, whether herdr's
// focus is on a pane of a task's workspace, when herdr last showed you at
// work in a factory pane (the proof of you: the focus alone, resting where
// it was at a start or moved by herdr itself, is none), and when the focus
// left the factory's panes; a pane of a workspace of yours is no factory
// pane, and a herdr that does not answer shows you in none (the alerts'
// toasts hold while you are in one, spec 9.F).
func TestObserveSaysWhetherYouAreInAFactoryPane(t *testing.T) {
	g := newRig(t)
	g.launch("fx-repro")
	g.observe()
	if f := g.r.PaneFocus(); f != (PaneFocus{}) {
		t.Fatalf("PaneFocus with herdr's focus nowhere = %+v, want none", f)
	}
	pane := store.Deref(g.session("fx-repro").HerdrPaneID)
	g.h.focus(g.agent("fx-repro"))
	g.observe()
	g.observe()
	if f := g.r.PaneFocus(); f.Pane != pane || !f.Since.IsZero() || !f.Left.IsZero() {
		t.Fatalf("PaneFocus with herdr's focus moved onto fx-repro's pane = %+v, want %s with no proof of you", f, pane)
	}
	g.clock.Add(2 * time.Minute)
	g.youAtWork("fx-repro")
	atWork := g.clock.Now()
	if f := g.r.PaneFocus(); f.Pane != pane || !f.Since.Equal(atWork) {
		t.Fatalf("PaneFocus after herdr showed you at work = %+v, want since %v", f, atWork)
	}
	g.restart() // a daemon start: herdr's focus rests where it was
	g.observe()
	if f := g.r.PaneFocus(); f.Pane != pane || !f.Since.IsZero() {
		t.Fatalf("PaneFocus after a start = %+v, want %s with no proof of you", f, pane)
	}
	g.clock.Add(2 * time.Minute)
	g.youAtWork("fx-repro")
	atWork = g.clock.Now()

	mine, err := g.h.WorkspaceCreate(g.ctx, herdr.WorkspaceCreateOptions{Label: "notes", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	g.h.mu.Lock()
	g.h.focused = mine.RootPane.ID
	g.h.mu.Unlock()
	g.observe()
	left := g.clock.Now()
	g.observe()
	if f := g.r.PaneFocus(); f.Pane != "" || !f.Left.Equal(left) || !f.Since.Equal(atWork) {
		t.Fatalf("PaneFocus in a workspace of yours = %+v, want none, left at %v, you at work at %v", f, left, atWork)
	}

	g.h.focus(g.agent("fx-repro"))
	g.observe()
	g.h.mu.Lock()
	g.h.snapErr = errors.New("herdr: dial: connection refused")
	g.h.mu.Unlock()
	g.clock.Add(10 * time.Second)
	if _, err := g.r.Observe(g.ctx); err == nil {
		t.Fatal("Observe with herdr down = nil error")
	}
	if f := g.r.PaneFocus(); f.Pane != "" || !f.Left.Equal(g.clock.Now()) {
		t.Fatalf("PaneFocus with herdr down = %+v, want none, left at %v", f, g.clock.Now())
	}
}

// Workspaces are the herdr workspaces of a task's live sessions, each once
// (where its sidebar token goes); none for a task that never launched.
func TestWorkspacesAreThoseOfTheTasksLiveSessions(t *testing.T) {
	g := newRig(t)
	if ws, err := g.r.Workspaces(g.ctx, g.task); err != nil || len(ws) != 0 {
		t.Fatalf("Workspaces before a launch = %v, %v; want none", ws, err)
	}
	g.launch("fx-repro", "fx-cause")
	want := store.Deref(g.session("fx-repro").HerdrWorkspaceID)
	if ws, err := g.r.Workspaces(g.ctx, g.task); err != nil || !slices.Equal(ws, []string{want}) {
		t.Fatalf("Workspaces = %v, %v; want [%s]", ws, err, want)
	}
	if ws, err := g.r.Workspaces(g.ctx, g.newTask("PR-12399")); err != nil || len(ws) != 0 {
		t.Fatalf("Workspaces of a task that never launched = %v, %v; want none", ws, err)
	}
}
