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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
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
// as "precheck: over 10m").
type PrecheckResult struct {
	Passed     bool
	Checks     []registry.Check
	Migrations []MigrationFlag
	Why        string
}

// errOverCap is the cause of a precheck's context its cap ended.
var errOverCap = errors.New("the precheck's cap")

// precheck runs task t's precheck in copy c at its head (see the file's
// comment). A precheck that does not pass is a result with Why, nil error;
// the error is a wait (an agent at work in the copy), the caller's context
// ending, or a failure to run (git). It writes precheck.md.
func (e *Engine) precheck(ctx context.Context, t registry.Task, c Copy) (PrecheckResult, error) {
	repo, err := e.factoryRepo(t)
	if err != nil {
		return PrecheckResult{}, err
	}
	if e.d.Git == nil {
		return PrecheckResult{}, heldError{who: "factory", what: "this build has no git"}
	}
	pc := repo.Precheck
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

	p := &precheckRun{e: e, t: t, c: c, head: bh.Head, base: bh.Base, ctx: cctx, limit: limit,
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
	switch {
	case errors.Is(context.Cause(cctx), errOverCap):
		p.res.Passed, p.res.Why = false, "precheck: over "+shortAge(limit)
		p.say("\n**Stopped: over its cap of %s.**\n", shortAge(limit))
	case ctx.Err() != nil:
		return p.res, ctx.Err()
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
	return p.res, nil
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
	// err is a failure to run that ends the precheck (git).
	err error
}

func (p *precheckRun) say(format string, a ...any) { fmt.Fprintf(&p.report, format, a...) }

// stopped reports whether the precheck ends here: its cap, the caller's
// context or a failure to run.
func (p *precheckRun) stopped() bool { return p.ctx.Err() != nil || p.err != nil }

// fail fails the precheck with why, its first failure's reason.
func (p *precheckRun) fail(why string) {
	if p.res.Passed {
		p.res.Passed, p.res.Why = false, "precheck: "+why
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

// run runs a command of the precheck in the copy: its run, whether it ran
// to its end (false: the cap or the caller's context stopped it, or it did
// not start, which fails the precheck with the reason).
func (p *precheckRun) run(what, cmd string) (ProofRun, bool) {
	p.e.noteCheck(p.t.ID, "precheck: "+what)
	run := ProofRun{Command: cmd}
	start := time.Now()
	res, err := p.e.script(p.ctx, p.c, cmd, "precheck "+what, p.limit)
	run.Took = time.Since(start).Round(time.Millisecond)
	code, denied, err := ranTo(res, err)
	run.Exit, run.Output = code, outputOf(res)
	switch {
	case p.ctx.Err() != nil:
		return run, false
	case denied != "":
		p.say("\n- `%s`: denied: %s\n", cmd, denied)
		p.fail(what + ": denied: " + denied)
		return run, false
	case err != nil:
		p.say("\n- `%s` did not run: %s\n", cmd, textx.FirstLine(err.Error()))
		p.fail(what + ": " + cmd + " did not run: " + textx.Clip(textx.FirstLine(err.Error()), 200))
		return run, false
	}
	return run, true
}

// offenseRe is a lint offense line: path:line[:column][:] message.
var offenseRe = regexp.MustCompile(`^(?:\./)?([^\s:]+):(\d+)(?::\d+)?:?\s+(.*)$`)

// lint runs each lint command on the changed files and counts what it says
// about the changed lines only: a command that exits non-zero fails the
// precheck when it names an offense on a changed line, or names none it can
// be read by (path:line: message), and passes when every offense it names
// is on a line the change did not touch.
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
		script := renderCommand(cmd, files, p.base)
		run, ok := p.run("lint ("+firstWord(cmd)+")", script)
		if !ok {
			if p.ctx.Err() == nil {
				p.record(registry.Check{Kind: registry.CheckLint, Result: registry.CheckFail, Cause: "code", Command: script, Exit: run.Exit, Took: run.Took})
			}
			continue
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
			p.fail(fmt.Sprintf("lint: %s exited %d and names no offense by path:line", firstWord(cmd), run.Exit))
		default:
			p.say("\n- `%s`: %s, none on a changed line\n", script, plural(named, "offense"))
		}
		p.record(check)
	}
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
	run, ok := p.run("specs", script)
	if !ok {
		if p.ctx.Err() == nil {
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
		baseScript := renderCommand(cmd, atBase, p.base)
		p.e.noteCheck(p.t.ID, "precheck: specs on the base ("+plural(len(atBase), "spec file")+" red at the head)")
		base, denied, err := p.e.baseRun(p.ctx, p.t, p.c, p.base, p.head, nil, baseScript, p.limit)
		switch {
		case p.ctx.Err() != nil:
			return
		case err != nil:
			p.err = err
			return
		case denied != "":
			p.fail("specs on the base: denied: " + denied)
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
	run, ok := p.run("jest", script)
	check := registry.Check{Kind: registry.CheckSpec, Result: registry.CheckPass, Command: script, Exit: run.Exit, Took: run.Took}
	switch {
	case !ok && p.ctx.Err() != nil:
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
		p.fail("migrations: " + err.Error())
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
			run, ok := p.run("migration "+filepath.Base(f.Path), script)
			if !ok {
				if p.ctx.Err() == nil {
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
