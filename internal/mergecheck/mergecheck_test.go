package mergecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
)

var (
	shaMerged = strings.Repeat("a", 40)
	shaHead   = strings.Repeat("b", 40)
	shaTree   = strings.Repeat("c", 40) // the merged tree (merge-tree's answer)
	shaCommit = strings.Repeat("d", 40) // its commit (commit-tree's answer)
	t0        = time.Date(2026, 10, 7, 15, 0, 0, 0, time.UTC)
)

// fakeSlots stands in for *slots.Manager: one slot, checkouts that put
// files into its directory, and a record of the release.
type fakeSlots struct {
	mu      sync.Mutex
	slot    store.Slot
	holdErr error
	trees   map[string][]string // sha → the files its checkout has
	current string
	checked []string
	// release
	releases   int
	releaseCtx error // ctx.Err() when ReleaseHeld ran
	refs       []string
}

func (f *fakeSlots) HoldFree(ctx context.Context, pool config.Pool, name, hold string) (store.Slot, error) {
	if f.holdErr != nil {
		return store.Slot{}, f.holdErr
	}
	if hold != Reason {
		return store.Slot{}, fmt.Errorf("hold %q", hold)
	}
	return f.slot, nil
}

func (f *fakeSlots) CheckoutHeld(ctx context.Context, slot store.Slot, pool config.Pool, hold, sha string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.RemoveAll(filepath.Join(slot.Path, "spec")); err != nil {
		return err
	}
	for _, file := range f.trees[sha] {
		p := filepath.Join(slot.Path, file)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte("# spec\n"), 0o644); err != nil {
			return err
		}
	}
	f.current = sha
	f.checked = append(f.checked, sha)
	return nil
}

func (f *fakeSlots) EnsureSchema(ctx context.Context, slot store.Slot, pool config.Pool) (string, error) {
	return "", nil
}

func (f *fakeSlots) ReleaseHeld(ctx context.Context, slot store.Slot, pool config.Pool, hold string, refs ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases++
	f.releaseCtx = ctx.Err()
	f.refs = refs
	return nil
}

func (f *fakeSlots) at() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current
}

// world is a check's fakes: git answers from maps, the shell writes the
// RSpec report of the tree checked out.
type world struct {
	t     *testing.T
	ctx   context.Context
	st    *store.Store
	slots *fakeSlots
	fake  *execx.Fake
	dir   string
	pool  config.Pool

	mu          sync.Mutex
	refs        map[string]string // local refs (and fetched ones)
	remote      map[string]string // what fetching a source gives
	conflicts   []string          // merge-tree's conflicted paths
	prFiles     []string          // merged...head
	mergedFiles []string          // merged^...merged
	reports     map[string]string // checked-out sha → RSpec JSON (missing: no report)
	exit        map[string]int    // checked-out sha → the spec run's exit (default 1 with failures)
	runErr      func(c execx.Cmd) error
	specLines   map[string][]string // checked-out sha → spec run command lines
	readiness   []string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(root, "magnum.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	slotDir := filepath.Join(root, "talkable.review2")
	if err := os.MkdirAll(slotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	w := &world{
		t: t, ctx: context.Background(), st: st, dir: filepath.Join(root, "state", "merge-check", "run"),
		pool: config.Pool{Repo: "talkable/talkable", MainClone: "/main", Base: "master", SlotName: "review{n}",
			Env: map[string]string{"WT_BRANCH": "{slot}"}},
		refs:      map[string]string{"origin/master": shaMerged},
		remote:    map[string]string{"refs/pull/11979/head": shaHead},
		reports:   map[string]string{},
		exit:      map[string]int{},
		specLines: map[string][]string{},
	}
	w.slots = &fakeSlots{
		slot: store.Slot{ID: 2, Name: "review2", Path: slotDir, MainClone: "/main", Kind: store.SlotKindPool, State: store.SlotHeld,
			HoldReason: store.Ptr(Reason)},
		trees: map[string][]string{},
	}
	w.fake = &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"git"}, Fn: w.git}, {Prefix: []string{Shell}, Fn: w.shell}}}
	return w
}

