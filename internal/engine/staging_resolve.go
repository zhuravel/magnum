package engine

// The staging resolver: staging.Resolver for the review daemon's staging
// client (Deps.Resolver). A deploy whose merge of a PR's head into a
// staging branch conflicts hands the conflict here (at most 2 tries per
// effect); Start answers at once with a job id, and the deploy reads the
// job's Result at each of its steps until it is done (a merge commit in a
// local repository, which the deploy fetches into its mirror and verifies
// again) or failed (why). Each job is journaled in the registry (kv
// staging.resolve.<job>, resolveJob as JSON), the source of truth, so
// Result and Cancel answer across a restart; a job the journal does not
// hold answers failed, "unknown job".
//
// One worker runs the jobs, one at a time per daemon, the oldest first, on
// a goroutine of its own (stagingWG, ended by the stop): Start and each
// tick wake it. A job works in a review slot of its PR's repository, taken
// as the fix and whiteboard jobs take one (staging_resolve_work.go): the
// PR's own slot once no round, open, fix or whiteboard holds the PR, or a
// free slot of the pool for a PR that has none; it never evicts a running
// review's slot nor touches it: while one holds the PR, the job waits ("waits
// for a slot", bounded by resolveWaitFor) and the next tick looks again, and
// the jobs after it go on meanwhile. In it,
// detached at the target's tip, `git merge --no-commit --no-ff <head>`
// leaves the conflicts for the agent (the fix role's model, effort and
// launch, with a prompt of its own and no GitHub credentials), which
// resolves them, runs their specs and writes a result file; the daemon
// then makes the merge commit itself as the bot and checks it as the
// deploy's verify does. The slot goes back as it was, with no guard hold.
// A ref, refs/magnum/staging-resolved/<effect>, keeps the merge in the
// repository until the effect is no longer live, at most a day (the
// tick's tendStagingResolves).
//
// The bounds: a job runs for at most resolveRunFor once it took its slot
// (its agent is stopped, "ran over 45 minutes"), and waits for its turn
// and its slot for at most resolveWaitFor, so the deploy's own wait of 2
// hours never ends it first. A restart goes on with a job whose agent
// still runs; one whose agent is gone fails, saying so, and one that had
// not started its agent yet starts again (recoverStagingResolves). With no
// [[factory.repo.staging]] entries there is no staging client, so no
// resolver: nothing of it runs.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/staging"
	"github.com/zhuravel/magnum/internal/store"
)

const (
	// resolveRunFor bounds a job from the moment it took its slot: its
	// agent is stopped and the job fails past it.
	resolveRunFor = 45 * time.Minute
	// resolveWaitFor bounds a job's wait for its turn and its slot: with
	// resolveRunFor it stays under the deploy's own wait (2 hours).
	resolveWaitFor = time.Hour
	// resolveKeep is how long an ended job's row stays: the deploy reads
	// its Result while the effect is live, never 2 hours after its Start.
	resolveKeep = 48 * time.Hour
	// resolveRefKeep bounds how long the ref that keeps a done job's merge
	// stays, live effect or not.
	resolveRefKeep = 24 * time.Hour
	// resolveRefPrefix names the ref that keeps a done job's merge in its
	// repository: refs/magnum/staging-resolved/<effect>.
	resolveRefPrefix = "refs/magnum/staging-resolved/"
	// maxResolveTry is the deploy's own bound (staging's maxResolves).
	maxResolveTry = 2
)

// The states of a job in the journal: queued (waits for its turn or its
// slot) and running are live; done and failed end it.
const (
	resolveQueued  = "queued"
	resolveRunning = staging.ResolveRunning
	resolveDone    = staging.ResolveDone
	resolveFailed  = staging.ResolveFailed
)

// The phases of a running job: in its slot, before its agent (the merge
// is prepared), its agent at work, the daemon's commit and checks.
const (
	resolvePhaseSlot   = "slot"
	resolvePhaseAgent  = "agent"
	resolvePhaseCommit = "commit"
)

