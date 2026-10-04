package engine

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
