package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/herdr"
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
	// Worktrees is the directory that holds every replay's worktree,
	// Checkout among them (magnum eval: state/eval/wt). A workspace
	// labelled as a replay of this PR whose panes all work under it is one
	// an earlier replay left open, closed before this one starts ("" =
	// Checkout alone).
	Worktrees string
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
// reports or notes. It closes the workspaces earlier replays of the PR left
// open (closeEvalLeftovers), runs the engine's own round preparation
// (preflight, pane environment, workspace "eval <repo>#<N>", agents,
// readiness probes), checks that every session works in that workspace
// (evalSessionsIn) and runs the pipeline, then stops: no pause, refund,
// toast or dismissal follows, those belong to the PR's real rounds. The
// registry rows it seeds (repo, PR in claiming, a per-PR slot at Checkout)
// live in the scratch store.
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
	if err := e.closeEvalLeftovers(ctx, c); err != nil {
		return pipeline.RoundResult{}, job.pr, fmt.Errorf("engine: eval %s/%s#%d: %w", c.Owner, c.Repo, c.Number, err)
	}
	// The daemon's tick is what tells a round that a turn ended, that an
	// interrupted agent went idle, and what answers trust and permission
	// prompts; a replay runs outside that loop, so it observes for itself
	// from the first agent start to the round's end.
	octx, stopObserving := context.WithCancel(ctx)
	observed := make(chan struct{})
	go func() {
		defer close(observed)
		e.safely(octx, "eval observer", "", func() { e.observeEvery(octx, EvalObserveEvery) }, nil)
	}()
	defer func() {
		stopObserving()
		<-observed
	}()
	in, ws, serr := e.prepare(ctx, job)
	if serr != nil {
		return pipeline.RoundResult{}, job.pr, fmt.Errorf("engine: eval %s/%s#%d: setup: %w", c.Owner, c.Repo, c.Number, serr)
	}
	if !in.DryRun || !in.Blind {
		return pipeline.RoundResult{}, job.pr, fmt.Errorf("engine: eval %s/%s#%d: the round is not a blind dry run", c.Owner, c.Repo, c.Number)
	}
	if err := e.evalSessionsIn(ctx, job.pr, ws); err != nil {
		return pipeline.RoundResult{}, job.pr, fmt.Errorf("engine: eval %s/%s#%d: %w", c.Owner, c.Repo, c.Number, err)
	}
	res, err := e.d.Rounds(job.pr.Identity).RunRound(ctx, in)
	return res, job.pr, err
}

// ReleaseEval closes the agents of an eval round's PR and its workspace.
// An agent herdr shows still at work (the judge finishing its turn after it
// wrote its result: turnTail) is interrupted first, the judge with one
// ctrl+c, which ends its turn as a cut own pass does, another agent with
// esc, and awaited up to abortQuietWait. Then the PR is parked
// (agents.Manager.Park, which closes the workspace; the conversations stay
// resumable in the agents' own history). When Park refuses, an agent still
// working, the replay's own workspaces (labelled "eval <repo>#<N>", holding
// the PR's sessions) are closed with whatever still runs there: they hold
// nothing to keep. The error names every workspace that stays open.
func (e *Engine) ReleaseEval(ctx context.Context, pr store.PR) error {
	if e.d.Layout.Scratch == "" {
		return ErrEvalLayout
	}
	if e.interruptEval(ctx, pr) > 0 && !e.waitAgentsQuiet(ctx, pr.ID) {
		e.log.Warn("eval: an agent still works after it was interrupted", "pr", pr.Number, "waited", abortQuietWait)
	}
	perr := e.d.Agents.Park(ctx, pr)
	if perr == nil {
		return nil
	}
	return e.closeEvalWorkspaces(ctx, pr, perr)
}

// evalWorkspaceLabel is the herdr label of a replay's workspace of PR
// <repo>#<number>: "eval <repo>#<N>" (the PR's own is "<repo>#<N>").
func evalWorkspaceLabel(repo string, number int) string {
	return fmt.Sprintf("eval %s#%d", repo, number)
}

