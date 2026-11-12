package factory

// A check that crashed or hung is not the writer's failure (the 2026-11-12
// run's retry stops, item 1). A lint that exits non-zero and names no
// offense, a test runner that exits non-zero and names no failing example,
// and a run that does not end within its bound (a lint, the specs on the
// base) name no failure of the code: before the writer or you hear of one,
// the precheck takes these steps, in order, each once at a head:
//  1. it runs the check again;
//  2. it runs the repository's installs (heal) and the check again;
//  3. a run that hangs runs once more with its bound doubled, up to
//     precheck.cap_max.
//
// check-faults.json in the task's directory keeps the steps spent at each
// head, by check, each once its run ended, so a restart in the middle
// neither takes one twice nor loses one. Each step is a check event and an
// alert. A check that still names no failure after them is a failure for
// you that asks a question (faultQuestion): skip it for the task (the PR
// body names it), run it with a longer bound (yours), or drop the task;
// never "retry", and the factory never edits your configuration.
// stops.json keeps your decision, which the precheck reads (the skipped
// checks, the bounds).

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/scrub"
	"github.com/zhuravel/magnum/internal/textx"
)

// checkFaultsFile keeps the fault steps the precheck took, in the task's
// directory.
const checkFaultsFile = "check-faults.json"

// The fault steps of a check that named no failure (faultSteps).
const (
	stepAgain  = "again"
	stepHeal   = "heal"
	stepLonger = "longer"
)

// specsBaseKey is the key of the specs' run on the base (its bound is its
// own); a skip of it skips the specs.
const specsBaseKey = "precheck.specs on the base"

// lintKey is the key of lint command cmd (its configured form).
func lintKey(cmd string) string { return "precheck.lint `" + cmd + "`" }

// checkFault is a check that still named no failure after its fault steps,
// for you: its key (and the key a skip takes), the head, what it did, the
// bound its last run had, the steps the factory took, the last lines it
// printed, whether those name a file the branch changed (the writer's
// option is then recommended), and the writer's words of it (handed to the
// writer: why, its failures and the command, as failCheck takes them).
type checkFault struct {
	key, skip, head string
	what            string
	hang            bool
	bound           time.Duration
	tried           []string
	tail            string
	namesChanged    bool
}

// faultSign is the positive sign that a failed run of a check (its exit,
// stdout and stderr, and the crash its stderr tells, crashOf) names a broken
// environment and no failure of the code: it printed nothing at all, a
// signal ended it, its stderr tells a crash the repository's installs bring
// back (a missing gem, library or module) or a missing command, or, for a
// lint, it exited 2 with an error line on stderr (rubocop's and eslint's
// exit for an error, never for an offense). "" for none: the run is the
// writer's, as any failure the change could have caused or the factory
// cannot read. A crash of the machine's (a locale) is the infrastructure
// hold's, no fault here.
func faultSign(exit int, stdout, stderr string, crash envCrash, isCrash, lint bool) string {
	switch {
	case isCrash && crash.sig.machine: // the infrastructure hold's
		return ""
	case strings.TrimSpace(textx.StripTerminal(stdout+stderr)) == "":
		return "printed nothing"
	case exit < 0 || (exit > 128 && exit < 160):
		return fmt.Sprintf("was ended by a signal (exit %d)", exit)
	case isCrash && !crash.sig.machine && (crash.sig.heal || crash.sig.scriptOnly):
		return "crashed for lack of " + crash.sig.lacks
	case lint && exit == 2 && lastLine(stderr) != "":
		return "exited 2 with an error and named no offense"
	}
	return ""
}

// namesChanged reports whether text names one of files, the paths the
// branch changed.
func namesChanged(text string, files []string) bool {
	return slices.ContainsFunc(files, func(f string) bool { return f != "" && strings.Contains(text, f) })
}

// toWriter reports whether your answer handed check key's faults to the
// writer for the task: its failure is the writer's, as any other.
func (p *precheckRun) toWriter(key string) bool { return slices.Contains(p.stops.Writer, key) }

// faultsRecord is check-faults.json: the steps spent, by faultKey.
type faultsRecord map[string][]string

// faultKey keys check key's steps at head.
func faultKey(head, key string) string { return head + " " + key }

// readFaults reads check-faults.json in the task directory dir; none is an
// empty record.
func readFaults(dir string) (faultsRecord, error) {
	rec := faultsRecord{}
	b, err := os.ReadFile(filepath.Join(dir, checkFaultsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return rec, nil
	}
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return faultsRecord{}, fmt.Errorf("%s: %w", checkFaultsFile, err)
	}
	return rec, nil
}

