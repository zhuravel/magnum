package engine

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/pipeline"
)

// localRound is round n of the factory's local review of task 7's change in
// checkout: a case of the change's high synthetic number, its base and the
// description and ticket files the factory wrote.
func localRound(h *harness, checkout string, n int) LocalRound {
	c := evalCase(h, checkout, false)
	c.Number = 900000 + 7*10 + n
	c.URL = fmt.Sprintf("https://github.com/zhuravel/widgets/pull/%d", c.Number)
	scratch := filepath.Join(h.layout.Home, "local", fmt.Sprint(n))
	return LocalRound{EvalCase: c, BaseSHA: strings.Repeat("cd", 20),
		DescriptionFile: filepath.Join(scratch, "description.md"), TicketFile: filepath.Join(scratch, "ticket.md")}
}

// An engine on the live layout never runs a local round, as it never runs a
// replay: the factory's rounds cannot write magnum's registry, reports or
// notes.
func TestRunLocalRefusesALiveLayout(t *testing.T) {
	h := newHarness(t)
	if _, _, err := h.e.RunLocal(h.ctx, localRound(h, t.TempDir(), 1)); !errors.Is(err, ErrEvalLayout) {
		t.Fatalf("RunLocal on the live layout: %v", err)
	}
	if len(h.rd.inputs) != 0 {
		t.Fatalf("a round ran: %+v", h.rd.inputs)
	}
}

// A local round is a blind dry run that always carries the change's base:
// the round input's base is the factory's (whatever the checkout's base
// branch says), its Local names the factory's files, and the post_review
// line rendered from it runs with --dry-run and --local-base <base>, so
// post-review reads nothing from GitHub. A round without a base or a
// description file never starts.
func TestRunLocalAlwaysGivesPostReviewTheBase(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.layout = paths.Layout{Home: h.layout.Home, Scratch: t.TempDir()}
		h.d.Layout = h.layout
	})
	h.rd.script = func(pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomeDryRun}, nil
	}
	for _, bad := range []func(*LocalRound){func(c *LocalRound) { c.BaseSHA = "" }, func(c *LocalRound) { c.BaseSHA = "master" },
		func(c *LocalRound) { c.DescriptionFile = "" }} {
		c := localRound(h, t.TempDir(), 1)
		bad(&c)
		if _, _, err := h.e.RunLocal(h.ctx, c); err == nil {
			t.Fatalf("RunLocal(%+v) ran", c)
		}
	}
	if len(h.rd.inputs) != 0 {
		t.Fatalf("a round without its base ran: %+v", h.rd.inputs)
	}
	c := localRound(h, t.TempDir(), 1)
	res, pr, err := h.e.RunLocal(h.ctx, c)
	if err != nil || res.Outcome != pipeline.OutcomeDryRun || pr.Number != c.Number {
		t.Fatalf("RunLocal: %+v, %+v, %v", res, pr, err)
	}
	if len(h.rd.inputs) != 1 {
		t.Fatalf("rounds: %d", len(h.rd.inputs))
	}
	in := h.rd.inputs[0]
	if !in.DryRun || !in.Blind || in.BaseSHA != c.BaseSHA || in.TargetSHA != c.Head || in.Local == nil ||
		in.Local.DescriptionFile != c.DescriptionFile || in.Local.TicketFile != c.TicketFile {
		t.Fatalf("round input: dry=%v blind=%v base=%s target=%s local=%+v", in.DryRun, in.Blind, in.BaseSHA, in.TargetSHA, in.Local)
	}
	line := agents.PostReviewLine(agents.JudgeData{Owner: "zhuravel", Repo: "widgets", Number: in.PR.Number, HeadSHA: in.TargetSHA,
		RunID: "r-1", ReviewerLogin: "zhuravel", DryRun: in.DryRun, Blind: in.Blind, BaseSHA: in.BaseSHA, ReviewFile: "/reviews/review.json"})
	if !strings.Contains(line, " --dry-run ") || !strings.Contains(line, " --local-base "+c.BaseSHA+" ") {
		t.Fatalf("post_review %q lacks --dry-run --local-base %s", line, c.BaseSHA)
	}
}
