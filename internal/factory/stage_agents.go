package factory

// The agents stages (spec 6.2, 6.3 and 6.8): a stage claims its task's
// copy, launches its roles in parallel in one workspace, sends each its
// prompt (paths, keys and commands: the ticket, the guidance and the
// answers are files the agents read as data), and passes when its end fact
// holds (CheckEnd), never on an agent's "done". A role whose run ends
// without a valid file gets one nudge, then the task waits on you, naming
// the role and the file. The runtime's events that need you (a dialog the
// factory does not answer, a pane lost after its recoveries, a turn stuck
// or a loop without progress after the watchdog's steps, Codex's hooks
// review, a refusal) move the task to NEEDS YOU with a plain reason; a
// pause is a wait, and the role goes on after it. On a Codex refusal the
// task is Codex-never (the engine's observe pass marks it): its Codex
// sessions are quit and its Codex roles run again as their Claude fallback
// (Runtime.RunsAs), writing the files the stage names them for.
//
// What a stage knows of its attempt lives in memory (agentsRun) and is
// rebuilt after a restart: the ledger says which role got its prompt and
// its nudge (no prompt goes out twice), the role runs which one the runtime
// verified, and each role's last run in the agents store what the lost
// events said (Runtime.RunState): an end not yet checked, an end whose
// nudge or wait on you was still to come, or a pause whose role goes on
// once it is over.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/store"
)

// PromptData is what a factory prompt (factory-*.md, with the blocks of
// factory-shared.md) renders with: paths, keys and commands, never the
// ticket's text, which reaches the agents only as the task's files (spec
// 12). Key is the Jira key ("" for a task of a free prompt), Kind and Repo
// the task's, Branch its local branch, Copy the checkout of its copy and
// TaskDir its directory. Ticket (ticket.md), Guidance (guidance.md), Prompt
// (prompt.md, the operator's request), Answers (answers.md) and Rules
// (rules.md, the learned rules) are paths in TaskDir, each "" while its
// file is not there. Findings is the role's findings-<role>.md in a
// findings stage, else the pattern of every investigator's file
// (findings-*.md); Questions the file the role writes its questions to
// (questions.json in the judge's report stage, questions-<role>.json for
// any other role); Report, Acceptance and Proof the judge's report.md and
// acceptance.json and the writer's proof.json; Review the local review's
// findings for the writer ("" before that stage). ErrorSkill is the
// repository's error_skill ("" = none: no prompt names one), DBLockCommand
// the `magnum db-lock` line of the copy for the role as it runs, and Port
// the copy's server's (0 when its env names none).
type PromptData struct {
	TaskID                                                 int64
	Key, Kind, Repo, Branch, Copy, TaskDir                 string
	Ticket, Guidance, Prompt, Answers, Rules               string
	Findings, Report, Questions, Acceptance, Proof, Review string
	ErrorSkill, DBLockCommand                              string
	Port                                                   int
}

// agentsRun is what the loop knows of a task's agents stage attempt.
type agentsRun struct {
	attempt string
	roles   []*stageRole
	// quitCodex: a refusal left the task's Codex sessions to quit
	// (Runtime.QuitCodex), tried again each pass until it went through.
	quitCodex bool
}

// stageRole is a role of a stage's attempt: the role as the stage names it
// (its files are named for it) and as it runs (its Claude fallback on a
// task Codex never works on), and how far it got.
type stageRole struct {
	role, runAs config.Role
	// prompted: its prompt went out; runs are the runs its prompts made in
	// the attempt (an end of another is no end of this role's).
	prompted bool
	runs     map[string]bool
	// settled is a run the store has as failed already (a restart's
	// rebuild, lastRunEvent): its end is checked again, never verified
	// twice.
	settled string
	// next is the prompt it gets next, once Send may: a nudge for its end
	// fact (problem says why), or the one to go on after a pause (pause
	// says which); nudged: its one nudge went out.
	next    string
	problem *endProblem
	pause   string
	nudged  bool
	// relaunch: it runs again as its fallback after a Codex refusal (a
	// launch, then its prompt).
	relaunch bool
	// ok: its end fact held at its run's end; failed: it did not after the
	// nudge (the file), and the task waits on you.
	ok     bool
	failed string
}

