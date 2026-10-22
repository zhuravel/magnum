package engine

// The notes critic (DECISIONS "The notes critic decides curation
// proposals"): with [notes] critic on, an agent on the strongest model
// ([notes] critic_model, its fallbacks critic_fallback_models) reviews each
// curation proposal the way the operator does with `magnum notes <repo>
// --review`, from copies in its run directory (<notes>/.critic/<owner>/
// <repo>/<run>), and writes decision.json: apply, reject or recurate, with
// the fingerprint of the state it reviewed, a one-line reason and its
// checked and losses lists. Magnum validates the answer (one nudge), then
// applies the proposal through the same claim-write-record path as the
// operator's y (ApplyNotesProposal: the notes must still be what the critic
// saw, the state the one it named, and nothing may be lost), rejects it or
// supersedes it, and marks the repository for a new curation that reads the
// reason. The registry keeps every critique (notes_critiques). One
// critique runs at a time, a learn job like a curation.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

const (
	// CurateTriggerReview is the curation that follows a proposal the
	// critic rejected or sent back (KVNotesRecurate): due at once, under the
	// daemon's own gates, never the operator's request.
	CurateTriggerReview = "review"
	// KVNotesCritic holds the critique running now (CriticMark as JSON),
	// deleted when it ends and at every start of the daemon.
	KVNotesCritic = "notes.critic"
	// CriticMaxAttempts is how many undecided critiques a proposal gets
	// before it is left to the operator for good.
	CriticMaxAttempts = 2
	// criticCloseTimeout bounds closing the critic's agent.
	criticCloseTimeout = retroCloseTimeout
)

// KVNotesRecurate marks a repository whose proposal the critic rejected or
// sent back: the proposal's id. The next curation scan starts a curation of
// trigger review (dueCurations); a curation that stores its proposal clears
// it.
func KVNotesRecurate(fullName string) string {
	return "notes." + strings.ToLower(fullName) + ".recurate"
}

// KVNotesCriticOver marks a repository whose notes a critic apply left past
// a limit: the apply's time. Its next over_limit curation waits a week
// from it, so a busy repository does not cost a curation and a critique a
// day.
func KVNotesCriticOver(fullName string) string {
	return "notes." + strings.ToLower(fullName) + ".critic_over"
}

// CriticMark is the critique running now (KVNotesCritic).
type CriticMark struct {
	Repo     string    `json:"repo"`
	Proposal int64     `json:"proposal"`
	Model    string    `json:"model"`
	Started  time.Time `json:"started"`
}

// ReadCritic is the critique running now, nil when none runs (or the mark
// is unreadable).
func ReadCritic(ctx context.Context, st *store.Store) *CriticMark {
	v, ok, err := st.GetKV(ctx, KVNotesCritic)
	if err != nil || !ok || v == "" {
		return nil
	}
	var m CriticMark
	if json.Unmarshal([]byte(v), &m) != nil || m.Repo == "" {
		return nil
	}
	return &m
}

// CriticRun is one critique: its id and run directory.
type CriticRun struct {
	ID  string
	Dir string
}

// CritiqueJob is what a Critic works on: the run directory, the rendered
// prompt (prompts/notes-critic.md) and Check, which reads decision.json and
// validates it (no problems: a valid decision).
type CritiqueJob struct {
	Repo   string
	Run    notes.CriticRun
	Prompt string
	Check  func() (notes.Decision, []string)
}

// CritiqueResult is how a critic's turns ended: a decision, valid when
// Problems is empty, or a Pause (a limit the agent hit). Model is the model
// the answer came from ("" = the role's).
type CritiqueResult struct {
	Decision notes.Decision
	Problems []string
	Pause    *pipeline.Pause
	Model    string
}

// Critic critiques one proposal; Close ends it.
type Critic interface {
	Critique(ctx context.Context, job CritiqueJob) (CritiqueResult, error)
	Close(ctx context.Context) error
}

// ErrCriticDown wraps a Critic's error that is not the decision's: the
// agent could not be set up or started, or went away.
var ErrCriticDown = errors.New("engine: the notes critic is not available")

// criticPane is the notes critic's pane agent.
var criticPane = paneSpec{what: "notes critique", workspace: "learn critic", owner: learnRepoOwner, name: config.CriticRoleName,
	role: (*config.Config).CriticRole}

// paneCritics is Deps.Critic for the pane agent: one agent per critique.
func paneCritics(d paneDeps) func(context.Context, CriticRun) (Critic, error) {
	d = d.withDefaults()
	return func(_ context.Context, run CriticRun) (Critic, error) {
		return &paneCritic{paneAgent: &paneAgent{d: d, spec: criticPane, dir: run.Dir, id: run.ID}}, nil
	}
}

