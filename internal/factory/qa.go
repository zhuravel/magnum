package factory

// A qa stage's run (R55 slice 1, steps 4 and 5). A run belongs to the
// task's head: qa-run.json in the task's directory is the task's newest
// run, and each run has its folder qa/<n>-<sha7> (qaRunDir), which holds the
// probe, the user-facing files, what the walker saves and the run's
// result.json. runQA passes the stage on a run whose result is clean or
// not_needed at the head, hands a run's defects that block to the writer
// (the fix loop, qa_fix.go), sends the task to you once on a run that could
// not run (your retry starts a fresh one), and otherwise goes on with the
// newest run or starts one: the repository's qa.when reads the changed
// files and prints the user-facing ones (none: no QA is needed; a failing
// command cannot tell, so QA could not run, never "nothing user-facing"),
// then the session starts, and once it is ready the fx-qa walker's turn
// walks the pages through the session's Chrome alone (runTurn, end fact qa:
// a valid verdict.json). Its end stops the session first; then the
// factory's own checks of the saved pages decide (evaluateQA), never the
// walker's word.
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
// context of its own and under its own recover. A session is work a restart
// would cut (WorkQA in work.json), from its server's start to its end, and a
// drain ends one whose walk has not begun at once. A run the factory cut
// itself (a drain, a stop signal, its task leaving it) is recorded
// interrupted by that cut and starts afresh, uncounted; a crash's (cutQA at
// the next start) counts, and two in a row at a head make QA unavailable.

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
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
	qaVerdictFile = "verdict.json"
)

// heavyQA is a QA run's use of the task's heavy slot, waitQA the task's
// waits on its run (qaLockWhat, the boot, qaReadyWhat).
const (
	heavyQA     = "qa"
	waitQA      = "qa"
	qaLockWhat  = "another QA runs on this Mac"
	qaReadyWhat = "ready for the walk"
)

// The env of the walker's copy attaches its browser tool to the session's
// Chrome as the prompt's flags do (agent-browser reads these as its --cdp
// and --session), so a command that leaves the flags out still reaches the
// factory's Chrome, never a browser the tool would start itself or yours;
// the tool's daemon of the run's session ends once idle for qaBrowserIdle.
const (
	qaEnvCDP      = "AGENT_BROWSER_CDP"
	qaEnvSession  = "AGENT_BROWSER_SESSION"
	qaEnvIdle     = "AGENT_BROWSER_IDLE_TIMEOUT_MS"
	qaBrowserIdle = 10 * time.Minute
	// qaListMost is how many blocked states a wait on you names.
	qaListMost = 3
)

// qaBrowserSession is the walker's own session of its browser tool in run n
// of task id: one per run, so it never shares the tool's default session
// (a browser every agent on the Mac shares) or an earlier run's.
func qaBrowserSession(id int64, n int) string { return fmt.Sprintf("magnum-qa-%d-%d", id, n) }

const (
	// qaProbeEvery is how often the boot probes the ready path, qaProbeTimeout
	// how long one probe may take; qaWhenCap bounds the user-facing check.
	qaProbeEvery   = 2 * time.Second
	qaProbeTimeout = 5 * time.Second
	qaWhenCap      = 30 * time.Second // it runs on the loop: well within launchd's 45 s ExitTimeOut
	// qaBootSlack is the boot's bound on the wall clock past its own (the
	// boot counts on the engine's clock), qaBootWait how long a teardown waits
	// for the boot's goroutine to end after it cancelled it.
	qaBootSlack = time.Minute
	qaBootWait  = 15 * time.Second
	// qaInARow is how many results of one kind in a row at a head
	// (interrupted by a crash, pressure) a fresh run follows: then QA is
	// unavailable.
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
// its end), when its server last started (zero before, and after a
// teardown with no cut: a restart cut nothing of a run with no server), and
// when it first started (Began: a boot more or a teardown keeps it, so the
// run's cap and its result's time count from it).
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
	Began          time.Time `json:"began,omitzero"`
}

// began is when run's server first started: Began, else Started (a run of
// an earlier build), zero before any start.
func (run qaRun) began() time.Time {
	if !run.Began.IsZero() {
		return run.Began
	}
	return run.Started
}

// QAResult is how a QA run ended (result.json in its folder; qa-summary.json
// in the task's directory is the one that passed the stage): its version,
// the run's number, folder and head, the pages it walked and the shots it
// took, its outcome (QAOutcome*) and why, what cut an interrupted run
// (qaCutDrain, qaCutTask, qaCutCrash), whether the walker's verdict decided
// it (Walked: the factory's checks of the pages it saved), the defects that
// block and those that do not (Known), the defects that would block but you
// accepted at the run's head (Accepted, `magnum factory retry --accept-qa`),
// the states the walker could not reach, and when it started and ended.
type QAResult struct {
	Version  int         `json:"version"`
	Run      int         `json:"run"`
	Pages    int         `json:"pages"`
	Shots    int         `json:"shots"`
	Dir      string      `json:"dir"`
	Head     string      `json:"head"`
	Outcome  string      `json:"outcome"`
	Why      string      `json:"why"`
	Cut      string      `json:"cut,omitempty"`
	Walked   bool        `json:"walked,omitempty"`
	Blocking []QADefect  `json:"blocking"`
	Known    []QADefect  `json:"known"`
	Accepted []QADefect  `json:"accepted,omitempty"`
	Blocked  []QABlocked `json:"blocked"`
	Started  time.Time   `json:"started"`
	Ended    time.Time   `json:"ended"`
}

// What cut an interrupted run (QAResult.Cut): the daemon's own stop (a
// drain, a stop signal), its task leaving the run (NEEDS YOU, a stop or a
// drop, another stage), or a daemon that died without its teardown (cutQA).
// Only a crash counts towards qaInARow.
const (
	qaCutDrain = "drain"
	qaCutTask  = "task"
	qaCutCrash = "crash"
)