// The prompts a role gets after its first (stageRole.next).
const (
	nextNudge    = "nudge"
	nextContinue = "continue"
)

// nudgeLead starts a role's one nudge for its end fact: after a restart
// the ledger tells a nudge from the other prompts by it.
const nudgeLead = "The factory checked your output after your turn"

// waitAgent is the WaitsWho of a task whose prompt waits on an agent's
// pane (you are in it, its last prompt's delivery is unknown).
const waitAgent = "agent"

// agentsEnds are the end facts an agents stage of this build checks: a
// stage of another end (the coding stage's proof, the PR body) waits for the
// build that brings its stage.
var agentsEnds = []string{config.FactoryEndFindings, config.FactoryEndReport, config.FactoryEndCritic}

// runAgents runs one pass of an agents stage of task t (the engine's runner
// for config.FactoryStageAgents); done reports that its end fact held for
// every role. A stage with When "wide" is skipped unless the report's Files
// name 3 or more files, the database or a security path (wideChange). Then
// the copy (claimCopy: the claim runs off the loop, a hold is a wait), the
// stage's attempt (the open one, else a new one), the launch of its roles
// in one workspace, the runtime's events, and the prompts each role is due:
// its first, its one nudge, the one to go on after a pause, its fallback's
// after a Codex refusal. A wait (a hold, a pause, you in a pane) comes back
// as a held error, which costs the task nothing. A stage whose end fact
// another build's stage checks (agentsEnds), or a build without agents,
// leaves the task where it is, as a stage type without a runner does.
func (e *Engine) runAgents(ctx context.Context, t registry.Task, st config.FactoryStage) (bool, error) {
	if !slices.Contains(agentsEnds, st.End) || e.d.Runtime == nil {
		return false, nil
	}
	a, open, passed, err := e.stageAttempt(ctx, t, st)
	if err != nil || passed {
		return passed, err
	}
	if !open && st.When == config.FactoryWhenWide {
		if skip, err := e.skipNarrow(ctx, t, st); err != nil || skip {
			return skip, err
		}
	}
	c, err := e.claimCopy(ctx, t)
	if err != nil {
		return false, err
	}
	if !open {
		if a, err = e.beginStage(ctx, t, st); err != nil {
			return false, err
		}
	}
	run, rebuilt, err := e.agentsRunOf(ctx, t, st, a)
	if err != nil {
		return false, e.stageFailed(ctx, t, err)
	}
	if a.State == registry.AttemptLaunching {
		if err := e.launch(ctx, t, a, c, run.runAsRoles()); err != nil {
			return false, err
		}
	}
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	you := ""
	for _, ev := range append(rebuilt, e.takeEvents(t.ID)...) { // the first event that needs you is what the task waits on you for
		if what := e.onAgentEvent(ctx, &t, st, a, run, ev, dir); what != "" && you == "" {
			you = what
		}
	}
	if you == "" {
		you = e.recheckFailed(st, a, run, dir)
	}
	if you != "" {
		return false, e.stageFailed(ctx, t, needsYou{you})
	}
	held, err := e.promptRoles(ctx, t, st, a, c, run, dir)
	switch {
	case err != nil:
		return false, e.stageFailed(ctx, t, err)
	case held != nil:
		return false, held
	}
	if agentWait(t.WaitsWho) {
		e.waitOn(ctx, t, "", "")
	}
	if slices.ContainsFunc(run.roles, func(r *stageRole) bool { return !r.ok }) {
		return false, nil
	}
	return true, e.passStage(ctx, t, st, a, dir)
}

// needsYou is what an agents stage waits on you for (an agent that needs
// you, a file it wrote no valid one of after its nudge): no failure of the
// task, which goes to NEEDS YOU with it (stageFailed).
type needsYou struct{ what string }

func (n needsYou) Error() string { return waitsYou + ": " + n.what }

