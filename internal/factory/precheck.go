package factory

// The precheck (spec 6.4): after the writer's commit, and after each fix of
// the local review, the factory runs the repository's checks on the change
// in its copy, as CI would: lint on the changed lines only (gitx.DiffLines;
// the copy's own configs are CI's), the specs the change affects with a
// baseline at the base (a spec red on the base too is not the change's),
// the related jest tests, and each migration the branch adds up and down,
// with the migration-safety lint of section 8.4. It runs under the
// machine's spec lock when it runs specs (one precheck at a time on the
// machine runs them; another waits, saying so) and stops at its cap. Every
// command goes through the deny list.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
// as "precheck: over 10m"). ForYou says the writer cannot fix why: a command
// the deny list refuses or that does not start, a lint whose output names no
// offense by path:line, a table_stats it cannot read (the configuration's or
// the machine's, named by its key); such a failure is the one Why says.
type PrecheckResult struct {
	Passed     bool
	Checks     []registry.Check
	Migrations []MigrationFlag
	Why        string
	ForYou     bool
}

// errOverCap is the cause of a precheck's context its cap ended.
var errOverCap = errors.New("the precheck's cap")

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
		if f.Status == 'A' && strings.HasPrefix(f.Path, "db/migrate/") && strings.HasSuffix(f.Path, ".rb") {
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

	limit := capOf(repo)
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

	p := &precheckRun{e: e, t: t, c: c, repo: repo, head: bh.Head, base: bh.Base, ctx: cctx, limit: limit, started: started,
		res: PrecheckResult{Passed: true}}
	p.say("# Precheck of %s at %s\n\nThe change from %s: %d files.\n", t.Branch, short(bh.Head), short(bh.Base), len(files))
	steps := []func(){
		func() { p.lint(pc.Lint, files, diff) },
		func() {
			switch {
			case specs:
				p.specs(pc.Specs, files)
			case pc.Specs != "":
				p.say("\n## Specs\n\nskipped: a %s runs no specs (skip_specs_for_kinds)\n", t.Kind)
				p.record(registry.Check{Kind: registry.CheckSpec, Result: registry.CheckSkipped, Command: pc.Specs})
			}
		},
		func() { p.jest(pc.Jest, files) },
		func() { p.migrations(repo, pc.Migrations, added) },
	}
	for _, step := range steps {
		if p.stopped() {
			break
		}
		step()
	}
	// What its commands changed goes back, guarded, however it ended. A copy
	// a base run left detached (its record is there) is resumeDetached's: the
	// precheck is a wait then, whatever else failed, so the task is never at
	// NEEDS YOU with its copy left detached, and the next check finds the rest
	// again.
	leftDetached := p.err != nil && e.hasDetachedRecord(t)
	if !leftDetached && !p.kept {
		p.restoreOwn()
	}
	overCap := errors.Is(context.Cause(cctx), errOverCap) || p.overCap
	if overCap {
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
	case overCap:
		p.res.Passed, p.res.Why = false, "precheck: over "+capText(limit)
	case isCrash:
		p.res.Passed, p.res.Why, crashed = false, "precheck: "+crash.Error(), crash
	case p.err != nil:
		return p.res, p.err
	}
	if p.res.Passed {
		p.say("\nPassed.\n")
	} else {
		p.say("\nFailed: %s\n", p.res.Why)
	}
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return p.res, err
	}
	if err := writeFile(filepath.Join(dir, precheckFile), p.report.String()); err != nil {
		return p.res, err
	}
	rec := precheckRecord{Head: p.head, Passed: p.res.Passed, Why: p.res.Why}
	if p.res.ForYou {
		rec.ForYou, rec.Line = true, p.line
		rec.Commands, rec.Env = e.checksPrint(repo)
	}
	if err := writeJSON(filepath.Join(dir, precheckRecordFile), rec); err != nil {
		return p.res, err
	}
	return p.res, crashed
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
}

