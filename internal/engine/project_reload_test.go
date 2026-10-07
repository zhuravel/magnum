package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// reloadingClaude makes claude-review's sessions reload the checkout's
// project config they loaded (a Claude session started with it, as
// agents.Manager.ReloadsProject reports) and records, for each quit, the
// commit the slot had checked out then.
func reloadingClaude(h *harness) *[]string {
	var at []string
	h.ag.mu.Lock()
	h.ag.reloads = map[string]bool{store.RoleClaude: true}
	h.ag.resumeIDs = map[agents.Role]string{agents.RoleClaude: "uuid-claude"}
	h.ag.onQuit = func(s store.Session) {
		at = append(at, s.Role+"@"+deref(h.slot("review1").CheckedOutSHA))
	}
	h.ag.mu.Unlock()
	return &at
}

func liveRole(h *harness, n int, role string) bool {
	h.t.Helper()
	s, err := h.st.LiveSessionByPRRole(h.ctx, h.pr(n).ID, role)
	return err == nil && s.State == store.SessionLive
}

// Claude Code reloads .claude/settings.json (hooks included) and its skills
// while it runs, and loads one a later commit adds: a claude-review session
// that started with the project config loaded would take the new head's
// from disk once the checkout moves under it. A re-review on a new head
// quits it first (its conversation parked), while the slot still has the
// reviewed head, and resumes it after the checkout, when its start decides
// with the new head; a session that does not reload (the judge) stays.
func TestALiveClaudeSessionIsParkedBeforeTheCheckoutMoves(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	if !liveRole(h, 2, store.RoleClaude) || !liveRole(h, 2, store.RoleJudge) {
		t.Fatal("the first review left no live sessions")
	}
	at := reloadingClaude(h)
	h.ag.mu.Lock()
	h.ag.efforts = nil
	h.ag.mu.Unlock()
	h.advance(30 * time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d, want the re-review", n)
	}
	pr := h.pr(2)
	if !slices.Equal(*at, []string{store.RoleClaude + "@b1"}) {
		t.Fatalf("quits (role@checked-out head) = %v, want claude-review before the checkout of b2", *at)
	}
	if slices.Contains(h.ag.all(), fmt.Sprintf("quit:%d:%s", pr.ID, store.RoleJudge)) {
		t.Fatalf("calls %v: the judge does not reload its project config", h.ag.all())
	}
	if starts := agentStarts(h); !slices.ContainsFunc(starts, func(s string) bool { return s == "claude-review:medium:uuid-claude" }) {
		t.Fatalf("starts %v: want claude-review resumed on the new head", starts)
	}
	evs := approvalEvents(t, h, 2, "round.project_reload_parked")
	if len(evs) != 1 || evs[0].Message != "claude-review parked before the checkout moves: its agent reloads the checkout's "+
		"project config while it runs, so it resumes on the new head with what that head allows" {
		t.Fatalf("events = %+v", evs)
	}
}

// A session that reloads the project config and is working (or blocked)
// cannot be quit safely, and the checkout must not move under it: the
// round waits, uncharged, and nothing is checked out.
func TestAWorkingClaudeSessionHoldsTheCheckout(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	reloadingClaude(h)
	s, err := h.st.LiveSessionByPRRole(h.ctx, h.pr(2).ID, store.RoleClaude)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.UpdateSession(h.ctx, s.ID, func(u *store.SessionUpdate) { u.Set("agent_status", "working") }); err != nil {
		t.Fatal(err)
	}
	h.advance(30 * time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d, want none on b2 while claude-review works", n)
	}
	if slices.ContainsFunc(h.sl.all(), func(c string) bool { return c == fmt.Sprintf("checkout:review1:%d:b2", h.pr(2).ID) }) {
		t.Fatalf("slot calls %v: the checkout moved under a working session", h.sl.all())
	}
	if !liveRole(h, 2, store.RoleClaude) {
		t.Fatal("a working session was quit")
	}
}

// A push while the reviewers run restarts them on the new head in place
// (RoundInput.Switch): a session that reloads the project config it loaded
// is quit before the switch moves the checkout and resumed after it, so the
// restart prompts it on the new head with what that head allows.
func TestRoundRestartParksAReloadingSessionAcrossTheSwitch(t *testing.T) {
	h := newHarness(t)
	h.queuedPR(2, "b1")
	h.advance(5 * time.Minute)
	var at *[]string
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		if !liveRole(h, 2, store.RoleClaude) {
			return pipeline.RoundResult{Outcome: pipeline.OutcomeError}, errors.New("claude-review is not live")
		}
		at = reloadingClaude(h)
		h.ag.mu.Lock()
		h.ag.efforts = nil
		h.ag.mu.Unlock()
		h.pushed(2, "b2")
		sw, err := in.Switch(context.Background(), "b2")
		if err != nil {
			return pipeline.RoundResult{Outcome: pipeline.OutcomeError, Error: err.Error()}, err
		}
		if !liveRole(h, 2, store.RoleClaude) {
			t.Error("claude-review is not live again after the switch")
		}
		return posted(sw.TargetSHA, 1), nil
	}
	h.tick()
	if at == nil || !slices.Equal(*at, []string{store.RoleClaude + "@b1"}) {
		t.Fatalf("quits (role@checked-out head) = %v, want claude-review before the switch to b2", at)
	}
	if starts := agentStarts(h); !slices.ContainsFunc(starts, func(s string) bool { return s == "claude-review:high:uuid-claude" }) {
		t.Fatalf("starts %v: want claude-review resumed after the switch", starts)
	}
	if got := h.wantState(2, store.PRReviewed); deref(got.ReviewedSHA) != "b2" {
		t.Fatalf("reviewed_sha = %q, want b2", deref(got.ReviewedSHA))
	}
}

