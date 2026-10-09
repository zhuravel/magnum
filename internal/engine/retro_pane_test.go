package engine

import (
	"context"
	"encoding/json"
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
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// fakePaneAgents is the retro agent's manager over the scratch registry:
// fakeAgents for workspaces, starts and parks, plus runs that the agent
// works on in a goroutine (reply), which writes what it likes and ends its
// turn the way the observation would (the run ends).
type fakePaneAgents struct {
	*fakeAgents
	scratch string // the registry's directory, from the layout it was made with
	reply   func(n int, text string) (end bool)
	screen  string // what ReadRecent shows
	// submitErr refuses prompts before sending; quitErr fails Quit.
	submitErr, quitErr error
	// parkSaw are the PR's runs ("<id>:<state>") at Park, which refuses
	// while one is in flight, as agents.Manager.Park does.
	parkSaw []string

	mu      sync.Mutex
	prompts []string
	wg      sync.WaitGroup
}

func (f *fakePaneAgents) PreflightRole(ctx context.Context, role config.Role) error {
	return f.Preflight(ctx, role.AgentKind())
}

func (f *fakePaneAgents) NewRun(ctx context.Context, pr store.PR, role agents.Role, kind string, round int) (store.Run, error) {
	f.record(fmt.Sprintf("new_run:%s:%d", kind, round))
	s, err := f.st.LiveSessionByPRRole(ctx, pr.ID, string(role))
	r := store.Run{ID: fmt.Sprintf("r-learn-%d", round), PRID: pr.ID, Round: round, Role: string(role), Kind: kind,
		TargetSHA: pr.HeadSHA, State: store.RunPending}
	if err == nil {
		r.SessionID = &s.ID
	}
	return f.st.CreateRun(ctx, r)
}

func (f *fakePaneAgents) Park(ctx context.Context, pr store.PR) error {
	runs, err := f.st.RunsByPR(ctx, pr.ID)
	if err != nil {
		return err
	}
	busy := false
	for _, r := range runs {
		f.parkSaw = append(f.parkSaw, r.ID+":"+r.State)
		busy = busy || slices.Contains([]string{store.RunPending, store.RunSubmitted, store.RunWorking}, r.State)
	}
	if busy {
		f.record(fmt.Sprintf("park:%d", pr.ID))
		return fmt.Errorf("agents: park pr %d: a run is in flight: %w", pr.Number, agents.ErrBusy)
	}
	return f.fakeAgents.Park(ctx, pr)
}

func (f *fakePaneAgents) Quit(ctx context.Context, s store.Session) error {
	f.record(fmt.Sprintf("quit:%d", s.ID))
	if f.quitErr != nil {
		return f.quitErr
	}
	return f.st.TransitionSession(ctx, s.ID, []string{store.SessionLive, store.SessionStarting}, store.SessionClosed, nil)
}

func (f *fakePaneAgents) Submit(ctx context.Context, run store.Run, text string) error {
	if f.submitErr != nil {
		return f.submitErr
	}
	f.mu.Lock()
	f.prompts = append(f.prompts, text)
	n := len(f.prompts)
	f.mu.Unlock()
	if err := f.st.TransitionRun(ctx, run.ID, []string{store.RunPending}, store.RunWorking, nil); err != nil {
		return err
	}
	f.wg.Go(func() {
		if f.reply == nil || f.reply(n, text) {
			_ = f.st.TransitionRun(context.Background(), run.ID, []string{store.RunWorking}, store.RunEnded, nil)
		}
	})
	return nil
}

func (f *fakePaneAgents) ReadRecent(context.Context, store.Session, int) (string, error) {
	return f.screen, nil
}

func (f *fakePaneAgents) sent() []string {
	f.wg.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.prompts)
}

// paneHarness is a pane classifier whose manager is a fakePaneAgents.
type paneHarness struct {
	*harness
	ag   *fakePaneAgents
	keys []string
	mk   func(context.Context, RetroRun) (Classifier, error)
	// step advances the clock on every sleep of the classifier and its
	// observation (0 = time stands still).
	step time.Duration
}

