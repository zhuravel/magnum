package engine

// The retro's classifier (DECISIONS "Learning loop: daily retro"): one
// interactive agent per retro, in a herdr pane, prompted once per PR. It is
// driven the way `magnum eval` drives a replay: a scratch registry under the
// retro's run directory holds its session and runs, a Manager tagged
// "learn" starts it (its name hashes the tag, so it never meets a PR's own
// agents), and a scratch engine observes herdr for it while it works
// (observeEvery), so trust dialogs, permission prompts (answered No), the
// hooks review and the end of a turn (agents.ObserveSnapshotAt, two idle
// observations) behave exactly as in rounds. The daemon's own observation
// never sees it: its sessions live in the scratch registry. The live
// registry gets only what the retro job stores. The notes curator
// (notes_curate.go) is the same kind of agent: a paneAgent with its own
// paneSpec.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

const (
	// LearnAgentTag tags the retro's agent (agents.Deps.Tag), as "eval"
	// tags a replay's.
	LearnAgentTag = "learn"
	// learnWorkspace labels its herdr workspace.
	learnWorkspace = "learn retro"
	// learnScratch is the scratch registry's directory in a run directory,
	// removed when the retro ends with its agent closed.
	learnScratch = ".scratch"
	// learnRepoOwner, learnRepoName and learnNumber name the scratch row
	// the agent's session hangs on; with the tag they make its herdr name
	// (agents.TaggedAgentName), the same for every retro.
	learnRepoOwner = "magnum"
	learnRepoName  = "retro"
	learnNumber    = 1
	// learnPoll is how often a turn's run row is read.
	learnPoll = 2 * time.Second
)

// RetroObserveEvery is how often the retro's agent is observed.
var RetroObserveEvery = 10 * time.Second

// paneAgents is the part of *agents.Manager the retro's classifier drives.
type paneAgents interface {
	Agents
	PreflightRole(ctx context.Context, role config.Role) error
	Quit(ctx context.Context, s store.Session) error
	NewRun(ctx context.Context, pr store.PR, role agents.Role, kind string, round int) (store.Run, error)
	Submit(ctx context.Context, run store.Run, text string) error
	ReadRecent(ctx context.Context, s store.Session, lines int) (string, error)
}

var _ paneAgents = (*agents.Manager)(nil)

// paneDeps are the pane classifier's collaborators.
type paneDeps struct {
	Config *config.Config
	Layout paths.Layout
	Logger *slog.Logger
	// Herdr takes the snapshots the scratch engine observes; Keys sends
	// keys to an agent (a turn past [learn] timeout is interrupted with esc,
	// a leftover agent quit with ctrl+c) and CloseWorkspace closes a herdr
	// workspace (the agent's, when Park could not).
	Herdr          Herdr
	Keys           func(ctx context.Context, target string, keys ...string) error
	CloseWorkspace func(ctx context.Context, id string) error
	// Agents makes the agents manager over the scratch registry
	// (agents.New with the live herdr client, tagged LearnAgentTag).
	Agents func(st *store.Store, cfg *config.Config, layout paths.Layout) paneAgents
	// Now and Sleep as in Deps (nil = the real ones); ObserveEvery and Poll
	// default to RetroObserveEvery and learnPoll.
	Now          func() time.Time
	Sleep        func(ctx context.Context, d time.Duration) error
	ObserveEvery time.Duration
	Poll         time.Duration
}