// KVStagingResolves matches every staging resolver job's row
// (kvStagingResolve) in one KVValues read: StagingResolverSays reads them
// for the card and `magnum staging`.
const KVStagingResolves = "staging.resolve.*"

// whyUnknownJob is the Why of a job the journal does not hold.
const whyUnknownJob = "unknown job"

// whyCancelled is the Why of a job the deploy cancelled.
const whyCancelled = "cancelled"

// kvStagingResolve is job id's row.
func kvStagingResolve(id string) string { return "staging.resolve." + id }

// resolveJob is a job's journal row.
type resolveJob struct {
	ID        string `json:"id"`
	Effect    string `json:"effect"`
	Repo      string `json:"repo"`
	Number    int    `json:"number"`
	Target    string `json:"target"`
	TargetTip string `json:"target_tip"`
	Head      string `json:"head"`
	Try       int    `json:"try"`
	State     string `json:"state"`
	Phase     string `json:"phase,omitempty"`
	Why       string `json:"why,omitempty"`
	// Where it runs: the PR's row, the slot (its id and name), the commit
	// the slot held before the job ("" for a slot the job claimed from the
	// pool: Claimed, handed back at the end) and the repository that holds
	// the merge (the slot's main clone, the Dir of a done job).
	PRID    int64  `json:"pr_id,omitempty"`
	SlotID  int64  `json:"slot_id,omitempty"`
	Slot    string `json:"slot,omitempty"`
	SlotWas string `json:"slot_was,omitempty"`
	Claimed bool   `json:"claimed,omitempty"`
	Dir     string `json:"dir,omitempty"`
	// The merge: merge-tree's tree of [TargetTip, Head] and its conflicted
	// paths, the agent's herdr name, pane and run (in the job's scratch
	// registry), and the merge commit once made; Kept while the ref holds
	// it in Dir.
	MergeTree string   `json:"merge_tree,omitempty"`
	Conflicts []string `json:"conflicts,omitempty"`
	// Untracked are the untracked files the slot held before the merge
	// (UntrackedRead: they were read), so the job's own are told apart.
	Untracked     []string `json:"untracked,omitempty"`
	UntrackedRead bool     `json:"untracked_read,omitempty"`
	Agent         string   `json:"agent,omitempty"`
	Pane          string   `json:"pane,omitempty"`
	Run           string   `json:"run,omitempty"`
	Merge         string   `json:"merge,omitempty"`
	Kept          bool     `json:"kept,omitempty"`
	// Cancel: the deploy cancelled it while it ran; the worker ends it.
	Cancel bool `json:"cancel,omitempty"`
	// Started is the Start, Began when it took its slot (resolveRunFor
	// counts from it), Ended its end.
	Started time.Time `json:"started"`
	Began   time.Time `json:"began,omitzero"`
	Updated time.Time `json:"updated"`
	Ended   time.Time `json:"ended,omitzero"`
}

// live reports whether the job still runs or waits.
func (j resolveJob) live() bool { return j.State == resolveQueued || j.State == resolveRunning }

// ref is the ref that keeps the job's merge in its repository.
func (j resolveJob) ref() string { return resolveRefPrefix + j.Effect }

// label is the job's PR, owner/name#N.
func (j resolveJob) label() string { return j.Repo + "#" + strconv.Itoa(j.Number) }

// resolveIDRe is a job id: the effect's id, the try and a random suffix.
var resolveIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,100}\.r[12]\.[0-9a-f]{8}$`)

// effectIDRe is an effect id as a job id and a ref carry it (staging's ids
// are lower-case letters, digits and dashes).
var effectIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,100}$`)

// stagingResolver is the review daemon's staging.Resolver: the engine it
// acts as, bound once it exists (FromApp makes the staging client first),
// the git of its jobs and its worker's state.
type stagingResolver struct {
	e   atomic.Pointer[Engine]
	git resolveGit

	mu      sync.Mutex // the journal's read-modify-write and the fields below
	working bool       // the worker runs
	again   bool       // woken while it ran: it looks once more before it stops
	running string     // the job it runs
	stop    context.CancelFunc
}

