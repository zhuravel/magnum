package factory

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

var t0 = time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)

// fakeRuntime is the agent runtime as the engine sees it: it records the
// launches, parks and deadline shifts it was asked for.
type fakeRuntime struct {
	mu         sync.Mutex
	launches   []int64
	parks      []int64
	shifts     []time.Duration
	events     []runtime.Event // what Observe reports
	observeErr error
	// launchErr is what Launch answers; observePanic, when set, is what
	// Observe panics with; watched counts the Watchdog calls.
	launchErr    error
	observePanic any
	watched      int
	// parkErr is what Park answers once it recorded the park.
	parkErr error
	// letGo records the LetGo calls, "<task key>/<role>"; drained is
	// whether BeginDrain was called.
	letGo   []string
	drained bool

	// The agents stage's side: cfg and reg are the harness's (RunsAs reads
	// the Codex fallbacks, Send writes the ledger as the runtime does);
	// launchRoles are the roles of each Launch, sent the prompts, verified
	// the Verify calls, runs the runs Send made; sendErr is what Send
	// answers (nil: a new run), quits the QuitCodex calls and quitErr their
	// answer.
	cfg         *config.Config
	reg         *registry.Registry
	launchRoles [][]string
	// launchPrompts are the prompt files of the roles of each Launch, as
	// the stage runs them, launchCopies the copy each ran in; voided the
	// Void calls.
	launchPrompts [][]string
	launchCopies  []Copy
	sent          []sentPrompt
	verified      []verifyCall
	voided        []verifyCall
	recovered     []string // the RecoverLost calls, "<task key>/<role>"
	runs          int
	sendErr       error
	quits         []int64
	quitErr       error
	roleQuits     []string // the QuitRole calls, "<task key>/<role>"
	// runState is each run's state as the agents store keeps it (RunState):
	// Send makes a working run, Observe's events end, pause or refuse it as
	// the runtime does, Verify settles it.
	runState map[string]runFact
	// atWork is the role AtWork names ("" for none), atWorkYou whether you
	// are in its pane (rather than its agent at work).
	atWork    string
	atWorkYou bool
	// touched is who TouchedSince names ("" for nobody), touchedAt when
	// (zero: for any time).
	touched   string
	touchedAt time.Time
	// The restart's recovery: calls are Recover, SettleWorkspaces and
	// Reconcile in order ("recover <key>", "workspaces", "reconcile <entry
	// id>", and what note adds), changed and orphans their answers,
	// recoverErr Recover's and SettleWorkspaces' error; verdict is
	// Reconcile's answer for an entry (nil: unknown), which it settles as
	// the runtime does; unknownNext are the roles whose next Send leaves its
	// entry unknown and its run pending, as a daemon stopped between Submit
	// and the verdict leaves them.
	calls       []string
	changed     map[int64][]string
	orphans     []runtime.Orphan
	recoverErr  error
	verdict     func(e registry.LedgerEntry) string
	unknownNext map[string]bool
	// failRecover is Recover's error for a task (on top of recoverErr).
	// provable are the entries whose transcript can prove they never
	// arrived (written past the prompt, the run never submitted): as the
	// runtime's, a read of one that settles nothing begins its proof
	// (proving, Proving), and a read that settles it, or one of an entry
	// that cannot be proven, ends it.
	failRecover map[int64]error
	provable    map[int64]bool
	proving     map[int64]bool
	// paneFocus is what PaneFocus answers, workspaces the herdr workspaces
	// of each task's live sessions (Workspaces).
	paneFocus  runtime.PaneFocus
	workspaces map[int64][]string
	// talks are the Talk calls in order; talk, when set, is what Talk does
	// (nil: the pane "p_1"), run without the lock.
	talks []talkCall
	talk  func(ctx context.Context, t registry.Task, prompt string) (string, error)
}

// talkCall is a Talk the engine made: the task and the prompt it rendered.
type talkCall struct {
	task   int64
	prompt string
}

func (f *fakeRuntime) Talk(ctx context.Context, t registry.Task, prompt string) (string, error) {
	f.mu.Lock()
	f.talks = append(f.talks, talkCall{task: t.ID, prompt: prompt})
	fn := f.talk
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, t, prompt)
	}
	return "p_1", nil
}

// talkCalls are the Talk calls so far.
func (f *fakeRuntime) talkCalls() []talkCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.talks)
}

// canProve makes entry id provable: its transcript is written past its
// prompt, and its run was never submitted.
func (f *fakeRuntime) canProve(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.provable == nil {
		f.provable = map[int64]bool{}
	}
	f.provable[id] = true
}

