package engine

// Curation of the repository notes (DECISIONS "Repository notes: triggers,
// history, usage and curation", "The retro's misses become notes
// proposals"). A curation runs for one repository at a time, on the [notes]
// curate triggers: a repository marked past its limits (over_limit), once a
// week (weekly), or one whose retro recorded misses for its notes (misses,
// KVNotesMisses); or when `magnum notes <repo> --curate` asks
// (ReqNotesCurate). It copies the notes and the harness under the notes lock
// into a scratch directory, with the repository's new misses (class miss,
// scope repo) as misses.json, where an interactive agent (the retro's pane
// machinery, tagged "learn") proposes new notes, a new harness and
// changes.json accounting for every section, file and miss; magnum checks
// the proposal (notes.Validate) with one nudge, and stores it in the registry
// whatever it is: a valid one waits for the operator (`magnum notes <repo>
// --review`, a toast and a count in the screens' titles) with what it did
// with each miss, an invalid one is kept with its problems. The live notes
// change only when the operator applies a proposal. A proposal nobody
// reviewed within a week expires.

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// ReqNotesCurate starts a curation of a repository's notes now
	// (NotesCuratePayload).
	ReqNotesCurate = "notes_curate"

	// Curation triggers (notes_proposals.trigger_reason).
	CurateTriggerOverLimit = config.CurateOverLimit
	CurateTriggerWeekly    = config.CurateWeekly
	CurateTriggerMisses    = config.CurateMisses
	CurateTriggerRequest   = "request"

	// ProposalTTL is how long a proposal waits for the operator before it
	// expires.
	ProposalTTL = 7 * 24 * time.Hour
	// NotesLockWait is how long a curation's copy and an apply wait for the
	// notes lock, as long as a judge waits for it.
	NotesLockWait = 3 * time.Minute

	// curateCheckEvery spaces the scans for a curation due.
	curateCheckEvery = 10 * time.Minute
	// curateOverLimitEvery and curateWeeklyEvery space the curations of one
	// repository; curateRetry the attempts that stored nothing (an agent
	// that could not start, a limit it hit).
	curateOverLimitEvery = 24 * time.Hour
	curateWeeklyEvery    = 7 * 24 * time.Hour
	curateRetry          = time.Hour
	// curateKeep is how long a curation's scratch directory is kept (the
	// registry keeps the proposal itself).
	curateKeep = 30 * 24 * time.Hour
	// curateRunFormat names a curation's directory (local time).
	curateRunFormat = "20060102-150405"
	// curateReasonRunes clips the problems stored with an invalid proposal.
	curateReasonRunes = 2000
	// curateRejections is how many earlier rejections the curator is shown.
	curateRejections = 5
)

// KVNotesMisses marks a repository whose retro recorded misses for its
// notes: the time of the retro that did (store.FormatTime). The misses
// trigger curates it, and a curation that began after that time clears it.
func KVNotesMisses(fullName string) string { return "notes." + strings.ToLower(fullName) + ".misses" }

// NotesCuratePayload is a `magnum notes <repo> --curate` request, or the
// new curation `magnum notes <repo> --review` asks for instead of a stale
// proposal: Supersede is that proposal's id.
type NotesCuratePayload struct {
	Repo      string `json:"repo"` // owner/name
	Supersede int64  `json:"supersede,omitempty"`
}

// CurateRun is one curation: its id and scratch directory.
type CurateRun struct {
	ID  string
	Dir string
}

// CurateJob is what a Curator works on: the scratch directory, the rendered
// prompt (prompts/notes-curate.md), and Check, which reads what the curator
// wrote and validates it (no problems: a valid proposal).
type CurateJob struct {
	Repo    string
	Scratch notes.Scratch
	Prompt  string
	Check   func() (notes.Proposal, []string)
}

// CurateResult is how a curator's turns ended: a proposal, valid when
// Problems is empty, or a Pause (a limit the agent hit).
type CurateResult struct {
	Proposal notes.Proposal
	Problems []string
	Pause    *pipeline.Pause
}

// Curator proposes curated notes for one curation; Close ends it.
type Curator interface {
	Curate(ctx context.Context, job CurateJob) (CurateResult, error)
	Close(ctx context.Context) error
}

// ErrCuratorDown wraps a Curator's error that is not the proposal's: the
// agent could not be set up or started, or went away.
var ErrCuratorDown = errors.New("engine: the notes curator is not available")

// curatePane is the notes curator's pane agent.
var curatePane = paneSpec{what: "notes curation", workspace: "learn notes", owner: learnRepoOwner, name: "notes",
	role: (*config.Config).NotesRole}

// paneCurators is Deps.Curator for the pane agent: one agent per curation.
func paneCurators(d paneDeps) func(context.Context, CurateRun) (Curator, error) {
	d = d.withDefaults()
	return func(_ context.Context, run CurateRun) (Curator, error) {
		return &paneCurator{paneAgent: &paneAgent{d: d, spec: curatePane, dir: run.Dir, id: run.ID}}, nil
	}
}

