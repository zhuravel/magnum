package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/store"
)

// replaysByKind counts the cases the gate's runs replayed, by the kind of
// run (its label's end: candidate, second, baseline, baseline judge).
func (f *gateFixture) replaysByKind() map[string]int {
	f.h.t.Helper()
	runs, err := eval.ListRuns(f.h.layout.Eval())
	if err != nil {
		f.h.t.Fatal(err)
	}
	out := map[string]int{}
	for _, r := range runs {
		if _, kind, ok := strings.Cut(r.Label, ": "); ok && strings.HasPrefix(r.Label, "learn gate of proposal ") {
			out[kind] += len(r.Cases)
		}
	}
	return out
}

// guardFixture is a gate whose operator's case (coupon-scope) guards: its
// newest stored replay, run 20261002-120000 with another proposal's inputs,
// found its defect, and master's own replays never did. The target has a
// stored baseline that misses it, and the candidate finds it every time.
func guardFixture(t *testing.T, candidate ...*eval.Score) *gateFixture {
	t.Helper()
	f := newGateFixture(t, gateFullFiles)
	f.baseline("20261001-120000", "rev-master", map[string]*eval.Score{f.target.Name: found(nil, f.target.Name)})
	f.fullRun("20261002-120000", "in-old", "rev-old", map[string]*eval.Score{f.other.Name: found([]string{"scope"})})
	f.ev.script(f.wt, f.target.Name, found([]string{f.target.Name}))
	f.ev.script(f.wt, f.other.Name, candidate...)
	return f
}

// A guard's baseline is lazy: the candidate replays first, and one that
// finds every defect the newest stored replay found passes with that replay
// as its baseline, whatever its inputs, and no replay of the base; a loss
// is replayed again, and only a second loss replays the base, whose score
// decides: a defect the base finds and the candidate lost twice is a
// regression. When the newest stored replay is the base's own, it is the
// base's score and nothing of the base is replayed.
func TestTheGateReplaysAGuardsBaseOnlyWhenTheCandidateLostItTwice(t *testing.T) {
	kept := guardFixture(t, found([]string{"scope"}))
	kept.h.tick()
	if p := kept.proposal(); p.State != store.LearnPassed {
		t.Fatalf("a guard the candidate kept: proposal %s %q, want passed", p.State, p.Reason)
	}
	if got := kept.replaysByKind(); got["baseline"] != 0 || got["candidate"] != 2 || got["second"] != 1 {
		t.Fatalf("a guard the candidate kept: replays %v, want 2 candidate (the target, the guard), 1 second (the target), no baseline", got)
	}
	if evs := kept.h.events("learn.gate_start"); len(evs) != 1 || !strings.Contains(kept.h.eventData("learn.gate_start"), `"guards":1`) {
		t.Fatalf("gate start: %+v %s, want one guard counted", evs, kept.h.eventData("learn.gate_start"))
	}
	c := kept.gate().Cases[1]
	if c.Case != kept.other.Name || !c.Guard || c.Target || !c.Stored || c.BaselineRun != "20261002-120000" || c.Before == nil ||
		c.Before.Found != 1 || c.After == nil || c.After.Found != 1 || !strings.Contains(c.Note, "in-old") || !strings.Contains(c.Note, "20261002-120000") {
		t.Fatalf("the guard's row = %+v, want the stored replay of 20261002-120000 as its baseline, its inputs named", c)
	}

	once := guardFixture(t, found(nil, "scope"), found([]string{"scope"}))
	once.h.tick()
	if p := once.proposal(); p.State != store.LearnPassed {
		t.Fatalf("a guard lost once: proposal %s %q, want passed", p.State, p.Reason)
	}
	if got := once.replaysByKind(); got["baseline"] != 0 || got["candidate"] != 2 || got["second"] != 2 {
		t.Fatalf("a guard lost once: replays %v, want its second replay and no baseline", got)
	}
	if c := once.gate().Cases[1]; !c.Retried || !c.Stored || c.After.Found != 1 {
		t.Fatalf("a guard lost once: row %+v", c)
	}

	twice := guardFixture(t, found(nil, "scope"))
	twice.ev.script(twice.base, twice.other.Name, found([]string{"scope"}))
	twice.h.tick()
	p := twice.proposal()
	if p.State != store.LearnFailed || !strings.Contains(p.Reason, twice.other.Name+" lost scope after a second replay") {
		t.Fatalf("a guard lost twice that the base finds: proposal %s %q, want failed on it", p.State, p.Reason)
	}
	// A regression fails it whatever it improved: no second replay confirms
	// the target's win.
	if got := twice.replaysByKind(); got["baseline"] != 1 || got["candidate"] != 2 || got["second"] != 1 {
		t.Fatalf("a guard lost twice: replays %v, want 1 baseline (the guard), 2 candidate, 1 second (the guard)", got)
	}
	g := twice.gate()
	if c := g.Cases[1]; c.Stored || !c.Retried || c.BaselineRun == "20261002-120000" || c.BaselineRun == "" || c.Before.Found != 1 ||
		c.Skip != "" || g.Cases[0].SecondRun != "" {
		t.Fatalf("a guard lost twice: rows %+v, want the base's replay as the guard's baseline", g.Cases)
	}

	// The newest stored replay is the base's own: it is the base's score.
	own := newGateFixture(t, gateFullFiles)
	own.baseline("20261001-120000", "rev-master", map[string]*eval.Score{own.target.Name: found(nil, own.target.Name)})
	own.baseline("20261002-120000", "rev-master", map[string]*eval.Score{own.other.Name: found([]string{"scope"})})
	own.ev.script(own.wt, own.target.Name, found([]string{own.target.Name}))
	own.ev.script(own.wt, own.other.Name, found(nil, "scope"))
	own.h.tick()
	if p := own.proposal(); p.State != store.LearnFailed || len(own.ev.callsFor(own.base)) != 0 {
		t.Fatalf("a guard lost twice whose newest replay is the base's: proposal %s %q, base replays %+v; want failed, none",
			p.State, p.Reason, own.ev.callsFor(own.base))
	}
}

