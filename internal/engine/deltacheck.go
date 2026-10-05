package engine

// Delta checks (DECISIONS "A small re-review delta gets a judge-only
// check"): a push during the judge's turn swapped two images and changed 4
// lines in two templates; the review approved the older commit, magnum
// dismissed that approval 30 seconds later, and the 4 lines waited under
// rereview_min_lines for rereview_max_wait (2h), then got a full round of
// every reviewer (20 to 30 agent-minutes). A re-review whose delta is that
// small (eligibility.DeltaCheck) now runs after the quiet period as a round
// of the judge alone, in its own session at its rereview effort, without
// triage or reruns; the judge's prompt asks for a short review of those
// commits. An App's approval of the older commit stands until the check
// posts (approval.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/eligibility"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// KVPRDeltaCheck holds the delta check a PR's round in flight runs
// (DeltaCheckRound as JSON), which `magnum status` reads while the PR is
// in flight; the round's end deletes it.
func KVPRDeltaCheck(prID int64) string { return fmt.Sprintf("pr.%d.delta_check", prID) }

// DeltaCheckRound is a delta check in flight (KVPRDeltaCheck).
type DeltaCheckRound struct {
	Lines  int    `json:"lines"`  // the delta's changed code lines
	Files  int    `json:"files"`  // its files
	Target string `json:"target"` // the commit the check reviews
}

// ParseDeltaCheckRound reads a KVPRDeltaCheck value; ok is false for "" or
// a value it cannot read.
func ParseDeltaCheckRound(s string) (DeltaCheckRound, bool) {
	var d DeltaCheckRound
	if s == "" || json.Unmarshal([]byte(s), &d) != nil || d.Target == "" {
		return DeltaCheckRound{}, false
	}
	return d, true
}

// deltaCheckLabel is how events name a delta check of lines
// changed code lines: "delta check (4 lines)".
func deltaCheckLabel(lines int) string {
	return "delta check (" + textx.Count(lines, "line", "lines") + ")"
}

// deltaCheckDue reports whether pr's next round is a delta check: a
// re-review whose delta (f, with deltaFacts) gets one under w
// (eligibility.DeltaCheck), unless it is forced, requested (a review
// request: requested), names roles (`magnum review --role`, a simplify
// request) or reviews a merged PR. Those run in full.
func (e *Engine) deltaCheckDue(ctx context.Context, w config.Watch, pr store.PR, f eligibility.PRFacts, requested bool) bool {
	if pr.Forced || requested || postMerge(pr) || !eligibility.DeltaCheck(e.cfg.ThrottleFor(&w), f) {
		return false
	}
	return len(e.requestedRoles(ctx, pr.ID)) == 0
}

// confirmDeltaCheck measures, for a round dispatched as a delta check, the
// commits it reviews: the reviewed commit to target, the commit the
// checkout found (measureRange as the watch's poll identity, this tick's
// comparison when the gate made one). nil, with an event, when they no
// longer make a delta check (the checkout found a newer head that grew past
// it) or cannot be measured: the round then runs in full.
func (e *Engine) confirmDeltaCheck(ctx context.Context, job *roundJob, target string) *pipeline.DeltaCheck {
	reviewed := deref(job.pr.ReviewedSHA)
	subject := prSubject(job.repo, job.pr.Number)
	drop := func(why string) *pipeline.DeltaCheck {
		e.event(ctx, "info", subject, "round.delta_check_dropped", "a full round instead of the delta check: "+why,
			map[string]any{"reviewed_sha": reviewed, "target_sha": target})
		return nil
	}
	gh := e.gh(job.watch.PollIdentity)
	switch {
	case reviewed == "" || reviewed == target:
		return drop("no commits since the review to check")
	case gh == nil:
		return drop("no GitHub client to measure the commits")
	}
	m, err := e.measureRange(ctx, gh, job.repo, job.diffBase(), reviewed, target)
	if err != nil {
		return drop("the commits since the review could not be measured: " + oneLine(err.Error(), triageWhyRunes))
	}
	size := m.size()
	f := eligibility.PRFacts{ReviewedSHA: reviewed, DeltaReadable: size.Readable(), DeltaLines: size.Lines, DeltaAddedFiles: size.AddedFiles}
	if !eligibility.DeltaCheck(e.cfg.ThrottleFor(&job.watch), f) {
		return drop(fmt.Sprintf("the commits since the review up to %s are not a small delta (%d lines, %d added files)",
			textx.ShortSHA(target), size.Lines, size.AddedFiles))
	}
	dc := &pipeline.DeltaCheck{Lines: size.Lines}
	for _, fd := range m.files() {
		dc.Files = append(dc.Files, pipeline.DeltaFile{Path: fd.Path, Status: fd.Status, Binary: slices.Contains(size.Binaries, fd.Path)})
	}
	return dc
}

// judgeAlone is the first judge of roles (the one a round runs), alone.
func judgeAlone(roles []config.Role) []config.Role {
	if i := slices.IndexFunc(roles, func(r config.Role) bool { return r.Judge }); i >= 0 {
		return []config.Role{roles[i]}
	}
	return nil
}

// noteDeltaCheckRound records the delta check the PR's round runs
// (KVPRDeltaCheck), or that it runs none.
func (e *Engine) noteDeltaCheckRound(ctx context.Context, prID int64, dc *pipeline.DeltaCheck, target string) {
	if dc == nil {
		e.delKV(ctx, KVPRDeltaCheck(prID))
		return
	}
	if b, err := json.Marshal(DeltaCheckRound{Lines: dc.Lines, Files: len(dc.Files), Target: target}); err == nil {
		e.setKV(ctx, KVPRDeltaCheck(prID), string(b))
	}
}
