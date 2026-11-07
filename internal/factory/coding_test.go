package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/gitx"
)

// codingRig is the coding stage over real git in temp directories: a bare
// origin with master, a work clone that moves master, the repository's
// main clone and the task's copy (a worktree of the main clone, as a claim
// leaves it), and the harness's engine with that copy as its claimed one.
// The repository's commands (the precheck's and the proof's scripts, run
// as `mise exec … /bin/sh -c <script>`) are fakes: bin/specs runs the spec
// files it is given (a spec file holds `expect: <text> in <path>` lines,
// each passing when the copy's file at path holds text), bin/lint prints
// what lint says, bin/jest passes, and bin/rails records the migrations it
// runs. No real process but git, and no git outside t.TempDir.
type codingRig struct {
	*harness
	root, origin, work, main, copyDir string
	real                              *execx.Real
	cp                                Copy

	mu      sync.Mutex
	scripts []scriptRun
	// lint is what bin/lint prints for its files (exit 1 when it prints
	// anything); migrate runs a bin/rails migration script in the copy at
	// dir and gives its exit (it may write the copy's files, as Rails dumps
	// db/schema.rb); hang makes a script never end on its own (it ends with
	// its command's timeout or its context, as execx ends one); failGit
	// fails the git commands it names (an error, not nil); seenGit sees
	// every git command as the factory built it (its env and its unset).
	lint func(files []string) string
	// lintCrash, when it returns a stderr, makes bin/lint print nothing on
	// stdout, that on stderr, and exit 2, as a lint that crashed does;
	// bundleFails, when set, is the stderr of a `bundle install` that exits 1.
	bundleFails string
	lintCrash   func(files []string) string
	migrate     func(dir, script string) int
	hang        func(run scriptRun) bool
	failGit     func(args []string) error
	seenGit     func(c execx.Cmd)
	// during runs inside each script, before it does its work (a person
	// entering the copy's pane mid-run, a spec writing a tracked file);
	// startErr is the error of a script that did not start (nil: it starts).
	during   func(dir string, run scriptRun)
	startErr func(run scriptRun) error
	// blocks hold a script whose first word they name until the channel
	// closes or its run's context ends; entered gets that word as one
	// starts to wait.
	blocks  map[string]chan struct{}
	entered chan string
}

// scriptRun is one script the copy ran: the script, the copy's branch and
// HEAD as it ran ("" for a detached HEAD), the content of the spec files it
// ran as it saw them, its exit and its output.
type scriptRun struct {
	Script       string
	Branch, Head string
	Specs        map[string]string
	Exit         int
	Out          string
}

const (
	creditRB   = "class Credit\n  def points\n    0\n  end\nend\n"
	creditSpec = "expect: class Credit in app/credit.rb\n"
)