// paneCurator is one curation's agent.
type paneCurator struct{ *paneAgent }

// Curate prompts the agent with job's prompt and waits for its turn to end;
// a proposal that is missing or invalid gets one nudge listing its problems.
// A usage limit, a logout, a per-model limit or an overload is a Pause; an
// agent that cannot be set up or started, or went away, is ErrCuratorDown.
func (c *paneCurator) Curate(ctx context.Context, job CurateJob) (CurateResult, error) {
	if err := c.setup(ctx); err != nil {
		return CurateResult{}, fmt.Errorf("%w: set up: %w", ErrCuratorDown, err)
	}
	if pause, err := c.ensureAgent(ctx); err != nil {
		if pause != nil || ctx.Err() != nil {
			return CurateResult{Pause: pause}, err
		}
		return CurateResult{}, fmt.Errorf("%w: start: %w", ErrCuratorDown, err)
	}
	text := job.Prompt
	for attempt := 0; ; attempt++ {
		t := c.turn(ctx, attempt, text)
		p, problems := job.Check()
		if len(problems) == 0 {
			return CurateResult{Proposal: p}, nil
		}
		if pause := c.health(ctx, t, job.Scratch.Changes()); pause != nil {
			return CurateResult{Pause: pause}, fmt.Errorf("the notes curator stopped: %s", pause.Kind)
		}
		switch {
		case ctx.Err() != nil:
			return CurateResult{}, ctx.Err()
		case t.down:
			return CurateResult{}, fmt.Errorf("%w: %w", ErrCuratorDown, t.err)
		case attempt > 0:
			return CurateResult{Proposal: p, Problems: problems}, nil
		}
		if t.err != nil {
			c.d.Logger.Warn("notes curation: the agent's turn did not end well; nudging", "run", c.id, "err", t.err)
		}
		text = curateNudge(job, problems)
	}
}

// curateNudge is the one reminder after a turn without a valid proposal:
// the scratch directory's paths and magnum's own list of problems.
func curateNudge(job CurateJob, problems []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The proposal in `%s` is not valid yet:\n", job.Scratch.Dir)
	for _, p := range problems {
		fmt.Fprintf(&b, "- %s\n", p)
	}
	fmt.Fprintf(&b, "Fix it as the first prompt describes: `%s`, `%s/` and `%s`. Write each file to <file>.tmp first, "+
		"then move it into place, then stop.", job.Scratch.Proposal(), job.Scratch.Harness(), job.Scratch.Changes())
	return b.String()
}

// curateData feeds the curator's prompt (prompts/notes-curate.md): the
// repository's name, paths and limits, never notes text; Misses is the
// path of misses.json ("" when the curation was given none) and MissCount
// how many it holds.
type curateData struct {
	Repo           string
	Dir            string
	Current        string
	CurrentHarness string
	Usage          string
	Proposal       string
	Harness        string
	Changes        string
	Limits         notes.Limits
	Over           []string
	UnusedRounds   int
	Misses         string
	MissCount      int
	// Superseded is the directory holding the stale proposal this curation
	// follows up on (notes.WriteSuperseded), "" when there is none, and
	// SupersededID its id.
	Superseded   string
	SupersededID int64
}

// curateMissesFile is the misses.json a curator reads: the repository's
// new misses for its notes, as data. Never a reviewer's or an author's
// login, a pull request's number, link or text: the title and the lesson
// pass learn.ScrubLesson again, the place is path:line at the reviewed
// commit, and the operator's reasons for rejecting earlier proposals that
// had a miss come with it.
type curateMissesFile struct {
	Repo   string       `json:"repo"`
	Misses []curateMiss `json:"misses"`
}

type curateMiss struct {
	ID         int64    `json:"id"`
	Severity   string   `json:"severity,omitempty"`
	Where      string   `json:"where,omitempty"` // path:line at the reviewed commit; none for a review summary
	Title      string   `json:"title,omitempty"`
	Lesson     string   `json:"lesson,omitempty"`
	Rejections []string `json:"rejections,omitempty"`
}

// curateUsage is the usage.json a curator reads.
type curateUsage struct {
	Repo         string            `json:"repo"`
	Limits       notes.Limits      `json:"limits"`
	Size         notes.Size        `json:"size"`
	Over         []string          `json:"over"`
	UnusedRounds int               `json:"unused_rounds"`
	Harness      []curateFile      `json:"harness"`
	Rejections   []curateRejection `json:"rejections"`
}

type curateFile struct {
	Name            string     `json:"name"`
	Bytes           int64      `json:"bytes"`
	Rounds          int        `json:"rounds"`
	Uses            int        `json:"uses"`
	LastUsed        *time.Time `json:"last_used,omitempty"`
	UnusedCandidate bool       `json:"unused_candidate"`
}

