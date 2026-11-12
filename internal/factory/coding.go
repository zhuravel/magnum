package factory

// The coding stage (spec 6.4): one writer in the task's copy, on the
// task's local branch, test first. Before its first prompt the branch
// starts fresh from the base (startCoding). The stage ends on its end fact
// `proof`, never on a "done": the branch has commits after origin/<base>,
// git status shows no tracked change, and the factory's own proof and the
// precheck passed at the current head. The proof and the precheck can take
// minutes, and the loop is sequential: they run off the loop, one check per
// task (checkHead), while the task waits with what the check does, and a
// later pass takes the result; a head that moved voids it.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/scrub"
	"github.com/zhuravel/magnum/internal/textx"
)

// waitPrecheck is the WaitsWho of a task whose factory checks (its proof,
// its precheck) run.
const waitPrecheck = "precheck"

// checkRetry is how long a check that did not run to its end (git failed
// in it) is kept before the next pass runs a new one; checkTries how many
// such checks in a row a task gets before it needs you.
const (
	checkRetry = time.Minute
	checkTries = 3
)

// baseRecord is base.json, the base_sha artifact: the task's branch and the
// commit of origin/<base> it starts from (its merge base), as the coding
// stage left them before the writer's first prompt.
type baseRecord struct {
	Branch string    `json:"branch"`
	Base   string    `json:"base"`
	At     time.Time `json:"at"`
}

// startCoding puts task t's copy c on the task's branch, fresh from the
// base, before a coding attempt's first prompt: a fetch of origin/<base>,
// then the branch created there (gitx.SwitchCreate), or, when it exists, a
// branch without a commit of its own (the one the first claim made) moved
// to the fresh base (gitx.FastForward, which carries the reproducer's spec
// left in the copy; a change in the way keeps the old base, with an event),
// a branch of an earlier attempt of the task (base.json names it) reused as
// it is, and a branch of the name that holds commits not the task's
// (another task's, yours) left alone: the task works on the name with the
// next free number, from origin/<base>. Its base is recorded (base.json,
// the base_sha artifact). It is a guarded step, with begin and ok or fail
// events, after the copy's guard (no agent at work in it), and it is
// idempotent: run again, it finds the branch it made. A switch git refuses
// over a file it does not track (yours, never written over) is a needsYou
// naming it (refusedWhat).
func (e *Engine) startCoding(ctx context.Context, t *registry.Task, c Copy) error {
	repo, err := e.factoryRepo(*t)
	if err != nil {
		return err
	}
	if t.Branch == "" {
		return fmt.Errorf("%s has no branch to code on", TaskKey(*t))
	}
	if err := e.copyIdle(ctx, *t); err != nil {
		if h, ok := errors.AsType[heldError](err); ok {
			e.waitOn(ctx, *t, h.who, h.what)
		}
		return err
	}
	git := e.d.Git
	base := baseOf(repo)
	upstream := "origin/" + base
	e.event(ctx, t.ID, "branch", registry.PhaseBegin, "prepares "+t.Branch+" in "+c.Name+" from a fresh "+upstream)
	fail := func(err error) error {
		e.event(ctx, t.ID, "branch", registry.PhaseFail, t.Branch+" in "+c.Name+": "+textx.Clip(textx.FirstLine(err.Error()), 300))
		if files, line, ok := refusal(err); ok { // a file of yours in the way: never written over, and yours to move
			return needsYou{refusedWhat(c.Name, files, line) + retryHint(*t)}
		}
		return err
	}
	if err := git.FetchBranch(ctx, cmp.Or(repo.MainClone, c.Path), base); err != nil {
		return fail(err)
	}
	switch tip, err := git.RevParse(ctx, c.Path, "refs/heads/"+t.Branch); {
	case errors.Is(err, gitx.ErrNoSuchRef):
		err := git.SwitchCreate(ctx, c.Path, t.Branch, upstream)
		if errors.Is(err, gitx.ErrBranchExists) { // made since the check
			err = git.Switch(ctx, c.Path, t.Branch)
		}
		if err != nil {
			return fail(err)
		}
	case err != nil:
		return fail(err)
	default:
		commits, err := git.CommitsAfter(ctx, c.Path, upstream, tip, false)
		if err != nil {
			return fail(err)
		}
		switch {
		case len(commits) == 0:
			if err := e.onBranch(ctx, c, t.Branch); err != nil {
				return fail(err)
			}
			if err := git.FastForward(ctx, c.Path, upstream); err != nil {
				e.event(ctx, t.ID, "branch", registry.PhaseInfo, fmt.Sprintf("%s stays at %s: the changes in %s stop its move to %s (%s)",
					t.Branch, short(tip), c.Name, upstream, textx.Clip(textx.FirstLine(err.Error()), 200)))
			}
		case e.ownBranch(*t):
			if err := e.onBranch(ctx, c, t.Branch); err != nil {
				return fail(err)
			}
		default:
			name, err := e.freeBranch(ctx, *t, repo, c)
			if err != nil {
				return fail(err)
			}
			if err := git.SwitchCreate(ctx, c.Path, name, upstream); err != nil {
				return fail(err)
			}
			if err := e.reg.TransitionTask(ctx, t.ID, []string{t.State}, "", func(u *registry.TaskUpdate) {
				u.Set("branch", name)
				u.Because(t.Branch + " holds commits not this task's")
			}); err != nil {
				return fail(err)
			}
			e.event(ctx, t.ID, "branch", registry.PhaseInfo, fmt.Sprintf("%s holds %s not this task's (at %s): the task works on %s, from %s",
				t.Branch, plural(len(commits), "commit"), short(tip), name, upstream))
			t.Branch = name
		}
	}
	head, err := git.RevParse(ctx, c.Path, "HEAD")
	if err != nil {
		return fail(err)
	}
	from, err := git.MergeBase(ctx, c.Path, head, upstream)
	if err != nil {
		return fail(err)
	}
	b, err := json.MarshalIndent(baseRecord{Branch: t.Branch, Base: from, At: e.now()}, "", "  ")
	if err != nil {
		return fail(err)
	}
	if err := e.saveArtifact(ctx, *t, "base_sha", baseFile, append(b, '\n')); err != nil {
		return fail(err)
	}
	e.event(ctx, t.ID, "branch", registry.PhaseOK, fmt.Sprintf("%s at %s in %s, from %s at %s", t.Branch, short(head), c.Name, upstream, short(from)))
	return nil
}