// qaSession is a QA run's hold on the Mac: its task and stage, the run, the
// walker's copy (qaWalkerCopy, its browser tool attached to the session's
// Chrome once it runs), the run's cap (from its server's start, the boot
// included), whether the boot answered (ready), whether
// the server and the browser were started (server, browser: their stop runs
// whatever came of the start), whether the boot's goroutine started
// (booting: only then does a teardown wait for its done), the boot's error,
// its goroutine's cancel and done (ready and err are written before done
// closes), and the release of the heavy slot, of the machine's QA lock and
// of its entry in the work record. The loop owns it but for ready and err;
// qaMu guards the map of them.
type qaSession struct {
	task                            int64
	stage                           string
	run                             qaRun
	copy                            Copy
	runCap                          time.Duration
	ready, server, browser, booting bool
	err                             error
	cancel                          context.CancelFunc
	done                            chan struct{}
	release, unlock, endWork        func()
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
// it is. The newest run's defects that block go on in their fix loop first
// (qaFixPhase), before the branch's checks: the writer's turn commits and
// leaves changes in the copy as it works. Else the task's branch must be at
// its checked head (a move goes back to coding, headMoved). The newest
// run's result at the head decides (qaDecide); a run with none goes on, and
// a fresh run starts after a result that asks for one or at another head.
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
	last, ok := readQARun(dir)
	if ok {
		if res, ended := readQAResult(dir, last); ended && res.Outcome == QAOutcomeDefects {
			if held, err := e.qaFixPhase(ctx, t, st, last, res); err != nil || held {
				return false, err
			}
		}
	}
	bh, err := e.branchHead(ctx, t, c, repo)
	switch {
	case err != nil:
		return false, err
	case bh.Problem != "":
		return false, e.stageFailed(ctx, t, needsYou{"QA cannot start: " + bh.Problem})
	case t.HeadSHA != "" && bh.Head != t.HeadSHA: // QA looks at a checked head only
		e.endQALogged(ctx, t, "the branch moved", qaCutTask)
		return false, e.headMoved(ctx, t, bh.Head)
	}
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
	if (fresh || run.Nonce == "") && e.startsHeld() { // nothing new runs while the daemon drains
		return false, heldError{who: holdRestart, what: drainWhat}
	}
	if fresh {
		e.endQALogged(ctx, t, "a fresh run starts", qaCutTask) // a session of an older run
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
	switch done, err := e.walkDone(ctx, t, st, run); {
	case err != nil:
		return false, err
	case done: // a pass whose end did not land (its result failed to write)
		return e.endWalk(ctx, t, st, run)
	}
	sess, err := e.qaSession(ctx, t, c, run)
	if err != nil || sess == nil || e.qaBounded(ctx, t, sess) {
		return false, err
	}
	if t.WaitsWho == waitQA { // the walker is at work: the task waits on nothing
		e.waitOn(ctx, t, "", "")
		t.WaitsWho, t.WaitsWhat = "", ""
	}
	done, err := e.runTurn(ctx, t, st, agentsTurn{qa: &qaWalk{run: sess.run, copy: sess.copy}})
	if err != nil || !done {
		return false, err
	}
	return e.endWalk(ctx, t, st, sess.run)
}

// qaWalk is a qa stage's walker turn (agentsTurn.qa): its run, as the
// session that is ready for it has it (its CDP endpoint), and the session's
// copy, which the walker launches in (qaWalkerCopy), never the task's
// claimed copy.
type qaWalk struct {
	run  qaRun
	copy Copy
}

// walkDone reports whether run's walk passed: an attempt of qa stage st
// after the run's attempts_before is verified (the walker's end fact held).
func (e *Engine) walkDone(ctx context.Context, t registry.Task, st config.FactoryStage, run qaRun) (bool, error) {
	as, err := e.reg.Attempts(ctx, t.ID)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(as, func(a registry.Attempt) bool {
		return a.Stage == st.Name && a.N > run.AttemptsBefore && a.State == registry.AttemptVerified
	}), nil
}

// endWalk ends run once its walker's turn passed: the session's teardown
// first (the browser and the server stop, the Mac's QA lock and the heavy
// slot go), then the factory's checks of what the walker saved
// (evaluateQA), and the run's result with its qa check. Clean passes the
// stage (qa-summary.json first); defects that block go to the writer
// (qaFixPhase); a walk the checks cannot call clean or defects (no page
// reached, a state not reached, a verdict that no longer reads) waits on
// you.
func (e *Engine) endWalk(ctx context.Context, t registry.Task, st config.FactoryStage, run qaRun) (bool, error) {
	e.endQALogged(ctx, t, "its walk ended", "")
	res, err := e.evaluateQA(ctx, t, run)
	if err != nil {
		return false, err
	}
	res = e.qaResultOf(run, res)
	if res.Outcome == QAOutcomeClean {
		return true, e.passQA(ctx, t, run, res)
	}
	if err := e.writeQAResult(ctx, t, run, res); err != nil {
		return false, err
	}
	if res.Outcome == QAOutcomeDefects {
		_, err := e.qaFixPhase(ctx, t, st, run, res)
		return false, err
	}
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	if n := qaRowAt(dir, run.Head, qaBlockedOnly); qaBlockedOnly(res) && n >= qaInARow {
		return true, e.passBlocked(ctx, t, run, res, n)
	}
	e.toOperator(ctx, t, qaYouWhat(t, dir, res), "QA could not judge the change")
	return false, nil
}

// qaBlockedOnly reports whether res is a walk that reached its pages and
// found nothing that blocks there, but could not reach every state.
func qaBlockedOnly(res QAResult) bool {
	return res.Outcome == QAOutcomeUnavailable && res.Walked && res.Pages > 0 && len(res.Blocking) == 0 && len(res.Blocked) > 0
}

// passBlocked passes the qa stage on run's result res, the n-th walk in a
// row at its head that reached its pages, found nothing that blocks there
// and could not reach the same kind of states (qaBlockedOnly; the first sent
// the task to you, and your retry walked once more): qa-summary.json alone,
// clean with those states, which go on flagged on the card and in the PR
// body. The run's result.json and its failed check stay as the walk ended,
// and no check passes: QA did not judge those states.
func (e *Engine) passBlocked(ctx context.Context, t registry.Task, run qaRun, res QAResult, n int) error {
	sum := res
	sum.Outcome = QAOutcomeClean
	sum.Why = fmt.Sprintf("the walker reached %s but not %s, in %d walks in a row at this head: they go on flagged on the card and "+
		"in the PR body (%s)", plural(res.Pages, "page"), plural(len(res.Blocked), "state"), n, qaBlockedList(res.Blocked))
	if err := writeJSON(filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), qaSummaryFile), sum); err != nil {
		return err
	}
	e.event(ctx, t.ID, "qa", registry.PhaseOK, fmt.Sprintf("run %d at %s: QA goes on with %s not reached, flagged, after %d walks in a row "+
		"that reached the pages but not them", run.N, textx.ShortSHA(run.Head), plural(len(res.Blocked), "state"), n))
	if t.WaitsWho == waitQA {
		e.waitOn(ctx, t, "", "")
	}
	return nil
}