func newCodingRig(t *testing.T) *codingRig {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &codingRig{root: root, blocks: map[string]chan struct{}{}, entered: make(chan string, 8),
		real: &execx.Real{BaseEnv: []string{
			"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
			"LC_ALL=C",
		}}}
	r.origin, r.work = filepath.Join(root, "origin.git"), filepath.Join(root, "work")
	r.main, r.copyDir = filepath.Join(root, "shop"), filepath.Join(root, "shop.factory1")
	r.harness = newHarness(t, func(d *Deps) {
		run := rigRunner{r}
		d.Run = run
		d.Git = gitx.New(Guard(run, d.Config))
	})
	r.e.lockPoll = 5 * time.Millisecond
	r.cfg.Factory.Repos = []config.FactoryRepo{{Repo: "example/shop", Base: "master", MainClone: r.main,
		Copies: filepath.Join(root, "shop.factory{n}"), CopyName: "factory{n}", Consoles: []string{"bin/remote-console"},
		GhostMarker: "GhostCommandGenerator",
		Precheck: config.FactoryPrecheck{Lint: []string{"bin/lint {files}"}, Specs: "bin/specs {files}", Jest: "bin/jest {files}",
			Migrations: []string{"bin/rails db:migrate:up VERSION={version}", "bin/rails db:migrate:down VERSION={version}"},
			Cap:        config.Duration{Duration: 10 * time.Minute}, SkipSpecsForKinds: []string{"chore"}}}}

	for _, d := range []string{r.origin, r.work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.git(r.origin, "init", "--quiet", "--bare", "--template=", "-b", "master")
	r.git(r.work, "init", "--quiet", "--template=", "-b", "master")
	r.write(r.work, ".gitignore", "tmp/\n")
	r.write(r.work, "README", "shop\n")
	r.write(r.work, "app/credit.rb", creditRB)
	r.write(r.work, "spec/models/credit_spec.rb", creditSpec)
	r.git(r.work, "add", "-A")
	r.git(r.work, "commit", "--quiet", "-m", "init")
	r.git(r.work, "remote", "add", "origin", r.origin)
	r.git(r.work, "push", "--quiet", "origin", "master")
	r.git(root, "clone", "--quiet", r.origin, r.main)
	r.git(r.main, "worktree", "add", "--quiet", "-b", "factory1", r.copyDir, "origin/master")
	r.cp = Copy{Name: "factory1", Path: r.copyDir, Number: 1, Port: 3001, Env: map[string]string{"PORT": "3001"}}
	r.copies.claimed = r.cp
	return r
}

// rigRunner sends git to the real runner and the copy's scripts to the
// rig's fakes.
type rigRunner struct{ r *codingRig }

func (x rigRunner) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	switch c.Name {
	case "git":
		x.r.mu.Lock()
		fail, seen := x.r.failGit, x.r.seenGit
		x.r.mu.Unlock()
		if seen != nil {
			seen(c)
		}
		if fail != nil {
			if err := fail(c.Args); err != nil {
				return execx.Result{Code: 128}, &execx.ExitError{Cmd: c, Code: 128, Stderr: err.Error()}
			}
		}
		return x.r.real.Run(ctx, c)
	case "mise":
		if c.Timeout > 0 { // as execx bounds a command
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, c.Timeout)
			defer cancel()
		}
		return x.r.script(ctx, c)
	}
	return execx.Result{Code: -1}, fmt.Errorf("the coding rig runs no %s", c.Name)
}

// script is a fake of the copy's command: `mise -C <copy> exec -- env …
// /bin/sh -c <script>`.
func (r *codingRig) script(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	script := c.Args[len(c.Args)-1]
	words := strings.Fields(script)
	for i, w := range words {
		words[i] = strings.Trim(w, "'")
	}
	run := scriptRun{Script: script, Specs: map[string]string{}}
	run.Branch = r.gitOut(c.Dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	run.Head = r.gitOut(c.Dir, "rev-parse", "HEAD")
	r.mu.Lock()
	block, hang, during, startErr := r.blocks[words[0]], r.hang, r.during, r.startErr
	r.mu.Unlock()
	if startErr != nil {
		if err := startErr(run); err != nil {
			return execx.Result{Code: -1}, &execx.RunError{Cmd: c, Err: err}
		}
	}
	if during != nil {
		during(c.Dir, run)
	}
	if hang != nil && hang(run) {
		<-ctx.Done()
		r.record(run)
		return execx.Result{Code: -1}, &execx.RunError{Cmd: c, Err: ctx.Err()}
	}
	if block != nil {
		r.entered <- words[0]
		select {
		case <-block:
		case <-ctx.Done():
			r.record(run)
			return execx.Result{Code: -1}, &execx.RunError{Cmd: c, Err: ctx.Err()}
		}
	}
	switch words[0] {
	case "bin/specs":
		run.Exit, run.Out = r.specs(c.Dir, words[1:], run.Specs)
	case "bin/lint":
		r.mu.Lock()
		lint, crash := r.lint, r.lintCrash
		r.mu.Unlock()
		if crash != nil {
			if stderr := crash(words[1:]); stderr != "" {
				r.record(run)
				return execx.Result{Code: 2, Stderr: []byte(stderr)}, &execx.ExitError{Cmd: c, Code: 2, Stderr: stderr}
			}
		}
		if lint != nil {
			if run.Out = lint(words[1:]); run.Out != "" {
				run.Exit = 1
			}
		}
	case "bundle": // `bundle install` (a heal) installs nothing, or fails with bundleFails
		r.mu.Lock()
		fails := r.bundleFails
		r.mu.Unlock()
		if fails != "" {
			r.record(run)
			return execx.Result{Code: 1, Stderr: []byte(fails)}, &execx.ExitError{Cmd: c, Code: 1, Stderr: fails}
		}
	case "bin/jest":
	case "bin/rails":
		r.mu.Lock()
		migrate := r.migrate
		r.mu.Unlock()
		if migrate != nil {
			run.Exit = migrate(c.Dir, script)
		}
	default: // as a shell answers a command it cannot find
		r.record(run)
		return execx.Result{Code: 127, Stderr: []byte("sh: " + words[0] + ": not found")},
			&execx.ExitError{Cmd: c, Code: 127, Stderr: "sh: " + words[0] + ": not found"}
	}
	r.record(run)
	res := execx.Result{Code: run.Exit, Stdout: []byte(run.Out)}
	if run.Exit != 0 {
		return res, &execx.ExitError{Cmd: c, Code: run.Exit}
	}
	return res, nil
}

func (r *codingRig) record(run scriptRun) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scripts = append(r.scripts, run)
}

