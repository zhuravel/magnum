package factory

// The factory's own proof (spec 6.4): the writer names its spec files in
// proof.json, never their results, and the factory runs them itself. On a
// detached checkout of the base with the head's spec files laid on it, the
// spec must fail (a bug, a feature: the kind's proof is red); at the head it
// must pass. A chore's kind needs no red run: its specs pass at the head.
// The commands, the exits, the output and the trees are recorded, as two
// checks and the proof artifact (factory-proof.json). The proof holds for
// its head only: a new commit voids it.
//
// The base run is a guarded step: before it detaches the copy it writes
// detached.json in the task's directory (where the copy is, and the branch
// and the head to restore) and a begin event; after the run it restores the
// branch and the head's tree, removes the file and writes an ok or a fail
// event, whatever ended the run (a stop of the daemon too). A daemon killed
// in the middle leaves the file: the next start restores the branch before
// anything else runs in that copy (resumeDetached), never while an agent
// works there and never over a change the base run did not make.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/textx"
)

// The files of a task's directory the coding stage reads and writes: the
// writer's proof.json, the factory's proof (the proof artifact), the
// record of a base run in progress, and the base the branch started from
// (the base_sha artifact).
const (
	proofFile        = "proof.json"
	factoryProofFile = "factory-proof.json"
	detachedFile     = "detached.json"
	baseFile         = "base.json"
)

// ProofFile is proof.json as the writer writes it: the spec files of its
// proof, relative to the checkout (never their results, which the factory
// finds itself).
type ProofFile struct {
	Specs []string `json:"specs"`
}

// Proof is the factory's proof of a head: the spec files it ran, its run on
// the base (none for a kind whose proof is green) and at the head, the head
// and the base (the merge base with origin/<base>) it ran on, whether it
// passed, and why not.
type Proof struct {
	Specs   []string `json:"specs"`
	Base    ProofRun `json:"base"`
	Head    ProofRun `json:"head"`
	HeadSHA string   `json:"head_sha"`
	BaseSHA string   `json:"base_sha"`
	Passed  bool     `json:"passed"`
	Why     string   `json:"why,omitempty"`
}

// ProofRun is one spec run of a proof: the command, its exit, the end of its
// output (redacted), the tree of the commit it ran on (the base's, with the
// head's spec files on it, for the base run) and how long it took.
type ProofRun struct {
	Command string        `json:"command,omitempty"`
	Exit    int           `json:"exit"`
	Output  string        `json:"output,omitempty"`
	Tree    string        `json:"tree,omitempty"`
	Took    time.Duration `json:"took,omitempty"`
}

// Holds reports whether p passed at head: another head voids it.
func (p Proof) Holds(head string) bool { return p.Passed && p.HeadSHA != "" && p.HeadSHA == head }

// outputKeep is how much of a run's output the factory keeps (its end).
const outputKeep = 8 << 10

// readProofFile reads proof.json in dir, newer than since: an object whose
// specs are clean paths relative to the checkout, none twice. No spec is no
// problem here (a chore's proof may name none); the proof says what its
// kind needs.
func readProofFile(dir string, since time.Time) (ProofFile, *endProblem) {
	text, p := readFresh(dir, proofFile, since)
	if p != nil {
		return ProofFile{}, p
	}
	var pf ProofFile
	if err := json.Unmarshal([]byte(text), &pf); err != nil {
		return ProofFile{}, &endProblem{file: proofFile, why: "not valid JSON (" + err.Error() + `): write {"specs": ["spec/…_spec.rb"]}`}
	}
	seen := map[string]bool{}
	for _, s := range pf.Specs {
		switch {
		case s == "" || !filepath.IsLocal(s) || path.Clean(s) != s:
			return ProofFile{}, &endProblem{file: proofFile, why: fmt.Sprintf("names %q, which is no clean path relative to the checkout", s)}
		case seen[s]:
			return ProofFile{}, &endProblem{file: proofFile, why: "names " + s + " twice"}
		}
		seen[s] = true
	}
	return pf, nil
}

// branchHead is the task's branch as its copy has it: HEAD, the base it
// starts from (the merge base with origin/<base>), and what keeps it from
// the factory's checks ("" for nothing): another branch or no branch
// checked out, tracked changes, no commit after origin/<base>.
type branchHead struct {
	Head, Base string
	Problem    string
}

