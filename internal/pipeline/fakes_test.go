package pipeline

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/identity"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// Compile-time checks: the real clients satisfy the interfaces.
var (
	_ Agents = (*agents.Manager)(nil)
	_ GitHub = (*github.Client)(nil)
	_ Git    = (*gitx.Client)(nil)
	_ Keys   = (*herdr.Client)(nil)
)

var t0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

const (
	target   = "abc1234def5678abc1234def5678abc1234def56"
	prevSHA  = "0011223344556677889900112233445566778899"
	slotPath = "/Users/x/Projects/talkable.review1"
)

// testClock is a settable clock shared by the store and the runner; Sleep
// advances it.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *testClock) Add(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

// submitCall is one Submit the pipeline made.
type submitCall struct {
	Run  store.Run
	Role agents.Role
	Text string
}

// codexCall is one RunShell the pipeline made (any shell role).
type codexCall struct {
	Role                 agents.Role
	Pane, Script, Marker string
	Timeout              time.Duration
}

// behavior scripts what an agent does with a submitted prompt. It runs after
// the run was moved to working (as the real Submit does on a working ack).
type behavior func(f *fakeAgents, run store.Run, text string) error

// fakeAgents implements Agents. NewRun, RolePrompt and ShellLine are the real
// agents.Manager (store and config only); Submit and RunShell are scripted.
type fakeAgents struct {
	t  *testing.T
	st *store.Store
	m  *agents.Manager

	mu         sync.Mutex
	submits    []submitCall
	behaviors  map[agents.Role][]behavior             // consumed in order; the last one repeats
	codex      func(f *fakeAgents, c codexCall) error // codex-review, and shell roles without a shells entry
	shells     map[agents.Role]func(f *fakeAgents, c codexCall) error
	exitStatus map[agents.Role]int // RunShell's status per shell role (default 0)
	codexCalls []codexCall
	reads      map[agents.Role]string
	preflight  map[string]error
	preflights []string
	refuse     map[agents.Role]error // Submit returns it before sending (run stays pending)
	order      []string              // "submit:<role>" / "shell:<role>" in call order
	// blocked: the next n Submits of a role are rejected before sending
	// (herdr agent_blocked): run failed, agents.ErrBlocked.
	blocked map[agents.Role]int
	// switches are the SwitchModel calls; switchErr fails them.
	switches  []switchCall
	switchErr error
}

// switchCall is one SwitchModel the pipeline made.
type switchCall struct {
	Role, Model, Reason string
}

// NoteModelLimit, FallbackModel and FallbackPrompt are the real
// agents.Manager (store and config only).
func (f *fakeAgents) NoteModelLimit(ctx context.Context, s store.Session, h agents.Health) (agents.ModelLimit, error) {
	return f.m.NoteModelLimit(ctx, s, h)
}

func (f *fakeAgents) FallbackModel(ctx context.Context, s store.Session, tried []string) (string, bool) {
	return f.m.FallbackModel(ctx, s, tried)
}

func (f *fakeAgents) FallbackPrompt(d agents.FallbackData) (string, error) {
	return f.m.FallbackPrompt(d)
}

// SwitchModel records the call and the session's model, as the real one
// does after typing the command; switchErr fails it.
func (f *fakeAgents) SwitchModel(ctx context.Context, s store.Session, model, reason string) error {
	f.mu.Lock()
	f.switches = append(f.switches, switchCall{Role: s.Role, Model: model, Reason: reason})
	f.order = append(f.order, "switch:"+s.Role+":"+model)
	err := f.switchErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	return f.st.SetKV(ctx, agents.KVSessionModel(s.ID), model)
}

func (f *fakeAgents) NewRun(ctx context.Context, pr store.PR, role agents.Role, kind string, round int) (store.Run, error) {
	return f.m.NewRun(ctx, pr, role, kind, round)
}

