package engine

// The retro (DECISIONS "Learning loop: daily retro"): after a pull request
// magnum reviewed closes, the comments other reviewers made on it are
// collected, what magnum already posted is dropped (internal/learn), the
// rest is written to a directory with the commented files, a Classifier
// sorts it, and every comment becomes a row of the misses table, every
// finding of magnum's that someone answered a row of declines
// (retro_declines.go). It runs in its own goroutine, one at a time, once a
// day after [learn] daily_at when [learn] enabled, or whenever `magnum
// retro` asks (ReqRetro), and takes a PR only once it closed [learn] settle
// ago (unless `magnum retro <ref>` names it), so the reviews posted right
// after a merge are in. A miss for the repository's notes (class miss,
// scope repo) marks the repository for its next notes curation
// (KVNotesMisses, notes_curate.go). Comment text stays in the retro's files
// and the registry, reply text in the files only: no event or log line
// carries either.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

const (
	// KVRetroDay is the local day (store.DayKey) of the last daily retro
	// that finished; the next one waits for another day.
	KVRetroDay = "learn.retro_day"
	// KVRetroLast is the last retro's RetroSummary (JSON).
	KVRetroLast = "learn.retro_last"
	// kvRetroQueued holds the `magnum retro` asked while another learn job
	// ran (retroQueued as JSON), which starts once that job ended; one at a
	// time, kept across a restart.
	kvRetroQueued = "learn.retro_queued"
	// retroKeep is how long a retro's run directory is kept.
	retroKeep = 30 * 24 * time.Hour
	// retroRunFormat names a run directory (local time).
	retroRunFormat = "20060102-150405"
	// retroWhyRunes clips a failure's text in events and retro_prs.error.
	retroWhyRunes = 200
	// retroCloseTimeout bounds closing the classifier after a retro.
	retroCloseTimeout = 2 * time.Minute
	// retroOfflineRetry is how long the daily retro waits after GitHub could
	// not be reached before it starts again (the day is not done).
	retroOfflineRetry = 15 * time.Minute
)

// RetroPayload is a `magnum retro` request: a retro now, whatever [learn]
// enabled and daily_at say (a drain, an infrastructure pause or a pause of
// the agent's CLI still holds it; `magnum pause` holds neither this nor the
// daily one).
type RetroPayload struct {
	// PRs limits the retro to these pull requests (registry ids), whenever
	// they closed, and implies Again for them; empty = every PR due within
	// the lookback.
	PRs []int64 `json:"prs,omitempty"`
	// Again also looks at PRs a retro already looked at.
	Again bool `json:"again,omitempty"`
	// Lookback replaces [learn] lookback (config.ParseDuration: "14d").
	Lookback string `json:"lookback,omitempty"`
}

// RetroSummary is what the last retro did (KVRetroLast).
type RetroSummary struct {
	Run        string    `json:"run"` // its directory under learn/retro
	At         time.Time `json:"at"`
	Finished   time.Time `json:"finished"`
	PRs        int       `json:"prs"`        // PRs looked at
	Classified int       `json:"classified"` // PRs whose candidates were classified
	Misses     int       `json:"misses"`     // candidates classified as a miss
	Caught     int       `json:"caught"`     // comments magnum had already posted
	Failed     int       `json:"failed"`     // PRs that failed
	Stopped    string    `json:"stopped,omitempty"`
	// Declines counts the answers to magnum's own findings it stored
	// (store.Decline), Declined those the author declined or argued.
	Declines int `json:"declines,omitempty"`
	Declined int `json:"declined,omitempty"`
}

// RetroRun is one retro: its id and directory (learn/retro/<id>).
type RetroRun struct {
	ID  string
	Dir string
}

// ClassifyJob is one PR's candidates for a Classifier: Dir holds
// CandidatesPath (learn.Candidates) and the commented files
// (learn.FilePath); the classifier writes its answer (learn.Output) to
// OutputPath.
type ClassifyJob struct {
	Dir            string
	CandidatesPath string
	OutputPath     string
	PR             store.PR
	Repo           store.Repo
	// ReviewedSHAs are the commits magnum reviewed, oldest first.
	ReviewedSHAs []string
	// Candidates are the candidates file's, for checking the answer, and
	// Rejected its rejected findings, which the answer may name; Threads
	// are its own threads, each of which the answer must classify.
	Candidates []learn.Candidate
	Rejected   []learn.Rejected
	Threads    []learn.OwnThread
}

// ClassifyResult is how a Classifier's turn ended, besides its error.
type ClassifyResult struct {
	// Unclassified: nothing classified the candidates (no classifier is
	// set up); they are stored unclassified, not as a failure.
	Unclassified bool
	// Pause: the classifier's agent hit a limit (Kind: usage_limit,
	// login_required, model_limit or overloaded). The retro stops without
	// recording the PR, which stays due; a usage limit and a logout also
	// pause the tool, as after a round.
	Pause *pipeline.Pause
}

// ErrClassifierDown wraps a Classifier's error that is not the PR's: the
// agent could not be set up or started, or went away (herdr down, a prompt
// refused before it was sent). The retro stops without recording the PR.
var ErrClassifierDown = errors.New("engine: the retro's classifier is not available")

// Classifier sorts the candidates of one PR at a time, writing
// ClassifyJob.OutputPath. One serves a whole retro; Close ends it.
type Classifier interface {
	Classify(ctx context.Context, job ClassifyJob) (ClassifyResult, error)
	Close(ctx context.Context) error
}

// RetroGitHub is what a retro reads from GitHub (*github.Client): the
// review threads, every review, the comparison of a reviewed commit with a
// later one and the commented files.
type RetroGitHub interface {
	ReviewThreads(ctx context.Context, owner, repo string, number int) ([]github.Thread, error)
	Reviews(ctx context.Context, owner, repo string, number int) ([]github.Review, error)
	CompareFilesStatus(ctx context.Context, owner, repo, base, head string) (string, []github.FileDelta, error)
	FileAt(ctx context.Context, owner, repo, path, ref string) ([]byte, error)
}