// Proving reports whether a read began entry id's proof and none settled it.
func (f *fakeRuntime) Proving(id int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.proving[id]
}

// setFailRecover makes Recover fail for task id with err (nil: not).
func (f *fakeRuntime) setFailRecover(id int64, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRecover == nil {
		f.failRecover = map[int64]error{}
	}
	f.failRecover[id] = err
}

// note adds what to the calls (a collaborator's call in the same order).
func (f *fakeRuntime) note(what string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, what)
}

func (f *fakeRuntime) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// Recover records the call and answers changed for the task.
func (f *fakeRuntime) Recover(_ context.Context, t registry.Task) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "recover "+TaskKey(t))
	if err := f.failRecover[t.ID]; err != nil {
		return nil, err
	}
	return f.changed[t.ID], f.recoverErr
}

// SettleWorkspaces records the call and answers orphans.
func (f *fakeRuntime) SettleWorkspaces(context.Context) ([]runtime.Orphan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "workspaces")
	if f.recoverErr != nil {
		return nil, f.recoverErr
	}
	return slices.Clone(f.orphans), nil
}

// sendUnknown has role's next Send leave its entry unknown.
func (f *fakeRuntime) sendUnknown(role string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unknownNext == nil {
		f.unknownNext = map[string]bool{}
	}
	f.unknownNext[role] = true
}

// setVerdict makes Reconcile answer v for every entry.
func (f *fakeRuntime) setVerdict(v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verdict = func(registry.LedgerEntry) string { return v }
}

// runFact is a run's state and why it failed or was given up.
type runFact struct{ state, why string }

// sentPrompt is a prompt Send took: the task, the attempt, the role (as it
// runs), the text, the run it made and the prompt file of the role as the
// stage runs it.
type sentPrompt struct {
	TaskID              int64
	Attempt, Role, Text string
	RunID               string
	PromptFile          string
}

// verifyCall is a Verify the stage made.
type verifyCall struct {
	RunID string
	OK    bool
	Why   string
}

func (f *fakeRuntime) Owner(context.Context, registry.Task) (store.PR, error) { return store.PR{}, nil }

// Launch records the launch and, as the runtime does, moves a launching
// attempt to running when it goes through.
func (f *fakeRuntime) Launch(ctx context.Context, t registry.Task, a registry.Attempt, c Copy, roles []config.Role) error {
	f.mu.Lock()
	f.launches = append(f.launches, t.ID)
	f.launchCopies = append(f.launchCopies, c)
	names, prompts := make([]string, 0, len(roles)), make([]string, 0, len(roles))
	for _, r := range roles {
		names, prompts = append(names, r.Name), append(prompts, r.Prompt)
	}
	f.launchRoles, f.launchPrompts = append(f.launchRoles, names), append(f.launchPrompts, prompts)
	err, reg := f.launchErr, f.reg
	f.mu.Unlock()
	if err != nil || reg == nil || a.ID == "" {
		return err
	}
	if err := reg.TransitionAttempt(ctx, a.ID, []string{registry.AttemptLaunching}, registry.AttemptRunning, nil); err != nil &&
		!errors.Is(err, registry.ErrConflict) {
		return err
	}
	return nil
}

// Send takes the prompt as a new run and writes its ledger entry, delivered,
// as the runtime does; sendErr refuses it before anything is written, and
// so does an entry of the role whose delivery is unknown
// (runtime.ErrDeliveryUnknown). A role in unknownNext leaves its entry
// unknown and its run pending.
func (f *fakeRuntime) Send(ctx context.Context, t registry.Task, a registry.Attempt, role config.Role, text string) (string, error) {
	f.mu.Lock()
	if f.sendErr != nil {
		err := f.sendErr
		f.mu.Unlock()
		return "", err
	}
	reg := f.reg
	f.mu.Unlock()
	if reg != nil {
		entries, err := reg.LedgerOf(ctx, t.ID)
		if err != nil {
			return "", err
		}
		if slices.ContainsFunc(entries, func(e registry.LedgerEntry) bool {
			return e.Role == role.Name && e.Delivered == registry.DeliveredUnknown
		}) {
			return "", fmt.Errorf("send to %s: %w", role.Name, runtime.ErrDeliveryUnknown)
		}
	}
	f.mu.Lock()
	f.runs++
	id := "run-" + strconv.Itoa(f.runs)
	f.sent = append(f.sent, sentPrompt{TaskID: t.ID, Attempt: a.ID, Role: role.Name, Text: text, RunID: id, PromptFile: role.Prompt})
	unknown := f.unknownNext[role.Name]
	delete(f.unknownNext, role.Name)
	if unknown {
		f.setRunLocked(id, store.RunPending, "")
	} else {
		f.setRunLocked(id, store.RunWorking, "")
	}
	f.mu.Unlock()
	if reg == nil {
		return id, nil
	}
	e, err := reg.AppendLedger(ctx, registry.LedgerEntry{TaskID: t.ID, AttemptID: a.ID, Role: role.Name, RunID: id,
		Hash: runtime.PromptHash(text), Text: text})
	if err != nil || unknown {
		return id, err
	}
	return id, reg.MarkDelivered(ctx, e.ID, registry.DeliveredYes)
}