// withDefaults fills d's optional fields.
func (d paneDeps) withDefaults() paneDeps {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Sleep == nil {
		d.Sleep = sleepCtx
	}
	if d.ObserveEvery <= 0 {
		d.ObserveEvery = RetroObserveEvery
	}
	if d.Poll <= 0 {
		d.Poll = learnPoll
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	return d
}

// paneSpec says which pane agent a paneAgent is: what it is called in logs
// and errors, the label of its herdr workspace, the scratch row its session
// hangs on (with LearnAgentTag they make its herdr name, the same for every
// run) and the role it runs as.
type paneSpec struct {
	what        string
	workspace   string
	owner, name string
	role        func(*config.Config) config.Role
}

// retroPane is the retro's classifier.
var retroPane = paneSpec{what: "retro", workspace: learnWorkspace, owner: learnRepoOwner, name: learnRepoName,
	role: (*config.Config).LearnRole}

// paneClassifiers is Deps.Classifier for the pane agent: a classifier per
// retro run, which sets itself up at its first PR.
func paneClassifiers(d paneDeps) func(context.Context, RetroRun) (Classifier, error) {
	d = d.withDefaults()
	return func(_ context.Context, run RetroRun) (Classifier, error) {
		return &paneClassifier{paneAgent: &paneAgent{d: d, spec: retroPane, dir: run.Dir, id: run.ID}, run: run}, nil
	}
}

// paneClassifier is one retro's agent.
type paneClassifier struct {
	*paneAgent
	run RetroRun
}

// paneAgent is one interactive agent of a run (a retro, a curation),
// prompted turn by turn.
type paneAgent struct {
	d    paneDeps
	spec paneSpec
	dir  string // the run's directory: the scratch registry is in it, the agent works in its parent
	id   string // the run's id

	scratch string // the scratch registry's directory
	cfg     *config.Config
	role    config.Role
	st      *store.Store
	mgr     paneAgents
	obs     *Engine  // observes herdr for mgr
	pr      store.PR // the scratch registry's row the agent's session hangs on
	turns   int

	stopObserving context.CancelFunc
	observed      chan struct{}
}

// paneTurn is how a prompted turn ended.
type paneTurn struct {
	run store.Run
	err error // nil: the turn ended (the agent went idle)
	// down: the turn never reached the agent (the prompt was refused
	// before it was sent) or the agent went away (its session was lost).
	down bool
}

// Classify prompts the agent with job's paths (prompts/retro.md) and waits
// for its turn to end; an answer that is missing or invalid gets one nudge.
// A usage limit, a logout, a per-model limit or an overload on the agent's
// screen (or a logout at preflight) is a Pause. An agent that cannot be set
// up or started, or that a turn could not reach, is ErrClassifierDown: no
// nudge, and the retro stops without blaming the PR.
func (c *paneClassifier) Classify(ctx context.Context, job ClassifyJob) (ClassifyResult, error) {
	if err := c.setup(ctx); err != nil {
		return ClassifyResult{}, fmt.Errorf("%w: set up: %w", ErrClassifierDown, err)
	}
	if pause, err := c.ensureAgent(ctx); err != nil {
		if pause != nil || ctx.Err() != nil {
			return ClassifyResult{Pause: pause}, err
		}
		return ClassifyResult{}, fmt.Errorf("%w: start: %w", ErrClassifierDown, err)
	}
	text, err := c.prompt(job)
	if err != nil { // the prompt file, not the PR
		return ClassifyResult{}, fmt.Errorf("%w: %w", ErrClassifierDown, err)
	}
	_ = os.Remove(job.OutputPath)
	for attempt := 0; ; attempt++ {
		t := c.turn(ctx, attempt, text)
		missing, valid := answered(job)
		if valid {
			return ClassifyResult{}, nil
		}
		if p := c.health(ctx, t, job.OutputPath); p != nil {
			return ClassifyResult{Pause: p}, fmt.Errorf("the retro's agent stopped: %s", p.Kind)
		}
		what := "is not valid"
		if missing {
			what = "was not written"
		}
		switch {
		case ctx.Err() != nil:
			return ClassifyResult{}, ctx.Err()
		case t.down:
			return ClassifyResult{}, fmt.Errorf("%w: %w", ErrClassifierDown, t.err)
		case attempt > 0:
			return ClassifyResult{}, errors.Join(fmt.Errorf("%s %s after a nudge", learn.OutputFile, what), t.err)
		}
		if t.err != nil {
			c.d.Logger.Warn("retro: the agent's turn did not end well; nudging", "run", c.run.ID, "err", t.err)
		}
		text = retroNudge(job, missing)
	}
}

// answered reads job's answer file: whether it is missing, and whether it
// is a valid answer for job's candidates.
func answered(job ClassifyJob) (missing, valid bool) {
	data, err := os.ReadFile(job.OutputPath)
	if err != nil {
		return true, false
	}
	_, err = learn.ParseOutput(data, job.Candidates, job.Rejected)
	return false, err == nil
}

// retroNudge is the one reminder after a turn without a valid answer: paths
// and the candidates' ids only, never the classifier's text.
func retroNudge(job ClassifyJob, missing bool) string {
	what := "does not follow the answer format of the first prompt"
	if missing {
		what = "was not written"
	}
	ids := make([]string, 0, len(job.Candidates))
	for _, c := range job.Candidates {
		ids = append(ids, c.ID)
	}
	return fmt.Sprintf("`%s` %s. Write it now: {\"items\": [...]} with exactly one item per candidate of `%s` (%s), "+
		"each with \"id\" and \"class\" (miss, not_issue, style or outside), and for a miss \"severity\", \"title\", "+
		"\"lesson\", \"scope\", \"lines\" and \"match\" as the first prompt describes. Write `%s.tmp`, move it to `%s`, then stop.",
		job.OutputPath, what, job.CandidatesPath, strings.Join(ids, ", "), job.OutputPath, job.OutputPath)
}

// prompt renders the retro prompt for job.
func (c *paneClassifier) prompt(job ClassifyJob) (string, error) {
	p, err := c.cfg.ResolvePrompt(c.cfg.Learn.Prompt)
	if err != nil {
		return "", fmt.Errorf("the retro prompt cannot be read: %w", err)
	}
	return agents.RenderPrompt(p, retroData{URL: job.PR.URL, ReviewedSHAs: job.ReviewedSHAs, Candidates: job.CandidatesPath,
		Files: filepath.Join(job.Dir, learn.FilesDir), Output: job.OutputPath, Count: len(job.Candidates)})
}

// setup makes, once, the scratch registry under the run directory with the
// row the agent's session hangs on, the agents manager over it with the
// agent's role ([learn], [notes]) as its only role, and the engine that
// observes herdr for it.
func (c *paneAgent) setup(ctx context.Context) error {
	if c.mgr != nil {
		return nil
	}
	c.scratch = filepath.Join(c.dir, learnScratch)
	layout := c.d.Layout // logs, locks and the gh config dirs stay the install's
	layout.Scratch = c.scratch
	if err := os.MkdirAll(c.scratch, 0o700); err != nil {
		return err
	}
	st, err := store.Open(layout.DB())
	if err != nil {
		return err
	}
	st.Clock = c.d.Now
	cfg := *c.d.Config
	c.role = c.spec.role(&cfg)
	cfg.Roles = []config.Role{c.role} // the agents manager knows the role by name
	node := "learn:" + c.spec.name
	repo, err := st.UpsertRepo(ctx, store.Repo{NodeID: node, Owner: c.spec.owner, Name: c.spec.name,
		WatchOwner: c.spec.owner, Mode: store.RepoModePerPR})
	if err == nil {
		var up store.PRUpsert
		up, err = st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: node + "#1", Number: learnNumber,
			URL: "file://" + c.dir, HeadSHA: c.id, GHState: store.GHOpen, InitialState: store.PRClaiming, Identity: LearnAgentTag})
		c.pr = up.PR
	}
	if err != nil {
		st.Close()
		_ = os.RemoveAll(c.scratch)
		return fmt.Errorf("seed the scratch registry: %w", err)
	}
	c.cfg, c.st = &cfg, st
	c.mgr = c.d.Agents(st, &cfg, layout)
	c.obs = New(Deps{Config: &cfg, Layout: layout, Store: st, Logger: c.d.Logger, Now: c.d.Now, Sleep: c.d.Sleep,
		Herdr: c.d.Herdr, Agents: c.mgr})
	return nil
}