// workspaceCloser is the herdr capability that closes a workspace
// (*herdr.Client has it; a Herdr port without it closes nothing).
type workspaceCloser interface {
	WorkspaceClose(ctx context.Context, workspaceID string) error
}

var _ workspaceCloser = (*herdr.Client)(nil)

// closeEvalLeftovers closes the herdr workspaces earlier replays of c's PR
// left open (a judge at work when its case ended, a crash): the ones
// labelled evalWorkspaceLabel whose every pane works under c.Worktrees (else
// c.Checkout). A replay's workspace holds nothing to keep, so what still
// runs there goes with it; a workspace with another label, or with a pane
// elsewhere, is never touched.
func (e *Engine) closeEvalLeftovers(ctx context.Context, c EvalCase) error {
	closer, ok := e.d.Herdr.(workspaceCloser)
	if !ok {
		return nil
	}
	snap, err := e.d.Herdr.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("herdr: %w", err)
	}
	label, root := evalWorkspaceLabel(c.Repo, c.Number), cmp.Or(c.Worktrees, c.Checkout)
	for _, w := range snap.Workspaces {
		if w.Label != label || !panesUnder(snap, w.ID, root) {
			continue
		}
		e.log.Warn("eval: closing the herdr workspace an earlier replay left open", "workspace", w.ID, "label", label)
		if err := closer.WorkspaceClose(ctx, w.ID); err != nil && !herdrGone(err) {
			return fmt.Errorf("close herdr workspace %s (%q), which an earlier replay left open: %w", w.ID, label, err)
		}
	}
	return nil
}

// panesUnder reports whether workspace ws has panes and each works under
// root (its cwd, else its foreground process's; an unknown one does not).
func panesUnder(snap herdr.Snapshot, ws, root string) bool {
	n := 0
	for _, p := range snap.Panes {
		if p.WorkspaceID != ws {
			continue
		}
		if !dirUnder(cmp.Or(p.Cwd, p.ForegroundCwd), root) {
			return false
		}
		n++
	}
	return n > 0
}