// stageFailed moves task t to NEEDS YOU on a needsYou (nil comes back: the
// task waits, it did not fail) and returns any other error as it is.
func (e *Engine) stageFailed(ctx context.Context, t registry.Task, err error) error {
	if n, ok := errors.AsType[needsYou](err); ok {
		e.toOperator(ctx, t, n.what, "an agent needs you")
		return nil
	}
	return err
}

// agentWait reports whether a task's wait is one an agents stage set (a
// pane it may not prompt, a CLI's pause): a pass that waited on nothing
// clears it.
func agentWait(who string) bool {
	switch who {
	case waitAgent, waitUsage, waitLogin, waitOverload:
		return true
	}
	return false
}

// stageAttempt finds the newest attempt of task t's stage st: open (it
// launches, runs or ended), or passed (verified, and the one the task
// counts: the stage passed and the move to the next one did not land).
// Neither is a new attempt's turn (beginStage).
func (e *Engine) stageAttempt(ctx context.Context, t registry.Task, st config.FactoryStage) (a registry.Attempt, open, passed bool, err error) {
	as, err := e.reg.Attempts(ctx, t.ID)
	if err != nil {
		return a, false, false, err
	}
	for _, x := range slices.Backward(as) {
		if x.Stage != st.Name {
			continue
		}
		switch x.State {
		case registry.AttemptLaunching, registry.AttemptRunning, registry.AttemptEnded:
			if t.Attempt != x.N {
				e.noteAttempt(ctx, t, x)
			}
			return x, true, false, nil
		case registry.AttemptVerified:
			return x, false, t.Attempt == x.N, nil
		}
		break
	}
	return a, false, false, nil
}

// beginStage begins a new attempt of task t's stage st and counts it on
// the task. The events kept for the task are an earlier stage's: dropped.
func (e *Engine) beginStage(ctx context.Context, t registry.Task, st config.FactoryStage) (registry.Attempt, error) {
	a, err := e.reg.BeginAttempt(ctx, t.ID, st.Name)
	if err != nil {
		return a, err
	}
	e.takeEvents(t.ID)
	delete(e.agentRuns, t.ID)
	e.noteAttempt(ctx, t, a)
	return a, nil
}

// noteAttempt counts attempt a on task t (Task.Attempt).
func (e *Engine) noteAttempt(ctx context.Context, t registry.Task, a registry.Attempt) {
	if err := e.reg.TransitionTask(ctx, t.ID, []string{t.State}, "", func(u *registry.TaskUpdate) { u.Set("attempt", a.N) }); err != nil {
		e.log.Warn("count a stage's attempt on its task", "task", t.ID, "attempt", a.ID, "err", err)
	}
}

// skipNarrow reports whether a stage that runs for a wide plan only is
// skipped: the report's Files are no wide change (wideChange), which an
// event says.
func (e *Engine) skipNarrow(ctx context.Context, t registry.Task, st config.FactoryStage) (bool, error) {
	b, err := os.ReadFile(filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), reportFile))
	if err != nil {
		return false, fmt.Errorf("the %s stage runs for a wide plan only, and %s cannot tell: %w", st.Name, reportFile, err)
	}
	if wide, why := wideChange(parseReport(string(b)).files); !wide {
		e.event(ctx, t.ID, "stage", registry.PhaseInfo, st.Name+" skipped: "+why)
		return true, nil
	}
	return false, nil
}