// evaluateQA decides run from its walker's verdict and the factory's own
// checks of every page the walker saved (detectQA on each probe and console
// log, then qaOutcome): defects when one blocks; unavailable when the walk
// reached no page, or reached pages but not every state it was to (a state
// it could not reach is a page no check judged, so the run is not clean, and
// a writer's turn cannot make a sign-in work: you look); else clean. A
// verdict the end fact took that no longer reads is unavailable too. The
// blocked states' words are the walker's, clipped and masked; a qa event
// counts what the checks found.
func (e *Engine) evaluateQA(ctx context.Context, t registry.Task, run qaRun) (QAResult, error) {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	res := QAResult{Walked: true, Started: run.began(), Ended: e.now()}
	checks, p := qaAcceptanceChecks(dir)
	var v QAVerdict
	if p == nil {
		v, p = readQAVerdict(dir, run.Dir, time.Time{}, qaWantOf(run, checks))
	}
	if p != nil {
		res.Outcome, res.Why = QAOutcomeUnavailable, "QA could not read the walker's verdict: "+p.String()
		return res, nil
	}
	runPath := filepath.Join(dir, filepath.FromSlash(run.Dir))
	// The defects' fingerprints follow each page by the path its probe read,
	// the factory's own spelling, which stays the same across runs whatever
	// the walker calls the page (a trailing slash, a query, no leading
	// slash): a defect that comes back after its fix is the same defect. The
	// walker's paths name the pages everywhere else.
	followed := v
	followed.Pages = slices.Clone(v.Pages)
	var found []QADefect
	for i, pg := range v.Pages {
		probe, err := readQAProbe(runPath, pg.Probe)
		if err != nil {
			return QAResult{}, fmt.Errorf("QA run %d: probe %s", run.N, qaFileWhy(pg.Probe, err))
		}
		console, err := readQAConsole(runPath, pg.Console)
		if err != nil {
			return QAResult{}, fmt.Errorf("QA run %d: console %s", run.N, qaFileWhy(pg.Console, err))
		}
		followed.Pages[i].Path = cmp.Or(qaProbePath(probe.URL), pg.Path)
		found = append(found, detectQA(followed.Pages[i], probe, console)...)
	}
	shots, err := countQAShots(runPath)
	if err != nil {
		return QAResult{}, fmt.Errorf("QA run %d: its folder: %w", run.N, err)
	}
	res.Outcome, res.Blocking, res.Known = qaOutcome(followed, found)
	qaApplyAccepted(dir, run, &res) // what you accepted at this head blocks no more
	res.Pages, res.Shots = len(v.Pages), shots
	for _, b := range v.Blocked {
		res.Blocked = append(res.Blocked, QABlocked{What: qaClip(b.What), Why: qaClip(b.Why), Check: qaClip(b.Check)})
	}
	switch {
	case res.Outcome == QAOutcomeUnavailable:
		why := "the walker's verdict lists no page and no state it could not reach"
		if len(res.Blocked) > 0 {
			why = cmp.Or(res.Blocked[0].Why, res.Blocked[0].What)
		}
		res.Why = "QA reached no page: " + why
	case res.Outcome == QAOutcomeDefects:
		res.Why = qaClassesAndPaths(res.Blocking, v.Pages)
	case len(res.Blocked) > 0:
		shows := "shows"
		if res.Pages > 1 {
			shows = "show"
		}
		res.Outcome = QAOutcomeUnavailable
		res.Why = fmt.Sprintf("QA could not reach %s of the change: %s; the %s it reached %s nothing that blocks",
			plural(len(res.Blocked), "state"), qaBlockedList(res.Blocked), plural(res.Pages, "page"), shows)
	case len(res.Accepted) > 0:
		res.Why = fmt.Sprintf("%s walked, nothing blocks (%s; %d accepted by you)", plural(res.Pages, "page"), plural(len(res.Known), "note"),
			len(res.Accepted))
	default:
		res.Why = fmt.Sprintf("%s walked, nothing blocks (%s)", plural(res.Pages, "page"), plural(len(res.Known), "note"))
	}
	e.event(ctx, t.ID, "qa", registry.PhaseInfo, fmt.Sprintf("run %d: the factory's checks of %s and %s found %s that block%s, %s; %s not reached",
		run.N, plural(res.Pages, "page"), plural(res.Shots, "shot"), plural(len(res.Blocking), "defect"), blocksS(len(res.Blocking)),
		plural(len(res.Known), "note"), plural(len(res.Blocked), "state")))
	return res, nil
}

