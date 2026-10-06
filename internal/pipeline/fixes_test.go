package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// setStatus records an agent status on a role's session, as Observe does.
func (e *env) setStatus(role agents.Role, status herdr.Status) {
	e.t.Helper()
	if err := e.st.UpdateSession(context.Background(), e.sess[role].ID, func(u *store.SessionUpdate) {
		u.Set("agent_status", string(status))
	}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) sendsTo(role agents.Role) []string {
	e.keys.mu.Lock()
	defer e.keys.mu.Unlock()
	var out []string
	prefix := "agent:" + store.Deref(e.sess[role].AgentName) + ":"
	for _, s := range e.keys.sends {
		if rest, ok := strings.CutPrefix(s, prefix); ok {
			out = append(out, rest)
		}
	}
	return out
}

// A judge whose turn timed out is interrupted (ctrl+c twice) and the round
// ends once it is seen idle, so it cannot post after the round.
func TestJudgeTimeoutInterruptsJudge(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleJudge] = []behavior{hang()}
	e.setStatus(agents.RoleJudge, herdr.StatusWorking)
	e.keys.onAgent = func(target string, keys []string) { e.setStatus(agents.RoleJudge, herdr.StatusIdle) }
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomeTimeout {
		t.Fatalf("result = %+v", res)
	}
	if got := strings.Join(e.sendsTo(agents.RoleJudge), " "); got != "ctrl+c ctrl+c" {
		t.Fatalf("judge keys = %q, want ctrl+c twice", got)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "still works") {
			t.Fatalf("warnings = %v", res.Warnings)
		}
	}

	// An agent that keeps working: the round still ends, after InterruptWait.
	e2 := newEnv(t)
	e2.ag.behaviors[agents.RoleJudge] = []behavior{hang()}
	e2.setStatus(agents.RoleJudge, herdr.StatusWorking)
	res, _ = e2.r.RunRound(e2.ctx, e2.input(KindInitial))
	if res.Outcome != OutcomeTimeout || !strings.Contains(strings.Join(res.Warnings, "\n"), "codex-judge still works") {
		t.Fatalf("result = %+v", res)
	}
}

// A PENDING draft carrying the marker is never taken for the posted review,
// so nothing is dismissed around it either.
func TestPendingReviewIsNotAccepted(t *testing.T) {
	e := newEnv(t)
	p := e.judgePosts(620, "PENDING", "COMMENT")
	e.ag.behaviors[agents.RoleJudge] = []behavior{p.behavior(t)}
	in := e.input(KindRereview)
	in.Previous = &PreviousReview{ID: 500, Event: "CHANGES_REQUESTED", SHA: prevSHA}
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome == OutcomePosted || res.ReviewID != 0 || len(e.gh.dismissed) != 0 {
		t.Fatalf("result = %+v, dismissed %v", res, e.gh.dismissed)
	}

	// A review the REST view shows unsubmitted is not accepted either.
	e2 := newEnv(t)
	e2.ag.behaviors[agents.RoleJudge] = []behavior{func(f *fakeAgents, run store.Run, text string) error {
		id := markerRunID(t, text)
		e2.gh.add(github.Review{DatabaseID: 621, State: "COMMENTED", Body: "x <!-- magnum:run=" + id + " -->", SubmittedAt: f.st.Clock(),
			CommitOid: target, AuthorLogin: "talkable", AuthorType: "Bot"},
			github.RESTReview{ID: 621, UserLogin: "talkable[bot]", UserType: "Bot", State: "PENDING", CommitID: target})
		return f.end(run.ID)
	}, endSilently()}
	res, _ = e2.r.RunRound(e2.ctx, e2.input(KindInitial))
	if res.Outcome == OutcomePosted || res.ReviewID != 0 {
		t.Fatalf("REST pending: result = %+v", res)
	}
}

