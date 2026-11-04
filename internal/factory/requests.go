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
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
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
)

// RetryPayload is a ReqRetry: task TaskID, which waits on you, goes on at
// its stage. Surface is where you asked, for the record of your input.
type RetryPayload struct {
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
		respond(e, ctx, q, e.startRequest)
	case ReqPreview:
		respond(e, ctx, q, e.previewRequest)
	case ReqAnswer:
		respond(e, ctx, q, e.answerRequest)
	case ReqDecide:
		respond(e, ctx, q, e.decideRequest)
	case ReqRetry:
		respond(e, ctx, q, e.retryTask)
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

// complete marks q done with result, or failed with err's text (redacted),
// and records it.
func (e *Engine) complete(ctx context.Context, q registry.Request, err error, result string) {
	state, phase := registry.RequestDone, registry.PhaseOK
	if err != nil {
		state, phase, result = registry.RequestFailed, registry.PhaseFail, execx.Redact(err.Error())
	}
	if cerr := e.reg.CompleteRequest(ctx, q.ID, state, result); cerr != nil {
		e.log.Warn("complete a factory request", "request", q.ID, "err", cerr)
	}
	e.event(ctx, 0, "request", phase, fmt.Sprintf("request %d (%s): %s", q.ID, q.Kind, result))
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

// endTask stops task t for good, because: its state moves to stopped and
// your input in is recorded, then its agents are parked, its open attempts
// end failed, its heavy slot is freed and its copy released (the branch
// stays): the copy it holds or a claim of it reserved, a claim that runs
// cancelled.
func (e *Engine) endTask(ctx context.Context, t registry.Task, because string, in registry.Input) error {
	if err := e.reg.TransitionTask(ctx, t.ID, stoppable, registry.TaskStopped, func(u *registry.TaskUpdate) {
		u.Set("waits_who", "")
		u.Set("waits_what", "")
		u.Because(because)
	}); err != nil {
		return err
	}
	if _, err := e.recordInput(ctx, in); err != nil {
		e.log.Warn("record why a task stopped", "task", t.ID, "kind", in.Kind, "err", err)
	}
	if e.d.Runtime != nil {
		if err := e.d.Runtime.Park(ctx, t); err != nil {
			e.event(ctx, t.ID, "park", registry.PhaseFail, err.Error())
		}
	}
	e.failOpenAttempts(ctx, t, because)
	e.freeHeavy(ctx, t, "its task stopped")
	// Whether or not the task names a copy: a claim that waited or failed
	// keeps the copy it reserved, and one that runs is cancelled and its copy
	// released once it ended (sweepClaims).
	e.releaseCopy(ctx, t, "stopped")
	delete(e.jiraTries, t.ID)
	delete(e.panics, t.ID)
	delete(e.inbox, t.ID)
	delete(e.agentRuns, t.ID)
	return nil
}

// startRequest answers a ReqStart (start): the task it made, or the task
// that holds the key (no failure: the work runs), or why it made none. A
// found key Jira refused for a cause of its own is named after the answer
// (the CLI has no preview to say so).
func (e *Engine) startRequest(ctx context.Context, p StartPayload) (string, error) {
	t, note, err := e.startNoted(ctx, p)
	switch {
	case errors.Is(err, ErrKeyRuns):
		return err.Error(), nil
	case err != nil && t.ID > 0: // made, and then a data file failed: the task waits on you
		return "", fmt.Errorf("made task %d, but %w", t.ID, err)
	case err != nil:
		return "", err
	}
	name := t.JiraKey
	if name == "" {
		name = strconv.Quote(t.Title)
	}
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

// previewRequest answers a ReqPreview with the JSON of the new-task form
// (preview).
func (e *Engine) previewRequest(ctx context.Context, p PreviewPayload) (string, error) {
	f, err := e.preview(ctx, p.Text)
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
// meanwhile.
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
	delete(e.agentRuns, t.ID)
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
// again.
func (e *Engine) retryTask(ctx context.Context, p RetryPayload) (string, error) {
	t, err := e.reg.TaskByID(ctx, p.TaskID)
	if err != nil {
		return "", err
	}
	if t.State != registry.TaskNeedsYou {
		return "", fmt.Errorf("%s is %s, not waiting on you: there is nothing to retry", TaskKey(t), t.State)
	}
	open, err := e.openQuestions(ctx, t.ID)
	if err != nil {
		return "", err
	}
	if len(open) > 0 { // asks.json only says whom the open questions wait on
		asks, err := e.readAsks(t.ID)
		if err != nil {
			return "", err
		}
		if state, _, _ := e.questionsWait(t, open, asks); state == registry.TaskNeedsYou {
			return "", fmt.Errorf("%s waits for your answers to %s: answer them with `magnum factory answer %d` (it prints them); "+
				"a retry would only ask them again", TaskKey(t), plural(len(open), "question"), t.ID)
		}
	}
	if err := e.reg.TransitionTask(ctx, t.ID, []string{registry.TaskNeedsYou}, registry.TaskActive, func(u *registry.TaskUpdate) {
		u.Set("waits_who", "")
		u.Set("waits_what", "")
		u.Because("you let it go on")
	}); err != nil {
		return "", err
	}
	if _, err := e.recordInput(ctx, registry.Input{TaskID: t.ID, Kind: registry.InputRetry, Surface: p.Surface, Stage: t.Stage}); err != nil {
		e.log.Warn("record a retry", "task", t.ID, "err", err)
	}
	delete(e.panics, t.ID)
	delete(e.lastFail, t.ID)
	e.checksMu.Lock()
	delete(e.checkFails, t.ID)
	e.checksMu.Unlock()
	return fmt.Sprintf("%s goes on at %s", TaskKey(t), cmp.Or(t.Stage, "its first stage")), nil
}
