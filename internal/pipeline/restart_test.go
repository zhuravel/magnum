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
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

const (
	head2 = "2222222222222222222222222222222222222222"
	head3 = "3333333333333333333333333333333333333333"
	head4 = "4444444444444444444444444444444444444444"
)

// switcher is a RoundInput.Switch that records the heads it was asked for.
type switcher struct {
	mu    sync.Mutex
	calls []string
	cur   string // the head checked out
	err   error
}

func (s *switcher) Switch(ctx context.Context, sha string) (Switched, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, sha)
	if s.err != nil {
		return Switched{}, s.err
	}
	s.cur = sha
	return Switched{TargetSHA: sha, BaseSHA: "base-" + sha[:7]}, nil
}

func (s *switcher) head() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

func (s *switcher) asked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// withRestarts sets in up for restarts (at most max) and makes codex-review
// write its report into the directory of the head checked out, and an
// interrupted agent go idle (as Observe records it).
func (e *env) withRestarts(in *RoundInput, max int) *switcher {
	sw := &switcher{cur: in.TargetSHA}
	in.MaxRestarts, in.Switch = max, sw.Switch
	e.ag.codex = func(f *fakeAgents, c codexCall) error {
		dir := e.layout.ReviewDir("talkable", "talkable", 11920, sw.head())
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "codex-review.md"), []byte("[P2] codex finding\n"), 0o600)
	}
	e.keys.onAgent = func(target string, keys []string) {
		for _, s := range e.sess {
			if store.Deref(s.AgentName) == target {
				_ = e.st.UpdateSession(e.ctx, s.ID, func(u *store.SessionUpdate) { u.Set("agent_status", "idle") })
			}
		}
	}
	return sw
}

// push records a new PR head, as the poller does for a push.
func (e *env) push(sha string) {
	e.t.Helper()
	if err := e.st.UpdatePR(e.ctx, e.pr.ID, func(u *store.PRUpdate) { u.Set("head_sha", sha) }); err != nil {
		e.t.Fatalf("push %s: %v", textx.ShortSHA(sha), err)
	}
}

// pushThen pushes sha while the agent works, then behaves as next.
func pushThen(e *env, sha string, next behavior) behavior {
	return func(f *fakeAgents, run store.Run, text string) error {
		e.push(sha)
		return next(f, run, text)
	}
}

func (e *env) dirOf(sha string) string { return e.layout.ReviewDir("talkable", "talkable", 11920, sha) }

// waitRun waits until role's run on head reaches state (a reviewer finishes
// its run in its own goroutine, so another role's behavior may wait for it).
func (e *env) waitRun(role agents.Role, head, state string) {
	e.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		for _, r := range e.runs() {
			if r.Role == string(role) && r.TargetSHA == head && r.State == state {
				return
			}
		}
	}
	e.t.Errorf("%s never reached %s on %s", role, state, textx.ShortSHA(head))
}