func (w *world) deps() Deps {
	return Deps{Store: w.st, Git: gitx.New(w.fake), Run: w.fake, Slots: w.slots, Now: func() time.Time { return t0 }}
}

func (w *world) options() Options {
	return Options{Pool: w.pool, Readiness: config.Readiness{Prepare: []string{"bin/rails db:test:prepare"}},
		PR: 11979, Merged: shaMerged[:7], MergedPR: 11939, Dir: w.dir, Magnum: "/opt/magnum"}
}

func (w *world) run(ctx context.Context, o Options) (Result, error) {
	w.t.Helper()
	return Run(ctx, w.deps(), o)
}

func exitErr(c execx.Cmd, code int, stdout string) (execx.Result, error) {
	return execx.Result{Stdout: []byte(stdout), Code: code}, &execx.ExitError{Cmd: c, Code: code}
}

// git answers the commands gitx sends, after `-C <dir>`.
func (w *world) git(c execx.Cmd) (execx.Result, error) {
	a := c.Args
	if len(a) >= 2 && a[0] == "-C" {
		a = a[2:]
	}
	for len(a) >= 2 && a[0] == "-c" {
		a = a[2:]
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	switch a[0] {
	case "fetch":
		spec := a[len(a)-1]
		src, dst, _ := strings.Cut(strings.TrimPrefix(spec, "+"), ":")
		if src == "refs/heads/master" {
			return execx.Result{}, nil
		}
		sha, ok := w.remote[src]
		if !ok {
			return exitErr(c, 128, "")
		}
		w.refs[dst] = sha
		return execx.Result{}, nil
	case "rev-parse":
		ref := strings.TrimSuffix(a[len(a)-1], "^{commit}")
		if sha, ok := w.refs[ref]; ok {
			return execx.Result{Stdout: []byte(sha + "\n")}, nil
		}
		for _, sha := range w.refs {
			if len(ref) >= 7 && strings.HasPrefix(sha, ref) {
				return execx.Result{Stdout: []byte(sha + "\n")}, nil
			}
		}
		return exitErr(c, 1, "")
	case "merge-tree":
		if len(w.conflicts) > 0 {
			return exitErr(c, 1, shaTree+"\x00"+strings.Join(w.conflicts, "\x00")+"\x00")
		}
		return execx.Result{Stdout: []byte(shaTree + "\x00")}, nil
	case "commit-tree":
		return execx.Result{Stdout: []byte(shaCommit + "\n")}, nil
	case "update-ref":
		w.refs[a[len(a)-2]] = a[len(a)-1]
		return execx.Result{}, nil
	case "diff":
		var files []string
		switch rng := a[len(a)-2]; rng {
		case shaMerged + "..." + shaHead:
			files = w.prFiles
		case shaMerged + "^..." + shaMerged:
			files = w.mergedFiles
		default:
			return exitErr(c, 128, "")
		}
		return execx.Result{Stdout: []byte(strings.Join(files, "\x00"))}, nil
	}
	return execx.Result{}, fmt.Errorf("fake git: unexpected %v", c.Args)
}

var outRe = regexp.MustCompile(`--out '?([^' ]+)'?`)

// shell runs `zsh -lc <line>`: a readiness command, or a spec run that
// writes the checked-out tree's report.
func (w *world) shell(c execx.Cmd) (execx.Result, error) {
	line := c.Args[1]
	m := outRe.FindStringSubmatch(line)
	if m == nil {
		w.mu.Lock()
		w.readiness = append(w.readiness, line)
		w.mu.Unlock()
		return execx.Result{}, nil
	}
	sha := w.slots.at()
	w.mu.Lock()
	w.specLines[sha] = append(w.specLines[sha], line)
	rep, ok := w.reports[sha]
	code, set := w.exit[sha]
	runErr := w.runErr
	w.mu.Unlock()
	if runErr != nil {
		if err := runErr(c); err != nil {
			return execx.Result{Code: -1}, err
		}
	}
	if ok {
		if err := os.WriteFile(m[1], []byte(rep), 0o600); err != nil {
			return execx.Result{}, err
		}
	}
	if !set && ok && strings.Contains(rep, `"failed"`) {
		code = 1
	}
	if code != 0 {
		return exitErr(c, code, "Finished in 1.2 seconds\n")
	}
	return execx.Result{Stdout: []byte("Finished in 1.2 seconds\n")}, nil
}

type ex struct{ file, desc, status string }

// rspecJSON is a report with examples (id "./<file>[1:n]").
func rspecJSON(examples ...ex) string {
	type exception struct {
		Class   string `json:"class"`
		Message string `json:"message"`
	}
	type example struct {
		ID              string     `json:"id"`
		FullDescription string     `json:"full_description"`
		Status          string     `json:"status"`
		FilePath        string     `json:"file_path"`
		LineNumber      int        `json:"line_number"`
		Exception       *exception `json:"exception,omitempty"`
	}
	var out struct {
		Examples []example `json:"examples"`
		Summary  struct {
			ExampleCount int `json:"example_count"`
			FailureCount int `json:"failure_count"`
		} `json:"summary"`
	}
	for i, e := range examples {
		x := example{ID: fmt.Sprintf("./%s[1:%d]", e.file, i+1), FullDescription: e.desc, Status: e.status,
			FilePath: "./" + e.file, LineNumber: 10 + i}
		if e.status == "failed" {
			x.Exception = &exception{Class: "RSpec::Expectations::ExpectationNotMetError", Message: "expected true\n got false"}
			out.Summary.FailureCount++
		}
		out.Examples = append(out.Examples, x)
	}
	out.Summary.ExampleCount = len(examples)
	b, _ := json.Marshal(out)
	return string(b)
}

// clashSetup: the PR changes spec/models/a_spec.rb, the merged commit
// app/models/b.rb (spec/models/b_spec.rb) and lib/c.rb (no spec), and the
// merged tree fails "A works".
func (w *world) clashSetup(headStatus string) {
	w.prFiles = []string{"app/models/a.rb", "spec/models/a_spec.rb", "spec/models/gone_spec.rb"}
	w.mergedFiles = []string{"app/models/b.rb", "lib/c.rb", "README.md"}
	w.slots.trees[shaCommit] = []string{"spec/models/a_spec.rb", "spec/models/b_spec.rb"}
	w.slots.trees[shaHead] = []string{"spec/models/a_spec.rb", "spec/models/b_spec.rb"}
	w.reports[shaCommit] = rspecJSON(ex{"spec/models/a_spec.rb", "A works", "failed"}, ex{"spec/models/b_spec.rb", "B works", "passed"})
	w.reports[shaHead] = rspecJSON(ex{"spec/models/a_spec.rb", "A works", headStatus})
}

// event is the check's merge_check.result event.
func (w *world) event() store.Event {
	w.t.Helper()
	evs, err := w.st.EventsBySubject(w.ctx, "pr:talkable/talkable#11979", 0)
	if err != nil {
		w.t.Fatal(err)
	}
	for _, e := range evs {
		if e.Kind == EventKind {
			return e
		}
	}
	w.t.Fatalf("no %s event among %d", EventKind, len(evs))
	return store.Event{}
}

func (w *world) wantReleased() {
	w.t.Helper()
	if w.slots.releases != 1 || w.slots.releaseCtx != nil {
		w.t.Fatalf("releases = %d (ctx err %v), want one with a live context", w.slots.releases, w.slots.releaseCtx)
	}
	want := []string{gitx.MergeCheckRefPrefix + "review2/head", gitx.MergeCheckRefPrefix + "review2/merged", gitx.MergeCheckRefPrefix + "review2/tree"}
	if !slices.Equal(w.slots.refs, want) {
		w.t.Fatalf("released refs = %v, want %v", w.slots.refs, want)
	}
}

func TestMergeCheckReportsAClashWhenTheExamplePassesAtThePRHead(t *testing.T) {
	w := newWorld(t)
	w.clashSetup("passed")
	res, err := w.run(w.ctx, w.options())
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != VerdictClash || res.Merged != shaMerged || res.Head != shaHead || res.Tree != shaCommit {
		t.Fatalf("result = %s merged=%s head=%s tree=%s", res.Verdict, res.Merged, res.Head, res.Tree)
	}
	if want := []string{"spec/models/a_spec.rb", "spec/models/b_spec.rb"}; !slices.Equal(res.Specs, want) {
		t.Fatalf("specs = %v, want %v (the PR's spec, the merged subject's spec; none for lib/c.rb or a deleted spec)", res.Specs, want)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("failures = %+v", res.Failures)
	}
	f := res.Failures[0]
	if f.File != "spec/models/a_spec.rb" || f.Line != 10 || f.Description != "A works" || f.AtHead != AtHeadPassed || f.Verdict != VerdictClash {
		t.Fatalf("failure = %+v", f)
	}
	if !strings.HasPrefix(f.Error, "RSpec::Expectations::ExpectationNotMetError: expected true") {
		t.Fatalf("error = %q", f.Error)
	}
	if !slices.Equal(w.slots.checked, []string{shaCommit, shaHead}) {
		t.Fatalf("checkouts = %v, want the merged tree's commit, then the PR head", w.slots.checked)
	}
	// The spec runs go through the slot's db-lock line, the PR head's with the failing file only.
	merged, head := w.specLines[shaCommit], w.specLines[shaHead]
	slot := w.slots.slot.Path
	prefix := "/opt/magnum db-lock --checkout " + slot + " --role merge-check -- bin/rspec --format progress --format json --out "
	if len(merged) != 1 || !strings.HasPrefix(merged[0], prefix) || !strings.HasSuffix(merged[0], " spec/models/a_spec.rb spec/models/b_spec.rb") {
		t.Fatalf("merged-tree run = %q", merged)
	}
	if len(head) != 1 || !strings.HasPrefix(head[0], prefix) || !strings.HasSuffix(head[0], ".json spec/models/a_spec.rb") {
		t.Fatalf("PR head run = %q", head)
	}
	for _, c := range w.fake.CallsWithPrefix(Shell) {
		if c.Dir != slot || c.Env["WT_BRANCH"] != "review2" || !c.NoTTY || c.Args[0] != "-lc" {
			t.Fatalf("shell call %v: dir=%s env=%v notty=%v", c.Args, c.Dir, c.Env, c.NoTTY)
		}
	}
	if !slices.Equal(w.readiness, []string{"bin/rails db:test:prepare", "bin/rails db:test:prepare"}) {
		t.Fatalf("readiness = %q, want prepare on each checkout", w.readiness)
	}
	w.wantReleased()
	if !res.Released || res.File != filepath.Join(w.dir, ResultFile) {
		t.Fatalf("released=%v file=%q", res.Released, res.File)
	}
	var saved Result
	b, err := os.ReadFile(res.File)
	if err == nil {
		err = json.Unmarshal(b, &saved)
	}
	if err != nil || saved.Verdict != VerdictClash || len(saved.Failures) != 1 || !saved.Released {
		t.Fatalf("result file: %+v, %v", saved, err)
	}
	e := w.event()
	want := "merge-check of #11979 with aaaaaaa (#11939) in review2: clash: 1 failure on the merged tree (2 spec files): 1 clash, 0 already failing at the PR head"
	if e.Level != "warn" || e.Message != want {
		t.Fatalf("event = %s %q", e.Level, e.Message)
	}
	if strings.Contains(e.Message, "A works") {
		t.Fatalf("the event carries PR text: %q", e.Message)
	}
}

func TestMergeCheckReportsAlreadyFailingWhenTheExampleFailsAtThePRHeadToo(t *testing.T) {
	w := newWorld(t)
	w.clashSetup("failed")
	res, err := w.run(w.ctx, w.options())
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != VerdictAlreadyFailing || res.Failures[0].AtHead != AtHeadFailed || res.Failures[0].Verdict != VerdictAlreadyFailing {
		t.Fatalf("result = %s %+v", res.Verdict, res.Failures)
	}
	if e := w.event(); e.Level != "info" {
		t.Fatalf("event level = %s", e.Level)
	}
	w.wantReleased()
}

func TestMergeCheckCountsAnExampleThePRHeadLacksAsAClash(t *testing.T) {
	w := newWorld(t)
	w.clashSetup("passed")
	// The merged commit added "B adds" to b_spec.rb; the PR head's b_spec.rb lacks it.
	w.reports[shaCommit] = rspecJSON(ex{"spec/models/b_spec.rb", "B adds", "failed"}, ex{"spec/models/b_spec.rb", "B works", "passed"})
	w.reports[shaHead] = rspecJSON(ex{"spec/models/b_spec.rb", "B works", "passed"})
	res, err := w.run(w.ctx, w.options())
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != VerdictClash || res.Failures[0].AtHead != AtHeadAbsent {
		t.Fatalf("result = %s %+v", res.Verdict, res.Failures)
	}
}

func TestMergeCheckClassifiesALoadErrorByTheHeadsLoad(t *testing.T) {
	w := newWorld(t)
	w.clashSetup("passed")
	w.reports[shaCommit] = `{"messages":["\nAn error occurred while loading ./spec/models/a_spec.rb.\nFailure/Error: B.new\n\nNameError:\n  uninitialized constant B"],` +
		`"examples":[],"summary":{"example_count":0,"failure_count":0,"errors_outside_of_examples_count":1}}`
	w.exit[shaCommit] = 1
	res, err := w.run(w.ctx, w.options())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("failures = %+v", res.Failures)
	}
	f := res.Failures[0]
	if f.Kind != KindLoadError || f.File != "spec/models/a_spec.rb" || f.AtHead != AtHeadPassed || f.Verdict != VerdictClash || f.Error != "Failure/Error: B.new" {
		t.Fatalf("failure = %+v", f)
	}
}

