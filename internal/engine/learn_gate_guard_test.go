package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/store"
)

// addCase adds an operator's case of talkable/talkable#n with one defect,
// d, to the fixture's corpus.
func (f *gateFixture) addCase(name string, n int) eval.Case {
	f.h.t.Helper()
	c := eval.Case{Name: name, PR: fmt.Sprintf("talkable/talkable#%d", n), Head: retroShaA,
		Defects: []eval.Defect{{ID: "d", Title: "the defect", Paths: []string{"app/models/" + name + ".rb"}}}}
	f.ev.mu.Lock()
	f.ev.corpus.Cases = append(f.ev.corpus.Cases, c)
	f.ev.mu.Unlock()
	return c
}

// storedRunAt stores a scored full replay of cases started at with inputs
// and reviewer inputs, as an earlier `magnum eval run` would.
func (f *gateFixture) storedRunAt(id, inputs, reviewer string, at time.Time, cases ...scored) {
	f.h.t.Helper()
	r := eval.Run{ID: id, Inputs: inputs, ReviewerInputs: reviewer, Commit: "abc1234", Started: at}
	for _, cs := range cases {
		cs.s.Case = cs.c.Name
		r.Cases = append(r.Cases, eval.CaseRun{Case: cs.c.Name, PR: cs.c.PR, Head: cs.c.Head, Outcome: "dry_run", Score: cs.s,
			ReportDir: filepath.Join(f.h.layout.Eval(), id, "cases", cs.c.Name)})
	}
	if err := eval.SaveRun(filepath.Join(f.h.layout.Eval(), id), r); err != nil {
		f.h.t.Fatal(err)
	}
}

// nextProposal makes another gating proposal like the fixture's, named id,
// that notes the fixture's miss too, with a worktree, a branch and inputs
// (in) of its own, and points the fixture's wt and p at it.
func (f *gateFixture) nextProposal(id, in string) {
	f.h.t.Helper()
	wt := filepath.Join(f.h.layout.LearnWorktrees(), id)
	if err := os.MkdirAll(wt, 0o700); err != nil {
		f.h.t.Fatal(err)
	}
	f.git.set(wt, "HEAD", learnCandidate)
	f.git.mu.Lock()
	f.git.worktree[wt] = [2]string{f.checkout, "learn/" + id}
	f.git.mu.Unlock()
	f.git.set(f.checkout, "refs/heads/learn/"+id, learnCandidate)
	f.ev.mu.Lock()
	f.ev.inputs[wt] = [2]string{in, f.ev.inputs[f.wt][1]}
	f.ev.mu.Unlock()
	p, err := f.h.st.CreateLearnProposal(f.h.ctx, store.LearnProposalInput{Trigger: store.LearnTriggerRequest, BaseSHA: learnMaster,
		Branch: "learn/" + id, CandidateSHA: learnCandidate, Files: f.p.Files, State: store.LearnGating, Model: "sonnet",
		Scratch: wt, RunDir: filepath.Join(f.h.layout.LearnSkill(), id),
		Inputs: []store.LearnInput{{Kind: store.LearnInputMiss, InputID: f.miss.ID, Outcome: store.MissNoted, Where: "SKILL.md section 4"}}})
	if err != nil {
		f.h.t.Fatal(err)
	}
	f.wt, f.p = wt, p
}

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
// newest stored replay, the operator's run 20261002-120000 with other inputs
// (eval-at.sh of an older master, say), found its defect, and the current
// master's own replays never did. The target has a
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

// A gate that regressed fails whatever its targets did, so it makes no
// second replay of a miss target its candidate's first replay found: that
// win is recorded neither way, never as not improved, so two such failed
// gates do not dismiss the input as "noted in 2 failed gates" (no second
// replay showed the candidate does not find it).
func TestAWonTargetInAGateThatRegressedElsewhereCountsNeitherWay(t *testing.T) {
	f := guardFixture(t, found(nil, "scope"))
	f.ev.script(f.base, f.other.Name, found([]string{"scope"}))
	for gate := 1; gate <= 2; gate++ {
		if gate == 2 {
			f.h.advance(time.Hour)
			f.nextProposal("20261005-120000", "in-cand-2")
			f.ev.script(f.wt, f.target.Name, found([]string{f.target.Name}))
			f.ev.script(f.wt, f.other.Name, found(nil, "scope"))
		}
		f.h.tick()
		p := f.proposal()
		if p.State != store.LearnFailed || !strings.Contains(p.Reason, f.other.Name+" lost scope after a second replay") {
			t.Fatalf("gate %d: proposal %s %q, want failed on the guard", gate, p.State, p.Reason)
		}
		if c := f.gate().Cases[0]; c.Case != f.target.Name || !c.WonOnce() || c.Rechecked || c.SecondRun != "" {
			t.Fatalf("gate %d: the target's row = %+v, want a first win no second replay checked", gate, c)
		}
		ins, err := f.h.st.LearnProposalInputs(f.h.ctx, p.ID)
		if err != nil || len(ins) != 1 || ins[0].Gate != "" || ins[0].State != store.MissNew {
			t.Fatalf("gate %d: inputs = %+v, %v; want the won target recorded neither way, its input new", gate, ins, err)
		}
	}
	if m := f.missNow(); m.State != store.MissNew || m.Dismissed != "" {
		t.Fatalf("the miss after two failed gates = %s %q, want new", m.State, m.Dismissed)
	}
}