// faultSteps are the steps a fault of check key takes: again, the heal
// when the repository has installs, and a longer bound for a hang.
func (p *precheckRun) faultSteps(hang bool) []string {
	steps := []string{stepAgain}
	if len(p.repo.Installs) > 0 {
		steps = append(steps, stepHeal)
	}
	if hang {
		steps = append(steps, stepLonger)
	}
	return steps
}

// nextFaultStep is the first step of a fault of check key (hang: it did
// not end within its bound) not spent at the head, "" when all are.
func (p *precheckRun) nextFaultStep(key string, hang bool) string {
	spent := p.faults[faultKey(p.head, key)]
	for _, s := range p.faultSteps(hang) {
		if !slices.Contains(spent, s) {
			return s
		}
	}
	return ""
}

// spendFaultStep records step of check key spent at the head, once its run
// ended (check-faults.json), with a check event and an alert that say what
// the factory did by itself (what, then how it ended: ended). A step the
// caller's end cut (the daemon stops, the head moved) is not spent: the
// next precheck takes it. A record that fails to write is logged: a restart
// may take the step again.
func (p *precheckRun) spendFaultStep(key, step, what, ended string) {
	if p.parent.Err() != nil {
		return
	}
	k := faultKey(p.head, key)
	p.faults[k] = append(p.faults[k], step)
	dir := p.e.d.Layout.FactoryTaskDir(p.t.ID)
	if err := writeJSON(filepath.Join(dir, checkFaultsFile), p.faults); err != nil {
		p.e.log.Warn("record a check's fault step", "task", p.t.ID, "err", err)
	}
	ctx := context.WithoutCancel(p.parent)
	text := fmt.Sprintf("%s at %s named no failure of the change: the factory %s, and %s", key, short(p.head), what, ended)
	p.e.event(ctx, p.t.ID, "check", registry.PhaseInfo, text)
	p.e.alert(ctx, p.t.ID, TaskKey(p.t)+": "+healsAlert+text)
}

// faultStepWords say fault step step in the factory's words, bound being the bound of its
// run.
func faultStepWords(step string, bound time.Duration, installs []config.FactoryInstall) string {
	switch step {
	case stepHeal:
		var runs []string
		for _, in := range installs {
			runs = append(runs, "`"+in.Run+"`")
		}
		return "ran the repository's installs (" + strings.Join(runs, ", ") + ") and ran it again"
	case stepLonger:
		return "ran it with its bound doubled, " + capText(bound)
	}
	return "ran it again"
}

// longerBound is twice bound, up to precheck.cap_max (never below bound);
// ok is false when that is no longer than bound.
func (p *precheckRun) longerBound(bound time.Duration) (time.Duration, bool) {
	most := cmp.Or(p.repo.Precheck.CapMax.Duration, config.DefaultPrecheckCapMax)
	longer := min(2*bound, max(most, bound))
	return longer, longer > bound
}

// healFault runs the repository's installs in the copy for check key, as
// heal does for a lint that crashed for lack of a gem: failed is why one
// of them failed ("" when they all ran).
func (p *precheckRun) healFault(key string) (failed string) {
	p.e.event(context.WithoutCancel(p.parent), p.t.ID, "heal", registry.PhaseBegin, fmt.Sprintf("%s: %s named no failure; runs the "+
		"repository's installs", p.c.Name, key))
	for _, in := range p.repo.Installs {
		if _, err := p.e.script(p.parent, p.c, in.Run, "precheck heal", capOf(p.repo)); err != nil {
			why := textx.Clip(textx.FirstLine(scrub.Text(err.Error())), outputLineRunes)
			if ee, ok := errors.AsType[*execx.ExitError](err); ok && firstLine(ee.Stderr) != "" {
				why = fmt.Sprintf("exited %d: %s", ee.Code, firstLine(ee.Stderr))
			}
			p.say("\nThe heal's `%s` failed: %s\n", in.Run, why)
			p.e.event(context.WithoutCancel(p.parent), p.t.ID, "heal", registry.PhaseFail, p.c.Name+": `"+in.Run+"` failed: "+why)
			return "`" + in.Run + "`: " + why
		}
	}
	p.say("\nThe factory ran the repository's installs in the copy, and runs %s again.\n", key)
	p.e.event(context.WithoutCancel(p.parent), p.t.ID, "heal", registry.PhaseOK, p.c.Name+": the installs ran; "+key+" runs again")
	return ""
}