// RunsAs is the runtime's: a Codex role of a task Codex never works on runs
// as its codex_fallback.
func (f *fakeRuntime) RunsAs(t registry.Task, role config.Role) (config.Role, error) {
	if !t.CodexNever || role.AgentKind() != config.KindCodex || f.cfg == nil {
		return role, nil
	}
	if fb, ok := f.cfg.FactoryRole(f.cfg.Factory.CodexFallback[role.Name]); ok {
		return fb, nil
	}
	return config.Role{}, errors.New(role.Name + " has no codex_fallback")
}

// QuitRole records the call, "<task key>/<role>".
func (f *fakeRuntime) QuitRole(_ context.Context, t registry.Task, role, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.roleQuits = append(f.roleQuits, TaskKey(t)+"/"+role)
	return nil
}

// roleQuitList are the QuitRole calls so far.
func (f *fakeRuntime) roleQuitList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.roleQuits)
}

func (f *fakeRuntime) QuitCodex(_ context.Context, t registry.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quits = append(f.quits, t.ID)
	return f.quitErr
}

func (f *fakeRuntime) setSendErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendErr = err
}

func (f *fakeRuntime) setEvents(evs []runtime.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = evs
}

// launched roles are the role names of each Launch.
func (f *fakeRuntime) launchedRoles() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.launchRoles)
}

// launchedCopies are the copies each Launch ran its roles in.
func (f *fakeRuntime) launchedCopies() []Copy {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.launchCopies)
}

// launchedPrompts are the prompt files of the roles of each Launch.
func (f *fakeRuntime) launchedPrompts() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.launchPrompts)
}

// promptsTo are the prompts Send took for role, oldest first.
func (f *fakeRuntime) promptsTo(role string) []sentPrompt {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sentPrompt
	for _, p := range f.sent {
		if p.Role == role {
			out = append(out, p)
		}
	}
	return out
}

func (f *fakeRuntime) allPrompts() []sentPrompt {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}

func (f *fakeRuntime) verifies() []verifyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.verified)
}

func (f *fakeRuntime) voids() []verifyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.voided)
}

func (f *fakeRuntime) codexQuits() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.quits)
}

// Observe reports the events, and moves their runs as the runtime does: an
// ended run ends, a paused one is abandoned with its pause, a refused one
// is abandoned.
func (f *fakeRuntime) Observe(context.Context) ([]runtime.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.observePanic != nil {
		panic(f.observePanic)
	}
	for _, ev := range f.events {
		if cur, ok := f.runState[ev.RunID]; !ok || cur.state != store.RunWorking {
			continue
		}
		switch ev.Kind {
		case runtime.EventEnded:
			f.setRunLocked(ev.RunID, store.RunEnded, "")
		case runtime.EventPaused:
			f.setRunLocked(ev.RunID, store.RunAbandoned, runtime.PausedPrefix+ev.Reason)
		case runtime.EventRefused:
			f.setRunLocked(ev.RunID, store.RunAbandoned, "refused: "+ev.Reason)
		}
	}
	return f.events, f.observeErr
}

func (f *fakeRuntime) setRunLocked(id, state, why string) {
	if f.runState == nil {
		f.runState = map[string]runFact{}
	}
	f.runState[id] = runFact{state, why}
}

// RunState is the run's state as Send, Observe and Verify left it.
func (f *fakeRuntime) RunState(_ context.Context, id string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runState[id]
	if !ok {
		return "", "", fmt.Errorf("run %s: %w", id, store.ErrNotFound)
	}
	return r.state, r.why, nil
}