var _ staging.Resolver = (*stagingResolver)(nil)

// bind makes e the engine the resolver acts as.
func (r *stagingResolver) bind(e *Engine) { r.e.Store(e) }

// engine is the bound engine, or errStagingUnbound.
func (r *stagingResolver) engine() (*Engine, error) {
	if e := r.e.Load(); e != nil {
		return e, nil
	}
	return nil, errStagingUnbound
}

// Start journals a job for req, queued, wakes the worker and returns its
// id at once. A live job of the same effect and try (a deploy that
// stopped between the Start and its journal) is answered with its id.
func (r *stagingResolver) Start(ctx context.Context, req staging.ResolveRequest) (string, error) {
	e, err := r.engine()
	if err != nil {
		return "", err
	}
	number, err := e.checkResolveRequest(req)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	jobs, err := e.resolveJobs(ctx)
	if err != nil {
		return "", err
	}
	if i := slices.IndexFunc(jobs, func(j resolveJob) bool { return j.live() && j.Effect == req.Effect && j.Try == req.Try }); i >= 0 {
		return jobs[i].ID, nil
	}
	id, err := newResolveID(req.Effect, req.Try)
	if err != nil {
		return "", err
	}
	now := e.now()
	job := resolveJob{ID: id, Effect: req.Effect, Repo: req.Repo, Number: number, Target: req.Target, TargetTip: req.TargetTip,
		Head: req.Head, Try: req.Try, State: resolveQueued, Why: "waits for its turn", Started: now, Updated: now}
	if err := e.saveResolveJob(ctx, job); err != nil {
		return "", err
	}
	e.log.Info("staging: a resolver job starts", "job", id, "pr", job.label(), "target", job.Target, "try", job.Try)
	r.wakeLocked(e)
	return id, nil
}

// Result is job's state as the journal holds it: queued and running answer
// running (with why it waits, or what it does and where), done the merge
// and its repository, failed why; a job it does not hold, unknown job.
func (r *stagingResolver) Result(ctx context.Context, job string) (staging.ResolveResult, error) {
	e, err := r.engine()
	if err != nil {
		return staging.ResolveResult{}, err
	}
	j, ok, err := e.resolveJob(ctx, job)
	switch {
	case err != nil:
		return staging.ResolveResult{}, err
	case !ok:
		return staging.ResolveResult{State: staging.ResolveFailed, Why: whyUnknownJob}, nil
	}
	switch j.State {
	case resolveQueued, resolveRunning:
		return staging.ResolveResult{State: staging.ResolveRunning, Why: j.Why}, nil
	case resolveDone:
		return staging.ResolveResult{State: staging.ResolveDone, MergeSHA: j.Merge, Why: j.Why, Dir: j.Dir}, nil
	}
	return staging.ResolveResult{State: staging.ResolveFailed, Why: j.Why}, nil
}

// Cancel stops job: a queued one fails "cancelled" at once; a running one
// is marked, and the worker stops its agent, puts its slot back, frees
// what it held and fails it "cancelled" (after a restart, the recovery
// does). An ended or unknown job is left as it is.
func (r *stagingResolver) Cancel(ctx context.Context, job string) error {
	e, err := r.engine()
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok, err := e.resolveJob(ctx, job)
	if err != nil || !ok || !j.live() {
		return err
	}
	if j.State == resolveQueued && j.ID != r.running {
		return e.endResolveJob(ctx, &j, resolveFailed, whyCancelled)
	}
	j.Cancel, j.Updated = true, e.now()
	if err := e.saveResolveJob(ctx, j); err != nil {
		return err
	}
	if j.ID == r.running && r.stop != nil {
		r.stop()
	}
	return nil
}