// paneCritic is one critique's agent.
type paneCritic struct{ *paneAgent }

// Critique prompts the agent with job's prompt and waits for its turn to
// end; a decision that is missing or invalid gets one nudge listing its
// problems. A model limit starts the agent again on its next fallback model
// ([notes] critic_fallback_models) with the first prompt, once (afterTurn);
// a usage limit, a logout, an overload or a model limit with no fallback
// left is a Pause; an agent that cannot be set up or started, or went away,
// is ErrCriticDown. The result's Model is the model the answer came from.
func (c *paneCritic) Critique(ctx context.Context, job CritiqueJob) (CritiqueResult, error) {
	if err := c.setup(ctx); err != nil {
		return CritiqueResult{}, fmt.Errorf("%w: set up: %w", ErrCriticDown, err)
	}
	if pause, err := c.ensureAgent(ctx); err != nil {
		if pause != nil || ctx.Err() != nil {
			return CritiqueResult{Pause: pause}, err
		}
		return CritiqueResult{}, fmt.Errorf("%w: start: %w", ErrCriticDown, err)
	}
	text := job.Prompt
	for attempt := 0; ; attempt++ {
		t := c.turn(ctx, attempt, text)
		d, problems := job.Check()
		if len(problems) == 0 {
			return CritiqueResult{Decision: d, Model: c.currentModel(ctx)}, nil
		}
		switch restarted, pause, err := c.afterTurn(ctx, t, job.Run.Decision()); {
		case pause != nil:
			return CritiqueResult{Pause: pause, Model: c.currentModel(ctx)}, fmt.Errorf("the notes critic stopped: %s", pause.Kind)
		case err != nil && ctx.Err() == nil:
			return CritiqueResult{}, fmt.Errorf("%w: %w", ErrCriticDown, err)
		case restarted: // a fresh session: the first prompt, the nudge count kept
			text, attempt = job.Prompt, attempt-1
			continue
		}
		switch {
		case ctx.Err() != nil:
			return CritiqueResult{}, ctx.Err()
		case t.down:
			return CritiqueResult{}, fmt.Errorf("%w: %w", ErrCriticDown, t.err)
		case attempt > 0:
			return CritiqueResult{Decision: d, Problems: problems, Model: c.currentModel(ctx)}, nil
		}
		if t.err != nil {
			c.d.Logger.Warn("notes critique: the agent's turn did not end well; nudging", "run", c.id, "err", t.err)
		}
		text = criticNudge(job, problems)
	}
}

// criticNudge is the one reminder after a turn without a valid decision:
// the decision file's path and magnum's own list of problems.
func criticNudge(job CritiqueJob, problems []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The decision in `%s` is not valid yet:\n", job.Run.Decision())
	for _, p := range problems {
		fmt.Fprintf(&b, "- %s\n", p)
	}
	fmt.Fprintf(&b, "Fix it as the first prompt describes. Write `%s.tmp` first, then move it to `%s`, then stop.", job.Run.Decision(), job.Run.Decision())
	return b.String()
}

// criticData feeds the critic's prompt (prompts/notes-critic.md): paths
// under the critique's run directory, ids, a fingerprint, flags and limits,
// never notes text. The copies are frozen for the critique, so their parts
// (agents.SplitNotes) stay valid while the critic reads them.
type criticData struct {
	Repo string
	// Dir is the run directory; the critic works in its parent, and every
	// file below is a copy inside it.
	Dir             string
	Review          string
	Rejections      string
	Current         string
	CurrentHarness  string
	Proposed        string
	ProposedHarness string
	Changes         string
	Decision        string
	// Diff and ArchiveDiff are the unified diffs from the notes now to what
	// an apply writes (notes.diff, archive.diff; no parts when the archive
	// does not change); CurrentParts and ProposedParts the parts of
	// current.md and proposed.md.
	Diff, ArchiveDiff           agents.NotesParts
	CurrentParts, ProposedParts agents.NotesParts
	ProposalID                  int64
	// Target is the fingerprint of the state the critic reviews: what an
	// apply writes (the proposed state, or a stale one's clean merge), else
	// the proposed state. decision.json names it.
	Target string
	// Stale: the notes changed since the proposal was made; Conflict: and
	// its merge conflicts; Lossy: an apply loses lines (not_preserved);
	// Blocked: Conflict or Lossy, so only reject and recurate are valid.
	Stale, Conflict, Lossy, Blocked bool
	Limits                          notes.Limits
	MaxReasonRunes                  int
}

// Files of a critique's run directory besides notes.CriticRun's.
const (
	criticDiffFile        = "notes.diff"
	criticArchiveDiffFile = "archive.diff"
)