// Verify records the call and, as the runtime does, the role's outcome in
// the attempt (the RoleRun of the run's attempt and role).
func (f *fakeRuntime) Verify(ctx context.Context, runID string, ok bool, why string) error {
	f.mu.Lock()
	f.verified = append(f.verified, verifyCall{RunID: runID, OK: ok, Why: why})
	if cur, known := f.runState[runID]; known && cur.state != store.RunEnded {
		f.mu.Unlock()
		return fmt.Errorf("verify %s: %w: it is %s", runID, store.ErrConflict, cur.state) // the store moves only an ended run
	}
	if ok {
		f.setRunLocked(runID, store.RunVerified, "")
	} else {
		f.setRunLocked(runID, store.RunFailed, why)
	}
	i := slices.IndexFunc(f.sent, func(p sentPrompt) bool { return p.RunID == runID })
	reg := f.reg
	var p sentPrompt
	if i >= 0 {
		p = f.sent[i]
	}
	f.mu.Unlock()
	if reg == nil || i < 0 {
		return nil
	}
	rr := registry.RoleRun{AttemptID: p.Attempt, Role: p.Role, Outcome: "verified", Verified: time.Now(), EndWhy: why}
	if !ok {
		rr.Outcome, rr.Verified = "failed:unverified", time.Time{}
	}
	return reg.RecordRoleRun(ctx, rr)
}

// Void records the call and, as the runtime does, keeps the run failed with
// why and its role's outcome voided.
func (f *fakeRuntime) Void(ctx context.Context, runID, why string) error {
	f.mu.Lock()
	f.voided = append(f.voided, verifyCall{RunID: runID, Why: why})
	if cur, known := f.runState[runID]; known && cur.state != store.RunEnded {
		f.mu.Unlock()
		return fmt.Errorf("void %s: %w: it is %s", runID, store.ErrConflict, cur.state) // the store moves only an ended run
	}
	f.setRunLocked(runID, store.RunFailed, why)
	i := slices.IndexFunc(f.sent, func(p sentPrompt) bool { return p.RunID == runID })
	reg := f.reg
	var p sentPrompt
	if i >= 0 {
		p = f.sent[i]
	}
	f.mu.Unlock()
	if reg == nil || i < 0 {
		return nil
	}
	return reg.RecordRoleRun(ctx, registry.RoleRun{AttemptID: p.Attempt, Role: p.Role, Outcome: "voided", EndWhy: why})
}

// setAtWork has role's agent at work in the task's copy ("" for none).
func (f *fakeRuntime) setAtWork(role string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.atWork, f.atWorkYou = role, false
}

// setOperatorIn has you in role's pane ("" for none).
func (f *fakeRuntime) setOperatorIn(role string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.atWork, f.atWorkYou = role, role != ""
}

// setTouched has who changed the task's copy since any time ("" for nobody).
func (f *fakeRuntime) setTouched(who string) { f.setTouchedAt(who, time.Time{}) }

// setTouchedAt has who change the task's copy at at: TouchedSince names
// them for a time before it (the zero at: for any time).
func (f *fakeRuntime) setTouchedAt(who string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched, f.touchedAt = who, at
}

// TouchedSince is who touched names, when they touched the copy after since.
func (f *fakeRuntime) TouchedSince(_ context.Context, _ registry.Task, since time.Time) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.touchedAt.IsZero() && !f.touchedAt.After(since) {
		return "", nil
	}
	return f.touched, nil
}

// AtWork is who atWork names: a run in flight in the task's copy, or you
// in a pane of the task.
func (f *fakeRuntime) AtWork(context.Context, registry.Task) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.atWork, f.atWorkYou, nil
}

func (f *fakeRuntime) Park(_ context.Context, t registry.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parks = append(f.parks, t.ID)
	return f.parkErr
}

// setPaneFocus is what the observe passes saw of you in the factory's
// panes (PaneFocus).
func (f *fakeRuntime) setPaneFocus(p runtime.PaneFocus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paneFocus = p
}

func (f *fakeRuntime) PaneFocus() runtime.PaneFocus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paneFocus
}

// setWorkspaces gives task id's live sessions the herdr workspaces ws.
func (f *fakeRuntime) setWorkspaces(id int64, ws ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.workspaces == nil {
		f.workspaces = map[int64][]string{}
	}
	f.workspaces[id] = ws
}

func (f *fakeRuntime) Workspaces(_ context.Context, t registry.Task) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.workspaces[t.ID]), nil
}

