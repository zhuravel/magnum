package engine

// The retro (DECISIONS "Learning loop: daily retro"): after a pull request
// magnum reviewed closes, the comments other reviewers made on it are
// collected, what magnum already posted is dropped (internal/learn), the
// rest is written to a directory with the commented files, a Classifier
// sorts it, and every comment becomes a row of the misses table. It runs in
// its own goroutine, one at a time, once a day after [learn] daily_at when
// [learn] enabled, or whenever `magnum retro` asks (ReqRetro), and takes a PR
// only once it closed [learn] settle ago (unless `magnum retro <ref>` names
// it), so the reviews posted right after a merge are in. A miss for the
// repository's notes (class miss, scope repo) marks the repository for its
// next notes curation (KVNotesMisses, notes_curate.go). Comment text stays in
// the retro's files and the registry: no event or log line carries it.

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
	// Rejected its rejected findings, which the answer may name.
	Candidates []learn.Candidate
	Rejected   []learn.Rejected
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

// maybeRetro starts the day's retro ([learn] enabled, past daily_at, not
// run today, none running, none waiting after GitHub stopped it, no drain
// or infrastructure pause, the agent's CLI not paused at a usage limit or a
// logout). `magnum pause` holds automatic
// reviews only: the retro reviews nothing, and a pause kept overnight
// started it hours late. A dry run and `magnum daemon --once` never start
// one.
func (e *Engine) maybeRetro(ctx context.Context) {
	lc := e.cfg.Learn
	if !lc.Enabled || e.d.DryRun || e.once {
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
	if e.retroBusy() || now.Before(e.retryRetroAt()) || e.holdReason(ctx) != "" || e.retroToolPause(ctx) != "" {
		return
	}
	e.startRetro(ctx, retroSpec{daily: true, lookback: lc.Lookback.Duration, settle: lc.Settle.Duration})
}

// requestRetro serves `magnum retro`: a retro now, unless a drain, an
// infrastructure pause or a pause of the agent's CLI holds it or one is
// running.
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
	// PRs named explicitly are looked at whether or not a retro did, and
	// whenever they closed.
	settle := e.cfg.Learn.Settle.Duration
	started, since := e.startRetro(ctx, retroSpec{prs: p.PRs, again: p.Again || len(p.PRs) > 0, lookback: lookback, settle: settle})
	if !started && since.IsZero() { // the daemon began stopping or draining since holdReason
		return "", fmt.Errorf("no retro now: %s", cmp.Or(e.stopping(ctx), "the daemon is stopping"))
	}
	if !started {
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

// startRetro runs a retro in its own goroutine on a child of ctx (the
// daemon's: shutdown cancels it), unless the daemon is stopping or draining
// (stopping: it reports false and a zero time) or one is running (false and
// when that one started).
func (e *Engine) startRetro(ctx context.Context, spec retroSpec) (bool, time.Time) {
	if e.stopping(ctx) != "" {
		return false, time.Time{}
	}
	e.retroMu.Lock()
	defer e.retroMu.Unlock()
	if e.retroCancel != nil {
		return false, e.retroStarted
	}
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
	return true, e.retroStarted
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
// retro.done event says so, as info. The next retro takes every PR still
// due: the ones not reached, the one a stop interrupted (it got no
// retro_prs row) and the failed ones until their third attempt; the rest
// are skipped.
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
	offline := false               // GitHub could not be reached
	repoMisses := map[string]int{} // repository -> its misses for the notes
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
		if o.repoMisses > 0 {
			repoMisses[o.repo] += o.repoMisses
		}
		if o.candidates > 0 {
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
	e.markRepoMisses(context.WithoutCancel(ctx), run, repoMisses)
	msg = fmt.Sprintf("retro %s: %d PR(s), %d classified, %d miss(es), %d already caught, %d failed",
		run.ID, sum.PRs, sum.Classified, sum.Misses, sum.Caught, sum.Failed)
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
	e.event(context.WithoutCancel(ctx), level, "", "retro.done", msg, map[string]any{"run": run.ID, "prs": sum.PRs,
		"classified": sum.Classified, "misses": sum.Misses, "caught": sum.Caught, "failed": sum.Failed})
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
	// miss, scope repo, still new).
	repoMisses int
	repo       string
}

// markRepoMisses marks every repository a retro stored misses for its
// notes of (KVNotesMisses), once the retro is over, so the curation they
// trigger starts after it and takes them all; a notes.misses event says
// how many.
func (e *Engine) markRepoMisses(ctx context.Context, run RetroRun, counts map[string]int) {
	for _, full := range slices.Sorted(maps.Keys(counts)) {
		n := counts[full]
		e.setKV(ctx, KVNotesMisses(full), store.FormatTime(e.now()))
		e.event(ctx, "info", notesSubjectOf(full), "notes.misses",
			fmt.Sprintf("retro %s: %s for the notes of %s; its next notes curation takes them", run.ID, textx.Count(n, "miss", "misses"), full),
			map[string]any{"run": run.ID, "misses": n})
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

	in, gh, err := e.retroInput(ctx, repo, pr)
	if err != nil {
		return readFail(retroOutcome{}, err)
	}
	res, err := learn.Build(in) // compares commits on GitHub
	if err != nil {
		return readFail(retroOutcome{}, err)
	}
	o := retroOutcome{candidates: len(res.Candidates), caught: res.Caught, repo: repo.FullName()}
	for _, c := range res.Outside {
		e.storeMiss(ctx, subject, pr, c, learn.Item{Class: store.MissOutside})
	}
	counts := map[string]any{"candidates": len(res.Candidates), "outside": len(res.Outside), "caught": res.Caught, "dropped": res.Dropped}
	if len(res.Candidates) == 0 {
		o.status = store.RetroNothing
		e.event(ctx, "info", subject, "retro.ok", fmt.Sprintf("retro: nothing to classify (%d outside, %d caught, %d dropped)",
			len(res.Outside), res.Caught, res.Dropped), counts)
		return record(o, "")
	}

	job, err := e.retroFiles(ctx, run, repo, pr, gh, in.Reviewed, res.Candidates, res.Rejected)
	if err != nil {
		if ctx.Err() == nil {
			e.storeCandidates(ctx, subject, repo, pr, in.Own, res.Candidates, nil)
		}
		return fail(o, err)
	}
	items, why := map[string]learn.Item(nil), error(nil)
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
			if items, rerr = learn.ParseOutput(data, res.Candidates, res.Rejected); rerr != nil {
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
	if why != nil {
		return fail(o, why)
	}
	counts["misses"], counts["status"] = o.misses, o.status
	if o.repoMisses > 0 {
		counts["repo_misses"] = o.repoMisses
	}
	if len(rejected) > 0 {
		counts["lesson_rejected"] = rejected
	}
	msg := fmt.Sprintf("retro: %d candidate(s) left unclassified (%d outside, %d caught, %d dropped)",
		len(res.Candidates), len(res.Outside), res.Caught, res.Dropped)
	if o.status == store.RetroClassified {
		msg = fmt.Sprintf("retro: %d candidate(s) classified, %d miss(es) (%d outside, %d caught, %d dropped)",
			len(res.Candidates), o.misses, len(res.Outside), res.Caught, res.Dropped)
		if len(rejected) > 0 {
			msg += fmt.Sprintf("; %d lesson(s) rejected (lesson_rejected)", len(rejected))
		}
	}
	e.event(ctx, "info", subject, "retro.ok", msg, counts)
	return record(o, "")
}

// retroInput reads what learn.Build needs about pr: its threads and every
// review (as the watch's poll identity), the commits magnum posted reviews
// of, magnum's logins and the judge's findings.
func (e *Engine) retroInput(ctx context.Context, repo store.Repo, pr store.PR) (learn.Input, RetroGitHub, error) {
	w := e.cfg.WatchFor(repo.FullName())
	if w == nil {
		return learn.Input{}, nil, fmt.Errorf("%s is not watched (no [[watch]] covers it)", repo.FullName())
	}
	gh := e.gh(w.PollIdentity)
	rg, ok := gh.(RetroGitHub)
	if gh == nil || !ok {
		return learn.Input{}, nil, fmt.Errorf("no GitHub client that reads reviews for poll identity %q", w.PollIdentity)
	}
	runs, err := e.st.RunsByPR(ctx, pr.ID)
	if err != nil {
		return learn.Input{}, nil, err
	}
	findings, err := e.st.FindingsByPR(ctx, pr.ID)
	if err != nil {
		return learn.Input{}, nil, err
	}
	in := learn.Input{
		Author:   github.Account(deref(pr.AuthorLogin), deref(pr.AuthorType)),
		Findings: findings, MinChars: e.cfg.Learn.MinCommentChars, IncludeBots: e.cfg.Learn.IncludeBots,
	}
	own := func(l string) {
		if l != "" && !slices.ContainsFunc(in.Own, func(o string) bool { return github.SameAccount(o, l) }) {
			in.Own = append(in.Own, l)
		}
	}
	for _, id := range e.cfg.Identities {
		own(id.Login)
	}
	own(deref(pr.LastReviewLogin))
	for _, r := range runs {
		own(r.ReviewerLogin)
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
		return learn.Input{}, nil, err
	}
	if in.Reviews, err = rg.Reviews(ctx, repo.Owner, repo.Name, pr.Number); err != nil {
		return learn.Input{}, nil, err
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
	return in, rg, nil
}

// retroFiles writes the PR's retro directory, run/<owner>/<repo>/<N>/: each
// commented file at its reviewed commit under files/ (skipped, and said so
// in the candidate, when GitHub has no copy or it is above
// github.FileAtLimit) and candidates.json with the rejected findings, every
// write confined to the directory (os.Root): the paths come from GitHub.
func (e *Engine) retroFiles(ctx context.Context, run RetroRun, repo store.Repo, pr store.PR, gh RetroGitHub,
	reviewed []learn.Reviewed, cands []learn.Candidate, rejected []learn.Rejected) (ClassifyJob, error) {
	dir := filepath.Join(run.Dir, repo.Owner, repo.Name, strconv.Itoa(pr.Number))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ClassifyJob{}, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return ClassifyJob{}, err
	}
	defer root.Close()
	cands = slices.Clone(cands)
	fetched := map[string]string{} // FilePath -> why it was skipped ("" = written)
	for i := range cands {
		c := &cands[i]
		if c.Path == "" {
			continue
		}
		rel := learn.FilePath(c.ReviewedSHA, c.Path)
		why, done := fetched[rel]
		if !done {
			why = e.retroFetch(ctx, root, repo, gh, rel, c.Path, c.ReviewedSHA)
			fetched[rel] = why
		}
		if why != "" {
			c.FileSkipped = why
		} else {
			c.File = rel
		}
	}
	shas := make([]string, 0, len(reviewed))
	for _, r := range reviewed {
		shas = append(shas, r.SHA)
	}
	b, err := json.MarshalIndent(learn.Candidates{PR: pr.URL, ReviewedSHAs: shas, FilesDir: learn.FilesDir, Candidates: cands,
		Rejected: rejected}, "", "  ")
	if err != nil {
		return ClassifyJob{}, err
	}
	if err := learn.WriteFileAtomic(root, learn.CandidatesFile, b); err != nil {
		return ClassifyJob{}, err
	}
	return ClassifyJob{Dir: dir, CandidatesPath: filepath.Join(dir, learn.CandidatesFile), OutputPath: filepath.Join(dir, learn.OutputFile),
		PR: pr, Repo: repo, ReviewedSHAs: shas, Candidates: cands, Rejected: rejected}, nil
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