func newPaneHarness(t *testing.T, mods ...func(*harness)) *paneHarness {
	p := &paneHarness{}
	p.harness = newHarness(t, mods...)
	p.ag = &fakePaneAgents{}
	var mu sync.Mutex
	p.mk = paneClassifiers(paneDeps{
		Config: p.cfg, Layout: p.layout, Logger: p.d.Logger, Herdr: p.hd, Now: p.clock.Now,
		Sleep: func(ctx context.Context, d time.Duration) error {
			if p.step > 0 {
				p.clock.Add(p.step)
			}
			return sleepCtx(ctx, time.Millisecond)
		},
		ObserveEvery: time.Millisecond, Poll: time.Millisecond,
		Keys: func(_ context.Context, target string, keys ...string) error {
			mu.Lock()
			p.keys = append(p.keys, target+":"+strings.Join(keys, ","))
			mu.Unlock()
			if slices.Contains(keys, "ctrl+c") { // the agent exits
				p.hd.mu.Lock()
				p.hd.agents = slices.DeleteFunc(p.hd.agents, func(a herdr.AgentInfo) bool { return a.Name == target })
				p.hd.mu.Unlock()
			}
			return nil
		},
		CloseWorkspace: p.hd.WorkspaceClose,
		Agents: func(st *store.Store, cfg *config.Config, layout paths.Layout) paneAgents {
			if len(cfg.Roles) != 1 || cfg.Roles[0].Name != config.LearnRoleName {
				t.Errorf("the manager's roles = %v, want the [learn] role alone", cfg.Roles)
			}
			p.ag.fakeAgents = &fakeAgents{st: st}
			p.ag.scratch = layout.Scratch
			return p.ag
		},
	})
	return p
}

// job writes a PR's retro directory with two candidates and returns its job.
func (p *paneHarness) job(run RetroRun, n int) ClassifyJob {
	p.t.Helper()
	dir := filepath.Join(run.Dir, "talkable", "talkable", fmt.Sprint(n))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		p.t.Fatal(err)
	}
	cands := []learn.Candidate{
		{ID: "t101", Kind: learn.KindThread, Path: "app/models/coupon.rb", Line: 42, ReviewedSHA: retroShaA, Body: retroBody},
		{ID: "r201", Kind: learn.KindReview, ReviewedSHA: retroShaA, Body: retroBody},
	}
	b, _ := json.Marshal(learn.Candidates{PR: "https://github.com/talkable/talkable/pull/" + fmt.Sprint(n), Candidates: cands})
	if err := os.WriteFile(filepath.Join(dir, learn.CandidatesFile), b, 0o600); err != nil {
		p.t.Fatal(err)
	}
	return ClassifyJob{Dir: dir, CandidatesPath: filepath.Join(dir, learn.CandidatesFile), OutputPath: filepath.Join(dir, learn.OutputFile),
		PR: store.PR{Number: n, URL: "https://github.com/talkable/talkable/pull/" + fmt.Sprint(n)}, ReviewedSHAs: []string{retroShaA},
		Candidates: cands}
}

// validAnswer writes a valid answer for job.
func validAnswer(t *testing.T, job ClassifyJob) {
	t.Helper()
	out := learn.Output{Items: []learn.Item{
		{ID: "t101", Class: "miss", Severity: "P2", Title: "Lookup ignores the tenant", Lesson: "When a finder reads a code, check the tenant scope.",
			Scope: "general", Lines: []int{42, 42}, Match: []string{"tenant"}},
		{ID: "r201", Class: "style"},
	}}
	b, _ := json.Marshal(out)
	if err := os.WriteFile(job.OutputPath, b, 0o600); err != nil {
		t.Error(err)
	}
}

func (p *paneHarness) classifier(run RetroRun) Classifier {
	p.t.Helper()
	cl, err := p.mk(p.ctx, run)
	if err != nil {
		p.t.Fatal(err)
	}
	return cl
}