// eventData is the data of the newest event of kind.
func (h *harness) eventData(kind string) string {
	h.t.Helper()
	var data string
	if err := h.st.DB().QueryRowContext(h.ctx, "SELECT coalesce(data_json, '') FROM events WHERE kind = ? ORDER BY id DESC LIMIT 1", kind).Scan(&data); err != nil {
		h.t.Fatal(err)
	}
	return data
}

// A guard the candidate lost twice that the base loses too is not the
// proposal's loss: the row is skipped, the proposal is judged on its
// target, and one warning (the case, the run that last found it, the base's
// inputs) and one toast say that the review at master no longer finds it,
// naming the case and nothing of its pull request.
func TestAGuardLostAtTheBaseWritesOneEventAndToast(t *testing.T) {
	f := guardFixture(t, found(nil, "scope"))
	f.ev.script(f.base, f.other.Name, found(nil, "scope"))
	f.h.tick()
	p := f.proposal()
	if p.State != store.LearnPassed || !strings.Contains(p.Reason, f.target.Name+" found") {
		t.Fatalf("proposal = %s %q, want passed on its target", p.State, p.Reason)
	}
	if c := f.gate().Cases[1]; c.Skip != learn.SkipLostAtBase || c.Stored || !strings.Contains(c.Note, "20261002-120000") {
		t.Fatalf("the guard's row = %+v, want skipped as lost at the base too", c)
	}
	evs := f.h.events("learn.guard_lost_at_base")
	if len(evs) != 1 || evs[0].Level != "warn" || !strings.Contains(evs[0].Message, f.other.Name) ||
		strings.Contains(evs[0].Message, f.other.PR) {
		t.Fatalf("events = %+v, want one warning naming the case alone", evs)
	}
	var data struct {
		Proposal int64  `json:"proposal"`
		Case     string `json:"case"`
		Run      string `json:"run"`
		Inputs   string `json:"inputs"`
	}
	if err := json.Unmarshal([]byte(f.h.eventData("learn.guard_lost_at_base")), &data); err != nil || data.Proposal != p.ID ||
		data.Case != f.other.Name || data.Run != "20261002-120000" || data.Inputs != "in-master" {
		t.Fatalf("event data = %+v, %v; want the case, the run that last found it and the base's inputs", data, err)
	}
	toasts := f.toasts()
	if strings.Count(toasts, f.other.Name+" is lost at master") != 1 || strings.Contains(toasts, f.other.PR) {
		t.Fatalf("toasts = %q, want one naming %s alone", toasts, f.other.Name)
	}
	f.h.advance(time.Hour)
	f.h.tick()
	if evs := f.h.events("learn.guard_lost_at_base"); len(evs) != 1 {
		t.Fatalf("events after another tick = %+v", evs)
	}
}

