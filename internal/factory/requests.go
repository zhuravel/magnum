package factory

// The CLI's and the screen's requests (the factory registry's requests
// table): each tick answers the pending ones before it advances any task,
// so an answer, a decision or a stop takes effect in the same tick. A
// payload decodes with unknown fields refused (a newer CLI's field must not
// be dropped), a request that waited longer than requestTTL fails as
// expired, and a handler that panics fails its request, never the daemon.

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/textx"
)

// Request kinds.
const (
	ReqStart   = "start"
	ReqAnswer  = "answer"
	ReqDecide  = "decide"
	ReqStop    = "stop"
	ReqPreview = "preview"
	ReqLetGo   = "let_go"
	ReqRetry   = "retry"
	ReqFocus   = "focus"
	ReqTalk    = "talk"
	ReqRate    = "rate"
)

// FocusPayload is a ReqFocus: the factory screen of process PID gained
// (Focused) or lost its terminal's focus at At. It carries no words and is
// no input of yours.
type FocusPayload struct {
	Focused bool
	PID     int
	At      time.Time
}

// Live reports whether f is the focus of a screen that still runs: it is
// focused, and its process lives as a magnum process (a pid reused by
// another program does not count). A screen that ended while focused
// holds nothing.
func (f FocusPayload) Live() bool {
	return f.Focused && f.PID > 0 && engine.DrainerAlive(f.PID)
}

// focusRequest keeps the screen's focus report p in focus.json, where the
// delivery of the alerts reads it (ReadScreenFocus), so a daemon restart
// keeps it: the latest report wins, and an older one that arrives late
// changes nothing.
func (e *Engine) focusRequest(_ context.Context, p FocusPayload) (string, error) {
	if p.PID <= 0 || p.At.IsZero() {
		return "", errors.New("a focus report names the screen's pid and its time")
	}
	if cur, err := ReadScreenFocus(e.d.Layout); err == nil && cur.At.After(p.At) {
		return "kept the newer focus report", nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(e.d.Layout.FactoryFocus()), 0o700); err != nil {
		return "", err
	}
	if err := fsx.WriteFileAtomic(e.d.Layout.FactoryFocus(), append(b, '\n'), 0o600); err != nil {
		return "", err
	}
	if p.Focused {
		return "the screen has focus", nil
	}
	return "the screen lost focus", nil
}

// RetryPayload is a ReqRetry: task TaskID, which waits on you, goes on at
// its stage. Surface is where you asked, for the record of your input.
type RetryPayload struct {
	TaskID  int64
	Surface string
	// AcceptQA is your acceptance of what QA's newest run left you at the
	// task's head (`magnum factory retry --accept-qa`): the defects that
	// block which the writer declined, its fixes or declines the run did not
	// walk again, or the states a walk that reached its pages could not
	// reach. The next QA run there lets the defects pass, the task goes on
	// at once with the states flagged, and the PR body names each as
	// accepted by you.
	AcceptQA bool
}

// TalkPayload is a ReqTalk: you want to talk with the liaison of task
// TaskID. Surface is where you asked, for the record.
type TalkPayload struct {
	TaskID  int64
	Surface string
}

// LetGoPayload is a ReqLetGo: you looked at the pane of task TaskID's Role,
// whose last prompt may or may not have arrived, and let the prompt go.
type LetGoPayload struct {
	TaskID        int64
	Role, Surface string
}

const (
	// requestTTL is how long a request may wait for the daemon; an older
	// one fails as expired: whoever sent it gave up long ago, and what it
	// asked may no longer fit.
	requestTTL = 30 * time.Minute
	// maxRequestsPerTick bounds the requests one tick answers.
	maxRequestsPerTick = 32
)

// StopPayload is a ReqStop: stop task TaskID for good. Surface is where you
// asked (the screen, the CLI), for the record of your input.
type StopPayload struct {
	TaskID  int64
	Surface string
}