// checksPrint is what the precheck commands of repo run with, as two
// hashes: commands, of its precheck commands as configured, and env, of
// the environment they run in (the copies' env of the configuration, and
// the daemon's locale and PATH, which every copy command inherits). They
// change when you edit either and restart the daemon on it, or your shell's
// PATH; never a value, only its hash, is kept.
func (e *Engine) checksPrint(repo config.FactoryRepo) (commands, env string) {
	pc := repo.Precheck
	commands = sha256Hex([]byte(strings.Join(slices.Concat(pc.Lint, []string{pc.Specs, pc.Jest}, pc.Migrations), "\x00")))
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
	var r precheckRecord
	b, err := os.ReadFile(filepath.Join(dir, precheckRecordFile))
	return err == nil && json.Unmarshal(b, &r) == nil && r.Passed && head != "" && r.Head == head
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
	ctx        context.Context
	limit      time.Duration
	res        PrecheckResult
	report     strings.Builder
	// err is a failure to run that ends the precheck (git, a start failure
	// of the moment); overCap a command that did not end within the cap
	// (its own bound); started when it began on a clean tree, before its
	// waits, or when it looked at the tree again after them (afterWait);
	// restoreErr a failed put-back of its own changes (restoreOwn), after
	// which it runs nothing more.
	err        error
	overCap    bool
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
	// repo is the task's repository; healed: a lint that crashed for lack
	// of a gem or a library had its installs run in the copy (heal), once,
	// and healFailed is why they failed ("" when they ran).
	repo       config.FactoryRepo
	healed     bool
	healFailed string
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
func (p *precheckRun) fail(why string) {
	if p.res.Passed {
		p.res.Passed, p.res.Why = false, "precheck: "+why
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
	start := time.Now()
	var res execx.Result
	var err error
	if dbKeys[key] { // the copy's databases: in turn with the local review's reviewers (db-lock)
		res, err = p.e.dbScript(p.ctx, p.c, cmd, "precheck "+what, p.limit, func(h dblock.Holder) {
			p.e.noteCheck(p.t.ID, "precheck: "+what+" waits for the copy's databases ("+h.String()+")")
		})
	} else {
		res, err = p.e.script(p.ctx, p.c, cmd, "precheck "+what, p.limit)
	}
	run.Took = time.Since(start).Round(time.Millisecond)
	code, denied, err := ranTo(res, err)
	run.Exit, run.Output = code, outputOf(res)
	p.stderr, p.exit = string(res.Stderr), code
	switch {
	case p.ctx.Err() != nil:
		return run, false
	case denied != "":
		p.say("\n- `%s`: denied: %s\n", cmd, denied)
		p.failForYou(key + " `" + cmd + "` is denied: " + denied + deniedFix(key))
		return run, false
	case errors.Is(err, context.DeadlineExceeded):
		p.overCap = true // its own bound is the cap
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
// told who holds it; a lock that stays held is a command that did not
// start, which the checks try again.
func (e *Engine) dbScript(ctx context.Context, c Copy, script, label string, timeout time.Duration, waiting func(dblock.Holder)) (execx.Result, error) {
	lock, err := dblock.Acquire(ctx, e.d.Layout.DBLock(c.Path), dblock.Holder{Role: "factory", Command: label, PID: os.Getpid()}, timeout, waiting)
	if err != nil {
		return execx.Result{Code: -1}, fmt.Errorf("the copy's database lock: %w", err)
	}
	defer lock.Release()
	return e.script(ctx, c, script, label, timeout)
}

// offenseRe is a lint offense line: path:line[:column][:] message.
var offenseRe = regexp.MustCompile(`^(?:\./)?([^\s:]+):(\d+)(?::\d+)?:?\s+(.*)$`)

// lint runs each lint command on the changed files and counts what it says
// about the changed lines only: a command that exits non-zero fails the
// precheck when it names an offense on a changed line, or names none it can
// be read by (path:line: message), and passes when every offense it names
// is on a line the change did not touch. One that crashed on its
// environment is lintCrashed's: a command it healed runs once more.
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
		if p.lintCmd(cmd, files, changed) && !p.stopped() {
			p.lintCmd(cmd, files, changed) // after the heal, once
		}
	}
}

// lintCmd runs lint command cmd on files (lint's): healed reports that it
// crashed for lack of something the repository's installs bring, and that
// they ran in the copy, so it runs again.
func (p *precheckRun) lintCmd(cmd string, files []string, changed map[string]map[int]bool) (healed bool) {
	script := renderCommand(cmd, files, p.base)
	run, ok := p.run("precheck.lint", "lint ("+firstWord(cmd)+")", script)
	if !ok {
		if !p.stopped() {
			p.record(registry.Check{Kind: registry.CheckLint, Result: registry.CheckFail, Cause: "code", Command: script, Exit: run.Exit, Took: run.Took})
		}
		return false
	}
	var mine []string
	named := 0
	for line := range strings.SplitSeq(run.Output, "\n") {
		m := offenseRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		named++
		file := strings.TrimPrefix(strings.TrimPrefix(m[1], p.c.Path+"/"), "./")
		n, _ := strconv.Atoi(m[2])
		if changed[file][n] {
			mine = append(mine, strings.TrimSpace(line))
		}
	}
	check := registry.Check{Kind: registry.CheckLint, Result: registry.CheckPass, Command: script, Exit: run.Exit, Took: run.Took}
	switch {
	case run.Exit == 0:
		p.say("\n- `%s`: passed\n", script)
	case len(mine) > 0:
		check.Result, check.Cause = registry.CheckFail, "code"
		p.say("\n- `%s`: %s on the changed lines:\n\n```\n%s\n```\n", script, plural(len(mine), "offense"), strings.Join(mine, "\n"))
		p.fail(fmt.Sprintf("lint: %s on the changed lines (%s)", plural(len(mine), "offense"), firstWord(cmd)))
	case named == 0:
		check.Result, check.Cause = registry.CheckFail, "unknown"
		p.say("\n- `%s` exited %d and names no offense by path:line:\n\n```\n%s\n```\n", script, run.Exit, run.Output)
		if crash, ok := crashOf("precheck.lint", cmd, p.c.Path, p.stderr); ok {
			check.Cause, healed = p.lintCrashed(crash, files)
			break
		}
		p.failForYou(p.lintCrashWhy(cmd))
	default:
		p.say("\n- `%s`: %s, none on a changed line\n", script, plural(named, "offense"))
	}
	p.record(check)
	return healed
}

// lintCrashed takes a lint that crashed on its environment (crash), the
// change being files: its check's cause, and healed when it ran the
// repository's installs in the copy and the lint runs again.
//   - One the machine's environment caused (a locale) is the precheck's
//     error: the infrastructure hold runs the checks again (provenHead),
//     at most envCrashTries times at one head.
//   - One for lack of a gem or a library runs the repository's installs in
//     the copy, once per precheck (heal), and the lint again.
//   - Else, a crash the change can be the cause of (it changed what the
//     lint loads, or its stderr names a changed file: branchCaused) is the
//     writer's to fix, and any other is yours, with the fix. Neither holds
//     another task.
func (p *precheckRun) lintCrashed(crash envCrash, files []string) (cause string, healed bool) {
	switch {
	case crash.sig.machine:
		p.say("\nIt crashed for lack of %s: the infrastructure hold runs the checks again at its next try.\n", crash.sig.lacks)
		p.err = crash
		return "env", false
	case crash.sig.heal && !p.healed && len(p.repo.Installs) > 0:
		p.healed = true
		if p.healFailed = p.heal(crash); p.healFailed == "" {
			return "env", true
		}
	}
	if theirs := branchCaused(crash, p.stderr, files); len(theirs) > 0 {
		p.say("\nIt crashed for lack of %s, and the change touches %s, which the lint loads: the writer's to fix.\n",
			crash.sig.lacks, clipList(theirs))
		p.fail(fmt.Sprintf("lint: `%s` crashed for lack of %s (%s), and the change touches %s, which the lint loads",
			firstWord(crash.cmd), crash.sig.lacks, crash.line, clipList(theirs)))
		return "code", false
	}
	fix := crash.fix()
	switch {
	case p.healFailed != "":
		fix += " (the factory ran the repository's installs there, and the installs failed: " + p.healFailed + ")"
	case p.healed:
		fix += " (the factory ran the repository's installs there once, and the lint still crashed)"
	}
	if !p.res.ForYou {
		p.line = crash.line
	}
	p.failForYou(crash.Error() + "; fix: " + fix)
	return "env", false
}

// heal runs the repository's installs in the copy (`bundle install`), as a
// claim runs them, after the lint crashed for lack of a gem or a library
// (crash), with heal begin and ok or fail events; failed is why one of them
// did not run to a zero exit (its error's line), "" when they all did.
// What they change in tracked files (Gemfile.lock) goes back with the
// precheck's own changes (restoreOwn).
func (p *precheckRun) heal(crash envCrash) (failed string) {
	p.e.event(p.ctx, p.t.ID, "heal", registry.PhaseBegin, fmt.Sprintf("%s: %s crashed for lack of %s; runs the repository's installs",
		p.c.Name, firstWord(crash.cmd), crash.sig.lacks))
	for _, in := range p.repo.Installs {
		if _, err := p.e.script(p.ctx, p.c, in.Run, "precheck heal "+in.Run, p.limit); err != nil {
			why := textx.Clip(textx.FirstLine(execx.Redact(err.Error())), outputLineRunes)
			if ee, ok := errors.AsType[*execx.ExitError](err); ok && firstLine(ee.Stderr) != "" { // its own words, not mise's command line
				why = fmt.Sprintf("exited %d: %s", ee.Code, firstLine(ee.Stderr))
			}
			p.say("\nThe heal's `%s` failed: %s\n", in.Run, why)
			p.e.event(context.WithoutCancel(p.ctx), p.t.ID, "heal", registry.PhaseFail, p.c.Name+": `"+in.Run+"` failed: "+why)
			return "`" + in.Run + "`: " + why
		}
	}
	p.say("\nIt crashed for lack of %s: the factory ran the repository's installs in the copy, and runs the lint again.\n", crash.sig.lacks)
	p.e.event(p.ctx, p.t.ID, "heal", registry.PhaseOK, p.c.Name+": the installs ran; the lint runs again")
	return ""
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
// files) at the head. Red ones are run again on the base, a detached
// checkout of it (the same guarded step as the proof's base run), with the
// base's own spec files: an example red there too is recorded as a failure
// that is not ours (not_ours) and does not fail the precheck; one green
// there, or of a spec file the base lacks, is the change's (code). A red
// run that names no failure it can be read by fails the precheck.
func (p *precheckRun) specs(cmd string, files []string) {
	p.say("\n## Specs\n")
	if len(files) == 0 {
		return
	}
	script := renderCommand(cmd, files, p.base)
	run, ok := p.run("precheck.specs", "specs", script)
	if !ok {
		if !p.stopped() {
			p.record(registry.Check{Kind: registry.CheckSpec, Result: registry.CheckFail, Cause: "code", Command: script, Exit: run.Exit, Took: run.Took})
		}
		return
	}
	if run.Exit == 0 {
		p.say("\n`%s`: passed\n", script)
		p.record(registry.Check{Kind: registry.CheckSpec, Result: registry.CheckPass, Command: script, Took: run.Took})
		return
	}
	red := failures(run.Output)
	if len(red) == 0 {
		p.say("\n`%s` exited %d and names no failing spec:\n\n```\n%s\n```\n", script, run.Exit, run.Output)
		p.record(registry.Check{Kind: registry.CheckSpec, Result: registry.CheckFail, Cause: "unknown", Command: script, Exit: run.Exit, Took: run.Took})
		p.fail(fmt.Sprintf("specs: %s exited %d and names no failing spec", firstWord(cmd), run.Exit))
		return
	}
	var atBase []string
	for _, f := range red {
		if slices.Contains(atBase, f.file) {
			continue
		}
		if _, err := p.e.d.Git.FileAt(p.ctx, p.c.Path, p.base, f.file); err == nil {
			atBase = append(atBase, f.file)
		}
	}
	baseRed := map[string]bool{}
	if len(atBase) > 0 {
		// The detach discards what differs from the branch: the precheck's
		// own changes go back first, guarded; what it cannot vouch for stays.
		if !p.restoreOwn() {
			return
		}
		baseScript := renderCommand(cmd, atBase, p.base)
		p.e.noteCheck(p.t.ID, "precheck: specs on the base ("+plural(len(atBase), "spec file")+" red at the head)")
		base, problem, err := p.e.baseRun(p.ctx, p.t, p.c, p.started, p.base, p.head, nil, baseScript, p.limit)
		if k, ok := errors.AsType[keptChanges](err); ok {
			p.say("\n**Not run on the base: %s.**\n", k.why)
			p.keep(k.why)
			return
		}
		switch {
		case p.ctx.Err() != nil:
			return
		case err != nil:
			p.err = err
			return
		case problem != "":
			p.failForYou("precheck.specs `" + baseScript + "` " + problem + problemFix("precheck.specs", baseScript, problem))
			return
		case base.Hung:
			p.overCap = true
			return
		}
		for _, f := range failures(base.Output) {
			baseRed[f.key] = true
		}
	}
	var ours, theirs []specFailure
	for _, f := range red {
		if baseRed[f.key] {
			theirs = append(theirs, f)
		} else {
			ours = append(ours, f)
		}
	}
	lines := func(fs []specFailure) string {
		var out []string
		for _, f := range fs {
			out = append(out, f.line)
		}
		return strings.Join(out, "\n")
	}
	if len(theirs) > 0 {
		p.say("\nRed on the base too, so not the change's (red on the base too):\n\n```\n%s\n```\n", lines(theirs))
		p.record(registry.Check{Kind: registry.CheckSpec, Result: registry.CheckFail, Cause: "not_ours", Command: script, Exit: run.Exit, Took: run.Took})
	}
	if len(ours) > 0 {
		var specFiles []string
		for _, f := range ours {
			if !slices.Contains(specFiles, f.file) {
				specFiles = append(specFiles, f.file)
			}
		}
		p.say("\nRed at the head and green on the base, the change's:\n\n```\n%s\n```\n", lines(ours))
		p.record(registry.Check{Kind: registry.CheckSpec, Result: registry.CheckFail, Cause: "code", Command: script, Exit: run.Exit, Took: run.Took})
		p.fail(fmt.Sprintf("specs: %s red at the head and green on the base (%s)", plural(len(ours), "example"), clipList(specFiles)))
	}
}

// jest runs the related jest tests of the changed files.
func (p *precheckRun) jest(cmd string, files []string) {
	if cmd == "" || len(files) == 0 {
		return
	}
	script := renderCommand(cmd, files, p.base)
	run, ok := p.run("precheck.jest", "jest", script)
	check := registry.Check{Kind: registry.CheckSpec, Result: registry.CheckPass, Command: script, Exit: run.Exit, Took: run.Took}
	switch {
	case !ok && (p.ctx.Err() != nil || p.overCap):
		return
	case !ok:
		check.Result, check.Cause = registry.CheckFail, "code"
	case run.Exit != 0:
		check.Result, check.Cause = registry.CheckFail, "code"
		p.say("\n## Jest\n\n`%s` exited %d:\n\n```\n%s\n```\n", script, run.Exit, run.Output)
		p.fail(fmt.Sprintf("jest: %s exited %d", firstWord(cmd), run.Exit))
	default:
		p.say("\n## Jest\n\n`%s`: passed\n", script)
	}
	p.record(check)
}

// migrationVersionRe is the version a migration's file name starts with.
var migrationVersionRe = regexp.MustCompile(`^(\d+)_`)

// migrations names each migration the branch adds with its flags
// (ScanMigrations) and runs the repository's migrations commands for it, in
// order ({version} its version, {file} its path): up and down. A gh-ost
// migration is not run (its down refuses by the marker's design, and a
// person runs it on its environments): a skipped check. A command that
// fails fails the precheck, naming the migration, and the migration's later
// commands do not run.
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
		if f.Ghost {
			p.say("\n%s is a gh-ost migration: not run here (a person runs it on %s)\n", f.Path, cmpJoin(f.Envs, "its environments"))
			p.record(registry.Check{Kind: registry.CheckMigration, Result: registry.CheckSkipped})
			continue
		}
		version := ""
		if m := migrationVersionRe.FindStringSubmatch(filepath.Base(f.Path)); m != nil {
			version = m[1]
		}
		for _, cmd := range cmds {
			script := strings.NewReplacer("{version}", version, "{file}", f.Path).Replace(renderCommand(cmd, []string{f.Path}, p.base))
			run, ok := p.run("precheck.migrations", "migration "+filepath.Base(f.Path), script)
			if !ok {
				if !p.stopped() {
					p.record(registry.Check{Kind: registry.CheckMigration, Result: registry.CheckFail, Cause: "code", Command: script})
				}
				break
			}
			check := registry.Check{Kind: registry.CheckMigration, Result: registry.CheckPass, Command: script, Exit: run.Exit, Took: run.Took}
			if run.Exit != 0 {
				check.Result, check.Cause = registry.CheckFail, "code"
				p.record(check)
				p.say("\n`%s` exited %d:\n\n```\n%s\n```\n", script, run.Exit, run.Output)
				p.fail(fmt.Sprintf("migration %s: %s exited %d", f.Path, script, run.Exit))
				break
			}
			p.record(check)
		}
	}
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
