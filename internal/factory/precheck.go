package factory

// The precheck (spec 6.4): after the writer's commit, and after each fix of
// the local review, the factory runs the repository's checks on the change
// in its copy, as CI would: lint on the changed lines only (gitx.DiffLines;
// the copy's own configs are CI's), the specs the change affects with a
// baseline at the base (a spec red on the base too is not the change's),
// the related jest tests, and each migration the branch adds up and down,
// on each database of its folder (migration_databases; without it, once,
// db/migrate's only: one of a folder with no databases is named and not
// run), with the migration-safety lint of section 8.4. It runs under the
// machine's spec lock when it runs specs (one precheck at a time on the
// machine runs them; another waits, saying so) and stops at its cap, which
// grows with the change (precheckCap: cap, cap_per_file per file, at most
// cap_max). A cap that stops the specs or jest is a note (Unfinished), no
// failure: the writer cannot fix a time limit, the task goes on, and CI runs
// them. A lint the cap stops hangs, a failure for you. The red examples are
// judged on the base, and the migrations run, whatever the cap: on the
// caller's context, the base run of the red files bounded by a cap grown
// from them as the precheck's is, or by twice the head's run of the specs
// when that is longer (baseBound), each migration command by the
// repository's cap (capOf), since CI never runs a migration down and up.
// Every command goes through the deny list.

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/dblock"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/scrub"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/textx"
)

// precheckFile is the precheck's report in the task's directory: what ran,
// how it ended, the offenses and the red specs, and the migrations with
// their flags (the writer's nudge names it).
const precheckFile = "precheck.md"

// specsLockPoll is how often a check that waits for the machine's spec lock
// tries it again.
const specsLockPoll = 2 * time.Second

// PrecheckResult is a precheck's end: whether it passed, the checks it
// recorded (the factory's), the migrations the branch adds with their
// flags, and why it did not pass (its first failure, in plain words, such
// as "precheck: lint: 1 offense on the changed lines (bin/lint)"). ForYou
// says the writer cannot fix why: a command the deny list refuses or that
// does not start, a lint whose output names no offense by path:line, a
// table_stats it cannot read (the configuration's or the machine's, named by
// its key); such a failure is the one Why says. Unfinished is what its cap
// kept it from, the specs or jest ("the specs did not finish within 30m, and
// the jest tests did not run"; "" when it ran to its end): no failure of its
// own, so a precheck its cap stopped there passes unless something failed
// before (precheck.json keeps it, and the ready card and the PR body name it).
// Command is the command of the failure Why says, as it ran (its files in
// it), when that failure is the writer's to fix: its fix turn asks for a
// run of it until it passes; "" for none.
type PrecheckResult struct {
	Passed     bool
	Checks     []registry.Check
	Migrations []MigrationFlag
	Why        string
	ForYou     bool
	Unfinished string
	Command    string
	// fault is the check that named no failure of the code after its fault
	// steps, when that is the failure for you Why says (faults.go: the
	// stage asks you its question); check and failures are the key and the
	// failures of the writer's failure Why says (failCheck).
	fault    *checkFault
	check    string
	failures []string
	// unnamed: the failures are firstFailing's, the run having named no
	// failing example or offense; your accept of them is the check's alone.
	unnamed bool
}

// errOverCap is the cause of a precheck's context its cap ended.
var errOverCap = errors.New("the precheck's cap")

// precheckCapTail is how many lines of what a command printed on stdout
// before the cap or its bound stopped it precheck.md keeps, and
// precheckErrTail how many of its stderr for a hang (where a tool says what
// it waits for), each read from the stream's last precheckTailWindow bytes.
const (
	precheckCapTail    = 40
	precheckErrTail    = 20
	precheckTailWindow = 64 << 10
)

// hangFix is the fix of a command that hangs (a lint the cap stopped, a
// migration or a base run past its bound).
const hangFix = "fix: find what it waits for in the copy (a lock, a server, a prompt)"

// precheckCap is the cap of a precheck of a change of n files: the
// repository's cap (capOf), cap_per_file more for each file, cap_max at most
// and never below the cap (a repository built without the keys takes their
// defaults, as Load fills them); how says how it was found, as precheck.md
// says it ("30m (10m + 15s × 129 files is 42m15s, at most 30m)"). The
// product is never taken past cap_max, so no cap_per_file wraps it.
func precheckCap(repo config.FactoryRepo, n int) (limit time.Duration, how string) {
	base, per, most := capOf(repo), repo.Precheck.CapPerFile.Duration, repo.Precheck.CapMax.Duration
	if per <= 0 {
		per = config.DefaultPrecheckCapPerFile
	}
	if most <= 0 {
		most = config.DefaultPrecheckCapMax
	}
	most = max(most, base)
	how = capText(base) + " + " + capText(per) + " × " + plural(n, "file")
	if n > 0 && per > (most-base)/time.Duration(n) {
		grown := "more than " + capText(most)
		if per <= (math.MaxInt64-base)/time.Duration(n) {
			grown = capText(base + per*time.Duration(n))
		}
		return most, capText(most) + " (" + how + " is " + grown + ", at most " + capText(most) + ")"
	}
	limit = base + per*time.Duration(n)
	return limit, capText(limit) + " (" + how + ")"
}

// precheck runs task t's precheck in copy c at its head (see the file's
// comment). A precheck that does not pass is a result with Why, nil error;
// the error is a wait (somebody at work in the copy), the caller's context
// ending, a failure to run (git), or a command that crashed on its
// environment (envCrash: its report is written, and the infrastructure hold
// runs the checks again). It started on a clean tree (branchHead),
// and looks at it again after its wait for the spec lock (afterWait: no
// command runs over a change made while it waited), so whatever its commands
// changed in tracked files (Rails dumps db/schema.rb after a migration) is
// its own unless somebody else could have changed the copy since: it puts
// those back as HEAD has them at its end, however it ended, and says so, or
// keeps them for you (keep). It writes precheck.md.
func (e *Engine) precheck(ctx context.Context, t registry.Task, c Copy) (PrecheckResult, error) {
	repo, err := e.factoryRepo(t)
	if err != nil {
		return PrecheckResult{}, err
	}
	if e.d.Git == nil {
		return PrecheckResult{}, heldError{who: "factory", what: "this build has no git"}
	}
	pc := repo.Precheck
	started := e.now() // before any wait: a change after it is the precheck's own, or somebody else's
	bh, err := e.branchHead(ctx, t, c, repo)
	if err != nil {
		return PrecheckResult{}, err
	}
	if bh.Problem != "" {
		return PrecheckResult{Why: "precheck: " + bh.Problem}, nil
	}
	if err := e.copyIdle(ctx, t); err != nil {
		return PrecheckResult{}, err
	}
	diff, err := e.d.Git.DiffLines(ctx, c.Path, bh.Base, bh.Head)
	if err != nil {
		return PrecheckResult{}, err
	}
	var files, added []string
	for _, f := range diff {
		if f.Status == 'D' {
			continue
		}
		files = append(files, f.Path)
		if f.Status == 'A' && isMigration(f.Path) {
			added = append(added, f.Path)
		}
	}
	specs := pc.Specs != "" && !slices.Contains(pc.SkipSpecsForKinds, t.Kind)
	if specs {
		unlock, err := e.specsLock(ctx, t, "precheck")
		if err != nil {
			return PrecheckResult{}, err
		}
		defer unlock()
		// No command runs over what changed while it waited (afterWait).
		if started, err = e.afterWait(ctx, t, c, started, "check", "the precheck"); err != nil {
			if k, ok := errors.AsType[keptChanges](err); ok {
				return PrecheckResult{Why: "precheck: " + k.why, ForYou: true}, nil
			}
			return PrecheckResult{}, err
		}
	}

	limit, how := precheckCap(repo, len(files))
	cctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stop := make(chan struct{})
	defer close(stop)
	timer := e.timer(limit)
	go func() {
		select {
		case <-timer:
			cancel(errOverCap)
		case <-stop:
		}
	}()

	p := &precheckRun{e: e, t: t, c: c, repo: repo, head: bh.Head, base: bh.Base, ctx: cctx, parent: ctx, limit: limit, started: started,
		res: PrecheckResult{Passed: true}}
	p.readDecisions()
	p.files = files
	defer func() {
		for _, cancel := range p.renewed {
			cancel(nil)
		}
	}()
	p.say("# Precheck of %s at %s\n\nThe change from %s: %d files.\n\nCap: %s.\n", t.Branch, short(bh.Head), short(bh.Base), len(files), how)
	steps := []precheckStep{
		{"the lint", len(pc.Lint) > 0 && len(files) > 0, false, func() { p.lint(pc.Lint, files, diff) }},
		{"the specs", specs && len(files) > 0, false, func() {
			switch {
			case specs:
				p.specs(pc.Specs, files)
			case pc.Specs != "":
				p.say("\n## Specs\n\nskipped: a %s runs no specs (skip_specs_for_kinds)\n", t.Kind)
				p.record(registry.Check{Kind: registry.CheckSpec, Result: registry.CheckSkipped, Command: pc.Specs})
			}
		}},
		{"the jest tests", pc.Jest != "" && len(files) > 0, false, func() { p.jest(pc.Jest, files) }},
		{"the migrations", len(added) > 0, true, func() { p.migrations(repo, pc.Migrations, added) }},
	}
	notRun := p.runSteps(steps)
	// What its commands changed goes back, guarded, however it ended. A copy
	// a base run left detached (its record is there) is resumeDetached's: the
	// precheck is a wait then, whatever else failed, so the task is never at
	// NEEDS YOU with its copy left detached, and the next check finds the rest
	// again.
	leftDetached := p.err != nil && e.hasDetachedRecord(t)
	if !leftDetached && !p.kept {
		p.restoreOwn()
	}
	// The cap stopped it: what it did not finish or start is a note, and no
	// failure of its own (CI runs the specs and jest).
	if (errors.Is(context.Cause(cctx), errOverCap) || p.overCap) && (p.cut != "" || len(notRun) > 0) {
		p.res.Unfinished = unfinishedWords(p.cut, notRun, capText(limit))
		p.say("\n**Stopped: over its cap of %s.**\n", capText(limit))
	}
	var crashed error // a crash of the environment: its report is written, and it is the precheck's error
	switch crash, isCrash := errors.AsType[envCrash](p.err); {
	case ctx.Err() != nil:
		return p.res, ctx.Err()
	case leftDetached && p.err != nil:
		return p.res, p.err
	case p.kept: // what it kept for you is its result, whatever failed to run before it (kept.json records it)
	case p.restoreErr != nil:
		return p.res, p.restoreErr
	case p.res.ForYou:
	case isCrash:
		p.res.Passed, p.res.Why, crashed = false, "precheck: "+crash.Error(), crash
	case p.err != nil:
		return p.res, p.err
	}
	switch {
	case p.res.Passed && p.res.Unfinished != "":
		p.say("\nPassed, unfinished: %s. The task goes on, and CI is their only run.\n", p.res.Unfinished)
	case p.res.Passed:
		p.say("\nPassed.\n")
	default:
		p.say("\nFailed: %s\n", p.res.Why)
		if p.res.Unfinished != "" {
			p.say("\nUnfinished: %s.\n", p.res.Unfinished)
		}
	}
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return p.res, err
	}
	if err := writeFile(filepath.Join(dir, precheckFile), p.report.String()); err != nil {
		return p.res, err
	}
	rec := precheckRecord{Head: p.head, Passed: p.res.Passed, Why: p.res.Why, Unfinished: p.res.Unfinished,
		MigrationsNotRun: migrationsNotRun(p.res.Migrations)}
	if p.res.ForYou {
		rec.ForYou, rec.Line = true, p.line
		rec.Commands, rec.Env = e.checksPrint(repo)
	}
	if err := writeJSON(filepath.Join(dir, precheckRecordFile), rec); err != nil {
		return p.res, err
	}
	return p.res, crashed
}

