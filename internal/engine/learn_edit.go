package engine

// The skill editor (DECISIONS "The editor proposes skill and prompt changes
// on a branch"), the self-improving loop's third stage: an interactive agent
// (the retro's pane machinery, tagged "learn", in a herdr workspace named
// "learn skill") drafts a change to the judge skill and the judge and
// reviewer prompts from the general misses and the general findings authors
// declined, in a git worktree of magnum's checkout on a branch of its own.
// The run directory (<data>/learn/skill/<run>/) gives it inputs.json and
// pins.json (the sentences the skill must keep, with its cap), and it
// writes the files and changes.json in the worktree. magnum checks the
// answer (learn.ValidateEdit), renders the candidate's prompts and runs the
// format gate (the golden files regenerated, the agents, pipeline and
// config tests, `make check-private`), with one nudge for whatever fails;
// it then appends the decision to the worktree's docs/DECISIONS.md,
// commits on the branch and stores the proposal for the gate (gating), or
// stores it invalid or stopped and removes the worktree and the branch.
// The checkout's own branch and files are never touched; nothing is pushed.

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

const (
	// learnSubject is the audit subject of the self-improving loop.
	learnSubject = "learn"
	// learnBase is the checkout's branch an edit starts from and a
	// candidate lands on.
	learnBase = "master"
	// learnBranchPrefix names a candidate's branch: learn/<run>.
	learnBranchPrefix = "learn/"
	// editRunFormat names an editor run (local time), as a retro's.
	editRunFormat = retroRunFormat
	// editReasonRunes clips the problems stored with an invalid proposal.
	editReasonRunes = 2000
	// editRejections is how many rejected proposals' reasons the editor reads.
	editRejections = 5
	// editGateLines is how much of a failing format gate command's output
	// a problem carries.
	editGateLines = 40
	// FormatGateTimeout bounds one command of the format gate.
	FormatGateTimeout = 20 * time.Minute
)

// editSeverities are the misses the editor learns from, as the learned
// corpus replays them.
var editSeverities = []string{"P0", "P1", "P2"}

// EditRun is one editor run: its id, its run directory and its worktree.
type EditRun struct {
	ID, Dir, Worktree string
}

// EditJob is what an Editor works on: the run, the rendered prompt
// (prompts/skill-edit.md) and Check, which reads what the editor wrote and
// returns its changes and every problem with it (none: a candidate).
type EditJob struct {
	Run    EditRun
	Prompt string
	Check  func() (learn.Changes, []string)
}

// EditResult is how an editor's turns ended: its changes, valid when
// Problems is empty, or a Pause (a limit the agent hit).
type EditResult struct {
	Changes  learn.Changes
	Problems []string
	Pause    *pipeline.Pause
}

// Editor drafts the skill change of one run; Close ends it.
type Editor interface {
	Edit(ctx context.Context, job EditJob) (EditResult, error)
	Close(ctx context.Context) error
}

// ErrEditorDown wraps an Editor's error that is not the edit's: the agent
// could not be set up or started, or went away.
var ErrEditorDown = errors.New("engine: the skill editor is not available")

// LearnGit is the git work of an editor run (*gitx.Client).
type LearnGit interface {
	RevParse(ctx context.Context, dir, ref string) (string, error)
	WorktreeAdd(ctx context.Context, mainClone, path, ref string, detach bool, branch string) error
	WorktreeDiscard(ctx context.Context, repo, dir string) error
	StatusPaths(ctx context.Context, dir string) ([]gitx.StatusEntry, error)
	CommitAll(ctx context.Context, dir, message string, paths ...string) (string, error)
	BranchDelete(ctx context.Context, mainClone, branch string, force bool) error
}

var _ LearnGit = (*gitx.Client)(nil)

// EditMark is the editor run in flight (KVLearnEditing).
type EditMark struct {
	Run     string    `json:"run"`
	Trigger string    `json:"trigger"`
	Started time.Time `json:"started"`
}

// editPane is the skill editor's pane agent; it works in the run's worktree.
var editPane = paneSpec{what: "skill edit", workspace: "learn skill", owner: learnRepoOwner, name: config.EditorRoleName,
	role: (*config.Config).EditorRole}

// paneEditors is Deps.Editor for the pane agent: one agent per run.
func paneEditors(d paneDeps) func(context.Context, EditRun) (Editor, error) {
	d = d.withDefaults()
	return func(_ context.Context, run EditRun) (Editor, error) {
		return &paneEditor{paneAgent: &paneAgent{d: d, spec: editPane, dir: run.Dir, id: run.ID, cwd: run.Worktree}}, nil
	}
}

// paneEditor is one run's agent.
type paneEditor struct{ *paneAgent }