// criticRejection is one entry of rejections.json.
type criticRejection struct {
	Proposal int64     `json:"proposal"`
	State    string    `json:"state"` // rejected | superseded
	By       string    `json:"by,omitempty"`
	At       time.Time `json:"at"`
	Reason   string    `json:"reason"`
}

// maybeCritic starts a critique of the oldest curation proposal that
// qualifies (nextCritique), every tick, unless the critic is off, the
// daemon runs once or dry, another learn job runs, the daily retro is due
// and unrun, the daily cap is reached, a hold or the critic's CLI pause
// holds it, or a pause holds the daemon's own work (a proposal of the
// operator's --curate is critiqued under `magnum pause`, as its curation
// ran there).
func (e *Engine) maybeCritic(ctx context.Context) {
	if !e.cfg.Notes.Critic || e.d.Critic == nil || e.d.DryRun || e.once || e.learnBusy() != "" || e.holdReason(ctx) != "" ||
		e.criticToolPause(ctx) != "" || e.retroWaiting(ctx) {
		return
	}
	if _, low := e.githubLowAny(); low {
		return
	}
	now := e.now()
	if n, err := e.st.CountCritiquesSince(ctx, localDayStart(now)); err != nil || n >= e.cfg.Notes.CriticPerDay {
		return
	}
	repo, p, ok := e.nextCritique(ctx, now)
	if !ok {
		return
	}
	e.startCritique(ctx, repo, p)
}

// localDayStart is the start of now's local day.
func localDayStart(now time.Time) time.Time {
	y, m, d := now.Local().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
}

// retroWaiting reports whether the daily retro goes first: due and unrun
// today, or queued.
func (e *Engine) retroWaiting(ctx context.Context) bool {
	if _, ok := e.queuedRetro(ctx); ok {
		return true
	}
	lc := e.cfg.Learn
	if !lc.Enabled {
		return false
	}
	now := e.now()
	at, err := lc.DailyTime(now.Local())
	if err != nil || now.Before(at) {
		return false
	}
	day, _ := e.getKV(ctx, KVRetroDay)
	return day != store.DayKey(now)
}

// criticToolPause is why the critic cannot work now ("" = it can): its
// CLI is paused.
func (e *Engine) criticToolPause(ctx context.Context) string {
	if e.d.Critic == nil {
		return ""
	}
	return e.kindPauseReason(ctx, []string{e.cfg.CriticRole().AgentKind()})
}

// nextCritique is the oldest pending curation proposal the critic may take
// now: a curator's (not a restore, not a recovery), with fewer than
// criticAttempts undecided critiques, no stop within curateRetry, its
// repository below [notes] critic_declines in a row and no round of it in
// its judge stage (no queue: a later tick tries again), and allowed by
// `magnum pause` (only the operator's --curate proposals are critiqued
// under it).
func (e *Engine) nextCritique(ctx context.Context, now time.Time) (store.Repo, store.NotesProposal, bool) {
	pending, err := e.st.NotesProposals(ctx, store.NotesProposalFilter{States: []string{store.ProposalPending}})
	if err != nil {
		e.warnUnlessStopped(ctx, err, "notes critic: list the waiting proposals")
		return store.Repo{}, store.NotesProposal{}, false
	}
	slices.SortFunc(pending, func(a, b store.NotesProposal) int { return cmp.Compare(a.ID, b.ID) })
	paused := e.userPause(ctx) != ""
	for _, p := range pending {
		if p.Kind != store.ProposalCuration || p.Trigger == CurateTriggerRecover || (paused && p.Trigger != CurateTriggerRequest) ||
			e.criticTriedRecently(p.ID, now) {
			continue
		}
		repo, err := e.st.RepoByID(ctx, p.RepoID)
		if err != nil {
			continue
		}
		if _, ok := notesRepo(e.d.Layout, repo.Owner, repo.Name); !ok {
			continue
		}
		if n, _ := CriticAttempts(ctx, e.st, p.ID); n >= CriticMaxAttempts {
			continue
		}
		if CriticDeclines(ctx, e.st, repo.ID) >= e.cfg.Notes.CriticDeclines || e.notesJudging(ctx, repo.ID) {
			continue
		}
		return repo, p, true
	}
	return store.Repo{}, store.NotesProposal{}, false
}

// CriticAttempts counts proposal id's undecided critiques: at
// CriticMaxAttempts the proposal is left to the operator for good.
func CriticAttempts(ctx context.Context, st *store.Store, id int64) (int, error) {
	cs, err := st.NotesCritiques(ctx, store.NotesCritiqueFilter{ProposalID: id})
	n := 0
	for _, c := range cs {
		if c.Outcome == store.CritiqueUndecided {
			n++
		}
	}
	return n, err
}