// precheckStep is a step of the precheck: its name, as a precheck its cap
// stopped says it; whether it runs a command; own: it runs on a bound of
// its own (ownBound), so the cap neither cuts it nor keeps it from running.
type precheckStep struct {
	name      string
	runs, own bool
	run       func()
}

// runSteps runs the precheck's steps in order, until it stops (a step on a
// bound of its own still runs after the cap): notRun are the steps with a
// command that its end kept from starting (the one whose command its cap
// stopped is p.cut).
func (p *precheckRun) runSteps(steps []precheckStep) (notRun []string) {
	for _, step := range steps {
		switch {
		case step.own && !p.halted():
			if p.stopped() && step.runs {
				p.say("\nThe precheck's cap ran out before %s; they run anyway, each command bounded by %s (CI never runs a migration down and up).\n",
					step.name, capText(capOf(p.repo)))
			}
			p.step = step.name
			p.ownBound(step.run)
		case p.stopped():
			if step.runs {
				notRun = append(notRun, step.name)
			}
		default:
			p.step = step.name
			step.run()
		}
	}
	return notRun
}

// unfinishedWords say what a precheck its cap stopped did not finish: the
// step whose command it stopped (cut, "" for none), then the steps it kept
// from starting (notRun), which did not run.
func unfinishedWords(cut string, notRun []string, limit string) string {
	switch {
	case cut != "" && len(notRun) > 0:
		return cut + " did not finish within " + limit + ", and " + andList(notRun) + " did not run"
	case cut != "":
		return cut + " did not finish within " + limit
	}
	return andList(notRun) + " did not run within the cap of " + limit
}

// precheckUnfinishedAt is what the last precheck in task directory dir did
// not finish, when it passed at head ("" for anything else): the ready
// card's FLAGS line and the PR body name it.
func precheckUnfinishedAt(dir, head string) string {
	r, _ := precheckPassedRecord(dir, head)
	return r.Unfinished
}

// precheckPassedRecord is precheck.json in task directory dir when its
// precheck passed at head.
func precheckPassedRecord(dir, head string) (precheckRecord, bool) {
	var r precheckRecord
	b, err := os.ReadFile(filepath.Join(dir, precheckRecordFile))
	if err != nil || json.Unmarshal(b, &r) != nil || !r.Passed || head == "" || r.Head != head {
		return precheckRecord{}, false
	}
	return r, true
}

// precheckRecordFile is precheck.json: the end of the task's last precheck
// that ran to its end, at its head, which the ready card and the publish
// read (precheckPassedAt) once the check's result in memory is gone.
const precheckRecordFile = "precheck.json"

// precheckRecord is precheck.json. A failure for you also records the line
// its command printed (Line, for a lint that crashed), and what it ran
// with (Commands, the repository's precheck commands, and Env, the
// environment they ran in, both as checksPrint's hashes, never a value): a
// retry that changed none of it, at the same head, says so (retryTask).
type precheckRecord struct {
	Head     string `json:"head_sha"`
	Passed   bool   `json:"passed"`
	Why      string `json:"why,omitempty"`
	ForYou   bool   `json:"for_you,omitempty"`
	Line     string `json:"line,omitempty"`
	Commands string `json:"commands,omitempty"`
	Env      string `json:"env,omitempty"`
	// Unfinished is the passed precheck's note of what its cap kept it
	// from (PrecheckResult.Unfinished).
	Unfinished string `json:"unfinished,omitempty"`
	// MigrationsNotRun are the migrations it named and did not run, since no
	// databases are set for their folders (MigrationFlag.NoDatabases).
	MigrationsNotRun []string `json:"migrations_not_run,omitempty"`
}

// checksPrint is what the precheck commands of repo run with, as two
// hashes: commands, of its precheck commands as configured (with
// migration_databases, when set), and env, of the environment they run in
// (the copies' env of the configuration, and the daemon's locale and PATH,
// which every copy command inherits). They change when you edit either and
// restart the daemon on it, or your shell's PATH; never a value, only its
// hash, is kept.
func (e *Engine) checksPrint(repo config.FactoryRepo) (commands, env string) {
	pc := repo.Precheck
	cmds := slices.Concat(pc.Lint, []string{pc.Specs, pc.Jest}, pc.Migrations)
	for _, folder := range slices.Sorted(maps.Keys(pc.MigrationDatabases)) { // none: the print it had before the key
		cmds = append(cmds, "migration_databases "+folder+"="+strings.Join(pc.MigrationDatabases[folder], ","))
	}
	commands = sha256Hex([]byte(strings.Join(cmds, "\x00")))
	var vars []string
	for _, k := range slices.Sorted(maps.Keys(repo.Env)) {
		vars = append(vars, k+"="+repo.Env[k])
	}
	for _, k := range []string{"LANG", "LC_ALL", "LC_CTYPE", "PATH"} {
		vars = append(vars, "daemon "+k+"="+e.getenv(k))
	}
	return commands, sha256Hex([]byte(strings.Join(vars, "\x00")))
}

// precheckPassedAt reports whether the last precheck in task directory dir
// passed at head.
func precheckPassedAt(dir, head string) bool {
	_, ok := precheckPassedRecord(dir, head)
	return ok
}

// timer is the precheck cap's timer (the after seam, else time.After).
func (e *Engine) timer(d time.Duration) <-chan time.Time {
	if e.after != nil {
		return e.after(d)
	}
	return time.After(d)
}

// precheckRun is a precheck under way.
type precheckRun struct {
	e          *Engine
	t          registry.Task
	c          Copy
	head, base string
	// ctx is the precheck's context, which its cap ends (a step on a bound
	// of its own swaps it: ownBound), and parent the caller's, which the cap
	// never ends; limit bounds each command.
	ctx, parent context.Context
	limit       time.Duration
	res         PrecheckResult
	report      strings.Builder
	// err is a failure to run that ends the precheck (git, a start failure
	// of the moment); overCap a command the cap or its bound stopped
	// (capStopped); step the step under way and cut the one whose command
	// the cap stopped, a note's (the specs, jest); started when it began on
	// a clean tree, before its waits, or when it looked at the tree again
	// after them (afterWait); restoreErr a failed put-back of its own changes
	// (restoreOwn), after which it runs nothing more.
	err        error
	overCap    bool
	step, cut  string
	started    time.Time
	restoreErr error
	// kept: it left changes for you (keep); it runs nothing more and puts
	// nothing back after that.
	kept bool
	// stderr and exit are what the command it ran last wrote to stderr, as
	// it wrote it (the lint's ask names its last line, redacted), and its
	// exit; line is that line of the lint whose failure for you is the
	// precheck's (precheck.json).
	stderr, line string
	exit         int
	// repo is the task's repository; hung: the lint command it ran last did
	// not end within its bound (capStopped), a fault for its steps.
	repo config.FactoryRepo
	hung bool
	// stops are your decisions for the task's checks (stops.json: the ones
	// skipped, accepted, their bounds), faults the fault steps spent at each
	// head (check-faults.json); files the paths the branch changed; renewed
	// the cancels of the caps renewCap gave the steps after a fault.
	stops   stopsRecord
	faults  faultsRecord
	files   []string
	renewed []context.CancelCauseFunc
}

