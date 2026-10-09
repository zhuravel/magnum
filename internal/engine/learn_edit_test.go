package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// The checkout an editor run works on: magnum's shape, small.
const (
	editSkillText = "# Skill\n\n## 4. Findings\nProve five facts.\nReachability decides the priority.\n"
	editDecisions = "# Decisions\n\n## Operations\n\n- **An old entry** (2026-10-01). It says why.\n"
	editGolden    = "internal/agents/testdata/judge_initial.golden"
	editAddedRule = "When two writers share a row, check the lock.\n"
	editDecision  = "The judge checks the lock of a row two writers share. A reviewer caught a lost update the review missed, " +
		"and the receiver rule already covers the declined finding."
)

// editFixture is a harness with a real git checkout of magnum's shape, a
// fake editor and a fake format gate, and a general miss and a general
// declined finding to learn from.
type editFixture struct {
	h        *harness
	ed       *fakeEditor
	gt       *fakeGoTest
	git      *gitx.Client
	run      *execx.Real
	checkout string
	master   string
	miss     store.Miss
	decline  store.Decline
}

// editGitRunner is a hermetic git environment: no user or system config,
// a fixed identity.
func editGitRunner(t *testing.T) *execx.Real {
	t.Helper()
	return &execx.Real{BaseEnv: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@example.com", "LC_ALL=C"}}
}

func (f *editFixture) gitOut(dir string, args ...string) string {
	f.h.t.Helper()
	res, err := f.run.Run(context.Background(), execx.Cmd{Name: "git", Args: append([]string{"-C", dir}, args...)})
	if err != nil {
		f.h.t.Fatalf("git %v: %v", args, err)
	}
	return res.Out()
}

func writeFile(t *testing.T, dir, rel, text string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newEditFixture(t *testing.T, turns ...func(f *editFixture, wt string)) *editFixture {
	t.Helper()
	f := &editFixture{run: editGitRunner(t)}
	if _, err := f.run.Run(context.Background(), execx.Cmd{Name: "git", Args: []string{"--version"}}); err != nil {
		t.Skipf("git not available: %v", err)
	}
	f.git = gitx.New(f.run)
	f.ed = &fakeEditor{f: f, turns: turns}
	f.gt = &fakeGoTest{}
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	f.checkout = filepath.Join(root, "magnum")
	f.h = newHarness(t, func(h *harness) {
		h.d.Editor = func(context.Context, EditRun) (Editor, error) { return f.ed, nil }
		h.d.GoTest = f.gt.run
		h.d.Checkout = f.checkout
		h.d.LearnGit = f.git
	})
	for rel, text := range map[string]string{
		learn.SkillFile: editSkillText, "prompts/judge-initial.md": "Review {{.URL}}.\n", "prompts/retro.md": "Classify.\n",
		learn.PinsSource:    string(learn.FormatPins([]string{"Prove five facts.", "Reachability decides the priority."}, 200)),
		learn.DecisionsFile: editDecisions, editGolden: "golden\n", "internal/engine/engine.go": "package engine\n",
	} {
		writeFile(t, f.checkout, rel, text)
	}
	f.gitOut(root, "init", "--quiet", "-b", "master", f.checkout)
	f.gitOut(f.checkout, "add", ".")
	f.gitOut(f.checkout, "commit", "--quiet", "-m", "c1")
	f.master = f.gitOut(f.checkout, "rev-parse", "master")

	pr := corpusPR(f.h, "talkable/talkable", 11)
	f.miss = corpusMiss(f.h, pr, 1, store.MissScopeGeneral)
	d, err := f.h.st.UpsertDecline(f.h.ctx, store.Decline{PRID: pr.ID, SourceURL: pr.URL + "#discussion_r5", CommentID: 5,
		Path: "lib/token.rb", Line: 30, Title: "Token can be nil", Priority: "P1", ReviewedSHA: retroShaA,
		Outcome: store.DeclineDeclined, Verdict: store.VerdictWrong, Lesson: "When the caller checks a value, do not ask for a guard.",
		Scope: store.MissScopeGeneral, Match: []string{"nil"}})
	if err != nil {
		t.Fatal(err)
	}
	f.decline = d
	return f
}

// ids are the fixture's inputs as inputs.json names them.
func (f *editFixture) ids() (string, string) {
	return learn.MissInputID(f.miss.ID), learn.DeclineInputID(f.decline.ID)
}

// changes writes changes.json into the worktree: the skill edited, the
// miss noted and the decline skipped, unless extra files say otherwise.
func (f *editFixture) changes(wt string, files ...learn.FileChange) {
	m, d := f.ids()
	ch := learn.Changes{
		Files:    append([]learn.FileChange{{Name: learn.SkillFile, Action: learn.FileEdited, Reason: "a rule for shared rows"}}, files...),
		Inputs:   []learn.InputChange{{ID: m, Action: learn.InputNoted, Where: "SKILL.md section 4"}, {ID: d, Action: learn.InputSkipped, Reason: "the receiver rule covers it"}},
		Decision: editDecision,
	}
	b, err := json.Marshal(ch)
	if err != nil {
		f.h.t.Fatal(err)
	}
	writeFile(f.h.t, wt, learn.ChangesFile, string(b))
}

// goodTurn adds a rule to the skill and accounts for every input.
func goodTurn(f *editFixture, wt string) {
	writeFile(f.h.t, wt, learn.SkillFile, editSkillText+editAddedRule)
	f.changes(wt)
}

// lostPinTurn rewrites a pinned sentence.
func lostPinTurn(f *editFixture, wt string) {
	writeFile(f.h.t, wt, learn.SkillFile, strings.Replace(editSkillText, "Reachability decides the priority.", "Reach decides.", 1)+editAddedRule)
	f.changes(wt)
}

// fakeEditor writes into the worktree the way the editor agent would, one
// turn function per turn (the last repeats), then checks the result as the
// pane editor does: a valid answer at once, an invalid one after one nudge.
type fakeEditor struct {
	f     *editFixture
	turns []func(f *editFixture, wt string)

	mu      sync.Mutex
	prompts []string
	nudges  []string
	closed  int
	entered chan struct{} // closed when Edit starts (block)
	block   chan struct{} // when set, Edit waits for it or its context
}

func (e *fakeEditor) Edit(ctx context.Context, job EditJob) (EditResult, error) {
	e.mu.Lock()
	e.prompts = append(e.prompts, job.Prompt)
	block, entered := e.block, e.entered
	e.mu.Unlock()
	if entered != nil {
		close(entered)
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return EditResult{}, ctx.Err()
		}
	}
	for turn := 0; ; turn++ {
		write := e.turns[min(turn, len(e.turns)-1)]
		write(e.f, job.Run.Worktree)
		ch, problems := job.Check()
		if len(problems) == 0 {
			return EditResult{Changes: ch}, nil
		}
		if turn > 0 {
			return EditResult{Changes: ch, Problems: problems}, nil
		}
		e.mu.Lock()
		e.nudges = append(e.nudges, editNudge(job, problems))
		e.mu.Unlock()
	}
}

func (e *fakeEditor) Close(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed++
	return nil
}

// fakeGoTest is the format gate's subprocess: it records each call, as
// "<dir>: <args>", regenerates a golden file on -update, and fails the
// calls whose args hold a key of fail with its output.
type fakeGoTest struct {
	mu    sync.Mutex
	calls []string
	fail  map[string]string
}

func (g *fakeGoTest) run(_ context.Context, dir string, args ...string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	call := strings.Join(args, " ")
	g.calls = append(g.calls, dir+": "+call)
	if slices.Contains(args, "-update") {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(editGolden)), []byte("golden, regenerated\n"), 0o644); err != nil {
			return "", err
		}
	}
	for k, out := range g.fail {
		if strings.Contains(call, k) {
			return out, errors.New("exit status 1")
		}
	}
	return "ok\n", nil
}