// TestPaneClassifierPromptsOneAgentOncePerPR: one retro starts one agent,
// tagged apart, in a workspace "learn retro" that works in the directory
// of every run, prompts it once per PR with paths, the URL and the reviewed
// commit (never a comment), and at the end parks it and removes the scratch
// registry while the run directory keeps the inputs and answers.
func TestPaneClassifierPromptsOneAgentOncePerPR(t *testing.T) {
	p := newPaneHarness(t)
	run := RetroRun{ID: "20261005-070000", Dir: filepath.Join(p.layout.Learn(), "retro", "20261005-070000")}
	jobs := []ClassifyJob{p.job(run, 7), p.job(run, 8)}
	p.ag.reply = func(n int, _ string) bool { validAnswer(t, jobs[n-1]); return true }
	cl := p.classifier(run)
	for _, job := range jobs {
		res, err := cl.Classify(p.ctx, job)
		if err != nil || res.Pause != nil || res.Unclassified {
			t.Fatalf("Classify(%d) = %+v, %v", job.PR.Number, res, err)
		}
	}
	sent := p.ag.sent()
	if len(sent) != 2 {
		t.Fatalf("prompts = %d, want one per PR", len(sent))
	}
	for i, text := range sent {
		job := jobs[i]
		for _, want := range []string{job.PR.URL, retroShaA, job.CandidatesPath, job.OutputPath, filepath.Join(job.Dir, learn.FilesDir)} {
			if !strings.Contains(text, want) {
				t.Errorf("prompt %d lacks %q", i+1, want)
			}
		}
		if strings.Contains(text, "SECRET-BODY") {
			t.Errorf("prompt %d carries a comment's text", i+1)
		}
	}
	calls := p.ag.all()
	starts := slices.DeleteFunc(slices.Clone(calls), func(c string) bool { return !strings.HasPrefix(c, "start:") })
	ws := slices.DeleteFunc(slices.Clone(calls), func(c string) bool { return !strings.HasPrefix(c, "ensure_workspace:") })
	if len(starts) != 1 || len(ws) != 1 || !strings.Contains(ws[0], ":"+filepath.Dir(run.Dir)+":learn retro:") || !strings.HasSuffix(ws[0], ":retro") {
		t.Fatalf("agent setup = %v", calls)
	}
	if !slices.Contains(calls, "preflight:claude") {
		t.Errorf("no preflight of the agent's CLI: %v", calls)
	}
	if err := cl.Close(p.ctx); err != nil {
		t.Fatal(err)
	}
	if p.ag.count("park:") != 1 {
		t.Fatalf("park calls: %v", p.ag.all())
	}
	if _, err := os.Stat(p.ag.scratch); !errors.Is(err, os.ErrNotExist) || !strings.HasPrefix(p.ag.scratch, run.Dir) {
		t.Fatalf("scratch registry %s: %v", p.ag.scratch, err)
	}
	if _, err := os.Stat(jobs[1].OutputPath); err != nil {
		t.Fatalf("the answers must stay: %v", err)
	}
	if n := p.ag.count("observe"); n == 0 {
		t.Fatal("the agent was never observed")
	}
}