// Edit prompts the agent with job's prompt and waits for its turn to end; a
// result with problems gets one nudge listing them. A usage limit, a
// logout, a per-model limit or an overload is a Pause; an agent that cannot
// be set up or started, or went away, is ErrEditorDown.
func (c *paneEditor) Edit(ctx context.Context, job EditJob) (EditResult, error) {
	if err := c.setup(ctx); err != nil {
		return EditResult{}, fmt.Errorf("%w: set up: %w", ErrEditorDown, err)
	}
	if pause, err := c.ensureAgent(ctx); err != nil {
		if pause != nil || ctx.Err() != nil {
			return EditResult{Pause: pause}, err
		}
		return EditResult{}, fmt.Errorf("%w: start: %w", ErrEditorDown, err)
	}
	text := job.Prompt
	for attempt := 0; ; attempt++ {
		t := c.turn(ctx, attempt, text)
		ch, problems := job.Check()
		if len(problems) == 0 {
			return EditResult{Changes: ch}, nil
		}
		if pause := c.health(ctx, t, filepath.Join(job.Run.Worktree, learn.ChangesFile)); pause != nil {
			return EditResult{Pause: pause}, fmt.Errorf("the skill editor stopped: %s", pause.Kind)
		}
		switch {
		case ctx.Err() != nil:
			return EditResult{}, ctx.Err()
		case t.down:
			return EditResult{}, fmt.Errorf("%w: %w", ErrEditorDown, t.err)
		case attempt > 0:
			return EditResult{Changes: ch, Problems: problems}, nil
		}
		if t.err != nil {
			c.d.Logger.Warn("skill edit: the agent's turn did not end well; nudging", "run", c.id, "err", t.err)
		}
		text = editNudge(job, problems)
	}
}

// editNudge is the one reminder after a turn whose result has problems:
// the worktree's paths and magnum's own list of problems.
func editNudge(job EditJob, problems []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The edit in `%s` is not valid yet:\n", job.Run.Worktree)
	for _, p := range problems {
		fmt.Fprintf(&b, "- %s\n", strings.ReplaceAll(p, "\n", "\n  "))
	}
	fmt.Fprintf(&b, "Fix it as the first prompt describes, change only %s, and write `%s` again. Write each file to <file>.tmp "+
		"first, then move it into place, then stop.", strings.Join(learn.EditAllowedGlobs, ", "), filepath.Join(job.Run.Worktree, learn.ChangesFile))
	return b.String()
}

// editData feeds the editor's prompt (prompts/skill-edit.md): paths and the
// skill's cap, never input text.
type editData struct {
	Dir      string // the run directory
	Worktree string
	Inputs   string // inputs.json
	Pins     string // pins.json
	Changes  string // changes.json, in the worktree
	Skill    string // the worktree's SKILL.md
	Prompts  string // the worktree's prompts/
	Cap      int    // the skill's cap, bytes
}

// editPins is pins.json.
type editPins struct {
	Cap  int      `json:"cap"`
	Pins []string `json:"pins"`
}

// editBusy reports whether an editor run is in flight.
func (e *Engine) editBusy() bool {
	e.editMu.Lock()
	defer e.editMu.Unlock()
	return e.editCancel != nil
}

// startEdit runs an editor run in its own goroutine on a child of ctx (the
// daemon's: shutdown cancels it), unless the daemon is stopping or draining
// (false and a zero time) or one is running (false and when that one
// started). The run is KVLearnEditing while it runs.
func (e *Engine) startEdit(ctx context.Context, trigger string) (bool, time.Time) {
	if e.stopping(ctx) != "" {
		return false, time.Time{}
	}
	e.editMu.Lock()
	defer e.editMu.Unlock()
	if e.editCancel != nil {
		return false, e.editStarted
	}
	ectx, cancel := context.WithCancel(ctx)
	e.editCancel, e.editStarted = cancel, e.now()
	started := e.editStarted
	if b, err := json.Marshal(EditMark{Run: started.Local().Format(editRunFormat), Trigger: trigger, Started: started}); err == nil {
		e.setKV(ctx, KVLearnEditing, string(b))
	}
	e.editWG.Go(func() {
		defer func() {
			e.delKV(context.WithoutCancel(ctx), KVLearnEditing)
			e.editMu.Lock()
			e.editCancel = nil
			e.editMu.Unlock()
			cancel()
		}()
		e.safely(ectx, "skill edit", learnSubject, func() { _, _ = e.runEditAt(ectx, trigger, started) }, nil)
	})
	return true, started
}

// stopEdit cancels a running editor run (shutdown).
func (e *Engine) stopEdit() {
	e.editMu.Lock()
	defer e.editMu.Unlock()
	if e.editCancel != nil {
		e.editCancel()
	}
}