// onBranch checks branch out in copy c unless it is checked out.
func (e *Engine) onBranch(ctx context.Context, c Copy, branch string) error {
	cur, err := e.d.Git.CurrentBranch(ctx, c.Path)
	if err != nil || cur == branch {
		return err
	}
	return e.d.Git.Switch(ctx, c.Path, branch)
}

// ownBranch reports whether task t's branch is one an earlier coding
// attempt of the task started (base.json names it).
func (e *Engine) ownBranch(t registry.Task) bool {
	b, err := os.ReadFile(filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), baseFile))
	if err != nil {
		return false
	}
	var rec baseRecord
	return json.Unmarshal(b, &rec) == nil && rec.Branch == t.Branch
}

// freeBranch is the name of task t's branch with the next free number
// (-2, -3, ...): no other task's, no branch of the copy's repository.
func (e *Engine) freeBranch(ctx context.Context, t registry.Task, repo config.FactoryRepo, c Copy) (string, error) {
	tasks, err := e.reg.Tasks(ctx)
	if err != nil {
		return "", err
	}
	taken := map[string]bool{}
	for _, o := range tasks {
		if o.ID != t.ID {
			taken[o.Branch] = true
		}
	}
	for n := 2; ; n++ {
		name := fmt.Sprintf("%s-%d", t.Branch, n)
		if taken[name] || e.branchExists(ctx, repo.Repo, name) {
			continue
		}
		if _, err := e.d.Git.RevParse(ctx, c.Path, "refs/heads/"+name); errors.Is(err, gitx.ErrNoSuchRef) {
			return name, nil
		} else if err != nil {
			return "", err
		}
	}
}