// specs runs the spec files among files (and, for app/<x>.rb,
// spec/models/<x>_spec.rb when there is one), as bin/specs does: each
// `expect: <text> in <path>` line passes when the copy's path holds text.
// A failing line prints `rspec ./<spec>:<line> # <text>`, as rspec's list of
// failures does.
func (r *codingRig) specs(dir string, files []string, seen map[string]string) (int, string) {
	var specs []string
	for _, f := range files {
		switch {
		case strings.HasSuffix(f, "_spec.rb"):
			specs = append(specs, f)
		case strings.HasPrefix(f, "app/") && strings.HasSuffix(f, ".rb"):
			s := "spec/models/" + strings.TrimSuffix(filepath.Base(f), ".rb") + "_spec.rb"
			if _, err := os.Stat(filepath.Join(dir, s)); err == nil {
				specs = append(specs, s)
			}
		}
	}
	var out []string
	exit := 0
	for _, s := range slices.Compact(slices.Sorted(slices.Values(specs))) {
		b, err := os.ReadFile(filepath.Join(dir, s))
		if err != nil {
			exit = 1
			out = append(out, "An error occurred while loading ./"+s)
			continue
		}
		seen[s] = string(b)
		for n, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			text, path, ok := strings.Cut(strings.TrimPrefix(line, "expect: "), " in ")
			if !ok {
				continue
			}
			got, _ := os.ReadFile(filepath.Join(dir, path))
			if !strings.Contains(string(got), text) {
				exit = 1
				out = append(out, fmt.Sprintf("rspec ./%s:%d # %s expects %s", s, n+1, path, text))
			}
		}
	}
	return exit, strings.Join(out, "\n")
}

// ran are the scripts the copy ran whose first word is word.
func (r *codingRig) ran(word string) []scriptRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []scriptRun
	for _, s := range r.scripts {
		if strings.HasPrefix(s.Script, word+" ") || s.Script == word {
			out = append(out, s)
		}
	}
	return out
}

func (r *codingRig) git(dir string, args ...string) string {
	r.t.Helper()
	res, err := r.real.Run(context.Background(), execx.Cmd{Name: "git", Args: append([]string{"-C", dir}, args...)})
	if err != nil {
		r.t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return res.Out()
}

// gitOut is git's answer in dir, "" when it fails.
func (r *codingRig) gitOut(dir string, args ...string) string {
	res, err := r.real.Run(context.Background(), execx.Cmd{Name: "git", Args: append([]string{"-C", dir}, args...)})
	if err != nil {
		return ""
	}
	return res.Out()
}

func (r *codingRig) write(dir, rel, text string) {
	r.t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *codingRig) read(rel string) string {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.copyDir, rel))
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