// agentsRunOf is what the loop knows of attempt a of task t's stage st,
// rebuilt from the registry and the agents store when it knows nothing (a
// restart, a new attempt): a role whose prompt the ledger holds was
// prompted (one settled as never sent was not), one whose prompt there
// starts with nudgeLead was nudged, one the runtime verified in the attempt
// is done, and a fallback with no prompt yet is still to launch. What the
// events of the observe pass said, which a restart loses, comes back from
// each role's last run (lastRunEvent): the events are the stage's to take
// with the observe pass's. A role that cannot run (a Codex role without a
// fallback on a task Codex never works on) is a needsYou.
func (e *Engine) agentsRunOf(ctx context.Context, t registry.Task, st config.FactoryStage, a registry.Attempt) (*agentsRun, []runtime.Event, error) {
	if run := e.agentRuns[t.ID]; run != nil && run.attempt == a.ID {
		return run, nil, nil
	}
	run := &agentsRun{attempt: a.ID}
	for _, name := range st.Roles {
		role, ok := e.cfg.FactoryRole(name)
		if !ok {
			return nil, nil, fmt.Errorf("the %s stage names %s, which is no [[factory.role]]", st.Name, name)
		}
		runAs, err := e.d.Runtime.RunsAs(t, role)
		if err != nil {
			return nil, nil, cannotRun(role, err)
		}
		run.roles = append(run.roles, &stageRole{role: role, runAs: runAs, runs: map[string]bool{}})
	}
	entries, err := e.reg.LedgerOf(ctx, t.ID)
	if err != nil {
		return nil, nil, err
	}
	done, err := e.reg.RoleRuns(ctx, a.ID)
	if err != nil {
		return nil, nil, err
	}
	var evs []runtime.Event
	for _, r := range run.roles {
		last := ""
		for _, en := range entries {
			if en.AttemptID != a.ID || en.Role != r.runAs.Name || en.Delivered == registry.DeliveredNo {
				continue
			}
			r.prompted = true
			r.runs[en.RunID] = true
			r.nudged = r.nudged || strings.HasPrefix(en.Text, nudgeLead)
			last = en.RunID
		}
		r.ok = slices.ContainsFunc(done, func(rr registry.RoleRun) bool { return rr.Role == r.runAs.Name && rr.Outcome == "verified" })
		r.relaunch = !r.prompted && r.runAs.Name != r.role.Name && a.State != registry.AttemptLaunching
		if r.ok || last == "" {
			continue
		}
		ev, err := e.lastRunEvent(ctx, t, r, last)
		if err != nil {
			return nil, nil, err
		}
		if ev != nil {
			evs = append(evs, *ev)
		}
	}
	e.agentRuns[t.ID] = run
	return run, evs, nil
}

// lastRunEvent is what role r's last run of the attempt, run, says of it in
// the agents store (Runtime.RunState), as the event that told it before a
// restart (nil when it says nothing to act on):
//   - ended and not verified: its end, which the stage checks now;
//   - failed (its end fact failed, the nudge after it or the file after the
//     nudge was still to come): its end again, checked against the files as
//     they are now, the run itself left as the store has it (settled);
//   - verified: the role is done;
//   - abandoned on its CLI's pause: the pause, so the role waits and goes on
//     once it is over (no event says a pause ended);
//   - in flight, or unknown: nothing, its events come.
func (e *Engine) lastRunEvent(ctx context.Context, t registry.Task, r *stageRole, run string) (*runtime.Event, error) {
	state, why, err := e.d.Runtime.RunState(ctx, run)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	}
	ev := &runtime.Event{TaskID: t.ID, Role: r.runAs.Name, RunID: run, Kind: runtime.EventEnded}
	switch state {
	case store.RunEnded:
		return ev, nil
	case store.RunFailed:
		r.settled = run
		return ev, nil
	case store.RunVerified:
		r.ok = true
	case store.RunAbandoned:
		if pause, ok := strings.CutPrefix(why, runtime.PausedPrefix); ok {
			ev.Kind, ev.Reason = runtime.EventPaused, pause
			return ev, nil
		}
	}
	return nil, nil
}

// cannotRun is the needsYou of a role that may not run on its task
// (Runtime.RunsAs's error).
func cannotRun(role config.Role, err error) needsYou {
	return needsYou{"the " + role.Name + " agent cannot run: " + err.Error()}
}

// runAsRoles are the roles of run as they run.
func (run *agentsRun) runAsRoles() []config.Role {
	out := make([]config.Role, 0, len(run.roles))
	for _, r := range run.roles {
		out = append(out, r.runAs)
	}
	return out
}

// byName is run's role that runs as name (or that the stage names name).
func (run *agentsRun) byName(name string) *stageRole {
	for _, r := range run.roles {
		if r.runAs.Name == name || r.role.Name == name {
			return r
		}
	}
	return nil
}