func (f *fakeAgents) Submit(ctx context.Context, run store.Run, text string) error {
	role := agents.Role(run.Role)
	f.mu.Lock()
	f.submits = append(f.submits, submitCall{Run: run, Role: role, Text: text})
	f.order = append(f.order, "submit:"+run.Role)
	if f.blocked[role] > 0 {
		f.blocked[role]--
		f.mu.Unlock()
		if err := f.st.TransitionRun(ctx, run.ID, []string{store.RunPending}, store.RunFailed, func(u *store.RunUpdate) {
			u.Set("error", "herdr agent.prompt: agent_blocked: agent is blocked")
			u.Set("ended_at", f.st.Clock())
		}); err != nil {
			return err
		}
		return fmt.Errorf("agents: prompt %s: %w", run.Role, agents.ErrBlocked)
	}
	var b behavior
	if q := f.behaviors[role]; len(q) > 0 {
		b = q[0]
		if len(q) > 1 {
			f.behaviors[role] = q[1:]
		}
	}
	refuse := f.refuse[role]
	f.mu.Unlock()
	if refuse != nil {
		return refuse
	}
	sess, err := f.st.LiveSessionByPRRole(ctx, run.PRID, run.Role)
	if err != nil {
		return fmt.Errorf("fake submit: %w", agents.ErrNoSession)
	}
	if err := f.st.TransitionRun(ctx, run.ID, []string{store.RunPending}, store.RunWorking, func(u *store.RunUpdate) {
		u.Set("session_id", sess.ID)
		u.Set("prompt_text", text)
		u.Set("submitted_at", f.st.Clock())
		u.Set("working_seen_at", f.st.Clock())
	}); err != nil {
		return err
	}
	if b == nil {
		return nil
	}
	return b(f, run, text)
}

func (f *fakeAgents) RunShell(ctx context.Context, pr store.PR, role config.Role, paneID, line, marker string, timeout time.Duration) (int, error) {
	c := codexCall{Role: agents.Role(role.Name), Pane: paneID, Script: line, Marker: marker, Timeout: timeout}
	f.mu.Lock()
	f.codexCalls = append(f.codexCalls, c)
	f.order = append(f.order, "shell:"+role.Name)
	fn := f.codex
	if g, ok := f.shells[c.Role]; ok {
		fn = g
	}
	status := f.exitStatus[c.Role]
	f.mu.Unlock()
	if fn == nil {
		return status, nil
	}
	if err := fn(f, c); err != nil {
		return agents.ShellStatusUnknown, err
	}
	return status, nil
}

func (f *fakeAgents) RolePrompt(role config.Role, promptKind string, data any) (string, error) {
	return f.m.RolePrompt(role, promptKind, data)
}

func (f *fakeAgents) ShellLine(role config.Role, d agents.ShellData) (string, error) {
	return f.m.ShellLine(role, d)
}

// shellCallsFor returns the RunShell calls of role.
func (f *fakeAgents) shellCallsFor(role agents.Role) []codexCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []codexCall
	for _, c := range f.codexCalls {
		if c.Role == role {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeAgents) ReadRecent(ctx context.Context, s store.Session, lines int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[agents.Role(s.Role)], nil
}

// PreflightRole records the role's agent kind; preflight[kind] (or
// preflight[<role name>]) is its error.
func (f *fakeAgents) PreflightRole(ctx context.Context, role config.Role) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	kind := role.AgentKind()
	f.preflights = append(f.preflights, kind)
	if err, ok := f.preflight[role.Name]; ok {
		return err
	}
	return f.preflight[kind]
}

func (f *fakeAgents) submitsFor(role agents.Role) []submitCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []submitCall
	for _, s := range f.submits {
		if s.Role == role {
			out = append(out, s)
		}
	}
	return out
}

// end moves a run to ended, as Observe does after two idle ticks.
func (f *fakeAgents) end(runID string) error {
	return f.st.TransitionRun(context.Background(), runID, []string{store.RunSubmitted, store.RunWorking}, store.RunEnded,
		func(u *store.RunUpdate) { u.Set("ended_at", f.st.Clock()) })
}

// writeReport writes the run's report file and ends the run.
func writeReport(content string) behavior {
	return func(f *fakeAgents, run store.Run, text string) error {
		path := store.Deref(run.ReportPath)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			return err
		}
		return f.end(run.ID)
	}
}