// projectGit answers ChangedUnder per head (the files the head changes at
// or under the paths since its merge base with the base) and records each
// comparison as "<base>...<head>:<paths>".
type projectGit struct {
	fakeGit
	mu      sync.Mutex
	changed map[string][]string
	err     error
	calls   []string
}

func (g *projectGit) ChangedUnder(_ context.Context, _, base, head string, paths ...string) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, base+"..."+head+":"+strings.Join(paths, ","))
	if g.err != nil {
		return nil, g.err
	}
	return g.changed[head], nil
}

func (g *projectGit) all() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.calls)
}

// claudePaths are the paths a comparison of Claude's project config names
// (agents.ProjectPaths), as projectGit records them.
var claudePaths = strings.Join(agents.ProjectPaths(agents.KindClaude), ",")

// A new head that leaves the reloaded project config alone brings the
// session nothing new to reload, so magnum does not quit it: a quit that
// cannot stop the agent (its MCP servers still in the pane's foreground)
// held such a round for good. The checkout's fetch runs first, once, and
// the head it fetched decides: by the PR's file list when it is complete
// and for that head, else by git (the head's changes at or under .claude
// and .mcp.json since its merge base with the base branch), which a PR of
// more files than the list holds needs. A head that changes the config, and
// a git that cannot tell, still quit the session.
func TestALiveClaudeSessionStaysWhenTheNewHeadLeavesItsConfigAlone(t *testing.T) {
	untouched := []string{"app/models/order.rb", "spec/models/order_spec.rb"}
	for _, c := range []struct {
		name      string
		files     []string
		truncated bool
		moveHead  string              // the fetch finds this head instead of the radar's b2
		changed   map[string][]string // git's answer per head
		gitErr    error
		fetchErr  error
		wantQuit  bool
		wantGit   string // the comparison git ran ("" = none)
	}{
		{name: "untouched list", files: untouched},
		{name: "settings changed", files: []string{"app/models/order.rb", ".claude/settings.json"}, wantQuit: true},
		{name: "mcp changed", files: []string{".mcp.json"}, wantQuit: true},
		// Claude Code loads the CLAUDE.md of each directory it reads files in,
		// and AGENTS.md where a project has none.
		{name: "a directory's CLAUDE.md changed", files: []string{"app/models/order.rb", "devops/eks/CLAUDE.md"}, wantQuit: true},
		{name: "AGENTS.md changed", files: []string{"AGENTS.md"}, wantQuit: true},
		{name: "cut-off list, the glob matches a wider name only", files: untouched, truncated: true,
			changed: map[string][]string{"b2": {"docs/AGENTIC.md"}}, wantGit: "origin/master...b2:" + claudePaths},
		{name: "cut-off list, git finds the config alone", files: untouched, truncated: true,
			wantGit: "origin/master...b2:" + claudePaths},
		{name: "cut-off list, git finds a change", files: untouched, truncated: true,
			changed: map[string][]string{"b2": {".claude/settings.json"}}, wantQuit: true, wantGit: "origin/master...b2:" + claudePaths},
		{name: "cut-off list, git fails", files: untouched, truncated: true, gitErr: errors.New("fatal: bad object"),
			wantQuit: true, wantGit: "origin/master...b2:" + claudePaths},
		{name: "no list, git finds the config alone", wantGit: "origin/master...b2:" + claudePaths},
		{name: "list for another head than the fetched one", files: untouched, moveHead: "b3",
			changed: map[string][]string{"b3": {".mcp.json"}}, wantQuit: true, wantGit: "origin/master...b3:" + claudePaths},
		{name: "the fetch fails, the radar head's list decides", files: untouched, fetchErr: errors.New("fetch: network down")},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := &projectGit{changed: c.changed, err: c.gitErr}
			h := newHarness(t, func(h *harness) { h.d.Git = g })
			h.reviewedPR(2, "b1")
			at := reloadingClaude(h)
			h.sl.mu.Lock()
			h.sl.moveHead, h.sl.fetchErr = c.moveHead, c.fetchErr
			h.sl.mu.Unlock()
			before := len(h.sl.all())
			h.advance(30 * time.Minute)
			h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2", files: c.files, filesTruncated: c.truncated})
			h.tick()
			h.advance(5 * time.Minute)
			h.tick()
			rounds := h.rd.all()
			if len(rounds) != 2 {
				t.Fatalf("rounds = %d, want the re-review", len(rounds))
			}
			if quit := len(*at) > 0; quit != c.wantQuit {
				t.Fatalf("quits = %v, want a quit: %v", *at, c.wantQuit)
			}
			calls := g.all()
			if c.wantGit == "" && len(calls) > 0 || c.wantGit != "" && !slices.Equal(calls, []string{c.wantGit}) {
				t.Fatalf("git comparisons = %q, want %q", calls, c.wantGit)
			}
			slotCalls := h.sl.all()[before:]
			fetches := slices.DeleteFunc(slices.Clone(slotCalls), func(s string) bool { return !strings.HasPrefix(s, "fetch:") })
			if want := 1 + btoi(c.fetchErr != nil); len(fetches) != want {
				t.Fatalf("slot calls %v: %d fetches, want %d (the checkout reuses the one before the decision)", slotCalls, len(fetches), want)
			}
			target := cmp.Or(c.moveHead, "b2")
			if c.fetchErr == nil && !slices.Contains(slotCalls, fmt.Sprintf("checkout-fetched:review1:%d:%s", h.pr(2).ID, target)) {
				t.Fatalf("slot calls %v: want the fetched head checked out without another fetch", slotCalls)
			}
			if got := rounds[1].TargetSHA; got != target {
				t.Fatalf("the re-review's target = %s, want the fetched %s", got, target)
			}
		})
	}
}