func TestMergeCheckPassesWhenNoExampleFails(t *testing.T) {
	w := newWorld(t)
	w.clashSetup("passed")
	w.reports[shaCommit] = rspecJSON(ex{"spec/models/a_spec.rb", "A works", "passed"}, ex{"spec/models/b_spec.rb", "B works", "passed"})
	res, err := w.run(w.ctx, w.options())
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != VerdictPass || res.HeadRun != nil || !slices.Equal(w.slots.checked, []string{shaCommit}) {
		t.Fatalf("result = %s head run %v, checkouts %v", res.Verdict, res.HeadRun, w.slots.checked)
	}
	if got := Summary(res); got != "2 examples of 2 spec files pass on the merged tree" {
		t.Fatalf("summary = %q", got)
	}
	w.wantReleased()
}

func TestMergeCheckRunsNothingWhenNeitherSideTouchesASpec(t *testing.T) {
	w := newWorld(t)
	w.prFiles, w.mergedFiles = []string{"README.md"}, []string{"lib/c.rb", "app/views/x.html.haml"}
	res, err := w.run(w.ctx, w.options())
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != VerdictNoSpecs || len(w.specLines) != 0 {
		t.Fatalf("result = %s, spec runs %v", res.Verdict, w.specLines)
	}
	w.wantReleased()
}

