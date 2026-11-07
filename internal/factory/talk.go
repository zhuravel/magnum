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
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// talkRequest begins request q's talk off the loop. A task has one talk at
// a time: a second request about it stays pending until the first ended
// (it then finds the liaison the first started), and the record of the
// work a restart would cut names it while it runs. A draining daemon (a
// stop's drain, or install's) starts no agent, so its requests stay
// pending for the next daemon, or until the drain is lifted.
func (e *Engine) talkRequest(ctx context.Context, q registry.Request) {
	var p TalkPayload
	if err := decode(q, &p); err != nil {
		e.complete(ctx, q, err, "")
		return
	}
	if e.startsHeld() || !e.beginTalk(p.TaskID) {
		return
	}
	// The pending requests were read at the start of the tick: the talk of
	// an earlier tick may have answered this one since.
	if cur, err := e.reg.RequestByID(ctx, q.ID); err == nil && cur.State != registry.RequestPending {
		e.endTalk(p.TaskID)
		return
	}
	key := fmt.Sprintf("task %d", p.TaskID)
	if t, err := e.reg.TaskByID(ctx, p.TaskID); err == nil {
		key = TaskKey(t)
	}
	endWork := e.beginWork(WorkTalk, p.TaskID, "talk with the liaison of "+key)
	e.bg.Go(func() {
		defer endWork()
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
// (Runtime.Park). A liaison this park leaves as well (its first turn in
// flight), or whose task the read after Talk could not tell, an observe
// pass parks once it may (Runtime.Observe).
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
	prompt, err := e.liaisonPrompt(ctx, t)
	if err != nil {
		return "", err
	}
	e.log.Info("a task's liaison is asked for", "task", t.ID, "surface", p.Surface)
	pane, talkErr := e.d.Runtime.Talk(ctx, t, prompt)
	// A drain may have cut the Talk: the re-read, the park and its event
	// still run.
	bg := context.WithoutCancel(ctx)
	now, err := e.reg.TaskByID(bg, t.ID)
	switch {
	case err != nil:
		e.log.Warn("read a task again after its talk; an observe pass parks its liaison if the task ended", "task", t.ID, "err", err)
	case talkEnded(now):
		if err := e.d.Runtime.Park(bg, now); err != nil {
			e.event(bg, t.ID, "park", registry.PhaseFail, "the liaison of a task that ended while it started: "+err.Error())
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
// copy only when the task holds one (copyOfTask; the prompt describes it
// then).
func (e *Engine) liaisonPrompt(ctx context.Context, t registry.Task) (string, error) {
	spec, ok := e.cfg.FactoryRole(runtime.LiaisonRole)
	if !ok {
		return "", fmt.Errorf("the liaison of %s: no factory role %s", TaskKey(t), runtime.LiaisonRole)
	}
	runAs, err := e.d.Runtime.RunsAs(t, spec)
	if err != nil {
		return "", fmt.Errorf("the liaison of %s: %w", TaskKey(t), err)
	}
	return e.renderRole(ctx, t, config.FactoryStage{}, spec, runAs, e.copyOfTask(t))
}

// copyOfTask is the copy task t holds: the one the engine holds for it (a
// claim handed it over), else, since that memory starts empty after a daemon
// restart and only a claim fills it, the copy the task's row names, resolved
// by its repository's copies pattern as the claims resolve a copy
// (copyNumber, the Pool's path): the read of a path, no claim and no
// provisioning. The zero Copy when the task holds none, or its repository
// renders no copy of that name.
func (e *Engine) copyOfTask(t registry.Task) Copy {
	if c, ok := e.heldCopy(t.ID); ok {
		return c
	}
	if t.Copy == "" {
		return Copy{}
	}
	repo, ok := e.cfg.FactoryRepo(t.Repo)
	if !ok {
		return Copy{}
	}
	n, ok := copyNumber(repo, t.Copy)
	if !ok {
		return Copy{}
	}
	return copyOf(repo, store.Slot{Name: t.Copy, Path: repo.Pool().Path(n)}, n)
}