// handleRequests answers the pending requests, oldest first.
func (e *Engine) handleRequests(ctx context.Context) {
	reqs, err := e.reg.PendingRequests(ctx, maxRequestsPerTick)
	if err != nil {
		e.log.Warn("factory requests", "err", err)
		return
	}
	now := e.now()
	for _, q := range reqs {
		if age := now.Sub(q.Created); age > requestTTL {
			e.complete(ctx, q, fmt.Errorf("expired after %s", shortAge(age)), "")
			continue
		}
		e.handleRequest(ctx, q)
	}
}

// handleRequest answers one request; a panic in its handler fails it.
func (e *Engine) handleRequest(ctx context.Context, q registry.Request) {
	defer func() {
		if r := recover(); r != nil {
			msg := textx.Clip(strings.Join(strings.Fields(execx.Redact(fmt.Sprint(r))), " "), panicRunes)
			e.log.Error("panic in a request handler; the daemon goes on", "request", q.ID, "kind", q.Kind, "panic", msg,
				"stack", execx.Redact(string(debug.Stack())))
			e.complete(context.WithoutCancel(ctx), q, fmt.Errorf("the factory daemon failed on this request: %s", msg), "")
		}
	}()
	switch q.Kind {
	case ReqStop:
		respond(e, ctx, q, e.stopTask)
	case ReqLetGo:
		respond(e, ctx, q, e.letGo)
	case ReqStart:
		respond(e, ctx, q, func(ctx context.Context, p StartPayload) (string, error) { return e.startRequest(ctx, q.ID, p) })
	case ReqPreview:
		respond(e, ctx, q, e.previewRequest)
	case ReqAnswer:
		respond(e, ctx, q, e.answerRequest)
	case ReqDecide:
		respond(e, ctx, q, e.decideRequest)
	case ReqRetry:
		respond(e, ctx, q, e.retryTask)
	case ReqFocus:
		respond(e, ctx, q, e.focusRequest)
	case ReqRate:
		respond(e, ctx, q, e.rateTask)
	case ReqTalk:
		e.talkRequest(ctx, q)
	default:
		e.complete(ctx, q, fmt.Errorf("unknown request kind %q", q.Kind), "")
	}
}

// respond completes request q with fn's result on its decoded payload.
func respond[T any](e *Engine, ctx context.Context, q registry.Request, fn func(context.Context, T) (string, error)) {
	var p T
	if err := decode(q, &p); err != nil {
		e.complete(ctx, q, err, "")
		return
	}
	res, err := fn(ctx, p)
	e.complete(ctx, q, err, res)
}

// decode reads q's payload into v, refusing a field v does not know: it
// comes from a newer CLI, and running without it could do what it was sent
// to prevent.
func decode(q registry.Request, v any) error {
	if len(q.Payload) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(q.Payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			return fmt.Errorf("request %d (%s): this factory daemon predates %s: `magnum factory install` restarts it on this build, then retry", q.ID, q.Kind, field)
		}
		return fmt.Errorf("request %d (%s): bad payload: %w", q.ID, q.Kind, err)
	}
	return nil
}

// wordsKinds are the requests that carry your words (a preview's draft, a
// start's text, an answer, a decision's note): their payload is emptied
// once answered, since only the inputs table keeps your words, scrubbed.
var wordsKinds = []string{ReqPreview, ReqStart, ReqAnswer, ReqDecide}

// complete marks q done with result, or failed with err's text (redacted),
// and records it; the payload of a request of your words goes (wordsKinds).
func (e *Engine) complete(ctx context.Context, q registry.Request, err error, result string) {
	state, phase := registry.RequestDone, registry.PhaseOK
	if err != nil {
		state, phase, result = registry.RequestFailed, registry.PhaseFail, execx.Redact(err.Error())
	}
	if cerr := e.reg.CompleteRequest(ctx, q.ID, state, result); cerr != nil {
		e.log.Warn("complete a factory request", "request", q.ID, "err", cerr)
	} else if slices.Contains(wordsKinds, q.Kind) { // your words as typed, unscrubbed: the inputs keep the scrubbed copy
		if ferr := e.reg.ForgetPayload(ctx, q.ID); ferr != nil {
			e.log.Warn("forget a request's words", "request", q.ID, "kind", q.Kind, "err", ferr)
		}
	}
	text := result
	if q.Kind == ReqPreview && err == nil { // the form carries your draft: the event says only that it was answered
		text = "the new task's form, answered"
	}
	e.event(ctx, 0, "request", phase, fmt.Sprintf("request %d (%s): %s", q.ID, q.Kind, text))
}

