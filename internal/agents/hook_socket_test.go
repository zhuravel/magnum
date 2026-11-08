package agents

import (
	"sync"
	"testing"

	"github.com/zhuravel/magnum/internal/agenthooks"
)

// A daemon whose listener comes up after its Manager was built names the
// socket with SetHookSocket: every pane made after it carries the socket and
// the binary, and every pane made after SetHookSocket("") carries neither,
// while the panes already made keep the env they were made with.
func TestSetHookSocketNamesTheSocketInThePanesMadeAfterIt(t *testing.T) {
	e := newEnv(t)
	e.m.d.HookBin = hookBin
	e.m.SetHookSocket(hookSocket)
	ws := e.workspace()

	e.m.SetHookSocket("")
	if _, err := e.m.EnsurePane(e.ctx, e.pr, ws, "/Users/x/Projects/talkable.review1", map[string]string{"MAGNUM_PR_URL": e.pr.URL},
		e.spec(RoleSimplify)); err != nil {
		t.Fatal(err)
	}

	e.h.mu.Lock()
	envs := []map[string]string{e.h.creates[0].Env}
	for _, s := range e.h.splits {
		envs = append(envs, s.Opts.Env)
	}
	e.h.mu.Unlock()
	if len(envs) != 4 {
		t.Fatalf("%d panes made, want the workspace's 3 and one after", len(envs))
	}
	for i, env := range envs[:3] {
		if got, want := hookEnvOf(env), [2]string{hookSocket, hookBin}; got != want {
			t.Errorf("pane %d, made after SetHookSocket: hook env %q, want %q", i, got, want)
		}
	}
	after := envs[3]
	if _, ok := after[agenthooks.EnvSocket]; ok {
		t.Errorf("the pane made after SetHookSocket(\"\") names a socket: %v", after)
	}
	if _, ok := after[agenthooks.EnvBin]; ok {
		t.Errorf("the pane made after SetHookSocket(\"\") names a binary: %v", after)
	}
}

// SetHookSocket may run while a pane starts on another goroutine (the
// listener comes up beside the daemon's first work): the pane env reads the
// socket under the Manager's lock, so -race sees no race, and each pane gets
// the whole hook env or none of it.
func TestSetHookSocketIsSafeBesideAPaneStart(t *testing.T) {
	e := newEnv(t)
	e.m.d.HookBin = hookBin
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 200 {
			if i%2 == 0 {
				e.m.SetHookSocket(hookSocket)
			} else {
				e.m.SetHookSocket("")
			}
		}
	})
	var envs []map[string]string
	wg.Go(func() {
		ws, err := e.m.EnsureWorkspace(e.ctx, e.pr, "/Users/x/Projects/talkable.review1",
			map[string]string{"WT_BRANCH": "review1"}, "talkable#11920", e.coreRoles())
		if err != nil {
			t.Errorf("EnsureWorkspace: %v", err)
			return
		}
		if _, err := e.m.EnsurePane(e.ctx, e.pr, ws, "/Users/x/Projects/talkable.review1", nil, e.spec(RoleSimplify)); err != nil {
			t.Errorf("EnsurePane: %v", err)
		}
		for range 200 {
			env, err := e.m.PaneEnv(map[string]string{"WT_BRANCH": "review1"}, e.spec(RoleSimplify))
			if err != nil {
				t.Errorf("PaneEnv: %v", err)
				return
			}
			envs = append(envs, env)
		}
	})
	wg.Wait()

	e.h.mu.Lock()
	envs = append(envs, e.h.creates[0].Env)
	for _, s := range e.h.splits {
		envs = append(envs, s.Opts.Env)
	}
	e.h.mu.Unlock()
	for i, env := range envs {
		if got := hookEnvOf(env); got != [2]string{hookSocket, hookBin} && got != [2]string{} {
			t.Fatalf("env %d has half the hook env: %q", i, got)
		}
	}
}