// TestPaneClassifierNudgesOnceThenGivesUp: a turn that ends without a
// valid answer gets one nudge, naming the files and the candidates' ids;
// a second failure is the PR's error.
func TestPaneClassifierNudgesOnceThenGivesUp(t *testing.T) {
	p := newPaneHarness(t)
	run := RetroRun{ID: "r", Dir: filepath.Join(p.layout.Learn(), "retro", "r")}
	ok, bad := p.job(run, 7), p.job(run, 8)
	p.ag.reply = func(n int, _ string) bool {
		switch n {
		case 2: // the nudge of PR 7
			validAnswer(t, ok)
		case 3: // PR 8: an answer off schema
			_ = os.WriteFile(bad.OutputPath, []byte(`{"items":[{"id":"t101","class":"bug"}]}`), 0o600)
		}
		return true
	}
	cl := p.classifier(run)
	if _, err := cl.Classify(p.ctx, ok); err != nil {
		t.Fatalf("after a nudge: %v", err)
	}
	_, err := cl.Classify(p.ctx, bad)
	if err == nil || !strings.Contains(err.Error(), "retro.json is not valid after a nudge") {
		t.Fatalf("second failure: %v", err)
	}
	sent := p.ag.sent()
	if len(sent) != 4 {
		t.Fatalf("prompts = %d, want 2 per PR", len(sent))
	}
	if n := sent[1]; !strings.Contains(n, ok.OutputPath+"` was not written") || !strings.Contains(n, "t101, r201") {
		t.Fatalf("nudge = %s", n)
	}
	if n := sent[3]; !strings.Contains(n, "does not follow the answer format") || strings.Contains(n, "bug") {
		t.Fatalf("nudge after an invalid answer = %s", n)
	}
	if !slices.Contains(p.ag.all(), "new_run:nudge:2") {
		t.Fatalf("the nudge is not a nudge run: %v", p.ag.all())
	}
	_ = cl.Close(p.ctx)
}

// TestPaneClassifierStopsAtAUsageLimit: a turn without an answer whose
// screen shows a usage limit is a Pause of the agent's CLI, without a
// nudge; a logged-out CLI is one before anything starts.
func TestPaneClassifierStopsAtAUsageLimit(t *testing.T) {
	p := newPaneHarness(t)
	run := RetroRun{ID: "r", Dir: filepath.Join(p.layout.Learn(), "retro", "r")}
	job := p.job(run, 7)
	p.ag.screen = job.OutputPath + "\n...\nYou've hit your usage limit. Try again in 2 hours."
	cl := p.classifier(run)
	res, err := cl.Classify(p.ctx, job)
	if err == nil || res.Pause == nil || res.Pause.Kind != string(agents.HealthUsageLimit) || res.Pause.Tool != "claude" ||
		!res.Pause.Until.Equal(p.clock.Now().Add(2*time.Hour)) {
		t.Fatalf("Classify = %+v, %v", res.Pause, err)
	}
	if n := len(p.ag.sent()); n != 1 {
		t.Fatalf("prompts = %d: a limit is not nudged", n)
	}
	_ = cl.Close(p.ctx)
}

// TestPaneClassifierRefusesALoggedOutCLI: the preflight's logout is a
// Pause, and no workspace or agent is started.
func TestPaneClassifierRefusesALoggedOutCLI(t *testing.T) {
	p := newPaneHarness(t)
	run := RetroRun{ID: "r", Dir: filepath.Join(p.layout.Learn(), "retro", "r")}
	job := p.job(run, 7)
	mk := p.mk
	p.mk = func(ctx context.Context, r RetroRun) (Classifier, error) {
		cl, err := mk(ctx, r)
		if pc, ok := cl.(*paneClassifier); ok {
			if err := pc.setup(ctx); err != nil {
				return nil, err
			}
			p.ag.loggedOut = map[string]bool{"claude": true}
		}
		return cl, err
	}
	cl := p.classifier(run)
	res, err := cl.Classify(p.ctx, job)
	if !errors.Is(err, agents.ErrLoginRequired) || res.Pause == nil || res.Pause.Kind != string(agents.HealthLoginRequired) {
		t.Fatalf("Classify = %+v, %v", res.Pause, err)
	}
	if p.ag.count("start:") != 0 || p.ag.count("ensure_workspace:") != 0 {
		t.Fatalf("started anyway: %v", p.ag.all())
	}
	_ = cl.Close(p.ctx)
}

