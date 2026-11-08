package factory

// A qa stage's run (R55 slice 1, step 4; the walk and the verdict are step
// 5's). A run belongs to the task's head: qa-run.json in the task's
// directory is the task's newest run, and each run has its folder
// qa/<n>-<sha7> (qaRunDir), which holds the probe, the user-facing files,
// what the walker saves and the run's result.json. runQA passes the stage
// on a run whose result is clean or not_needed at the head, sends the task
// to you once on a run that could not run (your retry starts a fresh one),
// and otherwise goes on with the newest run or starts one: the repository's
// qa.when reads the changed files and prints the user-facing ones (none: no
// QA is needed; a failing command cannot tell, so QA could not run, never
// "nothing user-facing"), then the session starts.
//
// The session is one QA on the Mac at a time: the task's heavy slot, then the
// machine's QA lock (Layout.FactoryQALock). It refuses a copy's port that
// answers before its server started (another server holds it, and QA would
// walk the wrong app), starts the copy's server and has a goroutine off the
// loop probe its ready path until it answers 2xx or 3xx or the boot runs out
// (a failed boot gets one more), then starts the factory's headless Chrome in
// the run's folder. Its teardown (endQASession) runs on every way out: the
// run's end, NEEDS YOU, a stop or a drop (freeHeavy), the task leaving the
// stage (sweepQA) and the daemon's stop (stopQASessions), each stop on a
// context of its own. A restart cuts a run whose server started: the next
// daemon's startup records it interrupted (cutQA) and a fresh run starts, at
// most twice in a row at a head.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/textx"
)

// The files of QA: the task's newest run (qa-run.json) and the result that
// passed the stage (qa-summary.json) in the task's directory; a run's
// result, its probe and its user-facing files in the run's folder.
const (
	qaRunFile     = "qa-run.json"
	qaSummaryFile = "qa-summary.json"
	qaResultFile  = "result.json"
	qaProbeFile   = "probe.js"
	qaChangedFile = "changed.txt"
)

// heavyQA is a QA run's use of the task's heavy slot, waitQA the task's
// waits on its run (qaLockWhat, the boot, qaReadyWhat).
const (
	heavyQA     = "qa"
	waitQA      = "qa"
	qaLockWhat  = "another QA runs on this Mac"
	qaReadyWhat = "ready for the walk"
)

const (
	// qaProbeEvery is how often the boot probes the ready path, qaProbeTimeout
	// how long one probe may take; qaWhenCap bounds the user-facing check.
	qaProbeEvery   = 2 * time.Second
	qaProbeTimeout = 5 * time.Second
	qaWhenCap      = 2 * time.Minute
	// qaBootSlack is the boot's bound on the wall clock past its own (the
	// boot counts on the engine's clock), qaBootWait how long a teardown waits
	// for the boot's goroutine to end after it cancelled it.
	qaBootSlack = time.Minute
	qaBootWait  = 15 * time.Second
	// qaInARow is how many results of one kind in a row at a head
	// (interrupted, pressure) a fresh run follows: then QA is unavailable.
	// A run gets one boot more after a failed one.
	qaInARow   = 2
	qaBootMore = 1
	// qaResultVersion is the version of result.json and qa-summary.json.
	qaResultVersion = 1
	// qaEventsRead bounds the events qaTold reads.
	qaEventsRead = 200
	// qaWhyRunes clips a line a command printed in a run's reason.
	qaWhyRunes = 200
)

// qaRunDir is run k's folder at head, relative to the task's directory:
// qa/<k>-<sha7>, as factory-qa.md's QARun names it.
func qaRunDir(k int, head string) string { return fmt.Sprintf("qa/%d-%s", k, textx.ShortSHA(head)) }

// qaRunDirRe is a run's folder name under qa/.
var qaRunDirRe = regexp.MustCompile(`^([1-9][0-9]*)-[0-9a-f]+$`)

// qaRun is qa-run.json, the task's newest QA run: its number, the head it
// looks at and the base that head starts from, its folder (qaRunDir), the
// copy's server (its port, the repository's qa.url with it, no path, and
// the ready path), the most pages it walks, the attempts of the stage
// before it (the walker's turn is the first after them), the boots that
// failed, the CDP endpoint of its browser, the probe's nonce and the
// user-facing files (both "" and nil until the user-facing check ran to
// its end), and when its server last started (zero before: a restart cut
// nothing of a run that never started one).
type qaRun struct {
	N              int       `json:"n"`
	Port           int       `json:"port"`
	MaxPages       int       `json:"max_pages"`
	AttemptsBefore int       `json:"attempts_before"`
	BootTries      int       `json:"boot_tries"`
	Head           string    `json:"head"`
	Base           string    `json:"base"`
	Dir            string    `json:"dir"`
	URL            string    `json:"url"`
	Ready          string    `json:"ready"`
	CDP            string    `json:"cdp,omitempty"`
	Nonce          string    `json:"nonce,omitempty"`
	Changed        []string  `json:"changed,omitempty"`
	Started        time.Time `json:"started"`
}