var _ RetroGitHub = (*github.Client)(nil)

// retroData feeds the retro prompt (prompts/retro.md): paths, the pull
// request's URL and the commits magnum reviewed, never a comment's text.
type retroData struct {
	URL          string   // the pull request
	ReviewedSHAs []string // the commits magnum reviewed, oldest first
	Candidates   string   // the candidates file (learn.Candidates)
	Files        string   // the commented files: <Files>/<sha12>/<path>
	Output       string   // the answer file the agent writes (learn.Output)
	Count        int      // the candidates to classify
	Threads      int      // magnum's own findings someone answered (learn.OwnThread)
}

// retroQueued is a queued `magnum retro` (kvRetroQueued): what it looks at,
// as the request said, and when it was asked. Its settle delay is [learn]
// settle's when it starts.
type retroQueued struct {
	PRs      []int64   `json:"prs,omitempty"`
	Again    bool      `json:"again,omitempty"`
	Lookback string    `json:"lookback"` // time.Duration.String()
	At       time.Time `json:"at"`
}

// retroSpec is what one retro looks at.
type retroSpec struct {
	daily    bool // the schedule's: it sets KVRetroDay when it finishes
	prs      []int64
	again    bool
	lookback time.Duration
	// settle: a PR closed more recently waits for a later retro (ignored
	// when prs names the PRs).
	settle time.Duration
}

// maybeRetro starts the queued retro (startQueuedRetro), which holds the
// day's while it waits, or else the day's retro ([learn] enabled, past
// daily_at, not run today, no learn job running (learnBusy: a retro, a
// notes curation, a skill edit or gate), none waiting after GitHub stopped
// it, no drain or infrastructure pause, the agent's CLI not paused at a
// usage limit or a logout). `magnum pause` holds automatic
// reviews only: the retro reviews nothing, and a pause kept overnight
// started it hours late. A dry run and `magnum daemon --once` never start
// one.
func (e *Engine) maybeRetro(ctx context.Context) {
	lc := e.cfg.Learn
	if e.d.DryRun || e.once || e.startQueuedRetro(ctx) || !lc.Enabled {
		return
	}
	now := e.now()
	at, err := lc.DailyTime(now.Local())
	if err != nil || now.Before(at) {
		return
	}
	// A read that failed because the daemon is stopping answers "" like a
	// day never run: it must not start the retro.
	if day, _ := e.getKV(ctx, KVRetroDay); day == store.DayKey(now) || ctx.Err() != nil {
		return
	}
	if e.learnBusy() != "" || now.Before(e.retryRetroAt()) || e.holdReason(ctx) != "" || e.retroToolPause(ctx) != "" {
		return
	}
	e.startRetro(ctx, retroSpec{daily: true, lookback: lc.Lookback.Duration, settle: lc.Settle.Duration})
}

// requestRetro serves `magnum retro`: a retro now, unless a drain, an
// infrastructure pause or a pause of the agent's CLI holds it (refused: a
// queue cannot wait them out) or one is running. While another learn job
// runs it is queued (queueRetro, the answer names the job) and starts once
// that job ended, before any curation; one retro waits at a time.
func (e *Engine) requestRetro(ctx context.Context, p RetroPayload) (string, error) {
	lookback := e.cfg.Learn.Lookback.Duration
	if p.Lookback != "" {
		d, err := config.ParseDuration(p.Lookback)
		if err != nil || d <= 0 {
			return "", fmt.Errorf("lookback %q: want a positive duration such as 14d", p.Lookback)
		}
		lookback = d
	}
	if reason := cmp.Or(e.holdReason(ctx), e.retroToolPause(ctx)); reason != "" {
		return "", fmt.Errorf("no retro now: %s", reason)
	}
	if e.d.DryRun {
		e.rec.Record(ctx, "", "retro", "a retro of the PRs closed within "+lookback.String())
		return "dry run: would run a retro", nil
	}
	if q, ok := e.queuedRetro(ctx); ok {
		return "a retro is queued already (asked " + q.At.Local().Format("15:04") + ") and " + e.queuedRetroStarts(), nil
	}
	// PRs named explicitly are looked at whether or not a retro did, and
	// whenever they closed.
	settle := e.cfg.Learn.Settle.Duration
	spec := retroSpec{prs: p.PRs, again: p.Again || len(p.PRs) > 0, lookback: lookback, settle: settle}
	if job, since := e.learnRunning(); job != "" && job != retroJob { // learn jobs run one at a time
		return e.queueRetro(ctx, spec, job, since)
	}
	started, since, other := e.startRetro(ctx, spec)
	switch {
	case !started && other == "": // the daemon began stopping or draining since holdReason
		return "", fmt.Errorf("no retro now: %s", cmp.Or(e.stopping(ctx), "the daemon is stopping"))
	case !started && other != retroJob: // another learn job started since learnRunning
		return e.queueRetro(ctx, spec, other, since)
	case !started:
		return fmt.Sprintf("a retro is already running (started %s); `magnum logs` follows it", since.Local().Format("15:04")), nil
	}
	what := "the PRs closed within " + lookback.String()
	switch {
	case len(p.PRs) > 0:
		what = fmt.Sprintf("%d PR(s)", len(p.PRs))
	case settle > 0:
		what += " and at least " + humanDuration(settle) + " ago"
	}
	return "retro started on " + what + "; `magnum misses` lists what it finds", nil
}