// headCheck is the factory's check of a task's head off the loop: the head
// and the specs it checks, whether it runs the proof too (the coding stage)
// or the precheck alone, done closed when it ended (at ended), cancel to end
// it, and its result (written before done closes).
type headCheck struct {
	head, specs string
	proof       bool
	done        chan struct{}
	cancel      context.CancelFunc
	endedAt     time.Time
	res         checkResult
	// counted: its failure to run to its end is counted (checkFails).
	counted bool
	// endWork takes the check out of the record of the work a restart
	// would cut (beginWork): once a tick took its result, which lives only
	// here, or once it is dropped. A restart before then loses the result.
	endWork func()
}

// end takes x out of the record of the work a restart would cut; a second
// call does nothing.
func (x *headCheck) end() {
	if x.endWork != nil {
		x.endWork()
	}
}

// checkResult is a check's end: its proof (nil without one), its precheck
// (nil when it did not get there), and the error of a check that did not run
// to its end (a wait, git, the daemon stopping).
type checkResult struct {
	Proof    *Proof
	Precheck *PrecheckResult
	Err      error
}

func (x *headCheck) ended() bool {
	select {
	case <-x.done:
		return true
	default:
		return false
	}
}

// checkRuns reports whether a check of task id runs.
func (e *Engine) checkRuns(id int64) bool {
	e.checksMu.Lock()
	defer e.checksMu.Unlock()
	x := e.checks[id]
	return x != nil && !x.ended()
}

// noteCheck says what the check of task id does now (its task's wait).
func (e *Engine) noteCheck(id int64, what string) {
	e.checksMu.Lock()
	defer e.checksMu.Unlock()
	e.checkNotes[id] = what
}

// checkNote is what the check of task id does now ("" for nothing said).
func (e *Engine) checkNote(id int64) string {
	e.checksMu.Lock()
	defer e.checksMu.Unlock()
	return e.checkNotes[id]
}

// countFailedCheck counts the ended check of task id that did not run to
// its end, once, and returns how many did in a row.
func (e *Engine) countFailedCheck(id int64) int {
	e.checksMu.Lock()
	defer e.checksMu.Unlock()
	if x := e.checks[id]; x != nil && !x.counted {
		x.counted = true
		e.checkFails[id]++
	}
	return e.checkFails[id]
}

// checkRanToItsEnd ends task id's run of checks that did not run to their
// end.
func (e *Engine) checkRanToItsEnd(id int64) {
	e.checksMu.Lock()
	defer e.checksMu.Unlock()
	delete(e.checkFails, id)
}

// forgetCheck drops x, the check of task id, once it ended or was stopped.
func (e *Engine) forgetCheck(id int64, x *headCheck) {
	e.checksMu.Lock()
	forgot := e.checks[id] == x
	if forgot {
		delete(e.checks, id)
		delete(e.checkNotes, id)
	}
	e.checksMu.Unlock()
	if forgot {
		x.end() // its record's write runs outside checksMu
	}
}