// QAResult is how a QA run ended (result.json in its folder; qa-summary.json
// in the task's directory is the one that passed the stage): its version,
// the run's number, folder and head, the pages it walked and the shots it
// took, its outcome (QAOutcome*) and why, the defects that block and those
// that do not (Known), the states the walker could not reach, and when it
// started and ended.
type QAResult struct {
	Version  int         `json:"version"`
	Run      int         `json:"run"`
	Pages    int         `json:"pages"`
	Shots    int         `json:"shots"`
	Dir      string      `json:"dir"`
	Head     string      `json:"head"`
	Outcome  string      `json:"outcome"`
	Why      string      `json:"why"`
	Blocking []QADefect  `json:"blocking"`
	Known    []QADefect  `json:"known"`
	Blocked  []QABlocked `json:"blocked"`
	Started  time.Time   `json:"started"`
	Ended    time.Time   `json:"ended"`
}

// qaSession is a QA run's hold on the Mac: its task and stage, the run, the
// walker's copy (qaWalkerCopy), whether the boot answered (ready), whether
// the server and the browser were started (server, browser: their stop runs
// whatever came of the start), the boot's error, its goroutine's cancel and
// done (ready and err are written before done closes), and the release of
// the heavy slot and of the machine's QA lock. The loop owns it but for
// ready and err; qaMu guards the map of them.
type qaSession struct {
	task                   int64
	stage                  string
	run                    qaRun
	copy                   Copy
	ready, server, browser bool
	err                    error
	cancel                 context.CancelFunc
	done                   chan struct{}
	release, unlock        func()
}

func (s *qaSession) ended() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// qaPanic is a boot whose probe panicked: no boot more.
type qaPanic struct{ msg string }

func (p qaPanic) Error() string { return "the QA boot's probe panicked: " + p.msg }

// qaSessionOf is task id's QA session, nil for none.
func (e *Engine) qaSessionOf(id int64) *qaSession {
	e.qaMu.Lock()
	defer e.qaMu.Unlock()
	return e.qaSessions[id]
}

// runQA runs one pass of qa stage st of task t (the engine's runner for
// config.FactoryStageQA). A repository without [factory.repo.qa] needs no
// QA: the summary says so at the head, and the stage passes (nextStage skips
// the stage before it runs; this is a task that came to it before the
// block went). A build without runtime, copies or git leaves the task where
// it is. The task's branch must be at its checked head (a move goes back to
// coding, headMoved). The newest run's result at the head decides
// (qaDecide); a run with none goes on, and a fresh run starts after a
// result that asks for one or at another head.
func (e *Engine) runQA(ctx context.Context, t registry.Task, st config.FactoryStage) (bool, error) {
	repo, err := e.factoryRepo(t)
	if err != nil {
		return false, err
	}
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	if repo.QA == nil {
		now := e.now()
		res := QAResult{Version: qaResultVersion, Head: t.HeadSHA, Outcome: QAOutcomeNotNeeded, Why: "the repository has no [factory.repo.qa]",
			Started: now, Ended: now}
		return true, writeJSON(filepath.Join(dir, qaSummaryFile), res)
	}
	if e.d.Runtime == nil || e.d.Copies == nil || e.d.Git == nil {
		return false, nil
	}
	c, err := e.claimCopy(ctx, t)
	if err != nil {
		return false, err
	}
	bh, err := e.branchHead(ctx, t, c, repo)
	switch {
	case err != nil:
		return false, err
	case bh.Problem != "":
		return false, e.stageFailed(ctx, t, needsYou{"QA cannot start: " + bh.Problem})
	case t.HeadSHA != "" && bh.Head != t.HeadSHA: // QA looks at a checked head only
		e.endQASessionLogged(ctx, t, "the branch moved")
		return false, e.headMoved(ctx, t, bh.Head)
	}
	last, ok := readQARun(dir)
	run, fresh := last, !ok || last.Head != bh.Head
	if !fresh {
		if res, ended := readQAResult(dir, last); ended {
			pass, again, err := e.qaDecide(ctx, t, dir, last, res)
			if err != nil || pass || !again {
				return pass, err
			}
			fresh = true
		}
	}
	if fresh {
		e.endQASessionLogged(ctx, t, "a fresh run starts") // a session of an older run
		if run, err = e.newQARun(dir, last, bh, c, *repo.QA); err != nil {
			return false, err
		}
	}
	if run.Nonce == "" { // the user-facing check has not run to its end
		pass, err := e.qaUserFacing(ctx, t, st, c, *repo.QA, &run)
		if err != nil || pass || run.Nonce == "" {
			return pass, err
		}
	}
	return false, e.qaSession(ctx, t, c, run)
}