// readDecisions reads your decisions for the task's checks (stops.json)
// and the fault steps spent (check-faults.json); one that cannot be read
// is logged and counts as none: the check runs, and its steps are taken.
func (p *precheckRun) readDecisions() {
	dir := p.e.d.Layout.FactoryTaskDir(p.t.ID)
	var err error
	if p.stops, err = readStops(dir); err != nil {
		p.e.log.Warn("read your decisions for the task's checks", "task", p.t.ID, "err", err)
	}
	if p.faults, err = readFaults(dir); err != nil {
		p.e.log.Warn("read the task's check fault steps", "task", p.t.ID, "err", err)
		p.faults = faultsRecord{}
	}
}

// failCheck fails the precheck with why, the writer's to fix, as fail
// does, naming the check (key) and its failures (the parsed ones, else its
// first failing line) for the fix turns' progress (fix_go_on.go); a failure
// of a check your answer accepted as known for the task is a note instead.
func (p *precheckRun) failCheck(key string, failures []string, why, cmd string) {
	p.failChecked(key, failures, false, why, cmd)
}

// failUnnamed is failCheck of a run that named no failing example or
// offense: its failure is the line firstFailing reads, and your accept of
// it holds for the check alone (unnamed).
func (p *precheckRun) failUnnamed(key string, run ProofRun, why, cmd string) {
	p.failChecked(key, firstFailing(run), true, why, cmd)
}

// failChecked is failCheck, unnamed for a failure read by firstFailing.
func (p *precheckRun) failChecked(key string, failures []string, unnamed bool, why, cmd string) {
	if p.stops.accepts(key, failures, unnamed) {
		p.say("\nAccepted as known for this task, on your answer to its question: %s.\n", why)
		return
	}
	if p.res.Passed {
		p.res.check, p.res.failures, p.res.unnamed = key, failures, unnamed
	}
	p.fail(why, cmd)
}

// restoreOwn puts back what the precheck's commands changed so far
// (Engine.restoreOwn, guarded), and says so in the report: kept changes
// (somebody else could have made them) fail the precheck for you, naming
// them whatever failed before (keep); a failed put-back is restoreErr. It
// reports whether the tree is now as the branch has it.
func (p *precheckRun) restoreOwn() bool {
	restored, kept, err := p.e.restoreOwn(p.ctx, p.t, p.c, p.started, "the precheck's commands")
	switch {
	case kept != "":
		p.say("\n**Left as it is: %s.**\n", kept)
		p.keep(kept)
		return false
	case err != nil:
		p.say("\n**The put-back failed: %s.**\n", err)
		if p.restoreErr == nil {
			p.restoreErr = err
		}
		return false
	case len(restored) > 0:
		p.say("\nRestored %s: the precheck's commands changed %s, and the copy is as the branch has it.\n",
			clipList(restored), map[bool]string{true: "it", false: "them"}[len(restored) == 1])
	}
	return true
}

func (p *precheckRun) say(format string, a ...any) { fmt.Fprintf(&p.report, format, a...) }

// stopped reports whether the precheck ends here: its cap, the caller's
// context, a failure to run, changes it left for you, or a put-back of its
// own that failed (no command runs on a tree it could not put back).
func (p *precheckRun) stopped() bool {
	return p.ctx.Err() != nil || p.err != nil || p.overCap || p.kept || p.restoreErr != nil
}

// halted reports whether the precheck ends here for anything but its cap:
// a step on a bound of its own still runs after the cap.
func (p *precheckRun) halted() bool {
	return p.parent.Err() != nil || p.err != nil || p.kept || p.restoreErr != nil
}

// ownBound runs step on the caller's context (never the precheck's, which
// its cap ends), each of its commands bounded by the repository's cap
// (capOf) on its own: the precheck's cap neither cuts it nor keeps it from
// running, and no deadline of the whole step cuts a healthy command late in
// a step of many (a migration on several shards).
func (p *precheckRun) ownBound(step func()) {
	saved, limit, over := p.ctx, p.limit, p.overCap
	p.ctx, p.limit, p.overCap = p.parent, capOf(p.repo), false
	defer func() { p.ctx, p.limit, p.overCap = saved, limit, over || p.overCap }()
	step()
}

// keep fails the precheck for you with why, changes it left for you, and
// ends it: it runs nothing more and puts nothing back. The changes are named
// whatever failed before: a failure for you that came first stays first, and
// why follows it.
func (p *precheckRun) keep(why string) {
	p.kept = true
	if p.res.ForYou {
		p.res.Why += "; and " + why
		return
	}
	p.failForYou(why)
}

// fail fails the precheck with why, its first failure's reason, unless a
// failure for you came first.
func (p *precheckRun) fail(why, cmd string) {
	if p.res.Passed {
		p.res.Passed, p.res.Why, p.res.Command = false, "precheck: "+why, cmd
	}
}

// failForYou fails the precheck with why, a failure the writer cannot fix:
// it is the one the result says, whatever failed before it.
func (p *precheckRun) failForYou(why string) {
	if !p.res.ForYou {
		p.res.Passed, p.res.Why, p.res.ForYou = false, "precheck: "+why, true
	}
}

// record records c as a check of the head by the factory.
func (p *precheckRun) record(c registry.Check) {
	c.TaskID, c.Source = p.t.ID, "factory"
	if c.HeadSHA == "" {
		c.HeadSHA = p.head
	}
	p.res.Checks = append(p.res.Checks, c)
	p.e.recordCheck(p.ctx, c)
}

// run runs a command of the precheck, of configuration key key, in the
// copy: its run, whether it ran to its end (false: the cap or the caller's
// context stopped it, or it did not run at all: refused by the deny list or
// not started, which fails the precheck for you, naming the key).
func (p *precheckRun) run(key, what, cmd string) (ProofRun, bool) {
	p.e.noteCheck(p.t.ID, "precheck: "+what)
	run := ProofRun{Command: cmd}
	start := p.e.now() // the engine's clock, as the QA run's: its time sets the base run's bound (baseBound)
	var res execx.Result
	var waited time.Duration // for the copy's databases: no part of the run's own time
	var err error
	if dbKeys[key] { // the copy's databases: in turn with the local review's reviewers (db-lock)
		res, waited, err = p.e.dbScript(p.ctx, p.c, cmd, "precheck "+what, p.limit, func(h dblock.Holder) {
			p.e.noteCheck(p.t.ID, "precheck: "+what+" waits for the copy's databases ("+h.String()+")")
		})
	} else {
		res, err = p.e.script(p.ctx, p.c, cmd, "precheck "+what, p.limit)
	}
	run.Took = (p.e.now().Sub(start) - waited).Round(time.Millisecond)
	code, denied, err := ranTo(res, err)
	run.Exit, run.Output = code, outputOf(res)
	run.out, run.errOut = lastBytes(res.Stdout, precheckTailWindow), lastBytes(res.Stderr, precheckTailWindow)
	p.stderr, p.exit = string(res.Stderr), code
	switch {
	case err != nil && p.parent.Err() != nil: // the caller's context: the daemon stops, or the head moved
		return run, false
	case err != nil && (p.ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded)): // the cap, or a bound
		p.capStopped(key, what, cmd, run) // it keeps what it printed before (execx)
		return run, false
	case denied != "":
		p.say("\n- `%s`: denied: %s\n", cmd, denied)
		p.failForYou(key + " `" + cmd + "` is denied: " + denied + deniedFix(key))
		return run, false
	case err != nil:
		why, transient := startFailure(err)
		p.say("\n- `%s` %s\n", cmd, why)
		if transient { // the machine's, of the moment: the checks run again
			p.err = fmt.Errorf("precheck %s: %s %w", what, cmd, err)
			return run, false
		}
		p.failForYou(key + " `" + cmd + "` " + why + runnableFix(key, cmd))
		return run, false
	case notRunnable(code) != "":
		p.say("\n- `%s` %s\n", cmd, notRunnable(code))
		p.failForYou(key + " `" + cmd + "` " + notRunnable(code) + runnableFix(key, cmd))
		return run, false
	}
	return run, true
}

// capKinds are the kinds of the checks of the precheck's commands, by
// configuration key.
var capKinds = map[string]string{"precheck.lint": registry.CheckLint, "precheck.specs": registry.CheckSpec,
	"precheck.jest": registry.CheckSpec, "precheck.migrations": registry.CheckMigration}

// capStopped notes that the precheck's cap or a step's own bound (p.limit)
// stopped run, the command what (cmd) of configuration key key: its check
// is recorded as skipped (the registry keeps a cause for a failure only),
// and precheck.md keeps the end of what it printed on stdout, where it
// stopped (sayTail). A stop in the specs or jest is the precheck's note
// (cut): CI runs them. A stop in the lint or the migrations hangs, a failure
// for you, never the writer's (a lint that takes the whole cap, the first
// step, waits on something: a lock, a server; CI never runs a migration
// down and up), and precheck.md keeps the end of its stderr too, where a
// tool says what it waits for.
func (p *precheckRun) capStopped(key, what, cmd string, run ProofRun) {
	p.overCap = true
	p.record(registry.Check{Kind: capKinds[key], Result: registry.CheckSkipped, Command: run.Command, Exit: run.Exit, Took: run.Took})
	p.say("\nIts run of %s did not end within %s, and the factory stopped it.\n", what, capText(p.limit))
	p.sayTail("stdout", run.out, precheckCapTail)
	switch key {
	case "precheck.specs", "precheck.jest":
		if p.cut == "" {
			p.cut = p.step
		}
	case "precheck.lint": // a fault: its steps run it again (lintCmd), and settleLint asks you
		p.sayTail("stderr", run.errOut, precheckErrTail)
		p.hung = true
	default:
		p.sayTail("stderr", run.errOut, precheckErrTail)
		p.failForYou(key + " `" + textx.Clip(cmd, 120) + "` did not end within " + capText(p.limit) + ": it hangs; " + hangFix)
	}
}

