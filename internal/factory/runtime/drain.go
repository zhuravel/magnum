package runtime

// What a drain for a reboot (`magnum drain`) asks of the runtime: the
// agents' turns in flight, which a reboot would cut (herdr's panes close
// with the Mac), and the hold of new prompts (Deps.MayPrompt). A restart of
// the factory daemon lets those turns go on, so a restart's drain waits for
// none of them; a reboot does not.

import (
	"context"
	"fmt"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// TurnInFlight is an agent's turn in flight: the task it works for (0 when the
// store's row names none), whether it is the task's liaison, its role and
// since when (the prompt's submit, else the run's creation).
type TurnInFlight struct {
	TaskID  int64
	Liaison bool
	Role    string
	Since   time.Time
}

// TurnsInFlight are the turns of every agent of the factory that a reboot
// would cut, oldest first: each run the agents store keeps pending,
// submitted or working, on a task's row or its liaison's. A run whose turn
// ended (ended, verified, failed, abandoned) is idle; a state the store
// cannot place counts as a turn in flight.
func (r *Runtime) TurnsInFlight(ctx context.Context) ([]TurnInFlight, error) {
	runs, err := r.st.ActiveRuns(ctx)
	if err != nil {
		return nil, fmt.Errorf("the agents' turns in flight: %w", err)
	}
	var out []TurnInFlight
	for _, run := range runs {
		switch run.State {
		case store.RunEnded, store.RunVerified, store.RunFailed, store.RunAbandoned:
			continue
		}
		t := TurnInFlight{Role: run.Role, Since: run.CreatedAt}
		if run.SubmittedAt != nil {
			t.Since = *run.SubmittedAt
		}
		if pr, err := r.st.PRByID(ctx, run.PRID); err == nil {
			if id, ok := LiaisonTask(pr.NodeID); ok {
				t.TaskID, t.Liaison = id, true
			} else if id, ok := OwnerTask(pr.NodeID); ok {
				t.TaskID = id
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// mayPrompt is the engine's hold of new prompts (Deps.MayPrompt: a drain
// for a reboot), a *WaitError in its words; nil when none holds them.
func (r *Runtime) mayPrompt() error {
	if r.d.MayPrompt == nil {
		return nil
	}
	if who, what, ok := r.d.MayPrompt(); !ok {
		return &WaitError{Who: who, What: what}
	}
	return nil
}
