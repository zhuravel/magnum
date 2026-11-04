package factory

// The queue of intake: a new task starts at once, or waits queued while
// memory is full or max_active tasks are at work, and starts on the first
// tick with room (admit), oldest first.

import (
	"context"
	"fmt"
	"slices"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// startState is the state a new task starts in, and what it waits on: active,
// or queued while memory is full or max_active tasks are at work
// (startGate).
func (e *Engine) startState(ctx context.Context) (state, who, what string, err error) {
	tasks, err := e.reg.Tasks(ctx)
	if err != nil {
		return "", "", "", err
	}
	if who, what = e.startGate(e.readMemory(ctx), tasks, 0); who != "" {
		return registry.TaskQueued, who, what, nil
	}
	return registry.TaskActive, "", "", nil
}

// memoryGate is the memory's word on a start: full is false while a task
// may start; pct is the swap's use, critical the kernel's pressure level.
type memoryGate struct {
	full, critical bool
	pct            int
}

// readMemory reads the machine's memory against [factory] memory_gate_swap
// (and the kernel's critical level), each time (Deps.Memory,
// engine.ReadMemory); no reader lets every start through.
func (e *Engine) readMemory(ctx context.Context) memoryGate {
	if e.d.Memory == nil {
		return memoryGate{}
	}
	s := e.d.Memory(ctx)
	g := memoryGate{critical: s.Level == engine.MemoryCritical}
	if s.Swap != nil && s.Swap.TotalBytes > 0 {
		g.pct = int(s.Swap.UsedBytes * 100 / s.Swap.TotalBytes)
	}
	gate := e.cfg.Factory.MemoryGateSwap
	g.full = g.critical || (gate > 0 && s.Swap != nil && s.Swap.TotalBytes > 0 && g.pct >= gate)
	return g
}

// startGate is what a task that would start now waits on ("" when it may
// start): memory ("memory: full, starts below 80% swap"), or a copy while
// max_active tasks other than self are at work ("copy: all 3 busy").
func (e *Engine) startGate(mem memoryGate, tasks []registry.Task, self int64) (who, what string) {
	switch {
	case mem.critical:
		return waitMemory, "full (critical pressure), starts when it eases"
	case mem.full:
		return waitMemory, fmt.Sprintf("full, starts below %d%% swap", e.cfg.Factory.MemoryGateSwap)
	}
	limit := max(1, e.cfg.Factory.MaxActive)
	n := 0
	for _, t := range tasks {
		if t.ID != self && atWork(t) {
			n++
		}
	}
	if n >= limit {
		return waitCopy, fmt.Sprintf("all %d busy", limit)
	}
	return "", ""
}

// atWork reports whether a task counts toward max_active: it is active, or
// it holds a copy and has not ended.
func atWork(t registry.Task) bool {
	switch t.State {
	case registry.TaskActive:
		return true
	case registry.TaskQueued, registry.TaskDone, registry.TaskStopped:
		return false
	}
	return t.Copy != ""
}

// admit starts the queued tasks there is room for, oldest first: memory
// below the gate and fewer than max_active tasks at work. A task that
// still waits keeps the wait that says on what; a queued task never jumps
// another.
func (e *Engine) admit(ctx context.Context) {
	tasks, err := e.reg.Tasks(ctx)
	if err != nil {
		e.log.Warn("factory admit", "err", err)
		return
	}
	if !slices.ContainsFunc(tasks, func(t registry.Task) bool { return t.State == registry.TaskQueued }) {
		return
	}
	mem := e.readMemory(ctx)
	blocked := false
	for i, t := range tasks {
		if t.State != registry.TaskQueued {
			continue
		}
		if who, what := e.startGate(mem, tasks, t.ID); who != "" || blocked {
			if who != "" {
				e.waitOn(ctx, t, who, what)
			}
			blocked = true
			continue
		}
		if err := e.reg.TransitionTask(ctx, t.ID, []string{registry.TaskQueued}, registry.TaskActive, func(u *registry.TaskUpdate) {
			u.Set("waits_who", "")
			u.Set("waits_what", "")
			u.Because("room to start")
		}); err != nil {
			e.log.Warn("start a queued task", "task", t.ID, "err", err)
			continue
		}
		tasks[i].State = registry.TaskActive
	}
}
