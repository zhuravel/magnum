package engine

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

func evalCase(h *harness, checkout string, notes bool) EvalCase {
	return EvalCase{
		Owner: "zhuravel", Repo: "widgets", Number: 7, URL: "https://github.com/zhuravel/widgets/pull/7",
		Head: strings.Repeat("ab", 20), BaseRef: "main", DefaultBranch: "main", Title: "t", Author: "alice",
		Checkout: checkout, MainClone: filepath.Join(h.layout.Home, "widgets"),
		Watch: h.cfg.Watches[1], Identity: "zhuravel", Notes: notes,
	}
}

// TestRunEvalRefusesTheLiveLayout: an engine on the live layout (no
// Scratch) never runs a replay, so magnum eval cannot write the live
// registry, reports or notes.
func TestRunEvalRefusesTheLiveLayout(t *testing.T) {
	h := newHarness(t)
	if _, _, err := h.e.RunEval(h.ctx, evalCase(h, t.TempDir(), false)); !errors.Is(err, ErrEvalLayout) {
		t.Fatalf("RunEval on the live layout: %v", err)
	}
	if len(h.rd.inputs) != 0 {
		t.Fatalf("a round ran: %+v", h.rd.inputs)
	}
}

// TestRunEvalRunsABlindDryRunAtThePinnedHead: a replay checks nothing out
// itself (the caller pinned the head), labels its workspace "eval
// <repo>#<N>", runs the watch's roles as a blind dry run that never restarts,
// and drops the notes unless asked; it never posts, so no GitHub client
// is involved.
func TestRunEvalRunsABlindDryRunAtThePinnedHead(t *testing.T) {
	for _, notes := range []bool{false, true} {
		h := newHarness(t, func(h *harness) {
			h.layout = paths.Layout{Home: h.layout.Home, Scratch: t.TempDir()}
			h.d.Layout = h.layout
		})
		h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
			return pipeline.RoundResult{Outcome: pipeline.OutcomeDryRun, Event: "COMMENT"}, nil
		}
		checkout := t.TempDir()
		c := evalCase(h, checkout, notes)
		res, pr, err := h.e.RunEval(h.ctx, c)
		if err != nil || res.Outcome != pipeline.OutcomeDryRun {
			t.Fatalf("RunEval: %+v, %v", res, err)
		}
		if len(h.rd.inputs) != 1 {
			t.Fatalf("rounds: %d", len(h.rd.inputs))
		}
		in := h.rd.inputs[0]
		if !in.DryRun || !in.Blind || in.MaxRestarts != 0 || in.TargetSHA != c.Head || in.SlotPath != checkout || in.Kind != pipeline.KindInitial {
			t.Fatalf("round input: dry=%v blind=%v restarts=%d target=%s slot=%s kind=%s",
				in.DryRun, in.Blind, in.MaxRestarts, in.TargetSHA, in.SlotPath, in.Kind)
		}
		if notes != (in.NotesPath != "") || (notes && !strings.HasPrefix(in.NotesPath, h.layout.Scratch)) {
			t.Fatalf("notes=%v: NotesPath %q", notes, in.NotesPath)
		}
		if calls := h.sl.all(); len(calls) != 0 {
			t.Fatalf("the replay touched slots: %v", calls)
		}
		if !slices.ContainsFunc(h.ag.all(), func(s string) bool { return strings.Contains(s, ":eval widgets#7:") }) {
			t.Fatalf("workspace label: %v", h.ag.all())
		}
		if pr.Number != 7 || pr.Identity != "zhuravel" || pr.HeadSHA != c.Head {
			t.Fatalf("seeded PR %+v", pr)
		}
		// One replay per scratch layout: a second one of the same PR refuses.
		if _, _, err := h.e.RunEval(h.ctx, c); err == nil || !strings.Contains(err.Error(), "already holds") {
			t.Fatalf("second RunEval: %v", err)
		}
		if got, err := h.st.PRByID(h.ctx, pr.ID); err != nil || got.State == store.PRClaiming {
			t.Fatalf("PR after the round: %+v, %v", got, err)
		}
	}
}

// TestRunEvalObservesHerdrDuringTheRound: outside the daemon loop nothing
// else marks a turn ended or an interrupted agent idle, so a replay observes
// herdr itself while its round runs, and stops when the round ends.
func TestRunEvalObservesHerdrDuringTheRound(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.layout = paths.Layout{Home: h.layout.Home, Scratch: t.TempDir()}
		h.d.Layout = h.layout
	})
	var seen int
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		deadline := time.Now().Add(5 * time.Second)
		for seen = h.ag.count("observe"); seen == 0 && time.Now().Before(deadline); seen = h.ag.count("observe") {
			time.Sleep(time.Millisecond)
		}
		return pipeline.RoundResult{Outcome: pipeline.OutcomeDryRun}, nil
	}
	if _, _, err := h.e.RunEval(h.ctx, evalCase(h, t.TempDir(), false)); err != nil {
		t.Fatal(err)
	}
	if seen == 0 {
		t.Fatal("no herdr observation while the round ran")
	}
	after := h.ag.count("observe")
	time.Sleep(20 * time.Millisecond)
	if n := h.ag.count("observe"); n != after {
		t.Fatalf("still observing after the round: %d → %d", after, n)
	}
}

