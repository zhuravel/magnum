package factory

// The talk request (ReqTalk): you want to talk with a task's liaison, the
// agent that is the task's front door (prompts/factory-liaison.md). The
// daemon renders the liaison's prompt with the task's data and hands it to
// Runtime.Talk, which starts the liaison or finds it, resumes its
// conversation and sends the first prompt to a conversation that never had
// one. Talk starts an agent and prompts it, which takes up to minutes where
// every other request is a registry write, so it runs off the loop, in a
// goroutine of its own on the engine's context as a claim does (claim.go),
// and the request completes when Talk ends: the tick that begins it ends at
// once and the other requests are answered in it. A drain cancels a Talk
// that runs and leaves its request pending: the next daemon talks again,
// and Talk finds what the first one started.

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/textx"
)

// talkRequest begins request q's talk off the loop. A task has one talk at
// a time: a second request about it stays pending until the first ended
// (it then finds the liaison the first started). A draining daemon starts
// no agent, so its requests stay pending for the next daemon.
func (e *Engine) talkRequest(ctx context.Context, q registry.Request) {
	var p TalkPayload
	if err := decode(q, &p); err != nil {
		e.complete(ctx, q, err, "")
		return
	}
	if e.draining.Load() || !e.beginTalk(p.TaskID) {
		return
	}
	// The pending requests were read at the start of the tick: the talk of
	// an earlier tick may have answered this one since.
	if cur, err := e.reg.RequestByID(ctx, q.ID); err == nil && cur.State != registry.RequestPending {
		e.endTalk(p.TaskID)
		return
	}
	e.bg.Go(func() {
		defer e.endTalk(p.TaskID) // after the request is answered: no tick sees it pending without the mark
		e.runTalk(q, p)
	})
}

// beginTalk marks a talk of task id as running, false when one already is.
func (e *Engine) beginTalk(id int64) bool {
	e.talkMu.Lock()
	defer e.talkMu.Unlock()
	if e.talking[id] {
		return false
	}
	e.talking[id] = true
	return true
}

// endTalk ends the mark beginTalk made.
func (e *Engine) endTalk(id int64) {
	e.talkMu.Lock()
	defer e.talkMu.Unlock()
	delete(e.talking, id)
}

// runTalk is the talk of request q on the engine's context: it completes q
// with the talk's answer, or fails it. A Talk the daemon's stop cut leaves q
// pending, and a panic in it fails q, never the daemon.
func (e *Engine) runTalk(q registry.Request, p TalkPayload) {
	ctx := e.bgCtx
	defer func() {
		if r := recover(); r != nil {
			msg := textx.Clip(strings.Join(strings.Fields(execx.Redact(fmt.Sprint(r))), " "), panicRunes)
			e.log.Error("panic in a talk; the daemon goes on", "request", q.ID, "task", p.TaskID, "panic", msg,
				"stack", execx.Redact(string(debug.Stack())))
			e.complete(context.WithoutCancel(ctx), q, fmt.Errorf("the factory daemon failed on this request: %s", msg), "")
		}
	}()
	res, err := e.talkTask(ctx, p)
	if ctx.Err() != nil {
		e.log.Info("the daemon stopped a talk; its request stays pending for the next daemon", "request", q.ID, "task", p.TaskID)
		return
	}
	e.complete(ctx, q, err, res)
}

// talkEnded reports whether t has ended: it has no liaison to talk with.
func talkEnded(t registry.Task) bool {
	return t.State == registry.TaskDone || t.State == registry.TaskStopped || t.State == registry.TaskFailed
}

// talkTask is your talk (ReqTalk): the liaison of task p.TaskID, started or
// found, with the liaison prompt rendered for the task as the stages render
// theirs (liaisonPrompt), handed to Runtime.Talk. It answers where you find
// the liaison. A task that ended is refused, naming its state.
//
// A Talk that errs with a pane is no failure: the liaison is live in the
// pane, only its first prompt waits (you typed into its pane within the
// human cooldown, or herdr refused the prompt), and the next talk sends it.
// A task that ended while Talk ran has its liaison parked again, since the
// park that came with its end found the Talk at work and left the liaison
// (Runtime.Park).
func (e *Engine) talkTask(ctx context.Context, p TalkPayload) (string, error) {
	t, err := e.reg.TaskByID(ctx, p.TaskID)
	if err != nil {
		return "", err
	}
	key := TaskKey(t)
	if talkEnded(t) {
		return "", fmt.Errorf("%s is %s: a task that ended has no liaison to talk with", key, t.State)
	}
	if e.d.Runtime == nil {
		return "", errors.New("this factory daemon runs no agents")
	}
	prompt, err := e.liaisonPrompt(t)
	if err != nil {
		return "", err
	}
	e.log.Info("a task's liaison is asked for", "task", t.ID, "surface", p.Surface)
	pane, talkErr := e.d.Runtime.Talk(ctx, t, prompt)
	if now, err := e.reg.TaskByID(context.WithoutCancel(ctx), t.ID); err == nil && talkEnded(now) {
		if err := e.d.Runtime.Park(context.WithoutCancel(ctx), now); err != nil {
			e.event(ctx, t.ID, "park", registry.PhaseFail, "the liaison of a task that ended while it started: "+err.Error())
		}
		return "", fmt.Errorf("%s is %s now: its liaison is parked again", key, now.State)
	}
	where := fmt.Sprintf("the liaison of %s talks in pane %s of the factory's herdr session", key, pane)
	switch {
	case talkErr == nil:
		return where, nil
	case pane == "":
		return "", talkErr
	case errors.Is(talkErr, agents.ErrHumanActive):
		return where + "; its first prompt waits: you typed into its pane within human_cooldown; the next talk sends it", nil
	}
	why := textx.Clip(strings.Join(strings.Fields(execx.Redact(textx.FirstLine(talkErr.Error()))), " "), panicRunes)
	return where + "; its first prompt waits, and the next talk sends it: " + why, nil
}

// liaisonPrompt is the liaison prompt for task t, rendered as a stage renders
// its roles' with the data of the task alone: the liaison runs outside any
// stage, in the task's folder, so no stage and no attempt come in, and the
// copy only when the task holds one (the prompt describes it then).
func (e *Engine) liaisonPrompt(t registry.Task) (string, error) {
	spec, ok := e.cfg.FactoryRole(runtime.LiaisonRole)
	if !ok {
		return "", fmt.Errorf("the liaison of %s: no factory role %s", TaskKey(t), runtime.LiaisonRole)
	}
	runAs, err := e.d.Runtime.RunsAs(t, spec)
	if err != nil {
		return "", fmt.Errorf("the liaison of %s: %w", TaskKey(t), err)
	}
	c, _ := e.heldCopy(t.ID)
	return e.renderRole(t, config.FactoryStage{}, spec, runAs, c)
}