// commit commits files (path -> text) in the copy on its branch.
func (r *codingRig) commit(msg string, files map[string]string) string {
	r.t.Helper()
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		r.write(r.copyDir, rel, files[rel])
		r.git(r.copyDir, "add", "--", rel)
	}
	r.git(r.copyDir, "commit", "--quiet", "-m", msg)
	return r.git(r.copyDir, "rev-parse", "HEAD")
}

// moveMaster commits on master in the work clone and pushes it.
func (r *codingRig) moveMaster(rel, text string) string {
	r.t.Helper()
	r.write(r.work, rel, text)
	r.git(r.work, "add", "--", rel)
	r.git(r.work, "commit", "--quiet", "-m", "master moves")
	r.git(r.work, "push", "--quiet", "origin", "master")
	return r.git(r.work, "rev-parse", "HEAD")
}

// codingTask creates an active task of kind at the coding stage with its
// branch created in the copy at origin/master, as its first claim does.
func (r *codingRig) codingTask(key, kind string) registry.Task {
	r.t.Helper()
	t := r.kindTask(key, kind, "coding")
	r.git(r.copyDir, "switch", "--quiet", "--no-track", "-c", t.Branch, "origin/master")
	return t
}

// fix is the writer's work: a fix of Credit#points, its spec, committed,
// and proof.json naming the spec.
func (r *codingRig) fix(t registry.Task) string {
	r.t.Helper()
	head := r.commit(t.JiraKey+" Credit#points counts the activities", map[string]string{
		"app/credit.rb":                     strings.Replace(creditRB, "    0\n", "    10\n", 1),
		"spec/models/credit_points_spec.rb": "expect: 10 in app/credit.rb\n",
	})
	r.taskFile(t.ID, proofFile, `{"specs": ["spec/models/credit_points_spec.rb"]}`)
	return head
}

// restart is the harness's restart with the rig's seams on the new engine.
func (r *codingRig) restart() {
	r.t.Helper()
	r.harness.restart()
	r.e.lockPoll = 5 * time.Millisecond
}