// qaProbePath is the path of the page a probe read, from its URL, cleaned
// ("/settings/security/" is "/settings/security"), its query and anchor left
// out, and a route kept in its hash ("#/settings", "#!/orders") kept, so a
// page of a hash-routed app is no "/" of every other; "" for a URL that does
// not parse. Following a defect by it is best-effort: an id in the path
// ("/orders/17", then "/orders/18") or a redirect loses the "came back
// after its fix" label, which steers the writer only, as the cap counts the
// defects files whatever the labels say.
func qaProbePath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	p := path.Clean("/" + u.Path)
	if strings.HasPrefix(u.Fragment, "/") || strings.HasPrefix(u.Fragment, "!/") {
		p += "#" + u.Fragment
	}
	return p
}

// blocksS is the verb's s after n things that block ("1 defect that
// blocks", "2 defects that block").
func blocksS(n int) string {
	if n == 1 {
		return "s"
	}
	return ""
}

// qaWantOf is what run's verdict is checked against: the run's head, nonce,
// port and page cap, and the acceptance checks' ids.
func qaWantOf(run qaRun, checks []string) qaWant {
	return qaWant{Head: run.Head, Nonce: run.Nonce, Port: run.Port, MaxPages: run.MaxPages, Checks: checks}
}

// qaAcceptanceChecks are the ids of the checks of acceptance.json in the
// task directory dir, which a QA verdict covers: none when the file is not
// there (nothing to cover). One that does not read is the operator's
// problem: no walker's turn fixes the judge's file.
func qaAcceptanceChecks(dir string) ([]string, *endProblem) {
	b, err := os.ReadFile(filepath.Join(dir, acceptanceFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, &endProblem{file: acceptanceFile, why: acceptanceFile + " cannot be read, so QA's checks are unknown: " + err.Error(),
			operator: true}
	}
	var a AcceptanceFile
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, &endProblem{file: acceptanceFile, why: acceptanceFile + " is not valid JSON, so QA's checks are unknown: " + err.Error(),
			operator: true}
	}
	ids := make([]string, 0, len(a.Checks))
	for _, c := range a.Checks {
		if c.ID != "" {
			ids = append(ids, c.ID)
		}
	}
	return ids, nil
}

// checkQAEnd is the qa end fact in the task directory dir for a walker's
// turn that started at since: the newest run's verdict.json (qa-run.json
// names the run), newer than since, valid for the run (readQAVerdict: its
// head, its nonce, the copy's port, the page cap) and covering every check
// of acceptance.json. A task with no run ready for a walk is the factory's
// problem, never the walker's.
func checkQAEnd(dir string, since time.Time) *endProblem {
	run, ok := readQARun(dir)
	if !ok || run.Nonce == "" {
		return &endProblem{file: qaRunFile, why: qaRunFile + " names no QA run ready for its walk", operator: true}
	}
	checks, p := qaAcceptanceChecks(dir)
	if p != nil {
		return p
	}
	_, p = readQAVerdict(dir, run.Dir, since, qaWantOf(run, checks))
	return p
}

// qaConsoleCap bounds the console log of a page the factory reads.
const qaConsoleCap = 1 << 20

// readQAConsole reads the console log at rel in the run's folder runPath,
// one message per line, at most qaConsoleCap bytes of it.
func readQAConsole(runPath, rel string) ([]string, error) {
	f, err := openInRun(runPath, rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, qaConsoleCap))
	if err != nil {
		return nil, err
	}
	return strings.Split(string(b), "\n"), nil
}

