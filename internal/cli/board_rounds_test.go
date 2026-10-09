package cli

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

var roundsNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// roundsFixture is one PR whose events and runs the board's round facts read.
type roundsFixture struct {
	h       *actHarness
	pr      store.PR
	subject string
	at      time.Time // the next event's time
}

func newRoundsFixture(t *testing.T) *roundsFixture {
	t.Helper()
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 729, store.PRReviewed)
	return &roundsFixture{h: h, pr: pr, subject: "pr:talkable/talkable#729", at: roundsNow.Add(-3 * time.Hour)}
}

// event appends an event of the PR a minute after the previous one; data is
// marshalled (a string is taken as the JSON text itself, nil as no data).
func (f *roundsFixture) event(kind string, data any) {
	f.eventOf(f.subject, kind, data)
}

func (f *roundsFixture) eventOf(subject, kind string, data any) {
	f.h.t.Helper()
	ev := store.Event{At: f.at, Subject: &subject, Kind: kind, Message: kind}
	switch d := data.(type) {
	case nil:
	case string:
		ev.Data = json.RawMessage(d)
	default:
		b, err := json.Marshal(d)
		if err != nil {
			f.h.t.Fatal(err)
		}
		ev.Data = b
	}
	if _, err := f.h.st.AppendEvent(f.h.ctx, ev); err != nil {
		f.h.t.Fatal(err)
	}
	f.at = f.at.Add(time.Minute)
}

// roundStart is the engine's engine.round_start event.
func (f *roundsFixture) roundStart(kind string, roles, requested []string, postMerge bool) {
	f.event("engine.round_start", map[string]any{"slot": "review1", "kind": kind, "target_sha": "abc1234", "roles": roles,
		"requested": requested, "post_merge": postMerge})
}

// triageDecision is a round.triage event of a decision.
func (f *roundsFixture) triageDecision(runs, skips []string, reason string) {
	f.event("round.triage", map[string]any{"lines": 40, "max_lines": 400, "runs": runs, "skips": skips, "reason": reason})
}

func (f *roundsFixture) rerun(role string, lines int) {
	f.event("round.rerun_role", map[string]any{"role": role, "lines": lines, "since": "abc1234", "min": 100})
}

// facts runs the filler over the PR's board row and returns the row.
func (f *roundsFixture) facts() tui.PRBoardRow {
	f.h.t.Helper()
	rows := []tui.PRBoardRow{{Owner: "talkable", Repo: "talkable", Number: 729}}
	if err := boardRoundFacts(f.h.ctx, f.h.st, []int64{f.pr.ID}, rows, roundsNow); err != nil {
		f.h.t.Fatalf("boardRoundFacts: %v", err)
	}
	return rows[0]
}

// why is the row's RoundWhy without its time, which
// TestBoardRoundFactsTellWhenTheRoundStarted checks.
func (f *roundsFixture) why() *tui.RoundWhy {
	f.h.t.Helper()
	w := f.facts().RoundWhy
	if w != nil {
		w.At = time.Time{}
	}
	return w
}

func TestBoardRoundFactsContinueRoundRunsTheJudgeAlone(t *testing.T) {
	f := newRoundsFixture(t)
	f.roundStart("rereview", []string{"codex-judge", "claude-review"}, nil, false)
	f.roundStart("continue", []string{"codex-judge"}, nil, false)
	want := &tui.RoundWhy{Kind: "continue", Roles: []string{"codex-judge"}}
	if got := f.why(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RoundWhy = %+v, want %+v", got, want)
	}
}

func TestBoardRoundFactsTriageDecisionWithSkipsAndReason(t *testing.T) {
	f := newRoundsFixture(t)
	f.triageDecision([]string{"codex-judge", "claude-review"}, []string{"codex-review", "claude-simplify"}, "small Ruby-only change")
	f.roundStart("rereview", []string{"codex-judge", "claude-review"}, nil, false)
	want := &tui.RoundWhy{Kind: "rereview", Roles: []string{"codex-judge", "claude-review"}, Triaged: true,
		Skipped: []string{"codex-review", "claude-simplify"}, Reason: "small Ruby-only change"}
	if got := f.why(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RoundWhy = %+v, want %+v", got, want)
	}
}

// A decision that dropped nothing has "skips": null (the engine's nil slice).
func TestBoardRoundFactsTriageThatSkippedNothing(t *testing.T) {
	f := newRoundsFixture(t)
	f.event("round.triage", `{"lines":40,"max_lines":400,"runs":["codex-judge","claude-review"],"skips":null,"reason":"touches payments"}`)
	f.roundStart("rereview", []string{"codex-judge", "claude-review"}, nil, false)
	got := f.why()
	if got == nil || !got.Triaged || len(got.Skipped) != 0 || got.Reason != "touches payments" || got.EveryRole != "" {
		t.Fatalf("RoundWhy = %+v, want a decision that skipped nothing", got)
	}
}