// The screen's own requests (its focus reports, the previews of what you
// type) are kept screenRequestsKept after their answer, with their events,
// and pruned at most every pruneEvery: they are no input of yours, and
// they would grow the registry with every focus change and every pause in
// typing.
const (
	screenRequestsKept = 24 * time.Hour
	pruneEvery         = time.Hour
)

// pruneScreenRequests prunes the screen's own requests answered more than
// screenRequestsKept before now, and their events, once per pruneEvery.
func (e *Engine) pruneScreenRequests(ctx context.Context, now time.Time) {
	if !e.pruned.IsZero() && now.Sub(e.pruned) < pruneEvery {
		return
	}
	e.pruned = now
	n, err := e.reg.PruneRequests(ctx, now.Add(-screenRequestsKept), ReqFocus, ReqPreview)
	switch {
	case err != nil:
		e.log.Warn("prune the screen's requests", "err", err)
	case n > 0:
		e.log.Info("pruned the screen's requests", "requests", n)
	}
}

// shortAge is d as a wait says it: "31m", "2h5m", "45s".
func shortAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d%time.Hour < time.Minute:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dh%dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
}

// stoppable are the states a task can be stopped from: every one but done
// and stopped.
var stoppable = []string{registry.TaskQueued, registry.TaskActive, registry.TaskNeedsYou, registry.TaskWaiting,
	registry.TaskPublished, registry.TaskFailed}

// stopTask stops a task for good (endTask), with your stop as its input.
func (e *Engine) stopTask(ctx context.Context, p StopPayload) (string, error) {
	t, err := e.reg.TaskByID(ctx, p.TaskID)
	if err != nil {
		return "", err
	}
	if t.State == registry.TaskDone || t.State == registry.TaskStopped {
		return fmt.Sprintf("%s is %s already", TaskKey(t), t.State), nil
	}
	if err := e.endTask(ctx, t, "stopped by you", registry.Input{TaskID: t.ID, Kind: registry.InputStop, Surface: p.Surface, Stage: t.Stage}); err != nil {
		return "", err
	}
	return "stopped " + TaskKey(t), nil
}

// endTask stops task t for good, because, with your input in recorded
// (closeTask): the branch stays.
func (e *Engine) endTask(ctx context.Context, t registry.Task, because string, in registry.Input) error {
	return e.closeTask(ctx, t, taskEnd{from: stoppable, to: registry.TaskStopped, because: because, in: &in})
}

// taskEnd is how a task ends (closeTask): from one of the states from, to
// stopped (you stopped or dropped it) or done (its PR merged), because,
// with your input that ended it (nil: you did not, as for a merge).
type taskEnd struct {
	from    []string
	to      string
	because string
	in      *registry.Input
}

// closeTask ends task t as end says: its state moves (compare-and-set from
// end.from) with its wait cleared and your input recorded when you ended
// it, then its agents are parked (parkEnded), its open attempts end failed,
// its heavy slot is freed and its copy released (the branch stays): the
// copy it holds or a claim of it reserved, a claim that runs cancelled. A
// stop and a merge share it.
func (e *Engine) closeTask(ctx context.Context, t registry.Task, end taskEnd) error {
	if err := e.reg.TransitionTask(ctx, t.ID, end.from, end.to, func(u *registry.TaskUpdate) {
		u.Set("waits_who", "")
		u.Set("waits_what", "")
		u.Because(end.because)
	}); err != nil {
		return err
	}
	if end.in != nil {
		if _, err := e.recordInput(ctx, *end.in); err != nil {
			e.log.Warn("record why a task ended", "task", t.ID, "kind", end.in.Kind, "err", err)
		}
	}
	e.parkEnded(ctx, t)
	e.failOpenAttempts(ctx, t, end.because)
	e.freeHeavy(ctx, t, "its task "+end.to)
	// Whether or not the task names a copy: a claim that waited or failed
	// keeps the copy it reserved, and one that runs is cancelled and its copy
	// released once it ended (sweepClaims).
	e.releaseCopy(ctx, t, end.to)
	delete(e.jiraTries, t.ID)
	delete(e.panics, t.ID)
	delete(e.lastFail, t.ID)
	delete(e.publishFails, t.ID)
	delete(e.inbox, t.ID)
	delete(e.agentRuns, t.ID)
	return nil
}

