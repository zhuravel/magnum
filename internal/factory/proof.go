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
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/scrub"
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
// passed, and why not; ForYou when the writer cannot fix why (the
// configuration's or the machine's: the task needs you, naming the key).
type Proof struct {
	Specs   []string `json:"specs"`
	Base    ProofRun `json:"base"`
	Head    ProofRun `json:"head"`
	HeadSHA string   `json:"head_sha"`
	BaseSHA string   `json:"base_sha"`
	Passed  bool     `json:"passed"`
	Why     string   `json:"why,omitempty"`
	ForYou  bool     `json:"for_you,omitempty"`
}

// ProofRun is one spec run of a proof: the command, its exit, the end of its
// output (its secrets masked: outputOf), the tree of the commit it ran on
// (the base's, with the head's spec files on it, for the base run), how
// long it took, and whether it hung (the factory stopped it at its bound:
// exit -1).
type ProofRun struct {
	Command string        `json:"command,omitempty"`
	Exit    int           `json:"exit"`
	Output  string        `json:"output,omitempty"`
	Tree    string        `json:"tree,omitempty"`
	Took    time.Duration `json:"took,omitempty"`
	Hung    bool          `json:"hung,omitempty"`
	// out and errOut are the ends of what it printed on stdout and on
	// stderr, raw (precheckTailWindow bytes each, never kept in a file as
	// they are): a precheck's tails of a run its cap or bound stopped.
	out, errOut []byte
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
// starts from (the merge base with origin/<base>), its commits after
// origin/<base>, and what keeps it from the factory's checks ("" for
// nothing): another branch or no branch checked out, tracked changes, no
// commit after origin/<base>.
type branchHead struct {
	Head, Base string
	Commits    int
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
	bh.Commits = len(commits)
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

// copyIdle is the guard before the factory changes task t's copy: no run
// of the task's agents is in flight there, and you are in none of the
// task's panes, where your edits are (Runtime.AtWork). Either is a wait,
// saying who.
func (e *Engine) copyIdle(ctx context.Context, t registry.Task) error {
	if e.d.Runtime == nil {
		return nil
	}
	role, you, err := e.d.Runtime.AtWork(ctx, t)
	switch {
	case err != nil:
		return err
	case role != "" && you:
		return heldError{who: waitAgent, what: "you're in the " + role + " pane; the factory changes the copy once you leave it"}
	case role != "":
		return heldError{who: waitAgent, what: "the " + role + " agent works in the copy; the factory's checks wait for its turn to end"}
	}
	return nil
}

// runProof runs the factory's proof of task t's head in copy c (spec 6.4):
// with nobody at work in the copy (copyIdle), the branch checked out with no
// tracked change and a commit after origin/<base>, and the spec files of
// proof.json in the head, it runs them on the base (unless the kind's proof
// is green) and at the head with the repository's specs command
// (precheck.specs; jest for spec files of JavaScript when every one is),
// under the machine's spec lock, once it looked at the copy again after
// that lock's wait (afterWait: no spec runs over a change made while it
// waited). It records a spec check for each run
// (source factory) and the proof (factory-proof.json, the proof artifact).
// A spec run bounded by the precheck's cap that does not end is a result,
// never a failure to run: red on the base, a failure at the head. A proof
// that does not hold is a Proof with Why set and nil error: ForYou when the
// writer cannot fix it (no specs command, a command the deny list refuses
// or that does not start: the configuration's or the machine's). The error
// is a wait (somebody at work in the copy) or a failure to run (git, the
// daemon stopping).
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
	started := e.now() // before any wait: a change after it is the proof's own, or somebody else's
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
	done := func() (Proof, error) { return proof, e.saveProof(ctx, t, proof) }
	forYou := func(why string) (Proof, error) {
		proof.Why, proof.ForYou = why, true
		return done()
	}
	red := e.cfg.Factory.Kinds[t.Kind].RedRun()
	if red && len(pf.Specs) == 0 {
		proof.Why = "proof.json names no spec: a " + t.Kind + " needs a spec of the change that fails on the base and passes at the head"
		return done()
	}
	for _, s := range pf.Specs {
		if has, err := e.d.Git.HasFile(ctx, c.Path, bh.Head, s); err != nil || !has { // a probe: a miss warns nothing
			proof.Why = "proof.json names " + s + ", which the head does not have: commit the spec"
			return done()
		}
	}
	if len(pf.Specs) == 0 {
		proof.Passed = true // a green proof with no spec: the precheck runs the specs the change affects
		return done()
	}
	cmd := specCommand(repo, pf.Specs)
	if cmd == "" {
		return forYou(repo.Repo + "'s [[factory.repo]] names no precheck.specs, so the factory cannot run the proof's spec files")
	}
	unlock, err := e.specsLock(ctx, t, "proof")
	if err != nil {
		return proof, err
	}
	defer unlock()
	if started, err = e.afterWait(ctx, t, c, started, "proof", "the proof"); err != nil { // no spec runs over what changed while it waited
		if k, ok := errors.AsType[keptChanges](err); ok {
			return forYou(k.why)
		}
		return proof, err
	}
	timeout := capOf(repo)
	key := "precheck.specs"
	if cmd != renderCommand(repo.Precheck.Specs, pf.Specs, "") {
		key = "precheck.jest"
	}
	if red {
		e.noteCheck(t.ID, "proof: runs "+strings.Join(pf.Specs, ", ")+" on the base")
		run, problem, err := e.baseRun(ctx, t, c, started, bh.Base, bh.Head, pf.Specs, cmd, timeout)
		proof.Base = run
		if k, ok := errors.AsType[keptChanges](err); ok {
			return forYou(k.why)
		}
		if err != nil {
			return proof, err
		}
		if problem != "" {
			return forYou(repo.Repo + "'s " + key + " `" + cmd + "` " + problem + problemFix(key, cmd, problem))
		}
		e.recordRun(ctx, t, "spec", bh.Base, run)
		if run.Exit == 0 {
			proof.Why = strings.Join(pf.Specs, ", ") + " passes on the base (" + short(bh.Base) + "), so it proves nothing: " +
				"write a spec that fails without the change"
			return done()
		}
	}
	e.noteCheck(t.ID, "proof: runs "+strings.Join(pf.Specs, ", ")+" at the head")
	run, problem, err := e.specRun(ctx, c, cmd, bh.Head, timeout, "proof specs")
	proof.Head = run
	// What the spec run changed goes back, guarded, however the run ended
	// (a drain too): the next check must find the tree as the branch has it.
	restored, kept, rerr := e.restoreOwn(ctx, t, c, started, "the proof's spec run")
	switch {
	case kept != "": // what it kept for you is its result, whatever ended the run (kept.json records it)
		return forYou(kept)
	case err != nil:
		return proof, err
	case rerr != nil:
		return proof, rerr
	case problem != "":
		return forYou(repo.Repo + "'s " + key + " `" + cmd + "` " + problem + problemFix(key, cmd, problem))
	}
	e.recordRun(ctx, t, "spec", bh.Head, run)
	if len(restored) > 0 { // as the precheck does: the files go back, the run is judged on its exit
		e.event(ctx, t.ID, "proof", registry.PhaseInfo, "the spec run at "+short(bh.Head)+" changed tracked files in the copy ("+
			clipList(restored)+"): the factory restored them")
	}
	switch {
	case run.Hung:
		proof.Why = strings.Join(pf.Specs, ", ") + " does not end at the head (" + short(bh.Head) + ") within " + capText(timeout) + ": it hangs"
		return done()
	case run.Exit != 0:
		proof.Why = strings.Join(pf.Specs, ", ") + " fails at the head (" + short(bh.Head) + ")"
		return done()
	}
	proof.Passed = true
	return done()
}

// capText is a cap as a wait says it ("10m", "10m30s", "1h5m", "300ms"):
// to the second, without the units that are zero.
func capText(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	d = d.Round(time.Second)
	var b strings.Builder
	for _, u := range []struct {
		d    time.Duration
		unit string
	}{{time.Hour, "h"}, {time.Minute, "m"}, {time.Second, "s"}} {
		if n := d / u.d; n > 0 {
			fmt.Fprintf(&b, "%d%s", n, u.unit)
		}
		d %= u.d
	}
	return b.String()
}

// short is a commit's first 12 characters.
func short(sha string) string { return sha[:min(len(sha), 12)] }

// capOf is the repository's precheck cap, which bounds each spec run of a
// proof (a precheck's own grows with the change: precheckCap).
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
// timeout. A run that does not end within it is a result (Hung, exit -1),
// as a red one is. problem is what keeps the command from running at all,
// for you: the deny list refused it, or it is missing or not executable
// (startFailure; a shell's exit 126 or 127). The error is the caller's
// context ending (the daemon stops), or a start failure of the moment
// (EAGAIN, ENOMEM), which the checks try again.
func (e *Engine) specRun(ctx context.Context, c Copy, cmd, rev string, timeout time.Duration, label string) (ProofRun, string, error) {
	run := ProofRun{Command: cmd}
	if tree, err := e.d.Git.TreeOf(ctx, c.Path, rev); err == nil {
		run.Tree = tree
	}
	start := time.Now()
	res, _, err := e.dbScript(ctx, c, cmd, label, timeout, nil) // specs use the copy's databases: in turn with the reviewers
	run.Took = time.Since(start).Round(time.Millisecond)
	code, denied, err := ranTo(res, err)
	run.Exit, run.Output = code, outputOf(res)
	run.out, run.errOut = lastBytes(res.Stdout, precheckTailWindow), lastBytes(res.Stderr, precheckTailWindow)
	switch {
	case denied != "":
		return run, "is denied: " + denied, nil
	case err == nil && notRunnable(code) != "":
		return run, notRunnable(code), nil
	case err == nil:
		return run, "", nil
	case ctx.Err() != nil:
		return run, "", err
	case errors.Is(err, context.DeadlineExceeded):
		run.Exit, run.Hung = -1, true
		run.Output = strings.TrimSpace(run.Output + "\n[the factory stopped it: it did not end within " + capText(timeout) + "]")
		return run, "", nil
	}
	if why, transient := startFailure(err); !transient {
		return run, why, nil
	}
	return run, "", fmt.Errorf("%s did not start: %w", cmd, err)
}

// startFailure says why a command did not start (err: not its exit, not the
// caller's context): missing or not executable, the configuration's
// (transient false), or a failure of the moment, the machine's (EAGAIN,
// ENOMEM: tried again).
func startFailure(err error) (why string, transient bool) {
	line := textx.Clip(textx.FirstLine(err.Error()), 200)
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.ENOEXEC) {
		return "is missing or not executable: " + line, false
	}
	return "did not start: " + line, true
}