func TestBoardRoundFactsTriageThatRanEveryRole(t *testing.T) {
	f := newRoundsFixture(t)
	f.event("round.triage", map[string]any{"why": "the diff could not be read: 503"})
	f.roundStart("initial", []string{"codex-judge", "claude-review", "codex-review"}, nil, false)
	want := &tui.RoundWhy{Kind: "initial", Roles: []string{"codex-judge", "claude-review", "codex-review"},
		EveryRole: "the diff could not be read: 503"}
	if got := f.why(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RoundWhy = %+v, want %+v", got, want)
	}
}

// A role that ran again because its code changed: once per role (a setup
// that was retried recorded it twice, the newer count counts).
func TestBoardRoundFactsRerunAddedARole(t *testing.T) {
	f := newRoundsFixture(t)
	f.rerun("claude-simplify", 180)
	f.rerun("claude-simplify", 212)
	f.rerun("codex-review", 95)
	f.roundStart("rereview", []string{"codex-judge", "claude-review", "claude-simplify", "codex-review"},
		[]string{"claude-simplify", "codex-review"}, false)
	want := &tui.RoundWhy{Kind: "rereview", Roles: []string{"codex-judge", "claude-review", "claude-simplify", "codex-review"},
		Requested: []string{"claude-simplify", "codex-review"},
		Reruns:    []tui.RoleRerun{{Role: "claude-simplify", Lines: 212}, {Role: "codex-review", Lines: 95}}}
	if got := f.why(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RoundWhy = %+v, want %+v", got, want)
	}
}

func TestBoardRoundFactsRequestedRole(t *testing.T) {
	f := newRoundsFixture(t)
	f.roundStart("rereview", []string{"codex-judge", "claude-simplify"}, []string{"claude-simplify"}, true)
	want := &tui.RoundWhy{Kind: "rereview", PostMerge: true, Roles: []string{"codex-judge", "claude-simplify"},
		Requested: []string{"claude-simplify"}}
	if got := f.why(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RoundWhy = %+v, want %+v", got, want)
	}
}

// What triage and the reruns recorded for an earlier round, for a round
// that has not started yet, or for another PR is not the last round's.
func TestBoardRoundFactsIgnoresEventsOfOtherRounds(t *testing.T) {
	f := newRoundsFixture(t)
	f.triageDecision([]string{"codex-judge"}, []string{"claude-review"}, "first round's reason")
	f.rerun("claude-simplify", 300)
	f.roundStart("initial", []string{"codex-judge"}, nil, false)
	f.roundStart("rereview", []string{"codex-judge", "claude-review"}, nil, false) // nothing decided its roles
	// The next round is still in setup: it has decided, not started.
	f.triageDecision([]string{"codex-judge"}, []string{"claude-review"}, "next round's reason")
	f.rerun("codex-review", 500)
	// Another PR's, and a subject of the PR's id.
	f.eventOf("pr:talkable/talkable#730", "round.triage", map[string]any{"runs": []string{"codex-judge"}, "skips": []string{"x"}, "reason": "other PR"})
	f.eventOf("pr:talkable/talkable#7290", "engine.round_start", map[string]any{"kind": "initial", "roles": []string{"zzz"}})
	f.eventOf("pr:1", "engine.round_start", map[string]any{"kind": "initial", "roles": []string{"zzz"}})

	want := &tui.RoundWhy{Kind: "rereview", Roles: []string{"codex-judge", "claude-review"}}
	if got := f.why(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RoundWhy = %+v, want %+v", got, want)
	}
}

// A setup that failed after triage and was retried recorded its triage more
// than once before the round started: the newest is the round's.
func TestBoardRoundFactsTakesTheNewestTriageOfARetriedSetup(t *testing.T) {
	f := newRoundsFixture(t)
	f.roundStart("initial", []string{"codex-judge", "claude-review"}, nil, false)
	f.event("round.triage", map[string]any{"why": "its answer names none of the round's roles"})
	f.triageDecision([]string{"codex-judge"}, []string{"claude-review"}, "docs only")
	f.roundStart("rereview", []string{"codex-judge"}, nil, false)
	got := f.why()
	if got == nil || !got.Triaged || got.Reason != "docs only" || got.EveryRole != "" || !reflect.DeepEqual(got.Skipped, []string{"claude-review"}) {
		t.Fatalf("RoundWhy = %+v, want the retry's decision", got)
	}
}

// Without a round that started there is nothing to say: the triage of a
// setup that has not reached its round does not make one.
func TestBoardRoundFactsNeedsAStartedRound(t *testing.T) {
	f := newRoundsFixture(t)
	if got := f.why(); got != nil {
		t.Fatalf("RoundWhy of a PR without events = %+v", got)
	}
	f.triageDecision([]string{"codex-judge"}, []string{"claude-review"}, "docs only")
	if got := f.why(); got != nil {
		t.Fatalf("RoundWhy of a setup that has not started = %+v", got)
	}
}