// Reconcile records the call and answers verdict's verdict (unknown without
// one), settling it as the runtime does: yes marks the entry delivered and
// its run at work, no marks it never sent and abandons its run. An unknown
// read of a provable entry begins its proof; any other read ends it.
func (f *fakeRuntime) Reconcile(ctx context.Context, e registry.LedgerEntry) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fmt.Sprintf("reconcile %d", e.ID))
	verdict, reg := f.verdict, f.reg
	f.mu.Unlock()
	v := registry.DeliveredUnknown
	if verdict != nil {
		v = verdict(e)
	}
	f.mu.Lock()
	if f.proving == nil {
		f.proving = map[int64]bool{}
	}
	f.proving[e.ID] = v == registry.DeliveredUnknown && f.provable[e.ID]
	f.mu.Unlock()
	if v == registry.DeliveredUnknown || reg == nil {
		return v, nil
	}
	if err := reg.MarkDelivered(ctx, e.ID, v); err != nil {
		return registry.DeliveredUnknown, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch v {
	case registry.DeliveredYes:
		f.setRunLocked(e.RunID, store.RunWorking, "")
	case registry.DeliveredNo:
		f.setRunLocked(e.RunID, store.RunAbandoned, "the prompt never arrived")
	}
	return v, nil
}

func (f *fakeRuntime) Watchdog(context.Context, time.Time) ([]runtime.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watched++
	return nil, nil
}

// RecoverLost records the role it recovers, "<task key>/<role>".
func (f *fakeRuntime) RecoverLost(_ context.Context, t registry.Task, _ registry.Attempt, role config.Role) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recovered = append(f.recovered, TaskKey(t)+"/"+role.Name)
	return nil
}

func (f *fakeRuntime) recoveries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.recovered)
}

// abandonRun has the agents store keep run id abandoned with why, as the
// runtime leaves a run a recovery or a pause gave up.
func (f *fakeRuntime) abandonRun(id, why string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setRunLocked(id, store.RunAbandoned, why)
}

// LetGo records the call and, as the runtime does, settles the role's
// entries of unknown delivery as abandoned and abandons a run of theirs
// that never got past pending (runtime.LetGoWhy).
func (f *fakeRuntime) LetGo(ctx context.Context, t registry.Task, role string) (int, error) {
	f.mu.Lock()
	f.letGo = append(f.letGo, TaskKey(t)+"/"+role)
	reg := f.reg
	f.mu.Unlock()
	if reg == nil {
		return 1, nil
	}
	entries, err := reg.LedgerOf(ctx, t.ID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.Role != role || e.Delivered != registry.DeliveredUnknown {
			continue
		}
		if err := reg.MarkDelivered(ctx, e.ID, registry.DeliveredAbandoned); err != nil {
			return n, err
		}
		n++
		f.mu.Lock()
		if f.runState[e.RunID].state == store.RunPending {
			f.setRunLocked(e.RunID, store.RunAbandoned, runtime.LetGoWhy)
		}
		f.mu.Unlock()
	}
	return n, nil
}

func (f *fakeRuntime) Shift(gap time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shifts = append(f.shifts, gap)
}

func (f *fakeRuntime) BeginDrain() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drained = true
}

func (f *fakeRuntime) isDrained() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.drained
}

func (f *fakeRuntime) launched() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.launches...)
}

func (f *fakeRuntime) parked() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.parks...)
}

// fakeCopies records the servers it was asked to start and stop and the
// releases; a claim reserves a copy for its task (reserved: the claimed
// one's name, else factory1) and answers claimed, or claimErr, after block
// (when set) is closed or ctx ends, and counts itself in claims. A release
// answers releaseErr, or frees the task's reservation.
type fakeCopies struct {
	mu         sync.Mutex
	started    []string
	stopped    []string
	released   []int64
	reserved   map[int64]string
	releaseErr error
	claimed    Copy
	claimErr   error
	claims     int
	block      chan struct{}
	// ended counts the claims that returned, cancelled those that returned
	// because their context ended.
	ended, cancelled int
	// onReconcile runs at each Reconcile.
	onReconcile func()
	// startedBrowsers and stoppedBrowsers name the copies whose browser
	// started and stopped; stopCtxErr is the context's error at each stop
	// of a server or a browser, in order.
	startedBrowsers, stoppedBrowsers []string
	stopCtxErr                       []error
	// stopErr is what each StopServer returns.
	stopErr error
	// startErrs are what the next StartServer calls return, one each (none
	// once they run out: the server starts); browserDirs are the folders the
	// StartBrowser calls got, in order; up names the copies whose server
	// started and has not stopped since.
	startErrs   []error
	browserDirs []string
	up          map[string]bool
}

// copiesLog is what the fake copies were asked to do so far: the servers
// started (a start that failed too) and stopped, the browsers started (and
// their folders) and stopped, and the context's error at each stop.
type copiesLog struct {
	started, stopped, browsers, browserDirs, stoppedBrowsers []string
	stopCtxErr                                               []error
}