func TestMergeCheckReportsATextualConflict(t *testing.T) {
	w := newWorld(t)
	w.clashSetup("passed")
	w.conflicts = []string{"app/models/b.rb", "config/routes.rb"}
	res, err := w.run(w.ctx, w.options())
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != VerdictConflict || !slices.Equal(res.Conflicts, w.conflicts) {
		t.Fatalf("result = %s %v", res.Verdict, res.Conflicts)
	}
	if len(w.slots.checked) != 0 || len(w.specLines) != 0 {
		t.Fatalf("a conflict checked out %v and ran %v", w.slots.checked, w.specLines)
	}
	if e := w.event(); e.Level != "warn" || !strings.Contains(e.Message, "the merge conflicts in 2 files") {
		t.Fatalf("event = %s %q", e.Level, e.Message)
	}
	w.wantReleased()
}

func TestMergeCheckReleasesTheSlotAfterAFailedSpecRun(t *testing.T) {
	w := newWorld(t)
	w.clashSetup("passed")
	delete(w.reports, shaCommit)
	w.exit[shaCommit] = 127
	res, err := w.run(w.ctx, w.options())
	if err == nil || !strings.Contains(err.Error(), "left no report") {
		t.Fatalf("err = %v", err)
	}
	if res.Verdict != VerdictError || !strings.Contains(res.Detail, "exit 127") {
		t.Fatalf("result = %s %q", res.Verdict, res.Detail)
	}
	w.wantReleased()
	if e := w.event(); e.Level != "error" {
		t.Fatalf("event level = %s", e.Level)
	}
}