// qaClassesAndPaths names defects ds by class and by the path of their page
// (pages are the verdict's), each pair once, in order, at most qaListMost of
// them and how many more: "escaped_markup on /settings/security, error_page
// on /cart". No title and no quote.
func qaClassesAndPaths(ds []QADefect, pages []QAPage) string {
	paths := map[string]string{}
	for _, pg := range pages {
		paths[pg.ID] = pg.Path
	}
	var out []string
	for _, d := range ds {
		if s := d.Class + " on " + qaClip(paths[d.Page]); !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	if n := len(out) - qaListMost; n > 0 {
		out = append(out[:qaListMost], fmt.Sprintf("and %d more", n))
	}
	return strings.Join(out, ", ")
}

// qaBlockedList names the states bs the walker could not reach, each with
// why, at most qaListMost of them: "the passkey autofill list (headless
// Chrome draws no autofill)".
func qaBlockedList(bs []QABlocked) string {
	var out []string
	for _, b := range bs[:min(len(bs), qaListMost)] {
		s := cmp.Or(b.What, "a state")
		if b.Why != "" {
			s += " (" + b.Why + ")"
		}
		out = append(out, s)
	}
	if n := len(bs) - qaListMost; n > 0 {
		out = append(out, fmt.Sprintf("and %d more", n))
	}
	return strings.Join(out, "; ")
}

// qaYouWhat is what task t waits on you for after its QA run ended with res
// (in the task directory dir): the walk's own reason (no page reached, a
// state not reached, a verdict that no longer reads), with the result's
// file, or why QA could not run; then the retry command, and for a walk
// that reached its pages but not every state, what the retry does (it
// walks once more, and a second such walk goes on with the states flagged).
// It names no page text. Defects that block come here only when their fix
// loop let them go with no stop (qaDecide's safety net): they go to the
// writer (qaFixPhase).
func qaYouWhat(t registry.Task, dir string, res QAResult) string {
	see := "; see " + filepath.Join(dir, filepath.FromSlash(res.Dir), qaResultFile)
	switch {
	case res.Outcome == QAOutcomeDefects:
		n := len(res.Blocking)
		return fmt.Sprintf("QA found %s that block%s: %s%s%s", plural(n, "defect"), blocksS(n), res.Why, see, retryHint(t))
	case qaBlockedOnly(res):
		return fmt.Sprintf("%s%s; then `magnum factory retry %d` walks once more, and a second walk that again reaches the pages but "+
			"not every state goes on with those states flagged on the card and in the PR body", res.Why, see, t.ID)
	case res.Outcome == QAOutcomeUnavailable && res.Walked:
		return res.Why + see + retryHint(t)
	case res.Outcome == QAOutcomeUnavailable:
		return qaUnavailableWhat(t, res.Why)
	}
	return fmt.Sprintf("QA run %d ended %s: %s%s", res.Run, res.Outcome, res.Why, retryHint(t))
}

// qaStage is the qa stage of task t's kind.
func (e *Engine) qaStage(t registry.Task) (config.FactoryStage, bool) {
	for _, st := range e.cfg.Factory.Kinds[t.Kind].Stages {
		if st.Type == config.FactoryStageQA {
			return st, true
		}
	}
	return config.FactoryStage{}, false
}

// qaBack sends task t back to its kind's qa stage when QA is what keeps its
// card (why, readyProblem's, is qaProblem's), as a moved head goes back to
// coding: a task that went past its qa stage before its repository had
// [factory.repo.qa], or whose QA looked at another head, runs QA at its
// head, then the stages after it, the PR body and the card among them. It
// reports whether it did. A retry would only meet the same problem again.
func (e *Engine) qaBack(ctx context.Context, t registry.Task, why string) (bool, error) {
	st, ok := e.qaStage(t)
	if why == "" || !ok || why != e.qaProblem(t) {
		return false, nil
	}
	because := why + ": QA runs at the task's head"
	if err := e.reg.TransitionTask(ctx, t.ID, liveStates, registry.TaskActive, func(u *registry.TaskUpdate) {
		u.Set("stage", st.Name)
		u.Set("attempt", 0)
		u.Set("waits_who", "")
		u.Set("waits_what", "")
		u.Because(because)
	}); err != nil {
		return false, err
	}
	e.event(ctx, t.ID, "ready", registry.PhaseInfo, "back to "+st.Name+": "+because)
	return true, nil
}

// qaBounded ends task t's session sess when it may run no longer, and
// reports whether it did: past its cap since its server first started (its
// boots and the walk share it: QA is unavailable, and the task needs you), or
// while the memory hold stands at critical pressure (a pressure result: the
// session stops, and a fresh run starts once the hold lifts, qaDecide).
// Every tick's sweep asks it, so a run stays bounded while a hold keeps the
// stages from running; the walker's attempt fails with the teardown (endQA).
func (e *Engine) qaBounded(ctx context.Context, t registry.Task, sess *qaSession) bool {
	run := sess.run
	if began := run.began(); !began.IsZero() && e.now().Sub(began) > sess.runCap {
		why := fmt.Sprintf("QA run %d went over its cap of %s (its server's boot and the walk)", run.N, sess.runCap)
		if err := e.qaUnavailable(ctx, t, run, why); err != nil {
			e.log.Warn("end a QA run past its cap", "task", t.ID, "err", err)
		}
		return true
	}
	if h := e.holds[HoldMemory]; h != nil && strings.HasPrefix(h.Reason, memoryCritical) {
		res := QAResult{Outcome: QAOutcomePressure, Started: run.began(),
			Why: "the Mac's memory came under critical pressure during the run: its server and browser were stopped"}
		if err := e.writeQAResult(ctx, t, run, res); err != nil {
			e.log.Warn("record a QA run the memory stopped", "task", t.ID, "err", err)
		}
		e.endQALogged(ctx, t, "the Mac's memory is under critical pressure", "")
		return true
	}
	return false
}

// qaProblem is why task t has no ready card for QA ("" for none): for a
// repository with [factory.repo.qa] and a kind with a qa stage, the result
// that passed the stage (qa-summary.json) must be clean or not_needed at
// the task's head.
func (e *Engine) qaProblem(t registry.Task) string {
	repo, ok := e.cfg.FactoryRepo(t.Repo)
	if _, has := e.qaStage(t); !ok || repo.QA == nil || !has {
		return ""
	}
	sum, ok := readQASummary(e.d.Layout.FactoryTaskDir(t.ID))
	switch {
	case !ok || (sum.Outcome != QAOutcomeClean && sum.Outcome != QAOutcomeNotNeeded):
		return "QA did not pass at " + textx.ShortSHA(t.HeadSHA)
	case sum.Head != t.HeadSHA:
		return fmt.Sprintf("QA looked at %s, another head than %s", textx.ShortSHA(sum.Head), textx.ShortSHA(t.HeadSHA))
	}
	return ""
}

// qaDecide acts on run's result res at the task's head: a summary of the run
// at the head, clean or not_needed, passes the stage, as does such a
// result; unavailable waits on you once (qaTold), and after your retry a
// fresh run starts; defects that block come here only once their fix loop
// let them go (qaFixPhase): after a stop you were told of, a fresh run
// starts (your retry), and with no stop they wait on you once, never a
// fresh walk with no writer and no cap; pressure waits for the memory hold
// to lift, interrupted starts afresh, each unless qaInARow of it came in a
// row at the head (interrupted by a crash only, qaInARowAt): then QA is
// unavailable.
func (e *Engine) qaDecide(ctx context.Context, t registry.Task, dir string, run qaRun, res QAResult) (pass, again bool, err error) {
	if sum, ok := readQASummary(dir); ok && sum.Run == run.N && sum.Head == run.Head &&
		(sum.Outcome == QAOutcomeClean || sum.Outcome == QAOutcomeNotNeeded) { // it passed the stage (passBlocked's keeps the run's result)
		if t.WaitsWho == waitQA {
			e.waitOn(ctx, t, "", "")
		}
		return true, false, nil
	}
	switch res.Outcome {
	case QAOutcomeClean, QAOutcomeNotNeeded:
		if t.WaitsWho == waitQA {
			e.waitOn(ctx, t, "", "")
		}
		return true, false, nil
	case QAOutcomeUnavailable:
		told, err := e.qaTold(ctx, t, res)
		return false, told && err == nil, err
	case QAOutcomeDefects:
		if _, stopped := readQAFixStop(dir, run); stopped {
			return false, true, nil
		}
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
// none of the daemon's secrets, for at most qaWhenCap (on the loop: a later
// step runs it off the loop, as the checks run). A failure cannot
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

// qaSession goes on with task t's session of run and returns it once it is
// ready for the walk (nil before): none starts one; one that boots keeps the
// task waiting; a failed boot is stopped, and one more boot starts once (a
// panic gets none), else QA is unavailable; a boot that answered starts the
// factory's Chrome, and the task waits "ready for the walk" until the next
// pass, which finds the session ready. A session of another run, copy or
// stage is ended first.
func (e *Engine) qaSession(ctx context.Context, t registry.Task, c Copy, run qaRun) (*qaSession, error) {
	sess := e.qaSessionOf(t.ID)
	if sess != nil && (sess.run.N != run.N || sess.run.Head != run.Head || sess.copy.Name != c.Name || sess.stage != t.Stage) {
		e.endQALogged(ctx, t, "its run is no longer the task's", qaCutTask)
		sess = nil
	}
	switch {
	case sess == nil:
		return nil, e.startQASession(ctx, t, c, run)
	case !sess.ended():
		return nil, nil // the task waits while the server boots
	case sess.err != nil:
		return nil, e.qaBootFailed(ctx, t, c, sess)
	case sess.run.CDP == "": // no browser yet, or a start of it that did not return
		return nil, e.qaBrowser(ctx, t, sess)
	}
	return sess, nil
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
// takes that boot's failure). From then on the session is in the work
// record (WorkQA) until its teardown.
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
		case sess != nil: // no boot began: the teardown waits for none
			e.endQALogged(ctx, t, "its start failed", "")
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
	if run.Began.IsZero() { // its first start: the cap counts from it, a boot more keeps it
		run.Began = run.Started
	}
	if err := writeJSON(filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), qaRunFile), run); err != nil {
		return err
	}
	bctx, cancel := context.WithTimeout(e.bgCtx, boot+qaBootSlack)
	sess = &qaSession{task: t.ID, stage: t.Stage, run: run, copy: wc, runCap: cmp.Or(repo.QA.Cap.Duration, config.DefaultQACap), server: true,
		cancel: cancel, done: make(chan struct{}),
		release: release, unlock: unlock, endWork: e.beginWork(WorkQA, t.ID, fmt.Sprintf("QA run %d of %s", run.N, TaskKey(t)))}
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
	sess.booting = true
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
	e.endQALogged(ctx, t, "its boot failed", "")
	run := sess.run
	if _, panicked := errors.AsType[qaPanic](sess.err); panicked || run.BootTries >= qaBootMore {
		return e.qaUnavailable(ctx, t, run, why)
	}
	run.BootTries++
	run.Started = time.Time{} // no server runs until the next start
	if err := writeJSON(filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), qaRunFile), run); err != nil {
		return err
	}
	e.event(ctx, t.ID, "qa", registry.PhaseInfo, fmt.Sprintf("run %d: %s; its server starts once more", run.N, why))
	return e.startQASession(ctx, t, c, run)
}