// ensureAgent starts the agent unless its session is live: a preflight of
// its CLI (a logout is a Pause, as for a round), then observation, the end
// of an agent an earlier run left behind (clearLeftover), its workspace
// and the agent itself. The agent works in the directory that holds every
// run, one path across runs: the CLIs trust the directory they start in
// (EnsureTrust), and a new path per run would add an entry to their config
// every day.
func (c *paneAgent) ensureAgent(ctx context.Context) (*pipeline.Pause, error) {
	if s, err := c.st.LiveSessionByPRRole(ctx, c.pr.ID, c.role.Name); err == nil && s.State == store.SessionLive {
		return nil, nil
	}
	if err := c.mgr.PreflightRole(ctx, c.role); err != nil {
		if errors.Is(err, agents.ErrLoginRequired) {
			return &pipeline.Pause{Kind: string(agents.HealthLoginRequired), Tool: c.role.AgentKind(),
				Detail: execx.Redact(err.Error())}, err
		}
		return nil, err
	}
	c.observe(ctx)
	if err := c.clearLeftover(ctx); err != nil {
		return nil, err
	}
	cwd := filepath.Dir(c.dir)
	ws, err := c.mgr.EnsureWorkspace(ctx, c.pr, cwd, nil, c.spec.workspace, []config.Role{c.role})
	if err != nil {
		return nil, err
	}
	pane := ws.Panes[agents.Role(c.role.Name)]
	if pane == "" {
		return nil, fmt.Errorf("the %q workspace has no pane for the agent", c.spec.workspace)
	}
	return nil, c.mgr.StartAgent(ctx, c.pr, c.role, pane, "")
}