// CriticDeclines counts the proposals of repoID the critic rejected or
// sent back in a row, newest first: an apply by anyone or a decision of the
// operator ends the run; magnum's own decisions (an expiry, an invalid
// proposal, a stale follow-up) and waiting proposals neither count nor end
// it. At [notes] critic_declines the critic steps back: the repository's
// next proposal waits for the operator.
func CriticDeclines(ctx context.Context, st *store.Store, repoID int64) int {
	ps, err := st.NotesProposals(ctx, store.NotesProposalFilter{RepoID: repoID, Limit: 50})
	if err != nil {
		return 0
	}
	n := 0
	for _, p := range ps {
		switch {
		case p.State == store.ProposalApplied || p.DecidedBy == store.DecidedByOperator:
			return n
		case p.DecidedBy == store.DecidedByCritic && (p.State == store.ProposalRejected || p.State == store.ProposalSuperseded):
			n++
		}
	}
	return n
}

func (e *Engine) criticTriedRecently(id int64, now time.Time) bool {
	e.criticMu.Lock()
	defer e.criticMu.Unlock()
	t, ok := e.criticTried[id]
	return ok && now.Sub(t) < curateRetry
}

// startCritique runs a critique of p in its own goroutine on a child of
// ctx, unless the daemon is stopping or another learn job runs (false).
// KVNotesCritic holds it while it runs.
func (e *Engine) startCritique(ctx context.Context, repo store.Repo, p store.NotesProposal) bool {
	if e.stopping(ctx) != "" {
		return false
	}
	e.learnMu.Lock()
	defer e.learnMu.Unlock()
	if job, _ := e.learnRunning(); job != "" {
		return false
	}
	e.criticMu.Lock()
	defer e.criticMu.Unlock()
	cctx, cancel := context.WithCancel(ctx)
	model := e.cfg.RoleModel(e.cfg.CriticRole())
	e.criticCancel, e.criticStarted, e.criticRepo, e.criticProposal = cancel, e.now(), repo.FullName(), p.ID
	if b, err := json.Marshal(CriticMark{Repo: repo.FullName(), Proposal: p.ID, Model: model, Started: e.criticStarted}); err == nil {
		e.setKV(ctx, KVNotesCritic, string(b))
	}
	started := e.criticStarted
	e.criticWG.Go(func() {
		defer func() {
			e.delKV(context.WithoutCancel(ctx), KVNotesCritic)
			e.criticMu.Lock()
			e.criticCancel = nil
			e.criticMu.Unlock()
			cancel()
		}()
		e.safely(cctx, "notes critique", notesSubjectOf(repo.FullName()), func() { e.runCritique(cctx, repo, p) },
			func(context.Context, string) {
				e.criticMu.Lock()
				e.criticTried[p.ID] = started
				e.criticMu.Unlock()
			})
	})
	return true
}

// stopCritic cancels a running critique (shutdown).
func (e *Engine) stopCritic() {
	e.criticMu.Lock()
	defer e.criticMu.Unlock()
	if e.criticCancel != nil {
		e.criticCancel()
	}
}

// criticRunning names the critique running ("" = none) and when it started.
func (e *Engine) criticRunning() (string, time.Time) {
	e.criticMu.Lock()
	defer e.criticMu.Unlock()
	if e.criticCancel == nil {
		return "", time.Time{}
	}
	return fmt.Sprintf("the notes critique of %s (proposal %d)", e.criticRepo, e.criticProposal), e.criticStarted
}

// critique is one critique's state while it runs.
type critique struct {
	e       *Engine
	repo    store.Repo
	nr      notes.Repo
	p       store.NotesProposal
	run     CriticRun
	id      int64 // notes_critiques row
	model   string
	subject string
	full    string
	start   time.Time
}