func (g *fakeGoTest) all() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.calls)
}

// edit runs the editor as `magnum learn --propose` asks.
func (f *editFixture) edit() store.LearnProposal {
	f.h.t.Helper()
	p, err := f.h.e.runEdit(f.h.ctx, store.LearnTriggerRequest)
	if err != nil {
		f.h.t.Fatalf("runEdit: %v", err)
	}
	return p
}

func (f *editFixture) branchExists(branch string) bool {
	_, err := f.git.RevParse(f.h.ctx, f.checkout, "refs/heads/"+branch)
	if err != nil && !errors.Is(err, gitx.ErrNoSuchRef) {
		f.h.t.Fatal(err)
	}
	return err == nil
}

func (f *editFixture) toasts() string {
	f.h.t.Helper()
	if _, err := f.h.e.batch.Flush(f.h.ctx); err != nil {
		f.h.t.Fatal(err)
	}
	return strings.Join(f.h.nh.all(), "\n")
}

func TestAnEditRunProposesACandidateOnItsOwnBranch(t *testing.T) {
	f := newEditFixture(t, goodTurn)
	p := f.edit()
	run := "20261005-100000"
	wt := filepath.Join(f.h.layout.LearnWorktrees(), run)
	if p.State != store.LearnGating || p.Branch != "learn/"+run || p.BaseSHA != f.master || p.Trigger != store.LearnTriggerRequest ||
		p.Scratch != wt || p.RunDir != filepath.Join(f.h.layout.LearnSkill(), run) || p.Model != "sonnet" || p.PromptSHA256 == "" {
		t.Fatalf("proposal = %+v", p)
	}
	if got := f.gitOut(f.checkout, "rev-parse", p.Branch); got != p.CandidateSHA || got == f.master {
		t.Fatalf("branch %s = %s, candidate %s, master %s", p.Branch, got, p.CandidateSHA, f.master)
	}
	if got := f.gitOut(f.checkout, "rev-parse", "master"); got != f.master {
		t.Fatalf("master moved to %s", got)
	}
	if b, _ := os.ReadFile(filepath.Join(f.checkout, learn.SkillFile)); string(b) != editSkillText {
		t.Fatalf("the checkout's skill changed: %q", b)
	}
	if want := []string{learn.DecisionsFile, editGolden, learn.SkillFile}; !slices.Equal(p.Files, want) {
		t.Fatalf("files = %q, want %q", p.Files, want)
	}
	if got := f.gitOut(f.checkout, "show", "--name-only", "--format=", p.CandidateSHA); got != strings.Join(p.Files, "\n") {
		t.Fatalf("the commit changes %q", got)
	}
	m, d := f.ids()
	ins, err := f.h.st.LearnProposalInputs(f.h.ctx, p.ID)
	if err != nil || len(ins) != 2 || ins[0].Kind != store.LearnInputDecline || ins[0].InputID != f.decline.ID || ins[0].Outcome != store.MissSkipped ||
		ins[1].Kind != store.LearnInputMiss || ins[1].InputID != f.miss.ID || ins[1].Outcome != store.MissNoted || ins[1].Where != "SKILL.md section 4" {
		t.Fatalf("inputs = %+v, %v", ins, err)
	}

	// The run directory holds the inputs, scrubbed of logins, the pins and
	// a copy of the answer; the worktree keeps none of them.
	var in learn.EditInputs
	b, err := os.ReadFile(filepath.Join(p.RunDir, learn.InputsFile))
	if err != nil || json.Unmarshal(b, &in) != nil {
		t.Fatalf("inputs.json: %v", err)
	}
	if len(in.Misses) != 1 || in.Misses[0].ID != m || in.Misses[0].Severity != "P1" || len(in.Declines) != 1 || in.Declines[0].ID != d ||
		in.Declines[0].Verdict != store.VerdictWrong || in.SkillCap != 200 || in.SkillBytes != len(editSkillText) || !slices.Equal(in.Files, learn.EditAllowedGlobs) {
		t.Fatalf("inputs.json = %+v", in)
	}
	for _, login := range []string{"alice", "rev-ann"} {
		if strings.Contains(string(b), login) {
			t.Errorf("inputs.json names %s", login)
		}
	}
	var pins struct {
		Cap  int      `json:"cap"`
		Pins []string `json:"pins"`
	}
	if b, err := os.ReadFile(filepath.Join(p.RunDir, learn.PinsFile)); err != nil || json.Unmarshal(b, &pins) != nil || pins.Cap != 200 || len(pins.Pins) != 2 {
		t.Fatalf("pins.json = %+v, %v", pins, err)
	}
	if _, err := os.Stat(filepath.Join(p.RunDir, learn.ChangesFile)); err != nil {
		t.Fatalf("no copy of changes.json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, learn.ChangesFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changes.json is still in the worktree: %v", err)
	}
	if prompts := f.ed.prompts; len(prompts) != 1 || !strings.Contains(prompts[0], filepath.Join(p.RunDir, learn.InputsFile)) ||
		!strings.Contains(prompts[0], filepath.Join(p.RunDir, learn.PinsFile)) || !strings.Contains(prompts[0], filepath.Join(wt, learn.ChangesFile)) ||
		!strings.Contains(prompts[0], "200 bytes") {
		t.Fatalf("prompt = %q", f.ed.prompts)
	}
	if evs := f.h.events("learn.edit_start"); len(evs) != 1 {
		t.Fatalf("edit_start events = %+v", evs)
	}
	if f.ed.closed != 1 || len(f.ed.nudges) != 0 {
		t.Fatalf("editor closed %d times, nudged %d times", f.ed.closed, len(f.ed.nudges))
	}
}