// Only the last 7 days are read.
func TestBoardRoundFactsReadsOnlyTheWindow(t *testing.T) {
	f := newRoundsFixture(t)
	f.at = roundsNow.Add(-8 * 24 * time.Hour)
	f.roundStart("initial", []string{"codex-judge"}, nil, false)
	if got := f.why(); got != nil {
		t.Fatalf("RoundWhy of a round from 8 days ago = %+v", got)
	}
}

// Another team changes what triage and the reruns record: a part that is
// missing or of another type is skipped, never an error or a panic.
func TestBoardRoundFactsReadsEventsDefensively(t *testing.T) {
	f := newRoundsFixture(t)
	f.event("round.triage", `["not","an","object"]`)
	f.event("round.triage", `"just a string"`)
	f.event("round.triage", nil)
	f.event("round.triage", map[string]any{"runs": "codex-judge", "skips": 3, "reason": 7})
	f.event("round.rerun_role", map[string]any{"role": 5, "lines": "many"})
	f.event("round.rerun_role", map[string]any{"lines": 40})
	f.event("round.rerun_role", `{"role":"claude-simplify","lines":"212"}`)
	f.event("engine.round_start", `{"kind":["rereview"],"roles":"codex-judge","requested":[1,"claude-simplify",null],"post_merge":"yes"}`)
	got := f.why()
	if got == nil {
		t.Fatal("no RoundWhy from a round_start with odd data")
	}
	if got.Kind != "" || len(got.Roles) != 0 || got.PostMerge || !reflect.DeepEqual(got.Requested, []string{"claude-simplify"}) {
		t.Errorf("round_start parts = %+v", got)
	}
	if got.Triaged || got.EveryRole != "" {
		t.Errorf("an unreadable triage = %+v", got)
	}
	if !reflect.DeepEqual(got.Reruns, []tui.RoleRerun{{Role: "claude-simplify"}}) {
		t.Errorf("reruns = %+v, want only the one with a role", got.Reruns)
	}

	f.event("engine.round_start", nil)
	if got := f.why(); got == nil || got.Kind != "" || len(got.Roles) != 0 {
		t.Errorf("a round_start without data = %+v", got)
	}
}

func TestBoardRoundFactsSpendOverTheLastWeek(t *testing.T) {
	f := newRoundsFixture(t)
	other := f.h.seedPR("talkable/talkable", 730, store.PRReviewed)
	quiet := f.h.seedPR("talkable/talkable", 731, store.PRReviewed)
	run := func(prID int64, round int, role string, created time.Duration, ended *time.Duration) {
		t.Helper()
		r := store.Run{PRID: prID, Round: round, Role: role, Kind: store.RunInitial, State: store.RunVerified, TargetSHA: "h1",
			Identity: "i", ReviewerLogin: "l", PromptText: "p", CreatedAt: roundsNow.Add(created)}
		if ended != nil {
			r.EndedAt = new(roundsNow.Add(*ended))
		} else {
			r.State = store.RunWorking
		}
		if _, err := f.h.st.CreateRun(f.h.ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	end := func(d time.Duration) *time.Duration { return &d }
	run(f.pr.ID, 1, store.RoleClaude, -50*time.Hour, end(-49*time.Hour))    // 1h
	run(f.pr.ID, 1, store.RoleJudge, -50*time.Hour, end(-48*time.Hour))     // 2h
	run(f.pr.ID, 2, store.RoleJudge, -2*time.Hour, end(-time.Hour))         // 1h
	run(f.pr.ID, 3, store.RoleJudge, -30*time.Minute, nil)                  // still going: 30m
	run(f.pr.ID, 0, store.RoleJudge, -8*24*time.Hour, end(-7*24*time.Hour)) // before the window
	run(other.ID, 1, store.RoleClaude, -time.Hour, end(-45*time.Minute))    // 15m

	rows := []tui.PRBoardRow{{Owner: "talkable", Repo: "talkable", Number: 729}, {Owner: "talkable", Repo: "talkable", Number: 730},
		{Owner: "talkable", Repo: "talkable", Number: 731}}
	if err := boardRoundFacts(f.h.ctx, f.h.st, []int64{f.pr.ID, other.ID, quiet.ID}, rows, roundsNow); err != nil {
		t.Fatal(err)
	}
	want := []*tui.SpendInfo{
		{Window: 7 * 24 * time.Hour, AgentTime: 4*time.Hour + 30*time.Minute, Rounds: 3},
		{Window: 7 * 24 * time.Hour, AgentTime: 15 * time.Minute, Rounds: 1},
		nil,
	}
	for i, r := range rows {
		if !reflect.DeepEqual(r.Spend, want[i]) {
			t.Errorf("row %d Spend = %+v, want %+v", i, r.Spend, want[i])
		}
	}
}

func TestBoardRoundFactsWithoutRowsReadsNothing(t *testing.T) {
	f := newRoundsFixture(t)
	if err := boardRoundFacts(f.h.ctx, f.h.st, nil, nil, roundsNow); err != nil {
		t.Fatal(err)
	}
}