// runCritique is one critique of p: its inputs copied into a run directory,
// the critic's turns, and what the daemon does with the decision. A stop
// that is not the critic's answer (no agent, a pause, a shutdown) records
// the critique stopped and is tried again after curateRetry; an answer
// still invalid after the nudge is an attempt.
func (e *Engine) runCritique(ctx context.Context, repo store.Repo, p store.NotesProposal) {
	nr, ok := notesRepo(e.d.Layout, repo.Owner, repo.Name)
	if !ok {
		return
	}
	start := e.now()
	c := &critique{e: e, repo: repo, nr: nr, p: p, model: e.cfg.RoleModel(e.cfg.CriticRole()), subject: notesSubject(nr),
		full: nr.FullName(), start: start}
	c.run = criticRunAt(nr, start)
	octx := context.WithoutCancel(ctx)

	in, err := e.critiqueInputs(ctx, repo, nr, p, c.run)
	id, serr := e.st.StartCritique(octx, store.CritiqueStart{ProposalID: p.ID, RepoID: repo.ID, RunID: c.run.ID, RunDir: c.run.Dir,
		Model: c.model, Target: in.data.Target, At: start})
	if serr != nil {
		e.log.Warn("notes critique: record its start", "proposal", p.ID, "err", serr)
		e.markCriticTried(p.ID, start)
		return
	}
	c.id = id
	e.event(octx, "info", c.subject, "notes.critic_start", fmt.Sprintf("notes critique of proposal %d for %s started (%s)", p.ID, c.full, c.model),
		map[string]any{"run": c.run.ID, "proposal": p.ID, "model": c.model})
	if err != nil {
		c.stop(octx, "its inputs: "+oneLine(err.Error(), retroWhyRunes))
		return
	}
	if e.d.Critic == nil {
		c.stop(octx, "no critic agent (herdr is not configured)")
		return
	}
	cr, err := e.d.Critic(ctx, c.run)
	if err != nil {
		c.stop(octx, "the critic cannot start: "+oneLine(err.Error(), retroWhyRunes))
		return
	}
	defer func() {
		cctx, cancel := context.WithTimeout(octx, criticCloseTimeout)
		defer cancel()
		if err := cr.Close(cctx); err != nil {
			e.event(cctx, "warn", c.subject, "notes.critic_close", "notes critique: closing the critic: "+oneLine(err.Error(), retroWhyRunes), nil)
		}
	}()
	res, err := cr.Critique(ctx, CritiqueJob{Repo: c.full, Run: in.run, Prompt: in.prompt, Check: func() (notes.Decision, []string) {
		d, _, problems := notes.ReadDecision(in.run)
		if len(problems) > 0 {
			return d, problems
		}
		return d, notes.ValidateDecision(d, in.check)
	}})
	c.model = cmp.Or(res.Model, c.model)
	switch {
	case res.Pause != nil:
		if res.Pause.Kind == string(agents.HealthUsageLimit) || res.Pause.Kind == string(agents.HealthLoginRequired) {
			e.pauseTool(octx, *res.Pause)
		}
		c.stop(octx, fmt.Sprintf("%s: %s", res.Pause.Tool, res.Pause.Kind))
		return
	case ctx.Err() != nil:
		c.stop(octx, "the daemon is stopping")
		return
	case err != nil:
		c.stop(octx, oneLine(err.Error(), retroWhyRunes))
		return
	case len(res.Problems) > 0:
		c.undecided(octx, res.Problems)
		return
	}
	c.act(octx, res.Decision, in)
}

// criticRunAt names a critique that starts at start: its run directory
// under nr's critiques, named by the local time, with a suffix when another
// critique of the same second has it (a critique the notes' change sent
// back starts again at the next tick).
func criticRunAt(nr notes.Repo, start time.Time) CriticRun {
	id := start.Local().Format(curateRunFormat)
	for n := 2; ; n++ {
		if _, err := os.Stat(filepath.Join(nr.Critic(), id)); os.IsNotExist(err) {
			return CriticRun{ID: id, Dir: filepath.Join(nr.Critic(), id)}
		}
		id = start.Local().Format(curateRunFormat) + "-" + strconv.Itoa(n)
	}
}

// markCriticTried keeps id from a critique until curateRetry passed.
func (e *Engine) markCriticTried(id int64, at time.Time) {
	e.criticMu.Lock()
	e.criticTried[id] = at
	e.criticMu.Unlock()
}

// critiqueIn is what a critique works from: its run directory, prompt and
// the check its decision gets.
type critiqueIn struct {
	run    notes.CriticRun
	prompt string
	data   criticData
	check  notes.DecisionCheck
	live   string // the notes' fingerprint the critique saw
}