// editUnavailable is why no editor run can start now ("" = one can): no
// agent, no checkout, no git or no format gate.
func (e *Engine) editUnavailable() string {
	switch {
	case e.d.Editor == nil:
		return "no editor agent (herdr is not configured)"
	case e.d.Checkout == "":
		return "no checkout of magnum to edit (an installed binary has none)"
	case !gitx.IsRepo(e.d.Checkout):
		return e.d.Checkout + " is not a git checkout"
	case e.d.LearnGit == nil:
		return "no git client"
	case e.d.GoTest == nil:
		return "no format gate (go and mise in the checkout)"
	}
	return ""
}

// editInputs gathers inputs.json's inputs: the general misses of severity
// P0 to P2 and the general declines a lesson was not drawn from yet (state
// new), oldest first, their titles, lessons and match patterns scrubbed of
// their pull requests' logins and repositories again (learn.ScrubLesson;
// a pattern that names one is dropped), and the operator's reasons for the
// latest rejected proposals and for each input. scrub is what the edit's
// checks hold the result to: those logins and repositories, and the
// branches of those pull requests.
func (e *Engine) editInputs(ctx context.Context) (learn.EditInputs, learn.EditCheck, error) {
	in := learn.EditInputs{Misses: []learn.EditMiss{}, Declines: []learn.EditDecline{}, Rejections: []learn.EditRejection{},
		Files: slices.Clone(learn.EditAllowedGlobs)}
	var check learn.EditCheck
	for _, id := range e.cfg.Identities {
		check.Own = append(check.Own, id.Login)
	}
	misses, err := e.st.Misses(ctx, store.MissFilter{Classes: []string{store.MissMiss}, Scopes: []string{store.MissScopeGeneral},
		States: []string{store.MissNew}})
	if err != nil {
		return in, check, err
	}
	declines, err := e.st.Declines(ctx, store.DeclineFilter{Verdicts: []string{store.VerdictWrong, store.VerdictOverRanked},
		Scopes: []string{store.MissScopeGeneral}, States: []string{store.MissNew}})
	if err != nil {
		return in, check, err
	}
	prs := map[int64]store.PR{}
	people := func(prID int64, more ...string) []string {
		pr, ok := prs[prID]
		if !ok {
			pr, _ = e.st.PRByID(ctx, prID)
			prs[prID] = pr
			if b := deref(pr.HeadRef); b != "" {
				check.Leaks.Branches = append(check.Leaks.Branches, b)
			}
		}
		out := append([]string{deref(pr.AuthorLogin)}, more...)
		check.People = append(check.People, out...)
		return out
	}
	repoWords := func(full string) []string {
		owner, name, _ := strings.Cut(full, "/")
		check.Repos = append(check.Repos, owner, name)
		return []string{owner, name}
	}
	scrub := func(s string, who, repo []string) string {
		s, _ = learn.ScrubLesson(s, store.MissScopeGeneral, who, check.Own, repo)
		return s
	}
	scrubAll := func(ss []string, who, repo []string) []string {
		out := []string{}
		for _, s := range ss {
			if s = scrub(s, who, repo); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	for _, m := range slices.Backward(misses) { // Misses is newest first
		if !slices.Contains(editSeverities, m.Severity) {
			continue
		}
		who, repo := people(m.PRID, m.Reviewer), repoWords(m.Repo)
		in.Misses = append(in.Misses, learn.EditMiss{ID: learn.MissInputID(m.ID), Severity: m.Severity, Title: scrub(m.Title, who, repo),
			Lesson: scrub(m.Lesson, who, repo), Match: scrubAll(m.Match, who, repo)})
	}
	for _, d := range slices.Backward(declines) {
		who, repo := people(d.PRID), repoWords(d.Repo)
		in.Declines = append(in.Declines, learn.EditDecline{ID: learn.DeclineInputID(d.ID), Priority: d.Priority, Verdict: d.Verdict,
			Title: scrub(d.Title, who, repo), Lesson: scrub(d.Lesson, who, repo), Match: scrubAll(d.Match, who, repo)})
	}
	rejected, err := e.st.LearnProposals(ctx, []string{store.LearnRejected})
	if err != nil {
		return in, check, err
	}
	seen := map[learn.EditRejection]bool{}
	addRejection := func(r learn.EditRejection) {
		if r.Reason != "" && !seen[r] {
			seen[r] = true
			in.Rejections = append(in.Rejections, r)
		}
	}
	for _, p := range rejected[:min(len(rejected), editRejections)] {
		addRejection(learn.EditRejection{Reason: p.Reason})
	}
	for _, id := range in.IDs() {
		kind, n, _ := learn.ParseInputID(id)
		reasons, err := e.st.LearnInputRejections(ctx, kind, n)
		if err != nil {
			return in, check, err
		}
		for _, r := range reasons {
			addRejection(learn.EditRejection{Input: id, Reason: r})
		}
	}
	check.People, check.Repos = compactWords(check.People), compactWords(check.Repos)
	check.Leaks.Branches = compactWords(check.Leaks.Branches)
	return in, check, nil
}

// compactWords drops empty and repeated words.
func compactWords(ws []string) []string {
	out := slices.DeleteFunc(slices.Clone(ws), func(w string) bool { return strings.TrimSpace(w) == "" })
	slices.Sort(out)
	return slices.Compact(out)
}

// runEdit is one editor run: the worktree on a new branch from master, the
// inputs and the pins, the editor's turns (checked by editCheck, one nudge)
// and, for a valid edit, the design log's entry, the commit and the
// proposal waiting for the gate; an invalid edit is stored with its
// problems. A run that stops (no editor, a limit, a shutdown) stores a
// stopped proposal once it has a base and returns why. Either way only a
// gating proposal keeps its worktree and branch.
func (e *Engine) runEdit(ctx context.Context, trigger string) (store.LearnProposal, error) {
	return e.runEditAt(ctx, trigger, e.now())
}

// runEditAt is runEdit started at start (its run's id).
func (e *Engine) runEditAt(ctx context.Context, trigger string, start time.Time) (store.LearnProposal, error) {
	run := EditRun{ID: start.Local().Format(editRunFormat)}
	run.Dir = filepath.Join(e.d.Layout.LearnSkill(), run.ID)
	run.Worktree = filepath.Join(e.d.Layout.LearnWorktrees(), run.ID)
	branch := learnBranchPrefix + run.ID
	octx := context.WithoutCancel(ctx)
	e.event(ctx, "info", learnSubject, "learn.edit_start", fmt.Sprintf("skill edit %s started (%s)", run.ID, trigger),
		map[string]any{"run": run.ID, "trigger": trigger})
	model := e.cfg.RoleModel(e.cfg.EditorRole())
	base, promptSHA := "", ""
	added := false
	stop := func(why string) (store.LearnProposal, error) {
		if added {
			e.dropCandidate(octx, run.Worktree, branch)
		}
		var p store.LearnProposal
		if base != "" {
			var err error
			p, err = e.st.CreateLearnProposal(octx, store.LearnProposalInput{Trigger: trigger, BaseSHA: base, Branch: branch,
				State: store.LearnStopped, Reason: textx.Clip(why, editReasonRunes), Model: model, PromptSHA: promptSHA,
				Scratch: run.Worktree, RunDir: run.Dir, At: start})
			if err != nil {
				e.log.Warn("skill edit: store a stopped proposal", "run", run.ID, "err", err)
			}
		}
		e.event(octx, "warn", learnSubject, "learn.edit_stopped", "skill edit "+run.ID+" stopped: "+why,
			map[string]any{"run": run.ID, "proposal": p.ID})
		e.toastRequestedEdit(trigger, run, "stopped", why+".")
		return p, errors.New(why)
	}
	if why := e.editUnavailable(); why != "" {
		return stop(why)
	}
	git, checkout := e.d.LearnGit, e.d.Checkout
	e.dropOrphanCandidates(ctx)
	inputs, check, err := e.editInputs(ctx)
	if err != nil {
		return stop("the inputs: " + oneLine(err.Error(), retroWhyRunes))
	}
	if len(inputs.IDs()) == 0 {
		return stop("no new general miss or declined finding to learn from")
	}
	if base, err = git.RevParse(ctx, checkout, "refs/heads/"+learnBase); err != nil {
		return stop("the checkout's " + learnBase + ": " + oneLine(err.Error(), retroWhyRunes))
	}
	if err := os.MkdirAll(filepath.Dir(run.Worktree), 0o700); err != nil {
		return stop("the worktrees' directory: " + oneLine(err.Error(), retroWhyRunes))
	}
	if err := git.WorktreeAdd(ctx, checkout, run.Worktree, base, false, branch); err != nil {
		return stop("add the worktree: " + oneLine(err.Error(), retroWhyRunes))
	}
	added = true
	cand, err := e.prepareEdit(run, &inputs, &check)
	if err != nil {
		return stop(oneLine(err.Error(), retroWhyRunes))
	}
	prompt, sum, err := e.editPrompt(run, inputs.SkillCap)
	if err != nil {
		return stop(oneLine(err.Error(), retroWhyRunes))
	}
	promptSHA = sum

	ed, err := e.d.Editor(ctx, run)
	if err != nil {
		return stop("the editor cannot start: " + oneLine(err.Error(), retroWhyRunes))
	}
	defer func() {
		cctx, cancel := context.WithTimeout(octx, retroCloseTimeout)
		defer cancel()
		if err := ed.Close(cctx); err != nil {
			e.event(cctx, "warn", learnSubject, "learn.edit_close", "skill edit: closing the editor: "+oneLine(err.Error(), retroWhyRunes), nil)
		}
	}()
	res, err := ed.Edit(ctx, EditJob{Run: run, Prompt: prompt, Check: func() (learn.Changes, []string) {
		return e.editCheck(ctx, run, cand, inputs, check)
	}})
	switch {
	case res.Pause != nil:
		if res.Pause.Kind == string(agents.HealthUsageLimit) || res.Pause.Kind == string(agents.HealthLoginRequired) {
			e.pauseTool(octx, *res.Pause)
		}
		return stop(fmt.Sprintf("%s: %s", res.Pause.Tool, res.Pause.Kind))
	case ctx.Err() != nil:
		return stop("the daemon is stopping")
	case err != nil:
		return stop(oneLine(err.Error(), retroWhyRunes))
	}
	if len(res.Problems) > 0 {
		e.dropCandidate(octx, run.Worktree, branch)
		p, err := e.st.CreateLearnProposal(octx, store.LearnProposalInput{Trigger: trigger, BaseSHA: base, Branch: branch,
			State: store.LearnInvalid, Reason: textx.Clip(strings.Join(res.Problems, "; "), editReasonRunes), Model: model,
			PromptSHA: promptSHA, Scratch: run.Worktree, RunDir: run.Dir, At: start})
		if err != nil {
			e.log.Warn("skill edit: store an invalid proposal", "run", run.ID, "err", err)
		}
		e.event(octx, "warn", learnSubject, "learn.edit_invalid",
			fmt.Sprintf("skill edit %s: the edit is still invalid after a nudge (%s); kept as proposal %d", run.ID,
				textx.Count(len(res.Problems), "problem", "problems"), p.ID), map[string]any{"run": run.ID, "proposal": p.ID, "problems": len(res.Problems)})
		e.toastRequestedEdit(trigger, run, "is invalid", fmt.Sprintf("The edit is still invalid after a nudge (%s), so nothing goes to the gate; "+
			"`magnum learn` lists it.", textx.Count(len(res.Problems), "problem", "problems")))
		return p, nil
	}
	sha, files, err := e.commitEdit(octx, run, cand, inputs, res.Changes, start)
	if err != nil {
		return stop("commit the candidate: " + oneLine(err.Error(), retroWhyRunes))
	}
	in := store.LearnProposalInput{Trigger: trigger, BaseSHA: base, Branch: branch, CandidateSHA: sha, Files: files, State: store.LearnGating,
		Model: model, PromptSHA: promptSHA, Scratch: run.Worktree, RunDir: run.Dir, At: start}
	noted := 0
	for _, c := range res.Changes.Inputs {
		kind, id, _ := learn.ParseInputID(strings.TrimSpace(c.ID))
		in.Inputs = append(in.Inputs, store.LearnInput{Kind: kind, InputID: id, Outcome: c.Action, Where: strings.TrimSpace(c.Where),
			Reason: strings.TrimSpace(c.Reason)})
		if c.Action == learn.InputNoted {
			noted++
		}
	}
	p, err := e.st.CreateLearnProposal(octx, in)
	if err != nil {
		return stop("store the proposal: " + oneLine(err.Error(), retroWhyRunes))
	}
	e.event(octx, "info", learnSubject, "learn.edit_ready",
		fmt.Sprintf("skill edit %s is proposal %d on %s: %d of %s noted, %s changed", run.ID, p.ID, branch, noted,
			textx.Count(len(in.Inputs), "input", "inputs"), textx.Count(len(files), "file", "files")),
		map[string]any{"run": run.ID, "proposal": p.ID, "inputs": len(in.Inputs), "noted": noted, "files": len(files)})
	return p, nil
}

// editCandidate is what an editor run knows of its worktree before the
// editor starts: the allowed files' text at the base (by path), and the
// pins.
type editCandidate struct {
	orig map[string][]byte
	pins []string
}

// prepareEdit reads the worktree's pins and skill, fills the inputs' size
// and cap, keeps the allowed files' text at the base, and writes inputs.json
// and pins.json into the run directory.
func (e *Engine) prepareEdit(run EditRun, inputs *learn.EditInputs, check *learn.EditCheck) (editCandidate, error) {
	cand := editCandidate{orig: map[string][]byte{}}
	b, err := fsx.ReadRegular(filepath.Join(run.Worktree, filepath.FromSlash(learn.PinsSource)), 1<<20)
	if err != nil {
		return cand, fmt.Errorf("the skill's pins (%s): %w", learn.PinsSource, err)
	}
	pins, limit, err := learn.ParsePins(b)
	if err != nil {
		return cand, fmt.Errorf("the skill's pins (%s): %w", learn.PinsSource, err)
	}
	cand.pins, inputs.SkillCap, check.Pins = pins, limit, pins
	for _, g := range learn.EditAllowedGlobs {
		matches, err := filepath.Glob(filepath.Join(run.Worktree, filepath.FromSlash(g)))
		if err != nil {
			return cand, err
		}
		for _, m := range matches {
			rel, err := filepath.Rel(run.Worktree, m)
			if err != nil {
				return cand, err
			}
			body, err := fsx.ReadRegular(m, notes.MaxFileBytes)
			if err != nil {
				return cand, fmt.Errorf("%s: %w", rel, err)
			}
			cand.orig[filepath.ToSlash(rel)] = body
		}
	}
	skill, ok := cand.orig[learn.SkillFile]
	if !ok {
		return cand, fmt.Errorf("the worktree has no %s", learn.SkillFile)
	}
	inputs.SkillBytes = len(skill)
	if err := os.MkdirAll(run.Dir, 0o700); err != nil {
		return cand, err
	}
	for name, v := range map[string]any{learn.InputsFile: inputs, learn.PinsFile: editPins{Cap: limit, Pins: pins}} {
		body, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return cand, err
		}
		if err := fsx.WriteFileAtomic(filepath.Join(run.Dir, name), body, 0o600); err != nil {
			return cand, err
		}
	}
	return cand, nil
}

// editPrompt renders the editor's prompt for run, and the SHA-256 of its
// template (kept with the proposal).
func (e *Engine) editPrompt(run EditRun, limit int) (string, string, error) {
	p, err := e.cfg.ResolvePrompt(e.cfg.Learn.EditorPrompt)
	if err != nil {
		return "", "", fmt.Errorf("the editor's prompt cannot be read: %w", err)
	}
	text, err := agents.RenderPrompt(p, editData{Dir: run.Dir, Worktree: run.Worktree, Inputs: filepath.Join(run.Dir, learn.InputsFile),
		Pins: filepath.Join(run.Dir, learn.PinsFile), Changes: filepath.Join(run.Worktree, learn.ChangesFile),
		Skill: filepath.Join(run.Worktree, learn.SkillFile), Prompts: filepath.Join(run.Worktree, "prompts"), Cap: limit})
	if err != nil {
		return "", "", fmt.Errorf("the editor's prompt does not render: %w", err)
	}
	sum := sha256.Sum256([]byte(p.Text))
	return text, hex.EncodeToString(sum[:]), nil
}

// regenerated reports whether a status entry is a golden file the format
// gate's -update rewrote: a tracked file of internal/agents/testdata.
func regenerated(s gitx.StatusEntry) bool {
	return !s.Untracked() && s.X != 'A' && strings.HasPrefix(s.Path, "internal/agents/testdata/") && strings.HasSuffix(s.Path, ".golden")
}

// editChanged lists what the editor changed in the worktree: every status
// entry but changes.json and the golden files the format gate rewrites.
func (e *Engine) editChanged(ctx context.Context, wt string) ([]string, []string, error) {
	entries, err := e.d.LearnGit.StatusPaths(ctx, wt)
	if err != nil {
		return nil, nil, err
	}
	var changed, goldens []string
	for _, s := range entries {
		switch {
		case s.Path == learn.ChangesFile:
		case regenerated(s):
			goldens = append(goldens, s.Path)
		default:
			changed = append(changed, s.Path)
		}
	}
	slices.Sort(changed)
	slices.Sort(goldens)
	return changed, goldens, nil
}

// editCheck reads and checks what the editor wrote: changes.json, the
// changed files (learn.ValidateEdit against the inputs, the pins, the cap
// and the leak rules), then the candidate's prompts (CheckPrompts on the
// config pointed at the worktree), then the format gate. The first stage
// with problems returns them, for the nudge or the registry.
func (e *Engine) editCheck(ctx context.Context, run EditRun, cand editCandidate, inputs learn.EditInputs, check learn.EditCheck) (learn.Changes, []string) {
	wt := run.Worktree
	b, err := fsx.ReadRegular(filepath.Join(wt, learn.ChangesFile), learn.ChangesMaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		return learn.Changes{}, []string{learn.ChangesFile + " was not written"}
	}
	if err != nil {
		return learn.Changes{}, []string{learn.ChangesFile + " cannot be read: " + oneLine(err.Error(), retroWhyRunes)}
	}
	changes, err := learn.ParseChanges(b)
	if err != nil {
		return changes, []string{oneLine(err.Error(), retroWhyRunes)}
	}
	changed, _, err := e.editChanged(ctx, wt)
	if err != nil {
		return changes, []string{"the worktree's status: " + oneLine(err.Error(), retroWhyRunes)}
	}
	var problems []string
	check.Inputs, check.Changed = inputs, changed
	check.Skill = cand.orig[learn.SkillFile]
	var diff strings.Builder
	for _, p := range changed {
		if !learn.AllowedEditFile(p) {
			continue
		}
		body, err := fsx.ReadRegular(filepath.Join(wt, filepath.FromSlash(p)), notes.MaxFileBytes)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			problems = append(problems, fmt.Sprintf("%s cannot be read: %s", p, oneLine(err.Error(), retroWhyRunes)))
			continue
		}
		if p == learn.SkillFile {
			check.Skill = body
		}
		diff.WriteString(notes.Unified("a/"+p, "b/"+p, string(cand.orig[p]), string(body), 0))
	}
	check.Diff = diff.String()
	if problems = append(problems, learn.ValidateEdit(check, changes)...); len(problems) > 0 {
		return changes, problems
	}
	cfg, err := e.cfg.WithCandidate(wt)
	if err == nil {
		_, err = CheckPrompts(cfg)
	}
	if err != nil {
		return changes, []string{"the prompts do not render: " + oneLine(err.Error(), 600)}
	}
	for _, args := range formatGate {
		if out, err := e.d.GoTest(ctx, wt, args...); err != nil {
			return changes, []string{fmt.Sprintf("the format gate's `%s` failed (%s):\n%s", strings.Join(args, " "),
				oneLine(err.Error(), retroWhyRunes), lastLines(execx.Redact(out), editGateLines))}
		}
	}
	return changes, nil
}