// A miss target the candidate's first replay found is replayed once more:
// a second replay that misses it leaves the target not improved (the
// proposal fails and the input's gate says so, so two such gates dismiss
// it), and a second replay an earlier gate that stopped short made is
// reused, not replayed again.
func TestAWonTargetIsReplayedAgainAndAStoredSecondReplayIsReused(t *testing.T) {
	f := newGateFixture(t, gateFullFiles)
	f.baseline("20261001-120000", "rev-master", map[string]*eval.Score{f.target.Name: found(nil, f.target.Name)})
	f.ev.script(f.wt, f.target.Name, found([]string{f.target.Name}), found(nil, f.target.Name))
	f.ev.script(f.base, f.other.Name, found(nil, "scope"))
	f.ev.script(f.wt, f.other.Name, found(nil, "scope"))
	f.h.tick()
	p := f.proposal()
	if p.State != store.LearnFailed || !strings.Contains(p.Reason, "no target improved ("+f.target.Name+" found by the first replay only)") {
		t.Fatalf("proposal = %s %q, want failed: the win was not found again", p.State, p.Reason)
	}
	c := f.gate().Cases[0]
	if c.Confirmed || c.SecondRun == "" || c.After == nil || c.After.Found != 1 || !strings.Contains(c.Note, c.SecondRun) {
		t.Fatalf("the target's row = %+v, want its second replay recorded, not confirmed", c)
	}
	ins, err := f.h.st.LearnProposalInputs(f.h.ctx, p.ID)
	if err != nil || len(ins) != 1 || ins[0].Gate != store.LearnTargetNotImproved || ins[0].State != store.MissNew {
		t.Fatalf("inputs = %+v, %v; want the target not improved", ins, err)
	}

	// The second replay a gate made before its verdict failed to be stored
	// is the next gate's.
	f2 := newGateFixture(t, gateFullFiles)
	f2.baseline("20261001-120000", "rev-master", map[string]*eval.Score{f2.target.Name: found(nil, f2.target.Name)})
	f2.ev.script(f2.wt, f2.target.Name, found([]string{f2.target.Name}))
	f2.ev.script(f2.base, f2.other.Name, found(nil, "scope"))
	f2.ev.script(f2.wt, f2.other.Name, found(nil, "scope"))
	if _, err := f2.h.st.DB().ExecContext(f2.h.ctx, `CREATE TRIGGER fail_decide BEFORE UPDATE OF state ON learn_proposals
BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	f2.h.tick()
	if p := f2.proposal(); p.State != store.LearnGating || len(f2.ev.callsFor(f2.wt)) != 3 {
		t.Fatalf("proposal %s after %d candidate replays, want gating after 3", p.State, len(f2.ev.callsFor(f2.wt)))
	}
	if _, err := f2.h.st.DB().ExecContext(f2.h.ctx, "DROP TRIGGER fail_decide"); err != nil {
		t.Fatal(err)
	}
	f2.h.advance(gateRetryAfter + time.Minute)
	f2.h.tick()
	if p := f2.proposal(); p.State != store.LearnPassed || len(f2.ev.callsFor(f2.wt)) != 3 {
		t.Fatalf("proposal %s %q after %d candidate replays, want passed on the stored replays", p.State, p.Reason, len(f2.ev.callsFor(f2.wt)))
	}
	if c := f2.gate().Cases[0]; !c.Confirmed || c.SecondRun == "" || c.SecondRun == c.CandidateRun {
		t.Fatalf("the target's row = %+v, want confirmed by the stored second replay", c)
	}
}