// critiqueInputs copies what the critic reads into its run directory (the
// review data with its diffs apart, the earlier rejections, the notes now
// and the proposal) and renders its prompt.
func (e *Engine) critiqueInputs(ctx context.Context, repo store.Repo, nr notes.Repo, p store.NotesProposal, run CriticRun) (critiqueIn, error) {
	var in critiqueIn
	view, err := ReviewProposal(ctx, e.st, e.cfg.Notes.MaxLine, repo.FullName(), nr, p)
	if err != nil {
		return in, err
	}
	target, ok := ApplyTarget(view.Check)
	if !ok {
		target = view.Check.Proposed
	}
	in.live = view.Check.Live.Fingerprint()
	diff, archiveDiff := view.Diff, view.ArchiveDiff
	view.Diff, view.ArchiveDiff = "", ""
	review, err := json.MarshalIndent(struct {
		NotesReviewView
		DiffFile        string `json:"notes_diff_file"`
		ArchiveDiffFile string `json:"archive_diff_file,omitempty"`
	}{view, criticDiffFile, map[bool]string{true: criticArchiveDiffFile}[archiveDiff != ""]}, "", "  ")
	if err != nil {
		return in, err
	}
	rejections, err := e.criticRejections(ctx, repo.ID)
	if err != nil {
		return in, err
	}
	in.run, err = notes.PrepareCritic(run.Dir, view.Check.Live, view.Check.Proposed, p.Changes, review, rejections)
	if err != nil {
		return in, err
	}
	d := criticData{Repo: repo.FullName(), Dir: run.Dir, Review: in.run.Review(), Rejections: in.run.Rejections(), Current: in.run.Current(),
		CurrentHarness: in.run.CurrentHarness(), Proposed: in.run.Proposed(), ProposedHarness: in.run.ProposedHarness(),
		Changes: in.run.Changes(), Decision: in.run.Decision(), ProposalID: p.ID, Target: target.Fingerprint(), Stale: view.Stale,
		Conflict: view.Stale && !ok, Lossy: len(view.NotPreserved) > 0, Limits: e.notesLimits(), MaxReasonRunes: notes.MaxReasonRunes}
	d.Blocked = d.Conflict || d.Lossy
	d.CurrentParts = agents.SplitNotes(in.run.Current(), view.Check.Live.Notes)
	d.ProposedParts = agents.SplitNotes(in.run.Proposed(), view.Check.Proposed.Notes)
	for _, f := range []struct {
		name  string
		text  string
		parts *agents.NotesParts
	}{{criticDiffFile, diff, &d.Diff}, {criticArchiveDiffFile, archiveDiff, &d.ArchiveDiff}} {
		if f.text == "" && f.name == criticArchiveDiffFile {
			continue
		}
		path := filepath.Join(run.Dir, f.name)
		if err := fsx.WriteFileAtomic(path, []byte(f.text), 0o600); err != nil {
			return in, err
		}
		*f.parts = agents.SplitNotes(path, []byte(f.text))
	}
	pr, err := e.cfg.ResolvePrompt(e.cfg.Notes.CriticPrompt)
	if err != nil {
		return in, fmt.Errorf("the critic's prompt cannot be read: %w", err)
	}
	if in.prompt, err = agents.RenderPrompt(pr, d); err != nil {
		return in, fmt.Errorf("the critic's prompt does not render: %w", err)
	}
	leaks, err := e.criticLeaks(ctx, repo)
	if err != nil {
		return in, err
	}
	in.data = d
	in.check = notes.DecisionCheck{Target: d.Target, Conflict: d.Conflict, Lossy: d.Lossy, Leaks: leaks}
	return in, nil
}

// criticRejections is rejections.json: the reasons of the repository's last
// curateRejections rejected or superseded curations (by a person or the
// critic; magnum's own supersedes of stale proposals say nothing of their
// content), with who decided each.
func (e *Engine) criticRejections(ctx context.Context, repoID int64) ([]byte, error) {
	ps, err := e.st.NotesProposals(ctx, store.NotesProposalFilter{RepoID: repoID, States: []string{store.ProposalRejected, store.ProposalSuperseded},
		Limit: 50})
	if err != nil {
		return nil, err
	}
	out := []criticRejection{}
	for _, p := range ps {
		if len(out) == curateRejections {
			break
		}
		if p.Kind != store.ProposalCuration || p.Trigger == CurateTriggerRecover || p.Reason == "" || p.DecidedBy == store.DecidedByMagnum {
			continue
		}
		out = append(out, criticRejection{Proposal: p.ID, State: p.State, By: p.DecidedBy, At: store.Deref(p.DecidedAt), Reason: p.Reason})
	}
	return json.MarshalIndent(out, "", "  ")
}

// criticLeaks is the critic reason's leak check (the one a revert's reason
// gets): a pull request reference, a link, a login of the registry's pull
// requests or magnum's, a watched repository's owner or name
// (learn.ScrubLesson), then a pull request number, a branch of the
// repository's pull requests, a probe file, a secret or a home path
// (notes.ReasonLeaks).
func (e *Engine) criticLeaks(ctx context.Context, repo store.Repo) (func(string) []string, error) {
	people, err := e.st.PeopleLogins(ctx)
	if err != nil {
		return nil, err
	}
	repos, err := e.st.ListRepos(ctx)
	if err != nil {
		return nil, err
	}
	var own, names []string
	for _, id := range e.cfg.Identities {
		own = append(own, id.Login)
	}
	for _, r := range repos {
		names = append(names, r.Owner, r.Name)
	}
	check, err := e.curateCheck(ctx, repo, notes.State{})
	if err != nil {
		return nil, err
	}
	return func(text string) []string {
		var out []string
		_, why := learn.ScrubLesson(text, store.MissScopeGeneral, people, own, names)
		if what := map[string]string{learn.LessonIssueRef: "names a pull request", learn.LessonURL: "holds a link", learn.LessonLogin: "names a login",
			learn.LessonNamesRepo: "names a repository"}[why]; what != "" {
			out = append(out, what)
		}
		for _, l := range notes.ReasonLeaks(text, check) {
			if !slices.Contains(out, l) {
				out = append(out, l)
			}
		}
		return out
	}, nil
}