// queueRetro makes spec, asked while job (started since) runs, the queued
// retro (kvRetroQueued) and answers as a queued curation does.
func (e *Engine) queueRetro(ctx context.Context, spec retroSpec, job string, since time.Time) (string, error) {
	b, err := json.Marshal(retroQueued{PRs: spec.prs, Again: spec.again, Lookback: spec.lookback.String(), At: e.now()})
	if err == nil {
		err = e.st.SetKV(ctx, kvRetroQueued, string(b))
	}
	if err != nil {
		return "", fmt.Errorf("no retro now: %s runs, and the retro could not be queued: %w", job, err)
	}
	what := "the PRs closed within " + spec.lookback.String()
	if len(spec.prs) > 0 {
		what = fmt.Sprintf("%d PR(s)", len(spec.prs))
	}
	at := since.Local().Format("15:04")
	e.event(ctx, "info", "", "retro.queued", fmt.Sprintf("retro of %s queued: starts when %s (started %s) ends", what, job, at),
		map[string]any{"lookback": spec.lookback.String(), "prs": len(spec.prs)})
	return fmt.Sprintf("queued: starts when %s (started %s) ends; `magnum logs` follows it", job, at), nil
}

// queuedRetro reads the queued retro (false: none, or a value that does
// not read, which startQueuedRetro drops).
func (e *Engine) queuedRetro(ctx context.Context) (retroQueued, bool) {
	var q retroQueued
	if v, ok := e.getKV(ctx, kvRetroQueued); !ok || v == "" || json.Unmarshal([]byte(v), &q) != nil {
		return retroQueued{}, false
	}
	return q, true
}

// queuedRetroStarts says when the queued retro starts, for an answer.
func (e *Engine) queuedRetroStarts() string {
	if job, since := e.learnRunning(); job != "" {
		return fmt.Sprintf("starts when %s (started %s) ends; `magnum logs` follows it", job, since.Local().Format("15:04"))
	}
	return "starts at the next tick; `magnum logs` follows it"
}

// startQueuedRetro starts the queued retro once no learn job runs and
// nothing holds it (a drain, an infrastructure pause, a pause of its
// agent's CLI): meanwhile it stays queued, and the next tick tries again.
// The tick runs it before maybeCurate, so it goes before the curations
// queued or due then: a retro feeds them with misses. True while a retro is
// queued or when it started; one whose value does not read is dropped.
func (e *Engine) startQueuedRetro(ctx context.Context) bool {
	v, ok := e.getKV(ctx, kvRetroQueued)
	if !ok || v == "" {
		return false
	}
	var q retroQueued
	err := json.Unmarshal([]byte(v), &q)
	lookback := time.Duration(0)
	if err == nil {
		lookback, err = time.ParseDuration(q.Lookback)
	}
	if err != nil || lookback <= 0 {
		e.delKV(ctx, kvRetroQueued)
		e.event(ctx, "warn", "", "retro.queue_dropped", "the queued retro does not read, so it is dropped: `magnum retro` asks again", nil)
		return false
	}
	if e.learnBusy() != "" || e.holdReason(ctx) != "" || e.retroToolPause(ctx) != "" {
		return true
	}
	if started, _, _ := e.startRetro(ctx, retroSpec{prs: q.PRs, again: q.Again, lookback: lookback, settle: e.cfg.Learn.Settle.Duration}); started {
		e.delKV(ctx, kvRetroQueued)
	}
	return true
}

// retroToolPause is why the retro's agent cannot work now ("" = it can):
// its CLI is paused after a usage limit or a logout, as rounds that need it
// wait. Without a classifier nothing needs the CLI.
func (e *Engine) retroToolPause(ctx context.Context) string {
	if e.d.Classifier == nil {
		return ""
	}
	return e.kindPauseReason(ctx, []string{e.cfg.LearnRole().AgentKind()})
}

// retroBusy reports whether a retro is running.
func (e *Engine) retroBusy() bool {
	e.retroMu.Lock()
	defer e.retroMu.Unlock()
	return e.retroCancel != nil
}

// retryRetroAt is when the daily retro that GitHub stopped starts again
// (zero: none waits).
func (e *Engine) retryRetroAt() time.Time {
	e.retroMu.Lock()
	defer e.retroMu.Unlock()
	return e.retroRetry
}

// retroJob is the retro's name among the learn jobs (learnRunning).
const retroJob = "the retro"

// startRetro runs a retro in its own goroutine on a child of ctx (the
// daemon's: shutdown cancels it), unless the daemon is stopping or draining
// (stopping: it reports false, a zero time and no job) or a learn job runs,
// a retro or another (false, when that one started and learnRunning's name
// of it).
func (e *Engine) startRetro(ctx context.Context, spec retroSpec) (bool, time.Time, string) {
	if e.stopping(ctx) != "" {
		return false, time.Time{}, ""
	}
	e.learnMu.Lock()
	defer e.learnMu.Unlock()
	if job, since := e.learnRunning(); job != "" {
		return false, since, job
	}
	e.retroMu.Lock()
	defer e.retroMu.Unlock()
	rctx, cancel := context.WithCancel(ctx)
	e.retroCancel, e.retroStarted = cancel, e.now()
	started := e.retroStarted
	e.retroWG.Go(func() {
		defer func() {
			e.retroMu.Lock()
			e.retroCancel = nil
			e.retroMu.Unlock()
			cancel()
		}()
		e.safely(rctx, "retro", "", func() { e.runRetro(rctx, spec) },
			func(ctx context.Context, msg string) { e.retroPanicked(ctx, spec, started, msg) })
	})
	return true, e.retroStarted, ""
}

// retroPanicked records a retro that panicked as a stopped one (KVRetroLast)
// and, for the daily retro, as the day's (KVRetroDay), so it is not started
// again on every tick; the PRs it did not finish stay due for the next one.
func (e *Engine) retroPanicked(ctx context.Context, spec retroSpec, started time.Time, msg string) {
	sum := RetroSummary{At: started, Finished: e.now(), Stopped: "the retro panicked: " + msg}
	if b, err := json.Marshal(sum); err == nil {
		e.setKV(ctx, KVRetroLast, string(b))
	}
	if spec.daily {
		e.setKV(ctx, KVRetroDay, store.DayKey(started))
	}
}