type curateRejection struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// maybeCurate starts a queued curation that can start now (every tick:
// queued ones wait for a judge stage to end), expires the proposals nobody
// reviewed and, unless [notes] curate is off, starts the curation due first
// (dueCurations), at most every curateCheckEvery: one at a time, none under
// a pause of any kind or while the curator's CLI is paused. A repository due
// while a round of it is in its judge stage is queued instead of skipped. A
// dry run and `magnum daemon --once` never curate.
func (e *Engine) maybeCurate(ctx context.Context) {
	if e.d.DryRun || e.once {
		return
	}
	if e.d.Curator != nil && !e.curateBusy() && e.holdReason(ctx) == "" && e.curateToolPause(ctx) == "" {
		e.startQueuedCuration(ctx)
	}
	now := e.now()
	if !e.curateChecked.IsZero() && now.Sub(e.curateChecked) < curateCheckEvery {
		return
	}
	e.curateChecked = now
	e.expireProposals(ctx)
	if e.d.Curator == nil || len(e.cfg.Notes.Curate) == 0 || e.curateBusy() || e.userPause(ctx) != "" || e.holdReason(ctx) != "" || e.curateToolPause(ctx) != "" {
		return
	}
	for _, due := range e.dueCurations(ctx, now) {
		full := due.repo.FullName()
		if due.stale != nil && !e.supersede(ctx, *due.stale, full, "the notes changed since it was made and its changes no longer merge with theirs; "+
			"a day without a review, so a new curation starts from the notes now") {
			continue
		}
		if e.notesJudging(ctx, due.repo.ID) {
			e.queueCurate(ctx, full, due.trigger, QueuedJudge)
			continue
		}
		e.startCurate(ctx, due.repo, due.trigger)
		return
	}
}

// curateDue is a repository due a curation, by trigger; stale is the
// waiting proposal it supersedes (CurateTriggerStale).
type curateDue struct {
	repo    store.Repo
	trigger string
	stale   *store.NotesProposal
}

// dueCurations lists the repositories due a curation by an [notes] curate
// trigger, in the registry's order, skipping the queued ones and those
// whose last attempt failed within curateRetry. One with a proposal waiting
// for the operator is due only when that proposal is a stale curation a day
// old whose changes no longer merge with the notes' (stale: the new
// curation supersedes it). Otherwise either its notes changed since its last
// curation began (or applied) and it is marked past a limit (over_limit)
// with its last curation a day old, or that is a week old (weekly); or a
// retro recorded misses for its notes (misses, its KVNotesMisses mark) that
// are still new, its last curation a day old.
func (e *Engine) dueCurations(ctx context.Context, now time.Time) []curateDue {
	repos, err := e.st.ListRepos(ctx)
	if err != nil {
		e.log.Warn("notes curation: list repositories", "err", err)
		return nil
	}
	queued := ReadCurateQueue(ctx, e.st)
	on := e.cfg.Notes.Curate.Has
	var out []curateDue
	for _, repo := range repos {
		if _, ok := notesRepo(e.d.Layout, repo.Owner, repo.Name); !ok || e.curateTriedRecently(repo.ID, now) ||
			slices.ContainsFunc(queued, func(q CurateQueued) bool { return strings.EqualFold(q.Repo, repo.FullName()) }) {
			continue
		}
		props, err := e.st.NotesProposals(ctx, store.NotesProposalFilter{RepoID: repo.ID, Limit: 50})
		if err != nil {
			continue
		}
		if i := slices.IndexFunc(props, func(p store.NotesProposal) bool { return p.State == store.ProposalPending }); i >= 0 {
			if p := props[i]; p.Kind == store.ProposalCuration && now.Sub(p.CreatedAt) >= staleRecurateAfter && e.staleConflicts(ctx, repo, p) {
				out = append(out, curateDue{repo: repo, trigger: CurateTriggerStale, stale: &p})
			}
			continue
		}
		var last *store.NotesProposal
		for i := range props {
			if props[i].Kind == store.ProposalCuration {
				last = &props[i]
				break
			}
		}
		since := time.Duration(1<<63 - 1)
		if last != nil {
			since = now.Sub(last.CreatedAt)
		}
		// The notes' own triggers want notes that changed since the last
		// curation.
		changed := false
		if latest, err := e.st.LatestNotesVersion(ctx, repo.ID); err == nil && (latest.Bytes > 0 || len(latest.Files) > 0) {
			changed = last == nil || latest.ID > max(store.Deref(last.BaseVersionID), store.Deref(last.AppliedVersionID))
		}
		trigger := ""
		marked, _ := e.getKV(ctx, KVNotesOver(repo.FullName()))
		switch {
		case changed && on(CurateTriggerOverLimit) && marked != "" && since >= curateOverLimitEvery:
			trigger = CurateTriggerOverLimit
		case changed && on(CurateTriggerWeekly) && since >= curateWeeklyEvery:
			trigger = CurateTriggerWeekly
		case on(CurateTriggerMisses) && since >= curateOverLimitEvery && e.missesDue(ctx, repo):
			trigger = CurateTriggerMisses
		default:
			continue
		}
		out = append(out, curateDue{repo: repo, trigger: trigger})
	}
	return out
}