// dryRunWithJudgeAtWork is a replay's round whose judge works on after it
// wrote its result (turnTail), as the judge of a run on 2026-10-08 did.
func dryRunWithJudgeAtWork(h *evalHerdr) func(pipeline.RoundInput) (pipeline.RoundResult, error) {
	return func(pipeline.RoundInput) (pipeline.RoundResult, error) {
		h.setStatus(store.RoleJudge, herdr.StatusWorking)
		return pipeline.RoundResult{Outcome: pipeline.OutcomeDryRun}, nil
	}
}

// evalCheckout is a replay's worktree, one directory of the run's worktrees
// (Worktrees), which every run of the case uses again.
func evalCheckout(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "wt", "oauth-session")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func evalCaseIn(h *harness, checkout string) EvalCase {
	c := evalCase(h, checkout, false)
	c.Worktrees = filepath.Dir(checkout)
	return c
}

// TestTwoReplaysOfACaseNeverShareAgents: two replays of one case in a row,
// the first one's judge still at work at its close (deaf to the interrupt,
// and herdr cannot close the workspace then, so the close fails and names
// the workspace). The second replay closes the workspace the first left
// open before it starts, all its sessions work in its own workspace, and no
// agent of it carries a name of the first one's. On 2026-10-08 the second
// run adopted the first one's judge, a plain shell by then, and its Claude
// reviewers with their conversations, by name.
func TestTwoReplaysOfACaseNeverShareAgents(t *testing.T) {
	h := newEvalHerdr()
	checkout := evalCheckout(t)

	first := newEvalReplay(t, h, "eval-a1b2c3")
	first.rd.script = dryRunWithJudgeAtWork(h)
	_, pr1, err := first.e.RunEval(first.ctx, evalCaseIn(first, checkout))
	if err != nil {
		t.Fatalf("first replay: %v", err)
	}
	live1 := first.liveSessions(pr1)
	if len(live1) == 0 {
		t.Fatal("the first replay has no live sessions")
	}
	firstWS, firstNames := deref(live1[0].HerdrWorkspaceID), h.startNames()
	h.mu.Lock()
	for _, n := range firstNames {
		if strings.Contains(n, "-"+store.RoleJudge+"-") {
			h.deaf[n] = true
		}
	}
	h.closeErr = errors.New("herdr: workspace busy")
	h.mu.Unlock()
	if err := first.e.ReleaseEval(first.ctx, pr1); err == nil || !strings.Contains(err.Error(), firstWS) {
		t.Fatalf("the first close: %v; want an error naming workspace %s", err, firstWS)
	}
	if !h.has(firstWS) {
		t.Fatalf("workspace %s closed although herdr refused", firstWS)
	}
	h.mu.Lock()
	h.closeErr = nil
	h.mu.Unlock()

	second := newEvalReplay(t, h, "eval-d4e5f6")
	second.rd.script = func(pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomeDryRun}, nil
	}
	_, pr2, err := second.e.RunEval(second.ctx, evalCaseIn(second, checkout))
	if err != nil {
		t.Fatalf("second replay: %v", err)
	}
	if h.has(firstWS) {
		t.Fatalf("the workspace %s the first replay left open is still open", firstWS)
	}
	live2 := second.liveSessions(pr2)
	if len(live2) != len(live1) {
		t.Fatalf("second replay: %d live sessions, the first had %d", len(live2), len(live1))
	}
	ownWS := deref(live2[0].HerdrWorkspaceID)
	if ownWS == firstWS || h.labelOf(ownWS) != "eval widgets#7" {
		t.Fatalf("second replay works in %s (%q)", ownWS, h.labelOf(ownWS))
	}
	for _, s := range live2 {
		if ws := deref(s.HerdrWorkspaceID); ws != ownWS || !strings.HasPrefix(deref(s.HerdrPaneID), ownWS+":") {
			t.Errorf("the %s session works in pane %s of %s, not in the replay's workspace %s", s.Role, deref(s.HerdrPaneID), ws, ownWS)
		}
		if n := deref(s.AgentName); n != "" && slices.Contains(firstNames, n) {
			t.Errorf("the %s session carries the first replay's agent name %s", s.Role, n)
		}
	}
	secondNames := h.startNames()[len(firstNames):]
	if len(secondNames) == 0 {
		t.Fatal("the second replay started no agent")
	}
	for _, n := range secondNames {
		if slices.Contains(firstNames, n) {
			t.Errorf("agent name %s started again: %v / %v", n, firstNames, secondNames)
		}
	}
}