// stopRetro cancels a running retro (shutdown).
func (e *Engine) stopRetro() {
	e.retroMu.Lock()
	defer e.retroMu.Unlock()
	if e.retroCancel != nil {
		e.retroCancel()
	}
}

// runRetro looks at the PRs due, newest closed first, until max_prs of
// them had candidates to classify, a stop that is not a PR's (retroPR) or
// ctx ended, then records the summary (KVRetroLast) and, for the daily
// retro, the day (KVRetroDay): a crash runs it again. A daily retro that
// GitHub stopped (retroOutcome.offline) records no day: it starts again
// retroOfflineRetry later. A retro a shutdown cut short (ctx ended) records
// neither, so it counts as neither done nor failed: the summary stays the
// last finished retro's and the next start runs the day's again; its
// retro.done event says so, as info, and the new general evidence it
// stored is marked for the editor as a finished retro's is
// (KVLearnEvidence: the retro run again finds it stored already). The next
// retro takes every PR still due: the ones not reached, the one a stop
// interrupted (it got no retro_prs row) and the failed ones until their
// third attempt; the rest are skipped.
func (e *Engine) runRetro(ctx context.Context, spec retroSpec) {
	start := e.now()
	run := RetroRun{ID: start.Local().Format(retroRunFormat)}
	run.Dir = filepath.Join(e.d.Layout.Learn(), "retro", run.ID)
	sum := RetroSummary{Run: run.ID, At: start}
	q := store.RetroQuery{Since: start.Add(-spec.lookback), Again: spec.again, PRIDs: spec.prs}
	if spec.settle > 0 && len(spec.prs) == 0 {
		q.Until = start.Add(-spec.settle)
	}
	msg := fmt.Sprintf("retro %s: looking at the PRs closed within %s", run.ID, spec.lookback)
	var data map[string]any
	if settling, err := e.st.RetroSettling(ctx, q); err == nil && settling > 0 {
		msg += fmt.Sprintf("; %s for the %s settle delay", textx.Count(settling, "PR waits", "PRs wait"), humanDuration(spec.settle))
		data = map[string]any{"settling": settling}
	}
	e.event(ctx, "info", "", "retro.start", msg, data)

	prs, err := e.st.RetroDue(ctx, q)
	if err != nil {
		// Recorded like any other stop, so the daily retro does not try
		// again on every tick.
		sum.Stopped = "the due PRs cannot be read: " + oneLine(err.Error(), retroWhyRunes)
	}
	// The skill editor's inputs before the retro: those it adds wake the
	// scheduled editor (learn_schedule.go).
	inputsBefore, inputsErr := e.generalEvidence(ctx)
	var cl Classifier
	var clErr error
	made := false
	classifier := func() (Classifier, error) {
		if !made && e.d.Classifier != nil {
			made = true
			cl, clErr = e.d.Classifier(ctx, run)
		}
		return cl, clErr
	}
	defer func() {
		if cl == nil {
			return
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), retroCloseTimeout)
		defer cancel()
		if err := cl.Close(cctx); err != nil {
			e.event(cctx, "warn", "", "retro.close", "retro: closing the classifier: "+oneLine(err.Error(), retroWhyRunes), nil)
		}
	}()

	withCandidates := 0
	offline := false                 // GitHub could not be reached
	repoMisses := map[string]int{}   // repository -> its misses for the notes
	repoDeclines := map[string]int{} // repository -> its declines routed to the notes
	for _, pr := range prs {
		if ctx.Err() != nil || withCandidates >= e.cfg.Learn.MaxPRs {
			break
		}
		o := e.retroPR(ctx, run, pr, classifier)
		if o.stop != "" && o.status == "" { // interrupted: the PR stays due
			sum.Stopped, offline = o.stop, o.offline
			break
		}
		sum.PRs++
		sum.Misses += o.misses
		sum.Caught += o.caught
		sum.Declines += o.declines
		sum.Declined += o.declined
		if o.repoMisses > 0 {
			repoMisses[o.repo] += o.repoMisses
		}
		if o.repoDeclines > 0 {
			repoDeclines[o.repo] += o.repoDeclines
		}
		if o.candidates > 0 || o.threads > 0 {
			withCandidates++
		}
		switch o.status {
		case store.RetroClassified:
			sum.Classified++
		case store.RetroFailed:
			sum.Failed++
		}
		if o.stop != "" {
			sum.Stopped = o.stop
			break
		}
	}
	sum.Finished = e.now()
	shutdown := ctx.Err() != nil
	if b, err := json.Marshal(sum); err == nil && !shutdown {
		e.setKV(context.WithoutCancel(ctx), KVRetroLast, string(b))
	}
	e.markRepoMisses(context.WithoutCancel(ctx), run, repoMisses, repoDeclines)
	if err := e.syncLearnedCorpus(context.WithoutCancel(ctx)); err != nil { // learn_corpus.go
		e.event(context.WithoutCancel(ctx), "warn", "", "learn.corpus", "retro "+run.ID+": the learned eval corpus was not written: "+
			oneLine(err.Error(), retroWhyRunes), map[string]any{"run": run.ID})
	}
	msg = fmt.Sprintf("retro %s: %d PR(s), %d classified, %d miss(es), %d already caught, %d failed",
		run.ID, sum.PRs, sum.Classified, sum.Misses, sum.Caught, sum.Failed)
	data = map[string]any{"run": run.ID, "prs": sum.PRs, "classified": sum.Classified, "misses": sum.Misses, "caught": sum.Caught,
		"failed": sum.Failed}
	// The evidence is marked whatever the shutdown: what the retro stored
	// stays, and the day's retro run again finds it there already.
	if inputsErr == nil {
		if after, err := e.generalEvidence(context.WithoutCancel(ctx)); err == nil {
			if n := newEvidence(inputsBefore, after); n > 0 {
				e.markEvidence(context.WithoutCancel(ctx), run.ID, n)
				msg += fmt.Sprintf(", %s for the skill editor", textx.Count(n, "new general input", "new general inputs"))
				data["learn_inputs"] = n
			}
		}
	}
	if sum.Declines > 0 {
		msg += fmt.Sprintf(", %d answered finding(s), %d declined", sum.Declines, sum.Declined)
		data["declines"], data["declined"] = sum.Declines, sum.Declined
	}
	switch {
	case shutdown:
		msg += "; stopped by the daemon's shutdown (neither done nor failed: the PRs not reached stay due)"
	case sum.Stopped != "":
		msg += "; stopped: " + sum.Stopped
		if offline && spec.daily {
			msg += "; the daily retro starts again in " + humanDuration(retroOfflineRetry)
		}
	}
	level := "info"
	if sum.Failed > 0 || (sum.Stopped != "" && !shutdown) {
		level = "warn"
	}
	e.event(context.WithoutCancel(ctx), level, "", "retro.done", msg, data)
	switch {
	case !spec.daily || ctx.Err() != nil:
	case offline:
		e.retroMu.Lock()
		e.retroRetry = e.now().Add(retroOfflineRetry)
		e.retroMu.Unlock()
	default:
		e.setKV(ctx, KVRetroDay, store.DayKey(start))
	}
}

