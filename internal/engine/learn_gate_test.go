package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/store"
)

// The commits of a gate's and a landing's fake checkout.
var (
	learnMaster    = strings.Repeat("1", 40) // master, the proposal's base
	learnCandidate = strings.Repeat("2", 40) // the proposal's commit on its branch
)

// fakeLearnGit is the loop's git (Deps.LearnGit) over a map of refs: what
// each directory's refs point at, the branch a repository has checked out,
// an operation in progress and the dirty paths; it records every command
// that changes something.
type fakeLearnGit struct {
	mu     sync.Mutex
	calls  []string
	refs   map[string]string // "<dir>|<ref>" -> sha
	branch map[string]string // repository -> its branch
	op     map[string]string // repository -> the operation in progress
	dirty  map[string][]string
	// parent is each commit's one parent (the history MergeBase walks and
	// "<sha>^" names), patch a commit's patch id ("patch-<sha>" when none:
	// RebaseOnto gives the new commit the old one's), worktree each
	// worktree's checkout and branch (RebaseOnto moves the branch), and
	// rebased counts the rebases.
	parent   map[string]string
	patch    map[string]string
	worktree map[string][2]string
	rebased  int
	// rebaseErr and mergeErr fail RebaseOnto and MergeFFOnly; onRebase and
	// onMerge run with the context of a rebase and of a fast-forward.
	rebaseErr, mergeErr error
	onRebase, onMerge   func(context.Context)
}

func newFakeLearnGit() *fakeLearnGit {
	return &fakeLearnGit{refs: map[string]string{}, branch: map[string]string{}, op: map[string]string{}, dirty: map[string][]string{},
		parent: map[string]string{}, patch: map[string]string{}, worktree: map[string][2]string{}}
}

func (g *fakeLearnGit) set(dir, ref, sha string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refs[dir+"|"+ref] = sha
}

func (g *fakeLearnGit) record(format string, args ...any) {
	g.calls = append(g.calls, fmt.Sprintf(format, args...))
}