// sayTail writes to precheck.md the last n lines of b, what a stopped
// command printed on stream (streamTail), in a fence they cannot close.
func (p *precheckRun) sayTail(stream string, b []byte, n int) {
	tail := streamTail(b, n)
	if tail == "" {
		p.say("\nIt printed nothing on %s.\n", stream)
		return
	}
	fence := fenceFor(tail)
	p.say("\nThe last lines it printed on %s (at most %d):\n\n%s\n%s\n%s\n", stream, n, fence, tail, fence)
}

// streamTail is the last n lines of b, a stream of a stopped command, read
// from its last precheckTailWindow bytes from a line's start, without what
// a terminal acts on (StripTerminal, first, so no escape splits a secret the
// scrub must see) and scrubbed (precheck.md is kept).
func streamTail(b []byte, n int) string {
	s := string(lastBytes(b, precheckTailWindow))
	if len(b) > precheckTailWindow {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	return lastLines(scrub.Text(textx.StripTerminal(s)), n)
}

// lastBytes is the end of b, n bytes at most.
func lastBytes(b []byte, n int) []byte { return b[max(0, len(b)-n):] }

// lastLines are the last n lines of s, without its trailing blank lines.
func lastLines(s string, n int) string {
	s = strings.TrimRight(s, "\n\t ")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}

// fenceFor is a Markdown code fence no line of s closes: one backtick more
// than the longest run of them in s, three at least.
func fenceFor(s string) string {
	longest, run := 0, 0
	for i := range len(s) {
		if s[i] != '`' {
			run = 0
			continue
		}
		run++
		longest = max(longest, run)
	}
	return strings.Repeat("`", max(3, longest+1))
}

// runnableFix is the fix of a command of key that is missing or not
// executable (cmd its script): its first word made runnable, then the
// factory daemon restarted on the PATH or the configuration that runs it.
func runnableFix(key, cmd string) string {
	return "; fix: make `" + firstWord(cmd) + "` runnable in the copy (install it, put its directory on your login shell's PATH, " +
		"or change " + key + "), then `magnum factory install`"
}

// deniedFix is the fix of a command of key the deny list refuses: another
// command in the configuration, which the daemon reads at its start.
func deniedFix(key string) string {
	return "; fix: change " + key + " in its [[factory.repo]] (the factory never runs that), then `magnum factory install`"
}

// problemFix is the fix of what keeps command cmd of key from running at
// all (specRun's problem: denied, or missing or not executable).
func problemFix(key, cmd, problem string) string {
	if strings.HasPrefix(problem, "is denied") {
		return deniedFix(key)
	}
	return runnableFix(key, cmd)
}

// dbKeys are the precheck's commands that use the copy's databases: they
// run under its database lock (dbScript).
var dbKeys = map[string]bool{"precheck.specs": true, "precheck.migrations": true}

// dbScript is script under copy c's database lock, the one `magnum db-lock`
// takes for the roles of a local round (Layout.DBLock of the copy): the
// factory's spec and migration commands and the reviewers never use the
// copy's databases at once. It waits for the lock within timeout, waiting
// told who holds it, and waited is how long, on the engine's clock (no part
// of the command's own time); a lock that stays held is a command that did
// not start, which the checks try again.
func (e *Engine) dbScript(ctx context.Context, c Copy, script, label string, timeout time.Duration,
	waiting func(dblock.Holder)) (_ execx.Result, waited time.Duration, _ error) {
	began := e.now()
	lock, err := dblock.Acquire(ctx, e.d.Layout.DBLock(c.Path), dblock.Holder{Role: "factory", Command: label, PID: os.Getpid()}, timeout, waiting)
	waited = e.now().Sub(began)
	if err != nil {
		return execx.Result{Code: -1}, waited, fmt.Errorf("the copy's database lock: %w", err)
	}
	defer lock.Release()
	res, err := e.script(ctx, c, script, label, timeout)
	return res, waited, err
}

// offenseRe is a lint offense line: path:line[:column][:] message.
var offenseRe = regexp.MustCompile(`^(?:\./)?([^\s:]+):(\d+)(?::\d+)?:?\s+(.*)$`)

// lint runs each lint command on the changed files and counts what it says
// about the changed lines only: a command that exits non-zero fails the
// precheck when it names an offense on a changed line, and passes when
// every offense it names is on a line the change did not touch. A run that
// names no failure of the code (it exits non-zero and names no offense by
// path:line, or does not end within its bound) takes the fault steps first
// (faults.go), then is settled (settleLint). A command your answer skipped
// does not run.
func (p *precheckRun) lint(cmds []string, files []string, diff []gitx.FileLines) {
	if len(cmds) == 0 || len(files) == 0 {
		return
	}
	changed := changedLines(diff)
	p.say("\n## Lint (the changed lines only)\n")
	for _, cmd := range cmds {
		if p.stopped() {
			return
		}
		p.lintCmd(cmd, files, changed)
	}
}

// lintRun is one run of a lint command (lintOnce): its run, whether it ran
// to its end (ran) or its bound stopped it (hung), the offenses it named on
// the changed lines (mine) and in all (named), the crash of the
// environment its stderr tells (crash, isCrash) with the changed files the
// crash names or the lint loads (theirs), the bound it ran with, and the
// sign that it names a broken environment (faultSign; "" for none).
type lintRun struct {
	run       ProofRun
	ran, hung bool
	mine      []string
	named     int
	crash     envCrash
	isCrash   bool
	theirs    []string
	bound     time.Duration
	sign      string
}

// fault reports whether r named no failure of the code: it hung, or it
// exited non-zero naming no offense with a positive sign of a broken
// environment (faultSign). One that names no offense and no such sign (an
// output the factory cannot read) is no fault: its format is your
// question, as before the fault steps.
func (r lintRun) fault() bool {
	return r.hung || (r.ran && r.run.Exit != 0 && r.named == 0 && r.sign != "")
}

// lintCmd runs lint command cmd on files (lint's): once, then its fault
// steps while it names no failure of the code (again, after the
// repository's installs, and with its bound doubled when it hangs), each
// once at the head; then its last run is settled (settleLint).
func (p *precheckRun) lintCmd(cmd string, files []string, changed map[string]map[int]bool) {
	key := lintKey(cmd)
	script := renderCommand(cmd, files, p.base)
	if p.stops.skipped(key) {
		p.skipCheck(registry.CheckLint, key, script)
		return
	}
	r := p.lintOnce(cmd, script, files, changed, p.stops.bound(key))
	var tried []string
	for r.fault() && !p.halted() && !p.toWriter(key) {
		step := p.nextFaultStep(key, r.hung)
		if step == "" {
			break
		}
		bound := r.bound
		if step == stepHeal {
			if failed := p.healFault(key); failed != "" {
				p.spendFaultStep(key, step, "ran the repository's installs", "the installs failed: "+failed)
				tried = append(tried, "ran the repository's installs, and the installs failed: "+failed)
				break
			}
		}
		if step == stepLonger {
			longer, ok := p.longerBound(bound)
			if !ok {
				p.spendFaultStep(key, step, "had no longer bound to run it with", "its bound is precheck.cap_max already")
				break
			}
			bound = longer
		}
		p.recordFaultRun(r, script)
		r = p.lintOnce(cmd, script, files, changed, bound)
		words := faultStepWords(step, bound, p.repo.Installs)
		tried = append(tried, words)
		p.spendFaultStep(key, step, words, lintEnded(r))
	}
	if p.overCap && !r.fault() && r.ran && !p.halted() { // its hang spent the cap, and it ran to its end after: the rest gets a cap of its own
		p.renewCap()
	}
	p.settleLint(cmd, script, r, tried)
}

// renewCap gives the precheck's steps after a check whose hang spent the
// cap, and whose run again then ended, a cap of their own (the precheck's
// cap again, on the cap's timer seam): the specs and jest run, never left
// unrun with a note because a fault took their time.
func (p *precheckRun) renewCap() {
	cctx, cancel := context.WithCancelCause(p.parent)
	timer := p.e.timer(p.limit)
	go func() {
		select {
		case <-timer:
			cancel(errOverCap)
		case <-cctx.Done():
		}
	}()
	p.say("\nIts hang spent the precheck's cap, and its run again ended: the steps after it get a cap of %s of their own.\n",
		capText(p.limit))
	p.ctx, p.overCap = cctx, false
	p.renewed = append(p.renewed, cancel)
}

// lintEnded says how lint run r ended, for its fault step's event.
func lintEnded(r lintRun) string {
	switch {
	case r.hung:
		return "it did not end within " + capText(r.bound) + " again"
	case !r.ran:
		return "it did not run"
	case r.fault():
		return fmt.Sprintf("it exited %d and named no offense again", r.run.Exit)
	}
	return "this run named what it found"
}

// recordFaultRun records lint run r of script, which named no failure of
// the code and runs again, as a failed check (its cause the crash's, else
// unknown); a hang's check its stop recorded already (capStopped).
func (p *precheckRun) recordFaultRun(r lintRun, script string) {
	if r.hung || !r.ran {
		return
	}
	cause := "unknown"
	if r.isCrash {
		cause = "env"
	}
	p.record(registry.Check{Kind: registry.CheckLint, Result: registry.CheckFail, Cause: cause, Command: script, Exit: r.run.Exit, Took: r.run.Took})
}

// lintOnce runs lint script (of command cmd, on files) once, on the
// precheck's cap, or on a bound of its own when bound is set (a rerun, or
// your answer's bound), and reads what it says (lintRun).
func (p *precheckRun) lintOnce(cmd, script string, files []string, changed map[string]map[int]bool, bound time.Duration) lintRun {
	var r lintRun
	run := func() { r.run, r.ran = p.run("precheck.lint", "lint ("+firstWord(cmd)+")", script) }
	if bound > 0 {
		p.onOwnBound(bound, run)
	} else {
		run()
	}
	r.bound, r.hung = cmp.Or(bound, p.limit), p.hung
	p.hung = false
	if !r.ran {
		return r
	}
	for _, o := range namedOffenses(r.run.Output) {
		r.named++
		file := strings.TrimPrefix(strings.TrimPrefix(o.file, p.c.Path+"/"), "./")
		if changed[file][o.line] {
			r.mine = append(r.mine, o.text)
		}
	}
	if r.run.Exit != 0 && r.named == 0 {
		stdout, _ := runText(r.run)
		if r.crash, r.isCrash = crashOf("precheck.lint", cmd, p.c.Path, p.stderr); r.isCrash {
			r.theirs = branchCaused(r.crash, p.stderr, files)
		}
		r.sign = faultSign(r.run.Exit, stdout, p.stderr, r.crash, r.isCrash, cmd)
		if r.sign != "" && !r.isCrash { // any other sign is the branch's too when it touched what the lint loads, or its output names the change
			r.theirs = branchCaused(envCrash{cmd: cmd}, stdout+"\n"+p.stderr, files)
		}
	}
	return r
}

// namedOffense is an offense a lint names: its file as the lint printed it,
// its line, and its words (path:line and the rest).
type namedOffense struct {
	file string
	line int
	text string
}

// lintRowRe is a row of a lint's report under its file's line: eslint's
// stylish and stylelint's "  1:3  ✖  Empty block  block-no-empty".
var lintRowRe = regexp.MustCompile(`^\s+(\d+):\d+\s+\S`)

// lintFileRe is the line of a report that names the file its rows are of:
// a path alone, not indented.
var lintFileRe = regexp.MustCompile(`^[^\s:]+\.[\w.]+$`)

// namedOffenses are the offenses output names: each path:line line
// (offenseRe), and each row of line:column under the line that names its
// file (lintFileRe, lintRowRe: eslint's stylish, stylelint's report), whose
// file is that line's. A row with no such line above it names no file.
func namedOffenses(output string) []namedOffense {
	var out []namedOffense
	file := ""
	for line := range strings.SplitSeq(textx.StripTerminal(output), "\n") {
		trimmed := strings.TrimSpace(line)
		if m := lintRowRe.FindStringSubmatch(line); m != nil {
			if file != "" {
				n, _ := strconv.Atoi(m[1])
				out = append(out, namedOffense{file: file, line: n, text: file + ":" + trimmed})
			}
			continue
		}
		if m := offenseRe.FindStringSubmatch(trimmed); m != nil && strings.Trim(m[1], "0123456789") != "" {
			n, _ := strconv.Atoi(m[2])
			out = append(out, namedOffense{file: m[1], line: n, text: trimmed})
			continue
		}
		switch {
		case lintFileRe.MatchString(line):
			file = trimmed
		case trimmed == "": // a blank line ends a file's rows
			file = ""
		}
	}
	return out
}

// onOwnBound runs fn on a context of the caller's that its own timer ends
// after bound (the cap's timer seam: no cap of the precheck's cuts it), each
// command bounded by bound, as ownBound does with the repository's cap.
func (p *precheckRun) onOwnBound(bound time.Duration, fn func()) {
	bctx, cancel := context.WithCancelCause(p.parent)
	stop := make(chan struct{})
	timer := p.e.timer(bound)
	go func() {
		select {
		case <-timer:
			cancel(errOverCap)
		case <-stop:
		}
	}()
	saved, limit, over := p.ctx, p.limit, p.overCap
	p.ctx, p.limit, p.overCap = bctx, bound, false
	defer func() {
		close(stop)
		cancel(nil)
		p.ctx, p.limit, p.overCap = saved, limit, over || p.overCap
	}()
	fn()
}

// settleLint records lint run r of script (command cmd), its fault steps
// tried, as the precheck's: a pass, the writer's offenses on the changed
// lines, a crash of the machine's (the infrastructure hold runs the checks
// again) or the branch's (the writer's to fix), and a run that still names
// no failure of the code, which asks you (faultForYou).
func (p *precheckRun) settleLint(cmd, script string, r lintRun, tried []string) {
	key := lintKey(cmd)
	check := registry.Check{Kind: registry.CheckLint, Result: registry.CheckPass, Command: script, Exit: r.run.Exit, Took: r.run.Took}
	switch {
	case r.hung && p.toWriter(key): // your answer handed its faults to the writer
		p.failCheck(key, []string{"did not end within " + capText(r.bound)}, "lint: `"+firstWord(cmd)+"` did not end within "+
			capText(r.bound), script)
		return
	case r.hung: // its stop recorded its check (capStopped)
		p.faultForYou(checkFault{key: key, skip: key, what: "did not end within " + capText(r.bound), hang: true, bound: r.bound,
			tried: tried, tail: streamTail(r.run.errOut, precheckErrTail)},
			"precheck.lint `"+textx.Clip(script, 120)+"` did not end within "+capText(r.bound)+": it hangs; "+hangFix)
		return
	case !r.ran:
		if !p.stopped() {
			p.record(registry.Check{Kind: registry.CheckLint, Result: registry.CheckFail, Cause: "code", Command: script, Exit: r.run.Exit, Took: r.run.Took})
		}
		return
	case r.run.Exit == 0:
		p.say("\n- `%s`: passed\n", script)
	case len(r.mine) > 0:
		check.Result, check.Cause = registry.CheckFail, "code"
		p.say("\n- `%s`: %s on the changed lines:\n\n```\n%s\n```\n", script, plural(len(r.mine), "offense"), strings.Join(r.mine, "\n"))
		p.failCheck(key, r.mine, fmt.Sprintf("lint: %s on the changed lines (%s)", plural(len(r.mine), "offense"), firstWord(cmd)), script)
	case r.named == 0:
		check.Result, check.Cause = registry.CheckFail, "unknown"
		p.say("\n- `%s` exited %d and names no offense by path:line:\n\n```\n%s\n```\n", script, r.run.Exit, r.run.Output)
		check.Cause = p.lintNoOffense(cmd, script, r, tried)
	default:
		p.say("\n- `%s`: %s, none on a changed line\n", script, plural(r.named, "offense"))
	}
	p.record(check)
}

// lintNoOffense settles a lint run r (of script, command cmd) that exited
// non-zero naming no offense, after its fault steps (tried), and returns
// its check's cause:
//   - a crash the machine's environment caused (a locale) is the
//     precheck's error: the infrastructure hold runs the checks again
//     (provenHead), at most envCrashTries times at one head;
//   - a crash the change can be the cause of (it changed what the lint
//     loads, or its stderr names a changed file: branchCaused) is the
//     writer's to fix;
//   - anything else names no failure of the code, which asks you
//     (faultForYou), with the crash's fix when a known crash tells why.
//
// Neither holds another task.
func (p *precheckRun) lintNoOffense(cmd, script string, r lintRun, tried []string) string {
	crash := r.crash
	switch {
	case r.isCrash && crash.sig.machine:
		p.say("\nIt crashed for lack of %s: the infrastructure hold runs the checks again at its next try.\n", crash.sig.lacks)
		p.err = crash
		return "env"
	case r.isCrash && len(r.theirs) > 0:
		p.say("\nIt crashed for lack of %s, and the change touches %s, which the lint loads: the writer's to fix.\n",
			crash.sig.lacks, clipList(r.theirs))
		why := fmt.Sprintf("lint: `%s` crashed for lack of %s (%s), and the change touches %s, which the lint loads",
			firstWord(crash.cmd), crash.sig.lacks, crash.line, clipList(r.theirs))
		p.failCheck(lintKey(cmd), []string{crash.line}, why, script)
		return "code"
	case len(r.theirs) > 0: // any other sign, on a change that touches what the lint loads or that its output names
		line := cmp.Or(lastLine(p.stderr), r.sign)
		p.say("\nIt %s, and the change touches %s, which the lint loads or its output names: the writer's to fix.\n", r.sign,
			clipList(r.theirs))
		p.failCheck(lintKey(cmd), []string{line}, fmt.Sprintf("lint: `%s` %s (%s), and the change touches %s, which the lint loads",
			firstWord(cmd), r.sign, line, clipList(r.theirs)), script)
		return "code"
	}
	key := lintKey(cmd)
	switch {
	case r.sign == "": // an output the factory cannot read: the writer's, with it, as any failure the change could have caused
		p.failUnnamed(key, r.run, fmt.Sprintf("lint: `%s` exited %d and names no offense by path:line: %s", firstWord(cmd), r.run.Exit,
			strings.Join(firstFailing(r.run), "; ")), script)
		return "code"
	case p.toWriter(key): // your answer handed its faults to the writer
		line := cmp.Or(lastLine(p.stderr), r.sign)
		p.failCheck(key, []string{line}, fmt.Sprintf("lint: `%s` exited %d and named no offense: %s", firstWord(cmd), r.run.Exit, line), script)
		return "code"
	}
	f := checkFault{key: key, skip: key, what: fmt.Sprintf("exited %d and named no offense by path:line", r.run.Exit), bound: r.bound, tried: tried,
		tail: lastLines(scrub.Text(textx.StripTerminal(p.stderr)), 10)}
	if !r.isCrash {
		p.faultForYou(f, p.lintCrashWhy(cmd))
		return "unknown"
	}
	f.what = "crashed for lack of " + crash.sig.lacks
	if !p.res.ForYou {
		p.line = crash.line
	}
	p.faultForYou(f, crash.Error()+"; fix: "+crash.fix())
	return "env"
}

// lintCrashWhy is why lint command cmd failed for you when its run exited
// non-zero naming no offense by path:line: the last line of its stderr
// that may say why (lastLine; its stdout, what it says about the branch,
// never), else, when cmd names no format of its own, a hint at one.
func (p *precheckRun) lintCrashWhy(cmd string) string {
	why := fmt.Sprintf("precheck.lint `%s` exited %d and names no offense by path:line", cmd, p.exit)
	line := lastLine(p.stderr)
	switch {
	case line != "":
		why += "; its stderr ends: " + line
	case !namesFormat(cmd):
		why += "; fix: have it print them so (rubocop --format emacs, eslint --format unix) in precheck.lint, then `magnum factory install`"
	}
	if !p.res.ForYou { // the failure for you the precheck says
		p.line = line
	}
	return why
}

// changedLines are the lines each file of diff has at the head that the
// change added or changed.
func changedLines(diff []gitx.FileLines) map[string]map[int]bool {
	out := map[string]map[int]bool{}
	for _, f := range diff {
		lines := map[int]bool{}
		for _, h := range f.Hunks {
			for n := h.Start; n < h.Start+h.Count; n++ {
				lines[n] = true
			}
		}
		out[f.Path] = lines
	}
	return out
}

// firstWord is a command's first word.
func firstWord(cmd string) string {
	if f := strings.Fields(cmd); len(f) > 0 {
		return f[0]
	}
	return cmd
}

// failureRe is a failure in a spec run's list of failures, as rspec prints
// it: `rspec ./spec/x_spec.rb:12 # what it says`.
var failureRe = regexp.MustCompile(`(?m)^rspec\s+(?:\./)?(\S+?_spec\.rb)(?::(\d+)|\[[0-9:,]+\])?(?:\s+#\s*(.*?))?\s*$`)

// specFailure is one failing example: its spec file, and its key (the file
// and what it says, else its place), which tells it at the head and on the
// base whatever the lines did.
type specFailure struct{ file, key, line string }

// failures are the failing examples of a spec run's output.
func failures(out string) []specFailure {
	var fs []specFailure
	for _, m := range failureRe.FindAllStringSubmatch(out, -1) {
		key := m[1] + " # " + m[3]
		if m[3] == "" {
			key = m[0]
		}
		fs = append(fs, specFailure{file: m[1], key: key, line: strings.TrimSpace(m[0])})
	}
	return fs
}

// specs runs the specs the change affects (the specs command on the changed
// files) at the head. Red ones are run again on the base (onBase): an
// example red there too is recorded as a failure that is not ours
// (not_ours) and does not fail the precheck; one green there, or of a spec
// file the base lacks, is the change's (code). A red run that names no
// failing example is the writer's, as anything the change could have caused
// or the factory cannot read (a file that does not load, errors outside of
// examples), unless it shows a positive sign of a broken environment
// (faultSign): then it takes the fault steps (again, after the
// repository's installs), then asks you. Every red run at the head leaves
// its row. Specs your answer skipped do not run; your bound bounds them.
func (p *precheckRun) specs(cmd string, files []string) {
	p.say("\n## Specs\n")
	if len(files) == 0 {
		return
	}
	script := renderCommand(cmd, files, p.base)
	const key = "precheck.specs"
	if p.stops.skipped(key) {
		p.skipCheck(registry.CheckSpec, key, script)
		return
	}
	r := p.runNaming(key, "specs", script, "failing spec", files, func(run ProofRun) bool { return len(failures(run.Output)) > 0 })
	run := r.run
	switch {
	case r.cutSign != "": // a fault whose run again the cap stopped never passes
		p.record(registry.Check{Kind: registry.CheckSpec, Result: registry.CheckFail, Cause: "unknown", Command: script, Exit: run.Exit, Took: run.Took})
		p.faultForYou(r.fault(key), fmt.Sprintf("specs: %s %s, and its run again did not end within the cap", firstWord(cmd),
			r.cutSign))
		return
	case !r.ok:
		if !p.stopped() {
			p.record(registry.Check{Kind: registry.CheckSpec, Result: registry.CheckFail, Cause: "code", Command: script, Exit: run.Exit, Took: run.Took})
		}
		return
	case run.Exit == 0:
		p.say("\n`%s`: passed\n", script)
		p.record(registry.Check{Kind: registry.CheckSpec, Result: registry.CheckPass, Command: script, Took: run.Took})
		return
	}
	red := failures(run.Output)
	if len(red) == 0 {
		p.say("\n`%s` exited %d and names no failing spec:\n\n```\n%s\n```\n", script, run.Exit, run.Output)
		p.record(registry.Check{Kind: registry.CheckSpec, Result: registry.CheckFail, Cause: "unknown", Command: script, Exit: run.Exit, Took: run.Took})
		if r.sign != "" {
			p.faultForYou(r.fault(key), fmt.Sprintf("specs: %s exited %d and %s", firstWord(cmd), run.Exit, r.sign))
			return
		}
		p.failUnnamed(key, run, fmt.Sprintf("specs: %s exited %d and names no failing spec", firstWord(cmd), run.Exit), script)
		return
	}
	p.judgeRed(cmd, script, run, red)
}

// runnerRun is the end of a test runner's runs with their fault steps
// (runNaming): its last run, whether it ran to its end, the sign of a broken
// environment it shows (faultSign; "" for none), the sign of the run before
// a run again the cap stopped (cutSign: a fault that never passes), the
// bound its runs had, and the steps tried.
type runnerRun struct {
	run           ProofRun
	ok            bool
	sign, cutSign string
	bound         time.Duration
	tried         []string
}

// fault is the check fault of r, check key's: what it did, its bound, its
// steps and its last lines.
func (r runnerRun) fault(key string) checkFault {
	sign := cmp.Or(r.sign, r.cutSign)
	stdout, stderr := runText(r.run)
	return checkFault{key: key, skip: key, what: fmt.Sprintf("exited %d and %s", r.run.Exit, sign), bound: r.bound, tried: r.tried,
		tail: lastLines(scrub.Text(stdout+"\n"+stderr), 10)}
}

// runNaming runs the test runner script (check key, what in the waits), on
// your bound for it when you gave one, and, while a run exits non-zero
// naming no failure (names reads it; failing words what it names) with a
// positive sign of a broken environment (faultSign, files the changed
// files: a crash their change could cause is no such sign), its fault
// steps: again, then after the repository's installs, each once at the
// head. A check your answer handed to the writer takes none. Each run that
// runs again is recorded as a failed check of unknown cause; one whose run
// again the cap stopped is cutSign's, a fault that never passes.
func (p *precheckRun) runNaming(key, what, script, failing string, files []string, names func(ProofRun) bool) runnerRun {
	r := runnerRun{bound: cmp.Or(p.stops.bound(key), p.limit)}
	runOnce := func() {
		if b := p.stops.bound(key); b > 0 {
			p.onOwnBound(b, func() { r.run, r.ok = p.run(key, what, script) })
		} else {
			r.run, r.ok = p.run(key, what, script)
		}
		r.sign = ""
		if r.ok && r.run.Exit != 0 && !names(r.run) && !p.toWriter(key) {
			stdout, stderr := runText(r.run)
			crash, isCrash := crashOf(key, script, p.c.Path, stderr)
			if !isCrash || len(branchCaused(crash, stderr, files)) == 0 {
				r.sign = faultSign(r.run.Exit, stdout, stderr, crash, isCrash, "")
			}
			if r.sign != "" && runnerCaused(r.run.Exit, stdout+"\n"+stderr, files) { // a crash on the change's code: the writer's
				r.sign = ""
			}
		}
	}
	runOnce()
	for r.sign != "" && !p.halted() {
		step := p.nextFaultStep(key, false)
		if step == "" {
			break
		}
		p.say("\n`%s` exited %d, %s and names no %s: the factory runs it again.\n", script, r.run.Exit, r.sign, failing)
		p.record(registry.Check{Kind: registry.CheckSpec, Result: registry.CheckFail, Cause: "unknown", Command: script, Exit: r.run.Exit, Took: r.run.Took})
		if step == stepHeal {
			if failed := p.healFault(key); failed != "" {
				p.spendFaultStep(key, step, "ran the repository's installs", "the installs failed: "+failed)
				r.tried = append(r.tried, "ran the repository's installs, and the installs failed: "+failed)
				break
			}
		}
		before := r.sign
		runOnce()
		words := faultStepWords(step, r.bound, p.repo.Installs)
		r.tried = append(r.tried, words)
		// The cap stopped the run again of a fault: it never passes. A run
		// again that did not start (p.err: the machine's, of the moment) or
		// that cannot run (a failure for you) is not the cap's: its own
		// cause stands, the infrastructure hold's or yours.
		if !r.ok && p.parent.Err() == nil && p.err == nil && !p.res.ForYou {
			r.cutSign = before
		}
		p.spendFaultStep(key, step, words, runEnded(r.run, r.ok, names(r.run)))
	}
	return r
}

// runEnded says how a check's run ended (ran: to its end; named: it named
// what failed), for its fault step's event.
func runEnded(run ProofRun, ran, named bool) string {
	switch {
	case !ran:
		return "it did not run to its end"
	case run.Exit == 0:
		return "it passed"
	case named:
		return "this run named what failed"
	}
	return fmt.Sprintf("it exited %d and named no failure again", run.Exit)
}

// judgeRed judges red, the red examples of the head's spec run (of script,
// command cmd), on the base (onBase): those red there too are not ours,
// the rest fail the precheck, the writer's.
func (p *precheckRun) judgeRed(cmd, script string, run ProofRun, red []specFailure) {
	baseRed, judged := p.onBase(cmd, run, red)
	if !judged {
		return
	}
	var ours, theirs []specFailure
	for _, f := range red {
		if baseRed[f.key] {
			theirs = append(theirs, f)
		} else {
			ours = append(ours, f)
		}
	}
	if len(theirs) > 0 {
		p.say("\nRed on the base too, so not the change's (red on the base too):\n\n```\n%s\n```\n", failureLines(theirs))
		p.record(registry.Check{Kind: registry.CheckSpec, Result: registry.CheckFail, Cause: "not_ours", Command: script, Exit: run.Exit, Took: run.Took})
	}
	if len(ours) > 0 {
		var specFiles, keys []string
		for _, f := range ours {
			if !slices.Contains(specFiles, f.file) {
				specFiles = append(specFiles, f.file)
			}
			keys = append(keys, f.key)
		}
		p.say("\nRed at the head and green on the base, the change's:\n\n```\n%s\n```\n", failureLines(ours))
		p.record(registry.Check{Kind: registry.CheckSpec, Result: registry.CheckFail, Cause: "code", Command: script, Exit: run.Exit, Took: run.Took})
		p.failCheck("precheck.specs", keys, fmt.Sprintf("specs: %s red at the head and green on the base (%s)", plural(len(ours), "example"),
			clipList(specFiles)), script)
	}
}

// onBase judges red, the red examples of run (the head's spec run of the
// specs command cmd), on the base: the spec files of red that the base has
// run there, in a detached checkout of it (the same guarded step as the
// proof's base run), with the base's own spec files. baseRed are the keys of
// the examples red there too; judged is false where the base run gives no
// verdict. They are judged whatever the cap: the lookup of their files on
// the base and the base run run on the caller's context, which the cap
// never ends, the base run bounded by baseBound (the cap grown from the red
// files, or twice the head's run when that is longer), or by your answer's
// bound for it. A base run that does not end within its bound hangs: it
// takes the fault steps (again, after the repository's installs, and with
// its bound doubled up to precheck.cap_max), then it is a failure for you
// that asks you (faultForYou), whatever it printed: at the stop rspec
// prints the result line of the examples it finished, so no line tells a
// hang from a run that ended. Where the base run gives no verdict (it
// hangs, has a problem, or meets changes the factory keeps for you),
// precheck.md names the head's red examples (unjudged).
func (p *precheckRun) onBase(cmd string, run ProofRun, red []specFailure) (baseRed map[string]bool, judged bool) {
	// On the caller's context from here: the cap neither cuts the lookup
	// (a red example of a file it did not find would be the change's) nor
	// the base run.
	var atBase []string
	for _, f := range red {
		if slices.Contains(atBase, f.file) {
			continue
		}
		if has, err := p.e.d.Git.HasFile(p.parent, p.c.Path, p.base, f.file); err == nil && has { // a probe: a file the base lacks warns nothing
			atBase = append(atBase, f.file)
		}
	}
	if p.parent.Err() != nil { // the daemon stops, or the head moved: no lookup that did not run judges an example
		return nil, false
	}
	if len(atBase) == 0 {
		return map[string]bool{}, true
	}
	if slices.Contains(p.stops.NoBase, specsBaseKey) { // your answer: the red specs are judged at the head alone
		p.say("\nNot run on the base: your answer judges the specs red at the head without their run there, as the change's.\n")
		return map[string]bool{}, true
	}
	headRed := registry.Check{Kind: registry.CheckSpec, Result: registry.CheckFail, Cause: "unknown", Command: run.Command, Exit: run.Exit, Took: run.Took}
	baseScript := renderCommand(cmd, atBase, p.base)
	bound, within := baseBound(p.repo, len(atBase), run.Took)
	if b := p.stops.bound(specsBaseKey); b > 0 {
		bound, within = b, capText(b)+" (your answer's bound)"
	}
	p.e.noteCheck(p.t.ID, "precheck: specs on the base ("+plural(len(atBase), "spec file")+" red at the head)")
	base, ok := p.baseOnce(red, atBase, headRed, baseScript, bound)
	var tried []string
	for ok && base.Hung && !p.halted() {
		step := p.nextFaultStep(specsBaseKey, true)
		if step == "" {
			break
		}
		next := bound
		switch step {
		case stepHeal:
			if failed := p.healFault(specsBaseKey); failed != "" {
				p.spendFaultStep(specsBaseKey, step, "ran the repository's installs", "the installs failed: "+failed)
				tried = append(tried, "ran the repository's installs, and the installs failed: "+failed)
				ok = false
			}
		case stepLonger:
			if next, ok = p.longerBound(bound); !ok {
				p.spendFaultStep(specsBaseKey, step, "had no longer bound to run it with", "its bound is precheck.cap_max already")
			}
		}
		if !ok {
			ok = true // the last run stands: it hung
			break
		}
		p.say("\n`%s` did not end on the base within %s: the factory runs it again.\n", baseScript, within)
		bound, within = next, capText(next)
		base, ok = p.baseOnce(red, atBase, headRed, baseScript, bound)
		words := faultStepWords(step, bound, p.repo.Installs)
		tried = append(tried, words)
		ended := "it did not end within " + capText(bound) + " again"
		if !base.Hung {
			ended = "this run ended"
		}
		p.spendFaultStep(specsBaseKey, step, words, ended)
	}
	if !ok {
		return nil, false
	}
	if base.Hung {
		p.record(headRed)
		p.say("\n`%s` did not end on the base within %s, and the factory stopped it.\n", baseScript, within)
		p.sayTail("stdout", base.out, precheckCapTail)
		p.sayTail("stderr", base.errOut, precheckErrTail)
		p.unjudged(red, atBase)
		p.faultForYou(checkFault{key: specsBaseKey, skip: "precheck.specs", what: "did not end within " + capText(bound),
			hang: true, bound: bound, tried: tried, tail: streamTail(base.errOut, precheckErrTail)},
			"precheck.specs `"+textx.Clip(baseScript, 120)+"` did not end on the base within "+within+": it hangs; "+hangFix+
				", or raise precheck.cap or precheck.cap_max when it only runs long")
		return nil, false
	}
	baseRed = map[string]bool{}
	for _, f := range failures(base.Output) {
		baseRed[f.key] = true
	}
	return baseRed, true
}

// baseOnce runs baseScript, the base run of the red examples red of the
// spec files atBase, bounded by bound: ok is false where it gave no run to
// judge (the precheck's own changes that cannot go back, changes the
// factory keeps for you, a problem that keeps it from running, the
// caller's end, a failure to run), each settled here as the precheck's
// (headRed is the head's red run's row).
func (p *precheckRun) baseOnce(red []specFailure, atBase []string, headRed registry.Check, baseScript string, bound time.Duration) (ProofRun, bool) {
	// The detach discards what differs from the branch: the precheck's
	// own changes go back first, guarded; what it cannot vouch for stays.
	if !p.restoreOwn() {
		p.unjudged(red, atBase)
		return ProofRun{}, false
	}
	base, problem, err := p.e.baseRun(p.parent, p.t, p.c, p.started, p.base, p.head, nil, baseScript, bound,
		"precheck specs on the base")
	if k, ok := errors.AsType[keptChanges](err); ok {
		p.say("\n**Not run on the base: %s.**\n", k.why)
		p.unjudged(red, atBase)
		p.keep(k.why)
		return base, false
	}
	switch {
	case p.parent.Err() != nil:
		return base, false
	case err != nil:
		p.err = err
		return base, false
	case problem != "":
		p.record(headRed)
		p.unjudged(red, atBase)
		p.failForYou("precheck.specs `" + baseScript + "` " + problem + problemFix("precheck.specs", baseScript, problem))
		return base, false
	}
	return base, true
}

// baseBound is the bound of the base run of n spec files red at the head,
// whose run of the change's spec files took took: the cap grown from the n
// files (precheckCap), or twice took when that is longer, since the head's
// run covered those files and more; within says it as the hang does
// ("26m22s (twice the head's run of 13m11s)"). took is rounded to the second
// first (from a second on: capText's), so the bound and both times in the
// text agree. The head's run is bounded by the precheck's cap, so twice it
// is at most twice cap_max.
func baseBound(repo config.FactoryRepo, n int, took time.Duration) (bound time.Duration, within string) {
	bound, _ = precheckCap(repo, n) // as the precheck's grows from the change's files
	if took >= time.Second {
		took = took.Round(time.Second)
	}
	if twice := 2 * took; twice > bound {
		return twice, capText(twice) + " (twice the head's run of " + capText(took) + ")"
	}
	return bound, capText(bound)
}

// unjudged names the head's red examples red in precheck.md where the base
// run gave no verdict: those of the spec files the base has (atBase), which
// no run judged, and apart from them those of a spec file the base lacks,
// the change's with no base run.
func (p *precheckRun) unjudged(red []specFailure, atBase []string) {
	var open, lacks []specFailure
	for _, f := range red {
		if slices.Contains(atBase, f.file) {
			open = append(open, f)
		} else {
			lacks = append(lacks, f)
		}
	}
	p.say("\nRed at the head, not judged on the base:\n\n```\n%s\n```\n", failureLines(open))
	if len(lacks) > 0 {
		p.say("\nRed at the head, of a spec file the base lacks (the change's):\n\n```\n%s\n```\n", failureLines(lacks))
	}
}

// failureLines are the lines of the failing examples fs, as rspec lists
// them, one a line.
func failureLines(fs []specFailure) string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.line)
	}
	return strings.Join(out, "\n")
}