func (f *fakeCopies) log() copiesLog {
	f.mu.Lock()
	defer f.mu.Unlock()
	return copiesLog{started: slices.Clone(f.started), stopped: slices.Clone(f.stopped), browsers: slices.Clone(f.startedBrowsers),
		browserDirs: slices.Clone(f.browserDirs), stoppedBrowsers: slices.Clone(f.stoppedBrowsers), stopCtxErr: slices.Clone(f.stopCtxErr)}
}

// serverUp reports whether a server of copy name started and has not
// stopped since.
func (f *fakeCopies) serverUp(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.up[name]
}

func (f *fakeCopies) Claim(ctx context.Context, t registry.Task) (Copy, error) {
	f.mu.Lock()
	f.claims++
	if f.reserved == nil {
		f.reserved = map[int64]string{}
	}
	f.reserved[t.ID] = cmp.Or(f.claimed.Name, "factory1")
	block := f.block
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.ended++
		f.mu.Unlock()
	}()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			f.mu.Lock()
			f.cancelled++
			f.mu.Unlock()
			return Copy{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return Copy{}, f.claimErr
	}
	return f.claimed, nil
}

// endedClaims are how many claims returned, and how many of them because
// their context ended.
func (f *fakeCopies) endedClaims() (ended, cancelled int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ended, f.cancelled
}

func (f *fakeCopies) releases() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.released...)
}

func (f *fakeCopies) claimCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims
}

func (f *fakeCopies) Release(_ context.Context, t registry.Task, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, t.ID)
	if f.releaseErr != nil {
		return f.releaseErr
	}
	delete(f.reserved, t.ID)
	return nil
}

func (f *fakeCopies) Reserved(context.Context) ([]Reservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Reservation
	for id, name := range f.reserved {
		out = append(out, Reservation{TaskID: id, Copy: name})
	}
	slices.SortFunc(out, func(a, b Reservation) int { return cmp.Compare(a.TaskID, b.TaskID) })
	return out, nil
}

// reserve makes copy name reserved for task id, as a claim leaves it.
func (f *fakeCopies) reserve(id int64, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reserved == nil {
		f.reserved = map[int64]string{}
	}
	f.reserved[id] = name
}

func (f *fakeCopies) setReleaseErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseErr = err
}

func (f *fakeCopies) Free(context.Context) (int, error) { return 1, nil }

func (f *fakeCopies) StartServer(_ context.Context, c Copy) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, c.Name)
	if len(f.startErrs) > 0 {
		err := f.startErrs[0]
		f.startErrs = f.startErrs[1:]
		return err
	}
	if f.up == nil {
		f.up = map[string]bool{}
	}
	f.up[c.Name] = true
	return nil
}

func (f *fakeCopies) StopServer(ctx context.Context, c Copy) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, c.Name)
	f.stopCtxErr = append(f.stopCtxErr, ctx.Err())
	delete(f.up, c.Name)
	return f.stopErr
}

func (f *fakeCopies) StartBrowser(_ context.Context, c Copy, dir string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startedBrowsers = append(f.startedBrowsers, c.Name)
	f.browserDirs = append(f.browserDirs, dir)
	return "http://127.0.0.1:9333", nil
}

func (f *fakeCopies) StopBrowser(ctx context.Context, c Copy) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stoppedBrowsers = append(f.stoppedBrowsers, c.Name)
	f.stopCtxErr = append(f.stopCtxErr, ctx.Err())
	return nil
}

// Reconcile runs onReconcile, when set (a test's record of the order), and
// ends the servers a stopped daemon left, as the copies' reconcile does.
func (f *fakeCopies) Reconcile(context.Context) error {
	f.mu.Lock()
	on := f.onReconcile
	f.up = nil
	f.mu.Unlock()
	if on != nil {
		on()
	}
	return nil
}

// harness is an engine on a test registry, fake collaborators and a clock
// the test moves; no real process, socket or directory outside t.TempDir.
type harness struct {
	t      *testing.T
	ctx    context.Context
	clock  *storetest.Clock
	reg    *registry.Registry
	cfg    *config.Config
	rt     *fakeRuntime
	copies *fakeCopies
	layout paths.Layout
	deps   Deps
	e      *Engine
}

// restart builds a new engine over the same registry and collaborators, as
// a daemon restart does: nothing of the old engine's memory comes along.
func (h *harness) restart() {
	h.t.Helper()
	h.e.stopBackground()
	h.e = New(h.deps)
	h.t.Cleanup(h.e.stopBackground)
}