// retroOutcome is what one PR's retro did.
type retroOutcome struct {
	status     string // store.Retro*; "" when a stop interrupted the PR (no retro_prs row)
	candidates int    // classified (or left unclassified), outside ones not counted
	misses     int
	caught     int
	stop       string // why the retro stops
	// offline: the stop is GitHub's (it could not be reached).
	offline bool
	// repoMisses counts the misses stored for the notes of repo (class
	// miss, scope repo, still new), repoDeclines the declines routed there
	// (declinesForNotes).
	repoMisses   int
	repoDeclines int
	repo         string
	// threads counts magnum's own threads someone answered; declines the
	// answers stored, declined those the author declined or argued.
	threads, declines, declined int
}

// markRepoMisses marks every repository a retro stored misses for its
// notes of (KVNotesMisses), the comments' (misses) and the declined
// findings' that need the repository's context (declines, routeDecline),
// once the retro is over, so the curation they trigger starts after it and
// takes them all; a notes.misses event says how many.
func (e *Engine) markRepoMisses(ctx context.Context, run RetroRun, misses, declines map[string]int) {
	repos := map[string]int{}
	maps.Copy(repos, misses)
	maps.Copy(repos, declines)
	for _, full := range slices.Sorted(maps.Keys(repos)) {
		n, d := misses[full]+declines[full], declines[full]
		what := textx.Count(n, "miss", "misses")
		data := map[string]any{"run": run.ID, "misses": n}
		if d > 0 {
			what += " (" + textx.Count(d, "from a declined finding", "from declined findings") + ")"
			data["declines"] = d
		}
		e.setKV(ctx, KVNotesMisses(full), store.FormatTime(e.now()))
		e.event(ctx, "info", notesSubjectOf(full), "notes.misses",
			fmt.Sprintf("retro %s: %s for the notes of %s; its next notes curation takes them", run.ID, what, full), data)
	}
}