// clearLeftover ends an agent with the run's tagged name that is not in
// this run's registry (a crash, a workspace an earlier run could not
// close): StartAgent would adopt it, its old conversation and all, and
// leave this run's new workspace empty. Its workspace is closed when it is
// one of the spec's ("learn retro") holding nothing else; otherwise the
// agent is quit (ctrl+c twice) and the workspace left to whoever split it.
func (c *paneAgent) clearLeftover(ctx context.Context) error {
	if c.d.Herdr == nil {
		return nil
	}
	name := agents.TaggedAgentName(LearnAgentTag, c.spec.owner+"/"+c.spec.name, learnNumber, agents.Role(c.role.Name))
	for attempt := range 5 {
		snap, err := c.d.Herdr.Snapshot(ctx)
		if err != nil {
			return err
		}
		a, ok := snap.AgentByName(name)
		if !ok {
			return nil
		}
		if attempt == 0 {
			c.d.Logger.Warn(c.spec.what+": ending an agent an earlier run left behind", "agent", name, "workspace", a.WorkspaceID)
			switch {
			case ownWorkspace(snap, a.WorkspaceID, a.PaneID, c.spec.workspace) && c.d.CloseWorkspace != nil:
				err = c.d.CloseWorkspace(ctx, a.WorkspaceID)
			case c.d.Keys != nil:
				err = c.d.Keys(ctx, name, "ctrl+c")
				if err == nil && c.d.Sleep(ctx, time.Second) == nil {
					err = c.d.Keys(ctx, name, "ctrl+c")
				}
			}
			if err != nil && !herdrGone(err) {
				return fmt.Errorf("end the leftover agent %s: %w", name, err)
			}
		}
		if err := c.d.Sleep(ctx, time.Second); err != nil {
			return err
		}
	}
	return fmt.Errorf("the agent %s an earlier run left behind is still running", name)
}

// ownWorkspace reports whether workspace ws is a pane agent's own: labelled
// label and holding no pane but pane.
func ownWorkspace(snap herdr.Snapshot, ws, pane, label string) bool {
	if ws == "" || !slices.ContainsFunc(snap.Workspaces, func(w herdr.Workspace) bool { return w.ID == ws && w.Label == label }) {
		return false
	}
	return !slices.ContainsFunc(snap.Panes, func(p herdr.Pane) bool { return p.WorkspaceID == ws && p.ID != pane })
}

// herdrGone reports whether herdr says the target no longer exists.
func herdrGone(err error) bool {
	return herdr.IsCode(err, herdr.CodeWorkspaceNotFound) || herdr.IsCode(err, herdr.CodePaneNotFound) ||
		herdr.IsCode(err, herdr.CodeAgentNotFound)
}

// observe starts observing herdr for the agent until Close (or ctx ends):
// nothing else tells its turns that they ended.
func (c *paneAgent) observe(ctx context.Context) {
	if c.observed != nil {
		return
	}
	octx, stop := context.WithCancel(ctx)
	c.stopObserving, c.observed = stop, make(chan struct{})
	go func() {
		defer close(c.observed)
		c.obs.observeEvery(octx, c.d.ObserveEvery)
	}()
}