// jest runs the related jest tests of the changed files: a run that exits
// non-zero is the writer's, its failing tests read through its colors and
// from its summary (jestFailures), or, when it names none the factory can
// read, its first failing line; only one that shows a positive sign of a
// broken environment (faultSign) takes the fault steps (again, after the
// repository's installs), then asks you. Tests your answer skipped do not
// run; your bound bounds them.
func (p *precheckRun) jest(cmd string, files []string) {
	if cmd == "" || len(files) == 0 {
		return
	}
	script := renderCommand(cmd, files, p.base)
	const key = "precheck.jest"
	if p.stops.skipped(key) {
		p.skipCheck(registry.CheckSpec, key, script)
		return
	}
	named := func(run ProofRun) []string {
		stdout, stderr := runText(run)
		return jestFailures(stdout + "\n" + stderr)
	}
	r := p.runNaming(key, "jest", script, "failing test", files, func(run ProofRun) bool { return len(named(run)) > 0 })
	run := r.run
	check := registry.Check{Kind: registry.CheckSpec, Result: registry.CheckPass, Command: script, Exit: run.Exit, Took: run.Took}
	switch failed := named(run); {
	case r.cutSign != "": // a fault whose run again the cap stopped never passes
		check.Result, check.Cause = registry.CheckFail, "unknown"
		p.faultForYou(r.fault(key), fmt.Sprintf("jest: %s %s, and its run again did not end within the cap", firstWord(cmd), r.cutSign))
	case !r.ok && (p.ctx.Err() != nil || p.overCap):
		return
	case !r.ok:
		check.Result, check.Cause = registry.CheckFail, "code"
	case run.Exit != 0 && r.sign != "":
		check.Result, check.Cause = registry.CheckFail, "unknown"
		p.say("\n## Jest\n\n`%s` exited %d and %s:\n\n```\n%s\n```\n", script, run.Exit, r.sign, run.Output)
		p.faultForYou(r.fault(key), fmt.Sprintf("jest: %s exited %d and %s", firstWord(cmd), run.Exit, r.sign))
	case run.Exit != 0:
		check.Result, check.Cause = registry.CheckFail, "code"
		p.say("\n## Jest\n\n`%s` exited %d:\n\n```\n%s\n```\n", script, run.Exit, run.Output)
		if len(failed) == 0 {
			p.failUnnamed(key, run, fmt.Sprintf("jest: %s exited %d", firstWord(cmd), run.Exit), script)
		} else {
			p.failCheck(key, failed, fmt.Sprintf("jest: %s exited %d", firstWord(cmd), run.Exit), script)
		}
	default:
		p.say("\n## Jest\n\n`%s`: passed\n", script)
	}
	p.record(check)
}