func (e *env) eventsOf(kind string) []store.Event {
	var out []store.Event
	for _, ev := range e.events() {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func TestPushDuringReviewersRestartsOnTheNewHead(t *testing.T) {
	e := newEnv(t)
	in := e.input(KindInitial)
	sw := e.withRestarts(&in, 2)
	// The push lands while claude's turn is in flight and after codex-review
	// finished on the old head (the reviewers run in parallel: a push before
	// codex starts abandons its pending run instead, which the scheduler would
	// otherwise pick at random).
	pushAfterCodex := func(f *fakeAgents, run store.Run, text string) error {
		e.waitRun(agents.RoleCodexReview, target, store.RunVerified)
		e.push(head2)
		return nil // the turn stays in flight until the push cuts it
	}
	e.ag.behaviors[agents.RoleClaude] = []behavior{pushAfterCodex, writeReport("## P2 on the new head\n")}
	post := e.judgePosts(601, "COMMENTED", "COMMENT")
	post.commit = head2
	e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(t)}

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.TargetSHA != head2 || res.Restarts != 1 || res.ReportDir != e.dirOf(head2) {
		t.Fatalf("result = %+v", res)
	}
	if got := sw.asked(); !slices.Equal(got, []string{head2}) {
		t.Errorf("switches = %v, want [%s]", got, textx.ShortSHA(head2))
	}
	claudeAgent := agents.AgentName("talkable/talkable", 11920, agents.RoleClaude)
	if !slices.Contains(e.keys.sends, "agent:"+claudeAgent+":esc") {
		t.Errorf("claude-review was not interrupted: %v", e.keys.sends)
	}

	// The same claude session is re-prompted with its restart prompt; codex
	// simply runs again.
	claude := e.ag.submitsFor(agents.RoleClaude)
	if len(claude) != 2 {
		t.Fatalf("claude submits = %d, want 2", len(claude))
	}
	mustContain(t, "claude restart prompt", claude[1].Text, "moved from `"+target+"` to `"+head2+"`",
		filepath.Join(e.dirOf(head2), "claude-review.md"), "git diff "+target+".."+head2)
	if codex := e.ag.shellCallsFor(agents.RoleCodexReview); len(codex) != 2 || !strings.Contains(codex[1].Script, e.dirOf(head2)) {
		t.Errorf("codex calls = %+v", codex)
	}
	judge := e.ag.submitsFor(agents.RoleJudge)
	if len(judge) != 1 {
		t.Fatalf("judge submits = %d, want 1", len(judge))
	}
	mustContain(t, "judge prompt", judge[0].Text, "head_sha: "+head2, "base_sha: base-"+head2[:7],
		"claude-review: "+filepath.Join(e.dirOf(head2), "claude-review.md"),
		"codex-review: "+filepath.Join(e.dirOf(head2), "codex-review.md"),
		"result_file: "+filepath.Join(e.dirOf(head2), "codex-judge.json"))

	// Runs: the cut claude turn and the unprompted judge run are abandoned;
	// codex finished on the old head; the new attempt runs on head2.
	byHead := map[string][]store.Run{}
	for _, r := range e.runs() {
		byHead[r.TargetSHA] = append(byHead[r.TargetSHA], r)
	}
	for _, r := range byHead[target] {
		switch r.Role {
		case string(agents.RoleClaude), string(agents.RoleJudge):
			if r.State != store.RunAbandoned || store.Deref(r.Outcome) != ReportHeadMoved || !strings.Contains(store.Deref(r.Error), textx.ShortSHA(head2)) {
				t.Errorf("old %s run = %s / %q / %q", r.Role, r.State, store.Deref(r.Outcome), store.Deref(r.Error))
			}
		case string(agents.RoleCodexReview):
			if r.State != store.RunVerified {
				t.Errorf("old codex run = %s, want verified (it finished before the push)", r.State)
			}
		}
	}
	if len(byHead[head2]) != 3 {
		t.Fatalf("runs on head2 = %d, want 3", len(byHead[head2]))
	}
	for _, r := range byHead[head2] {
		if r.State != store.RunVerified || r.Round != 1 || r.Kind != store.RunInitial {
			t.Errorf("new %s run = %s round %d kind %s", r.Role, r.State, r.Round, r.Kind)
		}
	}
	restarted := e.eventsOf("round.restarted")
	if len(restarted) != 1 {
		t.Fatalf("round.restarted events = %d", len(restarted))
	}
	mustContain(t, "round.restarted", restarted[0].Message, "from "+textx.ShortSHA(target)+" to "+textx.ShortSHA(head2), "restart 1 of 2", "cut short: claude-review")
}

func TestThirdPushDoesNotRestart(t *testing.T) {
	e := newEnv(t)
	in := e.input(KindInitial)
	sw := e.withRestarts(&in, 2)
	e.ag.behaviors[agents.RoleClaude] = []behavior{
		pushThen(e, head2, hang()),
		pushThen(e, head3, hang()),
		pushThen(e, head4, writeReport("## P2\n")), // the third push: no restart left
	}
	post := e.judgePosts(602, "COMMENTED", "COMMENT")
	post.commit = head3
	e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(t)}

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.TargetSHA != head3 || res.Restarts != 2 {
		t.Fatalf("result = %+v", res)
	}
	if got := sw.asked(); !slices.Equal(got, []string{head2, head3}) {
		t.Errorf("switches = %v", got)
	}
	if n := len(e.ag.submitsFor(agents.RoleClaude)); n != 3 {
		t.Errorf("claude submits = %d, want 3", n)
	}
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "head_sha: "+head3)
	if n := len(e.eventsOf("round.restarted")); n != 2 {
		t.Errorf("round.restarted events = %d, want 2", n)
	}
}