// parkEnded parks the agents of task t, which ended; while herdr is held
// (down) the park is owed to the next tick with herdr up (payParkDebts),
// so a merge the tick follows without herdr still parks them. A park that
// fails is an event of the task.
func (e *Engine) parkEnded(ctx context.Context, t registry.Task) {
	if e.d.Runtime == nil {
		return
	}
	if _, down := e.holds[HoldHerdr]; down {
		e.parkOwed[t.ID] = true
		return
	}
	if err := e.d.Runtime.Park(ctx, t); err != nil {
		e.event(ctx, t.ID, "park", registry.PhaseFail, err.Error())
	}
}

// payParkDebts parks, once herdr is up, the agents of the tasks that ended
// while it was down (parkEnded): one try each, a failure an event of the
// task, as a stop's own park.
func (e *Engine) payParkDebts(ctx context.Context) {
	if e.d.Runtime == nil {
		return
	}
	for id := range e.parkOwed {
		delete(e.parkOwed, id)
		t, err := e.reg.TaskByID(ctx, id)
		if err != nil {
			e.log.Warn("park an ended task's agents now herdr is up: read the task", "task", id, "err", err)
			continue
		}
		if err := e.d.Runtime.Park(ctx, t); err != nil {
			e.event(ctx, t.ID, "park", registry.PhaseFail, err.Error())
		}
	}
}

// startRequest answers a ReqStart (start): the task it made, or the task
// that holds the key (no failure: the work runs), or why it made none. A
// found key Jira refused for a cause of its own is named after the answer
// (the CLI has no preview to say so). The task records request id, so a
// request handled again after a kill that came before it completed (a
// drain past launchd's ExitTimeOut) answers with the task it made then and
// makes none.
func (e *Engine) startRequest(ctx context.Context, id int64, p StartPayload) (string, error) {
	if t, err := e.reg.TaskByRequest(ctx, id); err == nil {
		return fmt.Sprintf("made %s as task %d before the factory daemon restarted: it is %s", startName(t), t.ID, t.State), nil
	} else if !errors.Is(err, registry.ErrNotFound) {
		return "", err
	}
	p.request = id
	t, note, err := e.startNoted(ctx, p)
	switch {
	case errors.Is(err, ErrKeyRuns):
		return err.Error(), nil
	case err != nil && t.ID > 0: // made, and then a data file failed: the task waits on you
		return "", fmt.Errorf("made task %d, but %w", t.ID, err)
	case err != nil:
		return "", err
	}
	name := startName(t)
	var answer string
	switch {
	case t.State == registry.TaskWaiting && t.WaitsWho == waitJira:
		answer = fmt.Sprintf("%s waits as task %d: Jira does not answer; it starts when Jira answers", name, t.ID)
	case t.State == registry.TaskQueued:
		answer = fmt.Sprintf("queued %s as task %d: %s: %s", name, t.ID, t.WaitsWho, t.WaitsWhat)
	default:
		answer = fmt.Sprintf("started %s as task %d: %s in %s on %s", name, t.ID, t.Kind, t.Repo, t.Branch)
	}
	if note != "" {
		answer += ". " + note
	}
	return answer, nil
}

// startName is how the answer to a start names task t: its key, or its
// title quoted.
func startName(t registry.Task) string {
	if t.JiraKey != "" {
		return t.JiraKey
	}
	return strconv.Quote(t.Title)
}