// TestPaneClassifierInterruptsATurnPastItsTimeout: a turn that outlives
// [learn] timeout is interrupted (esc) and abandoned, so the workspace can
// close; the PR fails after its nudge does the same.
func TestPaneClassifierInterruptsATurnPastItsTimeout(t *testing.T) {
	p := newPaneHarness(t, func(h *harness) { h.cfg.Learn.Timeout = config.Duration{Duration: time.Minute} })
	run := RetroRun{ID: "r", Dir: filepath.Join(p.layout.Learn(), "retro", "r")}
	job := p.job(run, 7)
	p.step = 10 * time.Second
	p.ag.reply = func(int, string) bool { return false } // works on forever
	cl := p.classifier(run)
	_, err := cl.Classify(p.ctx, job)
	if err == nil || !strings.Contains(err.Error(), "no answer within 1m0s") {
		t.Fatalf("Classify: %v", err)
	}
	if len(p.keys) != 2 || !strings.HasSuffix(p.keys[0], ":esc") {
		t.Fatalf("interrupts = %v", p.keys)
	}
	for _, id := range []string{"r-learn-1", "r-learn-2"} {
		r, err := p.ag.st.RunByID(p.ctx, id)
		if err != nil || r.State != store.RunAbandoned {
			t.Fatalf("run %s = %+v, %v", id, r, err)
		}
	}
	if err := cl.Close(p.ctx); err != nil {
		t.Fatal(err)
	}
}

// TestRetroWithThePaneClassifier: the retro job and the pane classifier
// together: the PR's comments are classified by the agent's answer and
// stored, the agent is parked at the end, and nothing of the agent's
// session reaches the live registry.
func TestRetroWithThePaneClassifier(t *testing.T) {
	var p *paneHarness
	p = newPaneHarness(t, func(h *harness) {
		h.d.Classifier = func(ctx context.Context, run RetroRun) (Classifier, error) { return p.mk(ctx, run) }
	})
	pr := retroPR(p.harness, 7, 24*time.Hour)
	seedRetroGitHub(p.harness, 7)
	p.ag.reply = func(n int, text string) bool {
		out := learn.Output{}
		for _, id := range []string{"t101", "t103", "t105", "r201"} {
			out.Items = append(out.Items, learn.Item{ID: id, Class: "not_issue"})
		}
		b, _ := json.Marshal(out)
		path := text[strings.Index(text, "Write `")+len("Write `"):]
		path = path[:strings.IndexByte(path, '`')]
		_ = os.WriteFile(path, b, 0o600)
		return true
	}
	p.requestRetro(RetroPayload{})

	if rp, err := p.retroRecord(pr.ID); err != nil || rp.Status != store.RetroClassified {
		t.Fatalf("retro_prs = %+v, %v", rp, err)
	}
	if m := p.misses(pr.ID)["101"]; m.Class != store.MissNotIssue {
		t.Fatalf("t101 = %+v", m)
	}
	if p.ag.count("park:") != 1 {
		t.Fatalf("the agent was not parked: %v", p.ag.all())
	}
	if live, err := p.st.LiveSessions(p.ctx); err != nil || len(live) != 0 {
		t.Fatalf("live registry sessions = %+v, %v", live, err)
	}
	if runs, err := p.st.RunsByPR(p.ctx, pr.ID); err != nil || len(runs) != 1 {
		t.Fatalf("live registry runs of the PR = %d, %v (only magnum's review)", len(runs), err)
	}
}

// retroAgentName is the tagged herdr name of the retro's agent.
var retroAgentName = agents.TaggedAgentName(LearnAgentTag, "magnum/retro", 1, agents.Role(config.LearnRoleName))

// ownRetroWorkspace makes the fake herdr show workspace ws, labelled like
// the retro's, holding only pane.
func (p *paneHarness) ownRetroWorkspace(ws, pane string) {
	p.hd.mu.Lock()
	defer p.hd.mu.Unlock()
	p.hd.workspaces = append(p.hd.workspaces, herdr.Workspace{ID: ws, Label: learnWorkspace})
	p.hd.panes = append(p.hd.panes, herdr.Pane{ID: pane, WorkspaceID: ws})
}