// onAgentEvent takes one of the runtime's events about task t: an ended
// run's end fact is checked and the run verified (a failed one gets its
// nudge, or, nudged already, the role's file is what the task waits on you
// for), a pause is a wait until the role may go on, a Codex refusal runs the
// task's Codex roles as their fallback, and the rest need you. It returns
// what the task waits on you for, "" for nothing; a wait it records is on
// *t too.
func (e *Engine) onAgentEvent(ctx context.Context, t *registry.Task, st config.FactoryStage, a registry.Attempt, run *agentsRun,
	ev runtime.Event, dir string) string {
	r := run.byName(ev.Role)
	if r == nil {
		return "" // another stage's role
	}
	name := r.runAs.Name
	switch ev.Kind {
	case runtime.EventEnded:
		return e.ended(ctx, *t, st, a, r, ev.RunID, dir)
	case runtime.EventPaused:
		r.next, r.pause = nextContinue, ev.Reason
		who, what, ok := strings.Cut(ev.Reason, ": ")
		if !ok {
			who, what = waitAgent, ev.Reason
		}
		e.waitOn(ctx, *t, who, what)
		t.WaitsWho, t.WaitsWhat = who, what
	case runtime.EventRefused:
		if r.runAs.AgentKind() != config.KindCodex {
			return "the " + name + " agent was refused: " + ev.Reason
		}
		// Codex never works on the task again: every Codex role of the
		// stage runs as its fallback, the refused one and any other whose
		// session the quit ends.
		run.quitCodex = true
		for _, x := range run.roles {
			if x.runAs.AgentKind() == config.KindCodex && !x.ok {
				x.relaunch, x.prompted, x.next, x.nudged, x.failed = true, false, "", false, ""
			}
		}
	case runtime.EventBlocked:
		return "the " + name + " agent shows a prompt the factory does not know: " + strings.TrimPrefix(ev.Reason, "dialog: ")
	case runtime.EventHooksTrust:
		return "the " + name + " agent: " + strings.TrimPrefix(ev.Reason, r.runAs.AgentKind()+": ")
	case runtime.EventLost:
		return withReason("the "+name+" agent's pane was lost", ev.Reason)
	case runtime.EventStuck:
		return withReason("the "+name+" agent is stuck", ev.Reason)
	case runtime.EventNoProgress:
		return withReason("the "+name+" agent makes no progress", strings.TrimPrefix(ev.Reason, "no progress: "))
	}
	return "" // a delivery of unknown fate: the observe pass sent the task to you
}

// withReason is what, then ": " and reason when there is one.
func withReason(what, reason string) string {
	if strings.TrimSpace(reason) == "" {
		return what
	}
	return what + ": " + reason
}

// ended takes the end of role r's run runID: an end of a run that is not
// the role's in this attempt is left alone. The role's end fact holds, and
// the run is verified; or it does not, the run fails, and the role gets its
// one nudge, or, nudged already, the task waits on you for the file. A run
// the store has as failed already (settled) is not verified again.
func (e *Engine) ended(ctx context.Context, t registry.Task, st config.FactoryStage, a registry.Attempt, r *stageRole, runID, dir string) string {
	if !r.runs[runID] && !e.ledgerRun(ctx, t, a, r, runID) {
		return ""
	}
	verify := func(ok bool, why string) {
		if runID != r.settled {
			e.verify(ctx, runID, ok, why)
		}
	}
	if r.ok {
		verify(true, "")
		return ""
	}
	p := checkEnd(st.End, dir, a.Started, []string{r.role.Name})
	if p == nil {
		verify(true, "")
		r.ok, r.next, r.pause, r.failed = true, "", "", ""
		return ""
	}
	verify(false, p.String())
	if !r.nudged {
		r.next, r.problem = nextNudge, p
		return ""
	}
	r.failed = p.file
	return noValidFile(r, p.file)
}

// noValidFile is the wait of a task whose role wrote no valid file after
// its nudge.
func noValidFile(r *stageRole, file string) string {
	return "the " + r.runAs.Name + " agent wrote no valid " + file
}