// Every gate of one proposal takes the same guards with the same
// baselines: a gate that stopped short after a guard was lost at the base
// too resumes with that guard and the replay its first gate took as its
// baseline, whatever was replayed of it since (its own replay of the base,
// an improve run's replay that found nothing), and takes the base's score
// from the replay of the base that gate made, not from a new one; the
// warning and the toast are written once.
func TestAResumedGateKeepsItsGuardsAndTheBaseReplayItMade(t *testing.T) {
	f := guardFixture(t, found(nil, "scope"))
	f.ev.script(f.base, f.other.Name, found(nil, "scope"))
	var once sync.Once
	f.ev.after = func(c evalCall) { // the target's second replay meets the budget
		if c.Case == f.target.Name && c.Opts.Candidate == f.wt {
			once.Do(func() { f.ev.fail(f.wt, f.target.Name, eval.OutcomeBudget) })
		}
	}
	f.h.tick()
	if p := f.proposal(); p.State != store.LearnGating || len(f.h.events("learn.guard_lost_at_base")) != 1 {
		t.Fatalf("proposal %s, events %+v; want gating after one guard lost at the base", p.State, f.h.events("learn.guard_lost_at_base"))
	}
	// An improve run replays the guard meanwhile and finds nothing.
	f.storedRunAt("20261005-110000", "in-x", "rev-x", f.h.clock.Now().Add(30*time.Minute), scored{f.other, found(nil, "scope")})
	f.ev.fail(f.wt, f.target.Name, "")
	f.h.advance(gateRetryAfter + time.Minute)
	f.h.tick()
	p := f.proposal()
	if p.State != store.LearnPassed || !strings.Contains(p.Reason, f.target.Name+" found") {
		t.Fatalf("proposal = %s %q, want passed on its target", p.State, p.Reason)
	}
	c := f.gate().Cases[1]
	if c.Case != f.other.Name || !c.Guard || c.Skip != learn.SkipLostAtBase || !strings.Contains(c.Note, "20261002-120000") {
		t.Fatalf("the resumed gate's guard row = %+v, want coupon-scope guarding as before, lost at the base too", c)
	}
	if base := f.ev.callsFor(f.base); len(base) != 1 {
		t.Fatalf("base replays = %+v, want the first gate's alone", base)
	}
	if evs := f.h.events("learn.guard_lost_at_base"); len(evs) != 1 {
		t.Fatalf("events = %+v, want one", evs)
	}
	if toasts := f.toasts(); strings.Count(toasts, f.other.Name+" is lost at master") != 1 {
		t.Fatalf("toasts = %q, want one for %s", toasts, f.other.Name)
	}
}

// A gate's replays of its candidate never make a guard of a later gate, nor
// its baseline: proposal 1's candidate found coupon-scope's defect, which
// master's replays never did, and proposal 1 failed, so its skill never
// landed. Proposal 2, at master's skill, does not guard coupon-scope on that
// replay (it would lose it twice, replay the base, and warn that master
// lost it); it compares coupon-scope with master's stored replay as any
// other case. Where master's replay found the defect too, coupon-scope
// guards on master's replay, not on proposal 1's newer one.
func TestAnotherProposalsCandidateReplayIsNoGuardOfTheNextGate(t *testing.T) {
	for _, masterFinds := range []bool{false, true} {
		f := newGateFixture(t, gateFullFiles)
		other := found(nil, "scope")
		if masterFinds {
			other = found([]string{"scope"})
		}
		f.baseline("20261001-120000", "rev-master", map[string]*eval.Score{f.target.Name: found(nil, f.target.Name), f.other.Name: other})
		f.ev.script(f.wt, f.target.Name, found(nil, f.target.Name))
		f.ev.script(f.wt, f.other.Name, found([]string{"scope"}))
		f.h.tick()
		if p := f.proposal(); p.State != store.LearnFailed || !strings.Contains(p.Reason, "no target improved") {
			t.Fatalf("master finds it %v: proposal 1 = %s %q, want failed: its target did not improve", masterFinds, p.State, p.Reason)
		}
		f.h.advance(time.Hour)
		f.nextProposal("20261005-120000", "in-cand-2")
		f.ev.script(f.wt, f.target.Name, found([]string{f.target.Name}))
		f.ev.script(f.wt, f.other.Name, other)
		f.ev.script(f.base, f.other.Name, found(nil, "scope"))
		f.h.tick()
		if p := f.proposal(); p.State != store.LearnPassed {
			t.Fatalf("master finds it %v: proposal 2 = %s %q, want passed", masterFinds, p.State, p.Reason)
		}
		c := f.gate().Cases[1]
		if c.Case != f.other.Name || c.Guard != masterFinds || c.Stored != masterFinds || c.BaselineRun != "20261001-120000" || c.Skip != "" {
			t.Fatalf("master finds it %v: proposal 2's coupon-scope row = %+v, want guard %v on master's stored replay", masterFinds, c, masterFinds)
		}
		if evs, base := f.h.events("learn.guard_lost_at_base"), f.ev.callsFor(f.base); len(evs) != 0 || len(base) != 0 {
			t.Fatalf("master finds it %v: guard warnings %+v, base replays %+v; want none", masterFinds, evs, base)
		}
	}
}