// qaDecide acts on run's result res at the task's head: clean or not_needed
// passes the stage; unavailable or defects waits on you once (qaTold), and
// after your retry a fresh run starts; pressure waits for the memory hold to
// lift, interrupted starts afresh, each unless qaInARow of it came in a row
// at the head: then QA is unavailable.
func (e *Engine) qaDecide(ctx context.Context, t registry.Task, dir string, run qaRun, res QAResult) (pass, again bool, err error) {
	switch res.Outcome {
	case QAOutcomeClean, QAOutcomeNotNeeded:
		if t.WaitsWho == waitQA {
			e.waitOn(ctx, t, "", "")
		}
		return true, false, nil
	case QAOutcomeUnavailable, QAOutcomeDefects:
		told, err := e.qaTold(ctx, t, res)
		return false, told && err == nil, err
	case QAOutcomePressure:
		if h := e.holds[HoldMemory]; h != nil {
			e.waitOn(ctx, t, HoldMemory, h.Reason)
			return false, false, heldError{who: HoldMemory, what: h.Reason}
		}
		if qaInARowAt(dir, run.Head, QAOutcomePressure) >= qaInARow {
			return false, false, e.qaUnavailable(ctx, t, run, fmt.Sprintf("the Mac's memory stopped QA %d times in a row at this head", qaInARow))
		}
	case QAOutcomeInterrupted:
		if qaInARowAt(dir, run.Head, QAOutcomeInterrupted) >= qaInARow {
			return false, false, e.qaUnavailable(ctx, t, run, fmt.Sprintf("the daemon stopped during %d QA runs in a row at this head", qaInARow))
		}
	}
	return false, true, nil
}

// newQARun writes qa-run.json for the run after last (and after every
// folder under qa/): at bh's head, on copy c's server.
func (e *Engine) newQARun(dir string, last qaRun, bh branchHead, c Copy, q config.FactoryQA) (qaRun, error) {
	k := max(last.N, newestQARun(dir)) + 1
	run := qaRun{N: k, Head: bh.Head, Base: bh.Base, Dir: qaRunDir(k, bh.Head), Port: c.Port, MaxPages: q.MaxPages,
		URL: strings.TrimSuffix(q.PageURL(c.Port, ""), "/"), Ready: q.Ready}
	if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(run.Dir)), 0o700); err != nil {
		return qaRun{}, err
	}
	return run, writeJSON(filepath.Join(dir, qaRunFile), run)
}

// qaUserFacing runs the repository's qa.when in copy c on the run's changed
// files (every status, one per line on its stdin), with the copy's env and
// none of the daemon's secrets, for at most qaWhenCap. A failure cannot
// tell: QA could not run. No file printed needs no QA: the stage passes.
// Else the files go to changed.txt, the probe with a fresh nonce to
// probe.js, and the run records them (pass false, run.Nonce set).
func (e *Engine) qaUserFacing(ctx context.Context, t registry.Task, st config.FactoryStage, c Copy, q config.FactoryQA, run *qaRun) (bool, error) {
	if e.run == nil {
		return false, heldError{who: "factory", what: "this build runs no commands"}
	}
	changes, err := e.d.Git.ChangedFiles(ctx, c.Path, run.Base, run.Head)
	if err != nil {
		return false, err
	}
	var stdin strings.Builder
	for _, ch := range changes {
		stdin.WriteString(ch.Path + "\n")
	}
	res, err := e.run.Run(ctx, execx.Cmd{Name: "mise", Args: miseArgs(c, q.When), Dir: c.Path, Unset: unsetEnv(e.cfg),
		Stdin: []byte(stdin.String()), Timeout: qaWhenCap, Label: "qa.when in " + c.Name})
	if err != nil {
		if ctx.Err() != nil { // the daemon stops: the next one runs it again
			return false, err
		}
		return false, e.qaUnavailable(ctx, t, *run, qaWhenFailed(q.When, err))
	}
	var files []string
	for line := range strings.Lines(string(res.Stdout)) {
		if f := strings.TrimSpace(line); f != "" && !slices.Contains(files, f) {
			files = append(files, f)
		}
	}
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	if len(files) == 0 {
		now := e.now()
		return true, e.passQA(ctx, t, *run, QAResult{Outcome: QAOutcomeNotNeeded, Started: now, Ended: now,
			Why: fmt.Sprintf("the user-facing check `%s` printed none of the %s", q.When, plural(len(changes), "changed file"))})
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return false, err
	}
	n, err := e.attemptsOf(ctx, t, st)
	if err != nil {
		return false, err
	}
	runPath := filepath.Join(dir, filepath.FromSlash(run.Dir))
	if err := os.MkdirAll(runPath, 0o700); err != nil {
		return false, err
	}
	if err := writeFile(filepath.Join(runPath, qaChangedFile), strings.Join(files, "\n")+"\n"); err != nil {
		return false, err
	}
	next := *run
	next.Nonce, next.Changed, next.AttemptsBefore = hex.EncodeToString(nonce), files, n
	if err := fsx.WriteFileAtomic(filepath.Join(runPath, qaProbeFile), qaProbeScript(next.Nonce), 0o600); err != nil {
		return false, err
	}
	if err := writeJSON(filepath.Join(dir, qaRunFile), next); err != nil { // last: a run with its nonce has its files
		return false, err
	}
	*run = next
	e.event(ctx, t.ID, "qa", registry.PhaseInfo, fmt.Sprintf("run %d at %s: %s of the %s are user-facing", run.N, textx.ShortSHA(run.Head),
		plural(len(files), "file"), plural(len(changes), "changed file")))
	return false, nil
}

// qaWhenFailed is why QA could not run when the user-facing check `when`
// failed with err: its exit and the first line of its stderr, masked and
// clipped, or why it did not run to its end.
func qaWhenFailed(when string, err error) string {
	if ee, ok := errors.AsType[*execx.ExitError](err); ok {
		why := fmt.Sprintf("the user-facing check `%s` exited %d", when, ee.Code)
		if line := textx.Clip(textx.FirstLine(execx.Redact(ee.Stderr)), qaWhyRunes); line != "" {
			why += ": " + line
		}
		return why
	}
	return fmt.Sprintf("the user-facing check `%s` did not run to its end: %s", when, textx.Clip(textx.FirstLine(execx.Redact(err.Error())), qaWhyRunes))
}