// missesDue reports whether repo is marked by a retro (KVNotesMisses) and
// still has new misses for its notes; a mark without them is cleared.
func (e *Engine) missesDue(ctx context.Context, repo store.Repo) bool {
	key := KVNotesMisses(repo.FullName())
	if v, _ := e.getKV(ctx, key); v == "" {
		return false
	}
	n, err := e.st.CountMisses(ctx, notesMissFilter(repo.ID))
	if err != nil {
		return false
	}
	if n == 0 {
		e.delKV(ctx, key)
	}
	return n > 0
}

// notesMissFilter selects a repository's misses for its notes: class miss,
// scope repo, state new.
func notesMissFilter(repoID int64) store.MissFilter {
	return store.MissFilter{RepoID: repoID, Classes: []string{store.MissMiss}, Scopes: []string{store.MissScopeRepo}, States: []string{store.MissNew}}
}

func (e *Engine) curateTriedRecently(repoID int64, now time.Time) bool {
	e.curateMu.Lock()
	defer e.curateMu.Unlock()
	t, ok := e.curateTried[repoID]
	return ok && now.Sub(t) < curateRetry
}

// requestCurate serves `magnum notes <repo> --curate`: a curation now,
// whatever [notes] curate says, unless a drain, an infrastructure pause or a
// pause of the curator's CLI holds it or a proposal of the repository waits
// for review. With Supersede naming that proposal (a stale one the operator
// gave up on) it is superseded first and the new curation reads it. While a
// round of the repository is in its judge stage, or another curation runs,
// the curation is queued and starts once that ended (startQueuedCuration).
func (e *Engine) requestCurate(ctx context.Context, p NotesCuratePayload) (string, error) {
	repo, err := e.st.RepoByFullName(ctx, p.Repo)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("%s is not in the registry: magnum curates the notes of the repositories it reviews", p.Repo)
	}
	if err != nil {
		return "", err
	}
	full := repo.FullName()
	if _, ok := notesRepo(e.d.Layout, repo.Owner, repo.Name); !ok {
		return "", fmt.Errorf("%s has no notes path", full)
	}
	if reason := cmp.Or(e.holdReason(ctx), e.curateToolPause(ctx)); reason != "" {
		return "", fmt.Errorf("no curation now: %s", reason)
	}
	pending, err := e.st.NotesProposals(ctx, store.NotesProposalFilter{RepoID: repo.ID, States: []string{store.ProposalPending}, Limit: 1})
	if err != nil {
		return "", err
	}
	var stale *store.NotesProposal
	switch {
	case len(pending) > 0 && p.Supersede != 0 && pending[0].ID == p.Supersede && pending[0].Kind == store.ProposalCuration:
		stale = &pending[0]
	case len(pending) > 0:
		return "", fmt.Errorf("proposal %d for the notes of %s waits for review: `magnum notes %s --review` applies or rejects it first", pending[0].ID, full, full)
	case p.Supersede != 0:
		return "", fmt.Errorf("proposal %d for the notes of %s no longer waits for review", p.Supersede, full)
	}
	if e.d.DryRun {
		e.rec.Record(ctx, notesSubjectOf(full), "notes_curate", "a curation of the notes of "+full)
		return "dry run: would curate the notes of " + full, nil
	}
	if e.d.Curator == nil {
		return "", errors.New("no curator agent: the daemon has no herdr client to run one in")
	}
	prefix := ""
	if stale != nil {
		if !e.supersede(ctx, *stale, full, "the notes changed since it was made; the operator asked for a new curation from the notes now") {
			return "", fmt.Errorf("proposal %d for the notes of %s could not be superseded", stale.ID, full)
		}
		prefix = fmt.Sprintf("proposal %d superseded; ", stale.ID)
	}
	if e.notesJudging(ctx, repo.ID) {
		e.queueCurate(ctx, full, CurateTriggerRequest, QueuedJudge)
		return prefix + "queued: " + queuedText(QueuedJudge) + "; a toast says when its proposal is ready for `magnum notes " + full + " --review`", nil
	}
	if started, since, other := e.startCurate(ctx, repo, CurateTriggerRequest); !started {
		if other == "" { // the daemon began stopping or draining since holdReason
			return "", fmt.Errorf("no curation now: %s", cmp.Or(e.stopping(ctx), "the daemon is stopping"))
		}
		e.queueCurate(ctx, full, CurateTriggerRequest, QueuedBusy)
		return fmt.Sprintf("%squeued: starts when the running curation (of %s, started %s) ends; `magnum logs` follows it", prefix, other,
			since.Local().Format("15:04")), nil
	}
	return prefix + "notes curation of " + full + " started; a toast says when its proposal is ready for `magnum notes " + full + " --review`", nil
}

func notesSubjectOf(full string) string { return "notes:" + strings.ToLower(full) }