func newHarness(t *testing.T, edit ...func(d *Deps)) *harness {
	t.Helper()
	storetest.Parallel(t)
	clock := storetest.NewClock(t0)
	reg := registrytest.OpenWith(t, registry.Options{Clock: clock.Now})
	cfg := config.Defaults()
	h := &harness{t: t, ctx: t.Context(), clock: clock, reg: reg, cfg: cfg, rt: &fakeRuntime{cfg: cfg, reg: reg}, copies: &fakeCopies{},
		layout: paths.Layout{Home: t.TempDir()}}
	d := Deps{Config: cfg, Layout: h.layout, Registry: reg, Runtime: h.rt, Copies: h.copies, Clock: clock.Now,
		Log: slog.New(slog.DiscardHandler)}
	for _, f := range edit {
		f(&d)
	}
	h.deps = d
	h.e = New(d)
	t.Cleanup(h.e.stopBackground) // no claim or overlap outlives the test
	return h
}

// task creates a task of kind bug, in state and at stage.
func (h *harness) task(key, state, stage string) registry.Task {
	h.t.Helper()
	t, err := h.reg.CreateTask(h.ctx, registry.Task{JiraKey: key, Kind: "bug", Repo: "example/shop", Title: key + " title",
		State: state, Stage: stage})
	if err != nil {
		h.t.Fatal(err)
	}
	return t
}

// stage makes fn the runner of every stage of type typ.
func (h *harness) stage(typ string, fn stageRunner) { h.e.runners[typ] = fn }

// idleStage is a stage runner that does nothing and never passes: a test
// of what happens around the stages, not in them.
func idleStage(context.Context, registry.Task, config.FactoryStage) (bool, error) { return false, nil }

func (h *harness) tick() {
	h.t.Helper()
	_ = h.e.Tick(h.ctx) // a tick's errors are the steps' own, logged and recorded by them
}