// qaSession goes on with task t's session of run: none starts one; one that
// boots keeps the task waiting; a failed boot is stopped, and one more boot
// starts once (a panic gets none), else QA is unavailable; a boot that
// answered starts the factory's Chrome; then the session is ready for the
// walk. A session of another run, copy or stage is ended first.
func (e *Engine) qaSession(ctx context.Context, t registry.Task, c Copy, run qaRun) error {
	sess := e.qaSessionOf(t.ID)
	if sess != nil && (sess.run.N != run.N || sess.run.Head != run.Head || sess.copy.Name != c.Name || sess.stage != t.Stage) {
		e.endQASessionLogged(ctx, t, "its run is no longer the task's")
		sess = nil
	}
	switch {
	case sess == nil:
		return e.startQASession(ctx, t, c, run)
	case !sess.ended():
		return nil // the task waits while the server boots
	case sess.err != nil:
		return e.qaBootFailed(ctx, t, c, sess)
	case !sess.browser:
		return e.qaBrowser(ctx, t, sess)
	}
	e.waitOn(ctx, t, waitQA, qaReadyWhat)
	return nil
}

// startQASession starts task t's QA session of run in copy c, in this
// order: no hold of the machine stops launches (mayLaunch), the task's heavy
// slot, the machine's QA lock (held by another: the task waits), the copy's
// port answers nothing yet (else QA is unavailable: another server holds
// it), the copy's server starts, and a goroutine off the loop probes its
// ready path every qaProbeEvery until a 2xx or 3xx or the repository's boot
// runs out; a panic there is the boot's error. Whatever fails or panics
// before the session exists releases what was taken; after, the session's
// teardown does (a start that failed ends it at once, and the same pass
// takes that boot's failure).
func (e *Engine) startQASession(ctx context.Context, t registry.Task, c Copy, run qaRun) error {
	if who, what, ok := e.mayLaunch(); !ok {
		if who != holdRestart {
			e.waitOn(ctx, t, who, what)
		}
		return heldError{who: who, what: what}
	}
	repo, err := e.factoryRepo(t)
	if err != nil {
		return err
	}
	if repo.QA == nil {
		return fmt.Errorf("%s: %s has no [factory.repo.qa]", TaskKey(t), t.Repo)
	}
	boot := repo.QA.Boot.Duration
	release, ok := e.heavy(ctx, t, heavyQA)
	if !ok {
		return nil // the task waits "heavy: …"
	}
	var unlock func()
	var sess *qaSession
	ok = false
	defer func() { // a failure or a panic before the session is the session's own
		switch {
		case ok:
		case sess != nil:
			e.endQASessionLogged(ctx, t, "its start failed")
		default:
			if unlock != nil {
				unlock()
			}
			release()
		}
	}()
	lock := e.d.Layout.FactoryQALock()
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		return err
	}
	u, held, err := engine.AcquireLock(lock)
	if err != nil {
		return fmt.Errorf("the machine's QA lock %s: %w", lock, err)
	}
	if held {
		e.waitOn(ctx, t, waitQA, qaLockWhat)
		return heldError{who: waitQA, what: qaLockWhat}
	}
	unlock = u
	if run.Port <= 0 {
		return e.qaUnavailable(ctx, t, run, c.Name+" has no port: its env names no PORT")
	}
	probe, host := qaProbeURL(run)
	if status, perr := e.probeQA(ctx, probe, host); perr == nil {
		return e.qaUnavailable(ctx, t, run, fmt.Sprintf("port %d already answers before the copy's server started (%s answered %d): "+
			"another server holds it", run.Port, run.Ready, status))
	}
	wc, err := e.qaWalkerCopy(c)
	if err != nil {
		return err
	}
	run.Started = e.now()
	if err := writeJSON(filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), qaRunFile), run); err != nil {
		return err
	}
	bctx, cancel := context.WithTimeout(e.bgCtx, boot+qaBootSlack)
	sess = &qaSession{task: t.ID, stage: t.Stage, run: run, copy: wc, server: true, cancel: cancel, done: make(chan struct{}),
		release: release, unlock: unlock}
	e.qaMu.Lock()
	e.qaSessions[t.ID] = sess
	e.qaMu.Unlock()
	e.event(ctx, t.ID, "qa", registry.PhaseBegin, fmt.Sprintf("run %d at %s: starts %s's server, which has %s to answer %s",
		run.N, textx.ShortSHA(run.Head), c.Name, boot, run.Ready))
	if err := e.startServer(ctx, t, c); err != nil {
		if errors.Is(err, ErrHeld) {
			return err // the deferred teardown ends the session
		}
		sess.err = fmt.Errorf("the copy's server did not start: %s", textx.Clip(textx.FirstLine(execx.Redact(err.Error())), qaWhyRunes))
		cancel()
		close(sess.done)
		ok = true
		return e.qaBootFailed(ctx, t, c, sess)
	}
	e.waitOn(ctx, t, waitQA, fmt.Sprintf("the copy's server boots (up to %s)", boot))
	e.bg.Go(func() {
		defer close(sess.done)
		defer cancel()
		defer func() {
			if p := recover(); p != nil {
				msg := textx.Clip(strings.Join(strings.Fields(execx.Redact(fmt.Sprint(p))), " "), panicRunes)
				e.log.Error("panic in a QA boot's probe; the daemon goes on", "task", t.ID, "panic", msg, "stack", execx.Redact(string(debug.Stack())))
				sess.ready, sess.err = false, qaPanic{msg: msg}
			}
		}()
		sess.ready, sess.err = e.bootQA(bctx, probe, host, run.Ready, boot)
	})
	ok = true
	return nil
}