// TestAReplayClosesOnlyTheEvalWorkspaceAnEarlierReplayLeftOpen: before a
// replay starts, a workspace labelled as a replay of the same PR whose panes
// all work in the run's worktrees (an earlier replay's, of this case or
// another of the PR) is closed. The PR's own workspace, a replay of another
// PR and a workspace with the label but a pane elsewhere (the user's) stay.
func TestAReplayClosesOnlyTheEvalWorkspaceAnEarlierReplayLeftOpen(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.layout = paths.Layout{Home: h.layout.Home, Scratch: t.TempDir()}
		h.d.Layout = h.layout
	})
	checkout := evalCheckout(t)
	other := filepath.Join(filepath.Dir(checkout), "other-case")
	h.hd.workspaces = []herdr.Workspace{
		{ID: "w-left", Label: "eval widgets#7"},
		{ID: "w-own", Label: "widgets#7"},
		{ID: "w-other", Label: "eval widgets#70"},
		{ID: "w-mine", Label: "eval widgets#7"},
	}
	h.hd.panes = []herdr.Pane{
		{ID: "w-left:p1", WorkspaceID: "w-left", Cwd: checkout},
		{ID: "w-left:p2", WorkspaceID: "w-left", Cwd: other},
		{ID: "w-own:p1", WorkspaceID: "w-own", Cwd: checkout},
		{ID: "w-other:p1", WorkspaceID: "w-other", Cwd: checkout},
		{ID: "w-mine:p1", WorkspaceID: "w-mine", Cwd: checkout},
		{ID: "w-mine:p2", WorkspaceID: "w-mine", Cwd: t.TempDir()},
	}
	var closedAtRound []string
	h.rd.script = func(pipeline.RoundInput) (pipeline.RoundResult, error) {
		h.hd.mu.Lock()
		closedAtRound = slices.Clone(h.hd.closed)
		h.hd.mu.Unlock()
		return pipeline.RoundResult{Outcome: pipeline.OutcomeDryRun}, nil
	}
	if _, _, err := h.e.RunEval(h.ctx, evalCaseIn(h, checkout)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(closedAtRound, []string{"w-left"}) || !slices.Equal(h.hd.closed, []string{"w-left"}) {
		t.Fatalf("closed %v (by the round: %v), want only w-left", h.hd.closed, closedAtRound)
	}
}

// TestTheCloseStepInterruptsABusyJudgeAndClosesTheWorkspace: at a replay's
// close the judge still works (turnTail). It gets one ctrl+c, which ends its
// turn as a cut own pass does, and the workspace is closed; the idle
// reviewers get no key. A judge that works on through the ctrl+c does not
// keep the workspace open either: the replay's workspace holds nothing to
// keep.
func TestTheCloseStepInterruptsABusyJudgeAndClosesTheWorkspace(t *testing.T) {
	for _, deaf := range []bool{false, true} {
		h := newEvalHerdr()
		r := newEvalReplay(t, h, "eval-a1b2c3")
		r.rd.script = dryRunWithJudgeAtWork(h)
		_, pr, err := r.e.RunEval(r.ctx, evalCaseIn(r, evalCheckout(t)))
		if err != nil {
			t.Fatal(err)
		}
		judge, err := r.st.LiveSessionByPRRole(r.ctx, pr.ID, store.RoleJudge)
		if err != nil {
			t.Fatal(err)
		}
		ws, name := deref(judge.HerdrWorkspaceID), deref(judge.AgentName)
		h.mu.Lock()
		h.deaf[name] = deaf
		h.mu.Unlock()
		if err := r.e.ReleaseEval(r.ctx, pr); err != nil {
			t.Fatalf("deaf=%v: close: %v", deaf, err)
		}
		if h.has(ws) {
			t.Fatalf("deaf=%v: workspace %s stays open", deaf, ws)
		}
		if keys := h.sent(); !slices.Equal(keys, []string{name + ":ctrl+c"}) {
			t.Fatalf("deaf=%v: keys %v, want one ctrl+c to %s", deaf, keys, name)
		}
	}
}

// TestAReplayWhoseSessionWorksElsewhereFailsBeforeAnyPrompt: an agent that
// carries the replay's judge name and works in the checkout from a
// workspace that is not a replay's (the user renamed it) is left alone by
// the leftover close, and StartAgent adopts it by its name; the case then
// fails at once, naming the pane, and no round runs, so nothing is prompted.
func TestAReplayWhoseSessionWorksElsewhereFailsBeforeAnyPrompt(t *testing.T) {
	h := newEvalHerdr()
	checkout := evalCheckout(t)
	const tag = "eval-a1b2c3"
	ws := h.addWorkspace("kept by hand", checkout)
	h.addAgent(ws+":p1", agents.TaggedAgentName(tag, "zhuravel/widgets", 7, agents.RoleJudge), agents.KindCodex, herdr.StatusIdle)
	r := newEvalReplay(t, h, tag)
	_, _, err := r.e.RunEval(r.ctx, evalCaseIn(r, checkout))
	if err == nil || !strings.Contains(err.Error(), ws+":p1") || !strings.Contains(err.Error(), "nothing was prompted") {
		t.Fatalf("RunEval: %v; want the case to fail naming pane %s:p1", err, ws)
	}
	if len(r.rd.inputs) != 0 || len(h.prompts) != 0 {
		t.Fatalf("a round ran (%d) or a prompt went out (%v)", len(r.rd.inputs), h.prompts)
	}
	if !h.has(ws) {
		t.Fatalf("workspace %s, not a replay's, was closed", ws)
	}
}