func TestAnEditThatLosesAPinGetsOneNudgeThenIsInvalid(t *testing.T) {
	f := newEditFixture(t, lostPinTurn)
	p := f.edit()
	if p.State != store.LearnInvalid || !strings.Contains(p.Reason, "lost a pinned sentence") || p.CandidateSHA != "" {
		t.Fatalf("proposal = %+v", p)
	}
	if len(f.ed.nudges) != 1 || !strings.Contains(f.ed.nudges[0], `lost a pinned sentence: "Reachability decides the priority."`) ||
		!strings.Contains(f.ed.nudges[0], learn.ChangesFile) {
		t.Fatalf("nudges = %q", f.ed.nudges)
	}
	if calls := f.gt.all(); len(calls) != 0 {
		t.Fatalf("the format gate ran on an invalid edit: %q", calls)
	}
	if _, err := os.Stat(p.Scratch); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the worktree of an invalid edit stays: %v", err)
	}
	if f.branchExists(p.Branch) {
		t.Fatalf("the branch %s of an invalid edit stays", p.Branch)
	}
	if ins, _ := f.h.st.LearnProposalInputs(f.h.ctx, p.ID); len(ins) != 0 {
		t.Fatalf("an invalid proposal linked inputs: %+v", ins)
	}
	if evs := f.h.events("learn.edit_invalid"); len(evs) != 1 {
		t.Fatalf("edit_invalid events = %+v", evs)
	}
	if got := f.toasts(); !strings.Contains(got, "skill edit is invalid") {
		t.Fatalf("toasts = %q", got)
	}

	// A nudged turn that keeps the pin makes a candidate.
	f2 := newEditFixture(t, lostPinTurn, goodTurn)
	if p := f2.edit(); p.State != store.LearnGating || len(f2.ed.nudges) != 1 {
		t.Fatalf("after a good nudged turn: %+v, %d nudges", p, len(f2.ed.nudges))
	}
}