func (r *codingRig) events(id int64, kind string) []registry.Event {
	r.t.Helper()
	evs, err := r.reg.Events(r.ctx, id, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	return slices.DeleteFunc(evs, func(e registry.Event) bool { return e.Kind != kind })
}

func (r *codingRig) artifacts(id int64, kind string) []registry.Artifact {
	r.t.Helper()
	as, err := r.reg.Artifacts(r.ctx, id)
	if err != nil {
		r.t.Fatal(err)
	}
	return slices.DeleteFunc(as, func(a registry.Artifact) bool { return a.Kind != kind })
}

// The coding stage starts the writer on a fresh branch from the base: the
// branch the first claim made (no commit of its own) moves to origin/master
// as a fetch finds it, the reproducer's spec left in the copy comes along,
// and the base is recorded (base.json, the base_sha artifact) before the
// writer gets its prompt. An earlier attempt's branch keeps its commits.
func TestCodingStartsOnAFreshBranchFromTheBase(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12350", "bug")
	r.write(r.copyDir, "spec/models/credit_points_spec.rb", "expect: 10 in app/credit.rb\n") // the reproducer's, uncommitted
	fresh := r.moveMaster("app/notes.rb", "class Note; end\n")

	r.startStage(task)
	if got := r.git(r.copyDir, "rev-parse", task.Branch); got != fresh {
		t.Fatalf("the branch is at %s, want the fresh origin/master %s", got, fresh)
	}
	if b := r.git(r.copyDir, "symbolic-ref", "--short", "HEAD"); b != task.Branch {
		t.Fatalf("the copy is on %q, want %s", b, task.Branch)
	}
	if got := r.read("spec/models/credit_points_spec.rb"); !strings.Contains(got, "expect: 10") {
		t.Fatalf("the reproducer's spec went: %q", got)
	}
	var base baseRecord
	if err := json.Unmarshal([]byte(r.readTask(task.ID, baseFile)), &base); err != nil || base.Base != fresh || base.Branch != task.Branch {
		t.Fatalf("base.json = %+v, %v; want %s on %s", base, err, fresh, task.Branch)
	}
	if as := r.artifacts(task.ID, "base_sha"); len(as) != 1 || as[0].Path != baseFile {
		t.Fatalf("base_sha artifacts %+v, want base.json once", as)
	}
	ps := r.rt.promptsTo("fx-writer")
	if len(ps) != 1 {
		t.Fatalf("the writer got %d prompts, want its first", len(ps))
	}

	// A second attempt of coding (sent back from the ready card) keeps the
	// branch the first one committed on, whatever master did since.
	head := r.fix(task)
	r.moveMaster("app/other.rb", "class Other; end\n")
	if err := r.reg.TransitionAttempt(r.ctx, ps[0].Attempt, nil, registry.AttemptFailed, nil); err != nil {
		t.Fatal(err)
	}
	r.e.agentRuns = map[int64]*agentsRun{}
	r.tick()
	if got := r.git(r.copyDir, "rev-parse", task.Branch); got != head {
		t.Fatalf("a second attempt moved the branch to %s, want the first attempt's %s", got, head)
	}
	if as := r.attempts(task.ID); len(as) != 2 || len(r.rt.promptsTo("fx-writer")) != 2 {
		t.Fatalf("attempts %+v, %d prompts; want a second attempt with its prompt", as, len(r.rt.promptsTo("fx-writer")))
	}
}

// A branch of the task's name that holds commits the task did not make
// (another task's, yours) is never taken over: the task works on the name
// with the next free number, from origin/master, and says why.
func TestABranchOfAnotherTaskGetsASuffix(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12351", "feature")
	theirs := r.commit("someone else's work", map[string]string{"app/theirs.rb": "class Theirs; end\n"})

	r.startStage(task)
	got := r.reload(task.ID)
	if got.Branch != task.Branch+"-2" {
		t.Fatalf("the task's branch is %q, want %s-2", got.Branch, task.Branch)
	}
	if b := r.git(r.copyDir, "symbolic-ref", "--short", "HEAD"); b != got.Branch {
		t.Fatalf("the copy is on %q, want %s", b, got.Branch)
	}
	if head := r.git(r.copyDir, "rev-parse", "HEAD"); head != r.git(r.main, "rev-parse", "origin/master") {
		t.Fatalf("the new branch is at %s, want origin/master", head)
	}
	if kept := r.git(r.copyDir, "rev-parse", task.Branch); kept != theirs {
		t.Fatalf("the other branch moved to %s, want %s", kept, theirs)
	}
	evs := r.events(task.ID, "branch")
	if !slices.ContainsFunc(evs, func(e registry.Event) bool {
		return strings.Contains(e.Text, task.Branch+"-2") && strings.Contains(e.Text, "not this task's")
	}) {
		t.Fatalf("branch events %+v, want the suffix and why", evs)
	}
	if ps := r.rt.promptsTo("fx-writer"); len(ps) != 1 || !strings.Contains(ps[0].Text, got.Branch) {
		t.Fatalf("the writer's prompt does not name %s: %+v", got.Branch, ps)
	}
}

// The coding stage passes on its end fact, never on the writer's "done":
// commits on the branch after origin/master, a clean tree, the factory's own
// proof at the head and the precheck, both run off the loop while the task
// waits with what they do; a writer that committed nothing gets a fix turn.
func TestTheCodingStagePassesOnTheFactorysProofAndPrecheck(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12352", "bug")
	r.startStage(task)
	r.taskFile(task.ID, proofFile, `{"specs": ["spec/models/credit_points_spec.rb"]}`)
	r.ended(task, "fx-writer")
	r.tick()
	ps := r.rt.promptsTo("fx-writer")
	if len(ps) != 2 || !strings.HasPrefix(ps[1].Text, fixLead) || !strings.Contains(ps[1].Text, "no commit after origin/master") {
		t.Fatalf("the writer's prompts %+v, want a fix turn for its missing commit", ps)
	}

	r.clock.Add(time.Minute)
	head := r.fix(task)
	block := make(chan struct{})
	r.blocks["bin/lint"] = block
	r.ended(task, "fx-writer")
	r.tick()
	<-r.entered
	r.tick()
	if got := r.reload(task.ID); got.Stage != "coding" || got.WaitsWho != waitPrecheck || !strings.HasPrefix(got.WaitsWhat, "precheck: lint") {
		t.Fatalf("task %s at %s waits %q: %q; want coding, waiting on its precheck's lint", got.State, got.Stage, got.WaitsWho, got.WaitsWhat)
	}
	close(block)
	r.e.waitBackground()
	r.tick()

	got := r.reload(task.ID)
	if got.Stage != "local_review" || got.HeadSHA != head || got.WaitsWho != "" {
		t.Fatalf("task %s at %s, head %s, waits %q: %q; want local_review at %s", got.State, got.Stage, got.HeadSHA, got.WaitsWho, got.WaitsWhat, head)
	}
	if as := r.artifacts(task.ID, "proof"); len(as) != 1 || as[0].Path != factoryProofFile {
		t.Fatalf("proof artifacts %+v, want %s", as, factoryProofFile)
	}
	checks, err := r.reg.Checks(r.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, c := range checks {
		kinds = append(kinds, c.Kind+":"+c.Result)
	}
	if !slices.Contains(kinds, "spec:fail") || !slices.Contains(kinds, "spec:pass") || !slices.Contains(kinds, "lint:pass") {
		t.Fatalf("checks %q, want the proof's red and green runs and the precheck's", kinds)
	}
	if vs := r.rt.verifies(); len(vs) == 0 || !vs[len(vs)-1].OK {
		t.Fatalf("verifies %+v, want the writer's run verified", vs)
	}
	if b := r.git(r.copyDir, "symbolic-ref", "--short", "HEAD"); b != task.Branch {
		t.Fatalf("the copy is on %q after the checks, want %s", b, task.Branch)
	}
}

// The writer's prompt: the report, your answers and the acceptance checks
// by their paths, a failing spec first for a bug, the spec files named in
// proof.json and never their results, a commit that starts with the Jira
// key, and no push.
func TestTheWritersPromptNamesItsInputsAndTheProofFile(t *testing.T) {
	cfg := config.Defaults()
	d := fullPromptData()
	text := renderFactory(t, cfg, "factory-writer.md", d)
	for _, want := range []string{d.Report, d.Answers, d.Acceptance, d.Proof, "fails", "never the results", "starts with " + d.Key,
		"never push", d.Branch, "origin/" + d.Base} {
		if !strings.Contains(text, want) {
			t.Errorf("the writer's prompt lacks %q:\n%s", want, text)
		}
	}
	d.Key, d.Kind = "", "feature"
	if text := renderFactory(t, cfg, "factory-writer.md", d); strings.Contains(text, "starts with") {
		t.Errorf("a task without a Jira key asks for one in its commits:\n%s", text)
	}
}

// The writer's prompt follows the kind's proof, as the proof does
// (PromptData.RedRun), never the kind's name: a kind of another name whose
// proof is green keeps its specs green and is not told they must fail on
// the base; a kind whose proof is red is told so whatever its name.
func TestTheWritersPromptFollowsTheKindsProof(t *testing.T) {
	cfg := config.Defaults()
	d := fullPromptData()
	d.Kind, d.RedRun = "docs", false
	green := renderFactory(t, cfg, "factory-writer.md", d)
	if !strings.Contains(green, "The existing specs stay green") || !strings.Contains(green, "need not: its specs pass at the head") {
		t.Errorf("a green proof's prompt:\n%s\nwant the specs kept green and no red run asked", green)
	}
	d.Kind, d.RedRun = "chore", true
	red := renderFactory(t, cfg, "factory-writer.md", d)
	if strings.Contains(red, "need not: its specs pass at the head") || !strings.Contains(red, "where they must fail") {
		t.Errorf("a red proof's prompt:\n%s\nwant the red run on the base asked", red)
	}
}

// readTask is the text of name in the task's directory.
func (h *harness) readTask(id int64, name string) string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.layout.FactoryTaskDir(id), name))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}