// checkHead is how a stage gets the factory's checks of head (with the spec
// files specs) of task t in copy c: the proof and the precheck (proof), or
// the precheck alone. The first call for a head starts the check off the
// loop, as the task's heavy work (a slot of heavy_stages: the task waits
// "heavy: …" without one), named in the record of the work a restart
// would cut while it runs, and never while the daemon drains; every call
// while it runs is not ready; the call after it ended has its result, kept
// for that head. A check of another head (or other specs) is stopped, or,
// ended, dropped, and the next call starts one for this head.
func (e *Engine) checkHead(ctx context.Context, t registry.Task, c Copy, head, specs string, proof bool) (checkResult, bool) {
	e.checksMu.Lock()
	x := e.checks[t.ID]
	e.checksMu.Unlock()
	if x != nil {
		same := x.head == head && x.specs == specs && x.proof == proof
		switch {
		case !x.ended() && same:
			return checkResult{}, false
		case !x.ended():
			x.cancel() // the head moved: this check is void, a new one comes once it ended
			return checkResult{}, false
		case same:
			x.end() // taken: the stage that asked records what it did
			return x.res, true
		}
		e.forgetCheck(t.ID, x)
	}
	if e.startsHeld() {
		return checkResult{}, false
	}
	release, ok := e.heavy(ctx, t, waitPrecheck)
	if !ok {
		return checkResult{}, false
	}
	cctx, cancel := context.WithCancel(e.bgCtx)
	x = &headCheck{head: head, specs: specs, proof: proof, done: make(chan struct{}), cancel: cancel}
	what, work := "the precheck", "precheck of "
	if proof {
		what, work = "the proof and the precheck", "proof and precheck of "
	}
	e.checksMu.Lock()
	e.checks[t.ID] = x
	e.checkNotes[t.ID] = waitPrecheck + ": starts " + what + " of " + short(head)
	e.checksMu.Unlock()
	x.endWork = e.beginWork(WorkCheck, t.ID, work+TaskKey(t))
	e.event(ctx, t.ID, "check", registry.PhaseBegin, "checks "+short(head)+" off the loop: "+what)
	e.bg.Go(func() {
		defer e.Kick() // a tick takes the result now: until then a restart loses it
		defer close(x.done)
		defer cancel()
		defer release()
		defer func() {
			if r := recover(); r != nil {
				e.log.Error("panic in a task's checks; the daemon goes on", "task", t.ID, "panic", fmt.Sprint(r),
					"stack", execx.Redact(string(debug.Stack())))
				x.res = checkResult{Err: fmt.Errorf("the checks panicked: %v", r)}
			}
			x.endedAt = e.now()
			e.endCheckEvent(t, x)
		}()
		live := e.openLive(t.ID, short(head)+": "+what) // the run's live.log, emptied
		defer live.close()
		x.res = e.runChecks(withLive(cctx, live), t, c, proof)
	})
	return checkResult{}, false
}

// endCheckEvent writes the end of check x of task t.
func (e *Engine) endCheckEvent(t registry.Task, x *headCheck) {
	ctx := context.Background()
	_, crashed := errors.AsType[envCrash](x.res.Err)
	switch r := x.res; {
	case crashed:
		e.event(ctx, t.ID, "check", registry.PhaseFail, short(x.head)+": "+textx.Clip(r.Err.Error(), 300)+
			"; the infrastructure hold runs the checks again")
	case r.Err != nil:
		e.event(ctx, t.ID, "check", registry.PhaseFail, short(x.head)+": did not run to its end: "+textx.Clip(textx.FirstLine(r.Err.Error()), 300))
	case r.passed() && r.Precheck.Unfinished != "": // the cap's note: no failure, so no fix turn
		e.event(ctx, t.ID, "check", registry.PhaseOK, short(x.head)+": passed, but "+r.Precheck.Unfinished+
			" (the precheck's cap): the task goes on as after a pass, and CI is their only run")
	case r.passed():
		e.event(ctx, t.ID, "check", registry.PhaseOK, short(x.head)+": passed")
	default:
		e.event(ctx, t.ID, "check", registry.PhaseOK, short(x.head)+": "+r.why())
	}
}

// runChecks runs task t's checks in copy c: the proof first (proof), then,
// when it passed, the precheck.
func (e *Engine) runChecks(ctx context.Context, t registry.Task, c Copy, proof bool) checkResult {
	var r checkResult
	if proof {
		e.noteCheck(t.ID, "proof: starts")
		p, err := e.runProof(ctx, t, c)
		r.Proof = &p
		if err != nil || !p.Passed {
			r.Err = err
			return r
		}
	}
	e.noteCheck(t.ID, "precheck: starts")
	pre, err := e.precheck(ctx, t, c)
	r.Precheck, r.Err = &pre, err
	return r
}