func TestAnEditThatTouchesCodeIsInvalid(t *testing.T) {
	f := newEditFixture(t, func(f *editFixture, wt string) {
		writeFile(t, wt, learn.SkillFile, editSkillText+editAddedRule)
		writeFile(t, wt, "internal/engine/engine.go", "package engine\n\nconst x = 1\n")
		writeFile(t, wt, "prompts/retro.md", "Classify, and more.\n")
		f.changes(wt, learn.FileChange{Name: "internal/engine/engine.go", Action: learn.FileEdited, Reason: "needed"})
	})
	p := f.edit()
	if p.State != store.LearnInvalid || !strings.Contains(p.Reason, "internal/engine/engine.go changed, but only") ||
		!strings.Contains(p.Reason, "prompts/retro.md changed, but only") {
		t.Fatalf("proposal = %+v", p)
	}
	if got := f.gitOut(f.checkout, "rev-parse", "master"); got != f.master {
		t.Fatalf("master moved to %s", got)
	}
}

func TestTheFormatGateRunsGoldensThenTheTestsThenCheckPrivate(t *testing.T) {
	f := newEditFixture(t, goodTurn)
	p := f.edit()
	wt := p.Scratch
	want := []string{
		wt + ": go test ./internal/agents -run TestRenderGolden -update",
		wt + ": go test ./internal/agents ./internal/pipeline ./internal/config",
		wt + ": make check-private",
	}
	if got := f.gt.all(); !slices.Equal(got, want) {
		t.Fatalf("format gate calls = %q\nwant %q", got, want)
	}
	if got := f.gitOut(f.checkout, "show", p.CandidateSHA+":"+editGolden); got != "golden, regenerated" {
		t.Fatalf("the committed golden = %q", got)
	}

	// A gate that fails twice makes the edit invalid; the nudge carries the
	// failing command and its output.
	f2 := newEditFixture(t, goodTurn)
	f2.gt.fail = map[string]string{"check-private": "tracked files mention a private name:\nREADME.md:1: x\n"}
	p2 := f2.edit()
	if p2.State != store.LearnInvalid || !strings.Contains(p2.Reason, "make check-private") {
		t.Fatalf("proposal = %+v", p2)
	}
	if len(f2.ed.nudges) != 1 || !strings.Contains(f2.ed.nudges[0], "make check-private") ||
		!strings.Contains(f2.ed.nudges[0], "tracked files mention a private name") {
		t.Fatalf("nudges = %q", f2.ed.nudges)
	}
	if calls := f2.gt.all(); len(calls) != 6 {
		t.Fatalf("the gate ran %d commands over two turns, want 6: %q", len(calls), calls)
	}
}