// notRunnable is why a shell's exit says it ran no command: 127, none found;
// 126, not executable ("" for any other exit).
func notRunnable(code int) string {
	switch code {
	case 127:
		return "exited 127: the shell finds no such command"
	case 126:
		return "exited 126: its command is not executable"
	}
	return ""
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

// outputOf is the end of a command's output (stdout, then stderr), its
// secrets masked (scrub.Secrets): the factory keeps it in precheck.md and
// factory-proof.json, which the pr-body agent reads into the PR, and the
// writer's fix turns name them as the evidence of what failed, so the
// emails, hosts, addresses and file names a failure prints stay as printed.
func outputOf(res execx.Result) string {
	out := strings.TrimSpace(string(res.Stdout) + "\n" + string(res.Stderr))
	if len(out) > outputKeep {
		out = out[len(out)-outputKeep:]
		if i := strings.IndexByte(out, '\n'); i >= 0 {
			out = out[i+1:]
		}
	}
	return scrub.Secrets(out)
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

// hasDetachedRecord reports whether task t's directory holds the record of
// a base run that left its copy detached (detached.json).
func (e *Engine) hasDetachedRecord(t registry.Task) bool {
	_, err := os.Stat(filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), detachedFile))
	return err == nil
}

// keptChanges is a discarding git step the factory did not take: it would
// have lost changes the factory did not make, which it left for you, the
// paths; why names them, in plain words. No git step the factory runs in a
// copy discards a change it did not make.
type keptChanges struct {
	why   string
	paths []string
}