func TestRestartCountResetsPerRound(t *testing.T) {
	e := newEnv(t)
	post := func(id int64, commit string) behavior {
		p := e.judgePosts(id, "COMMENTED", "COMMENT")
		p.commit = commit
		return p.behavior(t)
	}
	e.ag.behaviors[agents.RoleClaude] = []behavior{
		pushThen(e, head2, hang()), writeReport("## round 1\n"),
		pushThen(e, head3, hang()), writeReport("## round 2\n"),
	}
	e.ag.behaviors[agents.RoleJudge] = []behavior{post(603, head2), post(604, head3)}

	in := e.input(KindInitial)
	e.withRestarts(&in, 1)
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Restarts != 1 || res.TargetSHA != head2 {
		t.Fatalf("round 1 = %+v, %v", res, err)
	}

	pr, err := e.st.PRByID(e.ctx, e.pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	in2 := RoundInput{PR: pr, Repo: e.repo, SlotPath: slotPath, Round: 2, Kind: KindRereview, TargetSHA: head2,
		BaseRef: "master", Previous: &PreviousReview{ID: 603, Event: "COMMENTED", SHA: head2}}
	e.withRestarts(&in2, 1)
	res, err = e.r.RunRound(e.ctx, in2)
	if err != nil || res.Restarts != 1 || res.TargetSHA != head3 {
		t.Fatalf("round 2 = %+v, %v", res, err)
	}
	claude := e.ag.submitsFor(agents.RoleClaude)
	// Round 2's restart prompt is confined to the delta since the previous review.
	mustContain(t, "round 2 restart prompt", claude[3].Text, "moved from `"+head2+"` to `"+head3+"`",
		"review only `git diff "+head2+".."+head3+"`", "do not re-derive a finding from that report")
}

func TestPushBetweenStagesRestartsBeforeTheJudge(t *testing.T) {
	e := newEnv(t)
	in := e.input(KindInitial)
	sw := e.withRestarts(&in, 2)
	// claude-review finishes; the push lands before the judge is prompted.
	e.ag.behaviors[agents.RoleClaude] = []behavior{pushThen(e, head2, writeReport("## old head\n")), writeReport("## new head\n")}
	post := e.judgePosts(605, "COMMENTED", "COMMENT")
	post.commit = head2
	e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(t)}

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.TargetSHA != head2 || res.Restarts != 1 || len(sw.asked()) != 1 {
		t.Fatalf("result = %+v, switches %v", res, sw.asked())
	}
	if len(e.keys.sends) != 0 {
		t.Errorf("nothing was in flight, yet keys were sent: %v", e.keys.sends)
	}
	ev := e.eventsOf("round.restarted")
	if len(ev) != 1 || strings.Contains(ev[0].Message, "cut short") {
		t.Errorf("round.restarted = %+v", ev)
	}
}

// A push cuts the stage short before its check: the restart settles the cut
// roles, then catches the edit one of them left and restores the checkout
// before it switches to the new head.
func TestPushRestoresACheckoutAReviewerEditedBeforeTheRestart(t *testing.T) {
	e := newEnv(t)
	in := e.input(KindInitial)
	in.Requested = []string{"simplify"}
	sw := e.withRestarts(&in, 2)
	var dirtyAtSwitch bool
	in.Switch = func(ctx context.Context, sha string) (Switched, error) {
		e.git.mu.Lock()
		dirtyAtSwitch = e.git.status.Dirty()
		e.git.head = sha // the slot's checkout moves with the switch
		e.git.mu.Unlock()
		return sw.Switch(ctx, sha)
	}
	e.restoreRules(false)
	edit := func(f *fakeAgents, run store.Run, text string) error {
		e.git.mu.Lock()
		e.git.status = gitx.Status{Tracked: 1}
		e.git.mu.Unlock()
		return nil
	}
	e.ag.behaviors[agents.RoleSimplify] = []behavior{pushThen(e, head2, edit), writeReport("No proposals.\n")}
	post := e.judgePosts(611, "COMMENTED", "COMMENT")
	post.commit = head2
	e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(t)}

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.TargetSHA != head2 || res.Restarts != 1 {
		t.Fatalf("result = %+v", res)
	}
	simplifyAgent := agents.AgentName("talkable/talkable", 11920, agents.RoleSimplify)
	if n := slices.Index(e.keys.sends, "agent:"+simplifyAgent+":esc"); n < 0 {
		t.Errorf("claude-simplify was not interrupted: %v", e.keys.sends)
	}
	if dirtyAtSwitch {
		t.Error("the checkout switched to the new head while still modified")
	}
	if n := len(e.exec.CallsWithPrefix("git", "-C", slotPath, "reset", "--hard", "--quiet")); n != 1 {
		t.Errorf("tree restores = %d, want 1 (before the switch)", n)
	}
	if ev := e.eventsOf("round.checkout_dirty"); len(ev) != 1 || !strings.Contains(ev[0].Message, "claude-simplify") {
		t.Errorf("round.checkout_dirty = %+v", ev)
	}
	// Every role ran again on head2: both reviewers and the simplifier.
	if n := len(e.ag.submitsFor(agents.RoleClaude)); n != 2 {
		t.Errorf("claude submits = %d, want 2", n)
	}
	if n := len(e.ag.submitsFor(agents.RoleSimplify)); n != 2 {
		t.Errorf("simplify submits = %d, want 2", n)
	}
	for _, r := range e.runs() {
		if r.Role == string(agents.RoleSimplify) && r.TargetSHA == target && r.State != store.RunAbandoned {
			t.Errorf("the cut simplify run is %s", r.State)
		}
	}
}