func TestTheEditRunAppendsTheDecisionsEntryAndCommits(t *testing.T) {
	f := newEditFixture(t, goodTurn)
	p := f.edit()
	m, d := f.ids()
	doc := f.gitOut(f.checkout, "show", p.CandidateSHA+":"+learn.DecisionsFile)
	entry, ok := strings.CutPrefix(doc+"\n", editDecisions)
	if !ok {
		t.Fatalf("the design log lost its text:\n%s", doc)
	}
	if !strings.HasPrefix(strings.ReplaceAll(entry, "\n  ", " "), "- **The judge checks the lock of a row two writers share** (2026-10-05, "+
		"the skill editor's proposal for "+m+", "+d+"). A reviewer caught") {
		t.Fatalf("entry = %q", entry)
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(entry, "\n"), "\n") {
		if n := utf8.RuneCountInString(line); n > 104 || (!strings.HasPrefix(line, "- ") && !strings.HasPrefix(line, "  ")) {
			t.Errorf("entry line of %d runes: %q", n, line)
		}
	}
	msg := f.gitOut(f.checkout, "log", "-1", "--format=%B", p.CandidateSHA)
	subject, body, _ := strings.Cut(msg, "\n\n")
	if subject != "The judge checks the lock of a row two writers share." {
		t.Fatalf("subject = %q", subject)
	}
	for _, want := range []string{m + " (noted)", d + " (skipped)", "SKILL.md " + itoa(int64(len(editSkillText))) + " to " +
		itoa(int64(len(editSkillText+editAddedRule))) + " bytes (cap 200)", "No new dependencies."} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(msg, "receiver rule covers it") {
		t.Errorf("the commit message carries the editor's reasons:\n%s", msg)
	}
}

