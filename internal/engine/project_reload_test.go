package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/gitx"
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
	h.ag.onQuit = func(s store.Session) { // on the round's goroutine: no t.Fatal
		sl, err := h.lookupSlot("review1")
		if err != nil {
			h.t.Error(err)
		}
		at = append(at, s.Role+"@"+deref(sl.CheckedOutSHA))
	}
	h.ag.mu.Unlock()
	return &at
}

// liveRole reports whether PR #n's role has a live session. Round scripts
// call it, so a PR it cannot read is an error, not the test's end.
func liveRole(h *harness, n int, role string) bool {
	h.t.Helper()
	pr, err := h.lookupPR(n)
	if err != nil {
		h.t.Error(err)
		return false
	}
	s, err := h.st.LiveSessionByPRRole(h.ctx, pr.ID, role)
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
		if err := h.pushed(2, "b2"); err != nil {
			return failRound(t, err)
		}
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

// projectGit answers ChangedNames per head (every file the head changes
// since its merge base with the base) and records each comparison as
// "<base>...<head>".
type projectGit struct {
	fakeGit
	mu      sync.Mutex
	changed map[string][]string
	err     error
	calls   []string
}

func (g *projectGit) ChangedNames(_ context.Context, _, base, head string) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, base+"..."+head)
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

// A new head that leaves the reloaded project config alone brings the
// session nothing new to reload, so magnum does not quit it: a quit that
// cannot stop the agent (its MCP servers still in the pane's foreground)
// held such a round for good. The checkout's fetch runs first, once, and
// the head it fetched decides: by the PR's file list when it is complete
// and for that head, else by git (every file the head changes since its
// merge base with the base branch, matched to .claude, .mcp.json and the
// instruction files), which a PR of more files than the list holds needs.
// A head that changes the config, a git that cannot tell, and a fetch that
// fails still quit the session.
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
			changed: map[string][]string{"b2": {"docs/AGENTIC.md"}}, wantGit: "origin/master...b2"},
		{name: "cut-off list, git finds the config alone", files: untouched, truncated: true,
			wantGit: "origin/master...b2"},
		{name: "cut-off list, git finds a change", files: untouched, truncated: true,
			changed: map[string][]string{"b2": {".claude/settings.json"}}, wantQuit: true, wantGit: "origin/master...b2"},
		{name: "cut-off list, git fails", files: untouched, truncated: true, gitErr: errors.New("fatal: bad object"),
			wantQuit: true, wantGit: "origin/master...b2"},
		{name: "no list, git finds the config alone", wantGit: "origin/master...b2"},
		{name: "list for another head than the fetched one", files: untouched, moveHead: "b3",
			changed: map[string][]string{"b3": {".mcp.json"}}, wantQuit: true, wantGit: "origin/master...b3"},
		// The checkout's own fetch may then bring a newer head than the
		// radar's list is of: no head fetched, no proof.
		{name: "the fetch fails", files: untouched, fetchErr: errors.New("fetch: network down"), wantQuit: true},
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

