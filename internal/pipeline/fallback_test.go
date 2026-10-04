package pipeline

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// Pane texts of Claude Code when one model's own cap is hit while the account
// windows (5h, 7d) still have room.
const (
	fableLimitPane = "  ⎿  You've reached your Fable limit. /model to switch models.\n" +
		"✻ Crunched for 6m 57s · done 12:29 PM\n" +
		"  Fable 5.1 | pr-23@HEAD | 52k/1m (5%) | effort: xhigh | 5h 20% @16:40 | 7d 78% @Fri Oct 9, 15:00\n"
	opusLimitPane = "  ⎿  You've hit your Opus limit · resets 3:45pm\n" +
		"  Opus 4.5 | pr-23@HEAD | 61k/1m (6%) | effort: high | 5h 24% @16:40 | 7d 79% @Fri Oct 9, 15:00\n"
	sonnetLimitPane = "  ⎿  You've reached your Sonnet limit. /model to switch models.\n" +
		"  Sonnet 4.5 | pr-23@HEAD | 70k/1m (7%) | effort: high | 5h 27% @16:40 | 7d 80% @Fri Oct 9, 15:00\n"
)

// hitsLimit is a turn that ends with nothing written and pane on screen. The
// pane keeps what earlier turns left (limit text of the model before the
// switch included) and shows this prompt's anchors (the run id, the report
// path, the head) before pane, as the real pane would, so the classifier has
// to read only the text after the last prompt.
func hitsLimit(pane string) behavior {
	return func(f *fakeAgents, run store.Run, text string) error {
		anchors := ""
		if m := runIDLine.FindStringSubmatch(text); m != nil {
			anchors += "run_id: " + m[1] + "\n"
		}
		if p := store.Deref(run.ReportPath); p != "" && strings.Contains(text, p) {
			anchors += p + "\n"
		}
		if strings.Contains(text, target) {
			anchors += target + "\n"
		}
		role := agents.Role(run.Role)
		f.mu.Lock()
		f.reads[role] += "❯ " + anchors + pane
		f.mu.Unlock()
		return f.end(run.ID)
	}
}