func TestMergeCheckReleasesTheSlotAfterAnInterrupt(t *testing.T) {
	w := newWorld(t)
	w.clashSetup("passed")
	ctx, cancel := context.WithCancel(w.ctx)
	defer cancel()
	w.runErr = func(c execx.Cmd) error {
		cancel() // ctrl+c while the merged tree's specs run
		return &execx.RunError{Cmd: c, Err: context.Canceled}
	}
	res, err := w.run(ctx, w.options())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the interrupt", err)
	}
	if res.Verdict != VerdictError || !res.Released {
		t.Fatalf("result = %s released=%v", res.Verdict, res.Released)
	}
	w.wantReleased()
	w.event() // recorded despite the interrupt
}

func TestMergeCheckKeepLeavesTheSlotHeld(t *testing.T) {
	w := newWorld(t)
	w.clashSetup("passed")
	o := w.options()
	o.Keep = true
	res, err := w.run(w.ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if w.slots.releases != 0 || !res.Kept || res.Released {
		t.Fatalf("releases = %d kept=%v released=%v", w.slots.releases, res.Kept, res.Released)
	}
}

func TestMergeCheckHoldsNothingWhenNoSlotIsFree(t *testing.T) {
	w := newWorld(t)
	w.slots.holdErr = errors.New("slots: no free slot")
	res, err := w.run(w.ctx, w.options())
	if err == nil || res.Slot != "" {
		t.Fatalf("err = %v slot = %q", err, res.Slot)
	}
	if w.slots.releases != 0 || len(w.fake.Calls) != 0 {
		t.Fatalf("released %d, ran %d commands", w.slots.releases, len(w.fake.Calls))
	}
	if _, err := os.Stat(filepath.Join(w.dir, ResultFile)); !os.IsNotExist(err) {
		t.Fatalf("a result file without a check: %v", err)
	}
}

func TestMergeCheckFetchesAMergedCommitByIDWhenTheBaseLacksIt(t *testing.T) {
	w := newWorld(t)
	w.clashSetup("passed")
	w.refs = map[string]string{} // origin/master does not hold it
	w.remote[shaMerged] = shaMerged
	o := w.options()
	o.Merged = shaMerged
	res, err := w.run(w.ctx, o)
	if err != nil || res.Merged != shaMerged {
		t.Fatalf("result = %s merged=%s, %v", res.Verdict, res.Merged, err)
	}
	if w.refs[gitx.MergeCheckRefPrefix+"review2/merged"] != shaMerged {
		t.Fatalf("refs = %v", w.refs)
	}
}

func TestMergeCheckAsksForTheFullIDOfAMergedCommitTheBaseLacks(t *testing.T) {
	w := newWorld(t)
	w.refs = map[string]string{}
	res, err := w.run(w.ctx, w.options())
	if err == nil || !strings.Contains(err.Error(), "full id") || res.Verdict != VerdictError {
		t.Fatalf("result = %s, %v", res.Verdict, err)
	}
	w.wantReleased()
}

func TestSpecsForFollowsRailsConventions(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"app/models/user.rb":                       {"spec/models/user_spec.rb"},
		"app/services/billing/charge.rb":           {"spec/services/billing/charge_spec.rb"},
		"app/controllers/api/v2/foo_controller.rb": {"spec/controllers/api/v2/foo_controller_spec.rb", "spec/requests/api/v2/foo_spec.rb"},
		"lib/talkable/x.rb":                        {"spec/lib/talkable/x_spec.rb"},
		"config/initializers/y.rb":                 {"spec/config/initializers/y_spec.rb"},
		"spec/models/user_spec.rb":                 nil,
		"spec/support/helpers.rb":                  nil,
		"app/views/a.html.haml":                    nil,
		"README.md":                                nil,
	}
	for in, want := range cases {
		if got := SpecsFor(in); !slices.Equal(got, want) {
			t.Errorf("SpecsFor(%q) = %q, want %q", in, got, want)
		}
	}
}