// branchHead reads task t's branch in copy c.
func (e *Engine) branchHead(ctx context.Context, t registry.Task, c Copy, repo config.FactoryRepo) (branchHead, error) {
	git := e.d.Git
	var bh branchHead
	cur, err := git.CurrentBranch(ctx, c.Path)
	if err != nil {
		return bh, err
	}
	if dirty, err := trackedChanges(ctx, git, c.Path); err != nil {
		return bh, err
	} else if len(dirty) > 0 {
		bh.Problem = fmt.Sprintf("git status shows %s in the copy (%s): commit them or undo them",
			plural(len(dirty), "tracked change"), clipList(dirty))
		return bh, nil
	}
	if cur != t.Branch {
		bh.Problem = fmt.Sprintf("the copy is on %s, not on %s: switch back to it (git switch %s) and commit there",
			cmpJoin(nonEmpty(cur), "a detached HEAD"), t.Branch, t.Branch)
		return bh, nil
	}
	if bh.Head, err = git.RevParse(ctx, c.Path, "HEAD"); err != nil {
		return bh, err
	}
	upstream := "origin/" + baseOf(repo)
	if bh.Base, err = git.MergeBase(ctx, c.Path, bh.Head, upstream); err != nil {
		return bh, err
	}
	commits, err := git.CommitsAfter(ctx, c.Path, upstream, bh.Head, false)
	if err != nil {
		return bh, err
	}
	if len(commits) == 0 {
		bh.Problem = t.Branch + " has no commit after " + upstream + ": commit your change on it"
	}
	return bh, nil
}

// nonEmpty is []string{s}, or none for "".
func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