// A restart's switch to a newer head asks the same question with the head
// it switches to: without a list for that head git decides, and the switch
// checks out the head it fetched for that, fetching once.
func TestRoundRestartDecidesByGitWithoutAListOfTheNewHead(t *testing.T) {
	g := &projectGit{}
	h := newHarness(t, func(h *harness) { h.d.Git = g })
	h.queuedPR(2, "b1")
	h.advance(5 * time.Minute)
	var at *[]string
	var during []string
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		at = reloadingClaude(h)
		h.pushed(2, "b2")
		before := len(h.sl.all())
		sw, err := in.Switch(context.Background(), "b2")
		during = h.sl.all()[before:]
		if err != nil {
			return pipeline.RoundResult{Outcome: pipeline.OutcomeError, Error: err.Error()}, err
		}
		return posted(sw.TargetSHA, 1), nil
	}
	h.tick()
	if at == nil || len(*at) != 0 {
		t.Fatalf("quits = %v, want none: git finds b2 leaves the config alone", at)
	}
	if calls := g.all(); !slices.Equal(calls, []string{"origin/master...b2:" + claudePaths}) {
		t.Fatalf("git comparisons = %q", calls)
	}
	id := h.pr(2).ID
	if want := []string{fmt.Sprintf("fetch:review1:%d", id), fmt.Sprintf("checkout-fetched:review1:%d:b2", id),
		fmt.Sprintf("checkout:review1:%d:b2", id)}; !slices.Equal(during, want) {
		t.Fatalf("slot calls of the switch = %v, want %v", during, want)
	}
	if got := h.wantState(2, store.PRReviewed); deref(got.ReviewedSHA) != "b2" {
		t.Fatalf("reviewed_sha = %q, want b2", deref(got.ReviewedSHA))
	}
}

// A live session that does not reload its project config needs no answer
// before the checkout: the checkout fetches as it always did, once.
func TestNoReloadingSessionFetchesInTheCheckoutOnly(t *testing.T) {
	g := &projectGit{}
	h := newHarness(t, func(h *harness) { h.d.Git = g })
	h.reviewedPR(2, "b1")
	before := len(h.sl.all())
	h.advance(30 * time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d, want the re-review", n)
	}
	got := h.sl.all()[before:]
	want := []string{fmt.Sprintf("fetch:review1:%d", h.pr(2).ID), fmt.Sprintf("checkout:review1:%d:b2", h.pr(2).ID)}
	if i := slices.Index(got, want[0]); i < 0 || i+1 >= len(got) || got[i+1] != want[1] || slices.ContainsFunc(got, func(s string) bool {
		return strings.HasPrefix(s, "checkout-fetched:")
	}) {
		t.Fatalf("slot calls %v, want %v", got, want)
	}
	if calls := g.all(); len(calls) > 0 {
		t.Fatalf("git comparisons %q without a reloading session", calls)
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// An idle session whose quit fails (its agent did not stop: MCP servers
// still in the pane's foreground) keeps the checkout where it is, but the
// failure is charged, not retried every tick for good: the PR's attempts
// count it and it backs off.
func TestAQuitThatFailsIsChargedNotRetriedForGood(t *testing.T) {
	h := newHarness(t)
	h.reviewedPR(2, "b1")
	reloadingClaude(h)
	h.ag.mu.Lock()
	h.ag.quitErr = fmt.Errorf("agents: quit session 1: %w: shell not idle after 10s", agents.ErrBusy)
	h.ag.mu.Unlock()
	h.advance(30 * time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	if n := len(h.rd.all()); n != 1 {
		t.Fatalf("rounds = %d, want none on b2", n)
	}
	if pr := h.pr(2); pr.Attempts != 1 {
		t.Fatalf("attempts = %d, want the failed quit charged once", pr.Attempts)
	}
}