// passedOrNone reports whether p is no proof or one that passed.
func (p *Proof) passedOrNone() bool { return p == nil || p.Passed }

// forYou reports whether the checks failed on what the writer cannot fix
// (the proof's or, after a proof that passed, the precheck's ForYou).
func (r checkResult) forYou() bool {
	return (r.Proof != nil && !r.Proof.Passed && r.Proof.ForYou) || (r.Proof.passedOrNone() && r.Precheck != nil && r.Precheck.ForYou)
}

// endedResult is the result of task id's check that ended, if one did.
func (e *Engine) endedResult(id int64) (checkResult, bool) {
	e.checksMu.Lock()
	x := e.checks[id]
	e.checksMu.Unlock()
	if x == nil || !x.ended() {
		return checkResult{}, false
	}
	return x.res, true
}

// passed reports whether the checks passed.
func (r checkResult) passed() bool {
	return r.Err == nil && (r.Proof == nil || r.Proof.Passed) && r.Precheck != nil && r.Precheck.Passed
}

// why is why the checks did not pass, in plain words: a precheck's
// failure carries what its cap kept it from, which is nothing to fix.
func (r checkResult) why() string {
	switch {
	case r.Proof != nil && !r.Proof.Passed:
		return "the factory's proof: " + r.Proof.Why
	case r.Precheck != nil && !r.Precheck.Passed && r.Precheck.Unfinished != "":
		return r.Precheck.Why + "; " + r.Precheck.Unfinished + " (the precheck's cap: nothing to fix)"
	case r.Precheck != nil && !r.Precheck.Passed:
		return r.Precheck.Why
	}
	return ""
}

// failedCheck is the check whose failure why says, and its failures: the
// factory's proof and the failing examples of its head run (else its why),
// or the precheck's failing check and its failures (failCheck); unnamed
// when they are no example's or offense's (firstFailing, the why's line).
func (r checkResult) failedCheck() (check string, failed []string, unnamed bool) {
	switch {
	case r.Proof != nil && !r.Proof.Passed:
		for _, f := range failures(r.Proof.Head.Output) {
			failed = append(failed, f.key)
		}
		if len(failed) == 0 { // its why, its head's sha read as any: a commit that fails the same way is no progress
			return "the factory's proof", []string{shaRe.ReplaceAllString(textx.FirstLine(r.Proof.Why), "<sha>")}, true
		}
		return "the factory's proof", failed, false
	case r.Precheck != nil && !r.Precheck.Passed:
		return r.Precheck.check, r.Precheck.failures, r.Precheck.unnamed
	}
	return "", nil, false
}

// failedCommand is the command of the failure why says, as it ran, for the
// writer to run until it passes: the head's spec run of a proof that failed
// there (red, or a hang), the precheck's failing command; "" when no
// command ran to that failure (a spec that passes on the base too, a
// proof.json naming a spec the head lacks).
func (r checkResult) failedCommand() string {
	switch {
	case r.Proof != nil && !r.Proof.Passed:
		if r.Proof.Head.Exit != 0 {
			return r.Proof.Head.Command
		}
		return ""
	case r.Precheck != nil && !r.Precheck.Passed:
		return r.Precheck.Command
	}
	return ""
}