// previewRequest answers a ReqPreview with the JSON of the new-task form
// (previewIn).
func (e *Engine) previewRequest(ctx context.Context, p PreviewPayload) (string, error) {
	f, err := e.previewIn(ctx, p.Text, p.Repo)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// letGo is your one action on a prompt of unknown delivery: you looked at
// the pane of the task's role, and the prompt is let go (Runtime.LetGo:
// never sent again). The stage rebuilds its roles from the ledger on its
// next pass, so a role whose let-go prompt was its turn's waits on you with
// a plain reason (recheckFailed). A task that waited on you for unknown
// deliveries goes on once none of them is left, and waits on the others
// meanwhile. It ends the wait after a failure of the task's stage
// (retryLater), as a retry does: the tick it kicks runs the stage.
func (e *Engine) letGo(ctx context.Context, p LetGoPayload) (string, error) {
	t, err := e.reg.TaskByID(ctx, p.TaskID)
	if err != nil {
		return "", err
	}
	if e.d.Runtime == nil {
		return "", fmt.Errorf("this factory daemon runs no agents")
	}
	n, err := e.d.Runtime.LetGo(ctx, t, p.Role)
	if err != nil {
		return "", err
	}
	if n == 0 { // a role mistyped, or a read settled the prompt since you looked
		return "", fmt.Errorf("%s has no prompt of unknown delivery to %s: there is nothing to let go", TaskKey(t), p.Role)
	}
	delete(e.agentRuns, t.ID)
	delete(e.lastFail, t.ID)
	if _, err := e.recordInput(ctx, registry.Input{TaskID: t.ID, Kind: registry.InputRetry, Surface: p.Surface,
		Stage: t.Stage, Role: p.Role}); err != nil {
		e.log.Warn("record a let-go", "task", t.ID, "err", err)
	}
	if t.State == registry.TaskNeedsYou { // a live task's stage settles its own wait
		if err := e.settleUnknown(ctx, t, "you let the prompt to "+p.Role+" go", false); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("let %d prompt(s) to %s go", n, p.Role), nil
}

// retryTask is your retry (ReqRetry): task p.TaskID, which waits on you,
// goes on at its stage, and your input is recorded. Nothing is skipped: the
// stage's own guard and checks run again on its next pass (a role's file
// checked again, recheckFailed; a fresh local loop after the one that
// stopped for you, toldOfStop; what a check kept for you settled first,
// settleKept; the publish's checks before any step), and the counts that
// sent the task to you (its panics, its checks that did not run to their
// end) start again. A retry sends no prompt of unknown delivery (its role
// stays held until a read settles it or you let it go) and never lifts
// Codex-never. A task that does not wait on you is refused, and so is one
// whose questions wait for your answers: its stage would only ask them
// again. With p.AcceptQA it accepts what QA's newest run left you at the
// task's head (acceptQA): the defects the writer declined, its fixes or
// declines the run did not walk again, or the states it could not reach;
// it is refused when QA left you none of them. Your retry's record (an
// input) is what gives a writer at its prompt cap more fix turns
// (fixBudgetOf) and starts the runtime's ladders over: a record that fails
// to write still lets the task go on, and the answer says so
// (retryUnrecorded).
func (e *Engine) retryTask(ctx context.Context, p RetryPayload) (string, error) {
	t, err := e.reg.TaskByID(ctx, p.TaskID)
	if err != nil {
		return "", err
	}
	if err := retryRefusal(ctx, e.reg, e.d.Layout, t); err != nil {
		return "", err
	}
	same := e.unchangedSince(ctx, t)
	var undo func() // your acceptance goes back when the task does not go on
	if p.AcceptQA {
		accepted, u, err := e.acceptQA(ctx, t, p.Surface)
		if err != nil {
			return "", err
		}
		same, undo = same+accepted, u
	}
	if err := e.reg.TransitionTask(ctx, t.ID, []string{registry.TaskNeedsYou}, registry.TaskActive, func(u *registry.TaskUpdate) {
		u.Set("waits_who", "")
		u.Set("waits_what", "")
		u.Because("you let it go on")
	}); err != nil {
		if undo != nil {
			undo()
		}
		return "", err
	}
	if _, err := e.recordInput(ctx, registry.Input{TaskID: t.ID, Kind: registry.InputRetry, Surface: p.Surface, Stage: t.Stage}); err != nil {
		e.log.Warn("record a retry", "task", t.ID, "err", err)
		same += "; " + retryUnrecorded
	}
	delete(e.panics, t.ID)
	delete(e.lastFail, t.ID)
	delete(e.publishFails, t.ID)
	// The stage is rebuilt from the registry and the agents store, as after a
	// restart: what the loop knew of it is what sent the task to you (a role
	// stuck with no turn at work, which the rebuild hands the runtime to
	// recover, agentsRunOf).
	delete(e.agentRuns, t.ID)
	e.checksMu.Lock()
	delete(e.checkFails, t.ID)
	e.checksMu.Unlock()
	return fmt.Sprintf("%s goes on at %s", TaskKey(t), cmp.Or(t.Stage, "its first stage")) + same, nil
}

// retryUnrecorded is what the answer to your retry adds when its record
// failed to write: the task goes on as if you had not retried at the cap.
const retryUnrecorded = "your retry was not recorded: the writer gets no more fix turns; retry again"

// unchangedSince is what your retry of task t adds when the precheck's
// failure for you it waits on (precheck.json) would only come again:
// nothing changed since (the branch's head, the precheck commands and
// their environment, checksPrint), with the line the command printed and
// that an edit of the configuration or the PATH needs the daemon's restart
// first (the daemon reads both at its start); ""
// when something changed, or the task waits on you for another cause, or
// any of it cannot be read. It refuses nothing: a cause of the moment may
// be gone.
func (e *Engine) unchangedSince(ctx context.Context, t registry.Task) string {
	var rec precheckRecord
	if b, err := os.ReadFile(filepath.Join(e.d.Layout.FactoryTaskDir(t.ID), precheckRecordFile)); err != nil ||
		json.Unmarshal(b, &rec) != nil {
		return ""
	}
	if rec.Passed || !rec.ForYou || rec.Commands == "" || !strings.Contains(t.WaitsWhat, rec.Why) {
		return ""
	}
	repo, ok := e.cfg.FactoryRepo(t.Repo)
	if !ok || e.d.Git == nil || t.Branch == "" {
		return ""
	}
	if head, err := e.d.Git.RevParse(ctx, repo.MainClone, "refs/heads/"+t.Branch); err != nil || head != rec.Head {
		return ""
	}
	if commands, env := e.checksPrint(repo); commands != rec.Commands || env != rec.Env {
		return ""
	}
	same := ", though nothing the factory sees changed since its last failure (the same head, precheck commands and environment)"
	if rec.Line != "" {
		same += ": " + rec.Line
	}
	return same + "; an edit of the configuration or of your shell's PATH reaches the checks only after `magnum factory install`"
}

// retryRefusal is the guard of a retry of task t, as retryTask and the
// screen's task card (its r) both ask it: nil when a retry goes on, else
// why not: t does not wait on you, or its questions wait for your answers
// (a retry would only ask them again; asks.json in t's directory of
// layout l says which open questions wait on someone you asked instead). A
// registry or a file that cannot be read is its error.
func retryRefusal(ctx context.Context, reg *registry.Registry, l paths.Layout, t registry.Task) error {
	if t.State != registry.TaskNeedsYou {
		return retryRefusalOf(t, nil, nil)
	}
	open, err := openQuestionsOf(ctx, reg, t)
	if err != nil || len(open) == 0 {
		return err
	}
	asks, err := readAsksIn(l.FactoryTaskDir(t.ID)) // asks.json only says whom the open questions wait on
	if err != nil {
		return err
	}
	return retryRefusalOf(t, open, asks)
}

// retryRefusalOf is retryRefusal's verdict on what it reads: task t, the
// open questions of its newest revision and asks.json's asks (the screen
// passes its own reads of them).
func retryRefusalOf(t registry.Task, open []registry.Question, asks map[string]ask) error {
	switch {
	case t.State != registry.TaskNeedsYou:
		return fmt.Errorf("%s is %s, not waiting on you: there is nothing to retry", TaskKey(t), t.State)
	case len(open) > 0 && questionsOnYou(open, asks):
		return fmt.Errorf("%s waits for your answers to %s: answer them with `magnum factory answer %d` (it prints them); "+
			"a retry would only ask them again", TaskKey(t), plural(len(open), "question"), t.ID)
	}
	return nil
}