// end records how the critique ended (best effort: logged).
func (c *critique) end(ctx context.Context, in store.CritiqueEnd) {
	in.Model = cmp.Or(in.Model, c.model)
	if err := c.e.st.EndCritique(ctx, c.id, in); err != nil {
		c.e.log.Warn("notes critique: record its end", "proposal", c.p.ID, "err", err)
	}
}

// stop ends a critique that is no answer of the critic: uncounted, tried
// again after curateRetry.
func (c *critique) stop(ctx context.Context, why string) {
	c.e.markCriticTried(c.p.ID, c.start)
	c.end(ctx, store.CritiqueEnd{Outcome: store.CritiqueStopped, Detail: why})
	c.e.event(ctx, "warn", c.subject, "notes.critic_stopped", fmt.Sprintf("notes critique of proposal %d for %s stopped: %s", c.p.ID, c.full, why),
		map[string]any{"run": c.run.ID, "proposal": c.p.ID})
}

// undecided ends a critique whose answer was still invalid after the
// nudge: an attempt. The second leaves the proposal to the operator, with a
// toast.
func (c *critique) undecided(ctx context.Context, problems []string) {
	c.end(ctx, store.CritiqueEnd{Outcome: store.CritiqueUndecided, Detail: textx.Clip(strings.Join(problems, "; "), curateReasonRunes)})
	n, _ := CriticAttempts(ctx, c.e.st, c.p.ID)
	c.e.event(ctx, "warn", c.subject, "notes.critic_undecided", fmt.Sprintf("the notes critic did not decide proposal %d for %s (attempt %d of %d, %s)",
		c.p.ID, c.full, n, CriticMaxAttempts, textx.Count(len(problems), "problem", "problems")),
		map[string]any{"run": c.run.ID, "proposal": c.p.ID, "attempt": n, "problems": len(problems)})
	if n >= CriticMaxAttempts {
		title := fmt.Sprintf("the critic did not decide proposal %d of %s", c.p.ID, c.full)
		c.e.info(notify.Item{Key: fmt.Sprintf("notes-critic-undecided:%d", c.p.ID), Title: title,
			Body: "Review it with `magnum notes " + c.full + " --review`.", Line: title})
	}
}