// formatGate is what a candidate must pass in its worktree, in order: the
// golden files regenerated from its prompts, the tests that read the
// prompts and the skill, and the private-name guard (Deps.GoTest).
var formatGate = [][]string{
	{"go", "test", "./internal/agents", "-run", "TestRenderGolden", "-update"},
	{"go", "test", "./internal/agents", "./internal/pipeline", "./internal/config"},
	{"make", "check-private"},
}

// lastLines is the last n lines of s.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = append([]string{"..."}, lines[len(lines)-n:]...)
	}
	return strings.Join(lines, "\n")
}

// commitEdit keeps a copy of changes.json in the run directory and takes it
// out of the worktree, appends the decision to the worktree's design log
// and commits the editor's files, the regenerated goldens and the log on
// the branch: the decision's headline, the inputs it answers by id, the
// skill's byte accounting. It returns the commit and its files.
func (e *Engine) commitEdit(ctx context.Context, run EditRun, cand editCandidate, inputs learn.EditInputs, changes learn.Changes, start time.Time) (string, []string, error) {
	wt := run.Worktree
	answer := filepath.Join(wt, learn.ChangesFile)
	b, err := fsx.ReadRegular(answer, learn.ChangesMaxBytes)
	if err != nil {
		return "", nil, err
	}
	if err := fsx.WriteFileAtomic(filepath.Join(run.Dir, learn.ChangesFile), b, 0o600); err != nil {
		return "", nil, err
	}
	if err := os.Remove(answer); err != nil {
		return "", nil, err
	}
	changed, goldens, err := e.editChanged(ctx, wt)
	if err != nil {
		return "", nil, err
	}
	ids := make([]string, 0, len(changes.Inputs))
	var parts []string
	for _, c := range changes.Inputs {
		id := strings.TrimSpace(c.ID)
		ids = append(ids, id)
		parts = append(parts, id+" ("+c.Action+")")
	}
	logPath := filepath.Join(wt, filepath.FromSlash(learn.DecisionsFile))
	doc, err := fsx.ReadRegular(logPath, 64<<20)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", learn.DecisionsFile, err)
	}
	fi, err := os.Stat(logPath)
	if err != nil {
		return "", nil, err
	}
	entry := learn.DecisionEntry(changes.Decision, start.Local().Format("2006-01-02"), ids)
	if err := fsx.WriteFileAtomic(logPath, learn.AppendDecision(doc, entry), fi.Mode().Perm()); err != nil {
		return "", nil, err
	}
	files := slices.Concat(changed, goldens, []string{learn.DecisionsFile})
	slices.Sort(files)
	files = slices.Compact(files)
	skill, err := os.ReadFile(filepath.Join(wt, learn.SkillFile))
	if err != nil {
		return "", nil, err
	}
	head, _ := learn.Headline(changes.Decision)
	msg := fmt.Sprintf("%s\n\nThe skill editor's proposal (run %s) answers %s. SKILL.md %d to %d bytes (cap %d). Changed: %s. No new dependencies.\n",
		head, run.ID, strings.Join(parts, ", "), len(cand.orig[learn.SkillFile]), len(skill), inputs.SkillCap, strings.Join(files, ", "))
	sha, err := e.d.LearnGit.CommitAll(ctx, wt, msg, files...)
	return sha, files, err
}