// bootQA probes the ready path at url (Host host) every qaProbeEvery until
// it answers 2xx or 3xx (true), or boot passed on the engine's clock, or ctx
// ended: the error names the ready path, the boot and the last answer.
func (e *Engine) bootQA(ctx context.Context, url, host, ready string, boot time.Duration) (bool, error) {
	start := e.now()
	var last string // what the last probe met
	for {
		status, err := e.probeQA(ctx, url, host)
		switch {
		case err == nil && status >= 200 && status < 400:
			return true, nil
		case err != nil:
			last = textx.Clip(textx.FirstLine(execx.Redact(err.Error())), qaWhyRunes)
		default:
			last = "it answered " + strconv.Itoa(status)
		}
		failed := fmt.Errorf("the copy's server did not answer %s within %s (last: %s)", ready, boot, last)
		if e.now().Sub(start) >= boot {
			return false, failed
		}
		select {
		case <-ctx.Done():
			return false, failed
		case <-e.timer(qaProbeEvery):
		}
	}
}

// qaBootFailed takes the failed boot of session sess: the session ends (its
// server stops, with its proof), and one more boot starts in copy c unless
// the run had its boot more or the boot's probe panicked: then QA is
// unavailable.
func (e *Engine) qaBootFailed(ctx context.Context, t registry.Task, c Copy, sess *qaSession) error {
	why := sess.err.Error()
	e.endQASessionLogged(ctx, t, "its boot failed")
	run := sess.run
	if _, panicked := errors.AsType[qaPanic](sess.err); panicked || run.BootTries >= qaBootMore {
		return e.qaUnavailable(ctx, t, run, why)
	}
	run.BootTries++
	if err := writeJSON(filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), qaRunFile), run); err != nil {
		return err
	}
	e.event(ctx, t.ID, "qa", registry.PhaseInfo, fmt.Sprintf("run %d: %s; its server starts once more", run.N, why))
	return e.startQASession(ctx, t, c, run)
}

// qaBrowser starts the factory's headless Chrome for session sess in its
// run's folder, once no hold stops launches, and records its CDP endpoint:
// the session is ready for the walk. A browser that does not start is QA
// unavailable.
func (e *Engine) qaBrowser(ctx context.Context, t registry.Task, sess *qaSession) error {
	if who, what, ok := e.mayLaunch(); !ok {
		if who != holdRestart {
			e.waitOn(ctx, t, who, what)
		}
		return heldError{who: who, what: what}
	}
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	sess.browser = true
	cdp, err := e.d.Copies.StartBrowser(ctx, sess.copy, filepath.Join(dir, filepath.FromSlash(sess.run.Dir)))
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return e.qaUnavailable(ctx, t, sess.run, "the factory's Chrome did not start: "+textx.Clip(textx.FirstLine(execx.Redact(err.Error())), qaWhyRunes))
	}
	sess.run.CDP = cdp
	if err := writeJSON(filepath.Join(dir, qaRunFile), sess.run); err != nil {
		return err
	}
	e.event(ctx, t.ID, "qa", registry.PhaseOK, fmt.Sprintf("run %d: the copy's server answers %s and the factory's Chrome runs at %s: %s",
		sess.run.N, sess.run.Ready, cdp, qaReadyWhat))
	e.waitOn(ctx, t, waitQA, qaReadyWhat)
	return nil
}

// qaWalkerCopy is copy c as the QA walker's agent runs in it: the copy's env
// with BROWSER=true and the factory's shims (whose `open` opens nothing)
// first on its PATH, the daemon's after, so the walker opens no tab in your
// browser either.
func (e *Engine) qaWalkerCopy(c Copy) (Copy, error) {
	shims := e.d.Layout.FactoryShims()
	if err := ensureOpenShim(shims); err != nil {
		return c, err
	}
	env := maps.Clone(c.Env)
	if env == nil {
		env = map[string]string{}
	}
	env["BROWSER"] = "true"
	env["PATH"] = shims
	if p := e.getenv("PATH"); p != "" {
		env["PATH"] += ":" + p
	}
	c.Env = env
	return c, nil
}

// qaProbeURL is the ready path of run's server and the Host it is asked
// with.
func qaProbeURL(run qaRun) (probe, host string) {
	probe = run.URL + "/" + strings.TrimPrefix(run.Ready, "/")
	if u, err := url.Parse(probe); err == nil {
		host = u.Host
	}
	return probe, host
}