// curateToolPause is why the curator cannot work now ("" = it can): its
// CLI is paused. Without a curator nothing needs the CLI.
func (e *Engine) curateToolPause(ctx context.Context) string {
	if e.d.Curator == nil {
		return ""
	}
	return e.kindPauseReason(ctx, []string{e.cfg.NotesRole().AgentKind()})
}

// curateBusy reports whether a curation is running.
func (e *Engine) curateBusy() bool {
	e.curateMu.Lock()
	defer e.curateMu.Unlock()
	return e.curateCancel != nil
}

// startCurate runs a curation of repo in its own goroutine on a child of
// ctx, unless the daemon is stopping or draining (stopping: it reports false,
// a zero time and no repository) or one is running: then it reports false,
// when that one started and its repository. The curation running is
// KVNotesCurating while it runs, and the repository leaves the queue.
func (e *Engine) startCurate(ctx context.Context, repo store.Repo, trigger string) (bool, time.Time, string) {
	if e.stopping(ctx) != "" {
		return false, time.Time{}, ""
	}
	e.curateMu.Lock()
	defer e.curateMu.Unlock()
	if e.curateCancel != nil {
		return false, e.curateStarted, e.curateRepo
	}
	cctx, cancel := context.WithCancel(ctx)
	e.curateCancel, e.curateStarted, e.curateRepo = cancel, e.now(), repo.FullName()
	e.unqueueCurate(ctx, repo.FullName())
	if b, err := json.Marshal(CurateMark{Repo: repo.FullName(), Trigger: trigger, Started: e.curateStarted}); err == nil {
		e.setKV(ctx, KVNotesCurating, string(b))
	}
	e.curateWG.Add(1)
	go func() {
		defer e.curateWG.Done()
		defer func() {
			e.delKV(context.WithoutCancel(ctx), KVNotesCurating)
			e.curateMu.Lock()
			e.curateCancel = nil
			e.curateMu.Unlock()
			cancel()
		}()
		e.runCurate(cctx, repo, trigger)
	}()
	return true, e.curateStarted, ""
}

// stopCurate cancels a running curation (shutdown).
func (e *Engine) stopCurate() {
	e.curateMu.Lock()
	defer e.curateMu.Unlock()
	if e.curateCancel != nil {
		e.curateCancel()
	}
}

