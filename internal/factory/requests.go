package factory

// The CLI's and the screen's requests (the factory registry's requests
// table): each tick answers the pending ones before it advances any task,
// so an answer, a decision or a stop takes effect in the same tick. A
// payload decodes with unknown fields refused (a newer CLI's field must not
// be dropped), a request that waited longer than requestTTL fails as
// expired, and a handler that panics fails its request, never the daemon.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
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
)

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
		answer(e, ctx, q, e.stopTask)
	case ReqStart, ReqAnswer, ReqDecide, ReqPreview:
		e.complete(ctx, q, fmt.Errorf("this factory daemon cannot %s a task yet: update magnum (`make build`), then `magnum factory install`", q.Kind), "")
	default:
		e.complete(ctx, q, fmt.Errorf("unknown request kind %q", q.Kind), "")
	}
}

// answer completes request q with fn's result on its decoded payload.
func answer[T any](e *Engine, ctx context.Context, q registry.Request, fn func(context.Context, T) (string, error)) {
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

// stopTask stops a task for good: its state moves to stopped and your input
// is recorded, then its agents are parked, its open attempts end failed,
// its heavy slot is freed and its copy released (the branch stays).
func (e *Engine) stopTask(ctx context.Context, p StopPayload) (string, error) {
	t, err := e.reg.TaskByID(ctx, p.TaskID)
	if err != nil {
		return "", err
	}
	if t.State == registry.TaskDone || t.State == registry.TaskStopped {
		return fmt.Sprintf("%s is %s already", TaskKey(t), t.State), nil
	}
	if err := e.reg.TransitionTask(ctx, t.ID, stoppable, registry.TaskStopped, func(u *registry.TaskUpdate) {
		u.Set("waits_who", "")
		u.Set("waits_what", "")
		u.Because("stopped by you")
	}); err != nil {
		return "", err
	}
	if _, err := e.reg.RecordInput(ctx, registry.Input{TaskID: t.ID, Kind: registry.InputStop, Surface: p.Surface, Stage: t.Stage}); err != nil {
		e.log.Warn("record a stop", "task", t.ID, "err", err)
	}
	if e.d.Runtime != nil {
		if err := e.d.Runtime.Park(ctx, t); err != nil {
			e.event(ctx, t.ID, "park", registry.PhaseFail, err.Error())
		}
	}
	if as, err := e.reg.Attempts(ctx, t.ID); err == nil {
		for _, a := range as {
			switch a.State {
			case registry.AttemptLaunching, registry.AttemptRunning, registry.AttemptEnded:
				if err := e.reg.TransitionAttempt(ctx, a.ID, []string{a.State}, registry.AttemptFailed, func(u *registry.AttemptUpdate) {
					u.Set("error", "stopped by you")
				}); err != nil {
					e.log.Warn("end a stopped task's attempt", "attempt", a.ID, "err", err)
				}
			}
		}
	}
	e.releaseHeavy(t.ID, true)
	if s, ok := e.servers[t.ID]; ok {
		e.stopServer(ctx, t, s)
	}
	if e.d.Copies != nil && t.Copy != "" {
		if err := e.d.Copies.Release(ctx, t, "stopped"); err != nil {
			e.event(ctx, t.ID, "release", registry.PhaseFail, err.Error())
		}
	}
	delete(e.panics, t.ID)
	delete(e.inbox, t.ID)
	return "stopped " + TaskKey(t), nil
}