// errResolveEnded is save's answer for a job whose row ended meanwhile: the
// worker's copy is stale, and it lets go of what it took.
var errResolveEnded = errors.New("the job ended meanwhile")

// save writes job's row as the worker has it, keeping a Cancel that came
// since the worker read it; a row that ended meanwhile is left as it is
// (errResolveEnded), never brought back to life.
func (r *stagingResolver) save(ctx context.Context, e *Engine, job *resolveJob) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok, err := e.resolveJob(ctx, job.ID); err == nil && ok {
		if !cur.live() {
			return errResolveEnded
		}
		job.Cancel = job.Cancel || cur.Cancel
	}
	return e.saveResolveJob(ctx, *job)
}

// end ends job in state with why (endResolveJob), under the journal's lock;
// a row that ended already keeps its end.
func (r *stagingResolver) end(ctx context.Context, e *Engine, job *resolveJob, state, why string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok, err := e.resolveJob(ctx, job.ID); err == nil && ok && !cur.live() {
		*job = cur
		return nil
	}
	return e.endResolveJob(ctx, job, state, why)
}

// wake starts the worker unless it runs; one that runs looks again before
// it stops.
func (r *stagingResolver) wake(e *Engine) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.wakeLocked(e)
}

func (r *stagingResolver) wakeLocked(e *Engine) {
	if r.working {
		r.again = true
		return
	}
	if e.stagingCtx == nil || e.stagingCtx.Err() != nil {
		return // the daemon stops: the next start goes on
	}
	r.working, r.again = true, false
	ctx := e.stagingCtx
	e.stagingWG.Go(func() {
		e.safely(ctx, "staging resolver", "", func() { r.work(ctx, e) }, nil)
		r.mu.Lock()
		r.working, r.running, r.stop = false, "", nil
		r.mu.Unlock()
	})
}

// work runs the live jobs, one at a time, the oldest first (a running one
// a restart goes on with before any queued), until none can go on now. A
// job that waits for its slot is passed over until the next wake, so it
// holds back no job after it.
func (r *stagingResolver) work(ctx context.Context, e *Engine) {
	waited := map[string]bool{} // the jobs that wait for their slot, passed over until the next wake
	for ctx.Err() == nil {
		job, ok := r.next(ctx, e, waited)
		if !ok {
			r.mu.Lock()
			again := r.again
			r.again = false
			r.mu.Unlock()
			if !again {
				return
			}
			clear(waited) // woken meanwhile: the waiting jobs look again
			continue
		}
		jctx, cancel := context.WithCancel(ctx)
		if job, ok = r.begin(ctx, e, job.ID, cancel); !ok {
			cancel()
			waited[job.ID] = true // ended since next read it, or unreadable now
			continue
		}
		waits := e.runResolveJob(jctx, ctx, r, &job)
		cancel()
		r.mu.Lock()
		r.running, r.stop = "", nil
		r.mu.Unlock()
		if waits {
			waited[job.ID] = true // the next tick looks again
		}
	}
}

// begin makes job id the one the worker runs (stop cancels it), reading its
// row again under the journal's lock: a Cancel since next read the rows may
// have ended it (false then, as when the row cannot be read). A Cancel from
// here on finds it running and marks it, and stop ends its work.
func (r *stagingResolver) begin(ctx context.Context, e *Engine, id string, stop context.CancelFunc) (resolveJob, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok, err := e.resolveJob(ctx, id)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "staging resolver: read a job", "job", id)
		return resolveJob{ID: id}, false
	}
	if !ok || !j.live() {
		return resolveJob{ID: id}, false
	}
	r.running, r.stop = j.ID, stop
	if j.Cancel {
		stop()
	}
	return j, true
}