// migrationVersionRe is the version a migration's file name starts with.
var migrationVersionRe = regexp.MustCompile(`^(\d+)_`)

// migrations names each migration the branch adds with its flags
// (ScanMigrations) and runs the repository's migrations commands for it on
// each database of its folder (migrationDatabases), in order: for each
// migration, for each database, every command ({version} its version,
// {file} its path, {database} the database): up, down and up. A gh-ost
// migration is not run (its down refuses by the marker's design, and a
// person runs it on its environments), nor one of a folder with no
// databases (its line says so): a skipped check, and no failure. A command
// that fails fails the precheck, naming the migration and the database, and
// its later commands on that database do not run (precheck.md says the
// database may be left part-way: partWay); its other databases still do.
func (p *precheckRun) migrations(repo config.FactoryRepo, cmds []string, added []string) {
	if len(added) == 0 {
		return
	}
	p.say("\n## Migrations\n\n")
	flags, err := ScanMigrations(p.c.Path, added, repo)
	if err != nil {
		p.say("- the migration-safety lint cannot read them: %v\n", err)
		p.failForYou("the migration-safety lint of " + repo.Repo + "'s migrations: " + err.Error())
		return
	}
	p.res.Migrations = flags
	for _, f := range flags {
		p.say("- %s\n", f)
	}
	for _, f := range flags {
		if p.stopped() {
			return
		}
		switch {
		case f.Ghost:
			p.say("\n%s is a gh-ost migration: not run here (a person runs it on %s)\n", f.Path, cmpJoin(f.Envs, "its environments"))
			p.record(registry.Check{Kind: registry.CheckMigration, Result: registry.CheckSkipped})
			continue
		case f.NoDatabases != "": // its line above says so
			p.record(registry.Check{Kind: registry.CheckMigration, Result: registry.CheckSkipped})
			continue
		}
		version := ""
		if m := migrationVersionRe.FindStringSubmatch(filepath.Base(f.Path)); m != nil {
			version = m[1]
		}
		_, dbs := migrationDatabases(repo.Precheck, f.Path)
		for _, db := range dbs {
			if p.stopped() {
				return
			}
			p.migrate(f.Path, version, db, cmds)
		}
	}
}

