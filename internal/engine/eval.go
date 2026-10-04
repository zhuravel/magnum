package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// EvalCase is one replay of `magnum eval run`: a PR at a pinned head that the
// caller already checked out (detached) in Checkout, reviewed by the watch's
// roles as a blind dry run. The PR fields come from GitHub; nothing here is
// written to the live registry.
type EvalCase struct {
	Owner, Repo string
	Number      int
	URL         string
	Head        string // the pinned head, checked out in Checkout
	BaseRef     string // the PR's base branch ("" = DefaultBranch)
	// DefaultBranch is the repository's default branch.
	DefaultBranch string
	Title, Author string
	AuthorType    string // "User" or "Bot" ("" = User)
	Checkout      string // absolute path of the worktree at Head
	MainClone     string // the clone the worktree belongs to
	Watch         config.Watch
	Identity      string // the posting identity whose rights the judge checks (it posts nothing)
	// Notes gives the judge the scratch copy of the repository notes
	// (paths.Layout.Notes under Scratch; the caller copies the live file).
	Notes bool
}

// EvalObserveEvery is how often RunEval observes herdr during a replay.
var EvalObserveEvery = 10 * time.Second

// ErrEvalLayout: RunEval needs an engine built on a scratch layout.
var ErrEvalLayout = errors.New("engine: magnum eval needs a scratch layout (paths.Layout.Scratch), never the live registry")

// RunEval runs one blind dry-run round for c and returns the pipeline's
// result and the agents' PR row (for Release). It refuses an engine whose
// layout has no Scratch, so a replay never writes the live registry,
// reports or notes. It runs the engine's own round preparation (preflight,
// pane environment, workspace "eval <repo>#<N>", agents, readiness probes)
// and pipeline, then stops: no pause, refund, toast or dismissal follows,
// those belong to the PR's real rounds. The registry rows it seeds (repo, PR
// in claiming, a per-PR slot at Checkout) live in the scratch store.
func (e *Engine) RunEval(ctx context.Context, c EvalCase) (pipeline.RoundResult, store.PR, error) {
	if e.d.Layout.Scratch == "" {
		return pipeline.RoundResult{}, store.PR{}, ErrEvalLayout
	}
	if c.Head == "" || c.Checkout == "" || c.Number <= 0 || c.URL == "" {
		return pipeline.RoundResult{}, store.PR{}, errors.New("engine: eval case needs a number, URL, head and checkout")
	}
	job, err := e.seedEval(ctx, c)
	if err != nil {
		return pipeline.RoundResult{}, store.PR{}, fmt.Errorf("engine: eval %s/%s#%d: %w", c.Owner, c.Repo, c.Number, err)
	}
	// The daemon's tick is what tells a round that a turn ended, that an
	// interrupted agent went idle, and what answers trust and permission
	// prompts; a replay runs outside that loop, so it observes for itself
	// from the first agent start to the round's end.
	octx, stopObserving := context.WithCancel(ctx)
	observed := make(chan struct{})
	go func() {
		defer close(observed)
		e.observeEvery(octx, EvalObserveEvery)
	}()
	defer func() {
		stopObserving()
		<-observed
	}()
	in, _, serr := e.prepare(ctx, job)
	if serr != nil {
		return pipeline.RoundResult{}, job.pr, fmt.Errorf("engine: eval %s/%s#%d: setup: %w", c.Owner, c.Repo, c.Number, serr)
	}
	if !in.DryRun || !in.Blind {
		return pipeline.RoundResult{}, job.pr, fmt.Errorf("engine: eval %s/%s#%d: the round is not a blind dry run", c.Owner, c.Repo, c.Number)
	}
	res, err := e.d.Rounds(job.pr.Identity).RunRound(ctx, in)
	return res, job.pr, err
}

// ReleaseEval quits the agents of an eval round's PR and closes its
// workspace (agents.Manager.Park); the conversations stay resumable in the
// agents' own history.
func (e *Engine) ReleaseEval(ctx context.Context, pr store.PR) error {
	if e.d.Layout.Scratch == "" {
		return ErrEvalLayout
	}
	return e.d.Agents.Park(ctx, pr)
}

// seedEval writes the case's repo, PR (claiming, forced, dry-run marked) and
// slot (claimed, at Head) into the scratch store and returns the round job.
func (e *Engine) seedEval(ctx context.Context, c EvalCase) (*roundJob, error) {
	if e.d.Identities[c.Identity] == nil {
		return nil, fmt.Errorf("identity %q is not configured", c.Identity)
	}
	repo, err := e.st.UpsertRepo(ctx, store.Repo{
		NodeID: "eval:" + c.Owner + "/" + c.Repo, Owner: c.Owner, Name: c.Repo, WatchOwner: c.Watch.Owner,
		ClonePath: store.Ptr(c.MainClone), DefaultBranch: c.DefaultBranch, Mode: store.RepoModePerPR,
	})
	if err != nil {
		return nil, fmt.Errorf("seed repo: %w", err)
	}
	authorType := c.AuthorType
	if authorType == "" {
		authorType = "User"
	}
	up, err := e.st.UpsertPRFromGitHub(ctx, store.GitHubPR{
		RepoID: repo.ID, NodeID: fmt.Sprintf("eval:%s/%s#%d", c.Owner, c.Repo, c.Number), Number: c.Number, URL: c.URL,
		HeadSHA: c.Head, Title: nonEmptyPtr(c.Title), AuthorLogin: nonEmptyPtr(c.Author), AuthorType: store.Ptr(authorType),
		BaseRef: nonEmptyPtr(c.BaseRef), GHState: store.GHOpen, InitialState: store.PRClaiming, Identity: c.Identity,
	})
	if err != nil {
		return nil, fmt.Errorf("seed PR: %w", err)
	}
	pr := up.PR
	if !up.New {
		return nil, errors.New("the scratch registry already holds this PR: one eval round per scratch layout")
	}
	if err := e.st.TransitionPR(ctx, pr.ID, []string{store.PRClaiming}, store.PRClaiming, func(u *store.PRUpdate) {
		u.Set("forced", true)
	}); err != nil {
		return nil, fmt.Errorf("mark PR forced: %w", err)
	}
	if pr, err = e.st.PRByID(ctx, pr.ID); err != nil {
		return nil, err
	}
	if err := e.st.SetKV(ctx, kvPRDryRun(pr.ID), store.PRQueued); err != nil {
		return nil, fmt.Errorf("mark dry run: %w", err)
	}
	sl, err := e.st.CreateSlot(ctx, store.Slot{
		Name: fmt.Sprintf("eval-%s-%d", c.Repo, c.Number), RepoID: &repo.ID, RepoFullName: repo.FullName(), Kind: store.SlotKindPerPR,
		Path: c.Checkout, MainClone: c.MainClone, State: store.SlotClaimed, PRID: &pr.ID, CheckedOutSHA: store.Ptr(c.Head),
	})
	if err != nil {
		return nil, fmt.Errorf("seed slot: %w", err)
	}
	return &roundJob{
		pr: pr, repo: repo, watch: c.Watch, slot: sl, hasSlo: true, kind: pipeline.KindInitial,
		evalHead: c.Head, evalNotes: c.Notes,
	}, nil
}

// observeEvery observes herdr (observe) every interval until ctx ends.
func (e *Engine) observeEvery(ctx context.Context, interval time.Duration) {
	for ctx.Err() == nil {
		e.observe(ctx)
		if err := e.d.Sleep(ctx, interval); err != nil {
			return
		}
	}
}

func nonEmptyPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