// TestPaneClassifierCloseQuitsABusyAgent: an agent Park leaves alone
// because it is busy is still ours (its tagged name): Close quits it and
// closes its workspace, then removes the scratch registry.
func TestPaneClassifierCloseQuitsABusyAgent(t *testing.T) {
	p := newPaneHarness(t)
	run := RetroRun{ID: "r", Dir: filepath.Join(p.layout.Learn(), "retro", "r")}
	job := p.job(run, 7)
	p.ag.reply = func(int, string) bool { validAnswer(t, job); return true }
	cl := p.classifier(run)
	if _, err := cl.Classify(p.ctx, job); err != nil {
		t.Fatal(err)
	}
	p.ownRetroWorkspace("w1", "p-retro")
	p.ag.parkErr = fmt.Errorf("agents: park pr 1: retro is working: %w", agents.ErrBusy)
	if err := cl.Close(p.ctx); err != nil {
		t.Fatal(err)
	}
	if p.ag.count("quit:") != 1 || !slices.Contains(p.hd.closed, "w1") {
		t.Fatalf("quit %d times, closed workspaces %v", p.ag.count("quit:"), p.hd.closed)
	}
	if _, err := os.Stat(p.ag.scratch); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch registry: %v", err)
	}
}

// TestPaneClassifierCloseKeepsTheRegistryWhenTheAgentStays: when the agent
// can be neither parked nor quit, Close says so and keeps the scratch
// registry, where its session is recorded, instead of losing track of it.
func TestPaneClassifierCloseKeepsTheRegistryWhenTheAgentStays(t *testing.T) {
	p := newPaneHarness(t)
	run := RetroRun{ID: "r", Dir: filepath.Join(p.layout.Learn(), "retro", "r")}
	job := p.job(run, 7)
	p.ag.reply = func(int, string) bool { validAnswer(t, job); return true }
	cl := p.classifier(run)
	if _, err := cl.Classify(p.ctx, job); err != nil {
		t.Fatal(err)
	}
	p.ag.parkErr = agents.ErrBusy
	p.ag.quitErr = agents.ErrBusy
	err := cl.Close(p.ctx)
	if err == nil || !strings.Contains(err.Error(), p.ag.scratch) {
		t.Fatalf("Close = %v, want the scratch registry's path", err)
	}
	if _, err := os.Stat(p.ag.scratch); err != nil {
		t.Fatalf("the scratch registry is gone: %v", err)
	}
	if len(p.hd.closed) != 0 {
		t.Fatalf("closed a workspace whose agent still runs: %v", p.hd.closed)
	}
}

// TestPaneClassifierCancelledMidTurn: a shutdown during a turn ends
// Classify with the context's error; Close then interrupts and abandons
// the turn, so the agent can be parked and nothing is left working.
func TestPaneClassifierCancelledMidTurn(t *testing.T) {
	p := newPaneHarness(t)
	run := RetroRun{ID: "r", Dir: filepath.Join(p.layout.Learn(), "retro", "r")}
	job := p.job(run, 7)
	ctx, cancel := context.WithCancel(p.ctx)
	p.ag.reply = func(int, string) bool { cancel(); return false } // works on when the daemon stops
	cl := p.classifier(run)
	if _, err := cl.Classify(ctx, job); !errors.Is(err, context.Canceled) {
		t.Fatalf("Classify = %v, want the context's error", err)
	}
	if n := len(p.ag.sent()); n != 1 {
		t.Fatalf("prompts = %d: a cancelled turn is not nudged", n)
	}
	if err := cl.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.keys) != 1 || !strings.HasSuffix(p.keys[0], ":esc") {
		t.Fatalf("interrupts = %v", p.keys)
	}
	if !slices.Equal(p.ag.parkSaw, []string{"r-learn-1:abandoned"}) || p.ag.count("quit:") != 0 {
		t.Fatalf("Park saw %v (quits %d): the turn must be abandoned before the park", p.ag.parkSaw, p.ag.count("quit:"))
	}
}

