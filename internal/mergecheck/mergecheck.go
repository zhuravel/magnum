// Package mergecheck is `magnum merge-check`: whether an open PR's specs
// still pass once another PR is merged. Two PRs that change no common line
// can still break the base branch together (talkable on 2026-10-05: #11939
// with #11979, one failing example; #11959 with #11939, 99), and the
// repository's CI does not run on push.
//
// A check holds a free pool slot (slots.HoldFree, hold_reason Reason),
// merges the PR's head into the merged commit without a work tree (git
// merge-tree, the merge `git merge` makes; a textual conflict is a result)
// and commits the merged tree, unreferenced but for a ref under
// gitx.MergeCheckRefPrefix, so the slot's checkout steps, its schema record
// and its guard read it like any commit. It checks that commit out with the
// slot's checkout steps and readiness (slots.CheckoutHeld, EnsureSchema, the
// repository's prepare and ready commands), runs the spec files the two
// sides touch through the slot's db-lock line, and runs the files of the
// failing examples again at the PR head alone, to tell a clash from a PR
// that was already red. The result goes to a JSON file and a
// merge_check.result event; the slot is released as a round's release does
// (slots.ReleaseHeld), also after a failure or an interrupt, unless Keep;
// any release of the slot deletes the check's refs (slots.MergeCheckRefs),
// the daemon's too after `magnum slots unpin`. It is an experiment the
// operator runs from the CLI; the daemon never does.
package mergecheck

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// Reason is the hold_reason of the slot a check holds: `magnum slots` and
// `magnum status` show it.
const Reason = "merge-check"

const (
	// DefaultCommand runs spec files when Options.Command is empty.
	DefaultCommand = "bin/rspec"
	// DefaultTimeout bounds each spec run when Options.Timeout is 0.
	DefaultTimeout = time.Hour
	// Shell runs the readiness commands and the spec runs as `zsh -lc`, the
	// login shell a round's readiness step and the agents' tools use.
	Shell = "zsh"
	// ResultFile is the check's JSON result, in Options.Dir.
	ResultFile = "result.json"
	// EventKind is the event a check records under the PR's subject.
	EventKind = "merge_check.result"
)

// Verdicts of a check.
const (
	VerdictPass           = "pass"            // no example fails on the merged tree
	VerdictNoSpecs        = "no_specs"        // neither side touches a spec file or a subject that has one
	VerdictConflict       = "conflict"        // the merge does not apply cleanly
	VerdictClash          = "clash"           // an example fails on the merged tree and not at the PR head
	VerdictAlreadyFailing = "already_failing" // every failing example fails at the PR head too
	VerdictUnknown        = "unknown"         // the PR head's run gave no answer for a failing example
	VerdictError          = "error"           // the check did not finish (Detail says why)
)

// What a failing example does at the PR head (Failure.AtHead).
const (
	AtHeadPassed  = "passed"
	AtHeadFailed  = "failed"
	AtHeadPending = "pending"
	AtHeadAbsent  = "absent"  // the PR head has no such example or file: it came with the merged commit
	AtHeadNotRun  = "not_run" // the head's run did not happen or gave no report
)

// Kinds of failures (Failure.Kind).
const (
	KindExample   = "example"
	KindLoadError = "load_error"       // the spec file failed to load
	KindOutside   = "outside_examples" // an error in a hook or the suite's setup
)

// Slots is the part of *slots.Manager a check drives.
type Slots interface {
	HoldFree(ctx context.Context, pool config.Pool, name, hold string) (store.Slot, error)
	CheckoutHeld(ctx context.Context, slot store.Slot, pool config.Pool, hold, sha string) error
	EnsureSchema(ctx context.Context, slot store.Slot, pool config.Pool) (string, error)
	ReleaseHeld(ctx context.Context, slot store.Slot, pool config.Pool, hold string, refs ...string) error
}

// Deps are a check's collaborators.
type Deps struct {
	Store *store.Store
	Git   *gitx.Client
	// Run runs the readiness commands and the spec runs in the slot.
	Run   execx.Runner
	Slots Slots
	Now   func() time.Time // nil: time.Now
	// Progress gets one line per stage (nil: none).
	Progress func(line string)
}