// A judge-only gate's guards are cases with a full replay whose reviewers
// read the candidate's prompts, made before the proposal: the gate makes
// none for a guard, so a case without one takes no place, and the next
// least recently replayed case guards instead. Each guard's judges read
// those reports: one the candidate's judge keeps passes on its stored
// replay, and one it lost twice takes the score of the base's judge alone
// on the same reports, a regression when that one finds it and skipped as
// lost at the base too when it does not.
func TestAJudgeOnlyGateGuardsCasesWithAFullReplayOfTheCandidatesReviewers(t *testing.T) {
	f := newGateFixture(t, []string{learn.DecisionsFile, learn.SkillFile}, func(h *harness) { h.cfg.Learn.GateGuards, h.cfg.Learn.GateCases = 3, 1 })
	nofull, kept, regress, atBase := f.addCase("no-full", 31), f.addCase("kept", 32), f.addCase("regress", 33), f.addCase("at-base", 34)
	day := func(d int) time.Time { return time.Date(2026, 10, d, 9, 0, 0, 0, time.Local) }
	f.storedRunAt("20261001-090000", "in-master", "rev-master", day(1), scored{f.target, found(nil, f.target.Name)})
	f.storedRunAt("20261001-100000", "in-old", "rev-old", day(1).Add(time.Hour), scored{nofull, found([]string{"d"})})
	f.storedRunAt("20261002-090000", "in-other", "rev-master", day(2), scored{kept, found([]string{"d"})})
	f.storedRunAt("20261003-090000", "in-other", "rev-master", day(3), scored{regress, found([]string{"d"})})
	f.storedRunAt("20261004-090000", "in-other", "rev-master", day(4), scored{atBase, found([]string{"d"})})
	f.ev.script(f.wt, f.target.Name, found([]string{f.target.Name}))
	f.ev.script(f.wt, kept.Name, found([]string{"d"}))
	f.ev.script(f.wt, regress.Name, found(nil, "d"))
	f.ev.script(f.wt, atBase.Name, found(nil, "d"))
	f.ev.script(f.base, regress.Name, found([]string{"d"}))
	f.ev.script(f.base, atBase.Name, found(nil, "d"))
	f.h.tick()

	p := f.proposal()
	if p.State != store.LearnFailed || !strings.Contains(p.Reason, regress.Name+" lost d after a second replay") {
		t.Fatalf("proposal = %s %q, want failed on %s", p.State, p.Reason, regress.Name)
	}
	g := f.gate()
	var guards []string
	for _, c := range g.Cases {
		if c.Guard {
			guards = append(guards, c.Case)
		}
	}
	if !slices.Equal(guards, []string{kept.Name, regress.Name, atBase.Name}) {
		t.Fatalf("guards = %q, want kept, regress, at-base: no-full has no full replay of the candidate's reviewers", guards)
	}
	rows := map[string]GateCase{}
	for _, c := range g.Cases {
		rows[c.Case] = c
	}
	if c := rows[kept.Name]; !c.Stored || c.Skip != "" || c.BaselineRun != "20261002-090000" || c.ReportsRun != "20261002-090000" || c.After.Found != 1 {
		t.Fatalf("the kept guard's row = %+v, want its stored replay as its baseline, on its reports", c)
	}
	if c := rows[regress.Name]; c.Stored || !c.Retried || c.Skip != "" || c.Before.Found != 1 || c.BaselineRun == "20261003-090000" ||
		c.ReportsRun != "20261003-090000" {
		t.Fatalf("the regressed guard's row = %+v, want the base's judge on its reports as its baseline", c)
	}
	if c := rows[atBase.Name]; c.Skip != learn.SkipLostAtBase || c.ReportsRun != "20261004-090000" {
		t.Fatalf("the guard lost at the base = %+v", c)
	}
	cand, base := f.ev.callsFor(f.wt), f.ev.callsFor(f.base)
	if len(cand) != 6 || len(base) != 2 || slices.ContainsFunc(append(cand, base...), func(c evalCall) bool { return !c.Opts.JudgeOnly }) {
		t.Fatalf("replays: candidate %+v, base %+v; want 6 and 2, the judge alone", cand, base)
	}
	if evs := f.h.events("learn.guard_lost_at_base"); len(evs) != 1 || !strings.Contains(evs[0].Message, atBase.Name) {
		t.Fatalf("events = %+v, want one for %s", evs, atBase.Name)
	}
}