// TestPaneClassifierClearsALeftoverAgent: an agent an earlier retro left
// behind carries the same tagged name, and StartAgent would adopt it,
// conversation and all: setup ends it first, closing its "learn retro"
// workspace, or quitting it when the workspace holds something else.
func TestPaneClassifierClearsALeftoverAgent(t *testing.T) {
	for _, own := range []bool{true, false} {
		p := newPaneHarness(t)
		run := RetroRun{ID: "r", Dir: filepath.Join(p.layout.Learn(), "retro", "r")}
		job := p.job(run, 7)
		p.ag.reply = func(int, string) bool { validAnswer(t, job); return true }
		p.ownRetroWorkspace("w-old", "p-old")
		p.hd.agents = []herdr.AgentInfo{{Name: retroAgentName, PaneID: "p-old", WorkspaceID: "w-old", Agent: "claude"}}
		if !own { // the user split the old workspace: it is not closed, the agent is quit
			p.hd.panes = append(p.hd.panes, herdr.Pane{ID: "p-user", WorkspaceID: "w-old"})
		}
		cl := p.classifier(run)
		if _, err := cl.Classify(p.ctx, job); err != nil {
			t.Fatal(err)
		}
		if own != slices.Contains(p.hd.closed, "w-old") {
			t.Fatalf("own=%v: closed workspaces %v", own, p.hd.closed)
		}
		if !own && !slices.Contains(p.keys, retroAgentName+":ctrl+c") {
			t.Fatalf("the leftover agent was not quit: %v", p.keys)
		}
		if len(p.hd.agents) != 0 || p.ag.count("start:") != 1 {
			t.Fatalf("agents %v, starts %d", p.hd.agents, p.ag.count("start:"))
		}
		_ = cl.Close(p.ctx)
	}
}

// TestPaneClassifierReportsModelLimitsAndOverloads: a per-model limit and
// an overload stop the retro as themselves, not as a usage limit (which
// would pause the CLI for every round).
func TestPaneClassifierReportsModelLimitsAndOverloads(t *testing.T) {
	for screen, kind := range map[string]agents.HealthKind{
		"You've hit your Opus limit · resets 3:45pm": agents.HealthModelLimit,
		"API Error: 529 overloaded_error":            agents.HealthOverloaded,
	} {
		p := newPaneHarness(t)
		run := RetroRun{ID: "r", Dir: filepath.Join(p.layout.Learn(), "retro", "r")}
		job := p.job(run, 7)
		p.ag.screen = job.OutputPath + "\n" + screen
		cl := p.classifier(run)
		res, err := cl.Classify(p.ctx, job)
		if err == nil || res.Pause == nil || res.Pause.Kind != string(kind) {
			t.Errorf("%q: pause %+v, %v; want %s", screen, res.Pause, err, kind)
		}
		_ = cl.Close(p.ctx)
	}
}

// TestPaneClassifierIsDownWhenThePromptIsRefused: a prompt refused before
// it was sent (no live session, herdr gone) is not the PR's failure: no
// nudge, and the error matches ErrClassifierDown, which stops the retro
// without recording the PR.
func TestPaneClassifierIsDownWhenThePromptIsRefused(t *testing.T) {
	p := newPaneHarness(t)
	run := RetroRun{ID: "r", Dir: filepath.Join(p.layout.Learn(), "retro", "r")}
	job := p.job(run, 7)
	p.ag.submitErr = fmt.Errorf("agents: submit: %w", agents.ErrNoSession)
	cl := p.classifier(run)
	if _, err := cl.Classify(p.ctx, job); !errors.Is(err, ErrClassifierDown) {
		t.Fatalf("Classify = %v, want ErrClassifierDown", err)
	}
	if p.ag.count("new_run:") != 1 {
		t.Fatalf("runs = %v: a refused prompt is not nudged", p.ag.all())
	}
	_ = cl.Close(p.ctx)
}