// hang leaves the run working forever.
func hang() behavior { return func(*fakeAgents, store.Run, string) error { return nil } }

// endSilently ends the turn without writing anything.
func endSilently() behavior {
	return func(f *fakeAgents, run store.Run, text string) error { return f.end(run.ID) }
}

var runIDLine = regexp.MustCompile(`run_id:? (r-[A-Za-z0-9._-]+)`)

// markerRunID is the run id the judge prompt asks the review to carry.
func markerRunID(t *testing.T, text string) string {
	t.Helper()
	m := runIDLine.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no run_id in prompt:\n%s", text)
	}
	return m[1]
}

// judgePost scripts the judge: post a review (GraphQL and REST views), write
// codex-judge.json, and optionally end the turn.
type judgePost struct {
	gh          *fakeGitHub
	reviewID    int64
	state       string // GraphQL state
	event       string // codex-judge.json event
	graphLogin  string
	graphType   string
	restLogin   string
	restType    string
	commit      string // "" = target
	noMarker    bool
	findings    map[string]int
	status      string // codex-judge.json status; "" = no file
	keepWorking bool
	body        string         // review body text before the marker ("" = a one-line verdict)
	after       string         // review body text after the marker (a footer, with what precedes it)
	extra       map[string]any // more codex-judge.json fields
}

func (p judgePost) behavior(t *testing.T) behavior {
	return func(f *fakeAgents, run store.Run, text string) error {
		id := markerRunID(t, text)
		commit := p.commit
		if commit == "" {
			commit = target
		}
		if p.gh != nil {
			body := cmp.Or(p.body, "**Verdict** findings") + "\n"
			if !p.noMarker {
				body += fmt.Sprintf("<!-- magnum:run=%s head=%s -->", id, commit[:7])
			}
			body += p.after
			rest := ""
			if p.after != "" {
				rest = body
			}
			p.gh.add(github.Review{DatabaseID: p.reviewID, State: p.state, Body: body,
				URL:         fmt.Sprintf("https://github.com/talkable/talkable/pull/11920#pullrequestreview-%d", p.reviewID),
				SubmittedAt: f.st.Clock(), CommitOid: commit, AuthorLogin: p.graphLogin, AuthorType: p.graphType},
				github.RESTReview{ID: p.reviewID, UserLogin: p.restLogin, UserType: p.restType, State: p.state, Body: rest,
					SubmittedAt: f.st.Clock(), CommitID: commit, HTMLURL: fmt.Sprintf("https://github.com/talkable/talkable/pull/11920#pullrequestreview-%d", p.reviewID)})
		}
		if p.status != "" {
			findings := p.findings
			if findings == nil {
				findings = map[string]int{}
			}
			result := map[string]any{
				"status": p.status, "run_id": id, "review_id": p.reviewID, "event": p.event,
				"review_url": "https://example.test/r", "findings": findings,
			}
			maps.Copy(result, p.extra)
			if err := writeJudgeJSON(store.Deref(run.ReportPath), result); err != nil {
				return err
			}
		}
		if p.keepWorking {
			return nil
		}
		return f.end(run.ID)
	}
}

func writeJudgeJSON(path string, v map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("{")
	first := true
	for k, val := range v {
		if !first {
			b.WriteString(",")
		}
		first = false
		switch x := val.(type) {
		case string:
			fmt.Fprintf(&b, "%q:%q", k, x)
		case int64:
			fmt.Fprintf(&b, "%q:%d", k, x)
		case map[string]int:
			fmt.Fprintf(&b, "%q:{", k)
			i := 0
			for fk, fv := range x {
				if i > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, "%q:%d", fk, fv)
				i++
			}
			b.WriteString("}")
		default:
			raw, err := json.Marshal(x)
			if err != nil {
				return fmt.Errorf("writeJudgeJSON: %T: %w", val, err)
			}
			fmt.Fprintf(&b, "%q:%s", k, raw)
		}
	}
	b.WriteString("}")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// fakeGitHub implements GitHub over in-memory reviews.