// qaBrowser starts the factory's headless Chrome for session sess in its
// run's folder, once no hold stops launches, and records its CDP endpoint,
// to which the walker's copy attaches its browser tool (qaEnvCDP, in the
// run's own session): the session is ready for the walk. A browser that does
// not start is QA unavailable; a start that panicked runs again on the next
// pass (the session has no endpoint), and the teardown stops whatever it
// started.
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
	sess.copy.Env[qaEnvCDP] = cdp
	sess.copy.Env[qaEnvSession] = qaBrowserSession(t.ID, sess.run.N)
	sess.copy.Env[qaEnvIdle] = strconv.FormatInt(qaBrowserIdle.Milliseconds(), 10)
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
// browser either (qaBrowser adds its browser tool's attachment), in an env
// of its own, never the claimed copy's.
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

// endQASession ends task t's QA session, if it has one, because why, its
// task leaving the run (endQA with qaCutTask).
func (e *Engine) endQASession(ctx context.Context, t registry.Task, why string) error {
	return e.endQA(ctx, t, why, qaCutTask)
}

// endQA ends task t's QA session, if it has one, because why, on a context
// of its own (the daemon's shutdown or the tick's end never cuts a stop). It
// cancels the boot's goroutine and waits for it (at most qaBootWait; none
// for a boot that never began), stops the browser (StopBrowser) and the
// server (stopServer; Copies.StopServer for one whose start failed, though a
// failed start already ended what it spawned, and a copy with no record of a
// server has nothing stopped or proven), each under its own
// recover (a panic is that step's error, and the next step runs), and
// records the run's end (qaCutRecord): a run with no result is interrupted
// by cut (qaCutDrain, qaCutTask), or, with no cut ("": a failed boot or
// start, QA unavailable), forgets its server's start, so a restart cuts
// nothing of it. Whatever happens, the machine's QA
// lock, then the heavy slot, then the session's work record go. A qa event
// says how it ended.
func (e *Engine) endQA(ctx context.Context, t registry.Task, why, cut string) error {
	ctx = context.WithoutCancel(ctx)
	e.qaMu.Lock()
	sess := e.qaSessions[t.ID]
	delete(e.qaSessions, t.ID)
	e.qaMu.Unlock()
	if sess == nil {
		return nil
	}
	defer sess.endWork()
	defer sess.release()
	defer sess.unlock()
	sess.cancel()
	if sess.booting {
		wait := time.NewTimer(qaBootWait)
		select {
		case <-sess.done:
		case <-wait.C:
			e.log.Warn("a QA boot's probe did not end with its run; the teardown goes on", "task", t.ID)
		}
		wait.Stop()
	}
	var errs []error
	step := func(what string, fn func() error) {
		defer func() {
			if p := recover(); p != nil {
				msg := textx.Clip(strings.Join(strings.Fields(execx.Redact(fmt.Sprint(p))), " "), panicRunes)
				e.log.Error("panic in a QA teardown; the teardown goes on", "task", t.ID, "step", what, "panic", msg,
					"stack", execx.Redact(string(debug.Stack())))
				errs = append(errs, fmt.Errorf("%s panicked: %s", what, msg))
			}
		}()
		if err := fn(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", what, err))
		}
	}
	if sess.browser {
		step("stop its browser", func() error { return e.d.Copies.StopBrowser(ctx, sess.copy) })
	}
	step("stop its server", func() error {
		if s, ok := e.servers[t.ID]; ok {
			return e.stopServer(ctx, t, s, "its QA run ended: "+why)
		}
		if !sess.server {
			return nil
		}
		err := e.d.Copies.StopServer(ctx, sess.copy)
		if _, held := errors.AsType[portHeld](err); held {
			return nil
		}
		return err
	})
	step("record its end", func() error { return e.qaCutRecord(ctx, t, sess, why, cut) })
	what := fmt.Sprintf("run %d at %s ended (%s)", sess.run.N, textx.ShortSHA(sess.run.Head), why)
	if err := errors.Join(errs...); err != nil {
		e.event(ctx, t.ID, "qa", registry.PhaseFail, what+": "+textx.Clip(strings.Join(strings.Fields(err.Error()), " "), panicRunes))
		return err
	}
	e.event(ctx, t.ID, "qa", registry.PhaseOK, what+": its browser and its server are stopped, the Mac's QA lock is free")
	return nil
}