// migrate runs cmds for migration path (of version) on database db ("" for
// none: a repository without migration_databases), in order, until one
// fails; precheck.md names the run, with its database.
func (p *precheckRun) migrate(path, version, db string, cmds []string) {
	if len(cmds) == 0 {
		return
	}
	on := ""
	if db != "" {
		on = " on " + db
	}
	for _, cmd := range cmds {
		script := strings.NewReplacer("{version}", version, "{file}", path, config.DatabasePlaceholder, db).
			Replace(renderCommand(cmd, []string{path}, p.base))
		run, ok := p.run("precheck.migrations", "migration "+filepath.Base(path)+on, script)
		if !ok {
			if p.overCap && p.parent.Err() == nil { // its own bound stopped it mid-run (capStopped)
				p.say("\n%s\n", partWay(path, db))
			}
			if !p.stopped() {
				p.record(registry.Check{Kind: registry.CheckMigration, Result: registry.CheckFail, Cause: "code", Command: script})
			}
			return
		}
		check := registry.Check{Kind: registry.CheckMigration, Result: registry.CheckPass, Command: script, Exit: run.Exit, Took: run.Took}
		if run.Exit != 0 {
			check.Result, check.Cause = registry.CheckFail, "code"
			p.record(check)
			p.say("\n%s%s: `%s` exited %d:\n\n```\n%s\n```\n", path, on, script, run.Exit, run.Output)
			p.say("\n%s\n", partWay(path, db))
			p.failCheck("precheck.migrations", []string{path + on + ": " + lastLine(run.Output)}, fmt.Sprintf("migration %s%s: %s exited %d", path, on, script, run.Exit), script)
			return
		}
		p.record(check)
	}
	p.say("\n%s%s: passed\n", path, on)
}