func (h *harness) reload(id int64) registry.Task {
	h.t.Helper()
	t, err := h.reg.TaskByID(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return t
}

// events returns the task's events of kind (taskID 0: the daemon's own).
func (h *harness) events(taskID int64, kind string) []registry.Event {
	h.t.Helper()
	evs, err := h.reg.Events(h.ctx, taskID, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []registry.Event
	for _, ev := range evs {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// calls records which tasks a stage runner saw, in order, and when.
type calls struct {
	mu  sync.Mutex
	ids []int64
	at  []time.Time
}

func (c *calls) add(id int64, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids = append(c.ids, id)
	c.at = append(c.at, at)
}

func (c *calls) list() []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int64(nil), c.ids...)
}

func TestFactoryTickHandlesRequestsBeforeTasks(t *testing.T) {
	h := newHarness(t)
	stopMe := h.task("PR-12301", registry.TaskActive, "investigate")
	other := h.task("PR-12302", registry.TaskActive, "investigate")
	var seen calls
	h.stage(config.FactoryStageAgents, func(_ context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		seen.add(t.ID, h.clock.Now())
		return false, nil
	})
	id, err := h.reg.EnqueueRequest(h.ctx, ReqStop, StopPayload{TaskID: stopMe.ID, Surface: "cli"})
	if err != nil {
		t.Fatal(err)
	}

	h.tick()

	req, err := h.reg.RequestByID(h.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if req.State != registry.RequestDone {
		t.Fatalf("stop request is %s (%s), want done", req.State, req.Result)
	}
	if got := h.reload(stopMe.ID).State; got != registry.TaskStopped {
		t.Fatalf("stopped task is %s", got)
	}
	if got := seen.list(); len(got) != 1 || got[0] != other.ID {
		t.Fatalf("the stage ran for %v, want only task %d: the stop came first", got, other.ID)
	}
	if got := h.rt.parked(); len(got) != 1 || got[0] != stopMe.ID {
		t.Fatalf("parked %v, want the stopped task's agents", got)
	}
}

func TestFactoryRunRefusesASecondDaemon(t *testing.T) {
	h := newHarness(t)
	if err := os.MkdirAll(h.layout.State(), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, held, err := engine.AcquireLock(h.layout.FactoryLock())
	if err != nil || held {
		t.Fatalf("lock: held %v, %v", held, err)
	}
	defer unlock()
	if err := os.WriteFile(h.layout.FactoryPid(), []byte("4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	procs := &fakeProcesses{table: map[int][2]string{4242: {"/opt/bin/magnum", "/opt/bin/magnum factory daemon"}}}
	h.e.procs = procs.processes()

	err = h.e.Run(h.ctx)
	if err == nil || !strings.Contains(err.Error(), "4242") {
		t.Fatalf("Run = %v, want an error naming pid 4242", err)
	}
	if b, _ := os.ReadFile(h.layout.FactoryPid()); strings.TrimSpace(string(b)) != "4242" {
		t.Fatalf("the refused daemon rewrote the pidfile: %q", b)
	}
}

func TestTheRunLoopTicksWritesItsPidAndRemovesIt(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12303", registry.TaskActive, "investigate")
	ticked := make(chan struct{}, 8)
	h.stage(config.FactoryStageAgents, func(_ context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		if t.ID == task.ID {
			select {
			case ticked <- struct{}{}:
			default:
			}
		}
		return false, nil
	})
	sigs := h.captureSignals()
	done := make(chan error, 1)
	go func() { done <- h.e.Run(h.ctx) }()
	select {
	case <-ticked:
	case <-time.After(10 * time.Second):
		t.Fatal("Run never ticked")
	}
	b, err := os.ReadFile(h.layout.FactoryPid())
	if err != nil || strings.TrimSpace(string(b)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("pidfile %q (%v), want this process", b, err)
	}
	(<-sigs) <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after SIGTERM = %v, want nil (launchd must not restart a clean stop)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after SIGTERM")
	}
	if _, err := os.Stat(h.layout.FactoryPid()); !os.IsNotExist(err) {
		t.Fatalf("the pidfile outlived the daemon: %v", err)
	}
}

// captureSignals replaces the engine's signal subscription: the channel Run
// subscribes arrives on the returned channel, for the test to send to.
func (h *harness) captureSignals() <-chan chan<- os.Signal {
	got := make(chan chan<- os.Signal, 1)
	h.e.notify = func(c chan<- os.Signal, _ ...os.Signal) { got <- c }
	h.e.stopNotify = func(chan<- os.Signal) {}
	return got
}

func TestAPassedStageMovesTheTaskToTheNextStage(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12304", registry.TaskActive, "investigate")
	h.stage(config.FactoryStageAgents, func(context.Context, registry.Task, config.FactoryStage) (bool, error) {
		return true, nil
	})

	h.tick()

	got := h.reload(task.ID)
	if got.Stage != "judge" || got.State != registry.TaskActive {
		t.Fatalf("after investigate passed the task is %s at %q, want active at judge", got.State, got.Stage)
	}
}

func TestATaskWithoutAStageStartsAtItsKindsFirstStage(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12305", registry.TaskActive, "")
	var seen []string
	h.stage(config.FactoryStageAgents, func(_ context.Context, _ registry.Task, st config.FactoryStage) (bool, error) {
		seen = append(seen, st.Name)
		return false, nil
	})

	h.tick()

	if len(seen) != 1 || seen[0] != "investigate" {
		t.Fatalf("ran stages %v, want investigate (the bug kind's first)", seen)
	}
	if got := h.reload(task.ID).Stage; got != "investigate" {
		t.Fatalf("stage %q, want investigate recorded", got)
	}
}

func TestObserveKeepsEachTasksEventsForItsNextAdvance(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12306", registry.TaskActive, "investigate")
	h.rt.events = []runtime.Event{{TaskID: task.ID, Role: "fx-cause", RunID: "r1", Kind: runtime.EventEnded}}
	h.rt.observeErr = errors.New("transcript: dial tcp: lookup api.example.com: no such host")

	h.e.observe(h.ctx)

	got := h.e.takeEvents(task.ID)
	if len(got) != 1 || got[0].RunID != "r1" || got[0].Kind != runtime.EventEnded {
		t.Fatalf("events %+v, want the ended run", got)
	}
	if again := h.e.takeEvents(task.ID); len(again) != 0 {
		t.Fatalf("events taken twice: %+v", again)
	}
	h.tick() // the failed read of the observe pass counts on the next tick
	if hd, held := h.e.holds[HoldInfra]; !held || hd.Reason != "DNS lookup failed" {
		t.Fatalf("hold %+v (%v), want a DNS hold from the observe pass", hd, held)
	}
}

func TestErrHeldIsNotAFailure(t *testing.T) {
	err := heldError{who: "disk", what: "9.0 GB free"}
	if !errors.Is(err, ErrHeld) || err.Error() != "disk: 9.0 GB free" {
		t.Fatalf("heldError %q is ErrHeld: %v", err.Error(), errors.Is(err, ErrHeld))
	}
}