// ledgerRun reports whether runID is a run of role r in attempt a that the
// ledger holds (a fresh session the watchdog started gets a run of its
// own), and notes it.
func (e *Engine) ledgerRun(ctx context.Context, t registry.Task, a registry.Attempt, r *stageRole, runID string) bool {
	if runID == "" {
		return false
	}
	entries, err := e.reg.LedgerOf(ctx, t.ID)
	if err != nil {
		e.log.Warn("read the ledger for an ended run", "task", t.ID, "run", runID, "err", err)
		return false
	}
	if slices.ContainsFunc(entries, func(en registry.LedgerEntry) bool {
		return en.AttemptID == a.ID && en.Role == r.runAs.Name && en.RunID == runID
	}) {
		r.runs[runID] = true
		return true
	}
	return false
}

// verify settles a run that ended (Runtime.Verify); a failure is logged,
// the stage goes on from the files.
func (e *Engine) verify(ctx context.Context, runID string, ok bool, why string) {
	if err := e.d.Runtime.Verify(ctx, runID, ok, why); err != nil {
		e.log.Warn("verify an ended run", "run", runID, "ok", ok, "err", err)
	}
}

// recheckFailed checks again the files of the roles that wrote none valid
// after their nudge, once the task is back at work (you fixed the file, or
// told the agent to): a valid one is done, an invalid one is what the task
// waits on you for again.
func (e *Engine) recheckFailed(st config.FactoryStage, a registry.Attempt, run *agentsRun, dir string) string {
	for _, r := range run.roles {
		if r.failed == "" {
			continue
		}
		if p := checkEnd(st.End, dir, a.Started, []string{r.role.Name}); p != nil {
			return noValidFile(r, p.file)
		}
		r.ok, r.failed = true, ""
	}
	return ""
}

// promptRoles sends each role of run what it is due: the quit of the task's
// Codex sessions after a refusal and the launch of a fallback first, then
// a role's first prompt, its nudge, or the prompt to go on after a pause.
// held is the first wait (a hold, a pause, you in a pane, a delivery of
// unknown fate), which costs the task nothing; err a needsYou, or a failure
// of the task.
func (e *Engine) promptRoles(ctx context.Context, t registry.Task, st config.FactoryStage, a registry.Attempt, c Copy, run *agentsRun,
	dir string) (held, err error) {
	if run.quitCodex {
		if qerr := e.d.Runtime.QuitCodex(ctx, t); qerr != nil {
			e.event(ctx, t.ID, "codex_never", registry.PhaseFail, "quit the task's Codex sessions (tried again): "+qerr.Error())
		} else {
			run.quitCodex = false
		}
	}
	for _, r := range run.roles {
		if r.ok || r.failed != "" {
			continue
		}
		if r.relaunch {
			runAs, rerr := e.d.Runtime.RunsAs(t, r.role)
			if rerr != nil {
				return nil, cannotRun(r.role, rerr)
			}
			if lerr := e.launch(ctx, t, a, c, []config.Role{runAs}); lerr != nil {
				if errors.Is(lerr, ErrHeld) {
					held = cmpErr(held, lerr)
					continue
				}
				return nil, lerr
			}
			r.runAs, r.relaunch = runAs, false
		}
		text, next := "", r.next
		switch {
		case !r.prompted:
			if text, err = e.renderRole(t, st, r.role, r.runAs, c); err != nil {
				return nil, err
			}
		case next == nextNudge:
			text = nudgeText(dir, r.problem)
		case next == nextContinue:
			text = continueText(r.pause, e.outputs(t, st, r.role))
		default:
			continue
		}
		id, serr := e.d.Runtime.Send(ctx, t, a, r.runAs, text)
		if id != "" {
			r.runs[id] = true
			r.prompted = true
			if next == nextNudge {
				r.nudged = true
			}
			r.next, r.problem, r.pause = "", nil, ""
		}
		if serr == nil {
			continue
		}
		switch w := e.sendRefused(ctx, t, r, serr); {
		case errors.Is(w, ErrHeld):
			held = cmpErr(held, w)
		case w != nil:
			return nil, w // needs you
		case id == "":
			return nil, serr
		}
	}
	return held, nil
}