// qaCutRecord records the end of session sess's run. One with no result is
// interrupted by cut (why it was cut; a drain's task waits on its run no
// more), or, with no cut, forgets its server's start (qa-run.json's
// started, when it is still this run's). Then, whatever ended it, the
// stage's attempts the run began that are still open fail: no walk goes on
// without its browser and its server (a walk that passed is verified, and
// stays so).
func (e *Engine) qaCutRecord(ctx context.Context, t registry.Task, sess *qaSession, why, cut string) error {
	if err := e.qaRecordEnd(ctx, t, sess, why, cut); err != nil {
		return err
	}
	if cut == qaCutDrain && t.WaitsWho == waitQA {
		e.waitOn(ctx, t, "", "")
	}
	if st, ok := e.stageOf(t, sess.stage); ok {
		e.failQAAttempts(ctx, t, st, sess.run, "its QA run ended: "+why)
	}
	return nil
}

// qaRecordEnd is qaCutRecord's record of a run with no result: interrupted
// by cut, or, with no cut, its server's start forgotten.
func (e *Engine) qaRecordEnd(ctx context.Context, t registry.Task, sess *qaSession, why, cut string) error {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	if _, ended := readQAResult(dir, sess.run); ended {
		return nil
	}
	if cut == "" {
		run, ok := readQARun(dir)
		if !ok || run.N != sess.run.N || run.Head != sess.run.Head || run.Started.IsZero() {
			return nil
		}
		run.Started = time.Time{}
		return writeJSON(filepath.Join(dir, qaRunFile), run)
	}
	text := "the run was cut (" + why + "): its server and browser were stopped"
	if cut == qaCutDrain {
		text = "the daemon stopped during the run (a drain or a signal): its server and browser were stopped"
	}
	return e.writeQAResult(ctx, t, sess.run, QAResult{Outcome: QAOutcomeInterrupted, Cut: cut, Why: text, Started: sess.run.began()})
}

// endQALogged is endQA whose failure the log keeps (its event says it too).
func (e *Engine) endQALogged(ctx context.Context, t registry.Task, why, cut string) {
	if err := e.endQA(ctx, t, why, cut); err != nil {
		e.log.Warn("end a QA session", "task", t.ID, "err", err)
	}
}

// endQASessionLogged is endQASession whose failure the log keeps.
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
		e.endQALogged(ctx, t, "the daemon stops", qaCutDrain)
	}
}

// sweepQA ends the QA sessions of the tasks that are not at their run: gone,
// not live (a request stopped them, they wait on you), or at another stage
// than the session's (sent back); and, on every tick whatever holds the
// stages (a drain, the infrastructure's or herdr's hold), a session past
// its cap or under critical memory pressure (qaBounded). While the daemon
// drains, it ends every session whose walk has not begun (qaWalking), as a
// drain's cut: such a session waits for a launch the drain holds, and
// install --drain would wait for it in turn.
func (e *Engine) sweepQA(ctx context.Context, tasks []registry.Task) {
	e.qaMu.Lock()
	sessions := maps.Clone(e.qaSessions)
	e.qaMu.Unlock()
	drain := e.startsHeld()
	for _, id := range slices.Sorted(maps.Keys(sessions)) {
		i := slices.IndexFunc(tasks, func(t registry.Task) bool { return t.ID == id })
		switch {
		case i < 0:
			e.endQASessionLogged(ctx, registry.Task{ID: id}, "its task is gone")
		case !isLive(tasks[i]):
			e.endQASessionLogged(ctx, tasks[i], "its task is not at work")
		case tasks[i].Stage != sessions[id].stage:
			e.endQASessionLogged(ctx, tasks[i], "its task left the "+sessions[id].stage+" stage")
		case e.qaBounded(ctx, tasks[i], sessions[id]): // ended: past its cap, or under critical pressure
		case drain && !e.qaWalking(ctx, tasks[i], sessions[id]):
			e.endQALogged(ctx, tasks[i], "a drain holds before its walk began", qaCutDrain)
		}
	}
}

// qaWalking reports whether session sess's walk began: an attempt of its
// stage after the run's attempts_before runs or ended unverified. One still
// launching has no walker yet (a hold, the drain among them, keeps its
// launch), so it has not begun. A registry that cannot be read counts as a
// walk (the session stays, and the next tick looks again).
func (e *Engine) qaWalking(ctx context.Context, t registry.Task, sess *qaSession) bool {
	as, err := e.reg.Attempts(ctx, t.ID)
	if err != nil {
		return true
	}
	return slices.ContainsFunc(as, func(a registry.Attempt) bool {
		return a.Stage == sess.stage && a.N > sess.run.AttemptsBefore &&
			(a.State == registry.AttemptRunning || a.State == registry.AttemptEnded)
	})
}