// dirUnder reports whether dir is root or below it, as written or with
// symlinks resolved ("" never is).
func dirUnder(dir, root string) bool {
	if dir == "" || root == "" {
		return false
	}
	within := func(d, r string) bool {
		rel, err := filepath.Rel(r, d)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	return within(filepath.Clean(dir), filepath.Clean(root)) || within(fsx.Canon(dir), fsx.Canon(root))
}

// evalSessionsIn checks that every live session of the replay's PR works on
// a pane of ws, the workspace the replay laid out. StartAgent adopts an
// agent that carries the role's name wherever it runs, and one another
// replay left behind would bring its conversation (or the shell it fell
// back to) into this one; the case then fails before the first prompt.
func (e *Engine) evalSessionsIn(ctx context.Context, pr store.PR, ws agents.Workspace) error {
	sessions, err := e.st.SessionsByPR(ctx, pr.ID)
	if err != nil {
		return err
	}
	for _, s := range sessions {
		if s.State != store.SessionLive && s.State != store.SessionStarting {
			continue
		}
		pane, wsID := deref(s.HerdrPaneID), deref(s.HerdrWorkspaceID)
		if wsID == ws.WorkspaceID && pane != "" && slices.Contains(slices.Collect(maps.Values(ws.Panes)), pane) {
			continue // on a pane EnsureWorkspace or EnsurePane laid out
		}
		return fmt.Errorf("the %s session is bound to pane %s of herdr workspace %s, not to this replay's workspace %s "+
			"(an agent another replay left behind?); nothing was prompted", s.Role, cmp.Or(pane, "?"), cmp.Or(wsID, "?"), ws.WorkspaceID)
	}
	return nil
}

// interruptEval interrupts the agents of the PR's live sessions that herdr
// shows working or blocked: the judge with one ctrl+c, another agent with
// esc. It returns how many it sent a key to.
func (e *Engine) interruptEval(ctx context.Context, pr store.PR) int {
	keys, ok := e.d.Herdr.(keySender)
	if !ok {
		return 0
	}
	sessions, err := e.st.SessionsByPR(ctx, pr.ID)
	if err != nil {
		e.log.Warn("eval: close: sessions", "pr", pr.Number, "err", err)
		return 0
	}
	snap, err := e.d.Herdr.Snapshot(ctx)
	if err != nil {
		e.log.Warn("eval: close: herdr snapshot", "pr", pr.Number, "err", err)
		return 0
	}
	n := 0
	for _, s := range sessions {
		if s.State != store.SessionLive && s.State != store.SessionStarting {
			continue
		}
		a, ok := sessionAgent(snap, s)
		if !ok || (a.AgentStatus != herdr.StatusWorking && a.AgentStatus != herdr.StatusBlocked) {
			continue
		}
		key := "esc"
		if r, ok := e.cfg.RoleByNameOrAlias(nil, s.Role); ok && r.Judge {
			key = "ctrl+c"
		}
		if err := keys.AgentSendKeys(ctx, cmp.Or(deref(s.AgentName), a.PaneID), key); err != nil {
			e.log.Warn("eval: close: interrupt", "pr", pr.Number, "role", s.Role, "err", err)
			continue
		}
		n++
	}
	return n
}

// closeEvalWorkspaces closes the replay's own workspaces after Park refused
// (perr): every workspace of the PR's live or lost sessions that herdr still
// has and that carries the replay's label. It returns nil once none is
// left, else perr with each workspace that stays open and why.
func (e *Engine) closeEvalWorkspaces(ctx context.Context, pr store.PR, perr error) error {
	if e.d.Herdr == nil {
		return perr
	}
	sessions, err := e.st.SessionsByPR(ctx, pr.ID)
	if err != nil {
		return fmt.Errorf("%w; its herdr workspace may stay open: %w", perr, err)
	}
	var ids []string
	for _, s := range sessions {
		switch id := deref(s.HerdrWorkspaceID); {
		case id == "" || slices.Contains(ids, id):
		case s.State == store.SessionStarting, s.State == store.SessionLive, s.State == store.SessionLost:
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	repo, err := e.st.RepoByID(ctx, pr.RepoID)
	if err != nil {
		return fmt.Errorf("%w; herdr workspace %s may stay open: %w", perr, strings.Join(ids, ", "), err)
	}
	snap, err := e.d.Herdr.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("%w; herdr workspace %s may stay open: %w", perr, strings.Join(ids, ", "), err)
	}
	label := evalWorkspaceLabel(repo.Name, pr.Number)
	closer, canClose := e.d.Herdr.(workspaceCloser)
	var open []string
	for _, id := range ids {
		i := slices.IndexFunc(snap.Workspaces, func(w herdr.Workspace) bool { return w.ID == id })
		switch {
		case i < 0:
			continue // gone
		case snap.Workspaces[i].Label != label:
			open = append(open, fmt.Sprintf("herdr workspace %s (%q) stays open: it is not this replay's", id, snap.Workspaces[i].Label))
			continue
		case !canClose:
			open = append(open, fmt.Sprintf("herdr workspace %s (%q) stays open", id, label))
			continue
		}
		e.log.Warn("eval: closing the replay's herdr workspace, which Park left open", "workspace", id, "err", perr)
		if err := closer.WorkspaceClose(ctx, id); err != nil && !herdrGone(err) {
			open = append(open, fmt.Sprintf("herdr workspace %s (%q) stays open: %v", id, label, err))
		}
	}
	if len(open) == 0 {
		return nil
	}
	return fmt.Errorf("%w; %s", perr, strings.Join(open, "; "))
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
		pr: pr, repo: repo, watch: c.Watch, slot: sl, hasSlot: true, kind: pipeline.KindInitial,
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