// cmpErr is the first of the two that is set.
func cmpErr(first, second error) error {
	if first != nil {
		return first
	}
	return second
}

// sendRefused maps a refused prompt to role r: a wait (the CLI's pause, you
// in its pane, a delivery of unknown fate, an agent that cannot take a
// prompt yet), a held error it records on the task, or a loop without
// progress, a needsYou; nil for any other error.
func (e *Engine) sendRefused(ctx context.Context, t registry.Task, r *stageRole, err error) error {
	var who, what string
	switch {
	case errors.Is(err, runtime.ErrNoProgress):
		return needsYou{"the " + r.runAs.Name + " agent makes no progress"}
	case errors.Is(err, runtime.ErrOperatorInPane):
		who, what = waitAgent, "you're in the "+r.runAs.Name+" pane; its prompt waits until you leave it"
	case errors.Is(err, runtime.ErrDeliveryUnknown):
		who, what = waitAgent, "prompt delivery unknown ("+r.runAs.Name+")"
	case errors.Is(err, agents.ErrBusy), errors.Is(err, agents.ErrNoSession):
		// Submit refused before the prompt left: the agent still works or
		// shows a dialog, or its session is being recovered (a lost pane
		// whose recoveries run out is EventLost).
		who, what = waitAgent, "the "+r.runAs.Name+" agent cannot take its prompt yet"
	default:
		w, ok := errors.AsType[*runtime.WaitError](err)
		if !ok {
			return nil
		}
		who, what = w.Who, w.What
	}
	e.waitOn(ctx, t, who, what)
	return heldError{who: who, what: what}
}

// nudgeText is a role's one nudge: its file at dir and what is wrong.
func nudgeText(dir string, p *endProblem) string {
	return nudgeLead + ": " + filepath.Join(dir, p.file) + ": " + p.why + ". Write it as your prompt asks, as the last step " +
		"of this turn, then end the turn."
}

// continueText is the prompt a role goes on with after its turn stopped on
// a pause (pause, the runtime's words for it) that is over, naming the
// files it writes.
func continueText(pause string, outputs []string) string {
	return "Your turn stopped (" + pause + "), and that is over now. Go on with the task where you stopped: your first prompt " +
		"still holds. Write " + strings.Join(outputs, " and ") + " as the last step, then end the turn."
}

// outputs are the files role writes in stage st of task t, as paths.
func (e *Engine) outputs(t registry.Task, st config.FactoryStage, role config.Role) []string {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	var names []string
	switch st.End {
	case config.FactoryEndFindings:
		names = []string{findingsFile(role.Name)}
	case config.FactoryEndReport:
		names = []string{reportFile, acceptanceFile, questionsFile}
	case config.FactoryEndCritic:
		names = []string{criticFile, roleQuestionsFile(role.Name)}
	}
	for i, n := range names {
		names[i] = filepath.Join(dir, n)
	}
	return names
}

// passStage ends a stage whose roles' end facts held: its attempt verified
// and, for the judge's report, the report, its acceptance checks and the
// answers it read recorded as artifacts (a revision each time one changed).
func (e *Engine) passStage(ctx context.Context, t registry.Task, st config.FactoryStage, a registry.Attempt, dir string) error {
	delete(e.agentRuns, t.ID)
	if err := e.reg.TransitionAttempt(ctx, a.ID, []string{registry.AttemptRunning, registry.AttemptEnded}, registry.AttemptVerified, nil); err != nil &&
		!errors.Is(err, registry.ErrConflict) {
		return err
	}
	if st.End != config.FactoryEndReport {
		return nil
	}
	for _, f := range []struct{ kind, name string }{{"report", reportFile}, {"acceptance", acceptanceFile}, {"answers", answersFile}} {
		b, err := os.ReadFile(filepath.Join(dir, f.name))
		if err != nil {
			continue // answers.md comes with the operator's answers
		}
		sum := sha256.Sum256(b)
		if err := e.reg.RecordArtifact(ctx, registry.Artifact{TaskID: t.ID, Kind: f.kind, Path: f.name, SHA256: hex.EncodeToString(sum[:])}); err != nil {
			e.log.Warn("record an artifact", "task", t.ID, "file", f.name, "err", err)
		}
	}
	return nil
}