// next is the job the worker runs now: the oldest running one, else the
// oldest queued, passing over those in skip; false when none is live.
func (r *stagingResolver) next(ctx context.Context, e *Engine, skip map[string]bool) (resolveJob, bool) {
	jobs, err := e.resolveJobs(ctx)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "staging resolver: read the jobs")
		return resolveJob{}, false
	}
	var best *resolveJob
	for i := range jobs {
		j := &jobs[i]
		switch {
		case !j.live() || skip[j.ID]:
		case best == nil, j.State == resolveRunning && best.State != resolveRunning:
			best = j
		case (j.State == resolveRunning) == (best.State == resolveRunning) && j.Started.Before(best.Started):
			best = j
		}
	}
	if best == nil {
		return resolveJob{}, false
	}
	return *best, true
}

// checkResolveRequest refuses a request the resolver cannot take: a
// repository that is no watched one with staging envs, a ref that is not
// its PR, a target that is none of its staging branches, a tip or head that
// is no full sha, an effect id that is none, a try out of 1..2. It returns
// the PR's number.
func (e *Engine) checkResolveRequest(req staging.ResolveRequest) (int, error) {
	repo, n, ok := strings.Cut(req.Ref, "#")
	number, err := strconv.Atoi(n)
	switch {
	case !slices.ContainsFunc(StagingRepos(e.cfg), func(r string) bool { return strings.EqualFold(r, req.Repo) }):
		return 0, fmt.Errorf("staging resolver: %q is no watched repository with staging envs", req.Repo)
	case !ok || err != nil || number <= 0 || !strings.EqualFold(repo, req.Repo):
		return 0, fmt.Errorf("staging resolver: %q is no PR of %s", req.Ref, req.Repo)
	case !slices.ContainsFunc(StagingEnvs(e.cfg, req.Repo), func(s staging.Env) bool { return s.Branch == req.Target }):
		return 0, fmt.Errorf("staging resolver: %q is no staging branch of %s", req.Target, req.Repo)
	case !fullSHA.MatchString(req.TargetTip) || !fullSHA.MatchString(req.Head):
		return 0, errors.New("staging resolver: the target's tip and the head must be full shas")
	case !effectIDRe.MatchString(req.Effect):
		return 0, fmt.Errorf("staging resolver: %q is no effect id", req.Effect)
	case req.Try < 1 || req.Try > maxResolveTry:
		return 0, fmt.Errorf("staging resolver: try %d is not 1 or 2", req.Try)
	}
	return number, nil
}

// newResolveID is a new job id of effect's try.
func newResolveID(effect string, try int) (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return effect + ".r" + strconv.Itoa(try) + "." + hex.EncodeToString(b), nil
}

// resolveJob reads job id's row; false when the journal holds none (or the
// id is none).
func (e *Engine) resolveJob(ctx context.Context, id string) (resolveJob, bool, error) {
	if !resolveIDRe.MatchString(id) {
		return resolveJob{}, false, nil
	}
	v, ok, err := e.st.GetKV(ctx, kvStagingResolve(id))
	if err != nil || !ok {
		return resolveJob{}, false, err
	}
	j, ok := parseResolveJob(v)
	if !ok || j.ID != id {
		return resolveJob{}, false, nil
	}
	return j, true, nil
}

// resolveJobs reads every job's row, the oldest Start first; a row that
// does not parse is passed over.
func (e *Engine) resolveJobs(ctx context.Context) ([]resolveJob, error) {
	values, err := e.st.KVValues(ctx, KVStagingResolves)
	if err != nil {
		return nil, err
	}
	var out []resolveJob
	for _, k := range slices.Sorted(maps.Keys(values)) {
		if j, ok := parseResolveJob(values[k]); ok && kvStagingResolve(j.ID) == k {
			out = append(out, j)
		}
	}
	slices.SortStableFunc(out, func(a, b resolveJob) int { return a.Started.Compare(b.Started) })
	return out, nil
}