// Options say what a check checks.
type Options struct {
	Pool      config.Pool
	Readiness config.Readiness // the repository's prepare and ready commands (config.Config.ReadinessFor)
	PR        int              // the open PR whose head is merged
	Merged    string           // the merged commit: a full or abbreviated commit id
	MergedPR  int              // the PR Merged merged, when known (labels only)
	Slot      string           // the slot to hold; "" = the least recently used free one
	Keep      bool             // leave the slot held for inspection instead of releasing it
	Command   string           // runs spec files; "" = DefaultCommand
	Magnum    string           // the magnum binary in the db-lock line; "" = magnum on PATH
	Dir       string           // where the run's files go (created)
	Timeout   time.Duration    // each spec run's limit; 0 = DefaultTimeout
}

// Result is a check's outcome, written to ResultFile.
type Result struct {
	Repo      string    `json:"repo"`
	PR        int       `json:"pr"`
	Head      string    `json:"head,omitempty"`
	Merged    string    `json:"merged"`
	MergedPR  int       `json:"merged_pr,omitempty"`
	Tree      string    `json:"tree,omitempty"` // the merged tree's local commit
	Slot      string    `json:"slot,omitempty"`
	Verdict   string    `json:"verdict"`
	Detail    string    `json:"detail,omitempty"`
	Conflicts []string  `json:"conflicts,omitempty"`
	Specs     []string  `json:"specs,omitempty"` // the spec files run on the merged tree
	MergedRun *SpecRun  `json:"merged_run,omitempty"`
	HeadRun   *SpecRun  `json:"head_run,omitempty"`
	Failures  []Failure `json:"failures,omitempty"`
	Released  bool      `json:"released"`
	Kept      bool      `json:"kept,omitempty"`
	// ReleaseError is why the release failed (the slot stays held).
	ReleaseError string    `json:"release_error,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	EndedAt      time.Time `json:"ended_at"`
	// File is where the result was written ("" when it could not be).
	File string `json:"-"`
}

// SpecRun is a tree's checkout and spec run.
type SpecRun struct {
	Tree      string   `json:"tree"` // "merged" or "head"
	SHA       string   `json:"sha"`
	Readiness []Check  `json:"readiness,omitempty"`
	Files     []string `json:"files,omitempty"`
	Examples  int      `json:"examples"`
	Failed    int      `json:"failed"`
	Pending   int      `json:"pending,omitempty"`
	// ErrorsOutside counts the errors outside examples (files that failed to
	// load, hooks).
	ErrorsOutside int    `json:"errors_outside_examples,omitempty"`
	Exit          int    `json:"exit"`
	Duration      string `json:"duration,omitempty"`
	Report        string `json:"report,omitempty"` // the runner's JSON report
	Log           string `json:"log,omitempty"`    // its output
}

// Check is one readiness step of a checkout.
type Check struct {
	Kind    string `json:"kind"` // schema, prepare or ready
	Command string `json:"command,omitempty"`
	OK      bool   `json:"ok"`
	Detail  string `json:"detail,omitempty"`
}

// Failure is a failing example (or error) on the merged tree and what it
// does at the PR head.
type Failure struct {
	Kind        string `json:"kind"`
	File        string `json:"file,omitempty"`
	Line        int    `json:"line,omitempty"`
	ID          string `json:"id,omitempty"`
	Description string `json:"description"`
	Error       string `json:"error,omitempty"`
	AtHead      string `json:"at_head"`
	Verdict     string `json:"verdict"` // clash, already_failing or unknown
}

// check is one run of Run.
type check struct {
	d   Deps
	o   Options
	res Result
	sl  store.Slot
}

// Run runs a check: it holds a slot, checks, releases the slot (unless
// Keep; with a context that an interrupt does not cancel) and records the
// result. The error is the check's (the result's Verdict is then
// VerdictError), the release's or the record's; a slot it could not hold is
// an error with nothing recorded.
func Run(ctx context.Context, d Deps, o Options) (Result, error) {
	if d.Now == nil {
		d.Now = time.Now
	}
	o.Command = cmp.Or(strings.TrimSpace(o.Command), DefaultCommand)
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	c := &check{d: d, o: o}
	c.res = Result{Repo: o.Pool.Repo, PR: o.PR, Merged: o.Merged, MergedPR: o.MergedPR, StartedAt: d.Now().UTC()}
	switch {
	case o.PR <= 0:
		return c.res, fmt.Errorf("merge-check: invalid PR number %d", o.PR)
	case strings.TrimSpace(o.Merged) == "":
		return c.res, errors.New("merge-check: no merged commit")
	case o.Dir == "":
		return c.res, errors.New("merge-check: no directory for the run's files")
	}
	sl, err := d.Slots.HoldFree(ctx, o.Pool, o.Slot, Reason)
	if err != nil {
		return c.res, fmt.Errorf("merge-check: hold a slot: %w", err)
	}
	c.sl, c.res.Slot = sl, sl.Name
	c.progress("holding %s (%s)", sl.Name, sl.Path)
	runErr := os.MkdirAll(o.Dir, 0o700)
	if runErr == nil {
		runErr = c.run(ctx)
	}
	if runErr != nil {
		c.res.Verdict, c.res.Detail = VerdictError, textx.Clip(execx.Redact(runErr.Error()), 1000)
	}
	c.progress("%s: %s", c.res.Verdict, Summary(c.res))
	relErr := c.release(ctx)
	c.res.EndedAt = d.Now().UTC()
	recErr := c.record(context.WithoutCancel(ctx))
	return c.res, errors.Join(runErr, relErr, recErr)
}

func (c *check) progress(format string, args ...any) {
	if c.d.Progress != nil {
		c.d.Progress(execx.Redact(fmt.Sprintf(format, args...)))
	}
}

// ref is the check's ref name in the held slot's main clone
// (slots.MergeCheckRef): head (the PR head), merged (a merged commit
// fetched by id) or tree (the merged tree's commit).
func (c *check) ref(name string) string { return slots.MergeCheckRef(c.sl.Name, name) }

// refs are all of them, which every release of the slot deletes.
func (c *check) refs() []string { return slots.MergeCheckRefs(c.sl.Name) }

// run is the check after the slot is held; it fills c.res.
func (c *check) run(ctx context.Context) error {
	main := cmp.Or(c.sl.MainClone, c.o.Pool.MainClone)
	c.progress("fetching origin/%s, #%d and %s", c.o.Pool.Base, c.o.PR, c.o.Merged)
	if err := c.d.Git.FetchBranch(ctx, main, c.o.Pool.Base); err != nil {
		return err
	}
	merged, err := c.mergedCommit(ctx, main)
	if err != nil {
		return err
	}
	c.res.Merged = merged
	head, err := c.d.Git.FetchInto(ctx, main, fmt.Sprintf("refs/pull/%d/head", c.o.PR), c.ref("head"))
	if err != nil {
		return err
	}
	c.res.Head = head
	tree, conflicts, err := c.d.Git.MergeTree(ctx, main, merged, head)
	if err != nil {
		return err
	}
	if len(conflicts) > 0 {
		c.res.Verdict, c.res.Conflicts = VerdictConflict, conflicts
		return nil
	}
	msg := fmt.Sprintf("magnum merge-check: #%d onto %s", c.o.PR, textx.ShortSHA(merged))
	commit, err := c.d.Git.CommitTree(ctx, main, tree, msg, merged, head)
	if err != nil {
		return err
	}
	if err := c.d.Git.UpdateRef(ctx, main, c.ref("tree"), commit); err != nil {
		return err
	}
	c.res.Tree = commit

	mr := &SpecRun{Tree: "merged", SHA: commit}
	c.res.MergedRun = mr
	if err := c.checkout(ctx, mr, "the merged tree"); err != nil {
		return err
	}
	specs, err := c.specs(ctx, merged, head)
	if err != nil {
		return err
	}
	c.res.Specs = specs
	if len(specs) == 0 {
		c.res.Verdict = VerdictNoSpecs
		return nil
	}
	mrep, err := c.rspec(ctx, mr, specs, "the merged tree")
	if err != nil {
		return err
	}
	failures := mrep.failures()
	if len(failures) == 0 {
		c.res.Verdict = VerdictPass
		return nil
	}
	for i := range failures {
		failures[i].AtHead, failures[i].Verdict = AtHeadNotRun, VerdictUnknown
	}
	c.res.Failures, c.res.Verdict = failures, VerdictUnknown

	hr := &SpecRun{Tree: "head", SHA: head}
	c.res.HeadRun = hr
	if err := c.checkout(ctx, hr, "the PR head"); err != nil {
		return err
	}
	present := c.present(specs)
	var again []string
	for _, f := range specs {
		if present[f] && rerun(f, failures) {
			again = append(again, f)
		}
	}
	hrep := &report{}
	if len(again) > 0 {
		c.progress("%s on the merged tree; running %s again at the PR head",
			textx.Count(len(failures), "failure", "failures"), textx.Count(len(again), "spec file", "spec files"))
		if hrep, err = c.rspec(ctx, hr, again, "the PR head"); err != nil {
			if ctx.Err() != nil {
				return err
			}
			c.res.Detail = textx.Clip(execx.Redact(err.Error()), 1000)
			return nil // every failure stays not_run: unknown
		}
		hrep.ran = true
	}
	c.res.Failures = classify(failures, hrep, present)
	c.res.Verdict = verdictOf(c.res.Failures)
	return nil
}

// mergedCommit resolves Options.Merged in main (fetched with the base
// branch), else fetches a full commit id by id.
func (c *check) mergedCommit(ctx context.Context, main string) (string, error) {
	sha, err := c.d.Git.RevParse(ctx, main, c.o.Merged)
	if err == nil || !errors.Is(err, gitx.ErrNoSuchRef) {
		return sha, err
	}
	if !fullOID(c.o.Merged) {
		return "", fmt.Errorf("commit %s is not in %s after fetching origin/%s; give its full id to fetch it by id",
			c.o.Merged, main, c.o.Pool.Base)
	}
	return c.d.Git.FetchInto(ctx, main, c.o.Merged, c.ref("merged"))
}

// fullOID reports whether s is a full commit id (SHA-1 or SHA-256 hex).
func fullOID(s string) bool {
	return (len(s) == 40 || len(s) == 64) && strings.Trim(s, "0123456789abcdef") == ""
}

// checkout puts run's commit into the slot with its checkout steps and its
// readiness (the schema the checkout needs, then the prepare commands and
// ready probes).
func (c *check) checkout(ctx context.Context, run *SpecRun, what string) error {
	c.progress("checking out %s (%s) in %s", what, textx.ShortSHA(run.SHA), c.sl.Name)
	if err := c.d.Slots.CheckoutHeld(ctx, c.sl, c.o.Pool, Reason, run.SHA); err != nil {
		return err
	}
	run.Readiness = c.readiness(ctx)
	return ctx.Err()
}

// readiness gives the checkout the databases its schema needs (EnsureSchema,
// a round's readiness rule) and runs the repository's prepare commands,
// then its ready probes, each as `zsh -lc` in the slot with the slot's env
// within the readiness budget. A failure is recorded, never fatal: the spec
// run shows what it costs.
func (c *check) readiness(ctx context.Context) []Check {
	var out []Check
	switch note, err := c.d.Slots.EnsureSchema(ctx, c.sl, c.o.Pool); {
	case err != nil:
		out = append(out, Check{Kind: "schema", Detail: textx.Clip(execx.Redact(err.Error()), 300)})
	case note != "":
		out = append(out, Check{Kind: "schema", OK: true, Detail: note})
	}
	r := c.o.Readiness
	budget := cmp.Or(r.Timeout, config.DefaultReadyTimeout)
	deadline := c.d.Now().Add(budget)
	type step struct{ kind, cmd string }
	var todo []step
	for _, s := range r.Prepare {
		todo = append(todo, step{"prepare", s})
	}
	for _, s := range r.Ready {
		todo = append(todo, step{"ready", s})
	}
	for _, s := range todo {
		chk := Check{Kind: s.kind, Command: s.cmd}
		left := deadline.Sub(c.d.Now())
		switch {
		case ctx.Err() != nil:
			chk.Detail = "skipped: interrupted"
		case left <= 0:
			chk.Detail = fmt.Sprintf("skipped: the readiness budget (%s) was spent", budget)
		default:
			res, err := c.d.Run.Run(ctx, execx.Cmd{
				Name: Shell, Args: []string{"-lc", s.cmd}, Dir: c.sl.Path, Env: c.o.Pool.SlotEnv(c.sl.Name),
				Unset: gitx.ScrubbedEnv(), Timeout: left, Mutates: s.kind == "prepare", Probe: s.kind == "ready",
				NoTTY: true, Label: "merge-check " + s.kind,
			})
			chk.OK = err == nil
			if err != nil {
				chk.Detail = failureText(res, err)
			}
		}
		out = append(out, chk)
	}
	return out
}

// failureText is a failed command's exit and last output line, redacted.
func failureText(res execx.Result, err error) string {
	var ee *execx.ExitError
	what := "could not run"
	if errors.As(err, &ee) {
		what = fmt.Sprintf("exit %d", ee.Code)
	} else if errors.Is(err, context.DeadlineExceeded) {
		what = "timed out"
	}
	if l := lastLine(res); l != "" {
		return what + ": " + l
	}
	return what
}

// lastLine is the last non-blank output line (stderr first), redacted and
// clipped.
func lastLine(res execx.Result) string {
	for _, s := range [][]byte{res.Stderr, res.Stdout} {
		lines := strings.Split(strings.TrimSpace(string(s)), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if l := strings.TrimSpace(lines[i]); l != "" {
				return textx.Clip(execx.Redact(strings.ToValidUTF8(l, "")), 200)
			}
		}
	}
	return ""
}

// specs are the spec files to run on the merged tree: those the PR changes
// (since its merge base with the merged commit), those the merged commit
// changes (since its first parent), and those of the subject files the
// merged commit changes (SpecsFor), each only when the merged tree has it.
func (c *check) specs(ctx context.Context, merged, head string) ([]string, error) {
	prFiles, err := c.d.Git.ChangedPaths(ctx, c.sl.Path, merged, head)
	if err != nil {
		return nil, err
	}
	mergedFiles, err := c.d.Git.ChangedPaths(ctx, c.sl.Path, merged+"^", merged)
	if err != nil {
		return nil, err
	}
	var cands []string
	for _, f := range prFiles {
		if IsSpec(f) {
			cands = append(cands, f)
		}
	}
	for _, f := range mergedFiles {
		if IsSpec(f) {
			cands = append(cands, f)
		} else {
			cands = append(cands, SpecsFor(f)...)
		}
	}
	slices.Sort(cands)
	cands = slices.Compact(cands)
	present := c.present(cands)
	var out []string
	for _, f := range cands {
		if present[f] {
			out = append(out, f)
		}
	}
	return out, nil
}

// present reports which of files are regular files in the slot's checkout,
// read through an os.Root confined to it (the names come from the PRs).
func (c *check) present(files []string) map[string]bool {
	out := map[string]bool{}
	root, err := os.OpenRoot(c.sl.Path)
	if err != nil {
		return out
	}
	defer root.Close()
	for _, f := range files {
		if fi, err := root.Stat(f); err == nil && fi.Mode().IsRegular() {
			out[f] = true
		}
	}
	return out
}

// IsSpec reports whether path is an RSpec file: spec/…/*_spec.rb.
func IsSpec(path string) bool {
	return strings.HasPrefix(path, "spec/") && strings.HasSuffix(path, "_spec.rb")
}

// SpecsFor are the spec files Rails convention gives a Ruby file:
// app/<dir>/<path>.rb → spec/<dir>/<path>_spec.rb (and, for a controller,
// spec/requests/<path without _controller>_spec.rb), lib/<path>.rb →
// spec/lib/<path>_spec.rb, any other <path>.rb → spec/<path>_spec.rb. None
// for a file that is not Ruby or is a spec already.
func SpecsFor(path string) []string {
	stem, ok := strings.CutSuffix(path, ".rb")
	if !ok || IsSpec(path) || strings.HasPrefix(path, "spec/") {
		return nil
	}
	if rest, ok := strings.CutPrefix(stem, "app/"); ok {
		out := []string{"spec/" + rest + "_spec.rb"}
		if ctrl, ok := strings.CutPrefix(rest, "controllers/"); ok {
			if name, ok := strings.CutSuffix(ctrl, "_controller"); ok {
				out = append(out, "spec/requests/"+name+"_spec.rb")
			}
		}
		return out
	}
	return []string{"spec/" + stem + "_spec.rb"}
}

// rerun reports whether spec file f must run again at the PR head: one of
// its examples failed or it failed to load, or an error outside examples
// leaves every file in doubt.
func rerun(f string, failures []Failure) bool {
	return slices.ContainsFunc(failures, func(x Failure) bool { return x.Kind == KindOutside || x.File == f })
}

// rspec runs files on the slot's current checkout: `<db-lock line>
// <command> --format progress --format json --out <report> <files>` through
// `zsh -lc` in the slot with its env. The report is read whatever the exit
// status (failing examples exit 1); no report is an error.
func (c *check) rspec(ctx context.Context, run *SpecRun, files []string, what string) (*report, error) {
	base := filepath.Join(c.o.Dir, run.Tree)
	run.Files, run.Report, run.Log = files, base+".json", base+".log"
	_ = os.Remove(run.Report)
	quoted := make([]string, len(files))
	for i, f := range files {
		quoted[i] = execx.ShellQuote(f)
	}
	line := agents.DBLockLine(c.o.Magnum, c.sl.Path, Reason) + " " + c.o.Command +
		" --format progress --format json --out " + execx.ShellQuote(run.Report) + " " + strings.Join(quoted, " ")
	c.progress("running %s on %s (log: %s)", textx.Count(len(files), "spec file", "spec files"), what, run.Log)
	res, err := c.d.Run.Run(ctx, execx.Cmd{
		Name: Shell, Args: []string{"-lc", line}, Dir: c.sl.Path, Env: c.o.Pool.SlotEnv(c.sl.Name),
		Unset: gitx.ScrubbedEnv(), Timeout: c.o.Timeout, Mutates: true, NoTTY: true,
		Label: "merge-check spec run (" + run.Tree + ")",
	})
	run.Exit, run.Duration = res.Code, res.Duration.Round(time.Second).String()
	c.writeLog(run.Log, line, res, err)
	if ctx.Err() != nil {
		return nil, fmt.Errorf("the spec run on %s was interrupted: %w", what, ctx.Err())
	}
	rep, rerr := readReport(run.Report)
	if rerr != nil {
		// The exit status only: the output is the PR's code talking, and
		// this error becomes the event's message. The log keeps it.
		why := fmt.Sprintf("exit %d", res.Code)
		if errors.Is(err, context.DeadlineExceeded) {
			why = "timed out after " + c.o.Timeout.String()
		}
		return nil, fmt.Errorf("the spec run on %s left no report (%s); its output is in %s", what, why, run.Log)
	}
	run.Examples, run.Failed, run.Pending = rep.Summary.ExampleCount, rep.Summary.FailureCount, rep.Summary.PendingCount
	run.ErrorsOutside = rep.Summary.ErrorsOutside
	return rep, nil
}

// writeLog keeps a spec run's command, output and exit, redacted (best
// effort).
func (c *check) writeLog(path, line string, res execx.Result, err error) {
	var b strings.Builder
	fmt.Fprintf(&b, "$ %s\n", line)
	b.Write(res.Stdout)
	if len(res.Stderr) > 0 {
		b.WriteString("\n--- stderr ---\n")
		b.Write(res.Stderr)
	}
	fmt.Fprintf(&b, "\n== exit %d (%s)", res.Code, res.Duration.Round(time.Millisecond))
	if err != nil {
		fmt.Fprintf(&b, ": %v", err)
	}
	b.WriteString("\n")
	if werr := os.WriteFile(path, []byte(execx.Redact(b.String())), 0o600); werr != nil {
		c.progress("write %s: %v", path, werr)
	}
}

// report is an RSpec JSON report (the json formatter's).
type report struct {
	Messages []string  `json:"messages"`
	Examples []example `json:"examples"`
	Summary  struct {
		ExampleCount  int `json:"example_count"`
		FailureCount  int `json:"failure_count"`
		PendingCount  int `json:"pending_count"`
		ErrorsOutside int `json:"errors_outside_of_examples_count"`
	} `json:"summary"`
	ran bool // a head report that was read (not the empty stand-in)
}

type example struct {
	ID              string `json:"id"`
	FullDescription string `json:"full_description"`
	Status          string `json:"status"`
	FilePath        string `json:"file_path"`
	LineNumber      int    `json:"line_number"`
	Exception       *struct {
		Class   string `json:"class"`
		Message string `json:"message"`
	} `json:"exception"`
}

// file is the spec file that runs the example: its id's file
// ("./spec/a_spec.rb[1:2]"), which an example of a shared group defined
// elsewhere keeps, else its file_path.
func (e example) file() string {
	if i := strings.Index(e.ID, "["); i > 0 {
		return specFile(e.ID[:i])
	}
	return specFile(e.FilePath)
}

func readReport(path string) (*report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r report
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &r, nil
}

// loadErrorRe finds the spec file RSpec names when one fails to load.
var loadErrorRe = regexp.MustCompile(`An error occurred while loading (\S+?\.rb)\b`)

// specFile is an RSpec path as the PR names it: without "./".
func specFile(p string) string { return strings.TrimPrefix(p, "./") }

// loadErrors are the spec files the report says failed to load.
func (r *report) loadErrors() map[string]string {
	out := map[string]string{}
	for _, m := range r.Messages {
		if sub := loadErrorRe.FindStringSubmatch(m); sub != nil {
			f := specFile(sub[1])
			if _, seen := out[f]; !seen {
				out[f] = errorLine(m, sub[0])
			}
		}
	}
	return out
}

// errorLine is the first line of message after the "while loading" line: the
// failure, clipped.
func errorLine(message, header string) string {
	_, after, _ := strings.Cut(message, header)
	return textx.Clip(textx.FirstLine(strings.TrimPrefix(strings.TrimSpace(after), ".")), 300)
}

// failures are the report's failing examples, files that failed to load and
// one entry for errors outside examples that no file explains.
func (r *report) failures() []Failure {
	var out []Failure
	for _, e := range r.Examples {
		if e.Status != "failed" {
			continue
		}
		f := Failure{Kind: KindExample, File: e.file(), ID: e.ID, Description: e.FullDescription}
		if specFile(e.FilePath) == f.File {
			f.Line = e.LineNumber
		}
		if x := e.Exception; x != nil {
			f.Error = textx.Clip(strings.TrimSpace(x.Class+": "+textx.FirstLine(x.Message)), 300)
		}
		out = append(out, f)
	}
	loads := r.loadErrors()
	for _, file := range slices.Sorted(maps.Keys(loads)) {
		out = append(out, Failure{Kind: KindLoadError, File: file, Description: "the file failed to load", Error: loads[file]})
	}
	if r.Summary.ErrorsOutside > len(loads) {
		out = append(out, Failure{Kind: KindOutside, Description: "an error outside of examples (a hook or the suite's setup)"})
	}
	return out
}

// classify says what each merged-tree failure does at the PR head: head is
// the head's report (ran is false when no file was there to run again),
// present the spec files the head has.
func classify(failures []Failure, head *report, present map[string]bool) []Failure {
	status := map[string]string{} // file NUL description → worst status at the head
	rank := map[string]int{"failed": 3, "passed": 2, "pending": 1}
	for _, e := range head.Examples {
		k := e.file() + "\x00" + e.FullDescription
		if rank[e.Status] > rank[status[k]] {
			status[k] = e.Status
		}
	}
	loads := head.loadErrors()
	outside := head.Summary.ErrorsOutside > len(loads)
	out := slices.Clone(failures)
	for i := range out {
		f := &out[i]
		_, unloadable := loads[f.File]
		switch {
		case f.Kind == KindOutside && !head.ran:
			f.AtHead = AtHeadNotRun
		case f.Kind == KindOutside && outside:
			f.AtHead = AtHeadFailed
		case f.Kind == KindOutside:
			f.AtHead = AtHeadPassed
		case !present[f.File]:
			f.AtHead = AtHeadAbsent
		case !head.ran:
			f.AtHead = AtHeadNotRun
		case unloadable:
			f.AtHead = AtHeadFailed
		case f.Kind == KindLoadError:
			f.AtHead = AtHeadPassed
		default:
			switch status[f.File+"\x00"+f.Description] {
			case "failed":
				f.AtHead = AtHeadFailed
			case "passed":
				f.AtHead = AtHeadPassed
			case "pending":
				f.AtHead = AtHeadPending
			default:
				f.AtHead = AtHeadAbsent
			}
		}
		f.Verdict = verdictFor(f.AtHead)
	}
	return out
}

// verdictFor is a failure's verdict from what it does at the PR head: one
// that does not fail there (it passes, or the head has no such example)
// fails only with the merge.
func verdictFor(atHead string) string {
	switch atHead {
	case AtHeadPassed, AtHeadAbsent:
		return VerdictClash
	case AtHeadFailed:
		return VerdictAlreadyFailing
	}
	return VerdictUnknown
}

// verdictOf is the check's verdict from its failures: a clash when one
// clashes, unknown when one has no answer, else already failing.
func verdictOf(failures []Failure) string {
	has := func(v string) bool {
		return slices.ContainsFunc(failures, func(f Failure) bool { return f.Verdict == v })
	}
	switch {
	case has(VerdictClash):
		return VerdictClash
	case has(VerdictUnknown):
		return VerdictUnknown
	}
	return VerdictAlreadyFailing
}

// Summary is one sentence about r's verdict, with counts only (no PR text).
func Summary(r Result) string {
	count := func(v string) int {
		n := 0
		for _, f := range r.Failures {
			if f.Verdict == v {
				n++
			}
		}
		return n
	}
	files := textx.Count(len(r.Specs), "spec file", "spec files")
	switch r.Verdict {
	case VerdictConflict:
		return "the merge conflicts in " + textx.Count(len(r.Conflicts), "file", "files")
	case VerdictNoSpecs:
		return "neither side changes a spec file or a file with one; nothing ran"
	case VerdictPass:
		n := 0
		if r.MergedRun != nil {
			n = r.MergedRun.Examples
		}
		return fmt.Sprintf("%s of %s pass on the merged tree", textx.Count(n, "example", "examples"), files)
	case VerdictClash, VerdictAlreadyFailing, VerdictUnknown:
		s := fmt.Sprintf("%s on the merged tree (%s): %d clash, %d already failing at the PR head",
			textx.Count(len(r.Failures), "failure", "failures"), files, count(VerdictClash), count(VerdictAlreadyFailing))
		if n := count(VerdictUnknown); n > 0 {
			s += fmt.Sprintf(", %d without an answer from the PR head", n)
		}
		return s
	case VerdictError:
		return "the check did not finish: " + r.Detail
	}
	return r.Verdict
}

// release hands the slot back (ReleaseHeld) with a context an interrupt
// does not cancel, unless Keep.
func (c *check) release(ctx context.Context) error {
	if c.o.Keep {
		c.res.Kept = true
		c.progress("keeping %s held for %s; `magnum slots unpin %s` hands it back", c.sl.Name, Reason, c.sl.Name)
		return nil
	}
	c.progress("releasing %s", c.sl.Name)
	err := c.d.Slots.ReleaseHeld(context.WithoutCancel(ctx), c.sl, c.o.Pool, Reason, c.refs()...)
	if err != nil {
		c.res.ReleaseError = textx.Clip(execx.Redact(err.Error()), 1000)
		return fmt.Errorf("merge-check: release %s: %w", c.sl.Name, err)
	}
	c.res.Released = true
	return nil
}

// record writes ResultFile and the merge_check.result event under the PR's
// subject (counts and commits only; the descriptions stay in the file).
func (c *check) record(ctx context.Context) error {
	var errs []error
	file := filepath.Join(c.o.Dir, ResultFile)
	b, err := json.MarshalIndent(c.res, "", "  ")
	if err == nil {
		err = fsx.WriteFileAtomic(file, append(b, '\n'), 0o600)
	}
	if err != nil {
		errs = append(errs, fmt.Errorf("merge-check: write %s: %w", file, err))
	} else {
		c.res.File = file
	}
	if c.d.Store == nil {
		return errors.Join(errs...)
	}
	level := "info"
	switch c.res.Verdict {
	case VerdictClash, VerdictConflict, VerdictUnknown:
		level = "warn"
	case VerdictError:
		level = "error"
	}
	label := textx.ShortSHA(c.res.Merged)
	if c.o.MergedPR > 0 {
		label += fmt.Sprintf(" (#%d)", c.o.MergedPR)
	}
	msg := fmt.Sprintf("merge-check of #%d with %s in %s: %s: %s", c.o.PR, label, c.sl.Name, c.res.Verdict, Summary(c.res))
	data, _ := json.Marshal(map[string]any{
		"verdict": c.res.Verdict, "head": c.res.Head, "merged": c.res.Merged, "merged_pr": c.o.MergedPR,
		"tree": c.res.Tree, "slot": c.sl.Name, "specs": len(c.res.Specs), "failures": len(c.res.Failures),
		"conflicts": len(c.res.Conflicts), "released": c.res.Released, "file": c.res.File,
	})
	subject := fmt.Sprintf("pr:%s#%d", c.o.Pool.Repo, c.o.PR)
	if _, err := c.d.Store.AppendEvent(ctx, store.Event{Level: level, Subject: &subject, Kind: EventKind,
		Message: textx.Clip(execx.Redact(msg), 2000), Data: data}); err != nil {
		errs = append(errs, fmt.Errorf("merge-check: record the event: %w", err))
	}
	return errors.Join(errs...)
}
