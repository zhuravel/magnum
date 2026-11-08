package agents

import (
	"testing"
	"time"
)

// The deny waits for the screen by the clock, not by the passes: the same
// prompt still on screen within denyCatchUp (10 s) of its deny is not denied
// again, however many passes come between (a hook's pass right after a
// look's deny, the clock's), and is denied again once that time is over.
func TestTheSamePromptIsDeniedAgainOnlyTenSecondsAfterItsDeny(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.blockedOn(RoleClaude, claudeRmPrompt) // the agent ignores the keys: the prompt stays
	if o := e.observe()[RoleClaude]; o.Kind != ObsPromptDenied {
		t.Fatalf("first pass = %+v, want prompt_denied", o)
	}
	deniedAt := e.clock.Now()
	for i := range 4 {
		e.clock.Set(deniedAt.Add(time.Duration(2*i+1) * time.Second)) // passes 1, 3, 5 and 7 s after the deny
		if o := e.observe()[RoleClaude]; o.Kind != ObsPromptDenied {
			t.Fatalf("pass %d after the deny = %+v, want prompt_denied (waiting for the screen)", i+1, o)
		}
		if n := len(e.promptEvents(EventPromptDenied)); n != 1 {
			t.Fatalf("pass %d, %s after the deny: %d denies, want the first alone (the screen may lag)", i+1,
				e.clock.Now().Sub(deniedAt), n)
		}
	}
	e.clock.Set(deniedAt.Add(denyCatchUp))
	e.observe()
	if n := len(e.promptEvents(EventPromptDenied)); n != 2 {
		t.Fatalf("%d denies once %s passed, want the prompt denied again", n, denyCatchUp)
	}
}