// A stale report that cannot be moved aside fails the round before anything
// is prompted: an empty run would otherwise pass it off as its report.
func TestStaleReportThatCannotMoveFailsRound(t *testing.T) {
	e := newEnv(t)
	dir := e.reportDir()
	if err := os.MkdirAll(filepath.Join(dir, "claude-review.md.prev", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "claude-review.md"), []byte("old report"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err == nil || res.Outcome != OutcomeError || !strings.Contains(res.Error, "stale claude-review.md") {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	if n := len(e.ag.submits); n != 0 {
		t.Fatalf("submits = %d, want none", n)
	}
}

// Login preflight runs once per kind and environment: a role pointing its
// CLI at another account is checked (and skipped) on its own.
func TestPreflightPerEnvironment(t *testing.T) {
	e := newEnv(t)
	for i := range e.cfg.Roles {
		if e.cfg.Roles[i].Name == "codex-review" {
			e.cfg.Roles[i].Env = map[string]string{"CODEX_HOME": "/other"}
		}
	}
	e.ag.preflight["codex-review"] = errors.Join(errors.New("codex login status: Not logged in"), agents.ErrLoginRequired)
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(630, "COMMENTED", "COMMENT").behavior(t)}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if got := strings.Join(e.ag.preflights, " "); got != "codex claude codex" {
		t.Fatalf("preflights = %s", got)
	}
	if rep := res.Reports[agents.RoleCodexReview]; rep.Status != string(agents.HealthLoginRequired) {
		t.Fatalf("codex-review report = %+v", rep)
	}
	if res.Outcome != OutcomePosted || len(e.ag.shellCallsFor(agents.RoleCodexReview)) != 0 {
		t.Fatalf("result = %+v", res)
	}
}

// A [[repo]] block's verdicts override the posting identity's in the judge
// prompt; without one the identity's values stand (TestHappyPathInitialRound).
func TestRepoVerdictsOverrideIdentity(t *testing.T) {
	e := newEnv(t)
	e.cfg.Repos = []config.Repo{{Repo: "Talkable/Talkable", NoFindingsEvent: "APPROVE", BlockingEvent: "COMMENT"}}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(640, "COMMENTED", "COMMENT").behavior(t)}
	if _, err := e.r.RunRound(e.ctx, e.input(KindInitial)); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "no_findings_event: APPROVE", "blocking_event: COMMENT")

	// Only one key set: the other comes from the identity.
	e2 := newEnv(t)
	e2.cfg.Repos = []config.Repo{{Repo: "talkable/talkable", BlockingEvent: "COMMENT"}}
	e2.ag.behaviors[agents.RoleJudge] = []behavior{e2.judgePosts(641, "COMMENTED", "COMMENT").behavior(t)}
	if _, err := e2.r.RunRound(e2.ctx, e2.input(KindInitial)); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	mustContain(t, "judge prompt", e2.ag.submitsFor(agents.RoleJudge)[0].Text, "no_findings_event: COMMENT", "blocking_event: COMMENT")
}

// A round cancelled while codex review runs in its shell pane, for any reason
// but a push (whose restart interrupts it), stops the command with ctrl+c:
// left running, it would hold the pane, and the next round's codex-review
// would end busy.
func TestCancelledRoundInterruptsShellCommand(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(e.ctx)
	var mu sync.Mutex
	running := map[string]bool{} // panes whose command still runs
	e.keys.onPane = func(pane string, keys []string) {
		if slices.Contains(keys, "ctrl+c") {
			mu.Lock()
			delete(running, pane)
			mu.Unlock()
		}
	}
	calls := 0
	e.ag.codex = func(f *fakeAgents, c codexCall) error {
		mu.Lock()
		defer mu.Unlock()
		if calls++; calls == 1 {
			running[c.Pane] = true // the round is cancelled; the command keeps going
			cancel()
			return context.Canceled
		}
		if running[c.Pane] {
			return fmt.Errorf("agents: %s: pane %s is not an idle shell: %w", c.Role, c.Pane, agents.ErrBusy)
		}
		return os.WriteFile(filepath.Join(e.reportDir(), "codex-review.md"), []byte("[P2] codex finding\n"), 0o600)
	}
	res, err := e.r.RunRound(ctx, e.input(KindInitial))
	if !errorsIs(err, context.Canceled) || res.Outcome != OutcomeStopped {
		t.Fatalf("cancelled round: result = %+v, err = %v", res, err)
	}
	pane := store.Deref(e.sess[agents.RoleCodexReview].HerdrPaneID)
	e.keys.mu.Lock()
	sends := slices.Clone(e.keys.sends)
	e.keys.mu.Unlock()
	if !slices.Contains(sends, "pane:"+pane+":ctrl+c") {
		t.Fatalf("keys = %v, want ctrl+c to the codex-review pane %s", sends, pane)
	}

	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(700, "CHANGES_REQUESTED", "REQUEST_CHANGES").behavior(t)}
	res, err = e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("next round: result = %+v, err = %v", res, err)
	}
	if rep := res.Reports[agents.RoleCodexReview]; rep.Status != ReportOK {
		t.Fatalf("next round's codex-review = %+v, want ok", rep)
	}
}