// runCurate is one curation of repo: the copy under the notes lock (the
// state found becomes the base version), the usage data, the curator's
// turns and the proposal stored in the registry, valid or not. A stop that
// is not the proposal's (no curator, a limit, a shutdown) stores nothing
// and is tried again after curateRetry.
func (e *Engine) runCurate(ctx context.Context, repo store.Repo, trigger string) {
	nr, ok := notesRepo(e.d.Layout, repo.Owner, repo.Name)
	if !ok {
		return
	}
	full, subject := nr.FullName(), notesSubject(nr)
	start := e.now()
	run := CurateRun{ID: start.Local().Format(curateRunFormat)}
	run.Dir = filepath.Join(nr.Curate(), run.ID)
	e.event(ctx, "info", subject, "notes.curate_start", fmt.Sprintf("notes curation of %s started (%s)", full, trigger),
		map[string]any{"run": run.ID, "trigger": trigger})
	stop := func(why string) {
		e.curateMu.Lock()
		e.curateTried[repo.ID] = start
		e.curateMu.Unlock()
		e.event(context.WithoutCancel(ctx), "warn", subject, "notes.curate_stopped", "notes curation of "+full+" stopped: "+why,
			map[string]any{"run": run.ID})
	}
	if e.d.Curator == nil {
		stop("no curator agent (herdr is not configured)")
		return
	}

	unlock, err := notes.Lock(ctx, nr.Lock(), NotesLockWait)
	if err != nil {
		stop("the notes lock: " + oneLine(err.Error(), retroWhyRunes))
		return
	}
	state, err := notes.ReadState(nr)
	var base store.NotesVersion
	if err == nil {
		base, err = e.recordNotes(ctx, repo, nr, state, store.NotesFromImport, 0, "")
	}
	unlock()
	if err != nil {
		stop("read the notes: " + oneLine(err.Error(), retroWhyRunes))
		return
	}
	usage, over, err := e.curateUsage(ctx, repo, nr, state)
	if err != nil {
		stop("usage data: " + oneLine(err.Error(), retroWhyRunes))
		return
	}
	misses, missesJSON, err := e.curateMisses(ctx, repo)
	if err != nil {
		stop("the misses: " + oneLine(err.Error(), retroWhyRunes))
		return
	}
	scratch, err := notes.PrepareScratch(run.Dir, state, usage)
	if err == nil && len(misses) > 0 {
		err = fsx.WriteFileAtomic(scratch.Misses(), missesJSON, 0o600)
	}
	if err != nil {
		stop("scratch directory: " + oneLine(err.Error(), retroWhyRunes))
		return
	}
	superseded, err := e.curateSuperseded(ctx, repo, scratch)
	if err != nil {
		stop("the superseded proposal: " + oneLine(err.Error(), retroWhyRunes))
		return
	}
	prompt, promptSHA, err := e.curatePrompt(scratch, nr, over, len(misses), superseded)
	if err != nil {
		stop(oneLine(err.Error(), retroWhyRunes))
		return
	}
	check, err := e.curateCheck(ctx, repo, state)
	if err != nil {
		stop(oneLine(err.Error(), retroWhyRunes))
		return
	}
	for _, m := range misses {
		check.Misses = append(check.Misses, m.ID)
	}

	cur, err := e.d.Curator(ctx, run)
	if err != nil {
		stop("the curator cannot start: " + oneLine(err.Error(), retroWhyRunes))
		return
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), retroCloseTimeout)
		defer cancel()
		if err := cur.Close(cctx); err != nil {
			e.event(cctx, "warn", subject, "notes.curate_close", "notes curation: closing the curator: "+oneLine(err.Error(), retroWhyRunes), nil)
		}
	}()
	res, err := cur.Curate(ctx, CurateJob{Repo: full, Scratch: scratch, Prompt: prompt, Check: func() (notes.Proposal, []string) {
		p, problems := notes.ReadProposal(scratch)
		if len(problems) > 0 {
			return p, problems
		}
		return p, notes.Validate(p, check)
	}})
	switch {
	case res.Pause != nil:
		if res.Pause.Kind == string(agents.HealthUsageLimit) || res.Pause.Kind == string(agents.HealthLoginRequired) {
			e.pauseTool(context.WithoutCancel(ctx), *res.Pause)
		}
		stop(fmt.Sprintf("%s: %s", res.Pause.Tool, res.Pause.Kind))
		return
	case ctx.Err() != nil:
		stop("the daemon is stopping")
		return
	case err != nil:
		stop(oneLine(err.Error(), retroWhyRunes))
		return
	}

	in := store.NotesProposalInput{RepoID: repo.ID, Kind: store.ProposalCuration, Trigger: trigger, BaseVersionID: base.ID,
		Model: e.cfg.RoleModel(e.cfg.NotesRole()), PromptSHA: promptSHA, Scratch: run.Dir}
	octx := context.WithoutCancel(ctx)
	if len(res.Problems) > 0 {
		in.State, in.Reason = store.ProposalInvalid, textx.Clip(strings.Join(res.Problems, "; "), curateReasonRunes)
		p, err := e.st.CreateNotesProposal(octx, in)
		if err != nil {
			e.log.Warn("notes curation: store an invalid proposal", "repo", full, "err", err)
		} else {
			e.clearMissesMark(octx, full, start)
		}
		e.event(octx, "warn", subject, "notes.curate_invalid",
			fmt.Sprintf("notes curation of %s: the proposal is still invalid after a nudge (%d problem(s)); kept as proposal %d",
				full, len(res.Problems), p.ID), map[string]any{"run": run.ID, "proposal": p.ID, "problems": len(res.Problems)})
		return
	}
	proposed := ContentOf(res.Proposal.State)
	in.State, in.Proposed, in.Changes = store.ProposalPending, &proposed, res.Proposal.ChangesJSON
	for _, m := range res.Proposal.Changes.Misses {
		in.Misses = append(in.Misses, store.ProposalMiss{MissID: m.ID, Outcome: m.Action, Section: strings.TrimSpace(m.Section),
			Reason: strings.TrimSpace(m.Reason)})
	}
	p, err := e.st.CreateNotesProposal(octx, in)
	if err != nil {
		stop("store the proposal: " + oneLine(err.Error(), retroWhyRunes))
		return
	}
	e.clearMissesMark(octx, full, start)
	before, after := state.Size(e.cfg.Notes.MaxLine), res.Proposal.State.Size(e.cfg.Notes.MaxLine)
	noted := 0
	for _, m := range in.Misses {
		if m.Outcome == store.MissNoted {
			noted++
		}
	}
	missText := ""
	if len(in.Misses) > 0 {
		missText = fmt.Sprintf(", %d of %s noted", noted, textx.Count(len(in.Misses), "miss", "misses"))
	}
	e.event(octx, "info", subject, "notes.curate_ready",
		fmt.Sprintf("notes curation of %s is ready as proposal %d: %d → %d bytes, %d → %d harness files%s; `magnum notes %s --review`",
			full, p.ID, before.Bytes, after.Bytes, before.HarnessFiles, after.HarnessFiles, missText, full),
		map[string]any{"run": run.ID, "proposal": p.ID, "bytes_before": before.Bytes, "bytes_after": after.Bytes,
			"harness_before": before.HarnessFiles, "harness_after": after.HarnessFiles, "misses": len(in.Misses), "misses_noted": noted})
	e.info(notify.Item{Key: fmt.Sprintf("notes-proposal:%d", p.ID), Title: "notes curation for " + full + " is ready",
		Body: "Review it with `magnum notes " + full + " --review`.", Line: "notes curation for " + full + " is ready"})
}