// renderRole renders the prompt of role (as the stage names it) for task t
// in stage st, as it runs as runAs in copy c.
func (e *Engine) renderRole(t registry.Task, st config.FactoryStage, role, runAs config.Role, c Copy) (string, error) {
	p, err := e.cfg.ResolvePrompt(runAs.Prompt)
	if err != nil {
		return "", fmt.Errorf("the %s prompt: %w", runAs.Name, err)
	}
	return agents.RenderPrompt(p, e.promptData(t, st, role, runAs, c))
}

// promptData is the data of role's prompt (as the stage names it, run as
// runAs) for task t in stage st, in copy c.
func (e *Engine) promptData(t registry.Task, st config.FactoryStage, role, runAs config.Role, c Copy) PromptData {
	dir := e.d.Layout.FactoryTaskDir(t.ID)
	in := func(name string) string { return filepath.Join(dir, name) }
	present := func(name string) string {
		if fsx.Exists(in(name)) {
			return in(name)
		}
		return ""
	}
	d := PromptData{TaskID: t.ID, Key: t.JiraKey, Kind: t.Kind, Repo: t.Repo, Branch: t.Branch, Copy: c.Path, TaskDir: dir,
		Ticket: present("ticket.md"), Guidance: present("guidance.md"), Prompt: present("prompt.md"), Answers: present(answersFile),
		Rules: present("rules.md"), Findings: in("findings-*.md"), Report: in(reportFile), Questions: in(roleQuestionsFile(role.Name)),
		Acceptance: in(acceptanceFile), Proof: in("proof.json"),
		DBLockCommand: agents.DBLockLine(e.d.Layout.Binary(), c.Path, runAs.Name), Port: c.Port}
	if repo, ok := e.cfg.FactoryRepo(t.Repo); ok {
		d.ErrorSkill = repo.ErrorSkill
	}
	switch st.End {
	case config.FactoryEndFindings:
		d.Findings = in(findingsFile(role.Name))
	case config.FactoryEndReport:
		d.Questions = in(questionsFile)
	}
	return d
}

// CheckPrompts renders the prompt of every factory role ([[factory.role]])
// with data whose every field is set, as the factory daemon does once at
// startup, after it loaded its prompts: a prompt (or factory-shared.md)
// that names a field the data lacks, or a block the partials file does not
// define, is an error naming the role and its file.
func CheckPrompts(cfg *config.Config) error {
	var errs []error
	seen := map[string]bool{}
	for _, r := range cfg.Factory.Roles {
		if r.Prompt == "" || seen[r.Prompt] {
			continue
		}
		seen[r.Prompt] = true
		p, err := cfg.ResolvePrompt(r.Prompt)
		if err == nil {
			_, err = agents.RenderPrompt(p, checkData(r))
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("factory role %s: %w", r.Name, err))
		}
	}
	return errors.Join(errs...)
}

// checkData is prompt data for role with every field set (CheckPrompts).
func checkData(role config.Role) PromptData {
	dir := "/factory/tasks/1"
	copyPath := "/factory/copies/copy1"
	return PromptData{TaskID: 1, Key: "PR-1", Kind: "bug", Repo: "example/app", Branch: "factory-check", Copy: copyPath,
		TaskDir: dir, Ticket: dir + "/ticket.md", Guidance: dir + "/guidance.md", Prompt: dir + "/prompt.md",
		Answers: dir + "/" + answersFile, Rules: dir + "/rules.md", Findings: dir + "/" + findingsFile(role.Name),
		Report: dir + "/" + reportFile, Questions: dir + "/" + questionsFile, Acceptance: dir + "/" + acceptanceFile,
		Proof: dir + "/proof.json", Review: dir + "/review-1.json", ErrorSkill: "error-tracker",
		DBLockCommand: agents.DBLockLine("magnum", copyPath, role.Name), Port: 3000}
}