// turn sends text as a new run and waits until the run ends, fails, loses
// its session, ctx ends or [learn] timeout passes (the turn is then
// interrupted). A run Submit left pending is abandoned; one it submitted is
// observed, never sent again.
func (c *paneAgent) turn(ctx context.Context, attempt int, text string) paneTurn {
	kind := store.RunInitial
	if attempt > 0 {
		kind = store.RunNudge
	}
	c.turns++
	run, err := c.mgr.NewRun(ctx, c.pr, agents.Role(c.role.Name), kind, c.turns)
	if err != nil {
		return paneTurn{err: err, down: true}
	}
	if err := c.mgr.Submit(ctx, run, text); err != nil {
		cur, rerr := c.st.RunByID(context.WithoutCancel(ctx), run.ID)
		switch {
		case rerr != nil:
			return paneTurn{run: run, err: errors.Join(err, rerr), down: true}
		case cur.State == store.RunPending:
			c.finish(ctx, cur.ID, err)
			return paneTurn{run: cur, err: err, down: true}
		case cur.State == store.RunFailed || cur.State == store.RunAbandoned:
			return paneTurn{run: cur, err: err, down: true}
		}
		c.d.Logger.Warn(c.spec.what+": the prompt was not acknowledged as working; waiting for the turn", "run", run.ID, "err", err)
	}
	return c.wait(ctx, run.ID)
}

// wait polls run id until it ends (the observation saw the agent idle
// twice), fails, loses its session, ctx ends or the role's timeout passes.
func (c *paneAgent) wait(ctx context.Context, id string) paneTurn {
	deadline := c.d.Now().Add(c.role.Timeout.Duration)
	for {
		run, err := c.st.RunByID(ctx, id)
		if err != nil {
			return paneTurn{run: run, err: err}
		}
		switch run.State {
		case store.RunEnded, store.RunVerified:
			return paneTurn{run: run}
		case store.RunFailed, store.RunAbandoned:
			return paneTurn{run: run, err: fmt.Errorf("run %s is %s: %s", run.ID, run.State, store.Deref(run.Error))}
		}
		if run.SessionID != nil {
			if s, err := c.st.SessionByID(ctx, *run.SessionID); err == nil && s.State != store.SessionLive && s.State != store.SessionStarting {
				return paneTurn{run: run, err: fmt.Errorf("the agent's session is %s", s.State), down: true}
			}
		}
		if !c.d.Now().Before(deadline) {
			c.interrupt(ctx, run)
			err := fmt.Errorf("no answer within %s", c.role.Timeout.Duration)
			c.finish(ctx, run.ID, err)
			return paneTurn{run: run, err: err}
		}
		if err := c.d.Sleep(ctx, c.d.Poll); err != nil {
			return paneTurn{run: run, err: err}
		}
	}
}

// interrupt stops the agent's turn (esc), so it neither writes late nor
// keeps the workspace busy.
func (c *paneAgent) interrupt(ctx context.Context, run store.Run) {
	if c.d.Keys == nil || run.SessionID == nil {
		return
	}
	s, err := c.st.SessionByID(context.WithoutCancel(ctx), *run.SessionID)
	if err != nil {
		return
	}
	target := store.Deref(s.AgentName)
	if target == "" {
		target = store.Deref(s.HerdrPaneID)
	}
	if target != "" {
		if err := c.d.Keys(context.WithoutCancel(ctx), target, "esc"); err != nil {
			c.d.Logger.Warn(c.spec.what+": interrupt the agent", "run", run.ID, "err", err)
		}
	}
}

// finish abandons a run that will not be waited for any more.
func (c *paneAgent) finish(ctx context.Context, id string, why error) {
	err := c.st.TransitionRun(context.WithoutCancel(ctx), id,
		[]string{store.RunPending, store.RunSubmitted, store.RunWorking, store.RunEnded}, store.RunAbandoned,
		func(u *store.RunUpdate) {
			u.Set("error", execx.Redact(why.Error()))
			u.Set("ended_at", c.d.Now())
		})
	if err != nil && !errors.Is(err, store.ErrConflict) {
		c.d.Logger.Warn(c.spec.what+": abandon a run", "run", id, "err", err)
	}
}