type fakeGitHub struct {
	mu          sync.Mutex
	reviews     []github.Review
	rest        map[int64]github.RESTReview
	markerCalls []string
	restCalls   []int64
	dismissed   []dismissCall
	dismissErr  error
	updates     []updateCall // UpdateReviewBody calls
	deleted     []int64      // DeletePendingReview calls
	comments    map[int64][]github.ReviewComment
	updateErr   error
	listErr     error
	failLists   int // the next n ReviewsWithMarker calls fail transiently
}

type dismissCall struct {
	ID      int64
	Message string
}

type updateCall struct {
	ID   int64
	Body string
}

func newFakeGitHub() *fakeGitHub { return &fakeGitHub{rest: map[int64]github.RESTReview{}} }

func (g *fakeGitHub) add(r github.Review, rest github.RESTReview) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reviews = append(g.reviews, r)
	g.rest[r.DatabaseID] = rest
}

func (g *fakeGitHub) ReviewsWithMarker(ctx context.Context, owner, repo string, number int, marker string) ([]github.Review, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.markerCalls = append(g.markerCalls, marker)
	if g.listErr != nil {
		return nil, g.listErr
	}
	if g.failLists > 0 {
		g.failLists--
		return nil, fmt.Errorf("gh api graphql: exit status 1: connection reset")
	}
	var out []github.Review
	for _, r := range g.reviews {
		if marker == "" || strings.Contains(r.Body, marker) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (g *fakeGitHub) ReviewREST(ctx context.Context, owner, repo string, number int, id int64) (github.RESTReview, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.restCalls = append(g.restCalls, id)
	r, ok := g.rest[id]
	if !ok {
		return github.RESTReview{}, fmt.Errorf("review %d: %w", id, github.ErrNotFound)
	}
	return r, nil
}

func (g *fakeGitHub) DismissReview(ctx context.Context, owner, repo string, number int, reviewID int64, message string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.dismissed = append(g.dismissed, dismissCall{reviewID, message})
	return g.dismissErr
}

// UpdateReviewBody records the call and, unless updateErr is set, replaces
// the review's body in both views.
func (g *fakeGitHub) UpdateReviewBody(ctx context.Context, owner, repo string, number int, reviewID int64, body string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.updates = append(g.updates, updateCall{reviewID, body})
	if g.updateErr != nil {
		return g.updateErr
	}
	for i := range g.reviews {
		if g.reviews[i].DatabaseID == reviewID {
			g.reviews[i].Body = body
		}
	}
	if r, ok := g.rest[reviewID]; ok {
		r.Body = body
		g.rest[reviewID] = r
	}
	return nil
}

func (g *fakeGitHub) calls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.markerCalls) + len(g.restCalls) + len(g.dismissed)
}

// fakeIdentity implements identity.Source.
type fakeIdentity struct {
	name, login, kind string
	env               map[string]string
	envErr            error
}

func (i fakeIdentity) Name() string  { return i.name }
func (i fakeIdentity) Login() string { return i.login }
func (i fakeIdentity) Kind() string  { return i.kind }
func (i fakeIdentity) Env(context.Context) (map[string]string, error) {
	return i.env, i.envErr
}
func (i fakeIdentity) Check(context.Context) (identity.Report, error) {
	return identity.Report{Pass: true}, nil
}

// fakeGit implements Git.
type fakeGit struct {
	mu       sync.Mutex
	head     string
	switches []string
	status   gitx.Status
	// ancestors are the commits MergeBase reports as ancestors of any
	// other; every other pair has no merge base.
	ancestors map[string]bool
}

func (g *fakeGit) MergeBase(ctx context.Context, dir, a, b string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if a == b || g.ancestors[a] {
		return a, nil
	}
	return "", fmt.Errorf("fake merge-base %s %s: %w", a, b, gitx.ErrNoSuchRef)
}

func (g *fakeGit) RevParse(ctx context.Context, dir, ref string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if ref != "HEAD" {
		return "", fmt.Errorf("fake rev-parse %s: %w", ref, gitx.ErrNoSuchRef)
	}
	return g.head, nil
}

func (g *fakeGit) SwitchDetach(ctx context.Context, dir, ref string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.switches = append(g.switches, ref)
	g.head = ref
	return nil
}