// StagingResolverSays is what the newest live resolver job of effect does
// now, among values (a KVValues read of KVStagingResolves): "try N: " and
// its Why made safe for a terminal, which names the agent's pane while the
// agent works; "" when no job of effect is live.
func StagingResolverSays(values map[string]string, effect string) string {
	var newest *resolveJob
	for k, v := range values {
		j, ok := parseResolveJob(v)
		if !ok || kvStagingResolve(j.ID) != k || j.Effect != effect || !j.live() {
			continue
		}
		if newest == nil || j.Started.After(newest.Started) {
			newest = &j
		}
	}
	if newest == nil {
		return ""
	}
	return "try " + strconv.Itoa(newest.Try) + ": " + stagingClean(newest.Why)
}

// parseResolveJob reads a row; false when it cannot be read or names no
// job.
func parseResolveJob(v string) (resolveJob, bool) {
	var j resolveJob
	if json.Unmarshal([]byte(v), &j) != nil || !resolveIDRe.MatchString(j.ID) {
		return resolveJob{}, false
	}
	return j, true
}

// saveResolveJob writes job's row.
func (e *Engine) saveResolveJob(ctx context.Context, job resolveJob) error {
	b, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return e.st.SetKV(context.WithoutCancel(ctx), kvStagingResolve(job.ID), string(b))
}

// endResolveJob ends job in state (done, failed) with why, and records the
// end on the PR's subject.
func (e *Engine) endResolveJob(ctx context.Context, job *resolveJob, state, why string) error {
	now := e.now()
	job.State, job.Why, job.Phase, job.Updated, job.Ended = state, oneLine(why, 300), "", now, now
	if err := e.saveResolveJob(ctx, *job); err != nil {
		return err
	}
	level := "info"
	if state != resolveDone {
		level = "warn"
	}
	e.event(context.WithoutCancel(ctx), level, resolveSubject(*job), "staging.resolve",
		fmt.Sprintf("staging resolver %s (%s, try %d): %s: %s", job.ID, job.Target, job.Try, state, job.Why),
		map[string]any{"job": job.ID, "effect": job.Effect, "target": job.Target, "try": job.Try, "state": state, "merge": job.Merge,
			"slot": job.Slot})
	return nil
}

// resolveSubject is the job's PR's audit subject.
func resolveSubject(job resolveJob) string {
	owner, name, _ := strings.Cut(job.Repo, "/")
	return prSubject(store.Repo{Owner: owner, Name: name}, job.Number)
}

// update applies fn to job id's row under the journal's lock; false when
// the journal holds no such job.
func (r *stagingResolver) update(ctx context.Context, e *Engine, id string, fn func(*resolveJob)) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok, err := e.resolveJob(ctx, id)
	if err != nil || !ok {
		return false, err
	}
	fn(&j)
	j.Updated = e.now()
	return true, e.saveResolveJob(ctx, j)
}

// recoverStagingResolves takes up the jobs a stopped daemon left running,
// at the start, before any tick dispatches a round: each holds its PR again
// (reserveSlotWork). One whose agent was at work goes on, and the worker
// finds out whether the agent still runs (it fails, saying so, when it is
// gone); one cancelled meanwhile ends "cancelled"; any other (its slot
// taken, its agent not prompted yet, or its commit cut) puts its slot back
// and waits again, queued, for its turn. Nothing without a resolver.
func (e *Engine) recoverStagingResolves(ctx context.Context) {
	r := e.d.stagingResolver
	if r == nil {
		return
	}
	jobs, err := e.resolveJobs(ctx)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "staging resolver: read the jobs")
		return
	}
	for _, j := range jobs {
		if j.State != resolveRunning {
			continue
		}
		held := e.holdForResolve(j.PRID)
		if !held && j.PRID != 0 {
			e.log.Warn("staging resolver: a job's PR is held by other work at the start", "job", j.ID, "pr", j.label())
		}
		if !j.Cancel && j.Phase == resolvePhaseAgent && j.Run != "" {
			e.log.Info("staging resolver: a job goes on after the restart", "job", j.ID, "pr", j.label(), "agent", j.Agent)
			continue
		}
		w := &resolveWork{e: e, r: r, job: &j, git: r.git, held: held, keepSlot: !held}
		note := w.release(ctx)
		if j.Cancel {
			if err := r.end(ctx, e, &j, resolveFailed, whyCancelled); err != nil {
				e.warnUnlessStopped(ctx, err, "staging resolver: end a job", "job", j.ID)
			}
			continue
		}
		why := "starts again: the daemon restarted before its agent finished"
		if note != "" {
			why += "; " + note
		}
		if _, err := r.update(ctx, e, j.ID, func(j *resolveJob) {
			*j = resolveJob{ID: j.ID, Effect: j.Effect, Repo: j.Repo, Number: j.Number, Target: j.Target, TargetTip: j.TargetTip,
				Head: j.Head, Try: j.Try, State: resolveQueued, Why: oneLine(why, 300), Started: j.Started}
		}); err != nil {
			e.warnUnlessStopped(ctx, err, "staging resolver: queue a job again", "job", j.ID)
		}
	}
	r.wake(e)
}