// health classifies the agent's screen after a turn without a valid answer,
// with its kind's health patterns, after the prompt (which names the answer
// file): a usage limit, a logout, a per-model limit or an overload is a
// Pause of that kind (the retro job pauses the tool for the first two
// only, as rounds do).
func (c *paneAgent) health(ctx context.Context, t paneTurn, anchor string) *pipeline.Pause {
	if t.run.SessionID == nil {
		return nil
	}
	s, err := c.st.SessionByID(context.WithoutCancel(ctx), *t.run.SessionID)
	if err != nil {
		return nil
	}
	text, err := c.mgr.ReadRecent(context.WithoutCancel(ctx), s, agents.HealthLines)
	if err != nil {
		return nil
	}
	if i := strings.LastIndex(text, anchor); i >= 0 {
		text = text[i+len(anchor):]
	}
	kind := c.role.AgentKind()
	h := agents.ClassifyAt(text, c.d.Now())
	if k, ok := c.cfg.KindSpec(kind); ok {
		if rx, err := k.HealthPatterns.Compile(); err == nil {
			h = agents.ClassifyWith(rx, text, c.d.Now())
		}
	}
	switch h.Kind {
	case agents.HealthUsageLimit, agents.HealthLoginRequired, agents.HealthModelLimit, agents.HealthOverloaded:
		p := &pipeline.Pause{Kind: string(h.Kind), Tool: kind, Detail: h.Detail}
		if h.ResetAt != nil {
			p.Until = *h.ResetAt
		}
		return p
	}
	return nil
}

// Close stops observing and closes the agent: a turn still in flight (a
// shutdown mid-turn) is interrupted and abandoned, then the agent is parked
// with its workspace (agents.Manager.Park); when Park refuses a busy agent,
// the agent, which is ours by its tagged name, is quit and its workspace
// closed. Only then is the scratch registry removed; if the agent stays,
// the registry stays too, where its session is recorded, and the error says
// where. The run directory keeps the inputs and the answers either way.
func (c *paneAgent) Close(ctx context.Context) error {
	if c.stopObserving != nil {
		c.stopObserving()
		<-c.observed
	}
	if c.mgr == nil {
		return nil
	}
	if err := c.release(ctx); err != nil {
		_ = c.st.Close()
		c.d.Logger.Warn(c.spec.what+": the agent was not closed; its scratch registry stays", "registry", c.scratch,
			"workspace", c.spec.workspace, "err", err)
		return fmt.Errorf("the %q workspace may still run the agent (its registry stays at %s): %w", c.spec.workspace, c.scratch, err)
	}
	var errs []error
	if err := c.st.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := os.RemoveAll(c.scratch); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// release ends the agent's runs, then parks it, else quits it and closes
// its workspace.
func (c *paneAgent) release(ctx context.Context) error {
	runs, err := c.st.RunsByPR(ctx, c.pr.ID)
	if err != nil {
		return err
	}
	for _, r := range runs {
		switch r.State {
		case store.RunPending, store.RunSubmitted, store.RunWorking:
			c.interrupt(ctx, r)
			c.finish(ctx, r.ID, errors.New("the "+c.spec.what+" ended"))
		}
	}
	perr := c.mgr.Park(ctx, c.pr)
	if perr == nil {
		return nil
	}
	s, err := c.st.LiveSessionByPRRole(ctx, c.pr.ID, c.role.Name)
	if errors.Is(err, store.ErrNotFound) {
		return perr // nothing live to quit: Park's reason stands
	}
	if err != nil {
		return errors.Join(perr, err)
	}
	if err := c.mgr.Quit(ctx, s); err != nil {
		return errors.Join(perr, err)
	}
	ws := store.Deref(s.HerdrWorkspaceID)
	if ws == "" || c.d.CloseWorkspace == nil {
		return nil
	}
	if snap, err := c.d.Herdr.Snapshot(ctx); err != nil || !ownWorkspace(snap, ws, store.Deref(s.HerdrPaneID), c.spec.workspace) {
		return nil // the agent is gone; a workspace someone split stays theirs
	}
	if err := c.d.CloseWorkspace(ctx, ws); err != nil && !herdrGone(err) {
		return err
	}
	return nil
}