// retroPR runs the retro of one PR: candidates from GitHub and the
// registry (learn.Build), their directory with the commented files, the
// classifier's answer, one misses row per candidate (outside ones too), the
// PR's retro_prs row and its retro.* events. A failure of the PR's own (an
// answer still invalid after the nudge, GitHub refusing the PR) is a failed
// row, which later retros try again (store.RetroMaxAttempts). One that is
// not the PR's (a shutdown, a GitHub read that failed for a connection
// cause, github.ConnectionCause, a classifier that cannot start or went
// away, ErrClassifierDown, or a limit the classifier hit) writes no row and
// stops the retro, so the PR stays due; only a usage limit and a logout
// pause the tool, as in rounds (a per-model limit and an overload are the
// model's or the moment's).
func (e *Engine) retroPR(ctx context.Context, run RetroRun, pr store.PR, classifier func() (Classifier, error)) retroOutcome {
	repo, err := e.st.RepoByID(ctx, pr.RepoID)
	if err != nil {
		e.event(ctx, "warn", fmt.Sprintf("pr:%d", pr.ID), "retro.fail", "retro: "+oneLine(err.Error(), retroWhyRunes), nil)
		return retroOutcome{status: store.RetroFailed}
	}
	subject := prSubject(repo, pr.Number)
	now := e.now()
	record := func(o retroOutcome, why string) retroOutcome {
		r := store.RetroPR{PRID: pr.ID, RetroAt: now, Day: store.DayKey(now), Status: o.status, Candidates: o.candidates, Error: why}
		if err := e.st.RecordRetroPR(context.WithoutCancel(ctx), r); err != nil {
			e.log.Warn("retro: record", "subject", subject, "err", err)
		}
		return o
	}
	stop := func(o retroOutcome, why string) retroOutcome {
		o.status, o.stop = "", why
		e.event(context.WithoutCancel(ctx), "warn", subject, "retro.stopped", "retro: stopped on this PR, which stays due: "+why, nil)
		return o
	}
	fail := func(o retroOutcome, err error) retroOutcome {
		if ctx.Err() != nil {
			return stop(o, "the daemon is stopping")
		}
		o.status = store.RetroFailed
		why := oneLine(err.Error(), retroWhyRunes)
		e.event(context.WithoutCancel(ctx), "warn", subject, "retro.fail", "retro: "+why, nil)
		return record(o, why)
	}
	// readFail is fail for what GitHub answered: a read it could not answer
	// (the network, a server error, a rate limit) is not the PR's.
	readFail := func(o retroOutcome, err error) retroOutcome {
		cause := github.ConnectionCause(err.Error())
		if cause == "" || ctx.Err() != nil {
			return fail(o, err)
		}
		o.offline = true
		if why := "GitHub unreachable"; cause != why {
			return stop(o, why+" ("+cause+")")
		}
		return stop(o, cause)
	}
	e.event(ctx, "info", subject, "retro.begin", fmt.Sprintf("retro %s: reading what other reviewers said", run.ID), nil)

	in, reviews, gh, err := e.retroInput(ctx, repo, pr)
	if err != nil {
		return readFail(retroOutcome{}, err)
	}
	res, err := learn.Build(in) // compares commits on GitHub
	if err != nil {
		return readFail(retroOutcome{}, err)
	}
	o := retroOutcome{candidates: len(res.Candidates), threads: len(res.Threads), caught: res.Caught, repo: repo.FullName()}
	for _, c := range res.Outside {
		e.storeMiss(ctx, subject, pr, c, learn.Item{Class: store.MissOutside})
	}
	counts := map[string]any{"candidates": len(res.Candidates), "outside": len(res.Outside), "caught": res.Caught, "dropped": res.Dropped}
	if len(res.Threads) > 0 {
		counts["threads"] = len(res.Threads)
	}
	if len(res.Candidates) == 0 && len(res.Threads) == 0 {
		o.status = store.RetroNothing
		e.event(ctx, "info", subject, "retro.ok", fmt.Sprintf("retro: nothing to classify (%d outside, %d caught, %d dropped)",
			len(res.Outside), res.Caught, res.Dropped), counts)
		return record(o, "")
	}

	job, err := e.retroFiles(ctx, run, repo, pr, gh, in.Reviewed, res.Candidates, res.Rejected, res.Threads)
	if err != nil {
		if ctx.Err() == nil {
			e.storeCandidates(ctx, subject, repo, pr, in.Own, res.Candidates, nil)
		}
		return fail(o, err)
	}
	items, threadItems, why := map[string]learn.Item(nil), map[string]learn.ThreadItem(nil), error(nil)
	o.status = store.RetroUnclassified
	cl, err := classifier()
	switch {
	case err != nil:
		return stop(o, "the classifier cannot start: "+oneLine(err.Error(), retroWhyRunes))
	case cl != nil:
		cres, cerr := cl.Classify(ctx, job)
		switch {
		case cres.Pause != nil:
			if p := *cres.Pause; p.Kind == string(agents.HealthUsageLimit) || p.Kind == string(agents.HealthLoginRequired) {
				e.pauseTool(context.WithoutCancel(ctx), p)
			}
			return stop(o, fmt.Sprintf("%s: %s", cres.Pause.Tool, cres.Pause.Kind))
		case ctx.Err() != nil:
			return stop(o, "the daemon is stopping")
		case errors.Is(cerr, ErrClassifierDown):
			return stop(o, oneLine(cerr.Error(), retroWhyRunes))
		}
		switch {
		case cerr != nil:
			why = cerr
		case cres.Unclassified:
		default:
			data, rerr := os.ReadFile(job.OutputPath)
			if rerr != nil {
				why = fmt.Errorf("the classifier wrote no %s", learn.OutputFile)
				break
			}
			if items, threadItems, rerr = learn.ParseOutput(data, res.Candidates, res.Rejected, res.Threads); rerr != nil {
				why = fmt.Errorf("%s is invalid: %w", learn.OutputFile, rerr)
				break
			}
			o.status = store.RetroClassified
		}
	}
	rejected, repoMisses := e.storeCandidates(ctx, subject, repo, pr, in.Own, learn.LinkRejected(res.Candidates, items, res.Rejected), items)
	o.repoMisses = repoMisses
	for _, it := range items {
		if it.Class == store.MissMiss {
			o.misses++
		}
	}
	// Only a classified answer is stored: a decline is the classifier's
	// outcome, and an own thread has nothing to store without one.
	stored, declined, derr := e.storeDeclines(ctx, pr, in, reviews, res.Threads, threadItems)
	if derr != nil {
		e.log.Warn("retro: store the declines", "subject", subject, "err", derr)
	}
	o.declines, o.declined = stored, declined
	o.repoDeclines = e.declinesForNotes(ctx, pr, res.Threads, threadItems)
	if why != nil {
		return fail(o, why)
	}
	counts["misses"], counts["status"] = o.misses, o.status
	if o.repoMisses > 0 {
		counts["repo_misses"] = o.repoMisses
	}
	if o.repoDeclines > 0 {
		counts["repo_declines"] = o.repoDeclines
	}
	if len(res.Threads) > 0 {
		counts["declines"], counts["declined"] = o.declines, o.declined
	}
	if len(rejected) > 0 {
		counts["lesson_rejected"] = rejected
	}
	msg := fmt.Sprintf("retro: %d candidate(s) left unclassified (%d outside, %d caught, %d dropped)",
		len(res.Candidates), len(res.Outside), res.Caught, res.Dropped)
	if len(res.Threads) > 0 {
		msg = fmt.Sprintf("retro: %d candidate(s) and %d answered finding(s) left unclassified (%d outside, %d caught, %d dropped)",
			len(res.Candidates), len(res.Threads), len(res.Outside), res.Caught, res.Dropped)
	}
	if o.status == store.RetroClassified {
		answered := ""
		if len(res.Threads) > 0 {
			answered = fmt.Sprintf(", %d answered finding(s), %d declined", o.declines, o.declined)
		}
		msg = fmt.Sprintf("retro: %d candidate(s) classified, %d miss(es)%s (%d outside, %d caught, %d dropped)",
			len(res.Candidates), o.misses, answered, len(res.Outside), res.Caught, res.Dropped)
		if len(rejected) > 0 {
			msg += fmt.Sprintf("; %d lesson(s) rejected (lesson_rejected)", len(rejected))
		}
	}
	e.event(ctx, "info", subject, "retro.ok", msg, counts)
	return record(o, "")
}