// provenHead is the rest of the proof end fact, past proof.json (checkEnd):
// task t's branch has commits after origin/<base> and no tracked change in
// copy c, and the factory's proof and the precheck passed at its head. While
// the checks run (or wait for a heavy slot) it is pending, and the task
// waits saying what they do; *t has its wait. A problem is the writer's to
// fix (a change problem: its fix turn names the file the checks wrote), or
// yours (operator: the configuration's or the machine's, named by its key,
// and checks that did not run to their end checkTries times in a row); head
// is the head that passed. err is a failure to read the branch.
func (e *Engine) provenHead(ctx context.Context, t *registry.Task, c Copy) (p *endProblem, head string, pending bool, err error) {
	if e.checkRuns(t.ID) {
		e.holdTryRuns(t.ID)
		e.precheckWait(ctx, t)
		return nil, "", true, nil
	}
	// An ended check whose failure is yours comes first: the tree may hold
	// what it left for you (changes it could not tell from others'), which
	// the writer must not be asked to commit. It is taken then: the task goes
	// to NEEDS YOU, and once it goes on its head is checked afresh, after
	// what the check kept is settled (settleKept).
	if res, ok := e.endedResult(t.ID); ok && res.Err == nil && res.forYou() {
		delete(e.envCrashed, t.ID)
		e.clearPrecheckWait(ctx, t)
		e.dropCheck(t.ID)
		return forYouProblem(*t, precheckFile, res), "", false, nil
	}
	if p, pending, err := e.keptFirst(ctx, t); p != nil || pending || err != nil {
		return p, "", pending, err
	}
	repo, err := e.factoryRepo(*t)
	if err != nil {
		return nil, "", false, err
	}
	bh, err := e.branchHead(ctx, *t, c, repo)
	if err != nil {
		return nil, "", false, err
	}
	if bh.Problem != "" {
		return &endProblem{file: "the branch", why: bh.Problem, change: true}, "", false, nil
	}
	pf, fp := readProofFile(e.d.Layout.FactoryTaskDir(t.ID), time.Time{})
	if fp != nil {
		return fp, "", false, nil
	}
	res, ready := e.checkHead(ctx, *t, c, bh.Head, strings.Join(pf.Specs, "\x00"), true)
	crash, crashed := errors.AsType[envCrash](res.Err)
	if ready && !crashed {
		delete(e.envCrashed, t.ID) // a check ran past the environment: no try of the hold any more
	}
	if !ready {
		if e.checkRuns(t.ID) { // a check that waits for a heavy slot is no try yet: the hold is not held up by it
			e.holdTryRuns(t.ID)
			e.precheckWait(ctx, t)
		} else if fresh, rerr := e.reg.TaskByID(ctx, t.ID); rerr == nil { // a heavy slot's wait, or the drain
			t.WaitsWho, t.WaitsWhat = fresh.WaitsWho, fresh.WaitsWhat
		}
		return nil, "", true, nil
	}
	e.clearPrecheckWait(ctx, t)
	switch {
	case errors.Is(res.Err, ErrHeld):
		e.dropCheck(t.ID)
		w, _ := errors.AsType[heldError](res.Err)
		e.waitOn(ctx, *t, w.who, w.what)
		t.WaitsWho, t.WaitsWhat = w.who, w.what
		return nil, "", true, nil
	case crashed:
		// The machine's environment: the infrastructure hold weighs it with
		// the tick's other failures (Tick), at no cost to the task, and the
		// next check, at the hold's try, is the try (holdTryRuns). A crash
		// that stays at one head envCrashTries times is no wait's to end:
		// the task needs you, and the hold no longer counts it.
		e.dropCheck(t.ID)
		e.checkRanToItsEnd(t.ID)
		ec := e.envCrashed[t.ID]
		if ec.head != bh.Head {
			ec = envCrashes{head: bh.Head}
		}
		if ec.n++; ec.n >= envCrashTries {
			delete(e.envCrashed, t.ID)
			return operatorProblem(*t, precheckFile, fmt.Sprintf("precheck: %s, %d times at %s; fix: %s", crash.Error(), ec.n,
				short(bh.Head), crash.fix())), "", false, nil
		}
		e.envCrashed[t.ID] = ec
		e.envFails = append(e.envFails, taskFailure{task: *t, err: crash})
		what := crash.Error() + "; the infrastructure hold runs the checks again"
		e.waitOn(ctx, *t, waitPrecheck, what)
		t.WaitsWho, t.WaitsWhat = waitPrecheck, what
		return nil, "", true, nil
	case res.Err != nil:
		if n := e.countFailedCheck(t.ID); n >= checkTries {
			e.dropCheck(t.ID)
			e.checkRanToItsEnd(t.ID)
			return operatorProblem(*t, "the checks", fmt.Sprintf("the factory's checks of %s did not run to their end "+
				"%d times: %s", short(bh.Head), n, clipEnd(textx.FirstLine(scrub.Text(res.Err.Error())), 240))), "", false, nil
		}
		e.retryCheck(ctx, t, res.Err)
		return nil, "", true, nil // a wait: a check that did not run to its end runs again after checkRetry
	case res.Proof != nil && res.Proof.Passed && !res.Proof.Holds(bh.Head):
		e.dropCheck(t.ID) // the head moved while it ran: void
		return nil, "", true, nil
	}
	e.checkRanToItsEnd(t.ID)
	if !res.passed() {
		file := precheckFile
		if res.Proof != nil && !res.Proof.Passed {
			file = factoryProofFile
		}
		if res.forYou() {
			return forYouProblem(*t, file, res), "", false, nil
		}
		check, failed, unnamed := res.failedCheck()
		p := &endProblem{file: file, why: res.why(), change: true, command: res.failedCommand(), check: check, failures: failed,
			unnamed: unnamed}
		if res.Precheck != nil && (res.Proof == nil || res.Proof.Passed) {
			p.all = res.Precheck.all
		}
		return p, "", false, nil
	}
	return nil, bh.Head, false, nil
}