// tendStagingResolves is the tick's part of the resolver: the ref of a done
// job goes once its effect is no longer live (s.Effects: its journal says it
// ended, or holds it no more), or resolveRefKeep after the job's end; an
// ended job's row goes resolveKeep after its end, with its directory; and
// while a job is live the worker is woken (one that waits for its slot
// looks again). A ref that cannot be dropped keeps its row no longer than
// that: when its repository is gone the row no longer keeps it, and past
// resolveKeep the row goes all the same (logged once), the ref left there.
// Nothing without a resolver.
func (e *Engine) tendStagingResolves(ctx context.Context, s Staging) {
	r := e.d.stagingResolver
	if r == nil {
		return
	}
	jobs, err := e.resolveJobs(ctx)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "staging resolver: read the jobs")
		return
	}
	now := e.now()
	effects := map[string][]staging.Effect{}
	live := false
	for _, j := range jobs {
		switch {
		case j.live():
			live = true
		case j.Kept:
			k := strings.ToLower(j.Repo)
			effs, read := effects[k]
			if !read && s != nil {
				effs = s.Effects(j.Repo)
				effects[k] = effs
			}
			on := slices.ContainsFunc(effs, func(f staging.Effect) bool {
				return string(f.ID) == j.Effect && (f.State == staging.EffectQueued || f.State == staging.EffectRunning)
			})
			if on && now.Sub(j.Ended) < resolveRefKeep {
				continue
			}
			if r.git == nil {
				continue
			}
			if err := r.git.UpdateRefDelete(ctx, j.Dir, j.ref()); err != nil {
				switch {
				case now.Sub(j.Ended) >= resolveKeep:
					e.log.Warn("staging resolver: a merge's ref stays, its job's row goes", "job", j.ID, "ref", j.ref(), "dir", j.Dir, "err", err)
					e.dropResolveJob(ctx, j.ID)
					continue
				case fsx.Exists(j.Dir):
					e.warnUnlessStopped(ctx, err, "staging resolver: drop a merge's ref", "job", j.ID, "ref", j.ref())
					continue
				}
				e.log.Info("staging resolver: a merge's repository is gone, with its ref", "job", j.ID, "dir", j.Dir)
			}
			if _, err := r.update(ctx, e, j.ID, func(j *resolveJob) { j.Kept = false }); err != nil {
				e.warnUnlessStopped(ctx, err, "staging resolver: record a dropped ref", "job", j.ID)
			}
		case now.Sub(j.Ended) >= resolveKeep:
			e.dropResolveJob(ctx, j.ID)
		}
	}
	if live {
		r.wake(e)
	}
}

// dropResolveJob deletes job id's row and its directory.
func (e *Engine) dropResolveJob(ctx context.Context, id string) {
	e.delKV(ctx, kvStagingResolve(id))
	if err := os.RemoveAll(e.resolveDir(id)); err != nil {
		e.log.Warn("staging resolver: an ended job's directory stays", "job", id, "err", err)
	}
}