// act does what the critic decided: apply (through the claim, the write and
// the record; notes that changed since the critique send the proposal to a
// new critique), reject or recurate (and mark the repository for a new
// curation, unless the critic stepped back).
func (c *critique) act(ctx context.Context, d notes.Decision, in critiqueIn) {
	e := c.e
	reason := strings.TrimSpace(d.Reason)
	rec := store.CritiqueEnd{Decision: d.Decision, Reason: reason, Checked: d.Checked, Losses: d.Losses}
	var err error
	switch d.Decision {
	case notes.DecisionApply:
		var applied store.NotesProposal
		applied, err = ApplyNotesProposal(ctx, e.st, c.nr, c.p, NotesApply{Live: in.live, Target: d.Target, By: store.DecidedByCritic, Reason: reason,
			Now: e.now()})
		var lost *NotPreservedError
		switch {
		case errors.Is(err, ErrNotesChanged) || errors.Is(err, ErrTargetChanged) || errors.Is(err, ErrStaleConflict):
			rec.Outcome, rec.Detail = store.CritiqueChanged, oneLine(err.Error(), retroWhyRunes)
			c.end(ctx, rec)
			e.event(ctx, "info", c.subject, "notes.critic_changed", fmt.Sprintf("the notes of %s changed since the critique of proposal %d: "+
				"it is critiqued again against them", c.full, c.p.ID), map[string]any{"run": c.run.ID, "proposal": c.p.ID})
			return
		case errors.As(err, &lost):
			c.undecided(ctx, lost.Problems)
			return
		case err == nil:
			rec.Outcome = store.CritiqueApplied
			c.end(ctx, rec)
			c.decided(ctx, "applied", reason, c.overLimit(ctx, applied))
			return
		}
	case notes.DecisionReject:
		_, err = e.st.DecideNotesProposal(ctx, c.p.ID, []string{store.ProposalPending}, store.ProposalRejected, store.DecidedByCritic, reason, e.now(), nil)
		if err == nil {
			rec.Outcome = store.CritiqueRejected
		}
	case notes.DecisionRecurate:
		_, err = e.st.DecideNotesProposal(ctx, c.p.ID, []string{store.ProposalPending}, store.ProposalSuperseded, store.DecidedByCritic, reason, e.now(), nil)
		if err == nil {
			rec.Outcome = store.CritiqueRecurated
		}
	}
	var decided *ProposalDecidedError
	switch {
	case errors.As(err, &decided) || errors.Is(err, store.ErrConflict):
		rec.Outcome, rec.Detail = store.CritiqueOvertaken, "decided first by someone else"
		c.end(ctx, rec)
		e.event(ctx, "info", c.subject, "notes.critic_overtaken", fmt.Sprintf("proposal %d for %s was decided while the critic worked on it; "+
			"its decision (%s) was dropped", c.p.ID, c.full, d.Decision), map[string]any{"run": c.run.ID, "proposal": c.p.ID})
		return
	case err != nil:
		c.stop(ctx, d.Decision+": "+oneLine(err.Error(), retroWhyRunes))
		return
	}
	c.end(ctx, rec)
	what := map[string]string{notes.DecisionReject: "rejected", notes.DecisionRecurate: "re-curated"}[d.Decision]
	if n := CriticDeclines(ctx, e.st, c.repo.ID); n >= e.cfg.Notes.CriticDeclines {
		c.decided(ctx, what, reason, fmt.Sprintf(" The critic declined %s in a row and stepped back: the next proposal of %s waits for you.",
			textx.Count(n, "proposal", "proposals"), c.full))
		return
	}
	e.setKV(ctx, KVNotesRecurate(c.full), strconv.FormatInt(c.p.ID, 10))
	c.decided(ctx, what, reason, " A new curation runs.")
}

// decided writes the decision's event and its toast, the one toast per
// proposal while the critic is on.
func (c *critique) decided(ctx context.Context, what, reason, more string) {
	msg := fmt.Sprintf("proposal %d of %s %s by the critic (%s)", c.p.ID, c.full, what, c.model)
	full := msg
	if reason != "" {
		full += ": " + reason
	}
	c.e.event(ctx, "info", c.subject, "notes.critic_decided", full+"."+more, map[string]any{"run": c.run.ID, "proposal": c.p.ID, "decision": what,
		"model": c.model})
	c.e.info(notify.Item{Key: fmt.Sprintf("notes-critic:%d", c.p.ID), Title: msg, Body: oneLine(cmp.Or(reason, what)+".", 240) + more, Line: msg})
}

// overLimit marks a repository whose notes the critic's apply left past a
// limit (KVNotesCriticOver: its next over_limit curation waits a week) and
// says so for the toast ("" when within every limit).
func (c *critique) overLimit(ctx context.Context, applied store.NotesProposal) string {
	if applied.AppliedVersionID == nil {
		return ""
	}
	content, err := c.e.st.NotesVersionContent(ctx, *applied.AppliedVersionID)
	if err != nil {
		return ""
	}
	l := c.e.notesLimits()
	over := names(StateOf(content).Size(l.MaxLine).Over(l))
	if len(over) == 0 {
		c.e.delKV(ctx, KVNotesCriticOver(c.full))
		return ""
	}
	c.e.setKV(ctx, KVNotesCriticOver(c.full), store.FormatTime(c.e.now()))
	return fmt.Sprintf(" The notes are still over %s: the next automatic curation for size is in 7 days.", strings.Join(over, ", "))
}

// runOlder reports whether a critique's run directory (named by
// criticRunAt) started before cutoff.
func runOlder(dir string, cutoff time.Time) bool {
	name := filepath.Base(dir)
	if len(name) < len(curateRunFormat) {
		return false
	}
	at, err := time.ParseInLocation(curateRunFormat, name[:len(curateRunFormat)], time.Local)
	return err == nil && at.Before(cutoff)
}

// pruneCritiques removes the critiques' run directories older than
// curateKeep; the registry keeps every critique.
func (e *Engine) pruneCritiques() {
	root := NotesRoot(e.d.Layout)
	if root == "" {
		return
	}
	cutoff := e.now().Add(-curateKeep)
	dirs, _ := filepath.Glob(filepath.Join(root, notes.CriticDirName, "*", "*", "*"))
	for _, d := range dirs {
		if !runOlder(d, cutoff) {
			continue
		}
		if err := os.RemoveAll(d); err != nil {
			e.log.Warn("notes critic: prune a run directory", "dir", d, "err", err)
		}
	}
}