// dropCandidate removes an edit's worktree and its branch; what fails is
// logged.
func (e *Engine) dropCandidate(ctx context.Context, wt, branch string) {
	if err := e.d.LearnGit.WorktreeDiscard(ctx, e.d.Checkout, wt); err != nil {
		e.log.Warn("skill edit: remove the worktree", "worktree", wt, "err", err)
	}
	if err := e.d.LearnGit.BranchDelete(ctx, e.d.Checkout, branch, true); err != nil {
		e.log.Warn("skill edit: delete the branch", "branch", branch, "err", err)
	}
}

// dropOrphanCandidates removes the worktrees (and their branches) of runs
// no waiting proposal holds: a run a crash cut short leaves one behind.
func (e *Engine) dropOrphanCandidates(ctx context.Context) {
	ents, err := os.ReadDir(e.d.Layout.LearnWorktrees())
	if err != nil {
		return
	}
	waiting, err := e.st.LearnProposals(ctx, []string{store.LearnEditing, store.LearnGating, store.LearnPassed, store.LearnHeld})
	if err != nil {
		return
	}
	for _, ent := range ents {
		wt := filepath.Join(e.d.Layout.LearnWorktrees(), ent.Name())
		if !ent.IsDir() || slices.ContainsFunc(waiting, func(p store.LearnProposal) bool { return p.Scratch == wt }) {
			continue
		}
		if _, err := time.ParseInLocation(editRunFormat, ent.Name(), time.Local); err != nil {
			continue
		}
		e.log.Warn("skill edit: removing a worktree no proposal holds", "worktree", wt)
		e.dropCandidate(ctx, wt, learnBranchPrefix+ent.Name())
	}
}