// clearMissesMark clears full's misses mark (KVNotesMisses) once a
// curation that began at start stored its proposal, valid or not: the
// repository got its curation. A mark a retro set after start stays.
func (e *Engine) clearMissesMark(ctx context.Context, full string, start time.Time) {
	key := KVNotesMisses(full)
	if at, ok := e.kvTime(ctx, key); ok && at.After(start) {
		return
	}
	e.delKV(ctx, key)
}

// curateMisses reads the repository's misses for its notes (class miss,
// scope repo, state new), oldest first, and writes them as misses.json
// (curateMissesFile): the title and the lesson scrubbed again of the logins
// of the miss's PR (its author and the reviewer) and of magnum, a pull
// request reference or a link (dropped when they have one), and the reasons
// of the rejected proposals each was in.
func (e *Engine) curateMisses(ctx context.Context, repo store.Repo) ([]store.Miss, []byte, error) {
	ms, err := e.st.Misses(ctx, notesMissFilter(repo.ID))
	if err != nil || len(ms) == 0 {
		return nil, nil, err
	}
	slices.SortFunc(ms, func(a, b store.Miss) int { return cmp.Compare(a.ID, b.ID) })
	ids := make([]int64, 0, len(ms))
	for _, m := range ms {
		ids = append(ids, m.ID)
	}
	rejections, err := e.st.MissRejections(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	var own []string
	for _, id := range e.cfg.Identities {
		own = append(own, id.Login)
	}
	authors := map[int64]string{}
	out := curateMissesFile{Repo: repo.FullName(), Misses: make([]curateMiss, 0, len(ms))}
	for _, m := range ms {
		author, ok := authors[m.PRID]
		if !ok {
			if pr, err := e.st.PRByID(ctx, m.PRID); err == nil {
				author = deref(pr.AuthorLogin)
			}
			authors[m.PRID] = author
		}
		scrub := func(s string) string {
			s, _ = learn.ScrubLesson(s, store.MissScopeRepo, []string{author, m.Reviewer}, own, []string{repo.Owner, repo.Name})
			return s
		}
		cm := curateMiss{ID: m.ID, Severity: m.Severity, Title: scrub(m.Title), Lesson: scrub(m.Lesson), Rejections: rejections[m.ID]}
		if m.Path != "" {
			cm.Where = m.Path
			if m.Line > 0 {
				cm.Where += ":" + strconv.Itoa(m.Line)
			}
		}
		out.Misses = append(out.Misses, cm)
	}
	b, err := json.MarshalIndent(out, "", "  ")
	return ms, b, err
}

// curateUsage writes the curator's usage data: the limits and the sizes,
// every harness file's rounds and uses, and the reasons of the latest
// rejected curations. It returns the JSON and the limits passed.
func (e *Engine) curateUsage(ctx context.Context, repo store.Repo, nr notes.Repo, state notes.State) ([]byte, []string, error) {
	l := e.notesLimits()
	size := state.Size(l.MaxLine)
	u := curateUsage{Repo: nr.FullName(), Limits: l, Size: size, Over: names(size.Over(l)), UnusedRounds: store.NotesUnusedRounds,
		Harness: []curateFile{}, Rejections: []curateRejection{}}
	uses, err := e.st.NotesFileUses(ctx, repo.ID)
	if err != nil {
		return nil, nil, err
	}
	byName := map[string]store.NotesFileUse{}
	for _, x := range uses {
		byName[x.File] = x
	}
	for _, b := range state.Files {
		x := byName[b.Path]
		u.Harness = append(u.Harness, curateFile{Name: b.Path, Bytes: int64(len(b.Body)), Rounds: x.Rounds, Uses: x.Uses,
			LastUsed: x.LastUsed, UnusedCandidate: x.Unused()})
	}
	rejected, err := e.st.NotesProposals(ctx, store.NotesProposalFilter{RepoID: repo.ID, States: []string{store.ProposalRejected}, Limit: curateRejections})
	if err != nil {
		return nil, nil, err
	}
	for _, p := range rejected {
		if p.Kind == store.ProposalCuration && p.Reason != "" {
			u.Rejections = append(u.Rejections, curateRejection{At: store.Deref(p.DecidedAt), Reason: p.Reason})
		}
	}
	b, err := json.MarshalIndent(u, "", "  ")
	return b, u.Over, err
}

// curateSuperseded writes the stale proposal this curation follows up on
// into scratch's superseded/ directory and returns its id: the repository's
// latest curation, when it was superseded (a curation that stored nothing
// leaves it the latest, so the next one reads it). 0 when there is none.
func (e *Engine) curateSuperseded(ctx context.Context, repo store.Repo, scratch notes.Scratch) (int64, error) {
	props, err := e.st.NotesProposals(ctx, store.NotesProposalFilter{RepoID: repo.ID, Limit: 50})
	if err != nil {
		return 0, err
	}
	i := slices.IndexFunc(props, func(p store.NotesProposal) bool { return p.Kind == store.ProposalCuration })
	if i < 0 || props[i].State != store.ProposalSuperseded || props[i].VersionID == nil {
		return 0, nil
	}
	p := props[i]
	content, err := e.st.NotesVersionContent(ctx, *p.VersionID)
	if err != nil {
		return 0, err
	}
	return p.ID, notes.WriteSuperseded(scratch, StateOf(content), p.Changes)
}

// curatePrompt renders the curator's prompt for scratch (with misses.json
// when it was given misses, and the superseded proposal it follows up on),
// and the SHA-256 of its template (kept with the proposal).
func (e *Engine) curatePrompt(scratch notes.Scratch, nr notes.Repo, over []string, misses int, superseded int64) (string, string, error) {
	p, err := e.cfg.ResolvePrompt(e.cfg.Notes.Prompt)
	if err != nil {
		return "", "", fmt.Errorf("the curator's prompt cannot be read: %w", err)
	}
	text, err := agents.RenderPrompt(p, curateData{Repo: nr.FullName(), Dir: scratch.Dir, Current: scratch.Current(),
		CurrentHarness: scratch.CurrentHarness(), Usage: scratch.Usage(), Proposal: scratch.Proposal(), Harness: scratch.Harness(),
		Changes: scratch.Changes(), Limits: e.notesLimits(), Over: over, UnusedRounds: store.NotesUnusedRounds,
		Misses: missesPath(scratch, misses), MissCount: misses, Superseded: supersededPath(scratch, superseded), SupersededID: superseded})
	if err != nil {
		return "", "", fmt.Errorf("the curator's prompt does not render: %w", err)
	}
	sum := sha256.Sum256([]byte(p.Text))
	return text, hex.EncodeToString(sum[:]), nil
}

// supersededPath is the superseded proposal's directory when the curation
// follows one up.
func supersededPath(s notes.Scratch, id int64) string {
	if id == 0 {
		return ""
	}
	return s.Superseded()
}

// missesPath is misses.json's path when the curation was given misses.
func missesPath(s notes.Scratch, n int) string {
	if n == 0 {
		return ""
	}
	return s.Misses()
}

// curateCheck is what a proposal of repo is validated against: the state
// the curation started from, and the numbers and head branches of the
// repository's pull requests the registry knows.
func (e *Engine) curateCheck(ctx context.Context, repo store.Repo, base notes.State) (notes.Check, error) {
	prs, err := e.st.ListPRs(ctx, store.PRFilter{RepoID: repo.ID})
	if err != nil {
		return notes.Check{}, err
	}
	c := notes.Check{Base: base}
	for _, pr := range prs {
		c.PRNumbers = append(c.PRNumbers, pr.Number)
		if b := deref(pr.HeadRef); b != "" && b != repo.DefaultBranch && b != deref(pr.BaseRef) {
			c.Branches = append(c.Branches, b)
		}
	}
	return c, nil
}

// expireProposals expires the proposals that waited for the operator longer
// than ProposalTTL; the registry keeps them.
func (e *Engine) expireProposals(ctx context.Context) {
	pending, err := e.st.NotesProposals(ctx, store.NotesProposalFilter{States: []string{store.ProposalPending}})
	if err != nil {
		e.log.Warn("notes proposals", "err", err)
		return
	}
	now := e.now()
	for _, p := range pending {
		if now.Sub(p.CreatedAt) < ProposalTTL {
			continue
		}
		_, err := e.st.DecideNotesProposal(ctx, p.ID, []string{store.ProposalPending}, store.ProposalExpired,
			"not reviewed within "+ProposalTTL.String(), now, nil)
		if err != nil {
			if !errors.Is(err, store.ErrConflict) {
				e.log.Warn("expire a notes proposal", "proposal", p.ID, "err", err)
			}
			continue
		}
		subject := ""
		if repo, err := e.st.RepoByID(ctx, p.RepoID); err == nil {
			subject = notesSubjectOf(repo.FullName())
		}
		e.event(ctx, "info", subject, "notes.proposal_expired", fmt.Sprintf("notes proposal %d expired: nobody reviewed it within 7 days", p.ID),
			map[string]any{"proposal": p.ID})
	}
}

// pruneCurations removes the curations' scratch directories older than
// curateKeep (<notes>/.curate/<owner>/<repo>/<run>); the registry keeps
// every proposal.
func (e *Engine) pruneCurations() {
	root := NotesRoot(e.d.Layout)
	if root == "" {
		return
	}
	cutoff := e.now().Add(-curateKeep)
	dirs, _ := filepath.Glob(filepath.Join(root, notes.CurateDirName, "*", "*", "*"))
	for _, d := range dirs {
		at, err := time.ParseInLocation(curateRunFormat, filepath.Base(d), time.Local)
		if err != nil || !at.Before(cutoff) {
			continue
		}
		if err := os.RemoveAll(d); err != nil {
			e.log.Warn("notes curation: prune a scratch directory", "dir", d, "err", err)
		}
	}
}