func (k keptChanges) Error() string { return k.why }

// baseRun is a run of cmd on the base: detachForBase, the run, then the
// branch and the head's tree restored (restoreBranch), whatever ended the
// run, with the begin event's ok or fail. The restore discards what the run
// changed, so it is guarded as restoreOwn is: with somebody else at work in
// the copy since the check began at since (othersIn), the copy stays
// detached with its record, a fail event says why, and the error is a wait;
// resumeDetached restores it once nobody works there, never over a change
// the factory did not make. A detach the copy's changes refused is a
// keptChanges.
func (e *Engine) baseRun(ctx context.Context, t registry.Task, c Copy, since time.Time, base, head string, specs []string, cmd string,
	timeout time.Duration) (ProofRun, string, error) {
	if err := e.detachForBase(ctx, t, c, base, head, specs); err != nil {
		if _, kept := errors.AsType[keptChanges](err); !kept {
			e.event(context.WithoutCancel(ctx), t.ID, "proof", registry.PhaseFail, "base run in "+c.Name+" did not start: "+
				textx.Clip(textx.FirstLine(err.Error()), 300))
		}
		return ProofRun{Command: cmd}, "", err
	}
	run, denied, err := e.specRun(ctx, c, cmd, base, timeout, "specs on the base")
	rec := detachedRecord{Copy: c.Path, CopyName: c.Name, Branch: t.Branch, Base: base, Head: head, Specs: specs}
	ectx := context.WithoutCancel(ctx) // the run's end is written whatever ended it
	if who := e.othersIn(ectx, t, since); who != "" {
		what := c.Name + " stays detached by the base run of " + TaskKey(t) + ": " + who
		e.event(ectx, t.ID, "proof", registry.PhaseFail, "base run: "+what+"; it is restored once nobody works in it, never over "+
			"changes the factory did not make")
		return run, "", errors.Join(heldError{who: waitCopy, what: what}, err)
	}
	rerr := e.restoreBranch(ectx, t, rec, true)
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

// detachForBase is the start of a base run in task t's copy c: the guards
// first, nobody at work in the copy (copyIdle) and nothing in it the switch
// would discard (its tree as the branch has it: no tracked change, and no
// file git does not track that the switch would lose, inTheWay; the callers
// put back their own changes before, so whatever is there is not the
// factory's, and a keptChanges names it for you, with a fail event and
// kept.json); then detached.json and the begin event, then the copy detached
// at base (a switch that refuses to write over a file git does not track)
// with specs laid on it from head. Its end is restoreBranch's; a daemon that
// dies before it leaves the record, which the next start acts on
// (resumeDetached).
func (e *Engine) detachForBase(ctx context.Context, t registry.Task, c Copy, base, head string, specs []string) error {
	if err := e.copyIdle(ctx, t); err != nil {
		return err
	}
	foreign, err := e.inTheWay(ctx, c.Path, base, head, 'D')
	if err != nil {
		return err
	}
	if dirty, err := trackedChanges(ctx, e.d.Git, c.Path); err != nil {
		return err
	} else if foreign = mergePaths(foreign, dirty); len(foreign) > 0 {
		why := fmt.Sprintf("%s has changes the factory did not make (%s), so it was not detached for the base run: "+
			"keep or undo them, then let the task go on", c.Name, clipList(foreign))
		e.event(ctx, t.ID, "proof", registry.PhaseFail, "base run: "+why)
		e.keepFor(t, c, time.Time{}, dirty, false, why)
		return keptChanges{why, foreign}
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
	if err := e.d.Git.SwitchDetachKeepUntracked(ctx, c.Path, base); err != nil {
		rerr := e.restoreBranch(context.WithoutCancel(ctx), t, rec, true)
		if files, line, ok := refusal(err); ok && rerr == nil { // in the way of the switch, though the look saw nothing: yours
			why := c.Name + " was not detached for the base run: " + refusedWhat(c.Name, files, line)
			e.event(ctx, t.ID, "proof", registry.PhaseFail, "base run: "+why)
			return keptChanges{why, files}
		}
		return errors.Join(err, rerr)
	}
	if len(specs) > 0 {
		if err := e.d.Git.RestoreFrom(ctx, c.Path, head, specs...); err != nil {
			return errors.Join(err, e.restoreBranch(context.WithoutCancel(ctx), t, rec, true))
		}
	}
	return nil
}

// inTheWay are the files of the copy at dir that git tracks none of
// (untracked or ignored: gitx.NotTracked) and that a switch between base
// and head would lose: at a path the head's change has with status
// (gitx.ChangedFiles, which reads no patch, so a change of any size fits:
// 'D' for the base's paths, which a switch to the base writes; 'A' for the
// head's, which a switch back to the branch writes), or in a directory at
// such a path. A directory of tracked files there (a file the head turned
// into one) and the tracked file of a case-only rename are no such file.
func (e *Engine) inTheWay(ctx context.Context, dir, base, head string, status byte) ([]string, error) {
	changes, err := e.d.Git.ChangedFiles(ctx, dir, base, head)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, f := range changes {
		if f.Status == status {
			paths = append(paths, f.Path)
		}
	}
	if len(paths) == 0 {
		return nil, nil
	}
	others, err := e.d.Git.NotTracked(ctx, dir)
	if err != nil {
		return nil, err
	}
	return lostBy(dir, paths, others), nil
}

// lostBy are what of others (gitx.NotTracked's entries in the work tree at
// dir) a switch that writes paths would lose: an entry at one of the paths,
// an entry under a directory at one, a file where one of their directories
// would be, or the file at one that lies in a directory git tracks nothing
// in. Names compare without case, as macOS's file system compares them (on
// another it errs on the side of keeping).
func lostBy(dir string, paths, others []string) []string {
	type entry struct{ fold, name string }
	es := make([]entry, len(others))
	for i, o := range others {
		es[i] = entry{strings.ToLower(o), o}
	}
	slices.SortFunc(es, func(a, b entry) int { return strings.Compare(a.fold, b.fold) })
	find := func(s string) (int, bool) {
		return slices.BinarySearchFunc(es, s, func(e entry, s string) int { return strings.Compare(e.fold, s) })
	}
	var out []string
	for _, p := range paths {
		fold := strings.ToLower(p)
		if i, ok := find(fold); ok {
			out = append(out, es[i].name)
		}
		i, _ := find(fold + "/")
		for ; i < len(es) && strings.HasPrefix(es[i].fold, fold+"/"); i++ {
			out = append(out, es[i].name)
		}
		for d := path.Dir(fold); d != "." && d != "/"; d = path.Dir(d) {
			if i, ok := find(d); ok { // a file where the switch makes a directory
				out = append(out, es[i].name)
				break
			}
			if _, ok := find(d + "/"); ok {
				if _, err := os.Lstat(filepath.Join(dir, p)); err == nil {
					out = append(out, p)
				}
				break
			}
		}
	}
	return mergePaths(out, nil)
}

// mergePaths is a and b in one sorted list, each path once.
func mergePaths(a, b []string) []string {
	out := slices.Concat(a, b)
	slices.Sort(out)
	return slices.Compact(out)
}

// restoreBranch ends a base run: the copy back on the branch at its tip,
// with the tip's tree, and detached.json removed. It discards what differs
// from the branch (a spec file it laid that the base lacks is removed first,
// since a switch never writes over a file git does not track, an ignored one
// included), so it is guarded: with own, the caller vouches that nobody else
// could have changed the copy since the check began; without it, nothing in
// the copy may be foreign (foreignIn), else it is a keptChanges, and nothing
// is switched or removed.
func (e *Engine) restoreBranch(ctx context.Context, t registry.Task, rec detachedRecord, own bool) error {
	if !own {
		foreign, err := e.foreignIn(ctx, rec)
		if err != nil {
			return err
		}
		if len(foreign) > 0 {
			return keptChanges{fmt.Sprintf("changes the factory did not make (%s)", clipList(foreign)), foreign}
		}
	}
	for _, s := range rec.Specs {
		if has, err := e.d.Git.HasFile(ctx, rec.Copy, "HEAD", s); err == nil && has { // a probe: a laid spec HEAD lacks warns nothing
			continue
		}
		if err := os.Remove(filepath.Join(rec.Copy, s)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := e.d.Git.SwitchDetachKeepUntracked(ctx, rec.Copy, "refs/heads/"+rec.Branch); err != nil {
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

// foreignIn are what in a copy a base run left detached (rec) is not the
// factory's: every tracked change, and every file git does not track that
// the switch back to the branch would lose (inTheWay 'A'), unless it is a
// spec file the base run laid that is still as the head has it.
func (e *Engine) foreignIn(ctx context.Context, rec detachedRecord) ([]string, error) {
	candidates, err := trackedChanges(ctx, e.d.Git, rec.Copy)
	if err != nil {
		return nil, err
	}
	if rec.Base != "" && rec.Head != "" {
		adds, err := e.inTheWay(ctx, rec.Copy, rec.Base, rec.Head, 'A')
		if err != nil {
			return nil, err
		}
		candidates = mergePaths(candidates, adds)
	}
	var foreign []string
	for _, p := range candidates {
		if !slices.Contains(rec.Specs, p) || !e.asTheHeadHasIt(ctx, rec, p) {
			foreign = append(foreign, p)
		}
	}
	return foreign, nil
}

// asTheHeadHasIt reports whether the copy's file at path p (a spec file a
// base run laid) is as the record's head has it, or gone: nothing of yours
// would be lost with it.
func (e *Engine) asTheHeadHasIt(ctx context.Context, rec detachedRecord, p string) bool {
	got, err := os.ReadFile(filepath.Join(rec.Copy, p))
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	want, err := e.d.Git.FileAt(ctx, rec.Copy, rec.Head, p)
	return err == nil && want == string(got)
}

// resumeDetached is the guard of a base run a stopped daemon cut short: when
// task t's directory holds detached.json and no check of the task runs, it
// acts on the copy before anything else runs there (startup does it for
// every task, each pass of a live task before its stage). It switches a
// copy only when the copy is still as the base run left it: the task still
// holds it (Copies.Reserved), and its HEAD is detached at the record's base.
// A copy back on the record's branch (you switched it back) drops the record
// with an ok event; a copy the task no longer holds, or that is elsewhere,
// drops it with an info event and is left as it is. Before it switches,
// nobody may be at work in the copy (an agent's run, you in a pane: a wait),
// and a change the base run did not make (foreignIn: outside its spec
// files, yours) is never discarded: a live task goes to NEEDS YOU naming it
// and saying how to put the copy back (keptDetached). Its begin event comes
// only once the guard let the restore go on. held is the wait, when there is
// one.
func (e *Engine) resumeDetached(ctx context.Context, t registry.Task) (held error) {
	if e.d.Git == nil || e.checkRuns(t.ID) {
		return nil
	}
	path := filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), detachedFile)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		delete(e.keptCopies, t.ID)
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
	drop := func(phase, text string) error {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		delete(e.keptCopies, t.ID)
		e.event(ctx, t.ID, "proof", phase, text)
		return nil
	}
	switch holds, known, err := e.holdsCopy(ctx, t, rec.CopyName); {
	case err != nil:
		return err
	case !known:
		return nil // this build has no copies to ask: the record waits
	case !holds:
		return drop(registry.PhaseInfo, "the record of a base run cut short in "+name+" is dropped: "+TaskKey(t)+
			" no longer holds the copy, which is left as it is")
	}
	cur, err := e.d.Git.CurrentBranch(ctx, rec.Copy)
	if err != nil {
		return err
	}
	if cur == rec.Branch {
		return drop(registry.PhaseOK, name+" is back on "+rec.Branch+": the record of the base run cut short there is dropped")
	}
	head, err := e.d.Git.RevParse(ctx, rec.Copy, "HEAD")
	if err != nil {
		return err
	}
	if cur != "" || head != rec.Base {
		where := "on " + cur
		if cur == "" {
			where = "detached at " + short(head)
		}
		return drop(registry.PhaseInfo, fmt.Sprintf("%s is %s, not where the base run left it (detached at %s): its record is dropped "+
			"and the copy left as it is", name, where, short(rec.Base)))
	}
	if err := e.copyIdle(ctx, t); err != nil {
		if h, ok := errors.AsType[heldError](err); ok {
			what := name + " was left detached by a base run; it is restored once nobody works in it (" + h.what + ")"
			if isLive(t) { // a task at NEEDS YOU keeps its wait
				e.waitOn(ctx, t, waitCopy, what)
			}
			return heldError{who: waitCopy, what: what}
		}
		return err
	}
	switch foreign, err := e.foreignIn(ctx, rec); {
	case err != nil:
		return err
	case len(foreign) > 0:
		return e.keptDetached(ctx, t, rec, keptChanges{fmt.Sprintf("changes the factory did not make (%s)", clipList(foreign)), foreign}, false)
	}
	e.event(ctx, t.ID, "proof", registry.PhaseBegin, "restores "+rec.Branch+" in "+name+" after a base run")
	err = e.restoreBranch(ctx, t, rec, false) // its guard again, right before its switch
	if k, ok := errors.AsType[keptChanges](err); ok {
		return e.keptDetached(ctx, t, rec, k, true)
	}
	if files, line, ok := refusal(err); ok { // git saw a file of yours in the way that the look did not
		return e.keptDetached(ctx, t, rec, keptChanges{fmt.Sprintf("files git does not track in the way of the switch back (%s), "+
			"which git refused to write over", cmpJoin(files, line)), files}, true)
	}
	if err != nil {
		e.event(ctx, t.ID, "proof", registry.PhaseFail, "restore "+rec.Branch+" in "+name+": "+err.Error())
		e.keptCopies[t.ID] = keptCopy{at: e.now(), copy: rec.Copy} // the sweep tries again once it is due, not each tick
		return err
	}
	delete(e.keptCopies, t.ID)
	e.event(ctx, t.ID, "proof", registry.PhaseOK, "restored "+rec.Branch+" in "+name+" after a base run")
	return nil
}

// keptDetached is resumeDetached's end for a copy a base run left detached
// with changes the factory did not make (k): the copy stays as it is, and
// the task needs you, naming them and saying how to put the copy back. A
// live task goes to NEEDS YOU with a fail event; a task the loop does not
// advance keeps its wait and gets the fail event and an alert once
// (keptCopies), and begun is a begin event that the fail event ends
// whatever was said before. The copy and its kept paths are noted, so the
// sweep looks at it again only when one of them changed (keptDue).
func (e *Engine) keptDetached(ctx context.Context, t registry.Task, rec detachedRecord, k keptChanges, begun bool) error {
	name := cmpJoin(nonEmpty(rec.CopyName), rec.Copy)
	back := "git switch " + rec.Branch
	if len(rec.Specs) > 0 {
		back = "remove the spec files the base run laid there (" + strings.Join(rec.Specs, ", ") + ") or keep them elsewhere, then " + back
	}
	what := fmt.Sprintf("%s was left detached at %s by a base run, with %s: keep or undo them, then put %s back on %s (%s)",
		name, short(rec.Base), k.why, name, rec.Branch, back)
	switch {
	case isLive(t):
		e.event(ctx, t.ID, "proof", registry.PhaseFail, "restore after a base run: "+what)
		e.toOperator(ctx, t, what+retryHint(t), "a base run left changes the factory did not make")
	case begun || e.keptCopies[t.ID].what != what: // a task the loop does not advance: said once
		e.event(ctx, t.ID, "proof", registry.PhaseFail, "restore after a base run: "+what)
		if e.keptCopies[t.ID].what != what {
			e.alert(ctx, t.ID, TaskKey(t)+": "+what)
		}
	}
	e.keptCopies[t.ID] = keptCopy{what: what, at: e.now(), copy: rec.Copy, paths: k.paths, marks: pathMarks(rec.Copy, k.paths)}
	return heldError{who: waitsYou, what: what}
}

// sweepDetached acts on the copies base runs left detached for the tasks
// the loop does not advance (at NEEDS YOU, stopped, ended) as a live task's
// pass does (resumeDetached), each under the panic guard of a task's step
// (safely): a copy is put back whatever its task's state, or held with its
// reason, and their waits stay as they are. A copy kept for you, one whose
// restore failed and one whose sweep panicked is looked at again only when
// it is due (keptDue). The tick runs it only while herdr answers, as the
// rest of its passes: herdr says who is in a copy.
func (e *Engine) sweepDetached(ctx context.Context, tasks []registry.Task) {
	for _, t := range tasks {
		if isLive(t) || !e.hasDetachedRecord(t) || !e.keptDue(t.ID) {
			continue
		}
		err := e.safely(ctx, t, func(ctx context.Context) error { return e.resumeDetached(ctx, t) })
		switch {
		case errors.Is(err, errPanicked): // its event is written; it is tried again once due, not each tick
			e.keptCopies[t.ID] = keptCopy{what: e.keptCopies[t.ID].what, at: e.now()}
		case err != nil && !errors.Is(err, ErrHeld):
			e.log.Warn("restore a copy a base run left detached", "task", t.ID, "err", err)
		}
	}
}

// holdsCopy reports whether task t holds the copy named name (its owner row
// reserves it: Copies.Reserved); known is false without copies to ask.
func (e *Engine) holdsCopy(ctx context.Context, t registry.Task, name string) (holds, known bool, err error) {
	if e.d.Copies == nil {
		return false, false, nil
	}
	rs, err := e.d.Copies.Reserved(ctx)
	if err != nil {
		return false, true, err
	}
	return slices.ContainsFunc(rs, func(r Reservation) bool { return r.TaskID == t.ID && r.Copy == name }), true, nil
}

// restoreOwn puts back what the factory's own commands changed in tracked
// files of task t's copy c (what, for the events: the precheck's commands,
// the proof's spec run): the checks start on a clean tree at since
// (branchHead), so a tracked change is theirs when nobody else could have
// made it. It is a guarded step: with nobody at work in the copy now and
// nobody who could have changed it since (othersIn), a begin event, each path
// back as HEAD has it (gitx.RestoreTracked), and an ok or a fail event. With
// somebody else, or when it cannot tell, it leaves the paths, writes a fail
// event and kept.json (keepFor: they may be the check's own, and go back
// under this guard once the task goes on and nobody else could have changed
// them, settleKept) and returns kept, the reason for you (the task needs
// you: keep or undo them). A put-back that fails records its paths in
// kept.json too, so they are settleKept's and never the writer's. It runs
// whatever ended the check (ctx's end does not stop it).
func (e *Engine) restoreOwn(ctx context.Context, t registry.Task, c Copy, since time.Time, what string) (restored []string, kept string,
	err error) {
	ctx = context.WithoutCancel(ctx)
	dirty, err := trackedChanges(ctx, e.d.Git, c.Path)
	if err != nil || len(dirty) == 0 {
		return nil, "", err
	}
	list := clipList(dirty)
	if who := e.othersIn(ctx, t, since); who != "" {
		kept = fmt.Sprintf("%s changed %s in %s, and %s since it began, so the factory cannot tell its changes from others' "+
			"and left them: keep or undo them, then let the task go on", what, list, c.Name, who)
		e.event(ctx, t.ID, "check", registry.PhaseFail, "left "+list+" in "+c.Name+" as it is: "+who)
		e.keepFor(t, c, since, dirty, true, kept)
		return nil, kept, nil
	}
	e.event(ctx, t.ID, "check", registry.PhaseBegin, "restores "+list+" in "+c.Name+": "+what+" changed it")
	if err := e.d.Git.RestoreTracked(ctx, c.Path, dirty...); err != nil {
		line := textx.Clip(textx.FirstLine(err.Error()), 300)
		e.event(ctx, t.ID, "check", registry.PhaseFail, "restore "+list+" in "+c.Name+": "+line)
		e.keepFor(t, c, since, dirty, true, "the factory could not put back "+list+" in "+c.Name+" ("+
			clipEnd(textx.FirstLine(scrub.Text(err.Error())), 240)+")")
		return nil, "", fmt.Errorf("restore what %s changed in %s (%s): %w", what, c.Name, list, err)
	}
	e.event(ctx, t.ID, "check", registry.PhaseOK, "restored "+list+" in "+c.Name+" as the branch has it")
	return dirty, "", nil
}

// othersIn says who besides the factory's own commands could have changed
// task t's copy since the time since, "" for nobody: an agent of the task at
// work or you in one of its panes now (Runtime.AtWork), or a prompt to one
// of its agents or your input since then (Runtime.TouchedSince). A runtime
// that cannot tell is somebody too: the factory then changes nothing.
func (e *Engine) othersIn(ctx context.Context, t registry.Task, since time.Time) string {
	if e.d.Runtime == nil {
		return ""
	}
	role, you, err := e.d.Runtime.AtWork(ctx, t)
	switch {
	case err != nil:
		return "the factory cannot tell who works in it (" + textx.Clip(textx.FirstLine(err.Error()), 200) + ")"
	case role != "" && you:
		return "you are in the " + role + " pane"
	case role != "":
		return "the " + role + " agent works in it"
	}
	who, err := e.d.Runtime.TouchedSince(ctx, t, since)
	if err != nil {
		return "the factory cannot tell who changed it (" + textx.Clip(textx.FirstLine(err.Error()), 200) + ")"
	}
	return who
}