// A link of the config a session loaded leads elsewhere (the base's
// .claude/skills -> ../.agents/skills): a new head that changes a file
// there changes what the session reloads, though its list names nothing
// under .claude/. The session is quit when the list, or git for a list cut
// short, changes a link's target, as the slot's disk has the links (the
// head the session loaded through) or the fetched head's tree has them
// (one the base added since); a head that leaves the targets alone keeps
// it, and a slot whose links cannot all be read is no proof.
func TestALiveClaudeSessionIsParkedWhenTheNewHeadChangesALinksTarget(t *testing.T) {
	skill := []string{"app/x.rb", ".agents/skills/x/SKILL.md"}
	tree := map[string]map[string]string{"b2": {".claude/skills": "../.agents/skills"}}
	for _, c := range []struct {
		name      string
		files     []string
		truncated bool
		tree      map[string]map[string]string
		imports   map[string]map[string]string // the tree's files, by head
		disk      bool                         // the slot has .claude/skills -> ../.agents/skills on disk
		changed   map[string][]string
		wantQuit  bool
		wantGit   string
	}{
		{name: "a target in the fetched head's tree", files: skill, tree: tree, wantQuit: true},
		{name: "a target on the slot's disk", files: skill, disk: true, wantQuit: true},
		{name: "a target the head leaves alone", files: []string{"app/x.rb", ".agents/skillsx/a.md"}, tree: tree, disk: true},
		// A path on the way to a target may become a link that leads
		// anywhere: .agentſ, which APFS opens for .agents, -> evil.
		{name: "a path on the way to a target", files: []string{".agentſ", "evil/skills/x/SKILL.md"}, disk: true, wantQuit: true},
		{name: "a file the head's instructions import", files: []string{"app/x.rb", ".docs/guide.md"},
			imports: map[string]map[string]string{"b2": {"CLAUDE.md": "@.docs/guide.md\n"}}, wantQuit: true},
		{name: "a cut-off list, git finds a target changed", files: []string{"app/x.rb"}, truncated: true, disk: true,
			changed: map[string][]string{"b2": {".agents/skills/x/SKILL.md"}}, wantQuit: true,
			wantGit: "origin/master...b2"},
		// git's icase pathspec folds ASCII only, while macOS writes
		// .agents/ſkills/pwn/ into .agents/skills/pwn/.
		{name: "a cut-off list, git finds a target changed under another spelling", files: []string{"app/x.rb"}, truncated: true, disk: true,
			changed: map[string][]string{"b2": {".agents/ſkills/pwn/SKILL.md"}}, wantQuit: true,
			wantGit: "origin/master...b2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := &projectGit{fakeGit: fakeGit{links: c.tree, files: c.imports}, changed: c.changed}
			h := newHarness(t, func(h *harness) { h.d.Git = g })
			h.reviewedPR(2, "b1")
			if c.disk {
				dir := t.TempDir()
				if err := os.MkdirAll(filepath.Join(dir, ".agents", "skills", "x"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../.agents/skills", filepath.Join(dir, ".claude", "skills")); err != nil {
					t.Fatal(err)
				}
				sl := h.slot("review1")
				if err := h.st.UpdateSlotFields(h.ctx, sl.ID, func(u *store.SlotUpdate) { u.Set("path", dir) }); err != nil {
					t.Fatal(err)
				}
			}
			at := reloadingClaude(h)
			h.advance(30 * time.Minute)
			h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2", files: c.files, filesTruncated: c.truncated})
			h.tick()
			h.advance(5 * time.Minute)
			h.tick()
			if n := len(h.rd.all()); n != 2 {
				t.Fatalf("rounds = %d, want the re-review", n)
			}
			if quit := len(*at) > 0; quit != c.wantQuit {
				t.Fatalf("quits = %v, want a quit: %v", *at, c.wantQuit)
			}
			if calls := g.all(); c.wantGit == "" && len(calls) > 0 || c.wantGit != "" && !slices.Equal(calls, []string{c.wantGit}) {
				t.Fatalf("git comparisons = %q, want %q", calls, c.wantGit)
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
		if err := h.pushed(2, "b2"); err != nil {
			return failRound(t, err)
		}
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
	if calls := g.all(); !slices.Equal(calls, []string{"origin/master...b2"}) {
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

// A stacked PR's sessions compare the project config with the default
// branch (agents.ProjectBase), since its base branch is another PR's head:
// a new head that merges what that branch's own .claude/ changes lists
// nothing under .claude/ against the PR's base. The reload check compares
// the new head with the same base as the session, by git, whatever the
// PR's list against its base branch says; and so for a PR retargeted from
// the default branch to another since its session started.
func TestAReloadCheckOfAStackedPRComparesWithTheDefaultBranch(t *testing.T) {
	untouched := []string{"app/models/order.rb"}
	for _, c := range []struct {
		name       string
		retargeted bool
		changed    map[string][]string
		wantQuit   bool
	}{
		{name: "stacked, the base branch's config merged", changed: map[string][]string{"b2": {".claude/settings.json"}}, wantQuit: true},
		{name: "stacked, the config alone"},
		{name: "retargeted, the base branch's config merged", retargeted: true, changed: map[string][]string{"b2": {".claude/settings.json"}},
			wantQuit: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := &projectGit{changed: c.changed}
			h := newHarness(t, func(h *harness) { h.d.Git = g })
			first := prSpec{n: 2, head: "b1", base: "feature-a"}
			if c.retargeted {
				first.base = ""
			}
			h.open(prSpec{n: 1, head: "base1"})
			h.startup()
			h.tick()
			h.open(prSpec{n: 1, head: "base1"}, first)
			h.tick()
			h.advance(5 * time.Minute)
			h.tick()
			h.wantState(2, store.PRReviewed)
			at := reloadingClaude(h)
			h.advance(30 * time.Minute)
			h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2", base: "feature-a", files: untouched})
			h.tick()
			h.advance(5 * time.Minute)
			h.tick()
			if n := len(h.rd.all()); n != 2 {
				t.Fatalf("rounds = %d, want the re-review", n)
			}
			if quit := len(*at) > 0; quit != c.wantQuit {
				t.Fatalf("quits = %v, want a quit: %v", *at, c.wantQuit)
			}
			if calls := g.all(); !slices.Equal(calls, []string{"origin/master...b2"}) {
				t.Fatalf("git comparisons = %q, want the default branch's", calls)
			}
		})
	}
}

// lackingGit's clone lacks the heads in lacks: their trees cannot be read
// until the test drops them (a fetch brought them).
type lackingGit struct {
	projectGit
	lacks sync.Map // head -> true
}

func (g *lackingGit) Tree(ctx context.Context, dir, rev string, keep func(string) bool) (gitx.Tree, error) {
	if _, ok := g.lacks.Load(rev); ok {
		return gitx.Tree{}, fmt.Errorf("gitx: ls-tree -r %s: exit status 128", rev)
	}
	return g.projectGit.Tree(ctx, dir, rev, keep)
}

// A head the clone lacked when a tick asked its links (a fork's, before
// any fetch) fails, and configLinks keeps that failure a while: the reload
// check's own fetch, or the checkout's, brings the head, and from then on
// its links are read again rather than taken as unreadable, which would
// quit every session that reloads the config and post a comment in place
// of the approval.
func TestAReloadCheckReadsAHeadAgainOnceItsFetchBringsIt(t *testing.T) {
	g := &lackingGit{}
	h := newHarness(t, func(h *harness) { h.d.Git = g })
	h.reviewedPR(2, "b1")
	at := reloadingClaude(h)
	h.advance(30 * time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2", files: []string{"app/models/order.rb"}})
	h.tick()
	h.advance(5 * time.Minute)
	g.lacks.Store("b2", true)
	pr := h.pr(2)
	if _, err := h.e.configLinks(h.ctx, pr); pr.HeadSHA != "b2" || err == nil {
		t.Fatalf("configLinks of %s, a head the clone lacks: %v, want an error", pr.HeadSHA, err)
	}
	g.lacks.Delete("b2")
	h.tick()
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d, want the re-review", n)
	}
	if len(*at) > 0 {
		t.Fatalf("quits = %v: the fetch brought b2, whose list leaves the config alone", *at)
	}
}

// The reload check takes the PR's stored file list only when it was read
// against the base branch the PR names now: a head whose list was read
// while the PR targeted release, and which the PR has targeted the default
// branch with since, without a push, may bring release's own .claude/
// changes, which a list against release does not name. git then decides,
// against the session's base, and the session is quit.
func TestAReloadCheckDoesNotDecideOnAFileListOfAnotherBase(t *testing.T) {
	g := &projectGit{changed: map[string][]string{"b2": {"app/models/order.rb", ".claude/settings.json"}}}
	h := newHarness(t, func(h *harness) { h.d.Git = g })
	h.reviewedPR(2, "b1")
	at := reloadingClaude(h)
	h.advance(30 * time.Minute)
	untouched := []string{"app/models/order.rb"}
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2", base: "release", files: untouched})
	h.tick()
	h.gh.fail(errors.New("GitHub is down"), nil)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2", files: untouched})
	h.advance(5 * time.Minute)
	h.tick()
	if f, ok, err := h.st.PRFilesOf(h.ctx, h.pr(2).ID); err != nil || !ok || f.BaseRef != "release" || f.HeadSHA != "b2" {
		t.Fatalf("file list = %+v, %v, %v; want b2's against release", f, ok, err)
	}
	if n := len(h.rd.all()); n != 2 {
		t.Fatalf("rounds = %d, want the re-review", n)
	}
	if len(*at) == 0 {
		t.Fatal("no quit: the list of another base decided")
	}
	if calls := g.all(); !slices.Equal(calls, []string{"origin/master...b2"}) {
		t.Fatalf("git comparisons = %q, want the default branch's", calls)
	}
}