func TestAnEditRunIsBackgroundWorkForRestarts(t *testing.T) {
	f := newEditFixture(t, goodTurn)
	f.ed.block, f.ed.entered = make(chan struct{}), make(chan struct{})
	if started, _ := f.h.e.startEdit(f.h.ctx, store.LearnTriggerRequest); !started {
		t.Fatal("the edit did not start")
	}
	<-f.ed.entered
	if busy := f.h.e.backgroundBusy(); !strings.Contains(busy, "the skill edit") {
		t.Fatalf("background work = %q", busy)
	}
	var mark EditMark
	if v, ok := f.h.e.getKV(f.h.ctx, KVLearnEditing); !ok || json.Unmarshal([]byte(v), &mark) != nil || mark.Run != "20261005-100000" ||
		mark.Trigger != store.LearnTriggerRequest || !mark.Started.Equal(f.h.clock.Now()) {
		t.Fatalf("mark = %+v (%v)", mark, ok)
	}
	if started, since := f.h.e.startEdit(f.h.ctx, store.LearnTriggerRequest); started || !since.Equal(f.h.clock.Now()) {
		t.Fatalf("a second edit started: %v, %v", started, since)
	}
	close(f.ed.block)
	f.h.settle()
	if busy := f.h.e.backgroundBusy(); busy != "" {
		t.Fatalf("background work after the edit = %q", busy)
	}
	if _, ok := f.h.e.getKV(f.h.ctx, KVLearnEditing); ok {
		t.Fatal("the mark stays after the edit")
	}
	if n, _ := f.h.st.CountLearnProposals(f.h.ctx, store.LearnGating); n != 1 {
		t.Fatalf("gating proposals = %d", n)
	}

	// A shutdown stops a running edit: nothing is proposed, its worktree
	// and branch go, and the proposal says it stopped.
	f.h.advance(time.Minute)
	f.ed.mu.Lock()
	f.ed.block, f.ed.entered = make(chan struct{}), make(chan struct{})
	f.ed.mu.Unlock()
	if started, _ := f.h.e.startEdit(f.h.ctx, store.LearnTriggerRequest); !started {
		t.Fatal("the second edit did not start")
	}
	<-f.ed.entered
	f.h.e.stopEdit()
	f.h.settle()
	ps, err := f.h.st.LearnProposals(f.h.ctx, []string{store.LearnStopped})
	if err != nil || len(ps) != 1 || !strings.Contains(ps[0].Reason, "the daemon is stopping") {
		t.Fatalf("stopped proposals = %+v, %v", ps, err)
	}
	if _, err := os.Stat(ps[0].Scratch); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stopped edit's worktree stays: %v", err)
	}
	if f.branchExists(ps[0].Branch) {
		t.Fatalf("the stopped edit's branch stays")
	}
	if evs := f.h.events("learn.edit_stopped"); len(evs) != 1 {
		t.Fatalf("edit_stopped events = %+v", evs)
	}
}