// holdTryRuns notes that the checks of task id, whose last check crashed
// on the machine's environment (envCrashed), run: they are the
// infrastructure hold's try, which neither lifts the hold nor fails until
// they end (hold).
func (e *Engine) holdTryRuns(id int64) {
	if _, ok := e.envCrashed[id]; ok {
		e.holdTry = true
	}
}

// envCrashes are the crashes of a task's lint on the machine's environment
// at one head, in a row: the head and how many.
type envCrashes struct {
	head string
	n    int
}

// operatorProblem is an end problem of task t's checks that needs you
// (the configuration's or the machine's, never the writer's): why, then the
// retry that lets the task go on once you fixed it (retryHint).
func operatorProblem(t registry.Task, file, why string) *endProblem {
	return &endProblem{file: file, why: why + retryHint(t), operator: true}
}

// forYouProblem is the end problem of task t's checks res, whose failure
// is yours (file the one they wrote): a check that named no failure of the
// code after its fault steps asks you its question (faultQuestion, the
// problem's ask), with why and no retry; any other is operatorProblem's.
func forYouProblem(t registry.Task, file string, res checkResult) *endProblem {
	if res.Proof.passedOrNone() && res.Precheck != nil && res.Precheck.fault != nil {
		q := faultQuestion(*res.Precheck.fault)
		return &endProblem{file: file, why: res.why(), operator: true, ask: &q}
	}
	return operatorProblem(t, file, res.why())
}

// keptFirst is provenHead's guard before it reads the tree: while task t
// has a record of changes a check left in its copy (kept.json: kept for
// you, or a put-back that failed), nothing hands that tree to the writer.
// An ended check that left them is taken first, whatever ended it: its
// failure to run is counted as any other's (checkTries in a row need you),
// and it is dropped. Then the record is settled (settleKept): p is what the
// task needs you for, pending a wait (*t has it), nil and false once the
// tree holds none of it.
func (e *Engine) keptFirst(ctx context.Context, t *registry.Task) (p *endProblem, pending bool, err error) {
	if !e.keptRecorded(t.ID) {
		return nil, false, nil
	}
	e.checksMu.Lock()
	x := e.checks[t.ID]
	e.checksMu.Unlock()
	if x != nil && x.ended() {
		if res := x.res; res.Err != nil {
			if n := e.countFailedCheck(t.ID); n >= checkTries {
				e.dropCheck(t.ID)
				e.checkRanToItsEnd(t.ID)
				e.clearPrecheckWait(ctx, t)
				return operatorProblem(*t, "the checks", fmt.Sprintf("the factory's checks of %s did not run to their end "+
					"%d times: %s", short(x.head), n, clipEnd(textx.FirstLine(scrub.Text(res.Err.Error())), 240))), false, nil
			}
		}
		e.dropCheck(t.ID)
	}
	why, err := e.settleKept(ctx, *t)
	switch {
	case errors.Is(err, ErrHeld):
		if h, ok := errors.AsType[heldError](err); ok {
			t.WaitsWho, t.WaitsWhat = h.who, h.what
		}
		return nil, true, nil
	case err != nil:
		return nil, false, err
	case why != "":
		e.clearPrecheckWait(ctx, t)
		return operatorProblem(*t, keptFile, why), false, nil
	}
	return nil, false, nil
}