// probeQA asks the copy's server for raw (Engine.qaProbe, else
// httpQAProbe): its status, or the error of no answer.
func (e *Engine) probeQA(ctx context.Context, raw, host string) (int, error) {
	if e.qaProbe != nil {
		return e.qaProbe(ctx, raw, host)
	}
	return httpQAProbe(ctx, raw, host)
}

// httpQAProbe GETs raw from this Mac whatever its host names: it dials
// 127.0.0.1 on raw's port, then [::1], asks with Host host, follows no
// redirect (a 3xx is an answer), uses no proxy and gives up after
// qaProbeTimeout. A certificate the system does not trust is no answer: the
// factory's Chrome would refuse it too.
func httpQAProbe(ctx context.Context, raw, host string) (int, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return 0, err
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443"}[u.Scheme]
		if port == "" {
			port = "80"
		}
	}
	dialer := &net.Dialer{Timeout: qaProbeTimeout}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			c, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", port))
			if err == nil {
				return c, nil
			}
			if c6, err6 := dialer.DialContext(ctx, "tcp", net.JoinHostPort("::1", port)); err6 == nil {
				return c6, nil
			}
			return nil, err
		},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: qaProbeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return 0, err
	}
	req.Host = host
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, nil
}

// endQASession ends task t's QA session, if it has one, because why, on a
// context of its own (the daemon's shutdown or the tick's end never cuts a
// stop): it cancels the boot's goroutine and waits for it (at most
// qaBootWait), then stops the browser (StopBrowser) and the server
// (stopServer, with its proof; Copies.StopServer for one whose start
// failed), then lets go of the machine's QA lock and the heavy slot. A step
// that fails does not stop the next one. A qa event says how it ended.
func (e *Engine) endQASession(ctx context.Context, t registry.Task, why string) error {
	ctx = context.WithoutCancel(ctx)
	e.qaMu.Lock()
	sess := e.qaSessions[t.ID]
	delete(e.qaSessions, t.ID)
	e.qaMu.Unlock()
	if sess == nil {
		return nil
	}
	sess.cancel()
	wait := time.NewTimer(qaBootWait)
	select {
	case <-sess.done:
	case <-wait.C:
		e.log.Warn("a QA boot's probe did not end with its run; the teardown goes on", "task", t.ID)
	}
	wait.Stop()
	var errs []error
	if sess.browser {
		if err := e.d.Copies.StopBrowser(ctx, sess.copy); err != nil {
			errs = append(errs, fmt.Errorf("stop its browser: %w", err))
		}
	}
	if s, ok := e.servers[t.ID]; ok {
		if err := e.stopServer(ctx, t, s, "its QA run ended: "+why); err != nil {
			errs = append(errs, err)
		}
	} else if sess.server { // a start that failed: its stop proves nothing is left
		if err := e.d.Copies.StopServer(ctx, sess.copy); err != nil {
			if _, held := errors.AsType[portHeld](err); !held {
				errs = append(errs, fmt.Errorf("stop its server: %w", err))
			}
		}
	}
	sess.unlock()
	sess.release()
	what := fmt.Sprintf("run %d at %s ended (%s)", sess.run.N, textx.ShortSHA(sess.run.Head), why)
	if err := errors.Join(errs...); err != nil {
		e.event(ctx, t.ID, "qa", registry.PhaseFail, what+": "+textx.Clip(textx.FirstLine(err.Error()), panicRunes))
		return err
	}
	e.event(ctx, t.ID, "qa", registry.PhaseOK, what+": its browser and its server are stopped, the Mac's QA lock is free")
	return nil
}

// endQASessionLogged is endQASession whose failure the log keeps (its event
// says it too).
func (e *Engine) endQASessionLogged(ctx context.Context, t registry.Task, why string) {
	if err := e.endQASession(ctx, t, why); err != nil {
		e.log.Warn("end a QA session", "task", t.ID, "err", err)
	}
}

// stopQASessions ends every QA session as the daemon stops (RunLocked).
func (e *Engine) stopQASessions(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	e.qaMu.Lock()
	ids := slices.Sorted(maps.Keys(e.qaSessions))
	e.qaMu.Unlock()
	for _, id := range ids {
		t, err := e.reg.TaskByID(ctx, id)
		if err != nil {
			t = registry.Task{ID: id}
		}
		e.endQASessionLogged(ctx, t, "the daemon stops")
	}
}

// sweepQA ends the QA sessions of the tasks that are not at their run: gone,
// not live (a request stopped them, they wait on you), or at another stage
// than the session's (sent back).
func (e *Engine) sweepQA(ctx context.Context, tasks []registry.Task) {
	e.qaMu.Lock()
	sessions := maps.Clone(e.qaSessions)
	e.qaMu.Unlock()
	for _, id := range slices.Sorted(maps.Keys(sessions)) {
		i := slices.IndexFunc(tasks, func(t registry.Task) bool { return t.ID == id })
		switch {
		case i < 0:
			e.endQASessionLogged(ctx, registry.Task{ID: id}, "its task is gone")
		case !isLive(tasks[i]):
			e.endQASessionLogged(ctx, tasks[i], "its task is not at work")
		case tasks[i].Stage != sessions[id].stage:
			e.endQASessionLogged(ctx, tasks[i], "its task left the "+sessions[id].stage+" stage")
		}
	}
}