// called lists the recorded commands that start with prefix.
func (g *fakeLearnGit) called(prefix string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, c := range g.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (g *fakeLearnGit) RevParse(_ context.Context, dir, ref string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if sha, ok := g.refs[dir+"|"+ref]; ok {
		return sha, nil
	}
	if sha, ok := strings.CutSuffix(ref, "^"); ok && g.parent[sha] != "" {
		return g.parent[sha], nil
	}
	return "", fmt.Errorf("rev-parse %s: %w", ref, gitx.ErrNoSuchRef) // "<sha>^2" too: no merge commits
}

func (g *fakeLearnGit) PatchID(_ context.Context, _, rev string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.patchOf(rev), nil
}

func (g *fakeLearnGit) patchOf(sha string) string { return cmp.Or(g.patch[sha], "patch-"+sha) }

func (g *fakeLearnGit) WorktreeAdd(_ context.Context, mainClone, path, ref string, _ bool, branch string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.record("worktree add %s %s %s", path, ref, branch)
	sha, ok := g.refs[mainClone+"|refs/heads/"+ref]
	if !ok {
		sha = ref
	}
	g.refs[path+"|HEAD"] = sha
	g.worktree[path] = [2]string{mainClone, cmp.Or(branch, ref)}
	if branch != "" {
		g.refs[mainClone+"|refs/heads/"+branch] = sha
	}
	return os.MkdirAll(path, 0o700)
}

func (g *fakeLearnGit) WorktreeDiscard(_ context.Context, _, dir string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.record("worktree discard %s", dir)
	delete(g.refs, dir+"|HEAD")
	return os.RemoveAll(dir)
}

func (g *fakeLearnGit) StatusPaths(_ context.Context, dir string) ([]gitx.StatusEntry, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []gitx.StatusEntry
	for _, p := range g.dirty[dir] {
		out = append(out, gitx.StatusEntry{X: ' ', Y: 'M', Path: p})
	}
	return out, nil
}

func (g *fakeLearnGit) CurrentBranch(_ context.Context, repo string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.branch[repo], nil
}

func (g *fakeLearnGit) CommitAll(context.Context, string, string, ...string) (string, error) {
	return "", errors.New("fake git: no commit")
}

func (g *fakeLearnGit) ChangedPaths(context.Context, string, string, string, ...string) ([]string, error) {
	return nil, errors.New("fake git: no diff")
}

func (g *fakeLearnGit) InProgress(_ context.Context, repo string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.op[repo], nil
}

func (g *fakeLearnGit) MergeBase(_ context.Context, _, a, b string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mergeBase(a, b)
}

// mergeBase is the first of a's ancestors (a included) that b has.
func (g *fakeLearnGit) mergeBase(a, b string) (string, error) {
	var bs []string
	for x := b; x != ""; x = g.parent[x] {
		bs = append(bs, x)
	}
	for x := a; x != ""; x = g.parent[x] {
		if slices.Contains(bs, x) {
			return x, nil
		}
	}
	return "", fmt.Errorf("merge-base %s %s: %w", a, b, gitx.ErrNoMergeBase)
}

// RebaseOnto puts the worktree's commit on onto as a new commit with its
// patch id, moving its branch, unless rebaseErr says otherwise.
func (g *fakeLearnGit) RebaseOnto(ctx context.Context, dir, onto, upstream string) error {
	g.mu.Lock()
	g.record("rebase %s %s %s", dir, onto, upstream)
	on := g.onRebase
	g.mu.Unlock()
	if on != nil {
		on(ctx)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.rebaseErr != nil {
		return g.rebaseErr
	}
	g.rebased++
	sha := strings.Repeat(strconv.Itoa(2+g.rebased), 40)
	g.parent[sha], g.patch[sha] = onto, g.patchOf(g.refs[dir+"|HEAD"])
	g.refs[dir+"|HEAD"] = sha
	if wt, ok := g.worktree[dir]; ok {
		g.refs[wt[0]+"|refs/heads/"+wt[1]] = sha
	}
	return nil
}

func (g *fakeLearnGit) DirtyPaths(_ context.Context, dir string, paths ...string) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, p := range g.dirty[dir] {
		if slices.Contains(paths, p) {
			out = append(out, p)
		}
	}
	return out, nil
}

// MergeFFOnly moves the repository's master and HEAD to branch when branch
// sits on master (nothing when master holds it), unless mergeErr says
// otherwise.
func (g *fakeLearnGit) MergeFFOnly(ctx context.Context, repo, branch string) error {
	g.mu.Lock()
	g.record("merge --ff-only %s", branch)
	on := g.onMerge
	g.mu.Unlock()
	if on != nil {
		on(ctx)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mergeErr != nil {
		return g.mergeErr
	}
	head, master := g.refs[repo+"|refs/heads/"+branch], g.refs[repo+"|refs/heads/"+learnBase]
	switch base, err := g.mergeBase(head, master); {
	case err == nil && base == head:
		return nil // already up to date
	case err != nil || base != master:
		return fmt.Errorf("merge --ff-only %s: %w", branch, gitx.ErrNotFastForward)
	}
	g.refs[repo+"|refs/heads/"+learnBase], g.refs[repo+"|HEAD"] = head, head
	return nil
}

func (g *fakeLearnGit) RestoreFrom(_ context.Context, dir, rev string, paths ...string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.record("restore %s %s %s", dir, rev, strings.Join(paths, " "))
	return errors.New("fake git: no restore")
}

func (g *fakeLearnGit) BranchDelete(_ context.Context, mainClone, branch string, _ bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.record("branch delete %s", branch)
	delete(g.refs, mainClone+"|refs/heads/"+branch)
	return nil
}

// fakeEval replays a case with the scores a test scripted for its
// candidate directory and name, one per replay (the last repeats), or with
// an outcome without a score; it records every replay.
type fakeEval struct {
	mu       sync.Mutex
	scores   map[string][]*eval.Score // "<candidate>|<case>"
	outcomes map[string]string        // "<candidate>|<case>" -> an outcome with no score
	inputs   map[string][2]string     // candidate -> its inputs and reviewer inputs
	root     string                   // the eval runs (Layout.Eval), where a judge-only replay finds its reports
	corpus   *eval.Corpus
	calls    []evalCall
	// entered and block hold a replay: entered is closed when one starts,
	// and it returns when block closes or its context ends.
	entered, block chan struct{}
	// after runs once a replay ended, on the gate's goroutine.
	after func(evalCall)
}

type evalCall struct {
	Case string
	Opts EvalGateOptions
}

func (f *fakeEval) script(candidate, name string, scores ...*eval.Score) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scores[candidate+"|"+name] = scores
}

// fail scripts the replays of a case with candidate to end with outcome and
// no score ("" = scored again).
func (f *fakeEval) fail(candidate, name, outcome string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if outcome == "" {
		delete(f.outcomes, candidate+"|"+name)
		return
	}
	f.outcomes[candidate+"|"+name] = outcome
}

func (f *fakeEval) run(ctx context.Context, c eval.Case, o EvalGateOptions) eval.CaseRun {
	cr := f.replay(ctx, c, o)
	f.mu.Lock()
	after := f.after
	f.mu.Unlock()
	if after != nil {
		after(evalCall{Case: c.Name, Opts: o})
	}
	return cr
}

func (f *fakeEval) replay(ctx context.Context, c eval.Case, o EvalGateOptions) eval.CaseRun {
	f.mu.Lock()
	key := o.Candidate + "|" + c.Name
	n := 0
	for _, call := range f.calls {
		if call.Opts.Candidate+"|"+call.Case == key {
			n++
		}
	}
	f.calls = append(f.calls, evalCall{Case: c.Name, Opts: o})
	entered, block := f.entered, f.block
	f.entered = nil
	f.mu.Unlock()
	if entered != nil {
		close(entered)
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return eval.CaseRun{Case: c.Name, PR: c.PR, Head: c.Head, Outcome: "error", Error: ctx.Err().Error()}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cr := eval.CaseRun{Case: c.Name, PR: c.PR, Head: c.Head, Outcome: "dry_run", ReportDir: filepath.Join(o.RunDir, "cases", c.Name), JudgeOnly: o.JudgeOnly}
	if o.JudgeOnly { // the reports of the newest full replay whose reviewers read what this one's would, as evalrun.RunCase takes them
		runs, err := eval.ListRuns(f.root)
		if err != nil {
			cr.Outcome, cr.Error, cr.ReportDir = "error", err.Error(), ""
			return cr
		}
		r, fc, ok := eval.FullReplayFor(runs, c.Name, c.Head, &eval.Run{ReviewerInputs: f.inputs[o.Candidate][1]})
		if !ok || fc.ReportDir == "" {
			cr.Outcome, cr.Error, cr.ReportDir = "error", "no full replay for the judge alone", ""
			return cr
		}
		cr.ReportsRun = r.ID
	}
	if out, ok := f.outcomes[key]; ok {
		cr.Outcome, cr.Error, cr.ReportDir = out, "scripted "+out, ""
		return cr
	}
	ss := f.scores[key]
	if len(ss) == 0 {
		cr.Outcome, cr.Error, cr.ReportDir = "error", "no score scripted for "+key, ""
		return cr
	}
	s := *ss[min(n, len(ss)-1)]
	s.Case = c.Name
	cr.Score = &s
	return cr
}

func (f *fakeEval) inputsOf(candidate string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in, ok := f.inputs[candidate]
	if !ok {
		return "", "", errors.New("no inputs for " + candidate)
	}
	return in[0], in[1], nil
}

func (f *fakeEval) callsFor(candidate string) []evalCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []evalCall
	for _, c := range f.calls {
		if c.Opts.Candidate == candidate {
			out = append(out, c)
		}
	}
	return out
}

// gateFixture is a harness with a gating proposal: its worktree on disk at
// its commit, a fake git and fake eval replays, a corpus of the learned
// case of the miss it noted (its target) and an operator's case.
type gateFixture struct {
	h        *harness
	git      *fakeLearnGit
	ev       *fakeEval
	checkout string
	wt       string
	base     string // the gate's worktree of the proposal's base
	miss     store.Miss
	target   eval.Case
	other    eval.Case
	p        store.LearnProposal
}

const gateRunID = "20261005-090000"

// The fixture's candidate files: a reviewer prompt changes, so the gate
// replays every role.
var gateFullFiles = []string{learn.DecisionsFile, "prompts/claude-review.md", learn.SkillFile}

func newGateFixture(t *testing.T, files []string, mods ...func(*harness)) *gateFixture {
	t.Helper()
	f := &gateFixture{git: newFakeLearnGit(), ev: &fakeEval{scores: map[string][]*eval.Score{}, outcomes: map[string]string{}}}
	f.checkout = filepath.Join(t.TempDir(), "magnum")
	f.h = newHarness(t, append([]func(*harness){func(h *harness) {
		h.d.Checkout, h.d.LearnGit = f.checkout, f.git
		h.d.EvalCase, h.d.EvalInputs = f.ev.run, f.ev.inputsOf
		h.d.EvalRuns = func() ([]eval.Run, error) { return eval.ListRuns(h.layout.Eval()) }
		f.ev.root = h.layout.Eval()
		h.d.EvalCorpus = func() (*eval.Corpus, string, error) { return f.ev.corpus, "", nil }
	}}, mods...)...)
	pr := corpusPR(f.h, "talkable/talkable", 11)
	m := corpusMiss(f.h, pr, 1, store.MissScopeGeneral)
	ms, err := f.h.st.Misses(f.h.ctx, store.MissFilter{}) // with its PR's repository and number
	if err != nil || len(ms) != 1 || ms[0].ID != m.ID {
		t.Fatalf("misses = %+v, %v", ms, err)
	}
	f.miss = ms[0]
	var ok bool
	if f.target, ok = learn.CaseFromMiss(f.miss); !ok {
		t.Fatal("the miss makes no case")
	}
	f.other = eval.Case{Name: "coupon-scope", PR: "talkable/talkable#12", Head: retroShaA,
		Defects: []eval.Defect{{ID: "scope", Title: "the site scope", Paths: []string{"app/models/coupon.rb"}}}}
	f.ev.corpus = &eval.Corpus{Cases: []eval.Case{f.other, f.target}}
	f.wt = filepath.Join(f.h.layout.LearnWorktrees(), gateRunID)
	f.base = filepath.Join(f.h.layout.LearnWorktrees(), gateBaseName)
	if err := os.MkdirAll(f.wt, 0o700); err != nil {
		t.Fatal(err)
	}
	f.git.set(f.wt, "HEAD", learnCandidate)
	f.git.parent[learnCandidate] = learnMaster
	f.git.worktree[f.wt] = [2]string{f.checkout, "learn/" + gateRunID}
	f.git.set(f.checkout, "refs/heads/learn/"+gateRunID, learnCandidate)
	f.git.set(f.checkout, "refs/heads/"+learnBase, learnMaster)
	f.git.set(f.checkout, "HEAD", learnMaster)
	f.git.branch[f.checkout] = learnBase
	rev := "rev-cand"
	if learn.Mode(files) == learn.GateJudgeOnly {
		rev = "rev-master"
	}
	// The checkout's own prompts are not the base's: a baseline replays the
	// gate's worktree of the base.
	f.ev.inputs = map[string][2]string{f.base: {"in-master", "rev-master"}, f.checkout: {"in-checkout", "rev-checkout"}, f.wt: {"in-cand", rev}}
	p, err := f.h.st.CreateLearnProposal(f.h.ctx, store.LearnProposalInput{Trigger: store.LearnTriggerRequest, BaseSHA: learnMaster,
		Branch: "learn/" + gateRunID, CandidateSHA: learnCandidate, Files: files, State: store.LearnGating, Model: "sonnet",
		Scratch: f.wt, RunDir: filepath.Join(f.h.layout.LearnSkill(), gateRunID),
		Inputs: []store.LearnInput{{Kind: store.LearnInputMiss, InputID: f.miss.ID, Outcome: store.MissNoted, Where: "SKILL.md section 4"}}})
	if err != nil {
		t.Fatal(err)
	}
	f.p = p
	return f
}

// baseline stores a scored full replay of cases with master's inputs, as an
// earlier `magnum eval run` would (reviewer "" records no reviewer inputs).
func (f *gateFixture) baseline(id, reviewer string, scores map[string]*eval.Score) {
	f.h.t.Helper()
	f.fullRun(id, "in-master", reviewer, scores)
}

// fullRun stores a scored full replay of cases with inputs and reviewer
// inputs.
func (f *gateFixture) fullRun(id, inputs, reviewer string, scores map[string]*eval.Score) {
	f.h.t.Helper()
	r := eval.Run{ID: id, Inputs: inputs, ReviewerInputs: reviewer, Commit: "abc1234"}
	for _, c := range []eval.Case{f.other, f.target} {
		if s, ok := scores[c.Name]; ok {
			s.Case = c.Name
			r.Cases = append(r.Cases, eval.CaseRun{Case: c.Name, PR: c.PR, Head: c.Head, Outcome: "dry_run", Score: s,
				ReportDir: filepath.Join(f.h.layout.Eval(), id, "cases", c.Name)})
		}
	}
	if err := eval.SaveRun(filepath.Join(f.h.layout.Eval(), id), r); err != nil {
		f.h.t.Fatal(err)
	}
}

// proposal reads the fixture's proposal back.
func (f *gateFixture) proposal() store.LearnProposal {
	f.h.t.Helper()
	p, err := f.h.st.LearnProposalByID(f.h.ctx, f.p.ID)
	if err != nil {
		f.h.t.Fatal(err)
	}
	return p
}

// gate decodes the proposal's gate result.
func (f *gateFixture) gate() GateResult {
	f.h.t.Helper()
	var g GateResult
	if err := json.Unmarshal(f.proposal().Gate, &g); err != nil {
		f.h.t.Fatalf("gate %q: %v", f.proposal().Gate, err)
	}
	return g
}

// candidateGit lists the recorded git commands that start with one of
// prefixes, leaving out those of the gate's base worktree.
func (f *gateFixture) candidateGit(prefixes ...string) []string {
	var out []string
	for _, prefix := range prefixes {
		for _, c := range f.git.called(prefix) {
			if !strings.Contains(c, f.base) {
				out = append(out, c)
			}
		}
	}
	return out
}

func (f *gateFixture) toasts() string {
	f.h.t.Helper()
	if _, err := f.h.e.batch.Flush(f.h.ctx); err != nil {
		f.h.t.Fatal(err)
	}
	return strings.Join(f.h.nh.all(), "\n")
}

// found is a score with the defects ids found and missed.
func found(ids []string, missed ...string) *eval.Score {
	s := &eval.Score{}
	for _, id := range ids {
		s.Defects = append(s.Defects, eval.DefectResult{ID: id, Found: true})
		s.Found++
		s.Total++
	}
	for _, id := range missed {
		s.Defects = append(s.Defects, eval.DefectResult{ID: id})
		s.Total++
	}
	return s
}

func TestTheGatePassesACandidateThatFindsItsTarget(t *testing.T) {
	f := newGateFixture(t, gateFullFiles)
	// The target has a baseline; the operator's case has none, so the
	// gate replays the base on it first.
	f.baseline("20261001-120000", "rev-master", map[string]*eval.Score{f.target.Name: found(nil, f.target.Name)})
	f.ev.script(f.base, f.other.Name, found([]string{"scope"}))
	f.ev.script(f.wt, f.target.Name, found([]string{f.target.Name}))
	f.ev.script(f.wt, f.other.Name, found([]string{"scope"}))
	f.h.startup()
	f.h.tick()

	p := f.proposal()
	if p.State != store.LearnPassed || !strings.Contains(p.Reason, f.target.Name+" found (0 → 1 of 1)") || p.DecidedAt != nil {
		t.Fatalf("proposal = %s %q decided %v, want passed naming its target", p.State, p.Reason, p.DecidedAt)
	}
	base, cand := f.ev.callsFor(f.base), f.ev.callsFor(f.wt)
	if len(base) != 1 || base[0].Case != f.other.Name || base[0].Opts.JudgeOnly {
		t.Fatalf("baseline replays = %+v, want one full replay of %s", base, f.other.Name)
	}
	if len(cand) != 2 || cand[0].Case != f.target.Name || cand[1].Case != f.other.Name || cand[0].Opts.JudgeOnly {
		t.Fatalf("candidate replays = %+v, want the target, then the other case, every role", cand)
	}
	if cand[0].Opts.RunDir == base[0].Opts.RunDir || cand[0].Opts.AgentTag == base[0].Opts.AgentTag ||
		!strings.HasPrefix(cand[0].Opts.AgentTag, "eval-") || !strings.HasPrefix(cand[0].Opts.RunDir, f.h.layout.Eval()+string(filepath.Separator)) {
		t.Fatalf("the runs share a directory or a tag: %+v and %+v", cand[0].Opts, base[0].Opts)
	}
	g := f.gate()
	if g.State != store.LearnPassed || g.Mode != learn.GateFull || len(g.Cases) != 2 || len(g.Runs) != 2 || g.Inputs != "in-cand" ||
		g.BaselineInputs != "in-master" {
		t.Fatalf("gate = %+v", g)
	}
	candRun, baseRun := g.Cases[0].CandidateRun, g.Cases[1].BaselineRun
	if candRun == baseRun || !slices.Contains(g.Runs, candRun) || !slices.Contains(g.Runs, baseRun) {
		t.Fatalf("runs %q: the candidate's %s, the baseline's %s", g.Runs, candRun, baseRun)
	}
	if c := g.Cases[0]; c.Case != f.target.Name || !c.Target || c.BaselineRun != "20261001-120000" || c.Before == nil || c.After == nil ||
		c.After.Found != 1 {
		t.Fatalf("the target's row = %+v", c)
	}
	if c := g.Cases[1]; c.Case != f.other.Name || c.Target || c.CandidateRun != candRun {
		t.Fatalf("the other case's row = %+v", c)
	}
	// Every replay is stored as `magnum eval run` stores one: a later gate
	// takes the baseline, a judge-only replay the reports.
	runs, err := eval.ListRuns(f.h.layout.Eval())
	if err != nil || len(runs) != 3 {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	for _, r := range runs[1:] {
		want := [2]string{"in-master", "rev-master"}
		if r.ID == candRun {
			want = [2]string{"in-cand", "rev-cand"}
		}
		if r.Inputs != want[0] || r.ReviewerInputs != want[1] || r.NoNotes || r.Finished.IsZero() || r.ID == baseRun && len(r.Cases) != 1 {
			t.Fatalf("run %s = %+v, want inputs %v", r.ID, r, want)
		}
	}
	if bs := eval.Baselines(runs, "in-master", f.other); len(bs) != 1 || bs[0].Run != baseRun {
		t.Fatalf("the baseline replay is no baseline: %+v", bs)
	}
	if evs := f.h.events("learn.gate_done"); len(evs) != 1 || !strings.Contains(evs[0].Message, "passed") || len(f.h.events("learn.gate_start")) != 1 {
		t.Fatalf("gate events: %+v", evs)
	}
	if _, ok := f.h.e.getKV(f.h.ctx, KVLearnGating); ok {
		t.Fatal("the gate's mark outlived it")
	}
	if got := f.candidateGit("worktree"); len(got) != 0 {
		t.Fatalf("a passed proposal's worktree was touched: %q", got)
	}
	// The next tick starts nothing more.
	f.h.tick()
	if n := len(f.ev.callsFor(f.wt)); n != 2 {
		t.Fatalf("the gate ran again: %d candidate replays", n)
	}
}

// The baseline is the proposal's base commit, replayed from a detached
// worktree of its own, whatever branch the checkout has checked out and
// whatever it left uncommitted: the worktree goes once the gate decided,
// and the gate's runs record the commit they read, clean, so a later gate
// reuses them.
func TestTheBaselineReplaysTheProposalsBaseNotTheCheckout(t *testing.T) {
	f := newGateFixture(t, gateFullFiles)
	// The operator works on another branch, with an experiment in a judge
	// prompt left uncommitted.
	f.git.branch[f.checkout] = "experiment"
	f.git.set(f.checkout, "HEAD", strings.Repeat("9", 40))
	f.git.dirty[f.checkout] = []string{"prompts/judge-initial.md"}
	f.baseline("20261001-120000", "rev-master", map[string]*eval.Score{f.target.Name: found(nil, f.target.Name)})
	f.ev.script(f.base, f.other.Name, found([]string{"scope"}))
	f.ev.script(f.wt, f.target.Name, found([]string{f.target.Name}))
	f.ev.script(f.wt, f.other.Name, found([]string{"scope"}))
	f.h.tick()

	if p := f.proposal(); p.State != store.LearnPassed {
		t.Fatalf("proposal = %s %q", p.State, p.Reason)
	}
	if got := f.ev.callsFor(f.checkout); len(got) != 0 {
		t.Fatalf("the checkout was replayed as the baseline: %+v", got)
	}
	base := f.ev.callsFor(f.base)
	if len(base) != 1 || base[0].Case != f.other.Name || base[0].Opts.JudgeOnly {
		t.Fatalf("baseline replays = %+v, want one full replay of the base's worktree", base)
	}
	g := f.gate()
	if g.BaselineInputs != "in-master" {
		t.Fatalf("baseline inputs = %q, want the base's", g.BaselineInputs)
	}
	want := []string{"worktree discard " + f.base, "worktree add " + f.base + " " + learnMaster + " ", "worktree discard " + f.base}
	if got := f.git.called("worktree"); !slices.Equal(got, want) {
		t.Fatalf("worktree commands = %q, want %q", got, want)
	}
	if _, err := os.Stat(f.base); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the base's worktree outlived the gate: %v", err)
	}
	runs, err := eval.ListRuns(f.h.layout.Eval())
	if err != nil || len(runs) != 3 {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	baseRun, candRun := g.Cases[1].BaselineRun, g.Cases[0].CandidateRun
	for _, r := range runs {
		switch r.ID {
		case baseRun:
			if r.Commit != learnShort(learnMaster) || r.Dirty || r.Candidate != f.base {
				t.Fatalf("the baseline run = %+v, want the base's commit, clean", r)
			}
		case candRun:
			if r.Commit != learnShort(learnCandidate) || r.Dirty {
				t.Fatalf("the candidate run = %+v, want the candidate's commit, clean", r)
			}
		}
	}
	if bs := eval.Baselines(runs, "in-master", f.other); len(bs) != 1 || bs[0].Run != baseRun {
		t.Fatalf("a later gate cannot reuse the baseline: %+v", bs)
	}
}

func TestTheGateReplaysALostCaseOnceBeforeFailing(t *testing.T) {
	f := newGateFixture(t, gateFullFiles)
	f.baseline("20261001-120000", "rev-master", map[string]*eval.Score{f.target.Name: found(nil, f.target.Name),
		f.other.Name: found([]string{"scope"})})
	f.ev.script(f.wt, f.target.Name, found([]string{f.target.Name}))
	f.ev.script(f.wt, f.other.Name, found(nil, "scope"), found(nil, "scope"))
	f.h.tick()

	if got := f.ev.callsFor(f.wt); len(got) != 3 || got[2].Case != f.other.Name || got[2].Opts.RunDir == got[1].Opts.RunDir {
		t.Fatalf("candidate replays = %+v, want the lost case replayed once more in a run of its own", got)
	}
	p := f.proposal()
	if p.State != store.LearnFailed || !strings.Contains(p.Reason, f.other.Name+" lost scope") || p.DecidedAt == nil {
		t.Fatalf("proposal = %s %q, want failed: the lost case", p.State, p.Reason)
	}
	g := f.gate()
	if c := g.Cases[1]; !c.Retried || c.CandidateRun != g.Runs[1] || len(g.Runs) != 2 {
		t.Fatalf("the lost row = %+v, runs %q", c, g.Runs)
	}

	// A second replay that finds it again is no loss.
	f2 := newGateFixture(t, gateFullFiles)
	f2.baseline("20261001-120000", "rev-master", map[string]*eval.Score{f2.target.Name: found(nil, f2.target.Name),
		f2.other.Name: found([]string{"scope"})})
	f2.ev.script(f2.wt, f2.target.Name, found([]string{f2.target.Name}))
	f2.ev.script(f2.wt, f2.other.Name, found(nil, "scope"), found([]string{"scope"}))
	f2.h.tick()
	if p := f2.proposal(); p.State != store.LearnPassed || len(f2.ev.callsFor(f2.wt)) != 3 {
		t.Fatalf("proposal = %s %q after the case was found again", p.State, p.Reason)
	}
}

func TestTheGateHoldsWhenTheTargetHasNoFullReplayForJudgeOnly(t *testing.T) {
	f := newGateFixture(t, []string{learn.DecisionsFile, "prompts/judge-initial.md", learn.SkillFile})
	// The baseline run recorded no reviewer inputs: its reports cannot
	// stand in for the candidate's reviewers.
	f.baseline("20261001-120000", "", map[string]*eval.Score{f.target.Name: found(nil, f.target.Name),
		f.other.Name: found([]string{"scope"})})
	f.h.tick()

	if got := f.ev.callsFor(f.wt); len(got) != 0 {
		t.Fatalf("the judge replayed without the reviewers' reports: %+v", got)
	}
	p := f.proposal()
	if p.State != store.LearnHeld || !strings.Contains(p.Reason, "no target could be replayed") || !strings.Contains(p.Reason, "no full replay") {
		t.Fatalf("proposal = %s %q, want held: no full replay", p.State, p.Reason)
	}
	if g := f.gate(); g.Mode != learn.GateJudgeOnly {
		t.Fatalf("mode = %s", g.Mode)
	}
	if evs := f.h.events("learn.held"); len(evs) != 1 || !strings.Contains(f.toasts(), fmt.Sprintf("skill proposal %d waits for review", p.ID)) {
		t.Fatalf("held events %+v, toasts %q", evs, f.toasts())
	}
	if got := f.candidateGit("worktree discard"); len(got) != 0 {
		t.Fatalf("a held proposal lost its worktree: %q", got)
	}

	// With a full replay whose reviewers read what the candidate's do, the
	// judge replays alone on it.
	f2 := newGateFixture(t, []string{learn.DecisionsFile, learn.SkillFile})
	f2.baseline("20261001-120000", "rev-master", map[string]*eval.Score{f2.target.Name: found(nil, f2.target.Name),
		f2.other.Name: found([]string{"scope"})})
	f2.ev.script(f2.wt, f2.target.Name, found([]string{f2.target.Name}))
	f2.ev.script(f2.wt, f2.other.Name, found([]string{"scope"}))
	f2.h.tick()
	got := f2.ev.callsFor(f2.wt)
	if len(got) != 2 || !got[0].Opts.JudgeOnly || f2.proposal().State != store.LearnPassed {
		t.Fatalf("judge-only replays = %+v, proposal %s", got, f2.proposal().State)
	}
}

// In judge-only mode both judges read the same reviewer reports: those of
// the case's newest full replay whose reviewers read the candidate's
// prompts. When the base's judge did not make that replay, its baseline is
// the base's judge replayed alone on those reports, not an older full
// replay whose reviewers happened to find less.
func TestAJudgeOnlyGateComparesBothJudgesOnTheSameReports(t *testing.T) {
	f := newGateFixture(t, []string{learn.DecisionsFile, learn.SkillFile})
	// An older full replay with the base's inputs, whose reviewers missed
	// the target, and a newer one with the same reviewers and another judge
	// (another proposal's), whose reviewers found it.
	f.baseline("20261001-120000", "rev-master", map[string]*eval.Score{f.target.Name: found(nil, f.target.Name),
		f.other.Name: found([]string{"scope"})})
	f.fullRun("20261002-120000", "in-other", "rev-master", map[string]*eval.Score{f.target.Name: found([]string{f.target.Name}),
		f.other.Name: found([]string{"scope"})})
	// On the newer reports the base's judge finds the target too: the
	// candidate's judge changes nothing.
	f.ev.script(f.base, f.target.Name, found([]string{f.target.Name}))
	f.ev.script(f.base, f.other.Name, found([]string{"scope"}))
	f.ev.script(f.wt, f.target.Name, found([]string{f.target.Name}))
	f.ev.script(f.wt, f.other.Name, found([]string{"scope"}))
	f.h.tick()

	p := f.proposal()
	if p.State != store.LearnFailed || !strings.Contains(p.Reason, "no target improved") {
		t.Fatalf("proposal = %s %q, want failed: on the same reports the target is found either way", p.State, p.Reason)
	}
	base := f.ev.callsFor(f.base)
	if len(base) != 2 || !base[0].Opts.JudgeOnly || base[0].Case != f.target.Name {
		t.Fatalf("baseline replays = %+v, want the base's judge alone on each case", base)
	}
	g := f.gate()
	for _, c := range g.Cases {
		if c.ReportsRun != "20261002-120000" || c.BaselineRun == "20261001-120000" || c.Before == nil || c.Before.Found != 1 {
			t.Fatalf("row %+v, want both judges on the reports of 20261002-120000", c)
		}
	}

	// A full replay the base's judge made is its own baseline: nothing of
	// the base is replayed.
	f2 := newGateFixture(t, []string{learn.DecisionsFile, learn.SkillFile})
	f2.fullRun("20261001-120000", "in-other", "rev-master", map[string]*eval.Score{f2.target.Name: found([]string{f2.target.Name})})
	f2.baseline("20261002-120000", "rev-master", map[string]*eval.Score{f2.target.Name: found(nil, f2.target.Name),
		f2.other.Name: found([]string{"scope"})})
	f2.ev.script(f2.wt, f2.target.Name, found([]string{f2.target.Name}))
	f2.ev.script(f2.wt, f2.other.Name, found([]string{"scope"}))
	f2.h.tick()
	if p := f2.proposal(); p.State != store.LearnPassed || len(f2.ev.callsFor(f2.base)) != 0 {
		t.Fatalf("proposal = %s %q, base replays %+v", p.State, p.Reason, f2.ev.callsFor(f2.base))
	}
	if c := f2.gate().Cases[0]; c.ReportsRun != "20261002-120000" || c.BaselineRun != "20261002-120000" {
		t.Fatalf("the target's row = %+v", c)
	}
}

func TestTheGateWaitsAboveTheSoftCap(t *testing.T) {
	u := &fakeUsage{}
	f := newGateFixture(t, gateFullFiles, func(h *harness) { h.d.Usage, h.d.UsageAt = u.read, u.readAt })
	u.set(85, f.h.clock.Now().Add(48*time.Hour))
	f.baseline("20261001-120000", "rev-master", map[string]*eval.Score{f.target.Name: found(nil, f.target.Name),
		f.other.Name: found([]string{"scope"})})
	f.h.startup()
	f.h.tick()
	f.h.advance(time.Minute)
	f.h.tick()
	if got := f.ev.callsFor(f.wt); len(got) != 0 || f.proposal().State != store.LearnGating {
		t.Fatalf("replays %+v at the soft cap, proposal %s", got, f.proposal().State)
	}
	if evs := f.h.events("learn.gate_waiting"); len(evs) != 1 || !strings.Contains(evs[0].Message, "codex_soft") {
		t.Fatalf("waiting events = %+v, want one for the window", evs)
	}
	if toasts := f.toasts(); strings.Count(toasts, "waits for the Codex budget") != 1 {
		t.Fatalf("toasts = %q", toasts)
	}
	// Below the cap again, the gate runs.
	f.ev.script(f.wt, f.target.Name, found([]string{f.target.Name}))
	f.ev.script(f.wt, f.other.Name, found([]string{"scope"}))
	u.set(40, f.h.clock.Now().Add(48*time.Hour))
	f.h.advance(2 * time.Minute)
	f.h.tick()
	if p := f.proposal(); p.State != store.LearnPassed {
		t.Fatalf("proposal = %s %q below the cap", p.State, p.Reason)
	}

	// A replay the budget refuses mid-gate stops it: the proposal stays
	// gating and the gate waits before it starts again.
	f2 := newGateFixture(t, gateFullFiles)
	f2.baseline("20261001-120000", "rev-master", map[string]*eval.Score{f2.target.Name: found(nil, f2.target.Name),
		f2.other.Name: found([]string{"scope"})})
	f2.ev.outcomes[f2.wt+"|"+f2.target.Name] = eval.OutcomeBudget
	f2.h.tick()
	f2.h.tick()
	if p, n := f2.proposal(), len(f2.ev.callsFor(f2.wt)); p.State != store.LearnGating || n != 1 {
		t.Fatalf("proposal %s after %d replays, want gating after one", p.State, n)
	}
	if evs := f2.h.events("learn.gate_waiting"); len(evs) != 1 || !strings.Contains(evs[0].Message, "budget") {
		t.Fatalf("waiting events = %+v", evs)
	}
}

// A running gate checks what holds it before each replay: a drain for a
// restart or `magnum pause` that came after it started stops it, the
// proposal stays gating, and the gate waits out its retry.
func TestADrainOrAPauseStopsARunningGateBeforeItsNextReplay(t *testing.T) {
	for _, c := range []struct{ name, key, want string }{
		{"drain", KVDaemonDraining, "draining for a restart"},
		{"pause", store.KVDaemonPaused, "daemon paused"},
	} {
		f := newGateFixture(t, gateFullFiles)
		f.baseline("20261001-120000", "rev-master", map[string]*eval.Score{f.target.Name: found(nil, f.target.Name),
			f.other.Name: found([]string{"scope"})})
		f.ev.script(f.wt, f.target.Name, found([]string{f.target.Name}))
		f.ev.script(f.wt, f.other.Name, found([]string{"scope"}))
		f.ev.after = func(evalCall) {
			if err := f.h.st.SetKV(context.WithoutCancel(f.h.ctx), c.key, "1"); err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
		}
		f.h.tick()
		if p, n := f.proposal(), len(f.ev.callsFor(f.wt)); p.State != store.LearnGating || n != 1 {
			t.Fatalf("%s: proposal %s after %d candidate replays, want gating after one", c.name, p.State, n)
		}
		if evs := f.h.events("learn.gate_waiting"); len(evs) != 1 || !strings.Contains(evs[0].Message, c.want) {
			t.Fatalf("%s: waiting events = %+v", c.name, evs)
		}
		if !f.h.e.gateWaits() {
			t.Fatalf("%s: the stopped gate does not wait before its next try", c.name)
		}
	}
}

// A target whose replay ends without a score (herdr went away, a GitHub
// read failed, the judge timed out) stops the gate and leaves the proposal
// gating for a later try instead of holding it for good; only a third gate
// in a row that stops so holds it, saying why. A baseline's replay counts
// the same; a case that is no target is still left out of the verdict.
func TestATargetReplayWithoutAScoreLeavesTheProposalGating(t *testing.T) {
	f := newGateFixture(t, gateFullFiles)
	f.baseline("20261001-120000", "rev-master", map[string]*eval.Score{f.target.Name: found(nil, f.target.Name),
		f.other.Name: found([]string{"scope"})})
	f.ev.fail(f.wt, f.target.Name, "needs_attention")
	f.h.tick()
	if p, n := f.proposal(), len(f.ev.callsFor(f.wt)); p.State != store.LearnGating || n != 1 {
		t.Fatalf("proposal %s %q after %d candidate replays, want gating after one", p.State, p.Reason, n)
	}
	if evs := f.h.events("learn.gate_waiting"); len(evs) != 1 || !strings.Contains(evs[0].Message, f.target.Name) ||
		!strings.Contains(evs[0].Message, "needs_attention") || !f.h.e.gateWaits() {
		t.Fatalf("waiting events = %+v", evs)
	}
	for i := 2; i <= gateStopLimit; i++ {
		f.h.advance(gateRetryAfter + time.Minute)
		f.h.tick()
		if n := len(f.ev.callsFor(f.wt)); n != i {
			t.Fatalf("gate %d: %d candidate replays", i, n)
		}
	}
	p := f.proposal()
	if p.State != store.LearnHeld || !strings.Contains(p.Reason, fmt.Sprintf("%d gates", gateStopLimit)) || !strings.Contains(p.Reason, "needs_attention") {
		t.Fatalf("proposal = %s %q, want held after %d stopped gates", p.State, p.Reason, gateStopLimit)
	}

	// A replay that scores again before then is judged as usual.
	f2 := newGateFixture(t, gateFullFiles)
	f2.baseline("20261001-120000", "rev-master", map[string]*eval.Score{f2.target.Name: found(nil, f2.target.Name),
		f2.other.Name: found([]string{"scope"})})
	f2.ev.script(f2.wt, f2.target.Name, found([]string{f2.target.Name}))
	f2.ev.script(f2.wt, f2.other.Name, found([]string{"scope"}))
	f2.ev.fail(f2.wt, f2.target.Name, "error")
	f2.h.tick()
	f2.ev.fail(f2.wt, f2.target.Name, "")
	f2.h.advance(gateRetryAfter + time.Minute)
	f2.h.tick()
	if p := f2.proposal(); p.State != store.LearnPassed {
		t.Fatalf("proposal = %s %q once the target scored", p.State, p.Reason)
	}

	// The target's baseline replay without a score stops it too, before
	// any candidate replay.
	f3 := newGateFixture(t, gateFullFiles)
	f3.ev.fail(f3.base, f3.target.Name, "error")
	f3.h.tick()
	if p := f3.proposal(); p.State != store.LearnGating || len(f3.ev.callsFor(f3.wt)) != 0 {
		t.Fatalf("proposal = %s %q after a baseline without a score", p.State, p.Reason)
	}

	// A case that is no target and ends without a score is left out.
	f4 := newGateFixture(t, gateFullFiles)
	f4.baseline("20261001-120000", "rev-master", map[string]*eval.Score{f4.target.Name: found(nil, f4.target.Name),
		f4.other.Name: found([]string{"scope"})})
	f4.ev.script(f4.wt, f4.target.Name, found([]string{f4.target.Name}))
	f4.ev.fail(f4.wt, f4.other.Name, "error")
	f4.h.tick()
	if p := f4.proposal(); p.State != store.LearnPassed || f4.gate().Cases[1].Skip == "" {
		t.Fatalf("proposal = %s %q, gate %+v", p.State, p.Reason, f4.gate().Cases)
	}
}

// A gate whose verdict the registry did not take (busy, a disk error)
// stops short: it waits out its retry instead of replaying every case again
// at the next tick.
func TestAGateWhoseVerdictIsNotStoredWaitsBeforeItRunsAgain(t *testing.T) {
	f := newGateFixture(t, gateFullFiles)
	f.baseline("20261001-120000", "rev-master", map[string]*eval.Score{f.target.Name: found(nil, f.target.Name),
		f.other.Name: found([]string{"scope"})})
	f.ev.script(f.wt, f.target.Name, found([]string{f.target.Name}))
	f.ev.script(f.wt, f.other.Name, found([]string{"scope"}))
	if _, err := f.h.st.DB().ExecContext(f.h.ctx, `CREATE TRIGGER fail_decide BEFORE UPDATE OF state ON learn_proposals
BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	f.h.tick()
	if p, n := f.proposal(), len(f.ev.callsFor(f.wt)); p.State != store.LearnGating || n != 2 || !f.h.e.gateWaits() {
		t.Fatalf("proposal %s after %d candidate replays, waits %v", p.State, n, f.h.e.gateWaits())
	}
	f.h.tick()
	if n := len(f.ev.callsFor(f.wt)); n != 2 {
		t.Fatalf("the next tick replayed again: %d candidate replays", n)
	}
	if _, err := f.h.st.DB().ExecContext(f.h.ctx, "DROP TRIGGER fail_decide"); err != nil {
		t.Fatal(err)
	}
	f.h.advance(gateRetryAfter + time.Minute)
	f.h.tick()
	if p := f.proposal(); p.State != store.LearnPassed {
		t.Fatalf("proposal = %s %q after the retry", p.State, p.Reason)
	}
}

func TestAGatingProposalIsResumedAfterARestart(t *testing.T) {
	f := newGateFixture(t, gateFullFiles)
	f.baseline("20261001-120000", "rev-master", map[string]*eval.Score{f.target.Name: found(nil, f.target.Name),
		f.other.Name: found([]string{"scope"})})
	f.ev.script(f.wt, f.target.Name, found([]string{f.target.Name}))
	f.ev.script(f.wt, f.other.Name, found([]string{"scope"}))
	// A crashed daemon's mark: the gate was in flight.
	mark, _ := json.Marshal(GateMark{Proposal: f.p.ID, Started: f.h.clock.Now().Add(-time.Hour)})
	f.h.e.setKV(f.h.ctx, KVLearnGating, string(mark))
	f.h.startup()
	if _, ok := f.h.e.getKV(f.h.ctx, KVLearnGating); ok {
		t.Fatal("startup kept a crashed daemon's gate mark")
	}
	if p := f.proposal(); p.State != store.LearnGating {
		t.Fatalf("startup moved the proposal to %s", p.State)
	}
	f.h.tick()
	if p := f.proposal(); p.State != store.LearnPassed {
		t.Fatalf("proposal = %s %q", p.State, p.Reason)
	}
	// The worktree left behind is reused, not added again.
	if got := f.candidateGit("worktree"); len(got) != 0 {
		t.Fatalf("worktree commands = %q, want none", got)
	}

	// One that is gone is added again from the branch.
	f2 := newGateFixture(t, gateFullFiles)
	f2.baseline("20261001-120000", "rev-master", map[string]*eval.Score{f2.target.Name: found(nil, f2.target.Name),
		f2.other.Name: found([]string{"scope"})})
	f2.ev.script(f2.wt, f2.target.Name, found([]string{f2.target.Name}))
	f2.ev.script(f2.wt, f2.other.Name, found([]string{"scope"}))
	if err := os.RemoveAll(f2.wt); err != nil {
		t.Fatal(err)
	}
	f2.h.startup()
	f2.h.tick()
	want := []string{"worktree discard " + f2.wt, "worktree add " + f2.wt + " learn/" + gateRunID + " "}
	if got := f2.candidateGit("worktree"); !slices.Equal(got, want) || f2.proposal().State != store.LearnPassed {
		t.Fatalf("worktree commands = %q, want %q; proposal %s", got, want, f2.proposal().State)
	}
}

func TestAFailedProposalLosesItsWorktree(t *testing.T) {
	f := newGateFixture(t, gateFullFiles)
	f.baseline("20261001-120000", "rev-master", map[string]*eval.Score{f.target.Name: found(nil, f.target.Name),
		f.other.Name: found([]string{"scope"})})
	f.ev.script(f.wt, f.target.Name, found(nil, f.target.Name))
	f.ev.script(f.wt, f.other.Name, found([]string{"scope"}))
	f.h.tick()

	p := f.proposal()
	if p.State != store.LearnFailed || !strings.Contains(p.Reason, "no target improved") {
		t.Fatalf("proposal = %s %q, want failed: no target improved", p.State, p.Reason)
	}
	want := []string{"worktree discard " + f.wt, "branch delete learn/" + gateRunID}
	if got := f.candidateGit("worktree", "branch"); !slices.Equal(got, want) {
		t.Fatalf("git = %q, want %q", got, want)
	}
	if _, err := os.Stat(f.wt); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the worktree is still there: %v", err)
	}
	ins, err := f.h.st.LearnProposalInputs(f.h.ctx, p.ID)
	if err != nil || len(ins) != 1 || ins[0].State != store.MissNew {
		t.Fatalf("inputs = %+v, %v: a failed proposal keeps them new", ins, err)
	}
	// A proposal whose branch is gone fails at once.
	f2 := newGateFixture(t, gateFullFiles)
	if err := os.RemoveAll(f2.wt); err != nil {
		t.Fatal(err)
	}
	f2.git.mu.Lock()
	delete(f2.git.refs, f2.checkout+"|refs/heads/learn/"+gateRunID)
	f2.git.mu.Unlock()
	f2.h.tick()
	if p := f2.proposal(); p.State != store.LearnFailed || !strings.Contains(p.Reason, "is gone") || len(f2.ev.callsFor(f2.wt)) != 0 {
		t.Fatalf("proposal = %s %q", p.State, p.Reason)
	}
}

// A running gate is background work: a restart for a new build waits for
// it, and shutdown cancels it, leaving the proposal gating.
func TestTheGateIsBackgroundWorkForRestarts(t *testing.T) {
	f := newGateFixture(t, gateFullFiles)
	f.baseline("20261001-120000", "rev-master", map[string]*eval.Score{f.target.Name: found(nil, f.target.Name),
		f.other.Name: found([]string{"scope"})})
	f.ev.entered, f.ev.block = make(chan struct{}), make(chan struct{})
	entered := f.ev.entered
	if err := f.h.e.Tick(f.h.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the tick started no gate")
	}
	if busy := f.h.e.backgroundBusy(); busy != "the skill gate" {
		t.Fatalf("backgroundBusy = %q", busy)
	}
	if v, ok := f.h.e.getKV(f.h.ctx, KVLearnGating); !ok || !strings.Contains(v, fmt.Sprintf(`"proposal":%d`, f.p.ID)) {
		t.Fatalf("gate mark = %q", v)
	}
	f.h.e.stopGate()
	f.h.e.gateWG.Wait()
	if p := f.proposal(); p.State != store.LearnGating {
		t.Fatalf("a stopped gate moved its proposal to %s", p.State)
	}
	if _, ok := f.h.e.getKV(f.h.ctx, KVLearnGating); ok {
		t.Fatal("the gate's mark outlived it")
	}
}