// dropCheck forgets the ended check of task id: the next pass runs a new
// one.
func (e *Engine) dropCheck(id int64) {
	e.checksMu.Lock()
	x := e.checks[id]
	e.checksMu.Unlock()
	if x != nil && x.ended() {
		e.forgetCheck(id, x)
	}
}

// retryCheck keeps a check that did not run to its end (err) for
// checkRetry, the task waiting with its reason, then drops it.
func (e *Engine) retryCheck(ctx context.Context, t *registry.Task, err error) {
	e.checksMu.Lock()
	x := e.checks[t.ID]
	e.checksMu.Unlock()
	if x == nil {
		return
	}
	next := x.endedAt.Add(checkRetry)
	if !e.now().Before(next) {
		e.forgetCheck(t.ID, x)
		return
	}
	what := "the checks did not run to their end (" + clipEnd(textx.FirstLine(scrub.Text(err.Error())), 240) + "); they run again at " +
		next.Local().Format("15:04")
	e.waitOn(ctx, *t, waitPrecheck, what)
	t.WaitsWho, t.WaitsWhat = waitPrecheck, what
}

// clipEnd is s, or its last n runes after an ellipsis: a command's error
// says what went wrong at its end, after the command and its paths.
func clipEnd(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}

// precheckWait is the wait of task t while its checks run: what they do.
func (e *Engine) precheckWait(ctx context.Context, t *registry.Task) {
	what := e.checkNote(t.ID)
	if what == "" {
		what = waitPrecheck + ": runs"
	}
	e.waitOn(ctx, *t, waitPrecheck, what)
	t.WaitsWho, t.WaitsWhat = waitPrecheck, what
}

// clearPrecheckWait clears the wait of task t's checks once they ended.
func (e *Engine) clearPrecheckWait(ctx context.Context, t *registry.Task) {
	if t.WaitsWho == waitPrecheck {
		e.waitOn(ctx, *t, "", "")
		t.WaitsWho, t.WaitsWhat = "", ""
	}
}

// sweepChecks stops the checks of the tasks that are not live (a stop, a
// NEEDS YOU, gone: a stopped check restores the copy's branch itself) and
// forgets their results, so a task back at work checks its head afresh.
func (e *Engine) sweepChecks(tasks []registry.Task) {
	live := map[int64]bool{}
	for _, t := range tasks {
		if isLive(t) {
			live[t.ID] = true
		}
	}
	for id := range e.envCrashed {
		if !live[id] {
			delete(e.envCrashed, id)
		}
	}
	var swept []*headCheck
	e.checksMu.Lock()
	for id := range e.checkFails {
		if !live[id] {
			delete(e.checkFails, id)
		}
	}
	for id, x := range e.checks {
		if live[id] {
			continue
		}
		if !x.ended() {
			x.cancel()
			continue
		}
		delete(e.checks, id)
		delete(e.checkNotes, id)
		swept = append(swept, x)
	}
	e.checksMu.Unlock()
	for _, x := range swept {
		x.end() // its record's write runs outside checksMu
	}
}

// stopCheck stops task t's check (its copy is released): it restores the
// copy's branch itself as it ends.
func (e *Engine) stopCheck(id int64) {
	e.checksMu.Lock()
	defer e.checksMu.Unlock()
	if x := e.checks[id]; x != nil && !x.ended() {
		x.cancel()
	}
}

// shaRe is a commit's sha in a line, short or whole.
var shaRe = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