// trackedChanges are the paths of the work tree at dir with tracked changes
// (untracked files are not changes of the branch), sorted.
func trackedChanges(ctx context.Context, git *gitx.Client, dir string) ([]string, error) {
	entries, err := git.StatusPaths(ctx, dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, en := range entries {
		if !en.Untracked() {
			out = append(out, en.Path)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// clipList is paths joined, the first five of them.
func clipList(paths []string) string {
	if len(paths) <= 5 {
		return strings.Join(paths, ", ")
	}
	return strings.Join(paths[:5], ", ") + fmt.Sprintf(" and %d more", len(paths)-5)
}

// baseOf is the repository's base branch.
func baseOf(repo config.FactoryRepo) string {
	if repo.Base != "" {
		return repo.Base
	}
	return config.DefaultFactoryBase
}

// factoryRepo is the [[factory.repo]] of task t.
func (e *Engine) factoryRepo(t registry.Task) (config.FactoryRepo, error) {
	repo, ok := e.cfg.FactoryRepo(t.Repo)
	if !ok {
		return repo, fmt.Errorf("%s: no [[factory.repo]] for %q", TaskKey(t), t.Repo)
	}
	return repo, nil
}

// copyIdle is the guard before the factory touches task t's copy: no run
// of the task's agents is in flight there (Runtime.AtWork). One that is is
// a wait, naming the role.
func (e *Engine) copyIdle(ctx context.Context, t registry.Task) error {
	if e.d.Runtime == nil {
		return nil
	}
	role, err := e.d.Runtime.AtWork(ctx, t)
	if err != nil {
		return err
	}
	if role != "" {
		return heldError{who: waitAgent, what: "the " + role + " agent works in the copy; the factory's checks wait for its turn to end"}
	}
	return nil
}

// runProof runs the factory's proof of task t's head in copy c (spec 6.4):
// with no agent at work in the copy, the branch checked out with no tracked
// change and a commit after origin/<base>, and the spec files of proof.json
// in the head, it runs them on the base (unless the kind's proof is green)
// and at the head with the repository's specs command (precheck.specs; jest
// for spec files of JavaScript when every one is), under the machine's spec
// lock. It records a spec check for each run (source factory) and the proof
// (factory-proof.json, the proof artifact). A proof that does not hold is a
// Proof with Why set and nil error; the error is a wait (an agent at work)
// or a failure to run.
func (e *Engine) runProof(ctx context.Context, t registry.Task, c Copy) (Proof, error) {
	repo, err := e.factoryRepo(t)
	if err != nil {
		return Proof{}, err
	}
	if e.d.Git == nil {
		return Proof{}, heldError{who: "factory", what: "this build has no git"}
	}
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	pf, p := readProofFile(dir, time.Time{})
	if p != nil {
		return Proof{Why: p.String()}, nil
	}
	bh, err := e.branchHead(ctx, t, c, repo)
	if err != nil {
		return Proof{}, err
	}
	if bh.Problem != "" {
		return Proof{Specs: pf.Specs, Why: bh.Problem}, nil
	}
	if err := e.copyIdle(ctx, t); err != nil {
		return Proof{}, err
	}
	proof := Proof{Specs: pf.Specs, HeadSHA: bh.Head, BaseSHA: bh.Base}
	red := e.cfg.Factory.Kinds[t.Kind].RedRun()
	if red && len(pf.Specs) == 0 {
		proof.Why = "proof.json names no spec: a " + t.Kind + " needs a spec of the change that fails on the base and passes at the head"
		return proof, e.saveProof(ctx, t, proof)
	}
	for _, s := range pf.Specs {
		if _, err := e.d.Git.FileAt(ctx, c.Path, bh.Head, s); err != nil {
			proof.Why = "proof.json names " + s + ", which the head does not have: commit the spec"
			return proof, e.saveProof(ctx, t, proof)
		}
	}
	if len(pf.Specs) == 0 {
		proof.Passed = true // a green proof with no spec: the precheck runs the specs the change affects
		return proof, e.saveProof(ctx, t, proof)
	}
	cmd := specCommand(repo, pf.Specs)
	if cmd == "" {
		proof.Why = repo.Repo + "'s precheck names no specs command, so the factory cannot run the proof's specs"
		return proof, e.saveProof(ctx, t, proof)
	}
	unlock, err := e.specsLock(ctx, t, "proof")
	if err != nil {
		return proof, err
	}
	defer unlock()
	timeout := capOf(repo)
	if red {
		e.noteCheck(t.ID, "proof: runs "+strings.Join(pf.Specs, ", ")+" on the base")
		run, denied, err := e.baseRun(ctx, t, c, bh.Base, bh.Head, pf.Specs, cmd, timeout)
		proof.Base = run
		if err != nil {
			return proof, err
		}
		if denied != "" {
			proof.Why = "the specs command is denied: " + denied
			return proof, e.saveProof(ctx, t, proof)
		}
		e.recordRun(ctx, t, "spec", bh.Base, run)
		if run.Exit == 0 {
			proof.Why = strings.Join(pf.Specs, ", ") + " passes on the base (" + short(bh.Base) + "), so it proves nothing: " +
				"write a spec that fails without the change"
			return proof, e.saveProof(ctx, t, proof)
		}
	}
	e.noteCheck(t.ID, "proof: runs "+strings.Join(pf.Specs, ", ")+" at the head")
	run, denied, err := e.specRun(ctx, c, cmd, bh.Head, timeout, "proof specs")
	proof.Head = run
	if err != nil {
		return proof, err
	}
	if denied != "" {
		proof.Why = "the specs command is denied: " + denied
		return proof, e.saveProof(ctx, t, proof)
	}
	e.recordRun(ctx, t, "spec", bh.Head, run)
	if dirty, err := trackedChanges(ctx, e.d.Git, c.Path); err != nil {
		return proof, err
	} else if len(dirty) > 0 {
		proof.Why = "the spec run left tracked changes in the copy (" + clipList(dirty) + "): git status must be clean before and after"
		return proof, e.saveProof(ctx, t, proof)
	}
	if run.Exit != 0 {
		proof.Why = strings.Join(pf.Specs, ", ") + " fails at the head (" + short(bh.Head) + ")"
		return proof, e.saveProof(ctx, t, proof)
	}
	proof.Passed = true
	return proof, e.saveProof(ctx, t, proof)
}

// short is a commit's first 12 characters.
func short(sha string) string { return sha[:min(len(sha), 12)] }

// capOf is the repository's precheck cap, which bounds each spec run of a
// proof too.
func capOf(repo config.FactoryRepo) time.Duration {
	if d := repo.Precheck.Cap.Duration; d > 0 {
		return d
	}
	return config.DefaultPrecheckCap
}

// jsSpecExts are the extensions of JavaScript spec files, which jest runs.
var jsSpecExts = []string{".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".coffee"}

// specCommand is the command that runs the spec files: the repository's
// jest when every one is a JavaScript file and jest is configured, else its
// specs command; {files} are the files, shell-quoted ("" for no command).
func specCommand(repo config.FactoryRepo, specs []string) string {
	cmd := repo.Precheck.Specs
	if repo.Precheck.Jest != "" && !slices.ContainsFunc(specs, func(s string) bool { return !slices.Contains(jsSpecExts, filepath.Ext(s)) }) {
		cmd = repo.Precheck.Jest
	}
	if cmd == "" {
		return ""
	}
	return renderCommand(cmd, specs, "")
}

// renderCommand renders a precheck command: {files} as the files,
// shell-quoted, {base} as the base.
func renderCommand(cmd string, files []string, base string) string {
	quoted := make([]string, len(files))
	for i, f := range files {
		quoted[i] = execx.ShellQuote(f)
	}
	return strings.NewReplacer("{files}", strings.Join(quoted, " "), "{base}", base).Replace(cmd)
}

// specRun runs a spec command in copy c at rev (checked out), bounded by
// timeout: the run, or the deny list's reason when it refused the command,
// or the error of a command that did not run to its end (ctx, a start
// failure).
func (e *Engine) specRun(ctx context.Context, c Copy, cmd, rev string, timeout time.Duration, label string) (ProofRun, string, error) {
	run := ProofRun{Command: cmd}
	if tree, err := e.d.Git.TreeOf(ctx, c.Path, rev); err == nil {
		run.Tree = tree
	}
	start := time.Now()
	res, err := e.script(ctx, c, cmd, label, timeout)
	run.Took = time.Since(start).Round(time.Millisecond)
	code, why, err := ranTo(res, err)
	run.Exit, run.Output = code, outputOf(res)
	return run, why, err
}

// ranTo reads a command's end: its exit (0, or a non-zero exit's), the deny
// list's reason when it refused it, or the error of a command that did not
// run to its end.
func ranTo(res execx.Result, err error) (code int, denied string, _ error) {
	switch {
	case err == nil:
		return res.Code, "", nil
	case errors.Is(err, ErrDenied):
		_, why, _ := strings.Cut(err.Error(), ErrDenied.Error()+": ")
		return -1, why, nil
	}
	if ee, ok := errors.AsType[*execx.ExitError](err); ok {
		return ee.Code, "", nil
	}
	return -1, "", err
}

// outputOf is the end of a command's output (stdout, then stderr),
// redacted.
func outputOf(res execx.Result) string {
	out := strings.TrimSpace(string(res.Stdout) + "\n" + string(res.Stderr))
	if len(out) > outputKeep {
		out = out[len(out)-outputKeep:]
		if i := strings.IndexByte(out, '\n'); i >= 0 {
			out = out[i+1:]
		}
	}
	return execx.Redact(out)
}

// script runs a command of the repository in copy c, as its pool scripts
// run (`mise -C <copy> exec -- env <copy env> /bin/sh -c <script>`), without
// the daemon's secrets, through the deny list.
func (e *Engine) script(ctx context.Context, c Copy, script, label string, timeout time.Duration) (execx.Result, error) {
	if e.run == nil {
		return execx.Result{Code: -1}, heldError{who: "factory", what: "this build runs no commands"}
	}
	return e.run.Run(ctx, execx.Cmd{Name: "mise", Args: miseArgs(c, script), Dir: c.Path, Unset: unsetEnv(e.cfg),
		Timeout: timeout, Mutates: true, Label: label + " in " + c.Name})
}

// recordRun records a spec or other run as a check of task t on rev, by the
// factory: pass for exit 0, else fail (the change's, code).
func (e *Engine) recordRun(ctx context.Context, t registry.Task, kind, rev string, run ProofRun) {
	result, cause := registry.CheckPass, ""
	if run.Exit != 0 {
		result = registry.CheckFail
	}
	e.recordCheck(ctx, registry.Check{TaskID: t.ID, HeadSHA: rev, Kind: kind, Result: result, Cause: cause, Source: "factory",
		Command: run.Command, Exit: run.Exit, Took: run.Took})
}

// recordCheck stores c; a failure is logged (the evidence is lost, the
// check's verdict stands).
func (e *Engine) recordCheck(ctx context.Context, c registry.Check) {
	if _, err := e.reg.RecordCheck(context.WithoutCancel(ctx), c); err != nil {
		e.log.Warn("record a check", "task", c.TaskID, "kind", c.Kind, "err", err)
	}
}

// saveProof writes the proof to factory-proof.json in the task's directory
// and records it as the proof artifact.
func (e *Engine) saveProof(ctx context.Context, t registry.Task, p Proof) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return e.saveArtifact(ctx, t, "proof", factoryProofFile, append(b, '\n'))
}

// saveArtifact writes name in task t's directory and records it as an
// artifact of kind (a new revision when its content changed).
func (e *Engine) saveArtifact(ctx context.Context, t registry.Task, kind, name string, b []byte) error {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := fsx.WriteFileAtomic(filepath.Join(dir, name), b, 0o600); err != nil {
		return err
	}
	if err := e.reg.RecordArtifact(context.WithoutCancel(ctx), registry.Artifact{TaskID: t.ID, Kind: kind, Path: name, SHA256: sha256Hex(b)}); err != nil {
		e.log.Warn("record an artifact", "task", t.ID, "file", name, "err", err)
	}
	return nil
}

// detachedRecord is detached.json: a base run in progress in a task's copy
// (its path and name), the branch and the head to restore after it, the
// base it detached at, and the spec files it laid on the base (the only
// tracked changes it makes).
type detachedRecord struct {
	Copy     string    `json:"copy"`
	CopyName string    `json:"copy_name"`
	Branch   string    `json:"branch"`
	Base     string    `json:"base"`
	Head     string    `json:"head"`
	Specs    []string  `json:"specs"`
	At       time.Time `json:"at"`
}

// baseRun is a run of cmd on the base: detachForBase, the run, then the
// branch and the head's tree restored (restoreBranch), whatever ended the
// run, with the begin event's ok or fail.
func (e *Engine) baseRun(ctx context.Context, t registry.Task, c Copy, base, head string, specs []string, cmd string,
	timeout time.Duration) (ProofRun, string, error) {
	if err := e.detachForBase(ctx, t, c, base, head, specs); err != nil {
		return ProofRun{Command: cmd}, "", err
	}
	run, denied, err := e.specRun(ctx, c, cmd, base, timeout, "specs on the base")
	rec := detachedRecord{Copy: c.Path, CopyName: c.Name, Branch: t.Branch, Head: head, Specs: specs}
	ectx := context.WithoutCancel(ctx) // the run's end is written whatever ended it
	rerr := e.restoreBranch(ectx, t, rec)
	switch {
	case rerr != nil:
		e.event(ectx, t.ID, "proof", registry.PhaseFail, fmt.Sprintf("base run: %s could not be restored in %s: %v", t.Branch, c.Name, rerr))
	case err != nil:
		e.event(ectx, t.ID, "proof", registry.PhaseFail, fmt.Sprintf("base run stopped (%s); %s restored at %s",
			textx.Clip(textx.FirstLine(err.Error()), 200), t.Branch, short(head)))
	default:
		e.event(ectx, t.ID, "proof", registry.PhaseOK, fmt.Sprintf("base run of %s on %s exited %d; %s restored at %s",
			strings.Join(specs, ", "), short(base), run.Exit, t.Branch, short(head)))
	}
	return run, denied, errors.Join(err, rerr)
}

// detachForBase is the start of a base run in task t's copy c: the guard
// (no agent at work in the copy), detached.json and the begin event first,
// then the copy detached at base (its tracked changes discarded: there are
// none, the caller saw a clean tree) with specs laid on it from head. Its
// end is restoreBranch's; a daemon that dies before it leaves the record,
// which the next start acts on (resumeDetached).
func (e *Engine) detachForBase(ctx context.Context, t registry.Task, c Copy, base, head string, specs []string) error {
	if err := e.copyIdle(ctx, t); err != nil {
		return err
	}
	rec := detachedRecord{Copy: c.Path, CopyName: c.Name, Branch: t.Branch, Base: base, Head: head, Specs: specs, At: e.now()}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := fsx.WriteFileAtomic(filepath.Join(dir, detachedFile), append(b, '\n'), 0o600); err != nil {
		return err
	}
	e.event(ctx, t.ID, "proof", registry.PhaseBegin, fmt.Sprintf("base run in %s: detached at %s with %d spec files of %s; %s comes back after it",
		c.Name, short(base), len(specs), short(head), t.Branch))
	if err := e.d.Git.SwitchDetach(ctx, c.Path, base); err != nil {
		return errors.Join(err, e.restoreBranch(context.WithoutCancel(ctx), t, rec))
	}
	if len(specs) > 0 {
		if err := e.d.Git.RestoreFrom(ctx, c.Path, head, specs...); err != nil {
			return errors.Join(err, e.restoreBranch(context.WithoutCancel(ctx), t, rec))
		}
	}
	return nil
}

// restoreBranch ends a base run: the copy back on the branch at its tip,
// with the tip's tree (the base run's changes discarded: a spec file it laid
// that the base lacks is removed first, since a switch never overwrites an
// untracked file; the branch has it), and detached.json removed.
func (e *Engine) restoreBranch(ctx context.Context, t registry.Task, rec detachedRecord) error {
	for _, s := range rec.Specs {
		if _, err := e.d.Git.FileAt(ctx, rec.Copy, "HEAD", s); err == nil {
			continue
		}
		if err := os.Remove(filepath.Join(rec.Copy, s)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := e.d.Git.SwitchDetach(ctx, rec.Copy, "refs/heads/"+rec.Branch); err != nil {
		return err
	}
	if err := e.d.Git.Switch(ctx, rec.Copy, rec.Branch); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), detachedFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// resumeDetached is the guard of a base run a stopped daemon cut short: when
// task t's directory holds detached.json and no check of the task runs, it
// restores the branch and the head's tree in that copy before anything else
// runs there (startup does it for every task, each pass of a live task
// before its stage). An agent at work in the copy is a wait; a tracked
// change the base run did not make (one outside its spec files: yours) is
// never discarded, and a live task goes to NEEDS YOU naming it. held is
// the wait, when there is one.
func (e *Engine) resumeDetached(ctx context.Context, t registry.Task) (held error) {
	if e.d.Git == nil || e.checkRuns(t.ID) {
		return nil
	}
	path := filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), detachedFile)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	var rec detachedRecord
	if err == nil {
		err = json.Unmarshal(b, &rec)
	}
	if err != nil || rec.Copy == "" || rec.Branch == "" {
		e.log.Warn("a base run's record is unreadable; it is left for you", "task", t.ID, "file", path, "err", err)
		return nil
	}
	name := cmpJoin(nonEmpty(rec.CopyName), rec.Copy)
	if err := e.copyIdle(ctx, t); err != nil {
		if h, ok := errors.AsType[heldError](err); ok {
			what := name + " was left detached by a base run cut short; it is restored once no agent works in it (" + h.what + ")"
			e.waitOn(ctx, t, waitCopy, what)
			return heldError{who: waitCopy, what: what}
		}
		return err
	}
	dirty, err := trackedChanges(ctx, e.d.Git, rec.Copy)
	if err != nil {
		return err
	}
	if theirs := slices.DeleteFunc(dirty, func(p string) bool { return slices.Contains(rec.Specs, p) }); len(theirs) > 0 {
		what := fmt.Sprintf("%s was left detached at %s by a base run cut short, with changes it did not make (%s): "+
			"keep or drop them, then put %s back on %s (git switch %s)", name, short(rec.Base), clipList(theirs), name, rec.Branch, rec.Branch)
		e.event(ctx, t.ID, "proof", registry.PhaseFail, "restore after a base run cut short: "+what)
		if isLive(t) {
			e.toOperator(ctx, t, what, "a base run cut short left changes it did not make")
		}
		return heldError{who: waitsYou, what: what}
	}
	e.event(ctx, t.ID, "proof", registry.PhaseBegin, "restores "+rec.Branch+" in "+name+" after a base run cut short")
	if err := e.restoreBranch(ctx, t, rec); err != nil {
		e.event(ctx, t.ID, "proof", registry.PhaseFail, "restore "+rec.Branch+" in "+name+": "+err.Error())
		return err
	}
	e.event(ctx, t.ID, "proof", registry.PhaseOK, "restored "+rec.Branch+" in "+name+" after a base run cut short")
	return nil
}
