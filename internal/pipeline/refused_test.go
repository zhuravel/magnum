package pipeline

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// codexRefusal is Codex's cybersecurity refusal as its TUI shows it.
const codexRefusal = "• Reading app/controllers/sessions_controller.rb\n\n" +
	"■ This content was flagged for possible cybersecurity risk. If this seems wrong, try rephrasing your request. " +
	"If you’re doing authorized security work that requires more cyber permissive safeguards, apply for Daybreak access via " +
	"https://platform.openai.com/settings/organization/status-and-access before retrying.\n\n› Implement {feature}\n"

// wantRefused checks a round that a refusal ended: its outcome, who was
// refused in which run, and the sentence the PR's last error carries.
func wantRefused(t *testing.T, res RoundResult, role, runID string) {
	t.Helper()
	if res.Outcome != OutcomeRefused || res.Refusal == nil {
		t.Fatalf("result = %+v, want refused", res)
	}
	if res.Refusal.Role != role || res.Refusal.RunID != runID || res.Refusal.Kind != agents.KindCodex || res.Refusal.Detail == "" {
		t.Errorf("refusal = %+v, want role %s run %s", *res.Refusal, role, runID)
	}
	want := "Codex refused the review: content flagged as a cybersecurity risk (" + role + ", run " + runID + ")"
	if res.Error != want || res.Refusal.Sentence() != want {
		t.Errorf("error = %q, sentence %q, want %q", res.Error, res.Refusal.Sentence(), want)
	}
	if res.Nudged {
		t.Error("a refused round was nudged")
	}
}

// The live case: the judge's own pass ended 6 s after its prompt on Codex's
// cybersecurity refusal. The round ends at once: claude-review, still at
// work, is interrupted and its run abandoned, the judge gets no candidates
// prompt and no nudge, and the round says who was refused in which run.
func TestARefusedOwnPassEndsTheRoundAtOnce(t *testing.T) {
	e := newEnv(t)
	e.ag.reads[agents.RoleJudge] = codexRefusal
	e.ag.behaviors[agents.RoleJudge] = []behavior{endSilently()}
	e.ag.behaviors[agents.RoleClaude] = []behavior{hang()}

	res, err := e.r.RunRound(e.ctx, e.ownInput(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	own := e.runOf(agents.RoleJudge, store.RunOwnPass)
	wantRefused(t, res, string(agents.RoleJudge), own.ID)
	wantRun(t, own, store.RunFailed, string(agents.HealthRefused))
	if n := len(e.ag.submitsFor(agents.RoleJudge)); n != 1 {
		t.Errorf("judge submits = %d, want the own pass alone", n)
	}
	wantRun(t, e.runOf(agents.RoleClaude, store.RunInitial), store.RunAbandoned, ReportCancelled)
	if judge := e.runOf(agents.RoleJudge, store.RunInitial); judge.State != store.RunAbandoned {
		t.Errorf("the judge's run = %s, want abandoned (never prompted)", judge.State)
	}
	if !slices.Contains(e.keys.sends, "agent:"+agents.AgentName("talkable/talkable", 11920, agents.RoleClaude)+":esc") {
		t.Errorf("claude-review was not interrupted: %v", e.keys.sends)
	}
	ends := e.eventsOf("round.end")
	if len(ends) != 1 || !strings.Contains(ends[0].Message, "refused") {
		t.Errorf("round.end = %+v", ends)
	}
}

// A refusal of the judge's candidates turn ends the round as refused, with
// no nudge.
func TestARefusedCandidatesTurnIsNotNudged(t *testing.T) {
	e := newEnv(t)
	e.ag.reads[agents.RoleJudge] = codexRefusal
	e.ag.behaviors[agents.RoleJudge] = []behavior{writeOwn(), endSilently()}

	res, err := e.r.RunRound(e.ctx, e.ownInput(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	judge := e.runOf(agents.RoleJudge, store.RunInitial)
	wantRefused(t, res, string(agents.RoleJudge), judge.ID)
	wantRun(t, judge, store.RunFailed, OutcomeRefused)
	if n := len(e.ag.submitsFor(agents.RoleJudge)); n != 2 {
		t.Errorf("judge submits = %d, want the own pass and the candidates, no nudge", n)
	}
	for _, r := range e.runs() {
		if r.Kind == store.RunNudge {
			t.Errorf("a nudge run: %+v", r)
		}
	}
}

// codex-review, Codex run as a shell command, is refused when its output
// says so, whether the command failed or not: the round ends naming it,
// the judge is never prompted, claude-review still at work is stopped.
func TestARefusedCodexReviewEndsTheRoundNamingIt(t *testing.T) {
	for name, status := range map[string]int{"failed": 1, "exit 0": 0} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.ag.exitStatus[agents.RoleCodexReview] = status
			e.ag.codex = func(f *fakeAgents, c codexCall) error {
				return os.WriteFile(filepath.Join(e.reportDir(), "codex-review.md"),
					[]byte("Reviewing the diff against the base\nERROR: This content was flagged for possible cybersecurity risk.\n"), 0o600)
			}
			e.ag.behaviors[agents.RoleClaude] = []behavior{hang()}

			res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
			if err != nil {
				t.Fatalf("RunRound: %v", err)
			}
			run := e.runOf(agents.RoleCodexReview, store.RunInitial)
			wantRefused(t, res, string(agents.RoleCodexReview), run.ID)
			wantRun(t, run, store.RunFailed, string(agents.HealthRefused))
			if rep := res.Reports[agents.RoleCodexReview]; rep.Status != string(agents.HealthRefused) || rep.Path != "" {
				t.Errorf("codex-review report = %+v", rep)
			}
			if n := len(e.ag.submitsFor(agents.RoleJudge)); n != 0 {
				t.Errorf("judge submits = %d, want none", n)
			}
			wantRun(t, e.runOf(agents.RoleClaude, store.RunInitial), store.RunAbandoned, ReportCancelled)
		})
	}
}

// A judge turn whose pane scrolled past the refusal is refused from its
// Codex rollout (the turn's error with codex_error_info cyber_policy).
func TestARefusalTheRolloutRecordsEndsTheRound(t *testing.T) {
	e := newEnv(t)
	e.ag.reads[agents.RoleJudge] = "Done.\n"
	e.ag.turnErrors = map[agents.Role]agents.TurnError{agents.RoleJudge: {
		Message: "This content was flagged for possible cybersecurity risk.", Info: agents.CodexCyberPolicy}}
	e.ag.behaviors[agents.RoleJudge] = []behavior{endSilently()}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	wantRefused(t, res, string(agents.RoleJudge), e.runOf(agents.RoleJudge, store.RunInitial).ID)
	if n := len(e.ag.submitsFor(agents.RoleJudge)); n != 1 {
		t.Errorf("judge submits = %d, want 1", n)
	}
}

// A judge that just stops, with another error in its rollout, is nudged as
// before: only a refusal ends the round at once.
func TestAJudgeThatJustStopsIsStillNudged(t *testing.T) {
	e := newEnv(t)
	e.ag.reads[agents.RoleJudge] = "Done.\n"
	e.ag.turnErrors = map[agents.Role]agents.TurnError{agents.RoleJudge: {Message: "unexpected status 404 Not Found", Info: "other"}}
	e.ag.behaviors[agents.RoleJudge] = []behavior{endSilently(), e.judgePosts(611, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || !res.Nudged || res.Refusal != nil {
		t.Fatalf("result = %+v, want posted after a nudge", res)
	}
}