func (g *fakeGit) Status(ctx context.Context, dir string) (gitx.Status, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.status, nil
}

// fakeKeys implements Keys.
type fakeKeys struct {
	mu    sync.Mutex
	sends []string // "agent:<target>:<keys>" / "pane:<id>:<keys>"
	// onAgent runs after every AgentSendKeys (e.g. the agent goes idle).
	onAgent func(target string, keys []string)
	// onPane runs after every PaneSendKeys (e.g. ctrl+c ends a command).
	onPane func(paneID string, keys []string)
}

func (k *fakeKeys) AgentSendKeys(ctx context.Context, target string, keys ...string) error {
	k.mu.Lock()
	k.sends = append(k.sends, "agent:"+target+":"+strings.Join(keys, ","))
	hook := k.onAgent
	k.mu.Unlock()
	if hook != nil {
		hook(target, keys)
	}
	return nil
}

func (k *fakeKeys) PaneSendKeys(ctx context.Context, paneID string, keys ...string) error {
	k.mu.Lock()
	k.sends = append(k.sends, "pane:"+paneID+":"+strings.Join(keys, ","))
	hook := k.onPane
	k.mu.Unlock()
	if hook != nil {
		hook(paneID, keys)
	}
	return nil
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// env is everything a round test needs.
type env struct {
	t      *testing.T
	ctx    context.Context
	st     *store.Store
	clock  *testClock
	cfg    *config.Config
	layout paths.Layout
	repo   store.Repo
	pr     store.PR
	ag     *fakeAgents
	gh     *fakeGitHub
	git    *fakeGit
	exec   *execx.Fake
	keys   *fakeKeys
	log    *logSink
	r      *Runner
	sess   map[agents.Role]store.Session
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "state", "magnum.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	clk := &testClock{now: t0}
	st.Clock = clk.Now

	repo, err := st.UpsertRepo(ctx, store.Repo{NodeID: "R_1", Owner: "talkable", Name: "talkable", WatchOwner: "talkable",
		DefaultBranch: "master", Mode: store.RepoModePool})
	if err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	up, err := st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: "PR_11920", Number: 11920,
		URL: "https://github.com/talkable/talkable/pull/11920", HeadSHA: target, AuthorLogin: store.Ptr("someone"),
		Title: store.Ptr("Ignored title"), GHState: store.GHOpen, InitialState: store.PRReviewing, Identity: "talkable-app"})
	if err != nil {
		t.Fatalf("UpsertPRFromGitHub: %v", err)
	}

	cfg := config.Defaults()
	cfg.Identities = []config.Identity{
		{Name: "talkable-app", Kind: "app", Login: "talkable[bot]", NoFindingsEvent: "COMMENT", BlockingEvent: "REQUEST_CHANGES"},
		{Name: "zhuravel", Kind: "gh", Login: "zhuravel", NoFindingsEvent: "APPROVE"},
	}
	layout := paths.Layout{Home: t.TempDir()}
	cfg.Layout = layout
	for i := range cfg.Roles {
		switch r := &cfg.Roles[i]; {
		case r.Judge:
			r.Skill = "/repo/skills/magnum-review/SKILL.md"
		case r.Name == config.RoleClaudeSimplify:
			// As [claude] simplify = "never": only on request (Requested
			// "simplify"), so most scenarios run the two reviewers only.
			r.Runs = config.RunsManual
		}
	}

	e := &env{t: t, ctx: ctx, st: st, clock: clk, cfg: cfg, layout: layout, repo: repo, pr: up.PR,
		gh: newFakeGitHub(), git: &fakeGit{head: target}, exec: &execx.Fake{}, keys: &fakeKeys{}, log: &logSink{},
		sess: map[agents.Role]store.Session{}}
	e.ag = &fakeAgents{t: t, st: st, behaviors: map[agents.Role][]behavior{}, reads: map[agents.Role]string{},
		preflight: map[string]error{}, refuse: map[agents.Role]error{}, blocked: map[agents.Role]int{},
		exitStatus: map[agents.Role]int{},
		m:          agents.New(agents.Deps{Store: st, Config: cfg, Layout: layout, Clock: clk.Now})}

	for _, role := range cfg.Roles {
		e.addSession(role)
	}

	e.r = &Runner{
		Agents: e.ag, GitHub: e.gh, Git: e.git, Exec: e.exec, Keys: e.keys, Store: st,
		Identity:  fakeIdentity{name: "talkable-app", login: "talkable[bot]", kind: "app", env: map[string]string{"GH_CONFIG_DIR": "/state/gh/talkable-app", "GH_TOKEN": ""}},
		SelfLogin: "zhuravel",
		Config:    cfg, Layout: layout, Clock: clk.Now, Logger: e.log,
		Sleep: func(ctx context.Context, d time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			clk.Add(d)
			return nil
		},
	}
	// Default reviewer behavior: write a report and finish.
	e.ag.behaviors[agents.RoleClaude] = []behavior{writeReport("## P2 something\n")}
	e.ag.codex = func(f *fakeAgents, c codexCall) error {
		return os.WriteFile(filepath.Join(e.reportDir(), "codex-review.md"), []byte("[P2] codex finding\n"), 0o600)
	}
	return e
}