// retroInput reads what learn.Build needs about pr: its threads and every
// review (as the watch's poll identity), the commits magnum posted reviews
// of and the run each review's findings are under, magnum's logins and the
// judge's findings. It returns each review's run with its skill hash too
// (reviewRunsOf, for storeDeclines).
func (e *Engine) retroInput(ctx context.Context, repo store.Repo, pr store.PR) (learn.Input, map[int64]reviewRun, RetroGitHub, error) {
	w := e.cfg.WatchFor(repo.FullName())
	if w == nil {
		return learn.Input{}, nil, nil, fmt.Errorf("%s is not watched (no [[watch]] covers it)", repo.FullName())
	}
	gh := e.gh(w.PollIdentity)
	rg, ok := gh.(RetroGitHub)
	if gh == nil || !ok {
		return learn.Input{}, nil, nil, fmt.Errorf("no GitHub client that reads reviews for poll identity %q", w.PollIdentity)
	}
	runs, err := e.st.RunsByPR(ctx, pr.ID)
	if err != nil {
		return learn.Input{}, nil, nil, err
	}
	findings, err := e.st.FindingsByPR(ctx, pr.ID)
	if err != nil {
		return learn.Input{}, nil, nil, err
	}
	reviews := reviewRunsOf(runs, findings)
	in := learn.Input{
		Author:   github.Account(deref(pr.AuthorLogin), deref(pr.AuthorType)),
		Own:      e.retroOwnLogins(pr, runs),
		Findings: findings, MinChars: e.cfg.Learn.MinCommentChars, IncludeBots: e.cfg.Learn.IncludeBots,
		ReviewRuns: make(map[int64]string, len(reviews)),
	}
	for id, rr := range reviews {
		in.ReviewRuns[id] = rr.ID
	}
	for _, r := range runs {
		if r.ReviewID == nil {
			continue
		}
		at := r.CreatedAt
		for _, t := range []*time.Time{r.SubmittedAt, r.EndedAt, r.VerifiedAt} {
			if t != nil {
				at = *t
			}
		}
		for _, sha := range []string{r.TargetSHA, deref(r.ReviewCommit)} {
			if sha != "" && !slices.ContainsFunc(in.Reviewed, func(x learn.Reviewed) bool { return x.SHA == sha }) {
				in.Reviewed = append(in.Reviewed, learn.Reviewed{SHA: sha, At: at})
			}
		}
	}
	slices.SortStableFunc(in.Reviewed, func(a, b learn.Reviewed) int { return a.At.Compare(b.At) })
	if in.Threads, err = rg.ReviewThreads(ctx, repo.Owner, repo.Name, pr.Number); err != nil {
		return learn.Input{}, nil, nil, err
	}
	if in.Reviews, err = rg.Reviews(ctx, repo.Owner, repo.Name, pr.Number); err != nil {
		return learn.Input{}, nil, nil, err
	}
	in.Compare = func(from, to string) (learn.Comparison, error) {
		status, files, err := rg.CompareFilesStatus(ctx, repo.Owner, repo.Name, from, to)
		if err != nil {
			return learn.Comparison{}, err
		}
		cmp := learn.Comparison{Descendant: status == "ahead" || status == "identical", Complete: len(files) < github.CompareFileLimit}
		for _, f := range files {
			cmp.Paths = append(cmp.Paths, f.Path)
			if f.PreviousPath != "" {
				cmp.Paths = append(cmp.Paths, f.PreviousPath)
			}
		}
		return cmp, nil
	}
	return in, reviews, rg, nil
}

// retroOwnLogins are magnum's logins on pr in Account form, each once: every
// configured identity's, the last review's and the ones pr's runs posted
// as.
func (e *Engine) retroOwnLogins(pr store.PR, runs []store.Run) []string {
	var out []string
	add := func(l string) {
		if l != "" && !slices.ContainsFunc(out, func(o string) bool { return github.SameAccount(o, l) }) {
			out = append(out, l)
		}
	}
	for _, id := range e.cfg.Identities {
		add(id.Login)
	}
	add(deref(pr.LastReviewLogin))
	for _, r := range runs {
		add(r.ReviewerLogin)
	}
	return out
}

// retroFiles writes the PR's retro directory, run/<owner>/<repo>/<N>/: each
// commented file, and the file of each of magnum's own answered findings,
// at its reviewed commit under files/ (each once; skipped, and said so in
// the candidate or thread, when GitHub has no copy or it is above
// github.FileAtLimit) and candidates.json with the rejected findings and
// magnum's own answered threads (replies included), every write confined to
// the directory (os.Root): the paths come from GitHub.
func (e *Engine) retroFiles(ctx context.Context, run RetroRun, repo store.Repo, pr store.PR, gh RetroGitHub,
	reviewed []learn.Reviewed, cands []learn.Candidate, rejected []learn.Rejected, threads []learn.OwnThread) (ClassifyJob, error) {
	dir := filepath.Join(run.Dir, repo.Owner, repo.Name, strconv.Itoa(pr.Number))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ClassifyJob{}, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return ClassifyJob{}, err
	}
	defer root.Close()
	fetched := map[string]string{} // FilePath -> why it was skipped ("" = written)
	fetch := func(path, sha string) (file, skipped string) {
		rel := learn.FilePath(sha, path)
		why, done := fetched[rel]
		if !done {
			why = e.retroFetch(ctx, root, repo, gh, rel, path, sha)
			fetched[rel] = why
		}
		if why != "" {
			return "", why
		}
		return rel, ""
	}
	cands = append([]learn.Candidate{}, cands...) // a list, even when only own threads are classified
	for i := range cands {
		if c := &cands[i]; c.Path != "" {
			c.File, c.FileSkipped = fetch(c.Path, c.ReviewedSHA)
		}
	}
	threads = slices.Clone(threads) // the classifier judges a finding against its code, not the author's claim alone
	for i := range threads {
		if th := &threads[i]; th.Path != "" && th.ReviewedSHA != "" {
			th.File, th.FileSkipped = fetch(th.Path, th.ReviewedSHA)
		}
	}
	shas := make([]string, 0, len(reviewed))
	for _, r := range reviewed {
		shas = append(shas, r.SHA)
	}
	b, err := json.MarshalIndent(learn.Candidates{PR: pr.URL, ReviewedSHAs: shas, FilesDir: learn.FilesDir, Candidates: cands,
		Rejected: rejected, Threads: threads}, "", "  ")
	if err != nil {
		return ClassifyJob{}, err
	}
	if err := learn.WriteFileAtomic(root, learn.CandidatesFile, b); err != nil {
		return ClassifyJob{}, err
	}
	return ClassifyJob{Dir: dir, CandidatesPath: filepath.Join(dir, learn.CandidatesFile), OutputPath: filepath.Join(dir, learn.OutputFile),
		PR: pr, Repo: repo, ReviewedSHAs: shas, Candidates: cands, Rejected: rejected, Threads: threads}, nil
}