// cutQA is startup's record of the QA runs a stopped daemon cut: each task
// at work or waiting on you, at a qa stage, whose newest run started its
// server and has no result gets the outcome interrupted (its server and its
// browser were stopped by Copies.Reconcile before), its wait on the run
// ends, and the stage's open attempts the run began (after AttemptsBefore)
// fail. A run that never started its server goes on.
func (e *Engine) cutQA(ctx context.Context, tasks []registry.Task) {
	for _, t := range tasks {
		if !isLive(t) && t.State != registry.TaskNeedsYou {
			continue
		}
		st, ok := e.stageOf(t, t.Stage)
		if !ok || st.Type != config.FactoryStageQA {
			continue
		}
		dir := e.d.Layout.FactoryTaskDir(t.ID)
		run, ok := readQARun(dir)
		if !ok || run.Started.IsZero() {
			continue
		}
		if _, ended := readQAResult(dir, run); ended {
			continue
		}
		res := QAResult{Outcome: QAOutcomeInterrupted, Why: "the daemon stopped during the run: its server and browser were stopped",
			Started: run.Started, Ended: e.now()}
		if err := e.writeQAResult(ctx, t, run, res); err != nil {
			e.log.Warn("record a QA run a restart cut", "task", t.ID, "err", err)
			continue
		}
		if t.WaitsWho == waitQA { // "ready for the walk" no more
			e.waitOn(ctx, t, "", "")
		}
		as, err := e.reg.Attempts(ctx, t.ID)
		if err != nil {
			e.log.Warn("read the attempts of a QA run a restart cut", "task", t.ID, "err", err)
			continue
		}
		for _, a := range as {
			if a.Stage != st.Name || a.N <= run.AttemptsBefore {
				continue
			}
			switch a.State {
			case registry.AttemptLaunching, registry.AttemptRunning, registry.AttemptEnded:
				if err := e.reg.TransitionAttempt(ctx, a.ID, []string{a.State}, registry.AttemptFailed, func(u *registry.AttemptUpdate) {
					u.Set("error", "the daemon stopped during its QA run")
				}); err != nil {
					e.log.Warn("end the attempt of a QA run a restart cut", "attempt", a.ID, "err", err)
				}
			}
		}
	}
}

// qaUnavailable ends run as QA that could not run, because why: its
// result.json and a failed qa check (the environment's), the session's
// teardown, then task t needs you with why and the retry command. A daemon
// that stops before the move (beforeStop, a test's seam) leaves the result
// for the next pass, which moves the task then (qaTold).
func (e *Engine) qaUnavailable(ctx context.Context, t registry.Task, run qaRun, why string) error {
	now := e.now()
	started := run.Started
	if started.IsZero() {
		started = now
	}
	err := e.writeQAResult(ctx, t, run, QAResult{Outcome: QAOutcomeUnavailable, Why: why, Started: started, Ended: now})
	e.endQASessionLogged(ctx, t, "QA could not run")
	if err != nil {
		return err
	}
	if e.beforeStop != nil {
		if err := e.beforeStop(); err != nil {
			return err
		}
	}
	e.toOperator(ctx, t, qaUnavailableWhat(t, why), "QA could not run")
	return nil
}

// qaUnavailableWhat is task t's wait on you when QA could not run, because
// why.
func qaUnavailableWhat(t registry.Task, why string) string {
	return "QA could not run for a user-facing change: " + why + retryHint(t)
}

// qaTold reports whether you were told of result res (an alert since it
// ended): a live task told so is one you let go on, and a fresh run starts.
// One not told is moved to you now, with the result's reason.
func (e *Engine) qaTold(ctx context.Context, t registry.Task, res QAResult) (bool, error) {
	evs, err := e.reg.Events(ctx, t.ID, qaEventsRead)
	if err != nil {
		return false, err
	}
	if slices.ContainsFunc(evs, func(ev registry.Event) bool { return ev.Kind == "alert" && !ev.At.Before(res.Ended) }) {
		return true, nil
	}
	what := qaUnavailableWhat(t, res.Why)
	if res.Outcome != QAOutcomeUnavailable {
		what = fmt.Sprintf("QA run %d ended %s: %s%s", res.Run, res.Outcome, res.Why, retryHint(t))
	}
	e.toOperator(ctx, t, what, "QA stopped for you")
	return false, nil
}

// qaResultOf is res as run's result: its version, number, folder and head,
// and its end (now when unset) and start (its end when unset).
func (e *Engine) qaResultOf(run qaRun, res QAResult) QAResult {
	res.Version, res.Run, res.Dir, res.Head = qaResultVersion, run.N, run.Dir, run.Head
	if res.Ended.IsZero() {
		res.Ended = e.now()
	}
	if res.Started.IsZero() {
		res.Started = res.Ended
	}
	return res
}