// addSession creates a live session for role in the next pane (p1, p2, ...):
// an agent session, or a shell pane (agent_kind "shell") for a shell role.
func (e *env) addSession(role config.Role) store.Session {
	e.t.Helper()
	r := agents.Role(role.Name)
	s := store.Session{PRID: e.pr.ID, Role: role.Name, AgentName: store.Ptr(agents.AgentName("talkable/talkable", 11920, r)),
		AgentKind: store.Ptr(role.AgentKind()), HerdrWorkspaceID: store.Ptr("w1"), HerdrPaneID: store.Ptr(fmt.Sprintf("p%d", len(e.sess)+1)),
		Cwd: store.Ptr(slotPath), State: store.SessionLive, StartedAt: t0.Add(-time.Hour)}
	if role.IsShell() {
		s.AgentName, s.AgentKind = nil, store.Ptr(agents.KindShell)
	}
	s, err := e.st.CreateSession(e.ctx, s)
	if err != nil {
		e.t.Fatalf("CreateSession %s: %v", role.Name, err)
	}
	e.sess[r] = s
	return s
}

func (e *env) reportDir() string {
	return e.layout.ReviewDir("talkable", "talkable", 11920, target)
}

func (e *env) input(kind string) RoundInput {
	return RoundInput{PR: e.pr, Repo: e.repo, SlotPath: slotPath, Round: 1, Kind: kind, TargetSHA: target,
		BaseRef: "master", BaseSHA: "base000111222333444555666777888999aaabbb"}
}

// judgePosts is the standard successful judge: App review, REQUEST_CHANGES.
func (e *env) judgePosts(id int64, state, event string) judgePost {
	return judgePost{gh: e.gh, reviewID: id, state: state, event: event, graphLogin: "talkable", graphType: "Bot",
		restLogin: "talkable[bot]", restType: "Bot", status: "posted", findings: map[string]int{"P2": 2}}
}

func (e *env) runs() []store.Run {
	e.t.Helper()
	rs, err := e.st.RunsByPR(e.ctx, e.pr.ID)
	if err != nil {
		e.t.Fatalf("RunsByPR: %v", err)
	}
	return rs
}

// runOf returns the single run of role and kind.
func (e *env) runOf(role agents.Role, kind string) store.Run {
	e.t.Helper()
	var found []store.Run
	for _, r := range e.runs() {
		if r.Role == string(role) && r.Kind == kind {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		e.t.Fatalf("runs of %s/%s: got %d, want 1: %+v", role, kind, len(found), found)
	}
	return found[0]
}

func (e *env) events() []store.Event {
	e.t.Helper()
	evs, err := e.st.EventsBySubject(e.ctx, "pr:talkable/talkable#11920", 0)
	if err != nil {
		e.t.Fatalf("EventsBySubject: %v", err)
	}
	return evs
}

func mustContain(t *testing.T, what, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("%s does not contain %q:\n%s", what, sub, s)
		}
	}
}

func errorsIs(err, target error) bool { return errors.Is(err, target) }