// retroFetch copies path at sha to rel inside root; it returns why it did
// not ("" = copied).
func (e *Engine) retroFetch(ctx context.Context, root *os.Root, repo store.Repo, gh RetroGitHub, rel, path, sha string) string {
	data, err := gh.FileAt(ctx, repo.Owner, repo.Name, path, sha)
	switch {
	case errors.Is(err, github.ErrFileTooLarge):
		return fmt.Sprintf("larger than %d KiB", github.FileAtLimit>>10)
	case errors.Is(err, github.ErrNotFound):
		return "not on GitHub at the reviewed commit"
	case err != nil:
		e.log.Warn("retro: read a commented file", "repo", repo.FullName(), "sha", textx.ShortSHA(sha), "err", err)
		return "GitHub could not be read"
	}
	if err := learn.WriteFileAtomic(root, rel, data); err != nil {
		e.log.Warn("retro: write a commented file", "repo", repo.FullName(), "err", err)
		return "its path cannot be written inside the retro directory"
	}
	return ""
}

// storeCandidates stores one misses row per candidate, classified by items
// (nil = unclassified, which never replaces an earlier classification:
// store.UpsertMiss), and returns the ids whose lesson was dropped
// (learn.ScrubLesson), each also a retro.lesson_rejected event with the
// reason and never the lesson, and how many stored rows are misses for the
// repository's notes (class miss, scope repo, state new). own are magnum's
// logins.
func (e *Engine) storeCandidates(ctx context.Context, subject string, repo store.Repo, pr store.PR, own []string,
	cands []learn.Candidate, items map[string]learn.Item) ([]string, int) {
	people := []string{deref(pr.AuthorLogin)}
	for _, c := range cands {
		people = append(people, c.Reviewer)
	}
	repoWords := []string{repo.Owner, repo.Name}
	var rejected []string
	forNotes := 0
	for _, c := range cands {
		it, ok := items[c.ID]
		if !ok {
			it = learn.Item{Class: store.MissUnclassified}
		}
		if it.Lesson != "" {
			var reason string
			if it.Lesson, reason = learn.ScrubLesson(it.Lesson, it.Scope, people, own, repoWords); reason != "" {
				rejected = append(rejected, c.ID)
				e.event(ctx, "info", subject, "retro.lesson_rejected", fmt.Sprintf("retro: the lesson of %s was dropped (%s)", c.ID, reason),
					map[string]any{"candidate": c.ID, "reason": reason})
			}
		}
		if m, ok := e.storeMiss(ctx, subject, pr, c, it); ok && m.Class == store.MissMiss && m.Scope == store.MissScopeRepo && m.State == store.MissNew {
			forNotes++
		}
	}
	return rejected, forNotes
}

// storeMiss upserts candidate c as classified by it and returns the stored
// row.
func (e *Engine) storeMiss(ctx context.Context, subject string, pr store.PR, c learn.Candidate, it learn.Item) (store.Miss, bool) {
	m := store.Miss{
		PRID: pr.ID, SourceURL: c.URL, SourceKind: c.Kind, Reviewer: c.Reviewer, Path: c.Path, Line: c.Line,
		ReviewedSHA: c.ReviewedSHA, Class: it.Class, Raised: c.Raised, FindingRef: c.FindingRef, ReasonCode: c.ReasonCode,
		Lines: c.Lines(),
	}
	if it.Class == store.MissMiss {
		m.Severity, m.Title, m.Lesson, m.Scope, m.Lines, m.Match = it.Severity, it.Title, it.Lesson, it.Scope, it.Lines, it.Match
	}
	got, err := e.st.UpsertMiss(context.WithoutCancel(ctx), m)
	if err != nil {
		e.log.Warn("retro: store a miss", "subject", subject, "candidate", c.ID, "err", err)
		return store.Miss{}, false
	}
	return got, true
}

// pruneRetro removes the retro run directories older than retroKeep
// (learn/retro/<run>, named by their start time).
func (e *Engine) pruneRetro() {
	if !e.d.Layout.Valid() {
		return
	}
	root := filepath.Join(e.d.Layout.Learn(), "retro")
	ents, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := e.now().Add(-retroKeep)
	for _, ent := range ents {
		at, err := time.ParseInLocation(retroRunFormat, ent.Name(), time.Local)
		if !ent.IsDir() || err != nil || !at.Before(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, ent.Name())); err != nil {
			e.log.Warn("retro: prune a run directory", "run", ent.Name(), "err", err)
		}
	}
}