// cutQA is startup's record of the QA runs a crashed daemon cut (one that
// stopped cleanly recorded its own, qaCutRecord): each task at work or
// waiting on you, at a qa stage, whose newest run started its server and has
// no result gets the outcome interrupted by a crash (its server and its
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
		res := QAResult{Outcome: QAOutcomeInterrupted, Cut: qaCutCrash, Why: "the daemon stopped during the run: its server and browser were stopped",
			Started: run.began(), Ended: e.now()}
		if err := e.writeQAResult(ctx, t, run, res); err != nil {
			e.log.Warn("record a QA run a restart cut", "task", t.ID, "err", err)
			continue
		}
		if t.WaitsWho == waitQA { // "ready for the walk" no more
			e.waitOn(ctx, t, "", "")
		}
		e.failQAAttempts(ctx, t, st, run, "the daemon stopped during its QA run")
	}
}

// failQAAttempts fails the open attempts of task t's qa stage st that run
// began (after its attempts_before): launching, running or ended unverified.
// A walk it failed was cut with its walker at work, or due: the stage's
// roles quit (quitWalkers), so no agent drives the stopped browser or holds
// the Mac's memory, and the next run's walker resumes no turn still running.
func (e *Engine) failQAAttempts(ctx context.Context, t registry.Task, st config.FactoryStage, run qaRun, because string) {
	as, err := e.reg.Attempts(ctx, t.ID)
	if err != nil {
		e.log.Warn("read the attempts of a cut QA run", "task", t.ID, "err", err)
		return
	}
	failed := false
	for _, a := range as {
		if a.Stage != st.Name || a.N <= run.AttemptsBefore {
			continue
		}
		switch a.State {
		case registry.AttemptLaunching, registry.AttemptRunning, registry.AttemptEnded:
			if err := e.reg.TransitionAttempt(ctx, a.ID, []string{a.State}, registry.AttemptFailed, func(u *registry.AttemptUpdate) {
				u.Set("error", because)
			}); err != nil {
				e.log.Warn("end the attempt of a cut QA run", "attempt", a.ID, "err", err)
				continue
			}
			failed = true
		}
	}
	if failed {
		e.quitWalkers(ctx, t, st, because)
	}
}

// quitWalkers quits the agents of task t's qa stage st, each role as it
// runs (Runtime.QuitRole), because why. A quit that fails (you in the pane,
// herdr) is a qa event and a warning, never the teardown's failure.
func (e *Engine) quitWalkers(ctx context.Context, t registry.Task, st config.FactoryStage, why string) {
	if e.d.Runtime == nil {
		return
	}
	for _, name := range st.Roles {
		role, ok := e.cfg.FactoryRole(name)
		if !ok {
			continue
		}
		if runAs, err := e.d.Runtime.RunsAs(t, role); err == nil {
			role = runAs
		}
		if err := e.d.Runtime.QuitRole(ctx, t, role.Name, why); err != nil {
			e.log.Warn("quit the agent of a cut QA run", "task", t.ID, "role", role.Name, "err", err)
			e.event(ctx, t.ID, "qa", registry.PhaseFail, "quit the "+role.Name+" agent of the cut run: "+
				textx.Clip(textx.FirstLine(execx.Redact(err.Error())), qaWhyRunes))
			continue
		}
		e.event(ctx, t.ID, "qa", registry.PhaseOK, "quit the "+role.Name+" agent: "+why)
	}
}

// qaUnavailable ends run as QA that could not run, because why: its
// result.json and a failed qa check (the environment's), the session's
// teardown, then task t needs you with why and the retry command. A daemon
// that stops before the move (beforeStop, a test's seam) leaves the result
// for the next pass, which moves the task then (qaTold).
func (e *Engine) qaUnavailable(ctx context.Context, t registry.Task, run qaRun, why string) error {
	now := e.now()
	started := run.began()
	if started.IsZero() {
		started = now
	}
	err := e.writeQAResult(ctx, t, run, QAResult{Outcome: QAOutcomeUnavailable, Why: why, Started: started, Ended: now})
	e.endQALogged(ctx, t, "QA could not run", "")
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
	e.toOperator(ctx, t, qaYouWhat(t, e.d.Layout.FactoryTaskDir(t.ID), res), "QA stopped for you")
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
			Source: "factory", Command: fmt.Sprintf("qa run %d", run.N), Took: res.Ended.Sub(res.Started)})
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

// qaInARowAt is how many of the newest runs at head in a row ended outcome;
// an interrupted run counts only as a crash's (a cut of one older than its
// cut counts too), and one the factory cut itself (a drain, its task
// leaving it) neither counts nor breaks the row.
func qaInARowAt(dir, head, outcome string) int {
	return qaRowAt(dir, head, func(res QAResult) bool { return res.Outcome == outcome })
}

// qaRowAt is qaInARowAt for the results that match, whatever their outcome.
func qaRowAt(dir, head string, match func(QAResult) bool) int {
	n := 0
	for _, k := range qaRunsAt(dir, head) {
		res, ok := readQAResult(dir, qaRun{Dir: qaRunDir(k, head)})
		if ok && res.Outcome == QAOutcomeInterrupted && res.Cut != "" && res.Cut != qaCutCrash {
			continue
		}
		if !ok || !match(res) || res.Head != head {
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
// [factory.repo.qa] (or with no [[factory.repo]] at all, as qaProblem has
// it). "" when every stage after i is skipped.
func (e *Engine) nextStage(t registry.Task, kind config.FactoryKind, i int) (name string, skipped []string) {
	repo, _ := e.cfg.FactoryRepo(t.Repo)
	for _, st := range kind.Stages[i+1:] {
		if st.Type == config.FactoryStageQA && repo.QA == nil {
			skipped = append(skipped, st.Name)
			continue
		}
		return st.Name, skipped
	}
	return "", skipped
}