// faultForYou fails the precheck for you with check fault f, its fault
// steps spent: why says what it did and what the factory tried, never a
// retry, and the result carries f for the stage's question (faultQuestion).
func (p *precheckRun) faultForYou(f checkFault, why string) {
	f.head = p.head
	f.namesChanged = f.namesChanged || namesChanged(f.tail, p.files)
	if len(f.tried) > 0 {
		why += "; the factory " + andList(f.tried)
	}
	p.say("\n**%s named no failure of the change after the factory's steps: it asks you.**\n", f.key)
	if !p.res.ForYou {
		p.res.fault = &f
	}
	p.failForYou(why)
}

// skipCheck records check key of kind as skipped by your answer (cmd its
// command), in precheck.md and the registry.
func (p *precheckRun) skipCheck(kind, key, cmd string) {
	p.say("\n- %s: skipped for this task, on your answer to its question (the PR body names it)\n", key)
	p.record(registry.Check{Kind: kind, Result: registry.CheckSkipped, Command: cmd})
}

// jestFailRe is a failing test in jest's output: a FAIL line of a test
// file, a test marked ✕ or ×, a failure's title after ●, or its summary
// line of the tests or the suites that failed.
var jestFailRe = regexp.MustCompile(`(?m)^\s*(?:FAIL\s+\S+|[✕×]\s+.+|●\s+.+?|Tests:\s+\d+ failed.*|Test Suites:\s+\d+ failed.*)\s*$`)

// jestFailures are the failing tests jest's output names (stdout and
// stderr, where jest writes them), each once, read through its colors.
func jestFailures(out string) []string {
	var fs []string
	for _, m := range jestFailRe.FindAllString(textx.StripTerminal(out), -1) {
		if m = strings.TrimSpace(m); !slices.Contains(fs, m) {
			fs = append(fs, m)
		}
	}
	return fs
}

// runText is what run printed on stdout and stderr, from the ends the
// precheck keeps (precheckTailWindow bytes each, raw), read through its
// colors: what its failures and its signs are read from.
func runText(run ProofRun) (stdout, stderr string) {
	return textx.StripTerminal(string(run.out)), textx.StripTerminal(string(run.errOut))
}

// orFailing is failed, the failures a run named, else its first failing
// line (firstFailing).
func orFailing(failed []string, run ProofRun) []string {
	if len(failed) > 0 {
		return failed
	}
	return firstFailing(run)
}

// firstFailing is the first line of run's output that may say why it
// failed (meaningfulLines), else its exit: the failure a check that names
// none gives the writer's progress (fix_go_on.go).
func firstFailing(run ProofRun) []string {
	stdout, stderr := runText(run)
	if line := firstLine(stdout + "\n" + stderr); line != "" {
		return []string{line}
	}
	return []string{fmt.Sprintf("exited %d", run.Exit)}
}

// faultQuestion is the question of check fault f (the stage that checked
// its role's change asks it): skip the check for the task, run it with a
// longer bound, drop the task, or hand it to the writer. A fault whose
// output names a file the branch changed recommends the writer, a hang the
// longer bound, any other the skip.
func faultQuestion(f checkFault) stopQuestion {
	tried := "the factory ran it again"
	if len(f.tried) > 0 {
		tried = "the factory " + andList(f.tried)
	}
	q := registry.Question{
		Text:    fmt.Sprintf("The factory's check %s %s at %s: how does the task go on?", f.key, f.what, short(f.head)),
		Context: tried + ", and it never named a failure of the change, so the writer cannot fix it",
		Example: cmp.Or(textx.Clip(f.tail, 600), "it printed nothing"),
		Options: []registry.Option{
			{Label: optionSkip, Means: "the precheck no longer runs " + cmp.Or(f.skip, f.key) + " for this task, and the PR body names it as skipped (CI still runs it)"},
			{Label: optionLonger, Means: "answer with the bound, such as `" + optionLonger + ": 40m`, longer than its last of " +
				capText(f.bound) + ": it runs with that bound for this task (twice its last one when you name none, or a shorter one)"},
			{Label: optionDrop, Means: "the task stops"},
			{Label: optionWriter, Means: "its failure goes to the writer as any failure of the change, with a fix turn, for this task"},
		},
		Recommended: 0, Why: "it never named a failure of the change, and CI runs it on the PR",
		Evidence: precheckFile,
	}
	switch {
	case f.namesChanged:
		q.Recommended, q.Why = 3, "what it printed names a file the branch changed: the change may be its cause"
	case f.hang:
		q.Recommended, q.Why = 1, "a run that only takes long passes with a longer bound"
	}
	return stopQuestion{
		ask: stopAsk{Kind: stopCheck, Key: faultKey(f.head, f.key), Check: f.key, Skip: f.skip, Bound: f.bound.String(),
			Why: f.key + " " + f.what + " at " + short(f.head)},
		q: q,
	}
}