func TestPushWithoutRestartsKeepsTheHead(t *testing.T) {
	e := newEnv(t)
	in := e.input(KindInitial) // MaxRestarts 0: the default of a caller that does not opt in
	e.ag.behaviors[agents.RoleClaude] = []behavior{pushThen(e, head2, writeReport("## P2\n"))}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(606, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted || res.TargetSHA != target || res.Restarts != 0 {
		t.Fatalf("result = %+v", res)
	}
}

func TestRadarCatchingUpWithTheCheckoutIsNoPush(t *testing.T) {
	e := newEnv(t)
	// The checkout fetched target before the radar saw it: the PR row still
	// had the older head, which the poller then replaces with target.
	e.push(prevSHA)
	pr, _ := e.st.PRByID(e.ctx, e.pr.ID)
	in := e.input(KindInitial)
	in.PR, in.DispatchedHead = pr, prevSHA
	sw := e.withRestarts(&in, 2)
	e.ag.behaviors[agents.RoleClaude] = []behavior{pushThen(e, target, writeReport("## P2\n"))}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(607, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Restarts != 0 || len(sw.asked()) != 0 {
		t.Fatalf("result = %+v, %v; switches %v", res, err, sw.asked())
	}
}

func TestPushDuringCheckoutBeforeTheRoundStartedRestarts(t *testing.T) {
	e := newEnv(t)
	// Dispatched at prevSHA; the checkout fetched target; the poller then
	// recorded head2, pushed after the fetch, before the round began.
	e.push(head2)
	pr, _ := e.st.PRByID(e.ctx, e.pr.ID)
	in := e.input(KindInitial)
	in.PR, in.DispatchedHead = pr, prevSHA
	sw := e.withRestarts(&in, 2)
	post := e.judgePosts(608, "COMMENTED", "COMMENT")
	post.commit = head2
	e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(t)}

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Restarts != 1 || res.TargetSHA != head2 || !slices.Equal(sw.asked(), []string{head2}) {
		t.Fatalf("result = %+v, %v; switches %v", res, err, sw.asked())
	}
	// Noticed before the first stage: nobody was prompted on the old head.
	if claude := e.ag.submitsFor(agents.RoleClaude); len(claude) != 1 || claude[0].Run.TargetSHA != head2 {
		t.Errorf("claude submits = %+v", claude)
	}
	if n := len(e.ag.shellCallsFor(agents.RoleCodexReview)); n != 1 {
		t.Errorf("codex calls = %d, want 1", n)
	}
	for _, r := range e.runs() {
		if r.TargetSHA == target && (r.State != store.RunAbandoned || r.SubmittedAt != nil) {
			t.Errorf("old %s run = %s (submitted %v)", r.Role, r.State, r.SubmittedAt)
		}
	}
}

func TestAnOlderHeadIsNoPush(t *testing.T) {
	e := newEnv(t)
	// The poller reports an intermediate head after the checkout fetched
	// target past it: an ancestor of the target is no push.
	e.git.ancestors = map[string]bool{head3: true}
	in := e.input(KindInitial)
	sw := e.withRestarts(&in, 2)
	e.ag.behaviors[agents.RoleClaude] = []behavior{pushThen(e, head3, writeReport("## P2\n"))}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(612, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Restarts != 0 || res.TargetSHA != target || len(sw.asked()) != 0 {
		t.Fatalf("result = %+v, %v; switches %v", res, err, sw.asked())
	}
}

func TestRestartSwitchFailureEndsTheRound(t *testing.T) {
	e := newEnv(t)
	in := e.input(KindInitial)
	sw := e.withRestarts(&in, 2)
	sw.err = errors.New("fetch refs/pull/11920/head: connection reset")
	e.ag.behaviors[agents.RoleClaude] = []behavior{pushThen(e, head2, hang())}

	res, err := e.r.RunRound(e.ctx, in)
	if err == nil || res.Outcome != OutcomeError || !strings.Contains(res.Error, "restart on "+textx.ShortSHA(head2)) {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if n := len(e.ag.submitsFor(agents.RoleJudge)); n != 0 {
		t.Errorf("judge prompted %d times", n)
	}
	for _, r := range e.runs() {
		if r.State == store.RunPending || r.State == store.RunWorking || r.State == store.RunSubmitted {
			t.Errorf("run %s %s left %s", r.ID, r.Role, r.State)
		}
	}
}

func TestRereviewEffort(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleJudge] = []behavior{
		e.judgePosts(609, "COMMENTED", "COMMENT").behavior(t),
		e.judgePosts(610, "COMMENTED", "COMMENT").behavior(t),
	}
	if _, err := e.r.RunRound(e.ctx, e.input(KindInitial)); err != nil {
		t.Fatal(err)
	}
	in := e.input(KindRereview)
	in.Round, in.Previous = 2, &PreviousReview{ID: 609, Event: "COMMENTED", SHA: prevSHA}
	if _, err := e.r.RunRound(e.ctx, in); err != nil {
		t.Fatal(err)
	}
	judge := e.ag.submitsFor(agents.RoleJudge)
	const line = "Work at high reasoning effort"
	if strings.Contains(judge[0].Text, line) {
		t.Errorf("the first review (xhigh, its launch effort) names an effort:\n%s", judge[0].Text)
	}
	// codex sets the effort at launch only: the prompt asks for rereview_effort.
	mustContain(t, "judge rereview prompt", judge[1].Text, line)
	// claude gets its effort twice: at launch (--effort, like codex, so the
	// session never runs at the operator's own effortLevel) and as
	// /code-review's argument. A re-review keeps the session's launch effort,
	// so its prompt also asks for rereview_effort, as codex's does.
	claude := e.ag.submitsFor(agents.RoleClaude)
	mustContain(t, "claude initial prompt", claude[0].Text, "/code-review https://github.com/talkable/talkable/pull/11920 high\n")
	if strings.Contains(claude[0].Text, "Work at") {
		t.Errorf("the first claude review (high, its launch effort) names an effort:\n%s", claude[0].Text)
	}
	mustContain(t, "claude rereview prompt", claude[1].Text, "/code-review https://github.com/talkable/talkable/pull/11920 medium\n",
		"git diff "+prevSHA+".."+target, "Work at medium reasoning effort")
}

func TestAppendToReview(t *testing.T) {
	e := newEnv(t)
	body := "**Verdict**\n<!-- magnum:run=r-1 head=abc1234 -->\n"
	e.gh.add(github.Review{DatabaseID: 700, Body: body}, github.RESTReview{ID: 700, UserLogin: "talkable[bot]", UserType: "Bot", Body: body})
	e.gh.add(github.Review{DatabaseID: 701, Body: "theirs"}, github.RESTReview{ID: 701, UserLogin: "someone", UserType: "User", Body: "theirs"})
	line := "_Reviewed abc1234; 2 commits arrived during the review, re-review follows._"

	for range 2 { // the second call finds the line already there
		if err := e.r.AppendToReview(e.ctx, "talkable", "talkable", 11920, 700, line); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.gh.updates) != 1 || e.gh.updates[0].Body != strings.TrimRight(body, "\n")+"\n\n"+line {
		t.Fatalf("updates = %+v", e.gh.updates)
	}
	if err := e.r.AppendToReview(e.ctx, "talkable", "talkable", 11920, 701, line); err == nil || len(e.gh.updates) != 1 {
		t.Errorf("another login's review: err %v, updates %d", err, len(e.gh.updates))
	}
	e.gh.updateErr = fmt.Errorf("update: %w", github.ErrForbidden)
	e.gh.add(github.Review{DatabaseID: 702, Body: "x"}, github.RESTReview{ID: 702, UserLogin: "talkable[bot]", Body: "x"})
	if err := e.r.AppendToReview(e.ctx, "talkable", "talkable", 11920, 702, line); !errors.Is(err, github.ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
}