// runsOf returns the runs of role and kind in creation order.
func (e *env) runsOf(role agents.Role, kind string) []store.Run {
	e.t.Helper()
	var out []store.Run
	for _, r := range e.runs() {
		if r.Role == string(role) && r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// eventsOfKind returns the round's events of a kind.
func (e *env) eventsOfKind(kind string) []store.Event {
	e.t.Helper()
	var out []store.Event
	for _, ev := range e.events() {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func (e *env) kv(key string) (string, bool) {
	e.t.Helper()
	v, ok, err := e.st.GetKV(e.ctx, key)
	if err != nil {
		e.t.Fatalf("GetKV %s: %v", key, err)
	}
	return v, ok
}

// switchModels are the models SwitchModel was called with, in order.
func (e *env) switchModels() []string {
	e.ag.mu.Lock()
	defer e.ag.mu.Unlock()
	var out []string
	for _, s := range e.ag.switches {
		out = append(out, s.Model)
	}
	return out
}

// wantRun checks a run's final state and outcome.
func wantRun(t *testing.T, r store.Run, state, outcome string) {
	t.Helper()
	if r.State != state || store.Deref(r.Outcome) != outcome {
		t.Errorf("run %s (%s %s) = %s / %q, want %s / %q", r.ID, r.Role, r.Kind, r.State, store.Deref(r.Outcome), state, outcome)
	}
}

// A reviewer whose model hit its own cap while the account still has usage
// continues on the next fallback model: one switch, a "continue" run with the
// model-fallback prompt, no pause, and the cap remembered for new sessions.
func TestReviewerModelLimitContinuesOnFallbackModel(t *testing.T) {
	e := newEnv(t)
	var limitAt time.Time
	hit := hitsLimit(fableLimitPane)
	e.ag.behaviors[agents.RoleClaude] = []behavior{
		func(f *fakeAgents, run store.Run, text string) error {
			limitAt = e.clock.Now()
			return hit(f, run, text)
		},
		writeReport("## P2 something\n"),
	}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(701, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.ReviewID != 701 || res.Pause != nil {
		t.Fatalf("a model limit with a fallback must not stop or pause anything: %+v", res)
	}

	e.ag.mu.Lock()
	switches := slices.Clone(e.ag.switches)
	e.ag.mu.Unlock()
	want := []switchCall{{Role: string(agents.RoleClaude), Model: "opus", Reason: agents.SwitchLimitHit}}
	if !slices.Equal(switches, want) {
		t.Fatalf("switches = %+v, want %+v", switches, want)
	}

	first := e.runOf(agents.RoleClaude, store.RunInitial)
	cont := e.runOf(agents.RoleClaude, store.RunContinue)
	wantRun(t, first, store.RunFailed, string(agents.HealthModelLimit))
	wantRun(t, cont, store.RunVerified, ReportOK)
	if cont.Round != first.Round {
		t.Errorf("continuation round %d != %d", cont.Round, first.Round)
	}
	rep := res.Reports[agents.RoleClaude]
	if rep.Status != ReportOK || rep.Path == "" || rep.RunID != cont.ID {
		t.Errorf("claude report = %+v, want ok from run %s", rep, cont.ID)
	}

	submits := e.ag.submitsFor(agents.RoleClaude)
	if len(submits) != 2 {
		t.Fatalf("claude submits = %d, want the review prompt and one continuation", len(submits))
	}
	mustContain(t, "continuation prompt", submits[1].Text, "opus", store.Deref(first.ReportPath), target)

	until, ok := e.kv(agents.KVModelLimited("claude", "fable"))
	if !ok {
		t.Fatal("the Fable limit was not recorded")
	}
	ut, err := store.ParseTime(until)
	if err != nil {
		t.Fatalf("limit end %q: %v", until, err)
	}
	if d := ut.Sub(limitAt); d < 5*time.Hour || d > 5*time.Hour+5*time.Minute {
		t.Errorf("the text names no reset, so the limit lasts the default 5h cooldown from %s; ends %s (%s)", limitAt, ut, d)
	}
	if v, _ := e.kv(agents.KVSessionModel(e.sess[agents.RoleClaude].ID)); v != "opus" {
		t.Errorf("session model = %q, want opus", v)
	}

	evs := e.eventsOfKind("round.model_fallback")
	if len(evs) != 1 {
		t.Fatalf("round.model_fallback events = %d, want 1", len(evs))
	}
	mustContain(t, "fallback event", evs[0].Message, "claude-review", "fable is limited", "switched to opus")
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "claude-review")
}

// The fallback model can hit its own cap too: the session moves on to the next
// fallback, one switch per model, and only the limit text after the newest
// prompt counts.
func TestReviewerSecondModelLimitMovesOnToNextFallback(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleClaude] = []behavior{
		hitsLimit(fableLimitPane), hitsLimit(opusLimitPane), writeReport("## P2 something\n"),
	}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(702, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.Pause != nil {
		t.Fatalf("result = %+v", res)
	}
	if got := e.switchModels(); !slices.Equal(got, []string{"opus", "sonnet"}) {
		t.Fatalf("switches = %v, want opus then sonnet", got)
	}
	e.ag.mu.Lock()
	for _, s := range e.ag.switches {
		if s.Role != string(agents.RoleClaude) || s.Reason != agents.SwitchLimitHit {
			t.Errorf("switch = %+v", s)
		}
	}
	e.ag.mu.Unlock()

	first := e.runOf(agents.RoleClaude, store.RunInitial)
	conts := e.runsOf(agents.RoleClaude, store.RunContinue)
	if len(conts) != 2 {
		t.Fatalf("continuation runs = %d, want 2", len(conts))
	}
	wantRun(t, first, store.RunFailed, string(agents.HealthModelLimit))
	wantRun(t, conts[0], store.RunFailed, string(agents.HealthModelLimit))
	wantRun(t, conts[1], store.RunVerified, ReportOK)
	if rep := res.Reports[agents.RoleClaude]; rep.Status != ReportOK || rep.RunID != conts[1].ID {
		t.Errorf("claude report = %+v, want ok from run %s", rep, conts[1].ID)
	}

	submits := e.ag.submitsFor(agents.RoleClaude)
	if len(submits) != 3 {
		t.Fatalf("claude submits = %d, want 3", len(submits))
	}
	mustContain(t, "first continuation", submits[1].Text, "opus")
	mustContain(t, "second continuation", submits[2].Text, "sonnet", store.Deref(first.ReportPath))

	if _, ok := e.kv(agents.KVModelLimited("claude", "fable")); !ok {
		t.Error("the Fable limit was not recorded")
	}
	until, ok := e.kv(agents.KVModelLimited("claude", "opus"))
	if !ok {
		t.Fatal("the Opus limit was not recorded")
	}
	// "resets 3:45pm" names the end; the test clock runs in UTC.
	if ut, err := store.ParseTime(until); err != nil || !ut.Equal(time.Date(2026, 10, 3, 15, 45, 0, 0, time.UTC)) {
		t.Errorf("Opus limit ends %q (%v), want 2026-10-03 15:45 UTC", until, err)
	}
}

// With every fallback limited or used up the limit is the usage limit it was
// before per-model limits existed: the report says so (the kind pauses), and
// no switch is made beyond the fallbacks the kind has.
func TestReviewerModelLimitWithNoFallbackLeftIsAUsageLimit(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleClaude] = []behavior{
		hitsLimit(fableLimitPane), hitsLimit(opusLimitPane), hitsLimit(sonnetLimitPane),
	}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(703, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if got := e.switchModels(); !slices.Equal(got, []string{"opus", "sonnet"}) {
		t.Fatalf("switches = %v, want opus then sonnet and no third", got)
	}
	rep := res.Reports[agents.RoleClaude]
	if rep.Status != string(agents.HealthUsageLimit) || rep.Health == nil || rep.Health.Kind != agents.HealthUsageLimit {
		t.Fatalf("claude report = %+v (health %+v), want usage_limit", rep, rep.Health)
	}
	if rep.Path != "" {
		t.Errorf("a limited turn left no report, got path %q", rep.Path)
	}
	if res.Outcome != OutcomePosted {
		t.Errorf("a reviewer's limit must not stop the round: %+v", res)
	}

	first := e.runOf(agents.RoleClaude, store.RunInitial)
	conts := e.runsOf(agents.RoleClaude, store.RunContinue)
	if len(conts) != 2 {
		t.Fatalf("continuation runs = %d, want 2", len(conts))
	}
	wantRun(t, first, store.RunFailed, string(agents.HealthModelLimit))
	wantRun(t, conts[0], store.RunFailed, string(agents.HealthModelLimit))
	wantRun(t, conts[1], store.RunFailed, string(agents.HealthUsageLimit))
	if rep.RunID != conts[1].ID {
		t.Errorf("report run = %s, want the last continuation %s", rep.RunID, conts[1].ID)
	}
	if _, ok := e.kv(agents.KVModelLimited("claude", "sonnet")); !ok {
		t.Error("the Sonnet limit was not recorded")
	}
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "claude-review: missing (usage_limit)")
}

// A kind without switch_model cannot move to another model in-session: its
// model limit pauses it as a usage limit, with no switch and no continuation.
func TestReviewerModelLimitOnKindWithoutSwitchCommandIsAUsageLimit(t *testing.T) {
	e := newEnv(t)
	k := e.cfg.Kinds["claude"]
	k.SwitchModel, k.FallbackModels = "", nil
	e.cfg.Kinds["claude"] = k
	e.ag.behaviors[agents.RoleClaude] = []behavior{hitsLimit(fableLimitPane)}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(704, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if n := len(e.switchModels()); n != 0 {
		t.Errorf("SwitchModel calls = %d, want none for a kind without switch_model", n)
	}
	if runs := e.runsOf(agents.RoleClaude, store.RunContinue); len(runs) != 0 {
		t.Errorf("continuation runs = %+v, want none", runs)
	}
	if n := len(e.ag.submitsFor(agents.RoleClaude)); n != 1 {
		t.Errorf("claude submits = %d, want only the review prompt", n)
	}
	rep := res.Reports[agents.RoleClaude]
	if rep.Status != string(agents.HealthUsageLimit) || rep.Health == nil || rep.Health.Kind != agents.HealthUsageLimit {
		t.Fatalf("claude report = %+v (health %+v), want usage_limit", rep, rep.Health)
	}
	wantRun(t, e.runOf(agents.RoleClaude, store.RunInitial), store.RunFailed, string(agents.HealthUsageLimit))
	if evs := e.eventsOfKind("round.model_fallback"); len(evs) != 0 {
		t.Errorf("round.model_fallback events = %d, want none", len(evs))
	}
}

// A switch that fails (the agent is busy, the pane is gone) leaves the limit
// as a usage limit: no continuation, and the failure is a warning that names
// the model.
func TestReviewerModelSwitchFailureFallsBackToUsageLimit(t *testing.T) {
	e := newEnv(t)
	e.ag.switchErr = errors.New("agents: switch claude-review to opus: agent is \"working\": agent is busy")
	e.ag.behaviors[agents.RoleClaude] = []behavior{hitsLimit(fableLimitPane)}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(705, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if got := e.switchModels(); !slices.Equal(got, []string{"opus"}) {
		t.Fatalf("switches = %v, want the one failed attempt on opus", got)
	}
	if runs := e.runsOf(agents.RoleClaude, store.RunContinue); len(runs) != 0 {
		t.Errorf("continuation runs = %+v, want none after a failed switch", runs)
	}
	if n := len(e.ag.submitsFor(agents.RoleClaude)); n != 1 {
		t.Errorf("claude submits = %d, want only the review prompt", n)
	}
	rep := res.Reports[agents.RoleClaude]
	if rep.Status != string(agents.HealthUsageLimit) {
		t.Fatalf("claude report = %+v, want usage_limit", rep)
	}
	wantRun(t, e.runOf(agents.RoleClaude, store.RunInitial), store.RunFailed, string(agents.HealthUsageLimit))

	var warned bool
	for _, ev := range e.eventsOfKind("round.warning") {
		if strings.Contains(ev.Message, "claude-review") && strings.Contains(ev.Message, "switch to opus") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no round.warning names the failed switch to opus; events: %+v", e.eventsOfKind("round.warning"))
	}
	if evs := e.eventsOfKind("round.model_fallback"); len(evs) != 0 {
		t.Errorf("round.model_fallback events = %d, want none", len(evs))
	}
	if v, ok := e.kv(agents.KVSessionModel(e.sess[agents.RoleClaude].ID)); ok {
		t.Errorf("session model = %q after a failed switch, want unset", v)
	}
}

// The built-in judge is a Codex session, which cannot switch models: its
// model limit is the usage limit that pauses codex, without a switch or a
// nudge.
func TestJudgeModelLimitOnKindWithoutSwitchCommandPausesTheKind(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleJudge] = []behavior{hitsLimit(fableLimitPane)}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomeUsageLimit || res.Pause == nil {
		t.Fatalf("result = %+v", res)
	}
	if res.Pause.Kind != string(agents.HealthUsageLimit) || res.Pause.Tool != agents.KindCodex {
		t.Errorf("pause = %+v, want a usage_limit pause of codex", res.Pause)
	}
	if n := len(e.switchModels()); n != 0 {
		t.Errorf("SwitchModel calls = %d, want none for codex", n)
	}
	if res.Nudged || len(e.ag.submitsFor(agents.RoleJudge)) != 1 {
		t.Errorf("a model limit must not nudge the judge: %+v", e.ag.submitsFor(agents.RoleJudge))
	}
	if runs := e.runsOf(agents.RoleJudge, store.RunContinue); len(runs) != 0 {
		t.Errorf("judge continuation runs = %+v, want none", runs)
	}
	wantRun(t, e.runOf(agents.RoleJudge, store.RunInitial), store.RunFailed, OutcomeUsageLimit)
}

// A judge kind that can switch continues on the fallback model: the
// continuation run is added to the judge's runs, the review it posts carries
// the first run's marker, and both runs are verified with the verdict.
func TestJudgeModelLimitContinuesOnFallbackModel(t *testing.T) {
	e := newEnv(t)
	k := e.cfg.Kinds["codex"]
	k.SwitchModel, k.FallbackModels = "/model {model}", []string{"gpt-5"}
	e.cfg.Kinds["codex"] = k

	// The model-fallback prompt carries no run id: the continuation posts
	// with the marker of the judge's first prompt.
	var marker string
	post := e.judgePosts(706, "COMMENTED", "COMMENT").behavior(t)
	hit := hitsLimit(fableLimitPane)
	e.ag.behaviors[agents.RoleJudge] = []behavior{
		func(f *fakeAgents, run store.Run, text string) error {
			marker = markerRunID(t, text)
			return hit(f, run, text)
		},
		func(f *fakeAgents, run store.Run, text string) error {
			return post(f, run, "run_id: "+marker)
		},
	}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.ReviewID != 706 || res.Pause != nil {
		t.Fatalf("result = %+v", res)
	}
	e.ag.mu.Lock()
	switches := slices.Clone(e.ag.switches)
	e.ag.mu.Unlock()
	want := []switchCall{{Role: string(agents.RoleJudge), Model: "gpt-5", Reason: agents.SwitchLimitHit}}
	if !slices.Equal(switches, want) {
		t.Fatalf("switches = %+v, want %+v", switches, want)
	}

	first := e.runOf(agents.RoleJudge, store.RunInitial)
	cont := e.runOf(agents.RoleJudge, store.RunContinue)
	if marker != first.ID || res.JudgeRunID != first.ID {
		t.Errorf("marker %q / JudgeRunID %q, want the first run %s", marker, res.JudgeRunID, first.ID)
	}
	for _, r := range []store.Run{first, cont} {
		wantRun(t, r, store.RunVerified, OutcomePosted)
		if store.Deref(r.ReviewID) != 706 {
			t.Errorf("run %s (%s) review id = %v, want 706", r.ID, r.Kind, store.Deref(r.ReviewID))
		}
	}
	if cont.Round != first.Round {
		t.Errorf("continuation round %d != %d", cont.Round, first.Round)
	}
	submits := e.ag.submitsFor(agents.RoleJudge)
	if len(submits) != 2 || res.Nudged {
		t.Fatalf("judge submits = %d (nudged %v), want the prompt and one continuation", len(submits), res.Nudged)
	}
	mustContain(t, "continuation prompt", submits[1].Text, "gpt-5", store.Deref(first.ReportPath), target)
	if len(e.gh.reviews) != 1 || !strings.Contains(e.gh.reviews[0].Body, "magnum:run="+first.ID) {
		t.Errorf("reviews = %+v, want one carrying the first run's marker", e.gh.reviews)
	}
}

// A push while a reviewer works on a fallback model cuts the continuation,
// not the run the stage started with (that one already ended on the limit).
func TestPushDuringModelFallbackSettlesTheContinuation(t *testing.T) {
	e := newEnv(t)
	in := e.input(KindInitial)
	e.withRestarts(&in, 2)
	e.ag.behaviors[agents.RoleClaude] = []behavior{hitsLimit(fableLimitPane), pushThen(e, head2, hang()), writeReport("## P2 on the new head\n")}
	post := e.judgePosts(602, "COMMENTED", "COMMENT")
	post.commit = head2
	e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(t)}

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.Restarts != 1 || res.Pause != nil {
		t.Fatalf("result = %+v", res)
	}
	cont := e.runsOf(agents.RoleClaude, store.RunContinue)
	if len(cont) != 1 || cont[0].TargetSHA != target || cont[0].State != store.RunAbandoned || store.Deref(cont[0].Outcome) != ReportHeadMoved {
		t.Fatalf("continuation runs = %+v, want one abandoned as head_moved", cont)
	}
	claudeAgent := agents.AgentName("talkable/talkable", 11920, agents.RoleClaude)
	if !slices.Contains(e.keys.sends, "agent:"+claudeAgent+":esc") {
		t.Errorf("the continuation was not interrupted: %v", e.keys.sends)
	}
	restarted := e.eventsOf("round.restarted")
	if len(restarted) != 1 || !strings.Contains(restarted[0].Message, "cut short: claude-review") {
		t.Fatalf("round.restarted = %+v", restarted)
	}
	if rep := res.Reports[agents.RoleClaude]; rep.Status != ReportOK {
		t.Fatalf("claude report = %+v", rep)
	}
}