// The editor is a pane agent like the curator: its own tagged agent in a
// "learn skill" workspace, working in the run's worktree, prompted with
// paths only. A first answer with problems gets one nudge naming them; the
// corrected edit is committed for the gate, and the agent is parked.
func TestThePaneEditorWorksInTheWorktreeAndNudgesOnce(t *testing.T) {
	f := newEditFixture(t, goodTurn)
	ag := &fakePaneAgents{}
	f.h.e.d.Editor = paneEditors(paneDeps{
		Config: f.h.cfg, Layout: f.h.layout, Logger: f.h.d.Logger, Herdr: f.h.hd, Now: f.h.clock.Now,
		Sleep:        func(ctx context.Context, _ time.Duration) error { return sleepCtx(ctx, time.Millisecond) },
		ObserveEvery: time.Millisecond, Poll: time.Millisecond, CloseWorkspace: f.h.hd.WorkspaceClose,
		Agents: func(st *store.Store, cfg *config.Config, layout paths.Layout) paneAgents {
			if len(cfg.Roles) != 1 || cfg.Roles[0].Name != config.EditorRoleName {
				t.Errorf("the manager's roles = %v, want the editor's alone", cfg.Roles)
			}
			ag.fakeAgents = &fakeAgents{st: st}
			ag.scratch = layout.Scratch
			return ag
		},
	})
	ag.reply = func(n int, _ string) bool {
		wts, _ := filepath.Glob(filepath.Join(f.h.layout.LearnWorktrees(), "2*"))
		wt := wts[len(wts)-1]
		if n == 1 { // the first answer loses a pin
			lostPinTurn(f, wt)
		} else {
			goodTurn(f, wt)
		}
		return true
	}
	p := f.edit()
	if p.State != store.LearnGating {
		t.Fatalf("proposal = %+v", p)
	}
	sent := ag.sent()
	if len(sent) != 2 || !strings.Contains(sent[1], "lost a pinned sentence") || !strings.Contains(sent[0], p.Scratch) {
		t.Fatalf("prompts = %q", sent)
	}
	calls := ag.all()
	ws := slices.DeleteFunc(slices.Clone(calls), func(c string) bool { return !strings.HasPrefix(c, "ensure_workspace:") })
	if len(ws) != 1 || !strings.Contains(ws[0], ":"+p.Scratch+":learn skill:") || !strings.HasSuffix(ws[0], ":skill") {
		t.Fatalf("agent setup = %v", calls)
	}
	if ag.count("park:") != 1 {
		t.Errorf("park calls: %v", calls)
	}
}

// The real format gate runs each command on the worktree under the
// checkout's mise environment (the worktree's own mise config is not
// trusted, and the checkout's .mise.local.toml holds the private patterns),
// without the variables that point git elsewhere, the GitHub tokens or the
// identities' keys, and hands back the output of a failure.
func TestTheFormatGateRunsUnderTheCheckoutsMise(t *testing.T) {
	run := &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"mise", "-C", "/src/magnum", "exec", "--", "make"}, Result: execx.Result{Code: 2, Stdout: []byte("out\n"), Stderr: []byte("err\n")}},
		{Prefix: []string{"mise"}, Result: execx.Result{Stdout: []byte("ok\n")}},
	}}
	gate := goTestRunner(run, "", "/src/magnum", []string{"MAGNUM_APP_KEY"})
	if out, err := gate(t.Context(), "/wt/1", "go", "test", "./internal/agents"); err != nil || out != "ok\n" {
		t.Fatalf("go test = %q, %v", out, err)
	}
	if out, err := gate(t.Context(), "/wt/1", "make", "check-private"); err == nil || out != "out\nerr\n" {
		t.Fatalf("make = %q, %v", out, err)
	}
	if len(run.Calls) != 2 {
		t.Fatalf("calls = %+v", run.Calls)
	}
	c := run.Calls[0]
	if got := strings.Join(append([]string{c.Name}, c.Args...), " "); got != "mise -C /src/magnum exec -- go -C /wt/1 test ./internal/agents" || c.Dir != "/wt/1" {
		t.Fatalf("command = %q in %q", got, c.Dir)
	}
	for _, k := range []string{"GIT_DIR", "GH_TOKEN", "GITHUB_TOKEN", "MAGNUM_APP_KEY"} {
		if !slices.Contains(c.Unset, k) {
			t.Errorf("%s is not unset: %q", k, c.Unset)
		}
	}
	if c.Timeout != FormatGateTimeout || !c.NoTTY {
		t.Errorf("timeout %s, no tty %v", c.Timeout, c.NoTTY)
	}
}