// writeQAResult writes res as run's result.json and records its qa check at
// the run's head: clean passes, not_needed is skipped, defects fail (the
// change's) and unavailable fails (the environment's); interrupted and
// pressure decide nothing and record none. A qa event says how the run
// ended.
func (e *Engine) writeQAResult(ctx context.Context, t registry.Task, run qaRun, res QAResult) error {
	res = e.qaResultOf(run, res)
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	if err := writeJSON(filepath.Join(dir, filepath.FromSlash(run.Dir), qaResultFile), res); err != nil {
		return err
	}
	result, cause, phase := "", "", registry.PhaseInfo
	switch res.Outcome {
	case QAOutcomeClean:
		result, phase = registry.CheckPass, registry.PhaseOK
	case QAOutcomeNotNeeded:
		result, phase = registry.CheckSkipped, registry.PhaseOK
	case QAOutcomeDefects:
		result, cause, phase = registry.CheckFail, "code", registry.PhaseFail
	case QAOutcomeUnavailable:
		result, cause, phase = registry.CheckFail, "env", registry.PhaseFail
	}
	if result != "" {
		e.recordCheck(ctx, registry.Check{TaskID: t.ID, HeadSHA: run.Head, Kind: registry.CheckQA, Result: result, Cause: cause,
			Source: "factory", Command: "qa " + run.Dir, Took: res.Ended.Sub(res.Started)})
	}
	e.event(ctx, t.ID, "qa", phase, fmt.Sprintf("run %d at %s ended %s: %s", run.N, textx.ShortSHA(run.Head), res.Outcome, res.Why))
	return nil
}

// passQA passes the qa stage on run's result res: qa-summary.json first,
// then the run's result.json, so a result that passed always has its
// summary; the task's wait on QA ends.
func (e *Engine) passQA(ctx context.Context, t registry.Task, run qaRun, res QAResult) error {
	res = e.qaResultOf(run, res)
	if err := writeJSON(filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), qaSummaryFile), res); err != nil {
		return err
	}
	if err := e.writeQAResult(ctx, t, run, res); err != nil {
		return err
	}
	if t.WaitsWho == waitQA {
		e.waitOn(ctx, t, "", "")
	}
	return nil
}

// readQARun reads qa-run.json in the task directory dir: false for none, or
// for one whose folder is not its number's at its head.
func readQARun(dir string) (qaRun, bool) {
	var run qaRun
	b, err := os.ReadFile(filepath.Join(dir, qaRunFile))
	if err != nil || json.Unmarshal(b, &run) != nil || run.N < 1 || run.Head == "" || run.Dir != qaRunDir(run.N, run.Head) {
		return qaRun{}, false
	}
	return run, true
}

// readQASummary reads qa-summary.json in the task directory dir, the QA
// result that passed the stage; false for none.
func readQASummary(dir string) (QAResult, bool) {
	var res QAResult
	b, err := os.ReadFile(filepath.Join(dir, qaSummaryFile))
	if err != nil || json.Unmarshal(b, &res) != nil || res.Outcome == "" {
		return QAResult{}, false
	}
	return res, true
}

// readQAResult reads run's result.json in the task directory dir; false for
// none (the run goes on).
func readQAResult(dir string, run qaRun) (QAResult, bool) {
	var res QAResult
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(run.Dir), qaResultFile))
	if err != nil || json.Unmarshal(b, &res) != nil || res.Outcome == "" {
		return QAResult{}, false
	}
	return res, true
}

// qaRunsAt are the numbers of the runs at head in the task directory dir,
// newest first.
func qaRunsAt(dir, head string) []int {
	var ns []int
	for _, k := range qaRunNumbers(dir) {
		if fsx.Exists(filepath.Join(dir, filepath.FromSlash(qaRunDir(k, head)))) {
			ns = append(ns, k)
		}
	}
	slices.Reverse(ns)
	return ns
}

// qaInARowAt is how many of the newest runs at head in a row ended outcome.
func qaInARowAt(dir, head, outcome string) int {
	n := 0
	for _, k := range qaRunsAt(dir, head) {
		res, ok := readQAResult(dir, qaRun{Dir: qaRunDir(k, head)})
		if !ok || res.Outcome != outcome || res.Head != head {
			break
		}
		n++
	}
	return n
}

// qaRunNumbers are the numbers of the run folders under dir/qa, ascending.
func qaRunNumbers(dir string) []int {
	entries, _ := os.ReadDir(filepath.Join(dir, "qa"))
	var ns []int
	for _, en := range entries {
		if m := qaRunDirRe.FindStringSubmatch(en.Name()); m != nil && en.IsDir() {
			if k, err := strconv.Atoi(m[1]); err == nil {
				ns = append(ns, k)
			}
		}
	}
	slices.Sort(ns)
	return slices.Compact(ns)
}

// newestQARun is the highest number of a run folder under dir/qa, 0 for
// none.
func newestQARun(dir string) int {
	if ns := qaRunNumbers(dir); len(ns) > 0 {
		return ns[len(ns)-1]
	}
	return 0
}

// nextStage is the stage after stage i of kind for task t, and the stages
// it skips on the way: a qa stage of a repository without
// [factory.repo.qa]. "" when every stage after i is skipped.
func (e *Engine) nextStage(t registry.Task, kind config.FactoryKind, i int) (name string, skipped []string) {
	repo, known := e.cfg.FactoryRepo(t.Repo)
	for _, st := range kind.Stages[i+1:] {
		if st.Type == config.FactoryStageQA && known && repo.QA == nil {
			skipped = append(skipped, st.Name)
			continue
		}
		return st.Name, skipped
	}
	return "", skipped
}