// toastRequestedEdit keeps the promise of a `magnum learn --propose` answer
// when no candidate comes: one toast for a requested run (trigger request)
// that stopped or ended invalid. A run the daemon started on its own is
// tried again, so only its event says so.
func (e *Engine) toastRequestedEdit(trigger string, run EditRun, what, body string) {
	if trigger != store.LearnTriggerRequest {
		return
	}
	title := "skill edit " + what
	e.info(notify.Item{Key: "learn-edit:" + run.ID, Title: title,
		Body: oneLine(body, 240) + " `magnum learn --propose` asks again.", Line: title})
}

// pruneEdits removes the editor's run directories older than retroKeep;
// the registry keeps every proposal.
func (e *Engine) pruneEdits() {
	if !e.d.Layout.Valid() {
		return
	}
	root := e.d.Layout.LearnSkill()
	ents, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := e.now().Add(-retroKeep)
	for _, ent := range ents {
		at, err := time.ParseInLocation(editRunFormat, ent.Name(), time.Local)
		if !ent.IsDir() || err != nil || !at.Before(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, ent.Name())); err != nil {
			e.log.Warn("skill edit: prune a run directory", "run", ent.Name(), "err", err)
		}
	}
}

// goTestRunner is Deps.GoTest for a checkout: args[0], go or make (both
// take -C), runs on dir under the checkout's mise environment (`mise -C
// <checkout> exec -- <tool> -C <dir> ...`: a worktree's own mise config is
// not trusted, and the checkout's gitignored .mise.local.toml holds what
// `make check-private` greps for), with none of the variables that point
// git elsewhere, the GitHub tokens or the identities' keys. It returns the
// command's output, stdout then stderr.
func goTestRunner(run execx.Runner, mise, checkout string, secrets []string) func(ctx context.Context, dir string, args ...string) (string, error) {
	bin := cmp.Or(mise, "mise")
	unset := slices.Concat(gitx.ScrubbedEnv(), slots.DefaultStripEnv, secrets)
	return func(ctx context.Context, dir string, args ...string) (string, error) {
		if len(args) == 0 {
			return "", errors.New("format gate: no command")
		}
		argv := append([]string{"-C", checkout, "exec", "--", args[0], "-C", dir}, args[1:]...)
		res, err := run.Run(ctx, execx.Cmd{Name: bin, Args: argv, Dir: dir, Unset: unset, Timeout: FormatGateTimeout, NoTTY: true,
			Label: "format gate: " + strings.Join(args, " ")})
		return string(res.Stdout) + string(res.Stderr), err
	}
}