// partWay says that a failed command of migration path (one that exited
// non-zero, or that its own bound stopped) can leave database db ("" for the
// copy's one database) part-way migrated: nothing puts back
// what it did (MySQL's DDL is no transaction), so the next run of the
// migration starts from there. A migration outside db/migrate adds the gap
// of the copy's next claim, which looks only at db/migrate's database
// (foreignSchema) to decide whether to reset the databases.
func partWay(path, db string) string {
	which := "The copy's database"
	if db != "" {
		which = db
	}
	s := which + " may be left part-way migrated: the factory does not put back what the failed command did, " +
		"so the migration's next run starts from there"
	if folder, _ := config.MigrationFolder(path); folder != config.DefaultMigrationFolder {
		s += "; and the copy's next claim looks only at db/migrate's database when it decides whether to reset the databases, " +
			"never at " + which
	}
	return s + "."
}

// migrationsNotRun are the paths of the migrations of flags the precheck
// did not run for lack of databases (MigrationFlag.NoDatabases).
func migrationsNotRun(flags []MigrationFlag) []string {
	var out []string
	for _, f := range flags {
		if f.NoDatabases != "" {
			out = append(out, f.Path)
		}
	}
	return out
}

// specsLock takes the machine's spec lock (Layout.FactorySpecsLock) for task
// t's what (the proof, the precheck): while another holds it, the task's
// check says so and tries again every lockPoll, until ctx ends.
func (e *Engine) specsLock(ctx context.Context, t registry.Task, what string) (unlock func(), err error) {
	path := e.d.Layout.FactorySpecsLock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	for {
		unlock, held, err := engine.AcquireLock(path)
		if err != nil {
			return nil, fmt.Errorf("the machine's spec lock %s: %w", path, err)
		}
		if !held {
			return unlock, nil
		}
		e.noteCheck(t.ID, what+": another precheck runs specs; this one waits for the machine's spec lock")
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(max(e.lockPoll, time.Millisecond)):
		}
	}
}

// miseArgs are the arguments of `mise` that run script in copy c with its
// env.
func miseArgs(c Copy, script string) []string { return slots.MiseExecArgs(c.Path, c.Env, script) }

// unsetEnv is what the factory's commands in a copy never inherit from the
// daemon: the variables that point git elsewhere, the GitHub tokens and the
// App identities' private keys, as magnum's heavy commands.
func unsetEnv(cfg *config.Config) []string {
	keys := slices.Concat(gitx.ScrubbedEnv(), slots.DefaultStripEnv)
	for _, id := range cfg.Identities {
		if id.PrivateKeyEnv != "" {
			keys = append(keys, id.PrivateKeyEnv)
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

// sha256Hex is b's sha256, lowercase hex.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// writeFile writes text to path atomically, private.
func writeFile(path, text string) error { return fsx.WriteFileAtomic(path, []byte(text), 0o600) }
